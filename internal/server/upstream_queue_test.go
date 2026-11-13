// 6020「排队等待」不是账号级限流（manager v2.11.13 / issue #416）：
// 账号不冷却、不计数、不禁用；对外仍回 429 让客户端等待。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

func TestApplyErrorPolicyUpstreamQueueDoesNotCool(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "a", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p})
	h.applyErrorPolicy("u1", upstream.ErrUpstreamQueue,
		`{"code":6020,"msg":"queue.waiting.title","data":{"queue_position":5580,"retry_after":40}}`,
		"kimi-k3.1-pro", &upstream.Error{Kind: upstream.ErrUpstreamQueue, Status: http.StatusTooManyRequests})
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 missing")
	}
	if st.Cooling || st.Disabled {
		t.Fatalf("排队不应冷却/禁用账号: %+v", st)
	}
	if st.SoftStreak != 0 {
		t.Errorf("排队不应推进 soft_streak: %d", st.SoftStreak)
	}
}

func TestChatUpstreamQueueReturns429WithoutCooling(t *testing.T) {
	const queueBody = `{"code":6020,"msg":"queue.waiting.title","data":{"queue_position":5580,"retry_after":40}}`
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "a", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return http.StatusTooManyRequests, queueBody, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429 body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
			Msg  string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if body.Error.Code != "rate_limit_exceeded" {
		t.Errorf("code=%q want rate_limit_exceeded", body.Error.Code)
	}
	if !strings.Contains(body.Error.Msg, "6020") {
		t.Errorf("应透传上游排队原文，got %q", body.Error.Msg)
	}
	// body 里的 data.retry_after=40 必须折算成标准 Retry-After，否则客户端会立刻
	// 重试、把全局队列再打一遍（review medium）。
	if got := rec.Header().Get("Retry-After"); got != "40" {
		t.Errorf("Retry-After=%q want 40（来自 data.retry_after）", got)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.SoftStreak != 0 {
		t.Fatalf("排队后账号不应被罚: %+v", st)
	}
}
