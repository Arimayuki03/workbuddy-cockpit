// keystore_auth_test.go wbk_ 多密钥分发接入网关鉴权链的接线测试（分段鉴权三段协议）。
//
// 覆盖清单：
//   - 零配置回归：KeyStore nil 时行为不变（全局 key 通行、wbk_ 401）；
//   - 段1（withAuth）：wbk_ 命中通行 + 全局 key 仍通行（兼容）、错 key/错前缀 401、
//     停用/过期/配额 403/429（Rejections 段，模型解析前）；
//   - 段2（chatCompletions，模型解析后）：realm/model 白名单 400、IP 白名单 400、
//     429 rate_limited 带 Retry-After: 60；
//   - /v1/models：realm 过滤 + 模型白名单裁剪（cn: 前缀写法等价）；
//   - 段3 记账（RecordUse）：成功请求计入 used_tokens/used_credits、失败请求不计入
//     （issue #52 语义）；
//   - 错误体：type 分流（invalid_request_error / insufficient_quota）。
//
// 风格对齐 handler_iprules_test.go 与 keystore_test.go：httptest + 真路由 + 真库。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/keystore"
	"workbuddy2api/internal/upstream"
)

// keyTestAccount 测试池账号（AccessToken=at1 即全局 key "at1"）。
func keyTestAccount() *auth.Auth {
	return &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}
}

// newKeyTestHandler 构建接线了 keystore 的 handler（真库，纯内存不落盘）。
func newKeyTestHandler(t *testing.T, ks *keystore.Store) *Handler {
	t.Helper()
	h := NewHandler(Config{
		Pool:     testPoolWith(keyTestAccount()),
		Upstream: upstream.New(),
		APIKey:   "at1", // 全局 key 与账号 AccessToken 同值（fake 上游校验 Bearer at1）
		KeyStore: ks,
	})
	t.Cleanup(func() { h.cfg.KeyStore = nil }) // 防御：不留悬挂引用
	return h
}

// mustCreateKey Create 一把密钥并返回明文（keystore_test.go mustCreate 的 server 侧版）。
func mustCreateKey(t *testing.T, ks *keystore.Store, p keystore.CreateParams) string {
	t.Helper()
	plain, _, err := ks.Create(p)
	if err != nil {
		t.Fatalf("Create(%+v) err=%v", p, err)
	}
	return plain
}

// keyErrorEnvelope OpenAI 错误信封（含 type 断言）。
type keyErrorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// decodeKeyError 解析错误信封并断言 code。
func decodeKeyError(t *testing.T, body, wantCode string) keyErrorEnvelope {
	t.Helper()
	var e keyErrorEnvelope
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("not openai error json: %v body=%s", err, body)
	}
	if e.Error.Code != wantCode {
		t.Fatalf("error code=%q want %q body=%s", e.Error.Code, wantCode, body)
	}
	return e
}

// chatReqBody 最小 chat 请求体。
const chatReqBody = `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`

// ---------------------------------------------------------------------------
// 零配置回归：KeyStore nil 时行为与 v1.15.1 一致
// ---------------------------------------------------------------------------

// TestNoKeyStoreBehaviorUnchanged 未接线 keystore：全局 key 通行、wbk_ 形态 Bearer
// 401（不 panic、不进分发分支）、/v1/models 原样输出（nil 裁剪 no-op）。
func TestNoKeyStoreBehaviorUnchanged(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(keyTestAccount()),
		Upstream: upstream.New(),
		APIKey:   "at1",
	})
	// 全局 key 通行。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer at1")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("global key must pass without keystore, code=%d body=%s", rec.Code, rec.Body)
	}
	// wbk_ 形态：不进 Resolve（无库），走原 401。
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/status", nil)
	req2.Header.Set("Authorization", "Bearer wbk_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("wbk_ bearer without keystore must be 401, code=%d", rec2.Code)
	}
	decodeKeyError(t, rec2.Body.String(), "invalid_api_key")
	// /v1/models nil 裁剪 no-op（不 panic）。
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("GET", "/v1/models", nil)
	req3.Header.Set("Authorization", "Bearer at1")
	h.ServeHTTP(rec3, req3)
	if rec3.Code != 200 {
		t.Fatalf("/v1/models with nil keystore must pass, code=%d body=%s", rec3.Code, rec3.Body)
	}
}

