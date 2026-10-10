// keys_ep_test.go 面板「API 密钥」页端点的行为锁：CRUD 全流程（创建→明文一次
// 性→列表无明文→PATCH→reset_usage→ips→delete→404）、CIDR 写侧 fail-fast 400、
// exports 全格式随创建下发、鉴权分级（无凭据 401 / token 写 403 / readonly 读放行）。
//
// 路由级用例用 httptest 打真 Panel（withAuth 三通道 + 分级表全链路），
// 与 tokens_test.go / routes_contract_test.go 同法。keystore 本体行为
// （Validate/记账/落盘）由 internal/keystore 的测试锁，这里只锁 HTTP 接线。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/keystore"
)

// newKeysTestPanel 组一个开鉴权 + 注入内存 keystore 的面板（TokenPath 空 =
// 纯内存 token 存储；keystore.Open("") = 纯内存 KeyStore 不落盘）。
func newKeysTestPanel(t *testing.T) *Panel {
	t.Helper()
	p := New(Config{Version: "test", APIKey: "test-key"})
	ks, err := keystore.Open("")
	if err != nil {
		t.Fatalf("open keystore: %v", err)
	}
	p.cfg.KeyStore = ks
	return p
}

// doKeys 按给定凭头发请求（header 可空；body 空串 = 空 body）。
func doKeys(p *Panel, method, path, body string, header map[string]string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	p.ServeHTTP(rec, req)
	return rec
}

// doKeysCookie 会话 cookie 通道发请求（管理端点主通道）。
func doKeysCookie(t *testing.T, p *Panel, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(loginCookie(t, p))
	p.ServeHTTP(rec, req)
	return rec
}

// keyCreateBody 全字段创建请求体（测试按需改字段）。
const keyCreateBody = `{"name":"ci-key","realm":"cn","expires_at":0,"max_ips":0,
"ip_whitelist":["10.0.0.0/8"],"model_whitelist":["cn:glm-5.2"],
"token_quota":1000,"credit_quota":5.5,"rate_limit":30}`

