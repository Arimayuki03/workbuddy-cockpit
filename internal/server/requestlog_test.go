package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRequestLogRing 环形覆盖：容量外旧条目被覆盖，快照最新优先。
func TestRequestLogRing(t *testing.T) {
	old := requestLog
	requestLog = &requestLogStore{buf: make([]requestLogEntry, 8)}
	t.Cleanup(func() { requestLog = old })

	for i := 0; i < 10; i++ {
		appendRequestLog(requestLogEntry{Model: "m", Status: 200, Tokens: i})
	}
	snap := requestLogsSnapshot(0)
	if len(snap) != 8 {
		t.Fatalf("snapshot len=%d want 8", len(snap))
	}
	// 最新在前：第一条 tokens=9（最后写入），最后一条 tokens=2（第 10 条写入后最早存活）。
	if snap[0].Tokens != 9 {
		t.Errorf("snap[0].Tokens=%d want 9", snap[0].Tokens)
	}
	if snap[7].Tokens != 2 {
		t.Errorf("snap[7].Tokens=%d want 2", snap[7].Tokens)
	}
	if snap[0].Seq != 10 || snap[7].Seq != 3 {
		t.Errorf("seq range=%d..%d want 10..3", snap[0].Seq, snap[7].Seq)
	}
	// limit 截断。
	if got := requestLogsSnapshot(3); len(got) != 3 || got[0].Tokens != 9 {
		t.Errorf("limit snapshot len=%d first=%d want 3/9", len(got), got[0].Tokens)
	}
}

// TestHandleRequestLogs 端点：limit 参数生效、JSON 形状（items/dropped/cap）。
func TestHandleRequestLogs(t *testing.T) {
	old := requestLog
	requestLog = &requestLogStore{buf: make([]requestLogEntry, 8)}
	t.Cleanup(func() { requestLog = old })
	appendRequestLog(requestLogEntry{Model: "cn:glm-5.2", Status: 400, Tokens: -1, Error: "http 400: boom"})

	h := &Handler{}
	w := httptest.NewRecorder()
	h.handleRequestLogs(w, httptest.NewRequest(http.MethodGet, "/api/request_logs?limit=5", nil))
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`"items"`, `"cap":1000`, `"dropped":0`, `"tokens":-1`, `http 400: boom`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
}

// TestRequestLogPayloadFromStat chatStat → 条目字段映射（含 error 摘要与 -1 哨兵保留）。
func TestRequestLogPayloadFromStat(t *testing.T) {
	st := newChatStat(time.Now(), []byte(`{"model":"cn:glm-5.2"}`), true)
	st.uid = "u-1234"
	st.nick = "昵称"
	st.status = 503
	st.ttfb = 120 * time.Millisecond
	st.setError("rate limited")
	e := requestLogPayloadFromStat(st)
	if e.Model != "cn:glm-5.2" || e.Status != 503 || e.TTFBMS != 120 || e.Error != "rate limited" || e.UID != "u-1234" {
		t.Errorf("payload mismatch: %+v", e)
	}
	if e.Tokens != -1 {
		t.Errorf("usage 缺失哨兵丢失: tokens=%d want -1", e.Tokens)
	}
}

// TestVersionLess 语义化版本比较（预发布后缀忽略、非法形态恒 false）。
func TestVersionLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v1.1.1", "v1.2.0", true},
		{"v1.2.0", "v1.2.0", false},
		{"v2.0.0", "v1.9.9", false},
		{"dev", "v9.9.9", false},  // 非法形态不提示更新
		{"v1.2.0-rc1", "v1.2.1", true},
		{"", "v1.0.0", false},
	}
	for _, c := range cases {
		if got := versionLess(c.a, c.b); got != c.want {
			t.Errorf("versionLess(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}
