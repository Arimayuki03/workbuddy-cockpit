// Package usage 记录并聚合逐请求 token 用量，供面板「用量」视图展示。
//
// 与 internal/pool 的 TokenUsage 的区别：
//   - pool 的 TokenUsage 是**每账号一个累计计数器**，只保留总量与「最近一次」，
//     没有时间维度，也无法按模型/时间下钻；
//   - 本包按 (时间片, realm, uid, model) 分桶累计，因此可以出「今天各模型各用了多少」
//     「这一小时 prompt 涨得多快」这类问题，且能长期保留。
//
// 保留策略（分片粒度自动降级，总量因此有界）：
//   - 近 hourlyKeep 小时内：小时桶（细粒度，看尖峰）
//   - 更早：折叠为日桶，**永久保留**（看长期趋势）
//
// 落盘：data/usage.json，原子替换 + 防抖刷新（默认 30s），重启不丢。
// 桶数上界 ≈ 账号数 × 模型数 × (hourlyKeep + 已过天数)，实测单桶约 90 字节。
package usage

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// hourlyKeep 小时桶的保留时长；超出后折叠为日桶。
const hourlyKeep = 90 * 24 * time.Hour

// flushInterval 防抖落盘间隔。
const flushInterval = 30 * time.Second

// maxBuckets 桶数硬上限。超过时立即触发一次折叠，避免异常流量把内存/文件撑爆。
const maxBuckets = 400_000

// bucketLimit 生效的桶数上限（Rollup 强制折叠与 Start 前置检查共用）。
// 初始值 = maxBuckets；独立成 var 仅供测试调小——构造 40 万个桶验证强制折叠
// 代价过高，小上限下同路径可复现。生产代码不得改写。
var bucketLimit = maxBuckets

// hourLayout / dayLayout 分片键的时间格式（本地时区，与用户直觉一致）。
const (
	hourLayout = "2006-01-02T15"
	dayLayout  = "2006-01-02"
)

// bucket 一个 (时间片, realm, uid, model) 的累计量。
// JSON 字段名刻意取短，因为桶数量会随时间增长。
type bucket struct {
	Scope string  `json:"s"` // "h:2006-01-02T15" 或 "d:2006-01-02"
	Realm string  `json:"r"`
	UID   string  `json:"u"`
	Model string  `json:"m"`
	Req   int64   `json:"q"`  // 请求数（含失败）
	Err   int64   `json:"e"`  // 失败数
	PT    int64   `json:"p"`  // prompt tokens
	CT    int64   `json:"c"`  // completion tokens
	TT    int64   `json:"t"`  // total tokens（上游给什么用什么的合计）
	LatMs int64   `json:"l"`  // 延迟累计（ms）
	LatN  int64   `json:"ln"` // 延迟样本数
	TPS   float64 `json:"v"`  // 吐字速率累计
	TPSN  int64   `json:"vn"` // 速率样本数
}

// file 落盘结构。
type file struct {
	Version int      `json:"version"`
	Saved   string   `json:"saved"`
	Buckets []bucket `json:"buckets"`
}

// Recorder 并发安全的用量记录器。
type Recorder struct {
	mu      sync.Mutex
	path    string
	buckets map[string]*bucket // key: scope|realm|uid|model
	dirty   bool
	started time.Time

	// writeMu 落盘 IO 段互斥（面板 Save 与后台 ticker 并发 flush 的撕裂写防护）：
	// 两个 flush 并发执行时共享同一 .tmp 路径，WriteFile/TRUNCATE/Rename 交叉后
	// Rename 可能把"半截 B"的内容以最终文件名落地，产生损坏的 usage.json。
	// 只串行化 IO 段；快照仍走 mu（不扩大 mu 临界区，写盘不阻塞 Add）。
	writeMu sync.Mutex

	startOnce   sync.Once
	startedFlag atomic.Bool
	stopOnce    sync.Once
	stop        chan struct{}
	done        chan struct{}
}

