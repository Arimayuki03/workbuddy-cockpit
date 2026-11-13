// growth_v125_test.go v1.25.0 成长中心覆盖度补齐（G1/G2/G3/G4/G5 上游层）：
// 盲盒额度、专家深链解析/市场检索、技能分享渠道、活动任务、签到延迟入账与
// 旅行 claim 确认语义。全部用 httptest 假上游，不触网。
package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestParseExpertIDFromJumpURL 深链解析必须严格：只接受 experts?expertId=ex_*，
// 外链必须包 deeplink 且 host 可信；不能把任意 URL 内嵌值当专家 ID。
func TestParseExpertIDFromJumpURL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"直接深链", "workbuddy://experts?expertId=ex_GDl31WahyIH0", "ex_GDl31WahyIH0"},
		{"deeplink 包装", "deeplink=" + "workbuddy%3A%2F%2Fexperts%3FexpertId%3Dex_FbY5nlWTqDod", "ex_FbY5nlWTqDod"},
		{"可信外链", "https://www.workbuddy.cn/a?deeplink=" + "workbuddy%3A%2F%2Fexperts%3FexpertId%3Dex_Weo3x2EZuSDm", "ex_Weo3x2EZuSDm"},
		{"子域可信", "https://x.codebuddy.cn/a?deeplink=" + "workbuddy%3A%2F%2Fexperts%3FexpertId%3Dex_WZY136wV5aMt", "ex_WZY136wV5aMt"},
		{"不可信 host", "https://evil.com/a?deeplink=" + "workbuddy%3A%2F%2Fexperts%3FexpertId%3Dex_GDl31WahyIH0", ""},
		{"多个 expertId", "workbuddy://experts?expertId=ex_GDl31WahyIH0&expertId=ex_FbY5nlWTqDod", ""},
		{"非法 ID", "workbuddy://experts?expertId=evil", ""},
		{"错误 host", "workbuddy://expert?expertId=ex_GDl31WahyIH0", ""},
		{"空串", "", ""},
		{"带 fragment", "workbuddy://experts?expertId=ex_GDl31WahyIH0#x", ""},
		{"普通 URL", "https://www.workbuddy.cn/experts?id=ex_GDl31WahyIH0", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseExpertIDFromJumpURL(tc.raw); got != tc.want {
				t.Fatalf("ParseExpertIDFromJumpURL(%q)=%q want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestBuddyQuotaOpenCount 开盒数量 = min(affordable, max_open_count, 5)；
// max_open_count 缺省按 1（设计口径，不能当不限）。
func TestBuddyQuotaOpenCount(t *testing.T) {
	cases := []struct {
		q    BuddyQuota
		want int
	}{
		{BuddyQuota{Affordable: 0}, 0},
		{BuddyQuota{Affordable: -1}, 0},
		{BuddyQuota{Affordable: 3, MaxOpenCount: 0}, 1},
		{BuddyQuota{Affordable: 3, MaxOpenCount: 2}, 2},
		{BuddyQuota{Affordable: 9, MaxOpenCount: 9}, 5},
		{BuddyQuota{Affordable: 2, MaxOpenCount: 9}, 2},
	}
	for _, tc := range cases {
		if got := tc.q.OpenCount(); got != tc.want {
			t.Errorf("OpenCount(%+v)=%d want %d", tc.q, got, tc.want)
		}
	}
}

// TestBuddyBoxHTTP 盲盒 quota/open 端点形状与 count/client_token 传参。
func TestBuddyBoxHTTP(t *testing.T) {
	var mu sync.Mutex
	var openBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/activity/growth/buddy/quota":
			_, _ = w.Write([]byte(`{"code":0,"data":{"affordable":3,"max_open_count":2,"balance":20}}`))
		case "/v2/activity/growth/buddy/open":
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&openBody)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"data":{"results":[{"instance":{"name":"喵A","rarity":"N"}},{"template":{"name":"喵B"}}]}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	q, err := c.BuddyQuota(a)
	if err != nil {
		t.Fatalf("quota: %v", err)
	}
	if n := q.OpenCount(); n != 2 {
		t.Fatalf("OpenCount=%d want 2", n)
	}
	res, err := c.BuddyOpen(a, 2, "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if res.Count != 2 || strings.Join(res.Names, ",") != "喵A,喵B" {
		t.Fatalf("open result=%+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := int(openBody["count"].(float64)); got != 2 {
		t.Fatalf("count=%d want 2", got)
	}
	if token, _ := openBody["client_token"].(string); token == "" {
		t.Fatal("client_token 必须自动生成")
	}
}

// TestMarketExpertSearchByID 市场分页搜索按 expert_id 精确落位（jump_url 动态 ID 主路径）。
func TestMarketExpertSearchByID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/portal/operation-platform/market/expert/list" {
			w.WriteHeader(404)
			return
		}
		var body struct {
			Page    int    `json:"page"`
			Keyword string `json:"keyword"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Page == 1 {
			// 满页 20 条：搜索器才会继续翻页。
			var rows []string
			for i := 0; i < 20; i++ {
				rows = append(rows, `{"expert_id":"ex_other`+string(rune('A'+i))+`00000","display_name_zh":"别的","profession_zh":"别的"}`)
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"experts":[` + strings.Join(rows, ",") + `]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"items":[{"id":"ex_GDl31WahyIH0","name":"卖得好","profession":"中国电商运营专家","_meta":{"version":"1.0.9"}}]}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	e, ok, err := c.SearchMarketExpert(a, "中国电商运营专家", "ex_GDl31WahyIH0")
	if err != nil || !ok {
		t.Fatalf("search ok=%v err=%v", ok, err)
	}
	if e.DisplayNameZH != "卖得好" || e.Version != "1.0.9" {
		t.Fatalf("expert=%+v", e)
	}
}

// TestSkillShareHTTP 技能市场 + open.workbuddy.cn 渠道创建：asset_ref 必须是
// JSON 字符串，渠道缺 share_code/link 视为失败。
func TestSkillShareHTTP(t *testing.T) {
	var channelBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/operation-platform/market/skill/list":
			_, _ = w.Write([]byte(`{"code":0,"data":{"skills":[{"skill_id":"skill_2097350077599879168","display_name_zh":"润泽小馆·日报撰写","description_zh":"写日报"}]}}`))
		case "/api/v2/open-platform/asset-shares/c2c/channels":
			_ = json.NewDecoder(r.Body).Decode(&channelBody)
			_, _ = w.Write([]byte(`{"code":0,"data":{"share_code":"sc-1","channel_id":"ch-1","ways":[{"share_way":"link","resource_ref":"https://open.workbuddy.cn/s/sc-1"}]}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, OpenBaseCN: srv.URL}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	skills, err := c.MarketSkillList(a)
	if err != nil || len(skills) != 1 {
		t.Fatalf("skills=%v err=%v", skills, err)
	}
	ch, err := c.CreateSkillShareChannel(a, skills[0])
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	if ch.ShareCode != "sc-1" || ch.Link == "" {
		t.Fatalf("channel=%+v", ch)
	}
	if raw, ok := channelBody["asset_ref"].(string); !ok || !strings.Contains(raw, `"asset_id":"skill_2097350077599879168"`) {
		t.Fatalf("asset_ref 必须是 JSON 字符串，got=%#v", channelBody["asset_ref"])
	}
	events := SkillShareChannelEvents("skill_1", "日报", "sc-1")
	if len(events) != 2 || events[0]["panelAction"] != "click" || events[1]["panelAction"] != "success" {
		t.Fatalf("events=%v", events)
	}
}

// TestCampaignTasksAndClaim 活动任务读定义/进度、viewed 与 claim 路径（含 query）。
func TestCampaignTasksAndClaim(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.URL.Path == "/portal/activity/tasks" {
			if r.URL.Query().Get("activity_code") != "campaign_starbucks_2026" {
				t.Errorf("activity_code=%q", r.URL.Query().Get("activity_code"))
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[{"task_code":"ai_chat","target_count":3,"need_view":true,"reward_credit":100}],"progress":[{"task_code":"ai_chat","progress":1,"completed_at":"","claimed_at":""}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	rows, err := c.CampaignTasks(a, "campaign_starbucks_2026")
	if err != nil {
		t.Fatalf("tasks: %v", err)
	}
	row := rows["ai_chat"]
	if row.Target != 3 || row.Progress != 1 || !row.NeedsView() {
		t.Fatalf("row=%+v", row)
	}
	if err := c.CampaignView(a, "campaign_starbucks_2026", "ai_chat"); err != nil {
		t.Fatalf("view: %v", err)
	}
	if err := c.CampaignClaim(a, "campaign_starbucks_2026", "ai_chat"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	joined := strings.Join(paths, ",")
	for _, want := range []string{
		"POST /portal/activity/tasks/ai_chat/viewed",
		"POST /portal/activity/tasks/ai_chat/claim",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("缺少 %s，paths=%v", want, paths)
		}
	}
}

// TestCampaignEventShapes 判据事件必须带活动页面上下文可注入、技能 slug、
// desktop_same 用产物目录名 cnb-content-calendar。
func TestCampaignEventShapes(t *testing.T) {
	chat := CampaignChatEvent("conv-1")
	if chat["eventCode"] != "chat_request_send" || chat["mode"] != "chat" {
		t.Fatalf("chat=%v", chat)
	}
	expert := CampaignExpertEvents("ex_NaiDbO06Adf3", "媒体工作助理", "conv-2")
	if len(expert) != 4 {
		t.Fatalf("expert events=%d", len(expert))
	}
	if expert[2]["expertType"] != "builtin" || expert[3]["expertId"] != "ex_NaiDbO06Adf3" {
		t.Fatalf("expert chain=%v", expert)
	}
	skill := CampaignSkillEvents("skill_2117491159283752960", "interview-material-organizer")
	if len(skill) != 2 || skill[1]["skillId"] != "skill_2117491159283752960" || skill[1]["skillCount"] != 1 {
		t.Fatalf("skill events=%v", skill)
	}
	desktop := CampaignDesktopContentCalendarEvent()
	if desktop["eventCode"] != "playbook_prompt_send" || desktop["id"] != "cnb-content-calendar" {
		t.Fatalf("desktop=%v", desktop)
	}
}

// TestCheckinDetailedDelayed credit_delayed=true 是「待入账」，回执成功且带标记。
func TestCheckinDetailedDelayed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"credit_delayed":true}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}
	res, err := c.DailyCheckinDetailed(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if !res.CreditDelayed {
		t.Fatalf("CreditDelayed=false want true")
	}
}

// TestTravelClaimDetailedUnconfirmed 200 但无 reward_credit → confirmed=false
// （调用方据此不开盒/不 depart）。
func TestTravelClaimDetailedUnconfirmed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	reward, confirmed, err := c.TravelClaimDetailed(&auth.Auth{AccessToken: "at", UID: "u1"}, 42)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if confirmed || reward != 0 {
		t.Fatalf("confirmed=%v reward=%d want false/0", confirmed, reward)
	}
}

// TestTaskValidityDateOnly 纯日期 valid_end 按当日 23:59:59 截止（不在当天凌晨误判过期）；
// valid_start 未来日期判定未上线。
func TestTaskValidityDateOnly(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, cstShanghai)
	if (Task{ValidEnd: "2026-11-13"}).Expired(now) {
		t.Fatal("当日不应判过期")
	}
	if !(Task{ValidEnd: "2026-11-12"}).Expired(now) {
		t.Fatal("昨日应判过期")
	}
	if !(Task{Locked: true, ValidStart: "2026-11-14"}).NotYetValid(now) {
		t.Fatal("未来 valid_start 应判未上线")
	}
}
