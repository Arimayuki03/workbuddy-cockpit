package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// rotation_governance_test.go 请求轮转循环三项治理（吸收自 workbuddy2api-panel）：
//   1. 11101 bad_params 立即 400 透传、停止轮转（panel commit fd5c5a6b / issue #99）；
//   2. 上游超时止损：不轮转、不罚号，末端 upstream_timeout 文案（panel commit 55d1e4d1）；
//   3. 成功判定/粘性绑定延后到流真成功之后，流中途 error 帧按内容处置账号
//      （panel commit 01ecb4a6）。

// sseOKWithErrFrame 成功正文帧之后夹一帧上游 error 帧（6004 模型级限流的流式形态）：
// 「200 已开流 + 一帧 error」是真实上游形态，此前网关在读第一帧前就记成功并绑粘性。
// error 帧必须在 [DONE] 之前——StreamHint 见 [DONE] 即停止读取（DONE 之后的任何
// 数据一律忽略，含垃圾帧）。
const sseOKWithErrFrame = sseOKBeforeDone +
	"data: {\"error\":{\"code\":6004,\"message\":\"您的使用量已超出频率限制\",\"requestId\":\"r-1\"}}\n\n" +
	"data: [DONE]\n\n"

// sseOKBeforeDone sseOK 的前两帧（正文 + usage 末帧），不含 [DONE]——供在 [DONE]
// 前插入 error 帧的流式夹具复用。
const sseOKBeforeDone = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n"

// sseAggregateWithErrFrame 非流式（stream=false）形态：上游照样可能以 SSE 流回错
//（ChatStream 对 200 一律给流，handler 按 stream 开关选择透传或聚合），聚合撞 error
// 帧返回 *upstream.Error（FrameKind 分类）。
const sseAggregateWithErrFrame = "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
	"data: {\"error\":{\"code\":6004,\"message\":\"您的使用量已超出频率限制\",\"requestId\":\"r-2\"}}\n\n"

// ---------------------------------------------------------------------------
// 治理 2：上游超时止损
// ---------------------------------------------------------------------------

// TestIsUpstreamTimeout 三态判定单测（panel 同口径）：
//   - net.Error.Timeout() → true（ResponseHeaderTimeout / Client.Timeout 形态）；
//   - context.DeadlineExceeded / os.ErrDeadlineExceeded → true；
//   - 客户端仍在（clientGone=false）+ context.Canceled → true（空闲看门狗掐流）；
//   - 客户端已走（clientGone=true）+ context.Canceled → false（客户端断连，走抖动分支）；
//   - 普通网络错误 → false。
func TestIsUpstreamTimeout(t *testing.T) {
	timeoutErr := &fakeNetTimeoutError{}
	wrappedDeadline := &upperError{inner: context.DeadlineExceeded}
	tests := []struct {
		name       string
		err        error
		clientGone bool
		want       bool
	}{
		{"nil error", nil, false, false},
		{"net timeout", timeoutErr, false, true},
		{"context deadline exceeded", context.DeadlineExceeded, false, true},
		{"os deadline exceeded", os.ErrDeadlineExceeded, false, true},
		{"wrapped deadline", wrappedDeadline, false, true},
		{"canceled while client alive (idle watchdog)", context.Canceled, false, true},
		{"canceled after client gone (real disconnect)", context.Canceled, true, false},
		{"plain network error", errors.New("connection reset"), false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUpstreamTimeout(tc.err, tc.clientGone); got != tc.want {
				t.Errorf("isUpstreamTimeout(%v, %v)=%v want %v", tc.err, tc.clientGone, got, tc.want)
			}
		})
	}
}

// fakeNetTimeoutError 实现 net.Error 的 Timeout 形态（ResponseHeaderTimeout 实测形态）。
type fakeNetTimeoutError struct{}

func (e *fakeNetTimeoutError) Error() string   { return "response header timeout" }
func (e *fakeNetTimeoutError) Timeout() bool   { return true }
func (e *fakeNetTimeoutError) Temporary() bool { return false }

// upperError 包装内层错误（验证 errors.Is 链路穿透，深层错误形态）。
type upperError struct{ inner error }

func (e *upperError) Error() string { return "wrapped: " + e.inner.Error() }
func (e *upperError) Unwrap() error { return e.inner }

