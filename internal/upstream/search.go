// search.go 联网搜索接口：/agenttool/v1/search。
//
// 上游契约（对齐官方客户端 WebSearchTool，panel 实测 2026-10）：
//   - POST {chatBase}/agenttool/v1/search，超时 20s；
//   - 请求体 {"query":..., "type":"text2text", "max_results":N}；
//   - 响应为**扁平结构**（非 {code,msg,data} 信封）：
//     {query, type, provider, results[{title,url,snippet,site?,favicon?}],
//     total_results, response_time_ms}；
//   - 业务错误才带 code/msg（如 code=15003 unsupported search type/provider），
//     且通常同时伴随 HTTP 4xx。
//
// 为什么不能复用 doJSON：doJSON 解的是 {code,msg,data} 统一信封并只返回 data；
// 搜索成功响应没有 data 包裹，直接套信封会拿到空 data。故单独实现 doSearch，
// 但错误分类与 doJSON 保持同一口径（复用 Classify/Error），不引入第二套判定。
//
// 本文件只做「一次上游调用 + 结果解析」，不感知账号池。选号、在途租约、token
// 刷新、错误冷却与轮转全部由 internal/server/search.go 负责——与 chat 的分层
// 完全一致，搜索不能绕开任何一条既有账号健康/配额语义。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

const (
	// searchPath 搜索端点路径。官方 CLI 同名常量 WEB_SEARCH_API_PATH。
	// 与 chat 共用同一 endpoint（官方 product.endpoint 同时承载 /v2/chat/completions
	// 与 /agenttool/v1/search），故 base 复用 chatBase（含 CN/global realm 分流）。
	searchPath = "/agenttool/v1/search"

	// searchTimeout 搜索请求整体超时，对齐官方 WEB_SEARCH_TIMEOUT_MS（2e4）。
	// 必须显式设置：搜索是同步 JSON 请求（非流式），上游 hang 时若无超时会一直占用
	// 在途租约，把账号池名额耗光。
	searchTimeout = 20 * time.Second

	// SearchDefaultMaxResults 默认返回条数 = 官方硬编码值（buildRequestBody 固定 5）。
	SearchDefaultMaxResults = 5

	// SearchMaxResultsCap 服务端实际返回上限（实测约 10）。
	// 请求值超过该上限不会报错、也不会多返回，只是被服务端截断——故在入口收敛，
	// 避免把无效配置透传给上游并给调用方「配了就该生效」的错觉。
	SearchMaxResultsCap = 10

	// SearchMinQueryLen 官方 zod schema 的 query.min(2)。
	// 按 Unicode 字符计数（不是字节）：单个多字节字符不能被当作「两个字符」放行。
	SearchMinQueryLen = 2
)

// SearchResult 单条搜索结果。
// site / favicon 是上游可选字段（官方客户端同样按「有才带」处理）。
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
	Site    string `json:"site,omitempty"`
	Favicon string `json:"favicon,omitempty"`
}

// SearchOutcome 一次搜索的完整结果，字段名与上游响应一致，便于直接透出。
type SearchOutcome struct {
	Query          string         `json:"query"`
	Results        []SearchResult `json:"results"`
	TotalResults   int            `json:"total_results"`
	Provider       string         `json:"provider,omitempty"`
	ResponseTimeMs int64          `json:"response_time_ms,omitempty"`
}

