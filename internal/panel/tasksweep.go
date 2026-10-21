// tasksweep.go 周期性任务奖励补领清扫（opt-in，默认不启动）。
//
// 职责边界（刻意保守，避免与 taskcenter/autotask 重复）：
//   - 不跑任何行为链路：不发起用户对话、不上报事件、不执行 autoActions。
//   - 只处理 Task.Claimable 的任务（!claimed && target>0 && current>=target），
//     且额外排除 Claimed/Locked/空 task_code；不使用 accept_status=="completed"
//     作为领奖判据（上游回读时该状态会短暂出现后回滚）。
//   - 对「已达标但尚未 accept」的任务，只做一次领奖前置的 accept（按批），
//     不批量接受无关任务；这部分仅服务于本轮即将 claim 的条目。
//   - 默认/小程序两套列表按 task_code 去重后合并，单轮内同一 task_code 只 claim 一次；
//     小程序专属码走 ClaimRewardMP，其余走 ClaimReward。
//
// 安全边界：
//   - 仅显式调用 StartTaskSweep/StartTaskSweepWithOptions 才启动；TaskSweepOptions
//     零值的 Enabled=false 即默认关闭，interval<=0 也不启动。
//   - 最小周期 15 分钟（低于下限自动抬到下限）；首轮等待一个周期后执行，
//     避免服务启动瞬间叠加全账号请求。
//   - 与单任务/一键完成/任务中心队列共用 Panel 的 per-account 锁：账号正在跑
//     任务动作时整账号跳过，下一轮自然补上。
//   - 任务中心队列 running 时整轮跳过，避免抢走队列准备执行的账号锁。
//   - 账号间并发上限 2；单账号单轮最多 claim 20 个任务（其余顺延下一轮）；
//     accept 按 20 个/批提交，避免异常任务表放大上游请求。
//   - 支持 context 取消：账号/批次/单项边界均检查 ctx；上游 HTTP 调用本身无
//     ctx 参数，取消时最多等待当前一次请求返回，不会开启新的请求。
//   - disabled / manual_disabled / global / enterprise 账号零上游请求。
//
// 本文件只新增独立符号，不改 Panel 结构体：后台句柄由调用方持有返回值，单飞
// 状态用按 *Panel 索引的包级注册表承载（生产通常只有一个 Panel）。
package panel

