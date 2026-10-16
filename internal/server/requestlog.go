package server

import (
	"log"
	"net/http"
	"net/netip"
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

// /api/request_logs 响应的 source 取值：标明本次 items 的真实**取数通路**（前端
// 据此决定提示文案），与「有没有数据」无关——归档未启用时 source 仍是 archive，
// 空集原因由 archive_enabled=false 解释（「归档没开」与「归档里没命中」排障含义
// 不同，不能混成一个字段）。
const (
	requestLogSourceRing    = "ring"    // 内存环形缓冲（无过滤参数）
	requestLogSourceArchive = "archive" // 磁盘 JSONL 归档（带任一过滤参数）
)

// requestLogArchiveErrMsg 归档读失败时的对外固定文案。不回显底层 err 文本：
// 归档读失败必是 *os.PathError（os.ReadDir / os.Open），err.Error() 会带上归档
// 目录/分片文件的绝对路径（以及 state 目录结构）——内部拓扑不该经 API 泄给客户端
// （端点虽经 withAuth 仅管理员可达，仍按最小暴露面处理）；细节进服务端日志。
const requestLogArchiveErrMsg = "read request archive failed"

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
//
// 查询参数（v1.19.0 过滤族，均可选）：
//   - limit：条数上限（既有，默认 100，上限 requestLogCap）；
//   - from / to：unix 秒，事件时间范围。语义直接复用 reqlog.Filter——**闭区间**
//     （两侧都含），与归档侧判定同源，不另立一套边界口径；
//   - ip / ua：client_ip / user_agent 子串，大小写不敏感（reqlog.containsFold）。
//
// 数据源分流（不传新参数 = 行为逐字不变）：
//   - 无 from/to/ip/ua：读内存环形缓冲（热数据，重启即清），既有响应逐字不变
//     （source="ring"）；
//   - 带任一过滤参数：读磁盘 JSONL 归档（reqlog.ReadArchive + Filter）——只有它
//     能回溯超过 1000 条/跨重启的历史，也是 Filter 能力的既有出口（source="archive"）。
//     归档未启用（logging.request_archive=false，默认）时返回空集，不静默降级成
//     别的数据源（观测缺位可见：排障时先看 archive_enabled / 这个开关）。
//
// 响应字段（「只增不改」：既有键 items/dropped/cap 全部保留，形状与取值口径不变）：
//   - items：条目数组（恒为数组，空集是 []，类型不随数据源变化）；
//   - dropped / cap：**仅描述内存环形缓冲**——dropped 是已被覆盖的旧条数、cap 是
//     缓冲容量（requestLogCap）。归档分支（source=archive）下这两个键与 items
//     无关：items 来自磁盘归档，既不占环形缓冲也不受其容量约束。前端在
//     source=archive 时不得用 cap 当分页/上限依据、不得把 dropped 当归档丢包数
//     （归档侧丢弃与写盘错误各有其观测出口，不在本响应里）；
//   - archive_enabled：归档是否已启用（判据见 requestArchiveEnabled）；
//   - source：本次 items 的取数通路，见上面 requestLogSource* 常量。
//
// 非法 from/to（非整数）或 <=0 按「未给」处理，与 limit 的既有风格一致；ip/ua 空串同理。
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
	var items []requestLogEntry
	source := requestLogSourceRing
	if filter, filtered := requestLogsFilter(r); filtered {
		source = requestLogSourceArchive
		rows, err := requestLogsFromArchive(limit, filter)
		if err != nil {
			// 归档读失败（目录权限/磁盘异常/目录被删）如实报错，不静默返回空集——
			// 「查不到」与「读不出来」在排障时是两回事。对外只给固定文案：err 是
			// *os.PathError，文本含归档目录的绝对路径（见 requestLogArchiveErrMsg），
			// 细节走服务端日志（信封形状 {"ok":false,"error":...} 保持不变）。
			log.Printf("ERR: [server] request_logs: 读请求归档失败: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"ok":    false,
				"error": requestLogArchiveErrMsg,
			})
			return
		}
		items = rows
	} else {
		items = requestLogsSnapshot(limit)
	}
	// items 恒为数组（归档关闭/无命中时是空片）：契约字段类型不随数据源变化。
	if items == nil {
		items = []requestLogEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":           items,
		"dropped":         requestLog.dropped.Load(), // 仅环形缓冲口径；归档分支与 items 无关（见上）
		"cap":             requestLogCap,             // 同上：环形缓冲容量，不是归档条数上限
		"archive_enabled": requestArchiveEnabled(),
		"source":          source,
	})
}

