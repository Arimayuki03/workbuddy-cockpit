// 模型级冷却的上游重置时刻保留（manager v2.11.14 / 0056）：
// 6004 的 Until 会被 soft_rate_max 截短，但上游真实 ResetAt 未到之前，
// 台账、选号与持久化都必须继续把它视为冷却。
package pool

import (
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestModelCooldownRetainedUntilUpstreamReset(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(10 * time.Millisecond)
	reset := time.Now().Add(2 * time.Hour)
	p.CooldownSoftForModel("u1", time.Millisecond, reset, "hy4free", "6004 model rate limit")

	time.Sleep(30 * time.Millisecond) // 等 Until（10ms 截断）过期
	now := time.Now()

	p.mu.Lock()
	e := p.byUID["u1"]
	e.pruneExpiredModelCooldowns(now)
	p.mu.Unlock()

	p.mu.RLock()
	mc, ok := e.modelCooldowns["hy4free"]
	cooled := e.modelCooled(now, "hy4free")
	p.mu.RUnlock()
	if !ok {
		t.Fatal("Until 过期但 ResetAt 在未来时 prune 不得删除模型冷却")
	}
	if !mc.ResetAt.Equal(reset) {
		t.Fatalf("ResetAt=%v want %v", mc.ResetAt, reset)
	}
	if !cooled {
		t.Fatal("Until 过期但 ResetAt 在未来时 modelCooled 仍应为 true")
	}

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("status missing")
	}
	if len(st.RateLimitedModels) != 1 {
		t.Fatalf("RateLimitedModels=%+v want 1 行（不得虚假恢复）", st.RateLimitedModels)
	}
	row := st.RateLimitedModels[0]
	if row.Model != "hy4free" || !row.ResetAt.Equal(reset) {
		t.Errorf("row=%+v want model=hy4free reset_at=%v", row, reset)
	}
	if !row.Until.After(now) {
		t.Errorf("row.Until=%v 应延伸到未来（有效截止），now=%v", row.Until, now)
	}

	locks := p.ModelLockView()
	if len(locks) != 1 {
		t.Fatalf("ModelLockView=%+v want 1 行", locks)
	}
	if locks[0].Model != "hy4free" || locks[0].Locked != 1 || locks[0].State != "locked" {
		t.Errorf("lock row=%+v want hy4free locked=1 state=locked", locks[0])
	}
	if !locks[0].UnlockAt.After(now) {
		t.Errorf("UnlockAt=%v 应为有效未来时刻", locks[0].UnlockAt)
	}
}

func TestModelCooldownPersistWhenUntilExpiredButResetAtFuture(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(10 * time.Millisecond)
	reset := time.Now().Add(3 * time.Hour)
	p.CooldownSoftForModel("u1", time.Millisecond, reset, "hy4free", "6004 model rate limit")

	time.Sleep(30 * time.Millisecond)
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.mu.RLock()
	mc, ok := p2.byUID["u1"].modelCooldowns["hy4free"]
	p2.mu.RUnlock()
	if !ok {
		t.Fatal("重启后 Until 已过期但 ResetAt 在未来的模型冷却应恢复")
	}
	if !mc.ResetAt.Equal(reset) {
		t.Fatalf("恢复后的 ResetAt=%v want %v", mc.ResetAt, reset)
	}
	if !mc.active(time.Now()) {
		t.Fatal("恢复后的条目应仍处于 active")
	}
}

