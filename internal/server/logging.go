// logging.go 请求级表格日志：每个 /v1/chat/completions 请求结束后打印一行到 stdout。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/usage"
)

// chatSeq 进程级请求序号。
var chatSeq atomic.Int64

// chatLogEnabled 聊天表格日志总开关。生产恒 true；
// 测试包经 TestMain 置 false 关闭 stdout 噪音，需要断言行输出的测试用 withChatLog 临时开启（R5）。
var chatLogEnabled = true

// chatLogOut 聊天表格日志输出目标。生产默认 os.Stdout；main 启用管理面板时经
// SetChatLogOutput 注入 MultiWriter（stdout + panel Ring），把每行镜像进
// /api/logs 的 chat 频道——表格行不走 log 包，必须单独镜像（panel 同款机制）。
var chatLogOut io.Writer = os.Stdout

// SetChatLogOutput 替换聊天表格日志输出目标（仅 main 启动期调用一次）。
func SetChatLogOutput(w io.Writer) { chatLogOut = w }

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	start  time.Time
	model  string
	mode   string // "stream" | "sync"
	uid    string // 完整 uid，展示时只取前 8 位
	nick   string // 账号昵称（auth.Auth.Nickname，登录时落盘）；空则只显示 uid8
	realm  string // 请求账号归属域（cn/global，DESIGN §3.5 分桶维度）；handler 选号后覆盖
	ttfb   time.Duration
	toks   int // <0 表示 usage 缺失 → 显示 "-"
	status int

	// metrics 采集字段（供 /v1/stats 聚合）：全部来自上游 usage，缺失时保持零值
	// 并由 hasUsage 区分「缺观测」与「显式 0」——与成本账本同一纪律。
	hasUsage  bool
	prompt    int
	cacheHit  int
	cacheMiss int
	cacheWr   int
	// reason 实际思考 token（usage 的 completion_tokens_details.reasoning_tokens /
	// 扁平别名 completion_thinking_tokens）：与 effort 档位并列的可观测代理指标。
	reason    int
	credit    float64
	hasCredit bool
	// totalMS 端到端耗时毫秒（done() 时落），供请求日志/归档的 duration_ms。
	totalMS int64

	// errSummary 非 200 出口的错误摘要（面板请求日志 Error 列；空 = 正常/无记录）。
	// 由 handler 错误路径经 setError 填充；截断到 ~200 字节防环形缓冲被长报文撑爆。
	errSummary string

	// effortIn / effortOut 思考档位观测（下游入站值 / 出站最终值）：
	//   - effortIn 来自 session.ParseRequest 的 reasoning_effort（snake 优先 camel），
	//     Anthropic/Responses 协议经转换后的 OpenAI 形态同样覆盖；
	//   - effortOut 由 upstream.EffectiveReasoningEffort 按 prepareBody 同口径推算
	//     （含补默认档与按模型支持档降级），随选号 realm 缓存、降级重写后重算。
	//   空串 = 无档位（展示为 "-"）。仅观测，不参与任何路由/改写决策。
	effortIn  string
	effortOut string

	// clientIP / userAgent 调用来源（可选，供请求日志环形缓冲与 JSONL 归档）。
	// 由 handler 在 newChatStat 后按 logging.request_client_info 开关填充
	// （默认 false 不采集，保持零值）；口径见 requestlog.go requestLogClientInfo。
	clientIP  string
	userAgent string

	logged bool
}

// newChatStat 以请求进入 handler 的时刻为起点构造统计对象；toks 默认 -1（usage 缺失）。
// realm 取请求模型域（cn/global；裸名 → cn，零回归）；handler 每次选号成功后以
// 账号实际 Realm() 覆盖（分桶按账号归属域，DESIGN §3.5）。
// model 空值（坏 body / 未带 model 字段）标 "-"，与 parseModelFromBody 旧口径一致。
func newChatStat(now time.Time, parsed session.Parsed, realm string) *chatStat {
	mode := "sync"
	if parsed.Stream {
		mode = "stream"
	}
	model := parsed.Model
	if model == "" {
		model = "-"
	}
	return &chatStat{start: now, model: model, mode: mode, realm: realm, toks: -1, effortIn: parsed.ReasoningEffort}
}

