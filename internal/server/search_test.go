// search_test.go POST /v1/search/messages 的端到端/内核测试。
//
// 覆盖：Anthropic web_search_tool_result 响应结构、输入校验与 unsupported
// constraints、全局 api_key 鉴权、wbk_ 分发密钥拒绝、IP 规则前置保护、账号池
// 轮转/在途租约/token 预刷新/错误策略、请求上下文取消、realm 路由、client IP
// 透传与会话头透传。上游全部是 httptest 假服务，不访问真实搜索 API。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/keystore"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// ---------------------------------------------------------------------------
// 测试夹具
// ---------------------------------------------------------------------------

// searchUpstreamState 假上游的调用记录（所有读取经 mutex，避免并发测试竞态）。
type searchUpstreamState struct {
	mu           sync.Mutex
	searchCalls  int
	refreshCalls int
	searchAuth   []string
	searchPaths  []string
	searchHdrs   []http.Header
	searchBodies []string
}

func (s *searchUpstreamState) recordSearch(r *http.Request, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.searchCalls++
	s.searchAuth = append(s.searchAuth, r.Header.Get("Authorization"))
	s.searchPaths = append(s.searchPaths, r.URL.Path)
	s.searchHdrs = append(s.searchHdrs, r.Header.Clone())
	s.searchBodies = append(s.searchBodies, string(body))
}

func (s *searchUpstreamState) recordRefresh() {
	s.mu.Lock()
	s.refreshCalls++
	s.mu.Unlock()
}

func (s *searchUpstreamState) counts() (searchCalls, refreshCalls int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.searchCalls, s.refreshCalls
}

func (s *searchUpstreamState) authAt(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.searchAuth) {
		return ""
	}
	return s.searchAuth[i]
}

func (s *searchUpstreamState) headerAt(i int) http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.searchHdrs) {
		return nil
	}
	return s.searchHdrs[i]
}

// newSearchTestUpstream 构造假上游：/agenttool/v1/search 交给 onSearch；
// /v2/plugin/auth/token/refresh 默认返回一个成功信封（覆盖 token 预刷新路径）。
func newSearchTestUpstream(t *testing.T, onSearch func(http.ResponseWriter, *http.Request, *searchUpstreamState)) (*upstream.Client, *searchUpstreamState) {
	t.Helper()
	return newSearchTestUpstreamWithRefresh(t, onSearch, nil)
}

