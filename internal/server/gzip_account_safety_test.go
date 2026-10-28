package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// TestGzipBodyRejected11140DoesNotDisableAccount 压缩体被上游以 11140
// request illegal 拒绝时，upstream.Client 必须先做一次未压缩重试；未压缩
// 成功则请求照常 200，且不能把这个请求级错误当账号故障 Disable。
//
// 修复前的致命链路：gzip 默认开启 → 上游对压缩体回 400/11140 → 客户端
// 不重试未压缩 → handler 按 request illegal 硬禁用账号 → 轮转下一个号仍发
// 同一份压缩体，逐个账号被永久禁用。
func TestGzipBodyRejected11140DoesNotDisableAccount(t *testing.T) {
	var gzipCalls, plainCalls atomic.Int32
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Content-Encoding") == "gzip" {
			gzipCalls.Add(1)
			return testSSEResponse(http.StatusBadRequest,
				`{"code":11140,"msg":"request illegal"}`), nil
		}
		plainCalls.Add(1)
		return testSSEResponse(http.StatusOK, sseOK), nil
	})
	// 用 New() 初始化完整默认值（含 proxyClients / hot 快照），再替换出站
	// client 为 fake：GzipBody 默认开启，SyncHot 后读侧快照才看得到。
	up := upstream.New()
	up.ChatBaseCN = "https://fake.example"
	up.BillingBaseCN = "https://fake.example"
	up.GzipBody = true
	up.SyncHot()
	up.HTTP = &http.Client{Transport: rt}
	up.ChatHTTP = &http.Client{Transport: rt}

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	// >64KB（gzipBodyThreshold）才触发压缩；长上下文是真实触发场景。
	body := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"` +
		strings.Repeat("长上下文", 20000) + `"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (uncompressed retry must succeed)", rec.Code, rec.Body)
	}
	if gzipCalls.Load() != 1 || plainCalls.Load() != 1 {
		t.Fatalf("upstream calls gzip=%d plain=%d want 1/1 (compress then uncompressed retry)",
			gzipCalls.Load(), plainCalls.Load())
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("account u1 missing from pool")
	}
	if st.Disabled {
		t.Fatalf("account must NOT be disabled: reason=%q (11140 came from the gzip body, "+
			"uncompressed request succeeded)", st.DisabledReason)
	}
	if st.Cooling {
		t.Fatalf("account must NOT be cooling after a successful request: %+v", st)
	}
}
