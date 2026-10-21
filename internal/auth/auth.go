// Package auth 解析 WorkBuddy auth 文件（嵌套形/扁平形双形态），
// 提供 refresh 后的原子写回。
package auth

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/logfmt"
)

// Auth 是归一化后的账号凭证（来源可以是插件 OAuth 嵌套形或手写扁平形）。
type Auth struct {
	// mu 串行化 RefreshToken 写与 SaveAtomic 读，防止并发写回半更新 token。
	mu sync.Mutex

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	// realm 账号域（"cn" / "global"），落盘于 auth.realm（嵌套形）或顶层 realm（扁平形）。
	// 空 = 缺省：Realm() 按 domain 后缀回落，最终恒非空。
	//
	// 命名注记：Go 不允许字段与方法同名，持久化字段用未导出 realm，计算访问器用
	// 导出的 Realm()（跨包调用全部走方法）。Parse/SaveAtomic/login 在包内读写字段。
	realm        string
	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string // 来源文件；refresh 后原子写回此处

	// DeviceToken 设备风控 Token（X-Device-Token 头），来源 auth 文件的 device_token 键。
	// 缺省为空 = 不注入该头（容器内无桌面端 Turing SDK 的常见部署）。
	// 手写扁平形 auth 文件可直接写 "device_token": "..."；插件 OAuth 嵌套形
	// 顶层 device_token 也会被解析（与桌面端共用状态文件的部署方式）。
	DeviceToken string

	// ProxyRoute 出站代理线路名（workbuddy-manager proxy_routes 吸收件）。
	// **只存线路名，绝不存代理 URL/凭据**：线路表（名 → 代理 URL）存共享配置
	// config.json 的 proxy_routes 段，账号文件只引用名字——代理凭据不在 auths/ 里
	// 扩散，导出/分享凭证文件不会连带泄密。空串 = 直连（现状行为，零配置零回归）；
	// 非空但线路表查无此名 → upstream 侧拒绝发出请求（绝不静默回退直连：回落
	// = 出口隔离失效且无人发现）。落盘于嵌套形 auth 段内 + 扁平形顶层 proxy_route。
	ProxyRoute string

	// Weight 账号级手写权重（issue #187），来源 auth 文件顶层 `weight` 字段，
	// 由面板的「权重」写入。0 = 未设置（等价 1.0）。
	//
	// 语义是**乘数**而非优先级：选号本来就按「余额 + 快过期占比 + 闲置时长」
	// 三因子加权随机（见 pool.weightOf），这里把结果再乘上该值 —— 权重 3 表示
	// 同等条件下被抽中的机会约为默认的 3 倍。之所以不做成「权重高的恒定优先」：
	// 那会让低权重的号一直闲置，积分用不完就过期（正是 #187 另一半诉求要避免的）。
	// 硬门槛（冷却 / 熔断 / 禁用 / 在途占满 / 成本分层）与 credits_desc 策略不受
	// 本字段影响。运行期写入必须经 SetWeightAndSave（同一临界区落盘）或
	// SetWeight（纯内存测试）；与 RefreshToken/SaveAtomic 共用 a.mu。
	Weight float64
}

// 手写权重的取值区间：低于 MinWeight 等于「几乎不用」（但仍留一丝机会，
// 与 pickWeighted 的权重保底 ≥1 同口径），高于 MaxWeight 没有实际意义。
const (
	MinWeight = 0.1
	MaxWeight = 100.0
)