// newSearchTestUpstreamWithRefresh 同上，onRefresh 非 nil 时接管 refresh 响应。
func newSearchTestUpstreamWithRefresh(t *testing.T, onSearch func(http.ResponseWriter, *http.Request, *searchUpstreamState), onRefresh func(http.ResponseWriter, *http.Request, *searchUpstreamState)) (*upstream.Client, *searchUpstreamState) {
	t.Helper()
	st := &searchUpstreamState{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/token/refresh":
			st.recordRefresh()
			if onRefresh != nil {
				onRefresh(w, r, st)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":3600}}`)
		case "/agenttool/v1/search":
			raw, _ := io.ReadAll(r.Body)
			st.recordSearch(r, raw)
			if onSearch != nil {
				onSearch(w, r, st)
			} else {
				searchWriteSuccess(w, "golang", 1)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := &upstream.Client{
		HTTP:           srv.Client(),
		ChatBaseCN:     srv.URL,
		ChatBaseGlobal: srv.URL,
		GlobalEnabled:  true,
	}
	return c, st
}

// searchTestPool 构造确定性账号池（随机源恒 0 → weighted 选最高积分；
// 调用方可用 SetCredits 调整首选顺序）。
func searchTestPool(auths ...*auth.Auth) *pool.Pool {
	p := pool.New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	for _, a := range auths {
		p.Add(a)
		p.SetCredits(a.UID, 1000)
	}
	return p
}

// searchStatusOf 读取账号状态（pool.Status 返回 (Status,bool)，测试里统一断言存在）。
func searchStatusOf(t *testing.T, p *pool.Pool, uid string) pool.Status {
	t.Helper()
	st, ok := p.Status(uid)
	if !ok {
		t.Fatalf("pool has no account %q", uid)
	}
	return st
}

// searchTestBody 构造 DSH 内置 provider 的真实请求形态：
// model/max_tokens + 单条 user content block（固定前缀）+ web_search 工具。
func searchTestBody(query string) string {
	return fmt.Sprintf(
		`{"model":"deepseek-v4-flash","max_tokens":4096,"messages":[{"role":"user","content":[{"type":"text","text":%q}]}],"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":5}]}`,
		searchQueryPrefix+query,
	)
}

// searchTestBodyWithTool 同 searchTestBody，但替换 tools 数组内容（校验用例）。
func searchTestBodyWithTool(query, tools string) string {
	return fmt.Sprintf(
		`{"model":"deepseek-v4-flash","max_tokens":4096,"messages":[{"role":"user","content":[{"type":"text","text":%q}]}],"tools":[%s]}`,
		searchQueryPrefix+query, tools,
	)
}

func searchWriteSuccess(w http.ResponseWriter, query string, n int) {
	w.Header().Set("Content-Type", "application/json")
	var results []string
	for i := 0; i < n; i++ {
		results = append(results, fmt.Sprintf(`{"title":"title-%d","url":"https://example.com/%d","snippet":"snippet-%d"}`, i, i, i))
	}
	_, _ = fmt.Fprintf(w, `{"query":%q,"type":"text2text","provider":"0","results":[%s],"total_results":%d,"response_time_ms":12}`,
		query, strings.Join(results, ","), n)
}

func searchWriteErr(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// searchPost 发一个带指定 body 的 POST /v1/search/messages。
func searchPost(h *Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/search/messages", strings.NewReader(body))
	h.ServeHTTP(rec, req)
	return rec
}

// searchErrorBody 解析 Anthropic 错误信封。
type searchErrorBody struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeSearchError(t *testing.T, rec *httptest.ResponseRecorder) searchErrorBody {
	t.Helper()
	var e searchErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("not Anthropic error json: %v body=%s", err, rec.Body)
	}
	return e
}

// searchResp 解析 Anthropic Messages 响应骨架。
type searchResp struct {
	ID         string           `json:"id"`
	Type       string           `json:"type"`
	Role       string           `json:"role"`
	Model      string           `json:"model"`
	Content    []map[string]any `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      map[string]any   `json:"usage"`
}

func decodeSearchResp(t *testing.T, rec *httptest.ResponseRecorder) searchResp {
	t.Helper()
	var resp searchResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response json: %v body=%s", err, rec.Body)
	}
	return resp
}

// ---------------------------------------------------------------------------
// 鉴权 / IP / 密钥边界
// ---------------------------------------------------------------------------

// TestSearchAuthRequiresGlobalKey 断言搜索与 /v1/chat/completions 同一 withAuth：
// 无 key/错 key 401，正确全局 key 放行；对外只出账号 accessToken，不回带网关 key。
func TestSearchAuthRequiresGlobalKey(t *testing.T) {
	up, st := newSearchTestUpstream(t, nil)
	p := searchTestPool(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "admin-secret"})
	h.MountSearch()

	post := func(key string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/search/messages", strings.NewReader(searchTestBody("golang")))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := post(""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key code=%d want 401 body=%s", rec.Code, rec.Body)
	}
	if rec := post("wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key code=%d want 401", rec.Code)
	}
	// 只有 x-api-key（DSH 同时发 Authorization，但网关口径单源 Bearer）必须 401。
	recX := httptest.NewRecorder()
	reqX := httptest.NewRequest(http.MethodPost, "/v1/search/messages", strings.NewReader(searchTestBody("golang")))
	reqX.Header.Set("x-api-key", "admin-secret")
	h.ServeHTTP(recX, reqX)
	if recX.Code != http.StatusUnauthorized {
		t.Fatalf("x-api-key only code=%d want 401（鉴权口径单源 Bearer）", recX.Code)
	}

	rec := post("admin-secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("correct key code=%d body=%s", rec.Code, rec.Body)
	}
	if got, _ := st.counts(); got != 1 {
		t.Fatalf("upstream search calls=%d want 1", got)
	}
	if got := st.authAt(0); got != "Bearer at1" {
		t.Errorf("upstream Authorization=%q want account token at1（网关 key 不得转发上游）", got)
	}
	h.cfg.Pool.Close()
}

// TestSearchRejectsDistributedKey 断言 wbk_ 分发密钥即使通过 withAuth 段1，也必须在
// 搜索入口被拒绝（否则搜索会绕过该密钥的 token/credit 配额），且不触达上游。
func TestSearchRejectsDistributedKey(t *testing.T) {
	ks, err := keystore.Open("")
	if err != nil {
		t.Fatalf("open keystore: %v", err)
	}
	t.Cleanup(func() { _ = ks.Close() })
	plain, _, err := ks.Create(keystore.CreateParams{Name: "search-test"})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	up, st := newSearchTestUpstream(t, nil)
	p := searchTestPool(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "admin-secret", KeyStore: ks})
	h.MountSearch()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/search/messages", strings.NewReader(searchTestBody("golang")))
	req.Header.Set("Authorization", "Bearer "+plain)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wbk_ key code=%d want 403 body=%s", rec.Code, rec.Body)
	}
	e := decodeSearchError(t, rec)
	if e.Error.Type != "permission_error" || !strings.Contains(e.Error.Message, "wbk_") {
		t.Errorf("error=%+v want permission_error mentioning wbk_", e.Error)
	}
	if got, _ := st.counts(); got != 0 {
		t.Errorf("wbk_ rejected request reached upstream %d times", got)
	}
	h.cfg.Pool.Close()
}

// TestSearchIPRulesRunBeforeHandler 断言搜索路由经过 withAuth 的 IP 规则前置闸：
// 黑名单 IP 在鉴权/业务/上游之前被拒。
func TestSearchIPRulesRunBeforeHandler(t *testing.T) {
	rules, _ := NewIPRules("", IPRulesSpec{Blacklist: []string{"203.0.113.7"}})
	up, st := newSearchTestUpstream(t, nil)
	p := searchTestPool(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "admin-secret", IPRules: rules})
	h.MountSearch()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/search/messages", strings.NewReader(searchTestBody("golang")))
	req.RemoteAddr = "203.0.113.7:45678"
	req.Header.Set("Authorization", "Bearer admin-secret")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("blocked IP code=%d want 403 body=%s", rec.Code, rec.Body)
	}
	if got, _ := st.counts(); got != 0 {
		t.Errorf("blocked IP reached upstream %d times", got)
	}
	h.cfg.Pool.Close()
}

// ---------------------------------------------------------------------------
// 输入校验 / unsupported constraints
// ---------------------------------------------------------------------------

// TestSearchValidation 断言请求体/工具约束在本地拒绝，不浪费上游调用。
func TestSearchValidation(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		status int
		want   string
	}{
		{"invalid json", `{`, http.StatusBadRequest, "JSON object"},
		{"empty body", ``, http.StatusBadRequest, "JSON object"},
		{"no web_search tool", `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"computer_20250124","name":"computer"}]}`, http.StatusBadRequest, "web_search"},
		{"short query", searchTestBody("a"), http.StatusBadRequest, "at least 2 characters"},
		{"allowed_domains", searchTestBodyWithTool("golang", `{"type":"web_search_20250305","name":"web_search","allowed_domains":["example.com"]}`), http.StatusBadRequest, "allowed_domains"},
		{"blocked_domains", searchTestBodyWithTool("golang", `{"type":"web_search_20250305","name":"web_search","blocked_domains":["example.com"]}`), http.StatusBadRequest, "blocked_domains"},
		{"user_location", searchTestBodyWithTool("golang", `{"type":"web_search_20250305","name":"web_search","user_location":{"type":"approximate","city":"Tokyo"}}`), http.StatusBadRequest, "user_location"},
		{"max_uses zero", searchTestBodyWithTool("golang", `{"type":"web_search_20250305","name":"web_search","max_uses":0}`), http.StatusBadRequest, "max_uses"},
		{"stream unsupported", strings.Replace(searchTestBody("golang"), `"max_tokens":4096`, `"max_tokens":4096,"stream":true`, 1), http.StatusBadRequest, "stream=true"},
	}
	up, st := newSearchTestUpstream(t, nil)
	p := searchTestPool(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	h.MountSearch()
	defer h.cfg.Pool.Close()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := searchPost(h, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("code=%d want %d body=%s", rec.Code, tc.status, rec.Body)
			}
			e := decodeSearchError(t, rec)
			if e.Type != "error" || e.Error.Type != "invalid_request_error" {
				t.Errorf("error=%+v want invalid_request_error", e.Error)
			}
			if !strings.Contains(e.Error.Message, tc.want) {
				t.Errorf("message=%q want contains %q", e.Error.Message, tc.want)
			}
		})
	}
	if got, _ := st.counts(); got != 0 {
		t.Errorf("invalid requests reached upstream %d times", got)
	}
}

// TestSearchBodyTooLarge 断言 1MB 请求体上限返回 413，且不触达上游。
func TestSearchBodyTooLarge(t *testing.T) {
	up, st := newSearchTestUpstream(t, nil)
	p := searchTestPool(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	h.MountSearch()
	defer h.cfg.Pool.Close()

	body := strings.Repeat("a", searchMessagesBodyLimit+16)
	rec := searchPost(h, body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d want 413 body=%s", rec.Code, rec.Body)
	}
	e := decodeSearchError(t, rec)
	if e.Error.Type != "request_too_large" {
		t.Errorf("error type=%q want request_too_large", e.Error.Type)
	}
	if got, _ := st.counts(); got != 0 {
		t.Errorf("oversize request reached upstream %d times", got)
	}
}

// TestExtractSearchQuery 直测请求文本还原：前缀剥离、block 拼接、只取最后一条、
// 大小写不敏感、空消息安全。
func TestExtractSearchQuery(t *testing.T) {
	turns := func(items ...string) []searchMessagesTurn {
		out := make([]searchMessagesTurn, 0, len(items))
		for _, s := range items {
			out = append(out, searchMessagesTurn{Role: "user", Content: json.RawMessage(s)})
		}
		return out
	}
	cases := []struct {
		name  string
		items []searchMessagesTurn
		want  string
	}{
		{"plain string", turns(`"Perform a web search for the query: hello world"`), "hello world"},
		{"block array", turns(`[{"type":"text","text":"Perform a web search for the query: block query"}]`), "block query"},
		{"case insensitive prefix", turns(`"perform A WEB SEARCH FOR THE QUERY: Mixed"`), "Mixed"},
		{"last message only", turns(`"Perform a web search for the query: first"`, `"Perform a web search for the query: second"`), "second"},
		{"no prefix passthrough", turns(`"plain query"`), "plain query"},
		{"multiple text blocks", turns(`[{"type":"text","text":"Perform a web search for the query: a"},{"type":"text","text":"b"}]`), "a\nb"},
		{"empty", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractSearchQuery(tc.items); got != tc.want {
				t.Errorf("got=%q want=%q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 响应结构
// ---------------------------------------------------------------------------

// TestSearchMessagesCompatAliases 锁定 panel messages.go 命名兼容面：
// buildMessagesSearchResponse 产出的 block 动态类型必须能被 messagesToolResultBlock
// 断言命中（类型别名同一性），否则按 panel 源测试形态写的回归测试会失效。
func TestSearchMessagesCompatAliases(t *testing.T) {
	resp := buildMessagesSearchResponse("golang", "m", &upstream.SearchOutcome{
		Results: []upstream.SearchResult{{Title: "t", URL: "https://a.example", Snippet: "s"}},
	})
	if len(resp.Content) != 2 {
		t.Fatalf("content blocks=%d want 2", len(resp.Content))
	}
	if _, ok := resp.Content[0].(messagesToolResultBlock); !ok {
		t.Fatalf("content[0] type %T not assignable to messagesToolResultBlock", resp.Content[0])
	}
	if !strings.HasPrefix(resp.ID, "msg_wb2api_") {
		t.Errorf("id=%q want msg_wb2api_ prefix", resp.ID)
	}
}

// TestSearchMessagesResponseShape 端到端：扁平上游结果 → Anthropic
// web_search_tool_result + web_search_result + citations（snippet 唯一承载处）。
// 同时断言会话头族/client IP 透传、ID 每次唯一、空 URL 条目丢弃。
func TestSearchMessagesResponseShape(t *testing.T) {
	up, st := newSearchTestUpstream(t, func(w http.ResponseWriter, r *http.Request, _ *searchUpstreamState) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"query":"golang",
			"type":"text2text",
			"provider":"0",
			"results":[
				{"title":"","url":"https://a.example/x","snippet":"snippet A"},
				{"title":"B","url":"https://b.example/y"},
				{"title":"bad","url":""}
			],
			"total_results":3
		}`)
	})
	up.PassthroughIP = true
	up.SyncHot() // headers.go 的 IP 注入读热改快照；生产 main 装配期同步，测试显式同步
	p := searchTestPool(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	h.MountSearch()
	defer h.cfg.Pool.Close()

	post := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/search/messages", strings.NewReader(searchTestBody("golang")))
		req.Header.Set("X-Conversation-ID", "conv-1")
		req.Header.Set("X-Conversation-Request-ID", "req-1")
		req.Header.Set("X-Trace-ID", "trace-1")
		req.Header.Set("X-Forwarded-For", "203.0.113.9")
		h.ServeHTTP(rec, req)
		return rec
	}
	rec := post()
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	resp := decodeSearchResp(t, rec)
	if resp.Type != "message" || resp.Role != "assistant" || resp.StopReason != "end_turn" {
		t.Errorf("skeleton=%+v", resp)
	}
	if !strings.HasPrefix(resp.ID, "msg_wb2api_") {
		t.Errorf("id=%q want msg_wb2api_ prefix", resp.ID)
	}
	if resp.Model != "deepseek-v4-flash" {
		t.Errorf("model=%q want request model echo", resp.Model)
	}
	if resp.Usage["input_tokens"] != float64(0) || resp.Usage["output_tokens"] != float64(0) {
		t.Errorf("usage=%v want zero", resp.Usage)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("content blocks=%d want 2 body=%s", len(resp.Content), rec.Body)
	}

	toolBlock := resp.Content[0]
	if toolBlock["type"] != "web_search_tool_result" {
		t.Fatalf("block0 type=%v", toolBlock["type"])
	}
	if id, _ := toolBlock["tool_use_id"].(string); !strings.HasPrefix(id, "srvtoolu_") {
		t.Errorf("tool_use_id=%v want srvtoolu_ prefix", toolBlock["tool_use_id"])
	}
	items, _ := toolBlock["content"].([]any)
	if len(items) != 2 {
		t.Fatalf("result items=%d want 2（空 url 丢弃）", len(items))
	}
	first := items[0].(map[string]any)
	if first["type"] != "web_search_result" || first["url"] != "https://a.example/x" {
		t.Errorf("item0=%v", first)
	}
	if first["title"] != "https://a.example/x" {
		t.Errorf("item0 title=%v want URL fallback", first["title"])
	}
	if second := items[1].(map[string]any); second["title"] != "B" {
		t.Errorf("item1=%v", second)
	}

	textBlock := resp.Content[1]
	if textBlock["type"] != "text" {
		t.Fatalf("block1 type=%v", textBlock["type"])
	}
	citations, _ := textBlock["citations"].([]any)
	if len(citations) != 1 {
		t.Fatalf("citations=%d want 1", len(citations))
	}
	cite := citations[0].(map[string]any)
	if cite["type"] != "web_search_result_location" || cite["url"] != "https://a.example/x" ||
		cite["cited_text"] != "snippet A" {
		t.Errorf("citation=%v", cite)
	}

	// 会话头族与 client IP 透传（SearchHeaders 复用 ChatHeaders）。
	hdr := st.headerAt(0)
	if hdr == nil {
		t.Fatal("missing upstream header snapshot")
	}
	if hdr.Get("X-Conversation-ID") != "conv-1" || hdr.Get("X-Conversation-Request-ID") != "req-1" || hdr.Get("X-Trace-ID") != "trace-1" {
		t.Errorf("session headers not forwarded: conv=%q req=%q trace=%q",
			hdr.Get("X-Conversation-ID"), hdr.Get("X-Conversation-Request-ID"), hdr.Get("X-Trace-ID"))
	}
	if hdr.Get("X-Forwarded-For") != "203.0.113.9" || hdr.Get("X-Real-IP") != "203.0.113.9" {
		t.Errorf("client IP not forwarded: xff=%q xreal=%q", hdr.Get("X-Forwarded-For"), hdr.Get("X-Real-IP"))
	}
	if hdr.Get("X-Agent-Intent") != "craft" || hdr.Get("X-Agent-Runtime") != "other" {
		t.Errorf("search headers missing: intent=%q runtime=%q", hdr.Get("X-Agent-Intent"), hdr.Get("X-Agent-Runtime"))
	}

	// 同 query 重复调用必须换新 ID（下游按 id 缓存/去重）。
	rec2 := post()
	resp2 := decodeSearchResp(t, rec2)
	if resp2.ID == resp.ID {
		t.Errorf("repeated search returned same message id %q", resp.ID)
	}
}

