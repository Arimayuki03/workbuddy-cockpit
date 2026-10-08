// keyexport.go 密钥一键导出：把一把网关密钥渲染成各客户端「填好 base_url +
// api_key 的配置片段」（移植自 workbuddy-manager 的 server/services/keyexport.py，
// 覆盖其全部导出目标，并按任务补充 Claude Code 直连形态与通用 OpenAI SDK 示例）。
//
// **为什么只能在创建时导出**：网关密钥只存 SHA-256 摘要 + 展示前缀（与
// tokens.go 的 wbt_ 管理令牌同一套约定），列表行里的前缀拼不出完整密钥。因此
// 渲染入口只接受调用方当场传进来的明文（RenderKeyExport 的 plainKey 参数），
// 本文件不读 Store、不查库——拿不到明文时函数无法凭空造出配置，这一点由
// 签名强制。实际交互应是「创建成功、明文一次性展示的弹窗里直接提供各格式
// 片段下载/复制」。
//
// **纯函数、无副作用**：只做「参数 → 片段文本」的转换，不写文件、不碰
// Store、不发网络；端点接线（GET /api/keys/{id}/export）由 panel 路由另做。
//
// **两种 base url 口径，不能混用**（同一个面板地址，两类客户端要的值不同）：
//
//	| 客户端                          | 需要的值                 | 原因                             |
//	|---------------------------------|--------------------------|----------------------------------|
//	| claude 系（Claude Code /        | http://host（根，不带    | Anthropic SDK 自己在 base url    |
//	|   cc-switch 的 claude）         | /v1，保留尾斜杠）        | 后拼 /v1/messages                |
//	| codex / ZCode / OpenAI 系       | http://host/v1           | 客户端在 base url 后拼路径       |
//
// 给 claude 填 …/v1 是最容易犯的错：实际请求变成 …/v1/v1/messages。参考实现
// 实测：面板上那个路径「存在但不是 POST 路由」时返回 405 而非 404，报错看起来
// 像"方法不对"，很容易把人引到错误方向去查。见 anthropicBaseURL。
//
// **与参考实现的口径差异（本仓库事实，勿照抄参考注释）**：参考实现对接的上游
// 是 OpenAI Responses 形态（codex 用 wire_api="responses"、ZCode 用
// api.type="openai-responses"）；本仓库网关现在同样是三协议齐全——
// POST /v1/chat/completions（internal/server/handler.go）、POST /v1/responses 与
// /responses（internal/server/responses.go，main.go 调 MountResponses 挂载）、
// POST /v1/messages（internal/server/anthropic.go）——codex 片段取
// wire_api="responses"（Responses 生态客户端优先走 Responses 协议）、ZCode 片段
// 取 "openai-responses"（ZCode 内置供应商里 OpenAI / x.ai 等 Responses 形态
// 即此取值，baseUrl 同为带 /v1 口径），与参考实现同源。claude 系由网关的
// /v1/messages（Anthropic 形态）路由决定可用性，本文件只保证 URL 口径正确。
package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ExportFormat 导出格式标识（可直接用作查询参数值，如 ?format=zcode）。
type ExportFormat string

// 全部导出格式（与 exportSpecs 一一对应，顺序即建议的前端展示顺序）：
//   - cc-switch 两份 settings_config 片段（claude 走 env.ANTHROPIC_*，codex 走
//     auth.OPENAI_API_KEY + 内嵌 TOML——结构照抄 cc-switch 真实数据）；
//   - Claude Code 直连两种形态：settings.json 的 env 块 / shell 环境变量；
//   - Codex CLI 的 ~/.codex/config.toml 片段；
//   - ZCode 的 provider_config.json 片段；
//   - 通用 OpenAI SDK（python / node）与 curl 示例。
const (
	FormatCCSwitchClaude     ExportFormat = "cc-switch-claude"
	FormatCCSwitchCodex      ExportFormat = "cc-switch-codex"
	FormatClaudeCodeSettings ExportFormat = "claude-code-settings"
	FormatClaudeCodeEnv      ExportFormat = "claude-code-env"
	FormatCodexTOML          ExportFormat = "codex-toml"
	FormatZCode              ExportFormat = "zcode"
	FormatOpenAIPython       ExportFormat = "openai-python"
	FormatOpenAINode         ExportFormat = "openai-node"
	FormatOpenAICurl         ExportFormat = "openai-curl"
)

