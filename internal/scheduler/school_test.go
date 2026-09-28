package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
// 触发直接跳过，不发任何上游调用；窗口内（InNightWindow=true 时）才走差额探测。
func TestRunCatNowSkipsOutsideNightWindow(t *testing.T) {
	if upstream.InNightWindow(time.Now()) {
		t.Skip("当前处于夜猫窗口内，窗口外跳过分支无法验证")
	}
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

// TestRunCatNowInsideNightWindow RunCatNow 窗口内：差额为 0（任务已完成）时不再发对话。
func TestRunCatNowInsideNightWindowZeroNeed(t *testing.T) {
	if !upstream.InNightWindow(time.Now()) {
		t.Skip("当前不处于夜猫窗口内，窗口内分支无法验证")
	}
	var chatCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/tasks"):
			// black_cat 已达标：current=target → need=0
			w.Write([]byte(`{"code":0,"data":{"list":[{"task_code":"black_cat","current":3,"target_count":3,"claimed":false}]}}`))
		case strings.HasSuffix(r.URL.Path, "/chat/completions"), strings.Contains(r.URL.Path, "chat"):
			chatCalls++
			w.Write([]byte(`{}`))
		default:
			w.Write([]byte(`{"code":0,"data":{}}`))
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	old := activityAccountDelay
	activityAccountDelay = 0
	t.Cleanup(func() { activityAccountDelay = old })

	s.RunCatNow()
	if chatCalls != 0 {
		t.Errorf("need=0 时不应发对话，got %d", chatCalls)
	}
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
