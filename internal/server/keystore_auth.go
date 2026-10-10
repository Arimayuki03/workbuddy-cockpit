// keystore_auth.go keystore（wbk_ 多密钥分发）接入网关鉴权链。
//
// 分段鉴权的结构性原因（为什么不在 withAuth 一段做完）：keystore.Validate 的参数
// 约定要求「模型映射之后的模型名 + ResolveModel 解析出的 realm」（keystore.go 包
// 注释——用请求名判 realm，配了别名映射的密钥会永远被 realm 检查打回，参考实现
// issue #47），而 withAuth 在模型解析之前运行。因此把校验拆成三段：
//
//	段1 withAuth（handler.go）：Resolve 定位密钥 + Rejections（与请求无关的首个
//	    拒绝：停用/过期/配额/限流）→ 命中把快照与客户端 IP 塞进 request context；
//	段2 chatCompletions（模型解析之后、轮转上游之前）与 /v1/models（列表装配后）：
//	    Validate(k, ip, mappedModel, realm, now)——realm/model 白名单/IP 白名单/
//	    MaxIPs 这些依赖请求的判定，拒绝按 Status/Reason 返 OpenAI 错误体；
//	段3 记账：请求成功后（usage 口径确认后）RecordUse(k, ip, tokens, credits)——
//	    被拒/失败请求不调（issue #52：被拒请求不占限流窗口额度，见 keystore 包注释）。
//
// 零配置回归：KeyStore 为 nil（未配密钥库）时本文件全部函数是 no-op，行为与
// v1.15.1 逐位一致。
package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/keystore"
)

// keystoreCtxKey context 私有 key 类型（防跨包 key 碰撞）。
type keystoreCtxKey struct{}

// keyStoreState 段1（withAuth）产出的密钥上下文：快照 + 客户端 IP。
// 快照可安全跨段传递（Resolve 返回深拷贝，Validate 对快照是纯函数；
// RecordUse 按其 ID 重锚到库内权威条目记账）。
type keyStoreState struct {
	key *keystore.Key
	ip  string
}

// withKeyState 返回带密钥上下文的请求副本；state 为 nil 时原样返回（零开销）。
func withKeyState(r *http.Request, state *keyStoreState) *http.Request {
	if state == nil {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), keystoreCtxKey{}, state))
}

// keyStateFrom 取段1 塞入的密钥上下文；无则 nil（未用 wbk_ 密钥 / keystore 未接线）。
func keyStateFrom(r *http.Request) *keyStoreState {
	v, _ := r.Context().Value(keystoreCtxKey{}).(*keyStoreState)
	return v
}

// keyBareModel 模型白名单比对用的归一：剥 `cn:` 与 `global:` 前缀，两侧同剥后
// 精确比对。与 keystore 包内 bareModel 同口径（界面上显示裸名、用户照着填，存量
// 白名单可能写带前缀的形态，归一后两种写法等价）。keystore 不导出该函数，这里保持
// 同一规则的两份实现——规则漂移由 keystore 侧 Validate 白名单单测与 server 侧
// 对齐测试共同锁定。global: 也剥的理由同 keystore.bareModel 注释：列表裁剪
// （本函数）与调用校验（keystore.Validate，server 传裸名）必须同口径，否则
// 「列表里能看到、调用必被拒」。
func keyBareModel(model string) string {
	if strings.HasPrefix(model, "cn:") {
		return model[3:]
	}
	if strings.HasPrefix(model, "global:") {
		return model[7:]
	}
	return model
}

// ---------------------------------------------------------------------------
// 段1：withAuth 鉴权段（handler.go withAuth 内调用）
// ---------------------------------------------------------------------------

// bearerToken 提取 Authorization 头的 Bearer 值（方案前缀大小写敏感，与
// httpauth.VerifyBearer 的 "Bearer " 前缀同口径）；非 Bearer 形态返回空串。
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, prefix) {
		return ""
	}
	return authz[len(prefix):]
}

// looksLikeDistribKey 报告 Bearer 是否形如 wbk_ 分发密钥。Resolve 内部还有一次
// 形态预检（前缀 + 最短长度），这里只做「进入分发密钥分支」的路由判定；len>=4
// 保证前缀判定切片安全（keystore.Resolve 的 12 字符下限由它自己把守）。
func looksLikeDistribKey(bearer string) bool {
	return len(bearer) >= 4 && strings.HasPrefix(bearer, keystore.KeyPrefix)
}

