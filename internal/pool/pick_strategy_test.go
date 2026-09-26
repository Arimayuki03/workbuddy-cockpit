package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// creditsDescPool 构造一个 3 账号池并启用 credits_desc 策略。
func creditsDescPool(t *testing.T) *Pool {
	t.Helper()
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Add(&auth.Auth{UID: "u3"})
	p.SetPickStrategy(StrategyCreditsDesc)
	return p
}

// TestCreditsDescAlwaysPicksHighest 单发请求恒命中最高余额号（Q1-A 严格确定型）：
// 关闭防撞号窗口后连续 pick 无随机性，u2 (50000) 每次都是冠军。
// （窗口内的沿序下探由 TestCreditsDescBurstWalksDownList 单独覆盖。）
func TestCreditsDescAlwaysPicksHighest(t *testing.T) {
	withNoPickGap(t)
	p := creditsDescPool(t)
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50000)
	p.SetCredits("u3", 300)
	for i := 0; i < 10; i++ {
		if got := p.Pick(""); got == nil || got.UID != "u2" {
			t.Fatalf("iter %d: pick=%+v want u2 (highest credits)", i, got)
		}
	}
}

// TestCreditsDescDeferOnCooldown 冠军冷却/熔断时顺延次高（Q1-A：不可用才顺延，
// 冷却/熔断/降权排除链路保留）。
func TestCreditsDescDeferOnCooldown(t *testing.T) {
	p := creditsDescPool(t)
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50000)
	p.SetCredits("u3", 300)
	p.Cooldown("u2", CoolHard, time.Hour, "余额不足")
	got := p.Pick("")
	if got == nil || got.UID != "u3" {
		t.Fatalf("pick=%+v want u3 (次高，u2 冷却中)", got)
	}
	// 冠军恢复后回到冠军。
	p.ReenableIfCredits("u2", 60000)
	if got := p.Pick(""); got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2 (解冻后回到冠军)", got)
	}
}

// TestCreditsDescTriedRotation 请求级轮换保留：tried 排除最高号后取次高。
func TestCreditsDescTriedRotation(t *testing.T) {
	p := creditsDescPool(t)
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50000)
	p.SetCredits("u3", 300)
	tried := map[string]bool{"u2": true}
	got := p.PickExcludingForRealm(tried, "", "")
	if got == nil || got.UID != "u3" {
		t.Fatalf("pick=%+v want u3 (u2 被 tried 排除后取次高)", got)
	}
}

// TestCreditsDescBurstWalksDownList 并发防撞号窗口保留（Q7-A）：同一批瞬时并发
// 沿余额序下探——第 1 个取 u2，窗口内第 2 个取次高 u3，第 3 个取 u1。
// 用串行模拟并发批次（pick 在写锁内串行执行，时序等价）。
func TestCreditsDescBurstWalksDownList(t *testing.T) {
	p := creditsDescPool(t)
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50000)
	p.SetCredits("u3", 300)
	want := []string{"u2", "u3", "u1"}
	for i, w := range want {
		if got := p.Pick(""); got == nil || got.UID != w {
			t.Fatalf("burst 第 %d 个: pick=%+v want %s (沿余额序下探)", i+1, got, w)
		}
	}
	// 窗口耗尽后回退严格序冠军 u2（非 LRU）。
	if got := p.Pick(""); got == nil || got.UID != "u2" {
		t.Fatalf("窗口耗尽后: pick=%+v want u2 (回退严格序冠军)", got)
	}
}

