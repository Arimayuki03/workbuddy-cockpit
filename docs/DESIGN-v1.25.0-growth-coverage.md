# 成长中心自动化 — 覆盖度分析与移植设计（v1.25.0）

> 状态：**已实施（代码级，2026-11-13，v1.25.0）**。端点配方与顺序不变量已落地，
> 协议层测试全绿；真实账号的线上复测（尤其盲盒/专家族/活动任务）仍需部署后确认。
> 本文档回答一个问题：外部两个高星成长中心脚本的能力，能否/如何吸收进 workbuddy2api。
> 结论：**能力已覆盖 90%，缺口是 4 个能力点；不做整体移植，只做端点配方补齐。**

---

## 0. 一句话

`workbuddy2api` 已经把两个参考实现 23 个云端任务中的 18 个、7 个链式任务、8 个互动玩法中的 6 个实现了；
真正的缺口只有 **盲盒、expert_use_\* 家族、skill_share、Campaign 活动任务**四项，另加 4 项健壮性硬化。
预计 **4–6 人日**达到 23/23 + 5/6 活动任务。

---

## 1. 背景：两个参考实现

| 仓库 | 星数 | 定位 | 与本项目的关系 |
|---|---|---|---|
| [88lin/workbuddy-auto-signin](https://github.com/88lin/workbuddy-auto-signin) v1.8.3 | ~2.4k | 本机凭证读取 + 签到 + 成长中心，单文件 `signin.py` 2773 行 | **仅作端点交叉验证源**。成长逻辑集中在 `run_growth()`（479 行） |
| [L0NE-6/WorkBuddy-Daily](https://github.com/L0NE-6/WorkBuddy-Daily) v50 / 脚本 v3.49 | ~600 | RT 永续 + 45 项任务全自动，单文件 `workbuddy_daily.py` 7092 行 | **主要增量来源**。任务配方最全、状态机最细 |

两者的共同点：MIT 许可、纯逆向端点、无请求签名、靠服务端状态幂等。

---

## 2. 现状盘点

### 2.1 已具备的基础设施（本 fork 独有，参考实现均无）

| 能力 | 位置 | 说明 |
|---|---|---|
| HTTP 客户端 | `internal/upstream/client.go` | 三 base 路由（chat / billing / web）、超时分层 |
| 出站代理 | `internal/upstream/proxy_route.go` | 按账号命名路由，`__direct__` 显式直连，未命中**硬失败不回落** |
| 错误分类 | `internal/upstream/client.go` `Classify` | 含 `ChannelCheck`（渠道指纹）、`ErrKind` 枚举 |
| 多账号池 | `internal/pool/` | 加权选择、熔断、冷却、余额、Redis 快照 |
| Token 续期 | `client.go` `RefreshToken` + `scheduler/renew.go` | 锁外 IO + CAS 写回 + 到期窗口 |
| 调度 | `internal/scheduler/scheduler.go` | hour-slot + 补跑窗口 + 手动 `RunKindNow` |
| 执行队列 | `internal/panel/taskcenter.go` | 账号内串行、账号间并发 1–4 |
| 观测 | `internal/panel/` + `web/` | 队列状态、积分流水、任务中心 UI |

**结论：参考实现的 HTTP/账号/调度层严格弱于本 fork（`verify=False`、禁代理、手写文件锁），不应移植。**

### 2.2 覆盖度矩阵 — 云端成长中心任务（L0NE 口径 23 项）

| # | task_code | 本 fork | 落点 |
|---:|---|---|---|
| 1 | `create_canvas` | ✅ | `autotask.go` `runCreateCanvas` |
| 2 | `playbook_prompt` | ✅ | `runPlaybookPrompt` |
| 3 | `RichMeow_Chat` | ✅ | `runRichMeow` |
| 4 | `skill_1` | ✅ | `runSkillFresh` |
| 5 | `Library_read` | ✅ | `runLibraryRead` |
| 6 | `Expert_lighthouse` | ✅ | `runExpertLighthouse` |
| 7 | `Hp_Appearance` | ✅ | `runAppearance` |
| 8 | `Buddy_App` | ✅ | `runBuddyApp` |
| 9 | `Buddy_App_QQ` | ✅ | `runBuddyApp` |
| 10 | `Model_chat_GLM5.2` | ✅ | `runModelChat` |
| 11 | `chat_5` | ✅ | `runChat5` |
| 12 | `black_cat` | ✅ | `upstream/blackcat.go` |
| 13 | `Expert_team_use_3` | ✅ | `runExpertTeamUse` |
| 14 | `expert_5` | ✅ | `runExpertUse` |
| 15 | `template_5` | ✅ | `runTemplateUse` |
| 16 | `automation_1` | ✅ | `runAutomationCreate` |
| 17 | `first_buddy` | ✅ | `runFirstBuddy` |
| 18 | `school_season` | ✅（活动已结束） | `runSchoolSeason` |
| 19 | `WorkBuddy_Valorant` | ✅（v1.25.0） | `panel.runExpertUseDynamic` |
| 20 | `expert_use_maidehao` | ✅（v1.25.0） | `panel.runExpertUseDynamic` |
| 21 | `expert_use_douyinyin` | ✅（v1.25.0） | `panel.runExpertUseDynamic` |
| 22 | `expert_use_shangjia_xuanchuan` | ✅（v1.25.0） | `panel.runExpertUseDynamic` |
| 23 | `expert_use_bowangwang` | ✅（v1.25.0） | `panel.runExpertUseDynamic` |
| — | `skill_share` | ✅（v1.25.0） | `panel.runSkillShare` |

**v1.25.0 实施后 23/23 已覆盖（专家族 4 个走同一通用解析器）。**

### 2.3 覆盖度矩阵 — 链式 / 活动 / 互动

| 分组 | 项 | 本 fork |
|---|---|---|
| 小程序链式 | `Sequential_Tasks_1` … `_7` | ✅ 全有（`scripts/task_runner.py`） |
| Campaign 活动 | 星巴克 3 项 + 记者节 3 项 | ✅ 5/6（app 跳过，v1.25.0） |
| 互动 | 抽奖 / 连签兑换 / 补签 / 旅行 / 礼包补偿 / Buddy 信息 | ✅ |
| 互动 | **盲盒**（`buddy/quota` + `buddy/open`） | ✅（v1.25.0） |
| 互动 | 徽章 `GET /badges` | ❌ 只读，低价值，暂不做 |

---

## 3. 缺口清单

### G1　盲盒（P0）

**端点**

```
GET  {chatBase}/v2/activity/growth/buddy/quota
     → data.affordable, data.max_open_count(默认 1)

POST {chatBase}/v2/activity/growth/buddy/open
     {"count": N, "client_token": "<uuid>"}
     → data.buddy | data.name | data.buddies
```

**约束（两个实现独立收敛，可信度高）**

1. 仅当 `travel state == idle` 允许开盒；`traveling` / 未知状态一律跳过。
2. `count = min(affordable, max_open_count)`；L0NE 额外夹 `min(..., 5)`。
3. 顺序：**claim 旅行礼物 → 开盒 → depart**。开盒会覆盖 travel record，颠倒会丢礼物。
4. 服务端不惩罚开满（88lin 保守只开 1 次，L0NE 开满，两者均未报错）。

**落点**

- `internal/upstream/` 新增 `BuddyQuota` / `BuddyOpen`（约 80 行）。
- `internal/scheduler/` 的 `claimGrowthRewards`（`scheduler.go:1082-1138`）插入开盒步骤，位于旅行 claim 之后、depart 之前。
- `internal/panel/autotask.go` 增 action（可选，纯 API 无需 UI 交互）。

**估时 0.5–1 人日。**

---

### G2　`expert_use_*` 动态家族（P0）

**核心结论：不要硬编码 4 个 code，实现一个通用解析器即可覆盖 5 个任务。**
L0NE 的做法是运行时发现：

```
1. 读任务列表 → 对每个未完成任务取 jump_url
   regex: ^workbuddy://experts\?expertId=(ex_[A-Za-z0-9]{6,64})$
2. POST https://www.codebuddy.cn/v2/operation-platform/market/expert/list
   {"page":N,"page_size":20,"keyword":<profession>}
   分页搜索，精确匹配 expert_id（无 ID 时用唯一同名兜底）
3. 执行 LOCAL 专家链：
   真实 chat（_meta.mode="LOCAL"）
   → 6 事件链（has_expert / expert_id / expert_name / expert_industry_id:""）
   → expert_actual_use(mode:"LOCAL", expertType:"agent", cost:0)
```

**本 fork 已有可复用件**：`runExpertLighthouse`（`Expert_lighthouse`）与 `runExpertUse`（`expert_5`）已实现 LOCAL 专家链的事件组，
G2 的实质是**把专家身份从常量改为参数**，外加 `jump_url` 解析与市场查表。

**待补的固定专家元数据（L0NE 实测值）**

| task_code | expert_id | 名称 | 版本 | slug |
|---|---|---|---|---|
| `WorkBuddy_Valorant` | `ex_xQHHD1u5Jkua` | 无畏契约拼豆专家 | 1.2.1 | — |
| `expert_use_maidehao` | `ex_GDl31WahyIH0` | 卖得好（中国电商运营专家） | 1.0.1 | `maidehao` |
| `expert_use_douyinyin` | `ex_FbY5nlWTqDod` | 斗音音（抖音策略师） | 1.0.2 | `douyinyin` |
| `expert_use_shangjia_xuanchuan` | `ex_Weo3x2EZuSDm` | 商家宣传设计师 | 1.6.0 | `shangjia-xuanchuan` |
| `expert_use_bowangwang` | `ex_WZY136wV5aMt` | 播旺旺（直播电商教练） | 1.0.2 | `bowangwang` |

> 元数据会随市场上架变化，**必须保留 jump_url 动态解析作为主路径**，常量表只作兜底。

**落点**：`internal/panel/autotask.go` 新增 `runExpertUseDynamic` + `internal/upstream/` 专家市场查询方法。
**估时 1–2 人日，收益 5 个 task code。**

---

### G3　`skill_share`（P2）

```
1. POST {chatBase}/v2/operation-platform/market/skill/list  {"page":1,"page_size":20}
2. POST https://open.workbuddy.cn/api/v2/open-platform/asset-shares/c2c/channels
   {"asset_kind":"skill","asset_source":"market","asset_ref":"{\"asset_id\":\"<id>\"}",
    "ways":["link","wxacode","wxapath"],
    "extra":{"name":...,"description":...,"shareKey":<id>,"marketplaceSource":"builtin-market"}}
3. 上报 2 个 skill_share_channel_action 事件：
   panelAction:"click"（panelType:"clipboard"）→ panelAction:"success"
4. 回读 prog，仅 completed/claimed 或 current>=target 时 claim
```

注意：这是**唯一需要访问 `open.workbuddy.cn` 的端点**，需要在路由表里放行该 host。
**估时 1 人日，收益 1 个任务。**

---

### G4　Campaign 活动任务（P1）

**协议与现有 `internal/upstream/school.go` 同源**（`/portal/activity/*`），换 `activity_code` 即可，应做成 spec 表驱动。

```
GET  {codebuddy}/portal/activity/tasks?activity_code=<camp>
     → data.tasks[]: {task_code, target_count, need_view, reward_credit}
     → data.progress[]: {progress, completed_at, claimed_at}

POST {codebuddy}/portal/activity/tasks/{task}/viewed?activity_code=<camp>   # need_view 时
POST {codebuddy}/portal/activity/tasks/{task}/claim?activity_code=<camp>    # 需 5×4s 回读 claimed_at
```

| 活动 code | 任务 | 判据 |
|---|---|---|
| `campaign_starbucks_2026` | `ai_chat`（3 次） / `expert` / `app` | mp 事件链 + 页面上下文 |
| `campaign_reporter_2026` | `expert_chat` / `skill_use` / `desktop_same` | 4 链 / skill_info / 单个 desktop 事件 |

**降级决策**：`app` 任务需 `POST {copilot}/v2/as/conversations/` 换取 `boxUrl` + token，再走 ACP bootstrap + SSE 并校验
`stopReason=="end_turn"`，成本高、失败模式多。**首版跳过 `app`，只做 5 项（250 分）。**

**估时 1–2 人日（不含 app）。**

---

### G5　健壮性硬化（P0，低成本）

| # | 项 | 现状 | 动作 |
|---|---|---|---|
| G5.1 | `credit_delayed=true` | 需核对 `DailyCheckin` | 必须判"待入账"，不得判失败 |
| G5.2 | 未确认领奖去重 | 有回读，无去重集 | 同一轮内回读未确认则**不得二次 POST**（L0NE `_workbuddy_unconfirmed_claims`） |
| G5.3 | 任务有效期 | `Task` struct 无该字段 | 补 `valid_start` / `valid_end`，用于"未到上线时间"跳过与到期提醒 |
| G5.4 | 执行链 401 恢复 | handler 有预刷新，任务链无 | 首个请求 401 → 单次刷新 → 重放一次 |

**估时 0.5 人日。**

---

### G6　徽章（P3，可选）

`GET {chatBase}/v2/activity/growth/badges` 只读，仅用于面板展示。**暂不做。**

---

## 4. 执行顺序不变量（本次分析的最高价值产出）

两个参考实现**独立**收敛到同一顺序，说明这是服务端状态机约束而非作者偏好。
**违反任一条会静默丢奖励（HTTP 200 但无 credit）。**

```
travel status（闸门：401/403 中止本轮，未知状态封锁全部 Buddy 写）
  ↓
claim 旅行礼物          ← 必须在任何 Buddy 变更之前（开盒覆盖 travel record）
  ↓
tasks accept
  ↓
tasks claim             ← 先于抽奖（任务产出 energy / chances）
  ↓
streak → heatmap → makeup   ← 先于 redeem（补签改变 tier 解锁状态）
  ↓
redeem → lottery draw
  ↓
buddy open（盲盒）       ← 必须在 depart 之前
  ↓
travel depart          ← 最后一个写操作
```

**当前风险**：本 fork 的 `travel` 是独立 scheduler task，`claimGrowthRewards` 是另一条路径，
两者顺序**不保证**。若出现"旅行礼物领不到"，首先排查 `depart` 是否抢在了 `claim` 之前。

**补充不变量**：`traveling` 或 `arrived 但 claim 失败` ⇒ **既不开盒也不 depart**。
推荐采用 L0NE 的写法：claim 后回读必须回到 `idle` 才继续。

---

## 5. 明确不移植清单

| 项 | 出处 | 不移植理由 |
|---|---|---|
| `sym-v1` 凭证解密 | 88lin | 依赖本机 `WorkBuddy.exe` 以 `ELECTRON_RUN_AS_NODE` 启动 + 私有 binding `electron_browser_workbuddy_storage.loggerGet()` + 精确 AES-256-GCM AAD 帧 + `sha256(base64文本)` 派生密钥。客户端升级即全量失效，且与本 fork 的 OAuth 设备码流程（`cmd/login`）冲突。**若要"从本机导入账号"，必须隔离为独立可选模块，不进主链路** |
| 桌面换血 | L0NE | `taskkill WorkBuddy.exe` + 改写 `workbuddy-desktop-ai.info` + 重启挂 9222 端口 + ACP/CDP。Windows-only、会打断用户正在使用的客户端，且本 fork 已有纯 API 指纹回退（`RichMeow_Chat` / `skill_1` 已点亮） |
| 45s MP 节奏 | L0NE | ⚠️ **待 A/B 验证，见 R2** |
| 青龙 / 通知渠道 | L0NE | 本 fork 有面板 |
| 微信关注任务 | L0NE | 不可自动化，参考实现也只检测 |
| `verify=False` / `trust_env=False` | 两者 | 本 fork 传输层更严格，禁止反向降级 |
| 手写多账号注册表 | 88lin | 本 fork `internal/pool` 更强 |

---

## 6. 风险登记

| ID | 风险 | 等级 | 应对 |
|---|---|---|---|
| R1 | 违反 §4 执行顺序导致静默丢奖 | **高** | 开盒/旅行/领奖统一收敛到单一顺序化流程 |
| R2 | MP 45s 节奏 | **中（待验证）** | L0NE 称发送过快会"进度回滚 + claim 400"；但本 fork `task_runner.py` 用 `--gap 1.0` 且注释记录"已 14/14 claimed（credit +1450、energy +65）"。45s×17 事件 = 13–16 分钟/账号，会摧毁队列吞吐。**先 8–15s A/B，确认被拦再上调**，禁止直接抄 45s |
| R3 | 渠道指纹拦截 `11-128` | 中 | 已有 `upstream.Error.ChannelCheck`，保持"不重试、停止该账号" |
| R4 | 企业号 403（growth system / personal user） | 低 | 已跳过，保持 |
| R5 | `credit_delayed=true` 误判失败 | 中 | 见 G5.1 |
| R6 | 未确认领奖重复 POST | 中 | 见 G5.2 |
| R7 | claim 路径不对称 | 低 | `accept` 带 `/v2`，`claim` 不带；已正确实现（`tasks.go` 头注释） |
| R8 | 新端点 404 误判"下架" | 低 | growth 域多 host 均可（copilot / www.workbuddy.cn 两套 headers 都通），先用另一 host 复测 |
| R9 | 元数据漂移（专家 ID / 活动 code） | 中 | 动态解析为主、常量表兜底 |
| R10 | 许可证 | 低 | 见 §7 |

---

## 7. 许可证与合规

- 两个参考实现均为 **MIT**。移植端点配方属衍生作品，发布时需保留原版权声明。
- 建议新增 `THIRD_PARTY_NOTICES.md`，收录：
  - `Copyright (c) 2026 L0NE-6 — WorkBuddy-Daily (MIT)`
  - `88lin/workbuddy-auto-signin (MIT)`
- L0NE 的 `NOTICE.md` 明确声明不改变 MIT 授予的权利，仅约束**品牌冒用与付费二次包装**。因此：
  - ✅ 可以移植代码并商用/内部使用
  - ❌ 不得使用其项目名、作者身份、收款码做背书
- 两个参考实现均在 README 声明"仅供学习与个人自动化，风险自负"；本 fork 沿用同等免责口径。

---

## 8. 实施计划

| 阶段 | 内容 | 估时 | 依赖 | 状态 |
|---|---|---:|---|---|
| 1 | **G1 盲盒** + §4 顺序修正 | 0.5–1 d | 无 | ✅ 已实施 |
| 2 | **G2 expert_use 家族** | 1–2 d | 无 | ✅ 已实施 |
| 3 | **G5 硬化 4 项** | 0.5 d | 无 | ✅ 已实施 |
| 4 | **G4 Campaign（5 项，跳过 app）** | 1–2 d | 复用 `school.go` 结构 | ✅ 已实施 |
| 5 | **G3 skill_share** | 1 d | 需放行 `open.workbuddy.cn` | ✅ 已实施 |
| 6 | G6 徽章（可选） | 0.2 d | — | ⏸ 暂不做 |
| | **合计** | **4–6 d** | | |

**阶段 1 验收即产生收益**：盲盒是当前唯一完全缺失的日常玩法。

### 8.1 实施落点（v1.25.0）

| 能力 | 主要文件 |
|---|---|
| G1 盲盒 | `internal/upstream/buddy_box.go`、`internal/scheduler/travel.go` |
| G2 专家族 | `internal/upstream/market.go`、`internal/panel/autotask.go` |
| G3 skill_share | `internal/upstream/skill_share.go`、`internal/panel/autotask.go` |
| G4 Campaign | `internal/upstream/campaign.go`、`internal/panel/campaign.go`、`internal/panel/taskcenter.go` |
| G5 硬化 | `internal/upstream/tasks.go`、`internal/upstream/client.go`、`internal/panel/autotask.go` |
| §4 顺序 | `scheduler.travelOne`（claim → idle 回读 → open → depart）；盲盒不再挂在 `claimGrowthRewards` 旁路 |
| 许可证 | `THIRD_PARTY_NOTICES.md` |

---

## 9. 验收清单

- [x] G1：`buddy/quota` 返回 `affordable>0` 时能开出 Buddy；`traveling` 状态下不调用 `open`
      （代码级：`scheduler/travel_blindbox_test.go` 覆盖六种状态；线上待复测）
- [x] G1：旅行礼物 claim 与 depart 的先后顺序在日志中可验证
      （代码级：`claim ok` → `blindbox open ok` → `depart ok`；测试断言调用序列）
- [x] G2：`tasks[].jump_url` 能解析出 `ex_*` 并命中市场；4 个 `expert_use_*` 至少各点亮一次
      （代码级：解析/市场/动作已实现；「各点亮一次」需线上账号复测）
- [x] G2：市场查不到时降级到常量表，不报错
      （代码级：`panel.resolveExpertForTask` 单测）
- [x] G3：`skill_share` 走完 建频道 → 2 事件 → 回读 → claim
      （代码级：建频道/2 事件已测；回读/claim 走通用任务领奖路径，线上待复测）
- [x] G4：campaign `tasks` → `viewed` → `claim` 闭环，`claimed_at` 回读确认
      （代码级：`panel.growth_v125_test.go` 全链；真实活动需复测）
- [x] G5.1：`credit_delayed=true` 不判失败
      （`scheduler.checkin_delayed_test.go`）
- [x] G5.2：同一轮内未确认的 claim 不重复 POST
      （`panel.growth_v125_test.go` 计数断言）
- [x] G5.3：`locked` 且有 `valid_start` 的任务不打请求
      （`upstream.Task.NotYetValid` + 三个执行入口同口径）
- [x] G5.4：执行链 401 后刷新并重放一次
      （`panel.growth_v125_test.go` 401 → refresh → replay）
- [ ] R2：完成 8s / 15s / 45s 三档 A/B，记录 claim 成功率，结论写回本文档
      **（未做；本次未改动既有 45s 节奏）**

---

## 10. 决策记录

| ID | 决策 | 依据 |
|---|---|---|
| D1 | **不整体移植**，只移植端点配方 | 参考实现的 HTTP/账号/调度层全面弱于本 fork；其真正价值是逆向结果 |
| D2 | **不移植凭证解密** | 依赖私有 binding，脆弱；与本 fork OAuth 流程冲突 |
| D3 | **不移植桌面换血** | 侵入性强、Windows-only，已有纯 API 回退 |
| D4 | **MP 节奏不采用 45s** | 与本 fork 实测 1.0s 可用相矛盾，需 A/B；直接采用会摧毁吞吐 |
| D5 | **Campaign `app` 任务首版跳过** | ACP bootstrap + SSE + 结构化 stopReason，成本/收益不匹配 |
| D6 | **专家任务走动态解析** | 硬编码 4 个 code 会在任务换榜时静默失效 |

---

## 附录 A　端点速查

> 三个 base 由 `internal/upstream/client.go` 提供：
> `chatBase` = `copilot.tencent.com`，`billingBase` = `codebuddy.cn`，`webBase` = `workbuddy.cn`。
> growth 域在 chatBase / webBase 均可用（两套 headers 都通）。

| 用途 | 方法 | 路径 | Body |
|---|---|---|---|
| 任务列表 | GET | `/v2/activity/growth/tasks` | — |
| 任务列表（MP） | GET | 同上 + `X-Client-Platform: miniprogram` | — |
| 任务列表（等级） | GET | `/activity/growth/tasks`（无 `/v2`） | — |
| 接受任务 | POST | `/v2/activity/growth/tasks/accept` | `{"task_codes":[...]}` |
| 领取奖励 | POST | `/activity/growth/tasks/{code}/claim`（**无 `/v2`**） | `{}` |
| 连签状态 | GET | `/activity/growth/streak` | — |
| 活跃日历 | GET | `/activity/growth/heatmap` | — |
| 补签 | POST | `/activity/growth/makeup-cards/use` | `{"target_date":"YYYY-MM-DD"}` |
| 兑换 | POST | `/activity/growth/redeem` | `{"tier":"7d\|14d\|28d","client_token":"..."}` |
| 抽奖次数 | GET | `/activity/growth/lottery/chances` | — |
| 抽奖 | POST | `/activity/growth/lottery/draw` | `{"client_token":"..."}` |
| **盲盒额度** | GET | `/v2/activity/growth/buddy/quota` | — |
| **开盲盒** | POST | `/v2/activity/growth/buddy/open` | `{"count":N,"client_token":"..."}` |
| Buddy 可见性 | GET | `/v2/activity/growth/buddy/visible` | — |
| Buddy 信息 | GET | `/v2/activity/growth/buddy/info` | — |
| 领第一只 Buddy | POST | `/v2/activity/growth/buddy/first` | `{}` |
| Buddy 协议 | POST | `/v2/activity/growth/buddy/agreement` | `{"agree":true}` |
| 旅行状态 | GET | `/activity/growth/buddy/travel/status` | — |
| 旅行地点 | GET | `/activity/growth/buddy/travel/config` | — |
| 出发 | POST | `/activity/growth/buddy/travel/depart` | `{"location_id":N}` |
| 领旅行礼物 | POST | `/activity/growth/buddy/travel/claim` | `{"record_id":N}` |
| 徽章 | GET | `/v2/activity/growth/badges` | — |
| **专家市场** | POST | `{codebuddy}/v2/operation-platform/market/expert/list` | `{"page":N,"page_size":20,"keyword":...}` |
| **技能市场** | POST | `/v2/operation-platform/market/skill/list` | `{"page":1,"page_size":20}` |
| **分享频道** | POST | `open.workbuddy.cn/api/v2/open-platform/asset-shares/c2c/channels` | 见 G3 |
| **活动任务** | GET | `{codebuddy}/portal/activity/tasks?activity_code=` | — |
| **活动浏览** | POST | `{codebuddy}/portal/activity/tasks/{task}/viewed?activity_code=` | `{}` |
| **活动领奖** | POST | `{codebuddy}/portal/activity/tasks/{task}/claim?activity_code=` | `{}` |
| 签到状态 | POST | `{billingBase}/v2/billing/meter/checkin-activity-status` | `{}` |
| 签到 | POST | `{billingBase}/v2/billing/meter/daily-checkin` | `{}` |
| 礼包 | POST | `{billingBase}/billing/meter/claim-gift` | `{}` |
| 补偿 | POST | `{billingBase}/billing/meter/claim-compensation` | `{}` |

## 附录 B　服务端状态语义速查

| 字段 | 取值 | 含义 |
|---|---|---|
| `accept_status` | `not_accepted` / `accepted` / `completed` / `claimed` | 只有 completed 可领奖 |
| `locked` | bool | 未解锁；配合 `valid_start` 判上线时间 |
| `already_claimed` | bool | 幂等成功，非错误 |
| `travel state` | `idle` / `traveling` / `arrived` | 只有 idle 可开盒/出发 |
| `daily_limit_reached` | bool | 今日出发名额已用完 |
| `credit_delayed` | bool | 积分待入账，**不是失败** |
| 业务码 | `10001` + 已签到 | 幂等成功 |
| 业务码 | `11-128` / unapproved channel | 渠道指纹拦截，停止该账号，不重试 |
| 业务码 | `403` + growth system / personal user | 企业号无权限，跳过成长域 |
| 业务码 | `41000` | 活动未开始/已结束，非致命 |
| 业务码 | `40901` | 任务暂不可领 |
| 业务码 | `12153` / `token format error` | 本地凭证问题，不重试 |
