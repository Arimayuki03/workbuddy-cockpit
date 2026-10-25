package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// TestMain 签到限速（checkinAccountDelay 3s/号）与瞬时失败重试（5/15/60s）是
// 上游真实节奏（manager patch 0029 吸收），测试默认清零——多号签到的固定间隔
// 会让包测试拖到数分钟；需要断言重试行为的用例自行设置（见
// TestDailyCheckinRetriesTransientFailure）。
func TestMain(m *testing.M) {
	oldDelay := checkinAccountDelay
	oldRetries := checkinRetryDelays
	checkinAccountDelay = 0
	checkinRetryDelays = nil
	defer func() {
		checkinAccountDelay = oldDelay
		checkinRetryDelays = oldRetries
	}()
	os.Exit(m.Run())
}

// TestDailyCheckinRetriesTransientFailure 签到撞上游瞬时拒绝（HTTP 500 / code
// 10001 且非「已签到」幂等形态）时按退避序列重试，第二次成功即不算失败。
func TestDailyCheckinRetriesTransientFailure(t *testing.T) {
	var checkinCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/billing/meter/daily-checkin" || r.URL.Path == "/billing/meter/daily-checkin":
			if checkinCalls.Add(1) == 1 {
				// 首次：瞬时拒绝（issue #256 实测形态：批量并发被限流）。
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"code":10001,"msg":"签到失败，请稍后重试"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case r.URL.Path == "/v2/billing/meter/get-user-resource" || r.URL.Path == "/billing/meter/get-user-resource":
			_, _ = w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":100,"CycleCapacityUsed":0}]}}}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	// 单测节奏：重试间隔清零（重试次数语义不变：1 次初试 + 1 次重试成功）。
	checkinRetryDelays = []time.Duration{0}
	t.Cleanup(func() { checkinRetryDelays = nil })

	outcomes, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("CheckinAll: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].Status != CheckinOK {
		t.Fatalf("瞬时失败重试后应成功: %+v", outcomes)
	}
	if n := checkinCalls.Load(); n != 2 {
		t.Fatalf("daily-checkin calls=%d want 2 (1 fail + 1 retry ok)", n)
	}
}

// TestCheckinRetryable 判据边界：5xx 与 10001 可重试；「已签到」幂等与其它错误不可。
func TestCheckinRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"5xx", &upstream.Error{Status: 500, Msg: "boom"}, true},
		{"code 10001 json", &upstream.Error{Status: 400, Msg: `{"code":10001,"msg":"签到失败，请稍后重试"}`}, true},
		{"code 10001 text", &upstream.Error{Status: 200, Msg: "code=10001 msg=签到失败，请稍后重试"}, true},
		// 「已签到」同为 10001：checkinRetryable 本身不排除它（IsAlreadyCheckin 在
		// dailyCheckinWithRetry 里先拦），这里锁住判据的原始语义。
		{"already checkin", &upstream.Error{Status: 200, Msg: "code=10001 msg=今天已签到"}, true},
		{"other code", &upstream.Error{Status: 400, Msg: `{"code":11101,"msg":"bad params"}`}, false},
		{"plain error", errPlain, false},
	}
	for _, c := range cases {
		if got := checkinRetryable(c.err); got != c.want {
			t.Errorf("%s: checkinRetryable=%v want %v", c.name, got, c.want)
		}
	}
}

var errPlain = context.DeadlineExceeded