import (
	"context"
	"log"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// TaskSweep 的安全策略常量。15 分钟是产品保守下限：claim/accept 都是幂等轻写，
// 但全账号扫描仍会放大上游请求，分钟级以内的高频轮询没有收益。
const (
	// DefaultTaskSweepInterval 默认（也是最小）清扫周期。
	DefaultTaskSweepInterval = 15 * time.Minute
	// MinTaskSweepInterval 允许的最小清扫周期；调用方传入更小值时抬到该值。
	MinTaskSweepInterval = DefaultTaskSweepInterval

	// taskSweepConcurrency 账号间并发上限（账号内天然串行，共用 per-account 锁）。
	taskSweepConcurrency = 2
	// taskSweepMaxClaimsPerAccount 单账号单轮 claim 上限；超出部分下一轮继续。
	taskSweepMaxClaimsPerAccount = 20
	// taskSweepAcceptBatch 领奖前置 accept 的单批 task_code 上限。
	taskSweepAcceptBatch = 20
)

// TaskSweepOptions 启动参数。零值 = 关闭（Enabled=false），供集成方直接绑定
// 配置开关；需要最短路径时也可直接调 StartTaskSweep(ctx, interval) 显式 opt-in。
type TaskSweepOptions struct {
	Enabled  bool          `json:"enabled"`
	Interval time.Duration `json:"interval"`
}

// DefaultTaskSweepOptions 返回安全的默认值：关闭 + 15 分钟。
func DefaultTaskSweepOptions() TaskSweepOptions {
	return TaskSweepOptions{Enabled: false, Interval: DefaultTaskSweepInterval}
}

// TaskSweepStats 一轮补领清扫的汇总（日志与手动触发回执可用）。
type TaskSweepStats struct {
	Accounts  int   `json:"accounts"`   // 进入单账号流程的账号数（含忙账号）
	Skipped   int   `json:"skipped"`    // 因账号任务锁被占用而整账号跳过的数量
	Filtered  int   `json:"filtered"`   // disabled/manual_disabled/global/enterprise 跳过数
	Accepted  int   `json:"accepted"`   // 领奖前置 accept 成功的任务数
	Claimed   int   `json:"claimed"`    // 本轮 claim 端点成功的任务数
	Credits   int64 `json:"credits"`    // 本轮到账积分
	Energy    int64 `json:"energy"`     // 本轮到账能量
	Errors    int   `json:"errors"`     // 单账号/单任务错误次数（不中断本轮）
	Deferred  int   `json:"deferred"`   // 因单账号上限顺延到下一轮的可领任务数
	Cancelled bool  `json:"cancelled"`  // 是否因 ctx 取消提前结束
	Busy      bool  `json:"busy"`       // 是否因另一轮清扫正在跑而直接返回
	QueueBusy bool  `json:"queue_busy"` // 是否因任务中心队列执行中而跳过
}

// Claims 本轮实际成功调用 claim 的任务数（与 Claimed 同义，保留上游语义化别名）。
func (s TaskSweepStats) Claims() int { return s.Claimed }

// TaskSweeper 一个 Panel 上已启动的周期清扫句柄。Stop 幂等；Done 在循环退出后关闭。
type TaskSweeper struct {
	panel    *Panel
	interval time.Duration
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once

	mu     sync.Mutex
	last   TaskSweepStats
	lastAt time.Time
}

// taskSweepPanelState 按 Panel 保存单飞门与当前 sweep 句柄。
type taskSweepPanelState struct {
	// runMu 保证同一 Panel 的周期轮与手动轮不重叠（TryLock 语义）。
	runMu sync.Mutex
	// mu 保护 sweeper 字段的启动/退出竞态。
	mu      sync.Mutex
	sweeper *TaskSweeper
}

var taskSweepStates sync.Map // map[*Panel]*taskSweepPanelState

func (p *Panel) taskSweepState() *taskSweepPanelState {
	if v, ok := taskSweepStates.Load(p); ok {
		return v.(*taskSweepPanelState)
	}
	v, _ := taskSweepStates.LoadOrStore(p, &taskSweepPanelState{})
	return v.(*taskSweepPanelState)
}

// StartTaskSweep 显式启动周期补领清扫（opt-in）。interval<=0 返回 nil（不启动）；
// 小于 MinTaskSweepInterval 时自动抬到 15 分钟。重复调用同一 Panel 返回现有句柄，
// 不会叠加 goroutine。调用方应保存返回句柄以便 Stop/LastStats；即使丢弃返回值，
// 循环也会随 ctx 取消退出。
func (p *Panel) StartTaskSweep(ctx context.Context, interval time.Duration) *TaskSweeper {
	if p == nil || interval <= 0 {
		return nil
	}
	if interval < MinTaskSweepInterval {
		log.Printf("panel: 任务补领清扫周期 %s 低于安全下限 %s，按 %s 启动",
			interval, MinTaskSweepInterval, MinTaskSweepInterval)
		interval = MinTaskSweepInterval
	}
	return p.startTaskSweep(ctx, interval)
}

// StartTaskSweepWithOptions 按带开关的配置启动：Enabled=false（零值）直接返回 nil；
// Enabled=true 且 Interval<=0 时回落 DefaultTaskSweepInterval。集成方可直接把
// 配置开关映射成该结构，避免忘记默认关闭。
func (p *Panel) StartTaskSweepWithOptions(ctx context.Context, opts TaskSweepOptions) *TaskSweeper {
	if p == nil || !opts.Enabled {
		return nil
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultTaskSweepInterval
	}
	return p.StartTaskSweep(ctx, opts.Interval)
}

// startTaskSweep 内部入口（不校验最小周期，测试用短周期验证非重叠；生产只经
// StartTaskSweep/StartTaskSweepWithOptions 进入）。
func (p *Panel) startTaskSweep(ctx context.Context, interval time.Duration) *TaskSweeper {
	if p == nil || interval <= 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	st := p.taskSweepState()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.sweeper != nil {
		return st.sweeper
	}
	child, cancel := context.WithCancel(ctx)
	s := &TaskSweeper{
		panel:    p,
		interval: interval,
		ctx:      child,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	st.sweeper = s
	go s.loop(st)
	return s
}

// loop 周期执行：上一轮结束后才重置计时器，天然保证周期轮不重叠。
func (s *TaskSweeper) loop(st *taskSweepPanelState) {
	defer func() {
		st.mu.Lock()
		if st.sweeper == s {
			st.sweeper = nil
		}
		st.mu.Unlock()
		close(s.done)
	}()

	timer := time.NewTimer(s.interval)
	defer timer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-timer.C:
			stats := s.panel.RunTaskSweepOnceContext(s.ctx)
			s.record(stats)
			if s.ctx.Err() != nil {
				return
			}
			timer.Reset(s.interval)
		}
	}
}

