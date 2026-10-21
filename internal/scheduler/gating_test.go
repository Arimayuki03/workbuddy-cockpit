package scheduler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// globalResourceBody is a successful read-only billing/meter response carrying
// 100 usable credits. It must be wrapped once in the API envelope.
const globalResourceBody = `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CapacityRemain":100}]}}}}`

func newGlobalCreditServer(t *testing.T) (*httptest.Server, *atomic.Int32, *[]string) {
	t.Helper()
	var calls atomic.Int32
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, globalResourceBody)
	}))
	return srv, &calls, &paths
}

func newGlobalTestClient(srv *httptest.Server) *upstream.Client {
	return &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL,
		BillingBaseGlobal: srv.URL, GlobalEnabled: true}
}

// TestGlobalAccountsSkipCheckinTravel gating: global accounts must not call
// CN checkin/travel endpoints, but scheduled checkin performs exactly one
// read-only balance request so pool credits are not stale.
func TestGlobalAccountsSkipCheckinTravel(t *testing.T) {
	fastTravel(t)
	fastActivity(t)

	srv, calls, paths := newGlobalCreditServer(t)
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	s := New(Config{Pool: p, Upstream: newGlobalTestClient(srv)})

	s.CheckinAll()
	s.RunTravelNow()

	if n := calls.Load(); n != 1 {
		t.Fatalf("global checkin/travel upstream calls=%d want 1 (only read-only balance)", n)
	}
	if len(*paths) != 1 || !strings.Contains((*paths)[0], "/billing/meter/get-user-resource") {
		t.Errorf("global account made non-read-only calls: %v", *paths)
	}
	st, ok := p.Status("g1")
	if !ok || st.Credits != 100 {
		t.Errorf("global credits were not refreshed: ok=%v credits=%d", ok, st.Credits)
	}
}

// TestGlobalAccountSkippedListedWithStatus verifies the skipped(global) receipt
// still carries refreshed credits.
func TestGlobalAccountSkippedListedWithStatus(t *testing.T) {
	fastTravel(t)
	fastActivity(t)

	srv, calls, _ := newGlobalCreditServer(t)
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	s := New(Config{Pool: p, Upstream: newGlobalTestClient(srv)})

	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(out) != 1 || out[0].UID != "g1" {
		t.Fatalf("out=%+v want 1 row for g1", out)
	}
	if out[0].Status != CheckinSkipped {
		t.Errorf("status=%q want skipped (global gating)", out[0].Status)
	}
	if out[0].Detail != "global" {
		t.Errorf("detail=%q want global", out[0].Detail)
	}
	if out[0].Credits == nil || *out[0].Credits != 100 {
		t.Errorf("outcome credits=%v want 100", out[0].Credits)
	}
	if calls.Load() != 1 {
		t.Errorf("upstream calls=%d want 1 (only read-only balance)", calls.Load())
	}
}

// A read-only balance refresh must not clear an explicitly forced cooldown.
func TestGlobalCheckinDoesNotClearForcedCooldown(t *testing.T) {
	fastTravel(t)
	fastActivity(t)

	srv, calls, _ := newGlobalCreditServer(t)
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	p.Cooldown("g1", pool.CoolHard, time.Hour, "forced cooldown")
	s := New(Config{Pool: p, Upstream: newGlobalTestClient(srv)})

	if _, err := s.CheckinAll(); err != nil {
		t.Fatalf("checkin: %v", err)
	}
	st, ok := p.Status("g1")
	if !ok {
		t.Fatal("global account missing from pool")
	}
	if !st.Cooling || st.CoolKind != pool.CoolHard.String() {
		t.Fatalf("read-only credit refresh cleared forced cooldown: %+v", st)
	}
	if st.Credits != 100 {
		t.Errorf("credits=%d want 100 after read-only refresh", st.Credits)
	}
	if calls.Load() != 1 {
		t.Errorf("upstream calls=%d want 1", calls.Load())
	}
}