// createKeyViaAPI 会话通道创建一把密钥，返回明文与 key DTO（断言基本形态）。
func createKeyViaAPI(t *testing.T, p *Panel, body string) (string, map[string]any) {
	t.Helper()
	rec := doKeysCookie(t, p, "POST", "/api/keys", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/keys: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Key       map[string]any `json:"key"`
		Plaintext string         `json:"plaintext"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.Plaintext, "wbk_") || resp.Key["id"] == "" {
		t.Fatalf("create resp = %+v", resp)
	}
	return resp.Plaintext, resp.Key
}

// TestKeysCRUDFlow 全流程：创建 → 明文一次性 → 列表无明文 → PATCH（enabled +
// 字段）→ ips → delete → 404。
func TestKeysCRUDFlow(t *testing.T) {
	p := newKeysTestPanel(t)

	// --- 创建：响应含 plaintext（wbk_ 明文）与 key DTO ---
	plain, key := createKeyViaAPI(t, p, keyCreateBody)
	if !strings.HasPrefix(plain, "wbk_") || len(plain) < 20 {
		t.Fatalf("plaintext 形态不对: %q", plain)
	}
	if key["prefix"] != plain[:12] {
		t.Errorf("prefix=%v, want 明文前 12 字符 %q", key["prefix"], plain[:12])
	}
	if _, ok := key["ip_whitelist"].([]any); !ok {
		t.Errorf("ip_whitelist 应透出数组（不限制也非 null）: %v", key["ip_whitelist"])
	}
	id, _ := key["id"].(string)
	if id == "" {
		t.Fatal("key.id 缺失")
	}

	// --- 列表：含该密钥但无明文无哈希，realm/ip_count 形态正确 ---
	rec := doKeysCookie(t, p, "GET", "/api/keys", ``)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: code=%d", rec.Code)
	}
	listBody := rec.Body.String()
	if strings.Contains(listBody, plain) || strings.Contains(listBody, `"hash"`) {
		t.Fatal("列表不得含明文或 hash 字段")
	}
	var list struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Keys) != 1 {
		t.Fatalf("list len=%d, want 1", len(list.Keys))
	}
	if _, ok := list.Keys[0]["ip_count"].(float64); !ok {
		t.Errorf("列表项应含 ip_count（数字）: %v", list.Keys[0])
	}
	if got := list.Keys[0]["realm"]; got != "cn" {
		t.Errorf("realm=%v, want cn", got)
	}
	if got := list.Keys[0]["rate_limit"]; got != float64(30) {
		t.Errorf("rate_limit=%v, want 30", got)
	}

	// --- PATCH enabled=false + 改字段：布尔指针语义（显式 false 生效）---
	rec = doKeysCookie(t, p, "PATCH", "/api/keys/"+id, `{"enabled":false,"token_quota":42,"max_ips":3}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var patched struct {
		Key map[string]any `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &patched); err != nil {
		t.Fatal(err)
	}
	if patched.Key["enabled"] != false {
		t.Errorf("PATCH enabled=false 未生效: %v", patched.Key["enabled"])
	}
	if patched.Key["token_quota"] != float64(42) || patched.Key["max_ips"] != float64(3) {
		t.Errorf("PATCH 字段未生效: %v", patched.Key)
	}
	// 未提交字段不被清掉（部分更新语义）。
	if patched.Key["rate_limit"] != float64(30) {
		t.Errorf("未提交的 rate_limit 不应被改: %v", patched.Key["rate_limit"])
	}

	// --- PATCH 不存在 id → 404 ---
	if rec = doKeysCookie(t, p, "PATCH", "/api/keys/nope", `{}`); rec.Code != http.StatusNotFound {
		t.Errorf("patch 404: code=%d", rec.Code)
	}

	// --- PATCH 显式清零（指针语义）：0 必须写进去（= 改回不限），不得被
	// 「!= 0 判空」静默吞——前端编辑表单恒发全量 payload，用户清空配额即踩中 ---
	rec = doKeysCookie(t, p, "PATCH", "/api/keys/"+id,
		`{"token_quota":0,"credit_quota":0,"rate_limit":0,"max_ips":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch zeros: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var zeroed struct {
		Key map[string]any `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &zeroed); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"token_quota", "credit_quota", "rate_limit", "max_ips"} {
		if zeroed.Key[f] != float64(0) {
			t.Errorf("PATCH %s=0 未生效（清零被吞）: %v", f, zeroed.Key[f])
		}
	}

	// --- PATCH 字段缺省 = 不改（部分更新语义不受指针化影响）---
	rec = doKeysCookie(t, p, "PATCH", "/api/keys/"+id, `{"name":"renamed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch name-only: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var kept struct {
		Key map[string]any `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &kept); err != nil {
		t.Fatal(err)
	}
	if kept.Key["name"] != "renamed" {
		t.Errorf("name 未改: %v", kept.Key["name"])
	}
	if kept.Key["rate_limit"] != float64(0) {
		// 前一步刚清零到 0，本步未提交该字段应保持 0（同时覆盖「缺省不改」）
		t.Errorf("未提交的 rate_limit 应保持 0: %v", kept.Key["rate_limit"])
	}
	if kept.Key["token_quota"] != float64(0) {
		t.Errorf("未提交的 token_quota 应保持 0: %v", kept.Key["token_quota"])
	}

	// --- reset_usage：RecordUse 记账后归零 ---
	k := p.cfg.KeyStore.List()[0]
	p.cfg.KeyStore.RecordUse(&k, "10.0.0.1", 500, 1.25)
	if rec = doKeysCookie(t, p, "POST", "/api/keys/"+id+"/reset_usage", ``); rec.Code != http.StatusOK {
		t.Fatalf("reset_usage: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var list2 struct {
		Keys []struct {
			UsedTokens  int64   `json:"used_tokens"`
			UsedCredits float64 `json:"used_credits"`
			LastUsedAt  int64   `json:"last_used_at"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(func() []byte { r := doKeysCookie(t, p, "GET", "/api/keys", ``); return r.Body.Bytes() }(), &list2); err != nil {
		t.Fatal(err)
	}
	if len(list2.Keys) != 1 || list2.Keys[0].UsedTokens != 0 || list2.Keys[0].UsedCredits != 0 {
		t.Errorf("reset_usage 后 used 应归零: %+v", list2.Keys)
	}

	// --- ips 端点：RecordUse 落的 IP 在数组里；不存在 id 404 ---
	rec = doKeysCookie(t, p, "GET", "/api/keys/"+id+"/ips", ``)
	if rec.Code != http.StatusOK {
		t.Fatalf("ips: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var ips struct {
		IPs []map[string]any `json:"ips"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ips); err != nil {
		t.Fatal(err)
	}
	if len(ips.IPs) != 1 || ips.IPs[0]["ip"] != "10.0.0.1" {
		t.Errorf("ips 应含 RecordUse 落的 10.0.0.1: %+v", ips.IPs)
	}
	if _, ok := ips.IPs[0]["first_seen"].(float64); !ok {
		t.Errorf("first_seen 应为数字: %v", ips.IPs[0]["first_seen"])
	}
	if rec = doKeysCookie(t, p, "GET", "/api/keys/nope/ips", ``); rec.Code != http.StatusNotFound {
		t.Errorf("ips 404: code=%d", rec.Code)
	}

	// --- delete（POST …/delete，前端契约）→ 再 404 → 列表空 ---
	if rec = doKeysCookie(t, p, "POST", "/api/keys/"+id+"/delete", ``); rec.Code != http.StatusOK {
		t.Fatalf("delete: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec = doKeysCookie(t, p, "POST", "/api/keys/"+id+"/delete", ``); rec.Code != http.StatusNotFound {
		t.Errorf("重复 delete 应 404: code=%d", rec.Code)
	}
	rec = doKeysCookie(t, p, "GET", "/api/keys", ``)
	if !strings.Contains(rec.Body.String(), `"keys":[]`) {
		t.Errorf("删除后列表应空: %s", rec.Body.String())
	}
}

// TestKeysCreateCIDRReject 非法 CIDR：创建 400（keystore 写侧 fail-fast 契约），
// PATCH 400 且不落变更；cn: 前缀白名单条目合法。
func TestKeysCreateCIDRReject(t *testing.T) {
	p := newKeysTestPanel(t)
	// 创建：非法条目 400 且不落库。
	rec := doKeysCookie(t, p, "POST", "/api/keys",
		`{"name":"bad","ip_whitelist":["10.0.0.999/33"],"max_ips":0}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad cidr create: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := len(p.cfg.KeyStore.List()); got != 0 {
		t.Errorf("校验失败不应落库: len=%d", got)
	}
	// 合法基准密钥，PATCH 非法 CIDR 400 且原白名单不被改。
	_, key := createKeyViaAPI(t, p, keyCreateBody)
	id, _ := key["id"].(string)
	rec = doKeysCookie(t, p, "PATCH", "/api/keys/"+id, `{"ip_whitelist":["not-a-cidr"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad cidr patch: code=%d body=%s", rec.Code, rec.Body.String())
	}
	list := p.cfg.KeyStore.List()
	if len(list) != 1 || len(list[0].IPWhitelist) != 1 || list[0].IPWhitelist[0] != "10.0.0.0/8" {
		t.Errorf("PATCH 失败不应改库内白名单: %+v", list)
	}
	// 单 IP 与 CIDR 混填合法；cn: 前缀模型白名单条目合法（写法等价裸名）。
	rec = doKeysCookie(t, p, "POST", "/api/keys",
		`{"name":"ok","ip_whitelist":["192.168.1.1","10.0.0.0/8"],"model_whitelist":["cn:glm-5.2"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("cn: 前缀白名单应合法: code=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestKeysCreateExports 全格式导出片段随创建响应下发：9 种格式齐全、codex 片段
// wire_api="responses"、含明文与 Host 推导的 base_url。列表响应不带 exports。
func TestKeysCreateExports(t *testing.T) {
	p := newKeysTestPanel(t)
	// 第一把密钥的明文仅用于后文的「列表不泄露别的明文」对照。
	otherPlain, _ := createKeyViaAPI(t, p, `{"name":"exp","realm":"global",
"model_whitelist":["global:gpt-5.6-sol"]}`)

	rec := doKeysCookie(t, p, "POST", "/api/keys",
		`{"name":"exp2","model_whitelist":["global:gpt-5.6-sol"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Plaintext string `json:"plaintext"`
		Exports   []struct {
			Format   string `json:"format"`
			Filename string `json:"filename"`
			Content  string `json:"content"`
		} `json:"exports"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.Plaintext, "wbk_") {
		t.Fatalf("plaintext=%q", resp.Plaintext)
	}
	if len(resp.Exports) != len(ExportFormats()) {
		t.Fatalf("exports 数=%d, want %d", len(resp.Exports), len(ExportFormats()))
	}
	formats := map[string]bool{}
	for _, e := range resp.Exports {
		formats[e.Format] = true
		if e.Filename == "" || e.Content == "" {
			t.Errorf("片段 %s 缺 filename/content", e.Format)
		}
		// codex-toml 刻意不含明文（codex CLI 把密钥放 auth.json，TOML 只写
		// requires_openai_auth=true；明文由 cc-switch-codex 的 auth 段承载）。
		if e.Format != string(FormatCodexTOML) && !strings.Contains(e.Content, resp.Plaintext) {
			t.Errorf("片段 %s 未内嵌本密钥明文", e.Format)
		}
		// 别把别的密钥的明文带进来。
		if strings.Contains(e.Content, otherPlain) {
			t.Errorf("片段 %s 混入了其它密钥的明文", e.Format)
		}
	}
	for _, f := range ExportFormats() {
		if !formats[string(f)] {
			t.Errorf("缺格式 %s", f)
		}
	}
	// codex 片段：wire_api="responses"（Responses 协议口径，keyexport 契约）+
	// base_url 带请求 Host 推导的 /v1 口径 + 模型取白名单第一项。
	for _, e := range resp.Exports {
		if e.Format == string(FormatCodexTOML) {
			if !strings.Contains(e.Content, `wire_api = "responses"`) {
				t.Errorf("codex 片段缺 wire_api=\"responses\":\n%s", e.Content)
			}
			if !strings.Contains(e.Content, "http://example.com/v1") {
				t.Errorf("codex 片段 base_url 应为请求 Host 推导的 /v1 口径:\n%s", e.Content)
			}
			if !strings.Contains(e.Content, "global:gpt-5.6-sol") {
				t.Errorf("codex 片段 model 应取白名单第一项:\n%s", e.Content)
			}
		}
	}
	// 列表不携带 exports/明文（明文一次性契约）。
	rec = doKeysCookie(t, p, "GET", "/api/keys", ``)
	if strings.Contains(rec.Body.String(), "exports") ||
		strings.Contains(rec.Body.String(), resp.Plaintext) ||
		strings.Contains(rec.Body.String(), otherPlain) {
		t.Error("列表响应不得含 exports 或明文")
	}
}

// TestKeysCreatePlaintextOneShot 第二把创建后：第一把密钥的明文不出现在任何
// 后续响应里（明文一次性契约）。
func TestKeysCreatePlaintextOneShot(t *testing.T) {
	p := newKeysTestPanel(t)
	plain1, key1 := createKeyViaAPI(t, p, `{"name":"one"}`)
	_, _ = createKeyViaAPI(t, p, `{"name":"two"}`)
	id1, _ := key1["id"].(string)
	for _, step := range []struct{ method, path, body string }{
		{"GET", "/api/keys", ``},
		{"PATCH", "/api/keys/" + id1, `{"enabled":false}`},
		{"GET", "/api/keys/" + id1 + "/ips", ``},
		{"POST", "/api/keys/" + id1 + "/reset_usage", ``},
	} {
		rec := doKeysCookie(t, p, step.method, step.path, step.body)
		if rec.Code >= 400 {
			t.Fatalf("%s %s: code=%d body=%s", step.method, step.path, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), plain1) {
			t.Fatalf("%s %s 响应泄露了第一把密钥明文", step.method, step.path)
		}
	}
}

// TestKeysAuthGates 鉴权分级：无凭据 401；wbt_ token 写端点 403
// （token_write_forbidden，admin 档也无例外）；readonly token 只读端点放行；
// Bearer api_key 通道全权。
func TestKeysAuthGates(t *testing.T) {
	p := newKeysTestPanel(t)
	ro, _ := createTokenViaAPI(t, p, `{"name":"ci-ro","scope":"readonly"}`)
	adm, _ := createTokenViaAPI(t, p, `{"name":"ci-adm","scope":"admin"}`)

	// 无凭据 → 401（api_key 非空的面板）。
	for _, step := range []struct{ method, path string }{
		{"GET", "/api/keys"},
		{"POST", "/api/keys"},
		{"PATCH", "/api/keys/x"},
		{"POST", "/api/keys/x/delete"},
		{"POST", "/api/keys/x/reset_usage"},
		{"GET", "/api/keys/x/ips"},
	} {
		rec := doKeys(p, step.method, step.path, ``, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("无凭据 %s %s: code=%d, want 401 (body=%s)", step.method, step.path, rec.Code, rec.Body.String())
		}
	}

	// 写端点族：readonly 与 admin token 一律 403（写语义不进分级表，
	// 与 /api/config 同口径；admin 档也无例外）。
	for _, token := range []string{ro, adm} {
		for _, step := range []struct{ method, path string }{
			{"POST", "/api/keys"},
			{"PATCH", "/api/keys/x"},
			{"POST", "/api/keys/x/delete"},
			{"POST", "/api/keys/x/reset_usage"},
		} {
			rec := doKeys(p, step.method, step.path, `{}`, map[string]string{"Authorization": "Bearer " + token})
			if rec.Code != http.StatusForbidden {
				t.Errorf("token %s %s: code=%d, want 403 (body=%s)", step.method, step.path, rec.Code, rec.Body.String())
				continue
			}
			if !strings.Contains(rec.Body.String(), "token_write_forbidden") {
				t.Errorf("token %s %s: body=%s, want token_write_forbidden", step.method, step.path, rec.Body.String())
			}
		}
	}

	// readonly token：GET /api/keys 放行（200）。
	rec := doKeys(p, "GET", "/api/keys", ``, map[string]string{"Authorization": "Bearer " + ro})
	if rec.Code != http.StatusOK {
		t.Errorf("readonly GET /api/keys: code=%d body=%s", rec.Code, rec.Body.String())
	}
	// readonly token：GET ips（先造一把密钥拿 id）。
	_, key := createKeyViaAPI(t, p, `{"name":"ro-read"}`)
	id, _ := key["id"].(string)
	rec = doKeys(p, "GET", "/api/keys/"+id+"/ips", ``, map[string]string{"Authorization": "Bearer " + ro})
	if rec.Code != http.StatusOK {
		t.Errorf("readonly GET ips: code=%d body=%s", rec.Code, rec.Body.String())
	}
	// /panel 原生前缀同口径。
	rec = doKeys(p, "GET", "/panel/api/keys", ``, map[string]string{"Authorization": "Bearer " + ro})
	if rec.Code != http.StatusOK {
		t.Errorf("readonly GET /panel/api/keys: code=%d", rec.Code)
	}
	// admin token 读也放行（分级表 read 档两档放行）。
	rec = doKeys(p, "GET", "/api/keys", ``, map[string]string{"Authorization": "Bearer " + adm})
	if rec.Code != http.StatusOK {
		t.Errorf("admin GET /api/keys: code=%d", rec.Code)
	}
	// Bearer api_key 通道全权（与 /api/config 同口径）。
	rec = doKeys(p, "POST", "/api/keys", `{"name":"via-apikey"}`, map[string]string{"Authorization": "Bearer test-key"})
	if rec.Code != http.StatusOK {
		t.Errorf("api_key POST /api/keys: code=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestKeysIPsOrdering ips 数组按 first_seen 降序 + 列表 ip_count 同步。
// keystore.RecordUse 是落 IP 的唯一入口（生产由网关调用；测试直接驱动）。
func TestKeysIPsOrdering(t *testing.T) {
	p := newKeysTestPanel(t)
	_, key := createKeyViaAPI(t, p, `{"name":"ips"}`)
	id, _ := key["id"].(string)
	k := p.cfg.KeyStore.List()[0]
	p.cfg.KeyStore.RecordUse(&k, "10.0.0.1", 0, 0)
	p.cfg.KeyStore.RecordUse(&k, "10.0.0.2", 0, 0)
	rec := doKeysCookie(t, p, "GET", "/api/keys/"+id+"/ips", ``)
	if rec.Code != http.StatusOK {
		t.Fatalf("ips: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		IPs []struct {
			IP        string `json:"ip"`
			FirstSeen int64  `json:"first_seen"`
		} `json:"ips"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.IPs) != 2 {
		t.Fatalf("ips len=%d, want 2", len(resp.IPs))
	}
	for i := 1; i < len(resp.IPs); i++ {
		if resp.IPs[i-1].FirstSeen < resp.IPs[i].FirstSeen {
			t.Errorf("ips 应按 first_seen 降序: %+v", resp.IPs)
		}
	}
	// 列表项的 ip_count 同步为 2（DTO 层补算契约）。
	rec = doKeysCookie(t, p, "GET", "/api/keys", ``)
	var listResp struct {
		Keys []struct {
			ID      string `json:"id"`
			IPCount int    `json:"ip_count"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	if len(listResp.Keys) != 1 || listResp.Keys[0].IPCount != 2 {
		t.Errorf("ip_count 应为 2: %+v", listResp.Keys)
	}
}

// TestKeysNilStore501 KeyStore 未注入（最小装配形态）：六端点 501 而非 panic。
func TestKeysNilStore501(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key"}) // KeyStore nil
	for _, step := range []struct{ method, path string }{
		{"GET", "/api/keys"},
		{"POST", "/api/keys"},
		{"PATCH", "/api/keys/x"},
		{"POST", "/api/keys/x/delete"},
		{"POST", "/api/keys/x/reset_usage"},
		{"GET", "/api/keys/x/ips"},
	} {
		rec := doKeys(p, step.method, step.path, `{}`, map[string]string{"Authorization": "Bearer test-key"})
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s %s: code=%d, want 501", step.method, step.path, rec.Code)
		}
	}
}

// TestKeysExportsHostFallback 空 Host 与 X-Forwarded-Proto 的 base_url 口径：
// 转发头 https 优先（反代终结 TLS），Host 空回落 localhost。
func TestKeysExportsHostFallback(t *testing.T) {
	p := newKeysTestPanel(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/keys", strings.NewReader(`{"name":"h"}`))
	req.AddCookie(loginCookie(t, p))
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Host = "panel.example.io"
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "https://panel.example.io/v1") {
		t.Error("X-Forwarded-Proto=https 应反映进导出片段的 base_url")
	}
}
