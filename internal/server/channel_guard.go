// channel_guard.go 上游「渠道校验」（HTTP 400 code=11-128）守卫。
//
// 背景：上游对 /v2/chat/completions 做渠道校验，判定请求不是官方客户端渠道发出时
// 返回 400 code=11-128（文案 "Illegal API invocation from an unapproved channel"
// 或中文「没通过渠道校验」）。该拒绝与**请求体**强相关，不是账号故障。
//
// 2026-10-28（manager 0039 吸收）起，11-128 按**请求级**处理：首次被拒先做一次
// system→user 内容通道抢救；仍被拒就透传上游原文回 400，**不冷却、不禁用、不换号**。
// 本守卫保留两项本地保护（都不涉及账号处罚）：
//  1. 抓取：ChannelCheck 发生时把当前出站 body 原样落盘（按 body 去重、总量封顶），
//     供离线定位触发指纹（system prompt / tools / 用户消息）；
//  2. 负缓存：按完整 body 哈希与模板哈希（system/developer 文本 + tools/functions）
//     记住**最终失败**的请求，短 TTL 内同 body/同模板直接本地 400——不选号、不冷却、
//     不再 503；
//  3. 预置短语：block_patterns 命中时在第一次出站前就本地拦截（默认空 = 不猜，
//     避免把正常请求误杀；抓到指纹后由运维显式加入）。
//
// 口径边界：负缓存是「上游实际拒绝过」的学习结果，不是对模板做白名单/黑名单猜测。
// 模板哈希只取 system/developer + tools/functions（对话内容千变万化，不能当模板）；
// 完整 body 哈希负责同请求重试的精确抑制。两者 TTL 都短，默认 10 分钟——渠道校验
// 可能按账号/时段波动，宁可在窗口后重试，也不长期误杀。
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// channelGuardDefaultBlockTTL 完整 body 负缓存默认 TTL。
	channelGuardDefaultBlockTTL = 10 * time.Minute
	// channelGuardDefaultTemplateTTL 模板负缓存默认 TTL。与 body 同量级：渠道校验
	// 可能按账号/时段波动，窗口过长会把本可成功的同模板请求长期拦在本地。
	channelGuardDefaultTemplateTTL = 10 * time.Minute
	// channelGuardDefaultCaptureMaxBytes 抓取目录总容量默认上限（256MB）。
	channelGuardDefaultCaptureMaxBytes = int64(256) << 20
	// channelGuardCapturePrefix 抓取文件名前缀。目录里只认本前缀，避免误删
	// 用户放进同目录的其它文件。
	channelGuardCapturePrefix = "channel-check-"
	// channelGuardPreviewBytes 元数据里 system 预览/上游原文的截断上限。
	channelGuardPreviewBytes = 4 << 10
	// channelGuardMaxPatternLen 单条 block_patterns 的长度上限（防御配置手滑）。
	channelGuardMaxPatternLen = 512
)

// ChannelGuardConfig 渠道校验守卫配置（features.channel_check_guard）。
// 零值 = 关闭（测试/未接线形态零行为）；生产由 config.Default() 打开。
type ChannelGuardConfig struct {
	Enabled         bool          // 总开关
	Mode            string        // "block"（默认，本地 400）/ "warn"（只记日志继续出站）/ "off"
	CaptureDir      string        // 抓取目录（默认 data/channel_check）
	CaptureMaxBytes int64         // 目录总容量上限（字节，<=0 回落默认 256MB）
	BlockTTL        time.Duration // 完整 body 负缓存 TTL（<=0 回落默认 10m）
	TemplateTTL     time.Duration // 模板负缓存 TTL（<=0 回落默认 10m）
	ScanMessages    bool          // block_patterns 是否扫描全部消息（true）还是只扫 system/tools
	BlockPatterns   []string      // 预置拦截短语（大小写不敏感子串）
	// ChannelCooldown 已废弃（manager 0039）：11-128 改为请求级处理，不冷却账号。
	// 保留字段仅为兼容旧配置，取值被忽略。
	ChannelCooldown time.Duration
	// FailFast 已废弃（manager 0039）：11-128 始终请求级处理（一次 system→user
	// 抢救后仍拒即透传 400），不再换号。保留字段仅为兼容旧配置，取值被忽略。
	FailFast bool
}

