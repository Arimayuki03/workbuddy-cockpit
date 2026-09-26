package pool

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// syncBuffer 并发安全的日志缓冲：watch goroutine（log.Printf）与测试 goroutine
// （读取断言）并发访问，bytes.Buffer 非线程安全（-race 下必报 DATA RACE）。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ---------------------------------------------------------------------------
// R3 修复回归：等权重洗牌生效（不被比较器 uid 决胜抵消）
// ---------------------------------------------------------------------------

// TestShufflePersistsInComparator 修复回归：等权重洗牌后，排序比较器的决胜分支
// 必须按洗牌序号（ws.shuf）而不是 uid——旧实现在洗牌之后按 `uid <` 排序，等权重
// 时完全洗回字典序，Fisher-Yates 沦为死代码（R3 审查 pick.go:225-239 实锤）。
//
// 断言方式（行为级，白盒构造避免依赖 time-seeded 源的随机分布）：
// 直接构造 8 个等权重候选，把洗牌逻辑替换为"受控置换"不可行（逻辑内联），
// 因此用固定序断言洗牌后的比较结果与 uid 序解耦：多轮 pick 中，若比较器仍按
// uid 决胜，ws 排序结果恒为字典序；修复后应跟随洗牌序。用统计断言：只看
// minPickGap=0 + SetRandomSource(0) 时 r=0 恒选 eligible 首位（ws 排序后的
// 第一个等权候选）——旧实现下该位恒为字典序最小者；修复后多轮应命中不同 uid。
//
// 与 TestShuffleEpsilonEqualWeights 的区别：那个测"洗牌触发后不饿死"（覆盖面），
// 本测锚定洗牌在**排序阶段不被抵消**（机制本身），洗牌被抵消时它必失败。
func TestShufflePersistsInComparator(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	// 8 个完全同构候选（等权重、len>5 触发洗牌路径）。
	for i := 0; i < 8; i++ {
		uid := fmt.Sprintf("c%02d", i)
		p.Add(&auth.Auth{UID: uid})
		p.SetCredits(uid, 100)
	}
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		a := p.Pick("")
		if a == nil {
			t.Fatal("pick returned nil")
		}
		seen[a.UID] = true
	}
	// 旧实现（洗牌被 uid 决胜抵消）下，eligible 首位恒为 c00：r=0 恒选 c00，
	// lastUsed 每轮被刷新但 idle 满分使权重恒等 → seen 恒 {c00}。
	// 修复后洗牌序保留到排序结果，r=0 选中的是随机置换的首位 → 多轮覆盖多个。
	if len(seen) < 2 {
		t.Errorf("等权重洗牌未生效（r=0 恒选同一账号 %v）: seen=%v", seen, seen)
	}
}

// ---------------------------------------------------------------------------
// R3 修复回归：模型级冷却 6004/11102 同槽互覆盖的对账日志
// ---------------------------------------------------------------------------

