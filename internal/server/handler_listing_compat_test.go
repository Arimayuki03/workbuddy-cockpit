package server

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestModelListCarriesMagpieCompatFields /v1/models 要能同时喂第三方目录客户端
// （issue #156：Magpie 只读 modalities.input 与 supported_reasoning_levels）。
//
// 我们的 supports_images / reasoning_supported_efforts 是既有约定，不能改；这里钉住
// 「额外补的兼容字段必须跟既有字段一一对应」，并且**全表**都成立 —— 以后加模型、
// 换目录时不会漏。
func TestModelListCarriesMagpieCompatFields(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, fullFieldsModelsBody, false
	})
	resetModelsCache()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: false})

	var hy3 map[string]any
	for _, m := range h.modelList() {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if obj["supports_images"] == true {
			mod, _ := obj["modalities"].(map[string]any)
			inputs, _ := mod["input"].([]any)
			hasImage := false
			for _, v := range inputs {
				if v == "image" {
					hasImage = true
				}
			}
			if !hasImage {
				t.Errorf("%v supports_images=true 却没有 modalities.input 含 image：%v", obj["id"], obj["modalities"])
			}
		}
		if efforts, ok := obj["reasoning_supported_efforts"].([]any); ok && len(efforts) > 0 {
			levels, ok := obj["supported_reasoning_levels"].([]any)
			if !ok || len(levels) != len(efforts) {
				t.Errorf("%v supported_reasoning_levels=%v 与 efforts=%v 对不上", obj["id"], obj["supported_reasoning_levels"], efforts)
				continue
			}
			for i, lv := range levels {
				lm, _ := lv.(map[string]any)
				if lm["effort"] != efforts[i] {
					t.Errorf("%v 第 %d 档 effort=%v，efforts=%v", obj["id"], i, lm["effort"], efforts[i])
				}
			}
		}
		if id, _ := obj["id"].(string); id == "cn:hy3" {
			hy3 = obj
		}
	}
	if hy3 == nil {
		t.Fatalf("fixture 里的 cn:hy3 没出现在列表里 —— 用例前提不成立")
	}
	if got := hy3["modalities"]; !reflect.DeepEqual(got, map[string]any{"input": []any{"text", "image"}}) {
		t.Errorf("cn:hy3 modalities=%v want {input:[text image]}", got)
	}
	want := []any{map[string]any{"effort": "low"}, map[string]any{"effort": "high"}}
	if got := hy3["supported_reasoning_levels"]; !reflect.DeepEqual(got, want) {
		t.Errorf("cn:hy3 supported_reasoning_levels=%v want %v", got, want)
	}
	if hy3["supports_images"] != true || hy3["reasoning_supported_efforts"] == nil {
		t.Error("既有字段被动了 —— 兼容字段只能新增，不能替换")
	}
}

// TestModelListGlobalBranchCarriesCompatFields global 分支（GlobalEnabled=true +
// /v2 探测命中富条目）同样要过 addListingCompat：modelList 有两条 entry 构建路径，
// 漏掉任一条，第三方目录客户端就会「因模型来自哪个域」而丢能力字段（静默降级，
// 比全丢更难查）。本用例钉住 global: 条目既有兼容字段、又有 cn:/global: 前缀。
func TestModelListGlobalBranchCarriesCompatFields(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	cf := newGlobalModelsHandlerFake(t, 200, fullFieldsModelsBody)
	p := testPoolWith(&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	var hy3 map[string]any
	for _, m := range h.modelList() {
		if id, _ := m["id"].(string); id == "global:hy3" {
			hy3 = m
		}
	}
	if hy3 == nil {
		t.Fatalf("fixture 里的 global:hy3 没出现在列表里 —— 用例前提不成立")
	}
	if hy3["supports_images"] != true {
		t.Fatalf("global:hy3 supports_images=%v want true（既有字段不被兼容层改动）", hy3["supports_images"])
	}
	if got := hy3["modalities"]; !reflect.DeepEqual(got, map[string]any{"input": []string{"text", "image"}}) {
		t.Errorf("global:hy3 modalities=%v want {input:[text image]}", got)
	}
	want := []map[string]any{{"effort": "low"}, {"effort": "high"}}
	if got := hy3["supported_reasoning_levels"]; !reflect.DeepEqual(got, want) {
		t.Errorf("global:hy3 supported_reasoning_levels=%v want %v", got, want)
	}
	// 前缀是网关路由协议（区分 cn/global 两版），兼容字段不得改变 id。
	if id, _ := hy3["id"].(string); !strings.HasPrefix(id, "global:") {
		t.Errorf("global:hy3 id 前缀被抹掉: %v", hy3["id"])
	}
}
