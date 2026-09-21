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

// RunBalanceRefreshNow 并发对所有非禁用账号查询余额并更新池内 credits。
// 解冻语义与签到一致（ReenableIfCredits：余额 > 0 的冷却账号自动解冻），
// 但不做签到、不刷新 token——只让"积分"这个观测量保持新鲜。
// 供面板手动触发（panel.balanceAll）。
func (s *Scheduler) RunBalanceRefreshNow() {
	var wg sync.WaitGroup
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
			remain, buckets, err := s.cfg.Upstream.UserResourceDetailed(a, s.cfg.ExpiringSoonWindow)
			if err != nil {
				log.Printf("balance %s: %v", logfmt.Label(uid, a.Nickname), err)
				return
			}
			if buckets.Expiring > 0 {
				s.cfg.Pool.SetCreditsDetailed(uid, remain, buckets.Expiring)
			} else {
				s.cfg.Pool.ReenableIfCredits(uid, remain)
			}
		}(a, st.UID)
	}
	wg.Wait()
}
