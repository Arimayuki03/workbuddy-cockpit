package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

// captureStdout 重定向 os.Stdout 并捕获 fn 期间的全部输出。
// 同时重置 chatLogOut（chat 表格日志的输出目标）：生产默认 os.Stdout，
// 但包级变量已被初始化时求值的 os.Stdout 覆盖（与重定向后的新 os.Stdout
// 不同一），不重置的话表格行会写进旧管道，测试捕获不到。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	oldOut := chatLogOut
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	chatLogOut = w
	t.Cleanup(func() {
		os.Stdout = old
		chatLogOut = oldOut
	})
	fn()
	_ = w.Close()
	raw, _ := io.ReadAll(r)
	return string(raw)
}

// withChatLog 临时开启聊天表格日志（TestMain 默认关闭），测试结束后恢复。
// 仅供断言表格行输出的用例使用。
func withChatLog(t *testing.T) {
	t.Helper()
	old := chatLogEnabled
	chatLogEnabled = true
	t.Cleanup(func() { chatLogEnabled = old })
}

// TestDeriveCacheWrite 缓存写入推导口径（上游不下发 write）：
//   - raw>0：上游真值优先（未来开始下发时零改动）；
//   - raw=0 且 miss≥64（最小缓存块）：write=miss（未命中前缀即被写入部分，
//     双轮探针 2858 miss → 次轮 2688 hit 佐证，精度 ~94%）；
//   - miss<64：实测不写缓存（50 token 前缀次轮 hit=0），保持 0。
func TestDeriveCacheWrite(t *testing.T) {
	cases := []struct {
		name      string
		raw, miss int
		want      int
	}{
		{"raw wins", 123, 2858, 123},
		{"derive from miss", 0, 2858, 2858},
		{"below cache block", 0, 50, 0},
		{"zero miss", 0, 0, 0},
	}
	for _, c := range cases {
		if got := deriveCacheWrite(c.raw, c.miss); got != c.want {
			t.Errorf("%s: deriveCacheWrite(%d,%d)=%d want %d", c.name, c.raw, c.miss, got, c.want)
		}
	}
}

// TestChatStatsReaderCacheWriteDerived 流式末帧 usage 的写入推导：
// 上游 write=0、miss=2858 → 观测 write=2858（hit/miss 原样）。
func TestChatStatsReaderCacheWriteDerived(t *testing.T) {
	sse := "data: {\"usage\":{\"prompt_tokens\":2858," +
		"\"prompt_cache_hit_tokens\":0,\"prompt_cache_miss_tokens\":2858,\"prompt_cache_write_tokens\":0}}\n\n" +
		"data: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatalf("copy: %v", err)
	}
	hit, miss, write := r.CacheTokens()
	if hit != 0 || miss != 2858 {
		t.Fatalf("hit/miss=%d/%d want 0/2858", hit, miss)
	}
	if write != 2858 {
		t.Fatalf("write=%d want 2858（推导：上游不下发 write）", write)
	}
}

// TestFillStatFromUsageCacheWriteDerived 非流式聚合 usage 同口径推导。
func TestFillStatFromUsageCacheWriteDerived(t *testing.T) {
	st := &chatStat{}
	fillStatFromUsage(st, map[string]any{
		"usage": map[string]any{
			"prompt_tokens":             float64(2867),
			"prompt_cache_hit_tokens":   float64(2688),
			"prompt_cache_miss_tokens":  float64(179),
			"prompt_cache_write_tokens": float64(0),
		},
	})
	if st.cacheHit != 2688 || st.cacheMiss != 179 {
		t.Fatalf("hit/miss=%d/%d want 2688/179", st.cacheHit, st.cacheMiss)
	}
	if st.cacheWr != 179 {
		t.Fatalf("write=%d want 179（次轮新前缀同样在写入）", st.cacheWr)
	}
}

func TestChatStatsReaderTokensFromUsage(t *testing.T) { // 起点回拨 1ms：内存流（strings.Reader）瞬时返回，用 time.Now() 作起点会让
	// ttfb=time.Since(start) 在同一时钟滴答内测得 0（Windows 精度 ~0.5ms 尤甚）。
	// 生产 SSE 是网络流 ttfb 必然 >0；此处回拨起点模拟"已过一段时间"的可分辨测量。
	r := newChatStatsReaderSince(strings.NewReader(sseOK), time.Now().Add(-time.Millisecond))
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatalf("copy: %v", err)
	}
	toks, ok := r.Tokens()
	if !ok || toks != 1 {
		t.Fatalf("tokens=%d ok=%v, want 1/true (from usage, not rune count)", toks, ok)
	}
	if r.TTFB() <= 0 {
		t.Errorf("ttfb=%v want >0", r.TTFB())
	}
}

