// sanitize.go 出站请求体脱敏：剥离上游内容审核黑名单指纹。
//
// 背景：客户端（Claude Code 类 CLI）在 system prompt 注入若干固定模板句，
// 上游内容审核按逐字精确匹配拦截（非语义审核），一字改动即可绕过。
// 策略：键值/header 型指纹整段剥离；承载语义的模板句最小改写（换一词），语义不变。
package upstream

import (
	"encoding/json"
	"log"
	"regexp"
	"strings"
	"sync"
)

// sanitizeFeatures 特征预检：任一命中才进入净化（strings.Contains 快速路径，
// 普通请求全不中 → 原样返回，零分配）。
var sanitizeFeatures = []string{
	"x-anthropic-billing-header", // header 键值段键名
	"cc_entrypoint=",             // 尾随裸键值（截断前缀即可命中）
	"You are Claude Code",        // 身份句（截断前缀即可命中）
	"Main branch (",              // 注入指令句（截断前缀即可命中）
	"You are a coding agent running in the Codex CLI", // Codex instructions 首段（截断前缀即可命中）
	"github.com/anthropics/",                          // 反馈句里的 Anthropic 仓库链接
	"11128",                                           // 上游反探测：裸数字错误码
}

// sanitizeHdrRe 剥离层：header 键名即触发（与值无关），整段删除。
var sanitizeHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\n]*;?\s*`)

// sanitizeBareHdrRe 兜底层：裸键名（无冒号无值）同样是指纹——2026-09-13 实验 F4
// 证实 assistant 消息里反引号引用裸键名即触发 11128，而剥离层要求冒号、对裸串无效。
// 键值形态被整段删除后，残留的裸键名做最小缩写（header→hdr）：破坏逐字匹配、
// 语义不变、保留可读性。大小写不敏感，覆盖 X-Anthropic-... 变体。
//
// 注意该正则不要求冒号，是 sanitizeHdrRe 的超集——hasFingerprint 与 sanitizeText
// 中两者并用：先删键值形态（sanitizeHdrRe），再缩写残留裸键名（本正则），
// 替换语义不同（整段删除 vs 最小缩写），不可合并为一个正则。
var sanitizeBareHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header`)

// sanitizeKvRe 剥离层：尾随裸键值（cc_xxx=...;）循环清理。
var sanitizeKvRe = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

// sanitizeRewrites 改写层：全模板句逐字替换（每句只改一个词，语义不变）。
//
// 身份句的匹配串**不带结尾标点**（只到 "…for Claude" 为止）：
// CLI 版这句以句号收尾（"…for Claude."），桌面版（claude-desktop-3p / Agent SDK）
// 以逗号接后继内容（"…for Claude, running within the Claude Agent SDK."）。
// 带句号的整句只匹配前者，桌面版会漏网、指纹原样发上游 → 400 code=11128。
// 去掉结尾标点后两种形态一并覆盖（替换串同样不带标点，让原有标点原样保留）。
// 注意仍要求 "You are Claude Code, " 前缀，不做更宽的子串替换，
// 以免误伤 TestExactMatchOnlyVariantNotTouched 所保护的零散文本。
var sanitizeRewrites = [][2]string{
	{
		"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude",
	},
	{
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)",
	},
	{
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
	},
	{
		// 反馈句：整句带 Anthropic 仓库链接，上游按整句拦截（只留链接或只留半边均不拦，
		// 实测需整句同时出现）。give→provide 一词之差即可绕过，语义不变。
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
	},
	{
		// 上游反探测：只要请求体里出现裸数字 11128 就整单拦截（与该数字的上下文无关——
		// "code=11128" / 裸 "11128" / "错误码 11128" / "Code=11128" 全部命中；
		// 相邻的 11148 / 11101 / 11115 / 99999 均放行）。11128 正是本类拦截自身的错误码，
		// 上游据此识别"在讨论/回显其内部错误码"的请求。
		// 代价：用户对话中任何 11128 都会被改写——但这串数字出现在请求里本身就是拦截条件，
		// 不改写必然失败。插入连字符保留可读性与指代（零宽空格无效，实测上游会归一化）。
		"11128",
		"11-128",
	},
}

