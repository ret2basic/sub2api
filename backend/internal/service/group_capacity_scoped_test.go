package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// 2026-11-02：GLM 的槽位按模型分桶（concurrency:account:{id}:m:{scope}），账号级批量读取
// 看不到，pool-capacity 的 concurrency_used 恒为 0。下面钉住汇总口径：每号上限不超过账号
// 并发的桶（glm-5.3=6，账号并发 6）计入 ConcurrencyUsed；上限更大的桶（glm-5.3-flash=50）
// 只进 ConcurrencyUsedByModel，否则 used 会远超 max，把工具误导成「池满」。

type scopedCapacityCacheStub struct {
	groupCapacityConcurrencyCacheStub
	scoped          map[int64]map[string]int
	requestedScopes []string
}

func (s *scopedCapacityCacheStub) GetAccountScopedConcurrencyBatch(_ context.Context, accountIDs []int64, scopes []string) (map[int64]map[string]int, error) {
	s.requestedScopes = append([]string(nil), scopes...)
	out := map[int64]map[string]int{}
	for _, id := range accountIDs {
		if perScope, ok := s.scoped[id]; ok {
			out[id] = perScope
		}
	}
	return out, nil
}

type scopedCapacityAccountRepoStub struct {
	AccountRepository
	accounts []Account
}

func (s *scopedCapacityAccountRepoStub) ListSchedulableByGroupID(context.Context, int64) ([]Account, error) {
	return append([]Account(nil), s.accounts...), nil
}

func newScopedCapacityCache() *scopedCapacityCacheStub {
	return &scopedCapacityCacheStub{
		groupCapacityConcurrencyCacheStub: groupCapacityConcurrencyCacheStub{counts: map[int64]int{1: 1}},
		scoped: map[int64]map[string]int{
			1: {"glm-5.3": 2, "glm-5.3-flash": 7},
			2: {"glm-5.3": 3},
		},
	}
}

func TestGroupCapacityByIDCountsSameUnitModelBuckets(t *testing.T) {
	cache := newScopedCapacityCache()
	repo := &scopedCapacityAccountRepoStub{accounts: []Account{{ID: 1, Concurrency: 6, Platform: PlatformZhipu}, {ID: 2, Concurrency: 6, Platform: PlatformZhipu}}}
	svc := NewGroupCapacityService(repo, nil, NewConcurrencyService(cache), nil, nil)
	svc.SetModelConcurrency(map[string]int{"glm-5.3": 6, "glm-5.3-flash": 50, "": 9, "zero": 0, "mystery-model": 4})

	summary, err := svc.GetGroupCapacityByID(context.Background(), 9)
	require.NoError(t, err)
	require.Equal(t, []string{"glm-5.3", "glm-5.3-flash", "mystery-model"}, cache.requestedScopes)
	require.Equal(t, 12, summary.ConcurrencyMax)
	require.Equal(t, 1+2+3, summary.ConcurrencyUsed, "账号级 1 + glm-5.3 桶 5；flash 桶不计入")
	require.Equal(t, map[string]int{"glm-5.3": 5, "glm-5.3-flash": 7}, summary.ConcurrencyUsedByModel)
	require.Equal(t, map[string]int{"glm-5.3": 12, "glm-5.3-flash": 100}, summary.ConcurrencyMaxByModel, "识别不出平台的模型不给容量")
}

