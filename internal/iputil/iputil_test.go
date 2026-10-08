// iputil_test.go 可信代理感知 IP 解析的单测：信任边界（对端不在可信网段 =
// 转发头一律不看）、XFF 右往左 N 跳、X-Real-IP 优先、IPv6 剥壳、CIDR 归一化。
// 信任模型的每一条都在防一个真实攻击面（伪造头绕过 IP 管控），测的是安全
// 契约不是实现细节。
package iputil

import (
	"net/netip"
	"testing"
)

// loopback 测试用可信网段：同机反代形态。
var loopback = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
}

// hdrs 构造 header 取值闭包（模拟 r.Header.Get）。
func hdrs(m map[string]string) func(string) string {
	return func(name string) string { return m[name] }
}

// TestParseClientIPUntrustedPeerIgnoresHeaders 信任边界核心：对端不在可信网段时，
// X-Real-IP / X-Forwarded-For 一律不采信，恒取 RemoteAddr。伪造头攻击的直接反制。
func TestParseClientIPUntrustedPeerIgnoresHeaders(t *testing.T) {
	cases := []struct {
		name    string
		peer    string
		headers map[string]string
	}{
		{"fake x-real-ip ignored", "203.0.113.7:443", map[string]string{"X-Real-IP": "9.9.9.9"}},
		{"fake xff ignored", "203.0.113.7:443", map[string]string{"X-Forwarded-For": "9.9.9.9, 8.8.8.8"}},
		{"both fakes ignored", "198.51.100.1:1234", map[string]string{"X-Real-IP": "1.1.1.1", "X-Forwarded-For": "2.2.2.2"}},
		{"no headers falls back to peer", "203.0.113.7:443", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseClientIP(tc.peer, hdrs(tc.headers), loopback, 1)
			want := CleanAddress(tc.peer)
			if got != want {
				t.Fatalf("ParseClientIP=%q want %q (untrusted peer must ignore forwarded headers)", got, want)
			}
		})
	}
	// 零配置（无可信网段）= 完全不信任转发头，即便对端是回环也直接取对端。
	got := ParseClientIP("127.0.0.1:5000", hdrs(map[string]string{"X-Real-IP": "9.9.9.9"}), nil, 1)
	if got != "127.0.0.1" {
		t.Fatalf("empty trustedCIDRs must distrust headers even for loopback peer, got %q", got)
	}
}

