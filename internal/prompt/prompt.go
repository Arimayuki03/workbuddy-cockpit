// Package prompt 提供网关自有系统提示词：内置默认 + 文件覆盖 + 降级中性提示词。
//
// 背景：客户端（Claude Code/Codex 等 CLI）在 system prompt 注入固定模板句，
// 上游内容审核按逐字精确匹配误杀合法流量（issue #36/PR39 的 11-128）。
// 方案：网关在出站前用自有系统提示词替换客户端 system/developer 消息，
// 从源头消灭 system 来源的指纹误报（用户/assistant 消息里的指纹串仍由
// internal/upstream/sanitize.go 清洗，两层叠加、互不替代）。
//
// 实现口径（大整数保真）：Append/Rewrite 用 map[string]json.RawMessage 透传——
// 只解码/重编码真正要改写的部分（messages 数组的增删），其余字段（顶层与消息内）
// 的**值**保持 RawMessage 原样字节。旧实现经 map[string]any 往返会把 JSON 数字统一
// float64 化，>2^53 的大整数（seed、metadata、工具 JSON Schema 整型枚举等）被
// 静默改写；本实现未改写部分的值字节级不变（未知字段值原样透传）。
// 编码层面说明：顶层输出经 json.Marshal(map[string]json.RawMessage) 重编码——键按
// 字典序排序、空白压缩、HTML 字符（< > &）转义，与 map[string]any 的编码行为一致；
// 即顶层键序/空白与原始输入不同，但这是既有行为，上游不做字节级指纹匹配，
// 重编码无功能影响。
package prompt

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
)

//go:embed defaultprompt.md
var defaultPrompt string

// Degraded 降级提示词：误报处理用，刻意极简中性。
//
// 触发场景：passthrough 模式下请求被上游内容策略拦截（HTTP 400 + 审核文案），
// 判定为指纹误报后换最小中性提示词重试一次。非对抗框架——只用于绕开
// system 来源的误报，不改变用户指令的合法性语义。
const Degraded = "You are a helpful assistant. Respond in the user's language, follow the user's instructions, and be direct and concise."

// Load 按 mode 与 file 加载系统提示词文本。
//   - file 非空 → 读文件（不存在/读失败返回 error，调用方 fail fast）；
//   - file 空 → 返回内置 defaultPrompt。
//
// mode 在此仅做透传记录（实际 custom/passthrough 路由由调用方决定），
// Load 只负责"拿到一段提示词文本"，不关心路由语义。
func Load(mode, file string) (string, error) {
	if file == "" {
		return defaultPrompt, nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("prompt file %s: %w", file, err)
	}
	return string(raw), nil
}

// gwMessage 构造网关注入的 system 消息（RawMessage 形态，直接拼进 messages 数组）。
func gwMessage(systemPrompt string) json.RawMessage {
	raw, err := json.Marshal(map[string]string{"role": "system", "content": systemPrompt})
	if err != nil {
		return json.RawMessage(`{"role":"system","content":""}`)
	}
	return raw
}

// decodeMessageElems 把 messages 的 RawMessage 解成元素级 RawMessage 切片
// （每个元素保持原始字节）。非数组/解析失败返回 false（调用方按「类型不符」兜底）。
func decodeMessageElems(raw json.RawMessage) ([]json.RawMessage, bool) {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, false
	}
	return arr, true
}

// msgRole 提取消息元素的 role 字符串。非对象消息 / 无 role / role 非字符串
// 一律返回 ""（与旧实现的 (map 断言 + string 断言) 双失败语义逐条对齐：
// 边界停止条件与保留条件都按 "" 处理）。
func msgRole(elem json.RawMessage) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(elem, &m); err != nil {
		return ""
	}
	r, ok := m["role"]
	if !ok {
		return ""
	}
	var role string
	if err := json.Unmarshal(r, &role); err != nil {
		return ""
	}
	return role
}

// remarshalWithMessages 复制顶层对象、以 messages 的新数组整体替换后重编码。
// 其余键的值是 RawMessage——原样字节透传（json.Marshal 对 map 键排序输出，
// 与旧 map[string]any 的行为一致；差异只在值不再经 any 往返）。
func remarshalWithMessages(obj map[string]json.RawMessage, msgs []json.RawMessage) ([]byte, bool) {
	out := make(map[string]json.RawMessage, len(obj))
	for k, v := range obj {
		out[k] = v
	}
	arr, err := json.Marshal(msgs)
	if err != nil {
		return nil, false
	}
	out["messages"] = arr
	b, err := json.Marshal(out)
	if err != nil {
		return nil, false
	}
	return b, true
}

