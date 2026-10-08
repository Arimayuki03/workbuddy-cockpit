package server

import (
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/iputil"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/reqlog"
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
	Tokens    int       `json:"tokens"`  // <0 = usage 缺失（观测缺失，非 0 token）
	TTFBMS    int64     `json:"ttfb_ms"` // 首字延迟（非流式/无帧 = 0）
	Credit    float64   `json:"credit"`  // 扣费（hasCredit=false 时无观测）
	HasCredit bool      `json:"has_credit"`
	// 缓存三段（上游 usage 的 prompt_cache_*_tokens）：HasCacheObs=false 表示本次
	// 无 usage 观测（缺观测 ≠ 命中 0，前端不显示标记）；true 时命中/未命中可全 0。
	CacheHit    int    `json:"cache_hit_tokens,omitempty"`
	CacheMiss   int    `json:"cache_miss_tokens,omitempty"`
	CacheWrite  int    `json:"cache_write_tokens,omitempty"`
	HasCacheObs bool   `json:"has_cache_obs,omitempty"`
	Error       string `json:"error,omitempty"` // 非 200 的原因摘要
	// ClientIP / UserAgent 调用来源（可选）：受 logging.request_client_info 开关
	// 控制（默认 false 不采集，保持零值即不落内存快照、不进 JSONL 归档——
	// omitempty 序列化时缺省）。采集口径见 newChatStat 的 TODO（iputil 接线）。
	ClientIP  string `json:"client_ip,omitempty"`
	UserAgent string `json:"user_agent,omitempty"`
}

// requestLogCap 环形缓冲容量（设计文档 §4.3.2：~1000 条，重启即清）。
// 持久化由 usage 分桶承担，二者不重叠。
const requestLogCap = 1000

// requestLogStore 请求日志环形缓冲：单写（chatStat.done() 单一埋点）多读
// （/api/request_logs 轮询）。mutex 保护切片+游标；dropped 只在 handleRequestLogs
// （读路径）被锁外读取，故用 atomic.Int64 与写锁内的写操作解耦（audit：此前
// 锁外读 + 锁内写构成数据竞争，-race 可复现）。
type requestLogStore struct {
	mu      sync.Mutex
	buf     []requestLogEntry // 定长环形
	next    int               // 下一个写位置
	seq     int64             // 全局递增序号（跨重启清零，仅运行期单调）
	dropped atomic.Int64      // 被覆盖的旧条数（观测用）
}

var requestLog = &requestLogStore{buf: make([]requestLogEntry, requestLogCap)}

// globalRequestArchive 请求日志 JSONL 归档 writer（reqlog 包，panel 移植件）。
// 包级而非 Handler 字段：chatStat 是值日志对象，不持 Handler 引用；单一埋点
// 读一个包级引用比回传 Handler 简单且测试可注入（SetRequestArchive，对齐
// SetUsageRecorder 的注入风格）。nil = 未配置归档（appendRequestLog 跳过，
// 热路径零开销）。
var globalRequestArchive *reqlog.Recorder

// SetRequestArchive 注入请求日志归档 writer（main 启动期调用一次；
// nil = 关闭归档——环形缓冲照常工作，仅无磁盘落盘）。
func SetRequestArchive(r *reqlog.Recorder) { globalRequestArchive = r }

// appendRequestLog 写入一条请求日志：环形缓冲（覆盖最旧）+ 可选 JSONL 归档
// （archive 非 nil 时异步投递；nil 则跳过，未配置归档时零开销）。
// 由 chatStat.done() 调用（单一埋点）。
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
		requestLog.dropped.Store(requestLog.seq - int64(len(requestLog.buf)))
	}
	// JSONL 归档（reqlog 包）：与环形缓冲互补——环形是热数据（重启即清），
	// 归档是持久层（按日轮转/保留上限）。投递非阻塞，队列满由 reqlog 丢弃并计数。
	// nil 跳过：未配置归档时这条判断是唯一的额外开销（一次指针判空）。
	if a := globalRequestArchive; a != nil {
		a.Record(requestLogArchiveEvent(e))
	}
}