// resolveDistribKey 段1：Bearer 未匹配全局 key 且形如 wbk_ 时定位分发密钥。
//
// 返回 (key, ok)：
//   - ok=true  → 命中分发密钥，key 为深拷贝快照（调用方塞 request context）；
//   - ok=false → 未命中：形态不是 wbk_（调用方走原 401 路径）或 Resolve 未命中/
//     出错（与全局 key 失败同口径 401）。
//
// keyStore 为 nil（未配密钥库）时恒 (nil, false)——行为与 v1.15.1 完全一致。
func (h *Handler) resolveDistribKey(bearer string) (*keystore.Key, bool) {
	if h.cfg.KeyStore == nil || !looksLikeDistribKey(bearer) {
		return nil, false
	}
	k, err := h.cfg.KeyStore.Resolve(bearer)
	if err != nil {
		return nil, false
	}
	return k, true
}

// keyRejections 段1：与具体请求无关的首个拒绝（nil = 放行）。停用/过期/配额/限流
// 在模型解析前即可判定——不必等下游 400 细分错误才告诉客户端「这把钥匙本身不可用」。
func (h *Handler) keyRejections(k *keystore.Key) *keystore.Rejection {
	if h.cfg.KeyStore == nil {
		return nil
	}
	return h.cfg.KeyStore.Rejections(k)
}

// writeKeyRejection 把 keystore.Rejection 按 OpenAI 错误体口径写给客户端：
//
//	{"error": {"message": 人话(含 reason 短码), "type": ..., "code": "<reason 短码>"}}
//
// type 分流（与 keystore 拒绝分类学对齐）：429 → insufficient_quota（OpenAI 同语义
// 口径），其余 → invalid_request_error。rate_limited 时加 Retry-After: 60 头（固定
// 60s：keystore 限流窗口即 RateWindow=60s，窗口滑过前重试无意义）。
func writeKeyRejection(w http.ResponseWriter, rej *keystore.Rejection) {
	if rej.Status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "60")
		writeJSON(w, rej.Status, map[string]any{
			"error": map[string]any{
				"message": keyRejectionMessage(rej),
				"type":    "insufficient_quota",
				"code":    rej.Reason,
			},
		})
		return
	}
	writeJSON(w, rej.Status, map[string]any{
		"error": map[string]any{
			"message": keyRejectionMessage(rej),
			"type":    "invalid_request_error",
			"code":    rej.Reason,
		},
	})
}

// keyRejectionMessage 拒绝短码 → 客户端可读的人话（含 reason 短码原样，稳定可 grep）。
func keyRejectionMessage(rej *keystore.Rejection) string {
	switch rej.Reason {
	case keystore.ReasonRealmMismatch:
		return "key is bound to another realm (realm_mismatch): this key only works for models of its configured edition"
	case keystore.ReasonModelNotAllowed:
		return "model not allowed for this key (model_not_allowed): add it to the key's model whitelist or use an allowed model"
	case keystore.ReasonIPNotAllowed:
		return "client ip not allowed for this key (ip_not_allowed): add it to the key's ip whitelist"
	case keystore.ReasonKeyDisabled:
		return "key is disabled (key_disabled)"
	case keystore.ReasonKeyExpired:
		return "key is expired (key_expired)"
	case keystore.ReasonMaxIPsExceeded:
		return "max sources reached for this key (max_ips_exceeded): an ip must be removed or max_ips raised"
	case keystore.ReasonTokenQuotaExhausted:
		return "token quota exhausted for this key (token_quota_exhausted)"
	case keystore.ReasonCreditQuotaExhausted:
		return "credit quota exhausted for this key (credit_quota_exhausted)"
	case keystore.ReasonRateLimited:
		return "key rate limited (rate_limited): retry in up to 60s"
	default:
		return "key rejected (" + rej.Reason + ")"
	}
}

// ---------------------------------------------------------------------------
// 段2：模型相关校验段（chatCompletions / models 内调用）
// ---------------------------------------------------------------------------

// validateKeyForRequest 段2：模型相关校验（realm / 模型白名单 / IP 白名单 / MaxIPs）。
// model/realm 必须是 ResolveModel（含模型映射）之后的产物——keystore.Validate 的
// 参数约定，见 keystore.go 包注释。now 注入时刻（生产 time.Now，测试注入）。
// 返回 nil = 放行（context 无密钥 / keystore 未接线时恒 nil → no-op）。
func (h *Handler) validateKeyForRequest(r *http.Request, model, realm string, now time.Time) *keystore.Rejection {
	st := keyStateFrom(r)
	if st == nil || h.cfg.KeyStore == nil {
		return nil
	}
	return h.cfg.KeyStore.Validate(st.key, st.ip, model, realm, now)
}

