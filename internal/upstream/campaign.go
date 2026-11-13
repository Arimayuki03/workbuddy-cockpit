// campaign.go 小程序活动任务（Campaign）协议（v1.25.0 G4 吸收件）。
//
// 与成长中心任务是**两套契约**：
//   - 任务定义/进度在 {billingBase}/portal/activity/tasks（codebuddy.cn）；
//   - 判据是小程序埋点事件（/v2/report），完成后必须再调 claim 才入账；
//   - claim 成功不等于到账：必须回读 claimed_at 才算确认（见 panel/campaign.go）。
//
// 来源：L0NE-6/WorkBuddy-Daily v50 实测（星巴克协作 / 记者节福利两活动）。
// 首版按设计 D5 跳过 app 任务（需 ACP bootstrap + SSE 校验 stopReason），
// 其余 5 项全部落到 spec 表。
package upstream

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
)

// CampaignTaskSpec 活动内单个任务的判据配方。
type CampaignTaskSpec struct {
	Task   string // 上游 task_code（ai_chat / expert / expert_chat / skill_use / desktop_same / app）
	Label  string // 中文展示名
	Kind   string // chat | expert | skill | desktop | app
	Times  int    // 目标次数（上游未下发 target_count 时兜底）
	Credit int64  // 展示用积分（以服务端 reward_credit 为准）
	// expert 判据的专家身份（缺省时调用方从市场列表任取一个真实专家）。
	ExpertID   string
	ExpertName string
	// skill 判据按**技能 slug** 认人（传中文名不计数）。
	SkillID   string
	SkillName string
	// desktop 判据的案例 id（必须传产物目录名 cnb-*，不是注册表 slug）。
	CaseID string
}

// CampaignSpec 单个活动定义。
type CampaignSpec struct {
	Code  string
	Label string
	// Page 真机埋点每条都带页面上下文，活动判据按页认人（缺了不推进度）。
	Page  map[string]any
	Tasks []CampaignTaskSpec
}

// CampaignSpecs 已实测的两个活动。返回新切片/新 map，调用方可安全修改。
func CampaignSpecs() []CampaignSpec {
	return []CampaignSpec{
		{
			Code: "campaign_starbucks_2026", Label: "星巴克协作",
			Page: map[string]any{
				"pageURL":  "subpackages/secondary/starbucks-activity-428/index",
				"pageName": "starbucks_activity",
				"source":   "mini_program",
			},
			Tasks: []CampaignTaskSpec{
				{Task: "ai_chat", Label: "与 AI 对话3次", Kind: "chat", Times: 3, Credit: 100},
				{Task: "expert", Label: "召唤专家并对话", Kind: "expert", Times: 1, Credit: 100},
				// 需手机端 App 登录后与 AI 对话一次；首版不伪造（设计 D5）。
				{Task: "app", Label: "解锁 App 强大能力", Kind: "app", Times: 1, Credit: 100},
			},
		},
		{
			Code: "campaign_reporter_2026", Label: "记者节福利活动",
			Page: map[string]any{
				"pageURL":  "subpackages/journalist-benefit/index",
				"pageName": "journalist_benefit",
				"source":   "mini_program",
			},
			Tasks: []CampaignTaskSpec{
				{
					Task: "expert_chat", Label: "体验「媒体工作助理」专家", Kind: "expert", Times: 1, Credit: 50,
					ExpertID: "ex_NaiDbO06Adf3", ExpertName: "媒体工作助理",
				},
				{
					Task: "skill_use", Label: "体验「采访素材整理」技能", Kind: "skill", Times: 1, Credit: 50,
					SkillID: "skill_2117491159283752960", SkillName: "interview-material-organizer",
				},
				{
					Task: "desktop_same", Label: "体验「内容发布排期」灵感", Kind: "desktop", Times: 1, Credit: 100,
					CaseID: "content-calendar",
				},
			},
		},
	}
}

