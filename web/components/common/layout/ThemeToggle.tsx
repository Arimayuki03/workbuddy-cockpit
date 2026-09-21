'use client';

import {useEffect, useState} from 'react';
import {MonitorIcon, MoonIcon, SunIcon} from 'lucide-react';
import {useTheme} from 'next-themes';
import {useThemeUtils} from '@/hooks/use-theme-utils';
import {useT} from '@/lib/i18n/provider';

/**
 * 主题切换：三态循环（浅色 → 深色 → 跟随系统 → 浅色）。
 *
 * 放在页面右上角语言切换旁、登录页同款位置：任何页面都能一键切，
 * 不必先打开个人信息弹窗（弹窗里的旧入口已随本组件移除）。
 *
 * 水合安全：next-themes 的实际主题要等客户端挂载后才确定，「是否处于
 * 跟随系统态」的判断（isSystem）必须在 mounted 之后再渲染，否则首帧
 * 与 SSR 不一致会报水合错误——挂载后的重渲染改 DOM 是安全的。
 * 未挂载 / 浅深两态沿用 dark: 变体做 CSS 级切换（html class 就位即生效，
 * 无闪烁）；跟随系统态显示 Monitor 图标 + systemShort 文案。
 * title / aria-label 不影响渲染结构，挂载后再补真实值。
 */
export function ThemeToggle({className = ''}: {className?: string}) {
  const themeUtils = useThemeUtils();
  const {theme} = useTheme();
  const t = useT();
  const [mounted, setMounted] = useState(false);

  useEffect(() => setMounted(true), []);

  // theme 首帧（含 SSR）拿不到真实值，必须等 mounted 再信它。
  const isSystem = mounted && theme === 'system';

  return (
    <button
      type="button"
      title={mounted ? themeUtils.getAction() : undefined}
      aria-label={mounted ? themeUtils.getAction() : t('theme.light')}
      onClick={themeUtils.toggle}
      // 高 32px（h-8）：此前 24px 的圆胶囊偏小、点击热区不足，三个切换器
      // 一并放大（字号 12px / 图标 14px 随之加大）。
      className={`inline-flex h-8 items-center gap-1.5 rounded-full border border-border/60 bg-muted/60 px-3 text-xs font-medium transition-colors hover:bg-muted/80 ${className}`}
    >
      {isSystem ? (
        <>
          <MonitorIcon className="h-3.5 w-3.5 shrink-0 opacity-70" />
          <span className="leading-none">{t('theme.systemShort')}</span>
        </>
      ) : (
        <>
          <SunIcon className="hidden h-3.5 w-3.5 shrink-0 opacity-70 dark:block" />
          <MoonIcon className="block h-3.5 w-3.5 shrink-0 opacity-70 dark:hidden" />
          <span className="leading-none dark:hidden">{t('theme.light')}</span>
          <span className="hidden leading-none dark:inline">{t('theme.dark')}</span>
        </>
      )}
    </button>
  );
}
