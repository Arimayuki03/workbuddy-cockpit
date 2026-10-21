package auth

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// 账号手写权重（issue #187）：auth 文件顶层 `weight`，面板写入。
// 这一组用例锁死三件事：双形态解析、非法值口径、以及 token 刷新/导出时的保留。

func readWeightTestFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return raw
}

func TestParseWeightNestedAndFlat(t *testing.T) {
	nested := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1,"domain":"d","realm":"cn"},
		"account":{"uid":"u1","nickname":"n"},"device_token":"dt","weight":2.5}`)
	a, err := Parse(nested)
	if err != nil {
		t.Fatalf("nested parse: %v", err)
	}
	if a.Weight != 2.5 {
		t.Errorf("嵌套形顶层 weight=%v want 2.5", a.Weight)
	}
	if a.AccessToken != "at" || a.RefreshToken != "rt" || a.UID != "u1" || a.DeviceToken != "dt" {
		t.Errorf("嵌套形凭证被权重解析破坏: %+v", a)
	}

	flat := []byte(`{"accessToken":"at-flat","expiresAt":1,"uid":"u2","weight":3}`)
	b, err := Parse(flat)
	if err != nil {
		t.Fatalf("flat parse: %v", err)
	}
	if b.Weight != 3 {
		t.Errorf("扁平形权重=%v want 3", b.Weight)
	}
	if b.AccessToken != "at-flat" || b.UID != "u2" {
		t.Errorf("扁平形凭证被权重解析破坏: %+v", b)
	}

	none := []byte(`{"accessToken":"at","expiresAt":1,"uid":"u3"}`)
	c, err := Parse(none)
	if err != nil {
		t.Fatalf("no-weight parse: %v", err)
	}
	if c.Weight != 0 {
		t.Errorf("没写 weight 时应为未设置(0)，实际 %v", c.Weight)
	}
	if got := c.PickWeight(); got != 1 {
		t.Errorf("未设置权重的生效值=%v want 1", got)
	}
}

// 负数/非有限值当「未设置」；合法但越界值保留原样（钳制在 PickWeight 做，
// 面板按原值回显「用户填了什么」）。
func TestParseWeightSanitizesOnlyInvalid(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want float64
	}{
		{"未设置", `{"accessToken":"at","expiresAt":1,"uid":"u"}`, 0},
		{"显式零", `{"accessToken":"at","expiresAt":1,"uid":"u","weight":0}`, 0},
		{"负数当未设置", `{"accessToken":"at","expiresAt":1,"uid":"u","weight":-2}`, 0},
		{"字符串当未设置", `{"accessToken":"at","expiresAt":1,"uid":"u","weight":"2.5"}`, 0},
		{"布尔当未设置", `{"accessToken":"at","expiresAt":1,"uid":"u","weight":true}`, 0},
		{"null 当未设置", `{"accessToken":"at","expiresAt":1,"uid":"u","weight":null}`, 0},
		{"超大数字当未设置", `{"accessToken":"at","expiresAt":1,"uid":"u","weight":1e999}`, 0},
		{"太小保留原样", `{"accessToken":"at","expiresAt":1,"uid":"u","weight":0.01}`, 0.01},
		{"太大保留原样", `{"accessToken":"at","expiresAt":1,"uid":"u","weight":1e9}`, 1e9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := Parse([]byte(tc.raw))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if a.Weight != tc.want {
				t.Errorf("weight=%v want %v", a.Weight, tc.want)
			}
		})
	}
	// NaN/Inf 不能出现在合法 JSON 里，直接锁 sanitizeWeight 的防御口径。
	if got := sanitizeWeight(math.NaN()); got != 0 {
		t.Errorf("sanitizeWeight(NaN)=%v want 0", got)
	}
	if got := sanitizeWeight(math.Inf(1)); got != 0 {
		t.Errorf("sanitizeWeight(+Inf)=%v want 0", got)
	}
	if got := sanitizeWeight(math.Inf(-1)); got != 0 {
		t.Errorf("sanitizeWeight(-Inf)=%v want 0", got)
	}
}

func TestPickWeightClamps(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want float64
	}{
		{"未设置=1", 0, 1},
		{"正常值原样", 2.5, 2.5},
		{"下边界", MinWeight, MinWeight},
		{"上边界", MaxWeight, MaxWeight},
		{"低于下限钳到 MinWeight", 0.01, MinWeight},
		{"高于上限钳到 MaxWeight", 1e9, MaxWeight},
		{"负数=默认", -3, 1},
		{"NaN=默认", math.NaN(), 1},
		{"+Inf=默认", math.Inf(1), 1},
		{"-Inf=默认", math.Inf(-1), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Auth{Weight: tc.in}
			if got := a.PickWeight(); got != tc.want {
				t.Errorf("PickWeight(%v)=%v want %v", tc.in, got, tc.want)
			}
		})
	}
	// nil 接收者防御（池/面板不应 panic）。
	var nilAuth *Auth
	if got := nilAuth.PickWeight(); got != 1 {
		t.Errorf("nil.PickWeight()=%v want 1", got)
	}
}

// SetWeight 是运行期唯一写入口：非法值按未设置处理，合法值原样落内存。
func TestSetWeightSanitizesAndRoundTrips(t *testing.T) {
	a := &Auth{}
	a.SetWeight(4.5)
	if got := a.WeightValue(); got != 4.5 {
		t.Errorf("WeightValue=%v want 4.5", got)
	}
	if got := a.PickWeight(); got != 4.5 {
		t.Errorf("PickWeight=%v want 4.5", got)
	}
	a.SetWeight(-1)
	if got := a.WeightValue(); got != 0 {
		t.Errorf("负数应清为未设置: %v", got)
	}
	a.SetWeight(math.Inf(1))
	if got := a.WeightValue(); got != 0 {
		t.Errorf("Inf 应清为未设置: %v", got)
	}
}

// TestSaveAtomicPreservesWeightAcrossRefresh 模拟 token 刷新后整份重写：
// 凭证字段更新、weight 与 device_token/proxy_route 等账号配置必须原样保留。
func TestSaveAtomicPreservesWeightAcrossRefresh(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{
			name: "嵌套形",
			raw: `{"auth":{"accessToken":"old","refreshToken":"rt","expiresAt":1,"domain":"d","realm":"cn","proxy_route":"route-a"},
				"account":{"uid":"u1","enterpriseId":"e1","nickname":"n1"},"device_token":"dt-1","weight":4}`,
		},
		{
			name: "扁平形",
			raw: `{"accessToken":"old","refreshToken":"rt","expiresAt":1,"domain":"d","uid":"u2",
				"nickname":"n2","device_token":"dt-2","proxy_route":"route-b","weight":2.5}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "workbuddy-test.json")
			if err := os.WriteFile(path, []byte(tc.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			a, err := Parse(readWeightTestFile(t, path))
			if err != nil {
				t.Fatal(err)
			}
			a.FilePath = path
			wantWeight := a.Weight
			wantUID, wantNick := a.UID, a.Nickname
			wantDevice := a.DeviceToken
			wantRoute := a.ProxyRoute
			// 模拟 RefreshToken 在锁内的写回。
			a.AccessToken = "new-at"
			a.RefreshToken = "new-rt"
			a.ExpiresAt = 1753600000
			if err := a.SaveAtomic(); err != nil {
				t.Fatalf("save: %v", err)
			}
			b, err := Parse(readWeightTestFile(t, path))
			if err != nil {
				t.Fatalf("reparse saved: %v", err)
			}
			if b.Weight != wantWeight {
				t.Errorf("刷新写回后 weight=%v want %v", b.Weight, wantWeight)
			}
			if b.AccessToken != "new-at" || b.RefreshToken != "new-rt" || b.ExpiresAt != 1753600000 {
				t.Errorf("刷新写回后 token 不一致: %+v", b)
			}
			if b.UID != wantUID || b.Nickname != wantNick || b.DeviceToken != wantDevice || b.ProxyRoute != wantRoute {
				t.Errorf("写回丢失账号配置: %+v", b)
			}
		})
	}
}

