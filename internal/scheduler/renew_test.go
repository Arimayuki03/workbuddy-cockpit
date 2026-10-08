// renew_test.go — Token 独立续期巡检（renew.go）单测：
// 选号语义（面板手动停用照常续 / 自动禁用跳过 / 临期判定 / 无凭证跳过）、
// last_renewed / renew_last_error 的写入与持久化、SetRenewState 幂等。
package scheduler

import (
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

// fakeRenewStub 模拟 refresh / user-resource 端点（复用 fakeUpstream 的路径形状）。
type fakeRenewStub struct {
	refreshCalls   atomic.Int32
	resourceRemain int64
}

func (f *fakeRenewStub) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			f.refreshCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
}

// TestRenewEligible 选号判定表驱动：renewEligible 是 runRenewOnce 与本测试共用的
// 单一事实来源，这里的每个用例都直接约束生产语义。
func TestRenewEligible(t *testing.T) {
	window := 7 * 24 * time.Hour
	far := time.Now().Add(60 * 24 * time.Hour).Unix() // 远未临期
	near := time.Now().Add(24 * time.Hour).Unix()     // 窗口内临期
	past := time.Now().Add(-1 * time.Hour).Unix()     // 已过期
	cases := []struct {
		name     string
		disabled bool
		expires  int64
		rt       string
		want     bool
	}{
		{"healthy_far_future_skip", false, far, "rt", false},
		{"expiring_within_window_renew", false, near, "rt", true},
		{"expired_renew", false, past, "rt", true},
		{"no_expiry_renew", false, 0, "rt", true}, // 解不出到期时间：NeedsRefresh 恒 true（补一次无害）
		{"no_refresh_token_skip", false, near, "", false},
		{"auto_disabled_skip", true, near, "rt", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &auth.Auth{UID: "u", RefreshToken: tc.rt, ExpiresAt: tc.expires}
			got := renewEligible(poolStatusView{Disabled: tc.disabled, Window: window}, a)
			if got != tc.want {
				t.Fatalf("renewEligible(disabled=%v expires=%d)=%v want %v", tc.disabled, tc.expires, got, tc.want)
			}
		})
	}
}

// TestRunRenewSkipsDisabledRenewsManualDisabled 核心语义区分：
//   - 自动禁用（disabled 位，session dead 判死）→ 跳过，零上游调用；
//   - 面板手动停用（manualDisabled 位）→ 照常续期。
func TestRunRenewSkipsDisabledRenewsManualDisabled(t *testing.T) {
	f := &fakeRenewStub{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "auto", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1})
	p.Add(&auth.Auth{UID: "manual", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1})
	p.Disable("auto", "12153 session dead")      // 自动禁用
	p.SetManualDisabled("manual", true, "panel") // 面板手动停用

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, ExpiringSoonWindow: 7 * 24 * time.Hour})
	s.runRenewOnce()

	// 两个账号都临期（ExpiresAt=1）：只有手动停用的被续，自动禁用的零调用。
	if n := f.refreshCalls.Load(); n != 1 {
		t.Fatalf("refresh calls=%d want 1（仅手动停用账号续期，自动禁用跳过）", n)
	}
	if st, ok := p.Status("manual"); !ok || !st.ManualDisabled {
		t.Fatalf("手动停用位不应被续期改动：%+v", st)
	}
}

// TestRunRenewUpdatesLastRenewed 临期账号续期成功后 last_renewed 更新、失败原因清空。
func TestRunRenewUpdatesLastRenewed(t *testing.T) {
	f := &fakeRenewStub{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1})
	p.SetRenewState("u1", errRenewFake) // 预置一条失败记录，成功后应被清空

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.runRenewOnce()

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号应仍在池内")
	}
	if st.LastRenewed.IsZero() {
		t.Fatal("续期成功后 last_renewed 不应为零值")
	}
	if since := time.Since(st.LastRenewed); since < 0 || since > time.Minute {
		t.Fatalf("last_renewed=%v 不是刚刚（%v）", st.LastRenewed, since)
	}
	if st.RenewLastError != "" {
		t.Fatalf("成功后 renew_last_error 应清空，实得 %q", st.RenewLastError)
	}
	if a := p.AuthByUID("u1"); a.AccessToken != "new" {
		t.Fatalf("token 未刷新：%s", a.AccessToken)
	}
}

// errRenewFake 测试用的续期失败原因（区别于空串与真实错误文本）。
var errRenewFake = errorString("fake refresh failure (test)")

type errorString string

func (e errorString) Error() string { return string(e) }

// TestRunRenewFailureRecorded 续期失败只记 renew_last_error、不动 last_renewed。
func TestRunRenewFailureRecorded(t *testing.T) {
	// refresh 端点 500 → RefreshToken 失败。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1})

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.runRenewOnce()

	st, _ := p.Status("u1")
	if !st.LastRenewed.IsZero() {
		t.Fatalf("失败不应记 last_renewed，实得 %v", st.LastRenewed)
	}
	if st.RenewLastError == "" {
		t.Fatal("失败应留 renew_last_error")
	}
}

// TestSetRenewStatePersistsAcrossRestart SetRenewState 落盘 → 重启恢复往返：
// last_renewed 与 renew_last_error 都持久化（观测跨重启不失忆）。
func TestSetRenewStatePersistsAcrossRestart(t *testing.T) {
	fp := t.TempDir() + "/state.json"
	p := pool.New(fp)
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	p.SetRenewState("u1", errRenewFake)
	p.Flush()

	p2 := pool.New(fp)
	defer p2.Close()
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("恢复后账号应存在")
	}
	if st.RenewLastError != string(errRenewFake) {
		t.Fatalf("renew_last_error 未恢复：%q", st.RenewLastError)
	}
	if !st.LastRenewed.IsZero() {
		t.Fatalf("失败路径不应有 last_renewed：%v", st.LastRenewed)
	}
}

// TestSetRenewStateSuccessPersists 成功态的 last_renewed 往返。
func TestSetRenewStateSuccessPersists(t *testing.T) {
	fp := t.TempDir() + "/state.json"
	p := pool.New(fp)
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	p.SetRenewState("u1", nil)
	p.Flush()

	p2 := pool.New(fp)
	defer p2.Close()
	st, _ := p2.Status("u1")
	if st.LastRenewed.IsZero() {
		t.Fatal("last_renewed 未恢复")
	}
	if st.RenewLastError != "" {
		t.Fatalf("成功态不应带失败原因：%q", st.RenewLastError)
	}
}

// TestRenewDisabledByDefaultAndScheduled opt-in 缺省关；RenewEnabled=true 时
// 按 renew_hours 进入 nextWake 候选（与 queue 同风格）。
func TestRenewDisabledByDefaultAndScheduled(t *testing.T) {
	s := newTestSched(t, Config{})
	snap := s.SnapshotAll()
	sn := snap[taskRenew]
	if sn.Kind != "renew" || sn.Enabled || sn.NextFire != "" {
		t.Fatalf("renew 缺省应禁用：%+v", sn)
	}
	// 显式开启：NextFire 落在 renew_hours（默认 3 点）。
	s2 := newTestSched(t, Config{RenewEnabled: true})
	snap2 := s2.SnapshotAll()
	sn2 := snap2[taskRenew]
	if !sn2.Enabled || sn2.NextFire == "" {
		t.Fatalf("RenewEnabled=true 应进排程：%+v", sn2)
	}
	if ts, err := time.Parse(time.RFC3339, sn2.NextFire); err != nil || ts.Hour() != 3 {
		t.Fatalf("renew next_fire 应在 3 点：%q (err=%v)", sn2.NextFire, err)
	}
}
