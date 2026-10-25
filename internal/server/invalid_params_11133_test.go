package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// TestInvalidRequestParams11133FastFail 11133「Invalid request parameters」是请求级
// 确定性拒绝（manager 52f2407c 吸收）：400 透传上游原文、不轮转、不喂连败——
// 否则一次 Kimi 探测就会把健康号的连败计数喂高。
func TestInvalidRequestParams11133FastFail(t *testing.T) {
	const body11133 = `{"code":11133,"msg":"Invalid request parameters",` +
		`"extError":{"code":"400001","message":"the request parameters were rejected by the model provider"}}`
	var calls atomic.Int32
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return testSSEResponse(http.StatusBadRequest, body11133), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: 600 * time.Second, MaxRotate: 3})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"kimi-k3.1-pro","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "11133") {
		t.Errorf("应透传上游 11133 原文: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"invalid_request"`) {
		t.Errorf("want invalid_request code: %s", rec.Body.String())
	}
	if calls.Load() != 1 {
		t.Errorf("upstream calls=%d want 1（确定性请求错误不轮转）", calls.Load())
	}
	for _, uid := range []string{"u1", "u2"} {
		st, _ := p.Status(uid)
		if st.Cooling || st.Disabled || st.ConsecutiveFails > 0 {
			t.Errorf("%s 被连坐: %+v", uid, st)
		}
	}
}

// requestId 里撞上 "11133" 数字串不构成 11133 判据（结构化 code 字段匹配）。
func TestRequestIDContaining11133NotFastFailed(t *testing.T) {
	const bodyReqID = `{"code":1500,"msg":"other error","requestId":"req-11133-abc"}`
	var calls atomic.Int32
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			if calls.Load() < 3 {
				return testSSEResponse(http.StatusBadRequest, bodyReqID), nil
			}
			return testSSEResponse(http.StatusOK, sseOK), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: 600 * time.Second, MaxRotate: 3})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s（requestId 撞串不得触发 11133 快速失败）", rec.Code, rec.Body.String())
	}
}