// exportSpecs 格式 → 下载文件名 / 一句话说明（说明供错误信息与前端展示复用）。
var exportSpecs = []struct {
	Format   ExportFormat
	Filename string
	Desc     string
}{
	{FormatCCSwitchClaude, "cc-switch-claude.json",
		"cc-switch 供应商片段（Claude Code）：并入 providers.settings_config（JSON）"},
	{FormatCCSwitchCodex, "cc-switch-codex.json",
		"cc-switch 供应商片段（Codex）：并入 providers.settings_config（auth + 内嵌 TOML）"},
	{FormatClaudeCodeSettings, "claude-code-settings.json",
		"Claude Code settings.json 的 env 块片段（JSON）"},
	{FormatClaudeCodeEnv, "claude-code.env",
		"Claude Code 环境变量片段（shell，可追加到 ~/.bashrc / ~/.zshrc）"},
	{FormatCodexTOML, "codex-config.toml",
		"Codex CLI config.toml 片段（追加到 ~/.codex/config.toml）"},
	{FormatZCode, "zcode-provider.json",
		"ZCode 供应商片段（并入 ~/.zcode/v2/provider_config.json，合并键为 providerId）"},
	{FormatOpenAIPython, "openai-python.py", "OpenAI SDK（Python）调用示例"},
	{FormatOpenAINode, "openai-node.mjs", "OpenAI SDK（Node）调用示例"},
	{FormatOpenAICurl, "openai-curl.sh", "curl 调用示例"},
}

// ExportFormats 返回全部支持的导出格式（供端点校验参数、前端列按钮）。
func ExportFormats() []ExportFormat {
	out := make([]ExportFormat, 0, len(exportSpecs))
	for _, s := range exportSpecs {
		out = append(out, s.Format)
	}
	return out
}

// 导出片段里的固定标识：供应商显示名 / ZCode 合并键 / TOML 里的 provider 键。
// 单面板实例场景用稳定串即可——重复导入时客户端按同一键覆盖，不会堆积重复项
// （参考提案 §7：ZCode 用 workbuddy-<前缀>，本函数拿不到前缀，退化为固定串）。
const (
	exportProviderName = "WorkBuddy Cockpit"
	exportProviderID   = "workbuddy-cockpit"
	exportProviderKey  = "workbuddy"
)

// RenderKeyExport 渲染一份客户端配置片段。
//
// baseURL 是面板对外地址（WB_PUBLIC_BASE_URL 或请求 Host 推导，不能硬编码
// 127.0.0.1）；plainKey 是密钥明文——只在创建时返回一次，调用方必须在那一刻
// 调用本函数；model 是主模型 id，必须用**网关口径**（带 cn: / global: 前缀，
// 与 /v1/models 一致——面板模型目录显示裸名，客户端实际调用用带前缀 id，两者
// 混用会让客户端选中模型后 404）。
//
// 返回下载文件名与片段全文；格式未知 / 参数为空时报错（错误文案给出可照做的
// 下一步，不要只丢一句"参数非法"）。
func RenderKeyExport(baseURL, plainKey, model string, format ExportFormat) (string, string, error) {
	if strings.TrimSpace(baseURL) == "" {
		return "", "", errors.New("面板对外地址不能为空：请配置 WB_PUBLIC_BASE_URL 或从请求 Host 推导")
	}
	if strings.TrimSpace(plainKey) == "" {
		return "", "", errors.New("密钥明文不能为空：网关只存哈希，明文只在创建时返回一次，无法事后补导")
	}
	if strings.TrimSpace(model) == "" {
		return "", "", errors.New("模型名不能为空：请传网关口径的模型 id（带 cn: / global: 前缀，与 /v1/models 一致）")
	}
	specIdx := -1
	for i := range exportSpecs {
		if exportSpecs[i].Format == format {
			specIdx = i
			break
		}
	}
	if specIdx < 0 {
		valid := make([]string, 0, len(exportSpecs))
		for _, s := range exportSpecs {
			valid = append(valid, string(s.Format))
		}
		return "", "", fmt.Errorf("未知的导出格式 %q（可选：%s）", format, strings.Join(valid, ", "))
	}
	content, err := renderExportContent(baseURL, plainKey, model, format)
	if err != nil {
		return "", "", err
	}
	return exportSpecs[specIdx].Filename, content, nil
}

