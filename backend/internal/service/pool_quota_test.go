package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// 池级额度（pool-capacity 的 quota 字段，2026-10-15）：GLM 与 Grok 互为替补时，
// 客户端靠 pace 判断哪一边的付费窗口烧得比匀速快。

var poolQuotaNow = time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC)

func zhipuQuotaAccount(id int64, usedPercent float64, resetIn time.Duration, updatedAgo time.Duration) Account {
	return Account{
		ID: id, Platform: PlatformZhipu, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Extra: map[string]any{
			"zhipu_weekly_used_percent": usedPercent,
			"zhipu_weekly_reset_at":     poolQuotaNow.Add(resetIn).Format(time.RFC3339),
			"zhipu_usage_updated_at":    poolQuotaNow.Add(-updatedAgo).Format(time.RFC3339),
		},
	}
}

func TestPoolQuotaZhipuWeeklyPace(t *testing.T) {
	week := time.Duration(poolQuotaWeekSeconds) * time.Second
	accounts := []Account{
		zhipuQuotaAccount(1, 50, week/2, time.Minute),   // elapsed 0.5
		zhipuQuotaAccount(2, 20, week/4, 2*time.Minute), // elapsed 0.75
	}
	q := BuildPoolQuotaSummary(PlatformZhipu, accounts, poolQuotaNow)
	require.NotNil(t, q)
	require.Equal(t, "weekly", q.BindingWindow)
	require.InDelta(t, 0.35, q.UsedFraction, 1e-4)
	require.InDelta(t, 0.625, q.ElapsedFraction, 1e-4)
	require.InDelta(t, 0.56, q.Pace, 1e-4)
	require.True(t, q.PaceKnown)
	require.Equal(t, poolQuotaWeekSeconds, q.WindowSeconds)
	require.Equal(t, 2, q.AccountsReporting)
	require.Equal(t, 0, q.AccountsExhausted)
	require.Equal(t, poolQuotaNow.Add(week/4).Format(time.RFC3339), q.ResetAt)
}

func TestPoolQuotaSkipsStaleInactiveAndDisabledAccounts(t *testing.T) {
	week := time.Duration(poolQuotaWeekSeconds) * time.Second
	stale := zhipuQuotaAccount(1, 90, week/2, 5*time.Hour)
	inactive := zhipuQuotaAccount(2, 90, week/2, time.Minute)
	inactive.Status = StatusDisabled
	disabled := zhipuQuotaAccount(3, 90, week/2, time.Minute)
	disabled.Schedulable = false
	fresh := zhipuQuotaAccount(4, 10, week/2, time.Minute)
	q := BuildPoolQuotaSummary(PlatformZhipu, []Account{stale, inactive, disabled, fresh}, poolQuotaNow)
	require.NotNil(t, q)
	require.Equal(t, 1, q.AccountsReporting)
	require.InDelta(t, 0.10, q.UsedFraction, 1e-4)
}

func TestPoolQuotaRolledOverWindowStartsFresh(t *testing.T) {
	// 快照之后窗口已经翻页：旧的 100% 不能把池子算成耗尽。
	q := BuildPoolQuotaSummary(PlatformZhipu, []Account{zhipuQuotaAccount(1, 100, -time.Hour, time.Minute)}, poolQuotaNow)
	require.NotNil(t, q)
	require.Zero(t, q.UsedFraction)
	require.Zero(t, q.AccountsExhausted)
}

func TestPoolQuotaCountsRateLimitedAndFullAccountsAsExhausted(t *testing.T) {
	week := time.Duration(poolQuotaWeekSeconds) * time.Second
	full := zhipuQuotaAccount(1, 100, week/3, time.Minute)
	limited := zhipuQuotaAccount(2, 40, week/3, time.Minute)
	until := time.Now().Add(time.Hour)
	limited.RateLimitResetAt = &until
	q := BuildPoolQuotaSummary(PlatformZhipu, []Account{full, limited}, poolQuotaNow)
	require.NotNil(t, q)
	require.Equal(t, 2, q.AccountsExhausted)
}

func TestPoolQuotaNilWithoutReports(t *testing.T) {
	require.Nil(t, BuildPoolQuotaSummary(PlatformZhipu, nil, poolQuotaNow))
	require.Nil(t, BuildPoolQuotaSummary(PlatformZhipu, []Account{{ID: 1, Platform: PlatformZhipu, Status: StatusActive, Schedulable: true}}, poolQuotaNow))
	// 不认识的平台不汇总。
	require.Nil(t, BuildPoolQuotaSummary(PlatformOpenAI, []Account{zhipuQuotaAccount(1, 10, time.Hour, time.Minute)}, poolQuotaNow))
}

func grokQuotaAccount(extra map[string]any) Account {
	return Account{ID: 25, Platform: PlatformGrok, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Extra: extra}
}

