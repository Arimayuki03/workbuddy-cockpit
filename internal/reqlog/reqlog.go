// Package reqlog 请求日志 JSONL 归档 + 内存指标快照（吸收自 workbuddy2api-panel）。
//
// 与 internal/server 的请求日志环形缓冲（requestlog.go）是互补关系，层次不同：
//   - 环形缓冲：进程内热数据（~1000 条，重启即清），面板「请求日志」tab 热路径数据源；
//   - 本包：磁盘 JSONL 归档（按日轮转、保留上限）+ 进程级指标快照（成功率/TTFB/
//     丢弃计数），供面板历史查询与 /api/request_metrics 类端点使用。
//
// 归档只写请求元数据（模型/状态/token/首字延迟/扣费/错误摘要），不写提示词、
// 响应正文、Authorization 或其它凭证。可选的调用来源（客户端 IP / User-Agent，
// 见 Event.ClientIP/UserAgent）由 server 侧按 logging.request_client_info 开关
// 决定是否填充；关掉即保持空串，归档里不会出现来源字段。
//
// 并发模型：Record 把事件投进带缓冲 channel（满了丢弃并计数，绝不阻塞请求路径），
// 单一后台 goroutine 负责写盘/轮转/保留清理。Close 刷盘排空队列后再关文件，
// 进程退出不丢尾部。
package reqlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// recentCap 内存「最近 N 条」环形容量（快照用；与磁盘归档无关）。
	recentCap = 100
	// defaultFileMaxBytes 单文件软上限：跨天或单文件超限时切新分片
	// （requests-YYYYMMDD.jsonl → requests-YYYYMMDD.N.jsonl），防单日巨文件。
	defaultFileMaxBytes = int64(64) << 20
	// defaultQueueSize 归档队列缓冲深度：1024 条足够吸收突发流量，
	// 超过即丢弃并计数（丢弃是可观测的，阻塞请求路径不可接受）。
	defaultQueueSize = 1024
	// defaultRetentionDays 按天保留上限（日志天数字段用）。
	defaultRetentionDays = 7
	// defaultMaxBytes 归档总容量上限（字节）：超过删最旧文件。
	defaultMaxBytes = int64(200) << 20
	// defaultReadLimit ReadArchive limit<=0 时的回落值。
	defaultReadLimit = 200
	// maxReadLimit ReadArchive 单次回读的最大条数（防面板一次捞爆内存）。
	maxReadLimit = 1000
	// archiveFilePrefix / archiveFileExt 归档文件名骨架：requests-<day>[.N].jsonl
	archiveFilePrefix = "requests-"
	archiveFileExt    = ".jsonl"
	// dayFormat 归档文件名中的日期格式（本地时区；跨天以事件时间判定）。
	dayFormat = "20060102"
	// pruneInterval 保留清理周期：每分钟跑一次足够（归档是分钟级运维关注项）。
	pruneInterval = time.Minute
	// flushInterval 兜底刷盘周期：低流量下事件长期间不足 bufio 缓冲，
	// 定时 flush 保证 crash 后最多丢这一窗而不是全部。
	flushInterval = time.Second
)

