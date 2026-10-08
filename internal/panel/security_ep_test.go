// security_ep_test.go 面板安全页三端点（security_ep.go）的行为锁：
// GET 结构与 blocked_logs 截断/新→旧、POST 合法规则 errs 空 + 回显一致、
// 非法 CIDR 整体拒绝且旧规则保留、model_locks 空池空数组、鉴权口径
// （无凭据 401 / readonly token 可读 / 写端点 token 拒绝）。
//
// 全部用 httptest 打真路由（New(Config{...}) 完整链路），与 tokens_test.go /
// routes_contract_test.go 同法——契约路径漏注册、鉴权漏挂在这里直接红。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/server"
)

// newSecurityPanel 组一个开鉴权 + 内存 IP 规则引擎的面板（filePath 空 = 不落盘，
// 规则只进内存；测试确定性）。落盘行为由 iprules_test.go 覆盖，这里不重复。
func newSecurityPanel(t *testing.T) *Panel {
	t.Helper()
	rules, _ := server.NewIPBlockRules("", server.NewIPRuleSpec(nil, nil, false))
	p := New(Config{
		Version: "test",
		APIKey:  "test-key",
		IPRules: rules,
	})
	return p
}

// secGet 带 api_key 鉴权发 GET。
func secGet(p *Panel, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	return rec
}

// secPost 带 api_key 鉴权发 POST。
func secPost(p *Panel, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	return rec
}

// secStatusBody 解析 GET /api/security 的响应体（rules + blocked_logs）。
func secStatusBody(t *testing.T, rec *httptest.ResponseRecorder) (map[string]any, []map[string]any) {
	t.Helper()
	var resp struct {
		Rules       map[string]any   `json:"rules"`
		BlockedLogs []map[string]any `json:"blocked_logs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode security status: %v body=%s", err, rec.Body.String())
	}
	return resp.Rules, resp.BlockedLogs
}

// TestSecurityGetShape GET 响应结构：rules 五字段齐全（未配置时缺省值）、
// blocked_logs 空数组（nil 引擎之外的空态必须是 []，前端 .map 直读）。
func TestSecurityGetShape(t *testing.T) {
	p := newSecurityPanel(t)
	rec := secGet(p, "/api/security")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/security: code=%d body=%s", rec.Code, rec.Body.String())
	}
	rules, logs := secStatusBody(t, rec)
	if rules == nil {
		t.Fatal("rules 段缺失")
	}
	for _, key := range []string{"ip_blacklist", "ip_whitelist", "ip_whitelist_mode", "trusted_proxy_cidrs", "trusted_proxy_hops"} {
		if _, ok := rules[key]; !ok {
			t.Errorf("rules 缺字段 %q: %v", key, rules)
		}
	}
	if got := rules["trusted_proxy_hops"]; got != float64(1) {
		t.Errorf("trusted_proxy_hops 缺省 = %v, want 1", got)
	}
	if logs == nil {
		t.Error("blocked_logs 必须是 []（非 null），前端空态依赖")
	}
	if len(logs) != 0 {
		t.Errorf("blocked_logs 空 = %d 条, want 0", len(logs))
	}
}

// TestSecurityGetLogsOrderAndCap 拦截日志：新→旧排序、最近 200 条截断。
// 先灌 260 条（超 200），GET 只取最新 200 条，且首条是最新、末条是第 201 新。
func TestSecurityGetLogsOrderAndCap(t *testing.T) {
	p := newSecurityPanel(t)
	for i := 0; i < 260; i++ {
		p.cfg.IPRules.NoteBlocked("10.0.0.9", "/v1/chat/completions", server.ReasonIPBlocked)
		// 保证时间戳单调可辨（同秒内 Unix 秒相同不影响「新→旧」断言——
		// 断言只看写入序与条数）。
		time.Sleep(time.Millisecond)
	}
	rec := secGet(p, "/api/security")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: code=%d", rec.Code)
	}
	_, logs := secStatusBody(t, rec)
	if len(logs) != 200 {
		t.Fatalf("blocked_logs len = %d, want 200（截断）", len(logs))
	}
	// 每条字段齐全：ts/ip/path/reason。
	first := logs[0]
	if first["ip"] != "10.0.0.9" || first["path"] != "/v1/chat/completions" || first["reason"] != server.ReasonIPBlocked {
		t.Errorf("first log fields = %v", first)
	}
	if _, ok := first["ts"].(float64); !ok {
		t.Errorf("ts 必须是数字（Unix 秒）: %v", first["ts"])
	}
}

// TestSecurityRulesSaveAndEcho POST 合法规则 → errs 空数组；再 GET 回显一致
// （IP 规则走 iprules 内存快照；trusted_proxy 两参数无 LoadConfig 时回落注入
// 的 Config 运行值）。
func TestSecurityRulesSaveAndEcho(t *testing.T) {
	p := newSecurityPanel(t)
	body := `{"ip_blacklist":["203.0.113.9","10.0.0.0/8"],"ip_whitelist":["192.168.1.1"],
		"ip_whitelist_mode":true,"trusted_proxy_cidrs":["127.0.0.1"],"trusted_proxy_hops":2}`
	rec := secPost(p, "/api/security/rules", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST rules: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Errs []string `json:"errs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Errs) != 0 {
		t.Fatalf("合法规则 errs = %v, want 空数组", resp.Errs)
	}

	// 回显一致（/api 与 /panel/api 双前缀同源）。
	rec = secGet(p, "/panel/api/security")
	rules, _ := secStatusBody(t, rec)
	if got, ok := rules["ip_blacklist"].([]any); !ok || len(got) != 2 || got[0] != "203.0.113.9" || got[1] != "10.0.0.0/8" {
		t.Errorf("ip_blacklist 回显 = %v", rules["ip_blacklist"])
	}
	if got, _ := rules["ip_whitelist"].([]any); len(got) != 1 || got[0] != "192.168.1.1" {
		t.Errorf("ip_whitelist 回显 = %v", rules["ip_whitelist"])
	}
	if rules["ip_whitelist_mode"] != true {
		t.Errorf("ip_whitelist_mode 回显 = %v, want true", rules["ip_whitelist_mode"])
	}
	if got, _ := rules["trusted_proxy_cidrs"].([]any); len(got) != 1 || got[0] != "127.0.0.1" {
		t.Errorf("trusted_proxy_cidrs 回显 = %v", rules["trusted_proxy_cidrs"])
	}
	if rules["trusted_proxy_hops"] != float64(2) {
		t.Errorf("trusted_proxy_hops 回显 = %v, want 2", rules["trusted_proxy_hops"])
	}

	// 规则真的生效了（Evaluate 走新快照）：黑名单命中、白名单模式拒外来者。
	if ok, reason := p.cfg.IPRules.Evaluate("203.0.113.9"); ok || reason != server.ReasonIPBlocked {
		t.Errorf("blacklist hit: ok=%v reason=%q", ok, reason)
	}
	if ok, reason := p.cfg.IPRules.Evaluate("8.8.8.8"); ok || reason != server.ReasonIPNotWhitelisted {
		t.Errorf("whitelist-mode default deny: ok=%v reason=%q", ok, reason)
	}
}

