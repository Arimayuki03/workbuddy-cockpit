package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// TestChatRowEffortColumn 表格日志的 eff= 列：入站>出站，双空显示 "-"。
func TestChatRowEffortColumn(t *testing.T) {
	out := captureStdout(t, func() {
		withChatLog(t)
		logChatRow(time.Second, 2*time.Second, "deepseek-v4.1-flash", "stream", "u1", "nick",
			http.StatusOK, 10, "xhigh", "high")
		logChatRow(time.Second, 2*time.Second, "glm-5.3-flash", "sync", "u1", "nick",
			http.StatusOK, 10, "", "")
	})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 rows, got %d: %q", len(lines), out)
	}
	if !strings.Contains(lines[0], "eff=xhigh>high") {
		t.Errorf("row1 should carry effort column: %s", lines[0])
	}
	if !strings.Contains(lines[1], "eff=-") {
		t.Errorf("row2 (no effort) should show eff=-: %s", lines[1])
	}
}

// TestChatStatEffortObservation 端到端：显式档位原样透传（无能力桶时不降级）；
// deepseek 缺 effort 时入站为空、出站为补默认档 high。表格行与请求日志（环形
// 缓冲）都带两个档位。
func TestChatStatEffortObservation(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return testSSEResponse(http.StatusOK, sseOK), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})
	out := captureStdout(t, func() {
		withChatLog(t)
		for _, raw := range []string{
			`{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"max"}`,
			`{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(raw)))
			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
			}
		}
	})
	if !strings.Contains(out, "eff=max>max") {
		t.Errorf("explicit effort should pass through: %q", out)
	}
	if !strings.Contains(out, "eff=->high") {
		t.Errorf("missing effort should default to high on deepseek: %q", out)
	}
	entries := requestLogsSnapshot(2)
	if len(entries) < 2 {
		t.Fatalf("request log ring entries=%d want 2", len(entries))
	}
	// snapshot 新→旧：第一条是缺 effort 的请求，第二条是显式 max。
	if entries[0].EffortIn != "" || entries[0].EffortOut != "high" {
		t.Errorf("entries[0] efforts = %q/%q want /high", entries[0].EffortIn, entries[0].EffortOut)
	}
	if entries[1].EffortIn != "max" || entries[1].EffortOut != "max" {
		t.Errorf("entries[1] efforts = %q/%q want max/max", entries[1].EffortIn, entries[1].EffortOut)
	}
}
