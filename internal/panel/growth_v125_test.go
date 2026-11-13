// growth_v125_test.go 面板 v1.25.0 新自动化路径：活动任务闭环、技能分享、
// expert_use 动态解析、未确认领奖去重与 401 刷新重放。
package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// newV125Panel 以单 httptest 假上游装配最小 Panel（各 base 同源）。
func newV125Panel(t *testing.T, handler http.Handler) *Panel {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
		WebBaseCN:     srv.URL,
		OpenBaseCN:    srv.URL,
	}
	up.SyncHot()
	return New(Config{Version: "test", Upstream: up})
}

func v125Auth() *auth.Auth { return &auth.Auth{AccessToken: "at", UID: "u1", Nickname: "测试号"} }

// TestClaimTaskRewardUnconfirmedDedup 回执无金额且回读未落账 → 本轮不重复 POST；
// 回读确认 claimed 后自动清除标记。
func TestClaimTaskRewardUnconfirmedDedup(t *testing.T) {
	var claimCalls atomic.Int32
	var claimed atomic.Bool
	p := newV125Panel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/activity/growth/tasks":
			status := "accepted"
			cur := int64(1)
			if claimed.Load() {
				status = "claimed"
			}
			fmt.Fprintf(w, `{"code":0,"data":{"tasks":[{"task_code":"chat_5","target":1,"current":%d,"accept_status":%q}]}}`, cur, status)
		case strings.HasSuffix(r.URL.Path, "/tasks/chat_5/claim"):
			claimCalls.Add(1)
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`)) // 无金额、非 already_claimed
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		}
	}))
	a := v125Auth()

	credit, energy, ok, err := p.claimTaskReward(a, "chat_5", false)
	if err != nil || ok || credit != 0 || energy != 0 {
		t.Fatalf("首次: credit=%d energy=%d ok=%v err=%v（应有回执未确认）", credit, energy, ok, err)
	}
	// 同一轮再次调用：不得二次 POST。
	if _, _, ok2, _ := p.claimTaskReward(a, "chat_5", false); ok2 {
		t.Fatal("未确认状态下不得确认领取")
	}
	if n := claimCalls.Load(); n != 1 {
		t.Fatalf("claim POST=%d want 1（未确认不得重复提交）", n)
	}
	// 上游落账后：回读确认并清除标记（仍不重复 POST）。
	claimed.Store(true)
	_, _, ok3, err := p.claimTaskReward(a, "chat_5", false)
	if err != nil || !ok3 {
		t.Fatalf("落账后应确认领取: ok=%v err=%v", ok3, err)
	}
	if n := claimCalls.Load(); n != 1 {
		t.Fatalf("claim POST=%d want 1（回读确认不产生新 POST）", n)
	}
	if p.claimPending(a.UID, "chat_5") {
		t.Fatal("回读确认后应清除未确认标记")
	}
}

// TestTaskByCode401RefreshReplay 执行链首个请求 401 → 刷新 token → 重放一次。
func TestTaskByCode401RefreshReplay(t *testing.T) {
	var listCalls, refreshCalls atomic.Int32
	p := newV125Panel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/activity/growth/tasks":
			if listCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[{"task_code":"chat_5","target":5,"current":1,"accept_status":"accepted"}]}}`))
		case "/v2/plugin/auth/token/refresh":
			refreshCalls.Add(1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":3600}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		}
	}))
	a := v125Auth()
	a.RefreshToken = "rt"

	task, err := p.taskByCode(a, "chat_5")
	if err != nil || task == nil {
		t.Fatalf("task=%v err=%v", task, err)
	}
	if listCalls.Load() != 2 || refreshCalls.Load() != 1 {
		t.Fatalf("list=%d refresh=%d want 2/1", listCalls.Load(), refreshCalls.Load())
	}
	if a.AccessTokenValue() != "new-at" {
		t.Fatalf("accessToken=%q want new-at", a.AccessTokenValue())
	}
}

// TestCampaignTaskChatCloseLoop 活动任务：viewed → 事件 → 回读 completed → claim
// → 回读 claimed_at。
func TestCampaignTaskChatCloseLoop(t *testing.T) {
	oldFast, oldFastN, oldSlow, oldSlowN, oldCG, oldCN := campaignFastGap, campaignFastPoll, campaignSlowGap, campaignSlowPoll, campaignClaimGap, campaignClaimPoll
	campaignFastGap, campaignFastPoll = 0, 1
	campaignSlowGap, campaignSlowPoll = 0, 1
	campaignClaimGap, campaignClaimPoll = 0, 2
	t.Cleanup(func() {
		campaignFastGap, campaignFastPoll = oldFast, oldFastN
		campaignSlowGap, campaignSlowPoll = oldSlow, oldSlowN
		campaignClaimGap, campaignClaimPoll = oldCG, oldCN
	})

	var mu sync.Mutex
	var calls []string
	reported, claimedFlag := false, false
	p := newV125Panel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.URL.Path == "/portal/activity/tasks":
			if r.URL.Query().Get("activity_code") != "campaign_starbucks_2026" {
				_, _ = fmt.Fprintf(w, `{"code":0,"data":{"tasks":[],"progress":[]}}`)
				return
			}
			progress, completed, claimed := 0, "", ""
			if reported {
				progress, completed = 1, "2026-11-13T10:00:00Z"
			}
			if claimedFlag {
				claimed = "2026-11-13T10:01:00Z"
			}
			_, _ = fmt.Fprintf(w, `{"code":0,"data":{"tasks":[{"task_code":"ai_chat","target_count":1,"need_view":true,"reward_credit":100}],"progress":[{"task_code":"ai_chat","progress":%d,"completed_at":%q,"claimed_at":%q}]}}`, progress, completed, claimed)
		case strings.HasSuffix(r.URL.Path, "/viewed"):
			mu.Lock()
			calls = append(calls, "viewed")
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		case r.URL.Path == "/v2/report":
			reported = true
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/claim"):
			claimedFlag = true
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		}
	}))

	ref, ok := campaignRefFor(campaignTaskCode("campaign_starbucks_2026", "ai_chat"))
	if !ok {
		t.Fatal("campaignRefFor 应命中 ai_chat")
	}
	msg, err := p.runCampaignTask(v125Auth(), ref)
	if err != nil {
		t.Fatalf("runCampaignTask: %v", err)
	}
	if !strings.Contains(msg, "已领取活动奖励") {
		t.Fatalf("msg=%q", msg)
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(calls, ",")
	if !strings.Contains(joined, "POST /portal/activity/tasks/ai_chat/viewed") ||
		!strings.Contains(joined, "POST /v2/report") ||
		!strings.Contains(joined, "POST /portal/activity/tasks/ai_chat/claim") {
		t.Fatalf("calls=%v", calls)
	}
}

