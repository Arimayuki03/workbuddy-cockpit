// responses.go OpenAI Responses API（/v1/responses、/responses）兼容层（v1.15.1）。
//
// 为什么需要：一批客户端（Codex 等 Responses 生态）只发 **Responses** 协议——
// 请求体是 `input` 而不是 `messages`、`instructions` 是顶层字段、工具是扁平形状；
// 响应体是 `output` 数组、流式是一串 `response.output_text.delta` 事件。上游
// （腾讯 CodeBuddy）只有 Chat Completions，两者不是"参数改名"级别的差别，
// 只能在本层做双向翻译。
//
// 职责边界：本文件只做**协议翻译**与**内部转发**，其余（鉴权、选号、轮转、降级、
// usage 采集、请求日志、prompt_cache_key 注入）一律复用 chatCompletions 既有管线
// ——多一个协议不走一套新逻辑。转换后的 chat 请求经 io.Pipe 喂给
// h.chatCompletions（ResponseWriter 拦截器抓状态码与响应体），流式响应再经
// rspStreamTranslator 翻译成 Responses 事件流。
//
// reasoning 回填分工：本层负责把 Responses 的 reasoning item 转成 OpenAI 消息
// 字段（reasoning + reasoning_content 双写、空文本补空格占位）；出站侧的
// deepseek 门控回填（镜像/占位兜底）仍由 internal/upstream/payload.go 的
// backfillReasoningContent 管线接管——两侧口径一致（" " 占位、字段存在即过闸）。
//
// 参考：workbuddy-manager `server/routers/responses.py`（语义逐条对齐）。
package server

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/session"
)

// ---------------------------------------------------------------------------
// 挂载（约定：不改 handler.go，由集成步骤在 NewHandler 末尾一行接线：
// h.mountResponses(h.mux)；anthropic 代理同理。测试内直接调用挂载验证。）
// ---------------------------------------------------------------------------

// mountResponses 注册 Responses API 路由（挂在给定 mux 上，鉴权与 /v1/chat/completions
// 同口径走 withAuth）。
//   - /v1/responses：SDK 的 baseURL 带 /v1 时走这里；
//   - /responses：OpenAI SDK 的 responses.create 是 {baseURL}/responses，
//     baseURL 只填到域名（不含 /v1）时走这条路径。
func (h *Handler) mountResponses(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/responses", h.withAuth(h.handleResponses))
	mux.HandleFunc("POST /responses", h.withAuth(h.handleResponses))
}

// MountResponses 挂载导出面：main.go 集成在 NewHandler 之后调用
// `h.MountResponses()`（等价 mountResponses(h.mux)），不改 handler.go 也不侵入
// 既有装配函数；未调用的构建里 /v1/responses 路由不存在，零影响。
func (h *Handler) MountResponses() { h.mountResponses(h.mux) }

// ---------------------------------------------------------------------------
// ID 生成（responses 侧的 item/response id）
// ---------------------------------------------------------------------------

// rspID 生成 `<prefix>_<32hex>` 形态的 ID（session.NewMessageID 的 crypto/rand 源）。
func rspID(prefix string) string {
	return prefix + "_" + session.NewMessageID()
}

// ---------------------------------------------------------------------------
// reasoning 凭据编码（多轮一致性"凭据"：客户端原样搬来搬去的不透明串）
// ---------------------------------------------------------------------------

// rspCredPrefix 凭据前缀：标明该串由本网关编码（可逆 base64 明文）。客户端回传后
// 能直接还原推理原文，满足腾讯「上一轮推理内容必须回传」的校验——缺了报 400
// 11155 reasoning_content_missing → 账号记失败 → 冷却 → 503 死循环。
const rspCredPrefix = "wbm1:"

// rspEncodeCredential 把推理原文编码成可回传的凭据串（可逆编码，非真加密：
// 客户端回传时能直接还原，不必引入真实密钥管理）。
func rspEncodeCredential(text string) string {
	return rspCredPrefix + base64.URLEncoding.EncodeToString([]byte(text))
}

// rspDecodeCredential 解回推理原文；不是本网关编出来的（前缀不符/解码失败）返回
// false——客户端可能回传真·OpenAI 的加密串（我们从未生成过），那种解不开也不该
// 报错，交调用方回落其它来源（summary/content）。
func rspDecodeCredential(raw string) (string, bool) {
	if !strings.HasPrefix(raw, rspCredPrefix) {
		return "", false
	}
	b, err := base64.URLEncoding.DecodeString(raw[len(rspCredPrefix):])
	if err != nil {
		return "", false
	}
	return string(b), true
}

// rspAttachReasoning 把推理文本挂到 assistant 消息上，**两个字段名都写**
// （`reasoning` + `reasoning_content`）：社区报告腾讯请求侧校验读的是 `reasoning`，
// 多写一个上游不认的字段无害、是零成本对冲；且上游兜底 backfillReasoningContent
// 读 `reasoning` 复制到 `reasoning_content`、见到 reasoning_content 已存在就跳过
// ——我们挂了反而会让上游兜底不生效，两个都写才稳。
//
// 空文本补单个空格（不是空串）：上游校验 len(reasoning)>0 且不做 trim——空白串
// 过闸、空串不过。与 internal/upstream/thinking.go backfillReasoningContent 的
// " " 占位口径一致（该字段是透传校验位非内容消费位，占位不污染上下文）。
func rspAttachReasoning(msg map[string]any, text string) {
	value := text
	if value == "" {
		value = " "
	}
	msg["reasoning"] = value
	msg["reasoning_content"] = value
}

// ---------------------------------------------------------------------------
// 工具桥接（namespace 子工具展开 + custom tool 桥接为 function call）
// ---------------------------------------------------------------------------

// rspToolBridge 把 Responses 扁平工具定义展开为 Chat Completions 嵌套形状，
// 并在响应侧还原客户端原名/类型。
//   - namespace 工具：子工具出站名带命名空间链回退名（`ns__tool`），客户端历史里
//     的 `ns.tool` / `ns__tool` / 裸名都映射到同一出站名（重名自动加 _2 后缀）；
//     响应侧还原为子工具原名。
//   - custom 工具（自由文本入参）：桥接为单参数 function（{"input": <原文>} JSON
//     包装），响应侧还原为 custom_tool_call。
type rspToolBridge struct {
	chatTools   []any
	customNames map[string]bool     // 出站名 → 是否 custom 桥接
	forward     map[string]string   // 客户端名/出站名 → 出站名
	reverse     map[string]rspIdent // 出站名 → 客户端身份
	taken       map[string]bool     // 已占用的出站名
}

// rspIdent 工具的客户端侧身份（原名 + 是否 custom）。
type rspIdent struct {
	name string
	cust bool
}

