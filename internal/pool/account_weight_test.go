package pool

import (
	"math"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 手写权重（issue #187）的选号语义：只缩放加权抽签概率，不改变候选资格。
//
// 这一组用例与 patch 0008 的 pick_weight_test.go 同源，并补齐当前架构里的
// credits_desc / 成本分层 / 在途占满门槛与 Status 生效值透出。

// TestManualWeightScalesWeightOf 确定性断言：同等条件下 weight=9 的号在
// weightOf 里恰好是无权重号的 9 倍（不依赖统计运气）。
func TestManualWeightScalesWeightOf(t *testing.T) {
	now := time.Now()
	p := New("")
	p.SetPreferExpiring(false)
	p.Add(&auth.Auth{UID: "heavy", Weight: 9})
	p.Add(&auth.Auth{UID: "light"})
	p.SetCredits("heavy", 100)
	p.SetCredits("light", 100)

	wHeavy := entryWeightOf(t, p, "heavy", 100, now)
	wLight := entryWeightOf(t, p, "light", 100, now)
	if wLight <= 0 {
		t.Fatalf("基准权重必须为正: %v", wLight)
	}
	if got := wHeavy / wLight; got != 9 {
		t.Errorf("weightOf 倍率=%.9f want 9（heavy=%.9f light=%.9f）", got, wHeavy, wLight)
	}

	// 越界值在 weightOf 里被钳制（1e9 → 100，0.01 → 0.1），不会压成一号独大。
	p2 := New("")
	p2.SetPreferExpiring(false)
	p2.Add(&auth.Auth{UID: "huge", Weight: 1e9})
	p2.Add(&auth.Auth{UID: "tiny", Weight: 0.01})
	p2.Add(&auth.Auth{UID: "base"})
	p2.SetCredits("huge", 100)
	p2.SetCredits("tiny", 100)
	p2.SetCredits("base", 100)
	wHuge := entryWeightOf(t, p2, "huge", 100, now)
	wTiny := entryWeightOf(t, p2, "tiny", 100, now)
	wBase := entryWeightOf(t, p2, "base", 100, now)
	if got, want := wHuge/wBase, float64(auth.MaxWeight); math.Abs(got-want) > 1e-9 {
		t.Errorf("1e9 钳制倍率=%.9f want %.1f", got, want)
	}
	if got, want := wTiny/wBase, float64(auth.MinWeight); math.Abs(got-want) > 1e-9 {
		t.Errorf("0.01 钳制倍率=%.9f want %.1f", got, want)
	}
}

// TestPickHonoursManualWeight 统计验收：两个条件完全相同的号（同 credits、
// 同时从未使用），权重 9:1 的抽中份额应接近 90%。
func TestPickHonoursManualWeight(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetPreferExpiring(false)
	p.Add(&auth.Auth{UID: "heavy", Weight: 9})
	p.Add(&auth.Auth{UID: "light"})
	p.SetCredits("heavy", 100)
	p.SetCredits("light", 100)

	const rounds = 4000
	counts := map[string]int{}
	for i := 0; i < rounds; i++ {
		a := p.Pick("")
		if a == nil {
			t.Fatal("pick returned nil")
		}
		counts[a.UID]++
	}
	share := float64(counts["heavy"]) / rounds
	// 理论值 0.9；阈值留足随机波动，但必须明显高于 0.5 —— 只断言「比另一个多」
	// 的话权重乘数失效也可能因随机偏差通过。
	if share < 0.80 || share > 0.96 {
		t.Errorf("权重 9 的账号抽中 %.1f%%，期望接近 90%%（counts=%v）", share*100, counts)
	}
}

// 不设权重（0）与显式 1.0 必须等价：否则「默认值」会随文件里有没有这个键漂移。
func TestManualWeightOneEqualsUnset(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetPreferExpiring(false)
	p.Add(&auth.Auth{UID: "u1", Weight: 1})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 100)

	const rounds = 2000
	counts := map[string]int{}
	for i := 0; i < rounds; i++ {
		counts[p.Pick("").UID]++
	}
	share := float64(counts["u1"]) / rounds
	if share < 0.45 || share > 0.55 {
		t.Errorf("权重 1 与未设置应当等价，实际占比 %.1f%%（counts=%v）", share*100, counts)
	}
}

