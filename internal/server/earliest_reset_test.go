// 多号同时限流时返回最早恢复的原文与 Retry-After（manager v2.11.11 / issue #403）。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestChat429ReportsEarliestReset(t *testing.T) {
	early := time.Now().Add(5 * time.Minute)
	late := time.Now().Add(2 * time.Hour)
	fmtTS := func(at time.Time) string {
		return at.In(upstreamRateResetLoc()).Format("2006-01-02 15:04:05")
	}
	earlyTS, lateTS := fmtTS(early), fmtTS(late)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-a" {
			return http.StatusTooManyRequests, "{\"code\":6004,\"msg\":\"将在 " + earlyTS + " UTC+8 重置\",\"requestId\":\"a\"}", false
		}
		return http.StatusTooManyRequests, "{\"code\":6004,\"msg\":\"将在 " + lateTS + " UTC+8 重置\",\"requestId\":\"b\"}", false
	})
	p := testPoolWith(
		&auth.Auth{UID: "a", AccessToken: "at-a", ExpiresAt: 9999999999},
		&auth.Auth{UID: "b", AccessToken: "at-b", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader("{\"model\":\"glm-5.3\",\"messages\":[]}")))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s (want 429)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, earlyTS) {
		t.Errorf("429 文案应给最早恢复时间 %s，got %s", earlyTS, body)
	}
	if strings.Contains(body, lateTS) {
		t.Errorf("429 文案不该给更晚的重置时间 %s，got %s", lateTS, body)
	}
	if sec := mustRetryAfter(t, rec); sec < 280 || sec > 305 {
		t.Errorf("Retry-After=%d want ~300（最早恢复时间）", sec)
	}
}

// 回归（review high）：早恢复的号只回 Retry-After 头、body 无「将在 … 重置」文案时，
// 它仍必须成为最早恢复候选，否则最终 429 会按最后一个号（更晚）计算，客户端白等。
func TestChat429ReportsEarliestResetFromHeader(t *testing.T) {
	late := time.Now().Add(2 * time.Hour)
	lateTS := late.In(upstreamRateResetLoc()).Format("2006-01-02 15:04:05")
	up := newFakeUpstreamHeaders(t, func(authz string) (int, string, bool, http.Header) {
		if authz == "Bearer at-a" {
			h := http.Header{}
			h.Set("Retry-After", "300") // header-only：5 分钟
			return http.StatusTooManyRequests, `{"code":1,"msg":"too many requests"}`, false, h
		}
		return http.StatusTooManyRequests,
			`{"code":6004,"msg":"将在 ` + lateTS + ` UTC+8 重置","requestId":"b"}`, false, nil
	})
	p := testPoolWith(
		&auth.Auth{UID: "a", AccessToken: "at-a", ExpiresAt: 9999999999},
		&auth.Auth{UID: "b", AccessToken: "at-b", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader("{\"model\":\"glm-5.3\",\"messages\":[]}")))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s (want 429)", rec.Code, rec.Body.String())
	}
	if sec := mustRetryAfter(t, rec); sec < 290 || sec > 305 {
		t.Errorf("Retry-After=%d want ~300（来自最早号的头，而不是最后一条 body 的 2h）", sec)
	}
	if strings.Contains(rec.Body.String(), lateTS) {
		t.Errorf("429 文案不该用更晚账号的重置时间 %s：%s", lateTS, rec.Body.String())
	}
}
