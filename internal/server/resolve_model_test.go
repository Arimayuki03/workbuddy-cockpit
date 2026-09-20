package server

import "testing"

// TestResolveModel 覆盖 PLAN D6 前缀解析协议：
// 取第一个 ":"，前段为 cn/global 才剥离，否则 (cn, 原串)。
func TestResolveModel(t *testing.T) {
	cases := []struct {
		in           string
		wantRealm    string
		wantBare     string
	}{
		{"cn:glm-5.2", "cn", "glm-5.2"},
		{"global:gpt-5.4", "global", "gpt-5.4"},
		{"glm-5.2", "cn", "glm-5.2"},
		{"deepseek:v3", "cn", "deepseek:v3"}, // 冒号前段不在枚举内，不剥离
	}
	for _, c := range cases {
		realm, bare := resolveModel(c.in)
		if realm != c.wantRealm || bare != c.wantBare {
			t.Errorf("resolveModel(%q)=(%q,%q) want (%q,%q)", c.in, realm, bare, c.wantRealm, c.wantBare)
		}
	}
}

// TestResolveModelEdge 边界：空串、仅冒号、空前缀、大小写。
func TestResolveModelEdge(t *testing.T) {
	cases := []struct {
		in        string
		wantRealm string
		wantBare  string
	}{
		{"", "cn", ""},
		{":", "cn", ":"},
		{":model", "cn", ":model"}, // 空前缀不匹配 cn/global，不剥离
		{"GLOBAL:gpt-5", "cn", "GLOBAL:gpt-5"}, // 大小写敏感：不做归一
		{"global:", "global", ""},              // 前缀合法 + 空裸名仍剥离
		{"global:gpt-5.4", "global", "gpt-5.4"},
		{"cn:", "cn", ""},
	}
	for _, c := range cases {
		realm, bare := resolveModel(c.in)
		if realm != c.wantRealm || bare != c.wantBare {
			t.Errorf("resolveModel(%q)=(%q,%q) want (%q,%q)", c.in, realm, bare, c.wantRealm, c.wantBare)
		}
	}
}
// TestApplyModelMap 链头映射（v1.2.0）：整名单次替换、不递归、恒等映射剔除。
func TestApplyModelMap(t *testing.T) {
	SetModelMap(map[string]string{
		"gpt-4o":     "cn:glm-5.2",  // 裸名客户端 → CN 前缀模型
		"cn:glm-5.2": "glm-5.3",     // 前缀名 → 裸名（默认 CN）
		"self":       "self",        // 恒等映射：SetModelMap 剔除
		"":           "x",           // 空键剔除
	})
	t.Cleanup(func() { SetModelMap(nil) })

	if got := applyModelMap("gpt-4o"); got != "cn:glm-5.2" {
		t.Errorf("applyModelMap(gpt-4o)=%q want cn:glm-5.2", got)
	}
	// 不递归：映射目标不再查表。
	if got := applyModelMap("cn:glm-5.2"); got != "glm-5.3" {
		t.Errorf("applyModelMap(cn:glm-5.2)=%q want glm-5.3", got)
	}
	// resolveModel 链头生效：映射后的裸名走前缀解析。
	if realm, bare := resolveModel("gpt-4o"); realm != "cn" || bare != "glm-5.2" {
		t.Errorf("resolveModel(gpt-4o)=(%q,%q) want (cn,glm-5.2)", realm, bare)
	}
	// 未命中原样返回。
	if got := applyModelMap("unknown-model"); got != "unknown-model" {
		t.Errorf("applyModelMap(unknown)=%q want unchanged", got)
	}
}

// TestModelMapView 副本语义：改副本不影响生效表。
func TestModelMapView(t *testing.T) {
	SetModelMap(map[string]string{"a": "b"})
	t.Cleanup(func() { SetModelMap(nil) })

	view := ModelMapView()
	view["a"] = "c"
	if got := ModelMapView()["a"]; got != "b" {
		t.Errorf("view mutation leaked: a=%q want b", got)
	}
}
