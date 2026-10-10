import axios, {AxiosError} from 'axios';
import {tp} from './i18n';
import type {Realm} from './realm-context';
import type {
  Account,
  AccountBalanceResponse,
  AccountCheckinResponse,
  AccountTasksResponse,
  AuthPollResponse,
  AuthRegionsResponse,
  AuthStartResponse,
  BalanceAllResponse,
  BatchStartResponse,
  ConfigGetResponse,
  ConfigSaveResponse,
  Me,
  MetricsSnapshot,
  ModelMap,
  ModelMapResponse,
  ModelProbesResponse,
  OkOnlyResponse,
  OkResponse,
  ProxyRoutesResponse,
  AccountImportResponse,
  OverviewResponse,
  PackagesResponse,
  PanelModelsResponse,
  RequestLogsResponse,
  ScanAllResponse,
  SystemLogsResponse,
  TaskAcceptResponse,
  TaskAutoAllResponse,
  TaskAutoResponse,
  TaskClaimResponse,
  TaskQueueResponse,
  TaskRunQueueResponse,
  UpdateCheck,
  UpstashTestResponse,
  UpstreamStatus,
  UsageSnapshot,
  VouchersResponse,
  KeysListResponse,
  KeyCreateResponse,
  KeyUpdateResponse,
  KeyIpsResponse,
  SecurityStatusResponse,
  SecurityRulesPayload,
  SecurityRulesResponse,
  ModelLocksResponse,
  RenewAllResponse,
  TaskRecordsResponse,
  TokensListResponse,
  TokenCreateResponse,
} from './types';

/**
 * HTTP 状态码提取：区分「端点未启用」（501，功能开关没开，提示而非报错）
 * 与「重入/冲突」（409，上一轮还在跑）这类有专属文案的业务状态。
 */
export function httpStatus(e: unknown): number | null {
  const ax = e as AxiosError | undefined;
  return ax?.response?.status ?? null;
}

export const http = axios.create({
  baseURL: '',
  withCredentials: true,
  timeout: 60000,
});

/**
 * 长端点超时（覆盖全局 60s）：
 *  - auto_all 是账号内串行流水线（每项含真实对话 + 回读轮询），后端设计
 *    兜底 5 分钟；balance_all 同步等全池逐号查上游。全局 60s 会先把前端
 *    掐死而后端还在跑，两者统一放宽到 10 分钟。
 */
const LONG_TIMEOUT = {timeout: 600000};

/**
 * 统一抽取后端错误信息。
 *
 * Go 侧错误统一是 `{"ok":false,"error":"..."}`（panel writeErr 口径）或
 * OpenAI 风格；这里过一遍短语表：命中已收录的后端文案就换成当前语言，
 * 没收录的原样展示。
 */
export function errText(e: unknown): string {
  const ax = e as AxiosError<{
    detail?: string;
    message?: string;
    error?: string | {message?: string; code?: string | number};
  }>;
  const d = ax?.response?.data;
  // OpenAI 风格错误体是 {error: {message, code}}——error 是对象而非字符串，
  // 直接 truthy 取值会把对象透给 toast 渲染，炸掉整个 React 树。
  const errField = d?.error;
  const fromErrorObj =
    typeof errField === 'string' ? errField : errField?.message;
  const rawCandidate =
    (typeof d === 'string' ? d : fromErrorObj || d?.detail || d?.message) ||
    ax?.message ||
    '';
  const raw = typeof rawCandidate === 'string' ? rawCandidate : String(rawCandidate);
  return raw ? tp(raw) : tp('请求失败');
}

http.interceptors.response.use(
  (r) => r,
  (error: AxiosError) => {
    if (error.response?.status === 401 && typeof window !== 'undefined') {
      // 会话失效：清掉缓存的登录态，避免仍显示管理入口
      window.sessionStorage.removeItem('wb-me');
      if (!window.location.pathname.startsWith('/login')) {
        window.location.href = '/login';
      }
    }
    return Promise.reject(error);
  },
);

