export type Role = 'admin' | 'viewer';

export interface Me {
  username: string;
  role: Role;
}

export interface Account {
  file: string;
  uid: string;
  nickname: string;
  enterprise_id: string;
  expires_at: number;
  is_expired: boolean;
  remain_seconds: number;
  /** 来自 workbuddy2api /status 的运行时字段，可能为空 */
  healthy?: boolean | null;
  disabled?: boolean | null;
  in_flight?: number | null;
  cooling?: boolean | null;
  success_count?: number | null;
  err_total?: number | null;
  last_used?: number | null;
  source: 'file' | 'pool';
}

export interface AccountsResponse {
  total: number;
  accounts: Account[];
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
}

export interface UsagePoint {
  day: string;
  requests: number;
  prompt_tokens: number;
  completion_tokens: number;
}

export interface UsageBreakdown {
  name: string;
  requests: number;
  prompt_tokens: number;
  completion_tokens: number;
}

export interface StatsSummary {
  today_requests: number;
  today_tokens: number;
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
  upstash?: UpstashConfig;
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
