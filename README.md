<p align="center">
  <img src="https://raw.githubusercontent.com/DGZSbot/ai-icon/refs/heads/main/WorkBuddy.png" alt="WorkBuddy Cockpit" width="120">
</p>

<h1 align="center">WorkBuddy Cockpit</h1>

<p align="center">
  <b>把 WorkBuddy / CodeBuddy 账号变成 OpenAI 兼容 API，并且管好这一切</b><br>
  多账号网关 · 内嵌 Web 管理面板 · 任务自动化 · 用量可观测
</p>

<p align="center">
  <img alt="Version" src="https://img.shields.io/badge/Version-v1.2.0.dev-6E56CF?style=flat-square">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white&style=flat-square">
  <img alt="Panel" src="https://img.shields.io/badge/Panel-Next.js%20%2B%20shadcn%2Fui-000000?logo=nextdotjs&logoColor=white&style=flat-square">
  <img alt="API" src="https://img.shields.io/badge/API-OpenAI_Compatible-412991?style=flat-square">
  <img alt="Deploy" src="https://img.shields.io/badge/Deploy-Docker_Compose-2496ED?logo=docker&logoColor=white&style=flat-square">
  <img alt="Transport" src="https://img.shields.io/badge/Transport-SSE%20%2F%20Streaming-0DBD8B?style=flat-square">
</p>

---

## 这是什么

WorkBuddy Cockpit 是一个自托管的一体化项目：**后端**是多账号 OpenAI 兼容网关（OAuth 登录、账号池轮转、熔断与冷却、会话粘性），**前端**是内嵌进同一个二进制的全功能 Web 管理面板（账号、任务、用量、配置、日志），部署一个容器、记住一个地址，全部搞定。

