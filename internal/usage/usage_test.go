package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// 成功/失败尝试计数、total 的 pt+ct 兜底口径、按域/账号聚合。
func TestAddAndTotals(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 100, HasPromptTokens: true, CompletionTokens: 50, HasCompletion: true, LatencyMs: 200, HasLatency: true}, true)
	// 失败尝试：无 usage → 只计请求数与失败数，token 不加。
	r.Add(now, "global", "uid1", "claude-4.6", Delta{}, false)
	// 上游没给 total 时用 pt+ct 兜底，保证总量口径连续。
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 10, HasPromptTokens: true, CompletionTokens: 5, HasCompletion: true}, true)

	s := r.Snapshot(24, nil)
	if s.Totals.Requests != 3 || s.Totals.Errors != 1 {
		t.Fatalf("requests/errors = %d/%d, want 3/1", s.Totals.Requests, s.Totals.Errors)
	}
	if s.Totals.PromptTokens != 110 || s.Totals.CompletionTok != 55 {
		t.Fatalf("pt/ct = %d/%d, want 110/55", s.Totals.PromptTokens, s.Totals.CompletionTok)
	}
	if s.Totals.TotalTokens != 165 {
		t.Fatalf("tt = %d, want 165（无 total 时按 pt+ct 兜底）", s.Totals.TotalTokens)
	}
	if s.Totals.AvgLatencyMs != 200 {
		t.Fatalf("avg latency = %v, want 200", s.Totals.AvgLatencyMs)
	}
	if len(s.ByRealm) != 2 {
		t.Fatalf("by_realm = %d 项, want 2", len(s.ByRealm))
	}
	if s.ByAccount[0].Realm == "" {
		t.Fatal("by_account 行缺 realm 标注")
	}
}

// Rollup 把超出 hourlyKeep 的小时桶折叠为日桶，且幂等：重复折叠不重复计数。
func TestRollupIdempotent(t *testing.T) {
	r := New("")
	old := time.Now().AddDate(0, 0, -100) // 100 天前，超出 90 天小时保留
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 1, HasPromptTokens: true}, true)

	r.Rollup(time.Now())
	after := r.Snapshot(24, nil)
	if after.Totals.Requests != 3 || after.Totals.PromptTokens != 15 {
		t.Fatalf("折叠后 totals = %d/%d, want 3/15", after.Totals.Requests, after.Totals.PromptTokens)
	}
	if len(after.Series) != 2 || after.Series[0].Scope != "day" || after.Series[1].Scope != "hour" {
		t.Fatalf("series = %+v, want 日点在前 + 小时点在后", after.Series)
	}

	r.Rollup(time.Now())
	again := r.Snapshot(24, nil)
	if again.Totals.Requests != 3 || again.Totals.PromptTokens != 15 {
		t.Fatalf("二次折叠后 totals = %d/%d, want 3/15（幂等被破坏）", again.Totals.Requests, again.Totals.PromptTokens)
	}
}

// 落盘→新实例恢复，数据不丢；落盘结构带版本号。
func TestFlushLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r1 := New(path)
	r1.Add(time.Now(), "cn", "u1", "glm-5.2", Delta{PromptTokens: 42, HasPromptTokens: true, TotalTokens: 42, HasTotal: true}, true)
	r1.Save()

	r2 := New(path)
	s := r2.Snapshot(24, nil)
	if s.Totals.Requests != 1 || s.Totals.TotalTokens != 42 {
		t.Fatalf("恢复后 totals = %d/%d, want 1/42", s.Totals.Requests, s.Totals.TotalTokens)
	}
	raw, _ := os.ReadFile(path)
	var f file
	if err := json.Unmarshal(raw, &f); err != nil || f.Version != 1 || len(f.Buckets) != 1 {
		t.Fatalf("落盘文件异常: err=%v buckets=%d", err, len(f.Buckets))
	}
}

