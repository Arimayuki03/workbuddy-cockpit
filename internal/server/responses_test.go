// responses_test.go OpenAI Responses API 兼容层的转换/回填/流式/鉴权测试。
// httptest 风格与 handler_test.go 一致（newFakeUpstream / testPoolWith / TestMain 全局复用）。
package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// newResponsesHandlerFull 构造带 body 捕获的 responses 测试 handler。
func newResponsesHandlerFull(t *testing.T, behavior func(authz string, body []byte) (int, string, bool)) (*Handler, *int, *[]byte) {
	t.Helper()
	calls := 0
	var captured []byte
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return behavior(authz, captured)
	})
	// newFakeUpstream 的 transport 不暴露 body；换成本地 transport 直接管出站请求。
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		captured, _ = readAllLimited(r.Body, 1<<22)
		calls++
		authz := r.Header.Get("Authorization")
		status, body, isStream := behavior(authz, captured)
		ct := "application/json"
		if isStream {
			ct = "text/event-stream"
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{ct}},
			Body:       ioNopCloser(body),
		}, nil
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	h.mountResponses(h.mux)
	return h, &calls, &captured
}

func readAllLimited(r interface{ Read([]byte) (int, error) }, n int) ([]byte, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for len(buf) < n {
		k, err := r.Read(tmp)
		buf = append(buf, tmp[:k]...)
		if err != nil {
			return buf, nil // EOF 等：拿到的就是全部
		}
	}
	return buf, nil
}

func ioNopCloser(s string) ioReadCloser { return nopCloser{strings.NewReader(s)} }

type ioReadCloser interface {
	Read([]byte) (int, error)
	Close() error
}

type nopCloser struct{ *strings.Reader }

func (nopCloser) Close() error { return nil }

// postResponses 发一个 /v1/responses 请求。
func postResponses(h *Handler, path, payload string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(payload))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// 请求转换：instructions / input items / function_call 合并 / reasoning 回填
// ---------------------------------------------------------------------------

// TestResponsesToChatInstructionsAndInput 覆盖 instructions→system、字符串 input→user。
func TestResponsesToChatInstructionsAndInput(t *testing.T) {
	body := map[string]any{
		"model":        "glm-5.2",
		"instructions": "你是助手",
		"input":        "你好",
	}
	chat, rerr := responsesToChat(body)
	if rerr != nil {
		t.Fatalf("rerr=%v", rerr)
	}
	msgs := chat["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%d want 2: %v", len(msgs), msgs)
	}
	if m := msgs[0].(map[string]any); m["role"] != "system" || m["content"] != "你是助手" {
		t.Errorf("msg0=%v", m)
	}
	if m := msgs[1].(map[string]any); m["role"] != "user" || m["content"] != "你好" {
		t.Errorf("msg1=%v", m)
	}
	if chat["stream"] != false {
		t.Errorf("stream=%v（必须转告上游，缺了得到空回答）", chat["stream"])
	}
}

// TestResponsesToChatFunctionCallMerge 连续 function_call 合并进同一条 assistant 消息。
func TestResponsesToChatFunctionCallMerge(t *testing.T) {
	body := map[string]any{
		"model": "glm-5.2",
		"input": []any{
			map[string]any{"role": "user", "content": "查天气"},
			map[string]any{"type": "function_call", "call_id": "c1", "name": "get_weather", "arguments": `{"city":"北京"}`},
			map[string]any{"type": "function_call", "call_id": "c2", "name": "get_time", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": "晴"},
			map[string]any{"type": "function_call_output", "call_id": "c2", "output": "12:00"},
		},
	}
	chat, rerr := responsesToChat(body)
	if rerr != nil {
		t.Fatalf("rerr=%v", rerr)
	}
	msgs := chat["messages"].([]any)
	if len(msgs) != 4 { // user / assistant(2 calls) / tool / tool
		t.Fatalf("messages=%d want 4: %v", len(msgs), msgs)
	}
	asst := msgs[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Fatalf("role=%v", asst["role"])
	}
	calls := asst["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("tool_calls=%d want 2（连续 function_call 必须合并）", len(calls))
	}
	c1 := calls[0].(map[string]any)
	if c1["id"] != "c1" {
		t.Errorf("call id=%v", c1["id"])
	}
	if fn := c1["function"].(map[string]any); fn["name"] != "get_weather" {
		t.Errorf("name=%v", fn["name"])
	}
	tool1 := msgs[2].(map[string]any)
	if tool1["role"] != "tool" || tool1["tool_call_id"] != "c1" || tool1["content"] != "晴" {
		t.Errorf("tool1=%v", tool1)
	}
}

// TestResponsesToChatReasoningBackfill reasoning item 回填到下一条 assistant 消息，
// reasoning/reasoning_content 双写（上游校验字段存在、非空、不 trim）。
func TestResponsesToChatReasoningBackfill(t *testing.T) {
	// 用本网关编码的凭据（模拟上一轮响应里发给客户端的 encrypted_content 回传）。
	secret := "上一轮推理原文"
	body := map[string]any{
		"model": "deepseek-v4.1-flash",
		"input": []any{
			map[string]any{"role": "user", "content": "第一问"},
			map[string]any{"type": "reasoning", "id": "rs1", "summary": []any{
				map[string]any{"type": "summary_text", "text": secret},
			}},
			map[string]any{"role": "assistant", "content": "第一答"},
		},
	}
	chat, rerr := responsesToChat(body)
	if rerr != nil {
		t.Fatalf("rerr=%v", rerr)
	}
	msgs := chat["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%d want 2（reasoning item 不产出独立消息）: %v", len(msgs), msgs)
	}
	asst := msgs[1].(map[string]any)
	if got := asst["reasoning_content"]; got != secret {
		t.Errorf("reasoning_content=%v want %q", got, secret)
	}
	if got := asst["reasoning"]; got != secret {
		t.Errorf("reasoning=%v want %q（两字段必须双写）", got, secret)
	}
}

