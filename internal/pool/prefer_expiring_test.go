// v1.20.0「快过期积分优先消耗」（pool.prefer_expiring）的确定性测试：
// 虚拟实例倍率（weighted 权重 ×3）、realm 可用 UID 列表的虚拟槽位、最早到期快照的
// 持久化往返与缺键兼容、开关热改、以及"credits_desc 逐字不受影响"的回归锚。
//
// 全部断言都不依赖真实时间流逝与统计运气：要么是位级相等的权重比，要么是把注入随机源
// 定在权重空间某个百分比上的确定性边界探测（r 由 pickWeighted 传入的 total 现算）。
package pool

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// preferExpiringPool 构造两号等余额的池（u1 有快过期快照，u2 没有），供权重/抽签断言用。
// 两号 credits 相同 → credits 占比项相同；从未使用 → 闲置补偿同为满分（idleWeightMax）。
// 于是"开着时 u1 权重恰好是 u2 的 expiringVirtualSlots 倍"成为可位级断言的事实。
func preferExpiringPool(t *testing.T, prefer bool) *Pool {
	t.Helper()
	p := New("")
	p.SetPreferExpiring(prefer)
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 100)
	// u1：最早到期包 2 小时后失效、余量 60（< credits，不被钳）。
	// u2：不给快过期快照 → 永不满足 expiringNow（对照组）。
	p.SetCreditEarliest("u1", time.Now().Add(2*time.Hour), 60)
	return p
}

// entryWeightOf 取单个账号在 maxCredits 口径下的权重（包内私有 helper，同 pool_test.go
// 的 entryWeight 风格，但允许指定 maxCredits 以获得可复现的整数权重）。
func entryWeightOf(t *testing.T, p *Pool, uid string, maxCredits int64, now time.Time) float64 {
	t.Helper()
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		t.Fatalf("账号 %s 不存在", uid)
	}
	return p.weightOf(e, maxCredits, now)
}

// TestPreferExpiringWeightMultiplier 核心确定性事实：开启时快过期号权重恰为
// expiringVirtualSlots 倍（虚拟实例 = 等价放三份）；关闭时与"从无快过期快照"的号逐字同权。
func TestPreferExpiringWeightMultiplier(t *testing.T) {
	now := time.Now()
	on := preferExpiringPool(t, true)
	off := preferExpiringPool(t, false)

	wOn := entryWeightOf(t, on, "u1", 100, now)
	wOff := entryWeightOf(t, off, "u1", 100, now)
	wRef := entryWeightOf(t, off, "u2", 100, now) // 无快过期快照的参照号

	if wOff != wRef {
		t.Fatalf("关闭时快过期号权重 %.9f != 无快照号 %.9f（旧行为应逐字相同）", wOff, wRef)
	}
	if want := wOff * expiringVirtualSlots; wOn != want {
		t.Errorf("开启时权重 %.9f want %.9f（=关闭时 ×%.0f 虚拟实例倍率）", wOn, want, expiringVirtualSlots)
	}
	// 对照组本身不受开关影响（没有快过期快照 = 无加成）。
	if wOn2 := entryWeightOf(t, on, "u2", 100, now); wOn2 != wRef {
		t.Errorf("无快过期快照的号权重 %.9f want %.9f（不应被开关影响）", wOn2, wRef)
	}
}

