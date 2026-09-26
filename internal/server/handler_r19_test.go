// handler_r19_test.go R19 批修复的回归测试：
//   - 粘性号 realm 不符时必须置空回落 realm 感知轮换（不得直接出站不符 realm 账号）
//   - MaxRotate=1 时内容拦截降级重试不耗尽唯一名额（客户端收到 400 content_blocked 而非 503）
//   - 轮转中途失败的 errSummary 在最终 200 成功时清除（请求日志行 200 + 无错误摘要）
//   - fetchDynamicModels 缓存过期瞬间并发放大收敛为单次上游拉取（single-flight）
//   - 413 / 读体 400 就地返回前补一条轻量请求观测（metrics + 请求日志可见）
package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// TestChatStickyRealmMismatchFallsBackToRealmAwareRotation 粘性号 realm 不符 →
// 解绑并置空回落 realm 感知轮换，不得用不符 realm 的账号出站（session 仅注入
// Available 时粘性分配无 realm 维度，handler 侧 realm 闸是防御第二道）。
// 场景：global 请求（"global:m"），池内仅 CN 号；会话粘性绑定到该 CN 号——
// 仅注入 Available（无 AvailableForModel）时 ResolveForModel 会命中该绑定，
// 修复前 handler 直接用它出站（realm 闸失效）；修复后必须解绑回落轮换。
// realm 感知轮换下 CN 池对该 global 请求无候选 → no healthy account。
// 出站授权头固定断言：任何不符 realm 的出站都会让 fake 上游记到 calls。
func TestChatStickyRealmMismatchFallsBackToRealmAwareRotation(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var calls atomic.Int32
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		return 200, sseOK, true
	})
	st := newBindStore()
	sess := session.New(session.Config{
		TTL: time.Minute,
		// 仅注入 Available（无 AvailableForModel）：粘性分配/校验不带模型与 realm 维度，
		// 跨 realm 绑定在此口径下会命中——正是生产 wiring 缺失时的防御场景。
		Available: func() []string { return []string{"cn-only"} },
	})
	p := testPoolWith(&auth.Auth{UID: "cn-only", AccessToken: "at-cn", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})

	sess.Bind("conv-r19", "cn-only")
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:m","stream":true,"messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-r19"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if n := calls.Load(); n != 0 {
		t.Fatalf("mismatched-realm account must not be used for outbound, upstream calls=%d", n)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 (realm-aware rotation finds no candidate)", rec.Code)
	}
	assertJSONErrorCode(t, rec.Body.String(), "no_healthy_account")
	// 绑定已解绑（下次请求重新分配），不会复活回不符 realm 的号。
	if _, ok := st.lastUID("conv-r19"); ok {
		t.Errorf("mismatched-realm sticky binding should be unbound, binds=%v", st.binds)
	}
}

// TestChatStickyRealmMismatchUsesRealmAccount 跨 realm 绑定 + realm 内另有健康号 →
// 解绑后 realm 感知轮换必须选中同 realm 的号成功（而非空转 503）。
func TestChatStickyRealmMismatchUsesRealmAccount(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var globalCalls, cnCalls atomic.Int32
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-gl" {
			globalCalls.Add(1)
			return 200, sseOK, true
		}
		cnCalls.Add(1)
		return 200, sseOK, true
	})
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     newBindStore(),
		Available: func() []string { return []string{"cn-stray", "gl-good"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "cn-stray", AccessToken: "at-cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "gl-good", AccessToken: "at-gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})

	sess.Bind("conv-r19b", "cn-stray")
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:m","stream":true,"messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-r19b"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 via realm account)", rec.Code, rec.Body)
	}
	if n := cnCalls.Load(); n != 0 {
		t.Errorf("CN account must not serve global request, cn calls=%d", n)
	}
	if n := globalCalls.Load(); n != 1 {
		t.Errorf("global account calls=%d want 1", n)
	}
}

// TestDegradedRetryNotConsumingRotateSlotMaxRotate1 MaxRotate=1 + passthrough 首遇
// 内容拦截：降级重试不消耗唯一轮转名额 → 客户端收到 200（重试成功），而非
// 503 no_healthy_account。修复前 i 自增后循环直接退出。
func TestDegradedRetryNotConsumingRotateSlotMaxRotate1(t *testing.T) {
	var bodies [][]byte
	var mu sync.Mutex
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, raw)
			n := len(bodies)
			mu.Unlock()
			if n == 1 {
				// 首次（原始 system 指纹）→ 内容拦截。
				return &http.Response{
					StatusCode: 400,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":11-128,"msg":"blocked by security policy"}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough", MaxRotate: 1})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"原始指纹"},{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (degraded retry must proceed with MaxRotate=1)", rec.Code, rec.Body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("want 2 upstream calls (first 400 + degraded retry), got %d", len(bodies))
	}
	if !strings.Contains(string(bodies[1]), prompt.Degraded) {
		t.Errorf("second body should contain Degraded prompt: %s", bodies[1])
	}
}

