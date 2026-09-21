'use client';

import {Languages} from 'lucide-react';

import {LOCALE_LABELS, type Locale} from '@/lib/i18n/config';
import {useI18n} from '@/lib/i18n/provider';
import {cn} from '@/lib/utils';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';

/**
 * 语言切换。
 *
 * 放在版本切换旁边（页面右上角）：与 RealmToggle 一样不在浮动底栏上加控件，
 * 底栏保持 LDC 原样。选项用各语言的母语写法，选完即写入 localStorage。
 */
export function LanguageToggle({className}: {className?: string}) {
  const {locale, setLocale, t, locales} = useI18n();

  return (
    <Select value={locale} onValueChange={(value) => setLocale(value as Locale)}>
      <SelectTrigger
        aria-label={t('language.switch')}
        title={t('language.switch')}
        className={cn(
          // 与 ThemeToggle / RealmToggle 完全同款（高 32px 圆胶囊，h-6 时代
          // 偏小已整体放大）：
          // - 基类高度带 data-[size] 变体（h-9/h-8），特异性高于裸 h-8，
          //   必须用同变体写法逐个压制为 h-8；
          // - 基类 border-none 与 border 不同合并组，须用 border-solid 同组覆盖；
          // - chevron 由基类注入（size-4 opacity-50，直接子 svg），
          //   用 [&>svg] 压成 size-3.5/opacity-70；Languages 图标类名含 size-，
          //   才不会被基类 [&_svg:not([class*='size-'])]:size-4 改写；
          // - 基类 select-value 的 text-xs/text-sm 用同选择器组覆盖为 text-xs。
          'h-8 w-auto gap-1.5 justify-start rounded-full border border-solid border-border/60 bg-muted/60 px-3 py-0 text-xs font-medium transition-colors hover:bg-muted/80 dark:bg-muted/60 dark:hover:bg-muted/80',
          'data-[size=default]:h-8 data-[size=sm]:h-8',
          '*:data-[slot=select-value]:text-xs [&:not([data-placeholder])_[data-slot=select-value]]:text-xs',
          '[&>svg]:size-3.5 [&>svg]:opacity-70',
          className,
        )}
      >
        <Languages className="size-3.5 shrink-0 opacity-70" />
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        {locales.map((id) => (
          <SelectItem key={id} value={id} className="text-xs">
            {LOCALE_LABELS[id]}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}
