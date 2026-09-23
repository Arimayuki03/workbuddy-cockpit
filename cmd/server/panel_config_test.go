package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

	// restartRequired 含 listen / auth_dir / global.enabled 等装配期字段；
	// schedule.*_hours 自 SetHours 热改后不再是重启项。
	found := map[string]bool{}
	for _, f := range restart {
		found[f] = true
	}
	for _, want := range []string{"listen", "global.enabled"} {
		if !found[want] {
			t.Errorf("restartRequired missing %q; got %v", want, restart)
		}
	}
	for _, gone := range []string{"schedule.checkin_hours", "schedule.queue_hours"} {
		if found[gone] {
			t.Errorf("restartRequired must no longer contain %q (hours hot-reload); got %v", gone, restart)
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

// TestPanelSaveConfigHoursHotApply 验证面板提交 schedule.checkin_hours 后：
// 落盘保留数组形态 + scheduler 小时表热生效（SnapshotAll 立即回新值，无需重启）。
// 回归：SetHours 引入前排程小时数组只在启动期装配，面板改了也不生效。
func TestPanelSaveConfigHoursHotApply(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	if err := os.WriteFile(fp, []byte(`{"api_key":"k1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	live := livecfg.New(livecfg.Snapshot{APIKey: "k1"})
	p := pool.New("")
	up := upstream.New()
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})

	if _, err := saveConfig([]byte(`{"schedule":{"checkin_hours":[7,12,23]}}`), fp, live, p, up, sch); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	var got map[string]any
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("config not valid json: %v", err)
	}
	schedSec, _ := got["schedule"].(map[string]any)
	hours, _ := schedSec["checkin_hours"].([]any)
	if len(hours) != 3 || hours[0] != float64(7) || hours[2] != float64(23) {
		t.Errorf("schedule.checkin_hours = %v, want [7,12,23]", schedSec["checkin_hours"])
	}

	// 热生效：调度器快照回新小时表（找不到 7/12/23 即未生效）。
	for _, ts := range sch.SnapshotAll() {
		if ts.Kind != "checkin" {
			continue
		}
		if len(ts.Hours) != 3 || ts.Hours[0] != 7 || ts.Hours[2] != 23 {
			t.Errorf("scheduler snapshot checkin hours = %v, want [7,12,23] (hot-applied)", ts.Hours)
		}
		if ts.Enabled && !strings.HasPrefix(ts.NextFire, "") && ts.NextFire == "" {
			t.Errorf("enabled task must carry next_fire")
		}
	}
}

// TestPanelSaveModelMapReplace 端到端验证模型映射保存链路（回归：面板删除
// 映射条目后写盘、重启后不复活）。model_map 是整段替换语义——面板提交的表
// 就是用户想要的最终表；若被当作普通嵌套段深合并，被删条目会从旧值合回来，
// 删除永远无法落盘。
func TestPanelSaveModelMapReplace(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	orig := `{
  "api_key": "k1",
  "model_map": {"gpt-4o": "cn:glm-5.2", "keep-me": "cn:glm-5.3"},
  "unknown_user_key": {"keep": true}
}`
	if err := os.WriteFile(fp, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}

	live := livecfg.New(livecfg.Snapshot{APIKey: "k1"})
	p := pool.New("")
	up := upstream.New()
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})

	// 面板提交：只保留 keep-me（等价于 UI 上删除 gpt-4o 条目 + 新增 new-map）。
	if err := saveModelMap(
		map[string]string{"keep-me": "cn:glm-5.3", "new-map": "cn:auto"},
		fp, live, p, up, sch,
	); err != nil {
		t.Fatalf("saveModelMap: %v", err)
	}

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	mm, _ := got["model_map"].(map[string]any)
	if mm == nil {
		t.Fatalf("model_map missing after save: %s", raw)
	}
	// 被删条目不得从旧值合回来（深合并回归点）。
	if _, ok := mm["gpt-4o"]; ok {
		t.Errorf("deleted entry %q resurrected by deep merge; model_map = %v", "gpt-4o", mm)
	}
	if mm["keep-me"] != "cn:glm-5.3" || mm["new-map"] != "cn:auto" {
		t.Errorf("model_map = %v, want keep-me/new-map kept and added", mm)
	}
	// 其余段落不受整段替换影响：未知键与未提交键原样。
	if _, ok := got["unknown_user_key"]; !ok {
		t.Error("unknown user key must survive model_map save")
	}
	if got["api_key"] != "k1" {
		t.Errorf("api_key = %v, want k1 (untouched)", got["api_key"])
	}
}
