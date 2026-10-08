// credit.go 积分变动流水（wbm services/credits.py record_balance 吸收件）。
//
// ## 背景：为什么必须「比对快照」而不是记上游日志
//
// 上游只在猫猫旅行领奖时打积分日志，签到/活跃上报/连登兑换等获取渠道根本
// 不打——按日志记账会把这些真实到账全部漏掉。改为：**每次余额刷新后与上次
// 快照比对**，余额增加就记一条流水（如「余额 +100（1300 → 1400）」），进
// 任务中心的记录流（panel creditStore，kind=credit）。
//
// ## 三条防误报口径（与 wbm record_balance 一致）
//
//   - **去重**：用「变动后的余额值」做 dedup key（credit|uid|value）。同一余额
//     重复刷新（面板手动 + 定时签到撞同一时点）只会留下一条流水；
//   - **防异常跳变**：单条增量超过 creditMaxDelta（10000）只记「余额跳变 A → B」
//     的 jump 记录，不带加号语义——上游兑换/补发/对账修正可能一次性到账大额
//     积分，把它记成「获得」会污染增量统计；
//   - **首次见号只建基线**：快照里没有该 uid 时只落快照、不记流水，否则历史
//     余额会被误报成「刚获得」。
//
// 减少与持平不记（消耗侧已由 NoteModelCost 的逐请求扣减覆盖，流水只记「赚到」）。
//
// 快照落盘 data/credit_snapshots.json（state_file 同目录推导，由 main 装配注入），
// 独立于 pool 的 state.json——积分快照是纯观测数据，混进池状态会让它的 5s 高频
// flush 背上不必要的写放大；独立文件 + 防抖原子写（usage.Recorder 同风格）。
package scheduler

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"

	"workbuddy2api/internal/logfmt"
)

// creditRecord 单条积分流水（任务中心记录流 kind=credit 的记录体）。
// 与 wbm task_logs 字段一一对应；delta 为 0 的 jump 记录不带加号语义。
// 导出名（CreditRecord）：panel.CreditTracker 跨包引用同一结构——两包共用
// 单一表示，避免序列化格式漂移。
type CreditRecord struct {
	// Ts 记录时刻（Unix 秒）。
	Ts int64 `json:"ts"`
	// UID 账号 uid；摘要类记录（未来扩展）可能为空。
	UID string `json:"uid,omitempty"`
	// Kind 恒 "credit"（任务中心记录流的类型标识，供前端筛选）。
	Kind string `json:"kind"`
	// Delta 积分增量（>0）；jump 记录为 0。
	Delta int64 `json:"delta"`
	// Prev / New 变动前后余额快照值。
	Prev int64 `json:"prev"`
	New  int64 `json:"new"`
	// Jump 异常跳变标记：true 时 Delta 恒 0、不带加号语义（「余额跳变 A → B」）。
	Jump bool `json:"jump,omitempty"`
	// Message 可读文案（「余额 +100（1300 → 1400） · 昵称」）。
	Message string `json:"message"`
	// DedupKey 去重键：credit|uid|变动后余额。同一余额重复刷新不重复记。
	DedupKey string `json:"dedup_key"`
}

// CreditMaxDelta 单条流水增量的上限：超过只记 jump（异常跳变），不带加号语义。
// 取 10000：正常单渠道到账（签到 20、成长任务 100-500、旅行 ≤500、连登 ≤150）
// 远低于该值；一次性到账超万是兑换补发/对账修正级别的异常事件。
const CreditMaxDelta = 10000

// creditSnapshotFile 快照文件名（state_file 同目录，main 装配期推导注入）。
const creditSnapshotFile = "credit_snapshots.json"

// creditSnapshotter 积分快照落盘 + 记录回调的最小接口。由 panel 侧实现
// （*panel.CreditTracker，main 装配期经 Scheduler.SetCreditSink 注入）——
// internal/scheduler 不能 import internal/panel（依赖方向相反），与
// queueRunner 的回调注入同一通路。nil 时流水记录整体关闭（零开销路径）。
type creditSnapshotter interface {
	// RecordBalance 比对快照并按需产出流水：新增条数返回（0 = 无流水）。
	RecordBalance(uid, nickname string, credits int64, at time.Time) int
}

// SetCreditSink 注入积分流水的产出与落盘执行体（main 装配期调用一次）。nil 清空。
// 幂等：重复注入以最后一次为准。并发读写函数值经 creditMu 同步（与 queueMu 同风格）。
func (s *Scheduler) SetCreditSink(fn creditSnapshotter) {
	s.creditMu.Lock()
	s.creditSink = fn
	s.creditMu.Unlock()
}

// creditSinkOf 锁内读当前 sink（nil = 未注入，流水关闭）。
func (s *Scheduler) creditSinkOf() creditSnapshotter {
	s.creditMu.Lock()
	defer s.creditMu.Unlock()
	return s.creditSink
}

// RecordBalanceChecked 余额刷新路径的统一记账钩子：余额查询成功（remain 已写回
// 池）后调用，与上次快照比对产出积分流水。sink 未注入时零开销直返。
// 所有余额刷新路径共用：签到（CheckinAll）、面板单号（accountBalance）、
// 面板全量（RunBalanceRefreshNow）——漏接一条路径，该渠道的到账就整体失明。
func (s *Scheduler) RecordBalanceChecked(uid, nickname string, credits int64) {
	sink := s.creditSinkOf()
	if sink == nil {
		return
	}
	if n := sink.RecordBalance(uid, nickname, credits, time.Now()); n > 0 {
		log.Printf("credit %s: 余额变动流水 +%d 条", logfmt.Label(uid, nickname), n)
	}
}

// ---------------------------------------------------------------------------
// 快照文件读写（panel.CreditTracker 复用的存储原语；导出供同仓库跨包使用）
// ---------------------------------------------------------------------------

// CreditSnapshots 快照文件结构：uid → 上次余额。导出：panel.CreditTracker 的
// 内存快照与落盘原语（Load/SaveCreditSnapshots）跨包复用同一格式。
type CreditSnapshots map[string]int64

// LoadCreditSnapshots 读快照文件；缺失/损坏返回空表（零状态起步，重新建基线）。
func LoadCreditSnapshots(fp string) CreditSnapshots {
	raw, err := os.ReadFile(fp)
	if err != nil {
		return CreditSnapshots{}
	}
	var out CreditSnapshots
	if json.Unmarshal(raw, &out) != nil {
		return CreditSnapshots{}
	}
	if out == nil {
		out = CreditSnapshots{}
	}
	return out
}

// SaveCreditSnapshots 原子落盘快照（tmp + rename，与 pool.writeStateFileSync 同口径）。
func SaveCreditSnapshots(fp string, snap CreditSnapshots) error {
	if dir := filepath.Dir(fp); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
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