func TestChatStatsReaderNoUsage(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.Copy(io.Discard, r)
	if toks, ok := r.Tokens(); ok || toks != 0 {
		t.Errorf("tokens=%d ok=%v, want 0/false for missing usage", toks, ok)
	}
}

func TestChatStatsReaderLastFrameUsageWins(t *testing.T) {
	sse := "data: {\"usage\":{\"completion_tokens\":5}}\n\n" +
		"data: {\"usage\":{\"completion_tokens\":12}}\n\n" +
		"data: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.Copy(io.Discard, r)
	toks, ok := r.Tokens()
	if !ok || toks != 12 {
		t.Fatalf("tokens=%d ok=%v, want 12 (last frame wins)", toks, ok)
	}
}

// TestChatStatsReaderCreditMissing (P0, RED): usage 存在但 credit 字段缺失时
// Credit() 必须返回 ok=false——缺失≠免费，不能把缺观测当 0 扣费记入账本
// （否则收费的 global 号可能被误判 tier0 免费被永久优先）。
func TestChatStatsReaderCreditMissing(t *testing.T) {
	sse := "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n" +
		"data: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.Copy(io.Discard, r)
	if toks, ok := r.Tokens(); !ok || toks != 5 {
		t.Fatalf("tokens=%d ok=%v want 5/true (usage still供 token)", toks, ok)
	}
	credit, ok := r.Credit()
	if ok {
		t.Errorf("Credit()=(%v,true) want ok=false: usage 无 credit 字段 ≠ 0 成本", credit)
	}
}

// TestChatStatsReaderCreditExplicitZero (P0, RED/GREEN): usage 显式 credit:0 是合法免费观测，
// Credit() 必须 ok=true 且 credit==0——真 0 不许丢（显式 0 与字段缺失语义不同）。
func TestChatStatsReaderCreditExplicitZero(t *testing.T) {
	sse := "data: {\"usage\":{\"prompt_tokens\":500,\"completion_tokens\":500,\"credit\":0}}\n\n" +
		"data: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.Copy(io.Discard, r)
	credit, ok := r.Credit()
	if !ok || credit != 0 {
		t.Errorf("Credit()=(%v,%v) want (0,true): 显式 credit:0 是合法免费观测", credit, ok)
	}
}

// TestChatStatsReaderJSONNullCredit 回归保护：usage.credit 显式 null 也算缺失
// （null ≠ 0），不得被当作免费观测。
func TestChatStatsReaderJSONNullCredit(t *testing.T) {
	sse := "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"credit\":null}}\n\n" +
		"data: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.Copy(io.Discard, r)
	if _, ok := r.Credit(); ok {
		t.Error("usage.credit=null 应视为缺失（ok=false）")
	}
}

// TestChatStatsReaderNoUsage 末帧完全无 usage → Credit() ok=false（现状已对，回归保护）。
func TestChatStatsReaderCreditNoUsage(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.Copy(io.Discard, r)
	if _, ok := r.Credit(); ok {
		t.Error("无 usage 帧 Credit() 应 ok=false")
	}
}

func TestChatStatsReaderTTFBOnlyOnDataFrame(t *testing.T) {
	start := time.Now().Add(-2 * time.Second)
	var s chatStatsReader
	s.start = start
	s.parseSSELine("event: ping")
	if s.TTFB() != 0 {
		t.Errorf("non-data line must not set TTFB: %v", s.TTFB())
	}
	s.parseSSELine("data: {\"choices\":[]}")
	first := s.TTFB()
	if first < time.Second {
		t.Errorf("first data frame TTFB=%v want >=2s", first)
	}
	s.parseSSELine("data: {\"choices\":[]}")
	if s.TTFB() != first {
		t.Errorf("second frame changed TTFB: %v -> %v", first, s.TTFB())
	}
}

func TestChatStatsReaderBytesPassthrough(t *testing.T) {
	sse := "data: {\"content\":\"你好\"}\n\ndata: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	out, _ := io.ReadAll(r)
	if string(out) != sse {
		t.Errorf("passthrough mismatch:\n got %q\nwant %q", out, sse)
	}
}

func TestParseModelFromBody(t *testing.T) {
	if got := parseModelFromBody([]byte(`{"model":"deepseek-v4-flash","stream":true}`)); got != "deepseek-v4-flash" {
		t.Errorf("got %q", got)
	}
	if got := parseModelFromBody([]byte(`{}`)); got != "-" {
		t.Errorf("got %q want -", got)
	}
	if got := parseModelFromBody([]byte(`not json`)); got != "-" {
		t.Errorf("got %q want -", got)
	}
}