// SearchHeaders 搜索请求头。
//
// 复用 ChatHeaders 的桌面端全套头族（UA 三段式 / Origin / Referer /
// X-CodeBuddy-Request / X-Machine-ID / X-Session-ID / Accept-Language /
// Authorization / X-User-Id / 企业与域头 / X-Agent-Purpose + X-IDE-* + X-Product /
// X-Device-Token / 会话头族 X-Conversation-* + X-Request-ID + X-Root-Request-ID + B3），
// 再按官方 WebSearchTool 的差异覆盖四处：
//   - Accept：搜索是非流式 JSON，官方为 application/json
//     （ChatHeaders 为 chat 流式设成 event-stream，必须覆盖）；
//   - Content-Type：官方搜索为 application/json;charset=UTF-8
//     （CommonHeaders 的 chat 形态是不带 charset 的 application/json）；
//   - X-Agent-Intent：官方固定 "craft"（搜索请求不携带会话 mode）；
//   - X-Agent-Runtime：官方 2.166.0 起必发，无 runtime 时回落 "other"。
//
// 为什么整体复用 ChatHeaders 而不是照抄官方 WebSearchTool 的精简头清单：
// 官方 CLI 的搜索头就是它同一个客户端的通用头（CLI 的 chat 与搜索共用
// `CLI/<ver>` UA、同一套会话头族），搜索并没有独立的头族。本项目出站身份是
// WorkBuddy 桌面端，chat 一直用 ChatHeaders 那套；若搜索另发一套精简头，上游会看到
// 「同一账号同一时段存在两种不同客户端指纹」，反而更像异常。故搜索 = 桌面端头族
// + 搜索特有的四项覆盖。clientIP 与 chat 同口径（仅 PassthroughIP 开启时注入）。
func (c *Client) SearchHeaders(req *http.Request, a *auth.Auth, clientIP string, meta ChatMeta) {
	c.ChatHeaders(req, a, clientIP, meta)
	req.Header.Set("Accept", "application/json")
	// 官方搜索 Content-Type 带 charset（MediaType.APPLICATION_JSON_UTF8）。
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Runtime", "other")
}

// ClampSearchMaxResults 把调用方给的条数收敛到 [1, SearchMaxResultsCap]；
// <=0 视为未指定，回落官方默认值。
func ClampSearchMaxResults(n int) int {
	if n <= 0 {
		return SearchDefaultMaxResults
	}
	if n > SearchMaxResultsCap {
		return SearchMaxResultsCap
	}
	return n
}

// searchRequestBody 出站搜索请求体。
//
// 用 struct 而不是 map：Go 的 map 序列化会按键名排序（max_results, query, type），
// 而官方 buildRequestBody 的字段顺序是 query → type → max_results。虽然 JSON 语义上
// 顺序无关，但保持与真实客户端逐字节同形，便于抓包比对与可能的 body 指纹风控。
type searchRequestBody struct {
	Query      string `json:"query"`
	Type       string `json:"type"`
	MaxResults int    `json:"max_results"`
}

// Search 对指定账号执行一次联网搜索（panel 兼容形态，不显式传客户端 IP）。
//
// 只做「一次上游调用 + 结果解析」，不涉及选号/轮转/冷却——那些属于 handler 的职责
// （与 chat 一致：upstream 层不感知账号池）。
//
// 返回的 error 在 HTTP 非 2xx 或业务 code != 0 时是 *Error（带 Kind，供 handler
// 决定是否罚号），其余（序列化/读体失败）是普通 error（传输层故障不罚号）。
func (c *Client) Search(ctx context.Context, a *auth.Auth, query string, maxResults int, meta ChatMeta) (*SearchOutcome, error) {
	return c.search(ctx, a, query, maxResults, "", meta)
}

// SearchWithClientIP 同 Search，但允许 handler 按请求传入客户端 IP（仅在
// Client.HotFields().PassthroughIP 开启时由 SearchHeaders 注入转发头）。传参而非
// 共享字段，避免并发请求交叉污染（与 ChatStream 的 clientIP 参数同纪律）。
func (c *Client) SearchWithClientIP(ctx context.Context, a *auth.Auth, query string, maxResults int, clientIP string, meta ChatMeta) (*SearchOutcome, error) {
	return c.search(ctx, a, query, maxResults, clientIP, meta)
}