// Event 一条请求级观测事件（归档落盘的最小单元）。
//
// Account 只保存「昵称(uid8)」标签形态，不落完整 UID——归档文件可能被人工
// cat/grep，最小化可识别信息。ClientIP/UserAgent 是调用来源，server 侧按
// logging.request_client_info 开关决定是否填充（默认 false 保持空串，
// omitempty 保证归档里不出现来源字段）。
type Event struct {
	Time    time.Time `json:"time"` // 事件时间（请求完成时刻；排序与轮转以此为准）
	Model   string    `json:"model,omitempty"`
	Account string    `json:"account,omitempty"` // "昵称(uid8)" 标签，不含完整 UID
	Mode    string    `json:"mode,omitempty"`    // "stream" | "sync"
	Status  int       `json:"status"`
	OK      bool      `json:"ok"` // 成功口径由 server 侧定（上游 usage 是否可用）
	// Tokens <0 = usage 缺失（观测缺失，非 0 token）；正数 = completion tokens。
	Tokens     int     `json:"tokens,omitempty"`
	TTFBMS     int64   `json:"ttfb_ms,omitempty"`     // 首字延迟毫秒（非流式/无帧 = 0）
	DurationMS int64   `json:"duration_ms,omitempty"` // 端到端耗时毫秒
	Credit     float64 `json:"credit,omitempty"`      // 扣费（HasCredit=false 时无观测）
	HasCredit  bool    `json:"has_credit"`
	// CacheHit/Miss/Write 缓存三段（上游 usage 的 prompt_cache_*_tokens）。
	CacheHit   int    `json:"cache_hit_tokens,omitempty"`
	CacheMiss  int    `json:"cache_miss_tokens,omitempty"`
	CacheWrite int    `json:"cache_write_tokens,omitempty"`
	Error      string `json:"error,omitempty"` // 非 200 的原因摘要（截断后）

	// ClientIP / UserAgent 调用来源（可选，脱敏开关控制是否采集）。
	ClientIP  string `json:"client_ip,omitempty"`
	UserAgent string `json:"user_agent,omitempty"`
}

// Filter ReadArchive 回读过滤器。字符串字段一律「包含」匹配（大小写不敏感），
// 便于用一段 IP 前缀或 UA 片段捞请求；From/To 是闭区间（零值 = 该侧不设界）。
type Filter struct {
	Model     string
	Account   string
	ClientIP  string
	UserAgent string
	Status    int // >0 时精确匹配 HTTP 状态码
	From      time.Time
	To        time.Time
}

// match 判定事件是否命中过滤器（零值字段 = 不筛该维度）。
func (f Filter) match(e Event) bool {
	if !containsFold(e.Model, f.Model) || !containsFold(e.Account, f.Account) ||
		!containsFold(e.ClientIP, f.ClientIP) || !containsFold(e.UserAgent, f.UserAgent) {
		return false
	}
	if f.Status > 0 && e.Status != f.Status {
		return false
	}
	if !f.From.IsZero() && e.Time.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && e.Time.After(f.To) {
		return false
	}
	return true
}

