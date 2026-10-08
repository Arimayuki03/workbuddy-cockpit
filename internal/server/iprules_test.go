// iprules_test.go 全局入站 IP 规则引擎单测：黑名单优先于白名单、白名单模式
// 默认拒绝、热更新原子性（并发 Evaluate 与 SetRules 无竞态、无中间态）、
// 拦截日志滚动上限。语义对齐参考实现 routers/security.py（先 deny 后 allow）。
package server

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// testRules 构造快照的便捷入口（测试直接拿 Rules.Evaluate 验证纯语义）。
func testRules(t *testing.T, spec ipRuleSpec) *Rules {
	t.Helper()
	r, errs := buildRules(spec)
	if len(errs) > 0 {
		t.Fatalf("test spec has invalid entries: %v", errs)
	}
	return r
}

// TestRulesBlacklistOverridesWhitelist 黑名单优先于白名单：同一 IP 同时命中
// 黑白名单时必须拒绝（security.py 先判 deny 的语义——白名单是「准入门槛」，
// 黑名单是「一票否决」，否决权高于准入）。
func TestRulesBlacklistOverridesWhitelist(t *testing.T) {
	r := testRules(t, ipRuleSpec{
		Blacklist:     []string{"10.0.0.5", "192.168.0.0/16"},
		Whitelist:     []string{"10.0.0.0/8"},
		WhitelistMode: true,
	})
	if allowed, reason := r.Evaluate("10.0.0.5"); allowed || reason != ReasonIPBlocked {
		t.Fatalf("blacklisted IP inside whitelist must be blocked, got allowed=%v reason=%q", allowed, reason)
	}
	if allowed, reason := r.Evaluate("192.168.1.1"); allowed || reason != ReasonIPBlocked {
		t.Fatalf("blacklisted CIDR hit must be blocked, got allowed=%v reason=%q", allowed, reason)
	}
	if allowed, _ := r.Evaluate("10.0.0.6"); !allowed {
		t.Fatal("whitelisted non-blacklisted IP must pass")
	}
	if allowed, reason := r.Evaluate("8.8.8.8"); allowed || reason != ReasonIPNotWhitelisted {
		t.Fatalf("whitelist mode rejects unknown IP, got allowed=%v reason=%q", allowed, reason)
	}
}

// TestRulesWhitelistModeDefaultDeny 白名单模式默认拒绝 + 空白名单防自我锁死。
func TestRulesWhitelistModeDefaultDeny(t *testing.T) {
	// 非空白名单：模式生效，不在名单 = 拒绝。
	r := testRules(t, ipRuleSpec{Whitelist: []string{"10.0.0.0/8"}, WhitelistMode: true})
	if allowed, reason := r.Evaluate("10.1.2.3"); !allowed {
		t.Fatalf("in-whitelist must pass: reason=%q", reason)
	}
	if allowed, reason := r.Evaluate("203.0.113.9"); allowed || reason != ReasonIPNotWhitelisted {
		t.Fatalf("out-of-whitelist must be rejected with %s, got allowed=%v reason=%q", ReasonIPNotWhitelisted, allowed, reason)
	}
	// 空白名单 + 模式开：自动失效（默认放行，防「配了空名单把自己锁死在门外」）。
	rEmpty := testRules(t, ipRuleSpec{WhitelistMode: true})
	if allowed, _ := rEmpty.Evaluate("203.0.113.9"); !allowed {
		t.Fatal("empty whitelist must disable whitelist mode (anti-lockout)")
	}
	// 模式关：黑名单仍生效（黑名单模式与白名单模式相互独立）。
	rBl := testRules(t, ipRuleSpec{Blacklist: []string{"203.0.113.9"}, WhitelistMode: false})
	if allowed, reason := rBl.Evaluate("203.0.113.9"); allowed || reason != ReasonIPBlocked {
		t.Fatalf("blacklist works regardless of mode, got allowed=%v reason=%q", allowed, reason)
	}
	if allowed, _ := rBl.Evaluate("8.8.8.8"); !allowed {
		t.Fatal("blacklist mode defaults to allow")
	}
}