// ---------------------------------------------------------------------------
// 段1：withAuth——通行/兼容/401/Rejections
// ---------------------------------------------------------------------------

// TestKeyStoreResolveAndGlobalKeyCompat keystore 有密钥：wbk_ 命中通行，全局 key
// 仍通行（兼容承诺），完全未知的 wbk_ 401（与全局 key 失败同口径）。
func TestKeyStoreResolveAndGlobalKeyCompat(t *testing.T) {
	ks, err := keystore.Open("")
	if err != nil {
		t.Fatalf("Open err=%v", err)
	}
	plain := mustCreateKey(t, ks, keystore.CreateParams{Name: "分发"})
	h := newKeyTestHandler(t, ks)

	// wbk_ 通行（/status 无 model 维度校验，段1 即全链）。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("wbk_ key must pass, code=%d body=%s", rec.Code, rec.Body)
	}
	// 全局 key 兼容通行。
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/status", nil)
	req2.Header.Set("Authorization", "Bearer at1")
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("global key must still pass alongside keystore, code=%d", rec2.Code)
	}
	// 错的 wbk_（前缀真、明文假）→ 401 invalid_api_key。
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("GET", "/status", nil)
	req3.Header.Set("Authorization", "Bearer wbk_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	h.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusUnauthorized {
		t.Fatalf("wrong wbk_ key must be 401, code=%d", rec3.Code)
	}
	decodeKeyError(t, rec3.Body.String(), "invalid_api_key")
	// 非 wbk_ 的错 key → 同口径 401（原有路径不受影响）。
	rec4 := httptest.NewRecorder()
	req4 := httptest.NewRequest("GET", "/status", nil)
	req4.Header.Set("Authorization", "Bearer wrong-key")
	h.ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusUnauthorized {
		t.Fatalf("wrong global-format key must be 401, code=%d", rec4.Code)
	}
}

// TestKeyStoreRejectionsInWithAuth 段1 Rejections（模型解析前判定）：停用 403、
// 过期 403、token 配额 429（insufficient_quota type 分流）。
func TestKeyStoreRejectionsInWithAuth(t *testing.T) {
	ks, err := keystore.Open("")
	if err != nil {
		t.Fatalf("Open err=%v", err)
	}
	h := newKeyTestHandler(t, ks)

	// 停用：Update 置 Enabled=false → 403 key_disabled。
	plain := mustCreateKey(t, ks, keystore.CreateParams{Name: "停用"})
	if err := ks.Update(mustKeyID(t, ks, plain), func(k *keystore.Key) { k.Enabled = false }); err != nil {
		t.Fatalf("Update err=%v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("disabled key must be 403, code=%d body=%s", rec.Code, rec.Body)
	}
	e := decodeKeyError(t, rec.Body.String(), "key_disabled")
	if e.Error.Type != "invalid_request_error" {
		t.Errorf("403 type=%q want invalid_request_error", e.Error.Type)
	}
	if !strings.Contains(e.Error.Message, "key_disabled") {
		t.Errorf("message must embed reason short code, got %q", e.Error.Message)
	}

	// 过期：ExpiresAt 已过 → 403 key_expired。
	plainExp := mustCreateKey(t, ks, keystore.CreateParams{Name: "过期", ExpiresAt: 1})
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/status", nil)
	req2.Header.Set("Authorization", "Bearer "+plainExp)
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("expired key must be 403, code=%d", rec2.Code)
	}
	decodeKeyError(t, rec2.Body.String(), "key_expired")

	// token 配额耗尽：quota=10、已用 10 → 429 insufficient_quota。
	plainQuota := mustCreateKey(t, ks, keystore.CreateParams{Name: "配额", TokenQuota: 10})
	ks.RecordUse(mustKeySnapshot(t, ks, plainQuota), "", 10, 0)
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("GET", "/status", nil)
	req3.Header.Set("Authorization", "Bearer "+plainQuota)
	h.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusTooManyRequests {
		t.Fatalf("quota-exhausted key must be 429, code=%d body=%s", rec3.Code, rec3.Body)
	}
	e3 := decodeKeyError(t, rec3.Body.String(), "token_quota_exhausted")
	if e3.Error.Type != "insufficient_quota" {
		t.Errorf("429 type=%q want insufficient_quota", e3.Error.Type)
	}
}