// renderExportContent 按格式分发到具体渲染器。两个 URL 口径在这里分岔：
// claude 系走 anthropicBaseURL（根地址），其余走 openAIBaseURL（带 /v1）。
func renderExportContent(baseURL, plainKey, model string, format ExportFormat) (string, error) {
	switch format {
	case FormatCCSwitchClaude, FormatClaudeCodeSettings:
		anthropicURL, err := anthropicBaseURL(baseURL)
		if err != nil {
			return "", err
		}
		return claudeEnvJSON(anthropicURL, plainKey, model)
	case FormatClaudeCodeEnv:
		anthropicURL, err := anthropicBaseURL(baseURL)
		if err != nil {
			return "", err
		}
		return claudeCodeEnvText(anthropicURL, plainKey, model), nil
	case FormatCodexTOML, FormatCCSwitchCodex, FormatZCode,
		FormatOpenAIPython, FormatOpenAINode, FormatOpenAICurl:
		openaiURL, err := openAIBaseURL(baseURL)
		if err != nil {
			return "", err
		}
		return renderOpenAICompatible(openaiURL, plainKey, model, format)
	default:
		return "", fmt.Errorf("未知的导出格式 %q", format)
	}
}

// renderOpenAICompatible 全部「base url 带 /v1」格式的渲染分发（URL 已归一）。
func renderOpenAICompatible(openaiURL, plainKey, model string, format ExportFormat) (string, error) {
	switch format {
	case FormatCodexTOML:
		return codexTOML(openaiURL, model), nil
	case FormatCCSwitchCodex:
		return ccSwitchCodexJSON(openaiURL, plainKey, model)
	case FormatZCode:
		return zcodeJSON(openaiURL, plainKey, model)
	case FormatOpenAIPython:
		return openAIPythonText(openaiURL, plainKey, model), nil
	case FormatOpenAINode:
		return openAINodeText(openaiURL, plainKey, model), nil
	case FormatOpenAICurl:
		return openAICurlText(openaiURL, plainKey, model), nil
	default:
		return "", fmt.Errorf("未知的导出格式 %q", format)
	}
}

// ExportPiece 一份导出片段（POST /api/keys 创建响应的 exports 数组元素）。
// Format/Filename 供前端分格式下载与展示；Content 是片段全文（各格式的完整
// 渲染结果，已含 base_url 与明文密钥）。
type ExportPiece struct {
	Format   ExportFormat `json:"format"`
	Filename string       `json:"filename"`
	Content  string       `json:"content"`
}

// RenderKeyExports 渲染**全部**导出格式（ExportFormats() 顺序）：POST /api/keys
// 创建成功那一刻一次性把各客户端配置片段一起返回给前端——「创建弹窗里直接提供
// 各格式下载」的体验要求全部格式同刻可用，逐个调用 RenderKeyExport 的循环样板
// 在 keys_ep.go 只会出现一次，故收口成本便利函数。
//
// 与 RenderKeyExport 相同的参数校验（baseURL/plainKey/model 任一为空报错）：
// 校验在循环前统一做一次，渲染途中不可能失败（格式枚举来自 ExportFormats()，
// 与 exportSpecs 一一对应，不会命中未知格式分支）。
func RenderKeyExports(baseURL, plainKey, model string) ([]ExportPiece, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("面板对外地址不能为空：请配置 WB_PUBLIC_BASE_URL 或从请求 Host 推导")
	}
	if strings.TrimSpace(plainKey) == "" {
		return nil, errors.New("密钥明文不能为空：网关只存哈希，明文只在创建时返回一次，无法事后补导")
	}
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("模型名不能为空：请传网关口径的模型 id（带 cn: / global: 前缀，与 /v1/models 一致）")
	}
	out := make([]ExportPiece, 0, len(exportSpecs))
	for _, s := range exportSpecs {
		content, err := renderExportContent(baseURL, plainKey, model, s.Format)
		if err != nil {
			return nil, err
		}
		out = append(out, ExportPiece{Format: s.Format, Filename: s.Filename, Content: content})
	}
	return out, nil
}

// —— base url 归一化 ——

// openAIBaseURL 面板对外地址 → OpenAI 兼容 base url（**带 /v1**）。
//
// 用于 codex / ZCode / OpenAI SDK 这类客户端：它们把 /chat/completions 或
// /responses 拼在 base url 后面，所以这里要含 /v1（…/v1 + /chat/completions =
// 面板的 /v1/chat/completions）。已经带 /v1 的不再重复追加（调用方可能直接填了
// 完整地址）。
func openAIBaseURL(publicBase string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(publicBase), "/")
	if base == "" {
		return "", errors.New("面板对外地址不能为空")
	}
	if strings.HasSuffix(base, "/v1") {
		return base, nil
	}
	return base + "/v1", nil
}