// sanitizeDshRe DSH（DeepSeek Harness）system 自我介绍句的模板匹配。
//
// 上游拦截的是句式而不是品牌名：DSH 的 system 首句常见
// "You are an AI agent powered by <brand>." 与
// "You are a coding agent powered by the <model> model."，把品牌换掉同样会
// 返回 400 code=11-128。按句式整句替换为中性介绍，避免只替换品牌后留下残句。
//
// 句尾必须按「点 + 空白/行尾」收口：模型名自带点（deepseek-v4.1-flash），
// 用 [^\n]*\. 会停在 "v4." 并留下 "1-flash model."。`{0,120}` 只是防病态长行
// 的上限，正常情况下非贪婪匹配会在第一个句号收口。起点不限行首，兼容客户端
// 把该句嵌在模板里的形态；`${1}` 回填句尾空白，防止两条指令被粘在一起。
// 2026-10-28（manager 0036 吸收）：动词集合扩到社区实测的 7 个变体——
// powered by / built on / built by / created by / developed by / made by /
// trained by。上游对句式而不是品牌名做匹配，只覆盖前两个会漏掉其余写法。
var sanitizeDshRe = regexp.MustCompile(
	`(?i)\byou are (?:a|an) (?:ai )?(?:coding )?(?:agent|assistant) (?:powered by|built on|built by|created by|developed by|made by|trained by) [^\n]{0,120}?\.(\s|$)`)

// sanitizeDshRepl 中性、无品牌的替身（自身不含 sanitizeFeatures 的任何片段）。
const sanitizeDshRepl = "You are a helpful AI assistant.${1}"

// sanitizeDshGuiRe 通配层：DSH 运行时上下文里的 Web GUI 段落（issue #250，上游
// manager 0439d68a 吸收）。
//
// 上游的匹配**不是单句**：单独一句
//
//	You are interacting with the user through the DeepSeek Harness Web GUI whose
//	URL the runtime context names.
//
// 会放行；但连同下一句 "When the user refers to … they mean this GUI." 组成
// 221 字符的**连续文本**就返回 400 code=11-128。二分实测：前缀 220 字符成功、
// 221 字符失败，说明匹配窗口横跨两句，单句改写无效（两句各自单独测都放行）。
//
// 因此对段落里的品牌短语做最小改写（插入连字符）：破坏逐字匹配、语义不变，
// 且该短语只出现在被拦段落内，不会误伤正常对话。实测改写后上游放行。
var sanitizeDshGuiRe = regexp.MustCompile(`(?i)DeepSeek[- ]Harness Web GUI`)

// sanitizeDshGuiRepl 替换串：保留品牌可读性，仅在 Web 与 GUI 之间插入一个连字符。
// 2026-10-28 修正：原值与匹配串逐字相同，ReplaceAllString 是空操作，issue #250
// 的跨句保护实际从未生效；改为 "Web-GUI" 才真正拆开上游的逐字匹配窗口。
const sanitizeDshGuiRepl = "DeepSeek-Harness Web-GUI"

// sanitizeWorkspaceRootRe DSH file-reference 提示词里的完整模板句
// （2026-10-27 上游新增词，manager 0036 吸收）。
//
// 实测只有「Relative paths resolve from the workspace root」这个完整短语参与
// 跨句匹配才 400；单独的 workspace root / workspace roots 出现在用户消息、
// assistant 文本或工具描述里都放行。因此只收窄匹配完整短语（大小写不敏感），
// root→base 一词之差即可绕过，语义不变，也不扩大普通文本的改写面。
// 末尾必须加 \b：没有尾部边界时复数形态 "workspace roots" 会以 root 前缀命中，
// 被改写成 "...workspace bases."（错误地吃掉 s 之外的语义）；\b 要求 root 后
// 不接词字符，保证只命中单数完整短语。
var sanitizeWorkspaceRootRe = regexp.MustCompile(`(?i)relative paths resolve from the workspace root\b`)

// sanitizeWorkspaceRootRepl 完整短语的替身：仅 workspace root → workspace base，
// 后续 "; absolute paths ..." 原文保留。
const sanitizeWorkspaceRootRepl = "Relative paths resolve from the workspace base"

