// account_weight_test.go POST /api/accounts/{uid}/weight 端点行为测试。
//
// 覆盖：写入 → 内存/落盘/overview 回显、清除、边界与非法输入、未知 uid、
// 鉴权（未带凭证 401；wbt_ token 写端点恒 403），以及导出→导入后权重保留。
package panel

import (
	"encoding/json"
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

// newWeightTestPanel 组一个带真实 auth 文件的面板：u1 初始 weight=2。
// 同时返回文件路径与 *auth.Auth，供断言「内存即时生效 + 文件原子落盘」。
func newWeightTestPanel(t *testing.T) (*Panel, *auth.Auth, string) {
	t.Helper()
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(authDir, "workbuddy-u1.json")
	raw := `{"auth":{"accessToken":"at-secret","refreshToken":"rt-secret","expiresAt":9999999999,"domain":"www.codebuddy.cn","realm":"cn"},
		"account":{"uid":"u1","nickname":"主力号"},"device_token":"dt-secret","weight":2}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := auth.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	a.FilePath = path
	p := New(Config{
		Version:   "test",
		APIKey:    "test-key",
		Pool:      pool.New(""),
		Upstream:  &upstream.Client{},
		AuthDir:   authDir,
		TokenPath: filepath.Join(dir, "panel_tokens.json"),
	})
	p.cfg.Pool.Add(a)
	return p, a, path
}

func weightPost(p *Panel, uid, body, authz string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/accounts/"+uid+"/weight", strings.NewReader(body))
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	p.ServeHTTP(rec, req)
	return rec
}

func weightDoc(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc
}

// TestAccountWeightSavePersistAndOverview 写入 → 内存立即生效、auth 文件落盘、
// overview 回显生效值；且响应/overview 都不泄露 token。
func TestAccountWeightSavePersistAndOverview(t *testing.T) {
	p, a, path := newWeightTestPanel(t)

	rec := weightPost(p, "u1", `{"weight":3.5}`, "Bearer test-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("save: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK              bool    `json:"ok"`
		Weight          float64 `json:"weight"`
		EffectiveWeight float64 `json:"effective_weight"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Weight != 3.5 || resp.EffectiveWeight != 3.5 {
		t.Fatalf("resp=%+v want ok weight=3.5 effective=3.5", resp)
	}
	// 内存即时生效（选号读同一个 *auth.Auth）。
	if got := a.PickWeight(); got != 3.5 {
		t.Errorf("内存 PickWeight=%v want 3.5", got)
	}
	// 文件落盘：顶层 weight，且凭证原样保留。
	doc := weightDoc(t, path)
	if got, ok := doc["weight"].(float64); !ok || got != 3.5 {
		t.Errorf("落盘 weight=%v want 3.5", doc["weight"])
	}
	if doc["device_token"] != "dt-secret" {
		t.Errorf("落盘丢失 device_token: %v", doc["device_token"])
	}
	authDoc, _ := doc["auth"].(map[string]any)
	if authDoc == nil || authDoc["accessToken"] != "at-secret" || authDoc["refreshToken"] != "rt-secret" {
		t.Errorf("落盘丢失凭证: %v", doc["auth"])
	}

	// overview 回显生效值；凭据不得出现在状态响应里。
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/overview", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("overview: code=%d body=%s", rec.Code, body)
	}
	if !contains(body, `"weight":3.5`) {
		t.Errorf("overview 未回显 weight=3.5: %s", body)
	}
	for _, secret := range []string{"at-secret", "rt-secret", "dt-secret"} {
		if contains(body, secret) {
			t.Errorf("overview 泄露凭证 %q: %s", secret, body)
		}
	}
}

