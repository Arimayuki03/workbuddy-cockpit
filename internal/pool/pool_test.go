package pool

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestPickHighestCredits(t *testing.T) {
	// P1-8：Top5 加权随机。积分悬殊时高积分账号应被多数选中（不再是必中）。
	p := New("")
	a1 := &auth.Auth{UID: "u1"}
	a2 := &auth.Auth{UID: "u2"}
	a3 := &auth.Auth{UID: "u3"}
	p.Add(a1)
	p.Add(a2)
	p.Add(a3)
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50000)
	p.SetCredits("u3", 300)
	counts := map[string]int{}
	for i := 0; i < 300; i++ {
		counts[p.Pick().UID]++
	}
	if counts["u2"] < 240 { // u2 权重 50000/50400 ≈ 99.2%
		t.Errorf("u2 picked %d/300, want overwhelming majority", counts["u2"])
	}
}

func TestPickSkipsCooling(t *testing.T) {
	p := New("")
	a1 := &auth.Auth{UID: "u1"}
	a2 := &auth.Auth{UID: "u2"}
	p.Add(a1)
	p.Add(a2)
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50)
	p.Cooldown("u1", CoolHard, time.Hour, "test")
	got := p.Pick()
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
}

func TestPickExpiredCooldownReturnsToHealthy(t *testing.T) {
	p := New("")
	a1 := &auth.Auth{UID: "u1"}
	p.Add(a1)
	p.SetCredits("u1", 100)
	p.Cooldown("u1", CoolSoft, time.Millisecond, "429")
	time.Sleep(5 * time.Millisecond)
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("pick=%+v want u1 after cooldown expiry", got)
	}
}

func TestPickNilWhenAllCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "x")
	if got := p.Pick(); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

func TestPickExcluding(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50)
	tried := map[string]bool{"u1": true}
	got := p.PickExcluding(tried)
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
	tried["u2"] = true
	if got := p.PickExcluding(tried); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

func TestPickExcludingStaysWithinHealthy(t *testing.T) {
	// 加权随机不能选出冷却/禁用账号。
	p := New("")
	p.Add(&auth.Auth{UID: "u-cold"})
	p.Add(&auth.Auth{UID: "u-hot"})
	p.SetCredits("u-cold", 9999)
	p.SetCredits("u-hot", 1)
	p.Cooldown("u-cold", CoolHard, time.Hour, "x")
	for i := 0; i < 20; i++ {
		got := p.PickExcluding(nil)
		if got == nil || got.UID != "u-hot" {
			t.Fatalf("iter %d: picked %+v, want only healthy u-hot", i, got)
		}
	}
}

func TestPickWeightedSkewTowardHighCredits(t *testing.T) {
	// Top5 加权随机：单账号 credits 占比足够高时，多数挑中它。
	p := New("")
	for _, u := range []string{"w1", "w2", "w3", "w4", "w5", "w6"} {
		p.Add(&auth.Auth{UID: u})
		p.SetCredits(u, 1)
	}
	p.SetCredits("w1", 1000)
	counts := map[string]int{}
	for i := 0; i < 500; i++ {
		counts[p.Pick().UID]++
	}
	if counts["w1"] < 300 {
		t.Errorf("w1 picked %d/500, want majority (weighted)", counts["w1"])
	}
}

func TestPickWeightedUniformWhenAllZero(t *testing.T) {
	// credits 全为 0 → 退化为均匀随机，不能只挑固定一个。
	p := New("")
	for _, u := range []string{"z1", "z2", "z3"} {
		p.Add(&auth.Auth{UID: u})
	}
	seen := map[string]bool{}
	for i := 0; i < 30; i++ {
		seen[p.Pick().UID] = true
	}
	if len(seen) != 3 {
		t.Errorf("uniform fallback should hit all, seen=%v", seen)
	}
}

