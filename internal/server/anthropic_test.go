// anthropic_test.go Anthropic 兼容层（anthropic.go）的单元与端到端测试。
// 转换纯函数直测；端点走 httptest + newFakeUpstream（与 handler_test.go 同风格）。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// parseAnthEvents 把 SSE 字节流拆成 (event, payload map) 序列（只认 event: 行）。
func parseAnthEvents(t *testing.T, body string) []struct {
	Name    string
	Payload map[string]any
} {
	t.Helper()
	var out []struct {
		Name    string
		Payload map[string]any
	}
	for _, block := range strings.Split(body, "\n\n") {
		var name string
		var payload map[string]any
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				name = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
					t.Fatalf("bad event payload %q: %v", line, err)
				}
			}
		}
		if name != "" {
			out = append(out, struct {
				Name    string
				Payload map[string]any
			}{name, payload})
		}
	}
	return out
}

// anthEventNames 取事件名序列（断言用）。
func anthEventNames(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, e := range parseAnthEvents(t, body) {
		out = append(out, e.Name)
	}
	return out
}

func eqStr(t *testing.T, got, want string, ctx string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %q want %q", ctx, got, want)
	}
}

// ---------------------------------------------------------------------------
// 请求转换
// ---------------------------------------------------------------------------

// TestToOpenAIRequestSystemBlocks system 顶层字段：字符串与 block 数组两种形态都
// 转成首条 system 消息。
func TestToOpenAIRequestSystemBlocks(t *testing.T) {
	// 字符串形态。
	out := toOpenAIRequest(map[string]any{
		"model": "m",
		"system": map[string]any{
			"role": "system",
		},
	})
	_ = out
	// 上面是占位防误写，真正断言在下面两种形态。
	out = toOpenAIRequest(map[string]any{
		"model":  "cn:glm-5.2",
		"system": "你是助手",
	})
	msgs := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("len=%d", len(msgs))
	}
	m0 := msgs[0].(map[string]any)
	eqStr(t, m0["role"].(string), "system", "role")
	eqStr(t, m0["content"].(string), "你是助手", "content")

	// block 数组形态（多个 text block 以换行连接）。
	out = toOpenAIRequest(map[string]any{
		"model": "cn:glm-5.2",
		"system": []any{
			map[string]any{"type": "text", "text": "第一段"},
			map[string]any{"type": "text", "text": "第二段"},
		},
	})
	msgs = out["messages"].([]any)
	m0 = msgs[0].(map[string]any)
	eqStr(t, m0["content"].(string), "第一段\n第二段", "system blocks joined")
}

// TestToOpenAIRequestThinkingAdaptive thinking.type=enabled 与 adaptive 都算启用
// 思考（只认 enabled 会丢新版 Claude Code 的推理）；budget_tokens 按档位映射
// reasoning_effort；thinking 块回传时挂 reasoning_content 到 assistant 消息。
func TestToOpenAIRequestThinkingAdaptive(t *testing.T) {
	mk := func(typ string) map[string]any {
		return map[string]any{
			"model": "cn:deepseek-v4-flash",
			"max_tokens": 1024,
			"thinking": map[string]any{"type": typ},
			"messages": []any{
				map[string]any{"role": "user", "content": "hi"},
			},
		}
	}
	if !anthThinkingEnabled(mk("enabled")) || !anthThinkingEnabled(mk("adaptive")) {
		t.Fatal("enabled 与 adaptive 都应算启用思考")
	}
	if anthThinkingEnabled(mk("disabled")) {
		t.Error("disabled 不应算启用")
	}

	// budget_tokens 分档：<4096 low、<16384 medium、<16384 high 之下、≥65536 max。
	cases := []struct {
		budget float64
		want   string
	}{{2048, "low"}, {8192, "medium"}, {32768, "high"}, {65536, "max"}}
	for _, c := range cases {
		body := map[string]any{
			"model": "cn:deepseek-v4-flash",
			"thinking": map[string]any{
				"type":          "enabled",
				"budget_tokens": c.budget,
			},
		}
		if got := anthReasoningEffort(body); got != c.want {
			t.Errorf("budget=%v effort=%q want %q", c.budget, got, c.want)
		}
	}
	// output_config.effort 优先于 budget_tokens。
	body := map[string]any{
		"output_config": map[string]any{"effort": "high"},
		"thinking":      map[string]any{"type": "enabled", "budget_tokens": 2048.0},
	}
	if got := anthReasoningEffort(body); got != "high" {
		t.Errorf("output_config.effort should win, got %q", got)
	}

	// assistant 消息里的 thinking 块 → reasoning_content。
	out := toOpenAIRequest(map[string]any{
		"model": "cn:deepseek-v4-flash",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "推理过程"},
				map[string]any{"type": "text", "text": "答案"},
			}},
		},
	})
	msgs := out["messages"].([]any)
	m0 := msgs[0].(map[string]any)
	if rc, _ := m0["reasoning_content"].(string); rc != "推理过程" {
		t.Errorf("reasoning_content=%q", rc)
	}
	eqStr(t, m0["content"].(string), "答案", "content")
}