// Append 解析 OpenAI 请求体并在"开头连续 system/developer 块"之后插入一条
// 网关自有 system 提示词（issue #129 append 模式）：
//   - 开头连续块 = 从 messages[0] 起向后 role 为 system/developer（精确字符串
//     匹配，与 Rewrite 删除口径一致）的消息；遇第一条非 system/developer
//     消息（含非对象消息、无 role 消息）即停；
//   - 插入点 = 连续块末尾之后（块长 0 时即 messages 最前）；
//   - 所有既有消息（含开头块、中途 system、user/assistant/tool）的值内容保持
//     原样（RawMessage 透传，大整数与未知字段不被改写；编码层面顶层会整体
//     重编码，见包注释）——客户端项目规范/工具约定与网关提示词并用。
//
// 守卫与 Rewrite 逐条一致：空 body / 空 systemPrompt / 坏 JSON → 原样返回
// （绝不失败）；无 messages 字段或 messages 类型不符 → messages=[网关 system]，
// 其余字段原样。
//
// 边界判定须显式同时匹配 system 与 developer：归一（developer→system）在
// 下游 prepareBody 的 normalizeRoles，Append 执行时开头块里的 developer
// 还是 developer。网关消息角色用 system 而非 developer——上游 role 白名单
// 不含 developer，插 developer 等于制造一次必然归一与多余的 11-128 风险窗口。
func Append(body []byte, systemPrompt string) []byte {
	if len(body) == 0 || systemPrompt == "" {
		return body
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgsRaw, present := obj["messages"]
	if !present {
		// 无 messages 字段 → messages=[网关 system]，其余字段原样保留。
		if out, ok := remarshalWithMessages(obj, []json.RawMessage{gwMessage(systemPrompt)}); ok {
			return out
		}
		return body
	}
	msgs, ok := decodeMessageElems(msgsRaw)
	if !ok {
		// messages 类型不符（非数组）→ 同上：插入单条 system 后原样保留其余字段。
		if out, ok2 := remarshalWithMessages(obj, []json.RawMessage{gwMessage(systemPrompt)}); ok2 {
			return out
		}
		return body
	}
	// 扫描开头连续 system/developer 块，遇第一条非 system/developer 即停。
	insertAt := 0
	for _, m := range msgs {
		role := msgRole(m)
		if role != "system" && role != "developer" {
			break
		}
		insertAt++
	}
	// 已有消息原始字节不动：只在插入点拼接，不重排、不改写任何元素。
	rewritten := make([]json.RawMessage, 0, len(msgs)+1)
	rewritten = append(rewritten, msgs[:insertAt]...)
	rewritten = append(rewritten, gwMessage(systemPrompt))
	rewritten = append(rewritten, msgs[insertAt:]...)
	if out, ok := remarshalWithMessages(obj, rewritten); ok {
		return out
	}
	return body
}

// Rewrite 解析 OpenAI 请求体并替换系统提示词：
//   - 删除 messages 中所有 role 为 system/developer 的消息；
//   - 在 messages 头部插入一条 {"role":"system","content":systemPrompt}；
//   - 其余字段与 user/assistant/tool 消息的值内容保持原样（RawMessage 透传，
//     大整数与未知字段不被 float64 往返改写；编码层面顶层会整体重编码，见包注释）。
//
// 解析失败 → 原样返回（绝不失败）：Rewrite 是出站改写的关键路径，
// 任何解析错误都不应阻塞请求转发，让上游按其原始语义处理。
func Rewrite(body []byte, systemPrompt string) []byte {
	if len(body) == 0 || systemPrompt == "" {
		return body
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgsRaw, present := obj["messages"]
	if !present {
		// 无 messages 字段 → messages=[网关 system]，其余字段原样保留。
		if out, ok := remarshalWithMessages(obj, []json.RawMessage{gwMessage(systemPrompt)}); ok {
			return out
		}
		return body
	}
	msgs, ok := decodeMessageElems(msgsRaw)
	if !ok {
		// messages 类型不符（非数组）→ 同上：插入单条 system 后原样保留其余字段。
		if out, ok2 := remarshalWithMessages(obj, []json.RawMessage{gwMessage(systemPrompt)}); ok2 {
			return out
		}
		return body
	}
	// 过滤掉所有 system/developer 消息（元素按原始字节保留，非对象消息照旧不删）。
	kept := make([]json.RawMessage, 0, len(msgs)+1)
	for _, m := range msgs {
		switch msgRole(m) {
		case "system", "developer":
			continue
		}
		kept = append(kept, m)
	}
	// 头部插入单条 system 消息（prepend 避免整体重排语义）。
	rewritten := make([]json.RawMessage, 0, len(kept)+1)
	rewritten = append(rewritten, gwMessage(systemPrompt))
	rewritten = append(rewritten, kept...)
	if out, ok := remarshalWithMessages(obj, rewritten); ok {
		return out
	}
	return body
}
