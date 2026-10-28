package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// issue #332（manager 0040）：自愈的判据必须与选号**同一个 realm 维度**。
//
// 现场：11 个国内版账号，deepseek-v4.1-flash 在全部账号上被 11102 负缓存，
// 而池里还有其它 realm 的账号（它们不被该模型的负缓存挡）。旧判据
// AvailableUIDsForModel(裸名) 看的是**全池**，于是返回非空 → 自愈不触发 →
// PickExcludingForRealm(realm=cn) 选不出号 → 503。
func TestModelBlockSelfHealIsRealmAware(t *testing.T) {
	cn1 := &auth.Auth{UID: "cn1", AccessToken: "a", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	cn2 := &auth.Auth{UID: "cn2", AccessToken: "b", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	gl := &auth.Auth{UID: "gl1", AccessToken: "c", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"}
	p := testPoolWith(cn1, cn2, gl)
	for _, uid := range []string{"cn1", "cn2"} {
		p.BlockModelBackoff(uid, "deepseek-v4.1-flash", "11102 model not available")
	}

	var calls int32
	up := &upstream.Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return testSSEResponse(200, sseOK), nil
	})}, ChatBaseCN: "https://test.invalid"}
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code == http.StatusServiceUnavailable {
		t.Fatalf("cn 池被负缓存挡住时必须自愈重试，实际 503：%s", rec.Body.String())
	}
	if atomic.LoadInt32(&calls) == 0 {
		t.Fatal("自愈后应当真的把请求发出去")
	}
	for _, uid := range []string{"cn1", "cn2"} {
		st, ok := p.Status(uid)
		if !ok {
			continue
		}
		if len(st.RateLimitedModels) != 0 {
			t.Fatalf("%s 的 11102 负缓存应已被清掉：%+v", uid, st.RateLimitedModels)
		}
	}
}

// 反向约束：模型**真的**在池里不可用时不能变成重试风暴。
// 自愈只清一次；上游始终 11102 时按错误返回，调用次数有上界。
func TestModelBlockSelfHealIsBounded(t *testing.T) {
	cn1 := &auth.Auth{UID: "cn1", AccessToken: "a", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	p := testPoolWith(cn1)
	p.BlockModelBackoff("cn1", "deepseek-v4.1-flash", "11102 model not available")

	var calls int32
	up := &upstream.Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return testSSEResponse(400, `{"code":11102,"msg":"model [deepseek-v4.1-flash] service info not found"}`), nil
	})}, ChatBaseCN: "https://test.invalid"}
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 4})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code == http.StatusOK {
		t.Fatalf("模型确实不存在时不该报成功：%s", rec.Body.String())
	}
	if n := atomic.LoadInt32(&calls); n > 8 {
		t.Fatalf("自愈不得变成重试风暴，实际上游调用 %d 次", n)
	}
}