// anthropicBaseURL 面板对外地址 → **Anthropic 口径**的 base url（**不带 /v1，
// 保留尾斜杠**）。
//
// Claude Code 走 Anthropic SDK，SDK 自己会在 base url 后面拼 /v1/messages，所以
// ANTHROPIC_BASE_URL 必须是**根地址**：填成 http://host/v1 的话实际请求会变成
// http://host/v1/v1/messages——实测返回 405 而非 404（那个路径存在但不是 POST
// 路由），报错像"方法不对"，极易把人引去查错误方向。
//
// 尾斜杠保留：SDK 直接做字符串拼接，不补斜杠会得到 http://hostv1/messages。
// 参考实现里真实 cc-switch 配置也是这个口径（根地址 + 尾斜杠）。
func anthropicBaseURL(publicBase string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(publicBase), "/")
	if base == "" {
		return "", errors.New("面板对外地址不能为空")
	}
	// 调用方常直接传 OpenAI 口径（…/v1）——claude 系必须回退到根地址。
	if strings.HasSuffix(base, "/v1") {
		base = strings.TrimSuffix(base, "/v1")
	}
	return base + "/", nil
}

// —— 引号转义 ——

// dqQuote 双引号字符串字面量：包双引号并转义 \ 与 "。TOML / Python / JS / shell
// 双引号语境的转义规则相同，共用一个实现。wbk_ 密钥与模型 id 本不含特殊字符，
// 这里是防御性转义（防调用方传入带引号的 name / model 破坏目标文件语法）。
func dqQuote(v string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
}

// —— claude 系（Anthropic 口径）——

// claudeEnv 三档模型全部指向同一模型：面板按模型名路由，没有 haiku/sonnet/opus
// 的概念；留空会让客户端回退到官方模型名而打到错误端点（参考实现注释）。字段
// 顺序即 JSON 输出顺序（struct 序列化保序，map 会按 key 重排）。
type claudeEnv struct {
	AuthToken     string `json:"ANTHROPIC_AUTH_TOKEN"`
	BaseURL       string `json:"ANTHROPIC_BASE_URL"`
	Model         string `json:"ANTHROPIC_MODEL"`
	DefaultHaiku  string `json:"ANTHROPIC_DEFAULT_HAIKU_MODEL"`
	DefaultSonnet string `json:"ANTHROPIC_DEFAULT_SONNET_MODEL"`
	DefaultOpus   string `json:"ANTHROPIC_DEFAULT_OPUS_MODEL"`
}

// claudeEnvDoc 片段外壳：cc-switch 的 claude settings_config 与 Claude Code
// settings.json 的 env 块都是 {"env": {...}} 形态（用户把 env 对象并入自己的文件）。
type claudeEnvDoc struct {
	Env claudeEnv `json:"env"`
}

// claudeEnvJSON 生成 {"env": {...}} JSON 片段：cc-switch 的 claude
// settings_config 与 Claude Code settings.json 的 env 块是同一份内容，共用渲染。
func claudeEnvJSON(anthropicURL, plainKey, model string) (string, error) {
	raw, err := json.MarshalIndent(claudeEnvDoc{Env: claudeEnv{
		AuthToken:     plainKey,
		BaseURL:       anthropicURL, // 根地址而非 /v1：SDK 自己拼 /v1/messages
		Model:         model,
		DefaultHaiku:  model,
		DefaultSonnet: model,
		DefaultOpus:   model,
	}}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal claude env: %w", err)
	}
	return string(raw) + "\n", nil
}

