package panel

// autotask_abort_test.go mp 真人节奏补报的可取消等待（OCR 审查 medium 修复回归）：
// 45s/条 × N 条的等待在取消广播后必须能提前退出（否则队列取消后账号锁仍被占
// 数分钟），need 钳制防止上游 target 异常把占锁放大到小时级。

import (
	"testing"
	"time"
)

// TestSleepAbortableNilChannelZeroOverhead abort 为 nil（无队列的手动路径）时
// 退化为纯睡眠，不 panic、正常睡满返回 false。
func TestSleepAbortableNilChannelZeroOverhead(t *testing.T) {
	start := time.Now()
	if interrupted := sleepAbortable(20*time.Millisecond, nil); interrupted {
		t.Fatal("nil abort 不应报中断")
	}
	if e := time.Since(start); e < 15*time.Millisecond {
		t.Errorf("应实际睡满 20ms，实际 %v", e)
	}
}

// TestSleepAbortableInterruptedMidWait 等待中途关闭 abort：立即返回 true，
// 不等睡满（取消响应时间 << 睡眠时长）。
func TestSleepAbortableInterruptedMidWait(t *testing.T) {
	abort := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(abort)
	}()
	start := time.Now()
	if interrupted := sleepAbortable(10*time.Second, abort); !interrupted {
		t.Fatal("abort 关闭后应报中断")
	}
	if e := time.Since(start); e > time.Second {
		t.Errorf("中断应立即返回，实际耗时 %v（远小于 10s 睡眠）", e)
	}
}

// TestSleepAbortableFullSleepNoAbort abort 未关闭：睡满返回 false（正常路径）。
func TestSleepAbortableFullSleepNoAbort(t *testing.T) {
	abort := make(chan struct{})
	if interrupted := sleepAbortable(20*time.Millisecond, abort); interrupted {
		t.Fatal("未关闭 abort 不应报中断")
	}
}

// TestAbortBroadcastIdempotent 中断广播幂等：重复 close 不 panic；
// resetAbort 后旧信号不复用（新一轮队列不被上一轮取消误杀）。
func TestAbortBroadcastIdempotent(t *testing.T) {
	p := &Panel{}

	// 未 reset 过：abortCh 为 nil，广播应为空操作（不 panic）。
	p.abortRunningTasks()

	// reset → 广播 → 再次广播（幂等）→ reset（拿到新通道）。
	p.resetAbort()
	ch1 := p.taskAbortCh()
	if ch1 == nil {
		t.Fatal("resetAbort 后应有通道")
	}
	p.abortRunningTasks()
	select {
	case <-ch1:
	default:
		t.Fatal("广播后通道应已关闭")
	}
	p.abortRunningTasks() // 幂等：二次 close nil 通道应无效果

	p.resetAbort()
	ch2 := p.taskAbortCh()
	if ch2 == nil {
		t.Fatal("二次 resetAbort 后应有新通道")
	}
	select {
	case <-ch2:
		t.Fatal("新通道不应继承旧的中断信号")
	default:
	}
}

// TestMaxMPChatEventsClamp need 钳制上限存在且覆盖 Sequential 链最大 target=10
// （留余量到 12）：上游 target 异常放大时单轮补报不超过 12 条（≈9 分钟占锁上界）。
func TestMaxMPChatEventsClamp(t *testing.T) {
	if maxMPChatEvents < 10 {
		t.Errorf("maxMPChatEvents=%d 不应低于 Sequential 链最大 target（10）", maxMPChatEvents)
	}
	if maxMPChatEvents > 20 {
		t.Errorf("maxMPChatEvents=%d 过大，失去占锁上界意义", maxMPChatEvents)
	}
}