// containsFold 大小写不敏感的子串匹配；needle 为空视为命中（不筛该字段）。
func containsFold(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// Config 归档参数。Dir 为空时只启用内存指标（归档关闭，零开销）。
type Config struct {
	Dir           string // 归档目录（如 data/requests）；空 = 关闭归档
	RetentionDays int    // 按天保留上限，默认 7（<=0 回落默认）
	MaxBytes      int64  // 归档总容量上限（字节），默认 200MB（<=0 回落默认）
	FileMaxBytes  int64  // 单文件软上限（字节），默认 64MB（<=0 回落默认）
	QueueSize     int    // 异步队列深度，默认 1024（<=0 回落默认）
}

// Stats 归档存储状态（面板状态视图用）。
type Stats struct {
	Enabled   bool   `json:"enabled"`
	Dir       string `json:"dir,omitempty"`
	Files     int    `json:"files"`
	Bytes     int64  `json:"bytes"`
	Dropped   uint64 `json:"dropped"` // 队列满被丢弃的事件数
	LastError string `json:"last_error,omitempty"`
}

// Snapshot 内存指标快照（/api/request_metrics 类端点的数据源）。
type Snapshot struct {
	StartedAt   time.Time `json:"started_at"` // 记录器创建时刻
	Completed   int64     `json:"completed"`  // 累计完成请求数
	Succeeded   int64     `json:"succeeded"`
	Failed      int64     `json:"failed"`
	SuccessRate float64   `json:"success_rate"` // 百分比，1 位小数
	AvgTTFBMS   float64   `json:"avg_ttfb_ms"`  // 平均首字延迟（仅统计 TTFB>0 的观测）
	MaxTTFBMS   int64     `json:"max_ttfb_ms"`
	// TTFBObs 参与 TTFB 统计的观测数（TTFB=0 视为缺观测，不参与平均——
	// 与 chatStat 的「无帧 = 0」哨兵口径一致）。
	TTFBObs int64   `json:"ttfb_obs"`
	Recent  []Event `json:"recent"` // 最近 recentCap 条（新→旧）
	Archive Stats   `json:"archive"`
}

// Recorder 请求指标记录器：内存快照 + 可选异步 JSONL 归档。
// 并发安全；nil 接收者全部方法安全（零值形态可直接用于测试）。
type Recorder struct {
	mu        sync.Mutex
	started   time.Time
	completed int64
	succeeded int64
	ttfbSum   int64 // 仅累计 TTFB>0 的观测
	ttfbMax   int64
	ttfbObs   int64
	recent    []Event
	archive   *archiveWriter
}

// New 构建记录器。cfg.Dir 为空 = 只启用内存指标（归档关闭）。
func New(cfg Config) *Recorder {
	r := &Recorder{started: time.Now(), recent: make([]Event, 0, recentCap)}
	r.archive = newArchiveWriter(cfg)
	return r
}

// Record 记录一个请求完成事件：更新内存指标并把事件投给归档队列
// （队列满丢弃并计数，不阻塞调用方——热路径安全）。
func (r *Recorder) Record(e Event) {
	if r == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	r.mu.Lock()
	r.completed++
	if e.OK {
		r.succeeded++
	}
	if e.TTFBMS > 0 {
		r.ttfbSum += e.TTFBMS
		r.ttfbObs++
		if e.TTFBMS > r.ttfbMax {
			r.ttfbMax = e.TTFBMS
		}
	}
	// recent 保持新→旧（与 server 环形缓冲快照同口径）：头部插入。
	r.recent = append(r.recent, Event{})
	copy(r.recent[1:], r.recent)
	r.recent[0] = e
	if len(r.recent) > recentCap {
		r.recent = r.recent[:recentCap]
	}
	r.mu.Unlock()
	if r.archive != nil {
		r.archive.enqueue(e)
	}
}

// Snapshot 返回内存指标与归档状态。
func (r *Recorder) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.mu.Lock()
	s := Snapshot{
		StartedAt: r.started,
		Completed: r.completed,
		Succeeded: r.succeeded,
		Failed:    r.completed - r.succeeded,
		MaxTTFBMS: r.ttfbMax,
		TTFBObs:   r.ttfbObs,
		Recent:    append([]Event(nil), r.recent...),
	}
	if r.completed > 0 {
		s.SuccessRate = round1(float64(r.succeeded) / float64(r.completed) * 100)
	}
	if r.ttfbObs > 0 {
		s.AvgTTFBMS = round1(float64(r.ttfbSum) / float64(r.ttfbObs))
	}
	r.mu.Unlock()
	if r.archive != nil {
		s.Archive = r.archive.stats()
	}
	return s
}

// ReadArchive 回读归档事件，按事件时间**倒序**返回最近 limit 条（不依赖文件
// mtime——同一秒内连续轮转的多个文件 mtime 可能相同，目录拷贝/备份恢复更会
// 打乱 mtime，时间顺序只能从事件本身还原）。filter 非 nil 时先过滤再截断；
// limit<=0 回落 200，上限 1000。归档关闭时返回 (nil, nil)。
func (r *Recorder) ReadArchive(limit int, filter *Filter) ([]Event, error) {
	if r == nil || r.archive == nil {
		return nil, nil
	}
	return r.archive.read(limit, filter)
}

// Close 刷盘并停止后台归档 goroutine：先排空队列写完，再 flush + 关文件。
// 进程优雅退出时调用（幂等；nil/未启用归档安全）。
func (r *Recorder) Close() {
	if r == nil || r.archive == nil {
		return
	}
	r.archive.close()
}

// round1 保留 1 位小数（面板展示口径）。
func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}

// archiveWriter 异步 JSONL 归档：带缓冲 channel + 单一后台 goroutine。
// 零值不可用（需 newArchiveWriter 构建）；未启用形态（cfg 关闭）返回哑实现，
// enqueue 直通、read/stats 返回零值——调用方无需判空。
type archiveWriter struct {
	cfg      Config
	ch       chan Event
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once // close 幂等：重复 Close 不重复关 stop

	dropped atomic.Uint64 // 队列满被丢弃的事件数（观测用）
	// 以下字段仅后台 goroutine 访问（run/writeEvent/openFile/flush/prune/read 除外——
	// read 由调用方 goroutine 直接读目录，不触碰下列句柄）。
	file *os.File
	buf  *bufio.Writer
	path string
	day  string
	size int64

	lastErrMu sync.Mutex
	lastErr   string
}