// TestPreferExpiringConditionEdgeCases 判据边界：余量为 0 / 失效时刻已过 / 余额为 0
// 都不加成；余量超总余额被钳到 credits。
func TestPreferExpiringConditionEdgeCases(t *testing.T) {
	now := time.Now()
	p := preferExpiringPool(t, false)
	wBase := entryWeightOf(t, p, "u1", 100, now) // 关闭时的基数（=旧行为）
	p.SetPreferExpiring(true)
	if got := entryWeightOf(t, p, "u1", 100, now); got != wBase*expiringVirtualSlots {
		t.Fatalf("开启时权重 %.9f want %.9f", got, wBase*expiringVirtualSlots)
	}

	// 余量 0：无信息（SetCreditEarliest 显式清空）→ 不加成。
	p.SetCreditEarliest("u1", time.Now().Add(2*time.Hour), 0)
	if got := entryWeightOf(t, p, "u1", 100, now); got != wBase {
		t.Errorf("余量 0 时权重 %.9f want %.9f（不加成）", got, wBase)
	}

	// 失效时刻已过：快照留着（非零）但判据要求"在未来" → 不加成。
	p.SetCreditEarliest("u1", now.Add(-time.Minute), 60)
	if got := entryWeightOf(t, p, "u1", 100, now); got != wBase {
		t.Errorf("已失效时权重 %.9f want %.9f（不加成）", got, wBase)
	}

	// 余量超总余额：钳到 credits（与 SetCreditsDetailed 对 expiring 的钳制同口径）。
	p.SetCreditEarliest("u1", time.Now().Add(2*time.Hour), 9999)
	p.mu.RLock()
	got := p.byUID["u1"].earliestRemaining
	p.mu.RUnlock()
	if got != 100 {
		t.Errorf("earliestRemaining=%d want 100（钳到 credits）", got)
	}

	// 余额归零（如签到回报 0）：即便快照仍在（非零、未失效）也不加成——余额空了
	// 无从"优先消耗"；此时权重应与"显式清空快照"逐字相同（读侧防御性判据）。
	p.SetCredits("u1", 0)
	wZero := entryWeightOf(t, p, "u1", 100, now)
	p.SetCreditEarliest("u1", time.Time{}, 0)
	if wCleared := entryWeightOf(t, p, "u1", 100, now); wZero != wCleared {
		t.Errorf("余额 0 有快照 %.9f != 清空快照 %.9f（都不得加成）", wZero, wCleared)
	}
	if wZero == wBase*expiringVirtualSlots {
		t.Error("余额 0 时不应保留虚拟实例加成")
	}
}

// TestPickPreferExpiringVirtualSlotShare 抽签分布对比（确定性，无统计运气）：
// 注入随机源把 r 定在权重空间的 k%（k=0..99）——u1 命中的 k 个数就是它占的槽位比例。
//
//	关闭：两号等权（1:1）→ u1 占 50/100
//	开启：快过期号虚拟 3 份（3:1）→ u1 占 75/100
//
// 每个探测点用新建的池（同一池内连续 Pick 会推进 lastUsed、改变闲置补偿项，
// 不是同一个候选集快照；新建池保证每次探测都是"同候选集、同权重"）。
func TestPickPreferExpiringVirtualSlotShare(t *testing.T) {
	withNoPickGap(t)
	pickAt := func(prefer bool, k int64) string {
		p := preferExpiringPool(t, prefer)
		p.SetRandomSource(func(total int64) int64 { return total * k / 100 })
		a := p.Pick("")
		if a == nil {
			t.Fatalf("Pick 返回 nil（prefer=%v k=%d）", prefer, k)
		}
		return a.UID
	}
	countU1 := func(prefer bool) int {
		n := 0
		for k := int64(0); k < 100; k++ {
			if pickAt(prefer, k) == "u1" {
				n++
			}
		}
		return n
	}
	if got := countU1(false); got != 50 {
		t.Errorf("关闭时 u1 槽位占比 %d/100 want 50/100（等权 1:1）", got)
	}
	if got := countU1(true); got != 75 {
		t.Errorf("开启时 u1 槽位占比 %d/100 want 75/100（虚拟 %d 份，3:1）", got, int(expiringVirtualSlots))
	}
	// 边界探测（逐字锚定权重比）：开启时 3/4 处是分界——r=75% 归 u2、r=75%-1 归 u1。
	if got := pickAt(true, 75); got != "u2" {
		t.Errorf("开启时 r=75%% 命中 %s want u2（分界在 3/4）", got)
	}
	p := preferExpiringPool(t, true)
	p.SetRandomSource(func(total int64) int64 { return total*3/4 - 1 })
	if a := p.Pick(""); a == nil || a.UID != "u1" {
		t.Errorf("开启时 r=75%%-1 命中 %v want u1（u1 占前三份）", a)
	}
}

