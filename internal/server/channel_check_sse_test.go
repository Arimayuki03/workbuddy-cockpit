package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
)

// 弱 11-128 的 SSE error 帧（只有 message + code，没有强渠道文案）不能把
// request illegal 当账号故障禁用，也不能解绑粘性。
func TestChannelCheckWeakErrorFrameDoesNotDisable(t *testing.T) {
	frame := `{"error":{"message":"request illegal","code":"11-128","requestId":"r1"}}`
	bindStore := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     bindStore,
		Available: func() []string { return []string{"u1"} },
	})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, "data: " + frame + "\n\n", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	sess.Bind("conv-weak", "u1")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-weak"}}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("wire code=%d want 200 (headers already sent)", rec.Code)
	}
	st, _ := p.Status("u1")
	if st.Disabled || st.Cooling || st.ConsecutiveFails != 0 {
		t.Fatalf("weak 11-128 error frame must not penalize the account: %+v", st)
	}
	if uid, ok := bindStore.lastUID("conv-weak"); !ok || uid != "u1" {
		t.Fatalf("weak 11-128 must not unbind sticky: uid=%q ok=%v", uid, ok)
	}
}

// 弱 11-128 的非流式 Aggregate：请求级 400 content_blocked，不罚号、不解绑粘性。
func TestChannelCheckWeakAggregateDoesNotDisable(t *testing.T) {
	frame := `{"error":{"message":"request illegal","code":"11-128","requestId":"r1"}}`
	bindStore := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     bindStore,
		Available: func() []string { return []string{"u1"} },
	})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, "data: " + frame + "\n\n", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	sess.Bind("conv-weak-agg", "u1")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-weak-agg"}}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 (request-level 11-128), body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "content_blocked") {
		t.Errorf("weak 11-128 aggregate should be content_blocked, got %s", rec.Body.String())
	}
	st, _ := p.Status("u1")
	if st.Disabled || st.Cooling || st.ConsecutiveFails != 0 {
		t.Fatalf("weak 11-128 aggregate must not penalize the account: %+v", st)
	}
	if uid, ok := bindStore.lastUID("conv-weak-agg"); !ok || uid != "u1" {
		t.Fatalf("weak 11-128 aggregate must not unbind sticky: uid=%q ok=%v", uid, ok)
	}
}

// 强渠道 error 帧仍按请求级处理：流式不罚号/不解绑，Aggregate 回 400 channel_check_blocked。
func TestChannelCheckStrongErrorFrameRequestScoped(t *testing.T) {
	frame := `{"error":{"message":"request illegal","code":"11-128","displayTips":{"en":"This call failed channel verification."}}}`
	bindStore := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     bindStore,
		Available: func() []string { return []string{"u1"} },
	})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, "data: " + frame + "\n\n", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	sess.Bind("conv-strong", "u1")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-strong"}}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("wire code=%d want 200", rec.Code)
	}
	st, _ := p.Status("u1")
	if st.Disabled || st.Cooling || st.ConsecutiveFails != 0 {
		t.Fatalf("strong channel error frame must not penalize: %+v", st)
	}
	if uid, ok := bindStore.lastUID("conv-strong"); !ok || uid != "u1" {
		t.Fatalf("strong channel error frame must not unbind sticky: uid=%q ok=%v", uid, ok)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-strong-agg"}}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "channel_check_blocked") {
		t.Fatalf("strong channel aggregate should be 400 channel_check_blocked, got code=%d body=%s", rec.Code, rec.Body.String())
	}
}