// TestRulesEmptyAndIPv6 无规则全放行 + IPv6 条目。
func TestRulesEmptyAndIPv6(t *testing.T) {
	r := testRules(t, ipRuleSpec{})
	if allowed, reason := r.Evaluate("1.2.3.4"); !allowed || reason != "" {
		t.Fatalf("no rules must allow, got allowed=%v reason=%q", allowed, reason)
	}
	r6 := testRules(t, ipRuleSpec{Blacklist: []string{"2001:db8::/32"}, Whitelist: []string{"::1"}})
	if allowed, _ := r6.Evaluate("2001:db8::1"); allowed {
		t.Fatal("ipv6 blacklist hit must be blocked")
	}
	if allowed, _ := r6.Evaluate("::1"); !allowed {
		t.Fatal("ipv6 whitelist must pass")
	}
	// v4-mapped 命中 v4 规则（EvaluateAddr 路径的 Unmap 归一）。
	rM, errs := buildRules(ipRuleSpec{Blacklist: []string{"10.0.0.0/8"}})
	if len(errs) > 0 {
		t.Fatalf("build: %v", errs)
	}
	if allowed, _ := rM.EvaluateAddr(netip.MustParseAddr("::ffff:10.1.2.3")); allowed {
		t.Fatal("v4-mapped addr must hit v4 blacklist after Unmap")
	}
}

// TestIPBlockRulesSetRulesAtomic 整体拒绝语义：任一条目非法 → 全部不生效
// （不半套用，防「想加一条白名单结果拼错把黑名单清空」的中间态事故）。
func TestIPBlockRulesSetRulesAtomic(t *testing.T) {
	b, errs := NewIPBlockRules("", ipRuleSpec{Blacklist: []string{"10.0.0.0/8"}})
	if len(errs) > 0 {
		t.Fatalf("init: %v", errs)
	}
	if allowed, _ := b.Evaluate("10.0.0.1"); allowed {
		t.Fatal("initial blacklist must be active")
	}
	// 非法更新被整体拒绝：旧规则原样保留。拒绝的 spec 里带 WhitelistMode=true——
	// 若半套用（只解析出部分条目就生效），8.8.8.8 会被白名单模式拦下；整体拒绝
	// 语义下旧规则（无白名单模式）继续放行它，这是新旧快照的判别性探针。
	if errList, err := b.SetRules(ipRuleSpec{Blacklist: []string{"bad-entry"}, Whitelist: []string{"1.2.3.4"}, WhitelistMode: true}); err == nil || len(errList) == 0 {
		t.Fatalf("invalid update must be rejected as a whole, errs=%v err=%v", errList, err)
	}
	if allowed, _ := b.Evaluate("10.0.0.1"); allowed {
		t.Fatal("old rules must survive rejected update")
	}
	if allowed, _ := b.Evaluate("8.8.8.8"); !allowed {
		t.Fatal("rejected spec's whitelist mode must NOT take effect (atomicity)")
	}
	// 合法更新生效。
	if _, err := b.SetRules(ipRuleSpec{Blacklist: []string{" 10.0.0.0/8 "}, Whitelist: []string{" 1.2.3.4"}, WhitelistMode: true}); err != nil {
		t.Fatalf("valid update: %v", err)
	}
	if allowed, _ := b.Evaluate("1.2.3.4"); !allowed {
		t.Fatal("updated whitelist must allow")
	}
	if allowed, _ := b.Evaluate("10.0.0.1"); allowed {
		t.Fatal("updated blacklist must block (whitespace-tolerant input)")
	}
}

