# WorkBuddy Cockpit v1.24.0

发布日期：2026-11-13。

吸收 [workbuddy-manager](https://github.com/ithtelab/workbuddy-manager)
v2.11.10..v2.11.14 中适用于本 Go 网关与内嵌面板的更新，并保留本仓库既有的
账号池、鉴权、分发密钥、双协议与状态持久化模型。

## 吸收内容

### 1. 模型级限流保留到上游真实重置时刻

- 6004 的本地软冷却窗口（`cooldown.soft_rate_max`）先到期、但上游
  `ResetAt` 仍在未来时，`/status` 的 `rate_limited_models`、模型锁池视图、
  选号判定与持久化都不再视为已恢复。
- 签到解冻、余额刷新与账号级冷却不再清空活跃的模型级限流台账；只有账号被
  自动禁用（终态）或面板「强制清冷却」才整体清空。
- 这样面板不会显示「已恢复（绿点）」后一调用又撞 6004，也不会把健康号反复
  送去触发必然失败的请求。

### 2. 429 返回最早恢复时间

- 多号同时限流时，轮转过程中记录最早的上游重置墙钟；末端 429 的
  `message`、`gateway_hint` 与 `Retry-After` 都取最早恢复的那条，而不是最后
  轮到的号的（更晚，会让客户端白等一整个周期）。
- 429 响应仍透传上游原文（code/msg/requestId 原样）。

### 3. 6020 排队不再误伤账号

- `429 + code 6020 / queue.waiting` 是上游全局排队，不是账号级限流。
- 该形态不冷却、不计数、不喂连败、不禁用账号；请求仍按 `MaxRotate` 轮转，
  最终以 429 返回，客户端按队列提示等待。
- `200 + 6020` 文案保持原有分类（不改变成功响应语义）。

### 4. 客户端余额协议

新增三个端点：

| 端点 | 鉴权 | 用途 |
|---|---|---|
| `GET /v1/credits` | 网关 key / 面板 session | RikkaHub 等客户端的默认余额路径；`total_usage` 与 `total_available` 同值 |
| `GET /api/usage/token/` | 网关 key / 面板 session | new-api 风格余额面板；`total_granted` / `total_used` / `total_available` |
| `GET /api/status` | 无（公开元数据） | 只声明单位 `UNIT`，不含任何额度 |

- 两份额度都设上限时，取更紧的剩余比例再折算回积分；只有一份设上限时直接
  报它的剩余量；都不设上限时报账号池可用积分。
- 账号池可用积分排除自动封禁与手动停用账号；冷却中的账号仍算可用（余额还能
  在恢复后花掉），与面板首页「可用积分」同口径。
- 支持 `?scope=cn|global|merged`；密钥本身限定 realm 时默认该档，否则默认
  `merged`。本项目不按「最近一次真实调用」自动判定（请求日志/用量桶没有按
  密钥维度落库），显式 scope 已覆盖实际需求。
- 余额端点只校验密钥本身，**不套用**停用/过期/配额/限流拒绝：停用的密钥仍
  能读到自己还剩多少——这恰恰是调不动时最需要的信息。IP 黑白名单照常生效。
- `/api/status` 从「面板未注册路径 404」变为公开元数据端点；不含账号、密钥
  或额度信息。

### 5. 净化词表热重载

- 修改 `config.json` 的 `features.sanitize_extra_features` /
  `sanitize_extra_rewrites` 后 10 秒内自动生效，不必重启容器。
- 词表整体替换且原子换指针，读路径（请求体净化）不会看到半新半旧的表。
- 配置解析失败只记 WARN 并保留旧词表，不会因一次手误把正在跑的净化关掉。
- 面板保存这两个键后不再提示「需要重启」（原先被列入 restart-required 清单）。
- 新增 `upstream.SanitizeExtraCounts()` 供启动与热重载日志回显条数。

### 6. 登录页表单安全

- 登录表单显式 `method="post"`：静态页面在 JS 就绪前的原生 GET 提交不会再
  把用户名/密码写进地址栏。
- 登录按钮在 auth 状态未就绪时同样禁用（原先只禁用提交中的 `busy`）。
- 用户名的 `placeholder` 从伪装的 `admin` 改为「用户名（任意值）」——本仓库
  的登录服务端**忽略用户名**，只校验密码（`api_key`），占位符不应让人误以为
  已经填了默认账号。

## 已存在、未重复移植

- 积分流水正差额记账：本仓库 `CreditTracker.RecordBalance` 一直只记
  `delta > 0`，上游「消费被重复记成收入」的修复已由该口径覆盖；本仓库没有
  manager 的积分包级台账，故不引入 `CreateTime`/按到期时间分组的记账。
- 模型目录裁剪：本仓库 `/v1/models` 一直按上游动态目录生成，不做静态名单。
- Docker 优雅退出：Go 二进制直接作为 PID 1 接收 SIGTERM，已有
  `srv.Shutdown` 与落盘收尾，不依赖 manager 的 uvicorn 参数。
- 余额回退本地快照：本仓库余额端点直接读进程内池快照（签到/余额刷新回写），
  没有 manager 的 Python 中间层去拉上游 `/status`，因此不存在「上游整池报 0
  时改用本地快照」这一层问题。
- 登录用户名提示：本仓库用户名被服务端忽略，占位符改为「任意值」而不是
  manager 的「默认 admin」。

## 未吸收项与原因

- **调用抓包 / 轨迹（Trace Tap）**：manager 的实现包含独立采集中间层、DB
  表、流式正文重建、JSON diff 与单文件 HTML 导出，是一套新的存储与前端子
  系统；且会落盘完整提示词/响应正文（敏感数据面显著扩大）。本仓库请求日志
  一贯只存元数据，本轮不引入，留待单独设计保留期、脱敏与访问控制后再做。
- **出站 HTTP/2 开关**：本仓库出站 Transport 刻意置空 `TLSNextProto` 强制
  HTTP/1.1，原因是实测半死 h2 流复用时表现为 `timeout awaiting response
  headers`。上游开关默认关闭，开与不开都不影响既有行为，但为一个未验证需求
  重新暴露已知故障面不划算，故不加。
- **重登队列按目标账号锁定版本/文件**：本仓库没有 manager 式重登队列；扫码
  加号流程由上游返回 uid 后落到 `auths/workbuddy-<uid>.json`，天然只写目标
  账号，不存在「拿国内版令牌刷国际版账号」的队列上下文。
- **余额按最近一次真实调用自动分档**：见上文，缺少按密钥维度的请求日志；
  显式 `?scope=` 与 `key.realm` 已覆盖双版本部署。
- **账号 × 模型报表与趋势成功曲线**：属于面板统计页的增量可视化，不影响
  网关正确性；本轮不扩大前端改动面。
- **代理测活诊断、双调度器冲突提示、令牌刷新频率提示、表格居中、Docker
  更新校验 auths 挂载**：依赖 manager 的 Python 中间层、独立调度器或特定
  部署形态；本仓库没有对应数据源/第二套调度器，部署文档已单独说明挂载要求。

## 配置变更

无新增配置键。净化词表热重载复用既有：

```jsonc
{
  "features": {
    "sanitize_extra_features": ["..."],
    "sanitize_extra_rewrites": [["old", "new"]]
  }
}
```

## 迁移

1. 备份 `config.json`、`auths/`、`data/`。
2. 替换程序并重启。
3. 客户端可把余额地址指向 `<base>/v1/credits`（OpenAI 兼容客户端）或
   `<base>/api/usage/token/`（new-api 风格）；需带同一把网关 key。
4. 若有脚本依赖 `/api/status` 返回 404，请改为检查 `success` 字段，或用
   `/healthz` 做健康检查。

## 验证

- `go test ./cmd/... ./internal/... -count=1`
- `go vet ./cmd/... ./internal/...`
- `go test -race ./internal/upstream -count=1`
- 面板 `npm run build:export`
- 新增测试覆盖：模型冷却跨 Until 保留与持久化、账号级冷却/解冻保留模型台账、
  6020 分类与不罚号、429 最早恢复时间与 Retry-After、余额端点三档计算 /
  scope / 停用密钥可读 / 公开元数据、净化词表热重载。
