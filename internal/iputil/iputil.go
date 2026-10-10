// Package iputil 可信代理感知的客户端 IP 解析与 CIDR 匹配工具。
//
// 信任模型（安全要点，参考实现 server/iputil.py——该项目实测被绕过）：
// `X-Forwarded-For` 与 `X-Real-IP` 都是**客户端可伪造**的普通请求头。反向代理以
// `$remote_addr` 覆盖写入 X-Real-IP 只是"通常如此"，前提是**请求确实经过那个反代**。
// 服务直接暴露（默认监听 0.0.0.0）时，攻击者加一行 `X-Real-IP: 9.9.9.9`
// 就能冒充任意来源 IP，从而绕过全局 IP 白/黑名单、密钥 IP 白名单与 max_ips、
// 以及登录失败按 IP 锁定——全部 IP 类管控形同虚设。
//
// 取值优先级（按可信度）：
//  1. 仅当 **TCP 对端落在可信代理网段**（trustedCIDRs）时，才采信 X-Real-IP；
//  2. 同上条件下，从 X-Forwarded-For **右往左**数第 N 个（N = 可信跳数 hops）；
//     右侧是反代追加的真实地址，左侧才是可伪造部分；
//  3. TCP 对端地址（RemoteAddr，可信度最高，永远可回退）。
//
// **默认零配置 = 完全不信任转发头**（trustedCIDRs 空 → 恒取 RemoteAddr），
// 防伪造 X-Real-IP 绕过全部 IP 管控。若前面还挂了 CDN，请把 hops 调成
// CDN + 反代的层数，并把可信网段加上 CDN 的回源网段。
//
// 纯函数包：只依赖标准库 + net/netip，不含任何全局状态，便于单测与复用。
package iputil

import (
	"net/http"
	"net/netip"
	"strconv"
	"strings"
)

// ParseClientIP 从请求解析客户端 IP（剥掉端口后的纯地址字符串）。
//
// peer 是 TCP 对端地址（形如 "1.2.3.4:5678" 或 "[::1]:1234"，可直接传
// r.RemoteAddr）；trustedCIDRs 是可信代理网段列表（空 = 完全不信任转发头）；
// hops 是可信代理层数（XFF 从右往左取第 hops 个，<1 回落 1）。
// 解析失败（对端缺失/非法）返回空串，调用方按无来源处理。
func ParseClientIP(peer string, header func(name string) string, trustedCIDRs []netip.Prefix, hops int) string {
	if header == nil {
		header = func(string) string { return "" }
	}
	host := CleanAddress(peer)
	if !IsTrustedProxy(host, trustedCIDRs) {
		// 关键：对端不可信时，转发头一律不看（这正是参考项目被绕过的地方）。
		return host
	}
	// X-Real-IP 仅单跳可信（hops<=1）：nginx/caddy 的 proxy_set_header X-Real-IP
	// $remote_addr 写入的是**紧邻对端**——单层反代时恰好是真实客户端；多跳
	// （CDN+反代，hops>=2）时该头承载的是中间跳（CDN 回源 IP）而非客户端，
	// 且它优先于 XFF 短路，会把 hops 配置架空（黑白名单按 CDN 地址判定）。
	// 多跳场景一律走 XFF 从右往左穿透。
	if hops <= 1 {
		if real := CleanAddress(header("X-Real-IP")); real != "" {
			return real
		}
	}
	// X-Forwarded-For 从右往左数第 hops 跳：右侧是最近一跳反代追加的真实对端地址，
	// 左侧才是客户端可伪造部分。hops=1 取最右一个；超出左端（parts 不够长）回落最左。
	xff := header("X-Forwarded-For")
	if xff != "" {
		parts := make([]string, 0, 8)
		for _, p := range strings.Split(xff, ",") {
			if c := CleanAddress(p); c != "" {
				parts = append(parts, c)
			}
		}
		if n := len(parts); n > 0 {
			if hops < 1 {
				hops = 1
			}
			if idx := n - hops; idx >= 0 {
				return parts[idx]
			}
			return parts[0]
		}
	}
	return host
}

