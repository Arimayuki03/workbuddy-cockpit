// market.go 专家市场检索 + 任务深链解析（v1.25.0 G2 吸收件）。
//
// 来源：L0NE-6/WorkBuddy-Daily v50 的运行时发现逻辑——
//  1. 读成长任务列表，对每个未完成的 expert_use_* 任务取 jump_url；
//  2. 只接受 workbuddy://experts?expertId=ex_xxx 这一种深链（外链必须包在
//     deeplink 参数里，且 host 属于 workbuddy.cn / codebuddy.cn / tencent.com）；
//  3. 用 keyword 分页搜索市场，按 expert_id 精确落位；无 ID 时只接受唯一同名结果；
//  4. 搜不到时调用方降级到常量表——本文件不把「市场不可用」升级成失败。
//
// 为什么必须动态解析：任务换榜时硬编码的 expert_id 会静默失效，而 jump_url 是
// 服务端当前下发的权威指向（docs/DESIGN-v1.25.0-growth-coverage.md D6）。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"workbuddy2api/internal/auth"
)

// expertIDRe 专家 ID 形态（与 L0NE 脚本逐字一致）。
var expertIDRe = regexp.MustCompile(`^ex_[A-Za-z0-9]{6,64}$`)

// trustedJumpHostSuffixes 允许从 http(s) 跳转链接里提取 deeplink 的 host 后缀。
var trustedJumpHostSuffixes = []string{"workbuddy.cn", "codebuddy.cn", "tencent.com"}

// ValidExpertID 报告 id 是否形如 ex_[A-Za-z0-9]{6,64}。
func ValidExpertID(id string) bool { return expertIDRe.MatchString(strings.TrimSpace(id)) }

// ParseExpertIDFromJumpURL 从任务 jump_url 中安全提取专家 ID。
// 返回 "" 表示无法识别（调用方应降级常量表，不得把任意 URL 内嵌值当 ID）。
//
// 接受三种形态：
//   - 直接深链 workbuddy://experts?expertId=ex_xxx
//   - deeplink=<URL 编码的深链>
//   - http(s)://<可信域>/...?deeplink=<URL 编码的深链>
func ParseExpertIDFromJumpURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if id := parseExpertDeeplink(raw); id != "" {
		return id
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "deeplink=") {
		return parseSingleDeeplinkQuery(raw)
	}
	outer, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	scheme := strings.ToLower(outer.Scheme)
	if scheme != "http" && scheme != "https" && scheme != "workbuddy" {
		return ""
	}
	if outer.Fragment != "" {
		return ""
	}
	if scheme == "http" || scheme == "https" {
		host := strings.ToLower(strings.TrimSuffix(outer.Hostname(), "."))
		if !trustedJumpHost(host) {
			return ""
		}
	}
	return parseSingleDeeplinkQuery(outer.RawQuery)
}

// parseSingleDeeplinkQuery 从 query 中取恰好一个 deeplink 并解析。
func parseSingleDeeplinkQuery(rawQuery string) string {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return ""
	}
	links := values["deeplink"]
	if len(links) != 1 {
		return ""
	}
	return parseExpertDeeplink(links[0])
}

// parseExpertDeeplink 严格解析 workbuddy://experts?expertId=ex_xxx。
func parseExpertDeeplink(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	if !strings.EqualFold(parsed.Scheme, "workbuddy") ||
		!strings.EqualFold(parsed.Host, "experts") ||
		(parsed.Path != "" && parsed.Path != "/") ||
		parsed.Fragment != "" {
		return ""
	}
	values := parsed.Query()
	ids := values["expertId"]
	if len(ids) != 1 {
		return ""
	}
	id := strings.TrimSpace(ids[0])
	if !ValidExpertID(id) {
		return ""
	}
	return id
}