// 跨请求节流（review HIGH）：第一次请求自愈后上游重新写回 11102 负缓存，
// 紧接着的第二次请求必须被闸门挡住——不再清缓存、不再打上游，直接 503。
// 没有闸门时每次请求都会清一次再打一次，上游调用数随请求数线性增长。
func TestModelBlockSelfHealThrottledAcrossRequests(t *testing.T) {
	cn1 := &auth.Auth{UID: "cn1", AccessToken: "a", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	p := testPoolWith(cn1)
	p.BlockModelBackoff("cn1", "deepseek-v4.1-flash", "11102 model not available")

	var calls int32
	up := &upstream.Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return testSSEResponse(400, `{"code":11102,"msg":"model [deepseek-v4.1-flash] service info not found"}`), nil
	})}, ChatBaseCN: "https://test.invalid"}
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 4})
	body := `{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec1.Code == http.StatusOK {
		t.Fatalf("上游始终 11102 时不该成功：%s", rec1.Body.String())
	}
	first := atomic.LoadInt32(&calls)
	if first == 0 {
		t.Fatal("第一次请求应先自愈再打上游")
	}

	// 上游已把负缓存写回；第二次请求落在 2 分钟窗口内。
	if blocked, _, _ := p.ModelBlockedNow("cn", "deepseek-v4.1-flash"); !blocked {
		t.Fatal("前置：第一次请求失败后应重新进入全池 11102 阻塞")
	}
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("被节流的请求应直接 503，实际 %d：%s", rec2.Code, rec2.Body.String())
	}
	if got := atomic.LoadInt32(&calls); got != first {
		t.Fatalf("2 分钟窗口内不得再次自愈清缓存/打上游：calls %d -> %d", first, got)
	}
	if blocked, _, _ := p.ModelBlockedNow("cn", "deepseek-v4.1-flash"); !blocked {
		t.Fatal("被节流的请求不得清掉负缓存")
	}
}

// 只清当前 realm（review HIGH）：cn1/cn2 与 gl1 都被 11102 挡住时，cn 请求的
// 自愈只能清 cn 账号的负缓存，global 账号的条目必须原样保留。
func TestModelBlockSelfHealClearsOnlyCurrentRealm(t *testing.T) {
	cn1 := &auth.Auth{UID: "cn1", AccessToken: "a", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	cn2 := &auth.Auth{UID: "cn2", AccessToken: "b", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	gl := &auth.Auth{UID: "gl1", AccessToken: "c", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"}
	p := testPoolWith(cn1, cn2, gl)
	for _, uid := range []string{"cn1", "cn2", "gl1"} {
		p.BlockModelBackoff(uid, "deepseek-v4.1-flash", "11102 model not available")
	}

	up := &upstream.Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return testSSEResponse(200, sseOK), nil
	})}, ChatBaseCN: "https://test.invalid"}
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code == http.StatusServiceUnavailable {
		t.Fatalf("cn 池自愈后应成功：%s", rec.Body.String())
	}
	for _, uid := range []string{"cn1", "cn2"} {
		st, _ := p.Status(uid)
		if len(st.RateLimitedModels) != 0 {
			t.Fatalf("%s 的 cn 负缓存应被清掉：%+v", uid, st.RateLimitedModels)
		}
	}
	st, _ := p.Status("gl1")
	if len(st.RateLimitedModels) != 1 {
		t.Fatalf("global realm 的 11102 负缓存不得被 cn 自愈误清：%+v", st.RateLimitedModels)
	}
}

// 闸门本身：同键首次放行、窗口内拒绝、超过窗口（回拨台账时刻）后再次放行；
// 台账规模达阈值时惰性清理过期条目。
func TestAllowModelBlockSelfHealGate(t *testing.T) {
	h := NewHandler(Config{})
	if !h.allowModelBlockSelfHeal("cn", "m") {
		t.Fatal("首次自愈应放行")
	}
	if h.allowModelBlockSelfHeal("cn", "m") {
		t.Fatal("2 分钟窗口内同键必须被节流")
	}
	if !h.allowModelBlockSelfHeal("global", "m") {
		t.Fatal("不同 realm 是独立的节流键")
	}
	h.modelBlockHealMu.Lock()
	h.modelBlockHealAt[modelBlockHealKey("cn", "m")] = time.Now().Add(-modelBlockSelfHealInterval - time.Second)
	h.modelBlockHealMu.Unlock()
	if !h.allowModelBlockSelfHeal("cn", "m") {
		t.Fatal("窗口过期的键应再次放行")
	}
	// 惰性清理：塞满阈值条目后，调用一次应把过期条目扫掉。
	h.modelBlockHealMu.Lock()
	stale := time.Now().Add(-modelBlockSelfHealInterval - time.Minute)
	for i := 0; i < modelBlockSelfHealThreshold; i++ {
		h.modelBlockHealAt[modelBlockHealKey("r", fmt.Sprintf("m%d", i))] = stale
	}
	h.modelBlockHealMu.Unlock()
	if !h.allowModelBlockSelfHeal("cn", "fresh") {
		t.Fatal("新键应放行")
	}
	h.modelBlockHealMu.Lock()
	defer h.modelBlockHealMu.Unlock()
	if _, ok := h.modelBlockHealAt[modelBlockHealKey("r", "m0")]; ok {
		t.Fatal("惰性清理应删除过期条目")
	}
}

// 自愈重试不消耗轮转名额（review 补测）：单账号 + MaxRotate=1 时，11102 负缓存
// 触发自愈后必须仍能把请求发出去且成功——i-- 抵消 for 自增，唯一的轮转名额
// 留给真正的那次上游调用。若 i-- 缺失，清缓存后循环直接结束，客户端收到 503。
func TestModelBlockSelfHealDoesNotConsumeRotateSlot(t *testing.T) {
	cn1 := &auth.Auth{UID: "cn1", AccessToken: "a", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	p := testPoolWith(cn1)
	p.BlockModelBackoff("cn1", "deepseek-v4.1-flash", upstream.ModelBlockReason)

	var calls int32
	up := &upstream.Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return testSSEResponse(200, sseOK), nil
	})}, ChatBaseCN: "https://test.invalid"}
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("MaxRotate=1 时自愈后仍应成功，code=%d body=%s", rec.Code, rec.Body)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("自愈重试不得额外消耗上游调用：calls=%d want 1", n)
	}
}

// 纯 6004 不进入自愈（review MEDIUM）：模型级冷却的 reason 是 6004 时，
// ClearModelBlocksForRealm 清 0 条，若此时先占用 2 分钟跨请求闸门，窗口内
// 真实 11102 就无法自愈。修复后 6004-only 直接走 503 model_blocked：不打
// 上游、不消耗闸门；随后（窗口内）改成 11102 全池锁仍能正常自愈。
func TestModelBlockSelfHealSkips6004OnlyBlock(t *testing.T) {
	cn1 := &auth.Auth{UID: "cn1", AccessToken: "a", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	p := testPoolWith(cn1)
	p.CooldownSoftForModel("cn1", time.Minute, time.Now().Add(10*time.Minute),
		"deepseek-v4.1-flash", "6004 model rate limit")

	var calls int32
	up := &upstream.Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return testSSEResponse(200, sseOK), nil
	})}, ChatBaseCN: "https://test.invalid"}
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1})
	body := `{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("6004 全池锁应回 503，code=%d body=%s", rec.Code, rec.Body)
	}
	assertJSONErrorCode(t, rec.Body.String(), "model_blocked")
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("6004-only 不得触发自愈重试：calls=%d want 0", n)
	}

	// 闸门未被占用：随后同键的 11102 自愈仍应放行。
	h.modelBlockHealMu.Lock()
	_, consumed := h.modelBlockHealAt[modelBlockHealKey("cn", "deepseek-v4.1-flash")]
	h.modelBlockHealMu.Unlock()
	if consumed {
		t.Fatal("6004-only 不得消耗 11102 自愈闸门")
	}
	// 2 分钟窗口内改成 11102 全池锁（覆盖同一 modelCooldowns 槽）：自愈应成功。
	p.BlockModelBackoff("cn1", "deepseek-v4.1-flash", upstream.ModelBlockReason)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("6004 换 11102 后应能自愈成功，code=%d body=%s", rec2.Code, rec2.Body)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("自愈应恰好打一次上游：calls=%d want 1", n)
	}
}
