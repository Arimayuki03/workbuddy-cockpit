// anthropic.go Anthropic Messages API 兼容层（/v1/messages 与 /v1/messages/count_tokens）。
//
// 为什么需要：上游（腾讯 CodeBuddy）只提供 OpenAI 兼容接口，但 Claude Code 等
// 一大批客户端只认 Anthropic 的 Messages 协议——协议不同不是配置问题：
//
//	请求   · system 是顶层字段（不在 messages 里）
//	       · max_tokens 必填（OpenAI 可选）
//	       · content 可以是字符串，也可以是 block 数组
//	       · 工具是 {name, input_schema}，没有 type:function 那层包装
//	响应   · content 是 block 数组（文本与工具调用同处一个列表）
//	       · stop_reason 是 end_turn 而非 stop
//	流式   · 带 event: 行的结构化事件流（message_start / content_block_delta / …）
//
// 架构：客户端 ──Anthropic 协议──▶ [本文件：翻译] ──OpenAI 协议──▶ chatCompletions
// 既有管线 ──▶ 上游。本文件只做协议双向翻译，其余（鉴权、选号、轮转、降级、
// reasoning 回填、usage 采集、metrics 与请求日志）全部复用管线既有实现——那是
// 网关真正的价值，不能因为多一个协议就走一套新逻辑。转换层是纯函数（流式状态机
// 除外），便于单测。
package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// mountAnthropic 挂载 Anthropic 协议路由（由主代理在 NewHandler 接线调用）。
// 两个端点共用一个 withAuth 包装：与 /v1/chat/completions 完全同口径——同一把
// api_key、同一个 httpauth.VerifyBearer（含会话 cookie 等价通道）。count_tokens
// 也不例外：只挑个能过的密钥就放行会把停用/过期/配额约束全漏掉，这是参考实现
// 明确踩过的坑。
func (h *Handler) mountAnthropic(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/messages", h.withAuth(h.anthropicMessages))
	mux.HandleFunc("POST /v1/messages/count_tokens", h.withAuth(h.anthropicCountTokens))
}

// ---------------------------------------------------------------------------
// 错误体与口径换算
// ---------------------------------------------------------------------------

// writeAnthropicError 写 Anthropic 风格错误体。与 OpenAI 的区别不只是字段名：
// Anthropic 把错误包在 {"type":"error","error":{...}} 里，客户端按这个结构解析，
// 共用 writeOpenAIError 会让 SDK 读不到错误信息。
func writeAnthropicError(w http.ResponseWriter, status int, errType, msg string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": msg,
		},
	})
}

