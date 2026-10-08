// tokens_test.go 管理面 API token 的行为锁：生成/解析往返、明文不入库、
// scope 降级与越权拒绝、last_used 节流（注入时钟）、停用即时失效、
// 落盘往返与并发安全。
//
// 路由级用例用 httptest 打真 Panel（withAuth 三通道 + 分级表全链路），
// 与 routes_contract_test.go 同法——白名单语义漂移（新增端点漏归类）在
// 这里直接红。
package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTokenTestPanel 组一个开鉴权的面板 + 空 token 落盘路径（临时目录）。
func newTokenTestPanel(t *testing.T) (*Panel, string) {
	t.Helper()
	dir := t.TempDir()
	p := New(Config{
		Version:   "test",
		APIKey:    "test-key",
		TokenPath: filepath.Join(dir, "panel_tokens.json"),
	})
	return p, filepath.Join(dir, "panel_tokens.json")
}

// loginCookie 走 /api/login 换发会话 cookie（管理端点用）。
func loginCookie(t *testing.T, p *Panel) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"password":"test-key"}`))
	p.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("login 未发会话 cookie")
	return nil
}

// createTokenViaAPI 会话通道创建 token，返回明文与记录。
func createTokenViaAPI(t *testing.T, p *Panel, body string) (string, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/tokens", strings.NewReader(body))
	req.AddCookie(loginCookie(t, p))
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/tokens: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK    bool           `json:"ok"`
		Token string         `json:"token"`
		Rec   map[string]any `json:"token_record"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || !strings.HasPrefix(resp.Token, tokenPrefix) {
		t.Fatalf("create resp = %+v", resp)
	}
	return resp.Token, resp.Rec
}

// TestTokenCreateResolveRoundtrip Create→Resolve 往返；正确 token 命中，
// 错 token / 陌生前缀 / 非 wbt_ 形态全部拒绝。
func TestTokenCreateResolveRoundtrip(t *testing.T) {
	p, path := newTokenTestPanel(t)
	plain, rec := createTokenViaAPI(t, p, `{"name":"ci","scope":"readonly"}`)

	// 记录字段：只存摘要与前缀，CreatedAt 已填。
	if rec["prefix"] != plain[:tokenPrefixLen] {
		t.Fatalf("prefix = %v, want %q", rec["prefix"], plain[:tokenPrefixLen])
	}
	if rec["scope"] != scopeReadonly {
		t.Fatalf("scope = %v, want readonly", rec["scope"])
	}
	if rec["created_at"].(float64) <= 0 {
		t.Fatalf("created_at not set: %v", rec["created_at"])
	}

	// 往返：明文可解析，摘要匹配。
	if got := p.tokenStore.Resolve(plain); got == nil || got.ID != rec["id"].(string) {
		t.Fatalf("Resolve(plain) = %v", got)
	}
	// 错 token（同前缀形态、不同值）：拒绝。
	if got := p.tokenStore.Resolve(tokenPrefix + strings.Repeat("A", 43)); got != nil {
		t.Fatalf("wrong token must not resolve: %v", got)
	}
	// 非 wbt_ 形态：拒绝。
	if got := p.tokenStore.Resolve("wbk_somegatewaykey"); got != nil {
		t.Fatalf("wbk_ key must not resolve via token store")
	}
	if got := p.tokenStore.Resolve(""); got != nil {
		t.Fatal("empty token must not resolve")
	}

	// 明文绝不入库：落盘文件与内存记录都不得包含明文。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), plain) {
		t.Fatal("plaintext token leaked into on-disk store")
	}
	for _, tok := range p.tokenStore.List() {
		if tok.SHA256 == plain || tok.SHA256 == "" {
			t.Fatalf("stored sha256 invalid: %q", tok.SHA256)
		}
	}
}

