// taskcenter.go 面板「任务中心」：全账号任务扫描 + 执行队列（可配并发）+
// 开学季独立状态。解决"不知道哪些账号有哪些任务没做"与"开学季状态不可见"。
//
// 语义：
//   - 扫描（scan_all）：并发拉取每账号的成长任务列表 + 开学季任务列表，
//     汇总出"未完成且可自动化"的待办清单（只读，不执行）。
//   - 执行队列（run_queue + queue）：把待办项按账号分组排队执行——账号内
//     串行（复用 per-account 锁，与单任务/一键完成互斥），账号间并发
//     （concurrency 信号量限制，默认 1）。队列状态可轮询。
//   - 开学季（school/status + school/run_all）：独立状态视图 + 一键闭环。
package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// ---------------------------------------------------------------------------
// 扫描（只读）
// ---------------------------------------------------------------------------

// schoolTaskView 开学季任务条目（面板展示口径）。
type schoolTaskView struct {
	Code   string `json:"task_code"`
	Status string `json:"status"` // pending | in_progress | completed | claimed
	Prog   int    `json:"progress"`
	Target int    `json:"target_count"`
}

// scanAccountItem 单账号扫描结果。
type scanAccountItem struct {
	UID       string           `json:"uid"`
	Nickname  string           `json:"nickname"`
	Growth    []upstream.Task  `json:"growth,omitempty"`
	GrowthErr string           `json:"growth_error,omitempty"`
	School    []schoolTaskView `json:"school,omitempty"`
	SchoolErr string           `json:"school_error,omitempty"`
	InPeriod  bool             `json:"in_period"`
}

// growthPending 任务是否"未完成且可自动化"。
func growthPending(t upstream.Task) bool {
	if t.Claimed {
		return false
	}
	if t.Target > 0 && t.Current >= t.Target {
		return false // 达标未领：也入队（队列执行后会自动领）
	}
	return autoActionFor(t.TaskCode) != nil
}

// schoolPending 开学季任务是否待办（排除学生认证）。
func schoolPending(t upstream.SchoolTask) bool {
	switch t.TaskCode {
	case "task_student_verify":
		return false // 需微信学生真实认证
	case "desktop_chat_1_time":
		return t.Status != "claimed"
	default:
		return t.Status != "claimed" && t.Status != "completed"
	}
}

// tasksScanAll 扫描全部账号：成长任务（未完成+可自动化）+ 开学季（未完成）。
// 只读操作，并发拉取（账号数个位数）。
func (p *Panel) tasksScanAll(w http.ResponseWriter, r *http.Request) {
	states := p.cfg.Pool.List()
	items := make([]scanAccountItem, len(states))
	var wg sync.WaitGroup
	for i, st := range states {
		if st.Disabled {
			continue
		}
		wg.Add(1)
		go func(i int, uid string) {
			defer wg.Done()
			a := p.cfg.Pool.AuthByUID(uid)
			if a == nil {
				return
			}
			it := &items[i]
			it.UID, it.Nickname = uid, a.Nickname
			// D4 门控：global 账号无 CN 成长/开学季任务体系，不发起任何上游调用。
			if a.IsGlobal() {
				return
			}
			if tasks, err := p.cfg.Upstream.ListTasks(a); err != nil {
				it.GrowthErr = err.Error()
			} else {
				for _, t := range tasks {
					if growthPending(t) {
						it.Growth = append(it.Growth, t)
					}
				}
			}
			// 小程序口径任务（school_season 校园日 / Sequential_Tasks_1 小程序首对话）
			// 仅在 mp 头列表下发，与默认口径不重叠——合并进待办列表；mp 列表失败
			// 静默（无 mp 任务的部署/活动结束时零影响）。
			if mpTasks, err := p.cfg.Upstream.ListTasksMP(a); err == nil {
				seen := map[string]bool{}
				for _, t := range it.Growth {
					seen[t.TaskCode] = true
				}
				for _, t := range mpTasks {
					if growthPending(t) && !seen[t.TaskCode] {
						it.Growth = append(it.Growth, t)
					}
				}
			}
			if stasks, inPeriod, err := p.cfg.Upstream.SchoolTasks(a); err != nil {
				it.SchoolErr = err.Error()
			} else {
				it.InPeriod = inPeriod
				for _, t := range stasks {
					if schoolPending(t) {
						it.School = append(it.School, schoolTaskView{
							Code: t.TaskCode, Status: t.Status, Prog: t.Progress, Target: t.TargetCount,
						})
					}
				}
			}
		}(i, st.UID)
	}
	wg.Wait()
	pending := 0
	for _, it := range items {
		pending += len(it.Growth) + len(it.School)
	}
	log.Printf("panel: 队列扫描完成：全部账号待办 %d 项（成长+开学季）", pending)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": items, "pending_count": pending})
}

