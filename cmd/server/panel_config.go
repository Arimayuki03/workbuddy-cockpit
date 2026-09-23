// panel_config.go 面板保存配置（panel 移植件，v1.2.0 设计文档 §3.6）：
// 校验 → 落盘 → 热应用 → 返回需重启的字段列表。
//
// 热生效范围（设计取舍）：
//   - api_key / cooldown.soft_rate / features.sanitize_blacklist_fingerprints → livecfg 快照
//   - pool.* → pool.SetBreaker/SetMaxInFlight/SetSoftRateMax/SetWeights/SetCostExploreInterval/SetDegrade
//   - schedule.*_enabled → scheduler.SetEnabled；schedule.*_hours → scheduler.SetHours
//     （两者均热生效免重启；normalize 保证 hours 非空、SetHours 拒绝非法小时）
//   - model_map → server.SetModelMap（模型映射链头热替换）
//
// 需重启（监听地址、HTTP client 超时、auth_dir 等装配期依赖）：
//   - listen / auth_dir / state_file / upstream.* / upstash.* / session_sticky.*（TTL 类）/
//     global.enabled（auth 包全局闸装配期注入）
//
// 落盘：深合并保留未知键（用户手写注释性字段不丢失）+ tmp+rename 原子替换；
// 校验与启动同一套 Default+normalize（ParseConfigInto），失败直接返回、不落盘。
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"workbuddy2api/internal/livecfg"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/upstream"
)