// requestLogArchiveEvent 环形缓冲条目 → reqlog 归档事件的字段映射。
// Account 复用表格日志的「昵称(uid8)」标签口径（logfmt.Label），不落完整 UID
// ——归档文件可被人工 cat/grep，最小化可识别信息。
func requestLogArchiveEvent(e requestLogEntry) reqlog.Event {
	return reqlog.Event{
		Time:       e.Time,
		Model:      e.Model,
		Account:    logfmt.Label(e.UID, e.Nick),
		Mode:       e.Mode,
		Status:     e.Status,
		OK:         e.Status >= 200 && e.Status < 400, // 3xx 视为成功（网关无重定向语义，防御性口径）
		Tokens:     e.Tokens,
		TTFBMS:     e.TTFBMS,
		Credit:     e.Credit,
		HasCredit:  e.HasCredit,
		CacheHit:   e.CacheHit,
		CacheMiss:  e.CacheMiss,
		CacheWrite: e.CacheWrite,
		Error:      e.Error,
		ClientIP:   e.ClientIP,
		UserAgent:  e.UserAgent,
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
		"dropped": requestLog.dropped.Load(),
		"cap":     requestLogCap,
	})
}

// requestLogClientInfo 从入站请求提取调用来源（client_ip / user_agent）。
// enabled=false（logging.request_client_info 缺省）返回零值——来源信息比 token
// 计数敏感，运营可自行决定是否采集/落盘。
//
// TODO(iputil 接线)：client_ip 目前取 r.RemoteAddr 的 IP 部分（iputil.CleanAddress，
// 剥端口/方括号），不解析 X-Forwarded-For。主代理接好 iputil.RequestClientIP
// （可信代理链 + XFF hop 数配置）后，此处换 RequestClientIP(r, trustedCIDRs, hops)
// 即可，环形缓冲/归档字段口径不变。
func requestLogClientInfo(r *http.Request, enabled bool) (clientIP, userAgent string) {
	if r == nil || !enabled {
		return "", ""
	}
	return iputil.CleanAddress(r.RemoteAddr), r.UserAgent()
}

// noteClientInfo 把入站请求的调用来源填进 chatStat（供环形缓冲与 JSONL 归档）。
// handler 在 newChatStat 之后调用一次（开关关闭时为 no-op，零开销）。
// 供主代理接线的一行：st.noteClientInfo(r, cfg.Logging.RequestClientInfo)。
func (s *chatStat) noteClientInfo(r *http.Request, enabled bool) {
	if s == nil || !enabled {
		return
	}
	s.clientIP, s.userAgent = requestLogClientInfo(r, enabled)
}

// requestLogPayloadFromStat 从 chatStat 抽取请求日志载荷（与表格日志同一观测源）。
// 前置条件：done() 已把 uid/nick/ttfb 等字段填齐；clientIP/userAgent 由 handler
// 按 logging.request_client_info 开关填充（缺省零值——不采集即不落任何视图）。
func requestLogPayloadFromStat(s *chatStat) requestLogEntry {
	return requestLogEntry{
		Time:        time.Now(),
		Model:       s.model,
		UID:         s.uid,
		Nick:        s.nick,
		Mode:        s.mode,
		Status:      s.status,
		Tokens:      s.toks,
		TTFBMS:      s.ttfb.Milliseconds(),
		Credit:      s.credit,
		HasCredit:   s.hasCredit,
		CacheHit:    s.cacheHit,
		CacheMiss:   s.cacheMiss,
		CacheWrite:  s.cacheWr,
		HasCacheObs: s.hasUsage,
		Error:       s.errSummary,
		ClientIP:    s.clientIP,
		UserAgent:   s.userAgent,
	}
}