// ChannelGuardAtom 被抓取请求的可读元数据（写 .meta.json，不进正文文件）。
type ChannelGuardMeta struct {
	Model        string
	Realm        string
	Mode         string // "stream" | "sync"
	Account      string // "昵称(uid8)" 标签
	Status       int    // 上游 HTTP 状态码
	UpstreamBody string // 上游错误原文（截断 4KB）
	RequestID    string // X-Conversation-Request-ID
	ClientIP     string // 按 logging.request_client_info 开关填充，空则不写
	UserAgent    string
}

// ChannelGuardHit 预检命中结果。
type ChannelGuardHit struct {
	Kind         string // "body" | "template" | "pattern"
	Reason       string
	BodyHash     string
	TemplateHash string
	CapturePath  string
	Pattern      string
}

// ChannelGuardNote NoteRejected 的返回值（日志用）。
type ChannelGuardNote struct {
	BodyHash     string
	TemplateHash string
	CapturePath  string
	Captured     bool
	MessageCount int
	ToolCount    int
}

// ChannelGuard 渠道校验守卫。并发安全；nil 接收者安全（等同于关闭）。
type ChannelGuard struct {
	cfg ChannelGuardConfig
	now func() time.Time

	mu            sync.Mutex
	bodies        map[string]time.Time // body hash -> 过期时刻
	templates     map[string]time.Time // template hash -> 过期时刻
	captures      map[string]string    // body/template hash -> 抓取文件路径
	capturedShort map[string]bool      // body hash 前 16 位（文件名去重键）
	capturedBytes int64
}

// NewChannelGuard 构建守卫并扫描既有抓取目录（重启后仍能维持去重与容量口径）。
// 目录创建失败只降级抓取（blocking 仍生效），不阻塞网关启动。
func NewChannelGuard(cfg ChannelGuardConfig) *ChannelGuard {
	if cfg.Mode == "" {
		cfg.Mode = "block"
	}
	cfg.Mode = strings.ToLower(strings.TrimSpace(cfg.Mode))
	if cfg.CaptureDir == "" {
		cfg.CaptureDir = "data/channel_check"
	}
	if cfg.CaptureMaxBytes <= 0 {
		cfg.CaptureMaxBytes = channelGuardDefaultCaptureMaxBytes
	}
	if cfg.BlockTTL <= 0 {
		cfg.BlockTTL = channelGuardDefaultBlockTTL
	}
	if cfg.TemplateTTL <= 0 {
		cfg.TemplateTTL = channelGuardDefaultTemplateTTL
	}
	// 配置短语归一：去空白、丢空段、限长（超长条目直接丢弃并记日志——它是手写
	// 配置，不该让一个笔误把整段请求都拦掉）。
	patterns := make([]string, 0, len(cfg.BlockPatterns))
	for _, p := range cfg.BlockPatterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if len(p) > channelGuardMaxPatternLen {
			log.Printf("WARN: [channel-guard] 忽略超长 block_pattern（%d > %d）", len(p), channelGuardMaxPatternLen)
			continue
		}
		patterns = append(patterns, p)
	}
	cfg.BlockPatterns = patterns
	g := &ChannelGuard{
		cfg:           cfg,
		now:           time.Now,
		bodies:        make(map[string]time.Time),
		templates:     make(map[string]time.Time),
		captures:      make(map[string]string),
		capturedShort: make(map[string]bool),
	}
	if !cfg.Enabled || cfg.Mode == "off" {
		return g
	}
	g.scanCaptureDir()
	return g
}

// Enabled 守卫是否启用。
func (g *ChannelGuard) Enabled() bool {
	return g != nil && g.cfg.Enabled && g.cfg.Mode != "off"
}

// Mode 当前模式（"block" / "warn" / "off"）。
func (g *ChannelGuard) Mode() string {
	if g == nil {
		return "off"
	}
	return g.cfg.Mode
}