// setError 记录非 200 出口的错误摘要（面板请求日志 Error 列）。
// 截断到 200 字符：环形缓冲容量 1000 条，长上游报文会把内存/响应撑爆；
// 完整原文在 handler 响应与 stdout 日志里仍可见，这里只是观测摘要。
func (s *chatStat) setError(msg string) {
	if s.errSummary != "" {
		return // 首个错误优先：轮转途中多号失败，最终原因往往在 lastErr（末端写入）
	}
	msg = strings.TrimSpace(msg)
	if len(msg) > 200 {
		msg = msg[:200]
	}
	s.errSummary = msg
}

// done 幂等落一行表格日志，并把本次请求记入 metrics 聚合（/v1/stats 数据源）、
// 请求日志环形缓冲（/api/request_logs 数据源，v1.2.0）与用量分桶（panel usage 移植件）。
//
// 单一埋点：流式 / 非流式 / 各类错误路径最终都汇到此处，故 metrics 天然覆盖全路径，
// 不需要在每个 return 前重复记账（重复记账反而会漏分支或双计）。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	total := time.Since(s.start)
	s.totalMS = total.Milliseconds()
	logChatRow(s.ttfb, total, s.model, s.mode, s.uid, s.nick, s.status, s.toks, s.effortIn, s.effortOut)
	recordChatMetric(s, total)
	appendRequestLog(requestLogPayloadFromStat(s))
	recordUsageBucket(s)
}

// recordUsageBucket 用量分桶记录（usage.Enabled=true 时由 main 注入）。
// ok 以「上游是否给了 usage」判定：hasUsage=false 的尝试（传输错误/>=400/解析失败）
// 计为失败——失败也计入请求数，否则重试放大在用量视图里看不见。
//
// 缺省值归 0、靠 Has* 标志表达缺失（audit：旧实现把 toks=-1 哨兵写进
// CompletionTokens 并参与 TotalTokens=prompt+(-1) 算术——HasCompletion/HasTotal
// 为 true 时 usage.Add 会把 -1 与 prompt-1 真的累计进桶）。hasUsage=true 时
// toks 必 >=0（handler 流式路径只在 hasUsage 时覆盖 st.toks，非流式
// completionTokens 缺失时保持 -1 且不置 hasUsage），但此处仍显式钳 0 防御。
func recordUsageBucket(s *chatStat) {
	if globalUsageRecorder == nil {
		return
	}
	if !s.hasUsage {
		globalUsageRecorder.Add(time.Now(), s.realmOf(), s.uid, s.model, usage.Delta{}, false)
		return
	}
	compl := s.toks
	if compl < 0 {
		compl = 0 // 防御：hasUsage 时 toks 不应为 -1 哨兵；万一出现按缺失归 0 处理
	}
	globalUsageRecorder.Add(time.Now(), s.realmOf(), s.uid, s.model, usage.Delta{
		PromptTokens:     int64(s.prompt),
		HasPromptTokens:  true,
		CompletionTokens: int64(compl),
		HasCompletion:    s.toks >= 0,
		TotalTokens:      int64(s.prompt + compl),
		HasTotal:         s.toks >= 0,
		LatencyMs:        s.ttfb.Milliseconds(),
		HasLatency:       s.ttfb > 0,
		// 实测扣费积分（上游 usage.credit）：hasCredit 区分「缺失」与「显式 0」，
		// 显式 0 是合法免费观测（tier0 限免），与成本账本同一纪律。
		Credit:    s.credit,
		HasCredit: s.hasCredit,
	}, s.toks >= 0)
}

// realmOf 请求账号的 realm（cn/global；未选号即失败的请求回落请求模型域——
// newChatStat 以 realm 前缀初始化，账号归属域由 handler 选号处覆盖；未知/空回落
// cn，与 usage 包缺省一致）。
func (s *chatStat) realmOf() string {
	if s.realm == "global" {
		return "global"
	}
	return "cn"
}

// globalUsageRecorder main 注入的用量记录器（nil = usage.enabled=false）。
// 包级而非 Handler 字段：chatStat 是值日志对象，不持 Handler 引用；单一埋点
// 读一个包级原子引用比回传 Handler 简单且测试可注入（SetUsageRecorder）。
var globalUsageRecorder *usage.Recorder

// SetUsageRecorder 注入用量记录器（main 启动期调用一次；nil = 关闭记录）。
func SetUsageRecorder(r *usage.Recorder) { globalUsageRecorder = r }