// TestSearchEmptyResultsBlockNotNull 断言空结果仍输出 web_search_tool_result，
// content 为 []（不是 null），客户端不会因缺 block 报 WEB_PROVIDER_ERROR。
func TestSearchEmptyResultsBlockNotNull(t *testing.T) {
	up, _ := newSearchTestUpstream(t, func(w http.ResponseWriter, r *http.Request, _ *searchUpstreamState) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"query":"golang","type":"text2text","provider":"0","results":[],"total_results":0}`)
	})
	p := searchTestPool(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	h.MountSearch()
	defer h.cfg.Pool.Close()

	rec := searchPost(h, searchTestBody("golang"))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	resp := decodeSearchResp(t, rec)
	if len(resp.Content) != 1 || resp.Content[0]["type"] != "web_search_tool_result" {
		t.Fatalf("content=%v want one tool result block", resp.Content)
	}
	raw := rec.Body.String()
	if !strings.Contains(raw, `"content":[]`) {
		t.Errorf("empty result content must be [] not null: %s", raw)
	}
}

// ---------------------------------------------------------------------------
// 账号池：轮转 / 租约 / 刷新 / 错误策略 / 取消
// ---------------------------------------------------------------------------

// TestSearchRotatesAndReleasesLease 两个账号：首选 500、次选 200 → 轮转成功；
// 结束后两个账号的在途租约都必须是 0（Acquire 能重新拿到名额）。
func TestSearchRotatesAndReleasesLease(t *testing.T) {
	up, st := newSearchTestUpstream(t, func(w http.ResponseWriter, r *http.Request, _ *searchUpstreamState) {
		if r.Header.Get("Authorization") == "Bearer at1" {
			searchWriteErr(w, http.StatusInternalServerError, `{"code":500,"msg":"boom"}`)
			return
		}
		searchWriteSuccess(w, "golang", 1)
	})
	p := searchTestPool(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	p.SetCredits("u1", 5000) // 确保 500 的首选是 u1（轮转路径可断言）
	p.SetMaxInFlight(1)
	h := NewHandler(Config{Pool: p, Upstream: up})
	h.MountSearch()
	defer p.Close()

	rec := searchPost(h, searchTestBody("golang"))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got, _ := st.counts(); got != 2 {
		t.Fatalf("upstream calls=%d want 2 (500 then success)", got)
	}
	if st.authAt(0) != "Bearer at1" || st.authAt(1) != "Bearer at2" {
		t.Errorf("rotation auths=%q,%q want at1,at2", st.authAt(0), st.authAt(1))
	}
	for _, uid := range []string{"u1", "u2"} {
		if !p.Acquire(uid) {
			t.Errorf("lease leaked for %s: Acquire failed after request", uid)
		} else {
			p.Release(uid)
		}
	}
}

// TestSearchLeaseHeldDuringUpstreamCall 断言搜索真实占用在途名额：第一个请求
// 阻塞在上游时，第二个请求因 MaxInFlight=1 直接 503，不会并发穿透。
func TestSearchLeaseHeldDuringUpstreamCall(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	up, st := newSearchTestUpstream(t, func(w http.ResponseWriter, r *http.Request, _ *searchUpstreamState) {
		close(started)
		<-release
		searchWriteSuccess(w, "golang", 1)
	})
	p := searchTestPool(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	h := NewHandler(Config{Pool: p, Upstream: up})
	h.MountSearch()
	defer p.Close()

	firstRec := httptest.NewRecorder()
	firstReq := httptest.NewRequest(http.MethodPost, "/v1/search/messages", strings.NewReader(searchTestBody("golang")))
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(firstRec, firstReq)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first search did not reach upstream")
	}
	if inflight := searchStatusOf(t, p, "u1").InFlight; inflight != 1 {
		t.Fatalf("in_flight=%d want 1 while upstream call is blocked", inflight)
	}
	second := searchPost(h, searchTestBody("other"))
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("second search code=%d want 503 while lease held body=%s", second.Code, second.Body)
	}
	if got, _ := st.counts(); got != 1 {
		t.Errorf("second request reached upstream (calls=%d)", got)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("first search did not finish after release")
	}
	if firstRec.Code != http.StatusOK {
		t.Errorf("first search code=%d body=%s", firstRec.Code, firstRec.Body)
	}
	if inflight := searchStatusOf(t, p, "u1").InFlight; inflight != 0 {
		t.Errorf("in_flight=%d want 0 after response (lease leak)", inflight)
	}
}

// TestSearchCancellationReleasesLease 客户端取消 → 上游调用中断、租约释放、
// 不计连败（isClientDisconnect 语义）。
func TestSearchCancellationReleasesLease(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	up, _ := newSearchTestUpstream(t, func(w http.ResponseWriter, r *http.Request, _ *searchUpstreamState) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	p := searchTestPool(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	h := NewHandler(Config{Pool: p, Upstream: up})
	h.MountSearch()
	defer p.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/search/messages", strings.NewReader(searchTestBody("golang"))).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("search did not reach upstream")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not return after client cancellation")
	}
	close(release)
	if inflight := searchStatusOf(t, p, "u1").InFlight; inflight != 0 {
		t.Errorf("in_flight=%d want 0 after cancellation (lease leak)", inflight)
	}
	if st := searchStatusOf(t, p, "u1"); st.ConsecutiveFails != 0 {
		t.Errorf("consecutive_fails=%d want 0（客户端断连不计连败）", st.ConsecutiveFails)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code=%d want 503 body=%s", rec.Code, rec.Body)
	}
}

// TestSearchSoftRateMaps429AndCoolsAccount 429/Retry-After → 对外 429 +
// 账号软冷却（复用 applyErrorPolicy 的 Retry-After 分支）。
func TestSearchSoftRateMaps429AndCoolsAccount(t *testing.T) {
	up, _ := newSearchTestUpstream(t, func(w http.ResponseWriter, r *http.Request, _ *searchUpstreamState) {
		w.Header().Set("Retry-After", "45")
		searchWriteErr(w, http.StatusTooManyRequests, `{"code":429,"msg":"rate limit exceeded"}`)
	})
	p := searchTestPool(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	h.MountSearch()
	defer p.Close()

	rec := searchPost(h, searchTestBody("golang"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429 body=%s", rec.Code, rec.Body)
	}
	e := decodeSearchError(t, rec)
	if e.Error.Type != "rate_limit_error" {
		t.Errorf("error type=%q want rate_limit_error", e.Error.Type)
	}
	st := searchStatusOf(t, p, "u1")
	if !st.Cooling || !strings.Contains(st.Reason, "retry-after") {
		t.Errorf("account cooling=%v reason=%q want retry-after cooldown", st.Cooling, st.Reason)
	}
}

// TestSearchSessionDeadDisablesAccount 401 HTML 网关页 → 账号禁用（需重登），
// 对外保留 401，错误体 authentication_error。
func TestSearchSessionDeadDisablesAccount(t *testing.T) {
	up, _ := newSearchTestUpstream(t, func(w http.ResponseWriter, r *http.Request, _ *searchUpstreamState) {
		searchWriteErr(w, http.StatusUnauthorized, `<html>401 Authorization Required</html>`)
	})
	p := searchTestPool(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	h.MountSearch()
	defer p.Close()

	rec := searchPost(h, searchTestBody("golang"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401 body=%s", rec.Code, rec.Body)
	}
	e := decodeSearchError(t, rec)
	if e.Error.Type != "authentication_error" {
		t.Errorf("error type=%q want authentication_error", e.Error.Type)
	}
	st := searchStatusOf(t, p, "u1")
	if !st.Disabled || st.DisabledReason != upstream.SessionRevokedReason {
		t.Errorf("account disabled=%v reason=%q want session revoked", st.Disabled, st.DisabledReason)
	}
}

// TestSearchRefreshFailureDisablesSessionDead 断言预刷新失败同样是账号语义：
// 401 网关页 → 禁用账号、不调用搜索端点、对外 401。
func TestSearchRefreshFailureDisablesSessionDead(t *testing.T) {
	up, st := newSearchTestUpstreamWithRefresh(t, func(w http.ResponseWriter, r *http.Request, _ *searchUpstreamState) {
		t.Error("search endpoint must not be called when token refresh fails")
	}, func(w http.ResponseWriter, r *http.Request, _ *searchUpstreamState) {
		searchWriteErr(w, http.StatusUnauthorized, `<html>401 Authorization Required</html>`)
	})
	a := &auth.Auth{
		UID: "u1", AccessToken: "old-at", RefreshToken: "old-rt",
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
		FilePath:  filepath.Join(t.TempDir(), "auth.json"),
	}
	p := searchTestPool(a)
	h := NewHandler(Config{Pool: p, Upstream: up})
	h.MountSearch()
	defer p.Close()

	rec := searchPost(h, searchTestBody("golang"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401 body=%s", rec.Code, rec.Body)
	}
	if searches, refreshes := st.counts(); searches != 0 || refreshes != 1 {
		t.Errorf("calls search=%d refresh=%d want 0/1", searches, refreshes)
	}
	stAcct := searchStatusOf(t, p, "u1")
	if !stAcct.Disabled || stAcct.DisabledReason != upstream.SessionRevokedReason {
		t.Errorf("account disabled=%v reason=%q want session revoked", stAcct.Disabled, stAcct.DisabledReason)
	}
}

// TestSearchBusinessHardCreditOnHTTP200CoolsAccount 断言 200 + 业务 code=14018
// 仍是账号级余额耗尽语义：走 applyErrorPolicy 硬冷却到次日 04:00，不能因为
// HTTP 200 就当成请求级错误放行。
func TestSearchBusinessHardCreditOnHTTP200CoolsAccount(t *testing.T) {
	up, _ := newSearchTestUpstream(t, func(w http.ResponseWriter, r *http.Request, _ *searchUpstreamState) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":14018,"msg":"credits exhausted"}`)
	})
	p := searchTestPool(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	h.MountSearch()
	defer p.Close()

	rec := searchPost(h, searchTestBody("golang"))
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("code=%d want 402 body=%s", rec.Code, rec.Body)
	}
	st := searchStatusOf(t, p, "u1")
	if !st.Cooling || st.CoolKind != "hard_credit" {
		t.Errorf("account cooling=%v kind=%q want hard_credit", st.Cooling, st.CoolKind)
	}
}

