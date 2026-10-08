'use client';

/**
 * 设置页「版本检查」分区（原 TabsContent value="system"，原样搬移，只拆不重构）：
 * 当前版本 / 最新版本展示 + 手动检查更新（只读，不做自更新）。
 */
import {Sparkles, RefreshCw} from 'lucide-react';
import {useI18n} from '@/lib/i18n/provider';
import {EmptyState} from '@/components/common/layout/EmptyState';
import {Button} from '@/components/ui/button';
import {Badge} from '@/components/ui/badge';
import {useSettings} from './settings-context';

export function SystemTab() {
  const {t} = useI18n();
  const s = useSettings();
  const update = s.update;

  return (
    <div className="mt-4 space-y-4">
      <div className="rounded-[20px] bg-muted p-4">
        <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
          <div className="flex items-center gap-2 text-sm font-medium">
            <Sparkles className="h-4 w-4" />
            {t('updatePanel.currentVersion')}
          </div>
          <Button
            variant="outline"
            size="sm"
            className="rounded-full"
            onClick={s.recheck}
            disabled={s.updateBusy}
          >
            <RefreshCw className={s.updateBusy ? 'animate-spin' : ''} />
            {t('updatePanel.checkUpdate')}
          </Button>
        </div>
        {update ? (
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <div className="rounded-2xl bg-background/60 px-3.5 py-3">
              <div className="text-[11px] text-muted-foreground">{t('updatePanel.manager')}</div>
              <div className="mt-1 text-sm font-semibold tabular-nums">{update.current || '—'}</div>
            </div>
            <div className="rounded-2xl bg-background/60 px-3.5 py-3">
              <div className="text-[11px] text-muted-foreground">{t('updatePanel.latest')}</div>
              <div className="mt-1 flex items-center gap-2 text-sm font-semibold tabular-nums">
                {update.latest || '—'}
                {update.has_update ? (
                  <Badge variant="secondary" className="rounded-full text-amber-600 dark:text-amber-400">
                    {t('updatePanel.hasUpdateBadge', {v: update.latest})}
                  </Badge>
                ) : (
                  <Badge variant="secondary" className="rounded-full text-emerald-600 dark:text-emerald-400">
                    {t('updatePanel.upToDate')}
                  </Badge>
                )}
              </div>
              {update.has_update && update.changelog_url && (
                <a
                  href={update.changelog_url}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="mt-1 inline-block text-[11px] text-blue-500 hover:underline"
                >
                  {t('updatePanel.changelogLink')}
                </a>
              )}
            </div>
          </div>
        ) : (
          <EmptyState
            icon={Sparkles}
            title={t('updatePanel.notChecked')}
            description={t('updatePanel.notCheckedDesc')}
            className="flex flex-col items-center justify-center py-8 text-center"
          />
        )}
        <p className="mt-3 text-[11px] leading-4 text-muted-foreground">
          {t('updatePanel.readonlyNote')}
        </p>
      </div>
    </div>
  );
}
