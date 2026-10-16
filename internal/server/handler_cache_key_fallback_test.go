package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// handler_cache_key_fallback_test.go 会话键回退的端到端锁定（任务书 prompt-cache-key §B）：
//
// 只发 prompt_cache_key 的客户端（dsh 等 pi-ai 系把会话 ID 放在这个 OpenAI 前缀缓存
// 字段里）没有 conversation 维度键 → parsed.ConversationID 恒空 → 出站 conversationId
// 此前也恒空。下游一切按「会话段」派生的东西因此失去会话维度：prepareBody 的
// prompt_cache_key 注入拿不到会话段（buildCacheKey 退化为 sha256(uid+"|") 常量键）、
// 上游会话头族的聚合也碎片化。回退 sessKey 后这类客户端恢复会话标识；而**完全无会话
// 标识**的客户端（既无 conversation 维度也无 prompt_cache_key）仍保持「不伪造、不发」
// 契约——本文件把两侧都钉住（只增语义，零回归）。

// postChatCaptureUpstream 发一次 chat 请求并返回上游实际收到的头与 body
// （roundTripFunc 捕获；响应用 sseOK 让非流式聚合成功）。
func postChatCaptureUpstream(t *testing.T, reqBody string) (http.Header, map[string]any) {
	t.Helper()
	var captured http.Header
	outBody := map[string]any{}
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			captured = r.Header.Clone()
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &outBody)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s want 200", rec.Code, rec.Body)
	}
	return captured, outBody
}

// TestChatPromptCacheKeyOnlyClientKeepsSessionDimension 只发 prompt_cache_key 的客户端：
// 会话标识必须进入出站 X-Conversation-ID（本修复点），且客户端自带的 prompt_cache_key
// 原值保留（优先级 1，网关绝不覆盖）。
func TestChatPromptCacheKeyOnlyClientKeepsSessionDimension(t *testing.T) {
	hdr, outBody := postChatCaptureUpstream(t,
		`{"model":"glm-5.2","stream":false,"prompt_cache_key":"pck-sess-1","messages":[{"role":"user","content":"hi"}]}`)

	if got := hdr.Get("X-Conversation-ID"); got != "pck-sess-1" {
		t.Errorf("X-Conversation-ID=%q want pck-sess-1（prompt_cache_key 客户端回退会话键）", got)
	}
	if got := outBody["prompt_cache_key"]; got != "pck-sess-1" {
		t.Errorf("上游 body prompt_cache_key=%v want pck-sess-1（客户端自带则原值保留）", got)
	}
	// 轮级聚合主键必须照常存在（会话维度增补不影响既有头族契约）。
	if !isValidB3Trace(hdr.Get("X-Conversation-Request-ID")) {
		t.Errorf("X-Conversation-Request-ID=%q want 32 hex 派生值", hdr.Get("X-Conversation-Request-ID"))
	}
}

// TestChatAnonymousClientStillSendsNoConversationID 无任何会话标识的请求
// （既无 conversation 维度也无 prompt_cache_key）保持既有契约：不发 X-Conversation-ID
// （不伪造）。prompt_cache_key 注入仍是账号段兜底，但**不得**因回退而凭空造出会话。
func TestChatAnonymousClientStillSendsNoConversationID(t *testing.T) {
	hdr, outBody := postChatCaptureUpstream(t,
		`{"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":"hi"}]}`)

	if got := hdr.Get("X-Conversation-ID"); got != "" {
		t.Errorf("X-Conversation-ID=%q want empty（无会话标识不伪造）", got)
	}
	// 注入键仍在（账号段隔离），只是会话段为空——存量行为零回归。
	if key, _ := outBody["prompt_cache_key"].(string); !strings.HasPrefix(key, "wb2a-u1-") {
		t.Errorf("prompt_cache_key=%v want wb2a-u1- 前缀的注入键", outBody["prompt_cache_key"])
	}
}

// TestChatConversationIDWinsOverPromptCacheKey conversation 维度键在场时优先
// （ExtractKey 契约：prompt_cache_key 只是兜底，绝不抢占）——回退不应改变这个优先级。
func TestChatConversationIDWinsOverPromptCacheKey(t *testing.T) {
	hdr, outBody := postChatCaptureUpstream(t,
		`{"model":"glm-5.2","stream":false,"conversation_id":"conv-priority","prompt_cache_key":"pck-loser","messages":[{"role":"user","content":"hi"}]}`)

	if got := hdr.Get("X-Conversation-ID"); got != "conv-priority" {
		t.Errorf("X-Conversation-ID=%q want conv-priority（conversation 维度优先）", got)
	}
	// 客户端自带 prompt_cache_key 时原值保留（注入优先级 1）——回退只影响「本来为空」的
	// 场景，不会覆盖客户端显式给的值。
	if got := outBody["prompt_cache_key"]; got != "pck-loser" {
		t.Errorf("prompt_cache_key=%v want pck-loser（客户端自带值原样保留）", outBody["prompt_cache_key"])
	}
}