// TestParseClientIPTrustedProxy 可信代理路径：采信 X-Real-IP 优先、XFF 次之、
// 都缺回落对端。
func TestParseClientIPTrustedProxy(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"x-real-ip wins over xff", map[string]string{"X-Real-IP": "9.9.9.9", "X-Forwarded-For": "8.8.8.8, 7.7.7.7"}, "9.9.9.9"},
		{"xff only", map[string]string{"X-Forwarded-For": "8.8.8.8, 7.7.7.7"}, "7.7.7.7"}, // hops=1 取最右
		{"empty headers falls back to peer", nil, "127.0.0.1"},
		{"garbage headers falls back to peer", map[string]string{"X-Real-IP": "", "X-Forwarded-For": " , ,"}, "127.0.0.1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseClientIP("127.0.0.1:5000", hdrs(tc.headers), loopback, 1)
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

// TestParseClientIPXFFHops XFF 从右往左数第 hops 跳（N=可信代理层数）：
// CDN + 反代双跳形态，hops=2 应穿透到 CDN 之前那跳。
func TestParseClientIPXFFHops(t *testing.T) {
	xff := "1.1.1.1, 2.2.2.2, 3.3.3.3"
	tests := []struct {
		hops int
		want string
	}{
		{1, "3.3.3.3"}, // 单跳反代：最右（反代追加的对端）
		{2, "2.2.2.2"}, // CDN+反代：穿透一层
		{3, "1.1.1.1"},
		{99, "1.1.1.1"}, // 超出左端回落最左（最左是客户端可伪造边界，再往左没有了）
		{0, "3.3.3.3"},  // <1 回落 1
	}
	for _, tc := range tests {
		got := ParseClientIP("127.0.0.1:5000", hdrs(map[string]string{"X-Forwarded-For": xff}), loopback, tc.hops)
		if got != tc.want {
			t.Fatalf("hops=%d got %q want %q", tc.hops, got, tc.want)
		}
	}
	// 空白容忍：XFF 条目带杂散空白照常解析。
	got := ParseClientIP("127.0.0.1:5000", hdrs(map[string]string{"X-Forwarded-For": " 5.5.5.5 ,  6.6.6.6"}), loopback, 1)
	if got != "6.6.6.6" {
		t.Fatalf("whitespace-tolerant xff: got %q", got)
	}
}

// TestParseClientIPIPv6 IPv6 剥壳与 v6 可信网段匹配。
func TestParseClientIPIPv6(t *testing.T) {
	// [::1]:1234 → ::1；v6 回环在可信网段内 → 采信 X-Real-IP。
	got := ParseClientIP("[::1]:1234", hdrs(map[string]string{"X-Real-IP": "2001:db8::1"}), loopback, 1)
	if got != "2001:db8::1" {
		t.Fatalf("ipv6 bracketed peer: got %q", got)
	}
	// 不可信 v6 对端：忽略头。
	got = ParseClientIP("[2001:db8::50]:443", hdrs(map[string]string{"X-Real-IP": "9.9.9.9"}), loopback, 1)
	if got != "2001:db8::50" {
		t.Fatalf("untrusted ipv6 peer: got %q", got)
	}
	// 纯 IPv6 无端口：不拆分（冒号 >1）。
	if got := CleanAddress("2001:db8::1"); got != "2001:db8::1" {
		t.Fatalf("bare ipv6 must not be split: %q", got)
	}
	// IPv4-mapped IPv6 归一为 v4 后匹配 v4 可信网段。
	mapped := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	got = ParseClientIP("[::ffff:10.1.2.3]:8080", hdrs(nil), mapped, 1)
	if got != "::ffff:10.1.2.3" {
		t.Fatalf("mapped peer keeps literal form: %q", got)
	}
	if !IsTrustedProxy("::ffff:10.1.2.3", mapped) {
		t.Fatal("mapped v6 addr must match v4 cidr after Unmap")
	}
}

// TestCleanAddress 剥端口/方括号/空白的形态清洗。
func TestCleanAddress(t *testing.T) {
	cases := map[string]string{
		"1.2.3.4:5678":   "1.2.3.4",
		" 1.2.3.4:5678 ": "1.2.3.4",
		"[::1]:1234":     "::1",
		"[::1]":          "::1",
		"::1":            "::1",
		"2001:db8::1":    "2001:db8::1",
		"1.2.3.4":        "1.2.3.4",
		"":               "",
		"   ":            "",
		"[broken":        "[broken", // 畸形原样返回（上层校验兜底）
	}
	for in, want := range cases {
		if got := CleanAddress(in); got != want {
			t.Errorf("CleanAddress(%q)=%q want %q", in, got, want)
		}
	}
}

// TestIsTrustedProxy 可信网段匹配：命中/未命中/非法地址 fail-closed。
func TestIsTrustedProxy(t *testing.T) {
	cidrs := []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("2001:db8::/32"),
	}
	if !IsTrustedProxy("127.0.0.1", cidrs) || !IsTrustedProxy("10.255.0.1", cidrs) {
		t.Fatal("in-range addrs must be trusted")
	}
	if !IsTrustedProxy("2001:db8::dead", cidrs) {
		t.Fatal("ipv6 in-range must be trusted")
	}
	if IsTrustedProxy("192.168.1.1", cidrs) {
		t.Fatal("out-of-range must not be trusted")
	}
	if IsTrustedProxy("not-an-ip", cidrs) || IsTrustedProxy("", cidrs) {
		t.Fatal("invalid/empty addr must be untrusted (fail-closed)")
	}
	if IsTrustedProxy("127.0.0.1", nil) {
		t.Fatal("empty cidr list must trust nobody")
	}
}

