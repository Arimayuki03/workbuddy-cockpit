// compensation.go 「活动补偿」一键领取（wbm 1.0.93 / CHANGELOG 1.0.93 吸收件）。
//
// ## 这个动作解决什么
//
// 平台会把活动补偿（如 2026-10-16 的 DeepSeek 故障补偿）发给账号，官方网页
// 「用量管理 → 领取」要用户手动点一次。上游调度器的活跃上报链路里本来也会调同一个
// 接口（scheduler.claimGrowthBonus → upstream.ClaimCompensation），但它挂在「活跃上报」
// 这个开关下——用户关掉活跃上报就领不到了。而这类补偿**有时限**（公告写着截止日期），
// 漏了就真的没了，所以这里给一条不依赖活跃上报开关的路：面板「立即执行」当场领一次，
// 也可选每天自动跑（schedule.compensation_enabled + compensation_hours）。
//
// ## 语义（三条，缺一条都会被用户当成 bug）
//
//   - **有则领、没有就跳过**：上游对「没有可领」与「已经领过」都回业务错误，两者都是
//     正常态，不是故障（upstream.CompensationClaim 把三态分开，见其注释）；
//   - **重复执行不会多领**：领取本身幂等，且每号先查一次状态——状态显示「已领过」时
//     直接跳过，省掉一次注定失败的写请求（wbm 同口径：认不出状态结构时照常尝试领取，
//     认不出不等于领过了）；
//   - **国际版账号自动跳过**：global 没有这套活动（公告只面向国内版），不发起任何请求。
//
// ## 与定时排程的关系
//
// 默认不自动跑（schedule.compensation_enabled 缺省 false、compensation_hours 默认空），
// 手动「立即执行」最稳——补偿公告常带截止日期，用户点一下当场拿结果。开启自动跑时按
// 与其他任务族同一套槽位结构接入（nextWake/dueBatches：整点 + 每日确定性随机偏移），
// 但**手动路径不加延后/错峰**（无 scheduledAccountKey 标记，见 random_schedule.go）。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/upstream"
)

// compensationAccountDelay 账号间限速：逐账号串行（与签到/旅行/续期同口径——低并发，
// 不把上游打出风控）。测试可置 0。
var compensationAccountDelay = 800 * time.Millisecond

// CompensationOutcome 一次「活动补偿」领取的汇总（面板 /api/compensation/run 的
// accounts/claimed/credits/message 与定时日志共用同一份口径）。
//
// 计数口径刻意与 wbm claim_compensation_all 对齐：
//   - Accounts 只算**真的发起了领取请求**的账号（claimed+nothing+failed）：已领过 /
//     跳过 / 无凭据的号没发请求，算进分母会让界面显示成「N/M 成功」，用户以为有个号
//     没领到；
//   - Failed 只算网络/协议层真故障（业务拒绝算 Nothing）。
type CompensationOutcome struct {
	Accounts int   // 尝试数（发起了领取请求的账号数）
	Claimed  int   // 本次到账的账号数
	Credits  int64 // 本次到账积分合计（回执缺字段记 0）
	Already  int   // 状态查询显示此前已领过（未发领取请求）
	Nothing  int   // 上游业务拒绝：没有可领（正常态）
	Skipped  int   // 跳过：自动禁用 / 面板手动停用 / 国际版 / 无凭据
	Failed   int   // 失败（网络或协议层，可重试）
	// FirstErr 首个失败原因（无失败为空）：面板 message 里要说明原因，日志同理。
	FirstErr string
	// Canceled ctx 在遍历中途被取消（剩余账号未处理）：面板据此报 ok=false +
	// 「已取消」说明，而不是把半程结果当完整结果汇报。
	Canceled bool
}

// OK 报告本次是否无失败（「没有可领」不算失败：补偿是一次性的，大多数时候本来就没得领）。
func (o CompensationOutcome) OK() bool { return o.Failed == 0 && !o.Canceled }

