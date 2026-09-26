// school.go 开学季任务（脚本类）与夜猫子任务（panel 纯 API 口径）的排程执行器。
//
// 背景：school（12:00）与 cat（01:00 夜猫窗口）原由系统 crontab 调
// scripts/school_open_day_cron.sh 执行——依赖外部系统 cron、容器重建可能丢失、
// 不在 config 里配置。迁入后成为第五、第六类任务，时点由 schedule.school_hours /
// schedule.cat_hours 配置，school_open_day_cron.sh 保留为手动触发入口。
//
// v1.2.0：cat 由 python 脚本（task_runner.py black_cat）改为 panel 移植的纯 API
// 实现（差额探测 + 真实 glm-5.2 对话 + 事件上报），夜猫窗口判定与补足次数对齐
// panel blackcat.go 口径（见 scheduler.RunCatNow）。
package scheduler

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/upstream"
)

// scriptTimeout 单个脚本命令的总时长上限。school_open_day_2026.py 全量闭环
// 实测分钟级，10 分钟已含数倍余量；挂死（上游不响应/解释器死锁）时到点强杀，
// 不再让 runMu[taskSchool] 永久持有、每小时排程堆积。
const scriptTimeout = 10 * time.Minute

// repoRoot 定位仓库根（容器内 /app、宿主 /root/workbuddy2api）。
// 策略：从当前工作目录逐级向上找 scripts/school_open_day_2026.py，
// 找不到回落 os.Getwd()（此时 Run 会因脚本缺失打 WARN，不 panic）。
// 注意：Go scheduler 在 cmd/server 内以工作目录启动（容器 WORKDIR /app），
// 若进程以别的工作目录拉起（如 systemd/裸 binary），上溯穷尽后仍以
// os.Getwd() 兜底，把缺失暴露成 WARN 而非静默。
func repoRoot() string {
	start, err := os.Getwd()
	if err != nil {
		return "."
	}
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, "scripts", "school_open_day_2026.py")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return start
		}
		dir = parent
	}
}

// scriptRunner 脚本子进程的最小执行面：可被测试替换，避免测试真正拉起 python3。
type scriptRunner interface {
	SetDir(string)
	Run() error
}

// scriptCmd exec.Cmd 适配器：把 exec.Cmd 的 Dir 字段包装成 SetDir 方法，
// 满足 scriptRunner 接口（exec.Cmd 本身只有字段没有方法）。ctx/cancel 是
// newScriptCmd 构建的超时上下文：Run 返回时先采样 ctx.Err() 再 cancel 释放计时器
// （cancel 之后 Err 恒非 nil，必须先采样；timedOut 供 runScript 的超时日志判定）。
type scriptCmd struct {
	cmd      *exec.Cmd
	ctx      context.Context
	cancel   context.CancelFunc
	timedOut bool
}

func (c *scriptCmd) SetDir(dir string) { c.cmd.Dir = dir }
func (c *scriptCmd) Run() error {
	err := c.cmd.Run()
	c.timedOut = c.ctx.Err() != nil // cancel 前采样：脚本运行期间 deadline 是否触发
	c.cancel()                      // 进程已结束（或启动失败）：释放 WithTimeout 计时器，幂等
	return err
}

// newScriptCmd 构建脚本子进程。包级变量便于测试注入 fake（installFakeExec 覆盖）。
// 工作目录由调用方 SetDir 显式设置仓库根。
// 超时治理：ctx 带 scriptTimeout（10 分钟）——脚本挂起时 CommandContext 的默认
// Cancel（os.Process.Kill）立即杀掉进程，WaitDelay 再兜底回收残留管道句柄；
// 否则 runMu[taskSchool] 永不释放、每小时排程持续堆积（旧缺陷：无 ctx 无超时的
// exec.Command().Run()）。不覆盖 cmd.Cancel：自定义 Cancel（只 cancel 不杀）会让
// 到期先等 WaitDelay 10s 才经兜底 Kill，超时治理被无故削弱；恢复默认立即杀语义。
// cancel 不能 defer 到本函数返回（进程尚未启动，ctx 就被取消会让 Start 直接报错），
// 由 scriptCmd.Run 收尾释放。
var newScriptCmd = func(program string, args ...string) scriptRunner {
	ctx, cancel := context.WithTimeout(context.Background(), scriptTimeout)
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.WaitDelay = 10 * time.Second
	return &scriptCmd{cmd: cmd, ctx: ctx, cancel: cancel}
}

// pythonCmd 返回执行 scripts/*.py 的解释器名。
//
// 默认 "python3"，与容器/Linux 现状完全一致，行为零变更；WB2A_PYTHON
// 显式指定时优先，供解释器不叫 python3 的环境使用（命名对齐仓库 Go 侧
// WB2A_* env 约定，如 WB2A_AUTH_DIR / WB2A_LISTEN）。
//
// 需要该开关的原因：Windows 官方安装器只提供 python.exe，且 PATH 上常存在
// Microsoft Store 的 python3.exe App Execution Alias 存根——exec.Command 能找到
// 它却无法真正执行，脚本类任务统一报 `exit status 9009`。
// 设 WB2A_PYTHON=python 即可绕过。
func pythonCmd() string {
	if v := strings.TrimSpace(os.Getenv("WB2A_PYTHON")); v != "" {
		return v
	}
	return "python3"
}

