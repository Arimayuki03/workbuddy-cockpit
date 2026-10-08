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
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// recordingSink 流水记录的测试桩：捕获 RecordBalance 的入参序列。
type recordingSink struct {
	balances []balanceCall
}

type balanceCall struct {
	uid, nickname string
	credits       int64
}

func (r *recordingSink) RecordBalance(uid, nickname string, credits int64, at time.Time) int {
	r.balances = append(r.balances, balanceCall{uid, nickname, credits})
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
// 调 RecordBalanceChecked（漏接该钩子 = 该渠道到账失明，回归锁）。
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
	if len(sink.balances) != 1 || sink.balances[0].credits != 500 {
		t.Fatalf("签到路径流水记账缺失或参数错误：%+v", sink.balances)
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