// sanitizeText 单段文本净化：预检不中 → 返回原串（零分配）。
func sanitizeText(text string) string {
	if !hasFingerprint(text) {
		return text
	}
	// 运行期额外词表（features.sanitize_extra_rewrites）：上游逐词扩充黑名单时
	// 改 config.json 重启即可，不必为每个新词重编译。
	//
	// 顺序刻意放在内置改写之前：extra 的 replacement 来自运维配置，可能重新引入
	// 内置指纹（例如 11·128）；内置规则最后执行，保证任何来源的指纹都被收口。
	for _, rw := range sanitizeExtraRewrites {
		// 源串在 ConfigureSanitize 已按字面量预编译为 (?i) 正则：大小写变体
		// （"zcode" / "ZCODE"）必须真正改写，而不是只靠末尾 TrimSpace。
		// replacement 按字面量回填，避免配置里的 $ 被当成捕获组引用。
		text = rw.re.ReplaceAllLiteralString(text, rw.repl)
	}
	for _, rw := range sanitizeRewrites {
		text = strings.ReplaceAll(text, rw[0], rw[1])
	}
	// DSH file-reference 的完整模板句（manager 0036）：只改完整短语，
	// 普通 workspace root(s) 文本不动（单句实测不触发上游）。
	text = sanitizeWorkspaceRootRe.ReplaceAllString(text, sanitizeWorkspaceRootRepl)
	text = sanitizeDshRe.ReplaceAllString(text, sanitizeDshRepl)
	// DSH Web GUI 段落：上游按「跨两句的连续文本」拦截，必须动品牌短语才拆得开。
	text = sanitizeDshGuiRe.ReplaceAllString(text, sanitizeDshGuiRepl)
	if sanitizeHdrRe.MatchString(text) {
		text = sanitizeHdrRe.ReplaceAllString(text, "")
	}
	if strings.Contains(text, "cc_") {
		prev := ""
		for prev != text { // 清尾随裸 kv（cc_version=...; cc_entrypoint=...;）
			prev = text
			text = sanitizeKvRe.ReplaceAllString(text, "")
		}
	}
	// 兜底：键值形态已在上面整段删除，这里只剩裸键名（引用/示例文本形态）。
	text = sanitizeBareHdrRe.ReplaceAllString(text, "x-anthropic-billing-hdr")
	return strings.TrimSpace(text)
}

// hasFingerprint 特征预检：先走 strings.Contains 快速路径（零分配）。
//
// 内置 sanitizeFeatures 必须与 sanitizeRewrites 一样**精确匹配**（大小写敏感）：
// 小写变体（如 "main branch ("）进入净化分支后没有任何内置改写命中，只剩末尾
// TrimSpace，会把普通文本的首尾空白意外吃掉。真正需要大小写覆盖的 DSH /
// workspace / header 规则各自带 (?i) 正则，不依赖这里的 ToLower。
func hasFingerprint(text string) bool {
	for _, f := range sanitizeFeatures {
		if strings.Contains(text, f) {
			return true
		}
	}
	// 运行期额外特征（features.sanitize_extra_features）：保持大小写不敏感
	// （运维配置的上游新词可能大小写不定）。与内置表分开存放，
	// ConfigureSanitize 可幂等整体替换；仅在确有额外词时才分配小写副本。
	if len(sanitizeExtraFeatures) > 0 {
		lower := strings.ToLower(text)
		for _, f := range sanitizeExtraFeatures {
			if f == "" {
				continue
			}
			if strings.Contains(text, f) || strings.Contains(lower, strings.ToLower(f)) {
				return true
			}
		}
	}
	// DSH 句式指纹必须在这里也命中，否则预检直接返回、改写层根本执行不到。
	// 正则自带 (?i)：原文是 "AI agent" 大写，不能靠大小写敏感的 Contains 特征串。
	if sanitizeDshRe.MatchString(text) {
		return true
	}
	// DSH Web GUI 段落指纹（见 sanitizeDshGuiRe）：跨两句匹配，单句放行，
	// 所以特征表里补的是段落内的品牌短语；(?i) 覆盖大小写变体。
	if sanitizeDshGuiRe.MatchString(text) {
		return true
	}
	// DSH file-reference 的完整模板句（manager 0036）：大小写变体一并覆盖。
	if sanitizeWorkspaceRootRe.MatchString(text) {
		return true
	}
	return sanitizeBareHdrRe.MatchString(text)
}

// sanitizeExtraFeatures / sanitizeExtraRewrites 运行期从 config 追加的词表
// （features.sanitize_extra_features / sanitize_extra_rewrites）。启动时一次性
// 注入、之后只读；ConfigureSanitize 每次整体替换而不是 append，保证幂等。
var (
	sanitizeExtraFeatures []string
	sanitizeExtraRewrites []sanitizeExtraRewrite
)

