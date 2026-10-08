// keyexport_test.go 密钥导出渲染测试：每种格式一份 golden（断言关键行存在：
// base_url 口径正确、密钥明文原样落位、模型 id 注入位置），外加 URL 归一化
// 边界（带 /v1、带尾斜杠、裸域名、IPv4:port）与参数校验。渲染是纯函数，
// 不需要 Panel 实例。
package panel

import (
	"encoding/json"
	"strings"
	"testing"
)

// testExportArgs 测试用的固定参数：URL 故意带尾斜杠 + /v1（最常见的「从 OpenAI
// 口径复制过来」的输入），验证两条 URL 链路各自归一正确。
const (
	testExportBase = "http://192.168.1.10:8787/v1/"
	testExportKey  = "wbk_AAAAAAAABBBBBBBBCCCCCCCCDDDDDDDD"
	testExportMdl  = "cn:glm-5.2"
)

func TestRenderKeyExportAllFormatsGolden(t *testing.T) {
	if len(ExportFormats()) != 9 {
		t.Fatalf("ExportFormats() 数量 = %d, want 9（新增格式时补齐本表）", len(ExportFormats()))
	}
	for _, format := range ExportFormats() {
		filename, content, err := RenderKeyExport(testExportBase, testExportKey, testExportMdl, format)
		if err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if filename == "" || content == "" {
			t.Fatalf("%s: 文件名或内容为空", format)
		}
		// 通用不变量：明文密钥必须原样出现在片段里（curl 片段放 Bearer 头，
		// 其余在引号字符串里）。唯一例外 codex-toml：~/.codex/config.toml 本就
		// 不含密钥（key 走 OPENAI_API_KEY 环境变量 / keyrings），片段只写
		// base_url / model——密钥原样性由 cc-switch-codex（auth 字段）覆盖。
		if format != FormatCodexTOML && !strings.Contains(content, testExportKey) {
			t.Errorf("%s: 片段缺少明文密钥", format)
		}
		if !strings.Contains(content, testExportMdl) {
			t.Errorf("%s: 片段缺少模型 id %q", format, testExportMdl)
		}
	}
}

// renderFor 要求格式渲染成功并返回内容（各专项测试的取内容捷径）。
func renderFor(t *testing.T, format ExportFormat) string {
	t.Helper()
	_, content, err := RenderKeyExport(testExportBase, testExportKey, testExportMdl, format)
	if err != nil {
		t.Fatalf("%s: %v", format, err)
	}
	return content
}

// 期望的两种 URL 口径：claude 系根地址 + 尾斜杠；OpenAI 系带 /v1 无尾斜杠。
const (
	wantAnthropicURL = "http://192.168.1.10:8787/"
	wantOpenAIURL    = "http://192.168.1.10:8787/v1"
)

// TestRenderKeyExportClaudeURLGolden claude 系：base url 必须是根地址（剥掉
// /v1、去尾斜杠再补一个）。这是参考实现实测 405 事故的回归断言——URL 错了
// 其余全错，所以每种 claude 形态都查。
func TestRenderKeyExportClaudeURLGolden(t *testing.T) {
	for _, format := range []ExportFormat{FormatCCSwitchClaude, FormatClaudeCodeSettings, FormatClaudeCodeEnv} {
		content := renderFor(t, format)
		if n := strings.Count(content, wantAnthropicURL); n == 0 {
			t.Errorf("%s: 未找到 Anthropic 口径根地址 %q", format, wantAnthropicURL)
		}
		// /v1 不得以 base url 身份出现（v1/v1 双拼事故）。片段里允许出现在
		// 注释解释文字中，但绝不能出现 "…8787/v1" 这种拼在主机后的形态。
		if strings.Contains(content, "8787/v1") {
			t.Errorf("%s: base url 疑似带了 /v1（SDK 会拼成 /v1/v1/messages）", format)
		}
		// 三档模型 + 主模型都必须注入。
		for _, field := range []string{
			"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
			"ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL",
		} {
			if !strings.Contains(content, field) {
				t.Errorf("%s: 缺少 %s", format, field)
			}
		}
	}
}

