// modelrate.go 模型积分倍率缓存表：credit_floor 积分保底（internal/pool）的
// upstream 数据源。上游随模型目录下发各模型的 credits 积分倍率原文（实测形态
// "x0.05 credits" / "0.50x"），本文件把原文解析为数值倍率并按 (realm, model)
// 缓存，供 main 装配 pool.SetModelRateOf 闭包时经 ModelRateOf 查询——保底判据
//（pick.go floorBlockedForRealmModel）在「本地台账无实测观测」时以此兜底判收费，
// 堵住「无观测 = 放行」被高价新模型打穿的洞（kimi-k3-1 实案：x1.62 两笔打穿
// 100 分的号并硬冷却到次日 04:00）。
//
// 口径（与内部既有目录一致）：
//   - 表键 = 裸模型名（目录 ModelInfo.ID 原样，不做小写化/别名归一，与 efforts
//     桶、globalModels 探测缓存的 key 形态一致）；cn:/global: 前缀只存在于
//     /v1/models 出口与 resolveModel 入口协议层，到选号/本表时已被剥掉
//    （internal/server/resolve_model.go），故本表不剥前缀、不认带前缀的键。
//   - realm 维度与 efforts 桶同款分层（cn/global，空按 cn——realmKey 老 CN 语义），
//     两域同名模型倍率互不污染（C-2 隔离原则）。
//
// 契约（与 pool.SetModelRateOf 对齐）：目录未覆盖/credits 不可解析 → 不入表 →
// ModelRateOf 返回 ""（= 未知放行，pool 侧 ParseFloat 失败同语义，见 pick.go
// floorBlockedForRealmModel 注释）；明确免费（"x0.00"）入表为 0，与「未知」严格区分。
package upstream

import (
	"context"
	"errors"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"

	"workbuddy2api/internal/auth"
)

