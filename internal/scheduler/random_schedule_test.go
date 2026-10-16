package scheduler

// random_schedule_test.go v1.19.2 每日随机延后 + 账号错峰（吸收 workbuddy-manager
// 补丁 0003 / CHANGELOG 1.0.91）行为契约：
//   - 偏移确定性：种子经 Config 注入、日期由夹具给定——同种子（含「重启重建」）派生相同、
//     不同部署/不同任务族/不同日期各自独立，用例不依赖真实时间与进程内随机状态；
//   - 延后与跨槽补跑共存：同小时先后两个槽位、延后到临近下一槽位时都不丢槽、不重复派发
//     （不破坏 scheduler.go 的 cursor 补跑语义）；
//   - 账号错峰：等待落在 [min,max] 区间、可被 ctx 取消、关闭（缺省/旧配置）时旧节流不变；
//     手动入口（背景 ctx）不错峰；
//   - 错峰开启时同批多类任务串行派发（否则两条账号遍历并行 = 错峰失效）。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// jitterSched 随机延后测试的最小调度器：签到/旅行同配 9 点与 21 点（便于构造「同一
// 小时两个槽位」），其余任务禁用——用例只关心枚举口径，不引入无关小时。
func jitterSched(seed string, minutes int) *Scheduler {
	return New(Config{
		RandomDelayMinutes: minutes,
		RandomDelaySeed:    seed,
		CheckinHours:       []int{9, 21},
		TravelHours:        []int{9, 21},
		ActivityDisabled:   true,
		KeepaliveDisabled:  true,
		SchoolDisabled:     true,
		CatDisabled:        true,
	})
}

// slotOffset 槽位相对原定整点的偏移（把偏移从绝对时刻里剥出来比较）。
func slotOffset(s *Scheduler, day time.Time, hour int, kind taskKind) time.Duration {
	base := time.Date(day.Year(), day.Month(), day.Day(), hour, 0, 0, 0, day.Location())
	return s.scheduledSlot(day, hour, kind).Sub(base)
}

// TestRandomDelaySlotDeterministic 偏移确定性：同种子重建（模拟重启）后同一槽位不变、
// 同一天重复派生恒定；跨天/跨任务族/跨部署各自独立取数。断言用「候选集里存在差异」
// 而不是钉死具体哈希值——改哈希输入格式时失败信息仍能指出哪一维丢了独立性。
func TestRandomDelaySlotDeterministic(t *testing.T) {
	day := time.Date(2026, 10, 14, 0, 0, 0, 0, cstZone)
	a := jitterSched("deploy-a", 60)
	same := jitterSched("deploy-a", 60) // 重启：同种子同配置重建
	got := a.scheduledSlot(day, 9, taskCheckin)

	if !got.Equal(same.scheduledSlot(day, 9, taskCheckin)) {
		t.Fatalf("同种子重建后偏移变了（重启会抖动）: %s vs %s", got, same.scheduledSlot(day, 9, taskCheckin))
	}
	if !got.Equal(a.scheduledSlot(day, 9, taskCheckin)) {
		t.Fatal("同一天同一槽位重复派生结果不稳定")
	}
	base := slotOffset(a, day, 9, taskCheckin)

	dayDiffers := false
	for d := 1; d <= 30 && !dayDiffers; d++ {
		dayDiffers = slotOffset(a, day.AddDate(0, 0, d), 9, taskCheckin) != base
	}
	if !dayDiffers {
		t.Fatal("30 天内偏移恒定：跨天没有独立取数")
	}
	taskDiffers := false
	for k := taskCheckin; k < kindCount && !taskDiffers; k++ {
		taskDiffers = slotOffset(a, day, 9, k) != base
	}
	if !taskDiffers {
		t.Fatal("同一小时内所有任务族偏移相同：跨任务族没有独立取数")
	}
	deployDiffers := false
	for i := 0; i < 20 && !deployDiffers; i++ {
		deployDiffers = slotOffset(jitterSched(fmt.Sprintf("deploy-%d", i), 60), day, 9, taskCheckin) != base
	}
	if !deployDiffers {
		t.Fatal("不同种子的偏移全相同：部署之间没有独立取数")
	}
}

