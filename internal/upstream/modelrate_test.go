// modelrate_test.go 模型积分倍率缓存表（modelrate.go）的契约锚定：
//   - NormalizeModelRate 各形态原文解析（含非法值/免费/未知的三分语义）；
//   - ModelRateTable 并发读写（-race）、upsert 合并、未命中、realm 隔离；
//   - StoreModelRates 填充点（FetchModels CN / global 探测）端到端入表；
//   - WarmModelRates 预热的跳过/单域失败降级/双域成功。
package upstream

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestNormalizeModelRate 各形态原文 → 数值倍率。可解析形态对齐上游实测
// （"x0.05 credits" 主形态 / "0.50x" 后缀记号 / 裸数值），ok=false 覆盖
// 空串、残形态、负数与 NaN/Inf（strconv 会成功解析这仨，须显式拒绝）。
func TestNormalizeModelRate(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		// 实测主形态。
		{"x0.05 credits", 0.05, true},
		{"x0.05", 0.05, true},
		{"0.50x", 0.50, true},
		{"1.62", 1.62, true},
		{"x1", 1, true},
		{"x0.00 credits", 0, true}, // 明确免费：合法 0，与「未知」严格区分
		{" 1.62 ", 1.62, true},     // 容忍首尾空白
		{"x 0.05", 0.05, true},     // 记号与数值间空白（宽容）
		// 非法：一律 ok=false（= 未知放行，不入表）。
		{"", 0, false},
		{"   ", 0, false},
		{"credits", 0, false},       // 只有单位词
		{"x", 0, false},             // 只有记号
		{"xabc credits", 0, false},  // 非数值
		{"x-0.5", 0, false},         // 负数
		{"-1.5", 0, false},          // 负数（裸）
		{"NaN", 0, false},           // strconv 可解析，须拒绝
		{"xNaN credits", 0, false},  // 记号形态 NaN
		{"Inf", 0, false},           // 同上
		{"xInf credits", 0, false},  // 记号形态 Inf
		{"0.50xx", 0, false},        // 双记号残形态：剥后缀 x 剩 "0.50x" 不可解析
		{"xx0.5", 0, false},         // 双前缀记号残形态
	}
	for _, c := range cases {
		got, ok := NormalizeModelRate(c.in)
		if ok != c.ok {
			t.Errorf("NormalizeModelRate(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok && math.Abs(got-c.want) > 1e-9 {
			t.Errorf("NormalizeModelRate(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestModelRateTableBasics upsert 合并、未命中、Size 与 realm 归一（空 → cn）。
func TestModelRateTableBasics(t *testing.T) {
	tb := NewModelRateTable()
	if got := tb.Size(); got != 0 {
		t.Fatalf("空表 Size() = %d, want 0", got)
	}

	// 批量入表。
	tb.Update("global", map[string]float64{"kimi-k3-1": 1.62, "free-x": 0})
	if v, ok := tb.Lookup("global", "kimi-k3-1"); !ok || v != 1.62 {
		t.Errorf("Lookup(global, kimi-k3-1) = (%v, %v), want (1.62, true)", v, ok)
	}
	if v, ok := tb.Lookup("global", "free-x"); !ok || v != 0 {
		t.Errorf("Lookup(global, free-x) = (%v, %v), want (0, true)（明确免费 ≠ 未知）", v, ok)
	}
	if got := tb.Size(); got != 2 {
		t.Errorf("Size() = %d, want 2", got)
	}

	// upsert：同键覆盖，未提及键保留（合并而非换桶——CN cli 面部分视图不丢既有条目）。
	tb.Update("global", map[string]float64{"kimi-k3-1": 2.0})
	if v, _ := tb.Lookup("global", "kimi-k3-1"); v != 2.0 {
		t.Errorf("upsert 后 kimi-k3-1 = %v, want 2.0", v)
	}
	if _, ok := tb.Lookup("global", "free-x"); !ok {
		t.Error("upsert 未提及的 free-x 应保留")
	}

	// 未命中：模型不存在 / 域不存在。
	if _, ok := tb.Lookup("global", "unknown-model"); ok {
		t.Error("未覆盖模型应未命中")
	}
	if _, ok := tb.Lookup("cn", "kimi-k3-1"); ok {
		t.Error("cn 域无条目应未命中（realm 隔离，见下）")
	}

	// 空 rates 零操作（防呆：空探测不清既有桶）。
	tb.Update("global", nil)
	if got := tb.Size(); got != 2 {
		t.Errorf("空 Update 后 Size() = %d, want 2", got)
	}

	// 空 realm → 按 cn（realmKey 老 CN 语义，与 resolveModel 裸名落 cn 一致）。
	tb.Update("", map[string]float64{"bare-model": 0.05})
	if v, ok := tb.Lookup("cn", "bare-model"); !ok || v != 0.05 {
		t.Errorf("Lookup(cn, bare-model) = (%v, %v), want (0.05, true)（空 realm 归一 cn）", v, ok)
	}
	if v, ok := tb.Lookup("", "bare-model"); !ok || v != 0.05 {
		t.Errorf("Lookup(\"\", bare-model) = (%v, %v), want (0.05, true)", v, ok)
	}

	// 零值表可用（惰性建桶）。
	var zero ModelRateTable
	zero.Update("cn", map[string]float64{"m": 1})
	if v, ok := zero.Lookup("cn", "m"); !ok || v != 1 {
		t.Errorf("零值表 Lookup = (%v, %v), want (1, true)", v, ok)
	}
}

// TestModelRateTableRealmIsolation 同名模型两域倍率互不污染（C-2 隔离：
// CN 探测结果不得作用到 global 同模型名）。
func TestModelRateTableRealmIsolation(t *testing.T) {
	tb := NewModelRateTable()
	tb.Update("cn", map[string]float64{"glm-5.3": 0.05})
	tb.Update("global", map[string]float64{"glm-5.3": 1.62})

	if v, _ := tb.Lookup("cn", "glm-5.3"); v != 0.05 {
		t.Errorf("cn glm-5.3 = %v, want 0.05", v)
	}
	if v, _ := tb.Lookup("global", "glm-5.3"); v != 1.62 {
		t.Errorf("global glm-5.3 = %v, want 1.62（不得被 cn 同名条目污染）", v)
	}
	if got := tb.Size(); got != 2 {
		t.Errorf("Size() = %d, want 2", got)
	}
}

// TestModelRateTableConcurrent 并发 Update/Lookup/Size 无数据竞争（-race 门禁）。
func TestModelRateTableConcurrent(t *testing.T) {
	tb := NewModelRateTable()
	const workers = 8
	const rounds = 200
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			realm := "cn"
			if w%2 == 1 {
				realm = "global"
			}
			for i := 0; i < rounds; i++ {
				tb.Update(realm, map[string]float64{
					"model-a": float64(i),
					"model-b": 0.05,
				})
				_, _ = tb.Lookup(realm, "model-a")
				_, _ = tb.Lookup(realm, "model-c") // 未命中路径同跑
				tb.Size()
			}
		}(w)
	}
	wg.Wait()
	// 收敛校验：两域各 2 键，终值合法（最后一次 Update 的 model-b 恒 0.05）。
	if got := tb.Size(); got != 4 {
		t.Errorf("Size() = %d, want 4（cn/global 各 2 键）", got)
	}
	if v, ok := tb.Lookup("cn", "model-b"); !ok || v != 0.05 {
		t.Errorf("并发收敛后 cn model-b = (%v, %v), want (0.05, true)", v, ok)
	}
}

// TestStoreModelRates 填充点入口：可解析入表、不可解析跳过（= 未知放行）、
// 空 ID/空 infos 零操作。
func TestStoreModelRates(t *testing.T) {
	c := &Client{HTTP: &http.Client{}} // modelRates 零值可用，无需显式初始化

	c.StoreModelRates("global", []ModelInfo{
		{ID: "paid-x", Credits: "x1.62 credits"},
		{ID: "free-y", Credits: "x0.00"},
		{ID: "weird-z", Credits: "xabc"}, // 不可解析 → 跳过
		{ID: "", Credits: "x1"},          // 空 ID → 跳过
		{ID: "no-credits"},               // 缺 Credits → 跳过
	})
	if v, ok := c.modelRates.Lookup("global", "paid-x"); !ok || v != 1.62 {
		t.Errorf("paid-x = (%v, %v), want (1.62, true)", v, ok)
	}
	if v, ok := c.modelRates.Lookup("global", "free-y"); !ok || v != 0 {
		t.Errorf("free-y = (%v, %v), want (0, true)", v, ok)
	}
	for _, m := range []string{"weird-z", "no-credits"} {
		if _, ok := c.modelRates.Lookup("global", m); ok {
			t.Errorf("%s 不可解析/缺失应不入表（未知放行），却命中了", m)
		}
	}

	// 空 infos / 空 realm 容错。
	c.StoreModelRates("cn", nil)
	c.StoreModelRates("", []ModelInfo{{ID: "cn-only", Credits: "0.79"}})
	if v, ok := c.modelRates.Lookup("cn", "cn-only"); !ok || v != 0.79 {
		t.Errorf("cn-only = (%v, %v), want (0.79, true)", v, ok)
	}
}

// TestModelRateOf 装配面：数值 → 字符串、未知 → ""（pool.SetModelRateOf 契约：
// 返回形如 "1.62" 的数值字符串，"" = 未知放行）。
func TestModelRateOf(t *testing.T) {
	c := &Client{HTTP: &http.Client{}}
	c.StoreModelRates("global", []ModelInfo{{ID: "kimi-k3-1", Credits: "x1.62 credits"}})

	if got := c.ModelRateOf("global", "kimi-k3-1"); got != "1.62" {
		t.Errorf("ModelRateOf(global, kimi-k3-1) = %q, want \"1.62\"", got)
	}
	// 明确免费："0"（pool 判 v>0 为收费，"0" 与 "" 同放行但语义更准）。
	c.StoreModelRates("global", []ModelInfo{{ID: "free-x", Credits: "x0.00"}})
	if got := c.ModelRateOf("global", "free-x"); got != "0" {
		t.Errorf("ModelRateOf(global, free-x) = %q, want \"0\"", got)
	}
	// 未覆盖 / 未命中 → ""。
	if got := c.ModelRateOf("global", "unknown-model"); got != "" {
		t.Errorf("ModelRateOf(global, unknown-model) = %q, want \"\"", got)
	}
	// 空 realm → cn 桶。
	if got := c.ModelRateOf("", "kimi-k3-1"); got != "" {
		t.Errorf("ModelRateOf(\"\", kimi-k3-1) = %q, want \"\"（global 条目不得被 cn 命中）", got)
	}
}

// rateSrv 构造一段同时服务 /v3/config、/v2|/console 企业端点与 CN 侧双路的
// 模型目录 fake：两域响应都带 credits 字段，供填充点端到端断言。
func rateSrv(t *testing.T) *httptest.Server {
	t.Helper()
	model := func(id, credits string) string {
		s := `{"id":"` + id + `","name":"` + id + `"`
		if credits != "" {
			s += `,"credits":"` + credits + `"`
		}
		return s + `}`
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		switch r.URL.Path {
		case "/v2/enterprises/personal/models", "/console/enterprises/personal/models":
			// 对象形态单路即可（两域企业端点共用形态）。CN 侧 fetchEnterpriseModels
			// 按 agents[cli] 过滤，故带 cli 面名单；global 侧解析忽略 agents，无副作用。
			_, _ = w.Write([]byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["ent-paid","ent-free"]}]` +
				`,"models":[` + model("ent-paid", "x1.62 credits") + "," + model("ent-free", "x0.00") + `]}}`))
		case "/v3/config":
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[` +
				model("v3-paid", "0.79x") + "," + model("v3-weird", "credits") + `]}}`))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"code":404}`))
		}
	}))
}

// TestFetchModelsFillsRateTable CN 填充点端到端：FetchModels 成功后目录 credits
// 入 cn 桶（global 同名隔离），不可解析条目跳过。
func TestFetchModelsFillsRateTable(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	srv := rateSrv(t)
	defer srv.Close()

	c := &Client{
		HTTP:         &http.Client{},
		ChatBaseCN:   strings.TrimSuffix(srv.URL, "/"),
		BillingBaseCN: strings.TrimSuffix(srv.URL, "/"),
		GlobalEnabled: true,
	}
	c.SyncHot()
	cnAcct := &auth.Auth{AccessToken: "at", RefreshToken: "rt", UID: "c1"}
	if _, err := c.FetchModels(cnAcct); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	// v3 主路条目（0.79x 后缀记号形态）+ 企业端点条目（credits 原文形态）都应入表。
	for _, tc := range []struct {
		model string
		want  float64
	}{
		{"v3-paid", 0.79},
		{"ent-paid", 1.62},
		{"ent-free", 0},
	} {
		if v, ok := c.modelRates.Lookup("cn", tc.model); !ok || v != tc.want {
			t.Errorf("cn %s = (%v, %v), want (%v, true)", tc.model, v, ok, tc.want)
		}
	}
	// 不可解析条目不入表。
	if _, ok := c.modelRates.Lookup("cn", "v3-weird"); ok {
		t.Error("v3-weird（credits=\"credits\"）应不入表")
	}
}

// TestGlobalProbeFillsRateTable global 填充点端到端：FetchGlobalModels 探测成功后
// 目录 credits 入 global 桶，与 cn 桶隔离。
func TestGlobalProbeFillsRateTable(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	srv := rateSrv(t)
	defer srv.Close()

	c := globalModelsClient(t, srv)
	if got := c.FetchGlobalModels(globalAcct()); len(got) == 0 {
		t.Fatalf("FetchGlobalModels = %v, want 非空", got)
	}
	// v3 主路 + 企业端点补充路条目均入 global 桶（同一 fake 服务两路都命中）。
	for _, tc := range []struct {
		model string
		want  float64
	}{
		{"v3-paid", 0.79},
		{"ent-paid", 1.62},
		{"ent-free", 0},
	} {
		if v, ok := c.modelRates.Lookup("global", tc.model); !ok || v != tc.want {
			t.Errorf("global %s = (%v, %v), want (%v, true)", tc.model, v, ok, tc.want)
		}
	}
	// realm 隔离：cn 桶不得被 global 探测污染。
	if _, ok := c.modelRates.Lookup("cn", "v3-paid"); ok {
		t.Error("global 探测不得污染 cn 桶")
	}
}

// TestWarmModelRates 预热：双域各一次探测，成功后两域条目同表；
// nil 账号域跳过不算失败。
func TestWarmModelRates(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	srv := rateSrv(t)
	defer srv.Close()

	c := &Client{
		HTTP:           &http.Client{},
		ChatBaseCN:     strings.TrimSuffix(srv.URL, "/"),
		ChatBaseGlobal: strings.TrimSuffix(srv.URL, "/"),
		GlobalEnabled:  true,
	}
	c.SyncHot()
	if err := WarmModelRates(context.Background(), c, &auth.Auth{AccessToken: "at", RefreshToken: "rt", UID: "c1"}, globalAcct()); err != nil {
		t.Fatalf("WarmModelRates: %v", err)
	}
	if v, ok := c.modelRates.Lookup("cn", "v3-paid"); !ok || v != 0.79 {
		t.Errorf("cn v3-paid = (%v, %v), want (0.79, true)", v, ok)
	}
	if v, ok := c.modelRates.Lookup("global", "ent-paid"); !ok || v != 1.62 {
		t.Errorf("global ent-paid = (%v, %v), want (1.62, true)", v, ok)
	}
	if got := c.modelRates.Size(); got < 4 {
		t.Errorf("Size() = %d, want >= 4（两域各 ≥2 条）", got)
	}

	// 双 nil 账号：无域可探，零操作且不算失败。
	c2 := &Client{HTTP: &http.Client{}}
	if err := WarmModelRates(context.Background(), c2, nil, nil); err != nil {
		t.Errorf("WarmModelRates(nil, nil) = %v, want nil", err)
	}
	if got := c2.modelRates.Size(); got != 0 {
		t.Errorf("双 nil 后 Size() = %d, want 0", got)
	}
}

// TestWarmModelRatesSingleDomainFail 单域失败降级另一域：CN 打不通（负缓存口径）
// 不拖累 global 入表，函数不返回错误（探测链路同款降级口径，启动不被阻断）。
func TestWarmModelRatesSingleDomainFail(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	// CN base 指向不通的地址（连接拒绝），global base 指向 fake。
	srv := rateSrv(t)
	defer srv.Close()

	c := &Client{
		HTTP:           &http.Client{},
		ChatBaseCN:     "http://127.0.0.1:1", // 拒绝连接
		ChatBaseGlobal: strings.TrimSuffix(srv.URL, "/"),
		GlobalEnabled:  true,
	}
	c.SyncHot()
	if err := WarmModelRates(context.Background(), c, &auth.Auth{AccessToken: "at", RefreshToken: "rt", UID: "c1"}, globalAcct()); err != nil {
		t.Fatalf("单域失败应降级不报错: %v", err)
	}
	if v, ok := c.modelRates.Lookup("global", "v3-paid"); !ok || v != 0.79 {
		t.Errorf("global v3-paid = (%v, %v), want (0.79, true)（global 域应正常入表）", v, ok)
	}
}

// TestWarmModelRatesCtxCanceled ctx 已取消：直接返回 ctx 错误，零探测。
func TestWarmModelRatesCtxCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &Client{HTTP: &http.Client{}}
	if err := WarmModelRates(ctx, c, nil, nil); err == nil {
		t.Fatal("ctx 已取消应返回错误")
	}
}
