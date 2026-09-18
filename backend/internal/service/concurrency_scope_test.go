package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestModelSlotScope(t *testing.T) {
	overrides := map[string]int{"glm-5.3": 6, "glm-5.3-flash": 50}

	slotCap, scope, ok := ModelSlotScope(overrides, "glm-5.3-flash")
	require.True(t, ok)
	require.Equal(t, 50, slotCap)
	require.Equal(t, "glm-5.3-flash", scope)

	_, _, ok = ModelSlotScope(overrides, "  glm-5.3  ")
	require.True(t, ok, "模型名两端空白应被忽略")

	_, _, ok = ModelSlotScope(overrides, "kimi-k3")
	require.False(t, ok)
	_, _, ok = ModelSlotScope(overrides, "")
	require.False(t, ok, "空模型名不得命中覆盖")

	_, _, ok = ModelSlotScope(map[string]int{"glm-5.3": 0}, "glm-5.3")
	require.False(t, ok, "<=0 的配置值应视为未配置")

	_, _, ok = ModelSlotScope(nil, "glm-5.3")
	require.False(t, ok)
}

func TestEffectiveAccountSlotCap(t *testing.T) {
	account := &Account{ID: 22, Concurrency: 6}
	overrides := map[string]int{"glm-5.3-flash": 50}

	slotCap, scope := EffectiveAccountSlotCap(overrides, account, "glm-5.3-flash")
	require.Equal(t, 50, slotCap)
	require.Equal(t, "glm-5.3-flash", scope, "分桶模型应返回非空作用域")

	slotCap, scope = EffectiveAccountSlotCap(overrides, account, "glm-5.3")
	require.Equal(t, 6, slotCap)
	require.Equal(t, "", scope, "未列出的模型沿用账号级单桶")

	slotCap, scope = EffectiveAccountSlotCap(overrides, nil, "glm-5.3-flash")
	require.Equal(t, 0, slotCap)
	require.Equal(t, "", scope)
}

func TestOpenAIRequestSlotScopeAndModelSlotLimit(t *testing.T) {
	cfg := config.GatewaySchedulingConfig{ModelConcurrency: map[string]int{"glm-5.3": 6, "glm-5.3-flash": 50}}
	account := &Account{ID: 1, Concurrency: 6}

	require.Equal(t, "glm-5.3-flash", openAIRequestSlotScope(cfg, "glm-5.3-flash"))
	require.Equal(t, "glm-5.3", openAIRequestSlotScope(cfg, "glm-5.3"))
	require.Equal(t, "", openAIRequestSlotScope(cfg, "glm-4.6"))

	slotCap, slotScope := openAIModelSlotLimit(cfg, account, "glm-5.3-flash")
	require.Equal(t, 50, slotCap)
	require.Equal(t, "glm-5.3-flash", slotScope)

	slotCap, slotScope = openAIModelSlotLimit(cfg, account, "glm-5.3")
	require.Equal(t, 6, slotCap)
	require.Equal(t, "glm-5.3", slotScope)

	slotCap, slotScope = openAIModelSlotLimit(cfg, account, "glm-4.6")
	require.Equal(t, account.Concurrency, slotCap)
	require.Equal(t, "", slotScope)

	slotCap, slotScope = openAIModelSlotLimit(cfg, nil, "glm-5.3-flash")
	require.Equal(t, 0, slotCap)
	require.Equal(t, "", slotScope)
}
