//go:build windows

package main

import (
	"context"
	"fmt"
	"net"
	"syscall"
)

// listenForServer 在 Windows 绑定 IPv6 通配地址时显式关闭 IPV6_V6ONLY。
// Windows 默认只收 IPv6，若不改，127.0.0.1 / IPv4 网卡完全不可达。
func listenForServer(listen string) (net.Listener, error) {
	lc := net.ListenConfig{}
	if wildcardIPv6(listen) {
		lc.Control = func(network, address string, c syscall.RawConn) error {
			if network != "tcp6" {
				return nil
			}
			var sockErr error
			if err := c.Control(func(fd uintptr) {
				sockErr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 0)
			}); err != nil {
				return err
			}
			if sockErr != nil {
				return fmt.Errorf("set IPV6_V6ONLY=0: %w", sockErr)
			}
			return nil
		}
	}
	return lc.Listen(context.Background(), "tcp", listen)
}
