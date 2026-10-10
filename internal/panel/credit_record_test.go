// credit_record_test.go — CreditTracker 记账口径单测（实现所在包内白盒驱动）：
// 基线（首见不记）/ 去重（同余额重复刷新不重复记）/ 跳变（超上限不带加号语义）/
// 减少不记 / 持久化往返（快照跨重启不重报历史余额）/ HTTP 端点（200 与 501）。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/upstream"
)

// newMemoryTracker 纯内存形态（不落盘、无后台 goroutine，测试确定性）。
func newMemoryTracker() *CreditTracker {
	return &CreditTracker{snap: scheduler.CreditSnapshots{}}
}

// TestCreditTrackerBaseline 首次见到账号只建基线、不记流水（否则历史余额被
// 误报成「刚获得」，wbm record_balance 同口径）。
func TestCreditTrackerBaseline(t *testing.T) {
	tr := newMemoryTracker()
	if n := tr.RecordBalance("u1", "猫猫", 1300, time.Now()); n != 0 {
		t.Fatalf("首见应只建基线，实得 %d 条", n)
	}
	if len(tr.Records("")) != 0 {
		t.Fatalf("基线不应产出记录：%+v", tr.Records(""))
	}
	// 快照已落：重启语义等价（再次同余额 → 去重，无记录）。
	if n := tr.RecordBalance("u1", "猫猫", 1300, time.Now()); n != 0 {
		t.Fatalf("同余额重复刷新应去重，实得 %d 条", n)
	}
}

// TestCreditTrackerDedupSameBalance 同余额重复刷新（定时 + 手动撞同一时点）
// 只留一条流水：去重键 = 变动后余额。
func TestCreditTrackerDedupSameBalance(t *testing.T) {
	tr := newMemoryTracker()
	tr.RecordBalance("u1", "猫猫", 1300, time.Now()) // 基线
	if n := tr.RecordBalance("u1", "猫猫", 1400, time.Now()); n != 1 {
		t.Fatalf("增加应记 1 条，实得 %d", n)
	}
	// 同一余额（1400）反复刷新：不再新增。
	if n := tr.RecordBalance("u1", "猫猫", 1400, time.Now().Add(time.Minute)); n != 0 {
		t.Fatalf("同余额重复刷新应去重，实得 %d", n)
	}
	// 经过一次回落再回到 1400：dedup key 已存在，仍不重复记
	// （回落是消耗，不记；回升到已见过的余额不重复报「刚获得」）。
	if n := tr.RecordBalance("u1", "猫猫", 1350, time.Now()); n != 0 {
		t.Fatalf("减少不应记：%d", n)
	}
	if n := tr.RecordBalance("u1", "猫猫", 1400, time.Now()); n != 0 {
		t.Fatalf("回升到已记账余额不应重复记：%d", n)
	}
	recs := tr.Records("")
	if len(recs) != 1 || recs[0].Delta != 100 || recs[0].Prev != 1300 || recs[0].New != 1400 {
		t.Fatalf("流水内容错误：%+v", recs)
	}
	if recs[0].Kind != "credit" || recs[0].DedupKey != "credit|u1|1400" {
		t.Fatalf("kind/dedup_key 错误：%+v", recs[0])
	}
	if !strings.Contains(recs[0].Message, "余额 +100（1300 → 1400）") {
		t.Fatalf("文案错误：%q", recs[0].Message)
	}
}

// TestCreditTrackerJump 单条增量超过 CreditMaxDelta：记 jump（Delta=0，不带
// 加号语义），文案不含「+」。
func TestCreditTrackerJump(t *testing.T) {
	tr := newMemoryTracker()
	tr.RecordBalance("u1", "猫猫", 1300, time.Now())
	if n := tr.RecordBalance("u1", "猫猫", 1300+scheduler.CreditMaxDelta+1, time.Now()); n != 1 {
		t.Fatalf("超上限应记 jump 一条，实得 %d", n)
	}
	recs := tr.Records("")
	rec := recs[0]
	if !rec.Jump || rec.Delta != 0 {
		t.Fatalf("jump 记录字段错误：%+v", rec)
	}
	if strings.Contains(rec.Message, "余额 +") {
		t.Fatalf("跳变文案不应带加号语义：%q", rec.Message)
	}
	if !strings.Contains(rec.Message, "余额跳变 1300 →") {
		t.Fatalf("跳变文案缺「余额跳变 A → B」：%q", rec.Message)
	}
	// 同一跳变余额重复刷新：同样去重。
	if n := tr.RecordBalance("u1", "猫猫", 1300+scheduler.CreditMaxDelta+1, time.Now()); n != 0 {
		t.Fatalf("跳变余额重复刷新应去重：%d", n)
	}
}