// newArchiveWriter 构建归档 writer。cfg.Dir 为空 = 归档关闭，返回哑实现。
// 目录创建失败同样降级为哑实现（归档缺位不能拖垮网关），错误记入 LastError。
func newArchiveWriter(cfg Config) *archiveWriter {
	if strings.TrimSpace(cfg.Dir) == "" {
		return &archiveWriter{cfg: cfg}
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = defaultRetentionDays
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultMaxBytes
	}
	if cfg.FileMaxBytes <= 0 {
		cfg.FileMaxBytes = defaultFileMaxBytes
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueueSize
	}
	w := &archiveWriter{
		cfg:  cfg,
		ch:   make(chan Event, cfg.QueueSize),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		w.setErr(fmt.Errorf("mkdir %s: %w", cfg.Dir, err))
		// 目录建不出来：归档不可用。ch 置 nil，enqueue 走丢弃计数（可观测）。
		w.ch = nil
		close(w.done)
		return w
	}
	go w.run()
	return w
}

// enabled 归档是否真正在写（目录可用 + 后台 goroutine 在跑）。
func (w *archiveWriter) enabled() bool {
	return w != nil && w.ch != nil && w.done != nil
}

// enqueue 投递事件到归档队列：非阻塞，满则丢弃并计数（热路径绝不等待）。
// 归档降级（未启用/目录坏）时同样丢弃并计数——丢弃必须可观测，调用方据此
// 能发现「以为在归档其实在丢」。
func (w *archiveWriter) enqueue(e Event) {
	if w == nil {
		return
	}
	select {
	case w.ch <- e:
	default:
		w.dropped.Add(1)
	}
}

// run 后台主循环：消费队列写盘；周期性 flush + 保留清理；stop 后排空余量再退出。
func (w *archiveWriter) run() {
	defer close(w.done)
	flushTicker := time.NewTicker(flushInterval)
	defer flushTicker.Stop()
	pruneTicker := time.NewTicker(pruneInterval)
	defer pruneTicker.Stop()
	for {
		select {
		case e := <-w.ch:
			w.writeEvent(e)
		case <-flushTicker.C:
			w.flush()
		case <-pruneTicker.C:
			w.flush()
			w.prune()
		case <-w.stop:
			// 排空队列（此刻不再有新投递：调用方约定 Close 后不再 Record），
			// 最后 flush + 关文件，保证尾部不丢。
			for {
				select {
				case e := <-w.ch:
					w.writeEvent(e)
				default:
					w.flush()
					w.closeFile()
					return
				}
			}
		}
	}
}

// writeEvent 序列化并写一行 JSONL；按事件时间跨天轮转 / 单文件超限分片。
// 仅后台 goroutine 调用。
func (w *archiveWriter) writeEvent(e Event) {
	raw, err := json.Marshal(e)
	if err != nil {
		w.setErr(err)
		return
	}
	now := e.Time
	if now.IsZero() {
		now = time.Now()
	}
	day := now.Format(dayFormat)
	// 跨天轮转：按**事件时间**判定（不是写盘时刻——长请求完成时可能已过午夜，
	// 事件属于昨天的就该落昨天的文件，与 ReadArchive 按事件时间回读的契约一致）。
	if w.file == nil || w.day != day {
		if err := w.openFile(now, false); err != nil {
			w.setErr(err)
			return
		}
	}
	// 单文件软上限：写不下就切分片（requests-<day>.1.jsonl, .2.jsonl ...）。
	if w.size > 0 && w.size+int64(len(raw))+1 > w.cfg.FileMaxBytes {
		if err := w.openFile(now, true); err != nil {
			w.setErr(err)
			return
		}
	}
	if _, err := w.buf.Write(raw); err != nil {
		w.setErr(err)
		return
	}
	if err := w.buf.WriteByte('\n'); err != nil {
		w.setErr(err)
		return
	}
	w.size += int64(len(raw)) + 1
}