// newRspToolBridge 展开 tools 数组（nil 安全：nil/空数组 → 空桥接，全部直通）。
func newRspToolBridge(tools []any) *rspToolBridge {
	b := &rspToolBridge{
		customNames: map[string]bool{},
		forward:     map[string]string{},
		reverse:     map[string]rspIdent{},
		taken:       map[string]bool{},
	}
	b.walk(tools, nil)
	return b
}

// rspToolInner 取工具定义本体：function 包裹形态（Chat 形状残留）或扁平形态。
func rspToolInner(tool map[string]any) map[string]any {
	if fn, ok := tool["function"].(map[string]any); ok {
		return fn
	}
	return tool
}

// rspToolName 取工具名（非 string 返回空串）。
func rspToolName(tool map[string]any) string {
	name, _ := rspToolInner(tool)["name"].(string)
	return name
}

// rspSanitizeToolName 工具名净化：非法字符 → "_"、去首尾 "_"、上限 64 字符、
// 空回落 "tool"（上游工具名白名单 [A-Za-z0-9_-]，出站名必须落域内）。
func rspSanitizeToolName(name string) string {
	var sb strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	cleaned := strings.Trim(sb.String(), "_")
	if len(cleaned) > 64 {
		cleaned = cleaned[:64]
	}
	if cleaned == "" {
		cleaned = "tool"
	}
	return cleaned
}

// unique 分配不冲突的出站名（冲突加 _2/_3 后缀，64 字符内截断保后缀）。
func (b *rspToolBridge) unique(base string) string {
	name, n := rspSanitizeToolName(base), 2
	for b.taken[name] {
		suffix := "_" + strconv.Itoa(n)
		if len(name)+len(suffix) > 64 {
			name = rspSanitizeToolName(base)[:64-len(suffix)] + suffix
		} else {
			name = rspSanitizeToolName(base) + suffix
		}
		n++
	}
	b.taken[name] = true
	return name
}

// walk 递归展开工具数组；path 是 namespace 链（父子以 __ 连接做回退名）。
func (b *rspToolBridge) walk(tools []any, path []string) {
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := tool["type"].(string)
		name := rspToolName(tool)
		if kind == "namespace" {
			if children, ok := tool["tools"].([]any); ok && name != "" {
				b.walk(children, append(append([]string{}, path...), name))
			}
			continue
		}
		if name == "" {
			continue
		}
		full := strings.Join(append(append([]string{}, path...), name), "__")
		// 裸名已被占用（跨 namespace 重名/顶层重名）→ 用带链名的形态做首选。
		preferred := name
		if b.taken[rspSanitizeToolName(name)] {
			preferred = full
		}
		chatName := b.unique(preferred)
		if _, seen := b.forward[name]; !seen {
			b.forward[name] = chatName
		}
		b.forward[chatName] = chatName
		if len(path) > 0 {
			// 客户端历史里 namespace 子工具的两种引用形态都归一到同一出站名。
			b.forward[strings.Join(append(append([]string{}, path...), name), ".")] = chatName
			b.forward[full] = chatName
		}
		cust := kind == "custom"
		b.reverse[chatName] = rspIdent{name: name, cust: cust}
		inner := rspToolInner(tool)
		fn := map[string]any{
			"name":        chatName,
			"description": rspString(inner["description"]),
			"parameters":  rspParams(inner["parameters"]),
		}
		if cust {
			b.customNames[chatName] = true
			// custom 工具经 function 包装执行：入参必须是 {input: <原文>}。
			// 语法说明进 description（Chat Completions 桥接不强制 grammar）。
			fn["description"] = fn["description"].(string) +
				"\nPass the complete raw text for this custom tool in the input string. " +
				"Do not encode it as function arguments inside that string. " +
				"Any custom grammar is not enforced by this Chat Completions bridge."
			fn["parameters"] = map[string]any{
				"type": "object",
				"properties": map[string]any{
					"input": map[string]any{
						"type":        "string",
						"description": "Complete raw custom tool input.",
					},
				},
				"required":             []any{"input"},
				"additionalProperties": false,
			}
			if fmtObj, ok := tool["format"].(map[string]any); ok {
				if ft, _ := fmtObj["type"].(string); ft == "grammar" {
					if def, ok := fmtObj["definition"].(string); ok && def != "" {
						fn["description"] = fn["description"].(string) +
							"\nRequested grammar (guidance only):\n" + def
					}
				}
			}
		}
		b.chatTools = append(b.chatTools, map[string]any{"type": "function", "function": fn})
	}
}

// rspString any → string（非 string 返回空串）。
func rspString(v any) string {
	s, _ := v.(string)
	return s
}

// rspParams 工具参数 schema：缺失/非对象回落空对象 schema（上游要求 parameters 存在）。
func rspParams(v any) any {
	if m, ok := v.(map[string]any); ok && m != nil {
		return m
	}
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// outboundName 客户端名 → 出站名（未登记原样返回）。
func (b *rspToolBridge) outboundName(name any) string {
	s := rspString(name)
	if mapped, ok := b.forward[s]; ok {
		return mapped
	}
	return s
}

// restore 出站名 → (客户端原名, 是否 custom)（未登记按 function 返回原名）。
func (b *rspToolBridge) restore(name string) (string, bool) {
	if ident, ok := b.reverse[name]; ok {
		return ident.name, ident.cust
	}
	return name, false
}

// rspCustomInputDecode 解码 custom tool 的临时 Chat 包装参数（{"input": "..."}）：
// 必须是恰好含一个 string 键 input 的对象（JSON 解码默认拒绝重复键）。畸形/歧义
// 包装必须失败而不是被当成可执行的普通 function call 发出去。
func rspCustomInputDecode(rawArgs string) (string, bool) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(rawArgs), &obj); err != nil {
		return "", false
	}
	if len(obj) != 1 {
		return "", false
	}
	input, ok := obj["input"].(string)
	if !ok {
		return "", false
	}
	return input, true
}

// ---------------------------------------------------------------------------
// 内容拆分（input items 的 text/image parts → OpenAI 消息字段）
// ---------------------------------------------------------------------------

// rspTextParts 拆出 Responses 内容里的文本与图片块。文本压平成字符串（上游对
// 字符串最宽容）；只要出现图片就必须保留块数组结构，否则图片会丢。
// 返回（换行拼接的文本, OpenAI content blocks）。
func rspTextParts(content any) (string, []map[string]any) {
	if s, ok := content.(string); ok {
		return s, nil
	}
	parts, ok := content.([]any)
	if !ok {
		return "", nil
	}
	var texts []string
	var blocks []map[string]any
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		switch rspString(part["type"]) {
		case "input_text", "output_text", "text", "summary_text":
			if text := rspString(part["text"]); text != "" {
				texts = append(texts, text)
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
		case "input_image", "image_url":
			// Responses 允许 image_url 是字符串或 {url: ...}。
			url := part["image_url"]
			if url == nil {
				url = part["url"]
			}
			if m, ok := url.(map[string]any); ok {
				url = m["url"]
			}
			if s := rspString(url); s != "" {
				blocks = append(blocks, map[string]any{"type": "image_url", "image_url": map[string]any{"url": s}})
			}
		}
	}
	return strings.Join(texts, "\n"), blocks
}

