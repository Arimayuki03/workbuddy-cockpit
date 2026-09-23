// transfer_test.go 凭据导出/导入端点测试：路由 + 鉴权 + 三种导入形态 +
// 落盘热加载 + 非法 uid 拒绝。导出格式契约（嵌套形往返）在 internal/auth 侧锁死。
package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// newTransferPanel 空池 + 临时 auth 目录的最小面板（导入/导出端点不依赖 scheduler/usage）。
func newTransferPanel(t *testing.T) (*Panel, string) {
	t.Helper()
	dir := t.TempDir()
	p := New(Config{
		Version:  "test",
		APIKey:   "test-key",
		Pool:     pool.New(""),
		Upstream: &upstream.Client{},
		AuthDir:  dir,
	})
	return p, dir
}

func doGet(p *Panel, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	return rec
}

func doPost(p *Panel, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	return rec
}

// TestExportImportRoundtrip 导出 → 清池 → 导入：全链路往返，凭证字段不丢，
// 落盘文件与热加载（Pool.AuthByUID）双确认。
func TestExportImportRoundtrip(t *testing.T) {
	p, dir := newTransferPanel(t)
	// 预置一个账号（直接走 SaveAtomic + pool.Add，模拟既有账号）。
	orig := `{"auth":{"accessToken":"at-1","refreshToken":"rt-1","expiresAt":1753600000,"domain":"www.codebuddy.cn"},"account":{"uid":"uid-1","enterpriseId":"e1","nickname":"n1"},"device_token":"dt-1"}`
	a, err := auth.Parse([]byte(orig))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a.FilePath = filepath.Join(dir, "workbuddy-uid-1.json")
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	p.cfg.Pool.Add(a)

	// 1) 导出：200 + 附件头 + 包裹形 accounts 数组。
	rec := doGet(p, "/api/accounts/export")
	if rec.Code != http.StatusOK {
		t.Fatalf("export code=%d body=%s", rec.Code, rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition=%q want attachment", cd)
	}
	var env struct {
		Accounts []json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("export parse: %v", err)
	}
	if len(env.Accounts) != 1 {
		t.Fatalf("exported %d accounts, want 1", len(env.Accounts))
	}
	// 2) 模拟导入方：空池新目录，原样 POST 导出文件。
	p2, dir2 := newTransferPanel(t)
	rec2 := doPost(p2, "/api/accounts/import", rec.Body.String())
	if rec2.Code != http.StatusOK {
		t.Fatalf("import code=%d body=%s", rec2.Code, rec2.Body.String())
	}
	var imp struct {
		Ok       bool `json:"ok"`
		Imported int  `json:"imported"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &imp); err != nil || !imp.Ok || imp.Imported != 1 {
		t.Fatalf("import resp=%s err=%v", rec2.Body.String(), err)
	}
	// 3) 热加载确认：池内有号且字段全同。
	got := p2.cfg.Pool.AuthByUID("uid-1")
	if got == nil {
		t.Fatal("imported account not in pool")
	}
	if got.AccessTokenValue() != "at-1" || got.RefreshTokenValue() != "rt-1" ||
		got.Nickname != "n1" || got.EnterpriseID != "e1" || got.DeviceToken != "dt-1" {
		t.Errorf("imported fields mismatch: %+v", got)
	}
	// 4) 落盘确认：auths/ 下有文件且含 token（SaveAtomic 嵌套形）。
	saved, err := os.ReadFile(filepath.Join(dir2, "workbuddy-uid-1.json"))
	if err != nil {
		t.Fatalf("saved file: %v", err)
	}
	if !strings.Contains(string(saved), "at-1") {
		t.Errorf("saved file missing token: %s", saved)
	}
}

// TestImportAcceptsFlatAndBareArray 导入兼容形态：扁平形单文件与裸数组
// （手拷 auth 文件、自拼列表）都要能吃。
func TestImportAcceptsFlatAndBareArray(t *testing.T) {
	p, dir := newTransferPanel(t)

	flat := `{"accessToken":"at-f","refreshToken":"rt-f","expiresAt":1753600000,"uid":"uid-flat","nickname":"nf"}`
	rec := doPost(p, "/api/accounts/import", flat)
	if rec.Code != http.StatusOK {
		t.Fatalf("flat import code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := p.cfg.Pool.AuthByUID("uid-flat"); got == nil || got.AccessTokenValue() != "at-f" {
		t.Fatalf("flat import not loaded: %+v", got)
	}

	arr := fmt.Sprintf(`[{"accessToken":"at-a","refreshToken":"rt","uid":"uid-arr1"},{"accessToken":"at-b","refreshToken":"rt","uid":"uid-arr2"}]`)
	rec = doPost(p, "/api/accounts/import", arr)
	if rec.Code != http.StatusOK {
		t.Fatalf("array import code=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, uid := range []string{"uid-arr1", "uid-arr2"} {
		if p.cfg.Pool.AuthByUID(uid) == nil {
			t.Errorf("array item %s not in pool", uid)
		}
	}
	// _ = dir：目录由 import 端点 MkdirAll，存在即可。
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("auth dir: %v", err)
	}
}

// TestImportRejectsBadUID 路径穿越防线：uid 含 ../ 时拒绝落盘并计入 skipped，
// 不得写进 auth 目录、不得进池。
func TestImportRejectsBadUID(t *testing.T) {
	p, dir := newTransferPanel(t)
	bad := `{"accessToken":"at","refreshToken":"rt","uid":"../../evil"}`
	rec := doPost(p, "/api/accounts/import", bad)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var imp struct {
		Imported int `json:"imported"`
		Skipped  []struct {
			Reason string `json:"reason"`
		} `json:"skipped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &imp); err != nil {
		t.Fatalf("parse resp: %v", err)
	}
	if imp.Imported != 0 || len(imp.Skipped) != 1 {
		t.Fatalf("want 0 imported 1 skipped, got %+v", imp)
	}
	if _, err := os.Stat(filepath.Join(dir, "evil.json")); !os.IsNotExist(err) {
		t.Error("path traversal wrote outside auth dir!")
	}
	if p.cfg.Pool.AuthByUID("../../evil") != nil {
		t.Error("bad uid entered pool")
	}
}

// TestImportRejectsGarbage 坏 body 形态：空体/无账号列表 400；单文档解析失败
// 按逐项 skipped 返回（带原因，用户能看到哪个文件坏了、其余项不受影响）。
func TestImportRejectsGarbage(t *testing.T) {
	p, _ := newTransferPanel(t)
	for name, body := range map[string]string{
		"empty":    "",
		"no accts": `{"accounts":[]}`,
	} {
		rec := doPost(p, "/api/accounts/import", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code=%d want 400 body=%s", name, rec.Code, rec.Body.String())
		}
	}
	for name, body := range map[string]string{
		"bad json":  `{not json`,
		"bad token": `{"auth":{"refreshToken":"r"},"account":{"uid":"u"}}`,
	} {
		rec := doPost(p, "/api/accounts/import", body)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: code=%d want 200 (逐项 skipped)", name, rec.Code)
			continue
		}
		var imp struct {
			Imported int `json:"imported"`
			Skipped  []struct {
				Reason string `json:"reason"`
			} `json:"skipped"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &imp); err != nil {
			t.Errorf("%s: parse resp: %v", name, err)
			continue
		}
		if imp.Imported != 0 || len(imp.Skipped) != 1 || imp.Skipped[0].Reason == "" {
			t.Errorf("%s: want 1 skipped with reason, got %+v", name, imp)
		}
	}
}

// TestExportRequiresAuth 导出含全部 token，必须过鉴权闸：无 key 一律 401。
func TestExportRequiresAuth(t *testing.T) {
	p, _ := newTransferPanel(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/accounts/export", nil)
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401", rec.Code)
	}
}