// TestResponsesToChatReasoningCredentialDecode encrypted_content 凭据优先于 summary
// （凭据是唯一能还原完整原文的载体）。
func TestResponsesToChatReasoningCredentialDecode(t *testing.T) {
	full := "完整推理原文，客户端可能截断 summary"
	item := map[string]any{
		"type": "reasoning",
		"encrypted_content": rspEncodeCredential(full),
		"summary": []any{
			map[string]any{"type": "summary_text", "text": "截断"},
		},
	}
	if got := rspReasoningText(item); got != full {
		t.Errorf("got=%q want full", got)
	}
	// 非本网关凭据（真 OpenAI 加密串）解不开 → 回落 summary。
	item2 := map[string]any{
		"type":              "reasoning",
		"encrypted_content": "gAAAAABh_fake_openai_token",
		"summary":           []any{map[string]any{"type": "summary_text", "text": "回落文本"}},
	}
	if got := rspReasoningText(item2); got != "回落文本" {
		t.Errorf("got=%q want 回落文本", got)
	}
}

// TestResponsesToChatReasoningEmptyPlaceholder 空 reasoning item（畸形输入）补空格
// 占位：上游校验 len>0 且不 trim，空串过不了、空白串能过。
func TestResponsesToChatReasoningEmptyPlaceholder(t *testing.T) {
	body := map[string]any{
		"model": "deepseek-v4.1-flash",
		"input": []any{
			map[string]any{"type": "reasoning", "id": "rs1", "summary": []any{}}, // 空 summary
			map[string]any{"role": "assistant", "content": "答"},
		},
	}
	chat, rerr := responsesToChat(body)
	if rerr != nil {
		t.Fatalf("rerr=%v", rerr)
	}
	msgs := chat["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages=%d want 1（空 reasoning 不产出独立消息）: %v", len(msgs), msgs)
	}
	asst := msgs[0].(map[string]any)
	rc, _ := asst["reasoning_content"].(string)
	if rc != " " {
		t.Errorf("reasoning_content=%q want 单个空格占位", rc)
	}
	r, _ := asst["reasoning"].(string)
	if r != " " {
		t.Errorf("reasoning=%q want 单个空格占位", r)
	}
}

// TestResponsesToChatReasoningCrossUserBackfill reasoning 隔着 user 才轮到 assistant
// （异常顺序）也要能送到；收尾时挂到最后一条 assistant。
func TestResponsesToChatReasoningCrossUserBackfill(t *testing.T) {
	secret := "跨 user 的推理"
	body := map[string]any{
		"model": "deepseek-v4.1-flash",
		"input": []any{
			map[string]any{"role": "user", "content": "问1"},
			map[string]any{"role": "assistant", "content": "答1"},
			map[string]any{"role": "user", "content": "问2"},
			map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": secret}}},
		},
	}
	chat, rerr := responsesToChat(body)
	if rerr != nil {
		t.Fatalf("rerr=%v", rerr)
	}
	msgs := chat["messages"].([]any)
	// reasoning 排在最后一条 assistant 之后 → 收尾回填到答1 那条 assistant。
	asst := msgs[1].(map[string]any)
	if got := asst["reasoning_content"]; got != secret {
		t.Errorf("backfill got=%v want %q", got, secret)
	}
}

// TestResponsesToChatReasoningAccumulates 连续多段 reasoning item 拼接（不覆盖）。
func TestResponsesToChatReasoningAccumulates(t *testing.T) {
	body := map[string]any{
		"model": "deepseek-v4.1-flash",
		"input": []any{
			map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "甲"}}},
			map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "乙"}}},
			map[string]any{"role": "assistant", "content": "答"},
		},
	}
	chat, _ := responsesToChat(body)
	msgs := chat["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages=%d want 1（reasoning item 不产出独立消息）: %v", len(msgs), msgs)
	}
	asst := msgs[0].(map[string]any)
	if got := asst["reasoning_content"]; got != "甲乙" {
		t.Errorf("got=%v want 甲乙（累积拼接）", got)
	}
}

