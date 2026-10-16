package server

import (
	"strings"
	"sync"
)

// modelMapLock 保护 modelMap 的读写（面板保存配置时替换整表）。
// 读多写少、整表替换：RWMutex 即可，无逐键锁的必要。
var modelMapLock sync.RWMutex

// fallbackModelMapKey 模型映射兜底键（吸收 manager 1.0.92「模型映射兜底」）：
// 精确匹配未命中时用它的值接住请求模型名，语义与 manager 的 mapping 兜底一致。
const fallbackModelMapKey = "*"

// modelMap 用户自定义模型映射（设计文档 §4.3.1）：key=请求模型名，value=实际模型名。
// 服务 cc-switch 等写死模型名的客户端——链头查找，命中即整名替换（含 realm 前缀语义）。
// 键 "*" 为兜底键（见 applyModelMap）。空表 = 不做映射（零开销路径）。
var modelMap = map[string]string{}

// SetModelMap 原子替换整张映射表（面板保存配置时调用；nil 等价清空）。
// 键 "*" 是合法兜底键，照常保留；清洗只针对空键/空值/恒等映射。
func SetModelMap(m map[string]string) {
	modelMapLock.Lock()
	defer modelMapLock.Unlock()
	next := make(map[string]string, len(m))
	for k, v := range m {
		if k == "" || v == "" || k == v {
			// 恒等映射与空键值无意义，剔除避免死循环/噪声
			//（"*"→"*" 属恒等，一并剔除；"*"→某模型 保留，是有效兜底）
			continue
		}
		next[k] = v
	}
	modelMap = next
}

// ModelMapView 返回映射表副本（面板读取用；调用方修改副本不影响生效表）。
// 兜底键 "*" 与普通键一样出现在视图里（面板原样读写，无需特殊处理）。
func ModelMapView() map[string]string {
	modelMapLock.RLock()
	defer modelMapLock.RUnlock()
	out := make(map[string]string, len(modelMap))
	for k, v := range modelMap {
		out[k] = v
	}
	return out
}

// applyModelMap 链头映射（设计文档 §4.3.1）：对**完整请求模型名**（含 realm 前缀）查表，
// 命中即整名替换。链头语义：映射在 realm 前缀解析之前生效，因此
//   - "gpt-4o" → "cn:glm-5.2"：裸名客户端整名换到 CN 前缀模型；
//   - "cn:glm-5.2" → "glm-5.3"：前缀名映射到裸名（默认 CN）；
//   - 值仍会走 resolveModel 的前缀解析，映射不改变前缀协议本身。
//
// 查找顺序（v1.19.0 吸收 manager 1.0.92「模型映射兜底」）：
//  1. 完整请求模型名精确匹配（含 realm 前缀，大小写敏感）；
//  2. 未命中时用兜底键 "*"（配置了即对全部未命中模型生效）；
//  3. 仍无则原样返回。
//
// 单次替换、不递归（映射目标不再查表，兜底值同样不二次展开）：避免 A→B→A 死循环，
// 语义与 manager _map_model（mapping.get(model, model) 单次 get）一致。
//
// ⚠️ 副作用：配置 "*" 后，**任何拼错的模型名也会被一起接住**并换成兜底模型，
// 不再报「模型不可用」——错拼、错 realm 前缀会被静默吞掉，排障时留意，谨慎使用。
func applyModelMap(model string) string {
	modelMapLock.RLock()
	mapped, ok := modelMap[model]
	if !ok {
		mapped, ok = modelMap[fallbackModelMapKey] // 精确未命中 → 兜底键
	}
	modelMapLock.RUnlock()
	if !ok {
		return model
	}
	return mapped
}

// resolveModel 解析模型名协议（PLAN D6）：
//
//	分布式前缀： "[realm:]model"
//
// 取第一个 ":"，前段恰为 "cn"/"global" 才剥离；否则视为裸名，realm=cn、bare=原串。
// 大小写敏感（前缀必须是精确的小写枚举）。bare 即出站/选号/账本使用的裸模型名。
//
// v1.2.0：入口先过 applyModelMap 链头映射（用户自定义整名替换），再做前缀解析——
// 映射面向「客户端发什么模型名」这一层，前缀解析面向「网关内部路由」这一层。
//
// 导出为 ResolveModel（cmd/server/main.go 粘性闭包需要），包内简写 resolveModel。
func resolveModel(model string) (realm, bare string) {
	model = applyModelMap(model)
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn", model
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" {
		return "cn", model
	}
	return prefix, model[idx+1:]
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }
