// credit_record.go 面板侧的积分流水存储与产出（任务中心记录流 kind=credit）。
//
// 与 scheduler/credit.go 的分工：scheduler 侧定义流水结构与「什么时候记账」
// （RecordBalanceChecked 钩子，接线在签到/单号余额/全量余额三条路径）；本文件
// 实现「怎么记账」——快照比对（去重/跳变/基线三条防误报口径）、记录留存、
// HTTP 暴露。scheduler 经 SetCreditSink 接口注入本实现（依赖方向 panel→scheduler
// 不能反，与 queueRunner 同一通路）。
//
// 存储：data/credit_snapshots.json（余额快照，scheduler.LoadCreditSnapshots 原语）
// + data/credit_records.json（流水，本地留存）。记录流是纯观测数据，不进 pool
// 的 state.json（池状态 5s 高频 flush，混入会把观测写放大绑上热路径）；独立文件
// + 30s 防抖原子写（usage.Recorder 同风格），Stop/SaveNow 保证停机不丢最后一笔。
//
// 记录容量：creditRecordMax 条环形截断（最新在前），防长期运行无限膨胀。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"workbuddy2api/internal/scheduler"
)

// creditRecordMax 流水留存上限（环形截断，最旧的丢弃）。签到 + 面板手动刷新的
// 产出频率下，2000 条覆盖数月的历史；超过只影响翻旧账，不影响余额快照的正确性。
const creditRecordMax = 2000

// creditDebounce 流水落盘防抖间隔（usage flushInterval 同量级：批量到账逐条
// 置脏，30s 合并成一次原子写）。
const creditDebounce = 30 * time.Second

