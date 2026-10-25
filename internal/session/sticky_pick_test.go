// sticky_pick_test.go 粘性首次分配的加权选号钩子（PickForModel，manager
// bbbf40c1 吸收）：weighted 策略下新会话在空闲候选段内按三因子抽签。
package session

import (
	"testing"
	"time"
)

// pickRouter 构造带加权选号回调的路由器：pick 固定返回候选里的 "exp" 号。
func pickRouter(avail []string) *Router {
	r := routerWith(newCountingStore(), avail, time.Minute)
	r.cfg.PickForModel = func(model string, candidates []string) string {
		for _, u := range candidates {
			if u == "exp" {
				return "exp"
			}
		}
		return ""
	}
	return r
}

// TestPickForModelFirstBindWeighted 新会话首绑取加权闭包选中的号（与哈希键无关）。
// 注意空闲优先语义：exp 被首个会话绑定后，后续新会话的候选段不再包含 exp
//（见 TestPickForModelCandidateIsIdlePreferred），所以这里只断言首个会话。
func TestPickForModelFirstBindWeighted(t *testing.T) {
	r := pickRouter([]string{"plain", "exp", "other"})
	got, ok := r.ResolveForModel("whatever-key", "glm-5.3")
	if !ok || got != "exp" {
		t.Errorf("ResolveForModel=%s ok=%v want exp（加权首绑，与哈希键无关）", got, ok)
	}
}

// TestPickForModelCandidateIsIdlePreferred 回调候选必须是空闲号优先段：
// exp 已被既有会话绑定时，候选里只剩未被绑定的号（exp 不在候选 → 回调返回空 →
// 回落哈希，但绝不能把已绑定的 exp 再绑给新会话之外产生混淆）。
func TestPickForModelCandidateIsIdlePreferred(t *testing.T) {
	r := pickRouter([]string{"exp", "plain"})
	r.Bind("old-session", "exp") // exp 已被占
	var seenCandidates []string
	r.cfg.PickForModel = func(model string, candidates []string) string {
		seenCandidates = append(seenCandidates, candidates...)
		return ""
	}
	if _, ok := r.ResolveForModel("fresh", "glm-5.3"); !ok {
		t.Fatal("应能分配")
	}
	for _, c := range seenCandidates {
		if c == "exp" {
			t.Errorf("候选段不应包含已被绑定的 exp: %v", seenCandidates)
		}
	}
}

// TestPickForModelOutOfRangeFallsBackHash 回调返回候选外 uid（实现 bug 防御）：
// 不得绑定候选外的号，回落哈希打散。
func TestPickForModelOutOfRangeFallsBackHash(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a", "b"}, time.Minute)
	r.cfg.PickForModel = func(model string, candidates []string) string {
		return "ghost" // 不在候选内
	}
	got, ok := r.ResolveForModel("fresh", "glm-5.3")
	if !ok {
		t.Fatal("应能分配")
	}
	if got != "a" && got != "b" {
		t.Errorf("ResolveForModel=%s 候选外 uid 不得被绑定", got)
	}
}

// TestPickForModelNilKeepsHash 未注入回调（最小装配/测试形态）时哈希打散零改动。
func TestPickForModelNilKeepsHash(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a", "b", "c"}, time.Minute)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		got, ok := r.ResolveForModel("k-"+string(rune('a'+i%26)), "glm-5.3")
		if !ok {
			t.Fatal("应能分配")
		}
		seen[got] = true
	}
	if len(seen) < 2 {
		t.Errorf("50 个会话只落到 %v，哈希打散疑似失效", seen)
	}
}