// TestRandomDelaySlotStaysInHourAndDay 偏移恒落在原定小时之内、不跨自然日（跨日会让
// 按日枚举丢槽）；60 分钟档最多偏移到 59:59，与下一整点槽位不重叠。
func TestRandomDelaySlotStaysInHourAndDay(t *testing.T) {
	day := time.Date(2026, 10, 14, 0, 0, 0, 0, cstZone)
	for _, minutes := range []int{1, 30, 59, 60} {
		s := jitterSched("deploy-a", minutes)
		limit := time.Duration(minutes) * time.Minute
		if minutes >= 60 {
			limit = time.Hour - time.Second
		}
		for d := 0; d < 30; d++ {
			date := day.AddDate(0, 0, d)
			for hour := 0; hour < 24; hour++ {
				for kind := taskCheckin; kind < kindCount; kind++ {
					at := s.scheduledSlot(date, hour, kind)
					if at.Hour() != hour || at.Format("2006-01-02") != date.Format("2006-01-02") {
						t.Fatalf("越出原定小时/自然日: minutes=%d date=%s at=%s",
							minutes, date.Format("2006-01-02"), at.Format(time.RFC3339))
					}
					if off := slotOffset(s, date, hour, kind); off < 0 || off > limit {
						t.Fatalf("偏移越界: minutes=%d off=%s limit=%s", minutes, off, limit)
					}
				}
			}
		}
	}
}

// TestRandomDelayZeroKeepsExactHoursAndMerge 缺省/旧配置（random_delay_minutes=0）：
// 槽位仍是整点、同整点多类任务仍合并成一批——旧行为逐字不变。
func TestRandomDelayZeroKeepsExactHoursAndMerge(t *testing.T) {
	s := jitterSched("deploy-a", 0)
	now := time.Date(2026, 10, 14, 8, 30, 0, 0, cstZone)
	at, kinds := s.nextWake(now)
	want := time.Date(2026, 10, 14, 9, 0, 0, 0, cstZone)
	if !at.Equal(want) {
		t.Fatalf("延后关闭后应仍为整点: %s want %s", at, want)
	}
	if len(kinds) != 2 || !hasKind(kinds, taskCheckin) || !hasKind(kinds, taskTravel) {
		t.Fatalf("同整点多类任务应合并: %v", kinds)
	}
	batches := s.dueBatches(at, time.Time{}, at)
	if len(batches) != 1 || !batches[0].at.Equal(want) || len(batches[0].kinds) != 2 {
		t.Fatalf("延后关闭后同刻仍应合成一批: %+v", batches)
	}
}

// TestRandomDelayNextWakeAndCatchupAgree 随机延后下 nextWake 与 dueBatches 口径一致：
// timer 到点那一刻补跑恰好派发该槽位（不丢、不重复），cursor 前移后不再重放。
func TestRandomDelayNextWakeAndCatchupAgree(t *testing.T) {
	s := jitterSched("deploy-a", 60)
	day := time.Date(2026, 10, 14, 0, 0, 0, 0, cstZone)
	next, kinds := s.nextWake(day)
	if next.IsZero() || len(kinds) == 0 {
		t.Fatal("夹具应有排程")
	}
	if !next.After(day.Add(9 * time.Hour)) {
		t.Fatalf("随机延后应把 9 点槽位推到 9 点之后: %s", next.Format(time.RFC3339))
	}
	due := s.dueBatches(next, time.Time{}, next)
	if len(due) != 1 || !due[0].at.Equal(next) {
		t.Fatalf("timer 到点应恰好补跑该槽位: %+v want %s", due, next.Format(time.RFC3339))
	}
	if len(due[0].kinds) != len(kinds) || !hasKind(due[0].kinds, kinds[0]) {
		t.Fatalf("nextWake 与 dueBatches 的批内任务不一致: %v vs %v", kinds, due[0].kinds)
	}
	if again := s.dueBatches(next, next, next); len(again) != 0 {
		t.Fatalf("已派发槽位被重放: %+v", again)
	}
}

