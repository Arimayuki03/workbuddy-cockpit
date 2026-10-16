// compensation_test.go v1.19.1「活动补偿一键领取」的调度侧契约：
//   - 账号口径：自动禁用 / 面板手动停用 / 国际版 / 无凭据一律跳过，且**零上游请求**；
//   - 幂等：状态显示「已领过」→ 不发领取请求；「没有可领」→ 计数为 nothing 而非失败；
//   - 状态查询失败（业务错误）不阻断领取（认不出 ≠ 领过了）；
//   - 手动「立即执行」不加账号错峰延后（错峰只在定时路径生效）；ctx 可取消；
//   - 到点跑：compensation 族按与其他任务族同一套槽位结构接入（nextWake/dueBatches），
//     开关开着但小时表为空 = 不排程（等价「只手动」）。
package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// compStub 活动补偿假上游：默认状态=未领过、领取=到账 300 积分；
// 记录调用次数与被请求的账号 uid（按 X-User-Id 头）。
type compStub struct {
	statusBody string
	claimBody  string
	statuses   atomic.Int32
	claims     atomic.Int32
	mu         sync.Mutex
	uids       []string
}

func newCompStub() *compStub {
	return &compStub{
		statusBody: `{"code":0,"data":{"claimed":false,"credit_num":300}}`,
		claimBody:  `{"code":0,"data":{"credit":300}}`,
	}
}

func (s *compStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.uids = append(s.uids, r.Header.Get("X-User-Id"))
		s.mu.Unlock()
		switch r.URL.Path {
		case "/billing/meter/compensation-status":
			s.statuses.Add(1)
			w.Write([]byte(s.statusBody))
		case "/billing/meter/claim-compensation":
			s.claims.Add(1)
			w.Write([]byte(s.claimBody))
		default:
			// 带 /v2 前缀的路径打不中：端点拼错会在这里变成 404（线上表现为「无可领」）。
			w.WriteHeader(404)
			w.Write([]byte(`{"code":404,"msg":"not found"}`))
		}
	})
}

// newCompSched 构造补偿 stub 的调度器与空池；账号间限速置 0（测试不真睡）。
func newCompSched(t *testing.T, srv *httptest.Server) (*Scheduler, *pool.Pool) {
	t.Helper()
	old := compensationAccountDelay
	compensationAccountDelay = 0
	t.Cleanup(func() { compensationAccountDelay = old })

	p := pool.New("")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up}), p
}

// TestCompensationSkipsDisabledManualAndGlobal 账号口径：只处理可用的国内版账号；
// 自动禁用 / 面板手动停用 / 国际版一律跳过且**零上游请求**（请求数=1 状态 +1 领取）。
func TestCompensationSkipsDisabledManualAndGlobal(t *testing.T) {
	auth.SetGlobalEnabled(true)
	stub := newCompStub()
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, p := newCompSched(t, srv)

	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "d1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "m1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "n1", AccessToken: "at", Domain: "www.codebuddy.cn"}) // 有 token 但无 refreshToken 不拦：见下
	p.Disable("d1", "session dead")
	p.SetManualDisabled("m1", true, "panel")

	out := s.RunCompensation(context.Background())
	// n1 有 AccessToken（补偿只要 access token，不需要 refresh），故与 u1 一起被处理。
	if out.Accounts != 2 || out.Claimed != 2 || out.Credits != 600 {
		t.Errorf("out=%+v want accounts=2 claimed=2 credits=600", out)
	}
	if out.Skipped != 3 || out.Failed != 0 || out.Already != 0 || out.Nothing != 0 {
		t.Errorf("out=%+v want skipped=3 failed=0 already=0 nothing=0", out)
	}
	if !out.OK() {
		t.Errorf("无可领/跳过都不是失败，OK() 应为 true：%+v", out)
	}
	if n := stub.statuses.Load(); n != 2 {
		t.Errorf("status calls=%d want 2（禁用/停用/国际版不得发请求）", n)
	}
	if n := stub.claims.Load(); n != 2 {
		t.Errorf("claim calls=%d want 2", n)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	for _, uid := range stub.uids {
		if uid == "d1" || uid == "m1" || uid == "g1" || uid == "" {
			t.Errorf("被跳过/无标识的账号发出了请求：uids=%v", stub.uids)
		}
	}
}

