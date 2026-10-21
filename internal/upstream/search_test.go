// search_test.go 联网搜索上游调用（/agenttool/v1/search）的契约测试。
//
// 覆盖：出站请求形状/头族、扁平响应解析、条数收敛、短 query 本地拒绝、
// HTTP/业务错误分类与 Retry-After、请求上下文取消、账号 proxy route 出口、
// realm 分流与错误文本凭据脱敏。全部用 httptest 假上游，不访问真实搜索服务。
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// searchRoundTripFunc 测试用 RoundTripper（与 client_test.go 的 rtFunc 同形；
// 这里独立命名，避免依赖其他测试文件的私有 helper）。
type searchRoundTripFunc func(*http.Request) (*http.Response, error)

func (f searchRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// searchTestClient 构造指向 httptest 假上游的 Client（CN 域，零值 GlobalEnabled
// 不影响 CN 测试；global 专门用例自行覆盖 ChatBaseGlobal）。
func searchTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
}

// TestSearchRequestShape 断言搜索出站请求的形状与官方契约一致：
// 路径 /agenttool/v1/search、请求体三字段、以及对齐官方 WebSearchTool 的请求头。
func TestSearchRequestShape(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	var gotRawBody string
	var got http.Header
	c := searchTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		got = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		gotRawBody = string(raw)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Errorf("body must be JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"query":"golang","type":"text2text","provider":"0","results":[],"total_results":0}`))
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	meta := ChatMeta{ConversationID: "conv-1", ConversationRequestID: "req-1", TraceID: "trace-1"}
	// 前后空白必须被裁掉；maxResults=0 → 回落官方默认值 5。
	if _, err := c.Search(context.Background(), a, "  golang  ", 0, meta); err != nil {
		t.Fatalf("search: %v", err)
	}

	if gotPath != "/agenttool/v1/search" {
		t.Errorf("path=%q want /agenttool/v1/search", gotPath)
	}
	if gotBody["query"] != "golang" {
		t.Errorf("query=%v want golang (trimmed)", gotBody["query"])
	}
	if gotBody["type"] != "text2text" {
		t.Errorf("type=%v want text2text (官方硬编码)", gotBody["type"])
	}
	if gotBody["max_results"] != float64(SearchDefaultMaxResults) {
		t.Errorf("max_results=%v want %d", gotBody["max_results"], SearchDefaultMaxResults)
	}
	// 字段顺序逐字节对齐官方 buildRequestBody（query → type → max_results）。
	if want := `{"query":"golang","type":"text2text","max_results":5}`; gotRawBody != want {
		t.Errorf("body=%s\n     want=%s（字段顺序须与官方一致）", gotRawBody, want)
	}

	// 官方 WebSearchTool 的关键头（本项目在 ChatHeaders 之上覆盖的四项 + 认证/身份）。
	if v := got.Get("Accept"); v != "application/json" {
		t.Errorf("Accept=%q want application/json（搜索非流式）", v)
	}
	if v := got.Get("Content-Type"); v != "application/json;charset=UTF-8" {
		t.Errorf("Content-Type=%q want application/json;charset=UTF-8", v)
	}
	if v := got.Get("X-Agent-Intent"); v != "craft" {
		t.Errorf("X-Agent-Intent=%q want craft", v)
	}
	if v := got.Get("X-Agent-Runtime"); v != "other" {
		t.Errorf("X-Agent-Runtime=%q want other（2.166.0 起官方必发）", v)
	}
	if v := got.Get("Authorization"); v != "Bearer at" {
		t.Errorf("Authorization=%q want Bearer at", v)
	}
	if v := got.Get("X-User-Id"); v != "u1" {
		t.Errorf("X-User-Id=%q want u1", v)
	}
	if v := got.Get("Origin"); v != "https://www.codebuddy.cn" {
		t.Errorf("Origin=%q want CN origin", v)
	}
	if v := got.Get("X-Agent-Purpose"); v != "conversation" {
		t.Errorf("X-Agent-Purpose=%q want conversation（复用桌面端头族）", v)
	}
	// 会话头族（复用 ChatHeaders）：X-Conversation-Message-ID 与 X-Request-ID 同值。
	if v := got.Get("X-Conversation-ID"); v != "conv-1" {
		t.Errorf("X-Conversation-ID=%q want conv-1", v)
	}
	if v := got.Get("X-Conversation-Request-ID"); v != "req-1" {
		t.Errorf("X-Conversation-Request-ID=%q want req-1", v)
	}
	if v := got.Get("X-Trace-ID"); v != "trace-1" {
		t.Errorf("X-Trace-ID=%q want trace-1", v)
	}
	if got.Get("X-Request-ID") == "" || got.Get("X-Conversation-Message-ID") != got.Get("X-Request-ID") {
		t.Error("X-Conversation-Message-ID 必须与 X-Request-ID 同值")
	}
	// 安全红线：出站绝不带 refresh token。
	if got.Get("X-Refresh-Token") != "" {
		t.Error("search 出站不得携带 X-Refresh-Token")
	}
}

// TestSearchParsesFlatResponse 断言扁平响应（非 {code,msg,data} 信封）能完整解析，
// 包括官方会读的 site / favicon 可选字段。
func TestSearchParsesFlatResponse(t *testing.T) {
	c := searchTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"query":"OpenAI 最新模型",
			"type":"text2text",
			"provider":"0",
			"results":[
				{"title":"模型发布说明","url":"https://help.openai.com/x","snippet":"摘要","site":"help.openai.com","favicon":"https://help.openai.com/f.ico"},
				{"title":"无 favicon","url":"https://example.com/y","snippet":"s2"}
			],
			"total_results":2,
			"response_time_ms":625
		}`))
	})
	out, err := c.Search(context.Background(), &auth.Auth{AccessToken: "at", UID: "u"}, "OpenAI 最新模型", 5, ChatMeta{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if out.Query != "OpenAI 最新模型" || out.Provider != "0" || out.TotalResults != 2 || out.ResponseTimeMs != 625 {
		t.Fatalf("outcome=%+v", out)
	}
	if len(out.Results) != 2 {
		t.Fatalf("results=%d want 2", len(out.Results))
	}
	first := out.Results[0]
	if first.Title != "模型发布说明" || first.URL != "https://help.openai.com/x" || first.Site != "help.openai.com" || first.Favicon == "" {
		t.Errorf("results[0]=%+v", first)
	}
	if out.Results[1].Favicon != "" {
		t.Errorf("results[1].Favicon=%q want empty", out.Results[1].Favicon)
	}
}

// TestSearchResultsNeverNull 断言空结果输出为空数组而非 null（JSON 消费方无需判空），
// 并在 total_results 缺省时用实际条数兜底。
func TestSearchResultsNeverNull(t *testing.T) {
	c := searchTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"query":"x","type":"text2text","provider":"0","results":[{"title":"t","url":"https://e/x"}],"total_results":0}`))
	})
	out, err := c.Search(context.Background(), &auth.Auth{AccessToken: "at", UID: "u"}, "xy", 5, ChatMeta{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if out.Results == nil {
		t.Fatal("Results must be non-nil")
	}
	if out.TotalResults != 1 {
		t.Errorf("TotalResults=%d want 1（缺省按实际条数兜底）", out.TotalResults)
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"results":[`) {
		t.Errorf("marshalled=%s want array", raw)
	}

	// nil results 的独立路径。
	c2 := searchTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"query":"x","type":"text2text","provider":"0","total_results":0}`))
	})
	out2, err := c2.Search(context.Background(), &auth.Auth{AccessToken: "at", UID: "u"}, "xy", 5, ChatMeta{})
	if err != nil {
		t.Fatalf("search2: %v", err)
	}
	if out2.Results == nil || len(out2.Results) != 0 {
		t.Errorf("Results=%v want non-nil empty", out2.Results)
	}
}

