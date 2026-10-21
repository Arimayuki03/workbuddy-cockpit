package config

import (
	"strings"
	"testing"
)

// Task reward sweep is opt-in: missing keys must stay disabled and the default
// interval must be the 15m safety floor.
func TestTaskSweepDefaultsOffAndFifteenMinutes(t *testing.T) {
	d := DefaultSchedule()
	if d.TaskSweepEnabled {
		t.Error("DefaultSchedule task_sweep_enabled must be false")
	}
	if d.TaskSweepIntervalMinutes != 15 {
		t.Errorf("default interval=%d want 15", d.TaskSweepIntervalMinutes)
	}
	s := normalizeFromJSON(t, `{}`)
	if s.TaskSweepEnabled || s.TaskSweepIntervalMinutes != 15 {
		t.Errorf("missing keys -> enabled=%v interval=%d want false/15",
			s.TaskSweepEnabled, s.TaskSweepIntervalMinutes)
	}
}

func TestTaskSweepExplicitOptIn(t *testing.T) {
	s := normalizeFromJSON(t, `{"task_sweep_enabled":true,"task_sweep_interval_minutes":30}`)
	if !s.TaskSweepEnabled || s.TaskSweepIntervalMinutes != 30 {
		t.Fatalf("explicit opt-in not honored: %+v", s)
	}
	// 0 = 未配置 → 回落安全默认，而不是 0 周期。
	s = normalizeFromJSON(t, `{"task_sweep_enabled":true,"task_sweep_interval_minutes":0}`)
	if s.TaskSweepIntervalMinutes != 15 {
		t.Errorf("zero interval=%d want 15", s.TaskSweepIntervalMinutes)
	}
}

func TestTaskSweepRejectsBelowMinimum(t *testing.T) {
	for _, mins := range []int{-1, 1, 14} {
		s := DefaultSchedule()
		s.TaskSweepIntervalMinutes = mins
		err := s.Normalize()
		if err == nil {
			t.Fatalf("interval=%d must be rejected", mins)
		}
		if !strings.Contains(err.Error(), "task_sweep_interval_minutes") {
			t.Errorf("interval=%d error must name the key: %v", mins, err)
		}
	}
}