// claudeCodeEnvText 生成 shell 环境变量片段（值经 dqQuote 防御性转义）。
func claudeCodeEnvText(anthropicURL, plainKey, model string) string {
	var b strings.Builder
	b.WriteString("# Claude Code（Anthropic 口径）环境变量：追加到 ~/.bashrc / ~/.zshrc，\n")
	b.WriteString("# 或存为 .env 供 IDE / 启动脚本注入。Windows PowerShell 对应：\n")
	b.WriteString("#   $env:ANTHROPIC_BASE_URL=" + dqQuote(anthropicURL) + "\n")
	b.WriteString("#\n")
	b.WriteString("# 注意：ANTHROPIC_BASE_URL 必须是根地址（不带 /v1）——Anthropic SDK 自己在\n")
	b.WriteString("# base url 后拼 /v1/messages；填 …/v1 会请求 …/v1/v1/messages（实测 405 而非\n")
	b.WriteString("# 404，报错像\"方法不对\"，极易查错方向）。\n")
	b.WriteString("export ANTHROPIC_BASE_URL=" + dqQuote(anthropicURL) + "\n")
	b.WriteString("export ANTHROPIC_AUTH_TOKEN=" + dqQuote(plainKey) + "\n")
	b.WriteString("export ANTHROPIC_MODEL=" + dqQuote(model) + "\n")
	b.WriteString("export ANTHROPIC_DEFAULT_HAIKU_MODEL=" + dqQuote(model) + "\n")
	b.WriteString("export ANTHROPIC_DEFAULT_SONNET_MODEL=" + dqQuote(model) + "\n")
	b.WriteString("export ANTHROPIC_DEFAULT_OPUS_MODEL=" + dqQuote(model) + "\n")
	b.WriteString("# anthropic-version 头由 Claude Code / Anthropic SDK 自动携带（如 2023-06-01），\n")
	b.WriteString("# 无需手动设置。\n")
	return b.String()
}

// —— codex 系（OpenAI 口径）——

// codexTOML 生成 ~/.codex/config.toml 片段（也是 cc-switch codex config 字段的
// 内容——cc-switch 把 base_url / model 写在 TOML 文本里而不是独立字段，照抄以免
// 客户端读不到）。wire_api="responses"：网关已实现 POST /v1/responses（见包注释
// 「口径差异」），codex 客户端优先走 Responses 协议。
func codexTOML(openaiURL, model string) string {
	var b strings.Builder
	b.WriteString("model_provider = " + dqQuote(exportProviderKey) + "\n")
	b.WriteString("model = " + dqQuote(model) + "\n")
	b.WriteString("\n")
	b.WriteString("[model_providers]\n")
	b.WriteString("[model_providers." + exportProviderKey + "]\n")
	b.WriteString("name = " + dqQuote(exportProviderName) + "\n")
	b.WriteString("wire_api = \"responses\"\n")
	b.WriteString("requires_openai_auth = true\n")
	b.WriteString("base_url = " + dqQuote(openaiURL) + "\n")
	return b.String()
}

// ccSwitchCodexDoc cc-switch codex 的 settings_config（auth 与 TOML 文本并列，
// 结构取自 cc-switch 真实数据——参考实现实测）。
type ccSwitchCodexDoc struct {
	Auth struct {
		OpenAIKey string `json:"OPENAI_API_KEY"`
	} `json:"auth"`
	Config string `json:"config"`
}

// ccSwitchCodexJSON 生成 cc-switch codex settings_config JSON（内嵌 TOML 由
// json.Marshal 负责转义，其中的双引号在输出文本里呈 \"）。
func ccSwitchCodexJSON(openaiURL, plainKey, model string) (string, error) {
	doc := ccSwitchCodexDoc{
		Config: codexTOML(openaiURL, model),
	}
	doc.Auth.OpenAIKey = plainKey
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal cc-switch codex: %w", err)
	}
	return string(raw) + "\n", nil
}

// —— ZCode（OpenAI 口径）——

// zcodeAccess / zcodeAPI / zcodeConfig 对齐 ~/.zcode/v2/provider_config.json 的
// providerConfigRules.providerRules[] 元素结构（本机实测形态，参考实现同源）。
type zcodeAccess struct {
	Type   string `json:"type"`
	APIKey string `json:"apiKey"`
}

type zcodeAPI struct {
	Type    string `json:"type"`
	BaseURL string `json:"baseUrl"`
}

type zcodeConfig struct {
	Group            string      `json:"group"`
	Access           zcodeAccess `json:"access"`
	API              zcodeAPI    `json:"api"`
	PersonalModelIDs []string    `json:"personalModelIds"`
	ModelOrder       []string    `json:"modelOrder"`
}

type zcodeProviderRule struct {
	ProviderID   string      `json:"providerId"`
	ProviderName string      `json:"providerName"`
	Config       zcodeConfig `json:"config"`
}

type zcodeExport struct {
	// ProviderRule 供应商配置片段；ProviderModelRules 模型级规则（contextWindow
	// 等）。本函数拿不到模型目录数据（contextWindow 需查 /v1/models 的
	// context_length），一律给空数组——宁可少写让客户端用默认值，也不瞎猜。
	ProviderRule       zcodeProviderRule `json:"providerRule"`
	ProviderModelRules []struct{}        `json:"providerModelRules"`
}

