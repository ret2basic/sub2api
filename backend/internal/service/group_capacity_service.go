package service

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// GroupCapacitySummary holds aggregated capacity for a single group.
type GroupCapacitySummary struct {
	GroupID         int64 `json:"group_id"`
	ConcurrencyUsed int   `json:"concurrency_used"`
	ConcurrencyMax  int   `json:"concurrency_max"`
	SessionsUsed    int   `json:"sessions_used"`
	SessionsMax     int   `json:"sessions_max"`
	RPMUsed         int   `json:"rpm_used"`
	RPMMax          int   `json:"rpm_max"`
	// ConcurrencyUsedByModel 是按模型分桶的在途槽位（gateway.scheduling.model_concurrency
	// 里配置的模型）。上限大于账号并发的桶（如 glm-5.3-flash）只出现在这里，不计入
	// ConcurrencyUsed。
	ConcurrencyUsedByModel map[string]int `json:"concurrency_used_by_model,omitempty"`
	// ConcurrencyMaxByModel 是各模型分桶的容量：该模型所属平台的可调度账号上，每号
	// 模型上限之和。只在单分组汇总（pool-capacity 用的 GetGroupCapacityByID）里给出。
	ConcurrencyMaxByModel map[string]int `json:"concurrency_max_by_model,omitempty"`
}

// GroupAccountCapacityRow is the lightweight account projection needed for
// capacity summary aggregation.
type GroupAccountCapacityRow struct {
	GroupID             int64
	AccountID           int64
	Concurrency         int
	Extra               map[string]any
	SessionWindowStart  *time.Time
	SessionWindowEnd    *time.Time
	SessionWindowStatus string
}

type groupCapacityActiveGroupIDLister interface {
	ListActiveIDs(ctx context.Context) ([]int64, error)
}

type groupCapacityAccountLister interface {
	ListSchedulableCapacityByGroupIDs(ctx context.Context, groupIDs []int64) ([]GroupAccountCapacityRow, error)
}

// GroupCapacityService aggregates per-group capacity from runtime data.
type GroupCapacityService struct {
	accountRepo        AccountRepository
	groupRepo          GroupRepository
	concurrencyService *ConcurrencyService
	sessionLimitCache  SessionLimitCache
	rpmCache           RPMCache
	// modelConcurrency 是 gateway.scheduling.model_concurrency（模型 → 每号槽位上限）。
	modelConcurrency map[string]int
	// countedScopes 是 gateway.scheduling.model_concurrency_counted：非 nil 时只有列在
	// 里面的分桶计入 ConcurrencyUsed；nil 沿用旧规则（见 countsTowardPoolUsed）。
	countedScopes map[string]struct{}
}

// NewGroupCapacityService creates a new GroupCapacityService.
func NewGroupCapacityService(
	accountRepo AccountRepository,
	groupRepo GroupRepository,
	concurrencyService *ConcurrencyService,
	sessionLimitCache SessionLimitCache,
	rpmCache RPMCache,
) *GroupCapacityService {
	return &GroupCapacityService{
		accountRepo:        accountRepo,
		groupRepo:          groupRepo,
		concurrencyService: concurrencyService,
		sessionLimitCache:  sessionLimitCache,
		rpmCache:           rpmCache,
	}
}

// GetAllGroupCapacity returns capacity summary for all active groups.
// ProvideGroupCapacityService 在 NewGroupCapacityService 之外带上按模型分桶配置，
// 让容量汇总能读到分桶槽位（2026-11-02）。
func ProvideGroupCapacityService(
	accountRepo AccountRepository,
	groupRepo GroupRepository,
	concurrencyService *ConcurrencyService,
	sessionLimitCache SessionLimitCache,
	rpmCache RPMCache,
	cfg *config.Config,
) *GroupCapacityService {
	s := NewGroupCapacityService(accountRepo, groupRepo, concurrencyService, sessionLimitCache, rpmCache)
	if cfg != nil {
		s.SetModelConcurrency(cfg.Gateway.Scheduling.ModelConcurrency)
		s.SetCountedModelScopes(cfg.Gateway.Scheduling.ModelConcurrencyCounted)
	}
	return s
}

