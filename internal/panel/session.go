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
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
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

// 登录失败速率限制：/api/login 是面板唯一免鉴权入口，无限制会被在线穷举 api_key
// （成功后等于拿到 /v1/* + 凭据导出全量能力）。按来源 IP 计数，达到阈值后短暂锁定。
const (
	loginFailWindow  = 10 * time.Minute // 计数窗口
	loginFailMax     = 5                // 窗口内允许失败次数
	loginLockoutTime = 15 * time.Minute // 锁定时长
)

// loginLimiter 按来源 IP 的失败计数 + 锁定时限。map 仅在 handleLogin 路径访问，
// 由独立 mutex 保护；条目惰性过期（命中即检查时间戳），不另起清理 goroutine。
type loginLimiter struct {
	mu      sync.Mutex
	entries map[string]*loginFailEntry
}
type loginFailEntry struct {
	fails       int
	windowStart time.Time
	lockedUntil time.Time
}

var loginRateLimit = &loginLimiter{entries: map[string]*loginFailEntry{}}

// allow 判定该来源当前是否可继续尝试。true=允许，false=已锁定。
func (l *loginLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[ip]
	if !ok {
		return true
	}
	if !e.lockedUntil.IsZero() {
		if time.Now().Before(e.lockedUntil) {
			return false
		}
		// 锁定到期：清掉重来。
		delete(l.entries, ip)
		return true
	}
	return true
}

// recordFailure 记一次失败，达到阈值即锁定。返回当前是否已被锁。
func (l *loginLimiter) recordFailure(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	e, ok := l.entries[ip]
	if !ok || now.Sub(e.windowStart) > loginFailWindow {
		e = &loginFailEntry{windowStart: now}
		l.entries[ip] = e
	}
	e.fails++
	if e.fails >= loginFailMax {
		e.lockedUntil = now.Add(loginLockoutTime)
		return true
	}
	return false
}

// reset 登录成功后清零该来源的失败计数。
func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, ip)
}

// clientIP 提取请求来源 IP（RemoteAddr 去端口；面板在反代后时应由部署方保证
// RemoteAddr 已是真实客户端，或另行注入 X-Forwarded-For 白名单——本项目直连形态为主）。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// handleLogin POST /api/login {username,password}。
// username 忽略（单凭证语义，manager 登录页保留输入框只是形态）；
// password 与 api_key 经 SHA-256 摘要后常量时间比较（与 httpauth.VerifyBearer
// 同口径，不泄露长度），成功换发签名 cookie，失败 401。
func (p *Panel) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !loginRateLimit.allow(ip) {
		writeErr(w, http.StatusTooManyRequests, "too_many_attempts")
		return
	}
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
	// 口令比较与 httpauth.VerifyBearer 同口径：先 SHA-256 摘要再常量时间比较。
	// 直接对原文 ConstantTimeCompare 会在长度不等时立即返回（泄露长度信息）；
	// 摘要把任意长度吸收进定长 32 字节，比较耗时与输入长度、相等与否都无关。
	if subtle.ConstantTimeCompare(sha256Sum(body.Password), sha256Sum(key)) != 1 {
		if loginRateLimit.recordFailure(ip) {
			log.Printf("panel: 登录失败 %d 次，来源 %s 锁定 %v", loginFailMax, ip, loginLockoutTime)
		}
		writeErr(w, http.StatusUnauthorized, "invalid_password")
		return
	}
	loginRateLimit.reset(ip)
	p.issueSession(w, key)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": "admin", "role": "admin"})
}

// sha256Sum 返回 s 的 SHA-256 摘要（定长 32 字节，供常量时间比较）。
// httpauth.digest 未导出（该文件不在本次修改范围），此处按同法实现，
// 语义与 VerifyBearer 的比较口径一致。
func sha256Sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
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