// openFile 打开（或切换）归档文件。rotate=false 时优先续写当天最新的既有分片
// （重启不另开新文件）；rotate=true 时强制开新分片（单文件超限）。
func (w *archiveWriter) openFile(now time.Time, rotate bool) error {
	w.closeFile()
	day := now.Format(dayFormat)
	if err := os.MkdirAll(w.cfg.Dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(w.cfg.Dir, archiveFilePrefix+day+archiveFileExt)
	if rotate {
		path = nextArchivePath(w.cfg.Dir, day)
	} else if latest := latestArchiveForDay(w.cfg.Dir, day); latest != "" {
		// 进程重启后续写当天已有的最新分片，避免碎片化。
		path = filepath.Join(w.cfg.Dir, latest)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.file = f
	w.buf = bufio.NewWriterSize(f, 64<<10)
	w.path = path
	w.day = day
	w.size = info.Size()
	return nil
}

// latestArchiveForDay 返回 dir 下某天（requests-<day>*.jsonl）分片序号最大的文件名；
// 无则空串。命名形态：requests-<day>.jsonl（序号 0）与 requests-<day>.N.jsonl。
func latestArchiveForDay(dir, day string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	prefix := archiveFilePrefix + day
	best, bestIdx := "", -1
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, archiveFileExt) {
			continue
		}
		idx := 0
		if mid := name[len(prefix) : len(name)-len(archiveFileExt)]; mid != "" {
			// 中段形如 ".1"/".2"；解析失败（脏文件）跳过。
			n, err := strconv.Atoi(strings.TrimPrefix(mid, "."))
			if err != nil || strings.TrimPrefix(mid, ".") == "" {
				continue
			}
			idx = n
		}
		if idx > bestIdx {
			bestIdx, best = idx, name
		}
	}
	return best
}

// nextArchivePath 生成当天下一个可用分片路径（不覆盖既有文件）。
func nextArchivePath(dir, day string) string {
	base := filepath.Join(dir, archiveFilePrefix+day)
	path := base + archiveFileExt
	for i := 1; fileExists(path); i++ {
		path = fmt.Sprintf("%s.%d%s", base, i, archiveFileExt)
	}
	return path
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// flush 把 bufio 缓冲刷到 OS（不 fsync：观测日志丢一窗可接受，fsync 开销不可接受）。
func (w *archiveWriter) flush() {
	if w.buf == nil {
		return
	}
	if err := w.buf.Flush(); err != nil {
		w.setErr(err)
	}
}

// closeFile flush + 关句柄 + 清空状态（可安全重复调用）。
func (w *archiveWriter) closeFile() {
	if w.buf != nil {
		_ = w.buf.Flush()
	}
	if w.file != nil {
		_ = w.file.Close()
	}
	w.file, w.buf = nil, nil
	w.path, w.day, w.size = "", "", 0
}

// prune 保留清理：按天（RetentionDays）+ 总容量（MaxBytes）双上限，超限删最旧
// （按文件名内嵌日期排旧，不用 mtime——mtime 会被拷贝/备份破坏）。当前打开中的
// 文件不删。仅后台 goroutine 调用。
func (w *archiveWriter) prune() {
	if !w.enabled() {
		return
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		w.setErr(err)
		return
	}
	type item struct {
		name string
		path string
		size int64
		day  string // 文件名内嵌日期（YYYYMMDD；解析失败为空，仅受容量约束）
	}
	items := make([]item, 0, len(entries))
	var total int64
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, archiveFilePrefix) || !strings.HasSuffix(name, archiveFileExt) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		it := item{name: name, path: filepath.Join(w.cfg.Dir, name), size: info.Size()}
		// 提取内嵌日期：requests-YYYYMMDD... → YYYYMMDD。
		day := strings.TrimPrefix(name, archiveFilePrefix)
		day = strings.SplitN(day, ".", 2)[0]
		if _, err := time.ParseInLocation(dayFormat, day, time.Local); err == nil {
			it.day = day
		}
		items = append(items, it)
		total += info.Size()
	}
	// 排旧：内嵌日期升序（无日期的脏文件排最后，只受容量约束）。
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].day == items[j].day {
			return items[i].name < items[j].name
		}
		if items[i].day == "" {
			return false
		}
		if items[j].day == "" {
			return true
		}
		return items[i].day < items[j].day
	})
	today := time.Now().Format(dayFormat)
	cutoff := ""
	if w.cfg.RetentionDays > 0 {
		// 按天：文件日期早于 today-(RetentionDays-1) 天即过期
		// （RetentionDays=7 → 今天+往前 7 个自然日都保留）。
		day, _ := time.ParseInLocation(dayFormat, today, time.Local)
		cutoff = day.AddDate(0, 0, -(w.cfg.RetentionDays - 1)).Format(dayFormat)
	}
	for _, it := range items {
		if it.path == w.path {
			continue // 打开中的文件不删（正在追加写）
		}
		expired := cutoff != "" && it.day != "" && it.day < cutoff
		// 容量上限 <=0 视为不设容量约束（防御：默认值已在 newArchiveWriter 兜底）。
		if expired || (w.cfg.MaxBytes > 0 && total > w.cfg.MaxBytes) {
			if err := os.Remove(it.path); err == nil {
				total -= it.size
			}
		}
	}
}

