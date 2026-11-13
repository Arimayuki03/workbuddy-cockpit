// campaign.go 面板「活动任务」自动化（v1.25.0 G4）。
//
// 与成长中心任务不同：活动任务定义在 codebuddy.cn 的 /portal/activity/*，
// 判据是小程序埋点事件，且「完成 ≠ 到账」——completed 后必须 claim，并回读
// claimed_at 才算确认。首版按设计 D5 跳过 app 任务（需 ACP 真机会话）。
//
// 活动任务以合成码 campaign:<activity_code>:<task_code> 接入既有任务中心：
//   - 扫描 / 队列 / 一键完成都会列出并执行（autoActions 在 init 中追加）；
//   - 合成码在 taskByCode 中单独解析，通用回读/展示复用 upstream.Task 形状；
//   - 领奖走活动域端点，不进入成长任务的 ClaimReward 路径。
package panel

import (
	"fmt"
	"log"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// campaignCodePrefix 合成任务码前缀（用户可见；与上游 task_code 无冲突）。
const campaignCodePrefix = "campaign:"

// 轮询参数（var 便于测试置 0；生产口径对齐 L0NE v50）。
var (
	campaignFastGap   = 3 * time.Second
	campaignFastPoll  = 6
	campaignSlowGap   = 10 * time.Second
	campaignSlowPoll  = 6
	campaignClaimGap  = 4 * time.Second
	campaignClaimPoll = 5
	campaignMaxEvents = 10 // 单任务单轮补报上限（target 异常时不至于无限循环）
)

// campaignTaskCode 生成活动任务的合成码。
func campaignTaskCode(activity, task string) string {
	return campaignCodePrefix + activity + ":" + task
}

// parseCampaignCode 解析合成码；非活动码返回 ok=false。
func parseCampaignCode(code string) (activity, task string, ok bool) {
	code = strings.TrimSpace(code)
	if !strings.HasPrefix(code, campaignCodePrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(code, campaignCodePrefix)
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// isCampaignTaskCode 报告是否为活动任务合成码。
func isCampaignTaskCode(code string) bool {
	_, _, ok := parseCampaignCode(code)
	return ok
}

// campaignTaskRef 活动 + 任务配方的组合引用。
type campaignTaskRef struct {
	Activity upstream.CampaignSpec
	Task     upstream.CampaignTaskSpec
}

// campaignRefFor 按合成码查配方；app 任务返回 ok=false（首版不支持）。
func campaignRefFor(code string) (campaignTaskRef, bool) {
	activity, task, ok := parseCampaignCode(code)
	if !ok {
		return campaignTaskRef{}, false
	}
	for _, spec := range upstream.CampaignSpecs() {
		if spec.Code != activity {
			continue
		}
		for _, ts := range spec.Tasks {
			if ts.Task == task && ts.Kind != "app" {
				return campaignTaskRef{Activity: spec, Task: ts}, true
			}
		}
	}
	return campaignTaskRef{}, false
}

// allCampaignRefs 全部可自动化活动任务（spec 顺序）。
func allCampaignRefs() []campaignTaskRef {
	var out []campaignTaskRef
	for _, spec := range upstream.CampaignSpecs() {
		for _, ts := range spec.Tasks {
			if ts.Kind == "app" {
				continue // 设计 D5：首版跳过 app（ACP + SSE，成本/收益不匹配）
			}
			out = append(out, campaignTaskRef{Activity: spec, Task: ts})
		}
	}
	return out
}

// init 把活动任务注册进既有 autoActions 执行表（顺序追加在成长任务之后）。
// 合成码的 run 自行完成 view → 事件 → 回读 → claim 全链，因此通用外层的
// 自动领奖会按 isCampaignTaskCode 跳过（见 autotask.go/taskcenter.go）。
func init() {
	for _, ref := range allCampaignRefs() {
		ref := ref
		code := campaignTaskCode(ref.Activity.Code, ref.Task.Task)
		autoActions = append(autoActions, autoAction{
			TaskCode: code,
			Desc:     ref.Activity.Label + " · " + ref.Task.Label + "（活动任务）",
			run: func(p *Panel, a *auth.Auth) (string, error) {
				return p.runCampaignTask(a, ref)
			},
		})
	}
}

// campaignTaskToTask 把活动任务状态转换成通用 Task（面板/队列/前端共用形状）。
func campaignTaskToTask(ref campaignTaskRef, row upstream.CampaignTask) upstream.Task {
	target := row.Target
	if target <= 0 {
		target = int64(ref.Task.Times)
	}
	if target <= 0 {
		target = 1
	}
	credit := row.Credit
	if credit <= 0 {
		credit = ref.Task.Credit
	}
	return upstream.Task{
		TaskCode:     campaignTaskCode(ref.Activity.Code, ref.Task.Task),
		Title:        ref.Task.Label,
		Description:  ref.Activity.Label,
		Credit:       credit,
		TaskType:     "campaign",
		Target:       target,
		Current:      row.Progress,
		AcceptStatus: "accepted",
		Claimable:    row.Completed() && !row.Claimed(),
		Claimed:      row.Claimed(),
	}
}

// campaignTasksForAccount 拉取该账号全部活动任务状态（includeClaimed=false 时
// 只返回未领奖条目，供待办列表使用）。
func (p *Panel) campaignTasksForAccount(a *auth.Auth, includeClaimed bool) []upstream.Task {
	if a == nil || a.IsGlobal() {
		return nil // global 无小程序活动体系，零上游请求
	}
	var out []upstream.Task
	for _, spec := range upstream.CampaignSpecs() {
		rows, err := p.campaignTasks(a, spec.Code)
		if err != nil {
			// 活动未下发/已结束/网络失败：跳过该活动（不阻塞成长任务）。
			continue
		}
		for _, ts := range spec.Tasks {
			if ts.Kind == "app" {
				continue
			}
			row, ok := rows[ts.Task]
			if !ok {
				continue
			}
			if row.Claimed() && !includeClaimed {
				continue
			}
			out = append(out, campaignTaskToTask(campaignTaskRef{Activity: spec, Task: ts}, row))
		}
	}
	return out
}

// campaignTasks 活动任务读数（401/12153 刷新 token 后重放一次，G5.4 同口径）。
func (p *Panel) campaignTasks(a *auth.Auth, activity string) (map[string]upstream.CampaignTask, error) {
	rows, err := p.cfg.Upstream.CampaignTasks(a, activity)
	if err != nil && panelAuthErr(err) && p.refreshAccountToken(a, err) {
		return p.cfg.Upstream.CampaignTasks(a, activity)
	}
	return rows, err
}

// campaignTaskByCode 读取单个活动任务状态（taskByCode 的合成码分支）。
func (p *Panel) campaignTaskByCode(a *auth.Auth, code string) (*upstream.Task, error) {
	if a == nil || a.IsGlobal() {
		return nil, nil // global 无活动体系：零上游请求
	}
	ref, ok := campaignRefFor(code)
	if !ok {
		return nil, nil
	}
	rows, err := p.campaignTasks(a, ref.Activity.Code)
	if err != nil {
		return nil, err
	}
	row, ok := rows[ref.Task.Task]
	if !ok {
		return nil, nil
	}
	t := campaignTaskToTask(ref, row)
	return &t, nil
}

// runCampaignTask 单活动任务闭环：view → 判据事件 → 轮询 → claim + 回读。
func (p *Panel) runCampaignTask(a *auth.Auth, ref campaignTaskRef) (string, error) {
	if a == nil || a.IsGlobal() {
		return "global 账号无活动任务体系，跳过", nil
	}
	rows, err := p.campaignTasks(a, ref.Activity.Code)
	if err != nil {
		return "", fmt.Errorf("活动任务读取失败: %w", err)
	}
	row, ok := rows[ref.Task.Task]
	if !ok {
		return "活动任务未下发/已下线", nil
	}
	if row.Claimed() {
		return "已领取", nil
	}
	target := row.Target
	if target <= 0 {
		target = int64(ref.Task.Times)
	}
	if target <= 0 {
		target = 1
	}
	if row.Completed() {
		return p.claimCampaignTask(a, ref, row.Credit)
	}
	// need_view 任务必须先 viewed 建进度行，否则完成也不记账。
	if row.NeedsView() {
		if err := p.cfg.Upstream.CampaignView(a, ref.Activity.Code, ref.Task.Task); err != nil {
			log.Printf("panel: 活动 %s/%s viewed: %v（继续尝试上报）", ref.Activity.Code, ref.Task.Task, err)
		}
	}

	cur := row.Progress
	switch ref.Task.Kind {
	case "desktop":
		if err := p.cfg.Upstream.ReportDesktopEvent(a, upstream.CampaignDesktopContentCalendarEvent()); err != nil {
			return "", fmt.Errorf("桌面判据上报: %w", err)
		}
		cur, _ = p.campaignPoll(a, ref, target, campaignSlowPoll, campaignSlowGap)
	case "chat":
		need := target - cur
		if need > int64(campaignMaxEvents) {
			need = int64(campaignMaxEvents)
		}
		for i := int64(0); i < need; i++ {
			if interrupted := sleepAbortable(campaignFastGap, p.taskAbortCh()); interrupted {
				return fmt.Sprintf("已取消（完成 %d/%d 次，进度保留待下轮续报）", i, need), nil
			}
			conv := fmt.Sprintf("wb2api-camp-%d-%d", time.Now().UnixMilli(), i)
			if err := p.cfg.Upstream.ReportMPEventEnv(a, ref.Activity.Page, upstream.CampaignChatEvent(conv)); err != nil {
				return fmt.Sprintf("完成 %d/%d 次后中断: %v", i, need, err), nil
			}
			cur, _ = p.campaignPoll(a, ref, target, campaignFastPoll, campaignFastGap)
			if cur >= target {
				break
			}
		}
	case "expert":
		expertID, expertName := ref.Task.ExpertID, ref.Task.ExpertName
		if expertID == "" {
			if experts, err := p.cfg.Upstream.MarketExpertList(a, "agent"); err == nil && len(experts) > 0 {
				expertID = experts[0].ExpertID
				expertName = experts[0].DisplayNameZH
			}
		}
		if expertID == "" {
			return "取不到真实专家 ID（不伪造专家身份）", nil
		}
		conv := fmt.Sprintf("wb2api-camp-%d", time.Now().UnixMilli())
		if err := p.cfg.Upstream.ReportMPEventEnv(a, ref.Activity.Page,
			upstream.CampaignExpertEvents(expertID, expertName, conv)...); err != nil {
			return "", fmt.Errorf("专家判据上报: %w", err)
		}
		cur, _ = p.campaignPoll(a, ref, target, campaignFastPoll, campaignFastGap)
	case "skill":
		if err := p.cfg.Upstream.ReportMPEventEnv(a, ref.Activity.Page,
			upstream.CampaignSkillEvents(ref.Task.SkillID, ref.Task.SkillName)...); err != nil {
			return "", fmt.Errorf("技能判据上报: %w", err)
		}
		cur, _ = p.campaignPoll(a, ref, target, campaignFastPoll, campaignFastGap)
	default:
		return fmt.Sprintf("未知活动任务类型 %q，跳过", ref.Task.Kind), nil
	}

	if cur < target {
		cur, _ = p.campaignPoll(a, ref, target, campaignSlowPoll, campaignSlowGap)
	}
	rows, err = p.campaignTasks(a, ref.Activity.Code)
	if err != nil {
		return fmt.Sprintf("判据已上报，但回读失败（%v），下轮重试", err), nil
	}
	row = rows[ref.Task.Task]
	credit := row.Credit
	if credit <= 0 {
		credit = ref.Task.Credit
	}
	if !row.Completed() {
		return fmt.Sprintf("判据延迟未落库（%d/%d），下轮自动补领", cur, target), nil
	}
	return p.claimCampaignTask(a, ref, credit)
}

// campaignPoll 轮询单任务进度；返回 (progress, completed)。
// 中断信号在条目间隙生效（队列取消时提前退出，进度保留）。
func (p *Panel) campaignPoll(a *auth.Auth, ref campaignTaskRef, target int64, tries int, gap time.Duration) (int64, bool) {
	var cur int64
	for i := 0; i < tries; i++ {
		if sleepAbortable(gap, p.taskAbortCh()) {
			return cur, false
		}
		rows, err := p.campaignTasks(a, ref.Activity.Code)
		if err != nil {
			continue
		}
		row, ok := rows[ref.Task.Task]
		if !ok {
			continue
		}
		cur = row.Progress
		if row.Completed() {
			if cur < target {
				cur = target
			}
			return cur, true
		}
	}
	return cur, false
}

// claimCampaignTask 领活动奖励并回读 claimed_at。
// claim 成功但未回读到 claimed_at 只算「未确认」：标记去重，本轮不重复 POST。
func (p *Panel) claimCampaignTask(a *auth.Auth, ref campaignTaskRef, credit int64) (string, error) {
	code := campaignTaskCode(ref.Activity.Code, ref.Task.Task)
	if p.claimPending(a.UID, code) {
		// 已提交未确认：不再 POST，但回读一次——上游落账后自动清除标记。
		if rows, err := p.campaignTasks(a, ref.Activity.Code); err == nil {
			if row, ok := rows[ref.Task.Task]; ok && row.Claimed() {
				p.clearClaimUnconfirmed(a.UID, code)
				return fmt.Sprintf("已领取活动奖励（+%d 积分）", credit), nil
			}
		}
		return "领奖已提交，等待入账确认（本轮不重复提交）", nil
	}
	err := p.cfg.Upstream.CampaignClaim(a, ref.Activity.Code, ref.Task.Task)
	if err != nil && panelAuthErr(err) && p.refreshAccountToken(a, err) {
		err = p.cfg.Upstream.CampaignClaim(a, ref.Activity.Code, ref.Task.Task)
	}
	if err != nil {
		return "", fmt.Errorf("活动领奖: %w", err)
	}
	for i := 0; i < campaignClaimPoll; i++ {
		if i > 0 {
			if sleepAbortable(campaignClaimGap, p.taskAbortCh()) {
				return "已取消（领奖已提交，待下轮回读确认）", nil
			}
		}
		rows, rerr := p.campaignTasks(a, ref.Activity.Code)
		if rerr != nil {
			continue
		}
		if row, ok := rows[ref.Task.Task]; ok && row.Claimed() {
			p.clearClaimUnconfirmed(a.UID, code)
			if credit <= 0 {
				credit = row.Credit
			}
			return fmt.Sprintf("已领取活动奖励（+%d 积分）", credit), nil
		}
	}
	p.markClaimUnconfirmed(a.UID, code)
	return "领奖已提交但未回读到 claimed_at，下轮确认（不重复提交）", nil
}