// PickWeight 返回用于选号的权重乘数：未设置（0）或非法值给 1，合法值钳制在
// [MinWeight, MaxWeight]。选号侧只读本方法、不直读字段 —— 钳制口径只有一处，
// 否则「文件里写了 1e9」这种输入会把选号直接压成一号独大。
//
// 加锁读取：面板权重端点会在运行期改写 Weight，而 weightOf/Status 在选号与
// 状态查询路径并发读取（与 AccessTokenValue 同款数据竞争防线）。
func (a *Auth) PickWeight() float64 {
	if a == nil {
		return 1
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return pickWeightLocked(a.Weight)
}

// pickWeightLocked 无锁版 PickWeight：调用方必须已持 a.mu（sync.Mutex 不可重入）。
func pickWeightLocked(weight float64) float64 {
	w := weight
	if math.IsNaN(w) || math.IsInf(w, 0) || w <= 0 {
		return 1
	}
	if w < MinWeight {
		return MinWeight
	}
	if w > MaxWeight {
		return MaxWeight
	}
	return w
}

// WeightValue 加锁读取原始持久化值（0 = 未设置；不做钳制，供面板回显「用户填了
// 什么」与保存失败回滚使用）。选择/展示权重一律用 PickWeight。
func (a *Auth) WeightValue() float64 {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Weight
}

// SetWeight 加锁写入原始权重值。负数/NaN/Inf 按「未设置」处理（与解析期
// sanitizeWeight 同口径）；合法越界值保留原样，选号时由 PickWeight 钳制。
func (a *Auth) SetWeight(weight float64) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.Weight = sanitizeWeight(weight)
	a.mu.Unlock()
}

// SetWeightAndSave 在同一把 a.mu 内完成「设置权重 + 整份原子落盘」，返回实际
// 持久化的原始值（0 = 未设置）。面板端点必须走本方法，而不是 SetWeight +
// SaveAtomic 两步：两步之间另一个并发保存可能已写入新权重，本请求落盘失败
// 后回滚则会把这个成功值覆盖回旧值（lost update）。
//
// FilePath 为空 = 纯内存装配（测试/最小形态）：只改内存并返回新值，不落盘。
// 落盘失败时把内存权重回滚到调用前值，文件保持旧内容（writeFileAtomic 的 tmp
// 写入失败不会 rename，故不会出现半份文件）。
func (a *Auth) SetWeightAndSave(weight float64) (float64, error) {
	if a == nil {
		return 0, fmt.Errorf("nil auth")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	prev := a.Weight
	a.Weight = sanitizeWeight(weight)
	if a.FilePath == "" {
		return a.Weight, nil
	}
	if err := a.saveAtomicLocked(); err != nil {
		a.Weight = prev
		return prev, err
	}
	return a.Weight, nil
}

// sanitizeWeight 解析期清洗：非有限值与负数一律当「未设置」（0）。合法但越界的
// 值**保留原样**，由 PickWeight 在选号时钳制 —— 面板靠原值回显「用户填了什么」，
// 这里改写会让界面显示与文件内容不一致。手改坏的值打一行 WARN，否则「权重没生效」
// 会变成没有线索的悬案。
func sanitizeWeight(weight float64) float64 {
	if math.IsNaN(weight) || math.IsInf(weight, 0) || weight < 0 {
		log.Printf("WARN: [auth] 忽略非法的 weight=%v（按未设置处理，应为 %v..%v）",
			weight, MinWeight, MaxWeight)
		return 0
	}
	return weight
}

// parseWeightRaw 解析 auth 文件里的 weight 原始值：缺省/类型错误/超大数字都
// 不阻断整份凭证加载，统一按「未设置」(0) 处理并打 WARN。权重只是选号调参，
// 不能因为一个手改坏的字段让整个账号被 LoadDir 跳过（那会让账号静默消失）。
// 合法但越界的数值仍原样保留，钳制交给 PickWeight。
func parseWeightRaw(raw json.RawMessage) float64 {
	if len(raw) == 0 {
		return 0
	}
	var weight float64
	if err := json.Unmarshal(raw, &weight); err != nil {
		log.Printf("WARN: [auth] 忽略无法解析的 weight=%q（按未设置处理，应为 %v..%v）",
			strings.TrimSpace(string(raw)), MinWeight, MaxWeight)
		return 0
	}
	return sanitizeWeight(weight)
}

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// AccessTokenValue 加锁读取 AccessToken（出站请求头一律经此取值，勿直读字段）。
//
// 为什么必须加锁：RefreshToken 在 a.mu 内改写 AccessToken/RefreshToken/Domain/ExpiresAt
// （client.go「第 2 段（锁内）：校验快照一致后写回」），而所有出站请求头构造
// （ChatHeaders / BillingHeaders / fetchEnterpriseModels / fetchV3Models /
// global_models）与调度器的 token 检查都在锁外直读这些字段。生产上两侧真会并发：
// Scheduler.RunKeepaliveNow 定时对**每个**非禁用账号刷新（与是否有在途请求无关），
// 而 handler 正基于**同一个** *auth.Auth 指针构造请求头（Pool.AuthByUID/List 返回的
// 就是池内同一个对象）。无同步直读构成数据竞争，go test -race 实证：
//
//	WARNING: DATA RACE
//	Write at ... by goroutine:
//	  (*Client).RefreshToken()  internal/upstream/client.go:929
//	Previous read at ... by goroutine:
//	  (*Client).ChatHeaders()   internal/upstream/headers.go:224
//
// （回归测试 upstream.TestChatHeadersRacesRefreshToken）。
func (a *Auth) AccessTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.AccessToken
}