// TestSearchClampsMaxResults 断言条数在入口收敛：0 → 官方默认 5；超上限 → 10；
// 合法值原样。服务端实际上限约 10（panel 实测 15/30/50 均只回 9–10 条）。
func TestSearchClampsMaxResults(t *testing.T) {
	var got int
	c := searchTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		if v, ok := body["max_results"].(float64); ok {
			got = int(v)
		} else {
			t.Errorf("max_results missing: %s", raw)
		}
		_, _ = w.Write([]byte(`{"query":"x","results":[],"total_results":0}`))
	})
	cases := []struct{ in, want int }{
		{0, SearchDefaultMaxResults},
		{-3, SearchDefaultMaxResults},
		{1, 1},
		{5, 5},
		{10, 10},
		{11, SearchMaxResultsCap},
		{50, SearchMaxResultsCap},
	}
	for _, tc := range cases {
		if _, err := c.Search(context.Background(), &auth.Auth{AccessToken: "at", UID: "u"}, "xy", tc.in, ChatMeta{}); err != nil {
			t.Fatalf("in=%d: %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("max_results in=%d got=%d want=%d", tc.in, got, tc.want)
		}
	}
}

// TestSearchRejectsShortQuery 断言短 query 在本地就被拒（不浪费一次上游调用），
// 与官方 zod schema 的 query.min(2) 同口径。按 Unicode 字符计数：单个 CJK 字符
// 只有 1 个字符（3 字节），不能被字节长度误放行。
func TestSearchRejectsShortQuery(t *testing.T) {
	called := false
	c := searchTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{}`))
	})
	for _, q := range []string{"", " ", "a", "  a  ", "你"} {
		_, err := c.Search(context.Background(), &auth.Auth{AccessToken: "at", UID: "u"}, q, 5, ChatMeta{})
		var ue *Error
		if !errors.As(err, &ue) {
			t.Fatalf("query=%q err=%v want *Error", q, err)
		}
		if ue.Kind != ErrClient {
			t.Errorf("query=%q kind=%v want ErrClient", q, ue.Kind)
		}
	}
	if called {
		t.Error("短 query 不应触发上游调用")
	}
}

// TestSearchBusinessErrorIsTyped 断言 2xx + code!=0 也按 *Error 返回（上游业务错误约定），
// 且 Kind 落到 ErrClient 兜底——不能让业务错误被当成成功空结果。
func TestSearchBusinessErrorIsTyped(t *testing.T) {
	c := searchTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":15003,"msg":"unsupported search type: text2search"}`))
	})
	_, err := c.Search(context.Background(), &auth.Auth{AccessToken: "at", UID: "u"}, "xy", 5, ChatMeta{})
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("err=%v want *Error", err)
	}
	if ue.Kind != ErrClient {
		t.Errorf("kind=%v want ErrClient", ue.Kind)
	}
	if !strings.Contains(ue.Msg, "15003") {
		t.Errorf("msg=%q want 含 code 15003", ue.Msg)
	}
}

