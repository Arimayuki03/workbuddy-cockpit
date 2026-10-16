package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// fakeScriptExec 记录命令构建参数并按需模拟执行失败，替代真实 exec 拉起 python3 子进程。
type fakeScriptExec struct {
	lastName string
	lastArgs []string
	lastDir  string
	runN     int
	err      error
}

func (f *fakeScriptExec) SetDir(dir string) { f.lastDir = dir }
func (f *fakeScriptExec) Run() error        { f.runN++; return f.err }

// installFakeExec 替换 newScriptCmd，测试结束还原。
func installFakeExec(t *testing.T) *fakeScriptExec {
	t.Helper()
	f := &fakeScriptExec{}
	orig := newScriptCmd
	newScriptCmd = func(name string, args ...string) scriptRunner {
		f.lastName, f.lastArgs = name, args
		return f
	}
	t.Cleanup(func() { newScriptCmd = orig })
	return f
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestNextWakeCatSlot 夜猫子任务在 cat_hours（默认 1 点）处有独立时点。
func TestNextWakeCatSlot(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{9},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		CatHours:          []int{1},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 14, 23, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 15, 1, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（cat 01:00 独立时点）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCat {
		t.Errorf("kinds=%v want [cat]", kinds)
	}
}

// TestNextWakeSchoolCatDisabled 显式禁用 school/cat 后排程只剩签到时点（互不影响）。
func TestNextWakeSchoolCatDisabled(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{21},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		SchoolHours:       []int{12},
		CatHours:          []int{1},
		SchoolDisabled:    true,
		CatDisabled:       true,
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 14, 8, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 14, 21, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（school/cat 禁用 → 只有签到 21:00）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}

// TestDispatchSchoolSkipsAfterRetirement 开学季活动结束（2026-09-24）后：
// dispatch(taskSchool) 不再执行任何脚本，只记一条「活动已结束」说明。
// taskSchool 枚举位保留（stable name "school" 兼容 /admin 热改与老 config）。
func TestDispatchSchoolSkipsAfterRetirement(t *testing.T) {
	f := installFakeExec(t)
	s := New(Config{})
	s.dispatch(context.Background(), taskSchool)
	if f.runN != 0 {
		t.Errorf("活动下线后不应执行任何脚本，runN=%d", f.runN)
	}
	// runOne 把摘要包装成 "<summary> (耗时 …)"，断言 lastOut 不再是 done。
	if got := s.lastOut[taskSchool].Load().(string); strings.HasPrefix(got, "done") {
		t.Errorf("lastOut=%q want skipped 摘要（活动已结束）", got)
	}
}

// TestRunCatNowSkipsOutsideNightWindow RunCatNow（v1.2.0 纯 API 口径）：夜猫窗口外
// 触发直接跳过，不发任何上游调用；窗口判定经 nightWindow 注入为恒假，跨时段可测
// （真实窗口 23:00–08:00 CST，按真实时钟写用例每天有 15 小时只能 skip）。
func TestRunCatNowSkipsOutsideNightWindow(t *testing.T) {
	forceNightWindow(t, false)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunCatNow()
	if calls != 0 {
		t.Errorf("窗口外不应发任何上游调用，got %d", calls)
	}
}

// TestRunCatNowInsideNightWindowZeroNeed RunCatNow 窗口内：差额为 0（black_cat 已
// 达标）时走到差额探测但不再发对话。tasks 桩 payload 用 data.tasks 形状（与
// upstream.parseGrowthTasks 一致）——旧用例误写 data.list 会被解析成空任务列表，
// 使「没发对话」恒真、断言失去意义。
func TestRunCatNowInsideNightWindowZeroNeed(t *testing.T) {
	forceNightWindow(t, true)
	silenceBatchDelays(t)
	l := newBatchLedger()
	var chatCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			chatCalls++
			w.Write([]byte(`{}`))
			return
		}
		batchStub(l).ServeHTTP(w, r)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunCatNow()
	if got := l.uidsOf(catTasksPath); len(got) != 1 || got[0] != "u1" {
		t.Errorf("差额探测账号=%v want [u1]（窗口内应到达 BlackcatNeed）", got)
	}
	if chatCalls != 0 {
		t.Errorf("need=0 时不应发对话，got %d", chatCalls)
	}
}

// —— 批量路径账号过滤（夜猫口径，吸收 workbuddy-panel b1a2284d）——

// catTasksPath 夜猫差额探测路径（= upstream.tasksListPath，未导出故按字面量对齐）。
const catTasksPath = "/v2/activity/growth/tasks"

// batchLedger 批量路径的上游请求台账：路径 → 账号 UID → 次数。断言只关心「哪个号
// 被遍历到」，按 UID 记账、顺序不参与断言（账号间限速/并发时序差异不引入 flake）。
type batchLedger struct {
	mu   sync.Mutex
	hits map[string]map[string]int
}

