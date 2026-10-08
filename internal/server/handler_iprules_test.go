// handler_iprules_test.go 入站 IP 规则接线（withAuth 最前）与 /status 新字段
// （credit_floor / model_locks）的最小接线测试。规则引擎本体语义见 iprules_test.go，
// 本文件只验证 Handler 侧接线：nil 引擎恒放行（零配置零回归）、黑名单拦截 403、
// /status 透出新字段（有配置时）与零配置省略。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// TestWithAuthNilIPRulesAllows nil IPRules（未配置 security 段）时 withAuth 恒放行：
// 无规则请求照常进入鉴权与业务（零配置零回归）。
func TestWithAuthNilIPRulesAllows(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("nil IPRules must not block requests, code=%d body=%s", rec.Code, rec.Body)
	}
	// 请求未记入拦截日志（nil 引擎 NoteBlocked 也是 no-op，防 panic 即可）。
	if logs := h.cfg.IPRules.BlockedLogs(); len(logs) != 0 {
		t.Fatalf("nil IPRules must have no block logs, got %d", len(logs))
	}
}

// TestWithAuthIPRulesBlocksWith403 黑名单命中 → 403 OpenAI 错误体（鉴权之前拦截，
// 无密钥也拦——规则先于鉴权执行），且拦截进入 NoteBlocked 环形日志。
func TestWithAuthIPRulesBlocksWith403(t *testing.T) {
	rules, errs := NewIPRules("", IPRulesSpec{Blacklist: []string{"192.0.2.0/24"}})
	if len(errs) > 0 {
		t.Fatalf("invalid spec: %v", errs)
	}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		IPRules:  rules,
	})
	// httptest.NewRequest 默认 RemoteAddr="192.0.2.1:1234"，恰在黑名单网段内。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("blacklisted IP must be blocked with 403, code=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "ip_blocked" {
		t.Fatalf("want OpenAI error envelope code=ip_blocked, got %s (err=%v)", rec.Body, err)
	}
	logs := h.cfg.IPRules.BlockedLogs()
	if len(logs) != 1 || logs[0].IP != "192.0.2.1" || logs[0].Reason != ReasonIPBlocked || logs[0].Path != "/status" {
		t.Fatalf("block log entry mismatch: %+v", logs)
	}
	// 非黑名单来源照常放行（200，/status 无鉴权配置直通业务）。
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/status", nil)
	req2.RemoteAddr = "203.0.113.7:4444"
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("non-blacklisted IP must pass, code=%d body=%s", rec2.Code, rec2.Body)
	}
}

// TestStatusCreditFloorAndModelLocks /status 新字段接线：配置 credit_floor 后透出
// 生效值；模型级冷却存在时透出 model_locks；零配置（floor=0、无锁）两者省略。
func TestStatusCreditFloorAndModelLocks(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCreditFloor(500)
	// 制造一条未过期模型级冷却（6004 语义），ModelLockView 应产生一行。
	p.CooldownSoftForModel("cn1", time.Hour, time.Now().Add(time.Hour), "gpt-x", "6004 model rate limit")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		CreditFloor int64 `json:"credit_floor"`
		ModelLocks  []struct {
			Realm string `json:"realm"`
			Model string `json:"model"`
			State string `json:"state"`
		} `json:"model_locks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status not json: %v body=%s", err, rec.Body)
	}
	if body.CreditFloor != 500 {
		t.Errorf("credit_floor=%d want 500", body.CreditFloor)
	}
	if len(body.ModelLocks) != 1 {
		t.Fatalf("model_locks rows=%d want 1, body=%s", len(body.ModelLocks), rec.Body)
	}
	if body.ModelLocks[0].Model != "gpt-x" || body.ModelLocks[0].State != "locked" {
		t.Errorf("model_locks[0]=%+v want model=gpt-x state=locked", body.ModelLocks[0])
	}
}

// TestStatusZeroConfigOmitsNewFields 零配置回归：credit_floor=0（关）与无模型锁时，
// /status 不透出这两个键（与 v1.15.1 字段集一致，只增不删）。
func TestStatusZeroConfigOmitsNewFields(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if _, ok := raw["credit_floor"]; ok {
		t.Error("credit_floor must be omitted when floor=0 (zero-config regression)")
	}
	if _, ok := raw["model_locks"]; ok {
		t.Error("model_locks must be omitted when no model cooldowns (zero-config regression)")
	}
}
