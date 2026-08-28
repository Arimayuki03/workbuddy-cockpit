// Package pool 账号池：内存索引 + 冷却/禁用状态机 + state.json 持久化。
// 挑选策略：healthy 账号中取 credits Top5 加权随机（全 0 退化为均匀随机）。
package pool

import (
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
)

// CoolKind 冷却类型。
type CoolKind int

const (
	CoolHard CoolKind = iota // 余额不足 → 长冷却
	CoolSoft                 // 429 → 短冷却
	CoolErr                  // 连续错误 → 中冷却
)

func (k CoolKind) String() string {
	switch k {
	case CoolHard:
		return "hard_credit"
	case CoolSoft:
		return "soft_rate"
	case CoolErr:
		return "error_threshold"
	}
	return "unknown"
}

// Status 单个账号对外暴露的状态（脱敏）。
type Status struct {
	UID      string    `json:"uid"`
	Nickname string    `json:"nickname,omitempty"`
	Credits  int64     `json:"credits"`
	Cooling  bool      `json:"cooling"`
	Until    time.Time `json:"until,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Disabled bool      `json:"disabled"`
	ErrCount int       `json:"err_count,omitempty"`
}

type entry struct {
	a        *auth.Auth
	credits  int64
	disabled bool
	reason   string
	until    time.Time
	errCount int
}

func (e *entry) healthy(now time.Time) bool {
	if e.disabled {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	return true
}

// stateFile 持久化格式。
type stateFile struct {
	Accounts map[string]struct {
		Credits  int64     `json:"credits"`
		Disabled bool      `json:"disabled"`
		Reason   string    `json:"reason,omitempty"`
		Until    time.Time `json:"until,omitempty"`
	} `json:"accounts"`
}

// flushInterval 后台落盘周期。
var flushInterval = 5 * time.Second

// Pool 账号池。
type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
	dirty   atomic.Bool // 内存有变更待落盘

	// randInt64N 仅供测试注入确定性随机源；nil 时用 math/rand/v2 全局源。
	// 生产代码不应设置此字段。
	randInt64N func(n int64) int64
}

// New 构建池；stateFp 非空时尝试加载旧状态，并启动后台周期性落盘 goroutine。
func New(stateFp string) *Pool {
	p := &Pool{byUID: map[string]*entry{}, stateFp: stateFp}
	if stateFp != "" {
		p.load()
		p.startFlusher()
	}
	return p
}

// SetRandomSource 仅供测试注入确定性随机源；生产代码不应调用。
// 注入源取 n∈[0,n) 后，pickWeighted 的抽签结果完全可预测。
func (p *Pool) SetRandomSource(fn func(n int64) int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.randInt64N = fn
}

// startFlusher 每 flushInterval 检查 dirty 标志，有变更则 saveLocked 落盘。
func (p *Pool) startFlusher() {
	go func() {
		t := time.NewTicker(flushInterval)
		defer t.Stop()
		for range t.C {
			p.mu.Lock()
			if p.dirty.Swap(false) {
				p.saveLocked()
			}
			p.mu.Unlock()
		}
	}()
}

// Flush 同步把内存状态落盘（幂等：无变更不写盘）。供进程退出前调用。
func (p *Pool) Flush() {
	p.mu.Lock()
	if p.dirty.Swap(false) {
		p.saveLocked()
	}
	p.mu.Unlock()
}

// Add 加入账号；已存在则保留原状态、更新凭证。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a // 保留 credits/cooling 状态
		return
	}
	p.byUID[a.UID] = &entry{a: a}
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
// 剔除结果持久化回 state.json，避免已删账号在下次启动时被 load() 复活。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, a := range auths {
		seen[a.UID] = true
		if e, ok := p.byUID[a.UID]; ok {
			e.a = a
		} else {
			p.byUID[a.UID] = &entry{a: a}
		}
	}
	changed := false
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
			changed = true
		}
	}
	if changed {
		p.saveLocked()
	}
}

// Pick 返回 healthy 中积分最高的账号；无可用返回 nil。
func (p *Pool) Pick() *auth.Auth {
	return p.PickExcluding(nil)
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
// 挑选策略：healthy 账号中取 credits 最高的前 5 名，按 credits 为权重随机抽签
// （credits 全为 0 时退化为均匀随机）。意图是打散热点，避免永远打同一个账号。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	top := p.topHealthy(tried, 5)
	if len(top) == 0 {
		return nil
	}
	return p.pickWeighted(top).a
}

// pickWeighted 按 credits 权重随机抽签；权重总和为 0 时退化为均匀随机。
// 输入假定已按 credits 降序（候选集 = Top5，仍是全量 healthy 的近似）。
// 随机源优先用 p.randInt64N（仅供测试注入确定性），nil 时回退 math/rand/v2 全局源。
func (p *Pool) pickWeighted(cands []*entry) *entry {
	var total int64
	for _, e := range cands {
		total += e.credits
	}
	rnd := rand.Int64N
	if p.randInt64N != nil {
		rnd = p.randInt64N
	}
	if total <= 0 {
		return cands[int(rnd(int64(len(cands))))]
	}
	r := rnd(total)
	var acc int64
	for _, e := range cands {
		acc += e.credits
		if r < acc {
			return e
		}
	}
	return cands[len(cands)-1]
}

// topHealthy 返回 healthy 账号按 credits 降序的前 n 名（不足 n 取全部，按 uid 稳定排序）。
func (p *Pool) topHealthy(tried map[string]bool, n int) []*entry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var cands []*entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		cands = append(cands, e)
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].credits != cands[j].credits {
			return cands[i].credits > cands[j].credits
		}
		return cands[i].a.UID < cands[j].a.UID
	})
	if len(cands) > n {
		cands = cands[:n]
	}
	return cands
}

// SetCredits 更新账号余额。
func (p *Pool) SetCredits(uid string, credits int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
		p.dirty.Store(true)
	}
}

// Cooldown 冷却账号至 now+d。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.reason = reason
		e.errCount = 0
		p.dirty.Store(true)
	}
}

// CooldownUntilTomorrow4AM 冷却到次日 04:00（本地时区）。
// 用于 ErrHardCredit 场景：积分耗尽账号等签到任务（09:00/21:00）恢复。
func (p *Pool) CooldownUntilTomorrow4AM(uid string, reason string) {
	now := time.Now()
	p.Cooldown(uid, CoolHard, nextDay4AM(now).Sub(now), reason)
}

// nextDay4AM 返回 now 所属日期的次日 04:00（与 now 同一时区）。
// time.Date 对日溢出自动进位（月末→下月 1 号、年末→下年 1 号），天然覆盖跨日/跨月/跨年。
func nextDay4AM(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), now.Day()+1, 4, 0, 0, 0, now.Location())
}

// Disable 永久禁用（session 死亡），需人工重登后手工恢复或文件替换。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
		p.dirty.Store(true)
	}
}

// ReenableIfCredits 签到后解冻：仅当 remain > 0 且账号处于冷却（非禁用）时恢复。
func (p *Pool) ReenableIfCredits(uid string, remain int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = remain
		if remain > 0 && !e.disabled {
			e.until = time.Time{}
			e.reason = ""
			e.errCount = 0
		}
		p.dirty.Store(true)
	}
}

// NoteError 记录一次非余额/非 429 错误；达到 threshold 自动冷却 d 时长。
func (p *Pool) NoteError(uid string, threshold int, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount++
		if e.errCount >= threshold {
			e.until = time.Now().Add(d)
			e.reason = "consecutive errors"
			e.errCount = 0
		}
		p.dirty.Store(true)
	}
}

// NoteSuccess 成功请求重置错误计数。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount = 0
	}
}

// Status 查询单账号状态。
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID 返回账号的完整凭证（给调度器/运维接口用）。
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// List 返回所有账号状态（按 UID 排序，稳定输出）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}

func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	return Status{
		UID:      uid,
		Nickname: e.a.Nickname,
		Credits:  e.credits,
		Cooling:  !e.until.IsZero() && now.Before(e.until),
		Until:    e.until,
		Reason:   e.reason,
		Disabled: e.disabled,
		ErrCount: e.errCount,
	}
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	for uid, s := range sf.Accounts {
		p.byUID[uid] = &entry{
			a:        &auth.Auth{UID: uid}, // placeholder，Add 时会换成完整凭证
			credits:  s.Credits,
			disabled: s.Disabled,
			reason:   s.Reason,
			until:    s.Until,
		}
	}
}

func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := stateFile{Accounts: map[string]struct {
		Credits  int64     `json:"credits"`
		Disabled bool      `json:"disabled"`
		Reason   string    `json:"reason,omitempty"`
		Until    time.Time `json:"until,omitempty"`
	}{}}
	for uid, e := range p.byUID {
		sf.Accounts[uid] = struct {
			Credits  int64     `json:"credits"`
			Disabled bool      `json:"disabled"`
			Reason   string    `json:"reason,omitempty"`
			Until    time.Time `json:"until,omitempty"`
		}{
			Credits:  e.credits,
			Disabled: e.disabled,
			Reason:   e.reason,
			Until:    e.until,
		}
	}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p.stateFp)
}
