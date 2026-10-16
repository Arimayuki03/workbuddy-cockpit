package upstream

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

// TestNormalizeRoles 验证出站请求体把 developer 角色归一为 system。
// 上游 role 白名单不含 developer（OpenAI 新规范的 system 别名），
// 命中即 HTTP 400 code=11128；此处走 PrepareBodyOptWithEfforts 全链路断言。
func TestNormalizeRoles(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantRoles []string // 与输出 messages 逐条对应的期望 role；len 即消息数
	}{
		{"developer 改写为 system",
			`{"messages":[{"role":"developer","content":"x"}]}`, []string{"system"}},
		{"Developer 首字母大写改写",
			`{"messages":[{"role":"Developer","content":"x"}]}`, []string{"system"}},
		{"DEVELOPER 全大写改写",
			`{"messages":[{"role":"DEVELOPER","content":"x"}]}`, []string{"system"}},
		{"前后空白 TrimSpace 后改写",
			`{"messages":[{"role":" developer ","content":"x"}]}`, []string{"system"}},
		{"system 原样保留",
			`{"messages":[{"role":"system","content":"x"}]}`, []string{"system"}},
		{"user 原样保留",
			`{"messages":[{"role":"user","content":"x"}]}`, []string{"user"}},
		{"assistant 原样保留",
			`{"messages":[{"role":"assistant","content":"x"}]}`, []string{"assistant"}},
		{"tool 原样保留（不因未知而改写）",
			`{"messages":[{"role":"tool","content":"x"}]}`, []string{"tool"}},
		{"messages 缺失不 panic 且其余字段不变",
			`{"model":"glm-5.2"}`, []string{}},
		{"messages 为空数组不 panic",
			`{"messages":[]}`, []string{}},
		{"混合消息仅 developer 被改写",
			`{"messages":[{"role":"developer","content":"a"},{"role":"user","content":"b"},{"role":"developer","content":"c"}]}`,
			[]string{"system", "user", "system"}},
		{"sanitize=false 时仍归一（与脱敏解耦）",
			`{"messages":[{"role":"developer","content":"x"}]}`, []string{"system"}},
		{"非对象消息元素跳过、其余正常处理",
			`{"messages":["str",{"role":"developer","content":"x"},42]}`, []string{"system"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 全程 sanitize=false：验证 role 归一与内容脱敏开关无关（D4）。
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("unmarshal: %v (out=%s)", err, out)
			}

			// 提取输出 messages 里的 role（非对象元素跳过，不 panic）。
			var got []string
			if msgs, ok := obj["messages"].([]any); ok {
				for _, m := range msgs {
					msg, ok := m.(map[string]any)
					if !ok {
						continue
					}
					if role, ok := msg["role"].(string); ok {
						got = append(got, role)
					}
				}
			}

			if len(got) != len(c.wantRoles) {
				t.Fatalf("role 数量不符: got %v (%d) want %v (%d)", got, len(got), c.wantRoles, len(c.wantRoles))
			}
			for i := range got {
				if got[i] != c.wantRoles[i] {
					t.Errorf("role[%d] = %q want %q", i, got[i], c.wantRoles[i])
				}
			}
		})
	}

	// messages 缺失时，其余字段必须原样保留（除强制 stream）。
	t.Run("messages 缺失时其余字段不变", func(t *testing.T) {
		out := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.2","temperature":0.7}`), false, nil)
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if obj["model"] != "glm-5.2" || obj["temperature"] != 0.7 {
			t.Errorf("其余字段被改动: %v", obj)
		}
	})
}

// TestPrepareBodyStreamOptions body 未显式带 stream_options 时注入
// {include_usage: true}（D7，官方 CLI 流式必发）；body 已带则不覆盖。
func TestPrepareBodyStreamOptions(t *testing.T) {
	// 未带 stream_options → 注入
	out := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.2","messages":[]}`), false, nil)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}
	so, ok := obj["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options not injected: %v", obj["stream_options"])
	}
	if so["include_usage"] != true {
		t.Errorf("stream_options.include_usage = %v want true", so["include_usage"])
	}

	// 已带 stream_options → 不覆盖
	out2 := PrepareBodyOptWithEffertsPreserve(t, `{"model":"glm-5.2","messages":[],"stream_options":{"include_usage":false}}`)
	obj2, err := decodeBody(out2)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	so2, ok := obj2["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options lost: %v", obj2["stream_options"])
	}
	if so2["include_usage"] != false {
		t.Errorf("stream_options.include_usage = %v want false (not overwritten)", so2["include_usage"])
	}
}

