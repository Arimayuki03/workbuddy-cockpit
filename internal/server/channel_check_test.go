package server

import (
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

// A channel check is not evidence that the credential is dead. It must put the
// account into a recoverable soft cooldown and never write Disabled.
func TestChatChannelCheckSoftCooldownNotDisable(t *testing.T) {
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

	st, _ := p.Status("u1")
	if st.Disabled {
		t.Fatalf("channel check must never disable a healthy account: reason=%q", st.DisabledReason)
	}
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("channel check should soft-cool the account, got %+v", st)
	}
	if !strings.Contains(st.Reason, "channel check") {
		t.Errorf("cooldown reason should identify a channel check, got %q", st.Reason)
	}
}

// A different account can still serve the same request after another account
// hits the channel check: the failure is per-request/path, not pool-wide.
func TestChatChannelCheckRotatesToHealthyAccount(t *testing.T) {
	var calls atomic.Int32
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			if r.Header.Get("Authorization") == "Bearer at1" {
				return testSSEResponse(http.StatusBadRequest, channelCheckServerBody()), nil
			}
			return testSSEResponse(http.StatusOK, sseOK), nil
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
	if rec.Code != http.StatusOK {
		t.Fatalf("healthy account should serve after channel-check rotation: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 2 {
		t.Errorf("upstream calls=%d want 2 (channel-check account + healthy account)", calls.Load())
	}
	st1, _ := p.Status("u1")
	if st1.Disabled {
		t.Error("channel-check account must not be disabled")
	}
	st2, _ := p.Status("u2")
	if st2.Disabled || st2.Cooling {
		t.Errorf("healthy account was penalized: %+v", st2)
	}
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