// ---------------------------------------------------------------------------
// 执行队列
// ---------------------------------------------------------------------------

// queueItem 队列执行单元。
type queueItem struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Kind     string `json:"kind"` // growth | school
	Code     string `json:"code"`
	Status   string `json:"status"` // pending | running | done | skipped | error | cancelled
	Message  string `json:"message,omitempty"`
}

// queueState 队列运行状态。Seq 每次启动 +1——前端只渲染"自己启动的那一轮"，
// 执行结束后的残留 items 不会覆盖后续的扫描结果视图。
type queueState struct {
	mu        sync.Mutex
	running   bool
	cancelReq bool // 手动取消请求：剩余 pending 待办停止调度，进行中条目不打断
	startedAt time.Time
	items     []queueItem
	conc      int
	seq       int
}

// Panel 队列字段在 Panel 结构体上（panel.go）由 initQueue 惰性初始化；
// 这里集中访问器，避免改动 New 构造链。
func (p *Panel) queue() *queueState {
	p.queueOnce.Do(func() { p.q = &queueState{} })
	return p.q
}

// queueBodyLimit 执行队列启动请求体上限（表单极小；64KB 与登录体上限同量级，
// 防 oversized/恶意 body 拖内存）。
const queueBodyLimit = 1 << 16

// tasksRunQueue 启动执行队列：{concurrency:1-4, growth:bool, school:bool}。
// 先做一次扫描，把全部待办项排队（growth 按账号内 autoActions 顺序执行，
// school 逐账号跑闭环），账号内串行、账号间受并发信号量约束。
// body 带 io.LimitReader 上限且解码错误显式返回 400：坏 body 静默吞掉会让
// 结构体保持零值、走默认分支（growth+school 全账号任务）——意外触发全账号
// 扫描比拒绝一次请求严重得多。
func (p *Panel) tasksRunQueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Concurrency int  `json:"concurrency"`
		Growth      bool `json:"growth"`
		School      bool `json:"school"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, queueBodyLimit)).Decode(&body); err != nil && r.ContentLength != 0 {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if !body.Growth && !body.School {
		body.Growth, body.School = true, true
	}
	if body.Concurrency < 1 {
		body.Concurrency = 1
	}
	if body.Concurrency > 4 {
		body.Concurrency = 4
	}
	total, seq, started, err := p.startTaskQueue(body.Concurrency, body.Growth, body.School)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if !started {
		log.Printf("panel: 队列启动：无可执行待办（全部账号任务已完成）")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": false, "message": "全部账号没有待办任务"})
		return
	}
	log.Printf("panel: 队列启动：%d 项（并发 %d，成长 %v 开学季 %v）", total, body.Concurrency, body.Growth, body.School)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true, "total": total, "seq": seq})
}

// tasksCancelQueue 手动取消执行队列：置 cancelReq 标记，runQueueItems 的执行
// 循环据此把剩余 pending 待办标为 cancelled。幂等——重复取消同样返回成功；
// 队列未在跑返回 409。进行中的条目不打断（自然完成当前动作后停止）。
func (p *Panel) tasksCancelQueue(w http.ResponseWriter, r *http.Request) {
	q := p.queue()
	q.mu.Lock()
	if !q.running {
		q.mu.Unlock()
		writeErr(w, http.StatusConflict, "队列未在执行")
		return
	}
	q.cancelReq = true
	q.mu.Unlock()
	log.Printf("panel: 队列取消请求：剩余待办停止调度")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cancelled": true})
}

// startTaskQueue 队列启动的公共实现（HTTP 入口与定时排程共用）：
// 原子占位 running → 并发扫描全账号待办 → 无待办回滚占位返回 started=false
// → 组队并异步执行。队列已在跑返回错误（HTTP 层 409；定时层记日志跳过，
// 下个时点再试）。
//
// 占位必须在扫描之前：此前「检查 running」与「置位 running」之间隔着一次
// 秒级全账号扫描，手动触发与定时排程撞车会跑出双队列，且旧队列的
// queueMarkAt 按索引写新队列的 items 切片，执行状态互相污染。
func (p *Panel) startTaskQueue(concurrency int, wantGrowth, wantSchool bool) (total, seq int, started bool, err error) {
	q := p.queue()
	// 检查与置位同一临界区完成（原子占位）：扫描期间后续的启动请求一律 409。
	q.mu.Lock()
	if q.running {
		q.mu.Unlock()
		return 0, 0, false, fmt.Errorf("队列正在执行中（可在任务中心查看进度）")
	}
	q.running = true
	q.mu.Unlock()
	// 扫描失败 / 无待办 / 组队为空时回滚占位；顺带清掉占位期间收到的取消
	// 请求——队列没真正开跑，取消意图不能残留到下一轮。
	defer func() {
		if !started {
			q.mu.Lock()
			q.running = false
			q.cancelReq = false
			q.mu.Unlock()
		}
	}()

	// 扫描待办（复用扫描逻辑的拉取部分）。
	states := p.cfg.Pool.List()
	type acct struct {
		a      *auth.Auth
		grow   []upstream.Task
		school bool
	}
	var accts []queueAccount
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, st := range states {
		if st.Disabled {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(a *auth.Auth, wantSchool bool) {
			defer wg.Done()
			one := queueAccount{a: a}
			// D4 门控：global 账号无 CN 成长/开学季任务体系，不发起任何上游调用。
			if a.IsGlobal() {
				return
			}
			if wantGrowth {
				if tasks, err := p.cfg.Upstream.ListTasks(a); err == nil {
					for _, t := range tasks {
						if growthPending(t) {
							one.grow = append(one.grow, t)
						}
					}
					// 合并小程序口径待办（与 tasksScanAll 同口径：mp 列表是默认口径
					// 超集，按 code 去重；失败静默）。此前此处漏合并——扫描显示
					// mp 待办而队列报"无可执行待办"。
					if mpTasks, mpErr := p.cfg.Upstream.ListTasksMP(a); mpErr == nil {
						seen := map[string]bool{}
						for _, t := range one.grow {
							seen[t.TaskCode] = true
						}
						for _, t := range mpTasks {
							if growthPending(t) && !seen[t.TaskCode] {
								one.grow = append(one.grow, t)
							}
						}
					}
					sort.Slice(one.grow, func(i, j int) bool { // 按 autoActions 顺序（依赖前置）
						return autoActionIndex(one.grow[i].TaskCode) < autoActionIndex(one.grow[j].TaskCode)
					})
				}
			}
			if wantSchool && p.cfg.Scheduler != nil {
				if stasks, _, err := p.cfg.Upstream.SchoolTasks(a); err == nil {
					for _, t := range stasks {
						if schoolPending(t) { // 认证等不可做任务已在口径外
							one.school = true
							break
						}
					}
				}
			}
			if len(one.grow) > 0 || one.school {
				mu.Lock()
				accts = append(accts, one)
				mu.Unlock()
			}
		}(a, wantSchool)
	}
	wg.Wait()

	// 组装队列（账号分组，保持顺序）。
	var items []queueItem
	for _, one := range accts {
		for _, t := range one.grow {
			items = append(items, queueItem{UID: one.a.UID, Nickname: one.a.Nickname, Kind: "growth", Code: t.TaskCode, Status: "pending"})
		}
		if one.school {
			items = append(items, queueItem{UID: one.a.UID, Nickname: one.a.Nickname, Kind: "school", Code: "school_daily", Status: "pending"})
		}
	}
	if len(items) == 0 {
		return 0, q.seq, false, nil // started=false：defer 回滚占位
	}

	// 组队成功：补齐队列元数据并异步执行（running 已在扫描前占位）。
	q.mu.Lock()
	q.startedAt = time.Now()
	q.items = items
	q.conc = concurrency
	q.seq++
	seq = q.seq
	q.mu.Unlock()

	go p.runQueueItems(accts, items, concurrency)
	return len(items), seq, true, nil
}

// runQueueItems 队列执行主体：按账号分组，账号内串行（per-account 锁），
// 账号间并发（信号量）。每项结果写回队列状态。
func (p *Panel) runQueueItems(accts []queueAccount, items []queueItem, concurrency int) {
	q := p.queue()
	defer func() {
		q.mu.Lock()
		q.running = false
		q.cancelReq = false
		q.mu.Unlock()
		log.Printf("panel: 队列执行结束（共 %d 项）", len(items))
	}()

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, one := range accts {
		wg.Add(1)
		go func(one queueAccount) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// per-account 互斥：与单任务/一键完成共用一把锁。
			if !p.tryLockAccount(one.a.UID) {
				p.queueSet(q, one.a.UID, func(it *queueItem) {
					it.Status, it.Message = "skipped", "该账号有其它任务动作在执行，跳过"
				})
				return
			}
			defer p.unlockAccount(one.a.UID)
			// 前置：批量接受尚未接受的任务。上游对 not_accepted 的任务不计数——
			// 面板「一键完成」一直有这步，队列路径此前漏了（表现为上报 200 但进度
			// 一直 not_accepted、无法领奖）。失败不阻塞（行为事件才是进度判据）。
			if accepted := p.acceptPendingTasks(one.a); accepted > 0 {
				time.Sleep(reportGap) // 给上游状态流转留时间
			}
			for i := range q.items {
				// 取消检查（每个 item 处理前，含首个——覆盖拿到账号锁后的开始时机）：
				// 置位则该账号剩余 pending 全部标 cancelled 并停止调度；已进入
				// runGrowthQueued/runSchoolQueued 的条目不打断，自然完成后停。
				if q.cancelRequested() {
					if n := p.cancelRemainingFor(q, one.a.UID); n > 0 {
						log.Printf("panel: 队列已取消：剩余待办标记为 cancelled")
					}
					break
				}
				uid, kind, code := q.snapshotAt(i)
				if uid != one.a.UID {
					continue
				}
				p.queueMarkAt(i, "running", "")
				var msg string
				var err error
				switch kind {
				case "growth":
					msg, err = p.runGrowthQueued(one.a, code)
				case "school":
					msg, err = p.runSchoolQueued(one.a)
				}
				if err != nil {
					p.queueMarkAt(i, "error", err.Error())
				} else {
					p.queueMarkAt(i, "done", msg)
				}
				time.Sleep(reportGap) // 项间节流
			}
		}(one)
	}
	wg.Wait()
}

// queueAccount 队列执行的账号单元（runQueueItems 参数）。
type queueAccount struct {
	a      *auth.Auth
	grow   []upstream.Task
	school bool
}

// ScheduledQueueRunner 定时排程入口（scheduler 第七类任务 taskQueue 到点回调，
// main 装配期经 Scheduler.SetQueueRunner 注入）：扫描全账号待办并启动执行队列。
// 成长任务 + 开学季闭环都做，并发固定 2（定时无人值守，宁稳勿激）。
// 队列已在跑（手动点过还没跑完）记日志跳过，等下个时点；无待办同样静默完成。
func (p *Panel) ScheduledQueueRunner() {
	pending, err := p.pendingCount()
	if err != nil {
		log.Printf("panel: 定时队列扫描失败: %v", err)
		return
	}
	if pending == 0 {
		log.Printf("panel: 定时队列：全部账号无待办任务")
		return
	}
	total, _, started, err := p.startTaskQueue(2, true, true)
	if err != nil {
		log.Printf("panel: 定时队列跳过: %v", err)
		return
	}
	if !started {
		log.Printf("panel: 定时队列：扫描到 %d 项待办但组队为空，跳过", pending)
		return
	}
	log.Printf("panel: 定时队列已启动：%d 项（并发 2）", total)
}

// pendingCount 只读扫描全账号待办总数（成长 + 开学季，与 startTaskQueue 同口径）。
// 供定时入口决定"无待办不启队列"；扫描失败返回错误（宁可下个时点再试，不误启）。
func (p *Panel) pendingCount() (int, error) {
	states := p.cfg.Pool.List()
	var wg sync.WaitGroup
	var mu sync.Mutex
	pending := 0
	scanFailed := false
	for _, st := range states {
		if st.Disabled {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(a *auth.Auth) {
			defer wg.Done()
			if a.IsGlobal() {
				return // D4 门控：global 无 CN 任务体系
			}
			n := 0
			failed := false
			// 首查结果记下 code 集合：mp 分支去重直接复用，不再对每账号重复
			// ListTasks（此前第二次调用只为构建去重集）；首查失败短路跳过
			// mp 去重（默认口径已拿不到，mp 增量的去重基准缺失，宁少不多计）。
			seen := map[string]bool{}
			haveDefault := false
			if tasks, err := p.cfg.Upstream.ListTasks(a); err == nil {
				for _, t := range tasks {
					if growthPending(t) {
						n++
					}
					seen[t.TaskCode] = true
				}
				haveDefault = true
			} else {
				failed = true
			}
			if haveDefault {
				if mpTasks, err := p.cfg.Upstream.ListTasksMP(a); err == nil {
					// mp 列表是默认口径的超集增量：只统计默认列表没有的 code。
					for _, t := range mpTasks {
						if growthPending(t) && !seen[t.TaskCode] {
							n++
						}
					}
				}
			}
			if stasks, _, err := p.cfg.Upstream.SchoolTasks(a); err == nil {
				for _, t := range stasks {
					if schoolPending(t) {
						n++
					}
				}
			}
			mu.Lock()
			if failed {
				scanFailed = true
			}
			pending += n
			mu.Unlock()
		}(a)
	}
	wg.Wait()
	if scanFailed {
		return pending, fmt.Errorf("部分账号任务列表查询失败")
	}
	return pending, nil
}

// cancelRequested 锁内读取消标记（执行循环每个 item 处理前检查）。
func (q *queueState) cancelRequested() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.cancelReq
}

// cancelRemainingFor 把该账号所有仍是 pending 的条目标为 cancelled，
// 返回标记条数（0 = 无需标记；已 running/done 的条目不动）。
func (p *Panel) cancelRemainingFor(q *queueState, uid string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for i := range q.items {
		if q.items[i].UID == uid && q.items[i].Status == "pending" {
			q.items[i].Status, q.items[i].Message = "cancelled", "已取消"
			n++
		}
	}
	return n
}

// snapshotAt 锁内读条目三元组（避免锁外持有指针）。
func (q *queueState) snapshotAt(i int) (uid, kind, code string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.items[i].UID, q.items[i].Kind, q.items[i].Code
}

// queueMarkAt 按索引更新队列条目状态（条目数组固定不再增删）。
func (p *Panel) queueMarkAt(i int, status, msg string) {
	q := p.queue()
	q.mu.Lock()
	q.items[i].Status, q.items[i].Message = status, msg
	q.mu.Unlock()
}

// queueSet 按 uid 批量改状态。
func (p *Panel) queueSet(q *queueState, uid string, fn func(*queueItem)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].UID == uid {
			fn(&q.items[i])
		}
	}
}

// acceptPendingTasks 批量接受该账号未接受的任务，返回接受的个数（失败返回 0 不阻塞）。
func (p *Panel) acceptPendingTasks(a *auth.Auth) int {
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		return 0
	}
	var codes []string
	for _, t := range tasks {
		if !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
			codes = append(codes, t.TaskCode)
		}
	}
	if len(codes) == 0 {
		return 0
	}
	if err := p.cfg.Upstream.AcceptTasks(a, codes); err != nil {
		log.Printf("panel: 队列 accept uid=%s: %v（不阻塞）", a.UID, err)
		return 0
	}
	log.Printf("panel: 队列 accept uid=%s: 已接受 %d 个任务", a.UID, len(codes))
	return len(codes)
}

// runGrowthQueued 执行单个成长任务（动作 + 回读 + 自动领奖；与
// accountTaskAuto 同语义，结果以文字返回）。
func (p *Panel) runGrowthQueued(a *auth.Auth, code string) (string, error) {
	act := autoActionFor(code)
	if act == nil {
		return "", fmt.Errorf("任务 %s 无自动动作", code)
	}
	// taskByCode 已双口径（mp 专属码自动回落 mp 列表）。
	before, err := p.taskByCode(a, code)
	if err != nil {
		return "", err
	}
	if before == nil {
		return "该账号无此任务", nil
	}
	isMP := isMPTaskCode(code)
	if before.Claimed {
		return "已完成（已领取）", nil
	}
	msg, err := act.run(p, a)
	if err != nil {
		return "", err
	}
	var after *upstream.Task
	if isMP {
		after, _ = p.taskByCodeMP(a, code)
	} else {
		after, _ = p.taskByCodeWaiting(a, code)
	}
	if after != nil && after.Claimable {
		var credit, energy int64
		var cerr error
		if isMP {
			credit, energy, cerr = p.cfg.Upstream.ClaimRewardMP(a, code)
		} else {
			credit, energy, cerr = p.cfg.Upstream.ClaimReward(a, code)
		}
		if cerr == nil && (credit > 0 || energy > 0) {
			msg += fmt.Sprintf("；自动领奖 +%d 分 +%d 能", credit, energy)
		}
	}
	if after != nil {
		msg += "（进度 " + taskProgressText(after) + "）"
	}
	log.Printf("panel: 队列 growth uid=%s code=%s: %s", a.UID, code, msg)
	return msg, nil
}

// runSchoolQueued 单账号开学季闭环（scheduler 四任务 + 抽奖）。
func (p *Panel) runSchoolQueued(a *auth.Auth) (string, error) {
	if p.cfg.Scheduler == nil {
		return "", fmt.Errorf("scheduler 不可用")
	}
	p.cfg.Scheduler.RunSchoolAccountNow(a)
	// 闭环后回读开学季状态做汇总。
	tasks, _, err := p.cfg.Upstream.SchoolTasks(a)
	if err != nil {
		return "闭环已执行（状态回读失败）", nil
	}
	done := 0
	for _, t := range tasks {
		if t.Status == "claimed" || (t.TaskCode != "task_student_verify" && t.Progress >= t.TargetCount && t.TargetCount > 0) {
			done++
		}
	}
	log.Printf("panel: 队列 school uid=%s: 闭环完成（%d/%d 项完成）", a.UID, done, len(tasks))
	return fmt.Sprintf("开学季闭环完成（%d/%d 项已完成，抽奖已抽完）", done, len(tasks)), nil
}

// tasksQueueStatus 队列状态（轮询用）。
func (p *Panel) tasksQueueStatus(w http.ResponseWriter, r *http.Request) {
	q := p.queue()
	q.mu.Lock()
	defer q.mu.Unlock()
	items := make([]queueItem, len(q.items))
	copy(items, q.items)
	writeJSON(w, http.StatusOK, map[string]any{
		"running":          q.running,
		"total":            len(items),
		"conc":             q.conc,
		"started":          !q.startedAt.IsZero(),
		"started_at":       q.startedAt,
		"seq":              q.seq,
		"cancel_requested": q.cancelReq,
		"items":            items,
	})
}

// ---------------------------------------------------------------------------
// 开学季独立视图
// ---------------------------------------------------------------------------

// schoolStatus 全账号开学季任务状态（含抽奖余额）。
func (p *Panel) schoolStatus(w http.ResponseWriter, r *http.Request) {
	states := p.cfg.Pool.List()
	type acctView struct {
		UID      string           `json:"uid"`
		Nickname string           `json:"nickname"`
		InPeriod bool             `json:"in_period"`
		Tasks    []schoolTaskView `json:"tasks"`
		Chances  int              `json:"chances"`
		Err      string           `json:"error,omitempty"`
	}
	out := make([]acctView, 0, len(states))
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, st := range states {
		if st.Disabled {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(a *auth.Auth) {
			defer wg.Done()
			v := acctView{UID: a.UID, Nickname: a.Nickname}
			// D4 门控：global 账号无开学季活动，不发起任何上游调用。
			if a.IsGlobal() {
				v.Err = "global realm（无开学季活动）"
				mu.Lock()
				out = append(out, v)
				mu.Unlock()
				return
			}
			tasks, inPeriod, err := p.cfg.Upstream.SchoolTasks(a)
			if err != nil {
				v.Err = err.Error()
			} else {
				v.InPeriod = inPeriod
				for _, t := range tasks {
					v.Tasks = append(v.Tasks, schoolTaskView{
						Code: t.TaskCode, Status: t.Status, Prog: t.Progress, Target: t.TargetCount,
					})
				}
			}
			v.Chances, _ = p.cfg.Upstream.SchoolChances(a)
			mu.Lock()
			out = append(out, v)
			mu.Unlock()
		}(a)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Nickname < out[j].Nickname })
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": out})
}

// schoolRunAll 一键执行全部账号开学季闭环（异步，进度看任务频道日志）。
// 按类型 TryLock 防重入：闭环逐账号向上游打全套请求，重复触发会叠加
// 全账号扫描，已在跑时返回 409 友好提示（与 panel.go 批量动作同口径）。
func (p *Panel) schoolRunAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	if !p.tryLockBatch("school") {
		writeErr(w, http.StatusConflict, "同类任务正在执行（开学季全账号闭环），请等本轮结束后再试")
		return
	}
	go func() {
		defer p.unlockBatch("school")
		p.cfg.Scheduler.RunSchoolNowAll() // panel 纯 API 口径全量闭环（school_api.go）
	}()
	log.Printf("panel: 开学季全账号闭环已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// schoolVouchers 我的券码：逐 CN 账号查开学季 /vouchers（3 并发，与 packages
// 同款限流），失败只在对应账号标 error。global 账号无开学季，不发上游调用。
func (p *Panel) schoolVouchers(w http.ResponseWriter, r *http.Request) {
	accts := p.cfg.Pool.List()
	type row struct {
		UID      string                   `json:"uid"`
		Nickname string                   `json:"nickname"`
		Vouchers []upstream.SchoolVoucher `json:"vouchers"`
		Err      string                   `json:"error,omitempty"`
	}
	out := make([]row, len(accts))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, st := range accts {
		if st.Disabled {
			continue // 未占位，行末统一压掉
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			it := row{UID: a.UID, Nickname: a.Nickname}
			switch {
			case a.IsGlobal():
				it.Err = "global realm（无开学季活动）"
			default:
				vs, err := p.cfg.Upstream.SchoolVouchers(a)
				if err != nil {
					it.Err = err.Error()
				} else {
					it.Vouchers = vs
				}
			}
			out[i] = it
		}(i, a)
	}
	wg.Wait()
	res := make([]row, 0, len(out))
	for _, it := range out {
		if it.UID != "" {
			res = append(res, it)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": res})
}