// FailFast 已废弃（manager 0039）：11-128 始终请求级处理。保留方法仅为兼容
// 既有调用方/测试，生产路径不再读取。
func (g *ChannelGuard) FailFast() bool { return g != nil && g.cfg.FailFast }

// ChannelCooldown 已废弃（manager 0039）：11-128 不冷却账号。保留方法仅为兼容
// 既有调用方/测试，生产路径不再读取。
func (g *ChannelGuard) ChannelCooldown() time.Duration {
	if g == nil {
		return 0
	}
	return g.cfg.ChannelCooldown
}

// Check 请求出站前的预检：命中已知坏 body / 坏模板 / 配置短语时返回命中详情。
// 未启用、空 body、无缓存且无短语时零开销返回 nil。
func (g *ChannelGuard) Check(body []byte) *ChannelGuardHit {
	if !g.Enabled() || len(body) == 0 {
		return nil
	}
	now := g.now()
	bh := channelBodyHash(body)

	g.mu.Lock()
	defer g.mu.Unlock()
	g.purgeLocked(now)

	if until, ok := g.bodies[bh]; ok && now.Before(until) {
		return &ChannelGuardHit{
			Kind:        "body",
			Reason:      "identical request body was rejected by upstream channel verification",
			BodyHash:    bh,
			CapturePath: g.captures[bh],
		}
	}

	// 模板哈希/短语匹配需要解析 body：只有确实存在模板缓存或配置短语时才付解析成本。
	if len(g.templates) == 0 && len(g.cfg.BlockPatterns) == 0 {
		return nil
	}
	ti := parseChannelTemplate(body, g.cfg.ScanMessages)
	if ti.Hash != "" {
		if until, ok := g.templates[ti.Hash]; ok && now.Before(until) {
			return &ChannelGuardHit{
				Kind:         "template",
				Reason:       "request template (system/tools) was rejected by upstream channel verification",
				BodyHash:     bh,
				TemplateHash: ti.Hash,
				CapturePath:  g.captures[ti.Hash],
			}
		}
	}
	lower := strings.ToLower(ti.ScanText)
	for _, p := range g.cfg.BlockPatterns {
		if strings.Contains(lower, strings.ToLower(p)) {
			return &ChannelGuardHit{
				Kind:         "pattern",
				Reason:       fmt.Sprintf("request matched local channel-check block pattern %q", p),
				BodyHash:     bh,
				TemplateHash: ti.Hash,
				CapturePath:  g.captures[ti.Hash],
				Pattern:      p,
			}
		}
	}
	return nil
}

// NoteRejected 记录一次上游 ChannelCheck：落盘抓取（每个 body 一次）+ 写入负缓存。
// 返回抓取路径/hash 供调用方打日志；未启用时返回 nil。
func (g *ChannelGuard) NoteRejected(body []byte, meta ChannelGuardMeta) *ChannelGuardNote {
	if !g.Enabled() || len(body) == 0 {
		return nil
	}
	now := g.now()
	bh := channelBodyHash(body)
	ti := parseChannelTemplate(body, false)

	g.mu.Lock()
	defer g.mu.Unlock()
	g.purgeLocked(now)
	g.bodies[bh] = now.Add(g.cfg.BlockTTL)
	if ti.Hash != "" {
		g.templates[ti.Hash] = now.Add(g.cfg.TemplateTTL)
	}
	n := &ChannelGuardNote{
		BodyHash:     bh,
		TemplateHash: ti.Hash,
		MessageCount: ti.MessageCount,
		ToolCount:    ti.ToolCount,
	}
	short := shortHash(bh)
	if g.capturedShort[short] {
		n.CapturePath = g.captures[bh]
		return n
	}
	path, ok := g.writeCaptureLocked(body, bh, ti, meta, now)
	if ok {
		g.capturedShort[short] = true
		g.captures[bh] = path
		if ti.Hash != "" {
			g.captures[ti.Hash] = path
		}
		n.CapturePath = path
		n.Captured = true
	}
	return n
}