// TestNormalizeScopeFallsBackToReadonly 非法 scope 值一律降级 readonly（最小
// 权限，不报错）；大小写与空白归一化。
func TestNormalizeScopeFallsBackToReadonly(t *testing.T) {
	cases := map[string]string{
		"":         scopeReadonly,
		"readonly": scopeReadonly,
		"admin":    scopeAdmin,
		"ADMIN":    scopeAdmin, // 大小写归一
		" admin ":  scopeAdmin,
		"root":     scopeReadonly, // 非法 → 降级
		"Admin":    scopeReadonly, // ToLower 后 "admin" 命中合法值——注释占位
		"write":    scopeReadonly,
	}
	// 修正上一行的预期：normalizeScope 先 TrimSpace 再 ToLower，"Admin" 归一为
	// "admin"（合法值大小写不敏感）；非法值判定只看归一化结果。
	cases["Admin"] = scopeAdmin
	delete(cases, "Admin ")
	for in, want := range cases {
		if got := normalizeScope(in); got != want {
			t.Errorf("normalizeScope(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTokenScopesOnRealRoutes 用 httptest 打真路由：
// readonly → 只读 GET 放行、一切 POST 拒绝（403 token_write_forbidden）；
// admin → 幂等运维 POST 放行、状态变更写端点仍拒绝；
// 白名单外端点（含 token 管理、export、config）两档都拒绝。
func TestTokenScopesOnRealRoutes(t *testing.T) {
	p, _ := newTokenTestPanel(t)
	ro, _ := createTokenViaAPI(t, p, `{"name":"ci-ro","scope":"readonly"}`)
	adm, _ := createTokenViaAPI(t, p, `{"name":"ci-adm","scope":"admin"}`)

	// Pool 为 nil 时只读 GET 会 panic（overview 直取 cfg.Pool）——给个空池。
	p.cfg.Pool = newSmokePool()

	get := func(token, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		p.ServeHTTP(rec, req)
		return rec
	}
	post := func(token, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		p.ServeHTTP(rec, req)
		return rec
	}
	del := func(token, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("DELETE", path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		p.ServeHTTP(rec, req)
		return rec
	}

	// --- readonly：只读端点放行（未注入 Usage 的 usage 端点 501，也证明过了
	// 403 闸——断言只要不是 401/403 即视为放行）---
	for _, path := range []string{
		"/api/overview", "/api/logs", "/api/usage", "/api/model_probes",
		"/api/tasks/queue", "/api/school/vouchers", "/api/packages",
	} {
		rec := get(ro, path)
		if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Errorf("readonly GET %s: code=%d body=%s, want past auth gate", path, rec.Code, rec.Body.String())
		}
	}
	// /panel 前缀原生路径同样放行（同一 withAuth）。
	if rec := get(ro, "/panel/api/overview"); rec.Code != http.StatusOK {
		t.Errorf("readonly GET /panel/api/overview: code=%d", rec.Code)
	}

	// --- readonly：写端点一律 403 token_write_forbidden ---
	for _, path := range []string{
		"/api/accounts/u1/disable", "/api/accounts/u1/enable", "/api/accounts/u1/remove",
		"/api/accounts/u1/balance", // POST 幂等运维也仅 admin
		"/api/config", "/api/usage/save", "/api/checkin_all", "/api/balance_all",
		"/api/accounts/import", "/api/settings/model-map", "/api/tasks/scan_all",
	} {
		rec := post(ro, path, `{}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("readonly POST %s: code=%d, want 403 (body=%s)", path, rec.Code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), "token_write_forbidden") {
			t.Errorf("readonly POST %s: body=%s, want token_write_forbidden", path, rec.Body.String())
		}
	}
	// DELETE 写方法同样拒绝：分级表无 DELETE 条目。未注册的 DELETE 路径由
	// mux 405 拒绝（同样到不了 handler），注册过的 DELETE（token 管理族）
	// 挂 withAuthSession → 401；分级语义由 TestTokenEndpointKeyWildcard 与
	// tokenAllowed 判定单测锁定。
	rec := httptest.NewRecorder()
	reqDel := httptest.NewRequest("DELETE", "/api/overview", nil)
	reqDel.Header.Set("Authorization", "Bearer "+ro)
	p.ServeHTTP(rec, reqDel)
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("readonly DELETE /api/overview: code=%d, want 403/405", rec.Code)
	}

	// --- admin：幂等运维放行（上游不可达 → 业务错误码而非 403）---
	// accountBalance 在账号不存在时 404；关键断言是「过了 403 闸」。
	if rec := post(adm, "/api/accounts/u1/balance", `{}`); rec.Code == http.StatusForbidden {
		t.Errorf("admin POST balance: unexpectedly 403 (body=%s)", rec.Body.String())
	}
	// usage/save 未注入 Usage 记录器时 501——同样证明过了 403 闸。
	if rec := post(adm, "/api/usage/save", `{}`); rec.Code == http.StatusForbidden {
		t.Errorf("admin POST usage/save: unexpectedly 403")
	}
	if rec := post(adm, "/api/balance_all", `{}`); rec.Code == http.StatusForbidden {
		t.Errorf("admin POST balance_all: unexpectedly 403 (scheduler nil 走 501，不该 403)")
	}

	// --- admin：状态变更写端点仍然拒绝 ---
	for _, path := range []string{
		"/api/accounts/u1/disable", "/api/accounts/u1/enable", "/api/accounts/u1/remove",
		"/api/config", "/api/settings/model-map", "/api/accounts/import",
	} {
		if rec := post(adm, path, `{}`); rec.Code != http.StatusForbidden {
			t.Errorf("admin POST %s: code=%d, want 403 (body=%s)", path, rec.Code, rec.Body.String())
		}
	}

	// --- 白名单外：token 管理本身两档都拒绝（泄露不能自助续命/提权）。
	// /api/tokens* 挂 withAuthSession（仅会话 cookie），token 鉴权不通过 →
	// 401 session_required；sessionAuth 对 Bearer 通道恒 false，效果等价拒绝。
	for _, tok := range []string{ro, adm} {
		rec := get(tok, "/api/tokens")
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Errorf("token GET /api/tokens: code=%d, want 401/403", rec.Code)
		}
		if rec := post(tok, "/api/tokens", `{}`); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Errorf("token POST /api/tokens: code=%d, want 401/403", rec.Code)
		}
		if rec := del(tok, "/api/tokens/whatever"); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Errorf("token DELETE /api/tokens/{id}: code=%d, want 401/403", rec.Code)
		}
	}
	// export 含上游凭据明文：两档都拒绝。
	if rec := get(adm, "/api/accounts/export"); rec.Code != http.StatusForbidden {
		t.Errorf("admin GET /api/accounts/export: code=%d, want 403", rec.Code)
	}
	// GET /api/config 含配置全貌（读但敏感）：两档都拒绝。
	if rec := get(adm, "/api/config"); rec.Code != http.StatusForbidden {
		t.Errorf("admin GET /api/config: code=%d, want 403", rec.Code)
	}
	// 登录/登出族：logout 挂 loopback（不挂鉴权闸），cookie 清除动作本身无
	// 害且幂等——token 语义上它不是面板数据端点，不属于分级表，但 logout
	// 走的是 loopback 直挂路径（无鉴权），行为是 200 清 cookie。此处锁
	// 「token 不会因 logout 获得任何面板数据」即可；真正要拒绝的是 login 族。
	if rec := post(adm, "/api/auth/start", `{}`); rec.Code != http.StatusForbidden {
		t.Errorf("admin POST /api/auth/start（OAuth 加号，落盘新凭证）: code=%d, want 403", rec.Code)
	}
	if rec := post(adm, "/api/tasks/scan_all", `{}`); rec.Code != http.StatusForbidden {
		t.Errorf("admin POST /api/tasks/scan_all: code=%d, want 403", rec.Code)
	}
}

// TestBadTokenUnauthorized 伪造/失效 token 统一 401，且不泄露原因（不存在、
// 已停用、摘要不符——错误体同形）。
func TestBadTokenUnauthorized(t *testing.T) {
	p, _ := newTokenTestPanel(t)
	plain, _ := createTokenViaAPI(t, p, `{"name":"t"}`)
	p.cfg.Pool = newSmokePool()

	probe := func(token string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/overview", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		p.ServeHTTP(rec, req)
		return rec
	}

	// 摘要不符 / 乱写前缀 / 空 token：401。
	for _, bad := range []string{
		tokenPrefix + strings.Repeat("Z", 43), // 同形伪造
		"garbage",
		"",
	} {
		if rec := probe(bad); rec.Code != http.StatusUnauthorized {
			t.Errorf("bad token %q: code=%d, want 401", bad, rec.Code)
		}
	}

	// 已停用 token：401，且与「不存在」的响应体同形（不给探测信号）。
	recMissing := probe(tokenPrefix + strings.Repeat("Z", 43))
	if rec := probe(plain); rec.Code != http.StatusOK {
		t.Fatalf("valid token: code=%d body=%s", rec.Code, rec.Body.String())
	}
	idRec := p.tokenStore.List()[0]
	if !p.tokenStore.Disable(idRec.ID, true) {
		t.Fatal("disable failed")
	}
	recDisabled := probe(plain)
	if recDisabled.Code != http.StatusUnauthorized {
		t.Fatalf("disabled token: code=%d, want 401", recDisabled.Code)
	}
	if recDisabled.Body.String() != recMissing.Body.String() {
		t.Errorf("disabled vs missing token bodies differ: %q vs %q（不得泄露原因）",
			recDisabled.Body.String(), recMissing.Body.String())
	}
	// 恢复后立即可用（停用/恢复即时生效，无缓存）。
	if !p.tokenStore.Disable(idRec.ID, false) {
		t.Fatal("re-enable failed")
	}
	if rec := probe(plain); rec.Code != http.StatusOK {
		t.Fatalf("re-enabled token: code=%d body=%s", rec.Code, rec.Body.String())
	}
	// 删除后立即失效。
	if !p.tokenStore.Delete(idRec.ID) {
		t.Fatal("delete failed")
	}
	if rec := probe(plain); rec.Code != http.StatusUnauthorized {
		t.Fatalf("deleted token: code=%d, want 401", rec.Code)
	}
}

// TestTokenLastUsedThrottle 注入时钟验证 60s 节流：窗口内多次 Resolve 只落盘
// 一次，跨窗口后下一次才更新。
func TestTokenLastUsedThrottle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panel_tokens.json")
	store := NewTokenStore(path)
	now := time.Unix(1_700_000_000, 0)
	store.now = func() time.Time { return now }

	tok, _ := store.Create("ci", scopeReadonly)
	tokID := tok.ID

	touches := func(n int) {
		for i := 0; i < n; i++ {
			store.touch(tokID)
		}
	}

	touches(5) // 窗口内 5 次
	if got := store.List()[0].LastUsedAt; got != now.Unix() {
		t.Fatalf("first touch recorded %d, want %d", got, now.Unix())
	}
	mtime := fileModTime(t, path)

	now = now.Add(30 * time.Second)
	touches(5) // 窗口内再 5 次：不更新
	if got := store.List()[0].LastUsedAt; got != now.Add(-30*time.Second).Unix() {
		t.Fatalf("throttled touch leaked: last_used=%d", got)
	}
	if got := fileModTime(t, path); !got.Equal(mtime) {
		t.Error("throttled touches must not rewrite the file")
	}

	now = now.Add(31 * time.Second) // 距上次 > 60s
	touches(1)
	if got := store.List()[0].LastUsedAt; got != now.Unix() {
		t.Fatalf("post-window touch recorded %d, want %d", got, now.Unix())
	}
	if got := fileModTime(t, path); got.Equal(mtime) {
		t.Error("post-window touch must rewrite the file")
	}
}

// fileModTime 读文件 mtime（节流断言用）。
func fileModTime(t *testing.T, path string) time.Time {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

// TestTokenPersistRoundtrip 落盘往返：新建 → 重开存储 → 明文仍可解析、
// 元数据（scope/disabled/last_used）保真；损坏文件零状态启动。
func TestTokenPersistRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panel_tokens.json")

	s1 := NewTokenStore(path)
	tok, plain := s1.Create("ci-deploy", scopeAdmin)
	if tok.Scope != scopeAdmin {
		t.Fatalf("scope = %q", tok.Scope)
	}
	// 非法 scope 落盘前已归一化。
	s1.Create("bad-scope", "superuser")

	s2 := NewTokenStore(path) // 重开（启用态落盘往返）
	if got := s2.Resolve(plain); got == nil || got.Disabled || got.Scope != scopeAdmin {
		t.Fatalf("roundtrip resolve = %+v", got)
	}
	if n := len(s2.List()); n != 2 {
		t.Fatalf("roundtrip list len = %d, want 2", n)
	}
	for _, rec := range s2.List() {
		if rec.Scope != scopeAdmin && rec.Scope != scopeReadonly {
			t.Fatalf("persisted scope not normalized: %q", rec.Scope)
		}
	}

	// 停用态落盘往返：停用 → 重开 → 明文不再解析（disabled 即时失效跨重启）。
	if !s2.Disable(tok.ID, true) {
		t.Fatal("disable failed")
	}
	s3 := NewTokenStore(path)
	if got := s3.Resolve(plain); got != nil {
		t.Fatalf("disabled token must not resolve after roundtrip, got %+v", got)
	}
	if rec := s3.List()[0]; rec.Name != "ci-deploy" || !rec.Disabled {
		t.Fatalf("disabled record lost metadata: %+v", rec)
	}

	// 损坏文件：零状态启动，不 panic、不带半条记录。
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s4 := NewTokenStore(path)
	if len(s4.List()) != 0 {
		t.Fatalf("corrupt file must yield empty store, got %d", len(s4.List()))
	}
}

// TestTokenConcurrentResolveCreate -race 压力：并发 Create / Resolve /
// Disable / List / touch 与真路由请求混跑，锁语义坏了立即红。
func TestTokenConcurrentResolveCreate(t *testing.T) {
	p, _ := newTokenTestPanel(t)
	p.cfg.Pool = newSmokePool()

	// 并发停用 goroutine：启停交替（Disable(true)/Disable(false)），与请求
	// goroutine 并发跑。请求侧容忍 401——那正是「停用即时生效」语义的体现
	// （token 恰好处于停用窗口时被拒）；断言的关键是**绝不出现 403/500**：
	// 403 意味着分级表误伤只读端点，500 意味着并发写坏了内部状态。
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_, plain := p.tokenStore.Create(fmt.Sprintf("t%d-%d", i, j), scopeReadonly)
				if p.tokenStore.Resolve(plain) == nil {
					t.Error("concurrent resolve failed")
					return
				}
				_ = p.tokenStore.List()
				req := httptest.NewRequest("GET", "/api/overview", nil)
				req.Header.Set("Authorization", "Bearer "+plain)
				rec := httptest.NewRecorder()
				p.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK && rec.Code != http.StatusUnauthorized {
					t.Errorf("concurrent token request: code=%d body=%s", rec.Code, rec.Body.String())
					return
				}
			}
		}(i)
	}
	// 并发启停既有 token（每轮全量翻转 disabled 位，制造与 Resolve 的竞争）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 40; j++ {
			for _, rec := range p.tokenStore.List() {
				_ = p.tokenStore.Disable(rec.ID, j%2 == 0)
			}
		}
	}()
	wg.Wait()
}

// TestTokenManagementRequiresSession 管理 token 本身只认会话 cookie：
// 无凭据 401；会话通过；Bearer api_key（全权通道）也不得管理 token
// （与 wbt_ 同一闸——凭据不能管理凭据，只有登录态可以）。
func TestTokenManagementRequiresSession(t *testing.T) {
	p, _ := newTokenTestPanel(t)

	// 无凭据 → 401 session_required。
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/api/tokens", nil))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "session_required") {
		t.Fatalf("no-auth list: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// Bearer api_key 也不行（session_required，不是 invalid_api_key）。
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/tokens", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("api-key list: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 会话 cookie 通过，且列表不含明文/摘要字段。
	plain, _ := createTokenViaAPI(t, p, `{"name":"x"}`)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/tokens", nil)
	req.AddCookie(loginCookie(t, p))
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session list: code=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, plain) || strings.Contains(body, `"sha256"`) {
		t.Fatal("token list must not expose plaintext or digest")
	}
}

// TestTokenEndpointKeyWildcard uid 通配归一：/panel 前缀剥离、export 不被
// 误通配、未知形态不命中。
func TestTokenEndpointKeyWildcard(t *testing.T) {
	cases := map[[2]string]string{
		{"GET", "/api/accounts/abc/tasks"}:    "GET /api/accounts/{uid}/tasks",
		{"GET", "/panel/api/accounts/x/tasks"}: "GET /api/accounts/{uid}/tasks",
		{"GET", "/api/accounts/export"}:       "GET /api/accounts/export",
		{"POST", "/api/accounts/u1/balance"}:  "POST /api/accounts/{uid}/balance",
		{"GET", "/api/overview"}:              "GET /api/overview",
		{"GET", "/api/unknown/path/deep"}:     "GET /api/unknown/path/deep",
	}
	for in, want := range cases {
		r := httptest.NewRequest(in[0], in[1], nil)
		if got := tokenEndpointKey(r); got != want {
			t.Errorf("tokenEndpointKey(%s %s) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

// TestPanelWithoutTokenPath 纯内存形态（TokenPath 为空）：功能完整、不落盘。
func TestPanelWithoutTokenPath(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key"}) // TokenPath 空
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/tokens", strings.NewReader(`{"name":"mem"}`))
	req.AddCookie(loginCookie(t, p))
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("in-memory create: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if p.tokenStore.Resolve(resp.Token) == nil {
		t.Fatal("in-memory token must resolve")
	}
}
