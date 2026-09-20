/**
 * 后端契约类型（WorkBuddy Cockpit）。
 *
 * 数据源分三类：
 *   1. panel 移植端点（/api/overview、/api/usage、/api/logs、任务中心等）——
 *      字段名照 panel Go 源码的 json tag 抄（蛇形命名是后端原样透出，不做转换）；
 *   2. 本网关原生端点（/status、/v1/chat/completions）——照主仓库
 *      internal/pool/entry.go 与 internal/server/metrics.go 的结构体抄；
 *   3. 尚未实现、双方按本文件约定的端点（/api/request_logs、
 *      /api/settings/model-map、/api/system/check-update）——形状是约定值，
 *      后端落地时以这里为准。
 */

/* ── 鉴权 ───────────────────────────────────────────── */

export type Role = 'admin' | 'viewer';

export interface Me {
  username: string;
  role: Role;
}

/** 账号域（与 lib/realm-context 一致；types 不 import React 模块，此处独立声明） */
export type RealmType = 'cn' | 'global';

/* ── 账号池（panel overview / 原生 /status）────────────── */

/**
 * 账号池单条状态。
 *
 * 主仓库 internal/pool/entry.go 的 `pool.Status` 原样透出（/api/overview 与
 * /status 同一份）。token 有效期相关字段（expires_at / remain_seconds 等）
 * 属 manager Python 中间层的产物，Go 侧没有——列表的时间信息改由
 * last_success / last_err 等运行时字段承担。
 */
export interface Account {
  uid: string;
  nickname?: string;
  /** 账号域：cn / global；旧 state 无该字段时前端按 cn 处理 */
  realm?: 'cn' | 'global';
  credits: number;
  /** 积分总额度（各套餐聚合）；0 = 未知 */
  credits_total?: number;
  cooling: boolean;
  cool_kind?: string;
  /** 冷却剩余秒数；0/缺失 = 未在冷却 */
  cool_remaining_sec?: number;
  reason?: string;
  /** 被限流的模型台账（6004 model rate limit 等逐条列出，到期即消失） */
  rate_limited_models?: {
    model: string;
    until?: string;
    reset_at?: string;
    reason?: string;
  }[];
  /** 每模型实测成本台账（仅有效观测期内非空） */
  model_costs?: {
    model: string;
    cost_per_1k?: number;
    last_seen?: string;
    samples?: number;
  }[];
  disabled: boolean;
  disabled_reason?: string;
  /** 运维手动停用位（与 disabled 并列独立，任务照常、只摘转发流量） */
  manual_disabled?: boolean;
  manual_reason?: string;
  success_count?: number;
  err_total?: number;
  last_success?: string;
  last_err?: string;
  /** 连续失败计数（连败降权进度；零值也显式输出） */
  consecutive_fails?: number;
  /** 连败降权截止（RFC3339；在未来 = 正被降权） */
  degrade_until?: string;
  in_flight?: number;
  breaker_fails?: number;
  breaker_until?: string;
  /** 每模型在途请求数（只含非零条目） */
  in_flight_by_model?: Record<string, number>;
  /** mergePoolStatus 的推导标记：本次读不到池状态（仅前端展示用） */
  pool_unknown?: boolean;
}

/** /status 的按域计数汇总（与顶层五元组同构） */
export interface PoolCounts {
  total: number;
  healthy: number;
  cooling: number;
  disabled: number;
  in_flight_full: number;
}

/**
 * 原生 GET /status（主仓库 internal/server/handler.go status()）。
 * 仪表盘的账号健康卡片读它；panel 的 /api/overview 提供同源的池计数。
 */
export interface UpstreamStatus {
  accounts: Account[];
  total: number;
  healthy: number;
  cooling: number;
  disabled: number;
  in_flight_full: number;
  realm_totals: Record<string, PoolCounts>;
  sticky_sessions: number;
  redis_mode: string;
  cost_explore: {
    events_total: number;
    per_model: Record<string, string>;
  };
}

/** GET /api/overview（panel overview 处理器原样形状） */
export interface OverviewResponse {
  version: string;
  uptime_sec: number;
  auth_required: boolean;
  redis_mode: string;
  sticky_sessions: number;
  total: number;
  healthy: number;
  cooling: number;
  disabled: number;
  in_flight_full: number;
  accounts: Account[];
}

/* ── OAuth 添加账号（panel login.go）───────────────────── */

export interface AuthStartResponse {
  ok: boolean;
  /** 授权链接（前端转二维码） */
  url: string;
  state: string;
  realm: RealmType;
}

