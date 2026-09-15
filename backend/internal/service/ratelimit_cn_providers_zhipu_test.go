package service

import (
	"testing"
	"time"
)

// 2026-09-15 GLM 池事故回归测试：429 分级（zhipu 瞬时限流短冷却）与
// weekly 长窗口守卫（5h 快照过期不得直接跳到 weekly reset）。

func zhipuCodingAccount(extra map[string]any) *Account {
	return &Account{
		ID:     1,
		Name:   "glm-test",
		Status: "active",
		Credentials: map[string]any{
			"api_key":      "test-key",
			"account_mode": AccountModeCoding,
		},
		Extra:    extra,
		Platform: PlatformZhipu,
		Type:     "apikey",
	}
}

func TestZhipuTransientRateLimitCooldown(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want time.Duration
	}{
		{
			name: "1113 concurrency string code",
			body: []byte(`{"error":{"code":"1113","message":"您当前的使用量或并发量已达到上限"}}`),
			want: 10 * time.Minute,
		},
		{
			name: "1113 concurrency numeric code",
			body: []byte(`{"error":{"code":1113,"message":"Concurrency limit exceeded"}}`),
			want: 10 * time.Minute,
		},
		{
			name: "1302 frequency spaced code",
			body: []byte(`{"error":{"code": "1302","message":"请求频率超上限"}}`),
			want: 2 * time.Minute,
		},
		{
			name: "window exhausted 429 body",
			body: []byte(`{"error":{"code":"1301","message":"您当前的使用量已达到上限"}}`),
			want: 0,
		},
		{
			name: "empty body",
			body: nil,
			want: 0,
		},
		{
			name: "unrelated 1113 in tokens field",
			body: []byte(`{"usage":{"total_tokens":11130}}`),
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := zhipuTransientRateLimitCooldown(tc.body)
			if got != tc.want {
				t.Fatalf("zhipuTransientRateLimitCooldown(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestCNProviderQuotaSnapshotResetWeeklyGuard(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	future5h := now.Add(2 * time.Hour)
	expired5h := now.Add(-6 * time.Minute)
	weekly := now.Add(80 * time.Hour)

	t.Run("5h snapshot fresh returns 5h reset", func(t *testing.T) {
		acct := zhipuCodingAccount(map[string]any{
			cnExtraKey(PlatformZhipu, cnExtraSuffix5hReset):     future5h.Format(time.RFC3339),
			cnExtraKey(PlatformZhipu, cnExtraSuffix5hUsed):      100,
			cnExtraKey(PlatformZhipu, cnExtraSuffixWeeklyReset): weekly.Format(time.RFC3339),
			cnExtraKey(PlatformZhipu, cnExtraSuffixWeeklyUsed):  42,
		})
		got := cnProviderQuotaSnapshotReset(acct, now)
		if got == nil || !got.Equal(future5h) {
			t.Fatalf("want 5h reset %v, got %v", future5h, got)
		}
	})

	t.Run("expired 5h must not fall through to weekly when weekly used below 100", func(t *testing.T) {
		// 2026-09-15 事故形态：5h 快照刚过期，weekly 仍在未来且未耗尽。
		// 修复前会直接冷却到 weekly（3-4 天）；修复后返回 nil 走默认短冷却。
		acct := zhipuCodingAccount(map[string]any{
			cnExtraKey(PlatformZhipu, cnExtraSuffix5hReset):     expired5h.Format(time.RFC3339),
			cnExtraKey(PlatformZhipu, cnExtraSuffix5hUsed):      100,
			cnExtraKey(PlatformZhipu, cnExtraSuffixWeeklyReset): weekly.Format(time.RFC3339),
			cnExtraKey(PlatformZhipu, cnExtraSuffixWeeklyUsed):  42,
		})
		got := cnProviderQuotaSnapshotReset(acct, now)
		if got != nil {
			t.Fatalf("want nil (short fallback cooldown), got %v", got)
		}
	})

	t.Run("weekly allowed when snapshot confirms exhaustion", func(t *testing.T) {
		acct := zhipuCodingAccount(map[string]any{
			cnExtraKey(PlatformZhipu, cnExtraSuffix5hReset):     expired5h.Format(time.RFC3339),
			cnExtraKey(PlatformZhipu, cnExtraSuffix5hUsed):      100,
			cnExtraKey(PlatformZhipu, cnExtraSuffixWeeklyReset): weekly.Format(time.RFC3339),
			cnExtraKey(PlatformZhipu, cnExtraSuffixWeeklyUsed):  100,
		})
		got := cnProviderQuotaSnapshotReset(acct, now)
		if got == nil || !got.Equal(weekly) {
			t.Fatalf("want weekly reset %v, got %v", weekly, got)
		}
	})

	t.Run("weekly guard holds when used percent missing", func(t *testing.T) {
		acct := zhipuCodingAccount(map[string]any{
			cnExtraKey(PlatformZhipu, cnExtraSuffix5hReset):     expired5h.Format(time.RFC3339),
			cnExtraKey(PlatformZhipu, cnExtraSuffixWeeklyReset): weekly.Format(time.RFC3339),
		})
		got := cnProviderQuotaSnapshotReset(acct, now)
		if got != nil {
			t.Fatalf("want nil when weekly used unknown, got %v", got)
		}
	})
}

func TestCNProviderWindowUsedAtLeast(t *testing.T) {
	cases := []struct {
		name    string
		raw     any
		want    bool
	}{
		{"float64 exhausted", float64(100), true},
		{"float64 above", float64(101.5), true},
		{"float64 below", float64(99.9), false},
		{"int exhausted", 100, true},
		{"int64 exhausted", int64(100), true},
		{"string exhausted", "100", true},
		{"string garbage", "abc", false},
		{"nil", nil, false},
		{"missing", "absent", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			extra := map[string]any{}
			if tc.raw != "absent" {
				extra[cnExtraKey(PlatformZhipu, cnExtraSuffixWeeklyUsed)] = tc.raw
			}
			acct := zhipuCodingAccount(extra)
			if got := cnProviderWindowUsedAtLeast(acct, PlatformZhipu, cnExtraSuffixWeeklyUsed, 100); got != tc.want {
				t.Fatalf("cnProviderWindowUsedAtLeast(%v) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