// ---------------------------------------------------------------------------
// 段2：chatCompletions 模型相关校验（realm / 白名单 / IP / 限流）
// ---------------------------------------------------------------------------

// TestKeyStoreValidateRealmMismatch 段2：密钥限定 global 域、请求裸名（=cn）→
// 400 realm_mismatch（模型解析后拦截，不打上游）。
func TestKeyStoreValidateRealmMismatch(t *testing.T) {
	ks, err := keystore.Open("")
	if err != nil {
		t.Fatalf("Open err=%v", err)
	}
	plain := mustCreateKey(t, ks, keystore.CreateParams{Name: "global限定", Realm: "global"})
	h := newKeyTestHandler(t, ks)

	upCalled := false
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		upCalled = true
		return 200, sseOK, true
	})
	h.cfg.Upstream = up

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatReqBody))
	req.Header.Set("Authorization", "Bearer "+plain)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("realm mismatch must be 400, code=%d body=%s", rec.Code, rec.Body)
	}
	decodeKeyError(t, rec.Body.String(), "realm_mismatch")
	if upCalled {
		t.Error("upstream must not be called after key rejection")
	}
}

// TestKeyStoreValidateModelWhitelist 段2：模型白名单——命中放行（打到上游）、
// 未命中 400 model_not_allowed；白名单写 `cn:` 前缀与下发裸名等价（keyBareModel）。
func TestKeyStoreValidateModelWhitelist(t *testing.T) {
	ks, err := keystore.Open("")
	if err != nil {
		t.Fatalf("Open err=%v", err)
	}
	// 白名单写带前缀形态，请求用裸名：归一后等价 → 放行。
	plain := mustCreateKey(t, ks, keystore.CreateParams{Name: "白名单", ModelWhitelist: []string{"cn:glm-5.2"}})
	h := newKeyTestHandler(t, ks)
	upCalled := false
	h.cfg.Upstream = newFakeUpstream(t, func(authz string) (int, string, bool) {
		upCalled = true
		return 200, sseOK, true
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatReqBody))
	req.Header.Set("Authorization", "Bearer "+plain)
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !upCalled {
		t.Fatalf("whitelisted model (cn: prefix form) must pass, code=%d body=%s upCalled=%v", rec.Code, rec.Body, upCalled)
	}

	// 未命中 → 400 model_not_allowed，上游不再被打。
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"other-model","messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("Authorization", "Bearer "+plain)
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("non-whitelisted model must be 400, code=%d body=%s", rec2.Code, rec2.Body)
	}
	decodeKeyError(t, rec2.Body.String(), "model_not_allowed")
}

// TestKeyStoreValidateIPWhitelist 段2：IP 白名单不匹配 → 400 ip_not_allowed
// （fail-closed：httptest 默认来源 192.0.2.1 不在白名单内）。
func TestKeyStoreValidateIPWhitelist(t *testing.T) {
	ks, err := keystore.Open("")
	if err != nil {
		t.Fatalf("Open err=%v", err)
	}
	plain := mustCreateKey(t, ks, keystore.CreateParams{Name: "IP限定", IPWhitelist: []string{"203.0.113.0/24"}})
	h := newKeyTestHandler(t, ks)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatReqBody))
	req.Header.Set("Authorization", "Bearer "+plain)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("ip not allowed must be 400, code=%d body=%s", rec.Code, rec.Body)
	}
	decodeKeyError(t, rec.Body.String(), "ip_not_allowed")
}

// TestKeyStoreRateLimitedRetryAfter 段2：限流窗口打满 → 429 rate_limited，
// 响应带 Retry-After: 60 头、type=insufficient_quota。限流在 Rejections 段1
// 即可判定（与请求无关），走 /status 端点验证同一口径。
func TestKeyStoreRateLimitedRetryAfter(t *testing.T) {
	ks, err := keystore.Open("")
	if err != nil {
		t.Fatalf("Open err=%v", err)
	}
	plain := mustCreateKey(t, ks, keystore.CreateParams{Name: "限流", RateLimit: 2})
	h := newKeyTestHandler(t, ks)
	snap := mustKeySnapshot(t, ks, plain)

	// 放行成功 2 次（占满窗口），第 3 次被限流。
	ks.RecordUse(snap, "", 1, 0)
	ks.RecordUse(snap, "", 1, 0)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("rate limited key must be 429, code=%d body=%s", rec.Code, rec.Body)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "60" {
		t.Errorf("Retry-After=%q want 60", ra)
	}
	e := decodeKeyError(t, rec.Body.String(), "rate_limited")
	if e.Error.Type != "insufficient_quota" {
		t.Errorf("429 type=%q want insufficient_quota", e.Error.Type)
	}
}

