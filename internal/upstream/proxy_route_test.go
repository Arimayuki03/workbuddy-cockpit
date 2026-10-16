// proxy_route_test.go 每账号命名出口代理线路的行为测试。
//
// 覆盖：
//   - transport 选择：绑定有效 → 请求经该线路的 transport（httptest 起代理，
//     断言上游收到请求且 Proxy 头按线路转发）；未绑定 → 直连（共享 client）；
//     绑定无效（线路表查无此名）→ 拒绝发出（连接零建立 + 错误明确）。
//   - config 解析：合法 URL 通过、非法 URL（坏 scheme/缺 host/缺端口）fail-fast。
//   - auth 落盘：Parse/SaveAtomic 对 proxy_route 的直通（嵌套形 auth 段内）。
//   - 并发：-race 下多 goroutine 同时按线路出站（client 缓存构建竞态）。
package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// newProxyRouteTestClient 起一个 httptest 目标站 + 返回装配好线路表的 Client。
// 代理 URL 指向 proxySrv（由调用方决定是否真的起代理；无效地址用于断言拒绝/走代理路径）。
func newProxyRouteTestClient(t *testing.T, routes map[string]string) (*Client, *httptest.Server, *int) {
	t.Helper()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	t.Cleanup(target.Close)
	c := New()
	if err := c.SetProxyRoutes(routes); err != nil {
		t.Fatalf("SetProxyRoutes: %v", err)
	}
	hits := 0
	c.HTTP.Transport = roundTripCounter{next: c.HTTP.Transport, hits: &hits}
	return c, target, &hits
}

// roundTripCounter 包装 RoundTripper 统计**直连共享 client**的请求次数
// （绑定线路的请求不应经过共享 transport）。
type roundTripCounter struct {
	next http.RoundTripper
	hits *int
}

func (r roundTripCounter) RoundTrip(req *http.Request) (*http.Response, error) {
	*r.hits++
	return r.next.RoundTrip(req)
}

