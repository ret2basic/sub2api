//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// scopedCacheStubForTest 在既有 stub 基础上实现按模型分桶扩展，并记录调用参数。
type scopedCacheStubForTest struct {
	*stubConcurrencyCacheForTest

	scopedAcquireCalls []scopedAcquireCall
	scopedReleaseCalls []scopedReleaseCall
	scopedLoadCalls    int
	scopedLoadBatch    map[int64]*AccountLoadInfo
	scopedAcquireOK    bool
}

type scopedAcquireCall struct {
	accountID int64
	scope     string
	max       int
	requestID string
}

type scopedReleaseCall struct {
	accountID int64
	scope     string
	requestID string
}

func (c *scopedCacheStubForTest) AcquireAccountSlotScoped(_ context.Context, accountID int64, scope string, maxConcurrency int, requestID string) (bool, error) {
	c.scopedAcquireCalls = append(c.scopedAcquireCalls, scopedAcquireCall{
		accountID: accountID,
		scope:     scope,
		max:       maxConcurrency,
		requestID: requestID,
	})
	return c.scopedAcquireOK, nil
}

func (c *scopedCacheStubForTest) ReleaseAccountSlotScoped(_ context.Context, accountID int64, scope, requestID string) error {
	c.scopedReleaseCalls = append(c.scopedReleaseCalls, scopedReleaseCall{
		accountID: accountID,
		scope:     scope,
		requestID: requestID,
	})
	return nil
}

func (c *scopedCacheStubForTest) GetAccountsLoadBatchScoped(_ context.Context, accounts []AccountWithConcurrency, _ string) (map[int64]*AccountLoadInfo, error) {
	c.scopedLoadCalls++
	if c.scopedLoadBatch != nil {
		return c.scopedLoadBatch, nil
	}
	result := make(map[int64]*AccountLoadInfo, len(accounts))
	for _, acc := range accounts {
		result[acc.ID] = &AccountLoadInfo{AccountID: acc.ID}
	}
	return result, nil
}

var _ ScopedAccountConcurrencyCache = (*scopedCacheStubForTest)(nil)

func TestConcurrencyService_AcquireAccountSlotScopedUsesScopedBucket(t *testing.T) {
	base := &stubConcurrencyCacheForTest{}
	cache := &scopedCacheStubForTest{stubConcurrencyCacheForTest: base, scopedAcquireOK: true}
	svc := NewConcurrencyService(cache)

	result, err := svc.AcquireAccountSlotScoped(context.Background(), 22, "glm-5.3-flash", 50)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Acquired)

	require.Len(t, cache.scopedAcquireCalls, 1)
	call := cache.scopedAcquireCalls[0]
	require.Equal(t, int64(22), call.accountID)
	require.Equal(t, "glm-5.3-flash", call.scope)
	require.Equal(t, 50, call.max)
	require.NotEmpty(t, call.requestID)

	require.NotNil(t, result.ReleaseFunc)
	result.ReleaseFunc()
	require.Len(t, cache.scopedReleaseCalls, 1)
	release := cache.scopedReleaseCalls[0]
	require.Equal(t, int64(22), release.accountID)
	require.Equal(t, "glm-5.3-flash", release.scope)
	require.Equal(t, call.requestID, release.requestID, "释放必须用同一个 requestID 落回分桶键")
}

func TestConcurrencyService_AcquireAccountSlotScopedEmptyScopeDelegates(t *testing.T) {
	base := &stubConcurrencyCacheForTest{acquireResult: true}
	cache := &scopedCacheStubForTest{stubConcurrencyCacheForTest: base}
	svc := NewConcurrencyService(cache)

	result, err := svc.AcquireAccountSlotScoped(context.Background(), 22, "", 6)
	require.NoError(t, err)
	require.True(t, result.Acquired)
	require.Empty(t, cache.scopedAcquireCalls, "空作用域必须走账号级单桶")
	result.ReleaseFunc()
	require.NotEmpty(t, base.releasedAccountIDs)
}

func TestConcurrencyService_AcquireAccountSlotScopedFallsBackWithoutSupport(t *testing.T) {
	base := &stubConcurrencyCacheForTest{acquireResult: true}
	svc := NewConcurrencyService(base)

	result, err := svc.AcquireAccountSlotScoped(context.Background(), 22, "glm-5.3-flash", 50)
	require.NoError(t, err)
	require.True(t, result.Acquired, "未实现分桶扩展的 cache 应回退账号级单桶")
	require.NotNil(t, result.ReleaseFunc)
	result.ReleaseFunc()
	require.Len(t, base.releasedAccountIDs, 1)
	require.Equal(t, int64(22), base.releasedAccountIDs[0])
}

func TestConcurrencyService_GetAccountsLoadBatchScopedPrefersScopedRead(t *testing.T) {
	base := &stubConcurrencyCacheForTest{}
	cache := &scopedCacheStubForTest{stubConcurrencyCacheForTest: base}
	svc := NewConcurrencyService(cache)

	load, err := svc.GetAccountsLoadBatchScoped(context.Background(), []AccountWithConcurrency{{ID: 22, MaxConcurrency: 50}}, "glm-5.3-flash")
	require.NoError(t, err)
	require.NotNil(t, load[22])
	require.Equal(t, 1, cache.scopedLoadCalls)
	require.Zero(t, base.loadBatchCalls.Load(), "分桶读取不得落到账号级负载缓存")
}

func TestConcurrencyService_GetAccountsLoadBatchScopedFallsBackWithoutSupport(t *testing.T) {
	base := &stubConcurrencyCacheForTest{loadBatch: map[int64]*AccountLoadInfo{22: {AccountID: 22}}}
	svc := NewConcurrencyService(base)

	load, err := svc.GetAccountsLoadBatchScoped(context.Background(), []AccountWithConcurrency{{ID: 22, MaxConcurrency: 6}}, "glm-5.3-flash")
	require.NoError(t, err)
	require.NotNil(t, load[22])
	require.Equal(t, int64(1), base.loadBatchCalls.Load(), "未实现分桶时回退账号级读取")
}
