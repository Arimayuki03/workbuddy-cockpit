// handler_audit_test.go audit 批修复的回归：
//   - 413 请求体上限（MaxBytesReader，config max_body_mb）
//   - api_key / soft_rate 经 livecfg 快照热改（面板改后免重启生效）
//   - 入站 X-Conversation-Request-ID / X-Trace-ID 白名单校验（非法值丢弃）
//   - /api/system/check-update force 最小间隔
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/livecfg"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// capturedHeaders fake upstream：返回 sseOK 并记录最后一次请求的指定头。
func newCapturingUpstream(t *testing.T, capture func(r *http.Request)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Body != nil {
				_, _ = io.ReadAll(r.Body)
			}
			capture(r)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// TestChatBodyTooLargeReturns413 max_body_mb 落地：超限 body 就地 413、不打上游、不罚号。
func TestChatBodyTooLargeReturns413(t *testing.T) {
	var calls int
	up := newCapturingUpstream(t, func(r *http.Request) { calls++ })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxBodyBytes: 1024})

	over := `{"model":"glm-5.2","messages":[],"pad":"` + strings.Repeat("a", 2048) + `"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(over)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d want 413 body=%s", rec.Code, rec.Body)
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if env.Error.Code != "request_too_large" {
		t.Errorf("code=%q want request_too_large", env.Error.Code)
	}
	if calls != 0 {
		t.Errorf("oversized body must not reach upstream, calls=%d", calls)
	}
	if st, _ := p.Status("u1"); st.Cooling || st.Disabled {
		t.Error("413 must not penalize account")
	}

	// 上限内照常进上游。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 || calls != 1 {
		t.Errorf("within limit: code=%d calls=%d want 200/1", rec.Code, calls)
	}

	// 其余读错误（断流）保持 400 路径。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", errReader{err: io.ErrUnexpectedEOF}))
	if rec.Code != 400 {
		t.Errorf("read error: code=%d want 400", rec.Code)
	}
}

// TestWithAuthHotReloadAPIKey 面板热改 api_key → 下一个请求用新值（免重启）。
func TestWithAuthHotReloadAPIKey(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{APIKey: "old-key"})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool:     p,
		Upstream: newCapturingUpstream(t, func(r *http.Request) {}),
		APIKey:   "old-key", // 静态字段是启动快照；运行期以 Live 为准
		Live:     live,
	})

	get := func(key string) int {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := get("old-key"); got != 200 {
		t.Fatalf("old key: code=%d want 200", got)
	}
	// 面板保存配置 → live.Store（saveConfig 同款）。
	live.Store(livecfg.Snapshot{APIKey: "new-key", SoftCooldown: 300 * time.Second})
	if got := get("old-key"); got != 401 {
		t.Errorf("old key after hot change: code=%d want 401", got)
	}
	if got := get("new-key"); got != 200 {
		t.Errorf("new key after hot change: code=%d want 200", got)
	}
	// 会话 cookie 通道同走 live：SessionKey 供应商闭包内经 live.Load() 读密钥
	// （main.go sessionKeySupplier 同款），改 key 后旧 cookie 立即失效。
	if h.cfg.SessionKey != nil && h.cfg.SessionKey() != "new-key" {
		t.Errorf("SessionKey()=%q want new-key (live snapshot)", h.cfg.SessionKey())
	}
}

// TestSoftCooldownHotReload 面板热改 soft_rate → applyErrorPolicy 用新基数。
func TestSoftCooldownHotReload(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{APIKey: "k", SoftCooldown: 600 * time.Second})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), SoftCooldown: 600 * time.Second, Live: live})
	if got := h.softCooldown(); got != 600*time.Second {
		t.Fatalf("initial softCooldown=%v want 600s", got)
	}
	live.Store(livecfg.Snapshot{APIKey: "k", SoftCooldown: 60 * time.Second})
	if got := h.softCooldown(); got != 60*time.Second {
		t.Errorf("hot softCooldown=%v want 60s", got)
	}
	// 无 Holder 回落静态字段（测试形态）。
	h2 := NewHandler(Config{Pool: p, Upstream: upstream.New(), SoftCooldown: 600 * time.Second})
	if got := h2.softCooldown(); got != 600*time.Second {
		t.Errorf("static fallback softCooldown=%v want 600s", got)
	}
}

// TestInboundHeaderSanitization 入站可控头校验：非法 X-Conversation-Request-ID /
// X-Trace-ID 被丢弃（conversationRequestID 回落服务端派生，TraceID 回落
// convReqID），合法值透传。
func TestInboundHeaderSanitization(t *testing.T) {
	var mu sync.Mutex
	var gotConvReqID, gotTraceID string
	up := newCapturingUpstream(t, func(r *http.Request) {
		mu.Lock()
		gotConvReqID = r.Header.Get("X-Conversation-Request-ID")
		gotTraceID = r.Header.Get("X-Trace-ID")
		mu.Unlock()
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	chat := func(headers map[string]string) {
		t.Helper()
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
	}

	// 非法值（含换行的控制字符形态 → 白名单外）：丢弃，回落服务端派生 32 hex。
	chat(map[string]string{
		"X-Conversation-Request-ID": "bad\rvalue",
		"X-Trace-ID":                "not*allowed*chars!",
	})
	mu.Lock()
	if gotConvReqID == "bad\rvalue" || len(gotConvReqID) != 32 {
		t.Errorf("invalid X-Conversation-Request-ID must be dropped, got %q (len %d)", gotConvReqID, len(gotConvReqID))
	}
	if gotTraceID == "not*allowed*chars!" || len(gotTraceID) != 32 {
		t.Errorf("invalid X-Trace-ID must be dropped, got %q (len %d)", gotTraceID, len(gotTraceID))
	}
	mu.Unlock()

	// 合法值：原样透传。
	chat(map[string]string{
		"X-Conversation-Request-ID": "client-req-1",
		"X-Trace-ID":                "client-trace-1",
	})
	mu.Lock()
	if gotConvReqID != "client-req-1" {
		t.Errorf("valid X-Conversation-Request-ID passthrough broken: %q", gotConvReqID)
	}
	if gotTraceID != "client-trace-1" {
		t.Errorf("valid X-Trace-ID passthrough broken: %q", gotTraceID)
	}
	mu.Unlock()
}

// TestChatUsageBucketGlobalRealm global 账号用量记入 global 桶（realmOf 分桶）。
func TestChatUsageBucketGlobalRealm(t *testing.T) {
	// 用 chatStat 直测（done() 的分桶路径），不依赖完整 e2e。
	st := newChatStat(time.Now(), session.ParseRequest([]byte(`{"model":"global:glm-5.3"}`)), "global")
	st.realm = "global" // handler 选号处覆盖为账号 Realm()
	st.uid = "g1"
	st.status = 200
	st.toks = 10
	st.hasUsage = true
	st.prompt = 5
	if got := st.realmOf(); got != "global" {
		t.Errorf("realmOf=%q want global", got)
	}
	// CN 缺省。
	st2 := newChatStat(time.Now(), session.ParseRequest([]byte(`{"model":"glm-5.2"}`)), "cn")
	if got := st2.realmOf(); got != "cn" {
		t.Errorf("realmOf=%q want cn", got)
	}
	// 空域（历史数据/直构造）回落 cn。
	st3 := &chatStat{}
	if got := st3.realmOf(); got != "cn" {
		t.Errorf("empty realmOf=%q want cn", got)
	}
}

// countingTransport 包装 http.DefaultTransport 统计外发请求数（versionHTTPClient
// 是包级可变变量，可直接替换 client；upstreamReleaseAPI 是 const 不可换，用
// 归位缓存时间控制首次拉取——首次真实外发打 GitHub 会在无网环境失败并走回退
// 路径，但 fail 也会写 lastError 而不写 fetched……因此改测纯时间门语义：
// 预置缓存（fetched=now），force 在间隔内必须直接回缓存、零外发。
func TestCheckUpdateForceMinInterval(t *testing.T) {
	var mu sync.Mutex
	var calls int
	prev := versionHTTPClient
	versionHTTPClient = &http.Client{Timeout: 5 * time.Second, Transport: countingRoundTrip{fn: func(r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
	}}}
	t.Cleanup(func() { versionHTTPClient = prev })

	versionCheck.mu.Lock()
	versionCheck.snapshot = UpdateCheck{Current: "v1.0.0", Latest: "v9.9.9", ChangelogURL: "https://example.invalid/releases/latest"}
	versionCheck.fetched = time.Now() // 刚刚检查过：60s 时间门生效中
	versionCheck.lastError = ""
	versionCheck.mu.Unlock()
	t.Cleanup(func() {
		versionCheck.mu.Lock()
		versionCheck.snapshot = UpdateCheck{}
		versionCheck.fetched = time.Time{}
		versionCheck.mu.Unlock()
	})

	h := &Handler{}
	rec := httptest.NewRecorder()
	h.handleCheckUpdate(rec, httptest.NewRequest("GET", "/api/system/check-update?force=true", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var out UpdateCheck
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if out.Latest != "v9.9.9" {
		t.Errorf("force within min interval must return cached value: %+v", out)
	}
	if !out.Cached {
		t.Errorf("force within min interval should be marked cached: %+v", out)
	}
	mu.Lock()
	if calls != 0 {
		t.Errorf("force within %v must not hit upstream, calls=%d", versionForceMinInterval, calls)
	}
	mu.Unlock()
}

type countingRoundTrip struct {
	fn func(r *http.Request)
}

func (c countingRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	c.fn(r)
	return http.DefaultTransport.RoundTrip(r)
}