// TestAccountWeightClear 0 恢复默认：返回 weight=0、生效 1，且 auth 文件
// 不再保留 weight 键（避免噪音）；凭证原样保留。
func TestAccountWeightClear(t *testing.T) {
	p, a, path := newWeightTestPanel(t)
	rec := weightPost(p, "u1", `{"weight":0}`, "Bearer test-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Weight          float64 `json:"weight"`
		EffectiveWeight float64 `json:"effective_weight"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Weight != 0 || resp.EffectiveWeight != 1 {
		t.Errorf("clear resp=%+v want stored=0 effective=1", resp)
	}
	if got := a.PickWeight(); got != 1 {
		t.Errorf("清除后内存生效值=%v want 1", got)
	}
	doc := weightDoc(t, path)
	if _, ok := doc["weight"]; ok {
		t.Errorf("恢复默认后不应保留 weight 键: %v", doc["weight"])
	}
	if authDoc, _ := doc["auth"].(map[string]any); authDoc == nil || authDoc["accessToken"] != "at-secret" {
		t.Errorf("清除权重不得影响凭证: %v", doc["auth"])
	}
}

// TestAccountWeightExplicitOne 显式 1 是合法值（manager 服务层同口径）：
// 原样持久化、生效值 1；面板弹窗自己会把 1 转成 0，不会因此产生文件噪音。
func TestAccountWeightExplicitOne(t *testing.T) {
	p, a, path := newWeightTestPanel(t)
	rec := weightPost(p, "u1", `{"weight":1}`, "Bearer test-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("weight=1: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := a.PickWeight(); got != 1 {
		t.Errorf("显式 1 生效值=%v want 1", got)
	}
	doc := weightDoc(t, path)
	if got, _ := doc["weight"].(float64); got != 1 {
		t.Errorf("显式 1 落盘=%v want 1", doc["weight"])
	}
}

// TestAccountWeightBoundaries 端点边界：0.1/100 合法；负数、低于下限、
// 高于上限、缺字段/null/NaN 形态全部 400 且不改内存/文件。
func TestAccountWeightBoundaries(t *testing.T) {
	p, a, path := newWeightTestPanel(t)
	before := weightDoc(t, path)

	okCases := []float64{0.1, 100}
	for _, want := range okCases {
		body, _ := json.Marshal(map[string]float64{"weight": want})
		rec := weightPost(p, "u1", string(body), "Bearer test-key")
		if rec.Code != http.StatusOK {
			t.Fatalf("weight=%v: code=%d body=%s", want, rec.Code, rec.Body.String())
		}
		if got := a.PickWeight(); got != want {
			t.Errorf("weight=%v: PickWeight=%v", want, got)
		}
		doc := weightDoc(t, path)
		if got, _ := doc["weight"].(float64); got != want {
			t.Errorf("weight=%v: 落盘=%v", want, doc["weight"])
		}
	}

	badBodies := []string{
		`{"weight":-1}`,     // 负数
		`{"weight":-0.5}`,   // 负数
		`{"weight":0.05}`,   // 低于下限（不能静默钳成 0.1）
		`{"weight":100.01}`, // 高于上限
		`{"weight":1e999}`,  // 溢出 → 解码错误
		`{"weight":null}`,   // 缺有效值
		`{}`,                // 缺字段
		`{"weight":"2"}`,    // 类型错误
		`{"weight":NaN}`,    // 非法 JSON 字面量
		`{`,                 // 坏 JSON
	}
	for _, body := range badBodies {
		rec := weightPost(p, "u1", body, "Bearer test-key")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code=%d want 400 body=%s", body, rec.Code, rec.Body.String())
		}
	}
	// 非法请求不得留下任何半态：内存仍是最后一次合法值 100，文件仍是 100。
	if got := a.PickWeight(); got != 100 {
		t.Errorf("非法请求污染内存: PickWeight=%v want 100", got)
	}
	after := weightDoc(t, path)
	if got, _ := after["weight"].(float64); got != 100 {
		t.Errorf("非法请求污染落盘: weight=%v want 100", after["weight"])
	}
	if before["auth"] != nil && after["auth"] == nil {
		t.Error("非法请求后凭证丢失")
	}
}

// TestAccountWeightErrors 未知 uid 404。
func TestAccountWeightErrors(t *testing.T) {
	p, _, _ := newWeightTestPanel(t)
	rec := weightPost(p, "nope", `{"weight":2}`, "Bearer test-key")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown uid: code=%d want 404 body=%s", rec.Code, rec.Body.String())
	}
}

// TestAccountWeightAuth 鉴权口径：无凭证 401；wbt_ token（readonly/admin）
// 是写语义 → 403 token_write_forbidden（不进 tokenEndpointLevels 分级表）。
func TestAccountWeightAuth(t *testing.T) {
	p, _, _ := newWeightTestPanel(t)

	rec := weightPost(p, "u1", `{"weight":2}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: code=%d want 401", rec.Code)
	}
	for _, scope := range []string{"readonly", "admin"} {
		tok, _ := createTokenViaAPI(t, p, `{"name":"weight-`+scope+`","scope":"`+scope+`"}`)
		rec = weightPost(p, "u1", `{"weight":2}`, "Bearer "+tok)
		if rec.Code != http.StatusForbidden || !contains(rec.Body.String(), "token_write_forbidden") {
			t.Fatalf("scope=%s: code=%d body=%s want 403 token_write_forbidden", scope, rec.Code, rec.Body.String())
		}
	}
}

// TestAccountWeightExportImportRoundTrip 导出→导入保留权重：导出文档含
// weight，导入方解析并落盘为同一个值（跨部署共享账号配置）。
func TestAccountWeightExportImportRoundTrip(t *testing.T) {
	p, a, _ := newWeightTestPanel(t)
	a.SetWeight(4.5)
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	body := exportBody(t, p)
	if !contains(body, `"weight":4.5`) {
		t.Fatalf("导出文档缺 weight: %s", body)
	}

	dir2 := t.TempDir()
	p2 := New(Config{
		Version:  "test",
		APIKey:   "test-key",
		Pool:     pool.New(""),
		Upstream: &upstream.Client{},
		AuthDir:  filepath.Join(dir2, "auths"),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/accounts/import", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-key")
	p2.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("import: code=%d body=%s", rec.Code, rec.Body.String())
	}
	got := p2.cfg.Pool.AuthByUID("u1")
	if got == nil || got.PickWeight() != 4.5 {
		t.Fatalf("导入后权重丢失: %+v", got)
	}
	saved, err := auth.LoadDir(filepath.Join(dir2, "auths"))
	if err != nil || len(saved) != 1 {
		t.Fatalf("导入后 LoadDir: %v %d", err, len(saved))
	}
	if saved[0].Weight != 4.5 {
		t.Errorf("导入落盘 weight=%v want 4.5", saved[0].Weight)
	}
}

// exportBody 重新取一次导出 JSON（避免测试里复用已被读取的 ResponseRecorder）。
func exportBody(t *testing.T, p *Panel) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/accounts/export", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("export: code=%d body=%s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}
