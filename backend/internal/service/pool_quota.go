package service

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"
)

// 池级额度视图（GET /v1/sub2api/pool-capacity 的 quota 字段，fuhrer fork 2026-10-15）。
//
// 研究工具按任务在 GLM 池与 Grok 池之间选池（Grok 4.7 与 GLM-5.3 互为替补），
// 需要知道哪一边的付费窗口烧得比匀速快。网关本来就持有逐账号的窗口快照
// （国产 coding plan 探测写入的 <provider>_weekly_*，Grok 的 billing 快照与限流头快照），
// 这里只把它们折叠成一个池级窗口，不新增任何上游探测。
//
// 口径：
//   - used_fraction：窗口已用比例（0..1），按上报账号取平均；
//   - elapsed_fraction：窗口已过去的比例（0..1），同样取平均；
//   - pace = used / max(elapsed, 0.02)：>1 表示比匀速烧得快、会提前耗尽，<1 表示会剩；
//   - 窗口位置未知时（Grok 只有滚动限流头），elapsed 按 used 填、pace=1 并标 pace_known=false，
//     客户端据此按「中性」处理，只用 used_fraction 判断是否耗尽。

const (
	poolQuotaWeekSeconds    int64 = 7 * 24 * 3600
	poolQuotaMinElapsed           = 0.02
	poolQuotaExhaustedUsed        = 0.995
	poolQuotaSnapshotMaxAge       = 3 * time.Hour
)

// PoolQuotaSummary 是一个池（分组）在其约束窗口上的额度汇总。
type PoolQuotaSummary struct {
	BindingWindow     string  `json:"binding_window"`
	UsedFraction      float64 `json:"used_fraction"`
	ElapsedFraction   float64 `json:"elapsed_fraction"`
	Pace              float64 `json:"pace"`
	PaceKnown         bool    `json:"pace_known"`
	WindowSeconds     int64   `json:"window_seconds,omitempty"`
	ResetAt           string  `json:"reset_at,omitempty"`
	AccountsReporting int     `json:"accounts_reporting"`
	AccountsExhausted int     `json:"accounts_exhausted"`
	UpdatedAt         string  `json:"updated_at,omitempty"`
}

// poolQuotaAccountWindow 是单账号在约束窗口上的位置。
type poolQuotaAccountWindow struct {
	window       string
	used         float64
	elapsed      float64
	elapsedKnown bool
	seconds      int64
	reset        time.Time
	updated      time.Time
}

// BuildPoolQuotaSummary 把一个分组内账号的窗口快照折叠成池级额度。
// 只统计 active 且 Schedulable（运营者启用）的账号；被限流的账号照样计入，
// 它们正是「额度紧张」的证据。无任何账号上报时返回 nil（客户端退化为只看容量）。
func BuildPoolQuotaSummary(platform string, accounts []Account, now time.Time) *PoolQuotaSummary {
	windows := make([]poolQuotaAccountWindow, 0, len(accounts))
	exhausted := 0
	for i := range accounts {
		account := &accounts[i]
		if !account.IsActive() || !account.Schedulable {
			continue
		}
		var (
			w  poolQuotaAccountWindow
			ok bool
		)
		switch platform {
		case PlatformZhipu, PlatformKimi:
			w, ok = cnPoolQuotaWindow(account, platform, now)
		case PlatformGrok:
			w, ok = grokPoolQuotaWindow(account, now)
		}
		if !ok {
			continue
		}
		if w.used >= poolQuotaExhaustedUsed || account.IsRateLimited() {
			exhausted++
		}
		windows = append(windows, w)
	}
	if len(windows) == 0 {
		return nil
	}

	var usedSum, elapsedSum float64
	elapsedCount := 0
	var earliestReset, latestUpdate time.Time
	var seconds int64
	binding := windows[0].window
	for _, w := range windows {
		usedSum += w.used
		if w.elapsedKnown {
			elapsedSum += w.elapsed
			elapsedCount++
		}
		if !w.reset.IsZero() && (earliestReset.IsZero() || w.reset.Before(earliestReset)) {
			earliestReset = w.reset
		}
		if w.updated.After(latestUpdate) {
			latestUpdate = w.updated
		}
		if w.seconds > seconds {
			seconds = w.seconds
		}
		if w.window != binding {
			binding = "mixed"
		}
	}
	used := clampUnit(usedSum / float64(len(windows)))
	summary := &PoolQuotaSummary{
		BindingWindow:     binding,
		UsedFraction:      roundQuota(used),
		WindowSeconds:     seconds,
		AccountsReporting: len(windows),
		AccountsExhausted: exhausted,
	}
	if elapsedCount > 0 {
		elapsed := clampUnit(elapsedSum / float64(elapsedCount))
		summary.ElapsedFraction = roundQuota(elapsed)
		summary.Pace = roundQuota(used / math.Max(elapsed, poolQuotaMinElapsed))
		summary.PaceKnown = true
	} else {
		summary.ElapsedFraction = roundQuota(used)
		summary.Pace = 1
	}
	if !earliestReset.IsZero() {
		summary.ResetAt = earliestReset.UTC().Format(time.RFC3339)
	}
	if !latestUpdate.IsZero() {
		summary.UpdatedAt = latestUpdate.UTC().Format(time.RFC3339)
	}
	return summary
}

