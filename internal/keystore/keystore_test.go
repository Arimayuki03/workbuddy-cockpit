// keystore 单测：往返、拒绝分类学、限流窗口、落盘、并发。
// 覆盖清单见各 Test 函数注释；验收命令：
//
//	go test ./internal/keystore/ -count=1 -race
package keystore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestStore 纯内存库（不落盘；落盘行为有专门测试）。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open("")
	if err != nil {
		t.Fatalf("Open(\"\") err=%v", err)
	}
	return s
}

// mustCreate Create 并返回明文与快照。
func mustCreate(t *testing.T, s *Store, p CreateParams) (string, *Key) {
	t.Helper()
	plain, k, err := s.Create(p)
	if err != nil {
		t.Fatalf("Create(%+v) err=%v", p, err)
	}
	return plain, &k
}

// reload 按 id 从库重读最新快照（RecordUse/Update 后的权威状态；List 顺序不稳定，
// 按 id 取而不用下标）。
func reload(t *testing.T, s *Store, id string) *Key {
	t.Helper()
	for _, k := range s.List() {
		if k.ID == id {
			return &k
		}
	}
	t.Fatalf("reload: id=%s 不存在", id)
	return nil
}

// fixedClock 恒定时刻时钟。
func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

// ---------------------------------------------------------------------------
// Create → Resolve 往返；明文只返回一次、库中无明文、prefix 索引、错 key 拒绝
// ---------------------------------------------------------------------------

func TestCreateResolveRoundTrip(t *testing.T) {
	s := newTestStore(t)
	plain, k := mustCreate(t, s, CreateParams{Name: "甲的钥匙"})

	if !strings.HasPrefix(plain, "wbk_") {
		t.Fatalf("明文前缀异常: %q", plain[:min(6, len(plain))])
	}
	// wbk_ + 32 字节 RawURL base64（43 字符）= 47 字符。
	if len(plain) != len(KeyPrefix)+43 {
		t.Fatalf("明文长度=%d want %d", len(plain), len(KeyPrefix)+43)
	}
	// prefix = 明文前 12 字符，索引命中。
	if k.Prefix != plain[:12] {
		t.Fatalf("prefix=%q want %q", k.Prefix, plain[:12])
	}
	// 哈希落库且正确。
	sum := sha256.Sum256([]byte(plain))
	if k.Hash != hex.EncodeToString(sum[:]) {
		t.Fatal("库中哈希与明文 SHA-256 不一致")
	}
	if k.ID == "" || !k.Enabled || k.CreatedAt == 0 {
		t.Fatalf("默认字段异常: id=%q enabled=%v created=%d", k.ID, k.Enabled, k.CreatedAt)
	}

	got, err := s.Resolve(plain)
	if err != nil {
		t.Fatalf("Resolve err=%v", err)
	}
	if got.ID != k.ID {
		t.Fatalf("Resolve 命中 id=%s want %s", got.ID, k.ID)
	}
	// 错 key（同前缀形态但不同明文）拒绝。
	if _, err := s.Resolve("wbk_" + strings.Repeat("A", 43)); err == nil {
		t.Fatal("错 key 未拒绝")
	}
	// 前缀正确但尾部错一个字符：哈希必不同（覆盖候选内未命中路径）。
	bad := []byte(plain)
	bad[len(bad)-1] ^= 1
	if _, err := s.Resolve(string(bad)); err == nil {
		t.Fatal("尾字符篡改的 key 未拒绝")
	}
	// 非法形态：裸前缀 / 空串 / 完全无关。
	for _, tok := range []string{"", "wbk_", "sk-xxx", "Bearer abc"} {
		if _, err := s.Resolve(tok); err == nil {
			t.Fatalf("非法明文 %q 未拒绝", tok)
		}
	}
	// List 快照的 JSON 表面无明文、无运行态字段。
	for _, item := range s.List() {
		raw := string(mustJSON(t, keyFile{Key: item}))
		if strings.Contains(raw, plain) {
			t.Fatal("List 快照 JSON 中出现明文")
		}
		if strings.Contains(raw, "rate_window") || strings.Contains(raw, "\"ips\"") {
			t.Fatal("List 快照 JSON 泄漏运行态字段")
		}
	}
}

func TestResolveMultipleCandidates(t *testing.T) {
	// 多把密钥共存时 prefix 索引逐一比对哈希，各自命中。
	s := newTestStore(t)
	plain1, k1 := mustCreate(t, s, CreateParams{})
	plain2, k2 := mustCreate(t, s, CreateParams{})
	if plain1[:12] == plain2[:12] {
		t.Skip("随机前缀碰撞（概率 ~2^-64），跳过本用例")
	}
	got1, err := s.Resolve(plain1)
	if err != nil || got1.ID != k1.ID {
		t.Fatalf("Resolve#1 err=%v", err)
	}
	got2, err := s.Resolve(plain2)
	if err != nil || got2.ID != k2.ID {
		t.Fatalf("Resolve#2 err=%v", err)
	}
}

// ---------------------------------------------------------------------------
// 拒绝分类学逐条（状态码 + 短码）
// ---------------------------------------------------------------------------

