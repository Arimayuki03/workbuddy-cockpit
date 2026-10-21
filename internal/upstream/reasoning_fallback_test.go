package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func withReasoningFallback(t *testing.T, on bool) {
	t.Helper()
	old := ReasoningFallbackEnabled()
	ConfigureReasoningFallback(on)
	t.Cleanup(func() { ConfigureReasoningFallback(old) })
}

func reasoningSSEFrame(t *testing.T, delta map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"id": "chatcmpl-x", "object": "chat.completion.chunk", "created": 1,
		"choices": []any{map[string]any{"index": 0, "delta": delta}},
	})
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	return "data: " + string(raw)
}

func reasoningMessage(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("no choices in %v", resp)
	}
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	if msg == nil {
		t.Fatalf("no message in %v", choice)
	}
	return msg
}

func TestAggregateBackfillsReasoningAsContent(t *testing.T) {
	withReasoningFallback(t, true)
	raw := strings.Join([]string{
		reasoningSSEFrame(t, map[string]any{"role": "assistant", "reasoning_content": "思考A"}),
		"",
		reasoningSSEFrame(t, map[string]any{"reasoning_content": "思考B"}),
		"",
		"data: [DONE]",
		"",
	}, "\n")

	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	msg := reasoningMessage(t, resp)
	if got := msg["content"]; got != "思考A思考B" {
		t.Errorf("content=%v want reasoning text when there is no assistant text", got)
	}
	if got := msg["reasoning_content"]; got != "思考A思考B" {
		t.Errorf("reasoning_content=%v must be preserved", got)
	}
}

func TestAggregateKeepsContentAndSkipsBackfillOnTools(t *testing.T) {
	withReasoningFallback(t, true)

	withContent := strings.Join([]string{
		reasoningSSEFrame(t, map[string]any{"reasoning_content": "思考"}),
		"",
		reasoningSSEFrame(t, map[string]any{"content": "正文"}),
		"",
		"data: [DONE]",
		"",
	}, "\n")
	resp, err := Aggregate(strings.NewReader(withContent))
	if err != nil {
		t.Fatalf("aggregate content: %v", err)
	}
	if got := reasoningMessage(t, resp)["content"]; got != "正文" {
		t.Errorf("content=%v want 正文; existing text must never be replaced", got)
	}

	withTools := strings.Join([]string{
		reasoningSSEFrame(t, map[string]any{"reasoning_content": "思考"}),
		"",
		reasoningSSEFrame(t, map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "id": "call_1", "type": "function",
			"function": map[string]any{"name": "f", "arguments": "{}"},
		}}}),
		"",
		"data: [DONE]",
		"",
	}, "\n")
	resp, err = Aggregate(strings.NewReader(withTools))
	if err != nil {
		t.Fatalf("aggregate tools: %v", err)
	}
	if got := reasoningMessage(t, resp)["content"]; got != "" {
		t.Errorf("content=%v want empty when tool calls exist", got)
	}
}

func TestAggregateReasoningFallbackDisabledByDefault(t *testing.T) {
	withReasoningFallback(t, false)
	raw := strings.Join([]string{
		reasoningSSEFrame(t, map[string]any{"reasoning_content": "思考"}),
		"",
		"data: [DONE]",
		"",
	}, "\n")
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if got := reasoningMessage(t, resp)["content"]; got != "" {
		t.Errorf("content=%v want empty when fallback is disabled", got)
	}
}

// The synthesized content must precede finish_reason, otherwise clients stop
// accumulating at finish and never render the text. Usage follows finish and
// must remain after it.
func TestStreamBackfillPrecedesFinishAndUsage(t *testing.T) {
	withReasoningFallback(t, true)
	raw := strings.Join([]string{
		reasoningSSEFrame(t, map[string]any{"role": "assistant", "reasoning_content": "思考A"}),
		"",
		reasoningSSEFrame(t, map[string]any{"reasoning_content": "思考B"}),
		"",
		`data: {"id":"chatcmpl-x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"",
		`data: {"id":"chatcmpl-x","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(raw), nil); err != nil {
		t.Fatalf("stream: %v", err)
	}
	out := rec.Body.String()
	contentPos := strings.Index(out, `"content":"思考A思考B"`)
	finishPos := strings.Index(out, `"finish_reason":"stop"`)
	usagePos := strings.Index(out, `"completion_tokens":2`)
	donePos := strings.Index(out, "data: [DONE]")
	if contentPos < 0 || finishPos < 0 || usagePos < 0 || donePos < 0 {
		t.Fatalf("missing frames: content=%d finish=%d usage=%d done=%d\n%s", contentPos, finishPos, usagePos, donePos, out)
	}
	if !(contentPos < finishPos && finishPos < usagePos && usagePos < donePos) {
		t.Errorf("frame order must be content < finish < usage < DONE (%d/%d/%d/%d):\n%s",
			contentPos, finishPos, usagePos, donePos, out)
	}
}

func TestStreamNormalTurnUnchangedByReasoningFallback(t *testing.T) {
	withReasoningFallback(t, true)
	raw := strings.Join([]string{
		reasoningSSEFrame(t, map[string]any{"reasoning_content": "思考"}),
		"",
		reasoningSSEFrame(t, map[string]any{"content": "正文"}),
		"",
		`data: {"id":"chatcmpl-x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(raw), nil); err != nil {
		t.Fatalf("stream: %v", err)
	}
	out := rec.Body.String()
	if strings.Contains(out, `"content":"思考"`) {
		t.Errorf("normal turn received a duplicate reasoning-as-content frame:\n%s", out)
	}
	if !strings.Contains(out, `"content":"正文"`) {
		t.Errorf("original content missing:\n%s", out)
	}
}

func TestStreamBackfillDisabled(t *testing.T) {
	withReasoningFallback(t, false)
	raw := strings.Join([]string{
		reasoningSSEFrame(t, map[string]any{"reasoning_content": "思考"}),
		"",
		`data: {"id":"chatcmpl-x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(raw), nil); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if strings.Contains(rec.Body.String(), `"content":"思考"`) {
		t.Errorf("fallback disabled but content frame was emitted:\n%s", rec.Body.String())
	}
}
