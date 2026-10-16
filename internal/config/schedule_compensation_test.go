// schedule_compensation_test.go v1.19.1「活动补偿」排程键（schedule.compensation_*）契约。
//
// 这一族与其余八类有意不对称，测试把两个不对称点钉死：
//  1. **缺省关**（compensation_enabled 缺省 false）：补偿公告带截止日期，
//     默认交给用户手动「立即执行」；
//  2. **空小时表不回落默认**（compensation_hours 空 = 只手动执行，不自动跑）：
//     若 Normalize 照其它 *_hours 那样回落成默认整点，「留空 = 只手动」会在用户
//     毫不知情的情况下变成「每天自动领补偿」——本文件的核心回归。
//
// 走的是真实装配路径（DefaultSchedule → json.Unmarshal → Normalize），
// 而不是手构结构体：面板设置页/任务页卡片保存的就是这个形态
// （前端 leave-empty 提交 `{"schedule":{"compensation_hours":[]}}`）。
package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// normalizeFromJSON 复刻 cmd/server 的配置装载：默认值前置 + 反序列化覆盖 + 归一。
// 注意本包的结构体就是 schedule 段本身（外层 "schedule" 键在 cmd/server.Config），
// 所以夹具直接给段内键，不再套一层。
func normalizeFromJSON(t *testing.T, raw string) Schedule {
	t.Helper()
	s := DefaultSchedule()
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	if err := s.Normalize(); err != nil {
		t.Fatalf("normalize %s: %v", raw, err)
	}
	return s
}

// TestCompensationScheduleDefaults 缺键默认：enabled=false、hours 为空（不是默认整点）。
func TestCompensationScheduleDefaults(t *testing.T) {
	d := DefaultSchedule()
	if d.CompensationEnabled {
		t.Error("DefaultSchedule 的 compensation_enabled 必须缺省 false（默认不自动跑）")
	}
	if len(d.CompensationHours) != 0 {
		t.Errorf("DefaultSchedule 的 compensation_hours 必须缺省为空：%v", d.CompensationHours)
	}
	// 缺键（空 schedule 段）经真实装载路径后仍是 false / 空。
	s := normalizeFromJSON(t, `{}`)
	if s.CompensationEnabled || len(s.CompensationHours) != 0 {
		t.Errorf("缺键归一出错：enabled=%v hours=%v want false/[]", s.CompensationEnabled, s.CompensationHours)
	}
	// 顺带确认其它族的老语义未被波及（空数组仍回落默认整点）。
	if len(s.CheckinHours) != 2 || s.CheckinHours[0] != 9 {
		t.Errorf("checkin_hours 回落被改动：%v want [9 21]", s.CheckinHours)
	}
}

// TestCompensationHoursEmptyNotFolded 显式空数组 + 开关开启 → **保持空**（不回落默认整点）。
// 这是本功能唯一的「空数组不回落」例外，前端「留空 = 只手动执行」全靠它。
func TestCompensationHoursEmptyNotFolded(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"enabled_true_empty_array", `{"compensation_enabled":true,"compensation_hours":[]}`},
		{"enabled_true_null", `{"compensation_enabled":true,"compensation_hours":null}`},
		{"enabled_true_absent", `{"compensation_enabled":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := normalizeFromJSON(t, tc.raw)
			if !s.CompensationEnabled {
				t.Fatal("开关应保持 true")
			}
			if len(s.CompensationHours) != 0 {
				t.Fatalf("空小时表被回落成默认整点 %v —— 用户「留空 = 只手动」会变成静默自动跑",
					s.CompensationHours)
			}
			// 不报错（先开开关、暂不填时刻是合法配置）。
		})
	}
}

// TestCompensationHoursExplicitKept 显式整点保留且照常校验范围。
func TestCompensationHoursExplicitKept(t *testing.T) {
	s := normalizeFromJSON(t, `{"compensation_enabled":true,"compensation_hours":[10,20]}`)
	if len(s.CompensationHours) != 2 || s.CompensationHours[0] != 10 || s.CompensationHours[1] != 20 {
		t.Errorf("显式 compensation_hours 应保留：[10 20]，得到 %v", s.CompensationHours)
	}
}

// TestCompensationHoursInvalidRejected 越界小时快速失败，错误信息带键名并指向开关。
func TestCompensationHoursInvalidRejected(t *testing.T) {
	for _, hours := range [][]int{{25}, {-1}, {10, 24}} {
		s := DefaultSchedule()
		s.CompensationHours = hours
		err := s.Normalize()
		if err == nil {
			t.Errorf("compensation_hours=%v 应报错", hours)
			continue
		}
		if !strings.Contains(err.Error(), "schedule.compensation_hours") ||
			!strings.Contains(err.Error(), "compensation_enabled") {
			t.Errorf("错误信息应带键名与开关指向：%q", err.Error())
		}
	}
}

// TestCompensationHoursEmptyStillValidAfterInvalidSibling 空小时表本身不触发校验错误
// （validateHours 对空表天然通过）——确保上一条用例不是靠「空表被别处拦下」通过。
func TestCompensationHoursEmptyStillValidAfterInvalidSibling(t *testing.T) {
	s := Schedule{CompensationEnabled: true} // 其余族全空 → 回落默认，无越界值
	if err := s.Normalize(); err != nil {
		t.Fatalf("空配置 + 开启补偿开关应合法：%v", err)
	}
	if len(s.CompensationHours) != 0 {
		t.Errorf("compensation_hours=%v want 空", s.CompensationHours)
	}
}
