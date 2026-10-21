package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// sweepTask 构造假上游的一个任务条目（字段名与 upstream.parseGrowthTasks 对齐）。
func sweepTask(code string, target, current int64, acceptStatus string, extra map[string]any) map[string]any {
	t := map[string]any{
		"task_code": code, "title": code, "target": target,
		"current": current, "accept_status": acceptStatus,
		"reward_credit": 100, "reward_energy": 5,
	}
	for k, v := range extra {
		t[k] = v
	}
	return t
}

// sweepBackend 假上游：默认/mp 任务列表、claim、accept；记录调用次数与 claim 码，
// 并允许 listHook 阻塞首个默认列表请求，用于取消/非重叠测试。
type sweepBackend struct {
	listFn   func(isMP bool) []map[string]any
	listHook func(isMP bool)
	claimFn  func(code string, isMP bool) (int, map[string]any) // (status, data)
	acceptFn func(codes []string, isMP bool) (int, string)      // (status, body)

	mu         sync.Mutex
	calls      []string
	claimCodes []string
	claimMP    map[string]bool

	reqs    atomic.Int32
	claims  atomic.Int32
	accepts atomic.Int32
}

func sweepTasksBody(tasks []map[string]any) string {
	b, _ := json.Marshal(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{"tasks": tasks}})
	return string(b)
}

func (b *sweepBackend) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b.reqs.Add(1)
		isMP := strings.EqualFold(r.Header.Get("X-Client-Platform"), "miniprogram")
		b.mu.Lock()
		b.calls = append(b.calls, r.Method+" "+r.URL.Path)
		b.mu.Unlock()

		switch {
		case r.URL.Path == "/v2/activity/growth/tasks":
			if b.listHook != nil {
				b.listHook(isMP)
			}
			var tasks []map[string]any
			if b.listFn != nil {
				tasks = b.listFn(isMP)
			}
			w.Write([]byte(sweepTasksBody(tasks)))
		case strings.HasSuffix(r.URL.Path, "/accept"):
			var body struct {
				TaskCodes []string `json:"task_codes"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			b.accepts.Add(int32(len(body.TaskCodes)))
			if b.acceptFn != nil {
				if status, msg := b.acceptFn(body.TaskCodes, isMP); status != 0 {
					w.WriteHeader(status)
					w.Write([]byte(msg))
					return
				}
			}
			w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/claim"):
			code := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/activity/growth/tasks/"), "/claim")
			b.claims.Add(1)
			b.mu.Lock()
			b.claimCodes = append(b.claimCodes, code)
			if b.claimMP == nil {
				b.claimMP = map[string]bool{}
			}
			b.claimMP[code] = isMP
			b.mu.Unlock()
			if b.claimFn != nil {
				if status, data := b.claimFn(code, isMP); status != 0 {
					out, _ := json.Marshal(map[string]any{"code": status, "msg": "claim failed"})
					w.WriteHeader(status)
					w.Write(out)
					return
				} else if data != nil {
					out, _ := json.Marshal(map[string]any{"code": 0, "msg": "OK", "data": data})
					w.Write(out)
					return
				}
			}
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":true}}`))
		default:
			w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
		}
	}
}

func (b *sweepBackend) snapshotCalls() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.calls))
	copy(out, b.calls)
	return out
}

func (b *sweepBackend) snapshotClaimCodes() ([]string, map[string]bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	codes := make([]string, len(b.claimCodes))
	copy(codes, b.claimCodes)
	mp := make(map[string]bool, len(b.claimMP))
	for k, v := range b.claimMP {
		mp[k] = v
	}
	return codes, mp
}

// newSweepPanel 搭一个可直接调用 RunTaskSweepOnce 的 Panel（假上游 + 指定账号）。
func newSweepPanel(t *testing.T, b *sweepBackend, accounts ...*auth.Auth) *Panel {
	t.Helper()
	srv := httptest.NewServer(b.handler())
	t.Cleanup(srv.Close)
	p := pool.New("")
	for _, a := range accounts {
		p.Add(a)
	}
	cl := &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL,
		WebBaseCN: srv.URL, GlobalEnabled: true,
	}
	cl.SyncHot()
	return &Panel{cfg: Config{Pool: p, Upstream: cl}}
}

