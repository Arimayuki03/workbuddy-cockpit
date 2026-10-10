// Package panel 内嵌式 Web 管理面板：账号池总览、单号运维（解冻/停用/签到/
// 刷新余额/移除）、浏览器内 OAuth 添加账号（免重启热加载进池）、手动批量
// 签到/保活，以及运行日志环形缓冲（镜像 log 包与 chat 表格日志）。
//
// 设计约束：
//   - 鉴权复用网关 api_key（Bearer 或会话 cookie 双通道，见 sessionAuth），
//     与 /v1/* 同一口径；api_key 为空 = 不鉴权（仅本机/私网使用）；
//   - 不改写既有池语义：所有运维操作落到 pool 已有入口（ReviveDisabled/
//     SetManualDisabled/Remove...），添加账号走 auth.SaveAtomic + pool.Add，
//     重启后与 auths/ 目录天然对齐；
//   - 路由双前缀：原生 /panel/api/* 保留（内部复用），另注册 /api/* 别名
//     （manager 壳契约，前端 lib/api.ts 全部走 /api/*，避开 /api/request_logs
//     与 /api/system/check-update；OAuth 扫码族契约路径为 /api/auth/*）。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/httpauth"
	"workbuddy2api/internal/keystore"
	"workbuddy2api/internal/livecfg"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usage"
)

// Config 面板依赖（main 装配注入）。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	Scheduler *scheduler.Scheduler // 手动触发签到/保活；nil 时对应接口返回 501
	AuthDir   string               // OAuth 登录完成后凭证落盘目录
	APIKey    string               // 空 = 不鉴权（与主服务同语义）；与 Live 同时给出时 Live 优先
	RedisMode string               // "upstash" / "noop"，仅观测透出
	Version   string               // 面板版本号（展示用）

	// Live 运行期可变配置（在线改配置立即生效）。
	Live *livecfg.Holder

	// ConfigPath config.json 路径与加载器（配置页读写用）。
	// LoadConfig 返回解析后的配置对象（前端展示/校验用，具体类型由 main 注入的闭包决定）；
	// nil 时配置页返回 501。
	ConfigPath string
	LoadConfig func() (any, error)
	// SaveConfig 校验并落盘配置，返回需要重启才能生效的字段列表；随后由 main 注入的
	// ApplyConfig 闭包完成热生效（池参数/排程/密钥/脱敏）。error 时配置不写盘。
	SaveConfig func(raw []byte) (restartRequired []string, err error)

	// StickyCount 返回粘性会话绑定数；nil 时报告 0。
	StickyCount func() int

	// UpstashSavedToken 返回 config.json 里已保存的 upstash token（面板"测试
	// Upstash 连通性"在 token 输入框留空时回落用）；nil 或返回空 = 无已保存值。
	UpstashSavedToken func() string

	// Usage 逐请求用量记录器（nil = 用量接口返回 501）。
	Usage *usage.Recorder

	// ProbeFile 模型输出上限探测结果文件（scripts/probe_max_tokens.py --panel-out
	// 写入；空或文件不存在 = model_probes 端点返回空集，面板不显示任何实测标注）。
	// 只读展示：网关不解析、不依赖其内容做任何路由/出站决策。
	ProbeFile string

	// ExpiringSoonWindow 快过期积分窗口（config pool.expiring_soon 的解析值）：
	// 余额查询按此窗口分桶，账号签到/余额刷新时把快过期积分子集写回池内
	// （选号优先消耗）。<=0 时上游不分桶（与 scheduler 同口径）。
	ExpiringSoonWindow time.Duration

	// TokenPath 管理面 API token 落盘路径（wbt_ 前缀，data/panel_tokens.json；
	// 空 = 纯内存形态，重启即失效）。生产装配用 stateSibling(state_file) 同目录。
	TokenPath string

	// Credits 积分流水追踪器（credit_record.go，nil = 流水整体关闭）：
	// 余额快照比对记账（去重/跳变/基线三口径）+ /api/tasks/records 数据源。
	// 生产装配用 data/credit_snapshots.json + data/credit_records.json。
	Credits *CreditTracker

	// LoopbackOnly 仅允许回环地址访问面板（config panel.loopback_only，默认 false）。
	// true 时 withAuth 前置闸：RemoteAddr 非 127.0.0.1/::1 的请求一律 403。
	LoopbackOnly bool

	// TrustedProxyCIDRs / TrustedProxyHops 可信代理解析参数（与 server.Handler
	// 的 TrustedProxyCIDRs/Hops 共用 main 装配的同一份 cfg.SecurityTrustedCIDRs
	// 解析结果——网关与面板对「真实客户端 IP」的判定口径必须一致，否则出现
	// 网关放行、面板限速误判的错位）。仅用于登录限速等"真实客户端识别"路径
	// （session.go clientIP）；loopback_only 安全闸不受影响（恒按 RemoteAddr 从严）。
	// 空 = 不信任转发头（零配置安全默认，行为与未注入一致）。
	TrustedProxyCIDRs []netip.Prefix
	TrustedProxyHops  int

	// IPRules 入站 IP 黑白名单规则引擎（security 段，iprules.go；nil = 安全页
	// 规则保存端点 501、GET 回显空规则）。生产装配传 server.NewIPRules 的产物
	//（与 handler 侧同一实例：面板热改规则，网关鉴权链即时生效）。
	IPRules *server.IPBlockRules

	// KeyStore 多密钥分发库（wbk_ 网关密钥，internal/keystore；nil = 密钥页
	// 全部端点 501）。生产装配恒非 nil（main 的 keystore.Open 产物，空库也注入），
	// 测试可省。keys_ep.go 六端点的存储层。
	KeyStore *keystore.Store
}