// TestToOpenAIRequestToolResultImages tool_result 内嵌图片：文字留在 role:tool
// 消息，图片提取并入紧随其后的 user 消息（不能把 base64 序列化成文本）。
func TestToOpenAIRequestToolResultImages(t *testing.T) {
	b64 := "aGVsbG8="
	out := toOpenAIRequest(map[string]any{
		"model": "cn:glm-5.2v",
		"messages": []any{
			map[string]any{"role": "user", "content": "看图"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "screenshot", "input": map[string]any{}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": []any{
					map[string]any{"type": "text", "text": "截图结果"},
					map[string]any{"type": "image", "source": map[string]any{
						"type": "base64", "media_type": "image/png", "data": b64,
					}},
				}},
			}},
		},
	})
	msgs := out["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("len=%d want 4 (user/assistant-toolcall/tool/text-user)", len(msgs))
	}
	// 第三条：role:tool，只有文字。
	tool := msgs[2].(map[string]any)
	eqStr(t, tool["role"].(string), "tool", "role")
	eqStr(t, tool["tool_call_id"].(string), "toolu_1", "tool_call_id")
	eqStr(t, tool["content"].(string), "截图结果", "tool content")
	// 第四条：user 消息承载图片。
	user := msgs[3].(map[string]any)
	eqStr(t, user["role"].(string), "user", "role")
	parts, _ := user["content"].([]any)
	if len(parts) != 1 {
		t.Fatalf("user parts=%d want 1 (image)", len(parts))
	}
	p0 := parts[0].(map[string]any)
	eqStr(t, p0["type"].(string), "image_url", "part type")
	imgURL := p0["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(imgURL, "data:image/png;base64,") || !strings.Contains(imgURL, b64) {
		t.Errorf("image url=%q", imgURL)
	}
	// 图片绝不落进 tool 文本。
	if strings.Contains(tool["content"].(string), b64) {
		t.Error("base64 must not leak into tool content")
	}
	// 全文扫描：base64 只出现在 image_url 里，不作为纯文本内容出现。
	raw, _ := json.Marshal(out)
	if strings.Count(string(raw), b64) != 1 {
		t.Errorf("base64 appears %d times, want 1 (image_url only)", strings.Count(string(raw), b64))
	}
}

// TestToOpenAIRequestToolResultTextOnly 纯文字 tool_result：转成 role:tool 消息；
// 只有图片时 tool content 用占位符（上游要求非空）。
func TestToOpenAIRequestToolResultTextOnly(t *testing.T) {
	out := toOpenAIRequest(map[string]any{
		"model": "cn:glm-5.2",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "42"},
			}},
		},
	})
	msgs := out["messages"].([]any)
	tool := msgs[0].(map[string]any)
	eqStr(t, tool["role"].(string), "tool", "role")
	eqStr(t, tool["content"].(string), "42", "content")
}

