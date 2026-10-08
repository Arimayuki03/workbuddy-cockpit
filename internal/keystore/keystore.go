// Package keystore 多密钥分发系统的存储与校验核心（吸收 workbuddy-manager keysvc.py）。
//
// 背景：网关原先只有单一 api_key 鉴权（server.handler 的 apiKey()+httpauth.VerifyBearer，
// handler.go:131），下游用户共用一把 key。本包把鉴权拆成可分发的多把独立密钥：
// 每把各自限额、白名单、有效期，启停互不影响。本包只做存储与判定，不改网关接线
// （接线由调用方完成）：Resolve 定位密钥 → Validate 判请求 → 调用方执行上游请求 →
// RecordUse 成功记账。
//
// 语义对齐参考实现（Python keysvc.py）的实战沉淀：
//
//   - 密钥形态：明文 = "wbk_" + 32 字节 crypto/rand 的 base64.RawURLEncoding。
//     库中只存 SHA-256 hex 摘要 + 明文前 12 字符 prefix，明文仅 Create 返回一次、
//     永不落盘。Resolve 按 prefix 建 map 索引定位候选，再用 subtle 常量时间比较
//     哈希——不全表扫，比较耗时与命中与否无关。
//   - 拒绝按「该怪谁」分流（直接决定 HTTP 状态码）。参考实现 issue #18 的现场：
//     密钥限定了国内版、却调国际版模型，这类**配置问题**被客户端显示成「密钥无效」，
//     用户反复新建密钥永远好不了。故：
//       400 invalid_request_error：realm_mismatch / model_not_allowed / ip_not_allowed
//         —— 「这次请求的参数不对」，客户端原样透出报文，用户一眼看到该改什么；
//       403：key_disabled / key_expired / max_ips_exceeded —— 凭据不可用，
//         显示成「密钥无效」是贴切的；
//       429 insufficient_quota：token_quota_exhausted / credit_quota_exhausted /
//         rate_limited —— 配额用尽与认证无关，403 会被读成密钥无效，把用户往
//         排查方向带偏（OpenAI 同语义用 429）。
//   - 每密钥限流（固定 60s 窗口）：**只有放行成功的请求才占窗口额度**。被拒请求
//     不计数——否则客户端重试与限流构成正反馈，限流器变熔断永不恢复（参考项目
//     issue #52 的真坑）。因此 Validate 只读不写，窗口与用量在 RecordUse（成功后）
//     才落账；IP 记忆同理，放行后由 RecordUse 落入，Validate 只按已有记忆判 MaxIPs。
//   - 写侧归一化：IP 白名单逐项去空白（参考实现 _norm_cidrs 的实测坑：带空格的
//     CIDR 能通过面板校验、存库后却匹配不上任何 IP，密钥对所有来源被拒而报错
//     看不出原因——校验与存储必须是同一份数据），且 Create/Update 当场校验 CIDR
//     语法（fail-fast，面板当场提示），匹配侧 iputil.Matches 对非法规则仍恒 false
//     （fail-closed 双保险）。
//
// 模型名归一化口径（与 internal/server/resolve_model.go 保持一致）：
// server.resolveModel 的前缀协议是「第一个 ':' 前段恰为 cn/global 才剥离，否则视为
// 裸名（默认 realm=cn）」，大小写敏感。本包 bareModel 对齐：剥 `cn:` 前缀（它只是
// 上游的路由约定而非模型名的一部分，界面上显示裸名、用户照着填，存量白名单可能
// 写带前缀的形态，归一后两种写法等价）；**保留 `global:`**——它决定路由到哪个
// 账号池，两个版本的同名模型不是一回事。realm 判定由调用方传入**模型映射之后**
// 的模型名与 ResolveModel 解析出的 realm（用请求名判，配了别名映射的密钥会永远
// 被 realm 检查打回——参考实现 issue #47），本包 realmOf 仅作 realm 参数缺省时的
// 推断兜底（裸名=cn，与 resolveModel 同口径）。
//
// 并发模型：热路径（Resolve/Validate/RecordUse）每请求都走。Key 是纯数据结构
// （无锁、可值拷贝），单 RWMutex 保护库内权威状态；Resolve/List 返回深拷贝快照
// （调用方可安全持有，Validate 对快照是纯函数、无锁）；RecordUse/Update 等写路径
// 短临界区（密钥量级为个位数~几十，RWMutex 无竞争热点）。没用 livecfg 的
// atomic.Pointer 快照：那里的读对象是「整体替换的配置」，这里 RecordUse 每请求
// 都要改计数器——纯快照会让计数器的权威归属跨代漂移（旧代记账丢失），RWMutex+
// clone 语义更直白且 -race 可证。持久化 tmp+rename 原子写、0600 权限、防抖合并
// （参照 pool/persist.go 的 writeStateFileSync）。只用标准库。
package keystore

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/iputil"
)

