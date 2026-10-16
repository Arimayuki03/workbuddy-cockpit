package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"workbuddy2api/internal/iputil"
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
// ---------------------------------------------------------------------------
// v1.19.0 过滤族（from/to/ip/ua）+ 来源采集口径
// ---------------------------------------------------------------------------

// requestLogsTestResp /api/request_logs 响应（既有形状：items/dropped/cap）。
type requestLogsTestResp struct {
	Items   []requestLogEntry `json:"items"`
	Dropped int64             `json:"dropped"`
	Cap     int               `json:"cap"`
}

// TestHandleRequestLogsFilters 过滤查询（from/to/ip/ua）走磁盘归档、复用
// reqlog.Filter 的现成语义：ip/ua 子串大小写不敏感、from/to **闭区间**
// （两端都含）、limit 按事件时间倒序截断。不传新参数仍走环形缓冲（逐字不变）。
func TestHandleRequestLogsFilters(t *testing.T) {
	rec := reqlog.New(reqlog.Config{Dir: t.TempDir()})
	oldLog, oldArchive := requestLog, globalRequestArchive
	requestLog = &requestLogStore{buf: make([]requestLogEntry, 8)}
	globalRequestArchive = rec
	t.Cleanup(func() {
		requestLog, globalRequestArchive = oldLog, oldArchive
		rec.Close()
	})

	// 整秒基准：unix 秒参数与事件时间必须无损往返（否则边界断言受纳秒误差干扰）。
	base := time.Now().Truncate(time.Second)
	appendRequestLog(requestLogEntry{Time: base.Add(-2 * time.Hour), Model: "cn:glm-5.2",
		Status: 200, Tokens: 1, ClientIP: "203.0.113.7", UserAgent: "wb2a-test/1.0"})
	appendRequestLog(requestLogEntry{Time: base.Add(-time.Hour), Model: "cn:glm-5.2",
		Status: 500, Tokens: 2, ClientIP: "203.0.113.9", UserAgent: "Mozilla/5.0 (Test) Chrome/120"})
	appendRequestLog(requestLogEntry{Time: base, Model: "global:claude-4.6",
		Status: 200, Tokens: 3, ClientIP: "198.51.100.4", UserAgent: "wb2a-test/2.0"})
	rec.Close() // 排空归档队列 + flush（归档是异步写）

	// 清空环形缓冲：此刻只有归档里还留着上面 3 条。后续断言因此能真正区分两条
	// 数据源（无参数/参数非法 → 环形缓冲；带过滤 → 归档），而不是靠数值巧合。
	requestLog = &requestLogStore{buf: make([]requestLogEntry, 4)}

	h := &Handler{}
	get := func(query string) requestLogsTestResp {
		t.Helper()
		w := httptest.NewRecorder()
		h.handleRequestLogs(w, httptest.NewRequest(http.MethodGet, "/api/request_logs"+query, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: code=%d body=%s", query, w.Code, w.Body.String())
		}
		var out requestLogsTestResp
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v (body=%s)", query, err, w.Body.String())
		}
		if out.Cap != requestLogCap {
			t.Fatalf("GET %s: cap=%d want %d（响应形状不变）", query, out.Cap, requestLogCap)
		}
		return out
	}

	cases := []struct {
		name  string
		query string
		want  int
	}{
		{"无新参数=环形缓冲（已清空 → 0 条）", "?limit=8", 0},
		{"非法 from 按未给处理=环形缓冲", "?from=abc", 0},
		{"非法 to 按未给处理=环形缓冲", "?to=1.5", 0},
		{"空 ip 参数按未给处理=环形缓冲", "?ip=", 0},
		{"空 ua 参数按未给处理=环形缓冲", "?ua=", 0},
		{"from/to=0 按未给处理=环形缓冲", "?from=0&to=0", 0},
		{"ip 前缀命中两条", "?ip=203.0.113", 2},
		{"ip 精确命中一条", "?ip=203.0.113.7", 1},
		{"ip 未命中", "?ip=192.0.2.9", 0},
		{"ua 子串命中（大小写不敏感）", "?ua=WB2A-TEST", 2},
		{"ua 大小写不敏感（混合）", "?ua=chrome", 1},
		{"ua 未命中", "?ua=curl/8.0", 0},
		{"from/to 覆盖全部事件", fmt.Sprintf("?from=%d&to=%d", base.Add(-2*time.Hour).Unix(), base.Unix()), 3},
		{"to 恰为事件时刻：含（闭区间）", fmt.Sprintf("?to=%d", base.Add(-time.Hour).Unix()), 2},
		{"from 恰为事件时刻：含（闭区间）", fmt.Sprintf("?from=%d", base.Add(-time.Hour).Unix()), 2},
		{"窗口在全部事件之后", fmt.Sprintf("?from=%d&to=%d", base.Add(time.Hour).Unix(), base.Add(2*time.Hour).Unix()), 0},
		{"ip+ua 组合过滤", "?ip=203.0.113.9&ua=chrome", 1},
		{"limit 截断（倒序取最新）", "?ip=203.0.113&limit=1", 1},
		{"ua 与 ip 维度同时命中", "?ip=198.51.100.4&ua=wb2a", 1},
	}
	for _, c := range cases {
		if got := get(c.query); len(got.Items) != c.want {
			t.Errorf("%s（%s）: %d 条命中, want %d（items=%+v）", c.name, c.query, len(got.Items), c.want, got.Items)
		}
	}

	// 归档事件折算回条目形状：字段与热路径同形（前端一套渲染），且 client_ip/ua
	// 正是过滤依据；limit=1 取的是时间最新一条。
	one := get("?ip=203.0.113&limit=1")
	if len(one.Items) != 1 {
		t.Fatalf("limit=1 命中 %d 条", len(one.Items))
	}
	it := one.Items[0]
	if it.Tokens != 2 || it.Status != 500 || it.Model != "cn:glm-5.2" {
		t.Errorf("归档条目字段不对: %+v", it)
	}
	if it.ClientIP != "203.0.113.9" || it.UserAgent != "Mozilla/5.0 (Test) Chrome/120" {
		t.Errorf("归档条目来源字段不对: %+v", it)
	}
	if !it.Time.Equal(base.Add(-time.Hour)) {
		t.Errorf("归档条目时间 = %v want %v", it.Time, base.Add(-time.Hour))
	}
}

