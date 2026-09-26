// Package session 会话粘性路由：同一会话（conversationId / metadata 键）尽量绑定同一账号。
//
// 设计参考 antigravityProxyGo internal/session（fast-path RLock / 双段分配 / TTL / 持久化），
// 但改为纯内存 + redisstore 异步镜像：
//   - 命中走 RLock 快查（绝大多数请求已绑定）；
//   - 未命中/失效走写锁 re-check 后分配，避免同 key 并发重复分配（TOCTOU 防护）；
//   - 分配优先"空闲账号"（未绑定任何会话的可用号）哈希，其次全池哈希（双段策略）；
//   - LastActive 滚动续期，TTL 过期由后台 GC 或快路径惰性过期清理；
//   - 每次绑定变更 fire-and-forget 镜像到 redisstore（防重启丢粘性）。
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/redisstore"
)

// entry 单条会话绑定。
type entry struct {
	uid        string
	lastActive time.Time
}

// Config 路由依赖；Available 返回"可用账号"（healthy 且未占满在途）的有序 uid 列表，
// 由 pool.AvailableUIDs 提供。Store 可为 redisstore.Noop（纯内存）。
type Config struct {
	TTL        time.Duration
	GCInterval time.Duration
	Store      redisstore.Store
	Available  func() []string
	// AvailableForModel 按请求模型返回"在该模型上可用"的账号
	// （healthy 且未占满在途，且未被该模型限流/限额）。nil 时回落 Available
	// （无模型维度，行为与引入前一致）。
	//
	// 为什么粘性需要模型维度：绑定只记 uid，而同一个会话可能换模型。账号被 6004
	// 模型级限额后对**其他模型**仍可用（issue #31 豁免），此时若只按账号级可用性
	// 校验，会话会被钉在这个号上反复失败——正是"限额后换不动号"的观感来源。
	AvailableForModel func(model string) []string
	// PreferredForModel 按请求模型返回"首次分配优先序"（可选）。nil 或返回空时
	// 回落既有哈希打散（行为与引入前一致）。
	//
	// 为什么需要：粘性首次分配原本是 hashIndex 哈希打散（设计初衷是多会话分摊
	// 流量），完全绕过 pool 的选号策略——credits_desc（余额从大到小）下新会话
	// 仍会被随机分到低余额号，直到绑定号不可用才重分配，"余额优先"对新会话
	// 形同虚设。注入后（wiring 接 pool.StickyPreferredForModelRealm）：
	// credits_desc 时返回按余额降序的可用账号，每个新会话都绑序首（最高余额
	// 号；并发的权威闸门是账号在途上限而非粘性，撞限自愈闭环见 ResolveForModel
	// 注释）；weighted 返回 nil，粘性保持哈希打散零改动。既有绑定（含 Redis
	// 恢复的）不受影响——优先序只决定"给新会话绑谁"，不动"已绑定的继续用"。
	PreferredForModel func(model string) []string
}

// Router 会话粘性路由器。
type Router struct {
	mu      sync.RWMutex
	entries map[string]entry
	cfg     Config
	stop    chan struct{}
}

// maxEntries 单路由器粘性条目上限：防异常客户端逐请求换会话键（随机
// conversationId 等）在 TTL 窗口内无限累积条目吃干内存。量级对齐 TTL 30m——
// 正常客户端 30 分钟内的会话数远小于该值，正常路径永远不会触达淘汰分支。
// 写满时 Bind 按 lastActive 淘汰最旧条目；ResolveForModel 分配路径不淘汰
//（最多多占一个槽位，GC 周期兜底回收过期条目）。
const maxEntries = 10000

// New 构建路由器。若 cfg.Store 为 nil 则用 Noop（纯内存）；cfg.Available 为 nil 视为空池。
// TTL/GCInterval 非正取默认（30m / 5m）——main 从 config 解析后传入，这里兜底。
func New(cfg Config) *Router {
	if cfg.Store == nil {
		cfg.Store = redisstore.Noop{}
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * time.Minute
	}
	if cfg.GCInterval <= 0 {
		cfg.GCInterval = 5 * time.Minute
	}
	return &Router{entries: map[string]entry{}, cfg: cfg}
}