// TestParseCIDRs 归一化：空白、单 IP 补位、v4-mapped、非法清单。
func TestParseCIDRs(t *testing.T) {
	prefixes, errs := ParseCIDRs([]string{" 10.0.0.0/8 ", " 1.2.3.4", "::1", "::ffff:192.168.0.0/16", "", "   ", "not-a-cidr", "10.0.0.0/33"})
	want := []string{"10.0.0.0/8", "1.2.3.4/32", "::1/128", "192.168.0.0/16"}
	if len(errs) != 2 {
		t.Fatalf("errs=%v want exactly 2 (not-a-cidr + bad bits)", errs)
	}
	if len(prefixes) != len(want) {
		t.Fatalf("prefixes=%v want %d entries", prefixes, len(want))
	}
	for i, p := range prefixes {
		if p.String() != want[i] {
			t.Errorf("prefixes[%d]=%s want %s", i, p, want[i])
		}
	}
	// 归一化后的条目照常匹配。
	if !Matches("1.2.3.4", "1.2.3.4") || !Matches("::ffff:1.2.3.4", "1.2.3.4") {
		t.Fatal("normalized single-IP entries must match")
	}
}

// TestParseTrustedCIDRsFailFast 启动期 fail-fast 语义：任一条非法整体报错。
func TestParseTrustedCIDRsFailFast(t *testing.T) {
	if _, err := ParseTrustedCIDRs([]string{"10.0.0.0/8", "oops"}); err == nil {
		t.Fatal("invalid entry must fail the whole list (fail-fast)")
	} else if ce, ok := err.(*CIDRError); !ok || len(ce.Errors) != 1 {
		t.Fatalf("want *CIDRError with 1 entry, got %v (%T)", err, err)
	}
	if _, err := ParseTrustedCIDRs([]string{"10.0.0.0/8"}); err != nil {
		t.Fatalf("valid list must pass: %v", err)
	}
}

// TestMatches 单 IP 与 CIDR 匹配、空白容忍、非法 fail-closed。
func TestMatches(t *testing.T) {
	cases := []struct {
		ip, cidr string
		want     bool
	}{
		{"1.2.3.4", "1.2.3.4", true},
		{"1.2.3.4", "1.2.3.0/24", true},
		{"1.2.4.4", "1.2.3.0/24", false},
		{" 1.2.3.4 ", " 1.2.3.0/24 ", true},    // 存量库可能带空白条目
		{"::1", "::1", true},                   // 裸 IPv6 不是 CIDR
		{"2001:db8::5", "2001:db8::/32", true}, // v6 CIDR
		{"::ffff:1.2.3.4", "1.2.3.0/24", true}, // mapped 归一
		{"bad", "1.2.3.0/24", false},
		{"1.2.3.4", "bad", false},
		{"1.2.3.4", "", false},
	}
	for _, tc := range cases {
		if got := Matches(tc.ip, tc.cidr); got != tc.want {
			t.Errorf("Matches(%q,%q)=%v want %v", tc.ip, tc.cidr, got, tc.want)
		}
	}
}

// TestRemoteAddrIP RemoteAddr 便捷解析。
func TestRemoteAddrIP(t *testing.T) {
	if got := RemoteAddrIP("192.168.1.5:12345"); got != "192.168.1.5" {
		t.Fatalf("got %q", got)
	}
	if got := RemoteAddrIP("[fe80::1]:80"); got != "fe80::1" {
		t.Fatalf("got %q", got)
	}
	if got := RemoteAddrIP("garbage"); got != "" {
		t.Fatalf("invalid addr must yield empty, got %q", got)
	}
}

// TestRequestClientIP 请求形态封装：nil 请求安全、空可信列表严格回落。
func TestRequestClientIP(t *testing.T) {
	if RequestClientIP(nil, loopback, 1) != "" {
		t.Fatal("nil request must yield empty")
	}
}