// TestSearchHTTPErrorClassified 断言 HTTP 4xx/5xx 走 Classify，并解析 Retry-After，
// 供 handler 决定是否罚号与冷却时长。
func TestSearchHTTPErrorClassified(t *testing.T) {
	cases := []struct {
		status       int
		body         string
		want         ErrKind
		wantRetrySec time.Duration
	}{
		// 401 + HTML 网关鉴权页在当前 Classify 里是 ErrSessionDead（需人工重登），
		// 不是通用 ErrClient。
		{http.StatusUnauthorized, `<html>401 Authorization Required</html>`, ErrSessionDead, 0},
		{http.StatusBadRequest, `{"code":400,"msg":"bad search request"}`, ErrClient, 0},
		{http.StatusTooManyRequests, `{"code":429,"msg":"rate limit exceeded, will reset in 60s"}`, ErrSoftRate, 90 * time.Second},
		{http.StatusInternalServerError, `{"code":500,"msg":"internal error"}`, ErrServer, 0},
	}
	for _, tc := range cases {
		c := searchTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if tc.status == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "90")
			}
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})
		_, err := c.Search(context.Background(), &auth.Auth{AccessToken: "at", UID: "u"}, "xy", 5, ChatMeta{})
		var ue *Error
		if !errors.As(err, &ue) {
			t.Fatalf("status=%d err=%v want *Error", tc.status, err)
		}
		if ue.Status != tc.status {
			t.Errorf("status=%d got=%d", tc.status, ue.Status)
		}
		if ue.Kind != tc.want {
			t.Errorf("status=%d kind=%v want %v", tc.status, ue.Kind, tc.want)
		}
		if ue.RetryAfter != tc.wantRetrySec {
			t.Errorf("status=%d retryAfter=%v want %v", tc.status, ue.RetryAfter, tc.wantRetrySec)
		}
	}
}

