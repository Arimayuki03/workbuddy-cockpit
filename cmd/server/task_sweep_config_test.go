package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/upstream"
)

// Production wiring is a pure config->options mapping; test that disabled is
// the default and explicit opt-in preserves the interval in minutes.
func TestTaskSweepOptionsFromConfig(t *testing.T) {
	t.Setenv("WB2A_API_KEY", "test-key")
	t.Setenv("WB2A_TASK_SWEEP_ENABLED", "")
	t.Setenv("WB2A_TASK_SWEEP_INTERVAL_MINUTES", "")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	opts := taskSweepOptionsFromConfig(c)
	if opts.Enabled {
		t.Error("task sweep must default to disabled")
	}
	if opts.Interval != 15*time.Minute {
		t.Errorf("default interval=%s want 15m", opts.Interval)
	}

	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(`{"schedule":{"task_sweep_enabled":true,"task_sweep_interval_minutes":30}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	opts = taskSweepOptionsFromConfig(c)
	if !opts.Enabled || opts.Interval != 30*time.Minute {
		t.Fatalf("opts=%+v want enabled/30m", opts)
	}

	// Env overrides the JSON keys with the same semantics.
	t.Setenv("WB2A_TASK_SWEEP_ENABLED", "0")
	t.Setenv("WB2A_TASK_SWEEP_INTERVAL_MINUTES", "20")
	c, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	opts = taskSweepOptionsFromConfig(c)
	if opts.Enabled || opts.Interval != 20*time.Minute {
		t.Fatalf("env opts=%+v want disabled/20m", opts)
	}
}

// The reasoning fallback is a process-wide switch; production main must call
// the same helper. Verify false/true both map, and restore global state so a
// developer's later test run is not polluted.
func TestApplyReasoningFallbackProductionWiring(t *testing.T) {
	t.Cleanup(func() { upstream.ConfigureReasoningFallback(false) })
	off := false
	on := true
	if applyReasoningFallback(&Config{ReasoningAsContent: &off}) {
		t.Fatal("false config enabled reasoning fallback")
	}
	if upstream.ReasoningFallbackEnabled() {
		t.Fatal("global fallback left enabled")
	}
	if !applyReasoningFallback(&Config{ReasoningAsContent: &on}) {
		t.Fatal("true config did not enable reasoning fallback")
	}
	if !upstream.ReasoningFallbackEnabled() {
		t.Fatal("global fallback not enabled by wiring")
	}
	if applyReasoningFallback(nil) {
		t.Fatal("nil config must not enable fallback")
	}
	if upstream.ReasoningFallbackEnabled() {
		t.Fatal("nil config left global fallback enabled")
	}
}