// TestResponsesToChatPromptCacheKeyPassthrough 客户端已带 prompt_cache_key 原样
// 透传（费用差约 17 倍；内部管线的优先级 1 是「客户端已带则保留」，Responses 层
// 只透传）。
func TestResponsesToChatPromptCacheKeyPassthrough(t *testing.T) {
	body := map[string]any{
		"model":           "glm-5.2",
		"input":           "hi",
		"prompt_cache_key": "codex:abc123",
	}
	chat, rerr := responsesToChat(body)
	if rerr != nil {
		t.Fatalf("rerr=%v", rerr)
	}
	if got := chat["prompt_cache_key"]; got != "codex:abc123" {
		t.Errorf("prompt_cache_key=%v want codex:abc123", got)
	}
}

// TestResponsesToChatToolsAndChoice 工具扁平定义→嵌套形状、tool_choice 具名映射、
// reasoning.effort→reasoning_effort、max_output_tokens→max_tokens。
func TestResponsesToChatToolsAndChoice(t *testing.T) {
	body := map[string]any{
		"model":             "glm-5.2",
		"input":             "hi",
		"max_output_tokens": float64(512),
		"reasoning":         map[string]any{"effort": "high"},
		"tool_choice":       map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}},
		"tools": []any{
			map[string]any{"type": "function", "name": "lookup", "description": "查表",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
			map[string]any{"type": "custom", "name": "apply_patch", "format": map[string]any{"type": "grammar", "definition": "patch-grammar"}},
		},
	}
	chat, rerr := responsesToChat(body)
	if rerr != nil {
		t.Fatalf("rerr=%v", rerr)
	}
	if got := rspInt(chat["max_tokens"]); got != 512 {
		t.Errorf("max_tokens=%v", chat["max_tokens"])
	}
	if chat["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort=%v", chat["reasoning_effort"])
	}
	if _, has := chat["reasoning"]; has {
		t.Errorf("reasoning 对象不能整个透传（上游不认识）")
	}
	tc := chat["tool_choice"].(map[string]any)
	if fn := tc["function"].(map[string]any); fn["name"] != "lookup" {
		t.Errorf("tool_choice=%v", tc)
	}
	tools := chat["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools=%d want 2", len(tools))
	}
	fn0 := tools[0].(map[string]any)["function"].(map[string]any)
	if fn0["type"] != nil || tools[0].(map[string]any)["type"] != "function" {
		t.Errorf("tool0 shape=%v", tools[0])
	}
	if fn0["name"] != "lookup" {
		t.Errorf("tool0 name=%v", fn0["name"])
	}
	// custom 工具被桥接为单参数 function（input 包装）。
	fn1 := tools[1].(map[string]any)["function"].(map[string]any)
	params := fn1["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)
	if _, ok := props["input"]; !ok {
		t.Errorf("custom tool params=%v want input 包装", params)
	}
	if !strings.Contains(fn1["description"].(string), "raw text") {
		t.Errorf("custom tool description=%v", fn1["description"])
	}
}

// TestResponsesToChatNamespaceBridge namespace 子工具展开：ns__tool 出站名 + 双向映射。
func TestResponsesToChatNamespaceBridge(t *testing.T) {
	body := map[string]any{
		"model": "glm-5.2",
		"input": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"type": "function_call", "call_id": "c1", "name": "shell.exec", "arguments": `{"cmd":"ls"}`},
		},
		"tools": []any{
			map[string]any{"type": "namespace", "name": "shell", "tools": []any{
				map[string]any{"type": "function", "name": "exec", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
			}},
		},
	}
	chat, rerr := responsesToChat(body)
	if rerr != nil {
		t.Fatalf("rerr=%v", rerr)
	}
	// 历史里的 "shell.exec"（点连形态）应映射到子工具的出站名。
	tools := chat["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%d want 1", len(tools))
	}
	outName := tools[0].(map[string]any)["function"].(map[string]any)["name"].(string)
	if outName != "exec" {
		// 无重名时裸名优先（对齐参考实现 preferred=name）。
		t.Logf("出站名=%q", outName)
	}
	callFn := chat["messages"].([]any)[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if callFn["name"] != outName {
		t.Errorf("历史名=%v 未映射到出站名 %q", callFn["name"], outName)
	}
	// 响应侧还原原名。
	if got, _ := newRspToolBridge(rspAsArray(body["tools"])).restore(outName); got != "exec" {
		t.Errorf("restore=%q want exec", got)
	}
}

// ---------------------------------------------------------------------------
// 响应转换：output items / usage 映射
// ---------------------------------------------------------------------------