// TestManualWeightDoesNotBypassGates 硬门槛优先于权重：冷却 / 自动禁用 /
// 手动停用 / 在途占满的高权重号都不能被选中。
func TestManualWeightDoesNotBypassGates(t *testing.T) {
	cases := []struct {
		name string
		gate func(p *Pool)
	}{
		{"cooling", func(p *Pool) { p.Cooldown("heavy", CoolSoft, time.Hour, "test") }},
		{"disabled", func(p *Pool) { p.Disable("heavy", "test") }},
		{"manual_disabled", func(p *Pool) { p.SetManualDisabled("heavy", true, "test") }},
		{"in_flight_full", func(p *Pool) {
			p.SetMaxInFlight(1)
			if !p.Acquire("heavy") {
				t.Fatal("Acquire(heavy) failed")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withNoPickGap(t)
			p := New("")
			p.SetPreferExpiring(false)
			p.Add(&auth.Auth{UID: "heavy", Weight: 100})
			p.Add(&auth.Auth{UID: "ok"})
			p.SetCredits("heavy", 100)
			p.SetCredits("ok", 100)
			tc.gate(p)
			for i := 0; i < 50; i++ {
				got := p.Pick("")
				if got == nil || got.UID != "ok" {
					t.Fatalf("门槛 %s 被高权重绕过: pick=%+v", tc.name, got)
				}
			}
		})
	}
}

// TestManualWeightDoesNotBypassCostTier 成本分层是硬过滤：tier 0（已实测免费）
// 即使权重只有 0.1，也必须优先于 tier 2（已实测收费）的权重 100 号。
func TestManualWeightDoesNotBypassCostTier(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetPreferExpiring(false)
	p.SetCostExploreInterval(0)
	p.Add(&auth.Auth{UID: "free", Weight: auth.MinWeight})
	p.Add(&auth.Auth{UID: "paid", Weight: auth.MaxWeight})
	p.SetCredits("free", 1000)
	p.SetCredits("paid", 1000)
	p.NoteModelCost("free", "hy4-preview", 0, 1000)   // 实测免费 tier 0
	p.NoteModelCost("paid", "hy4-preview", 2.9, 1000) // 实测收费 tier 2
	for i := 0; i < 20; i++ {
		a := p.PickExcludingForRealm(nil, "hy4-preview", "")
		if a == nil || a.UID != "free" {
			t.Fatalf("第 %d 次选中 %v，want free（手写权重不得越过成本分层）", i, a)
		}
	}
}

// TestManualWeightDoesNotAffectCreditsDesc credits_desc 是严格余额序：
// 低余额高权重的号不能抢过高余额低权重号（该策略在权重计算前就返回）。
func TestManualWeightDoesNotAffectCreditsDesc(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "rich", Weight: auth.MinWeight})
	p.Add(&auth.Auth{UID: "poor", Weight: auth.MaxWeight})
	p.SetCredits("rich", 50000)
	p.SetCredits("poor", 100)
	p.SetPickStrategy(StrategyCreditsDesc)
	for i := 0; i < 20; i++ {
		got := p.Pick("")
		if got == nil || got.UID != "rich" {
			t.Fatalf("credits_desc 被权重污染: pick=%+v want rich", got)
		}
	}
}

// TestStatusExposesEffectiveWeight Status（/status 与 /api/overview 的数据源）
// 只透出生效乘数：未设置 = 1、越界钳制、非法值 = 1，且不含任何凭证字段。
func TestStatusExposesEffectiveWeight(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "unset"})
	p.Add(&auth.Auth{UID: "set", Weight: 2.5})
	p.Add(&auth.Auth{UID: "huge", Weight: 1e9})
	p.Add(&auth.Auth{UID: "negative", Weight: -2})

	cases := map[string]float64{
		"unset":    1,
		"set":      2.5,
		"huge":     auth.MaxWeight,
		"negative": 1,
	}
	for uid, want := range cases {
		st, ok := p.Status(uid)
		if !ok {
			t.Fatalf("Status(%s) missing", uid)
		}
		if st.Weight != want {
			t.Errorf("Status(%s).Weight=%v want %v", uid, st.Weight, want)
		}
	}
}