// Panel 管理面板 handler。挂载方式：外层 mux Handle("/panel/", panel)
// 与 Handle("/api/", panel)（后者是 manager 壳的别名前缀），本 mux 的
// pattern 均带两种前缀（外层不做前缀剥离）。
type Panel struct {
	cfg     Config
	mux     *http.ServeMux
	started time.Time
	logs    *Ring

	// logins 进行中的 OAuth 设备授权会话（state → 会话信息）。
	// poll 成功或超时（loginTTL）后剔除；面板常驻进程，容量天然有界。
	loginMu sync.Mutex
	logins  map[string]loginSession

	// taskMu/taskLocks 一键完成任务的 per-account 互斥：同一账号的任务动作
	// （单任务 / 全量）同时只允许一条在跑。重复点击直接返回 409"仍在执行"，
	// 而不是并发跑两遍浪费上游请求（动作虽幂等，expert 系每遍含 8 次真实对话）。
	// 不同账号之间不互斥（并行照旧）。TryLock 语义，锁条目常驻（账号数有界）。
	taskMu    sync.Mutex
	taskLocks map[string]*sync.Mutex

	// batchMu/batchLocks 全账号批量任务的按类型互斥（签到/旅行/活跃/保活/
	// 余额/开学季）：同一类型同时只允许一轮全账号扫描在跑，重复触发返回
	// 409「同类任务正在执行」而不是叠加扫描（TryLock 语义，条目常驻）。
	// 注：只防面板手动触发的重入；scheduler 定时排程不经此锁（既有语义不变）。
	batchMu    sync.Mutex
	batchLocks map[string]*sync.Mutex

	// 任务中心执行队列（taskcenter.go）。
	queueOnce sync.Once
	q         *queueState

	// abortMu/abortCh 长动作中断广播：队列取消（tasksCancelQueue）时关闭通道，
	// 正在执行的 mp 真人节奏等待（每条 45s+，不可打断的分钟级占锁）在条目间隙
	// select 到关闭即提前退出，保留进度下次续报。容量 0 一次性信号；下一次
	// startTaskQueue 重新置一个新通道。nil 通道 select 永远阻塞（无队列的
	// 纯手动路径零开销）。
	abortMu sync.Mutex
	abortCh chan struct{}

	// tokenStore 管理面 API token 存储（wbt_ 前缀，见 tokens.go）。nil 时
	// token 通道整体关闭（withAuth 不认 Bearer wbt_；/api/tokens* 返回 501）。
	// 生产装配恒非 nil；测试可用 Config.TokenStore 注入或以 nil 简化。
	tokenStore *TokenStore

	// secMu/secTPCIDRs/secTPHops 安全页最近保存的 trusted_proxy 参数（POST
	// /api/security/rules 成功后镜像，GET 回显优先读它——SaveConfig/LoadConfig
	// 未注入的最小装配形态也能如实回显刚保存的值；见 security_ep.go）。
	secMu      sync.Mutex
	secTPCIDRs []string
	secTPHops  int
}

// taskAbortCh 返回当前的中断信号通道（nil = 无中断源）。
func (p *Panel) taskAbortCh() chan struct{} {
	p.abortMu.Lock()
	defer p.abortMu.Unlock()
	return p.abortCh
}

// abortRunningTasks 广播中断信号（队列取消时调用）；幂等，重复调用无效果。
func (p *Panel) abortRunningTasks() {
	p.abortMu.Lock()
	defer p.abortMu.Unlock()
	if p.abortCh != nil {
		close(p.abortCh)
		p.abortCh = nil
	}
}

// resetAbort 为新一轮队列执行重置中断通道。
func (p *Panel) resetAbort() {
	p.abortMu.Lock()
	defer p.abortMu.Unlock()
	p.abortCh = make(chan struct{})
}

// tryLockAccount 尝试锁定账号的任务执行；已在执行返回 false。
func (p *Panel) tryLockAccount(uid string) bool {
	p.taskMu.Lock()
	if p.taskLocks == nil {
		p.taskLocks = make(map[string]*sync.Mutex)
	}
	mu := p.taskLocks[uid]
	if mu == nil {
		mu = &sync.Mutex{}
		p.taskLocks[uid] = mu
	}
	p.taskMu.Unlock()
	return mu.TryLock()
}

// unlockAccount 释放账号任务锁（与 tryLockAccount 配对）。
func (p *Panel) unlockAccount(uid string) {
	p.taskMu.Lock()
	mu := p.taskLocks[uid]
	p.taskMu.Unlock()
	if mu != nil {
		mu.Unlock()
	}
}

// tryLockBatch 尝试锁定某类型的全账号批量任务；已在执行返回 false。
// 类型即调用方给定的任务名（checkin/travel/activity/keepalive/balance/school），
// 锁条目常驻（类型集合固定且有限）。
func (p *Panel) tryLockBatch(kind string) bool {
	p.batchMu.Lock()
	if p.batchLocks == nil {
		p.batchLocks = make(map[string]*sync.Mutex)
	}
	mu := p.batchLocks[kind]
	if mu == nil {
		mu = &sync.Mutex{}
		p.batchLocks[kind] = mu
	}
	p.batchMu.Unlock()
	return mu.TryLock()
}

// unlockBatch 释放类型批量锁（与 tryLockBatch 配对）。
func (p *Panel) unlockBatch(kind string) {
	p.batchMu.Lock()
	mu := p.batchLocks[kind]
	p.batchMu.Unlock()
	if mu != nil {
		mu.Unlock()
	}
}

// loginTTL 授权 URL 的最长有效期：超时的 state 直接回收，
// 防止"开了添加账号弹窗就走开"的会话永久滞留。
const loginTTL = 15 * time.Minute

// loginSession 进行中的 OAuth 会话：创建时刻 + realm（cn/global，用于落盘与端点切换）。
type loginSession struct {
	created time.Time
	realm   string // "cn" / "global"，缺省 cn
}

// New 构建面板。
func New(cfg Config) *Panel {
	if cfg.RedisMode == "" {
		cfg.RedisMode = "noop"
	}
	p := &Panel{
		cfg:        cfg,
		mux:        http.NewServeMux(),
		started:    time.Now(),
		logs:       NewRing(500),
		logins:     map[string]loginSession{},
		tokenStore: NewTokenStore(cfg.TokenPath),
	}
	p.routes()
	return p
}

// Logs 返回日志环形缓冲（main 经 MultiWriter 镜像 log 与 chat 表格日志进来）。
func (p *Panel) Logs() *Ring { return p.logs }

// api 路由注册 helper：panel 原生 /panel/api/* 与 manager 壳契约的 /api/* 双前缀
// 挂同一批 handler。aliasPath 为空串表示该路径不做 /api/* 别名（当前路由表
// 全部注册别名，该形态仅为扩展保留）。冲突路径（/api/request_logs、
// /api/system/check-update 已被 server.Handler 注册，ServeMux 重复注册会 panic）
// 天然避开——本表不出现这两个路径。
func (p *Panel) api(method, panelPath, aliasPath string, h http.HandlerFunc) {
	p.mux.HandleFunc(method+" /panel"+panelPath, p.withAuth(h))
	if aliasPath != "" {
		p.mux.HandleFunc(method+" "+aliasPath, p.withAuth(h))
	}
}

func (p *Panel) routes() {
	// manager 壳契约（v1.2.0）：登录换发会话 cookie / 会话查询 / 登出。
	// login/logout 本身不设鉴权闸（未登录态就是这两条路的调用方），但
	// loopback_only 闸必须覆盖：该闸是「谁能碰到面板」的网络边界，若只挂在
	// withAuth 里，login/logout 会成为绕过口（先登录拿 cookie 再谈鉴权）。
	// 循环闸只检查 RemoteAddr；loopback_only=false（默认）时恒放行，行为零变化。
	p.mux.HandleFunc("POST /api/login", p.loopback(p.handleLogin))
	p.mux.HandleFunc("GET /api/me", p.withAuth(p.handleMe))
	p.mux.HandleFunc("POST /api/logout", p.loopback(p.handleLogout))
	// manager 设置页：模型映射读写（server.ModelMapView/SetModelMap + 写回 config.json）。
	p.api("GET", "/api/settings/model-map", "/api/settings/model-map", p.handleGetModelMap)
	p.api("POST", "/api/settings/model-map", "/api/settings/model-map", p.handleSetModelMap)
	// manager 设置页：Upstash 连通性测试（settings.testUpstash；保存走 /api/config）。
	p.api("POST", "/api/settings/upstash/test", "/api/settings/upstash/test", p.testUpstash)

	// 全表 /api/* 别名（manager 壳契约，避开 server.Handler 已注册的
	// /api/request_logs、/api/system/check-update——ServeMux 重复注册 panic）。
	// OAuth 扫码加号族另挂 /api/auth/*：web/lib/api.ts 契约路径
	// （/panel/api/login/* 原生与 /api/login/* 同步保留，内部脚本兼容）。
	p.api("GET", "/api/overview", "/api/overview", p.overview)
	p.api("GET", "/api/logs", "/api/logs", p.logsHandler)
	p.api("GET", "/api/models", "/api/models", p.models)
	p.api("POST", "/api/login/start", "/api/auth/start", p.loginStart)
	p.api("GET", "/api/login/poll", "/api/auth/poll", p.loginPoll)
	p.api("GET", "/api/login/regions", "/api/auth/regions", p.loginRegions)
	p.api("POST", "/api/accounts/{uid}/revive", "/api/accounts/{uid}/revive", p.accountRevive)
	p.api("POST", "/api/accounts/{uid}/clear-cooldown", "/api/accounts/{uid}/clear-cooldown", p.accountClearCooldown)
	p.api("POST", "/api/accounts/{uid}/disable", "/api/accounts/{uid}/disable", p.accountDisable)
	p.api("POST", "/api/accounts/{uid}/enable", "/api/accounts/{uid}/enable", p.accountEnable)
	// 账号备注（accounts_note.go）：面板 accounts 页后端化备注的写入端点，空串=清除。
	// 写语义，不进 wbt_ token 分级表（分级表外恒拒，与 disable/enable 同口径）。
	p.api("POST", "/api/accounts/{uid}/note", "/api/accounts/{uid}/note", p.accountNote)
	// 出口代理线路（proxy_route.go）：线路表回显（密码脱敏）+ 账号绑定/解绑
	// （空 route = 解绑直连）。写语义，不进 wbt_ token 分级表（与 note 同口径）。
	p.api("GET", "/api/proxy_routes", "/api/proxy_routes", p.proxyRoutes)
	p.api("POST", "/api/accounts/{uid}/proxy_route", "/api/accounts/{uid}/proxy_route", p.accountProxyRoute)
	p.api("POST", "/api/accounts/{uid}/checkin", "/api/accounts/{uid}/checkin", p.accountCheckin)
	p.api("POST", "/api/accounts/{uid}/balance", "/api/accounts/{uid}/balance", p.accountBalance)
	p.api("POST", "/api/accounts/{uid}/remove", "/api/accounts/{uid}/remove", p.accountRemove)
	// 凭据导入/导出（transfer.go）：跨部署共享账号。导出含 token，走同一 withAuth 闸。
	p.api("GET", "/api/accounts/export", "/api/accounts/export", p.accountsExport)
	p.api("POST", "/api/accounts/import", "/api/accounts/import", p.accountsImport)
	p.api("GET", "/api/accounts/{uid}/tasks", "/api/accounts/{uid}/tasks", p.accountTasks)
	p.api("POST", "/api/accounts/{uid}/tasks/accept", "/api/accounts/{uid}/tasks/accept", p.accountTaskAccept)
	p.api("POST", "/api/accounts/{uid}/tasks/accept_all", "/api/accounts/{uid}/tasks/accept_all", p.taskAcceptAll)
	p.api("POST", "/api/accounts/{uid}/tasks/claim", "/api/accounts/{uid}/tasks/claim", p.accountTaskClaim)
	p.api("POST", "/api/accounts/{uid}/tasks/auto", "/api/accounts/{uid}/tasks/auto", p.accountTaskAuto)
	p.api("POST", "/api/accounts/{uid}/tasks/auto_all", "/api/accounts/{uid}/tasks/auto_all", p.accountTaskAutoAll)
	p.api("POST", "/api/tasks/scan_all", "/api/tasks/scan_all", p.tasksScanAll)
	p.api("POST", "/api/tasks/run_queue", "/api/tasks/run_queue", p.tasksRunQueue)
	p.api("POST", "/api/tasks/queue/cancel", "/api/tasks/queue/cancel", p.tasksCancelQueue)
	p.api("GET", "/api/tasks/queue", "/api/tasks/queue", p.tasksQueueStatus)
	// 积分流水记录（credit_record.go）：任务中心记录流 kind=credit 的读取端点。
	// uid 查询参数非空时按账号过滤；tracker 未装配（credit_track_enabled=false）
	// 返回 501，前端按「未启用」展示而非空白。
	p.api("GET", "/api/tasks/records", "/api/tasks/records", p.tasksRecords)
	// 手动全量续期巡检（renew.go 语义：仅临期账号；schedule.renew_enabled 只管定时）。
	p.api("POST", "/api/renew_all", "/api/renew_all", p.renewAll)
	// school/status 与 school/run_all 已随开学季活动结束（2026-09-24）下线；
	// 券码查询保留（历史券码仍可查）。
	p.api("GET", "/api/school/vouchers", "/api/school/vouchers", p.schoolVouchers)
	p.api("POST", "/api/checkin_all", "/api/checkin_all", p.checkinAll)
	p.api("POST", "/api/travel_all", "/api/travel_all", p.travelAll)
	p.api("POST", "/api/activity_all", "/api/activity_all", p.activityAll)
	p.api("POST", "/api/keepalive_all", "/api/keepalive_all", p.keepaliveAll)
	p.api("POST", "/api/balance_all", "/api/balance_all", p.balanceAll)
	p.api("GET", "/api/packages", "/api/packages", p.packages)
	p.api("GET", "/api/usage", "/api/usage", p.usage)
	p.api("POST", "/api/usage/save", "/api/usage/save", p.usageSave)
	p.api("GET", "/api/model_probes", "/api/model_probes", p.modelProbes)
	p.api("GET", "/api/config", "/api/config", p.getConfig)
	p.api("POST", "/api/config", "/api/config", p.saveConfig)
	// 安全页（security_ep.go）：入站 IP 规则读写 + 拦截日志 + 模型锁池。
	// GET 两条进 wbt_ token 分级表只读档（securityEndpointLevels），POST 保存
	// 属写语义，分级表外恒拒（与 /api/config 同口径）。
	p.api("GET", "/api/security", "/api/security", p.handleSecurityGet)
	p.api("POST", "/api/security/rules", "/api/security/rules", p.handleSecuritySetRules)
	p.api("GET", "/api/security/model_locks", "/api/security/model_locks", p.handleSecurityModelLocks)

	// API 密钥页（keys_ep.go，keystore 存储）：删除走 POST …/delete（前端
	// keyApi.remove 契约，非 DELETE 方法）。GET 两条进 wbt_ token 分级表只读档；
	// 创建/修改/删除/重置是写语义，分级表外恒拒（与 /api/config 同口径）。
	p.api("GET", "/api/keys", "/api/keys", p.keysHandler)
	p.api("POST", "/api/keys", "/api/keys", p.keyCreate)
	p.api("PATCH", "/api/keys/{id}", "/api/keys/{id}", p.keyUpdate)
	p.api("POST", "/api/keys/{id}/delete", "/api/keys/{id}/delete", p.keyDelete)
	p.api("POST", "/api/keys/{id}/reset_usage", "/api/keys/{id}/reset_usage", p.keyResetUsage)
	p.api("GET", "/api/keys/{id}/ips", "/api/keys/{id}/ips", p.keyIPs)

	// 管理面 API token 管理（tokens.go）：**仅会话 cookie 可用**（withAuthSession
	// 直挂，不走双通道）——泄露的 wbt_ token 不能创建/吊销/提权另一个 token，
	// 否则泄露即等于「自助持久化 + 提权」。明文仅 POST 响应返回一次。
	p.mux.HandleFunc("POST /api/tokens", p.withAuthSession(p.handleTokenCreate))
	p.mux.HandleFunc("POST /panel/api/tokens", p.withAuthSession(p.handleTokenCreate))
	p.mux.HandleFunc("GET /api/tokens", p.withAuthSession(p.handleTokenList))
	p.mux.HandleFunc("GET /panel/api/tokens", p.withAuthSession(p.handleTokenList))
	p.mux.HandleFunc("DELETE /api/tokens/{id}", p.withAuthSession(p.handleTokenDelete))
	p.mux.HandleFunc("DELETE /panel/api/tokens/{id}", p.withAuthSession(p.handleTokenDelete))
}

