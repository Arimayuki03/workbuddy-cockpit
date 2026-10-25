// search.go 联网搜索的 Anthropic Messages 兼容入口与轮转内核。
//
// 对外只暴露一个协议入口：POST /v1/search/messages（Anthropic Messages 兼容形态）。
// 存在理由：DSH 内置的搜索 provider（@deepseek-ai/dsh-web-search-deepseek）把搜索
// 请求硬编码发到 `${baseURL}/messages`，且要求响应是 Anthropic Messages 形态
// （含 web_search_tool_result block）。把该协议壳做在网关侧，客户端只需把搜索
// baseURL 指向 `<网关>/v1/search`，零代码改动即可用上本网关的搜索能力。
//
// 为什么不占用 /v1/messages：那是 Anthropic 模型对话的标准路径，将来网关若要
// 支持 Anthropic 协议的 chat 转发会与搜索冲突。搜索挂在 /v1/search/messages，
// 与既有 /v1/messages 完全隔离。
//
// 与真实 Anthropic 行为的差异（有意为之）：本端点不做模型调用。真实 Anthropic
// 的 web_search 是 server tool，由模型决定何时搜、并生成带引用的自然语言回答；
// 这里直接执行一次搜索并返回结构化结果块——因为网关没有模型，客户端（DSH）只从
// 结果块里取 sources，不消费自然语言回答。
//
// 账号语义：runSearch 复用当前 Handler 的 Pool 选号、Acquire/Release 在途租约、
// token 预刷新、applyErrorPolicy 错误冷却/熔断、WAF IP fail-fast 与 rotateBackoff，
// 与 /v1/chat/completions 的轮转内核同口径。搜索失败同样按 ErrKind 冷却对应账号，
// 不允许搜索流量绕开账号健康与配额语义。
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

const (
	// searchMessagesBodyLimit Messages 请求体上限：只含 query 与工具声明，1MB 富余。
	searchMessagesBodyLimit = 1 << 20

	// searchQueryPrefix 辅助搜索模型收到的固定指令前缀（DSH / 官方 CLI 的约定）。
	// 它是给模型看的指令，不是搜索词的一部分，必须剥掉后再送去搜索。
	searchQueryPrefix = "Perform a web search for the query:"

	// searchToolLabel 搜索流量在请求日志「模型」列里的标识。
	// 搜索不是模型调用，但该列留空会让面板无法区分这类流量与异常请求。
	searchToolLabel = "WorkBuddy搜索工具"

	// searchDefaultModel 请求未带 model 时响应里回落的模型标识。
	searchDefaultModel = "web-search"
)

// ---------------------------------------------------------------------------
// 挂载与鉴权边界
// ---------------------------------------------------------------------------

// MountSearch 挂载联网搜索路由（POST /v1/search/messages）。
//
// 与 MountAnthropic / MountResponses 同一导出模式：NewHandler 不隐式挂载，由 main
// 在 h.MountAnthropic() 同位调用 h.MountSearch()。这样现有测试/最小装配形态不会
// 因为新路由改变行为，集成方按需接线。
//
// 路由经 withAuth：与 /v1/chat/completions 完全同口径——同一把全局 api_key、
// 同一套 IP 黑白名单（withAuth 最前的 iprules）、同一套会话 cookie 通道
// （面板 dashboard 等价管理员身份）。搜索**不新增任何公开未鉴权入口**。
func (h *Handler) MountSearch() { h.mountSearch(h.mux) }

// mountSearch 挂载到给定 mux（导出 MountSearch 的内部实现，测试可直接调用）。
func (h *Handler) mountSearch(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/search/messages", h.withAuth(h.searchMessages))
}