// New 创建记录器。path 为空时禁用落盘（纯内存，测试用）。
func New(path string) *Recorder {
	r := &Recorder{
		path:    path,
		buckets: make(map[string]*bucket),
		started: time.Now(),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if path != "" {
		if err := r.load(); err != nil {
			log.Printf("[usage] 读取 %s 失败（从零开始）: %v", path, err)
		}
	}
	return r
}

// Start 启动后台防抖落盘与折叠。Stop 前一直运行。
func (r *Recorder) Start() {
	r.startOnce.Do(func() {
		r.startedFlag.Store(true)
		go func() {
			defer close(r.done)
			t := time.NewTicker(flushInterval)
			defer t.Stop()
			for {
				select {
				case <-r.stop:
					r.flush(true)
					return
				case <-t.C:
					r.mu.Lock()
					n := len(r.buckets)
					r.mu.Unlock()
					if n > bucketLimit {
						r.Rollup(time.Now())
					}
					r.flush(false)
				}
			}
		}()
	})
}

// Stop 停止后台循环并做最后一次落盘。Start 未调用时直接返回（done 永不关闭，
// 等待会死锁——防御「构造后未 Start 就 Stop」的初始化失败路径）。
func (r *Recorder) Stop() {
	if !r.startedFlag.Load() {
		return
	}
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
}

// Delta 一次请求尝试的用量增量（与 pool.TokenUsageDelta 同形，避免包间依赖）。
type Delta struct {
	PromptTokens     int64
	HasPromptTokens  bool
	CompletionTokens int64
	HasCompletion    bool
	TotalTokens      int64
	HasTotal         bool
	LatencyMs        int64
	HasLatency       bool
	TokensPerSecond  float64
	HasTPS           bool
}

// Add 记录一次请求尝试。
//
// ok=false 表示该次尝试失败（传输错误 / 上游 >=400 / 解析失败）。失败尝试通常
// 没有 usage，但**仍要计入请求数与失败数**——重试放大正是靠这一列才看得出来。
func (r *Recorder) Add(now time.Time, realm, uid, model string, d Delta, ok bool) {
	if r == nil {
		return
	}
	if realm == "" {
		realm = "cn"
	}
	if model == "" {
		model = "(unknown)"
	}
	scope := "h:" + now.Format(hourLayout)
	key := scope + "|" + realm + "|" + uid + "|" + model

	r.mu.Lock()
	defer r.mu.Unlock()

	b := r.buckets[key]
	if b == nil {
		b = &bucket{Scope: scope, Realm: realm, UID: uid, Model: model}
		r.buckets[key] = b
	}
	b.Req++
	if !ok {
		b.Err++
	}
	if d.HasPromptTokens {
		b.PT += d.PromptTokens
	}
	if d.HasCompletion {
		b.CT += d.CompletionTokens
	}
	if d.HasTotal {
		b.TT += d.TotalTokens
	} else if d.HasPromptTokens || d.HasCompletion {
		// 上游没给 total：用 pt+ct 兜底，保证总量口径连续。
		b.TT += d.PromptTokens + d.CompletionTokens
	}
	if d.HasLatency {
		b.LatMs += d.LatencyMs
		b.LatN++
	}
	if d.HasTPS {
		b.TPS += d.TokensPerSecond
		b.TPSN++
	}
	r.dirty = true
}

// Rollup 把超出 hourlyKeep 的小时桶折叠为日桶（按本地日历日）。
// 幂等：同一小时反复折叠不会重复计数（先累加再删源桶）。
//
// 桶数超限时强制折叠：hourlyKeep 口径只折叠 90 天前的桶，账号×模型×2160 小时片
// 超过 maxBuckets 时「按期折叠」退化为空操作，内存无界。此时按时间升序从最旧的
// 未到期小时桶开始折叠为日桶，直到桶数 ≤ maxBuckets*0.8（留 20% 余量，避免
// 每 30s 的 flush 前置检查每轮都触发折叠）。
func (r *Recorder) Rollup(now time.Time) {
	if r == nil {
		return
	}
	cutoff := now.Add(-hourlyKeep)

	r.mu.Lock()
	defer r.mu.Unlock()

	type move struct{ from, to string }
	var moves []move
	for k, b := range r.buckets {
		if !strings.HasPrefix(b.Scope, "h:") {
			continue
		}
		ts, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(b.Scope, "h:"), time.Local)
		if err != nil || !ts.Before(cutoff) {
			continue
		}
		day := "d:" + ts.Format(dayLayout)
		moves = append(moves, move{from: k, to: day + "|" + b.Realm + "|" + b.UID + "|" + b.Model})
	}

	// 强制折叠（桶数超限）：近期小时桶按 Scope（即时间）升序，从最旧的开始折叠
	// 为日桶，直到桶数 ≤ bucketLimit*0.8（留 20% 余量，防每次 30s 检查都触发）。
	// 净桶数变化 = 删源桶 -1，新建日桶 +1：用 projected 模拟应用 moves 后的桶数
	// 决定何时收手，剩余可折叠的近期小时桶数是天然上界，防死循环。
	if len(r.buckets) > bucketLimit {
		limit := bucketLimit * 8 / 10
		var recent []*bucket
		for _, b := range r.buckets {
			if !strings.HasPrefix(b.Scope, "h:") {
				continue
			}
			if ts, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(b.Scope, "h:"), time.Local); err == nil && !ts.Before(cutoff) {
				recent = append(recent, b)
			}
		}
		sort.Slice(recent, func(i, j int) bool { return recent[i].Scope < recent[j].Scope })
		projected := len(r.buckets)
		created := map[string]bool{} // moves 应用后将新建的日桶键
		forced := 0
		for _, b := range recent {
			if projected <= limit {
				break
			}
			day := "d:" + b.Scope[2:12]
			dst := day + "|" + b.Realm + "|" + b.UID + "|" + b.Model
			moves = append(moves, move{from: b.Scope + "|" + b.Realm + "|" + b.UID + "|" + b.Model, to: dst})
			forced++
			if r.buckets[dst] == nil && !created[dst] {
				created[dst] = true // 删源 +1 建桶，净 0
			} else {
				projected-- // 日桶已存在（或本轮已建），仅删源，净 -1
			}
		}
		if forced > 0 {
			log.Printf("[usage] 桶数超限（%d > %d），强制折叠最旧小时桶 %d 个", len(r.buckets), bucketLimit, forced)
		}
	}

	for _, m := range moves {
		src := r.buckets[m.from]
		if src == nil {
			continue
		}
		dst := r.buckets[m.to]
		if dst == nil {
			cp := *src
			cp.Scope = strings.SplitN(m.to, "|", 2)[0]
			dst = &cp
			r.buckets[m.to] = dst
		} else {
			dst.Req += src.Req
			dst.Err += src.Err
			dst.PT += src.PT
			dst.CT += src.CT
			dst.TT += src.TT
			dst.LatMs += src.LatMs
			dst.LatN += src.LatN
			dst.TPS += src.TPS
			dst.TPSN += src.TPSN
		}
		delete(r.buckets, m.from)
	}
	if len(moves) > 0 {
		r.dirty = true
		log.Printf("[usage] 折叠 %d 个小时桶为日桶（保留 %v 细粒度）", len(moves), hourlyKeep)
	}
}

