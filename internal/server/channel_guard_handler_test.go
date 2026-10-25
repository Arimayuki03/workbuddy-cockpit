package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// TestChannelGuardBlocksRepeatedChannelCheck 首次 11-128 抓取并短冷却；同 body /
// 同模板的后续请求在网关本地 400，不再打到上游，也不再 503。
func TestChannelGuardBlocksRepeatedChannelCheck(t *testing.T) {
	var calls atomic.Int32
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return testSSEResponse(http.StatusBadRequest, channelCheckServerBody()), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	dir := t.TempDir()
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		SoftCooldown: 600 * time.Second,
		MaxRotate:    3,
		ChannelGuard: ChannelGuardConfig{
			Enabled:         true,
			Mode:            "block",
			CaptureDir:      dir,
			CaptureMaxBytes: 4 << 20,
			BlockTTL:        time.Hour,
			TemplateTTL:     time.Hour,
			ChannelCooldown: 30 * time.Second,
			ScanMessages:    true,
			FailFast:        true,
		},
	})

	body := func(system, user string) string {
		return `{"model":"glm-5.2","stream":true,"messages":[` +
			`{"role":"system","content":"` + system + `"},` +
			`{"role":"user","content":"` + user + `"}]}`
	}
	do := func(raw string) *http.Response {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(raw)))
		return rec.Result()
	}

	// 第一次：上游渠道校验，fail_fast 立即 400；请求体被抓取。
	resp := do(body("template-A", "hi"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("first request code=%d want 400", resp.StatusCode)
	}
	if calls.Load() != 1 {
		t.Fatalf("first request upstream calls=%d want 1 (fail_fast)", calls.Load())
	}
	files, _ := filepath.Glob(filepath.Join(dir, channelGuardCapturePrefix+"*.json"))
	if len(files) == 0 {
		t.Fatal("expected a captured request body")
	}
	captured, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(captured), "template-A") {
		t.Errorf("capture should contain the offending template: %s", captured)
	}

	// 同一 body 重试：本地拦截，不再打上游。
	resp = do(body("template-A", "hi"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("repeat code=%d want 400", resp.StatusCode)
	}
	if calls.Load() != 1 {
		t.Fatalf("repeat must be blocked locally, upstream calls=%d", calls.Load())
	}
	// 同模板不同用户消息：模板命中，同样本地拦截。
	resp = do(body("template-A", "different question"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("template repeat code=%d want 400", resp.StatusCode)
	}
	if calls.Load() != 1 {
		t.Fatalf("template repeat must be blocked locally, upstream calls=%d", calls.Load())
	}

	// 不同模板应放行到上游（清掉首号冷却，避免选号阶段提前 503）。
	p.ForceClearCooldown("u1")
	resp = do(body("template-B", "hi"))
	if calls.Load() != 2 {
		t.Fatalf("different template should reach upstream, calls=%d want 2", calls.Load())
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("different template code=%d want 400 (upstream channel check)", resp.StatusCode)
	}
}

// TestChannelCheckEndReturns400Not503 轮转耗尽后渠道校验回 400 channel_check_blocked，
// 不再伪装成 503 no_healthy_account。
func TestChannelCheckEndReturns400Not503(t *testing.T) {
	var calls atomic.Int32
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return testSSEResponse(http.StatusBadRequest, channelCheckServerBody()), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: 600 * time.Second, MaxRotate: 3})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"channel_check_blocked"`) {
		t.Errorf("want channel_check_blocked code: %s", rec.Body.String())
	}
	if calls.Load() != 3 {
		t.Errorf("upstream calls=%d want 3 (MaxRotate)", calls.Load())
	}
}

// TestChannelCheckUsesShortCooldown 渠道校验用 channel_check_cooldown，而不是
// cooldown.soft_rate（示例配置 6h），避免一个坏模板把健康号停半天。
func TestChannelCheckUsesShortCooldown(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return testSSEResponse(http.StatusBadRequest, channelCheckServerBody()), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		SoftCooldown: 6 * time.Hour,
		ChannelGuard: ChannelGuardConfig{
			Enabled:         true,
			Mode:            "block",
			CaptureDir:      t.TempDir(),
			BlockTTL:        time.Minute,
			TemplateTTL:     time.Minute,
			ChannelCooldown: 45 * time.Second,
			FailFast:        true,
		},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400", rec.Code, rec.Body.String())
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("account missing")
	}
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("channel check must soft-cool, got %+v", st)
	}
	if remain := time.Until(st.Until); remain > 90*time.Second {
		t.Fatalf("channel-check cooldown too long: %s (want ~45s, not 6h)", remain)
	}
	if !strings.Contains(st.Reason, "channel check") {
		t.Errorf("reason should identify channel check, got %q", st.Reason)
	}
}