func TestCompletionTokensExtraction(t *testing.T) {
	got := completionTokens(map[string]any{
		"usage": map[string]any{"prompt_tokens": 10.0, "completion_tokens": 234.0, "total_tokens": 244.0},
	})
	if got != 234 {
		t.Errorf("got %d want 234", got)
	}
	if got := completionTokens(map[string]any{}); got != -1 {
		t.Errorf("missing usage: got %d want -1", got)
	}
	if got := completionTokens(map[string]any{"usage": map[string]any{}}); got != -1 {
		t.Errorf("missing completion_tokens: got %d want -1", got)
	}
}

func TestUIDPrefix(t *testing.T) {
	if got := uidPrefix("00e26541abcdef012345"); got != "00e26541" {
		t.Errorf("long uid -> %q", got)
	}
	if got := uidPrefix("abc"); got != "abc" {
		t.Errorf("short uid -> %q", got)
	}
	if got := uidPrefix(""); got != "-" {
		t.Errorf("empty uid -> %q", got)
	}
}

func TestLogChatRowFormat(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(412*time.Millisecond, 27100*time.Millisecond, "deepseek-v4.1-flash", "stream", "00e26541abcdef", "sample", http.StatusOK, 1234, "xhigh", "high")
	})
	for _, want := range []string{
		"| #", "deepseek-v4.1-flash", "| stream |", "| 200 |", "sample(00e26541)", "TTFB=412ms", "tok=1234", "tok/s", "total=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("row missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "00e26541abcdef") {
		t.Errorf("full uid leaked: %s", out)
	}
}

// rowAcctSegment 取流水行的账号列原始片段（含两侧空格），供定宽断言。
func rowAcctSegment(row string) (string, bool) {
	parts := strings.Split(row, "|")
	// "", seq, time, model, mode, status, acct, TTFB, tok, rate, eff, total, ""
	if len(parts) < 8 {
		return "", false
	}
	return parts[6], true
}

// TestLogChatRowAccountFixedWidth 账号列严格定宽（chatAcctWidth）：昵称超宽时按
// 显示列宽截断、uid8 后缀完整保留；4 字中文昵称（18 列）原样放下不截。
// 旧实现「只补不截」会让掩码手机号昵称（21 列）把整行撑宽，尾部列被顶出可视区。
func TestLogChatRowAccountFixedWidth(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(0, time.Second, "m", "stream", "a1b2c3d4e5", "173****6289", http.StatusOK, 1, "", "")
		logChatRow(0, time.Second, "m", "stream", "c5f12af0e6", "蝶舞飞扬", http.StatusOK, 1, "", "")
	})
	rows := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d: %q", len(rows), out)
	}
	// 掩码手机号：昵称截到 8 列 + "(a1b2c3d4)" 后缀 = 18，uid8 完整保留。
	if !strings.Contains(rows[0], "173****6(a1b2c3d4)") {
		t.Errorf("long nick should truncate with uid8 suffix intact:\n%s", rows[0])
	}
	// 4 字中文昵称（8 列 + 10 = 18）恰好放满，不截。
	if !strings.Contains(rows[1], "蝶舞飞扬(c5f12af0)") {
		t.Errorf("4-char CJK nick should fit untruncated:\n%s", rows[1])
	}
	// 两行账号列均严格等于 chatAcctWidth 显示列宽（定宽承诺）。
	for i, row := range rows {
		seg, ok := rowAcctSegment(row)
		if !ok {
			t.Fatalf("row %d: acct segment not found: %q", i, row)
		}
		if w := logfmt.DisplayWidth(seg) - 2; w != chatAcctWidth { // 去掉两侧空格
			t.Errorf("row %d: acct column width=%d want %d (segment %q)", i, w, chatAcctWidth, seg)
		}
	}
}

// TestLogChatRowEffortColumnFullWidth 思考档位列按最宽组合（minimal×2 = 15 列）
// 完整展示：旧的 11 列会把 "minimal>minimal" 截成 "minimal>mini"。
func TestLogChatRowEffortColumnFullWidth(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(0, time.Second, "m", "stream", "u1", "nick", http.StatusOK, 1, "minimal", "minimal")
	})
	if !strings.Contains(out, "eff=minimal>minimal") {
		t.Errorf("widest effort pair must render untruncated:\n%s", out)
	}
}

// TestLogChatRowModelNotTruncated 守护模型名不再被截断。
// 旧实现硬截 11 字节，把 "cn:deepseek-v4.1-flash" 切成 "cn:deepseek"，
// 运维会误以为是另一个模型（真实踩坑点）。26 列宽覆盖 realm 前缀 + 最长模型名。
func TestLogChatRowModelNotTruncated(t *testing.T) {
	withChatLog(t)
	for _, model := range []string{"cn:deepseek-v4.1-flash", "global:deepseek-v4.1-flash"} {
		out := captureStdout(t, func() {
			logChatRow(0, time.Second, model, "stream", "00e26541abcdef", "sample", http.StatusOK, 1, "", "")
		})
		if !strings.Contains(out, model) {
			t.Errorf("model %q truncated to something else:\n%s", model, out)
		}
	}
}

