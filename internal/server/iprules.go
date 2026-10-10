// iprules.go 全局入站 IP 白/黑名单规则引擎（多密钥分发的前置件）。
//
// 与 wafip.go 的关系（两个都要保留，层次不同）：
//   - wafip 是**上游 WAF** 的 IP 级 fail-fast（出口维度：网关自己的出口 IP 被
//     上游防火墙拦，影响的是轮转决策）；本文件是**入站访问控制**（入口维度：
//     哪些来源 IP 可以访问本网关）。
//   - 执行顺序：入站 IP 规则（本文件，鉴权链最前）→ 鉴权（withAuth 密钥/会话）
//     → 上游出站 → WAF IP fail-fast（wafip，出站遇 WAF 403 时的轮转止损）。
//     入站被本规则拒绝的请求不出站、不打上游，WAF 状态机感知不到它们。
//
// 语义（对齐参考实现 routers/security.py）：
//   - 黑名单优先于白名单：先判黑（命中即拒），再判白；
//   - 白名单模式（whitelist_mode=true 且白名单非空）：不在白名单 = 拒绝
//     （默认拒绝）；白名单为空时白名单模式自动失效（等于全放行，防自我锁死）；
//   - 黑名单模式（黑名单命中即拒）与白名单模式可同时存在，黑名单先判。
//
// 并发模型（对齐 livecfg 原子快照模式）：Rules 不可变快照，atomic.Pointer
// 整体替换；读方 Evaluate 无锁（热路径零争用），写方 SetRules 构建新快照后
// 一次性换指针——并发 Evaluate 与 SetRules 无数据竞争，读到的要么是旧快照
// 要么是新快照，不存在中间态。
//
// 拦截访问日志：**只记拦截不记放行**（正常流量淹没安全信号就没有观测价值），
// 固定容量环形缓冲（对齐 panel/ring.go 的 FIFO 淘汰风格），超限淘汰最旧。
package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/iputil"
)

// 拦截原因稳定短码（后续 i18n 用；契约：只增不改，面板/客户端按码匹配）。
const (
	ReasonMissingKey       = "missing_key"        // 无鉴权凭据
	ReasonInvalidKey       = "invalid_key"        // 凭据错误
	ReasonIPBlocked        = "ip_blocked"         // 命中黑名单
	ReasonIPNotWhitelisted = "ip_not_whitelisted" // 白名单模式且不在白名单
)

// Rules 一份不可变的 IP 规则快照（Evaluate 热路径只读，整体替换换新）。
type Rules struct {
	// Blacklist / Whitelist 归一化后的 CIDR 前缀（ParseCIDRs 产物：去空白、
	// 单 IP 补 /32 或 /128）。
	Blacklist []netip.Prefix `json:"ip_blacklist"`
	Whitelist []netip.Prefix `json:"ip_whitelist"`
	// WhitelistMode 白名单模式开关：true 且白名单非空时，不在白名单的来源
	// 一律拒绝（默认拒绝）。黑名单模式不受本开关影响（有黑名单就判）。
	WhitelistMode bool `json:"ip_whitelist_mode"`
	// blacklist 保留原始字符串（与 Blacklist 前缀一一对应），供视图/落盘回读
	// 时展示用户输入形态（归一化会抹掉 "1.2.3.4" 与 "1.2.3.4/32" 的书写差异）。
	BlacklistRaw []string `json:"-"`
	WhitelistRaw []string `json:"-"`
}

// ipRuleSpec 面板/配置侧的规则输入形态（原始字符串列表 + 模式开关）。
type ipRuleSpec struct {
	Blacklist     []string `json:"ip_blacklist"`
	Whitelist     []string `json:"ip_whitelist"`
	WhitelistMode bool     `json:"ip_whitelist_mode"`
}