// SetModelConcurrency 设置按模型分桶的每号槽位上限（模型 → 上限）。
func (s *GroupCapacityService) SetModelConcurrency(modelConcurrency map[string]int) {
	if s == nil {
		return
	}
	copied := make(map[string]int, len(modelConcurrency))
	for model, limit := range modelConcurrency {
		if model != "" && limit > 0 {
			copied[model] = limit
		}
	}
	s.modelConcurrency = copied
}

// SetCountedModelScopes 设置哪些分桶的在途数计入 ConcurrencyUsed。nil 沿用旧规则；
// 空列表表示分桶一律不计入，ConcurrencyUsed 只剩账号级桶。
func (s *GroupCapacityService) SetCountedModelScopes(models *[]string) {
	if s == nil {
		return
	}
	if models == nil {
		s.countedScopes = nil
		return
	}
	set := make(map[string]struct{}, len(*models))
	for _, model := range *models {
		if model = strings.TrimSpace(model); model != "" {
			set[model] = struct{}{}
		}
	}
	s.countedScopes = set
}

// countsTowardPoolUsed 判断一个分桶的在途数是否计入 ConcurrencyUsed。
// 显式列表优先。旧规则「每号上限不超过账号并发」在 2026-11-27 Flash 降到每号 5 后失效：
// Flash 会被当成 max 车道的同口径桶，各工具看到的 max 车道余量被 Flash 吃掉。
func (s *GroupCapacityService) countsTowardPoolUsed(scope string, accountConcurrency int) bool {
	if s.countedScopes != nil {
		_, ok := s.countedScopes[scope]
		return ok
	}
	limit := s.modelConcurrency[scope]
	return limit > 0 && limit <= accountConcurrency
}

// scopedUsage 汇总按模型分桶的在途槽位。
//
// 分桶槽位落在 concurrency:account:{id}:m:{scope}，账号级批量读取看不到，2026-11-02
// 发现 GLM 池的 concurrency_used 因此恒为 0，三个 saber 工具算余量时看不到全机负载。
// 只有和 concurrency_max 同口径的桶计入 counted，其余只进 byModel，否则 used 会远超 max，
// 让工具误以为池满而停派。哪些桶同口径由 countsTowardPoolUsed 判断：配置了
// model_concurrency_counted 就按列表，否则按旧规则（每号上限不超过账号并发，如 glm-5.3=6）。
func (s *GroupCapacityService) scopedUsage(ctx context.Context, accountIDs []int64, accountConcurrency map[int64]int) (map[int64]int, map[int64]map[string]int) {
	counted := map[int64]int{}
	perAccount := map[int64]map[string]int{}
	if s.concurrencyService == nil || len(s.modelConcurrency) == 0 || len(accountIDs) == 0 {
		return counted, perAccount
	}
	scopes := make([]string, 0, len(s.modelConcurrency))
	for model := range s.modelConcurrency {
		scopes = append(scopes, model)
	}
	sort.Strings(scopes)
	usage := s.concurrencyService.GetAccountScopedConcurrencyBatch(ctx, accountIDs, scopes)
	for accountID, perScope := range usage {
		for scope, n := range perScope {
			if n <= 0 {
				continue
			}
			if perAccount[accountID] == nil {
				perAccount[accountID] = map[string]int{}
			}
			perAccount[accountID][scope] += n
			if s.countsTowardPoolUsed(scope, accountConcurrency[accountID]) {
				counted[accountID] += n
			}
		}
	}
	return counted, perAccount
}

