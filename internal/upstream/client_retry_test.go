// client_retry_test.go 钉住签到/余额维护类计费调用的瞬时错误有界重试（吸收 panel
// commit dd4ea34d）：上游 5xx（实测偶发 code 10000 / http 500）与网络抖动重试，
// 业务错误（已签到/参数错/风控 4xx）不重试——重试只会原样再失败一次。
package upstream

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// shortBillingRetry 测试用重试间隔（生产 2s 会让单测秒级膨胀）。
func shortBillingRetry(t *testing.T) {
	t.Helper()
	old := billingRetryDelay
	billingRetryDelay = time.Millisecond
	t.Cleanup(func() { billingRetryDelay = old })
}

// checkinBody code 10000 形态的上游 5xx 信封（上游 billing 网关偶发故障的实测原文）。
const checkin500Body = `{"code":10000,"msg":"API request failed with status code: 500"}`

// TestRetryBillingTransientHelper helper 单元口径：ErrServer/网络层错误算瞬时，
// 其余分类（限流/余额/风控/客户端业务错误）与 nil 一律不算。
// 注：分类返回 ErrClient 的业务错误不重试；非 *Error 的普通 error（网络抖动/
// 网关脏响应）按瞬时算（对齐 panel 参考口径，见 TestUserResourceDetailedRetriesPlainError）。
func TestRetryBillingTransientHelper(t *testing.T) {
	if isTransientBillingErr(nil) {
		t.Error("nil 不是瞬时错误")
	}
	if !isTransientBillingErr(&Error{Kind: ErrServer, Status: 500, Msg: checkin500Body}) {
		t.Error("ErrServer（code 10000 / http 500）应判瞬时")
	}
	if !isTransientBillingErr(errors.New("connection reset by peer")) {
		t.Error("网络层普通 error（非 *Error）应判瞬时")
	}
	for _, ue := range []*Error{
		{Kind: ErrClient, Status: 200, Msg: "code=14001 msg=今日已签到"},
		{Kind: ErrSoftRate, Status: 429, Msg: "too many requests"},
		{Kind: ErrHardCredit, Status: 402, Msg: "insufficient credit"},
		{Kind: ErrAccountFault, Status: 403, Msg: "request illegal"},
	} {
		if isTransientBillingErr(ue) {
			t.Errorf("Kind=%s 不应判瞬时（业务错误不重试）", ue.Kind)
		}
	}
}

// TestDailyCheckinRetriesTransient500 签到首次 500 → 重试成功（calls=2）。
func TestDailyCheckinRetriesTransient500(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/daily-checkin") {
			return nil, errors.New("wrong path")
		}
		if atomic.AddInt32(&calls, 1) == 1 {
			return jsonResp(500, checkin500Body), nil
		}
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	if err := c.DailyCheckin(&auth.Auth{AccessToken: "at"}); err != nil {
		t.Fatalf("首次 500 应重试成功，err=%v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("calls=%d want 2（1 次失败 + 1 次重试）", n)
	}
}

// TestDailyCheckinRetriesExhausted 持续 500 → 首次 + 2 次补打封顶后返回错误。
func TestDailyCheckinRetriesExhausted(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return jsonResp(500, checkin500Body), nil
	})
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatal("持续 500 应返回错误")
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Fatalf("calls=%d want 3（1 次 + 2 次重试封顶）", n)
	}
}

// TestDailyCheckinNoRetryOnBusinessError 「今日已签到」是业务幂等拒绝（code!=0），
// 重试无意义——calls=1 且错误仍可被 IsAlreadyCheckin 识别（调用方语义零漂移）。
func TestDailyCheckinNoRetryOnBusinessError(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return jsonResp(200, `{"code":14001,"msg":"今日已签到"}`), nil
	})
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil || !IsAlreadyCheckin(err) {
		t.Fatalf("err=%v want already-checkin business error", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("calls=%d want 1（业务错误不重试）", n)
	}
}

// TestDailyCheckinNoRetryOn4xx 403 风控（11140 request illegal）非瞬时，不重试。
func TestDailyCheckinNoRetryOn4xx(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return jsonResp(403, `{"code":11140,"msg":"request illegal"}`), nil
	})
	if err := c.DailyCheckin(&auth.Auth{AccessToken: "at"}); err == nil {
		t.Fatal("403 应返回错误")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("calls=%d want 1（4xx 非瞬时，不重试）", n)
	}
}

// TestUserResourceDetailedRetriesTransient500 余额查询首次 500 → 重试成功，
// remain 聚合口径不变（panel 的 UserResourceDetailedWithExpiry 修复点同型）。
func TestUserResourceDetailedRetriesTransient500(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/get-user-resource") {
			return nil, errors.New("wrong path")
		}
		if atomic.AddInt32(&calls, 1) == 1 {
			return jsonResp(500, checkin500Body), nil
		}
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"PackageName":"p","CapacitySize":100,"CapacityRemain":60}
		]}}}}`), nil
	})
	remain, _, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 0)
	if err != nil || remain != 60 {
		t.Fatalf("remain=%d err=%v, want 60 nil", remain, err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("calls=%d want 2（1 次失败 + 1 次重试）", n)
	}
}

// TestUserResourceDetailedRetriesPlainError 网络层/解析层普通 error（非 *Error）
// 按瞬时口径处理（对齐 panel 参考实现 isTransientBillingErr：非 *Error 一律算
// 瞬时——200 + 半截/脏 body 常是网关抖动，重打一次值得）；有界 3 次封顶。
func TestUserResourceDetailedRetriesPlainError(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return jsonResp(200, `<html>gateway hiccup</html>`), nil
	})
	if _, _, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 0); err == nil {
		t.Fatal("解析失败应返回错误")
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Fatalf("calls=%d want 3（普通 error 按瞬时重试，1 次 + 2 次封顶）", n)
	}
}
