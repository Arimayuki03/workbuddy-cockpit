// security_ep.go 面板安全页三端点（manager 壳 security 页契约，web/lib/api.ts
// securityApi）：入站 IP 黑白名单读写、拦截访问日志、模型锁池只读视图。
//
// 规则持久化分工（两个持久层，各管各的）：
//   - IP 黑白名单 + 白名单模式 → iprules.SetRules 落盘 data/ip_rules.json
//     （面板热改的单一事实来源，启动读盘优先于 config 种子，见 iprules.go）；
//   - trusted_proxy_cidrs / trusted_proxy_hops → config.json 的 security 段
//     （config 级参数：trustedHops 参与 handler 侧客户端 IP 解析的装配语义，
//     非规则引擎输入；经 panel 既有 SaveConfig 闭包深合并写盘 + 校验，与
//     model-map 设置同一路径）。
//
// 回显口径（GET）：IP 规则取 RulesView()（落盘值，权威）；trusted_proxy 两参
// 数优先读 LoadConfig()（面板刚保存未重启也如实回显，与 UpstashSavedToken 同
// 一读盘哲学），读不到回落注入的运行值（main 经 Config 注入），再回落缺省
// （空表 + 1 跳 = 不信任转发头的安全默认）。
package panel

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"

	"workbuddy2api/internal/iputil"
	"workbuddy2api/internal/server"
)

// securityRulesPayload POST /api/security/rules 的请求体（与 GET rules 段同构，
// web/lib/types.ts SecurityRulesPayload 五字段契约）。
type securityRulesPayload struct {
	IPBlacklist     []string `json:"ip_blacklist"`
	IPWhitelist     []string `json:"ip_whitelist"`
	IPWhitelistMode bool     `json:"ip_whitelist_mode"`
	TrustedCIDRs    []string `json:"trusted_proxy_cidrs"`
	TrustedHops     int      `json:"trusted_proxy_hops"`
}

