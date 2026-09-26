// config.go 面板配置页接口：读取当前配置、校验并保存（热生效 + 重启项标注）。
//
// 分工：cmd/server 持有 Config 类型与校验逻辑（Load/normalize），此处只做
// HTTP 编排——GET 回显、POST 透传给注入的 SaveConfig 闭包（由 main 完成
// "校验 → 落盘 → 热应用 → 返回需重启字段列表"）。
package panel

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"workbuddy2api/internal/redisstore"
)

// sensitiveKeys 配置里的敏感标量键 → 脱敏标记前缀（标记键 = "has_"+prefix /
// prefix+"_masked"）。getConfig 把它们替换为这些**非配置键**的脱敏标记；
// saveConfig 收到含标记的提交时必须原样剔除（见 stripSensitiveMarkers），
// 否则会被深合并吸收进 config.json，下次启动 Unmarshal 校验直接失败。
// api_key 的标记是 has_key/key_masked（前端 settings 页按此契约读取）。
var sensitiveKeys = map[string]string{
	"token":   "token",
	"api_key": "key",
}

// markerKeySet 剥离键集合 = sensitiveKeys 生成的全部标记键（两端必须对称：
// 多于生成集会把用户手写的同名字段误删，少于生成集会让标记漏进 config.json）。
var markerKeySet = func() map[string]bool {
	out := map[string]bool{}
	for _, prefix := range sensitiveKeys {
		out["has_"+prefix] = true
		out[prefix+"_masked"] = true
	}
	return out
}()

// maskValue 生成形如 "****abcd" 的掩码（保留末 4 位；长度 ≤4 时全掩）。
func maskValue(v string) string {
	if len(v) <= 4 {
		return "****"
	}
	return "****" + v[len(v)-4:]
}

// sanitizeConfigCopy 返回 cfg 的深拷贝：map 中命中 sensitiveKeys 的字符串值
// 被替换为脱敏标记（has_<prefix> + <prefix>_masked），其余键原样保留（嵌套
// map/slice 递归）。只读展示用——绝不能改到调用方持有的原 map。
func sanitizeConfigCopy(cfg any) any {
	switch t := cfg.(type) {
	case map[string]any:
		out := make(map[string]any, len(t)+2)
		for k, v := range t {
			if s, ok := v.(string); ok && s != "" {
				if prefix, hit := sensitiveKeys[k]; hit {
					out["has_"+prefix] = true
					out[prefix+"_masked"] = maskValue(s)
					continue
				}
				out[k] = v
				continue
			}
			out[k] = sanitizeConfigCopy(v)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, v := range t {
			out[i] = sanitizeConfigCopy(v)
		}
		return out
	default:
		return cfg
	}
}

// getConfig 返回当前配置文件内容与路径（前端按 schema 渲染表单）。
func (p *Panel) getConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	cfg, err := p.cfg.LoadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load config: "+err.Error())
		return
	}
	// LoadConfig 返回的是 *Config 结构体，先经 JSON 往返变成通用 map 再脱敏
	// （sanitizeConfigCopy 只遍历 map/slice；结构体直通 default 分支会漏掉敏感键）。
	// 往返同时天然遵守 json:"-" 等标签（PromptText 不会透出）。
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encode config: "+err.Error())
		return
	}
	var cfgMap map[string]any
	if err := json.Unmarshal(cfgJSON, &cfgMap); err != nil {
		writeErr(w, http.StatusInternalServerError, "normalize config: "+err.Error())
		return
	}
	// 敏感值（upstash.token / api_key）不回显明文：替换为 has_token/token_masked/
	// has_key 脱敏标记。保存路径对标记键剔除（saveConfig 侧），深合并保留原值。
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"path":   p.cfg.ConfigPath,
		"config": sanitizeConfigCopy(cfgMap),
	})
}

// saveConfig 保存配置：body 直接是配置 JSON（前端按 schema 组装完整对象）。
// SaveConfig 闭包内部完成校验+落盘+热应用；校验失败返回 400 且不写盘。
// 高级模式会把 GET 回显的脱敏标记（has_token/token_masked/has_key）原样回传，
// stripSensitiveMarkers 先剔除它们，避免被深合并吸收进 config.json。
func (p *Panel) saveConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if stripped, changed, stripErr := stripSensitiveMarkers(raw); stripErr == nil && changed {
		raw = stripped
	}
	restartRequired, err := p.cfg.SaveConfig(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if restartRequired == nil {
		restartRequired = []string{}
	}
	log.Printf("panel: 配置已保存（热生效完成；需重启字段 %d 个）", len(restartRequired))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"restart_required": restartRequired,
	})
}

// testUpstash 测试 Upstash 连通性（manager 设置页"测试连接"按钮）。
// body {"url": "...", "token": "..."}——token 留空表示沿用已保存的值
// （UpstashSavedToken 闭包，未注入或空则无回落）。url 必填。
// 响应 {"ok": bool, "message": string}，ok=false 时 message 含脱敏后的失败原因。
func (p *Panel) testUpstash(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, loginBodyLimit)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	token := body.Token
	if token == "" && p.cfg.UpstashSavedToken != nil {
		token = p.cfg.UpstashSavedToken()
	}
	if err := redisstore.Probe(body.URL, token); err != nil {
		if errors.Is(err, redisstore.ErrEmptyURL) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "url 未填写"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "连接成功"})
}

// stripSensitiveMarkers 从提交的配置 JSON 里剔除脱敏标记键（深遍历）。
// 返回重序列化的 JSON 与「是否发生过剔除」；解析失败原样返回。
// changed=false（正常保存根本不含标记）走零拷贝快路径。
func stripSensitiveMarkers(raw []byte) ([]byte, bool, error) {
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return raw, false, err
	}
	cleaned, changed := stripMarkersWalk(root)
	if !changed {
		return raw, false, nil
	}
	out, err := json.Marshal(cleaned)
	if err != nil {
		return raw, false, err
	}
	return out, true, nil
}

// stripMarkersWalk 递归剔除 map 里的标记键（含数组内对象）。
func stripMarkersWalk(v any) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		changed := false
		for k := range t {
			if markerKeySet[k] {
				delete(t, k)
				changed = true
			}
		}
		for k, vv := range t {
			c, ch := stripMarkersWalk(vv)
			if ch {
				t[k] = c
				changed = true
			}
		}
		return t, changed
	case []any:
		changed := false
		for i, vv := range t {
			c, ch := stripMarkersWalk(vv)
			if ch {
				t[i] = c
				changed = true
			}
		}
		return t, changed
	default:
		return v, false
	}
}
