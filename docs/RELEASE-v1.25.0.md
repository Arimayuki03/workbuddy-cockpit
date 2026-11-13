# WorkBuddy Cockpit v1.25.0

发布日期：2026-11-13。

吸收 [88lin/workbuddy-auto-signin](https://github.com/88lin/workbuddy-auto-signin)
v1.8.3 与 [L0NE-6/WorkBuddy-Daily](https://github.com/L0NE-6/WorkBuddy-Daily)
v50 的**成长中心 / 活动任务端点配方**，把设计中剩余的 4 个能力点与 4 项硬化
落到网关、调度器与面板任务中心。两个参考实现均为 MIT；未复制其 HTTP/账号/
调度层，只取逆向结果，声明见根目录 [`THIRD_PARTY_NOTICES.md`](../THIRD_PARTY_NOTICES.md)。

设计与覆盖度分析见
[`docs/DESIGN-v1.25.0-growth-coverage.md`](DESIGN-v1.25.0-growth-coverage.md)。

## 1. 盲盒（G1）

- 新增 `GET /v2/activity/growth/buddy/quota`、`POST /v2/activity/growth/buddy/open`。
- 开盒数量 `count = min(affordable, max_open_count, 5)`；`max_open_count`
  缺省按 1（不把缺省当不限）。
- 旅行巡检改为单一顺序化状态机，严格遵循服务端约束：

  ```
  travel status（闸门）
    → claim 旅行礼物
    → 回读必须回到 idle
    → open 盲盒
    → depart
  ```

  `traveling`、未知状态、`arrived` 但 claim 未确认（HTTP 200 但回执无
  `reward_credit`）、claim 后状态仍非 idle 时，**一律不做后续 Buddy 写操作**。
  该顺序在日志中可逐行验证（`claim ok` → `blindbox open ok` → `depart ok`）。

## 2. `expert_use_*` 动态家族（G2）

- 新增 5 个任务动作：`WorkBuddy_Valorant`、`expert_use_maidehao`、
  `expert_use_douyinyin`、`expert_use_shangjia_xuanchuan`、
  `expert_use_bowangwang`，接入既有「一键完成 / 执行队列」。
- 专家身份解析顺序：任务 `jump_url` 的
  `workbuddy://experts?expertId=ex_*`（严格解析，外链必须是 `deeplink=`
  且 host 属于 workbuddy.cn / codebuddy.cn / tencent.com）→ 市场分页搜索按
  `expert_id` 精确落位 → 常量表兜底。市场不可用不报错。
- 判据链：真实对话（服务端 `requestId`）→ 召唤事件 → chat 链的
  `agent_task_created` 补 `has_expert/expert_id/expert_name/expert_industry_id` →
  `expert_actual_use(mode=LOCAL, type="", cost=0)`。

## 3. `skill_share`（G3）

- 市场技能列表 → `https://open.workbuddy.cn/api/v2/open-platform/asset-shares/c2c/channels`
  创建真实分享渠道（`asset_ref` 保持官方客户端要求的 JSON **字符串**形态）→
  上报 `skill_share_channel_action` 两条（`clipboard` 的 `click` / `success`）。
- 市场没有可分享技能时跳过，不伪造分享；渠道创建成功但事件上报失败时不领奖。
- 新增 `upstream.Client.OpenBaseCN` 字段（默认 `https://open.workbuddy.cn`，
  仅供部署/测试覆盖，未新增 config 键），请求仍走账号绑定的出口线路。

## 4. 小程序活动任务（G4）

- 新增 `{billingBase}/portal/activity/*` 协议与 spec 表：星巴克协作
  （`ai_chat` / `expert`）与记者节福利（`expert_chat` / `skill_use` /
  `desktop_same`），共 5 项。
- 流程：`tasks → viewed（need_view）→ 判据事件 → 快速轮询 → 慢速轮询 →
  claim → 回读 claimed_at`。claim 成功但未回读到 `claimed_at` 只算「未确认」，
  不重复 POST。
- 活动任务以合成码 `campaign:<activity_code>:<task_code>` 接入任务中心：
  扫描、执行队列、单任务「立即完成」、账号任务列表、手动「领取」都可见/可执行。
- `app` 任务首版跳过（需 ACP bootstrap + SSE 校验 `stopReason`，设计 D5）；
  `desktop_same` 只上报一条桌面域 `playbook_prompt_send`，`id` 使用产物目录名
  `cnb-content-calendar`。

## 5. 健壮性硬化（G5）

| 项 | 行为 |
|---|---|
| `credit_delayed=true` | 签到照常记 `ok`，detail 标注「积分待入账」，不触发当日重签 |
| 未确认领奖去重 | claim 回执无金额且回读未落账时登记未确认，当前轮次内不再重复 POST；回读确认 `claimed` 或拿到到账额后自动清除。下一轮（队列启动 / 单号一键完成 / 补领清扫）先回读再决定是否重试一次 |
| `valid_start/valid_end` | `Task` 新增字段；`locked` 且 `valid_start` 在未来的任务直接跳过（单任务/全量/队列三处同口径） |
| 执行链 401 恢复 | 任务列表/领奖/动作首个请求 401 或 12153 时刷新 token 并重放一次；刷新成功后落盘 |

## 6. 行为变化与注意事项

- **无新增配置键**：无需修改 `config.json`。盲盒随旅行排程执行（`travel_hours`）；
  活动任务随任务中心扫描/执行队列执行（`queue_enabled` 为 opt-in）。
- `accountTaskAutoAll` 的后端兜底超时从 5 分钟放宽到 9 分钟（活动任务含判据落库
  轮询；前端 `LONG_TIMEOUT` 仍为 10 分钟）。
- 任务补领清扫中，领奖回执未确认的条目计入 `deferred`（下一轮回读确认），
  不会在同一轮重复 POST。
- `expert_use_*` 与 `WorkBuddy_Valorant` 登记为 mp 回读/领奖口径：小程序任务列表
  是默认口径的超集，`ClaimRewardMP` 在 chat 域返回 400 时会自动降级 Web 域。

## 7. 验证

- 新增测试：
  - `internal/upstream`：深链解析（含不可信 host/多 ID/非法 ID）、盲盒 quota/open、
    专家市场分页精确落位、技能分享渠道体、活动任务 paths/view/claim、活动事件形状、
    `credit_delayed`、旅行 claim 未确认。
  - `internal/scheduler`：盲盒六种状态顺序（idle / traveling / 未知 / arrived→idle /
    claim 未确认 / claim 后仍非 idle）、签到延迟入账。
  - `internal/panel`：活动任务全链、app 排除、技能分享、jump_url 覆盖常量表、
    未确认领奖去重、401 刷新重放、`valid_start` 跳过。
- `go test ./...` 全绿。

## 8. 未包含 / 后续

- `GET /v2/activity/growth/badges` 徽章只读展示（设计 G6，低价值）。
- Campaign `app` 任务（设计 D5）。
- MP 对话节奏 8s / 15s / 45s 的 A/B 实测（设计 R2）仍需在真实账号上完成；
  本次未改动既有 45s 节奏，活动任务自带轮询间隔。
- 盲盒 / 专家族 / 活动任务均需真实账号线上复测（端点与形状来自参考实现与
  设计文档，测试仅覆盖协议层）。
