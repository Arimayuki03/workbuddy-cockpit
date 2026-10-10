// tokens.go 管理面作用域化 API Token（wbt_ 前缀）：生成、解析、校验与落盘。
//
// 供 CI / 脚本免交互调用面板 /api/* 只读端点（登录会话 cookie 与网关密钥
// wbk_ 均不适合该场景：前者要维护 cookie jar 与重登，后者授权的是 /v1/*
// 模型调用而非管理面——两者与 wbt_ 刻意区分，避免混用）。
//
// 安全约束（源自管理端提权后门事故的倒推清单，逐条对照 tokensvc.py 参考实现）：
//   - 明文只在创建响应里返回一次，库中只存 SHA-256 摘要 + 前 12 字符 prefix；
//     列表接口永不回传明文或哈希，任何日志 / 错误体也不回显；
//   - 解析先按 prefix 定位候选，再用 subtle 常量时间比较摘要，不全表裸比较；
//   - scope 只有两档（readonly / admin），非法值一律降级 readonly（最小权限：
//     宁可少授权让用户来问，也不因拼写错误意外给出管理能力）；
//   - token 只用于读数：所有写端点（含 admin scope 的幂等运维端点之外的一切
//     状态变更）在鉴权层一律 403 拒绝（token_write_forbidden）；token 管理
//     本身（/api/tokens*）只接受会话 cookie——泄露的 token 不能自助续命或提权；
//   - 吊销 / 停用即时生效：每次请求都走存储校验，不做鉴权结果缓存；
//   - last_used 更新 60s 节流（面板总览页 30s 轮询，逐请求写盘会把落盘打热）。
//
// 存储：data/panel_tokens.json（tmp + rename 原子写，0600，与 pool persist 同法）。
package panel

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// tokenPrefix 管理 token 前缀，与网关数据面密钥 wbk_ 刻意区分（两类凭据授权的
// 面完全不同，前缀差异让日志与配置里一眼可辨）。
const tokenPrefix = "wbt_"

// tokenPrefixLen 展示/定位用的明文前缀长度（"wbt_" + 8 个随机字符）。
const tokenPrefixLen = 12

// tokenScopes scope 全集：readonly（GET 类只读）/ admin（额外放开幂等运维端点）。
var tokenScopes = map[string]bool{"readonly": true, "admin": true}

// scopeReadonly / scopeAdmin scope 常量。
const (
	scopeReadonly = "readonly"
	scopeAdmin    = "admin"
)

// lastUsedThrottle last_used 更新节流窗口。
const lastUsedThrottle = 60 * time.Second

// tokenNameMax 名称与备注长度上限（防超长字段撑大落盘文件）。
const tokenNameMax = 64

// Token 一条管理面 API token 记录（落盘形态，panel_tokens.json 数组元素）。
// 明文永不落库：只有 sha256 摘要与用于展示/定位的 prefix。
type Token struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Prefix     string `json:"prefix"`
	SHA256     string `json:"sha256"` // hex(SHA-256(明文))
	Scope      string `json:"scope"`
	CreatedAt  int64  `json:"created_at"` // unix 秒
	LastUsedAt int64  `json:"last_used_at,omitempty"`
	Disabled   bool   `json:"disabled"`
}

// tokenFile 落盘文件形态（带版本号，后续演进可迁移）。
type tokenFile struct {
	Version int      `json:"version"`
	Tokens  []*Token `json:"tokens"`
}

// normalizeScope 归一化 scope；空串与非法值一律降级为最小权限 readonly
// （不报错——调用方传错不该被授予更高权限）。大小写与首尾空白归一化。
func normalizeScope(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if tokenScopes[v] {
		return v
	}
	return scopeReadonly
}

// hashToken 明文的 SHA-256 hex 摘要（比较两侧同用本函数，口径只有一份）。
func hashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// TokenStore 管理面 token 存储：内存全量 + 互斥保护 + 变更即落盘。
// Resolve 每次都读当前内存态（不做缓存），吊销/停用即时生效。
type TokenStore struct {
	mu     sync.Mutex
	tokens []*Token
	path   string // 落盘路径；空 = 纯内存（测试形态）
	// now 时钟注入点（测试节流用）；nil 时用 time.Now。
	now func() time.Time
}

// NewTokenStore 构建 token 存储并从 path 加载既有记录（文件缺失/损坏时零状态
// 启动，与 pool load 同口径——损坏文件不阻塞启动，也不静默重写）。path 为空
// 表示纯内存形态。
func NewTokenStore(path string) *TokenStore {
	s := &TokenStore{path: path}
	if raw, err := os.ReadFile(path); err == nil {
		var f tokenFile
		if json.Unmarshal(raw, &f) == nil {
			for _, t := range f.Tokens {
				if t != nil && validTokenRecord(t) {
					s.tokens = append(s.tokens, t)
				}
			}
		}
	}
	return s
}

