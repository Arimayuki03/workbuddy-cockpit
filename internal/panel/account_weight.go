// account_weight.go 账号手写权重端点（POST /api/accounts/{uid}/weight）。
//
// 语义与上游 patch 0008 / manager AccountWeightDialog 一致：weight 是**乘数**
// 而非固定优先级——选号本身按「余额 + 快过期占比 + 闲置时长」三因子加权随机，
// 该值把结果再乘一遍（见 internal/pool/pick.go 的 weightOf）。冷却 / 熔断 /
// 禁用 / 手动停用 / 在途占满 / 成本分层 / credits_desc 都不受它影响。
//
// 存储：写进该账号 auth 文件顶层 `weight`（与 device_token / proxy_route 同级），
// 而不是 state.json —— 账号文件是凭证与账号配置的单一事实来源，导入/导出、
// token 刷新后的 SaveAtomic 都会原样保留。0 = 未设置（等价 1）；面板弹窗把
// 用户输入的 1 转成 0 提交，避免给默认账号的文件添 weight: 1 噪音。
//
// 鉴权口径：经 p.api() 注册（withAuth 闸：会话 cookie / Bearer api_key 双通道），
// 与 note/proxy_route 等写端点同源。**不进** wbt_ token 分级表：写语义在分级
// 表外恒 403 token_write_forbidden，无需在此显式拒绝。
package panel

import (
	"encoding/json"
	"log"
	"math"
	"net/http"

	"workbuddy2api/internal/auth"
)

// accountWeightBody 请求体：{"weight": 2.5}。0 = 恢复默认（删除持久化键）。
// 用指针区分「缺字段」与「显式 0」——缺字段按 invalid_weight 拒绝，避免把
// 畸形请求静默当成「清除权重」。
type accountWeightBody struct {
	Weight *float64 `json:"weight"`
}

// accountWeight 保存单个账号的手写权重（POST /api/accounts/{uid}/weight）。
//
//   - 404 account not found：uid 不在池里（与 note/proxy_route 同判据）；
//   - 400 invalid_body：请求体不是合法 JSON（含 NaN/Inf 字面量、超长 body）；
//   - 400 invalid_weight：缺 weight 字段、负数、非有限值、0<weight<0.1 或
//     weight>100（端点输入拒绝越界；文件里手工写坏的旧值由 PickWeight 钳制）；
//   - 500：auth 文件落盘失败（内存值回滚到保存前，不留半态）；
//   - 200 {"ok":true,"weight":<stored>,"effective_weight":<clamped>}：weight=0
//     表示已恢复默认；effective_weight 是选号实际使用的乘数。
//
// 落盘前先改内存：选号在 p.cfg.Pool 内直接读同一个 *auth.Auth，改完无需重载
// 上游即可生效；落盘失败则回滚，保持「文件与内存一致」的既有口径。
func (p *Panel) accountWeight(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	var body accountWeightBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body")
		return
	}
	if body.Weight == nil {
		writeErr(w, http.StatusBadRequest, "invalid_weight")
		return
	}
	weight := *body.Weight
	if math.IsNaN(weight) || math.IsInf(weight, 0) || weight < 0 || weight > auth.MaxWeight {
		writeErr(w, http.StatusBadRequest, "invalid_weight")
		return
	}
	// (0, MinWeight) 是端点非法输入（UI 也按非法处理）：合法区间只有 0（清除）
	// 与 [MinWeight, MaxWeight]。注意不能把 0.05 静默钳成 0.1 —— 那会让调用方
	// 以为存进去的是自己填的值。
	if weight > 0 && weight < auth.MinWeight {
		writeErr(w, http.StatusBadRequest, "invalid_weight")
		return
	}
	// 注意不把 1 归一为 0：1 是合法显式值（manager 服务层同口径），面板弹窗
	// 在用户填 1 时自己发 0，所以正常 UI 仍不会给文件添 weight: 1 噪音。
	// 直接调 API 写 1 会原样落盘，语义与「未设置」等价（PickWeight 恒 1）。

	// 持久化与内存写入必须在同一临界区完成（auth.SetWeightAndSave 内部持
	// a.mu）：拆成 SetWeight + SaveAtomic 两步时，两个并发保存之间可插入，
	// 后失败的一条回滚会覆盖先成功那条的权重（lost update）。
	// 写入即落盘；FilePath 为空 = 纯内存装配，方法会跳过落盘只改内存。
	stored, err := a.SetWeightAndSave(weight)
	if err != nil {
		log.Printf("panel: weight uid=%s save failed: %v", uid, err)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	effective := a.PickWeight()
	if weight == 0 {
		log.Printf("panel: weight uid=%s（已恢复默认 1）", uid)
	} else {
		log.Printf("panel: weight uid=%s → %g（生效 %g）", uid, weight, effective)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"weight":           stored,
		"effective_weight": effective,
	})
}
