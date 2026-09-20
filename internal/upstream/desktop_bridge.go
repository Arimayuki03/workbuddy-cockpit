// desktop_bridge.go panel 移植底座桥（desktop.go / tasks.go / school.go / blackcat.go
// 快照拷贝适配产物）。主仓库 client.go/report.go/headers.go 与 panel client.go 存在
// 底座差异，这里补齐**主仓库缺失的最小能力**，均为 panel 侧原样语义：
//
//   - WebBaseCN / webBase：panel 的官网域字段与 realm 感知访问器（tasks.ClaimReward、
//     desktop.ReportWebEvent 依赖）。主仓库 Client 无该字段——新增导出字段 + 访问器，
//     不改主仓库 client.go 既有行为（缺省 CN 官网域，global 账号走国际站）。
//   - ReportChatActivityModel：panel report.go 的带模型活跃上报（blackcat.RunNightChats
//     依赖 glm-5.2 形状；主仓库 report.go 只有固定 deepseek 形状的 ReportChatActivity）。
//   - growthJSONMP / mpPlatform / mpReportPath：panel 的小程序口径请求底座
//     （tasks.ListTasksMP/AcceptTasksMP/ClaimRewardMP 与 school.ReportMPEvent 依赖）。
//   - deriveAccountStableID 对齐注记：desktop.deriveID 与主仓库 headers.go 的
//     deriveAccountStableID 同构（sha256→36hex），但盐前缀不同（panel 口径），保留
//     desktop.go 原样实现不动主仓库 headers.go。
package upstream

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"workbuddy2api/internal/auth"
)

// WebBaseCN 官网（workbuddy.cn）域：字段本体定义在 client.go 的 Client 结构体
// （panel 同名字段 parity）；这里只放访问器。
const defaultWebBaseCN = "https://www.workbuddy.cn"

// webBase 返回官网域（任务领奖类接口；未注入时回落默认）。
// realm 感知：global 账号切国际站 workbuddy.ai，CN 用 workbuddy.cn。
func (c *Client) webBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return defaultGlobalBase
	}
	if c.WebBaseCN != "" {
		return c.WebBaseCN
	}
	return defaultWebBaseCN
}

// ReportChatActivityModel 同 ReportChatActivity，但可指定上报携带的模型：供「体验某模型」
// 类任务对齐实际模型（如 Model_chat_GLM5.2 需 requestModelId=glm-5.2 与独立 requestID）。
// panel report.go 同名方法快照拷贝；主仓库 report.go 的 chatRequestEvent 形状与 panel
// 一致，仅落点为 RootRequestID=requestID（panel 语义），不改 report.go 既有 ReportChatActivity。
func (c *Client) ReportChatActivityModel(a *auth.Auth, conversationID, requestID, modelID, modelName string) error {
	if requestID == "" {
		requestID = conversationID
	}
	if modelID == "" {
		modelID = "deepseek-v4-flash"
	}
	if modelName == "" {
		modelName = modelID
	}
	now := time.Now().UnixMilli()
	ev := chatRequestEvent{
		EventCode:             "chat_request_send",
		Timestamp:             now,
		ReportDelay:           0,
		Mode:                  "craft",
		ConversationID:        conversationID,
		RequestID:             requestID,
		InputLength:           12,
		RequestModelID:        modelID,
		RequestModelName:      modelName,
		IsPlan:                false,
		IsAutoExecuteTerminal: false,
		IsAutoModify:          false,
		CodebaseEnable:        false,
		MaxToken:              0,
		MaxSteps:              0,
		Temperature:           0,
		MaxRetries:            0,
		MentionContexts:       []any{},
		KnowledgeID:           []any{},
		KnowledgeName:         []any{},
		CodebaseID:            "",
		MentionContextCount:   0,
		Command:               "",
		ExpertID:              "",
		RecommendID:           "",
		SkillID:               "",
		SkillCount:            0,
		TotalCount:            0,
		FileURI:               "",
		PresentAt:             now,
		TraceID:               "",
		RootRequestID:         requestID,
		ParentConversationID:  conversationID,
		AgentName:             "default",
		AgentType:             "conversation",
		UserID:                a.UID,
	}
	raw, err := json.Marshal([]chatRequestEvent{ev})
	if err != nil {
		return err
	}
	_, err = c.billingJSON(a, http.MethodPost, reportPath, json.RawMessage(raw))
	return err
}

// mpPlatform 小程序口径头值：小程序限定任务（Sequential_Tasks_1 / school_season）
// 的列表下发、accept、claim 全链路要求 X-Client-Platform: miniprogram。
const mpPlatform = "miniprogram"

// mpReportPath 小程序埋点上报通道（同 reportPath 的 /v2/report，独立常量保持
// panel school.go 快照原样语义）。
const mpReportPath = "/v2/report"

// growthJSONMP 发 growth 域请求（小程序口径：BillingHeaders 之上叠加
// X-Client-Platform: miniprogram）并解信封。panel tasks.go 同名方法快照拷贝；
// 错误语义与 doJSON 一致：HTTP 非 2xx / 业务 code != 0 → *Error。
func (c *Client) growthJSONMP(a *auth.Auth, method, path string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.chatBase(a)+path, rdr)
	if err != nil {
		return nil, err
	}
	c.BillingHeaders(req, a)
	req.Header.Set("X-Client-Platform", mpPlatform)
	return c.doJSON(req)
}
