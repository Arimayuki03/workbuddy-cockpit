// 模型锁池可见性（吸收自 workbuddy2api-panel b3f92dd9/5ae2200f 的 modelview.go）：
// 把「哪些 (域, 模型) 当前被模型级冷却锁住、锁了几个号、还要锁多久」聚合成一张
// 只读清单，供 /status 与面板直接展示——运维不必先让一次真实请求失败（no_healthy_account）
// 才发现某个模型不能用、还要等多久。
package pool

import (
	"sort"
	"strings"
	"time"
)

// ModelLockRow 单个 (域, 模型) 的锁池画像：这个模型在这个域还能不能选、被锁了几个号、
// 最早/全部解锁是什么时候。
//
// 与单点判定的分工：healthyForModel 回答「**这一个**账号对**这一个**模型此刻能不能选」
// （选号热路径），AvailableUIDsForModelRealm 回答「**一个域**里还有哪些号可选」；
// 本结构是**全清单**版本——把当前所有有未过期模型级冷却的 (域, 模型) 一次性列出来，
// 供 /status 与面板直接展示。
type ModelLockRow struct {
	// Model 裸模型名（不含 cn:/global: 前缀；域由 Realm 字段表达）。
	Model string `json:"model"`
	// Realm 账号域（cn | global）。同名模型在两个域各自独立计数。
	Realm string `json:"realm"`
	// Total 该域**参与选号**的账号数（disabled / manualDisabled 号不计，与
	// countsDetailedForRealm 的 disabled 口径一致），作为「可选 / 总数」的分母。
	Total int `json:"total"`
	// Servable 此刻真正能服务该模型的账号数（healthyForModel 且未在途占满）。
	// 注意：不含积分保底（creditFloor）维度——保底是选号层的收费模型过滤
	//（pick 内 floorBlocked），与模型冷却正交，触底号在本视图仍计为 servable。
	Servable int `json:"servable"`
	// Locked 因该模型自身冷却（6004 模型级限流 / 11102 负缓存）被挡的账号数。
	Locked int `json:"locked"`
	// State 聚合状态：
	//   - locked  ：全部参与选号的账号都被该模型冷却挡住（换模型或等解锁才有用）；
	//   - starved ：此刻没有号能服务，但不是模型冷却造成的（账号级冷却/熔断/降权/
	//               在途占满）——等一下就会好，与模型无关，换模型没用；
	//   - partial ：仍有号能服务该模型，只是部分号被锁。
	State string `json:"state"`
	// UnlockAt 最早解锁时刻：第一个被锁账号恢复的时刻（partial 下「何时值得重试」）。
	UnlockAt time.Time `json:"unlock_at"`
	// FullyUnlockAt 全部解锁时刻：最后一个被锁账号恢复的时刻。
	FullyUnlockAt time.Time `json:"fully_unlock_at"`
	// Reason 该模型被锁的原因（取最早解锁账号的上游原文；6004 恒为
	// "6004 model rate limit"，11102 以 "11102" 开头）。
	Reason string `json:"reason,omitempty"`
}

