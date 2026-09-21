'use client';

import {useEffect, useState} from 'react';
import {MoonIcon, SunIcon} from 'lucide-react';
import {useThemeUtils} from '@/hooks/use-theme-utils';
import {useT} from '@/lib/i18n/provider';

/**
 * 主题切换（浅色 / 深色 / 跟随系统）。
 *
 * 放在页面右上角语言切换旁、登录页同款位置：任何页面都能一键切，
 * 不必先打开个人信息弹窗（弹窗里的旧入口已随本组件移除）。
 *
 * 图标与文字用 dark: 变体做 CSS 级切换——next-themes 的实际主题要等
 * 客户端挂载后才确定，若用 state 渲染首帧会与 SSR 不一致（水合报错）；
 * CSS 变体在 html class 就位后立即生效，无闪烁。
 * title / aria-label 不影响渲染结构，挂载后再补真实值。
 */
export function ThemeToggle({className = ''}: {className?: string}) {
  const themeUtils = useThemeUtils();
  const t = useT();
  const [mounted, setMounted] = useState(false);

  useEffect(() => setMounted(true), []);

  return (
    <button
      type="button"
      title={mounted ? themeUtils.getAction() : undefined}
      aria-label={mounted ? themeUtils.getAction() : t('theme.light')}
      onClick={themeUtils.toggle}
      className={`inline-flex h-6 items-center gap-1.5 rounded-full border border-border/60 bg-muted/60 px-2.5 text-[11px] font-medium transition-colors hover:bg-muted/80 ${className}`}
    >
      <SunIcon className="hidden h-3 w-3 shrink-0 opacity-70 dark:block" />
      <MoonIcon className="block h-3 w-3 shrink-0 opacity-70 dark:hidden" />
      <span className="leading-none dark:hidden">{t('theme.light')}</span>
      <span className="hidden leading-none dark:inline">{t('theme.dark')}</span>
    </button>
  );
}