// TestSetPreferExpiringHotToggle 开关热改：下一次选号立即按新值；快照不清、
// 不需要等刷新周期（关→开仍用同一份快照，权重仍是 ×3）。
func TestSetPreferExpiringHotToggle(t *testing.T) {
	now := time.Now()
	p := preferExpiringPool(t, true)
	if !p.PreferExpiring() {
		t.Fatal("New 的默认应为开启（与 config pool.prefer_expiring 默认一致）")
	}
	wOn := entryWeightOf(t, p, "u1", 100, now)

	p.SetPreferExpiring(false)
	if p.PreferExpiring() {
		t.Fatal("SetPreferExpiring(false) 未生效")
	}
	wOff := entryWeightOf(t, p, "u1", 100, now)
	if wOn != wOff*expiringVirtualSlots {
		t.Fatalf("热改后权重 %.9f want %.9f（=旧值 / %.0f）", wOff, wOn/expiringVirtualSlots, expiringVirtualSlots)
	}
	// 关掉时不清理历史快照（数据留着不参与决策，重新打开即可复用）。
	p.mu.RLock()
	er, ee := p.byUID["u1"].earliestRemaining, p.byUID["u1"].earliestExpiry
	p.mu.RUnlock()
	if er != 60 || ee.IsZero() {
		t.Fatalf("关闭开关不应清快照: remaining=%d expiry=%v", er, ee)
	}

	p.SetPreferExpiring(true)
	if got := entryWeightOf(t, p, "u1", 100, now); got != wOn {
		t.Errorf("重新打开后权重 %.9f want %.9f（同一快照复用）", got, wOn)
	}
}

// TestCreditsDescIgnoresPreferExpiring credits_desc 回归锚：余额严格降序路径
// 逐字不消费 prefer_expiring——低余额号有快过期快照，仍选最高余额号。
func TestCreditsDescIgnoresPreferExpiring(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetPreferExpiring(true)
	p.SetPickStrategy(StrategyCreditsDesc)
	p.Add(&auth.Auth{UID: "hi"})
	p.Add(&auth.Auth{UID: "lo"})
	p.SetCredits("hi", 500)
	p.SetCredits("lo", 100)
	p.SetCreditEarliest("lo", time.Now().Add(time.Hour), 50) // 快过期的是低余额号
	// 注入最末一个 r：credits_desc 根本不看随机源，无论怎么注入都必须选序首（hi）。
	p.SetRandomSource(func(n int64) int64 { return n - 1 })
	a := p.Pick("")
	if a == nil || a.UID != "hi" {
		t.Fatalf("credits_desc 应选最高余额号 hi，实际 %v（prefer_expiring 不得影响该策略）", a)
	}
}