// TestGlobalAndCNMixedPoolServed 混池：activity **两类都上报**（global 不再跳过，
// PR #45 实测 workbuddy.ai /v2/report code=0 OK）；travel 仍只服务 CN（global 跳过）。
// 一次 fake upstream 同时统计两类账号真正打到的请求——区隔"放行"不是池空。
func TestGlobalAndCNMixedPoolServed(t *testing.T) {
	fastTravel(t)
	fastActivity(t)

	stub := &reportStub{} // /v2/report 统计 + X-User-Id 判定
	growth := &travelStub{buddy: "null"}
	// 合并 growth/billing 端点：activity 的 report + travel 的 buddy 都要能被两类账号打到。
	both := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/report" {
			stub.handler().ServeHTTP(w, r)
			return
		}
		growth.handler().ServeHTTP(w, r)
	}))
	defer both.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	up := &upstream.Client{HTTP: both.Client(), ChatBaseCN: both.URL, BillingBaseCN: both.URL,
		BillingBaseGlobal: both.URL, GlobalEnabled: true}
	s := New(Config{Pool: p, Upstream: up, ActivityReportCount: 1})

	s.RunActivityNow()
	// CN + global 各上报一次 + CN 无猫账号领养前置 report 一次（v1.2.0 adoptBuddy
	// 链路：report → agreement → buddy/first）= 3（global 放行上报，但不走领养）。
	if n := stub.calls.Load(); n != 3 {
		t.Errorf("activity report calls=%d want 3（CN+global 各 1 + CN 领养前置 1）", n)
	}
	// activity 阶段会顺带领猫（travelAdoptForce），把 buddy/info 计数打进快照；
	// 旅行检验只数 RunTravelNow 这段的增量（global 被 travel 跳过，只有 CN 走）。
	infoBefore := growth.infoCalls.Load()

	s.RunTravelNow()
	infoDelta := growth.infoCalls.Load() - infoBefore
	if infoDelta != 1 {
		t.Errorf("CN travel buddy-info delta=%d want 1（仅 CN 账号旅行）", infoDelta)
	}
}

// TestRunActivityNowReportsGlobalAccounts 核心验收：global 账号的 report 上报**不再跳过**
// （PR #45 实测国际版 /v2/report 在 workbuddy.ai 上 code=0 OK）。fake upstream 断言
// global 账号真的发出 /v2/report 请求；且按 realm 路由到 global billing base。
func TestRunActivityNowReportsGlobalAccounts(t *testing.T) {
	fastActivity(t)

	var globalCalls, cnCalls atomic.Int32
	reportOK := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/report" {
			t.Errorf("path=%s want /v2/report", r.URL.Path)
		}
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	})
	globalSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		globalCalls.Add(1)
		reportOK.ServeHTTP(w, r)
	}))
	defer globalSrv.Close()
	cnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cnCalls.Add(1)
		reportOK.ServeHTTP(w, r)
	}))
	defer cnSrv.Close()

	p := pool.New("")
	// 纯 global 池：若 report 仍被 IsGlobal() 跳过，globalSrv 计数保持 0。
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	up := &upstream.Client{HTTP: globalSrv.Client(), ChatBaseCN: cnSrv.URL, BillingBaseCN: cnSrv.URL,
		BillingBaseGlobal: globalSrv.URL, GlobalEnabled: true}
	s := New(Config{Pool: p, Upstream: up, ActivityReportCount: 1})

	s.RunActivityNow()

	if n := globalCalls.Load(); n != 1 {
		t.Errorf("global account report calls=%d want 1（global 上报不再跳过）", n)
	}
	if n := cnCalls.Load(); n != 0 {
		t.Errorf("global account 不应打到 CN base: cn_calls=%d", n)
	}
}

// TestRunActivityNowGlobalReportFailWarnsButOthersProceed global report 失败（如上游
// 不认 /v2/report）→ 记 WARN 不影响其他账号：同一趟里 CN 账号照常上报。
func TestRunActivityNowGlobalReportFailWarnsButOthersProceed(t *testing.T) {
	fastActivity(t)

	var cnCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/report":
			uid := r.Header.Get("X-User-Id")
			if uid == "g1" {
				http.Error(w, "no such report endpoint on global", 404)
				return
			}
			cnCalls.Add(1)
			w.Write([]byte(`{"code":0,"msg":"OK"}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "cn1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, GlobalEnabled: true}
	s := New(Config{Pool: p, Upstream: up, ActivityReportCount: 1})

	s.RunActivityNow() // 不应 panic，g1 的 report 失败只记 WARN

	if n := cnCalls.Load(); n != 1 {
		t.Errorf("CN account report calls=%d want 1（global 失败不影响 CN）", n)
	}
}

// TestRunKeepaliveStillRefreshesGlobal keepalive 不拦 global：token refresh 端点
// 对 global 存在，刷新调用照发。
func TestRunKeepaliveStillRefreshesGlobal(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "g1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1,
		Domain: "www.workbuddy.ai"}
	p.Add(a)

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow()
	if f.refreshCalls.Load() != 1 {
		t.Errorf("global keepalive refresh calls=%d want 1（keepalive 不拦 global）", f.refreshCalls.Load())
	}
	if a.AccessToken != "new" {
		t.Errorf("global token 未刷新: %s", a.AccessToken)
	}
}
