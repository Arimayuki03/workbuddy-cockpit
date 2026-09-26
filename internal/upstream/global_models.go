// global 模型名目录探测：只产模型名，不产倍率（PLAN §3.D2「模型名目录 ≠ 倍率表」）。
//
// credits 数值一律不进入本包实现——探测端点即便返回倍率字段也忽略，名单只喂
// /v1/models 的 global: 前缀输出，不注入 costTier、不参与选号。
package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
)

// GlobalModelNames 国际版（global realm）历史静态名单（PLAN §7.2 附录 21 名）。
// 纯动态化后**不再作为模型目录的基底/兜底**：/v1/models 只透出上游实际下发的模型，
// 生产链路对本名单零引用。保留仅作历史对照（global e2e 观测日志差集参照）。
var GlobalModelNames = []string{
	"default-model",
	"fast-model",
	"balanced-model",
	"primary-model",
	"hy4-preview",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"deep-model",
	"deepseek-v4.1-flash",
	"gpt-6-astra",
	"hy4-preview-f",
	"hy3",
	"glm-5.2",
	"gpt-5.6-luna",
	"gpt-5.5",
	"gpt-5.4",
	"gpt-5.3-codex",
	"gemini-3.5-flash",
	"glm-5.3",
	"kimi-k3",
	"kimi-k2.6",
}

// fetchGlobalModelsCache 探测结果缓存（语义参照 CN 侧 handler.dynamicModelsCache：1h TTL +
// 5min 失败负缓存）。按 Client 实例持有（effortsMu 同模式），测试新建 Client 即隔离。
// Mutex 内嵌，与 modelList 无并发读路径竞争（唯一读写点本文件内）。
type fetchGlobalModelsCache struct {
	sync.Mutex
	names    []string    // 成功缓存：探测结果（已去重）；nil = 未探测/失败
	infos    []ModelInfo // 成功缓存：探测对象形态的全字段条目（窄表/失败形态为 nil）
	fetched  time.Time
	lastFail time.Time
}

// globalModelsTTL / globalModelsFailCooldown 探测缓存时长：成功 1h，失败 5min 负缓存。
const (
	globalModelsTTL          = time.Hour
	globalModelsFailCooldown = 5 * time.Minute
)

// v3 探测 UA（吸收 panel 分支 9dce68a 实测，2026-09-22）：
// /v3/config 对不同 User-Agent 下发**不同模型集合**——
//   - IDE UA（CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0）→ 模型少但单条字段更全
//     （响应体积更大；独有 o4-mini / enhance-1.0 / auto-chat，无 deepseek 系列）
//   - CLI UA（CLI/<ver> CodeBuddy/<ver> 三段式，即本网关默认出站 UA）→ 模型多
//     （实测含 deepseek-v4.1-flash / gpt-6-astra / kimi-k2.8-preview）
//
// 两路各有独有模型，故 global 侧 v3 主路并发两 UA 取并集（IDE 路字段权威、
// CLI 路补缺失 id）；单路失败降级另一路。CN 侧 FetchModels 的 v3 路保持
// CommonHeaders 默认 UA 单路不变（零回归）。
const v3ProbeIDEUA = "CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0"

// globalModelsProbePaths global 企业模型目录端点候选序列（按 realm 切 base，路径"家族"）：
// /v2 家族优先（PR #20 实测 /v2/enterprises/personal/models 200 含完整模型表），
// /console 作 fallback（同域旧路径，或 500）。参考 PLAN v1 §2.2 分歧③ 与
// rockswang/wild-work PR #20 实测结论：console 路径在 global 上非 200 → 先 /v2。
// v3-config-merge 后该家族降为企业补充路（/v3/config 为主路，与家族并发探测；
// gpt-5.3-codex 等家族独有模型经此进并集）。
var globalModelsProbePaths = []string{
	"/v2/enterprises/personal/models",
	"/console/enterprises/personal/models",
}

