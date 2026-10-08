// proxy_route.go 每账号命名出口代理线路（workbuddy-manager proxy_routes 吸收件）。
//
// 设计语义（严格照参考实现 server/config.py proxy_routes/account_proxy +
// server/services/tencent.py _account_http_client）：
//   - 线路表存**共享配置**（线路名 → 代理 URL，http/https 形态），落在 config.json
//     的 proxy_routes 段；账号文件只存线路名（auth.ProxyRoute），代理凭据不在
//     auths/ 里扩散——导出/分享凭证不会连带泄密。
//   - 解析三态（account_proxy 同口径）：
//     空串   → 直连（现状行为，零配置零回归）；
//     命中表 → 用该代理构建独立 transport 出站；
//     未命中 → **拒绝发出**并返回明确错误，绝不静默回退直连——回落 = 出口
//     隔离失效且无人发现（多账号同 IP 撞风控的正是要防的事）。
//   - 全链路生效：所有出站都汇入 client.go 的 doJSON / ChatStreamContext /
//     endpoint 底座，按账号 ProxyRoute 选 client——对话轮转、签到、余额、
//     续期、加号登录（login.go 的 loginHTTP 除外：该路径发生在账号绑定之前，
//     由请求体显式携带线路名，见 panel/login.go）。
//
// WAF/风控联动说明：internal 侧 wafip 是按上游返回形态驱动的拦截状态机
// （状态挂在账号池条目上），与出口 IP 无关——换出口后同一上游域名的拦截
// 状态继续共享，无需按出口隔离（共用状态对多号同线路的场景是保守正确的）。
package upstream

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

// proxySchemes 当前支持的代理 URL scheme（http/https 标准代理）。
//
// socks5 支持结论（任务书要求显式说明）：golang.org/x/net/proxy 提供 socks5
// Dialer，但项目 go.mod 依赖约束是「只允许标准库 + go-redis」（go.sum 无任何
// x/net 条目），任务书明确「若需要引依赖就只支持 http/https 并在报告说明，
// 别擅自加依赖」——故本实现只支持 http/https 代理，socks5:// 在配置加载
// （cmd/server normalize）即 fail-fast 拒绝，绑定线路的请求同样拒绝发出。
var proxySchemes = map[string]bool{"http": true, "https": true}

// proxyRouteEntry 线路表中一条已验证的线路：解析后的 URL + 构造参数快照。
type proxyRouteEntry struct {
	url *url.URL
}

// proxyClientCache 按线路名缓存的出站 client（含独立 transport，连接池按线路
// 天然隔离——不同出口不共用连接，杜绝「A 线路的 TCP 连接被 B 线路复用」的
// 隔离漏洞）。条目惰性构建、互斥保护；线路表整体替换时缓存清空。
type proxyClientCache struct {
	mu      sync.Mutex
	clients map[string]*http.Client
}

// newProxyClientCache 空缓存。
func newProxyClientCache() *proxyClientCache {
	return &proxyClientCache{clients: map[string]*http.Client{}}
}

// clientFor 返回指定代理 URL 的（缓存的）出站 client。缓存键由调用方保证为
// 同一 URL 字符串；表内 URL 变更经 SetProxyRoutes 全量替换触发缓存重建。
// 构造参数与 newTransport() 对齐（禁 h2 / TLS 握手超时 / 短 keepalive 探测），
// 仅叠加 Proxy 拨号——连接层加固对代理线路同样生效。
func (cc *proxyClientCache) clientFor(routeName, rawURL string) (*http.Client, error) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if c, ok := cc.clients[routeName]; ok {
		return c, nil
	}
	u, err := parseProxyURL(rawURL)
	if err != nil {
		return nil, err
	}
	tr := newTransport()
	tr.Proxy = http.ProxyURL(u)
	c := &http.Client{
		// 与 Client.HTTP 同款总时长上限（短 RPC 语义；chat 走同一 transport 的
		// 独立 client，见 chatClientFor）。
		Timeout:   120 * time.Second,
		Transport: tr,
	}
	cc.clients[routeName] = c
	return c, nil
}