// anthAsInt 把 JSON 数字安全转 int。上游是外部进程，usage 可能畸形（字符串/null/
// 嵌套对象）；转不动按 0 计——在流式生成器内部抛异常会直接掐断流，客户端连
// message_stop 都收不到（参考实现记录过这类事故）。兼容 int 家族：finish 收尾
// 构造的 usage map 值是 int（非 JSON 反序列化的 float64），同一条换算路径都要能走。
func anthAsInt(v any) int {
	switch n := v.(type) {
	case float64:
		if n != n { // NaN 防御
			return 0
		}
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

// anthUsageFields 把上游 usage 映射成 Anthropic 口径的字段。
//
// 两个口径的方向相反（社区实测踩过）：
//   - OpenAI/上游：prompt_tokens 已包含命中的缓存 token（另在
//     prompt_cache_hit_tokens 给出命中数供摊销展示）；
//   - Anthropic：input_tokens 不含缓存——缓存拆成 cache_read_input_tokens 与
//     cache_creation_input_tokens 两个独立字段，客户端统计总量时会把
//     input + cache_read + cache_creation 相加。
//
// 所以不能把 prompt_tokens 直接当 input_tokens 发：那样相加时缓存被算两遍
// （实测 1024 命中被计成 2048）。必须减去命中/写入量。钳到非负：上游若给畸形
// 命中数（> prompt），相减得负会让客户端算出离谱的上下文余量。
func anthUsageFields(usage map[string]any) map[string]any {
	prompt := anthAsInt(usage["prompt_tokens"])
	completion := anthAsInt(usage["completion_tokens"])
	hit := anthAsInt(usage["prompt_cache_hit_tokens"])
	write := anthAsInt(usage["prompt_cache_write_tokens"])
	return map[string]any{
		"input_tokens":                max(0, prompt-hit-write),
		"cache_read_input_tokens":     hit,
		"cache_creation_input_tokens": write,
		"output_tokens":               completion,
	}
}

// anthUsageMap 从对象里取 usage/error 子对象（形态不对返回 nil）。
func anthUsageMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// stopReasonOf OpenAI finish_reason → Anthropic stop_reason。
// sawTool：出现过工具调用但上游没给 finish_reason 时按 tool_use 收尾更贴近实际
// （流式 force 收尾路径用）。
func stopReasonOf(finish string, sawTool bool) string {
	switch finish {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "end_turn"
	}
	if sawTool {
		return "tool_use"
	}
	return "end_turn"
}

// ---------------------------------------------------------------------------
// 请求转换：Anthropic → OpenAI Chat Completions
// ---------------------------------------------------------------------------

// anthTextOf 把 Anthropic 的 content（字符串或 block 数组）压成纯文本。
// 只认 text block；tool_result 的 content（字符串或子 block 数组）同款处理。
func anthTextOf(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	blocks, ok := content.([]any)
	if !ok {
		return ""
	}
	var out []string
	for _, b := range blocks {
		if m, ok := b.(map[string]any); ok {
			if t, _ := m["type"].(string); t == "text" {
				if s, _ := m["text"].(string); s != "" {
					out = append(out, s)
				}
			}
		}
	}
	return strings.Join(out, "\n")
}

// anthImageURL Anthropic 图片块 source → OpenAI image_url.url。
// base64 形态转 data URL；url 形态原样；不认识的形态返回空串（调用方跳过——
// 不把垃圾形态序列化成文本喂上游）。
func anthImageURL(source any) string {
	m, ok := source.(map[string]any)
	if !ok {
		return ""
	}
	switch t, _ := m["type"].(string); t {
	case "base64":
		media, _ := m["media_type"].(string)
		if media == "" {
			media = "image/png"
		}
		data, _ := m["data"].(string)
		if data == "" {
			return ""
		}
		return "data:" + media + ";base64," + data
	case "url":
		u, _ := m["url"].(string)
		return u
	}
	return ""
}

// anthToSlice any → []any 的窄化（nil 安全）。
func anthToSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// toOpenAIRequest Anthropic 请求体 → OpenAI Chat Completions 请求体。
//
// 最绕的一处是工具结果：Anthropic 把它当作 user 消息里的一个 block，而 OpenAI
// 要求它是独立的 role:tool 消息。因此一条 Anthropic 消息可能被拆成多条 OpenAI
// 消息（工具结果在前，剩余文本在后）。
func toOpenAIRequest(body map[string]any) map[string]any {
	out := map[string]any{
		"model":  body["model"],
		"stream": body["stream"] == true,
	}
	var messages []any

	// system 是顶层字段 → 转成首条 system 消息（字符串与 block 数组两种形态都认）。
	if sys := body["system"]; sys != nil {
		if text := anthTextOf(sys); text != "" {
			messages = append(messages, map[string]any{"role": "system", "content": text})
		}
	}

	for _, raw := range anthToSlice(body["messages"]) {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		content := msg["content"]

		// 纯字符串 content：直通（多数上游对字符串更宽容）。
		if s, ok := content.(string); ok {
			messages = append(messages, map[string]any{"role": role, "content": s})
			continue
		}
		blocks, ok := content.([]any)
		if !ok {
			continue
		}

		var parts []any          // 文本/图片（本条消息的常规内容）
		var toolCalls []any      // assistant 发起的工具调用
		var toolResults []any    // user 回传的工具结果 → 拆成独立 role:tool 消息
		var toolImages []any     // 工具结果里的图片 → 提升到后续 user 消息
		var thinkingText *string // 本条 thinking 块的推理文本（nil = 没见到块）
		for _, rawBlock := range blocks {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				continue
			}
			switch kind, _ := block["type"].(string); kind {
			case "text":
				t, _ := block["text"].(string)
				parts = append(parts, map[string]any{"type": "text", "text": t})
			case "image":
				if u := anthImageURL(block["source"]); u != "" {
					parts = append(parts, map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": u},
					})
				}
			case "thinking", "redacted_thinking":
				// 推理块不能丢：上游对 DeepSeek 要求多轮回传推理内容，丢了会被拒
				// （11155 reasoning_content_missing）→ 记账号失败 → 连败出池 →
				// 客户端重试变成与模型无关的 503 死循环。
				// 累积而非覆盖：一条 assistant 可能有多个 thinking 块（分段推理/
				// redacted + 明文并存），只留最后一个会把前文丢掉。
				var txt string
				if t, ok := block["thinking"].(string); ok && t != "" {
					txt = t
				} else if t, ok := block["data"].(string); ok {
					// redacted_thinking 只有密文：字段存在就是上游回填管线的触发条件，
					// 带上密文比丢掉安全（密文不含可识别的自然语言指纹）。
					txt = t
				}
				if thinkingText == nil {
					thinkingText = &txt
				} else {
					merged := *thinkingText + txt
					thinkingText = &merged
				}
			case "tool_use":
				id, _ := block["id"].(string)
				name, _ := block["name"].(string)
				// OpenAI 的 arguments 是字符串，Anthropic 的 input 是对象。
				args, _ := json.Marshal(block["input"])
				if args == nil {
					args = []byte("{}")
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   id,
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": string(args),
					},
				})
			case "tool_result":
				// OpenAI 的 role:tool 消息 content 只能是字符串，图片物理上无处安放。
				// 若把整段 base64 序列化成文本发给上游，几 MB 的图会被按百万级 token
				// 计数（实测 prompt too long: 2349045 > 1048576），上下文瞬间撑爆且
				// 会话无法恢复。正确做法：tool 消息只留文字，图片提升到紧随其后的
				// user 消息——与人类上传的图片同等对待，上游能识别为视觉输入。
				rawResult := block["content"]
				resultText := anthTextOf(rawResult)
				var resultImages []any
				if subs, ok := rawResult.([]any); ok {
					for _, sub := range subs {
						if m, ok := sub.(map[string]any); ok {
							if t, _ := m["type"].(string); t == "image" {
								if u := anthImageURL(m["source"]); u != "" {
									resultImages = append(resultImages, map[string]any{
										"type":      "image_url",
										"image_url": map[string]any{"url": u},
									})
								}
							}
						}
					}
				}
				// 既无文字也无图片：未知形态仍以文本透出（保留参考实现的兜底，
				// 不编造结构）；只有图片时 tool content 用占位符——上游要求非空。
				if resultText == "" && len(resultImages) == 0 {
					if rawResult != nil {
						resultText = fmt.Sprintf("%v", rawResult)
					}
				} else if resultText == "" {
					resultText = "[图片]"
				}
				toolImages = append(toolImages, resultImages...)
				tid, _ := block["tool_use_id"].(string)
				toolResults = append(toolResults, map[string]any{
					"role":         "tool",
					"tool_call_id": tid,
					"content":      resultText,
				})
			}
		}

		// 工具结果必须先于本条的其余内容（它们对应上一轮 assistant 的调用）。
		messages = append(messages, toolResults...)
		// 工具结果里的图片提升到这里：并入同一条 user 消息。
		parts = append(toolImages, parts...)

		if len(toolCalls) > 0 {
			// assistant 工具调用消息：content 通常为空，但保留文本更稳
			// （部分上游要求非 null）。
			var texts []string
			for _, p := range parts {
				if m, ok := p.(map[string]any); ok {
					if t, _ := m["type"].(string); t == "text" {
						if s, _ := m["text"].(string); s != "" {
							texts = append(texts, s)
						}
					}
				}
			}
			m := map[string]any{
				"role":       "assistant",
				"tool_calls": toolCalls,
				"content":    strings.Join(texts, "\n"),
			}
			if thinkingText != nil {
				m["reasoning_content"] = *thinkingText
			}
			messages = append(messages, m)
		} else if len(parts) > 0 || thinkingText != nil {
			var m map[string]any
			// 只有一个纯文本块时压平为字符串——多数上游对字符串更宽容。
			if len(parts) == 1 {
				if pm, ok := parts[0].(map[string]any); ok {
					if t, _ := pm["type"].(string); t == "text" {
						m = map[string]any{"role": role, "content": pm["text"]}
					}
				}
			}
			if m == nil {
				if len(parts) > 0 {
					m = map[string]any{"role": role, "content": parts}
				} else {
					// 本条只有 thinking 块、没有正文：仍要落一条消息把推理痕迹带上，
					// 否则这段推理无处安放、等于又丢掉了。
					m = map[string]any{"role": role, "content": nil}
				}
			}
			// 只在 assistant 消息上挂 reasoning_content——上游的校验针对 assistant
			// 回合；挂在别处会造成上游不认的组合，那比不挂更糟。
			if thinkingText != nil && role == "assistant" {
				m["reasoning_content"] = *thinkingText
			}
			messages = append(messages, m)
		}
	}
	out["messages"] = messages

	// max_tokens / temperature / top_p：名字相同直接透传（max_tokens 缺失由调用方校验）。
	if v, ok := body["max_tokens"]; ok && v != nil {
		out["max_tokens"] = v
	}
	for _, k := range []string{"temperature", "top_p"} {
		if v, ok := body[k]; ok && v != nil {
			out[k] = v
		}
	}
	// 推理档位：output_config.effort（显式档位）优先于 thinking.budget_tokens
	// （按预算分档）。不映射的话用户选了档位却拿到上游默认档，且无从自查。
	if effort := anthReasoningEffort(body); effort != "" {
		out["reasoning_effort"] = effort
	}
	if ss := anthToSlice(body["stop_sequences"]); len(ss) > 0 {
		out["stop"] = ss
	}
	// metadata.user_id → OpenAI user（上游按它做用户维度的缓存/风控分桶）。
	if meta, ok := body["metadata"].(map[string]any); ok {
		if uid, _ := meta["user_id"].(string); uid != "" {
			out["user"] = uid
		}
	}

	// tools：{name, input_schema} → OpenAI 的 type:function 包装层。
	if tools := anthToSlice(body["tools"]); len(tools) > 0 {
		var outTools []any
		for _, raw := range tools {
			t, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			name, _ := t["name"].(string)
			if name == "" {
				continue
			}
			desc, _ := t["description"].(string)
			schema := t["input_schema"]
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			outTools = append(outTools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        name,
					"description": desc,
					"parameters":  schema,
				},
			})
		}
		if len(outTools) > 0 {
			out["tools"] = outTools
		}
	}

	// tool_choice 映射。"none" 是 OpenAI 合法字符串值；真正删 tools 的动作交给
	// 下游 payload 管线 normalizeToolChoice（单一事实来源，不重复实现）。
	if choice, ok := body["tool_choice"].(map[string]any); ok {
		switch kind, _ := choice["type"].(string); kind {
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "tool":
			if name, _ := choice["name"].(string); name != "" {
				out["tool_choice"] = map[string]any{
					"type":     "function",
					"function": map[string]any{"name": name},
				}
			}
		case "none":
			out["tool_choice"] = "none"
		}
		// disable_parallel_tool_use 是「这一轮只准调一个工具」开关，藏在 tool_choice
		// 里而非顶层。漏掉它客户端以为并发被禁了、实际没禁：上游可能一次回多个
		// tool_use，串行编排拿到意料之外的结果。只在显式 true 时映射：Anthropic 的
		// 默认（未给该字段）允许并发，OpenAI 侧默认同样允许，不写字段即语义一致。
		if choice["disable_parallel_tool_use"] == true {
			out["parallel_tool_calls"] = false
		}
	}
	return out
}