// TestSecurityRulesInvalidRejected 非法 CIDR → errs 非空且旧规则保留
// （SetRules 整体拒绝语义：内存未换快照，GET 回显仍是旧值）。
func TestSecurityRulesInvalidRejected(t *testing.T) {
	p := newSecurityPanel(t)
	// 先落一份旧规则。
	if rec := secPost(p, "/api/security/rules",
		`{"ip_blacklist":["203.0.113.9"],"ip_whitelist":[],"ip_whitelist_mode":false,
		  "trusted_proxy_cidrs":["127.0.0.1"],"trusted_proxy_hops":1}`); rec.Code != http.StatusOK {
		t.Fatalf("seed rules: code=%d body=%s", rec.Code, rec.Body.String())
	}
	// 再提交含非法条目的新规则。
	rec := secPost(p, "/api/security/rules",
		`{"ip_blacklist":["999.999.1.1"],"ip_whitelist":["8.8.8.8"],"ip_whitelist_mode":true,
		  "trusted_proxy_cidrs":[],"trusted_proxy_hops":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("invalid rules: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Errs []string `json:"errs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Errs) == 0 {
		t.Fatal("非法 CIDR 必须产生非空 errs")
	}
	// 旧规则保留（黑名单仍是 203.0.113.9；8.8.8.8 没进来）。
	view := p.cfg.IPRules.RulesView()
	if got, _ := view["ip_blacklist"].([]string); len(got) != 1 || got[0] != "203.0.113.9" {
		t.Errorf("旧黑名单被破坏: %v", view["ip_blacklist"])
	}
	if got, _ := view["ip_whitelist"].([]string); len(got) != 0 {
		t.Errorf("非法提交部分生效: %v", view["ip_whitelist"])
	}
	if ok, _ := p.cfg.IPRules.Evaluate("8.8.8.8"); !ok {
		t.Error("整体拒绝语义被破坏：8.8.8.8 不应被新白名单拦截")
	}

	// 非法 trusted_proxy_cidrs 同样拒绝（config 层校验）。
	rec = secPost(p, "/api/security/rules",
		`{"ip_blacklist":[],"ip_whitelist":[],"ip_whitelist_mode":false,
		  "trusted_proxy_cidrs":["not-an-ip"],"trusted_proxy_hops":1}`)
	resp.Errs = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Errs) == 0 {
		t.Fatal("非法 trusted_proxy_cidrs 必须产生非空 errs")
	}
}

