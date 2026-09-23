// Package scheduler 定时任务：签到 / 活跃上报 / 猫猫旅行 / token keepalive / 开学季 / 夜猫子 / 任务中心执行队列 七类独立排程。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// Config 调度器依赖。
//
// 任务开关用「禁用」命名而非「启用」：零值 Config 即六类任务都启用（hours 回落默认），
// 与引入开关前的行为逐字一致（老调用方/老测试无需改动）。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int // 默认 [9, 21]
	TravelHours    []int // 默认 [9,21]：一趟派出 + 一趟领奖闭环
	ActivityHours  []int // 默认 [10]
	KeepaliveHours []int // 默认 [22]
	SchoolHours    []int // 默认 [12]：开学季任务（迁移自系统 crontab）
	CatHours       []int // 默认 [1]：夜猫子任务（迁移自系统 crontab）
	QueueHours     []int // 默认 [10]：任务中心执行队列（成长任务 + 开学季闭环）
	// ActivityReportCount 每号每次活跃上报的条数：领猫前置需 5 次对话，
	// 默认 5 条同一 conversationId 内多轮上报把 chat_5 刷满；0/缺省=1 兼容旧行为。
	ActivityReportCount int

	// ExpiringSoonWindow 快过期积分窗口：签到查余额时，把到期时间 <= now+window 的
	// 套餐余额标记为"快过期"（pool 据此优先消耗，见 entry.creditsExpiring）。
	// <=0 时禁用分桶（全部归长期，行为与引入前一致）。默认建议 7*24h。
	ExpiringSoonWindow time.Duration

	// CheckinDisabled 显式关闭签到排程（对应 config 的 schedule.checkin_enabled=false）。
	// 禁用后不再有任何签到时点。旅行不再搭签到便车（已剥离为独立排程）。
	CheckinDisabled bool
	// TravelDisabled 显式关闭猫猫旅行排程（schedule.travel_enabled=false）。
	TravelDisabled bool
	// ActivityDisabled 显式关闭活跃上报排程（schedule.activity_enabled=false）。
	ActivityDisabled bool
	// KeepaliveDisabled 显式关闭 token 保活排程（schedule.keepalive_enabled=false）。
	KeepaliveDisabled bool
	// SchoolDisabled 显式关闭开学季任务排程（schedule.school_enabled=false）。
	SchoolDisabled bool
	// CatDisabled 显式关闭夜猫子任务排程（schedule.cat_enabled=false）。
	CatDisabled bool
	// QueueEnabled 任务中心执行队列排程开关（schedule.queue_enabled）。
	// 命名与上面六个相反（Enabled 而非 Disabled）：队列缺省关——它对全账号执行
	// 真实任务动作链（消耗上游配额），由用户显式打开；其余六类零值即启用是历史兼容。
	QueueEnabled bool
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	// mu/adoptTried 领养当日失败记录：uid → 自然日（CST）。门槛未达的账号当日不再重试，
	// 避免同日多趟对上游重试轰炸；进程重启即清零（无需持久化）。
	mu         sync.Mutex
	adoptTried map[string]string

	// rewardClaimed 连登奖励按自然日（CST）领取标记：uid → 当日日期。已领/已尝试的账号
	// 当日不再重打 redeem（领取类写操作按天幂等，避免对上游重复写请求）；自然日 00:00
	// CST 重置（上游增长体系按 CST 自然日刷新，见 travelDay/cstZone）。进程重启即清零
	// （服务端幂等兜底：重启后当日重复 redeem 会拿 409 正常态，无副作用）。
	rewardClaimed map[string]string

	// checkinMu 串行化签到：定时入口与手动触发互斥，避免同一时刻重复打上游签到接口。
	checkinMu sync.Mutex

	// —— 以下为 /admin 热管理与观测新增字段（不影响既有定时/CLI 行为）——
	//
	// hoursMu 保护 hoursTab：SetHours 热改（/admin PATCH 与面板保存配置）写、
	// nextWake/hoursOf/SnapshotAll 读。hours 缺席时回落 cfg（启动期装配的快照），
	// 因此不热改路径的行为与引入前逐位一致。
	hoursMu sync.RWMutex
	hoursTab [kindCount][]int
	// enabled 六类任务的可热改排程开关：New 时从 Config.*Disabled 取反初始化，
	// 之后 SetEnabled 原子改写并经 wake 唤醒 Run 重排定时器（热生效免重启）。
	// nextWake 只读本组标志，不再读 cfg 的 Disabled bool（cfg 保持不可变快照语义）。
	enabled [kindCount]atomic.Bool
	// wake 容量 1 的通知通道：SetEnabled 后让阻塞在旧 timer 上的 Run 立即重算。
	wake chan struct{}
	// runMu 每类任务一把锁：定时 dispatch 阻塞式排队，手动触发 TryLock 撞车回 ErrBusy。
	// （checkin 另有 CheckinAll 内部 checkinMu，历史语义保留不动。）
	runMu [kindCount]sync.Mutex
	// running/lastRun/lastOut 观测字段：正在执行 / 上次执行完成时间(unix) / 结果摘要。
	running [kindCount]atomic.Bool
	lastRun [kindCount]atomic.Int64
	lastOut [kindCount]atomic.Value // string

	// （原 rearmBalance/balanceInterval 字段已删：StartBalanceRefresh/SetBalanceInterval
	// 是无调用方的死代码，随 balance_refresh.go 的清理一并移除，见该文件头注释。）

	// queueRunner 任务中心执行队列的执行体（panel 的队列实现，main 装配期经
	// SetQueueRunner 注入；internal/scheduler 不能 import internal/panel——依赖
	// 方向相反，回调注入是唯一通路）。nil 时 dispatch 记 WARN 跳过（panel 未装配
	// 或旧调用方），不 panic。
	queueRunner func()
	// queueMu 保护 queueRunner 的注入与读取（装配后注入一次，热改免锁也无害，
	// 但 Go 内存模型上并发读写函数值仍需同步）。
	queueMu sync.RWMutex
}

