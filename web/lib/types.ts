export type Role = 'admin' | 'viewer';

export interface Me {
  username: string;
  role: Role;
}

export interface Account {
  /** 该令牌签发的总时长（秒）；后端从 JWT 解出，解不出为 null */
  ttl_seconds?: number | null;
  file: string;
  uid: string;
  nickname: string;
  enterprise_id: string;
  expires_at: number;
  is_expired: boolean;
  remain_seconds: number;
  /** 当前可花费积分余额（上游聚合套餐剩余额度），null 表示尚未同步 */
  credits?: number | null;
  /** 来自 workbuddy2api /status 的运行时字段，可能为空 */
  healthy?: boolean | null;
  disabled?: boolean | null;
  in_flight?: number | null;
  cooling?: boolean | null;
  success_count?: number | null;
  err_total?: number | null;
  breaker_fails?: number | null;
  last_success?: string | null;
  last_used?: number | null;
  source: 'file' | 'pool';
}

export interface AccountsResponse {
  total: number;
  accounts: Account[];
  /** 已从上游同步到运行时状态的账号数 */
  pool_synced?: number;
  /** 上游是否可达 */
  pool_available?: boolean;
}

export interface UpstreamStatus {
  connected: boolean;
  accounts?: Record<string, unknown>[];
  cooling?: number;
  disabled?: number;
  healthy?: number;
  total?: number;
  in_flight_full?: number;
  redis_mode?: string;
  sticky_sessions?: number;
  error?: string;
}

export interface ModelInfo {
  id: string;
  owned_by?: string;
  context_window?: number;
}

export interface ApiKey {
  id: number;
  name: string;
  prefix: string;
  enabled: boolean;
  expires_at: number | null;
  max_ips: number;
  ip_allowlist: string[];
  models: string[];
  quota: number | null;
  used_tokens: number;
  created_at: number;
  last_used_at: number | null;
  /** 仅在创建时返回一次 */
  key?: string;
}

export interface RequestLog {
  id: number;
  ts: number;
  key_id: number | null;
  key_name: string | null;
  ip: string;
  model: string | null;
  mapped_model: string | null;
  status: number;
  prompt_tokens: number;
  completion_tokens: number;
  latency_ms: number;
  ua: string | null;
  error: string | null;
  stream: boolean;
  /** 本次调用的真实扣费（上游 usage.credit）；null = 上游未返回，不是 0 */
  credit: number | null;
}

export interface UsagePoint {
  day: string;
  requests: number;
  prompt_tokens: number;
  completion_tokens: number;
  /** 当日实际扣费合计 */
  credit: number;
}

export interface UsageBreakdown {
  name: string;
  requests: number;
  prompt_tokens: number;
  completion_tokens: number;
  /** 该维度实际扣费合计 */
  credit: number;
}

export interface StatsSummary {
  today_requests: number;
  today_tokens: number;
  /** 实际扣费（上游 usage.credit 合计）；上游未返回该字段时恒为 0 */
  today_credit: number;
  week_credit: number;
  total_credit: number;
  week_requests: number;
  week_tokens: number;
  total_requests: number;
  total_tokens: number;
  active_keys: number;
  top_model: string | null;
}

export interface IpRule {
  id: number;
  kind: 'allow' | 'deny';
  cidr: string;
  note: string;
  created_at: number;
}

export interface IpAccessLog {
  id: number;
  ts: number;
  ip: string;
  path: string;
  blocked: boolean;
  ua: string | null;
}

export interface SecurityConfig {
  enabled: boolean;
  mode: 'whitelist' | 'blacklist';
}

export interface UserItem {
  username: string;
  role: Role;
}

export interface UpstashConfig {
  /** Upstash 地址，支持 https://xxx.upstash.io 或 xxx.upstash.io */
  url: string;
  /** 是否已配置 token（内容不回传） */
  has_token: boolean;
  /** token 掩码，仅用于展示 */
  token_masked: string;
}

export interface UpstreamConfig {
  /** 是否成功读取到上游配置文件；false 时前端应提示并禁止保存 */
  available?: boolean;
  /** 上游配置文件路径 */
  config_path?: string;
  /** 读取失败原因 */
  error?: string;
  listen?: string;
  api_key_masked?: string;
  auth_dir?: string;
  /** 上游 config.json 中声明的 auth_dir，与管理端不一致时出现 */
  upstream_auth_dir?: string;
  schedule?: Record<string, unknown>;
  pool?: Record<string, unknown>;
  cooldown?: Record<string, unknown>;
  features?: Record<string, unknown>;
  session_sticky?: Record<string, unknown>;
  prompt?: Record<string, unknown>;
  server?: Record<string, unknown>;
  upstream?: Record<string, unknown>;
  upstash?: UpstashConfig;
}

