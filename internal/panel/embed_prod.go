//go:build embed_panel

// embed_prod.go 生产构建的前端静态产物内嵌（-tags embed_panel 时编译本文件）。
//
// 构建链（设计文档 §7 三段构建）：Node（web/ npm run build:export 产出静态导出）
// → 把导出产物拷入本目录（internal/panel/dist/）→ `go build -tags embed_panel ./cmd/server`。
// dist 缺 index.html 时本文件编译即报错（fail-fast：嵌入残缺的前端不如编译失败明确）。
package panel

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// DistFS 返回前端静态导出产物的文件系统（以 dist/ 为根：index.html、*_next/、
// dashboard/、accounts/ 等）。供 server.NewHandler 的 Static 字段挂根路径。
// 生产构建路径：dist 目录存在且内含 index.html。
func DistFS() (fs.FS, bool) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil, false
	}
	return sub, true
}

// HasEmbeddedFrontend 报告本次构建是否内嵌了前端（embed_panel 标签构建恒 true）。
func HasEmbeddedFrontend() bool { return true }