// CreditTracker 积分流水追踪器：快照比对 + 记录留存 + 落盘。
// 实现 scheduler.creditSnapshotter（RecordBalance），经 Scheduler.SetCreditSink 注入。
type CreditTracker struct {
	mu       sync.Mutex
	snapFp   string // 余额快照文件（credit_snapshots.json）
	recFp    string // 流水记录文件（credit_records.json）
	snap     scheduler.CreditSnapshots
	records  []scheduler.CreditRecord // 最新在前
	dirty    bool
	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewCreditTracker 构建并启动后台防抖落盘 goroutine（Stop 收尾）。snapFp/recFp
// 为空（纯内存测试形态）时不落盘也不起 goroutine，记录只留内存。
func NewCreditTracker(snapFp, recFp string) *CreditTracker {
	t := &CreditTracker{
		snapFp: snapFp,
		recFp:  recFp,
		snap:   scheduler.LoadCreditSnapshots(snapFp),
	}
	t.loadRecords()
	if snapFp != "" || recFp != "" {
		t.stopCh = make(chan struct{})
		go t.loop()
	}
	return t
}

// Stop 停止后台落盘并做最后一次落盘（幂等）。
func (t *CreditTracker) Stop() {
	t.stopOnce.Do(func() {
		if t.stopCh != nil {
			close(t.stopCh)
		}
	})
	t.SaveNow()
}

// loop 后台防抖落盘（usage.Recorder 同风格：dirty 标志 + 周期检查）。
func (t *CreditTracker) loop() {
	ticker := time.NewTicker(creditDebounce)
	defer ticker.Stop()
	for {
		select {
		case <-t.stopCh:
			return
		case <-ticker.C:
			t.saveIfDirty()
		}
	}
}

// saveIfDirty 落盘有变更的快照与记录（失败只记日志，下轮重试——观测数据
// 不值得为它中断服务，与 usage 落盘同口径）。
func (t *CreditTracker) saveIfDirty() {
	t.mu.Lock()
	if !t.dirty {
		t.mu.Unlock()
		return
	}
	t.dirty = false
	snap, records := t.snap, t.records
	t.mu.Unlock()
	t.persist(snap, records)
}

// persist 实际写盘（锁外）：快照与记录两个文件独立原子写。
func (t *CreditTracker) persist(snap scheduler.CreditSnapshots, records []scheduler.CreditRecord) {
	if err := scheduler.SaveCreditSnapshots(t.snapFp, snap); err != nil {
		log.Printf("WARN: credit_snapshots 落盘失败: %v", err)
	}
	if raw, err := json.MarshalIndent(records, "", "  "); err == nil {
		if err := writeFilePanelAtomic(t.recFp, raw); err != nil {
			log.Printf("WARN: credit_records 落盘失败: %v", err)
		}
	}
}

// SaveNow 同步落盘（Stop 收尾 / 面板手动触发用）。
func (t *CreditTracker) SaveNow() {
	t.mu.Lock()
	snap, records := t.snap, t.records
	t.mu.Unlock()
	t.persist(snap, records)
}

// loadRecords 读流水记录文件；缺失/损坏静默（零记录起步，快照才是记账的事实源）。
func (t *CreditTracker) loadRecords() {
	if t.recFp == "" {
		return
	}
	raw, err := os.ReadFile(t.recFp)
	if err != nil {
		return
	}
	var out []scheduler.CreditRecord
	if json.Unmarshal(raw, &out) != nil {
		return
	}
	t.records = out
}

// RecordBalance 比对快照并按需产出流水（scheduler.creditSnapshotter 实现）。
// 返回新增记录条数。三条防误报口径见 scheduler/credit.go 文件头：
// 首次见号只建基线；同余额去重（dedup key=变动后余额）；超大增量记 jump。
func (t *CreditTracker) RecordBalance(uid, nickname string, credits int64, at time.Time) int {
	if uid == "" {
		return 0
	}
	t.mu.Lock()
	prev, seen := t.snap[uid]
	t.snap[uid] = credits
	// 快照变更置脏（首次建基线也要落盘——重启后不能把历史余额再误报一遍）。
	t.dirty = true
	if !seen {
		// 首次见到该账号：只建基线，不记流水。
		t.mu.Unlock()
		return 0
	}
	delta := credits - prev
	if delta <= 0 {
		// 减少/持平：不记（消耗侧由 NoteModelCost 逐请求扣减覆盖；流水只记「赚到」）。
		t.mu.Unlock()
		return 0
	}
	// 去重（键 = 变动后余额，wbm record_balance 口径）：同余额重复刷新（定时 +
	// 手动撞同一时点 / 回落到消耗前的值后再回到同一值）不重复记。判定前置——
	// 正常增量与跳变共享同一去重键空间，跳变余额重复刷新同样被这里拦下。
	if t.hasDedupLocked(creditDedupKey(uid, credits)) {
		t.mu.Unlock()
		return 0
	}
	// 异常跳变（超过 CreditMaxDelta）：记 jump（Delta=0，不带加号语义）。
	jump := delta > scheduler.CreditMaxDelta
	rec := scheduler.CreditRecord{
		Ts: at.Unix(), UID: uid, Kind: "credit",
		Prev: prev, New: credits, Jump: jump,
		Message:  creditMessage(uid, nickname, delta, prev, credits, jump),
		DedupKey: creditDedupKey(uid, credits),
	}
	if !jump {
		rec.Delta = delta
	}
	t.records = append(t.records, rec)
	// 容量截断（环形）：超上限丢弃最旧的。sort 后最新在前（records 追加序即时间序，
	// 但跨重启合并的历史记录可能乱序，统一按 ts 降序排）。
	sort.Slice(t.records, func(i, j int) bool {
		if t.records[i].Ts != t.records[j].Ts {
			return t.records[i].Ts > t.records[j].Ts
		}
		return t.records[i].DedupKey > t.records[j].DedupKey
	})
	if len(t.records) > creditRecordMax {
		t.records = t.records[:creditRecordMax]
	}
	t.mu.Unlock()
	return 1
}

// hasDedupLocked 线性扫描去重键（records ≤ 2000，比对频率为每次余额刷新一次，
// O(n) 足够；建 map 的常数反而更大）。调用方必须已持 t.mu。
func (t *CreditTracker) hasDedupLocked(key string) bool {
	for i := range t.records {
		if t.records[i].DedupKey == key {
			return true
		}
	}
	return false
}

// creditDedupKey 去重键：credit|uid|变动后余额。同一余额只会留下一条流水。
func creditDedupKey(uid string, value int64) string {
	return "credit|" + uid + "|" + itoa64(value)
}

// creditMessage 可读文案。正常增量：「余额 +100（1300 → 1400） · 昵称」；
// 跳变：「余额跳变 1300 → 9500 · 昵称」（不带加号语义）。
func creditMessage(uid, nickname string, delta, prev, now int64, jump bool) string {
	label := nickname
	if label == "" {
		label = uid
	}
	if jump {
		return "余额跳变 " + itoa64(prev) + " → " + itoa64(now) + "（增量异常，未计入收益） · " + label
	}
	return "余额 +" + itoa64(delta) + "（" + itoa64(prev) + " → " + itoa64(now) + "） · " + label
}

// itoa64 极简 int64 → string（strconv.FormatInt 直通，统一文案拼接口径）。
func itoa64(v int64) string {
	return strconv.FormatInt(v, 10)
}

// Records 返回流水快照（最新在前）。uid 非空时按账号过滤。
func (t *CreditTracker) Records(uid string) []scheduler.CreditRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]scheduler.CreditRecord, 0, len(t.records))
	for _, rec := range t.records {
		if uid == "" || rec.UID == uid {
			out = append(out, rec)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// HTTP 层：记录读取端点 + 面板手动触发的接线
// ---------------------------------------------------------------------------

// tasksRecords GET /api/tasks/records：积分流水记录（任务中心记录流 kind=credit）。
// uid 查询参数非空时按账号过滤。tracker 未装配返回 501（credit_record 记录整体
// 关闭的部署形态），前端按「未启用」展示。
func (p *Panel) tasksRecords(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Credits == nil {
		writeErr(w, http.StatusNotImplemented, "credit tracking not available")
		return
	}
	records := p.cfg.Credits.Records(r.URL.Query().Get("uid"))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"records": records,
		"total":   len(records),
	})
}

// renewAll 手动触发全账号续期巡检（异步执行；防重入口径同 checkinAll）。
// 仅对临期账号刷新（与定时巡检同一 runRenewOnce 语义），非「全量刷一遍」。
func (p *Panel) renewAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	if !p.tryLockBatch("renew") {
		writeErr(w, http.StatusConflict, "同类任务正在执行（全量续期巡检），请等本轮结束后再试")
		return
	}
	go func() {
		defer p.unlockBatch("renew")
		p.cfg.Scheduler.RunRenewNow()
	}()
	log.Printf("panel: 手动全量续期巡检已触发（仅临期账号）")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// writeFilePanelAtomic 原子写文件（tmp + rename，与 pool.writeStateFileSync /
// auth.writeFileAtomic 同口径；panel 包内已有 json 落盘惯例但无此原语，就地补一个）。
func writeFilePanelAtomic(fp string, raw []byte) error {
	if dir := filepath.Dir(fp); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := fp + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, fp)
}
