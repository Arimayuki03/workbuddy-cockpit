package upstream

import (
	"encoding/json"
	"reflect"
	"testing"
)

// normalizeUsageCacheAliases 专项测试（usage.go，吸收 panel PR #57 同源修复）。
//
// 背景：部分上游在 prompt_tokens_details.cached_tokens 返回真实命中量，同时把
// cache_read_input_tokens / cached_tokens 别名留 0。网关出站前取全部别名的最大
// 正值回写所有键，保证任意口径的下游读到一致的命中量。

func TestNormalizeUsageDetailsTrueFlatZero(t *testing.T) {
	// 核心缺陷形态：details 嵌套有真值、扁平别名全 0 → 全部别名对齐真值。
	in := map[string]any{
		"prompt_tokens":           100.0,
		"prompt_tokens_details":   map[string]any{"cached_tokens": 80.0},
		"cache_read_input_tokens": 0.0,
		"cached_tokens":           0.0,
	}
	got := normalizeUsageCacheAliases(in)
	if got["cache_read_input_tokens"] != 80.0 || got["cached_tokens"] != 80.0 || got["prompt_cache_hit_tokens"] != 80.0 {
		t.Errorf("扁平别名应对齐真值 80: %+v", got)
	}
	details, _ := got["prompt_tokens_details"].(map[string]any)
	if details == nil || details["cached_tokens"] != 80.0 {
		t.Errorf("prompt_tokens_details.cached_tokens 应为 80: %+v", got["prompt_tokens_details"])
	}
	// 原 map 不得被修改（ensureUsageTotal 同纪律：出站改写走克隆）。
	if in["cache_read_input_tokens"] != 0.0 {
		t.Errorf("原 map 被改写: %+v", in)
	}
}

func TestNormalizeUsageFlatTrueNoDetails(t *testing.T) {
	// Anthropic 口径上游：只有扁平 cache_read_input_tokens 有真值 → 补齐其余别名
	// 与 details 对象（prompt 侧补造、input 侧不造）。
	in := map[string]any{
		"prompt_tokens":           100.0,
		"cache_read_input_tokens": 64.0,
	}
	got := normalizeUsageCacheAliases(in)
	for _, k := range []string{"cached_tokens", "prompt_cache_hit_tokens"} {
		if got[k] != 64.0 {
			t.Errorf("%s=%v want 64", k, got[k])
		}
	}
	details, ok := got["prompt_tokens_details"].(map[string]any)
	if !ok || details["cached_tokens"] != 64.0 {
		t.Errorf("prompt_tokens_details 应补造并对齐 64: %+v", got["prompt_tokens_details"])
	}
	if _, exists := got["input_tokens_details"]; exists {
		t.Error("上游未提供 input_tokens_details 时不得凭空造（Chat-only 客户端不需要）")
	}
}

func TestNormalizeUsageInputDetailsPreserved(t *testing.T) {
	// Responses API 消费形态：上游已给 input_tokens_details → 对齐真值但保留其他键。
	in := map[string]any{
		"prompt_tokens":         100.0,
		"prompt_tokens_details": map[string]any{"cached_tokens": 0.0, "audio_tokens": 7.0},
		"input_tokens_details":  map[string]any{"cached_tokens": 0.0, "image_tokens": 3.0},
		"cached_tokens":         50.0,
	}
	got := normalizeUsageCacheAliases(in)
	inputDetails, ok := got["input_tokens_details"].(map[string]any)
	if !ok || inputDetails["cached_tokens"] != 50.0 || inputDetails["image_tokens"] != 3.0 {
		t.Errorf("input_tokens_details 应对齐 50 且保留 image_tokens: %+v", got["input_tokens_details"])
	}
	promptDetails, _ := got["prompt_tokens_details"].(map[string]any)
	if promptDetails["audio_tokens"] != 7.0 {
		t.Errorf("prompt details 其他键应保留: %+v", promptDetails)
	}
}

func TestNormalizeUsageAllZeroNoHit(t *testing.T) {
	// 全别名 0（确实未命中）→ 零改动：补 0 无意义，缺失别名也不补（不污染下游判断）。
	in := map[string]any{
		"prompt_tokens":           100.0,
		"cache_read_input_tokens": 0.0,
		"cached_tokens":           0.0,
	}
	got := normalizeUsageCacheAliases(in)
	if !reflect.DeepEqual(got, in) {
		t.Errorf("全零未命中应原样返回: got %+v", got)
	}
	if _, exists := got["prompt_tokens_details"]; exists {
		t.Error("未命中时不得补造 details 对象")
	}
}

func TestNormalizeUsageEmptyAndIllegal(t *testing.T) {
	// 空表 / 别名非法值（string）/ details 形态非法 → 零改动不 panic。
	empty := map[string]any{}
	if got := normalizeUsageCacheAliases(empty); len(got) != 0 {
		t.Errorf("空 usage 应原样返回: %+v", got)
	}
	illegal := map[string]any{
		"cached_tokens":         "many",
		"prompt_tokens_details": "not-an-object",
	}
	if !reflect.DeepEqual(normalizeUsageCacheAliases(illegal), illegal) {
		t.Errorf("非法值形态应原样返回: %+v", illegal)
	}
}

func TestNormalizeUsageMarshaledShape(t *testing.T) {
	// 端到端形态锁定：经 JSON 往返（真实出站路径）后别名全对齐。
	in := map[string]any{
		"prompt_tokens":           100.0,
		"prompt_tokens_details":   map[string]any{"cached_tokens": 80.0},
		"cache_read_input_tokens": 0.0,
	}
	got := normalizeUsageCacheAliases(in)
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"cache_read_input_tokens", "cached_tokens", "prompt_cache_hit_tokens"} {
		if out[k] != 80.0 {
			t.Errorf("%s=%v want 80（JSON 往返后）", k, out[k])
		}
	}
}