export interface AuthPollResponse {
  done: boolean;
  message?: string;
  uid?: string;
  nickname?: string;
  realm?: string;
  credits?: number;
  credits_total?: number;
  checkin_message?: string;
}

export interface AuthRegion {
  code: string;
  name: string;
}

export interface AuthRegionsResponse {
  ok: boolean;
  regions: AuthRegion[];
}

/* ── 账号操作（panel 形状）──────────────────────────────── */

/** 单号签到（panel accountCheckin：ok 恒 true，业务失败在 *_error 字段里） */
export interface AccountCheckinResponse {
  ok: boolean;
  checkin_message?: string;
  credits?: number;
  credits_total?: number;
  balance_error?: string;
}

export interface AccountBalanceResponse {
  ok: boolean;
  credits: number;
  credits_total: number;
}

export interface OkResponse {
  ok: boolean;
  file_error?: string;
}

/** 批量任务触发（checkin_all / travel_all / activity_all / keepalive_all） */
export interface BatchStartResponse {
  ok: boolean;
  started: boolean;
}

/** 全量余额刷新：同步等待，完成后返回最新池快照 */
export interface BalanceAllResponse {
  ok: boolean;
  accounts: Account[];
}

/* ── 积分包（panel packages）────────────────────────────── */

export interface CreditPackage {
  name: string;
  remain: number;
  used: number;
  size: number;
  /** 该包周期结束时间（上游 ExpiredTime / PackageEndTime 取有值者） */
  end_time?: string;
  /** 发放时刻 RFC3339——区分「首登赠送」与「活动奖励」的唯一依据 */
  created_at?: string;
  package_code?: string;
  sub_product_code?: string;
}

export interface PackageRow {
  uid: string;
  nickname: string;
  realm: string;
  remain: number;
  size: number;
  packages: CreditPackage[];
  error?: string;
}

export interface PackagesResponse {
  accounts: PackageRow[];
}

/* ── 任务中心（panel tasks.go / taskcenter.go）───────────── */

/** 成长任务（panel upstream.Task 原样：蛇形 json tag） */
export interface GrowthTask {
  task_code: string;
  title?: string;
  description?: string;
  task_desc?: string;
  credit?: number;
  energy?: number;
  has_reward?: boolean;
  reward_buddy?: boolean;
  task_type?: string;
  tag?: string;
  jump_url?: string;
  locked?: boolean;
  /** 目标次数（恒输出：0 是有效进度值） */
  target: number;
  /** 当前进度（恒输出） */
  current: number;
  accept_status?: string;
  status?: string;
  /** 进度达标且未领取（panel 本地推算） */
  claimable?: boolean;
  claimed?: boolean;
}

export interface AccountTasksResponse {
  ok: boolean;
  tasks: GrowthTask[];
}

export interface TaskAcceptResponse {
  ok: boolean;
  accepted?: number;
  failed?: string[];
  message?: string;
}

/** 一键完成单任务（panel accountTaskAuto） */
export interface TaskAutoResponse {
  ok: boolean;
  skipped?: boolean;
  message?: string;
  progress_before?: string;
  progress_after?: string;
  claimable?: boolean;
  attempt?: number;
  verify_supported?: boolean;
  claimed?: boolean;
  credit?: number;
  energy?: number;
  claim_error?: string;
}

/** 一键完成全部任务的单项结果（panel runAutoAll） */
export interface AutoAllResult {
  task_code: string;
  status: 'done' | 'error' | string;
  message?: string;
  credit?: number;
  energy?: number;
}

export interface TaskAutoAllResponse {
  ok: boolean;
  results?: AutoAllResult[];
}

/** 领取任务奖励（panel accountTaskClaim） */
export interface TaskClaimResponse {
  ok: boolean;
  already_claimed?: boolean;
  message?: string;
  credit?: number;
  energy?: number;
}

/* ── 任务中心：扫描与执行队列（panel taskcenter.go）────────── */

/** 开学季任务条目（面板展示口径） */
export interface SchoolTaskView {
  task_code: string;
  /** pending | in_progress | completed | claimed */
  status: string;
  progress: number;
  target_count: number;
}

/** 单账号扫描结果 */
export interface ScanAccountItem {
  uid: string;
  nickname?: string;
  /** 未完成且可自动化的成长任务 */
  growth?: GrowthTask[];
  growth_error?: string;
  /** 未完成的开学季任务 */
  school?: SchoolTaskView[];
  school_error?: string;
  in_period?: boolean;
}

export interface ScanAllResponse {
  ok: boolean;
  accounts: ScanAccountItem[];
  pending_count: number;
}