// anthThinkingEnabled 请求是否启用扩展思考。
//
// adaptive 同样算启用：新版 Claude Code 判断模型是否支持 adaptive 的依据是模型名
// 是否在它的官方能力表里——cn:deepseek-v4-flash 这类第三方模型名一律查不到，于是
// 全部回落到 {'type':'adaptive'}。只认 enabled 会让这些请求的推理在网关这一跳被
// 整段丢弃：上游照常思考，但客户端一个 thinking_delta 都收不到。两种 type 对网关
// 的语义相同——客户端能接受 thinking 块——因此都回。
//
// 宽容处理畸形值：不是 dict、或缺 type，都按「没启用」——宁可少回一个 thinking
// 块（客户端拿不到时只是多花点 token 重新思考），也不要对着不认这种块的客户端硬塞。
func anthThinkingEnabled(body map[string]any) bool {
	think, ok := body["thinking"].(map[string]any)
	if !ok {
		return false
	}
	typ, _ := think["type"].(string)
	typ = strings.ToLower(strings.TrimSpace(typ))
	return typ == "enabled" || typ == "adaptive"
}

// anthReasoningEffort 把 Anthropic 侧的推理档位映射成上游 reasoning_effort
// （无则空串）。两个来源，优先级：output_config.effort > thinking.budget_tokens。
//
// budget_tokens → 档位分界按官方文档量级取：1024 是最小可用预算，4k 以下 low、
// 16k 以下 medium、64k 以下 high、再往上 max（≥64k 表达「不限思考」，对应上游
// 最强档；给 xhigh 会让这种请求拿不到应有的深度）。精度不重要——它是「用户愿意
// 花多少」的粗略表达，真正的「该模型支持哪些档」由上游管线 normalizeReasoningEffort
// 按能力降级，我们不重复实现那张表（重复就是两份事实来源）。
func anthReasoningEffort(body map[string]any) string {
	if cfg, ok := body["output_config"].(map[string]any); ok {
		if raw, _ := cfg["effort"].(string); strings.TrimSpace(raw) != "" {
			switch e := strings.ToLower(strings.TrimSpace(raw)); e {
			case "minimal", "low", "medium", "high", "xhigh", "max":
				return e
			}
		}
	}
	think, ok := body["thinking"].(map[string]any)
	if !ok {
		return ""
	}
	budget, ok := think["budget_tokens"].(float64)
	if !ok || budget <= 0 {
		return ""
	}
	switch {
	case budget < 4096:
		return "low"
	case budget < 16384:
		return "medium"
	case budget < 65536:
		return "high"
	default:
		return "max"
	}
}

