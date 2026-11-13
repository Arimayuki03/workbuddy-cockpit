package main

import (
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/upstream"
)

// 改 config.json 后词表可热重载；解析失败保留旧词表。
func TestReloadSanitizeConfig(t *testing.T) {
	t.Cleanup(func() { upstream.ConfigureSanitize(nil, nil) })
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("{\"api_key\":\"test-key\",\"features\":{\"sanitize_extra_features\":[\"foo-phrase\"],\"sanitize_extra_rewrites\":[[\"a\",\"b\"]]}}")
	if !reloadSanitizeConfig(path) {
		t.Fatal("首次热重载失败")
	}
	f, r := upstream.SanitizeExtraCounts()
	if f != 2 || r != 1 {
		t.Fatalf("词表数量不对：features=%d rewrites=%d（rewrite 源串应自动入 features）", f, r)
	}

	// 配置损坏：保留旧词表，不能把正在跑的净化关掉。
	write("{not json")
	if reloadSanitizeConfig(path) {
		t.Fatal("损坏配置不应报告成功")
	}
	f, r = upstream.SanitizeExtraCounts()
	if f != 2 || r != 1 {
		t.Fatalf("解析失败后旧词表被清空：features=%d rewrites=%d", f, r)
	}
}