// TestToOpenAIRequestToolsAndChoice tools/tool_choice 映射：input_schema →
// parameters，any → required，tool → {type:function}，none 直传（真正的删 tools
// 动作由下游 payload 管线做）；disable_parallel_tool_use → parallel_tool_calls:false。
func TestToOpenAIRequestToolsAndChoice(t *testing.T) {
	out := toOpenAIRequest(map[string]any{
		"model":     "cn:glm-5.2",
		"max_tokens": 100,
		"tools": []any{
			map[string]any{
				"name":         "get_weather",
				"description":  "查天气",
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
		"tool_choice": map[string]any{"type": "any"},
	})
	tools := out["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	eqStr(t, fn["name"].(string), "get_weather", "name")
	if _, ok := fn["parameters"].(map[string]any); !ok {
		t.Error("parameters missing")
	}
	eqStr(t, out["tool_choice"].(string), "required", "any→required")

	// tool 指定 + parallel 禁用。
	out = toOpenAIRequest(map[string]any{
		"model":       "cn:glm-5.2",
		"tool_choice": map[string]any{"type": "tool", "name": "get_weather", "disable_parallel_tool_use": true},
	})
	tc := out["tool_choice"].(map[string]any)
	eqStr(t, tc["type"].(string), "function", "choice type")
	if v, ok := out["parallel_tool_calls"]; !ok || v != false {
		t.Errorf("parallel_tool_calls=%v want false", out["parallel_tool_calls"])
	}

	// stop_sequences → stop；metadata.user_id → user。
	out = toOpenAIRequest(map[string]any{
		"model":          "cn:glm-5.2",
		"stop_sequences": []any{"END"},
		"metadata":       map[string]any{"user_id": "u-777"},
	})
	if stop, ok := out["stop"].([]any); !ok || len(stop) != 1 || stop[0] != "END" {
		t.Errorf("stop=%v", out["stop"])
	}
	eqStr(t, out["user"].(string), "u-777", "user")
}

// TestToAnthropicResponseStopReason 响应转换：finish_reason → stop_reason 映射、
// reasoning_content → thinking block、tool_calls → tool_use block、usage 口径换算。
func TestToAnthropicResponseStopReason(t *testing.T) {
	mk := func(finish string) map[string]any {
		return map[string]any{
			"id":      "chatcmpl-1",
			"object":  "chat.completion",
			"model":   "glm-5.2",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "你好"}, "finish_reason": finish}},
			"usage": map[string]any{
				"prompt_tokens":              100.0,
				"completion_tokens":          20.0,
				"prompt_cache_hit_tokens":    40.0,
				"prompt_cache_write_tokens":  10.0,
			},
		}
	}
	cases := map[string]string{
		"stop":           "end_turn",
		"length":         "max_tokens",
		"tool_calls":     "tool_use",
		"content_filter": "end_turn",
	}
	for finish, want := range cases {
		resp := toAnthropicResponse(mk(finish), "glm-5.2", false)
		if got := resp["stop_reason"]; got != want {
			t.Errorf("finish=%s stop_reason=%v want %s", finish, got, want)
		}
	}
	// usage 口径：input 不含缓存（100-40-10=50），缓存两段独立。
	resp := toAnthropicResponse(mk("stop"), "glm-5.2", false)
	usage := resp["usage"].(map[string]any)
	if usage["input_tokens"] != 50 {
		t.Errorf("input_tokens=%v want 50（缓存不得双算）", usage["input_tokens"])
	}
	if usage["cache_read_input_tokens"] != 40 || usage["cache_creation_input_tokens"] != 10 {
		t.Errorf("cache fields=%v/%v want 40/10", usage["cache_read_input_tokens"], usage["cache_creation_input_tokens"])
	}
	// thinking=true 时 reasoning_content → thinking block（带 signature），在最前。
	data := mk("stop")
	data["choices"] = []any{map[string]any{
		"index":         0,
		"message":       map[string]any{"role": "assistant", "content": "答案", "reasoning_content": "思考中"},
		"finish_reason": "stop",
	}}
	resp = toAnthropicResponse(data, "glm-5.2", true)
	blocks := resp["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("blocks=%d want 2 (thinking+text)", len(blocks))
	}
	tb := blocks[0].(map[string]any)
	eqStr(t, tb["type"].(string), "thinking", "block type")
	eqStr(t, tb["thinking"].(string), "思考中", "thinking")
	if tb["signature"] == "" {
		t.Error("signature missing（客户端回传凭据）")
	}
	eqStr(t, blocks[1].(map[string]any)["type"].(string), "text", "second block")
}