// Summary 一行可读摘要（面板 message 与定时日志共用；分段文案对齐 wbm dailyplan）。
func (o CompensationOutcome) Summary() string {
	var parts []string
	if o.Claimed > 0 {
		parts = append(parts, fmt.Sprintf("%d 个领到 +%d 积分", o.Claimed, o.Credits))
	}
	if o.Already > 0 {
		parts = append(parts, fmt.Sprintf("%d 个已领过", o.Already))
	}
	if o.Nothing > 0 {
		parts = append(parts, fmt.Sprintf("%d 个无可领", o.Nothing))
	}
	if o.Failed > 0 {
		reason := o.FirstErr
		if reason == "" {
			reason = "未知原因"
		}
		parts = append(parts, fmt.Sprintf("%d 个失败（%s）", o.Failed, reason))
	}
	if o.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d 个跳过（禁用/停用/国际版/无凭据）", o.Skipped))
	}
	if o.Canceled {
		parts = append(parts, "已取消（剩余账号未处理）")
	}
	if len(parts) == 0 {
		return "没有可执行的账号"
	}
	return joinParts(parts)
}

// joinParts 用「；」拼段落（与 wbm dailyplan 的用户可见文案同形）。
func joinParts(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "；"
		}
		out += p
	}
	return out
}

// RunCompensation 给池内账号领一遍活动补偿并返回汇总；ctx 取消时立即放弃剩余账号
// （已完成的账号照常计入汇总）——与 runTravel/runActivity 同口径。
//
// Canceled 只在**确有未处理账号**时置真（字段语义「剩余账号未处理」，Summary/OK
// 同此口径）：取消恰好落在最后一个账号的处理期间时，全部账号其实都已处理完，
// 报「已取消」会让面板把完整结果说成半程结果；取消发生在某号处理**之前**（或
// 限速等待被打断）时，该号确定没轮到，照旧置真。限速等待被打断、以及取消落在
// 中间账号之后的场景，都按「后面还有条目」保守置真（后续条目可能因禁用被跳过，
// 不再细分——与 sleep 分支的中断口径一致）。
//
// 手动「立即执行」（面板）与定时到点（runOne 的 taskCompensation 分支）共用本入口，
// 单一事实来源：两条路的账号口径、幂等语义、汇总文案不会漂移。
// 不在此处 recover：定时路径的调用方（dispatchSafe/RunKindNow）已有兜底，面板侧是
// 同步 HTTP handler（net/http 自会兜住 panic，且本函数只做 HTTP 调用与计数）。
func (s *Scheduler) RunCompensation(ctx context.Context) CompensationOutcome {
	var out CompensationOutcome
	first := true
	sts := s.cfg.Pool.List()
	for i, st := range sts {
		// 账号口径（两道停用都跳过，且**与 renew 巡检相反**，理由各异）：
		//   - Disabled（自动禁用，session dead 判死）：凭证已废，打过去只会 401；
		//   - ManualDisabled（面板手动停用）：运维明确的「这个号先别动」意图，而领取
		//     补偿是写请求 + 领取通知类动作（可能给账号发通知/留痕），尊重该意图。
		//     renew 巡检照常续期是因为那边只维护凭证、不产生业务动作（见 renew.go 文件头）。
		if st.Disabled || st.ManualDisabled {
			out.Skipped++
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			out.Skipped++
			continue
		}
		// D4 门控：realm=global 账号没有这套活动（公告只面向国内版），不发起任何
		// 上游调用以免白吃 4xx/被当成异常形态（同 travel.go 的 global 门控写法）。
		// 逃生门（global.enabled=false）下 global 账号被降级为 cn 并按 CN 处理——与
		// checkin/travel 引用处一致（纯 CN 部署锁死一切 global）。
		if a.IsGlobal() {
			out.Skipped++
			continue
		}
		// 账号错峰：定时路径叠加 accountGap（手动 ctx 无 scheduledAccountKey 标记 →
		// 逐字返回 base，即「立即执行」不被延后/错峰）。
		if !first {
			if !sleepCtx(ctx, s.accountGap(ctx, compensationAccountDelay)) {
				// 等待被打断：本号（及其后）都没轮到，确有未处理账号。
				out.Canceled = true
				break // 优雅停机：不等限速睡满，剩余账号下轮（或用户重跑）再领
			}
		}
		first = false
		if !s.compensationOne(ctx, a, &out) {
			// 取消发生在处理本号**之前**（compensationOne 入口 ctx 检查）：本号未发
			// 任何请求，属于「剩余账号未处理」，照旧置真。
			out.Canceled = true
			break
		}
		if ctx.Err() != nil {
			// 本号已处理完，ctx 在处理期间/之后被取消：只有后面还有账号才算未处理。
			// 最后一个账号上取消（i == len-1）→ 全部账号已处理完，保持 false。
			out.Canceled = i < len(sts)-1
			break
		}
	}
	log.Printf("compensation done: accounts=%d claimed=%d credits=%d already=%d nothing=%d failed=%d skipped=%d",
		out.Accounts, out.Claimed, out.Credits, out.Already, out.Nothing, out.Failed, out.Skipped)
	return out
}

