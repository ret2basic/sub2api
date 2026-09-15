package handler

import (
	"context"
	"math"
	"net/http"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// 池容量查询（GET /v1/sub2api/pool-capacity）。
//
// 2026-09-15 定规：客户端并发上限动态跟随池子有效容量（账号窗口耗尽自动
// 缩、回池自动恢复），不再手动广播调并发。数据与口径直接复用管理台
// GroupCapacityService（可调度账号并发聚合 + Redis 实时占用），不另造
// 聚合；本端点只解决认证域——管理台走 admin JWT，客户端工具只持有池 key。

// poolCapacitySafetyFactor 是推荐并发的安全系数：available_slots × 0.8，
// 留出排队与突发裕量，避免客户端把池子每个槽都顶满。
const poolCapacitySafetyFactor = 0.8

// poolCapacityReader 抽象 GroupCapacityService 的单组查询，便于测试注入。
type poolCapacityReader interface {
	GetGroupCapacityByID(ctx context.Context, groupID int64) (service.GroupCapacitySummary, error)
}

type poolCapacityResponse struct {
	Platform                  string `json:"platform"`
	AvailableSlots            int    `json:"available_slots"`
	ConcurrencyUsed           int    `json:"concurrency_used"`
	RecommendedMaxConcurrency int    `json:"recommended_max_concurrency"`
}

// buildPoolCapacityRecommended 从可调度槽位数推导推荐总并发：
// 池空返回 0（客户端应暂停派发而非报错），否则 floor(slots×0.8) 且至少 1。
func buildPoolCapacityRecommendation(availableSlots int) int {
	if availableSlots <= 0 {
		return 0
	}
	recommended := int(math.Floor(float64(availableSlots) * poolCapacitySafetyFactor))
	if recommended < 1 {
		recommended = 1
	}
	return recommended
}

// PoolCapacity 返回认证 key 所属分组（池）的有效并发容量。
// GET /v1/sub2api/pool-capacity
func (h *GatewayHandler) PoolCapacity(c *gin.Context) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if apiKey.GroupID == nil {
		h.errorResponse(c, http.StatusForbidden, "permission_error", "API key is not assigned to a group")
		return
	}
	if h.groupCapacity == nil {
		h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "Pool capacity is unavailable")
		return
	}
	summary, err := h.groupCapacity.GetGroupCapacityByID(c.Request.Context(), *apiKey.GroupID)
	if err != nil {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "Pool capacity query failed")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, poolCapacityResponse{
		Platform:                  apiKey.Group.Platform,
		AvailableSlots:            summary.ConcurrencyMax,
		ConcurrencyUsed:           summary.ConcurrencyUsed,
		RecommendedMaxConcurrency: buildPoolCapacityRecommendation(summary.ConcurrencyMax),
	})
}