// SetQueueRunner 注入任务中心执行队列的执行体（panel.QueueRunner，main 装配期
// 调用一次）。nil 清空。幂等：重复注入以最后一次为准。
func (s *Scheduler) SetQueueRunner(fn func()) {
	s.queueMu.Lock()
	s.queueRunner = fn
	s.queueMu.Unlock()
}

// kindCount 与 taskKind 枚举数量一致（checkin/travel/activity/keepalive/school/cat/queue）。
const kindCount = 7

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.TravelHours) == 0 {
		cfg.TravelHours = []int{9, 21}
	}
	if len(cfg.ActivityHours) == 0 {
		cfg.ActivityHours = []int{10}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	if len(cfg.SchoolHours) == 0 {
		cfg.SchoolHours = []int{12}
	}
	if len(cfg.CatHours) == 0 {
		cfg.CatHours = []int{1}
	}
	if len(cfg.QueueHours) == 0 {
		cfg.QueueHours = []int{10}
	}
	// 0/缺省 = 1 条（兼容旧行为：每号每天 1 条上报点亮连登）。
	if cfg.ActivityReportCount <= 0 {
		cfg.ActivityReportCount = 1
	}
	s := &Scheduler{cfg: cfg, adoptTried: make(map[string]string), rewardClaimed: make(map[string]string), wake: make(chan struct{}, 1)}
	// 排程开关初始化自 Config 的 Disabled 标志：零值 Config = 全部启用，与引入前逐字一致。
	s.enabled[taskCheckin].Store(!cfg.CheckinDisabled)
	s.enabled[taskTravel].Store(!cfg.TravelDisabled)
	s.enabled[taskActivity].Store(!cfg.ActivityDisabled)
	s.enabled[taskKeepalive].Store(!cfg.KeepaliveDisabled)
	s.enabled[taskSchool].Store(!cfg.SchoolDisabled)
	s.enabled[taskCat].Store(!cfg.CatDisabled)
	s.enabled[taskQueue].Store(cfg.QueueEnabled)
	for i := range s.lastOut {
		s.lastOut[i].Store("")
	}
	return s
}

// checkinRefreshSkew 签到前判定"token 是否临近过期"的时间窗口（10 分钟）。
// 长时间停机/容器长期停跑后 access token 往往已过期，不先刷新则签到必然 401 白跑。
const checkinRefreshSkew = 10 * time.Minute

// CheckinStatus 单账号签到结果状态。
type CheckinStatus string

const (
	CheckinOK      CheckinStatus = "ok"      // 签到成功
	CheckinAlready CheckinStatus = "already" // 上游判定今天已签到（幂等重复，视为正常）
	CheckinFail    CheckinStatus = "fail"    // 刷新 token / 签到 / 余额查询失败
	CheckinSkipped CheckinStatus = "skipped" // 禁用账号或无有效凭证，未参与
)

// CheckinOutcome 单账号签到结果（供手动签到回执与日志汇总）。
type CheckinOutcome struct {
	UID      string        `json:"uid"`
	Nickname string        `json:"nickname,omitempty"`
	Status   CheckinStatus `json:"status"`
	Credits  *int64        `json:"credits,omitempty"` // 签到后余额（余额查询成功才有值）
	Detail   string        `json:"detail,omitempty"`  // 失败/跳过原因（"已签到"不填）
}

// ErrBusy 已有一次签到正在执行（手动入口与定时撞车）。
var ErrBusy = errors.New("checkin already running")

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// taskKind 调度任务类型。
type taskKind int

const (
	taskCheckin taskKind = iota
	taskTravel
	taskActivity
	taskKeepalive
	taskSchool
	taskCat
	taskQueue
)

// nextWake 返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部任务。
// 多类任务若配到同一小时（如签到与旅行都含 9），该时刻多类任务需一并执行。
// 已显式禁用的任务不进候选（nextFire 对其零值返回零时间，nextWake 再跳过零时点）。
//
// 禁用判定读 enabled 原子标志（New 时初始化自 Config.*Disabled，之后可由
// SetEnabled 热改）——热改经 wake 唤醒 Run 重算，无需重启。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	type slot struct {
		at   time.Time
		kind taskKind
	}
	var slots []slot
	if s.enabled[taskCheckin].Load() {
		slots = append(slots, slot{nextFire(now, s.hoursOf(taskCheckin)), taskCheckin})
	}
	if s.enabled[taskTravel].Load() {
		slots = append(slots, slot{nextFire(now, s.hoursOf(taskTravel)), taskTravel})
	}
	if s.enabled[taskActivity].Load() {
		slots = append(slots, slot{nextFire(now, s.hoursOf(taskActivity)), taskActivity})
	}
	if s.enabled[taskKeepalive].Load() {
		slots = append(slots, slot{nextFire(now, s.hoursOf(taskKeepalive)), taskKeepalive})
	}
	if s.enabled[taskSchool].Load() {
		slots = append(slots, slot{nextFire(now, s.hoursOf(taskSchool)), taskSchool})
	}
	if s.enabled[taskCat].Load() {
		slots = append(slots, slot{nextFire(now, s.hoursOf(taskCat)), taskCat})
	}
	if s.enabled[taskQueue].Load() {
		slots = append(slots, slot{nextFire(now, s.hoursOf(taskQueue)), taskQueue})
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []taskKind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

// wakeupGraceDelay 迟到唤醒补跑的派发前网络宽限：Windows Modern Standby exit 后
// 网络栈/DNS 1-2s 才恢复（issue #152 实测 dial tcp lookup no such host 与
// Kernel-Power 507 standby exit ≤1s 重合），宽限 5s 覆盖 90%+ 唤醒场景。
// 只对迟到补跑生效（准点触发零延迟），零配置（分析报告裁定全套配置不成比例）。
// 测试可缩短（与 travelAccountDelay「测试可置 0」同口径）。
var wakeupGraceDelay = 5 * time.Second

// wakeupLateThreshold 迟到判定阈值：now 晚于槽位计划时刻超过 1s 才算迟到补跑。
// 毫秒级抖动（timer 正常触发的偏移量级）不算，避免准点触发被误宽限。
const wakeupLateThreshold = 1 * time.Second

// awaitWakeupGrace 迟到唤醒补跑派发前的网络宽限：槽位时刻已过点超过阈值
// （机器刚从睡眠唤醒）时先等满 wakeupGraceDelay 让网络栈/DNS 就绪再派发。
// 准点/阈值内抖动零延迟直接放行。ctx 取消立即返回 false（优雅停机不等宽限睡满，
// 本批放弃，下轮 nextWake 照旧从"现在"起算）。返回是否继续派发。
func awaitWakeupGrace(ctx context.Context, planned time.Time) bool {
	if late := time.Since(planned); late <= wakeupLateThreshold {
		return ctx.Err() == nil // 准点触发：零延迟放行
	}
	log.Printf("wakeup grace %s: late catch-up for slot %s", wakeupGraceDelay, planned.Format("15:04"))
	return sleepCtx(ctx, wakeupGraceDelay)
}

// Run 主循环，阻塞直到 ctx 取消。
//
// wake 分支（/admin 热改排程开关后由 SetEnabled 投递）：立即停掉旧 timer、
// 从"现在"重算 nextWake——禁用即撤排、启用即补排，全程不重启进程。
// runBatch 执行期间到达的 wake 会留在容量 1 的通道里，本批收尾后下一轮循环消费。
func (s *Scheduler) Run(ctx context.Context) {
	for {
		next, kinds := s.nextWake(time.Now())
		if next.IsZero() {
			// 六类任务全部禁用：不空转，等退出信号或热改启用通知。
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
				continue
			}
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			timer.Stop()
			continue
		case <-timer.C:
			// 到点任务在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
			// 迟到唤醒（睡眠跨过槽位时刻，timer 在唤醒瞬间才到期）先等网络宽限：
			// 唤醒瞬间 DNS 未就绪，零宽限派发等于把唯一一次补跑机会打在注定失败
			// 的窗口里（issue #152）；准点触发零延迟不受影响。
			if !awaitWakeupGrace(ctx, next) {
				return // ctx 取消：放弃本批，优雅退出
			}
			// 唤醒时全部并行派发：每类一个 goroutine，慢任务族（如活跃上报
			// 54 号 × 5 条 ≈ 7-8 分钟睡眠）不再阻塞同槽其他任务族；返回前
			// 等全部任务收尾（下一轮 nextWake 照旧从"现在"起算，多轮重叠
			// 的风险与串行版相同——nextWake 只挑现在之后的时点）。
			s.runBatch(ctx, kinds)
		}
	}
}