// TestTaskSweepDefaultDisabledNoTraffic 默认关闭：零值 options / interval<=0 均不启动、
// 无任何上游请求；显式启动时低于 15 分钟的周期被抬到安全下限。
func TestTaskSweepDefaultDisabledNoTraffic(t *testing.T) {
	b := &sweepBackend{
		listFn: func(bool) []map[string]any {
			return []map[string]any{sweepTask("chat_5", 5, 5, "accepted", nil)}
		},
	}
	p := newSweepPanel(t, b, &auth.Auth{UID: "u1", AccessToken: "at"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if opts := DefaultTaskSweepOptions(); opts.Enabled {
		t.Fatalf("DefaultTaskSweepOptions().Enabled = true, want false（默认关闭）")
	}
	if s := p.StartTaskSweepWithOptions(ctx, DefaultTaskSweepOptions()); s != nil {
		t.Fatalf("默认 options 不应启动 sweep，got %+v", s)
	}
	if s := p.StartTaskSweep(ctx, 0); s != nil {
		t.Fatalf("interval=0 不应启动 sweep，got %+v", s)
	}
	time.Sleep(40 * time.Millisecond)
	if got := b.reqs.Load(); got != 0 {
		t.Fatalf("默认关闭时不应产生上游请求，got %d calls=%v", got, b.snapshotCalls())
	}

	// 低于下限的显式启动被抬到 15 分钟（不会立即跑，Stop 可立即收尾）。
	s := p.StartTaskSweep(ctx, time.Minute)
	if s == nil {
		t.Fatal("显式 StartTaskSweep 应返回句柄")
	}
	if got := s.Interval(); got != MinTaskSweepInterval {
		t.Errorf("Interval() = %s, want %s（安全下限）", got, MinTaskSweepInterval)
	}
	s.Stop()
	if got := b.reqs.Load(); got != 0 {
		t.Fatalf("周期未到不应有请求，got %d", got)
	}

	// Enabled=true + Interval=0 回落默认 15 分钟（仍然只是启动，不立刻跑）。
	s2 := p.StartTaskSweepWithOptions(ctx, TaskSweepOptions{Enabled: true})
	if s2 == nil {
		t.Fatal("Enabled=true 应启动")
	}
	if got := s2.Interval(); got != DefaultTaskSweepInterval {
		t.Errorf("默认 interval = %s, want %s", got, DefaultTaskSweepInterval)
	}
	s2.Stop()
}

// TestTaskSweepClaimEligibilityAndDedup 单轮：只 claim 真正 Claimable 的任务；
// 默认/mp 重复码去重；mp 专属码走 mp 端点；未达标/已领/锁定/空 target 不产生写请求；
// 已达标但未 accepted 的任务先做一次领奖前置 accept。
func TestTaskSweepClaimEligibilityAndDedup(t *testing.T) {
	b := &sweepBackend{
		listFn: func(isMP bool) []map[string]any {
			if isMP {
				return []map[string]any{
					sweepTask("school_season", 1, 1, "accepted", nil), // mp 专属 → ClaimRewardMP
					sweepTask("chat_5", 5, 5, "accepted", nil),        // 与默认列表重复 → 去重
				}
			}
			return []map[string]any{
				sweepTask("chat_5", 5, 5, "accepted", nil),
				sweepTask("verify_done", 1, 1, "not_accepted", nil), // 先 accept 再 claim
				sweepTask("dup_1", 1, 1, "accepted", nil),
				sweepTask("dup_1", 1, 1, "accepted", nil), // 列表内重复 → 只 claim 一次
				sweepTask("halfway", 10, 3, "accepted", nil),
				sweepTask("claimed_1", 5, 5, "claimed", nil),
				sweepTask("locked_1", 5, 5, "accepted", map[string]any{"locked": true}),
				sweepTask("zero_target", 0, 0, "accepted", nil),
			}
		},
		claimFn: func(code string, isMP bool) (int, map[string]any) {
			if code == "school_season" && !isMP {
				t.Errorf("school_season 应走 mp claim 端点")
			}
			return 0, map[string]any{"already_claimed": false, "credit": 100, "energy": 5}
		},
	}
	p := newSweepPanel(t, b, &auth.Auth{UID: "u1", AccessToken: "at"})
	st := p.RunTaskSweepOnce()

	if st.Accounts != 1 || st.Skipped != 0 || st.Errors != 0 {
		t.Fatalf("stats = %+v, want accounts=1 skipped=0 errors=0", st)
	}
	if st.Accepted != 1 {
		t.Errorf("accepted = %d, want 1（仅 verify_done 需要领奖前置 accept）", st.Accepted)
	}
	if st.Claimed != 4 || st.Credits != 400 || st.Energy != 20 {
		t.Errorf("claimed=%d credits=%d energy=%d, want 4/400/20", st.Claimed, st.Credits, st.Energy)
	}
	codes, mp := b.snapshotClaimCodes()
	count := map[string]int{}
	for _, c := range codes {
		count[c]++
	}
	if count["dup_1"] != 1 {
		t.Errorf("dup_1 claim 次数 = %d, want 1（单轮去重）", count["dup_1"])
	}
	if count["chat_5"] != 1 {
		t.Errorf("chat_5 claim 次数 = %d, want 1（默认/mp 合并去重）", count["chat_5"])
	}
	if count["school_season"] != 1 || !mp["school_season"] {
		t.Errorf("school_season claim=%d mp=%v, want 1/true", count["school_season"], mp["school_season"])
	}
	for _, forbidden := range []string{"halfway", "claimed_1", "locked_1", "zero_target"} {
		if count[forbidden] != 0 {
			t.Errorf("%s 不应 claim，got %d", forbidden, count[forbidden])
		}
	}
	if got := b.accepts.Load(); got != 1 {
		t.Errorf("accept 提交任务数 = %d, want 1（不得批量接受无关任务）", got)
	}
}

// TestTaskSweepSkipsDisabledManualGlobalEnterprise disabled / manual_disabled /
// global / enterprise 账号零上游请求，只有正常 CN 个人账号参与。
func TestTaskSweepSkipsDisabledManualGlobalEnterprise(t *testing.T) {
	b := &sweepBackend{
		listFn: func(bool) []map[string]any {
			return []map[string]any{sweepTask("chat_5", 5, 5, "accepted", nil)}
		},
		claimFn: func(string, bool) (int, map[string]any) {
			return 0, map[string]any{"already_claimed": false, "credit": 100, "energy": 5}
		},
	}
	p := newSweepPanel(t, b,
		&auth.Auth{UID: "u1", AccessToken: "at"},
		&auth.Auth{UID: "u2", AccessToken: "at"},
		&auth.Auth{UID: "u3", AccessToken: "at"},
		&auth.Auth{UID: "g1", AccessToken: "at", Domain: "www.workbuddy.ai"},
		&auth.Auth{UID: "e1", AccessToken: "at", EnterpriseID: "ent-1"},
	)
	p.cfg.Pool.Disable("u2", "test auto disable")
	if found, _ := p.cfg.Pool.SetManualDisabled("u3", true, "test manual disable"); !found {
		t.Fatal("precondition: SetManualDisabled 未找到 u3")
	}

	st := p.RunTaskSweepOnce()
	if st.Accounts != 1 || st.Filtered != 4 {
		t.Errorf("stats = %+v, want accounts=1 filtered=4", st)
	}
	if st.Claimed != 1 || st.Credits != 100 || st.Energy != 5 {
		t.Errorf("claimed=%d credits=%d energy=%d, want 1/100/5（仅 u1）", st.Claimed, st.Credits, st.Energy)
	}
	// u1: 默认列表 + mp 列表 + 一次 claim。过滤账号不得出现任何请求。
	if got := b.reqs.Load(); got != 3 {
		t.Fatalf("上游请求数 = %d, want 3；calls=%v", got, b.snapshotCalls())
	}
}

// TestTaskSweepSkipsBusyAccount 账号持有 per-account 锁时整账号跳过、零请求；
// 解锁后同一路径可正常 claim。
func TestTaskSweepSkipsBusyAccount(t *testing.T) {
	b := &sweepBackend{
		listFn: func(bool) []map[string]any {
			return []map[string]any{sweepTask("chat_5", 5, 5, "accepted", nil)}
		},
	}
	p := newSweepPanel(t, b, &auth.Auth{UID: "u1", AccessToken: "at"})
	if !p.tryLockAccount("u1") {
		t.Fatal("precondition: 模拟占用账号锁失败")
	}
	st := p.RunTaskSweepOnce()
	if st.Accounts != 1 || st.Skipped != 1 || st.Claimed != 0 {
		t.Errorf("stats = %+v, want accounts=1 skipped=1 claimed=0", st)
	}
	if got := b.reqs.Load(); got != 0 {
		t.Fatalf("忙账号不应产生上游请求，got %d", got)
	}
	p.unlockAccount("u1")

	st = p.RunTaskSweepOnce()
	if st.Skipped != 0 || st.Claimed != 1 {
		t.Errorf("解锁后 stats = %+v, want skipped=0 claimed=1", st)
	}
}

// TestTaskSweepQueueBusySkipsRound 任务中心队列 running 时整轮跳过，
// 避免清扫抢走队列准备执行的账号锁。
func TestTaskSweepQueueBusySkipsRound(t *testing.T) {
	b := &sweepBackend{
		listFn: func(bool) []map[string]any {
			return []map[string]any{sweepTask("chat_5", 5, 5, "accepted", nil)}
		},
	}
	p := newSweepPanel(t, b, &auth.Auth{UID: "u1", AccessToken: "at"})
	q := p.queue()
	q.mu.Lock()
	q.running = true
	q.mu.Unlock()

	st := p.RunTaskSweepOnce()
	if !st.QueueBusy || st.Accounts != 0 || st.Claimed != 0 {
		t.Errorf("stats = %+v, want queue_busy=true 且无账号处理", st)
	}
	if got := b.reqs.Load(); got != 0 {
		t.Fatalf("队列执行中不应有清扫请求，got %d", got)
	}

	q.mu.Lock()
	q.running = false
	q.mu.Unlock()
	st = p.RunTaskSweepOnce()
	if st.QueueBusy || st.Claimed != 1 {
		t.Errorf("队列结束后 stats = %+v, want queue_busy=false claimed=1", st)
	}
}

// TestTaskSweepCancellation ctx 取消可中断本轮：取消后不再发起 mp 列表/claim；
// 预取消则零请求。
func TestTaskSweepCancellation(t *testing.T) {
	t.Run("pre-cancelled", func(t *testing.T) {
		b := &sweepBackend{}
		p := newSweepPanel(t, b, &auth.Auth{UID: "u1", AccessToken: "at"})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		st := p.RunTaskSweepOnceContext(ctx)
		if !st.Cancelled || st.Accounts != 0 {
			t.Errorf("stats = %+v, want cancelled=true accounts=0", st)
		}
		if got := b.reqs.Load(); got != 0 {
			t.Errorf("预取消不应产生请求，got %d", got)
		}
	})

	t.Run("mid-account", func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		b := &sweepBackend{
			listHook: func(isMP bool) {
				if !isMP {
					once.Do(func() {
						close(entered)
						<-release
					})
				}
			},
			listFn: func(bool) []map[string]any {
				return []map[string]any{sweepTask("chat_5", 5, 5, "accepted", nil)}
			},
		}
		p := newSweepPanel(t, b, &auth.Auth{UID: "u1", AccessToken: "at"})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan TaskSweepStats, 1)
		go func() { done <- p.RunTaskSweepOnceContext(ctx) }()

		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("默认任务列表请求未到达")
		}
		cancel()
		close(release)
		st := <-done
		if !st.Cancelled {
			t.Errorf("stats = %+v, want cancelled=true", st)
		}
		if st.Claimed != 0 {
			t.Errorf("取消后不应 claim，got %d", st.Claimed)
		}
		if got := b.reqs.Load(); got != 1 {
			t.Errorf("取消后不应再发 mp 列表/claim，requests=%d calls=%v", got, b.snapshotCalls())
		}
	})
}

