package upstream

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// mkPackagesResp 构造 get-user-resource 逐包明细响应（形态照上游真实 JSON：信封
// code/data 两层，data 下 Response.Data.Accounts；DeductionEndTime 是 epoch 毫秒，
// CycleEndTime 是 UTC+8 墙钟字符串——两种口径共存，解析层都要吃下）。
func mkPackagesResp(accounts string) *http.Response {
	body := `{"code":0,"msg":"ok","data":{"Response":{"Data":{"TotalCount":1,"Accounts":[` + accounts + `]}}}}`
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

// 真实响应形态（panel commit acb3830c 实测结论）：DeductionEndTime 是真失效时刻
// （epoch 毫秒），ExpiredTime 恒空；请求参数叫 PackageEndTimeRange* 但响应里没有
// PackageEndTime 字段——判「用不完就没了」以 DeductionEndTime 为准。
func TestCreditPackagesDeductionEndTimePrimary(t *testing.T) {
	// 固定毫秒时间戳：1792459200000 = 2026-10-20 09:20:00 UTC+8。
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkPackagesResp(
			`{"PackageName":"国内运营裂变包","DeductionEndTime":1792459200000,"CycleEndTime":"2026-11-01 00:00:00","CycleCapacitySize":1500,"CycleCapacityRemain":1200,"CycleCapacityUsed":300},` +
				`{"PackageName":"周期包","CycleEndTime":"2026-11-15 00:00:00","CycleCapacitySize":500,"CycleCapacityRemain":300,"CycleCapacityUsed":200}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	packs, remain, size, err := c.CreditPackages(a)
	if err != nil {
		t.Fatalf("CreditPackages: %v", err)
	}
	if len(packs) != 2 {
		t.Fatalf("packs=%d want 2", len(packs))
	}
	if remain != 1500 || size != 2000 {
		t.Errorf("remain=%d size=%d want 1500/2000", remain, size)
	}
	// 包 1：DeductionEndTime 优先，CycleEndTime（更晚的周期边界）不被误用。
	p := packs[0]
	if p.Name != "国内运营裂变包" {
		t.Errorf("order: first pack = %s (want DeductionEndTime 更早的裂变包)", p.Name)
	}
	if p.EndTime != "2026-10-20T09:20:00+08:00" {
		t.Errorf("EndTime=%q want 2026-10-20T09:20:00+08:00 (epoch 毫秒转 RFC3339)", p.EndTime)
	}
	if p.ExpiresAt != 1792459200000 {
		t.Errorf("ExpiresAt=%d want 1792459200000", p.ExpiresAt)
	}
	if !p.Cycle {
		t.Error("CycleCapacitySize>0 的包 Cycle 应为 true")
	}
	// remain 钳制与 used 修正走 packageRemainUsed 同一事实来源。
	if p.Remain != 1200 || p.Used != 300 || p.Size != 1500 {
		t.Errorf("remain/used/size = %d/%d/%d want 1200/300/1500", p.Remain, p.Used, p.Size)
	}
	// 包 2：无 DeductionEndTime → 回落 CycleEndTime（墙钟字符串），并换算出毫秒。
	q := packs[1]
	if q.EndTime != "2026-11-15 00:00:00" {
		t.Errorf("fallback EndTime=%q want 原始墙钟字符串", q.EndTime)
	}
	want, _ := time.ParseInLocation(packageEndLayout, "2026-11-15 00:00:00", softRateResetLoc)
	if q.ExpiresAt != want.UnixMilli() {
		t.Errorf("fallback ExpiresAt=%d want %d", q.ExpiresAt, want.UnixMilli())
	}
}

// 到期升序（FEFO 口径）：快过期的在前，无到期时间的垫底；同到期按面额降序。
func TestCreditPackagesSortsByExpiryAsc(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkPackagesResp(
			`{"PackageName":"无到期","CycleCapacitySize":100,"CycleCapacityRemain":100},` +
				`{"PackageName":"晚到期","DeductionEndTime":1893459200000,"CycleCapacitySize":300,"CycleCapacityRemain":300},` +
				`{"PackageName":"早到期大包","DeductionEndTime":1792459200000,"CycleCapacitySize":1500,"CycleCapacityRemain":1500},` +
				`{"PackageName":"早到期小包","DeductionEndTime":1792459200000,"CycleCapacitySize":200,"CycleCapacityRemain":200}`), nil
	})
	packs, _, _, err := c.CreditPackages(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("CreditPackages: %v", err)
	}
	want := []string{"早到期大包", "早到期小包", "晚到期", "无到期"}
	for i, w := range want {
		if packs[i].Name != w {
			t.Errorf("packs[%d]=%s want %s (full order: %s, %s, %s, %s)",
				i, packs[i].Name, w, packs[0].Name, packs[1].Name, packs[2].Name, packs[3].Name)
		}
	}
}

// Capacity 三字段路径（CycleCapacitySize=0 的按次发放包）：used 取 size-remain 修正。
func TestCreditPackagesCapacityFallback(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkPackagesResp(
			`{"PackageName":"按次包","CapacitySize":600,"CapacityRemain":450,"CreateTime":1792000000000,"PackageCode":"TCACA_code_007","SubProductName":"国内运营裂变包"}`), nil
	})
	packs, remain, size, err := c.CreditPackages(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("CreditPackages: %v", err)
	}
	if len(packs) != 1 {
		t.Fatalf("packs=%d want 1", len(packs))
	}
	p := packs[0]
	if p.Cycle {
		t.Error("CycleCapacitySize=0 的包 Cycle 应为 false")
	}
	if p.Remain != 450 || p.Used != 150 || p.Size != 600 {
		t.Errorf("remain/used/size = %d/%d/%d want 450/150/600", p.Remain, p.Used, p.Size)
	}
	if remain != 450 || size != 600 {
		t.Errorf("sums remain=%d size=%d want 450/600", remain, size)
	}
	// 无任何到期字段：EndTime/ExpiresAt 留空零值（不伪造 1970）。
	if p.EndTime != "" || p.ExpiresAt != 0 {
		t.Errorf("no-expiry pack EndTime=%q ExpiresAt=%d want empty/0", p.EndTime, p.ExpiresAt)
	}
	// 发放时刻 epoch 毫秒 → RFC3339；来源标识原样透出。
	if p.CreatedAt == "" {
		t.Error("CreateTime>0 应产出 CreatedAt")
	}
	if p.PackageCode != "TCACA_code_007" || p.SubProductName != "国内运营裂变包" {
		t.Errorf("codes = %s/%s", p.PackageCode, p.SubProductName)
	}
}

// 字段缺失/零值容错：全空的包不炸、不计入到期、remain 不为负。
func TestCreditPackagesZeroValueTolerant(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkPackagesResp(
			`{"PackageName":"空包","CapacityRemain":-5,"DeductionEndTime":0,"CycleEndTime":""}`), nil
	})
	packs, _, _, err := c.CreditPackages(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("CreditPackages: %v", err)
	}
	if len(packs) != 1 {
		t.Fatalf("packs=%d want 1", len(packs))
	}
	if packs[0].Remain != 0 {
		t.Errorf("negative remain not clamped: %d", packs[0].Remain)
	}
	if packs[0].EndTime != "" || packs[0].ExpiresAt != 0 {
		t.Errorf("dirty expiry not tolerated: %q/%d", packs[0].EndTime, packs[0].ExpiresAt)
	}
}
