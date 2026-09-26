// blackcat.go 夜猫子任务（black_cat）+ 昨日漏签检查。
// 来源：panel internal/upstream/blackcat.go 快照拷贝 + 适配（2026-09-21）。
// 主仓库漂移处理：
//   - ClaimGift / ClaimCompensation / UseMakeupCard 与主仓库 growth_bonus.go 重复，
//     已删除重复方法（主仓库版本语义一致，调用方直接用既有实现）；
//   - HeatmapYesterdayMissed 主仓库已有等价 GrowthHeatmap + HeatmapDayScore /
//     GrowthYesterdayDate（growth_bonus.go），本文件不再重复实现；
//   - panel 版 RunNightChats 依赖 ReportChatActivityModel（panel report.go 私有底座），
//     主仓库 report.go 只有 ReportChatActivity——该底座能力在 desktop_bridge.go 补齐。
//
// 判据（WorkBuddy-Daily 项目实测口径 + 本网关验证）：black_cat 要求在
// **23:00–08:00（CST，Asia/Shanghai——窗口按上游服务端时区计数，固定 +8）窗口内**
// 完成 3 次 glm-5.2 对话并上报 chat 事件链；
// 窗口外行为不计分。真实对话走网关既有 ChatStream（glm-5.2），事件链用
// ReportChatActivityModel（chat_5 同款上报形状）。
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"workbuddy2api/internal/auth"
)

// InNightWindow 当前是否处于夜猫子计数窗口（23:00–08:00，上游按 CST/Asia/Shanghai
// 口径计数——与 growth_bonus.go cstShanghai / scheduler travelDay 的固定 +8 先例统一）。
// 此前 now.Hour() 依赖进程本地时区，非 CST 主机（裸二进制部署）窗口错位
// （23-08 本地 = 07-16 CST），夜猫对话全部不计分。
func InNightWindow(now time.Time) bool {
	h := now.In(cstShanghai).Hour()
	return h >= 23 || h < 8
}

// BlackcatNeed 查 black_cat 任务剩余差额（需要再完成几次对话）。
// 任务不存在返回 0（无可做）；拉取失败返回错误。
func (c *Client) BlackcatNeed(a *auth.Auth) (int64, error) {
	tasks, err := c.ListTasks(a)
	if err != nil {
		return 0, err
	}
	for _, t := range tasks {
		if t.TaskCode == "black_cat" {
			if t.Claimed || t.Current >= t.Target {
				return 0, nil
			}
			return t.Target - t.Current, nil
		}
	}
	return 0, nil
}

// RunNightChats 夜猫子：发 need 次 glm-5.2 真实对话（读干流）并上报事件链。
// 返回成功次数。对话内容极短（1+1），消耗可忽略。
func (c *Client) RunNightChats(a *auth.Auth, need int) (int64, error) {
	var ok int64
	for i := 0; i < need; i++ {
		body, _ := json.Marshal(map[string]any{
			"model":    "glm-5.2",
			"messages": []map[string]any{{"role": "user", "content": "1+1等于几？直接回答。"}},
			"stream":   true,
		})
		rc, status, respBody, err := c.ChatStreamContext(context.Background(), a, body, "", ChatMeta{})
		if err != nil || status >= 400 {
			if rc != nil {
				rc.Close()
			}
			return ok, fmt.Errorf("第 %d 次对话失败: http=%d err=%v body=%.120s", i+1, status, err, respBody)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
		rc.Close()
		if err := c.ReportChatActivityModel(a, fmt.Sprintf("wb2api-night-%d-%d", time.Now().UnixMilli(), i), "", "glm-5.2", "GLM-5.2"); err != nil {
			return ok, fmt.Errorf("第 %d 次上报失败: %w", i+1, err)
		}
		ok++
		time.Sleep(4 * time.Second)
	}
	return ok, nil
}