/** 队列执行单元 */
export interface QueueItem {
  uid: string;
  nickname?: string;
  /** growth | school */
  kind: string;
  code: string;
  /** pending | running | done | skipped | error */
  status: string;
  message?: string;
}

export interface TaskQueueResponse {
  running: boolean;
  total: number;
  conc: number;
  started: boolean;
  started_at: string;
  seq: number;
  items: QueueItem[];
}

export interface TaskRunQueueResponse {
  ok: boolean;
  started: boolean;
  total?: number;
  seq?: number;
  message?: string;
}

/* ── 开学季（panel taskcenter.go school*）────────────────── */

export interface SchoolAccountView {
  uid: string;
  nickname?: string;
  in_period: boolean;
  tasks: SchoolTaskView[];
  /** 剩余抽奖次数 */
  chances: number;
  error?: string;
}

export interface SchoolStatusResponse {
  ok: boolean;
  accounts: SchoolAccountView[];
}

/** 券码行（panel schoolVouchers） */
export interface VoucherRow {
  uid: string;
  nickname?: string;
  vouchers: {
    grant_id: number;
    draw_uuid?: string;
    /** kfc_ice_cream / voucher_luckin / voucher_kugou … */
    sku_code?: string;
    /** 肯德基冰淇淋 */
    prize_name?: string;
    /** 券码本体（复制给店员核销） */
    code: string;
    valid_from?: string;
    valid_to?: string;
    granted_at?: string;
  }[];
  error?: string;
}

export interface VouchersResponse {
  accounts: VoucherRow[];
}

/* ── 模型（panel models / model_probes）─────────────────── */

/** panel 模型条目（两域共用；id 带 cn:/global: 前缀即调用值） */
export interface PanelModel {
  id: string;
  name: string;
  default_effort?: string;
  supported_efforts?: string[];
  can_disable_thinking?: boolean;
  supports_reasoning?: boolean;
  supports_images?: boolean;
  credits?: string;
  description?: string;
  tags?: string[];
  vendor?: string;
  is_default?: boolean;
  supports_tool_call?: boolean;
  only_reasoning?: boolean;
  reasoning_effort?: string;
  reasoning_summary?: string;
  max_allowed_size?: number;
  context_length?: number;
  max_output_tokens?: number;
}

export interface PanelModelsResponse {
  ok: boolean;
  models: PanelModel[];
}

/** 单模型探测结果（scripts/probe_max_tokens.py --panel-out 契约文件条目） */
export interface ModelProbe {
  claimed?: number;
  /** 实测上限；at_least 形态时为下限 */
  measured?: number | null;
  /** ok | clamped | at_least | inconclusive */
  verdict: string;
  note?: string;
  tested_at?: string;
  source?: string;
}

export interface ModelProbesResponse {
  probes: Record<string, ModelProbe>;
  exists: boolean;
  updated_at?: string;
}

/* ── 用量（panel usage.Snapshot）────────────────────────── */

/** 聚合行（panel usage.Agg） */
export interface UsageAgg {
  requests: number;
  errors: number;
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  avg_latency_ms: number;
  avg_tokens_per_second: number;
}

/** 按维度聚合的一行（key + 可选 realm / 昵称） */
export interface UsageKeyedAgg extends UsageAgg {
  key: string;
  realm?: string;
  /** 账号行放昵称 */
  extra?: string;
}

/** 时序上的一个点（scope=hour 时 t 形如 2026-09-21T14；day 时为日期） */
export interface UsageSeriesPoint extends UsageAgg {
  t: string;
  /** hour | day */
  scope: string;
}

/** GET /api/usage?hours=（panel usage.Snapshot 原样） */
export interface UsageSnapshot {
  totals: UsageAgg;
  by_realm: UsageKeyedAgg[];
  by_account: UsageKeyedAgg[];
  by_model: UsageKeyedAgg[];
  series: UsageSeriesPoint[];
  buckets: number;
  file_bytes: number;
  since?: string;
  generated: string;
}

export interface OkOnlyResponse {
  ok: boolean;
}

/* ── 原生 /v1/stats（主仓库 internal/server/metrics.go）────── */

/** /v1/stats 的单模型聚合统计（字段名与社区面板约定一致） */
export interface ModelStat {
  model: string;
  requests: number;
  success: number;
  failed: number;
  streaming: number;
  avg_ttfb_ms: number;
  avg_latency_ms: number;
  tokens_per_sec: number;
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  cache_hit_tokens: number;
  cache_miss_tokens: number;
  cache_write_tokens: number;
  cache_hit_rate: number;
  credit: number;
  credit_per_req: number;
  /** 上游积分倍率原文（如 "x0.06"）；缺失 = 目录未下发或缓存冷 */
  credits?: string;
  last_seen?: string;
}