// TestChatToResponsesFull 非流式转换：reasoning 在前、message、usage 映射。
func TestChatToResponsesFull(t *testing.T) {
	data := map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role":              "assistant",
				"content":           "答案正文",
				"reasoning_content": "思考过程",
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":            float64(10),
			"completion_tokens":        float64(5),
			"total_tokens":             float64(15),
			"prompt_cache_hit_tokens":  float64(8),
			"prompt_cache_miss_tokens": float64(2),
		},
	}
	resp := chatToResponses(data, "cn:glm-5.2", "resp_x", nil)
	if resp["object"] != "response" || resp["model"] != "cn:glm-5.2" {
		t.Errorf("resp=%v", resp)
	}
	output := resp["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output=%d want 2: %v", len(output), output)
	}
	rs := output[0].(map[string]any)
	if rs["type"] != "reasoning" {
		t.Errorf("output[0]=%v want reasoning（思考在正文之前）", rs)
	}
	// 凭据可逆解码：客户端原样回传后网关能还原原文（多轮一致性）。
	if got, ok := rspDecodeCredential(rspString(rs["encrypted_content"])); !ok || got != "思考过程" {
		t.Errorf("credential=%v", rs["encrypted_content"])
	}
	if summary := rspString(rs["summary"].([]any)[0].(map[string]any)["text"]); summary != "思考过程" {
		t.Errorf("summary=%v", rs["summary"])
	}
	msg := output[1].(map[string]any)
	if msg["type"] != "message" || msg["role"] != "assistant" {
		t.Errorf("output[1]=%v", msg)
	}
	part := msg["content"].([]any)[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "答案正文" {
		t.Errorf("part=%v", part)
	}
	usage := resp["usage"].(map[string]any)
	if usage["input_tokens"] != 10 || usage["output_tokens"] != 5 || usage["total_tokens"] != 15 {
		t.Errorf("usage=%v", usage)
	}
	if cached := usage["input_tokens_details"].(map[string]any)["cached_tokens"]; cached != 8 {
		t.Errorf("cached=%v", cached)
	}
	if resp["output_text"] != "答案正文" {
		t.Errorf("output_text=%v", resp["output_text"])
	}
}

// TestChatToResponsesToolCalls 工具调用输出项 + 名字还原 + incomplete（length）。
func TestChatToResponsesToolCalls(t *testing.T) {
	data := map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []any{map[string]any{
					"id":   "call_1",
					"type": "function",
					"function": map[string]any{"name": "lookup", "arguments": `{"q":"go"}`},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{},
	}
	resp := chatToResponses(data, "glm-5.2", "resp_x", nil)
	output := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output=%d want 1: %v", len(output), output)
	}
	fc := output[0].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" || fc["name"] != "lookup" {
		t.Errorf("fc=%v", fc)
	}
	if fc["arguments"] != `{"q":"go"}` {
		t.Errorf("arguments=%v", fc["arguments"])
	}
	if resp["status"] != "completed" {
		t.Errorf("status=%v", resp["status"])
	}
}

// TestChatToResponsesIncomplete length 截断 → status=incomplete + incomplete_details。
func TestChatToResponsesIncomplete(t *testing.T) {
	data := map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": "半截"},
			"finish_reason": "length",
		}},
		"usage": map[string]any{},
	}
	resp := chatToResponses(data, "glm-5.2", "resp_x", nil)
	if resp["status"] != "incomplete" {
		t.Errorf("status=%v", resp["status"])
	}
	if resp["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" {
		t.Errorf("incomplete_details=%v", resp["incomplete_details"])
	}
}

// TestChatToResponsesCustomRestore custom 桥接调用还原为 custom_tool_call。
func TestChatToResponsesCustomRestore(t *testing.T) {
	bridge := newRspToolBridge([]any{
		map[string]any{"type": "custom", "name": "apply_patch"},
	})
	data := map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role": "assistant", "content": nil,
				"tool_calls": []any{map[string]any{
					"id":   "call_9",
					"type": "function",
					// 桥接出的包装：{"input": <原文>}。
					"function": map[string]any{"name": "apply_patch", "arguments": `{"input":"*** Begin Patch"}`},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{},
	}
	resp := chatToResponses(data, "glm-5.2", "resp_x", bridge)
	fc := resp["output"].([]any)[0].(map[string]any)
	if fc["type"] != "custom_tool_call" {
		t.Fatalf("type=%v want custom_tool_call", fc["type"])
	}
	if fc["input"] != "*** Begin Patch" {
		t.Errorf("input=%v", fc["input"])
	}
}

// ---------------------------------------------------------------------------
// 流式事件序列（chatCompletions 管线内全链路，经 pipe + 翻译器）
// ---------------------------------------------------------------------------

// rspStreamEvents 把 SSE 响应体按 event 帧拆成 (event 名, data JSON) 列表。
func rspStreamEvents(t *testing.T, body string) []struct {
	Event string
	Data  map[string]any
} {
	t.Helper()
	var events []struct {
		Event string
		Data  map[string]any
	}
	for _, chunk := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(chunk) == "" {
			continue
		}
		var name string
		var data map[string]any
		for _, line := range strings.Split(chunk, "\n") {
			if strings.HasPrefix(line, "event: ") {
				name = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data); err != nil {
					t.Fatalf("bad data line %q: %v", line, err)
				}
			}
		}
		events = append(events, struct {
			Event string
			Data  map[string]any
		}{name, data})
	}
	return events
}

