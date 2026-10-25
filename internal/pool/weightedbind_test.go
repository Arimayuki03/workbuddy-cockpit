package pool

import (
	"math/rand/v2"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// newDeterministicSource 固定种子的确定性随机源：加权分布断言在 CI 上必须可复现，
// 否则偶发越界会让本组用例变成 flaky。
func newDeterministicSource() func(n int64) int64 {
	src := rand.New(rand.NewPCG(20251023, 265))
	return func(n int64) int64 { return src.Int64N(n) }
}

// TestWeightedPickAmongPrefersExpiring 核心语义（issue #265，manager bbbf40c1 吸收）：
// 候选集内快过期积分占比高的账号，被粘性首绑选中的概率必须显著更高——这正是
// Pick 的三因子权重里 expiringWeight 的意图，也是首绑加权要继承的语义。
func TestWeightedPickAmongPrefersExpiring(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "plain"})
	p.Add(&auth.Auth{UID: "expiring"})
	p.SetCreditsDetailed("plain", 5000, 0)
	p.SetCreditsDetailed("expiring", 5000, 5000)
	p.SetRandomSource(newDeterministicSource())

	counts := map[string]int{}
	const rounds = 1000
	for i := 0; i < rounds; i++ {
		uid := p.WeightedPickAmong([]string{"plain", "expiring"}, "")
		if uid == "" {
			t.Fatalf("第 %d 次选号返回空串（候选两个账号均可用）", i)
		}
		counts[uid]++
	}
	// 权重：plain = 1 + 5000/5000×10 + 闲置满分 5 = 16；
	//       expiring = 1 + 10 + 1.0×8 + 5 = 24 → 期望 60% / 40%。
	if counts["expiring"] <= counts["plain"] {
		t.Fatalf("快过期号被选中 %d 次，未超过无快过期号的 %d 次（权重未生效）",
			counts["expiring"], counts["plain"])
	}
	if ratio := float64(counts["expiring"]) / rounds; ratio < 0.52 || ratio > 0.68 {
		t.Errorf("快过期号占比 %.3f 偏离期望 0.60（±0.08）", ratio)
	}
}

// TestWeightedPickAmongEmptyCandidates 空候选返回空串，不 panic、不随便选一个。
func TestWeightedPickAmongEmptyCandidates(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if got := p.WeightedPickAmong(nil, ""); got != "" {
		t.Errorf("空候选应返回空串，got %q", got)
	}
	if got := p.WeightedPickAmong([]string{}, "m"); got != "" {
		t.Errorf("空候选应返回空串，got %q", got)
	}
}

// TestWeightedPickAmongSkipsUnavailable 候选里刚进冷却/不健康的号不得被绑定
//（筛完到选号之间的时间窗），全不可用返回空串由调用方兜底。
func TestWeightedPickAmongSkipsUnavailable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "ok"})
	p.Add(&auth.Auth{UID: "cooling"})
	p.SetCreditsDetailed("ok", 100, 0)
	p.SetCreditsDetailed("cooling", 9000, 0)
	// cooling 进软冷却（权重再高也不可选）。
	p.Cooldown("cooling", CoolSoft, time.Hour, "test")

	for i := 0; i < 50; i++ {
		if got := p.WeightedPickAmong([]string{"ok", "cooling"}, ""); got != "ok" {
			t.Fatalf("第 %d 次选中 %q，冷却号必须被跳过", i, got)
		}
	}
	if got := p.WeightedPickAmong([]string{"cooling"}, ""); got != "" {
		t.Fatalf("全不可用应返回空串，got %q", got)
	}
}
