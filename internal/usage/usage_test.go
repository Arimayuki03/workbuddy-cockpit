package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
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