// TestIPBlockRulesConcurrentEvalSet 热更新原子性（对齐 wafip_conc_test.go 的
// 并发压测风格）：并发 Evaluate 与 SetRules 交错，不变量——每个请求看到的规则
// 要么是旧快照要么是新快照，绝无中间态（半黑半白）；且无数据竞争（-race 把关）。
func TestIPBlockRulesConcurrentEvalSet(t *testing.T) {
	b, errs := NewIPBlockRules("", ipRuleSpec{Blacklist: []string{"10.0.0.0/8"}})
	if len(errs) > 0 {
		t.Fatalf("init: %v", errs)
	}
	var wg sync.WaitGroup
	// 评估者：10 goroutine 各打 1.2.3.4（恒放行）与 10.0.0.1（恒拦截）各 200 次。
	// 两个快照版本下这两个判定都成立：旧版黑名单 10/8 拦 10.0.0.1；新版黑名单
	// 带空白条目归一后同样拦。若出现中间态（黑名单被清空又重建），10.0.0.1 会
	// 漏放行 → 测试失败。
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				if allowed, _ := b.Evaluate("10.0.0.1"); allowed {
					t.Error("10.0.0.1 must be blocked under every consistent snapshot")
					return
				}
				if allowed, _ := b.Evaluate("1.2.3.4"); !allowed {
					t.Error("1.2.3.4 must be allowed under every consistent snapshot")
					return
				}
			}
		}()
	}
	// 更新者：并发换规则（合法快照），原子指针替换下评估者永远看不到中间态。
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 50; n++ {
				if _, err := b.SetRules(ipRuleSpec{Blacklist: []string{" 10.0.0.0/8 "}, WhitelistMode: n%2 == 0}); err != nil {
					t.Errorf("SetRules: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestIPBlockRulesPersistRoundTrip 落盘回读：SetRules 落盘 → 新实例从盘恢复
// （重启持久语义）；带空白的条目落盘前归一。
func TestIPBlockRulesPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "iprules.json")
	b, errs := NewIPBlockRules(fp, ipRuleSpec{Blacklist: []string{"10.0.0.0/8"}, Whitelist: []string{" 1.2.3.4 "}, WhitelistMode: true})
	if len(errs) > 0 {
		t.Fatalf("init: %v", errs)
	}
	if _, err := b.SetRules(ipRuleSpec{Blacklist: []string{" 172.16.0.0/12 ", "192.168.1.100"}, Whitelist: []string{"8.8.8.8"}, WhitelistMode: true}); err != nil {
		t.Fatalf("SetRules: %v", err)
	}
	// 落盘文件是干净 JSON（tmp 已 rename 走）。
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read persist file: %v", err)
	}
	if strings.Contains(string(raw), " 172.16.0.0/12 ") || strings.Contains(string(raw), "\" 1.2.3.4 \"") {
		t.Errorf("persisted entries must be trimmed: %s", raw)
	}
	// 新实例从盘恢复（启动 spec 被落盘值覆盖）。
	b2, errs := NewIPBlockRules(fp, ipRuleSpec{Blacklist: []string{"1.1.1.1"}})
	if len(errs) > 0 {
		t.Fatalf("reinit: %v", errs)
	}
	if allowed, reason := b2.Evaluate("192.168.1.100"); allowed || reason != ReasonIPBlocked {
		t.Fatalf("restored blacklist must block, got allowed=%v reason=%q", allowed, reason)
	}
	if allowed, _ := b2.Evaluate("8.8.8.8"); !allowed {
		t.Fatal("restored whitelist must allow")
	}
	if allowed, reason := b2.Evaluate("1.1.1.1"); allowed || reason != ReasonIPNotWhitelisted {
		t.Fatalf("disk rules must override startup seed spec, got allowed=%v reason=%q", allowed, reason)
	}
	view := b2.RulesView()
	if got := fmt.Sprint(view["ip_blacklist"]); !strings.Contains(got, "172.16.0.0/12") || !strings.Contains(got, "192.168.1.100") {
		t.Errorf("RulesView blacklist=%s want restored entries", got)
	}
}