// StartGC 启动后台 GC goroutine（幂等）。进程退出时调 StopGC。
//
// stop channel 必须在启 goroutine 前捕获到**局部变量**：goroutine 在 select 里
// 每轮重新求值 r.stop 是无锁读，而 StopGC 持写锁把它置 nil——既是数据竞争
// （-race 可复现），又会在读到 nil 后让该 case 永久阻塞（nil channel 永不就绪），
// 于是关停彻底失效：goroutine 再也不会退出，ticker 无限触发 gcOnce（goroutine
// 泄漏 + 关停后仍持续 GC）。捕获局部变量后，close(stop) 与 select 观测的是同一个
// channel，StopGC 一定能让 goroutine 退出。
func (r *Router) StartGC() {
	r.mu.Lock()
	if r.stop != nil {
		r.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	r.stop = stop
	r.mu.Unlock()

	go func() {
		t := time.NewTicker(r.cfg.GCInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				r.gcOnce(time.Now())
			}
		}
	}()
}

// StopGC 停止后台 GC（幂等）。
func (r *Router) StopGC() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stop != nil {
		close(r.stop)
		r.stop = nil
	}
}

// LoadFromStore 启动时从 redisstore 恢复绑定（内存覆盖本地，读操作仅此处发生）。
// 已有本地绑定被保留——Redis 仅为恢复备份，本地一旦建立即为权威。
func (r *Router) LoadFromStore() {
	binds := r.cfg.Store.LoadBinds()
	if len(binds) == 0 {
		return
	}
	now := time.Now()
	r.mu.Lock()
	loaded := 0
	for key, uid := range binds {
		if _, exists := r.entries[key]; exists {
			continue
		}
		r.entries[key] = entry{uid: uid, lastActive: now}
		loaded++
	}
	r.mu.Unlock()
	if loaded > 0 {
		log.Printf("[session] 从 Redis 恢复 %d 条粘性会话绑定", loaded)
	}
}

