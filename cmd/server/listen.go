package main

import (
	"net"
	"strings"
)

// wildcardIPv6 报告 listen 是否绑定 IPv6 通配地址（"::" / "[::]:port"）。
func wildcardIPv6(listen string) bool {
	listen = strings.TrimSpace(listen)
	if listen == "::" {
		return true
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	return host == "::"
}