// ServeHTTP 统一入口：先写安全响应头再分发，保证页面、静态资源、API
// 与 401 错误响应全都带上（API 也可能在浏览器里被直接打开）。
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	p.mux.ServeHTTP(w, r)
}

// loopbackOnly 报告该请求是否可通过 loopback 闸。仅 cfg.LoopbackOnly=true 时
// 启用判定：RemoteAddr host 非 127.0.0.1/::1 的请求被拒。反代部署下 RemoteAddr
// 是反代地址，本闸按"直接暴露"语义设计（与 /admin 管理面同口径），
// 不解析 X-Forwarded-For（可伪造，不做鉴权依据）。
//
// 安全闸从严：本闸**刻意不换轨** iputil.RequestClientIP（cfg.TrustedProxyCIDRs
// 只供 session.go clientIP 的登录限速/审计路径使用）。转发头永远客户端可伪造，
// 若闸按转发头放行，攻击者加一行 X-Real-IP: 127.0.0.1 即可穿透网络边界；
// 按对端判定至多损失"反代后误 403"的可用性（此时应关掉 loopback_only 并依赖
// api_key 鉴权），绝不损失安全性。
func (p *Panel) loopbackOnly(r *http.Request) bool {
	if !p.cfg.LoopbackOnly {
		return true
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "::1"
}

// loopback 仅回环闸中间件：loopback_only=true 时把 RemoteAddr 检查前置到
// 不走 withAuth 的路由（login/logout），被拒请求统一 403。闸关闭（默认）
// 时直接透传，行为与无中间件完全一致。
func (p *Panel) loopback(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !p.loopbackOnly(r) {
			writeErr(w, http.StatusForbidden, "panel restricted to loopback")
			return
		}
		next(w, r)
	}
}

// withAuthSession 仅会话 cookie 闸（**不认 Bearer**）：管理 token 本身的端点专用。
// 原因见 routes() 注释——wbt_ token 一旦可以管理 token，泄露即等于持久化后门。
// api_key 为空（未启用鉴权）时与 sessionAuth 同口径放行。
func (p *Panel) withAuthSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !p.loopbackOnly(r) {
			writeErr(w, http.StatusForbidden, "panel restricted to loopback")
			return
		}
		if !p.sessionAuth(r) {
			writeErr(w, http.StatusUnauthorized, "session_required")
			return
		}
		next(w, r)
	}
}

// tokenEndpointLevel wbt_ token 可访问的端点分级（method + path 形态）。
//
// 分类依据（panel.go routes() 路由表逐条归类）：
//   - tokenLevelRead：纯读端点（GET）——readonly / admin 一律放行；
//   - tokenLevelOps：幂等运维端点（只查上游 / 触发缓存回写，不改账号状态）——
//     仅 admin scope 放行；
//   - 白名单外（含全部写端点、登录/登出、token 管理族、含敏感数据的 export）：
//     一律拒绝。
//
// 刻意不放行的边界（即使 admin scope）：
//   - GET /api/accounts/export：响应含上游 token 明文（凭据导出），脚本读数
//     用不到它，暴露面过大；
//   - POST /api/config、/api/settings/model-map 等：配置保存是状态变更；
//   - 全部 /api/tasks/*、/api/checkin_all 等：触发真实业务动作（写语义）；
//   - /api/auth/start|poll：OAuth 加号流程会落盘新凭证（写语义）；
//   - /api/logout、/api/login/*：会话族与 token 无关。
type tokenEndpointLevel int

const (
	tokenLevelRead tokenEndpointLevel = iota // 只读：readonly ∪ admin
	tokenLevelOps                            // 幂等运维：仅 admin
)