// ResolveForModel 返回会话 key 在该模型上应绑定的账号 uid。
// 命中且账号在该模型可用 → 滚动 lastActive 并直接返回；否则（绑定号已冷却/占满/
// 被该模型限流）走重新分配。
//
// 为什么必须带模型：绑定只记 uid，同一个会话可能换模型；账号被 6004 模型级限额后
// 对其他模型仍可用（见 pool.healthyForModel 的模型级冷却豁免）。若只按账号级
// 可用性校验，会话会被钉在一个"对当前模型不可用"的号上反复失败。
func (r *Router) ResolveForModel(key, model string) (string, bool) {
	now := time.Now()
	// available 惰性构建：仅当快路径命中且未过期（或慢路径 re-check 命中）时
	// 才调用可用性回调建集合——键不存在/粘性未命中的请求不必白建（原实现
	// 在查 entries 之前无条件构建，多数未命中请求白付一次回调开销）。
	var available map[string]bool

	// ── Fast path: RLock 快查 ──────────────────────────────
	r.mu.RLock()
	e, found := r.entries[key]
	r.mu.RUnlock()
	if found && !expired(e, now, r.cfg.TTL) {
		if available == nil {
			available = r.availableSet(model)
		}
		if available[e.uid] {
			r.touch(key, e.uid, now)
			return e.uid, true
		}
		// 绑定号在该模型上已冷却/占满/被限流 → 失效，落入慢路径重分配。
	}

	// ── Slow path: 写锁 re-check 后分配 ────────────────────
	r.mu.Lock()
	defer r.mu.Unlock()

	// re-check：并发同 key 可能已被其他 goroutine 分配好。
	if e2, found2 := r.entries[key]; found2 && !expired(e2, now, r.cfg.TTL) {
		if available == nil {
			available = r.availableSet(model)
		}
		if available[e2.uid] {
			r.entries[key] = entry{uid: e2.uid, lastActive: now}
			return e2.uid, true
		}
		delete(r.entries, key) // 失效：清掉再分配
	}

	uids := r.availableSlice(model)
	if len(uids) == 0 {
		return "", false
	}

	// 双段策略：优先"空闲账号"（未被任何会话绑定的可用号），其次全池。
	bound := map[string]bool{}
	for _, v := range r.entries {
		bound[v.uid] = true
	}
	// 策略感知优先序（credits_desc 余额降序）：新会话优先绑序首（最高余额号），
	// 序首已被其他会话绑定则沿序下探——与 pool.pick 的"最高者不可用才顺延次高"
	// 同向。序内候选必须仍在当前可用集（uids）里（序是 Available 之外的独立
	// 快照，冷却/占满可能刚发生）；全序被绑或序外候选才回落哈希打散。
	// weighted（回调 nil 或返回空）不进此分支，哈希打散零改动。
	// 注意 bound 只含本路由器内的会话绑定：允许两个并发会话短暂共享同一最高
	// 余额号——账号在途上限（max_in_flight）才是并发的权威闸门，粘性的职责
	// 是会话连续性不是并发分摊；真撞上限时 PickByUIDForModel 返回 nil，handler
	// 解绑重分配，自愈闭环（与绑定额满的既有语义一致）。
	if r.cfg.PreferredForModel != nil {
		if pref := r.cfg.PreferredForModel(model); len(pref) > 0 {
			availableSet := make(map[string]bool, len(uids))
			for _, u := range uids {
				availableSet[u] = true
			}
			for _, u := range pref {
				if availableSet[u] {
					// 命中优先序：绑定 + 镜像（与下方哈希分支同口径）。
					prev, existed := r.entries[key]
					r.entries[key] = entry{uid: u, lastActive: now}
					if existed && prev.uid != u {
						r.cfg.Store.DelBind(key)
					}
					r.cfg.Store.SetBind(key, u, redisstore.MaxMirrorTTL)
					return u, true
				}
			}
		}
	}
	var idle []string
	for _, u := range uids {
		if !bound[u] {
			idle = append(idle, u)
		}
	}
	pool2 := idle
	if len(pool2) == 0 {
		pool2 = uids
	}
	uid := pool2[hashIndex(key, len(pool2))]

	prev, existed := r.entries[key]
	r.entries[key] = entry{uid: uid, lastActive: now}
	if existed && prev.uid != uid {
		r.cfg.Store.DelBind(key)
	}
	// 镜像 TTL 与内存 TTL 解耦：redisstore.MaxMirrorTTL(7d)。镜像的职责是扛长停机
	// 重启(防丢粘性),内存 TTL(30m)只管在线滚动——停机超 30m 后内存 map 清空,
	// 镜像若同 TTL 也已过期,LoadFromStore 恢复 0 条,镜像在其最需要的长停机场景
	// 失效。恢复侧按 lastActive=now 重置内存 TTL,旧绑定复活无副作用。
	r.cfg.Store.SetBind(key, uid, redisstore.MaxMirrorTTL)
	return uid, true
}

// touch 滚动 lastActive 并异步镜像（只在快路径命中时写最后一次）。
// CAS 语义：写覆盖前比对 entries[key].uid == uid——并发窗口内该绑定可能刚被
// Unbind（粘性号失败）或被 Bind 改绑到别的号，无条件覆盖会把旧 uid "复活"回去
// （复活后该会话继续打失败号/旧号，直到 TTL 或下次 Unbind 才纠正）。uid 已变
// 则放弃 touch（lastActive 略旧无害，TTL 30m 兜底）。
func (r *Router) touch(key, uid string, now time.Time) {
	r.mu.Lock()
	// 仅当绑定仍存在且 uid 未变才写（键缺失 = 刚被 Unbind，不复活）。
	if cur, ok := r.entries[key]; ok && cur.uid == uid {
		r.entries[key] = entry{uid: uid, lastActive: now}
		r.mu.Unlock()
		r.cfg.Store.SetBind(key, uid, redisstore.MaxMirrorTTL) // 镜像 TTL 解耦(7d,见 Bind 注释)
		return
	}
	r.mu.Unlock()
}