// trustedJumpHost 判定 host 是否为可信跳转域（等于后缀或为其子域）。
func trustedJumpHost(host string) bool {
	for _, suffix := range trustedJumpHostSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// marketExpertList 专家市场列表底座（MarketExpertList / SearchMarketExpert 共用）。
// 请求头与调用形态对齐桌面客户端（现有 expert_5 / Expert_lighthouse 实测可用）。
func (c *Client) marketExpertList(a *auth.Auth, body map[string]any) ([]MarketExpert, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost,
		c.chatBase(a)+"/portal/operation-platform/market/expert/list", bytes.NewReader(raw))
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
		Experts []marketExpertRow `json:"experts"`
		Items   []marketExpertRow `json:"items"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("expert list parse: %w", err)
	}
	rows := out.Experts
	if len(rows) == 0 {
		rows = out.Items
	}
	list := make([]MarketExpert, 0, len(rows))
	for _, r := range rows {
		list = append(list, r.toMarketExpert())
	}
	return list, nil
}

// marketExpertRow 市场响应行的宽容视图：不同版本用 expert_id / source_id / id，
// 名称字段也有 display_name_zh / name 两套，版本可能藏在 _meta.version。
type marketExpertRow struct {
	ExpertID      string `json:"expert_id"`
	SourceID      string `json:"source_id"`
	ID            string `json:"id"`
	ExpertType    string `json:"expert_type"`
	DisplayNameZH string `json:"display_name_zh"`
	DisplayName   string `json:"display_name"`
	Name          string `json:"name"`
	ProfessionZH  string `json:"profession_zh"`
	Profession    string `json:"profession"`
	Version       string `json:"version"`
	Categories    []any  `json:"categories"`
	Meta          struct {
		Version string `json:"version"`
	} `json:"_meta"`
}

func (r marketExpertRow) toMarketExpert() MarketExpert {
	id := firstNonEmpty(r.ExpertID, r.SourceID, r.ID)
	name := firstNonEmpty(r.DisplayNameZH, r.DisplayName, r.Name)
	profession := firstNonEmpty(r.ProfessionZH, r.Profession, name)
	version := firstNonEmpty(r.Version, r.Meta.Version)
	return MarketExpert{
		ExpertID: id, ExpertType: r.ExpertType,
		DisplayNameZH: name, ProfessionZH: profession,
		Version: version, Categories: r.Categories,
	}
}

// SearchMarketExpert 按 keyword 分页搜索专家市场，返回与 expectedID 精确匹配的条目。
// expectedID 为空时只接受唯一同名结果（L0NE 口径）；最多 3 页，每页 20 条。
// 返回 (条目, 是否命中, 错误)：市场不可用/无关键词视为未命中而非致命错误，
// 调用方据此降级常量表（设计 §9「市场查不到时降级到常量表，不报错」）。
func (c *Client) SearchMarketExpert(a *auth.Auth, keyword, expectedID string) (MarketExpert, bool, error) {
	keyword = strings.TrimSpace(keyword)
	expectedID = strings.TrimSpace(expectedID)
	if keyword == "" {
		return MarketExpert{}, false, nil
	}
	if expectedID != "" && !ValidExpertID(expectedID) {
		return MarketExpert{}, false, nil
	}
	fallback := map[string]MarketExpert{}
	for page := 1; page <= 3; page++ {
		experts, err := c.marketExpertList(a, map[string]any{
			"page": page, "page_size": 20, "keyword": keyword,
		})
		if err != nil {
			if page == 1 {
				return MarketExpert{}, false, nil // 市场不可用：降级，不报错
			}
			break
		}
		if len(experts) == 0 {
			break
		}
		for _, e := range experts {
			if !ValidExpertID(e.ExpertID) {
				continue
			}
			if expectedID != "" && e.ExpertID == expectedID {
				return normalizeExpert(e, keyword), true, nil
			}
			if expectedID == "" && (e.DisplayNameZH == keyword || e.ProfessionZH == keyword) {
				fallback[e.ExpertID] = e
			}
		}
		if len(experts) < 20 {
			break
		}
	}
	if expectedID == "" && len(fallback) == 1 {
		for _, e := range fallback {
			return normalizeExpert(e, keyword), true, nil
		}
	}
	return MarketExpert{}, false, nil
}

// normalizeExpert 补齐市场条目的空字段（名称/职业/类型）。
func normalizeExpert(e MarketExpert, keyword string) MarketExpert {
	if e.ExpertType == "" {
		e.ExpertType = "agent"
	}
	if e.DisplayNameZH == "" {
		e.DisplayNameZH = keyword
	}
	if e.ProfessionZH == "" {
		e.ProfessionZH = keyword
	}
	return e
}

// firstNonEmpty 返回第一个非空字符串（行解析用）。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