// TestHandleRequestLogsFilterArchiveDisabled 归档未启用（logging.request_archive
// 缺省 false）时过滤查询返回空集，而不是静默改用别的数据源——观测缺位要可见
// （排障第一步是确认归档开关）；items 仍是空数组（契约字段类型不随数据源变化）。
func TestHandleRequestLogsFilterArchiveDisabled(t *testing.T) {
	oldLog, oldArchive := requestLog, globalRequestArchive
	requestLog = &requestLogStore{buf: make([]requestLogEntry, 4)}
	globalRequestArchive = nil
	t.Cleanup(func() { requestLog, globalRequestArchive = oldLog, oldArchive })
	appendRequestLog(requestLogEntry{Model: "m", Status: 200, Tokens: 1, ClientIP: "203.0.113.7"})

	h := &Handler{}
	w := httptest.NewRecorder()
	h.handleRequestLogs(w, httptest.NewRequest(http.MethodGet, "/api/request_logs?ip=203.0.113", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatalf("归档关闭时过滤查询应返回空集: %s", w.Body.String())
	}
	// 同一进程不传新参数仍是环形缓冲（既有行为）。
	w2 := httptest.NewRecorder()
	h.handleRequestLogs(w2, httptest.NewRequest(http.MethodGet, "/api/request_logs", nil))
	if !strings.Contains(w2.Body.String(), `"tokens":1`) {
		t.Fatalf("无过滤参数应仍走环形缓冲: %s", w2.Body.String())
	}
}

// TestRequestLogClientInfoTrustedProxy 采集侧 client_ip 与入站安全同口径
// （iputil 可信代理解析）：零配置/对端不可信时恒取 RemoteAddr，转发头一律不认
// （默认安全口径不放宽——伪造 X-Real-IP 不能污染日志来源）；对端落在可信网段内
// 才按 hops 采信 X-Real-IP / X-Forwarded-For。
func TestRequestLogClientInfoTrustedProxy(t *testing.T) {
	oldCIDRs, oldHops := clientInfoTrustedCIDRs, clientInfoTrustedHops
	t.Cleanup(func() { clientInfoTrustedCIDRs, clientInfoTrustedHops = oldCIDRs, oldHops })

	req := func(peer, realIP, xff string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.RemoteAddr = peer
		if realIP != "" {
			r.Header.Set("X-Real-IP", realIP)
		}
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}

	// 零配置（默认）：伪造转发头无效，恒 RemoteAddr。
	SetClientInfoTrust(nil, 0)
	if ip, _ := requestLogClientInfo(req("203.0.113.7:51234", "9.9.9.9", "9.9.9.9, 8.8.8.8"), true); ip != "203.0.113.7" {
		t.Fatalf("零配置下必须忽略转发头: %q", ip)
	}

	// 注入回环可信段（本机反代形态）：hops=1 时 X-Real-IP 优先。
	SetClientInfoTrust(iputil.LoopbackCIDRs(), 1)
	if ip, _ := requestLogClientInfo(req("127.0.0.1:5000", "198.51.100.9", ""), true); ip != "198.51.100.9" {
		t.Fatalf("可信对端的 X-Real-IP 应被采信: %q", ip)
	}
	// 无 X-Real-IP 时取 XFF 右起第 hops 个（hops=1 → 最右，即紧邻反代追加的值）。
	if ip, _ := requestLogClientInfo(req("127.0.0.1:5000", "", "198.51.100.9, 10.0.0.1"), true); ip != "10.0.0.1" {
		t.Fatalf("hops=1 应取 XFF 最右: %q", ip)
	}
	// hops=2（CDN + 反代两跳）→ 右起第 2 个才是真实客户端（多跳时不看 X-Real-IP）。
	SetClientInfoTrust(iputil.LoopbackCIDRs(), 2)
	if ip, _ := requestLogClientInfo(req("127.0.0.1:5000", "9.9.9.9", "198.51.100.9, 10.0.0.1"), true); ip != "198.51.100.9" {
		t.Fatalf("hops=2 应取 XFF 右起第 2 个: %q", ip)
	}
	// 对端不在可信网段：即使配了可信段也不认转发头。
	if ip, _ := requestLogClientInfo(req("203.0.113.7:51234", "9.9.9.9", "9.9.9.9"), true); ip != "203.0.113.7" {
		t.Fatalf("不可信对端的转发头必须忽略: %q", ip)
	}
	// 开关关闭（logging.request_client_info 缺省 false）：仍然零值不采集。
	if ip, ua := requestLogClientInfo(req("127.0.0.1:5000", "198.51.100.9", ""), false); ip != "" || ua != "" {
		t.Fatalf("开关关闭仍采集: ip=%q ua=%q", ip, ua)
	}
}

// TestRequestLogClientInfoUserAgentTruncate UA 截断到 userAgentCap 个字符：
// 按字符（rune）截断，多字节字符不被劈成非法 UTF-8（JSON 里会退化成替换字符）。
func TestRequestLogClientInfoUserAgentTruncate(t *testing.T) {
	ua := func(v string) string {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.RemoteAddr = "203.0.113.7:51234"
		r.Header.Set("User-Agent", v)
		_, got := requestLogClientInfo(r, true)
		return got
	}

	short := "wb2a-test/1.0"
	if got := ua(short); got != short {
		t.Fatalf("短 UA 不应被改动: %q", got)
	}
	atCap := strings.Repeat("A", userAgentCap)
	if got := ua(atCap); got != atCap {
		t.Fatalf("恰好 %d 字符不应截断: len=%d", userAgentCap, len(got))
	}
	long := strings.Repeat("A", userAgentCap+80)
	got := ua(long)
	if got != long[:userAgentCap] {
		t.Fatalf("超长 UA 应截断到 %d 字符: len=%d", userAgentCap, len(got))
	}
	cjk := strings.Repeat("测", userAgentCap+50)
	gotCJK := ua(cjk)
	if n := utf8.RuneCountInString(gotCJK); n != userAgentCap {
		t.Fatalf("多字节 UA 应按字符截断: runes=%d want %d", n, userAgentCap)
	}
	if !utf8.ValidString(gotCJK) {
		t.Fatal("多字节 UA 截断后必须是合法 UTF-8（不得劈开字符）")
	}
}

// ---------------------------------------------------------------------------
// v1.19.x 响应契约（只增不改）：source / archive_enabled + 500 分支不泄内部路径
// ---------------------------------------------------------------------------

// requestLogsSourceResp /api/request_logs 响应（既有 items/dropped/cap + 新增
// source/archive_enabled）。新增字段用指针：区分「字段缺失」与「零值（false/空串）」
// ——「只增不改」的契约必须验证字段真的在响应里，而不是被解码成默认值蒙混过关。
type requestLogsSourceResp struct {
	Items          []requestLogEntry `json:"items"`
	Dropped        *int64            `json:"dropped"`
	Cap            *int              `json:"cap"`
	ArchiveEnabled *bool             `json:"archive_enabled"`
	Source         *string           `json:"source"`
}

// TestHandleRequestLogsSourceFields source / archive_enabled 标明本次 items 的真实
// 取数通路：无过滤参数走内存环形缓冲（source=ring）、带任一过滤参数走磁盘归档
// （source=archive）；archive_enabled 反映归档是否已启用（包级引用 nil = 未启用）。
//
// 归档未启用的空集形态（前端契约）：items:[] + source:"archive" + archive_enabled:false
// ——source 描述**通路**而非结果，前端据此提示「归档未启用」而不是「归档里没命中」
// （两者排障含义不同）；既有键 dropped/cap 在两个分支都必须保留（存在性一并断言，
// 且它们只描述环形缓冲，与归档分支的 items 无关）。
func TestHandleRequestLogsSourceFields(t *testing.T) {
	oldLog, oldArchive := requestLog, globalRequestArchive
	t.Cleanup(func() { requestLog, globalRequestArchive = oldLog, oldArchive })

	// 归档侧素材：真落一条事件（Close 排空队列 + flush），供 archive 通路断言。
	rec := reqlog.New(reqlog.Config{Dir: t.TempDir()})
	rec.Record(reqlog.Event{Time: time.Now().Truncate(time.Second), Model: "cn:glm-5.2",
		Status: 200, Tokens: 7, ClientIP: "203.0.113.7"})
	rec.Close()

	cases := []struct {
		name       string
		archive    bool // 注入归档引用；false = 未启用（nil）
		query      string
		wantSource string
		wantItems  int
		wantModel  string // 命中条目的 model：证 items 真来自所标通路（空集为 ""）
	}{
		{"归档已启用+无过滤参数=环形缓冲通路", true, "?limit=5", requestLogSourceRing, 1, "ring:only"},
		{"归档已启用+过滤参数=归档通路", true, "?ip=203.0.113.7", requestLogSourceArchive, 1, "cn:glm-5.2"},
		{"归档未启用+无过滤参数=环形缓冲通路", false, "?limit=5", requestLogSourceRing, 1, "ring:only"},
		{"归档未启用+过滤参数=归档通路但空集", false, "?ip=203.0.113.7", requestLogSourceArchive, 0, ""},
	}
	h := &Handler{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			requestLog = &requestLogStore{buf: make([]requestLogEntry, 4)}
			appendRequestLog(requestLogEntry{Model: "ring:only", Status: 200, Tokens: 1})
			if c.archive {
				globalRequestArchive = rec
			} else {
				globalRequestArchive = nil
			}

			w := httptest.NewRecorder()
			h.handleRequestLogs(w, httptest.NewRequest(http.MethodGet, "/api/request_logs"+c.query, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
			}
			var out requestLogsSourceResp
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode: %v (body=%s)", err, w.Body.String())
			}
			if out.Source == nil || *out.Source != c.wantSource {
				t.Errorf("source=%v want %q（body=%s）", out.Source, c.wantSource, w.Body.String())
			}
			if out.ArchiveEnabled == nil || *out.ArchiveEnabled != c.archive {
				t.Errorf("archive_enabled=%v want %v（body=%s）", out.ArchiveEnabled, c.archive, w.Body.String())
			}
			// 既有键必须保留（只增不改）：cap 仍是环形缓冲容量。
			if out.Dropped == nil || out.Cap == nil || *out.Cap != requestLogCap {
				t.Errorf("既有键 dropped/cap 缺失或变了: dropped=%v cap=%v", out.Dropped, out.Cap)
			}
			if len(out.Items) != c.wantItems {
				t.Fatalf("items=%d want %d (%+v)", len(out.Items), c.wantItems, out.Items)
			}
			if c.wantItems > 0 && out.Items[0].Model != c.wantModel {
				t.Errorf("items 来源与 source 不符: model=%q want %q", out.Items[0].Model, c.wantModel)
			}
		})
	}
}