// IsTrustedProxy 报告 addr（已剥端口的 IP 字符串）是否落在可信代理网段内。
// 仅当对端确实是我们配置的反代（同机回环或指定私网/回源网段）时，才采信它
// 写入的转发头。非法地址恒 false（fail-closed）。
func IsTrustedProxy(addr string, trustedCIDRs []netip.Prefix) bool {
	if addr == "" || len(trustedCIDRs) == 0 {
		return false
	}
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		return false
	}
	ip = ip.Unmap() // IPv4-mapped IPv6（::ffff:1.2.3.4）按 v4 归一后再比对
	for _, p := range trustedCIDRs {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// CleanAddress 去掉地址两侧空白、端口与方括号，兼容形如 "[::1]:1234" 的
// host:port 复合形态。纯 IPv6（"::1"，冒号数 >1）不做拆分——net.SplitHostPort
// 对无端口 IPv6 会报错，且拆掉即毁地址。结果不做合法性校验（ParseAddr 在
// 匹配层兜底），仅做形态清洗。
func CleanAddress(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "[") {
		// [::1]:1234 → ::1（剥方括号壳；右括号缺失按畸形原样返回）
		if end := strings.IndexByte(s, ']'); end > 0 {
			return s[1:end]
		}
		return s
	}
	// IPv4:port / 单冒号形态去端口（host:port 恰好一个冒号且端口是数字）；
	// 端口非数字的怪值（如 "a:b:c" 形态已排除）按原样交给上层校验。
	if i := strings.LastIndexByte(s, ':'); i >= 0 && strings.Count(s, ":") == 1 {
		if _, err := strconv.Atoi(s[i+1:]); err == nil {
			return s[:i]
		}
	}
	return s
}

// ParseCIDRs 把字符串列表归一化为 netip.Prefix：去两侧空白、单 IP 自动补
// /32（v4）或 /128（v6）、IPv4-mapped IPv6 归一为等价 v4。非法条目**不静默
// 丢弃**：返回已解析的前缀与逐条错误清单（"条目: 原因"），调用方决定
// fail-fast 还是提示面板用户。空白/空串条目跳过（无意义输入不算错误）。
func ParseCIDRs(items []string) ([]netip.Prefix, []string) {
	var prefixes []netip.Prefix
	var errs []string
	for _, item := range items {
		s := strings.TrimSpace(item)
		if s == "" {
			continue
		}
		// 伪装成 CIDR 的裸 IPv6（"::1" 不带斜杠）直接走单 IP 分支，
		// ParsePrefix 会误判；先尝试 ParseAddr 兜住。
		if !strings.Contains(s, "/") {
			addr, err := netip.ParseAddr(s)
			if err != nil {
				errs = append(errs, s+": "+err.Error())
				continue
			}
			addr = normalizeAddr(addr)
			prefixes = append(prefixes, netip.PrefixFrom(addr, addr.BitLen()))
			continue
		}
		prefix, err := netip.ParsePrefix(s)
		if err != nil {
			errs = append(errs, s+": "+err.Error())
			continue
		}
		prefix = normalizePrefix(prefix)
		prefixes = append(prefixes, prefix)
	}
	return prefixes, errs
}

// normalizeAddr 归一单个地址：IPv4-mapped IPv6 → 纯 v4（同 IsTrustedProxy 口径）。
func normalizeAddr(addr netip.Addr) netip.Addr {
	return addr.Unmap()
}

// normalizePrefix 归一前缀：地址段 Unmap + bits 钳制（ParsePrefix 对 /33 已报错，
// 这里只兜 Unmap 后 v4 残留 v6 bits 的边缘形态）。
func normalizePrefix(p netip.Prefix) netip.Prefix {
	addr := p.Addr().Unmap()
	bits := p.Bits()
	if addr.Is4() && bits > 32 {
		bits = 32
	}
	return netip.PrefixFrom(addr, bits)
}