// CampaignTask 活动任务的服务端状态行。
type CampaignTask struct {
	TaskCode    string
	Target      int64
	Progress    int64
	CompletedAt string
	ClaimedAt   string
	NeedView    *bool // nil = 上游未下发，按 true 处理（与 L0NE 默认一致）
	Credit      int64
}

// Completed 报告任务是否已完成（completed_at 有值或进度达标）。
func (t CampaignTask) Completed() bool {
	if t.CompletedAt != "" {
		return true
	}
	target := t.Target
	if target <= 0 {
		target = 1
	}
	return t.Progress >= target
}

// Claimed 报告是否已领奖（claimed_at 有值）。
func (t CampaignTask) Claimed() bool { return t.ClaimedAt != "" }

// NeedsView 报告是否需要先点「查看」建进度行。
func (t CampaignTask) NeedsView() bool { return t.NeedView == nil || *t.NeedView }

// campaignJSON 活动域请求底座：billingBase + 小程序身份头（桌面 AT 直接可用）。
func (c *Client) campaignJSON(a *auth.Auth, method, path string, body any) (json.RawMessage, error) {
	var rdr *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, c.billingBase(a)+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	req.Header.Set("X-Client-Product", "workbuddy-mp")
	req.Header.Set("X-Client-Version", "2.4.0")
	req.Header.Set("X-Client-Platform", "mp-weixin")
	req.Header.Set("X-Platform", "wechatmp")
	return c.doJSONAuth(a, req)
}