// TestChatUpstreamTimeoutStopsRotationWithoutPenalty 端到端：上游传输层超时 →
// **不换号、不罚号**（NoteFailures 不喂、账号不冷却不降权），轮转立即终止（第二个
// 账号不被调用），末端回 503 + 可区分的 upstream_timeout 文案（不再是
// no_healthy_account——两者排查方向完全不同）。
func TestChatUpstreamTimeoutStopsRotationWithoutPenalty(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		// 传输层超时形态：roundTripFunc 返回 net.Error（Timeout=true）。
		return 0, "", false
	})
	// 定制 transport：对 at-bad 返回超时错误（newFakeUpstream 无法表达传输层错误）。
	up.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls[r.Header.Get("Authorization")]++
		return nil, &fakeNetTimeoutError{}
	})}
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000) // 确定性源 r=0 → 先选 bad
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s (want 503 upstream_timeout)", rec.Code, rec.Body)
	}
	assertJSONErrorCode(t, rec.Body.String(), "upstream_timeout")
	if !strings.Contains(rec.Body.String(), "upstream timed out") {
		t.Errorf("message should be the dedicated upstream_timeout text: %s", rec.Body)
	}
	// 止损：只有第一个账号被调用一次，轮转不再打第二个号（旧语义会换号重试）。
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 0 {
		t.Errorf("calls=%v want bad 1 次、good 0 次（超时止损不换号）", calls)
	}
	// 不罚号：无冷却、无禁用、无熔断计数、无连败计数。
	for _, uid := range []string{"bad", "good"} {
		st, _ := p.Status(uid)
		if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 || st.ConsecutiveFails != 0 {
			t.Errorf("upstream timeout must not penalize account %s: %+v", uid, st)
		}
	}
}

// ---------------------------------------------------------------------------
// 治理 3：成功判定与粘性绑定延后到流真成功之后
// ---------------------------------------------------------------------------

// TestChatStreamErrorFrameNotRecordedSuccess 流式：上游 200 + 正文帧 + error 帧 →
// **不记成功**（success_count 不加、errTotal 不加）、不绑粘性、粘性旧绑定解绑、
// 6004 按 ErrSoftRate 冷却该号。wire 上 error 帧照常透传（HTTP 头已发出只能 200）。
func TestChatStreamErrorFrameNotRecordedSuccess(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"u1"} },
	})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOKWithErrFrame, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	// 预绑定 u1 为粘性号：中途 error 帧后必须解绑（旧语义在流前绑定，error 帧后仍钉死）。
	sess.Bind("conv-1", "u1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-1"}}`)))
	// HTTP 头已发出，wire 恒 200；error 帧原样透传给客户端。
	if rec.Code != http.StatusOK {
		t.Fatalf("wire code=%d (headers already sent before error frame detected)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "6004") {
		t.Errorf("error frame should be passed through to client: %s", rec.Body)
	}
	// 不记成功：success_count 不加。
	if stt, _ := p.Status("u1"); stt.SuccessCount != 0 {
		t.Errorf("mid-stream error frame must not record success, success_count=%d", stt.SuccessCount)
	}
	// 按 ErrSoftRate（6004）冷却该号：账号级或模型级冷却生效（RateLimitedModels 非空）。
	if stt, _ := p.Status("u1"); !stt.Cooling && len(stt.RateLimitedModels) == 0 {
		t.Errorf("6004 error frame should cool the account (soft/model cooldown): %+v", stt)
	}
	// 不绑粘性且解绑旧绑定：会话键应无绑定（下次请求重新分配）。
	if uid, ok := st.lastUID("conv-1"); ok {
		t.Errorf("sticky binding must be unbound after mid-stream error frame, got %s", uid)
	}
}

// TestChatStreamSuccessStillBindsAfterRotation 流式真成功仍绑粘性（延后绑定不破坏
// 既有语义）：bad 500 失败、good 200 真成功 → 绑定收敛到 good、success 记在 good。
func TestChatStreamSuccessStillBindsAfterRotation(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"bad", "good"} },
	})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 500, `{"code":500}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	sess.Bind("conv-1", "bad")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-1"}}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("binding should follow final stream success to good, got %s ok=%v", uid, ok)
	}
	if stt, _ := p.Status("good"); stt.SuccessCount != 1 {
		t.Errorf("stream success should record success_count=1, got %d", stt.SuccessCount)
	}
}