// TestCreditTrackerDecreaseIgnored 减少与持平不记（流水只记「赚到」）。
func TestCreditTrackerDecreaseIgnored(t *testing.T) {
	tr := newMemoryTracker()
	tr.RecordBalance("u1", "", 1400, time.Now())
	if n := tr.RecordBalance("u1", "", 1300, time.Now()); n != 0 {
		t.Fatalf("减少不应记：%d", n)
	}
	if n := tr.RecordBalance("u1", "", 1300, time.Now()); n != 0 {
		t.Fatalf("持平不应记：%d", n)
	}
	if len(tr.Records("")) != 0 {
		t.Fatalf("不应有任何记录：%+v", tr.Records(""))
	}
}

// TestCreditTrackerPersistRoundTrip 快照 + 记录落盘 → 重建 tracker 往返：
// 快照恢复后同余额不再重报（重启不会把历史余额误报一遍），记录留存。
func TestCreditTrackerPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	snapFp, recFp := dir+"/credit_snapshots.json", dir+"/credit_records.json"

	tr := NewCreditTracker(snapFp, recFp)
	tr.RecordBalance("u1", "猫猫", 1300, time.Now()) // 基线
	tr.RecordBalance("u1", "猫猫", 1400, time.Now()) // +100
	tr.SaveNow()
	tr.Stop()

	// 重建：快照读回 1400。
	tr2 := NewCreditTracker(snapFp, recFp)
	defer tr2.Stop()
	if n := tr2.RecordBalance("u1", "猫猫", 1400, time.Now()); n != 0 {
		t.Fatalf("重启后同余额不应重报：%d", n)
	}
	recs := tr2.Records("")
	if len(recs) != 1 || recs[0].Delta != 100 {
		t.Fatalf("记录未恢复：%+v", recs)
	}
}

// TestCreditTrackerSnapshotFileFormat 快照文件形状（uid → int64 的 JSON 对象）。
func TestCreditTrackerSnapshotFileFormat(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/credit_snapshots.json"
	tr := newMemoryTracker()
	tr.snap["u1"] = 1400
	if err := scheduler.SaveCreditSnapshots(fp, tr.snap); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc map[string]int64
	if err := json.Unmarshal(raw, &doc); err != nil || doc["u1"] != 1400 {
		t.Fatalf("文件非 uid→int64 形状：%v (err=%v)", doc, err)
	}
	// LoadCreditSnapshots 原语往返。
	snap := scheduler.LoadCreditSnapshots(fp)
	if snap["u1"] != 1400 {
		t.Fatalf("LoadCreditSnapshots 往返错误：%v", snap)
	}
}

// TestCreditTrackerCheckinRecord 签到到账口径：before/after 差值 >0 记
// source=checkin 流水（文案「签到到账 +N」）；差值 <=0（已签无奖励/奖励被
// 同期消耗吃掉）只刷基线不记；签到后基线落快照，后续常规刷新不再重复报。
func TestCreditTrackerCheckinRecord(t *testing.T) {
	tr := newMemoryTracker()
	now := time.Now()
	// 签到前 100 → 签到后 120：记一条 +20。
	if n := tr.RecordCheckin("u1", "猫猫", 100, 120, now); n != 1 {
		t.Fatalf("签到到账应记 1 条，实得 %d", n)
	}
	recs := tr.Records("")
	if len(recs) != 1 || recs[0].Delta != 20 || recs[0].Prev != 100 || recs[0].New != 120 {
		t.Fatalf("签到流水内容错误：%+v", recs)
	}
	if recs[0].Source != "checkin" || recs[0].Kind != "credit" {
		t.Fatalf("source/kind 错误：%+v", recs[0])
	}
	if recs[0].DedupKey != "credit|u1|120" {
		t.Fatalf("dedup_key 错误：%+v", recs[0])
	}
	if !strings.Contains(recs[0].Message, "签到到账 +20（100 → 120）") {
		t.Fatalf("文案错误：%q", recs[0].Message)
	}
	// 同余额重复签到（幂等已签）：去重，不重复记。
	if n := tr.RecordCheckin("u1", "猫猫", 120, 120, now.Add(time.Minute)); n != 0 {
		t.Fatalf("同余额重复签到应去重：%d", n)
	}
	// 奖励被消耗抵消（before 120 → after 110）：只刷基线，不记负数流水。
	if n := tr.RecordCheckin("u1", "猫猫", 120, 110, now.Add(2*time.Minute)); n != 0 {
		t.Fatalf("净减不应记：%d", n)
	}
	// 签到后基线已落快照：常规刷新同余额不重复报。
	if n := tr.RecordBalance("u1", "猫猫", 110, now.Add(3*time.Minute)); n != 0 {
		t.Fatalf("常规刷新同余额应去重：%d", n)
	}
	if len(tr.Records("")) != 1 {
		t.Fatalf("记录总数应仍为 1：%+v", tr.Records(""))
	}
}