// rspSplitToolOutput 拆出 function_call_output 的（文字, 图片块）。tool 消息的
// content 在 OpenAI 协议里只能是字符串放不下结构化图片：只取文字会静默丢图，
// 整段 repr 会把图片 base64 当文本分词撑爆上下文——文字留在 tool 消息里，
// 图片提出来并入紧随其后的 user 消息。
func rspSplitToolOutput(output any) (string, []map[string]any) {
	switch v := output.(type) {
	case string:
		return v, nil
	case []any:
		text, blocks := rspTextParts(v)
		var images []map[string]any
		for _, b := range blocks {
			if b["type"] == "image_url" {
				images = append(images, b)
			}
		}
		if text == "" && len(images) > 0 {
			text = "[图片]" // 上游要求 tool content 非空
		}
		return text, images
	case nil:
		return "", nil
	default:
		return fmt.Sprintf("%v", v), nil
	}
}

// ---------------------------------------------------------------------------
// reasoning item 文本提取（来源按可靠性排序）
// ---------------------------------------------------------------------------

// rspReasoningText 从 reasoning item 里取出推理文本：
//   - encrypted_content —— 我们发出去、客户端原样带回的凭据，**优先**用它：唯一能
//     还原完整推理原文的载体（summary 可能被客户端截断）；
//   - summary / content —— Responses 规范形态（parts 数组，Codex 无凭据时靠它带回）；
//   - reasoning_content / reasoning —— 已是扁平字符串的形态。
//
// 取不到返回空串（调用方据此仍挂字段+占位——上游要的是「字段存在且非空」）。
func rspReasoningText(item map[string]any) string {
	if dec, ok := rspDecodeCredential(rspString(item["encrypted_content"])); ok && dec != "" {
		return dec
	}
	for _, key := range []string{"summary", "content"} {
		if text, _ := rspTextParts(item[key]); text != "" {
			return text
		}
	}
	for _, key := range []string{"reasoning_content", "reasoning"} {
		if s := rspString(item[key]); s != "" {
			return s
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 请求转换：Responses → Chat Completions
// ---------------------------------------------------------------------------

// rspErr 请求转换错误（带 OpenAI 错误码，就地回客户端不进管线）。
type rspErr struct {
	status int
	code   string
	msg    string
}

func (e *rspErr) Error() string { return e.msg }

// responsesToChat 把 Responses 请求体转换为 Chat Completions 请求体。
//
// reasoning item **不能丢**（宽容挂载，不假设顺序）：累积起来落到下一条 assistant
// 消息上；中间隔着 user 的异常顺序也保留待挂；收尾时仍待挂则回填到已发出的
// 最后一条 assistant 上——宁可挂到一条不相关的 assistant 上（腾讯只要求
// assistant 消息上有该字段，多余内容无害），也不要丢掉凭据触发 11155。
// 连续多段 reasoning 拼接（客户端可能把一段推理拆成多个 item 下发）。
func responsesToChat(body map[string]any) (map[string]any, *rspErr) {
	bridge := newRspToolBridge(rspAsArray(body["tools"]))
	// stream 必须转告上游：下游管线（session.ParseRequest → chatCompletions 分流）
	// 靠它决定走 SSE 透传还是聚合；漏掉会得到一个"看起来很成功"的空回答。
	out := map[string]any{
		"model":  body["model"],
		"stream": rspBool(body["stream"]),
	}
	messages := make([]any, 0, 8)

	// instructions 是 Responses 里承载 system 的字段（不在 input 里）。
	switch instr := body["instructions"].(type) {
	case string:
		if strings.TrimSpace(instr) != "" {
			messages = append(messages, map[string]any{"role": "system", "content": instr})
		}
	case []any:
		if text, _ := rspTextParts(instr); text != "" {
			messages = append(messages, map[string]any{"role": "system", "content": text})
		}
	}

	switch in := body["input"].(type) {
	case string:
		if strings.TrimSpace(in) != "" {
			messages = append(messages, map[string]any{"role": "user", "content": in})
		}
	case []any:
		// 连续 function_call 合并进同一条 assistant 消息：OpenAI 要求一次 assistant
		// 回合里的多个工具调用同属一条消息，拆成多条会让上游 400。
		var pendingCalls []any
		// 待挂推理：nil=没见到 reasoning item；""=见到了但没文本（畸形输入——上游
		// 触发条件是字段存在，占位语义交给 rspAttachReasoning 的空格兜底）。
		var pendingReasoning *string
		flushCalls := func() {
			if len(pendingCalls) == 0 {
				return
			}
			msg := map[string]any{"role": "assistant", "content": nil, "tool_calls": pendingCalls}
			// 工具调用回合也可能带推理（模型先思考再调工具），一并保留，
			// 否则这类多轮同样会因缺痕迹被拒。
			if pendingReasoning != nil {
				rspAttachReasoning(msg, *pendingReasoning)
				pendingReasoning = nil
			}
			messages = append(messages, msg)
			pendingCalls = nil
		}
		for _, rawItem := range in {
			item, ok := rawItem.(map[string]any)
			if !ok {
				continue
			}
			kind := rspString(item["type"])
			switch kind {
			case "function_call", "custom_tool_call":
				rawArguments := item["arguments"]
				if kind == "custom_tool_call" {
					rawInput, ok := item["input"].(string)
					if !ok {
						return nil, &rspErr{status: http.StatusBadRequest, code: "invalid_request_error",
							msg: "Custom tool history input must be a string."}
					}
					wrapped, _ := json.Marshal(map[string]any{"input": rawInput})
					rawArguments = string(wrapped)
				}
				args, ok := rawArguments.(string)
				if !ok {
					// Responses 的 arguments 规范上是字符串；对象形态则序列化。
					if rawArguments == nil {
						args = "{}"
					} else {
						b, err := json.Marshal(rawArguments)
						if err != nil {
							return nil, &rspErr{status: http.StatusBadRequest, code: "invalid_request_error",
								msg: "tool call arguments serialization failed: " + err.Error()}
						}
						args = string(b)
					}
				}
				callID := rspString(item["call_id"])
				if callID == "" {
					callID = rspString(item["id"])
				}
				pendingCalls = append(pendingCalls, map[string]any{
					"id":   callID,
					"type": "function",
					"function": map[string]any{
						"name":      bridge.outboundName(item["name"]),
						"arguments": args,
					},
				})
				continue
			}
			flushCalls()

			switch kind {
			case "function_call_output", "custom_tool_call_output":
				outText, outImages := rspSplitToolOutput(item["output"])
				messages = append(messages, map[string]any{
					"role":         "tool",
					"tool_call_id": rspString(item["call_id"]),
					"content":      outText,
				})
				// 图片必须紧跟其后（顺序即语义：它们属于这次工具调用）。
				if len(outImages) > 0 {
					blocks := make([]any, 0, len(outImages))
					for _, b := range outImages {
						blocks = append(blocks, b)
					}
					messages = append(messages, map[string]any{"role": "user", "content": blocks})
				}
			case "reasoning":
				if text := rspReasoningText(item); text != "" {
					if pendingReasoning != nil {
						text = *pendingReasoning + text // 累积：连续多段拼接
					}
					pendingReasoning = &text
				} else if pendingReasoning == nil {
					// 客户端发了 reasoning 项但无可提取文本（畸形输入，正常客户端
					// 不会把空的 reasoning item 发回来）：记空串占位，不因这个字段
					// 丢掉整轮凭据。WARN 暴露畸形输入，真遇到 11155 时有据可查。
					empty := ""
					pendingReasoning = &empty
					log.Printf("WARN: [server] responses: reasoning item 无可提取文本 keys=%v，按上游口径补占位", rspKeys(item))
				}
			default:
				// message（或没写 type 的 {role, content}——宽容处理）。
				role := rspString(item["role"])
				if role == "" {
					role = "user"
				}
				if role == "developer" {
					role = "system"
				}
				text, blocks := rspTextParts(item["content"])
				var msg map[string]any
				hasImage := false
				for _, b := range blocks {
					if b["type"] == "image_url" {
						hasImage = true
						break
					}
				}
				switch {
				case hasImage:
					contentBlocks := make([]any, 0, len(blocks))
					for _, b := range blocks {
						contentBlocks = append(contentBlocks, b)
					}
					msg = map[string]any{"role": role, "content": contentBlocks}
				case text != "":
					msg = map[string]any{"role": role, "content": text}
				default:
					msg = nil
				}
				if msg != nil {
					// 只在 assistant 消息上挂推理（校验针对 assistant 回合，客户端
					// 把 reasoning 放别处时不硬塞，免得造出上游不认的组合）；
					// 非 assistant 时**保留**待挂状态留给后面那条 assistant。
					if role == "assistant" && pendingReasoning != nil {
						rspAttachReasoning(msg, *pendingReasoning)
						pendingReasoning = nil
					}
					messages = append(messages, msg)
				}
			}
		}
		flushCalls()
		// 收尾：推理项排在最后一条 assistant 之后时，循环里没有「下一条
		// assistant」可挂。回填到已发出的最后一条 assistant 上——它本来就是
		// 这一轮的推理（顺序异常不影响归属）。
		if pendingReasoning != nil {
			for i := len(messages) - 1; i >= 0; i-- {
				if msg, ok := messages[i].(map[string]any); ok && msg["role"] == "assistant" {
					rspAttachReasoning(msg, *pendingReasoning)
					break
				}
			}
		}
	}
	out["messages"] = messages

	if v := body["max_output_tokens"]; v != nil {
		out["max_tokens"] = v
	}
	for _, key := range []string{"temperature", "top_p"} {
		if v := body[key]; v != nil {
			out[key] = v
		}
	}
	if len(bridge.chatTools) > 0 {
		out["tools"] = bridge.chatTools
	}
	if choice := rspConvertToolChoice(body["tool_choice"], bridge); choice != nil {
		out["tool_choice"] = choice
	}
	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok {
			// 档位不整个透传 reasoning 对象（上游不认识），只转 reasoning_effort；
			// 不支持档位的降级交给 payload.go normalizeReasoningEffort 管线。
			out["reasoning_effort"] = effort
		}
	}
	// prompt_cache_key 透传（费用差约 17 倍）：内部管线的优先级 1 就是「客户端
	// 已带则原值保留」（payload.go prepareBodyPass / cache_key.go InjectPromptCacheKey
	// 均有钉死测试），此处原样带出即可，不覆盖不加工。
	if s := rspString(body["prompt_cache_key"]); s != "" {
		out["prompt_cache_key"] = s
	}
	// store/include 等 OpenAI 专有字段上游不消费，不透传。
	return out, nil
}

// rspConvertToolChoice Responses 工具选择 → Chat 形态：字符串透传；具名对象映射
// 出站名；其余 nil = 不设置（交给 payload.go normalizeToolChoice 兜底）。
func rspConvertToolChoice(choice any, bridge *rspToolBridge) any {
	switch c := choice.(type) {
	case string:
		if c != "" {
			return c
		}
	case map[string]any:
		name := rspString(c["name"])
		if name == "" {
			if fn, ok := c["function"].(map[string]any); ok {
				name = rspString(fn["name"])
			}
		}
		if name == "" {
			if cu, ok := c["custom"].(map[string]any); ok {
				name = rspString(cu["name"])
			}
		}
		if name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": bridge.outboundName(name)}}
		}
	}
	return nil
}

// rspAsArray any → []any（非数组返回 nil）。
func rspAsArray(v any) []any {
	arr, _ := v.([]any)
	return arr
}

// rspBool any → bool（非 bool 返回 false）。
func rspBool(v any) bool {
	b, _ := v.(bool)
	return b
}

// rspKeys 列出 map 的键（畸形输入 WARN 日志用）。
func rspKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// ---------------------------------------------------------------------------
// 响应转换：Chat Completions → Responses（非流式）
// ---------------------------------------------------------------------------

// rspUsageObject usage 字段按 Responses 口径映射。input_tokens 在 OpenAI 语义里
// **已包含**命中缓存的 token，缓存数单独放 input_tokens_details.cached_tokens
// ——客户端会把它从 input 里减掉再显示，不能在这里先减。缓存/推理明细优先取
// details 对象，回落本网关上游的扁平别名（prompt_cache_hit_tokens 等）。
func rspUsageObject(usage map[string]any) map[string]any {
	prompt := rspInt(usage["prompt_tokens"])
	completion := rspInt(usage["completion_tokens"])
	cached := rspInt(usage["prompt_cache_hit_tokens"])
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		if v := rspInt(details["cached_tokens"]); v > 0 {
			cached = v
		}
	}
	reasoning := rspInt(usage["reasoning_tokens"])
	if details, ok := usage["completion_tokens_details"].(map[string]any); ok {
		if v := rspInt(details["reasoning_tokens"]); v > 0 {
			reasoning = v
		}
	}
	return map[string]any{
		"input_tokens":          prompt,
		"input_tokens_details":  map[string]any{"cached_tokens": cached},
		"output_tokens":         completion,
		"output_tokens_details": map[string]any{"reasoning_tokens": reasoning},
		"total_tokens":          prompt + completion,
	}
}