// ---------------------------------------------------------------------------
// 密钥模型
// ---------------------------------------------------------------------------

// KeyPrefix 明文密钥前缀。
const KeyPrefix = "wbk_"

// RateWindow 每密钥限流的固定窗口（60s）。窗口起点按 unix 秒向下取整到整窗口，
// 重启后窗口错位到整分钟——对「每 60s N 次」的限流语义无损。窗口计数不落盘：
// 重启即视为全新窗口，宁可少计（放行）也不误拒，与「被拒请求不占额度」同一取向。
const RateWindow = 60 * time.Second

// 拒绝短码（稳定，后续 i18n 用；取值与参考实现 Rejection.code 同源）。
const (
	ReasonRealmMismatch        = "realm_mismatch"
	ReasonModelNotAllowed      = "model_not_allowed"
	ReasonIPNotAllowed         = "ip_not_allowed"
	ReasonKeyDisabled          = "key_disabled"
	ReasonKeyExpired           = "key_expired"
	ReasonMaxIPsExceeded       = "max_ips_exceeded"
	ReasonTokenQuotaExhausted  = "token_quota_exhausted"
	ReasonCreditQuotaExhausted = "credit_quota_exhausted"
	ReasonRateLimited          = "rate_limited"
)

// Rejection 一次拒绝的原因与对应 HTTP 状态码。接线方按 Status 分流、按 Reason
// 细化文案（Reason 是稳定短码）。
type Rejection struct {
	Status int    // 建议回给客户端的 HTTP 状态码（400/403/429）
	Reason string // 稳定短码，见包注释的拒绝分类学
}

func (r *Rejection) Error() string {
	return fmt.Sprintf("keystore: 拒绝 status=%d reason=%s", r.Status, r.Reason)
}

// Key 一把可分发密钥。零值语义：ExpiresAt=0 永不过期、MaxIPs=0 不限 IP 数、
// IPWhitelist/ModelWhitelist 空=不限、TokenQuota/CreditQuota=0 不限、
// RateLimit=0 无每密钥限流（调用方的全局默认限流在接线层另行施加）。
// 库中（磁盘与内存）只有哈希与 prefix，明文永不保存。
//
// 类型约束：Key 必须保持「纯数据、可值拷贝」（不得加入锁/chan 等不可拷贝字段）
// ——Resolve/List 的 clone 快照语义依赖这一点（见包注释的并发模型）。
type Key struct {
	ID             string   `json:"id"`                        // crypto/rand hex
	Name           string   `json:"name"`                      // 展示名
	Enabled        bool     `json:"enabled"`                   // 启停
	Prefix         string   `json:"prefix"`                    // 明文前 12 字符（wbk_+8），Resolve 索引
	Hash           string   `json:"hash"`                      // 明文 SHA-256 hex；鉴权凭据本体
	CreatedAt      int64    `json:"created_at"`                // unix 秒
	ExpiresAt      int64    `json:"expires_at"`                // unix 秒；0=永不过期
	MaxIPs         int      `json:"max_ips"`                   // 可用来源 IP 数上限；0=不限
	IPWhitelist    []string `json:"ip_whitelist,omitempty"`    // CIDR/单 IP 列表；空=不限
	ModelWhitelist []string `json:"model_whitelist,omitempty"` // 模型名列表；空=不限
	Realm          string   `json:"realm,omitempty"`           // ""/cn/global；判映射之后的模型名
	TokenQuota     int64    `json:"token_quota"`               // token 总额；0=不限
	CreditQuota    float64  `json:"credit_quota"`              // 积分总额；0=不限
	UsedTokens     int64    `json:"used_tokens"`               // 按上游真实 usage 累计
	UsedCredits    float64  `json:"used_credits"`              // 按上游真实扣费累计
	LastUsedAt     int64    `json:"last_used_at"`              // unix 秒；0=从未使用
	RateLimit      int      `json:"rate_limit"`                // 每 60s 窗口放行成功次数；0=不限

	// 运行态（不导出、不序列化）：限流窗口与来源 IP 表。只随 keyFile 落盘 ips。
	rateWindowStart int64            // 当前窗口起点（unix 秒，向下取整到 RateWindow）
	rateCount       int              // 当前窗口内放行成功次数
	ips             map[string]int64 // ip → first_seen（unix 秒）
}

// IsExpired 报告密钥在 now 时刻是否已过期（ExpiresAt=0 永不过期；恰在到期秒即过期，
// 与参考实现 expires_at < time.time() 同口径取闭区间）。
func (k *Key) IsExpired(now time.Time) bool {
	return k.ExpiresAt > 0 && k.ExpiresAt <= now.Unix()
}