// cnPoolQuotaWindow 取国产 coding plan 账号的周窗口（CN 探测每 ~10 分钟写一次）。
// 5h 窗口不参与配速：它耗尽时账号被限流、池容量随之收缩，客户端靠容量就能看见。
func cnPoolQuotaWindow(account *Account, provider string, now time.Time) (poolQuotaAccountWindow, bool) {
	updated, ok := quotaTime(account.Extra[cnExtraKey(provider, cnExtraSuffixUsageUpdated)])
	if !ok || now.Sub(updated) > poolQuotaSnapshotMaxAge {
		return poolQuotaAccountWindow{}, false
	}
	usedPercent, ok := quotaNumber(account.Extra[cnExtraKey(provider, cnExtraSuffixWeeklyUsed)])
	if !ok {
		return poolQuotaAccountWindow{}, false
	}
	w := poolQuotaAccountWindow{
		window:  "weekly",
		used:    clampUnit(usedPercent / 100),
		seconds: poolQuotaWeekSeconds,
		updated: updated,
	}
	if reset, ok := quotaTime(account.Extra[cnExtraKey(provider, cnExtraSuffixWeeklyReset)]); ok {
		if !reset.After(now) {
			// 快照之后窗口已翻页：旧用量作废，新窗口刚开始。
			w.used, w.elapsed, w.elapsedKnown = 0, 0, true
			return w, true
		}
		w.reset = reset
		w.elapsed = clampUnit(1 - reset.Sub(now).Seconds()/float64(poolQuotaWeekSeconds))
		w.elapsedKnown = true
	}
	return w, true
}

// grokPoolQuotaWindow 优先取 billing 周窗口（usage_percent + period_start/end），
// 没有用量百分比时退回限流头的滚动窗口（grok_sched_utilization，0..100，窗口位置未知）。
func grokPoolQuotaWindow(account *Account, now time.Time) (poolQuotaAccountWindow, bool) {
	if billing, ok := account.Extra[grokBillingExtraKey].(map[string]any); ok {
		updated, fresh := quotaTime(billing["weekly_updated_at"])
		if !fresh {
			updated, fresh = quotaTime(billing["updated_at"])
		}
		usedPercent, hasUsed := quotaNumber(billing["usage_percent"])
		start, hasStart := quotaTime(billing["period_start"])
		end, hasEnd := quotaTime(billing["period_end"])
		if fresh && now.Sub(updated) <= poolQuotaSnapshotMaxAge && hasUsed && hasStart && hasEnd && end.After(start) {
			w := poolQuotaAccountWindow{
				window:  "weekly",
				used:    clampUnit(usedPercent / 100),
				seconds: int64(end.Sub(start).Seconds()),
				reset:   end,
				updated: updated,
			}
			if !end.After(now) {
				w.used, w.elapsed, w.elapsedKnown = 0, 0, true
				w.reset = time.Time{}
				return w, true
			}
			w.elapsed = clampUnit(now.Sub(start).Seconds() / end.Sub(start).Seconds())
			w.elapsedKnown = true
			return w, true
		}
	}
	updated, ok := quotaTime(account.Extra["grok_sched_usage_updated_at"])
	if !ok || now.Sub(updated) > poolQuotaSnapshotMaxAge {
		return poolQuotaAccountWindow{}, false
	}
	utilization, ok := quotaNumber(account.Extra["grok_sched_utilization"])
	if !ok {
		return poolQuotaAccountWindow{}, false
	}
	w := poolQuotaAccountWindow{window: "rolling", used: clampUnit(utilization / 100), updated: updated}
	if reset, ok := quotaTime(account.Extra["grok_sched_reset_at"]); ok {
		if !reset.After(now) {
			w.used = 0
		} else {
			w.reset = reset
		}
	}
	return w, true
}

// GetGroupQuotaByID 汇总分组内全部（含已限流）账号的窗口快照。
func (s *GroupCapacityService) GetGroupQuotaByID(ctx context.Context, groupID int64, platform string) (*PoolQuotaSummary, error) {
	switch platform {
	case PlatformZhipu, PlatformKimi, PlatformGrok:
	default:
		return nil, nil
	}
	accounts, err := s.accountRepo.ListByGroup(ctx, groupID)
	if err != nil {
		return nil, err
	}
	return BuildPoolQuotaSummary(platform, accounts, time.Now()), nil
}

func quotaNumber(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, !math.IsNaN(v) && !math.IsInf(v, 0)
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	}
	return 0, false
}

func quotaTime(value any) (time.Time, bool) {
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999-07:00"} {
		if t, err := time.Parse(layout, strings.TrimSpace(text)); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func clampUnit(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func roundQuota(v float64) float64 {
	return math.Round(v*10000) / 10000
}