// FetchGlobalModels 探测 global 账号的模型名目录并返回模型名列表（含 context 无关、无倍率）。
//
// 纯动态：成功返回探测结果（去重），缓存 1h；失败（家族端点全非 2xx / 解析失败 /
// 空列表）记 5min 负缓存，返回 nil（无静态回落）。缓存/负缓存命中：直接返回，零上游调用。
//
// 调用方负责：仅在有 global 账号时调用（无则不探测）；GlobalEnabled 关闭时（逃生门）
// 不得调用——本方法由 globalOn(a) 内部兜底，若账号因开关回落 cn 则返回 nil。
func (c *Client) FetchGlobalModels(a *auth.Auth) []string {
	names, _ := c.fetchGlobalModelsOnce(a)
	return names
}

// FetchGlobalModelInfos 探测 global 账号的模型目录并返回全字段 ModelInfo 列表
// （global /v2 模型对象与 CN 同构，2026-09-15 真实账号 /v2 探测实证）。
// 与 FetchGlobalModels 共享同一次探测与缓存（names + infos 一体落缓存）：
// 对象形态 200 → 全字段条目；窄表形态 / 探测失败 / 负缓存 / 非 global 路由账号
// → nil（调用方按 ID 名单输出裸条目，不编造字段）。
// 账号因 GlobalEnabled 开关回落 cn 时不探测（globalOn 兜底，零上游调用）。
func (c *Client) FetchGlobalModelInfos(a *auth.Auth) []ModelInfo {
	_, infos := c.fetchGlobalModelsOnce(a)
	return infos
}

// GlobalModelInfosSnapshot 只读 global 模型目录全字段缓存（TTL 内快照）；
// 冷 / 过期 / 窄表形态 / 未探测 → nil。不发起任何上游探测——与
// FetchGlobalModelInfos 的差异点（那个在 miss 时触发探测，服务 /v1/models；
// 本方法服务 /v1/stats 的倍率透出，只读已有数据）。
// 返回 slices.Clone 浅拷贝：调用方 append/改写不会污染缓存 backing array。
func (c *Client) GlobalModelInfosSnapshot() []ModelInfo {
	c.globalModels.Lock()
	defer c.globalModels.Unlock()
	if len(c.globalModels.infos) == 0 || time.Since(c.globalModels.fetched) >= globalModelsTTL {
		return nil
	}
	return slices.Clone(c.globalModels.infos)
}