// search 是 Search/SearchWithClientIP 的共同实现。
func (c *Client) search(ctx context.Context, a *auth.Auth, query string, maxResults int, clientIP string, meta ChatMeta) (*SearchOutcome, error) {
	query = strings.TrimSpace(query)
	if utf8.RuneCountInString(query) < SearchMinQueryLen {
		return nil, &Error{Kind: ErrClient, Msg: fmt.Sprintf("search query must be at least %d characters", SearchMinQueryLen)}
	}
	if a == nil {
		return nil, &Error{Kind: ErrClient, Msg: "search requires an authenticated account"}
	}
	body, err := json.Marshal(searchRequestBody{
		Query:      query,
		Type:       "text2text", // 官方硬编码；服务端只接受小写该值
		MaxResults: ClampSearchMaxResults(maxResults),
	})
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+searchPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.SearchHeaders(req, a, clientIP, meta)
	return c.doSearch(req, a)
}

// doSearch 发搜索请求并解析扁平响应。
//
// 错误处理与 doJSON 同口径：
//   - HTTP >= 400 → Classify 出 Kind，包成 *Error（Retry-After 头一并解析，
//     供 handler 的软冷却精度复用）；
//   - 2xx 但 code != 0 → 业务错误（ErrNone 时兜底 ErrClient），同样包成 *Error；
//     分类喂**完整 body**（含 code 字段），与 doJSON 的结构化判定同口径；
//   - 读体失败 → 普通 error（半截 body 不参与账号惩罚）。
//
// 出站客户端经 doEndpoint(a) 选择：账号绑定 proxy route 时走线路独立 client，
// 绑定无效直接拒绝发出；未绑定回落共享 HTTP——搜索与 billing/refresh 等短 RPC
// 同一出口语义，绝不绕过代理线路隔离。账号参数用可变参数保持 panel 源测试形态
// （doSearch(req)）兼容；生产路径由 Search/SearchWithClientIP 传入账号。
//
// 所有对外错误文本经 redactSearchSecrets 脱敏：上游错误 body 可能回显
// Authorization / access_token 等敏感片段，搜索端点不能把凭据带进响应或日志。
// 日志只打账号标签/状态码/Kind，绝不打 query、body 或 Authorization。
func (c *Client) doSearch(req *http.Request, acct ...*auth.Auth) (*SearchOutcome, error) {
	var a *auth.Auth
	if len(acct) > 0 {
		a = acct[0]
	}
	hc, err := c.doEndpoint(a)
	if err != nil {
		return nil, err
	}
	if hc == nil {
		return nil, fmt.Errorf("search: upstream http client is nil")
	}
	resp, err := hc.Do(req)
	if err != nil {
		log.Printf("ERR: [upstream] search acct=%s: transport error: %v", searchAccountLabel(a), err)
		// 与 ChatStreamContext 同口径：传输层失败后清空本次所用 transport 的空闲
		// 连接池，避免下一个请求继续捡到坏连接（绑定线路时只清该线路 client）。
		roundTripCloseIdle(hc.Transport)
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		msg := redactSearchSecrets(truncate(string(raw), 200))
		if strings.TrimSpace(msg) == "" {
			msg = fmt.Sprintf("upstream search returned HTTP %d", resp.StatusCode)
		}
		// ChannelCheck 由**完整 raw** 判定后固化在错误信封：Msg 只保留前 200
		// 字符，渠道校验文案若在截断点之后，下游 applyErrorPolicy 重新解析 Msg
		// 会漏判并把封套里的 request illegal 当账号封禁硬禁用。分类不能依赖
		// 被截断的展示字符串。
		ue := &Error{Kind: kind, Status: resp.StatusCode, Msg: msg,
			ChannelCheck: IsChannelCheck(string(raw))}
		if d, ok := ParseRetryAfter(resp.Header); ok {
			ue.RetryAfter = d
		}
		log.Printf("WARN: [upstream] search acct=%s: upstream %d kind=%s", searchAccountLabel(a), resp.StatusCode, kind)
		return nil, ue
	}
	// 成功响应没有 data 包裹：code/msg 与结果字段同级。code 可能是数字 0，
	// 也可能是字符串业务码（渠道校验实测 `"code":"11-128"`）；用 RawMessage
	// 探测，不能直接解进 int（否则 200+渠道校验会退化成 parse failed 普通错误，
	// 既丢失稳定 ChannelCheck 分类又让客户端看到 503 no healthy account）。
	var probe struct {
		Code  json.RawMessage `json:"code"`
		Msg   json.RawMessage `json:"msg"`
		Error json.RawMessage `json:"error"`
		SearchOutcome
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, redactSearchSecrets(truncate(string(raw), 120)))
	}
	if len(probe.Error) > 0 && strings.TrimSpace(string(probe.Error)) != "null" {
		// 兼容上游偶尔返回 {"error":{...}} 而非 code/msg 形态。
		kind := Classify(resp.StatusCode, string(raw))
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode,
			Msg:          redactSearchSecrets(truncate(string(raw), 200)),
			ChannelCheck: IsChannelCheck(string(raw))}
	}
	if len(probe.Code) > 0 && strings.TrimSpace(string(probe.Code)) != "null" {
		codeText := strings.TrimSpace(string(probe.Code))
		var numericCode int
		numericOK := json.Unmarshal(probe.Code, &numericCode) == nil
		if !numericOK || numericCode != 0 {
			kind := Classify(resp.StatusCode, string(raw))
			if kind == ErrNone {
				kind = ErrClient
			}
			var msgText string
			_ = json.Unmarshal(probe.Msg, &msgText)
			if strings.TrimSpace(msgText) == "" {
				msgText = truncate(string(raw), 160)
			}
			return nil, &Error{Kind: kind, Status: resp.StatusCode,
				Msg:          fmt.Sprintf("code=%s msg=%s", codeText, redactSearchSecrets(truncate(msgText, 160))),
				ChannelCheck: IsChannelCheck(string(raw))}
		}
	}
	out := probe.SearchOutcome
	// results 恒输出为数组（nil 会让 JSON 变成 null，调用方还要判空）。
	if out.Results == nil {
		out.Results = []SearchResult{}
	}
	// 上游个别情况下 total_results 缺省，用实际条数兜底，避免对外出现 0 条却有条目的矛盾。
	if out.TotalResults == 0 && len(out.Results) > 0 {
		out.TotalResults = len(out.Results)
	}
	return &out, nil
}

