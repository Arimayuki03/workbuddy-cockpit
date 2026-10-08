'use client';

/**
 * 设置页「关于」分区（原 TabsContent value="about"，原样搬移，只拆不重构）。
 */
import {Settings as SettingsIcon} from 'lucide-react';
import {useI18n} from '@/lib/i18n/provider';
import {RichText} from '@/lib/i18n/rich-text';

export function AboutTab() {
  const {t} = useI18n();

  return (
    <div className="mt-4">
      <div className="rounded-[20px] bg-muted p-5 text-xs leading-6 text-muted-foreground">
        <div className="mb-2 flex items-center gap-2 text-sm font-medium text-foreground">
          <SettingsIcon className="h-4 w-4" />
          WorkBuddy Cockpit
        </div>
        <p>
          {t('settings.aboutDesc')}
        </p>
        <p className="mt-3">
          {t('settings.aboutCredits')}
        </p>
        <p className="mt-3">
          <RichText text={t('settings.aboutUsage')} />
        </p>
      </div>
    </div>
  );
}
