package upstream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func writeRoutesFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

func TestLoadRoutesFromFileLoadsProxyRoutes(t *testing.T) {
	p := writeRoutesFile(t, `{
		"proxy_routes": {
			"yunserver": "http://u:p@127.0.0.1:17891",
			"lisa": "http://192.0.2.10:20291"
		}
	}`)
	c := New()
	names, err := LoadRoutesFromFile(c, p)
	if err != nil {
		t.Fatalf("LoadRoutesFromFile: %v", err)
	}
	if len(names) != 2 || names[0] != "lisa" || names[1] != "yunserver" {
		t.Fatalf("线路名=%v want [lisa yunserver]", names)
	}
	if hc, err := c.TransportForForTest(&auth.Auth{UID: "u1", ProxyRoute: "yunserver"}); err != nil || hc == nil {
		t.Fatalf("线路未真正装配: client=%v err=%v", hc, err)
	}
}

func TestLoadRoutesFromFileWithoutSection(t *testing.T) {
	p := writeRoutesFile(t, `{"listen":":7863"}`)
	names, err := LoadRoutesFromFile(New(), p)
	if err != nil {
		t.Fatalf("无 proxy_routes 段不该报错: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("无 proxy_routes 段应得空表，得到 %v", names)
	}
}

func TestLoadRoutesFromFileErrors(t *testing.T) {
	if _, err := LoadRoutesFromFile(New(), filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("缺失配置文件应返回错误")
	}
	p := writeRoutesFile(t, `{"proxy_routes":`)
	if _, err := LoadRoutesFromFile(New(), p); err == nil {
		t.Fatal("非法 JSON 应返回错误")
	}
}

func TestConfigPathFromEnvPrefersExplicitPath(t *testing.T) {
	t.Setenv("WB2A_CONFIG", "/tmp/wb2a-explicit.json")
	if got := ConfigPathFromEnv(); got != "/tmp/wb2a-explicit.json" {
		t.Fatalf("ConfigPathFromEnv()=%q", got)
	}
}

func TestLoadRoutesAndLogSpeaks(t *testing.T) {
	p := writeRoutesFile(t, `{"proxy_routes":{"route-a":"http://127.0.0.1:1080"}}`)
	t.Setenv("WB2A_CONFIG", p)
	var lines []string
	names := LoadRoutesAndLog(New(), func(format string, args ...any) {
		lines = append(lines, format)
	})
	if len(names) != 1 {
		t.Fatalf("应加载 1 条线路，得到 %v", names)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "已加载") {
		t.Fatalf("加载日志缺失: %v", lines)
	}
}
