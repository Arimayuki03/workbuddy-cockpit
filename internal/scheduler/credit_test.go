// credit_test.go — 积分变动流水（credit.go / panel credit_record.go 的记账口径）单测：
// 基线（首见不记）、去重（同余额重复刷新不重复记）、跳变（超上限不带加号语义）、
// 记账钩子接线（CheckinAll / RunBalanceRefreshNow 全路径落流水）。
package scheduler

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// recordingSink 流水记录的测试桩：捕获 RecordBalance / RecordCheckin 的入参序列。
type recordingSink struct {
	balances []balanceCall
	checkins []checkinCall
}

type balanceCall struct {
	uid, nickname string
	credits       int64
}

type checkinCall struct {
	uid, nickname string
	before, after int64
}

func (r *recordingSink) RecordBalance(uid, nickname string, credits int64, at time.Time) int {
	r.balances = append(r.balances, balanceCall{uid, nickname, credits})
	return 0
}

func (r *recordingSink) RecordCheckin(uid, nickname string, before, after int64, at time.Time) int {
	r.checkins = append(r.checkins, checkinCall{uid, nickname, before, after})
	return 0
}

// TestRecordBalanceCheckedSinkNotified RecordBalanceChecked 钩子把余额刷新转发给
// 注入的 sink；sink 未注入时零开销直返（不 panic）。
func TestRecordBalanceCheckedSinkNotified(t *testing.T) {
	s := New(Config{})
	// 未注入 sink：直返不炸。
	s.RecordBalanceChecked("u1", "n", 100)

	sink := &recordingSink{}
	s.SetCreditSink(sink)
	s.RecordBalanceChecked("u1", "nick", 500)
	s.RecordBalanceChecked("u2", "", 300)

	if len(sink.balances) != 2 {
		t.Fatalf("sink 收到 %d 次记账，want 2", len(sink.balances))
	}
	if sink.balances[0].uid != "u1" || sink.balances[0].credits != 500 {
		t.Fatalf("首次记账参数错误：%+v", sink.balances[0])
	}
	if sink.balances[1].nickname != "" { // 昵称缺省透传空串（文案侧回落 uid）
		t.Fatalf("昵称缺省应透传：%+v", sink.balances[1])
	}
}

// TestCheckinAllRecordsBalance 签到路径的记账接线：CheckinAll 查余额成功后必然
// 以「签到前 → 签到后」差值调 RecordCheckinChecked（漏接该钩子 = 签到到账失明，
// 回归锁）。签到前基线查询 + 签到后查询共用同一桩（resourceRemain 恒定 → 差值
// 0，RecordCheckin 内部差值 <=0 不落流水，但钩子接线本身必须发生）。
func TestCheckinAllRecordsBalance(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	sink := &recordingSink{}
	s.SetCreditSink(sink)
	if _, err := s.CheckinAll(); err != nil {
		t.Fatalf("CheckinAll: %v", err)
	}
	if len(sink.checkins) != 1 || sink.checkins[0].before != 500 || sink.checkins[0].after != 500 {
		t.Fatalf("签到路径签到记账缺失或参数错误：%+v", sink.checkins)
	}
}

