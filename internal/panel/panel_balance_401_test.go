// panel_balance_401_test.go 面板余额接口的「401 + HTML 网关页」处置契约：
// 上游鉴权层拒绝账号凭证时，单号余额刷新必须自动禁用该账号（disabled_reason
// 写明需重新登录），并把可读原因返回给前端——不能把整段 HTML 当错误弹窗，
// 也不能只报错不禁用（死号会一直在池里反复 401）。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// newBalance401Panel 组装一个所有上游请求都返回 401 HTML 网关页的面板。
func newBalance401Panel(t *testing.T) (*Panel, *pool.Pool) {
	t.Helper()
	const html401 = `<html> <head><title>401 Authorization Required</title></head> <body> <center><h1>401 Authorization Required</h1></center> <hr><center>openresty</center> <p><em>Powered by <a href="https://apisix.apache.org/">APISIX</a></p> </body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(html401))
	}))
	t.Cleanup(srv.Close)

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Version: "test", APIKey: "test-key", Pool: p, Upstream: up}), p
}

// TestAccountBalance401GatewayPageAutoDisables 单号余额刷新撞 401 HTML：
//   - HTTP 502，错误体是可读的 SessionRevokedReason（不含整段 HTML）；
//   - 账号立即自动禁用，disabled_reason 带 401 gateway page（面板显示需重登）。
func TestAccountBalance401GatewayPageAutoDisables(t *testing.T) {
	panel, pool := newBalance401Panel(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/accounts/u1/balance", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	panel.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON：%v（body=%s）", err, rec.Body.String())
	}
	if resp.OK {
		t.Errorf("ok should be false: %+v", resp)
	}
	if resp.Error != upstream.SessionRevokedReason {
		t.Errorf("error=%q want %q", resp.Error, upstream.SessionRevokedReason)
	}
	if strings.Contains(resp.Error, "<html") {
		t.Errorf("error must not leak raw HTML: %q", resp.Error)
	}

	st, ok := pool.Status("u1")
	if !ok {
		t.Fatal("账号应仍在池内")
	}
	if !st.Disabled {
		t.Fatalf("401 网关页必须自动禁用账号：%+v", st)
	}
	if !strings.Contains(st.DisabledReason, "401 gateway page") {
		t.Errorf("disabled_reason=%q want 401 gateway page", st.DisabledReason)
	}
}