// PrepareBodyOptWithEffertsPreserve helper：PrepareBodyOptWithEfforts 包装。
func PrepareBodyOptWithEffertsPreserve(t *testing.T, body string) []byte {
	t.Helper()
	return PrepareBodyOptWithEfforts([]byte(body), false, nil)
}

// decodeBody helper：解析 body JSON。
func decodeBody(b []byte) (map[string]any, error) {
	var obj map[string]any
	err := json.Unmarshal(b, &obj)
	return obj, err
}

func TestPrepareBodyOptWithEfforts(t *testing.T) {
	efforts := map[string][]string{
		"glm-5.2":      {"off", "low", "high"},
		"glm-5.2-mini": {"low", "medium"},
		"glm-5.2-max":  {"high", "xhigh"},
	}
	cases := []struct {
		name    string
		body    string
		efforts map[string][]string
		wantKey string // 输出应带有的 effort 字段名；空表示该字段应不存在
		wantVal string // 期望值
	}{
		{"downgrade to highest supported at or below request",
			`{"model":"glm-5.2-mini","reasoning_effort":"high"}`, efforts, "reasoning_effort", "medium"},
		{"floor to lowest when all supported above request",
			`{"model":"glm-5.2-max","reasoning_effort":"low"}`, efforts, "reasoning_effort", "high"},
		{"supported effort passes through unchanged",
			`{"model":"glm-5.2","reasoning_effort":"low"}`, efforts, "reasoning_effort", "low"},
		{"camelCase field name downgrades and keeps key",
			`{"model":"glm-5.2-mini","reasoningEffort":"high"}`, efforts, "reasoningEffort", "medium"},
		{"unknown model passes through",
			`{"model":"unknown","reasoning_effort":"max"}`, efforts, "reasoning_effort", "max"},
		{"unknown effort value passes through",
			`{"model":"glm-5.2","reasoning_effort":"ultra"}`, efforts, "reasoning_effort", "ultra"},
		{"empty cache passes through",
			`{"model":"glm-5.2","reasoning_effort":"max"}`, map[string][]string{}, "reasoning_effort", "max"},
		{"no effort field untouched",
			`{"model":"glm-5.2-mini","messages":[]}`, efforts, "", ""},
		{"nil efforts map passes through",
			`{"model":"glm-5.2","reasoning_effort":"max"}`, nil, "reasoning_effort", "max"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, c.efforts)
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatalf("unmarshal: %v (body=%s)", err, out)
			}
			if c.wantKey == "" {
				if _, ok := m["reasoning_effort"]; ok {
					t.Errorf("reasoning_effort should be absent, got %v", m["reasoning_effort"])
				}
				if _, ok := m["reasoningEffort"]; ok {
					t.Errorf("reasoningEffort should be absent, got %v", m["reasoningEffort"])
				}
				return
			}
			got, ok := m[c.wantKey].(string)
			if !ok || got != c.wantVal {
				t.Errorf("%s: got %v (%T) want %q", c.wantKey, m[c.wantKey], m[c.wantKey], c.wantVal)
			}
		})
	}
}

