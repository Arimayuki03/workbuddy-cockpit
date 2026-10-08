// global_models_routes_test.go 钉住 /v3/config 三 UA 探测路的合并与容错口径
// （吸收 panel commit 7b24e228：桌面端 UA 主路 + IDE + CLI，gpt-6-sol 等新模型
// 只出现在桌面端 UA 的探测响应里），以及 nonChatModel 生成类标签过滤。
package upstream

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestMergeV3RoutesPriority 主路字段权威：同 id 在三路都出现时，字段取自主路
// （桌面端 UA），后续路不得覆盖其窗口/effort/credits。
func TestMergeV3RoutesPriority(t *testing.T) {
	mk := func(id string, ctx, out int64, credits string, efforts []string) ModelInfo {
		return ModelInfo{ID: id, ContextWindow: ctx, MaxTokens: out, Credits: credits, Efforts: efforts}
	}
	desktop := probeResult{
		label: "desktop-UA",
		names: []string{"shared", "only-desktop"},
		infos: []ModelInfo{
			mk("shared", 1000000, 128000, "x1.33", []string{"low", "high"}),
			mk("only-desktop", 500000, 64000, "x0.10", []string{"medium"}),
		},
	}
	ide := probeResult{
		label: "IDE-UA",
		names: []string{"shared", "only-ide"},
		infos: []ModelInfo{
			// 同 id 但字段不同：不得覆盖主路。
			mk("shared", 176000, 24000, "x9.99", []string{"max"}),
			mk("only-ide", 200000, 32000, "x0.20", nil),
		},
	}
	cli := probeResult{
		label: "CLI-UA",
		names: []string{"shared", "only-cli"},
		infos: []ModelInfo{
			mk("shared", 272000, 72000, "x8.88", []string{"xhigh"}),
			mk("only-cli", 256000, 32000, "x0.30", nil),
		},
	}
	got := mergeV3Routes(desktop, ide, cli)
	if got.err != nil {
		t.Fatalf("unexpected err: %v", got.err)
	}
	wantIDs := []string{"shared", "only-desktop", "only-ide", "only-cli"}
	if len(got.names) != len(wantIDs) {
		t.Fatalf("names = %v, want %v", got.names, wantIDs)
	}
	for i, id := range wantIDs {
		if got.names[i] != id {
			t.Errorf("names[%d] = %q, want %q (full %v)", i, got.names[i], id, got.names)
		}
	}
	// 主路字段权威：shared 的窗口/credits/efforts 必须是主路值。
	var shared *ModelInfo
	for i := range got.infos {
		if got.infos[i].ID == "shared" {
			shared = &got.infos[i]
		}
	}
	if shared == nil {
		t.Fatal("shared model missing from infos")
	}
	if shared.ContextWindow != 1000000 || shared.MaxTokens != 128000 {
		t.Errorf("shared window = %d/%d, want 1000000/128000 (primary route must win)", shared.ContextWindow, shared.MaxTokens)
	}
	if shared.Credits != "x1.33" {
		t.Errorf("shared credits = %q, want x1.33", shared.Credits)
	}
	if len(shared.Efforts) != 2 || shared.Efforts[0] != "low" {
		t.Errorf("shared efforts = %v, want [low high]", shared.Efforts)
	}
}

// TestMergeV3RoutesPartialFailure 逐路容错：单路失败不拖垮整次探测（第三路失败
// 不影响已有两路——失败路 warn 跳过，成功路并集照常产出）；全路失败才返回 err。
func TestMergeV3RoutesPartialFailure(t *testing.T) {
	mk := func(id string) ModelInfo { return ModelInfo{ID: id} }
	ok := probeResult{names: []string{"a"}, infos: []ModelInfo{mk("a")}}
	bad := probeResult{err: errors.New("v3/config status 500")}

	got := mergeV3Routes(
		probeResult{label: "desktop-UA", err: bad.err},
		probeResult{label: "IDE-UA", names: ok.names, infos: ok.infos},
		probeResult{label: "CLI-UA", err: bad.err},
	)
	if got.err != nil {
		t.Fatalf("partial failure should not error, got %v", got.err)
	}
	if len(got.names) != 1 || got.names[0] != "a" {
		t.Errorf("names = %v, want [a]", got.names)
	}

	// 全路失败 → err（取首路错误，供调用方降级到企业端点）。
	all := mergeV3Routes(
		probeResult{label: "desktop-UA", err: bad.err},
		probeResult{label: "IDE-UA", err: errors.New("second")},
		probeResult{label: "CLI-UA", err: errors.New("third")},
	)
	if all.err == nil {
		t.Fatal("all routes failed: want err, got nil")
	}
}