// clone 深拷贝（Resolve/List/Update/flush 的快照语义依赖：调用方改切片/map 不得
// 影响库内状态，库内改也不得撕裂调用方正在读的视图）。Key 必须保持纯数据（见
// 类型注释），值拷贝 + 三个引用字段的显式深拷贝即完整。
func (k *Key) clone() *Key {
	dup := *k
	if k.IPWhitelist != nil {
		dup.IPWhitelist = append([]string(nil), k.IPWhitelist...)
	}
	if k.ModelWhitelist != nil {
		dup.ModelWhitelist = append([]string(nil), k.ModelWhitelist...)
	}
	if k.ips != nil {
		dup.ips = make(map[string]int64, len(k.ips))
		for ip, ts := range k.ips {
			dup.ips[ip] = ts
		}
	}
	return &dup
}

// keyFile 落盘形态：Key 全部导出字段（json tag 直通）+ 运行态中的来源 IP 表。
// 窗口计数不落盘（RateWindow 注释）。 ips 单列在顶层而不用嵌套：与 Key 的其余
// 字段平铺，手工排查文件时一眼可见。
type keyFile struct {
	Key
	IPs map[string]int64 `json:"ips,omitempty"`
}

// ---------------------------------------------------------------------------
// 模型名归一化（口径对齐 internal/server/resolve_model.go，见包注释）
// ---------------------------------------------------------------------------

// bareModel 白名单比对用的模型名归一：剥 `cn:` 前缀，**保留 `global:`**。
// 大小写敏感（与 resolveModel 的「前缀必须是精确的小写枚举」同口径——参考实现
// _bare_model 是小写不敏感，此处从 server 口径，理由见包注释）。
func bareModel(model string) string {
	if strings.HasPrefix(model, "cn:") {
		return model[3:]
	}
	return model
}

// realmOf 模型名的版本归属推断：`global:` 前缀 = global，其余（含 `cn:` 前缀与
// 裸名）= cn——与 resolveModel「裸名默认 realm=cn」同口径。仅当调用方未传 realm
// 时兜底；主判据是调用方传入的 ResolveModel 解析结果。
func realmOf(model string) string {
	if strings.HasPrefix(model, "global:") {
		return "global"
	}
	return "cn"
}

// normRealm 归一版本归属：去空白 + 小写后仅认 cn/global，其余（含空）= 不限制
// （存量密钥的形态；参考实现 _norm_realm 同语义，管理端脏值不报错）。
func normRealm(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "cn":
		return "cn"
	case "global":
		return "global"
	default:
		return ""
	}
}

