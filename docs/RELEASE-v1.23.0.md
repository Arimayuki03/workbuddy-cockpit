# WorkBuddy Cockpit v1.23.0

发布日期：2026-11-04。

吸收 [workbuddy-manager](https://github.com/ithtelab/workbuddy-manager)
`v2.11.5..v2.11.10` 中适用于本 Go 网关与面板的更新，并保留本仓库既有的
账号池、鉴权、分发密钥、双协议与状态持久化模型。

## 吸收内容

### 1. 大上下文改为账号级滑动预算

- 请求体达到 `pool.large_request_bytes`（默认 2 MiB）后计入账号级预算。
- 单号在 `pool.large_request_budget_window`（默认 120s）内承接达到
  `pool.large_request_budget`（默认 6 条）后，仅下一条大请求迁移会话；成功后在
  新账号上重新绑定，之后继续命中新账号的前缀缓存。
- 不再采用 v2.11.7 的「大请求逐请求换号」默认策略。逐请求轮换只在
  `large_request_budget < 0` 且 `large_request_cooldown > 0` 时启用，避免默认
  打掉长对话缓存。
- 全池都达到预算时退化为普通轮换，不直接返回 503。

### 2. 网关错误契约

- 上游实际返回 11102「模型不存在」时，不再在轮转耗尽后伪装成
  `503 no_healthy_account`；改为 `404 model_not_found`，保留上游原文与
  `gateway_hint`。
- 上游 429 的响应头或「将在 ... 重置」文案会换算成标准 `Retry-After`，
  上限 1 天。
- `/v1`、`/v2` 下未注册路径与方法不匹配统一返回 OpenAI 形状 JSON：
  `404 not_found` / `405 method_not_allowed`。不会再把静态站 HTML 当成 API
  响应。

### 3. 模型名与白名单

- CN 模型在 `/v1/models` 与面板模型目录中改用裸名，与国际版 `global:` 前缀
  形成一致口径；旧客户端显式发送 `cn:xxx` 继续可用。
- `scripts/probe_max_tokens.py` 默认探测全部模型；面板继续兼容读取旧版按
  `cn:` 前缀保存的实测上限记录。
- 未锁定 realm 的密钥白名单中：裸名视为「同名模型的任意版本」；显式
  `cn:` / `global:` 只放行对应版本。列表裁剪与调用校验共用同一语义。
- 密钥锁定 realm 时仍由密钥统一约束版本，白名单只比较裸名。

### 4. Responses 与脱敏

- 流式 Responses 的 `response.reasoning_summary_text.delta` 补上规范必填的
  `summary_index: 0`。ai-sdk 系客户端不再把思考增量整帧丢弃。
- DSH file-reference 首句 `Tokens prefixed with @ are paths ...` 纳入脱敏，
  改为 `Tokens prefixed with the at-sign ...`；后文 `@"..."` 示例保持原样。

### 5. 账号状态与出口线路

- 11140 带 `unusual activity` / 异常行为话术时明确标记为上游封禁、需要申诉，
  不再提示重新登录；普通 `11140 request illegal` 与 401 会话失效仍提示重登。
- 面板积分卡拆分可用与不可用余额：冷却账号仍算可用；封禁、手动停用与状态未知
  的余额单独统计。
- 兼容 manager 的 `__direct__` 直连保留名。
- `signin` / `trial` / `activity` / `credit` 独立工具启动时读取同一份
  `config.json` 的 `proxy_routes`；读取失败会明确记日志，绑定线路的账号不会
  静默回退默认出口。

### 6. 网络监听

- Windows 绑定 `::` 时显式关闭 `IPV6_V6ONLY`，IPv4 回环与网卡可正常访问
  双栈监听。非 Windows 平台沿用系统默认语义。

## 已存在、未重复移植

- `pool.credit_floor` 积分保底：本仓库已有更细的实测成本台账与目录倍率兜底。
- 管理端任务按账号错峰：本仓库已有 `schedule.account_delay_*` 与确定性调度。
- 11102 realm 自愈、11-128 请求级处理、gzip 大请求体、Responses 孤儿工具输出：
  v1.22.0 已吸收。

## 未吸收项与原因

- **DeepSeek 官方价估算**：本仓库用量统计已有上游真实 `usage.credit` 口径，
  且当前分桶未保存官方价所需的逐请求缓存命中/未命中拆分。再叠一套估算价容易
  与真实扣费混淆，本次不引入。
- **按添加时间排序**：Go 侧账号来自 `auths/*.json`，没有可靠的历史添加时间。
  用文件 mtime 或令牌刷新时间冒充会误导用户，因此不加入。
- **历史密钥再次查看**：本仓库密钥库按设计只保存 SHA-256；为回看而新增明文
  存储会扩大泄露面，与现有安全模型不符。
- **manager 的独立保护暂停、预警数据库、解禁队列、更新弹窗、自动更新、
  Windows 启动器、watchdog/SMTP/QQ、页面监控、账户回填日志诊断**：依赖 manager
  的 Python 管理面、SQLite 状态或独立部署形态；本仓库没有对应数据源，照搬会
  形成第二套状态真相。

## 配置变更

新增：

```jsonc
{
  "pool": {
    "large_request_bytes": 2097152,
    "large_request_budget": 6,
    "large_request_budget_window": "120s",
    "large_request_cooldown": "0s"
  }
}
```

- `large_request_bytes` / `large_request_budget`：负值关闭对应策略；0 表示
  未设置并回落默认。
- `large_request_budget_window`：预算滑动窗口。
- `large_request_cooldown`：legacy 逐请求轮换窗口，仅在预算关闭时生效。

## 迁移

1. 备份 `config.json`、`auths/`、`data/`。
2. 替换程序并重启。大请求预算四键在面板保存后需重启生效。
3. 旧客户端继续发送 `cn:xxx` 无需改动；新客户端从 `/v1/models` 拉取时将看到
   CN 裸名。
4. 白名单里若写了裸名且只想放行 CN，应改成显式 `cn:xxx`；想同时放行两版则
   保持裸名。

## 验证

- `go test ./... -count=1`
- `go vet ./...`
- `go test -tags embed_panel ./cmd/server -count=1`
- 面板 `npm run build:export`
- 面板积分拆分 `node --test lib/credit-availability.test.mjs`
- `python -m py_compile scripts/probe_max_tokens.py`
- 新增测试覆盖大请求预算与会话迁移、11102→404、429 `Retry-After`、`/v1`/`/v2`
  JSON 404/405、Responses `summary_index`、白名单版本语义、`__direct__`、
  独立工具线路加载、11140 封禁原因与 Windows 双栈监听。
