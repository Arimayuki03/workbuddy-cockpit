// renew.go Token 独立续期巡检（wbm services/renew.py 吸收件，issue #40 同源）。
//
// ## 背景：为什么需要这个独立巡检
//
// 网关本身有刷新能力，但只在三个**被动**时机触发：
//   - 保活排程（keepalive_hours，默认每天 22 点一次「到点全量刷」）；
//   - 选号后 token 距到期不足 RefreshSkew（10 分钟）时；
//   - 签到前 token 临期时。
//
// 后两条都要求**真的有人在用这个号**。账号长期闲置、没有对话流量时 token
// 会一路走到过期而无人续期（accessToken 实测 60 天有效期），过期即彻底不可用，
// 只能重新扫码。本巡检按**剩余寿命**挑号：有效期不足阈值（ExpiringSoonWindow，
// 即 config pool.expiring_soon 口径，默认 7 天）才主动 refresh——「快过期了才刷」
// 与「到点就刷一遍」（保活）是两件事，前者才覆盖长期闲置的号。
//
// ## 与其它「停用」的语义区分（两种停用的存储位置，勿混淆）
//
//   - **面板手动停用**：pool.SetManualDisabled（entry.manualDisabled，state.json
//     的 manual_disabled 位，issue #138/#118）。它是运维明确的「摘出流量」意图，
//     但语义是「只摘对话流量，凭证与积分保持活跃」（entry.manualDisabled 注释；
//     wbm renew.py 同口径：manual_disabled 照常续期）——本巡检**照常续期**；
//   - **自动禁用**：pool.Disable（entry.disabled，session dead 连续判定）。
//     该号已被系统判定 session 死亡，续期大概率 401 白打，且 NoteSessionDead
//     有自己的连续计数与复活路径（keepalive/revive）——本巡检**跳过**；
//   - 熔断/冷却/连败降权（until/breakerUntil/degradeUntil）只是「暂时不可用」
//     的计时器，与 token 存活性正交——**照常续期**（冷却期正是把号续活的好时机）。
//
// ## 失败语义
//
// 续期是尽力而为的辅助动作：失败不冷却、不禁用（wbm 同口径——真正的可用性
// 判断仍由选号与保活负责）。结果经 Pool.SetRenewState 留痕：成功记 last_renewed
// 并清 renew_last_error，失败只更新 renew_last_error，经 /api/overview 透出。
package scheduler

import (
	"log"
	"time"

	"workbuddy2api/internal/logfmt"
)

// renewWindow 巡检判定「临期」的额外提前量：NeedsRefresh(within) 判据是
// now+within >= ExpiresAt，巡检直接用 ExpiringSoonWindow 做窗口——该窗口同时是
// 余额分桶口径（pool.expiring_soon，默认 7 天），一个口径两个用途，不另设配置。
// 巡检间隔（小时级）远小于窗口，最多晚一个槽位发现待续号，仍远早于过期。
func (s *Scheduler) renewWindow() time.Duration {
	if s.cfg.ExpiringSoonWindow > 0 {
		return s.cfg.ExpiringSoonWindow
	}
	return defaultRenewWindow
}

// defaultRenewWindow ExpiringSoonWindow 未配置（<=0）时续期巡检的兜底窗口。
// 余额分桶禁用（显式 "0"）不代表续期也该禁用——token 过期会让账号彻底报废，
// 与「是否优先消耗快过期积分」的选号偏好无关，故独立兜底而非复用 0=禁用语义。
const defaultRenewWindow = 7 * 24 * time.Hour

