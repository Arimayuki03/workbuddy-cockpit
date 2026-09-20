package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/livecfg"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/upstream"
)

// TestPanelSaveConfigMergePersist 端到端验证面板保存配置链路：
// 深合并保留未知键 → 原子写回 → 返回需重启字段。
func TestPanelSaveConfigMergePersist(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	orig := `{
  "api_key": "k1",
  "listen": ":9999",
  "user_custom_unknown_key": {"keep": true},
  "pool": {"max_in_flight": 1},
  "schedule": {"checkin_enabled": false}
}`
	if err := os.WriteFile(fp, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}

	live := livecfg.New(livecfg.Snapshot{APIKey: "k1"})
	p := pool.New("")
	up := upstream.New()
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})

	// 面板提交：只带它管理的键（api_key + soft_rate + schedule 开关）。
	submit := []byte(`{"api_key":"k2","cooldown":{"soft_rate":"300s"},"schedule":{"checkin_enabled":true}}`)
	restart, err := saveConfig(submit, fp, live, p, up, sch)
	if err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	// 落盘结果：未知键保留 + 提交键生效 + 未提交兄弟键原样。
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["api_key"] != "k2" {
		t.Errorf("api_key = %v, want k2", got["api_key"])
	}
	if _, ok := got["user_custom_unknown_key"]; !ok {
		t.Error("unknown user key must survive deep merge")
	}
	listen, _ := got["listen"].(string)
	if listen != ":9999" {
		t.Errorf("listen = %v, want :9999 (untouched sibling)", listen)
	}
	poolSec, _ := got["pool"].(map[string]any)
	if poolSec["max_in_flight"] != float64(1) {
		t.Errorf("pool.max_in_flight = %v, want 1 (untouched sibling)", poolSec["max_in_flight"])
	}
	schedSec, _ := got["schedule"].(map[string]any)
	if schedSec["checkin_enabled"] != true {
		t.Errorf("schedule.checkin_enabled = %v, want true", schedSec["checkin_enabled"])
	}
	// schedule.checkin_hours 等未提交键不被洗掉。
	if _, ok := schedSec["checkin_hours"]; ok {
		t.Errorf("unsubmitted schedule keys must not be added by panel (merge only)")
	}

	// livecfg 快照热生效。
	if live.Load().APIKey != "k2" {
		t.Errorf("live APIKey = %q, want k2 (hot-applied)", live.Load().APIKey)
	}
	if live.Load().SoftCooldown != 300e9 {
		t.Errorf("live SoftCooldown = %v, want 300s", live.Load().SoftCooldown)
	}

	// restartRequired 含 listen / auth_dir / schedule hours 等装配期字段。
	found := map[string]bool{}
	for _, f := range restart {
		found[f] = true
	}
	for _, want := range []string{"listen", "schedule.checkin_hours", "global.enabled"} {
		if !found[want] {
			t.Errorf("restartRequired missing %q; got %v", want, restart)
		}
	}

	// 校验失败不落盘：非法 soft_rate 被拒、文件保持上一版内容。
	if _, err := saveConfig([]byte(`{"cooldown":{"soft_rate":"bogus"}}`), fp, live, p, up, sch); err == nil {
		t.Fatal("invalid soft_rate must be rejected")
	}
	raw2, _ := os.ReadFile(fp)
	var got2 map[string]any
	_ = json.Unmarshal(raw2, &got2)
	if got2["api_key"] != "k2" {
		t.Errorf("failed save must not touch disk; api_key = %v", got2["api_key"])
	}
}