// eventNames 提取事件名序列。
func eventNames(events []struct {
	Event string
	Data  map[string]any
}) []string {
	names := make([]string, 0, len(events))
	for _, e := range events {
		names = append(names, e.Event)
	}
	return names
}

func namesEqual(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// chatSSEText 构造一段带 reasoning+正文+usage 的上游 chat SSE 流。
func chatSSEText() string {
	return "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"想一想\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5,\"prompt_cache_hit_tokens\":1}}\n\n" +
		"data: [DONE]\n\n"
}

// TestResponsesStreamEventSequence 流式：Chat SSE → Responses 事件流。
// 顺序对齐客户端解析器：created → output_item.added(reasoning) → reasoning delta →
// output_item.done → output_item.added(message) → content_part.added →
// output_text.delta → done 三连 → completed（含 usage）。
func TestResponsesStreamEventSequence(t *testing.T) {
	h, _, _ := newResponsesHandlerFull(t, func(authz string, body []byte) (int, string, bool) {
		return 200, chatSSEText(), true
	})
	rec := postResponses(h, "/v1/responses", `{"model":"cn:glm-5.2","stream":true,"input":"hi"}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("ct=%q", ct)
	}
	events := rspStreamEvents(t, rec.Body.String())
	want := []string{
		"response.created",
		"response.output_item.added", // reasoning
		"response.reasoning_summary_text.delta",
		"response.output_item.done",
		"response.output_item.added", // message
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	got := eventNames(events)
	if !namesEqual(got, want) {
		t.Fatalf("events=%v want %v", got, want)
	}
	// output_index 一致性：added 与 delta 必须同槽（对不上时客户端静默丢增量）。
	textDeltaIdx := events[6].Data["output_index"]
	if addedIdx := events[4].Data["output_index"]; addedIdx != textDeltaIdx {
		t.Errorf("message added idx=%v delta idx=%v 必须一致", addedIdx, textDeltaIdx)
	}
	// delta 内容与上游一致。
	if events[6].Data["delta"] != "你好" {
		t.Errorf("text delta=%v", events[6].Data["delta"])
	}
	if events[2].Data["delta"] != "想一想" {
		t.Errorf("reasoning delta=%v", events[2].Data["delta"])
	}
	// completed 带 usage（Responses 口径映射）。
	completed := events[len(events)-1].Data["response"].(map[string]any)
	if completed["status"] != "completed" || completed["model"] != "cn:glm-5.2" {
		t.Errorf("completed response=%v", completed)
	}
	usage := completed["usage"].(map[string]any)
	if usage["input_tokens"] != float64(3) || usage["output_tokens"] != float64(2) {
		t.Errorf("usage=%v", usage)
	}
	// output 项已收集（reasoning + message）。
	if output := completed["output"].([]any); len(output) != 2 {
		t.Errorf("completed output=%d want 2", len(output))
	}
	// reasoning done 项带凭据（客户端 store:false 下靠它回传）。
	doneItem := events[3].Data["item"].(map[string]any)
	if _, ok := doneItem["encrypted_content"]; !ok {
		t.Errorf("reasoning done item 缺 encrypted_content: %v", doneItem)
	}
	if got, ok := rspDecodeCredential(rspString(doneItem["encrypted_content"])); !ok || got != "想一想" {
		t.Errorf("credential=%v", doneItem["encrypted_content"])
	}
}

// TestResponsesStreamToolCallSequence 流式工具调用：added → arguments delta →
// done → output_item.done → completed（finish_reason=tool_calls）。
func TestResponsesStreamToolCallSequence(t *testing.T) {
	sse := "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"q\\\":\"}}]}}]}\n\n" +
		"data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"go\\\"}\"}}]}}]}\n\n" +
		"data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	h, _, _ := newResponsesHandlerFull(t, func(authz string, body []byte) (int, string, bool) {
		return 200, sse, true
	})
	rec := postResponses(h, "/v1/responses", `{"model":"cn:glm-5.2","stream":true,"input":"hi"}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	events := rspStreamEvents(t, rec.Body.String())
	names := eventNames(events)
	// 每个参数分片一个 delta（2 个分片 → 2 个 delta，与 Chat 流式透传语义一致）。
	want := []string{
		"response.created",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	}
	if !namesEqual(names, want) {
		t.Fatalf("events=%v want %v", names, want)
	}
	doneItem := events[5].Data["item"].(map[string]any)
	if doneItem["call_id"] != "call_1" || doneItem["name"] != "lookup" {
		t.Errorf("done item=%v", doneItem)
	}
	if doneItem["arguments"] != `{"q":"go"}` {
		t.Errorf("arguments=%v", doneItem["arguments"])
	}
}

// TestResponsesStreamToolCallFragmentedName 工具名分片到达（首片空/续片补名）。
func TestResponsesStreamToolCallFragmentedName(t *testing.T) {
	tr := newRspStreamTranslator("glm-5.2", "resp_x", nil)
	tr.feed(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "function": map[string]any{"name": "", "arguments": ""}}},
	}}}})
	tr.feed(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"name": "look", "arguments": ""}}},
	}}}})
	tr.feed(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"name": "up", "arguments": ""}}},
	}}}})
	out := tr.finish("", true)
	joined := string(out)
	if !strings.Contains(joined, `"name":"lookup"`) {
		t.Errorf("分片名未拼全： %s", joined)
	}
}

