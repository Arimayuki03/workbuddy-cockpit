package reqlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSnapshotMetrics 成功率 / TTFB 平均与最大 / 最近 N 条 / 失败计数。
func TestSnapshotMetrics(t *testing.T) {
	r := New(Config{})
	// 4 条：3 成功（TTFB 10/30/50ms），1 失败（TTFB 0 = 缺观测）。
	for i, e := range []Event{
		{Model: "cn:glm-5.2", Status: 200, OK: true, Tokens: 10, TTFBMS: 10},
		{Model: "cn:glm-5.2", Status: 200, OK: true, Tokens: 20, TTFBMS: 30},
		{Model: "cn:glm-5.2", Status: 503, OK: false, Tokens: -1, Error: "rate limited"},
		{Model: "cn:glm-5.2", Status: 200, OK: true, Tokens: 30, TTFBMS: 50},
	} {
		e.Time = time.Now().Add(time.Duration(i) * time.Second)
		r.Record(e)
	}
	s := r.Snapshot()
	if s.Completed != 4 || s.Succeeded != 3 || s.Failed != 1 {
		t.Fatalf("counts = %+v", s)
	}
	if s.SuccessRate != 75 {
		t.Errorf("success_rate=%v want 75", s.SuccessRate)
	}
	// TTFB 只统计 >0 的观测：avg=(10+30+50)/3=30，max=50，obs=3。
	if s.AvgTTFBMS != 30 || s.MaxTTFBMS != 50 || s.TTFBObs != 3 {
		t.Errorf("ttfb = avg:%v max:%v obs:%v want 30/50/3", s.AvgTTFBMS, s.MaxTTFBMS, s.TTFBObs)
	}
	// Recent 新→旧：最后写入（TTFB=50）在前，最早的（TTFB=10）在后。
	if len(s.Recent) != 4 || s.Recent[0].TTFBMS != 50 || s.Recent[3].TTFBMS != 10 {
		t.Errorf("recent order wrong: %+v", s.Recent)
	}
	// Snapshot 返回副本：改它不影响内部状态。
	s.Recent[0].Tokens = 999
	if r.Snapshot().Recent[0].Tokens == 999 {
		t.Error("Snapshot.Recent 不是副本")
	}
}

// TestRecentCapEvictsOldest 内存「最近 N 条」封顶淘汰最旧。
func TestRecentCapEvictsOldest(t *testing.T) {
	r := New(Config{})
	for i := 0; i < recentCap+10; i++ {
		r.Record(Event{Status: 200, OK: true, Tokens: i})
	}
	recent := r.Snapshot().Recent
	if len(recent) != recentCap {
		t.Fatalf("recent len=%d want %d", len(recent), recentCap)
	}
	// 新→旧：头部是最后写入（109），尾部是最早存活（10）。
	if recent[0].Tokens != recentCap+9 || recent[recentCap-1].Tokens != 10 {
		t.Errorf("evict wrong: head=%d tail=%d want %d/10", recent[0].Tokens, recent[recentCap-1].Tokens, recentCap+9)
	}
}

// TestNilReceiverSafety nil 接收者全方法安全（零值形态用于最小装配/测试）。
func TestNilReceiverSafety(t *testing.T) {
	var r *Recorder
	r.Record(Event{Status: 200, OK: true}) // 不 panic
	s := r.Snapshot()
	if s.Completed != 0 {
		t.Errorf("nil snapshot = %+v", s)
	}
	rows, err := r.ReadArchive(10, nil)
	if rows != nil || err != nil {
		t.Errorf("nil read = %v, %v", rows, err)
	}
	r.Close() // 不 panic
}

// TestArchiveDailyRotation 跨天轮转：事件时间跨两天 → 两个文件，按天归位。
func TestArchiveDailyRotation(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir})
	base := time.Now().Truncate(24 * time.Hour).Add(-24 * time.Hour) // 昨天 00:00
	events := []Event{
		{Model: "cn:glm-5.2", Status: 200, OK: true, Time: base.Add(time.Hour)},
		{Model: "cn:glm-5.2", Status: 200, OK: true, Time: base.Add(2 * time.Hour)},
		{Model: "cn:glm-5.2", Status: 200, OK: true, Time: base.Add(25 * time.Hour)}, // 今天
	}
	for _, e := range events {
		r.Record(e)
	}
	r.Close()

	day1 := base.Format(dayFormat)
	day2 := base.Add(24 * time.Hour).Format(dayFormat)
	for _, day := range []string{day1, day2} {
		raw, err := os.ReadFile(filepath.Join(dir, "requests-"+day+".jsonl"))
		if err != nil {
			t.Fatalf("read %s: %v", day, err)
		}
		lines := strings.Count(strings.TrimSpace(string(raw)), "\n") + 1
		want := 2
		if day == day2 {
			want = 1
		}
		if lines != want {
			t.Errorf("day %s lines=%d want %d", day, lines, want)
		}
		// 每行都是合法 JSON 事件（无半截行）。
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			var e Event
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				t.Errorf("bad jsonl line: %v (%s)", err, line)
			}
		}
	}
}