// ---------------------------------------------------------------- 持久化 ----

func (r *Recorder) load() error {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		// 文件损坏：先改名隔离（保留人工抢救机会），否则下一轮 flush 会用
		// 从零开始的内存态覆盖原文件，历史用量永久丢失。隔离失败仅打日志继续。
		broken := r.path + ".broken-" + time.Now().Format("20060102-150405")
		if rerr := os.Rename(r.path, broken); rerr != nil {
			log.Printf("[usage] 隔离损坏文件失败（%s 继续在原位）: %v", r.path, rerr)
		} else {
			log.Printf("[usage] 检测到损坏的用量文件，已隔离为 %s（从零开始）", broken)
		}
		return err
	}
	for i := range f.Buckets {
		b := f.Buckets[i]
		r.buckets[b.Scope+"|"+b.Realm+"|"+b.UID+"|"+b.Model] = &b
	}
	log.Printf("[usage] 已恢复 %d 个用量桶（%s）", len(r.buckets), r.path)
	return nil
}

func (r *Recorder) flush(force bool) {
	if r == nil || r.path == "" {
		return
	}
	r.mu.Lock()
	if !r.dirty && !force {
		r.mu.Unlock()
		return
	}
	snap := file{Version: 1, Saved: time.Now().Format(time.RFC3339), Buckets: make([]bucket, 0, len(r.buckets))}
	for _, b := range r.buckets {
		snap.Buckets = append(snap.Buckets, *b)
	}
	r.dirty = false
	r.mu.Unlock()

	// 任何一步失败都恢复 dirty：让这批增量在下一轮 flush 重试——否则一旦磁盘
	// 抖动/ENOSPC 期间没有新请求进来，丢掉的用量统计会永久缺失，违背「重启不丢」承诺。
	markDirty := func() {
		r.mu.Lock()
		r.dirty = true
		r.mu.Unlock()
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[usage] 序列化失败: %v", err)
		markDirty()
		return
	}
	// IO 段在 writeMu 临界区内执行：面板 Save（flush(true)）与后台 ticker
	// （flush(false)）可能并发到达，二者共享同一 .tmp 路径——WriteFile(A) /
	// WriteFile(B, TRUNC) / Rename 交叉时，Rename 落地的可能是对方写到一半的
	// 临时文件（撕裂写，usage.json 损坏）。锁只护 IO 段，快照仍走 mu。
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		log.Printf("[usage] 建目录失败: %v", err)
		markDirty()
		return
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("[usage] 写临时文件失败: %v", err)
		markDirty()
		return
	}
	if err := os.Rename(tmp, r.path); err != nil {
		log.Printf("[usage] 原子替换失败: %v", err)
		markDirty()
	}
}