// TestCompensationNothingIsNotFailure 上游业务拒绝（没有可领 / 已经领过）→ nothing 计数，
// **不计失败**（界面不能把「今天本来就没得领」报成「N 个账号失败」）。
func TestCompensationNothingIsNotFailure(t *testing.T) {
	stub := newCompStub()
	stub.claimBody = `{"code":40001,"msg":"no compensation"}`
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, p := newCompSched(t, srv)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"})

	out := s.RunCompensation(context.Background())
	if out.Accounts != 1 || out.Nothing != 1 || out.Failed != 0 || out.Claimed != 0 || out.Credits != 0 {
		t.Errorf("out=%+v want accounts=1 nothing=1 failed=0 claimed=0 credits=0", out)
	}
	if !out.OK() {
		t.Error("「无可领」是正常态，OK() 应为 true")
	}
	if out.Summary() != "1 个无可领" {
		t.Errorf("summary=%q want 1 个无可领", out.Summary())
	}
}

// TestCompensationAlreadyClaimedSkipsClaim 状态显示「已领过」→ 不发领取请求（幂等的
// 第二道闸：省一次注定失败的写请求），计入 already 而非尝试数。
func TestCompensationAlreadyClaimedSkipsClaim(t *testing.T) {
	stub := newCompStub()
	stub.statusBody = `{"code":0,"data":{"claimed":true,"claimed_at":1760000000}}`
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, p := newCompSched(t, srv)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"})

	out := s.RunCompensation(context.Background())
	if n := stub.claims.Load(); n != 0 {
		t.Errorf("claim calls=%d want 0（已领过不再打写接口）", n)
	}
	if out.Accounts != 0 || out.Already != 1 || out.Claimed != 0 {
		t.Errorf("out=%+v want accounts=0 already=1 claimed=0", out)
	}
	if out.Summary() != "1 个已领过" {
		t.Errorf("summary=%q want 1 个已领过", out.Summary())
	}
}

// TestCompensationStatusFailureStillClaims 状态查询失败（业务错误）→ **照常尝试领取**
//（认不出/查不到不等于领过了，把有时限的机会放过更糟），失败只体现在日志里。
func TestCompensationStatusFailureStillClaims(t *testing.T) {
	stub := newCompStub()
	stub.statusBody = `{"code":10001,"msg":"x"}`
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, p := newCompSched(t, srv)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"})

	out := s.RunCompensation(context.Background())
	if n := stub.claims.Load(); n != 1 {
		t.Errorf("claim calls=%d want 1（状态查询失败不阻断领取）", n)
	}
	if out.Claimed != 1 || out.Credits != 300 || out.Failed != 0 {
		t.Errorf("out=%+v want claimed=1 credits=300 failed=0", out)
	}
}

// TestCompensationClaimFailureCountsAndSummarizes 领取失败（401 token 失效）→ failed 计数 +
// 首个原因进 message（面板 ok=false 时要能说明原因）。刻意不用 5xx 夹具：那会走上游侧
// 瞬时重试（2s+4s 真睡），把本用例拖到 6 秒；5xx 的重试计数在 upstream 包已钉住。
func TestCompensationClaimFailureCountsAndSummarizes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/compensation-status") {
			w.Write([]byte(`{"code":0,"data":{}}`))
			return
		}
		w.WriteHeader(401)
		w.Write([]byte(`{"code":12153,"msg":"offline session"}`))
	}))
	defer srv.Close()
	s, p := newCompSched(t, srv)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"})

	out := s.RunCompensation(context.Background())
	if out.Failed != 1 || out.OK() {
		t.Errorf("out=%+v want failed=1 且 OK()=false", out)
	}
	if out.Accounts != 1 {
		t.Errorf("accounts=%d want 1（失败也是一次尝试，与 wbm tried 分母同口径）", out.Accounts)
	}
	if out.FirstErr == "" || !strings.Contains(out.Summary(), "1 个失败") {
		t.Errorf("失败原因未进摘要：err=%q summary=%q", out.FirstErr, out.Summary())
	}
}