// TestLogChatRowNicknameFallback 无昵称（旧 auth 文件未落 account.nickname）时退回 uid8。
func TestLogChatRowNicknameFallback(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(0, time.Second, "glm-5.2", "sync", "00e26541abcdef", "", http.StatusOK, 1, "", "")
	})
	if !strings.Contains(out, "00e26541 ") && !strings.Contains(out, "00e26541|") {
		t.Errorf("want bare uid8 label without nickname:\n%s", out)
	}
	if strings.Contains(out, "(") {
		t.Errorf("empty nickname must not render parens:\n%s", out)
	}
}

func TestLogChatRowNoUsageShowsDash(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(0, time.Second, "glm-5.2", "sync", "s1", "", http.StatusServiceUnavailable, -1, "", "")
	})
	for _, want := range []string{"TTFB=-", "tok=-", "| 503 |"} {
		if !strings.Contains(out, want) {
			t.Errorf("row missing %q:\n%s", want, out)
		}
	}
	// 无 usage 时速率列也应是裸 "-"，不能凭空报 0.0tok/s（会把缺失当零值读）。
	if strings.Contains(out, "0.0tok/s") || strings.Contains(out, "-tok/s") {
		t.Errorf("missing usage must render bare '-' rate column:\n%s", out)
	}
}

func TestLogChatRowSeqIncrements(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(0, time.Second, "m", "sync", "u", "", 200, 1, "", "")
		logChatRow(0, time.Second, "m", "sync", "u", "", 200, 1, "", "")
	})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d:\n%s", len(lines), out)
	}
	first := strings.Fields(lines[0])[1]
	second := strings.Fields(lines[1])[1]
	if !strings.HasPrefix(first, "#") || !strings.HasPrefix(second, "#") {
		t.Fatalf("seq columns missing: %q %q", first, second)
	}
	if first == second {
		t.Errorf("seq not incremented: %q == %q", first, second)
	}
}

func TestChatLogsStreamRow(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	for _, want := range []string{"| stream |", "| 200 |", "| u1 ", "TTFB=", "tok=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream row missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "tok=1") {
		t.Errorf("tok: want precise usage completion_tokens: %s", out)
	}
}

// sseNoUsage 与 sseOK 同形但末帧不带 usage 子对象（上游未回报口径）。
const sseNoUsage = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

// TestChatLogsStreamRowNoUsageShowsDash 流式末帧无 usage 时 tok 列应显示 "-"。
// chatStat.toks 初值 -1 即「观测缺失」哨兵（见字段注释与 logChatRow 的 toks<0
// 分支），不得被 stats.Tokens() 的零值 0 覆盖成「测得 0 token」——后者是把「缺观测」
// 伪装成「测得 0」的伪造观测（tok=0 tok/s=0.0）。非流式同场景走 completionTokens
// 返回 -1 保留了哨兵，两条路径口径必须一致。
func TestChatLogsStreamRowNoUsageShowsDash(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseNoUsage, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	if !strings.Contains(out, "tok=-") {
		t.Errorf("流式无 usage 应显示 tok=-（观测缺失），实际:\n%s", out)
	}
	if strings.Contains(out, "tok=0") {
		t.Errorf("流式无 usage 被伪造成 tok=0（测得 0 token）:\n%s", out)
	}
}

func TestChatLogsSyncRowTTFBDash(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	for _, want := range []string{"| sync ", "| 200 |", "TTFB=-", "tok=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("sync row missing %q:\n%s", want, out)
		}
	}
}

func TestChatLogsErrorRow(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 402, `{"code":1,"msg":"余额不足"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 503 {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
	})
	for _, want := range []string{"| u1 ", "| 503 |", "tok=-"} {
		if !strings.Contains(out, want) {
			t.Errorf("error row missing %q:\n%s", want, out)
		}
	}
}

func TestHealthzDoesNotLogTableRow(t *testing.T) {
	withChatLog(t) // 日志开启也应无表格行：非 chat 路由根本不走 logChatRow
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
		rec2 := httptest.NewRecorder()
		h.ServeHTTP(rec2, httptest.NewRequest("GET", "/v1/models", nil))
		rec3 := httptest.NewRecorder()
		h.ServeHTTP(rec3, httptest.NewRequest("GET", "/status", nil))
	})
	if strings.Contains(out, "| #") {
		t.Errorf("healthz/models/status must not emit table rows:\n%s", out)
	}
}