// DomainValue 加锁读取 Domain（同 AccessTokenValue：RefreshToken 在锁内改写它）。
func (a *Auth) DomainValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Domain
}

// RefreshTokenValue 加锁读取 RefreshToken（同 AccessTokenValue：RefreshToken 在锁内
// 改写它）。调度器的「有无凭证」前置守卫（checkin/keepalive/travel 的
// `a.RefreshToken == ""`）必须经此取值，勿直读字段。
//
// 与 #125 修的 AccessToken/Domain 属同一类：守卫是纯读、刷新是纯写，二者无同步即
// 构成数据竞争（RefreshToken 写回在 client.go「第 2 段（锁内）」的 `if tok.RefreshToken
// != ""` 分支）。go test -race 实证（回归测试 scheduler.TestKeepaliveGuardRacesRefreshToken）：
//
//	WARNING: DATA RACE
//	Read at ... by goroutine:
//	  (*Scheduler).RunKeepaliveNow()  internal/scheduler/scheduler.go:654
//	Previous write at ... by goroutine:
//	  (*Client).RefreshToken()        internal/upstream/client.go:931
func (a *Auth) RefreshTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.RefreshToken
}

// SetNickname 加锁写 Nickname（余额刷新的昵称同步在锁内改写它，见 balance_refresh.go）。
func (a *Auth) SetNickname(nick string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.Nickname = nick
	a.mu.Unlock()
}

// NicknameValue 加锁读取 Nickname（同 AccessTokenValue：昵称同步在锁内改写它）。
//
// 池的 Status/List（statusOf 直读 e.a.Nickname）与面板任务中心 goroutine 都在锁外
// 读该字段，与余额刷新的写并发即数据竞争——同族字段 AccessToken/Domain/RefreshToken
// 均有加锁访问器，Nickname 补齐同款口径。
func (a *Auth) NicknameValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Nickname
}

// globalEnabled 全局开关：global realm 是否路由（D5 双保险）。
// 默认开启（与 config global.enabled 缺省 true 一致）：Realm() 正常按显式 realm/
// domain 判定 global/cn。显式 SetGlobalEnabled(false)（config "enabled": false）关闭
// → 逃生门：纯 CN 部署，即便 auth 文件写了 realm=global 或 domain 为 .workbuddy.ai
// 也恒判 cn——「关了才锁死」的单一闸口集中收敛在 Realm()/IsGlobal() 里。
var globalEnabled atomic.Bool

func init() { globalEnabled.Store(true) }

// SetGlobalEnabled 注入 global realm 路由开关（false = 锁死纯 CN，逃生门）。
func SetGlobalEnabled(enabled bool) { globalEnabled.Store(enabled) }

// Realm 返回账号的归一化域：显式 Realm=="global" 或 domain 后缀 .workbuddy.ai → "global"，
// 否则 "cn"。显式 global 优先于 domain 回落（D1）。
// 全局开关 SetGlobalEnabled(false) 时恒 "cn"（逃生门：纯 CN 锁定，不影响默认行为）。
// 空 realm + 空 domain → "cn"（老 CN 凭证零回归）。
func (a *Auth) Realm() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realmLocked()
}

