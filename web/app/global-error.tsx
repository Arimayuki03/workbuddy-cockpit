'use client';

/**
 * 根级兜底页（global-error）：error.tsx 之上的最后一道防线。
 *
 * 当根布局自身抛异常（I18nProvider / AuthProvider / ThemeProvider 崩了）时，
 * error.tsx 不在渲染树里无法接管，由本页兜底。Next 要求 global-error 必须
 * 自带 <html>/<body>——此时根布局整棵都没了，字体变量、暗色 class 也拿不到：
 * 先按浅色渲染，挂载后读 localStorage 里的主题偏好手动补上 dark class，
 * 避免深色用户在系统崩溃页上突然白屏闪眼。
 */
import {useEffect, useState} from 'react';
import {RefreshCw, TriangleAlert} from 'lucide-react';
import {Button} from '@/components/ui/button';

/** next-themes 的存储键（ThemeProvider storageKey 默认值，保持一致） */
const THEME_STORAGE_KEY = 'theme';

export default function GlobalErrorPage({
  error,
  reset,
}: {
  error: Error & {digest?: string};
  reset: () => void;
}) {
  const [dark, setDark] = useState(false);
  const [message, setMessage] = useState('');

  useEffect(() => {
    console.error(error);
    setMessage(error?.message || '');
    try {
      setDark(window.localStorage.getItem(THEME_STORAGE_KEY) === 'dark');
    } catch {/* 存储不可用：保持浅色 */}
  }, [error]);

  useEffect(() => {
    document.documentElement.classList.toggle('dark', dark);
  }, [dark]);

  return (
    <html lang="zh-CN" className={dark ? 'dark' : ''} suppressHydrationWarning>
      <body className="antialiased">
        <div className="fixed inset-0 flex items-center justify-center bg-background px-4 text-foreground">
          <div className="flex w-full max-w-md flex-col items-center rounded-[20px] bg-muted/50 p-8 text-center">
            <div className="mb-4 grid h-15 w-15 place-items-center rounded-full bg-muted">
              <TriangleAlert className="h-8 w-8 text-amber-500" />
            </div>
            <p className="mb-2 text-base font-bold">Something went wrong</p>
            <p className="mb-1 text-xs text-muted-foreground">
              页面发生了未预期的错误，界面未能正常渲染。
            </p>
            {message && (
              <p className="mt-2 max-h-24 w-full overflow-y-auto break-all rounded-lg bg-muted/60 px-3 py-2 font-mono text-[10px] leading-relaxed text-muted-foreground">
                {message}
              </p>
            )}
            <Button className="mt-6 rounded-full" onClick={reset}>
              <RefreshCw />
              重试
            </Button>
          </div>
        </div>
      </body>
    </html>
  );
}
