// usage.go 缓存命中 usage 别名归一（吸收 panel PR #57 同源修复，2026-09）。
//
// 背景：部分上游在 prompt_tokens_details.cached_tokens 返回真实缓存命中量，同时把
// cache_read_input_tokens / cached_tokens 兼容别名留为 0。严格按 Anthropic/DeepSeek
// 口径取别名的下游会读到 0 而误判「缓存未命中」（计费/统计/缓存率全面失真）。
//
// 归一策略（出站最后一跳，对本项目两处消费点兜底）：
// 取全部别名中的最大正值为真值，回写所有别名键，使任意口径的下游读到一致的命中量。
// 不做跨字段合成（无任何正别名时零改动——不臆造数据）。
package upstream

// usageCacheAliasPaths 缓存命中量的别名路径全集（按优先级；最大正值取胜，顺序仅影响
// 并列时的可读性）。顶层三键为 Anthropic / DeepSeek / OpenAI 各自的扁平命名，
// 两个 details 嵌套对象为 OpenAI Responses / Chat Completions 的结构化形态。
var usageCacheAliasPaths = []struct{ section, key string }{
	{"prompt_tokens_details", "cached_tokens"},
	{"", "prompt_cache_hit_tokens"},
	{"", "cache_read_input_tokens"},
	{"", "cached_tokens"},
	{"input_tokens_details", "cached_tokens"},
}

// normalizeUsageCacheAliases 返回别名一致的 usage map。best <= 0（未命中/无别名/全部
// 非法值）时原样返回（不克隆、不写 0——别名缺失是合法形态，补 0 反而污染下游判断）。
// 有真值时通过克隆合并，绝不修改调用方持有的原 map（与 ensureUsageTotal 同纪律）。
func normalizeUsageCacheAliases(usage map[string]any) map[string]any {
	best := 0.0
	for _, p := range usageCacheAliasPaths {
		var v any
		if p.section == "" {
			v = usage[p.key]
		} else if details, ok := usage[p.section].(map[string]any); ok {
			v = details[p.key]
		}
		if n, ok := v.(float64); ok && n > best {
			best = n
		}
	}
	if best <= 0 {
		return usage
	}
	out := make(map[string]any, len(usage)+2)
	for k, v := range usage {
		out[k] = v
	}
	// 顶层别名回写：三个扁平键全部对齐真值（下游读哪个都一致）。
	out["cache_read_input_tokens"] = best
	out["cached_tokens"] = best
	out["prompt_cache_hit_tokens"] = best
	// 嵌套形态：prompt 侧回写真值；input 侧（Responses API 消费形态）仅在上游
	// 已提供时回写——不为 Chat-only 客户端凭空造对象。
	promptDetails := usageDetailsCopy(out, "prompt_tokens_details")
	if promptDetails == nil {
		promptDetails = map[string]any{}
	}
	promptDetails["cached_tokens"] = best
	out["prompt_tokens_details"] = promptDetails
	if _, exists := out["input_tokens_details"]; exists {
		inputDetails := usageDetailsCopy(out, "input_tokens_details")
		inputDetails["cached_tokens"] = best
		out["input_tokens_details"] = inputDetails
	}
	return out
}

// usageDetailsCopy 取 usage[section] 的浅拷贝；缺失或形态非法返回 nil（调用方自行兜底）。
// 浅拷贝足够：归一只覆盖 cached_tokens 一个标量键，不深入二级嵌套。
func usageDetailsCopy(usage map[string]any, section string) map[string]any {
	raw, ok := usage[section].(map[string]any)
	if !ok {
		return nil
	}
	cp := make(map[string]any, len(raw)+1)
	for k, v := range raw {
		cp[k] = v
	}
	return cp
}