// buildRules 把字符串规则归一化为不可变快照。返回错误清单（逐条 "条目: 原因"），
// 调用方决定 fail-fast（启动期）还是拒绝保存（面板热改期）。
func buildRules(spec ipRuleSpec) (*Rules, []string) {
	blacklist, blErrs := iputil.ParseCIDRs(spec.Blacklist)
	whitelist, wlErrs := iputil.ParseCIDRs(spec.Whitelist)
	r := &Rules{
		Blacklist:     blacklist,
		Whitelist:     whitelist,
		WhitelistMode: spec.WhitelistMode,
		BlacklistRaw:  append([]string(nil), spec.Blacklist...),
		WhitelistRaw:  append([]string(nil), spec.Whitelist...),
	}
	return r, append(blErrs, wlErrs...)
}

// NewIPRuleSpec 构造规则输入形态（ipRuleSpec 的导出构造器）：panel 面板热改等
// 包外调用方无法字面量构造未导出类型，经本入口组装后传入 SetRules（返回值按
// 不透明值透传即可，调用方无需感知类型定义）。
func NewIPRuleSpec(blacklist, whitelist []string, whitelistMode bool) ipRuleSpec {
	return ipRuleSpec{
		Blacklist:     blacklist,
		Whitelist:     whitelist,
		WhitelistMode: whitelistMode,
	}
}

// Evaluate 判定来源 IP 是否放行（无锁读快照，热路径安全）。
// 返回 (allowed, reason)；allowed=false 时 reason 是稳定短码
// （ip_blocked / ip_not_whitelisted），供拦截日志与后续 i18n 使用。
// 判定顺序：黑名单 → 白名单（黑名单优先级更高，参考 security.py 先 deny 后 allow）。
// ip 为空（对端缺失/非法解析）：白名单模式下按默认拒绝处理，否则放行——
// 规则面向已知来源，解析不出来源不算命中黑名单。
func (r *Rules) Evaluate(ip string) (bool, string) {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		addr = netip.Addr{}
	}
	return r.EvaluateAddr(addr)
}

// EvaluateAddr 同 Evaluate，接受已解析地址（调用方已持有 netip.Addr 时免二次
// 解析）。零值/非法地址（IsValid=false）按「解析不出来源」处理：白名单模式下
// 默认拒绝，否则放行。
func (r *Rules) EvaluateAddr(addr netip.Addr) (bool, string) {
	if !addr.IsValid() {
		if r.WhitelistMode && len(r.Whitelist) > 0 {
			return false, ReasonIPNotWhitelisted
		}
		return true, ""
	}
	addr = addr.Unmap()
	// 先判黑：命中即拒，优先级高于白名单。
	for _, p := range r.Blacklist {
		if p.Contains(addr) {
			return false, ReasonIPBlocked
		}
	}
	// 再判白：白名单模式仅在白名单非空时生效（空白名单 = 默认放行防自我锁死）。
	if r.WhitelistMode && len(r.Whitelist) > 0 {
		for _, p := range r.Whitelist {
			if p.Contains(addr) {
				return true, ""
			}
		}
		return false, ReasonIPNotWhitelisted
	}
	return true, ""
}

// IPBlockRules 全局入站 IP 规则引擎：持有当前快照 + 拦截日志环。
// 零值不可用（需 NewIPBlockRules 构建；规则为空时 Evaluate 恒放行）。
type IPBlockRules struct {
	rules atomic.Pointer[Rules]
	// mu 保护落盘路径（写盘串行化）与日志环（见 blockLog）。
	mu       sync.Mutex
	filePath string // 规则 JSON 落盘路径；空 = 不落盘（纯内存形态，测试用）
	// blocked 环形缓冲：只记拦截不记放行。超 capacity 淘汰最旧（FIFO）。
	blocked   []BlockLogEntry
	blockCap  int
	blockHead int // 环写指针（buf 未满时等于 len）
}

// defaultBlockLogCap 拦截日志容量：安全信号流量天然小（拦截是异常事件），
// 512 条足够回溯；对齐 panel/ring 的「固定容量 + FIFO 淘汰」风格。
const defaultBlockLogCap = 512