// 回归（review high）：Until 仍未过期但早于 ResetAt 时，「何时能好」必须用上游
// ResetAt（active 实际拦到 ResetAt），不能用被 soft_rate_max 截短的 Until——
// 否则 ModelLockView 显示提前解锁、model_blocked 的 Retry-After 也会偏短。
// 注：/status 的 row.Until 仍按既有契约展示截断值（与 ResetAt 字段配合观察），
// 真实解锁时刻由 ModelLockView/ModelBlockedNow 表达。
func TestModelCooldownUnlockAtUsesLaterResetAt(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(10 * time.Minute)     // Until = now + 10m（截断）
	reset := time.Now().Add(3 * time.Hour) // 上游真实重置更晚
	p.CooldownSoftForModel("u1", time.Minute, reset, "glm-5.3", "6004 model rate limit")

	locks := p.ModelLockView()
	if len(locks) != 1 {
		t.Fatalf("ModelLockView=%+v want 1 行", locks)
	}
	if d := time.Until(locks[0].UnlockAt); d < 2*time.Hour {
		t.Fatalf("UnlockAt=%v 应为 ResetAt（约 3h 后），而不是被截短的 Until", locks[0].UnlockAt)
	}
	if d := time.Until(locks[0].FullyUnlockAt); d < 2*time.Hour {
		t.Fatalf("FullyUnlockAt=%v 应为 ResetAt（约 3h 后）", locks[0].FullyUnlockAt)
	}

	st, ok := p.Status("u1")
	if !ok || len(st.RateLimitedModels) != 1 {
		t.Fatalf("status=%+v want 1 条模型台账", st)
	}
	row := st.RateLimitedModels[0]
	if d := row.ResetAt.Sub(reset); d < -time.Second || d > time.Second {
		t.Fatalf("row.reset_at=%v want ~%v", row.ResetAt, reset)
	}
	if rem := time.Until(row.Until); rem > 10*time.Minute+time.Second {
		t.Fatalf("row.until=%v 应保持 soft_rate_max 截断（≤10m）", row.Until)
	}

	blocked, unlockAt, _ := p.ModelBlockedNow("cn", "glm-5.3")
	if !blocked {
		t.Fatal("唯一账号被该模型冷却时 ModelBlockedNow 应为 blocked")
	}
	if d := time.Until(unlockAt); d < 2*time.Hour {
		t.Fatalf("earliestUnlock=%v 应为 ResetAt（约 3h 后）", unlockAt)
	}
}

func TestAccountLevelCooldownKeepsActiveModelCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	reset := time.Now().Add(time.Hour)
	p.CooldownSoftForModel("u1", time.Minute, reset, "hy4free", "6004 model rate limit")

	p.Cooldown("u1", CoolSoft, 10*time.Minute, "429 rate limit")
	if !hasActiveModelCooldown(t, p, "u1", "hy4free") {
		t.Fatal("账号级 Cooldown 不得清掉活跃的模型级冷却")
	}
	// 账号级冷却期间，活跃的模型条目不得让探活把账号误判为「模型豁免可用」。
	if p.ServableNow() {
		t.Fatal("账号级冷却 + 模型冷却并存时 ServableNow 必须为 false")
	}

	p.CooldownSoftRate("u1", time.Minute, time.Time{}, "429 rate limit")
	if !hasActiveModelCooldown(t, p, "u1", "hy4free") {
		t.Fatal("账号级 CooldownSoftRate 不得清掉活跃的模型级冷却")
	}

	p.ReenableIfCredits("u1", 500)
	if !hasActiveModelCooldown(t, p, "u1", "hy4free") {
		t.Fatal("签到解冻不得清掉活跃的模型级冷却")
	}
}

func hasActiveModelCooldown(t *testing.T, p *Pool, uid, model string) bool {
	t.Helper()
	p.mu.RLock()
	defer p.mu.RUnlock()
	mc, ok := p.byUID[uid].modelCooldowns[model]
	return ok && mc.active(time.Now())
}

// 手动「强制清冷却」仍必须全量归零：账号级冷却保留模型台账的语义只适用于
// 自动解冻/账号级冷却，人工入口不能失效。
func TestForceClearCooldownClearsModelCooldowns(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "hy4free", "6004 model rate limit")
	found, cleared := p.ForceClearCooldown("u1")
	if !found || !cleared {
		t.Fatalf("ForceClearCooldown found=%v cleared=%v", found, cleared)
	}
	p.mu.RLock()
	n := len(p.byUID["u1"].modelCooldowns)
	p.mu.RUnlock()
	if n != 0 {
		t.Fatalf("强制清冷却后 modelCooldowns=%d want 0", n)
	}
}