// 没有权重的文件不该被凭空加上 weight 键（否则每次刷新都在所有账号上留噪音）。
func TestSaveAtomicOmitsUnsetWeight(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy-u2.json")
	if err := os.WriteFile(path, []byte(`{"accessToken":"old","expiresAt":1,"uid":"u2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Parse(readWeightTestFile(t, path))
	if err != nil {
		t.Fatal(err)
	}
	a.FilePath = path
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(readWeightTestFile(t, path), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["weight"]; ok {
		t.Errorf("未设置权重时不该写入 weight 键: %v", doc["weight"])
	}
}

// 导出文档带 weight 且能被 Parse 回读（导入方无需特殊分支）。
func TestExportDocWeightRoundTrip(t *testing.T) {
	a := &Auth{AccessToken: "at", RefreshToken: "rt", UID: "u1", Weight: 7.5}
	raw, err := json.Marshal(a.ExportDoc())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"weight":7.5`) {
		t.Fatalf("导出文档缺 weight: %s", raw)
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(export): %v", err)
	}
	if b.Weight != 7.5 || b.AccessToken != "at" || b.UID != "u1" {
		t.Errorf("导出/导入权重或凭证不一致: %+v", b)
	}
}

// SetWeightAndSave is the panel write path: one lock covers field update and
// whole-file atomic save, so a failed save rolls back only this request's value.
func TestSetWeightAndSaveAtomicAndRollback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy-u1.json")
	a := &Auth{AccessToken: "at", RefreshToken: "rt", UID: "u1", Weight: 2, FilePath: path}
	if _, err := a.SetWeightAndSave(3.5); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := a.WeightValue(); got != 3.5 {
		t.Fatalf("weight=%v want 3.5", got)
	}
	var doc map[string]any
	if err := json.Unmarshal(readWeightTestFile(t, path), &doc); err != nil {
		t.Fatal(err)
	}
	if got, _ := doc["weight"].(float64); got != 3.5 {
		t.Errorf("persisted weight=%v want 3.5", doc["weight"])
	}

	// Force writeFileAtomic to fail: FilePath is an existing directory, so the
	// .tmp open fails before rename. Memory must roll back to 3.5.
	a.FilePath = dir
	got, err := a.SetWeightAndSave(8)
	if err == nil {
		t.Fatal("save into a directory should fail")
	}
	if got != 3.5 || a.WeightValue() != 3.5 {
		t.Errorf("failed save did not roll back: stored=%v weight=%v", got, a.WeightValue())
	}
}

// 并发读写不变量：运行期面板改权重与选号/状态查询并发时必须安全
// （本用例在 -race 下有实际意义；无 race 时也验证最终值一致）。
func TestWeightConcurrentAccess(t *testing.T) {
	a := &Auth{Weight: 1}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(v float64) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				a.SetWeight(v)
			}
		}(float64(i + 1))
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = a.PickWeight()
				_ = a.WeightValue()
			}
		}()
	}
	wg.Wait()
	if got := a.PickWeight(); got < MinWeight || got > MaxWeight {
		t.Errorf("并发写入后生效权重越界: %v", got)
	}
}
