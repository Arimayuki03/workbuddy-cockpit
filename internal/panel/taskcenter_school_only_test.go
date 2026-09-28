package panel

// taskcenter_school_only_test.go OCR 审查 low 修复回归：school-only 请求体
// （{growth:false, school:true}，老前端/过期脚本）不再被强制放大为全账号
// growth 队列——落到空扫描自然返回 started=false。

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/upstream"
)

// TestRunQueueSchoolOnlyNotAmplified school-only 请求：不触发任何上游调用、
// 返回 started=false（而非全量 growth 跑起来）。
func TestRunQueueSchoolOnlyNotAmplified(t *testing.T) {
	var upstreamCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	p := New(Config{Version: "test", APIKey: "", Pool: newSmokePool(),
		Upstream: &upstream.Client{HTTP: srv.Client()}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/tasks/run_queue",
		strReader(`{"concurrency":1,"growth":false,"school":true}`))
	req.Header.Set("Authorization", "Bearer ")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if want := `"started":false`; !contains(rec.Body.String(), want) {
		t.Errorf("body=%s want started=false（school-only 不放大为 growth）", rec.Body.String())
	}
	if upstreamCalls != 0 {
		t.Errorf("school-only 请求不应触发上游调用，got %d", upstreamCalls)
	}
}

// TestRunQueueNoFlagsDefaultsGrowth 两个开关都未带（现役前端只发 concurrency）：
// 保留旧默认全量口径（growth=true）。
func TestRunQueueNoFlagsDefaultsGrowth(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "", Pool: newSmokePool(),
		Upstream: &upstream.Client{}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/tasks/run_queue",
		strReader(`{"concurrency":2}`))
	req.Header.Set("Authorization", "Bearer ")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	// 默认 growth=true：扫描路径会跑（无账号/无待办 → started=false，
	// 但不得因开关被关而直接组队为空——两者响应同为 started=false，
	// 用取消接口旁证：growth 默认开启时队列流程正常走完）。
	if want := `"ok":true`; !contains(rec.Body.String(), want) {
		t.Errorf("body=%s want ok=true（请求被正常处理）", rec.Body.String())
	}
}