// ParseTrustedCIDRs 便捷入口：归一化失败即报错（启动期 fail-fast 用——
// 可信代理网段配错等于信任模型失效，静默降级不可接受）。
func ParseTrustedCIDRs(items []string) ([]netip.Prefix, error) {
	prefixes, errs := ParseCIDRs(items)
	if len(errs) > 0 {
		return nil, &CIDRError{Errors: errs}
	}
	return prefixes, nil
}

// CIDRError CIDR 归一化错误清单（多条一次报齐，便于面板一次性提示修正）。
type CIDRError struct {
	Errors []string
}

func (e *CIDRError) Error() string {
	return "非法的 IP / CIDR 条目: " + strings.Join(e.Errors, "; ")
}

// Matches 报告 ip（字符串）是否命中 cidr（字符串，支持单 IP 与 CIDR）。
// 两侧均做空白容忍与 Unmap 归一；任一侧非法恒 false（fail-closed：非法规则
// 永不匹配，绝不把"解析不了"误判成"放行"）。
func Matches(ip, cidr string) bool {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return false
	}
	return MatchesAddr(addr.Unmap(), strings.TrimSpace(cidr))
}

// MatchesAddr 同 Matches，接受已解析地址（规则引擎热路径复用，免二次解析）。
func MatchesAddr(addr netip.Addr, cidr string) bool {
	if !addr.IsValid() {
		return false
	}
	s := strings.TrimSpace(cidr)
	if s == "" {
		return false
	}
	if !strings.Contains(s, "/") {
		peer, err := netip.ParseAddr(s)
		if err != nil {
			return false
		}
		return addr == peer.Unmap()
	}
	prefix, err := netip.ParsePrefix(s)
	if err != nil {
		return false
	}
	return normalizePrefix(prefix).Contains(addr.Unmap())
}

// parsePeerAddr 从 host:port 复合地址剥出 IP（RemoteAddr 形态），解析失败返回零值。
// 供需要 netip.Addr 形态的调用方；普通路径经 CleanAddress + 字符串比对即可。
func parsePeerAddr(remoteAddr string) (netip.Addr, bool) {
	host := CleanAddress(remoteAddr)
	if host == "" {
		return netip.Addr{}, false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

// RemoteAddrIP r.RemoteAddr 的便捷解析（剥端口 + 合法性校验）。
// 供 panel 等已有 clientIP 的调用方逐步换轨（签名对齐既有用法）。
func RemoteAddrIP(remoteAddr string) string {
	if addr, ok := parsePeerAddr(remoteAddr); ok {
		return addr.String()
	}
	return ""
}

// RequestClientIP 从 *http.Request 解析客户端 IP（ParseClientIP 的请求形态封装）。
// trustedCIDRs 为 nil/空时严格回落 RemoteAddr（零配置 = 不信任转发头）。
func RequestClientIP(r *http.Request, trustedCIDRs []netip.Prefix, hops int) string {
	if r == nil {
		return ""
	}
	return ParseClientIP(r.RemoteAddr, r.Header.Get, trustedCIDRs, hops)
}

// LoopbackCIDRs 回环网段（127.0.0.0/8 与 ::1/128）：本机反代（nginx/caddy 同机
// 形态）的标准可信网段，供部署方拼入 trusted_proxy_cidrs。
func LoopbackCIDRs() []netip.Prefix {
	return []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("::1/128"),
	}
}

// PrivateCIDRs RFC1918/链路本地/ULA 私网网段（10/8、172.16/12、192.168/16、
// 169.254/16、fe80::/10）：内网反代形态的可信网段候选。**不含回环**——
// 需要时与 LoopbackCIDRs 并用。
func PrivateCIDRs() []netip.Prefix {
	return []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("fe80::/10"),
	}
}
