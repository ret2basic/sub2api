package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// 智谱号的同时会话上限（2026-11-28，fuhrer 分支）。
//
// 背景：11-25、11-28 智谱三次给 GLM Coding Plan 号发风控提示（「账号已进入风险提示状态」）。
// 网关账本显示，一个号在 5 分钟里要接 17～42 个不同的 Claude Code 会话；同一个会话的
// 请求还会被分到 2～6 个号上（会话绑定的号满了就把这次请求临时转给别的号）。按智谱的
// 订阅协议，套餐只供订阅人本人用，禁止共享和聚合转发；在它看来这两种形态都像多人共用一个号。
//
// 做法：智谱号在 extra 里设了 max_sessions 时，一个号同时服务的会话不超过这个数。会话的
// 标识就是粘性会话哈希（Claude Code 自带的会话 ID 派生），最后一次请求后空闲
// session_idle_timeout_minutes 分钟才让出名额（默认 5）。已登记在这个号上的会话照常通过；
// 新会话只能落到还有名额的号上，都满了就返回无可用账号，由客户端稍后重试。
// 会话本身固定在一个号上，靠的是配置里的 sticky_session_max_waiting：号满了就排队等自己的号，
// 不再临时转去别的号。
//
// 登记表复用 Anthropic 会话上限的 SessionLimitCache（Redis 有序集合 session_limit:account:{id}），
// 所以 pool-capacity 和管理台的会话数口径与 Anthropic 号一致。Redis 出错时放行（与 Anthropic 一致）。

// maxZhipuSessionAdmissionPasses 限制一次选号里因为会话满而重选的次数，防止异常时空转。
const maxZhipuSessionAdmissionPasses = 16

// errZhipuSessionCapReached 表示所有可用的智谱号都已服务满 max_sessions 个别的会话。
var errZhipuSessionCapReached = fmt.Errorf("%w: every zhipu account is serving its max concurrent sessions", ErrNoAvailableAccounts)

// SetSessionLimitCache 注入会话登记表（wire 里由 ProvideOpenAIGatewayService 调用）。
func (s *OpenAIGatewayService) SetSessionLimitCache(cache SessionLimitCache) {
	if s != nil {
		s.sessionLimitCache = cache
	}
}

// zhipuSessionCapApplies 判断这个号有没有会话上限。
func zhipuSessionCapApplies(account *Account) bool {
	return account != nil && account.Platform == PlatformZhipu && account.GetMaxSessions() > 0
}

// admitZhipuSession 在选中的号上登记会话。返回 false 表示这个号已经服务满了别的会话。
func (s *OpenAIGatewayService) admitZhipuSession(ctx context.Context, account *Account, sessionHash string) bool {
	if s == nil || s.sessionLimitCache == nil || sessionHash == "" || !zhipuSessionCapApplies(account) {
		return true
	}
	idle := time.Duration(account.GetSessionIdleTimeoutMinutes()) * time.Minute
	allowed, err := s.sessionLimitCache.RegisterSession(ctx, account.ID, sessionHash, account.GetMaxSessions(), idle)
	if err != nil {
		slog.Warn("zhipu_session_cap.register_failed_open", "account_id", account.ID, "error", err)
		return true
	}
	return allowed
}

// selectAccountWithSchedulerAdmitted 在 selectAccountWithSchedulerBySize 外面加智谱会话上限：
// 选中的号满了，就释放刚占的槽位、排除这个号再选。非智谱号和没设上限的号不受影响。
func (s *OpenAIGatewayService) selectAccountWithSchedulerAdmitted(
	ctx context.Context,
	groupID *int64,
	previousResponseID string,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredTransport OpenAIUpstreamTransport,
	requiredCapability OpenAIEndpointCapability,
	requiredImageCapability OpenAIImagesCapability,
	requireCompact bool,
	platform string,
	previousResponseCanMove bool,
	useUpstreamTokenCost bool,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	return s.admitZhipuSessionSelection(ctx, sessionHash, excludedIDs,
		func(excluded map[int64]struct{}) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
			return s.selectAccountWithSchedulerBySize(ctx, groupID, previousResponseID, sessionHash, requestedModel, excluded, requiredTransport, requiredCapability, requiredImageCapability, requireCompact, platform, previousResponseCanMove, useUpstreamTokenCost)
		})
}

// admitZhipuSessionSelection 是会话上限的重选循环，selectFn 按给定的排除集选一次号。
// 调用方传进来的排除集（换号失败过的号）不改动，会话满的号记在副本里。
func (s *OpenAIGatewayService) admitZhipuSessionSelection(
	ctx context.Context,
	sessionHash string,
	excludedIDs map[int64]struct{},
	selectFn func(excluded map[int64]struct{}) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error),
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	excluded := excludedIDs
	cloned := false
	for pass := 0; ; pass++ {
		selection, decision, err := selectFn(excluded)
		if err != nil || selection == nil || selection.Account == nil {
			if pass > 0 && err != nil {
				// 前面几轮排除的号都是因为会话满：如实告诉调用方原因。
				return nil, decision, errZhipuSessionCapReached
			}
			return selection, decision, err
		}
		if s.admitZhipuSession(ctx, selection.Account, sessionHash) {
			return selection, decision, nil
		}
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		id := selection.Account.ID
		slog.Info("zhipu_session_cap.account_full", "account_id", id, "max_sessions", selection.Account.GetMaxSessions(), "pass", pass)
		if !cloned {
			excluded = cloneExcludedAccountIDs(excluded)
			if excluded == nil {
				excluded = make(map[int64]struct{})
			}
			cloned = true
		}
		if _, dup := excluded[id]; dup || pass+1 >= maxZhipuSessionAdmissionPasses {
			return nil, decision, errZhipuSessionCapReached
		}
		excluded[id] = struct{}{}
	}
}