// TestSearchRefreshesExpiringTokenBeforeCall 断言临近过期 token 先刷新，且搜索
// 使用刷新后的 accessToken（不会把过期 token 打到搜索端点误伤账号）。
func TestSearchRefreshesExpiringTokenBeforeCall(t *testing.T) {
	up, st := newSearchTestUpstream(t, func(w http.ResponseWriter, r *http.Request, _ *searchUpstreamState) {
		if got := r.Header.Get("Authorization"); got != "Bearer new-at" {
			t.Errorf("search Authorization=%q want refreshed token new-at", got)
		}
		searchWriteSuccess(w, "golang", 1)
	})
	authPath := filepath.Join(t.TempDir(), "auth.json")
	a := &auth.Auth{
		UID: "u1", AccessToken: "old-at", RefreshToken: "old-rt",
		ExpiresAt: time.Now().Add(time.Minute).Unix(), FilePath: authPath,
	}
	p := searchTestPool(a)
	h := NewHandler(Config{Pool: p, Upstream: up})
	h.MountSearch()
	defer p.Close()

	rec := searchPost(h, searchTestBody("golang"))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if _, refreshes := st.counts(); refreshes != 1 {
		t.Fatalf("refresh calls=%d want 1", refreshes)
	}
	if got := a.AccessTokenValue(); got != "new-at" {
		t.Errorf("account access token=%q want new-at", got)
	}
}

