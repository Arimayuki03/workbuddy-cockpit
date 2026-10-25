package upstream

import "testing"

func TestEffectiveReasoningEffort(t *testing.T) {
	c := &Client{}
	// 与手机实测一致的桶：v4.1-flash 支持 low/high/max，默认 high。
	c.storeEfforts("cn",
		map[string][]string{"deepseek-v4.1-flash": {"low", "high", "max"}},
		map[string]string{"deepseek-v4.1-flash": "high"})
	// 默认档本身不支持时的降级链：注入 xhigh → 落到 ≤xhigh 的最高支持档 high。
	c.storeEfforts("global",
		map[string][]string{"deepseek-v4-pro": {"high", "xhigh"}},
		map[string]string{"deepseek-v4-pro": "high"})

	cases := []struct {
		name  string
		body  string
		realm string
		want  string
	}{
		{"缺 effort 补默认档", `{"model":"deepseek-v4.1-flash","messages":[]}`, "cn", "high"},
		{"camel 键同认", `{"model":"deepseek-v4.1-flash","reasoningEffort":"max"}`, "cn", "max"},
		{"不支持档降级", `{"model":"deepseek-v4.1-flash","reasoning_effort":"xhigh"}`, "cn", "high"},
		{"支持档原样", `{"model":"deepseek-v4.1-flash","reasoning_effort":"low"}`, "cn", "low"},
		{"thinking disabled 删档", `{"model":"deepseek-v4.1-flash","reasoning_effort":"high","thinking":{"type":"disabled"}}`, "cn", ""},
		{"非 deepseek 透传", `{"model":"glm-5.3-flash","reasoning_effort":"high"}`, "cn", "high"},
		{"非 deepseek 无档", `{"model":"glm-5.3-flash"}`, "cn", ""},
		{"global 桶独立", `{"model":"deepseek-v4-pro"}`, "global", "high"},
		{"坏 body", `not-json`, "cn", ""},
	}
	for _, tc := range cases {
		if got := c.EffectiveReasoningEffort([]byte(tc.body), tc.realm); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}