// runScript 依次执行若干脚本命令：任一命令失败只记一行 WARN，不向上抛、
// 不影响调度主循环继续跑下一个时点。单命令失败不中断后续命令。
// 超时（scriptTimeout 到期被 CommandContext 杀进程）也是失败的一种：记 WARN
// 指明已超时强杀，调用方按失败语义继续后续命令。
func runScript(name, root string, commands [][]string) {
	for _, cmdArgs := range commands {
		c := newScriptCmd(cmdArgs[0], cmdArgs[1:]...)
		c.SetDir(root)
		if err := c.Run(); err != nil {
			if scriptTimedOut(c) {
				log.Printf("WARN: %s (%s): 超过 %s 强杀（脚本挂起）", name, cmdArgs[1], scriptTimeout)
				continue
			}
			log.Printf("WARN: %s (%s): %v", name, cmdArgs[1], err)
			continue
		}
		log.Printf("%s: ok (%s)", name, cmdArgs[1])
	}
}

// scriptTimedOut 报告脚本命令是否因 scriptTimeout 到期被 CommandContext 杀掉。
// 判定用构建时的 ctx.Err()（Run 返回时已采样进 scriptCmd.timedOut，取消计时器前
// 置位）而非 errors.Is(err, context.DeadlineExceeded)（旧实现 isContextDeadline）：
// 超时路径上 Cmd.Wait 返回被杀进程的 *ExitError（"signal: killed"/Windows
// TerminateProcess），DeadlineExceeded 只存在于 Cmd.Wait 因 Process.Wait 的错误被
// 优先而丢弃的 watchCtx 分支，errors.Is 判定恒 false（Windows 实测）——超时被记成
// 普通失败，丢失"强杀"语义。fake runner（测试注入）非 *scriptCmd，恒 false，
// 走普通失败日志。
func scriptTimedOut(c scriptRunner) bool {
	sc, ok := c.(*scriptCmd)
	return ok && sc.timedOut
}

// RunSchoolNow 立即执行开学季任务：school_open_day_2026.py ALL --run --yes。
// 全量跑任务点亮 + 领奖 + 自动抽空抽奖余额。活动下线（in_period=false）时脚本
// 各段全量跳过、正常退出，不视为失败。失败只记 WARN。
func (s *Scheduler) RunSchoolNow() {
	root := repoRoot()
	runScript("school", root, [][]string{
		{pythonCmd(), "scripts/school_open_day_2026.py", "ALL", "--run", "--yes"},
	})
}

// RunCatNow 立即执行夜猫子任务（panel 纯 API 口径并入，不再走 python 脚本）：
// 23:00–08:00 窗口内对池内账号补足 glm-5.2 对话并上报事件链（BlackcatNeed 差额
// 探测 + RunNightChats 真实对话）。窗口外触发直接跳过（black_cat 窗口外不计分，
// 打 skip 正常态）。单号失败只该号 WARN；账号间限速 activityAccountDelay。
func (s *Scheduler) RunCatNow() {
	// panel 裸 goroutine 入口：任务体 panic 不应击穿整个网关进程（与 RunCheckinNow
	// 同理，runBatch/RunKindNow 的 recover 不覆盖本入口）。
	defer func() {
		if r := recover(); r != nil {
			log.Printf("scheduler: task cat panic: %v", r)
		}
	}()
	s.runCat(context.Background())
}

// runCat 夜猫子任务遍历，随 ctx 取消立即退出（无 ctx 的外部入口 RunCatNow 取
// 背景 ctx，语义与引入前 time.Sleep 版一致；账号间限速等待中取消不等睡满）。
func (s *Scheduler) runCat(ctx context.Context) {
	if !upstream.InNightWindow(time.Now()) {
		log.Printf("cat: 当前不在 23:00–08:00 计数窗口，跳过")
		return
	}
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		if a.IsGlobal() {
			continue // D4 门控：global 无 CN 任务体系，不发起任何上游调用
		}
		need, err := s.cfg.Upstream.BlackcatNeed(a)
		if err != nil {
			log.Printf("cat %s: %v", logfmt.Label(a.UID, a.Nickname), err)
			continue
		}
		if need <= 0 {
			continue
		}
		ok, err := s.cfg.Upstream.RunNightChats(a, int(need))
		if err != nil {
			log.Printf("cat %s: %d/%d 完成，中断: %v", logfmt.Label(a.UID, a.Nickname), ok, need, err)
		} else {
			log.Printf("cat %s: 完成 %d 次夜间对话", logfmt.Label(a.UID, a.Nickname), ok)
		}
		if !sleepCtx(ctx, activityAccountDelay) {
			return // 优雅停机：不等限速睡满，剩余账号下轮再补
		}
	}
}

// RunQueueNow 到点执行任务中心执行队列（panel 注入的 queueRunner 回调）：
// 扫描全账号待办 → 按账号分组排队执行（成长任务 + 开学季闭环），语义与面板
// 任务中心「启动执行队列」完全一致。panel 未装配（回调 nil）或队列已在跑
// （panel 侧运行中直接拒启）时跳过并说明，均不视为失败。
// 返回观测摘要（供 lastOut 展示）。
func (s *Scheduler) RunQueueNow() string {
	s.queueMu.RLock()
	run := s.queueRunner
	s.queueMu.RUnlock()
	if run == nil {
		log.Printf("queue: 面板队列执行体未注入（panel 未装配），跳过")
		return "skipped: no runner"
	}
	run()
	return "done"
}
