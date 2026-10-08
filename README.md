<div align="center">

<img src="https://raw.githubusercontent.com/DGZSbot/ai-icon/refs/heads/main/WorkBuddy.png" alt="WorkBuddy Cockpit" width="120">

# WorkBuddy Cockpit

**把 WorkBuddy / CodeBuddy 账号变成 OpenAI 兼容 API，并且管好这一切**

多账号网关 · 内嵌 Web 管理面板 · 任务自动化 · 用量可观测 · 单容器部署

[![Release](https://img.shields.io/github/v/release/Arimayuki03/workbuddy-cockpit?style=flat-square&logo=github)](https://github.com/Arimayuki03/workbuddy-cockpit/releases/latest)
[![Release Date](https://img.shields.io/github/release-date/Arimayuki03/workbuddy-cockpit?style=flat-square)](https://github.com/Arimayuki03/workbuddy-cockpit/releases/latest)
[![Build & Publish](https://img.shields.io/github/actions/workflow/status/Arimayuki03/workbuddy-cockpit/build.yml?branch=master&style=flat-square&logo=githubactions&logoColor=black&label=CI%20build)](https://github.com/Arimayuki03/workbuddy-cockpit/actions/workflows/build.yml)
[![License](https://img.shields.io/github/license/Arimayuki03/workbuddy-cockpit?style=flat-square)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white&style=flat-square)](https://go.dev)
[![Next.js](https://img.shields.io/badge/Panel-Next.js%2015%20%2B%20shadcn%2Fui-000000?logo=nextdotjs&logoColor=white&style=flat-square)](web/)
[![GHCR Image](https://img.shields.io/badge/image-ghcr.io%2Farimayuki03%2Fworkbuddy--cockpit-2088FF?logo=github&style=flat-square)](https://github.com/Arimayuki03/workbuddy-cockpit/pkgs/container/workbuddy-cockpit)
[![Stars](https://img.shields.io/github/stars/Arimayuki03/workbuddy-cockpit?style=flat-square&color=6E56CF)](https://github.com/Arimayuki03/workbuddy-cockpit/stargazers)

**简体中文** · [功能总览](#-功能总览) · [快速开始](#-快速开始) · [配置](#️-配置) · [架构](#️-架构总览) · [版本](#-版本) · [文档](#-文档) · [免责声明](#️-免责声明)

</div>

---

## 📖 项目简介

WorkBuddy Cockpit 是一个自托管的一体化项目：**后端**是多账号 OpenAI 兼容网关（OAuth 登录、账号池轮转、熔断与冷却、会话粘性），**前端**是内嵌进同一个二进制的全功能 Web 管理面板（账号、任务、用量、配置、日志），部署一个容器、记住一个地址，全部搞定。

> 前身是 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) 的 fork（**上游仓库自 2026-09-23 起已被作者删除、不可访问**，链接仅为历史署名；本项目按 MIT 独立继续演进）。v1.2.0 起项目以独立形态演进：吸收 [workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel) 的后端管理能力与 [workbuddy-manager](https://github.com/ithtelab/workbuddy-manager) 的前端设计，网关核心与上游保持同源兼容。设计决策见 [docs/DESIGN-v1.2.0-panel-manager.md](docs/DESIGN-v1.2.0-panel-manager.md)。

### 与上游 / 同类项目的关系

| | 上游 workbuddy2api | workbuddy2api-panel | workbuddy-manager | **本项目** |
|---|---|---|---|---|
| 形态 | 纯网关，无面板 | 网关 + 原生 HTML 面板 | 外挂面板（Python 中间层 + Next.js） | **网关 + 内嵌 Next.js 面板** |
| 部署 | 单容器 | 单容器 | 网关 + 面板双容器 | **单容器** |
| 账号池治理 | ✓ | ✓ | —（调网关 API） | ✓（上游同源 + cost_explore） |
| 任务自动化 | ✓ 内置六类调度 | ✓ 全内置 | 依赖 Python 脚本 | ✓ 内置调度 + 成长任务/顺序任务链一键自动化 |
| 用量统计 | `/v1/stats` 聚合 | 逐请求分桶 | 自建 SQLite | **两者都有** |
| Windows 工作流 | ✓ cmd 启停脚本 | — | 脚本 | ✓ cmd 启停 + bat 交互菜单 + CLI 全套 |

## ✨ 功能总览

### 🔑 多密钥分发（wbk_）

面板「API 密钥」页可创建多把独立分发密钥（`wbk_` 前缀），替代单一全局 `api_key`（全局 key 仍兼容可用）。每把密钥独立可设：**realm 版本限定**（cn / global / 不限）、有效期、最大 IP 数、IP 白名单（CIDR）、模型白名单、Token 配额、**积分配额**（按上游真实扣费累计）与每分钟限流。明文仅创建时展示一次，库中只存 SHA-256 摘要；拒绝分类清晰可辨（参数 / 配置问题 400、凭据不可用 403、配额用尽 429 `insufficient_quota`），只有放行成功的请求才计入限流窗口与用量。创建弹窗内可一键导出 9 种客户端配置片段（Claude Code / Codex / cc-switch / ZCode / OpenAI SDK 等），密钥填错方向的问题一次消灭。

### 🔀 双协议直连（Anthropic Messages + OpenAI Responses）

- **Anthropic Messages API**（`/v1/messages` + `/v1/messages/count_tokens`）— Claude Code 等 Anthropic 协议客户端免转换层直连；`thinking` 的 enabled / adaptive 两态都支持；tool_result 内嵌图片正确提取为视觉输入（不再把 base64 塞进文本撑爆上下文）；count_tokens 按 CJK / 非 CJK 分类保守估算；鉴权与 chat completions 同口径（同一把 key）。
- **OpenAI Responses API**（`/v1/responses`）— Codex 等 Responses 客户端直连；instructions / input items 完整转换、reasoning item 宽容回填（防上游 11155「推理内容缺失」死循环）、`prompt_cache_key` 透传（缓存命中后费用差约 17 倍）、namespace 工具桥接为 function call。

### 🛡️ 入站 IP 安全

`security` 配置段（`trusted_proxy_cidrs` / `trusted_proxy_hops` / `ip_blacklist` / `ip_whitelist` / `ip_whitelist_mode`）：**默认零配置完全不信任转发头**——直连部署下伪造 `X-Real-IP` 无法绕过任何 IP 管控；反代 / CDN 后部署按网段声明可信代理，网关才从 `X-Forwarded-For` 还原真实来源。面板「安全」页提供 IP 规则热编辑、拦截日志（只记拦截，环形 512 条）与模型锁池卡片。

### 💰 credit_floor 积分保底

`pool.credit_floor`（0 = 关）：积分低于保底线的账号不接收费模型（按模型积分倍率折算成本判断），防止触底号继续烧费；全冷却兜底路径同判据，不给触底号留暗门；倍率表启动预热，模型目录更新即刷新。签到回血后账号自动恢复接单。

### 🎛️ Web 管理面板

浏览器打开即用，与网关同端口同鉴权，明暗主题，简中 / 繁中 / 英 / 日 / 韩五语言：

- **总览** — 账号健康分布、今日请求 / token / 积分扣费一目了然；到期积分按天归并口径切换（本地偏好）
- **账号** — 池总览（状态 / 积分进度条 / 冷却倒计时 / 成功失败计数 / 在途数）；单号禁用 / 恢复 / 复活 / 签到 / 查余额 / 移除；批量签到 / 保活 / 余额刷新；**扫码 / 授权登录添加账号**，凭证落盘后热加载进池，免重启；**凭据导出 / 导入**（跨部署迁移账号，同 UID 覆盖更新）；**强制清冷却**（冷却 / 熔断 / 连败降权 / 模型级限流一键归零，不碰禁用位）；**账号备注**（本地存储）与**搜索 + 分页**；积分变动流水（余额刷新对比快照，增加即记）
- **任务** — 成长任务一键自动化（25 个任务动作纯 API 完成：推进 → 轮询计分 → 自动领奖，执行队列账号内串行、账号间并发；含小程序 Sequential_Tasks_1..7 顺序任务链——专家对话、5/10 次对话、GLM5.2、灵感功能等，链式依赖每日解锁自动重试，**锁定环不扫入待办**，小程序对话上报按真人节奏执行——每条间隔 45s+ 随机抖动、可被队列取消中断）；历史券码二维码查询（开学季任务闭环已随活动 2026-09-24 结束下线）；六类定时任务快照与手动触发；**积分变动流水**（任务中心记录流 kind=credit）
- **统计** — 逐请求用量分桶（时间片 × 域 × 账号 × 模型）时间轴图表 + 按模型 / 账号聚合的请求量 / token / 延迟 / 扣费 / **积分（含积分每百万 token 单价，与积分同时观测的配对 token 才计入比例）**；时间窗筛选图和表同步生效；时间窗含近 24h / 72h / 7d / 30d 数字档与「启动以来」全量档
- **日志** — 请求日志（模型 / 账号 / 状态 / tokens / 首字延迟 / 扣费 / 错误 / **prompt 缓存命中三段观测**，环形缓冲最近约 1000 条）+ 系统日志（任务 / 对话 / 系统三频道）；请求日志可 **JSONL 归档**（`data/requests/` 按天轮转，重启不丢，`logging` 段配置保留天数与容量上限；client_ip / user_agent 脱敏开关默认不采集）
- **安全** — **IP 黑 / 白名单规则编辑**（CIDR，热生效）、**拦截日志**（只记拦截，环形 512 条：missing_key / invalid_key / ip_blocked / ip_not_whitelisted）、**模型锁池卡片**（locked / starved / partial 三态、最早 / 全部解锁时刻）
- **API 密钥** — **多密钥分发管理**（见上）：创建 / 停用 / 用量清零 / 已绑定 IP 查看，创建弹窗内一次性提供 9 种客户端配置片段
- **设置** — 配置热编辑（深合并原子写回，部分键立即生效，排程小时免重启热改，已拆子路由）、Upstash 连通性测试、**模型映射**（自定义模型名 → 真实模型，服务 cc-switch 等写死模型名的客户端）、**作用域 API Token 管理**（`wbt_` 前缀，只读 / 管理两档，供 CI / 脚本调面板 API）、版本检查提示；⌘K 命令面板全局可用，加载失败与空态区分、全局错误兜底页
- **Playground** — 面板内直接对话测试，走本网关 `/v1/chat/completions`

### 🐝 账号池治理

- **OAuth 设备授权登录** — `login.sh` / 面板内扫码加号，token 自动刷新、凭证落盘、热加载
- **三因子加权随机选号** — 积分比例 ×10 + 快过期积分占比 ×8 + 闲置补偿，Top-5 候选短名单内抽签，防惊群；`pool.pick_strategy` 可切换 `credits_desc` 余额从大到小严格选号（面板热生效；粘性会话的首次分配同样遵循策略，credits_desc 下新会话绑余额最高号）
- **在途租约** — 单号最大并发占用限制；`/status` 透出 `in_flight_by_model` 每模型在途台账
- **账本择优** — 按 `usage.credit` 折算每千 token 单价记入 (账号， 模型) 账本，免费 / 便宜的号优先，观测 EMA 平滑、6h 失效
- **成本分层条件探索** — tier 0 垄断时搭车改道探索未知号，承接真实请求零新增上游调用，成功即毕业
- **分级熔断与冷却** — 429 软冷却指数退避、402 硬冷却至次日 04:00（不被软冷却 / 限流覆盖翻型，零余额号不会提前回池）、连败熔断、模型级限流独立冷却（切模型豁免）、WAF IP 级 fail-fast；运维可从面板**强制清冷却**。签到 / 余额刷新自动解冻只清账号级冷却，不重置软限流退避指数与模型级台账（余额恢复不证明 chat 通道健康，避免零余额号反复回池撞 429）
- **模型锁池可见性** — `/status` 透出当前模型级冷却聚合（locked 全锁 / starved 全饿 / partial 部分锁定三态、最早 / 全部解锁时刻）；全池模型级阻塞时 503 返回 `model_blocked` 而非误导性的「无可用账号」，客户端退避策略不再混同
- **会话粘性** — conversation 四键 → prompt_cache_key → 首条 user 消息派生，多轮上下文不跳号；粘性按模型判活
- **双域适配** — 国内版（`copilot.tencent.com`）与国际版（`www.workbuddy.ai`）共享一池，按 realm 或模型名前缀路由

### ⏰ 定时积分任务

六类任务独立排程、独立开关（`schedule.*_enabled`），**触发小时可在面板 / API 热改（免重启）**：签到（09/21 点，尾部自动跑连登管家：7/14/28 档自动兑换 + 抽奖自动抽完）、活跃地图（10 点）、猫猫旅行（09/21 点，领养状态机修正）、token 保活（22 点）、夜猫子（01 点）、任务执行队列（10 点，默认关）。**Token 续期巡检**（`schedule.renew_hours` / `renew_enabled`，默认 [3] 点、缺省关）：按剩余寿命挑号——有效期不足 `pool.expiring_soon` 窗口（默认 7 天）的临期账号主动刷新，覆盖长期闲置无对话流量的号；面板手动停用的账号跳过（摘流量不摘凭证），仅冷却 / 熔断等计时位异常的照常续期。**睡眠补跑**：进程睡眠跨过整点槽位时（如系统休眠 / 宿主机挂起），唤醒后按时间升序自动补跑窗口内（24h）已到点的槽位，同刻多类合并、批内并行；开学季 `school_*` 配置键仅为老配置兼容保留（活动已结束，触发即跳过）。面板与 `/admin/tasks` 均可手动触发；单任务 panic 有 recover 兜底，不会击穿网关进程。

### 🔌 OpenAI 兼容接口

- `/v1/chat/completions` 流式 + 非流式（出站强制 stream，SSE 白名单重建，非流式本地聚合）、**`/v1/messages` + `/v1/messages/count_tokens`（Anthropic Messages 协议，Claude Code 直连）**、**`/v1/responses`（OpenAI Responses 协议，Codex 直连）**、`/v1/models`（effort 档位 / 积分倍率 / 上下文全字段）、`/v1/stats`、`/status`、`/healthz`——三族端点同一套鉴权
- 出站改写管线：强制 `stream:true`、`developer` 角色归一、tool_choice 归一、`image_url` 字符串兼容、DeepSeek 思维链注入、`reasoning_effort` 档位降级、`reasoning_content` 回填、指纹脱敏、**prompt 缓存命中别名归一**（部分上游 `details.cached_tokens` 有真值而扁平别名留 0 时取全部别名最大正值回写，下游不再误判未命中）
- **出站修复一批**（吸收两上游实案）：GPT 系 `max_tokens<16` 钳制（防 11133 触发全号轮转）；工具 schema pattern `\_` 转义归一（防 11129 整体拒收）；背靠背 tool_calls 消息合并（防 11148 tool_call_sequence_broken 顶死会话）；11101 bad_params 立即 400 直通不再轮转（确定性失败不伪装成「账号不可用」）；上游超时止损（不换号、不罚号、不喂连败计数）；流式成功判定延后到真成功（上游「200 开流 + 一帧 error」不再把限流号记成健康号、粘性不再被钉死在它身上）
- 系统提示词三模式（passthrough / custom / append）+ 内容拦截降级重试
- 模型映射、错误分类轮转退避；国际版模型目录**多 UA 并发探测取并集**（桌面 / IDE / CLI 三路 UA——桌面 UA 补齐、IDE UA 字段权威、CLI UA 补 deepseek 系缺失 id），试用横幅模型自动补入目录；上下文窗口按「远端权威 → 静态知识表 → model.json 缓存 → models.dev 按需拉取」四级回退

### 🖥️ Windows 原生工作流

**启动服务.bat** 交互菜单（积分 / 签到 / 任务 / 启停 / 日志 / 加号 / 统计 / 账号运维），`login.exe` / `credit.exe` / `acct.exe` / `stats.exe` / `task.exe` 等 CLI 全套；源码模式 `go run ./cmd/server` 即跑。详见 [使用指南.md](使用指南.md)。

## 🚀 快速开始

### 方式一：Docker Compose（推荐）

```bash
git clone https://github.com/Arimayuki03/workbuddy-cockpit.git
cd workbuddy-cockpit
mkdir -p config && cp config.example.json config/config.json
# 编辑 config/config.json：至少设置 api_key；开启 panel 或 admin 时 api_key 必填（缺失 fail-fast）
docker compose up -d --build

# 打开面板
# http://localhost:7863/   →  登录密码 = 你设置的 api_key
```

> 也可以用 CI 预构建镜像 `ghcr.io/arimayuki03/workbuddy-cockpit:latest`（多架构 amd64/arm64；把 compose 里的 `build: .` 换成 `image:` 即可）。ghcr 镜像由 [Build & Publish](.github/workflows/build.yml) 在每次打 `v*` tag 时发布；发行版二进制（Windows zip / Linux / macOS，含任务工具 exe 与 sha256 校验）随 Release 资产自动上传。若拉取不到镜像，Workflow 页面另有 amd64 离线 tar.gz 供下载。

添加账号：面板「账号」页扫码 / 授权登录，或 `./login.sh`。

### 方式二：源码构建

```bash
# 前端面板（需要 Node 22+，产物 embed 进二进制）
cd web && npm ci && npm run build:export && cd ..
cp -r web/out internal/panel/dist   # Windows: robocopy /MIR web\out internal\panel\dist

# 后端（-tags embed_panel 启用面板内嵌；无该 tag 则为纯网关构建）
go build -trimpath -tags embed_panel -ldflags="-s -w" -o wb2api ./cmd/server
./wb2api -config config.json
# 开发态直跑：go run ./cmd/server -config config.json
```

> ⚠️ `go build` 开启 `-tags embed_panel` 而 `internal/panel/dist/` 缺失时**编译直接报错**（fail-fast：不嵌入残缺前端），请先完成前端构建与拷贝。

### 方式三：Windows 一键脚本

双击 **启动服务.bat** 交互菜单即可（自动编译所需工具、自动检测端口与僵死进程）。发行版 exe 双击即可用：配置文件不存在时首启引导会自动生成带随机 `api_key` 的最小 `config.json` 并打印密钥（双击报错窗口保持可见）。详见 [使用指南.md](使用指南.md)。

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

### 接入客户端

v1.16.0 起 Claude Code 与 Codex 可**免转换层直连**（Anthropic 协议走 `/v1/messages`，Responses 协议走 `/v1/responses`，配置示例见[使用指南](使用指南.md)）；面板「API 密钥」页创建密钥时可直接导出对应客户端的配置片段。

以 **cc-switch**（Claude Code 等 Anthropic 协议客户端的本地转换器）为例：

- **Base URL**：`http://127.0.0.1:7863/v1`，**API Key**：`config.json` 的 `api_key`（或多密钥分发的 `wbk_` 密钥）
- **模型映射建议**：Default / Sonnet / Opus → `deepseek-v4-pro`；Haiku / Subagent → `deepseek-v4-flash`
- 客户端写死的模型名（如 `gpt-4o`）可在面板 `/settings` 模型映射里整名映射到网关模型

## ⚙️ 配置

配置以 [`config.example.json`](config.example.json) 为完整参考，`WB2A_*` 环境变量可覆盖（28 个，含 `WB2A_CONFIG` 指定配置路径）。常用段：

```jsonc
{
  "listen": ":7863",            // 监听地址
  "api_key": "your-gateway-key", // 网关鉴权 key（panel/admin 开启时必填，缺失拒绝启动）
  "auth_dir": "./auths",        // 账号凭证目录（5s 轮询热加载）
  "upstash": { "url": "", "token": "" }, // 状态镜像到 Upstash Redis（可选）
  "usage":   { "enabled": true },  // 逐请求用量分桶落盘 data/usage.json
  "panel":   { "enabled": true, "loopback_only": false }, // Web 管理面板
  "schedule": { "checkin_enabled": true, "checkin_hours": [9, 21], "queue_enabled": false } // 六类任务排程（面板可热改）
}
```

| 新增键 | 默认 | 说明 |
|---|---|---|
| `panel.enabled` | `true` | 启用 Web 面板（关掉即回到纯网关形态） |
| `panel.loopback_only` | `false` | 面板仅限本机回环访问（公网反代场景保持 false） |
| `usage.enabled` | `true` | 逐请求用量分桶统计（关掉则统计页只剩 `/v1/stats` 聚合） |
| `schedule.queue_hours` / `queue_enabled` | `[10]` / `false` | 任务中心执行队列排程（默认关；面板任务页可手动触发） |
| `schedule.renew_hours` / `renew_enabled` | `[3]` / `false` | Token 独立续期巡检：对有效期不足 `pool.expiring_soon` 窗口的临期账号主动刷新令牌（面板手动停用号跳过） |
| `pool.pick_strategy` | `weighted` | 选号策略：`weighted` 三因子加权随机 / `credits_desc` 余额从大到小严格选号（面板设置页热改，免重启） |
| `pool.credit_floor` | `0` | 积分保底线：低于该值的账号不接收费模型（按模型倍率折算成本），防触底号烧费；0 = 关 |

`security` 段（入站 IP 安全，均可在面板「安全」页热改）与 `logging` 段（请求日志归档，**归档默认关**）：

```jsonc
{
  "security": {
    "trusted_proxy_cidrs": [],   // 可信代理网段（CIDR）。默认空 = 完全不信任转发头，
                                 // 直连部署最安全；反代/CDN 后部署才需要配
    "trusted_proxy_hops": 1,     // 可信代理层数（X-Forwarded-For 从右往左取第几跳，
                                 // <1 按 1 处理）；前面还挂 CDN 时调成 CDN + 反代的层数
    "ip_blacklist": [],          // 黑名单 CIDR，命中即拒（优先于白名单）
    "ip_whitelist": [],          // 白名单 CIDR
    "ip_whitelist_mode": false   // true 且白名单非空 = 仅放行白名单（默认拒绝）
  },
  "logging": {
    "request_archive": false,       // JSONL 归档开关（默认关，开档才写 data/requests/）
    "request_retention_days": 7,    // 归档按天保留上限
    "request_archive_max_mb": 200,  // 归档总容量上限（超限删最旧）
    "request_client_info": false    // 采集 client_ip / user_agent（脱敏开关，默认不采集）
  }
}
```

> ⚠️ 反代部署务必配置 `trusted_proxy_cidrs`（如 nginx 同机的 `127.0.0.1/32`、Docker 网桥 `172.16.0.0/12`），否则网关看到的永远是反代地址，IP 白名单与密钥 IP 管控无法按真实来源生效；反之直连部署保持默认空，任何人伪造 `X-Real-IP: 9.9.9.9` 都不会绕过 IP 管控。

安全契约：`panel.enabled` 或 `admin.enabled` 为 true 且 `api_key` 为空时**拒绝启动**。面板与 API 同端口同鉴权，自带严格 CSP 等安全头；登录失败限速（按 IP 10 分钟内 5 次失败锁定 15 分钟，防 api_key 在线穷举）；公网部署务必设置 api_key 并置于 HTTPS 反代之后。

> ⚠️ 合规须知：本项目是**非官方**网关，使用 WorkBuddy / CodeBuddy 账号作为上游，**仅限本人授权账号、本机 / 私有环境测试**。详细边界见[免责声明](#️-免责声明)。

## 🏗️ 架构总览

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
    U -->|"billing / auth / growth"| CB
```

上游请求在出站前经历统一的改写管线（`internal/upstream/payload.go`）：强制 `stream:true`、`developer` 角色归一、tool_choice 归一、`image_url` 字符串兼容为 OpenAI 对象形态、DeepSeek 思维链注入、`reasoning_effort` 档位降级、`reasoning_content` 回填、指纹脱敏。

### 目录结构

```
workbuddy-cockpit/
├── cmd/                  # CLI 工具源码（server 网关 / login / credit / acct / stats / task …）
├── internal/             # 核心库（auth / pool / scheduler / server / upstream / panel / usage …）
├── web/                  # Next.js 15 面板前端（静态导出后 go:embed 内嵌）
├── scripts/              # 辅助脚本（Python 任务体 / PowerShell 服务启停）
├── docs/                 # 设计文档
├── config.example.json   # 配置模板（复制为 config.json 后填 api_key）
├── docker-compose.yml    # Docker 一键部署
├── Dockerfile            # 三段构建：node:22 → golang:1.26 → alpine:3.20
└── LICENSE               # MIT
```

## 📚 文档

| 文档 | 内容 |
|---|---|
| [使用指南.md](使用指南.md) | 启停 / 配置 / 账号管理 / 接口验证 / 多密钥分发 / 双协议接入 / 入站 IP 安全 / credit_floor / 作用域 Token / 面板用法（含凭据导入导出）/ 常见问题排查 |
| [docs/DESIGN-v1.2.0-panel-manager.md](docs/DESIGN-v1.2.0-panel-manager.md) | v1.2.0 面板整合设计定稿 |
| [config.example.json](config.example.json) | 全量配置键参考（含注释级说明） |

## 🔄 版本

| 版本 | 日期 | 说明 |
|---|---|---|
| v1.0.0 | 2026-09-14 | 上游基线（Sliverkiss/workbuddy2api），ghcr 镜像发布流程 |
| v1.1.0 | 2026-09-20 | 吸收上游 2026-09-20 合并：`/v1/stats`、账号停用/恢复/复活端点、auths 热加载、global 域路由 |
| v1.1.1 | 2026-09-20 | `/status` 透出 `in_flight_by_model` 每模型在途台账 |
| v1.2.0 | 2026-09-22 | **项目更名 WorkBuddy Cockpit**；整合 Web 管理面板（panel 后端 + manager 前端壳）、模型映射、请求日志、版本检查；吸收上游两段式治理流水线、`image_url` 兼容等。网关侧 `/v1/*`、`/admin/*`、`/status` 接口契约保持完全兼容 |
| v1.2.1 | 2026-09-23 | 用量页时间窗修复：新增「启动以来」全量档、切窗竞态修复、Y 轴刻度遮挡修复 |
| v1.3.0 | 2026-09-23 | **账号凭据导出 / 导入**——跨部署迁移账号（同 UID 覆盖更新，导出格式与落盘同形） |
| v1.12.0 | 2026-09-24 | 账号表列排序（本地偏好持久化）、日志页昵称列与昵称搜索、仪表盘积分排序扩容、学院抽奖 `draw_uuid` 修复（标准 uuid v4 + 回归测试） |
| v1.13.0 | 2026-09-26 | **选号策略手动切换**——`pool.pick_strategy` 新增 `credits_desc`（按积分余额从大到小严格选号，面板设置页下拉，热生效免重启），默认 `weighted` 行为不变 |
| v1.14.0 | 2026-09-26 | **吸收两上游修复 + 发行流程固化**——任务待办过滤 locked 环（顺序任务链不再空跑）、签到解冻不再重置软限流退避与模型级冷却（旧语义下零余额号被误判健康回池再撞 429）、usage 缓存命中别名归一（部分上游 `cached_tokens` 真值不再被误判未命中）；CI 打 `v*` tag 自动构建四平台二进制 + 任务工具 exe + sha256 上传 release 资产，镜像 / 二进制统一注入版本号 |
| v1.15.0 | 2026-09-29 | **调度器跨槽补跑 + mp 真人节奏 + usage 积分维度 + 下架开学季**——睡眠跨槽自动补跑（系统休眠 / 宿主挂起后唤醒补齐窗口内错过的整点任务）、小程序顺序任务链对话上报改真人节奏（每条 45s+ 抖动、可被队列取消中断，规避上游反作弊回滚）、用量分桶新增积分维度（积分 / 积分每百万 token 单价，落盘格式 v2 向后兼容）、开学季活动下线（任务与面板入口移除，`school_*` 配置键兼容保留、历史券码查询保留）；面板设置页口径同步（六类任务） |
| v1.15.1 | 2026-09-29 | **镜像构建修复**——多架构镜像的前端 / Go 构建段钉 `BUILDPLATFORM` 原生构建 + Go 按 `TARGETOS/TARGETARCH` 交叉编译，不再走 QEMU 模拟（v1.15.0 的 ghcr 镜像构建在模拟 arm64 内 `next/font` 拉 Google Fonts ETIMEDOUT 失败，release 二进制不受影响）；网关代码与 v1.15.0 完全一致 |
| v1.16.0 | 2026-10-08 | **吸收两上游能力大版本**——**多密钥分发**（`wbk_` 前缀，realm / 有效期 / IP 白名单 / 模型白名单 / Token 与积分配额 / 限流逐把可设，明文仅创建时可见、库中只存 SHA-256，创建弹窗一次性导出 9 种客户端配置片段）；**双协议直连**（Anthropic Messages `/v1/messages` + count_tokens，Claude Code 直连；OpenAI Responses `/v1/responses`，Codex 直连，reasoning item 宽容回填防 11155 死循环、`prompt_cache_key` 透传省费约 17 倍）；**入站 IP 安全**（`security` 段：默认零配置完全不信任转发头防伪造 `X-Real-IP`，反代按网段声明可信代理；面板「安全」页规则编辑 + 拦截日志 + 模型锁池卡片）；**credit_floor 积分保底**（触底号不接收费模型，防烧费）；模型锁池可见性（`/status` 三态聚合 + 503 `model_blocked`）；请求日志 JSONL 归档（按天轮转重启不丢，client_ip/UA 脱敏默认不采集）；**作用域 Token**（`wbt_` 前缀只读 / 管理两档，写操作仅会话 cookie 可用）；账号治理增量（积分变动流水 / Token 续期巡检 `schedule.renew_*` / 账号备注与搜索分页 / 昵称同步）；出站修复一批（GPT 系 max_tokens 钳制防 11133、工具 schema `\_` 归一防 11129、背靠背 tool_calls 合并防 11148、11101 立即 400 直通、上游超时止损不罚号、流式成功判定延后到真成功）；面板 UI（⌘K 命令面板 / 全局错误兜底页 / 加载失败与空态区分 / settings 拆子路由）；运维（docker-compose PUID/PGID 参数化、单文件 bind mount EBUSY 写回回退、签到/余额瞬时错误有界重试、模型目录桌面 UA 第三路探测、tok/s 200ms 护栏、密钥导出片段） |

完整变更见 [Releases](https://github.com/Arimayuki03/workbuddy-cockpit/releases)。

## 🙏 致谢

- [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) — 网关核心的上游基座（本 fork 的起点；上游仓库已删库下线，署名依 MIT 保留）
- [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel) — 面板后端能力（账号池视图 / 任务自动化 / 开学季 / 用量分桶 / OAuth 加号 / 配置热编辑）的移植源（MIT）
- [ithtelab/workbuddy-manager](https://github.com/ithtelab/workbuddy-manager) — 前端面板设计与页面结构的来源（MIT）；其 UI 设计 token 与组件层改编自 [linux-do/cdk](https://github.com/linux-do/cdk)（MIT），一并致谢

## ⚠️ 免责声明

本项目（包括但不限于代码、脚本、文档、配置示例及仓库内任何资源，下称「本项目内容」）**仅供个人学习与研究使用**。使用本项目表示您已阅读并接受本声明全部条款；如不同意，请立即停止使用并删除全部相关内容。

**1. 用途限制。** 本项目内容仅可用于个人学习、研究等非商业用途；请勿将本项目用于任何商业目的或牟利行为，请勿违反所属国家 / 地区 / 组织的任何法律法规。本项目不构成对任何软件、服务、平台的使用建议或授权。

**2. 账号与数据责任。** 本项目可能涉及个人账号凭证的获取、存储与使用。您应仅使用本人持有且已获授权的账号，自行确认相关平台的服务条款与允许范围，并自行承担使用、存储凭证（如 `auths/` 中的文件）及调用上游服务所产生的全部责任与风险。本项目不参与、不介入您与任何平台之间的契约关系。

**3. 内容与第三方界限。** 本项目内容中引用的第三方产品、服务、LOGO、图片、文案等，其权利均归各自权利人所有；本项目不保证此类内容的准确性、完整性、合法性，亦不代表支持或推荐任何第三方。如实存在侵权情形，请通过 Issues 告知，经核实后本项目会尽快处理。

**4. 无担保与风险自担。** 本项目内容按「现状」提供，不附带任何明示或默示的担保（包括但不限于适销性、特定用途适用性、准确性、不侵权等）。使用本项目（包括直接或间接）所产生的任何风险与后果（包括但不限于账号异常、数据丢失、服务中断、纠纷或损失），均由使用者自行承担，与本项目及其全部贡献者无关。

**5. 责任限定。** 在任何情况下，本项目及其作者、贡献者均不对任何直接、间接、偶然、特殊或后果性损害承担责任，无论该等损害是否基于合同、侵权或其他法律理论，即使已被告知发生该等损害的可能性。

**6. 修改与分发。** 基于本项目源代码进行的任何修改、衍生均系第三方自发行为，与本项目无关，相应后果由该第三方自行承担。本项目内所有资源文件，禁止任何公众号、自媒体进行任何形式的转载、发布。未经授权，任何组织或个人不得将本项目内容用于转载、发布或再分发。

**7. 条款变更。** 本项目保留随时修改、补充本声明的权利。修改后的声明自发布之日起生效，继续使用本项目即视为接受修订后的声明。本项目所有内容仅供学习和研究使用，请于学习研究完成后及时删除。

## 📄 License

本项目采用 [MIT License](LICENSE) 开源协议。

- 在遵守 MIT License 前提下，允许使用、复制、修改、合并本项目源代码
- 再分发（源码或二进制形式）时，须保留本项目及上游 Sliverkiss/workbuddy2api、workbuddy2api-panel、workbuddy-manager 的 MIT 版权声明与许可声明
- 本项目不授予任何上游（WorkBuddy / CodeBuddy）接口或服务的权利；使用者仍需自行遵守上游服务条款
- 本项目的使用同时受上方**免责声明**约束；如免责声明与 MIT License 存在不一致，以免责声明为准

---

<div align="center">

如果这个项目对你有帮助，欢迎点一个 ⭐

</div>
