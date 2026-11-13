// 客户端余额协议端点测试（manager v2.11.12 / v2.11.13 吸收）。
package server

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/keystore"
	"workbuddy2api/internal/pool"
)

func creditTestPool() *pool.Pool {
	p := pool.New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	add := func(uid, domain string, credits int64) {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid, ExpiresAt: 9999999999, Domain: domain})
		p.SetCredits(uid, credits)
	}
	add("cn1", "", 100)
	add("cn2", "", 50)
	add("g1", "workbuddy.ai", 200)
	add("cn-banned", "", 999)
	add("g-manual", "workbuddy.ai", 888)
	p.Disable("cn-banned", "banned by upstream")
	if found, changed := p.SetManualDisabled("g-manual", true, "paused"); !found || !changed {
		panic("manual disable fixture failed")
	}
	return p
}

func creditTestHandler(t *testing.T) (*Handler, *keystore.Store) {
	t.Helper()
	ks, err := keystore.Open("")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: creditTestPool(), KeyStore: ks, APIKey: "admin-key"})
	return h, ks
}

func getCreditJSON(t *testing.T, h *Handler, path, bearer string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v body=%s", path, err, rec.Body.String())
		}
	}
	return rec.Code, out
}

func creditData(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	data, ok := out["data"].(map[string]any)
	if !ok {
		t.Fatalf("response missing data object: %+v", out)
	}
	return data
}

func creditNum(t *testing.T, data map[string]any, key string) float64 {
	t.Helper()
	v, ok := data[key].(float64)
	if !ok {
		t.Fatalf("data.%s missing/not number: %+v", key, data)
	}
	return v
}

func mustCreateCreditKey(t *testing.T, ks *keystore.Store, p keystore.CreateParams) (string, keystore.Key) {
	t.Helper()
	plain, k, err := ks.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	return plain, k
}

func TestUsageTokenPoolCreditsAndScopes(t *testing.T) {
	h, ks := creditTestHandler(t)
	plain, _ := mustCreateCreditKey(t, ks, keystore.CreateParams{Name: "cn-unlimited", Realm: "cn"})

	code, out := getCreditJSON(t, h, "/api/usage/token/", plain)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%+v", code, out)
	}
	data := creditData(t, out)
	if got := creditNum(t, data, "total_available"); got != 150 {
		t.Errorf("cn key total_available=%v want 150 (banned/manual excluded)", got)
	}
	if got := data["scope"]; got != "cn" {
		t.Errorf("scope=%v want cn", got)
	}
	if got := data["unlimited_quota"]; got != false {
		t.Errorf("unlimited_quota=%v want false", got)
	}

	code, out = getCreditJSON(t, h, "/api/usage/token/?scope=global", plain)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%+v", code, out)
	}
	data = creditData(t, out)
	if got := creditNum(t, data, "total_available"); got != 200 {
		t.Errorf("global scope total_available=%v want 200", got)
	}

	code, out = getCreditJSON(t, h, "/api/usage/token/?scope=merged", plain)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%+v", code, out)
	}
	if got := creditNum(t, creditData(t, out), "total_available"); got != 350 {
		t.Errorf("merged total_available=%v want 350", got)
	}

	// 全局 api_key 是管理身份：无密钥限额，报全池可用积分。
	_, out = getCreditJSON(t, h, "/api/usage/token/", "admin-key")
	if got := creditNum(t, creditData(t, out), "total_available"); got != 350 {
		t.Errorf("global api_key total_available=%v want 350", got)
	}
}

func TestUsageTokenKeyQuotas(t *testing.T) {
	h, ks := creditTestHandler(t)

	creditPlain, creditKey := mustCreateCreditKey(t, ks, keystore.CreateParams{
		Name: "credit-quota", CreditQuota: 100,
	})
	ks.RecordUse(&creditKey, "10.0.0.1", 0, 30)
	_, out := getCreditJSON(t, h, "/api/usage/token/", creditPlain)
	data := creditData(t, out)
	if creditNum(t, data, "total_granted") != 100 || creditNum(t, data, "total_used") != 30 ||
		creditNum(t, data, "total_available") != 70 {
		t.Errorf("credit quota amounts=%+v want 100/30/70", data)
	}
	if data["scope"] != "merged" {
		t.Errorf("quota key scope=%v want merged", data["scope"])
	}

	tokenPlain, tokenKey := mustCreateCreditKey(t, ks, keystore.CreateParams{
		Name: "token-quota", TokenQuota: 2_000_000,
	})
	ks.RecordUse(&tokenKey, "10.0.0.1", 500_000, 0)
	_, out = getCreditJSON(t, h, "/api/usage/token/", tokenPlain)
	data = creditData(t, out)
	if creditNum(t, data, "total_granted") != 2 || creditNum(t, data, "total_used") != 0.5 ||
		creditNum(t, data, "total_available") != 1.5 {
		t.Errorf("token quota amounts=%+v want 2/0.5/1.5", data)
	}

	bothPlain, bothKey := mustCreateCreditKey(t, ks, keystore.CreateParams{
		Name: "both", CreditQuota: 100, TokenQuota: 1_000_000,
	})
	ks.RecordUse(&bothKey, "10.0.0.1", 200_000, 90)
	_, out = getCreditJSON(t, h, "/api/usage/token/", bothPlain)
	data = creditData(t, out)
	if creditNum(t, data, "total_granted") != 100 || creditNum(t, data, "total_used") != 90 ||
		creditNum(t, data, "total_available") != 10 {
		t.Errorf("both quotas amounts=%+v want 100/90/10 (tighter credit ratio)", data)
	}
}