// TestRenderKeyExportCCSwitchClaudeJSON cc-switch claude 片段须是合法 JSON 且
// env.ANTHROPIC_BASE_URL 为根地址。
func TestRenderKeyExportCCSwitchClaudeJSON(t *testing.T) {
	content := renderFor(t, FormatCCSwitchClaude)
	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("片段不是合法 JSON: %v\n%s", err, content)
	}
	if got := doc.Env["ANTHROPIC_BASE_URL"]; got != wantAnthropicURL {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want %q（根地址，SDK 自己拼 /v1/messages）", got, wantAnthropicURL)
	}
	if got := doc.Env["ANTHROPIC_AUTH_TOKEN"]; got != testExportKey {
		t.Errorf("ANTHROPIC_AUTH_TOKEN = %q, want 明文原样", got)
	}
	if got := doc.Env["ANTHROPIC_MODEL"]; got != testExportMdl {
		t.Errorf("ANTHROPIC_MODEL = %q, want %q", got, testExportMdl)
	}
}

// TestRenderKeyExportClaudeCodeSettings 与 cc-switch claude 同内容（env 块）。
func TestRenderKeyExportClaudeCodeSettings(t *testing.T) {
	content := renderFor(t, FormatClaudeCodeSettings)
	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("片段不是合法 JSON: %v\n%s", err, content)
	}
	if got := doc.Env["ANTHROPIC_BASE_URL"]; got != wantAnthropicURL {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want %q", got, wantAnthropicURL)
	}
}

// TestRenderKeyExportClaudeCodeEnv 环境变量形态：export 行 + PowerShell 提示。
func TestRenderKeyExportClaudeCodeEnv(t *testing.T) {
	content := renderFor(t, FormatClaudeCodeEnv)
	for _, want := range []string{
		`export ANTHROPIC_BASE_URL="` + wantAnthropicURL + `"`,
		`export ANTHROPIC_AUTH_TOKEN="` + testExportKey + `"`,
		`export ANTHROPIC_MODEL="` + testExportMdl + `"`,
		"anthropic-version", // 版本头说明：SDK 自动携带，无需手动设置
	} {
		if !strings.Contains(content, want) {
			t.Errorf("片段缺少 %q", want)
		}
	}
}

// TestRenderKeyExportCodexTOML codex 片段：base_url 带 /v1、model 注入在
// model = 行、wire_api 与本仓库 /v1/responses 路由一致。
func TestRenderKeyExportCodexTOML(t *testing.T) {
	content := renderFor(t, FormatCodexTOML)
	for _, want := range []string{
		`model = "` + testExportMdl + `"`,
		`base_url = "` + wantOpenAIURL + `"`,
		`model_provider = "workbuddy"`,
		`[model_providers.workbuddy]`,
		`wire_api = "responses"`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("TOML 片段缺少 %q", want)
		}
	}
}

// TestRenderKeyExportCCSwitchCodex cc-switch codex：JSON 里 auth.OPENAI_API_KEY
// 原样、config 内嵌 TOML 里的双引号已按 JSON 规则转义、base_url 带 /v1。
func TestRenderKeyExportCCSwitchCodex(t *testing.T) {
	content := renderFor(t, FormatCCSwitchCodex)
	var doc ccSwitchCodexDoc
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("片段不是合法 JSON: %v\n%s", err, content)
	}
	if doc.Auth.OpenAIKey != testExportKey {
		t.Errorf("auth.OPENAI_API_KEY = %q, want 明文原样", doc.Auth.OpenAIKey)
	}
	for _, want := range []string{
		`base_url = "` + wantOpenAIURL + `"`,
		`model = "` + testExportMdl + `"`,
		`wire_api = "responses"`,
	} {
		if !strings.Contains(doc.Config, want) {
			t.Errorf("内嵌 TOML 缺少 %q；全文：\n%s", want, doc.Config)
		}
	}
}