// NoteBlocked 预检命中（body/模板负缓存或配置短语）时的抓取：不新增缓存（命中
// 即已在缓存或走配置短语），只把 body 落盘供运维核对拦截是否误伤。短语命中而
// 上游从未拒绝过时，这里是唯一的现场证据。
func (g *ChannelGuard) NoteBlocked(body []byte, meta ChannelGuardMeta) *ChannelGuardNote {
	if !g.Enabled() || len(body) == 0 {
		return nil
	}
	now := g.now()
	bh := channelBodyHash(body)
	ti := parseChannelTemplate(body, false)

	g.mu.Lock()
	defer g.mu.Unlock()
	g.purgeLocked(now)
	short := shortHash(bh)
	if g.capturedShort[short] {
		return &ChannelGuardNote{BodyHash: bh, TemplateHash: ti.Hash, CapturePath: g.captures[bh],
			MessageCount: ti.MessageCount, ToolCount: ti.ToolCount}
	}
	path, ok := g.writeCaptureLocked(body, bh, ti, meta, now)
	if ok {
		g.capturedShort[short] = true
		g.captures[bh] = path
		if ti.Hash != "" {
			g.captures[ti.Hash] = path
		}
	}
	return &ChannelGuardNote{BodyHash: bh, TemplateHash: ti.Hash, CapturePath: path, Captured: ok,
		MessageCount: ti.MessageCount, ToolCount: ti.ToolCount}
}

// purgeLocked 清理过期负缓存（每次 Check/Note 调用，均摊 O(过期条目)）。
func (g *ChannelGuard) purgeLocked(now time.Time) {
	for k, until := range g.bodies {
		if !now.Before(until) {
			delete(g.bodies, k)
		}
	}
	for k, until := range g.templates {
		if !now.Before(until) {
			delete(g.templates, k)
			delete(g.captures, k)
		}
	}
}

// channelTemplate 从请求体提取的模板信息。
type channelTemplate struct {
	Hash         string // system/developer + tools/functions 的 SHA-256；无模板时为空
	TemplateText string
	ScanText     string // 模板文本 + （ScanMessages 时）全部消息文本，供短语匹配
	SystemPrev   string
	MessageCount int
	ToolCount    int
}