// runBatch 并行派发一批任务（同一唤醒时刻的多类任务），等全部完成返回。
// 供 Run 主循环与测试使用；ctx 取消时由各任务内部的 sleepCtx 快速收尾。
func (s *Scheduler) runBatch(ctx context.Context, kinds []taskKind) {
	var wg sync.WaitGroup
	for _, k := range kinds {
		wg.Add(1)
		go func(k taskKind) {
			defer wg.Done()
			s.dispatch(ctx, k)
		}(k)
	}
	wg.Wait()
}

// dispatch 按任务类型分发到对应执行函数。脚本类（school/cat）失败只记 WARN、
// 不影响其余任务继续执行（与现有各任务"单账号失败不阻断遍历"同口径）。
// ctx 传导给带账号间限速的遍历（取消时立即放弃剩余账号），纯脚本类任务不感知。
func (s *Scheduler) dispatch(ctx context.Context, k taskKind) {
	s.runMu[k].Lock()
	defer s.runMu[k].Unlock()
	s.runOne(ctx, k)
}

// runOne 单类任务真正执行体 + 观测记录（running/lastRun/lastOut）。
// 调用方必须已持有 runMu[k]（定时 dispatch 阻塞排队；手动 RunKindNow TryLock 独占）。
// checkin 分支把 RunCheckinNow 的 CheckinAll 结果收进摘要；撞车仅记日志的旧语义不变。
func (s *Scheduler) runOne(ctx context.Context, k taskKind) {
	s.running[k].Store(true)
	started := time.Now()
	var summary string
	switch k {
	case taskCheckin:
		out, err := s.CheckinAll()
		if err != nil {
			log.Printf("scheduled checkin skipped: %v", err)
			summary = "skipped: " + err.Error()
		} else {
			summary = summarizeCheckin(out)
		}
	case taskTravel:
		s.runTravel(ctx)
		summary = "done"
	case taskActivity:
		s.runActivity(ctx)
		summary = "done"
	case taskKeepalive:
		s.RunKeepaliveNow()
		summary = "done"
	case taskSchool:
		s.RunSchoolNow()
		summary = "done"
	case taskCat:
		s.RunCatNow()
		summary = "done"
	case taskQueue:
		summary = s.RunQueueNow()
	}
	// 收尾顺序：running 先于 lastRun/lastOut 落定——观测者（/admin 轮询方）看到
	// lastRun 更新时 running 必已复位，不存在"有结果却仍在跑"的中间态。
	s.running[k].Store(false)
	s.lastRun[k].Store(time.Now().Unix())
	s.lastOut[k].Store(fmt.Sprintf("%s (耗时 %s)", summary, time.Since(started).Round(time.Second)))
}

// summarizeCheckin 把逐账号签到回执压成一行摘要（供 /admin 观测展示）。
func summarizeCheckin(out []CheckinOutcome) string {
	var okN, alreadyN, failN, skipN int
	for _, o := range out {
		switch o.Status {
		case CheckinOK:
			okN++
		case CheckinAlready:
			alreadyN++
		case CheckinSkipped:
			skipN++
		default:
			failN++
		}
	}
	return fmt.Sprintf("ok=%d already=%d fail=%d skipped=%d", okN, alreadyN, failN, skipN)
}

// RunCheckinNow 定时触发的立即签到：逐账号结果由 CheckinAll 记日志，此处只兜住"撞车跳过"。
// 签到成功后追加连登管家（runStreakBonusTail 尾段：全档兑换 + 抽完抽奖次数；按天幂等，
// 与活跃上报侧的 claimGrowthRewards 共用同一 rewardClaimed 日闸）——panel 连登管家
// 并入签到尾部，不新增 schedule 键（设计文档 §3.8）。
func (s *Scheduler) RunCheckinNow() {
	if _, err := s.CheckinAll(); err != nil {
		log.Printf("scheduled checkin skipped: %v", err)
		return
	}
	s.runStreakBonusTail()
}