const get = async <T>(url: string, params?: Record<string, unknown>, cfg?: {timeout?: number}): Promise<T> =>
  (await http.get<T>(url, {params, ...cfg})).data;
const post = async <T>(
  url: string,
  body?: unknown,
  cfg?: {timeout?: number; headers?: Record<string, string>},
): Promise<T> => (await http.post<T>(url, body, cfg)).data;
/** PATCH（axios 没有专属 helper，body 序列化口径与 post 一致） */
const patch = async <T>(url: string, body?: unknown): Promise<T> =>
  (await http.patch<T>(url, body)).data;
/** DELETE（带可选 JSON body——token 停用/恢复复用 DELETE + {disabled}） */
const del = async <T>(url: string, body?: unknown): Promise<T> =>
  (await http.delete<T>(url, body ? {data: body} : undefined)).data;
/* ── 鉴权 ───────────────────────────────────────────── */
export const authApi = {
  me: () => get<Me>('/api/me'),
  /** 密码 = api_key；用户名随意填（后端只验密码） */
  login: (username: string, password: string) =>
    post<{ok: boolean; username: string; role: string}>('/api/login', {username, password}),
  logout: () => post<{ok: boolean}>('/api/logout'),
};

/* ── 账号池（panel 移植端点）────────────────────────────── */
export const accountApi = {
  /** 池总览：计数 + 每账号状态 + 面板元信息 */
  overview: () => get<OverviewResponse>('/api/overview'),
  /** 原生 /status：账号健康（realm_totals / cost_explore 台账） */
  status: () => get<UpstreamStatus>('/status'),

  /* ── OAuth 扫码加号（panel login.go）───────────────────
   * start 返回 {ok,url,state,realm}——panel 的 auth_url 在适配层改名为 url。 */
  start: (realm: Realm = 'cn') => post<AuthStartResponse>('/api/auth/start', {realm}),
  poll: (state: string, realm?: Realm, region?: string) =>
    get<AuthPollResponse>('/api/auth/poll', {state, realm, region}),
  regions: () => get<AuthRegionsResponse>('/api/auth/regions'),

  /* ── 单号运维 ──────────────────────────────────────── */
  revive: (uid: string) =>
    post<OkResponse>(`/api/accounts/${encodeURIComponent(uid)}/revive`),
  /** 强制清除冷却/熔断/连败降权/模型级限流（不碰禁用位） */
  clearCooldown: (uid: string) =>
    post<OkResponse>(`/api/accounts/${encodeURIComponent(uid)}/clear-cooldown`),
  disable: (uid: string) =>
    post<OkResponse>(`/api/accounts/${encodeURIComponent(uid)}/disable`),
  /** 单号签到：签到 + 余额查询解冻（"今天已签到"等业务错误不阻塞余额刷新） */
  checkin: (uid: string) =>
    post<AccountCheckinResponse>(`/api/accounts/${encodeURIComponent(uid)}/checkin`),
  balance: (uid: string) =>
    post<AccountBalanceResponse>(`/api/accounts/${encodeURIComponent(uid)}/balance`),
  /** 保存账号备注（后端化，落 state.json；空串 = 清除）。旧 localStorage 数据首载迁移 */
  setNote: (uid: string, note: string) =>
    post<OkResponse>(`/api/accounts/${encodeURIComponent(uid)}/note`, {note}),
  /** 出口线路表（密码脱敏 ***）+ 全池账号当前绑定视图 */
  proxyRoutes: () => get<ProxyRoutesResponse>('/api/proxy_routes'),
  /** 绑定/解绑账号出口线路（空 route = 解绑恢复直连；落 auth 文件 + 内存即时生效） */
  setProxyRoute: (uid: string, route: string) =>
    post<OkResponse>(`/api/accounts/${encodeURIComponent(uid)}/proxy_route`, {route}),
  remove: (uid: string) =>
    post<OkResponse>(`/api/accounts/${encodeURIComponent(uid)}/remove`),

  /* ── 凭据导入/导出（panel transfer.go）─────────────────── */
  /**
   * 导出全部账号凭据为 JSON 文件下载（含 token，勿外传不可信对象）。
   * 走 axios blob 拿响应体，再由调用方触发浏览器保存。
   */
  exportAll: async () => {
    const res = await http.get<Blob>('/api/accounts/export', {
      responseType: 'blob',
    });
    const cd = String(res.headers['content-disposition'] ?? '');
    const m = cd.match(/filename="?([^";]+)"?/i);
    return {blob: res.data, filename: m?.[1] ?? 'workbuddy-accounts.json'};
  },
  /** 导入凭据：body 为导出文件原样（兼容裸数组/单个 auth 文件内容） */
  import: (raw: string) => post<AccountImportResponse>('/api/accounts/import', raw, {
    headers: {'Content-Type': 'application/json'},
  }),

  /* ── 批量任务（异步执行，进度看系统日志频道）──────────────── */
  checkinAll: () => post<BatchStartResponse>('/api/checkin_all'),
  travelAll: () => post<BatchStartResponse>('/api/travel_all'),
  activityAll: () => post<BatchStartResponse>('/api/activity_all'),
  keepaliveAll: () => post<BatchStartResponse>('/api/keepalive_all'),
  /** 全量余额刷新：同步等待全池逐号查上游，返回最新池快照（长超时） */
  balanceAll: () => post<BalanceAllResponse>('/api/balance_all', undefined, LONG_TIMEOUT),
  /** 手动触发全账号续期巡检（异步执行）。重入回 409，未启用回 501。 */
  renewAll: () => post<RenewAllResponse>('/api/renew_all'),

  /* ── 积分包构成（券码/积分来源对比视图）──────────────────── */
  packages: () => get<PackagesResponse>('/api/packages'),
};