// Save 立即落盘（面板「刷新」或关闭前调用）。
func (r *Recorder) Save() { r.flush(true) }

// ---------------------------------------------------------------- 聚合 ----

// Agg 一组累计量。
type Agg struct {
	Requests      int64   `json:"requests"`
	Errors        int64   `json:"errors"`
	PromptTokens  int64   `json:"prompt_tokens"`
	CompletionTok int64   `json:"completion_tokens"`
	TotalTokens   int64   `json:"total_tokens"`
	AvgLatencyMs  float64 `json:"avg_latency_ms"`
	AvgTPS        float64 `json:"avg_tokens_per_second"`
}

// aggAcc 是聚合过程中的累加器：Agg 只放已算好的结果，均值需要样本数才能
// 正确加权（不能对每桶的均值再取平均），所以样本数留在这里。
type aggAcc struct {
	Agg
	latSum     int64
	latSamples int64
	tpsSum     float64
	tpsSamples int64
}

func (g *aggAcc) add(b *bucket) {
	g.Requests += b.Req
	g.Errors += b.Err
	g.PromptTokens += b.PT
	g.CompletionTok += b.CT
	g.TotalTokens += b.TT
	g.latSum += b.LatMs
	g.latSamples += b.LatN
	g.tpsSum += b.TPS
	g.tpsSamples += b.TPSN
}

func (g *aggAcc) finish() Agg {
	a := g.Agg
	if g.latSamples > 0 {
		a.AvgLatencyMs = float64(g.latSum) / float64(g.latSamples)
	}
	if g.tpsSamples > 0 {
		a.AvgTPS = g.tpsSum / float64(g.tpsSamples)
	}
	return a
}

// KeyedAgg 按某个维度聚合的一行。
type KeyedAgg struct {
	Key   string `json:"key"`
	Realm string `json:"realm,omitempty"`
	Extra string `json:"extra,omitempty"` // 账号行放昵称
	Agg
}

// Point 时序上的一个点。
// Realm 标注该点属于哪个域（cn/global），前端据此把「今日 token / 趋势图」
// 按版本拆分——没有它，国际版和国内版会看到同一份聚合（桶本身带 realm，
// 但 series 聚合时丢掉了这个维度）。omitted（旧消费方/历史数据）= 无标注。
type Point struct {
	T     string `json:"t"`
	Scope string `json:"scope"` // "hour" | "day"
	Realm string `json:"realm,omitempty"`
	Agg
}

// Snapshot 面板一次拉取的全部用量视图数据。
type Snapshot struct {
	Totals    Agg        `json:"totals"`
	ByRealm   []KeyedAgg `json:"by_realm"`
	ByAccount []KeyedAgg `json:"by_account"`
	ByModel   []KeyedAgg `json:"by_model"`
	Series    []Point    `json:"series"`
	Buckets   int        `json:"buckets"`
	FileBytes int64      `json:"file_bytes"`
	Since     string     `json:"since,omitempty"`
	Generated string     `json:"generated"`
}

