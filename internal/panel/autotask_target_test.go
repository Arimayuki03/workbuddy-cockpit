package panel

// autotask_target_test.go accept 后回读真实 target（吸收 workbuddy-panel d9edc067）：
// accept **之前**上游下发 progress=null（target=0），按兜底 target=1 会少报事件，并与
// 陈旧 target 比较误判达标 → claim 返回 400 task not completed（panel 首轮实测
// Sequential_Tasks_6 真实 target=10：进度 0→1 即触发误领）。三处 accept 点
// （runMPMiniChatTask / runMiniExpert / runSequentialEventTask）都须在 accept 登记生效
// 后回读真实 target 参与「补报数量/达标判定」；回读失败保持旧兜底逻辑（不阻塞、不把
// 读失败升级成错误）。这里用 httptest 假上游按「列表调用序号」喂出实测读写序列。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// stubTaskCode 用例默认任务码；stubRealTarget 对齐 d9edc067 实测的真实 target。
const (
	stubTaskCode   = "Sequential_Tasks_1"
	stubRealTarget = 10
)

// mpTaskStubState 单次任务列表调用的状态（accept 前后进度字段不同形）。
type mpTaskStubState struct {
	target       int64  // 真实 target（0 = 上游下发 progress=null）
	current      int64  // 固定 current（trackReports=true 时忽略）
	trackReports bool   // true：current = min(已上报条数, target)——服务端按条数累加进度
	acceptStatus string // accept_status（"" 视为 not_accepted）
	missing      bool   // true：列表不含该任务（mp 口径未下发）
	status       int    // 非 0：该次列表调用返回该 HTTP 状态（模拟回读失败）
}

// mpTaskStub 假 mp 上游：列表按序喂状态，统计 accept/上报/领奖次数。
type mpTaskStub struct {
	mu      sync.Mutex
	code    string
	script  []mpTaskStubState
	lists   int
	accepts int
	reports int
	claims  int
}

// stubScriptAcceptThenRealTarget 对齐 d9edc067 实测的读序列：
//  1. accept 前列表：progress=null（target=0）、not_accepted；
//  2. acceptWithVerifyMP 的登记回读：accept 已登记，但进度字段此刻仍为 null
//     （accept 只是「报名」，不产生进度）；
//  3. 之后（本次修复新增的回读 + 回读轮询）：真实 target=10，current 随上报条数累加。
func stubScriptAcceptThenRealTarget() []mpTaskStubState {
	return []mpTaskStubState{
		{acceptStatus: "not_accepted"},
		{acceptStatus: "accepted"},
		{target: stubRealTarget, trackReports: true, acceptStatus: "accepted"},
	}
}

// nextListState 取本次列表调用的状态：按 script 序号推进，耗尽后沿用最后一条
// （回读轮询会重复读同一稳态）。
func (s *mpTaskStub) nextListState() mpTaskStubState {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists++
	i := s.lists - 1
	if i >= len(s.script) {
		i = len(s.script) - 1
	}
	return s.script[i]
}

// counts 返回 (上报条数, 领奖次数, accept 次数) 快照。
func (s *mpTaskStub) counts() (reports, claims, accepts int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reports, s.claims, s.accepts
}

// stubTaskJSON 任务列表元素：progress 恒 null（对齐 accept 前上游形状），
// target/current 平铺以便同一形状复用。
type stubTaskJSON struct {
	TaskCode     string `json:"task_code"`
	Progress     any    `json:"progress"`
	Target       int64  `json:"target"`
	Current      int64  `json:"current"`
	AcceptStatus string `json:"accept_status"`
}

