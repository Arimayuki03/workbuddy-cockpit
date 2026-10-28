package upstream

import (
	"strings"
	"testing"
)

// 2026-10-28（manager 0036 吸收）：DSH file-reference 的完整模板句是上游
// 2026-10-27 新增指纹。单句放行、跨句连读即 400；root→base 一词之差即可绕过。
func TestWorkspaceRootRewritten(t *testing.T) {
	in := "Relative paths resolve from the workspace root; absolute paths identify files or directories on the host."
	out := sanitizeText(in)
	if strings.Contains(strings.ToLower(out), "workspace root") {
		t.Errorf("workspace root 未被改写: %q", out)
	}
	// 只替换完整短语，句首大写与后续 "; absolute paths ..." 原文保留。
	const want = "Relative paths resolve from the workspace base; absolute paths identify files or directories on the host."
	if out != want {
		t.Errorf("改写结果不符:\n 得到 %q\n 期望 %q", out, want)
	}
}

// 普通文本 / 工具描述里的 workspace root(s) 不参与上游跨句匹配，不得改写；
// 收窄后的正则只认完整 DSH 模板短语。末尾 \b 保证复数形态的完整句也不会
// 以 "workspace root" 前缀命中（否则会产出 "...workspace bases."）。
func TestWorkspaceRootOrdinaryTextNotTouched(t *testing.T) {
	for _, in := range []string{
		"Please explain the workspace root layout.",
		"The workspace roots differ between machines.",
		"Returns the workspace root directory for a project.",
		"workspace roots",
		"Relative paths resolve from the workspace roots.",
		"Relative paths resolve from the workspace roots; absolute paths identify files on the host.",
	} {
		if hasFingerprint(in) {
			t.Errorf("普通文本不应命中预检: %q", in)
		}
		if out := sanitizeText(in); out != in {
			t.Errorf("普通文本不应被改写: %q -> %q", in, out)
		}
	}
}

func TestWorkspaceRootFingerprintDetected(t *testing.T) {
	if !hasFingerprint("Relative paths resolve from the workspace root;") {
		t.Error("完整 DSH 句子未被预检识别")
	}
	if !hasFingerprint("Relative paths resolve from the WORKSPACE ROOT;") {
		t.Error("大写变体未被预检识别")
	}
}

// 身份句式的 7 个动词集合（community 实测）：只覆盖 powered by/built on 会漏拦。
func TestDshIdentityAllSevenVerbs(t *testing.T) {
	for _, verb := range []string{
		"powered by", "built on", "built by", "created by", "developed by", "made by", "trained by",
	} {
		in := "You are an AI agent " + verb + " DeepSeek Harness."
		if !hasFingerprint(in) {
			t.Errorf("%q: 预检未识别", verb)
			continue
		}
		out := sanitizeText(in)
		if strings.Contains(out, "DeepSeek Harness") {
			t.Errorf("%q: 品牌指纹残留: %q", verb, out)
		}
		if !strings.Contains(out, "You are a helpful AI assistant.") {
			t.Errorf("%q: 未替换为中性句: %q", verb, out)
		}
	}
}

// Web GUI 替换串必须是真正的改写：原实现 repl 与 pattern 逐字相同，
// ReplaceAllString 是空操作，issue #250 的跨句保护实际未生效。
func TestDshGuiPhraseActuallyRewritten(t *testing.T) {
	in := `You are interacting with the user through the DeepSeek-Harness Web GUI whose URL the runtime context names. When the user refers to "this page", "this GUI", or "this app" without naming another target, they mean this GUI.`
	out := sanitizeText(in)
	if strings.Contains(out, "DeepSeek-Harness Web GUI") {
		t.Errorf("旧短语仍残留: %q", out)
	}
	if !strings.Contains(out, "DeepSeek-Harness Web-GUI") {
		t.Errorf("未生成带连字符的替身: %q", out)
	}
	if restored := strings.ReplaceAll(out, "DeepSeek-Harness Web-GUI", "DeepSeek-Harness Web GUI"); restored != in {
		t.Errorf("段落其他内容被改动:\n 得到 %q\n 期望 %q", restored, in)
	}
	if twice := sanitizeText(out); twice != out {
		t.Errorf("非幂等: %q -> %q", out, twice)
	}
}