// TestTaskSweepPeriodicNonOverlapAndStop 周期轮上一轮未结束时不会叠加下一轮；
// 手动轮与周期轮共享单飞门（返回 Busy）；Stop 可立即取消并停止新请求。
func TestTaskSweepPeriodicNonOverlapAndStop(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	b := &sweepBackend{
		listHook: func(isMP bool) {
			if !isMP {
				once.Do(func() {
					close(entered)
					<-release
				})
			}
		},
		listFn: func(bool) []map[string]any {
			return []map[string]any{sweepTask("chat_5", 5, 5, "accepted", nil)}
		},
	}
	p := newSweepPanel(t, b, &auth.Auth{UID: "u1", AccessToken: "at"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := p.startTaskSweep(ctx, 20*time.Millisecond) // 内部入口：短周期仅测试用
	if s == nil {
		t.Fatal("startTaskSweep 返回 nil")
	}

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("周期轮未启动默认任务列表请求")
	}
	// 首轮被阻塞超过多个 interval；同步循环不应叠加第二轮。
	time.Sleep(100 * time.Millisecond)
	if got := b.reqs.Load(); got != 1 {
		t.Fatalf("周期扫描发生重叠（阻塞期间 requests=%d, want 1）", got)
	}

	// 周期轮持有单飞门时，手动轮直接 Busy 且不产生请求。
	st := p.RunTaskSweepOnce()
	if !st.Busy {
		t.Errorf("手动轮 stats = %+v, want busy=true", st)
	}
	if got := b.reqs.Load(); got != 1 {
		t.Errorf("Busy 手动轮不应发请求，got %d", got)
	}

	cancel()
	close(release)
	s.Stop()
	after := b.reqs.Load()
	time.Sleep(60 * time.Millisecond)
	if got := b.reqs.Load(); got != after {
		t.Errorf("Stop 后仍有新请求：after=%d got=%d", after, got)
	}
}

// TestTaskSweepIdempotentSecondRound 第二轮上游状态已 claimed → 不再 claim；
// 单轮内已由 Eligibility 测试覆盖，二者共同证明不会重复领奖。
func TestTaskSweepIdempotentSecondRound(t *testing.T) {
	var mu sync.Mutex
	claimed := false
	b := &sweepBackend{
		listFn: func(bool) []map[string]any {
			mu.Lock()
			defer mu.Unlock()
			status := "accepted"
			if claimed {
				status = "claimed"
			}
			return []map[string]any{sweepTask("chat_5", 5, 5, status, nil)}
		},
		claimFn: func(string, bool) (int, map[string]any) {
			mu.Lock()
			claimed = true
			mu.Unlock()
			return 0, map[string]any{"already_claimed": false, "credit": 100, "energy": 5}
		},
	}
	p := newSweepPanel(t, b, &auth.Auth{UID: "u1", AccessToken: "at"})

	first := p.RunTaskSweepOnce()
	if first.Claimed != 1 || first.Credits != 100 {
		t.Fatalf("第一轮 claimed=%d credits=%d, want 1/100", first.Claimed, first.Credits)
	}
	b.claims.Store(0)
	second := p.RunTaskSweepOnce()
	if second.Claimed != 0 || second.Errors != 0 {
		t.Errorf("第二轮 claimed=%d errors=%d, want 0/0", second.Claimed, second.Errors)
	}
	if got := b.claims.Load(); got != 0 {
		t.Errorf("第二轮不应再打 claim 端点，got %d", got)
	}
}

// TestTaskSweepClaimErrorDoesNotBlock 单个 claim 失败只计 errors，不阻断后续任务。
func TestTaskSweepClaimErrorDoesNotBlock(t *testing.T) {
	b := &sweepBackend{
		listFn: func(bool) []map[string]any {
			return []map[string]any{
				sweepTask("bad_1", 5, 5, "accepted", nil),
				sweepTask("good_1", 5, 5, "accepted", nil),
			}
		},
		claimFn: func(code string, _ bool) (int, map[string]any) {
			if code == "bad_1" {
				return http.StatusBadRequest, nil
			}
			return 0, map[string]any{"already_claimed": false, "credit": 100, "energy": 5}
		},
	}
	p := newSweepPanel(t, b, &auth.Auth{UID: "u1", AccessToken: "at"})
	st := p.RunTaskSweepOnce()
	if st.Errors != 1 {
		t.Errorf("errors = %d, want 1", st.Errors)
	}
	if st.Claimed != 1 || st.Credits != 100 {
		t.Errorf("claimed=%d credits=%d, want 1/100（失败项不阻挡成功项）", st.Claimed, st.Credits)
	}
}

// TestTaskSweepCapsClaimsPerAccount 单账号单轮最多 claim 20 个任务，超出部分
// 计入 Deferred 留给下一轮，防止异常任务表把一轮放大成无界写请求。
func TestTaskSweepCapsClaimsPerAccount(t *testing.T) {
	tasks := make([]map[string]any, 0, 25)
	for i := 0; i < 25; i++ {
		tasks = append(tasks, sweepTask("cap_"+strconv.Itoa(i), 1, 1, "accepted", nil))
	}
	b := &sweepBackend{
		listFn: func(isMP bool) []map[string]any {
			if isMP {
				return nil
			}
			return tasks
		},
		claimFn: func(string, bool) (int, map[string]any) {
			return 0, map[string]any{"already_claimed": false, "credit": 100, "energy": 5}
		},
	}
	p := newSweepPanel(t, b, &auth.Auth{UID: "u1", AccessToken: "at"})
	st := p.RunTaskSweepOnce()
	if st.Claimed != taskSweepMaxClaimsPerAccount || st.Deferred != 5 {
		t.Errorf("claimed=%d deferred=%d, want %d/5", st.Claimed, st.Deferred, taskSweepMaxClaimsPerAccount)
	}
	if got := b.claims.Load(); got != int32(taskSweepMaxClaimsPerAccount) {
		t.Errorf("claim 请求数 = %d, want %d", got, taskSweepMaxClaimsPerAccount)
	}
}
