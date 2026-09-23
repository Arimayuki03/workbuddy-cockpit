// transfer.go 账号凭据导出/导入（panel 端点）：把 auths/ 目录里的凭证打包成
// 单个 JSON 文件带出，或在另一台网关上整批导入。
//
// 格式与用途：导出文件是 {"ok":true,"accounts":[<SaveAtomic 嵌套形>,...]} 的
// 包裹文档——accounts 数组元素与 auths/workbuddy-*.json 完全同形，因此导入走
// auth.Parse（嵌套形/扁平形双形态），导出的文件、手拷的单个 auth 文件、
// 扁平形旧文件都能吃；导入方落盘 SaveAtomic 后统一归一为嵌套形。
//
// 安全口径：凭据含 accessToken/refreshToken（等于账号本身），导出前必须过
// withAuth（api_key / 会话双通道，与全部 panel API 同一闸口，无独立开关）；
// 全程不落任何 token 到日志。导入的 uid 仍过 validUID（防路径穿越拼文件名）。
package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

// importBodyLimit 导入请求体上限：单个凭证文档 2~4 KB 量级，100 个账号
// 远不到 1 MB；20MB 是防御性上限（防大 body 拖内存）。
const importBodyLimit = 20 << 20

// accountsExport GET /api/accounts/export：导出全部账号凭据为可下载 JSON。
// 响应头按附件下载处理（Content-Disposition + nosniff），避免浏览器把含
// token 的 JSON 直接渲染到页面。
func (p *Panel) accountsExport(w http.ResponseWriter, r *http.Request) {
	docs := make([]map[string]any, 0)
	for _, s := range p.cfg.Pool.List() {
		a := p.cfg.Pool.AuthByUID(s.UID)
		if a == nil {
			continue
		}
		docs = append(docs, a.ExportDoc())
	}
	raw, err := json.Marshal(map[string]any{
		"ok":          true,
		"exported_at": time.Now().Format(time.RFC3339),
		"accounts":    docs,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "marshal export: "+err.Error())
		return
	}
	log.Printf("panel: 导出账号凭据 %d 个（含 token，勿外传不可信对象）", len(docs))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="workbuddy-accounts-%s.json"`, time.Now().Format("20060102-150405")))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// accountsImport POST /api/accounts/import：body 为导出文件原样（或裸数组 /
// 单个凭证文档）。逐个 Parse → validUID 防路径穿越 → BackfillRealm →
// SaveAtomic 落盘 → pool.Add 热加载（同 uid 覆盖凭证，即「更新」语义）。
// 不做签到/领奖（与 OAuth 登录不同：导入的是存量号，动作留给排程/手动）。
func (p *Panel) accountsImport(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, importBodyLimit))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	docs, err := importDocs(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(docs) == 0 {
		writeErr(w, http.StatusBadRequest, "导入文件里没有账号凭据")
		return
	}
	if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "mkdir auth dir: "+err.Error())
		return
	}

	type skipRow struct {
		UID    string `json:"uid,omitempty"`
		Reason string `json:"reason"`
	}
	var imported int
	var skipped []skipRow
	seen := map[string]bool{}
	for i, doc := range docs {
		a, err := auth.Parse(doc)
		if err != nil {
			skipped = append(skipped, skipRow{Reason: fmt.Sprintf("第 %d 项解析失败: %v", i+1, err)})
			continue
		}
		// uid 来自导入文件，落盘前过白名单（与 OAuth 登录同一防线，防路径穿越）。
		if !validUID(a.UID) {
			skipped = append(skipped, skipRow{Reason: fmt.Sprintf("第 %d 项 uid 非法或缺失，拒绝落盘", i+1)})
			continue
		}
		// 空 realm 的旧文件按 domain 推断补齐（与 LoadDir 存量迁移同语义），落盘形态统一。
		_, _ = a.BackfillRealm()
		a.FilePath = filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", a.UID))
		if err := a.SaveAtomic(); err != nil {
			skipped = append(skipped, skipRow{UID: a.UID, Reason: "落盘失败: " + err.Error()})
			continue
		}
		p.cfg.Pool.Add(a)
		// 导入 = 人工恢复口径（与 OAuth 登录一致）：清旧禁用/手动停用；
		// 冷却/熔断到期与成功自愈，不在此覆盖。
		p.cfg.Pool.ReviveDisabled(a.UID)
		p.cfg.Pool.SetManualDisabled(a.UID, false, "")
		if seen[a.UID] {
			log.Printf("panel: 导入覆盖同 uid=%s（文件内重复，后者胜出）", a.UID)
		}
		seen[a.UID] = true
		imported++
	}
	log.Printf("panel: 导入账号凭据完成 imported=%d skipped=%d", imported, len(skipped))
	if skipped == nil {
		skipped = []skipRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"imported": imported,
		"skipped":  skipped,
	})
}

// importDocs 从导入 body 解出凭证文档列表。兼容三种形态：
//  1. 导出文件包裹形 {"accounts":[...]}（可选省略外层，直接是数组）；
//  2. 裸数组 [...]（多个凭证）；
//  3. 单个凭证文档（auths/workbuddy-*.json 原样，嵌套形或扁平形）。
//
// 判形依据是「能否解出 accounts 键」，不猜内容——单个文档无论什么形态都走
// auth.Parse 兜底，形态 3 不做前置结构校验。
func importDocs(raw []byte) ([]json.RawMessage, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fmt.Errorf("empty body")
	}
	var probe struct {
		Accounts []json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil && probe.Accounts != nil {
		return probe.Accounts, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, fmt.Errorf("parse array: %w", err)
		}
		return arr, nil
	}
	// 单个文档原样透传（是否合法交 auth.Parse 判定）。
	return []json.RawMessage{json.RawMessage(raw)}, nil
}
