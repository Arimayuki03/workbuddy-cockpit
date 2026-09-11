'use client';

import {createContext, useCallback, useContext, useEffect, useState, type ReactNode} from 'react';
import {authApi, http} from '@/lib/api';
import type {Me, Role} from '@/lib/types';

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
  const [loading, setLoading] = useState(true);

  const refresh = useCallback(async () => {
    try {
      const data = await authApi.me();
      setMe(data);
    } catch {
      setMe(null);
    } finally {
      setLoading(false);
    }
  }, []);

  const login = useCallback(async (username: string, password: string) => {
    const res = await authApi.login(username, password);
    setMe({username: res.username, role: res.role as Role});
  }, []);

  const logout = useCallback(async () => {
    try {
      await authApi.logout();
    } catch {
      /* 忽略登出异常 */
    }
    setMe(null);
    if (typeof window !== 'undefined') window.location.href = '/login';
  }, []);

  useEffect(() => {
    // 仅在同一会话内拉取一次，避免多页面重复请求
    if (http.defaults.headers.common['x-me-loaded'] === '1' && me) return;
    refresh();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

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