// TestChatAggregateErrorFrameNotRecordedSuccess 非流式：上游 200 流内 error 帧 →
// 502 upstream_parse（HTTP 头未发出，可正常错误化）、**不记成功**、不绑粘性、
// 6004 按 ErrSoftRate 处置账号（此前聚合路径把错误帧前收到的成功记在账上）。
func TestChatAggregateErrorFrameNotRecordedSuccess(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"u1"} },
	})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseAggregateWithErrFrame, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	sess.Bind("conv-1", "u1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-1"}}`)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s (want 502 upstream_parse)", rec.Code, rec.Body)
	}
	assertJSONErrorCode(t, rec.Body.String(), "upstream_parse")
	if !strings.Contains(rec.Body.String(), "6004") {
		t.Errorf("502 message should carry upstream error frame body: %s", rec.Body)
	}
	if stt, _ := p.Status("u1"); stt.SuccessCount != 0 {
		t.Errorf("aggregate error frame must not record success, success_count=%d", stt.SuccessCount)
	}
	if stt, _ := p.Status("u1"); !stt.Cooling && len(stt.RateLimitedModels) == 0 {
		t.Errorf("6004 error frame should cool the account: %+v", stt)
	}
	if uid, ok := st.lastUID("conv-1"); ok {
		t.Errorf("sticky binding must be unbound after aggregate error frame, got %s", uid)
	}
}

// TestChatStreamEmptyUpstreamNotRecordedSuccess 空流（200+0 有效帧）是上游缺陷非
// 账号成功：不记成功（502 观测，handler_stream_empty_test 已锁定日志口径），
// 账号零动作（不冷却不罚）。
func TestChatStreamEmptyUpstreamNotRecordedSuccess(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, "", true // 空 body，非 SSE
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("wire code=%d want 200", rec.Code)
	}
	if stt, _ := p.Status("u1"); stt.SuccessCount != 0 {
		t.Errorf("empty stream must not record success, success_count=%d", stt.SuccessCount)
	}
	if stt, _ := p.Status("u1"); stt.Cooling || stt.Disabled || stt.ErrTotal != 0 {
		t.Errorf("empty stream is upstream defect, account untouched: %+v", stt)
	}
}

// ---------------------------------------------------------------------------
// sse 层单测：WithErrorFrameObserver（账号处置挂载点）
// ---------------------------------------------------------------------------

// TestStreamHintErrorFrameObserver StreamHint 对上游 error 帧调用观察者（原始
// payload），透传字节不变；正文帧不触发；无 error 帧不触发。
func TestStreamHintErrorFrameObserver(t *testing.T) {
	upstreamBody := "data: {\"id\":\"c1\",\"choices\":[]}\n\n" +
		"data: {\"error\":{\"code\":6004,\"message\":\"您的使用量已超出频率限制\"}}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	var observed []string
	err := upstream.StreamHint(rec, strings.NewReader(upstreamBody), nil,
		upstream.WithErrorFrameObserver(func(payload string) { observed = append(observed, payload) }))
	if err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	if len(observed) != 1 {
		t.Fatalf("observer calls=%d want 1, observed=%v", len(observed), observed)
	}
	var f struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(observed[0]), &f) != nil || f.Error.Code != 6004 {
		t.Errorf("observer payload should be the raw error frame: %s", observed[0])
	}
	// 透传字节不变：error 帧原样出现在响应里。
	if !strings.Contains(rec.Body.String(), `"code":6004`) {
		t.Errorf("error frame should still be forwarded verbatim: %s", rec.Body)
	}
}

// TestStreamHintNoObserverWithoutErrorFrame 正常流（无 error 帧）观察者零触发。
func TestStreamHintNoObserverWithoutErrorFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	var calls int
	err := upstream.StreamHint(rec, strings.NewReader(sseOK), nil,
		upstream.WithErrorFrameObserver(func(payload string) { calls++ }))
	if err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	if calls != 0 {
		t.Errorf("observer must not fire without error frames, calls=%d", calls)
	}
}

// TestAggregateErrorFrameReturnsTypedError Aggregate 撞流内 error 帧返回
// *upstream.Error（FrameKind 判 Kind=ErrSoftRate，Msg 装 payload 原文）——handler
// 非流式路径据此分类处置账号。锁既有行为（sse_aggregate_test 已覆盖部分），此处
// 从「账号处置」视角再锁一次 Kind/Msg 契约。
func TestAggregateErrorFrameReturnsTypedError(t *testing.T) {
	body := sseAggregateWithErrFrame
	resp, err := upstream.Aggregate(strings.NewReader(body))
	if err == nil {
		t.Fatalf("Aggregate should fail on in-stream error frame, got resp=%v", resp)
	}
	var ue *upstream.Error
	if !errors.As(err, &ue) {
		t.Fatalf("err should be *upstream.Error, got %T: %v", err, err)
	}
	if ue.Kind != upstream.ErrSoftRate {
		t.Errorf("Kind=%v want ErrSoftRate (6004 model rate limit)", ue.Kind)
	}
	if !strings.Contains(ue.Msg, "6004") {
		t.Errorf("Msg should carry the raw error frame payload: %s", ue.Msg)
	}
}

// TestClientDisconnectWriteFailureKeepsSuccess 流中途客户端写失败（断连）：
// 账号健康照常记成功（与 default 同语义），粘性照常绑定——观察者/解绑逻辑
// 不误伤该形态。用 httptest.NewServer + 立即关闭的客户端模拟写失败：
// 直接以自定义 ResponseWriter 在首帧后返回错误更可控。
type failAfterFirstWrite struct {
	http.ResponseWriter
	headerDone bool
	writes     int
}

func (f *failAfterFirstWrite) WriteHeader(code int) {
	f.headerDone = true
	f.ResponseWriter.WriteHeader(code)
}

func (f *failAfterFirstWrite) Write(p []byte) (int, error) {
	f.writes++
	if f.writes > 1 {
		return 0, errors.New("client gone")
	}
	return f.ResponseWriter.Write(p)
}

func TestClientDisconnectWriteFailureKeepsSuccess(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	// 直接调 handler（绕过 ServeMux），用会写失败的 ResponseWriter 模拟客户端断连。
	inner := httptest.NewRecorder()
	fw := &failAfterFirstWrite{ResponseWriter: inner}
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req = req.WithContext(context.Background())
	h.chatCompletions(fw, req)
	if stt, _ := p.Status("u1"); stt.SuccessCount != 1 {
		t.Errorf("client write failure mid-stream keeps account success, success_count=%d", stt.SuccessCount)
	}
}
