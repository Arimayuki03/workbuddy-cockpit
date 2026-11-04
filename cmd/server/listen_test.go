package main

import (
	"net"
	"strconv"
	"testing"
	"time"
)

func TestWildcardIPv6(t *testing.T) {
	cases := map[string]bool{
		"::":           true,
		"[::]:7863":    true,
		"0.0.0.0:7863": false,
		"127.0.0.1:1":  false,
		":7863":        false,
	}
	for in, want := range cases {
		if got := wildcardIPv6(in); got != want {
			t.Errorf("wildcardIPv6(%q)=%v want %v", in, got, want)
		}
	}
}

// Windows 默认 IPV6_V6ONLY=1；绑定 [::] 后必须仍能从 IPv4 回环连接。
func TestListenIPv6WildcardAcceptsIPv4(t *testing.T) {
	ln, err := listenForServer("[::]:0")
	if err != nil {
		t.Skipf("IPv6 unavailable: %v", err)
	}
	defer ln.Close()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr=%T", ln.Addr())
	}
	conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(addr.Port)), 2*time.Second)
	if err != nil {
		t.Fatalf("IPv4 无法连接 IPv6 双栈监听: %v", err)
	}
	_ = conn.Close()
}