func TestGroupCapacityBatchSplitsModelBucketsPerGroup(t *testing.T) {
	cache := newScopedCapacityCache()
	accountRepo := &groupCapacityAccountRepoStub{rows: []GroupAccountCapacityRow{
		{GroupID: 9, AccountID: 1, Concurrency: 6},
		{GroupID: 9, AccountID: 2, Concurrency: 6},
		{GroupID: 16, AccountID: 3, Concurrency: 5},
	}}
	groupRepo := &groupCapacityGroupRepoStub{groupIDs: []int64{9, 16}}
	svc := NewGroupCapacityService(accountRepo, groupRepo, NewConcurrencyService(cache), nil, nil)
	svc.SetModelConcurrency(map[string]int{"glm-5.3": 6, "glm-5.3-flash": 50})

	results, err := svc.GetAllGroupCapacity(context.Background())
	require.NoError(t, err)
	require.Len(t, results, 2)
	require.Equal(t, 6, results[0].ConcurrencyUsed)
	require.Equal(t, map[string]int{"glm-5.3": 5, "glm-5.3-flash": 7}, results[0].ConcurrencyUsedByModel)
	require.Equal(t, 0, results[1].ConcurrencyUsed)
	require.Nil(t, results[1].ConcurrencyUsedByModel, "没有分桶占用的分组不带这个字段")
}

func TestGroupCapacityWithoutModelConcurrencyKeepsOldBehaviour(t *testing.T) {
	cache := newScopedCapacityCache()
	repo := &scopedCapacityAccountRepoStub{accounts: []Account{{ID: 1, Concurrency: 6}, {ID: 2, Concurrency: 6}}}
	svc := NewGroupCapacityService(repo, nil, NewConcurrencyService(cache), nil, nil)

	summary, err := svc.GetGroupCapacityByID(context.Background(), 9)
	require.NoError(t, err)
	require.Nil(t, cache.requestedScopes, "没配分桶时不去读分桶键")
	require.Equal(t, 1, summary.ConcurrencyUsed)
	require.Nil(t, summary.ConcurrencyUsedByModel)
}

func TestGroupCapacityByIDGivesNoGLMCapacityToOtherPlatforms(t *testing.T) {
	cache := newScopedCapacityCache()
	cache.scoped = nil
	repo := &scopedCapacityAccountRepoStub{accounts: []Account{{ID: 25, Concurrency: 5, Platform: PlatformGrok}, {ID: 26, Concurrency: 5, Platform: PlatformGrok}}}
	svc := NewGroupCapacityService(repo, nil, NewConcurrencyService(cache), nil, nil)
	svc.SetModelConcurrency(map[string]int{"glm-5.3": 6, "glm-5.3-flash": 50})

	summary, err := svc.GetGroupCapacityByID(context.Background(), 16)
	require.NoError(t, err)
	require.Equal(t, 10, summary.ConcurrencyMax)
	require.Nil(t, summary.ConcurrencyMaxByModel, "Grok 池不该出现 glm 的分桶容量")
	require.Nil(t, summary.ConcurrencyUsedByModel)
}

// 2026-11-25：池子空闲（分桶里一个在途请求都没有）时，每个有容量的桶也要带出在途 0。
// 旧实现此时整个字段省略，客户端误判「网关没有分桶」，flash 准入退回 max 车道的推荐值。
func TestGroupCapacityByIDReportsIdleModelBucketsAsZero(t *testing.T) {
	cache := newScopedCapacityCache()
	cache.scoped = nil
	repo := &scopedCapacityAccountRepoStub{accounts: []Account{{ID: 1, Concurrency: 6, Platform: PlatformZhipu}, {ID: 2, Concurrency: 6, Platform: PlatformZhipu}}}
	svc := NewGroupCapacityService(repo, nil, NewConcurrencyService(cache), nil, nil)
	svc.SetModelConcurrency(map[string]int{"glm-5.3": 6, "glm-5.3-flash": 50})

	summary, err := svc.GetGroupCapacityByID(context.Background(), 9)
	require.NoError(t, err)
	require.Equal(t, map[string]int{"glm-5.3": 0, "glm-5.3-flash": 0}, summary.ConcurrencyUsedByModel)
	require.Equal(t, map[string]int{"glm-5.3": 12, "glm-5.3-flash": 100}, summary.ConcurrencyMaxByModel)
}