func (s *TaskSweeper) record(stats TaskSweepStats) {
	s.mu.Lock()
	s.last = stats
	s.lastAt = time.Now()
	s.mu.Unlock()
}

// Interval 返回实际生效周期。
func (s *TaskSweeper) Interval() time.Duration {
	if s == nil {
		return 0
	}
	return s.interval
}

// Done 在周期循环退出后关闭。
func (s *TaskSweeper) Done() <-chan struct{} {
	if s == nil || s.done == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return s.done
}

// Stop 取消并等待周期循环退出；幂等。未启动/已退出的句柄直接返回。
// 等价 StopContext(context.Background())：调用方必须接受等待在途 HTTP 请求
// （上游任务 API 无 ctx 参数）；进程退出路径应使用带超时的 StopContext。
func (s *TaskSweeper) Stop() {
	s.StopContext(context.Background())
}

// StopContext 取消周期循环并最多等待到 ctx 结束；返回 true 表示循环已退出，
// false 表示在途上游请求仍在收尾、ctx 先到期。取消后不会再发起新请求，但
// 单次已在执行的 HTTP 调用无法被中断（沿用 upstream 既有 120s 客户端超时）。
func (s *TaskSweeper) StopContext(ctx context.Context) bool {
	if s == nil {
		return true
	}
	if s.cancel != nil {
		s.stopOnce.Do(s.cancel)
	}
	if s.done == nil {
		return true
	}
	if ctx == nil {
		<-s.done
		return true
	}
	select {
	case <-s.done:
		return true
	case <-ctx.Done():
		return false
	}
}