// Snapshot 聚合当前全部桶。hours 控制时间窗：>0 时 series / by_account /
// by_model 只聚合窗口内的桶——面板的时间筛选必须对这三份视图同时生效（否则
// 切时间窗只有图变、表不动）；hours<=0 表示「自记录以来」全量口径（面板的
// 「启动以来」档）：排行聚合全部桶，series 保留近 30 天小时粒度（与最长数字
// 档一致，短历史不塌成日点）、更早的折叠为日点，长期趋势不丢。
// totals / by_realm 恒为全量累计（「累计请求」卡片的文案就是累计口径）。
// nicks 是 uid→昵称映射，仅用于展示。
func (r *Recorder) Snapshot(hours int, nicks map[string]string) Snapshot {
	if r == nil {
		return Snapshot{Series: []Point{}, Generated: time.Now().Format(time.RFC3339)}
	}
	allTime := hours <= 0 // 面板「启动以来」档：排行与时序都吃全部桶
	if !allTime && hours > 24*60 {
		hours = 72
	}

	r.mu.Lock()
	bs := make([]bucket, 0, len(r.buckets))
	for _, b := range r.buckets {
		bs = append(bs, *b)
	}
	r.mu.Unlock()

	var total aggAcc
	realmAgg := map[string]*aggAcc{}
	acctAgg := map[string]*aggAcc{}
	acctRealm := map[string]string{}
	// 模型行按时序同样要分域：同一裸模型名在两个域是两份用量，
	// 内部键用 realm|model 拆桶，输出时 Key=裸名、Realm=域（见 keyed 调用处）。
	modelAgg := map[string]*aggAcc{}
	// series 与桶同构地按 (realm, scope) 分桶：key = realm|scope。
	// 同一时刻两个域各有一个点，输出时相邻（先按 key 排序天然满足）。
	hourSeries := map[string]*aggAcc{}
	daySeries := map[string]*aggAcc{}

	nowHour := time.Now().Truncate(time.Hour)
	hourFrom := nowHour.Add(-time.Duration(hours-1) * time.Hour)
	// allTime 档时序的小时粒度窗口：与最长数字档（30 天）一致。全部按日折叠会让
	// 短历史（如仅 2 天数据）的图塌成 3 个日点、观感上「启动以来反而变糊」；
	// 保留近 30 天小时点，更早的折叠为日点，排行表才是全量口径。
	allHourFrom := nowHour.Add(-(30*24 - 1) * time.Hour)

	for i := range bs {
		b := &bs[i]
		total.add(b)

		if realmAgg[b.Realm] == nil {
			realmAgg[b.Realm] = &aggAcc{}
		}
		realmAgg[b.Realm].add(b)

		// 账号/模型行跟随时间窗（allTime 时全量）：小时桶按时间戳判断，日桶落在窗口内才计入。
		// 注意与 series 的 stitching 口径不同——那里窗口外的小时点并回日点保时序
		// 连续；这里是筛选，窗口外的数据直接不计入。
		inWindow := allTime
		if !inWindow {
			if strings.HasPrefix(b.Scope, "h:") {
				if ts, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(b.Scope, "h:"), time.Local); err == nil {
					inWindow = !ts.Before(hourFrom)
				}
			} else if day, ok := strings.CutPrefix(b.Scope, "d:"); ok {
				if ts, err := time.ParseInLocation(dayLayout, day, time.Local); err == nil {
					inWindow = !ts.Before(hourFrom)
				}
			}
		}
		if inWindow {
			if acctAgg[b.UID] == nil {
				acctAgg[b.UID] = &aggAcc{}
			}
			acctAgg[b.UID].add(b)
			// 一个账号只属于一个 realm，这里记下来供前端展示「域」列；
			// keyed() 的 Realm 字段默认是空的（它按 key 分组，不知道 realm）。
			if acctRealm[b.UID] == "" {
				acctRealm[b.UID] = b.Realm
			}

			if modelAgg[b.Realm+"|"+b.Model] == nil {
				modelAgg[b.Realm+"|"+b.Model] = &aggAcc{}
			}
			modelAgg[b.Realm+"|"+b.Model].add(b)
		}

		scope := strings.TrimPrefix(b.Scope, "h:")
		isHour := strings.HasPrefix(b.Scope, "h:")
		if isHour {
			ts, err := time.ParseInLocation(hourLayout, scope, time.Local)
			if err != nil {
				continue
			}
			// allTime 档保留近 30 天的小时粒度（与最长数字档一致），更早的按日归并；
			// 数字档只保留窗口内的小时点，窗口外并日避免时序出现空洞。
			if (!allTime && !ts.Before(hourFrom)) || (allTime && !ts.Before(allHourFrom)) {
				if hourSeries[b.Realm+"|"+scope] == nil {
					hourSeries[b.Realm+"|"+scope] = &aggAcc{}
				}
				hourSeries[b.Realm+"|"+scope].add(b)
			} else {
				// 超出小时窗口的细粒度数据并入其所在日，避免时序出现空洞。
				d := ts.Format(dayLayout)
				if daySeries[b.Realm+"|"+d] == nil {
					daySeries[b.Realm+"|"+d] = &aggAcc{}
				}
				daySeries[b.Realm+"|"+d].add(b)
			}
		} else {
			if daySeries[b.Realm+"|"+scope] == nil {
				daySeries[b.Realm+"|"+scope] = &aggAcc{}
			}
			daySeries[b.Realm+"|"+scope].add(b)
		}
	}

	snap := Snapshot{
		Totals:  total.finish(),
		ByRealm: keyed(realmAgg, func(k string) (string, string) { return k, "" }),
		// Series 显式空片初始化：零桶（刚启动无流量）时 append 从不执行，
		// nil 切片序列化成 JSON null，面板 /stats/ 页 usage.series.filter()
		// 直接 TypeError 白屏（客户端异常）。契约：数组字段恒为数组。
		Series: []Point{},
		ByAccount: keyed(acctAgg, func(k string) (string, string) {
			return k, nicks[k]
		}),
		// 内部键是 realm|model：Key 输出裸模型名（与旧口径一致，不破坏消费方），
		// realm 单独放 Realm 字段（omitempty，前端 (realm ?? 'cn') 过滤）。
		ByModel: keyedRealm(modelAgg, func(k string) (string, string) {
			_, model := modelRealmOf(k)
			return model, ""
		}, func(k string) string {
			rlm, _ := modelRealmOf(k)
			return rlm
		}),
		Buckets:   len(bs),
		Generated: time.Now().Format(time.RFC3339),
	}
	for i := range snap.ByAccount {
		snap.ByAccount[i].Realm = acctRealm[snap.ByAccount[i].Key]
	}

	// 日点（升序）+ 小时点（升序）拼成一条连续时序；key 为 realm|时间片，
	// 排序后同一天的相邻点即相邻 realm（同天同域相邻，跨域点穿插但不乱序）。
	dayKeys := make([]string, 0, len(daySeries))
	for k := range daySeries {
		dayKeys = append(dayKeys, k)
	}
	sort.Strings(dayKeys)
	for _, k := range dayKeys {
		rlm, day := modelRealmOf(k)
		snap.Series = append(snap.Series, Point{T: day, Scope: "day", Realm: rlm, Agg: daySeries[k].finish()})
	}
	hourKeys := make([]string, 0, len(hourSeries))
	for k := range hourSeries {
		hourKeys = append(hourKeys, k)
	}
	sort.Strings(hourKeys)
	for _, k := range hourKeys {
		rlm, hour := modelRealmOf(k)
		snap.Series = append(snap.Series, Point{T: hour, Scope: "hour", Realm: rlm, Agg: hourSeries[k].finish()})
	}

	if r.path != "" {
		if fi, err := os.Stat(r.path); err == nil {
			snap.FileBytes = fi.Size()
		}
	}
	// 最早的分片即数据起点。
	if len(snap.Series) > 0 {
		snap.Since = snap.Series[0].T
	}
	return snap
}