// Bind 显式把会话 key 绑定到 uid（幂等覆盖旧值），并异步镜像到 redisstore。
// 供"粘性跟随最终成功号"用：请求成功返回前，把会话重绑到实际成功的账号，让多轮对话下一跳稳定
// 收敛到"对该会话持续成功的号"（对齐 antigravity 语义）。空 key 直接返回（无会话则不绑）。
//
// 写入前检查容量上限 maxEntries：写满时按 lastActive 淘汰最旧条目（全扫一遍取
// 最小，N=10000 时每次绑定的全扫开销可接受），并同步 DelBind 镜像。防异常
// 客户端逐请求换键在 TTL 窗口内无界泛洪 entries。
func (r *Router) Bind(key, uid string) {
	if key == "" || uid == "" {
		return
	}
	now := time.Now()
	evicted := ""
	r.mu.Lock()
	if _, exists := r.entries[key]; !exists && len(r.entries) >= maxEntries {
		// 新键且已满：按 lastActive 淘汰最旧条目（TTL 内最久未活跃的会话）。
		var oldest time.Time
		first := true
		for k, e := range r.entries {
			if first || e.lastActive.Before(oldest) {
				evicted, oldest, first = k, e.lastActive, false
			}
		}
		if evicted != "" {
			delete(r.entries, evicted)
		}
	}
	r.entries[key] = entry{uid: uid, lastActive: now}
	r.mu.Unlock()
	if evicted != "" {
		r.cfg.Store.DelBind(evicted)
	}
	r.cfg.Store.SetBind(key, uid, redisstore.MaxMirrorTTL) // 镜像 TTL 解耦(7d,见 resolve 注释)
}

// Unbind 解除会话绑定（请求失败时调用，让该会话下次重新分配）。返回是否存在。
func (r *Router) Unbind(key string) bool {
	r.mu.Lock()
	_, found := r.entries[key]
	if found {
		delete(r.entries, key)
	}
	r.mu.Unlock()
	if found {
		r.cfg.Store.DelBind(key)
	}
	return found
}

// Count 返回当前绑定数（供 /status 观测）。
func (r *Router) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// gcOnce 清理 TTL 过期的绑定，并镜像删除。
func (r *Router) gcOnce(now time.Time) int {
	r.mu.Lock()
	var expiredKeys []string
	for key, e := range r.entries {
		if now.Sub(e.lastActive) > r.cfg.TTL {
			expiredKeys = append(expiredKeys, key)
		}
	}
	for _, key := range expiredKeys {
		delete(r.entries, key)
	}
	r.mu.Unlock()
	for _, key := range expiredKeys {
		r.cfg.Store.DelBind(key)
	}
	return len(expiredKeys)
}

// availableSet 把可用账号列表转集合（快路径命中校验用）。
func (r *Router) availableSet(model string) map[string]bool {
	uids := r.availableSlice(model)
	set := make(map[string]bool, len(uids))
	for _, u := range uids {
		set[u] = true
	}
	return set
}

// availableSlice 安全调用可用账号函数（nil 函数视空池）。
// 优先走 AvailableForModel（带模型过滤）；未注入时回落 Available（无模型维度）。
func (r *Router) availableSlice(model string) []string {
	if r.cfg.AvailableForModel != nil {
		return r.cfg.AvailableForModel(model)
	}
	if r.cfg.Available == nil {
		return nil
	}
	return r.cfg.Available()
}

func expired(e entry, now time.Time, ttl time.Duration) bool {
	return now.Sub(e.lastActive) > ttl
}

// hashIndex FNV-1a 哈希取模（antigravity 双段分配的稳定散列）。
func hashIndex(key string, n int) int {
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return int(h % uint32(n))
}

