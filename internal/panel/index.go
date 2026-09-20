// index.go 面板安全响应头（v1.2.0：panel 原生 index.html/app.js 已删——
// 被 manager 前端壳替代，静态资源由 internal/panel/embed*.go + internal/server
// 的 static.go 托管）。本文件只保留统一安全头：
//
// 安全头对"全部 /panel/api/* 与 /api/* 响应"生效（manager 壳的静态资源由
// static handler 另行写入同一组头）：CSP 限制脚本只能来自本服务，禁止被
// iframe 嵌套（防点击劫持），禁 MIME 嗅探，并声明不泄露 Referer 出去。
package panel

import (
	"net/http"
)

// csp 内容安全策略（严格版，无需 unsafe-inline）：
//   - default-src 'self'        前端是 Next.js 静态导出（多 chunk + 样式），按同源自治理
//   - script-src 'self'         只跑同源脚本；manager 壳无内联脚本依赖
//   - style-src 'self' 'unsafe-inline'
//     shadcn/tailwind 运行时少量内联样式；允许内联样式不会导致脚本执行
//   - connect-src 'self'        前端 fetch 只能打本服务
//   - img-src 'self' data:      图标/内联图/QR
//   - form-action 'none'        页面无表单提交目标（设置页是 JS 提交）
//   - frame-ancestors 'none'    禁止被任何站点 iframe 嵌套（点击劫持）
//   - base-uri 'none'          禁止注入 <base> 改写相对路径
const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; img-src 'self' data:; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// setSecurityHeaders 写入面板统一安全响应头（页面与 API 都要，API 也含 JSON 数据）。
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff") // 禁 MIME 嗅探
	w.Header().Set("X-Frame-Options", "DENY")           // 老浏览器兜底（CSP frame-ancestors 的等价项）
	w.Header().Set("Referrer-Policy", "no-referrer")    // 不外泄面板地址给外部站点
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}
