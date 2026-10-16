// taskcenter_test.go 任务中心「达标未领入队补领」回归（吸收 panel 5f6c7caf）。
//
// 口径：growthPending 把「已达标但未领取」（Target>0 && Current>=Target && !Claimed）
// 也视为需要动作——奖励没到账就是待办，队列执行 runGrowthQueued 会回读 claimable
// 并自动领奖；此前该分支恒 false，这类任务永远不进待办、奖励永不自动补领。
// 闸门仍是有自动化动作（autoActionFor != nil）：无动作的任务入队只会在执行时报
// "任务 X 无自动动作"，白占一次账号锁。
//
// 测试风格沿用包内既有做法：判据走表驱动单测，入队/拾取/补领走 httptest 假上游
// （upstream.Client 的 ChatBaseCN/BillingBaseCN/WebBaseCN 指向桩地址）。
package panel

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// TestTaskCenterGrowthPendingReachedUnclaimed 判据表驱动：达标未领 → 待办为真；
// 已领 → 不入队（不重复领奖）；无自动化动作 → 不入队（排队也执行不了）。
// 「未达标 + 有动作」仍是任务中心主业（自动化补足进度），本次改动不触碰该分支。
func TestTaskCenterGrowthPendingReachedUnclaimed(t *testing.T) {
	const code = "chat_5" // 已登记自动化动作
	if autoActionFor(code) == nil {
		t.Fatalf("precondition: %s 应已登记自动化动作", code)
	}
	cases := []struct {
		name string
		task upstream.Task
		want bool
	}{
		{"达标未领", upstream.Task{TaskCode: code, Target: 5, Current: 5, AcceptStatus: "accepted"}, true},
		{"达标未领（超额）", upstream.Task{TaskCode: code, Target: 5, Current: 7}, true},
		{"已领", upstream.Task{TaskCode: code, Target: 5, Current: 5, AcceptStatus: "claimed", Claimed: true}, false},
		{"未达标（有自动化动作，既有待办口径）", upstream.Task{TaskCode: code, Target: 5, Current: 3}, true},
		{"达标未领但无自动化动作", upstream.Task{TaskCode: "no_such_task", Target: 5, Current: 5}, false},
		{"未达标且无自动化动作", upstream.Task{TaskCode: "no_such_task", Target: 5, Current: 1}, false},
		{"锁定环", upstream.Task{TaskCode: code, Target: 5, Current: 5, Locked: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := growthPending(tc.task); got != tc.want {
				t.Errorf("growthPending(%+v) = %v, want %v", tc.task, got, tc.want)
			}
		})
	}
}

// taskCenterStub 任务中心假上游：growth 列表按用例注入，其余端点回 200 空信封
// （accept/事件上报等写动作不产生进度，断言只看"是否发出"），并记录全部请求。
type taskCenterStub struct {
	mu    sync.Mutex
	calls []string
	tasks string // data.tasks 的 JSON 数组字面量
}

func (s *taskCenterStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.calls = append(s.calls, r.Method+" "+r.URL.Path)
	s.mu.Unlock()
	switch r.URL.Path {
	case "/v2/activity/growth/tasks":
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":` + s.tasks + `}}`))
	case "/activity/growth/tasks/Library_read/claim":
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":100,"energy":5}}`))
	default:
		w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}
}

func (s *taskCenterStub) called(method, path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if c == method+" "+path {
			return true
		}
	}
	return false
}

func (s *taskCenterStub) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.calls))
	copy(out, s.calls)
	return out
}

// taskQueueItems 队列条目快照（锁内拷贝；执行中状态会变，存在性/终态断言用）。
func taskQueueItems(p *Panel) []queueItem {
	q := p.queue()
	q.mu.Lock()
	defer q.mu.Unlock()
	items := make([]queueItem, len(q.items))
	copy(items, q.items)
	return items
}

