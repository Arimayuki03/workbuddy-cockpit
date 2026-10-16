// credit_exhausted_test.go 积分耗尽冷却时长（config 键 cooldown.credit_exhausted，
// 吸收 workbuddy-manager 补丁 0004）在池内的行为护栏：
//   - 不配（0）= 旧行为逐字不变：CoolHard 冷却到次日 04:00，且全冷却兜底仍排除它；
//   - 配正值 = 固定时长软冷却（CoolSoft）、到期回池、不做指数堆加；
//   - 面板/运维把值改回 0 立刻回到旧行为；负值非法保留现值。
package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestCreditExhaustedDefaultKeepsHardCooldown 默认（没有 credit_exhausted）必须保持
// 旧行为：硬冷却到次日 04:00。这是「不配就一点都别变」的护栏——老部署升级后行为
// 必须逐字不变（含 CoolHard 的兜底排除语义：调了必 402，浪费轮换）。
func TestCreditExhaustedDefaultKeepsHardCooldown(t *testing.T) {
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
		t.Fatalf("应处于冷却：%+v", st)
	}
	if st.CoolKind != "hard_credit" {
		t.Errorf("cool_kind=%q want hard_credit（默认仍是硬冷却）", st.CoolKind)
	}
	if st.Until.Hour() != 4 {
		t.Errorf("until=%v want 次日 04:00（hour=4）", st.Until)
	}
	// 截止必须落在「此刻之后的最近一个 04:00」：晚于 now、距今不超过 24h
	//（凌晨 00:00~04:00 触发落在当天 04:00，其余落在次日 04:00）。
	if st.Until.Before(after) || st.Until.Sub(after) > 24*time.Hour {
		t.Errorf("until %v 必须落在 (now, now+24h]（调用窗 %v..%v）", st.Until, before, after)
	}
	if st.Reason != "余额不足" {
		t.Errorf("reason=%q want 余额不足", st.Reason)
	}
	// 全冷却时硬冷却（余额耗尽）号不参与兜底 → 返回 nil（等签到恢复）。
	if got := p.Pick(""); got != nil {
		t.Errorf("硬冷却号不该被兜底选中，got %+v", got)
	}
}

// TestCreditExhaustedSoftCooldownFromConfig 配正值 → 固定时长软冷却：截止 ≈ now+配置值
// （不是次日 04:00）、cool_kind=soft_rate、soft_streak 不推进（Cooldown 只写固定时长，
// 不并入指数退避域）。
func TestCreditExhaustedSoftCooldownFromConfig(t *testing.T) {
	for _, dur := range []time.Duration{10 * time.Minute, 30 * time.Second} {
		t.Run(dur.String(), func(t *testing.T) {
			p := New("")
			p.SetCreditExhausted(dur)
			p.Add(&auth.Auth{UID: "u1"})
			// 写入时刻落在 [before, after] 内：以 before 为锚，允许少量调度延迟（+5s），
			// 但绝不接受 04:00 那样的量级偏差（软冷却没生效）。
			before := time.Now()
			p.CooldownUntilTomorrow4AM("u1", "余额不足")

			st, ok := p.Status("u1")
			if !ok {
				t.Fatal("no status")
			}
			if st.CoolKind != "soft_rate" {
				t.Errorf("cool_kind=%q want soft_rate", st.CoolKind)
			}
			if d := st.Until.Sub(before); d < dur-time.Second || d > dur+5*time.Second {
				t.Errorf("until-now=%v want ≈%v", d, dur)
			}
			if st.SoftStreak != 0 {
				t.Errorf("soft_streak=%d want 0（固定时长，不做指数堆加）", st.SoftStreak)
			}
			if st.Reason != "余额不足" {
				t.Errorf("reason=%q want 余额不足", st.Reason)
			}
		})
	}
}

// TestCreditExhaustedRepeatedCallsDoNotStack 重复触发（重试/并发再撞同一个 402 号）后
// 剩余时长仍是配置值本身（≈10m），不是 ×2 的指数堆加、也不是 04:00：固定时长语义。
func TestCreditExhaustedRepeatedCallsDoNotStack(t *testing.T) {
	const dur = 10 * time.Minute
	p := New("")
	p.SetCreditExhausted(dur)
	p.Add(&auth.Auth{UID: "u1"})

	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	p.CooldownUntilTomorrow4AM("u1", "余额不足") // 第二次触发（重试路径）

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if st.CoolKind != "soft_rate" {
		t.Errorf("cool_kind=%q want soft_rate", st.CoolKind)
	}
	if d := st.Until.Sub(time.Now()); d < dur-time.Second || d > dur+5*time.Second {
		t.Errorf("重复触发后剩余 %v want ≈%v（不堆加）", d, dur)
	}
	if st.SoftStreak != 0 {
		t.Errorf("soft_streak=%d want 0", st.SoftStreak)
	}
}

// TestCreditExhaustedReturnsAfterWindow 窗口过去即回池（本特性的核心诉求：不是等签到）：
// 到期后不再 cooling、选号重新可见。
func TestCreditExhaustedReturnsAfterWindow(t *testing.T) {
	p := New("")
	p.SetCreditExhausted(5 * time.Minute)
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownUntilTomorrow4AM("u1", "余额不足")

	if st, _ := p.Status("u1"); !st.Cooling {
		t.Fatal("冷却期内应处于 cooling")
	}
	// 把截止拨到过去 = 窗口过去（不真等 5 分钟；与既有 soft cooldown 测试同手法）。
	p.mu.Lock()
	p.byUID["u1"].until = time.Now().Add(-time.Second)
	p.mu.Unlock()

	st, _ := p.Status("u1")
	if st.Cooling {
		t.Errorf("窗口过后不该还在冷却：%+v", st)
	}
	if st.CoolKind != "" {
		t.Errorf("窗口过后 cool_kind 应清空（陈旧值会误导排查）：%q", st.CoolKind)
	}
	if got := p.Pick(""); got == nil || got.UID != "u1" {
		t.Errorf("窗口过后应回池可选中，got %+v", got)
	}
}

// TestCreditExhaustedSetterSemantics SetCreditExhausted 的入参语义（面板热改同路）：
// 0 是**合法值**（回到旧行为开关），不能沿用 SetSoftRateMax「非正值即忽略」的写法；
// 负值非法保留现值。这是「配 0 = 逐字旧行为」在 setter 层的护栏。
func TestCreditExhaustedSetterSemantics(t *testing.T) {
	cases := []struct {
		name string
		sets []time.Duration // 依次调用 SetCreditExhausted
		want string          // 期望 cool_kind
	}{
		{"配10m=软冷却", []time.Duration{10 * time.Minute}, "soft_rate"},
		{"配0=旧行为硬冷却", []time.Duration{0}, "hard_credit"},
		{"10m改0=回旧行为", []time.Duration{10 * time.Minute, 0}, "hard_credit"},
		{"负值非法保留现值10m", []time.Duration{10 * time.Minute, -time.Minute}, "soft_rate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := New("")
			for _, d := range c.sets {
				p.SetCreditExhausted(d)
			}
			p.Add(&auth.Auth{UID: "u1"})
			p.CooldownUntilTomorrow4AM("u1", "余额不足")

			st, ok := p.Status("u1")
			if !ok {
				t.Fatal("no status")
			}
			if st.CoolKind != c.want {
				t.Errorf("cool_kind=%q want %q", st.CoolKind, c.want)
			}
			if c.want == "hard_credit" && st.Until.Hour() != 4 {
				t.Errorf("旧行为路径的 until=%v want 04:00", st.Until)
			}
		})
	}
}
