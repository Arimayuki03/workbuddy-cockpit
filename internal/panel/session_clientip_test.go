// session_clientip_test.go clientIP 换轨 iputil 的行为契约测试：
//   - 零配置（无可信代理网段）时伪造 X-Real-IP / X-Forwarded-For 无效，
//     clientIP 恒取 RemoteAddr（防伪造头绕过登录限速）；
//   - 配置可信代理（回环网段）后，来自可信对端的请求按转发头取真实客户端 IP；
//   - 登录失败限速按解析后的真实 IP 计数：反代后不同真实来源互不干扰，
//     不再因共享代理地址误锁全体。
//
// 限定口径：loopback_only 安全闸不参与本测试——它恒按 RemoteAddr 判定
// （panel.go loopbackOnly 注释），与真实客户端识别是两条独立路径。
package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/iputil"
)

// loginPost 构造带伪造转发头的登录请求（RemoteAddr 由 httptest 自定）。
func loginPost(t *testing.T, remote string, headers map[string]string) *http.Request {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"password":"wrong"}`))
	req.RemoteAddr = remote
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

// resetLoginLimiter 清空包级限速表（测试间隔离，避免其他用例的失败计数串扰）。
func resetLoginLimiter() {
	loginRateLimit.mu.Lock()
	defer loginRateLimit.mu.Unlock()
	loginRateLimit.entries = map[string]*loginFailEntry{}
}

// loopbackTrusted 本机反代形态的标准可信网段（nginx/caddy 同机）。
var loopbackTrusted = iputil.LoopbackCIDRs()

// TestClientIP_NoTrustedProxy_IgnoresSpoofedHeaders 零配置安全默认：
// trustedCIDRs 为 nil 时转发头完全不参与解析，恒取 RemoteAddr。
func TestClientIP_NoTrustedProxy_IgnoresSpoofedHeaders(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k"})
	req := loginPost(t, "203.0.113.77:5555", map[string]string{
		"X-Real-IP":       "9.9.9.9",
		"X-Forwarded-For": "9.9.9.9, 8.8.8.8",
	})
	if got := p.clientIP(req); got != "203.0.113.77" {
		t.Errorf("clientIP with spoofed headers = %q, want RemoteAddr 203.0.113.77", got)
	}
}

// TestClientIP_TrustedProxy_ResolvesRealIP 可信对端（回环）+ 转发头：
// X-Real-IP 优先；仅 XFF 时从右往左取第 hops 跳；直连（头缺失）回落对端。
func TestClientIP_TrustedProxy_ResolvesRealIP(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", TrustedProxyCIDRs: loopbackTrusted, TrustedProxyHops: 1})

	cases := []struct {
		name    string
		remote  string
		headers map[string]string
		want    string
	}{
		{"x-real-ip", "127.0.0.1:1000", map[string]string{"X-Real-IP": "198.51.100.7"}, "198.51.100.7"},
		{"xff-hop1", "127.0.0.1:1000", map[string]string{"X-Forwarded-For": "9.9.9.9, 198.51.100.7"}, "198.51.100.7"},
		{"no-headers", "127.0.0.1:1000", nil, "127.0.0.1"},
	}
	for _, tc := range cases {
		if got := p.clientIP(loginPost(t, tc.remote, tc.headers)); got != tc.want {
			t.Errorf("%s: clientIP = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestClientIP_UntrustedPeer_IgnoresHeaders 即便配置了可信网段，对端不在网段内
// （直连外网客户端）时转发头依然无效——信任模型的关键边界。
func TestClientIP_UntrustedPeer_IgnoresHeaders(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", TrustedProxyCIDRs: loopbackTrusted, TrustedProxyHops: 1})
	req := loginPost(t, "203.0.113.5:443", map[string]string{"X-Real-IP": "9.9.9.9"})
	if got := p.clientIP(req); got != "203.0.113.5" {
		t.Errorf("untrusted peer clientIP = %q, want RemoteAddr 203.0.113.5", got)
	}
}

// TestLoginRateLimitByRealIP 登录限速按解析后的真实 IP 计数（httptest 全链路）：
// 反代（127.0.0.1）后面两个不同真实来源分别计数互不干扰；同一真实来源达到
// 阈值后锁定，而其他来源不受影响（修复"共享代理 IP 误锁全体"）。
func TestLoginRateLimitByRealIP(t *testing.T) {
	resetLoginLimiter()
	t.Cleanup(resetLoginLimiter)
	p := New(Config{Version: "test", APIKey: "k", TrustedProxyCIDRs: loopbackTrusted, TrustedProxyHops: 1})

	login := func(realIP string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := loginPost(t, "127.0.0.1:2000", map[string]string{"X-Real-IP": realIP})
		p.ServeHTTP(rec, req)
		return rec
	}

	// 真实来源 A（198.51.100.1）：打满 loginFailMax 次失败 → 第 loginFailMax+1 次 429。
	for i := 0; i < loginFailMax; i++ {
		if rec := login("198.51.100.1"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure #%d for real IP A: code=%d want 401", i+1, rec.Code)
		}
	}
	if rec := login("198.51.100.1"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-threshold for real IP A: code=%d want 429, body=%s", rec.Code, rec.Body.String())
	}

	// 真实来源 B（198.51.100.2）：同样来自反代 127.0.0.1，但不受 A 的锁定影响。
	if rec := login("198.51.100.2"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("real IP B must be independent of A: code=%d want 401, body=%s", rec.Code, rec.Body.String())
	}
}

// TestLoginRateLimitNoTrustedProxy_RemoteAddrBasis 无可信代理（直连形态）：
// 伪造 X-Real-IP 不能分化限速计数——同一对端无论怎么换头，5 次失败后即锁。
func TestLoginRateLimitNoTrustedProxy_RemoteAddrBasis(t *testing.T) {
	resetLoginLimiter()
	t.Cleanup(resetLoginLimiter)
	p := New(Config{Version: "test", APIKey: "k"})

	login := func(spoofed string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := loginPost(t, "198.51.100.9:6000", map[string]string{"X-Real-IP": spoofed})
		p.ServeHTTP(rec, req)
		return rec
	}
	for i := 0; i < loginFailMax; i++ {
		if rec := login(spoofRotate(i)); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure #%d: code=%d want 401", i+1, rec.Code)
		}
	}
	if rec := login("9.9.9.9"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("spoofed rotation must not reset limit: code=%d want 429, body=%s", rec.Code, rec.Body.String())
	}
}

// spoofRotate 生成几个不同的伪造 IP（直连形态下都应无效）。
func spoofRotate(i int) string {
	ips := []string{"9.9.9.9", "8.8.8.8", "9.9.9.10", "8.8.8.9", "9.9.9.11"}
	return ips[i%len(ips)]
}
