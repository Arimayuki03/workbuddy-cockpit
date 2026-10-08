// profile.go Web 控制台账号资料拉取（panel f1496d0a 移植，issue #94：
// 用户在官网改名后免重登同步昵称）。
//
// GET {webBase}/console/account，Bearer + x-client-platform: web（与 ClaimReward
// 同一 Web 请求形态，实测无需 Web 会话 cookie）。
//
// 隐私边界（重要）：该接口响应包含手机号（phoneNumber）等个人敏感信息。本方法
// 只解析 nickname 与 uid（uid 仅做一致性核对），其余字段一概不解析、不落日志、
// 不透传——调用方也拿不到。
//
// 接线说明（只建能力不接线）：昵称同步进 balance_refresh 由后续提交单独完成，
// 避免与并行改动的文件冲突。预期口径（panel f1496d0a）：仅在面板手动「刷新」
// （balance_all 手动路径）时逐账号调用本方法同步昵称；后台余额定时器不调用
// 资料接口（逐账号失败静默跳过，不参与惩罚）。
package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"

	"workbuddy2api/internal/auth"
)

// FetchAccountProfile 拉取 Web 控制台账号资料并返回最新昵称。
// uid 与凭证不一致时报错（防串号，避免把 A 号昵称写到 B 号档案）；
// 网络/业务错误原样返回（*Error），调用方静默跳过即可，不应计入账号惩罚。
//
// 组合取舍：请求构建/发送复用 client.go 既有导出与非导出成员（webBase、doJSON、
// userAgent——同包可见，无需改 client.go），请求头口径取 ClaimReward 的 Web 形态
// （Bearer + x-client-platform: web + Origin/Referer 同源 + X-User-Id 等归属头），
// 而非 BillingHeaders 的 CLI 形态——/console/account 是 Web 控制台端点。
func (c *Client) FetchAccountProfile(a *auth.Auth) (string, error) {
	base := c.webBase(a)
	req, err := http.NewRequest(http.MethodGet, base+"/console/account", nil)
	if err != nil {
		return "", err
	}
	// Web 端请求头形状（对照 ClaimReward / 浏览器实际请求）：Origin/Referer 与
	// URL 同源，带 x-client-platform: web 标记来源端。
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", base)
	req.Header.Set("Referer", base+"/profile/account-settings")
	req.Header.Set("x-client-platform", "web")
	if ua := c.userAgent(a); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	}

	// doJSON 已解 apiEnvelope：非 2xx / 业务 code!=0 返回 *Error，成功返回 env.Data。
	// 敏感字段（手机号等）在 Data 里，但这里只 Unmarshal 下述两个键，其余即弃。
	data, err := c.doJSON(req)
	if err != nil {
		return "", err
	}
	var resp struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("profile parse: %w", err)
	}
	// 防串号：双方都非空才比对（老档案缺 uid / 上游缺 uid 时不误伤）。
	if resp.UID != "" && a.UID != "" && resp.UID != a.UID {
		return "", fmt.Errorf("profile uid mismatch: resp=%s auth=%s", resp.UID, a.UID)
	}
	return resp.Nickname, nil
}