// handleSecurityGet GET /api/security（/panel/api/security 同前缀）：
// {rules: {五字段}, blocked_logs: [{ts, ip, path, reason}]}。
// blocked_logs 取拦截日志环最近 200 条（新→旧，前端倒序表直读）；ts 为 Unix 秒
// （前端 fmtDateTime 直接吃秒值，零值条目透传 0 由前端空态处理）。
func (p *Panel) handleSecurityGet(w http.ResponseWriter, r *http.Request) {
	logs := p.cfg.IPRules.BlockedLogs()
	// 最近 200 条（新→旧）：环快照本身是时间升序，从尾部倒着取即新→旧。
	out := make([]map[string]any, 0, len(logs))
	for i := len(logs) - 1; i >= 0 && len(out) < 200; i-- {
		out = append(out, map[string]any{
			"ts":     logs[i].TS.Unix(),
			"ip":     logs[i].IP,
			"path":   logs[i].Path,
			"reason": logs[i].Reason,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rules":        p.securityRulesView(),
		"blocked_logs": out,
	})
}

// handleSecuritySetRules POST /api/security/rules：body 同 rules 五字段，
// {errs: string[]}——errs 非空 = 校验未通过、整体未生效（前端逐条展示，
// 不清草稿）；errs 空 = 保存成功。响应恒 200（校验失败是业务结果不是传输错误，
// 与 errs 契约一体）。
//
// 保存顺序：先校验 trusted_proxy 参数（config 层，**不触碰任何状态**），再落
// IP 规则（iprules 层整体拒绝语义），最后写 config——任一步失败立即返回，
// 且失败点之前的步骤不产生副作用（半套用从根上不可能）。
func (p *Panel) handleSecuritySetRules(w http.ResponseWriter, r *http.Request) {
	var body securityRulesPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, loginBodyLimit)).Decode(&body); err != nil && r.ContentLength != 0 {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	// trusted_proxy_hops 归一：<1 按 1（与 config normalize 同口径；防 0/负值
	// 落盘后启动校验 fail-fast，面板改个参数把服务改挂）。
	if body.TrustedHops < 1 {
		body.TrustedHops = 1
	}
	if p.cfg.IPRules == nil {
		writeErr(w, http.StatusNotImplemented, "ip rules not available")
		return
	}
	// 1) trusted_proxy 参数先过 config 校验（ParseTrustedCIDRs fail-fast 语义，
	// 错误清单与 IP 规则同形——「条目: 原因」）。**前置到任何 SetRules 之前**：
	// 校验失败直接返回，不触碰 IP 规则（内存与磁盘都不动）——此前先落新规则
	// 再以空规则「回滚」会把旧黑白名单清空覆盖写盘（内存+磁盘双丢）。
	if tpErrs := validateTrustedProxy(body.TrustedCIDRs); len(tpErrs) > 0 {
		writeJSON(w, http.StatusOK, map[string]any{"errs": tpErrs})
		return
	}
	// 2) IP 规则整体校验 + 换快照 + 落盘（非法条目整体拒绝，errs 逐条）。
	errs, err := p.cfg.IPRules.SetRules(server.NewIPRuleSpec(body.IPBlacklist, body.IPWhitelist, body.IPWhitelistMode))
	if len(errs) > 0 || err != nil {
		// IP 规则非法：内存未换、磁盘未写（SetRules 整体拒绝），trusted_proxy
		// 也未落盘——直接把错误清单回显。
		writeJSON(w, http.StatusOK, map[string]any{"errs": errs})
		return
	}
	// 3) trusted_proxy 参数经 SaveConfig 深合并写回 config.json 的 security 段
	// （五字段整体镜像：IP 规则也写进 config 作为下次启动的种子——虽然启动读盘
	// 优先，config 里的值保持与生效规则一致，避免「配置页显示值」与实际漂移）。
	if p.cfg.SaveConfig != nil {
		raw := mustJSON(map[string]any{"security": map[string]any{
			"ip_blacklist":        body.IPBlacklist,
			"ip_whitelist":        body.IPWhitelist,
			"ip_whitelist_mode":   body.IPWhitelistMode,
			"trusted_proxy_cidrs": body.TrustedCIDRs,
			"trusted_proxy_hops":  body.TrustedHops,
		}})
		if _, err := p.cfg.SaveConfig(raw); err != nil {
			// IP 规则已生效（内存+落盘），config 写盘失败不回滚——提示即可，
			// 重启后 IP 规则从 ip_rules.json 恢复（权威层），trusted_proxy 回落旧值。
			writeJSON(w, http.StatusOK, map[string]any{
				"errs":  []string{},
				"error": "config persist: " + err.Error(),
			})
			return
		}
	}
	log.Printf("panel: security rules saved (bl=%d wl=%d wlmode=%v tp_cidrs=%d hops=%d)",
		len(body.IPBlacklist), len(body.IPWhitelist), body.IPWhitelistMode, len(body.TrustedCIDRs), body.TrustedHops)
	// 记住刚保存的 trusted_proxy 参数（GET 回显优先读它：SaveConfig/LoadConfig
	// 未注入的形态也如实回显；见 securityTrustedProxy 的取值顺序）。
	p.rememberTrustedProxy(body.TrustedCIDRs, body.TrustedHops)
	writeJSON(w, http.StatusOK, map[string]any{"errs": []string{}})
}

// securityRulesView 组装 GET 的 rules 段：IP 规则来自 RulesView()（落盘值），
// trusted_proxy 两参数来自 live config（loadSecurityTrustedProxy）。
func (p *Panel) securityRulesView() map[string]any {
	var view map[string]any
	if p.cfg.IPRules != nil {
		view = p.cfg.IPRules.RulesView()
	} else {
		view = map[string]any{"ip_blacklist": []string{}, "ip_whitelist": []string{}, "ip_whitelist_mode": false}
	}
	cidrs, hops := p.securityTrustedProxy()
	view["trusted_proxy_cidrs"] = cidrs
	view["trusted_proxy_hops"] = hops
	return view
}

