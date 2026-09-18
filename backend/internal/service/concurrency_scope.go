package service

import "strings"

// ModelSlotScope 解析按模型并发覆盖（gateway.scheduling.model_concurrency）。
// 模型命中且值 >0 时返回 (上限, 模型作用域, true)；否则 ok=false。
// 返回的 scope 既是槽位作用域，也是 Redis 键后缀。
func ModelSlotScope(modelConcurrency map[string]int, model string) (int, string, bool) {
	model = strings.TrimSpace(model)
	if model == "" || len(modelConcurrency) == 0 {
		return 0, "", false
	}
	if v, ok := modelConcurrency[model]; ok && v > 0 {
		return v, model, true
	}
	return 0, "", false
}

// EffectiveAccountSlotCap 计算账号在给定模型下的槽位上限与作用域：
// 模型有覆盖 → (覆盖值, 模型作用域)；否则 → (账号级上限, 空作用域)。
// 空作用域表示沿用账号级单桶，行为与历史一致。
func EffectiveAccountSlotCap(modelConcurrency map[string]int, account *Account, model string) (int, string) {
	if account == nil {
		return 0, ""
	}
	if cap, scope, ok := ModelSlotScope(modelConcurrency, model); ok {
		return cap, scope
	}
	return account.Concurrency, ""
}
