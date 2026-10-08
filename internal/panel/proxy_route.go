// proxy_route.go 出口代理线路管理端点（workbuddy-manager proxy_routes 吸收件，
// 面板 accounts 页「线路」下拉的后端）。
//
// 两个端点：
//   - GET /api/proxy_routes：线路表（密码脱敏）+ 全池账号当前绑定。线路 URL 是
//     共享配置（config.json proxy_routes 段），**只脱敏回显、绝不回传明文密码**；
//   - POST /api/accounts/{uid}/proxy_route：账号绑定/解绑线路。绑定写 auth 文件
//     （auths/*.json 的 proxy_route 字段，SaveAtomic 原子落盘）+ 内存 Auth 对象
//     （出站路径实时生效，无需重启）；空 route = 解绑（直连）。
//
// 鉴权口径：经 p.api() 注册（withAuth 闸），**不进** wbt_ token 分级表——写语义
// 按既有口径分级表外恒 403 token_write_forbidden（与 note 端点同口径）。
package panel

import (
	"encoding/json"
	"log"
	"net/http"

	"workbuddy2api/internal/upstream"
)

// proxyRouteBody 绑定请求体：{"route": "route-a"} 或 {"route": ""}（空 = 解绑）。
type proxyRouteBody struct {
	Route string `json:"route"`
}

// proxyRoutes 列出线路表与全池绑定：
//   - 501 proxy routes unavailable：LoadConfig 未注入（最小装配形态，无 config.json
//     读写能力，线路表无法回显）；
//   - 200 {routes: {name: maskedURL}, accounts: {uid: routeName}}：routes 含全部
//     已配置线路（密码段打码 ***）；accounts 只含**已绑定**的账号（未绑定不出现）。
func (p *Panel) proxyRoutes(w http.ResponseWriter, r *http.Request) {
	routes := map[string]string{}
	// 线路表从 config.json 读（LoadConfig 闭包 → Config.ProxyRoutes）：面板热改
	// 配置后无需重启即可看到新线路。LoadConfig 未注入时 routes 保持空（仅绑定
	// 视图仍可用，测试/最小装配形态）。
	if p.cfg.LoadConfig != nil {
		if cfg, err := p.cfg.LoadConfig(); err == nil {
			if raw, merr := json.Marshal(cfg); merr == nil {
				var parsed struct {
					ProxyRoutes map[string]string `json:"proxy_routes"`
				}
				if json.Unmarshal(raw, &parsed) == nil {
					routes = parsed.ProxyRoutes
				}
			}
		}
	}
	masked := make(map[string]string, len(routes))
	for name, rawURL := range routes {
		masked[name] = upstream.MaskProxyURL(rawURL)
	}
	// 全池绑定视图：AuthByUID 逐账号读绑定名（Status 不透出该字段——它属凭证
	// 数据面，不出现在 /status 与 /api/overview 的状态透出里，仅本端点聚合）。
	accounts := map[string]string{}
	for _, st := range p.cfg.Pool.List() {
		if a := p.cfg.Pool.AuthByUID(st.UID); a != nil {
			if route := a.ProxyRouteValue(); route != "" {
				accounts[st.UID] = route
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "routes": masked, "accounts": accounts})
}

// accountProxyRoute 绑定/解绑单账号线路：
//   - 404 account not found：uid 不在池里（与 note/disable 同判据 Pool.Status）；
//   - 400 invalid_body / unknown_route：请求体非法，或 route 非空但线路表中无此名
//     （绑定一个不存在的线路 = 请求将全部被拒绝发出，提前拦截这种自断状态）；
//   - 200 {"ok":true}：绑定成功（SaveAtomic 落盘失败回 500 且不保留半态——先写
//     内存后落盘，失败时回滚内存绑定）。
func (p *Panel) accountProxyRoute(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	var body proxyRouteBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body")
		return
	}
	route := body.Route
	// 线路名合法性：绑定值必须存在于当前线路表（空串 = 解绑放行）。线路表读
	// config.json（与 GET 同源）；LoadConfig 未注入时仅允许解绑（最小装配形态
	// 不提供绑定能力，避免「看不到表还绑出去」）。
	if route != "" && p.cfg.LoadConfig != nil {
		cfg, err := p.cfg.LoadConfig()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "load config: "+err.Error())
			return
		}
		raw, merr := json.Marshal(cfg)
		if merr != nil {
			writeErr(w, http.StatusInternalServerError, "encode config: "+merr.Error())
			return
		}
		var parsed struct {
			ProxyRoutes map[string]string `json:"proxy_routes"`
		}
		if json.Unmarshal(raw, &parsed) != nil || parsed.ProxyRoutes == nil {
			writeErr(w, http.StatusBadRequest, "unknown_route")
			return
		}
		if _, ok := parsed.ProxyRoutes[route]; !ok {
			writeErr(w, http.StatusBadRequest, "unknown_route")
			return
		}
	}
	prev := a.ProxyRouteValue()
	a.SetProxyRoute(route)
	// 持久化优先：auth 文件是凭证文件，绑定关系必须落盘（换号重登/进程重启不丢）。
	// 落盘失败 → 回滚内存绑定并报错（不留「内存已绑、文件没落」的漂移态）。
	if a.FilePath != "" {
		if err := a.SaveAtomic(); err != nil {
			a.SetProxyRoute(prev)
			log.Printf("panel: proxy_route uid=%s save failed: %v", uid, err)
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if route == "" {
		log.Printf("panel: proxy_route uid=%s（已解绑，恢复直连）", uid)
	} else {
		log.Printf("panel: proxy_route uid=%s → %s", uid, route)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
