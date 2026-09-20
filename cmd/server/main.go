// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/livecfg"
	"workbuddy2api/internal/panel"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usage"
)

// modelJSONPath 由 state.json 路径推导 model.json 路径（同目录同名换缀）：
// 两者同为数据目录持久化物（Docker ./data volume），配套而非各自配置。
// state 路径为空（纯内存测试形态）→ 空 = 禁用 model.json 落盘（内存 + 种子仍可用）。
func modelJSONPath(stateFile string) string {
	if stateFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(stateFile), "model.json")
}

// usagePathFor 由 state.json 路径推导 usage.json 路径（panel 同款推导：与 state
// 同目录；state 为空的纯内存形态下落 data/usage.json 缺省位置）。
func usagePathFor(stateFile string) string {
	if stateFile == "" {
		return filepath.Join("data", "usage.json")
	}
	return filepath.Join(filepath.Dir(stateFile), "usage.json")
}

// stateSibling 返回与 state 文件同目录的指定文件名路径（相对路径场景回落当前目录）。
// output_probes.json（模型上限探测，panel 移植件）共用本规则。
func stateSibling(stateFile, name string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// 配置文件不存在时给一次机会用纯默认 + env
		if os.IsNotExist(err) {
			log.Printf("config %s not found, using defaults+env", *cfgPath)
			cfg, err = Load("")
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	// fail-fast：admin.enabled=true 时 api_key 必填。withAuth 对空 api_key 一律放行，
	// 开启 admin 而不配 key 等于把 /admin 管理端点裸奔在监听地址上（缺省 :7863 全网卡）。
	if cfg.Admin.Enabled && strings.TrimSpace(cfg.APIKey) == "" {
		log.Fatalf("admin.enabled=true 但 api_key 为空：请先在 config.json 配置 api_key 再开启 admin（否则管理端点无鉴权暴露）")
	}
	// fail-fast（v1.2.0 设计文档 §8）：panel.enabled=true（缺省）且 api_key 为空拒启。
	// 面板是浏览器可交互的管理面（登录换发 30 天会话 cookie + 全量运维端点），
	// 空密钥部署等于把面板裸奔在监听地址上。校验在 main（normalize 层不做：
	// panel.enabled 缺省 true，库内拦截会让全部既有空 api_key 配置一起拒载）。
	if cfg.Panel.Enabled && strings.TrimSpace(cfg.APIKey) == "" {
		log.Fatalf("panel.enabled=true 但 api_key 为空：请先在 config.json 配置 api_key，或将 panel.enabled 置 false 拒绝启动面板")
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// 模型映射链头种子（v1.2.0 设计文档 §4.3.1）：config model_map 段启动即生效；
	// 运行期由面板设置页热改（SetModelMap + saveConfig 写回）。空表零开销路径。
	if len(cfg.ModelMap) > 0 {
		server.SetModelMap(cfg.ModelMap)
		log.Printf("[model_map] 用户自定义模型映射已生效：%d 条", len(cfg.ModelMap))
	}

	// global realm 路由开关（config global.enabled，缺省 true）：注入 auth 包全局闸。
	// Realm()/IsGlobal() 先过此闸——显式 false 时恒 cn（逃生门：纯 CN 锁定的第一道闸）。
	auth.SetGlobalEnabled(cfg.Global.Enabled)

	// model.json 本地缓存接线（context_length 四级查找链第 3 级）：数据目录与
	// state.json 同风格（Docker volume 持久化路径 ./data）。首次缺失/损坏自动回落
	// 仓库种子 embed；models.dev 按需拉取成功后原子写回。
	upstream.SetModelCatalogPath(modelJSONPath(cfg.StateFile))

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	defer p.Close() // 进程退出前停后台落盘 goroutine + 最后补一次落盘（FIX-4:goroutine 泄漏）
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// auths 目录热加载：新增凭证文件自动进池，免去「加完账号手动重启网关」。
	// 启动时的 SyncToDir 已建立基线，监听只在后续目录内容变化时触发（见 pool/watch.go）。
	stopWatch := p.StartAuthDirWatch(cfg.AuthDir)
	defer stopWatch()

	// 熔断器 + 在途上限 + 三因子加权调优（从 config 注入，非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	// 连败降权（issue #114）：ErrClient/传输层连败 N 次临时出池。
	p.SetDegrade(cfg.Pool.DegradeThreshold, cfg.DegradeCooldownDur, cfg.DegradeCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(cfg.Pool.MaxInFlightGlobal) // global 域在途分档（WAF 403 修复 P1-1，默认 2）
	p.SetSoftRateMax(cfg.SoftRateMaxDur)               // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	p.SetCostExploreInterval(cfg.CostExploreIntervalDur) // costTier 探索窗口（issue #136，默认 30m；0 关停）

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
			// 按模型的可用性口径：绑定号在当前模型被 6004 限额时重分配，
			// 而不是被钉在这个号上反复失败。
			// realm 感知闭包：带前缀模型名按 realm 过滤可用账号（跨 realm 不泄漏，
			// 见 wiring.go）；裸名走 cn（现状零回归）。
			AvailableForModel: realmAwareAvailableForModel(p),
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	// 用量分桶记录器（panel 移植件，v1.2.0）：与 state 文件同目录，datapath 由
	// state_file 路径推导，避免再加一个配置项。usage.enabled=false 时不构建，
	// Handler/面板以 nil 判定"未启用"。
	var usageRec *usage.Recorder
	if cfg.Usage.Enabled {
		usageRec = usage.New(usagePathFor(cfg.StateFile))
		usageRec.Start()
		defer usageRec.Stop()
		log.Printf("[usage] 逐请求用量记录已启用: %s (%s)", usagePathFor(cfg.StateFile), usageRec.Describe())
		server.SetUsageRecorder(usageRec)
	}

	// live 承载可热改字段（api_key/soft_rate/脱敏开关），面板保存配置时在线替换
	// （v1.2.0 panel 移植件；withAuth/会话签名经 Holder 快照读，改 key 免重启生效）。
	live := livecfg.New(livecfg.Snapshot{
		APIKey:               cfg.APIKey,
		SoftCooldown:         cfg.SoftRateDur,
		SanitizeFingerprints: cfg.Features.SanitizeBlacklistFingerprints,
	})

	up := upstream.New()
	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints = cfg.Features.SanitizeBlacklistFingerprints
	// 出站 UA（A 段）：非空才做显式覆盖，空 = 默认 WorkBuddy 三段式
	// `WorkBuddy/<client_version> WorkBuddy/<client_version> CLI/<cli_version>`。
	up.UserAgent = cfg.Upstream.UserAgent
	// 版本段（upstream.client_version / cli_version）：空 = 各走内置默认。
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	// 设备风控头（X-Device-Token）全局兜底 + 文件读取路径；空 = 不注入。
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	// 用量归属头（X-Product/X-IDE-*）+ 客户端 IP 透传开关（见 ChatHeaders / handler）。
	up.ClientName = cfg.Upstream.ClientName
	up.PassthroughIP = cfg.Upstream.PassthroughIP
	// global realm 双域路由（config global 段）：base 空回落内置默认 https://www.workbuddy.ai；
	// GlobalEnabled 与 auth 包开关一致（双保险第二道闸在 upstream.globalOn）。
	up.ChatBaseGlobal = cfg.Global.ChatBase
	up.BillingBaseGlobal = cfg.Global.BillingBase
	up.GlobalEnabled = cfg.Global.Enabled

	sch := scheduler.New(scheduler.Config{
		Pool:                p,
		Upstream:            up,
		CheckinHours:        cfg.Schedule.CheckinHours,
		TravelHours:         cfg.Schedule.TravelHours,
		ActivityHours:       cfg.Schedule.ActivityHours,
		KeepaliveHours:      cfg.Schedule.KeepaliveHours,
		SchoolHours:         cfg.Schedule.SchoolHours,
		CatHours:            cfg.Schedule.CatHours,
		ActivityReportCount: cfg.Schedule.ActivityReportCount,
		ExpiringSoonWindow:  cfg.ExpiringSoonDur, // 快过期积分优先消耗（issue:积分过期）
		CheckinDisabled:     !cfg.Schedule.CheckinEnabled,
		TravelDisabled:      !cfg.Schedule.TravelEnabled,
		ActivityDisabled:    !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:   !cfg.Schedule.KeepaliveEnabled,
		SchoolDisabled:      !cfg.Schedule.SchoolEnabled,
		CatDisabled:         !cfg.Schedule.CatEnabled,
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每号 %d 条，点亮连登 + 补满领猫对话门槛）", cfg.Schedule.ActivityHours, cfg.Schedule.ActivityReportCount)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	if !cfg.Schedule.SchoolEnabled {
		log.Printf("开学季任务已禁用（schedule.school_enabled=false）")
	} else {
		log.Printf("开学季任务已启用：%v 点（school_open_day_2026.py ALL --run --yes）", cfg.Schedule.SchoolHours)
	}
	if !cfg.Schedule.CatEnabled {
		log.Printf("夜猫子任务已禁用（schedule.cat_enabled=false）")
	} else {
		log.Printf("夜猫子任务已启用：%v 点（task_runner.py ALL --yes --only black_cat）", cfg.Schedule.CatHours)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)

	// Web 管理面板（v1.2.0，panel 移植件）：/panel/api/* 原生 + /api/* 别名
	//（manager 壳契约）+ 登录会话。cfg.Panel.Enabled=false 时不构建（网关行为
	// 与引入前一致）。panel.enabled=true 且 api_key 空已在 normalize fail-fast。
	var panelHandler http.Handler
	var staticFS http.FileSystem
	if cfg.Panel.Enabled {
		// 管理面板日志镜像：标准 log（stderr）与 chat 表格日志（stdout）双路复制
		// 进面板环形缓冲，供 /panel/api/logs（与 /api/logs 别名）读取；控制台输出不变。
		pn := panel.New(panel.Config{
			Pool:               p,
			Usage:              usageRec,
			Upstream:           up,
			Scheduler:          sch,
			AuthDir:            cfg.AuthDir,
			APIKey:             cfg.APIKey,
			RedisMode:          redisMode,
			StickyCount:        sessCount,
			Version:            server.AppVersion(),
			Live:               live,
			ExpiringSoonWindow: cfg.ExpiringSoonDur,
			ProbeFile:          stateSibling(cfg.StateFile, "output_probes.json"),
			LoopbackOnly:       cfg.Panel.LoopbackOnly,
			ConfigPath:         *cfgPath,
			LoadConfig: func() (any, error) {
				return Load(*cfgPath)
			},
			SaveConfig: func(raw []byte) ([]string, error) {
				return saveConfig(raw, *cfgPath, live, p, up, sch)
			},
		})
		log.SetOutput(io.MultiWriter(os.Stderr, pn.Logs()))
		// chat 表格日志不走 log 包（stdout 直写），单独镜像进面板 ring 的 chat 频道。
		server.SetChatLogOutput(io.MultiWriter(os.Stdout, pn.Logs()))
		panelHandler = pn

		// 前端静态托管：embed_panel 标签构建内嵌 manager 壳产物（DistFS）；
		// 默认构建 HasEmbeddedFrontend()=false，根路径不注册静态路由（网关照常）。
		if panel.HasEmbeddedFrontend() {
			if fsys, ok := panel.DistFS(); ok {
				staticFS = http.FS(fsys)
				log.Printf("[panel] 前端静态托管已内嵌（-tags embed_panel 构建）")
			}
		} else {
			log.Printf("[panel] 未内嵌前端（默认构建）：根路径不提供管理页面；生产构建请用 -tags embed_panel（先在 web/ 执行前端构建并拷入 internal/panel/dist/）")
		}
	}

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		Session:      sessRouter,
		StickyCount:  sessCount,
		RedisMode:    redisMode,
		SoftCooldown: cfg.SoftRateDur,
		PromptMode:   cfg.Prompt.Mode,
		PromptText:   cfg.PromptText,
		// global realm 开关（handler 侧第三道闸：modelList 据此决定是否列 global 名单）。
		GlobalEnabled: cfg.Global.Enabled,
		// /admin 管理面（本地 tasks/credits/shutdown + 上游 accounts 运维端点共用
		// admin.enabled 开关；缺省关闭。本地端点开启后 loopback + api_key 双重限制）。
		// OnShutdown=stop：POST /admin/shutdown 等价一次 Ctrl+C，走既有 flush→close→
		// srv.Shutdown 优雅路径（stop 可重复调用，defer 再触发无害）。
		Admin: server.AdminConfig{
			Enabled:                  cfg.Admin.Enabled,
			CreditRefreshMinInterval: time.Duration(cfg.Admin.CreditRefreshMinIntervalSec) * time.Second,
			ConfigPath:               *cfgPath,
		},
		Sched:      sch,
		OnShutdown: stop,
		// 面板与静态托管（v1.2.0）：panel.enabled=false 时二者均为 nil（路由不注册）。
		Panel:  panelHandler,
		Static: staticFS,
	})

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout 覆盖整个请求读取（含 body）：防慢速 body 拖死连接。
		// max_body_mb 已移除（请求体无上限，交由上游自然响应），超大 body 成为
		// 唯一的自然约束：60s 内传不完会得到连接错误（read timeout）而非 413。
		ReadTimeout: 60 * time.Second,
		// IdleTimeout keep-alive 空闲连接回收：配合 ctx 传播（FIX-2）防连接泄漏堆积。
		// 注意：SSE 流式响应期间连接非空闲，不受此项掐断；不设全局 WriteTimeout
		// （长流式生成合法时长可达数分钟，全局 WriteTimeout 会误杀在途 SSE）。
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		// Flush 已把最后一笔状态快照提交给 Redis（fire-and-forget）；store.Close
		// 等 Upstash 在途/排队写排空再关连接——最后一笔镜像必须写完才退出（发现 4）。
		// Noop 的 Close 是空操作；单写上限 5s × 上限 8，Close 内部另有超时兜底。
		if cErr := store.Close(); cErr != nil {
			log.Printf("WARN: [server] redisstore close: %v", cErr)
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if cfg.Global.Enabled {
		log.Printf("global realm 已启用（chat_base=%q billing_base=%q，空=默认 workbuddy.ai）",
			cfg.Global.ChatBase, cfg.Global.BillingBase)
	} else {
		log.Printf("global realm 已禁用（config global.enabled=false，纯 CN）")
	}
	log.Printf("workbuddy2api listening on %s (api_key=%v)", cfg.Listen, cfg.APIKey != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}
