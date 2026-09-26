// listen.go listen 地址归一化（cmd/stats 与 cmd/acct 共用的单一实现）。
//
// 起因（审查发现 12）：两命令曾各持一份逐字相同的 normalizeListen 复制品，
// IPv6 等边界一旦走样只有一处被测试抓住。抽到本包单份实现，两命令共用。
package config

import (
	"net"
	"strconv"
	"strings"
)

// NormalizeListen 把配置里的 listen（":7863" / "0.0.0.0:7863" / "127.0.0.1:7863"）
// 归一成本机可访问的 http 基址。监听通配地址时收敛到回环——运维工具总是和网关
// 同机运行，往 0.0.0.0 / :: 发请求在部分平台会直接失败。
//
// 用 net.SplitHostPort 而非手工切冒号：IPv6 字面量（"::" / "[::]:7863"）本身含冒号，
// `strings.LastIndex(listen, ":")` 会把 "::" 切成 host=":" port=""，拼出
// "http://::7863" 这种非法基址（测试抓到过）。
func NormalizeListen(listen string) string {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return "http://127.0.0.1:7863"
	}
	host, port := "", ""
	if h, p, err := net.SplitHostPort(listen); err == nil {
		host, port = h, p
	} else {
		// 无冒号（"7863"）或畸形：把纯数字整体当端口，否则当 host。
		if _, convErr := strconv.Atoi(listen); convErr == nil {
			port = listen
		} else {
			// SplitHostPort 失败也可能是 "[::]:x" 这类缺端口的写法，退一步处理。
			host = strings.Trim(strings.TrimSuffix(listen, ":"), "[]")
		}
	}
	if port == "" {
		port = "7863"
	}
	switch host {
	// ":" 是裸 "::" 经 SplitHostPort 的产物（Go 把 "::" 解析为 host=":"）；
	// 这些写法都表示「监听全部网卡」，统一收敛到回环。
	case "", "0.0.0.0", "::", ":":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}