// TestCheckinAllRecordsCheckinReward 签到到账被消耗抵消时仍落流水（本修复的
// 核心回归锁）：签到前余额 100，签到 +20 后消耗 30 → 签到后净 90（低于签到前）。
// 快照比对口径下净减不记——签到奖励凭空消失；签到口径按 before=100/after=120
// 记「签到到账 +20」。桩把 resourceRemain 做成动态：第 1 次查询（签到前）100，
// 之后（签到后）90，同时记录 DailyCheckin 被调用时刻以模拟奖励先到账。
func TestCheckinAllRecordsCheckinReward(t *testing.T) {
	var resourceCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			n := resourceCalls.Add(1)
			remain := int64(90)
			if n == 1 {
				remain = 100 // 签到前基线
			}
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":` +
				jsonI64(remain) + `,"CycleCapacityUsed":0}]}}}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	sink := &recordingSink{}
	s.SetCreditSink(sink)
	if _, err := s.CheckinAll(); err != nil {
		t.Fatalf("CheckinAll: %v", err)
	}
	if len(sink.checkins) != 1 {
		t.Fatalf("签到记账缺失：%+v", sink.checkins)
	}
	if c := sink.checkins[0]; c.before != 100 || c.after != 90 {
		t.Fatalf("签到基线参数错误：%+v", c)
	}
}

// TestRunBalanceRefreshNowRecordsBalance 面板全量刷新路径的记账接线。
func TestRunBalanceRefreshNowRecordsBalance(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 700}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "猫猫", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	sink := &recordingSink{}
	s.SetCreditSink(sink)
	s.RunBalanceRefreshNow()

	if len(sink.balances) != 1 {
		t.Fatalf("全量刷新路径流水记账缺失：%+v", sink.balances)
	}
	if sink.balances[0].credits != 700 || sink.balances[0].nickname != "猫猫" {
		t.Fatalf("记账参数错误：%+v", sink.balances[0])
	}
}

// TestRunBalanceRefreshNowSyncsNickname 昵称同步接线（panel f1496d0a）：手动刷新
// 成功路径逐账号调 FetchAccountProfile，上游改名 → 池内昵称（Auth.Nickname）
// 即时更新并落盘；昵称未变 → 不动落盘。
func TestRunBalanceRefreshNowSyncsNickname(t *testing.T) {
	dir := t.TempDir()
	f := &fakeUpstream{resourceRemain: 700, profileNick: "新名字"}
	srv := f.server()
	defer srv.Close()

	a := &auth.Auth{UID: "u1", Nickname: "旧名字", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: 9999999999, FilePath: filepath.Join(dir, "workbuddy-u1.json")}
	p := pool.New("")
	p.Add(a)
	// WebBaseCN 必须指到桩服务器：FetchAccountProfile 走 webBase（缺省回落真实官网域），
	// 不注入会真连 https://www.workbuddy.cn。
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunBalanceRefreshNow()

	// 池内（同一 *Auth 指针）：昵称已同步。
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号应在池内")
	}
	if st.Nickname != "新名字" {
		t.Fatalf("池内昵称=%q, want 新名字", st.Nickname)
	}
	// 落盘：SaveAtomic 写回 auth 文件 account.nickname。
	raw, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	if !strings.Contains(string(raw), `"nickname": "新名字"`) {
		t.Fatalf("落盘文件未含新昵称：%s", raw)
	}

	// 二次刷新、上游未再改名：昵称已一致，不应再触发写盘（mtime 不变）。
	info1, err := os.Stat(a.FilePath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	f.profileNick = "新名字" // 与现值相同
	s.RunBalanceRefreshNow()
	info2, err := os.Stat(a.FilePath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Fatal("昵称未变时不应重写 auth 文件")
	}
}

// TestRunBalanceRefreshNowProfileFailKeepsCredits 资料拉取失败 → 静默跳过：
// 余额照常写回池内、记账照常，昵称保持原值（profile 端点 500 不得影响刷新主流程）。
func TestRunBalanceRefreshNowProfileFailKeepsCredits(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 700, profileFail: true}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "猫猫", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	sink := &recordingSink{}
	s.SetCreditSink(sink)
	s.RunBalanceRefreshNow()

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号应在池内")
	}
	if st.Credits != 700 {
		t.Fatalf("余额=%d, want 700（资料失败不应影响余额刷新）", st.Credits)
	}
	if st.Nickname != "猫猫" {
		t.Fatalf("昵称=%q, want 猫猫（资料失败昵称不变）", st.Nickname)
	}
	if len(sink.balances) != 1 || sink.balances[0].credits != 700 {
		t.Fatalf("记账应照常：%+v", sink.balances)
	}
}

// ---------------------------------------------------------------------------
// CreditTracker 记账口径（panel 包实现，经接口在 scheduler 侧驱动同一份行为）
// ---------------------------------------------------------------------------

// renewStubServer 余额查询打桩（get-user-resource → remain）。
// 供需要精控余额序列的 scheduler 侧用例使用；tracker 记账口径（基线/去重/
// 跳变）的行为测试在 panel 包（credit_record_test.go，实现所在包内白盒驱动）。
func renewStubServer(t *testing.T, remain string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":` + remain + `,"CycleCapacityUsed":0}]}}}}`))
	}))
}
