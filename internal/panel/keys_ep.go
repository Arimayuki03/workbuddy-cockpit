// keys_ep.go 面板「API 密钥」页的后端 CRUD 端点（manager 壳契约，web/lib/api.ts
// keyApi 六条）：列表 / 创建（明文一次性 + 全格式导出片段）/ 修改 / 删除 /
// 重置用量 / 来源 IP 列表。keystore.Store 提供存储与校验（internal/keystore，
// 定稿不改），本文件只做 HTTP 接线：请求体 ↔ CreateParams / Update mut 闭包、
// Key 落库结构 → 前端 ApiKey DTO 的补算投影。
//
// 响应 DTO 不直接序列化 keystore.Key，原因有三（keysView 单一投影点）：
//   - 前端 ApiKey 契约要 ip_count（Key.ips 运行态 map 的长度）——Key 结构体
//     没有该字段，map 也未导出，只能在 DTO 层经 IPList 补算；
//   - Key 含 hash 字段（SHA-256 hex 摘要）——凭据本体的一部分，前端契约没有
//     它，逐字段投影天然不透出（比「序列化后删字段」不易漏）；
//   - realm "" 在契约里是合法值（不限制），不设 omitempty 也要稳定透出。
//
// 导出片段装配：明文只在 Create 返回一次（网关只存哈希），RenderKeyExports
// 必须在那一刻调用（keyexport.go 的签名约束）；片段含明文，随创建响应一次性
// 下发，列表/修改响应永不携带。片段里的 base_url 从请求 Host 推导（反代下由
// X-Forwarded-Proto 补 scheme），无单值模型时用「第一个白名单条目，否则模板
// 占位 cn:glm-5.2」——片段里的模型只是可改的示例值，占位足以让各片段语法完整。
//
// 鉴权口径：本族全部是面板管理语义——写端点（创建/修改/删除/重置）与 /api/config
// 同级，不进 wbt_ token 分级表（tokenAllowed 表外恒拒 403）；两个只读端点
// （GET /api/keys、GET /api/keys/{id}/ips）进 tokenLevelRead（只读档，脚本取数）。
package panel

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"

	"workbuddy2api/internal/keystore"
)

// exportModelFallback 白名单为空时导出片段里的模型占位（网关口径带 cn: 前缀；
// 片段里的模型是示例值，用户在客户端里改，占位只需让片段语法完整）。
const exportModelFallback = "cn:glm-5.2"

// keyPayload POST /api/keys 请求体（web/lib/types.ts KeyCreatePayload 契约）。
type keyPayload struct {
	Name           string   `json:"name"`
	Realm          string   `json:"realm"`
	ExpiresAt      int64    `json:"expires_at"`
	MaxIPs         int      `json:"max_ips"`
	IPWhitelist    []string `json:"ip_whitelist"`
	ModelWhitelist []string `json:"model_whitelist"`
	TokenQuota     int64    `json:"token_quota"`
	CreditQuota    float64  `json:"credit_quota"`
	RateLimit      int      `json:"rate_limit"`
	// Enabled 仅 PATCH 消费（KeyUpdatePayload 的附加字段）；POST 忽略——
	// 新密钥恒启用（keystore.Create 语义），前端创建表单也不发该字段。
	Enabled *bool `json:"enabled"`
}

// keyView 前端 ApiKey DTO（web/lib/types.ts 同名字段契约；json tag 即响应键）。
// 刻意不用 keystore.Key 直出：见文件头注释（ip_count 补算 + 不透 hash）。
type keyView struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Prefix         string   `json:"prefix"`
	Realm          string   `json:"realm"`
	Enabled        bool     `json:"enabled"`
	CreatedAt      int64    `json:"created_at"`
	ExpiresAt      int64    `json:"expires_at"`
	MaxIPs         int      `json:"max_ips"`
	IPWhitelist    []string `json:"ip_whitelist"`
	ModelWhitelist []string `json:"model_whitelist"`
	TokenQuota     int64    `json:"token_quota"`
	CreditQuota    float64  `json:"credit_quota"`
	UsedTokens     int64    `json:"used_tokens"`
	UsedCredits    float64  `json:"used_credits"`
	RateLimit      int      `json:"rate_limit"`
	LastUsedAt     int64    `json:"last_used_at"`
	IPCount        int      `json:"ip_count"`
}