// requestArchiveEnabled 报告归档是否已启用，作为响应 archive_enabled 的唯一判据。
//
// 判据是**包级注入引用非 nil**：main 只在 logging.request_archive=true 时才调用
// SetRequestArchive 注入 non-nil recorder（cmd/server/main.go），nil 即「未启用」
// ——与 appendRequestLog / requestLogsFromArchive 的分流判据同源，两处不会各说各话。
// 为什么不用 reqlog.Snapshot().Archive.Enabled：那会真的扫一遍归档目录
// （O(文件数)），而本端点被面板轮询，判据只需要一次指针判空；何况归档目录坏了时
// ReadArchive 会走 500 分支，也不靠这个布尔值表达。
func requestArchiveEnabled() bool { return globalRequestArchive != nil }

// requestLogsFilter 把查询参数解析成 reqlog.Filter。返回值 filtered=false 表示
// 一个过滤参数都没给：调用方保持既有环形缓冲路径（不传新参数 = 行为逐字不变）。
// 非法 from/to 静默忽略（与 limit 同风格），不报错也不半途启用其它维度；
// from/to <= 0 同样按「未给」处理——面板侧 0 是空态哨兵（见 panel unixSec 注释），
// 若把 to=0 当真会变成「1970 之前」的空结果，纯属埋雷。
func requestLogsFilter(r *http.Request) (reqlog.Filter, bool) {
	q := r.URL.Query()
	var f reqlog.Filter
	filtered := false
	if n, err := strconv.ParseInt(q.Get("from"), 10, 64); err == nil && n > 0 {
		f.From = time.Unix(n, 0)
		filtered = true
	}
	if n, err := strconv.ParseInt(q.Get("to"), 10, 64); err == nil && n > 0 {
		f.To = time.Unix(n, 0)
		filtered = true
	}
	if v := q.Get("ip"); v != "" {
		f.ClientIP = v
		filtered = true
	}
	if v := q.Get("ua"); v != "" {
		f.UserAgent = v
		filtered = true
	}
	return f, filtered
}

// requestLogsFromArchive 用 reqlog 的 Filter 回读磁盘归档（按事件时间倒序、最多
// limit 条），并把归档事件折算回环形缓冲的条目形状。
//
// 为什么折算而不是直接透传 reqlog.Event：面板前端只认 requestLogEntry 的字段名
// （uid/nick/has_cache_obs/...），过滤查询与热路径必须同形，否则同一个列表要两套
// 渲染分支。归档不存完整 UID（隐私最小化，Account 是「昵称(uid8)」标签），故 uid
// 留空、标签放 nick 列展示。
func requestLogsFromArchive(limit int, filter reqlog.Filter) ([]requestLogEntry, error) {
	a := globalRequestArchive
	if a == nil {
		return []requestLogEntry{}, nil // 归档未启用：观测缺位，返回空集
	}
	rows, err := a.ReadArchive(limit, &filter)
	if err != nil {
		return nil, err
	}
	out := make([]requestLogEntry, 0, len(rows))
	for _, e := range rows {
		out = append(out, requestLogEntryFromArchiveEvent(e))
	}
	return out, nil
}

// requestLogEntryFromArchiveEvent 归档事件 → 请求日志条目：字段一一对应，只有
// 归档侧本就不保存的两处做降级处理（uid 留空、has_cache_obs 近似），见下面注释。
func requestLogEntryFromArchiveEvent(e reqlog.Event) requestLogEntry {
	return requestLogEntry{
		Time:       e.Time,
		Model:      e.Model,
		Nick:       e.Account, // "昵称(uid8)" 标签；uid 不落归档（留空）
		Mode:       e.Mode,
		Status:     e.Status,
		Tokens:     e.Tokens,
		TTFBMS:     e.TTFBMS,
		Credit:     e.Credit,
		HasCredit:  e.HasCredit,
		CacheHit:   e.CacheHit,
		CacheMiss:  e.CacheMiss,
		CacheWrite: e.CacheWrite,
		// has_cache_obs 不在归档事件里（reqlog.Event 无此字段）：三段全 0 时无法
		// 区分「无 usage 观测」与「观测到全 0」。取「任一段非 0 = 有观测」的保守
		// 近似——缺观测不冒充命中 0（前端 cacheBadge 正是这个口径：无观测不显示角标）。
		HasCacheObs: e.CacheHit+e.CacheMiss+e.CacheWrite > 0,
		Error:       e.Error,
		ClientIP:    e.ClientIP,
		UserAgent:   e.UserAgent,
	}
}

