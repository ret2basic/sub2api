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
