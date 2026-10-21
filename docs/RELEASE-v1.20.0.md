# WorkBuddy Cockpit v1.20.0

发布日期：2026-10-22。

吸收 [workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel/commit/73a7560efc7a88ebd9865db4c71ae7f6d85859f8) 和 [workbuddy-manager](https://github.com/ithtelab/workbuddy-manager/commit/77dce7e8cc161539b3fc7b658fa342d5e31cb53a) 的可适用更新，继续使用单 Go 二进制及内嵌面板。保留现有账号池、鉴权、分发密钥和三种对话协议。

## 新增

- **账号手写权重**：面板账号页可设置 `0.1–100` 的选号乘数，默认 1；API 设 0 可清除自定义值。凭据持久化、导入导出、token 刷新均保留。权重不绕过禁用、冷却、在途并发及成本分层，`credits_desc` 仍按积分降序选择。
- **联网搜索入口**：`POST /v1/search/messages`，返回 Anthropic `web_search_tool_result` 兼容结构。使用全局 `api_key` 或管理员面板会话，共用 IP 规则、账号池和错误策略。搜索尚无独立配额记账，`wbk_` 分发密钥显式拒绝；不支持的域名/位置约束及流式参数显式返回 400。
- **任务奖励补扫**：`schedule.task_sweep_enabled` 默认 false，`task_sweep_interval_minutes` 最小 15。只领取已达标奖励，不发起对话或执行任务动作；跳过国际版、企业号及禁用账号，与任务锁和队列互斥。
- **reasoning-only 兼容**：顶层 `reasoning_as_content` 默认 false。显式开启后，整轮没有正文或工具调用时将思考文本补为正文；支持 reasoning 字段的客户端保持默认可避免重复显示。保存配置后需要重启。

## 修复

- DSH system prompt 指纹改写及快速预检；渠道校验拒绝使用软冷却，避免永久禁用健康账号，分类不依赖截断后的错误文本。
- 11151 空消息错误立即返回确定性 400，不再轮转账号池或惩罚账号。
- reasoning/content、finish 和 usage 的流式帧序修复。
- 上游流截断或读失败在 `[DONE]` 之前报告 `upstream_truncated` / `upstream_read_error`，日志及指标记为失败；非流式聚合与 Anthropic/Responses 同样拒绝伪成功。
- 国际版账号使用只读余额刷新，保留 token 预刷新、积分流水、取消/错峰和强制冷却语义。
- 国际版 DeepSeek V4.1 Flash 补齐 `high` / `max` 档位与请求支持。

## 升级

1. 停服务备份现有程序和 `config`、`auths`、`data`，校验备份可读；备份含凭据，不要上传到公开仓库或 Release。
2. 按平台下载主程序，使用同一 Release 的 `sha256sums-v1.20.0.txt` 校验。发行二进制内嵌面板；不包含个人配置、凭据、日志或业务数据。
3. 保留原配置和数据挂载，替换程序并重启。新增两个开关默认关闭，无需为升级启用额外任务。
4. 检查面板版本、健康、账号集合及鉴权。R28S / OpenWrt 的运行文件校验和程序回退见 [部署手册](DEPLOY-R28S.md)。

## 验证与限制

- Go 全量测试、vet、构建，前端 TypeScript 检查与静态导出、内嵌面板测试已通过。ARM64 部署验证覆盖程序哈希、账号池、面板资源、鉴权及搜索路由参数校验。
- 搜索和奖励补扫使用 mock upstream 回归测试；未执行真实外部搜索或奖励领取，不能视为真实上游调用已验证。
- 任务 API 的在途 HTTP 请求尚无 context 参数；取消阻止后续请求，服务退出最多等待 5 秒。
- 本地环境未具备 race 检测所需的 C 工具链，未宣称通过 race。
- 本次不引入 Python 中间层、用户角色数据库、多上游分组、pgsync、自更新器或公开领取接口。
