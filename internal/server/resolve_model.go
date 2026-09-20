package server

import (
	"strings"
	"sync"
)

// modelMapLock 保护 modelMap 的读写（面板保存配置时替换整表）。
// 读多写少、整表替换：RWMutex 即可，无逐键锁的必要。
var modelMapLock sync.RWMutex

// modelMap 用户自定义模型映射（设计文档 §4.3.1）：key=请求模型名，value=实际模型名。
// 服务 cc-switch 等写死模型名的客户端——链头查找，命中即整名替换（含 realm 前缀语义）。
// 空表 = 不做映射（零开销路径）。
var modelMap = map[string]string{}

// SetModelMap 原子替换整张映射表（面板保存配置时调用；nil 等价清空）。
func SetModelMap(m map[string]string) {
	modelMapLock.Lock()
	defer modelMapLock.Unlock()
	next := make(map[string]string, len(m))
	for k, v := range m {
		if k == "" || v == "" || k == v {
			continue // 恒等映射与空键值无意义，剔除避免死循环/噪声
		}
		next[k] = v
	}
	modelMap = next
}

// ModelMapView 返回映射表副本（面板读取用；调用方修改副本不影响生效表）。
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
// 单次替换、不递归（映射目标不再查表）：避免 A→B→A 死循环，语义与 manager
// _map_model（mapping.get(model, model) 单次 get）一致。
// 未命中返回原串。
func applyModelMap(model string) string {
	modelMapLock.RLock()
	mapped, ok := modelMap[model]
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
