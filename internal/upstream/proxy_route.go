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
//   - 私网/本机目标强制直连（workbuddy-manager d79f13cc 吸收件）：目标 host 是
//     本机/内网（loopback、RFC1918、link-local、ULA、localhost / .local /
//     host.docker.internal）时**这一跳不走绑定线路**。理由：线路的出口在另一张
//     网络上，让外部代理去连 127.0.0.1/192.168.x.x 只会连到代理自己那台机器或
//     根本连不通，表现为超时/拒绝且没有任何线索指向「代理绕行」，自查成本极高。
//     判定挂在 transport 的 Proxy 函数上（本文件唯一能拿到目标 base URL 的位置，
//     见 routeProxyFunc）；与安全口径同向：服务端出站只允许 http/https（见
//     proxySchemes，scheme 校验未放宽），且不把私网目标交给外部代理转发。
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
	"net/netip"
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

// localHostSuffix mDNS 主机名后缀（抽成常量，长度比较与字面量只写一处）。
const localHostSuffix = ".local"

// directOnlyTargetPrefixes 「目标必须直连（不交给出口代理）」的网段表：
// loopback（127/8、::1）、RFC1918（10/8、172.16/12、192.168/16）、link-local
// （169.254/16、fe80::/10）、ULA（fc00::/7，含 fd00::/8）。
//
// 为什么在此自建而不复用 internal/iputil.PrivateCIDRs：那边是**入站**可信反代
// 判定的口径（且不含 ULA），与出站「目标是否私网」不是同一件事，混用会让两处
// 语义互相牵制（改一边的部署假设会悄悄改另一边的出站行为）。169.254.169.254
// 这类云元数据地址落在 link-local 段内，一并直连——私网目标本就只可能由部署方
// 自己访问，直连与「服务端出站只允许 http/https」的安全口径同向收敛。
//
// 表在包初始化时解析一次（MustParsePrefix）：每请求只做 O(1) 段比对，不重复
// 解析网段字符串（任务书「不要每请求重解析主机名集合」的落点）。
var directOnlyTargetPrefixes = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),    // loopback v4
	netip.MustParsePrefix("::1/128"),        // loopback v6
	netip.MustParsePrefix("10.0.0.0/8"),     // RFC1918
	netip.MustParsePrefix("172.16.0.0/12"),  // RFC1918
	netip.MustParsePrefix("192.168.0.0/16"), // RFC1918
	netip.MustParsePrefix("169.254.0.0/16"), // link-local v4（含 169.254.169.254 元数据地址）
	netip.MustParsePrefix("fe80::/10"),      // link-local v6
	netip.MustParsePrefix("fc00::/7"),       // ULA（fc00::/7 = fc00::/8 + fd00::/8）
}

// directOnlyHost 判定目标 host 是否属于「必须直连」的一类（host 取
// url.URL.Hostname() 口径：已剥掉 [] 与端口）：
//   - 字面 IP 且属 loopback / RFC1918 / link-local / ULA → true；
//   - 主机名（大小写不敏感）等于 localhost / host.docker.internal，或以 .local
//     结尾 → true；
//   - 其余（公网域名、公网 IP）→ false，维持现状走绑定线路。
//
// 只做字面判定、不做 DNS 解析：解析会给热路径加 IO/缓存负担，且结果随 DNS 变化
// （本判定需要的是稳定语义）；而这些目标在实践里就是字面写法（部署方写
// 127.0.0.1 / host.docker.internal，不会写一个解析到内网的公网域名）。
// 也不被「IP 字面量形态的域名」骗到：127.0.0.1.evil.com 这类 ParseAddr 失败、
// 走主机名分支、不匹配任何内网规则 → 仍走绑定线路（判定只为连通性，不承担
// 反 SSRF 职责；私网目标交给外部代理照样连不上，故无需按 DNS 结果放宽）。
//
// 分配说明：全程零堆分配——netip.Addr 是值类型，主机名分支只用 EqualFold 与
// 切片比较（不做 ToLower、不拼字符串）；IP 解析前先做 looksLikeIPLiteral 廉价
// 预筛，绕开 netip.ParseAddr 失败路径的对象构造（实测 1 次分配/次，而绝大多数
// 出站目标是域名）。有 TestDirectOnlyHostZeroAlloc 兜底。
func directOnlyHost(host string) bool {
	if host == "" {
		return false
	}
	// FQDN 绝对形尾点先归一（"localhost." / "192.168.1.9."）：不归一会让前者落进
	// 主机名比较、后者落进主机名分支白名单不命中，两条都漏判成「走线路」。
	h := strings.TrimSuffix(host, ".")
	if looksLikeIPLiteral(h) {
		// IP 字面量（v4/v6 同口径）。
		if addr, err := netip.ParseAddr(h); err == nil {
			// 两处归一：4-in-6（::ffff:192.168.1.9）先 Unmap 回 v4，否则 v4
			// 私网段整片漏判；zone（fe80::1%eth0）去掉，否则 Prefix.Contains
			// 对带 zone 的地址恒为 false（库语义：前缀不含 zone）。
			addr = addr.WithZone("").Unmap()
			for _, p := range directOnlyTargetPrefixes {
				if p.Contains(addr) {
					return true
				}
			}
			return false
		}
	}
	if strings.EqualFold(h, "localhost") || strings.EqualFold(h, "host.docker.internal") {
		return true
	}
	// ".local" 后缀（mDNS 内网名，大小写不敏感）。长度必须大于后缀本身：
	// 排除裸 ".local"，同时 "mylocal"/"notlocal" 不含点前缀也不命中。
	return len(h) > len(localHostSuffix) && strings.EqualFold(h[len(h)-len(localHostSuffix):], localHostSuffix)
}

// looksLikeIPLiteral IP 字面量形态的廉价预筛（零分配，见 directOnlyHost 的分配
// 说明）：含 ':' 视作 v6 形态放行（顺带覆盖 v4-mapped/4-in-6 与 zone 形态），
// 否则只放行「全数字与点」的 v4 形态。域名在第一个字母处即被拒（api.example.com
// 是常态目标），不进入 netip.ParseAddr 的失败路径。
func looksLikeIPLiteral(host string) bool {
	if strings.IndexByte(host, ':') >= 0 {
		return true
	}
	for i := 0; i < len(host); i++ {
		if c := host[i]; (c < '0' || c > '9') && c != '.' {
			return false
		}
	}
	return true
}

// routeProxyFunc 构造绑定线路 transport 的 Proxy 选择函数：目标是本机/内网 →
// 返回 (nil, nil)（标准库语义 = 本次请求直连、不设代理），否则返回线路代理 URL。
//
// 为什么挂在这里而不是 client 选择点（endpoint/transportFor）：本包的目标 base
// URL 由每个请求自带（chatBase 按账号/域切换、realm 分支等），而 client 是按
// **线路**缓存复用的（与账号、目标域无关），构建期根本拿不到目标 host；
// transport.Proxy 是标准库给的每请求钩子，正是「按目标决定这一跳走不走代理」的
// 正确挂载点，也让改动集中在 proxy_route.go 内。
// 热路径成本：线路 URL 与网段表都在构建期/初始化期固定，每请求只做一次 host
// 比对（O(1)、零分配、无锁）——比再叠一层判定结果缓存更便宜，故不设缓存。
// req/URL 为 nil 是标准库不会出现的异常形态：此时无法判定目标，返回线路代理
// （维持现状、宁可走线路，也不静默放宽出口隔离）。
func routeProxyFunc(proxyURL *url.URL) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		if req != nil && req.URL != nil && directOnlyHost(req.URL.Hostname()) {
			return nil, nil
		}
		return proxyURL, nil
	}
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
// 代理钩子经 routeProxyFunc：本机/内网目标在**同一 client** 内自动回落直连，
// 无需为此再建一套「直连版线路 client」（判定按请求生效，见 routeProxyFunc）。
func (cc *proxyClientCache) clientFor(routeName, rawURL string) (*http.Client, error) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.clientForLocked(routeName, rawURL)
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
// 线路 transport 的**唯一**构造点：Proxy 钩子一律经 routeProxyFunc（私网/本机
// 目标直连，见该函数注释），避免两处构造各自演化出不一致的出口语义。
func (cc *proxyClientCache) clientForLocked(routeName, rawURL string) (*http.Client, error) {
	if c, ok := cc.clients[routeName]; ok {
		return c, nil
	}
	u, err := parseProxyURL(rawURL)
	if err != nil {
		return nil, err
	}
	tr := newTransport()
	tr.Proxy = routeProxyFunc(u)
	c := &http.Client{
		// 与 Client.HTTP 同款总时长上限（短 RPC 语义；chat 走同一 transport 的
		// 独立 client，见 chatClientFor）。
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
// 空表/nil 合法（= 全部直连）。`_` 前缀键视为注释说明（_note 等）跳过不校验
// ——proxy_routes 是 map[string]string 段（无 struct 段的 _note_* 字位），手写
// 配置只能靠 `_` 前缀键写段内说明，不能让说明文字被当线路解析成空 scheme 报错。
func ValidateProxyRoutes(routes map[string]string) error {
	for name, rawURL := range routes {
		if strings.HasPrefix(name, "_") {
			continue // 注释键（如 _note）：不是线路
		}
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
// 连接池整体作废，新请求按新 URL 重建）。`_` 前缀键视为注释说明（_note 等）
// 跳过——与 ValidateProxyRoutes 同口径，手写配置的段内说明不得被当线路解析。
func (c *Client) SetProxyRoutes(routes map[string]string) error {
	clean := make(map[string]proxyRouteEntry, len(routes))
	for name, rawURL := range routes {
		if strings.HasPrefix(name, "_") {
			continue // 注释键（如 _note）：不是线路
		}
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
			log.Printf("ERR: [upstream] proxy_route acct=%s: %v", logfmt.Label(a.UID, a.NicknameValue()), err)
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
