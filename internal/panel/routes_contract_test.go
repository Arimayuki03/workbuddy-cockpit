package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/upstream"
)

func strReader(s string) *strings.Reader { return strings.NewReader(s) }

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// manager 壳前端（web/lib/api.ts）调用的全部 /api/* 契约路径。
// 回归锁：404 page not found 的根因是路由表别名缺失（前端全走 /api/*，
// 后端只有部分端点注册了 /api/* 别名），这里全量枚举锁死——以后新增端点
// 若只挂 /panel 前缀，本测试立即红。/api/request_logs、/api/system/check-update
// 属 server.Handler（不挂 panel mux），不在本表。
func TestManagerShellContractRoutesExist(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key", Pool: newSmokePool(),
		Upstream: &upstream.Client{}})

	getPaths := []string{
		"/api/me",
		"/api/overview",
		"/api/logs",
		"/api/models",
		"/api/model_probes",
		"/api/usage",
		"/api/packages",
		"/api/config",
		"/api/settings/model-map",
		"/api/tasks/queue",
		"/api/school/status",
		"/api/school/vouchers",
		"/api/auth/regions",
		"/api/accounts/u1/tasks",
		"/api/accounts/export",
	}
	postPaths := []string{
		"/api/login",
		"/api/logout",
		"/api/config",
		"/api/usage/save",
		"/api/settings/model-map",
		"/api/settings/upstash/test",
		"/api/checkin_all",
		"/api/travel_all",
		"/api/activity_all",
		"/api/keepalive_all",
		"/api/balance_all",
		"/api/tasks/scan_all",
		"/api/tasks/run_queue",
		"/api/tasks/queue/cancel",
		"/api/school/run_all",
		"/api/auth/start",
		"/api/accounts/import",
		"/api/accounts/u1/revive",
		"/api/accounts/u1/disable",
		"/api/accounts/u1/enable",
		"/api/accounts/u1/checkin",
		"/api/accounts/u1/balance",
		"/api/accounts/u1/clear-cooldown",
		"/api/accounts/u1/remove",
		"/api/accounts/u1/tasks/accept",
		"/api/accounts/u1/tasks/accept_all",
		"/api/accounts/u1/tasks/claim",
		"/api/accounts/u1/tasks/auto",
		"/api/accounts/u1/tasks/auto_all",
	}
	getQueries := map[string]string{
		"/api/auth/poll": "state=x",
	}

	// 鉴权层在路由分发之前吗？否：未注册路径不进 withAuth，直接 mux 404 纯文本。
	// 带上正确 key：已注册的路径过鉴权层后走到 handler（可能 4xx/5xx 业务错），
	// 唯独 404 "404 page not found"（mux 默认纯文本）代表路由缺失。
	for _, path := range getPaths {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer test-key")
		p.ServeHTTP(rec, req)
		if containsMux404(rec.Body.String()) {
			t.Errorf("GET %s: 契约路径未注册（mux 404）", path)
		}
	}
	for _, path := range postPaths {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, nil)
		req.Header.Set("Authorization", "Bearer test-key")
		p.ServeHTTP(rec, req)
		if containsMux404(rec.Body.String()) {
			t.Errorf("POST %s: 契约路径未注册（mux 404）", path)
		}
	}
	for path, q := range getQueries {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path+"?"+q, nil)
		req.Header.Set("Authorization", "Bearer test-key")
		p.ServeHTTP(rec, req)
		if containsMux404(rec.Body.String()) {
			t.Errorf("GET %s: 契约路径未注册（mux 404）", path)
		}
	}
}

// containsMux404 识别 Go mux 默认 404 纯文本（panel writeErr 的 JSON 错误
// 形态是 {"ok":false,...}，业务 4xx/5xx 不算路由缺失）。
func containsMux404(body string) bool {
	return body == "404 page not found\n"
}

// TestUpstashTestEndpoint /api/settings/upstash/test 行为：空 url 提示未配置
// （ok=false 而非 500）；非空 url 走 Probe（不可达地址返回 ok=false + 脱敏
// message，不炸 500）；坏 body 400。
func TestUpstashTestEndpoint(t *testing.T) {
	p := New(Config{Version: "test", APIKey: ""})

	// 空 url → ok=false 提示（不是 500，前端 notify 展示 message）。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/settings/upstash/test", strReader(`{"url":""}`))
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty url: code=%d want 200 (ok:false payload)", rec.Code)
	}
	if want := `"ok":false`; !contains(rec.Body.String(), want) {
		t.Fatalf("empty url: body=%s want %s", rec.Body.String(), want)
	}

	// 不可达地址（保留端口语义的无效 host）→ ok=false + message，不 500。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/settings/upstash/test",
		strReader(`{"url":"rediss://default:tok@127.0.0.1:1","token":""}`))
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unreachable: code=%d want 200", rec.Code)
	}
	if contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("unreachable must not be ok: %s", rec.Body.String())
	}

	// 坏 body → 400。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/settings/upstash/test", strReader(`{`))
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: code=%d want 400", rec.Code)
	}
}
