// credits_api.go 客户端余额协议端点（吸收 manager v2.11.12 / v2.11.13）。
//
// 三个端点分工固定：
//   - /v1/credits：RikkaHub 等 OpenAI 兼容客户端的默认余额路径（{{baseUrl}}/credits），
//     同时给 new-api 读的 total_available 与 RikkaHub 读的 total_usage；
//   - /api/usage/token/：new-api 风格余额面板，按网关密钥返回剩余额度；
//   - /api/status：公开元数据，只声明数值单位，不含任何额度。
//
// 鉴权口径与调用侧刻意不同：停用 / 过期 / 配额用尽都照常返回数字，不拦请求。
// 这里回答的是「我还剩多少」——恰恰在调不动的时候最需要。只校验密钥本身。
package server

import (
	"log"
	"math"
	"net/http"
	"strings"

	"workbuddy2api/internal/httpauth"
	"workbuddy2api/internal/iputil"
	"workbuddy2api/internal/keystore"
)

const (
	// creditsTokenScale token 额度按百万折算：token 数以百万计，与几十量级的
	// 积分额度差三个数量级，同一位置上的数字才不会忽长忽短。
	creditsTokenScale = 1_000_000
	// creditsUnit 三档口径（比例折算积分 / 剩余积分 / 百万 token）量纲不同，
	// 没有任何真实货币能同时表达，公开元数据恒为 UNIT。
	creditsUnit = "UNIT"
)

// registerCreditsAPI 挂载余额协议端点。必须在 Panel 的 /api/ 前缀之前注册
// （ServeMux 按最长 pattern 匹配，显式路径优先于面板前缀）。
func (h *Handler) registerCreditsAPI() {
	h.mux.HandleFunc("GET /v1/credits", h.withBalanceAuth(h.handleCredits))
	h.mux.HandleFunc("GET /api/usage/token/", h.withBalanceAuth(h.handleTokenUsage))
	h.mux.HandleFunc("GET /api/usage/token", h.withBalanceAuth(h.handleTokenUsage))
	// 公开元数据同样受入站 IP 规则约束（body 无额度，但运维的 ACL 不应被绕过，
	// 且与另外两个余额端点同口径）。
	h.mux.HandleFunc("GET /api/status", h.withIPOnly(h.handleBalanceStatus))
}

// checkIP 执行入站 IP 黑白名单判定（与 withAuth 同序：先于任何鉴权/业务）。
// 被拒时已写 403 并记账，返回 (ip, false)。IPRules 为 nil 时恒放行。
func (h *Handler) checkIP(w http.ResponseWriter, r *http.Request) (string, bool) {
	ip := iputil.RequestClientIP(r, h.trustedCIDRs, h.trustedHops)
	if allowed, reason := h.cfg.IPRules.Evaluate(ip); !allowed {
		h.cfg.IPRules.NoteBlocked(ip, r.URL.Path, reason)
		log.Printf("WARN: [server] ip rules: blocked %s %s reason=%s", ip, r.URL.Path, reason)
		writeOpenAIError(w, http.StatusForbidden, "ip_blocked",
			"request blocked by access rules ("+reason+")")
		return ip, false
	}
	return ip, true
}

// withIPOnly 只做 IP 规则、不验密钥（公开元数据端点用）。
func (h *Handler) withIPOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := h.checkIP(w, r); !ok {
			return
		}
		next(w, r)
	}
}

// withBalanceAuth 余额端点专用鉴权：IP 规则 + 密钥定位，但不做 Rejections
// （停用/过期/配额）判定。全局 api_key 与面板 session 视为管理身份（无密钥限额）。
func (h *Handler) withBalanceAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip, ok := h.checkIP(w, r)
		if !ok {
			return
		}
		globalKey := h.apiKey()
		if globalKey != "" {
			if httpauth.VerifyBearer(r, globalKey) {
				next(w, r)
				return
			}
			if h.cfg.SessionKey != nil && httpauth.VerifySessionRequest(r, h.cfg.SessionKey()) {
				next(w, r)
				return
			}
		}
		// wbk_ 分发密钥：即使全局 api_key 为空（开放网关）也识别，让密钥自己的
		// 配额口径生效；未命中才在全局 key 非空时回 401。
		if bearer := bearerToken(r); looksLikeDistribKey(bearer) && h.cfg.KeyStore != nil {
			if k, err := h.cfg.KeyStore.Resolve(bearer); err == nil {
				next(w, withKeyState(r, &keyStoreState{key: k, ip: ip}))
				return
			}
		}
		if globalKey != "" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": map[string]any{
					"message": "missing or invalid API key",
					"type":    "authentication_error",
					"code":    "invalid_api_key",
				},
			})
			return
		}
		next(w, r)
	}
}

// handleBalanceStatus 公开元数据：只声明数值单位，不含任何额度，无鉴权不泄露信息。
func (h *Handler) handleBalanceStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data": map[string]any{
			"quota_per_unit":                1,
			"quota_display_type":            "CUSTOM",
			"custom_currency_symbol":        creditsUnit,
			"custom_currency_exchange_rate": 1,
		},
	})
}