// modelRealmOf 拆开 realm|xxx 形态的内部聚合键，返回 (realm, xxx)。
// 与桶键的分隔符同款：realm 名不含 "|"（取值仅 cn/global），无歧义。
func modelRealmOf(key string) (string, string) {
	rlm, rest, _ := strings.Cut(key, "|")
	return rlm, rest
}

func keyed(m map[string]*aggAcc, label func(string) (string, string)) []KeyedAgg {
	return keyedRealm(m, label, nil)
}

// keyedRealm 在 keyed 之上支持 realmOf：内部键含 realm 维度（如 realm|model）时，
// 输出行按 realmOf 还原 Realm 字段——Key 仍走 label（裸名），Realm 单独透出。
func keyedRealm(m map[string]*aggAcc, label func(string) (string, string), realmOf func(string) string) []KeyedAgg {
	out := make([]KeyedAgg, 0, len(m))
	for k, v := range m {
		key, extra := label(k)
		row := KeyedAgg{Key: key, Extra: extra, Agg: v.finish()}
		if realmOf != nil {
			row.Realm = realmOf(k)
		}
		out = append(out, row)
	}
	// 按总量降序；同量按 key 升序，保证输出稳定（前端 diff 不抖）。
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalTokens != out[j].TotalTokens {
			return out[i].TotalTokens > out[j].TotalTokens
		}
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Describe 返回一行人类可读的占用摘要（启动日志用）。
func (r *Recorder) Describe() string {
	if r == nil {
		return "disabled"
	}
	r.mu.Lock()
	n := len(r.buckets)
	r.mu.Unlock()
	var sz int64
	if r.path != "" {
		if fi, err := os.Stat(r.path); err == nil {
			sz = fi.Size()
		}
	}
	return fmt.Sprintf("%d buckets, file %d bytes", n, sz)
}
