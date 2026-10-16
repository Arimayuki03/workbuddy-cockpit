// random_schedule.go 每日随机延后 + 账号错峰（吸收 workbuddy-manager 补丁 0003 /
// CHANGELOG 1.0.91）。
//
// 两个用户可见行为：
//  1. 随机延后（schedule.random_delay_minutes）：自动任务不再卡在整点，而是在
//     「原定整点 + 0..N 分钟」内的**固定**偏移触发（scheduledSlot）。偏移由部署私有
//     种子确定性派生，重启后不变、不同部署互不相同——固定整点的批量流量因此被摊开，
//     而「同一天同一任务何时跑」仍可预期（不是每次都换时间）。
//  2. 账号错峰（schedule.account_delay_min_seconds/max_seconds）：同一批任务里处理完
//     一个账号后，随机等 [min,max] 秒再处理下一个账号（accountGap），避免整批账号在
//     同一秒打上游（风控视角的「批量特征」）。
//
// 边界（契约）：
//   - 手动「立即执行」（RunCheckinNow/RunKindNow/panel「立即执行」/cmd/task 等）不经
//     scheduledCtx 标记，两条行为都不生效，保持立即执行；
//   - 旧配置（没有新键 → 两个区间都是 0）两条路径逐字返回既有值，行为不变；
//   - 种子（api_key / auth_dir）只参与 sha256 输入，绝不进日志或错误信息。
package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"log"
	"math/rand"
	"time"
)

// scheduledAccountKey ctx 键：标记「本次账号遍历由定时排程派发」。
//
// 为什么用 ctx 标记而不是给 Scheduler 加字段：错峰只该作用于定时派发，而手动入口
// （panel 裸 goroutine、cmd/task、/admin 手动执行）随时可能在同一个 Scheduler 上并发
// 触发——传给遍历的 ctx 是唯一天然区分两条路径的载体（dispatch 注入，手动路径的背景
// ctx 没有标记），无需额外锁，也不会互相污染。
type scheduledAccountKey struct{}

// accountDelayUnit 账号错峰区间的时间单位（配置值单位「秒」→ Duration）。
// 抽成包级变量只为测试能把 30-120 秒缩到毫秒级断言、不真睡分钟级
// （风格同 travelAccountDelay「测试可置 0」/ wakeupGraceDelay「测试可缩短」）。
var accountDelayUnit = time.Second

// clampRandomDelayMinutes / clampAccountDelay 装配期防御性归一（供 New 调用）。
//
// config.Normalize 已在配置层带键名快速失败；这里兜住零值 Config 与绕过 Normalize 的
// 调用方（cmd/task、测试直构），风格同 clampHours：非法值一律按「关闭」处理并记一行
// WARN——调度循环不该因一个配置值 panic，也不该用越界值算出意外时点。
func clampRandomDelayMinutes(v int) int {
	if v < 0 || v > 60 {
		log.Printf("scheduler: random_delay_minutes=%d 越界（合法 0-60 分钟），按 0（关闭随机延后）处理", v)
		return 0
	}
	return v
}

func clampAccountDelay(lo, hi int) (int, int) {
	if lo < 0 || hi < 0 || hi > 600 || lo > hi {
		log.Printf("scheduler: account_delay=%d-%d 越界（需 0 <= min <= max <= 600 秒），按 0（关闭账号错峰）处理", lo, hi)
		return 0, 0
	}
	return lo, hi
}

// accountGap 定时排程路径的账号间隔：在原有最小间隔 base 上叠加 [min,max] 秒随机等待。
//
// 手动路径（ctx 无 scheduledAccountKey 标记）与区间关闭（max<=0，含旧配置缺键）
// 逐字返回 base——旧行为不变（base 为 activityAccountDelay 这类既有节流）。
// 区间非法（Normalize 已拦，防御性再判）同样返回 base：调度不该因配置错误中断。
func (s *Scheduler) accountGap(ctx context.Context, base time.Duration) time.Duration {
	if ctx.Value(scheduledAccountKey{}) != true || s.cfg.AccountDelayMaxSeconds <= 0 {
		return base
	}
	lo, hi := s.cfg.AccountDelayMinSeconds, s.cfg.AccountDelayMaxSeconds
	if lo < 0 || hi < lo || hi > 600 {
		return base
	}
	// rand.Intn 用全局源：Go 1.20+ 默认自动随机播种、并发安全；这里是「运行期摊开流量」
	// 的一次性抖动，不需要可复现（区别于 scheduledSlot 的确定性偏移）。
	return base + time.Duration(lo+rand.Intn(hi-lo+1))*accountDelayUnit
}

// scheduledSlot 计算某类任务某天某整点的实际触发时刻：原定整点 + 确定性随机偏移。
//
// 稳定性契约（用户可见：重启后偏移不变、不同部署互不相同）：
//   - 种子取部署私有值（cfg.APIKey 优先，为空退 cfg.AuthDir，见 cmd/server/main.go），
//     只作为 sha256 输入的一部分，绝不写进日志/错误信息；
//   - 偏移按「任务族(kind) + 日期 + 整点」派生：同一天同一任务的同一槽位恒定，
//     跨天/跨任务/跨部署各自独立；
//   - 纯函数派生，无进程内随机状态：重启、多实例天然一致，也没有状态可丢。
//
// 边界：偏移恒落在原定小时之内（最多 59:59），既不与下一整点槽位重叠、也不跨自然日
// （跨日会让 dueBatches 的按日枚举丢槽）。RandomDelayMinutes<=0（缺省/旧配置）
// 逐字返回原整点——旧行为不变。
func (s *Scheduler) scheduledSlot(day time.Time, hour int, kind taskKind) time.Time {
	at := time.Date(day.Year(), day.Month(), day.Day(), hour, 0, 0, 0, day.Location())
	minutes := s.cfg.RandomDelayMinutes
	if minutes <= 0 {
		return at
	}
	if minutes > 60 {
		minutes = 60 // 防御：config.Normalize 已拦；越界按上限处理而不是悄悄关掉
	}
	seconds := minutes * 60
	if seconds >= 3600 {
		seconds = 3599 // 60 分钟时最多偏移到 59:59，最后一秒留给本小时
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%d",
		s.cfg.RandomDelaySeed, at.Format("2006-01-02"), hour, kind)))
	offset := binary.BigEndian.Uint64(digest[:8]) % uint64(seconds+1)
	return at.Add(time.Duration(offset) * time.Second)
}

// nextScheduledFire 返回 hours 表里 after now 最近的一次实际触发时刻（含随机偏移）。
//
// 随机延后关闭时直接走 nextFire(now, hours)，与引入本文件前逐字等价。
// 开启时只扫「今天 + 明天」两天：偏移恒在本小时之内，明天的任意槽位都晚于今天的
// 任意时点，故两天的候选里必含全局最近的一个（与上游补丁 scheduledSlot 同口径）。
func (s *Scheduler) nextScheduledFire(now time.Time, hours []int, kind taskKind) time.Time {
	if s.cfg.RandomDelayMinutes <= 0 {
		return nextFire(now, hours)
	}
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	var earliest time.Time
	for i := 0; i < 2; i++ {
		d := day.AddDate(0, 0, i)
		for _, h := range hours {
			at := s.scheduledSlot(d, h, kind)
			if at.After(now) && (earliest.IsZero() || at.Before(earliest)) {
				earliest = at
			}
		}
	}
	return earliest
}

// hasHour 报告小时表是否含某整点（dueBatches 逐小时扫描时用）。
func hasHour(hours []int, h int) bool {
	for _, v := range hours {
		if v == h {
			return true
		}
	}
	return false
}
