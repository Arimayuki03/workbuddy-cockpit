package upstream

import (
	"reflect"
	"testing"
)

// TestEffortListingRemoteWins 远端已解析档位为权威：无视静态表，直接用 remoteEfforts。
func TestEffortListingRemoteWins(t *testing.T) {
	efforts, def := EffortListing("cn", "glm-5.2",
		[]string{"low", "high"}, "low")
	if !reflect.DeepEqual(efforts, []string{"low", "high"}) {
		t.Errorf("efforts=%v want [low high] (remote authoritative)", efforts)
	}
	if def != "low" {
		t.Errorf("defaultEffort=%q want low (remote default, ∈ efforts)", def)
	}
}

// TestEffortListingStaticFallback 远端无档位 → 落到 CN 静态兜底表（照抄参考仓库档位）。
func TestEffortListingStaticFallback(t *testing.T) {
	cases := []struct {
		realm, model string
		wantEfforts  []string
		wantDefault  string
	}{
		{"cn", "deepseek-v4.1-flash", []string{"low", "high", "max"}, "high"},
		{"cn", "deepseek-v4-pro", []string{"low", "high", "xhigh"}, "high"},
		{"cn", "glm-5.3", []string{"low", "high", "max"}, "high"},
		{"cn", "glm-5.2", []string{"high", "xhigh"}, "high"},
		{"cn", "hy4-preview", []string{"high"}, "high"},
		{"", "deepseek-v4.1-flash", []string{"low", "high", "max"}, "high"}, // 空 realm 视作 cn
	}
	for _, c := range cases {
		efforts, def := EffortListing(c.realm, c.model, nil, "")
		if !reflect.DeepEqual(efforts, c.wantEfforts) {
			t.Errorf("%s/%s: efforts=%v want %v", c.realm, c.model, efforts, c.wantEfforts)
		}
		if def != c.wantDefault {
			t.Errorf("%s/%s: defaultEffort=%q want %q", c.realm, c.model, def, c.wantDefault)
		}
	}
}

// TestEffortListingRealmSplit issue #84 核心：同模型 deepseek-v4.1-flash 在两个 realm 档位刻意不同。
// global 只有 ['high']（WorkBuddy 国际版实测），不能沿用 CN 三档。
func TestEffortListingRealmSplit(t *testing.T) {
	cnEfforts, cnDef := EffortListing("cn", "deepseek-v4.1-flash", nil, "")
	if !reflect.DeepEqual(cnEfforts, []string{"low", "high", "max"}) || cnDef != "high" {
		t.Errorf("cn deepseek-v4.1-flash=%v/%q want [low high max]/high", cnEfforts, cnDef)
	}
	gEfforts, gDef := EffortListing("global", "deepseek-v4.1-flash", nil, "")
	if !reflect.DeepEqual(gEfforts, []string{"high"}) {
		t.Errorf("global deepseek-v4.1-flash=%v want [high]", gEfforts)
	}
	if gDef != "" {
		t.Errorf("global deepseek-v4.1-flash defaultEffort=%q want empty (无 defaultReasoningEffort)", gDef)
	}
}

// TestEffortListingUnknownOmits 三级皆无 → efforts 返回 nil（调用方省略字段，非空数组）。
func TestEffortListingUnknownOmits(t *testing.T) {
	efforts, def := EffortListing("cn", "no-such-model", nil, "")
	if efforts != nil {
		t.Errorf("efforts=%v want nil (unknown model → omit field)", efforts)
	}
	if def != "" {
		t.Errorf("defaultEffort=%q want empty", def)
	}
	// global 域未知模型同理。
	efforts, _ = EffortListing("global", "deep-model", nil, "")
	if efforts != nil {
		t.Errorf("global deep-model efforts=%v want nil (该模型无档位声明)", efforts)
	}
}

// TestEffortListingDefaultNotInEffortsOmitted defaultEffort 不在档位表时不得宣称
// （对齐参考仓库 resolveModel 的 `defaultEffort ∈ efforts` 防御）。
func TestEffortListingDefaultNotInEffortsOmitted(t *testing.T) {
	efforts, def := EffortListing("cn", "m", []string{"low", "high"}, "max")
	if !reflect.DeepEqual(efforts, []string{"low", "high"}) {
		t.Errorf("efforts=%v want [low high]", efforts)
	}
	if def != "" {
		t.Errorf("defaultEffort=%q want empty (max ∉ efforts)", def)
	}
}

// TestEffortListingDeepSeekV4FlashDefault CN 三档模型 deepseek-v4-flash 的静态默认档
// 与注释口径一致（defaultEffort 均为 high，档位含 high）。
func TestEffortListingDeepSeekV4FlashDefault(t *testing.T) {
	efforts, def := EffortListing("cn", "deepseek-v4-flash", nil, "")
	if !reflect.DeepEqual(efforts, []string{"low", "high", "max"}) {
		t.Errorf("efforts=%v want [low high max]", efforts)
	}
	if def != "high" {
		t.Errorf("defaultEffort=%q want high", def)
	}
}

// TestGlobalEffortMapDefaultOverrideGated globalEffortMap 的远端默认档覆盖与
// EffortListing 的跨源防御同口径：仅当远端也下发了该模型档位（remoteEfforts[id]
// 非空）才覆盖默认档，不得拼出「档位是静态、默认档是 remote」的矛盾组合。
func TestGlobalEffortMapDefaultOverrideGated(t *testing.T) {
	// deepseek-v4.1-flash global 静态档位 ['high'] 无默认档；远端只给了默认档
	// 没给档位 → 默认档不得落 defs（否则 high 之外无档位可回退，矛盾组合）。
	efforts, defs := globalEffortMap(
		nil,
		map[string]string{"deepseek-v4.1-flash": "max"},
	)
	if !sameStrings(efforts["deepseek-v4.1-flash"], []string{"high"}) {
		t.Errorf("static efforts must stay: %v want [high]", efforts["deepseek-v4.1-flash"])
	}
	if defs["deepseek-v4.1-flash"] != "" {
		t.Errorf("default without remote efforts must be dropped, got %q", defs["deepseek-v4.1-flash"])
	}

	// 远端档位+默认档成对下发 → 正常覆盖（静态表没有的远端新模型也成立）。
	efforts, defs = globalEffortMap(
		map[string][]string{"gpt-5.4": {"low", "high"}},
		map[string]string{"gpt-5.4": "low"},
	)
	if !sameStrings(efforts["gpt-5.4"], []string{"low", "high"}) {
		t.Errorf("remote efforts must override: %v want [low high]", efforts["gpt-5.4"])
	}
	if defs["gpt-5.4"] != "low" {
		t.Errorf("remote default with matching remote efforts must win, got %q", defs["gpt-5.4"])
	}
}