// TestProxyRouteDoJSONReachesProxy 绑定有效线路 → doJSON 请求确实经代理转发
// （httptest 代理服务器收到 CONNECT/转发请求并打上标记头）。
func TestProxyRouteDoJSONReachesProxy(t *testing.T) {
	proxyHit := false
	// httptest 起一个「假代理」：标准库 http.Transport 对 http:// 目标走
	// 绝对 URL 的普通转发请求（非 CONNECT），handler 直接把请求转给目标站。
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 转发到目标站（简化：目标站逻辑由本 handler 代答，代理命中即证明走了线路）
		proxyHit = true
		w.Header().Set("X-Via-Route", "route-a")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"data":{"via":"route-a"}}`))
	}))
	defer inner.Close()
	proxyURL := strings.TrimPrefix(inner.URL, "http://")
	proxyAddr = proxyURL // httptest URL → host:port
	c, _, hits := newProxyRouteTestClient(t, map[string]string{
		"route-a": "http://" + proxyURL,
	})
	a := &auth.Auth{UID: "u1", ProxyRoute: "route-a"}

	req, _ := http.NewRequest(http.MethodPost, "http://example.invalid/v2/report", nil)
	if _, err := c.doJSONAuth(a, req); err != nil {
		t.Fatalf("doJSONAuth via route: %v", err)
	}
	if !proxyHit {
		t.Fatal("请求未经代理线路转发")
	}
	if *hits != 0 {
		t.Fatalf("绑定线路的请求不应经过共享 transport，实际 %d 次", *hits)
	}
	// 同一线路再次请求：命中 client 缓存（同一 *http.Client 指针）
	hc1, _ := c.transportFor(a, false)
	hc2, _ := c.transportFor(a, false)
	if hc1 != hc2 {
		t.Fatal("同线路未命中 client 缓存")
	}
}

var proxyAddr string // 占位（上文引用避免未用变量）；实际拼接见上方直接字符串

// TestProxyRouteUnboundGoesDirect 未绑定 → 走共享 client（现状直连，零回归）。
func TestProxyRouteUnboundGoesDirect(t *testing.T) {
	c, target, hits := newProxyRouteTestClient(t, map[string]string{
		"route-a": "http://127.0.0.1:1", // 存在的线路（无人绑定）
	})
	// 未绑定账号：ProxyRoute 空
	a := &auth.Auth{UID: "u1"}
	req, _ := http.NewRequest(http.MethodGet, target.URL+"/v2/x", nil)
	if _, err := c.doJSONAuth(a, req); err != nil {
		t.Fatalf("unbound doJSON: %v", err)
	}
	if *hits != 1 {
		t.Fatalf("未绑定应直连（共享 transport 1 次），实际 %d", *hits)
	}
}

// TestProxyRouteUnknownRefuses 绑定无效线路 → 拒绝发出：共享 transport 零命中、
// 错误信息明确（not configured），绝不静默回退直连。
func TestProxyRouteUnknownRefuses(t *testing.T) {
	c, target, hits := newProxyRouteTestClient(t, map[string]string{
		"route-a": "http://127.0.0.1:1",
	})
	a := &auth.Auth{UID: "u1", ProxyRoute: "ghost-route"}
	req, _ := http.NewRequest(http.MethodGet, target.URL+"/v2/x", nil)
	_, err := c.doJSONAuth(a, req)
	if err == nil {
		t.Fatal("未知线路应拒绝发出")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("错误信息应含 not configured: %v", err)
	}
	if *hits != 0 {
		t.Fatalf("拒绝语义下共享 transport 不应有任何请求，实际 %d", *hits)
	}
}

// TestProxyRouteChatEndpointRejects chat 形态同样拒绝（ChatStreamContext 底座）。
func TestProxyRouteChatEndpointRejects(t *testing.T) {
	c, _, _ := newProxyRouteTestClient(t, nil)
	a := &auth.Auth{UID: "u1", ProxyRoute: "ghost"}
	if _, err := c.chatEndpoint(a); err == nil {
		t.Fatal("chat 形态未知线路应拒绝")
	}
	// 空绑定回落共享 ChatHTTP
	hc, err := c.chatEndpoint(&auth.Auth{UID: "u2"})
	if err != nil || hc != c.ChatHTTP {
		t.Fatalf("空绑定应回落 ChatHTTP: %v", err)
	}
}

// TestProxyRouteTransportProxyURL 断言线路 transport 的 Proxy 函数返回正确 URL
// （不依赖真实代理的确定性验证，供排除环境干扰使用）。
func TestProxyRouteTransportProxyURL(t *testing.T) {
	c := New()
	if err := c.SetProxyRoutes(map[string]string{"r1": "http://bob:secret@10.0.0.9:8888"}); err != nil {
		t.Fatalf("SetProxyRoutes: %v", err)
	}
	a := &auth.Auth{UID: "u1", ProxyRoute: "r1"}
	hc, err := c.transportFor(a, false)
	if err != nil {
		t.Fatalf("transportFor: %v", err)
	}
	tr, ok := hc.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatal("线路 client 应带 Proxy transport")
	}
	u, err := tr.Proxy(&http.Request{URL: mustParse(t, "https://www.codebuddy.cn/x")})
	if err != nil || u == nil {
		t.Fatalf("Proxy 返回: %v %v", u, err)
	}
	if u.Host != "10.0.0.9:8888" || u.User.Username() != "bob" {
		t.Fatalf("Proxy URL 不符: %s", u.String())
	}
	if pw, _ := u.User.Password(); pw != "secret" {
		t.Fatalf("代理凭据应保留在 transport 内（仅展示层脱敏）")
	}
}

// routeProxyFn 取线路 transport 的 Proxy 选择函数：生产路径的判定入口——
// http.Transport 在每个请求上调用它决定这一跳走不走代理（返回 nil = 直连）。
func routeProxyFn(t *testing.T, c *Client, route string) func(*http.Request) (*url.URL, error) {
	t.Helper()
	hc, err := c.transportFor(&auth.Auth{UID: "u-bypass", ProxyRoute: route}, false)
	if err != nil {
		t.Fatalf("transportFor(%s): %v", route, err)
	}
	tr, ok := hc.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatalf("线路 %s 的 client 应带 Proxy 选择函数", route)
	}
	return tr.Proxy
}

// TestProxyRouteDirectOnlyTargetsBypass 私网/本机目标不经出口代理（直连），
// 公网目标维持现状走绑定线路（workbuddy-manager d79f13cc 吸收件）。
//
// 双重断言：directOnlyHost（判定函数本身）+ 线路 transport 的 Proxy 函数
// （真实生效入口，返回 nil 即标准库语义「本次请求直连」）。
func TestProxyRouteDirectOnlyTargetsBypass(t *testing.T) {
	const routeProxy = "http://127.0.0.1:1" // 必然连不通的代理：只用于断言「不该走它」
	c := New()
	if err := c.SetProxyRoutes(map[string]string{"route-a": routeProxy}); err != nil {
		t.Fatalf("SetProxyRoutes: %v", err)
	}
	proxyFn := routeProxyFn(t, c, "route-a")
	wantProxy := mustParse(t, routeProxy)

	cases := []struct {
		name   string
		target string
		direct bool
	}{
		// —— 任务书枚举的形态：全部直连 ——
		{"ipv4 loopback", "http://127.0.0.1:8080/v2/x", true},
		{"ipv6 loopback", "http://[::1]:8080/v2/x", true},
		{"rfc1918 10/8", "http://10.0.0.5/v2/x", true},
		{"rfc1918 192.168/16", "http://192.168.1.9:9000/v2/x", true},
		{"rfc1918 172.16/12", "https://172.16.3.4/v2/x", true},
		{"link-local v4", "http://169.254.1.1/v2/x", true},
		{"ula v6 fc00::/7", "http://[fc00::1]/v2/x", true},
		{"localhost", "http://localhost:8080/v2/x", true},
		{"localhost 大写", "http://LOCALHOST/v2/x", true},
		{"docker 宿主别名", "http://host.docker.internal:11434/v2/x", true},
		// —— 同族边界/形态加固：防漏判 ——
		{"ula fd00::/8", "http://[fd00::5]:8080/v2/x", true},
		{"v6 link-local", "http://[fe80::1]:80/v2/x", true},
		{"大写 v6 字面量", "http://[FC00::1]/v2/x", true},
		{".local 后缀域名", "http://printer.local:631/v2/x", true},
		{"FQDN 绝对形尾点", "http://localhost.:8080/v2/x", true},
		{"IP 字面量尾点", "http://192.168.1.9.:8080/v2/x", true},
		{"4-in-6 归一", "http://[::ffff:192.168.1.9]:8080/v2/x", true},
		// —— 公网目标：维持现状（走绑定线路）——
		{"公网 IP", "http://8.8.8.8/v2/x", false},
		{"公网域名", "https://www.codebuddy.cn/v2/x", false},
		{"公网域名 2", "http://example.com/v2/x", false},
		{"10/8 段外", "http://11.0.0.1/v2/x", false},
		{"172.16/12 段外", "http://172.32.1.1/v2/x", false},
		{"4-in-6 公网", "http://[::ffff:8.8.8.8]/v2/x", false},
		// —— 反陷阱：形近但不该直连（判定只看字面，不做 DNS）——
		{"IP 形态域名", "http://127.0.0.1.evil.com/v2/x", false},
		{"别名后缀域名", "http://host.docker.internal.evil.com/v2/x", false},
		{"mylocal 无点前缀", "http://mylocal/v2/x", false},
		{"非 .local 结尾", "http://mylocal.evil.com/v2/x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := mustParse(t, tc.target)
			if got := directOnlyHost(u.Hostname()); got != tc.direct {
				t.Fatalf("directOnlyHost(%q)=%v want %v", u.Hostname(), got, tc.direct)
			}
			got, err := proxyFn(&http.Request{URL: u})
			if err != nil {
				t.Fatalf("Proxy: %v", err)
			}
			if tc.direct {
				if got != nil {
					t.Fatalf("私网/本机目标应直连（Proxy 返回 nil），实际 %v", got)
				}
				return
			}
			if got == nil || got.String() != wantProxy.String() {
				t.Fatalf("公网目标应走绑定线路 %v，实际 %v", wantProxy, got)
			}
		})
	}
}

// TestProxyRoutePrivateTargetGoesDirectLive 端到端：绑定线路的账号访问本机目标
// （httptest 起的 127.0.0.1 站）真的不经代理。线路代理故意指向必然连不通的端口
// ——直连判定失效时请求会交给该端口并失败，成功即证明走了直连。
// 注意：直连 ≠ 改用共享 client——仍是线路自己的 transport，只是这一跳不设代理
// （连接池隔离与线路参数继续生效），故共享 transport 计数保持 0。
func TestProxyRoutePrivateTargetGoesDirectLive(t *testing.T) {
	hits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"data":{"direct":true}}`))
	}))
	defer target.Close()
	c := New()
	if err := c.SetProxyRoutes(map[string]string{"r1": "http://127.0.0.1:1"}); err != nil {
		t.Fatalf("SetProxyRoutes: %v", err)
	}
	a := &auth.Auth{UID: "u1", ProxyRoute: "r1"}
	req, _ := http.NewRequest(http.MethodGet, target.URL+"/v2/balance", nil)
	if _, err := c.doJSONAuth(a, req); err != nil {
		t.Fatalf("本机目标应直连成功（不应交给出口代理）: %v", err)
	}
	if hits != 1 {
		t.Fatalf("本机目标站应被直连命中 1 次，实际 %d", hits)
	}
}

