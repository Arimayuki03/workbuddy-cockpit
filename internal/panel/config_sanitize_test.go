// config_sanitize_test.go GET /api/config 敏感值脱敏 + saveConfig 脱敏标记剔除
// 的回归测试。链条：LoadConfig 返回 *Config 结构体 → JSON 往返成 map →
// sanitizeConfigCopy 注入 has_token/token_masked/has_key 标记（明文不透出）；
// 提交侧 stripSensitiveMarkers 把回传的标记剔除（深合并不会被标记键污染）。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sanitizeFixture 形如 cmd/server Config 序列化后的片段：顶层 api_key +
// 嵌套 upstash.token。
var sanitizeFixture = map[string]any{
	"api_key": "sk-abcdef123456",
	"upstash": map[string]any{
		"url":   "https://xxx.upstash.io",
		"token": "tok-9876543210",
	},
	"listen": ":8080",
}

func TestSanitizeConfigCopyMasksSecrets(t *testing.T) {
	out := sanitizeConfigCopy(sanitizeFixture).(map[string]any)

	// 明文绝不透出。
	if strings.Contains(anyJSON(t, out), "sk-abcdef123456") {
		t.Error("api_key plaintext leaked in sanitized config")
	}
	if strings.Contains(anyJSON(t, out), "tok-9876543210") {
		t.Error("upstash token plaintext leaked in sanitized config")
	}
	// 标记就位：has_* + *_masked（保留末 4 位）。
	if v, _ := out["has_key"].(bool); !v {
		t.Errorf("has_key = %v, want true", out["has_key"])
	}
	if got, _ := out["key_masked"].(string); got != "****3456" {
		t.Errorf("key_masked = %q, want ****3456", got)
	}
	up := out["upstash"].(map[string]any)
	if v, _ := up["has_token"].(bool); !v {
		t.Errorf("upstash.has_token = %v, want true", up["has_token"])
	}
	if got, _ := up["token_masked"].(string); got != "****3210" {
		t.Errorf("upstash.token_masked = %q, want ****3210", got)
	}
	if up["url"] != "https://xxx.upstash.io" {
		t.Errorf("non-sensitive sibling url = %v, want unchanged", up["url"])
	}
	// 空值敏感键不注入标记（避免 has_token:true 误导）。
	empty := sanitizeConfigCopy(map[string]any{"token": ""})
	if _, ok := empty.(map[string]any)["has_token"]; ok {
		t.Error("empty token must not produce has_token marker")
	}
	// 原入参不被改写（只读展示语义）。
	if sanitizeFixture["api_key"] != "sk-abcdef123456" {
		t.Error("sanitizeConfigCopy must not mutate its input")
	}
}

func TestStripSensitiveMarkers(t *testing.T) {
	// 模拟高级模式：把 GET 回显的 JSON 原样改两个字段后整体回传。
	raw := []byte(`{
		"api_key": "sk-new-value",
		"has_key": true,
		"key_masked": "****3456",
		"upstash": {
			"url": "https://yyy.upstash.io",
			"has_token": true,
			"token_masked": "****3210"
		},
		"listen": ":9090"
	}`)
	stripped, changed, err := stripSensitiveMarkers(raw)
	if err != nil || !changed {
		t.Fatalf("strip: changed=%v err=%v", changed, err)
	}
	var m map[string]any
	if err := json.Unmarshal(stripped, &m); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"has_key", "key_masked"} {
		if _, ok := m[bad]; ok {
			t.Errorf("top-level marker %q survived strip", bad)
		}
	}
	up := m["upstash"].(map[string]any)
	for _, bad := range []string{"has_token", "token_masked"} {
		if _, ok := up[bad]; ok {
			t.Errorf("upstash marker %q survived strip", bad)
		}
	}
	// 合法键原样保留（深合并语义不受影响）。
	if m["api_key"] != "sk-new-value" || up["url"] != "https://yyy.upstash.io" || m["listen"] != ":9090" {
		t.Fatalf("legit keys altered: %v", m)
	}

	// 无标记的普通提交走快路径（原字节返回，changed=false）。
	plain := []byte(`{"listen":":8080","upstash":{"url":"https://x.io"}}`)
	got, changed2, err := stripSensitiveMarkers(plain)
	if err != nil || changed2 || string(got) != string(plain) {
		t.Fatalf("plain passthrough: changed=%v err=%v got=%s", changed2, err, got)
	}
}

func TestStripMarkersWalkNested(t *testing.T) {
	// 数组内对象与任意嵌套层的标记都要被剔掉。
	v := map[string]any{
		"pool": []any{
			map[string]any{"has_token": true, "keep": 1},
		},
		"features": map[string]any{
			"deep": map[string]any{"token_masked": "****", "ok": true},
		},
	}
	cleaned, changed := stripMarkersWalk(v)
	if !changed {
		t.Fatal("expected markers found")
	}
	if anyJSON(t, cleaned) != `{"features":{"deep":{"ok":true}},"pool":[{"keep":1}]}` {
		t.Fatalf("nested markers not stripped: %v", cleaned)
	}
}

// TestGetConfigSanitizesStructLoadedConfig 端到端：LoadConfig 返回 *Config
// 结构体（生产形态）时，GET /api/config 也不得透出明文——结构体必须先经
// JSON 往返成 map 才进 sanitizeConfigCopy。
func TestGetConfigSanitizesStructLoadedConfig(t *testing.T) {
	type fakeConfig struct {
		APIKey  string `json:"api_key"`
		Upstash struct {
			URL   string `json:"url"`
			Token string `json:"token"`
		} `json:"upstash"`
	}
	p := New(Config{
		Version: "test",
		LoadConfig: func() (any, error) {
			return &fakeConfig{
				APIKey: "sk-live-9999",
				Upstash: struct {
					URL   string `json:"url"`
					Token string `json:"token"`
				}{URL: "https://x.upstash.io", Token: "super-secret-token"},
			}, nil
		},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/config", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/config: code=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "sk-live-9999") || strings.Contains(body, "super-secret-token") {
		t.Fatalf("plaintext secret leaked in /api/config: %s", body)
	}
	var resp struct {
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Config["has_key"] != true {
		t.Errorf("has_key = %v, want true", resp.Config["has_key"])
	}
	up, _ := resp.Config["upstash"].(map[string]any)
	if up == nil || up["has_token"] != true || up["token_masked"] != "****oken" {
		t.Fatalf("upstash sanitize markers wrong: %v", resp.Config["upstash"])
	}
}

func anyJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