// 可配置词表：features.sanitize_extra_features / _rewrites 注入后立即生效，
// ConfigureSanitize 幂等（重复调用不会叠加）。
func TestConfigureSanitizeExtraTerms(t *testing.T) {
	t.Cleanup(func() { ConfigureSanitize(nil, nil) })
	in := "please mention ZCode in your answer"
	if hasFingerprint(in) {
		t.Fatal("未注入前不应命中")
	}
	ConfigureSanitize([]string{"ZCode"}, [][2]string{{"ZCode", "WorkBuddy"}})
	if !hasFingerprint(in) {
		t.Fatal("注入后应命中")
	}
	if out := sanitizeText(in); strings.Contains(out, "ZCode") || !strings.Contains(out, "WorkBuddy") {
		t.Errorf("额外改写未生效: %q", out)
	}
	ConfigureSanitize([]string{"ZCode"}, [][2]string{{"ZCode", "WorkBuddy"}})
	if out := sanitizeText(in); strings.Count(out, "WorkBuddy") != 1 {
		t.Errorf("重复注入导致叠加: %q", out)
	}
	ConfigureSanitize(nil, nil)
	if hasFingerprint(in) {
		t.Error("清空后仍命中")
	}
}

// 内置 sanitizeFeatures 必须精确匹配：小写变体不得进入净化分支。
// 否则内置 sanitizeRewrites（精确匹配）什么都不改，只剩末尾 TrimSpace，
// 普通文本的首尾空白会被意外吃掉。
func TestBuiltinFeatureExactMatchDoesNotTrim(t *testing.T) {
	in := "  main branch (lowercase prose in a user message)  "
	if hasFingerprint(in) {
		t.Fatal("内置 feature 应精确匹配，小写变体不应命中")
	}
	if out := sanitizeText(in); out != in {
		t.Errorf("小写变体被误改（TrimSpace 泄漏）: %q -> %q", in, out)
	}
	// 精确形态仍必须命中并改写。
	if !hasFingerprint("Main branch (you will usually use this for PRs)") {
		t.Error("精确形态应命中预检")
	}
}

// 运行期额外 feature 保持大小写不敏感（与内置表策略不同）。
func TestExtraFeatureCaseInsensitiveStillDetected(t *testing.T) {
	t.Cleanup(func() { ConfigureSanitize(nil, nil) })
	ConfigureSanitize([]string{"ZCode"}, nil)
	if !hasFingerprint("please mention zcode in your answer") {
		t.Error("额外 feature 的大小写变体应命中")
	}
	if !hasFingerprint("please mention ZCODE in your answer") {
		t.Error("额外 feature 的大写变体应命中")
	}
}

// 空白源串会把全文空格替换成任意 replacement，必须在 ConfigureSanitize 过滤；
// 非空白源串则做 TrimSpace 后保存。
func TestConfigureSanitizeRejectsBlankRewriteSource(t *testing.T) {
	t.Cleanup(func() { ConfigureSanitize(nil, nil) })
	ConfigureSanitize([]string{"  ZCode  "}, [][2]string{
		{" ", "X"},
		{"\t\n", "Y"},
		{"   ", "   "},
		{"  ZCode  ", "WorkBuddy"},
	})
	if len(sanitizeExtraFeatures) != 1 || sanitizeExtraFeatures[0] != "ZCode" {
		t.Fatalf("额外 feature 未按预期清洗: %v", sanitizeExtraFeatures)
	}
	if len(sanitizeExtraRewrites) != 1 {
		t.Fatalf("空白源串未被过滤: %v", sanitizeExtraRewrites)
	}
	// 源串 TrimSpace 后按字面量预编译为大小写不敏感正则（元字符已 QuoteMeta）。
	if got := sanitizeExtraRewrites[0].re.String(); got != "(?i)ZCode" {
		t.Fatalf("源串未按字面量预编译: %q", got)
	}
	if got := sanitizeExtraRewrites[0].repl; got != "WorkBuddy" {
		t.Fatalf("replacement 未保留: %q", got)
	}
	// 若空白源串漏进来，下面每个空格都会被替换成 X。
	in := "a b ZCode"
	if out := sanitizeText(in); out != "a b WorkBuddy" {
		t.Errorf("空白源串造成大范围误改: %q -> %q", in, out)
	}
}

// extra rewrite 的 replacement 可能重新引入内置指纹；内置规则最后执行，
// 必须把这类字符串收口掉。
func TestExtraRewriteReintroducedFingerprintScrubbed(t *testing.T) {
	t.Cleanup(func() { ConfigureSanitize(nil, nil) })
	ConfigureSanitize([]string{"ZCode"}, [][2]string{{"ZCode", fpCode}})
	out := sanitizeText("say ZCode")
	if strings.Contains(out, fpCode) {
		t.Errorf("extra replacement 重新引入的指纹未被内置规则收口: %q", out)
	}
	if !strings.Contains(out, fpCodeRewritten) {
		t.Errorf("内置改写未生效: %q", out)
	}
}