// modelCapacity 按模型给出分桶容量：模型所属平台（DetectModelPlatform）的可调度账号上，
// 每号模型上限之和。zk-saber 2026-11-02 提出：只有占用、没有容量时，工具只能拿全池的
// recommended 减 glm-5.3 的占用去算 flash 余量，flash 被 glm-5.3 拖住。识别不出平台的模型
// 不给容量（宁缺勿错）；没有任何适用模型时返回 nil。
func (s *GroupCapacityService) modelCapacity(accounts []Account) map[string]int {
	if len(s.modelConcurrency) == 0 || len(accounts) == 0 {
		return nil
	}
	var out map[string]int
	for scope, limit := range s.modelConcurrency {
		platform, ok := DetectModelPlatform(scope)
		if !ok || limit <= 0 {
			continue
		}
		total := 0
		for i := range accounts {
			if accounts[i].Platform == platform {
				total += limit
			}
		}
		if total > 0 {
			if out == nil {
				out = map[string]int{}
			}
			out[scope] = total
		}
	}
	return out
}

// usedByModelWithZeros 让每个有容量的分桶都出现在在途数里，没有在途请求时写 0。
//
// 2026-11-25：concurrency_used_by_model 按 omitempty 输出，只含有在途请求的桶。flash
// 闲着时整个字段消失，客户端就当网关没有分桶，flash 准入退回 max 车道的推荐值
// （dlt-saber 自 11-22 起一直记 no-flash-bucket）。空闲时最该放宽准入，偏偏看不到桶。
func usedByModelWithZeros(used, maxByModel map[string]int) map[string]int {
	if len(maxByModel) == 0 {
		return used
	}
	out := make(map[string]int, len(maxByModel)+len(used))
	for scope := range maxByModel {
		out[scope] = 0
	}
	for scope, n := range used {
		out[scope] += n
	}
	return out
}

// addModelUsage 把一个账号的分桶在途数累加进汇总。
func addModelUsage(summary *GroupCapacitySummary, perScope map[string]int) {
	for scope, n := range perScope {
		if summary.ConcurrencyUsedByModel == nil {
			summary.ConcurrencyUsedByModel = map[string]int{}
		}
		summary.ConcurrencyUsedByModel[scope] += n
	}
}

func (s *GroupCapacityService) GetAllGroupCapacity(ctx context.Context) ([]GroupCapacitySummary, error) {
	groupIDs, err := s.listActiveGroupIDs(ctx)
	if err != nil {
		return nil, err
	}

	if lister, ok := s.accountRepo.(groupCapacityAccountLister); ok {
		return s.getGroupCapacitiesBatch(ctx, groupIDs, lister)
	}

	return s.getGroupCapacitiesSequential(ctx, groupIDs), nil
}

func (s *GroupCapacityService) listActiveGroupIDs(ctx context.Context) ([]int64, error) {
	if adminaccess.RestrictedFromContext(ctx) != nil {
		if repo, ok := s.groupRepo.(interface {
			ListScopedAdminCapacityGroupIDs(context.Context) ([]int64, error)
		}); ok {
			return repo.ListScopedAdminCapacityGroupIDs(ctx)
		}
		return nil, infraerrors.ServiceUnavailable("SCOPED_GROUP_CAPACITY_UNAVAILABLE", "受限分组容量服务不可用")
	}

	if lister, ok := s.groupRepo.(groupCapacityActiveGroupIDLister); ok {
		return lister.ListActiveIDs(ctx)
	}

	groups, err := s.groupRepo.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	groupIDs := make([]int64, 0, len(groups))
	for i := range groups {
		groupIDs = append(groupIDs, groups[i].ID)
	}
	return groupIDs, nil
}

func (s *GroupCapacityService) getGroupCapacitiesSequential(ctx context.Context, groupIDs []int64) []GroupCapacitySummary {
	results := make([]GroupCapacitySummary, 0, len(groupIDs))
	for _, groupID := range groupIDs {
		cap, err := s.GetGroupCapacityByID(ctx, groupID)
		if err != nil {
			// Skip groups with errors, return partial results
			continue
		}
		cap.GroupID = groupID
		results = append(results, cap)
	}
	return results
}

type groupCapacityAccountRef struct {
	groupID   int64
	accountID int64
}