// realmLocked Realm 的无锁内部实现：仅限**已持 a.mu** 的调用方使用（sync.Mutex 不可重入，
// 锁内再调 Realm() 会自锁）。realm 由 BackfillRealm 改写、Domain 由 RefreshToken 在锁内
// 改写，故读取必须与写方同锁（理由见 AccessTokenValue 注释）。
func (a *Auth) realmLocked() string {
	if !globalEnabled.Load() {
		return "cn"
	}
	if strings.TrimSpace(a.realm) == "global" || isGlobalDomain(a.Domain) {
		return "global"
	}
	return "cn"
}

// ResolveRealm 归一化 realm（cn/global）：显式非空优先，否则按原始 domain 推断
// （isGlobalDomain）。不受逃生门影响（逃生门是路由锁，不应影响标识判定）；
// domain 也为空 → "cn"（老 CN 凭证零回归）。
func ResolveRealm(explicit, domain string) string {
	if r := strings.TrimSpace(explicit); r != "" {
		return r
	}
	if isGlobalDomain(domain) {
		return "global"
	}
	return "cn"
}

// BackfillRealm 为缺省 realm 标识的账号持久化补标识：a.realm 为空时按「原始 domain 推断」
// 写回（cn/global），返回 (是否有变更, 归一化后的 realm)。已有标识不动（幂等）。
//
// 注意用 isGlobalDomain(a.Domain) 直接推断，而非 Realm()——Realm() 在逃生门
// （SetGlobalEnabled(false)）下恒降级 cn，把 global 账号写死成 cn 会永久污染凭证
// （逃生门是纯 CN 部署的临时锁，不应改写落盘数据）。domain 也为空时写 "cn"（老 CN 凭证）。
func (a *Auth) BackfillRealm() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.realm) != "" {
		return false, a.realm
	}
	r := ResolveRealm("", a.Domain)
	a.realm = r
	return true, r
}

// RealmStored 直读持久化的 realm 标识（可能为空 = 未 backfill 的旧文件，Realm() 会 fallback）。
func (a *Auth) RealmStored() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realm
}

// IsGlobal 报告账号是否属于 global realm（= Realm() == "global"）。
func (a *Auth) IsGlobal() bool { return a.Realm() == "global" }

// ProxyRouteValue 加锁读取 ProxyRoute（出站代理线路名，upstream 每次出站按此
// 查线路表选 transport）。与 AccessTokenValue 同款：ProxyRoute 由面板绑定端点
// （pool 内同一 *Auth 对象）在锁内改写，出站路径并发读取必须同锁，否则构成
// 数据竞争（go test -race 实证模式，见 AccessTokenValue 注释）。
func (a *Auth) ProxyRouteValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ProxyRoute
}

// SetProxyRoute 加锁写入 ProxyRoute（面板绑定端点用；空串 = 解绑直连）。
func (a *Auth) SetProxyRoute(route string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ProxyRoute = strings.TrimSpace(route)
}

