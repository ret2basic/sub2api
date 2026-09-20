package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 2026-09-19 事故回归:glm-5.3-flashx(套餐外模型)被 zhipu 以 HTTP 429 + code 1311
// 拒绝,网关把它伪装成 "Upstream rate limit exceeded" 还循环走完整个账号池。
// 这组测试钉住两件事:1311 立即耗尽不换号;客户端收到 403 model_not_entitled
// 与上游真实文案。

const zhipu1311Body = `{"error":{"code":"1311","message":"当前订阅套餐暂未开放GLM-5.3-FlashX权限"}}`

func TestFailoverZhipu1311ExhaustsImmediatelyWithoutSwitch(t *testing.T) {
	state := NewFailoverState(10, false)
	before := state.SwitchCount

	action := state.HandleFailoverError(
		context.Background(),
		noopTempUnscheduler{},
		42,
		service.PlatformZhipu,
		3,
		&service.UpstreamFailoverError{
			StatusCode:   http.StatusTooManyRequests,
			ResponseBody: []byte(zhipu1311Body),
		},
	)

	require.Equal(t, FailoverExhausted, action)
	require.Equal(t, before, state.SwitchCount, "entitlement error must not switch accounts")
	require.Len(t, state.FailedAccountIDs, 0, "entitlement is not an account failure")
}

func TestFailoverNonEntitled429StillSwitches(t *testing.T) {
	state := NewFailoverState(10, false)
	action := state.HandleFailoverError(
		context.Background(),
		noopTempUnscheduler{},
		42,
		service.PlatformZhipu,
		3,
		&service.UpstreamFailoverError{
			StatusCode:   http.StatusTooManyRequests,
			ResponseBody: []byte(`{"error":{"code":"1302","message":"request frequency too high"}}`),
		},
	)
	require.Equal(t, FailoverContinue, action)
	require.Equal(t, 1, state.SwitchCount)
}

func TestOpenAIExhaustedZhipu1311Surfaces403NotEntitled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode:   http.StatusTooManyRequests,
		ResponseBody: []byte(zhipu1311Body),
	}, false)

	require.Equal(t, http.StatusForbidden, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, "model_not_entitled")
	require.Contains(t, body, "当前订阅套餐暂未开放")
	require.NotContains(t, body, "rate limit", "1311 must not be masked as a rate limit")
}

func TestGatewayExhaustedZhipu1311Surfaces403NotEntitled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	(&GatewayHandler{}).handleFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode:   http.StatusTooManyRequests,
		ResponseBody: []byte(zhipu1311Body),
	}, service.PlatformZhipu, false)

	require.Equal(t, http.StatusForbidden, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, "model_not_entitled")
	require.Contains(t, body, "当前订阅套餐暂未开放")
}

type noopTempUnscheduler struct{}

func (noopTempUnscheduler) TempUnscheduleRetryableError(ctx context.Context, accountID int64, err *service.UpstreamFailoverError) {}