/* ── 任务中心（panel tasks.go / taskcenter.go）──────────── */
export const taskApi = {
  /** 单账号全量任务（默认口径 + 小程序口径合并，按 task_code 去重） */
  accountTasks: (uid: string) =>
    get<AccountTasksResponse>(`/api/accounts/${encodeURIComponent(uid)}/tasks`),
  /** 接受任务（报名；幂等）。body {task_codes: string[]} */
  accept: (uid: string, taskCodes: string[]) =>
    post<TaskAcceptResponse>(`/api/accounts/${encodeURIComponent(uid)}/tasks/accept`, {
      task_codes: taskCodes,
    }),
  /** 接受该账号全部尚未接受的任务 */
  acceptAll: (uid: string) =>
    post<TaskAcceptResponse>(`/api/accounts/${encodeURIComponent(uid)}/tasks/accept_all`),
  /** 领取任务奖励。body {task_code: string} */
  claim: (uid: string, taskCode: string) =>
    post<TaskClaimResponse>(`/api/accounts/${encodeURIComponent(uid)}/tasks/claim`, {
      task_code: taskCode,
    }),
  /** 一键完成单个任务（动作 + 回读 + 自动领奖） */
  auto: (uid: string, taskCode: string) =>
    post<TaskAutoResponse>(`/api/accounts/${encodeURIComponent(uid)}/tasks/auto`, {
      task_code: taskCode,
    }),
  /** 一键完成全部可自动任务（账号内串行流水线，后端兜底 5 分钟；长超时） */
  autoAll: (uid: string) =>
    post<TaskAutoAllResponse>(
      `/api/accounts/${encodeURIComponent(uid)}/tasks/auto_all`,
      undefined,
      LONG_TIMEOUT,
    ),

  /* ── 扫描与执行队列 ─────────────────────────────────── */
  /** 全账号扫描（只读）：成长任务待办 */
  scanAll: () => post<ScanAllResponse>('/api/tasks/scan_all'),
  /** 启动执行队列。body {concurrency?: 1-4, growth?: bool} */
  runQueue: (body: {concurrency?: number; growth?: boolean} = {}) =>
    post<TaskRunQueueResponse>('/api/tasks/run_queue', body),
  /** 队列状态（轮询用） */
  queue: () => get<TaskQueueResponse>('/api/tasks/queue'),
  /** 手动取消执行队列：剩余待办停止调度，进行中条目自然完成后停止（幂等） */
  cancelQueue: () => post<{ok: boolean; cancelled: boolean}>('/api/tasks/queue/cancel'),

  /* ── 积分流水 ─────────────────────────────────────────── */
  /** 积分流水（签到/任务入账与余额跳变，按时间倒序）。未启用时后端回 501。 */
  records: (uid?: string) =>
    get<TaskRecordsResponse>('/api/tasks/records', uid ? {uid} : undefined),
};