// findSameHourSlots 找「签到与旅行同在 9 点、偏移不同且都 >0」的日期夹具：
// 同小时内先后两个槽位是随机延后开启后的常态。
func findSameHourSlots(t *testing.T, s *Scheduler, start time.Time) (early, late time.Time) {
	t.Helper()
	for d := 0; d < 120; d++ {
		date := start.AddDate(0, 0, d)
		first := s.scheduledSlot(date, 9, taskCheckin)
		second := s.scheduledSlot(date, 9, taskTravel)
		if first.Equal(second) {
			continue
		}
		if second.Before(first) {
			first, second = second, first
		}
		if first.After(time.Date(date.Year(), date.Month(), date.Day(), 9, 0, 0, 0, date.Location())) {
			return first, second
		}
	}
	t.Fatal("120 天内未找到同小时双槽位夹具（种子或偏移派生改动？）")
	return time.Time{}, time.Time{}
}

// TestRandomDelayCatchupSameHourSlotsNoLossNoDuplicate 同一小时内先后两个延后槽位：
// 补跑两个都要派发（各一批）、第一个派发后（cursor 停在它上面）第二个到点必须仍被发现
// ——按「cursor+1h 逐整点步进」的旧口径会把它整段跳过一个小时（丢槽）。
func TestRandomDelayCatchupSameHourSlotsNoLossNoDuplicate(t *testing.T) {
	s := jitterSched("deploy-a", 60)
	day := time.Date(2026, 10, 14, 0, 0, 0, 0, cstZone)
	early, late := findSameHourSlots(t, s, day)

	// 睡前 timer 排的是较早那个；睡到两个槽位都过点后醒来。
	now := late.Add(time.Minute)
	batches := s.dueBatches(now, time.Time{}, early)
	if len(batches) != 2 {
		t.Fatalf("补跑应含同小时两个槽位: %+v", batches)
	}
	if !batches[0].at.Equal(early) || !batches[1].at.Equal(late) {
		t.Fatalf("补跑顺序/时刻不对: %s / %s want %s / %s",
			batches[0].at.Format(time.RFC3339), batches[1].at.Format(time.RFC3339),
			early.Format(time.RFC3339), late.Format(time.RFC3339))
	}
	if len(batches[0].kinds) != 1 || len(batches[1].kinds) != 1 {
		t.Fatalf("同小时不同偏移的两类任务不应并成一批: %+v", batches)
	}

	// 第一个槽位已派发（cursor=early）→ 第二个槽位到点仍必须被发现。
	due := s.dueBatches(late, early, late)
	if len(due) != 1 || !due[0].at.Equal(late) {
		t.Fatalf("过早派发后同小时后一个槽位被丢掉: %+v want %s", due, late.Format(time.RFC3339))
	}
	// 第二个槽位也派发（cursor=late）→ 不重放。
	if again := s.dueBatches(late, late, late); len(again) != 0 {
		t.Fatalf("已派发槽位被重放: %+v", again)
	}
}

