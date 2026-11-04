//go:build !windows

package main

import "net"

// listenForServer 在非 Windows 平台沿用 Go 默认 TCP 监听语义；Linux/macOS
// 对 IPv6 通配监听默认已是双栈（或由系统 sysctl 决定）。
func listenForServer(listen string) (net.Listener, error) {
	return net.Listen("tcp", listen)
}