// Snapshot 把小时窗口外的细粒度并入日点，时序不出现空洞。
func TestSnapshotStitching(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now.Add(-48*time.Hour), "cn", "u", "m", Delta{PromptTokens: 5, HasPromptTokens: true}, true) // 窗口(24h)外 → 日点
	r.Add(now, "cn", "u", "m", Delta{PromptTokens: 3, HasPromptTokens: true}, true)                    // 窗口内 → 小时点
	s := r.Snapshot(24, nil)
	if len(s.Series) != 2 || s.Series[0].Scope != "day" || s.Series[1].Scope != "hour" {
		t.Fatalf("series = %+v", s.Series)
	}
	if s.Series[0].PromptTokens != 5 || s.Series[1].PromptTokens != 3 {
		t.Fatalf("series tokens = %d/%d, want 5/3", s.Series[0].PromptTokens, s.Series[1].PromptTokens)
	}
}

// Stop 触发最终落盘（Start 后未到防抖间隔也要落）。
func TestLifecycleFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r := New(path)
	r.Start()
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 9, HasPromptTokens: true}, true)
	r.Stop()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stop 后应有落盘文件: %v", err)
	}
}

// series 与 by_model 按 realm 拆分：双域各自出点/出行，同天相邻。
// 前端「今日 token / 模型表」靠这个维度区分国际版与国内版数据。
func TestSnapshotRealmDimension(t *testing.T) {
	r := New("")
	now := time.Now()
	// cn 域今天 + 昨天；global 域仅今天。同一天两域各一个点。
	r.Add(now, "cn", "u1", "glm-5.2", Delta{PromptTokens: 10, HasPromptTokens: true}, true)
	r.Add(now.Add(-48*time.Hour), "cn", "u1", "glm-5.2", Delta{PromptTokens: 5, HasPromptTokens: true}, true)
	r.Add(now, "global", "u2", "glm-5.2", Delta{PromptTokens: 7, HasPromptTokens: true}, true)

	s := r.Snapshot(24, nil)
	// 24h 窗口：昨天的点被折叠为日点（1），今天的两个域各一个小时点（2）。
	if len(s.Series) != 3 {
		t.Fatalf("series = %d 个点, want 3（日点 + 双域小时点）", len(s.Series))
	}
	if s.Series[0].Realm != "cn" || s.Series[0].Scope != "day" {
		t.Fatalf("series[0] = %s/%s, want day/cn", s.Series[0].Scope, s.Series[0].Realm)
	}
	// 同一天的双域点相邻（realm 升序 cn < global），时间升序保持。
	if s.Series[1].Realm != "cn" || s.Series[2].Realm != "global" {
		t.Fatalf("双域小时点应相邻且按域升序: %s, %s", s.Series[1].Realm, s.Series[2].Realm)
	}
	if s.Series[1].PromptTokens != 10 || s.Series[2].PromptTokens != 7 {
		t.Fatalf("series tokens = %d/%d, want 10/7", s.Series[1].PromptTokens, s.Series[2].PromptTokens)
	}

	// by_model 按 (realm, model) 拆行：同裸名两行，Key 是裸名、Realm 单独标注。
	if len(s.ByModel) != 2 {
		t.Fatalf("by_model = %d 行, want 2（双域各一行）", len(s.ByModel))
	}
	if s.ByModel[0].Key != "glm-5.2" || s.ByModel[0].Realm == "" {
		t.Fatalf("by_model[0] = key=%q realm=%q, want 裸名 + realm 标注", s.ByModel[0].Key, s.ByModel[0].Realm)
	}
	if s.ByModel[0].Realm == s.ByModel[1].Realm {
		t.Fatalf("by_model 两行应分属不同 realm: %q/%q", s.ByModel[0].Realm, s.ByModel[1].Realm)
	}
}