// rspInt JSON 数值（float64）→ int；其他类型 0。
func rspInt(v any) int {
	f, ok := v.(float64)
	if !ok {
		return 0
	}
	return int(f)
}

// rspMessageItem 输出项：assistant 文本消息。
func rspMessageItem(text, itemID string) map[string]any {
	return map[string]any{
		"type":   "message",
		"id":     itemID,
		"status": "completed",
		"role":   "assistant",
		"content": []any{map[string]any{
			"type": "output_text", "text": text, "annotations": []any{},
		}},
	}
}

// rspFunctionCallItem 输出项：function 调用。
func rspFunctionCallItem(callID, itemID, name, arguments string) map[string]any {
	return map[string]any{
		"type": "function_call", "id": itemID, "call_id": callID,
		"name": name, "arguments": arguments, "status": "completed",
	}
}

// rspCustomCallItem 输出项：custom tool 调用（input 是原始文本）。
func rspCustomCallItem(callID, itemID, name, rawInput string) map[string]any {
	return map[string]any{
		"type": "custom_tool_call", "id": itemID, "call_id": callID,
		"name": name, "input": rawInput,
	}
}

// rspReasoningItem 推理输出项：summary 给人看，encrypted_content 供客户端回传。
// 两个字段都要有，少任何一个都会出问题：只有 summary → 客户端在 store:false 下
// 没有可回传的凭据，丢掉整项；只有凭据 → 界面上看不到思考过程。
func rspReasoningItem(text, itemID string) map[string]any {
	return map[string]any{
		"type": "reasoning", "id": itemID,
		"summary":           []any{map[string]any{"type": "summary_text", "text": text}},
		"encrypted_content": rspEncodeCredential(text),
	}
}