// TestRandomDelayCatchupNearNextHourSlot 延后到「临近下一整点槽位」：签到 09:5x 与
// 旅行 10:0x 相隔很近时，睡过两个槽位后补跑按时间升序各派发一次，不丢槽、不重复执行。
func TestRandomDelayCatchupNearNextHourSlot(t *testing.T) {
	s := New(Config{
		RandomDelayMinutes: 60,
		RandomDelaySeed:    "deploy-adjacent",
		CheckinHours:       []int{9},
		TravelHours:        []int{10},
		ActivityDisabled:   true,
		KeepaliveDisabled:  true,
		SchoolDisabled:     true,
		CatDisabled:        true,
	})
	day := time.Date(2026, 10, 14, 0, 0, 0, 0, cstZone)
	var checkinAt, travelAt time.Time
	for d := 0; d < 200 && checkinAt.IsZero(); d++ {
		date := day.AddDate(0, 0, d)
		c := s.scheduledSlot(date, 9, taskCheckin)
		tr := s.scheduledSlot(date, 10, taskTravel)
		if gap := tr.Sub(c); gap > 0 && gap < 30*time.Minute {
			checkinAt, travelAt = c, tr
		}
	}
	if checkinAt.IsZero() {
		t.Fatal("200 天内未找到「相邻两小时槽位间隔 <30min」的夹具")
	}
	// 睡过两个槽位再醒：补跑按时间升序各一批，恰好两个。
	batches := s.dueBatches(travelAt.Add(time.Minute), time.Time{}, checkinAt)
	if len(batches) != 2 || !batches[0].at.Equal(checkinAt) || !batches[1].at.Equal(travelAt) {
		t.Fatalf("相邻槽位补跑不完整: %+v", batches)
	}
	if len(batches[0].kinds) != 1 || batches[0].kinds[0] != taskCheckin ||
		len(batches[1].kinds) != 1 || batches[1].kinds[0] != taskTravel {
		t.Fatalf("相邻槽位应各成一批且任务正确: %+v", batches)
	}
	// 签到那批已派发（cursor）→ 旅行那批到点时仍被发现（不丢槽），且签到不重放。
	due := s.dueBatches(travelAt, checkinAt, travelAt)
	if len(due) != 1 || !due[0].at.Equal(travelAt) || due[0].kinds[0] != taskTravel {
		t.Fatalf("cursor 跨小时后旅行槽位丢失或被重复: %+v", due)
	}
}

// TestAccountDelayGapRangeAndManualBypass 错峰等待落在 [min,max]（叠加既有最小间隔），
// 手动路径（背景 ctx 无标记）与区间关闭（缺省/旧配置）逐字返回既有间隔。
func TestAccountDelayGapRangeAndManualBypass(t *testing.T) {
	s := New(Config{AccountDelayMinSeconds: 30, AccountDelayMaxSeconds: 120})
	scheduled := context.WithValue(context.Background(), scheduledAccountKey{}, true)
	base := 800 * time.Millisecond
	for i := 0; i < 200; i++ {
		gap := s.accountGap(scheduled, base)
		if gap < base+30*time.Second || gap > base+120*time.Second {
			t.Fatalf("第 %d 次错峰等待越界: %s", i, gap)
		}
	}
	if got := s.accountGap(context.Background(), base); got != base {
		t.Fatalf("手动路径被加了等待（应立即执行）: %s", got)
	}
	s.cfg.AccountDelayMinSeconds, s.cfg.AccountDelayMaxSeconds = 0, 0
	if got := s.accountGap(scheduled, base); got != base {
		t.Fatalf("区间关闭后旧节流被改动: %s", got)
	}
	// min==max：区间退化为固定值（可精确断言）。
	fixed := New(Config{AccountDelayMinSeconds: 45, AccountDelayMaxSeconds: 45})
	if got := fixed.accountGap(scheduled, 0); got != 45*time.Second {
		t.Fatalf("min==max 应为固定 45s: %s", got)
	}
}

// TestScheduledCheckinStaggeringCancellable 定时签到路径的错峰等待可被 ctx 取消
// （取消后立即返回，不等满随机区间），且已处理账号的回照常返回；同一配置下手动
// 入口（CheckinAll）两个账号立即处理完——手动「立即执行」不受错峰影响。
func TestScheduledCheckinStaggeringCancellable(t *testing.T) {
	s, p := newCheckinS(t, &checkinStub{checkinBody: `{"code":0,"data":{}}`, resourceRemain: 100})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	s.cfg.AccountDelayMinSeconds, s.cfg.AccountDelayMaxSeconds = 30, 120

	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), scheduledAccountKey{}, true), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	out, err := s.checkinAll(ctx)
	if err != context.DeadlineExceeded || len(out) != 1 {
		t.Fatalf("取消后的定时批次: out=%d err=%v want 1/DeadlineExceeded", len(out), err)
	}
	if e := time.Since(started); e > time.Second {
		t.Fatalf("取消后仍等满随机延迟: %v", e)
	}

	out, err = s.CheckinAll()
	if err != nil || len(out) != 2 {
		t.Fatalf("手动批次应两号立即处理: out=%d err=%v", len(out), err)
	}
}