func (s *GroupCapacityService) getGroupCapacitiesBatch(ctx context.Context, groupIDs []int64, lister groupCapacityAccountLister) ([]GroupCapacitySummary, error) {
	results := make([]GroupCapacitySummary, len(groupIDs))
	groupIndex := make(map[int64]int, len(groupIDs))
	for i, groupID := range groupIDs {
		results[i].GroupID = groupID
		groupIndex[groupID] = i
	}
	if len(groupIDs) == 0 {
		return results, nil
	}

	rows, err := lister.ListSchedulableCapacityByGroupIDs(ctx, groupIDs)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return results, nil
	}

	refs := make([]groupCapacityAccountRef, 0, len(rows))
	seenGroupAccount := make(map[groupCapacityAccountRef]struct{}, len(rows))
	accountIDSet := make(map[int64]struct{}, len(rows))
	accountIDs := make([]int64, 0, len(rows))
	sessionTimeouts := make(map[int64]time.Duration)
	accountConcurrency := make(map[int64]int, len(rows))

	for _, row := range rows {
		idx, ok := groupIndex[row.GroupID]
		if !ok || row.AccountID <= 0 {
			continue
		}

		ref := groupCapacityAccountRef{groupID: row.GroupID, accountID: row.AccountID}
		if _, ok := seenGroupAccount[ref]; ok {
			continue
		}
		seenGroupAccount[ref] = struct{}{}
		refs = append(refs, ref)

		if _, ok := accountIDSet[row.AccountID]; !ok {
			accountIDSet[row.AccountID] = struct{}{}
			accountIDs = append(accountIDs, row.AccountID)
		}

		acc := Account{
			ID:                  row.AccountID,
			Concurrency:         row.Concurrency,
			Extra:               row.Extra,
			SessionWindowStart:  row.SessionWindowStart,
			SessionWindowEnd:    row.SessionWindowEnd,
			SessionWindowStatus: row.SessionWindowStatus,
		}

		results[idx].ConcurrencyMax += acc.Concurrency
		accountConcurrency[acc.ID] = acc.Concurrency

		if maxSessions := acc.GetMaxSessions(); maxSessions > 0 {
			results[idx].SessionsMax += maxSessions
			timeout := time.Duration(acc.GetSessionIdleTimeoutMinutes()) * time.Minute
			if timeout <= 0 {
				timeout = 5 * time.Minute
			}
			sessionTimeouts[acc.ID] = timeout
		}

		if rpm := acc.GetBaseRPM(); rpm > 0 {
			results[idx].RPMMax += rpm
		}
	}

	if len(accountIDs) == 0 {
		return results, nil
	}

	concurrencyMap := map[int64]int{}
	if s.concurrencyService != nil {
		concurrencyMap, _ = s.concurrencyService.GetAccountConcurrencyBatch(ctx, accountIDs)
	}
	scopedCounted, scopedPerAccount := s.scopedUsage(ctx, accountIDs, accountConcurrency)

	sessionAccountIDs := accountIDsForGroupsWithLimit(refs, groupIndex, results, func(summary GroupCapacitySummary) bool {
		return summary.SessionsMax > 0
	})
	var sessionsMap map[int64]int
	if len(sessionAccountIDs) > 0 && s.sessionLimitCache != nil {
		sessionsMap, _ = s.sessionLimitCache.GetActiveSessionCountBatch(ctx, sessionAccountIDs, sessionTimeouts)
	}

	rpmAccountIDs := accountIDsForGroupsWithLimit(refs, groupIndex, results, func(summary GroupCapacitySummary) bool {
		return summary.RPMMax > 0
	})
	var rpmMap map[int64]int
	if len(rpmAccountIDs) > 0 && s.rpmCache != nil {
		rpmMap, _ = s.rpmCache.GetRPMBatch(ctx, rpmAccountIDs)
	}

	for _, ref := range refs {
		idx := groupIndex[ref.groupID]
		results[idx].ConcurrencyUsed += concurrencyMap[ref.accountID] + scopedCounted[ref.accountID]
		addModelUsage(&results[idx], scopedPerAccount[ref.accountID])
		if sessionsMap != nil && results[idx].SessionsMax > 0 {
			results[idx].SessionsUsed += sessionsMap[ref.accountID]
		}
		if rpmMap != nil && results[idx].RPMMax > 0 {
			results[idx].RPMUsed += rpmMap[ref.accountID]
		}
	}
	return results, nil
}