// ---------------------------------------------------------------------------
// /v1/models：realm 过滤 + 白名单裁剪
// ---------------------------------------------------------------------------

// TestModelsFilteredForKey /v1/models 密钥裁剪：realm=global 只留 global: 条目；
// 白名单按映射后名字（下发口径）匹配，cn: 前缀写法等价；无密钥（全局 key）原样输出。
func TestModelsFilteredForKey(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, fullFieldsModelsBody, false // CN 目录：hy3 → 下发 "cn:hy3"
	})
	ks, err := keystore.Open("")
	if err != nil {
		t.Fatalf("Open err=%v", err)
	}
	h := NewHandler(Config{
		Pool:     testPoolWith(keyTestAccount()),
		Upstream: up,
		APIKey:   "at1",
		// GlobalEnabled 默认 false：global 名单空，realm=global 的密钥裁剪后为空列表
		//（口径本身由 filterModelsForKey 单测锁定，这里重点验证白名单与无密钥形态）。
		KeyStore: ks,
	})
	resetModelsCache()

	// 全局 key：原样输出（裁剪 no-op），cn:hy3 在列。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer at1")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("global key /v1/models must pass, code=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cn:hy3") {
		t.Fatalf("global key must see cn:hy3, body=%s", rec.Body)
	}

	// wbk_ 白名单含 hy3（cn: 前缀写法）：列表裁剪后仍含 cn:hy3（「列表里有的就能调用」）。
	plain := mustCreateKey(t, ks, keystore.CreateParams{Name: "单模型", ModelWhitelist: []string{"cn:hy3"}})
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/v1/models", nil)
	req2.Header.Set("Authorization", "Bearer "+plain)
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("wbk_ /v1/models must pass, code=%d", rec2.Code)
	}
	var listBody struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &listBody); err != nil {
		t.Fatalf("models not json: %v", err)
	}
	if len(listBody.Data) != 1 {
		t.Fatalf("whitelisted key must see exactly 1 model, got %d body=%s", len(listBody.Data), rec2.Body)
	}
	if listBody.Data[0]["id"] != "cn:hy3" {
		t.Errorf("id=%v want cn:hy3", listBody.Data[0]["id"])
	}

	// wbk_ 白名单不含 hy3：列表为空。
	plain2 := mustCreateKey(t, ks, keystore.CreateParams{Name: "别家", ModelWhitelist: []string{"cn:other"}})
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("GET", "/v1/models", nil)
	req3.Header.Set("Authorization", "Bearer "+plain2)
	h.ServeHTTP(rec3, req3)
	if rec3.Code != 200 {
		t.Fatalf("wbk_ /v1/models must pass, code=%d", rec3.Code)
	}
	if strings.Contains(rec3.Body.String(), "cn:hy3") {
		t.Errorf("non-whitelisted model must be pruned, body=%s", rec3.Body)
	}

	// realm=global 密钥：CN 列表整表裁掉（GlobalEnabled=false 无 global 条目 → 空）。
	plain3 := mustCreateKey(t, ks, keystore.CreateParams{Name: "global限定", Realm: "global"})
	rec4 := httptest.NewRecorder()
	req4 := httptest.NewRequest("GET", "/v1/models", nil)
	req4.Header.Set("Authorization", "Bearer "+plain3)
	h.ServeHTTP(rec4, req4)
	if rec4.Code != 200 {
		t.Fatalf("wbk_ /v1/models must pass, code=%d", rec4.Code)
	}
	if strings.Contains(rec4.Body.String(), "cn:hy3") {
		t.Errorf("global-realm key must not see cn: entries, body=%s", rec4.Body)
	}
}

// ---------------------------------------------------------------------------
// 段3：RecordUse——成功计入 / 失败不计入（issue #52）
// ---------------------------------------------------------------------------

