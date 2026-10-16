// compensation_test.go v1.19.1 面板「活动补偿 → 立即执行」端点契约：
//
//	POST /api/compensation/run → {ok, accounts, claimed, credits, message}（同步）
//
// 前端按 web/lib/types.ts CompensationRunResponse 消费（字段缺失按 0 容错、ok=false
// 时用 message 弹错误提示），因此这里钉住：形状（五个字段都在）、鉴权、失败分支
// （HTTP 200 + ok=false + message 说明原因；不是 5xx——fetch 层会把非 2xx 当成
// 「请求本身失败」而丢掉计数明细）、以及依赖缺失（scheduler 未装配 → 501）与
// 重入（同类任务在跑 → 409）两个护栏。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/upstream"
)

// newCompPanel 组装「面板 + 真调度器 + 补偿假上游」的最小装配。
// claimBody 为空时用默认（到账 300）；否则领取端点按该 body 返回。
func newCompPanel(t *testing.T, statusBody, claimBody string) *Panel {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/billing/meter/compensation-status":
			if statusBody == "" {
				statusBody = `{"code":0,"data":{"claimed":false}}`
			}
			_, _ = w.Write([]byte(statusBody))
		case "/billing/meter/claim-compensation":
			if claimBody == "" {
				claimBody = `{"code":0,"data":{"credit":300}}`
			}
			_, _ = w.Write([]byte(claimBody))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"code":404,"msg":"not found"}`))
		}
	}))
	t.Cleanup(srv.Close)

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	return New(Config{Version: "test", APIKey: "test-key", Pool: p, Upstream: up, Scheduler: sch})
}

// postComp 发一次带鉴权的 POST /api/compensation/run。
func postComp(t *testing.T, p *Panel, key string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/compensation/run", nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	p.ServeHTTP(rec, req)
	return rec
}

// TestCompensationRunResponseShape 成功路径：五字段齐全且计数正确（前端据此渲染
//「N 个号尝试 · M 个领到 · 共 X 积分」）。
func TestCompensationRunResponseShape(t *testing.T) {
	p := newCompPanel(t, "", "")
	rec := postComp(t, p, "test-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK       bool   `json:"ok"`
		Accounts int    `json:"accounts"`
		Claimed  int    `json:"claimed"`
		Credits  int64  `json:"credits"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON 契约：%v（body=%s）", err, rec.Body.String())
	}
	if !resp.OK || resp.Accounts != 1 || resp.Claimed != 1 || resp.Credits != 300 {
		t.Errorf("resp=%+v want ok=true accounts=1 claimed=1 credits=300", resp)
	}
	if resp.Message == "" {
		t.Error("message 不应为空（前端在无 message 时才回落到默认文案）")
	}
	// 五个字段都必须出现在 JSON 里（前端按可选字段容错，但缺字段会显示 0）。
	for _, key := range []string{"ok", "accounts", "claimed", "credits", "message"} {
		if !strings.Contains(rec.Body.String(), `"`+key+`"`) {
			t.Errorf("响应缺字段 %q：%s", key, rec.Body.String())
		}
	}
}

// TestCompensationRunNothingIsOK 没有可领（业务拒绝）→ ok=true、claimed=0；
// 「今天本来就没得领」不是失败（否则用户会反复重试一个永远不会成功的动作）。
func TestCompensationRunNothingIsOK(t *testing.T) {
	p := newCompPanel(t, "", `{"code":40001,"msg":"no compensation"}`)
	rec := postComp(t, p, "test-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK       bool   `json:"ok"`
		Accounts int    `json:"accounts"`
		Claimed  int    `json:"claimed"`
		Message  string `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.OK || resp.Claimed != 0 || resp.Accounts != 1 {
		t.Errorf("resp=%+v want ok=true claimed=0 accounts=1", resp)
	}
	if !strings.Contains(resp.Message, "无可领") {
		t.Errorf("message=%q 应说明没有可领", resp.Message)
	}
}

// TestCompensationRunFailureIsOK200WithOkFalse 领取失败 → **HTTP 200 + ok=false +
// message 说明原因**（非 2xx 会被前端 fetch 层当成请求本身失败，丢掉计数明细）。
func TestCompensationRunFailureIsOK200WithOkFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/compensation-status") {
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
			return
		}
		w.WriteHeader(401) // token 失效：真失败（业务拒绝才是 nothing）
		_, _ = w.Write([]byte(`{"code":12153,"msg":"offline session"}`))
	}))
	defer srv.Close()
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pl, Upstream: up,
		Scheduler: scheduler.New(scheduler.Config{Pool: pl, Upstream: up})})

	rec := postComp(t, p, "test-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("失败也必须 HTTP 200（前端按 ok 字段判失败）：code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK       bool   `json:"ok"`
		Accounts int    `json:"accounts"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON：%v", err)
	}
	if resp.OK {
		t.Error("有账号领取失败时 ok 必须为 false")
	}
	if resp.Accounts != 1 {
		t.Errorf("accounts=%d want 1（失败也是一次尝试）", resp.Accounts)
	}
	if !strings.Contains(resp.Message, "失败") {
		t.Errorf("message=%q 应说明失败原因", resp.Message)
	}
}

// TestCompensationRunAuth 鉴权闸：不带凭据 401（与 /api/* 全族同 withAuth 口径）。
func TestCompensationRunAuth(t *testing.T) {
	p := newCompPanel(t, "", "")
	rec := postComp(t, p, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无凭据 code=%d want 401", rec.Code)
	}
}

// TestCompensationRunSchedulerMissing scheduler 未装配（老装配/最小形态）→ 501，
// 不是 500 也不是静默 ok=true。
func TestCompensationRunSchedulerMissing(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key", Pool: newSmokePool()})
	rec := postComp(t, p, "test-key")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("code=%d want 501", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Errorf("错误响应应为 ok=false 形态：%s", rec.Body.String())
	}
}

// TestCompensationRunBusy409 同类任务在跑（同步端点怕叠加）→ 409；释放后即可执行。
func TestCompensationRunBusy409(t *testing.T) {
	p := newCompPanel(t, "", "")
	if !p.tryLockBatch("compensation") {
		t.Fatal("首次取锁应成功")
	}
	if rec := postComp(t, p, "test-key"); rec.Code != http.StatusConflict {
		t.Fatalf("在跑时 code=%d want 409", rec.Code)
	}
	p.unlockBatch("compensation")
	if rec := postComp(t, p, "test-key"); rec.Code != http.StatusOK {
		t.Fatalf("释放后 code=%d want 200", rec.Code)
	}
}