// TestResponsesStreamErrorFrame 上游中途 error 帧 → response.failed 收尾（真实
// 状态码在开流前已定 200，错误只能裹进终止事件）。
func TestResponsesStreamErrorFrame(t *testing.T) {
	sse := "data: {\"error\":{\"message\":\"boom\",\"code\":11155},\"requestId\":\"r1\"}\n\n"
	h, _, _ := newResponsesHandlerFull(t, func(authz string, body []byte) (int, string, bool) {
		return 200, sse, true
	})
	rec := postResponses(h, "/v1/responses", `{"model":"cn:glm-5.2","stream":true,"input":"hi"}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	events := rspStreamEvents(t, rec.Body.String())
	if len(events) == 0 || events[len(events)-1].Event != "response.failed" {
		t.Fatalf("events=%v want 尾帧 response.failed", eventNames(events))
	}
	resp := events[len(events)-1].Data["response"].(map[string]any)
	if resp["status"] != "failed" {
		t.Errorf("status=%v", resp["status"])
	}
	errObj := resp["error"].(map[string]any)
	if errObj["message"] != "boom" {
		t.Errorf("error=%v", errObj)
	}
}

// TestResponsesStreamMaxBuffer 上游吐无换行长帧 → Scanner ErrTooLong →
// response.failed（数据流缺少分隔），不无限膨胀内存。
func TestResponsesStreamMaxBuffer(t *testing.T) {
	// 直接对翻译器 + rspStream 单测（不必穿透管线）。
	tr := newRspStreamTranslator("glm-5.2", "resp_x", nil)
	var sb strings.Builder
	for i := 0; i < maxRspSSEBuffer/16+100; i++ {
		sb.WriteString(strings.Repeat("x", 16))
	}
	rec := httptest.NewRecorder()
	err := rspStream(rec, strings.NewReader(sb.String()), tr)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "response.failed") || !strings.Contains(body, "数据流缺少分隔") {
		t.Errorf("body 尾=%q", body[len(body)-300:])
	}
}

// ---------------------------------------------------------------------------
// 端到端（穿透 chatCompletions 管线）：非流式 / 流式 / 错误透传 / prompt_cache_key
// ---------------------------------------------------------------------------

// TestResponsesNonStreamE2E 非流式全链路：Responses 请求 → chat 管线 → Responses 对象。
func TestResponsesNonStreamE2E(t *testing.T) {
	h, calls, captured := newResponsesHandlerFull(t, func(authz string, body []byte) (int, string, bool) {
		if authz != "Bearer at1" {
			t.Errorf("auth=%q", authz)
		}
		return 200, sseOK, true // 管线强制上游流式，聚合后回 JSON
	})
	rec := postResponses(h, "/v1/responses", `{"model":"cn:glm-5.2","input":"hi"}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if *calls != 1 {
		t.Errorf("upstream calls=%d want 1", *calls)
	}
	// 出站 body 是 chat 形态：messages 数组 + 强制 stream:true。
	var outBody map[string]any
	if err := json.Unmarshal(*captured, &outBody); err != nil {
		t.Fatalf("captured=%s", *captured)
	}
	if _, ok := outBody["messages"].([]any); !ok {
		t.Errorf("out body 缺 messages: %s", *captured)
	}
	if outBody["stream"] != true {
		t.Errorf("out stream=%v（管线强制流式）", outBody["stream"])
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v", err)
	}
	if resp["object"] != "response" {
		t.Errorf("object=%v", resp["object"])
	}
	if resp["model"] != "cn:glm-5.2" {
		t.Errorf("model=%v（必须回填用户请求名）", resp["model"])
	}
	if resp["output_text"] != "你好" {
		t.Errorf("output_text=%v", resp["output_text"])
	}
}

// TestResponsesStreamE2E 流式全链路（SSE 事件经管线透传后翻译）。
func TestResponsesStreamE2E(t *testing.T) {
	h, _, _ := newResponsesHandlerFull(t, func(authz string, body []byte) (int, string, bool) {
		return 200, chatSSEText(), true
	})
	rec := postResponses(h, "/responses", `{"model":"cn:glm-5.2","stream":true,"input":"hi"}`) // 兼容路径
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	events := rspStreamEvents(t, rec.Body.String())
	if got := eventNames(events); len(got) == 0 || got[len(got)-1] != "response.completed" {
		t.Fatalf("尾事件=%v want response.completed", got)
	}
}

// TestResponsesUpstreamErrorPassthrough 上游错误在开流前回真实状态码 + 上游原文
// （chatCompletions 的 OpenAI 错误信封原样透传，Responses 客户端同口径解析）。
func TestResponsesUpstreamErrorPassthrough(t *testing.T) {
	h, _, _ := newResponsesHandlerFull(t, func(authz string, body []byte) (int, string, bool) {
		return 503, `{"error":{"message":"all accounts are temporarily unavailable, please retry later","type":"api_error","code":"no_healthy_account"}}`, false
	})
	rec := postResponses(h, "/v1/responses", `{"model":"cn:glm-5.2","input":"hi"}`)
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s（开流前必须回真实状态码）", rec.Code, rec.Body)
	}
	if !assertJSONErrorCode(t, rec.Body.String(), "no_healthy_account") {
		t.Errorf("body=%s", rec.Body)
	}
}