// runStreakBonusTail 签到尾段：逐可用账号执行连登管家（补签保连登 → 全档兑换 → 抽完）。
// 由 RunCheckinNow 尾部调用；单号失败只该号 WARN，不影响其他账号；账号间限速
// activityAccountDelay（与活跃上报同口径）。签到失败/撞车（ErrBusy）时跳过——
// 管家依赖签到刚建立的当日活跃态，撞车场景说明另一处签到正在跑，其尾部自会触发。
func (s *Scheduler) runStreakBonusTail() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		if a.IsGlobal() {
			continue // D4 门控：global 无 CN 任务体系，不发起任何上游调用
		}
		if s.rewardClaimedToday(a.UID) {
			continue // 当日已领过一轮（活跃上报侧已跑）：按天幂等
		}
		s.makeupYesterday(a) // 补签保连登（幂等写：无漏签/无卡静默）
		// 全档兑换 + 抽完抽奖次数。复用活跃上报侧的挑档/抽取实现（streakClaimTiers）。
		s.streakClaimTiers(a)
		time.Sleep(activityAccountDelay)
	}
}

// CheckinAll 全量签到：按需刷新 token → daily-checkin → 查余额 → 解冻冷却账号。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
// 同一时刻只允许一次签到在跑，重复调用返回 ErrBusy（防止手动触发与定时撞车重复打上游）。
//
// session dead 走 Pool.NoteSessionDead 的**连续计数**语义（与 keepalive 一致）：
// 一次刷新失败不再立即杀号，连续 sessionDeadThreshold 次才禁用，刷新成功清计数。
func (s *Scheduler) CheckinAll() ([]CheckinOutcome, error) {
	if !s.checkinMu.TryLock() {
		return nil, ErrBusy
	}
	defer s.checkinMu.Unlock()

	statuses := s.cfg.Pool.List()
	out := make([]CheckinOutcome, 0, len(statuses))
	var okN, alreadyN, failN, skipN int
	for _, st := range statuses {
		oc := CheckinOutcome{UID: st.UID, Nickname: st.Nickname}
		if st.Disabled {
			oc.Status, oc.Detail = CheckinSkipped, "disabled"
			skipN++
			out = append(out, oc)
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			oc.Status, oc.Detail = CheckinSkipped, "no credentials"
			skipN++
			out = append(out, oc)
			continue
		}
		// D4 门控：realm=global 账号无签到体系/任务中心，直接跳过（不发起任何上游调用，避免风控）。
		// 经 auth.Realm() 统一判定：逃生门（global.enabled=false）下 global 账号被降级为 cn、
		// 按 CN 处理——这是 D5 逃生门的刻意语义（纯 CN 部署锁死一切 global），与引用处一致。
		if a.IsGlobal() {
			oc.Status, oc.Detail = CheckinSkipped, "global"
			skipN++
			out = append(out, oc)
			continue
		}
		// 停机跨过 token 有效期（关机过夜/容器长期停跑）时先补一次刷新，否则签到必然 401 白跑。
		if a.NeedsRefresh(checkinRefreshSkew) {
			if err := s.cfg.Upstream.RefreshToken(a); err != nil {
				log.Printf("checkin %s refresh: %v", logfmt.Label(st.UID, st.Nickname), err)
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					if s.cfg.Pool.NoteSessionDead(st.UID) {
						log.Printf("WARN: checkin %s: 连续 %d 次 12153 session dead — 禁用", logfmt.Label(st.UID, st.Nickname), pool.SessionDeadThreshold())
					}
				}
				// 刷新只是"提前补票"：token 若仍有效，继续照常签到（否则刷新接口抖动
				// 会让本可成功的签到被白白跳过）；真正过期才判定失败。
				if a.NeedsRefresh(0) {
					oc.Status, oc.Detail = CheckinFail, "refresh: "+err.Error()
					failN++
					out = append(out, oc)
					continue
				}
			} else {
				a.BackfillRealm() // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
				if err := a.SaveAtomic(); err != nil {
					// 刷新成功但落盘失败：重启会用旧 token，必须暴露。
					log.Printf("checkin %s save: %v", logfmt.Label(st.UID, st.Nickname), err)
				}
			}
		}
		// 签到返回错误（含"今天已签到"）也继续查余额：余额恢复即可解冻账号。
		if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
			if upstream.IsAlreadyCheckin(err) {
				// "今天已签到"是幂等成功，不是错误：不填 detail，免得回执里
				// 出现一整段 400 报文、被误读成签到失败。
				oc.Status = CheckinAlready
			} else {
				oc.Status = CheckinFail
				oc.Detail = err.Error()
				log.Printf("checkin %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			}
		} else {
			oc.Status = CheckinOK
		}
		// 分桶查余额：快过期窗口内的积分单独标记，pool 优先消耗（issue:积分过期）。
		// ExpiringSoonWindow<=0 时退化为纯总量（与引入前一致）。
		remain, buckets, err := s.cfg.Upstream.UserResourceDetailed(a, s.cfg.ExpiringSoonWindow)
		if err != nil {
			log.Printf("user-resource %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			oc.Status = CheckinFail
			oc.Detail = joinDetail(oc.Detail, "resource: "+err.Error())
			failN++
			out = append(out, oc)
			continue
		}
		s.cfg.Pool.ReenableIfCredits(st.UID, remain)
		s.cfg.Pool.SetCreditsDetailed(st.UID, remain, buckets.Expiring)
		oc.Credits = &remain
		switch oc.Status {
		case CheckinOK:
			okN++
		case CheckinAlready:
			alreadyN++
		default:
			failN++
		}
		out = append(out, oc)
	}
	log.Printf("checkin done: total=%d ok=%d already=%d fail=%d skipped=%d",
		len(statuses), okN, alreadyN, failN, skipN)
	return out, nil
}

// joinDetail 拼接多段原因，避免后一段覆盖前一段的失败信息。
func joinDetail(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}

// RunActivityNow 立即对池内所有可用账号执行对话活跃上报。
// 禁用账号跳过；无 AccessToken 的跳过；账号间限速 activityAccountDelay。
// CN 与 global 账号**都上报**（PR #45 实测国际版 /v2/report 可用）；单账号失败
// 只记 WARN 不影响遍历。
//
// 每号上报 N 条（ActivityReportCount，默认 5）：N 条共用同一 conversationId
// （wb2api-<ms>），模拟同一会话内 N 轮对话——这是领养猫（buddy/first）对话量
// 门槛的实测刷法（chat_5 前置需 5 次对话）。requestId 各条独立（同会话多轮）。
// 账号内 N 条之间间隔 activityReportGap（1.5s）避免秒发触发风控。
//
// 0/缺省 ActivityReportCount = 1 条，兼容旧行为（仅点亮连登 + 解锁 first_buddy）。
//
// 上报成功后：① streak 自检（回读连登，发现「200 但静默丢弃」）；
// ② 无猫账号立即重试领养（travelAdoptForce）——对话量刚补满的新状态，不算重试，
// 豁免 adoptTriedToday 当日防抖（旅行排程 09 点已领养过且 skip，10 点上报补满后
// 不能依赖下一轮旅行领养，就地闭环）。
// RunActivityNow 立即对池内所有可用账号执行对话活跃上报（无 ctx 的外部入口：
// cmd/activity 一次性触发、测试）。内部走 runActivity，取背景 ctx（不可取消，
// 语义与引入前 time.Sleep 版一致）。
func (s *Scheduler) RunActivityNow() {
	s.runActivity(context.Background())
}

// runActivity 活跃上报遍历，随 ctx 取消立即退出。
func (s *Scheduler) runActivity(ctx context.Context) {
	count := s.cfg.ActivityReportCount
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		// global 账号同样上报（PR #45 实测国际版 /v2/report 在 workbuddy.ai 上 code=0 OK，
		// 点亮连登）；realmBase 路由/头由 upstream.billingJSON/BillingHeaders 按 realm 切。
		// 单账号失败只记 WARN 不影响遍历（下方 report err → break 该号 → continue 下号）。
		if !first {
			if !sleepCtx(ctx, activityAccountDelay) {
				return // 优雅停机：不等限速睡满，剩余账号下轮再报
			}
		}
		first = false
		// N 条共用同一 conversationId（同会话），requestId 各自独立（每条一个）。
		cid := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
		ok := 0
		for i := 1; i <= count; i++ {
			rid := fmt.Sprintf("%s-r%d", cid, i)
			if err := s.cfg.Upstream.ReportChatActivity(a, cid, rid); err != nil {
				log.Printf("activity %s: report %d/%d: %v", logfmt.Label(a.UID, a.Nickname), i, count, err)
				break // 本号上报失败：不再续发，streak 自检无意义
			}
			log.Printf("activity %s: report %d/%d ok", logfmt.Label(a.UID, a.Nickname), i, count)
			ok++
			if i < count {
				// 账号内 5 条之间间隔，避免秒发风控；取消时立即放弃本号剩余条数。
				if !sleepCtx(ctx, activityReportGap) {
					return
				}
			}
		}
		if ok < count {
			continue // N 条未发满：streak 自检与领养均无意义，下个账号
		}
		s.checkActivityStreak(a) // N 条全发满 → 回读 streak 自检（只留结论行）
		if !a.IsGlobal() {
			// 无猫账号对话量刚补满 → 立即重试领养（豁免防抖）。global 无 CN 领养体系：
			// adoptBuddy 前置 report（v1.2.0）会对 global 发 /v2/report 之外的 growth 写，
			// 故在调用侧门控（PR #45 的 global 上报放行语义不变）。
			s.travelAdoptForce(a)
		}
		s.claimGrowthRewards(a) // 连登奖励 + 抽奖：点亮连登后按天领取（finally 语义：失败不拖累上报；global 内部门控）
	}
}