// TestToAnthropicResponseToolUse tool_calls → tool_use block：arguments 字符串
// 解析为 input 对象；非法 JSON 落 _raw 占位（不让整次调用失败）。
func TestToAnthropicResponseToolUse(t *testing.T) {
	data := map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role": "assistant",
				"tool_calls": []any{
					map[string]any{"id": "call_1", "type": "function", "function": map[string]any{
						"name": "get_weather", "arguments": `{"city":"北京"}`,
					}},
					map[string]any{"id": "call_2", "type": "function", "function": map[string]any{
						"name": "broken", "arguments": `{oops`,
					}},
				},
			},
			"finish_reason": "tool_calls",
		}},
	}
	resp := toAnthropicResponse(data, "glm-5.2", false)
	if resp["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason=%v", resp["stop_reason"])
	}
	blocks := resp["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("blocks=%d", len(blocks))
	}
	b0 := blocks[0].(map[string]any)
	eqStr(t, b0["type"].(string), "tool_use", "type")
	eqStr(t, b0["id"].(string), "call_1", "id")
	input := b0["input"].(map[string]any)
	eqStr(t, input["city"].(string), "北京", "input.city")
	// 非法 JSON → _raw 原文。
	b1 := blocks[1].(map[string]any)
	raw := b1["input"].(map[string]any)["_raw"]
	if raw != `{oops` {
		t.Errorf("_raw=%v", raw)
	}
}

// ---------------------------------------------------------------------------
// 流式事件序列
// ---------------------------------------------------------------------------

