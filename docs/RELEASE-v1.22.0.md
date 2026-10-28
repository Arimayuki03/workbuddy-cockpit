# WorkBuddy Cockpit v1.22.0

发布日期：2026-10-28。

吸收 [workbuddy-manager](https://github.com/ithtelab/workbuddy-manager) v2.10.0..v2.11.4 中**适用于本 Go 网关**的更新（`dev/upstream-patches/0030..0040`），并保留本仓库既有的账号池、鉴权、分发密钥与三种对话协议。

## 吸收内容

### 1. 11-128 渠道校验改为请求级（不再罚账号）

- 首次被上游以 `11-128` 拒绝时，先做一次**请求级抢救**：把 system/developer 文本原样搬进首条 user、system 换成中性提示词后重试（只换承载通道，不改内容）。
- 仍被拒就透传上游原文回 `400 channel_check_blocked`，**不冷却、不禁用、不换号、不解绑粘性会话**。旧 `channel_check_cooldown` / `fail_fast` 字段保留但取值被忽略。
- 守卫的抓包与 body/模板负缓存保留，但只缓存**最终失败**的原始出站模板；抢救成功的请求不缓存，避免后续同请求被本地拦住而拿不到抢救机会。
- `gateway_hint` 不再误报“account-level fault / will rotate or disable”。`HintContext` 新增 `ChannelCheck` 权威标记：`Error.Msg` 只保留前 200 字符，渠道校验文案常在截断点之后，不能靠重新解析。

### 2. DSH 指纹脱敏扩展

- 新增 `workspace root` → `workspace base` 改写：只命中 DSH file-reference 完整模板句 `Relative paths resolve from the workspace root`（上游 2026-10-27 新增词；该句每个请求都会带），普通用户文本/工具描述中的 `workspace root(s)` 不动。
- 身份句动词集合从 2 个扩到 7 个（powered by / built on / built by / created by / developed by / made by / trained by）。
- 特征预检不再整体放宽：内置 `sanitizeFeatures` 保持精确匹配（大小写敏感，避免小写命中后只做 `TrimSpace` 吃掉空白）；DSH / workspace 正则与 `sanitize_extra_features` 走大小写不敏感，避免大写复述漏网。
- 新增运行期词表 `features.sanitize_extra_features` / `sanitize_extra_rewrites`：上游逐词扩充黑名单时改配置重启即可，不必重编译。
- 修正 Web GUI 替换串的空操作：`sanitizeDshGuiRepl` 原值与匹配串逐字相同，`ReplaceAllString` 从未生效；现改为 `DeepSeek-Harness Web-GUI`，并兼容 `DeepSeek-Harness` / `DeepSeek Harness` 两种写法。

### 3. 11102 模型负缓存自愈

- 11102 首次冷却从 6h 缩短到 **2m**，指数退避封顶从 24h 缩短到 **10m**；一次偶发注册表抖动不再把整池锁死一整天。
- 选号侧发现当前 realm 对该模型被 11102 负缓存整体挡死时，自动清一次**当前 realm** 的 11102 负缓存并重试；只清一次，模型真下线时仍按上游原文返回 503。
- 判据用 `ModelBlockedNow(realm, model)`（与选号同一 realm 维度）；清理用 `ClearModelBlocksForRealm(model, realm)`，双 realm 部署下不会误清另一域的负缓存，也不会因另一域还有可选账号而永不触发。
- 新增管理端点 `POST /admin/accounts/{uid}/models/{model}/available`：管理端探测成功后只解除该账号该模型的 11102 负缓存，不碰 6004 配额与手动停用。

### 4. Responses 孤儿工具结果

客户端只下发 `function_call_output` / `custom_tool_call_output` 而没有对应 `function_call` 时（Codex 创建子会话的形状），不再写成 `role=tool` 触发上游 11148：
- 带 `call_id` 且命中 `openCallIDs`（最近一组已发出、尚未被消费的调用）：按 tool 消息处理；
- 带 `call_id` 但不在 `openCallIDs`（没有对应 `function_call` 的孤儿输出）：与无 `call_id` 孤儿一样降级为 user 消息；
- 无 `call_id` 但有未消费调用：按声明顺序配对；
- 孤儿结果：降级为 user 消息并保留 `[tool output: <name>]` 内容。

### 5. 大请求体 gzip

- `upstream.gzip_body`（默认 true，`WB2A_GZIP_BODY` 可覆盖）：chat 请求体超过 64KB 时 gzip 压缩后发上游；短请求零开销。
- 上游对压缩体返回**任意 4xx**（含 415 / 解析失败类 400 / 11140 / 渠道校验）时，自动去压缩重发**同一条路径**一次（5xx 不重试）；压缩只是省带宽，不该为它换来整轮失败或账号误禁用。
- 启动时读 `upstream.gzip_body` / `WB2A_GZIP_BODY` 作为默认值；面板保存配置时经 `HotFields` 快照热改（物化进运行态），无需重启；面板 UI 未新增开关时直接编辑 `config.json` 即可。`features.sanitize_extra_*` 才是启动期字段，改动需重启。

## 复审修复（前台子代理并行审查后）

- **Responses 孤儿输出**：带 `call_id` 但不在 `openCallIDs` 的结果同样降级为 user（否则会被上游 `cleanupOrphanToolCalls` 整条删除）；`openCallIDs` 改为只属于最近一组 tool calls，并在普通消息回合边界清空，避免跨轮错配。
- **11102 自愈**：判据改为 `ModelBlockedNow(realm, model)` + `ClearModelBlocksForRealm`（只清当前 realm，且只清 reason 以 `11102` 开头的条目，6004 配额不动）；新增 2 分钟跨请求闸门；清 0 条或清完仍 blocked（还有 6004）时不重试；自愈重试用 `i--` 不占轮转名额。旧 `state.json` 的 6h/24h 11102 条目恢复时钳到 10m；`Hits` 在到期后保留 10m 宽限期，指数退避不再被 prune 重置。
- **搜索路径 11-128**：`/v1/search/messages` 直接返回 400 终止轮转，不再打满 `MaxRotate` 个账号；账号零冷却、零禁用。
- **管理面鉴权**：`wbk_` 分发密钥不再能调用 `/admin/*`（含 disable/enable/revive/models/available），返回 `403 admin_requires_gateway_key`；观测/管理面 `/status`、`/api/request_logs`、`/v1/stats`、`/v1/stats/reset` 同样只认全局 api_key 或面板 session，`wbk_` 一律 403；模型调用面（`/v1/models`、chat/messages/responses）不受影响（`/v1/search/messages` 仍按既有口径拒绝 `wbk_`）。
- **`MoveSystemToUser`**：遇到非字符串 content 的 system/developer 时保守返回 false，避免数组/对象形态的提示词被整条删除；空/null content 仍可安全跳过。
- **脱敏**：`workspace root` 只改写 DSH 完整模板句 `Relative paths resolve from the workspace root`（加词边界，普通用户文本/工具描述不再误改）；内置 `sanitizeFeatures` 恢复精确预检（大小写敏感，避免小写命中只做 `TrimSpace`）；DSH / workspace 正则与 `sanitize_extra_features` 大小写不敏感；extra rewrites 先于内置改写执行、按大小写不敏感匹配、拒绝空/纯空白源串与空 replacement，且工具参数回写前校验 JSON，避免配置把请求改坏。
- **gzip**：压缩体命中**任意 4xx** 时先按同一条路径未压缩重试，避免上游把 gzip 请求误判成 11140 request illegal 而误禁用账号；5xx 仅在响应含强渠道文案时重试（普通 5xx 保持原语义）。闩锁只在**未压缩重试成功**且压缩版拒绝明确指向 gzip/渠道（415、解析失败类 400、11101、11140、channel verification / unapproved channel）时置 10 分钟，期间直接发未压缩，避免长上下文每次双发；未压缩仍 4xx 不置闩锁。面板保存按 `WB2A_GZIP_BODY` 重新应用；`features.sanitize_extra_*` 加入重启清单。
- **弱 11-128 封套**：只有 11-128 业务码 + `request illegal`、没有强渠道文案的封套归请求级 `ErrContentBlocked`（不罚号、不禁用、走内容拦截降级/400），不再落入 `accountFaultRule` 的 request illegal 硬禁用分支。
- **工具定义 name 不改写**：`sanitizeTools` 只改 description / parameters；工具名是客户端与模型之间的调用契约，改写 definition name 会造成历史 tool_calls/tool_choice 不一致、客户端不认识上游返回的新名。工具名含指纹时宁可让请求按原语义失败。
- **流式/非流式渠道校验**：SSE error 帧与非流式 Aggregate 中的强渠道校验也按请求级处理——不冷却、不禁用、不解绑粘性；非流式直接回 400 `channel_check_blocked`，不再写成 502 upstream_parse。弱 11-128（仅 message + code）的 error 帧同样请求级：流式不罚号/不解绑，Aggregate 回 400 `content_blocked`（`FrameKind` 只看 message 会丢 code，handler 用 `IsWeakChannelCode` 兜底）。
- **`custom` 提示词模式**：11-128 的 system→user 抢救只在 `passthrough` / `append` 下执行；`custom` 模式直接用网关提示词，首次强渠道校验即透传 400（文档已注明该例外）。
- **面板设置页**：移除已废弃的 `fail_fast` / `channel_check_cooldown` 字段（后端仍兼容旧配置但取值被忽略），`mode` 描述明确只控制本地预检；嵌入 `internal/panel/dist` 已重新构建，设置页不再出现会误导的旧开关。
- **测试**：新增/更新 Responses 带 call_id 孤儿与跨轮错配、自愈跨请求闸门/跨 realm 保留/MaxRotate=1、admin wbk_ 403、MoveSystemToUser 混合 content、脱敏负例与 JSON 校验、gzip 4xx 安全重试/闩锁/面板 env/重启清单等用例；`go test ./...` 与 `go vet ./...` 全绿。

## 未吸收项与原因

- **/console 路径优先与路径记忆（0030/0031/0032/0037）**：本网关自 #119 起刻意固定 `/v2`（`/console` 挂腾讯云 WAF body 内容规则，命令执行特征确定性 403）。11-128 已按请求级处理，不需要再为账号切换备用路径。
- **面板/部署类更新**：watchdog 页面监控、SMTP/QQ 通知、自动更新、Windows 启动器、systemd 降权、解禁队列、更新弹窗、积分图表等均为 manager 自有 UI/部署形态，本仓库无对应面。
- **管理面安全批次（868befe）**：CORS `*`、Cookie `X-Forwarded-Proto`、登录用户名枚举、`/api` 分块请求体上限等均为 FastAPI 管理面问题；本仓库已用 `http.MaxBytesReader`、IP 规则中间件与不同的鉴权模型覆盖同口径。
- **密钥白名单前缀归一（f91700f）**：本仓库 `internal/keystore` 的 `Validate` 与 `/v1/models` 裁剪已对白名单两侧统一 `bareModel`，行为等价。
- **三条协议思考档位入日志（7494958）**：Anthropic / Responses 均转换为 OpenAI 请求体后复用 `chatCompletions`，`effort_in/effort_out` 已在同一管线记录（v1.21.0 已验证）。

## 配置变更

新增：

```jsonc
{
  "upstream": { "gzip_body": true },
  "features": {
    "sanitize_extra_features": [],
    "sanitize_extra_rewrites": []
  }
}
```

废弃（保留字段、取值被忽略）：

```jsonc
{
  "features": {
    "channel_check_guard": {
      "channel_check_cooldown": "60s",
      "fail_fast": true
    }
  }
}
```

## 验证

- `go test ./... -count=1` 全绿。
- `go vet ./...` 通过。
- 新增/更新测试：11-128 请求级与 system→user 抢救、原始模板负缓存、ChannelCheck hint 截断、workspace root / 7 动词 / 可配置词表 / GUI 连字符、11102 TTL 与 realm 自愈、Responses 孤儿输出配对、gzip 阈值/任意 4xx/渠道校验去压缩重试、wbk_ 管理/观测面 403、管理端探测回执。

## 迁移

1. 备份 `config`、`auths`、`data`。
2. 替换程序并重启（`features.sanitize_extra_*` 为启动期字段，改动需重启；`upstream.gzip_body` 启动时读配置 / `WB2A_GZIP_BODY` 作为默认值，之后面板保存经 `HotFields` 热改，无需重启）。
3. 旧配置无需修改：`channel_check_cooldown` / `fail_fast` 会被忽略，不会报错。
4. 升级后如需立即验证 11-128：`data/channel_check/` 下会保留最终失败请求的抓包与 `.meta.json`；确认是新指纹时把词加进 `features.sanitize_extra_features` + `sanitize_extra_rewrites` 后重启。