// tokenEndpointLevels 端点分级表。isWriteMethods 之外的 method（GET）在表内
// 且命中即两档放行；POST 条目仅 admin。
var tokenEndpointLevels = map[string]tokenEndpointLevel{
	// 只读（GET）：
	"GET /api/overview":             tokenLevelRead,
	"GET /api/logs":                 tokenLevelRead,
	"GET /api/models":               tokenLevelRead,
	"GET /api/usage":                tokenLevelRead,
	"GET /api/packages":             tokenLevelRead,
	"GET /api/model_probes":         tokenLevelRead,
	"GET /api/tasks/queue":          tokenLevelRead,
	"GET /api/school/vouchers":      tokenLevelRead,
	"GET /api/accounts/{uid}/tasks": tokenLevelRead,
	// 安全页只读（security_ep.go）：规则/拦截日志/模型锁池均为展示数据，
	// readonly token 可读；保存（POST /api/security/rules）不进表 = 拒绝。
	"GET /api/security":             tokenLevelRead,
	"GET /api/security/model_locks": tokenLevelRead,
	// 密钥页只读（keys_ep.go）：列表与来源 IP 是脚本取数场景（readonly 档）；
	// 创建/修改/删除/重置（POST/PATCH）是写语义，不进表 = 两档全拒。
	"GET /api/keys":          tokenLevelRead,
	"GET /api/keys/{id}/ips": tokenLevelRead,
	// 幂等运维：上游余额/积分查询（把结果写回池内缓存，无账号状态变更语义），
	// 以及用量落盘触发。仅 admin scope。
	"POST /api/accounts/{uid}/balance": tokenLevelOps,
	"POST /api/accounts/{uid}/checkin": tokenLevelOps, // 签到=解冻+余额刷新（幂等查询+缓存回写，上游"今日已签到"幂等）
	"POST /api/balance_all":            tokenLevelOps,
	"POST /api/usage/save":             tokenLevelOps,
}

// tokenWriteMethods 判定「写方法」的方法集合（GET 之外的表内条目按 admin 档判定）。
var tokenWriteMethods = map[string]bool{"POST": true, "PATCH": true, "PUT": true, "DELETE": true}

// tokenEndpointKey 归一化当前请求为分级表键：/panel 前缀剥离 + /api/accounts/
// {uid}/... 与 /api/keys/{id}/... 的实际 id 段通配。
func tokenEndpointKey(r *http.Request) string {
	path := strings.TrimPrefix(r.URL.Path, "/panel")
	segs := strings.Split(strings.Trim(path, "/"), "/")
	// /api/accounts/{uid}/<action> 与 /api/accounts/{uid} 两形态（export 无 uid，
	// 保留字面量使 export 键天然不命中通配——它本就不在表内）。
	if len(segs) >= 4 && segs[0] == "api" && segs[1] == "accounts" && segs[3] != "export" {
		segs[2] = "{uid}"
	} else if len(segs) == 3 && segs[0] == "api" && segs[1] == "accounts" && segs[2] != "export" {
		segs[2] = "{uid}"
	}
	// /api/keys/{id}/<action>（ips）通配为 {id}；/api/keys/{id} 两段形态只在
	// PATCH 出现——PATCH 是写方法，分级表查无条目恒拒，无需通配。
	if len(segs) == 4 && segs[0] == "api" && segs[1] == "keys" {
		segs[2] = "{id}"
	}
	return r.Method + " /" + strings.Join(segs, "/")
}

// tokenAllowed 判定 wbt_ token（scope=scopeVal）是否可访问当前请求。
// 分级表外的一切 → 拒绝；GET 条目两档放行；POST 条目仅 admin。
func (p *Panel) tokenAllowed(r *http.Request, scopeVal string) bool {
	level, listed := tokenEndpointLevels[tokenEndpointKey(r)]
	if !listed {
		return false
	}
	if !tokenWriteMethods[r.Method] {
		return true
	}
	return scopeVal == scopeAdmin && level == tokenLevelOps
}

// withAuth 面板 API 多通道鉴权：先过 loopback_only 回环闸（login/logout 走
// loopback 中间件共用同一判定），再按序验三通道：
//  1. 会话 cookie（manager 壳登录态）；
//  2. Bearer 网关 api_key（httpauth 常量时间比较，与 /v1/* 同口径；**wbt_ 形态
//     的值不进本分支**——api_key 是用户自选字符串，可能与 wbt_ 前缀撞形，
//     语义上它授权的是 /v1/* 模型调用，此处按既有契约继续作为面板全权通道）；
//  3. Bearer wbt_ 管理 token（tokens.go：prefix 定位 + 常量时间比较 + scope
//     白名单——只读脚本通道，写端点一律 403 token_write_forbidden）。
//
// 任一通过即放行。api_key 为空时放行（本机/私网部署，与主服务同语义）。
// 密钥经 livecfg 快照读取：面板里改了 api_key，下一个请求即用新值（无需重启）。
//
// 通道顺序说明：api_key 分支在 token 分支之前（保持既有 Bearer 语义逐字节
// 不变——纯新增）；wbt_ 前缀值在 api_key 分支里比较必然失败（除非管理员刻意
// 把 api_key 设成 wbt_ 开头，此时按 api_key 全权放行也是该配置的明确意图），
// 随后落进 token 分支按最小权限放行。
func (p *Panel) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !p.loopbackOnly(r) {
			writeErr(w, http.StatusForbidden, "panel restricted to loopback")
			return
		}
		if p.sessionAuth(r) {
			next(w, r)
			return
		}
		// Bearer 网关 api_key 通道（既有语义，保持不变）。
		apiKeyOK := httpauth.VerifyBearer(r, p.apiKey())
		if apiKeyOK {
			next(w, r)
			return
		}
		// Bearer wbt_ 管理 token 通道（新增；仅当值带 wbt_ 前缀才有意义）。
		if tc := p.checkTokenAuth(r); tc.ok {
			if p.tokenAllowed(r, tc.scope) {
				next(w, r)
				return
			}
			writeErr(w, http.StatusForbidden, "token_write_forbidden")
			return
		}
		writeErr(w, http.StatusUnauthorized, "invalid_api_key")
	}
}

// apiKey 当前生效密钥（Live 优先，回落静态字段）。
func (p *Panel) apiKey() string {
	if p.cfg.Live != nil {
		return p.cfg.Live.Load().APIKey
	}
	return p.cfg.APIKey
}

// ---------------------------------------------------------------------------
// 只读接口
// ---------------------------------------------------------------------------

// overview 总览：池计数 + 每账号状态 + 面板元信息。
func (p *Panel) overview(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := p.cfg.Pool.CountsDetailed()
	sticky := 0
	if p.cfg.StickyCount != nil {
		sticky = p.cfg.StickyCount()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":         p.cfg.Version,
		"uptime_sec":      int(time.Since(p.started).Seconds()),
		"auth_required":   p.apiKey() != "",
		"redis_mode":      p.cfg.RedisMode,
		"sticky_sessions": sticky,
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"in_flight_full":  inFlightFull,
		"accounts":        p.cfg.Pool.List(),
	})
}

