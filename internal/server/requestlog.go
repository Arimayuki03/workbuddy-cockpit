package server

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// requestLogEntry 单条请求日志（面板「请求日志」tab 的行）。
// 字段与 chatStat 观测同源：模型/账号/状态/tokens/首字延迟/扣费/错误。
type requestLogEntry struct {
	Seq       int64     `json:"seq"`
	Time      time.Time `json:"time"`
	Model     string    `json:"model"`
	UID       string    `json:"uid,omitempty"`
	Nick      string    `json:"nick,omitempty"`
	Mode      string    `json:"mode"` // "stream" | "sync"
	Status    int       `json:"status"`
	Tokens    int     `json:"tokens"`    // <0 = usage 缺失（观测缺失，非 0 token）
	TTFBMS    int64   `json:"ttfb_ms"`   // 首字延迟（非流式/无帧 = 0）
	Credit    float64 `json:"credit"`    // 扣费（hasCredit=false 时无观测）
	HasCredit bool    `json:"has_credit"`
	Error     string  `json:"error,omitempty"` // 非 200 的原因摘要
}

// requestLogCap 环形缓冲容量（设计文档 §4.3.2：~1000 条，重启即清）。
// 持久化由 usage 分桶承担，二者不重叠。
const requestLogCap = 1000

// requestLogStore 请求日志环形缓冲：单写（chatStat.done() 单一埋点）多读
// （/api/request_logs 轮询）。mutex 保护切片+游标；无锁竞争热点（写频率=请求频率）。
type requestLogStore struct {
	mu      sync.Mutex
	buf     []requestLogEntry // 定长环形
	next    int               // 下一个写位置
	seq     int64             // 全局递增序号（跨重启清零，仅运行期单调）
	dropped int64             // 被覆盖的旧条数（观测用）
}

var requestLog = &requestLogStore{buf: make([]requestLogEntry, requestLogCap)}

// appendRequestLog 写入一条请求日志（覆盖最旧）。由 chatStat.done() 调用（单一埋点）。
func appendRequestLog(e requestLogEntry) {
	requestLog.mu.Lock()
	defer requestLog.mu.Unlock()
	requestLog.seq++
	e.Seq = requestLog.seq
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	requestLog.buf[requestLog.next] = e
	requestLog.next = (requestLog.next + 1) % len(requestLog.buf)
	if requestLog.seq > int64(len(requestLog.buf)) {
		requestLog.dropped = requestLog.seq - int64(len(requestLog.buf))
	}
}

// requestLogsSnapshot 返回最新的 n 条（时间倒序：最新在前）；n<=0 取全部。
func requestLogsSnapshot(n int) []requestLogEntry {
	requestLog.mu.Lock()
	defer requestLog.mu.Unlock()
	total := len(requestLog.buf)
	if requestLog.seq < int64(total) {
		total = int(requestLog.seq) // 未写满：只取已写部分
	}
	if n > 0 && n < total {
		total = n
	}
	out := make([]requestLogEntry, 0, total)
	// 从最新往回走：next-1 是最后写入位。
	for i := 0; i < total; i++ {
		idx := (requestLog.next - 1 - i + len(requestLog.buf)) % len(requestLog.buf)
		out = append(out, requestLog.buf[idx])
	}
	return out
}

// handleRequestLogs GET /api/request_logs?limit=N（挂进 Handler mux；鉴权同 /v1/stats）。
func (h *Handler) handleRequestLogs(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
			if limit > requestLogCap {
				limit = requestLogCap
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":   requestLogsSnapshot(limit),
		"dropped": requestLog.dropped,
		"cap":     requestLogCap,
	})
}

// requestLogPayloadFromStat 从 chatStat 抽取请求日志载荷（与表格日志同一观测源）。
// 前置条件：done() 已把 uid/nick/ttfb 等字段填齐。
func requestLogPayloadFromStat(s *chatStat) requestLogEntry {
	return requestLogEntry{
		Time:      time.Now(),
		Model:     s.model,
		UID:       s.uid,
		Nick:      s.nick,
		Mode:      s.mode,
		Status:    s.status,
		Tokens:    s.toks,
		TTFBMS:    s.ttfb.Milliseconds(),
		Credit:    s.credit,
		HasCredit: s.hasCredit,
		Error:     s.errSummary,
	}
}
