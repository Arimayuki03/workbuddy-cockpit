package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// ClearModelBlocks 只清 11102 负缓存，不碰 6004 的独立冷却表。
func TestClearModelBlocks(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})

	now := time.Now()
	block := func() modelCooldown {
		return modelCooldown{Until: now.Add(6 * time.Hour), Reason: "11102 model not available"}
	}
	p.mu.Lock()
	p.byUID["u1"].modelCooldowns = map[string]modelCooldown{
		"glm-5.3":             block(),
		"deepseek-v4.1-flash": block(),
	}
	p.byUID["u2"].modelCooldowns = map[string]modelCooldown{
		"deepseek-v4.1-flash": block(),
		// 6004 是配额重置语义，不能被本方法顺带清掉
		"glm-5.3": {Until: now.Add(time.Hour), ResetAt: now.Add(time.Hour), Reason: "6004"},
	}
	p.mu.Unlock()

	if n := p.ClearModelBlocks("deepseek-v4.1-flash"); n != 2 {
		t.Fatalf("ClearModelBlocks = %d, want 2", n)
	}
	if n := p.ClearModelBlocks("deepseek-v4.1-flash"); n != 0 {
		t.Fatalf("重复清理 = %d, want 0", n)
	}
	if n := p.ClearModelBlocks("glm-5.3"); n != 1 {
		t.Fatalf("glm-5.3 清理 = %d, want 1（只清 u1 的 11102）", n)
	}
	p.mu.RLock()
	_, kept := p.byUID["u2"].modelCooldowns["glm-5.3"]
	p.mu.RUnlock()
	if !kept {
		t.Fatal("u2 的 6004 冷却不应被清掉")
	}
	if n := p.ClearModelBlocks(""); n != 0 {
		t.Fatalf("空模型名 = %d, want 0", n)
	}
}

// ClearModelBlocksForRealm 只清指定 realm 的 11102 负缓存：
// 跨 realm 条目与 6004 配额冷却保留；realm="" 退化为全池（ClearModelBlocks 的兼容形态）。
func TestClearModelBlocksForRealm(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "gl1", Domain: "www.workbuddy.ai"})

	now := time.Now()
	block := func() modelCooldown {
		return modelCooldown{Until: now.Add(time.Minute), Reason: "11102 model not available"}
	}
	p.mu.Lock()
	p.byUID["cn1"].modelCooldowns = map[string]modelCooldown{
		"deepseek-v4.1-flash": block(),
		// 6004 是配额重置语义，不能被本方法顺带清掉
		"glm-5.3": {Until: now.Add(time.Hour), ResetAt: now.Add(time.Hour), Reason: "6004 model rate limit"},
	}
	p.byUID["gl1"].modelCooldowns = map[string]modelCooldown{
		"deepseek-v4.1-flash": block(), // 跨 realm 同名模型：cn 清理不得越界
	}
	p.mu.Unlock()

	// 空 model：恒 0（与 realm 无关，不触碰任何条目）。
	if n := p.ClearModelBlocksForRealm("", "cn"); n != 0 {
		t.Fatalf("空模型名（cn）= %d, want 0", n)
	}
	if n := p.ClearModelBlocksForRealm("", ""); n != 0 {
		t.Fatalf("空模型名（全池）= %d, want 0", n)
	}

	// 清 cn 域：只清 cn1 的 11102，gl1 的跨 realm 条目保留。
	if n := p.ClearModelBlocksForRealm("deepseek-v4.1-flash", "cn"); n != 1 {
		t.Fatalf("cn 域清理 = %d, want 1", n)
	}
	p.mu.RLock()
	_, cnBlocked := p.byUID["cn1"].modelCooldowns["deepseek-v4.1-flash"]
	_, glKept := p.byUID["gl1"].modelCooldowns["deepseek-v4.1-flash"]
	_, sixKept := p.byUID["cn1"].modelCooldowns["glm-5.3"]
	p.mu.RUnlock()
	if cnBlocked {
		t.Error("cn1 的 11102 条目应被清掉")
	}
	if !glKept {
		t.Error("global 账号的跨 realm 11102 条目不得被 cn 域清理误删")
	}
	if !sixKept {
		t.Error("6004 条目不得被 ClearModelBlocksForRealm 清除")
	}

	// 空 realm 退化为全池：剩余的 global 条目也应被清掉。
	if n := p.ClearModelBlocksForRealm("deepseek-v4.1-flash", ""); n != 1 {
		t.Fatalf("全池清理 = %d, want 1（仅剩 gl1）", n)
	}
	p.mu.RLock()
	left := len(p.byUID["gl1"].modelCooldowns)
	p.mu.RUnlock()
	if left != 0 {
		t.Errorf("全池清理后 gl1 的 modelCooldowns 应为空, got %d 条", left)
	}

	// 已不存在的条目：再清恒 0。
	if n := p.ClearModelBlocks("deepseek-v4.1-flash"); n != 0 {
		t.Fatalf("重复清理 = %d, want 0", n)
	}
}

// 过期 11102 条目不算「仍有效」：不清、不计数。它是 Hits 指数退避的承载体
// （见 pruneExpiredModelCooldowns），删除会把下一次命中的退避重置回 hits=1。
func TestClearModelBlocksForRealmSkipsExpired(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	p.mu.Lock()
	p.byUID["u1"].modelCooldowns = map[string]modelCooldown{
		"m": {Until: time.Now().Add(-time.Minute), Reason: "11102 model not available", Hits: 3},
	}
	p.mu.Unlock()

	if n := p.ClearModelBlocksForRealm("m", "cn"); n != 0 {
		t.Fatalf("过期 11102 条目 = %d, want 0（不拦路由，不应计入清理）", n)
	}
	if n := p.ClearModelBlocks("m"); n != 0 {
		t.Fatalf("全池包装清理过期条目 = %d, want 0", n)
	}
	p.mu.RLock()
	mc, kept := p.byUID["u1"].modelCooldowns["m"]
	p.mu.RUnlock()
	if !kept {
		t.Fatal("过期 11102 条目应保留（Hits 承载体），不得被清理入口删除")
	}
	if mc.Hits != 3 {
		t.Errorf("Hits=%d want 3（清理不得重置指数退避）", mc.Hits)
	}
}