// searchDistributedKeyRejected 报告当前请求是否必须拒绝（wbk_ 分发密钥）。
//
// 为什么搜索暂时拒绝 wbk_ 密钥：wbk_ 的 token/积分配额与每分钟限流只对**模型调用**
// 记账（keystore_auth.go 段3 从 chatStat 读取上游 usage）；搜索不产生模型 token/credit
// usage，若放行 wbk_ 密钥，等于给分发密钥开了一条不计配额、不计成本的旁路，直接
// 绕过现有 model/token/credit 配额。搜索配额语义（按搜索次数？按上游成本？）尚未
// 定义前，安全默认是拒绝，而不是静默放行。
//
// 判定来源：
//   - keyStateFrom(r) 非 nil：withAuth 已识别并放行了一把 wbk_ 密钥（段1）；
//   - 兜底：APIKey 为空时 withAuth 整段跳过（网关本就未鉴权），此时若 Bearer 形态
//     仍是 wbk_，也显式拒绝，避免「未鉴权部署 + wbk_ 头」绕过拒绝语义。
//
// 全局管理员 api_key（含面板会话 cookie 等价通道）不受影响。
func (h *Handler) searchDistributedKeyRejected(r *http.Request) bool {
	if keyStateFrom(r) != nil {
		return true
	}
	bearer := bearerToken(r)
	// 全局 api_key 极端情况下也可能以 wbk_ 开头（用户自定义）：它已经通过
	// withAuth 的全局 key 校验，不能仅凭前缀误判为分发密钥。
	if bearer != "" && bearer == h.apiKey() {
		return false
	}
	return looksLikeDistribKey(bearer)
}

// writeSearchKeyRejection 写 wbk_ 密钥拒绝体（Anthropic 错误形态）。
func writeSearchKeyRejection(w http.ResponseWriter) {
	writeAnthropicError(w, http.StatusForbidden, "permission_error",
		"search is not available for wbk_ distributed keys yet: search quota accounting is not defined, so allowing it would bypass existing model/token/credit quotas; use the gateway admin api_key for search")
}

// ---------------------------------------------------------------------------
// 请求解析与输入校验
// ---------------------------------------------------------------------------

// searchMessagesRequest Anthropic Messages 请求里本端点关心的字段。
type searchMessagesRequest struct {
	Model    string               `json:"model"`
	Stream   bool                 `json:"stream"`
	Tools    []searchMessagesTool `json:"tools"`
	Messages []searchMessagesTurn `json:"messages"`
}

// searchMessagesTool 工具声明。搜索工具两种形态都认：`{type:"web_search_20250305"}`
// 与 `{name:"web_search"}`（后者是部分客户端的简化形态）。
//
// max_uses 是 Anthropic web_search server tool 的「最多搜索几次」约束；本端点每次
// 只执行一次搜索，max_uses>=1 均可接受（实际只执行 1 次）。allowed_domains /
// blocked_domains / user_location 需要上游搜索 API 支持域名过滤或地理位置，当前
// 上游 /agenttool/v1/search 只接受 query/type/max_results，无法兑现——必须显式
// 拒绝，不能静默忽略后返回未过滤的结果（那会让调用方误以为约束已生效）。
type searchMessagesTool struct {
	Type           string          `json:"type"`
	Name           string          `json:"name"`
	MaxUses        *int            `json:"max_uses"`
	AllowedDomains []string        `json:"allowed_domains"`
	BlockedDomains []string        `json:"blocked_domains"`
	UserLocation   json.RawMessage `json:"user_location"`
}

// searchMessagesTurn 单条消息。content 既可能是字符串，也可能是 block 数组，
// 用 RawMessage 保留原始形态交给 extractSearchQuery 处理。
type searchMessagesTurn struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// searchMessagesContentBlock Messages content 数组里的文本块。
type searchMessagesContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// 以下类型别名/包装函数保留 panel messages.go 的内部命名面（extractMessagesQuery /
// buildMessagesSearchResponse / messagesTurnItem 等）。本文件是 panel 搜索协议壳在
// 当前仓库的唯一移植点；别名让按 panel 源测试形态编写的回归测试无需改字段名，
// 同时真实实现只保留一份（search* 类型），不会与 anthropic.go 的同名概念冲突。
type messagesTurnItem = searchMessagesTurn
type messagesTool = searchMessagesTool
type messagesRequest = searchMessagesRequest
type messagesContentBlock = searchMessagesContentBlock
type messagesSearchResultItem = searchResultItem
type messagesToolResultBlock = searchToolResultBlock
type messagesCitation = searchCitation
type messagesTextBlock = searchTextBlock
type messagesUsage = searchUsage
type messagesResponse = searchMessagesResponse

const (
	messagesBodyLimit   = searchMessagesBodyLimit
	messagesQueryPrefix = searchQueryPrefix
)

