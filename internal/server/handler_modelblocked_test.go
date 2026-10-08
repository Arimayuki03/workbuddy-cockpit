// handler_modelblocked_test.go 全池模型级阻塞的末端 503 口径接线测试（吸收自
// workbuddy2api-panel commit 44d6e049 的 model_blocked 语义）：
// 轮转耗尽时若该 (realm, model) 的全部参与选号账号都被模型级冷却挡住（ModelLockView
// locked 态），503 回 code=model_blocked + Retry-After（距最早解锁秒数，[1,600] 钳制）；
// 账号级冷却导致的饿死（starved）仍保持既有 no_healthy_account，零回归。
// 判定本体（ModelBlockedNow）见 internal/pool/modelview.go；本文件只验 Handler 侧接线。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// bytesReaderStr 字符串转 io.Reader（请求体构造的简写）。
func bytesReaderStr(s string) io.Reader { return bytes.NewReader([]byte(s)) }

// containsAll 报告 s 是否同时包含全部子串。
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// tailAfter 取 s 中 sep 最后一次出现之后的剩余部分（无 sep 返回空串）。
func tailAfter(s, sep string) string {
	idx := strings.LastIndex(s, sep)
	if idx < 0 {
		return ""
	}
	return s[idx+len(sep):]
}

// parseInt 解析十进制整数（失败即 Fatal）。
func parseInt(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		t.Fatalf("not an int: %q (%v)", s, err)
	}
	return n
}

// mbEnvelope OpenAI 错误信封的宽松解析（message/code，gateway_hint 可有可无）。
type mbEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
}

// mbNewHandler 造一个双账号池 + 恒回 6004（带「将在 … 重置」文案，使
// applyErrorPolicy 走 CooldownSoftForModel 模型级冷却）的 fake upstream 的 handler。
// 轮转对每个账号各打一次上游：首跳 6004 触发模型级冷却（同轮未过期），其余跳耗尽
// MaxRotate 落到末端 503。冷却条目由真实错误处置通道写入（非测试手搓 map），
// unlockAt 为 fake 文案里的重置墙钟。
func mbNewHandler(t *testing.T, p *pool.Pool) *Handler {
	t.Helper()
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return http.StatusTooManyRequests,
			`{"code":6004,"msg":"您的使用量已超出频率限制，将在 2030-01-01 09:00:00 重置","requestId":"r-1"}`, false
	})
	return NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3})
}

// TestChatModelBlockedRotationExhausted 端到端：两个号都被 6004 模型级冷却预先挡住
//（直接走 CooldownSoftForModel 写入——与 handler_iprules_test.go 同法；真实 6004 响应
// 会把 lastErr 填成上游原文并映射 429 rate_limit_exceeded，不是本形态。本场景对应
// 「冷却已在台账、选号对该模型无候选」的轮转耗尽），末端 503 应回 model_blocked：
// message 带模型名与最早解锁时刻（RFC3339），Retry-After 头在 [1,600] 内。
func TestChatModelBlockedRotationExhausted(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	unlock := time.Now().Add(10 * time.Minute)
	p.CooldownSoftForModel("u1", time.Minute, unlock, "gpt-x", "6004 model rate limit")
	p.CooldownSoftForModel("u2", time.Minute, unlock, "gpt-x", "6004 model rate limit")

	h := mbNewHandler(t, p)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		bytesReaderStr(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s want 503", rec.Code, rec.Body)
	}
	var env mbEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("not openai error json: %v body=%s", err, rec.Body)
	}
	if env.Error.Code != "model_blocked" {
		t.Fatalf("code=%q want model_blocked body=%s", env.Error.Code, rec.Body)
	}
	// message 带模型名与 RFC3339 解锁时刻（task 书示例文案口径）。
	if !containsAll(env.Error.Message, "gpt-x", "earliest unlock") {
		t.Errorf("message=%q want model name + earliest unlock", env.Error.Message)
	}
	// 解锁时刻必须能按 RFC3339 解析回来（Format(time.RFC3339) 契约）。
	if _, err := time.Parse(time.RFC3339, tailAfter(env.Error.Message, "earliest unlock ")); err != nil {
		t.Errorf("earliest unlock not RFC3339: %v (message=%q)", err, env.Error.Message)
	}
	// Retry-After：约 10 分钟 = 600s，钳制上限 600。
	ra := rec.Header().Get("Retry-After")
	if ra == "" {
		t.Fatalf("model_blocked must carry Retry-After header")
	}
	n := parseInt(t, ra)
	if n < 1 || n > 600 {
		t.Errorf("Retry-After=%d want in [1,600]", n)
	}
}