// parseConfigInto 把 JSON 覆盖到 c 上并 normalize（不做 env、不读文件）。
// 与 Load 的文件分支同一套校验链（Default 预置缺省 → Unmarshal → normalize），
// 保证面板保存的配置与下次启动实际加载的行为一致。
func parseConfigInto(raw []byte, c *Config) (*Config, error) {
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// saveConfig 面板保存配置入口（panel.Config.SaveConfig 闭包的实现）。
// raw 是面板提交的配置 JSON（整体或仅含其管理的键——深合并都能正确处理）。
func saveConfig(raw []byte, path string, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler) ([]string, error) {
	// 1) 解析现有文件为 map（保留用户手写的未知键），再深合并面板提交的键。
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read current config: %w", err)
	}
	var cur, incoming map[string]any
	if err := json.Unmarshal(oldRaw, &cur); err != nil {
		cur = map[string]any{}
	}
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, fmt.Errorf("parse submitted config: %w", err)
	}
	merged := mergeConfigMaps(cur, incoming)

	// 2) 校验（与启动同一套 Default+normalize），失败直接返回、不落盘。
	newCfg, err := parseConfigInto(mergedJSON(merged), Default())
	if err != nil {
		return nil, err
	}

	// 3) 落盘（原子替换：tmp + rename）。
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, fmt.Errorf("replace config: %w", err)
	}

	// 4) 热应用：能立即生效的字段全部应用，并列出仍需重启的字段。
	live.Store(livecfg.Snapshot{
		APIKey:               newCfg.APIKey,
		SoftCooldown:         newCfg.SoftRateDur,
		SanitizeFingerprints: newCfg.Features.SanitizeBlacklistFingerprints,
	})
	// upstream.Client 热改普通字段（UA/版本段/归属名/脱敏/IP 透传）经 SetHotFields
	// 整体替换快照：headers.go 读侧走 HotFields() 同步读——直接写普通字段与出站
	// 路径的并发读构成数据竞争（audit P0），快照整体替换无半新半旧窗口。
	up.SetHotFields(upstream.HotFields{
		SanitizeFingerprints: newCfg.Features.SanitizeBlacklistFingerprints,
		UserAgent:            newCfg.Upstream.UserAgent,
		ClientName:           newCfg.Upstream.ClientName,
		ClientVersion:        newCfg.Upstream.ClientVersion,
		CliVersion:           newCfg.Upstream.CliVersion,
		DeviceToken:          newCfg.Upstream.DeviceToken,
		DeviceTokenFile:      newCfg.Upstream.DeviceTokenFile,
		PassthroughIP:        newCfg.Upstream.PassthroughIP,
	})
	p.SetBreaker(newCfg.Pool.BreakerThreshold, newCfg.BreakerCooldownDur, newCfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(newCfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(newCfg.Pool.MaxInFlightGlobal)
	p.SetDegrade(newCfg.Pool.DegradeThreshold, newCfg.DegradeCooldownDur, newCfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(newCfg.SoftRateMaxDur)
	p.SetCostExploreInterval(newCfg.CostExploreIntervalDur) // costTier 探索窗口热生效（0 关停）
	p.SetWeights(newCfg.Pool.IdleWeightPerHour, newCfg.Pool.IdleWeightMax)
	// 排程开关热改（主仓库排程开关经 SetEnabled）+ 触发小时热改（SetHours，
	// 通知 Run 主循环立即重排定时器——面板保存配置与 /admin PATCH hours 共用）。
	for _, kind := range scheduler.Kinds() {
		var enabled bool
		switch kind {
		case "checkin":
			enabled = newCfg.Schedule.CheckinEnabled
		case "travel":
			enabled = newCfg.Schedule.TravelEnabled
		case "activity":
			enabled = newCfg.Schedule.ActivityEnabled
		case "keepalive":
			enabled = newCfg.Schedule.KeepaliveEnabled
		case "school":
			enabled = newCfg.Schedule.SchoolEnabled
		case "cat":
			enabled = newCfg.Schedule.CatEnabled
		case "queue":
			enabled = newCfg.Schedule.QueueEnabled
		}
		_ = sch.SetEnabled(kind, enabled)
	}
	// hours 热改：normalize 保证小时数组非空（空数组回落默认），SetHours 校验 0-23。
	_ = sch.SetHours("checkin", newCfg.Schedule.CheckinHours)
	_ = sch.SetHours("travel", newCfg.Schedule.TravelHours)
	_ = sch.SetHours("activity", newCfg.Schedule.ActivityHours)
	_ = sch.SetHours("keepalive", newCfg.Schedule.KeepaliveHours)
	_ = sch.SetHours("school", newCfg.Schedule.SchoolHours)
	_ = sch.SetHours("cat", newCfg.Schedule.CatHours)
	_ = sch.SetHours("queue", newCfg.Schedule.QueueHours)

	return restartRequiredFields(newCfg), nil
}

// restartRequiredFields 返回本次改动中无法热生效、需要重启进程的字段名。
// 恒返回完整清单中的"与当前进程装配期依赖相关"的项——面板据此提示用户。
func restartRequiredFields(c *Config) []string {
	var out []string
	// 这些字段在进程内被监听地址/HTTP client/目录句柄等装配期对象捕获。
	if c.Listen != "" {
		out = append(out, "listen")
	}
	if c.AuthDir != "" {
		out = append(out, "auth_dir")
	}
	if c.StateFile != "" {
		out = append(out, "state_file")
	}
	out = append(out, "upstream.timeout_seconds", "upstream.header_timeout_seconds", "upstream.idle_timeout_seconds")
	if c.Upstash.URL != "" || c.Upstash.Token != "" {
		out = append(out, "upstash")
	}
	out = append(out, "session_sticky.ttl", "session_sticky.gc_interval")
	// global.enabled 在 auth.SetGlobalEnabled / handler GlobalEnabled / upstream.GlobalEnabled
	// 三处装配期注入；prompt 文本与 global base 同理。
	out = append(out, "global.enabled", "global.chat_base", "global.billing_base",
		"prompt.mode", "prompt.file")
	return out
}

// modelMapKey config.json 里的模型映射段名。它是**整段替换**语义：面板保存映射
// 时提交的是「用户想要的完整映射表」（前端 saveModelMap 提交全量 next，
// 删除条目的方式就是不带该键）——若走深合并，被删条目会从旧值里合回来，
// 删除永远无法落盘，重启后"已删"条目复活。
const modelMapKey = "model_map"

// mergeConfigMaps 把 incoming 深合并进 cur（原地），返回 cur。
// 对嵌套对象逐键覆盖而不是整体替换：面板表单只提交它管理的键，
// 未提交的兄弟键（含用户手写的未知键）保持原样。
// model_map 例外：整段替换（见 modelMapKey 注释）。
func mergeConfigMaps(cur, incoming map[string]any) map[string]any {
	for k, v := range incoming {
		if k != modelMapKey {
			if inMap, ok := v.(map[string]any); ok {
				if curMap, ok := cur[k].(map[string]any); ok {
					cur[k] = mergeConfigMaps(curMap, inMap)
					continue
				}
			}
		}
		cur[k] = v
	}
	return cur
}

// mergedJSON 把合并后的 map 序列化回 JSON（供 ParseConfigInto 校验）。
func mergedJSON(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// saveModelMap 把模型映射写回 config.json（panel.SetModelMap 端点复用 saveConfig
// 的合并/校验/落盘链路，只携带 model_map 一个键——model_map 为整段替换语义，
// 传入的表即最终落盘的表，深合并不会把已删条目合回来）。
// 热生效（server.SetModelMap）由端点先行调用，此处仅负责持久化。
func saveModelMap(m map[string]string, path string, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler) error {
	raw, err := json.Marshal(map[string]any{modelMapKey: m})
	if err != nil {
		return fmt.Errorf("marshal model_map: %w", err)
	}
	_, err = saveConfig(raw, path, live, p, up, sch)
	return err
}