// declaresWebSearch 判断请求是否声明了 web_search server tool（panel 命名兼容）。
func declaresWebSearch(tools []messagesTool) bool {
	for _, t := range tools {
		if searchIsWebSearchTool(t) {
			return true
		}
	}
	return false
}

// extractMessagesQuery 是 extractSearchQuery 的 panel 命名兼容入口。
func extractMessagesQuery(items []messagesTurnItem) string {
	return extractSearchQuery(items)
}

// buildMessagesSearchResponse 是 buildSearchMessagesResponse 的 panel 命名兼容入口。
func buildMessagesSearchResponse(query, model string, out *upstream.SearchOutcome) messagesResponse {
	return buildSearchMessagesResponse(query, model, out)
}

// searchIsWebSearchTool 判断一条工具声明是否 web_search。
// 判定口径与 panel/DSH 内置 provider 一致：type 以 "web_search" 开头，
// 或 name 恰为 "web_search"。
func searchIsWebSearchTool(t searchMessagesTool) bool {
	return strings.HasPrefix(t.Type, "web_search") || t.Name == "web_search"
}

// validateSearchTools 校验 web_search 工具声明并返回首个搜索工具。
// 返回非空 reason 即 400。多个 web_search 声明时逐个校验约束，避免「第一个干净、
// 第二个带 allowed_domains」被静默放过。
func validateSearchTools(tools []searchMessagesTool) (searchMessagesTool, string) {
	var first searchMessagesTool
	declared := false
	for _, t := range tools {
		if !searchIsWebSearchTool(t) {
			continue
		}
		if !declared {
			first = t
			declared = true
		}
		if t.MaxUses != nil && *t.MaxUses < 1 {
			return searchMessagesTool{}, "web_search tool max_uses must be a positive integer"
		}
		if len(t.AllowedDomains) > 0 {
			return searchMessagesTool{}, `web_search tool constraint "allowed_domains" is not supported by this gateway; the upstream search API accepts only a query and result count`
		}
		if len(t.BlockedDomains) > 0 {
			return searchMessagesTool{}, `web_search tool constraint "blocked_domains" is not supported by this gateway; the upstream search API accepts only a query and result count`
		}
		if len(t.UserLocation) > 0 && string(t.UserLocation) != "null" {
			return searchMessagesTool{}, `web_search tool constraint "user_location" is not supported by this gateway; the upstream search API accepts only a query and result count`
		}
	}
	if !declared {
		return searchMessagesTool{}, "request does not declare the web_search tool; this endpoint only serves web search, not chat"
	}
	return first, ""
}

// extractSearchQuery 从 Messages 请求里还原真实搜索词。
//
// 约定（DSH / 官方 CLI 相同）：请求里恰好一条 user 消息，文本为
// `Perform a web search for the query: <query>`。前缀必须剥掉，否则会把指令本身
// 当成搜索词送去上游。
//
// 只取最后一条带文本的消息，不拼接整段历史。拼接会在客户端带多轮上下文时把
// 历史问句一起当搜索词送上游（且前缀因不在开头而剥不掉，最终搜的是一段垃圾）；
// 「最后一次提问」才是此处唯一正确的语义。与 panel search.go 口径一致。
func extractSearchQuery(items []searchMessagesTurn) string {
	joined := ""
	for _, m := range items {
		if len(m.Content) == 0 {
			continue
		}
		// content 为纯字符串（Anthropic 允许的简写形态）。
		var plain string
		if err := json.Unmarshal(m.Content, &plain); err == nil {
			if t := strings.TrimSpace(plain); t != "" {
				joined = t
			}
			continue
		}
		var blocks []searchMessagesContentBlock
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			continue
		}
		var text []string
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				text = append(text, b.Text)
			}
		}
		// 同一条消息内的多个 text block 拼接（一条消息被拆块是合法的）。
		if t := strings.TrimSpace(strings.Join(text, "\n")); t != "" {
			joined = t
		}
	}
	// 前缀大小写不敏感地剥离：不同客户端的模板大小写可能有差异。
	if len(joined) >= len(searchQueryPrefix) &&
		strings.EqualFold(joined[:len(searchQueryPrefix)], searchQueryPrefix) {
		joined = strings.TrimSpace(joined[len(searchQueryPrefix):])
	}
	return joined
}