// rspArgsString 取 function.arguments 统一成字符串（Responses 里就是字符串）。
// 非法 JSON 原样保留：那是上游给的内容，改成 {} 会让客户端以为「模型决定不传参」，
// 反而把问题藏起来。
func rspArgsString(fn map[string]any) string {
	if fn == nil {
		return "{}"
	}
	switch v := fn["arguments"].(type) {
	case string:
		return v
	case nil:
		return "{}"
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "{}"
		}
		return string(b)
	}
}

// chatToResponses 把 Chat Completions 非流式响应转换为 Responses 响应体。
// model 回填**用户请求的名字**（不是映射后的上游名），否则客户端会认为
// 「我请求的模型被换掉了」。
func chatToResponses(data map[string]any, model, respID string, bridge *rspToolBridge) map[string]any {
	if bridge == nil {
		bridge = newRspToolBridge(nil)
	}
	choice := map[string]any{}
	if choices := rspAsArray(data["choices"]); len(choices) > 0 {
		if c, ok := choices[0].(map[string]any); ok && c != nil {
			choice = c
		}
	}
	message, _ := choice["message"].(map[string]any)
	if message == nil {
		message = map[string]any{}
	}
	finish := rspString(choice["finish_reason"])
	if finish == "" {
		finish = "stop"
	}

	var output []any
	// 推理项放最前（与流式顺序一致：思考在正文之前）。必须带凭据
	// encrypted_content，否则客户端在 store:false 下丢整项，下一轮不带推理
	// 痕迹 → 上游 11155。
	if reasoning := rspString(message["reasoning_content"]); reasoning != "" {
		output = append(output, rspReasoningItem(reasoning, rspID("rs")))
	}
	// length 截断的工具调用绝不能作为可执行输出暴露（参数可能是残缺 JSON，
	// 客户端解析成非法 JSON 卡死会话）——参考实现 suppress 全部 calls。
	suppressTools := false
	var callItems []any
	for _, rawCall := range rspAsArray(message["tool_calls"]) {
		call, ok := rawCall.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := call["function"].(map[string]any)
		rawName := ""
		if fn != nil {
			rawName = rspString(fn["name"])
		}
		_, isCustom := bridge.restore(rawName)
		if (isCustom || bridge.customNames[rawName]) && finish == "length" {
			suppressTools = true
			break
		}
		callID := rspString(call["id"])
		if callID == "" {
			callID = "call_" + session.NewMessageID()[:12]
		}
		if isCustom {
			// custom 包装解码失败时宁可空 input 也不伪装成合法调用。
			rawInput, _ := rspCustomInputDecode(rspArgsString(fn))
			callItems = append(callItems, rspCustomCallItem(callID, rspID("ctc"), rawName, rawInput))
			continue
		}
		callItems = append(callItems, rspFunctionCallItem(callID, rspID("fc"), rawName, rspArgsString(fn)))
	}
	if !suppressTools {
		output = append(output, callItems...)
	}
	if text := rspString(message["content"]); text != "" {
		output = append(output, rspMessageItem(text, rspID("msg")))
	}

	status := "completed"
	var incomplete any
	if finish == "length" {
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	}
	usageObj := map[string]any{}
	if u, ok := data["usage"].(map[string]any); ok && u != nil {
		usageObj = u
	}
	return map[string]any{
		"id":                  respID,
		"object":              "response",
		"created_at":          time.Now().Unix(),
		"status":              status,
		"model":               model,
		"output":              output,
		"output_text":         rspString(message["content"]),
		"parallel_tool_calls": true,
		"error":               nil,
		"incomplete_details":  incomplete,
		"usage":               rspUsageObject(usageObj),
	}
}

// ---------------------------------------------------------------------------
// 流式：Chat Completions SSE → Responses 事件流（状态机）
// ---------------------------------------------------------------------------

// maxRspSSEBuffer SSE 单行缓冲上限（与参考实现 MAX_SSE_BUFFER 同口径）：上游若
// 持续吐不含换行的数据，读缓冲会一直长下去直至吃光内存，超限即中止转发。
const maxRspSSEBuffer = 1 << 20

// rspStreamTranslator 把无结构的 Chat delta 流重建成 Responses 的有结构事件流。
//
// 需要状态机的原因：Responses 的流**有边界**——每个输出项要先 output_item.added、
// 增量若干次、再 output_item.done，且带递增 output_index；上游只给一串无结构
// delta，边界只能由我们在内容类型切换或流结束时推断。两条硬性要求（对着客户端
// 解析实现对齐）：
//   - 流必须以 response.completed 之类终止事件收尾——客户端没见到终止事件会按
//     「失败」处理（stream ended before a terminal response event），收尾事件不能省；
//   - 文本增量与 output_item.added 的 output_index 必须一致——解析器按
//     output_index 建立槽位，对不上时增量被静默丢弃（表现为「有回复但内容为空」，
//     比报错更难查）。
type rspStreamTranslator struct {
	model     string
	respID    string
	createdAt int64
	bridge    *rspToolBridge

	started  bool
	finished bool
	nextIdx  int
	items    []any

	reasonIdx int             // -1 = 未开
	reasonID  string
	reasonBuf strings.Builder

	textIdx   int // -1 = 未开
	textID    string
	textBuf   strings.Builder

	// toolSeq → 工具状态（上游可能并发给多个工具，各自编号；toolOrder 保序收尾）。
	tools     map[int]*rspToolState
	toolOrder []int
	// bufferToolAdded 请求带 custom 工具时置位：工具名可能分片到达、包装参数要等
	// 完整后才能校验解码，output_item.added 延迟到收尾统一补发。
	bufferToolAdded bool

	finishReason string
	usage        map[string]any
}

