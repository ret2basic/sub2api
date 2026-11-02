package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// 2026-09-15 池容量端点：数据复用 GroupCapacityService，本测试覆盖
// 认证/分组缺失错误路径、happy path 组装与 recommended 推导公式。

type stubPoolCapacityReader struct {
	summary service.GroupCapacitySummary
	err     error
}

func (s *stubPoolCapacityReader) GetGroupCapacityByID(ctx context.Context, groupID int64) (service.GroupCapacitySummary, error) {
	return s.summary, s.err
}

func requestPoolCapacityForTest(h *GatewayHandler, apiKey *service.APIKey) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/sub2api/pool-capacity", nil)
	if apiKey != nil {
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	}
	h.PoolCapacity(c)
	return rec
}

func TestPoolCapacityHandlerRejectsMissingKey(t *testing.T) {
	rec := requestPoolCapacityForTest(&GatewayHandler{}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestPoolCapacityHandlerRejectsGrouplessKey(t *testing.T) {
	h := &GatewayHandler{groupCapacity: &stubPoolCapacityReader{}}
	rec := requestPoolCapacityForTest(h, &service.APIKey{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (group missing)", rec.Code)
	}
}

func TestPoolCapacityHandlerRejectsMissingService(t *testing.T) {
	groupID := int64(8)
	h := &GatewayHandler{}
	rec := requestPoolCapacityForTest(h, &service.APIKey{GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformKimi}})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (capacity service unavailable)", rec.Code)
	}
}

func TestPoolCapacityHandlerHappyPath(t *testing.T) {
	groupID := int64(9)
	h := &GatewayHandler{groupCapacity: &stubPoolCapacityReader{
		summary: service.GroupCapacitySummary{
			ConcurrencyUsed: 7,
			ConcurrencyMax:  24,
		},
	}}
	rec := requestPoolCapacityForTest(h, &service.APIKey{GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformZhipu}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Platform                  string `json:"platform"`
		AvailableSlots            int    `json:"available_slots"`
		ConcurrencyUsed           int    `json:"concurrency_used"`
		RecommendedMaxConcurrency int    `json:"recommended_max_concurrency"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Platform != service.PlatformZhipu || body.AvailableSlots != 24 || body.ConcurrencyUsed != 7 {
		t.Fatalf("summary passthrough mismatch: %+v", body)
	}
	if body.RecommendedMaxConcurrency != 19 { // floor(24×0.8)
		t.Fatalf("recommended = %d, want 19", body.RecommendedMaxConcurrency)
	}
}

func TestBuildPoolCapacityRecommendation(t *testing.T) {
	cases := []struct {
		slots int
		want  int
	}{
		{0, 0},   // 池空：客户端暂停派发
		{-3, 0},  // 异常输入防御
		{1, 1},   // 单槽下限保护（floor(0.8)=0 → 1）
		{5, 4},   // floor(4.0)
		{24, 19}, // 2026-09-15 GLM 池实况：4 号存活
		{48, 38}, // 满血 8 号
	}
	for _, tc := range cases {
		if got := buildPoolCapacityRecommendation(tc.slots); got != tc.want {
			t.Fatalf("buildPoolCapacityRecommendation(%d) = %d, want %d", tc.slots, got, tc.want)
		}
	}
}

// 2026-10-15 quota 字段：读得到就带上，读不到（或容量服务不支持）就是 null，
// 容量部分永远不受影响。

type stubPoolCapacityQuotaReader struct {
	stubPoolCapacityReader
	quota       *service.PoolQuotaSummary
	quotaErr    error
	gotGroupID  int64
	gotPlatform string
}

func (s *stubPoolCapacityQuotaReader) GetGroupQuotaByID(ctx context.Context, groupID int64, platform string) (*service.PoolQuotaSummary, error) {
	s.gotGroupID, s.gotPlatform = groupID, platform
	return s.quota, s.quotaErr
}

func decodePoolCapacityQuota(t *testing.T, rec *httptest.ResponseRecorder) (map[string]any, bool) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	raw, present := body["quota"]
	if !present {
		t.Fatalf("quota key missing: %s", rec.Body.String())
	}
	quota, _ := raw.(map[string]any)
	return quota, raw != nil
}

func TestPoolCapacityHandlerIncludesQuota(t *testing.T) {
	groupID := int64(12)
	reader := &stubPoolCapacityQuotaReader{
		stubPoolCapacityReader: stubPoolCapacityReader{summary: service.GroupCapacitySummary{ConcurrencyMax: 2}},
		quota: &service.PoolQuotaSummary{BindingWindow: "weekly", UsedFraction: 0.3, ElapsedFraction: 0.5, Pace: 0.6, PaceKnown: true,
			WindowSeconds: 604800, AccountsReporting: 1},
	}
	h := &GatewayHandler{groupCapacity: reader}
	rec := requestPoolCapacityForTest(h, &service.APIKey{GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformGrok}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	quota, ok := decodePoolCapacityQuota(t, rec)
	if !ok || quota["binding_window"] != "weekly" || quota["pace"] != 0.6 || quota["pace_known"] != true {
		t.Fatalf("quota = %v", quota)
	}
	if reader.gotGroupID != groupID || reader.gotPlatform != service.PlatformGrok {
		t.Fatalf("quota lookup used group %d platform %q", reader.gotGroupID, reader.gotPlatform)
	}
}

func TestPoolCapacityHandlerQuotaNullWhenUnavailable(t *testing.T) {
	groupID := int64(9)
	apiKey := &service.APIKey{GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformZhipu}}
	// 不支持 quota 的容量服务（老实现 / 测试桩）。
	rec := requestPoolCapacityForTest(&GatewayHandler{groupCapacity: &stubPoolCapacityReader{summary: service.GroupCapacitySummary{ConcurrencyMax: 6}}}, apiKey)
	if _, ok := decodePoolCapacityQuota(t, rec); ok {
		t.Fatalf("quota should be null without a quota reader: %s", rec.Body.String())
	}
	// 读额度出错：容量照常 200，quota 为 null。
	failing := &stubPoolCapacityQuotaReader{
		stubPoolCapacityReader: stubPoolCapacityReader{summary: service.GroupCapacitySummary{ConcurrencyMax: 6}},
		quotaErr:               context.DeadlineExceeded,
	}
	rec = requestPoolCapacityForTest(&GatewayHandler{groupCapacity: failing}, apiKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when quota fails", rec.Code)
	}
	if _, ok := decodePoolCapacityQuota(t, rec); ok {
		t.Fatalf("quota should be null when the lookup fails: %s", rec.Body.String())
	}
}

// 2026-11-02：分桶在途数要随响应带出（concurrency_used_by_model），否则工具看不到 flash 桶。
func TestPoolCapacityHandlerReportsModelBuckets(t *testing.T) {
	groupID := int64(9)
	h := &GatewayHandler{groupCapacity: &stubPoolCapacityReader{
		summary: service.GroupCapacitySummary{
			ConcurrencyUsed:        6,
			ConcurrencyMax:         60,
			ConcurrencyUsedByModel: map[string]int{"glm-5.3": 5, "glm-5.3-flash": 7},
		},
	}}
	rec := requestPoolCapacityForTest(h, &service.APIKey{GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformZhipu}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"concurrency_used":6`, `"recommended_max_concurrency":48`, `"concurrency_used_by_model":{"glm-5.3":5,"glm-5.3-flash":7}`} {
		if !strings.Contains(body, want) {
			t.Fatalf("响应缺少 %s：%s", want, body)
		}
	}
}