// securityTrustedProxy 回显 trusted_proxy_cidrs / trusted_proxy_hops，取值
// 优先级：本进程最近一次面板保存的值 → LoadConfig 落盘值（面板刚保存未重启
// 也如实回显，与 UpstashSavedToken 同一读盘哲学）→ 注入的运行值（main 经
// Config 注入的 security 段解析产物）→ 缺省（空表 + 1 跳 = 不信任转发头）。
func (p *Panel) securityTrustedProxy() ([]string, int) {
	if cidrs, hops, ok := p.savedTrustedProxy(); ok {
		return cidrs, hops
	}
	if p.cfg.LoadConfig != nil {
		if cfg, err := p.cfg.LoadConfig(); err == nil {
			raw, mErr := json.Marshal(cfg)
			if mErr == nil {
				var probe struct {
					Security struct {
						TrustedProxyCIDRs []string `json:"trusted_proxy_cidrs"`
						TrustedProxyHops  int      `json:"trusted_proxy_hops"`
					} `json:"security"`
				}
				if json.Unmarshal(raw, &probe) == nil {
					if probe.Security.TrustedProxyHops < 1 {
						probe.Security.TrustedProxyHops = 1
					}
					if probe.Security.TrustedProxyCIDRs == nil {
						probe.Security.TrustedProxyCIDRs = []string{}
					}
					return probe.Security.TrustedProxyCIDRs, probe.Security.TrustedProxyHops
				}
			}
		}
	}
	// 注入的运行值（[]netip.Prefix → 字符串列表回显）；均未配置时缺省：
	// 空 CIDR 表（完全不信任转发头）+ 1 跳。
	cidrs := make([]string, 0, len(p.cfg.TrustedProxyCIDRs))
	for _, pfx := range p.cfg.TrustedProxyCIDRs {
		cidrs = append(cidrs, pfx.String())
	}
	hops := p.cfg.TrustedProxyHops
	if hops < 1 {
		hops = 1
	}
	return cidrs, hops
}

// rememberTrustedProxy 记录最近一次面板保存的 trusted_proxy 参数（Panel 常驻
// 进程内存小镜像；重启后回落 LoadConfig/运行值——config 已落盘，口径收敛）。
func (p *Panel) rememberTrustedProxy(cidrs []string, hops int) {
	p.secMu.Lock()
	defer p.secMu.Unlock()
	p.secTPCIDRs = append([]string(nil), cidrs...)
	p.secTPHops = hops
}

// savedTrustedProxy 读最近一次面板保存的 trusted_proxy 参数。
// 从未保存过返回 ok=false（回落 LoadConfig/运行值）。
func (p *Panel) savedTrustedProxy() ([]string, int, bool) {
	p.secMu.Lock()
	defer p.secMu.Unlock()
	if p.secTPHops < 1 {
		return nil, 0, false // 从未保存（零值）→ 走回落链
	}
	out := append([]string(nil), p.secTPCIDRs...)
	return out, p.secTPHops, true
}

// validateTrustedProxy 校验 trusted_proxy_cidrs（ParseTrustedCIDRs 归一化，
// 非法条目逐条进清单——与 IP 规则的 errs 同形，前端无差别展示）。
// trusted_proxy_hops 的归一（<1 → 1）已在 handler 入口完成。
func validateTrustedProxy(cidrs []string) []string {
	if len(cidrs) == 0 {
		return nil
	}
	_, errs := iputil.ParseCIDRs(cidrs)
	return errs
}

// handleSecurityModelLocks GET /api/security/model_locks：
// {locks: [ModelLockRow...]}——pool.ModelLockView() 的只读透传。pool 为 nil 时
// 返回空数组（测试/最小装配形态）。unlock_at / fully_unlock_at 转 Unix 秒
// （前端拿秒值做剩余时间算术，time.Time 的 RFC3339 序列化会让算术产出 NaN；
// 零值转 0，前端按空态处理）。
func (p *Panel) handleSecurityModelLocks(w http.ResponseWriter, r *http.Request) {
	rows := []map[string]any{}
	if p.cfg.Pool != nil {
		for _, row := range p.cfg.Pool.ModelLockView() {
			rows = append(rows, map[string]any{
				"model":           row.Model,
				"realm":           row.Realm,
				"state":           row.State,
				"servable":        row.Servable,
				"total":           row.Total,
				"locked":          row.Locked,
				"unlock_at":       unixSec(row.UnlockAt),
				"fully_unlock_at": unixSec(row.FullyUnlockAt),
				"reason":          row.Reason,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"locks": rows})
}

// unixSec 时刻转 Unix 秒（零值 → 0：前端 `value ? ... : 0` 的空态判断依赖它）。
func unixSec(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