// TestSearchContextCancellation 断言请求 ctx 取消会立即中断上游调用（不是等 20s
// 超时），且错误链保持 context.Canceled 供 handler 识别客户端断连。
func TestSearchContextCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		// 不依赖服务端 r.Context() 的传播（部分连接在客户端取消后仍保持 active，
		// 会让 httptest.Server.Close 等满 5s）；测试用 release 显式放行 handler。
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Search(ctx, &auth.Auth{AccessToken: "at", UID: "u"}, "cancelled query", 5, ChatMeta{})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream search was not called")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Search did not return after context cancellation")
	}
	close(release)
}

// TestSearchUsesAccountProxyRoute 断言出站走 doEndpoint(a)：账号绑定未配置的
// proxy route 时拒绝发出（绝不静默回退直连），证明搜索没有绕开既有 egress 隔离。
func TestSearchUsesAccountProxyRoute(t *testing.T) {
	called := false
	c := &Client{
		HTTP: &http.Client{Transport: searchRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			called = true
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	a := &auth.Auth{AccessToken: "at", UID: "u", ProxyRoute: "missing-route"}
	_, err := c.Search(context.Background(), a, "route test", 5, ChatMeta{})
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("err=%v want proxy route not configured", err)
	}
	if called {
		t.Error("bound-but-missing proxy route must not fall back to direct HTTP")
	}
}

// TestSearchGlobalRealmBase 断言 realm 分流：global 账号走 ChatBaseGlobal 且
// Origin/Referer 切国际域，证明搜索复用 chatBase 的 realm 路由。
func TestSearchGlobalRealmBase(t *testing.T) {
	var gotPath, gotOrigin, gotAuthz string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotOrigin = r.Header.Get("Origin")
		gotAuthz = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"query":"q","results":[],"total_results":0}`))
	}))
	defer srv.Close()
	c := &Client{
		HTTP:           srv.Client(),
		ChatBaseCN:     "http://127.0.0.1:1",
		ChatBaseGlobal: srv.URL,
		GlobalEnabled:  true,
	}
	a, err := auth.Parse([]byte(`{"auth":{"accessToken":"gat","refreshToken":"grt","expiresAt":9999999999,"domain":"www.workbuddy.ai","realm":"global"},"account":{"uid":"ug","nickname":"G"}}`))
	if err != nil {
		t.Fatalf("parse auth: %v", err)
	}
	if _, err := c.Search(context.Background(), a, "global query", 5, ChatMeta{}); err != nil {
		t.Fatalf("search: %v", err)
	}
	if gotPath != "/agenttool/v1/search" {
		t.Errorf("path=%q", gotPath)
	}
	if gotOrigin != "https://www.workbuddy.ai" {
		t.Errorf("origin=%q want global origin", gotOrigin)
	}
	if gotAuthz != "Bearer gat" {
		t.Errorf("authorization=%q want global account token", gotAuthz)
	}
}

// TestSearchRedactsSecretsInErrors 断言上游错误 body 里的凭据不会原样进入
// *Error.Msg（随后可能进客户端响应与请求日志）。
func TestSearchRedactsSecretsInErrors(t *testing.T) {
	const secretBearer = "sk-live-super-secret-123"
	const secretAccess = "access-token-value-456"
	const secretWBK = "wbk_ABCDEFGHIJKLMNOPQRSTUV"
	c := searchTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"msg":"Authorization: Bearer ` + secretBearer + ` access_token=` + secretAccess + ` key=` + secretWBK + `"}`))
	})
	_, err := c.Search(context.Background(), &auth.Auth{AccessToken: "at", UID: "u"}, "xy", 5, ChatMeta{})
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("err=%v want *Error", err)
	}
	for _, secret := range []string{secretBearer, secretAccess, secretWBK} {
		if strings.Contains(ue.Msg, secret) {
			t.Errorf("error message leaked secret %q: %s", secret, ue.Msg)
		}
	}
	if !strings.Contains(ue.Msg, "[REDACTED]") {
		t.Errorf("error message=%q want redaction marker", ue.Msg)
	}
	if !strings.Contains(ue.Msg, "msg") {
		t.Errorf("error message=%q lost upstream diagnostic text", ue.Msg)
	}
}
