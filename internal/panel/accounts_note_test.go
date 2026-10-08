// accounts_note_test.go POST /api/accounts/{uid}/note 端点行为测试。
//
// 覆盖：写入 → /api/overview 回显、空串清除、404 未知 uid、400 坏体/超长、
// 鉴权（未带凭证 401；wbt_ token 写端点恒 403 token_write_forbidden）。
// 路由注册由 routes_contract_test.go 的契约表加锁（新增端点必须在表内）。
package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// newNoteTestPool 带 u1 账号的最小面板装配（空 state 路径 = 纯内存池）。
func newNoteTestPool() *Panel {
	p := New(Config{Version: "test", APIKey: "test-key", Pool: newSmokePool(),
		Upstream: &upstream.Client{}})
	p.cfg.Pool.Add(&auth.Auth{UID: "u1"})
	return p
}

// TestAccountNoteSaveAndClear 写入 → overview 回显 note；空串清除 → note 消失。
func TestAccountNoteSaveAndClear(t *testing.T) {
	p := newNoteTestPool()

	// 写入
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/accounts/u1/note", strReader(`{"note":"生产主力号"}`))
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("save body=%s", rec.Body.String())
	}

	// overview 回显
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/overview", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	if !contains(rec.Body.String(), `"note":"生产主力号"`) {
		t.Fatalf("overview 未回显 note: %s", rec.Body.String())
	}

	// 空串清除
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/accounts/u1/note", strReader(`{"note":""}`))
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/overview", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	if contains(rec.Body.String(), `"note"`) {
		t.Fatalf("清除后 overview 不应再含 note 键: %s", rec.Body.String())
	}
}

// TestAccountNoteErrors 未知 uid 404；坏 body 400；超长 400。
func TestAccountNoteErrors(t *testing.T) {
	p := newNoteTestPool()

	// 未知 uid → 404（与 disable/enable 同判据）
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/accounts/nope/note", strReader(`{"note":"x"}`))
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown uid: code=%d want 404", rec.Code)
	}

	// 坏 body → 400
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/accounts/u1/note", strReader(`{`))
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: code=%d want 400", rec.Code)
	}

	// 超长（>200 字符）→ 400
	long := strings.Repeat("长", 201)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/accounts/u1/note", strReader(`{"note":"`+long+`"}`))
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("too long: code=%d want 400", rec.Code)
	}
}

// TestAccountNoteAuth 鉴权口径：无凭证 401；wbt_ token 是写语义 → 403
// token_write_forbidden（不进 tokenEndpointLevels 分级表）。
func TestAccountNoteAuth(t *testing.T) {
	p := newNoteTestPool()

	// 未带任何凭证 → 401
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/accounts/u1/note", strReader(`{"note":"x"}`))
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: code=%d want 401", rec.Code)
	}

	// wbt_ token → 分级表外恒 403（写端点口径）。用 tokens_test.go 的会话通道
	// 造 token（readonly/admin 同拒——本端点根本不在分级表内）。
	ro, _ := createTokenViaAPI(t, p, `{"name":"note-ro","scope":"readonly"}`)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/accounts/u1/note", strReader(`{"note":"x"}`))
	req.Header.Set("Authorization", "Bearer "+ro)
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !contains(rec.Body.String(), "token_write_forbidden") {
		t.Fatalf("wbt_ token: code=%d body=%s want 403 token_write_forbidden", rec.Code, rec.Body.String())
	}
}