// keysHandler GET /api/keys：{keys: [...]}。顺序按创建时间升序（map 快照无序，
// 面板展示要稳定的行序；最新创建的密钥在列表尾，与创建动作的时间直觉一致）。
func (p *Panel) keysHandler(w http.ResponseWriter, r *http.Request) {
	if p.cfg.KeyStore == nil {
		writeErr(w, http.StatusNotImplemented, "key store not available")
		return
	}
	list := p.cfg.KeyStore.List()
	sort.SliceStable(list, func(i, j int) bool { return list[i].CreatedAt < list[j].CreatedAt })
	out := make([]keyView, 0, len(list))
	for i := range list {
		out = append(out, p.keyViewOf(&list[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

// keyCreate POST /api/keys：body → CreateParams，明文一次性返回并随响应下发
// 全格式导出片段。CIDR 非法等业务错误 400（keystore 写侧 fail-fast 契约）。
func (p *Panel) keyCreate(w http.ResponseWriter, r *http.Request) {
	if p.cfg.KeyStore == nil {
		writeErr(w, http.StatusNotImplemented, "key store not available")
		return
	}
	var body keyPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, loginBodyLimit)).Decode(&body); err != nil && r.ContentLength != 0 {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	plaintext, k, err := p.cfg.KeyStore.Create(keystore.CreateParams{
		Name:           body.Name,
		Realm:          body.Realm,
		ExpiresAt:      body.ExpiresAt,
		MaxIPs:         body.MaxIPs,
		IPWhitelist:    body.IPWhitelist,
		ModelWhitelist: body.ModelWhitelist,
		TokenQuota:     body.TokenQuota,
		CreditQuota:    body.CreditQuota,
		RateLimit:      body.RateLimit,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	resp := map[string]any{"key": p.keyViewOf(&k), "plaintext": plaintext}
	// 导出片段：明文丢弃前唯一时机（keyexport.go 签名约束）。渲染失败降级为
	// 仅无片段（创建本身已成功，密钥可用），不回滚不 5xx——网关只存哈希，
	// 片段事后补不出，但前端仍能手工配置（响应里有明文）。
	pieces, err := p.renderKeyExports(r, plaintext, body)
	if err != nil {
		log.Printf("WARN: [panel] 密钥导出片段渲染失败（创建响应不含 exports）: %v", err)
	} else {
		resp["exports"] = pieces
	}
	log.Printf("panel: key created id=%s prefix=%s name=%q realm=%q", k.ID, k.Prefix, k.Name, k.Realm)
	writeJSON(w, http.StatusOK, resp)
}

// keyUpdate PATCH /api/keys/{id}：部分字段可选（指针 nil = 不改），转成
// keystore.Update 的 mut 闭包在库锁内生效。身份字段（ID/Hash/Prefix）本就不在
// payload 里，Update 侧另有强制还原（keystore 契约，双保险）。CIDR 非法透传 400。
func (p *Panel) keyUpdate(w http.ResponseWriter, r *http.Request) {
	if p.cfg.KeyStore == nil {
		writeErr(w, http.StatusNotImplemented, "key store not available")
		return
	}
	id := r.PathValue("id")
	var body keyPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, loginBodyLimit)).Decode(&body); err != nil && r.ContentLength != 0 {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	// mut 语义：指针非 nil 才写（部分更新；零值显式提交也走这条——前端传
	// max_ips:0 是「改回不限」而不是「不改」，指针可空区分了这两者）。
	err := p.cfg.KeyStore.Update(id, func(k *keystore.Key) {
		if body.Name != "" {
			k.Name = body.Name
		}
		if body.Realm != "" {
			k.Realm = body.Realm
		}
		if body.ExpiresAt != 0 {
			k.ExpiresAt = body.ExpiresAt
		}
		if body.MaxIPs != 0 {
			k.MaxIPs = body.MaxIPs
		}
		if body.IPWhitelist != nil {
			k.IPWhitelist = body.IPWhitelist
		}
		if body.ModelWhitelist != nil {
			k.ModelWhitelist = body.ModelWhitelist
		}
		if body.TokenQuota != 0 {
			k.TokenQuota = body.TokenQuota
		}
		if body.CreditQuota != 0 {
			k.CreditQuota = body.CreditQuota
		}
		if body.RateLimit != 0 {
			k.RateLimit = body.RateLimit
		}
		if body.Enabled != nil {
			k.Enabled = *body.Enabled
		}
	})
	switch {
	case errors.Is(err, keystore.ErrNotFound):
		writeErr(w, http.StatusNotFound, "key not found")
		return
	case err != nil:
		// CIDR 非法等校验错误（keystore 写侧 fail-fast）：400 原文透传，
		// 面板当场提示修正。
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Update 无返回值，回读快照组装 DTO（取到的一定是刚写后的状态）。
	list := p.cfg.KeyStore.List()
	for i := range list {
		if list[i].ID == id {
			writeJSON(w, http.StatusOK, map[string]any{"key": p.keyViewOf(&list[i])})
			return
		}
	}
	// 并发删除窗口：Update 已成功但快照里已无该 id——404 让前端刷新列表。
	writeErr(w, http.StatusNotFound, "key not found")
}

// keyDelete POST /api/keys/{id}/delete：前端契约（web/lib/api.ts keyApi.remove
// 发 POST …/delete，不用 DELETE 方法）。删除凭据不可恢复，响应仅 {ok:true}。
func (p *Panel) keyDelete(w http.ResponseWriter, r *http.Request) {
	if p.cfg.KeyStore == nil {
		writeErr(w, http.StatusNotImplemented, "key store not available")
		return
	}
	id := r.PathValue("id")
	if err := p.cfg.KeyStore.Delete(id); err != nil {
		if errors.Is(err, keystore.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "key not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("panel: key deleted id=%s", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// keyResetUsage POST /api/keys/{id}/reset_usage：已用 token 与积分一起归零
// （keystore.ResetUsage 契约：两个量同清，界面是同一个按钮）。
func (p *Panel) keyResetUsage(w http.ResponseWriter, r *http.Request) {
	if p.cfg.KeyStore == nil {
		writeErr(w, http.StatusNotImplemented, "key store not available")
		return
	}
	id := r.PathValue("id")
	if err := p.cfg.KeyStore.ResetUsage(id); err != nil {
		if errors.Is(err, keystore.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "key not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// keyIPs GET /api/keys/{id}/ips：来源 IP → first_seen 快照转数组，按首次使用
// 时间降序（最近活跃的在最前，运维视角最关心）。密钥不存在 404（IPList 对已删
// id 返回空表不报错——存在性判定用 List 快照对账）。
func (p *Panel) keyIPs(w http.ResponseWriter, r *http.Request) {
	if p.cfg.KeyStore == nil {
		writeErr(w, http.StatusNotImplemented, "key store not available")
		return
	}
	id := r.PathValue("id")
	list := p.cfg.KeyStore.List()
	var target *keystore.Key
	for i := range list {
		if list[i].ID == id {
			target = &list[i]
			break
		}
	}
	if target == nil {
		writeErr(w, http.StatusNotFound, "key not found")
		return
	}
	m := p.cfg.KeyStore.IPList(target)
	out := make([]map[string]any, 0, len(m))
	for ip, ts := range m {
		out = append(out, map[string]any{"ip": ip, "first_seen": ts})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i]["first_seen"].(int64) > out[j]["first_seen"].(int64)
	})
	writeJSON(w, http.StatusOK, map[string]any{"ips": out})
}

// keyViewOf keystore.Key 快照 → 前端 ApiKey DTO（文件头注释的三点投影理由）。
// ip_count 经 IPList 取运行态 ips map 长度（Key 结构体无该导出字段）。
func (p *Panel) keyViewOf(k *keystore.Key) keyView {
	return keyView{
		ID:             k.ID,
		Name:           k.Name,
		Prefix:         k.Prefix,
		Realm:          k.Realm,
		Enabled:        k.Enabled,
		CreatedAt:      k.CreatedAt,
		ExpiresAt:      k.ExpiresAt,
		MaxIPs:         k.MaxIPs,
		IPWhitelist:    nonNilStrings(k.IPWhitelist),
		ModelWhitelist: nonNilStrings(k.ModelWhitelist),
		TokenQuota:     k.TokenQuota,
		CreditQuota:    k.CreditQuota,
		UsedTokens:     k.UsedTokens,
		UsedCredits:    k.UsedCredits,
		RateLimit:      k.RateLimit,
		LastUsedAt:     k.LastUsedAt,
		IPCount:        len(p.cfg.KeyStore.IPList(k)),
	}
}

// nonNilStrings nil 切片 → 空数组（前端契约 ip_whitelist/model_whitelist 是
// string[]，JSON null 会让 `xxx.length` 类直接消费炸掉）。
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// renderKeyExports 全格式导出片段装配（keyexport.go RenderKeyExports）。
// model 取值链：请求体模型白名单第一项 → 空白名单用模板占位（片段里的模型
// 是可改的示例值，占位只需让各片段语法完整）。baseURL 从请求 Host 推导。
func (p *Panel) renderKeyExports(r *http.Request, plaintext string, body keyPayload) ([]ExportPiece, error) {
	model := exportModelFallback
	for _, m := range body.ModelWhitelist {
		if s := strings.TrimSpace(m); s != "" {
			model = s
			break
		}
	}
	return RenderKeyExports(requestBaseURL(r), plaintext, model)
}

// requestBaseURL 请求 Host 推导面板对外地址（X-Forwarded-Proto 补 scheme：
// 反代终结 TLS 时 r.TLS 为 nil，r.Host 恒是 http——显式转发头优先，与浏览器
// 地址栏一致；r.TLS 非 nil = 直连 HTTPS）。panel 包其余端点无此需求（错误体
// 不含自引 URL），首例从此。
func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	return scheme + "://" + host
}