// TestHandleRequestLogsArchiveErrorNoPathLeak 归档读失败（目录被删/权限异常）走
// 500 分支：对外固定文案 + 细节只进服务端日志，绝不回显底层 *os.PathError 的文本
// ——它含归档目录/分片文件的绝对路径（内部拓扑）。错误信封形状 {"ok":false,"error":...}
// 保持不变（只换文案）。
func TestHandleRequestLogsArchiveErrorNoPathLeak(t *testing.T) {
	// 目录名带独特片段：跨平台都能在响应体里做「路径未泄漏」断言（Windows 的
	// 反斜杠在 JSON 里会变成 \\，用名字片段就不受转义影响）。
	dir := filepath.Join(t.TempDir(), "reqlog-pathleak-probe")
	rec := reqlog.New(reqlog.Config{Dir: dir})
	rec.Close() // 先停后台写 goroutine：读路径不受写路径干扰，删目录也不受句柄牵制

	oldLog, oldArchive := requestLog, globalRequestArchive
	requestLog = &requestLogStore{buf: make([]requestLogEntry, 4)}
	globalRequestArchive = rec
	t.Cleanup(func() { requestLog, globalRequestArchive = oldLog, oldArchive })

	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("删除归档目录: %v", err)
	}
	// 自证触发条件：底层错误文本确实带绝对路径（否则「不泄漏」断言是空跑）。
	if _, err := rec.ReadArchive(0, &reqlog.Filter{ClientIP: "203.0.113.7"}); err == nil || !strings.Contains(err.Error(), dir) {
		t.Fatalf("触发条件不成立（期望含路径的读失败）: %v", err)
	}

	h := &Handler{}
	w := httptest.NewRecorder()
	h.handleRequestLogs(w, httptest.NewRequest(http.MethodGet, "/api/request_logs?ip=203.0.113.7", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d want 500 body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"ok":false`) || !strings.Contains(body, requestLogArchiveErrMsg) {
		t.Fatalf("信封形状/固定文案不对: %s", body)
	}
	for _, leak := range []string{dir, "reqlog-pathleak-probe"} {
		if strings.Contains(body, leak) {
			t.Fatalf("响应体泄漏内部路径 %q（底层错误应只进日志）: %s", leak, body)
		}
	}
	// 旧形态（"read request archive: " + err.Error()）不得复现：冒号后跟底层文本。
	if strings.Contains(body, "read request archive: ") {
		t.Fatalf("响应体仍回显底层错误文本: %s", body)
	}
}