// rspToolState 单个工具调用的流式累积状态。
type rspToolState struct {
	index  int
	id     string
	callID string
	name   string
	args   strings.Builder
	closed bool
}

// newRspStreamTranslator 构造流式翻译器。
func newRspStreamTranslator(model, respID string, bridge *rspToolBridge) *rspStreamTranslator {
	if bridge == nil {
		bridge = newRspToolBridge(nil)
	}
	return &rspStreamTranslator{
		model: model, respID: respID, createdAt: time.Now().Unix(),
		bridge: bridge, reasonIdx: -1, textIdx: -1,
		tools:           map[int]*rspToolState{},
		usage:           map[string]any{},
		bufferToolAdded: len(bridge.customNames) > 0,
	}
}

// rspEventFrame Responses 的 SSE 帧：event 行 + 单行 data + 空行结尾。与 Chat
// Completions 的裸 data 不同，这里**必须**带 event 行——解析器按 event 分派，
// 只给 data 会让它拿不到类型而直接跳过。
func rspEventFrame(name string, payload map[string]any) []byte {
	b, err := json.Marshal(payload)
	if err != nil {
		b = []byte("{}")
	}
	return []byte("event: " + name + "\ndata: " + string(b) + "\n\n")
}

// created 生成 response.created 事件（首次 feed 或空流收尾时兜底发）。
func (t *rspStreamTranslator) created() []byte {
	t.started = true
	return rspEventFrame("response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id": t.respID, "object": "response", "created_at": t.createdAt,
			"status": "in_progress", "model": t.model, "output": []any{},
		},
	})
}

// openReason 开推理输出项。
func (t *rspStreamTranslator) openReason() []byte {
	t.reasonIdx = t.nextIdx
	t.nextIdx++
	t.reasonID = rspID("rs")
	return rspEventFrame("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": t.reasonIdx,
		"item": map[string]any{"type": "reasoning", "id": t.reasonID, "summary": []any{}},
	})
}

// closeReason 关推理输出项。收尾 item 与 output_item.added 的**不是同一个形状**：
// 凭据 encrypted_content 只能在这里补上——它要等推理全文到齐才能算出（客户端在
// output_item.done 上取这个字段，缺了下一轮不带推理痕迹 → 上游 11155）。
func (t *rspStreamTranslator) closeReason() []byte {
	if t.reasonIdx < 0 {
		return nil
	}
	idx := t.reasonIdx
	t.reasonIdx = -1
	item := rspReasoningItem(t.reasonBuf.String(), t.reasonID)
	t.items = append(t.items, item)
	return rspEventFrame("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": idx, "item": item,
	})
}

// openText 开文本输出项（output_item.added + content_part.added 两事件）。
func (t *rspStreamTranslator) openText() []byte {
	t.textIdx = t.nextIdx
	t.nextIdx++
	t.textID = rspID("msg")
	return append(
		rspEventFrame("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": t.textIdx,
			"item": map[string]any{
				"type": "message", "id": t.textID, "status": "in_progress",
				"role": "assistant", "content": []any{},
			},
		}),
		rspEventFrame("response.content_part.added", map[string]any{
			"type": "response.content_part.added", "output_index": t.textIdx,
			"content_index": 0, "item_id": t.textID,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})...,
	)
}

// closeText 关文本输出项（done 三连：output_text / content_part / item）。
func (t *rspStreamTranslator) closeText() []byte {
	if t.textIdx < 0 {
		return nil
	}
	idx := t.textIdx
	t.textIdx = -1
	item := rspMessageItem(t.textBuf.String(), t.textID)
	t.items = append(t.items, item)
	out := rspEventFrame("response.output_text.done", map[string]any{
		"type": "response.output_text.done", "output_index": idx,
		"content_index": 0, "item_id": t.textID, "text": t.textBuf.String(),
	})
	out = append(out, rspEventFrame("response.content_part.done", map[string]any{
		"type": "response.content_part.done", "output_index": idx,
		"content_index": 0, "item_id": t.textID,
		"part": map[string]any{"type": "output_text", "text": t.textBuf.String(), "annotations": []any{}},
	})...)
	out = append(out, rspEventFrame("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": idx, "item": item,
	})...)
	return out
}

// openTool 登记工具调用状态。
func (t *rspStreamTranslator) openTool(seq int, callID, name string) {
	t.tools[seq] = &rspToolState{
		index: t.nextIdx, id: rspID("fc"), callID: callID, name: name,
	}
	t.nextIdx++
	t.toolOrder = append(t.toolOrder, seq)
}

// toolAddedEvent 工具项的 output_item.added 事件。arguments 必须给空串（不能
// 省略）：客户端据此决定是否开始累积参数。
func (t *rspStreamTranslator) toolAddedEvent(state *rspToolState, name string) []byte {
	return rspEventFrame("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": state.index,
		"item": map[string]any{
			"type": "function_call", "id": state.id, "call_id": state.callID,
			"name": name, "arguments": "", "status": "in_progress",
		},
	})
}

// closeTools 关全部工具项：added（若此前缓冲）→ arguments/input delta → done →
// output_item.done。custom 工具在 length 截断时整项剔除（残缺包装参数绝不能作为
// 可执行输出暴露）。
func (t *rspStreamTranslator) closeTools() []byte {
	var out []byte
	for _, seq := range t.toolOrder {
		state := t.tools[seq]
		if state == nil || state.closed {
			continue
		}
		state.closed = true
		restoredName, isCustom := t.bridge.restore(state.name)
		// 「是不是 custom」与「取哪份入参」用同一个判据（restore 的注册表），
		// 不对称会出现判据说 custom、取值却按 function 的断流。
		value, field, family := state.args.String(), "arguments", "function_call_arguments"
		if isCustom {
			if decoded, ok := rspCustomInputDecode(value); ok {
				value = decoded
			}
			field, family = "input", "custom_tool_call_input"
			if t.finishReason == "length" {
				continue // length 截断的 custom 调用不暴露（不可执行）
			}
		}
		var item map[string]any
		if isCustom {
			item = rspCustomCallItem(state.callID, state.id, restoredName, value)
		} else {
			item = rspFunctionCallItem(state.callID, state.id, restoredName, value)
		}
		t.items = append(t.items, item)
		if t.bufferToolAdded {
			// 缓冲路径（custom 桥接）：added/delta 延迟到收尾补发——name 可能
			// 分片到达、包装参数要等全文才能校验解码。added 副本入参字段置空 +
			// in_progress（与参考实现同形状）。
			added := map[string]any{"type": "function_call", "id": state.id,
				"call_id": state.callID, "name": restoredName,
				field: "", "status": "in_progress"}
			out = append(out, rspEventFrame("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": state.index, "item": added,
			})...)
			out = append(out, rspEventFrame("response."+family+".delta", map[string]any{
				"type": "response." + family + ".delta", "output_index": state.index,
				"item_id": state.id, "delta": value,
			})...)
		}
		out = append(out, rspEventFrame("response."+family+".done", map[string]any{
			"type": "response." + family + ".done", "output_index": state.index,
			"item_id": state.id, field: value,
		})...)
		out = append(out, rspEventFrame("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": state.index, "item": item,
		})...)
	}
	return out
}

