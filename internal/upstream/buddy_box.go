// buddy_box.go growth 域「Buddy 盲盒」接口：额度查询 / 开盒。
// 逆向来源：88lin/workbuddy-auto-signin v1.8.3 + L0NE-6/WorkBuddy-Daily v50
// （两实现独立收敛到同一端点与同一前置约束，见 docs/DESIGN-v1.25.0-growth-coverage.md G1）。
//
//	GET  {chatBase}/v2/activity/growth/buddy/quota → data.affordable / data.max_open_count
//	POST {chatBase}/v2/activity/growth/buddy/open  {"count":N,"client_token":"<uuid>"}
//	     → data.buddy | data.name | data.buddies | data.results
//
// 服务端状态约束（违反会静默丢奖励，HTTP 200 但无 credit）：
//   - 仅当 travel state == idle 时才能开盒（调用方判定闸门，本文件不做状态推断）；
//   - 开盒会覆盖 travel record，必须排在「领旅行礼物」之后、「派出」之前；
//   - 服务端不惩罚开满：count = min(affordable, max_open_count)，再夹到 5。
package upstream

import (
	"encoding/json"
	"net/http"

	"workbuddy2api/internal/auth"
)

// Buddy 盲盒路径（v2 前缀，与服务端 activity 域其他端点不同：quota/open 在 /v2 下）。
const (
	buddyQuotaPath = "/v2/activity/growth/buddy/quota"
	buddyOpenPath  = "/v2/activity/growth/buddy/open"
)

// buddyOpenMaxCount 单次开盒的服务端安全上限（L0NE 实现额外夹 5，两实现均未报错）。
const buddyOpenMaxCount = 5

// BuddyQuota 盲盒额度回执。
// Affordable 为当前能量可开次数；MaxOpenCount 为单次请求允许的最大 count
// （服务端缺省回落 1——按设计文档口径，不能把缺省当作不限）。
type BuddyQuota struct {
	Affordable   int64 `json:"affordable"`
	MaxOpenCount int64 `json:"max_open_count"`
	Balance      int64 `json:"balance"` // 当前能量（失败展示用）
}

// OpenCount 计算本次应开盒数量：min(affordable, max_open_count, 5)。
// max_open_count <= 0 时按服务端默认 1；affordable <= 0 返回 0（调用方跳过）。
func (q *BuddyQuota) OpenCount() int {
	if q == nil || q.Affordable <= 0 {
		return 0
	}
	max := q.MaxOpenCount
	if max <= 0 {
		max = 1
	}
	n := q.Affordable
	if n > max {
		n = max
	}
	if n > buddyOpenMaxCount {
		n = buddyOpenMaxCount
	}
	return int(n)
}

// BuddyQuota 查询盲盒额度（只读）。无 Buddy / 活动未开启时上游返回业务错误，
// 调用方按「跳过」处理，不当失败。
func (c *Client) BuddyQuota(a *auth.Auth) (*BuddyQuota, error) {
	data, err := c.growthJSON(a, http.MethodGet, buddyQuotaPath, nil)
	if err != nil {
		return nil, err
	}
	var q BuddyQuota
	if err := json.Unmarshal(data, &q); err != nil {
		return nil, err
	}
	return &q, nil
}

// BuddyOpenResult 开盒回执。不同版本/账号形态下发字段不同（buddy / buddies /
// results），这里统一收敛成名字列表，仅用于日志与面板展示。
type BuddyOpenResult struct {
	Count int
	Names []string
}

// BuddyOpen 开盲盒 count 次；clientToken 为空时自动生成新幂等键。
// 返回的错误为上游业务错误（能量不足/旅行非 idle 等由调用方闸门保证）。
func (c *Client) BuddyOpen(a *auth.Auth, count int, clientToken string) (*BuddyOpenResult, error) {
	if count <= 0 {
		return &BuddyOpenResult{}, nil
	}
	if clientToken == "" {
		clientToken = growthClientToken("buddy-open")
	}
	data, err := c.growthJSON(a, http.MethodPost, buddyOpenPath,
		map[string]any{"count": count, "client_token": clientToken})
	if err != nil {
		return nil, err
	}
	return parseBuddyOpen(data), nil
}

// parseBuddyOpen 宽松解析开盒回执：依次尝试 results / buddies / buddy / name。
func parseBuddyOpen(data json.RawMessage) *BuddyOpenResult {
	res := &BuddyOpenResult{Count: 1}
	var resp struct {
		Name    string          `json:"name"`
		Buddy   json.RawMessage `json:"buddy"`
		Buddies []buddyBoxItem  `json:"buddies"`
		Results []struct {
			Instance buddyBoxItem `json:"instance"`
			Template buddyBoxItem `json:"template"`
		} `json:"results"`
	}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &resp)
	}
	add := func(item buddyBoxItem) {
		if item.Name != "" {
			res.Names = append(res.Names, item.Name)
		}
	}
	for _, r := range resp.Results {
		if r.Instance.Name != "" {
			add(r.Instance)
			continue
		}
		add(r.Template)
	}
	if len(res.Names) == 0 {
		for _, b := range resp.Buddies {
			add(b)
		}
	}
	if len(res.Names) == 0 && len(resp.Buddy) > 0 && string(resp.Buddy) != "null" {
		var b buddyBoxItem
		if json.Unmarshal(resp.Buddy, &b) == nil {
			add(b)
		}
	}
	if len(res.Names) == 0 && resp.Name != "" {
		res.Names = append(res.Names, resp.Name)
	}
	if len(resp.Results) > 0 {
		res.Count = len(resp.Results)
	} else if len(resp.Buddies) > 0 {
		res.Count = len(resp.Buddies)
	}
	return res
}

// buddyBoxItem 盲盒奖励的通用条目（instance/template/buddies 共用字段子集）。
type buddyBoxItem struct {
	Name   string `json:"name"`
	Rarity string `json:"rarity"`
}