// clientInfoTrustedCIDRs / clientInfoTrustedHops 采集侧可信代理解析参数
// （config security.trusted_proxy_cidrs / trusted_proxy_hops 的归一化产物）。
//
// 为什么是包级变量而不是 Handler 字段：采集点 chatStat 是值日志对象、不持 Handler
// 引用（与 globalRequestArchive / globalUsageRecorder / chatLogOut 同一注入风格），
// 单一埋点读包级引用比回传 Handler 简单，测试也可注入。
//
// 零值（默认，未调用 SetClientInfoTrust）= 不信任任何转发头：client_ip 恒取 TCP
// 对端地址（fail-closed）。这与入站安全同口径——转发头是客户端可伪造的普通请求头，
// 只有确实经过可信代理的请求才采信，绝不因「日志」用途而放宽默认安全口径。
var (
	clientInfoTrustedCIDRs []netip.Prefix
	clientInfoTrustedHops  int
)

// SetClientInfoTrust 注入可信代理参数（main 启动期调用一次，与网关入站安全共用
// 同一份 cfg.SecurityTrustedCIDRs / Security.TrustedProxyHops——两侧口径必须一致，
// 否则网关按其判定放行、请求日志却记另一个来源 IP，排障会指向错的地方）：
//
//	server.SetClientInfoTrust(cfg.SecurityTrustedCIDRs, cfg.Security.TrustedProxyHops)
//
// 不调用 = 保持零值（不信任任何转发头），采集行为与接线前逐字一致。
func SetClientInfoTrust(cidrs []netip.Prefix, hops int) {
	clientInfoTrustedCIDRs = cidrs
	clientInfoTrustedHops = hops
}

// userAgentCap User-Agent 采集上限（字符）。UA 长度由客户端支配（可到 KB 级），
// 不截断会按条放大环形缓冲与归档体积；200 覆盖全部真实 UA（含 Chrome/Firefox/
// 各 SDK 的长串），只在恶意/异常超长时生效。
const userAgentCap = 200

// requestLogClientInfo 从入站请求提取调用来源（client_ip / user_agent）。
// enabled=false（logging.request_client_info 缺省）返回零值——来源信息比 token
// 计数敏感，运营可自行决定是否采集/落盘（默认 false 不变）。
//
// client_ip 走 iputil.RequestClientIP 的可信代理感知解析（与入站鉴权/规则引擎
// 同一函数）：仅当 TCP 对端落在 clientInfoTrustedCIDRs 内才采信 X-Real-IP /
// X-Forwarded-For（按 hops 从右往左取），否则一律 RemoteAddr 的 IP 部分。
// 注：面板 loopback_only 安全闸不在此列——那是网络边界判定，恒按 RemoteAddr 从严。
func requestLogClientInfo(r *http.Request, enabled bool) (clientIP, userAgent string) {
	if r == nil || !enabled {
		return "", ""
	}
	return iputil.RequestClientIP(r, clientInfoTrustedCIDRs, clientInfoTrustedHops), truncateUserAgent(r.UserAgent())
}

// truncateUserAgent 把 UA 截断到 userAgentCap 个字符。按字符（rune）截断而不是
// 按字节：UA 里可能出现非 ASCII（少见但合法），按字节切会把多字节字符劈成非法
// UTF-8，JSON 序列化后变成替换字符——截断不该损坏文本本身。
func truncateUserAgent(ua string) string {
	if len(ua) <= userAgentCap { // 快路径：ASCII 常见形态字节数即字符数
		return ua
	}
	rs := []rune(ua)
	if len(rs) > userAgentCap {
		rs = rs[:userAgentCap]
	}
	return string(rs)
}

// noteClientInfo 把入站请求的调用来源填进 chatStat（供环形缓冲与 JSONL 归档）。
// handler 在 newChatStat 之后按 logging.request_client_info 开关调用一次
// （开关关闭时为 no-op，零开销）；可信代理解析参数经 SetClientInfoTrust 注入。
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
