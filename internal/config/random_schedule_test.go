package config

// random_schedule_test.go v1.19.2 每日随机延后 + 账号错峰（吸收 workbuddy-manager
// 补丁 0003 / CHANGELOG 1.0.91）的配置层契约：
//   - random_delay_minutes 0-60（0=关），越界报错且错误信息带键名；
//   - account_delay_min_seconds/max_seconds 0-600 且 min<=max，越界/倒挂报错带键名；
//   - 旧配置（没有新键）三个值全为 0 = 关闭，行为逐字不变。

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestScheduleRandomDelayBounds 随机延后窗口边界：0/1/30/60 合法（0 = 关闭），
// 负数与 >60 报错（错误信息必须带键名，用户能直接定位 config.json 的哪一行）。
func TestScheduleRandomDelayBounds(t *testing.T) {
	for _, minutes := range []int{0, 1, 30, 60} {
		s := DefaultSchedule()
		s.RandomDelayMinutes = minutes
		if err := s.Normalize(); err != nil {
			t.Fatalf("合法分钟数 %d 被拒: %v", minutes, err)
		}
	}
	for _, minutes := range []int{-1, 61, 999} {
		s := DefaultSchedule()
		s.RandomDelayMinutes = minutes
		err := s.Normalize()
		if err == nil {
			t.Fatalf("非法分钟数 %d 被接受", minutes)
		}
		if !strings.Contains(err.Error(), "random_delay_minutes") {
			t.Errorf("分钟数 %d 的错误信息未带键名: %v", minutes, err)
		}
	}
}

// TestScheduleAccountDelayBounds 账号错峰区间边界：0<=min<=max<=600 合法（0/0 = 关闭），
// 负数 / >600 / min>max 报错（错误信息带键名）。
func TestScheduleAccountDelayBounds(t *testing.T) {
	for _, pair := range [][2]int{{0, 0}, {0, 1}, {0, 120}, {30, 120}, {600, 600}, {0, 600}} {
		s := DefaultSchedule()
		s.AccountDelayMinSeconds, s.AccountDelayMaxSeconds = pair[0], pair[1]
		if err := s.Normalize(); err != nil {
			t.Fatalf("合法区间 %v 被拒: %v", pair, err)
		}
	}
	for _, pair := range [][2]int{{-1, 120}, {0, -1}, {-1, -1}, {30, 29}, {0, 601}, {601, 601}} {
		s := DefaultSchedule()
		s.AccountDelayMinSeconds, s.AccountDelayMaxSeconds = pair[0], pair[1]
		err := s.Normalize()
		if err == nil {
			t.Fatalf("非法区间 %v 被接受", pair)
		}
		if !strings.Contains(err.Error(), "account_delay") {
			t.Errorf("区间 %v 的错误信息未带键名: %v", pair, err)
		}
	}
}

// TestScheduleDelayKeysAbsentDefaultsOff 旧配置兼容：键缺席时三个值保持 0（全关）；
// 显式配置可正常载入；JSON 载入的越界值仍要经 Normalize 拦下（配置层是最后一道闸）。
func TestScheduleDelayKeysAbsentDefaultsOff(t *testing.T) {
	s := DefaultSchedule()
	if err := json.Unmarshal([]byte(`{"checkin_hours":[9,21]}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.RandomDelayMinutes != 0 || s.AccountDelayMinSeconds != 0 || s.AccountDelayMaxSeconds != 0 {
		t.Fatalf("旧配置（无新键）不应开启随机延后/账号错峰: minutes=%d account=%d-%d",
			s.RandomDelayMinutes, s.AccountDelayMinSeconds, s.AccountDelayMaxSeconds)
	}
	if err := s.Normalize(); err != nil {
		t.Fatalf("旧配置归一失败: %v", err)
	}

	// 显式新键载入（面板/手工写的 config.json 形态）。
	s = DefaultSchedule()
	body := `{"random_delay_minutes":30,"account_delay_min_seconds":30,"account_delay_max_seconds":120}`
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatal(err)
	}
	if s.RandomDelayMinutes != 30 || s.AccountDelayMinSeconds != 30 || s.AccountDelayMaxSeconds != 120 {
		t.Fatalf("新键未载入: minutes=%d account=%d-%d",
			s.RandomDelayMinutes, s.AccountDelayMinSeconds, s.AccountDelayMaxSeconds)
	}
	if err := s.Normalize(); err != nil {
		t.Fatalf("显式新键归一失败: %v", err)
	}

	// JSON 载入的越界值：Normalize 必须报错（不能靠 JSON 层的类型校验兜）。
	s = DefaultSchedule()
	if err := json.Unmarshal([]byte(`{"random_delay_minutes":61}`), &s); err != nil {
		t.Fatal(err)
	}
	if err := s.Normalize(); err == nil {
		t.Fatal("JSON 载入的越界 random_delay_minutes 未被拦下")
	}
}