// 面板时间筛选必须对 by_account/by_model 同样生效：切 24h/72h/60 天时两张表
// 的行与数值跟着窗口变；totals/by_realm 恒为全量累计（「累计请求」卡片口径）。
// 回归背景：曾经 by_account/by_model 聚合全部历史桶，切时间窗只有趋势图变化。
func TestSnapshotWindowFiltersBreakdowns(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "glm-5.2", Delta{PromptTokens: 30, HasPromptTokens: true}, true)                      // 窗口内
	r.Add(now.Add(-48*time.Hour), "cn", "u2", "claude-4.6", Delta{PromptTokens: 50, HasPromptTokens: true}, true) // 24h 窗口外

	s24 := r.Snapshot(24, nil)
	if len(s24.ByModel) != 1 || s24.ByModel[0].Key != "glm-5.2" || s24.ByModel[0].PromptTokens != 30 {
		t.Fatalf("24h by_model = %+v, want 仅 glm-5.2/30", s24.ByModel)
	}
	if len(s24.ByAccount) != 1 || s24.ByAccount[0].Key != "u1" || s24.ByAccount[0].PromptTokens != 30 {
		t.Fatalf("24h by_account = %+v, want 仅 u1/30", s24.ByAccount)
	}
	// 全量口径不受窗口影响。
	if s24.Totals.PromptTokens != 80 {
		t.Fatalf("24h totals = %d, want 80（全量累计）", s24.Totals.PromptTokens)
	}
	if len(s24.ByRealm) != 1 || s24.ByRealm[0].PromptTokens != 80 {
		t.Fatalf("24h by_realm = %+v, want 80", s24.ByRealm)
	}

	s72 := r.Snapshot(72, nil)
	if len(s72.ByModel) != 2 || len(s72.ByAccount) != 2 {
		t.Fatalf("72h by_model/by_account = %d/%d 行, want 各 2", len(s72.ByModel), len(s72.ByAccount))
	}

	// 日桶（Rollup 折叠产物）同样受窗口过滤：手工注入 40 天前的日桶。
	r.mu.Lock()
	day := time.Now().AddDate(0, 0, -40).Format(dayLayout)
	r.buckets["d:"+day+"|cn|u3|glm-5.2"] = &bucket{Scope: "d:" + day, Realm: "cn", UID: "u3", Model: "glm-5.2", Req: 1, PT: 90, TT: 90}
	r.mu.Unlock()

	s24b := r.Snapshot(24, nil)
	if s24b.Totals.PromptTokens != 170 {
		t.Fatalf("注入日桶后 totals = %d, want 170", s24b.Totals.PromptTokens)
	}
	for _, row := range s24b.ByAccount {
		if row.Key == "u3" {
			t.Fatalf("24h by_account 不应含 40 天前的日桶: %+v", s24b.ByAccount)
		}
	}
	found := false
	for _, row := range r.Snapshot(1440, nil).ByAccount { // 60 天窗口
		if row.Key == "u3" {
			found = true
		}
	}
	if !found {
		t.Fatal("60 天窗口 by_account 应含日桶 u3")
	}

	// 「启动以来」全量档（hours<=0）：排行恢复全量口径，含 40 天前日桶与 48h 前小时桶；
	// series 保留近 30 天小时粒度（48h 点仍是 hour），40 天前日桶仍是 day。
	sAll := r.Snapshot(0, nil)
	var u3Tokens, u2Tokens int64
	for _, row := range sAll.ByAccount {
		switch row.Key {
		case "u3":
			u3Tokens = row.PromptTokens
		case "u2":
			u2Tokens = row.PromptTokens
		}
	}
	if u3Tokens != 90 || u2Tokens != 50 {
		t.Fatalf("全量档 by_account 缺行: u3=%d u2=%d, want 90/50", u3Tokens, u2Tokens)
	}
	if sAll.Totals.PromptTokens != 170 {
		t.Fatalf("全量档 totals = %d, want 170", sAll.Totals.PromptTokens)
	}
	hourPts, dayPts := 0, 0
	for _, p := range sAll.Series {
		switch p.Scope {
		case "hour":
			hourPts++
		case "day":
			dayPts++
		}
	}
	if hourPts < 1 {
		t.Fatalf("全量档 series 应保留近 30 天小时点, got %+v", sAll.Series)
	}
	if dayPts < 1 {
		t.Fatalf("全量档 series 应含 30 天前的日点(40 天前日桶), got %+v", sAll.Series)
	}
}