// TestArchiveFileSharding 单文件超限分片：同一天内切 .1.jsonl 分片，不覆盖旧数据。
func TestArchiveFileSharding(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir, FileMaxBytes: 200})
	now := time.Now()
	for i := 0; i < 8; i++ {
		r.Record(Event{Model: "cn:glm-5.2", Status: 200, OK: true, Time: now, Tokens: i})
	}
	r.Close()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("sharding 未发生：files=%d", len(entries))
	}
	names := make(map[string]bool, len(entries))
	for _, e := range entries {
		names[e.Name()] = true
	}
	if !names["requests-"+now.Format(dayFormat)+".jsonl"] || !names["requests-"+now.Format(dayFormat)+".1.jsonl"] {
		t.Errorf("分片命名不对: %v", names)
	}
}

// TestArchiveRestartAppendsSameDay 重启（重建 writer）后续写当天既有分片，不另开新文件。
func TestArchiveRestartAppendsSameDay(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	r1 := New(Config{Dir: dir})
	r1.Record(Event{Model: "m1", Status: 200, OK: true, Time: now})
	r1.Close()

	r2 := New(Config{Dir: dir})
	r2.Record(Event{Model: "m2", Status: 200, OK: true, Time: now.Add(time.Second)})
	r2.Close()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("重启后应续写同一文件，实际 %d 个: %v", len(entries), entries)
	}
	rows, err := r2.ReadArchive(10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("回读 %d 条 want 2", len(rows))
	}
	// 倒序：m2（时间更新）在前。
	if rows[0].Model != "m2" || rows[1].Model != "m1" {
		t.Errorf("order = %s,%s want m2,m1", rows[0].Model, rows[1].Model)
	}
}