// validTokenRecord 加载侧结构校验：缺摘要或前缀的残缺条目直接丢弃
// （手工脏数据防御，与 pool 恢复侧的过期过滤同思路）。
func validTokenRecord(t *Token) bool {
	return t.ID != "" && t.SHA256 != "" && strings.HasPrefix(t.Prefix, tokenPrefix)
}

// saveLocked 原子落盘（tmp + fsync + rename，0600；与 pool writeStateFileSync
// 同法）。落盘失败只记日志：内存态已生效，下次变更自然重试。
// 调用方必须已持 s.mu。
func (s *TokenStore) saveLocked() {
	if s.path == "" {
		return
	}
	f := tokenFile{Version: 1, Tokens: s.tokens}
	if s.tokens == nil {
		f.Tokens = []*Token{}
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		log.Printf("panel: panel_tokens.json 序列化失败: %v", err)
		return
	}
	tmp := s.path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		log.Printf("panel: panel_tokens.json 写盘失败: %v", err)
		return
	}
	if _, err := file.Write(raw); err != nil {
		file.Close()
		os.Remove(tmp)
		log.Printf("panel: panel_tokens.json 写盘失败: %v", err)
		return
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(tmp)
		log.Printf("panel: panel_tokens.json 写盘失败: %v", err)
		return
	}
	if err := file.Close(); err != nil {
		os.Remove(tmp)
		log.Printf("panel: panel_tokens.json 写盘失败: %v", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		log.Printf("panel: panel_tokens.json 写盘失败: %v", err)
	}
}

// newTokenID 生成记录 id（时间戳 + 随机尾巴，无中心分配器，够用且有序可读）。
func newTokenID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return time.Now().Format("20060102150405") + "-" + base64.RawURLEncoding.EncodeToString(b[:])
}

// Create 新建一条 token，返回记录与**仅此一次**的明文。
// name 截断到 64 字符，scope 非法降级 readonly。
func (s *TokenStore) Create(name, scope string) (*Token, string) {
	plain := tokenPrefix + base64.RawURLEncoding.EncodeToString(randomBytes(32))
	name = strings.TrimSpace(name)
	if name == "" {
		name = "unnamed"
	}
	if len(name) > tokenNameMax {
		name = name[:tokenNameMax]
	}
	t := &Token{
		ID:        newTokenID(),
		Name:      name,
		Prefix:    plain[:tokenPrefixLen],
		SHA256:    hashToken(plain),
		Scope:     normalizeScope(scope),
		CreatedAt: time.Now().Unix(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = append(s.tokens, t)
	s.saveLocked()
	return t, plain
}

// randomBytes 返回 n 字节 crypto/rand 随机数（失败 panic：随机源坏掉的进程
// 不可能安全地发行凭据，与 ssl/tls 标准库同立场）。
func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("panel: crypto/rand 不可用: " + err.Error())
	}
	return b
}

// List 返回全部记录的**副本**（含 prefix / scope / 最后使用；永不回传明文与
// 摘要——结构体里本就没有这两个字段以外的明文，sha256 字段由 handler 剥离）。
func (s *TokenStore) List() []Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Token, 0, len(s.tokens))
	for _, t := range s.tokens {
		out = append(out, *t)
	}
	return out
}

// Disable 启停一条 token（Disabled=false 即恢复）。不存在返回 false。
func (s *TokenStore) Disable(id string, disabled bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tokens {
		if t.ID == id {
			if t.Disabled != disabled {
				t.Disabled = disabled
				s.saveLocked()
			}
			return true
		}
	}
	return false
}

// Delete 删除一条 token。不存在返回 false。
func (s *TokenStore) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.tokens {
		if t.ID == id {
			s.tokens = append(s.tokens[:i], s.tokens[i+1:]...)
			s.saveLocked()
			return true
		}
	}
	return false
}

// Resolve 校验明文 token：先按 prefix 定位候选（s.mu 读锁窗口极短），再对
// 摘要做常量时间比较。未命中 / 已停用均返回 nil——**不区分**失败原因，不给
// 探测者任何信号（错误响应由调用方统一 401）。
func (s *TokenStore) Resolve(plain string) *Token {
	if !strings.HasPrefix(plain, tokenPrefix) {
		return nil
	}
	// 长度防御：恰好 4 字节前缀（如 "wbt_"）连 12 字节展示前缀都凑不齐，
	// 下方 plain[:tokenPrefixLen] 会越界 panic（未认证可稳定触发）。
	if len(plain) < tokenPrefixLen {
		return nil
	}
	digest := hashToken(plain)
	prefix := plain[:tokenPrefixLen]
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tokens {
		if t.Prefix != prefix {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(t.SHA256), []byte(digest)) == 1 {
			if t.Disabled {
				return nil
			}
			cp := *t
			return &cp
		}
	}
	return nil
}

