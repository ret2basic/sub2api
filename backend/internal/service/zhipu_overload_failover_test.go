package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestZhipuModelOverloadStopsAccountFailover(t *testing.T) {
	service := &OpenAIGatewayService{}
	account := &Account{Platform: PlatformZhipu}
	body := []byte(`{"error":{"code":"1305","message":"model overloaded"}}`)

	failure := service.newOpenAIAccountFailoverError(account, 529, nil, body, "", false, true)
	require.False(t, failure.ShouldRetryNextAccount())
	require.False(t, failure.RetryableOnSameAccount)
	require.Equal(t, "30", failure.ResponseHeaders.Get("Retry-After"))

	headers := http.Header{"Retry-After": {"45"}}
	numericCode := []byte(`{"error":{"code":1305}}`)
	failure = service.newOpenAIAccountFailoverError(account, 529, headers, numericCode, "", false, true)
	require.False(t, failure.ShouldRetryNextAccount())
	require.Equal(t, "45", failure.ResponseHeaders.Get("Retry-After"))

	failure = service.newOpenAIAccountFailoverError(account, 529, nil, []byte(`{"error":{"code":"1302"}}`), "", false, true)
	require.True(t, failure.ShouldRetryNextAccount())
	require.True(t, failure.RetryableOnSameAccount)

	failure = service.newOpenAIAccountFailoverError(&Account{Platform: PlatformOpenAI}, 529, nil, body, "", false, true)
	require.True(t, failure.ShouldRetryNextAccount())
	remapped := service.newOpenAIAccountFailoverError(account, http.StatusTooManyRequests, nil, body, "", false, true)
	require.True(t, remapped.ShouldRetryNextAccount())
}

// 2026-11-28：智谱 1302（这个号请求频率超限）不换号重发，回 429 加 Retry-After。
func TestZhipuRateLimitStopsAccountFailover(t *testing.T) {
	service := &OpenAIGatewayService{}
	account := &Account{Platform: PlatformZhipu}
	body := []byte(`{"error":{"code":"1302","message":"您的账户已达到速率限制，请您控制请求频率"}}`)

	failure := service.newOpenAIAccountFailoverError(account, http.StatusTooManyRequests, nil, body, "", false, true)
	require.False(t, failure.ShouldRetryNextAccount())
	require.False(t, failure.RetryableOnSameAccount)
	require.Equal(t, "30", failure.ResponseHeaders.Get("Retry-After"))

	headers := http.Header{"Retry-After": {"12"}}
	failure = service.newOpenAIAccountFailoverError(account, http.StatusTooManyRequests, headers, []byte(`{"error":{"code":1302}}`), "", false, true)
	require.False(t, failure.ShouldRetryNextAccount())
	require.Equal(t, "12", failure.ResponseHeaders.Get("Retry-After"))

	// 其他平台、其他 429 不受影响。
	failure = service.newOpenAIAccountFailoverError(&Account{Platform: PlatformGrok}, http.StatusTooManyRequests, nil, body, "", false, true)
	require.True(t, failure.ShouldRetryNextAccount())
	failure = service.newOpenAIAccountFailoverError(account, http.StatusTooManyRequests, nil, []byte(`{"error":{"code":"1308","message":"5h quota"}}`), "", false, true)
	require.True(t, failure.ShouldRetryNextAccount())
}