// renewEligible 续期选号判定（runRenewOnce 与单测共用的单一事实来源）。
//
// 口径（顺序即短路序）：
//   - st.Disabled（pool 自动禁用，session dead 判死）→ false：见文件头「语义区分」；
//   - st.ManualDisabled（面板手动停用）→ **true**：手动停用只摘对话流量，凭证
//     是活的，停用期的号更该保持 token 可用（恢复 enable 后立即能用）；
//   - a.NeedsRefresh(st.Window)（临期或已过期/无 expiry）→ true：巡检的主体。
//
// 熔断/冷却/降权不参与判定（不在 Status 上短路）：它们是暂时性计时器，到期
// 自愈，与 token 剩余寿命正交。
//
// poolStatusView / authLike：判定的最小依赖视图。runRenewOnce 传
// poolStatusView{Disabled: st.Disabled, Window: window} 与池内 *auth.Auth
// （满足 authLike）；测试用同一函数驱动表驱动用例，两处判定永不漂移。
type poolStatusView struct {
	Disabled bool
	Window   time.Duration
}

type authLike interface {
	RefreshTokenValue() string
	NeedsRefresh(within time.Duration) bool
}

func renewEligible(st poolStatusView, a authLike) bool {
	if st.Disabled {
		return false
	}
	if a == nil || a.RefreshTokenValue() == "" {
		return false // 无 refreshToken 就真的续不了（wbm 同口径：需重新扫码）
	}
	return a.NeedsRefresh(st.Window)
}

// RunRenewNow 手动/定时触发的立即续期巡检入口（外部公开，与 RunKeepaliveNow 同风格）。
// 空池瞬时完成；单号失败只记 renew_last_error，不影响遍历。
// panel 裸 goroutine 直接调用的公开入口：任务体 panic 不应击穿整个网关进程
// （与 RunCheckinNow 同理，runBatch/RunKindNow 的 recover 不覆盖本入口）。
func (s *Scheduler) RunRenewNow() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("scheduler: task renew panic: %v", r)
		}
	}()
	s.runRenewOnce()
}

// runRenewOnce 巡检一遍全账号：对临期且非自动禁用的账号主动 RefreshToken。
// 串行逐号（账号数个位数到几十，上游对高频 refresh 有风控；保活也是串行同口径）。
// 结果汇总打一行日志（可 grep：`renew done: checked=%d renewed=%d failed=%d skipped=%d`）。
//
// 复用现有刷新链（upstream.RefreshToken 原子写回 + BackfillRealm + SaveAtomic），
// 不新写上游调用。刷新成功后 ClearSessionDead（成功是「session 未死」的最强证据，
// 与保活同口径）——续期巡检因此也给了误判禁用的账号一条复活计数清除路径。
func (s *Scheduler) runRenewOnce() {
	window := s.renewWindow()
	var checked, renewed, failed, skipped int
	for _, st := range s.cfg.Pool.List() {
		// pool 自动禁用（session dead 判死）跳过；面板手动停用照常续（见文件头）。
		if st.Disabled {
			skipped++
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if !renewEligible(poolStatusView{Disabled: st.Disabled, Window: window}, a) {
			continue // 未临期/无凭证/自动禁用：不动（「快过期了才刷」与保活的「到点全刷」的区分点）
		}
		checked++
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("renew %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			s.cfg.Pool.SetRenewState(st.UID, err) // 失败留痕（renew_last_error）
			failed++
			continue
		}
		s.cfg.Pool.ClearSessionDead(st.UID) // 成功是未死的证据（保活同口径）
		s.cfg.Pool.SetRenewState(st.UID, nil)
		a.BackfillRealm() // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
		if err := a.SaveAtomic(); err != nil {
			// 刷新成功但落盘失败：重启会用旧 token，必须暴露（checkin 同口径）。
			log.Printf("renew %s save: %v", logfmt.Label(st.UID, st.Nickname), err)
		}
		renewed++
		log.Printf("renew %s ok（临期续期完成）", logfmt.Label(st.UID, st.Nickname))
	}
	if checked > 0 || renewed > 0 || failed > 0 {
		log.Printf("renew done: checked=%d renewed=%d failed=%d skipped=%d", checked, renewed, failed, skipped)
	}
}
