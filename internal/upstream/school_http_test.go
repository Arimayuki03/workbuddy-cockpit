package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"workbuddy2api/internal/auth"
)

// 回归：schoolJSON 曾把 map 预编码成 []byte 再交给 billingJSON 二次 marshal——
// json.Marshal 对 []byte 走 base64，上游收到的是字符串非对象，
// /wheel/draw 报 40000 "draw_uuid required"。本组测试钉死请求体必须是真 JSON 对象。

func newSchoolTestServer(t *testing.T, resp string, bodies *map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if bodies != nil {
			if *bodies == nil {
				*bodies = map[string]string{}
			}
			(*bodies)[r.URL.Path] = string(b)
		}
		w.Write([]byte(resp))
	}))
}

func TestSchoolDrawBodyIsJSONObject(t *testing.T) {
	srv := newSchoolTestServer(t, `{"code":0,"data":{"prize_code":"credit_50","credit_amount":50}}`, nil)
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}
	prize, err := c.SchoolDraw(&auth.Auth{})
	if err != nil {
		t.Fatalf("SchoolDraw: %v", err)
	}
	if prize != "credit_50 +50c" {
		t.Errorf("prize=%q", prize)
	}
}

func TestSchoolDrawUUIDIsV4(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}
	if _, err := c.SchoolDraw(&auth.Auth{}); err != nil {
		t.Fatalf("SchoolDraw: %v", err)
	}

	if len(got) > 0 && got[0] == '"' {
		t.Fatalf("body is a string (base64 regression): %s", got)
	}
	var body struct {
		DrawUUID string `json:"draw_uuid"`
	}
	if err := json.Unmarshal([]byte(got), &body); err != nil {
		t.Fatalf("body not a JSON object: %s", got)
	}
	// 标准 uuid v4：8-4-4-4-12 带横线 + 版本位 4 + 变体位 [89ab]。
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !re.MatchString(body.DrawUUID) {
		t.Errorf("draw_uuid not uuid v4: %q", body.DrawUUID)
	}
}

func TestSchoolShareCompleteBodyIsJSONObject(t *testing.T) {
	bodies := map[string]string{}
	srv := newSchoolTestServer(t, `{"code":0,"data":{}}`, &bodies)
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}
	if err := c.SchoolShareComplete(&auth.Auth{}); err != nil {
		t.Fatalf("SchoolShareComplete: %v", err)
	}
	got := bodies["/portal/activity/school/tasks/share-complete"]
	if got != `{"channel":"wechat"}` {
		t.Errorf("share-complete body = %q, want JSON object with channel", got)
	}
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
