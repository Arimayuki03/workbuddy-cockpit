package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

func channelCheckServerBody() string {
	code := "1" + "1" + "1" + "2" + "8"
	return `{"code":` + code + `,"msg":"request illegal","requestId":"x",` +
		`"displayTips":{"zh":"这次调用没通过渠道校验，可能不是官方客户端发出的。",` +
		`"en":"This call failed channel verification, possibly because it was not sent from an official client."}}`
}

func testSSEResponse(status int, body string) *http.Response {
	ct := "application/json"
	if status == http.StatusOK {
		ct = "text/event-stream"
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{ct}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// A channel check is a request/content rejection (manager 0039), not evidence
// that the credential is dead: it must return 400 without cooling, disabling,
// rotating, or feeding the consecutive-fail counter.
func TestChatChannelCheckRequestScopedNotDisable(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return testSSEResponse(http.StatusBadRequest, channelCheckServerBody()), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: 600 * time.Second})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("channel check must return 400, got code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "channel_check_blocked") ||
		!strings.Contains(rec.Body.String(), "request illegal") {
		t.Errorf("upstream原文应透传: %s", rec.Body.String())
	}
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Fatalf("channel check must never disable a healthy account: reason=%q", st.DisabledReason)
	}
	if st.Cooling {
		t.Fatalf("channel check is request-scoped and must not cool the account, got %+v", st)
	}
	if st.ConsecutiveFails != 0 {
		t.Errorf("channel check must not feed consecutive-fail counter: %d", st.ConsecutiveFails)
	}
}

// A different account must NOT be tried after a channel rejection: the same
// request body would hit the same content check on every account (manager 0039).
func TestChatChannelCheckDoesNotRotate(t *testing.T) {
	var calls atomic.Int32
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return testSSEResponse(http.StatusBadRequest, channelCheckServerBody()), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: 600 * time.Second})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("channel check must be returned to the client, got code=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 1 {
		t.Errorf("upstream calls=%d want 1 (no account rotation on content rejection)", calls.Load())
	}
	for _, uid := range []string{"u1", "u2"} {
		st, _ := p.Status(uid)
		if st.Disabled || st.Cooling || st.ConsecutiveFails != 0 {
			t.Errorf("%s was penalized by a request-level rejection: %+v", uid, st)
		}
	}
}

// system 内容被渠道校验拦下时，做一次请求级抢救：把 system 文本原样搬进首条
// user、system 换成中性提示词后重试；成功则账号零冷却、零禁用（manager 0035）。
func TestChatChannelCheckMovesSystemIntoUserAndRetries(t *testing.T) {
	var bodies []string
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			bodies = append(bodies, string(b))
			if systemHasPersona(b) {
				return testSSEResponse(http.StatusBadRequest, channelCheckServerBody()), nil
			}
			return testSSEResponse(http.StatusOK, sseOK), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough", SoftCooldown: 600 * time.Second})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[`+
			`{"role":"system","content":"PERSONA-SAFE"},`+
			`{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("system 文本搬进 user 后应成功，got code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(bodies) < 2 {
		t.Fatalf("应至少重试一次，实际上游请求 %d 次", len(bodies))
	}
	last := bodies[len(bodies)-1]
	if systemHasPersona([]byte(last)) {
		t.Fatalf("重试请求仍是原始 system，未搬迁: %s", last)
	}
	if !strings.Contains(last, "PERSONA-SAFE") {
		t.Fatalf("重试请求丢了 system 文本: %s", last)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.ConsecutiveFails != 0 {
		t.Fatalf("内容问题不该罚账号：cooling=%v disabled=%v fails=%d", st.Cooling, st.Disabled, st.ConsecutiveFails)
	}
}

// systemHasPersona 判断请求体里是否还有「原始 system 内容」（persona 标记仍在
// system 通道）。出站前 client 会 normalize 重排消息字段，所以按 JSON 解析判断。
func systemHasPersona(body []byte) bool {
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return false
	}
	for _, m := range req.Messages {
		if m.Role == "system" && strings.Contains(m.Content, "PERSONA") {
			return true
		}
	}
	return false
}

// 11151 is deterministic for the request body. It must be a 400 after one
// upstream call, with no rotation and no account penalty.
func TestEmptyMessageReturns400WithoutRotation(t *testing.T) {
	const emptyBody = `{"code":11151,"msg":"a message has empty content, please check the conversation history and retry","requestId":"r1"}`
	var calls atomic.Int32
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return testSSEResponse(http.StatusBadRequest, emptyBody), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: 600 * time.Second})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":""}]}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty message should be HTTP 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 1 {
		t.Errorf("empty message must not rotate: upstream calls=%d want 1", calls.Load())
	}
	if !strings.Contains(rec.Body.String(), "11151") {
		t.Errorf("response should pass through upstream code 11151: %s", rec.Body.String())
	}
	for _, uid := range []string{"u1", "u2"} {
		st, _ := p.Status(uid)
		if st.Cooling || st.Disabled {
			t.Errorf("%s was penalized for a request-content error: %+v", uid, st)
		}
	}
}