// sanitize 可能把字符串化 JSON 改坏（例如 extra replacement 含引号）；
// 此时必须保留原 arguments，不得把非法 JSON 写给上游。
func TestToolCallArgumentsInvalidJSONPreserved(t *testing.T) {
	t.Cleanup(func() { ConfigureSanitize(nil, nil) })
	// replacement 是单个双引号：替换后 JSON 不再合法。
	ConfigureSanitize([]string{"ZCode"}, [][2]string{{"ZCode", `"`}})
	const orig = `{"command":"echo ZCode"}`
	msgs := []any{
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{
				"name":      "Bash",
				"arguments": orig,
			}},
		}},
	}
	if sanitizeMessages(msgs) {
		t.Fatal("非法 JSON 不应报告改动")
	}
	fn := msgs[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if got := fn["arguments"].(string); got != orig {
		t.Errorf("非法 JSON 参数被写坏: got %q want %q", got, orig)
	}
}

// extra rewrite 必须大小写不敏感：只命中大小写变体时也要真正改写，
// 不能进入净化分支后只执行末尾 TrimSpace（旧实现即如此，指纹实际未改写）。
func TestExtraRewriteCaseInsensitive(t *testing.T) {
	t.Cleanup(func() { ConfigureSanitize(nil, nil) })
	ConfigureSanitize([]string{"ZCode"}, [][2]string{{"ZCode", "WorkBuddy"}})
	for _, in := range []string{"  zcode  ", "  ZCODE  ", "  ZcOdE  "} {
		if out := sanitizeText(in); out != "WorkBuddy" {
			t.Errorf("大小写变体未真正改写: %q -> %q want %q", in, out, "WorkBuddy")
		}
	}
}

// replacement 按字面量回填：$ 不得被解释为捕获组引用。
func TestExtraRewriteReplacementLiteralDollar(t *testing.T) {
	t.Cleanup(func() { ConfigureSanitize(nil, nil) })
	ConfigureSanitize([]string{"ZCode"}, [][2]string{{"ZCode", "$1 $$ ${name}"}})
	if out := sanitizeText("use ZCode"); out != "use $1 $$ ${name}" {
		t.Errorf("replacement 的 $ 被解释: %q", out)
	}
}

// 只配 sanitize_extra_rewrites、不配 features 时，rewrite 源串自动纳入预检，
// 改写必须生效（否则运维只填 rewrites 会静默空转）。
func TestConfigureSanitizeRewritesOnly(t *testing.T) {
	t.Cleanup(func() { ConfigureSanitize(nil, nil) })
	ConfigureSanitize(nil, [][2]string{{"Solo", "X"}})
	if !hasFingerprint("solo") {
		t.Fatal("rewrite 源串未自动纳入预检")
	}
	if out := sanitizeText("  solo  "); out != "X" {
		t.Errorf("rewrites-only 未生效: %q", out)
	}
}

// JSON 单元素 pair（[["foo"]]）会被解码成 {"foo",""}：空 replacement 等于删除
// 全文所有 foo，属于大范围误改。ConfigureSanitize 必须过滤该条并保持原文本。
func TestConfigureSanitizeRejectsEmptyReplacement(t *testing.T) {
	t.Cleanup(func() { ConfigureSanitize(nil, nil) })
	ConfigureSanitize([]string{"foo"}, [][2]string{{"foo"}})
	if len(sanitizeExtraRewrites) != 0 {
		t.Fatalf("空 replacement 条目未被过滤: %v", sanitizeExtraRewrites)
	}
	const in = "foo bar foo; keep the text"
	if out := sanitizeText(in); out != in {
		t.Errorf("空 replacement 造成文本被删除: %q -> %q", in, out)
	}
	// 同一批里的合法条目仍应生效（只丢弃空 replacement 那一条）。
	ConfigureSanitize([]string{"foo"}, [][2]string{{"foo"}, {"foo", "bar"}})
	if len(sanitizeExtraRewrites) != 1 {
		t.Fatalf("合法条目被连带丢弃: %v", sanitizeExtraRewrites)
	}
	if out := sanitizeText("foo"); out != "bar" {
		t.Errorf("合法 extra rewrite 未生效: %q", out)
	}
}