// chatStatsReader 在流式透传时抓取 SSE 末帧的 usage.completion_tokens 精确值，
// 并记录首个 data 帧的 TTFB；原始字节原样返回给下游透传。
// 注意：不做 rune 估算，token 数一律采信上游 usage。
type chatStatsReader struct {
	br        *bufio.Reader
	start     time.Time
	ttfb      time.Duration
	seen      bool // 已见过首个 data 帧（TTFB 只记一次）
	hasUsage  bool // 末帧是否带 usage
	hasCredit bool // 是否出现过带 credit 的 usage（缺失≠0，见 Credit() 注释）
	tokens    int
	credit    float64 // 末帧 usage.credit（本次真实扣费，供成本账本）
	prompt    int     // 末帧 usage.prompt_tokens（与 completion 合计折算单价）
	cacheHit  int     // 末帧 usage.prompt_cache_hit_tokens（供 /v1/stats）
	cacheMiss int     // 末帧 usage.prompt_cache_miss_tokens
	cacheWr   int     // 末帧 usage.prompt_cache_write_tokens
	reason    int     // 末帧思考 token（completion_tokens_details.reasoning_tokens / 扁平别名）
	pend      []byte  // 已读未返回的行缓存
}

// newChatStatsReaderSince 以 since 为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	return &chatStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since}
}

// TTFB 返回首个 data 帧到达耗时；无帧时为 0。
func (s *chatStatsReader) TTFB() time.Duration { return s.ttfb }

// Tokens 返回末帧 usage.completion_tokens 与是否缺失；无 usage 时 ok=false。
func (s *chatStatsReader) Tokens() (int, bool) { return s.tokens, s.hasUsage }

// Credit 返回末帧 usage.credit（本次真实扣费）。ok=true 要求 usage 存在**且** credit
// 字段显式出现——字段缺失时 ok=false（缺失≠0：不能把"缺观测"当"0 成本"写入账本，
// 否则收费的号可能被误判 tier0 免费层）。显式 credit:0 仍是合法免费观测（ok=true）。
func (s *chatStatsReader) Credit() (float64, bool) { return s.credit, s.hasUsage && s.hasCredit }

// TotalTokens 返回本次请求总 token 数（prompt + completion），供成本单价折算。
func (s *chatStatsReader) TotalTokens() int { return s.prompt + s.tokens }

// PromptTokens 返回末帧 usage.prompt_tokens（供 /v1/stats 输入侧统计）。
func (s *chatStatsReader) PromptTokens() int { return s.prompt }

// ReasoningTokens 返回末帧 usage 的思考 token 数（上游未下发时为 0）。
func (s *chatStatsReader) ReasoningTokens() int { return s.reason }

// CacheTokens 返回缓存三段计数（命中 / 未命中 / 写入），供 /v1/stats 的命中率聚合。
func (s *chatStatsReader) CacheTokens() (hit, miss, write int) {
	return s.cacheHit, s.cacheMiss, s.cacheWr
}

// cacheWriteMinTokens 推导写入量的最小未命中阈值（上游前缀缓存的最小可缓存块）。
//
// 实测（2026-10-26 手机双轮探针，conversation_id 固定 + 网关注入 prompt_cache_key）：
//   - 50 token 前缀：次轮 hit=0 —— 低于缓存块粒度，根本不写；
//   - 2858 token 前缀：次轮 hit=2688（42×64）—— 除尾部活跃段外全量写入。
//
// 64 是 DeepSeek 系上下文缓存的块粒度量级，用它挡掉「必然不写」的小请求。
const cacheWriteMinTokens = 64

// deriveCacheWrite 缓存写入观测值。
//
// 上游（CN /v2）**从不下发** prompt_cache_write_tokens / cache_creation_input_tokens
// ——双轮探针里首轮 miss=2858、write=0，次轮 hit=2688 证明写入确实发生了，只是
// 不上报（160 条归档 154 条有 hit、0 条有 write）。写入量只能推导：带缓存键的
// 请求（网关对每个出站请求都注入 prompt_cache_key）其未命中前缀正是被写入、
// 下一轮变成命中的部分，故 write=miss（≥最小缓存块；阈值下实测不写，保持 0）。
// 推导精度实测 ~94%（2858 推导 vs 2688 次轮实际命中，差额是尾部活跃段不入缓存）。
// raw>0（未来上游开始下发）时原值优先，推导只补零值。
func deriveCacheWrite(raw, miss int) int {
	if raw > 0 {
		return raw
	}
	if miss >= cacheWriteMinTokens {
		return miss
	}
	return 0
}

