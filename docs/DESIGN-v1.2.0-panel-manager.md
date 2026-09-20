# WorkBuddy Cockpit — v1.2.0 整合设计文档（定稿）

> 状态：**定稿**（经三轮设计拷问确认，2026-09-21）
> 本文档是 v1.2.0「面板整合」版本的唯一设计依据；实施过程中的偏离必须先修订本文档。

## 0. 一句话

以本 fork（Arimayuki03/workbuddy2api）为基座，移植
[linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)
的后端管理功能，前端采用
[ithtelab/workbuddy-manager](https://github.com/ithtelab/workbuddy-manager)
的 Next.js 静态导出壳（go:embed 内嵌），构成「OpenAI 兼容网关 + 全功能 Web 管理面板」一体的单二进制项目。
新对外名称：**WorkBuddy Cockpit**（GitHub 仓库名 `workbuddy-cockpit`）。

## 1. 背景与三个源仓库的关系

| 仓库 | 性质 | 与本项目的关系 |
|---|---|---|
| [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)（上游） | Go 网关，无面板 | 代码基座；fork 在其上加 /admin REST、/v1/stats、in_flight_by_model、cost_explore、bat 菜单等 |
| [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)（下称 panel） | 上游的手工维护增强分支，Go 网关 + go:embed 原生 HTML/JS 面板（/panel/api/*），v1.11.1 | **后端功能移植源**。与上游无 fork 关系、与本仓库无共同 git 祖先；auths/ 凭证格式同源 |
| [ithtelab/workbuddy-manager](https://github.com/ithtelab/workbuddy-manager)（下称 manager） | FastAPI 中间层 + Next.js 15/React 19/shadcn-ui 静态导出前端 + SQLite 全栈应用，五语言 i18n，v1.0.60 | **前端壳移植源**。其前端从不直连 Go 网关（60+ 个 /api/* 全打自身 Python 后端），故"抄前端"的实质是：抄 UI/交互/i18n，数据源全部换接成本项目 Go 端点 |

关键事实（决定了方案形态）：

1. panel **不是外挂面板**，而是完整网关——"整合"实为两个平行 fork 间的特性移植，机制为**快照拷贝 + 适配**（panel 与本仓库无共同祖先，cherry-pick 不可行）。
2. manager 的 keys（多密钥体系）、security（IP 安全层）、用户管理/审计等能力在其 **Python 中间层**，前端只是展示壳；本 fork 不移植该层（见 §4 裁剪决定）。
3. manager 前端为纯静态导出（`output:'export'` + trailingSlash），对 go:embed 极友好；MIT 许可（含 linux-do/cdk 衍生声明，需保留）。

## 2. 不变的基座（本 fork 独有资产，全部保留）

- `/admin/*` REST 管理面（loopback-only，TrafficMonitor 插件配套）：tasks 快照/手动触发/热改排程、credits 缓存/实时/热改间隔、shutdown
- `/admin/accounts/{uid}/disable|enable|revive` 运维端点
- `/v1/stats`（按模型聚合）+ `/v1/stats/reset` + `cmd/stats` 终端视图
- `/status` 的 `in_flight_by_model`、`model_costs`、`rate_limited_models`、`cost_explore` 台账
- 成本分层条件探索（costTier 搭车改道）、WAF IP 级熔断（wafip）、三因子加权选号、会话粘性、提示词三模式、双域适配
- Windows 工作流：启动服务.bat 菜单、start/stop/status cmd、acct/stats 等 CLI、dev.sh
- 现有配置体系：`config.json` 增量演进、`WB2A_*` 环境变量覆盖、fail-fast 契约

## 3. 移植清单：panel 后端功能（快照拷贝 + 适配）

按 panel 当前 HEAD 快照拷贝源码文件，逐个适配到本 fork 的内部 API 签名：

1. **Web 面板核心**（账号池总览/积分条/冷却倒计时/成功失败计数/在途数；单号复活/禁用/签到/查余额/移除；批量签到/旅行巡检/活跃上报/保活/余额刷新）
2. **浏览器内 OAuth 加号**（面板扫码/授权登录 → 凭证落盘 auths/ + 热加载进池免重启，CN/国际双域）
3. **成长任务一键自动化**（17/18 任务纯 API 完成；执行队列：账号内串行、账号间并发 1-3；含桌面指纹通道 internal/upstream/desktop.go）
4. **开学季活动**（5 任务状态矩阵 + 一键闭环 + 券码 QR 查询）
5. **逐请求用量分桶统计**（按时间片 × realm × uid × model 分桶，落盘 data/usage.json，30s 防抖原子写；带 `usage.enabled` 开关，默认 true）
6. **配置热编辑**（在线改 config.json：深合并 + 原子替换 + 保留未知键；部分键立即热生效，装配期字段提示需重启）
7. **面板系统日志**（任务/对话/系统三频道，最近 500 行）
8. **后台自动化增强**（**折叠进现有六类定时任务，不新增 schedule 键**）：连登管家并入 checkin 尾部（连登 7/14/28 档自动兑换 + 抽奖自动抽完）；旅行领养 report 前置修正在 travel 内生效；夜猫子并入 cat
9. **模型实测上限标注**（读 data/output_probes.json 只读透传；探测脚本 probe_max_tokens.py 等一并拷入 scripts/）

## 4. 前端壳：manager UI 的取舍与改造

### 4.1 页面清单（10 → 8+1）

| 页面 | 处置 | 数据源 |
|---|---|---|
| login | 保留 | Go 实现 HMAC 签名会话 cookie；**密码 = api_key**，用户名随意填（单凭证，不引入用户体系） |
| dashboard | 保留 | 账号健康 ← /status；今日用量/扣费 ← panel 用量分桶 |
| accounts | 保留 | panel 池总览 + 单号/批量操作 + OAuth start/poll 扫码加号 |
| models | 保留 | panel 模型目录 + 实测上限标注 |
| playground | 保留 | 代理到本网关 /v1/chat/completions |
| tasks | 保留 | panel 任务自动化 + 执行队列（manager 原走 Python 子进程，换接） |
| stats | 保留 | panel 用量分桶 + 本网关 /v1/stats；**无 key 维度** |
| logs | 保留，改双 tab | 请求日志（本网关新增环形缓冲）+ 系统日志（panel 三频道） |
| settings | **裁剪** | 保留：配置热编辑、Upstash 连通性测试；新增：模型映射编辑、版本检查提示。**砍掉**：用户管理、审计日志、一键更新（降级为版本检查） |
| 活动（新建） | **新建页面** | panel 开学季 5 任务 + 券码 QR；按 manager 设计语言（shadcn 卡片、明暗主题）新建 |
| keys / security | **整页删除** | 路由/导航/i18n 键/类型全部移除；未来需要时再建（见 §6） |

### 4.2 改造要点

- **鉴权**：保 manager 登录页形态，HttpOnly Cookie 会话（无状态 HMAC 签名，重启不掉线）；`panel.enabled=true` 且 `api_key` 为空 → fail-fast 拒启
- **静态托管**：前端产物挂根路径；精确文件命中 → `{path}/index.html` 兜底（trailingSlash 语义）；**必须实测深链直访** /dashboard、/accounts（manager 已知 issue #48 同类问题）
- **i18n**：manager 五语言存量照抄；新增字符串（活动页、logs 双 tab、settings 裁剪区、模型映射、版本检查）只做**简中 + 英文**，其余语言回退英文
- **API 适配层**：Go 侧实现 manager 前端所需的最小 /api/* 端点集，内部调 panel 移植代码与本网关既有端点

### 4.3 新增后端小件（非移植，自研）

1. **模型映射**：`resolve_model.go` 查找链**链头**插用户自定义映射（服务 cc-switch 等写死模型名的客户端）；面板可编辑
2. **请求日志环形缓冲**：埋点在 chatStat.done() 处 append（模型/账号/状态/tokens/首字延迟/扣费/错误），容量 ~1000 条，重启即清（持久化由 usage 分桶承担，二者不重叠）
3. **版本检查提示**：只读端点，比对 GitHub 上游 release + changelog 链接，**不做自更新**
4. **panel 后端全套**（§3）

## 5. 不移植 / 裁剪清单（含理由）

| 项 | 理由 |
|---|---|
| manager keys 多密钥体系（密钥 CRUD/额度/IP 白名单/逐请求记账） | 用户明确定位：非公益站、单人自用，单 api_key 足够；这是本次最大 avoided 工程量 |
| manager security IP 安全层 | 公网部署暂不需要；本 fork 的 wafip 是"上游拦我"的熔断，语义不同但已有 |
| manager 用户管理 + 审计日志 | 与"登录=api_key 单凭证"决定冲突，多人共管才需要 |
| manager 一键自更新器 | 嵌入二进制自替换 exe 语义断裂（Docker/Windows 服务不可靠）；降级为版本检查提示 |
| panel 的 /panel/* 原生前端（index.html/app.js） | 被 manager 壳整体替代；**后端 /panel/api/* 端点照搬**，但对外路径改为 /api/* 适配层 |
| panel 首启自动生成随机 api_key | 与本 fork fail-fast 启动契约冲突 |

## 6. 已议而未行（留档，未来版本候选）

- **keys 多密钥体系**（点亮 dashboard 按密钥分账、stats 按密钥图表）：若未来做公益/共享站，重启此分支；届时 logs/stats/dashboard 加 key 维度
- **IP 安全层**：公网/反代部署时重启
- **manager 的新版页面跟进**（它日更活跃）：一次性移植不承诺持续吸收；边界按本文档切分，后续吸收按需重估

## 7. 构建与工程

- **三段构建**：Node（`npm ci && npm run build:export` 产出前端静态文件）→ Go（go:embed 内嵌）→ alpine 运行时；**不提交前端构建产物进仓库**
- **fresh-clone 无 Node 的行为**：embed 目录缺失时 `go build` 直接报错并提示先跑前端构建（fail-fast，与项目启动契约一致），不做降级嵌入空面板
- **Dockerfile** 改多阶段（node:xx → golang:xx → alpine:3.20）；ghcr CI（build.yml）同步
- **bat 菜单自动重编译加 Node 检测**：无 Node 时不重编译，提示缺 Node 并继续使用**上一次成功构建**的 exe（本机增量场景）；fresh clone 场景按上一条 fail-fast 报错
- **Go module 名保持 `workbuddy2api` 不变**（改 import 成本高收益零）；GitHub 仓库名改 `workbuddy-cockpit`，旧名自动重定向
- **master 直提、分阶段提交**：面板后端移植 → 前端壳+适配层 → 模型映射/请求日志/版本检查 → 配置/文档收尾；每步 `go build ./...` + `go vet ./...` + `go test ./...` 全绿；完成后打 tag **v1.2.0**
- **验收**：本机起服务浏览器实测——登录流程、深链直访、明暗主题、空账号池布局、playground 对话

## 8. 路由与部署不变式

- 前端挂根路径 `/`；适配层 `/api/*`；`/v1/*`、`/admin/*`、`/status`、`/v1/stats*`、`/healthz` **原样保留零冲突**
- `/panel/*` 路径不再保留（被 manager 壳替代）
- 面板暴露：默认 Bearer 等价（会话 cookie 由登录换发）+ 严格安全头（CSP 等），设计上可挂反代公网；`panel.loopback_only=true`（默认 false）可限回环
- 新配置段 `panel`：`panel.enabled`（默认 true）、`panel.loopback_only`（默认 false）；fail-fast：enabled 且 api_key 空拒启
- Docker 挂载在现有基础上增量：`data/`（用量分桶落盘）、`config/`（面板热改写回）已有约定沿用
- auths/ 凭证与 panel、manager 两仓同源兼容；state.json 以本 fork 格式为准

## 9. 文档与致谢义务

- README 重写（已完成，同 commit）：新身份 WorkBuddy Cockpit、面板功能为叙事中心、摘除上游作者的 Telegram 徽标与打赏钱包（fork 不得代收）、三方致谢
- `config.example.json` 增量新增 `panel` 段与 `usage` 键
- 使用指南.md 增补面板章节
- 致谢与许可：panel（MIT，后端移植）、workbuddy-manager（MIT，前端壳）、linux-do/cdk（manager 前端 UI token 衍生源，MIT）——均在 README「致谢」与 License 段保留署名（仓库不设独立 NOTICE 文件）
- 版本史：v1.2.0（2026-09）——Web 管理面板整合（panel 后端 + manager 前端壳），"不做 Web 面板"声明废止

## 10. 风险清单

1. **manager 前端改造面**：60+ /api/* 端点中仅取页面所需最小集；types.ts 是现成契约文档，逐页换接、逐页验收
2. **静态导出深链**：issue #48 同类问题，验收清单已含深链直访实测
3. **panel 代码与本 fork 内部 API 漂移**：panel 日均 7-8 commit 快速演化，快照拷贝日即锚定日，不做持续追踪
4. **tasks 页三模式语义**（preview/claim/full）：与 panel 的任务自动化对齐后可能只有近似映射，验收时逐模式实测
5. **前端构建链入 CI**：Node 版本钉死在 Dockerfile/CI，避免漂移