// TestRenderKeyExportZCode ZCode 片段：合法 JSON、api.type 与本仓库路由一致、
// baseUrl 带 /v1、personalModelIds 注入网关口径模型 id。
func TestRenderKeyExportZCode(t *testing.T) {
	content := renderFor(t, FormatZCode)
	var doc struct {
		ProviderRule struct {
			ProviderID   string `json:"providerId"`
			ProviderName string `json:"providerName"`
			Config       struct {
				Access struct {
					Type   string `json:"type"`
					APIKey string `json:"apiKey"`
				} `json:"access"`
				API struct {
					Type    string `json:"type"`
					BaseURL string `json:"baseUrl"`
				} `json:"api"`
				PersonalModelIDs []string `json:"personalModelIds"`
			} `json:"config"`
		} `json:"providerRule"`
	}
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("片段不是合法 JSON: %v\n%s", err, content)
	}
	rule := doc.ProviderRule
	if rule.ProviderID != exportProviderID {
		t.Errorf("providerId = %q, want %q（合并键）", rule.ProviderID, exportProviderID)
	}
	if rule.Config.Access.APIKey != testExportKey {
		t.Errorf("access.apiKey = %q, want 明文原样", rule.Config.Access.APIKey)
	}
	if got := rule.Config.API.BaseURL; got != wantOpenAIURL {
		t.Errorf("api.baseUrl = %q, want %q", got, wantOpenAIURL)
	}
	if got := rule.Config.API.Type; got != "openai-responses" {
		t.Errorf("api.type = %q, want openai-responses（本仓库路由 /v1/responses）", got)
	}
	if len(rule.Config.PersonalModelIDs) != 1 || rule.Config.PersonalModelIDs[0] != testExportMdl {
		t.Errorf("personalModelIds = %v, want [%q]", rule.Config.PersonalModelIDs, testExportMdl)
	}
}

// TestRenderKeyExportOpenAISnippets 通用 OpenAI SDK 三份示例：base_url/baseURL
// 带 /v1、密钥与模型落位。
func TestRenderKeyExportOpenAISnippets(t *testing.T) {
	py := renderFor(t, FormatOpenAIPython)
	for _, want := range []string{
		`base_url="` + wantOpenAIURL + `"`,
		`api_key="` + testExportKey + `"`,
		`model="` + testExportMdl + `"`,
	} {
		if !strings.Contains(py, want) {
			t.Errorf("python 片段缺少 %q", want)
		}
	}
	node := renderFor(t, FormatOpenAINode)
	for _, want := range []string{
		`baseURL: "` + wantOpenAIURL + `"`,
		`apiKey: "` + testExportKey + `"`,
		`model: "` + testExportMdl + `"`,
	} {
		if !strings.Contains(node, want) {
			t.Errorf("node 片段缺少 %q", want)
		}
	}
	curl := renderFor(t, FormatOpenAICurl)
	for _, want := range []string{
		wantOpenAIURL + "/chat/completions",
		`Authorization: Bearer ` + testExportKey,
		`"model": "` + testExportMdl + `"`,
	} {
		if !strings.Contains(curl, want) {
			t.Errorf("curl 片段缺少 %q", want)
		}
	}
}

// TestRenderKeyExportFilename 每种格式给出下载文件名（扩展名随内容形态）。
func TestRenderKeyExportFilename(t *testing.T) {
	wantExt := map[ExportFormat]string{
		FormatCCSwitchClaude:     ".json",
		FormatCCSwitchCodex:      ".json",
		FormatClaudeCodeSettings: ".json",
		FormatClaudeCodeEnv:      ".env",
		FormatCodexTOML:          ".toml",
		FormatZCode:              ".json",
		FormatOpenAIPython:       ".py",
		FormatOpenAINode:         ".mjs",
		FormatOpenAICurl:         ".sh",
	}
	for format, ext := range wantExt {
		filename, _, err := RenderKeyExport(testExportBase, testExportKey, testExportMdl, format)
		if err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if !strings.HasSuffix(filename, ext) {
			t.Errorf("%s: filename = %q, 想要后缀 %s", format, filename, ext)
		}
	}
}

