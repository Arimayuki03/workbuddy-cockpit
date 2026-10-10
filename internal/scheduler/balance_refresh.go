// balance_refresh.go 余额刷新（panel 移植件）。
//
// 并发对所有非禁用账号查询余额并写回池内 credits，带与签到一致的解冻语义
// （ReenableIfCredits：余额 > 0 的冷却账号自动解冻，只清冷却域、不动熔断器）。
//
// 死代码清理（2026-09-21）：原 StartBalanceRefresh（后台周期 ticker）与
// SetBalanceInterval（热改间隔）全仓库无任何调用方，文档也未承诺该功能，
// 经确认删除而非接线；连带的 rearmBalance 通知通道与 balanceInterval 字段
// 一并移除（Scheduler 结构体不再声明）。面板手动全量刷新入口
// RunBalanceRefreshNow 有调用方（panel.balanceAll），保留。
//
// 主仓库漂移适配（panel 快照日即锚定日）：
//   - upstream.UserResourceDetailed 返回 (remain, CreditBuckets, err)；
//   - pool.SetCreditsDetailed(uid, credits, expiring)；ReenableIfCredits(uid, remain)。
package scheduler

import (
	"log"
	"sync"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

// nickOf 从昵称快照取展示名（缺账号回落 uid；仅日志/文案用途）。
func nickOf(nicks map[string]string, uid string) string {
	if n := nicks[uid]; n != "" {
		return n
	}
	return uid
}

// RunBalanceRefreshNow 并发对所有非禁用账号查询余额并更新池内 credits。
// 解冻语义与签到一致（ReenableIfCredits：余额 > 0 的冷却账号自动解冻），
// 但不做签到、不刷新 token——只让"积分"这个观测量保持新鲜。
// 供面板手动触发（panel.balanceAll）。
//
// 昵称同步（panel f1496d0a 口径）：余额查询成功后逐账号调 FetchAccountProfile
// 同步最新昵称（用户在官网改名后免重登）。仅本手动路径接线——后台余额定时器
// **不调用**资料接口（2026-09-21 起本函数已是唯一入口且仅面板手动触发；若日后
// 重新引入定时全量刷新，请另立函数，勿把资料接口挂上高频路径）。
// 资料拉取失败静默跳过（DEBUG 日志即可），不影响余额结果、不计入账号惩罚。
//
// 并发上限 4（信号量）：面板全量刷新账号多时若不设闸，瞬时 N 路并发全打上游
// GET /resource（对照 panel/taskcenter.go schoolVouchers 的限流写法）。
func (s *Scheduler) RunBalanceRefreshNow() {
	// panel 裸 goroutine 入口：任务体 panic 不应击穿整个网关进程（与 RunCheckinNow
	// 同理，runBatch/RunKindNow 的 recover 不覆盖本入口）。
	defer func() {
		if r := recover(); r != nil {
			log.Printf("scheduler: task balance-refresh panic: %v", r)
		}
	}()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	// 昵称表：先单独遍历一遍池构建完整快照，再进并发循环——若边启 goroutine 边写
	// map，先前 goroutine 的读与主循环的写并发，触发「concurrent map read and
	// map write」直接 fatal 整个网关进程。
	nicks := map[string]string{}
	for _, st := range s.cfg.Pool.List() {
		nicks[st.UID] = st.Nickname
	}
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(a *auth.Auth, uid string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			remain, buckets, err := s.cfg.Upstream.UserResourceDetailed(a, s.cfg.ExpiringSoonWindow)
			if err != nil {
				log.Printf("balance %s: %v", logfmt.Label(uid, a.NicknameValue()), err)
				return
			}
			if buckets.Expiring > 0 {
				s.cfg.Pool.SetCreditsDetailed(uid, remain, buckets.Expiring)
			} else {
				s.cfg.Pool.ReenableIfCredits(uid, remain)
			}
			// 积分流水：余额比对记账（credit.go）。昵称取自池状态快照（并发 goroutine
			// 外先查好，避免 goroutine 内再读池）。
			s.RecordBalanceChecked(uid, nickOf(nicks, uid), remain)

			// 昵称同步（panel f1496d0a）：手动刷新路径逐账号拉一次 Web 控制台资料，
			// 上游有改名则更新内存 + 落盘（沿用 RefreshToken 的锁内写字段约定，
			// SaveAtomic 原子写回 auth 文件 account.nickname；pool 的 Status/List
			// 直读同一 *Auth，昵称即时生效）。失败静默跳过——改名同步是"顺手"功能，
			// 不值得为它打扰用户，也不影响本次余额结果。
			if nick, perr := s.cfg.Upstream.FetchAccountProfile(a); perr != nil {
				log.Printf("DEBUG: balance %s: profile skip: %v", logfmt.Label(uid, nickOf(nicks, uid)), perr)
			} else if nick != "" && nick != nicks[uid] {
				a.SetNickname(nick)
				if err := a.SaveAtomic(); err != nil {
					// 内存已更新（本次会话即时生效），落盘失败仅记日志：重启后回落旧名。
					log.Printf("balance %s: nickname save: %v", logfmt.Label(uid, nick), err)
				} else {
					log.Printf("balance %s: nickname synced %q -> %q", logfmt.Label(uid, nick), nicks[uid], nick)
				}
			}
		}(a, st.UID)
	}
	wg.Wait()
}
