// sticky_preferred_test.go 粘性首次分配的策略感知优先序（PreferredForModel）：
// credits_desc 下新会话必须绑余额最高号，而不是哈希打散（回归背景见 Config 注释）。
package session

import (
	"testing"
	"time"
)

// preferredRouter 构造带优先序回调的路由器：pref 固定返回 [hi mid lo]。
func preferredRouter(avail []string) *Router {
	r := routerWith(newCountingStore(), avail, time.Minute)
	r.cfg.PreferredForModel = func(model string) []string {
		return []string{"hi", "mid", "lo"}
	}
	return r
}

// TestPreferredFirstBindTakesHead 新会话首次分配取优先序首名（最高余额号），
// 且与哈希键无关——任意 key 都落在序首，而非 hashIndex 散开。
func TestPreferredFirstBindTakesHead(t *testing.T) {
	r := preferredRouter([]string{"lo", "mid", "hi"})
	for _, key := range []string{"c1", "c2", "other-键", "abc"} {
		got, ok := r.ResolveForModel(key, "glm-5.3")
		if !ok {
			t.Fatalf("key %q: 应能分配", key)
		}
		if got != "hi" {
			t.Errorf("ResolveForModel(%q)=%s want hi（优先序首名，与哈希键无关）", key, got)
		}
	}
}

// TestPreferredConcurrentSessionsShareHead 多个新会话（credits_desc 语义下）
// 都绑最高余额号：并发的权威闸门是账号在途上限（PickByUIDForModel/轮换），
// 不是粘性绑定本身——绑定共享无害，真撞上限时 handler 解绑重分配自愈。
func TestPreferredConcurrentSessionsShareHead(t *testing.T) {
	r := preferredRouter([]string{"lo", "mid", "hi"})
	for _, key := range []string{"s1", "s2", "s3"} {
		got, ok := r.ResolveForModel(key, "glm-5.3")
		if !ok || got != "hi" {
			t.Errorf("ResolveForModel(%q)=%s ok=%v want hi（并发会话共享最高余额号）", key, got, ok)
		}
	}
}

// TestPreferredExistingBindingUnchanged 优先序只决定"给新会话绑谁"：
// 既有绑定（快路径命中）不受影响，继续沿用原号。
func TestPreferredExistingBindingUnchanged(t *testing.T) {
	r := preferredRouter([]string{"lo", "mid", "hi"})
	r.Bind("c1", "lo")
	got, ok := r.ResolveForModel("c1", "glm-5.3")
	if !ok || got != "lo" {
		t.Errorf("ResolveForModel=%s ok=%v want lo（既有绑定不受优先序影响）", got, ok)
	}
}

// TestPreferredNilCallbackKeepsHash 未注入 PreferredForModel（weighted 生产语义）
// 时保持哈希打散零改动：多会话不会全部聚到同一号。
func TestPreferredNilCallbackKeepsHash(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"lo", "mid", "hi"}, time.Minute)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		got, ok := r.ResolveForModel("key-"+string(rune('a'+i%20))+"-"+string(rune('A'+i%26)), "glm-5.3")
		if !ok {
			t.Fatal("应能分配")
		}
		seen[got] = true
	}
	if len(seen) < 2 {
		t.Errorf("无优先序时 50 个会话只落到 %v，哈希打散疑似失效", seen)
	}
}

// TestPreferredFallsBackWhenPreferredEmpty 优先序回调返回空（如池瞬间无可用）
// 时，回落既有哈希打散兜底（不因优先序而分配失败）。
func TestPreferredFallsBackWhenPreferredEmpty(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"lo", "mid", "hi"}, time.Minute)
	r.cfg.PreferredForModel = func(model string) []string { return nil }
	got, ok := r.ResolveForModel("fresh", "glm-5.3")
	if !ok {
		t.Fatal("优先序为空时应回落哈希兜底分配，而不是失败")
	}
	if got != "hi" && got != "mid" && got != "lo" {
		t.Errorf("ResolveForModel=%s 应是可用账号之一", got)
	}
}

// TestPreferredRespectsAvailability 优先序里的候选若不在当前可用集
// （如序首刚被 6004 模型限额），跳过它取序内下一个可用号——
// 可用性以 AvailableForModel 为准，优先序只定次序不定可用。
func TestPreferredRespectsAvailability(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"lo", "mid", "hi"}, time.Minute)
	r.cfg.AvailableForModel = func(model string) []string {
		return []string{"lo", "mid"} // hi 刚变不可用
	}
	r.cfg.PreferredForModel = func(model string) []string {
		return []string{"hi", "mid", "lo"}
	}
	got, ok := r.ResolveForModel("fresh", "glm-5.3")
	if !ok || got != "mid" {
		t.Errorf("ResolveForModel=%s ok=%v want mid（序首 hi 不可用 → 取序内次可用）", got, ok)
	}
}