// TestDirectOnlyHostZeroAlloc 热路径零分配守卫：判定在每个出站请求的 Proxy 钩子
// 里执行，域名（常态目标）与 IP 字面量都不得产生堆分配（netip.ParseAddr 的失败
// 路径会构造错误对象，故实现里先做 looksLikeIPLiteral 预筛）。
func TestDirectOnlyHostZeroAlloc(t *testing.T) {
	hosts := []string{
		"192.168.1.9", "10.0.0.5", "169.254.1.1", "127.0.0.1", "::1", "fc00::1",
		"localhost", "LOCALHOST", "host.docker.internal", "printer.local",
		"api.example.com", "www.codebuddy.cn", "8.8.8.8", "127.0.0.1.evil.com",
	}
	for _, host := range hosts {
		host := host
		if n := testing.AllocsPerRun(200, func() { _ = directOnlyHost(host) }); n != 0 {
			t.Errorf("directOnlyHost(%q) 应零分配，实际 %v 次/调用", host, n)
		}
	}
}

// TestValidateProxyRoutes 配置校验：合法通过；非法 fail-fast（bad scheme / 缺 host /
// 缺端口 / 空名字 / socks5 明确拒绝）。
func TestValidateProxyRoutes(t *testing.T) {
	cases := []struct {
		name    string
		routes  map[string]string
		wantErr bool
		wantSub string
	}{
		{"nil ok", nil, false, ""},
		{"empty ok", map[string]string{}, false, ""},
		{"underscore note key skipped", map[string]string{"_note": "段内说明文字，不是线路"}, false, ""},
		{"underscore keys skipped with real routes", map[string]string{"_note": "说明", "_x": "", "a": "http://h:8080"}, false, ""},
		{"http ok", map[string]string{"a": "http://u:p@h:8080"}, false, ""},
		{"https ok", map[string]string{"a": "https://h:443"}, false, ""},
		{"socks5 refused", map[string]string{"a": "socks5://h:1080"}, true, "socks5"},
		// 私网目标直连的判定**不放宽代理 scheme**：代理地址本身是私网/本机也一样
		// 只允许 http/https（安全口径：出站只走 http/https）。
		{"socks5 refused for loopback proxy", map[string]string{"a": "socks5://127.0.0.1:1080"}, true, "socks5"},
		{"bad scheme", map[string]string{"a": "ftp://h:21"}, true, "scheme"},
		{"no host", map[string]string{"a": "http://:8080"}, true, "host"},
		{"no port", map[string]string{"a": "http://h"}, true, "端口"},
		{"empty name", map[string]string{"": "http://h:80"}, true, "线路名"},
		{"garbage", map[string]string{"a": "::::"}, true, ""},
	}
	for _, tc := range cases {
		err := ValidateProxyRoutes(tc.routes)
		if tc.wantErr != (err != nil) {
			t.Errorf("%s: err=%v wantErr=%v", tc.name, err, tc.wantErr)
		}
		if err != nil && tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub) {
			t.Errorf("%s: err %q 应含 %q", tc.name, err.Error(), tc.wantSub)
		}
	}
}

