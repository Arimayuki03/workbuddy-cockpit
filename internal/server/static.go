// static.go manager 前端壳的静态托管（v1.2.0 设计文档 §4.2 / §8）。
//
// 挂载：NewHandler 在 cfg.Static 非 nil 时注册到 mux 根（"/"）——ServeMux 的
// 最长前缀匹配保证 /v1/*、/admin/*、/status、/api/request_logs、/api/system/
// check-update 等已注册路由优先于根，静态 handler 只兜住其余路径。
//
// 路径语义（Next.js trailingSlash 静态导出）：
//   - 精确文件命中（/app.js、/_next/xxx.js、/dashboard/ → 物理文件
//     dashboard/index.html）→ http.FileServer 直接服务；
//   - 无尾斜杠目录名（/dashboard）→ FileServer 自动 301 补斜杠，再走上一条；
//   - "/" → 根 index.html（FileServer 目录索引语义）；
//   - 找不到 → 404 页（内嵌 not-found.html 优先，缺失回落 index.html——Next.js
//     静态导出的客户端路由由前端接管未知路径，深链刷新不白屏）。
//
// 安全头（CSP/X-Frame-Options/X-Content-Type-Options/Referrer-Policy）在静态
// handler 统一写，与 /panel/api/*、/api/* 响应同一组，覆盖 manager 壳全部页面。
package server

import (
	"io"
	"net/http"
	"strings"
)

// staticHandler 前端静态托管 handler。
type staticHandler struct {
	root http.FileSystem
	// fileServer 复用 stdlib 语义：目录 301 补斜杠、目录索引 index.html、
	// ETag/Last-Modified/Range/条件请求全套。
	fileServer http.Handler
}

// ServeHTTP 实现 http.Handler：见文件头注释的路径语义。
func (h staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setStaticSecurityHeaders(w)

	// 防路径穿越：含 ".." 段的原始路径直接 404（规范化交给 FileServer；
	// 这里只做拒绝判定，不改写 URL，保持深链语义）。
	if strings.Contains(r.URL.Path, "..") {
		http.NotFound(w, r)
		return
	}

	// 已知 API 前缀显式 404：静态 handler 兜在根路径，Admin.Enabled=false /
	// Panel=nil 的部署里 /admin/*、/api/*、未注册的 /v1/*（如方法不匹配）不会
	// 有更具体的 mux 条目命中——若回落 not-found.html 会伪装成 200 HTML 页，
	// 掩盖 API 404/405 语义。这里显式 404，保证 API 路径的错误形态可被客户端识别。
	if strings.HasPrefix(r.URL.Path, "/v1/") || strings.HasPrefix(r.URL.Path, "/admin/") ||
		strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}

	// 精确命中探测：路径存在（文件或目录）→ 交 FileServer（301 补斜杠 /
	// 目录索引 / 静态文件服务全套 stdlib 语义）。
	// 注意 http.FS 的坑：Open("/login/")（尾斜杠）报 invalid argument——
	// 探测前必须剥掉尾斜杠（根 "/" 单独处理）；而 FileServer 收到的仍是
	// 原始 URL，目录索引 / 301 补斜杠语义不受影响（2026-09-21 实测定位）。
	probePath := strings.TrimSuffix(r.URL.Path, "/")
	if probePath == "" {
		probePath = "/" // 根路径本身：Open("/") 在 http.FS 下合法（返回目录）
	}
	if probePath == "/" || func() bool {
		f, err := h.root.Open(probePath)
		if err != nil {
			return false
		}
		f.Close()
		return true
	}() {
		h.fileServer.ServeHTTP(w, r)
		return
	}

	// 未命中 → not-found.html 优先（Next.js 404 导出物），缺失回落 index.html。
	if h.serveFile(w, r, "/not-found.html") || h.serveFile(w, r, "/index.html") {
		return
	}
	http.NotFound(w, r)
}

// serveFile 以 200 服务指定文件（io.ReadSeeker 经 ServeContent 获得完整
// 条件请求语义）；不存在返回 false。
func (h staticHandler) serveFile(w http.ResponseWriter, r *http.Request, name string) bool {
	f, err := h.root.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		return false
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		return false
	}
	http.ServeContent(w, r, name, st.ModTime(), rs)
	return true
}

// setStaticSecurityHeaders 静态资源的统一安全响应头（与 panel API 同组）。
// script-src 必须放行 'unsafe-inline'：Next.js App Router 静态导出把 RSC payload
// 以多条内联 <script>self.__next_f.push(...)</script> 内嵌（加 next-themes 引导块），
// 缺它则内联脚本被整体拦截，React 水合无初始数据 → 页面白屏（2026-09-21 实测）。
// 注入面由 frame-ancestors/base-uri/self 兜住。
func setStaticSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; "+
		"style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; "+
		"form-action 'none'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}

// registerStatic 把前端静态 FS 挂到根路径（NewHandler 装配期调用）。
// static 为 nil 时不注册（未嵌入前端构建的默认构建）。
func registerStatic(h *Handler, static http.FileSystem) {
	if static == nil {
		return
	}
	h.mux.Handle("/", staticHandler{root: static, fileServer: http.FileServer(static)})
}
