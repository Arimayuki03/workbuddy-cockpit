package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// ---------------------------------------------------------------------------
// R15-2 GlobalRegisterStatus needsRegion 判定收紧
// ---------------------------------------------------------------------------

// globalRegStatusSrv 起一个 fake 注册激活端点，respond 决定信封 (code,msg)。
func globalRegStatusSrv(t *testing.T, respond func() (int, string)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/auth/realms/copilot/overseas/user/register") {
			t.Errorf("path=%s want register endpoint", r.URL.Path)
		}
		code, msg := respond()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "msg": msg, "data": nil})
	}))
	t.Cleanup(srv.Close)
	return &Client{
		HTTP:              &http.Client{},
		BillingBaseGlobal: srv.URL,
		GlobalEnabled:     true,
	}
}

// 回归：needsRegion 判定收紧——msg 含 "region required" 才算需补地区；
// 其余 code==500 类服务端故障归入 default 分支（needsRegion=false），不再把
// 上游故障账号误送进"提交地区+二次激活"完整链路。
func TestGlobalRegisterStatusNeedsRegionOnlyOnMsgMatch(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	cases := []struct {
		name            string
		code            int
		msg             string
		wantActivated   bool
		wantNeedsRegion bool
	}{
		{"activated", 200, "register success", true, false},
		{"region required msg wins over code 500", 500, "region required", false, true},
		{"region required msg even with other code", 503, "please set Region Required first", false, true},
		{"server 500 without region msg", 500, "internal server error", false, false},
		{"server 500 empty msg", 500, "", false, false},
		{"other business failure", 14017, "trial not activated", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := globalRegStatusSrv(t, func() (int, string) { return tc.code, tc.msg })
			activated, needsRegion, msg, err := c.GlobalRegisterStatus(globalAcct())
			if err != nil {
				t.Fatalf("status: %v", err)
			}
			if activated != tc.wantActivated || needsRegion != tc.wantNeedsRegion {
				t.Errorf("code=%d msg=%q: activated=%v needsRegion=%v, want %v/%v",
					tc.code, msg, activated, needsRegion, tc.wantActivated, tc.wantNeedsRegion)
			}
		})
	}
}

// GlobalCompleteRegistration 对"服务端 500 故障账号"不再走补地区链路：
// 不应调用 get-country-code / console/login/account，直接以未激活错误返回。
func TestGlobalCompleteRegistrationServerFaultSkipsRegionFlow(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var regionFlowCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/auth/realms/copilot/overseas/user/register"):
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 500, "msg": "internal server error", "data": nil})
		case strings.Contains(r.URL.Path, "/billing/area/get-country-code"),
			strings.Contains(r.URL.Path, "/console/login/account"):
			regionFlowCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "ok", "data": "null"})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := &Client{HTTP: &http.Client{}, BillingBaseGlobal: srv.URL, GlobalEnabled: true}
	activated, err := c.GlobalCompleteRegistration(globalAcct())
	if activated {
		t.Fatal("server fault should not report activated")
	}
	if err == nil || !strings.Contains(err.Error(), "internal server error") {
		t.Fatalf("err=%v, want not-activated error carrying server msg", err)
	}
	if regionFlowCalls.Load() != 0 {
		t.Errorf("region flow invoked %d times on server fault, want 0", regionFlowCalls.Load())
	}
}

// ---------------------------------------------------------------------------
// R15-3 fetchGlobalModelsOnce singleflight 门闩
// ---------------------------------------------------------------------------