// TestCreditsDescTieBreak 平局按闲置时长降序再按 uid 升序（Q3）：
// u2/u3 同余额，u3 更久未用 → u3 在前；闲置也相同时 uid 小者在前。
func TestCreditsDescTieBreak(t *testing.T) {
	p := creditsDescPool(t)
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 500)
	p.SetCredits("u3", 500)
	// 手工设置 lastUsed：u2 刚用过，u3 久未用 → 平局时 u3 排前。
	now := time.Now()
	p.mu.Lock()
	p.byUID["u2"].lastUsed = now.Add(-1 * time.Second)
	p.byUID["u3"].lastUsed = now.Add(-48 * time.Hour)
	p.mu.Unlock()
	if got := p.Pick(""); got == nil || got.UID != "u3" {
		t.Fatalf("pick=%+v want u3 (同余额、闲置更久者排前)", got)
	}
	// 两者 lastUsed 相同（含从未使用）：uid 升序 → u2 在前。
	p2 := New("")
	p2.Add(&auth.Auth{UID: "b"})
	p2.Add(&auth.Auth{UID: "a"})
	p2.SetPickStrategy(StrategyCreditsDesc)
	p2.SetCredits("a", 500)
	p2.SetCredits("b", 500)
	if got := p2.Pick(""); got == nil || got.UID != "a" {
		t.Fatalf("pick=%+v want a (平局且闲置相同 → uid 升序)", got)
	}
}

// TestCreditsDescZeroCreditsNotExcluded 零余额排最后但不排除（Q3）：
// 其余全冷却时零余额号仍是合法候选（与 weighted 的 credits 不过滤口径一致）。
func TestCreditsDescZeroCreditsNotExcluded(t *testing.T) {
	p := creditsDescPool(t)
	p.SetCredits("u1", 0)
	p.SetCredits("u2", 50000)
	p.SetCredits("u3", 300)
	p.Cooldown("u2", CoolHard, time.Hour, "余额不足")
	p.Cooldown("u3", CoolHard, time.Hour, "余额不足")
	if got := p.Pick(""); got == nil || got.UID != "u1" {
		t.Fatalf("pick=%+v want u1 (仅剩零余额号也应被选中，不排除)", got)
	}
}

// TestCreditsDescIgnoresIdleAndExpiringWeights 严格模式绕过闲置补偿与快过期
// 补偿（Q1-A）：u1 闲置 48h + 快过期积分拉满权重，u2 余额更高仍应胜出——
// 排序只看 credits。
func TestCreditsDescIgnoresIdleAndExpiringWeights(t *testing.T) {
	withNoPickGap(t) // 连续验证纯余额序，排除窗口下探干扰
	p := creditsDescPool(t)
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 200)
	p.SetCredits("u3", 300)
	p.mu.Lock()
	p.byUID["u1"].lastUsed = time.Now().Add(-48 * time.Hour) // 闲置拉满
	p.byUID["u1"].creditsExpiring = 100                      // 快过期拉满（weighted 下 ×8）
	p.mu.Unlock()
	for i := 0; i < 5; i++ {
		if got := p.Pick(""); got == nil || got.UID != "u3" {
			t.Fatalf("iter %d: pick=%+v want u3 (纯余额序，闲置/快过期不参与)", i, got)
		}
	}
}

// TestCreditsDescFallsBackWhenAllCooling 全候选冷却时仍走既有全冷却兜底
// （选 until 最早到期者），策略只替换 normal 路径的排序。
func TestCreditsDescFallsBackWhenAllCooling(t *testing.T) {
	p := creditsDescPool(t)
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50000)
	p.SetCredits("u3", 300)
	p.Cooldown("u1", CoolSoft, 30*time.Minute, "429")
	p.Cooldown("u2", CoolSoft, 10*time.Minute, "429") // 最早到期
	p.Cooldown("u3", CoolSoft, time.Hour, "429")
	if got := p.Pick(""); got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2 (全冷却兜底选最早到期，而非余额最高)", got)
	}
}