// NewIPBlockRules 构建引擎。spec 为初始规则（非法条目进错误清单但不阻塞构建
// ——非法条目跳过、合法条目生效，与参考实现「存量库带空条目也能兜住」同一
// 容忍哲学）；filePath 非空时先尝试读盘恢复，再以 spec 兜底。
func NewIPBlockRules(filePath string, spec ipRuleSpec) (*IPBlockRules, []string) {
	b := &IPBlockRules{filePath: filePath, blockCap: defaultBlockLogCap}
	// diskUnreadable：读到了文件但解析失败——损坏文件保留不覆盖（见下）。
	diskUnreadable := false
	// 读盘优先：上次运行的规则优先于本次启动 spec（落盘 = 面板热改的持久层，
	// 启动 spec 只是首次运行的种子）。读盘/解析失败回落 spec，不阻塞启动，
	// 但要留信号（否则上一轮保存的规则丢了无人知晓）。
	if filePath != "" {
		raw, err := os.ReadFile(filePath)
		switch {
		case err == nil:
			var disk ipRuleSpec
			if uErr := json.Unmarshal(raw, &disk); uErr != nil {
				// 解析失败：回落种子，但**不把种子落盘覆盖**损坏文件——保留
				// 原文件供人工排查（覆盖即销毁证据，且用户可能只是想修个笔误）。
				diskUnreadable = true
				log.Printf("WARN: [server] ip rules file %s 解析失败（%v），回落启动种子，损坏文件保留不覆盖", filePath, uErr)
			} else {
				spec = disk
			}
		case os.IsNotExist(err):
			// 首次运行：正常路径，无信号。
		default:
			log.Printf("WARN: [server] ip rules file %s 读取失败（%v），回落启动种子", filePath, err)
		}
	}
	rules, errs := buildRules(spec)
	b.rules.Store(rules)
	// diskUnreadable 时跳过种子落盘：写盘会用种子 spec 覆盖损坏文件，上一轮
	// 保存的规则就无信号丢失了（内存已回落种子运行，等用户人工修复或面板
	// 下次 SetRules 再正常落盘）。
	if filePath != "" && len(errs) == 0 && !diskUnreadable {
		// 初始规则合法时照常落盘（幂等：内容相同写一次无害，且让「落盘文件
		// 存在」成为规则已持久化的单一事实来源）。
		if err := b.writeLocked(rules); err != nil {
			log.Printf("WARN: [server] ip rules persist: %v (in-memory rules stay effective)", err)
		}
	} else if len(errs) > 0 {
		log.Printf("WARN: [server] ip rules: %d invalid entries skipped: %v", len(errs), errs)
	}
	// M1 收窄口径：白名单模式开启但有效白名单为空（条目全非法被清空，或本就
	// 未配置）时，Evaluate 的「空白名单自动失效防自我锁死」语义会让该组合
	// 静默全放行——这是防锁死的有意设计不能改，但必须打 ERROR 级响亮告警，
	// 否则管理员以为开了白名单实际门户大开。
	if rules.WhitelistMode && len(rules.Whitelist) == 0 {
		log.Printf("ERROR: [server] 白名单模式开启但有效白名单为空，已回落全放行，请检查 ip_whitelist 条目（空白名单自动失效是防自我锁死设计，但该组合通常意味着条目全部非法）")
	}
	return b, errs
}

// SetRules 热更新规则（面板/主代理接线用）：归一化 → 原子换快照 → 落盘。
// 任一条目非法时**整体拒绝**（不半套用），返回错误清单供调用方回显；
// 落盘失败不影响内存生效（返回值 err 提示，规则已在内存生效，重启后回落旧值）。
func (b *IPBlockRules) SetRules(spec ipRuleSpec) ([]string, error) {
	rules, errs := buildRules(spec)
	if len(errs) > 0 {
		return errs, fmt.Errorf("%d 条规则非法，未生效", len(errs))
	}
	b.rules.Store(rules)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.filePath == "" {
		return nil, nil
	}
	if err := b.writeLocked(rules); err != nil {
		return nil, err
	}
	return nil, nil
}