func TestRejectKeyDisabled(t *testing.T) {
	s := newTestStore(t)
	_, k := mustCreate(t, s, CreateParams{})
	if rej := s.Validate(k, "", "m", "", time.Now()); rej != nil {
		t.Fatalf("新建密钥被拒: %+v", rej)
	}
	if err := s.Update(k.ID, func(v *Key) { v.Enabled = false }); err != nil {
		t.Fatal(err)
	}
	fresh := reload(t, s, k.ID)
	rej := s.Validate(fresh, "", "", "", time.Now())
	if rej == nil || rej.Status != 403 || rej.Reason != ReasonKeyDisabled {
		t.Fatalf("停用拒绝=%+v want 403/key_disabled", rej)
	}
}

func TestRejectKeyExpired(t *testing.T) {
	s := newTestStore(t)
	now := time.Unix(1_800_000_000, 0)
	s.now = fixedClock(now)
	_, k := mustCreate(t, s, CreateParams{ExpiresAt: now.Unix() - 10})
	rej := s.Validate(k, "", "", "", now)
	if rej == nil || rej.Status != 403 || rej.Reason != ReasonKeyExpired {
		t.Fatalf("过期拒绝=%+v want 403/key_expired", rej)
	}
	// 边界：ExpiresAt 恰等于 now → 已过期（闭区间口径）。
	_, k2 := mustCreate(t, s, CreateParams{ExpiresAt: now.Unix()})
	if rej := s.Validate(k2, "", "", "", now); rej == nil || rej.Reason != ReasonKeyExpired {
		t.Fatalf("到期秒拒绝=%+v", rej)
	}
	// 下一秒仍未过期。
	_, k3 := mustCreate(t, s, CreateParams{ExpiresAt: now.Unix() + 1})
	if rej := s.Validate(k3, "", "", "", now); rej != nil {
		t.Fatalf("未到期被拒: %+v", rej)
	}
	// ExpiresAt=0 永不过期。
	_, k4 := mustCreate(t, s, CreateParams{})
	if rej := s.Validate(k4, "", "", "", now.AddDate(10, 0, 0)); rej != nil {
		t.Fatalf("永不过期被拒: %+v", rej)
	}
}

func TestRejectRealmMismatch(t *testing.T) {
	s := newTestStore(t)
	_, kcn := mustCreate(t, s, CreateParams{Realm: "cn"})
	_, kgl := mustCreate(t, s, CreateParams{Realm: "global"})
	_, kany := mustCreate(t, s, CreateParams{Realm: "  CN "}) // 归一化：大小写/空白 → cn
	if kany.Realm != "cn" {
		t.Fatalf("realm 归一=%q want cn", kany.Realm)
	}
	// 脏值归一为不限制。
	_, kdirty := mustCreate(t, s, CreateParams{Realm: "mars"})
	if kdirty.Realm != "" {
		t.Fatalf("脏 realm 归一=%q want 空", kdirty.Realm)
	}

	now := time.Now()
	rej := s.Validate(kgl, "", "cn:glm-5.2", "cn", now)
	if rej == nil || rej.Status != 400 || rej.Reason != ReasonRealmMismatch {
		t.Fatalf("global key 调 cn 模型=%+v want 400/realm_mismatch", rej)
	}
	rej = s.Validate(kcn, "", "global:glm-5.2", "global", now)
	if rej == nil || rej.Status != 400 || rej.Reason != ReasonRealmMismatch {
		t.Fatalf("cn key 调 global 模型=%+v want 400/realm_mismatch", rej)
	}
	// 同域放行。
	if rej := s.Validate(kcn, "", "cn:glm-5.2", "cn", now); rej != nil {
		t.Fatalf("cn key 调 cn 模型被拒: %+v", rej)
	}
	if rej := s.Validate(kgl, "", "global:glm-5.2", "global", now); rej != nil {
		t.Fatalf("global key 调 global 模型被拒: %+v", rej)
	}
	// 不限制 realm：两侧都放行。
	if rej := s.Validate(kdirty, "", "global:glm-5.2", "global", now); rej != nil {
		t.Fatalf("不限 realm 被拒: %+v", rej)
	}
	// realm 参数缺省（""）时按 model 前缀推断：裸名 = cn。
	rej = s.Validate(kgl, "", "glm-5.2", "", now)
	if rej == nil || rej.Reason != ReasonRealmMismatch {
		t.Fatalf("realm 缺省推断=%+v want realm_mismatch", rej)
	}
}

