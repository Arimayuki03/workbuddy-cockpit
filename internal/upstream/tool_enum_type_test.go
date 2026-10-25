package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// toolEnumBody 构造一个只带 tools 的请求体，便于断言改写结果。
func toolEnumBody(t *testing.T, params map[string]any) []byte {
	t.Helper()
	body := map[string]any{
		"model":    "kimi-k3.1-pro",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":       "probe",
					"parameters": params,
				},
			},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// prepareAndProperty 跑一遍出站改写，取回 tools[0].function.parameters.properties[key]。
func prepareAndProperty(t *testing.T, params map[string]any, key string) map[string]any {
	t.Helper()
	out := PrepareBodyOpt(toolEnumBody(t, params), false)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal out: %v", err)
	}
	tools, _ := got["tools"].([]any)
	if len(tools) == 0 {
		t.Fatalf("tools 丢失: %s", out)
	}
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	props, _ := fn["parameters"].(map[string]any)["properties"].(map[string]any)
	prop, _ := props[key].(map[string]any)
	if prop == nil {
		t.Fatalf("属性 %q 丢失", key)
	}
	return prop
}

// enum 缺 type：按取值类型补 type，且不能改动 enum 本身。
func TestToolEnumWithoutTypeGetsInferredType(t *testing.T) {
	cases := []struct {
		name string
		enum []any
		want string
	}{
		{"strings", []any{"quick", "deep"}, "string"},
		{"integers", []any{float64(1), float64(2)}, "integer"},
		{"integers and float", []any{float64(1), float64(2.5)}, "number"},
		{"floats", []any{float64(0.5)}, "number"},
		{"booleans", []any{true, false}, "boolean"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prop := prepareAndProperty(t, map[string]any{
				"type":       "object",
				"properties": map[string]any{"x": map[string]any{"enum": c.enum}},
			}, "x")
			if got := prop["type"]; got != c.want {
				t.Errorf("type=%v want %s", got, c.want)
			}
			enum, ok := prop["enum"].([]any)
			if !ok || len(enum) != len(c.enum) {
				t.Errorf("enum 被改动: %v", prop["enum"])
			}
		})
	}
}

// 已有 type：不覆盖（显式声明优先，哪怕与取值形态不一致——schema 由客户端负责）。
func TestToolEnumExistingTypeNotOverwritten(t *testing.T) {
	prop := prepareAndProperty(t, map[string]any{
		"type":       "object",
		"properties": map[string]any{"x": map[string]any{"type": "string", "enum": []any{"a"}}},
	}, "x")
	if prop["type"] != "string" {
		t.Errorf("type 被覆盖: %v", prop["type"])
	}
}

// 混合标量/含 null：无法无损推断，不改写（交由上游原样报错）。
func TestToolEnumMixedScalarsNotInferred(t *testing.T) {
	prop := prepareAndProperty(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mix":  map[string]any{"enum": []any{"a", float64(1)}},
			"null": map[string]any{"enum": []any{"a", nil}},
		},
	}, "mix")
	if _, has := prop["type"]; has {
		t.Errorf("混合标量不应补 type: %v", prop)
	}
}

// 嵌套 schema（items / anyOf / $defs）里的 enum 同样要补。
func TestToolEnumNestedSchemaInferred(t *testing.T) {
	prop := prepareAndProperty(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"list": map[string]any{
				"type":  "array",
				"items": map[string]any{"enum": []any{"a", "b"}},
			},
			"any": map[string]any{
				"anyOf": []any{
					map[string]any{"enum": []any{float64(1)}},
					map[string]any{"type": "string"},
				},
			},
		},
	}, "list")
	items, _ := prop["items"].(map[string]any)
	if items == nil || items["type"] != "string" {
		t.Errorf("items.enum 未补 type: %v", prop["items"])
	}
}

// 旧式 functions[] 形态（parameters 直接挂在条目上）同样处理。
func TestToolEnumLegacyFunctionsForm(t *testing.T) {
	body := map[string]any{
		"model":    "kimi-k3.1-pro",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"functions": []any{
			map[string]any{
				"name": "probe",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"x": map[string]any{"enum": []any{"a"}}},
				},
			},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := PrepareBodyOpt(raw, false)
	if !strings.Contains(string(out), `"enum":["a"],"type":"string"`) {
		t.Errorf("functions[] 未补 type: %s", out)
	}
}
