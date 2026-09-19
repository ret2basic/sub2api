package service

import (
	"context"
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

func kimiCodingAccount(extra map[string]any) *Account {
	return &Account{
		ID:     16,
		Name:   "kimi-test",
		Status: "active",
		Credentials: map[string]any{
			"api_key":      "test-key",
			"account_mode": AccountModeCoding,
		},
		Extra:    extra,
		Platform: PlatformKimi,
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

// 2026-09-15 kimi 卡会话事故回归：usage-limit 403 必须冷却到快照窗口重置点。
func TestCNProviderResponseIndicatesUsageWindowLimit(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		body []byte
		want bool
	}{
		{
			name: "5-hour usage limit",
			msg:  "You've reached your 5-hour usage limit. Your quota will reset when the current 5-hour window ends.",
			want: true,
		},
		{
			name: "weekly usage limit",
			msg:  "You've reached your weekly (7-day) usage limit. Your quota will reset when the current 7-day window ends.",
			want: true,
		},
		{
			name: "concurrency limit is not a window limit",
			msg:  kimiConcurrentRequestLimitMessage,
			want: false,
		},
		{
			name: "body fallback when message empty",
			msg:  "",
			body: []byte(`{"error":{"message":"You've reached your 5-hour usage limit"}}`),
			want: true,
		},
		{
			name: "empty everything",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cnProviderResponseIndicatesUsageWindowLimit(tc.msg, tc.body); got != tc.want {
				t.Fatalf("cnProviderResponseIndicatesUsageWindowLimit(%q, %s) = %v, want %v", tc.msg, tc.body, got, tc.want)
			}
		})
	}
}

// 5h 窗口耗尽（reset 在未来）必须返回 5h 重置点，而不是 weekly 或 nil——
// 修复前 kimi 走 OpenAI 10 分钟阶梯，到期放回调度后粘性会话反复砸同一账号。
func TestCNProviderQuotaSnapshotResetKimi5hWindow(t *testing.T) {
	now := time.Date(2026, 9, 15, 4, 20, 0, 0, time.UTC)
	reset5h := now.Add(8 * time.Hour)
	weekly := now.Add(6 * 24 * time.Hour)
	acct := kimiCodingAccount(map[string]any{
		cnExtraKey(PlatformKimi, cnExtraSuffix5hReset):     reset5h.Format(time.RFC3339),
		cnExtraKey(PlatformKimi, cnExtraSuffix5hUsed):      100,
		cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyReset): weekly.Format(time.RFC3339),
		cnExtraKey(PlatformKimi, cnExtraSuffixWeeklyUsed):  31,
	})
	got := cnProviderQuotaSnapshotReset(acct, now)
	if got == nil || !got.Equal(reset5h) {
		t.Fatalf("want 5h reset %v, got %v", reset5h, got)
	}
}

// 2026-09-19 GLM 池打毒事故回归：智谱 1311「当前订阅套餐暂未开放 X 权限」是
// 模型权限错误，不是窗口耗尽——识别后必须完全不冷却账号。事故形态是一条
// model=glm-5.3-flashX 的请求经 10 次账号故障转移把整池 10 个号全部冷却。
func TestZhipuModelEntitlementError(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "1311 事故原文",
			body: `{"error":{"code":"1311","message":"当前订阅套餐暂未开放GLM-5.3-FlashX权限"}}`,
			want: true,
		},
		{
			name: "1311 数字码",
			body: `{"error":{"code":1311,"message":"model not included in plan"}}`,
			want: true,
		},
		{
			name: "仅文案（错误码形态变化兜底）",
			body: `{"error":{"message":"当前订阅套餐暂未开放GLM-5.3-FlashX权限"}}`,
			want: true,
		},
		{
			name: "1113 并发超上限不算",
			body: `{"error":{"code":"1113","message":"您当前的使用量或并发量已达到上限"}}`,
			want: false,
		},
		{
			name: "1302 频率超上限不算",
			body: `{"error":{"code":"1302","message":"请求频率超上限"}}`,
			want: false,
		},
		{
			name: "窗口耗尽不算",
			body: `{"error":{"code":"1301","message":"您当前的使用量已达到上限"}}`,
			want: false,
		},
		{
			name: "余额不足不算",
			body: `{"error":{"code":"1113","message":"余额不足，请充值"}}`,
			want: false,
		},
		{
			name: "无关字段里的 1311 不算",
			body: `{"usage":{"total_tokens":13110}}`,
			want: false,
		},
		{name: "空体", body: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body []byte
			if tc.body != "" {
				body = []byte(tc.body)
			}
			if got := zhipuModelEntitlementError(body); got != tc.want {
				t.Fatalf("zhipuModelEntitlementError(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// zhipuRateLimitStubRepo 只记录 SetRateLimited 调用，用于断言"冷却/不冷却"。
type zhipuRateLimitStubRepo struct {
	AccountRepository
	rateLimitedIDs []int64
}

func (s *zhipuRateLimitStubRepo) SetRateLimited(_ context.Context, id int64, _ time.Time) error {
	s.rateLimitedIDs = append(s.rateLimitedIDs, id)
	return nil
}

func TestApplyCNProviderReactive429ZhipuEntitlementNoCooldown(t *testing.T) {
	now := time.Now()
	// 事故当时的快照形态：5h 窗口标记为耗尽且重置点在未来——旧逻辑会把它当作
	// 冷却终点（这正是整池被打毒的方式）。
	extra := map[string]any{
		cnExtraKey(PlatformZhipu, cnExtraSuffix5hReset): now.Add(2 * time.Hour).Format(time.RFC3339),
		cnExtraKey(PlatformZhipu, cnExtraSuffix5hUsed):  100,
	}

	t.Run("1311 模型无权限：不冷却账号", func(t *testing.T) {
		repo := &zhipuRateLimitStubRepo{}
		svc := &RateLimitService{accountRepo: repo}
		handled := svc.applyCNProviderReactive429(
			context.Background(),
			zhipuCodingAccount(extra),
			nil,
			[]byte(`{"error":{"code":"1311","message":"当前订阅套餐暂未开放GLM-5.3-FlashX权限"}}`),
		)
		if !handled {
			t.Fatal("应当识别为已处理（不让 429 继续落通用冷却逻辑）")
		}
		if len(repo.rateLimitedIDs) != 0 {
			t.Fatalf("模型无权限错误不得冷却账号，实际冷却了 %v", repo.rateLimitedIDs)
		}
	})

	t.Run("对照：窗口耗尽 429 仍照旧冷却", func(t *testing.T) {
		repo := &zhipuRateLimitStubRepo{}
		svc := &RateLimitService{accountRepo: repo}
		handled := svc.applyCNProviderReactive429(
			context.Background(),
			zhipuCodingAccount(extra),
			nil,
			[]byte(`{"error":{"code":"1301","message":"您当前的使用量已达到上限"}}`),
		)
		if !handled {
			t.Fatal("窗口耗尽 429 应被处理")
		}
		if len(repo.rateLimitedIDs) != 1 {
			t.Fatalf("窗口耗尽 429 应冷却账号一次，实际 %v", repo.rateLimitedIDs)
		}
	})
}
