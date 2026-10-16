package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/usage"
)

// panelHourLayout 与 usage 包桶键的时间格式一致（本地时区整点）。
// 测试里的时间窗边界必须落在整点上，与桶片起点严格重合，边界断言才不因时区
// 偏移或桶内偏移错位一档。
const panelHourLayout = "2006-01-02T15"

// panelHour 把任意时刻归一为本地整点（桶片起点 / 窗口边界）。
func panelHour(t time.Time) time.Time {
	ts, err := time.ParseInLocation(panelHourLayout, t.Format(panelHourLayout), time.Local)
	if err != nil {
		panic(err)
	}
	return ts
}

// usageWindowResp /api/usage 响应里本文件需要断言的字段（其余字段属既有契约）。
type usageWindowResp struct {
	Totals     usage.Agg        `json:"totals"`
	ByModel    []usage.KeyedAgg `json:"by_model"`
	Series     []usage.Point    `json:"series"`
	WindowFrom int64            `json:"window_from"`
	WindowTo   int64            `json:"window_to"`
}

// newUsagePanel 最小装配：空池（无上游依赖）+ 纯内存用量记录器；
// APIKey 留空 = 不鉴权（与其它单测同口径，本文件只验参数语义）。
func newUsagePanel(t *testing.T) (*Panel, *usage.Recorder) {
	t.Helper()
	rec := usage.New("")
	return New(Config{Version: "test", Pool: pool.New(""), Usage: rec}), rec
}

// getUsagePanel 发一次 GET /api/usage 并解出响应（非 200 时不解码，由调用方断言码）。
func getUsagePanel(t *testing.T, p *Panel, path string) (int, usageWindowResp) {
	t.Helper()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var out usageWindowResp
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v (body=%s)", path, err, rec.Body.String())
		}
	}
	return rec.Code, out
}

// from/to 同时给出时用显式窗口并**优先于 hours**：即使带了 hours=24，48 小时前的
// 桶只要落在窗口内就要被聚合；生效窗口在响应里回显（前端据此确认窗口被采纳）。
func TestUsageEndpointWindowOverridesHours(t *testing.T) {
	p, rec := newUsagePanel(t)
	now := panelHour(time.Now())
	old := now.Add(-48 * time.Hour)
	rec.Add(old, "cn", "u1", "glm-5.2", usage.Delta{PromptTokens: 50, HasPromptTokens: true}, true)
	rec.Add(now.Add(-time.Hour), "cn", "u1", "glm-5.2", usage.Delta{PromptTokens: 7, HasPromptTokens: true}, true)

	from, to := old.Add(-time.Hour), old.Add(time.Hour)
	code, resp := getUsagePanel(t, p, fmt.Sprintf("/api/usage?hours=24&from=%d&to=%d", from.Unix(), to.Unix()))
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if resp.WindowFrom != from.Unix() || resp.WindowTo != to.Unix() {
		t.Fatalf("生效窗口回显 = %d/%d, want %d/%d", resp.WindowFrom, resp.WindowTo, from.Unix(), to.Unix())
	}
	// 窗口内只有 48 小时前那条（小时前的桶在窗外）：hours=24 被忽略即证明优先。
	if len(resp.ByModel) != 1 || resp.ByModel[0].PromptTokens != 50 {
		t.Fatalf("by_model = %+v, want 仅窗口内 50（hours 被窗口覆盖）", resp.ByModel)
	}
	if len(resp.Series) != 1 || resp.Series[0].T != old.Format(panelHourLayout) {
		t.Fatalf("series = %+v, want 仅 %s 一个点", resp.Series, old.Format(panelHourLayout))
	}
	// totals 恒全量累计（「累计请求」口径不随窗口变）。
	if resp.Totals.PromptTokens != 57 {
		t.Fatalf("totals = %d, want 57（全量）", resp.Totals.PromptTokens)
	}
}

// 窗口需要两端同时给出：缺一端或参数非法一律按「未给窗口」处理（hours 档生效），
// 回显 0/0——前端据此知道窗口没生效，而不是把 hours 数据误当窗口数据展示。
func TestUsageEndpointWindowNeedsBothBounds(t *testing.T) {
	p, rec := newUsagePanel(t)
	now := panelHour(time.Now())
	rec.Add(now.Add(-48*time.Hour), "cn", "u1", "glm-5.2", usage.Delta{PromptTokens: 50, HasPromptTokens: true}, true)
	rec.Add(now, "cn", "u1", "glm-5.2", usage.Delta{PromptTokens: 7, HasPromptTokens: true}, true)

	cases := []struct {
		name  string
		query string
	}{
		{"仅 from", fmt.Sprintf("?hours=72&from=%d", now.Add(-72*time.Hour).Unix())},
		{"仅 to", fmt.Sprintf("?hours=72&to=%d", now.Unix())},
		{"from 非法", "?hours=72&from=abc"},
		{"to 非法（小数）", "?hours=72&to=1.5"},
		{"一侧非法", fmt.Sprintf("?hours=72&from=nope&to=%d", now.Unix())},
		{"两侧都空串", "?hours=72&from=&to="},
		{"两侧都 0（空态哨兵）", "?hours=72&from=0&to=0"},
	}
	for _, c := range cases {
		code, resp := getUsagePanel(t, p, "/api/usage"+c.query)
		if code != http.StatusOK {
			t.Fatalf("%s: code=%d", c.name, code)
		}
		if resp.WindowFrom != 0 || resp.WindowTo != 0 {
			t.Errorf("%s: 窗口未成立却回显 %d/%d", c.name, resp.WindowFrom, resp.WindowTo)
		}
		// hours=72 档：48 小时前的桶在窗内 → 两个小时点。
		if len(resp.Series) != 2 {
			t.Errorf("%s: series = %+v, want 2 个小时点（hours=72 档）", c.name, resp.Series)
		}
		if resp.Totals.PromptTokens != 57 {
			t.Errorf("%s: totals = %d, want 57", c.name, resp.Totals.PromptTokens)
		}
	}
}

// 不带任何新参数 = 既有 hours 语义逐字不变：默认 72 档按 72 小时窗口聚合，
// 且不回显窗口（0/0 = hours 档）。
func TestUsageEndpointDefaultHoursUnchanged(t *testing.T) {
	p, rec := newUsagePanel(t)
	now := panelHour(time.Now())
	rec.Add(now, "cn", "u1", "glm-5.2", usage.Delta{PromptTokens: 1, HasPromptTokens: true}, true)
	rec.Add(now.Add(-71*time.Hour), "cn", "u1", "glm-5.2", usage.Delta{PromptTokens: 2, HasPromptTokens: true}, true) // 窗内（72 个整点桶含两端）
	rec.Add(now.Add(-73*time.Hour), "cn", "u1", "glm-5.2", usage.Delta{PromptTokens: 4, HasPromptTokens: true}, true) // 窗外

	code, resp := getUsagePanel(t, p, "/api/usage")
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if resp.WindowFrom != 0 || resp.WindowTo != 0 {
		t.Fatalf("hours 档不应回显窗口: %d/%d", resp.WindowFrom, resp.WindowTo)
	}
	if len(resp.Series) != 2 {
		t.Fatalf("series = %+v, want 默认 72 档的 2 个小时点", resp.Series)
	}
	if len(resp.ByModel) != 1 || resp.ByModel[0].PromptTokens != 3 {
		t.Fatalf("by_model = %+v, want 窗口内 1+2=3", resp.ByModel)
	}
	if resp.Totals.PromptTokens != 7 {
		t.Fatalf("totals = %d, want 7（全量）", resp.Totals.PromptTokens)
	}
}