// ExtractKey 从请求体提取会话键；按下列顺序依次尝试，找不到返回空串（绝不失败）。
//  1. metadata.conversation_id
//  2. metadata.conversationId
//  3. conversation_id
//  4. conversationId
//  5. prompt_cache_key（第 5 项，见下）
//
// 前四项均为 conversation 维度（对话级）。metadata.user_id 不再作为粘性键
// （P1-anti-monopoly 剔除，issue118-deep-review §3）：user 维度粒度过粗——一个
// user 的全部并行对话会钉同一账号（粘性范围远大于上游 prompt cache 的对话级边界），
// 且曾抢占顶层 conversation_id 的优先级。剔除后发 user_id 的客户端回落加权轮换
// （与无标识客户端同路径），旧 user_id 绑定靠 TTL（30m 滚动）与 Redis 镜像 TTL
// （7d 兜底）自然过期，键消失不产生脏绑定。
//
// issue #35：客户端实际发 camelCase 的 conversationId，此前只识别 snake_case，
// 导致粘性路由不命中、同对话轮转不同账号、上游上下文缓存 miss。现两种命名均识别，
// snake_case 优先级高于 camelCase（同值不同名命中同一对话时返回相同值，天然不混用）。
//
// 第 5 项 prompt_cache_key：pi-ai 驱动的客户端（dsh 等）把会话 ID 放在这个 OpenAI
// 前缀缓存字段里（而非 conversation_id），网关在 upstream 侧本就认它（见
// InjectPromptCacheKey 优先级 1：客户端自带则原值保留）。纳入识别后，这类客户端
// 无需改配置即可命中粘性。置于最后，绝不抢占 conversation 维度的优先级。
func ExtractKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strOrEmpty(meta["conversation_id"]); v != "" {
			return v
		}
		if v := strOrEmpty(meta["conversationId"]); v != "" {
			return v
		}
	}
	if v := strOrEmpty(obj["conversation_id"]); v != "" {
		return v
	}
	if v := strOrEmpty(obj["conversationId"]); v != "" {
		return v
	}
	// 5. prompt_cache_key：OpenAI 系的会话级前缀缓存键，语义就是"同一会话复用同一
	//    前缀"，与粘性诉求同源。部分客户端（pi-ai 驱动的 dsh 等）把会话 ID 放在这里
	//    而非 conversation_id——见 upstream.InjectPromptCacheKey 的优先级 1：客户端
	//    自带 key 即原值保留。放最后，不抢占 conversation 维度的优先级。
	if v := strOrEmpty(obj["prompt_cache_key"]); v != "" {
		return v
	}
	return ""
}

// StickyFallbackKey 为**无会话标识**的客户端派生会话级稳定粘性键。
//
// 为什么需要：OpenAI 兼容协议本身没有会话 ID 字段。dsh / Codex / Cherry Studio 等
// 客户端的请求体里既无 conversationId 也无 metadata，ExtractKey 恒返回空串 →
// 粘性路由永不参与 → 同一段连续请求在账号池里逐请求轮换换号（上游前缀缓存也被打散，
// 费用上升）。本函数给这类客户端一个不依赖其配合的会话级键：
// body 里**首条** role=="user" 消息文本的 sha256 前 16 字节。
//
// 为什么取首条：会话内历史不断追加，但首条 user 消息在整段会话中恒定 → 同会话恒同键；
// 用户开新会话（首条消息不同）→ 自然换键。
//
// 与 TurnKey 的区别（勿混用）：TurnKey 取**最后一条** user 消息，是**轮级**键，供上游
// 会话头族按"对话轮"聚合；本函数取**首条**，是**会话级**键，供粘性绑定长期复用。
//
// 抑制条件（P1-anti-monopoly 契约在 fallback 路径的延伸）：body 携带
// metadata.user_id 或顶层 user_id 时**恒返回 ""**。ExtractKey 有意剔除 user_id
// 作粘性键（user 维度粒度过粗——一个 user 的全部并行对话会被钉到同一账号，远粗于
// 上游对话级缓存边界），这类客户端按契约回落加权轮换。若 fallback 不设此闸，
// 只发 user_id 的请求会借首条 prompt 重新获得粘性，使该契约在 handler 侧失效。
//
// 无 body / 无 messages / 无 user 消息 / 该消息无文本 → ""（调用方回落无粘性，
// 保持旧行为；不伪造会话）。
func StickyFallbackKey(body []byte) string {
	if hasUserID(body) {
		return ""
	}
	text := firstUserText(body)
	if text == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	return "fb:" + hex.EncodeToString(sum[:16])
}

