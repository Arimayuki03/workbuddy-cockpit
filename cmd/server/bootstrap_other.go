//go:build !windows

// bootstrap_other.go 非 Windows 平台的 fatal 出口：无「双击闪退」问题（终端/
// 容器日志里错误始终可见），与原 log.Fatalf 行为一致。
package main

import "log"

func fatalExit(format string, args ...any) {
	log.Fatalf(format, args...)
}