/* ── 开学季券码（panel taskcenter.go schoolVouchers）──────── */
// 任务状态与一键闭环已随活动结束（2026-09-24）下线；券码查询保留（历史券码仍可查）。
export const schoolApi = {
  /** 我的券码（逐 CN 账号查询） */
  vouchers: () => get<VouchersResponse>('/api/school/vouchers'),
};

/* ── 模型与实测上限 ───────────────────────────────────── */
export const modelApi = {
  /** 实时查询上游模型列表与 reasoning 实际档位（直连上游，不读路由层缓存） */
  models: (realm?: Realm) => get<PanelModelsResponse>('/api/models', realm ? {realm} : undefined),
  /** 模型输出上限探测结果（probe_max_tokens.py 契约文件只读透传） */
  probes: () => get<ModelProbesResponse>('/api/model_probes'),
};

/* ── 用量统计 ───────────────────────────────────────── */
export const statsApi = {
  /** 用量分桶快照；hours 控制时间窗（默认 72，上限 1440=60 天；'all'=自记录以来全量） */
  usage: (hours: number | 'all' = 72) => get<UsageSnapshot>('/api/usage', {hours}),
  /** 立即把内存中的用量桶落盘（正常由后台 30s 防抖负责） */
  saveUsage: () => post<OkOnlyResponse>('/api/usage/save'),
  /** 原生 /v1/stats：按模型聚合的请求统计（主仓库 metrics.go 形状） */
  native: () => get<MetricsSnapshot>('/v1/stats'),
};

/* ── 日志 ───────────────────────────────────────────── */
export const logApi = {
  /** 系统日志（panel ring 三频道：task | chat | sys，最近 500 行） */
  system: (channel: 'task' | 'chat' | 'sys' | '' = '', limit = 200) =>
    get<SystemLogsResponse>('/api/logs', {
      ...(channel ? {channel} : {}),
      limit,
    }),
  /** 请求日志（internal/server/requestlog.go 环形缓冲；items 最新在前） */
  requestLogs: (limit = 200) => get<RequestLogsResponse>('/api/request_logs', {limit}),
};

/* ── 设置 ───────────────────────────────────────────── */
export const settingsApi = {
  /** 读取当前配置（config.json 原样对象 + 路径） */
  config: () => get<ConfigGetResponse>('/api/config'),
  /** 保存配置：body 直接是配置 JSON（深合并 + 原子替换 + 保留未知键） */
  saveConfig: (body: Record<string, unknown>) => post<ConfigSaveResponse>('/api/config', body),
  /** 测试 Upstash 连通性；token 留空表示使用已保存的值 */
  testUpstash: (url: string, token?: string) =>
    post<UpstashTestResponse>('/api/settings/upstash/test', {url, token}),
  /** 模型映射（后端 server.ModelMapView / SetModelMap + 配置写回）；响应是 {ok,map} 信封 */
  modelMap: () => get<ModelMapResponse>('/api/settings/model-map'),
  saveModelMap: (map: ModelMap) => post<ModelMapResponse>('/api/settings/model-map', {map}),
  /** 版本检查（后端 internal/server/version.go；只读比对，不做自更新） */
  checkUpdate: () => get<UpdateCheck>('/api/system/check-update'),
};

export type {Account};