// stubOK 以 code=0 信封写成功响应（upstream 客户端只认 code=0）。
func stubOK(w http.ResponseWriter, data string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"code":0,"msg":"OK","data":%s}`, data)
}

func (s *mpTaskStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/tasks/accept"):
			s.mu.Lock()
			s.accepts++
			s.mu.Unlock()
			stubOK(w, `{}`)
		case r.URL.Path == "/v2/activity/growth/tasks":
			st := s.nextListState()
			if st.status != 0 { // 回读失败形态：连接可用但响应 5xx
				w.WriteHeader(st.status)
				fmt.Fprintf(w, `{"code":%d,"msg":"stub failure"}`, st.status)
				return
			}
			if st.missing {
				stubOK(w, `{"tasks":[]}`)
				return
			}
			cur := st.current
			if st.trackReports {
				reports, _, _ := s.counts()
				cur = int64(reports)
				if cur > st.target {
					cur = st.target
				}
			}
			body, _ := json.Marshal([]stubTaskJSON{{
				TaskCode:     s.code,
				Progress:     nil,
				Target:       st.target,
				Current:      cur,
				AcceptStatus: st.acceptStatus,
			}})
			stubOK(w, `{"tasks":`+string(body)+`}`)
		case r.URL.Path == "/v2/report": // ReportMPEvent（判据事件上报）
			s.mu.Lock()
			s.reports++
			s.mu.Unlock()
			stubOK(w, `{}`)
		case r.URL.Path == "/portal/operation-platform/market/expert/list":
			stubOK(w, `{"experts":[{"expert_id":"ex_1","expert_type":"agent","display_name_zh":"测试专家"}]}`)
		case strings.HasSuffix(r.URL.Path, "/claim"):
			s.mu.Lock()
			s.claims++
			s.mu.Unlock()
			stubOK(w, `{"already_claimed":false,"credit":100,"energy":5}`)
		default:
			http.NotFound(w, r)
		}
	})
}

// fastMPPacing 把 mp 动作节奏（accept 间隔/补报间隔与抖动/回读轮询）压到 0，让补报
// 路径在测试里秒级跑完；t.Cleanup 还原，避免污染同包其它用例（本包测试不并行）。
func fastMPPacing(t *testing.T) {
	t.Helper()
	gap, jitter, action, poll := mpChatEventGap, mpChatEventJitter, mpActionGap, claimPollGap
	mpChatEventGap, mpChatEventJitter, mpActionGap, claimPollGap = 0, 0, 0, 0
	t.Cleanup(func() {
		mpChatEventGap, mpChatEventJitter, mpActionGap, claimPollGap = gap, jitter, action, poll
	})
}

// newMPTaskPanel 以假 mp 上游装配最小 Panel（同 taskcenter_school_only_test.go 的
// httptest + 三 base 注入方式），供 autotask 动作直调。
func newMPTaskPanel(t *testing.T, s *mpTaskStub) *Panel {
	t.Helper()
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
		WebBaseCN:     srv.URL,
	}
	up.SyncHot() // 热改快照与结构体字段同步（读侧走 HotFields）
	return New(Config{Version: "test", Upstream: up})
}

// stubAuth 最小 CN 域账号（mp 事件仅 CN 域；global 会被 ReportMPEvent 前置拒绝）。
func stubAuth() *auth.Auth {
	return &auth.Auth{AccessToken: "at", UID: "u1", Nickname: "测试号"}
}

// TestMPTaskTargetFallback target 兜底口径表驱动：accept 前 progress=null（target=0）
// 与畸形值一律回落 1，不 panic；正 target 原样透出。
func TestMPTaskTargetFallback(t *testing.T) {
	cases := []struct {
		name string
		in   *upstream.Task
		want int64
	}{
		{"nil 任务防御性回落 1", nil, 1},
		{"progress=null（target=0）回落 1", &upstream.Task{Target: 0}, 1},
		{"负数畸形值回落 1", &upstream.Task{Target: -3}, 1},
		{"target=1 原样", &upstream.Task{Target: 1}, 1},
		{"真实 target=10 原样", &upstream.Task{Target: stubRealTarget}, stubRealTarget},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := taskTarget(c.in); got != c.want {
				t.Errorf("taskTarget=%d want %d", got, c.want)
			}
		})
	}
}

// TestMPChatEventDelayZeroJitterNoPanic 抖动上限置 0 时补报等待退化为固定 gap，
// 且不触发 rand.Int64N(0)（该参数会 panic——测试置 0 的路径必须安全）。
func TestMPChatEventDelayZeroJitterNoPanic(t *testing.T) {
	fastMPPacing(t)
	if got := mpChatEventDelay(); got != 0 {
		t.Errorf("gap/jitter 置 0 后等待应为 0，got %v", got)
	}
}

// TestMPRefreshTaskAfterAcceptRead 回读 helper 的口径：新 target>0 才换用新任务
// （含 accept 后真实 target 与 accept 前 null 的差异）；读失败/任务未下发/仍为 null
// 一律保留原任务（不阻塞、不升级错误、不把已有 target 回退成 0）。
func TestMPRefreshTaskAfterAcceptRead(t *testing.T) {
	cases := []struct {
		name        string
		script      []mpTaskStubState
		oldTarget   int64
		oldCurrent  int64
		wantSame    bool // true：返回原任务对象（未换用）
		wantTarget  int64
		wantCurrent int64
	}{
		{
			name:       "accept 后回读到真实 target=10 换用新任务",
			script:     []mpTaskStubState{{target: stubRealTarget, current: 2, acceptStatus: "accepted"}},
			wantSame:   false,
			wantTarget: stubRealTarget, wantCurrent: 2,
		},
		{
			name:       "回读仍是 progress=null（target=0）保留旧任务",
			script:     []mpTaskStubState{{acceptStatus: "accepted"}},
			wantSame:   true,
			wantTarget: 0, wantCurrent: 0,
		},
		{
			name:       "回读失败（500）保留旧任务",
			script:     []mpTaskStubState{{status: http.StatusInternalServerError}},
			wantSame:   true,
			wantTarget: 0, wantCurrent: 0,
		},
		{
			name:       "任务未下发（列表无该码）保留旧任务",
			script:     []mpTaskStubState{{missing: true}},
			wantSame:   true,
			wantTarget: 0, wantCurrent: 0,
		},
		{
			name:      "旧 target=10、回读 target=0 不回退已有进度目标",
			script:    []mpTaskStubState{{target: 0, current: 3, acceptStatus: "accepted"}},
			oldTarget: stubRealTarget, oldCurrent: 3,
			wantSame:   true,
			wantTarget: stubRealTarget, wantCurrent: 3,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &mpTaskStub{code: stubTaskCode, script: c.script}
			p := newMPTaskPanel(t, s)
			old := &upstream.Task{TaskCode: stubTaskCode, Target: c.oldTarget, Current: c.oldCurrent}
			got := p.refreshTaskAfterAcceptMP(stubAuth(), stubTaskCode, old)
			if got == nil {
				t.Fatal("回读 helper 不应返回 nil")
			}
			if c.wantSame && got != old {
				t.Errorf("读失败/无效 target 应保留原任务对象，got %+v", got)
			}
			if !c.wantSame && got == old {
				t.Error("回读到有效 target 应换用新任务对象")
			}
			if got.Target != c.wantTarget || got.Current != c.wantCurrent {
				t.Errorf("回读结果 target/current=%d/%d want %d/%d",
					got.Target, got.Current, c.wantTarget, c.wantCurrent)
			}
		})
	}
}

// TestAutoAcceptReReadTargetAcrossSites 三处 accept 点（runMPMiniChatTask /
// runSequentialEventTask / runMiniExpert）共用同一实测读序列：accept 前 target=0、
// accept 后回读 target=10。补报条数与是否误判达标都以回读值为准：
//   - runMPMiniChatTask 按差额补报：10 条（旧逻辑只补 1 条）；
//   - 单条判据的两个骨架：上报 1 条 < 10，不得误判达标去 claim（旧逻辑会领到
//     假上游的奖励，真实上游则 400 task not completed）；
//   - 回读失败时保持旧兜底（补 1 条、不阻塞、不把读失败升级成错误）。
func TestAutoAcceptReReadTargetAcrossSites(t *testing.T) {
	// 单条判据 + 真实 target=10：用于回读 target 重算的骨架用例。
	oneMPReport := func(code string) func(p *Panel, a *auth.Auth) (string, error) {
		return func(p *Panel, a *auth.Auth) (string, error) {
			return p.runSequentialEventTask(a, code, func() error {
				return p.cfg.Upstream.ReportMPEvent(a, upstream.SchoolChatTimesEvents("wb2api-stub"))
			}, nil)
		}
	}
	failingReReadScript := []mpTaskStubState{
		{acceptStatus: "not_accepted"},
		{acceptStatus: "accepted"},
		{status: http.StatusInternalServerError}, // 第 3 次列表 = 修复新增的回读
		{target: stubRealTarget, trackReports: true, acceptStatus: "accepted"},
	}
	cases := []struct {
		name        string
		code        string
		script      []mpTaskStubState
		run         func(p *Panel, a *auth.Auth) (string, error)
		wantReports int
		wantClaims  int
		wantMsg     string
	}{
		{
			name: "runMPMiniChatTask：accept 后按回读 target=10 补满 10 条并领奖",
			code: stubTaskCode,
			run: func(p *Panel, a *auth.Auth) (string, error) {
				return p.runMPMiniChatTask(a, stubTaskCode, false)
			},
			wantReports: stubRealTarget, wantClaims: 1, wantMsg: "任务点亮并领取奖励",
		},
		{
			name: "runSequentialEventTask：回读 target=10，单条判据不误判达标",
			code: "Sequential_Tasks_4",
			run:  oneMPReport("Sequential_Tasks_4"),
			// 假上游不校验进度：旧逻辑（target=1）会走到 claim，故 claims=0 即「未误判」。
			wantReports: 1, wantClaims: 0, wantMsg: "已上报但进度未点亮",
		},
		{
			name: "runMiniExpert：回读 target=10，单条判据不误判达标",
			code: "Sequential_Tasks_2",
			run: func(p *Panel, a *auth.Auth) (string, error) {
				return runMiniExpert(p, a)
			},
			wantReports: 1, wantClaims: 0, wantMsg: "已上报但进度未归账",
		},
		{
			name:   "accept 后回读失败：保持旧兜底只补 1 条（不阻塞、不升级错误）",
			code:   stubTaskCode,
			script: failingReReadScript,
			run: func(p *Panel, a *auth.Auth) (string, error) {
				return p.runMPMiniChatTask(a, stubTaskCode, false)
			},
			// 假上游不校验进度，故旧逻辑仍会 claim——本用例只锁「回读失败不阻塞、
			// 不报错升级、按兜底 target=1 照旧跑」这三点。
			wantReports: 1, wantClaims: 1, wantMsg: "领取奖励",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fastMPPacing(t)
			script := c.script
			if script == nil {
				script = stubScriptAcceptThenRealTarget()
			}
			s := &mpTaskStub{code: c.code, script: script}
			p := newMPTaskPanel(t, s)

			msg, err := c.run(p, stubAuth())
			if err != nil {
				t.Fatalf("动作不应返回错误（读失败也不升级）: %v", err)
			}
			if !strings.Contains(msg, c.wantMsg) {
				t.Errorf("结果=%q 应含 %q", msg, c.wantMsg)
			}
			reports, claims, accepts := s.counts()
			if reports != c.wantReports {
				t.Errorf("上报条数=%d want %d（补报数量须按 accept 后回读的 target 计算）", reports, c.wantReports)
			}
			if claims != c.wantClaims {
				t.Errorf("领奖次数=%d want %d（未达标不得误领）", claims, c.wantClaims)
			}
			if accepts == 0 {
				t.Error("应发生至少一次 accept 登记")
			}
		})
	}
}
