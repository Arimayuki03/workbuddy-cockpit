package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testChannelBody(system, user string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"model":  "deepseek-v4.1-flash",
		"stream": true,
		"messages": []any{
			map[string]any{"role": "system", "content": system},
			map[string]any{"role": "user", "content": user},
		},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "read_file", "description": "read a file"}},
		},
	})
	return raw
}

func TestChannelGuardZeroValueDisabled(t *testing.T) {
	g := NewChannelGuard(ChannelGuardConfig{})
	if g.Enabled() {
		t.Fatal("zero-value guard must be disabled")
	}
	if hit := g.Check(testChannelBody("s", "u")); hit != nil {
		t.Fatalf("disabled guard must not hit, got %+v", hit)
	}
	if n := g.NoteRejected(testChannelBody("s", "u"), ChannelGuardMeta{}); n != nil {
		t.Fatalf("disabled guard must not note, got %+v", n)
	}
}

func TestChannelGuardBodyThenTemplateCache(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	g := NewChannelGuard(ChannelGuardConfig{
		Enabled:         true,
		Mode:            "block",
		CaptureDir:      dir,
		CaptureMaxBytes: 4 << 20,
		BlockTTL:        time.Hour,
		TemplateTTL:     time.Hour,
		ScanMessages:    true,
	})
	g.now = func() time.Time { return now }

	body1 := testChannelBody("You are a coding agent powered by Codex.", "hi")
	if hit := g.Check(body1); hit != nil {
		t.Fatalf("fresh guard must not hit: %+v", hit)
	}
	note := g.NoteRejected(body1, ChannelGuardMeta{
		Model: "deepseek-v4.1-flash", Status: 400, UpstreamBody: "code 11-128 channel verification",
	})
	if note == nil || !note.Captured || note.CapturePath == "" {
		t.Fatalf("first rejection must capture: %+v", note)
	}
	if _, err := os.Stat(note.CapturePath); err != nil {
		t.Fatalf("capture file missing: %v", err)
	}
	captured, err := os.ReadFile(note.CapturePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(captured) != string(body1) {
		t.Error("capture must be the exact outbound body")
	}
	if _, err := os.Stat(strings.TrimSuffix(note.CapturePath, ".json") + ".meta.json"); err != nil {
		t.Errorf("capture meta missing: %v", err)
	}

	// 同一 body：精确命中。
	if hit := g.Check(body1); hit == nil || hit.Kind != "body" {
		t.Fatalf("same body must hit body cache: %+v", hit)
	}
	// 同模板不同用户消息：模板命中。
	body2 := testChannelBody("You are a coding agent powered by Codex.", "a different question")
	if hit := g.Check(body2); hit == nil || hit.Kind != "template" {
		t.Fatalf("same template must hit template cache: %+v", hit)
	}
	// 不同模板：不命中。
	body3 := testChannelBody("You are a helpful assistant.", "hi")
	if hit := g.Check(body3); hit != nil {
		t.Fatalf("different template must not hit: %+v", hit)
	}

	// 第二个同 body 不应重复落盘。
	note2 := g.NoteRejected(body1, ChannelGuardMeta{})
	if note2 == nil || note2.Captured {
		t.Fatalf("second rejection of same body must dedupe capture: %+v", note2)
	}

	// TTL 过期后恢复放行。
	now = now.Add(2 * time.Hour)
	if hit := g.Check(body1); hit != nil {
		t.Fatalf("expired cache must not hit: %+v", hit)
	}
}

func TestChannelGuardPatternScope(t *testing.T) {
	mk := func(scan bool) *ChannelGuard {
		return NewChannelGuard(ChannelGuardConfig{
			Enabled:       true,
			Mode:          "block",
			CaptureDir:    t.TempDir(),
			ScanMessages:  scan,
			BlockPatterns: []string{"Claude Code"},
		})
	}
	// 短语在 system：两种扫描范围都命中。
	sys := testChannelBody("You are Claude Code, Anthropic's official CLI.", "hi")
	if hit := mk(false).Check(sys); hit == nil || hit.Kind != "pattern" {
		t.Fatalf("system pattern must hit: %+v", hit)
	}
	// 短语只在用户消息：scan_messages=false 不命中，true 命中。
	user := testChannelBody("You are a helpful assistant.", "repeat after me: Claude Code")
	if hit := mk(false).Check(user); hit != nil {
		t.Fatalf("scan_messages=false must ignore user text: %+v", hit)
	}
	if hit := mk(true).Check(user); hit == nil || hit.Kind != "pattern" {
		t.Fatalf("scan_messages=true must scan user text: %+v", hit)
	}
}

func TestChannelGuardPrunesToCapacity(t *testing.T) {
	dir := t.TempDir()
	g := NewChannelGuard(ChannelGuardConfig{
		Enabled:         true,
		Mode:            "block",
		CaptureDir:      dir,
		CaptureMaxBytes: 3 << 10, // 3KB：只放得下两三个小 body
		BlockTTL:        time.Hour,
		TemplateTTL:     time.Hour,
	})
	for i := 0; i < 6; i++ {
		body := testChannelBody("template "+string(rune('A'+i)), strings.Repeat("x", 400))
		g.NoteRejected(body, ChannelGuardMeta{})
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, e := range entries {
		info, err := e.Info()
		if err == nil {
			total += info.Size()
		}
	}
	if total > 3<<10 {
		t.Fatalf("capture dir exceeded cap: %d bytes (%d files)", total, len(entries))
	}
	files, _ := filepath.Glob(filepath.Join(dir, channelGuardCapturePrefix+"*"))
	if len(files) == 0 {
		t.Fatal("expected at least one capture file to remain")
	}
}