// ---------------------------------------------------------------------------
// 响应转换：OpenAI Chat Completions → Anthropic
// ---------------------------------------------------------------------------

// anthThinkingBlock Anthropic 的 thinking 块：thinking 给人看，signature 供客户端
// 回传。两个字段缺一不可：只有 thinking → 客户端没有可回传的签名，整个块可能被
// 丢弃（下一轮不带推理痕迹，DeepSeek 报 11155）；只有 signature → 界面上看不到
// 思考过程。本网关没有跨轮凭据加密体系（无 Responses 侧 encrypted_content 编解码
// 需要对齐），signature 以推理原文占位——回传时请求侧转换把 thinking 明文原样
// 带回（见 toOpenAIRequest 的 thinking 分支），凭据链条在本协议内自洽。
func anthThinkingBlock(text string) map[string]any {
	return map[string]any{
		"type":      "thinking",
		"thinking":  text,
		"signature": text,
	}
}

// toAnthropicResponse OpenAI 非流式响应 → Anthropic 响应体。
// thinking=true 时在最前面加 thinking 块（推理原文）。没启用的客户端不处理
// thinking 块，多出来反而可能被当成协议异常（与 Anthropic 官方行为一致：
// 不开思考就没有该块）。
func toAnthropicResponse(data map[string]any, model string, thinking bool) map[string]any {
	choice := map[string]any{}
	if ch := anthToSlice(data["choices"]); len(ch) > 0 {
		if c, ok := ch[0].(map[string]any); ok {
			choice = c
		}
	}
	message, _ := choice["message"].(map[string]any)
	if message == nil {
		message = map[string]any{}
	}

	var content []any
	if thinking {
		if rc, _ := message["reasoning_content"].(string); rc != "" {
			content = append(content, anthThinkingBlock(rc))
		}
	}
	if text, _ := message["content"].(string); text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, raw := range anthToSlice(message["tool_calls"]) {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			fn = map[string]any{}
		}
		// OpenAI 的 arguments 是字符串，Anthropic 的 input 是对象。上游给了非法
		// JSON 时原样塞进占位字段，别让整次调用失败。
		var input any = map[string]any{}
		if rawArgs, _ := fn["arguments"].(string); strings.TrimSpace(rawArgs) != "" {
			var parsed any
			if json.Unmarshal([]byte(rawArgs), &parsed) == nil {
				input = parsed
			} else {
				input = map[string]any{"_raw": rawArgs}
			}
		}
		id, _ := call["id"].(string)
		name, _ := fn["name"].(string)
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  name,
			"input": input,
		})
	}
	if content == nil {
		content = []any{}
	}

	finish, _ := choice["finish_reason"].(string)
	usage := anthUsageMap(data["usage"])
	if usage == nil {
		usage = map[string]any{}
	}
	return map[string]any{
		"id":            "msg_" + anthNewID(),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReasonOf(finish, false),
		"stop_sequence": nil,
		// 口径转换见 anthUsageFields：Anthropic 的 input 不含缓存，
		// 直接发 prompt_tokens 会让客户端把缓存算两遍。
		"usage": anthUsageFields(usage),
	}
}

// ---------------------------------------------------------------------------
// 流式状态机：OpenAI SSE → Anthropic SSE
// ---------------------------------------------------------------------------

// sseEvent Anthropic SSE 事件：带 event: 行，且两道换行结尾（与 OpenAI 的裸
// data: {...} 完全不同的帧形态）。
func sseEvent(name string, payload any) []byte {
	raw, _ := json.Marshal(payload)
	return []byte("event: " + name + "\ndata: " + string(raw) + "\n\n")
}

// streamTranslator OpenAI SSE → Anthropic 事件流的转换状态机。
//
// 为什么需要状态机而不是逐块替换：Anthropic 的流是有结构的——每个内容块必须先
// content_block_start、增量若干次 content_block_delta、再 content_block_stop，
// 且块有递增的 index；整条消息还要用 message_start / message_delta / message_stop
// 包起来。而 OpenAI 只给一串无结构的 delta，没有任何"边界"信息，所以边界只能由
// 我们在遇到内容类型切换或流结束时自己推断。
//
// 文本与工具调用的增量语义也不同：文本 → text_delta；工具调用参数 →
// input_json_delta，且参数是分片下发的（{"loc` + `ation": ...}` 形态），必须原样
// 透传片段、由客户端拼接。
type streamTranslator struct {
	model    string
	thinking bool // 请求启用了扩展思考才回 thinking 块（不开思考的客户端不处理该块）

	msgID    string
	started  bool
	finished bool

	thinkIdx int // 当前打开的思考块 index（-1 = 未打开）
	textIdx  int // 当前打开的文本块 index（-1 = 未打开）

	// toolSlots 并行工具调用分槽：上游 tool_calls 各带 index 字段，参数分片可能
	// 跨帧交错到达（index=0 与 index=1 的 arguments 交替）。只记单个「当前工具」
	// 会让后到的分片写进错误的块（input_json_delta 串块，客户端拼出损坏 JSON）。
	// key = 上游 index，value = 本翻译器分配的 Anthropic content_block index
	// （-1 = 该槽还未开块）。
	toolSlots map[int]int
	toolOrder []int // 槽位开块顺序（finish 收尾按此顺序关块）
	toolID    string

	thinkBuf  string // 思考全文（用于 signature）
	nextIndex int
	sawTool   bool
	sawText   bool

	inputTokens  int
	outputTokens int
	cacheRead    int
	cacheWrite   int
}

