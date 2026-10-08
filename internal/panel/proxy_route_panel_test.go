// proxy_route_panel_test.go GET /api/proxy_routes 与 POST /api/accounts/{uid}/proxy_route
// 端点行为测试（照 accounts_note_test.go 风格）。
//
// 覆盖：线路表回显（密码脱敏 ***）、绑定视图（只含已绑定账号）、绑定/解绑、
// 404 未知 uid、400 坏体/未知线路、鉴权（未带凭证 401；wbt_ token 写端点恒 403）。
// 路由注册由 routes_contract_test.go 的契约表加锁。
package panel

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// newProxyRouteTestPanel 带线路表 config 的最小面板装配：tmp 目录写一份
// config.json（LoadConfig 闭包从盘读，模拟生产装配），池里放 u1/u2 两个账号。
func newProxyRouteTestPanel(t *testing.T) *Panel {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfgRaw := `{"proxy_routes":{"route-a":"http://bob:secret@hk-1.example.com:8080","route-b":"http://10.0.0.9:3128"}}`
	if err := os.WriteFile(cfgPath, []byte(cfgRaw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	p := New(Config{Version: "test", APIKey: "test-key", Pool: newSmokePool(),
		Upstream: &upstream.Client{}})
	p.cfg.ConfigPath = cfgPath
	p.cfg.LoadConfig = func() (any, error) {
		// 测试用最小 Load：直接返回带 ProxyRoutes 的 map（panel 侧只按 JSON 往返读）。
		return map[string]any{"proxy_routes": map[string]any{
			"route-a": "http://bob:secret@hk-1.example.com:8080",
			"route-b": "http://10.0.0.9:3128",
		}}, nil
	}
	p.cfg.Pool.Add(&auth.Auth{UID: "u1"})
	p.cfg.Pool.Add(&auth.Auth{UID: "u2"})
	return p
}

// TestProxyRoutesList 线路表回显：密码打码 ***、用户名与 host 保留；accounts
// 初始为空（未绑定不出现）。
func TestProxyRoutesList(t *testing.T) {
	p := newProxyRouteTestPanel(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/proxy_routes", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !contains(body, `"route-a":"http://bob:***@hk-1.example.com:8080"`) {
		t.Fatalf("route-a 未脱敏回显: %s", body)
	}
	if strings.Contains(body, "secret") {
		t.Fatalf("明文密码泄漏: %s", body)
	}
	if !contains(body, `"route-b":"http://10.0.0.9:3128"`) {
		t.Fatalf("route-b 缺失: %s", body)
	}
	if contains(body, `"accounts":{"u`) {
		t.Fatalf("未绑定时 accounts 不应含账号: %s", body)
	}
}

// TestAccountProxyRouteBindUnbind 绑定 → 回显 accounts；解绑（空 route）→ 消失。
// 绑定写 auth 文件（FilePath 设置时 SaveAtomic 落盘）。
func TestAccountProxyRouteBindUnbind(t *testing.T) {
	p := newProxyRouteTestPanel(t)
	// 给 u1 一个真实文件路径验证落盘（AccessToken 必须非空：SaveAtomic 防御
	// 空凭证覆盖的既有口径）
	fp := filepath.Join(t.TempDir(), "workbuddy-u1.json")
	if err := os.WriteFile(fp, []byte(`{"auth":{"accessToken":"tok-u1","refreshToken":"r"},"account":{"uid":"u1"}}`), 0o600); err != nil {
		t.Fatalf("seed auth file: %v", err)
	}
	a1 := p.cfg.Pool.AuthByUID("u1")
	a1.FilePath = fp
	a1.AccessToken = "tok-u1"

	// 绑定 route-a
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/accounts/u1/proxy_route", strReader(`{"route":"route-a"}`))
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("bind: code=%d body=%s", rec.Code, rec.Body.String())
	}
	// 内存生效
	if got := a1.ProxyRouteValue(); got != "route-a" {
		t.Fatalf("内存绑定=%q", got)
	}
	// 落盘生效（重新 Parse 验证 round trip）
	raw, _ := os.ReadFile(fp)
	parsed, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if parsed.ProxyRoute != "route-a" {
		t.Fatalf("落盘绑定=%q", parsed.ProxyRoute)
	}
	// GET 回显 accounts
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/proxy_routes", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	if !contains(rec.Body.String(), `"accounts":{"u1":"route-a"}`) {
		t.Fatalf("绑定视图: %s", rec.Body.String())
	}
	// 解绑（空 route）
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/accounts/u1/proxy_route", strReader(`{"route":""}`))
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unbind: code=%d", rec.Code)
	}
	if got := a1.ProxyRouteValue(); got != "" {
		t.Fatalf("解绑后=%q", got)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/proxy_routes", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	if contains(rec.Body.String(), `"u1"`) {
		t.Fatalf("解绑后 accounts 不应含 u1: %s", rec.Body.String())
	}
}

// TestAccountProxyRouteErrors 未知 uid 404；坏 body 400；未知线路 400 unknown_route。
func TestAccountProxyRouteErrors(t *testing.T) {
	p := newProxyRouteTestPanel(t)
	// 未知 uid → 404
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/accounts/nope/proxy_route", strReader(`{"route":"route-a"}`))
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown uid: code=%d want 404", rec.Code)
	}
	// 坏 body → 400
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/accounts/u1/proxy_route", strReader(`{`))
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: code=%d want 400", rec.Code)
	}
	// 未知线路 → 400 unknown_route（绑定不存在的线路 = 自断状态，提前拦截）
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/accounts/u1/proxy_route", strReader(`{"route":"ghost"}`))
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !contains(rec.Body.String(), "unknown_route") {
		t.Fatalf("ghost route: code=%d body=%s want 400 unknown_route", rec.Code, rec.Body.String())
	}
}

// TestAccountProxyRouteAuth 鉴权口径：无凭证 401；wbt_ token 写语义 → 403
// token_write_forbidden（与 note 端点同口径，不进分级表）。
func TestAccountProxyRouteAuth(t *testing.T) {
	p := newProxyRouteTestPanel(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/accounts/u1/proxy_route", strReader(`{"route":"route-a"}`))
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: code=%d want 401", rec.Code)
	}
	ro, _ := createTokenViaAPI(t, p, `{"name":"pr-ro","scope":"readonly"}`)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/accounts/u1/proxy_route", strReader(`{"route":"route-a"}`))
	req.Header.Set("Authorization", "Bearer "+ro)
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !contains(rec.Body.String(), "token_write_forbidden") {
		t.Fatalf("wbt_ token: code=%d body=%s want 403 token_write_forbidden", rec.Code, rec.Body.String())
	}
}
