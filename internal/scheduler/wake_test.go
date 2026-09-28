package scheduler

// wake_test.go 槽位补跑（吸收 panel 97335bdf / manager 4022fffb，issue #99 同源）：
// timer 走单调时钟，睡眠期间不推进——睡到槽位的长 timer 被整体顺延，跨过的槽位
// 被绕过。修复后 timer 到期对墙钟枚举 (cursor, now] 内的全部已到点批次逐个补跑。
// 本文件锁 dueBatches 的枚举口径（Run 主循环的派发顺序由其决定）。

import (
	"testing"
	"time"
)

// newCatchupTestScheduler 签到 09/21 + 保活 22 + 旅行 09，其余禁用（确定性时点表）。
func newCatchupTestScheduler() *Scheduler {
	return New(Config{
		CheckinHours:   []int{9, 21},
		KeepaliveHours: []int{22},
		TravelHours:    []int{9},
		// travel 与 checkin 同在 9 点：验证同刻合并。
		ActivityDisabled: true,
		SchoolDisabled:   true,
		CatDisabled:      true,
	})
}

// TestDueBatchesSleepAcrossSlots 核心：「20:00 睡到次日 09:05」——21:00（签到）、
// 22:00（保活）、次日 09:00（签到+旅行同刻合并成一批）三个被跨过的槽位全部补跑。
// 修前行为：timer 到期只派发 21:00 那批（排程时确定），22:00 与次日 09:00 永久丢失
// （09:00 过点后 nextFire 直接排到明天）。
func TestDueBatchesSleepAcrossSlots(t *testing.T) {
	s := newCatchupTestScheduler()
	planned := time.Date(2026, 9, 27, 21, 0, 0, 0, time.Local) // 睡前排程的槽位
	wakeAt := time.Date(2026, 9, 28, 9, 5, 0, 0, time.Local)   // 次日 09:05 唤醒

	batches := s.dueBatches(wakeAt, time.Time{}, planned)
	if len(batches) != 3 {
		t.Fatalf("batches=%d want 3（21:00 签到 / 22:00 保活 / 09:00 签到+旅行）", len(batches))
	}
	if want := planned; !batches[0].at.Equal(want) {
		t.Errorf("batch0 at=%v want %v（planned 本身是首个批次）", batches[0].at, want)
	}
	if len(batches[0].kinds) != 1 || batches[0].kinds[0] != taskCheckin {
		t.Errorf("batch0 kinds=%v want [checkin]", batches[0].kinds)
	}
	if want := time.Date(2026, 9, 27, 22, 0, 0, 0, time.Local); !batches[1].at.Equal(want) {
		t.Errorf("batch1 at=%v want %v（保活 22:00）", batches[1].at, want)
	}
	if len(batches[1].kinds) != 1 || batches[1].kinds[0] != taskKeepalive {
		t.Errorf("batch1 kinds=%v want [keepalive]", batches[1].kinds)
	}
	if want := time.Date(2026, 9, 28, 9, 0, 0, 0, time.Local); !batches[2].at.Equal(want) {
		t.Errorf("batch2 at=%v want %v", batches[2].at, want)
	}
	if !hasKind(batches[2].kinds, taskCheckin) || !hasKind(batches[2].kinds, taskTravel) {
		t.Errorf("batch2 kinds=%v want checkin+travel 同刻合并", batches[2].kinds)
	}
	if len(batches[2].kinds) != 2 {
		t.Errorf("batch2 kinds=%v 恰两类（09 点只有签到+旅行）", batches[2].kinds)
	}
}

// TestDueBatchesCursorNoReplay cursor 槽位本身不重跑：从 cursor+1h 起枚举。
// 场景：21:00 批次已派发（cursor=21:00），执行期间又睡到次日 09:05——
// 补跑应含 22:00（保活）与次日 09:00（签到+旅行），不再重放 21:00。
func TestDueBatchesCursorNoReplay(t *testing.T) {
	s := newCatchupTestScheduler()
	cursor := time.Date(2026, 9, 27, 21, 0, 0, 0, time.Local)
	wakeAt := time.Date(2026, 9, 28, 9, 5, 0, 0, time.Local)

	batches := s.dueBatches(wakeAt, cursor, cursor)
	if len(batches) != 2 {
		t.Fatalf("batches=%d want 2（22:00 保活 + 次日 09:00 签到+旅行）", len(batches))
	}
	if want := time.Date(2026, 9, 27, 22, 0, 0, 0, time.Local); !batches[0].at.Equal(want) {
		t.Errorf("batch0 at=%v want %v", batches[0].at, want)
	}
	if want := time.Date(2026, 9, 28, 9, 0, 0, 0, time.Local); !batches[1].at.Equal(want) {
		t.Errorf("batch1 at=%v want %v", batches[1].at, want)
	}
	if hasKind(batches[1].kinds, taskKeepalive) {
		t.Errorf("batch1 kinds=%v 不应含保活（22 点已过）", batches[1].kinds)
	}
}