// TestIPBlockRulesBlockLogRolling 拦截日志滚动上限：只记拦截、超容量 FIFO
// 淘汰最旧、满环 wrap 后仍保持时间序。
func TestIPBlockRulesBlockLogRolling(t *testing.T) {
	b, errs := NewIPBlockRules("", ipRuleSpec{Blacklist: []string{"10.0.0.0/8"}})
	if len(errs) > 0 {
		t.Fatalf("init: %v", errs)
	}
	// 引擎从不自动记日志（放行/拦截都只是 Evaluate 返回值），接线方仅在
	// allowed=false 分支调 NoteBlocked——先各评估一次，日志应为空。
	b.Evaluate("8.8.8.8")  // 放行：调用方不应记录
	b.Evaluate("10.0.0.1") // 拦截但尚未调用 NoteBlocked
	if logs := b.BlockedLogs(); len(logs) != 0 {
		t.Fatalf("Evaluate must never auto-log, got %+v", logs)
	}
	if allowed, reason := b.Evaluate("10.0.0.1"); allowed || reason != ReasonIPBlocked {
		t.Fatalf("blocked eval: allowed=%v reason=%q", allowed, reason)
	}
	b.NoteBlocked("10.0.0.1", "/v1/chat/completions", ReasonIPBlocked)
	if logs := b.BlockedLogs(); len(logs) != 1 || logs[0].IP != "10.0.0.1" || logs[0].Reason != ReasonIPBlocked {
		t.Fatalf("logs=%+v want exactly 1 blocked entry", logs)
	}
	// 滚动上限：写满默认容量后再写，总量恒 ≤ cap，最旧被淘汰（时间序头部递增）。
	// n=0..561 共 562 次写入，IP=10.0.(n>>8).(n&0xff)：最旧 50 条被淘汰。
	for n := 0; n < defaultBlockLogCap+50; n++ {
		b.NoteBlocked(fmt.Sprintf("10.0.%d.%d", n>>8, n&0xff), "/v1/models", ReasonIPBlocked)
	}
	logs := b.BlockedLogs()
	if len(logs) > defaultBlockLogCap {
		t.Fatalf("logs=%d must be capped at %d", len(logs), defaultBlockLogCap)
	}
	if len(logs) != defaultBlockLogCap {
		t.Fatalf("full ring must report exactly cap entries, got %d", len(logs))
	}
	// 最旧 50 条已被淘汰：第一条应是第 50 次写入（下标 50）。
	if got := logs[0].IP; got != "10.0.0.50" {
		t.Fatalf("oldest surviving entry=%q want 10.0.0.50 (FIFO eviction)", got)
	}
	if got := logs[len(logs)-1].IP; got != "10.0.2.49" { // 最后一次写入 n=561 → 10.0.2.49
		t.Fatalf("newest entry=%q want 10.0.2.49", got)
	}
	// 时间序：逐条 TS 单调不减（wrap 拼接后仍有序）。
	for i := 1; i < len(logs); i++ {
		if logs[i].TS.Before(logs[i-1].TS) {
			t.Fatalf("logs not in time order at %d: %v before %v", i, logs[i].TS, logs[i-1].TS)
		}
	}
}

// TestIPBlockRulesNilSafety 零值/nil 防御：nil 引擎恒放行，NoteBlocked 不 panic。
func TestIPBlockRulesNilSafety(t *testing.T) {
	var b *IPBlockRules
	if allowed, _ := b.Evaluate("1.2.3.4"); !allowed {
		t.Fatal("nil engine must allow")
	}
	b.NoteBlocked("1.2.3.4", "/", ReasonIPBlocked) // 不 panic
	if logs := b.BlockedLogs(); logs != nil {
		t.Fatalf("nil engine logs=%v want nil", logs)
	}
}