// normList 归一字符串列表（IP 白名单 / 模型白名单共用）：逐项去空白、丢空项；
// 全空返回 nil（空切片与 nil 同义 = 不限制，JSON 里以 omitempty 省略）。
func normList(items []string) []string {
	out := make([]string, 0, len(items))
	for _, raw := range items {
		if s := strings.TrimSpace(raw); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// validateCIDRs IP 白名单的写侧语法校验（fail-fast：非法条目让 Create/Update 当场
// 报错，面板能提示修正，而不是留到运行时表现为「对所有来源都被拒」且报错看不出
// 原因——参考实现 _norm_cidrs 注释的现场）。单 IP 与 CIDR 均合法，与 iputil.Matches
// 的匹配口径同一来源。
func validateCIDRs(items []string) error {
	_, errs := iputil.ParseCIDRs(items)
	if len(errs) > 0 {
		return fmt.Errorf("keystore: 非法的 IP/CIDR 白名单条目: %s", strings.Join(errs, "; "))
	}
	return nil
}

// clampKey Create/Update 共用的数值收敛：负值一律钳到 0。额度负值钳 0（=不限）
// 而非当「已超限」——参考实现 _norm_credit_quota 的取舍：格式脏值不该让保存失败，
// 而钳到「不限」绝不会把密钥悄悄放行成「已超限」（那会让线上调用突然全 429，
// 方向反了）。
func clampKey(v *Key) {
	if v.ExpiresAt < 0 {
		v.ExpiresAt = 0
	}
	if v.MaxIPs < 0 {
		v.MaxIPs = 0
	}
	if v.TokenQuota < 0 {
		v.TokenQuota = 0
	}
	if v.CreditQuota < 0 {
		v.CreditQuota = 0
	}
	if v.RateLimit < 0 {
		v.RateLimit = 0
	}
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

// ErrNoKey Resolve 未命中（前缀查无候选，或哈希不匹配）。调用方按 401 处理。
var ErrNoKey = errors.New("keystore: 密钥不存在")

// ErrNotFound Update/Delete/ResetUsage/IPList 的 id 不存在。调用方按 404 处理。
var ErrNotFound = errors.New("keystore: id 不存在")

// CreateParams Create 的入参。零值语义见 Key 注释；归一化在 Create 内完成
// （Realm 脏值→不限制、白名单去空白、CIDR 当场校验、负值钳 0）。
type CreateParams struct {
	Name           string
	ExpiresAt      int64    // unix 秒；0=永不过期
	MaxIPs         int      // 0=不限
	IPWhitelist    []string // CIDR/单 IP；空=不限
	ModelWhitelist []string // 模型名；空=不限
	Realm          string   // ""/cn/global；其它值归一为 ""
	TokenQuota     int64    // 0=不限
	CreditQuota    float64  // 0=不限
	RateLimit      int      // 每 60s 窗口放行成功次数；0=不限
}

// Store 多密钥库。单 RWMutex 保护 byID/byPrefix 及 Key 的运行态字段；
// 读路径返回 clone 快照，Validate 对快照无锁工作（并发模型见包注释）。
type Store struct {
	path string // 落盘路径；"" = 纯内存（不落盘）
	mu   sync.RWMutex
	// byID 权威条目（指针，写路径原地改）；byPrefix prefix → 候选集（同前缀极
	// 罕见——随机明文前 12 字符碰撞概率可忽略——仍按参考实现保留多候选结构）。
	byID     map[string]*Key
	byPrefix map[string][]*Key

	// 落盘防抖：变更置 dirty，flushTimer 若已在窗口内挂起则合并（不滚动推迟，
	// 持续高频写时最坏每 flushEvery 一写，不会无限挂起丢盘）。flushMu 串行化
	// 磁盘 IO（tmp 文件唯一写者），磁盘 IO 在 s.mu 之外。
	now        func() time.Time // 可注入时钟（测试）；生产为 time.Now
	flushEvery time.Duration    // 防抖窗口，默认 500ms（测试可调短）
	flushTimer *time.Timer
	dirty      bool
	flushMu    sync.Mutex
}

// flushDebounce 变更后合并落盘的默认窗口。
const flushDebounce = 500 * time.Millisecond

// writeFile 落盘执行体（包级变量仅为测试注入写计数；生产恒为 writeFileSync）。
var writeFile = writeFileSync

// Open 打开密钥库：path 形如 data/api_keys.json，不存在则创建空库文件（[]）——
// 启动即物化目录与文件，能把「数据目录不可写」这类环境问题挡在启动期（若等首次
// 变更才落盘，静默失败会让管理员以为存上了，重启后密钥全丢）。path 为空 = 纯内存。
// 已有文件损坏（非法 JSON）时**报错而不是清空启动**：静默清空会让首次落盘把好
// 文件覆盖掉，比启动失败危险得多。
func Open(path string) (*Store, error) {
	s := &Store{
		path:       path,
		byID:       map[string]*Key{},
		byPrefix:   map[string][]*Key{},
		now:        time.Now,
		flushEvery: flushDebounce,
	}
	if path == "" {
		return s, nil
	}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := s.loadRaw(raw); err != nil {
			return nil, fmt.Errorf("keystore: 解析 %s 失败: %w", path, err)
		}
	case os.IsNotExist(err):
		// 空库启动：立即物化空文件（见函数注释）。
		if err := writeFile(path, []byte("[]\n")); err != nil {
			return nil, fmt.Errorf("keystore: 创建 %s 失败: %w", path, err)
		}
	default:
		return nil, fmt.Errorf("keystore: 读取 %s 失败: %w", path, err)
	}
	return s, nil
}

// loadRaw 解析落盘 JSON 重建内存状态。文件级损坏由 Open 报错；条目级损坏
// （缺 id/hash 的单条）跳过并打日志——宁可少一把密钥也不拖垮整个库（与参考实现
// _json_list 的容错方向一致，方向相反的场景见 Open 注释：文件级损坏必须报错）。
func (s *Store) loadRaw(raw []byte) error {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil // 空文件 = 空库（手工清空/截断的容错形态）
	}
	var entries []keyFile
	if err := json.Unmarshal(raw, &entries); err != nil {
		return err
	}
	byID := make(map[string]*Key, len(entries))
	byPrefix := map[string][]*Key{}
	for i := range entries {
		e := entries[i]
		k := e.Key
		if k.ID == "" || k.Hash == "" || k.Prefix == "" {
			log.Printf("WARN: [keystore] 跳过损坏的密钥条目（缺 id/hash/prefix）idx=%d", i)
			continue
		}
		k.ips = e.IPs
		// 同把密钥重复 id：后到者胜并告警（手工编辑文件的防御，不静默吞）。
		if _, dup := byID[k.ID]; dup {
			log.Printf("WARN: [keystore] 重复的密钥 id=%s，保留后者", k.ID)
		}
		byID[k.ID] = &k
		byPrefix[k.Prefix] = append(byPrefix[k.Prefix], &k)
	}
	s.byID = byID
	s.byPrefix = byPrefix
	return nil
}

// keygen 生成新密钥的明文/哈希/prefix/id。随机源失败直接报错（降级为可预测密钥
// 不可接受）。
func keygen() (plaintext, hash, prefix, id string, err error) {
	var raw [32]byte
	if _, err = rand.Read(raw[:]); err != nil {
		return "", "", "", "", fmt.Errorf("keystore: 生成随机密钥失败: %w", err)
	}
	plaintext = KeyPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	sum := sha256.Sum256([]byte(plaintext))
	hash = hex.EncodeToString(sum[:])
	prefix = plaintext[:12]
	var idraw [8]byte
	if _, err = rand.Read(idraw[:]); err != nil {
		return "", "", "", "", fmt.Errorf("keystore: 生成密钥 id 失败: %w", err)
	}
	id = hex.EncodeToString(idraw[:])
	return plaintext, hash, prefix, id, nil
}

// Create 生成新密钥。明文**仅此一次**返回（库中只存哈希与 prefix）；返回的 k 是
// 值拷贝快照（运行态由库内部管理）。
func (s *Store) Create(p CreateParams) (plaintext string, k Key, err error) {
	plaintext, hash, prefix, id, err := keygen()
	if err != nil {
		return "", Key{}, err
	}
	if err := validateCIDRs(p.IPWhitelist); err != nil {
		return "", Key{}, err
	}
	// 负值参数在入字段前钳 0（clampKey 语义见定义）。
	if p.ExpiresAt < 0 {
		p.ExpiresAt = 0
	}
	if p.MaxIPs < 0 {
		p.MaxIPs = 0
	}
	if p.TokenQuota < 0 {
		p.TokenQuota = 0
	}
	if p.CreditQuota < 0 {
		p.CreditQuota = 0
	}
	if p.RateLimit < 0 {
		p.RateLimit = 0
	}
	k = Key{
		ID:             id,
		Name:           p.Name,
		Enabled:        true,
		Prefix:         prefix,
		Hash:           hash,
		CreatedAt:      s.now().Unix(),
		ExpiresAt:      p.ExpiresAt,
		MaxIPs:         p.MaxIPs,
		IPWhitelist:    normList(p.IPWhitelist),
		ModelWhitelist: normList(p.ModelWhitelist),
		Realm:          normRealm(p.Realm),
		TokenQuota:     p.TokenQuota,
		CreditQuota:    p.CreditQuota,
		RateLimit:      p.RateLimit,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := k.clone()
	s.byID[stored.ID] = stored
	s.byPrefix[stored.Prefix] = append(s.byPrefix[stored.Prefix], stored)
	s.scheduleFlushLocked()
	return plaintext, k, nil
}

// List 全部密钥（深拷贝快照，不含明文——明文本就不在库中；调用方修改副本不影响
// 库内状态）。顺序不保证（map 遍历），面板展示按需排序。
func (s *Store) List() []Key {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Key, 0, len(s.byID))
	for _, k := range s.byID {
		out = append(out, *k.clone())
	}
	return out
}

// Update 按 id 修改密钥。mut 在 s.mu 写临界区内执行，**不得再调用 Store 的任何
// 方法**（重入死锁）；mut 拿到的是权威条目的深拷贝视图，只能改导出字段（运行态
// 字段不导出，外部包改不到）。身份字段（ID/Hash/Prefix）不允许经 Update 变更：
// 哈希是凭据本体、prefix 由明文派生，换密钥 = Delete+Create。找不到返回
// ErrNotFound；CIDR 白名单非法时报错且**不落任何变更**。
func (s *Store) Update(id string, mut func(*Key)) error {
	if mut == nil {
		return errors.New("keystore: Update mut 为 nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.byID[id]
	if !ok {
		return ErrNotFound
	}
	view := stored.clone()
	mut(view)
	if err := validateCIDRs(view.IPWhitelist); err != nil {
		return err
	}
	// 写侧归一化收口（与 Create 同一路径）：mut 里直接塞的切片也要过同一把筛。
	clampKey(view)
	view.IPWhitelist = normList(view.IPWhitelist)
	view.ModelWhitelist = normList(view.ModelWhitelist)
	view.Realm = normRealm(view.Realm)
	view.ID, view.Hash, view.Prefix = stored.ID, stored.Hash, stored.Prefix
	view.rateWindowStart, view.rateCount, view.ips = stored.rateWindowStart, stored.rateCount, stored.ips
	*stored = *view
	s.scheduleFlushLocked()
	return nil
}

// Delete 按 id 删除密钥及其来源 IP 记忆。找不到返回 ErrNotFound。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byID[id]
	if !ok {
		return ErrNotFound
	}
	delete(s.byID, id)
	list := s.byPrefix[k.Prefix]
	for i, cand := range list {
		if cand == k {
			s.byPrefix[k.Prefix] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(s.byPrefix[k.Prefix]) == 0 {
		delete(s.byPrefix, k.Prefix)
	}
	s.scheduleFlushLocked()
	return nil
}

// Resolve 按 Bearer 明文定位密钥：prefix map 索引出候选，再对每条候选做 SHA-256
// 摘要 + subtle 常量时间比较（不全表扫）。未命中返回 ErrNoKey。
//
// 返回**深拷贝快照**：调用方可安全持有、传入 Validate/RecordUse；RecordUse 按
// 其 ID 重锚到库内权威条目记账。拿到快照到使用之间密钥可能被停用/删除——窗口
// 内的过期判定是既有语义（参考实现读库同样有此窗口），最多多放行一个在途请求。
func (s *Store) Resolve(bearer string) (*Key, error) {
	// 形态预检只做廉价裁剪：不以 wbk_ 开头或不足 12 字符不可能有候选，直接
	// ErrNoKey；哈希比较本身常量时间，预检不泄露额外信息（前缀在明文里可见，
	// 本就不是秘密）。
	if len(bearer) < 12 || !strings.HasPrefix(bearer, KeyPrefix) {
		return nil, ErrNoKey
	}
	prefix := bearer[:12]
	sum := sha256.Sum256([]byte(bearer))
	digest := hex.EncodeToString(sum[:])
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.byPrefix[prefix] {
		// 常量时间比较（评审点）：候选间前缀相同但哈希不同，统一走 subtle——
		// 比较耗时与命中与否无关，不因提前返回泄露「对了几位」。哈希命中唯一
		//（同前缀多候选是合法形态，明文不同则哈希不同）。
		if subtle.ConstantTimeCompare([]byte(k.Hash), []byte(digest)) == 1 {
			return k.clone(), nil
		}
	}
	return nil, ErrNoKey
}

// Validate 校验请求是否放行。nil = 放行；非 nil 时按其 Status 回客户端、Reason
// 细化文案。本方法**纯函数、无锁**（对 Resolve 返回的快照工作），限流窗口与 IP
// 记忆的写入都在放行成功后的 RecordUse——被拒请求不占窗口额度（issue #52，见
// 包注释）。
//
// 参数约定：ip 客户端来源 IP（空 = 无来源，白名单开启时拒绝——fail-closed）；
// model **模型映射之后**的模型名（realm 与白名单都判它，理由见包注释）；realm
// 调用方经 server.ResolveModel 得到的实际路由版本（"" 时按 model 前缀推断兜底）；
// now 注入时刻（生产传 time.Now()，测试注入）。
//
// 判定顺序即拒绝分类学的分流顺序：先凭据（403）→ 配额（429）→ 请求参数（400）。
// 并发语义：多个请求同刻通过同一窗口的最后名额是可能的（check-then-act 间隙），
// 限流与 MaxIPs 都是软上限，与参考实现读库判定的 TOCTOU 同级，不做记账侧二次
// 严查（限流器不是计费器，过冲 1-2 个无害，为此加锁不值）。
func (s *Store) Validate(k *Key, ip, model, realm string, now time.Time) *Rejection {
	if k == nil {
		return &Rejection{Status: 403, Reason: ReasonKeyDisabled}
	}
	// --- 凭据本身不可用 → 403 ---
	if !k.Enabled {
		return &Rejection{Status: 403, Reason: ReasonKeyDisabled}
	}
	if k.IsExpired(now) {
		return &Rejection{Status: 403, Reason: ReasonKeyExpired}
	}
	// --- 配额用尽 → 429（token 与积分各自独立，任一超限即拦）---
	if k.TokenQuota > 0 && k.UsedTokens >= k.TokenQuota {
		return &Rejection{Status: 429, Reason: ReasonTokenQuotaExhausted}
	}
	if k.CreditQuota > 0 && k.UsedCredits >= k.CreditQuota {
		return &Rejection{Status: 429, Reason: ReasonCreditQuotaExhausted}
	}
	// --- 限流窗口 → 429（只查不写；记账在 RecordUse）---
	if k.RateLimit > 0 {
		win := int64(RateWindow / time.Second)
		if k.rateWindowStart == now.Unix()/win*win && k.rateCount >= k.RateLimit {
			return &Rejection{Status: 429, Reason: ReasonRateLimited}
		}
	}
	// --- IP 白名单 → 400（「来源不对」是这次请求的问题，报文要能透出——403
	// 会被一批客户端折叠成「密钥无效」，用户就不知道该加白名单了）---
	if len(k.IPWhitelist) > 0 {
		ok := false
		if ip != "" {
			for _, cidr := range k.IPWhitelist {
				if iputil.Matches(ip, cidr) {
					ok = true
					break
				}
			}
		}
		if !ok {
			return &Rejection{Status: 400, Reason: ReasonIPNotAllowed}
		}
	}
	// --- MaxIPs → 403（凭据维度的约束：这把密钥绑满了，新来源不可用）。已知 IP
	// 不占新名额；判据是 Validate 时点的快照（放行后 RecordUse 才落 IP）。---
	if k.MaxIPs > 0 {
		if _, known := k.ips[ip]; !known && len(k.ips) >= k.MaxIPs {
			return &Rejection{Status: 403, Reason: ReasonMaxIPsExceeded}
		}
	}
	// --- 版本归属 → 400。判**映射之后**的名字；realm 参数（ResolveModel 结果）
	// 优先，缺省按 model 前缀推断（裸名=cn，与 resolveModel 同口径）。---
	if want := k.Realm; want != "" {
		got := normRealm(realm)
		if got == "" {
			got = realmOf(model)
		}
		if got != want {
			return &Rejection{Status: 400, Reason: ReasonRealmMismatch}
		}
	}
	// --- 模型白名单 → 400。**不能因 model 缺失就跳过检查**（参考实现的实测坑：
	// `if models and model and ...` 让限定单模型的密钥可用「不带 model」走上游
	// 默认模型，白名单形同虚设）。比对两侧都过 bareModel：请求名与白名单条目的
	// `cn:` 前缀写法等价；`global:` 保留（决定路由域，两个版本同名模型不是一回事）。
	if len(k.ModelWhitelist) > 0 {
		m := strings.TrimSpace(model)
		if m == "" {
			return &Rejection{Status: 400, Reason: ReasonModelNotAllowed}
		}
		for _, allow := range k.ModelWhitelist {
			if bareModel(allow) == bareModel(m) {
				return nil
			}
		}
		return &Rejection{Status: 400, Reason: ReasonModelNotAllowed}
	}
	return nil
}

// RecordUse 放行的请求成功后调用：计入限流窗口、累计用量、记来源 IP、刷
// LastUsedAt。tokens/credits 是本次请求**上游真实计量**的值（usage token 数与
// 真实扣费积分），调用方负责从上游响应提取；credits 为 0 表示上游未返回扣费
// 字段——不计入而不是按 0 记（与参考实现 touch 的口径一致）；负值按异常输入
// 吃掉。k 是 Resolve/List 返回的快照：按其 ID 重锚到库内权威条目记账，密钥已
// 删除则静默丢弃（无处可记，窗口同样不推进）。
func (s *Store) RecordUse(k *Key, ip string, tokens int64, credits float64) {
	if k == nil {
		return
	}
	s.mu.Lock()
	stored, ok := s.byID[k.ID]
	if !ok {
		s.mu.Unlock()
		return
	}
	// 限流窗口：跨窗重开，同窗累加。只有放行成功的请求走到这里（被拒请求连
	// RecordUse 都不该调——契约见 Validate 注释）。
	if stored.RateLimit > 0 {
		win := int64(RateWindow / time.Second)
		cur := s.now().Unix() / win * win
		if stored.rateWindowStart != cur {
			stored.rateWindowStart = cur
			stored.rateCount = 0
		}
		stored.rateCount++
	}
	if tokens > 0 {
		stored.UsedTokens += tokens
	}
	if credits > 0 {
		stored.UsedCredits += credits
	}
	now := s.now().Unix()
	if ip != "" {
		if stored.ips == nil {
			stored.ips = map[string]int64{}
		}
		if _, known := stored.ips[ip]; !known {
			stored.ips[ip] = now
		}
	}
	stored.LastUsedAt = now
	s.mu.Unlock()
	s.scheduleFlush()
}

// Rejections 返回该密钥**与具体请求无关**的首个拒绝（nil = 当前时刻凭据、配额、
// 限流全部通过），面板「这把 key 为什么被拒」展示用。IP 白名单、realm、模型
// 白名单依赖具体请求的 ip/model，不在本判定内（它们在 Validate 的请求路径上报）。
// 与 Validate 共用同一判据（单一份事实，不另写面板特化逻辑）。
func (s *Store) Rejections(k *Key) *Rejection {
	if k == nil {
		return nil
	}
	now := s.now()
	if !k.Enabled {
		return &Rejection{Status: 403, Reason: ReasonKeyDisabled}
	}
	if k.IsExpired(now) {
		return &Rejection{Status: 403, Reason: ReasonKeyExpired}
	}
	if k.TokenQuota > 0 && k.UsedTokens >= k.TokenQuota {
		return &Rejection{Status: 429, Reason: ReasonTokenQuotaExhausted}
	}
	if k.CreditQuota > 0 && k.UsedCredits >= k.CreditQuota {
		return &Rejection{Status: 429, Reason: ReasonCreditQuotaExhausted}
	}
	if k.RateLimit > 0 {
		win := int64(RateWindow / time.Second)
		if k.rateWindowStart == now.Unix()/win*win && k.rateCount >= k.RateLimit {
			return &Rejection{Status: 429, Reason: ReasonRateLimited}
		}
	}
	return nil
}

// IPList 返回该密钥的来源 IP → first_seen 快照（面板展示用）。k 为 nil 或已删除
// 返回空表。
func (s *Store) IPList(k *Key) map[string]int64 {
	out := map[string]int64{}
	if k == nil {
		return out
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	stored, ok := s.byID[k.ID]
	if !ok {
		return out
	}
	for ip, ts := range stored.ips {
		out[ip] = ts
	}
	return out
}

// ResetUsage 把已用 Token 与已用积分一起归零（配额重置）。两个量一起清是刻意的：
// 界面上它们是同一个「重置用量」按钮，只清 token 不清积分会留下一个看不见的
// 残留额度，下次超额时用户会莫名其妙（参考实现 reset_usage 同语义）。若只想放开
// 其中一项，正确做法是把对应的**额度**调大，而不是靠重置。找不到返回 ErrNotFound。
// 不清限流窗口与 IP 记忆（重置的是额度，不是限流状态）。
func (s *Store) ResetUsage(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byID[id]
	if !ok {
		return ErrNotFound
	}
	k.UsedTokens = 0
	k.UsedCredits = 0
	s.scheduleFlushLocked()
	return nil
}

// ---------------------------------------------------------------------------
// 防抖落盘（tmp+rename 原子写；与 pool/persist.go 的 writeStateFileSync 同构）
// ---------------------------------------------------------------------------

// scheduleFlushLocked 变更后挂防抖定时器：窗口内多次变更合并为一次磁盘 IO
// （高频记账路径不每请求打盘）。已在窗口内挂起则直接合并（**不**滚动推迟——
// 滚动推迟遇上持续写入会让落盘无限顺延，进程退出时丢掉全部增量）。调用方必须
// 已持 s.mu（写锁）。
func (s *Store) scheduleFlushLocked() {
	if s.path == "" {
		return
	}
	s.dirty = true
	if s.flushTimer == nil {
		s.flushTimer = time.AfterFunc(s.flushEvery, s.flush)
	}
}

// scheduleFlush 锁外便捷入口（RecordUse 收尾用）。
func (s *Store) scheduleFlush() {
	s.mu.Lock()
	s.scheduleFlushLocked()
	s.mu.Unlock()
}

// markDirty 落盘失败后的重试挂载：置回 dirty 并武装下一轮防抖定时器（快照全量
// 语义，重复写安全）。失败即存在未持久化的变更，无条件置 dirty——不能查 dirty
// 再决定（flush 在写盘前已把它清掉，查到 false 会漏挂，丢掉重试信号）。
func (s *Store) markDirty() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return
	}
	s.dirty = true
	if s.flushTimer == nil {
		s.flushTimer = time.AfterFunc(s.flushEvery, s.flush)
	}
}

// flush 落盘当前状态（防抖定时器回调）。s.mu 内只做 dirty 判定 + 深拷贝快照 +
// 序列化（纯内存），磁盘 IO 在 s.mu 之外、flushMu 串行化（tmp 唯一写者）。
func (s *Store) flush() {
	s.mu.Lock()
	s.flushTimer = nil
	if !s.dirty || s.path == "" {
		s.mu.Unlock()
		return
	}
	s.dirty = false
	entries := make([]keyFile, 0, len(s.byID))
	for _, k := range s.byID {
		c := k.clone()
		entries = append(entries, keyFile{Key: *c, IPs: c.ips})
	}
	raw, err := json.MarshalIndent(entries, "", "  ")
	s.mu.Unlock()
	if err != nil {
		log.Printf("ERR: [keystore] 密钥库序列化失败: %v", err)
		s.markDirty()
		return
	}
	s.flushMu.Lock()
	err = writeFile(s.path, raw)
	s.flushMu.Unlock()
	if err != nil {
		log.Printf("WARN: [keystore] 密钥库落盘失败 path=%s err=%v", s.path, err)
		s.markDirty()
	}
}

// Flush 同步落盘（幂等：无变更不写）。进程退出前调用；返回时最近一次变更已
// 落盘（或落盘失败已打日志并保留 dirty——同步场景下失败直接透传给调用方判断）。
func (s *Store) Flush() error {
	s.mu.Lock()
	if t := s.flushTimer; t != nil {
		t.Stop()
		s.flushTimer = nil
	}
	if !s.dirty || s.path == "" {
		s.mu.Unlock()
		return nil
	}
	s.dirty = false
	entries := make([]keyFile, 0, len(s.byID))
	for _, k := range s.byID {
		c := k.clone()
		entries = append(entries, keyFile{Key: *c, IPs: c.ips})
	}
	raw, err := json.MarshalIndent(entries, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("keystore: 序列化失败: %w", err)
	}
	s.flushMu.Lock()
	err = writeFile(s.path, raw)
	s.flushMu.Unlock()
	if err != nil {
		return fmt.Errorf("keystore: 落盘 %s 失败: %w", s.path, err)
	}
	return nil
}

// Close 语义别名：停止防抖并做最后一次落盘。与 pool.Pool.Close 对齐，进程优雅
// 停机路径调用。幂等。
func (s *Store) Close() error { return s.Flush() }

// writeFileSync 原子写：MkdirAll → tmp(0600) → Write → Sync（防掉电产生半截
// 文件；Windows 上落到 FlushFileBuffers 同样有效）→ rename。目录 fsync 在
// Windows 无对应 API，best-effort 跳过（与 pool.writeStateFileSync 同构）。
func writeFileSync(path string, raw []byte) error {
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