// TestAvailableUIDsPreferExpiringVirtualSlots realm 可用 UID 列表的虚拟槽位：
// 开启时合格号从原位移出、垫尾 expiringVirtualSlots 份（确定性顺序）；
// 关闭时逐字返回旧列表；无域变体（AvailableUIDs）不带虚拟槽位。
func TestAvailableUIDsPreferExpiringVirtualSlots(t *testing.T) {
	p := realmPool(t) // cn1/cn2/g1/g2（realm_test.go 的 helper）
	p.SetPreferExpiring(false)
	off := p.AvailableUIDsForRealm("cn")
	if want := []string{"cn1", "cn2"}; !reflect.DeepEqual(off, want) {
		t.Fatalf("关闭时 AvailableUIDsForRealm(cn)=%v want %v", off, want)
	}

	// cn1 拿到快过期快照（先给余额：SetCreditEarliest 会钳到 credits）。
	p.SetCredits("cn1", 100)
	p.SetCreditEarliest("cn1", time.Now().Add(2*time.Hour), 40)
	p.SetPreferExpiring(true)

	got := p.AvailableUIDsForRealm("cn")
	want := []string{"cn2", "cn1", "cn1", "cn1"} // 非合格号保持升序，合格号恰好 3 份虚拟槽位
	if !reflect.DeepEqual(got, want) {
		t.Errorf("开启时 AvailableUIDsForRealm(cn)=%v want %v", got, want)
	}
	if n := strings.Count(strings.Join(got, ",")+",", "cn1,"); n != int(expiringVirtualSlots) {
		t.Errorf("合格号出现 %d 次 want %d 次（虚拟实例份数）", n, int(expiringVirtualSlots))
	}
	// 模型维度同口径（会话粘性生产路径）。
	if gotM := p.AvailableUIDsForModelRealm("glm-5.2", "cn"); !reflect.DeepEqual(gotM, want) {
		t.Errorf("AvailableUIDsForModelRealm=%v want %v", gotM, want)
	}
	// 只影响合格号所在域：global 域列表逐字不变。
	if gotG := p.AvailableUIDsForRealm("global"); !reflect.DeepEqual(gotG, []string{"g1", "g2"}) {
		t.Errorf("global 域=%v want [g1 g2]（虚拟槽位只落合格号）", gotG)
	}
	// 无域/无模型变体不叠虚拟槽位（长度语义被 watch.go 的可用数日志消费，不能有重复项）。
	if gotAll := p.AvailableUIDs(); !reflect.DeepEqual(gotAll, []string{"cn1", "cn2", "g1", "g2"}) {
		t.Errorf("AvailableUIDs=%v want [cn1 cn2 g1 g2]（无域变体不叠虚拟槽位）", gotAll)
	}

	// 关掉开关：逐字回到旧列表。
	p.SetPreferExpiring(false)
	if back := p.AvailableUIDsForRealm("cn"); !reflect.DeepEqual(back, off) {
		t.Errorf("关闭后=%v want 逐字等于旧列表 %v", back, off)
	}

	// 快照失效（时刻已过）：即使开关开着也不产生虚拟槽位。
	p.SetPreferExpiring(true)
	p.SetCreditEarliest("cn1", time.Now().Add(-time.Minute), 40)
	if got2 := p.AvailableUIDsForRealm("cn"); !reflect.DeepEqual(got2, off) {
		t.Errorf("快照已失效时=%v want 旧列表 %v", got2, off)
	}
}

// TestNoteModelCostDecrementsEarliestRemaining credits 内插口径：实扣同样从
// 「最先失效的那一笔」里出（上游 FEFO），余量随之递减、扣穿归零后不再加成。
func TestNoteModelCostDecrementsEarliestRemaining(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 500, 400)
	p.SetCreditEarliest("u1", time.Now().Add(2*time.Hour), 300)

	p.NoteModelCost("u1", "m", 120, 1000) // 实扣 120 → d=120
	p.mu.RLock()
	e := p.byUID["u1"]
	credits, expiring, earliest := e.credits, e.creditsExpiring, e.earliestRemaining
	live := e.expiringNow(time.Now())
	p.mu.RUnlock()
	if credits != 380 || expiring != 280 || earliest != 180 {
		t.Errorf("扣 120 后 credits=%d expiring=%d earliest=%d want 380/280/180",
			credits, expiring, earliest)
	}
	if !live {
		t.Error("仍有 180 余量且未失效：expiringNow 应为 true")
	}

	p.NoteModelCost("u1", "m", 999, 1000) // 扣穿：余额与余量一并归零（不产生负数）
	p.mu.RLock()
	credits, earliest = e.credits, e.earliestRemaining
	live = e.expiringNow(time.Now())
	p.mu.RUnlock()
	if credits != 0 || earliest != 0 {
		t.Errorf("扣穿后 credits=%d earliest=%d want 0/0", credits, earliest)
	}
	if live {
		t.Error("余量归零后 expiringNow 应为 false（无加成）")
	}
}

