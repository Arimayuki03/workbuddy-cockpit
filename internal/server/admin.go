// admin.go — /admin 本地管理 API（TrafficMonitor 插件 workbuddy2api-trafficmonitor-plugin 配套）。
//
// 安全边界（三道）：
//  1. config admin.enabled=true 才注册路由（缺省 false：整个二进制一条 /admin* 路由都不存在，
//     未启用部署的行为与引入前逐位一致）；
//  2. 仅接受 loopback（127.0.0.1/::1）来源——listen 是 0.0.0.0 时，shutdown/credits 这类
//     敏感操作也不泄漏到局域网，LAN 一律 403；
//  3. 复用既有 Bearer api_key 鉴权（api_key 为空 = 与 /status 一致的本地无鉴权语义）。
//
// 端点：
//
//	GET   /admin/tasks        任务快照：kind/中文标签/enabled/hours/next_fire/running/last_run/last_result
//	POST  /admin/tasks/run    {kind|"all"} 异步手动触发；同 kind 撞车回 409（不排队不重复打上游）
//	PATCH /admin/tasks        {kind, enabled} 热改排程开关 + 写回 config.json（最小 diff + .bak 备份）；
//	                          或 {kind, hours:[...]} 热改触发小时（SetHours 免重启）+ 写回 config.json
//	POST  /admin/credits      实时积分（每号 1 次上游余额查询）：服务端冷却 + 单飞，超频 429
//	GET   /admin/credits      上次查询缓存 + 冷却截止时间（纯本地，零上游）
//	PATCH /admin/credits-interval {interval_sec} 热改冷却 + 写回 config.json（60–86400 秒）
//	POST  /admin/shutdown     优雅停机：走 main 注入的 ctx cancel（flush state → 关 store → srv.Shutdown）
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"strings"
	"time"

	"workbuddy2api/internal/scheduler"
)

// AdminConfig /admin 端点配置（由 cmd/server 从 config.json 注入）。
type AdminConfig struct {
	// Enabled 总开关（config admin.enabled，缺省 false = 不注册任何 /admin 路由）。
	Enabled bool
	// CreditRefreshMinInterval POST /admin/credits 两次上游查询的最小间隔（防手滑连点打爆上游，
	// 触发腾讯侧风控）。<=0 兜底 600s。计时锚点是查询开始时刻：慢查询自然拉长间隔。
	CreditRefreshMinInterval time.Duration
	// ConfigPath PATCH /admin/tasks 写回用的 config.json 路径（main 的 -config 实参，
	// 相对路径按服务进程 CWD 解析）。空 = 只改内存不落盘（响应 persisted=false 注明）。
	ConfigPath string
}

// adminState /admin 运行态。挂在 Handler.adm，仅 Admin.Enabled 时非 nil。
type adminState struct {
	// mu 除保护下列运行态字段外，还保护 h.cfg.Admin.CreditRefreshMinInterval 的
	// 热改读写：该字段挂在共享的 cfg 上，PATCH /admin/credits-interval 写、
	// GET/POST /admin/credits 读，锁外访问即数据竞争。registerAdmin 里的启动期
	// 默认值写入先于任何请求（无并发），不在其列。
	mu sync.Mutex
	// manual 手动触发去重标记（kind → true）：让 /tasks/run 能在响应里如实报告 busy，
	// 而不是把撞车甩给调度器的 ErrBusy 日志。实际互斥仍由 scheduler.runMu 保证。
	manual map[string]bool
	// creditRunning/creditLastStart 积分查询单飞 + 冷却（锚点=开始时刻）。
	creditRunning   bool
	creditLastStart time.Time
	// creditCache 上次成功查询的完整回执（含逐号 ok/error），GET /admin/credits 直接回放。
	creditCache *creditReport
}