// LastStats 返回最近一轮结果与完成时刻（零值表示尚未跑过）。
func (s *TaskSweeper) LastStats() (TaskSweepStats, time.Time) {
	if s == nil {
		return TaskSweepStats{}, time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last, s.lastAt
}

// RunTaskSweepOnce 对所有账号执行一轮补领清扫（与周期轮共享单飞门）。
// 保留无参形式，方便集成方/测试直接调用；需要取消时用 RunTaskSweepOnceContext。
func (p *Panel) RunTaskSweepOnce() TaskSweepStats {
	return p.RunTaskSweepOnceContext(context.Background())
}

// RunTaskSweepOnceContext 执行一轮补领清扫。并发轮/手动轮撞车时返回 Busy=true
// 且不产生任何上游请求；任务中心队列 running 时返回 QueueBusy=true 并跳过。
func (p *Panel) RunTaskSweepOnceContext(ctx context.Context) TaskSweepStats {
	if ctx == nil {
		ctx = context.Background()
	}
	if p == nil || p.cfg.Pool == nil || p.cfg.Upstream == nil {
		return TaskSweepStats{Errors: 1}
	}
	st := p.taskSweepState()
	if !st.runMu.TryLock() {
		return TaskSweepStats{Busy: true}
	}
	defer st.runMu.Unlock()

	if ctx.Err() != nil {
		return TaskSweepStats{Cancelled: true}
	}
	// 任务中心队列 running 时整轮跳过：队列的账号组队发生在扫描阶段，若清扫抢先
	// 拿到某个账号锁，队列会把该账号标 skipped——保守选择是让队列先跑完。
	q := p.queue()
	q.mu.Lock()
	queueRunning := q.running
	q.mu.Unlock()
	if queueRunning {
		log.Printf("panel: 任务补领清扫：任务中心队列执行中，本轮跳过")
		return TaskSweepStats{QueueBusy: true}
	}

	stats := p.runTaskSweepAccounts(ctx)
	if stats.Claimed > 0 || stats.Accepted > 0 || stats.Errors > 0 || stats.Deferred > 0 || stats.Cancelled || stats.Skipped > 0 {
		log.Printf("panel: 任务补领清扫：%d 个账号（忙跳过 %d / 过滤 %d）· 接受 %d · 领奖 %d · +%d 分 +%d 能 · 顺延 %d · 出错 %d%s",
			stats.Accounts, stats.Skipped, stats.Filtered, stats.Accepted,
			stats.Claimed, stats.Credits, stats.Energy, stats.Deferred, stats.Errors,
			taskSweepCancelledSuffix(stats.Cancelled))
	}
	return stats
}

func taskSweepCancelledSuffix(cancelled bool) string {
	if cancelled {
		return " · 已取消"
	}
	return ""
}

// taskSweepJob 单账号任务单元。
type taskSweepJob struct {
	uid string
	a   *auth.Auth
}

// runTaskSweepAccounts 取账号快照、过滤无成长体系账号、按并发上限分发。
func (p *Panel) runTaskSweepAccounts(ctx context.Context) TaskSweepStats {
	var stats TaskSweepStats
	statuses := p.cfg.Pool.List()
	jobs := make([]taskSweepJob, 0, len(statuses))
	for _, status := range statuses {
		if status.Disabled || status.ManualDisabled {
			stats.Filtered++
			continue
		}
		a := p.cfg.Pool.AuthByUID(status.UID)
		// global 无 CN 成长任务体系；enterprise 上游返回 403 growth system is only
		// available for personal users（上游 tasksweep 同口径），二者零请求跳过。
		if a == nil || a.IsGlobal() || a.EnterpriseID != "" {
			stats.Filtered++
			continue
		}
		jobs = append(jobs, taskSweepJob{uid: status.UID, a: a})
	}
	if len(jobs) == 0 {
		return stats
	}

	sem := make(chan struct{}, taskSweepConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	cancelled := false
dispatch:
	for _, job := range jobs {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		select {
		case <-ctx.Done():
			cancelled = true
			break dispatch
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(job taskSweepJob) {
			defer wg.Done()
			defer func() { <-sem }()
			r := p.sweepAccount(ctx, job.a, job.uid)
			mu.Lock()
			stats.Accounts += r.accounts
			stats.Skipped += r.skipped
			stats.Filtered += r.filtered
			stats.Accepted += r.accepted
			stats.Claimed += r.claimed
			stats.Credits += r.credit
			stats.Energy += r.energy
			stats.Errors += r.errors
			stats.Deferred += r.deferred
			if r.cancelled {
				stats.Cancelled = true
			}
			mu.Unlock()
		}(job)
	}
	wg.Wait()
	if cancelled || ctx.Err() != nil {
		stats.Cancelled = true
	}
	return stats
}

// taskSweepResult 单账号清扫结果。
type taskSweepResult struct {
	accounts, skipped, filtered, accepted, claimed, errors, deferred int
	credit, energy                                                   int64
	cancelled                                                        bool
}

// sweepAccount 单账号一轮清扫。命中 per-account 锁时整账号跳过，不打断正在跑
// 的任务动作；所有写操作都只针对 Claimable 的已完成任务。
func (p *Panel) sweepAccount(ctx context.Context, a *auth.Auth, uid string) (res taskSweepResult) {
	res.accounts = 1
	if ctx.Err() != nil {
		res.cancelled = true
		return res
	}
	// 账号快照可能在调度后被改状态（disable/manual disable）；锁前复核一次。
	if status, ok := p.cfg.Pool.Status(uid); !ok || status.Disabled || status.ManualDisabled {
		res.filtered = 1
		return res
	}
	if !p.tryLockAccount(uid) {
		res.skipped = 1
		return res
	}
	defer p.unlockAccount(uid)
	if ctx.Err() != nil {
		res.cancelled = true
		return res
	}
	// 拿锁后再复核一次（disable/manual disable 可能发生在快照与锁之间）。
	if status, ok := p.cfg.Pool.Status(uid); !ok || status.Disabled || status.ManualDisabled {
		res.filtered = 1
		return res
	}

	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		res.errors++
		return res
	}
	if ctx.Err() != nil {
		res.cancelled = true
		return res
	}
	// mp 列表是默认口径的超集（实测含常规任务），失败静默（活动结束/无 mp 任务）。
	// 两套列表按 task_code 去重合并；仅 mp 列表才出现的码记入 mpOnly 以选择 mp 端点。
	mpOnly := map[string]bool{}
	if mpTasks, mpErr := p.cfg.Upstream.ListTasksMP(a); mpErr == nil {
		tasks, mpOnly = mergeTaskSweepTasks(tasks, mpTasks)
	}
	if ctx.Err() != nil {
		res.cancelled = true
		return res
	}

	// 只取真正可领的已完成任务：Claimable 由 upstream 用 progress 推算，额外
	// 排除已领/锁定/空 code。单轮内按 task_code 去重（防御上游列表重复下发）。
	claimable := make([]upstream.Task, 0, 8)
	seen := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		// Claimable 已由 upstream 按 progress 推算；这里再显式核对
		// target/current，防未来字段口径漂移时误领未达标任务。
		if !t.Claimable || t.Target <= 0 || t.Current < t.Target || t.Claimed || t.Locked || t.TaskCode == "" {
			continue
		}
		if seen[t.TaskCode] {
			continue
		}
		seen[t.TaskCode] = true
		claimable = append(claimable, t)
	}
	if len(claimable) > taskSweepMaxClaimsPerAccount {
		res.deferred = len(claimable) - taskSweepMaxClaimsPerAccount
		claimable = claimable[:taskSweepMaxClaimsPerAccount]
	}
	if len(claimable) == 0 {
		return res
	}

	// 领奖前置：只对「本轮即将 claim 且尚未 accepted/completed」的条目做 accept，
	// 不批量接受无关任务。默认/mp 口径必须分开提交（端标记不同）。
	var defaultCodes, mpCodes []string
	for _, t := range claimable {
		if t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" {
			continue
		}
		if taskSweepUsesMP(t.TaskCode, mpOnly) {
			mpCodes = append(mpCodes, t.TaskCode)
		} else {
			defaultCodes = append(defaultCodes, t.TaskCode)
		}
	}
	p.acceptTaskSweepCodes(ctx, a, defaultCodes, false, &res)
	if res.cancelled {
		return res
	}
	p.acceptTaskSweepCodes(ctx, a, mpCodes, true, &res)
	if res.cancelled {
		return res
	}

	// 逐项 claim：单项错误只计数、不阻断后续；claim 幂等（重复领返回
	// already_claimed=true → 0 分 0 能），宁可多试一轮也不漏领。
	for _, t := range claimable {
		if ctx.Err() != nil {
			res.cancelled = true
			return res
		}
		if t.Claimed || t.Locked || !t.Claimable {
			continue
		}
		var credit, energy int64
		var cErr error
		if taskSweepUsesMP(t.TaskCode, mpOnly) {
			credit, energy, cErr = p.cfg.Upstream.ClaimRewardMP(a, t.TaskCode)
		} else {
			credit, energy, cErr = p.cfg.Upstream.ClaimReward(a, t.TaskCode)
		}
		if cErr != nil {
			res.errors++
			continue
		}
		res.claimed++
		res.credit += credit
		res.energy += energy
	}
	return res
}

