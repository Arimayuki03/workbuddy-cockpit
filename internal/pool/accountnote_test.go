// accountnote_test.go 账号备注（Pool.SetNote）的状态位与持久化语义测试。
//
// 覆盖三条不变量：
//  1. SetNote 写入的备注经 Status.Note 透出（/api/overview accounts[].note 的数据源）；
//  2. 空串 = 清空（不让空备注残留）；同值重复写幂等（不置脏）；未知 uid 报错；
//  3. 备注持久化到 state.json，重启保留（换浏览器不再丢备注的后端锚点）。
//
// 写法参照 manualdisable_test.go / renew 观测测试的口径。
package pool

import (
	"os"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestSetNoteRoundTrip 写入 → Status 透出 → 清空 → 透出空。备注是纯展示元数据，
// 不应影响选号（健康号写备注后仍可选）。
func TestSetNoteRoundTrip(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	if err := p.SetNote("u1", "生产主力号，勿动"); err != nil {
		t.Fatalf("SetNote: %v", err)
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("precondition: 账号应在池里")
	}
	if st.Note != "生产主力号，勿动" {
		t.Errorf("Status.Note=%q want %q", st.Note, "生产主力号，勿动")
	}
	// List（/api/overview 的数据源）同样带出
	for _, s := range p.List() {
		if s.UID == "u1" && s.Note != "生产主力号，勿动" {
			t.Errorf("List().Note=%q want 写入值", s.Note)
		}
	}
	// 纯元数据：不影响选号
	if got := p.Pick(""); got == nil || got.UID != "u1" {
		t.Fatalf("写备注后账号应仍可选, got %+v", got)
	}

	// 空串 = 清空
	if err := p.SetNote("u1", ""); err != nil {
		t.Fatalf("SetNote(空): %v", err)
	}
	if st, _ := p.Status("u1"); st.Note != "" {
		t.Errorf("清空后 Status.Note=%q want 空", st.Note)
	}
}

// TestSetNoteIdempotentAndUnknown 同值重复写幂等、未知 uid 返回错误。
func TestSetNoteIdempotentAndUnknown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	if err := p.SetNote("u1", "a"); err != nil {
		t.Fatalf("首次写入: %v", err)
	}
	// 同值重复写：不报错（幂等），Status 不变
	if err := p.SetNote("u1", "a"); err != nil {
		t.Fatalf("同值重复写不应报错: %v", err)
	}
	if st, _ := p.Status("u1"); st.Note != "a" {
		t.Errorf("重复写后 Note=%q want a", st.Note)
	}
	// 未知 uid：报错（端点据此回 404/500，与 SetManualDisabled 的 (false,false) 口径不同——
	// error 返回让 handler 能区分「不存在」与「成功」）
	if err := p.SetNote("nope", "x"); err == nil {
		t.Error("未知 uid 应返回错误")
	}
}

// TestSetNotePersists 备注落盘持久化——重启保留（本功能的核心诉求：备注不再
// 随浏览器 localStorage 丢失）。
func TestSetNotePersists(t *testing.T) {
	dir := t.TempDir()
	state := dir + "/state.json"

	p := New(state)
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetNote("u1", "跨重启要保留的备注")
	p.Flush()

	// 模拟重启：新池读同一 state 文件
	p2 := New(state)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.Add(&auth.Auth{UID: "u2"})

	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("no status after reload")
	}
	if st.Note != "跨重启要保留的备注" {
		t.Fatalf("重启后备注丢失: %q", st.Note)
	}
	// 未写备注的账号不受影响（omitempty：空备注不落冗余键）
	if st2, _ := p2.Status("u2"); st2.Note != "" {
		t.Errorf("u2 不应被连带写入备注: %q", st2.Note)
	}

	// 清空后落盘，再重启确认已清（空串=删除的语义跨重启成立）
	p2.SetNote("u1", "")
	p2.Flush()
	p3 := New(state)
	p3.Add(&auth.Auth{UID: "u1"})
	if st3, _ := p3.Status("u1"); st3.Note != "" {
		t.Fatalf("清空后重启不应复活备注: %q", st3.Note)
	}
	// 落盘文件里空备注不残留冗余键（omitempty 口径）
	raw, err := os.ReadFile(state)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if strings.Contains(string(raw), `"note"`) {
		t.Errorf("清空后 state.json 不应再含 note 键: %s", raw)
	}
}