// feed 喂一个上游 SSE data 对象，返回要下发的事件字节（可能为空）。收尾后不再
// 吐事件：上游若在 finish_reason 之后继续发内容帧，客户端会收到终止事件之后的
// 事件，协议被污染。
func (t *rspStreamTranslator) feed(obj map[string]any) []byte {
	if t.finished {
		return nil
	}
	var out []byte
	if !t.started {
		out = append(out, t.created()...)
	}
	if usage, ok := obj["usage"].(map[string]any); ok && usage != nil {
		for k, v := range usage {
			t.usage[k] = v
		}
	}
	choices := rspAsArray(obj["choices"])
	if len(choices) == 0 {
		return out
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return out
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		// 有的上游把完整消息放在 message 里（非 delta）：整条按 delta 处理。
		if m, ok := choice["message"].(map[string]any); ok {
			delta = m
		} else {
			delta = map[string]any{}
		}
	}

	// 推理增量：单独成 reasoning 输出项——它与正文是两个 item，混在一起会让
	// 客户端的思考区与正文区错位。
	if rc := rspString(delta["reasoning_content"]); rc != "" {
		if t.reasonIdx < 0 {
			out = append(out, t.closeText()...)
			out = append(out, t.openReason()...)
		}
		t.reasonBuf.WriteString(rc)
		out = append(out, rspEventFrame("response.reasoning_summary_text.delta", map[string]any{
			"type": "response.reasoning_summary_text.delta", "output_index": t.reasonIdx,
			"item_id": t.reasonID, "delta": rc,
		})...)
	}

	// 正文增量。
	if text := rspString(delta["content"]); text != "" {
		if t.reasonIdx >= 0 {
			out = append(out, t.closeReason()...)
		}
		if t.textIdx < 0 {
			out = append(out, t.openText()...)
		}
		t.textBuf.WriteString(text)
		out = append(out, rspEventFrame("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "output_index": t.textIdx,
			"content_index": 0, "item_id": t.textID, "delta": text,
		})...)
	}

	// 工具调用增量（按 index 分槽累积；参数分片原样透传，拼接语义与 Chat 一致）。
	for _, rawCall := range rspAsArray(delta["tool_calls"]) {
		call, ok := rawCall.(map[string]any)
		if !ok {
			continue
		}
		seq := rspInt(call["index"])
		fn, _ := call["function"].(map[string]any)
		name := ""
		if fn != nil {
			name = rspString(fn["name"])
		}
			state, exists := t.tools[seq]
			if !exists {
				if len(t.tools) >= 256 {
					// 正常一次回答只有几个 item；上游畸形数据不无限追加对象。
					log.Printf("WARN: [server] responses: 工具调用数量超过 256，忽略后续项")
					continue
				}
				if t.textIdx >= 0 {
					out = append(out, t.closeText()...)
				}
				if t.reasonIdx >= 0 {
					out = append(out, t.closeReason()...)
				}
				callID := rspString(call["id"])
				if callID == "" {
					callID = "call_" + session.NewMessageID()[:12]
				}
				t.openTool(seq, callID, name)
				state = t.tools[seq]
				if !t.bufferToolAdded {
					out = append(out, t.toolAddedEvent(state, state.name)...)
				}
			} else if name != "" && name != state.name {
				// 名字分片：首片可能为空（后续补名）或重名分片（续拼）。
				if state.name == "" || len(name) > len(state.name) && strings.HasPrefix(name, state.name) {
					state.name = name
				} else {
					state.name += name
				}
			}
			if fn != nil {
				if args := rspString(fn["arguments"]); args != "" {
					state.args.WriteString(args)
					// 非 custom 桥接：参数分片原样透传（拼接交给客户端），与 Chat
					// 流式语义对齐。custom 桥接缓冲到收尾（完整包装要等全文才能
					// 校验解码，见 closeTools）。
					if !t.bufferToolAdded {
						out = append(out, rspEventFrame("response.function_call_arguments.delta", map[string]any{
							"type": "response.function_call_arguments.delta",
							"output_index": state.index, "item_id": state.id, "delta": args,
						})...)
					}
				}
			}
	}

	if fr := rspString(choice["finish_reason"]); fr != "" {
		out = append(out, t.finish(fr, false)...)
	}
	return out
}

// finish 收尾：关掉打开的输出项并下发终止事件（幂等）。force 时上游出现过工具
// 调用却没给 finish_reason 也按 tool_calls 收尾——漏掉 arguments.done 会让客户端
// 的参数累积停在半截。
func (t *rspStreamTranslator) finish(finishReason string, force bool) []byte {
	if t.finished {
		return nil
	}
	t.finished = true
	if finishReason == "" && force && len(t.toolOrder) > 0 {
		finishReason = "tool_calls"
	}
	t.finishReason = finishReason
	var out []byte
	if !t.started {
		out = append(out, t.created()...)
	}
	out = append(out, t.closeText()...)
	out = append(out, t.closeReason()...)
	out = append(out, t.closeTools()...)

	eventName, status := "response.completed", "completed"
	var incomplete any
	if finishReason == "length" {
		eventName, status = "response.incomplete", "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	}
	out = append(out, rspEventFrame(eventName, map[string]any{
		"type": eventName,
		"response": map[string]any{
			"id": t.respID, "object": "response", "created_at": t.createdAt,
			"status": status, "model": t.model, "output": t.items,
			"incomplete_details": incomplete, "error": nil,
			"usage": rspUsageObject(t.usage),
		},
	})...)
	return out
}

// rspStream 流式主循环：读 chatCompletions 透传出的 Chat SSE 帧，翻译成 Responses
// 事件流写出（SSE 头与 200 状态码在此设置）。流内 error 帧（上游中途报错）以
// response.failed 收尾——流一旦以 200 开始状态码就固定了，错误只能裹进终止事件。
// 返回 error 仅供观测（客户端断连/缓冲超限），不改变已发出的字节。
func rspStream(w http.ResponseWriter, r io.Reader, translator *rspStreamTranslator) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	flush := func(b []byte) error {
		if len(b) == 0 {
			return nil
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	// Scanner 带 Buffer 上限：单帧超过 maxRspSSEBuffer（如上游持续吐不含换行的
	// 数据）时 Scan 返回 false 并置 ErrTooLong——防读缓冲无限膨胀吃光内存。
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxRspSSEBuffer)
	errorText := ""
	for sc.Scan() {
		trimmed := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(payload), &obj) != nil || obj == nil {
			continue
		}
		// 上游可能**中途**回 error 帧（形如 {"error":{...}}，无 choices）。
		if errObj, ok := obj["error"].(map[string]any); ok && len(rspAsArray(obj["choices"])) == 0 {
			msg := rspString(errObj["message"])
			if msg == "" {
				b, _ := json.Marshal(errObj)
				msg = string(b)
			}
			if len(msg) > 500 {
				msg = msg[:500]
			}
			errorText = msg
			break
		}
		if werr := flush(translator.feed(obj)); werr != nil {
			// 客户端断连：写失败即终止（人已走，终止事件没有意义）。
			return werr
		}
	}
	if serr := sc.Err(); serr != nil {
		if errors.Is(serr, bufio.ErrTooLong) {
			log.Printf("WARN: [server] responses: SSE 缓冲超过 %d 字节仍未见换行，中止转发", maxRspSSEBuffer)
			errorText = "上游响应异常：数据流缺少分隔"
		} else {
			_ = flush(translator.finish("", true))
			return serr
		}
	}
	if errorText != "" {
		// 收尾事件已发出就不能再报错（客户端已按成功处理），所以发
		// response.failed、不走 finish()。
		return flush(rspEventFrame("response.failed", map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": translator.respID, "object": "response",
				"created_at": translator.createdAt, "status": "failed",
				"model": translator.model, "output": translator.items,
				"error": map[string]any{"code": "upstream_error", "message": errorText},
			},
		}))
	}
	return flush(translator.finish("", true))
}