// checkActivityStreak 上报成功后回读连登天数（只读 oracle，发现静默失败）。
// 背景：REPORT-active-map.md §2 实测「上报 200 但静默丢弃」（缺 userId 时 progress 不动），
// 上报 200 ≠ streak 计分——需要回读验证闭环。
// 异常检测口径：days==0 → warn（report OK but streak.days=0 (silent drop?)）；
// GET 失败 → warn 但不影响主流程（上报本身已成功，按天幂等，不做重试）。
// 日志每号一行、一眼可 grep：`activity %s: streak days=%d`（成功也打，方便对账）。
// 返回 true 表示「上报 OK 但 streak 可疑」（days==0 或回读失败），供测试断言。
func (s *Scheduler) checkActivityStreak(a *auth.Auth) bool {
	days, err := s.cfg.Upstream.GrowthStreak(a)
	if err != nil {
		log.Printf("WARN: activity %s: streak check failed (report OK): %v", logfmt.Label(a.UID, a.Nickname), err)
		return true
	}
	if days == 0 {
		log.Printf("WARN: activity %s: report OK but streak.days=0 (silent drop?)", logfmt.Label(a.UID, a.Nickname))
		return true
	}
	log.Printf("activity %s: streak days=%d", logfmt.Label(a.UID, a.Nickname), days)
	return false
}