// TestNormalizeImageURL 覆盖 OpenAI chat 多模态内容的 image_url 兼容：
// 字符串形态必须转为上游需要的对象形态；对象形态及其中字段必须原样保留；
// 无效输入不补默认值，继续交给上游返回真实错误。
func TestNormalizeImageURL(t *testing.T) {
	tests := []struct {
		name string
		body string
		want any
	}{
		{
			name: "data url string to object",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":"data:image/png;base64,QUJD"}]}]}`,
			want: map[string]any{"url": "data:image/png;base64,QUJD"},
		},
		{
			name: "http url string to object",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"https://example.test/a.png"}]}]}`,
			want: map[string]any{"url": "https://example.test/a.png"},
		},
		{
			name: "object with detail preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD","detail":"low","mime_type":"image/png"}}]}]}`,
			want: map[string]any{"url": "data:image/png;base64,QUJD", "detail": "low", "mime_type": "image/png"},
		},
		{
			name: "invalid object url type preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":123}}]}]}`,
			want: map[string]any{"url": float64(123)},
		},
		{
			name: "missing image url preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url"}]}]}`,
			want: nil,
		},
		{
			name: "empty string preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":""}]}]}`,
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, sanitize := range []bool{false, true} {
				out := PrepareBodyOptWithEfforts([]byte(tc.body), sanitize, nil)
				obj, err := decodeBody(out)
				if err != nil {
					t.Fatalf("sanitize=%v unmarshal: %v (out=%s)", sanitize, err, out)
				}
				msgs := obj["messages"].([]any)
				content := msgs[0].(map[string]any)["content"].([]any)
				var part map[string]any
				for _, rawPart := range content {
					candidate, ok := rawPart.(map[string]any)
					if ok && candidate["type"] == "image_url" {
						part = candidate
						break
					}
				}
				if part == nil {
					t.Fatal("image_url part not found")
				}
				if tc.want == nil {
					if _, exists := part["image_url"]; exists {
						t.Fatalf("sanitize=%v: missing image_url should stay missing, got %#v", sanitize, part)
					}
					continue
				}
				if got := part["image_url"]; !reflect.DeepEqual(got, tc.want) {
					t.Errorf("sanitize=%v: image_url=%#v want %#v", sanitize, got, tc.want)
				}
			}
		})
	}
}

// TestClampGPTMinMaxTokens GPT 系上游要求 max_tokens ≥ 16（实测 gpt-6-sol/gpt-6-luna/
// gpt-5.6-sol：15 → 400 code=11133 model_param_invalid，16 → 200）。Claude Code 切
// 模型时的探针请求 max_tokens 极小，全号轮转同样被拒 → 客户端 503。非 GPT 模型不动。
func TestClampGPTMinMaxTokens(t *testing.T) {
	cases := []struct {
		name string
		body string
		want any // nil 表示字段不存在
	}{
		{"gpt below floor", `{"model":"gpt-6-sol","max_tokens":1,"messages":[]}`, float64(16)},
		{"gpt global prefix", `{"model":"global:gpt-5.6-sol","max_tokens":15,"messages":[]}`, float64(16)},
		{"gpt at floor", `{"model":"gpt-6-luna","max_tokens":16,"messages":[]}`, float64(16)},
		{"gpt above floor", `{"model":"gpt-6-sol","max_tokens":32000,"messages":[]}`, float64(32000)},
		{"gpt alias translated then clamped", `{"model":"gpt-6-sol","max_completion_tokens":1,"messages":[]}`, float64(16)},
		// 越界 float64（客户端按剩余额度算出的量级）：下限钳制不得把它误判成"低于下限"
		// 压成 16（int64(n) 溢出为 MinInt64 的旧行为），最终由管线末的上限钳制收到 1e6。
		{"gpt huge float: floor no-op, ceiling clamps", `{"model":"gpt-6-sol","max_tokens":1e19,"messages":[]}`, float64(1000000)},
		{"gpt absent untouched", `{"model":"gpt-6-sol","messages":[]}`, nil},
		{"non-gpt untouched", `{"model":"hy4-preview-f","max_tokens":1,"messages":[]}`, float64(1)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj, err := decodeBody(PrepareBodyOptWithEffertsPreserve(t, c.body))
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got, has := obj["max_tokens"]
			if c.want == nil {
				if has {
					t.Fatalf("max_tokens = %v, want absent", got)
				}
				return
			}
			if got != c.want {
				t.Fatalf("max_tokens = %v, want %v", got, c.want)
			}
		})
	}
}

// TestClampGPTMinMaxTokensFloatOverflow 钉住下限钳制自身的 float64 分支（不经管线）：
// 越界值不能走 int64(n)（amd64 溢出得 MinInt64 → 被误判成"低于下限"而压成 16，
// 与"只抬极小探针值"的本意相反）；负数越界仍按"低于下限"正常抬到 16。
func TestClampGPTMinMaxTokensFloatOverflow(t *testing.T) {
	obj := map[string]any{"model": "gpt-6-sol", "max_tokens": 1e19}
	clampGPTMinMaxTokens(obj)
	if got := obj["max_tokens"]; got != 1e19 {
		t.Fatalf("max_tokens = %v (%T), want untouched 1e19", got, got)
	}
	obj = map[string]any{"model": "gpt-6-sol", "max_tokens": -1e19}
	clampGPTMinMaxTokens(obj)
	if got := obj["max_tokens"]; got != int64(gptMinMaxTokens) {
		t.Fatalf("negative overflow: max_tokens = %v, want %d", got, gptMinMaxTokens)
	}
}

// TestClampMaxTokensCeiling max_tokens 上限钳制（吸收 workbuddy-manager 1.0.90）：
// 客户端按「剩余额度」做减法会算出 9.9e9 这类越界值，上游把越界整数包成
// no_healthy_account + 503，客户端误判服务不可用而反复重试/换号（body 不变则每次
// 重试都无用）。上限对**所有**模型生效；未越界/非数值不动，0 与负数仍走既有下限与
// 边缘逻辑（上限不改变下限语义）。别名形态必须先被翻译成 max_tokens 再钳制。
func TestClampMaxTokensCeiling(t *testing.T) {
	cases := []struct {
		name string
		body string
		want any // nil 表示字段不存在
	}{
		{"9.9e9 float64 clamped",
			`{"model":"hy4-preview-f","max_tokens":9.9e9,"messages":[]}`, float64(1000000)},
		{"exactly at ceiling untouched",
			`{"model":"hy4-preview-f","max_tokens":1000000,"messages":[]}`, float64(1000000)},
		{"one above ceiling clamped",
			`{"model":"hy4-preview-f","max_tokens":1000001,"messages":[]}`, float64(1000000)},
		{"real-world large value below ceiling untouched",
			`{"model":"hy4-preview-f","max_tokens":128000,"messages":[]}`, float64(128000)},
		{"alias 9.9e9 translated then clamped",
			`{"model":"deepseek-v4.1-flash","max_completion_tokens":9.9e9,"messages":[]}`, float64(1000000)},
		{"alias below ceiling translated, not clamped",
			`{"model":"deepseek-v4.1-flash","max_completion_tokens":128000,"messages":[]}`, float64(128000)},
		{"explicit max_tokens wins then clamps",
			`{"model":"deepseek-v4.1-flash","max_tokens":9900000000,"max_completion_tokens":123,"messages":[]}`, float64(1000000)},
		{"gpt alias big: floor no-op, ceiling clamps",
			`{"model":"gpt-6-sol","max_completion_tokens":9.9e9,"messages":[]}`, float64(1000000)},
		{"gpt below floor still raised (ceiling unrelated)",
			`{"model":"gpt-6-sol","max_tokens":1,"messages":[]}`, float64(16)},
		{"zero untouched (upstream default)",
			`{"model":"deepseek-v4.1-flash","max_tokens":0,"messages":[]}`, float64(0)},
		{"negative untouched (upstream rejects)",
			`{"model":"deepseek-v4.1-flash","max_tokens":-5,"messages":[]}`, float64(-5)},
		{"gpt negative still floored (existing edge logic intact)",
			`{"model":"gpt-6-sol","max_tokens":-5,"messages":[]}`, float64(16)},
		{"absent untouched",
			`{"model":"deepseek-v4.1-flash","messages":[]}`, nil},
		{"model absent still clamped (ceiling is model-independent)",
			`{"max_tokens":9.9e9,"messages":[]}`, float64(1000000)},
		{"string mangled passes through",
			`{"model":"deepseek-v4.1-flash","max_tokens":"9900000000","messages":[]}`, "9900000000"},
		// 越出 int64 可表示范围（|n| > ~9.22e18）：回归——原实现先 int64(n) 再比较，
		// Go 规范里越界转换结果实现相关（amd64 得 MinInt64 负数），比较恒假、钳制
		// 静默失效，越界值原样出站仍撞上游 503 no_healthy_account。
		{"1e19 float64 beyond int64 clamped (regression)",
			`{"model":"hy4-preview-f","max_tokens":1e19,"messages":[]}`, float64(1000000)},
		{"MaxFloat64 clamped (regression)",
			`{"model":"hy4-preview-f","max_tokens":1.7976931348623157e308,"messages":[]}`, float64(1000000)},
		// 别名形态：9e18 仍可翻译（≤ MaxInt64）→ 搬进 max_tokens 后走上限钳制；
		// 1e19 的翻译守卫（v == float64(int64(v))）在越界转换下为假，别名只删不译，
		// 出站无 max_tokens（越界值同样不外泄，回落上游默认档）。
		{"alias 9e18 translated then clamped",
			`{"model":"deepseek-v4.1-flash","max_completion_tokens":9e18,"messages":[]}`, float64(1000000)},
		{"alias 1e19 dropped by translate guard, no overflow outbound",
			`{"model":"deepseek-v4.1-flash","max_completion_tokens":1e19,"messages":[]}`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj, err := decodeBody(PrepareBodyOptWithEffertsPreserve(t, c.body))
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got, has := obj["max_tokens"]
			if c.want == nil {
				if has {
					t.Fatalf("max_tokens = %v, want absent", got)
				}
			} else if got != c.want {
				t.Fatalf("max_tokens = %v (%T), want %v", got, got, c.want)
			}
			// 别名无论翻译与否都被删除（既有语义，本次改动不改）。
			if _, has := obj["max_completion_tokens"]; has {
				t.Fatal("alias max_completion_tokens must be deleted")
			}
		})
	}
}

// TestClampMaxTokensCeilingIntForms 手构造 map 的 int/int64 形态（非 JSON 解码路径）
// 防御性兼容，取值口径与 clampGPTMinMaxTokens 一致：越界一律写回 int64 上限；
// 未越界不改写（原类型原样保留）；非数值形态原样不动（沿用既有「畸形值透传、
// 上游报 11101」策略）。
func TestClampMaxTokensCeilingIntForms(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want any
	}{
		{"int64 over ceiling", int64(9_900_000_000), int64(maxTokensCeiling)},
		{"int over ceiling", int(1_000_001), int64(maxTokensCeiling)},
		{"float64 over ceiling", 9.9e9, int64(maxTokensCeiling)},
		{"int64 at ceiling untouched", int64(1_000_000), int64(1_000_000)},
		{"int below ceiling untouched", int(42), int(42)},
		{"float64 below ceiling untouched", 128000.0, float64(128000)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj := map[string]any{"model": "m", "max_tokens": c.in}
			clampMaxTokensCeiling(obj)
			if got := obj["max_tokens"]; got != c.want {
				t.Fatalf("max_tokens = %v (%T), want %v (%T)", got, got, c.want, c.want)
			}
		})
	}

	for _, in := range []any{nil, "9900000000", true} {
		obj := map[string]any{"model": "m", "max_tokens": in}
		clampMaxTokensCeiling(obj)
		if obj["max_tokens"] != in {
			t.Errorf("non-numeric max_tokens %v mutated to %v", in, obj["max_tokens"])
		}
	}
}

// TestClampMaxTokensCeilingFloatBeyondInt64 越出 int64 可表示范围的 float64 形态
// （OCR 审查发现：原实现先 int64(n) 再比较，Go 规范里越界 float64→int64 转换结果
// 实现相关——amd64 得 MinInt64 负数，v > maxTokensCeiling 恒假，1e19 这类值静默
// 通过、仍撞上游 503 no_healthy_account）。修后先按浮点比较，越界一律钳回上限且
// 不 panic；int64/int 越界形态（值域天然在 int64 内）行为不变。
func TestClampMaxTokensCeilingFloatBeyondInt64(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want any
	}{
		{"float64 1e19 beyond int64 clamped", 1e19, int64(maxTokensCeiling)},
		{"float64 MaxFloat64 clamped", math.MaxFloat64, int64(maxTokensCeiling)},
		{"float64 1e300 clamped", 1e300, int64(maxTokensCeiling)},
		{"float64 -MaxFloat64 untouched (negative edge unchanged)", -math.MaxFloat64, -math.MaxFloat64},
		{"float64 boundary at ceiling untouched", 1_000_000.0, 1_000_000.0},
		{"float64 boundary one above clamped", 1_000_001.0, int64(maxTokensCeiling)},
		{"int64 MaxInt64 clamped", int64(math.MaxInt64), int64(maxTokensCeiling)},
		{"int int32-representable large clamped", int(1_500_000_000), int64(maxTokensCeiling)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj := map[string]any{"model": "m", "max_tokens": c.in}
			clampMaxTokensCeiling(obj) // 不得 panic（越界转换曾是实现相关 UB）
			if got := obj["max_tokens"]; got != c.want {
				t.Fatalf("max_tokens = %v (%T), want %v (%T)", got, got, c.want, c.want)
			}
		})
	}

	// NaN/±Inf 不可能出现在 JSON 里，但手构造 map 的调用方可能给出：不得 panic；
	// +Inf 判为越界并钳回上限，-Inf 不越界（与负数边缘语义一致，原样透传）。
	for _, c := range []struct {
		name string
		in   float64
		want any
	}{
		{"+Inf clamped", math.Inf(1), int64(maxTokensCeiling)},
		{"-Inf untouched", math.Inf(-1), math.Inf(-1)},
	} {
		t.Run(c.name, func(t *testing.T) {
			obj := map[string]any{"model": "m", "max_tokens": c.in}
			clampMaxTokensCeiling(obj)
			if got := obj["max_tokens"]; got != c.want {
				t.Fatalf("max_tokens = %v (%T), want %v (%T)", got, got, c.want, c.want)
			}
		})
	}
	t.Run("NaN untouched", func(t *testing.T) {
		obj := map[string]any{"model": "m", "max_tokens": math.NaN()}
		clampMaxTokensCeiling(obj) // 不得 panic
		if got, ok := obj["max_tokens"].(float64); !ok || !math.IsNaN(got) {
			t.Fatalf("max_tokens = %v, want NaN untouched", obj["max_tokens"])
		}
	})

	// 不变式（端到端）：任何越界输入形态都不得让越界 max_tokens 出站——要么被钳到
	// 上限，要么字段缺失（别名守卫拒绝翻译）。这正是要消灭的 503 形态。
	hostile := []string{
		`{"model":"hy4-preview-f","max_tokens":1e19,"messages":[]}`,
		`{"model":"hy4-preview-f","max_tokens":1.7976931348623157e308,"messages":[]}`,
		`{"model":"gpt-6-sol","max_tokens":1e19,"messages":[]}`,
		`{"model":"deepseek-v4.1-flash","max_completion_tokens":1e19,"messages":[]}`,
		`{"model":"deepseek-v4.1-flash","max_completion_tokens":9e18,"messages":[]}`,
		`{"model":"deepseek-v4.1-flash","max_completion_tokens":1e19,"max_tokens":9.9e9,"messages":[]}`,
		`{"max_tokens":1e19,"messages":[]}`,
	}
	for _, body := range hostile {
		obj, err := decodeBody(PrepareBodyOptWithEffertsPreserve(t, body))
		if err != nil {
			t.Fatalf("unmarshal: %v (in=%s)", err, body)
		}
		switch v := obj["max_tokens"].(type) {
		case nil:
			// 字段缺失：无越界值可出站（别名守卫拒绝翻译），合规。
		case float64:
			if v > maxTokensCeiling {
				t.Errorf("in=%s: outbound max_tokens=%v exceeds ceiling", body, v)
			}
		case int64:
			if v > maxTokensCeiling {
				t.Errorf("in=%s: outbound max_tokens=%d exceeds ceiling", body, v)
			}
		case int:
			if v > maxTokensCeiling {
				t.Errorf("in=%s: outbound max_tokens=%d exceeds ceiling", body, v)
			}
		default:
			t.Errorf("in=%s: unexpected outbound max_tokens type %T", body, v)
		}
	}
}

// TestNormalizeToolPatterns 工具 schema pattern 的 `\_` 转义归一（11129 实案：
// exa agent_run 的 `^agent\_run\_` 被 deepseek 系确定性拒收，归一后上游 200）。
// 覆盖：function.parameters 与裸 parameters 两形态、嵌套 schema、patternProperties
// 键、消息正文不碰、无 tools no-op、sanitize 开关两态一致。
func TestNormalizeToolPatterns(t *testing.T) {
	lookupPattern := func(t *testing.T, body string, path ...any) string {
		t.Helper()
		var obj map[string]any
		if err := json.Unmarshal([]byte(body), &obj); err != nil {
			t.Fatalf("out not json: %v", err)
		}
		cur := any(obj)
		for _, p := range path {
			switch k := p.(type) {
			case string:
				m, ok := cur.(map[string]any)
				if !ok {
					t.Fatalf("path %v: want object, got %T", path, cur)
				}
				cur = m[k]
			case int:
				l, ok := cur.([]any)
				if !ok || k >= len(l) {
					t.Fatalf("path %v: bad array at %T", path, cur)
				}
				cur = l[k]
			}
		}
		s, _ := cur.(string)
		return s
	}

	t.Run("exa agent_run pattern normalized", func(t *testing.T) {
		bs := string(byte(92)) // 反斜杠，测试体经工具链多层转义易被吞，运行时拼装保真
		body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"agent_run","parameters":{"type":"object","properties":{"runId":{"type":"string","pattern":"^agent` + bs + bs + `_run` + bs + bs + `_"},"query":{"type":"string"}},"required":["query"]}}}]}` //nolint:lll // 实案 body 原样
		out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))
		if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "properties", "runId", "pattern"); got != `^agent_run_` {
			t.Fatalf("pattern = %q, want %q", got, `^agent_run_`)
		}
		// 同 schema 的兄弟字段不受影响
		if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "properties", "query", "type"); got != "string" {
			t.Fatalf("sibling field disturbed: %q", got)
		}
	})

	t.Run("nested anyOf items pattern normalized", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":{"anyOf":[{"properties":{"code":{"pattern":"x` + bs + bs + `_y"}}},{"type":"string"}]}}}]}`
		out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))
		if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "anyOf", 0, "properties", "code", "pattern"); got != "x_y" {
			t.Fatalf("nested pattern = %q, want %q", got, "x_y")
		}
	})

	t.Run("bare tool parameters form normalized", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"name":"t","parameters":{"properties":{"id":{"pattern":"p` + bs + bs + `_q"}}}}]}`
		out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))
		if got := lookupPattern(t, out, "tools", 0, "parameters", "properties", "id", "pattern"); got != "p_q" {
			t.Fatalf("bare parameters pattern = %q, want %q", got, "p_q")
		}
	})

	t.Run("patternProperties key normalized", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":{"patternProperties":{"^a` + bs + bs + `_b` + bs + bs + `_":{"type":"string"}}}}}]}`
		var obj map[string]any
		out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("out not json: %v", err)
		}
		tool := obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
		props := tool["patternProperties"].(map[string]any)
		if _, ok := props["^a_b_"]; !ok {
			t.Fatalf("normalized key missing: %v", props)
		}
		for k := range props {
			if strings.Contains(k, string(byte(92))) {
				t.Fatalf("patternProperties key not normalized: %q", k)
			}
		}
	})

	t.Run("message content backslash untouched", func(t *testing.T) {
		bs := string(byte(92))
		raw := `{"messages":[{"role":"user","content":"path C:` + bs + bs + `_dir and regex a` + bs + bs + `_b"}],"tools":[{"type":"function","function":{"name":"t","parameters":{"properties":{"p":{"pattern":"q` + bs + bs + `_r"}}}}}]}`
		out := string(PrepareBodyOptWithEfforts([]byte(raw), false, nil))
		var obj map[string]any
		if err := json.Unmarshal([]byte(out), &obj); err != nil {
			t.Fatalf("out not json: %v", err)
		}
		msg := obj["messages"].([]any)[0].(map[string]any)
		if got := msg["content"]; got != `path C:\_dir and regex a\_b` {
			t.Fatalf("message content mutated: %q", got)
		}
	})

	t.Run("clean pattern untouched", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":{"properties":{"p":{"pattern":"^[a-z]+` + bs + bs + `d_$"}}}}}]}`
		out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))
		if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "properties", "p", "pattern"); got != `^[a-z]+\d_$` {
			t.Fatalf("clean pattern mutated: %q", got)
		}
	})

	t.Run("no tools is no-op", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"model":"m","messages":[{"role":"user","content":"a` + bs + bs + `_b"}]}`
		var inObj, outObj map[string]any
		if err := json.Unmarshal([]byte(body), &inObj); err != nil {
			t.Fatal(err)
		}
		out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
		if err := json.Unmarshal(out, &outObj); err != nil {
			t.Fatalf("out not json: %v", err)
		}
		inMsg, _ := json.Marshal(inObj["messages"])
		outMsg, _ := json.Marshal(outObj["messages"])
		if string(inMsg) != string(outMsg) {
			t.Fatalf("messages changed without tools: %s -> %s", inMsg, outMsg)
		}
		if _, has := outObj["tools"]; has {
			t.Fatal("tools appeared out of nowhere")
		}
	})

	t.Run("sanitize on and off behave the same", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":{"properties":{"p":{"pattern":"a` + bs + bs + `_b"}}}}}]}` //nolint:lll
		for _, sanitize := range []bool{false, true} {
			out := string(PrepareBodyOptWithEfforts([]byte(body), sanitize, nil))
			if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "properties", "p", "pattern"); got != "a_b" {
				t.Fatalf("sanitize=%v: pattern = %q, want %q", sanitize, got, "a_b")
			}
		}
	})
}
