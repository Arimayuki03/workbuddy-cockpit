// balance_refresh.go 余额后台刷新（panel 移植件，v1.2.0 设计文档 §3.1）。
//
// 并发对所有非禁用账号查询余额并写回池内 credits，带与签到一致的解冻语义
// （ReenableIfCredits：余额 > 0 的冷却账号自动解冻，只清冷却域、不动熔断器）。
// 供两类入口复用：后台周期任务（StartBalanceRefresh，独立 ticker）与面板手动
// 全量刷新（panel.balanceAll → RunBalanceRefreshNow）。
//
// 主仓库漂移适配（panel 快照日即锚定日）：
//   - upstream.UserResourceDetailed 返回 (remain, CreditBuckets, err)；
//   - pool.SetCreditsDetailed(uid, credits, expiring)；ReenableIfCredits(uid, remain)。
package scheduler

import (
	"context"
	"log"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

// RunBalanceRefreshNow 并发对所有非禁用账号查询余额并更新池内 credits。
// 解冻语义与签到一致（ReenableIfCredits：余额 > 0 的冷却账号自动解冻），
// 但不做签到、不刷新 token——只让"积分"这个观测量保持新鲜。
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

// StartBalanceRefresh 后台周期性余额刷新（独立 ticker goroutine，ctx 取消即停）。
// interval<=0 不启动。独立于 Run 的小时制排程：余额是分钟级观测量，不值得为它
// 扩展 nextFire 的粒度。运行期可用 SetBalanceInterval 热改间隔（下一轮生效）。
func (s *Scheduler) StartBalanceRefresh(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	s.balanceInterval.Store(int64(interval))
	go func() {
		var logged time.Duration
		for {
			cur := time.Duration(s.balanceInterval.Load())
			if cur != logged {
				log.Printf("scheduler: 余额后台刷新每 %s（暂停中显示 0s）", cur)
				logged = cur
			}
			if cur <= 0 {
				// 被热改暂停：等重排通知（重新启用时唤醒）或退出。
				select {
				case <-ctx.Done():
					return
				case <-s.rearmBalance:
					continue
				}
			}
			timer := time.NewTimer(cur)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.rearmBalance:
				timer.Stop() // 间隔已变：立刻按新值重算
			case <-timer.C:
				s.RunBalanceRefreshNow()
			}
		}
	}()
}

// SetBalanceInterval 热改余额刷新间隔；<=0 表示暂停循环（面板关闭该开关时）。
func (s *Scheduler) SetBalanceInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.balanceInterval.Store(int64(d))
	// poke：容量 1 通知「间隔已变，重算」。与 SetEnabled 的 wake 同风格；
	// 已有未消费通知时跳过（重算幂等，无需排队）。
	select {
	case s.rearmBalance <- struct{}{}:
	default:
	}
}
