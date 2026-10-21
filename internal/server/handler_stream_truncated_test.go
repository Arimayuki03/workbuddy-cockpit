package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// A 200 response whose upstream stream ends after a content frame, without
// finish_reason or [DONE], must expose a protocol error before [DONE] and must
// be logged as 502. HTTP headers are already committed, so changing the wire
// status is impossible; failure metrics/logging must still tell the truth.
func TestChatStreamTruncatedLogged502WithProtocolError(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, "data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"half\"}}]}\n\n", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	out := captureStdout(t, func() {
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	})

	if rec.Code != 200 {
		t.Fatalf("wire code=%d want 200 (headers already committed)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "upstream_truncated") {
		t.Fatalf("client must receive truncation error event: %s", body)
	}
	if strings.Index(body, "upstream_truncated") > strings.Index(body, "data: [DONE]") {
		t.Errorf("truncation event must precede [DONE]: %s", body)
	}
	if !strings.Contains(out, "| 502 |") {
		t.Errorf("truncated stream must be logged as 502, got:\n%s", out)
	}
}

// Production Anthropic route: a truncated OpenAI stream must be translated into
// an Anthropic `event: error` before message_stop, and must not be reported as a
// normal completed message.
func TestAnthropicStreamTruncatedSurfacesError(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, "data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"half\"}}]}\n\n", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	h.MountAnthropic()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	body := rec.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Fatalf("Anthropic stream must surface an error event: %s", body)
	}
	if idxErr, idxStop := strings.Index(body, "event: error"), strings.Index(body, "event: message_stop"); idxStop < 0 || idxErr > idxStop {
		t.Errorf("Anthropic error event must precede message_stop: %s", body)
	}
}

// Production Responses route: the same truncated stream must terminate with
// response.failed, never response.completed.
func TestResponsesStreamTruncatedFails(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, "data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"half\"}}]}\n\n", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	h.MountResponses()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"input":"hi"}`)))
	body := rec.Body.String()
	if !strings.Contains(body, "response.failed") {
		t.Fatalf("Responses stream must terminate with response.failed: %s", body)
	}
	if strings.Contains(body, "response.completed") {
		t.Errorf("truncated Responses stream must not emit response.completed: %s", body)
	}
}

// Non-stream path: Aggregate must reject a stream that ended without
// finish_reason/[DONE] instead of synthesizing a normal 200 response.
func TestChatAggregateTruncatedReturns502(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, "data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"half\"}}]}\n\n", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	out := captureStdout(t, func() {
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":"hi"}]}`)))
	})
	if rec.Code != 502 {
		t.Fatalf("non-stream truncated code=%d want 502 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "upstream_truncated") {
		t.Errorf("non-stream truncation must be diagnosable: %s", rec.Body.String())
	}
	if !strings.Contains(out, "| 502 |") {
		t.Errorf("non-stream truncation must be logged as 502, got:\n%s", out)
	}
	st, _ := p.Status("u1")
	if st.SuccessCount != 0 {
		t.Errorf("truncated aggregate must not be recorded as success: %+v", st)
	}
}
