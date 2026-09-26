// 分池选号域：按 realm（cn/global）过滤选号与可用集合。realm=="" 退化为现状。
package pool

import (
	"sort"
	"time"

	"workbuddy2api/internal/auth"
)

// PickExcludingForRealm 按 realm 过滤的轮换选号：候选仅限 Realm()==realm 的账号。
// realm=="" 退化为 PickExcluding（现状语义，老调用零改动）。
// 可选做请求级轮换（tried）与模型感知（reqModel，6004 模型豁免照常生效）；
// reqModel 非空时健康口径换成 healthyForModel。realm 不匹配的全冷却兜底同样排除。
func (p *Pool) PickExcludingForRealm(tried map[string]bool, reqModel, realm string) *auth.Auth {
	return p.pick(tried, reqModel, realm)
}

// AvailableUIDsForRealm 同 AvailableUIDs，但仅返回 Realm()==realm 的账号。
// DeptestOnly: 仅 realm_test.go 引用；生产经 wiring.go 走
// AvailableUIDsForModelRealm。保留作 ForModelRealm 的模型维度退化
// （model=""）语义锚点测试。
// realm=="" 退化为 AvailableUIDs（现状语义）。
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	return p.availableUIDsFrom(realm, func(e *entry, now time.Time) bool { return e.healthy(now) })
}

// AvailableUIDsForModelRealm 同 AvailableUIDsForModel，但仅返回 Realm()==realm 的账号
// （6004 模型豁免照常生效）。realm=="" 退化为 AvailableUIDsForModel。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	return p.availableUIDsFrom(realm,
		func(e *entry, now time.Time) bool { return e.healthyForModel(now, model) })
}

// StickyPreferredForModelRealm 粘性首次分配的策略感知优先序（session 包注入）：
// 选号策略为 credits_desc 时返回"该 (realm, 模型) 可用账号按缓存余额降序"的列表
// （含 6004 模型豁免与在途占满过滤，与 AvailableUIDsForModelRealm 同一可用性口径，
// 仅排序不同）——粘性新会话沿此序绑定最高余额号，而非哈希打散；weighted（默认）
// 返回 nil，粘性保持既有哈希打散行为零改动。
//
// 排序口径与 pick 的 credits_desc 分支完全一致（credits 降序 → 闲置久者在前 →
// uid 升序稳定），首名即 pick 在同一快照下会选中的号。
// 排序在池写锁外的副本上进行：余额/lastUsed 读的是 RLock 快照，与选号竞态无害
// （粘性绑定是长寿命分配，绑定时刻的次序领先即满足"优先最高余额"语义）。
func (p *Pool) StickyPreferredForModelRealm(model, realm string) []string {
	p.mu.RLock()
	if p.pickStrategy != StrategyCreditsDesc {
		p.mu.RUnlock()
		return nil
	}
	now := time.Now()
	type cand struct {
		uid      string
		credits  int64
		lastUsed time.Time
	}
	cands := make([]cand, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		cands = append(cands, cand{uid: uid, credits: e.credits, lastUsed: e.lastUsed})
	}
	p.mu.RUnlock()
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].credits != cands[j].credits {
			return cands[i].credits > cands[j].credits
		}
		li, lj := cands[i].lastUsed, cands[j].lastUsed
		switch {
		case li.IsZero() && !lj.IsZero():
			return true // 从未使用 → 闲置最久
		case !li.IsZero() && lj.IsZero():
			return false
		case !li.IsZero() && !lj.IsZero() && !li.Equal(lj):
			return li.Before(lj)
		}
		return cands[i].uid < cands[j].uid
	})
	uids := make([]string, len(cands))
	for i, c := range cands {
		uids[i] = c.uid
	}
	return uids
}
