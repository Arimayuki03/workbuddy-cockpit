//go:build !embed_panel

// embed_stub.go 默认构建（无 embed_panel 标签）的前端占位。
//
// fresh-clone 未构建前端时的行为（设计文档 §7 fail-fast 约定的工程折衷）：目录
// internal/panel/dist/ 以占位 README 进仓库，`go build ./...` 不因缺前端产物而失败
// （本 stub 提供空 FS）；真正的前端嵌入走 `-tags embed_panel`（embed_prod.go）。
// 运行期根路径返回提示文案，网关与面板 API 全部照常可用。
package panel

import (
	"io/fs"
)

// DistFS 返回空 FS（未嵌入前端）。第二返回值 false 供调用方判定降级：
// main 据此不注册静态托管路由，由 static.go 的占位 handler 兜底根路径。
func DistFS() (fs.FS, bool) {
	return nil, false
}

// HasEmbeddedFrontend 报告本次构建是否内嵌了前端（默认构建 false：提示先跑前端
// 构建再以 `-tags embed_panel` 编译）。
func HasEmbeddedFrontend() bool { return false }
