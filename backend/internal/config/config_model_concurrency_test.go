package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLoadModelConcurrencyDottedModelNames 锁定带点模型名的按模型分桶配置。
//
// 回归背景：viper 的 AllSettings 以 "." 为层级分隔符把扁平键重建为嵌套 map，
// 模型名 glm-5.3 被拆成 glm-5 → 3，map[string]int 解码随 Go map 迭代序在成败
// 之间抖动（2026-09-18 生产事故：同一配置文件 12 次加载 7 次失败，sub2api 在
// systemd 里反复崩溃循环）。修复后改为直接读配置文件原文，必须次次成功，
// 因此这里循环多次以守住确定性。
func TestLoadModelConcurrencyDottedModelNames(t *testing.T) {
	resetViperWithJWTSecret(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
gateway:
  scheduling:
    model_concurrency:
      glm-5.3: 6
      glm-5.3-flash: 50
`), 0o600))
	t.Setenv("CONFIG_FILE", path)

	want := map[string]int{"glm-5.3": 6, "glm-5.3-flash": 50}
	for i := 0; i < 30; i++ {
		cfg, err := Load()
		require.NoErrorf(t, err, "第 %d 次加载失败", i+1)
		require.Equalf(t, want, cfg.Gateway.Scheduling.ModelConcurrency, "第 %d 次解析结果不一致", i+1)
	}
}

// TestLoadModelConcurrencyAbsent 未配置该键时保持为空（账号级单桶行为不变）。
func TestLoadModelConcurrencyAbsent(t *testing.T) {
	resetViperWithJWTSecret(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("timezone: UTC\n"), 0o600))
	t.Setenv("CONFIG_FILE", path)

	cfg, err := Load()
	require.NoError(t, err)
	require.Empty(t, cfg.Gateway.Scheduling.ModelConcurrency)
}
