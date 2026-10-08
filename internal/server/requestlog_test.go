package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/reqlog"
	"workbuddy2api/internal/session"
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
	st := newChatStat(time.Now(), session.ParseRequest([]byte(`{"model":"cn:glm-5.2","stream":true}`)), "cn")
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

// TestRequestLogArchiveNilInjector 未配置归档（globalRequestArchive=nil）时
// appendRequestLog 行为与引入归档前逐位一致：环形缓冲照常、无 panic。
func TestRequestLogArchiveNilInjector(t *testing.T) {
	oldLog, oldArchive := requestLog, globalRequestArchive
	requestLog = &requestLogStore{buf: make([]requestLogEntry, 4)}
	globalRequestArchive = nil
	t.Cleanup(func() { requestLog, globalRequestArchive = oldLog, oldArchive })

	for i := 0; i < 6; i++ {
		appendRequestLog(requestLogEntry{Model: "m", Status: 200, Tokens: i})
	}
	snap := requestLogsSnapshot(0)
	if len(snap) != 4 || snap[0].Tokens != 5 {
		t.Fatalf("nil 归档时环形缓冲异常: len=%d head=%d", len(snap), snap[0].Tokens)
	}
}

// TestRequestLogArchiveEnabled 开启归档后事件异步落盘（环形缓冲与归档双写）。
func TestRequestLogArchiveEnabled(t *testing.T) {
	dir := t.TempDir()
	rec := reqlog.New(reqlog.Config{Dir: dir})
	oldLog, oldArchive := requestLog, globalRequestArchive
	requestLog = &requestLogStore{buf: make([]requestLogEntry, 4)}
	globalRequestArchive = rec
	t.Cleanup(func() {
		requestLog, globalRequestArchive = oldLog, oldArchive
		rec.Close()
	})

	appendRequestLog(requestLogEntry{
		Model: "cn:glm-5.2", UID: "u-1234", Nick: "号甲", Mode: "stream",
		Status: 200, Tokens: 42, TTFBMS: 120,
	})
	rec.Close() // 排空队列 + flush

	rows, err := rec.ReadArchive(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("归档 %d 条 want 1", len(rows))
	}
	e := rows[0]
	if e.Model != "cn:glm-5.2" || e.Mode != "stream" || e.Status != 200 ||
		e.Tokens != 42 || e.TTFBMS != 120 || !e.OK {
		t.Errorf("归档事件字段不对: %+v", e)
	}
	// Account 走「昵称(uid8)」标签口径（不落完整 UID）。
	if e.Account == "" || e.Account == "u-1234" {
		t.Errorf("Account 应为昵称(uid8) 标签形态: %q", e.Account)
	}
}

// TestRequestLogClientInfoSwitch client_info 开关控制来源字段采集：
// 关（默认）零值不采集；开则填 RemoteAddr 的 IP 部分与 UA。
func TestRequestLogClientInfoSwitch(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = "203.0.113.7:51234"
	r.Header.Set("User-Agent", "wb2a-test/1.0")

	// 关（缺省口径）：零值。
	ip, ua := requestLogClientInfo(r, false)
	if ip != "" || ua != "" {
		t.Errorf("开关关闭仍采集: ip=%q ua=%q", ip, ua)
	}
	// 开：IP 剥端口 + UA 原样。
	ip, ua = requestLogClientInfo(r, true)
	if ip != "203.0.113.7" || ua != "wb2a-test/1.0" {
		t.Errorf("采集口径不对: ip=%q ua=%q", ip, ua)
	}
	// IPv6 复合形态剥方括号壳。
	r6 := httptest.NewRequest(http.MethodPost, "/", nil)
	r6.RemoteAddr = "[2001:db8::1]:443"
	if ip, _ = requestLogClientInfo(r6, true); ip != "2001:db8::1" {
		t.Errorf("ipv6 剥壳不对: %q", ip)
	}
}

// TestChatStatClientInfoFlowsToPayload chatStat 的来源字段经 payload 流入
// 环形缓冲条目与归档事件（noteClientInfo → requestLogPayloadFromStat 全链）。
func TestChatStatClientInfoFlowsToPayload(t *testing.T) {
	oldArchive := globalRequestArchive
	rec := reqlog.New(reqlog.Config{Dir: t.TempDir()})
	globalRequestArchive = rec
	t.Cleanup(func() { globalRequestArchive = oldArchive; rec.Close() })

	st := newChatStat(time.Now(), session.ParseRequest([]byte(`{"model":"cn:glm-5.2"}`)), "cn")
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = "198.51.100.4:9999"
	r.Header.Set("User-Agent", "wb2a-e2e/2.0")
	st.noteClientInfo(r, true)
	st.status = 200
	e := requestLogPayloadFromStat(st)
	if e.ClientIP != "198.51.100.4" || e.UserAgent != "wb2a-e2e/2.0" {
		t.Fatalf("来源字段未流入 payload: %+v", e)
	}
	globalRequestArchive.Record(requestLogArchiveEvent(e))
	rec.Close()
	rows, _ := rec.ReadArchive(0, nil)
	if len(rows) != 1 || rows[0].ClientIP != "198.51.100.4" || rows[0].UserAgent != "wb2a-e2e/2.0" {
		t.Errorf("归档来源字段不对: %+v", rows)
	}
}
func TestVersionLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v1.1.1", "v1.2.0", true},
		{"v1.2.0", "v1.2.0", false},
		{"v2.0.0", "v1.9.9", false},
		{"dev", "v9.9.9", false}, // 非法形态不提示更新
		{"v1.2.0-rc1", "v1.2.1", true},
		{"", "v1.0.0", false},
	}
	for _, c := range cases {
		if got := versionLess(c.a, c.b); got != c.want {
			t.Errorf("versionLess(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}
