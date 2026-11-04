package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestChatModelBlockedReturns404(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "a", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return http.StatusBadRequest, `{"code":11102,"msg":"model [missing-model] service info not found"}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"missing-model","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 body=%s", rec.Code, rec.Body.String())
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
	if body.Error.Code != "model_not_found" {
		t.Fatalf("code=%q want model_not_found", body.Error.Code)
	}
	if !strings.Contains(body.Error.Msg, "11102") {
		t.Fatalf("上游原文丢失: %q", body.Error.Msg)
	}
}

func TestChat429CarriesRetryAfterFromBody(t *testing.T) {
	resetAt := time.Now().Add(90 * time.Second).In(upstreamRateResetLoc())
	body := fmt.Sprintf(`{"code":6004,"msg":"模型限流，将在 %s 重置"}`, resetAt.Format("2006-01-02 15:04:05"))
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "a", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return http.StatusTooManyRequests, body, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429 body=%s", rec.Code, rec.Body.String())
	}
	sec := mustRetryAfter(t, rec)
	if sec < 80 || sec > 95 {
		t.Fatalf("Retry-After=%d want ~90", sec)
	}
}

func TestChat429CarriesRetryAfterFromHeader(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "a", ExpiresAt: 9999999999})
	up := newFakeUpstreamHeaders(t, func(string) (int, string, bool, http.Header) {
		h := http.Header{}
		h.Set("Retry-After", "77")
		return http.StatusTooManyRequests, `{"code":1,"msg":"rate limit"}`, false, h
	})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429 body=%s", rec.Code, rec.Body.String())
	}
	if sec := mustRetryAfter(t, rec); sec != 77 {
		t.Fatalf("Retry-After=%d want 77", sec)
	}
}

func upstreamRateResetLoc() *time.Location {
	return time.FixedZone("UTC+8", 8*60*60)
}

func mustRetryAfter(t *testing.T, rec *httptest.ResponseRecorder) int {
	t.Helper()
	raw := rec.Header().Get("Retry-After")
	if raw == "" {
		t.Fatalf("missing Retry-After body=%s", rec.Body.String())
	}
	sec, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("Retry-After=%q: %v", raw, err)
	}
	return sec
}
