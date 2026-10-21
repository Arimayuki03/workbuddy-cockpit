package main

import (
	"os"
	"path/filepath"
	"testing"
)

// reasoning-as-content must default to false: clients that render both fields
// would otherwise show the same reasoning text twice.
func TestReasoningAsContentDefaultsOff(t *testing.T) {
	t.Setenv("WB2A_API_KEY", "test-key")
	t.Setenv("WB2A_REASONING_AS_CONTENT", "")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.ReasoningAsContent == nil || *c.ReasoningAsContent {
		t.Fatalf("default reasoning_as_content=%v want false", c.ReasoningAsContent)
	}
}

func TestReasoningAsContentJSONAndEnvOverride(t *testing.T) {
	t.Setenv("WB2A_API_KEY", "test-key")
	dir := t.TempDir()
	onPath := filepath.Join(dir, "on.json")
	if err := os.WriteFile(onPath, []byte(`{"reasoning_as_content":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	offPath := filepath.Join(dir, "off.json")
	if err := os.WriteFile(offPath, []byte(`{"reasoning_as_content":false}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("WB2A_REASONING_AS_CONTENT", "")
	c, err := Load(onPath)
	if err != nil {
		t.Fatal(err)
	}
	if c.ReasoningAsContent == nil || !*c.ReasoningAsContent {
		t.Fatalf("JSON true not honored: %v", c.ReasoningAsContent)
	}

	c, err = Load(offPath)
	if err != nil {
		t.Fatal(err)
	}
	if c.ReasoningAsContent == nil || *c.ReasoningAsContent {
		t.Fatalf("JSON false not honored: %v", c.ReasoningAsContent)
	}

	t.Setenv("WB2A_REASONING_AS_CONTENT", "1")
	c, err = Load(offPath)
	if err != nil {
		t.Fatal(err)
	}
	if c.ReasoningAsContent == nil || !*c.ReasoningAsContent {
		t.Fatalf("env true should override JSON false: %v", c.ReasoningAsContent)
	}

	t.Setenv("WB2A_REASONING_AS_CONTENT", "0")
	c, err = Load(onPath)
	if err != nil {
		t.Fatal(err)
	}
	if c.ReasoningAsContent == nil || *c.ReasoningAsContent {
		t.Fatalf("env false should override JSON true: %v", c.ReasoningAsContent)
	}
}