// TestCompensationManualRunNotStaggered 手动「立即执行」不加账号错峰延后：
// 错峰区间配成「固定 1 秒/1 秒」（accountDelayUnit 默认秒）时，两号仍毫秒级跑完——
// 若手动路径误带 scheduledAccountKey 标记，两号之间会实等满 1 秒（测试会超时失败）。
func TestCompensationManualRunNotStaggered(t *testing.T) {
	stub := newCompStub()
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	p := pool.New("")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	old := compensationAccountDelay
	compensationAccountDelay = 0 // 只看错峰增量（base 会被两个路径都加上）
	t.Cleanup(func() { compensationAccountDelay = old })
	s := New(Config{Pool: p, Upstream: up, AccountDelayMinSeconds: 1, AccountDelayMaxSeconds: 1})
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", Domain: "www.codebuddy.cn"})

	started := time.Now()
	out := s.RunCompensation(context.Background())
	elapsed := time.Since(started)
	if out.Claimed != 2 {
		t.Fatalf("out=%+v want claimed=2", out)
	}
	if elapsed > 900*time.Millisecond {
		t.Fatalf("手动路径等了 %v：错峰只该作用于定时派发（手动「立即执行」不延后）", elapsed)
	}
}

// TestCompensationCanceledCtxStops 可取消 ctx：已取消的 ctx 下不发任何请求，
// 汇总标记 Canceled（面板据此报「已取消」而不是把半程结果当完整结果）。
func TestCompensationCanceledCtxStops(t *testing.T) {
	stub := newCompStub()
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, p := newCompSched(t, srv)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := s.RunCompensation(ctx)
	if !out.Canceled || out.OK() {
		t.Errorf("out=%+v want Canceled=true 且 OK()=false", out)
	}
	if stub.statuses.Load() != 0 || stub.claims.Load() != 0 {
		t.Errorf("取消后不得发请求：status=%d claim=%d", stub.statuses.Load(), stub.claims.Load())
	}
	if !strings.Contains(out.Summary(), "已取消") {
		t.Errorf("summary=%q 应说明已取消", out.Summary())
	}
}

// TestCompensationScheduleSlotWiring compensation 族接入既有槽位结构：
// 开启且配了整点 → nextWake/dueBatches 把它当普通任务族排程；开关开着但小时表为空
// → 不排程（等价「只手动」）。其余任务族全禁用，夹具只观察 compensation。
func TestCompensationScheduleSlotWiring(t *testing.T) {
	fix := func(hours []int) *Scheduler {
		return New(Config{
			CompensationEnabled: true,
			CompensationHours:   hours,
			CheckinDisabled:     true, TravelDisabled: true, ActivityDisabled: true,
			KeepaliveDisabled: true, SchoolDisabled: true, CatDisabled: true,
		})
	}
	s := fix([]int{9})
	now := time.Date(2026, 10, 16, 8, 30, 0, 0, cstZone)
	at, kinds := s.nextWake(now)
	want := time.Date(2026, 10, 16, 9, 0, 0, 0, cstZone)
	if !at.Equal(want) || len(kinds) != 1 || kinds[0] != taskCompensation {
		t.Fatalf("nextWake=%s %v want %s [compensation]", at, kinds, want)
	}
	// 到点补跑：09:05 醒来时 09:00 那一槽必须被派发（首次补跑 planned=at）。
	batches := s.dueBatches(now.Add(35*time.Minute), time.Time{}, at)
	if len(batches) != 1 || !batches[0].at.Equal(want) ||
		len(batches[0].kinds) != 1 || batches[0].kinds[0] != taskCompensation {
		t.Fatalf("dueBatches=%+v want 一批 09:00 [compensation]", batches)
	}
	// 已派发（cursor 前移）→ 不重放。
	if again := s.dueBatches(now.Add(35*time.Minute), want, at); len(again) != 0 {
		t.Fatalf("已派发槽位被重放：%+v", again)
	}
	// 开关开着但小时表为空 = 只手动：不排程（不回落默认整点）。
	empty := fix(nil)
	if at, kinds := empty.nextWake(now); !at.IsZero() || len(kinds) != 0 {
		t.Fatalf("空小时表应不排程，得到 %s %v", at, kinds)
	}
	// 缺省（开关关）同样不排程。
	off := New(Config{CheckinDisabled: true, TravelDisabled: true, ActivityDisabled: true,
		KeepaliveDisabled: true, SchoolDisabled: true, CatDisabled: true})
	if at, kinds := off.nextWake(now); !at.IsZero() || len(kinds) != 0 {
		t.Fatal("开关缺省关时不该排程 compensation")
	}
	// /admin 手动触发路径：kind 名与枚举对齐（RunKindNow/SetEnabled 都吃这个名字）。
	if k, ok := KindFromName("compensation"); !ok || k != taskCompensation {
		t.Errorf("KindFromName(compensation)=%v,%v want %v,true", k, ok, taskCompensation)
	}
}