// isGlobalDomain 判定 domain 是否指向 www.workbuddy.ai 家族。
// 同时接受裸域 workbuddy.ai 与任意子域（HasSuffix("www.workbuddy.ai") 或裸域本身）。
func isGlobalDomain(d string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	return d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai")
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （插件 OAuth 输出）
//	扁平形 {"accessToken":...,"uid":...}   （手写/旧版）
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var a Auth
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Domain       string `json:"domain"`
				Realm        string `json:"realm"`
				// ProxyRoute 出站代理线路名（嵌套形放 auth 段内，与 SaveAtomic 写回同形）。
				ProxyRoute string `json:"proxy_route"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
			// DeviceToken 顶层 device_token（嵌套形与扁平形共用）。
			// 放在 auth 段之外，手写时无需嵌进 auth 对象，降低配置门槛。
			DeviceToken string `json:"device_token"`
			// ProxyRouteTop 顶层 proxy_route 兼容形态：与 auth 段内等价（面板绑定
			// 端点写 auth 段内，手工维护的文件可直接写顶层，两形态都能读）。
			ProxyRouteTop string `json:"proxy_route"`
			// Weight 顶层 weight（手写权重，面板写入；见 Auth.Weight）。
			// 用 RawMessage 容忍手改坏的类型：解析失败按未设置处理，不让整份
			// 凭证文件被 LoadDir 跳过。
			Weight json.RawMessage `json:"weight"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		proxyRoute := n.Auth.ProxyRoute
		if strings.TrimSpace(proxyRoute) == "" {
			proxyRoute = n.ProxyRouteTop // auth 段内缺失时回落顶层兼容键
		}
		a = Auth{
			AccessToken:  n.Auth.AccessToken,
			RefreshToken: n.Auth.RefreshToken,
			ExpiresAt:    n.Auth.ExpiresAt,
			Domain:       n.Auth.Domain,
			realm:        n.Auth.Realm,
			UID:          n.Account.UID,
			EnterpriseID: n.Account.EnterpriseID,
			Nickname:     n.Account.Nickname,
			DeviceToken:  n.DeviceToken,
			ProxyRoute:   strings.TrimSpace(proxyRoute),
			Weight:       parseWeightRaw(n.Weight),
		}
	} else {
		var f struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			Realm        string `json:"realm"`
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
			DeviceToken  string `json:"device_token"`
			ProxyRoute   string `json:"proxy_route"`
			// Weight 顶层 weight（与 proxy_route 同层级，面板写入；见 Auth.Weight）。
			// RawMessage 理由见嵌套形同名注释。
			Weight json.RawMessage `json:"weight"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  f.AccessToken,
			RefreshToken: f.RefreshToken,
			ExpiresAt:    f.ExpiresAt,
			Domain:       f.Domain,
			realm:        f.Realm,
			UID:          f.UID,
			EnterpriseID: f.EnterpriseID,
			Nickname:     f.Nickname,
			DeviceToken:  f.DeviceToken,
			ProxyRoute:   strings.TrimSpace(f.ProxyRoute),
			Weight:       parseWeightRaw(f.Weight),
		}
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return &a, nil
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename），保持嵌套形（插件可读）格式。
// 全程持 a.mu：防止与 RefreshToken 修改 token 字段并发，杜绝写回半更新。
// 防御：accessToken 为空时拒绝写回，避免误用空凭证覆盖有效文件。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveAtomicLocked()
}

// saveAtomicLocked 是 SaveAtomic 的无锁实现：调用方必须已持 a.mu。
// 供 SetWeightAndSave 在同一临界区内完成「改字段 + 整份落盘」，避免两次
// 加锁之间被另一个面板请求插入——否则一个请求落盘失败回滚时，会把另一个
// 已成功写入的权重覆盖回旧值。
func (a *Auth) saveAtomicLocked() error {
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s)", a.UID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"realm":        a.realm,
			// ProxyRoute 非空才写（omitempty 语义手工实现）：旧文件保持原形，不引入空键。
			// 只写线路名——代理 URL/凭据只在 config.json 的 proxy_routes 段，绝不入 auth 文件。
			"proxy_route": a.ProxyRoute,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			// 直读字段：本函数全程持 a.mu（防止半更新快照），NicknameValue 会自锁。
			"nickname": a.Nickname,
		},
	}
	// DeviceToken 非空才写回顶层 device_token：避免在无该字段的旧文件里引入空键
	// （保持与插件 OAuth 输出形状一致，插件读取忽略未知键）。
	if a.DeviceToken != "" {
		doc["device_token"] = a.DeviceToken
	}
	// Weight 同理只在非零时写回（issue #187）：刷新 token 会整份重建文件，
	// 不保留等于把用户设的权重静默清零。0 = 未设置，不写键（面板把 1 当默认，
	// 手改过的文件也不会因此多出 weight: 1 这种噪音）。
	if a.Weight != 0 {
		doc["weight"] = a.Weight
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(a.FilePath, raw)
}

// writeFileAtomic 原子写文件，口径与 pool.writeStateFileSync 一致：
// OpenFile(tmp) → Write → f.Sync()（防掉电后 rename 了半截内容，Windows 上
// Sync 落到 FlushFileBuffers 同样有效）→ Close → Rename；任一步失败逐级
// Remove(tmp)，不留残片（WriteFile 失败会让 .tmp 残留，污染目录）。
func writeFileAtomic(fp string, raw []byte) error {
	tmp := fp + ".tmp"
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
	if err := os.Rename(tmp, fp); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ExportDoc 返回与 SaveAtomic 落盘同形的嵌套凭据文档（供账号导出）。
// 全程持 a.mu：与 RefreshToken 锁内写回互斥，导出的是一份完整的 token 快照，
// 不会夹带半更新的 accessToken/refreshToken。返回值为全新结构，调用方可自由改写。
func (a *Auth) ExportDoc() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"realm":        a.realm,
			// ProxyRoute 与 SaveAtomic 同形：只透出线路名，不含代理凭据（导出文件
			// 可安全流转；线路 URL 本体在 config.json proxy_routes，导入方自行配置）。
			"proxy_route": a.ProxyRoute,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			// 直读字段：本函数全程持 a.mu（完整 token 快照语义），NicknameValue 会自锁。
			"nickname": a.Nickname,
		},
	}
	if a.DeviceToken != "" {
		doc["device_token"] = a.DeviceToken
	}
	// 权重属于账号配置而非秘密，随导出文件往返（导入方 Parse 直接读回）。
	// 只在非零时写出，未设置的文件不会凭空多出 weight: 1。
	if a.Weight != 0 {
		doc["weight"] = a.Weight
	}
	return doc
}

// AuthFileGlob auth 文件的统一 glob 模式（宽侧：workbuddy*.json）。
// 网关 LoadDir 与 cmd 运维工具（signin/credit/trial）共用此单一来源——
// 此前 cmd 侧私用 workbuddy-*.json 窄模式，不带连字符的文件（如
// workbuddy_new.json）被网关加载却被运维工具跳过，排障口径对不上
// （审查发现 10）。
const AuthFileGlob = "workbuddy*.json"

// LoadAuthFiles 返回 dir 下按 AuthFileGlob 匹配的 auth 文件清单（已排序）。
// 供 cmd 运维工具复用：只列文件、不解析不迁移（LoadDir 才做 backfill 等副作用），
// 保持 signin/credit/trial 原有的「逐文件 Parse、损坏即跳过/报行错」流程不变。
func LoadAuthFiles(dir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, AuthFileGlob))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// LoadDir 扫描并解析 dir 下 workbuddy*.json；解析失败的文件静默跳过（启动日志由调用方统计）。
// 顺带做 realm 标识存量迁移：对空 realm 的 auth 自动 backfill（原始 domain 推断）并 SaveAtomic
// 落盘，一次性把旧文件补上 realm 键。单个文件写失败不阻断启动（log WARN 继续），
// 避免历史 auth 目录个别文件不可写时整个服务起不来。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := LoadAuthFiles(dir)
	if err != nil {
		return nil, err
	}
	// seenUID 重复 UID 检测：同 UID 出现在多个文件时（双 realm 同名 UID 概率近零）
	// 打 WARN 告警含两文件路径，由「后载入者胜出」保持现状行为（不改变加载结果）。
	seenUID := make(map[string]string, len(files))
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		// 缺 uid 的凭证文件跳过（Parse 保持向后兼容不拒绝）：uid 是池内的账号主键，
		// 空 UID 的文件都进来会被合并成单个 UID="" 账号（多份凭证互相覆盖）。
		if strings.TrimSpace(a.UID) == "" {
			log.Printf("WARN: 跳过缺 uid 的凭证文件 %s（请补 account.uid 或删除该文件）", f)
			continue
		}
		a.FilePath = f
		if prev, ok := seenUID[a.UID]; ok {
			log.Printf("WARN: uid %s duplicated across %s and %s — 后者覆盖（不同 realm 同名 UID？）",
				logfmt.Label(a.UID, a.NicknameValue()), prev, f)
		}
		seenUID[a.UID] = f
		if a.RealmStored() == "" {
			if changed, r := a.BackfillRealm(); changed {
				if err := a.SaveAtomic(); err != nil {
					log.Printf("WARN: auth %s realm backfill save: %v", logfmt.Label(a.UID, a.NicknameValue()), err)
				} else if r == "global" {
					log.Printf("auth %s 存量迁移: 补 realm=global（domain=%s）", logfmt.Label(a.UID, a.NicknameValue()), a.Domain)
				}
			}
		}
		out = append(out, a)
	}
	return out, nil
}