// fetchGlobalModelsOnce 单次探测决策（缓存命中/负缓存/触发探测），返回 (names, infos)。
// 纯动态：成功 = 探测结果去重（不与任何静态名单合并）；一切失败 = nil（不回落静态）。
// infos 仅对象形态成功探测时非 nil。
//
// 探测段带 singleflight 门闩（globalProbing + globalProbeDone，chan 自实现，无
// x/sync 依赖）：锁内查缓存/负缓存 miss 后，第一个进入者标记 probing 并持 done
// chan 锁外探测；并发进入者看到 probing 就等 done 关闭后重查缓存——probe 自身
// 已是 3 路并发，无门闩时 N 个 /v1/models 同时 miss 会 N 倍重复打上游。等待者
// 重查仍 miss（持门闩者失败且负缓存已写入的窗口）按 miss 口径返回 nil，下次
// TTL/冷却内已有结论。
func (c *Client) fetchGlobalModelsOnce(a *auth.Auth) (names []string, infos []ModelInfo) {
	if !c.globalOn(a) {
		// 逃生门兜底：账号不路由 global 上游 → 不探测（零上游调用）。
		return nil, nil
	}

	c.globalModels.Lock()
	if len(c.globalModels.names) > 0 && time.Since(c.globalModels.fetched) < globalModelsTTL {
		// 缓存命中：返回 slices.Clone 浅拷贝——共享 backing array 时调用方
		// append/改写会污染缓存（后续所有命中者都读到被改写的数据）。
		names, infos := slices.Clone(c.globalModels.names), slices.Clone(c.globalModels.infos)
		c.globalModels.Unlock()
		return names, infos
	}
	if !c.globalModels.lastFail.IsZero() && time.Since(c.globalModels.lastFail) < globalModelsFailCooldown {
		// 负缓存冷却期内：避免反复打上游，直接按失败处理（无静态回落）。
		c.globalModels.Unlock()
		return nil, nil
	}
	if c.globalProbing {
		// 已有探测进行中（singleflight 等待侧）：等持门闩者收尾后重查缓存。
		done := c.globalProbeDone
		c.globalModels.Unlock()
		if done != nil {
			<-done
		}
		c.globalModels.Lock()
		defer c.globalModels.Unlock()
		if len(c.globalModels.names) > 0 && time.Since(c.globalModels.fetched) < globalModelsTTL {
			return slices.Clone(c.globalModels.names), slices.Clone(c.globalModels.infos)
		}
		// 仍 miss（持门闩者失败，负缓存口径）：不重复探测（其已写 lastFail，
		// 下轮进入者会走负缓存分支），直接按失败返回。
		return nil, nil
	}
	// 第一个进入者：持门闩，锁外探测。
	c.globalProbing = true
	c.globalProbeDone = make(chan struct{})
	c.globalModels.Unlock()
	defer func() {
		c.globalModels.Lock()
		c.globalProbing = false
		close(c.globalProbeDone)
		c.globalProbeDone = nil
		c.globalModels.Unlock()
	}()

	names, infos, efforts, defaults, err := c.probeGlobalModels(a)
	if err != nil || len(names) == 0 {
		// 探测失败：负缓存 + 返回 nil（effort 桶不写，prepareBody 走 globalEffortMap 静态兜底）。
		c.globalModels.Lock()
		c.globalModels.lastFail = time.Now()
		c.globalModels.names = nil
		c.globalModels.infos = nil
		c.globalModels.Unlock()
		return nil, nil
	}
	// global 域 effort 能力：探测下发的 supportedEfforts/defaultEffort 权威写入 global 桶
	// （raw remote，不并入静态表——静态兜底在 prepareBody 的 globalEffortMap 与
	// /v1/models 的 EffortListing 里按需 fallback）。空探测不写（防清既有桶）。
	if len(efforts) > 0 || len(defaults) > 0 {
		c.storeEfforts("global", efforts, defaults)
	}

	// 成功：探测结果去重。names/infos 均落缓存；倍率等选号敏感字段只透出展示，
	// 不注入 costTier（§3.D2 不变）。
	seen := make(map[string]bool, len(names))
	merged := make([]string, 0, len(names))
	for _, id := range names {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		merged = append(merged, id)
	}

	c.globalModels.Lock()
	c.globalModels.names = merged
	c.globalModels.infos = infos
	c.globalModels.fetched = time.Now()
	c.globalModels.lastFail = time.Time{}
	c.globalModels.Unlock()
	// 返回值同样浅拷贝：merged/infos 即缓存本体，调用方 append/改写不得波及缓存。
	return slices.Clone(merged), slices.Clone(infos)
}