// TestScheduledCheckinStaggeringWaitsBetweenAccounts 错峰真的等：把区间单位缩到毫秒
// （accountDelayUnit 注入点，风格同 activityAccountDelay「测试可置 0」），断言两号之间
// 至少等满 min（且不超过 max 量级）。
func TestScheduledCheckinStaggeringWaitsBetweenAccounts(t *testing.T) {
	old := accountDelayUnit
	accountDelayUnit = time.Millisecond
	t.Cleanup(func() { accountDelayUnit = old })

	s, p := newCheckinS(t, &checkinStub{checkinBody: `{"code":0,"data":{}}`, resourceRemain: 100})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	s.cfg.AccountDelayMinSeconds, s.cfg.AccountDelayMaxSeconds = 50, 80

	started := time.Now()
	out, err := s.checkinAll(context.WithValue(context.Background(), scheduledAccountKey{}, true))
	if err != nil || len(out) != 2 {
		t.Fatalf("定时批次: out=%d err=%v", len(out), err)
	}
	if e := time.Since(started); e < 50*time.Millisecond {
		t.Fatalf("两号之间未等待错峰区间: %v", e)
	} else if e > 5*time.Second {
		t.Fatalf("等待超出区间上限量级: %v", e)
	}
}

// TestRunBatchSerializesKindsWhenStaggeringEnabled 账号错峰开启时同批多类任务串行派发
// （两条账号遍历并行 = 错峰失效）；缺省（旧配置）保持既有并行语义。
// 判据：上游同时在途请求数的峰值——串行时恒为 1，并行时签到与保活会同时在途。
func TestRunBatchSerializesKindsWhenStaggeringEnabled(t *testing.T) {
	for _, tc := range []struct {
		name      string
		lo, hi    int
		wantPeak  int32
		peakCheck string
	}{
		{"错峰开启（串行）", 1, 1, 1, "不应并发"},
		{"错峰缺省关闭（并行）", 0, 0, 2, "应并发"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var now, peak atomic.Int32
			enter := func() {
				n := now.Add(1)
				for old := peak.Load(); n > old; old = peak.Load() {
					if peak.CompareAndSwap(old, n) {
						return
					}
				}
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				enter()
				defer now.Add(-1)
				time.Sleep(60 * time.Millisecond) // 拉长在途窗口，让并行可观测
				switch {
				case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
					w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
				case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
					w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":50,"CycleCapacityUsed":0}]}}}}`))
				case strings.HasSuffix(r.URL.Path, "/token/refresh"):
					w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
				default:
					http.Error(w, "not found", 404)
				}
			}))
			defer srv.Close()

			p := pool.New("")
			p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
			up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
			s := New(Config{Pool: p, Upstream: up, AccountDelayMinSeconds: tc.lo, AccountDelayMaxSeconds: tc.hi})

			s.runBatch(context.Background(), []taskKind{taskCheckin, taskKeepalive})
			if got := peak.Load(); tc.hi > 0 && got != tc.wantPeak {
				t.Fatalf("在途峰值=%d want %d（错峰开启时同类遍历%s并行）", got, tc.wantPeak, tc.peakCheck)
			} else if tc.hi == 0 && got < tc.wantPeak {
				t.Fatalf("在途峰值=%d want >=%d（缺省应保持并行派发）", got, tc.wantPeak)
			}
		})
	}
}