> 前身是 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) 的 fork（Arimayuki03/workbuddy2api，v1.1.1）。v1.2.0 起项目以独立形态演进：吸收 [workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel) 的后端管理能力与 [workbuddy-manager](https://github.com/ithtelab/workbuddy-manager) 的前端设计，网关核心与上游保持同源兼容。设计决策见 [docs/DESIGN-v1.2.0-panel-manager.md](docs/DESIGN-v1.2.0-panel-manager.md)。

### 与上游 / 同类项目的关系

| | 上游 workbuddy2api | workbuddy2api-panel | workbuddy-manager | **本项目** |
|---|---|---|---|---|
| 形态 | 纯网关，无面板 | 网关 + 原生 HTML 面板 | 外挂面板（Python 中间层 + Next.js） | **网关 + 内嵌 Next.js 面板** |
| 部署 | 单容器 | 单容器 | 网关 + 面板双容器 | **单容器** |
| 账号池治理 | ✓ | ✓ | —（调网关 API） | ✓（上游同源 + cost_explore） |
| 任务自动化 | ✓ 内置六类调度 | ✓ 全内置 | 依赖 Python 脚本 | ✓ 内置调度 + 成长任务/开学季一键自动化 |
| 用量统计 | /v1/stats 聚合 | 逐请求分桶 | 自建 SQLite | **两者都有** |
| Windows 工作流 | ✓ cmd 启停脚本 | — | 脚本 | ✓ cmd 启停 + bat 交互菜单 + CLI 全套 |

## 核心能力

### 🎛️ Web 管理面板（v1.2.0 新增）

> 🚧 **本节为 v1.2.0 设计目标**（设计定稿见文末版本节），随实施逐步落地；当前代码仅含网关能力。下述条目完成情况以 Release 说明为准。

浏览器打开即用，与网关同端口同鉴权，明暗主题、简中/英文等多语言：

- **总览** — 账号健康分布、今日请求 / token / 积分扣费一目了然
- **账号** — 池总览（状态 / 积分进度条 / 冷却倒计时 / 成功失败计数 / 在途数）；单号禁用 / 恢复 / 复活 / 签到 / 查余额 / 移除；批量签到 / 保活 / 余额刷新；**扫码 / 授权登录添加账号**，凭证落盘后热加载进池，免重启
- **任务** — 成长任务一键自动化（17/18 个任务纯 API 完成：推进 → 轮询计分 → 自动领奖，执行队列账号内串行、账号间并发）；开学季活动 5 任务一键闭环 + **券码二维码**；六类定时任务快照与手动触发
- **统计** — 逐请求用量分桶（时间片 × 域 × 账号 × 模型）时间轴图表 + 按模型聚合的请求量 / token / 延迟 / 扣费
- **日志** — 请求日志（模型 / 账号 / 状态 / tokens / 首字延迟 / 扣费 / 错误，最近 1000 条）+ 系统日志（任务 / 对话 / 系统三频道）
- **设置** — 配置热编辑（深合并原子写回，部分键立即生效）、Upstash 连通性测试、**模型映射**（自定义模型名 → 真实模型，服务 cc-switch 等写死模型名的客户端）、版本检查提示
- **Playground** — 面板内直接对话测试，走本网关 `/v1/chat/completions`

### 🐝 账号池治理

- **OAuth 设备授权登录** — `login.sh` / 面板内加号，token 自动刷新、凭证落盘、热加载
- **三因子加权随机选号** — 积分比例 ×10 + 快过期积分占比 ×8 + 闲置补偿，Top-5 候选短名单内抽签，防惊群
- **在途租约** — 单号最大并发占用限制；`/status` 透出 `in_flight_by_model` 每模型在途台账
- **账本择优** — 按 `usage.credit` 折算每千 token 单价记入 (账号， 模型) 账本，免费 / 便宜的号优先，观测 EMA 平滑、6h 失效
- **成本分层条件探索** — tier 0 垄断时搭车改道探索未知号，承接真实请求零新增上游调用，成功即毕业（issue #136 的学习冻结破解）
- **分级熔断与冷却** — 429 软冷却指数退避、402 硬冷却至次日 04:00、连败熔断、6004 模型级限流独立冷却（切模型豁免）、WAF IP 级 fail-fast
- **会话粘性** — conversation 四键 → prompt_cache_key → 首条 user 消息派生，多轮上下文不跳号；粘性按模型判活
- **双域适配** — 国内版（copilot.tencent.com）与国际版（www.workbuddy.ai）共享一池，按 realm 或模型名前缀路由

### ⏰ 定时积分任务

六类任务独立排程、独立开关（`schedule.*_enabled`）：签到（09/21 点，尾部自动跑连登管家：7/14/28 档自动兑换 + 抽奖自动抽完）、活跃地图（10 点）、猫猫旅行（09/21 点，领养状态机修正）、token 保活（22 点）、开学季（12 点）、夜猫子（01 点）。面板与 `/admin/tasks` 均可手动触发。

### 🔌 OpenAI 兼容接口

- `/v1/chat/completions` 流式 + 非流式（出站强制 stream，SSE 白名单重建，非流式本地聚合）、`/v1/models`（effort 档位 / 积分倍率 / 上下文全字段）、`/v1/stats`、`/status`、`/healthz`
- DeepSeek 思维链注入与 `reasoning_content` 回填、`reasoning_effort` 档位降级
- 系统提示词三模式（passthrough / custom / append）+ 内容拦截降级重试
- 指纹脱敏、模型映射、错误九分类轮转退避

### 🖥️ Windows 原生工作流

**启动服务.bat** 交互菜单（积分 / 签到 / 任务 / 启停 / 日志 / 加号 / 统计 / 账号运维），`login.exe` / `credit.exe` / `acct.exe` / `stats.exe` / `task.exe` 等 CLI 全套；源码模式 `go run ./cmd/server` 即跑。详见 [使用指南.md](使用指南.md)。

## 快速开始

### Docker Compose

```bash
git clone https://github.com/Arimayuki03/workbuddy-cockpit.git
cd workbuddy-cockpit
mkdir -p config && cp config.example.json config/config.json
# 编辑 config/config.json：至少设置 api_key；开启 panel 或 admin 时 api_key 必填（缺失 fail-fast）
docker compose up -d --build

# 打开面板
# http://localhost:7863/   →  登录密码 = 你设置的 api_key
```

> 注：`workbuddy-cockpit` 为 v1.2.0 起的新仓库名；改名生效前请用旧地址 `https://github.com/Arimayuki03/workbuddy2api.git`（改名后旧名由 GitHub 自动重定向）。

添加账号：面板「账号」页扫码 / 授权登录，或 `./login.sh`。

### 源码构建

```bash
# 前端面板（需要 Node 20+，产物 embed 进二进制）
cd web && npm ci && npm run build:export && cd ..

# 后端
go build ./...
go run ./cmd/server -config config.json
# 或单文件：CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o wb2api ./cmd/server
```

### 验证

```bash
# 网关身份与健康
curl -s http://localhost:7863/healthz

# 模型列表 / 账号状态 / 请求统计
curl -s http://localhost:7863/v1/models  -H "Authorization: Bearer your-api-key"
curl -s http://localhost:7863/status     -H "Authorization: Bearer your-api-key"
curl -s http://localhost:7863/v1/stats   -H "Authorization: Bearer your-api-key"

# 流式聊天
curl -sN http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

## 配置

配置以 `config.example.json` 为完整参考，`WB2A_*` 环境变量可覆盖（22 个）。v1.2.0 新增段：

| 键 | 默认 | 说明 |
|---|---|---|
| `panel.enabled` | `true` | 启用 Web 面板（关掉即回到纯网关形态） |
| `panel.loopback_only` | `false` | 面板仅限本机回环访问（公网反代场景保持 false） |
| `usage.enabled` | `true` | 逐请求用量分桶统计（关掉则统计页只剩 /v1/stats 聚合） |

安全契约：`panel.enabled` 或 `admin.enabled` 为 true 且 `api_key` 为空时**拒绝启动**。面板与 API 同端口同鉴权，自带严格 CSP 等安全头；公网部署务必设置 api_key 并置于 HTTPS 反代之后。

> ⚠️ 合规须知：本项目是**非官方**网关，使用 WorkBuddy / CodeBuddy 账号作为上游，**仅限本人授权账号、本机 / 私有环境测试**。详细边界见[安全与合规](#安全与合规)。

## 架构总览

```mermaid
flowchart LR
    B["浏览器 / SDK"] --> ROOT["/: Web 面板\nNext.js 静态导出 (go:embed)"]
    B --> API["/api/*: 面板适配层"]
    C["客户端 / SDK\nOpenAI 兼容请求"] --> H

    subgraph GWI["WorkBuddy Cockpit :7863"]
        ROOT --> EMBED["静态文件服务"]
        API --> PG["面板后端\n账号/任务/用量/配置/日志"]
        H["HTTP Handler\n鉴权 · 模型映射 · 提示词改写 · 轮转"] --> P
        H --> S
        P["账号池\n三因子加权 · 熔断 · 冷却 · 租约"] --> U
        S["会话粘性路由"] -.绑定镜像.-> REDIS
        T["定时调度\n六类任务 + 连登管家"] --> P
        U["上游 Client\nChatHTTP 流式 · 短 RPC"]
        PG --> P
        PG -.用量分桶.-> DATA[("data/usage.json")]
    end

    P -. "读凭证 (0600)" .-> AUTH[("auths/*.json")]
    P -. "状态镜像" .-> REDIS[("Upstash Redis\n可选")]
    U -->|"chat/completions (SSE)"| CB["WorkBuddy / CodeBuddy\ncopilot.tencent.com · workbuddy.ai"]
```

## 安全与合规

- **CI 自动打包**：GitHub Actions 多架构（amd64/arm64）构建发布至 `ghcr.io`，同时输出 amd64 离线 tar.gz；也可本地 `docker compose build`
- 上游 CodeBuddy 属第三方商业产品，本项目是其**非官方 OpenAI 兼容网关**；使用其账号做 API 网关涉及目标平台服务条款与账号风险，作者不对账号封禁、条款违约或使用结果负责
- 仅限**本人授权账号**、本机 / 私有环境测试；不得共享、转售、违规分发
- 妥善保管 `auths/`（明文凭证）、`config/`（含 api_key）与网关端口；面板公网暴露务必 HTTPS 反代 + 强 api_key

## 版本

当前版本：**v1.2.0（开发中）**。设计定稿见 [docs/DESIGN-v1.2.0-panel-manager.md](docs/DESIGN-v1.2.0-panel-manager.md)。

- **v1.0.0**（2026-09-14）— 上游基线（Sliverkiss/workbuddy2api），ghcr 镜像发布流程
- **v1.1.0**（2026-09-20）— 吸收上游 2026-09-20 合并：`/v1/stats`、账号停用/恢复/复活端点、auths 热加载、global 域 /v2 路由
- **v1.1.1**（2026-09-20）— `/status` 透出 `in_flight_by_model` 每模型在途台账
- **v1.2.0**（开发中）— 项目更名 **WorkBuddy Cockpit**；整合 Web 管理面板（后端能力移植自 workbuddy2api-panel，前端设计采用 workbuddy-manager，均 MIT）；新增模型映射、请求日志环形缓冲、版本检查；"纯网关不做面板"定位废止，网关侧对既有客户端的接口契约（`/v1/*`、`/admin/*`、`/status`）保持完全兼容

## 致谢

- [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) — 网关核心的上游基座（本 fork 的起点）
- [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel) — 面板后端能力（账号池视图 / 任务自动化 / 开学季 / 用量分桶 / OAuth 加号 / 配置热编辑）的移植源（MIT）
- [ithtelab/workbuddy-manager](https://github.com/ithtelab/workbuddy-manager) — 前端面板设计与页面结构的来源（MIT）；其 UI 设计 token 与组件层改编自 [linux-do/cdk](https://github.com/linux-do/cdk)（MIT），一并致谢

## 免责声明

本项目（包括但不限于代码、脚本、文档、配置示例及仓库内任何资源，下称「本项目内容」）**仅供个人学习与研究使用**。使用本项目表示您已阅读并接受本声明全部条款；如不同意，请立即停止使用并删除全部相关内容。

**1. 用途限制。** 本项目内容仅可用于个人学习、研究等非商业用途；请勿将本项目用于任何商业目的或牟利行为，请勿违反所属国家 / 地区 / 组织的任何法律法规。本项目不构成对任何软件、服务、平台的使用建议或授权。

**2. 账号与数据责任。** 本项目可能涉及个人账号凭证的获取、存储与使用。您应仅使用本人持有且已获授权的账号，自行确认相关平台的服务条款与允许范围，并自行承担使用、存储凭证（如 `auths/` 中的文件）及调用上游服务所产生的全部责任与风险。本项目不参与、不介入您与任何平台之间的契约关系。

**3. 内容与第三方界限。** 本项目内容中引用的第三方产品、服务、LOGO、图片、文案等，其权利均归各自权利人所有；本项目不保证此类内容的准确性、完整性、合法性，亦不代表支持或推荐任何第三方。如实存在侵权情形，请通过 Issues 告知，经核实后本项目会尽快处理。

**4. 无担保与风险自担。** 本项目内容按「现状」提供，不附带任何明示或默示的担保（包括但不限于适销性、特定用途适用性、准确性、不侵权等）。使用本项目（包括直接或间接）所产生的任何风险与后果（包括但不限于账号异常、数据丢失、服务中断、纠纷或损失），均由使用者自行承担，与本项目及其全部贡献者无关。

**5. 责任限定。** 在任何情况下，本项目及其作者、贡献者均不对任何直接、间接、偶然、特殊或后果性损害承担责任，无论该等损害是否基于合同、侵权或其他法律理论，即使已被告知发生该等损害的可能性。

**6. 修改与分发。** 基于本项目源代码进行的任何修改、衍生均系第三方自发行为，与本项目无关，相应后果由该第三方自行承担。本项目内所有资源文件，禁止任何公众号、自媒体进行任何形式的转载、发布。未经授权，任何组织或个人不得将本项目内容用于转载、发布或再分发。

**7. 条款变更。** 本项目保留随时修改、补充本声明的权利。修改后的声明自发布之日起生效，继续使用本项目即视为接受修订后的声明。本项目所有内容仅供学习和研究使用，请于学习研究完成后及时删除。

## License

本项目采用 [MIT License](LICENSE) 开源协议。

- 在遵守 MIT License 前提下，允许使用、复制、修改、合并本项目源代码
- 再分发（源码或二进制形式）时，须保留本项目及上游 Sliverkiss/workbuddy2api、workbuddy2api-panel、workbuddy-manager 的 MIT 版权声明与许可声明
- 本项目不授予任何上游（WorkBuddy / CodeBuddy）接口或服务的权利；使用者仍需自行遵守上游服务条款
- 本项目的使用同时受上方**免责声明**约束；如免责声明与 MIT License 存在不一致，以免责声明为准