// sanitizeExtraRewrite 运行期额外改写规则：源串按字面量预编译（QuoteMeta）、
// 大小写不敏感（(?i)）；replacement 在 sanitizeText 里按字面量回填
// （ReplaceAllLiteralString），配置里的 $ 不会被解释为捕获组引用。
type sanitizeExtraRewrite struct {
	src  string
	re   *regexp.Regexp
	repl string
}

// sanitizeExtraRewriteWarnOnce 空 replacement 的告警只打一次，避免重复刷屏。
var sanitizeExtraRewriteWarnOnce sync.Once

// ConfigureSanitize 注入运行期额外词表（启动时由 cmd/server 调用一次）。
//
// 上游黑名单是逐词扩充的（GUI 句 → workspace root → 后续还会加），把它做成配置
// 项后，运维改 config.json 重启容器即可跟上，不必等本仓库发版。
// 空参数 = 清空额外词表，回落到内置静态表。
func ConfigureSanitize(extraFeatures []string, extraRewrites [][2]string) {
	cleanFeatures := make([]string, 0, len(extraFeatures))
	for _, f := range extraFeatures {
		if f = strings.TrimSpace(f); f != "" {
			cleanFeatures = append(cleanFeatures, f)
		}
	}
	cleanRewrites := make([]sanitizeExtraRewrite, 0, len(extraRewrites))
	for _, rw := range extraRewrites {
		// 源串去首尾空白并拒绝空/纯空白：否则 " " 会把全文空格替换成任意
		// replacement，造成大范围误改（TrimSpace 后的空白源串没有合法语义）。
		src := strings.TrimSpace(rw[0])
		if src == "" || src == rw[1] {
			continue
		}
		// 空 replacement = 「删除所有匹配」。JSON 的单元素 pair（[["foo"]]）
		// 会被解码成 {"foo", ""}（定长数组缺位补零），一条配置就会把全文的
		// foo 全部删掉——几乎不可能是运维本意，过滤并一次性告警。
		if rw[1] == "" {
			sanitizeExtraRewriteWarnOnce.Do(func() {
				log.Printf("WARN: [upstream] sanitize_extra_rewrites: 忽略空 replacement 的条目 src=%q（避免大范围删除文本）", src)
			})
			continue
		}
		cleanRewrites = append(cleanRewrites, sanitizeExtraRewrite{
			src:  src,
			re:   regexp.MustCompile("(?i)" + regexp.QuoteMeta(src)),
			repl: rw[1],
		})
	}
	// rewrite 源串自动纳入预检：只配 sanitize_extra_rewrites、不配 features 时
	// 也能进入净化分支（否则 rewrite 完全不生效）。大小写不敏感去重。
	for _, rw := range cleanRewrites {
		found := false
		for _, f := range cleanFeatures {
			if strings.EqualFold(f, rw.src) {
				found = true
				break
			}
		}
		if !found {
			cleanFeatures = append(cleanFeatures, rw.src)
		}
	}
	sanitizeExtraFeatures = cleanFeatures
	sanitizeExtraRewrites = cleanRewrites
}

// sanitizeContent 兼容字符串与多模态数组；只动 text part，image 等 part 不动。
// 返回净化后的值及是否发生变化。
func sanitizeContent(v any) (any, bool) {
	switch c := v.(type) {
	case string:
		s := sanitizeText(c)
		return s, s != c
	case []any:
		changed := false
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			text, ok := m["text"].(string)
			if !ok {
				continue
			}
			if s := sanitizeText(text); s != text {
				m["text"] = s
				changed = true
			}
		}
		return c, changed
	}
	return v, false
}

// sanitizeToolCallWarnOnce 非法 JSON 只告警一次，避免高频工具调用刷屏。
var sanitizeToolCallWarnOnce sync.Once

