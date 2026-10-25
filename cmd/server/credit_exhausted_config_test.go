// credit_exhausted_config_test.go cooldown.credit_exhausted 配置测试（吸收
// workbuddy-manager 补丁 0004 / v1.0.92）：空串 = 旧行为（0，硬冷却到次日 04:00）；
// 非空必须是正时长，非法值在 normalize 阶段报错且点出键名；WB2A_CREDIT_EXHAUSTED
// 覆盖同一条校验链；面板保存链路（深合并写回 + SetCreditExhausted 热应用）不丢键。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/livecfg"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/upstream"
)

// TestCreditExhaustedConfigTable 一张表覆盖「键缺席 / 显式空串 / 合法值 / 非法值」：
// 空串 = 旧行为（0）；非空必须是**正**时长（"0s" 与负值同样非法：冷却立即到期等于
// 没冷却，静默放行会让"配置写错却毫无效果"，故 fail-fast）；非法值的错误必须点出
// 键名，用户才知道改哪（与 soft_rate 的解析/校验同风格）。
func TestCreditExhaustedConfigTable(t *testing.T) {
	cases := []struct {
		name    string
		body    string // config.json 全文（"键缺席"用不含 credit_exhausted 的 body）
		want    time.Duration
		wantErr bool
	}{
		{
			name: "键缺席=旧行为",
			body: `{"api_key":"k","cooldown":{"soft_rate":"600s","soft_rate_max":"2h"}}`,
			want: 0,
		},
		{
			name: "显式空串=旧行为",
			body: `{"api_key":"k","cooldown":{"credit_exhausted":""}}`,
			want: 0,
		},
		{
			name: "合法值10m",
			body: `{"api_key":"k","cooldown":{"credit_exhausted":"10m"}}`,
			want: 10 * time.Minute,
		},
		{
			name: "合法值30s",
			body: `{"api_key":"k","cooldown":{"credit_exhausted":"30s"}}`,
			want: 30 * time.Second,
		},
		{
			name:    "非法值-不是时长",
			body:    `{"api_key":"k","cooldown":{"credit_exhausted":"不是时长"}}`,
			wantErr: true,
		},
		{
			name:    "非法值-负时长",
			body:    `{"api_key":"k","cooldown":{"credit_exhausted":"-5m"}}`,
			wantErr: true,
		},
		{
			name:    "非法值-零时长",
			body:    `{"api_key":"k","cooldown":{"credit_exhausted":"0s"}}`,
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fp := filepath.Join(t.TempDir(), "c.json")
			if err := os.WriteFile(fp, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(fp)
			if c.wantErr {
				if err == nil {
					t.Fatalf("非法值必须报错：%s", c.body)
				}
				if !strings.Contains(err.Error(), "cooldown.credit_exhausted") {
					t.Errorf("错误信息必须点出键名 cooldown.credit_exhausted：%v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.CreditExhaustedDur != c.want {
				t.Errorf("CreditExhaustedDur=%v want %v", cfg.CreditExhaustedDur, c.want)
			}
		})
	}
}

// TestCreditExhaustedEnvOverride WB2A_CREDIT_EXHAUSTED 与文件同链路：
// env 覆盖文件值、坏 env 同样在 normalize 阶段拦下（env 不能绕过校验）。
func TestCreditExhaustedEnvOverride(t *testing.T) {
	// 1) 无文件 + env → 生效。
	t.Setenv("WB2A_API_KEY", "k")
	t.Setenv("WB2A_CREDIT_EXHAUSTED", "10m")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CreditExhaustedDur != 10*time.Minute {
		t.Errorf("WB2A_CREDIT_EXHAUSTED 未生效：%v", cfg.CreditExhaustedDur)
	}

	// 2) env 覆盖文件值（文件 1m，env 10m → 取 env）。
	fp := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(fp, []byte(`{"api_key":"k","cooldown":{"credit_exhausted":"1m"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = Load(fp); err != nil {
		t.Fatalf("Load(file): %v", err)
	}
	if cfg.CreditExhaustedDur != 10*time.Minute {
		t.Errorf("env 应覆盖文件值，got %v", cfg.CreditExhaustedDur)
	}

	// 3) 坏 env → 启动失败并点出键名（与文件路径同一套校验）。
	t.Setenv("WB2A_CREDIT_EXHAUSTED", "-5m")
	_, err = Load("")
	if err == nil {
		t.Fatal("坏 env 必须报错")
	}
	if !strings.Contains(err.Error(), "cooldown.credit_exhausted") {
		t.Errorf("坏 env 的报错必须点出键名：%v", err)
	}
}

// TestCreditExhaustedPanelSavePersistsAndHotApplies 面板保存链路不丢新键：
// cooldown 段是普通嵌套对象，credit_exhausted 经深合并写回（与未提交的兄弟键
// soft_rate_max 并存、互不洗掉），并经 pool.SetCreditExhausted 热生效（免重启）；
// 非法值走与启动同一条 normalize 链，被拒且不落盘。
func TestCreditExhaustedPanelSavePersistsAndHotApplies(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "config.json")
	orig := `{"api_key":"k1","cooldown":{"soft_rate":"600s","soft_rate_max":"2h"}}`
	if err := os.WriteFile(fp, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	live := livecfg.New(livecfg.Snapshot{APIKey: "k1"})
	p := pool.New("")
	up := upstream.New()
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	// 账号入池：用冷却类型/剩余秒数观察热生效（不为测试新增 getter）。
	p.Add(&auth.Auth{UID: "u1"})

	// 面板提交：只带它管理的键（soft_rate 与新的 credit_exhausted）。
	if _, err := saveConfig([]byte(`{"cooldown":{"soft_rate":"300s","credit_exhausted":"10m"}}`), fp, live, p, up, sch, nil); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	// 1) 落盘：新键进 cooldown 段，未提交兄弟键保留。
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("落盘不是合法 JSON：%v", err)
	}
	cooldownSec, _ := got["cooldown"].(map[string]any)
	if cooldownSec == nil {
		t.Fatalf("cooldown 段丢失：%s", raw)
	}
	if cooldownSec["credit_exhausted"] != "10m" {
		t.Errorf("cooldown.credit_exhausted = %v want 10m（面板保存丢键）", cooldownSec["credit_exhausted"])
	}
	if cooldownSec["soft_rate_max"] != "2h" {
		t.Errorf("未提交兄弟键被洗掉：soft_rate_max = %v", cooldownSec["soft_rate_max"])
	}

	// 2) 热生效：保存后立即按新时长软冷却（无需重启）。
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if st.CoolKind != "soft_rate" {
		t.Errorf("cool_kind=%q want soft_rate（10m 软冷却热生效）", st.CoolKind)
	}
	if st.CoolRemaining < 9*60 || st.CoolRemaining > 10*60 {
		t.Errorf("cool_remaining=%ds want ≈600（10m）", st.CoolRemaining)
	}

	// 3) 非法值被拒且不落盘（文件保持上一版）。
	if _, err := saveConfig([]byte(`{"cooldown":{"credit_exhausted":"不是时长"}}`), fp, live, p, up, sch, nil); err == nil {
		t.Fatal("非法 credit_exhausted 必须被拒")
	}
	raw2, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	var got2 map[string]any
	if err := json.Unmarshal(raw2, &got2); err != nil {
		t.Fatal(err)
	}
	if cd2, _ := got2["cooldown"].(map[string]any); cd2["credit_exhausted"] != "10m" {
		t.Errorf("失败的保存不得改盘：credit_exhausted = %v", cd2["credit_exhausted"])
	}
}