// cancelOnClaimSrv 包装补偿 stub：指定 uid 的**领取**请求到达时取消 ctx
// （响应照常返回；upstream 补偿调用不带 ctx，取消不会打断在途请求——模拟
// 「停机信号恰好落在某号的处理期间」这一时序，而不是处理前/请求失败）。
func cancelOnClaimSrv(stub *compStub, uid string, cancel context.CancelFunc) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/claim-compensation") && r.Header.Get("X-User-Id") == uid {
			cancel()
		}
		stub.handler().ServeHTTP(w, r)
	}))
}

// TestCompensationCanceledOnLastAccountNotCanceled 取消恰好落在**最后一个账号**的
// 处理期间：全部账号其实都已处理完，Canceled 必须保持 false（OK() 为真），Summary
// 不得出现「已取消（剩余账号未处理）」——否则面板把完整结果误报成半程结果。
func TestCompensationCanceledOnLastAccountNotCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stub := newCompStub()
	srv := cancelOnClaimSrv(stub, "u1", cancel)
	defer srv.Close()
	s, p := newCompSched(t, srv)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"})

	out := s.RunCompensation(ctx)
	if out.Canceled {
		t.Fatalf("最后一个账号已领完，Canceled 不得置真：%+v", out)
	}
	if !out.OK() || out.Claimed != 1 || out.Accounts != 1 {
		t.Fatalf("out=%+v want claimed=1 accounts=1 OK()=true", out)
	}
	if strings.Contains(out.Summary(), "已取消") {
		t.Fatalf("summary=%q 不得出现「已取消」（没有剩余账号）", out.Summary())
	}
	if n := stub.claims.Load(); n != 1 {
		t.Errorf("claim calls=%d want 1（取消在领取之后才被观察，本号照常领完）", n)
	}
}

// TestCompensationCanceledOnMiddleAccountStops 取消落在**中间账号**的处理期间：
// 后面还有账号没轮到 → Canceled 置真、OK()=false，后续账号一个请求都不发。
func TestCompensationCanceledOnMiddleAccountStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stub := newCompStub()
	srv := cancelOnClaimSrv(stub, "u2", cancel)
	defer srv.Close()
	s, p := newCompSched(t, srv)
	for _, uid := range []string{"u1", "u2", "u3"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", Domain: "www.codebuddy.cn"})
	}

	out := s.RunCompensation(ctx)
	if !out.Canceled || out.OK() {
		t.Fatalf("中间账号取消后还有 u3 未处理：out=%+v want Canceled=true OK()=false", out)
	}
	if out.Claimed != 2 || out.Accounts != 2 {
		t.Fatalf("out=%+v want 前两号已领完（claimed=2 accounts=2）", out)
	}
	if n := stub.claims.Load(); n != 2 {
		t.Errorf("claim calls=%d want 2（u3 不得再发请求）", n)
	}
	if n := stub.statuses.Load(); n != 2 {
		t.Errorf("status calls=%d want 2（u3 不得再发请求）", n)
	}
	if !strings.Contains(out.Summary(), "已取消") {
		t.Errorf("summary=%q 应说明已取消", out.Summary())
	}
}