// chatClientFor 返回指定线路的聊天 SSE 专用 client（Timeout=0，首字节由
// transport 的 ResponseHeaderTimeout 约束——与主路径 ChatHTTP 同语义），
// 与短 RPC client 共享同一个 transport（连接池同主路径口径不重复）。
func (cc *proxyClientCache) chatClientFor(routeName, rawURL string) (*http.Client, error) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	chatKey := routeName + "\x00chat"
	if c, ok := cc.clients[chatKey]; ok {
		return c, nil
	}
	// 先确保短 RPC client 存在（同一 transport 被两个 client 共享，与主路径
	// HTTP/ChatHTTP 共用 newTransport 产物同构）。
	base, err := cc.clientForLocked(routeName, rawURL)
	if err != nil {
		return nil, err
	}
	c := &http.Client{Timeout: 0, Transport: base.Transport}
	cc.clients[chatKey] = c
	return c, nil
}

// clientForLocked clientFor 的锁内实现（供 chatClientFor 复用，避免重入死锁）。
func (cc *proxyClientCache) clientForLocked(routeName, rawURL string) (*http.Client, error) {
	if c, ok := cc.clients[routeName]; ok {
		return c, nil
	}
	u, err := parseProxyURL(rawURL)
	if err != nil {
		return nil, err
	}
	tr := newTransport()
	tr.Proxy = http.ProxyURL(u)
	c := &http.Client{
		Timeout:   120 * time.Second,
		Transport: tr,
	}
	cc.clients[routeName] = c
	return c, nil
}

// parseProxyURL 校验代理 URL：scheme 必须 http/https（socks5 支持结论见
// proxySchemes 注释）、host 非空、port 合法。错误信息带上下文，直接面向
// 「配置写错」的用户（fail-fast 语义，调用方不做二次包装）。
func parseProxyURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("proxy url parse: %w", err)
	}
	if !proxySchemes[strings.ToLower(u.Scheme)] {
		return nil, fmt.Errorf("proxy url scheme %q 不支持（仅 http/https；socks5 未启用——避免引入 x/net 依赖）", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("proxy url 缺 host")
	}
	if p := u.Port(); p == "0" || p == "" {
		return nil, fmt.Errorf("proxy url 缺端口或端口非法")
	}
	if _, _, err := net.SplitHostPort(u.Host); err != nil {
		return nil, fmt.Errorf("proxy url host:port 解析失败: %w", err)
	}
	return u, nil
}

// MaskProxyURL 代理 URL 脱敏（面板回显用）：密码段替换为 ***，端口与用户名
// 保留（运维需要看是哪台机器）。无密码原样返回（深拷贝避免调用方改到表内
// 缓存的对象）。形如 http://user:pass@host:port → http://user:***@host:port。
func MaskProxyURL(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.User == nil {
		return rawURL
	}
	_, hasPass := u.User.Password()
	if !hasPass {
		return rawURL
	}
	cp := *u
	cp.User = url.UserPassword(u.User.Username(), "***")
	// url.UserPassword 经 String() 会把 * 编码为 %2A——手工重排 userinfo 还原
	// 字面星号（展示层可读性；该结果不回灌解析）。
	s := cp.String()
	return strings.Replace(s, url.QueryEscape("***"), "***", 1)
}

// ValidateProxyRoutes 线路表纯校验（不触碰 Client 状态）：cmd/server 的 config
// normalize 与面板保存共用（fail-fast 语义，非法 URL 在保存/启动时即被拒绝）。
// 空表/nil 合法（= 全部直连）。
func ValidateProxyRoutes(routes map[string]string) error {
	for name, rawURL := range routes {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("线路名不能为空")
		}
		if _, err := parseProxyURL(rawURL); err != nil {
			return fmt.Errorf("%q: %w", name, err)
		}
	}
	return nil
}