// TestModelCooldownOverwriteCrossKindLog 同一 (账号, 模型) 槽被另一类条目（6004 vs
// 11102）覆盖时打对账日志；同型覆盖（6004→6004 / 11102→11102，TTL 正常刷新）不打。
// 本测试不 t.Parallel：log.SetOutput 是进程级全局，需串行。
func TestModelCooldownOverwriteCrossKindLog(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	// 首次写入（槽为空）：不打。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(30*time.Minute), "m", "6004 model rate limit")
	if strings.Contains(buf.String(), "model cooldown 覆盖") {
		t.Errorf("空槽首写不应打对账日志: %s", buf.String())
	}
	// 6004 → 11102：异型覆盖，打。
	p.BlockModelBackoff("u1", "m", "11102 model not available")
	if !strings.Contains(buf.String(), "model cooldown 覆盖") {
		t.Errorf("6004 条目被 11102 覆盖应打对账日志: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `old="6004 model rate limit"`) || !strings.Contains(buf.String(), `new="11102 model not available"`) {
		t.Errorf("对账日志应含新旧 reason: %s", buf.String())
	}
	// 11102 → 11102（重试退避刷新）：同型，不打。
	buf.Reset()
	p.BlockModelBackoff("u1", "m", "11102 model not available")
	if strings.Contains(buf.String(), "model cooldown 覆盖") {
		t.Errorf("同型覆盖（11102 退避刷新）不应打对账日志: %s", buf.String())
	}
	// 11102 → 6004：异型覆盖（反方向），打。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(30*time.Minute), "m", "6004 model rate limit")
	if !strings.Contains(buf.String(), "model cooldown 覆盖") {
		t.Errorf("11102 条目被 6004 覆盖应打对账日志: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `old="11102`) {
		t.Errorf("对账日志 old 应为 11102 reason: %s", buf.String())
	}
}

// ---------------------------------------------------------------------------
// R3 修复回归：reloadAuthDir 失败返回 error（watch 循环据此保留旧指纹重试）
// ---------------------------------------------------------------------------

// TestReloadAuthDirErrorReturn reloadAuthDir 失败（LoadDir 出错）必须返回非 nil
// error、成功返回 nil。watch 循环依赖该返回值决定是否推进目录指纹：失败时保留
// 旧指纹、下一轮 5s 重试——先推进后加载的旧实现会把该次变更永久丢失（R3 审查
// watch.go:62-68 实锤）。
//
// 失败注入：目录名含未闭合的 `[` → filepath.Glob 返回 ErrBadPattern（auth.LoadDir
// 唯一可稳定触发的错误路径，其余错误路径都被"跳过坏文件"吞掉）。
func TestReloadAuthDirErrorReturn(t *testing.T) {
	p := New("")
	defer p.Close()

	// 成功路径：nil。
	okDir := t.TempDir()
	writeAuthFile(t, okDir, "u1", "一号")
	if err := p.reloadAuthDir(okDir); err != nil {
		t.Fatalf("成功 reload 应返回 nil, got %v", err)
	}
	if n := len(p.AvailableUIDs()); n != 1 {
		t.Fatalf("成功 reload 后账号数=%d want 1", n)
	}

	// 失败路径：非 nil error，且池状态保持不变。
	badDir := filepath.Join(t.TempDir(), "bad[pattern")
	if err := p.reloadAuthDir(badDir); err == nil {
		t.Fatal("LoadDir 失败应返回非 nil error（watch 循环据此不推进指纹、下一轮重试）")
	}
	if n := len(p.AvailableUIDs()); n != 1 {
		t.Errorf("失败 reload 不得改动池状态: 账号数=%d want 1", n)
	}
}

// TestWatchRetriesAfterReloadFailure watch 循环行为级回归：指纹变化触发 reload 失败
// 后，旧指纹必须保留（修复后下一 tick 仍重试；修复前 last 被先推进，该次变更被
// 永久吞掉、日志只有 1 条失败 WARN）。
//
// 失败注入：真实目录名含未闭合 `[` → dirFingerprint 用 os.ReadDir（字面路径，不
// glob）正常返回 ok=true，而 auth.LoadDir 用 filepath.Glob 返回 ErrBadPattern——
// 两个阶段恰好分离，可稳定构造「指纹可读但加载必失败」。判定：数「重新加载失败」
// WARN 条数，≥3 说明在逐 tick 重试（旧实现恒为 1）。
func TestWatchRetriesAfterReloadFailure(t *testing.T) {
	oldInterval := watchInterval
	watchInterval = 20 * time.Millisecond
	t.Cleanup(func() { watchInterval = oldInterval })

	// 目录名含未闭合 [：ReadDir 成功、Glob 失败。
	dir := filepath.Join(t.TempDir(), "bad[pattern")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeAuthFile(t, dir, "u1", "一号")

	p := New("")
	defer p.Close()

	var buf syncBuffer // 并发安全：watch goroutine 写 / 测试 goroutine 读
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	stop := p.StartAuthDirWatch(dir)
	defer stop()

	// 首个 tick 后指纹已等于基线（StartAuthDirWatch 建基线不 reload），改一次
	// 指纹触发「变化 → reload 失败」。
	time.Sleep(60 * time.Millisecond) // ≥2 ticks，越过基线 tick
	writeAuthFile(t, dir, "u1", "一号改") // 同名覆盖 → mtime 变 → 指纹变化

	// 轮询等待 ≥3 次失败日志（不依赖固定 sleep 的 tick 计数，race/高负载下稳定）：
	// 旧实现 last 被先推进 → 失败日志恒 1 条，2s 内（100 ticks）永远到不了 3；
	// 修复后每 tick 重试一次，通常几十 ms 内即达。
	deadline := time.Now().Add(2 * time.Second)
	fails := 0
	for time.Now().Before(deadline) {
		fails = strings.Count(buf.String(), "重新加载")
		if fails >= 3 {
			return // 修复生效：失败后每 tick 重试
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("reload 失败后应每 tick 重试（2s 内 ≥3 次失败日志），实际 %d 次——旧指纹可能被提前推进，变更被永久吞掉", fails)
}
