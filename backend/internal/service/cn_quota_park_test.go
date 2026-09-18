package service

// 2026-09-18 kimi 池回归：窗口烧满时冷却终点必须落在「已耗尽窗口」的重置点上。
// 修复前一律取最早重置点：5h 窗口空着、weekly 烧满的账号只被冷却到 5h 翻页，
// 回到调度再吃一次 403，面板在「限流中」与「正常」之间来回跳。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCNProviderQuotaSnapshotResetPrefersExhaustedWindow(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	reset5h := now.Add(4 * time.Hour)
	weekly := now.Add(3 * 24 * time.Hour)

	t.Run("5h empty and weekly exhausted picks weekly", func(t *testing.T) {
		acct := kimiCodingAccount(map[string]any{
			cnExtraKey(PlatformKimi, cnExtraSuffix5hReset):     reset5h.Format(time.RFC3339),
			cnExtraKey(PlatformKimi, cnExtraSuffix5hUsed):      0,
			cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyReset): weekly.Format(time.RFC3339),
			cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyUsed):  100,
		})
		got := cnProviderQuotaSnapshotReset(acct, now)
		if got == nil || !got.Equal(weekly) {
			t.Fatalf("want weekly reset %v, got %v", weekly, got)
		}
	})

	t.Run("both windows exhausted picks the latest reset", func(t *testing.T) {
		acct := kimiCodingAccount(map[string]any{
			cnExtraKey(PlatformKimi, cnExtraSuffix5hReset):     now.Add(time.Hour).Format(time.RFC3339),
			cnExtraKey(PlatformKimi, cnExtraSuffix5hUsed):      100,
			cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyReset): weekly.Format(time.RFC3339),
			cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyUsed):  100,
		})
		got := cnProviderQuotaSnapshotReset(acct, now)
		if got == nil || !got.Equal(weekly) {
			t.Fatalf("want latest exhausted reset %v, got %v", weekly, got)
		}
	})

	t.Run("only 5h exhausted keeps 5h reset", func(t *testing.T) {
		acct := kimiCodingAccount(map[string]any{
			cnExtraKey(PlatformKimi, cnExtraSuffix5hReset):     reset5h.Format(time.RFC3339),
			cnExtraKey(PlatformKimi, cnExtraSuffix5hUsed):      100,
			cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyReset): weekly.Format(time.RFC3339),
			cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyUsed):  12,
		})
		got := cnProviderQuotaSnapshotReset(acct, now)
		if got == nil || !got.Equal(reset5h) {
			t.Fatalf("want 5h reset %v, got %v", reset5h, got)
		}
	})

	t.Run("nothing exhausted falls back to earliest reset", func(t *testing.T) {
		acct := kimiCodingAccount(map[string]any{
			cnExtraKey(PlatformKimi, cnExtraSuffix5hReset):     reset5h.Format(time.RFC3339),
			cnExtraKey(PlatformKimi, cnExtraSuffix5hUsed):      20,
			cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyReset): weekly.Format(time.RFC3339),
			cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyUsed):  40,
		})
		got := cnProviderQuotaSnapshotReset(acct, now)
		if got == nil || !got.Equal(reset5h) {
			t.Fatalf("want earliest reset %v, got %v", reset5h, got)
		}
	})
}

func TestCNQuotaExhaustedWindowReset(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	soon := now.Add(2 * time.Hour)
	later := now.Add(72 * time.Hour)
	iso := func(t time.Time) string { return t.Format(time.RFC3339) }

	t.Run("picks the latest exhausted window", func(t *testing.T) {
		got := cnQuotaExhaustedWindowReset([]CNQuotaTier{
			{Window: "5h", UsedPercent: 100, ResetAt: iso(soon)},
			{Window: "weekly", UsedPercent: 100, ResetAt: iso(later)},
		}, now)
		require.NotNil(t, got)
		require.True(t, got.Equal(later))
	})

	t.Run("ignores windows with headroom", func(t *testing.T) {
		got := cnQuotaExhaustedWindowReset([]CNQuotaTier{
			{Window: "5h", UsedPercent: 67, ResetAt: iso(soon)},
			{Window: "weekly", UsedPercent: 99, ResetAt: iso(later)},
		}, now)
		require.Nil(t, got)
	})

	t.Run("ignores already expired resets", func(t *testing.T) {
		got := cnQuotaExhaustedWindowReset([]CNQuotaTier{
			{Window: "weekly", UsedPercent: 100, ResetAt: iso(now.Add(-time.Hour))},
		}, now)
		require.Nil(t, got)
	})
}

type quotaParkRecorder struct {
	AccountRepository
	rateLimited []time.Time
	cleared     int
}

func (r *quotaParkRecorder) SetRateLimited(_ context.Context, _ int64, resetAt time.Time) error {
	r.rateLimited = append(r.rateLimited, resetAt)
	return nil
}

func (r *quotaParkRecorder) ClearRateLimit(_ context.Context, _ int64) error {
	r.cleared++
	return nil
}

func TestApplyQuotaWindowSchedulingParksAndUnparks(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	weekly := now.Add(72 * time.Hour)
	snapshot := func(weeklyUsed float64) []CNQuotaTier {
		return []CNQuotaTier{
			{Window: "5h", UsedPercent: 0, ResetAt: now.Add(3 * time.Hour).Format(time.RFC3339)},
			{Window: "weekly", UsedPercent: weeklyUsed, ResetAt: weekly.Format(time.RFC3339)},
		}
	}

	t.Run("exhausted window parks until its reset", func(t *testing.T) {
		repo := &quotaParkRecorder{}
		svc := &CNProviderQuotaService{accountRepo: repo}
		svc.applyQuotaWindowScheduling(context.Background(), &Account{ID: 9, Platform: PlatformKimi}, PlatformKimi, snapshot(100), now)
		require.Len(t, repo.rateLimited, 1)
		require.True(t, repo.rateLimited[0].Equal(weekly))
		require.Zero(t, repo.cleared)
	})

	t.Run("recovered snapshot clears an active quota rate limit", func(t *testing.T) {
		repo := &quotaParkRecorder{}
		svc := &CNProviderQuotaService{accountRepo: repo}
		until := now.Add(48 * time.Hour)
		account := &Account{ID: 2, Platform: PlatformKimi, RateLimitResetAt: &until}
		svc.applyQuotaWindowScheduling(context.Background(), account, PlatformKimi, snapshot(0), now)
		require.Empty(t, repo.rateLimited)
		require.Equal(t, 1, repo.cleared)
	})

	t.Run("healthy account without rate limit is untouched", func(t *testing.T) {
		repo := &quotaParkRecorder{}
		svc := &CNProviderQuotaService{accountRepo: repo}
		svc.applyQuotaWindowScheduling(context.Background(), &Account{ID: 16, Platform: PlatformKimi}, PlatformKimi, snapshot(0), now)
		require.Empty(t, repo.rateLimited)
		require.Zero(t, repo.cleared)
	})
}