/** 积分查询来源：实时查询 or 命中 60 秒缓存 */
export interface CreditsMeta {
  cached: boolean;
  cache_age: number | null;
  message?: string;
}

export interface CheckinLog {
  id: number;
  ts: number;
  uid: string;
  nickname: string;
  /** 触发来源：manual 手动 / manual-batch 批量 / add 添加账号时 */
  source: string;
  /** 类型：checkin 签到 / keepalive 保活 */
  kind: string;
  success: boolean;
  code: number | null;
  message: string;
}

/** 上游自动任务留痕（猫猫旅行 / 活跃上报 / 自动签到 / 保活） */
export interface TaskLog {
  id: number;
  ts: number;
  uid: string;
  /** travel / activity / checkin / keepalive / user-resource / credit */
  kind: string;
  /** credit 有积分收益 / ok 成功 / info 跳过 / warn 警告 / error 失败 */
  level: string;
  credits: number;
  /** 上游英文原文（排查用，界面上作为悬浮提示） */
  message: string;
  /** 中文展示文案 */
  message_cn?: string;
  /** 账号昵称（后端按 uid/前缀解析；上游日志只带 uid 前 8 位） */
  nickname?: string;
}

export interface TaskLogResponse {
  logs: TaskLog[];
  /** 当前筛选下的总条数（分页用；与 stats.total 在未筛选时一致） */
  total: number;
  stats: {
    by_kind: Record<string, {count: number; credits: number}>;
    total: number;
    total_credits: number;
  };
  kinds: Record<string, string>;
  collector: {at?: number; parsed?: number; added?: number; error?: string};
}


/** 签到记录分页返回 */
export interface CheckinLogPage {
  items: CheckinLog[];
  /** 当前筛选下的总条数 */
  total: number;
}

export interface UpdateLogLine {
  ts: number;
  level: string;
  text: string;
}

export interface UpdateStatus {
  available: boolean;
  /** 是否正在更新 */
  running: boolean;
  /** 上次更新是否成功（null = 未运行过） */
  ok: boolean | null;
  step: string;
  logs: UpdateLogLine[];
  /** 当前部署版本 */
  version: string;
  updater_found: boolean;
  upstream_dir: string;
  /** 当前固定的上游版本（空 = 跟随分支） */
  upstream_ref?: string;
  started_at?: number;
  finished_at?: number | null;
  duration?: number;
  pid?: number;
  /** 日志原文（便于复制反馈） */
  log_tail?: string;
}

export interface VersionSide {
  /** 当前版本 */
  current: string;
  /** 远端最新 */
  latest: string;
  /** 是否有更新可用 */
  has_update: boolean;
  /** 检测失败原因 */
  error: string;
}

export interface ManagerVersion extends VersionSide {
  /** Release 页面地址 */
  url: string;
  repo: string;
}

/** 上游两个版本之间的单条提交 */
export interface UpstreamChange {
  sha: string;
  subject: string;
  date: string;
}

export interface UpstreamVersion extends VersionSide {
  /** 最新提交时间 */
  date: string;
  /** 最新提交说明 */
  subject: string;
  /** 本地落后远端多少个提交（0 = 未知或不落后） */
  ahead?: number;
  /** 两版本之间的提交总数 */
  total?: number;
  /** 变更说明列表（最新在前，最多 20 条） */
  changes?: UpstreamChange[];
  /** 提交数超过展示上限，列表被截断 */
  truncated?: boolean;
  repo: string;
}

export interface UpdateCheck {
  /** 检测时间（秒） */
  checked_at: number;
  /** 是否为缓存结果 */
  cached: boolean;
  manager: ManagerVersion;
  upstream: UpstreamVersion;
  /** 任一组件有更新 */
  has_any: boolean;
}

export interface Versions {
  manager: string;
  upstream_connected: boolean;
  upstream_accounts: number | null;
  upstream_dir: string;
}

export interface ReloadState {
  /** 正在执行重启 */
  running: boolean;
  /** 有重启在排队（短时间内多次改动的合并） */
  pending: boolean;
  /** 上次重启完成时间（秒） */
  last_at: number;
  last_ok: boolean | null;
  last_message: string;
  restart_count: number;
}

export interface Page<T> {
  total: number;
  items: T[];
}
