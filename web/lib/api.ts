import axios, {AxiosError} from 'axios';
import type {
  Account,
  CheckinLog,
  AccountsResponse,
  ApiKey,
  IpAccessLog,
  IpRule,
  Me,
  ModelInfo,
  Page,
  ReloadState,
  RequestLog,
  SecurityConfig,
  StatsSummary,
  UpstreamConfig,
  UpstreamStatus,
  UsageBreakdown,
  UsagePoint,
  UserItem,
} from './types';

export const http = axios.create({
  baseURL: '',
  withCredentials: true,
  timeout: 60000,
});

/** 统一抽取后端错误信息 */
export function errText(e: unknown): string {
  const ax = e as AxiosError<{detail?: string; error?: string}>;
  const d = ax?.response?.data;
  return (typeof d === 'string' ? d : d?.detail || d?.error) || ax?.message || '请求失败';
}

http.interceptors.response.use(
  (r) => r,
  (error: AxiosError) => {
    if (error.response?.status === 401 && typeof window !== 'undefined') {
      // 会话失效：清掉缓存的登录态，避免仍显示管理员入口
      window.sessionStorage.removeItem('wb-me');
      if (!window.location.pathname.startsWith('/login')) {
        window.location.href = '/login';
      }
    }
    return Promise.reject(error);
  },
);

const get = async <T>(url: string, params?: Record<string, unknown>): Promise<T> =>
  (await http.get<T>(url, {params})).data;
const post = async <T>(url: string, body?: unknown): Promise<T> =>
  (await http.post<T>(url, body)).data;
const patch = async <T>(url: string, body?: unknown): Promise<T> =>
  (await http.patch<T>(url, body)).data;
const del = async <T>(url: string): Promise<T> => (await http.delete<T>(url)).data;

/* ── 鉴权 ───────────────────────────────────────────── */
export const authApi = {
  me: () => get<Me>('/api/me'),
  login: (username: string, password: string) =>
    post<{ok: boolean; username: string; role: string}>('/api/login', {username, password}),
  logout: () => post<{ok: boolean}>('/api/logout'),
};

/* ── 账号 ───────────────────────────────────────────── */
export const accountApi = {
  list: () => get<AccountsResponse>('/api/accounts'),
  start: () => post<{state: string; authUrl: string}>('/api/auth/start'),
  poll: (state: string) =>
    get<{
      status: 'waiting' | 'success' | 'expired' | 'invalid';
      uid?: string;
      nickname?: string;
      updated?: boolean;
    }>('/api/auth/poll', {state}),
  remove: (file: string) => del<{success: boolean}>(`/api/accounts/${encodeURIComponent(file)}`),
  checkin: (file: string) =>
    post<{code: number; message: string}>(`/api/accounts/${encodeURIComponent(file)}/checkin`),
  checkinAll: () =>
    post<{total: number; succeeded: number; results: {nickname: string; ok: boolean; message: string}[]}>(
      '/api/accounts/checkin-all',
    ),
  checkinLogs: (limit = 200, uid?: string) => get<CheckinLog[]>('/api/checkin-logs', {limit, uid}),
  clearCheckinLogs: () => post<{ok: boolean}>('/api/checkin-logs/clear'),
  upstreamLogs: (limit = 200) =>
    get<{available: boolean; lines: string[]; total: number}>('/api/upstream/logs', {limit}),
  test: (file: string) =>
    post<{ok: boolean; message: string}>(`/api/accounts/${encodeURIComponent(file)}/test`),
  refresh: (file: string) =>
    post<{ok: boolean; message: string}>(`/api/accounts/${encodeURIComponent(file)}/refresh`),
  restart: () => post<{ok: boolean; message: string}>('/api/restart'),
};

/* ── 上游状态 ───────────────────────────────────────── */
export const upstreamApi = {
  status: () => get<UpstreamStatus>('/api/status'),
  models: () => get<ModelInfo[]>('/api/models'),
};

/* ── API 密钥 ───────────────────────────────────────── */
export const keyApi = {
  list: () => get<ApiKey[]>('/api/keys'),
  create: (body: Partial<ApiKey>) => post<ApiKey>('/api/keys', body),
  update: (id: number, body: Partial<ApiKey>) => patch<ApiKey>(`/api/keys/${id}`, body),
  remove: (id: number) => del<{ok: boolean}>(`/api/keys/${id}`),
  resetUsage: (id: number) => post<{ok: boolean}>(`/api/keys/${id}/reset-usage`),
};

/* ── 日志 ───────────────────────────────────────────── */
export const logApi = {
  list: (params: Record<string, unknown>) => get<Page<RequestLog>>('/api/logs', params),
  clear: () => post<{ok: boolean}>('/api/logs/clear'),
};

/* ── 用量统计 ───────────────────────────────────────── */
export const statsApi = {
  summary: () => get<StatsSummary>('/api/stats/summary'),
  daily: (days = 30) => get<UsagePoint[]>('/api/stats/daily', {days}),
  byModel: (days = 30) => get<UsageBreakdown[]>('/api/stats/by-model', {days}),
  byKey: (days = 30) => get<UsageBreakdown[]>('/api/stats/by-key', {days}),
};

/* ── 安全 / IP ──────────────────────────────────────── */
export const securityApi = {
  config: () => get<SecurityConfig>('/api/security/config'),
  saveConfig: (body: SecurityConfig) => post<SecurityConfig>('/api/security/config', body),
  rules: () => get<IpRule[]>('/api/security/rules'),
  addRule: (body: {kind: 'allow' | 'deny'; cidr: string; note?: string}) =>
    post<IpRule>('/api/security/rules', body),
  removeRule: (id: number) => del<{ok: boolean}>(`/api/security/rules/${id}`),
  logs: (limit = 200) => get<IpAccessLog[]>('/api/security/logs', {limit}),
  logsClear: () => post<{ok: boolean}>('/api/security/logs/clear'),
};

/* ── 上游配置 / 用户 ───────────────────────────────── */
export const settingsApi = {
  upstream: () => get<UpstreamConfig>('/api/settings/upstream'),
  saveUpstream: (body: Record<string, unknown>) =>
    post<UpstreamConfig>('/api/settings/upstream', body),
  /** 测试 Upstash 连通性；token 留空表示使用已保存的值 */
  testUpstash: (url: string, token?: string) =>
    post<{ok: boolean; message: string}>('/api/settings/upstash/test', {url, token}),
  /** 上游重载状态（保存配置后自动重启） */
  reloadState: () => get<ReloadState>('/api/upstream/reload-state'),
  /** 立即重启上游（一般无需手动调用，保存配置会自动重载） */
  reloadUpstream: () =>
    post<{ok: boolean; message: string}>('/api/settings/upstash/reload'),
  modelMap: () => get<Record<string, string>>('/api/settings/model-map'),
  saveModelMap: (body: Record<string, string>) =>
    post<Record<string, string>>('/api/settings/model-map', body),
  users: () => get<UserItem[]>('/api/users'),
  addUser: (body: {username: string; password: string; role: string}) =>
    post<UserItem>('/api/users', body),
  updateUser: (username: string, body: {password?: string; role?: string}) =>
    patch<UserItem>(`/api/users/${encodeURIComponent(username)}`, body),
  removeUser: (username: string) => del<{ok: boolean}>(`/api/users/${encodeURIComponent(username)}`),
};

export type {Account};