func TestRejectModelWhitelist(t *testing.T) {
	s := newTestStore(t)
	_, k := mustCreate(t, s, CreateParams{ModelWhitelist: []string{"cn:glm-5.2", "kimi-k3"}})
	now := time.Now()

	// 请求名裸名、白名单条目带 cn: 前缀 → 放行（bareModel 归一，双向等价）。
	if rej := s.Validate(k, "", "glm-5.2", "cn", now); rej != nil {
		t.Fatalf("裸名请求被拒: %+v", rej)
	}
	// 白名单里另一条裸名命中。
	if rej := s.Validate(k, "", "kimi-k3", "cn", now); rej != nil {
		t.Fatalf("第二条白名单被拒: %+v", rej)
	}
	// global: 前缀同剥归一（两侧裸名比对——server 侧调用传的就是剥完前缀的裸名，
	// 白名单写 global: 形态与裸名等价，与 cn: 同理）。白名单外的其它模型 → 拒。
	rej := s.Validate(k, "", "gpt-9", "global", now)
	if rej == nil || rej.Status != 400 || rej.Reason != ReasonModelNotAllowed {
		t.Fatalf("白名单外模型(global realm)=%+v want 400/model_not_allowed", rej)
	}
	// H7 回归：白名单照 /v1/models 下发 id 写 global: 前缀形态，请求剥前缀后的
	// 裸名必须放行（此前 bareModel 保留 global: 导致恒 400）。
	_, kg := mustCreate(t, s, CreateParams{ModelWhitelist: []string{"global:gpt-5.4"}})
	if rej := s.Validate(kg, "", "gpt-5.4", "global", now); rej != nil {
		t.Fatalf("global: 白名单条目对裸名请求被拒: %+v", rej)
	}
	// 完全不在白名单（cn realm 同口径）。
	rej = s.Validate(k, "", "gpt-9", "cn", now)
	if rej == nil || rej.Status != 400 || rej.Reason != ReasonModelNotAllowed {
		t.Fatalf("白名单外模型=%+v want 400/model_not_allowed", rej)
	}
	// 白名单启用但请求不带 model：**拒绝**（不能因缺失跳过检查——参考实现实测坑）。
	rej = s.Validate(k, "", "", "cn", now)
	if rej == nil || rej.Status != 400 || rej.Reason != ReasonModelNotAllowed {
		t.Fatalf("缺 model=%+v want 400/model_not_allowed", rej)
	}
	// 纯空白 model 同拒。
	if rej := s.Validate(k, "", "   ", "cn", now); rej == nil || rej.Reason != ReasonModelNotAllowed {
		t.Fatalf("空白 model=%+v", rej)
	}
	// 白名单为空 = 不限：任何模型放行（含 global: 形态）。
	_, kfree := mustCreate(t, s, CreateParams{})
	if rej := s.Validate(kfree, "", "global:anything", "global", now); rej != nil {
		t.Fatalf("空白名单被拒: %+v", rej)
	}
}

func TestRejectIPWhitelist(t *testing.T) {
	s := newTestStore(t)
	_, k := mustCreate(t, s, CreateParams{IPWhitelist: []string{" 10.0.0.0/8 ", "203.0.113.7"}})
	// 写侧已去空白（_norm_cidrs 的坑）。
	if k.IPWhitelist[0] != "10.0.0.0/8" {
		t.Fatalf("白名单归一=%q", k.IPWhitelist[0])
	}
	now := time.Now()
	if rej := s.Validate(k, "10.1.2.3", "", "", now); rej != nil {
		t.Fatalf("网段内 IP 被拒: %+v", rej)
	}
	if rej := s.Validate(k, "203.0.113.7", "", "", now); rej != nil {
		t.Fatalf("单 IP 命中被拒: %+v", rej)
	}
	rej := s.Validate(k, "192.0.2.1", "", "", now)
	if rej == nil || rej.Status != 400 || rej.Reason != ReasonIPNotAllowed {
		t.Fatalf("白名单外 IP=%+v want 400/ip_not_allowed", rej)
	}
	// 空 IP（无来源）fail-closed。
	rej = s.Validate(k, "", "", "", now)
	if rej == nil || rej.Reason != ReasonIPNotAllowed {
		t.Fatalf("空 IP=%+v want ip_not_allowed", rej)
	}
	// 非法 CIDR 在 Create fail-fast。
	if _, _, err := s.Create(CreateParams{IPWhitelist: []string{"999.1.1.1"}}); err == nil {
		t.Fatal("非法 CIDR 未被写侧校验拦截")
	}
	// 绕过写侧的非法规则（手工构造）匹配侧仍 fail-closed：永不匹配。
	kbad := &Key{Enabled: true, IPWhitelist: []string{"999.1.1.1"}}
	if rej := s.Validate(kbad, "999.1.1.1", "", "", now); rej == nil || rej.Reason != ReasonIPNotAllowed {
		t.Fatalf("非法规则未 fail-closed: %+v", rej)
	}
	// IPv6 网段。
	_, k6 := mustCreate(t, s, CreateParams{IPWhitelist: []string{"2001:db8::/32"}})
	if rej := s.Validate(k6, "2001:db8::1", "", "", now); rej != nil {
		t.Fatalf("IPv6 命中被拒: %+v", rej)
	}
	if rej := s.Validate(k6, "2001:db9::1", "", "", now); rej == nil {
		t.Fatal("IPv6 网段外未拒")
	}
}