// searchRealmOf 从请求 model 的 realm 前缀推断账号域（仅显式前缀才限制）：
//   - "global:..." → global；
//   - "cn:..." → cn；
//   - 裸名/空 → ""（不限域，按账号池既有策略选号）。
//
// 为什么不复用 resolveModel 的返回值：resolveModel 对裸名恒回落 "cn"，直接拿它当
// 选号域会让「只配 global 账号」的部署在 DSH 默认裸模型名（如 deepseek-v4-flash）
// 下 503。搜索没有模型调用，裸名不代表必须走 CN；显式前缀才表达路由意图。
func searchRealmOf(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	model = applyModelMap(model)
	if strings.HasPrefix(model, "global:") {
		return "global"
	}
	if strings.HasPrefix(model, "cn:") {
		return "cn"
	}
	return ""
}

// searchMetaFromRequest 从入站请求头提取会话头族并补全聚合主键。
//
// 搜索通常由某个会话内的 agent 发起，入站若带 X-Conversation-ID /
// X-Conversation-Request-ID / X-Trace-ID 必须原样透传给上游——上游按会话聚合
// 搜索流量，缺失会被视为请求不完整。客户端没带时才本地生成聚合主键；
// ConversationID 保持空串（不伪造一个假会话，避免污染后台真实会话聚合）。
// 客户端可控的头值先过 validConversationID（与 chat 同口径），非法值丢弃，
// 防止控制字符/超长值被写进上游请求头。
func searchMetaFromRequest(r *http.Request) upstream.ChatMeta {
	meta := upstream.ChatMeta{}
	if v := strings.TrimSpace(r.Header.Get("X-Conversation-ID")); v != "" && validConversationID(v) {
		meta.ConversationID = v
	}
	if v := strings.TrimSpace(r.Header.Get("X-Conversation-Request-ID")); v != "" && validConversationID(v) {
		meta.ConversationRequestID = v
	}
	if v := strings.TrimSpace(r.Header.Get("X-Trace-ID")); v != "" && validConversationID(v) {
		meta.TraceID = v
	}
	if meta.ConversationRequestID == "" {
		meta.ConversationRequestID = session.NewMessageID()
	}
	return meta
}

// ---------------------------------------------------------------------------
// 响应结构（Anthropic Messages）
// ---------------------------------------------------------------------------

// searchResultItem 响应里 `web_search_tool_result.content[]` 的单项。
// 注意：Anthropic 原生不带 snippet，摘要走 text 块的 citations，故这里只有 url/title。
type searchResultItem struct {
	Type  string `json:"type"`
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
}

// searchToolResultBlock 搜索结果块。
type searchToolResultBlock struct {
	Type      string             `json:"type"`
	ToolUseID string             `json:"tool_use_id"`
	Content   []searchResultItem `json:"content"`
}

// searchCitation 引用条目（snippet 的唯一承载处）。
type searchCitation struct {
	Type      string `json:"type"`
	URL       string `json:"url"`
	CitedText string `json:"cited_text"`
}

// searchTextBlock 承载 citations 的文本块。
type searchTextBlock struct {
	Type      string           `json:"type"`
	Text      string           `json:"text"`
	Citations []searchCitation `json:"citations,omitempty"`
}

// searchUsage Anthropic 响应必带的 usage。搜索不消耗模型 token，故为 0。
type searchUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// searchMessagesResponse Anthropic Messages 响应。
type searchMessagesResponse struct {
	ID           string      `json:"id"`
	Type         string      `json:"type"`
	Role         string      `json:"role"`
	Model        string      `json:"model"`
	Content      []any       `json:"content"`
	StopReason   string      `json:"stop_reason"`
	StopSequence any         `json:"stop_sequence"`
	Usage        searchUsage `json:"usage"`
}