func TestPoolQuotaGrokBillingWeeklyWindow(t *testing.T) {
	start := poolQuotaNow.Add(-84 * time.Hour) // 3.5 天前开窗
	account := grokQuotaAccount(map[string]any{
		grokBillingExtraKey: map[string]any{
			"usage_percent":     30.0,
			"period_start":      start.Format(time.RFC3339Nano),
			"period_end":        start.Add(7 * 24 * time.Hour).Format(time.RFC3339Nano),
			"weekly_updated_at": poolQuotaNow.Add(-time.Minute).Format(time.RFC3339),
		},
	})
	q := BuildPoolQuotaSummary(PlatformGrok, []Account{account}, poolQuotaNow)
	require.NotNil(t, q)
	require.Equal(t, "weekly", q.BindingWindow)
	require.InDelta(t, 0.30, q.UsedFraction, 1e-4)
	require.InDelta(t, 0.50, q.ElapsedFraction, 1e-4)
	require.InDelta(t, 0.60, q.Pace, 1e-4)
	require.True(t, q.PaceKnown)
}

func TestPoolQuotaGrokFallsBackToRollingHeaders(t *testing.T) {
	// 账单快照没有用量百分比（新号常见）：退回限流头的滚动窗口，pace 中性。
	account := grokQuotaAccount(map[string]any{
		grokBillingExtraKey: map[string]any{
			"period_start":      poolQuotaNow.Add(-time.Hour).Format(time.RFC3339Nano),
			"period_end":        poolQuotaNow.Add(167 * time.Hour).Format(time.RFC3339Nano),
			"weekly_updated_at": poolQuotaNow.Format(time.RFC3339),
		},
		"grok_sched_utilization":      40.0,
		"grok_sched_usage_updated_at": poolQuotaNow.Add(-time.Minute).Format(time.RFC3339),
	})
	q := BuildPoolQuotaSummary(PlatformGrok, []Account{account}, poolQuotaNow)
	require.NotNil(t, q)
	require.Equal(t, "rolling", q.BindingWindow)
	require.InDelta(t, 0.40, q.UsedFraction, 1e-4)
	require.Equal(t, 1.0, q.Pace)
	require.False(t, q.PaceKnown)
}

func TestPoolQuotaAcceptsRealBillingSnapshotTimestamps(t *testing.T) {
	// 线上 grok_billing_snapshot 的 period_* 是带毫秒与 +00:00 的格式。
	account := grokQuotaAccount(map[string]any{
		grokBillingExtraKey: map[string]any{
			"usage_percent":     12.5,
			"period_start":      "2026-10-15T16:56:41.791+00:00",
			"period_end":        "2026-10-22T16:56:41.791+00:00",
			"weekly_updated_at": "2026-10-15T17:59:28Z",
		},
	})
	q := BuildPoolQuotaSummary(PlatformGrok, []Account{account}, poolQuotaNow)
	require.NotNil(t, q)
	require.Equal(t, "weekly", q.BindingWindow)
	require.InDelta(t, 0.125, q.UsedFraction, 1e-4)
}

type fakeGrokBillingProber struct {
	mu     sync.Mutex
	probed []int64
}

func (f *fakeGrokBillingProber) ProbeBilling(ctx context.Context, accountID int64) (*GrokQuotaProbeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probed = append(f.probed, accountID)
	return &GrokQuotaProbeResult{}, nil
}

func TestCNBalanceCheckRefreshesStaleGrokBillingOnly(t *testing.T) {
	now := time.Now().UTC()
	fresh := Account{ID: 1, Platform: PlatformGrok, Type: AccountTypeOAuth, Status: StatusActive,
		Extra: map[string]any{grokBillingExtraKey: map[string]any{"updated_at": now.Add(-time.Minute).Format(time.RFC3339)}}}
	stale := Account{ID: 2, Platform: PlatformGrok, Type: AccountTypeOAuth, Status: StatusActive,
		Extra: map[string]any{grokBillingExtraKey: map[string]any{"updated_at": now.Add(-time.Hour).Format(time.RFC3339)}}}
	missing := Account{ID: 3, Platform: PlatformGrok, Type: AccountTypeOAuth, Status: StatusActive}
	apiKey := Account{ID: 4, Platform: PlatformGrok, Type: AccountTypeAPIKey, Status: StatusActive}
	disabled := Account{ID: 5, Platform: PlatformGrok, Type: AccountTypeOAuth, Status: StatusDisabled}
	repo := &fakeCNCheckRepo{byPlatform: map[string][]Account{
		PlatformGrok: {fresh, stale, missing, apiKey, disabled},
	}}
	prober := &fakeGrokBillingProber{}
	svc := &CNProviderBalanceCheckService{accountRepo: repo, cfg: &config.Config{}}
	svc.SetGrokBillingProber(prober)

	svc.runOnce()

	require.ElementsMatch(t, []int64{2, 3}, prober.probed)
}