// claimGrowthRewards 领取连登奖励（里程碑兑换）+ 执行连登抽奖。
// 在 runActivity 上报成功 + streak 自检之后调用：连登达标（days>=某档）才能领奖，
// 领奖送的 chances 才是抽奖次数来源，故先领奖后抽奖。
//
// 全链（panel 连登管家吸收版）：礼包/补偿领取 → 读 reward-state → 补签保连登
// （补签成功重读 state 吃恢复后的天数）→ 挑档 redeem → chances → draw。
//
// 幂等/风控语义（与现有 travel/travel 同口径：单号失败只该号 WARN，不影响其他账号）：
//   - 按天幂等：每日每号最多领一轮（rewardClaimedToday 闸；自然日 CST 重置，进程重启清零——
//     重启后当日重复 redeem 由上游 409 duplicate 正常态兜底，不刷 WARN）。
//   - 服务端正常态识别为静默跳过：redeem 409 duplicate/403 天数不足；lottery 400 无次数/未开启——
//     这些不是失败，不刷 WARN（见 upstream.IsRedeemAlreadyClaimed 等）。
//   - 领奖只领「本次新达标」的档位：状态非 claimed 且 days>=档位天数。跨档连领（14d 未领而
//     days 已到 28）是官方正常态（spa 按 byTier 逐档可兑），但每日一轮限一档，避免同日多写。
//   - 礼包/补偿是「有则领」的幂等写，业务错误静默；补签只在「昨日漏签且有卡」时触发，
//     无卡/无漏签不写。
//
// 日志每号一行可 grep：`activity %s: redeem tier=%s ...` / `activity %s: lottery ...`。
func (s *Scheduler) claimGrowthRewards(a *auth.Auth) {
	if a == nil || a.AccessTokenValue() == "" {
		return
	}
	// global 门控：连登奖励/抽奖链只服务 CN。国际版 /activity/growth/* 端点虽同构存在
	// （/tmp/analysis-global-credit.md §1.1：lottery/streak/redeem 在国际版上线），但真实
	// global 新账号 GET /activity/growth/streak 返回 500（实测 sliverkiss）——链上第一步就
	// 拿不到 days，无法挑档；且 streak 500 会每趟刷 WARN 污染日志。结论：证据不足，跳过
	// global（不发起任何领取类调用）。CN 账号无此问题（CN streak 200 days=N）。
	if a.IsGlobal() {
		return
	}
	if s.rewardClaimedToday(a.UID) {
		return // 当日已领过一轮，跳过（按天幂等）
	}
	// 0. 礼包/补偿领取（panel 连登管家口径）：每号一次 / 有则领的幂等写，
	// 业务错误静默跳过（不刷 WARN），先于 redeem——到账积分不依赖连登状态。
	s.claimGrowthBonus(a)
	state, err := s.cfg.Upstream.GrowthRewardState(a)
	if err != nil {
		log.Printf("WARN: activity %s: reward-state: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	// 0.5 补签保连登（panel makeupYesterday 口径）：昨日漏签（heatmap score==0）且
	// 有补签卡 → 补昨日。放在 reward-state 之后：若补签把 streak 恢复到新档位，
	// 重读 state 让本日 redeem 直接吃到恢复后的天数（补签是保 7d/14d/28d 里程碑的关键）。
	if s.makeupYesterday(a) {
		if st2, err2 := s.cfg.Upstream.GrowthRewardState(a); err2 == nil {
			state = st2 // 补签成功 → 用恢复后的天数挑档
		}
	}
	days := state.Days()
	tier := growthEligibleTier(days, &state.Redemption)
	if tier == "" {
		// 无新达标档位：不动写接口（不刷 WARN，这是正常态——很多天没到 7d）。
		return
	}
	res, err := s.cfg.Upstream.GrowthRedeem(a, tier, "")
	switch {
	case err == nil:
		log.Printf("activity %s: redeem tier=%s ok (+%d credit, +%d energy, +%d chances)",
			logfmt.Label(a.UID, a.Nickname), tier, res.CreditGranted, res.EnergyGranted, res.ChancesGranted)
	case upstream.IsRedeemAlreadyClaimed(err) || upstream.IsRedeemNotEnoughDays(err):
		log.Printf("activity %s: redeem tier=%s skip (already claimed or days not enough)", logfmt.Label(a.UID, a.Nickname), tier)
	default:
		log.Printf("activity %s: redeem tier=%s: %v", logfmt.Label(a.UID, a.Nickname), tier, err)
	}
	// 标记当日已处理（无论 redeem 是否成功都记一次：领取类各状态当日不再重试，
	// 避免对上游重复写；成功→无需再领，失败→当日不轰炸，次日自然日重置/上游幂等兜底）。
	s.markRewardClaimed(a.UID)
	s.claimGrowthLottery(a)
}

// streakClaimTiers 连登兑换链的公共尾段（panel 连登管家口径）：
// 读 reward-state → 挑所有「达标未领」档位逐档 redeem → 查 chances → 抽完。
// 与旧 claimGrowthRewards 的差异： redeem 失败不中断后续档位（403 天数不足是正常态）；
// 抽奖改为 claimGrowthLottery 的全抽完语义。调用方负责日闸（rewardClaimedToday）
// 与标记（markRewardClaimed）。
func (s *Scheduler) streakClaimTiers(a *auth.Auth) {
	state, err := s.cfg.Upstream.GrowthRewardState(a)
	if err != nil {
		log.Printf("WARN: streak-bonus %s: reward-state: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	days := state.Days()
	redeemed := 0
	for i := len(state.Redemption.Tiers) - 1; i >= 0; i-- {
		tier := state.Redemption.Tiers[i].Tier
		if days < state.Redemption.Tiers[i].Days || state.Redemption.Claimed(tier) {
			continue
		}
		res, err := s.cfg.Upstream.GrowthRedeem(a, tier, "")
		switch {
		case err == nil:
			redeemed++
			log.Printf("streak-bonus %s: redeem tier=%s ok (+%d credit, +%d energy, +%d chances)",
				logfmt.Label(a.UID, a.Nickname), tier, res.CreditGranted, res.EnergyGranted, res.ChancesGranted)
		case upstream.IsRedeemAlreadyClaimed(err) || upstream.IsRedeemNotEnoughDays(err):
			// 正常态：已领/服务端判定不足，静默跳过（跨档竞态兜底）。
		default:
			log.Printf("streak-bonus %s: redeem tier=%s: %v", logfmt.Label(a.UID, a.Nickname), tier, err)
		}
	}
	if redeemed == 0 {
		// 无新达标档位：不动抽奖（兑换送次数才有的抽）。
		return
	}
	s.markRewardClaimed(a.UID)
	s.claimGrowthLottery(a)
}

// growthEligibleTier 按当前连登天数挑选「尚未领取且达标」的最高档位。
// 返回 "" 表示无可领档（未达标或全部已领），调用方据此跳过 redeem（正常态）。
func growthEligibleTier(days int, rs *upstream.GrowthRedemptionStatus) string {
	if rs == nil {
		return ""
	}
	// 档位按 days 升序，从高到低挑最高的已达标未领档（一次领一份，每日一轮）。
	for i := len(rs.Tiers) - 1; i >= 0; i-- {
		sp := rs.Tiers[i]
		if days >= sp.Days && !rs.Claimed(sp.Tier) {
			return sp.Tier
		}
	}
	return ""
}

// claimGrowthBonus 新手礼包 + 活动补偿领取（panel 连登管家口径）。
// 两者都是幂等写：礼包每号一次（已领业务错误静默）、补偿有则领（无则业务错误静默）。
// 只在成功到账时打日志（每号一生一次的事件，不值得每日刷行）；失败静默——
// 业务错误是常态（绝大多数号早已领过），无法与真错误可靠区分，不刷 WARN。
func (s *Scheduler) claimGrowthBonus(a *auth.Auth) {
	if credit, err := s.cfg.Upstream.ClaimGift(a); err == nil && credit > 0 {
		log.Printf("activity %s: gift ok (+%d credit)", logfmt.Label(a.UID, a.Nickname), credit)
	}
	if credit, err := s.cfg.Upstream.ClaimCompensation(a); err == nil && credit > 0 {
		log.Printf("activity %s: compensation ok (+%d credit)", logfmt.Label(a.UID, a.Nickname), credit)
	}
}

// makeupYesterday 昨日漏签且有补签卡时自动补签（保住连登连续天数，panel 口径）。
// 连续天数一断就要重攒 7 天，一张卡代价远小——有漏签 + 有卡即补。
// 判据链：heatmap 昨日格 score==0（漏签）→ streak.makeup_cards.balance>0（有卡）
// → POST makeup-cards/use {"target_date":昨日}。
// 无卡 / 无漏签 / 无该日格 / 查询失败均静默返回 false（不影响主流程）；
// 补签成功打一行日志并返回 true（调用方重读 streak 天数挑档）。
func (s *Scheduler) makeupYesterday(a *auth.Auth) bool {
	cells, err := s.cfg.Upstream.GrowthHeatmap(a)
	if err != nil {
		return false // 只读判据失败：静默（每日重试，无写风险）
	}
	yesterday := upstream.GrowthYesterdayDate(time.Now())
	score, ok := upstream.HeatmapDayScore(cells, yesterday)
	if !ok || score != 0 {
		return false // 昨日有分或无判据：无需补签
	}
	// 有漏签 → 查补签卡余额（streak 端点同一响应体）。
	st, err := s.cfg.Upstream.GrowthStreakWithCards(a)
	if err != nil || st.MakeupCards.Balance <= 0 {
		return false // 无卡或查询失败：静默（次日再判）
	}
	if err := s.cfg.Upstream.UseMakeupCard(a, yesterday); err != nil {
		log.Printf("activity %s: makeup %s: %v", logfmt.Label(a.UID, a.Nickname), yesterday, err)
		return false
	}
	log.Printf("activity %s: makeup ok %s (+streak kept)", logfmt.Label(a.UID, a.Nickname), yesterday)
	return true
}

// claimGrowthLottery 抽完当前全部抽奖次数（panel 连登管家口径：全抽完而非抽一次）。
// 余额查询失败记 WARN 返回；无次数静默跳过；单抽失败即停（剩余次数次日幂等重试）。
// 400 无次数/未开启是正常态静默。client_token 每次 draw 必须新键
// （security-relevant，见 upstream.GrowthLotteryDraw）。
func (s *Scheduler) claimGrowthLottery(a *auth.Auth) {
	chances, err := s.cfg.Upstream.GrowthLotteryChances(a)
	if err != nil {
		log.Printf("WARN: activity %s: lottery-chances: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	for i := 0; i < chances; i++ {
		res, err := s.cfg.Upstream.GrowthLotteryDraw(a, "") // 每次自动新 client_token
		switch {
		case err == nil:
			log.Printf("activity %s: lottery %d/%d drawn prize=%s (%s)", logfmt.Label(a.UID, a.Nickname), i+1, chances, res.PrizeName, res.PrizeType)
		case upstream.IsLotteryNoChance(err) || upstream.IsLotteryDisabled(err):
			log.Printf("activity %s: lottery skip (no chances or disabled)", logfmt.Label(a.UID, a.Nickname))
			return
		default:
			log.Printf("activity %s: lottery draw: %v", logfmt.Label(a.UID, a.Nickname), err)
			return
		}
	}
	if chances > 0 {
		log.Printf("activity %s: lottery done %d draw(s)", logfmt.Label(a.UID, a.Nickname), chances)
	}
}

// rewardClaimedToday 该账号当日是否已处理过连登奖励领取（自然日 CST）。
func (s *Scheduler) rewardClaimedToday(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rewardClaimed[uid] == travelDay(time.Now())
}

// markRewardClaimed 记录该账号当日已处理连登奖励领取。
func (s *Scheduler) markRewardClaimed(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rewardClaimed[uid] = travelDay(time.Now())
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
// 12153 禁用走 Pool.NoteSessionDead 的**连续计数**语义：一次刷新失败不再立即杀号，
// 连续 sessionDeadThreshold 次（3 次）才禁用（P0-1：13 个 disabled 号全是历史误判）。
// 刷新成功 → ClearSessionDead 清计数（错误判定的账号有复活路径）。
func (s *Scheduler) RunKeepaliveNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("WARN: keepalive %s: 连续 %d 次 12153 session dead — 禁用", logfmt.Label(st.UID, st.Nickname), pool.SessionDeadThreshold())
				}
			}
			continue
		}
		s.cfg.Pool.ClearSessionDead(st.UID) // 刷新成功清误判计数，失败不该累计
		a.BackfillRealm()                   // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", logfmt.Label(st.UID, st.Nickname), err)
		}
	}
}