func newBatchLedger() *batchLedger {
	return &batchLedger{hits: make(map[string]map[string]int)}
}

func (l *batchLedger) record(path, uid string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.hits[path]
	if m == nil {
		m = make(map[string]int)
		l.hits[path] = m
	}
	m[uid]++
}

// uidsOf 某路径命中的 UID 列表（去重 + 排序，可逐位比较）。
func (l *batchLedger) uidsOf(path string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.hits[path]))
	for uid := range l.hits[path] {
		out = append(out, uid)
	}
	sort.Strings(out)
	return out
}

// batchStub 夜猫/旅行/活跃三条批量路径共用的上游桩：按路径记台账，并把每个端点回成
// 「无事可做」的良性格（black_cat 已达标 / 已有猫且在途 / 上报 OK）——断言只落在
// 「谁被遍历到」，不触发真实对话（RunNightChats 每条后睡 4s，测试会白等）。
func batchStub(l *batchLedger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := r.Header.Get("X-User-Id")
		switch r.URL.Path {
		case catTasksPath:
			l.record(r.URL.Path, uid)
			// current==target → BlackcatNeed=0：只验证「走到了差额探测」。
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"tasks":[{"task_code":"black_cat","target":3,"current":3}]}}`))
		case "/v2/report":
			l.record(r.URL.Path, uid)
			w.Write([]byte(`{"code":0,"msg":"OK"}`))
		case "/activity/growth/buddy/info":
			l.record(r.URL.Path, uid)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"buddy":{"id":1,"name":"档案喵"}}}`))
		case "/activity/growth/buddy/travel/status":
			l.record(r.URL.Path, uid)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"state":"traveling","record_id":7}}`))
		default:
			// 其余端点（streak/连登奖励等）回良性空壳：单号失败只记 WARN，不中断遍历。
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		}
	})
}

// forceNightWindow 把夜猫窗口判定注入为恒真/恒假：runCat 的窗口分支跨时段可测
// （生产恒为 upstream.InNightWindow，见 school.go 的 nightWindow 注释）。
func forceNightWindow(t *testing.T, in bool) {
	t.Helper()
	old := nightWindow
	nightWindow = func(time.Time) bool { return in }
	t.Cleanup(func() { nightWindow = old })
}

// silenceBatchDelays 关掉批量路径的账号间/账号内限速，避免测试白等（口径同
// activity_test.go 的 fastActivity，这里再含旅行与夜猫两条）。
func silenceBatchDelays(t *testing.T) {
	t.Helper()
	oldAcc, oldTravel, oldGap, oldAdopt := activityAccountDelay, travelAccountDelay, activityReportGap, adoptReportGap
	activityAccountDelay, travelAccountDelay, activityReportGap, adoptReportGap = 0, 0, 0, 0
	t.Cleanup(func() {
		activityAccountDelay, travelAccountDelay, activityReportGap, adoptReportGap = oldAcc, oldTravel, oldGap, oldAdopt
	})
}

// newBatchScheduler 构造「一手动停用号（manual）+ 一正常号（plain）」的调度器与台账桩。
// 两个 token 都给：夜猫看 AccessToken、旅行看 RefreshToken，缺一个就会因凭证不足被
// 跳过——跳过原因若与被测的过滤位无关，断言即失去意义。
func newBatchScheduler(t *testing.T) (*Scheduler, *batchLedger) {
	t.Helper()
	silenceBatchDelays(t)
	l := newBatchLedger()
	srv := httptest.NewServer(batchStub(l))
	t.Cleanup(srv.Close)

	p := pool.New("")
	for _, uid := range []string{"manual", "plain"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	p.SetManualDisabled("manual", true, "让位防风控（测试）")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up, ActivityReportCount: 1}), l
}

// assertVisited 断言某路径恰好被 manual + plain 两个号各命中：两号都被执行（顺序
// 无关）。任一路径被顺手加上 ManualDisabled 过滤时，manual 缺席即在此报红。
func assertVisited(t *testing.T, l *batchLedger, path string) {
	t.Helper()
	got := l.uidsOf(path)
	if len(got) != 2 || got[0] != "manual" || got[1] != "plain" {
		t.Fatalf("%s 命中账号=%v want [manual plain]（手动停用号应照常执行）", path, got)
	}
}

// TestRunCatSkipsManualDisabled 夜猫口径（吸收 workbuddy-panel b1a2284d）：手动停用
// （ManualDisabled，语义「让位防风控」）号在夜猫批量路径被跳过——夜猫子是唯一
// 「整任务都是真实模型对话」的批量任务（RunNightChats 逐条 ChatStream 发 glm-5.2），
// 把已让位的号半夜拉出来发消息与让位语义正面冲突。表驱动覆盖四种账号态，断言落点是
// 「谁到达了差额探测（BlackcatNeed）」。
func TestRunCatSkipsManualDisabled(t *testing.T) {
	type seed struct {
		uid     string
		disable bool // pool.Disable：系统判定（session dead 等）
		manual  bool // pool.SetManualDisabled：运维手动停用
	}
	cases := []struct {
		name       string
		seeds      []seed
		wantProbed []string // 期望到达差额探测的 UID（排序后逐位比较）
	}{
		{
			name:       "正常号照常探测",
			seeds:      []seed{{uid: "plain"}},
			wantProbed: []string{"plain"},
		},
		{
			name:       "手动停用号跳过、同池正常号不受影响",
			seeds:      []seed{{uid: "manual", manual: true}, {uid: "plain"}},
			wantProbed: []string{"plain"},
		},
		{
			name:       "系统禁用号跳过（既有口径不变）",
			seeds:      []seed{{uid: "auto", disable: true}, {uid: "plain"}},
			wantProbed: []string{"plain"},
		},
		{
			name:       "自动禁用与手动停用叠加同样跳过",
			seeds:      []seed{{uid: "both", disable: true, manual: true}},
			wantProbed: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forceNightWindow(t, true)
			silenceBatchDelays(t)
			l := newBatchLedger()
			srv := httptest.NewServer(batchStub(l))
			defer srv.Close()

			p := pool.New("")
			for _, sd := range tc.seeds {
				p.Add(&auth.Auth{UID: sd.uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
				if sd.disable {
					p.Disable(sd.uid, "test: auto disable")
				}
				if sd.manual {
					p.SetManualDisabled(sd.uid, true, "test: manual disable")
				}
			}
			up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
			s := New(Config{Pool: p, Upstream: up})

			s.runCat(context.Background())

			got := l.uidsOf(catTasksPath)
			if len(got) != len(tc.wantProbed) {
				t.Fatalf("差额探测账号=%v want %v（夜猫只跳 Disabled||ManualDisabled）", got, tc.wantProbed)
			}
			for i := range got {
				if got[i] != tc.wantProbed[i] {
					t.Fatalf("差额探测账号=%v want %v（夜猫只跳 Disabled||ManualDisabled）", got, tc.wantProbed)
				}
			}
		})
	}
}

// TestCatManualDisabledSkipIsNightOnly 口径边界（吸收 panel b1a2284d）：「手动停用号
// 跳过」是**夜猫专有**口径，不得外溢——同一号在旅行/活跃（含连登成长链）批量路径
// 仍被执行。三条路径共用同一账号构造，任一路径被顺手加上 ManualDisabled 断言即红。
func TestCatManualDisabledSkipIsNightOnly(t *testing.T) {
	t.Run("旅行批量路径仍执行手动停用号", func(t *testing.T) {
		s, l := newBatchScheduler(t)
		s.RunTravelNow()
		assertVisited(t, l, "/activity/growth/buddy/info")
		assertVisited(t, l, "/activity/growth/buddy/travel/status")
	})

	t.Run("活跃上报（成长连登链）批量路径仍执行手动停用号", func(t *testing.T) {
		s, l := newBatchScheduler(t)
		s.RunActivityNow()
		assertVisited(t, l, "/v2/report")
	})

	t.Run("夜猫批量路径跳过手动停用号（对照组）", func(t *testing.T) {
		forceNightWindow(t, true)
		s, l := newBatchScheduler(t)
		s.runCat(context.Background())
		if got := l.uidsOf(catTasksPath); len(got) != 1 || got[0] != "plain" {
			t.Fatalf("差额探测账号=%v want [plain]（夜猫专有跳过，对照上面两条路径）", got)
		}
	})
}

// TestPythonCmd WB2A_PYTHON 覆盖解释器名：缺省/空白回落 "python3"（保持
// 容器与既有测试的行为不变），显式设置时取其值（Windows 等仅有 python 的环境）。
func TestPythonCmd(t *testing.T) {
	t.Setenv("WB2A_PYTHON", "")
	if got := pythonCmd(); got != "python3" {
		t.Errorf("default pythonCmd()=%q want python3", got)
	}

	t.Setenv("WB2A_PYTHON", "   ")
	if got := pythonCmd(); got != "python3" {
		t.Errorf("blank pythonCmd()=%q want python3", got)
	}

	t.Setenv("WB2A_PYTHON", "python")
	if got := pythonCmd(); got != "python" {
		t.Errorf("override pythonCmd()=%q want python", got)
	}

	t.Setenv("WB2A_PYTHON", "  /usr/bin/python3.10  ")
	if got := pythonCmd(); got != "/usr/bin/python3.10" {
		t.Errorf("trim pythonCmd()=%q want /usr/bin/python3.10", got)
	}
}

// guard：os 与 http 包仍被同文件其余测试引用（编译期存在性兜底，防误删 import）。
var _ = os.Getenv
var _ = http.StatusOK
