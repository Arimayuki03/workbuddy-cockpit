package server

// admin_auth_test.go /admin/accounts/* 的鉴权隔离（review HIGH）：
// wbk_ 分发密钥此前在 withAuth 段1 命中后对任何路径直接放行，等于把
// disable/enable/revive/models/available 这些会改账号运行态的管理端点
// 暴露给每一把分发密钥。修复后 wbk_ 访问 /admin/ 前缀一律 403，只有
// 全局 api_key（或面板 session cookie）能进管理面。

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/keystore"
	"workbuddy2api/internal/upstream"
)

// postAdminWithKey 发一个管理面 POST（key 为空时不带 Authorization）。
func postAdminWithKey(h *Handler, path, key string) *httptest.ResponseRecorder {
	return doWithKey(h, http.MethodPost, path, key)
}

// doWithKey 发一个带 Bearer 的请求（key 为空时不带 Authorization）。
func doWithKey(h *Handler, method, path, key string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	h.ServeHTTP(rec, req)
	return rec
}

// getWithBearer 发一个 GET（用于 /v1/models 的 wbk_ 放行断言）。
func getWithBearer(h *Handler, path, key string) *httptest.ResponseRecorder {
	return doWithKey(h, http.MethodGet, path, key)
}

func TestDistribKeyCannotAccessAdminEndpoints(t *testing.T) {
	ks, err := keystore.Open("")
	if err != nil {
		t.Fatalf("keystore.Open: %v", err)
	}
	plain := mustCreateKey(t, ks, keystore.CreateParams{Name: "分发"})

	p := testPoolWith(keyTestAccount())
	p.BlockModelBackoff("u1", "glm-5.3", upstream.ModelBlockReason)
	h := NewHandler(Config{
		Pool:     p,
		Upstream: upstream.New(),
		APIKey:   "at1",
		KeyStore: ks,
		Admin:    AdminConfig{Enabled: true},
	})

	// 1) wbk_ → /admin/accounts/{uid}/disable：403，且管理动作不得执行。
	rec := postAdminWithKey(h, "/admin/accounts/u1/disable", plain)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wbk_ disable code=%d want 403 body=%s", rec.Code, rec.Body)
	}
	decodeKeyError(t, rec.Body.String(), "admin_requires_gateway_key")
	if stopped, _, found := p.ManualDisabledState("u1"); !found || stopped {
		t.Fatalf("wbk_ 请求不得执行 disable（账号应仍未被手动停用）: found=%v stopped=%v", found, stopped)
	}
	// 既有 revive 入口同样被隔离（均属 /admin/accounts/*）。
	if rec := postAdminWithKey(h, "/admin/accounts/u1/revive", plain); rec.Code != http.StatusForbidden {
		t.Fatalf("wbk_ revive code=%d want 403 body=%s", rec.Code, rec.Body)
	}

	// 2) wbk_ → /admin/accounts/{uid}/models/{m}/available：403，且不得清负缓存。
	rec = postAdminWithKey(h, "/admin/accounts/u1/models/glm-5.3/available", plain)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wbk_ models/available code=%d want 403 body=%s", rec.Code, rec.Body)
	}
	if st, _ := p.Status("u1"); len(st.RateLimitedModels) != 1 {
		t.Fatalf("wbk_ 请求不得清 11102 负缓存: %+v", st.RateLimitedModels)
	}

	// 3) 同一把 wbk_ 密钥访问 /v1/models 仍放行（只隔离管理面，不误伤模型调用面）。
	if rec := getWithBearer(h, "/v1/models", plain); rec.Code != http.StatusOK {
		t.Fatalf("wbk_ /v1/models code=%d want 200 body=%s", rec.Code, rec.Body)
	}

	// 4) 全局 api_key 走同一路径仍可用：200 且动作真正执行。
	rec = postAdminWithKey(h, "/admin/accounts/u1/disable", "at1")
	if rec.Code != http.StatusOK {
		t.Fatalf("gateway key disable code=%d want 200 body=%s", rec.Code, rec.Body)
	}
	if stopped, _, found := p.ManualDisabledState("u1"); !found || !stopped {
		t.Fatalf("gateway key 的 disable 应生效: found=%v stopped=%v", found, stopped)
	}
	rec = postAdminWithKey(h, "/admin/accounts/u1/models/glm-5.3/available", "at1")
	if rec.Code != http.StatusOK {
		t.Fatalf("gateway key models/available code=%d want 200 body=%s", rec.Code, rec.Body)
	}
	if st, _ := p.Status("u1"); len(st.RateLimitedModels) != 0 {
		t.Fatalf("gateway key 的 available 应清负缓存: %+v", st.RateLimitedModels)
	}
}

// TestDistribKeyCannotAccessOpsEndpoints（review HIGH）：wbk_ 隔离不能只拦
// /admin/——/status（全量账号状态）、/api/request_logs（跨 key 请求日志）、
// GET /v1/stats 与 POST /v1/stats/reset（全局统计读取/清零）同属管理面，
// 此前可被任意分发密钥访问。修复后统一 403 admin_requires_gateway_key；
// 模型调用面 /v1/models 与全局 api_key 的管理面访问零回归。
func TestDistribKeyCannotAccessOpsEndpoints(t *testing.T) {
	ks, err := keystore.Open("")
	if err != nil {
		t.Fatalf("keystore.Open: %v", err)
	}
	plain := mustCreateKey(t, ks, keystore.CreateParams{Name: "分发"})

	p := testPoolWith(keyTestAccount())
	h := NewHandler(Config{
		Pool:     p,
		Upstream: upstream.New(),
		APIKey:   "at1",
		KeyStore: ks,
		Admin:    AdminConfig{Enabled: true},
	})

	cases := []struct{ method, path string }{
		{http.MethodGet, "/status"},
		{http.MethodGet, "/api/request_logs"},
		{http.MethodGet, "/v1/stats"},
		{http.MethodPost, "/v1/stats/reset"},
	}
	for _, c := range cases {
		rec := doWithKey(h, c.method, c.path, plain)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("wbk_ %s %s code=%d want 403 body=%s", c.method, c.path, rec.Code, rec.Body)
		}
		decodeKeyError(t, rec.Body.String(), "admin_requires_gateway_key")
	}

	// 模型调用/列表面不受隔离影响。
	if rec := getWithBearer(h, "/v1/models", plain); rec.Code != http.StatusOK {
		t.Fatalf("wbk_ /v1/models code=%d want 200 body=%s", rec.Code, rec.Body)
	}
	// 全局 api_key 仍是管理/运维面的合法凭据。
	if rec := getWithBearer(h, "/status", "at1"); rec.Code != http.StatusOK {
		t.Fatalf("gateway key /status code=%d want 200 body=%s", rec.Code, rec.Body)
	}
}