// TestNonChatModelGenerationTags 生成类（图片/视频）模型不得进对话目录。
// 回归用例：nonChatModel 早期只拦 text-to-image，桌面端 UA 目录带入的
// text-to-video / image-to-video（seedance 系列）会漏过并混进 /v1/models
// （选中后报 code=11102）。
func TestNonChatModelGenerationTags(t *testing.T) {
	cases := []struct {
		name string
		id   string
		out  int64
		tags []string
		want bool
	}{
		{"图片生成（既有规则）", "hunyuan-image-alpha", 0, []string{"text-to-image"}, true},
		{"图生图", "gpt-image-2.5-sunburst", 0, []string{"text-to-image", "image-to-image"}, true},
		{"文生视频（本次补齐）", "seedance-2.5", 0, []string{"text-to-video", "image-to-video"}, true},
		{"图生视频（本次补齐）", "seedance-2.5-pro", 1024, []string{"image-to-video"}, true},
		{"普通对话模型", "gpt-6-sol", 128000, nil, false},
		{"新模型无 tags", "grok-4.7", 128000, []string{}, false},
		{"对话模型带无关 tag", "balanced-model", 32000, []string{"craft"}, false},
		{"tiny 输出仍剔除", "completion-1.0", 256, nil, true},
		{"nes 前缀仍剔除", "nes-1.2", 8192, nil, true},
	}
	for _, c := range cases {
		if got := nonChatModel(c.id, c.out, c.tags); got != c.want {
			t.Errorf("%s: nonChatModel(%q, %d, %v) = %v, want %v", c.name, c.id, c.out, c.tags, got, c.want)
		}
	}
}

// TestDesktopUAProbeRouteForGlobalAccount 桌面端 UA 探测主路按 realm 切平台段：
// global 账号的 /v3/config 探测必须带 `WorkBuddy AI` 平台段 + CLI 段的桌面端
// 三段式 UA（送错平台段会触发上游 403 code=11140 风控），且**不受**用户显式
// UserAgent 配置影响（目录探测与官方客户端对齐，chat 出站 UA 的自定义覆盖与此无关）。
func TestDesktopUAProbeRouteForGlobalAccount(t *testing.T) {
	var mu sync.Mutex
	uas := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v3/config") {
			mu.Lock()
			uas[r.Header.Get("User-Agent")] = true
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"gpt-6-sol","name":"GPT-6-Sol"}]}}`))
	}))
	defer srv.Close()

	c := globalModelsClient(t, srv)
	// 用户显式配置自定义 UA：探测主路仍用桌面端默认形态（HotFields 快照与字段同步）。
	c.UserAgent = "MyAgent/9.9 custom"
	c.SyncHot()

	if got := c.FetchGlobalModels(globalAcct()); len(got) != 1 || got[0] != "gpt-6-sol" {
		t.Fatalf("FetchGlobalModels = %v, want [gpt-6-sol]", got)
	}
	mu.Lock()
	defer mu.Unlock()
	var hasDesktop bool
	for ua := range uas {
		if strings.Contains(ua, "WorkBuddy AI/") && strings.Contains(ua, "CLI/") {
			hasDesktop = true
		}
		if strings.Contains(ua, "MyAgent") {
			t.Errorf("probe UA = %q must ignore user override", ua)
		}
	}
	if !hasDesktop {
		t.Errorf("desktop probe UA (WorkBuddy AI platform + CLI segment) not observed, got %v", uas)
	}
}

// TestDesktopUAProbeRouteOnlyUniqueModels 桌面端主路独有模型进并集：
// 只有桌面端 UA 探测响应里下发的 gpt-6-sol（IDE/CLI 路均不下发）必须出现在
// FetchGlobalModels 结果里（commit 7b24e228 的核心回归判据）。三路 UA 靠
// X-Probe-Route 回显区分（探测请求不打该头，服务器按已见 UA 顺序判定：
// 首个到达者不一定是主路，故按 UA 形态而非到达序分发）。
func TestDesktopUAProbeRouteOnlyUniqueModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/v3/config") {
			ua := r.Header.Get("User-Agent")
			switch {
			case strings.Contains(ua, "CodeBuddyIDE"):
				// IDE 路：无 gpt-6-sol。
				_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"o4-mini","name":"o4-mini"}]}}`))
			case strings.HasPrefix(ua, "WorkBuddy/"):
				// 桌面端主路（三段式 WorkBuddy 形态）：唯一下发 gpt-6-sol。
				_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"gpt-6-sol","name":"GPT-6-Sol"}]}}`))
			default:
				// CLI 路（空 UA 覆盖 = CommonHeaders 默认）：测试客户端无账号
				// 版本段回填，UA 串形态不定——同样不下发 gpt-6-sol。
				_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"deepseek-v4.1-flash","name":"DS"}]}}`))
			}
			return
		}
		// 企业端点家族同样不下发 gpt-6-sol（404 触发 /console fallback，一并 404）。
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"code":404,"msg":"nope"}`))
	}))
	defer srv.Close()

	got := globalModelsClient(t, srv).FetchGlobalModels(globalAcct())
	counts := map[string]int{}
	for _, id := range got {
		counts[id]++
	}
	for _, id := range []string{"gpt-6-sol", "o4-mini", "deepseek-v4.1-flash"} {
		if counts[id] != 1 {
			t.Errorf("model %s count=%d want 1 (three-UA union, deduped): %v", id, counts[id], got)
		}
	}
}