func accountIDsForGroupsWithLimit(refs []groupCapacityAccountRef, groupIndex map[int64]int, summaries []GroupCapacitySummary, include func(GroupCapacitySummary) bool) []int64 {
	seen := make(map[int64]struct{})
	accountIDs := make([]int64, 0)
	for _, ref := range refs {
		idx, ok := groupIndex[ref.groupID]
		if !ok || !include(summaries[idx]) {
			continue
		}
		if _, ok := seen[ref.accountID]; ok {
			continue
		}
		seen[ref.accountID] = struct{}{}
		accountIDs = append(accountIDs, ref.accountID)
	}
	return accountIDs
}

// GetGroupCapacityByID 返回单个分组容量聚合（可调度账号并发总和 + Redis
// 实时占用）。2026-09-15 起除管理台外，还被池 key 容量端点
// （/v1/sub2api/pool-capacity）复用，作为客户端动态并发上限的事实来源。
func (s *GroupCapacityService) GetGroupCapacityByID(ctx context.Context, groupID int64) (GroupCapacitySummary, error) {
	accounts, err := s.accountRepo.ListSchedulableByGroupID(ctx, groupID)
	if err != nil {
		return GroupCapacitySummary{}, err
	}
	if len(accounts) == 0 {
		return GroupCapacitySummary{}, nil
	}

	// Collect account IDs and config values
	accountIDs := make([]int64, 0, len(accounts))
	sessionTimeouts := make(map[int64]time.Duration)
	var concurrencyMax, sessionsMax, rpmMax int

	accountConcurrency := make(map[int64]int, len(accounts))
	for i := range accounts {
		acc := &accounts[i]
		accountIDs = append(accountIDs, acc.ID)
		concurrencyMax += acc.Concurrency
		accountConcurrency[acc.ID] = acc.Concurrency

		if ms := acc.GetMaxSessions(); ms > 0 {
			sessionsMax += ms
			timeout := time.Duration(acc.GetSessionIdleTimeoutMinutes()) * time.Minute
			if timeout <= 0 {
				timeout = 5 * time.Minute
			}
			sessionTimeouts[acc.ID] = timeout
		}

		if rpm := acc.GetBaseRPM(); rpm > 0 {
			rpmMax += rpm
		}
	}

	// Batch query runtime data from Redis
	concurrencyMap, _ := s.concurrencyService.GetAccountConcurrencyBatch(ctx, accountIDs)
	scopedCounted, scopedPerAccount := s.scopedUsage(ctx, accountIDs, accountConcurrency)
	maxByModel := s.modelCapacity(accounts)

	var sessionsMap map[int64]int
	if sessionsMax > 0 && s.sessionLimitCache != nil {
		sessionsMap, _ = s.sessionLimitCache.GetActiveSessionCountBatch(ctx, accountIDs, sessionTimeouts)
	}

	var rpmMap map[int64]int
	if rpmMax > 0 && s.rpmCache != nil {
		rpmMap, _ = s.rpmCache.GetRPMBatch(ctx, accountIDs)
	}

	// Aggregate
	var concurrencyUsed, sessionsUsed, rpmUsed int
	var byModel GroupCapacitySummary
	for _, id := range accountIDs {
		concurrencyUsed += concurrencyMap[id] + scopedCounted[id]
		addModelUsage(&byModel, scopedPerAccount[id])
		if sessionsMap != nil {
			sessionsUsed += sessionsMap[id]
		}
		if rpmMap != nil {
			rpmUsed += rpmMap[id]
		}
	}

	return GroupCapacitySummary{
		ConcurrencyUsed:        concurrencyUsed,
		ConcurrencyMax:         concurrencyMax,
		SessionsUsed:           sessionsUsed,
		SessionsMax:            sessionsMax,
		RPMUsed:                rpmUsed,
		RPMMax:                 rpmMax,
		ConcurrencyUsedByModel: usedByModelWithZeros(byModel.ConcurrencyUsedByModel, maxByModel),
		ConcurrencyMaxByModel:  maxByModel,
	}, nil
}
