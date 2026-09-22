// bootstrap.go 首启引导：发行 exe 双击即可用。
//
// 此前 config.json 不存在时走 Load("")（纯默认 + env），随后必然在 api_key 必填
// 校验上 fatal——而 Windows 双击场景控制台窗口随进程退出关闭，用户只见「闪退」。
// 现改为：配置文件缺失时自动生成一份最小 config.json（随机 api_key），并把密钥
// 打印给用户（面板登录密码 = api_key）。显式 -config 指向的文件缺失同样适用
//（docker/CI 的 cp 流程不受影响：文件存在则零行为变化）。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// bootstrapConfig 配置文件不存在时生成最小配置并落盘。返回生成的 api_key。
// 生成失败（目录不可写等）返回空串，调用方按原路径报错退出——错误文案里
// 附带 absPath，用户至少能看清是哪个文件没写成。
func bootstrapConfig(path string) string {
	key := generateAPIKey()
	// Default() 已含全部默认值；仅填 api_key 与用户第一眼需要的注释性字段。
	// JSON 无注释，说明放进生成的 README 提示与 fatal 文案。
	c := Default()
	c.APIKey = key
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return ""
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return ""
	}
	// 0600：文件含鉴权密钥，与其他用户隔离。
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return ""
	}
	return key
}

// generateAPIKey 256bit 随机密钥（crypto/rand，hex 编码）。失败回落空串由
// 调用方 fatal——密钥是安全边界，绝不允许静默用固定值。
func generateAPIKey() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return "wb-" + hex.EncodeToString(b[:])
}

// printFirstRunBanner 首启生成配置后的引导输出（stdout，重定向也留痕）。
func printFirstRunBanner(path, key string) {
	fmt.Fprintf(os.Stderr, `
==========================================================
 首次启动：已生成配置文件 %s
 随机 api_key（面板登录密码 / 客户端 Bearer 密钥）：

     %s

 请立即抄写保存。客户端与面板均使用此密钥。
 后续可在此文件或面板「设置」页修改。
==========================================================
`, path, key)
	log.Printf("bootstrap: 已生成 %s（随机 api_key）", path)
}