/* ── API 密钥管理（对外网关分发密钥）────────────────────────
 * 契约（后端 internal/panel/keys_ep.go，双方按 types.ts 落地）：
 *  - GET    /api/keys              → {keys: ApiKey[]}
 *  - POST   /api/keys              → {key, plaintext}（明文仅此一次）
 *  - PATCH  /api/keys/{id}         → {key}（全字段可选）
 *  - DELETE /api/keys/{id}
 *  - POST   /api/keys/{id}/reset_usage
 *  - GET    /api/keys/{id}/ips     → {ips: [{ip, first_seen}]}
 * 4xx 错误体是 {ok: false, error: "字符串"}（panel.writeErr），展示时直接
 * 用 errText 取 error 文案即可。 */
export const keyApi = {
  list: () => get<KeysListResponse>('/api/keys'),
  create: (body: KeyCreatePayload) => post<KeyCreateResponse>('/api/keys', body),
  update: (id: string, body: KeyUpdatePayload) =>
    patch<KeyUpdateResponse>(`/api/keys/${encodeURIComponent(id)}`, body),
  remove: (id: string) => post<OkResponse>(`/api/keys/${encodeURIComponent(id)}/delete`),
  resetUsage: (id: string) => post<OkResponse>(`/api/keys/${encodeURIComponent(id)}/reset_usage`),
  ips: (id: string) => get<KeyIpsResponse>(`/api/keys/${encodeURIComponent(id)}/ips`),
};

/** POST /api/keys 请求体（expires_at 为 Unix 秒，0 = 永不过期） */
export interface KeyCreatePayload {
  name: string;
  /** 'cn' | 'global' | ''（不限制，仅存量密钥） */
  realm: 'cn' | 'global' | '';
  expires_at: number;
  max_ips: number;
  ip_whitelist: string[];
  model_whitelist: string[];
  token_quota: number;
  credit_quota: number;
  /** 密钥级限流（req/min）；0 = 默认 */
  rate_limit: number;
}

/** PATCH /api/keys/{id} 请求体：与创建相同但全字段可选 */
export type KeyUpdatePayload = Partial<KeyCreatePayload> & {enabled?: boolean};

/* ── 管理面作用域 Token（wbt_，internal/panel/tokens.go）──────────
 * 契约（双方按 types.ts 的 PanelToken 落地）：
 *  - POST   /api/tokens        body {name, scope} → {ok, token, token_record}
 *                              （token 是仅此一次的明文）
 *  - GET    /api/tokens        → {ok, tokens}（无明文无摘要）
 *  - DELETE /api/tokens/{id}   → {ok:true}；带 body {disabled: true|false}
 *                              为停用/恢复（缺省直接删除）
 * 这组端点**仅会话 cookie 可用**（Bearer 一律 401 session_required）——
 * http 实例 withCredentials 天然满足，切勿给它们手动加 Authorization 头。 */
export const tokenApi = {
  list: () => get<TokensListResponse>('/api/tokens'),
  create: (body: {name: string; scope: 'readonly' | 'admin'}) =>
    post<TokenCreateResponse>('/api/tokens', body),
  /** 删除令牌；传 disabled 时改为停用/恢复（保留审计与 last_used 历史） */
  remove: (id: string) => del<OkResponse>(`/api/tokens/${encodeURIComponent(id)}`),
  setDisabled: (id: string, disabled: boolean) =>
    del<OkResponse>(`/api/tokens/${encodeURIComponent(id)}`, {disabled}),
};

/* ── 安全（入站 IP 管控 / 拦截日志 / 模型锁池）────────────────
 * 契约（后端 internal/panel/security_ep.go）：
 *  - GET  /api/security            → {rules, blocked_logs}
 *  - POST /api/security/rules      → {errs: string[], error?}（errs 非空 = 校验未通过
 *                                    整体未生效；errs 空但 error 非空 = 规则已生效、
 *                                    config 落盘失败，重启会回落旧值）
 *  - GET  /api/security/model_locks → {locks: ModelLockRow[]} */
export const securityApi = {
  status: () => get<SecurityStatusResponse>('/api/security'),
  saveRules: (body: SecurityRulesPayload) => post<SecurityRulesResponse>('/api/security/rules', body),
  modelLocks: () => get<ModelLocksResponse>('/api/security/model_locks'),
};
