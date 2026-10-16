package usage

import (
	"testing"
	"time"
)

// localHourBoundary 把任意时刻归一为本地整点（桶片起点，也是窗口边界）。
// 桶键按本地时区整点格式化，测试里的窗口边界必须与它严格重合，否则（半个时区
// 偏移等形态下）边界的「含 / 不含」会错位一档。
func localHourBoundary(t time.Time) time.Time {
	ts, err := time.ParseInLocation(hourLayout, t.Format(hourLayout), time.Local)
	if err != nil {
		panic(err)
	}
	return ts
}

// 窗口档小时桶归属：左闭右开——起点恰等于 from 的桶计入、恰等于 to 的排除；
// 窗外桶不进排行与时序，但 totals / by_realm 恒为全量（与 Snapshot 同口径）。
func TestSnapshotWindowHourBounds(t *testing.T) {
	r := New("")
	base := localHourBoundary(time.Now()).Add(-6 * time.Hour)
	from, to := base, base.Add(3*time.Hour)

	r.Add(base.Add(-time.Hour), "cn", "u1", "m", Delta{PromptTokens: 1, HasPromptTokens: true}, true) // 左边界之外
	r.Add(base, "cn", "u1", "m", Delta{PromptTokens: 2, HasPromptTokens: true}, true)                 // 左边界：含
	r.Add(base.Add(time.Hour), "cn", "u2", "m", Delta{PromptTokens: 4, HasPromptTokens: true}, true)  // 窗内
	r.Add(to, "cn", "u3", "m", Delta{PromptTokens: 8, HasPromptTokens: true}, true)                   // 右边界：不含

	s := r.SnapshotWindow(from, to, nil)
	if len(s.Series) != 2 {
		t.Fatalf("series = %+v, want 2 个窗口内小时点", s.Series)
	}
	if s.Series[0].T != from.Format(hourLayout) || s.Series[0].PromptTokens != 2 {
		t.Fatalf("series[0] = %+v, want 左边界桶 %s/2", s.Series[0], from.Format(hourLayout))
	}
	if s.Series[1].T != from.Add(time.Hour).Format(hourLayout) || s.Series[1].PromptTokens != 4 {
		t.Fatalf("series[1] = %+v, want 窗内桶/4", s.Series[1])
	}
	// 排行只含窗内两行：u1（左边界桶 2；更早那个桶在窗左外）、u2（窗内 4）；
	// 右边界桶 u3 排除。
	got := map[string]int64{}
	for _, row := range s.ByAccount {
		got[row.Key] = row.PromptTokens
	}
	if len(got) != 2 || got["u1"] != 2 || got["u2"] != 4 {
		t.Fatalf("by_account = %+v, want u1=2/u2=4（窗外与右边界桶不入排行）", s.ByAccount)
	}
	if len(s.ByModel) != 1 || s.ByModel[0].PromptTokens != 6 {
		t.Fatalf("by_model = %+v, want 单模型 6", s.ByModel)
	}
	// 累计口径不随窗口变。
	if s.Totals.PromptTokens != 15 {
		t.Fatalf("totals = %d, want 15（全量累计）", s.Totals.PromptTokens)
	}
	if len(s.ByRealm) != 1 || s.ByRealm[0].PromptTokens != 15 {
		t.Fatalf("by_realm = %+v, want 全量 15", s.ByRealm)
	}
	// 生效窗口回显（unix 秒，与入参同形）。
	if s.WindowFrom != from.Unix() || s.WindowTo != to.Unix() {
		t.Fatalf("窗口回显 = %d/%d, want %d/%d", s.WindowFrom, s.WindowTo, from.Unix(), to.Unix())
	}
}

// 窗口档日桶：窗口可以落在 90 天折叠线之前（那段历史只剩日桶），窗口内的日桶
// 必须画成日点；边界同样左闭右开；窗外日桶不进任何视图（但不影响 totals）。
func TestSnapshotWindowDayBuckets(t *testing.T) {
	r := New("")
	day := time.Now().AddDate(0, 0, -40)
	dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.Local)
	r.mu.Lock()
	r.buckets["d:"+dayStart.Format(dayLayout)+"|cn|u3|m3"] = &bucket{
		Scope: "d:" + dayStart.Format(dayLayout), Realm: "cn", UID: "u3", Model: "m3",
		Req: 1, PT: 90, TT: 90,
	}
	r.mu.Unlock()

	in := r.SnapshotWindow(dayStart, dayStart.Add(24*time.Hour), nil)
	if len(in.Series) != 1 || in.Series[0].Scope != "day" || in.Series[0].PromptTokens != 90 {
		t.Fatalf("窗口档日桶未按日点上图: %+v", in.Series)
	}
	if len(in.ByAccount) != 1 || in.ByAccount[0].Key != "u3" {
		t.Fatalf("窗口档日桶未进排行: %+v", in.ByAccount)
	}
	// 左边界：from == 日桶起点 → 计入。
	if left := r.SnapshotWindow(dayStart, dayStart.Add(time.Hour), nil); len(left.Series) != 1 {
		t.Fatalf("from == 日桶起点应计入: %+v", left.Series)
	}
	// 右边界：to == 日桶起点 → 排除（左闭右开）。
	if right := r.SnapshotWindow(dayStart.Add(-24*time.Hour), dayStart, nil); len(right.Series) != 0 {
		t.Fatalf("to == 日桶起点应排除: %+v", right.Series)
	}
	// 窗口整体落在当日之后：日桶既不进时序也不进排行，totals 仍是全量。
	after := r.SnapshotWindow(dayStart.Add(24*time.Hour), dayStart.Add(48*time.Hour), nil)
	if len(after.Series) != 0 || len(after.ByModel) != 0 || len(after.ByAccount) != 0 {
		t.Fatalf("窗外日桶不应进入任何视图: series=%+v by_model=%+v", after.Series, after.ByModel)
	}
	if after.Totals.PromptTokens != 90 {
		t.Fatalf("totals 恒全量: %d want 90", after.Totals.PromptTokens)
	}
}

