'use client';

import {createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode} from 'react';
import {accountApi, authApi} from '@/lib/api';
import {clearAllCache, warmCache} from '@/lib/data-cache';
import type {Me, Role} from '@/lib/types';

const ME_CACHE_KEY = 'wb-me';

/**
 * 登录后预热的慢端点：/api/packages 要逐账号向上游查余额（最慢 1-2 秒），
 * /api/overview 是账号页与首页的主体数据。登录完成前就在后台拉，
 * 用户落到第一个页面时缓存里大概率已有数据——首屏直接出内容。
 */
const WARM_TARGETS = [
  {key: 'packages', fetcher: () => accountApi.packages()},
  {key: 'overview', fetcher: () => accountApi.overview()},
];

/** 缓存登录态，避免每次整页加载都先空一下再填充 */
function readCachedMe(): Me | null {
  if (typeof window === 'undefined') return null;
  try {
    const raw = window.sessionStorage.getItem(ME_CACHE_KEY);
    return raw ? (JSON.parse(raw) as Me) : null;
  } catch {
    return null;
  }
}

function writeCachedMe(me: Me | null): void {
  if (typeof window === 'undefined') return;
  try {
    if (me) window.sessionStorage.setItem(ME_CACHE_KEY, JSON.stringify(me));
    else window.sessionStorage.removeItem(ME_CACHE_KEY);
  } catch {
    /* 隐私模式下 sessionStorage 可能不可用，忽略 */
  }
}

interface AuthContextValue {
  me: Me | null;
  loading: boolean;
  isAdmin: boolean;
  role: Role | null;
  refresh: () => Promise<void>;
  login: (username: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({children}: {children: ReactNode}) {
  const [me, setMe] = useState<Me | null>(null);
  // 有缓存时直接视为已就绪，页面无需为登录校验停留
  const [loading, setLoading] = useState(true);
  const hydrated = useRef(false);

  const refresh = useCallback(async () => {
    try {
      const data = await authApi.me();
      setMe(data);
      writeCachedMe(data);
    } catch {
      setMe(null);
      writeCachedMe(null);
    } finally {
      setLoading(false);
    }
  }, []);

  const login = useCallback(async (username: string, password: string) => {
    const res = await authApi.login(username, password);
    const next = {username: res.username, role: res.role as Role};
    setMe(next);
    writeCachedMe(next);
    setLoading(false);
    warmCache(WARM_TARGETS);
  }, []);

  const logout = useCallback(async () => {
    try {
      await authApi.logout();
    } catch {
      /* 忽略登出异常 */
    }
    setMe(null);
    writeCachedMe(null);
    // 数据缓存一并清掉：换账号登录不应看到上一会话的池/统计快照
    clearAllCache();
    if (typeof window !== 'undefined') window.location.href = '/login';
  }, []);

  useEffect(() => {
    if (hydrated.current) return;
    hydrated.current = true;
    // 先用缓存立即还原界面，再后台校验一次
    const cached = readCachedMe();
    if (cached) {
      setMe(cached);
      setLoading(false);
    }
    refresh();
    // 已登录（或缓存视为已登录）就预热慢端点：登录跳转完成前数据已在路上
    if (cached) warmCache(WARM_TARGETS);
  }, [refresh]);

  return (
    <AuthContext.Provider
      value={{me, loading, isAdmin: me?.role === 'admin', role: me?.role ?? null, refresh, login, logout}}
    >
      {children}
    </AuthContext.Provider>
  );
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error('useAuth 必须在 AuthProvider 内使用');
  return ctx;
}