// compensationOne 单账号领取：状态查询（已领过就跳过）→ 领取 → 记账。
// 返回 false 表示 ctx 在处理本号**之前**已被取消（未发任何请求、未计入任何计数）；
// 处理完成后才发现取消不在返回值里表达——本号结果依然有效，由调用方查 ctx 决定
// 是否中止遍历（是否置 Canceled 取决于后面还有没有账号，见 RunCompensation）。
func (s *Scheduler) compensationOne(ctx context.Context, a *auth.Auth, out *CompensationOutcome) bool {
	label := logfmt.Label(a.UID, a.NicknameValue())
	if ctx.Err() != nil {
		return false
	}
	// 状态前置：已知领过 → 跳过一次注定失败的写请求（幂等的第二道闸）。
	// 查询失败（网络/业务错误）不当失败：**照常尝试领取**——认不出/查不到不等于领过了，
	// 把一次有时限的机会放过比多发一次幂等写请求糟糕得多（wbm 同口径）。
	st, err := s.cfg.Upstream.CompensationStatus(a)
	if err != nil {
		if errors.Is(err, upstream.ErrCompensationGlobal) {
			out.Skipped++
			return true
		}
		log.Printf("compensation %s: status: %v（照常尝试领取）", label, err)
	}
	if st.AlreadyClaimed() {
		out.Already++
		log.Printf("compensation %s: skip (already claimed)", label)
		return true
	}
	out.Accounts++ // 尝试数：本次真的发起了领取请求（与 wbm 的 tried 分母同口径）
	claimed, credits, err := s.cfg.Upstream.CompensationClaim(a)
	switch {
	case errors.Is(err, upstream.ErrCompensationGlobal):
		// 客户端侧第二道 realm 防线（账号已在上方门控，正常路径不可达）：
		// 请求没发出去，从尝试数里退回。
		out.Accounts--
		out.Skipped++
	case err != nil:
		out.Failed++
		if out.FirstErr == "" {
			out.FirstErr = err.Error()
		}
		log.Printf("compensation %s: claim: %v", label, err)
	case claimed:
		out.Claimed++
		out.Credits += credits
		log.Printf("compensation %s: claim ok (+%d credit)", label, credits)
	default:
		// 没有可领 / 已经领过：上游业务拒绝，正常态——只在汇总里计数，不刷 WARN
		//（与 claimGrowthBonus 的礼包/补偿静默口径一致：绝大多数号本来就没得领）。
		out.Nothing++
		log.Printf("compensation %s: claim skip (nothing to claim)", label)
	}
	// 本号已处理完（结果已记账）：即便此刻 ctx 已取消也返回 true——是否中止遍历、
	// 是否置 Canceled 由调用方按「后面还有没有账号」判定。
	return true
}
