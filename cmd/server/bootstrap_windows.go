//go:build windows

// bootstrap_windows.go 双击发行 exe 的「报错可见性」兜底：
// Windows 双击 exe 得到的是真控制台，此前任何 log.Fatalf 都让进程立即退出——
// 控制台窗口随进程关闭，错误一闪而过，用户只能看到「闪退」。真控制台交互
// （stdin/stdout 均未重定向）下，fatal 出口打印醒目错误并等一次回车再退出；
// 脚本/重定向场景（dev.sh、start-*.cmd、docker、CI、计划任务）按无人值守处理，
// 行为与原 log.Fatalf 完全一致。
package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
)

// interactiveConsole 真控制台交互（双击/手动 cmd 运行）判定：stdin 与 stdout
// 必须同时是字符设备。任一端被重定向（日志文件、管道、RedirectStandardOutput、
// 计划任务）即视为无人值守。ModeCharDevice 无法区分控制台与 NUL 设备，但脚本
// 场景 stdout 必被重定向（非字符设备），NUL+控制台组合仅人为构造可及，风险
// 可接受且不引入 kernel32/unsafe。
func interactiveConsole() bool {
	return isCharDevice(os.Stdin) && isCharDevice(os.Stdout)
}

func isCharDevice(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// fatalExit 统一 fatal 出口（替代散落各处的 log.Fatalf）：非交互仅记日志即退
// （退出码 1，与 log.Fatalf 一致）；交互控制台额外打印醒目错误块并等待回车，
// 让用户来得及看清死因。
func fatalExit(format string, args ...any) {
	log.Printf(format, args...)
	if interactiveConsole() {
		fmt.Fprintf(os.Stderr, "\n[启动失败] "+format+"\n", args...)
		fmt.Fprint(os.Stderr, "\n按回车键退出...")
		bufio.NewScanner(os.Stdin).Scan()
	}
	os.Exit(1)
}
