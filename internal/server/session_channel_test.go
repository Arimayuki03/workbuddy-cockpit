package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"workbuddy2api/internal/httpauth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// sessionCookieFor 用签发函数造一个有效期 d 的 wb_session 值（与 panel 登录产物同形态）。
func sessionCookieFor(t *testing.T, key string, d time.Duration) string {
	t.Helper()
	payload, err := json.Marshal(map[string]int64{"exp": time.Now().Add(d).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return httpauth.SignSessionToken(payload, key)
}

// TestSessionChannelNativeEndpoints v1.2.0 会话通道：原生 withAuth 端点
// （/status、/v1/stats、/api/request_logs）带有效 wb_session 放行；无 cookie/
// 错签名/过期仍 401；SessionKey=nil（面板未启用）时不认任何 cookie。
func TestSessionChannelNativeEndpoints(t *testing.T) {
	p := pool.New("")
	up := &upstream.Client{HTTP: httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).Client()}
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "k1",
		SessionKey: func() string { return "k1" }})

	good := sessionCookieFor(t, "k1", time.Hour)
	expired := sessionCookieFor(t, "k1", -time.Hour)
	wrongSig := sessionCookieFor(t, "other-key", time.Hour)

	cases := []struct {
		name   string
		cookie string
		want   int
	}{
		{"有效 cookie → 200", good, http.StatusOK},
		{"无 cookie → 401", "", http.StatusUnauthorized},
		{"过期 cookie → 401", expired, http.StatusUnauthorized},
		{"错签名 cookie → 401", wrongSig, http.StatusUnauthorized},
	}
	for _, c := range cases {
		for _, path := range []string{"/status", "/v1/stats", "/api/request_logs"} {
			req := httptest.NewRequest("GET", path, nil)
			if c.cookie != "" {
				req.AddCookie(&http.Cookie{Name: httpauth.SessionCookieName, Value: c.cookie})
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Errorf("%s %s: code=%d want %d", c.name, path, rec.Code, c.want)
			}
		}
	}
}

// TestSessionChannelDisabledWithoutPanel SessionKey=nil（panel.enabled=false）：
// 即便 cookie 签名正确也不放行——会话通道只随面板开启。
func TestSessionChannelDisabledWithoutPanel(t *testing.T) {
	p := pool.New("")
	up := &upstream.Client{HTTP: httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).Client()}
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "k1"})

	req := httptest.NewRequest("GET", "/status", nil)
	req.AddCookie(&http.Cookie{Name: httpauth.SessionCookieName, Value: sessionCookieFor(t, "k1", time.Hour)})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("cookie must be refused when panel session channel is off: code=%d", rec.Code)
	}
}