// ModelLockView 汇总当前所有「有未过期模型级冷却」的 (域, 模型)，按不可用优先排序
// （locked → starved → partial，再按模型名、域名字典序，保证 /status 输出稳定）。
//
// 口径与选号（healthyForModel）严格一致，三处易错点：
//   - 只看**参与选号**的账号：disabled / manualDisabled 号跳过——它们的不可用与
//     模型无关，计进来会把「模型被锁」和「号被停了」混为一谈；
//   - 只看**真正拦路由**的冷却：Until 零值条目不算锁（与 modelCooled 同口径；
//     本项目的 modelCooldown 无 panel 侧 AuditOnly 形态，未过期条目都拦路由）；
//   - 已过期的冷却不算锁。本方法只读不清理——过期条目的真正删除仍由 pick 写锁
//     路径的 pruneExpiredModelCooldowns 负责，这里按「未到期才有效」自然过滤。
//
// 只读：持 RLock 遍历，不改任何状态。无锁时返回 nil（JSON 里是 null，面板按空态渲染）。
func (p *Pool) ModelLockView() []ModelLockRow {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()

	// 第一遍：按 (域, 模型) 聚合被锁账号数、最早/全部解锁时刻与原因，同时
	// 统计各域参与选号的账号数（分母）。键 = realm + "\x1f" + model，
	// 与 pick 的 exploreLast 同款分隔符（\x1f 不会出现在模型名/域名里）。
	type agg struct {
		locked   int
		unlockAt time.Time
		fullyAt  time.Time
		reason   string
	}
	rows := make(map[string]*agg)
	routable := make(map[string]int) // realm → 参与选号的账号数

	for _, e := range p.byUID {
		realm := e.a.Realm()
		if e.disabled || e.manualDisabled {
			continue // 停用号不参与选号：不计分母，也不构成「模型被锁」的证据
		}
		routable[realm]++
		for model, mc := range e.modelCooldowns {
			if !modelLockActive(now, mc) {
				continue // 零值/已过期：与 modelCooled 同口径，不算锁
			}
			key := realm + "\x1f" + model
			a := rows[key]
			if a == nil {
				a = &agg{}
				rows[key] = a
			}
			a.locked++
			// 最早解锁：取最小 Until，原因跟随最早解锁账号（运维先看「何时能好」）。
			if a.unlockAt.IsZero() || mc.Until.Before(a.unlockAt) {
				a.unlockAt = mc.Until
				a.reason = mc.Reason
			}
			// 全部解锁：取最大 Until。
			if a.fullyAt.IsZero() || mc.Until.After(a.fullyAt) {
				a.fullyAt = mc.Until
			}
		}
	}
	if len(rows) == 0 {
		return nil
	}

	// 第二遍：对每个 (域, 模型) 统计此刻真正能服务的账号数（口径与 pick 一致：
	// healthyForModel 放行 且 未占满在途名额）。
	out := make([]ModelLockRow, 0, len(rows))
	for key, a := range rows {
		sep := strings.IndexByte(key, '\x1f')
		realm, model := key[:sep], key[sep+1:]
		row := ModelLockRow{
			Model:         model,
			Realm:         realm,
			Total:         routable[realm],
			Locked:        a.locked,
			UnlockAt:      a.unlockAt,
			FullyUnlockAt: a.fullyAt,
			Reason:        a.reason,
		}
		for _, e := range p.byUID {
			if e.a.Realm() != realm {
				continue
			}
			if e.healthyForModel(now, model) && !p.inFlightFull(e) {
				row.Servable++
			}
		}
		switch {
		case row.Servable > 0:
			// 仍有号能服务该模型：锁是部分的，请求照常能过。
			row.State = "partial"
		case row.Locked >= row.Total:
			// 参与选号的号全被这个模型挡住：换模型或等解锁才有用。
			row.State = "locked"
		default:
			// 此刻没号能服务，但模型冷却没锁满——账号级冷却/熔断/降权/在途占满
			// 所致：等一下就会好，与模型无关，换模型没用。
			row.State = "starved"
		}
		out = append(out, row)
	}

	// 排序：整池不可用（locked）最先、starved 次之、partial 最后——运维一眼看到
	// 最该处理的；同状态按模型名、域名字典序保证输出稳定（map 遍历无序）。
	rank := func(state string) int {
		switch state {
		case "locked":
			return 0
		case "starved":
			return 1
		default:
			return 2
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if ri, rj := rank(out[i].State), rank(out[j].State); ri != rj {
			return ri < rj
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Realm < out[j].Realm
	})
	return out
}

// modelLockActive 单条模型级冷却条目当前是否构成「真锁」：Until 非零且未到期。
// ModelLockView 的聚合判定与 ModelBlockedNow 的单点判定共用这一个谓词，保证两处
// 口径永远一致（此前该判定的两份内联表达式就散在 ModelLockView 里，抽取即防分叉）。
func modelLockActive(now time.Time, mc modelCooldown) bool {
	return !mc.Until.IsZero() && now.Before(mc.Until)
}

// ModelBlockedNow 回答「该 (域, 模型) 此刻是否被模型级冷却**整体**挡死」——即
// ModelLockView 口径下的 locked 态：所有参与选号的账号都被该模型自身的冷却挡住
// （6004 模型级限流 / 11102 负缓存），没有任何账号能服务它。供 handler 轮转耗尽
// 末端区分 503 口径：blocked → model_blocked（换模型或等解锁才有用），非 blocked
// （starved：账号级冷却/熔断/在途占满）仍报 no_healthy_account（等一下就会好，
// 与模型无关）。
//
// 判定语义与 ModelLockView 的 locked 态严格一致（共享 modelLockActive 谓词，
// 停用号不计分母、servable 按 healthyForModel+在途口径），只是把全清单聚合换成
// 单 (realm, model) 的轻量单遍扫描——轮转耗尽热路径上不值得为一次判定构建
// 整张全清单。
//
// 返回值：
//   - blocked：true = 该 (realm, model) 全部参与选号的账号都被该模型冷却挡住；
//     false = 不构成整体锁（无冷却 / 部分被锁仍有号可选 / 账号级原因饿死）。
//   - earliestUnlock：最早解锁时刻（第一个被锁账号恢复的时刻，blocked 时恒非零；
//     未 blocked 时零值，调用方不得使用）。
//   - reason：最早解锁账号的冷却原因（上游原文；6004 恒为 "6004 model rate limit"）。
//
// realm 为空时恒返回 false（handler 侧 resolveModel 之外的异常形态，宁可不改写
// 口径也不猜）；只读：持 RLock 遍历，不改任何状态。
func (p *Pool) ModelBlockedNow(realm, model string) (blocked bool, earliestUnlock time.Time, reason string) {
	if realm == "" || model == "" {
		return false, time.Time{}, ""
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()

	// 单遍扫描：跳过停用号（disabled / manualDisabled 与模型无关，与 ModelLockView
	// 分母口径一致），对参与选号的账号分别累计「被该模型锁住」与「此刻能服务」。
	total, locked, servable := 0, 0, 0
	for _, e := range p.byUID {
		if e.a.Realm() != realm || e.disabled || e.manualDisabled {
			continue
		}
		total++
		if mc, ok := e.modelCooldowns[model]; ok && modelLockActive(now, mc) {
			locked++
			// 最早解锁：取最小 Until，原因跟随最早解锁账号（与 ModelLockView 同口径）。
			if earliestUnlock.IsZero() || mc.Until.Before(earliestUnlock) {
				earliestUnlock = mc.Until
				reason = mc.Reason
			}
		}
		if e.healthyForModel(now, model) && !p.inFlightFull(e) {
			servable++
		}
	}
	// locked 态判定与 ModelLockView 完全一致：仍有号能服务 → 不算整体锁（partial）；
	// 全部被该模型冷却挡住（locked >= total，total==0 空域自然排除）→ 整体锁。
	return servable == 0 && locked >= total && total > 0, earliestUnlock, reason
}