// creditAccount/creditTotal/creditReport 与 cmd/credit 的 JSON 契约逐字段一致
// （uid/nickname/remain/used/size/packages/ok/error + total 汇总），插件两端一套解析。
type creditAccount struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Remain   *int64 `json:"remain"`
	Used     *int64 `json:"used"`
	Size     *int64 `json:"size"`
	Packages int    `json:"packages,omitempty"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

type creditTotal struct {
	Remain   int64 `json:"remain"`
	Used     int64 `json:"used"`
	Size     int64 `json:"size"`
	Accounts int   `json:"accounts"`
	OK       int   `json:"ok"`
	Failed   int   `json:"failed"`
}

type creditReport struct {
	Service  string          `json:"service"`
	Ts       int64           `json:"ts"`
	Total    creditTotal     `json:"total"`
	Accounts []creditAccount `json:"accounts"`
}

// adminEndpointCount registerAdmin 注册的 /admin 路由条数（上方 HandleFunc 调用数，
// 启动日志观测口径——与路由注册同处一个函数，增删路由时同步维护）。
const adminEndpointCount = 7

// registerAdmin 注册全部 /admin 路由（NewHandler 在 cfg.Admin.Enabled 时调用）。
func (h *Handler) registerAdmin() {
	h.adm = &adminState{manual: map[string]bool{}}
	// 启动期写默认值：先于任何请求，无并发；此后该字段的热改读写一律走 adm.mu。
	if h.cfg.Admin.CreditRefreshMinInterval <= 0 {
		h.cfg.Admin.CreditRefreshMinInterval = 600 * time.Second
	}
	h.mux.HandleFunc("GET /admin/tasks", h.withAdmin(h.adminTasks))
	h.mux.HandleFunc("POST /admin/tasks/run", h.withAdmin(h.adminTaskRun))
	h.mux.HandleFunc("PATCH /admin/tasks", h.withAdmin(h.adminTaskPatch))
	h.mux.HandleFunc("GET /admin/credits", h.withAdmin(h.adminCreditsGet))
	h.mux.HandleFunc("POST /admin/credits", h.withAdmin(h.adminCreditsRefresh))
	h.mux.HandleFunc("PATCH /admin/credits-interval", h.withAdmin(h.adminCreditsIntervalPatch))
	h.mux.HandleFunc("POST /admin/shutdown", h.withAdmin(h.adminShutdown))
	log.Printf("admin API 已启用（loopback only，%d 个端点，积分查询冷却 %s）",
		adminEndpointCount, h.cfg.Admin.CreditRefreshMinInterval)
}

// withAdmin 在既有 Bearer 鉴权前再垫一道 loopback 闸。
func (h *Handler) withAdmin(next http.HandlerFunc) http.HandlerFunc {
	authed := h.withAuth(next)
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackAddr(r.RemoteAddr) {
			writeOpenAIError(w, http.StatusForbidden, "forbidden", "/admin 仅接受本机（loopback）请求")
			return
		}
		authed(w, r)
	}
}

// isLoopbackAddr RemoteAddr（ip:port 或带 zone 的 IPv6）→ 是否回环来源。
func isLoopbackAddr(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// decodeAdminJSON 解析小体积管理请求体（4KB 上限、拒未知字段——typo 直接 400，
// 免得 PATCH 少个字母把"没改"当成"改好了"）。成功返回 true。
func decodeAdminJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求体 JSON 不合法: "+err.Error())
		return false
	}
	// 尾部残余校验（audit）：Decode 只消费首个 JSON 值，{"kind":"checkin"}garbage
	// 这类拼接体会被静默截断放行——拒绝之，防止半截请求被当成合法意图。
	if dec.More() {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求体 JSON 不合法: 尾部存在多余内容")
		return false
	}
	return true
}

func admin503(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"service": ServiceName,
		"error":   "scheduler not wired",
	})
}

// adminTasks 六类任务快照（含禁用项；next_fire 对禁用项为空串）。
func (h *Handler) adminTasks(w http.ResponseWriter, _ *http.Request) {
	if h.cfg.Sched == nil {
		admin503(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": ServiceName,
		"tasks":   h.cfg.Sched.SnapshotAll(),
	})
}

// adminTaskRun 手动触发（异步语义）：起 goroutine 跑 scheduler.RunKindNow，
// 202 立即返回——旅行/活跃全量遍历可达分钟级，不吊着 HTTP 连接，进度经
// GET /admin/tasks 的 running/last_run/last_result 观测。撞车 409（不落队）。
func (h *Handler) adminTaskRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind string `json:"kind"`
	}
	if !decodeAdminJSON(w, r, &req) {
		return
	}
	if h.cfg.Sched == nil {
		admin503(w)
		return
	}
	targets := scheduler.Kinds()
	if req.Kind != "all" {
		if _, ok := scheduler.KindFromName(req.Kind); !ok {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("kind=%q 非法（可选：%v 或 all）", req.Kind, targets))
			return
		}
		targets = []string{req.Kind}
	}

	st := h.adm
	st.mu.Lock()
	var started, busy []string
	for _, k := range targets {
		if st.manual[k] {
			busy = append(busy, k)
			continue
		}
		st.manual[k] = true
		started = append(started, k)
	}
	st.mu.Unlock()

	if len(started) == 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"service": ServiceName,
			"error":   "task_busy",
			"busy":    busy,
		})
		return
	}
	for _, k := range started {
		kind := k // 闭包捕获快照，防循环变量复用（Go 1.22+ 语义下冗余但无害）
		go func() {
			defer func() {
				st.mu.Lock()
				delete(st.manual, kind)
				st.mu.Unlock()
			}()
			log.Printf("admin: 手动触发 %s 开始", kind)
			if err := h.cfg.Sched.RunKindNow(kind); err != nil {
				// ErrBusy 只剩"定时器抢占窗口"一种可能（manual 标记已挡重发）：只记日志。
				if errors.Is(err, scheduler.ErrBusy) {
					log.Printf("admin: 手动触发 %s 撞车跳过（调度器执行中）: %v", kind, err)
					return
				}
				log.Printf("admin: 手动触发 %s 异常: %v", kind, err)
			}
		}()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"service": ServiceName,
		"started": started,
		"busy":    busy,
	})
}

// adminTaskPatch 热改排程开关/触发小时：内存生效（调度器即时重排）+ 写回 config.json。
// 带 hours 数组=改触发小时（SetHours 热生效免重启）；不带=改 enabled 开关（SetEnabled）。
// 写回失败不回滚内存改动，但响应里 persisted=false + note 说明，重启后会退回旧值——
// 插件 UI 据此提示"仅本次运行生效"。
func (h *Handler) adminTaskPatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind    string `json:"kind"`
		Enabled bool   `json:"enabled"`
		// Hours 触发小时表（0-23，可多个）：存在即走"改小时"分支。用指针区分
		//「未携带」与「携带空数组」——空数组语义=禁用（历史决定，config.Schedule），
		// 端点侧直接拒绝并指向 *_enabled 开关。
		Hours   *[]int `json:"hours"`
	}
	if !decodeAdminJSON(w, r, &req) {
		return
	}
	if h.cfg.Sched == nil {
		admin503(w)
		return
	}
	if _, ok := scheduler.KindFromName(req.Kind); !ok {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("kind=%q 非法（可选：%v）", req.Kind, scheduler.Kinds()))
		return
	}

	// —— 改触发小时 ——（TM 插件「应用」按钮与面板设置页走这里）
	if req.Hours != nil {
		if len(*req.Hours) == 0 {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("hours 不能为空数组（禁用任务请用 schedule.%s_enabled=false）", req.Kind))
			return
		}
		if !h.cfg.Sched.SetHours(req.Kind, *req.Hours) {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
				"hours 需为 0-23 的小时数字")
			return
		}
		resp := map[string]any{
			"service":   ServiceName,
			"kind":      req.Kind,
			"hours":     *req.Hours,
			"persisted": false,
		}
		path := h.cfg.Admin.ConfigPath
		if path == "" {
			resp["note"] = "ConfigPath 未配置，小时仅内存生效（重启后丢失）"
		} else {
			changed, err := patchConfigHours(path, req.Kind+"_hours", *req.Hours)
			switch {
			case err != nil:
				resp["note"] = "写回 config.json 失败，小时仅内存生效（重启后丢失）: " + err.Error()
				log.Printf("WARN: admin: 写回 config.json: %v", err)
			case !changed:
				resp["persisted"] = true
				resp["note"] = "config.json 已是目标值，未改动"
			default:
				resp["persisted"] = true
				log.Printf("admin: schedule.%s_hours=%v 已热生效并写回 %s（原文件备份 .bak）", req.Kind, *req.Hours, path)
			}
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// —— 改排程开关 ——（不带 hours 的旧形态，兼容既有插件/脚本）
	h.cfg.Sched.SetEnabled(req.Kind, req.Enabled)

	resp := map[string]any{
		"service":   ServiceName,
		"kind":      req.Kind,
		"enabled":   req.Enabled,
		"persisted": false,
	}
	path := h.cfg.Admin.ConfigPath
	if path == "" {
		resp["note"] = "ConfigPath 未配置，开关仅内存生效（重启后丢失）"
	} else {
		changed, err := patchConfigBool(path, "schedule", req.Kind+"_enabled", req.Enabled)
		switch {
		case err != nil:
			resp["note"] = "写回 config.json 失败，开关仅内存生效（重启后丢失）: " + err.Error()
			log.Printf("WARN: admin: 写回 config.json: %v", err)
		case !changed:
			resp["persisted"] = true
			resp["note"] = "config.json 已是目标值，未改动"
		default:
			resp["persisted"] = true
			log.Printf("admin: schedule.%s_enabled=%v 已热生效并写回 %s（原文件备份 .bak）", req.Kind, req.Enabled, path)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// adminCreditsGet 回放上次查询缓存 + 冷却截止时间（零上游成本）。
func (h *Handler) adminCreditsGet(w http.ResponseWriter, _ *http.Request) {
	st := h.adm
	st.mu.Lock()
	// interval 与下方运行态同锁读：PATCH /admin/credits-interval 可在别的
	// goroutine 热改它（字段挂在共享 cfg 上，锁外读即数据竞争）。
	interval := h.cfg.Admin.CreditRefreshMinInterval
	cache := st.creditCache
	var next int64
	if !st.creditLastStart.IsZero() {
		next = st.creditLastStart.Add(interval).Unix()
	}
	st.mu.Unlock()
	writeJSON(w, http.StatusOK, creditsView(cache, next))
}

// adminCreditsIntervalPatch 热改积分查询冷却：内存立即生效（含已在跑的冷却，
// 下次 GET/POST /admin/credits 即按新间隔算）+ 写回 config.json（重启后保持）。
// TrafficMonitor 插件保存"自动刷新周期"时调用——插件按服务端冷却节奏查询，
// 冷却不同步的话插件侧周期再小也会被 429 顶回，形同虚设。
func (h *Handler) adminCreditsIntervalPatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IntervalSec int64 `json:"interval_sec"`
	}
	if !decodeAdminJSON(w, r, &req) {
		return
	}
	// 风控兜底闸：下限 60s。再小就逼近上游风控阈值了，宁可直接拒绝也不放开。
	if req.IntervalSec < 60 || req.IntervalSec > 86400 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("interval_sec=%d 超出范围（60–86400 秒）", req.IntervalSec))
		return
	}
	// 内存热改纳入 adm.mu：GET/POST /admin/credits 在别的 goroutine 锁读该字段，
	// 裸写即数据竞争。锁一放新间隔即时生效（下次 GET/POST 即按新值算冷却）。
	st := h.adm
	st.mu.Lock()
	h.cfg.Admin.CreditRefreshMinInterval = time.Duration(req.IntervalSec) * time.Second
	st.mu.Unlock()

	resp := map[string]any{
		"service":      ServiceName,
		"interval_sec": req.IntervalSec,
		"persisted":    false,
	}
	path := h.cfg.Admin.ConfigPath
	if path == "" {
		resp["note"] = "ConfigPath 未配置，冷却仅内存生效（重启后丢失）"
	} else {
		changed, err := patchConfigInt(path, "admin", "credit_refresh_min_interval_sec", req.IntervalSec)
		switch {
		case err != nil:
			resp["note"] = "写回 config.json 失败，冷却仅内存生效（重启后丢失）: " + err.Error()
			log.Printf("WARN: admin: 写回 config.json: %v", err)
		case !changed:
			resp["persisted"] = true
			resp["note"] = "config.json 已是目标值，未改动"
		default:
			resp["persisted"] = true
			log.Printf("admin: admin.credit_refresh_min_interval_sec=%d 已热生效并写回 %s（原文件备份 .bak）",
				req.IntervalSec, path)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// creditsView 统一组装积分查询响应体：无缓存只回 cached=false + cooldown_until。
func creditsView(cache *creditReport, cooldownUntil int64) map[string]any {
	v := map[string]any{
		"service":        ServiceName,
		"cached":         cache != nil,
		"cooldown_until": cooldownUntil,
	}
	if cache != nil {
		v["credit_service"] = cache.Service
		v["ts"] = cache.Ts
		v["total"] = cache.Total
		v["accounts"] = cache.Accounts
	}
	return v
}

// adminCreditsRefresh 实时积分查询：逐号 ResourceSummary（与 cmd/credit 同口径、
// 同 200ms 账号间隔限速），成功的账号把权威余额回写 ledger（/status 立即变准）。
// 服务端两道保护：单飞（并发 429 running）+ 最小间隔冷却（429 + Retry-After）。
// 逐号串行全程感知 r.Context()：轮首检查 + 账号间限速等待均可被客户端断开打断，
// 提前收尾时响应带 aborted/completed/note（部分回执不覆盖 creditCache——其契约
// 是"上次成功查询的完整回执"）。单号 ResourceSummary 自身不感知 ctx（120s 硬
// 超时，client.go），断开后最多再等当前账号返回。
// 这是全链路唯一打上游余额接口的入口，频控以服务端为准——插件/UI 只是第一道装饰。
func (h *Handler) adminCreditsRefresh(w http.ResponseWriter, r *http.Request) {
	st := h.adm
	st.mu.Lock()
	// interval 同 adminCreditsGet：锁内读，防与热改 PATCH 构成数据竞争。
	interval := h.cfg.Admin.CreditRefreshMinInterval
	if st.creditRunning {
		st.mu.Unlock()
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"service": ServiceName, "error": "credit_refresh_running",
			"retry_after_sec": int(interval.Seconds()),
		})
		return
	}
	if !st.creditLastStart.IsZero() {
		if rem := time.Until(st.creditLastStart.Add(interval)); rem > 0 {
			st.mu.Unlock()
			w.Header().Set("Retry-After", strconv.Itoa(int(rem.Seconds())+1))
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"service": ServiceName, "error": "credit_refresh_cooldown",
				"retry_after_sec": int(rem.Seconds()) + 1,
				"cooldown_until":  st.creditLastStart.Add(interval).Unix(),
			})
			return
		}
	}
	st.creditRunning = true
	st.creditLastStart = time.Now()
	st.mu.Unlock()
	defer func() {
		st.mu.Lock()
		st.creditRunning = false
		st.mu.Unlock()
	}()

	statuses := h.cfg.Pool.List()
	rep := &creditReport{Service: "workbuddy", Ts: time.Now().Unix()}
	rep.Accounts = make([]creditAccount, 0, len(statuses))
	// aborted 客户端在串行刷新中途断开：不再对剩余账号发起上游查询。
	aborted := false
	for i, s := range statuses {
		if r.Context().Err() != nil {
			aborted = true
			break
		}
		ca := creditAccount{UID: s.UID, Nickname: s.Nickname}
		a := h.cfg.Pool.AuthByUID(s.UID)
		if a == nil || a.AccessTokenValue() == "" {
			ca.Error = "no credentials"
		} else {
			remain, used, size, packs, err := h.cfg.Upstream.ResourceSummary(a)
			if err != nil {
				ca.Error = err.Error()
			} else {
				ca.Remain, ca.Used, ca.Size = &remain, &used, &size
				ca.Packages, ca.OK = packs, true
				// 权威余额回写：估算账本校正 + 有余额即解冻（与 CheckinAll 收尾同两板斧）。
				h.cfg.Pool.ReenableIfCredits(s.UID, remain)
				h.cfg.Pool.SetCredits(s.UID, remain)
			}
		}
		rep.Accounts = append(rep.Accounts, ca)
		if ca.OK {
			rep.Total.OK++
			if ca.Remain != nil {
				rep.Total.Remain += *ca.Remain
				rep.Total.Used += *ca.Used
				rep.Total.Size += *ca.Size
			}
		}
		if i < len(statuses)-1 {
			// 与 cmd/credit collect 同口径限速；等待期间客户端断开则提前收尾，
			// 不再白等 N×200ms（更不再进入下一号可达分钟级的上游查询）。
			select {
			case <-time.After(200 * time.Millisecond):
			case <-r.Context().Done():
				aborted = true
			}
			if aborted {
				break
			}
		}
	}
	rep.Total.Accounts = len(rep.Accounts)
	rep.Total.Failed = rep.Total.Accounts - rep.Total.OK
	if aborted {
		log.Printf("admin: 积分实时查询中止（客户端断开）：已完成 %d/%d，ok=%d",
			rep.Total.Accounts, len(statuses), rep.Total.OK)
	} else {
		log.Printf("admin: 积分实时查询完成 ok=%d/%d（remain=%d）", rep.Total.OK, rep.Total.Accounts, rep.Total.Remain)
	}

	st.mu.Lock()
	if !aborted {
		st.creditCache = rep // 部分回执不入缓存：creditCache 契约是"上次完整回执"
	}
	next := st.creditLastStart.Add(interval).Unix()
	st.mu.Unlock()

	resp := creditsView(rep, next)
	if aborted {
		resp["aborted"] = true
		resp["completed"] = rep.Total.Accounts
		resp["total_accounts"] = len(statuses)
		resp["note"] = "客户端断开，提前终止"
	}
	writeJSON(w, http.StatusOK, resp)
}

// adminShutdown 优雅停机：先回 200（插件要拿到成功回执再进入"等待端口释放"轮询），
// 300ms 后触发 main 注入的 ctx cancel，走既有关机路径：p.Flush 落盘 → store.Close
// → srv.Shutdown → ListenAndServe 正常返回。OnShutdown 未接线时退化为 os.Exit(0)。
func (h *Handler) adminShutdown(w http.ResponseWriter, _ *http.Request) {
	log.Printf("admin: 收到优雅停机请求（POST /admin/shutdown）")
	writeJSON(w, http.StatusOK, map[string]any{"service": ServiceName, "ok": true, "message": "shutting down"})
	cb := h.cfg.OnShutdown
	go func() {
		time.Sleep(300 * time.Millisecond)
		if cb != nil {
			cb()
		} else {
			os.Exit(0)
		}
	}()
}

// ============================================================================
// config.json 最小 diff 标量补丁（PATCH /admin/tasks、PATCH /admin/credits-interval 落盘用）
// ============================================================================

// patchConfigBool 把配置文件里二级对象 section.key 的布尔值原子替换（或插入）。
func patchConfigBool(path, section, key string, val bool) (bool, error) {
	lit := []byte("false")
	if val {
		lit = []byte("true")
	}
	return patchConfigScalar(path, section, key, lit)
}

// patchConfigInt 同 patchConfigBool，但写整数标量（splice 对任意标量字面量通用）。
func patchConfigInt(path, section, key string, val int64) (bool, error) {
	return patchConfigScalar(path, section, key, []byte(strconv.FormatInt(val, 10)))
}

// patchConfigHours 把配置文件里 schedule.<key> 的小时数组原子替换为 hours。
// 整体替换、元素原样（元素数 0 由调用方拒绝）；用 json.Marshal 生成字面量后走
// 与标量补丁同一条 splice 通路——splice 的 value token 跨度替换对数组同样适用。
func patchConfigHours(path, key string, hours []int) (bool, error) {
	lit, err := json.Marshal(hours)
	if err != nil {
		return false, err
	}
	return patchConfigScalar(path, "schedule", key, lit)
}

// patchConfigMu 串行化 config.json 的读-改-写临界区。PATCH /admin/tasks 与
// PATCH /admin/credits-interval 都经 patchConfigScalar 落盘：无互斥时并发 PATCH
// 各自基于旧字节做 splice，后落盘者覆盖先落盘者的改动（补丁丢失）而两个响应仍都
// 报 persisted=true。adminState.mu 只管运行态字段，覆盖不到落盘路径，故设包级锁。
//
// 跨包共享（C-config 双写路径修复）：面板 saveConfig 的整文件重写（cmd/server
// panel_config.go）也经 ConfigMu() 拿同一把锁——admin splice 与面板整写并发时
// 互相丢补丁（admin 基于旧字节的 splice 落盘可被面板基于旧字节的整文件 rename
// 回滚，或反向），两条写路径收敛到同一临界区后互斥。
var patchConfigMu sync.Mutex

// ConfigMu 返回 config.json 写路径的共享互斥（cmd/server 的面板 saveConfig 用）。
// 调用方必须在完整读-改-写跨度内持锁（ReadFile → merge/splice → tmp+rename），
// 与 patchConfigScalar 的临界区口径一致。
func ConfigMu() *sync.Mutex {
	return &patchConfigMu
}

// patchConfigScalar 把配置文件里二级对象 section.key 的标量值原子替换（或插入），
// 其余字节原样保留——对用户的 config.json 是影响最小化：只有目标那一处变。
//
// 规则：
//   - 文件不是合法 JSON / 顶层不是对象 → 报错并拒绝写回（config.json 坏了服务下次
//     重启也起不来，不能在这里把现场覆盖掉）；
//   - key 存在 → 只替换其 value token 跨度；原值与目标相等时 changed=false 零写入；
//   - section 存在而 key 缺失 → 在 section 开括号后插入（缩进跟随现有第一个成员；
//     空对象则不带逗号）；
//   - section 整体缺失 → 在顶层末尾新增整个 section 对象；
//   - 写盘前新字节必须过 json.Valid；先备份原文件到 path+".bak"，再 tmp+rename
//     原子替换（与 state.json/auths 落盘同风格）。
//
// 返回 changed：文件字节是否发生改动（等值提交时 false）。
func patchConfigScalar(path, section, key string, lit []byte) (bool, error) {
	// 整个读-改-写持包级锁（含备份与 tmp+rename 落盘）：临界区必须覆盖 ReadFile
	// 到 Rename 的完整跨度，否则并发 PATCH 仍是"后写者基于旧字节"。
	patchConfigMu.Lock()
	defer patchConfigMu.Unlock()
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read config: %w", err)
	}
	newRaw, changed, err := spliceBool(raw, section, key, lit)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	if !json.Valid(newRaw) {
		return false, errors.New("internal error: 补丁后的 JSON 非法，已放弃写入")
	}
	if err := os.WriteFile(path+".bak", raw, 0o600); err != nil {
		return false, fmt.Errorf("backup: %w", err)
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return false, fmt.Errorf("open tmp: %w", err)
	}
	// 权限位对齐原文件（audit）：rename 会整体替换目录项，tmp 的 0o600 会覆盖掉
	// 原config.json 的权限位（如 0o644）——按原文件 mode 补一次 chmod。
	if info, statErr := os.Stat(path); statErr == nil {
		if chmodErr := os.Chmod(tmp, info.Mode().Perm()); chmodErr != nil {
			f.Close()
			os.Remove(tmp)
			return false, fmt.Errorf("chmod tmp: %w", chmodErr)
		}
	}
	if _, err := f.Write(newRaw); err != nil {
		f.Close()
		os.Remove(tmp)
		return false, fmt.Errorf("write tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return false, fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return false, fmt.Errorf("rename: %w", err)
	}
	return true, nil
}

// spliceBool 用 json.Decoder token 流定位 section.key 的值并做跨度替换/插入
// （名字沿用旧称）。只依赖 Token()+InputOffset()，不重建对象——键序、未知字段、
// 数字/字符串原文全部原样；lit 是目标值的 JSON 字面量（true/false/数字/字符串，
// 或小时补丁的数组字面量 [9,21]——数组按"从值首字节到闭括号"的整段跨度替换）。
func spliceBool(raw []byte, section, key string, lit []byte) ([]byte, bool, error) {
	if !json.Valid(raw) {
		return nil, false, fmt.Errorf("config 不是合法 JSON，拒绝写回")
	}
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false, fmt.Errorf("config 顶层不是 JSON 对象，拒绝写回")
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	type frame struct {
		obj     bool
		key     string // 本 frame（作为父对象成员时）的键；顶层 frame 为空
		wantKey bool
	}
	var stack []frame
	var topOpen, secOpen int64 = -1, -1 // 顶层 / section 对象 '{' 之后的偏移
	pending := false                    // 已读到目标成员名，正在等它的值
	pendingPre := int64(0)
	// 数组值跨度模式（hours 补丁）：目标 key 的值以 '[' 开头时，跨 token 消费到
	// 匹配闭括号，再对 [vStart, off) 整段替换——标量路径"下一个标量 token 即值"
	// 的假设对数组不成立，数组内部元素只消费不处理。
	pendingArr := false
	arrDepth := 0
	arrVStart := int64(0)
	skipWS := func(i, off int64) int64 {
		for i < off && (raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\r' || raw[i] == '\n') {
			i++
		}
		return i
	}
	// valueStart 从成员名 token 之后定位值的起始字节：越过 空白 → `:` → 空白。
	// Decoder 不为键值间的 `:` 出 token，标量/数组两条路径共用。
	valueStart := func(off int64) (int64, error) {
		v := skipWS(pendingPre, off)
		if v >= off || raw[v] != ':' {
			return 0, fmt.Errorf("config 结构异常：找不到 %s.%s 的值分隔符，拒绝写回", section, key)
		}
		return skipWS(v+1, off), nil
	}

	for {
		t, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false, fmt.Errorf("扫描 config 失败: %w", err)
		}
		if d, ok := t.(json.Delim); ok {
			off := dec.InputOffset()
			if pendingArr {
				switch d {
				case '[', '{':
					arrDepth++
				case ']', '}':
					arrDepth--
					if arrDepth == 0 {
						if bytes.Equal(raw[arrVStart:off], lit) {
							return raw, false, nil // 等值：零改动
						}
						out := make([]byte, 0, len(raw)+len(lit)+8)
						out = append(out, raw[:arrVStart]...)
						out = append(out, lit...)
						out = append(out, raw[off:]...)
						return out, true, nil
					}
				}
				continue
			}
			if pending {
				// 目标 key 的值以定界符开头：数组=整段跨度替换（hours 补丁），对象仍拒写。
				if d == '[' {
					v, verr := valueStart(off)
					if verr != nil {
						return nil, false, verr
					}
					pendingArr, arrDepth, arrVStart = true, 1, v
					continue
				}
				return nil, false, fmt.Errorf("config 里 %s.%s 的值不是标量，拒绝写回", section, key)
			}
			switch d {
			case '{', '[':
				stack = append(stack, frame{obj: d == '{', wantKey: d == '{'})
				if len(stack) == 1 && d == '{' {
					topOpen = off
				} else if len(stack) == 2 && d == '{' && secOpen < 0 && stack[0].key == section {
					secOpen = off
				}
			case '}', ']':
				stack = stack[:len(stack)-1]
				if len(stack) > 0 && stack[len(stack)-1].obj {
					stack[len(stack)-1].wantKey = true
				}
			}
			continue
		}
		// 标量 token
		if len(stack) == 0 {
			return nil, false, fmt.Errorf("config 顶层不是 JSON 对象，拒绝写回")
		}
		f := &stack[len(stack)-1]
		off := dec.InputOffset()
		if f.wantKey {
			s, _ := t.(string) // 对象成员名必为字符串
			f.key = s
			f.wantKey = false
			if len(stack) == 2 && stack[0].key == section && s == key {
				pending, pendingPre = true, off
			}
			continue
		}
		if pendingArr {
			continue // 数组值内部元素：只消费（跨度替换在闭括号处整体完成）
		}
		if pending {
			// 值跨度：[vStart, off)。
			vStart, verr := valueStart(off)
			if verr != nil {
				return nil, false, verr
			}
			if bytes.Equal(raw[vStart:off], lit) {
				return raw, false, nil // 等值：零改动
			}
			out := make([]byte, 0, len(raw)+8)
			out = append(out, raw[:vStart]...)
			out = append(out, lit...)
			out = append(out, raw[off:]...)
			return out, true, nil
		}
		f.wantKey = f.obj
	}

	// —— 未命中值：走插入路径 ——
	ins := []byte("\"" + key + "\": " + string(lit))
	if secOpen >= 0 { // section 存在，key 缺失
		indent := memberIndentAfter(raw, secOpen)
		needComma := firstNonSpaceByte(raw, secOpen) != '}'
		var add []byte
		add = append(add, '\n')
		add = append(add, indent...)
		add = append(add, ins...)
		if needComma {
			add = append(add, ',')
		}
		out := make([]byte, 0, len(raw)+len(add))
		out = append(out, raw[:secOpen]...)
		out = append(out, add...)
		out = append(out, raw[secOpen:]...)
		return out, true, nil
	}
	// section 整体缺失：在顶层对象末尾新增（成员缩进 4 空格、闭合缩进 2，与主流
	// config.json 风格对齐；这是"新文件从未有 schedule 段"的少见路径，只求可读不求精美）。
	secBody := []byte("\"" + section + "\": {" + "\n    " + string(ins) + "\n  }")
	last := len(raw)
	for last > 0 && (raw[last-1] == ' ' || raw[last-1] == '\t' || raw[last-1] == '\r' || raw[last-1] == '\n') {
		last--
	}
	if last == 0 || raw[last-1] != '}' || topOpen < 0 {
		return nil, false, fmt.Errorf("config 结构不符合预期（找不到顶层对象收尾），拒绝写回")
	}
	emptyTop := firstNonSpaceByte(raw, topOpen) == '}'
	var add []byte
	if emptyTop {
		add = append([]byte("\n  "), secBody...)
		add = append(add, '\n')
		insertAt := topOpen
		out := make([]byte, 0, len(raw)+len(add))
		out = append(out, raw[:insertAt]...)
		out = append(out, add...)
		out = append(out, raw[last-1:]...) // 保留原顶层收尾 '}'（及其后空白）
		return out, true, nil
	}
	// 非空顶层：逗号贴在最后一个成员值之后（越过其后的空白），保持惯用排版。
	cut := last - 1
	for cut > 0 && (raw[cut-1] == ' ' || raw[cut-1] == '\t' || raw[cut-1] == '\r' || raw[cut-1] == '\n') {
		cut--
	}
	out := make([]byte, 0, len(raw)+len(secBody)+8)
	out = append(out, raw[:cut]...)
	out = append(out, ",\n  "...)
	out = append(out, secBody...)
	out = append(out, raw[cut:]...) // 原有的收尾空白 + '}'
	return out, true, nil
}

// firstNonSpaceByte 从 off 起第一个非空白字节的值（越界返回 0）。
func firstNonSpaceByte(raw []byte, off int64) byte {
	for int64(len(raw)) > off {
		if c := raw[off]; c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			return c
		}
		off++
	}
	return 0
}

// memberIndentAfter 对象开括号后第一个成员行的缩进前缀（含起始换行后的空白）。
// 找不到成员行（空对象）或无换行缩进时回落两个空格。
func memberIndentAfter(raw []byte, openOff int64) []byte {
	i := openOff
	for i < int64(len(raw)) && (raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\r' || raw[i] == '\n') {
		i++
	}
	if i >= int64(len(raw)) || raw[i] != '"' {
		return []byte("  ")
	}
	lineStart := i
	for lineStart > 0 && raw[lineStart-1] != '\n' {
		lineStart--
	}
	// lineStart 没退过本对象的行首 = 成员与开括号同行（单行对象如 {"a":1}，
	// 行首在 openOff 之前）：无真缩进可用，照抄前缀会把 `{"schedule":{` 这类
	// 内容当缩进插入、产出非法 JSON。改用自带换行的两空格缩进。
	if lineStart <= openOff {
		return []byte("\n  ")
	}
	ind := raw[lineStart:i]
	if len(ind) == 0 {
		return []byte("  ")
	}
	return ind
}

// ---- 上游运维端点（issue #138 / #118）：账号临时停用 / 恢复 / 复活 ----
//
// 与上方本地 /admin 管理面共用 config admin.enabled 开关与路由注册（见 handler.go
// NewHandler），但鉴权面不同：accounts 端点与 /status 同源（仅 withAuth + 同一个
// api_key，不设 loopback 闸、不另立管理密钥）——面板可能跑在局域网其他机器上。
//
// 设计要点（照录上游）：
//   - 手动停用是**独立状态位** manual_disabled，与自动禁用 disabled 并列、互不影响。
//     复用同一字段会让运维意图被签到解冻、refresh 成功等自动复活路径意外解除。
//   - 语义是「对话流量摘除」而非「账号冻结」：不碰冷却/熔断维度，签到与保活照常，
//     凭证和积分都是活的；恢复时拿到的是停用期间真实发生的状态。
//   - 幂等：面板重试不会报错；重复调用只更新原因文案。

// adminAccountState 账号运维端点的统一响应体：回显操作后的双位状态，面板据此直接
// 更新 UI，不必再打一次 /status。（上游原命名 adminState 与本地积分管理面的运行态
// 结构体重名，合并时改为 adminAccountState。）
type adminAccountState struct {
	UID            string `json:"uid"`
	ManualDisabled bool   `json:"manual_disabled"`
	ManualReason   string `json:"manual_reason,omitempty"`
	// Disabled 保留在响应里让面板能区分「手动摘除」与「系统判定坏了」——
	// 恢复按钮的语义对两者不同（enable 解手动位，revive 解自动位）。
	Disabled bool `json:"disabled"`
	Changed  bool `json:"changed"`
}

// adminUID 提取并校验路径段 uid。返回 false 表示已写出响应（uid 为空 → 400），
// 调用方应直接 return。
// 开关判断不在这里：路由按 cfg.Admin.Enabled 条件注册（见 NewHandler），
// 未开启时这些 handler 根本不可达——handler 内再判开关是多余的存在性泄露面。
func adminUID(w http.ResponseWriter, r *http.Request) (string, bool) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "uid is required")
		return "", false
	}
	return uid, true
}

// adminReasonFromBody 读可选 JSON 体里的 reason 字段。
// 空体/非 JSON/无该字段都返回空串（端点不因体格式拒绝——无体是最常见调用形态）。
func adminReasonFromBody(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	// 限制读取量：reason 是短文本，避免畸形大请求占用内存。
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil || len(raw) == 0 {
		return ""
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return ""
	}
	return strings.TrimSpace(body.Reason)
}

// adminAccountDisable 手动停用：把账号摘出选号池，但保留在池里
// （状态/冷却/成本台账继续归它管，签到与保活照常）。
func (h *Handler) adminAccountDisable(w http.ResponseWriter, r *http.Request) {
	uid, ok := adminUID(w, r)
	if !ok {
		return
	}
	reason := adminReasonFromBody(r)
	if reason == "" {
		reason = "manual"
	}
	found, changed := h.cfg.Pool.SetManualDisabled(uid, true, reason)
	if !found {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
		return
	}
	stopped, stopReason, _ := h.cfg.Pool.ManualDisabledState(uid)
	writeJSON(w, http.StatusOK, adminAccountState{
		UID: uid, ManualDisabled: stopped, ManualReason: stopReason,
		Disabled: h.accountAutoDisabled(uid), Changed: changed,
	})
}

// adminAccountEnable 解除手动停用。若账号仍被系统自动禁用（disabled），它**不会**
// 因此回到选号池——那需要 revive。响应里的 disabled 字段就是给面板看的提示。
func (h *Handler) adminAccountEnable(w http.ResponseWriter, r *http.Request) {
	uid, ok := adminUID(w, r)
	if !ok {
		return
	}
	found, changed := h.cfg.Pool.SetManualDisabled(uid, false, "")
	if !found {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
		return
	}
	stopped, reason, _ := h.cfg.Pool.ManualDisabledState(uid)
	writeJSON(w, http.StatusOK, adminAccountState{
		UID: uid, ManualDisabled: stopped, ManualReason: reason,
		Disabled: h.accountAutoDisabled(uid), Changed: changed,
	})
}

// adminAccountRevive 解除系统自动禁用（清 disabled + reason + 连续 12153 计数）。
// 不碰手动停用位：运维明确摘除的号不应被一次 revive 悄悄放回选号池。
func (h *Handler) adminAccountRevive(w http.ResponseWriter, r *http.Request) {
	uid, ok := adminUID(w, r)
	if !ok {
		return
	}
	if _, _, found := h.cfg.Pool.ManualDisabledState(uid); !found {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
		return
	}
	changed := h.cfg.Pool.ReviveDisabled(uid)
	stopped, reason, _ := h.cfg.Pool.ManualDisabledState(uid)
	writeJSON(w, http.StatusOK, adminAccountState{
		UID: uid, ManualDisabled: stopped, ManualReason: reason,
		Disabled: h.accountAutoDisabled(uid), Changed: changed,
	})
}

// accountAutoDisabled 读某账号当前的自动禁用位（供响应回显）。
// 复用 Pool.List 的单账号查询：轮询全部账号在小池下开销可忽略，
// 且避免为此在 pool 上再开一个只读访问器（保持接口面最小）。
func (h *Handler) accountAutoDisabled(uid string) bool {
	for _, st := range h.cfg.Pool.List() {
		if st.UID == uid {
			return st.Disabled
		}
	}
	return false
}