func newStreamTranslator(model string, thinking bool) *streamTranslator {
	return &streamTranslator{
		model:     model,
		thinking:  thinking,
		msgID:     "msg_" + anthNewID(),
		thinkIdx:  -1,
		textIdx:   -1,
		toolSlots: map[int]int{},
	}
}

func (t *streamTranslator) startMessage() []byte {
	t.started = true
	return sseEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            t.msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         t.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": t.inputTokens, "output_tokens": 0},
		},
	})
}

// closeBlock 关掉一个打开的内容块（index<0 时空操作）。
func (t *streamTranslator) closeBlock(index int) []byte {
	if index < 0 {
		return nil
	}
	return sseEvent("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": index,
	})
}

// closeToolSlots 关掉全部打开的工具块（按开块顺序），并清空槽位表。
func (t *streamTranslator) closeToolSlots() []byte {
	var out []byte
	for _, upIdx := range t.toolOrder {
		if slot, ok := t.toolSlots[upIdx]; ok && slot >= 0 {
			out = append(out, t.closeBlock(slot)...)
		}
	}
	t.toolSlots = map[int]int{}
	t.toolOrder = nil
	return out
}

// closeThink 关掉思考块。先把 signature 发完、再 stop——顺序不能反：
// content_block_stop 之后到达的 delta 会被客户端丢弃（块已经关闭），签名就白发
// 了，而签名正是客户端回传推理的唯一凭据。
func (t *streamTranslator) closeThink() []byte {
	if t.thinkIdx < 0 {
		return nil
	}
	index := t.thinkIdx
	t.thinkIdx = -1
	out := sseEvent("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{"type": "signature_delta", "signature": t.thinkBuf},
	})
	return append(out, t.closeBlock(index)...)
}

// openBlock 分配下一个 index 并发 content_block_start，返回新 index。
func (t *streamTranslator) openBlock(contentBlock map[string]any) int {
	index := t.nextIndex
	t.nextIndex++
	return index
}

// feed 喂一个 OpenAI SSE 的 data 对象，返回要下发的事件字节。
func (t *streamTranslator) feed(obj map[string]any) []byte {
	// 已经收尾过就不再吐事件：上游若在带 finish_reason 的帧之后继续发内容帧
	// （它自己有 bug、或被打穿），客户端会收到 message_stop 之后的事件，而那些块
	// 永远等不到 content_block_stop——协议被污染。
	if t.finished {
		return nil
	}
	var out []byte
	if !t.started {
		out = append(out, t.startMessage()...)
	}

	// usage 可能在任何帧（通常末帧）。只累积原始数值，口径转换（input 减缓存）
	// 留到发送时统一走 anthUsageFields。
	if u := anthUsageMap(obj["usage"]); u != nil {
		if v := anthAsInt(u["prompt_tokens"]); v > 0 {
			t.inputTokens = v
		}
		if v := anthAsInt(u["completion_tokens"]); v > 0 {
			t.outputTokens = v
		}
		if v := anthAsInt(u["prompt_cache_hit_tokens"]); v > 0 {
			t.cacheRead = v
		}
		if v := anthAsInt(u["prompt_cache_write_tokens"]); v > 0 {
			t.cacheWrite = v
		}
	}

	choices := anthToSlice(obj["choices"])
	if len(choices) == 0 {
		return out
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return out
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		// 有的上游把完整消息放在 message 里（非 delta 整条下发）：同构消费。
		delta, _ = choice["message"].(map[string]any)
	}
	if delta == nil {
		delta = map[string]any{}
	}

	// 推理增量（上游 reasoning_content）→ thinking 块。位置在文本之前——与上游
	// 给增量的顺序一致（先思考后正文）。
	if t.thinking {
		if rc, _ := delta["reasoning_content"].(string); rc != "" {
			if t.thinkIdx < 0 {
				t.thinkIdx = t.openBlock(map[string]any{"type": "thinking", "thinking": ""})
				out = append(out, sseEvent("content_block_start", map[string]any{
					"type":          "content_block_start",
					"index":         t.thinkIdx,
					"content_block": map[string]any{"type": "thinking", "thinking": ""},
				})...)
			}
			t.thinkBuf += rc
			out = append(out, sseEvent("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": t.thinkIdx,
				"delta": map[string]any{"type": "thinking_delta", "thinking": rc},
			})...)
		}
	}

	if text, _ := delta["content"].(string); text != "" {
		t.sawText = true
		// 切到正文前先把思考块关掉（含发签名）——思考与正文是两个块。
		out = append(out, t.closeThink()...)
		// 从工具块切回文本时，先把所有工具块关掉。
		out = append(out, t.closeToolSlots()...)
		if t.textIdx < 0 {
			t.textIdx = t.nextIndex
			t.nextIndex++
			out = append(out, sseEvent("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         t.textIdx,
				"content_block": map[string]any{"type": "text", "text": ""},
			})...)
		}
		out = append(out, sseEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": t.textIdx,
			"delta": map[string]any{"type": "text_delta", "text": text},
		})...)
	}

	for _, raw := range anthToSlice(delta["tool_calls"]) {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			fn = map[string]any{}
		}
		// 文本块与工具块不能并存：切到工具前先关文本；思考块同理（签名要在关闭
		// 前发完）。
		out = append(out, t.closeThink()...)
		out = append(out, t.closeBlock(t.textIdx)...)
		t.textIdx = -1

		// 上游 index 分槽（并行 tool calls 各自独立成块；缺省 index=0 与
		// 单工具序列形态兼容——首个未带 index 的分片也落 0 号槽）。
		upIdx := 0
		if v := anthAsInt(call["index"]); v > 0 {
			upIdx = v
		}
		slot, opened := t.toolSlots[upIdx]
		if name, _ := fn["name"].(string); name != "" {
			// 新工具（该槽首个带名的分片）：关掉槽里旧的，开一个新的。
			if opened {
				out = append(out, t.closeBlock(slot)...)
			} else {
				t.toolOrder = append(t.toolOrder, upIdx)
			}
			t.toolID, _ = call["id"].(string)
			slot = t.nextIndex
			t.nextIndex++
			t.toolSlots[upIdx] = slot
			t.sawTool = true
			out = append(out, sseEvent("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": slot,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    t.toolID,
					"name":  name,
					"input": map[string]any{},
				},
			})...)
		}
		// 参数分片原样透传，拼接交给客户端（我们无从判断 JSON 何时完整）。
		if args, _ := fn["arguments"].(string); args != "" && slot >= 0 {
			out = append(out, sseEvent("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": slot,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
			})...)
		}
	}

	if finish, _ := choice["finish_reason"].(string); finish != "" {
		out = append(out, t.finish(finish)...)
	}
	return out
}