/** GET /v1/stats 响应（models 按请求数降序） */
export interface MetricsSnapshot {
  enabled: boolean;
  message?: string;
  since: string;
  now: string;
  uptime_sec: number;
  total: ModelStat;
  models: ModelStat[];
}

/* ── 系统日志（panel ring.go 三频道）──────────────────────── */

/** 单条日志（ch: task | chat | sys） */
export interface SystemLogEntry {
  ts: string;
  ch: string;
  text: string;
}

export interface SystemLogsResponse {
  entries: SystemLogEntry[];
}

/* ── 请求日志（环形缓冲：后端 internal/server/requestlog.go）── */

/**
 * 单条请求日志。
 *
 * 后端已实现 /api/request_logs（internal/server/requestlog.go：chatStat.done()
 * 单一埋点写入环形缓冲，容量 1000 条，重启即清；持久化由 usage 分桶承担）。
 * 字段与后端 requestLogEntry 的 json tag 一一对应。
 */
export interface RequestLog {
  /** 全局递增序号（重启清零） */
  seq: number;
  /** 时刻（RFC3339） */
  time: string;
  model: string;
  uid: string;
  nick: string;
  /** "stream" | "sync" */
  mode: string;
  /** HTTP 状态码 */
  status: number;
  /** token 合计；-1 = usage 缺失（观测缺失，非 0 token） */
  tokens: number;
  /** 首字延迟（毫秒） */
  ttfb_ms: number;
  /** 本次真实扣费（上游 usage.credit；has_credit=false 时无观测） */
  credit: number;
  has_credit: boolean;
  error: string;
}

export interface RequestLogsResponse {
  items: RequestLog[];
  dropped: number;
  cap: number;
}

/* ── 配置热编辑（panel config.go）────────────────────────── */

/** GET /api/config（panel config.go） */
export interface ConfigGetResponse {
  ok: boolean;
  path: string;
  /** config.json 原样对象（深合并回写） */
  config: Record<string, unknown>;
}

export interface ConfigSaveResponse {
  ok: boolean;
  /** 需要重启才能生效的字段列表 */
  restart_required: string[];
}

/* ── 设置：Upstash / 模型映射 / 版本检查 ───────────────────── */

export interface UpstashTestResponse {
  ok: boolean;
  message: string;
}

/** GET /api/settings/model-map（后端 server.ModelMapView：生效映射表副本） */
export type ModelMap = Record<string, string>;

/**
 * GET /api/system/check-update（后端 internal/server/version.go）。
 * 只读比对 GitHub release；不做自更新（一键更新已裁剪）。
 * cached=true 表示命中 6h 缓存或网络失败回退缓存值；latest="unknown" 表示无网。
 */
export interface UpdateCheck {
  current: string;
  latest: string;
  has_update: boolean;
  changelog_url: string;
  cached: boolean;
  checked_at?: string;
}

/* ── 模型视图（前端派生）────────────────────────────────── */

/** 模型列表来源标注（panel models 恒为动态探测；无静态回退表） */
export type ModelSource = 'dynamic' | 'static' | 'unknown';

/** /api/models 条目 + /api/model_probes 合成后的前端展示行 */
export interface CatalogModel {
  id: string;
  name: string;
  context_length: number;
  max_output_tokens: number;
  efforts: string[];
  default_effort: string;
  description?: string;
  credits?: string;
  vendor?: string;
  tags?: string[];
  is_default?: boolean;
  supports_reasoning?: boolean;
  supports_tool_call?: boolean;
  only_reasoning?: boolean;
  reasoning_summary?: string;
  supports_images: boolean;
  /** 系列归属（按 id 前缀推导，仅用于分组浏览） */
  series: string;
  /** 实测输出上限（探测数据缺失时为 null，界面不显示标注） */
  probe?: ModelProbe | null;
}

export interface CatalogSummary {
  total: number;
  reasoning: number;
  large_context: number;
  max_context: number;
  series: string[];
  unique_ids: number;
}

export interface ModelCatalog {
  models: CatalogModel[];
  source: ModelSource;
  source_label: string;
  via: string;
  errors: string[];
  cached: boolean;
  cache_age: number;
  summary: CatalogSummary;
}

/* ── 测试台 ─────────────────────────────────────────────── */

export interface PlaygroundModel {
  id: string;
  name: string;
  /** 支持的推理档位；空数组 = 该模型不支持调整 */
  efforts: string[];
  series: string;
}

export interface PlaygroundModels {
  models: PlaygroundModel[];
  source: ModelSource;
  source_label: string;
}