// CampaignTasks 读取活动任务定义 + 进度（GET /portal/activity/tasks?activity_code=）。
// 返回 map[task_code]CampaignTask；活动未下发/已结束时返回空 map（非错误）。
func (c *Client) CampaignTasks(a *auth.Auth, activityCode string) (map[string]CampaignTask, error) {
	path := "/portal/activity/tasks?activity_code=" + url.QueryEscape(activityCode)
	data, err := c.campaignJSON(a, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Tasks []struct {
			TaskCode     string `json:"task_code"`
			TargetCount  int64  `json:"target_count"`
			NeedView     *bool  `json:"need_view"`
			RewardCredit int64  `json:"reward_credit"`
		} `json:"tasks"`
		Progress []struct {
			TaskCode    string `json:"task_code"`
			Progress    int64  `json:"progress"`
			CompletedAt string `json:"completed_at"`
			ClaimedAt   string `json:"claimed_at"`
		} `json:"progress"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	rows := make(map[string]CampaignTask, len(resp.Tasks))
	for _, t := range resp.Tasks {
		if t.TaskCode == "" {
			continue
		}
		rows[t.TaskCode] = CampaignTask{
			TaskCode: t.TaskCode, Target: t.TargetCount,
			NeedView: t.NeedView, Credit: t.RewardCredit,
		}
	}
	for _, p := range resp.Progress {
		row, ok := rows[p.TaskCode]
		if !ok {
			continue
		}
		row.Progress = p.Progress
		row.CompletedAt = p.CompletedAt
		row.ClaimedAt = p.ClaimedAt
		rows[p.TaskCode] = row
	}
	return rows, nil
}

// CampaignView 点「查看」建进度行（need_view 任务必须先 viewed）。
func (c *Client) CampaignView(a *auth.Auth, activityCode, task string) error {
	_, err := c.campaignJSON(a, http.MethodPost,
		"/portal/activity/tasks/"+url.PathEscape(task)+"/viewed?activity_code="+url.QueryEscape(activityCode),
		map[string]any{})
	return err
}

// CampaignClaim 领取活动任务奖励；调用方必须回读 claimed_at 才算确认。
func (c *Client) CampaignClaim(a *auth.Auth, activityCode, task string) error {
	_, err := c.campaignJSON(a, http.MethodPost,
		"/portal/activity/tasks/"+url.PathEscape(task)+"/claim?activity_code="+url.QueryEscape(activityCode),
		map[string]any{})
	return err
}

// CampaignChatEvent 单条小程序对话事件（ai_chat 判据）。
func CampaignChatEvent(conversationID string) map[string]any {
	ev := SchoolChatTimesEvents(conversationID)
	ev["mode"] = "chat"
	return ev
}

// CampaignExpertEvents 专家召唤+对话 4 事件链（expert / expert_chat 判据）。
func CampaignExpertEvents(expertID, expertName, conversationID string) []map[string]any {
	if expertName == "" {
		expertName = expertID
	}
	rid := "wb2api-" + session.NewMessageID()
	const category = "16-BackToSchool"
	summonClick := map[string]any{
		"eventCode": "expert_summon_click", "id": expertID, "name": expertID,
		"expertTitle": expertName, "type": category, "position": 0,
	}
	summoned := map[string]any{
		"eventCode": "expert_summoned", "id": expertID, "name": expertID,
		"expertTitle": expertName,
	}
	actualUse := map[string]any{
		"eventCode": "expert_actual_use", "id": expertID, "name": expertID,
		"expertTitle": expertName, "type": category, "characterCount": 14,
		"expertType": "builtin",
	}
	chat := SchoolChatTimesEvents(conversationID)
	chat["traceId"] = rid
	chat["rootRequestId"] = rid
	chat["messageId"] = "msg-" + tail8(rid)
	chat["codebuddy.conversation_request_id"] = rid
	chat["expertId"] = expertID
	chat["expertName"] = expertName
	return []map[string]any{summonClick, summoned, actualUse, chat}
}

// CampaignSkillEvents 技能体验判据：skill_info + 带 skillId 的 chat_request_send。
// 注意 skillName 必须是市场 registry 的 slug（中文名不计数）。
func CampaignSkillEvents(skillID, skillName string) []map[string]any {
	conv := "wb2api-camp-" + session.NewMessageID()
	rid := "wb2api-" + session.NewMessageID()
	info := map[string]any{
		"eventCode": "skill_info", "id": skillName, "skillId": skillID,
		"skillName": skillName, "skillType": "market", "skillSource": "market",
		"position": 0, "conversationId": conv,
	}
	chat := SchoolChatTimesEvents(conv)
	chat["skillId"] = skillID
	chat["skillCount"] = 1
	chat["traceId"] = rid
	chat["rootRequestId"] = rid
	chat["messageId"] = "msg-" + tail8(rid)
	chat["codebuddy.conversation_request_id"] = rid
	return []map[string]any{info, chat}
}

// CampaignDesktopEvent 桌面域 `playbook_prompt_send`（desktop_same 判据）。
// id 必须传产物目录名 `cnb-content-calendar`（传 slug / 中文名都不计数）。
func CampaignDesktopEvent(caseID, name, typ string, promptLength int) DesktopEvent {
	if name == "" {
		name = "内容发布排期表"
	}
	if typ == "" {
		typ = "table"
	}
	return DesktopEvent{
		"eventCode": "playbook_prompt_send",
		"id":        caseID, "name": name, "type": typ,
		"promptLength": promptLength, "isOfficial": 1,
		"source": "discover", "query": "", "ext1": "discover",
		"requestId": uuidV4(), "conversationId": uuidV4(),
	}
}

// campaignCaseContentCalendar desktop_same 的实测案例元数据（L0NE v50）。
var campaignCaseContentCalendar = struct {
	EventID, Name, Type string
	PromptLength        int
}{
	EventID: "cnb-content-calendar", Name: "内容发布排期表", Type: "table", PromptLength: 359,
}

// CampaignDesktopContentCalendarEvent 构造「内容发布排期」案例事件（唯一内置案例）。
func CampaignDesktopContentCalendarEvent() DesktopEvent {
	c := campaignCaseContentCalendar
	return CampaignDesktopEvent(c.EventID, c.Name, c.Type, c.PromptLength)
}