// logsHandler 返回日志环形缓冲快照（时间升序，含频道标记 chat/task/sys）。
// channel 查询参数过滤（chat/task/sys；空 = 全部，panel 原生契约）。
func (p *Panel) logsHandler(w http.ResponseWriter, r *http.Request) {
	entries := p.logs.Snapshot()
	if ch := r.URL.Query().Get("channel"); ch != "" {
		filtered := entries[:0:0]
		for _, e := range entries {
			if e.Ch == ch {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// models 实时查询上游模型列表与 reasoning 实际档位（直连上游，不读路由层 1h 缓存）：
// 回答"该模型到底支持哪几档思考"。顺带刷新 client 的 effort 降级能力缓存。
// 与 /v1/models 同口径的双域输出：CN 域模型加 "cn:" 前缀、global 域加 "global:" 前缀
// （gateway 路由协议，前端显示的 id 就是调用时要填的完整 model 值）。
// 各域独立探测、独立容错：某域无可用账号则整域跳过；两域全空时才报错
// （有错误明细回 502，一个账号都没有回 503）。
// realm 查询参数（前端按当前版本取列表）："cn" 只探 CN 域、"global" 只探
// global 域，空/其它值保持双域——省掉切版本后前端丢弃半份结果的探测开销，
// 也避免「探了但没用上」的全局错误副作用。
func (p *Panel) models(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]any, 0)
	var fetchErrs []string

	realm := r.URL.Query().Get("realm")
	probeCN := realm == "" || realm == "cn"
	probeGlobal := realm == "" || realm == "global"

	// CN 域：有可用 CN 账号才查（此前无条件 Pool.Pick()+FetchModels——选中 global
	// 账号时打 CN 端点必然失败，混合池表现为偶发 502，纯 global 池必炸）。
	if probeCN {
		if uids := p.cfg.Pool.AvailableUIDsForRealm("cn"); len(uids) > 0 {
			if acct := p.cfg.Pool.AuthByUID(uids[0]); acct != nil {
				infos, err := p.cfg.Upstream.FetchModels(acct)
				if err != nil {
					fetchErrs = append(fetchErrs, "cn: "+err.Error())
				} else {
					for _, mi := range infos {
						out = append(out, panelModelEntry("cn", mi, mi.Efforts, mi.DefaultEffort, p.cfg.Upstream.HTTP))
					}
				}
			}
		}
	}

	// global 域：路由开关开且有可用 global 账号才查（独立目录端点，FetchGlobalModelInfos；
	// Upstream.GlobalEnabled 是探测侧同一道闸，与 main 装配的 config global.enabled 一致）。
	if probeGlobal && p.cfg.Upstream.GlobalEnabled {
		if uids := p.cfg.Pool.AvailableUIDsForRealm("global"); len(uids) > 0 {
			if acct := p.cfg.Pool.AuthByUID(uids[0]); acct != nil {
				infos := p.cfg.Upstream.FetchGlobalModelInfos(acct)
				if len(infos) == 0 {
					fetchErrs = append(fetchErrs, "global: 上游未返回可用模型")
				} else {
					efforts, defaults := p.cfg.Upstream.GlobalEffortSnapshot()
					for _, mi := range infos {
						out = append(out, panelModelEntry("global", mi, efforts[mi.ID], defaults[mi.ID], p.cfg.Upstream.HTTP))
					}
				}
			}
		}
	}

	if len(out) == 0 {
		if len(fetchErrs) > 0 {
			writeErr(w, http.StatusBadGateway, "fetch models: "+strings.Join(fetchErrs, "; "))
			return
		}
		writeErr(w, http.StatusServiceUnavailable, "没有可用账号：请先在面板添加账号再查询")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": out})
}

// panelModelEntry 构造单个模型条目（两域共用）：id 带 realm 前缀（调用值即显示值），
// context_length / max_output_tokens 走四级查找链，effort 档位按 realm 域取
// EffortListing（远端权威 ∪ 静态兜底表）——与 /v1/models 同一口径，两侧不再漂移。
// 注：主仓库 ModelInfo 无 panel 快照的 CanDisableThinking 字段（上游未下发），
// 该键不透出（漂移适配，前端按缺省处理）。
func panelModelEntry(realm string, mi upstream.ModelInfo, remoteEfforts []string, remoteDefault string, httpc *http.Client) map[string]any {
	entry := map[string]any{
		"id":                 realm + ":" + mi.ID,
		"name":               mi.Name,
		"default_effort":     mi.DefaultEffort,
		"supported_efforts":  mi.Efforts,
		"supports_reasoning": mi.SupportsReasoning,
		"supports_images":    mi.SupportsImages,
		"credits":            mi.Credits,
		"description":        mi.Description,
		"tags":               mi.Tags,
		"vendor":             mi.Vendor,
		"is_default":         mi.IsDefault,
		"supports_tool_call": mi.SupportsToolCall,
		"only_reasoning":     mi.OnlyReasoning,
		"reasoning_effort":   mi.ReasoningEffort,
		"reasoning_summary":  mi.ReasoningSummary,
	}
	if mi.MaxAllowedSize > 0 {
		entry["max_allowed_size"] = mi.MaxAllowedSize
	}
	entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, httpc)
	if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, httpc); ok {
		entry["max_output_tokens"] = mo
	}
	if efforts, def := upstream.EffortListing(realm, mi.ID, remoteEfforts, remoteDefault); efforts != nil {
		entry["supported_efforts"] = efforts
		if def != "" {
			entry["default_effort"] = def
		}
	}
	return entry
}

// modelProbes 返回模型输出上限的探测结果（scripts/probe_max_tokens.py --panel-out
// 写入的契约文件），供前端在「模型与档位」的实测列做风险标注。
//
// 设计边界：纯只读透传——文件缺失/未配置返回空集（面板退化为无标注，与历史行为
// 一致），网关自身不解析字段语义、不据此做任何路由或出站决策；上游改了限制后
// 重跑一次工具、下次查询即刷新，无需重启网关。
func (p *Panel) modelProbes(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"probes": map[string]json.RawMessage{}, "exists": false}
	if p.cfg.ProbeFile == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	raw, err := os.ReadFile(p.cfg.ProbeFile)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, out)
			return
		}
		writeErr(w, http.StatusInternalServerError, "read probes: "+err.Error())
		return
	}
	var f struct {
		Version int                        `json:"version"`
		Probes  map[string]json.RawMessage `json:"probes"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		writeErr(w, http.StatusBadGateway, "parse probes: "+err.Error())
		return
	}
	if f.Probes == nil {
		f.Probes = map[string]json.RawMessage{}
	}
	out["probes"] = f.Probes
	out["exists"] = true
	if fi, err := os.Stat(p.cfg.ProbeFile); err == nil {
		out["updated_at"] = fi.ModTime().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// 账号运维
// ---------------------------------------------------------------------------

// accountRevive 复活：清自动禁用 + 连续 12153 计数（ReviveDisabled）并清手动
// 停用位（SetManualDisabled(false)）。panel 原生 Revive 是"无条件全恢复"语义，
// 主仓库把禁用拆成自动/手动两位独立状态——面板"复活"的意图是让号回到可用，
// 故两位都清（不动冷却/熔断：那些到期/成功自愈，与 /admin revive 口径一致）。
func (p *Panel) accountRevive(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.ReviveDisabled(uid)
	p.cfg.Pool.SetManualDisabled(uid, false, "")
	log.Printf("panel: revive uid=%s（人工清除禁用/手动停用）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountClearCooldown 运维强制清除冷却与模型限流（吸收 workbuddy-manager 1.0.64
// 的 force-clear）：冷却域 + 熔断 + 连败降权 + 6004 模型级冷却表全归零，
// 不碰禁用/手动停用两位。账号冷却中的场景下，等自然到期没有意义时用。
func (p *Panel) accountClearCooldown(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	found, cleared := p.cfg.Pool.ForceClearCooldown(uid)
	if !found {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if cleared {
		log.Printf("panel: clear-cooldown uid=%s（人工强制清除冷却/限流）", uid)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cleared": cleared})
}

// accountDisable 人工停用（SetManualDisabled(true)：只摘除选号流量，排程照常，
// 凭证与积分保持活跃——主仓库手动停用位语义；panel 原生 Disable 是永久禁用，
// 语义收窄为可逆停用，恢复走 enable/revive）。
func (p *Panel) accountDisable(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.SetManualDisabled(uid, true, "manual disable (panel)")
	log.Printf("panel: disable uid=%s（人工停用）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountEnable 解除人工停用（SetManualDisabled(false)）。主仓库手动停用位
// 的对称操作（panel 原生无此端点，manager 壳 accounts 页两态按钮需要）。
func (p *Panel) accountEnable(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.SetManualDisabled(uid, false, "")
	log.Printf("panel: enable uid=%s（解除人工停用）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountCheckin 单号签到：DailyCheckin + 余额查询解冻（已签到等业务错误不阻塞余额刷新），
// 与 scheduler.RunCheckinNow 的单号语义一致。
func (p *Panel) accountCheckin(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	checkinMsg := ""
	// 签到前基线查询：签到到账流水以「签到前余额」为基线（scheduler 侧 CheckinAll
	// 同口径）——签到与上次快照刷新之间隔着 API 消耗，快照比对会把奖励抵成净减
	// 而漏记。查询失败只降级（本次不记签到流水），不阻塞签到。
	creditBefore, creditBeforeOK := int64(0), false
	if v, _, berr := p.cfg.Upstream.UserResourceDetailed(a, 0); berr == nil {
		creditBefore, creditBeforeOK = v, true
	}
	if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
		checkinMsg = err.Error() // "今天已签到"等业务错误照常查余额
	}
	resp := map[string]any{"ok": true}
	if checkinMsg != "" {
		resp["checkin_message"] = checkinMsg
	}
	remain, buckets, err := p.cfg.Upstream.UserResourceDetailed(a, p.cfg.ExpiringSoonWindow)
	if err != nil {
		resp["balance_error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	p.cfg.Pool.ReenableIfCredits(uid, remain)
	// 分桶补写：快过期子集供三因子选号的 ×8 权重因子，与 scheduler.go 签到口径一致
	// （先解冻再补分桶；SetCreditsDetailed 会把 expiring 钳到 [0, credits]）。
	p.cfg.Pool.SetCreditsDetailed(uid, remain, buckets.Expiring)
	// 签到到账流水：以签到前基线记精确差值；基线查询失败时不记（0 基线会把全部
	// 余额误报成「刚获得」）。tracker 未装配时零开销直返。
	if creditBeforeOK {
		p.recordCheckin(uid, creditBefore, remain)
	}
	resp["credits"] = remain
	resp["credits_total"] = buckets.Total()
	log.Printf("panel: checkin uid=%s msg=%q credits=%d/%d", uid, checkinMsg, remain, buckets.Total())
	writeJSON(w, http.StatusOK, resp)
}

// accountBalance 单号余额刷新：UserResourceDetailed → SetCreditsDetailed
// （总量+快过期分桶，与 scheduler 签到/余额刷新口径一致；不触碰冷却状态）。
func (p *Panel) accountBalance(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	remain, buckets, err := p.cfg.Upstream.UserResourceDetailed(a, p.cfg.ExpiringSoonWindow)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "user resource: "+err.Error())
		return
	}
	p.cfg.Pool.SetCreditsDetailed(uid, remain, buckets.Expiring)
	p.recordBalance(uid, remain) // 积分流水（同 accountCheckin）
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credits": remain, "credits_total": buckets.Total()})
}

// recordBalance 面板侧余额刷新路径的积分流水记账钩子：经 Scheduler 上注入的
// CreditTracker 落流水（与签到/全量刷新同一 sink）。Scheduler 缺失时直返
// （测试形态/panel 无调度器的部署，流水只覆盖定时与全量路径）。
func (p *Panel) recordBalance(uid string, remain int64) {
	if p.cfg.Scheduler == nil {
		return
	}
	p.cfg.Scheduler.RecordBalanceChecked(uid, p.nicknameOf(uid), remain)
}

// recordCheckin 面板单号签到的签到到账记账钩子（before/after 差值口径，
// scheduler.CheckinAll 同款）：经同一 CreditTracker 落 source=checkin 流水。
// Scheduler 缺失时直返（同 recordBalance 口径）。
func (p *Panel) recordCheckin(uid string, before, after int64) {
	if p.cfg.Scheduler == nil {
		return
	}
	p.cfg.Scheduler.RecordCheckinChecked(uid, p.nicknameOf(uid), before, after)
}

// nicknameOf 池状态快照取昵称（文案展示用；缺账号回落 uid）。
func (p *Panel) nicknameOf(uid string) string {
	if st, ok := p.cfg.Pool.Status(uid); ok && st.Nickname != "" {
		return st.Nickname
	}
	return uid
}

// accountRemove 移除账号：先出池（立即落盘 state），再删 auth 文件。
// auths/ 目录监听（pool.StartAuthDirWatch）随后会对齐目录内容——Remove 已
// 即时出池，双方收敛于同一状态，无复活竞态。
func (p *Panel) accountRemove(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.Remove(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	fileMsg := ""
	if a.FilePath != "" {
		if err := os.Remove(a.FilePath); err != nil && !os.IsNotExist(err) {
			fileMsg = err.Error()
		}
	}
	if fileMsg != "" {
		log.Printf("panel: remove uid=%s（auth 文件删除失败: %s）", uid, fileMsg)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "file_error": fileMsg})
		return
	}
	log.Printf("panel: remove uid=%s（已出池并删除凭证文件）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// 批量任务
// ---------------------------------------------------------------------------

// checkinAll 手动触发全量签到（异步执行，进度看日志区/账号状态变化）。
// 按类型 TryLock 防重入：全账号动作都是秒级以上的扫描+上游调用，重复点击
// 会叠加执行、放大上游压力，已在跑时返回 409 友好提示（批量族共用口径）。
func (p *Panel) checkinAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	if !p.tryLockBatch("checkin") {
		writeErr(w, http.StatusConflict, "同类任务正在执行（全量签到），请等本轮结束后再试")
		return
	}
	go func() {
		defer p.unlockBatch("checkin")
		p.cfg.Scheduler.RunCheckinNow()
	}()
	log.Printf("panel: 手动全量签到已触发（含猫猫旅行）")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// travelAll 手动触发全量猫猫旅行巡检（异步执行；防重入口径同 checkinAll）。
func (p *Panel) travelAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	if !p.tryLockBatch("travel") {
		writeErr(w, http.StatusConflict, "同类任务正在执行（全量旅行巡检），请等本轮结束后再试")
		return
	}
	go func() {
		defer p.unlockBatch("travel")
		p.cfg.Scheduler.RunTravelNow()
	}()
	log.Printf("panel: 手动全量旅行巡检已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// activityAll 手动触发全量活跃上报（异步执行；点亮连登 + 解锁领养前置；
// 防重入口径同 checkinAll）。
func (p *Panel) activityAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	if !p.tryLockBatch("activity") {
		writeErr(w, http.StatusConflict, "同类任务正在执行（全量活跃上报），请等本轮结束后再试")
		return
	}
	go func() {
		defer p.unlockBatch("activity")
		p.cfg.Scheduler.RunActivityNow()
	}()
	log.Printf("panel: 手动全量活跃上报已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// keepaliveAll 手动触发全量 token 保活（异步执行；防重入口径同 checkinAll）。
func (p *Panel) keepaliveAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	if !p.tryLockBatch("keepalive") {
		writeErr(w, http.StatusConflict, "同类任务正在执行（全量保活），请等本轮结束后再试")
		return
	}
	go func() {
		defer p.unlockBatch("keepalive")
		p.cfg.Scheduler.RunKeepaliveNow()
	}()
	log.Printf("panel: 手动全量保活已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// balanceAll 手动全量刷新余额：并发查上游、写回池内 credits（含解冻语义），
// 完成后返回——面板紧接着拉 overview 即是最新值。账号量小（个位数），
// 同步等待（上限受短 RPC 超时约束）比"触发后盲刷"体验更确定。
// 按类型 TryLock 防重入：同步端点更怕叠加（每次都等全程），已在跑返回 409。
func (p *Panel) balanceAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	if !p.tryLockBatch("balance") {
		writeErr(w, http.StatusConflict, "同类任务正在执行（全量余额刷新），请等本轮结束后再试")
		return
	}
	defer p.unlockBatch("balance")
	p.cfg.Scheduler.RunBalanceRefreshNow()
	log.Printf("panel: 手动全量余额刷新完成")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": p.cfg.Pool.List()})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// usage 返回逐请求用量聚合。hours 查询参数控制时间窗（默认 72；"all" 或 <=0
// 表示自记录以来全量，即「启动以来」档的模型/账号排行；数字上限 1440=60 天）：
// series / by_account / by_model 只聚合窗口内的桶（面板的时间筛选对图和表同时生效）；
// totals / by_realm 恒为全量累计。「启动以来」档的时序保留近 30 天小时粒度，
// 更早折叠为日点，长期趋势不丢。
func (p *Panel) usage(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	hours := 72
	if v := r.URL.Query().Get("hours"); v != "" {
		if strings.EqualFold(v, "all") {
			hours = 0
		} else if n, err := strconv.Atoi(v); err == nil && n > 0 {
			hours = n
		}
	}
	if hours > 1440 {
		hours = 1440
	}
	// 昵称仅用于展示，取自池快照（不含任何凭证）。
	nicks := map[string]string{}
	for _, s := range p.cfg.Pool.List() {
		if s.Nickname != "" {
			nicks[s.UID] = s.Nickname
		}
	}
	writeJSON(w, http.StatusOK, p.cfg.Usage.Snapshot(hours, nicks))
}

// usageSave 立即把内存中的用量桶落盘（正常由后台 30s 防抖刷新负责）。
func (p *Panel) usageSave(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	p.cfg.Usage.Save()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// packages 返回全部账号的积分构成，供「积分构成 / 到期提醒」视图对比。
//
// 逐个账号向上游查（并发有上限，避免瞬时打满上游限流），失败只在对应账号上
// 标 error，不影响其它账号——一个号 token 失效不该让整页空白。
// v1.17 恢复逐包明细（upstream.CreditPackages，含 DeductionEndTime 到期语义）：
// remain/used/size 聚合口径不变，packages 数组每包带 name/remain/used/size/
// end_time/expires_at/created_at 等（前端按到期升序消费，FEFO 结论依赖它）。
// fetched_at 为本次响应生成时刻（实时透传上游、无服务端缓存，即数据年龄）；
// 前端据此展示「数据采集于 X 分钟前」，避免旧快照被误当实时。
func (p *Panel) packages(w http.ResponseWriter, r *http.Request) {
	accts := p.cfg.Pool.List()
	type row struct {
		UID           string                   `json:"uid"`
		Nickname      string                   `json:"nickname"`
		Realm         string                   `json:"realm"`
		Remain        int64                    `json:"remain"`
		Used          int64                    `json:"used"`
		Size          int64                    `json:"size"`
		PackagesCount int                      `json:"packages_count"`
		Packages      []upstream.CreditPackage `json:"packages"`
		Error         string                   `json:"error,omitempty"`
	}
	out := make([]row, len(accts))

	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, s := range accts {
		wg.Add(1)
		go func(i int, s pool.Status) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			it := row{UID: s.UID, Nickname: s.Nickname, Realm: s.Realm}
			a := p.cfg.Pool.AuthByUID(s.UID)
			if a == nil {
				it.Error = "account not loaded"
				out[i] = it
				return
			}
			packs, remain, size, err := p.cfg.Upstream.CreditPackages(a)
			if err != nil {
				it.Error = err.Error()
				out[i] = it
				return
			}
			it.Packages = packs
			it.PackagesCount = len(packs)
			var used int64
			for _, pk := range packs {
				used += pk.Used
			}
			it.Remain, it.Used, it.Size = remain, used, size
			out[i] = it
		}(i, s)
	}
	wg.Wait()

	// 余额降序：多的在前，便于和少的对比。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Remain > out[j].Remain })
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out, "fetched_at": time.Now().Format(time.RFC3339)})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}