// writeLocked 原子落盘：tmp + rename（对齐 pool/writeStateFileSync 风格，
// 防掉电产生半截 JSON）。调用方必须已持 b.mu。
func (b *IPBlockRules) writeLocked(rules *Rules) error {
	spec := ipRuleSpec{
		Blacklist:     normalizeRawList(rules.BlacklistRaw),
		Whitelist:     normalizeRawList(rules.WhitelistRaw),
		WhitelistMode: rules.WhitelistMode,
	}
	raw, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(b.filePath); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := b.filePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, b.filePath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// normalizeRawList 去空白 + 丢空串（落盘形态干净，回读时 ParseCIDRs 少干活）。
func normalizeRawList(items []string) []string {
	out := make([]string, 0, len(items))
	for _, s := range items {
		if t := trimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// trimSpace 薄封装（strings.TrimSpace；独立出来仅为落盘调用点可读性）。
func trimSpace(s string) string {
	return strings.TrimSpace(s)
}

// RulesView 返回当前规则的对外视图（副本，面板展示用；原始书写形态保留）。
func (b *IPBlockRules) RulesView() map[string]any {
	r := b.rules.Load()
	if r == nil {
		return map[string]any{"ip_blacklist": []string{}, "ip_whitelist": []string{}, "ip_whitelist_mode": false}
	}
	return map[string]any{
		"ip_blacklist":      append([]string(nil), r.BlacklistRaw...),
		"ip_whitelist":      append([]string(nil), r.WhitelistRaw...),
		"ip_whitelist_mode": r.WhitelistMode,
	}
}

// Evaluate 用当前快照判定来源 IP（无锁）。allowed=false 时调用方应把短码
// reason 与请求上下文交给 NoteBlocked 记拦截日志，再以 403 回客户端。
func (b *IPBlockRules) Evaluate(ip string) (bool, string) {
	if b == nil {
		return true, ""
	}
	if r := b.rules.Load(); r != nil {
		return r.Evaluate(ip)
	}
	return true, ""
}

// BlockLogEntry 一条拦截访问日志（入站 IP 规则拦截；不含放行流量）。
type BlockLogEntry struct {
	TS     time.Time `json:"ts"`     // 拦截时刻
	IP     string    `json:"ip"`     // 来源 IP（ParseClientIP 解析结果）
	Path   string    `json:"path"`   // 请求路径（如 /v1/chat/completions）
	Reason string    `json:"reason"` // 拦截原因短码（ip_blocked / ip_not_whitelisted）
}

// NoteBlocked 记一条拦截日志（环形缓冲，超容量淘汰最旧；只记拦截——放行
// 不入环，防正常流量淹没安全信号）。path/ua 由调用方从请求提取。
func (b *IPBlockRules) NoteBlocked(ip, path, reason string) {
	if b == nil {
		return
	}
	entry := BlockLogEntry{TS: time.Now(), IP: ip, Path: path, Reason: reason}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.blockCap <= 0 {
		b.blockCap = defaultBlockLogCap
	}
	if len(b.blocked) < b.blockCap {
		b.blocked = append(b.blocked, entry)
		return
	}
	// 已满：环写指针覆盖最旧。
	b.blocked[b.blockHead] = entry
	b.blockHead = (b.blockHead + 1) % b.blockCap
}

// BlockedLogs 按写入时间序返回拦截日志快照（副本，调用方可安全持有）。
// 满环时先返回 wrap 后的旧段再返回新段（时间序由环结构保证）。
// nil 引擎返回 nil（与 Evaluate 的 nil 防御同口径）。
func (b *IPBlockRules) BlockedLogs() []BlockLogEntry {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]BlockLogEntry, len(b.blocked))
	if b.blockHead < len(b.blocked) {
		// 满环：从写指针起的旧段 + 开头的新段拼回时间序。
		n := copy(out, b.blocked[b.blockHead:])
		copy(out[n:], b.blocked[:b.blockHead])
	} else {
		copy(out, b.blocked)
	}
	return out
}
