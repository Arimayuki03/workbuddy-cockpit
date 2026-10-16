package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// handler_tc_mismatch_test.go 11148 工具调用/结果错配的端到端验收（任务书
// tool-call-mismatch，最高优先）：上游 400 + code=11148 → 判定为**请求级**错误，
// 立即 400 透传上游原文，绝不轮转、绝不罚号。
//
// 背景：tool_pairing.go 已在出站前预防能修的 11148，但客户端回放的污染历史修不好时
// 上游仍会判 11148。此时旧路径（Classify 通用 4xx 兜底 → ErrClient）会把请求轮转全池
// 并按连败计数（NoteFailures）逐号打出去池——**用一个请求自己的历史错误罚光健康账号池**，
// 客户端还会收到 503 no_healthy_account，把它读成「账号不可用、稍后再试」而对必然失败
// 的请求无限重试。本组用例钉住两条纪律：不轮转（上游只被打一次）、不罚号（无冷却/无
// 连败/无熔断/无降权/无租约泄漏），并透传上游原文 + 会话级 gateway_hint。

// TestChatToolCallMismatchDoesNotRotateOrPenalize 非流式：多账号池 + 上游恒返
// 400/11148 → 400 tool_call_mismatch，上游只被打 1 次（不轮转），两个账号都干净。
func TestChatToolCallMismatchDoesNotRotateOrPenalize(t *testing.T) {
	const raw = `{"code":11148,"msg":"tool calls and tool results do not match, please start a new conversation and retry","requestId":"req-11148-abc","extError":{"code":"tool_call_sequence_broken"}}`
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 400, raw, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400（请求级错误，不是 503 no_healthy_account）", rec.Code, rec.Body)
	}
	// 不轮转：上游只被调用一次（此前的 ErrClient 路径会打满 MaxRotate 次，默认 3）。
	if calls != 1 {
		t.Fatalf("upstream calls=%d want 1（11148 换号不可能修好，禁止轮转）", calls)
	}
	var e struct {
		Error struct {
			Code        string `json:"code"`
			Message     string `json:"message"`
			GatewayHint string `json:"gateway_hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if e.Error.Code != "tool_call_mismatch" {
		t.Errorf("code=%q want tool_call_mismatch", e.Error.Code)
	}
	// message 逐字透传上游原文（code/msg/requestId/extError 原样，禁止固定词覆盖）。
	if e.Error.Message != raw {
		t.Errorf("message=%q want raw upstream body passthrough %q", e.Error.Message, raw)
	}
	if !strings.Contains(e.Error.Message, "11148") ||
		!strings.Contains(e.Error.Message, "tool calls and tool results do not match") {
		t.Errorf("message must keep upstream code and text: %s", e.Error.Message)
	}
	// gateway_hint：会话级指引（重建历史），并明确与账号无关——客户端不该按账号故障重试。
	if e.Error.GatewayHint == "" {
		t.Error("gateway_hint missing（11148 必须给出「开始新会话重建历史」的可行动作）")
	}
	if !strings.Contains(e.Error.GatewayHint, "new conversation") ||
		!strings.Contains(e.Error.GatewayHint, "not an account issue") {
		t.Errorf("gateway_hint=%q want 重建历史指向 + 与账号无关声明", e.Error.GatewayHint)
	}
	if strings.Contains(e.Error.Message, "all accounts are temporarily unavailable") {
		t.Errorf("message must NOT be the local scheduling text: %s", e.Error.Message)
	}
	// 不罚号：无冷却/禁用/累计错误/连败/降权/熔断/在途泄漏。
	for _, uid := range []string{"u1", "u2"} {
		st, ok := p.Status(uid)
		if !ok {
			t.Fatalf("status missing for %s", uid)
		}
		if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.ConsecutiveFails != 0 ||
			!st.DegradeUntil.IsZero() || st.BreakerFails != 0 || st.InFlight != 0 {
			t.Errorf("request-level mismatch penalized or leaked lease for %s: %+v", uid, st)
		}
	}
}

// TestChatToolCallMismatchStreamRequestFailsFast 流式请求（body stream=true）撞上游
// 400/11148：错误发生在开流前，两条路径共用同一 status>=400 分支——同样立即 400、
// 不轮转（上游 1 次）、不罚号（不会把 200 SSE 才有的处置路径当成账号故障）。
func TestChatToolCallMismatchStreamRequestFailsFast(t *testing.T) {
	const raw = `{"code":11148,"msg":"tool_call_sequence_broken: tool results do not match the preceding tool_calls"}`
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 400, raw, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Fatalf("upstream calls=%d want 1（流式请求同样不轮转）", calls)
	}
	assertJSONErrorCode(t, rec.Body.String(), "tool_call_mismatch")
	if !strings.Contains(rec.Body.String(), "tool_call_sequence_broken") {
		t.Errorf("upstream body must be preserved verbatim: %s", rec.Body)
	}
	for _, uid := range []string{"u1", "u2"} {
		st, _ := p.Status(uid)
		if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.ConsecutiveFails != 0 ||
			!st.DegradeUntil.IsZero() || st.BreakerFails != 0 || st.InFlight != 0 {
			t.Errorf("stream-path mismatch penalized or leaked lease for %s: %+v", uid, st)
		}
	}
}

// TestChatToolCallMismatchNestedEnvelopeAndQuotedCode 上游以深层嵌套 + 字符串 code
// （error.data.code="11148"）上报：分类必须命中断言同一路径，而不是退化成 ErrClient
// 去轮转（嵌套/引号形态是上游真实报文形状，见 client_test.go 的 11148 表）。
func TestChatToolCallMismatchNestedEnvelopeAndQuotedCode(t *testing.T) {
	const raw = `{"error":{"data":{"code":"11148","message":"invalid tool history"}}}`
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 400, raw, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != http.StatusBadRequest || calls != 1 {
		t.Fatalf("code=%d calls=%d body=%s want 400/1（嵌套信封同样请求级）", rec.Code, calls, rec.Body)
	}
	assertJSONErrorCode(t, rec.Body.String(), "tool_call_mismatch")
	// 原文在 JSON 里是转义形态，断言走解码后的 message 字段（逐字等于上游原文）。
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if e.Error.Message != raw {
		t.Errorf("message=%q want raw upstream body passthrough %q", e.Error.Message, raw)
	}
}