// TestStreamTranslatorEventSequence OpenAI SSE → Anthropic 事件流：文本流、
// thinking+text 流、tool_use 流的事件序列与增量内容。
func TestStreamTranslatorEventSequence(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		tr := newStreamTranslator("glm-5.2", false)
		var buf []byte
		buf = append(buf, tr.feed(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"role": "assistant", "content": "你"}}},
		})...)
		buf = append(buf, tr.feed(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": "好"}, "finish_reason": nil}},
		})...)
		buf = append(buf, tr.feed(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 5.0, "completion_tokens": 2.0},
		})...)
		events := parseAnthEvents(t, string(buf))
		names := anthEventNames(t, string(buf))
		want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
		if strings.Join(names, ",") != strings.Join(want, ",") {
			t.Fatalf("events=%v want %v", names, want)
		}
		// delta 内容。
		if d := events[2].Payload["delta"].(map[string]any); d["text"] != "你" || d["type"] != "text_delta" {
			t.Errorf("delta1=%v", d)
		}
		// message_delta 带累计 usage（input_tokens 必须在末尾补全）。
		// JSON 断言值是 float64：与 int 常量做 any 比较类型不同恒不等。
		md := events[len(events)-2].Payload
		if u := md["usage"].(map[string]any); u["input_tokens"] != float64(5) || u["output_tokens"] != float64(2) {
			t.Errorf("message_delta usage=%v", u)
		}
		if sr := md["delta"].(map[string]any)["stop_reason"]; sr != "end_turn" {
			t.Errorf("stop_reason=%v", sr)
		}
	})

	t.Run("thinking", func(t *testing.T) {
		tr := newStreamTranslator("glm-5.2", true)
		var buf []byte
		buf = append(buf, tr.feed(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"reasoning_content": "想"}}},
		})...)
		buf = append(buf, tr.feed(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": "答"}}},
		})...)
		buf = append(buf, tr.finish("stop")...)
		names := anthEventNames(t, string(buf))
		// thinking 块在 text 块之前开；signature_delta 在 content_block_stop 之前
		// （stop 后到达的 delta 会被客户端丢弃，签名就白发了）。
		want := []string{
			"message_start",
			"content_block_start", // thinking
			"content_block_delta", // thinking_delta
			"content_block_delta", // signature_delta
			"content_block_stop",  // thinking
			"content_block_start", // text
			"content_block_delta", // text_delta
			"content_block_stop",  // text
			"message_delta",
			"message_stop",
		}
		if strings.Join(names, ",") != strings.Join(want, ",") {
			t.Fatalf("events=%v want %v", names, want)
		}
		events := parseAnthEvents(t, string(buf))
		if d := events[2].Payload["delta"].(map[string]any); d["type"] != "thinking_delta" || d["thinking"] != "想" {
			t.Errorf("thinking delta=%v", d)
		}
		if d := events[3].Payload["delta"].(map[string]any); d["type"] != "signature_delta" || d["signature"] != "想" {
			t.Errorf("signature delta=%v", d)
		}
		// thinking 块 index 0、text 块 index 1（递增）。
		if idx := events[1].Payload["index"]; idx != float64(0) {
			t.Errorf("think index=%v", idx)
		}
		if idx := events[5].Payload["index"]; idx != float64(1) {
			t.Errorf("text index=%v", idx)
		}
	})

	t.Run("tool_use", func(t *testing.T) {
		tr := newStreamTranslator("glm-5.2", false)
		var buf []byte
		buf = append(buf, tr.feed(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": 0, "id": "call_9", "type": "function", "function": map[string]any{"name": "run", "arguments": ""}},
			}}}},
		})...)
		// 参数分片原样透传（partial_json），拼接交给客户端。
		buf = append(buf, tr.feed(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": 0, "function": map[string]any{"arguments": `{"pa`}},
			}}}},
		})...)
		buf = append(buf, tr.feed(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": 0, "function": map[string]any{"arguments": `th":"x}"}`}},
			}}}},
		})...)
		buf = append(buf, tr.finish("tool_calls")...)
		events := parseAnthEvents(t, string(buf))
		names := anthEventNames(t, string(buf))
		want := []string{
			"message_start",
			"content_block_start",  // tool_use
			"content_block_delta",  // partial {"pa
			"content_block_delta",  // partial th":"x}"
			"content_block_stop",   // tool_use
			"message_delta",
			"message_stop",
		}
		if strings.Join(names, ",") != strings.Join(want, ",") {
			t.Fatalf("events=%v want %v", names, want)
		}
		start := events[1].Payload["content_block"].(map[string]any)
		eqStr(t, start["type"].(string), "tool_use", "block type")
		eqStr(t, start["id"].(string), "call_9", "tool id")
		if d := events[2].Payload["delta"].(map[string]any); d["type"] != "input_json_delta" || d["partial_json"] != `{"pa` {
			t.Errorf("partial1=%v", d)
		}
		if d := events[3].Payload["delta"].(map[string]any); d["partial_json"] != `th":"x}"}` {
			t.Errorf("partial2=%v", d)
		}
		if sr := events[5].Payload["delta"].(map[string]any)["stop_reason"]; sr != "tool_use" {
			t.Errorf("stop_reason=%v", sr)
		}
	})

	t.Run("finished latch", func(t *testing.T) {
		tr := newStreamTranslator("glm-5.2", false)
		// 先喂一个不含 finish_reason 的内容帧：收尾必须由显式 finish 触发，
		// 否则首个 finish 之前流就 closed，n==0 的断言失去意义。
		_ = tr.feed(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": "x"}}},
		})
		n := len(tr.finish("stop"))
		if extra := tr.feed(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": "late"}}},
		}); len(extra) != 0 {
			t.Error("frames after finish must not emit events")
		}
		if extra := tr.finish("stop"); len(extra) != 0 {
			t.Error("finish must be idempotent")
		}
		if n == 0 {
			t.Error("first finish must emit")
		}
	})

	// M10 回归：并行 tool_calls 参数分片跨帧交错（index=0 与 index=1 交替到达），
	// 各自分槽独立成块——单 toolIdx 形态会把后到分片写进错误的块（串块）。
	t.Run("parallel tool_use interleaved", func(t *testing.T) {
		tr := newStreamTranslator("glm-5.2", false)
		var buf []byte
		// 两把工具同时开块（同一帧内两个带名分片）。
		buf = append(buf, tr.feed(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": 0, "id": "call_a", "type": "function", "function": map[string]any{"name": "search", "arguments": ""}},
				map[string]any{"index": 1, "id": "call_b", "type": "function", "function": map[string]any{"name": "fetch", "arguments": ""}},
			}}}},
		})...)
		// 参数分片交错：0 号的片段、1 号的片段、再 0 号的片段。
		buf = append(buf, tr.feed(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": 0, "function": map[string]any{"arguments": `{"q":`}},
				map[string]any{"index": 1, "function": map[string]any{"arguments": `{"u`}},
			}}}},
		})...)
		buf = append(buf, tr.feed(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": 1, "function": map[string]any{"arguments": `rl":"x"}`}},
				map[string]any{"index": 0, "function": map[string]any{"arguments": `"hi"}`}},
			}}}},
		})...)
		buf = append(buf, tr.finish("tool_calls")...)
		events := parseAnthEvents(t, string(buf))

		// 两个 tool_use 块（index 0 与 1），各两片 partial_json，交错片段必须
		// 落到各自块的 index 上（0 号块收到 {"q": 与 "hi"}，1 号块收到 {"u 与 rl":"x"}）。
		type frag struct{ block int; pj string }
		var got []frag
		for _, ev := range events {
			if ev.Payload["type"] != "content_block_delta" {
				continue
			}
			if d, ok := ev.Payload["delta"].(map[string]any); ok && d["type"] == "input_json_delta" {
				got = append(got, frag{block: int(ev.Payload["index"].(float64)), pj: d["partial_json"].(string)})
			}
		}
		want := []frag{{0, `{"q":`}, {1, `{"u`}, {1, `rl":"x"}`}, {0, `"hi"}`}}
		if len(got) != len(want) {
			t.Fatalf("partials=%v want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("partial[%d]=%+v want %+v（交错分片串块）", i, got[i], want[i])
			}
		}
		// 两块都关掉（各一个 content_block_stop，index 0 与 1）。
		stops := map[float64]bool{}
		for _, ev := range events {
			if ev.Payload["type"] == "content_block_stop" {
				stops[ev.Payload["index"].(float64)] = true
			}
		}
		if !stops[0] || !stops[1] {
			t.Errorf("content_block_stop 缺块: %v", stops)
		}
	})
}

// ---------------------------------------------------------------------------
// count_tokens 估算
// ---------------------------------------------------------------------------

// TestCountTokensEstimate CJK 约 1 字符 1 token、其余约 4 字符 1 token、每 block
// +4 结构开销、下限 1；允许不带 model；鉴权与 /v1/messages 同口径。
func TestCountTokensEstimate(t *testing.T) {
	// 纯 CJK：5 字 → 5 token + 1 block*4 = 9。
	// 纯 ASCII："abcdefgh" 8 字符 → 2 token + 4 = 6。
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})})
	h.mountAnthropic(h.mux) // 主代理接线前 NewHandler 不含 Anthropic 路由，测试显式挂载
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(body)))
		return rec
	}
	// 不带 model（常态）。
	rec := post(`{"messages":[{"role":"user","content":"abcdefgh"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("bad json %s", rec.Body)
	}
	if out.InputTokens != 2+4 {
		t.Errorf("ascii 8 chars: %d want 6", out.InputTokens)
	}
	// CJK 高估方向：同样 5 个字符，中文必须不少于英文的估算。
	rec = post(`{"messages":[{"role":"user","content":"你好世界呀"}]}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.InputTokens != 5+4 {
		t.Errorf("cjk 5 chars: %d want 9", out.InputTokens)
	}
	// system + 多消息 + tools 的结构开销。
	rec = post(`{"system":"sys","messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}],"tools":[{"name":"t","input_schema":{"type":"object"}}]}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	// system "sys"=1 + 3 个 block*4=12，"a"=1+4? 总 = 1+1+1+12+8(tools json≈2)+... 只断言下限与单调。
	if out.InputTokens < 20 {
		t.Errorf("multi-block estimate too low: %d", out.InputTokens)
	}
	// 空体：下限 1。
	rec = post(`{}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.InputTokens < 1 {
		t.Errorf("empty=%d want >=1", out.InputTokens)
	}
}

// estimateTokens 直测：CJK 与 ASCII 的量级差异。
func TestEstimateTokensCJK(t *testing.T) {
	en := estimateTokens("abcdefgh") // 8 ASCII → 2
	zh := estimateTokens("你好世界呀") // 5 CJK → 5
	if en != 2 || zh != 5 {
		t.Errorf("en=%d zh=%d want 2/5", en, zh)
	}
	if estimateTokens("") != 0 {
		t.Error("empty must be 0")
	}
}

// ---------------------------------------------------------------------------
// 端到端（经管线）
// ---------------------------------------------------------------------------

// newAnthropicHandler 构造带 fake 上游的 handler（与 handler_test.go 同风格，
// 直接复用既有 newFakeUpstream）。路由显式挂载：主代理接线前 NewHandler 不含
// Anthropic 路由，测试经 mountAnthropic 走与生产相同的注册路径。
func newAnthropicHandler(t *testing.T, behavior func(authz string) (int, string, bool)) *Handler {
	t.Helper()
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, behavior),
	})
	h.mountAnthropic(h.mux)
	return h
}

// TestAnthropicMessagesNonStreaming 端到端：Anthropic 请求 → 管线 → Anthropic 响应。
func TestAnthropicMessagesNonStreaming(t *testing.T) {
	upCalled := false
	h := newAnthropicHandler(t, func(authz string) (int, string, bool) {
		upCalled = true
		return 200, sseOK, true
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"cn:glm-5.2","max_tokens":100,"messages":[{"role":"user","content":"你好"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !upCalled {
		t.Fatal("upstream not called（必须复用管线，不允许新开上游路径）")
	}
	var resp map[string]any
	if json.Unmarshal(rec.Body.Bytes(), &resp) != nil {
		t.Fatalf("bad json %s", rec.Body)
	}
	if resp["type"] != "message" || resp["role"] != "assistant" {
		t.Errorf("type/role=%v/%v", resp["type"], resp["role"])
	}
	if id, _ := resp["id"].(string); !strings.HasPrefix(id, "msg_") {
		t.Errorf("id=%v want msg_ prefix", resp["id"])
	}
	blocks, _ := resp["content"].([]any)
	if len(blocks) == 0 {
		t.Fatal("no content blocks")
	}
	// sseOK 内容是 "你好"。
	if b0 := blocks[0].(map[string]any); b0["type"] != "text" || b0["text"] != "你好" {
		t.Errorf("block0=%v", b0)
	}
	if resp["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason=%v", resp["stop_reason"])
	}
}

// TestAnthropicMessagesStreaming 端到端流式：管线 OpenAI SSE → Anthropic 事件流。
func TestAnthropicMessagesStreaming(t *testing.T) {
	h := newAnthropicHandler(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"cn:glm-5.2","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"你好"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type=%q", ct)
	}
	names := anthEventNames(t, rec.Body.String())
	want := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("events=%v want %v", names, want)
	}
	// message_delta 必须带 usage（sseOK 末帧 usage prompt=1 completion=1）。
	for _, e := range parseAnthEvents(t, rec.Body.String()) {
		if e.Name == "message_delta" {
			u := e.Payload["usage"].(map[string]any)
			if u["output_tokens"] != float64(1) || u["input_tokens"] != float64(1) {
				t.Errorf("message_delta usage=%v", u)
			}
		}
	}
}

// TestAnthropicMessagesUpstreamErrorRealStatus 上游报错必须在开流之前回真实状态码
// （非流式 502 → Anthropic 错误体；流式：管线在开流前失败同样回真实状态码）。
func TestAnthropicMessagesUpstreamErrorRealStatus(t *testing.T) {
	// 非流式：上游 500 → 管线轮转耗尽 → 503 错误信封 → Anthropic 错误体。
	h := newAnthropicHandler(t, func(authz string) (int, string, bool) {
		return 500, `{"error":{"message":"boom"}}`, false
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"cn:glm-5.2","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if json.Unmarshal(rec.Body.Bytes(), &resp) != nil {
		t.Fatalf("bad json %s", rec.Body)
	}
	if resp["type"] != "error" {
		t.Fatalf("type=%v want error", resp["type"])
	}
	errObj := resp["error"].(map[string]any)
	if !strings.Contains(errObj["message"].(string), "boom") {
		t.Errorf("message=%v want upstream 原文", errObj["message"])
	}

	// 流式：上游 500 → 真实 5xx 状态码（不是 200 裹事件）。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"cn:glm-5.2","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code < 400 {
		t.Fatalf("stream code=%d want >=400（开流前必须定型真实状态码）", rec.Code)
	}
}

// TestAnthropicMessagesMaxTokensRequired max_tokens 缺失 → 400（Anthropic 必填）。
func TestAnthropicMessagesMaxTokensRequired(t *testing.T) {
	h := newAnthropicHandler(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["type"] != "error" {
		t.Errorf("type=%v want error", resp["type"])
	}
}

// TestAnthropicAuthRejects401 鉴权与 /v1/chat/completions 同口径：无 key/错 key
// → 401（两个端点都是）；正确 key 放行。APIKey 由 withAuth 读取，不认 x-api-key
// 头（口径单源：httpauth.VerifyBearer）。
func TestAnthropicAuthRejects401(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "secret"})
	h.mountAnthropic(h.mux) // 主代理接线后 NewHandler 内调用；测试直接挂到内部 mux

	post := func(path, key string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, strings.NewReader(`{"model":"cn:glm-5.2","max_tokens":10,"messages":[]}`))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		if rec := post(path, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: no key code=%d want 401", path, rec.Code)
		}
		if rec := post(path, "wrong"); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: wrong key code=%d want 401", path, rec.Code)
		}
		// x-api-key 不在 withAuth 口径内：仅带它必须 401（口径单源，不引入第二鉴权头）。
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, strings.NewReader(`{"model":"cn:glm-5.2","max_tokens":10,"messages":[]}`))
		req.Header.Set("x-api-key", "secret")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: x-api-key only code=%d want 401（鉴权口径单源 Bearer）", path, rec.Code)
		}
		if rec := post(path, "secret"); rec.Code == http.StatusUnauthorized {
			t.Errorf("%s: correct key should pass", path)
		}
	}
}

// TestAnthropicCountTokensMatchesChatAuth count_tokens 与 /v1/messages 同一条
// withAuth 通道：被停用的 key 无法绕过（同一把 key，行为一致即可）。
func TestAnthropicCountTokensMatchesChatAuth(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "k1"})
	h.mountAnthropic(h.mux)

	// 正确 key：count_tokens 与 messages 都过鉴权层。
	for _, path := range []string{"/v1/messages/count_tokens", "/v1/messages"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer k1")
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("%s: correct key got 401", path)
		}
	}
}