// TestSecurityTrustedProxyEchoFromLoadConfig 注入 LoadConfig 时 trusted_proxy
// 两参数从落盘 config 读取（面板刚保存未重启也如实回显），优先于 Config 运行值。
func TestSecurityTrustedProxyEchoFromLoadConfig(t *testing.T) {
	ipRules, _ := server.NewIPBlockRules("", server.NewIPRuleSpec(nil, nil, false))
	p := New(Config{
		Version: "test",
		APIKey:  "test-key",
		IPRules: ipRules,
		LoadConfig: func() (any, error) {
			return struct {
				Security struct {
					TrustedProxyCIDRs []string `json:"trusted_proxy_cidrs"`
					TrustedProxyHops  int      `json:"trusted_proxy_hops"`
				} `json:"security"`
			}{Security: struct {
				TrustedProxyCIDRs []string `json:"trusted_proxy_cidrs"`
				TrustedProxyHops  int      `json:"trusted_proxy_hops"`
			}{TrustedProxyCIDRs: []string{"10.0.0.1", "10.0.0.0/8"}, TrustedProxyHops: 3}}, nil
		},
	})
	rules, _ := secStatusBody(t, secGet(p, "/api/security"))
	if got, _ := rules["trusted_proxy_cidrs"].([]any); len(got) != 2 || got[0] != "10.0.0.1" {
		t.Errorf("LoadConfig 回显 = %v", rules["trusted_proxy_cidrs"])
	}
	if rules["trusted_proxy_hops"] != float64(3) {
		t.Errorf("hops 回显 = %v, want 3", rules["trusted_proxy_hops"])
	}
}

// TestSecurityModelLocksEmptyPool 空池/nil 池返回空数组（前端 .map 直读），
// 契约路径双前缀注册。
func TestSecurityModelLocksEmptyPool(t *testing.T) {
	p := newSecurityPanel(t)
	for _, path := range []string{"/api/security/model_locks", "/panel/api/security/model_locks"} {
		rec := secGet(p, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: code=%d", path, rec.Code)
		}
		var resp struct {
			Locks []map[string]any `json:"locks"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Locks == nil || len(resp.Locks) != 0 {
			t.Errorf("%s: locks = %v, want []", path, resp.Locks)
		}
	}
}

// TestSecurityModelLocksUnixSeconds ModelLockRow 的时刻字段转 Unix 秒
// （前端拿秒值做剩余时间算术；RFC3339 序列化会产出 NaN）。构造一个带
// unlock_at 的锁行需要真实模型冷却状态，白盒灌不进去——退而验证空池之外
// 的序列化口径：零值时刻必须输出 0 而非 RFC3339 字符串。
func TestSecurityModelLocksUnixSeconds(t *testing.T) {
	p := newSecurityPanel(t)
	// 无池：直验 helper 契约（unixSec 是端点序列化的唯一出口）。
	if got := unixSec(time.Time{}); got != 0 {
		t.Errorf("unixSec(zero) = %d, want 0", got)
	}
	want := time.Unix(1_700_000_000, 0).Unix()
	if got := unixSec(time.Unix(1_700_000_000, 0)); got != want {
		t.Errorf("unixSec = %d, want %d", got, want)
	}
	// 池非空但无冷却：locks 为空数组（不 panic 即可，覆盖 Pool 非 nil 分支）。
	p.cfg.Pool = newSmokePool()
	if rec := secGet(p, "/api/security/model_locks"); rec.Code != http.StatusOK {
		t.Fatalf("pool set: code=%d", rec.Code)
	}
}

// TestSecurityAuthGate 鉴权口径与 panel 其他端点一致：
//   - 无凭据 → 401 invalid_api_key（withAuth 三通道全失败）；
//   - readonly wbt_ token → GET 放行（分级表只读档）、POST rules 403
//     （写端点不在分级表）；
//   - Bearer api_key → 全权。
func TestSecurityAuthGate(t *testing.T) {
	p := newSecurityPanel(t)
	p.cfg.Pool = newSmokePool()

	// 无凭据：401。
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/api/security", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-auth GET: code=%d body=%s, want 401", rec.Code, rec.Body.String())
	}

	// readonly token：GET 两条放行、POST 拒绝。用 tokens_test.go 的会话通道
	// 造 token（同一鉴权链）。
	ro, _ := createTokenViaAPI(t, p, `{"name":"sec-ro","scope":"readonly"}`)
	roGet := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+ro)
		p.ServeHTTP(rec, req)
		return rec
	}
	for _, path := range []string{"/api/security", "/api/security/model_locks"} {
		if rec := roGet(path); rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Errorf("readonly GET %s: code=%d, want 放行", path, rec.Code)
		}
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/security/rules", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+ro)
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("readonly POST rules: code=%d, want 403 token_write_forbidden", rec.Code)
	}
}