// waitTaskQueueIdle 等异步队列跑完（轮询 running；起点是 startTaskQueue 返回后的
// goroutine，条目间还有 reportGap 节流，故给足超时）。
func waitTaskQueueIdle(t *testing.T, p *Panel, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		q := p.queue()
		q.mu.Lock()
		running := q.running
		q.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("队列 %v 内未结束（执行卡住或桩未被应答）", timeout)
}

// TestTaskCenterQueuePicksUpReachedUnclaimed 全链路：达标未领 → 待办计数为真、
// 队列启动并入队、被队列拾取执行、自动发出领奖请求；已领 → 队列不启动（不重复
// 入队）；未达标 → 入队做动作但不领奖（领奖只在达标后发生）。
func TestTaskCenterQueuePicksUpReachedUnclaimed(t *testing.T) {
	const (
		claimPath = "/activity/growth/tasks/Library_read/claim"
		// Library_read 的自动动作是单次事件上报（无 mp 节奏、无轮询等待），
		// 最省桩交互；accept_status=accepted 让队列跳过前置 accept（省一次节流睡眠）。
		reachedUnclaimed = `[{"task_code":"Library_read","accept_status":"accepted","target":5,"current":5,"reward_credit":100,"reward_energy":5}]`
		claimed          = `[{"task_code":"Library_read","accept_status":"claimed","target":5,"current":5,"reward_credit":100,"reward_energy":5}]`
		notReached       = `[{"task_code":"Library_read","accept_status":"accepted","target":5,"current":3,"reward_credit":100,"reward_energy":5}]`
	)
	cases := []struct {
		name        string
		tasks       string
		wantPending int
		wantStarted bool
		wantTotal   int
		wantClaim   bool
	}{
		{"达标未领→入队补领", reachedUnclaimed, 1, true, 1, true},
		{"已领→不重复入队", claimed, 0, false, 0, false},
		{"未达标→入队做动作但不领奖", notReached, 1, true, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &taskCenterStub{tasks: tc.tasks}
			srv := httptest.NewServer(stub)
			defer srv.Close()
			up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
			up.SyncHot()
			p := New(Config{Version: "test", APIKey: "test-key", Pool: newSmokePool(), Upstream: up})
			p.cfg.Pool.Add(&auth.Auth{UID: "u1", AccessToken: "at"})

			// 待办（扫描）口径：达标未领要出现在待办计数里。
			if n, err := p.pendingCount(); err != nil || n != tc.wantPending {
				t.Fatalf("pendingCount = %d, err=%v; want %d", n, err, tc.wantPending)
			}

			total, _, started, err := p.startTaskQueue(1, true)
			if err != nil {
				t.Fatalf("startTaskQueue: %v", err)
			}
			if started != tc.wantStarted || total != tc.wantTotal {
				t.Fatalf("startTaskQueue = total %d, started %v; want total %d, started %v",
					total, started, tc.wantTotal, tc.wantStarted)
			}
			if !tc.wantStarted {
				// 已领任务不得留下队列条目（重复入队会让补领再跑一遍动作）。
				if items := taskQueueItems(p); len(items) != 0 {
					t.Errorf("已领任务不应入队，items=%+v", items)
				}
				if stub.called("POST", claimPath) {
					t.Errorf("未启动队列却发出了领奖请求；calls=%v", stub.snapshot())
				}
				return
			}

			// 被队列拾取：条目按 账号 + code 落在队列里（状态随后由执行 goroutine 推进，
			// 故只断言存在性，终态在下方等执行结束后再断）。
			items := taskQueueItems(p)
			if len(items) != 1 || items[0].UID != "u1" || items[0].Kind != "growth" || items[0].Code != "Library_read" {
				t.Fatalf("队列条目 = %+v; want u1/growth/Library_read", items)
			}

			waitTaskQueueIdle(t, p, 20*time.Second)
			items = taskQueueItems(p)
			if len(items) != 1 || items[0].Status != "done" {
				t.Errorf("队列条目终态 = %+v; want status=done", items)
			}
			if got := stub.called("POST", claimPath); got != tc.wantClaim {
				t.Errorf("领奖请求 called=%v want %v；calls=%v", got, tc.wantClaim, stub.snapshot())
			}
		})
	}
}