// TestCreditTrackerCheckinBaselineRefresh 签到把快照基线推进到 after：后续
// 消耗回落再回升到同一 after 值（常规 RecordBalance）不重复报「刚获得」。
func TestCreditTrackerCheckinBaselineRefresh(t *testing.T) {
	tr := newMemoryTracker()
	now := time.Now()
	tr.RecordCheckin("u1", "猫猫", 100, 120, now) // 签到 +20，基线 → 120
	// 消耗到 90（不记），随后旅行领奖回升到 120（常规路径）——120 的 dedup key
	// 已被签到记录占用，不重复记；这正是「基线推进」想要的防双计语义。
	if n := tr.RecordBalance("u1", "猫猫", 90, now.Add(time.Minute)); n != 0 {
		t.Fatalf("消耗不应记：%d", n)
	}
	if n := tr.RecordBalance("u1", "猫猫", 120, now.Add(2*time.Minute)); n != 0 {
		t.Fatalf("回升到已记账余额不应重复记：%d", n)
	}
	if len(tr.Records("")) != 1 {
		t.Fatalf("记录总数应仍为 1：%+v", tr.Records(""))
	}
}

// TestTasksRecordsEndpoint /api/tasks/records：有 tracker 返回 200 + records；
// 无 tracker 返回 501（前端按「未启用」展示）。uid 参数过滤生效。
func TestTasksRecordsEndpoint(t *testing.T) {
	tr := newMemoryTracker()
	tr.RecordBalance("u1", "猫猫", 1300, time.Now())
	tr.RecordBalance("u1", "猫猫", 1400, time.Now())
	tr.RecordBalance("u2", "汪汪", 100, time.Now())
	tr.RecordBalance("u2", "汪汪", 300, time.Now())

	p := New(Config{Version: "test", APIKey: "k", Pool: newSmokePool(), Credits: tr})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/tasks/records", nil)
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK      bool `json:"ok"`
		Total   int  `json:"total"`
		Records []struct {
			UID   string `json:"uid"`
			Kind  string `json:"kind"`
			Delta int64  `json:"delta"`
		} `json:"records"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Total != 2 || len(resp.Records) != 2 {
		t.Fatalf("全量记录异常：total=%d %+v", resp.Total, resp.Records)
	}
	// uid 过滤。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/tasks/records?uid=u2", nil)
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Total != 1 || len(resp.Records) != 1 || resp.Records[0].UID != "u2" {
		t.Fatalf("uid 过滤异常：%+v", resp.Records)
	}

	// 无 tracker → 501。
	p2 := New(Config{Version: "test", APIKey: "k", Pool: newSmokePool()})
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/api/tasks/records", nil)
	req2.Header.Set("Authorization", "Bearer k")
	p2.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotImplemented {
		t.Fatalf("无 tracker 应 501，实得 %d", rec2.Code)
	}
}

// TestRenewAllEndpoint /api/renew_all 路由连通（Scheduler 缺失 → 501 而非 404）。
func TestRenewAllEndpoint(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", Pool: newSmokePool(),
		Upstream: &upstream.Client{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/renew_all", nil)
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("Scheduler 缺失应 501，实得 %d body=%s", rec.Code, rec.Body.String())
	}
}
