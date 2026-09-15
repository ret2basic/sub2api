package service

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 国产供应商（kimi/zhipu/deepseek）的响应式冷却辅助。
//
// 与 openai/anthropic 不同：
//   - 余额不足是「可恢复」状态（充值/检测恢复后自动重新调度），不能走 handleAuthError
//     永久置 status=error。这里改为 SetTempUnschedulable，由 CN 余额检测周期任务
//     （cn_provider_balance_check_service.go）在余额恢复后 ClearTempUnschedulable。
//   - Coding Plan 滚动窗口耗尽（429）的冷却终点应是真实的窗口重置时间（已由
//     CNProviderQuotaService 落入 account.Extra 快照），而非默认的秒级兜底。

// cnBalanceExtraSuffixLow 标记账号响应过「余额不足」，供余额检测任务区分
// 「确属余额不足」与「尚未探测」。
const cnBalanceExtraSuffixLow = "balance_low"

// cnBalanceLowReasonPrefix 是余额不足临时停调 reason 的稳定前缀。
// 周期余额检测任务据此识别「是我们停调的」并在余额恢复后安全清除——不会误清
// 其他子系统（阈值/限流/401）写入的临时停调。
const cnBalanceLowReasonPrefix = "cn_balance_low"

const kimiConcurrentRequestLimitMessage = "You've reached your concurrent request limit. Please wait for your ongoing requests to finish and try again."

const cnConcurrencyLimitReasonPrefix = "cn_concurrency_limit"

func isCNProviderConcurrencyLimit403(account *Account, upstreamMsg string) bool {
	return account != nil && account.Platform == PlatformKimi &&
		strings.TrimSpace(upstreamMsg) == kimiConcurrentRequestLimitMessage
}

// cnProviderResponseIndicatesUsageWindowLimit 识别 kimi 配额窗口耗尽的文案
//（"You've reached your 5-hour usage limit" / "weekly (7-day) usage limit"）。
// 与并发 403（isCNProviderConcurrencyLimit403，精确匹配）相区分：usage limit
// 属窗口耗尽，冷却终点应取快照重置点；并发限制属瞬态，短冷却即可。
func cnProviderResponseIndicatesUsageWindowLimit(upstreamMsg string, responseBody []byte) bool {
	if strings.Contains(strings.ToLower(upstreamMsg), "usage limit") {
		return true
	}
	return len(responseBody) > 0 &&
		strings.Contains(strings.ToLower(string(responseBody)), "usage limit")
}

// zhipuTransientRateLimitCooldown 识别智谱的瞬时限流 429：1113（并发数量超过上限）
// 与 1302（请求频率超上限）。这两类是分钟级瞬时约束，与 5h/weekly 配额窗口无关；
// 若套用窗口重置点会把账号停调数小时乃至数天（2026-09-15 GLM 池 3 天冷却事故）。
// 返回 0 表示未命中。
func zhipuTransientRateLimitCooldown(responseBody []byte) time.Duration {
	if len(responseBody) == 0 {
		return 0
	}
	if jsonErrorCodeIs(responseBody, "1113") {
		return 10 * time.Minute
	}
	if jsonErrorCodeIs(responseBody, "1302") {
		return 2 * time.Minute
	}
	return 0
}

// jsonErrorCodeIs 判断 JSON 错误体里 error.code 是否等于指定字符串码。
// 兼容 "code":"1113" / "code": "1113" / "code":1113 三种形态。
func jsonErrorCodeIs(body []byte, code string) bool {
	return bytes.Contains(body, []byte(`"code":"`+code+`"`)) ||
		bytes.Contains(body, []byte(`"code": "`+code+`"`)) ||
		bytes.Contains(body, []byte(`"code":`+code))
}

// handleZhipuTransientRateLimit 对智谱瞬时限流做分钟级临时停调（SetTempUnschedulable，
// 与 kimi 并发 403 同口径），到期自动回到调度，不写 rate_limit_reset_at。
func (s *RateLimitService) handleZhipuTransientRateLimit(
	ctx context.Context,
	account *Account,
	cooldown time.Duration,
	responseBody []byte,
) {
	until := time.Now().Add(cooldown)
	reason := cnConcurrencyLimitReasonPrefix + ": zhipu transient 429 " + truncateForLog(responseBody, 256)
	s.notifyAccountSchedulingBlocked(account, until, cnConcurrencyLimitReasonPrefix)
	if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, reason); err != nil {
		slog.Warn("zhipu_transient_rate_limit_set_failed", "account_id", account.ID, "error", err)
		return
	}
	slog.Info("zhipu_transient_rate_limited",
		"account_id", account.ID,
		"platform", account.Platform,
		"cooldown", cooldown.String(),
		"until", until.UTC(),
	)
}