// ---------------------------------------------------------------------------
// 内部转发 + HTTP 入口（/v1/responses 与 /responses 共用）
// ---------------------------------------------------------------------------

// rspInnerWriter 内部转发用的 ResponseWriter：捕获 chatCompletions 写出的状态码
// 与响应头，body 字节经 io.Pipe 同步交给主 goroutine（io.Pipe 无缓冲——写端阻塞
// 到读端消费，天然背压，流式翻译零额外缓冲）。
type rspInnerWriter struct {
	header   http.Header
	status   int
	pw       *io.PipeWriter
	sig      chan struct{}
	sigOnce  sync.Once
}

func newRspInnerWriter(pw *io.PipeWriter) *rspInnerWriter {
	return &rspInnerWriter{header: http.Header{}, pw: pw, sig: make(chan struct{})}
}

func (w *rspInnerWriter) signal() { w.sigOnce.Do(func() { close(w.sig) }) }

func (w *rspInnerWriter) Header() http.Header { return w.header }

func (w *rspInnerWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.signal()
}

func (w *rspInnerWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK // 隐式 200（SSE 透传路径从不显式 WriteHeader）
	}
	w.signal()
	return w.pw.Write(b)
}

// Flush 空实现：io.Pipe 写是同步的（读端消费才返回），无需 flush 传播。
// chatCompletions 流式路径的 http.Flusher 断言落空后照常逐帧写出。
func (w *rspInnerWriter) Flush() {}

// handleResponses /v1/responses 与 /responses 的共享入口。
func (h *Handler) handleResponses(w http.ResponseWriter, r *http.Request) {
	// 请求体上限沿用 MaxBytesReader 口径（与 chatCompletions 同一 maxBodyBytes）。
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBodyBytes()))
	if err != nil {
		status, code := http.StatusBadRequest, "invalid_request"
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			status, code = http.StatusRequestEntityTooLarge, "request_too_large"
		}
		writeOpenAIError(w, status, code, "read body: "+err.Error())
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil || req == nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error",
			"request body must be a JSON object")
		return
	}
	model := rspString(req["model"])
	if strings.TrimSpace(model) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_model", "model 必须是字符串")
		return
	}
	// stream 不在此处消费：responsesToChat 已转告上游（out["stream"]），
	// chatCompletions 自行从 body 解析并分流 SSE/聚合路径。

	chat, rerr := responsesToChat(req)
	if rerr != nil {
		writeOpenAIError(w, rerr.status, rerr.code, rerr.msg)
		return
	}
	if len(rspAsArray(chat["messages"])) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "empty_input",
			"input 为空：Responses 请求必须带 input 或 instructions")
		return
	}
	// bridge 与请求转换各建一份（工具数组通常很小）：响应侧还原客户端原名
	// （namespace 子工具/重名改名/custom 桥接）。漏传时非流式会把内部名原样发给
	// 客户端，同一请求换 stream 标志就得到两套工具名。
	bridge := newRspToolBridge(rspAsArray(req["tools"]))
	chatBody, err := json.Marshal(chat)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error",
			"request conversion failed: "+err.Error())
		return
	}

	// 内部转发：转换后的 chat body 喂给 chatCompletions（复用选号/轮转/降级/
	// usage 采集/请求日志/prompt_cache_key 注入，不新开上游调用路径）。
	pr, pw := io.Pipe()
	iw := newRspInnerWriter(pw)
	innerReq := r.Clone(r.Context()) // 复用请求 ctx：客户端断连立即中断在途上游调用
	innerReq.URL.Path = "/v1/chat/completions"
	innerReq.ContentLength = int64(len(chatBody))
	innerReq.Body = io.NopCloser(bytes.NewReader(chatBody))
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer pw.Close() // chatCompletions 返回后关写端 → 读端 EOF
		h.chatCompletions(iw, innerReq)
	}()

	respID := rspID("resp")
	<-iw.sig // 状态码就绪（错误信封 / 首个数据帧隐式 200）
	switch {
	case iw.status != http.StatusOK:
		// 错误在**开流前**用真实状态码回掉：错误体是 chatCompletions 的 OpenAI
		// 错误信封（含上游原文透传与 gateway_hint），Responses 客户端同口径解析。
		errBody, _ := io.ReadAll(pr) // 排空（直到 pw 关闭的 EOF）
		<-done
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(iw.status)
		_, _ = w.Write(errBody)
	case strings.Contains(iw.header.Get("Content-Type"), "text/event-stream"):
		translator := newRspStreamTranslator(model, respID, bridge)
		if serr := rspStream(w, pr, translator); serr != nil {
			log.Printf("DEBUG: [server] responses stream write error (client likely disconnected): %v", serr)
		}
		_ = pr.Close() // 关读端：chatCompletions 在途 pipe 写立刻失败（客户端已走）
		<-done
	default:
		// 非流式：聚合后的 chat.completion JSON → Responses 对象。
		data, _ := io.ReadAll(pr)
		<-done
		var resp map[string]any
		if json.Unmarshal(data, &resp) != nil || resp == nil {
			// 200 但响应体不是 JSON：不能当作「成功但空回答」返回——那正是最难
			// 排查的一种表现（客户端不重试、不报错）。显式 502 让问题可见。
			text := string(data)
			if len(text) > 300 {
				text = text[:300]
			}
			writeOpenAIError(w, http.StatusBadGateway, "upstream_invalid_body",
				"上游返回了无法解析的响应："+text)
			return
		}
		writeJSON(w, http.StatusOK, chatToResponses(resp, model, respID, bridge))
	}
}
