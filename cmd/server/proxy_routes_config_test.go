// proxy_routes_config_test.go config.json proxy_routes 段的解析/校验测试：
// 合法 URL 通过、非法 URL fail-fast（启动与面板保存同一套 normalize 链）、
// 面板保存热应用（SetProxyRoutes 整表替换 + client 缓存重建）。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/livecfg"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/upstream"
)

// newSaveConfigHarness saveConfig 的最小装配（照 panel_config_test.go 口径）。
func newSaveConfigHarness(t *testing.T) (*livecfg.Holder, *pool.Pool, *upstream.Client, *scheduler.Scheduler) {
	t.Helper()
	live := livecfg.New(livecfg.Snapshot{APIKey: "k1"})
	p := pool.New("")
	up := upstream.New()
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	return live, p, up, sch
}

// mustAuthWithRoute 带绑定线路的测试账号。
func mustAuthWithRoute(t *testing.T, route string) *auth.Auth {
	t.Helper()
	return &auth.Auth{UID: "u-test", ProxyRoute: route}
}

// TestConfigProxyRoutesNormalize 合法线路表通过 normalize。
func TestConfigProxyRoutesNormalize(t *testing.T) {
	c := Default()
	c.APIKey = "k"
	c.ProxyRoutes = map[string]string{
		"route-a": "http://user:pass@hk-1.example.com:8080",
		"route-b": "https://10.0.0.9:3128",
	}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
}

// TestConfigProxyRoutesFailFast 非法 URL fail-fast：bad scheme / socks5 / 缺 host /
// 缺端口 / 空名字。错误信息带 proxy_routes 前缀便于定位。
func TestConfigProxyRoutesFailFast(t *testing.T) {
	cases := []struct {
		name    string
		routes  map[string]string
		wantSub string
	}{
		{"socks5", map[string]string{"a": "socks5://h:1080"}, "socks5"},
		{"bad scheme", map[string]string{"a": "ftp://h:21"}, "scheme"},
		{"no host", map[string]string{"a": "http://:8080"}, "host"},
		{"no port", map[string]string{"a": "http://h"}, "端口"},
		{"empty name", map[string]string{"": "http://h:80"}, "线路名"},
	}
	for _, tc := range cases {
		c := Default()
		c.APIKey = "k"
		c.ProxyRoutes = tc.routes
		err := c.normalize()
		if err == nil {
			t.Errorf("%s: 应 fail-fast", tc.name)
			continue
		}
		if !strings.HasPrefix(err.Error(), "proxy_routes:") {
			t.Errorf("%s: 错误应有 proxy_routes 前缀: %v", tc.name, err)
		}
		if tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub) {
			t.Errorf("%s: err %q 应含 %q", tc.name, err.Error(), tc.wantSub)
		}
	}
}

// TestLoadProxyRoutesFromFile Load 从 config.json 读 proxy_routes 段（含坏值拒载）。
func TestLoadProxyRoutesFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	// 合法
	os.WriteFile(fp, []byte(`{"api_key":"k","proxy_routes":{"r1":"http://h:1"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("load valid: %v", err)
	}
	if c.ProxyRoutes["r1"] != "http://h:1" {
		t.Fatalf("routes=%v", c.ProxyRoutes)
	}
	// 非法 → 拒载
	os.WriteFile(fp, []byte(`{"api_key":"k","proxy_routes":{"bad":"socks5://h:1"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("非法线路 URL 应拒载")
	}
}

// TestSaveConfigAppliesProxyRoutes 面板保存配置的热应用链：saveConfig 落盘后
// Client 线路表同步整表替换（旧表条目失效 + 新表生效）。
func TestSaveConfigAppliesProxyRoutes(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(cfgPath, []byte(`{"api_key":"k","proxy_routes":{"old":"http://h1:80"}}`), 0o600)

	live, p, up, sch := newSaveConfigHarness(t)
	up.SetProxyRoutes(map[string]string{"old": "http://h1:80"})

	raw := `{"api_key":"k","proxy_routes":{"new":"http://h2:8080"}}`
	if _, err := saveConfig([]byte(raw), cfgPath, live, p, up, sch); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	// 新表生效
	aNew := mustAuthWithRoute(t, "new")
	if _, err := up.TransportForForTest(aNew); err != nil {
		t.Fatalf("new route: %v", err)
	}
	// 旧表条目失效（整表替换语义）
	aOld := mustAuthWithRoute(t, "old")
	if _, err := up.TransportForForTest(aOld); err == nil {
		t.Fatal("旧线路应随整表替换失效")
	}
	// 落盘校验
	blob, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(blob), `"new"`) || strings.Contains(string(blob), `"old"`) {
		t.Fatalf("落盘应为整表替换: %s", blob)
	}
}

// TestRestartRequiredNotIncludingProxyRoutes proxy_routes 可热改，不应进重启清单。
func TestRestartRequiredNotIncludingProxyRoutes(t *testing.T) {
	c := Default()
	for _, f := range restartRequiredFields(c) {
		if strings.Contains(f, "proxy") {
			t.Fatalf("proxy_routes 可热改不应在重启清单: %v", restartRequiredFields(c))
		}
	}
}

// TestExampleConfigProxyRoutesLoad H1 端到端回归：config.example.json（Dockerfile
// COPY 为镜像默认配置、CI cp 为默认 config.json）必须能原样通过 Load+normalize
// 整链——修复前 proxy_routes 段的 _note 说明键被 map[string]string 吸收，
// parseProxyURL 解析空 scheme 必报错，默认镜像启动即 fail-fast。
// （example 的 api_key 是空串待用户填写，normalize 必填校验会拒——测试补一个
// key，模拟「拿到 example 后填 key 启动」的最短路径，其余字节原样。）
func TestExampleConfigProxyRoutesLoad(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("example config not valid JSON: %v", err)
	}
	m["api_key"] = "k1"
	raw, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	if err := os.WriteFile(fp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("example config must load as-is (镜像/CI 默认配置): %v", err)
	}
	// 示例线路表原样生效（_note 已移段外，段内只剩真实线路）。
	if len(c.ProxyRoutes) != 2 {
		t.Fatalf("example proxy_routes = %v, want 2 routes", c.ProxyRoutes)
	}
	if c.ProxyRoutes["route-hk"] == "" || c.ProxyRoutes["route-jp"] == "" {
		t.Fatalf("example proxy_routes entries = %v", c.ProxyRoutes)
	}
}