// TestCampaignSkipsAppTask 首版跳过 app 任务：不入待办、不生成合成码动作。
func TestCampaignSkipsAppTask(t *testing.T) {
	p := newV125Panel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/portal/activity/tasks" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[{"task_code":"ai_chat","target_count":3,"reward_credit":100},{"task_code":"app","target_count":1,"reward_credit":100}],"progress":[]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	tasks := p.campaignTasksForAccount(v125Auth(), false)
	if len(tasks) != 1 || tasks[0].TaskCode != campaignTaskCode("campaign_starbucks_2026", "ai_chat") {
		t.Fatalf("tasks=%+v（app 必须排除）", tasks)
	}
	if autoActionFor(tasks[0].TaskCode) == nil {
		t.Fatal("活动任务应注册 autoAction")
	}
	if !growthPending(tasks[0]) {
		t.Fatal("活动任务应进入待办")
	}
}

// TestSkillShareAction 市场技能 → 真实渠道 → 两条渠道事件。
func TestSkillShareAction(t *testing.T) {
	var channelCalls, reportEvents atomic.Int32
	p := newV125Panel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/activity/growth/tasks":
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[{"task_code":"skill_share","target":1,"current":0,"accept_status":"accepted","reward_credit":200}]}}`))
		case r.URL.Path == "/v2/operation-platform/market/skill/list":
			_, _ = w.Write([]byte(`{"code":0,"data":{"skills":[{"skill_id":"skill_2097350077599879168","display_name_zh":"润泽小馆·日报撰写","description_zh":"写日报"}]}}`))
		case r.URL.Path == "/api/v2/open-platform/asset-shares/c2c/channels":
			channelCalls.Add(1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"share_code":"sc-9","ways":[{"share_way":"link","resource_ref":"https://open.workbuddy.cn/s/sc-9"}]}}`))
		case r.URL.Path == "/v2/report":
			var events []map[string]any
			_ = json.NewDecoder(r.Body).Decode(&events)
			reportEvents.Add(int32(len(events)))
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		}
	}))
	msg, err := runSkillShare(p, v125Auth())
	if err != nil {
		t.Fatalf("runSkillShare: %v", err)
	}
	if !strings.Contains(msg, "sc-9") {
		t.Fatalf("msg=%q", msg)
	}
	if channelCalls.Load() != 1 || reportEvents.Load() != 2 {
		t.Fatalf("channel=%d events=%d want 1/2", channelCalls.Load(), reportEvents.Load())
	}
}

// TestResolveExpertForTaskJumpURLOverride jump_url 的 expertId 覆盖常量表；
// 市场不可用时降级常量表且不报错。
func TestResolveExpertForTaskJumpURLOverride(t *testing.T) {
	p := newV125Panel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound) // 市场不可用
		_, _ = w.Write([]byte(`{"code":404,"msg":"not found"}`))
	}))
	task := &upstream.Task{
		TaskCode: "expert_use_maidehao",
		JumpURL:  "workbuddy://experts?expertId=ex_NewDynamic01",
	}
	expert, err := p.resolveExpertForTask(v125Auth(), "expert_use_maidehao", task)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if expert.ExpertID != "ex_NewDynamic01" {
		t.Fatalf("expert_id=%q want jump_url 覆盖", expert.ExpertID)
	}
	if expert.DisplayNameZH != "卖得好" || expert.Version != "1.0.1" {
		t.Fatalf("降级常量表失败: %+v", expert)
	}
}

// TestTaskNotYetValidLocked 锁定且 valid_start 在未来 → 不执行动作。
func TestTaskNotYetValidLocked(t *testing.T) {
	future := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	task := upstream.Task{Locked: true, ValidStart: future}
	if !task.NotYetValid(time.Now()) {
		t.Fatal("未来 valid_start 应判定未到上线时间")
	}
	past := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	if (upstream.Task{Locked: true, ValidStart: past}).NotYetValid(time.Now()) {
		t.Fatal("过去 valid_start 不应阻塞")
	}
}
