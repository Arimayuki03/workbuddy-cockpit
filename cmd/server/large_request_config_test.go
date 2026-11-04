package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLargeRequestConfigDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"api_key":"k"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.LargeRequestBytes != 2<<20 ||
		c.Pool.LargeRequestBudget != 6 ||
		c.LargeRequestBudgetWindowDur != 120*time.Second ||
		c.LargeRequestCooldownDur != 0 {
		t.Fatalf("large request defaults=%+v", c.Pool)
	}
}

func TestLargeRequestConfigParseAndDisable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	raw := `{"api_key":"k","pool":{
		"large_request_bytes":1048576,
		"large_request_budget":-1,
		"large_request_budget_window":"45s",
		"large_request_cooldown":"90s"
	}}`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.LargeRequestBytes != 1<<20 || c.Pool.LargeRequestBudget != -1 {
		t.Fatalf("bytes=%d budget=%d", c.Pool.LargeRequestBytes, c.Pool.LargeRequestBudget)
	}
	if c.LargeRequestBudgetWindowDur != 45*time.Second || c.LargeRequestCooldownDur != 90*time.Second {
		t.Fatalf("window=%s cooldown=%s", c.LargeRequestBudgetWindowDur, c.LargeRequestCooldownDur)
	}
}

func TestLargeRequestConfigRejectsBadWindow(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"api_key":"k","pool":{"large_request_budget_window":"nope"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "pool.large_request_budget_window") {
		t.Fatalf("error=%v", err)
	}
}
