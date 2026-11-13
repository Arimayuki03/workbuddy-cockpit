// skill_share.go growth 域「分享技能给好友」接口（v1.25.0 G3 吸收件）。
//
// 来源：L0NE-6/WorkBuddy-Daily v50（5.9.3 客户端实证）：
//  1. 市场技能列表 POST {chatBase}/v2/operation-platform/market/skill/list
//  2. 创建真实分享渠道 POST {openBase}/api/v2/open-platform/asset-shares/c2c/channels
//  3. 上报 2 条 skill_share_channel_action（clipboard click → success）
//  4. 回读进度，completed 才 claim
//
// 注意：这是全仓库唯一访问 open.workbuddy.cn 的端点。它走与其它上游请求相同的
// 账号出口线路（doJSONAuth），不放宽 TLS/代理安全口径。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

const (
	marketSkillListPath = "/v2/operation-platform/market/skill/list"
	skillShareChannels  = "/api/v2/open-platform/asset-shares/c2c/channels"
	defaultOpenBaseCN   = "https://open.workbuddy.cn"
)

// openBase 返回开放平台域（realm 感知：global 无该活动，调用方已门控；
// 这里保持 CN 默认值，避免把 global 域名拼进不存在的接口）。
func (c *Client) openBase(a *auth.Auth) string {
	if c.OpenBaseCN != "" {
		return c.OpenBaseCN
	}
	return defaultOpenBaseCN
}

// MarketSkill 市场技能条目（share/create 所需字段子集）。
type MarketSkill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// MarketSkillList 拉取市场技能列表（page=1，20 条）。
// 返回空列表不是错误：skill_share 任务要求必须有真实市场技能，调用方据此跳过。
func (c *Client) MarketSkillList(a *auth.Auth) ([]MarketSkill, error) {
	raw, err := json.Marshal(map[string]any{"page": 1, "page_size": 20})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+marketSkillListPath, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", desktopUA)
	req.Header.Set("X-Domain", c.chatBase(a))
	req.Header.Set("X-Product", "SaaS")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	data, err := c.doJSONAuth(a, req)
	if err != nil {
		return nil, err
	}
	var out struct {
		Skills []struct {
			SkillID       string `json:"skill_id"`
			ID            string `json:"id"`
			DisplayNameZH string `json:"display_name_zh"`
			DisplayNameEN string `json:"display_name_en"`
			Name          string `json:"name"`
			DescriptionZH string `json:"description_zh"`
			Description   string `json:"description"`
			DescriptionEN string `json:"description_en"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("skill list parse: %w", err)
	}
	list := make([]MarketSkill, 0, len(out.Skills))
	for _, s := range out.Skills {
		id := firstNonEmpty(s.SkillID, s.ID)
		if id == "" {
			continue
		}
		name := firstNonEmpty(s.DisplayNameZH, s.Name, s.DisplayNameEN)
		if name == "" {
			continue
		}
		desc := firstNonEmpty(s.DescriptionZH, s.Description, s.DescriptionEN)
		if len(desc) > 60 {
			desc = desc[:60]
		}
		list = append(list, MarketSkill{ID: id, Name: name, Description: desc})
	}
	return list, nil
}

// SkillShareChannel 分享渠道创建回执（官方响应 data 子集）。
type SkillShareChannel struct {
	ShareCode string
	ChannelID string
	Link      string
}

// CreateSkillShareChannel 创建真实技能分享渠道（官方 5.9.3 客户端请求体）。
// asset_ref 是 JSON 字符串而非对象——官方客户端逐字如此，改成对象会被拒。
func (c *Client) CreateSkillShareChannel(a *auth.Auth, skill MarketSkill) (*SkillShareChannel, error) {
	if strings.TrimSpace(skill.ID) == "" {
		return nil, fmt.Errorf("skill_share: market skill id empty")
	}
	assetRef, err := json.Marshal(map[string]string{"asset_id": skill.ID})
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"asset_kind":   "skill",
		"asset_source": "market",
		"asset_ref":    string(assetRef),
		"ways":         []string{"link", "wxacode", "wxapath"},
		"extra": map[string]any{
			"name": skill.Name, "description": skill.Description,
			"shareKey": skill.ID, "marketplaceSource": "builtin-market",
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.openBase(a)+skillShareChannels, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("X-User-Id", a.UID)
	req.Header.Set("X-Domain", c.chatBase(a))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", desktopUA)
	data, err := c.doJSONAuth(a, req)
	if err != nil {
		return nil, err
	}
	var out struct {
		ShareCode string `json:"share_code"`
		ChannelID string `json:"channel_id"`
		Ways      []struct {
			ShareWay    string `json:"share_way"`
			ResourceRef string `json:"resource_ref"`
		} `json:"ways"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("skill share parse: %w", err)
	}
	ch := &SkillShareChannel{ShareCode: out.ShareCode, ChannelID: out.ChannelID}
	for _, w := range out.Ways {
		if w.ShareWay == "link" {
			ch.Link = w.ResourceRef
			break
		}
	}
	if ch.ShareCode == "" || ch.Link == "" {
		return nil, fmt.Errorf("skill_share: channel missing share_code or link")
	}
	return ch, nil
}

// SkillShareChannelEvents 构造官方渠道事件两条（click → success）。
// panelType=clipboard：真实客户端在「复制链接」面板上依次上报。
func SkillShareChannelEvents(skillID, skillName, shareCode string) []DesktopEvent {
	now := nowUnixMilli()
	mk := func(action string) DesktopEvent {
		return DesktopEvent{
			"eventCode": "skill_share_channel_action",
			"skillId":   skillID, "skillName": skillName, "skillType": "market",
			"action": "link", "panelAction": action, "panelType": "clipboard",
			"isSuccessful": true, "cost": 0, "id": shareCode, "actionAt": now,
		}
	}
	return []DesktopEvent{mk("click"), mk("success")}
}

// nowUnixMilli 当前毫秒时间戳（包内共享；集中一处便于日志/事件对齐）。
func nowUnixMilli() int64 { return time.Now().UnixMilli() }