// TestDegradedRetryExhaustedStillBlockedReturnsContentBlocked passthrough 首遇降级重试
// 后第二次仍拦 → 400 content_blocked（同号重试在豁免后进入第二分支）。
// MaxRotate=1 下旧实现此处是 503 no_healthy_account。
func TestDegradedRetryExhaustedStillBlockedReturnsContentBlocked(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11-128,"msg":"blocked by security policy"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough", MaxRotate: 1})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"原始指纹"},{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s (want 400 content_blocked)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"code":"content_blocked"`) {
		t.Errorf("want content_blocked code: %s", rec.Body)
	}
}

// TestRequestLogErrSummaryClearedOnFinalSuccess 轮转中途失败一次后换号成功 →
// 请求日志行 status=200 且 Error 为空（首个错误优先策略不得在 200 行残留摘要）。
func TestRequestLogErrSummaryClearedOnFinalSuccess(t *testing.T) {
	old := requestLog
	requestLog = &requestLogStore{buf: make([]requestLogEntry, 8)}
	t.Cleanup(func() { requestLog = old })

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 500, `{"code":500,"msg":"internal error"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000) // 确定性源 r=0 → 先选 bad 失败，再换 good 成功
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	snap := requestLogsSnapshot(1)
	if len(snap) != 1 {
		t.Fatalf("request log entries=%d want 1", len(snap))
	}
	e := snap[0]
	if e.Status != 200 {
		t.Fatalf("request log status=%d want 200", e.Status)
	}
	if e.Error != "" {
		t.Errorf("request log Error must be empty on final 200, got %q", e.Error)
	}
}

// TestRequestLogErrSummaryKeptOnFinalFailure 对照组：轮转全部失败 → 错误摘要保留
// （清空只发生在最终 200 出口，不吞真失败）。
func TestRequestLogErrSummaryKeptOnFinalFailure(t *testing.T) {
	old := requestLog
	requestLog = &requestLogStore{buf: make([]requestLogEntry, 8)}
	t.Cleanup(func() { requestLog = old })

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `{"code":500,"msg":"internal error"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d want 503", rec.Code)
	}
	snap := requestLogsSnapshot(1)
	if len(snap) != 1 {
		t.Fatalf("request log entries=%d want 1", len(snap))
	}
	if snap[0].Status != 503 || snap[0].Error == "" {
		t.Errorf("failure entry must keep status/error, got status=%d error=%q", snap[0].Status, snap[0].Error)
	}
}

// TestFetchDynamicModelsSingleFlight 缓存过期瞬间并发 5 个 goroutine 拉取 →
// 上游 FetchModels 只打一组（fake 收敛计数=2：企业端点 + /v3/config 两路），
// 其余等待者重查缓存共享结果。修复前 N 并发 → N×2 个上游请求。
func TestFetchDynamicModelsSingleFlight(t *testing.T) {
	resetModelsCache()
	t.Cleanup(resetModelsCache)

	// 慢 mock：拉取期间让并发 goroutine 都聚到门闩上。
	var calls atomic.Int32
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			time.Sleep(80 * time.Millisecond) // 慢响应：放大并发窗口
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":0,"data":{"models":[{"id":"dyn-sf","maxInputTokens":1024,"maxOutputTokens":1024}],"agents":[{"name":"cli","models":["dyn-sf"]}]}}`)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	const n = 5
	var wg sync.WaitGroup
	results := make([][]upstream.ModelInfo, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// 先在缓存上等一拍再并发进入，尽量把 5 个 goroutine 聚在同一 miss 窗口。
			results[i] = h.fetchDynamicModels()
		}(i)
	}
	wg.Wait()

	if got := calls.Load(); got != 2 {
		t.Errorf("upstream calls=%d want 2 (single fetch: enterprise + v3)", got)
	}
	for i, r := range results {
		if len(r) != 1 || r[0].ID != "dyn-sf" {
			t.Errorf("goroutine %d result=%v want [dyn-sf]", i, r)
		}
	}
	// 门闩复位：拉取完成后 fetching 必须为 false（并发下重复 fetch 仍可用）。
	dynamicModelsCache.Lock()
	fetching := dynamicModelsCache.fetching
	dynamicModelsCache.Unlock()
	if fetching {
		t.Error("latch not released after fetch completed")
	}
}