// zcodeJSON 生成 ZCode 的 provider_config.json **片段**而非整份文件：整份文件里
// 还有用户自己的其它供应商与顺序，导出端不该替用户决定 providerOrder 的全量
// 内容。调用方负责按 providerId（exportProviderID）合并。
// api.type="openai-responses"：网关已实现 POST /v1/responses（见包注释「口径差
// 异」），且 ZCode 内置供应商里 OpenAI / x.ai 等 Responses 形态即此取值
// （baseUrl 同为带 /v1 口径），与参考实现一致。
func zcodeJSON(openaiURL, plainKey, model string) (string, error) {
	doc := zcodeExport{
		ProviderRule: zcodeProviderRule{
			ProviderID:   exportProviderID,
			ProviderName: exportProviderName,
			Config: zcodeConfig{
				Group:            "standard-personal",
				Access:           zcodeAccess{Type: "api-key", APIKey: plainKey},
				API:              zcodeAPI{Type: "openai-responses", BaseURL: openaiURL},
				PersonalModelIDs: []string{model},
				ModelOrder:       []string{model},
			},
		},
		ProviderModelRules: []struct{}{},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal zcode provider: %w", err)
	}
	return string(raw) + "\n", nil
}

// —— 通用 OpenAI SDK 示例 ——

// openAIPythonText 生成 Python 调用示例（OpenAI SDK 自己在 base_url 后拼完整
// 路径，所以 base_url 带 /v1）。
func openAIPythonText(openaiURL, plainKey, model string) string {
	var b strings.Builder
	b.WriteString("# OpenAI 兼容调用示例（Python）：pip install openai\n")
	b.WriteString("# base_url 带 /v1：网关路由在 /v1/chat/completions（SDK 自己拼完整路径）\n")
	b.WriteString("from openai import OpenAI\n")
	b.WriteString("\n")
	b.WriteString("client = OpenAI(\n")
	b.WriteString("    base_url=" + dqQuote(openaiURL) + ",\n")
	b.WriteString("    api_key=" + dqQuote(plainKey) + ",\n")
	b.WriteString(")\n")
	b.WriteString("\n")
	b.WriteString("resp = client.chat.completions.create(\n")
	b.WriteString("    model=" + dqQuote(model) + ",\n")
	b.WriteString("    messages=[{\"role\": \"user\", \"content\": \"你好\"}],\n")
	b.WriteString(")\n")
	b.WriteString("print(resp.choices[0].message.content)\n")
	return b.String()
}

// openAINodeText 生成 Node（ESM）调用示例。
func openAINodeText(openaiURL, plainKey, model string) string {
	var b strings.Builder
	b.WriteString("// OpenAI 兼容调用示例（Node ESM）：npm install openai\n")
	b.WriteString("// baseURL 带 /v1：网关路由在 /v1/chat/completions（SDK 自己拼完整路径）\n")
	b.WriteString("import OpenAI from \"openai\";\n")
	b.WriteString("\n")
	b.WriteString("const client = new OpenAI({\n")
	b.WriteString("  baseURL: " + dqQuote(openaiURL) + ",\n")
	b.WriteString("  apiKey: " + dqQuote(plainKey) + ",\n")
	b.WriteString("});\n")
	b.WriteString("\n")
	b.WriteString("const resp = await client.chat.completions.create({\n")
	b.WriteString("  model: " + dqQuote(model) + ",\n")
	b.WriteString("  messages: [{ role: \"user\", content: \"你好\" }],\n")
	b.WriteString("});\n")
	b.WriteString("console.log(resp.choices[0].message.content);\n")
	return b.String()
}

// openAICurlText 生成 curl 示例（Authorization: Bearer 与网关 httpauth 口径一致）。
func openAICurlText(openaiURL, plainKey, model string) string {
	var b strings.Builder
	b.WriteString("# OpenAI 兼容调用示例（curl）：网关路由为 POST /v1/chat/completions\n")
	b.WriteString("curl " + openaiURL + "/chat/completions \\\n")
	b.WriteString("  -H \"Content-Type: application/json\" \\\n")
	b.WriteString("  -H \"Authorization: Bearer " + plainKey + "\" \\\n")
	b.WriteString("  -d '{\"model\": " + dqQuote(model) +
		", \"messages\": [{\"role\": \"user\", \"content\": \"你好\"}]}'\n")
	return b.String()
}