func TestRejectMaxIPs(t *testing.T) {
	s := newTestStore(t)
	_, k := mustCreate(t, s, CreateParams{MaxIPs: 2})
	now := time.Now()
	// 无记忆时放行；成功后 IP 落入记忆。
	if rej := s.Validate(k, "10.0.0.1", "", "", now); rej != nil {
		t.Fatalf("首个 IP 被拒: %+v", rej)
	}
	s.RecordUse(k, "10.0.0.1", 0, 0)
	// 已知 IP 不占新名额。
	fresh := reload(t, s, k.ID)
	if rej := s.Validate(fresh, "10.0.0.1", "", "", now); rej != nil {
		t.Fatalf("已知 IP 被拒: %+v", rej)
	}
	// 第二个新 IP 放行并落记。
	if rej := s.Validate(fresh, "10.0.0.2", "", "", now); rej != nil {
		t.Fatalf("第二个 IP 被拒: %+v", rej)
	}
	s.RecordUse(fresh, "10.0.0.2", 0, 0)
	fresh = reload(t, s, k.ID)
	// 第三个新 IP：403 max_ips_exceeded。
	rej := s.Validate(fresh, "10.0.0.3", "", "", now)
	if rej == nil || rej.Status != 403 || rej.Reason != ReasonMaxIPsExceeded {
		t.Fatalf("超上限 IP=%+v want 403/max_ips_exceeded", rej)
	}
	// 已知 IP 仍放行。
	if rej := s.Validate(fresh, "10.0.0.1", "", "", now); rej != nil {
		t.Fatalf("已知 IP 在满员后被拒: %+v", rej)
	}
	// MaxIPs=0 不限。
	_, kfree := mustCreate(t, s, CreateParams{})
	for i := 0; i < 5; i++ {
		if rej := s.Validate(kfree, "10.0.1.5", "", "", now); rej != nil && rej.Reason == ReasonMaxIPsExceeded {
			t.Fatalf("不限 IP 数被拒: %+v", rej)
		}
	}
}

func TestRejectQuotas(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	// token 配额。
	_, k := mustCreate(t, s, CreateParams{TokenQuota: 100})
	if rej := s.Validate(k, "", "", "", now); rej != nil {
		t.Fatalf("未超配额被拒: %+v", rej)
	}
	s.RecordUse(k, "", 60, 0)
	k = reload(t, s, k.ID)
	if rej := s.Validate(k, "", "", "", now); rej != nil {
		t.Fatalf("60/100 被拒: %+v", rej)
	}
	s.RecordUse(k, "", 40, 0) // 累计到 100
	k = reload(t, s, k.ID)
	rej := s.Validate(k, "", "", "", now)
	if rej == nil || rej.Status != 429 || rej.Reason != ReasonTokenQuotaExhausted {
		t.Fatalf("token 配额=%+v want 429/token_quota_exhausted", rej)
	}
	// 顶格判断是 >=：已用 99 不拒。
	_, k99 := mustCreate(t, s, CreateParams{TokenQuota: 100})
	s.RecordUse(k99, "", 99, 0)
	k99 = reload(t, s, k99.ID)
	if rej := s.Validate(k99, "", "", "", now); rej != nil {
		t.Fatalf("已用 99/100 被拒: %+v", rej)
	}

	// credit 配额（独立判定）。
	_, kc := mustCreate(t, s, CreateParams{CreditQuota: 1.5})
	s.RecordUse(kc, "", 0, 1.5)
	kc = reload(t, s, kc.ID)
	rej = s.Validate(kc, "", "", "", now)
	if rej == nil || rej.Status != 429 || rej.Reason != ReasonCreditQuotaExhausted {
		t.Fatalf("credit 配额=%+v want 429/credit_quota_exhausted", rej)
	}
	// 零额度 = 不限。
	_, k0 := mustCreate(t, s, CreateParams{TokenQuota: 0, CreditQuota: 0})
	s.RecordUse(k0, "", 99999, 99999)
	k0 = reload(t, s, k0.ID)
	if rej := s.Validate(k0, "", "", "", now); rej != nil {
		t.Fatalf("零额度被拒: %+v", rej)
	}
	// 优先级：凭据 403 先于配额 429（停用后即便配额满也报停用）。
	_, km := mustCreate(t, s, CreateParams{TokenQuota: 1})
	s.RecordUse(km, "", 1, 0)
	_ = s.Update(km.ID, func(v *Key) { v.Enabled = false })
	km2 := reload(t, s, km.ID)
	rej = s.Validate(km2, "", "", "", now)
	if rej == nil || rej.Reason != ReasonKeyDisabled {
		t.Fatalf("停用+配额满=%+v want key_disabled 优先", rej)
	}
}

// ---------------------------------------------------------------------------
// 限流：窗口计数到顶 429、被拒请求不占额度、窗口翻新
// ---------------------------------------------------------------------------