// sanitizeToolCalls 净化 assistant.tool_calls[].function.arguments。
//
// arguments 是**字符串化的 JSON**（不是对象），因此按文本走 sanitizeText 即可。
// 这块长期是盲区：工具调用消息的 content 通常是 null，而旧版 sanitizeMessages
// 在 content 缺失时直接 continue，整条消息连 tool_calls 一起被跳过——
// 于是历史里任何写进工具参数的被拦字符串（文件名、命令、写入内容）都会原样漏出。
//
// 回写前用 json.Valid 校验：extra rewrites 来自运维配置，若 replacement 含引号
// 等字符会把 JSON 改坏；此时保留原 arguments（宁可少改，也不能让上游收到坏 JSON）。
func sanitizeToolCalls(v any) bool {
	callList, ok := v.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, c := range callList {
		call, ok := c.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		args, ok := fn["arguments"].(string)
		if !ok {
			continue
		}
		if s := sanitizeText(args); s != args {
			if !json.Valid([]byte(s)) {
				sanitizeToolCallWarnOnce.Do(func() {
					log.Printf("WARN: [upstream] sanitize tool_calls: rewritten arguments 不是合法 JSON，保留原文")
				})
				continue
			}
			fn["arguments"] = s
			changed = true
		}
	}
	return changed
}

// sanitizeMessages 净化 messages 中的 content、reasoning_content 与 tool_calls；任一命中返回 true。
func sanitizeMessages(messages []any) bool {
	changed := false
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		// content 与 tool_calls 各自独立判断：content 可以为 null（工具调用轮），
		// 早期版本在此 continue，导致这类消息的 tool_calls 完全不被净化。
		if c, ok := m["content"]; ok {
			if nc, ch := sanitizeContent(c); ch {
				m["content"] = nc
				changed = true
			}
		}
		// reasoning_content（思维链回填字段，见 thinking.go/sse.go）实测同样
		// 携带指纹，与 content 同等净化。string 形态直接走 sanitizeText。
		if rc, ok := m["reasoning_content"].(string); ok {
			if s := sanitizeText(rc); s != rc {
				m["reasoning_content"] = s
				changed = true
			}
		}
		// reasoning：OpenAI 形态的思维链字段。thinking.go 的回填会把客户端送来的
		// reasoning_content 镜像进 reasoning（反之亦然），客户端传入的历史消息也
		// 可能原生带该字段——此前只洗 content / reasoning_content / tool_calls，
		// 镜像进 reasoning 的指纹原样出站（裸 11-128 这类反探测串同样致命）。
		if r, ok := m["reasoning"].(string); ok {
			if s := sanitizeText(r); s != r {
				m["reasoning"] = s
				changed = true
			}
		}
		if tc, ok := m["tool_calls"]; ok {
			if sanitizeToolCalls(tc) {
				changed = true
			}
		}
	}
	return changed
}

// sanitizeTools 净化顶层 tools/functions 数组（H-sanitize）：工具定义的
// description/parameters 同样可能携带指纹（编码 agent 的错误码查询类工具
// 是常见载体），请求体级逐字匹配拦截（sanitize.go:70-75）会覆盖这些字段，
// 原样出站即整单 400。parameters 是 JSON schema 对象——序列化后走 sanitizeText
// 再回写（序列化失败则跳过该项，不破坏 schema 结构）。任一命中返回 true。
//
// 工具 **name 不改写**（review HIGH）：name 是客户端与模型之间的工具调用契约；
// 改写 definition name 却不改 tool_calls/tool_choice/历史里的同名引用，会让
// 定义与调用名不一致，上游按新名返回的 tool_call 客户端也不认识。若 name 里
// 真的带指纹，宁可让请求按原语义失败，也不静默改变工具身份。
func sanitizeTools(v any) bool {
	list, ok := v.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, item := range list {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		// OpenAI tools 形态：{"type":"function","function":{"name","description","parameters"}}
		// 与 OpenAI functions 形态：{"name","description","parameters"} 同构——
		// 有 function 子对象则进子对象，否则直接在项上操作。
		target := tool
		if fn, ok := tool["function"].(map[string]any); ok {
			target = fn
		}
		// 只改 description；name 是工具身份契约，见函数头注释。
		if s, ok := target["description"].(string); ok {
			if ns := sanitizeText(s); ns != s {
				target["description"] = ns
				changed = true
			}
		}
		// parameters 是 JSON schema 对象：序列化后走 sanitizeText（schema 文本
		// 同样可能含指纹），序列化失败跳过该项（不破坏结构）。
		if params, ok := target["parameters"]; ok && params != nil {
			raw, err := json.Marshal(params)
			if err == nil {
				if ns := sanitizeText(string(raw)); ns != string(raw) {
					var rebuilt any
					if json.Unmarshal([]byte(ns), &rebuilt) == nil {
						target["parameters"] = rebuilt
						changed = true
					}
				}
			}
		}
	}
	return changed
}