// RunMinichatNow 立即执行小程序成长任务（Sequential_Tasks_1）：
// task_runner.py ALL --yes --only Sequential_Tasks_1。
// 仅手动触发（本地菜单专用）：不进定时排程、不占 taskKind 枚举——判据是一次
// mini 指纹对话（写操作），每天至多点亮一次，已领/未完成的幂等判定在
// task_runner 专段内部完成。
func (s *Scheduler) RunMinichatNow() {
	root := repoRoot()
	runScript("minichat", root, [][]string{
		{pythonCmd(), "scripts/task_runner.py", "ALL", "--yes", "--only", "Sequential_Tasks_1"},
	})
}

// ============================================================================
// /admin 热管理与观测（server 包的 /admin 端点依赖；对既有定时/CLI 行为零影响）
// ============================================================================

// kindNames/kindLabels 七类任务的稳定字符串标识与中文显示名（顺序与 taskKind 枚举一致）。
var kindNames = [kindCount]string{"checkin", "travel", "activity", "keepalive", "school", "cat", "queue"}

var kindLabels = [kindCount]string{"签到", "猫猫旅行", "活跃上报", "Token 保活", "开学季", "夜猫子", "任务队列"}

// Kinds 返回六类任务的字符串标识（枚举顺序），供外部遍历与参数校验。
func Kinds() []string {
	out := make([]string, 0, kindCount)
	out = append(out, kindNames[:]...)
	return out
}

