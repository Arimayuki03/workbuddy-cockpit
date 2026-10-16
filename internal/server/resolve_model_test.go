package server

import "testing"

// TestResolveModel 覆盖 PLAN D6 前缀解析协议：
// 取第一个 ":"，前段为 cn/global 才剥离，否则 (cn, 原串)。
func TestResolveModel(t *testing.T) {
	cases := []struct {
		in        string
		wantRealm string
		wantBare  string
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
		{":model", "cn", ":model"},             // 空前缀不匹配 cn/global，不剥离
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
		"gpt-4o":     "cn:glm-5.2", // 裸名客户端 → CN 前缀模型
		"cn:glm-5.2": "glm-5.3",    // 前缀名 → 裸名（默认 CN）
		"self":       "self",       // 恒等映射：SetModelMap 剔除
		"":           "x",          // 空键剔除
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

// TestApplyModelMapFallback 兜底键 "*"：查找顺序 = 完整请求名精确匹配 → "*" 兜底 → 原样返回，
// 单次替换、不递归。table=nil 表示不配置任何映射。
func TestApplyModelMapFallback(t *testing.T) {
	cases := []struct {
		name  string
		table map[string]string
		in    string
		want  string
	}{
		{"精确匹配优先于兜底", map[string]string{"gpt-4o": "cn:glm-5.2", "*": "glm-5.3"}, "gpt-4o", "cn:glm-5.2"},
		{"无精确命中时兜底生效", map[string]string{"gpt-4o": "cn:glm-5.2", "*": "glm-5.3"}, "gpt-4o-typo", "glm-5.3"},
		{"拼错的 realm 前缀同样被兜底接住", map[string]string{"*": "glm-5.3"}, "globla:gpt-5.4", "glm-5.3"},
		{"未配置兜底键则未命中原样返回", map[string]string{"gpt-4o": "cn:glm-5.2"}, "gpt-4o-typo", "gpt-4o-typo"},
		{"空表原样返回", nil, "gpt-4o-typo", "gpt-4o-typo"},
		{"兜底值不再二次查表", map[string]string{"*": "glm-5.3", "glm-5.3": "glm-9"}, "gpt-4o-typo", "glm-5.3"},
		{"请求名恰为星号走精确匹配", map[string]string{"*": "glm-5.3"}, "*", "glm-5.3"},
		{"空模型名同样被兜底接住", map[string]string{"*": "glm-5.3"}, "", "glm-5.3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			SetModelMap(c.table)
			t.Cleanup(func() { SetModelMap(nil) })
			if got := applyModelMap(c.in); got != c.want {
				t.Errorf("applyModelMap(%q)=%q want %q (table=%v)", c.in, got, c.want, c.table)
			}
		})
	}
}

// TestApplyModelMapFallbackCleaning SetModelMap 清洗与 "*" 的边界：
// "*"→某模型 保留（有效兜底）；"*"→"*" 恒等、"*"→"" 空值剔除。
func TestApplyModelMapFallbackCleaning(t *testing.T) {
	t.Cleanup(func() { SetModelMap(nil) })

	SetModelMap(map[string]string{"*": "*"})
	if view := ModelMapView(); len(view) != 0 {
		t.Errorf(`SetModelMap({"*":"*"}) 应剔除恒等映射，got %v`, view)
	}
	SetModelMap(map[string]string{"*": ""})
	if view := ModelMapView(); len(view) != 0 {
		t.Errorf(`SetModelMap({"*":""}) 应剔除空值，got %v`, view)
	}

	// "*"→某模型 必须保留：未命中精确键时生效，命中精确键的仍以精确为准。
	SetModelMap(map[string]string{"*": "glm-5.3", "gpt-4o": "cn:glm-5.2"})
	if got := ModelMapView()["*"]; got != "glm-5.3" {
		t.Errorf(`SetModelMap 应保留兜底键 "*"→"glm-5.3"，got %q`, got)
	}
	if got := applyModelMap("gpt-4o-typo"); got != "glm-5.3" {
		t.Errorf("兜底未生效: applyModelMap(gpt-4o-typo)=%q want glm-5.3", got)
	}
	if got := applyModelMap("gpt-4o"); got != "cn:glm-5.2" {
		t.Errorf("精确匹配被兜底覆盖: applyModelMap(gpt-4o)=%q want cn:glm-5.2", got)
	}
}

// TestModelMapViewFallback 面板读写视图：兜底键 "*" 以普通键形式出现在副本里
// （面板整表读写，无键过滤，无需特殊处理）。
func TestModelMapViewFallback(t *testing.T) {
	SetModelMap(map[string]string{"*": "glm-5.3", "gpt-4o": "cn:glm-5.2"})
	t.Cleanup(func() { SetModelMap(nil) })

	view := ModelMapView()
	if len(view) != 2 || view["*"] != "glm-5.3" || view["gpt-4o"] != "cn:glm-5.2" {
		t.Fatalf(`ModelMapView 应含兜底键，got %v`, view)
	}
	// 副本语义对 "*" 同样成立：改副本不影响生效表。
	view["*"] = "glm-9"
	if got := ModelMapView()["*"]; got != "glm-5.3" {
		t.Errorf(`view 副本修改泄漏: "*"=%q want glm-5.3`, got)
	}
}

// TestResolveModelWithFallback realm 前缀名在映射表（含兜底）下的行为：
// 映射发生在前缀解析之前——精确键（含 cn:/global: 前缀键）命中即整名替换，
// 前缀解析协议本身不变；未命中的前缀名被 "*" 整名替换，realm 随兜底值走。
func TestResolveModelWithFallback(t *testing.T) {
	cases := []struct {
		name      string
		table     map[string]string
		in        string
		wantRealm string
		wantBare  string
	}{
		{"cn: 前缀精确键命中", map[string]string{"cn:glm-5.2": "cn:glm-5.3", "*": "x"}, "cn:glm-5.2", "cn", "glm-5.3"},
		{"global: 前缀精确键命中（值裸名回落 CN）", map[string]string{"global:gpt-5.4": "gpt-5.5", "*": "x"}, "global:gpt-5.4", "cn", "gpt-5.5"},
		{"前缀名未命中走兜底且 realm 随兜底值", map[string]string{"*": "global:gpt-5.4"}, "cn:glm-5.2", "global", "gpt-5.4"},
		{"无兜底时前缀名未命中不变", map[string]string{"cn:glm-5.2": "glm-5.3"}, "global:gpt-5.4", "global", "gpt-5.4"},
		{"非枚举前缀名未命中同样被兜底接住", map[string]string{"*": "glm-5.3"}, "deepseek:v3", "cn", "glm-5.3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			SetModelMap(c.table)
			t.Cleanup(func() { SetModelMap(nil) })
			realm, bare := resolveModel(c.in)
			if realm != c.wantRealm || bare != c.wantBare {
				t.Errorf("resolveModel(%q)=(%q,%q) want (%q,%q) (table=%v)",
					c.in, realm, bare, c.wantRealm, c.wantBare, c.table)
			}
		})
	}
}
