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
//
// v1.20.0 例外：prefer_expiring 开启且有快过期号时列表带虚拟槽位（boost=true），
// 此时不再与 AvailableUIDs 逐元素相同（见 availableUIDsFrom / expiringVirtualUIDs）；
// 关闭时逐字返回旧列表（零回归）。面板 models 探针只取 uids[0]，重复项无害。
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	return p.availableUIDsFrom(realm, func(e *entry, now time.Time) bool { return e.healthy(now) }, true)
}

// AvailableUIDsForModelRealm 同 AvailableUIDsForModel，但仅返回 Realm()==realm 的账号
// （6004 模型豁免照常生效）。realm=="" 退化为 AvailableUIDsForModel（v1.20.0 起：
// 开关开启且有快过期号时会带虚拟槽位，见 AvailableUIDsForRealm 的同款说明）。
//
// 这是**会话粘性分配**的生产口径（wiring.go 的 realmAwareAvailableForModel）：
// 返回的切片既是"可用集合"也是哈希打散的槽位池，快过期号的虚拟槽位因此直接
// 提升其被分配概率——与 weighted 选号的权重 ×3 语义一致（同一开关、同一判据）。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	return p.availableUIDsFrom(realm,
		func(e *entry, now time.Time) bool { return e.healthyForModel(now, model) }, true)
}

// expiringVirtualUIDs 把基础可用 UID 列表按「快过期积分优先消耗」展开：
// rest（基础列表去掉合格项，保持原有升序相对序）+ 合格项各 expiringVirtualSlots
// 份（按 UID 升序垫尾）。
//
// 为什么"移出原位 + 垫尾"而不是原地保留再追加：语义是「虚拟实例」——合格号在
// 槽位池里恰好占 expiringVirtualSlots 份（与 weighted 路径"等价被放三份"逐字对应），
// 基础列表里的那一份由虚拟份取代；若原地保留 + 追加 3 份就变成 4 份，与权重 ×3 不
// 一致（同一开关在两个路径上必须是同一个倍数，否则面板上"开关效果"无法解释）。
//
// 确定性：输入 base/exp 均已按 UID 升序（availableUIDsFrom 内 sort），本函数不做
// map 遍历，输出对同一输入完全确定（会话粘性的哈希分配要求可复现）。
// 调用方保证 exp ⊆ base（同一遍历收集）。
func expiringVirtualUIDs(base, exp []string) []string {
	inExp := make(map[string]bool, len(exp))
	for _, u := range exp {
		inExp[u] = true
	}
	slots := int(expiringVirtualSlots)
	out := make([]string, 0, len(base)-len(exp)+len(exp)*slots)
	for _, u := range base {
		if !inExp[u] {
			out = append(out, u) // 非合格号：保持原升序相对序
		}
	}
	for _, u := range exp {
		for i := 0; i < slots; i++ {
			out = append(out, u)
		}
	}
	return out
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