// hasUserID 报告 body 是否携带 user 维度标识（metadata.user_id 或顶层 user_id）。
// 只判"字段存在且为非空字符串"，与 ExtractKey 的 strOrEmpty 口径一致。
// 解析失败按"无 user_id"处理（不因坏 body 抑制 fallback——坏 body 本就在
// firstUserText 里返回 ""，两条路径都收敛到无粘性）。
func hasUserID(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return false
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if strOrEmpty(meta["user_id"]) != "" {
			return true
		}
	}
	return strOrEmpty(obj["user_id"]) != ""
}

// firstUserText 取 body 里**首条** role=="user" 消息的内容签名（去首尾空白）；
// 无则 ""。签名走 ids.go contentSignature：纯文本与旧 contentText 结果一致
// （存量粘性键零漂移），纯图片轮可签名（首图会话的粘性盲区修复，G1）。
func firstUserText(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	for i := range obj.Messages {
		if obj.Messages[i].Role != "user" {
			continue
		}
		if text := strings.TrimSpace(contentSignature(obj.Messages[i].Content)); text != "" {
			return text
		}
		// 首条 user 消息无可签名内容（空/null 等）→ 不继续往后找：往后找会让
		// 键随会话推进而漂移（一旦某轮该位置带上文本），破坏"同会话恒同键"。
		return ""
	}
	return ""
}

// strOrEmpty 把 JSON 字符串字段安全转 string（非字符串类型返回空）。
func strOrEmpty(v any) string {
	s, _ := v.(string)
	return s
}

// Parsed 一次解析请求体得到的全部派生值（handler 热路径的"解析一次、多处复用"）：
// 此前 handler 对同一 body 依次做 peek + ExtractKey + StickyFallbackKey + TurnKey +
// hasImagePart + ResolveConversationID 六轮全量 JSON 解码（MB 级请求体上每轮都是
// 一遍完整 unmarshal）。ParseRequest 两次解码（原六次：顶层 map 与 messages 骨架
// 各一遍）后，各字段在同一份结果上求值，字段值与旧独立函数**逐字段等价**（每个
// 字段委托既有实现或同口径复刻，行为契约不变）。
type Parsed struct {
	// Stream / Model 出站路由需要的顶层形态（旧 peek 结构）。
	Stream bool
	Model  string
	// SessKey 会话键（ExtractKey 契约：metadata/顶层 conversation 维度 +
	// prompt_cache_key 兜底，找不到为空）。
	SessKey string
	// StickyKey 粘性专用键（SessKey 非空取 SessKey；空时 StickyFallbackKey——
	// 首条 user 消息派生，user_id 在场恒空）。
	StickyKey string
	// TurnKey 轮级聚合键（最后一条 user 消息"序号+内容签名"，无轮为空）。
	TurnKey string
	// ConversationID 会话头族 conversationId（只认 conversation 维度，缺省空）。
	ConversationID string
	// HasImage 请求是否携带 image_url part（hint 判定，畸形/其他形态 false）。
	HasImage bool
}