// 回归：并发冷启动（缓存全 miss）时探测只发一次——旧实现锁内查缓存、锁外探测，
// N 个 /v1/models 同时 miss 会 N 倍打上游（probe 自身已是 3 路并发）。
// 慢探测注入：服务器首次命中延迟 200ms，5 个 goroutine 同时进入，
// 断言上游收到的 /v3/config 请求数 == 2（单次探测的双 UA 路数）。
func TestFetchGlobalModelsOnceConcurrentColdStartSingleFlight(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var v3Calls atomic.Int32
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/v3/config") {
			v3Calls.Add(1)
			once.Do(func() { time.Sleep(200 * time.Millisecond) }) // 冷启动慢探测（仅首请求）
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"gpt-5.4","name":"GPT-5.4"}]}}`))
	}))
	defer srv.Close()

	c := globalModelsClient(t, srv)

	const n = 5
	var wg sync.WaitGroup
	results := make([][]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = c.FetchGlobalModels(globalAcct())
		}(i)
	}
	wg.Wait()

	if got := v3Calls.Load(); got != 2 {
		t.Errorf("concurrent cold start made %d /v3/config calls, want 2 (single probe, 2 UA paths)", got)
	}
	for i, r := range results {
		if len(r) != 1 || r[0] != "gpt-5.4" {
			t.Errorf("goroutine %d result=%v, want [gpt-5.4]", i, r)
		}
	}
	// 门闩已复位：TTL 内二次调用零上游请求（缓存命中）。
	before := v3Calls.Load()
	if got := c.FetchGlobalModels(globalAcct()); len(got) != 1 {
		t.Errorf("cached refetch = %v", got)
	}
	if v3Calls.Load() != before {
		t.Errorf("cached refetch made new upstream calls")
	}
}

// 失败路径同样持门闩：并发 miss 全失败时探测只发生一次（负缓存由持门闩者写入）。
func TestFetchGlobalModelsOnceConcurrentFailureSingleFlight(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var v3Calls atomic.Int32
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/v3/config") {
			v3Calls.Add(1)
			once.Do(func() { time.Sleep(150 * time.Millisecond) })
		}
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"code":500,"msg":"boom"}`))
	}))
	defer srv.Close()

	c := globalModelsClient(t, srv)

	const n = 5
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := c.FetchGlobalModels(globalAcct()); len(got) != 0 {
				t.Errorf("failure result=%v want empty", got)
			}
		}()
	}
	wg.Wait()

	if got := v3Calls.Load(); got != 2 {
		t.Errorf("concurrent failure made %d /v3/config calls, want 2", got)
	}
	// 负缓存生效：冷却期内再调零新请求。
	before := v3Calls.Load()
	if got := c.FetchGlobalModels(globalAcct()); len(got) != 0 {
		t.Errorf("negative cache refetch = %v", got)
	}
	if v3Calls.Load() != before {
		t.Errorf("negative cache refetch made new upstream calls")
	}
}

// ---------------------------------------------------------------------------
// R15-4 scanServerRequestID 扫描指针前进
// ---------------------------------------------------------------------------

// 回归：首个 "id":" 候选不匹配 idRegex 时扫描指针前进，其后合法 id 仍被扫到。
// 旧实现 bytes.Index 恒从 buf[0] 起，首个非法候选永久卡死扫描 → 流结束误报
// "SSE 中未找到服务端 requestId"。
func TestScanServerRequestIDAdvancesPastInvalidCandidate(t *testing.T) {
	valid := "cmb-" + strings.Repeat("a", 32)
	buf := []byte(`data: {"id":"invalid-not-32hex","x":1}` + "\n\n" +
		`data: {"id":"` + valid + `"}` + "\n\n")

	id, _, found := scanServerRequestID(buf, 0)
	if !found {
		t.Fatalf("valid id after invalid candidate not found: %q", id)
	}
	if id != valid {
		t.Errorf("id=%q want %q", id, valid)
	}
}

// 合法 id 在前：立即命中（与旧行为一致，零回归）。
func TestScanServerRequestIDFirstCandidateValid(t *testing.T) {
	valid := strings.Repeat("b", 32) // 裸 32hex 形态
	buf := []byte(`data: {"id":"` + valid + `"}`)

	id, _, found := scanServerRequestID(buf, 0)
	if !found || id != valid {
		t.Fatalf("id=%q found=%v want %q true", id, found, valid)
	}
}

// 候选跨 chunk 截断：值未闭合时不前进（停在候选起点待拼齐），拼接后可命中。
func TestScanServerRequestIDTruncatedCandidateWaitsForMoreData(t *testing.T) {
	valid := "cmb-" + strings.Repeat("c", 32)
	part1 := []byte(`data: {"id":"cmb-cccc`) // 值被 chunk 边界截断

	id, next, found := scanServerRequestID(part1, 0)
	if found {
		t.Fatalf("truncated candidate should not match, got %q", id)
	}
	// 从返回的 next 续扫拼接后的完整 buf。
	full := append(append([]byte{}, part1...), []byte(valid[len("cmb-cccc"):]+`","y":2}`)...)
	id, _, found = scanServerRequestID(full, next)
	if !found || id != valid {
		t.Fatalf("resumed scan id=%q found=%v want %q true", id, found, valid)
	}
}

