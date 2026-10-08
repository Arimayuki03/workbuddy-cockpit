'use client';

/**
 * 全局命令面板（⌘K / Ctrl+K）。
 *
 * 吸收 workbuddy-manager 的快捷唤起思路：键盘党不必去底部 Dock 找入口，
 * 一处搜索即可完成「跳页 / 切主题 / 刷新账号」三类高频动作。
 *
 * 实现要点：
 *  - 挂载在 main layout（登录后才有意义：跳转目标全是管理页，未登录时
 *    layout 的跳登录逻辑会兜住）；
 *  - 全局键盘监听只在窗口层面挂一个 keydown：只拦 ⌘K/Ctrl+K 这一个组合，
 *    不影响输入框打字与浏览器其它快捷键；
 *  - 动作条目在打开时才构建：主题标签、账号数都依赖当前渲染时刻的状态，
 *    关着面板时轮询它们毫无意义；
 *  - 主题项用 useThemeUtils.toggle 同款三态循环，文案直接复用 theme.* 键。
 */
import {useCallback, useEffect, useMemo, useState} from 'react';
import {useRouter} from 'next/navigation';
import {
  BarChart3,
  Boxes,
  ClipboardList,
  Coins,
  KeyRound,
  Monitor,
  Moon,
  RefreshCw,
  ScrollText,
  Settings,
  ShieldAlert,
  Sun,
  TrendingUp,
  Users,
} from 'lucide-react';
import {useTheme} from 'next-themes';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/animate-ui/radix/dialog';
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from '@/components/ui/command';
import {accountApi, errText} from '@/lib/api';
import {notify} from '@/lib/toast';
import {useT, useI18n} from '@/lib/i18n/provider';
import {useThemeUtils} from '@/hooks/use-theme-utils';
import {LOCALE_LABELS, type Locale} from '@/lib/i18n/config';

