package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 管理端探测成功回执只清该账号该模型的 11102 负缓存，不碰 6004 配额与手动停用
// （manager 0039）。
func TestSuccessfulProbeClearsOnlyMissingModel(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	p.BlockModelBackoff("u1", "deepseek-v4.1-flash", "11102 model not available")
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.3", "6004 model rate limit")
	p.SetManualDisabled("u1", true, "operator")
	h := NewHandler(Config{Pool: p, APIKey: "test", Admin: AdminConfig{Enabled: true}})
	url := "/admin/accounts/u1/models/deepseek-v4.1-flash/available"
	request := httptest.NewRequest("POST", url, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("recovery requires gateway key: %d", rec.Code)
	}
	request = httptest.NewRequest("POST", url, nil)
	request.Header.Set("Authorization", "Bearer test")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, request)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	st, _ := p.Status("u1")
	if !st.ManualDisabled || len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Fatalf("must preserve real quota and operator state: %+v", st)
	}
}

// 小请求成功不能证明真实 6004 token 配额已恢复：同一模型的 6004 冷却必须保留。
func TestProbeRecoveryDoesNotClearSameModelQuota(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "deepseek-v4.1-flash", "6004 model rate limit")
	h := NewHandler(Config{Pool: p, Admin: AdminConfig{Enabled: true}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/accounts/u1/models/deepseek-v4.1-flash/available", nil))
	st, _ := p.Status("u1")
	if rec.Code != http.StatusOK || len(st.RateLimitedModels) != 1 {
		t.Fatalf("successful small probe must not erase token quota: %d %+v", rec.Code, st)
	}
}
