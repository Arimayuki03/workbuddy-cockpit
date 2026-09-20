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
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/upstream"
)

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
// 满足 scriptRunner 接口（exec.Cmd 本身只有字段没有方法）。
type scriptCmd struct{ cmd *exec.Cmd }

func (c *scriptCmd) SetDir(dir string) { c.cmd.Dir = dir }
func (c *scriptCmd) Run() error        { return c.cmd.Run() }

// newScriptCmd 构建脚本子进程。包级变量便于测试注入 fake（installFakeExec 覆盖）。
// 工作目录由调用方 SetDir 显式设置仓库根。
var newScriptCmd = func(program string, args ...string) scriptRunner {
	return &scriptCmd{cmd: exec.Command(program, args...)}
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
func runScript(name, root string, commands [][]string) {
	for _, cmdArgs := range commands {
		c := newScriptCmd(cmdArgs[0], cmdArgs[1:]...)
		c.SetDir(root)
		if err := c.Run(); err != nil {
			log.Printf("WARN: %s (%s): %v", name, cmdArgs[1], err)
			continue
		}
		log.Printf("%s: ok (%s)", name, cmdArgs[1])
	}
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
		time.Sleep(activityAccountDelay)
	}
}