// parseChannelTemplate 解析 outbound body 的模板骨架。畸形 body 返回零值（上游
// 通常报 11101 而非渠道校验，不需要模板判定）。
func parseChannelTemplate(body []byte, scanMessages bool) channelTemplate {
	var obj struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools          json.RawMessage `json:"tools"`
		Functions      json.RawMessage `json:"functions"`
		ResponseFormat json.RawMessage `json:"response_format"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return channelTemplate{}
	}
	var tpl, scan strings.Builder
	ti := channelTemplate{MessageCount: len(obj.Messages)}
	for _, m := range obj.Messages {
		text := channelContentText(m.Content)
		if m.Role == "system" || m.Role == "developer" {
			tpl.WriteString(text)
			tpl.WriteByte('\n')
			// 模板文本始终参与短语匹配（scan_messages 只控制是否再扫用户/助手消息）。
			scan.WriteString(text)
			scan.WriteByte('\n')
			if ti.SystemPrev == "" {
				ti.SystemPrev = truncateRunes(text, channelGuardPreviewBytes)
			}
		} else if scanMessages {
			scan.WriteString(text)
			scan.WriteByte('\n')
		}
	}
	appendCanonical := func(raw json.RawMessage) int {
		if len(raw) == 0 || string(raw) == "null" {
			return 0
		}
		var v any
		if json.Unmarshal(raw, &v) != nil {
			return 0
		}
		b, err := json.Marshal(v)
		if err != nil {
			return 0
		}
		tpl.Write(b)
		tpl.WriteByte('\n')
		scan.Write(b)
		scan.WriteByte('\n')
		if arr, ok := v.([]any); ok {
			return len(arr)
		}
		return 1
	}
	ti.ToolCount = appendCanonical(obj.Tools) + appendCanonical(obj.Functions)
	appendCanonical(obj.ResponseFormat)

	ti.TemplateText = normalizeTemplateText(tpl.String())
	ti.ScanText = normalizeTemplateText(scan.String())
	if ti.TemplateText == "" {
		return ti
	}
	sum := sha256.Sum256([]byte(ti.TemplateText))
	ti.Hash = hex.EncodeToString(sum[:])
	return ti
}

// channelContentText 提取消息文本：字符串 / 多段 part 的 text/content 字段；
// 其它形态（图片 part 等）用 JSON 原文参与匹配，避免漏掉藏在 part 里的指纹。
func channelContentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []any
	if json.Unmarshal(raw, &parts) != nil {
		return string(raw)
	}
	var b strings.Builder
	for _, p := range parts {
		switch v := p.(type) {
		case string:
			b.WriteString(v)
		case map[string]any:
			if t, ok := v["text"].(string); ok && t != "" {
				b.WriteString(t)
			} else if c, ok := v["content"].(string); ok && c != "" {
				b.WriteString(c)
			} else if rawPart, err := json.Marshal(v); err == nil {
				b.Write(rawPart)
			}
		default:
			if rawPart, err := json.Marshal(v); err == nil {
				b.Write(rawPart)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// normalizeTemplateText 模板归一：统一换行、去行尾空白、压缩连续空行、去首尾空白。
// 只做确定性的空白归一，不改内容——让同一模板在不同客户端的换行差异下仍同哈希。
func normalizeTemplateText(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	var b strings.Builder
	blank := false
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			if blank {
				continue
			}
			blank = true
		} else {
			blank = false
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

// captureMeta 抓取目录里的 .meta.json 结构（正文在同名 .json 文件）。
type captureMeta struct {
	Time           string   `json:"time"`
	Model          string   `json:"model,omitempty"`
	Realm          string   `json:"realm,omitempty"`
	Mode           string   `json:"mode,omitempty"`
	Account        string   `json:"account,omitempty"`
	Status         int      `json:"status,omitempty"`
	UpstreamBody   string   `json:"upstream_body,omitempty"`
	BodySHA256     string   `json:"body_sha256"`
	TemplateSHA256 string   `json:"template_sha256,omitempty"`
	SystemPreview  string   `json:"system_preview,omitempty"`
	MessageCount   int      `json:"message_count"`
	ToolCount      int      `json:"tool_count"`
	BlockPatterns  []string `json:"block_patterns,omitempty"`
	RequestID      string   `json:"request_id,omitempty"`
	ClientIP       string   `json:"client_ip,omitempty"`
	UserAgent      string   `json:"user_agent,omitempty"`
	BodyBytes      int      `json:"body_bytes"`
	Truncated      bool     `json:"truncated,omitempty"`
}

// writeCaptureLocked 落盘一次抓取：<prefix><ts>-<body16>.json 正文 +
// 同名 .meta.json。调用方持锁。总容量超限时先删最旧文件；仍放不下则放弃本次抓取
// （负缓存照常生效，不能因为磁盘满而阻塞请求路径）。
func (g *ChannelGuard) writeCaptureLocked(body []byte, bh string, ti channelTemplate, meta ChannelGuardMeta, now time.Time) (string, bool) {
	if err := os.MkdirAll(g.cfg.CaptureDir, 0o700); err != nil {
		log.Printf("WARN: [channel-guard] 抓取目录不可用 dir=%s: %v（负缓存仍生效）", g.cfg.CaptureDir, err)
		return "", false
	}
	short := shortHash(bh)
	name := fmt.Sprintf("%s%s-%s.json", channelGuardCapturePrefix, now.Format("20060102-150405"), short)
	path := filepath.Join(g.cfg.CaptureDir, name)
	if _, err := os.Stat(path); err == nil {
		return path, false
	}
	need := int64(len(body)) + 64<<10 // 正文 + meta 预留
	g.pruneLocked(need)
	if g.capturedBytes+int64(len(body)) > g.cfg.CaptureMaxBytes {
		log.Printf("WARN: [channel-guard] 抓取容量已满（%d/%d 字节），跳过本次落盘 body=%s",
			g.capturedBytes, g.cfg.CaptureMaxBytes, short)
		return "", false
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		log.Printf("WARN: [channel-guard] 写抓取文件失败 path=%s: %v", path, err)
		return "", false
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		log.Printf("WARN: [channel-guard] 写抓取文件失败 path=%s: %v", path, err)
		return "", false
	}
	if err := f.Close(); err != nil {
		log.Printf("WARN: [channel-guard] 关闭抓取文件失败 path=%s: %v", path, err)
	}
	g.capturedBytes += int64(len(body))

	cm := captureMeta{
		Time:           now.Format(time.RFC3339),
		Model:          meta.Model,
		Realm:          meta.Realm,
		Mode:           meta.Mode,
		Account:        meta.Account,
		Status:         meta.Status,
		UpstreamBody:   truncateRunes(meta.UpstreamBody, channelGuardPreviewBytes),
		BodySHA256:     bh,
		TemplateSHA256: ti.Hash,
		SystemPreview:  ti.SystemPrev,
		MessageCount:   ti.MessageCount,
		ToolCount:      ti.ToolCount,
		RequestID:      meta.RequestID,
		ClientIP:       meta.ClientIP,
		UserAgent:      meta.UserAgent,
		BodyBytes:      len(body),
	}
	if len(g.cfg.BlockPatterns) > 0 {
		cm.BlockPatterns = append([]string(nil), g.cfg.BlockPatterns...)
	}
	if raw, err := json.MarshalIndent(cm, "", "  "); err == nil {
		metaPath := strings.TrimSuffix(path, ".json") + ".meta.json"
		if err := os.WriteFile(metaPath, raw, 0o600); err != nil {
			log.Printf("WARN: [channel-guard] 写抓取元数据失败 path=%s: %v", metaPath, err)
		} else {
			g.capturedBytes += int64(len(raw))
		}
	}
	return path, true
}

// pruneLocked 按文件名时间序删除最旧抓取文件，直到能容纳 need 字节（或只剩当前文件）。
// 只动本守卫前缀的文件；调用方持锁。
func (g *ChannelGuard) pruneLocked(need int64) {
	if g.cfg.CaptureMaxBytes <= 0 {
		return
	}
	if g.capturedBytes+need <= g.cfg.CaptureMaxBytes {
		return
	}
	entries, err := os.ReadDir(g.cfg.CaptureDir)
	if err != nil {
		return
	}
	type item struct {
		path string
		size int64
	}
	items := make([]item, 0, len(entries))
	var total int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), channelGuardCapturePrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{path: filepath.Join(g.cfg.CaptureDir, e.Name()), size: info.Size()})
		total += info.Size()
	}
	// 文件名前缀带时间戳，字典序即时间序（同秒内顺序不影响“删最旧”语义）。
	sort.Slice(items, func(i, j int) bool { return items[i].path < items[j].path })
	for _, it := range items {
		if total+need <= g.cfg.CaptureMaxBytes {
			break
		}
		if os.Remove(it.path) == nil {
			total -= it.size
		}
	}
	g.capturedBytes = total
}

// scanCaptureDir 启动时统计既有抓取文件：维持去重键与容量计数（重启后不会重复
// 落同一 body，也不会把目录当成空的重头写）。
func (g *ChannelGuard) scanCaptureDir() {
	entries, err := os.ReadDir(g.cfg.CaptureDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("WARN: [channel-guard] 扫描抓取目录失败 dir=%s: %v", g.cfg.CaptureDir, err)
		}
		return
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), channelGuardCapturePrefix) {
			continue
		}
		info, err := e.Info()
		if err == nil {
			total += info.Size()
		}
		if strings.HasSuffix(e.Name(), ".json") && !strings.HasSuffix(e.Name(), ".meta.json") {
			// 文件名：channel-check-<ts>-<body16>.json
			base := strings.TrimSuffix(e.Name(), ".json")
			if i := strings.LastIndex(base, "-"); i >= 0 && len(base[i+1:]) == 16 {
				g.capturedShort[base[i+1:]] = true
			}
		}
	}
	g.capturedBytes = total
}

// channelBodyHash 完整 body 的 SHA-256（十六进制）。
func channelBodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// shortHash 取哈希前 16 位作文件名/日志短键。
func shortHash(h string) string {
	if len(h) > 16 {
		return h[:16]
	}
	return h
}

// truncateRunes 按 rune 截断（日志/元数据预览用，不切断 UTF-8）。
func truncateRunes(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
