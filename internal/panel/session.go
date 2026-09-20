// session.go manager 壳登录契约（v1.2.0）：无状态 HMAC 签名会话 cookie。
//
// 设计（设计文档 §4.2 / §8）：
//   - 密码 = api_key（单凭证，不引入用户体系；username 任意填，服务端忽略）；
//   - cookie 值 = base64url(payload) + "." + hex(HMAC-SHA256(payload, api_key))，
//     payload = {"exp":unix秒}。无服务端会话表，重启不掉线；
//   - TTL 30 天；api_key 变更（livecfg 热改）后旧签名全部失效——密钥即签名密钥；
//   - HttpOnly + SameSite=Lax；Path=/ 覆盖根下全部页面与 API；
//   - 校验用常量时间比较（hmac.Equal / subtle），与 httpauth 同纪律。
//
// 双通道：sessionAuth 先验 cookie，再验 Bearer（httpauth.VerifyBearer），
// 二者任一通过即放行（面板 API 对脚本/CLI 与浏览器同开）。
package panel

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"workbuddy2api/internal/httpauth"
	"workbuddy2api/internal/server"
)

// sessionCookie 会话 cookie 名（httpauth 共享常量：panel 签发、server 校验同一名）。
const sessionCookie = httpauth.SessionCookieName

// sessionTTL 会话有效期（30 天）。
const sessionTTL = 30 * 24 * time.Hour

// loginBodyLimit 登录/映射请求体上限（防大 body 拖内存；表单极小）。
const loginBodyLimit = 1 << 16

// handleLogin POST /api/login {username,password}。
// username 忽略（单凭证语义，manager 登录页保留输入框只是形态）；
// password 与 api_key 常量时间比较，成功换发签名 cookie，失败 401。
func (p *Panel) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, loginBodyLimit)).Decode(&body); err != nil && r.ContentLength != 0 {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	key := p.apiKey()
	if key == "" {
		// 未启用鉴权的部署没有"登录"概念：直接发一个长效会话，manager 壳可正常进入。
		p.issueSession(w, "")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": "admin", "role": "admin"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Password), []byte(key)) != 1 {
		writeErr(w, http.StatusUnauthorized, "invalid_password")
		return
	}
	p.issueSession(w, key)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": "admin", "role": "admin"})
}

// issueSession 签发会话 cookie（payload 以当前时刻 + TTL 计算 exp）。
// key 为空时不发（无鉴权部署无需会话）。签名实现见 httpauth.SignSessionToken
// （server 侧 withAuth 用同一函数校验，口径只有一份）。
func (p *Panel) issueSession(w http.ResponseWriter, key string) {
	if key == "" {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    httpauth.SignSessionToken(mustJSON(map[string]int64{"exp": time.Now().Add(sessionTTL).Unix()}), key),
		Path:     "/",
		Expires:  time.Now().Add(sessionTTL),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// mustJSON 编码失败回 "{}"（仅 int64 map，不可能失败；防御性兜底）。
func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return raw
}

// sessionAuth 会话通道判定：cookie 有效（或未启用鉴权）返回 true。
// 密钥经 livecfg 快照读取——改 api_key 后旧 cookie 全部失效（签名密钥变更）。
func (p *Panel) sessionAuth(r *http.Request) bool {
	return httpauth.VerifySessionRequest(r, p.apiKey())
}

// handleMe GET /api/me：manager 壳登录态探测。
// 走到此处说明双通道鉴权已通过（withAuth）；响应固定 admin 角色。
func (p *Panel) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"username": "admin", "role": "admin"})
}

// handleLogout POST /api/logout：清会话 cookie（Max-Age<0 即时过期）。
func (p *Panel) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// manager 设置页：模型映射（server.ModelMapView / SetModelMap + saveConfig 写回）
// ---------------------------------------------------------------------------

// handleGetModelMap GET /api/settings/model-map → server.ModelMapView()（生效表副本）。
func (p *Panel) handleGetModelMap(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "map": server.ModelMapView()})
}

// handleSetModelMap POST /api/settings/model-map body {"map":{...}}：
// SetModelMap 立即生效 → SaveConfig 闭包写回 config.json（深合并原子写，
// 键 model_map）→ 返回生效表。写盘失败仍返回生效表（内存已生效；
// 重启后回落 config 值），错误随 ok:false 提示。
func (p *Panel) handleSetModelMap(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Map map[string]string `json:"map"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, loginBodyLimit)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	server.SetModelMap(body.Map)
	view := server.ModelMapView()
	if p.cfg.SaveConfig != nil {
		raw := mustJSON(map[string]any{"model_map": view})
		if _, err := p.cfg.SaveConfig(raw); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "map": view})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "map": view})
}
