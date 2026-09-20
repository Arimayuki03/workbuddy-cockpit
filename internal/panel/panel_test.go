package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/httpauth"
	"workbuddy2api/internal/pool"
)

// newSmokePool overview 冒烟的最小依赖：空池（无账号、无上游调用）。
func newSmokePool() *pool.Pool {
	return pool.New("") // 空 state 路径 = 纯内存形态
}

// 冒烟（v1.2.0 manager 壳契约最小集）：/api/login 成功发 cookie、错密码 401、
// /api/overview 需鉴权、/api/overview 带会话 cookie 通过（Bearer 与 cookie 双通道）。

// smokePool 是 overview 的最小依赖：Pool 为 nil 时 overview 会 panic，
// 用一个空池即可（CountedList 全零）——冒烟只验证路由/鉴权/会话层。
func TestLoginIssuesSessionCookie(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"username":"whatever","password":"test-key"}`))
	req.Header.Set("Content-Type", "application/json")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("login: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK       bool   `json:"ok"`
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Username != "admin" || resp.Role != "admin" {
		t.Fatalf("login resp = %+v", resp)
	}
	cookies := rec.Result().Cookies()
	var sess *http.Cookie
	for _, c := range cookies {
		if c.Name == sessionCookie {
			sess = c
		}
	}
	if sess == nil || sess.Value == "" {
		t.Fatal("login must set wb_session cookie")
	}
	if !sess.HttpOnly {
		t.Error("session cookie must be HttpOnly")
	}
	// cookie 值形态：base64url(payload) + "." + hex(HMAC)。
	if !strings.Contains(sess.Value, ".") {
		t.Fatalf("cookie value not signed form: %q", sess.Value)
	}
	if !httpauth.VerifySessionValue(sess.Value, "test-key") {
		t.Error("issued cookie must verify against the same key")
	}
	if httpauth.VerifySessionValue(sess.Value, "other-key") {
		t.Error("cookie signed with another key must not verify (api_key rotation invalidates sessions)")
	}
}

func TestLoginWrongPassword401(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"username":"x","password":"wrong"}`))
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: code=%d want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_password") {
		t.Fatalf("error body = %s", rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			t.Fatal("failed login must not set session cookie")
		}
	}
}

func TestOverviewRequiresAuth(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key"})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/api/overview", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: code=%d want 401", rec.Code)
	}
}

func TestOverviewPassesWithSessionCookie(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key"})

	// 登录拿 cookie。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"password":"test-key"}`))
	p.ServeHTTP(rec, req)
	var sess *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			sess = c
		}
	}
	if sess == nil {
		t.Fatal("no session cookie from login")
	}

	// cookie 通道访问 /api/overview：过鉴权层后因 Pool 为 nil 不会发生——
	// 冒烟给一个真池（空池，无上游调用）。
	p2 := New(Config{Version: "test", APIKey: "test-key", Pool: newSmokePool()})
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/api/overview", nil)
	req2.AddCookie(sess)
	p2.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("overview with cookie: code=%d body=%s", rec2.Code, rec2.Body.String())
	}
	var got struct {
		Version string `json:"version"`
		Total   int    `json:"total"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != "test" || got.Total != 0 {
		t.Fatalf("overview payload = %+v", got)
	}

	// Bearer 通道依旧可用（双通道并存回归保护）。
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("GET", "/api/overview", nil)
	req3.Header.Set("Authorization", "Bearer test-key")
	p2.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("overview with bearer: code=%d", rec3.Code)
	}
}

func TestLogoutClearsCookie(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key"})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/api/logout", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("logout: code=%d", rec.Code)
	}
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("logout must expire wb_session cookie")
	}
}