func TestRateLimitWindow(t *testing.T) {
	s := newTestStore(t)
	base := time.Unix(1_800_000_000, 0) // 整窗口对齐的时刻：稳定控制窗口边界
	s.now = fixedClock(base)
	_, k := mustCreate(t, s, CreateParams{RateLimit: 3})
	now := base

	// 窗口内放行 3 次后到顶。
	for i := 0; i < 3; i++ {
		if rej := s.Validate(k, "", "", "", now); rej != nil {
			t.Fatalf("第 %d 次放行被拒: %+v", i+1, rej)
		}
		s.RecordUse(k, "", 1, 0)
	}
	cur := reload(t, s, k.ID)
	rej := s.Validate(cur, "", "", "", now)
	if rej == nil || rej.Status != 429 || rej.Reason != ReasonRateLimited {
		t.Fatalf("窗口满=%+v want 429/rate_limited", rej)
	}

	// **被拒请求不占额度**（issue #52）：窗口满后再来被拒请求不调 RecordUse
	//（契约）→ 窗口计数不变。
	if cur.rateCount != 3 {
		t.Fatalf("被拒期间计数漂移: %d want 3", cur.rateCount)
	}
	_ = s.Validate(cur, "", "", "", now)
	_ = s.Validate(cur, "", "", "", now)
	cur = reload(t, s, k.ID)
	if cur.rateCount != 3 {
		t.Fatalf("被拒请求推进了窗口: %d want 3", cur.rateCount)
	}

	// 窗口翻新（59s 后仍在旧窗口，61s 后新窗口清零）。
	if rej := s.Validate(cur, "", "", "", base.Add(59*time.Second)); rej == nil {
		t.Fatal("59s 仍在旧窗口却放行")
	}
	later := base.Add(61 * time.Second)
	s.now = fixedClock(later)
	cur = reload(t, s, k.ID)
	if rej := s.Validate(cur, "", "", "", later); rej != nil {
		t.Fatalf("窗口翻新后被拒: %+v", rej)
	}
	// 新窗口重新计满。
	for i := 0; i < 3; i++ {
		if rej := s.Validate(cur, "", "", "", later); rej != nil {
			t.Fatalf("新窗口第 %d 次被拒: %+v", i+1, rej)
		}
		s.RecordUse(cur, "", 1, 0)
	}
	cur = reload(t, s, k.ID)
	if rej := s.Validate(cur, "", "", "", later); rej == nil || rej.Reason != ReasonRateLimited {
		t.Fatalf("新窗口计满未拒: %+v", rej)
	}
	// RateLimit=0 = 无每密钥限流。
	_, kfree := mustCreate(t, s, CreateParams{})
	for i := 0; i < 50; i++ {
		s.RecordUse(kfree, "", 1, 0)
	}
	kfree = reload(t, s, kfree.ID)
	if rej := s.Validate(kfree, "", "", "", base.Add(2*time.Minute)); rej != nil {
		t.Fatalf("无限流被拒: %+v", rej)
	}
}

func TestRateLimitOnlySuccessfulCountIssue52(t *testing.T) {
	// 场景回归：被拒请求（模型白名单 400）不得让限流窗口推进——否则客户端重试
	// 与限流构成正反馈，限流器变熔断永不恢复。
	s := newTestStore(t)
	base := time.Unix(1_800_000_000, 0)
	s.now = fixedClock(base)
	_, k := mustCreate(t, s, CreateParams{RateLimit: 5, ModelWhitelist: []string{"allowed"}})

	// 触发 10 次拒绝（白名单外模型），一律不调 RecordUse。
	for i := 0; i < 10; i++ {
		if rej := s.Validate(k, "", "blocked", "", base); rej == nil {
			t.Fatal("拒绝未触发")
		}
	}
	// 窗口未满：放行请求仍可成功 5 次。
	for i := 0; i < 5; i++ {
		if rej := s.Validate(k, "", "allowed", "", base); rej != nil {
			t.Fatalf("被拒请求挤占了窗口额度（issue #52）: 第 %d 次 %+v", i+1, rej)
		}
		s.RecordUse(k, "", 0, 0)
	}
	cur := reload(t, s, k.ID)
	if rej := s.Validate(cur, "", "allowed", "", base); rej == nil || rej.Reason != ReasonRateLimited {
		t.Fatalf("真实放行 5 次后未限流: %+v", rej)
	}
}

// ---------------------------------------------------------------------------
// RecordUse 记账：用量、IP 表、LastUsedAt、credits=0 不计入
// ---------------------------------------------------------------------------

func TestRecordUseAccounting(t *testing.T) {
	s := newTestStore(t)
	ts := time.Unix(1_777_000_000, 0)
	s.now = fixedClock(ts)
	plain, k := mustCreate(t, s, CreateParams{})
	s.RecordUse(k, "10.0.0.9", 123, 0.05)
	s.RecordUse(k, "10.0.0.9", 77, 0) // credits=0：上游未返回扣费 → 不计入
	got, err := s.Resolve(plain)
	if err != nil {
		t.Fatal(err)
	}
	if got.UsedTokens != 200 {
		t.Fatalf("UsedTokens=%d want 200", got.UsedTokens)
	}
	if got.UsedCredits != 0.05 {
		t.Fatalf("UsedCredits=%v want 0.05", got.UsedCredits)
	}
	if got.LastUsedAt != ts.Unix() {
		t.Fatalf("LastUsedAt=%d want %d", got.LastUsedAt, ts.Unix())
	}
	ips := s.IPList(got)
	if len(ips) != 1 || ips["10.0.0.9"] != ts.Unix() {
		t.Fatalf("IPList=%v", ips)
	}
	// 负值吃掉；已删除密钥静默丢弃。
	s.RecordUse(got, "10.0.0.9", -5, -1)
	got = reload(t, s, got.ID)
	if got.UsedTokens != 200 || got.UsedCredits != 0.05 {
		t.Fatalf("负值未吃掉: %d %v", got.UsedTokens, got.UsedCredits)
	}
	_ = s.Delete(got.ID)
	s.RecordUse(got, "10.0.0.10", 99, 99)
	if len(s.List()) != 0 {
		t.Fatal("已删除密钥不应能记账")
	}
	s.RecordUse(nil, "x", 1, 1) // nil 快照：不 panic
}

