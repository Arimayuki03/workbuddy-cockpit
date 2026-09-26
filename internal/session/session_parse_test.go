// session_parse_test.go ParseRequest（热路径单次解析）与 Router.touch CAS 的回归：
// ParseRequest 是 handler「解析一次、多处复用」的入口，字段值必须与旧独立函数
// （ExtractKey / StickyFallbackKey / TurnKey / ResolveConversationID / hasImagePart /
// peek）逐字段等价；touch CAS 防「复活刚被 Unbind/改绑的会话」。
package session

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// parseFields 独立函数路径的参照值（旧实现逐个喂同一 body）。
func parseFields(body []byte) Parsed {
	return Parsed{
		Stream:         peekStream(body),
		Model:          peekModel(body),
		SessKey:        ExtractKey(body),
		StickyKey:      stickyKeyLegacy(body),
		TurnKey:        TurnKey(body),
		ConversationID: ResolveConversationID(body),
		HasImage:       hasImageLegacy(body),
	}
}

// peekStream / peekModel 复刻 handler 旧 peek 结构行为。
func peekStream(body []byte) bool {
	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &peek)
	return peek.Stream
}

func peekModel(body []byte) string {
	var peek struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)
	return peek.Model
}

// stickyKeyLegacy 复刻 handler 旧 stickyKey 派生：sessKey 优先，空则 StickyFallbackKey。
func stickyKeyLegacy(body []byte) string {
	k := ExtractKey(body)
	if k == "" {
		k = StickyFallbackKey(body)
	}
	return k
}

// hasImageLegacy handler.hasImagePart 的同包复刻（口径一致：messages[].content[].type=="image_url"）。
func hasImageLegacy(body []byte) bool {
	var peek struct {
		Messages []struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &peek) != nil {
		return false
	}
	for _, m := range peek.Messages {
		for _, p := range m.Content {
			if p.Type == "image_url" {
				return true
			}
		}
	}
	return false
}

// TestParseRequestEquivalence 单次解析与旧独立函数路径逐字段等价（行为不变契约）。
func TestParseRequestEquivalence(t *testing.T) {
	bodies := []string{
		// 官方客户端形态：metadata + camelCase + stream + 图片轮
		`{"model":"cn:glm-5.2","stream":true,"metadata":{"conversationId":"conv-abc"},"messages":[{"role":"system","content":"sys"},{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}},{"type":"text","text":"看图"}]}]}`,
		// snake_case 会话键 + 纯文本轮
		`{"model":"glm-5.3","stream":false,"conversation_id":"conv-snake","messages":[{"role":"user","content":"你好"}]}`,
		// prompt_cache_key 兜底（dsh 等客户端）
		`{"model":"deepseek-v4","prompt_cache_key":"pck-1","messages":[{"role":"user","content":"hi"}]}`,
		// user_id 在场 → fallback 恒空
		`{"model":"m","user_id":"u-1","messages":[{"role":"user","content":"first"}]}`,
		`{"model":"m","metadata":{"user_id":"u-1"},"messages":[{"role":"user","content":"first"}]}`,
		// 无会话键无 user：走首条 user 消息 fallback + 末条轮级键
		`{"messages":[{"role":"user","content":"multi-1"},{"role":"assistant","content":"ok"},{"role":"user","content":"multi-2"}]}`,
		// 空 body / 畸形 JSON / 空对象
		``,
		`not json`,
		`{}`,
		`[]`,
		// model/stream 类型不符（零值）
		`{"model":123,"stream":"yes"}`,
		// 末条 user 纯图：TurnKey 走 contentSignature，HasImage=true
		`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{}}]}]}`,
		// 首条 user 空内容：fallback 不往后找
		`{"messages":[{"role":"user","content":""},{"role":"user","content":"second"}]}`,
		// M3 等价性破口形态：图片消息在前、字符串 content 消息在后的混合 body。
		// 旧 hasImagePart 因字符串 content 整体 unmarshal 失败恒 false；修复前
		// ParseRequest 遇到图片即 break 返回 true——两者不等价。
		`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"u"}}]},{"role":"user","content":"text after"}]}`,
		// 反向：字符串在前、图片在后——旧实现同样整体失败恒 false。
		`{"messages":[{"role":"user","content":"text before"},{"role":"user","content":[{"type":"image_url","image_url":{"url":"u"}}]}]}`,
	}
	for i, body := range bodies {
		want := parseFields([]byte(body))
		got := ParseRequest([]byte(body))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("case %d body=%.60q\n got=%+v\nwant=%+v", i, body, got, want)
		}
	}
}

// TestParseRequestPromptCacheKeyNotConversationID prompt_cache_key 进会话键
// 但不进会话头族 conversationId（两契约的边界与旧函数一致）。
func TestParseRequestPromptCacheKeyNotConversationID(t *testing.T) {
	p := ParseRequest([]byte(`{"prompt_cache_key":"pck-9","messages":[]}`))
	if p.SessKey != "pck-9" {
		t.Errorf("SessKey=%q want pck-9", p.SessKey)
	}
	if p.ConversationID != "" {
		t.Errorf("ConversationID=%q want empty (prompt_cache_key 不入会话头族)", p.ConversationID)
	}
}

// TestTouchDoesNotResurrectUnbound touch 的 CAS：写覆盖前比对 uid，防止复活刚被
// Unbind 的绑定（TOCTOU——快路径持旧 uid 慢一步，Unbind 已删键，旧实现无条件
// 覆盖会让失败号立刻回到粘性位）。
func TestTouchDoesNotResurrectUnbound(t *testing.T) {
	r := New(Config{
		TTL:        time.Hour,
		GCInterval: time.Hour,
		// 两号都可用：ResolveForModel 快路径命中 + available 校验必须放行。
		Available: func() []string { return []string{"u1", "u2"} },
	})
	r.Bind("k", "u1")
	if uid, ok := r.ResolveForModel("k", ""); !ok || uid != "u1" {
		t.Fatalf("precondition: resolve=%q ok=%v", uid, ok)
	}
	// 模拟竞态：快路径读到 u1 之后、touch 之前，Unbind 删掉了绑定。
	// touch 自身不再调用 ResolveForModel（那是被测对象），改用 entries 直查断言。
	r.Unbind("k")
	r.touch("k", "u1", time.Now())
	r.mu.RLock()
	_, exists := r.entries["k"]
	r.mu.RUnlock()
	if exists {
		t.Error("unbound key resurrected by touch")
	}
	// 键已被改绑到别的 uid：touch 旧 uid 不得覆盖。
	r.Bind("k", "u2")
	r.touch("k", "u1", time.Now())
	r.mu.RLock()
	cur := r.entries["k"]
	r.mu.RUnlock()
	if cur.uid != "u2" {
		t.Errorf("touch overwrote rebind: uid=%q want u2", cur.uid)
	}
	// 正常路径：同 uid touch 照常续期（lastActive 被刷新）。
	now := time.Now()
	r.touch("k", "u2", now.Add(-time.Minute)) // 先写旧时间戳
	r.touch("k", "u2", now)                   // 同 uid touch 覆盖
	r.mu.RLock()
	cur = r.entries["k"]
	r.mu.RUnlock()
	if cur.uid != "u2" || !cur.lastActive.Equal(now) {
		t.Errorf("same-uid touch broken: %+v", cur)
	}
}