// newBareWriter 手工构建不跑后台 goroutine 的 writer（prune/丢弃计数等单元测试用，
// 避免与真实后台 goroutine 竞争内部句柄；对齐参考实现测试的构建方式）。
func newBareWriter(dir string, cfg Config) *archiveWriter {
	cfg.Dir = dir
	w := &archiveWriter{
		cfg:  cfg,
		ch:   make(chan Event, 1),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	return w
}

// TestRetentionDeletesOldestByDay 保留上限（按天）：过期文件被删，窗口内保留。
// prune 依据文件名内嵌日期判旧（不依赖 mtime）。
func TestRetentionDeletesOldestByDay(t *testing.T) {
	dir := t.TempDir()
	today := time.Now()
	for i := 9; i >= 0; i-- {
		day := today.AddDate(0, 0, -i).Format(dayFormat)
		path := filepath.Join(dir, "requests-"+day+".jsonl")
		if err := os.WriteFile(path, []byte(`{"time":"2026-01-01T00:00:00Z","status":200}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w := newBareWriter(dir, Config{RetentionDays: 7})
	w.prune()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 7 {
		t.Errorf("剩余文件 %d 个 want 7: %v", len(entries), entries)
	}
	keepFrom := today.AddDate(0, 0, -6).Format(dayFormat)
	for _, e := range entries {
		day := strings.TrimSuffix(strings.TrimPrefix(e.Name(), "requests-"), ".jsonl")
		if day < keepFrom {
			t.Errorf("过期文件未删: %s (窗口起点 %s)", e.Name(), keepFrom)
		}
	}
}

// TestRetentionHonorsCapacityMB 容量上限：总字节超限删最旧（与天数无关）。
func TestRetentionHonorsCapacityMB(t *testing.T) {
	dir := t.TempDir()
	today := time.Now().Format(dayFormat)
	// 3 个文件各 40 字节；RetentionDays 极大（天数不触发），MaxBytes=70：
	// 120 → 删 20260101 → 80 仍超 → 删 20260102 → 40 ≤ 70 停。留今天。
	names := []string{"requests-20260101.jsonl", "requests-20260102.jsonl", "requests-" + today + ".jsonl"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Repeat("x", 40)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w := newBareWriter(dir, Config{RetentionDays: 3000, MaxBytes: 70})
	w.prune()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != names[2] {
		t.Errorf("剩余 = %v want [%s]", entries, names[2])
	}
}

// TestPruneKeepsOpenFile 打开中的文件不被 prune 删除（正在追加写）。
func TestPruneKeepsOpenFile(t *testing.T) {
	dir := t.TempDir()
	open := filepath.Join(dir, "requests-"+time.Now().Format(dayFormat)+".jsonl")
	if err := os.WriteFile(open, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := newBareWriter(dir, Config{MaxBytes: 1}) // 容量 1 字节：任何文件都"超限"
	w.path = open
	w.prune()
	if _, err := os.Stat(open); err != nil {
		t.Errorf("打开中的文件被 prune 删除: %v", err)
	}
}

// TestAsyncDropCounter 队列满载丢弃并计数，且不阻塞投递方。
func TestAsyncDropCounter(t *testing.T) {
	dir := t.TempDir()
	w := newBareWriter(dir, Config{})
	w.ch <- Event{Model: "occupied"} // 手工灌满（无后台 goroutine 消费）
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			w.enqueue(Event{Model: "drop", Status: 200, OK: true})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("enqueue 被阻塞：队列满时必须丢弃而非等待")
	}
	if got := w.dropped.Load(); got != 100 {
		t.Errorf("dropped=%d want 100", got)
	}
}

// TestReadArchiveOrderAndFilter 回读按事件时间倒序 + filter 各维度过滤，
// 不依赖文件 mtime（mtime 与事件时间刻意相反）。
func TestReadArchiveOrderAndFilter(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir, FileMaxBytes: 120}) // 极小分片：事件散落多个文件
	base := time.Now().Add(-time.Minute)
	const n = 8
	for i := 0; i < n; i++ {
		r.Record(Event{
			Time:    base.Add(time.Duration(i) * time.Second),
			Model:   "cn:glm-5.2",
			Status:  200,
			OK:      true,
			Tokens:  i,
			Account: "号甲(uid0001)",
		})
	}
	// 混入一条时间最新的不匹配事件：不同模型 + 500。
	r.Record(Event{Time: base.Add(time.Duration(n) * time.Second), Model: "other", Status: 500, OK: false, Tokens: -1})
	r.Close()

	// 把文件 mtime 按字典序递增设置：装最早事件的基准文件字典序最大、mtime 也
	// 最大——若实现依赖 mtime 排序就会把最早事件排最前。本用例钉死「只按事件
	// 时间排序」（目录拷贝/备份恢复后 mtime 不可信，同一秒轮转的文件 mtime 相同）。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1700000000, 0)
	for i, e := range entries {
		ts := stamp.Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(filepath.Join(dir, e.Name()), ts, ts); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := r.ReadArchive(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != n+1 {
		t.Fatalf("rows=%d want %d", len(rows), n+1)
	}
	// 倒序：时间最新的 other 在前，其后是 tokens=n-1 ... 0。
	if rows[0].Model != "other" || rows[1].Tokens != n-1 || rows[n].Tokens != 0 {
		t.Errorf("order wrong: head=%+v", rows[0])
	}

	// limit 截断（截的是倒序后的头部）。
	rows, err = r.ReadArchive(3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].Tokens != -1 || rows[1].Tokens != n-1 {
		t.Errorf("limit rows=%d tokens=%d,%d want 3 / -1,%d", len(rows), rows[0].Tokens, rows[1].Tokens, n-1)
	}

	// model 前缀过滤（包含匹配）。
	rows, err = r.ReadArchive(0, &Filter{Model: "glm"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != n || rows[0].Tokens != n-1 {
		t.Errorf("model filter rows=%d", len(rows))
	}

	// account 片段过滤。
	rows, err = r.ReadArchive(0, &Filter{Account: "号甲"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != n {
		t.Errorf("account filter rows=%d", len(rows))
	}

	// 状态码精确过滤。
	rows, err = r.ReadArchive(0, &Filter{Status: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Model != "other" {
		t.Errorf("status filter = %+v", rows)
	}

	// 时间区间（闭区间）：只取第 3~5 秒的三条。
	rows, err = r.ReadArchive(0, &Filter{From: base.Add(3 * time.Second), To: base.Add(5 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].Tokens != 5 || rows[2].Tokens != 3 {
		t.Errorf("time range = %+v", rows)
	}
}

// TestReadArchiveAcrossDays 跨天文件回读：时间顺序跨文件还原（昨天的排后面）。
func TestReadArchiveAcrossDays(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir})
	base := time.Now().Truncate(24 * time.Hour)
	for _, ts := range []time.Time{
		base.Add(-25 * time.Hour), // 前天
		base.Add(-time.Hour),      // 昨天
		base.Add(time.Hour),       // 今天
	} {
		r.Record(Event{Time: ts, Model: "cn:glm-5.2", Status: 200, OK: true})
	}
	r.Close()
	rows, err := r.ReadArchive(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows=%d", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].Time.Before(rows[i].Time) {
			t.Errorf("rows[%d] 时间乱序: %v -> %v", i, rows[i-1].Time, rows[i].Time)
		}
	}
}

// TestCloseFlushCompleteness Close 后队列全部落盘（尾部不丢），且无半截行。
func TestCloseFlushCompleteness(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir})
	const n = 500
	for i := 0; i < n; i++ {
		r.Record(Event{Model: "cn:glm-5.2", Status: 200, OK: true, Tokens: i})
	}
	r.Close() // 排空 + flush + 关文件

	rows, err := r.ReadArchive(maxReadLimit, nil) // 上限内一次取全
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != n {
		t.Fatalf("回读 %d 条 want %d（Close 必须排空队列）", len(rows), n)
	}
	// 逐文件数行：全部是合法 JSON（无半截写入）。
	entries, _ := os.ReadDir(dir)
	total := 0
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line == "" {
				continue
			}
			var ev Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				head := line
				if len(head) > 40 {
					head = head[:40]
				}
				t.Errorf("半截/损坏行: %v (%s...)", err, head)
			}
			total++
		}
	}
	if total != n {
		t.Errorf("文件总行数 %d want %d", total, n)
	}
	// 幂等：重复 Close 不 panic。
	r.Close()
}

// TestConcurrentRecordRace 并发 Record（-race 压测）：队列投递/快照/回读无竞争。
func TestConcurrentRecordRace(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				r.Record(Event{
					Model:  fmt.Sprintf("cn:glm-%d", g),
					Status: 200, OK: true, Tokens: i, TTFBMS: int64(i % 7),
				})
			}
		}(g)
	}
	// 同时读快照（与写并发）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = r.Snapshot()
		}
	}()
	wg.Wait()
	r.Close()
	rows, err := r.ReadArchive(maxReadLimit, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 8*50 {
		t.Errorf("落盘 %d 条 want %d", len(rows), 8*50)
	}
	if s := r.Snapshot(); s.Completed != 8*50 {
		t.Errorf("completed=%d want %d", s.Completed, 8*50)
	}
}

// TestArchiveDisabledWhenDirEmpty Dir 为空 = 归档关闭：Record 零落盘、stats 报未启用。
func TestArchiveDisabledWhenDirEmpty(t *testing.T) {
	r := New(Config{})
	r.Record(Event{Model: "m", Status: 200, OK: true})
	r.Close()
	s := r.Snapshot().Archive
	if s.Enabled {
		t.Errorf("归档应未启用: %+v", s)
	}
	// 内存指标仍工作。
	if s := r.Snapshot(); s.Completed != 1 {
		t.Errorf("completed=%d want 1", s.Completed)
	}
}

// TestArchiveBadDirDegrades 目录创建失败降级为哑实现：不 panic、丢弃计数可观测。
func TestArchiveBadDirDegrades(t *testing.T) {
	// 用一个文件路径当目录：MkdirAll 必失败。
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(Config{Dir: file})
	for i := 0; i < 5; i++ {
		r.Record(Event{Model: "m", Status: 200, OK: true})
	}
	s := r.Snapshot().Archive
	if s.Enabled {
		t.Errorf("坏目录下归档应降级未启用: %+v", s)
	}
	if s.Dropped != 5 {
		t.Errorf("dropped=%d want 5（丢弃必须可观测）", s.Dropped)
	}
	if s.LastError == "" {
		t.Error("LastError 应记录 mkdir 失败原因")
	}
	r.Close()
	if rows, _ := r.ReadArchive(0, nil); rows != nil {
		t.Errorf("降级态回读应返回 nil: %v", rows)
	}
}

// TestReadArchiveSkipsCorruptLines 半截行（crash 残尾）跳过不放大成整文件错误。
func TestReadArchiveSkipsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	good := `{"time":"2026-10-08T01:02:03Z","model":"cn:glm-5.2","status":200}`
	raw := good + "\n" + `{"time":"2026-10-08T01:02:0` + "\n" + good + "\n"
	if err := os.WriteFile(filepath.Join(dir, "requests-20261008.jsonl"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(Config{Dir: dir})
	defer r.Close()
	rows, err := r.ReadArchive(0, nil)
	if err != nil {
		t.Fatalf("损坏行不应报错: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("rows=%d want 2", len(rows))
	}
}