// TestSnapshotEmptySeriesNotNull 零桶契约：刚启动无流量时 Snapshot.Series 必须是
// 空数组而非 nil——Go nil 切片序列化为 JSON null，面板 /stats/ 页 usage.series.filter()
// 直接 TypeError 白屏（2026-09-22 双击发行 exe 首启后打开 /stats/ 即崩的根因）。
func TestSnapshotEmptySeriesNotNull(t *testing.T) {
	for name, snap := range map[string]func() Snapshot{
		"empty-recorder": func() Snapshot { return New("").Snapshot(72, nil) },
		"nil-recorder":   func() Snapshot { return (*Recorder)(nil).Snapshot(72, nil) },
	} {
		s := snap()
		if s.Series == nil {
			t.Fatalf("%s: Series = nil, want 非 nil 空片", name)
		}
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("%s: unmarshal: %v", name, err)
		}
		if decoded["series"] == nil {
			t.Fatalf("%s: JSON series = null, 面板消费方会崩: %s", name, raw)
		}
	}
}

// TestFlushConcurrentNoTearing 并发 flush 撕裂写防护：面板 Save（flush(true)）与
// 后台 ticker（flush(false)）并发时共享同一 .tmp 路径，WriteFile/Rename 交叉后
// 落盘文件可能是半截内容。writeMu 串行化 IO 段后，任意时刻读回的文件必须是
// 合法 JSON（回归背景：撕裂写产物 load 失败 → 用量从零开始，违背「重启不丢」）。
func TestFlushConcurrentNoTearing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r := New(path)
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 1, HasPromptTokens: true, TotalTokens: 1, HasTotal: true}, true)
	r.dirty = true // 绕过防抖：保证并发 flush 全部执行 IO 段

	const workers = 2
	const rounds = 100
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				r.flush(true)
			}
		}()
	}
	wg.Wait()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回落盘文件: %v", err)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("并发 flush 后落盘文件非法 JSON（撕裂写）: %v\n前 120 字节: %q", err, raw[:min(len(raw), 120)])
	}
	if f.Version != 1 || len(f.Buckets) != 1 {
		t.Fatalf("落盘内容异常: version=%d buckets=%d", f.Version, len(f.Buckets))
	}
}

// TestRollupForcedWhenOverLimit 桶数超限强制折叠：hourlyKeep 口径只折叠 90 天前
// 的小时桶，近期桶数超限时「按期折叠」退化为空操作（内存无界）。Rollup 必须按
// 时间升序折叠最旧的未到期小时桶，把桶数压回上限*0.8 以内，且数据总量不丢。
// 生产上限 40 万构造代价过高，测试把 bucketLimit 调小走同一条强制折叠路径。
func TestRollupForcedWhenOverLimit(t *testing.T) {
	limit := 100
	oldLimit := bucketLimit
	bucketLimit = limit
	t.Cleanup(func() { bucketLimit = oldLimit })

	r := New("")
	now := time.Now()
	// limit+1 个近期（90 天内）小时桶，分布在不同小时（不同 Scope）。
	// 直接写桶表注入（Add 会归并进当前小时的单桶，造不出多桶）。
	r.mu.Lock()
	for i := 0; i <= limit; i++ {
		ts := now.Add(-time.Duration(i+1) * time.Hour).Format(hourLayout)
		key := "h:" + ts + "|cn|u|m"
		r.buckets[key] = &bucket{Scope: "h:" + ts, Realm: "cn", UID: "u", Model: "m", Req: 1, PT: 10, TT: 10}
	}
	r.mu.Unlock()

	r.Rollup(now)

	r.mu.Lock()
	n := len(r.buckets)
	r.mu.Unlock()
	if want := limit * 8 / 10; n > want {
		t.Fatalf("强制折叠后桶数 = %d, want ≤ %d（上限*0.8）", n, want)
	}
	// 折叠只改分片粒度不改总量：requests/tokens 守恒。
	s := r.Snapshot(0, nil)
	if s.Totals.Requests != int64(limit+1) || s.Totals.PromptTokens != int64(limit+1)*10 {
		t.Fatalf("折叠后 totals = %d/%d, want %d/%d（数据丢失）",
			s.Totals.Requests, s.Totals.PromptTokens, limit+1, (limit+1)*10)
	}
}
