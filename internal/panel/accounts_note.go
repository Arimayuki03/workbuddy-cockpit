// accounts_note.go 账号备注端点（POST /api/accounts/{uid}/note）：面板 accounts
// 页备注的后端化落点。历史版本备注存浏览器 localStorage（key accountsNotes，
// 按 uid），换浏览器即丢；后端化后经 Pool.SetNote 进池状态、state.json 落盘、
// /api/overview accounts[].note 回显，前端首载做一次性迁移。
//
// 鉴权口径：经 p.api() 注册（withAuth 闸：会话 cookie / Bearer api_key 双通道），
// 与 disable/enable 等写端点同源。**不进** wbt_ token 分级表（tokenEndpointLevels）
// ——写语义按既有口径分级表外恒 403 token_write_forbidden，无需在此显式拒绝。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
)

// noteBody 请求体：{"note": "..."}。空串 = 清除备注（与前端「清空即删除」语义对齐）。
// 前端上限 200 字符（NoteEditor maxLength），服务端同步钳制防绕过。
type noteBody struct {
	Note string `json:"note"`
}

// noteMaxLength 备注长度上限，与 web 前端 NoteEditor 的 maxLength=200 同口径。
const noteMaxLength = 200

// accountNote 保存单账号备注。空 note = 清除。
//   - 404 account not found：uid 不在池里（与 disable/enable 同判据 Pool.Status）；
//   - 400 invalid_body / note_too_long：请求体非法或超长；
//   - 200 {"ok":true}：写入成功（幂等：同值重复写不重复落盘，见 pool.SetNote）。
func (p *Panel) accountNote(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	var body noteBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body")
		return
	}
	note := body.Note
	if len(note) > noteMaxLength {
		writeErr(w, http.StatusBadRequest, "note_too_long")
		return
	}
	if err := p.cfg.Pool.SetNote(uid, note); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if note == "" {
		log.Printf("panel: note uid=%s（备注已清除）", uid)
	} else {
		log.Printf("panel: note uid=%s（备注已保存，%d 字符）", uid, len(note))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