// parseSSELine 解析一行 "data: {...}"：首帧记 TTFB，含 usage 时采信精确 completion_tokens。
func (s *chatStatsReader) parseSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data: ") {
		return
	}
	payload := strings.TrimPrefix(line, "data: ")
	if payload == "[DONE]" {
		return
	}
	if !s.seen {
		s.seen = true
		s.ttfb = time.Since(s.start)
	}
	var chunk struct {
		Usage *struct {
			CompletionTokens int      `json:"completion_tokens"`
			PromptTokens     int      `json:"prompt_tokens"`
			Credit           *float64 `json:"credit"` // 指针区分「缺失」与「显式 0」
			// 缓存三段（上游实测字段名，见 /v1/stats 的 cache_* 口径）。
			PromptCacheHitTokens   int `json:"prompt_cache_hit_tokens"`
			PromptCacheMissTokens  int `json:"prompt_cache_miss_tokens"`
			PromptCacheWriteTokens int `json:"prompt_cache_write_tokens"`
			// 思考 token：扁平别名与 details 对象两种形态都认（取大者语义上同源）。
			CompletionThinkingTokens int `json:"completion_thinking_tokens"`
			CompletionTokensDetails  *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil || chunk.Usage == nil {
		return
	}
	s.hasUsage = true
	s.tokens = chunk.Usage.CompletionTokens
	s.prompt = chunk.Usage.PromptTokens
	s.cacheHit = chunk.Usage.PromptCacheHitTokens
	s.cacheMiss = chunk.Usage.PromptCacheMissTokens
	s.cacheWr = deriveCacheWrite(chunk.Usage.PromptCacheWriteTokens, chunk.Usage.PromptCacheMissTokens)
	s.reason = chunk.Usage.CompletionThinkingTokens
	if d := chunk.Usage.CompletionTokensDetails; d != nil && d.ReasoningTokens > s.reason {
		s.reason = d.ReasoningTokens
	}
	if chunk.Usage.Credit != nil {
		s.hasCredit = true
		s.credit = *chunk.Usage.Credit
	}
}

// Read 返回原始数据，同时解析统计 TTFB/token。
func (s *chatStatsReader) Read(p []byte) (int, error) {
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	line, err := s.br.ReadString('\n')
	if line != "" {
		s.parseSSELine(line)
		s.pend = []byte(line)
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	return 0, err
}

// parseModelFromBody 从请求 JSON 取 model 字段，缺省标 "-"。
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["completion_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// usageCreditTotal 从聚合响应提取本次真实扣费与总 token 数（供成本账本）。
// ok=false 表示 usage 缺失或字段类型不符——此时不记录观测，避免污染账本。
func usageCreditTotal(resp map[string]any) (credit float64, total int, ok bool) {
	u, isMap := resp["usage"].(map[string]any)
	if !isMap {
		return 0, 0, false
	}
	c, hasCredit := u["credit"].(float64)
	pt, hasPrompt := u["prompt_tokens"].(float64)
	ct, hasCompletion := u["completion_tokens"].(float64)
	if !hasCredit || (!hasPrompt && !hasCompletion) {
		return 0, 0, false
	}
	return c, int(pt) + int(ct), true
}

// uidPrefix 只显示 uid 前 8 位；空 uid 显示 "-"。
//
// 保留本函数是因为 logging_test.go 直接断言它；实现委托 logfmt.UID8，避免
// "截 8 位" 的规则在 server 与 logfmt 两处各写一份而走样。
func uidPrefix(uid string) string {
	return logfmt.UID8(uid)
}

// 请求流水行的固定列宽（显示列宽，非字节）。取固定宽度而不是让内容自然长度撑开，
// 是为了让 stdout 里成百上千行能竖着扫——否则模型名长短不一、中文昵称按字节补空格
// 错位，根本没法用肉眼对齐着一列列看（这正是上一版 11 字节硬截断要解决的问题）。
const (
	// chatModelWidth 覆盖 realm 前缀 + 最长模型名："global:" (7) + "deepseek-v4.1-flash" (19) = 26。
	// 旧的 11 字节截断会把 "cn:deepseek-v4-flash" 切成 "cn:deepseek"，让人误以为是另一个模型。
	chatModelWidth = 26
	// chatAcctWidth 容纳 "昵称(uid8)"：4 字中文昵称 + uid8 恰好 18 列（多喝水/
	// 蝶舞飞扬/玛露希尔这类常见昵称原样放下）。
	// 定宽 + 超宽截断（见 acctLabel）：此前的「只补不截」让掩码手机号昵称
	// （"173****6289(78b6dc68)" = 21 列）把整行撑宽，尾部 eff/total 列被顶出
	// 终端可视区——用户要求缩小账号列让每列数据都完整可见。截掉昵称尾部与
	// UID8 哲学同口径（全量昵称/uid 在 auths/ 与 state.json 可查）。
	chatAcctWidth = 18
	chatTTFBWidth = 8
	chatTokWidth  = 6
	chatRateWidth = 11 // 形如 "183.6tok/s"
	// chatEffortWidth 形如 "xhigh>xhigh"（入站>出站）。最宽档位 "minimal"（7 列）
	// ×2 + 分隔符 = 15：旧的 11 列会把 "minimal>minimal" 截成 "minimal>mini"，
	// 违反「每列数据完整展示」，加宽到最宽组合原样呈现。
	chatEffortWidth = 15
)

// acctLabel 组装 "昵称(uid8)" 账号标签，并把昵称按**显示列宽**截断到 width 内
//（uid8 后缀永不截断——它是 grep/对账的主键，昵称尾部去 auths/ 查）。
// 无昵称时退回 uid8（与 logfmt.Label 空昵称口径一致，不出 "(...)" 括号）。
// 结果保证 ≤ width（配合 Pad 得到严格定宽列）；width 连 "(uid8)" 都容不下时
// 整体按宽截断（极端配置的自保，不 panic）。
func acctLabel(uid, nick string, width int) string {
	label := logfmt.Label(uid, nick)
	if logfmt.DisplayWidth(label) <= width {
		return label
	}
	suffix := "(" + logfmt.UID8(uid) + ")"
	avail := width - logfmt.DisplayWidth(suffix)
	if avail < 1 {
		return logfmt.TruncateWidth(label, width)
	}
	return logfmt.TruncateWidth(strings.TrimSpace(nick), avail) + suffix
}

// effortField 拼接「入站>出站」思考档位列：空档位显示 "-"，双空显示单个 "-"
// （无思考观测的请求不占视觉噪音）。
func effortField(effortIn, effortOut string) string {
	in, out := effortIn, effortOut
	if in == "" {
		in = "-"
	}
	if out == "" {
		out = "-"
	}
	if in == "-" && out == "-" {
		return "-"
	}
	return in + ">" + out
}

// logChatRow 打印一行请求级表格日志（直接输出 stdout，无 log 时间戳前缀）。
//
// 参数：
//   - model：模型名（含 realm 前缀），超 chatModelWidth 截断（模型名是 ASCII，字节截即列宽）；
//   - uid/nick：完整 uid 与账号昵称，经 acctLabel 拼成 "昵称(uid8)" 展示并截断到
//     chatAcctWidth（uid8 后缀完整保留；昵称尾部去 auths/ 查）——只有 uid8 时人眼
//     无法判断是哪个号，要辨认必须再查 auths/，排障多一跳；
//   - toks<0 表示 usage 缺失，显示 "-"；
//   - effortIn/effortOut：思考档位（下游入站 > 出站最终值），空串显示 "-"。
func logChatRow(ttfb, total time.Duration, model, mode, uid, nick string, status int, toks int, effortIn, effortOut string) {
	if !chatLogEnabled {
		return
	}
	seq := chatSeq.Add(1)
	model = logfmt.Pad(logfmt.Truncate(model, chatModelWidth), chatModelWidth)
	// 账号列严格定宽：昵称超宽时按显示列宽截断（uid8 后缀保留），保证该列不把
	// 整行撑宽、后续每列都完整落在可视区内（此前「只补不截」会让掩码手机号昵称
	// 顶出尾部列——用户要求每列数据都能完整展示）。
	acct := logfmt.Pad(acctLabel(uid, nick, chatAcctWidth), chatAcctWidth)
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		if total > 0 {
			tokpsField = fmt.Sprintf("%.1ftok/s", float64(toks)/total.Seconds())
		} else {
			tokpsField = "0.0tok/s"
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	fmt.Fprintf(chatLogOut, "| #%03d | %s | %s | %s | %d | %s | TTFB=%s | tok=%s | %s | eff=%s | total=%.1fs |\n",
		seq,
		time.Now().Format("15:04:05"),
		model,
		mode,
		status,
		acct,
		logfmt.Pad(ttfbMS, chatTTFBWidth),
		logfmt.Pad(tokField, chatTokWidth),
		logfmt.Pad(tokpsField, chatRateWidth),
		logfmt.Pad(logfmt.Truncate(effortField(effortIn, effortOut), chatEffortWidth), chatEffortWidth),
		total.Seconds(),
	)
}