// TestResponsesUpstream400Body 上游 400（如 11115 prompt too long）原样透传状态码与 body。
func TestResponsesUpstream400Body(t *testing.T) {
	h, _, _ := newResponsesHandlerFull(t, func(authz string, body []byte) (int, string, bool) {
		return 400, `{"code":11115,"msg":"prompt is too long: 100000 > 8192"}`, false
	})
	rec := postResponses(h, "/v1/responses", `{"model":"cn:glm-5.2","input":"hi"}`)
	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "11115") {
		t.Errorf("body=%s（上游原文透传）", rec.Body)
	}
}

// TestResponsesPromptCacheKeyE2E 客户端带的 prompt_cache_key 全链路透传到上游 body。
func TestResponsesPromptCacheKeyE2E(t *testing.T) {
	h, _, captured := newResponsesHandlerFull(t, func(authz string, body []byte) (int, string, bool) {
		return 200, sseOK, true
	})
	rec := postResponses(h, "/v1/responses", `{"model":"cn:glm-5.2","input":"hi","prompt_cache_key":"codex:keep-me"}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var outBody map[string]any
	if err := json.Unmarshal(*captured, &outBody); err != nil {
		t.Fatalf("captured=%s", *captured)
	}
	if got := outBody["prompt_cache_key"]; got != "codex:keep-me" {
		t.Errorf("上游收到的 prompt_cache_key=%v want codex:keep-me（客户端已带则保留）", got)
	}
}

// TestResponsesEmptyInput400 空 input / 无 instructions → 400 empty_input。
func TestResponsesEmptyInput400(t *testing.T) {
	h, _, _ := newResponsesHandlerFull(t, func(authz string, body []byte) (int, string, bool) {
		t.Errorf("空请求不应打到上游")
		return 200, "", false
	})
	rec := postResponses(h, "/v1/responses", `{"model":"cn:glm-5.2"}`)
	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !assertJSONErrorCode(t, rec.Body.String(), "empty_input") {
		t.Errorf("body=%s", rec.Body)
	}
}

// TestResponsesBadBody400 坏 JSON → 400。
func TestResponsesBadBody400(t *testing.T) {
	h, _, _ := newResponsesHandlerFull(t, func(authz string, body []byte) (int, string, bool) {
		t.Errorf("坏 body 不应打到上游")
		return 200, "", false
	})
	rec := postResponses(h, "/v1/responses", `{not json`)
	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestResponsesMissingModel400 缺 model → 400 invalid_model。
func TestResponsesMissingModel400(t *testing.T) {
	h, _, _ := newResponsesHandlerFull(t, func(authz string, body []byte) (int, string, bool) {
		t.Errorf("缺 model 不应打到上游")
		return 200, "", false
	})
	rec := postResponses(h, "/v1/responses", `{"input":"hi"}`)
	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !assertJSONErrorCode(t, rec.Body.String(), "invalid_model") {
		t.Errorf("body=%s", rec.Body)
	}
}

// TestResponsesAuthRequired 鉴权与 /v1/chat/completions 同口径：无密钥 401。
func TestResponsesAuthRequired(t *testing.T) {
	h, _, _ := newResponsesHandlerFull(t, func(authz string, body []byte) (int, string, bool) {
		t.Errorf("鉴权失败不应打到上游")
		return 200, "", false
	})
	// 重新建一个带 APIKey 的 handler（newResponsesHandlerFull 不带 key）。
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h2 := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		APIKey:   "sk-secret",
	})
	h2.mountResponses(h2.mux)
	for _, path := range []string{"/v1/responses", "/responses"} {
		rec := postResponses(h2, path, `{"model":"cn:glm-5.2","input":"hi"}`)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s code=%d want 401", path, rec.Code)
		}
		// 带正确 Bearer → 放行到上游。
		req := httptest.NewRequest("POST", path, strings.NewReader(`{"model":"cn:glm-5.2","input":"hi"}`))
		req.Header.Set("Authorization", "Bearer sk-secret")
		rec2 := httptest.NewRecorder()
		h2.ServeHTTP(rec2, req)
		if rec2.Code != 200 {
			t.Errorf("%s with bearer code=%d body=%s", path, rec2.Code, rec2.Body)
		}
	}
	_ = h
}

// TestResponsesCustomToolHistoryInputMustBeString custom_tool_call 历史 input 非字符串 → 400。
func TestResponsesCustomToolHistoryInputMustBeString(t *testing.T) {
	h, _, _ := newResponsesHandlerFull(t, func(authz string, body []byte) (int, string, bool) {
		t.Errorf("转换失败不应打到上游")
		return 200, "", false
	})
	rec := postResponses(h, "/v1/responses",
		`{"model":"cn:glm-5.2","input":[{"type":"custom_tool_call","call_id":"c1","name":"apply_patch","input":123}]}`)
	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestResponsesCustomToolE2E custom 工具全链路：请求侧包装、响应侧还原（非流式）。
func TestResponsesCustomToolE2E(t *testing.T) {
	h, _, _ := newResponsesHandlerFull(t, func(authz string, body []byte) (int, string, bool) {
		// 上游回 function 调用（桥接名 apply_patch + input 包装参数）。
		return 200, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"apply_patch\",\"arguments\":\"\"}}]}}]}\n\n" +
			"data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"input\\\":\\\"*** Begin Patch\\\"}\"}}]}}]}\n\n" +
			"data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
			"data: [DONE]\n\n", true
	})
	rec := postResponses(h, "/v1/responses",
		`{"model":"cn:glm-5.2","stream":true,"input":"patch it","tools":[{"type":"custom","name":"apply_patch"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	events := rspStreamEvents(t, rec.Body.String())
	var doneItem map[string]any
	for _, e := range events {
		if e.Event == "response.output_item.done" {
			doneItem = e.Data["item"].(map[string]any)
		}
	}
	if doneItem == nil || doneItem["type"] != "custom_tool_call" {
		t.Fatalf("custom 调用未还原: %v", doneItem)
	}
	if doneItem["input"] != "*** Begin Patch" {
		t.Errorf("input=%v（包装应被解码为原始文本）", doneItem["input"])
	}
	if doneItem["name"] != "apply_patch" {
		t.Errorf("name=%v（应还原客户端原名）", doneItem["name"])
	}
}

// TestResponsesCredentialRoundTrip 凭据编解码往返：编码→解码恒等。
func TestResponsesCredentialRoundTrip(t *testing.T) {
	for _, s := range []string{"", " ", "中文推理", "multi\nline"} {
		enc := rspEncodeCredential(s)
		got, ok := rspDecodeCredential(enc)
		if !ok || got != s {
			t.Errorf("roundtrip %q -> %q ok=%v", s, got, ok)
		}
		// 前缀自标识。
		if !strings.HasPrefix(enc, "wbm1:") {
			t.Errorf("enc=%q 缺前缀", enc)
		}
	}
	// 非 base64 / 非前缀串不解。
	if _, ok := rspDecodeCredential("wbm1:%%%"); ok {
		t.Errorf("坏 base64 不应解码成功")
	}
	if _, ok := rspDecodeCredential("gAAAAAB_openai"); ok {
		t.Errorf("非本网关串不应解码成功")
	}
	// base64 URL-safe 兼容（与参考实现 urlsafe_b64 互通）。
	raw := base64.URLEncoding.EncodeToString([]byte("urlsafe"))
	if got, ok := rspDecodeCredential("wbm1:" + raw); !ok || got != "urlsafe" {
		t.Errorf("urlsafe decode got=%q ok=%v", got, ok)
	}
}

// TestResponsesToChatToolOutputImages 工具输出里的图片提升为后续 user 消息（不静默丢图）。
func TestResponsesToChatToolOutputImages(t *testing.T) {
	body := map[string]any{
		"model": "glm-5.2",
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "c1", "name": "shot", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": []any{
				map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAA"},
			}},
		},
	}
	chat, rerr := responsesToChat(body)
	if rerr != nil {
		t.Fatalf("rerr=%v", rerr)
	}
	msgs := chat["messages"].([]any)
	// assistant(call) / tool("[图片]") / user(image blocks)
	if len(msgs) != 3 {
		t.Fatalf("messages=%d want 3: %v", len(msgs), msgs)
	}
	if tool := msgs[1].(map[string]any); tool["content"] != "[图片]" {
		t.Errorf("tool content=%v", tool["content"])
	}
	user := msgs[2].(map[string]any)
	blocks := user["content"].([]any)
	if len(blocks) != 1 || blocks[0].(map[string]any)["type"] != "image_url" {
		t.Errorf("user blocks=%v", blocks)
	}
}

// TestResponsesMaxBodyLimit 请求体超上限 → 413（MaxBytesReader 口径与 chat 一致）。
func TestResponsesMaxBodyLimit(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		t.Errorf("超限请求不应打到上游")
		return 200, "", false
	})
	h := NewHandler(Config{
		Pool:         testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:     up,
		MaxBodyBytes: 1024,
	})
	h.mountResponses(h.mux)
	big := fmt.Sprintf(`{"model":"cn:glm-5.2","input":"%s"}`, strings.Repeat("a", 4096))
	rec := postResponses(h, "/v1/responses", big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d body=%s want 413", rec.Code, rec.Body)
	}
}