/** ⌘K 唤起键：Mac 上是 Meta，其余平台是 Ctrl */
function isComboKey(e: KeyboardEvent): boolean {
  return e.key.toLowerCase() === 'k' && (e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey;
}

export function CommandPalette() {
  const router = useRouter();
  const t = useT();
  const {locale, setLocale, locales} = useI18n();
  const themeUtils = useThemeUtils();
  const {theme} = useTheme();
  const [open, setOpen] = useState(false);
  const [mounted, setMounted] = useState(false);

  useEffect(() => setMounted(true), []);

  // ⌘K / Ctrl+K 全局唤起；再按一次或 Esc 关闭（Dialog 自带 Esc）。
  // 浏览器原生 ⌘K 是地址栏搜索：preventDefault 只在命中面板快捷键时调用，
  // 不干扰其它任何按键。
  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      if (!isComboKey(e)) return;
      e.preventDefault();
      setOpen((prev) => !prev);
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, []);

  /** 跳页后必须关面板：留着遮罩会让用户以为还要再操作一步 */
  const go = useCallback(
    (href: string) => {
      setOpen(false);
      router.push(href);
    },
    [router],
  );

  const pages = useMemo(
    () => [
      {href: '/dashboard', label: t('nav.dashboard'), icon: BarChart3},
      {href: '/accounts', label: t('nav.accounts'), icon: Users},
      {href: '/keys', label: t('nav.keys'), icon: KeyRound},
      {href: '/tasks', label: t('nav.tasks'), icon: ClipboardList},
      {href: '/models', label: t('nav.models'), icon: Boxes},
      {href: '/playground', label: t('nav.playground'), icon: Coins},
      {href: '/stats', label: t('nav.stats'), icon: TrendingUp},
      {href: '/logs', label: t('nav.logs'), icon: ScrollText},
      {href: '/security', label: t('nav.security'), icon: ShieldAlert},
      {href: '/settings', label: t('nav.settings'), icon: Settings},
    ],
    [t],
  );

  /** 全量刷新余额（同步等全池查完，长超时），成功后广播账号变更 */
  const refreshBalances = useCallback(async () => {
    setOpen(false);
    try {
      await accountApi.balanceAll();
      notify.ok(t('accounts.balanceAllDone'));
      window.dispatchEvent(new Event('workbuddy-manager:accounts-changed'));
    } catch (e) {
      notify.err(errText(e));
    }
  }, [t]);

  /** 主题三态循环：与右上角 ThemeToggle 完全同一条切换链 */
  const toggleTheme = useCallback(() => {
    setOpen(false);
    themeUtils.toggle();
  }, [themeUtils]);

  // 主题项文案要等挂载后再信 theme（hydration 安全，与 ThemeToggle 同约定）。
  // 图标随主题态切换：浅色显示月亮（点了去深色）、深色显示太阳、系统态显示显示器。
  const themeLabel = !mounted
    ? t('theme.switchToDark')
    : theme === 'light'
      ? t('theme.switchToDark')
      : theme === 'dark'
        ? t('theme.switchToLight')
        : t('theme.system');

  const ThemeIcon = !mounted ? Monitor : theme === 'dark' ? Sun : Moon;

  const languages = useMemo(
    () => locales.map((l: Locale) => ({code: l, label: LOCALE_LABELS[l]})),
    [locales],
  );

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogContent
        showCloseButton={false}
        className="max-w-lg overflow-hidden p-0 [&>button]:hidden"
        // cmdk 自管键盘导航（上下选择 / Enter 执行），Dialog 的方向键翻焦点
        // 会打断列表光标，整体关掉才干净。
        onOpenAutoFocus={(e) => {
          e.preventDefault();
          // 焦点仍要进面板：交给 cmdk 的输入框
          requestAnimationFrame(() => {
            (document.querySelector('[data-slot="command-input"]') as HTMLInputElement | null)
              ?.focus();
          });
        }}
      >
        <DialogHeader className="sr-only">
          <DialogTitle>{t('commandPalette.title')}</DialogTitle>
          <DialogDescription>{t('commandPalette.desc')}</DialogDescription>
        </DialogHeader>
        <Command loop>
          <CommandInput placeholder={t('commandPalette.placeholder')} />
          <CommandList>
            <CommandEmpty>{t('commandPalette.empty')}</CommandEmpty>
            <CommandGroup heading={t('commandPalette.groupPages')}>
              {pages.map((p) => (
                <CommandItem
                  key={p.href}
                  value={`${p.label} ${p.href}`}
                  onSelect={() => go(p.href)}
                >
                  <p.icon className="opacity-70" />
                  <span className="flex-1">{p.label}</span>
                  <span className="font-mono text-[10px] text-muted-foreground">{p.href}</span>
                </CommandItem>
              ))}
            </CommandGroup>
            <CommandGroup heading={t('commandPalette.groupActions')}>
              <CommandItem
                value={`${t('commandPalette.refreshBalances')} balance`}
                onSelect={refreshBalances}
              >
                <RefreshCw className="opacity-70" />
                <span className="flex-1">{t('commandPalette.refreshBalances')}</span>
                <span className="text-[10px] text-muted-foreground">{t('commandPalette.action')}</span>
              </CommandItem>
              <CommandItem
                value={`${themeLabel} theme`}
                onSelect={toggleTheme}
              >
                <ThemeIcon className="opacity-70" />
                <span className="flex-1">{themeLabel}</span>
                <span className="text-[10px] text-muted-foreground">{t('commandPalette.action')}</span>
              </CommandItem>
            </CommandGroup>
            <CommandGroup heading={t('commandPalette.groupLanguage')}>
              {languages.map((l) => (
                <CommandItem
                  key={l.code}
                  value={`${l.label} ${l.code}`}
                  // 当前语言标为选中且不可再选：重复切换无意义
                  disabled={l.code === locale}
                  onSelect={() => {
                    setOpen(false);
                    setLocale(l.code);
                  }}
                >
                  <span className="flex-1">{l.label}</span>
                  {l.code === locale && (
                    <span className="text-[10px] text-muted-foreground">{t('commandPalette.current')}</span>
                  )}
                </CommandItem>
              ))}
            </CommandGroup>
          </CommandList>
          {/* 底部快捷键提示条（非交互，纯说明） */}
          <div className="flex items-center justify-between border-t border-border/60 px-3 py-2 text-[10px] text-muted-foreground">
            <span>{t('commandPalette.footerNavigate')}</span>
            <span className="font-mono">Esc {t('commandPalette.footerClose')}</span>
          </div>
        </Command>
      </DialogContent>
    </Dialog>
  );
}