// handleTokenUsage /api/usage/token/：new-api 风格余额面板。
func (h *Handler) handleTokenUsage(w http.ResponseWriter, r *http.Request) {
	amount := h.creditAmounts(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"code": true,
		"data": map[string]any{
			"object": "token_usage",
			// 恒为 false：即便两份额度都不设上限，也要把账号池剩余积分显示出来；
			// 标成 true 会让面板只画「无限」而丢掉这个数字。
			"unlimited_quota": false,
			"total_granted":   amount.total,
			"total_used":      amount.used,
			"total_available": amount.available,
			"scope":           amount.scope,
		},
	})
}

// handleCredits /v1/credits：客户端默认余额路径。
// RikkaHub 把 data.total_usage 当余额显示；new-api 用 data.total_available。
// 同一份数值给两个键，客户端不必改配置。
func (h *Handler) handleCredits(w http.ResponseWriter, r *http.Request) {
	amount := h.creditAmounts(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"code": true,
		"data": map[string]any{
			"object":          "credits",
			"total_credits":   amount.total,
			"total_used":      amount.used,
			"total_available": amount.available,
			"total_usage":     amount.available,
			"scope":           amount.scope,
		},
	})
}

type creditAmounts struct {
	available float64
	used      float64
	total     float64
	scope     string
}

// creditAmounts 计算当前密钥/身份的三档额度。
//   - 两份额度都设上限：取更紧的剩余比例折算回积分（谁先见底就跟着谁走）；
//   - 只有一份设上限：直接报它的剩余量；
//   - 都不设上限：报账号池可用积分（排除封禁/手动停用账号，冷却仍算可用）。
func (h *Handler) creditAmounts(r *http.Request) creditAmounts {
	var k *keystore.Key
	if st := keyStateFrom(r); st != nil {
		k = st.key
	}
	limitCredit, limitToken, usedCredit, usedToken := 0.0, 0.0, 0.0, 0.0
	if k != nil {
		limitCredit = nonNegative(k.CreditQuota)
		limitToken = nonNegative(float64(k.TokenQuota))
		usedCredit = nonNegative(k.UsedCredits)
		usedToken = nonNegative(float64(k.UsedTokens))
	}
	hasLimit := limitCredit > 0 || limitToken > 0
	scope := resolveCreditScope(r.URL.Query().Get("scope"), k, hasLimit)
	if !hasLimit {
		pool := h.availablePoolCredits(scope)
		return creditAmounts{available: pool, used: 0, total: pool, scope: scope}
	}

	remainCredit := math.Max(0, limitCredit-usedCredit)
	remainToken := math.Max(0, limitToken-usedToken)
	switch {
	case limitCredit > 0 && limitToken > 0:
		ratio := math.Min(remainCredit/limitCredit, remainToken/limitToken)
		available := roundCredit(ratio * limitCredit)
		return creditAmounts{
			available: available,
			used:      roundCredit(math.Max(0, limitCredit-available)),
			total:     roundCredit(limitCredit),
			scope:     scope,
		}
	case limitCredit > 0:
		// 单配额分支：只对 total/available 取整，used 由差值派生，保证
		// total_granted = total_used + total_available（独立取整会出现 2.00+0.01=2.01）。
		total := roundCredit(limitCredit)
		available := roundCredit(remainCredit)
		return creditAmounts{
			available: available,
			used:      roundCredit(math.Max(0, total-available)),
			total:     total,
			scope:     scope,
		}
	default:
		total := roundCredit(limitToken / creditsTokenScale)
		available := roundCredit(remainToken / creditsTokenScale)
		return creditAmounts{
			available: available,
			used:      roundCredit(math.Max(0, total-available)),
			total:     total,
			scope:     scope,
		}
	}
}

// resolveCreditScope 决定这次余额报哪个池：
//  1. 显式 ?scope=cn|global|merged → 用指定值；
//  2. 密钥设了额度上限 → merged（按密钥自己的上限算，拆池会自相矛盾）；
//  3. 密钥本身限定版本 → 那一档；
//  4. 否则 merged。
//
// 本项目不按「最近一次真实调用」自动判定：请求日志/用量桶没有按密钥维度落库，
// 无法可靠回溯；显式 scope 或 key.realm 已覆盖双版本部署的实际需求。
func resolveCreditScope(requested string, k *keystore.Key, hasLimit bool) string {
	switch strings.ToLower(strings.TrimSpace(requested)) {
	case "cn":
		return "cn"
	case "global":
		return "global"
	case "merged":
		return "merged"
	}
	if hasLimit {
		return "merged"
	}
	if k != nil {
		if realm := normKeyRealm(k.Realm); realm != "" {
			return realm
		}
	}
	return "merged"
}

// availablePoolCredits 账号池可用积分（排除自动封禁与手动停用；冷却仍算可用）。
// scope=cn/global 时只统计该版本；merged/空 统计全池。
// 走 Pool.AvailableCredits 的轻量求和：余额端点被客户端轮询，不应为四个字段
// 构造全量 Status（statusOf 会排序/扫描模型台账与在途快照，且持有 RLock）。
func (h *Handler) availablePoolCredits(scope string) float64 {
	if h.cfg.Pool == nil {
		return 0
	}
	return roundCredit(float64(h.cfg.Pool.AvailableCredits(scope)))
}

func nonNegative(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return v
}

func roundCredit(v float64) float64 {
	v = nonNegative(v)
	return math.Round(v*100) / 100
}