// probeGlobalModels 发起一次 global 模型目录探测（v3-config-merge）：
// /v3/config（主）与企业端点家族（/v2 → /console 兜底，补缺）**并发**探测后并集合并。
// 返回模型名列表（已合并、未再去重——去重在 fetchGlobalModelsOnce）、全字段
// ModelInfo（对象形态；窄表为 nil）及 effort 能力桶（supportedEfforts/defaultEffort，
// 可为空）。合并口径：v3 条目为主（credits 等字段以 v3 为准），企业端点只补 v3 缺失的
// 模型 id（如 gpt-5.3-codex 只在 /v2，作为补充进并集）；去重 key = 模型 id，输出顺序
// 稳定（v3 原序在前、企业端点补充项在后）。两路全失败才返回错误（等价原「家族端点全
// 非 2xx」负缓存语义）；单路失败降级为另一路结果 + warn 日志，互不拖累。
func (c *Client) probeGlobalModels(a *auth.Auth) (names []string, infos []ModelInfo, efforts map[string][]string, defaults map[string]string, err error) {
	type probeResult struct {
		names    []string
		infos    []ModelInfo
		efforts  map[string][]string
		defaults map[string]string
		err      error
	}
	// probeV3 单次 /v3/config 探测（UA 参数化）。该端点对不同 UA 下发**不同模型集合**
	// （见 v3ProbeIDEUA 注释），故 IDE/CLI 两路并发后并集合并。
	probeV3 := func(ua string) chan probeResult {
		ch := make(chan probeResult, 1)
		go func() {
			names, infos, efforts, defaults, perr := c.globalModelsOnceUA(a, v3ConfigPath, ua)
			ch <- probeResult{names, infos, efforts, defaults, perr}
		}()
		return ch
	}
	v3IDECh := probeV3(v3ProbeIDEUA)
	v3CLICh := probeV3("") // 空 = CommonHeaders 默认 UA（本网关三段式 CLI 形态）
	enterpriseCh := make(chan probeResult, 1)
	go func() {
		// 企业端点家族：/v2 首选 → /console 兜底（既有探活序，零回归）。
		var lastErr error
		for _, path := range globalModelsProbePaths {
			names, infos, efforts, defaults, perr := c.globalModelsOnce(a, path)
			if perr != nil {
				lastErr = perr
				continue
			}
			enterpriseCh <- probeResult{names, infos, efforts, defaults, nil}
			return
		}
		enterpriseCh <- probeResult{err: lastErr}
	}()
	v3IDE := <-v3IDECh
	v3CLI := <-v3CLICh
	enterprise := <-enterpriseCh
	// v3 两 UA 自合并：IDE 路字段权威（响应体积更大、单条字段更全），CLI 路只补
	// 缺失的模型 id（deepseek 系列等）。单路成功即用该路；两路全失败才带 err
	// 进入下游降级判断。
	var v3 probeResult
	switch {
	case v3IDE.err != nil && v3CLI.err != nil:
		v3 = probeResult{err: v3IDE.err}
	case v3IDE.err != nil:
		log.Printf("WARN: [upstream] global models: v3/config IDE-UA probe failed (default-UA only): %v", v3IDE.err)
		v3 = v3CLI
	case v3CLI.err != nil:
		log.Printf("WARN: [upstream] global models: v3/config default-UA probe failed (IDE-UA only): %v", v3CLI.err)
		v3 = v3IDE
	default:
		vn, vi := mergeGlobalCatalog(v3IDE.names, v3IDE.infos, v3CLI.names, v3CLI.infos)
		efforts := mergeEffortBuckets(v3IDE.efforts, v3CLI.efforts)
		defaults := mergeEffortDefaults(v3IDE.defaults, v3CLI.defaults)
		v3 = probeResult{names: vn, infos: vi, efforts: efforts, defaults: defaults}
	}

	if v3.err != nil && enterprise.err != nil {
		// 两路全失败 → 负缓存语义（等价原家族端点全非 2xx）。
		return nil, nil, nil, nil, v3.err
	}
	if v3.err != nil {
		// /v3 失败降级：不拖累企业端点结果（任务书实现要点：降级仅企业端点 + warn）。
		log.Printf("WARN: [upstream] global models: v3/config probe failed (degraded to enterprise endpoint): %v", v3.err)
		return enterprise.names, enterprise.infos, enterprise.efforts, enterprise.defaults, nil
	}
	if enterprise.err != nil {
		log.Printf("WARN: [upstream] global models: enterprise endpoint failed (v3/config only): %v", enterprise.err)
		return v3.names, v3.infos, v3.efforts, v3.defaults, nil
	}
	// 两路皆成功：v3 为主、企业端点补缺合并。
	names, infos = mergeGlobalCatalog(v3.names, v3.infos, enterprise.names, enterprise.infos)
	efforts = mergeEffortBuckets(v3.efforts, enterprise.efforts)
	defaults = mergeEffortDefaults(v3.defaults, enterprise.defaults)
	return names, infos, efforts, defaults, nil
}

