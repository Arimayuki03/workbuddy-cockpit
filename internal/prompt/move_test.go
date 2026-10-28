package prompt

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMoveSystemToUserMovesTextAndKeepsUserContent(t *testing.T) {
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"system","content":"PERSONA-A"},` +
		`{"role":"developer","content":"PERSONA-B"},` +
		`{"role":"user","content":"hello"},` +
		`{"role":"assistant","content":"hi"}]}`)
	out, ok := MoveSystemToUser(body)
	if !ok {
		t.Fatal("有 system 文本与 user 消息时应成功")
	}
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("两条 system 应合并为一条中性 system，got %d 条", len(req.Messages))
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content != Degraded {
		t.Fatalf("首条应换成中性提示词，got %+v", req.Messages[0])
	}
	if req.Messages[1].Role != "user" {
		t.Fatalf("第二条应仍是 user，got %+v", req.Messages[1])
	}
	if !strings.Contains(req.Messages[1].Content, "PERSONA-A") ||
		!strings.Contains(req.Messages[1].Content, "PERSONA-B") ||
		!strings.HasSuffix(req.Messages[1].Content, "hello") {
		t.Fatalf("system 文本应原样搬到 user 前部、原 user 内容在尾部，got %q", req.Messages[1].Content)
	}
	if req.Messages[2].Content != "hi" {
		t.Fatalf("后续消息应逐字不动，got %+v", req.Messages[2])
	}
}

func TestMoveSystemToUserBails(t *testing.T) {
	cases := map[string]string{
		"非 JSON":     `not json`,
		"无 messages": `{}`,
		"没有 system":  `{"messages":[{"role":"user","content":"hi"}]}`,
		"没有 user":    `{"messages":[{"role":"system","content":"s"}]}`,
		"user 非纯文本": `{"messages":[{"role":"system","content":"s"},` +
			`{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
		// 混合字符串 system + 数组 content system：数组 system 的文本无法原样搬进
		// user，若继续则会被重写阶段整条删除，必须保守放弃（文本一字不改契约）。
		"混合字符串与数组 system": `{"messages":[` +
			`{"role":"system","content":"S"},` +
			`{"role":"system","content":[{"type":"text","text":"S2"}]},` +
			`{"role":"user","content":"u"}]}`,
		"对象 content system": `{"messages":[` +
			`{"role":"system","content":{"type":"text","text":"S"}},` +
			`{"role":"user","content":"u"}]}`,
		"数字 content system": `{"messages":[` +
			`{"role":"system","content":123},` +
			`{"role":"user","content":"u"}]}`,
	}
	for name, body := range cases {
		if _, ok := MoveSystemToUser([]byte(body)); ok {
			t.Errorf("%s 应保守返回 false", name)
		}
	}
}

// 空字符串 / 缺失 / null 的 system content 无文本可搬，可安全跳过；
// 其余字符串 system 文本仍须原样搬到 user。
func TestMoveSystemToUserSkipsEmptySystemContent(t *testing.T) {
	body := []byte(`{"messages":[` +
		`{"role":"system","content":""},` +
		`{"role":"system"},` +
		`{"role":"developer","content":null},` +
		`{"role":"system","content":"KEEP"},` +
		`{"role":"user","content":"hello"}]}`)
	out, ok := MoveSystemToUser(body)
	if !ok {
		t.Fatal("存在非空字符串 system 文本时应成功")
	}
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("所有 system/developer 应被单条中性 system 取代，got %d 条", len(req.Messages))
	}
	if req.Messages[0].Content != Degraded {
		t.Fatalf("首条应为中性提示词，got %+v", req.Messages[0])
	}
	if req.Messages[1].Content != "KEEP\n\nhello" {
		t.Fatalf("仅非空字符串 system 文本应搬到 user 前部，got %q", req.Messages[1].Content)
	}
}

// 顶层未知字段与大整数必须原样透传（与 Rewrite/Append 同一 RawMessage 口径）。
func TestMoveSystemToUserPreservesUnknownFields(t *testing.T) {
	body := []byte(`{"seed":9007199254740993,"metadata":{"k":1},"messages":[` +
		`{"role":"system","content":"S"},{"role":"user","content":"u"}]}`)
	out, ok := MoveSystemToUser(body)
	if !ok {
		t.Fatal("应成功")
	}
	if !strings.Contains(string(out), `"seed":9007199254740993`) {
		t.Fatalf("大整数被改写: %s", out)
	}
	if !strings.Contains(string(out), `"metadata":{"k":1}`) {
		t.Fatalf("未知字段丢失: %s", out)
	}
}