// acceptTaskSweepCodes 分批提交领奖前置 accept（每批最多 taskSweepAcceptBatch）。
// 失败只记一次错误并停止当前口径的后续批次；claim 仍会尝试，由上游兜底幂等。
func (p *Panel) acceptTaskSweepCodes(ctx context.Context, a *auth.Auth, codes []string, isMP bool, res *taskSweepResult) {
	for start := 0; start < len(codes); start += taskSweepAcceptBatch {
		if ctx.Err() != nil {
			res.cancelled = true
			return
		}
		end := start + taskSweepAcceptBatch
		if end > len(codes) {
			end = len(codes)
		}
		var err error
		if isMP {
			err = p.cfg.Upstream.AcceptTasksMP(a, codes[start:end])
		} else {
			err = p.cfg.Upstream.AcceptTasks(a, codes[start:end])
		}
		if err != nil {
			res.errors++
			return
		}
		res.accepted += end - start
	}
}

// mergeTaskSweepTasks 合并默认/mp 任务列表：按 task_code 去重，保留首次出现顺序；
// 同码重复项中若后项已达标可领而前项不可领，则用后项覆盖（防列表重复下发时
// 选中陈旧进度）。mpOnly 只记录「默认列表没有、仅 mp 列表出现」的码。
func mergeTaskSweepTasks(defaultTasks, mpTasks []upstream.Task) ([]upstream.Task, map[string]bool) {
	out := make([]upstream.Task, 0, len(defaultTasks)+len(mpTasks))
	index := make(map[string]int, len(defaultTasks)+len(mpTasks))
	mpOnly := make(map[string]bool)
	add := func(t upstream.Task, fromMP bool) {
		if t.TaskCode == "" {
			return
		}
		if i, ok := index[t.TaskCode]; ok {
			if !out[i].Claimable && t.Claimable {
				out[i] = t
			}
			return
		}
		index[t.TaskCode] = len(out)
		out = append(out, t)
		if fromMP {
			mpOnly[t.TaskCode] = true
		}
	}
	for _, t := range defaultTasks {
		add(t, false)
	}
	for _, t := range mpTasks {
		add(t, true)
	}
	return out, mpOnly
}

// taskSweepUsesMP 报告某任务是否必须走小程序口径的 accept/claim 端点。
func taskSweepUsesMP(code string, mpOnly map[string]bool) bool {
	return isMPTaskCode(code) || mpOnly[code]
}