// TestSetPickStrategyRoundTrip 策略热切换：weighted 下加权随机可选中闲置号，
// 切到 credits_desc 后恒取最高余额号，切回 weighted 恢复加权行为。
func TestSetPickStrategyRoundTrip(t *testing.T) {
	withNoPickGap(t) // 关窗口让 weighted 路径可以连选同一号
	p := New("")
	p.Add(&auth.Auth{UID: "lo"})
	p.Add(&auth.Auth{UID: "hi"})
	p.SetCredits("lo", 1)
	p.SetCredits("hi", 50000)
	// weighted 默认：lo 仍可能被抽中（闲置补偿权重）。
	seenLo := false
	for i := 0; i < 200; i++ {
		if p.Pick("").UID == "lo" {
			seenLo = true
			break
		}
	}
	if !seenLo {
		t.Fatalf("weighted 下低余额闲置号应有机会被抽中（本轮未观察到，疑似策略未生效？）")
	}
	// 热切到 credits_desc：恒取 hi。
	p.SetPickStrategy(StrategyCreditsDesc)
	for i := 0; i < 50; i++ {
		if got := p.Pick(""); got == nil || got.UID != "hi" {
			t.Fatalf("iter %d: pick=%+v want hi (热切 credits_desc 后恒取最高)", i, got)
		}
	}
	// 热切回 weighted：恢复加权随机（lo 再次可被抽中）。
	p.SetPickStrategy(StrategyWeighted)
	seenLo = false
	for i := 0; i < 200; i++ {
		if p.Pick("").UID == "lo" {
			seenLo = true
			break
		}
	}
	if !seenLo {
		t.Fatalf("weighted 下低余额闲置号应恢复可被抽中")
	}
}

// TestParsePickStrategy 字符串解析：合法值原样、空串/非法值回落 weighted。
func TestParsePickStrategy(t *testing.T) {
	cases := map[string]PickStrategy{
		"weighted":     StrategyWeighted,
		"credits_desc": StrategyCreditsDesc,
		"":             StrategyWeighted,
		"bogus":        StrategyWeighted,
	}
	for in, want := range cases {
		if got := ParsePickStrategy(in); got != want {
			t.Errorf("ParsePickStrategy(%q)=%q want %q", in, got, want)
		}
	}
}

// TestStickyPreferredCreditsDescOrder 粘性优先序：credits_desc 时返回余额降序的
// 可用列表（首名与 pick 同快照的冠军一致），冷却/熔断号排除。
func TestStickyPreferredCreditsDescOrder(t *testing.T) {
	p := creditsDescPool(t)
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50000)
	p.SetCredits("u3", 300)
	p.Cooldown("u3", CoolHard, time.Hour, "余额不足")
	got := p.StickyPreferredForModelRealm("", "")
	if len(got) != 2 || got[0] != "u2" || got[1] != "u1" {
		t.Fatalf("StickyPreferredForModelRealm=%v want [u2 u1]（余额降序，u3 冷却排除）", got)
	}
}

// TestStickyPreferredWeightedNil weighted（默认策略）返回 nil——粘性保持哈希
// 打散零改动（回调语义：nil = 不启用优先序）。
func TestStickyPreferredWeightedNil(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)
	if got := p.StickyPreferredForModelRealm("", ""); got != nil {
		t.Fatalf("weighted 下应返回 nil，got %v", got)
	}
}

// TestStickyPreferredRealmFilter 粘性优先序带 realm 过滤：global 前缀模型
// 只见 global 域账号（与 AvailableUIDsForModelRealm 同口径）。
func TestStickyPreferredRealmFilter(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.SetPickStrategy(StrategyCreditsDesc)
	p.SetCredits("cn1", 9000)
	p.SetCredits("g1", 100)
	got := p.StickyPreferredForModelRealm("m", "global")
	if len(got) != 1 || got[0] != "g1" {
		t.Fatalf("StickyPreferredForModelRealm(global)=%v want [g1]（跨 realm 不泄漏）", got)
	}
}

// TestStickyPreferredHotSwitch 策略热切换联动：面板保存 credits_desc/weighted
// 后，粘性优先序立即跟随（SetPickStrategy 热生效无需重启）。
func TestStickyPreferredHotSwitch(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)
	if got := p.StickyPreferredForModelRealm("", ""); got != nil {
		t.Fatalf("初始 weighted 应返回 nil，got %v", got)
	}
	p.SetPickStrategy(StrategyCreditsDesc)
	if got := p.StickyPreferredForModelRealm("", ""); len(got) != 1 || got[0] != "u1" {
		t.Fatalf("热切 credits_desc 后应返回 [u1]，got %v", got)
	}
	p.SetPickStrategy(StrategyWeighted)
	if got := p.StickyPreferredForModelRealm("", ""); got != nil {
		t.Fatalf("热切回 weighted 后应返回 nil，got %v", got)
	}
}