// buildSearchMessagesResponse 把上游搜索结果渲染成 Anthropic Messages 响应。
//
// 结构契约（DSH 内置 provider 的 mapAnthropicResponse 逐条要求）：
//   - 必须至少有一个 type="web_search_tool_result" 的 block，否则客户端报
//     WEB_PROVIDER_ERROR；
//   - 该 block 的 content[] 每项 type="web_search_result"，url 非空；客户端还会按
//     url 去重，故空 url 的畸形条目在网关侧直接丢弃；
//   - snippet 只能从 text block 的 citations[].cited_text 取（web_search_result
//     本身不带摘要），故有摘要时追加一个 text block 承载引用。
func buildSearchMessagesResponse(query, model string, out *upstream.SearchOutcome) searchMessagesResponse {
	_ = query // 保留参数与 panel 实现同形；响应内容不消费 query（结果块已带 URL/标题）
	if out == nil {
		out = &upstream.SearchOutcome{}
	}
	items := make([]searchResultItem, 0, len(out.Results))
	citations := make([]searchCitation, 0, len(out.Results))
	for _, r := range out.Results {
		url := strings.TrimSpace(r.URL)
		if url == "" {
			// 客户端按 url 去重/渲染；空 url 会让 sources 出现不可点击的空条目。
			continue
		}
		title := r.Title
		if title == "" {
			title = url // 无标题时用 URL 兜底，客户端渲染 title ?? hostname 才不会空
		}
		items = append(items, searchResultItem{
			Type:  "web_search_result",
			URL:   url,
			Title: title,
		})
		if r.Snippet != "" {
			citations = append(citations, searchCitation{
				Type:      "web_search_result_location",
				URL:       url,
				CitedText: r.Snippet,
			})
		}
	}
	if model == "" {
		model = searchDefaultModel
	}
	// tool_use_id 与 msg id 每次调用都必须唯一：Anthropic 协议里它们标识的是
	// 「一次工具调用」，不是「一次查询的内容」。用 query 哈希会让同一 query 的
	// 重复搜索返回同一个 id（下游按 id 缓存/去重会互相覆盖），32 位哈希本身也可
	// 能碰撞。随机 16 hex 兼顾唯一性与可读性。
	callID := searchRandomHex(16)
	content := []any{searchToolResultBlock{
		Type:      "web_search_tool_result",
		ToolUseID: "srvtoolu_" + callID,
		Content:   items,
	}}
	if len(citations) > 0 {
		content = append(content, searchTextBlock{Type: "text", Text: "", Citations: citations})
	}
	return searchMessagesResponse{
		ID:           "msg_wb2api_" + callID,
		Type:         "message",
		Role:         "assistant",
		Model:        model,
		Content:      content,
		StopReason:   "end_turn",
		StopSequence: nil,
		Usage:        searchUsage{InputTokens: 0, OutputTokens: 0},
	}
}

// searchIDSeq 熵源故障时的兜底计数器（正常路径不使用）。
var searchIDSeq uint64

// searchRandomHex 返回 n 字节随机数的 hex 串。熵源失败时回落时间+计数派生值：
// 仍保证非空且每次调用都不同（id 唯一性是这里的硬要求）。
func searchRandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%0*x", n*2, uint64(time.Now().UnixNano())^atomic.AddUint64(&searchIDSeq, 1))
	}
	return hex.EncodeToString(b)
}

// searchAnthropicErrorType 把 HTTP 状态映射成 Anthropic 错误词表。
func searchAnthropicErrorType(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "api_error"
	}
}

// ---------------------------------------------------------------------------
// HTTP handler
// ---------------------------------------------------------------------------