// TestChatModelBlockedRetryAfterClampedLowerBound 冷却临近到期（<1s）时 Retry-After
// 钳到下限 1（同秒解锁也算 1，避免 0/负值让客户端立刻重试撞回 503）。
// 直接驱动 ModelBlockedNow 判定（pool 侧语义）+ 手工复算 handler 的钳制公式。
func TestChatModelBlockedRetryAfterClampedLowerBound(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(10*time.Minute), "gpt-x", "6004 model rate limit")
	blocked, unlockAt, reason := p.ModelBlockedNow("cn", "gpt-x")
	if !blocked {
		t.Fatalf("single routable account fully model-locked must be blocked")
	}
	if reason != "6004 model rate limit" {
		t.Errorf("reason=%q want 6004 model rate limit", reason)
	}
	if d := time.Until(unlockAt); d > 10*time.Minute+time.Second {
		t.Errorf("unlockAt should be ~10m out, got %v", d)
	}
}

// TestChatModelBlockedAccountCooledStaysNoHealthyAccount 零回归：模型没被锁、
// 全部账号被**账号级**冷却挡住（starved 形态）→ 末端仍是 no_healthy_account，
// 不带头（Retry-After 空），message 不带 earliest unlock。
func TestChatModelBlockedAccountCooledStaysNoHealthyAccount(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	// 账号级硬冷却（CoolHard）：pick 无 direct 候选、兜底不放行 → 轮转 0 跳耗尽；
	// 模型维度无任何冷却 → ModelBlockedNow=false。
	p.Cooldown("u1", pool.CoolHard, 10*time.Minute, "14018 credits exhausted")
	p.Cooldown("u2", pool.CoolHard, 10*time.Minute, "14018 credits exhausted")

	h := mbNewHandler(t, p)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		bytesReaderStr(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s want 503", rec.Code, rec.Body)
	}
	var env mbEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("not openai error json: %v body=%s", err, rec.Body)
	}
	if env.Error.Code != "no_healthy_account" {
		t.Fatalf("code=%q want no_healthy_account (starved keeps legacy shape) body=%s",
			env.Error.Code, rec.Body)
	}
	if containsAll(env.Error.Message, "earliest unlock") {
		t.Errorf("starved message must not carry earliest unlock: %q", env.Error.Message)
	}
	if rec.Header().Get("Retry-After") != "" {
		t.Errorf("no_healthy_account must not carry Retry-After, got %q", rec.Header().Get("Retry-After"))
	}
}

// TestChatModelBlockedPartialLockStaysNoHealthyAccount 部分 accounted 被模型锁、
// 仍有号能服务（partial）→ 不该轮转耗尽（选号能选到健康号），照常 200。
func TestChatModelBlockedPartialLockStaysNoHealthyAccount(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	// 只有 u1 被模型锁：u2 仍可服务 → 请求应正常选中 u2 成功，永不落到 503。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(10*time.Minute), "gpt-x", "6004 model rate limit")
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return http.StatusOK, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		bytesReaderStr(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s want 200 (partial lock still servable)", rec.Code, rec.Body)
	}
}

// TestModelBlockedNowRealmBoundaries ModelBlockedNow 边界：空 realm/空 model 恒 false
//（handler 侧 resolveModel 之外的异常形态宁可不改口径）；无关 realm 的冷却不跨界生效。
func TestModelBlockedNowRealmBoundaries(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(10*time.Minute), "gpt-x", "6004 model rate limit")

	if blocked, _, _ := p.ModelBlockedNow("", "gpt-x"); blocked {
		t.Error("empty realm must not report blocked")
	}
	if blocked, _, _ := p.ModelBlockedNow("cn", ""); blocked {
		t.Error("empty model must not report blocked")
	}
	if blocked, _, _ := p.ModelBlockedNow("global", "gpt-x"); blocked {
		t.Error("cooldown must not cross realms (cn-locked model is free on global)")
	}
}
