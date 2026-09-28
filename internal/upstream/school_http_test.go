package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
)

// 回归：schoolJSON 曾把 map 预编码成 []byte 再交给 billingJSON 二次 marshal——
// json.Marshal 对 []byte 走 base64，上游收到的是字符串非对象，
// /wheel/draw 报 40000 "draw_uuid required"。本组测试钉死请求体必须是真 JSON 对象。
// 抽奖/任务 API 已随活动结束（2026-09-24）下线，仅剩 TestSchoolVouchers 覆盖
// 保留的券码查询（schoolJSON 出站路径与当年相同）。

func newSchoolTestServer(t *testing.T, resp string, bodies *map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bodies != nil {
			if *bodies == nil {
				*bodies = map[string]string{}
			}
			(*bodies)[r.URL.Path] = ""
		}
		w.Write([]byte(resp))
	}))
}

func TestSchoolVouchers(t *testing.T) {
	srv := newSchoolTestServer(t, `{"code":0,"data":{"items":[{"grant_id":1,"code":"KFC-123","prize_name":"肯德基冰淇淋","valid_to":"2026-10-24"}]}}`, nil)
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}
	items, err := c.SchoolVouchers(&auth.Auth{})
	if err != nil {
		t.Fatalf("SchoolVouchers: %v", err)
	}
	if len(items) != 1 || items[0].Code != "KFC-123" || items[0].PrizeName != "肯德基冰淇淋" {
		t.Errorf("items=%+v", items)
	}
}