func TestResetUsage(t *testing.T) {
	s := newTestStore(t)
	_, k := mustCreate(t, s, CreateParams{TokenQuota: 10, CreditQuota: 10})
	s.RecordUse(k, "10.0.0.1", 10, 10)
	cur := reload(t, s, k.ID)
	if rej := s.Validate(cur, "", "", "", time.Now()); rej == nil {
		t.Fatal("配额未触发")
	}
	if err := s.ResetUsage(k.ID); err != nil {
		t.Fatal(err)
	}
	cur = reload(t, s, k.ID)
	if cur.UsedTokens != 0 || cur.UsedCredits != 0 {
		t.Fatalf("重置后=%d/%v", cur.UsedTokens, cur.UsedCredits)
	}
	// IP 记忆与限流状态不清（重置的是额度，不是限流状态）。
	if ips := s.IPList(cur); len(ips) != 1 {
		t.Fatalf("重置误清 IP 记忆: %v", ips)
	}
	if err := s.ResetUsage("no-such"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知 id=%v want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Update / Delete / List 语义
// ---------------------------------------------------------------------------

func TestUpdateSemantics(t *testing.T) {
	s := newTestStore(t)
	plain, k := mustCreate(t, s, CreateParams{Name: "旧名", Realm: "  CN ", MaxIPs: -3})
	if k.Realm != "cn" || k.MaxIPs != 0 {
		t.Fatalf("Create 归一: realm=%q maxIPs=%d", k.Realm, k.MaxIPs)
	}
	// 改字段 + 归一化收口。
	if err := s.Update(k.ID, func(v *Key) {
		v.Name = "新名"
		v.Realm = "GLOBAL"
		v.IPWhitelist = []string{" 192.0.2.1 ", ""}
		v.ModelWhitelist = []string{"", " glm-5.2 "}
		v.TokenQuota = -100
	}); err != nil {
		t.Fatal(err)
	}
	cur := reload(t, s, k.ID)
	if cur.Name != "新名" || cur.Realm != "global" || cur.TokenQuota != 0 {
		t.Fatalf("Update 归一: name=%q realm=%q quota=%d", cur.Name, cur.Realm, cur.TokenQuota)
	}
	if len(cur.IPWhitelist) != 1 || cur.IPWhitelist[0] != "192.0.2.1" {
		t.Fatalf("Update 白名单归一: %v", cur.IPWhitelist)
	}
	if len(cur.ModelWhitelist) != 1 || cur.ModelWhitelist[0] != "glm-5.2" {
		t.Fatalf("Update 模型归一: %v", cur.ModelWhitelist)
	}
	// 运行态在 Update 后保留。
	s.RecordUse(cur, "10.9.9.9", 5, 0)
	_ = s.Update(k.ID, func(v *Key) { v.Name = "再改" })
	cur = reload(t, s, k.ID)
	if len(s.IPList(cur)) != 1 || cur.UsedTokens != 5 {
		t.Fatalf("Update 丢运行态: ips=%v tokens=%d", s.IPList(cur), cur.UsedTokens)
	}
	// 身份字段篡改被忽略（mut 视图改 ID/Hash/Prefix 无效）。
	_ = s.Update(k.ID, func(v *Key) {
		v.ID = "hacked"
		v.Hash = "hacked"
		v.Prefix = "hacked"
	})
	got, err := s.Resolve(plain)
	if err != nil || got.ID != k.ID {
		t.Fatalf("身份字段被篡改: err=%v", err)
	}
	// 非法 CIDR 在 Update 处报错且不落变更。
	if err := s.Update(k.ID, func(v *Key) { v.IPWhitelist = []string{"300.1.1.1"} }); err == nil {
		t.Fatal("Update 非法 CIDR 未报错")
	}
	if cur = reload(t, s, k.ID); cur.Name != "再改" {
		t.Fatalf("Update 失败后误落变更: name=%q", cur.Name)
	}
	// mut 为 nil / 未知 id。
	if err := s.Update(k.ID, nil); err == nil {
		t.Fatal("nil mut 未报错")
	}
	if err := s.Update("no-such", func(*Key) {}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知 id=%v", err)
	}
	// Delete 后 Resolve 失效。
	plain2, k2 := mustCreate(t, s, CreateParams{})
	if err := s.Delete(k2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(plain2); !errors.Is(err, ErrNoKey) {
		t.Fatalf("删除后 Resolve=%v want ErrNoKey", err)
	}
	if err := s.Delete(k2.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删除=%v", err)
	}
}

func TestRejectionsPanelView(t *testing.T) {
	s := newTestStore(t)
	base := time.Unix(1_800_000_000, 0)
	s.now = fixedClock(base)
	// 与请求无关的拒绝：停用 / 过期 / 配额 / 限流。
	_, k := mustCreate(t, s, CreateParams{TokenQuota: 5})
	s.RecordUse(k, "", 5, 0)
	cur := reload(t, s, k.ID)
	if rej := s.Rejections(cur); rej == nil || rej.Reason != ReasonTokenQuotaExhausted {
		t.Fatalf("面板拒绝视图=%+v", rej)
	}
	_ = s.Update(k.ID, func(v *Key) { v.Enabled = false })
	cur = reload(t, s, k.ID)
	if rej := s.Rejections(cur); rej == nil || rej.Reason != ReasonKeyDisabled {
		t.Fatalf("停用视图=%+v", rej)
	}
	// 干净密钥 → nil；请求相关的白名单不在此判定（面板无请求上下文）。
	_, k2 := mustCreate(t, s, CreateParams{ModelWhitelist: []string{"x"}, IPWhitelist: []string{"10.0.0.0/8"}})
	if rej := s.Rejections(k2); rej != nil {
		t.Fatalf("请求相关白名单不应进面板判定: %+v", rej)
	}
	if rej := s.Rejections(nil); rej != nil {
		t.Fatalf("nil 快照=%+v", rej)
	}
}

// ---------------------------------------------------------------------------
// 落盘：防抖合并、原子写、往返、失败重试
// ---------------------------------------------------------------------------

func TestPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api_keys.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Unix(1_800_000_000, 0)
	s.now = fixedClock(base)
	plain1, k1 := mustCreate(t, s, CreateParams{
		Name: "持久化", Realm: "global", MaxIPs: 3, TokenQuota: 1000, CreditQuota: 9.5, RateLimit: 30,
		IPWhitelist:    []string{"10.0.0.0/8"},
		ModelWhitelist: []string{"glm-5.2"},
	})
	plain2, k2 := mustCreate(t, s, CreateParams{ExpiresAt: base.Unix() + 3600})
	s.RecordUse(k1, "10.0.0.1", 42, 1.25)
	if err := s.Flush(); err != nil {
		t.Fatalf("Flush err=%v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close err=%v", err)
	}

	// 重新 Open 后一致。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重开 err=%v", err)
	}
	list := s2.List()
	if len(list) != 2 {
		t.Fatalf("重载条数=%d want 2", len(list))
	}
	byName := map[string]Key{}
	for _, k := range list {
		byName[k.Name] = k
	}
	got := byName["持久化"]
	if got.ID != k1.ID || got.Hash != k1.Hash || got.Prefix != plain1[:12] ||
		got.Realm != "global" || got.MaxIPs != 3 || got.TokenQuota != 1000 ||
		got.CreditQuota != 9.5 || got.RateLimit != 30 ||
		len(got.IPWhitelist) != 1 || len(got.ModelWhitelist) != 1 {
		t.Fatalf("重载字段不一致: %+v", got)
	}
	if got.UsedTokens != 42 || got.UsedCredits != 1.25 || got.LastUsedAt != base.Unix() {
		t.Fatalf("重载用量不一致: %+v", got)
	}
	if ips := s2.IPList(&got); len(ips) != 1 || ips["10.0.0.1"] != base.Unix() {
		t.Fatalf("重载 IP 记忆不一致: %v", ips)
	}
	// Resolve 往返。
	if got2, err := s2.Resolve(plain1); err != nil || got2.ID != k1.ID {
		t.Fatalf("重载后 Resolve err=%v", err)
	}
	if got2, err := s2.Resolve(plain2); err != nil || got2.ExpiresAt != k2.ExpiresAt {
		t.Fatalf("第二把重载 err=%v", err)
	}
	// 明文不在文件里，哈希在。
	raw := string(readTestFile(t, path))
	if strings.Contains(raw, plain1) || strings.Contains(raw, plain2) {
		t.Fatal("明文泄漏进落盘文件")
	}
	if !strings.Contains(raw, k1.Hash) {
		t.Fatal("落盘文件缺哈希")
	}
}

func TestOpenCreatesEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "api_keys.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("空库启动 err=%v", err)
	}
	raw := strings.TrimSpace(string(readTestFile(t, path))) // 文件已被物化（数据目录不可写挡在启动期）
	if raw != "[]" {
		t.Fatalf("空库文件=%q want []", raw)
	}
	if len(s.List()) != 0 {
		t.Fatal("空库 List 非空")
	}
}

func TestOpenCorruptFileFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	writeTestFile(t, path, "{不是 JSON")
	if _, err := Open(path); err == nil {
		t.Fatal("损坏文件未报错（静默清空会覆盖好文件）")
	}
	// 条目级损坏：跳过坏条目，好条目保留。
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	raw := mustJSON(t, []keyFile{
		{Key: Key{ID: "ok", Hash: "h", Prefix: "wbk_okokok"}},
		{Key: Key{ID: ""}}, // 缺 id：损坏条目
	})
	if err := s.loadRaw(raw); err != nil {
		t.Fatalf("条目级损坏不应整库失败: %v", err)
	}
	if len(s.List()) != 1 {
		t.Fatalf("坏条目未跳过: %d", len(s.List()))
	}
}

func TestFlushDebounceMergesWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api_keys.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.flushEvery = 50 * time.Millisecond
	// writes 由 flush 定时器 goroutine 并发自增（写盘回调在独立 goroutine 执行），
	// 用 atomic 计数，race 下合法。
	var writes atomic.Int32
	orig := writeFile
	writeFile = func(p string, raw []byte) error {
		writes.Add(1)
		return orig(p, raw)
	}
	t.Cleanup(func() { writeFile = orig })

	// 密集变更（防抖窗口内）合并为一次写。
	for i := 0; i < 10; i++ {
		if _, _, err := s.Create(CreateParams{Name: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond) // > 防抖窗口
	if n := writes.Load(); n != 1 {
		t.Fatalf("防抖合并失效: writes=%d want 1", n)
	}
	// 再改一次 → 又恰好一次写。
	if err := s.ResetUsage(s.List()[0].ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if n := writes.Load(); n != 2 {
		t.Fatalf("第二次变更落盘: writes=%d want 2", n)
	}
	// Flush 同步兜底 + 幂等（无变更不写）。
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if n := writes.Load(); n != 2 {
		t.Fatalf("Flush 幂等失效: writes=%d want 2", n)
	}
}

func TestFlushFailureRetries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api_keys.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.flushEvery = 20 * time.Millisecond
	var failNext bool
	orig := writeFile
	writeFile = func(p string, raw []byte) error {
		if failNext {
			failNext = false
			return errors.New("注入写失败")
		}
		return orig(p, raw)
	}
	t.Cleanup(func() { writeFile = orig })

	if _, _, err := s.Create(CreateParams{}); err != nil {
		t.Fatal(err)
	}
	// 首次防抖写失败 → dirty 回挂 → 下一轮防抖自动重试成功。
	waitFor(t, 2*time.Second, func() bool {
		raw := readTestFile(t, path)
		return strings.Contains(string(raw), "hash")
	})
	// Flush 失败透传错误（同步场景调用方可见）。
	failNext = true
	if _, _, err := s.Create(CreateParams{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err == nil {
		t.Fatal("Flush 写失败未透传")
	}
	// 失败后 dirty 已回挂：下次 Flush 仍会重写并成功。
	if err := s.Flush(); err != nil {
		t.Fatalf("失败后重试 Flush 仍错: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 并发安全（-race 下跑）：8 goroutine 混合读写
// ---------------------------------------------------------------------------

func TestConcurrentMixed(t *testing.T) {
	s := newTestStore(t)
	plains := make([]string, 4)
	ids := make([]string, 4)
	for i := range plains {
		p, k := mustCreate(t, s, CreateParams{RateLimit: 100000})
		plains[i], ids[i] = p, k.ID
	}
	base := time.Unix(1_800_000_000, 0)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			i := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				i++
				switch g % 4 {
				case 0: // Resolve（热路径：prefix 索引 + 常量时间比对）
					if _, err := s.Resolve(plains[i%len(plains)]); err != nil {
						t.Errorf("Resolve err=%v", err)
						return
					}
				case 1: // Validate（对 Resolve 快照的纯函数判定）
					k, err := s.Resolve(plains[i%len(plains)])
					if err != nil {
						t.Errorf("Resolve err=%v", err)
						return
					}
					_ = s.Validate(k, "10.0.0.1", "m", "cn", base)
				case 2: // RecordUse + List + IPList（记账与快照读混跑）
					k, err := s.Resolve(plains[i%len(plains)])
					if err != nil {
						t.Errorf("Resolve err=%v", err)
						return
					}
					s.RecordUse(k, "10.0.0.2", 1, 0.5)
					_ = s.List()
					_ = s.IPList(k)
				case 3: // Update / Create / Delete（结构变更与快照替换）
					if i%3 == 0 {
						if err := s.Update("nonexistent", func(*Key) {}); !errors.Is(err, ErrNotFound) {
							t.Errorf("Update 未知 id 返回 %v", err)
							return
						}
					} else {
						_, _, err := s.Create(CreateParams{Name: "并发"})
						if err != nil {
							t.Errorf("Create err=%v", err)
							return
						}
						if l := s.List(); len(l) > len(plains) {
							// 只删自己刚建的（按名识别，原始 4 把不删）。
							for _, k := range l {
								if k.Name == "并发" {
									_ = s.Delete(k.ID)
									break
								}
							}
						}
					}
				}
			}
		}(g)
	}
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	// 原始 4 把完好且可解析（并发 Delete 未误伤）。
	for i, p := range plains {
		got, err := s.Resolve(p)
		if err != nil || got.ID != ids[i] {
			t.Fatalf("并发后密钥 #%d 失效: err=%v", i, err)
		}
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("并发后 Flush err=%v", err)
	}
}

func TestConcurrentRecordUseSameKey(t *testing.T) {
	// 同把密钥高并发记账：用量累计无丢失。
	s := newTestStore(t)
	_, k := mustCreate(t, s, CreateParams{RateLimit: 100000})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				s.RecordUse(k, "10.0.0.1", 10, 0.5)
			}
		}()
	}
	wg.Wait()
	got := s.List()[0]
	if got.UsedTokens != 8*200*10 {
		t.Fatalf("token 累计丢失: %d want %d", got.UsedTokens, 8*200*10)
	}
	if got.UsedCredits != 8*200*0.5 {
		t.Fatalf("credit 累计丢失: %v want %v", got.UsedCredits, 8*200*0.5)
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func waitFor(t *testing.T, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("waitFor 超时")
}
