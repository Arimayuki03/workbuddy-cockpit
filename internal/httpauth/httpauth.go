// Package httpauth 网关与面板共用的 Bearer 鉴权原语。
//
// 单独成包的原因：server（/v1/*、/status）与 panel（/panel/api/*）两处鉴权
// 必须完全同口径——此前各自复制了一份"字符串直接比较"的实现，既容易漂移，
// 又都带计时侧信道。统一到这里后，口径只有一份，且天然常量时间比较。
package httpauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// bearerPrefix 认证方案前缀（大小写敏感，与 HTTP 规范及既有实现一致）。
const bearerPrefix = "Bearer "

// VerifyBearer 校验请求头是否携带正确的 Bearer 密钥。
//
// key 为空表示"未启用鉴权"，恒返回 true（调用方据此放行）。
// 比较用 SHA-256 摘要 + subtle.ConstantTimeCompare：
//   - 常量时间，不因前缀匹配长度而泄露信息；
//   - 先摘要再比较，长度差异被吸收进摘要（不会因长度不同提前返回）；
//   - 摘要本身不可逆，即便有侧信道也拿不到密钥原文。
func VerifyBearer(r *http.Request, key string) bool {
	if key == "" {
		return true
	}
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, bearerPrefix) {
		// 缺头/方案不对：仍走一次摘要比较，保持耗时形状一致。
		subtle.ConstantTimeCompare(digest(""), digest(key))
		return false
	}
	tok := authz[len(bearerPrefix):]
	return subtle.ConstantTimeCompare(digest(tok), digest(key)) == 1
}

// digest 返回 s 的 SHA-256（定长 32 字节，供常量时间比较）。
func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// ---------------------------------------------------------------------------
// 会话 cookie 通道（v1.2.0）：HMAC 签名的无状态会话，面板登录换发。
// server 原生端点（/v1/*、/status、/api/request_logs）与 panel API 双通道同权：
// Bearer ∨ 会话 cookie。实现放本包的原因：panel 包 import server（模型映射），
// server 不能反向 import panel——校验逻辑抽到无依赖的共享包，两侧同一份。
// ---------------------------------------------------------------------------

// SessionCookieName 面板会话 cookie 名（登录换发 / 登出清除 / 校验三方共用）。
const SessionCookieName = "wb_session"

// SignSessionToken 生成 base64url(payload)+"."+hex(HMAC-SHA256(b64(payload),key))。
// HMAC 输入是 **base64 后的 payload 字符串**（与 VerifySessionValue 对齐——
// 校验侧只能从 cookie 值里拆出 b64 段，两侧对同一字节序列签名）。
func SignSessionToken(payload []byte, key string) string {
	b64 := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(b64))
	return b64 + "." + hex.EncodeToString(mac.Sum(nil))
}

// VerifySessionValue 校验会话 cookie 值：签名正确且未过期返回 true。
// key 为空 = 未启用鉴权，恒 true（与 VerifyBearer 语义一致）。
func VerifySessionValue(value, key string) bool {
	if key == "" {
		return true
	}
	dot := strings.LastIndexByte(value, '.')
	if dot < 0 {
		return false
	}
	payload, sigHex := value[:dot], value[dot+1:]
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(payload))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return false
	}
	var body struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return false
	}
	return body.Exp > time.Now().Unix()
}

// VerifySessionRequest 从请求的 wb_session cookie 校验会话通道；
// 无 cookie / 无效 / 过期返回 false。key 为空恒 true（未启用鉴权）。
func VerifySessionRequest(r *http.Request, key string) bool {
	c, err := r.Cookie(SessionCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	return VerifySessionValue(c.Value, key)
}