// finish 收尾：关掉打开的块，发 message_delta + message_stop（幂等）。
// message_delta 发的是累计 usage，必须把 input_tokens 一并带上——message_start
// 那一刻上游还没给 usage（必然是 0），真实值只能在末尾补；少了它客户端整条流里
// 再也看不到真实的输入量，只能回退成按字符估算（中文会被低估约 1.5 倍；社区
// 实测网关侧统计 7447 万、客户端只显示 3648 万，差了整整一倍）。
func (t *streamTranslator) finish(finishReason string) []byte {
	if t.finished {
		return nil
	}
	t.finished = true
	var out []byte
	if !t.started {
		out = append(out, t.startMessage()...)
	}
	// 思考块先关（含发签名）：顺序与开块顺序一致，也让签名一定落在 stop 之前。
	out = append(out, t.closeThink()...)
	out = append(out, t.closeBlock(t.textIdx)...)
	t.textIdx = -1
	out = append(out, t.closeToolSlots()...)

	out = append(out, sseEvent("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReasonOf(finishReason, t.sawTool),
			"stop_sequence": nil,
		},
		"usage": anthUsageFields(map[string]any{
			"prompt_tokens":             t.inputTokens,
			"completion_tokens":         t.outputTokens,
			"prompt_cache_hit_tokens":   t.cacheRead,
			"prompt_cache_write_tokens": t.cacheWrite,
		}),
	})...)
	out = append(out, sseEvent("message_stop", map[string]any{"type": "message_stop"})...)
	return out
}

// ---------------------------------------------------------------------------
// 端点：POST /v1/messages
// ---------------------------------------------------------------------------