func TestPickWeightedTopFiveOnly(t *testing.T) {
	// 第 6 高 credits 的账号在 Top5 之外，权重抽签永远轮不到它。
	p := New("")
	for _, u := range []string{"a1", "a2", "a3", "a4", "a5", "a6"} {
		p.Add(&auth.Auth{UID: u})
	}
	p.SetCredits("a1", 1000)
	p.SetCredits("a2", 1000)
	p.SetCredits("a3", 1000)
	p.SetCredits("a4", 1000)
	p.SetCredits("a5", 1000)
	p.SetCredits("a6", 5) // Top5 之外
	for i := 0; i < 2000; i++ {
		if got := p.Pick(); got == nil || got.UID == "a6" {
			t.Fatalf("iter %d: picked %+v, a6 must stay outside top-5", i, got)
		}
	}
}

func TestPickDeterministicViaSetRandomSource(t *testing.T) {
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50)
	// r=0 ∈ [0,50) → 命中 u1。注入源应使选号完全确定。
	for i := 0; i < 50; i++ {
		if got := p.Pick(); got == nil || got.UID != "u1" {
			t.Fatalf("iter %d: pick=%+v want u1 (deterministic)", i, got)
		}
	}
}

func TestCooldownPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.Flush() // 状态变更走 dirty 标志，落盘由 Flush / 后台 goroutine 负责
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.Pick() != nil {
		t.Fatal("cooldown lost after reload")
	}
	st, ok := p2.Status("u1")
	if !ok || st.Reason != "余额不足" {
		t.Errorf("status=%+v ok=%v", st, ok)
	}
}

func TestDisablePersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	p.Flush()
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.Pick() != nil {
		t.Fatal("disabled account picked after reload")
	}
	st, _ := p2.Status("u1")
	if !st.Disabled || st.Reason != "12153 session dead" {
		t.Errorf("status=%+v", st)
	}
}

func TestReenableIfCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.ReenableIfCredits("u1", 500)
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("should reenable, pick=%+v", got)
	}
}

func TestReenableZeroCreditsKeepsCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.ReenableIfCredits("u1", 0)
	if p.Pick() != nil {
		t.Fatal("zero credits should stay cooling")
	}
}

func TestReenableDoesNotTouchDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p.ReenableIfCredits("u1", 500)
	if p.Pick() != nil {
		t.Fatal("disabled must not auto-reenable")
	}
}

func TestNoteErrorThreshold(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for i := 0; i < 2; i++ {
		p.NoteError("u1", 3, 10*time.Minute)
		if p.Pick() == nil {
			t.Fatalf("cooling too early at %d", i+1)
		}
	}
	p.NoteError("u1", 3, 10*time.Minute)
	if p.Pick() != nil {
		t.Fatal("threshold 3 should cool the account")
	}
}

func TestNoteSuccessResetsCounter(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteError("u1", 3, time.Hour)
	p.NoteError("u1", 3, time.Hour)
	p.NoteSuccess("u1")
	p.NoteError("u1", 3, time.Hour)
	p.NoteError("u1", 3, time.Hour)
	if p.Pick() == nil {
		t.Fatal("success should reset error counter")
	}
}

func TestNextDay4AMBoundaries(t *testing.T) {
	cases := []struct {
		name string
		now  string // RFC3339 (UTC 表示)
		want string // 次日 04:00（同一时区，UTC 表示）
	}{
		{"普通日", "2026-08-28T17:00:00+08:00", "2026-08-29T04:00:00+08:00"},
		{"凌晨未到4点", "2026-08-28T03:59:59+08:00", "2026-08-29T04:00:00+08:00"},
		{"正好4点", "2026-08-28T04:00:00+08:00", "2026-08-29T04:00:00+08:00"},
		{"4点刚过", "2026-08-28T04:00:01+08:00", "2026-08-29T04:00:00+08:00"},
		{"月末(31天月)", "2026-01-31T12:00:00+08:00", "2026-02-01T04:00:00+08:00"},
		{"月末(28天月)", "2026-02-28T12:00:00+08:00", "2026-03-01T04:00:00+08:00"},
		{"闰年月末", "2028-02-29T12:00:00+08:00", "2028-03-01T04:00:00+08:00"},
		{"年末", "2026-12-31T23:59:59+08:00", "2027-01-01T04:00:00+08:00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, c.now)
			if err != nil {
				t.Fatal(err)
			}
			want, err := time.Parse(time.RFC3339, c.want)
			if err != nil {
				t.Fatal(err)
			}
			if got := nextDay4AM(now); !got.Equal(want) {
				t.Errorf("nextDay4AM(%v)=%v want %v", c.now, got, want)
			}
		})
	}
}