// ParseRequest 两次解码（顶层 map + messages 骨架，原六次全量解码）并填充
// Parsed。畸形 JSON（顶层非对象/语法错误）时
// 各字段取旧函数路径的同款零值：Stream=false、Model=""、SessKey/StickyKey/
// TurnKey/ConversationID=""、HasImage=false（与各函数逐个喂坏 body 的行为一致）。
func ParseRequest(body []byte) Parsed {
	var p Parsed
	if len(body) == 0 {
		return p
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return p
	}
	// 顶层形态：与旧 peek struct { Stream bool; Model string } 同口径——
	// 类型断言失败（非 bool/非 string）取零值，与 json.Unmarshal 到 bool/string
	// 字段失败时整体报错、字段留零值的行为一致。
	if v, ok := obj["stream"].(bool); ok {
		p.Stream = v
	}
	p.Model = strOrEmpty(obj["model"])

	// 会话键：与 ExtractKey 完全同序同口径（metadata 优先、snake 优先于 camel、
	// prompt_cache_key 兜底）。
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strOrEmpty(meta["conversation_id"]); v != "" {
			p.SessKey = v
		} else if v := strOrEmpty(meta["conversationId"]); v != "" {
			p.SessKey = v
		}
	}
	if p.SessKey == "" {
		p.SessKey = strOrEmpty(obj["conversation_id"])
	}
	if p.SessKey == "" {
		p.SessKey = strOrEmpty(obj["conversationId"])
	}
	if p.SessKey == "" {
		p.SessKey = strOrEmpty(obj["prompt_cache_key"])
	}

	// 会话头族 conversationId：与 ResolveConversationID 同序（metadata/snake/camel，
	// 不回落 prompt_cache_key 与 user 维度）。
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strOrEmpty(meta["conversation_id"]); v != "" {
			p.ConversationID = v
		} else if v := strOrEmpty(meta["conversationId"]); v != "" {
			p.ConversationID = v
		}
	}
	if p.ConversationID == "" {
		p.ConversationID = strOrEmpty(obj["conversation_id"])
	}
	if p.ConversationID == "" {
		p.ConversationID = strOrEmpty(obj["conversationId"])
	}

	// messages 骨架：一次解出 role/content 原始字节，供轮级键/粘性兜底键/图片
	// 判定共用（三者在旧路径各解一遍 messages）。
	var skel struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &skel)

	// 轮级键：TurnKey 同口径（末条 user、无可签名内容即空、不往前找）。
	for i := len(skel.Messages) - 1; i >= 0; i-- {
		if skel.Messages[i].Role != "user" {
			continue
		}
		if sig := contentSignature(skel.Messages[i].Content); sig != "" {
			p.TurnKey = fmt.Sprintf("u%d:%s", i, sig)
		}
		break
	}

	// 粘性兜底键：StickyFallbackKey 同口径——user 维度标识在场恒空；
	// 首条 user 消息签名（纯文本与 contentText 一致 + 图片轮可签名），
	// 首条 user 无可签名内容不往后找。
	if !hasUserIDIn(obj) {
		for i := range skel.Messages {
			if skel.Messages[i].Role != "user" {
				continue
			}
			if text := strings.TrimSpace(contentSignature(skel.Messages[i].Content)); text != "" {
				sum := sha256.Sum256([]byte(text))
				p.StickyKey = "fb:" + hex.EncodeToString(sum[:16])
			}
			break
		}
	}
	if p.SessKey != "" {
		p.StickyKey = p.SessKey
	}

	// 图片形态：与 hasImagePart 同口径（messages[].content[] 的 type=="image_url"）。
	// hasImagePart 的 peek struct 要求所有 content 都是数组：任一消息 content 为
	// 字符串等类型会导致**整体** unmarshal 失败 → 恒 false——即便图片出现在更早
	// 的消息里。等价复刻：一遍扫描同时记录「全部 content 解得出 parts 数组」与
	// 「扫到图」，仅当全部解出才采用扫描结果；不能扫到图就提前返回，否则
	// 「图片在前、字符串 content 在后」的混合 body 会误判 true（M3 等价性）。
	sawImage, allParsed := false, true
	for _, m := range skel.Messages {
		var parts []struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(m.Content, &parts); err != nil {
			allParsed = false
			break
		}
		for _, part := range parts {
			if part.Type == "image_url" {
				sawImage = true
				break
			}
		}
	}
	if allParsed {
		p.HasImage = sawImage
	}
	return p
}

// hasUserIDIn 已解析顶层 map 的 user 维度判定（hasUserID 的 map 形态复刻，
// 口径一致：metadata.user_id 或顶层 user_id 存在且为非空字符串）。
func hasUserIDIn(obj map[string]any) bool {
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if strOrEmpty(meta["user_id"]) != "" {
			return true
		}
	}
	return strOrEmpty(obj["user_id"]) != ""
}