// TestPreferExpiringPersistRoundTrip 两个字段落盘再读回：重启后到下一个刷新周期
// 之间不失忆（同 creditsExpiring 的持久化理由）。
func TestPreferExpiringPersistRoundTrip(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 500)
	exp := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	p.SetCreditEarliest("u1", exp, 300)
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"earliest_expiry"`) || !strings.Contains(string(raw), `"earliest_remaining": 300`) {
		t.Errorf("落盘应含两字段:\n%s", raw)
	}

	p2 := New(fp)
	defer p2.Close()
	p2.mu.RLock()
	e, ok := p2.byUID["u1"]
	p2.mu.RUnlock()
	if !ok {
		t.Fatal("重启后账号缺失")
	}
	if !e.earliestExpiry.Equal(exp) {
		t.Errorf("恢复后 earliestExpiry=%v want %v", e.earliestExpiry, exp)
	}
	if e.earliestRemaining != 300 {
		t.Errorf("恢复后 earliestRemaining=%d want 300", e.earliestRemaining)
	}
	// 重启后开关默认开且快照可判 → 加成直接可用（不需要等下一次刷新周期补数据）。
	if !p2.PreferExpiring() {
		t.Fatal("重启后 prefer_expiring 应默认开启")
	}
	p2.mu.RLock()
	live := e.expiringNow(time.Now())
	p2.mu.RUnlock()
	if !live {
		t.Error("重启后 expiringNow 应为 true（快照已恢复）")
	}
}

// TestPreferExpiringPersistMissingKeyAndExpired 缺键兼容与过期过滤：
//   - 旧 state.json（无这两键）→ 零值恢复，不报错、不迁移；
//   - 失效时刻已过/非未来 → 丢弃（过期快照不复活）；
//   - 余量超总余额 → 钳到 credits（脏数据防御）。
func TestPreferExpiringPersistMissingKeyAndExpired(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	future := time.Now().Add(72 * time.Hour).Format(time.RFC3339)
	past := time.Now().Add(-72 * time.Hour).Format(time.RFC3339)
	// u1 旧格式（缺键）；u2 过期快照；u3 未来快照 + 超限余量。
	body := fmt.Sprintf(`{"accounts":{"u1":{"credits":100},"u2":{"credits":100,"earliest_expiry":%q,"earliest_remaining":50},"u3":{"credits":100,"earliest_expiry":%q,"earliest_remaining":9999}}}`, past, future)
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	defer p.Close()

	p.mu.RLock()
	e1, e2, e3 := p.byUID["u1"], p.byUID["u2"], p.byUID["u3"]
	p.mu.RUnlock()
	if e1.earliestRemaining != 0 || !e1.earliestExpiry.IsZero() {
		t.Errorf("缺键账号应恢复零值: remaining=%d expiry=%v", e1.earliestRemaining, e1.earliestExpiry)
	}
	if e2.earliestRemaining != 0 || !e2.earliestExpiry.IsZero() {
		t.Errorf("已失效快照应丢弃: remaining=%d expiry=%v", e2.earliestRemaining, e2.earliestExpiry)
	}
	if e3.earliestRemaining != 100 {
		t.Errorf("超限余量应钳到 credits=100, got %d", e3.earliestRemaining)
	}
	if !e3.expiringNow(time.Now()) {
		t.Error("未来快照恢复后 expiringNow 应为 true")
	}

	// 缺键账号后续照常写入（幂等置脏 + 落盘）。
	p.SetCreditEarliest("u1", time.Now().Add(time.Hour), 10)
	p.Flush()
	raw, _ := os.ReadFile(fp)
	if !strings.Contains(string(raw), `"earliest_remaining": 10`) {
		t.Errorf("缺键账号写入后应落盘新快照:\n%s", raw)
	}
}