func TestCreditsEndpointMirrorsTotalAvailable(t *testing.T) {
	h, ks := creditTestHandler(t)
	plain, key := mustCreateCreditKey(t, ks, keystore.CreateParams{CreditQuota: 100})
	ks.RecordUse(&key, "10.0.0.1", 0, 25)

	_, out := getCreditJSON(t, h, "/v1/credits", plain)
	data := creditData(t, out)
	if data["object"] != "credits" {
		t.Errorf("object=%v want credits", data["object"])
	}
	if creditNum(t, data, "total_available") != 75 || creditNum(t, data, "total_usage") != 75 {
		t.Errorf("credits=%+v want total_available=total_usage=75", data)
	}
}

func TestCreditsEndpointAllowsDisabledKey(t *testing.T) {
	h, ks := creditTestHandler(t)
	plain, key := mustCreateCreditKey(t, ks, keystore.CreateParams{CreditQuota: 100})
	if err := ks.Update(key.ID, func(k *keystore.Key) { k.Enabled = false }); err != nil {
		t.Fatal(err)
	}
	code, out := getCreditJSON(t, h, "/api/usage/token/", plain)
	if code != http.StatusOK {
		t.Fatalf("disabled key must still read balance, status=%d body=%+v", code, out)
	}
	if got := creditNum(t, creditData(t, out), "total_available"); got != 100 {
		t.Errorf("total_available=%v want 100", got)
	}
}

func TestCreditsEndpointRejectsInvalidKey(t *testing.T) {
	h, _ := creditTestHandler(t)
	code, out := getCreditJSON(t, h, "/api/usage/token/", "wbk_not-a-real-key")
	if code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401 body=%+v", code, out)
	}
	errObj, _ := out["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "invalid_api_key" {
		t.Errorf("error=%+v want code=invalid_api_key", out["error"])
	}
}

func TestBalanceStatusIsPublicMetadata(t *testing.T) {
	h, _ := creditTestHandler(t)
	code, out := getCreditJSON(t, h, "/api/status", "")
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%+v", code, out)
	}
	data := creditData(t, out)
	if data["quota_display_type"] != "CUSTOM" || data["custom_currency_symbol"] != "UNIT" {
		t.Errorf("status metadata=%+v", data)
	}
	if _, ok := data["total_available"]; ok {
		t.Error("public status must not expose any quota")
	}
}

// 余额端点（含公开元数据）必须遵守入站 IP 规则：body 无额度也不能绕过运维 ACL，
// 且三个端点同口径（review medium）。
func TestBalanceRoutesHonorIPRules(t *testing.T) {
	h, _ := creditTestHandler(t)
	rules, _ := NewIPRules("", IPRulesSpec{Blacklist: []string{"192.0.2.0/24"}})
	h.cfg.IPRules = rules
	for _, path := range []string{"/api/status", "/v1/credits", "/api/usage/token/"} {
		code, out := getCreditJSON(t, h, path, "admin-key")
		if code != http.StatusForbidden {
			t.Errorf("%s status=%d want 403 body=%+v", path, code, out)
			continue
		}
		errObj, _ := out["error"].(map[string]any)
		if errObj == nil || errObj["code"] != "ip_blocked" {
			t.Errorf("%s error=%+v want code=ip_blocked", path, out["error"])
		}
	}
}

// 单配额分支只对一个字段取整、其余派生：三档数字必须自洽（review low）。
// 修复前 2_000_000 token / 已用 5_000 会给出 total=2.00、used=0.01、available=2.00。
func TestTokenQuotaAmountsStayConsistent(t *testing.T) {
	h, ks := creditTestHandler(t)
	plain, key := mustCreateCreditKey(t, ks, keystore.CreateParams{TokenQuota: 2_000_000})
	ks.RecordUse(&key, "10.0.0.1", 5_000, 0)

	_, out := getCreditJSON(t, h, "/api/usage/token/", plain)
	data := creditData(t, out)
	total := creditNum(t, data, "total_granted")
	used := creditNum(t, data, "total_used")
	available := creditNum(t, data, "total_available")
	if math.Abs(total-(used+available)) > 1e-9 {
		t.Errorf("total_granted=%v != total_used(%v)+total_available(%v)", total, used, available)
	}
}

// 显式余额路由必须优先于面板的 /api/ 前缀挂载（ServeMux 最长 pattern 匹配）。
func TestCreditsRoutesPrecedePanelMount(t *testing.T) {
	panel := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := NewHandler(Config{Pool: creditTestPool(), APIKey: "admin-key", Panel: panel})

	code, out := getCreditJSON(t, h, "/api/status", "")
	if code != http.StatusOK {
		t.Fatalf("/api/status status=%d body=%+v want 200 (not panel)", code, out)
	}
	code, out = getCreditJSON(t, h, "/api/usage/token/", "admin-key")
	if code != http.StatusOK {
		t.Fatalf("/api/usage/token/ status=%d body=%+v want 200 (not panel)", code, out)
	}
}