func (s *RateLimitService) handleCNProviderConcurrencyLimit403(
	ctx context.Context,
	account *Account,
) {
	until := time.Now().Add(time.Duration(openAI403CooldownMinutesDefault) * time.Minute)
	reason := cnConcurrencyLimitReasonPrefix + ": " + kimiConcurrentRequestLimitMessage
	s.notifyAccountSchedulingBlocked(account, until, cnConcurrencyLimitReasonPrefix)
	if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, reason); err != nil {
		slog.Warn("cn_concurrency_limit_set_temp_unschedulable_failed", "account_id", account.ID, "error", err)
		return
	}
	slog.Info("cn_provider_concurrency_limited",
		"account_id", account.ID,
		"platform", account.Platform,
		"until", until.UTC(),
	)
}

// cnBalanceLowReason 构造余额不足临时停调的 reason（带稳定前缀）。
func cnBalanceLowReason(upstreamMsg string) string {
	if upstreamMsg = strings.TrimSpace(upstreamMsg); upstreamMsg != "" {
		return cnBalanceLowReasonPrefix + ": " + upstreamMsg
	}
	return cnBalanceLowReasonPrefix + ": 余额不足，账号临时停调"
}

// cnProviderResponseIndicatesInsufficientBalance 通过响应体文案识别余额不足
// （智谱 payg 无独立余额端点，仅能靠响应文案识别）。
func cnProviderResponseIndicatesInsufficientBalance(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	s := strings.ToLower(string(body))
	return strings.Contains(s, "余额不足") ||
		strings.Contains(s, "insufficient balance") ||
		strings.Contains(s, "insufficient_credit") ||
		strings.Contains(s, "balance is not enough") ||
		strings.Contains(s, "no enough balance")
}

// handleCNProviderInsufficientBalance 把余额不足标记为可恢复的临时停调：
// 写入 balance_low 快照 + SetTempUnschedulable 一个余额检测周期，
// 由周期任务在余额恢复后清除。返回前已通知调度阻塞。
func (s *RateLimitService) handleCNProviderInsufficientBalance(
	ctx context.Context,
	account *Account,
	upstreamMsg string,
) {
	msg := cnBalanceLowReason(upstreamMsg)

	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		cnExtraKey(account.Platform, cnBalanceExtraSuffixLow): true,
	}); err != nil {
		slog.Warn("cn_balance_low_mark_failed", "account_id", account.ID, "error", err)
	}

	until := time.Now().Add(s.cnBalanceCooldownDuration())
	s.notifyAccountSchedulingBlocked(account, until, "cn_insufficient_balance")
	if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, msg); err != nil {
		slog.Warn("cn_balance_set_temp_unschedulable_failed", "account_id", account.ID, "error", err)
		return
	}
	slog.Info("cn_provider_insufficient_balance",
		"account_id", account.ID,
		"platform", account.Platform,
		"until", until.UTC(),
	)
}

// cnBalanceCooldownDuration 返回余额不足临时停调的持续时长（= 2× 余额检测周期，
// 默认 20 分钟）。周期任务会在余额恢复后提前清除，故此处只需保证冷却覆盖到下一次
// 周期检测即可。
func (s *RateLimitService) cnBalanceCooldownDuration() time.Duration {
	minutes := 10
	if s != nil && s.cfg != nil {
		if cfgMin := s.cfg.Gateway.CNProviders.BalanceCheckIntervalMinutes; cfgMin > 0 {
			minutes = cfgMin
		}
	}
	cooldown := time.Duration(minutes) * time.Minute * 2
	if cooldown < time.Minute {
		cooldown = 10 * time.Minute
	}
	return cooldown
}

// cnProviderQuotaSnapshotReset 读取 Coding Plan 账号快照中最早一个仍在未来的窗口
// 重置时间（5h / weekly）。429 多数由 5h 滚动窗口触发，取较早的重置点可避免
// 把账号冷却到 weekly 重置（可达数天）的过度停调；如果确是 weekly 窗口耗尽，
// 周期额度探测刷新快照后阈值评估会再次停调到正确的时间点。
//
// 长窗口守卫（2026-09-15 GLM 池事故修正）：weekly/monthly 重置点只有在其
// used_percent ≥ 100（快照证实窗口确实耗尽）时才可采纳。此前 5h 快照恰好过期
// （周期探测失败或窗口刚翻页）时会「无路可走」直接落到 weekly，把瞬时的
// 5h 翻页/探测盲区放大成 3-4 天停调。5h 窗口（最长 5 小时）reset 在未来即信任。
// 无快照或均已过期返回 nil。
func cnProviderQuotaSnapshotReset(account *Account, now time.Time) *time.Time {
	if account == nil || len(account.Extra) == 0 {
		return nil
	}
	if !account.IsOpenCodeGo() && (!account.IsCNProvider() || !account.IsCodingPlan()) {
		return nil
	}
	provider := account.Platform
	suffixes := []string{cnExtraSuffix5hReset, cnExtraSuffixWeeklyReset}
	if account.IsOpenCodeGo() {
		suffixes = append(suffixes, cnExtraSuffixMonthlyReset)
	}
	var earliest *time.Time
	for _, suffix := range suffixes {
		t := parseSchedulingResetAt(account.Extra[cnExtraKey(provider, suffix)])
		if t == nil || !t.After(now) {
			continue
		}
		// 长窗口必须被快照证实耗尽（used>=100）才允许作为冷却终点。
		if suffix == cnExtraSuffixWeeklyReset && !cnProviderWindowUsedAtLeast(account, provider, cnExtraSuffixWeeklyUsed, 100) {
			continue
		}
		if suffix == cnExtraSuffixMonthlyReset && !cnProviderWindowUsedAtLeast(account, provider, cnExtraSuffixMonthlyUsed, 100) {
			continue
		}
		if earliest == nil || t.Before(*earliest) {
			earliest = t
		}
	}
	return earliest
}

