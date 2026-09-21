// queue_task.go 对应的单测：第七类任务（任务中心执行队列排程）的开关/排程/回调语义。
package scheduler

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestQueueTaskOptInDefaultOff queue 缺省禁用（opt-in）：快照无排程时点；
// 显式 QueueEnabled=true 后到点进入候选。
func TestQueueTaskOptInDefaultOff(t *testing.T) {
	s := newTestSched(t, Config{})
	var called atomic.Int32
	s.SetQueueRunner(func() { called.Add(1) })

	if _, kinds := s.nextWake(nowForTest()); containsKind(kinds, taskQueue) {
		t.Fatalf("缺省 Config 下 queue 不应进入唤醒候选，kinds=%v", kinds)
	}
	if snap := s.SnapshotAll()[taskQueue]; snap.Enabled || snap.NextFire != "" {
		t.Fatalf("queue 缺省快照应禁用：%+v", snap)
	}

	// 热开关打开：SetEnabled 与 New(Config{QueueEnabled:true}) 等效。
	if !s.SetEnabled("queue", true) {
		t.Fatal("SetEnabled(queue) 应合法")
	}
	snap := s.SnapshotAll()[taskQueue]
	if !snap.Enabled || snap.NextFire == "" || len(snap.Hours) != 1 || snap.Hours[0] != 10 {
		t.Fatalf("启用后 queue 快照错误：%+v", snap)
	}

	// 手动触发走 RunKindNow：回调被调（禁用中的任务手动执行同语义）。
	s.enabled[taskQueue].Store(false)
	if err := s.RunKindNow("queue"); err != nil {
		t.Fatalf("RunKindNow(queue): %v", err)
	}
	if called.Load() != 1 {
		t.Fatalf("queue 回调被调 %d 次，want 1", called.Load())
	}
	if got := s.lastOut[taskQueue].Load().(string); !strings.Contains(got, "done") {
		t.Fatalf("lastOut=%q 应含 done", got)
	}
}

// TestQueueTaskNilRunnerSkips 回调未注入（panel 未装配）：跳过不 panic。
func TestQueueTaskNilRunnerSkips(t *testing.T) {
	s := newTestSched(t, Config{QueueEnabled: true, QueueHours: []int{1}})
	if err := s.RunKindNow("queue"); err != nil {
		t.Fatalf("RunKindNow(queue): %v", err)
	}
	if got := s.lastOut[taskQueue].Load().(string); !strings.Contains(got, "skipped") {
		t.Fatalf("nil 回调应记 skipped：%q", got)
	}
}

// TestQueueTaskFiresOnSchedule 启用 + 回调注入：到点 dispatch 执行回调。
// 基准 19:30 + 队列 20 点：queue 是全部七类中最近的触发点（其余默认小时都在
// 21/22 点或次日），排除与其他任务并列候选的巧合。
func TestQueueTaskFiresOnSchedule(t *testing.T) {
	s := newTestSched(t, Config{QueueEnabled: true, QueueHours: []int{20}})
	var called atomic.Int32
	s.SetQueueRunner(func() { called.Add(1) })

	at, kinds := s.nextWake(nowForTest())
	if !containsKind(kinds, taskQueue) {
		t.Fatalf("启用后到点应含 queue，kinds=%v", kinds)
	}
	if len(kinds) != 1 {
		t.Fatalf("20 点应只有 queue 一个候选，kinds=%v", kinds)
	}
	if got := at.Format("15:04"); got != "20:00" {
		t.Fatalf("nextWake=%s want 20:00", got)
	}
	s.RunKindNow("queue")
	if called.Load() != 1 {
		t.Fatalf("回调被调 %d 次，want 1", called.Load())
	}
}

// TestSetQueueRunnerIdempotent 重复注入以最后一次为准；nil 清空后回到 skipped 语义。
func TestSetQueueRunnerIdempotent(t *testing.T) {
	s := newTestSched(t, Config{QueueEnabled: true})
	first, second := 0, 0
	s.SetQueueRunner(func() { first++ })
	s.SetQueueRunner(func() { second++ })
	s.RunKindNow("queue")
	if first != 0 || second != 1 {
		t.Fatalf("重复注入应只执行最后一次（first=%d second=%d）", first, second)
	}
	s.SetQueueRunner(nil)
	s.RunKindNow("queue")
	if second != 1 {
		t.Fatalf("nil 注入后不应再执行（second=%d）", second)
	}
}

func containsKind(kinds []taskKind, want taskKind) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}

// nowForTest 固定测试基准时刻：非整点，避免边界抖动。
func nowForTest() time.Time { return time.Date(2026, 9, 21, 19, 30, 0, 0, time.Local) }