// read 回读归档：扫目录全部 requests-*.jsonl，逐行解码过滤，按事件时间倒序截断。
// 由调用方 goroutine 调用（与后台写并发：只读目录与文件，不触碰 writer 句柄）。
// 当前打开中文件的最后一段可能尚未 flush——回读看不到尾巴是可接受的（观测数据）。
func (w *archiveWriter) read(limit int, filter *Filter) ([]Event, error) {
	if !w.enabled() {
		return nil, nil
	}
	if limit <= 0 {
		limit = defaultReadLimit
	}
	if limit > maxReadLimit {
		limit = maxReadLimit
	}
	var fl Filter
	if filter != nil {
		fl = *filter
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, archiveFilePrefix) || !strings.HasSuffix(name, archiveFileExt) {
			continue
		}
		rows, err := readArchiveFile(filepath.Join(w.cfg.Dir, name), fl)
		if err != nil {
			return out, err
		}
		out = append(out, rows...)
	}
	// 契约：按事件时间倒序返回最近 limit 条（同一时刻用 Stable 保留落盘先后）。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// readArchiveFile 读单个归档文件，逐行解码 + 过滤。行损坏（半截写入/crash 残尾）
// 跳过该行继续，不放大成整文件错误。
func readArchiveFile(path string, filter Filter) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20) // 单行上限 1MB：事件是固定结构，远小于此
	var out []Event
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Event
		if json.Unmarshal(line, &e) != nil || !filter.match(e) {
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// stats 归档状态视图（面板展示用）。
func (w *archiveWriter) stats() Stats {
	s := Stats{Enabled: w.enabled(), Dir: w.cfg.Dir, Dropped: w.dropped.Load(), LastError: w.errString()}
	if !s.Enabled {
		return s
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		s.LastError = err.Error()
		return s
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, archiveFilePrefix) || !strings.HasSuffix(name, archiveFileExt) {
			continue
		}
		if info, err := entry.Info(); err == nil {
			s.Files++
			s.Bytes += info.Size()
		}
	}
	return s
}

// close 停止后台 goroutine：先关闸（stop），等排空退出（done）。幂等。
func (w *archiveWriter) close() {
	if !w.enabled() {
		return
	}
	w.stopOnce.Do(func() {
		close(w.stop)
		<-w.done
	})
}

// setErr 记录最近一次写盘错误（覆盖式，仅观测）。
func (w *archiveWriter) setErr(err error) {
	if err == nil {
		return
	}
	w.lastErrMu.Lock()
	w.lastErr = err.Error()
	w.lastErrMu.Unlock()
}

func (w *archiveWriter) errString() string {
	w.lastErrMu.Lock()
	defer w.lastErrMu.Unlock()
	return w.lastErr
}
