// hours_clear_test.go ClearHours 的调度侧契约（v1.19.x：补偿族「留空 = 只手动执行」
// 必须**立即**落进运行态，不能等重启）：
//   - 只认合法族名；写入**非 nil** 空切片（nil 会被 hoursOf 当成「该族未热改」而回落
//     启动快照，等于没清）；
//   - 清空后不排程：nextScheduledFire 零时点、nextWake 无候选、dueBatches 空
//     ——即使该族 enabled=true（「开关开 + 留空」是补偿的合法态，不该有任何派发）；
//   - 快照契约：启用但无排程时点 → NextFire 空串（不是 0001-01-01T00:00:00Z）；
//   - SetHours 的「空数组拒绝」通用语义不变（清空的唯一入口是 ClearHours）。
package scheduler

import (
	"testing"
	"time"
)

// clearHoursFix 九族全禁 + compensation 开关开、启动快照配 09:00：
// 唯一排程候选就是 compensation 的那一槽（被清空的必须是运行态里这一份表）。
func clearHoursFix() *Scheduler {
	return New(Config{
		CompensationEnabled: true,
		CompensationHours:   []int{9},
		CheckinDisabled:     true, TravelDisabled: true, ActivityDisabled: true,
		KeepaliveDisabled: true, SchoolDisabled: true, CatDisabled: true,
	})
}

// kindSnapshotOf 取单类任务的快照（不存在即夹具错误）。
func kindSnapshotOf(t *testing.T, s *Scheduler, kind string) KindSnapshot {
	t.Helper()
	for _, sn := range s.SnapshotAll() {
		if sn.Kind == kind {
			return sn
		}
	}
	t.Fatalf("快照里找不到 %s", kind)
	return KindSnapshot{}
}

func TestClearHoursUnknownKind(t *testing.T) {
	s := clearHoursFix()
	if s.ClearHours("nope") {
		t.Fatal("未知族名必须返回 false")
	}
	// 拒绝路径不得改动运行态：启动快照照旧排程（09:00 compensation）。
	now := time.Date(2026, 10, 16, 8, 30, 0, 0, cstZone)
	if at, kinds := s.nextWake(now); at.IsZero() || len(kinds) != 1 || kinds[0] != taskCompensation {
		t.Fatalf("未知族名的拒绝路径改动了运行态：nextWake=%s %v", at, kinds)
	}
}

// TestClearHoursBlanksRuntimeHours ClearHours 后运行态小时表为空且不排程：
// 只写 nil 会回落启动快照（[9]）继续自动跑——本用例同时钉住「非 nil」契约
// 与「清空即停排」行为（回归：面板留空保存后运行态照旧自动领，直到重启）。
func TestClearHoursBlanksRuntimeHours(t *testing.T) {
	s := clearHoursFix()
	now := time.Date(2026, 10, 16, 8, 30, 0, 0, cstZone)
	planned, kinds := s.nextWake(now)
	if planned.IsZero() || len(kinds) != 1 || kinds[0] != taskCompensation {
		t.Fatalf("前置夹具应排到 09:00 [compensation]：%s %v", planned, kinds)
	}

	if !s.ClearHours("compensation") {
		t.Fatal("ClearHours 合法族名应返回 true")
	}
	// 契约：非 nil 空切片（nil = 「未热改」→ hoursOf 回落启动快照，等于没清）。
	if h := s.hoursOf(taskCompensation); h == nil || len(h) != 0 {
		t.Fatalf("ClearHours 必须写入非 nil 空切片，实得 %#v", h)
	}
	// 清空后不排程：零时点（不是回落启动快照的 09:00）。
	if at := s.nextScheduledFire(now, s.hoursOf(taskCompensation), taskCompensation); !at.IsZero() {
		t.Fatalf("清空后 nextScheduledFire=%s want 零时点", at)
	}
	if at, k := s.nextWake(now); !at.IsZero() || len(k) != 0 {
		t.Fatalf("清空后 nextWake=%s %v want 零/空", at, k)
	}
	// 到点补跑：跨过 09:00 醒来也不得派发（hasHour 对空表恒 false），且两条游标
	// 路径（首轮 planned / 递增 cursor）都为空——无重复派发。
	if b := s.dueBatches(now.Add(35*time.Minute), time.Time{}, planned); len(b) != 0 {
		t.Fatalf("清空后被跨过的槽位不得补跑：%+v", b)
	}
	if b := s.dueBatches(now.Add(35*time.Minute), planned, planned); len(b) != 0 {
		t.Fatalf("游标非零路径同样不得派发：%+v", b)
	}
	// 唤醒：照 SetHours 的写法投递一次（Run 立即重排；本测试未跑 Run，通道初始为空，
	// 故此刻必有且只有一次通知）。
	select {
	case <-s.wake:
	default:
		t.Fatal("ClearHours 未投递 wake：Run 不会重排，旧 timer 到点仍会派发")
	}
}

// TestClearHoursSnapshotNextFireEmpty 快照契约：启用但无排程时点（小时表为空）→
// NextFire 必须为空串。修复前零时点 Format 会输出 "0001-01-01T00:00:00Z"，
// 与「"" = 无排程时点」的字段契约矛盾；启动即空与热改清空两条路径同口径。
func TestClearHoursSnapshotNextFireEmpty(t *testing.T) {
	// 启动即空（New 的合法态：开关开、hours 留空，只由 New 记 WARN）。
	empty := New(Config{CompensationEnabled: true, CheckinDisabled: true, TravelDisabled: true,
		ActivityDisabled: true, KeepaliveDisabled: true, SchoolDisabled: true, CatDisabled: true})
	if sn := kindSnapshotOf(t, empty, "compensation"); !sn.Enabled || sn.NextFire != "" {
		t.Fatalf("启用 + 空小时表的快照应无排程时点：%+v", sn)
	}
	// 热改清空（ClearHours）与启动即空一致：enabled 保持 true、hours 空、NextFire ""。
	cleared := clearHoursFix()
	if !cleared.ClearHours("compensation") {
		t.Fatal("ClearHours 合法族名应返回 true")
	}
	sn := kindSnapshotOf(t, cleared, "compensation")
	if !sn.Enabled || sn.NextFire != "" || len(sn.Hours) != 0 {
		t.Fatalf("ClearHours 后快照 = %+v，want enabled=true + next_fire=\"\" + 空 hours", sn)
	}
}

// TestSetHoursStillRejectsEmpty 清空是 ClearHours 的专属语义：SetHours 的空数组
// 拒绝（通用语义 = 「禁用请走 SetEnabled」）对九类照旧，防止两者被互相稀释。
func TestSetHoursStillRejectsEmpty(t *testing.T) {
	s := clearHoursFix()
	if s.SetHours("compensation", []int{}) || s.SetHours("checkin", nil) {
		t.Fatal("SetHours 必须继续拒绝空数组/nil（清空的唯一入口是 ClearHours）")
	}
	if h := s.hoursOf(taskCompensation); len(h) != 1 || h[0] != 9 {
		t.Fatalf("拒绝路径不得改动运行态小时表：%v", h)
	}
}
