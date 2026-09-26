package panel

import (
	"testing"

	"workbuddy2api/internal/upstream"
)

// growthPending 过滤判据测试（taskcenter.go 扫描/建队列共用本函数，
// 6 处调用点——扫描视图、run_queue 内联扫描、pendingCount——同口径生效）。

// TestGrowthPendingSkipsLocked Sequential 族每日零点解锁一环：做完一环后下一环以
// locked=true 形态出现在 mp 列表。锁定环入队后 accept 必然不落账（上游拒绝），
// 表现为「做完再扫还冒出来」。吸收 panel 修复：locked 任务不入待办，
// 零点解锁后（locked=false）自动回到待办视图。
func TestGrowthPendingSkipsLocked(t *testing.T) {
	if autoActionFor("Sequential_Tasks_2") == nil {
		// autoActionFor 返回 *autoAction（nil = 不可自动化），Sequential_Tasks_2 必须已登记。
		t.Fatal("precondition: Sequential_Tasks_2 应已登记自动化动作")
	}
	if growthPending(upstream.Task{TaskCode: "Sequential_Tasks_2", Locked: true}) {
		t.Error("locked 任务不得入待办（accept 不落账，白跑一轮队列）")
	}
	if !growthPending(upstream.Task{TaskCode: "Sequential_Tasks_2"}) {
		t.Error("同任务解锁后（locked=false）应入待办")
	}
}

// TestGrowthPendingClaimedAndDone 既有语义回归：已领取 / 已达标未领 / 未登记动作
// 三类的判定不因 Locked 过滤的加入而改变。
func TestGrowthPendingClaimedAndDone(t *testing.T) {
	if growthPending(upstream.Task{TaskCode: "Sequential_Tasks_2", Claimed: true}) {
		t.Error("已领取任务不得入待办")
	}
	if growthPending(upstream.Task{TaskCode: "Sequential_Tasks_2", Target: 5, Current: 5}) {
		// 达标未领仍入队（队列执行后自动领奖），Locked 过滤不得改变这一分支。
		t.Error("达标未领任务应入待办（队列会自动领奖）")
	}
	if growthPending(upstream.Task{TaskCode: "no_such_task"}) {
		t.Error("未登记自动化动作的任务不得入待办")
	}
}