// 多个非法候选连续出现：指针逐一前进，不重扫已判起点（死循环防护 + 末尾合法 id 可达）。
func TestScanServerRequestIDMultipleInvalidThenValid(t *testing.T) {
	valid := strings.Repeat("d", 32)
	buf := []byte(`{"id":"x"}` + `{"id":"yy"}` + `{"id":"zzz"}` + `{"id":"` + valid + `"}`)

	id, next, found := scanServerRequestID(buf, 0)
	if !found || id != valid {
		t.Fatalf("id=%q found=%v want %q true", id, found, valid)
	}
	// 从命中点之后续扫：合法 id 的值早已消费，其后无 needle 残余，安全返回 false
	// （不越界、不 panic、不重复命中）。
	if _, _, again := scanServerRequestID(buf, next+10); again {
		t.Error("rescan after hit should not find another id")
	}
}

// 集成（端到端）：DesktopChatWithExpert 的 SSE 扫描路径在首个候选非法时仍能
// 抓到其后合法的服务端 requestId。
func TestDesktopChatWithExpertFindsIDAfterInvalidCandidate(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	valid := "cmb-" + strings.Repeat("e", 32)
	sse := "data: {\"id\":\"invalid-not-32hex\",\"choices\":[]}\n\n" +
		"data: {\"id\":\"" + valid + "\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/chat/completions" {
			t.Errorf("path=%s want /v2/chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, ChatBaseGlobal: srv.URL, GlobalEnabled: true}
	c.SyncHot()
	_, reqID, err := c.DesktopChatWithExpert(globalAcct(), "expert-1")
	if err != nil {
		t.Fatalf("DesktopChatWithExpert: %v", err)
	}
	if reqID != valid {
		t.Errorf("requestID=%q want %q", reqID, valid)
	}
}

// ---------------------------------------------------------------------------
// R15-5 InNightWindow 固定 CST
// ---------------------------------------------------------------------------

// 回归：InNightWindow 不再依赖进程本地时区——now.In(cstShanghai) 固定 +8。
// UTC 15:00 == CST 23:00（窗口起点）；UTC 16:01 == CST 00:01（窗口内）；
// UTC 07:00 == CST 15:00（窗口外）。此前非 CST 主机上 23-08 本地 = 07-16 CST，
// 窗口错位、夜猫对话全部不计分。
func TestInNightWindowFixedCST(t *testing.T) {
	utc := time.FixedZone("UTC", 0)
	cases := []struct {
		utcHour, utcMin int
		want            bool
	}{
		{15, 0, true},   // CST 23:00 窗口起点
		{16, 1, true},   // CST 00:01
		{23, 59, true},  // CST 07:59 窗口终点（含）
		{7, 0, false},   // CST 15:00 窗口外
		{7, 59, false},  // CST 15:59 窗口外
		{8, 0, false},   // CST 16:00 窗口外
		{12, 0, false},  // CST 20:00 窗口外
		{14, 59, false}, // CST 22:59 窗口外（起点 23:00 前一分钟）
	}
	for _, tc := range cases {
		now := time.Date(2024, 1, 1, tc.utcHour, tc.utcMin, 0, 0, utc)
		if got := InNightWindow(now); got != tc.want {
			t.Errorf("InNightWindow(UTC %02d:%02d)=%v want %v (CST %02d:%02d)",
				tc.utcHour, tc.utcMin, got, tc.want, (tc.utcHour+8)%24, tc.utcMin)
		}
	}
	// 显式带时区的时间（如已 CST）不二次偏移。
	cst := time.FixedZone("CST", 8*60*60)
	if got := InNightWindow(time.Date(2024, 1, 1, 23, 0, 0, 0, cst)); !got {
		t.Error("InNightWindow(CST 23:00) should be true")
	}
	if got := InNightWindow(time.Date(2024, 1, 1, 12, 0, 0, 0, cst)); got {
		t.Error("InNightWindow(CST 12:00) should be false")
	}
}
