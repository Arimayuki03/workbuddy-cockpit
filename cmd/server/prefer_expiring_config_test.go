// prefer_expiring_config_test.go pool.prefer_expiring 配置测试（v1.20.0）：
// 默认 true / 文件显式 true|false / 键缺席保持默认 / env 覆盖（含非法值静默保留）。
//
// 表驱动（同 config_test.go 的既有风格）：每个用例只声明"输入 → 期望取值"，
// 解析路径（文件 Load / env 覆盖）由用例自带的 setup 决定。
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPreferExpiringConfig(t *testing.T) {
	cases := []struct {
		name string
		// file 非空时写入临时 config.json（键缺席用例显式留空 → 不写该键）；
		// env 非空时以 WB2A_POOL_PREFER_EXPIRING 覆盖。
		file string
		env  string
		want bool
	}{
		{
			// 键缺席：Default 已置 true，Load 的 json.Unmarshal 不覆盖缺失键 → 保持 true。
			name: "键缺席默认开启",
			file: `{"api_key":"k"}`,
			want: true,
		},
		{
			name: "文件显式 false 恢复旧行为",
			file: `{"api_key":"k","pool":{"prefer_expiring":false}}`,
			want: false,
		},
		{
			name: "文件显式 true",
			file: `{"api_key":"k","pool":{"prefer_expiring":true}}`,
			want: true,
		},
		{
			name: "env false 覆盖文件 true",
			file: `{"api_key":"k","pool":{"prefer_expiring":true}}`,
			env:  "false",
			want: false,
		},
		{
			name: "env true 覆盖文件 false",
			file: `{"api_key":"k","pool":{"prefer_expiring":false}}`,
			env:  "true",
			want: true,
		},
		{
			// 非法 env 静默保留文件值（与既有布尔覆盖同风格：不因坏 env 阻塞启动）。
			name: "非法 env 保留文件值",
			file: `{"api_key":"k","pool":{"prefer_expiring":false}}`,
			env:  "maybe",
			want: false,
		},
		{
			name: "非法 env 保留默认值",
			file: `{"api_key":"k"}`,
			env:  "maybe",
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fp := filepath.Join(t.TempDir(), "c.json")
			if err := os.WriteFile(fp, []byte(tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.env != "" {
				t.Setenv("WB2A_POOL_PREFER_EXPIRING", tc.env)
			}
			c, err := Load(fp)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if c.Pool.PreferExpiring != tc.want {
				t.Errorf("pool.prefer_expiring=%v want %v", c.Pool.PreferExpiring, tc.want)
			}
		})
	}
}

// TestPreferExpiringDefault 纯默认（无文件）：Default 与 normalize 后都必须是开启
// ——"忘了开"会让奖励积分白白作废（不可逆），比"抽签略偏好"的代价大得多。
func TestPreferExpiringDefault(t *testing.T) {
	c := Default()
	c.APIKey = "k" // api_key 无条件必填：normalize 校验需要非空 key
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !c.Pool.PreferExpiring {
		t.Error("pool.prefer_expiring 默认应为 true（Default + normalize 后）")
	}
}