// searchMessages 处理 POST /v1/search/messages（Anthropic Messages 兼容搜索端点）。
func (h *Handler) searchMessages(w http.ResponseWriter, r *http.Request) {
	// 请求级观测：搜索没有 chatStat 模型调用，但仍按 chat 同一套表格日志/请求日志/
	// metrics/usage 分桶记录（模型列用 searchToolLabel 标识）。只有通过 withAuth 的
	// 请求会进到这里，401 在鉴权层直接返回。
	st := newChatStat(time.Now(), session.Parsed{Model: searchToolLabel}, "")
	st.noteClientInfo(r, h.requestClientInfo())
	defer st.done()

	// wbk_ 分发密钥：在读取 body/选号之前拒绝，不消耗账号池配额，也不产生任何
	// 上游调用。全局管理员 api_key / 面板会话通道不受影响。
	if h.searchDistributedKeyRejected(r) {
		st.status = http.StatusForbidden
		st.setError("search rejected for distributed key")
		writeSearchKeyRejection(w)
		return
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, searchMessagesBodyLimit))
	if err != nil {
		var mbe *http.MaxBytesError
		status := http.StatusBadRequest
		if errors.As(err, &mbe) {
			status = http.StatusRequestEntityTooLarge
		}
		st.status = status
		st.setError("read body: " + err.Error())
		writeAnthropicError(w, status, searchAnthropicErrorType(status), "failed to read request body")
		return
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		st.status = http.StatusBadRequest
		st.setError("empty request body")
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "request body must be a JSON object")
		return
	}
	var req searchMessagesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		st.status = http.StatusBadRequest
		st.setError("invalid request json: " + err.Error())
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "request body must be a JSON object")
		return
	}
	// 本端点只提供同步 JSON 搜索；stream=true 的客户端期待 SSE，静默返回 JSON 会让
	// SDK 解析失败。显式 400，让调用方关掉 stream 或改用支持的端点。
	if req.Stream {
		st.status = http.StatusBadRequest
		st.setError("stream not supported")
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"stream=true is not supported by the search endpoint; retry with stream=false")
		return
	}
	tool, reason := validateSearchTools(req.Tools)
	if reason != "" {
		st.status = http.StatusBadRequest
		st.setError(reason)
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", reason)
		return
	}
	_ = tool // 工具声明只做校验；实际搜索次数固定为 1（max_uses 已限定 >=1）
	query := extractSearchQuery(req.Messages)
	if utf8.RuneCountInString(query) < upstream.SearchMinQueryLen {
		st.status = http.StatusBadRequest
		st.setError("missing search query")
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("could not read a search query from the request (need at least %d characters)", upstream.SearchMinQueryLen))
		return
	}

	realm := searchRealmOf(req.Model)
	if realm != "" {
		st.realm = realm
	}
	var clientIP string
	if h.cfg.Upstream.PassthroughIP {
		clientIP = upstream.ExtractClientIP(r)
	}
	out, fail := h.runSearch(r.Context(), query, 0, realm, clientIP, searchMetaFromRequest(r), st)
	if fail != nil {
		st.status = fail.status
		st.setError(fail.msg)
		msg := fail.msg
		if fail.hint != "" {
			msg = anthJoinHint(msg, fail.hint)
		}
		writeAnthropicError(w, fail.status, searchAnthropicErrorType(fail.status), msg)
		return
	}
	st.status = http.StatusOK
	// 搜索不产生模型 token usage，但请求本身成功了：显式置 hasUsage=true/toks=0，
	// 让 usage 分桶按成功计（hasUsage=false 会被 recordUsageBucket 当失败请求）。
	// 0 token/0 credit 是事实，不是缺失。
	st.hasUsage = true
	st.toks = 0
	model := strings.TrimSpace(req.Model)
	writeJSON(w, http.StatusOK, buildSearchMessagesResponse(query, model, out))
}

// ---------------------------------------------------------------------------
// 轮转内核
// ---------------------------------------------------------------------------

// searchFailure 轮转全部失败后的出口状态（由 handler 渲染成 Anthropic 错误形态）。
type searchFailure struct {
	status int
	msg    string
	hint   string
}

// searchFailureStatus 上游/调度失败 → 对外 HTTP 状态（与 panel 同口径）：
//   - 限流（ErrSoftRate）统一 429；
//   - 上游 4xx 原样透传（401/403/404/400 等）；
//   - 其余（5xx/传输层/池空）503。
func searchFailureStatus(uerr *upstream.Error) int {
	if uerr == nil {
		return http.StatusServiceUnavailable
	}
	if uerr.Kind == upstream.ErrSoftRate {
		return http.StatusTooManyRequests
	}
	if uerr.Status >= 400 && uerr.Status < 500 {
		return uerr.Status
	}
	// 2xx + 账号级业务错误（HTTP 状态码本身不携带语义）按 Kind 映射。
	if uerr.Status < 400 {
		switch uerr.Kind {
		case upstream.ErrHardCredit:
			return http.StatusPaymentRequired
		case upstream.ErrSessionDead:
			return http.StatusUnauthorized
		case upstream.ErrAccountFault:
			return http.StatusForbidden
		case upstream.ErrSoftRate:
			return http.StatusTooManyRequests
		case upstream.ErrBadParams, upstream.ErrContentBlocked, upstream.ErrPromptTooLong,
			upstream.ErrImageInvalid, upstream.ErrToolCallMismatch, upstream.ErrEmptyMessage,
			upstream.ErrClient:
			return http.StatusBadRequest
		}
	}
	return http.StatusServiceUnavailable
}