// TestSearchRealmRoutesExplicitPrefix 断言显式 realm 前缀只选对应域账号：
// global: → global 账号；cn: → CN 账号（裸名不限域）。
func TestSearchRealmRoutesExplicitPrefix(t *testing.T) {
	cnCalled, globalCalled := 0, 0
	cnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cnCalled++
		searchWriteSuccess(w, "golang", 1)
	}))
	defer cnSrv.Close()
	globalSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		globalCalled++
		searchWriteSuccess(w, "golang", 1)
	}))
	defer globalSrv.Close()

	up := &upstream.Client{
		HTTP:           &http.Client{Timeout: 5 * time.Second},
		ChatBaseCN:     cnSrv.URL,
		ChatBaseGlobal: globalSrv.URL,
		GlobalEnabled:  true,
	}
	globalAuth, err := auth.Parse([]byte(`{"auth":{"accessToken":"gat","refreshToken":"grt","expiresAt":9999999999,"domain":"www.workbuddy.ai","realm":"global"},"account":{"uid":"ug"}}`))
	if err != nil {
		t.Fatalf("parse global auth: %v", err)
	}
	p := searchTestPool(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		globalAuth,
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	h.MountSearch()
	defer p.Close()

	bodyGlobal := strings.Replace(searchTestBody("golang"), `"model":"deepseek-v4-flash"`, `"model":"global:deepseek-v4.1-flash"`, 1)
	if rec := searchPost(h, bodyGlobal); rec.Code != http.StatusOK {
		t.Fatalf("global code=%d body=%s", rec.Code, rec.Body)
	}
	if globalCalled != 1 || cnCalled != 0 {
		t.Errorf("global prefix calls: cn=%d global=%d want 0/1", cnCalled, globalCalled)
	}

	bodyCN := strings.Replace(searchTestBody("golang"), `"model":"deepseek-v4-flash"`, `"model":"cn:deepseek-v4.1-flash"`, 1)
	if rec := searchPost(h, bodyCN); rec.Code != http.StatusOK {
		t.Fatalf("cn code=%d body=%s", rec.Code, rec.Body)
	}
	if cnCalled != 1 || globalCalled != 1 {
		t.Errorf("cn prefix calls: cn=%d global=%d want 1/1", cnCalled, globalCalled)
	}
}

// TestSearchNoHealthyAccount 空池 → 503，Anthropic api_error，无上游调用。
func TestSearchNoHealthyAccount(t *testing.T) {
	up, st := newSearchTestUpstream(t, nil)
	p := searchTestPool()
	h := NewHandler(Config{Pool: p, Upstream: up})
	h.MountSearch()
	defer p.Close()

	rec := searchPost(h, searchTestBody("golang"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	e := decodeSearchError(t, rec)
	if e.Error.Type != "api_error" || !strings.Contains(e.Error.Message, "all accounts") {
		t.Errorf("error=%+v want all accounts api_error", e.Error)
	}
	if got, _ := st.counts(); got != 0 {
		t.Errorf("empty pool reached upstream %d times", got)
	}
}
