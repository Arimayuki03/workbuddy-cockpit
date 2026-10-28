# WorkBuddy Cockpit v1.21.0

发布日期：2026-10-26。

吸收 [workbuddy-manager](https://github.com/ithtelab/workbuddy-manager) 77dce7e8..v2.10.0（24 提交）中的可适用更新（8 项），并落地渠道校验守卫、思考强度全链路观测与一批面板修复。保留现有账号池、鉴权、分发密钥与三种对话协议。

## 新增

- **渠道校验守卫（11-128 防风控）**：上游以「没通过渠道校验 / Illegal API invocation from an unapproved channel」拒绝请求时——
  - **抓包留证**：把当前出站请求体按 body 去重落盘到 `data/channel_check/`（附 `.meta.json`：模型/账号/上游原文/system 预览），目录总量封顶自动删最旧；本地拦截命中同样留证；
  - **本地拦截**：按完整 body 哈希与模板哈希（system/developer + tools）做短 TTL 负缓存，窗口内同 body / 同模板直接本地 400 `channel_check_blocked`，不再选号、不再冷却账号、不再把确定性拒绝伪装成 503；
  - **专用冷却**：渠道校验命中走 `channel_check_cooldown`（默认 60s），不再占用 `cooldown.soft_rate`（可能 6h）；填 0 回落旧口径；
  - **fail_fast**（默认 false）：首个号被拒即终止轮转并回 400；
  - **预置拦截短语**：`block_patterns` 命中在出站前直接本地拒绝（默认空 = 不猜，先看抓包再填）；
  - 面板设置页新增「渠道校验守卫（防风控）」分组，**全字段保存即热生效**（守卫实例整体热替换，无需重启）。
- **思考强度全链路观测**：请求日志新增 `effort_in` / `effort_out`（下游入站档位 > 出站最终档位——补默认档与按模型支持档降级后的值，与出站管线同口径推算）、`reasoning_tokens` / `prompt_tokens` / `duration_ms` / `realm`；stdout 流水行新增 `eff=入站>出站` 列；面板日志页「思考强度」列与详情行。OpenAI / Anthropic / Responses 三协议自动覆盖。
- **请求日志持久化与来源采集面板化**：设置页新增「日志观测」分组——`request_archive` / 保留天数 / 容量上限（需重启）、`request_client_info`（保存即热生效）。

## 吸收 manager v2.10.0

- **DSH Web GUI 跨句指纹净化**：上游按 221 字符**跨两句连续文本**逐字匹配拦 11-128（单句放行、差一字符即拦），单句改写无效；对段落内品牌短语 `DeepSeek Harness Web GUI` 插连字符拆开匹配。
- **纯 401 连续计数禁用**：无 12153 / 无业务信封的 401（APISIX 拦截页/空体）新增 `ErrAuthRejected`，与 12153 共用连续计数（3 次达阈禁用，reason=`401 auth rejected`），坏号不再「降权 10 分钟→回池→再失败」循环。本仓库保留「401+HTML 网关页立即禁用」的既有从宽判定（测试注明分叉）。
- **Kimi 工具 schema 兼容**：`enum` 缺 `type` 的工具属性出站前按取值推断补齐（string/integer/number/boolean；混合标量不动）；上游 11133 同步改为快速失败——400 透传原文，不轮转、不喂连败（此前一次 Kimi 探测把健康号连败计数喂高）。
- **空流可重试**：error 帧 message 改为 `upstream stream ended before completion (…)`，OpenAI 兼容客户端按传输层错误自动重试。
- **签到限速与重试**：账号间最小间隔 3s（批量签到 3 秒并发会撞 `10001 请稍后重试`）；瞬时失败（HTTP 5xx / code 10001）按 5s/15s/60s 退避重试，不再错过当天签到。
- **会话粘性首绑三因子加权**（issue #265）：首绑从哈希取模改为 `pool.WeightedPickAmong` 三因子抽签（credits ×10 + 快过期积分 ×8 + 闲置补偿），「快过期积分优先消耗」对粘性流量生效；`credits_desc` 的严格序优先序不受影响。
- **构建 GOPROXY**：Dockerfile 默认 `goproxy.cn`（`--build-arg GOPROXY=` 可覆盖），修国内 `go mod download` 超时。

## 修复

- **提示词缓存写入恒 0**：上游 CN /v2 从不下发 `prompt_cache_write_tokens`（双轮探针实证：首轮 miss=2858/write=0，次轮 hit=2688）。改为推导：带缓存键的请求 `write = miss`（≥64 token 最小缓存块才计入；上游真值优先）。流式 / 非流式 / `/v1/stats` 三处同口径，推导精度实测约 94%（差额为尾部活跃段不入缓存）。
- **面板提示词模式切换报错**：settings 字段解析被「按字段名全局映射」覆盖（`prompt.mode` 与守卫分组 `mode` 同名，后者注册在后），切换提示词模式恒报「取值不在允许范围内」。改为**分组内解析**（`fieldOf`），删除全局映射，开发期对跨分组重名字段告警。
- **请求日志页固定列宽**：auto 布局下 36 字符 UUID / 长 UA / 模型名把表格推宽出横向滚动。改 `table-fixed` 百分比定宽 + 列内省略（悬停看全文）；账号列显示 uid8（与网关流水行同口径，详情有全量），11 列一屏完整呈现。
- **stdout 流水行定宽**：账号列 22→18 定宽（昵称按显示列宽截断、uid8 后缀永不截断），掩码手机号昵称不再把尾部列顶出可视区；eff 列 11→15（`minimal>minimal` 不再截断）。新增 `logfmt.TruncateWidth` 按显示列宽截断（CJK 对齐安全）。
- **轮转末端丢失渠道校验标记**：`lastErr` 不携带 `ChannelCheck`，轮转耗尽时渠道校验被伪装成 503 `no_healthy_account`；现在透传上游原文并回 400 `channel_check_blocked`。
- 守卫预检命中（负缓存/短语）同样落盘留证；`request_client_info` 改走 live 快照（面板热改即时生效）。

## 升级

1. 停服务备份现有程序和 `config`、`auths`、`data`，校验备份可读；备份含凭据，不要上传到公开仓库或 Release。
2. 按平台下载主程序，使用同一 Release 的 `sha256sums-v1.21.0.txt` 校验。发行二进制内嵌面板；不包含个人配置、凭据、日志或业务数据。
3. 保留原配置和数据挂载，替换程序并重启。新配置段 `features.channel_check_guard` 默认启用（mode=block、TTL 10m、冷却 60s）、`logging` 段默认关闭归档，无需为升级启用额外任务；不想要守卫可显式 `enabled=false` 或 `mode=off`。
4. 检查面板版本、健康、账号集合及鉴权。R28S / 手机容器的运行文件校验和程序回退见既有部署手册。

## 验证与限制

- Go 全量测试、vet、构建通过；面板 tsc / eslint / 静态导出通过，dist 已重新内嵌。`internal/pool` 两个冷却用例在本地时间 03:00–04:00 窗口固有失败（断言「距次日 04:00 至少 1 小时」，与内置签到时刻相关，非本轮引入）。
- 新增测试覆盖：Web GUI 指纹净化（6 组）、401 分类与跨形态计数、工具 enum 推断（6 组）、11133 快速失败与 requestId 撞串反例、签到重试、加权首绑分布（确定性随机源）、缓存写入推导（3 组）、effort 观测端到端、流水行定宽。
- 守卫抓包/拦截、缓存写入推导、思考强度字段与固定列宽均在 ARM64 真机（Android 容器部署）实测验证；未执行真实上游渠道校验的定向复现（触发依赖上游策略），拦截路径以单元测试与真实流量观察为准。
- 本轮不引入 Python 中间层、数据库、多上游分组或自更新器。
