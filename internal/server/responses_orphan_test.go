package server

import (
	"encoding/json"
	"strings"
	"testing"

	"workbuddy2api/internal/upstream"
)

// manager 9bf9775 吸收：Codex 子会话只下发 function_call_output、没有对应
// function_call。原样写 role=tool 会被上游 11148 拒绝整段历史；必须降级为 user
// 消息保留内容。
func TestResponsesOrphanFunctionCallOutputDegradesToUser(t *testing.T) {
	body := map[string]any{
		"model": "glm-5.2",
		"input": []any{
			map[string]any{"role": "user", "content": "delegate"},
			map[string]any{"type": "function_call_output", "name": "spawn_agent", "output": "child result"},
		},
	}
	chat, rerr := responsesToChat(body)
	if rerr != nil {
		t.Fatalf("rerr=%v", rerr)
	}
	msgs := chat["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%d want 2: %v", len(msgs), msgs)
	}
	m := msgs[1].(map[string]any)
	if m["role"] != "user" {
		t.Fatalf("orphan output must degrade to user, got %v", m)
	}
	content, _ := m["content"].(string)
	if !strings.Contains(content, "[tool output: spawn_agent]") || !strings.Contains(content, "child result") {
		t.Fatalf("content lost: %q", content)
	}
}

// Responses 规范里 call_id 必填：带 call_id 但历史里没有对应 function_call 的
// 孤儿输出（Codex 子会话）同样必须降级 user 并保留内容——原样写 role=tool 会被
// 上游 cleanupOrphanToolCalls 整条删除，内容静默丢失。
func TestResponsesOrphanFunctionCallOutputWithCallIDKeepsContent(t *testing.T) {
	body := map[string]any{
		"model": "glm-5.2",
		"input": []any{
			map[string]any{"role": "user", "content": "delegate"},
			map[string]any{"type": "function_call_output", "call_id": "call_missing", "name": "spawn_agent", "output": "child result"},
		},
	}
	chat, rerr := responsesToChat(body)
	if rerr != nil {
		t.Fatalf("rerr=%v", rerr)
	}
	msgs := chat["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%d want 2: %v", len(msgs), msgs)
	}
	m := msgs[1].(map[string]any)
	if m["role"] != "user" {
		t.Fatalf("orphan output with call_id must degrade to user, got %v", m)
	}
	content, _ := m["content"].(string)
	if !strings.Contains(content, "[tool output: spawn_agent]") || !strings.Contains(content, "child result") {
		t.Fatalf("content lost: %q", content)
	}

	// 出站管线（cleanupOrphanToolCalls）：降级后的 user 消息必须原样存活。
	// 若仍写 role=tool，tool_call_id 无对应 tool_call，这一步会删掉整条消息。
	raw, err := json.Marshal(chat)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var prepared map[string]any
	if err := json.Unmarshal(upstream.PrepareBodyOpt(raw, false), &prepared); err != nil {
		t.Fatalf("unmarshal prepared body: %v", err)
	}
	pmsgs, ok := prepared["messages"].([]any)
	if !ok || len(pmsgs) != 2 {
		t.Fatalf("prepared messages=%v（孤儿降级消息不得被清理）", prepared["messages"])
	}
	pm := pmsgs[1].(map[string]any)
	pc, _ := pm["content"].(string)
	if pm["role"] != "user" || !strings.Contains(pc, "[tool output: spawn_agent]") || !strings.Contains(pc, "child result") {
		t.Fatalf("content lost after PrepareBodyOpt: %v", pm)
	}
}

// 旧回合未消费的 function_call 不得跨回合配对：中间隔着消息（回合边界）后，
// 后续无 call_id 的输出必须按孤儿降级 user，而不能配给旧调用。
func TestResponsesStaleOpenCallNotMatchedAcrossTurns(t *testing.T) {
	body := map[string]any{
		"model": "glm-5.2",
		"input": []any{
			map[string]any{"role": "user", "content": "第一轮"},
			map[string]any{"type": "function_call", "call_id": "old", "name": "get_weather", "arguments": "{}"},
			map[string]any{"role": "assistant", "content": "工具没执行成功，继续"},
			map[string]any{"role": "user", "content": "第二轮"},
			map[string]any{"type": "function_call_output", "name": "get_weather", "output": "旧回合结果"},
		},
	}
	chat, rerr := responsesToChat(body)
	if rerr != nil {
		t.Fatalf("rerr=%v", rerr)
	}
	msgs := chat["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("messages=%d want 5: %v", len(msgs), msgs)
	}
	if asst := msgs[1].(map[string]any); asst["role"] != "assistant" || len(asst["tool_calls"].([]any)) != 1 {
		t.Fatalf("old call not emitted: %v", asst)
	}
	for _, raw := range msgs {
		if m, ok := raw.(map[string]any); ok && m["role"] == "tool" {
			t.Fatalf("stale call must not be paired, got tool msg: %v", m)
		}
	}
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("output without call_id after turn boundary must degrade to user, got %v", last)
	}
	if content, _ := last["content"].(string); !strings.Contains(content, "[tool output: get_weather]") || !strings.Contains(content, "旧回合结果") {
		t.Fatalf("content lost: %q", content)
	}
}

// 没有 call_id 但确实有未消费的 function_call：按声明顺序配对为 tool 消息。
func TestResponsesFunctionCallOutputWithoutCallIDPairsOpenCall(t *testing.T) {
	body := map[string]any{
		"model": "glm-5.2",
		"input": []any{
			map[string]any{"role": "user", "content": "查天气"},
			map[string]any{"type": "function_call", "call_id": "c1", "name": "get_weather", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "output": "晴"},
		},
	}
	chat, rerr := responsesToChat(body)
	if rerr != nil {
		t.Fatalf("rerr=%v", rerr)
	}
	msgs := chat["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages=%d want 3: %v", len(msgs), msgs)
	}
	tool := msgs[2].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "c1" || tool["content"] != "晴" {
		t.Fatalf("unpaired output should match the open call: %v", tool)
	}
}