// TestRecordUseOnSuccessAndNotOnFailure 成功请求（stream 200 + usage）计入
// used_tokens/used_credits/限流窗口/LastUsedAt；被段2 拒绝的请求全部不计入。
func TestRecordUseOnSuccessAndNotOnFailure(t *testing.T) {
	ks, err := keystore.Open("")
	if err != nil {
		t.Fatalf("Open err=%v", err)
	}
	plain := mustCreateKey(t, ks, keystore.CreateParams{Name: "记账", RateLimit: 10,
		ModelWhitelist: []string{"cn:glm-5.2"}}) // 白名单供「失败请求不计入」构造拒绝
	h := newKeyTestHandler(t, ks)
	// usage：prompt=100, completion=50, credit=2.0（上游真实口径）。
	h.cfg.Upstream = newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50,\"credit\":2.0}}\n\ndata: [DONE]\n\n", true
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatReqBody))
	req.Header.Set("Authorization", "Bearer "+plain)
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("success request must pass, code=%d body=%s", rec.Code, rec.Body)
	}
	k := mustKeySnapshot(t, ks, plain)
	if k.UsedTokens != 150 {
		t.Errorf("UsedTokens=%d want 150 (prompt+completion)", k.UsedTokens)
	}
	if k.UsedCredits != 2.0 {
		t.Errorf("UsedCredits=%v want 2.0", k.UsedCredits)
	}
	if k.LastUsedAt == 0 {
		t.Error("LastUsedAt must be set on success")
	}
	if len(ks.IPList(k)) != 1 {
		t.Errorf("IP memory must record the source ip, got %v", ks.IPList(k))
	}

	// 失败请求（段2 白名单拒绝）不计入：用量与窗口均无增量。
	before := mustKeySnapshot(t, ks, plain)
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"not-allowed-model","messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("Authorization", "Bearer "+plain)
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("rejected request must be 400, code=%d", rec2.Code)
	}
	after := mustKeySnapshot(t, ks, plain)
	if after.UsedTokens != before.UsedTokens || after.UsedCredits != before.UsedCredits {
		t.Errorf("rejected request must not record usage: before=%d/%v after=%d/%v",
			before.UsedTokens, before.UsedCredits, after.UsedTokens, after.UsedCredits)
	}
	if after.LastUsedAt != before.LastUsedAt {
		t.Error("rejected request must not bump LastUsedAt")
	}
}

// TestRecordUseSuccessPathNonStream 非流式成功出口同样记账（fillStatFromUsage 同口径）。
func TestRecordUseSuccessPathNonStream(t *testing.T) {
	ks, err := keystore.Open("")
	if err != nil {
		t.Fatalf("Open err=%v", err)
	}
	plain := mustCreateKey(t, ks, keystore.CreateParams{Name: "非流式"})
	h := newKeyTestHandler(t, ks)
	h.cfg.Upstream = newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true // Aggregate 输入是 SSE（upstream.Aggregate 消费流）
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatReqBody))
	req.Header.Set("Authorization", "Bearer "+plain)
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("non-stream success must pass, code=%d body=%s", rec.Code, rec.Body)
	}
	k := mustKeySnapshot(t, ks, plain)
	// sseOK usage: prompt=1 completion=1 → 合计 2；无 credit 字段 → 不计入（0）。
	if k.UsedTokens != 2 {
		t.Errorf("UsedTokens=%d want 2", k.UsedTokens)
	}
	if k.UsedCredits != 0 {
		t.Errorf("UsedCredits=%v want 0 (credit missing → not counted)", k.UsedCredits)
	}
}

// ---------------------------------------------------------------------------
// helpers：按明文取库内快照/id（经 Resolve 重锚，不依赖 List 顺序）
// ---------------------------------------------------------------------------

// mustKeySnapshot 按明文 Resolve 出当前快照（RecordUse 后重读权威状态的入口）。
func mustKeySnapshot(t *testing.T, ks *keystore.Store, plain string) *keystore.Key {
	t.Helper()
	k, err := ks.Resolve(plain)
	if err != nil {
		t.Fatalf("Resolve(%s...) err=%v", plain[:12], err)
	}
	return k
}

// mustKeyID 按明文取密钥 id（Update 用）。
func mustKeyID(t *testing.T, ks *keystore.Store, plain string) string {
	t.Helper()
	return mustKeySnapshot(t, ks, plain).ID
}