// SetProxyRoutes 整体替换线路表（main 装配期与面板热改共用入口）。传入前
// 已由 config normalize 做过 URL 校验（fail-fast），此处防御性复验：任何一条
// 非法 → 报错且**整表不替换**（保持旧表，半新半旧比全旧更危险）。nil/空表
// = 清空（全部绑定回落直连？不——空表使绑定名必然未命中，按拒绝语义拒绝
// 发出，行为正确且显式）。替换成功后清空 per-route client 缓存（旧 URL 的
// 连接池整体作废，新请求按新 URL 重建）。
func (c *Client) SetProxyRoutes(routes map[string]string) error {
	clean := make(map[string]proxyRouteEntry, len(routes))
	for name, rawURL := range routes {
		name = strings.TrimSpace(name)
		if name == "" {
			return fmt.Errorf("proxy route 名不能为空")
		}
		u, err := parseProxyURL(rawURL)
		if err != nil {
			return fmt.Errorf("proxy route %q: %w", name, err)
		}
		clean[name] = proxyRouteEntry{url: u}
	}
	c.proxyMu.Lock()
	defer c.proxyMu.Unlock()
	c.proxyRoutes = clean
	c.proxyClients = newProxyClientCache()
	c.proxyRaw = make(map[string]string, len(clean))
	for name, e := range clean {
		c.proxyRaw[name] = e.url.String()
	}
	return nil
}

// resolveProxy 解析账号绑定的线路：
//   - 空绑定 → ("", "", nil)：直连（现状行为）；
//   - 命中表 → (线路名, 代理URL, nil)；
//   - 未命中 → 非空错误（拒绝发出语义，绝不静默回退直连）。
func (c *Client) resolveProxy(a *auth.Auth) (string, string, error) {
	route := a.ProxyRouteValue()
	if route == "" {
		return "", "", nil
	}
	c.proxyMu.RLock()
	raw, ok := c.proxyRaw[route]
	c.proxyMu.RUnlock()
	if !ok {
		return "", "", fmt.Errorf("proxy route %q not configured (账号绑定的线路不存在于 config.json proxy_routes，拒绝直连出站)", route)
	}
	return route, raw, nil
}

// transportFor 按账号选择出站传输底座：
//   - 未绑定线路 → (nil, nil)：调用方沿用现有共享 client（HTTP/ChatHTTP），零回归；
//   - 绑定有效   → 返回按线路缓存的 *http.Client（短 RPC / chat 形态由 caller 区分）；
//   - 绑定无效   → 明确错误（调用方拒绝发出）。
func (c *Client) transportFor(a *auth.Auth, chat bool) (*http.Client, error) {
	route, raw, err := c.resolveProxy(a)
	if err != nil || route == "" {
		return nil, err
	}
	if chat {
		return c.proxyClients.chatClientFor(route, raw)
	}
	return c.proxyClients.clientFor(route, raw)
}

// endpoint 出站端点的统一选择器（client.go 各 Do 调用点改造的单一入口）：
// 返回本次请求应使用的 *http.Client。
//   - 账号未绑定线路 → 共享默认 client（现状路径，零回归）；
//   - 绑定有效       → 按线路缓存的独立 client（独立 transport，连接池隔离）；
//   - 绑定无效       → 直接在调用方 goroutine 记日志并返回错误——请求绝不发出。
func (c *Client) endpoint(a *auth.Auth, chat bool) (*http.Client, error) {
	hc, err := c.transportFor(a, chat)
	if err != nil {
		if a != nil {
			log.Printf("ERR: [upstream] proxy_route acct=%s: %v", logfmt.Label(a.UID, a.Nickname), err)
		}
		return nil, err
	}
	if hc == nil {
		if chat {
			return c.chatHTTP(), nil
		}
		return c.HTTP, nil
	}
	return hc, nil
}

// chatEndpoint ChatStreamContext / DesktopChatWithExpert 的 chat 形态选择器
// （Timeout=0 的 SSE client；未绑定回落 ChatHTTP 语义）。
func (c *Client) chatEndpoint(a *auth.Auth) (*http.Client, error) {
	return c.endpoint(a, true)
}

// doEndpoint doJSON（全部 billing/growth/refresh/模型目录/资料等短 RPC）的
// client 选择器。挂在 doJSON 上是全链路收口点：调用方无需逐个改造。
func (c *Client) doEndpoint(a *auth.Auth) (*http.Client, error) {
	return c.endpoint(a, false)
}

// TransportForForTest transportFor 的测试导出（跨包测试 cmd/server 断言整表
// 替换语义用；生产代码不得调用）。
func (c *Client) TransportForForTest(a *auth.Auth) (*http.Client, error) {
	return c.transportFor(a, false)
}
