// schedule_renew_test.go — schedule.renew_* 配置键（Token 独立续期巡检）的
// 默认值/归一化行为（与既有 queue_hours 的 opt-in 三态同风格）。
package config

import (
	"testing"
)

// TestDefaultScheduleRenewKeys renew_hours 缺省 [3]、renew_enabled 缺省关
// （opt-in：主动打 refresh 写接口，由用户显式打开，与 queue_enabled 同风格）。
func TestDefaultScheduleRenewKeys(t *testing.T) {
	s := DefaultSchedule()
	if s.RenewEnabled {
		t.Error("DefaultSchedule: RenewEnabled 应为 false（opt-in 缺省关）")
	}
	// 注意 RenewEnabled 是缺省 false 的开关，DefaultSchedule 不置 true；
	// RenewHours 则给默认值（normalize 也会兜底）。
	if len(s.RenewHours) != 1 || s.RenewHours[0] != 3 {
		t.Errorf("default renew_hours=%v want [3]", s.RenewHours)
	}
}

// TestNormalizeRenewHours 空数组/null 回落默认 [3]（与其它 *_hours 同口径）。
func TestNormalizeRenewHours(t *testing.T) {
	s := Schedule{}
	if err := s.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(s.RenewHours) != 1 || s.RenewHours[0] != 3 {
		t.Errorf("normalize 后 renew_hours=%v want [3]", s.RenewHours)
	}
	// 显式配置保留。
	s2 := Schedule{RenewHours: []int{4, 16}}
	if err := s2.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(s2.RenewHours) != 2 || s2.RenewHours[0] != 4 || s2.RenewHours[1] != 16 {
		t.Errorf("显式 renew_hours 应保留：%v", s2.RenewHours)
	}
}