// 2026-11-27：Flash 每号上限降到 5，不超过账号并发 6，按旧规则 Flash 的在途会算进
// ConcurrencyUsed，各 saber 工具随之把 max 车道（glm-5.4 走账号级桶）的余量看小。
// model_concurrency_counted 写成空列表后，分桶一律不计入。
func TestGroupCapacityEmptyCountedScopesKeepsFlashOutOfPoolUsed(t *testing.T) {
	cache := newScopedCapacityCache()
	cache.scoped = map[int64]map[string]int{1: {"glm-5.3-flash": 4}, 2: {"glm-5.3-flash": 5}}
	repo := &scopedCapacityAccountRepoStub{accounts: []Account{{ID: 1, Concurrency: 6, Platform: PlatformZhipu}, {ID: 2, Concurrency: 6, Platform: PlatformZhipu}}}
	svc := NewGroupCapacityService(repo, nil, NewConcurrencyService(cache), nil, nil)
	svc.SetModelConcurrency(map[string]int{"glm-5.3-flash": 5})

	legacy, err := svc.GetGroupCapacityByID(context.Background(), 9)
	require.NoError(t, err)
	require.Equal(t, 1+4+5, legacy.ConcurrencyUsed, "旧规则：Flash 上限 5 不超过账号并发 6，被算进去")

	empty := []string{}
	svc.SetCountedModelScopes(&empty)
	summary, err := svc.GetGroupCapacityByID(context.Background(), 9)
	require.NoError(t, err)
	require.Equal(t, 1, summary.ConcurrencyUsed, "只剩账号级桶的 1 个在途")
	require.Equal(t, map[string]int{"glm-5.3-flash": 9}, summary.ConcurrencyUsedByModel)
	require.Equal(t, map[string]int{"glm-5.3-flash": 10}, summary.ConcurrencyMaxByModel)

	svc.SetCountedModelScopes(nil)
	back, err := svc.GetGroupCapacityByID(context.Background(), 9)
	require.NoError(t, err)
	require.Equal(t, 1+4+5, back.ConcurrencyUsed, "nil 回到旧规则")
}

func TestGroupCapacityCountedScopesCountsOnlyNamedBuckets(t *testing.T) {
	cache := newScopedCapacityCache()
	repo := &scopedCapacityAccountRepoStub{accounts: []Account{{ID: 1, Concurrency: 6, Platform: PlatformZhipu}, {ID: 2, Concurrency: 6, Platform: PlatformZhipu}}}
	svc := NewGroupCapacityService(repo, nil, NewConcurrencyService(cache), nil, nil)
	svc.SetModelConcurrency(map[string]int{"glm-5.3": 6, "glm-5.3-flash": 5})
	counted := []string{" glm-5.3 ", ""}
	svc.SetCountedModelScopes(&counted)

	summary, err := svc.GetGroupCapacityByID(context.Background(), 9)
	require.NoError(t, err)
	require.Equal(t, 1+2+3, summary.ConcurrencyUsed, "账号级 1 + glm-5.3 桶 5；Flash 上限虽然不超过 6，也不在列表里")
	require.Equal(t, map[string]int{"glm-5.3": 5, "glm-5.3-flash": 7}, summary.ConcurrencyUsedByModel)
}

func TestGroupCapacityBatchHonoursEmptyCountedScopes(t *testing.T) {
	cache := newScopedCapacityCache()
	accountRepo := &groupCapacityAccountRepoStub{rows: []GroupAccountCapacityRow{
		{GroupID: 9, AccountID: 1, Concurrency: 6},
		{GroupID: 9, AccountID: 2, Concurrency: 6},
	}}
	groupRepo := &groupCapacityGroupRepoStub{groupIDs: []int64{9}}
	svc := NewGroupCapacityService(accountRepo, groupRepo, NewConcurrencyService(cache), nil, nil)
	svc.SetModelConcurrency(map[string]int{"glm-5.3": 6, "glm-5.3-flash": 5})
	empty := []string{}
	svc.SetCountedModelScopes(&empty)

	results, err := svc.GetAllGroupCapacity(context.Background())
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, 1, results[0].ConcurrencyUsed)
	require.Equal(t, map[string]int{"glm-5.3": 5, "glm-5.3-flash": 7}, results[0].ConcurrencyUsedByModel)
}
