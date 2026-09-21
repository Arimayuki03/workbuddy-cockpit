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
  OverviewResponse,
  PackagesResponse,
  PanelModelsResponse,
  RequestLogsResponse,
  ScanAllResponse,
  SchoolStatusResponse,
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
} from './types';

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
  const ax = e as AxiosError<{detail?: string; error?: string; message?: string}>;
  const d = ax?.response?.data;
  const raw = (typeof d === 'string' ? d : d?.error || d?.detail || d?.message) || ax?.message || '';
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
const post = async <T>(url: string, body?: unknown, cfg?: {timeout?: number}): Promise<T> =>
  (await http.post<T>(url, body, cfg)).data;
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
  disable: (uid: string) =>
    post<OkResponse>(`/api/accounts/${encodeURIComponent(uid)}/disable`),
  /** 单号签到：签到 + 余额查询解冻（"今天已签到"等业务错误不阻塞余额刷新） */
  checkin: (uid: string) =>
    post<AccountCheckinResponse>(`/api/accounts/${encodeURIComponent(uid)}/checkin`),
  balance: (uid: string) =>
    post<AccountBalanceResponse>(`/api/accounts/${encodeURIComponent(uid)}/balance`),
  remove: (uid: string) =>
    post<OkResponse>(`/api/accounts/${encodeURIComponent(uid)}/remove`),

  /* ── 批量任务（异步执行，进度看系统日志频道）──────────────── */
  checkinAll: () => post<BatchStartResponse>('/api/checkin_all'),
  travelAll: () => post<BatchStartResponse>('/api/travel_all'),
  activityAll: () => post<BatchStartResponse>('/api/activity_all'),
  keepaliveAll: () => post<BatchStartResponse>('/api/keepalive_all'),
  /** 全量余额刷新：同步等待全池逐号查上游，返回最新池快照（长超时） */
  balanceAll: () => post<BalanceAllResponse>('/api/balance_all', undefined, LONG_TIMEOUT),

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
  /** 全账号扫描（只读）：成长任务待办 + 开学季待办 */
  scanAll: () => post<ScanAllResponse>('/api/tasks/scan_all'),
  /** 启动执行队列。body {concurrency?: 1-4, growth?: bool, school?: bool} */
  runQueue: (body: {concurrency?: number; growth?: boolean; school?: boolean} = {}) =>
    post<TaskRunQueueResponse>('/api/tasks/run_queue', body),
  /** 队列状态（轮询用） */
  queue: () => get<TaskQueueResponse>('/api/tasks/queue'),
  /** 手动取消执行队列：剩余待办停止调度，进行中条目自然完成后停止（幂等） */
  cancelQueue: () => post<{ok: boolean; cancelled: boolean}>('/api/tasks/queue/cancel'),
};

/* ── 开学季活动（panel taskcenter.go school*）───────────── */
export const schoolApi = {
  /** 全账号开学季任务状态（含抽奖余额） */
  status: () => get<SchoolStatusResponse>('/api/school/status'),
  /** 一键执行全部账号开学季闭环（异步，进度看任务频道日志） */
  runAll: () => post<BatchStartResponse>('/api/school/run_all'),
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
  /** 用量分桶快照；hours 控制小时粒度时序窗口（默认 72，上限 1440=60 天） */
  usage: (hours = 72) => get<UsageSnapshot>('/api/usage', {hours}),
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
