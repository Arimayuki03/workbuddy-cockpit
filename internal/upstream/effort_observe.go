// effort_observe.go 出站思考档位的观测推算（只读，不改写任何真实出站请求）。
//
// 用途：请求日志/面板的「出站档位」列——chatCompletions 在选号后调用，把 body
// 复制一份走与 prepareBodyPass 完全相同的 effort 管线（injectThinking 补默认档 →
// normalizeReasoningEffort 按模型支持档降级），读出最终出站值。因为直接复用
// 管线函数，口径与真实出站恒一致；代价是每个请求多一次 JSON 解析（只发生在
// 观测需要时，非 deepseek 模型同样能算——injectThinking 自带模型闸门）。
package upstream

import "encoding/json"

// EffectiveReasoningEffort 返回 body 经思维链注入与档位降级管线后的最终出站
// reasoning_effort（realm 按账号归属域取 effort/defaultEffort 桶，与 prepareBody
// 同参）。空串 = 无档位（非 deepseek 且客户端未带 / body 不可解析 / 无 model）。
// 纯观测：内部 unmarshal 独立副本，绝不影响真实出站 body。
func (c *Client) EffectiveReasoningEffort(body []byte, realm string) string {
	if c == nil || len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	if _, ok := obj["model"].(string); !ok {
		return ""
	}
	injectThinking(obj, lookupDefaultEffort(c.defaultEffortsSnapshot(realm), obj["model"].(string)))
	normalizeReasoningEffort(obj, c.effortsSnapshot(realm))
	if v, ok := obj["reasoning_effort"].(string); ok && v != "" {
		return v
	}
	if v, ok := obj["reasoningEffort"].(string); ok && v != "" {
		return v
	}
	return ""
}