// searchSecretPatterns 搜索错误输出脱敏规则。只覆盖凭据形态，不碰普通业务文案：
//   - Authorization: Bearer <token>（含 JSON/日志嵌入形态）；
//   - wbk_ 分发密钥 / wbt_ 作用域令牌明文；
//   - access_token / refresh_token 键值（JSON 与 kv 两种形态）。
//
// 上游错误原文仍保留 code/msg/requestId 等排障信息；脱敏只把秘密替换为
// [REDACTED]，不改变错误分类（分类在脱敏前完成）。
var searchSecretPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?i)(authorization\s*[:=]\s*bearer\s+)[^\s"',;}\]]+`), `${1}[REDACTED]`},
	{regexp.MustCompile(`\bwbk_[A-Za-z0-9_-]{8,}\b`), `wbk_[REDACTED]`},
	{regexp.MustCompile(`\bwbt_[A-Za-z0-9_-]{8,}\b`), `wbt_[REDACTED]`},
	{regexp.MustCompile(`(?i)("?(?:access_token|refresh_token|accessToken|refreshToken)"?\s*[:=]\s*"?)[^"\s,;}]+`), `${1}[REDACTED]`},
}

// redactSearchSecrets 对搜索错误文本做凭据脱敏（见 searchSecretPatterns）。
func redactSearchSecrets(s string) string {
	if s == "" {
		return s
	}
	for _, p := range searchSecretPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

// searchAccountLabel 仅用于日志的账号标签（完整 UID 不落日志，复用 logfmt.Label）。
func searchAccountLabel(a *auth.Auth) string {
	if a == nil {
		return "-"
	}
	return logfmt.Label(a.UID, a.NicknameValue())
}