// touch 记录最近使用时间，**60s 节流**（防高频轮询打热落盘）。记账是旁路：
// 任何异常都不影响请求本身，静默失败。
func (s *TokenStore) touch(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tokens {
		if t.ID != id {
			continue
		}
		now := s.clock().Unix()
		if t.LastUsedAt > 0 && now-t.LastUsedAt < int64(lastUsedThrottle/time.Second) {
			return
		}
		t.LastUsedAt = now
		s.saveLocked()
		return
	}
}

// clock 返回当前时刻（测试注入点；生产恒 time.Now）。
func (s *TokenStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// ---------------------------------------------------------------------------
// HTTP handler：token 管理端点（**仅会话 cookie 可用**，见 withAuthTokenAdmin）
// ---------------------------------------------------------------------------

// tokenView 列表响应条目：**不含摘要**（明文从未存储，摘要也不该回传给页面——
// 减少侧信道面；前缀已足够展示与定位）。
type tokenView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Prefix     string `json:"prefix"`
	Scope      string `json:"scope"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at,omitempty"`
	Disabled   bool   `json:"disabled"`
}

func viewOf(t *Token) tokenView {
	return tokenView{
		ID: t.ID, Name: t.Name, Prefix: t.Prefix, Scope: t.Scope,
		CreatedAt: t.CreatedAt, LastUsedAt: t.LastUsedAt, Disabled: t.Disabled,
	}
}

// handleTokenCreate POST /api/tokens body {"name":"ci-deploy","scope":"readonly"}。
// 明文只在本次响应里出现一次；scope 缺省 readonly。body 极小，loginBodyLimit 足够。
func (p *Panel) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, loginBodyLimit)).Decode(&body); err != nil && r.ContentLength != 0 {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	t, plain := p.tokenStore.Create(body.Name, body.Scope)
	log.Printf("panel: 创建 API token id=%s scope=%s prefix=%s", t.ID, t.Scope, t.Prefix)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"token":        plain, // 仅此一次
		"token_record": viewOf(t),
	})
}

// handleTokenList GET /api/tokens：全部记录（不含明文/摘要）。
func (p *Panel) handleTokenList(w http.ResponseWriter, r *http.Request) {
	toks := p.tokenStore.List()
	views := make([]tokenView, 0, len(toks))
	for i := range toks {
		views = append(views, tokenView{
			ID: toks[i].ID, Name: toks[i].Name, Prefix: toks[i].Prefix,
			Scope: toks[i].Scope, CreatedAt: toks[i].CreatedAt,
			LastUsedAt: toks[i].LastUsedAt, Disabled: toks[i].Disabled,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tokens": views})
}

// handleTokenDelete DELETE /api/tokens/{id}：吊销（即时生效）。
// 支持表单体 {"disabled":true} 停用而不删除（可选；缺省直接删除）。
func (p *Panel) handleTokenDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id required")
		return
	}
	// 可选 body：{"disabled":true} = 只停用（保留审计与 last_used 历史）；
	// {"disabled":false} = 恢复。缺省直接删除。
	var body struct {
		Disabled *bool `json:"disabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, loginBodyLimit)).Decode(&body); err != nil && r.ContentLength != 0 {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	var ok bool
	if body.Disabled != nil {
		ok = p.tokenStore.Disable(id, *body.Disabled)
	} else {
		ok = p.tokenStore.Delete(id)
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "token not found")
		return
	}
	log.Printf("panel: %s API token id=%s", opWord(body.Disabled), id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// opWord 日志用词：停用 / 恢复 / 删除。
func opWord(disabled *bool) string {
	if disabled == nil {
		return "删除"
	}
	if *disabled {
		return "停用"
	}
	return "恢复"
}

// ---------------------------------------------------------------------------
// Bearer wbt_ token 鉴权分支（withAuth 第三通道）
// ---------------------------------------------------------------------------

// tokenAuthCheck Bearer wbt_ token 校验结果。
type tokenAuthCheck struct {
	ok    bool
	scope string // 命中时为该 token 的归一化 scope
}

// checkTokenAuth 从 Authorization: Bearer 头解析 wbt_ token 并校验。
// 返回 ok=false 的情形（统一由调用方 401，不区分原因）：无 wbt_ 前缀、
// 摘要不匹配、已停用。网关 api_key（Bearer 不带 wbt_ 前缀）不进本函数——
// 调用顺序见 withAuth。
func (p *Panel) checkTokenAuth(r *http.Request) tokenAuthCheck {
	authz := r.Header.Get("Authorization")
	const scheme = "Bearer "
	if !strings.HasPrefix(authz, scheme) {
		return tokenAuthCheck{}
	}
	raw := authz[len(scheme):]
	if !strings.HasPrefix(raw, tokenPrefix) {
		return tokenAuthCheck{}
	}
	t := p.tokenStore.Resolve(raw)
	if t == nil {
		return tokenAuthCheck{}
	}
	p.tokenStore.touch(t.ID)
	return tokenAuthCheck{ok: true, scope: t.Scope}
}