// TestDueBatchesWindowCutoff 补跑窗口 24h：关机多天不重放整周——窗口外（早于
// now-24h）的槽位被截断。25/26 日的槽位在 3 天后唤醒时已出窗；窗口下限
// （now-24h=27 日 09:05）向下对齐到整点 09:00，该边界整点纳入（枚举恒为
// 整点序列，见 catchUpWindow 注释）。
func TestDueBatchesWindowCutoff(t *testing.T) {
	s := newCatchupTestScheduler()
	planned := time.Date(2026, 9, 25, 21, 0, 0, 0, time.Local)
	wakeAt := time.Date(2026, 9, 28, 9, 5, 0, 0, time.Local) // 3 天后

	batches := s.dueBatches(wakeAt, time.Time{}, planned)
	if len(batches) != 4 {
		t.Fatalf("batches=%d want 4（边界整点 27 日 09:00 + 27 日 21/22 + 28 日 09:00），%v", len(batches), batches)
	}
	// 最老一批是窗口下限对齐出的边界整点，其余批次都在下限之后。
	if want := time.Date(2026, 9, 27, 9, 0, 0, 0, time.Local); !batches[0].at.Equal(want) {
		t.Errorf("batch0 at=%v want %v（边界整点）", batches[0].at, want)
	}
	for _, b := range batches[1:] {
		if b.at.Before(wakeAt.Add(-catchUpWindow)) {
			t.Errorf("batch at=%v 早于补跑窗口下限 %v", b.at, wakeAt.Add(-catchUpWindow))
		}
	}
}

// TestDueBatchesDisabledTaskNotEnqueued 禁用的任务不入批：禁用保活后，
// 22:00 槽位不再出现在补跑列表（热改 SetEnabled 后同口径生效）。
func TestDueBatchesDisabledTaskNotEnqueued(t *testing.T) {
	s := newCatchupTestScheduler()
	s.SetEnabled("keepalive", false)
	planned := time.Date(2026, 9, 27, 20, 0, 0, 0, time.Local)
	wakeAt := time.Date(2026, 9, 27, 22, 30, 0, 0, time.Local)

	batches := s.dueBatches(wakeAt, time.Time{}, planned)
	// 21:00 签到、22:30 唤醒时刻不在整点（无槽位）——只有 21:00 一批。
	if len(batches) != 1 {
		t.Fatalf("batches=%d want 1（仅 21:00 签到）", len(batches))
	}
	if len(batches[0].kinds) != 1 || batches[0].kinds[0] != taskCheckin {
		t.Errorf("batch0 kinds=%v want [checkin]", batches[0].kinds)
	}
}

// TestDueBatchesNoBatchesWhenNothingDue 空窗：cursor 之后无任何已到点槽位
// （准点触发下一槽位前唤醒）→ 空列表，Run 不做任何派发。
func TestDueBatchesNoBatchesWhenNothingDue(t *testing.T) {
	s := newCatchupTestScheduler()
	planned := time.Date(2026, 9, 27, 21, 0, 0, 0, time.Local)
	cursor := time.Date(2026, 9, 27, 21, 0, 0, 0, time.Local)

	// 21:35 唤醒：cursor+1h=22:00 尚未到点 → 无批次。
	if batches := s.dueBatches(planned.Add(35*time.Minute), cursor, planned); len(batches) != 0 {
		t.Errorf("batches=%v want 空（22:00 尚未到点）", batches)
	}
	// 对照：22:05 唤醒则恰好一批（22:00 保活）。
	if batches := s.dueBatches(planned.Add(65*time.Minute), cursor, planned); len(batches) != 1 {
		t.Errorf("batches=%v want 1（22:00 保活）", batches)
	}
}

// TestDueBatchesHalfHourCursorAligned cursor/窗口下限不在整点时对齐本地整点，
// 不残留半点起点（步进恒为整点序列）。
func TestDueBatchesHalfHourCursorAligned(t *testing.T) {
	s := newCatchupTestScheduler()
	cursor := time.Date(2026, 9, 27, 21, 30, 12, 0, time.Local) // 非整点 cursor
	wakeAt := time.Date(2026, 9, 27, 22, 5, 0, 0, time.Local)

	batches := s.dueBatches(wakeAt, cursor, cursor.Add(time.Hour))
	if len(batches) != 1 {
		t.Fatalf("batches=%d want 1", len(batches))
	}
	if want := time.Date(2026, 9, 27, 22, 0, 0, 0, time.Local); !batches[0].at.Equal(want) {
		t.Errorf("batch0 at=%v want %v（22:00 整点）", batches[0].at, want)
	}
}
