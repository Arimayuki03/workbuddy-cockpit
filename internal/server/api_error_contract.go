package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
)

// gatewayAPIPath 报告路径是否属于对外网关协议面。这些路径的兜底错误必须是
// OpenAI 形状 JSON，不能回落静态站点的 HTML/纯文本错误页。
func gatewayAPIPath(path string) bool {
	return path == "/v1" || path == "/v2" ||
		strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/v2/")
}

// gatewayErrorWriter 只在 /v1、/v2 的 404/405 响应没有 JSON Content-Type 时
// 接管响应体。显式 JSON 错误（如 404 model_not_found）原样透传；SSE 200 在
// WriteHeader 时直接放行，不增加缓冲。
type gatewayErrorWriter struct {
	w          http.ResponseWriter
	path       string
	statusCode int
	wrote      bool
	intercept  bool
	done       bool
}

func (w *gatewayErrorWriter) Header() http.Header { return w.w.Header() }

func (w *gatewayErrorWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.statusCode = status
	ct := strings.ToLower(w.Header().Get("Content-Type"))
	if (status == http.StatusNotFound || status == http.StatusMethodNotAllowed) &&
		!strings.Contains(ct, "application/json") {
		w.intercept = true
		return
	}
	w.w.WriteHeader(status)
}

func (w *gatewayErrorWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if w.intercept {
		return len(p), nil
	}
	return w.w.Write(p)
}

func (w *gatewayErrorWriter) finish() {
	if !w.intercept || w.done {
		return
	}
	w.done = true
	h := w.Header()
	h.Del("Content-Length")
	h.Set("Content-Type", "application/json")
	code := "not_found"
	msg := "unknown gateway API path: " + w.path
	if w.statusCode == http.StatusMethodNotAllowed {
		code = "method_not_allowed"
		msg = "method not allowed for gateway API path: " + w.path
	}
	raw, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "invalid_request_error",
			"code":    code,
		},
	})
	w.w.WriteHeader(w.statusCode)
	_, _ = w.w.Write(raw)
}

func (w *gatewayErrorWriter) Flush() {
	if w.intercept {
		w.finish()
	}
	if f, ok := w.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *gatewayErrorWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.w.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

func (w *gatewayErrorWriter) Push(target string, opts *http.PushOptions) error {
	p, ok := w.w.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return p.Push(target, opts)
}

func (w *gatewayErrorWriter) Unwrap() http.ResponseWriter { return w.w }

func (w *gatewayErrorWriter) ReadFrom(r io.Reader) (int64, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if w.intercept {
		return io.Copy(io.Discard, r)
	}
	if rf, ok := w.w.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(w.w, r)
}
