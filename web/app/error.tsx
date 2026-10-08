'use client';

/**
 * 渲染异常兜底页（App Router error boundary）。
 *
 * 只捕获本段布局树内的渲染异常：数据页崩了不再白屏/黑屏，给出错误摘要与
 * 「重试」入口。重试用 reset()——App Router 会重新渲染出错的那段树，
 * 不需要整页刷新，登录态与已加载的其它段落都还在。
 *
 * 不放业务布局（ManagementBar 等）：出错时那些组件自身也可能是肇事者，
 * 兜底页必须最小化依赖。深浅色走 html class 上的 dark 变体（ThemeProvider
 * 已在根布局，class 模式切换对独立渲染的 error 页依然生效）。
 */
import {useEffect, useState} from 'react';
import {RefreshCw, TriangleAlert} from 'lucide-react';
import {Button} from '@/components/ui/button';
import {useT} from '@/lib/i18n/provider';

export default function ErrorPage({
  error,
  reset,
}: {
  error: Error & {digest?: string};
  reset: () => void;
}) {
  const t = useT();
  // 渲染期直接读 error.message 会在重试后 UI 不刷新（Next 官方建议落 state）
  const [message, setMessage] = useState('');

  useEffect(() => {
    // 上报钩子位：真要接监控时在这里调 reporting tool
    console.error(error);
    setMessage(error?.message || '');
  }, [error]);

  return (
    <div className="fixed inset-0 flex items-center justify-center bg-background px-4">
      <div className="flex w-full max-w-md flex-col items-center rounded-[20px] bg-muted/50 p-8 text-center">
        <div className="mb-4 grid h-15 w-15 place-items-center rounded-full bg-muted">
          <TriangleAlert className="h-8 w-8 text-amber-500" />
        </div>
        <p className="mb-2 text-base font-bold">{t('errorBoundary.title')}</p>
        <p className="mb-1 text-xs text-muted-foreground">{t('errorBoundary.desc')}</p>
        {message && (
          <p className="mt-2 max-h-24 w-full overflow-y-auto break-all rounded-lg bg-muted/60 px-3 py-2 font-mono text-[10px] leading-relaxed text-muted-foreground">
            {message}
          </p>
        )}
        <Button className="mt-6 rounded-full" onClick={reset}>
          <RefreshCw />
          {t('errorBoundary.retry')}
        </Button>
      </div>
    </div>
  );
}