// TestFetchDynamicModelsSingleFlightRetryAfterMiss 门闩复位语义：首拉失败进负缓存后
// 把 lastFail 拨回冷却外再拉 → 仍能触发第二次拉取（门闩不吞后续请求）。
func TestFetchDynamicModelsSingleFlightRetryAfterMiss(t *testing.T) {
	resetModelsCache()
	t.Cleanup(resetModelsCache)

	var calls atomic.Int32
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		return 500, `boom`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	if got := h.fetchDynamicModels(); got != nil {
		t.Fatalf("first fetch should fail (500), got %v", got)
	}
	// 门闩已复位：并发 3 个仍各按负缓存短路（不再打上游）。
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.fetchDynamicModels()
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 2 {
		t.Errorf("upstream calls=%d want 2 (first fetch only, negative cache short-circuits rest)", got)
	}
	// 冷却外拨 → 门闩复位状态下可再次拉取（无悬挂 fetching）。
	dynamicModelsCache.Lock()
	dynamicModelsCache.lastFail = time.Now().Add(-10 * time.Minute)
	dynamicModelsCache.Unlock()
	_ = h.fetchDynamicModels()
	if got := calls.Load(); got != 4 {
		t.Errorf("upstream calls=%d want 4 (second fetch after cooldown)", got)
	}
}

// TestReadBodyErrorObservedInRequestLog 客户端断流（读体 400）→ 响应仍是 400
// invalid_request，且补入请求日志观测（status=400、模型/模式可见）。修复前该
// 路径完全不进 metrics/请求日志。
func TestReadBodyErrorObservedInRequestLog(t *testing.T) {
	old := requestLog
	requestLog = &requestLogStore{buf: make([]requestLogEntry, 8)}
	t.Cleanup(func() { requestLog = old })

	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", errReader{errors.New("client reset")}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}
	assertJSONErrorCode(t, rec.Body.String(), "invalid_request")
	if calls != 0 {
		t.Errorf("upstream must not be called, calls=%d", calls)
	}
	snap := requestLogsSnapshot(1)
	if len(snap) != 1 {
		t.Fatalf("read-body error must be observed in request log, entries=%d", len(snap))
	}
	if snap[0].Status != http.StatusBadRequest || snap[0].Mode != "sync" || snap[0].Error == "" {
		t.Errorf("entry mismatch: status=%d mode=%q error=%q", snap[0].Status, snap[0].Mode, snap[0].Error)
	}
}

// TestBodyTooLargeObservedInRequestLog 413 超限路径 → 响应不变，请求日志补入
// status=413 观测。
func TestBodyTooLargeObservedInRequestLog(t *testing.T) {
	old := requestLog
	requestLog = &requestLogStore{buf: make([]requestLogEntry, 8)}
	t.Cleanup(func() { requestLog = old })

	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxBodyBytes: 1024})

	over := `{"model":"glm-5.2","stream":true,"messages":[],"pad":"` + strings.Repeat("a", 2048) + `"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(over)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d want 413 body=%s", rec.Code, rec.Body)
	}
	if calls != 0 {
		t.Errorf("oversized body must not reach upstream, calls=%d", calls)
	}
	snap := requestLogsSnapshot(1)
	if len(snap) != 1 {
		t.Fatalf("413 must be observed in request log, entries=%d", len(snap))
	}
	// 截断后的 body 不可解析 → ParseRequest 零值：model="-"、mode="sync"（不编造）。
	// 状态与错误摘要必须可见（观测目的即让运维看到被拒的大请求）。
	if snap[0].Status != http.StatusRequestEntityTooLarge || snap[0].Error == "" {
		t.Errorf("entry mismatch: status=%d error=%q", snap[0].Status, snap[0].Error)
	}
	// metrics 同样可见（413 计入 failed）。
	snapMetrics := MetricsSnapshotOf()
	if snapMetrics.Total.Failed == 0 {
		t.Error("413 should be counted in metrics (failed)")
	}
}

// TestReadBodyErrorObservedInMetrics 读体 400 同样计入 metrics（对照断言独立成测，
// 与请求日志断言分开，避免环形缓冲覆盖顺序耦合）。
func TestReadBodyErrorObservedInMetrics(t *testing.T) {
	ResetMetrics()
	t.Cleanup(ResetMetrics)

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", errReader{errors.New("client reset")}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}
	snap := MetricsSnapshotOf()
	if snap.Total.Requests != 1 || snap.Total.Failed != 1 {
		t.Errorf("read-body 400 must be counted: requests=%d failed=%d want 1/1", snap.Total.Requests, snap.Total.Failed)
	}
}