// NormalizeModelRate 把上游 credits 倍率原文解析为数值倍率：
//   - "x0.05 credits" → 0.05（x 前缀倍率记号 + credits 尾巴单位词，实测主形态）
//   - "0.50x"         → 0.50（后缀倍率记号）
//   - "0.05" / "1.62" → 原样数值
//
// ok=false（一律不入表，等价「未知」，调用方按放行处理）：空串、剥离记号后
// 非数值、负数、NaN/Inf（strconv 会把 "NaN"/"Inf"/"xInf" 解析成功，须显式拒绝）。
// 0 是合法结果（"x0.00" = 明确免费），与「未知」严格区分——与 cmd/stats 的
// creditsRate 排序哨兵 -1 各司其职（那边缺「未知/免费」的显式区分）。
func NormalizeModelRate(s string) (v float64, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	// 剥离尾巴 "credits" 单位词（"x0.05 credits" → "x0.05"）；整串就是
	// "credits" 时剥出空串不采纳，保持原串进数值解析（必然失败 → ok=false）。
	if rest, has := strings.CutSuffix(s, "credits"); has && strings.TrimSpace(rest) != "" {
		s = strings.TrimSpace(rest)
	}
	// 剥离倍率记号 x：前缀（"x0.05"）或后缀（"0.50x"）二选一；剥出空串
	// （"x" / "0.50xx" 类残形态）不采纳，原串进解析失败 → ok=false。
	// 剥离后 TrimSpace（"x 0.05" 记号与数值间空白，宽容口径）。
	if rest, has := strings.CutPrefix(s, "x"); has && strings.TrimSpace(rest) != "" {
		s = strings.TrimSpace(rest)
	} else if rest, has := strings.CutSuffix(s, "x"); has && strings.TrimSpace(rest) != "" {
		s = strings.TrimSpace(rest)
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// ModelRateTable 并发安全的 (realm, model) → 数值倍率表。零值可用（Update 内
// 惰性建桶），测试直接构造/内嵌 Client 字段即隔离——与 efforts 桶/探测缓存的
// 按实例持有同款。
type ModelRateTable struct {
	mu    sync.RWMutex
	table map[string]map[string]float64 // realm（realmKey 归一）→ model（裸名）→ 数值倍率
}

// NewModelRateTable 构造空表（与零值等价，显式构造便于阅读）。
func NewModelRateTable() *ModelRateTable { return &ModelRateTable{} }

// Update 批量入表（upsert 合并语义）：同 (realm, model) 以新值覆盖，未提及的
// 既有条目保留。合并而非换桶的理由：CN 侧目录是 agents[cli] 过滤后的部分视图
//（FetchModels），换桶会把上一次探测见过、本次 cli 面之外的模型倍率丢掉——
// 丢掉即「未知放行」，保底反而开口子。模型下线后条目滞留无副作用（选号不再
// 命中该模型）。rates 为空时零操作（与 storeEfforts 的「空探测不清既有桶」同
// 防呆），非法值（调用方应经 NormalizeModelRate 过滤）不在此二次校验。
func (t *ModelRateTable) Update(realm string, rates map[string]float64) {
	if len(rates) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.table == nil {
		t.table = make(map[string]map[string]float64)
	}
	k := realmKey(realm)
	bucket := t.table[k]
	if bucket == nil {
		bucket = make(map[string]float64, len(rates))
		t.table[k] = bucket
	}
	for m, v := range rates {
		bucket[m] = v
	}
}

// Lookup 查询 (realm, model) 的数值倍率。未命中 → (0, false)——调用方按
// 「未知放行」处理（契约见文件头）。realm 空 → 按 cn（realmKey 老 CN 语义：
// 裸模型名协议默认 CN 域，与 resolveModel 的裸名落 cn 一致）。
func (t *ModelRateTable) Lookup(realm, model string) (float64, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	v, ok := t.table[realmKey(realm)][model]
	return v, ok
}

// Size 返回全表条目总数（两域合计）。预热效果/测试观测用。
func (t *ModelRateTable) Size() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n := 0
	for _, bucket := range t.table {
		n += len(bucket)
	}
	return n
}

// StoreModelRates 模型目录条目 → 倍率表填充（FetchModels / global 探测成功处的
// 唯一入口）：逐条解析 Credits 原文，可解析的入表，不可解析的跳过（不入表 =
// 未知放行，不惩罚）。realm 用探测账号的域（CN 探测结果不得污染 global 域，
// C-2 同款隔离；global 探测固定传 "global"）。infos 空（窄表形态/无数据）时零
// 操作。表自带锁，可在任意调用点安全触发，与调用方持有的其他锁互不嵌套。
func (c *Client) StoreModelRates(realm string, infos []ModelInfo) {
	if len(infos) == 0 {
		return
	}
	rates := make(map[string]float64, len(infos))
	for _, mi := range infos {
		if mi.ID == "" {
			continue
		}
		if v, ok := NormalizeModelRate(mi.Credits); ok {
			rates[mi.ID] = v
		}
	}
	c.modelRates.Update(realm, rates)
}

// ModelRateOf 供 pool.SetModelRateOf 装配的查询闭包（main 接线用）：
// 返回形如 "1.62" 的数值字符串；目录未覆盖/不可解析 → ""（= 未知放行，pool 侧
// 语义见 floorBlockedForRealmModel）。已知免费返回 "0"（pool 判 v>0 为收费，
// "0" 与 "" 同为放行，但 "0" 语义更准：明确免费而非未知）。
func (c *Client) ModelRateOf(realm, model string) string {
	v, ok := c.modelRates.Lookup(realm, model)
	if !ok {
		return ""
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// WarmModelRates 启动预热：并发探测 CN/global 两域模型目录并把倍率入表
//（main 启动装配用）。探测即既有链路（FetchModels / FetchGlobalModelInfos），
// 成功后填充点自动入表——本函数只负责「触发 + 等待」，不另起解析路径。
//
// cnAccount / globalAccount 允许 nil（或 global 域被逃生门关闭：该域跳过，
// 无账号/无路由的域无从探测，不算错误）。单域失败降级另一域（WARN 日志，与
// 探测链路的降级口径一致）；两域都请求且都失败才返回错误（errors.Join，调用方
// 可日志告警但不建议阻断启动——运行期首次 /v1/models 会自然重探）。
// ctx 已取消时直接返回不探测；探测进行中 ctx 取消则等待其自然结束（出站超时
// 由 Client.HTTP 兜底，预热是启动期一次性开销，不值得为提前返回引入通道编排）。
func WarmModelRates(ctx context.Context, c *Client, cnAccount, globalAccount *auth.Auth) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		cnErr     error
		globalErr error
	)
	if cnAccount != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.FetchModels(cnAccount)
			mu.Lock()
			cnErr = err
			mu.Unlock()
		}()
	}
	// global 域逃生门兜底：globalOn 为假（config 关闭/账号回落 cn）时 fetchGlobalModelsOnce
	// 会零调用返回 nil——与 nil 账号同按「跳过」处理，不算失败。
	if globalAccount != nil && c.globalOn(globalAccount) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 直接走 once：失败返回 (nil, nil) 不带 err，以名单空判失败。
			names, _ := c.fetchGlobalModelsOnce(globalAccount)
			if len(names) == 0 {
				mu.Lock()
				globalErr = errors.New("global models probe failed (names empty)")
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	switch {
	case cnErr != nil && globalErr != nil:
		return errors.Join(cnErr, globalErr)
	case cnErr != nil:
		log.Printf("WARN: [upstream] model rate warmup: cn probe failed (global ok/skipped): %v", cnErr)
	case globalErr != nil:
		log.Printf("WARN: [upstream] model rate warmup: global probe failed (cn ok/skipped): %v", globalErr)
	}
	return nil
}