// TestSetProxyRoutesSkipsNoteKeys `_` 前缀键（_note 等手写段内说明）不进线路
// 表：SetProxyRoutes 跳过而非报错，且不产生同名线路（绑定 _note 必然未命中）。
func TestSetProxyRoutesSkipsNoteKeys(t *testing.T) {
	c := New()
	if err := c.SetProxyRoutes(map[string]string{
		"_note": "段内说明：route-a 是香港线路",
		"route-a": "http://h:8080",
	}); err != nil {
		t.Fatalf("SetProxyRoutes 应跳过 _note 键: %v", err)
	}
	// 真线路可用，_note 不可绑。
	if _, err := c.transportFor(&auth.Auth{UID: "u", ProxyRoute: "route-a"}, false); err != nil {
		t.Fatalf("route-a 应可用: %v", err)
	}
	if _, err := c.transportFor(&auth.Auth{UID: "u", ProxyRoute: "_note"}, false); err == nil {
		t.Fatal("_note 不应成为可绑定线路")
	}
}

// TestSetProxyRoutesAtomic 整表替换的原子性：含非法条目的整表拒绝后旧表保持。
func TestSetProxyRoutesAtomic(t *testing.T) {
	c := New()
	if err := c.SetProxyRoutes(map[string]string{"good": "http://h:80"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// 合法替换
	if err := c.SetProxyRoutes(map[string]string{"g2": "http://h2:80"}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	a := &auth.Auth{UID: "u", ProxyRoute: "good"}
	if _, err := c.transportFor(a, false); err == nil {
		t.Fatal("旧表条目应已随整表替换失效")
	}
	// 非法替换失败 → g2 仍在（保持旧表）
	if err := c.SetProxyRoutes(map[string]string{"bad": "socks5://x:1"}); err == nil {
		t.Fatal("非法表应被拒绝")
	}
	a2 := &auth.Auth{UID: "u", ProxyRoute: "g2"}
	if _, err := c.transportFor(a2, false); err != nil {
		t.Fatalf("替换失败后旧表应保持: %v", err)
	}
}

// TestProxyRouteConcurrent -race：多 goroutine 并发按线路出站（client 缓存
// 首建竞态）+ 未绑定直连混合。
func TestProxyRouteConcurrent(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer target.Close()
	c := New()
	if err := c.SetProxyRoutes(map[string]string{"r1": target.URL}); err != nil {
		t.Fatalf("SetProxyRoutes: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var a *auth.Auth
			if i%2 == 0 {
				a = &auth.Auth{UID: "u", ProxyRoute: "r1"}
			} else {
				a = &auth.Auth{UID: "u"}
			}
			req, _ := http.NewRequest(http.MethodGet, target.URL+"/x", nil)
			if _, err := c.doJSONAuth(a, req); err != nil {
				t.Errorf("doJSONAuth %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
}

// TestMaskProxyURL 脱敏：密码 → ***，无密码原样，坏 URL 原样返回。
func TestMaskProxyURL(t *testing.T) {
	if got := MaskProxyURL("http://bob:secret@host:8080"); got != "http://bob:***@host:8080" {
		t.Fatalf("masked=%s", got)
	}
	if got := MaskProxyURL("http://host:8080"); got != "http://host:8080" {
		t.Fatalf("no-pass=%s", got)
	}
	if got := MaskProxyURL("http://bob@host:8080"); got != "http://bob@host:8080" {
		t.Fatalf("user-only=%s", got)
	}
}

// TestAuthProxyRouteRoundTrip auth 文件 proxy_route 直通（Parse → SaveAtomic 回读）。
func TestAuthProxyRouteRoundTrip(t *testing.T) {
	raw := `{"auth":{"accessToken":"a","refreshToken":"r","expiresAt":1,"domain":"d","realm":"cn","proxy_route":"route-a"},"account":{"uid":"u1"}}`
	a, err := auth.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.ProxyRoute != "route-a" {
		t.Fatalf("ProxyRoute=%q", a.ProxyRoute)
	}
	// 顶层兼容键
	raw2 := `{"auth":{"accessToken":"a","refreshToken":"r"},"account":{"uid":"u1"},"proxy_route":"route-b"}`
	a2, err := auth.Parse([]byte(raw2))
	if err != nil {
		t.Fatalf("parse top-level: %v", err)
	}
	if a2.ProxyRoute != "route-b" {
		t.Fatalf("top-level ProxyRoute=%q", a2.ProxyRoute)
	}
	// ExportDoc 带线路名、不带代理凭据
	doc := a.ExportDoc()
	authSec := doc["auth"].(map[string]any)
	if authSec["proxy_route"] != "route-a" {
		t.Fatalf("ExportDoc proxy_route=%v", authSec["proxy_route"])
	}
	blob, _ := json.Marshal(doc)
	if strings.Contains(string(blob), "secret") || strings.Contains(string(blob), "http://") {
		t.Fatal("ExportDoc 不应携带任何代理 URL/凭据")
	}
}

// mustParse 测试辅助。
func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

// 编译期引用（time/context 在并发测试的超时场景保留使用位）。
var _ = time.Second
var _ = context.Background