// filterModelsForKey 按「当前请求密钥的 realm + 模型白名单」裁剪 /v1/models 列表。
//
// 裁剪口径（与 keystore.Validate 对齐，保证「列表里有的就能调用」一致性——
// keystore 侧 model_whitelist 存的也是映射后名字，直接对齐）：
//   - realm：key.Realm 非空（归一后）时只保留该域条目，条目归属按下发 id 前缀判
//     （global: 前缀 = global，cn: 前缀与裸名 = cn，与 resolveModel 前缀协议同口径）；
//   - 白名单：key.ModelWhitelist 非空时只保留命中条目，下发 id 与白名单条目两侧过
//     keyBareModel 后精确比对（cn:/global: 前缀写法等价——与调用侧 keystore.Validate
//     同口径，保证「列表里有的就能调用」）。id 用 /v1/models 现有下发口径原样匹配。
//
// 无密钥上下文 / keystore 未接线 / 两个约束都空 → 原样返回（零裁剪零回归）。
func (h *Handler) filterModelsForKey(r *http.Request, list []map[string]any) []map[string]any {
	st := keyStateFrom(r)
	if st == nil || h.cfg.KeyStore == nil || st.key == nil {
		return list
	}
	wantRealm := normKeyRealm(st.key.Realm)
	hasModels := len(st.key.ModelWhitelist) > 0
	if wantRealm == "" && !hasModels {
		return list
	}
	out := make([]map[string]any, 0, len(list))
	for _, entry := range list {
		id, ok := entry["id"].(string)
		if !ok {
			continue
		}
		if wantRealm != "" && keyRealmOf(id) != wantRealm {
			continue
		}
		if hasModels && !keyModelAllowed(st.key.ModelWhitelist, id) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// normKeyRealm 密钥 realm 归一：去空白 + 小写后仅认 cn/global，其余（含空）= 不限制
// （与 keystore.normRealm 口径一致，管理端脏值不报错）。
func normKeyRealm(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "cn":
		return "cn"
	case "global":
		return "global"
	default:
		return ""
	}
}

// keyRealmOf 下发模型 id 的 realm 归属：global: 前缀 = global，其余（cn: 前缀与
// 裸名）= cn。与 resolveModel 的前缀协议同口径。
func keyRealmOf(id string) string {
	if strings.HasPrefix(id, "global:") {
		return "global"
	}
	return "cn"
}

// keyModelAllowed 报告下发 id 是否命中密钥的模型白名单：两侧过 keyBareModel 后
// 精确比对（与 keystore.Validate 的白名单比对同规则）。
func keyModelAllowed(allow []string, id string) bool {
	bare := keyBareModel(id)
	for _, a := range allow {
		if keyBareModel(a) == bare {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 段3：成功记账（usage 口径确认后的成功路径调用）
// ---------------------------------------------------------------------------

// keyUsageOf 从 chatStat 提取记账用量（上游 usage 真实口径，与 /v1/stats 与成本
// 账本同源同纪律）：tokens = prompt+completion 合计（usage 缺失/负哨兵时按观测
// 到的部分计，与 metrics 的 compTok 只计正值的口径一致），credits = 上游真实扣费
// （hasCredit=false 传 0——RecordUse 对 0 不计入，缺失≠0 的语义不破）。
func keyUsageOf(st *chatStat) (tokens int64, credits float64) {
	if st.hasUsage {
		if st.toks > 0 {
			tokens += int64(st.toks)
		}
		tokens += int64(st.prompt)
	}
	if st.hasCredit {
		credits = st.credit
	}
	return tokens, credits
}

// recordKeyUse 段3：成功请求记账（限流窗口、用量、来源 IP、LastUsedAt）。
// tokens 用上游 usage 的 prompt+completion 合计（metrics/成本账本同口径）、credits
// 用上游真实扣费（usage.credit；缺失传 0——RecordUse 内部「缺失不计入」）。
// 被拒/失败请求不调本函数（issue #52）。无密钥上下文 / keystore 未接线 → no-op。
func (h *Handler) recordKeyUse(r *http.Request, tokens int64, credits float64) {
	st := keyStateFrom(r)
	if st == nil || h.cfg.KeyStore == nil {
		return
	}
	h.cfg.KeyStore.RecordUse(st.key, st.ip, tokens, credits)
}