// runSearch 带账号轮转执行一次搜索，成功返回结果，失败返回出口状态。
//
// 轮转语义与 chat 完全一致：PickExcludingForRealm → Acquire 在途租约 →
// token 预刷新（NeedsRefresh/RefreshToken/SaveAtomic）→ 上游调用 →
// 按 ErrKind 走 applyErrorPolicy → 抖动退避换号。realm 来自请求 model 的显式
// 前缀（裸名不限域）；clientIP 由调用方按 PassthroughIP 口径传入。
//
// 搜索无模型维度：选号/租约/错误策略的 model 参数传空（不写模型级冷却），
// 租约走 Pool.Acquire/Release（不占 per-model 台账）。
func (h *Handler) runSearch(ctx context.Context, query string, maxResults int, realm, clientIP string, meta upstream.ChatMeta, st *chatStat) (*upstream.SearchOutcome, *searchFailure) {
	if ctx == nil {
		ctx = context.Background()
	}
	tried := map[string]bool{}
	var lastErr error
	var lastStatus int
	// 在途租约兜底释放：正常路径每次 Acquire 后立即 releaseHeld；defer 兜住
	// upstream 调用 panic / 未来新增 return 分支，绝不把名额永久占住。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}

	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.cfg.Pool.PickExcludingForRealm(tried, "", realm)
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		// 在途租约：与 chat 共用同一名额池。搜索也是真实上游调用，不占名额会让
		// 搜索流量绕过 MaxInFlight 限流。
		if !h.cfg.Pool.Acquire(acct.UID) {
			if !rotateBackoff(i, ctx) {
				break
			}
			continue
		}
		heldUID = acct.UID
		if st != nil {
			st.uid = acct.UID
			st.nick = acct.Nickname
			st.realm = acct.Realm()
		}
		// token 临近过期先刷新：搜索与 chat 共用同一 accessToken，把过期 token
		// 打到搜索端点会换回 401，被 applyErrorPolicy 当账号故障处置、误伤健康号。
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if rerr := h.cfg.Upstream.RefreshToken(acct); rerr != nil {
				releaseHeld()
				lastErr = rerr
				var ue *upstream.Error
				if errors.As(rerr, &ue) {
					lastStatus = ue.Status
					// 与 chat rotate 前的刷新失败同口径：401 网关页 → 直接禁用；
					// 12153 → 连续计数达阈才禁用；其余 → 记一次错误。
					if ue.Kind == upstream.ErrSessionDead {
						if upstream.IsGatewayAuthPageError(rerr) {
							h.cfg.Pool.Disable(acct.UID, upstream.SessionRevokedReason)
							log.Printf("WARN: [server] search refresh acct=%s: upstream 401 gateway page — auto-disabled (re-login required)",
								logfmt.Label(acct.UID, acct.Nickname))
						} else {
							h.cfg.Pool.NoteSessionDead(acct.UID)
						}
					} else {
						h.cfg.Pool.NoteError(acct.UID)
					}
				} else {
					h.cfg.Pool.NoteError(acct.UID)
					lastStatus = http.StatusServiceUnavailable
				}
				if !rotateBackoff(i, ctx) {
					break
				}
				continue
			}
			acct.BackfillRealm()
			if serr := acct.SaveAtomic(); serr != nil {
				// 刷新成功但落盘失败：下次启动会拿旧 token，必须暴露。
				log.Printf("ERR: [server] search refresh acct=%s: save auth failed: %v",
					logfmt.Label(acct.UID, acct.Nickname), serr)
			}
		}

		out, serr := h.cfg.Upstream.SearchWithClientIP(ctx, acct, query, maxResults, clientIP, meta)
		releaseHeld()
		if serr == nil {
			// 真成功：清熔断/连败/降权与 session-dead 计数（与 chat 成功同口径）。
			h.cfg.Pool.NoteSuccess(acct.UID)
			return out, nil
		}
		lastErr = serr
		var ue *upstream.Error
		if errors.As(serr, &ue) {
			lastStatus = ue.Status
			// 2xx + 业务 code!=0：先区分账号级 vs 请求级。
			//   - 14018 余额耗尽 / 12153 session dead / 429 限流 / 14017 账号故障
			//     即使 HTTP 200 也是账号级语义，必须走 applyErrorPolicy 冷却/禁用；
			//   - 其余（如 15003 unsupported search type）是请求级确定性错误，换号
			//     不会改变结果，也不该按账号故障罚号。直接 400 透传。
			if ue.Status < 400 {
				switch ue.Kind {
				case upstream.ErrHardCredit, upstream.ErrSoftRate, upstream.ErrSessionDead, upstream.ErrAccountFault:
					// 落到下方统一错误策略。
				default:
					return nil, &searchFailure{
						status: http.StatusBadRequest,
						msg:    ue.Msg,
						hint:   h.hintOf(ue.Kind, ue.Msg, "", false, ue),
					}
				}
			}
			// 与 chat 复用同一错误策略：余额不足/限流/WAF/账号故障各自冷却或熔断，
			// model 传空——搜索没有模型维度，不写模型级冷却。
			h.applyErrorPolicy(acct.UID, ue.Kind, ue.Msg, "", ue)
			// 请求级确定性错误：同一搜索换任何账号结果不变，立即透传、终止轮转，
			// 不浪费健康号配额（与 chat 的 11101/11115/11135/11148 分支同哲学）。
			switch ue.Kind {
			case upstream.ErrBadParams, upstream.ErrContentBlocked,
				upstream.ErrPromptTooLong, upstream.ErrImageInvalid,
				upstream.ErrToolCallMismatch, upstream.ErrEmptyMessage:
				return nil, &searchFailure{
					status: searchFailureStatus(ue),
					msg:    ue.Msg,
					hint:   h.hintOf(ue.Kind, ue.Msg, "", false, ue),
				}
			}
			// WAF IP 级 fail-fast（与 chat 同）：短窗多号 WAF 403 → 终止轮转，
			// 不再放大同出口 IP 的请求量。
			if ue.Kind == upstream.ErrWafBlock && h.wafIP.noteWaf(acct.UID) {
				break
			}
		} else {
			// 上游超时 / 停滞：不换号、不罚号（换号只会撞同一个慢上游）。
			if isUpstreamTimeout(serr, ctx.Err() != nil) {
				lastStatus = http.StatusServiceUnavailable
				lastErr = fmt.Errorf("%w: %v", errUpstreamTimeout, serr)
				log.Printf("WARN: [server] search upstream timeout acct=%s: %v (rotation stopped, account not penalized)",
					logfmt.Label(acct.UID, acct.Nickname), serr)
				break
			}
			// 客户端断连：不计连败（防止通过反复断连把健康号打出池）。
			if isClientDisconnect(ctx, serr) {
				lastStatus = http.StatusServiceUnavailable
				log.Printf("DEBUG: [server] search transport error caused by client disconnect, skip consecutive-fail uid=%s: %v",
					logfmt.UID8(acct.UID), serr)
			} else {
				h.cfg.Pool.NoteFailures(acct.UID)
				lastStatus = http.StatusServiceUnavailable
			}
		}
		if !rotateBackoff(i, ctx) {
			break
		}
	}

	// 末端出口（与 chat/panel 同口径）：
	//   - 选不到号 / 传输层失败 → 503 no_healthy_account
	//   - 上游错误 → 原文透传；限流语义映射 429，其余保留上游 4xx 状态码
	// gateway_hint 同样与 chat 对齐：本地调度错误给固定提示，上游错误按 Kind 判定，
	// 让运维在一个字段里就能分清「池子没号」和「上游报错」。
	hint := upstream.NoHealthyAccountHint()
	if lastErr == nil {
		return nil, &searchFailure{
			status: http.StatusServiceUnavailable,
			msg:    "all accounts are temporarily unavailable, please retry later",
			hint:   hint,
		}
	}
	status := http.StatusServiceUnavailable
	var ue *upstream.Error
	if errors.As(lastErr, &ue) {
		// 以**最后一条错误**的 Kind/Status 定状态，而不是轮转过程中残留的
		// lastStatus：前面账号 429、最后账号传输失败时不能被旧状态带偏。
		return nil, &searchFailure{
			status: searchFailureStatus(ue),
			msg:    ue.Msg,
			hint:   h.hintOf(ue.Kind, ue.Msg, "", false, ue),
		}
	}
	if lastStatus >= 400 && lastStatus < 500 {
		status = lastStatus
	}
	return nil, &searchFailure{
		status: status,
		msg:    lastErr.Error(),
		hint:   hint,
	}
}