// TestOpenAIBaseURLNormalization OpenAI 口径：追加 /v1、已带 /v1 不重复、
// 去尾斜杠；裸域名 / IPv4:port / 反代子路径都落到 …/v1 收口。
func TestOpenAIBaseURLNormalization(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://192.168.1.10:8787", "http://192.168.1.10:8787/v1"},
		{"http://192.168.1.10:8787/", "http://192.168.1.10:8787/v1"},
		{"http://192.168.1.10:8787//", "http://192.168.1.10:8787/v1"},
		{"http://192.168.1.10:8787/v1", "http://192.168.1.10:8787/v1"},
		{"http://192.168.1.10:8787/v1/", "http://192.168.1.10:8787/v1"},
		{"http://panel.example.com", "http://panel.example.com/v1"},
		{"https://panel.example.com/", "https://panel.example.com/v1"},
		{"https://panel.example.com/v1", "https://panel.example.com/v1"},
		// 反代子路径：归一只关心结尾的 /v1，子路径原样保留。
		{"https://example.com/wb", "https://example.com/wb/v1"},
		{"https://example.com/wb/", "https://example.com/wb/v1"},
		{"https://example.com/wb/v1", "https://example.com/wb/v1"},
		// 两侧空白容忍（表单粘贴常见）。
		{"  http://a.example.com  ", "http://a.example.com/v1"},
	}
	for _, c := range cases {
		got, err := openAIBaseURL(c.in)
		if err != nil {
			t.Errorf("openAIBaseURL(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("openAIBaseURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if _, err := openAIBaseURL("   "); err == nil {
		t.Error("空白地址应报错")
	}
	if _, err := openAIBaseURL("///"); err == nil {
		t.Error("纯斜杠应报错（去尾斜杠后为空）")
	}
}

// TestAnthropicBaseURLNormalization Anthropic 口径：剥 /v1 + 去尾斜杠 + 补一个
// 尾斜杠。/v1/v1 双拼与 hostv1（少斜杠直拼）两类事故都在这里拦。
func TestAnthropicBaseURLNormalization(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://192.168.1.10:8787", "http://192.168.1.10:8787/"},
		{"http://192.168.1.10:8787/", "http://192.168.1.10:8787/"},
		{"http://192.168.1.10:8787/v1", "http://192.168.1.10:8787/"},
		{"http://192.168.1.10:8787/v1/", "http://192.168.1.10:8787/"},
		{"http://192.168.1.10:8787//", "http://192.168.1.10:8787/"},
		{"https://panel.example.com", "https://panel.example.com/"},
		{"https://panel.example.com/v1", "https://panel.example.com/"},
		{"https://example.com/wb/v1", "https://example.com/wb/"},
		{"  http://a.example.com  ", "http://a.example.com/"},
	}
	for _, c := range cases {
		got, err := anthropicBaseURL(c.in)
		if err != nil {
			t.Errorf("anthropicBaseURL(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("anthropicBaseURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if _, err := anthropicBaseURL(""); err == nil {
		t.Error("空地址应报错")
	}
}

// TestRenderKeyExportValidation 参数校验：空参数与未知格式的错误文案要可照做。
func TestRenderKeyExportValidation(t *testing.T) {
	if _, _, err := RenderKeyExport("", testExportKey, testExportMdl, FormatZCode); err == nil ||
		!strings.Contains(err.Error(), "面板对外地址") {
		t.Errorf("空 baseURL 报错 = %v", err)
	}
	if _, _, err := RenderKeyExport(testExportBase, "", testExportMdl, FormatZCode); err == nil ||
		!strings.Contains(err.Error(), "明文") {
		t.Errorf("空 plainKey 报错 = %v", err)
	}
	if _, _, err := RenderKeyExport(testExportBase, testExportKey, "", FormatZCode); err == nil ||
		!strings.Contains(err.Error(), "模型") {
		t.Errorf("空 model 报错 = %v", err)
	}
	_, _, err := RenderKeyExport(testExportBase, testExportKey, testExportMdl, "nope")
	if err == nil || !strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), "zcode") {
		t.Errorf("未知格式报错应含格式名与可选清单，got %v", err)
	}
}

// TestDQQuote 防御性转义：模型名 / 名称里出现反斜杠或双引号时不破坏 TOML/JSON。
func TestDQQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{`plain`, `"plain"`},
		{`a"b`, `"a\"b"`},
		{`a\b`, `"a\\b"`},
		{`a\"b`, `"a\\\"b"`},
	}
	for _, c := range cases {
		if got := dqQuote(c.in); got != c.want {
			t.Errorf("dqQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