// mergeGlobalCatalog 两路合并（v3 主、企业补缺）：names 按 id 去重（v3 原序在前、
// 企业端点补充项在其原序后追加——稳定输出）；infos 同步合并（v3 条目字段权威，
// 企业端点条目只在 id 缺失时进并集，其 credits 为 v2 原值）。
// 窄表形态（infos nil）时保持 nil——无对象字段不编造。
func mergeGlobalCatalog(primaryNames []string, primaryInfos []ModelInfo, secondaryNames []string, secondaryInfos []ModelInfo) (names []string, infos []ModelInfo) {
	if len(secondaryNames) == 0 {
		return primaryNames, primaryInfos
	}
	seen := make(map[string]bool, len(primaryNames)+len(secondaryNames))
	out := make([]string, 0, len(primaryNames)+len(secondaryNames))
	for _, id := range primaryNames {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	var outInfos []ModelInfo
	if primaryInfos != nil {
		outInfos = make([]ModelInfo, 0, len(primaryInfos)+len(secondaryInfos))
		for _, mi := range primaryInfos {
			outInfos = append(outInfos, mi)
		}
	}
	for _, id := range secondaryNames {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		// 窄表企业响应（secondaryInfos nil / 超出条目数）时该 id 无对象字段，
		// infos 保持原样（调用方按 id 名单输出裸条目，不编造字段）。
		for _, mi := range secondaryInfos {
			if mi.ID == id {
				outInfos = append(outInfos, mi)
				break
			}
		}
	}
	return out, outInfos
}

// mergeEffortBuckets 合并两路 effort 桶：主路（v3）权威，企业端点只补主路缺失的模型档位。
func mergeEffortBuckets(primary, secondary map[string][]string) map[string][]string {
	if len(secondary) == 0 {
		return primary
	}
	out := primary
	if out == nil {
		out = make(map[string][]string, len(secondary))
	}
	for id, v := range secondary {
		if _, ok := out[id]; !ok {
			out[id] = v
		}
	}
	return out
}

// mergeEffortDefaults 合并两路 defaultEffort：主路（v3）权威，企业端点只补缺失。
func mergeEffortDefaults(primary, secondary map[string]string) map[string]string {
	if len(secondary) == 0 {
		return primary
	}
	out := primary
	if out == nil {
		out = make(map[string]string, len(secondary))
	}
	for id, v := range secondary {
		if _, ok := out[id]; !ok {
			out[id] = v
		}
	}
	return out
}

// globalModelsOnce 单端点探测。2xx + 解析出非空名单 → (names, infos, efforts, defaults, nil)；否则 (nil,...,err)。
func (c *Client) globalModelsOnce(a *auth.Auth, path string) ([]string, []ModelInfo, map[string][]string, map[string]string, error) {
	return c.globalModelsOnceUA(a, path, "")
}

// globalModelsOnceUA 同 globalModelsOnce，ua 非空时覆盖 User-Agent（/v3/config 对
// UA 敏感且不同 UA 下发不同模型集合，见 v3ProbeIDEUA；空串走 CommonHeaders 默认）。
func (c *Client) globalModelsOnceUA(a *auth.Auth, path, ua string) ([]string, []ModelInfo, map[string][]string, map[string]string, error) {
	url := c.chatBase(a) + path // 按 realm 切 base：global 账号 → global base
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	c.CommonHeaders(req, a) // 共享请求头（Origin/Referer/UA），与 FetchModels 同款
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		// 读失败 → 传输层错误：半截 body 不进解析（探测负缓存走 lastFail，不罚号）。
		return nil, nil, nil, nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, nil, nil, fmt.Errorf("global models status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	return parseGlobalModelNames(raw)
}

// parseGlobalModelNames 容忍两种形态解析模型名：
//   - 对象数组：data.models[].id/.name（id 优先），disabled 剔除；
//   - 窄表：data 为字符串数组。
//
// 对象形态与 CN 模型对象同构（dynModelEntry 共用解析口径），额外产出全字段 ModelInfo
// 与 reasoning.supportedEfforts / defaultEffort（P0：global 域 effort 探测，解析不到时
// 调用方回落 staticEffortCap 兜底表——prepareBody 的 globalEffortMap 与 /v1/models 的
// EffortListing）。窄表形态无元数据 → infos/桶为空。
//
// 解析成功但名单为空 → 返回错误（调用方回落静态，等价"该端点没给全"）。
func parseGlobalModelNames(raw []byte) (names []string, infos []ModelInfo, efforts map[string][]string, defaults map[string]string, err error) {
	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("global models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, nil, nil, nil, fmt.Errorf("global models code=%d", env.Code)
	}
	trimmed := strings.TrimSpace(string(env.Data))
	if strings.HasPrefix(trimmed, "[") {
		// 窄表形态：data 为字符串数组（无 effort 元数据、无对象字段 → infos nil）。
		var arr []string
		if err := json.Unmarshal(env.Data, &arr); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("global models parse (narrow): %w", err)
		}
		out := make([]string, 0, len(arr))
		for _, id := range arr {
			if id = strings.TrimSpace(id); id != "" {
				out = append(out, id)
			}
		}
		if len(out) == 0 {
			return nil, nil, nil, nil, fmt.Errorf("global models empty list")
		}
		return out, nil, nil, nil, nil
	}
	// 对象形态：data.models[].id/.name（id 优先），disabled 剔除，全字段落 ModelInfo。
	// dynModelEntry 与 CN FetchModels 共用（两域模型对象同构），零解析口径漂移。
	// ModelTrialBanner：上游把「N 天免费试用」的模型只放在
	// data.productFeaturesConfig.ModelTrialBanner.banners[].modelId，不在 data.models
	// 里（实测 global 侧 hy4-preview-f 即如此：modelId=hy4-preview-f、
	// targetModelId=hy4-preview、trialDays=14），纯 data.models 解析会漏——但该模型
	// 实际可调用。元数据口径：能力字段从 targetModelId 的既有条目继承（试用版与转正
	// 目标是同族模型，能力一致）；Credits/Tags 显式清空（它们描述"转正后"的计费与
	// 营销信息，用在免费试用版上会误导下游展示）；firstUseTimeKey/trialDays 属账号级
	// 试用状态，不透出。CN 侧 data.models 已含同 id 时去重跳过，行为不变。
	var obj struct {
		Models                []dynModelEntry `json:"models"`
		ProductFeaturesConfig struct {
			ModelTrialBanner struct {
				Banners []struct {
					ModelID       string `json:"modelId"`
					TargetModelID string `json:"targetModelId"`
				} `json:"banners"`
			} `json:"ModelTrialBanner"`
		} `json:"productFeaturesConfig"`
	}
	if err := json.Unmarshal(env.Data, &obj); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("global models parse: %w", err)
	}
	out := make([]string, 0, len(obj.Models))
	infos = make([]ModelInfo, 0, len(obj.Models))
	for _, m := range obj.Models {
		id := m.ID
		if id == "" {
			id = m.Name
		}
		if id == "" || m.Disabled {
			continue
		}
		out = append(out, id)
		mi := m.modelInfo()
		mi.ID = id // name 兜底形态下 id 取自 name，对齐 names 输出
		infos = append(infos, mi)
		// effort 桶：supportedEfforts 数组优先；缺数组但 reasoning.effort 单档非空 → 视作单档表。
		if len(m.Reasoning.SupportedEfforts) > 0 {
			if efforts == nil {
				efforts = make(map[string][]string)
			}
			efforts[id] = m.Reasoning.SupportedEfforts
		} else if e := strings.TrimSpace(m.Reasoning.Effort); e != "" {
			if efforts == nil {
				efforts = make(map[string][]string)
			}
			efforts[id] = []string{e}
		}
		if d := strings.TrimSpace(m.Reasoning.DefaultEffort); d != "" {
			if defaults == nil {
				defaults = make(map[string]string)
			}
			defaults[id] = d
		}
	}
	// 补入试用横幅模型（ModelTrialBanner）：能力继承 targetModelId 条目、
	// Credits/Tags 清空（口径见上方结构体注释）。
	byID := make(map[string]ModelInfo, len(infos))
	for _, mi := range infos {
		byID[mi.ID] = mi
	}
	for _, b := range obj.ProductFeaturesConfig.ModelTrialBanner.Banners {
		id := strings.TrimSpace(b.ModelID)
		if id == "" {
			continue
		}
		if _, exists := byID[id]; exists {
			continue
		}
		mi := ModelInfo{ID: id}
		if tgt := strings.TrimSpace(b.TargetModelID); tgt != "" {
			if base, ok := byID[tgt]; ok {
				mi = base
				mi.ID = id
			}
		}
		mi.Credits = ""
		mi.Tags = nil
		byID[id] = mi
		out = append(out, id)
		infos = append(infos, mi)
	}
	if len(out) == 0 {
		return nil, nil, nil, nil, fmt.Errorf("global models empty list")
	}
	return out, infos, efforts, defaults, nil
}