// cnProviderWindowUsedAtLeast 判断快照中某窗口的 used_percent 是否 ≥ threshold。
// used_percent 由 CNProviderQuotaService 写入（int 百分比）。读取失败/缺失返回 false
// （保守：不确认耗尽就不允许长窗口冷却）。
func cnProviderWindowUsedAtLeast(account *Account, provider string, usedSuffix string, threshold float64) bool {
	raw, ok := account.Extra[cnExtraKey(provider, usedSuffix)]
	if !ok || raw == nil {
		return false
	}
	var used float64
	switch v := raw.(type) {
	case float64:
		used = v
	case int:
		used = float64(v)
	case int64:
		used = float64(v)
	case json.Number:
		parsed, err := v.Float64()
		if err != nil {
			return false
		}
		used = parsed
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return false
		}
		used = parsed
	default:
		return false
	}
	return used >= threshold
}

// applyCNProviderReactive429 处理国产供应商的 429 响应。
// 返回 true 表示已处理（调用方应 return），false 表示未命中、继续走默认 429 逻辑。
func (s *RateLimitService) applyCNProviderReactive429(
	ctx context.Context,
	account *Account,
	headers http.Header,
	responseBody []byte,
) bool {
	if account.IsOpenCodeGo() {
		if until := cnProviderQuotaSnapshotReset(account, time.Now()); until != nil {
			s.notifyAccountSchedulingBlocked(account, *until, "429")
			if err := s.accountRepo.SetRateLimited(ctx, account.ID, *until); err != nil {
				slog.Warn("rate_limit_set_failed", "account_id", account.ID, "error", err)
				return true
			}
			slog.Info("opencode_go_rate_limited",
				"account_id", account.ID,
				"platform", account.Platform,
				"reset_at", *until,
			)
			return true
		}
		if resetAt := parseOpenAIRateLimitResetTime(responseBody); resetAt != nil {
			resetTime := time.Unix(*resetAt, 0)
			s.notifyAccountSchedulingBlocked(account, resetTime, "429")
			if err := s.accountRepo.SetRateLimited(ctx, account.ID, resetTime); err != nil {
				slog.Warn("rate_limit_set_failed", "account_id", account.ID, "error", err)
				return true
			}
			slog.Info("opencode_go_rate_limited",
				"account_id", account.ID,
				"platform", account.Platform,
				"reset_at", resetTime,
			)
			return true
		}
		return false
	}
	if !account.IsCNProvider() {
		return false
	}
	// 0) 智谱瞬时限流（1113 并发超上限 / 1302 请求频率超上限）：分钟级临时停调，
	// 绝不套用配额窗口重置点（此类错误与 5h/weekly 窗口无关）。
	if account.Platform == PlatformZhipu {
		if cooldown := zhipuTransientRateLimitCooldown(responseBody); cooldown > 0 {
			s.handleZhipuTransientRateLimit(ctx, account, cooldown, responseBody)
			return true
		}
	}
	// 1) 余额不足文案：可恢复临时停调（含智谱 payg 这类无余额端点的场景）。
	if cnProviderResponseIndicatesInsufficientBalance(responseBody) {
		s.handleCNProviderInsufficientBalance(ctx, account, extractUpstreamErrorMessage(responseBody))
		return true
	}
	// 2) Coding Plan 窗口耗尽：冷却到快照中最早的窗口重置点（见
	// cnProviderQuotaSnapshotReset：429 多由 5h 窗口触发，取较早点避免过度停调）。
	if account.IsCodingPlan() {
		if until := cnProviderQuotaSnapshotReset(account, time.Now()); until != nil {
			s.notifyAccountSchedulingBlocked(account, *until, "429")
			if err := s.accountRepo.SetRateLimited(ctx, account.ID, *until); err != nil {
				slog.Warn("rate_limit_set_failed", "account_id", account.ID, "error", err)
				return true
			}
			slog.Info("cn_coding_plan_rate_limited",
				"account_id", account.ID,
				"platform", account.Platform,
				"reset_at", *until,
			)
			return true
		}
	}
	return false
}