func TestCooldownUntilTomorrow4AM(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	before := time.Now()
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	after := time.Now()
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if !st.Cooling {
		t.Fatalf("should be cooling: %+v", st)
	}
	if st.Reason != "余额不足" {
		t.Errorf("reason=%q", st.Reason)
	}
	// 冷却截止必须是"此刻之后的最近一个 04:00"：晚于 now、距今不超过 24h。
	if st.Until.Before(after) {
		t.Errorf("until %v is in the past (call span %v..%v)", st.Until, before, after)
	}
	if st.Until.Hour() != 4 {
		t.Errorf("until hour=%d want 4", st.Until.Hour())
	}
	if d := st.Until.Sub(after); d > 24*time.Hour {
		t.Errorf("until %v is more than 24h out: %v", st.Until, d)
	}
	// 冷却拒选。
	if p.Pick() != nil {
		t.Fatal("cooling account should not be picked")
	}
}

func TestCooldownUntilTomorrow4AMPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	p.Flush()
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.Pick() != nil {
		t.Fatal("4am cooldown lost after reload")
	}
	st, ok := p2.Status("u1")
	if !ok || st.Until.Hour() != 4 || st.Reason != "余额不足" {
		t.Errorf("status after reload=%+v ok=%v", st, ok)
	}
}

func TestList(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "nick1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 42)
	p.Cooldown("u2", CoolSoft, time.Minute, "429")
	list := p.List()
	if len(list) != 2 {
		t.Fatalf("list=%d", len(list))
	}
	var s1, s2 Status
	for _, s := range list {
		if s.UID == "u1" {
			s1 = s
		}
		if s.UID == "u2" {
			s2 = s
		}
	}
	if s1.Credits != 42 || s1.Nickname != "nick1" || s1.Disabled || s1.Cooling {
		t.Errorf("s1=%+v", s1)
	}
	if !s2.Cooling || s2.Reason != "429" {
		t.Errorf("s2=%+v", s2)
	}
}

func TestRemoveMissingFromDir(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SyncToDir([]*auth.Auth{{UID: "u2"}})
	if p.Pick() == nil || p.Pick().UID != "u2" {
		t.Fatal("u1 should be removed")
	}
	if _, ok := p.Status("u1"); ok {
		t.Fatal("u1 should not exist")
	}
}

func TestFlushPersistsCredits(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 42)
	p.Flush()
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || st.Credits != 42 {
		t.Fatalf("flush not persisted: %+v ok=%v", st, ok)
	}
}

func TestAutoFlush(t *testing.T) {
	old := flushInterval
	flushInterval = 20 * time.Millisecond
	defer func() { flushInterval = old }()

	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 77)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(fp); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("state.json not written by background flusher")
		}
		time.Sleep(10 * time.Millisecond)
	}
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || st.Credits != 77 {
		t.Fatalf("auto flush not persisted: %+v ok=%v", st, ok)
	}
}

func TestFlushIdempotentWhenClean(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Flush() // 无 dirty，不应写盘
	if _, err := os.Stat(fp); !os.IsNotExist(err) {
		t.Fatalf("flush on clean pool should not write: %v", err)
	}
}