// anthOpenAIErrorEnvelope 提取管线 OpenAI error 信封的 message 与 gateway_hint。
// 解析不出（非 JSON / 无 error 对象）时 message 为空，由调用方兜底。
func anthOpenAIErrorEnvelope(body string) (msg, hint string) {
	var env struct {
		Error struct {
			Message     string `json:"message"`
			GatewayHint string `json:"gateway_hint"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &env) != nil {
		return "", ""
	}
	return env.Error.Message, env.Error.GatewayHint
}

// anthropicMessages Anthropic Messages API 主端点。
//
// 转换后的 OpenAI 请求体经克隆请求直入 chatCompletions 管线——选号/轮转/降级/
// reasoning 回填/usage 采集/metrics/请求日志全部复用，不新开上游调用路径。
// 本端点只在外围做三件事：
//  1. 协议校验（model/max_tokens）与请求体转换；
//  2. 用 anthResponseBridge 捕获管线产出，流式现场翻译、非流式整体转换；
//  3. 错误信封按 Anthropic 错误体透传（真实状态码定型——上游报错必须在开流
//     之前回真实状态码，200 定型后错误只能裹成事件）。
//
// 鉴权已由挂载时的 withAuth 完成（与 /v1/chat/completions 完全同口径）。
func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, err := h.readJSONBody(w, r)
	if err != nil {
		return // 响应已写出
	}
	var req map[string]any
	if json.Unmarshal(body, &req) != nil || req == nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "请求体不是合法 JSON 对象")
		return
	}
	model, _ := req["model"].(string)
	if strings.TrimSpace(model) == "" {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "model 必须是字符串")
		return
	}
	// max_tokens 在 Anthropic 协议里是必填项，缺失即 400（与 OpenAI 不同）。
	if req["max_tokens"] == nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "缺少必填字段 max_tokens")
		return
	}
	stream := req["stream"] == true
	wantThinking := anthThinkingEnabled(req)

	payload := toOpenAIRequest(req)
	// 流式显式带 stream_options.include_usage：管线 payload 层只在字段缺失时补，
	// 这里自己带上语义相同但意图明确——Anthropic 客户端靠 message_delta 的 usage
	// 更新 token 计数，缺失则整条流看不到真实用量。
	if stream {
		payload["stream_options"] = map[string]any{"include_usage": true}
	}
	outBody, err := json.Marshal(payload)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "请求转换失败："+err.Error())
		return
	}

	// 内部转发：io.Pipe 直连（对齐 responses.go 的 rspInnerWriter 模式）——管线
	// 写一帧、这里翻译一帧下发，客户端在生成期间持续收到字节（真流式）；此前
	// anthResponseBridge 全量缓冲整个上游响应，流式语义丢失、首字节延迟等于
	// 全程生成时长、buf 无上限。非流式分支同样从 pipe 聚合（行为不变）。
	pr, pw := io.Pipe()
	iw := &anthInnerWriter{header: http.Header{}, pw: pw, sig: make(chan struct{})}
	cloned := r.Clone(r.Context())
	// Body 必须装实际转换后的字节：管线 session.ParseRequest 从 body 读 stream/
	// model/会话键等全部派生字段（Body 为空则 parsed.Stream=false，流式请求会
	// 静默退化成非流式，且选号/粘性全部失去输入）。
	cloned.Body = io.NopCloser(bytes.NewReader(outBody))
	cloned.ContentLength = int64(len(outBody))
	cloned.Header = r.Header.Clone()
	cloned.Header.Del("Authorization")
	cloned.Header.Del("Cookie")
	cloned.Header.Set("Content-Type", "application/json")
	cloned.Header.Set("Content-Length", fmt.Sprintf("%d", len(outBody)))
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer pw.Close() // chatCompletions 返回后关写端 → 读端 EOF
		h.chatCompletions(iw, cloned)
	}()

	// 状态码就绪（错误信封显式写头 / 首个数据帧隐式 200）。在此之前不向客户端
	// 定型任何状态——错误必须以真实状态码回给 Anthropic SDK 触发重试/报错路径。
	<-iw.sig

	// 错误未开流：真实状态码回 Anthropic 错误体（不裹事件——客户端还没看到任何
	// 200 语义）。流式/非流式同口径：排空 pipe、等管线收尾，再一次性写错误体。
	if iw.status >= 400 {
		errBody, _ := io.ReadAll(pr)
		<-done
		msg, hint := anthOpenAIErrorEnvelope(string(errBody))
		if strings.TrimSpace(msg) == "" {
			msg = strings.TrimSpace(string(errBody))
		}
		if msg == "" {
			msg = fmt.Sprintf("upstream returned %d", iw.status)
		}
		writeAnthropicError(w, iw.status, "api_error", anthJoinHint(msg, hint))
		return
	}

	// 流式：管线产出的 OpenAI SSE 在飞行中翻译为 Anthropic 事件流。
	if stream {
		h.anthropicStream(w, pr, model, wantThinking)
		_ = pr.Close() // 关读端：chatCompletions 在途 pipe 写立刻失败（客户端已走）
		<-done
		return
	}

	// 非流式：聚合完整响应体（pw 关闭后 ReadAll 返回）。
	body2, _ := io.ReadAll(pr)
	<-done

	var data map[string]any
	if json.Unmarshal(body2, &data) != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream response is not valid JSON")
		return
	}
	writeJSON(w, http.StatusOK, toAnthropicResponse(data, model, wantThinking))
}

// anthJoinHint 把 gateway_hint 拼在 message 后（SSE/错误体只有一个 message 字段位
// 时，丢弃 hint 等于让「该怎么办」这句建议消失；非流式 JSON 有独立字段位，同样
// 并入 message 保持与流式一致的形态）。
func anthJoinHint(msg, hint string) string {
	if hint == "" {
		return msg
	}
	return msg + "（" + hint + "）"
}

// anthropicStream 把 bridge 捕获的 OpenAI SSE 流翻译成 Anthropic 事件流写出。
//
// 状态码定型：bridge.status() < 400 时管线已产 200（chatCompletions 在选号成功、
// 上游开流后才写头），真实错误已在轮转内消化或以 error 帧形态在流内出现——
// 这里按 SSE 输出；>= 400 时直接以真实状态码回 Anthropic 错误体（未开流，
// 状态码可自由定型）。
func (h *Handler) anthropicStream(w http.ResponseWriter, body io.Reader, model string, thinking bool) {
	// 状态码 >= 400 的分支由调用方在开流前处理（真实状态码回错误体）；进入本函数
	// 时管线已产 200，真实错误只能以 error 事件形态出现在流内（见下方 error 帧
	// 转换）。
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	tr := newStreamTranslator(model, thinking)
	var errorText string
	// 逐行流式消费（不再全量缓冲）：pipe 读一帧翻译一帧，边生成边下发。
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), maxRspSSEBuffer)
	for sc.Scan() {
		raw := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(raw, "data:") {
			continue // 注释/空行/事件名行
		}
		raw = strings.TrimSpace(raw[len("data:"):])
		if raw == "" || raw == "[DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(raw), &obj) != nil {
			continue
		}
		// 上游可能中途回 error 帧（{"error":{...}}，没有 choices）：整帧丢弃会让
		// 客户端只收到 message_start 加一个空回答——显式转成 error 事件并中止
		// 迭代（pr 由调用方关闭，pipe 写端立刻失败，chatCompletions 尽快收尾）。
		if e := anthUsageMap(obj["error"]); e != nil && len(anthToSlice(obj["choices"])) == 0 {
			msg, _ := e["message"].(string)
			if msg == "" {
				rawMsg, _ := json.Marshal(e)
				msg = string(rawMsg)
			}
			errorText = msg
			break
		}
		if ev := tr.feed(obj); len(ev) > 0 {
			_, _ = w.Write(ev)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	// 收尾顺序要紧：error 必须发在 message_stop 之前——多数 SDK 把 message_stop
	// 当流的终止信号，读到它就结束迭代、不再读后续事件；先 stop 后 error 等于
	// 那个错误永远不会被客户端看到，客户端仍表现为「成功但空回复」。
	if errorText != "" {
		_, _ = w.Write(sseEvent("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": errorText},
		}))
	}
	// 仍补一套收尾事件（含 message_start，若流尚未开始）：部分客户端期待流以
	// message_stop 结束，缺了会一直等（挂住）。error 已先发出，不会再被吞掉。
	if tail := tr.finish(""); len(tail) > 0 {
		_, _ = w.Write(tail)
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// ---------------------------------------------------------------------------
// 端点：POST /v1/messages/count_tokens
// ---------------------------------------------------------------------------

// estimateTokens 按字符类别粗略估算 token 数（供 count_tokens，估不到上游分词器）。
//
// 统一「字符数 / 3」对中文是严重低估，而低估会让客户端以为还能塞更多、真实请求
// 却在发出时被上游以「上下文过长」拒绝，且用户看不出是估算接口给了错数字。
// 高估才是安全方向（客户端少塞一点，请求仍能过）：
//   - CJK（汉字/假名/韩文）：约 1 字符 1 token（保守取整字符数）；
//   - 其余（拉丁/代码/JSON）：约 4 字符 1 token。
//
// 只做数量级估算，不追求精确——它唯一的作用是让客户端留够余量。判据用码位区间
// 而不是 unicode 宽度表：区间判定稳定且无查表开销。
func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	cjk, other := 0, 0
	for _, r := range text {
		cp := int(r)
		// 汉字（含扩展 A）、假名、谚文音节、CJK 标点与全角形式、扩展 B 区。
		if (cp >= 0x3000 && cp <= 0x9FFF) || (cp >= 0xAC00 && cp <= 0xD7AF) ||
			(cp >= 0xF900 && cp <= 0xFAFF) || (cp >= 0xFF00 && cp <= 0xFFEF) ||
			(cp >= 0x20000 && cp <= 0x2FA1F) {
			cjk++
		} else {
			other++
		}
	}
	return cjk + (other+3)/4
}

// anthropicCountTokens 粗略估算输入 token 数。
//
// Claude Code 等客户端先调这个接口决定上下文还能塞多少。拿不到上游分词器，
// 只能给粗略估算。估算口径按脚本分开加权（理由见 estimateTokens），每个 block
// 再加 4 token 结构开销——消息/工具边界在真实分词里都要额外占位，纯按字符数算
// 必然偏低；这部分是「宁可高估」的落点。
//
// 鉴权与 /v1/messages 同一条 withAuth 通道（挂载时包装，同口径同密钥）；
// 允许不带 model（那是常态）。请求体上限与主端点同一 readJSONBody 口径——
// 这个端点同样对外开放，不能成为绕过网关请求体上限的后门。
func (h *Handler) anthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := h.readJSONBody(w, r)
	if err != nil {
		return
	}
	var req map[string]any
	if json.Unmarshal(body, &req) != nil || req == nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "请求体不是 JSON 对象")
		return
	}
	total, blocks := 0, 0
	if sys := req["system"]; sys != nil {
		blocks++
		total += estimateTokens(anthTextOf(sys))
	}
	for _, raw := range anthToSlice(req["messages"]) {
		if m, ok := raw.(map[string]any); ok {
			blocks++
			total += estimateTokens(anthTextOf(m["content"]))
		}
	}
	for _, raw := range anthToSlice(req["tools"]) {
		if t, ok := raw.(map[string]any); ok {
			blocks++
			rawJSON, _ := json.Marshal(t)
			total += estimateTokens(string(rawJSON))
		}
	}
	// 下限 1：空请求也是一次分词。
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": max(1, total+blocks*4)})
}

// ---------------------------------------------------------------------------
// 内部复用设施
// ---------------------------------------------------------------------------

// readJSONBody 请求体读取（MaxBytesReader 上限 + 413/400 分类），chatCompletions
// 与 Anthropic 端点共用同一口径。错误响应在本函数内写出（OpenAI 信封形态）——
// Anthropic 端点复用它时仍是 OpenAI 错误形态，但此路径只发生在协议解析之前
// （体太大/断流），与 chatCompletions 既有形态一致即可，不再二次包装。
func (h *Handler) readJSONBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBodyBytes()))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large",
				fmt.Sprintf("request body exceeds limit (%d MB); reduce context/messages or raise max_body_mb",
					h.maxBodyBytes()>>20))
		} else {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		}
		return nil, err
	}
	return body, nil
}

// anthIDSeq 消息/请求 ID 的进程级计数（时间戳 + 计数，进程内唯一即可）。
var anthIDSeq atomic.Uint64

// anthInnerWriter 内部转发用的 ResponseWriter（对齐 responses.go 的 rspInnerWriter）：
// 捕获 chatCompletions 写出的状态码与响应头，body 字节经 io.Pipe 同步交给主
// goroutine——io.Pipe 无缓冲，写端阻塞到读端消费，天然背压，流式翻译零额外缓冲。
type anthInnerWriter struct {
	header  http.Header
	status  int
	pw      *io.PipeWriter
	sig     chan struct{}
	sigOnce sync.Once
}

func (w *anthInnerWriter) signal() { w.sigOnce.Do(func() { close(w.sig) }) }

func (w *anthInnerWriter) Header() http.Header { return w.header }

func (w *anthInnerWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.signal()
}

func (w *anthInnerWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK // 隐式 200（SSE 透传路径从不显式 WriteHeader）
	}
	w.signal()
	return w.pw.Write(b)
}

// Flush 空实现：io.Pipe 写是同步的（读端消费才返回），无需 flush 传播。
// chatCompletions 流式路径的 http.Flusher 断言落空后照常逐帧写出。
func (w *anthInnerWriter) Flush() {}

// anthNewID 生成 Anthropic 形态的消息 ID 片段（msg_ 前缀由调用方拼）。
func anthNewID() string {
	return fmt.Sprintf("%x%04x", time.Now().UnixNano(), anthIDSeq.Add(1)&0xffff)
}