// KindFromName 字符串标识 → taskKind。
func KindFromName(name string) (taskKind, bool) {
	for i, n := range kindNames {
		if n == name {
			return taskKind(i), true
		}
	}
	return 0, false
}

// SetEnabled 热改某类任务的排程开关：原子更新后通知 Run 主循环重排定时器（免重启）。
// 只影响"定时自动跑"，不影响 RunKindNow/cmd/task 的手动触发语义。
// 返回值 ok=false 表示 name 不是合法任务标识。
func (s *Scheduler) SetEnabled(name string, on bool) (ok bool) {
	k, exist := KindFromName(name)
	if !exist {
		return false
	}
	if s.enabled[k].Swap(on) != on {
		select {
		case s.wake <- struct{}{}:
		default: // 已有一次未消费的通知：重算本来就是幂等的，无需排队
		}
	}
	return true
}

// KindSnapshot 单类任务的观测快照（供 GET /admin/tasks）。
type KindSnapshot struct {
	Kind        string `json:"kind"`
	Label       string `json:"label"`
	Enabled     bool   `json:"enabled"`
	Hours       []int  `json:"hours"`
	NextFire    string `json:"next_fire"` // RFC3339；""= 已禁用（无排程时点）
	Running     bool   `json:"running"`
	LastRunUnix int64  `json:"last_run_unix"` // 0 = 本进程启动以来未执行过
	LastResult  string `json:"last_result"`   // 上次执行摘要；""= 未执行过
}

// SnapshotAll 六类任务的观测快照（枚举顺序）。hours 为 cfg 不可变快照的拷贝。
func (s *Scheduler) SnapshotAll() []KindSnapshot {
	out := make([]KindSnapshot, 0, kindCount)
	for i := range kindCount {
		k := taskKind(i)
		sn := KindSnapshot{
			Kind:        kindNames[k],
			Label:       kindLabels[k],
			Enabled:     s.enabled[k].Load(),
			Hours:       append([]int(nil), s.hoursOf(k)...),
			Running:     s.running[k].Load(),
			LastRunUnix: s.lastRun[k].Load(),
		}
		if v, _ := s.lastOut[k].Load().(string); v != "" {
			sn.LastResult = v
		}
		if sn.Enabled {
			sn.NextFire = nextFire(time.Now(), s.hoursOf(k)).Format(time.RFC3339)
		}
		out = append(out, sn)
	}
	return out
}

// RunKindNow 手动触发单类任务（供 POST /admin/tasks/run，异步 goroutine 中调用）：
// 与定时器或另一次手动撞车时返回 ErrBusy，不排队不重复打上游。
// 不看排程开关——禁用中的任务同样允许手动执行一次（与 cmd/task 语义一致）。
// 执行完成（含失败）后写 running/lastRun/lastOut 观测字段。
func (s *Scheduler) RunKindNow(name string) error {
	k, ok := KindFromName(name)
	if !ok {
		return fmt.Errorf("unknown task kind %q", name)
	}
	if !s.runMu[k].TryLock() {
		return ErrBusy
	}
	defer s.runMu[k].Unlock()
	s.runOne(context.Background(), k)
	return nil
}

// hoursOf 单类任务的排程小时表：优先 SetHours 热改值，缺席回落 cfg 快照。
// 读锁内逐项拷贝不可变切片本身（切片头），调用方拿到的切片内容此后不再变化
// （SetHours 整体替换切片，从不就地改元素）。
func (s *Scheduler) hoursOf(k taskKind) []int {
	s.hoursMu.RLock()
	defer s.hoursMu.RUnlock()
	if h := s.hoursTab[k]; h != nil {
		return h
	}
	return s.cfgHours(k)
}

// cfgHours 启动期装配的小时表（cfg 不可变快照，New 归一化过缺省值）。
func (s *Scheduler) cfgHours(k taskKind) []int {
	switch k {
	case taskCheckin:
		return s.cfg.CheckinHours
	case taskTravel:
		return s.cfg.TravelHours
	case taskActivity:
		return s.cfg.ActivityHours
	case taskKeepalive:
		return s.cfg.KeepaliveHours
	case taskSchool:
		return s.cfg.SchoolHours
	case taskCat:
		return s.cfg.CatHours
	case taskQueue:
		return s.cfg.QueueHours
	default:
		return s.cfg.CatHours
	}
}

// SetHours 热改某类任务的排程小时表（/admin PATCH hours 与面板保存配置）：
// 替换后通知 Run 主循环重排定时器，无需重启进程。空数组与 nil 拒绝（语义是
// 「禁用」，走 SetEnabled，见 config.Schedule 的历史决定）；非法小时拒绝。
// 返回 ok=false 表示 kind 非法或 hours 非法。
func (s *Scheduler) SetHours(name string, hours []int) bool {
	k, exist := KindFromName(name)
	if !exist || len(hours) == 0 {
		return false
	}
	for _, h := range hours {
		if h < 0 || h > 23 {
			return false
		}
	}
	cp := append([]int(nil), hours...)
	s.hoursMu.Lock()
	s.hoursTab[k] = cp
	s.hoursMu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default: // 已有未消费通知：重排幂等，无需排队
	}
	return true
}