// 窗口档不折叠窗口内的小时桶：30~90 天前的数据只有小时桶（Rollup 只折 90 天前的），
// 折叠成日点会把用户显式指定的范围糊掉——这里验证 40 天前的小时桶保持小时粒度
// 上图；同一份数据走全量档则按既有规则并回日点（两档差别只在图示粒度）。
func TestSnapshotWindowKeepsHourGranularity(t *testing.T) {
	r := New("")
	old := localHourBoundary(time.Now().AddDate(0, 0, -40))
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)

	s := r.SnapshotWindow(old.Add(-time.Hour), old.Add(time.Hour), nil)
	if len(s.Series) != 1 || s.Series[0].Scope != "hour" || s.Series[0].T != old.Format(hourLayout) {
		t.Fatalf("40 天前的小时桶应保持小时粒度: %+v", s.Series)
	}
	if s.Series[0].PromptTokens != 7 {
		t.Fatalf("窗口档小时点 tokens = %d, want 7", s.Series[0].PromptTokens)
	}
	all := r.Snapshot(0, nil)
	if len(all.Series) != 1 || all.Series[0].Scope != "day" || all.Series[0].PromptTokens != 7 {
		t.Fatalf("全量档应把 30 天外的小时桶并回日点: %+v", all.Series)
	}
}

// 空窗口 / 反序窗口 / nil 记录器：排行与时序为空但 Series 必须是空数组而非 nil
// （前端契约：数组字段恒为数组，nil 序列化成 null 会让面板 .filter 直接 TypeError），
// totals 仍为全量，窗口照常回显。
func TestSnapshotWindowEmptyRange(t *testing.T) {
	r := New("")
	now := localHourBoundary(time.Now())
	r.Add(now, "cn", "u", "m", Delta{PromptTokens: 5, HasPromptTokens: true}, true)

	empty := r.SnapshotWindow(now, now, nil) // from == to：左闭右开 → 无桶
	if empty.Series == nil || len(empty.Series) != 0 {
		t.Fatalf("空窗口 series = %+v, want 非 nil 空片", empty.Series)
	}
	if len(empty.ByModel) != 0 || len(empty.ByAccount) != 0 {
		t.Fatalf("空窗口排行应为空: by_model=%+v by_account=%+v", empty.ByModel, empty.ByAccount)
	}
	if empty.Totals.PromptTokens != 5 {
		t.Fatalf("空窗口 totals 仍全量: %d want 5", empty.Totals.PromptTokens)
	}
	if empty.WindowFrom != now.Unix() || empty.WindowTo != now.Unix() {
		t.Fatalf("空窗口回显 = %d/%d", empty.WindowFrom, empty.WindowTo)
	}
	// 反序窗口（from > to）同样为空——不静默交换两端（交换会把「用户填反了」
	// 变成「看起来正常但其实看了另一段数据」）。
	rev := r.SnapshotWindow(now.Add(time.Hour), now, nil)
	if len(rev.Series) != 0 || rev.Totals.PromptTokens != 5 {
		t.Fatalf("反序窗口应空且 totals 全量: series=%+v totals=%d", rev.Series, rev.Totals.PromptTokens)
	}
	// hours 档（既有 Snapshot）不回显窗口：0/0 = 未使用显式窗口。
	if h := r.Snapshot(24, nil); h.WindowFrom != 0 || h.WindowTo != 0 {
		t.Fatalf("hours 档不应回显窗口: %d/%d", h.WindowFrom, h.WindowTo)
	}
	// nil 记录器：空片 + 窗口回显（防御性形态，见 Snapshot 的 nil 契约）。
	ns := (*Recorder)(nil).SnapshotWindow(now, now.Add(time.Hour), nil)
	if ns.Series == nil || ns.WindowFrom != now.Unix() {
		t.Fatalf("nil 记录器窗口档 = %+v", ns)
	}
}
