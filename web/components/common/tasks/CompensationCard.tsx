'use client';

import {useState} from 'react';
import {Coins, Loader2, Play, Save} from 'lucide-react';
import {notify} from '@/lib/toast';
import {errText, compensationApi, settingsApi} from '@/lib/api';
import {useCachedAsync} from '@/lib/data-cache';
import type {ConfigGetResponse} from '@/lib/types';
import {fmtNumber} from '@/lib/format';
import {useT} from '@/lib/i18n/provider';
import {RichText} from '@/lib/i18n/rich-text';
import {useAuth} from '@/lib/auth-context';
import {Button} from '@/components/ui/button';
import {Badge} from '@/components/ui/badge';
import {Input} from '@/components/ui/input';
import {Switch} from '@/components/ui/switch';
import {hoursToText, parseHoursInput} from '@/components/settings/settings-fields';

/**
 * 「每日领补偿」卡片（任务页）。
 *
 * 平台会把活动补偿（比如故障补偿）发给账号，以前只能去网页「用量管理」手动领。
 * 这里给出与设置页同一份配置（schedule.compensation_enabled / compensation_hours）
 * 的快捷入口，交互沿用任务页既有任务行的风格：
 *  - 开关 = 是否**每天自动**领（默认关：补偿公告常带截止日期，手动点更稳妥）；
 *  - 时刻 = 自动领取的整点，可多个；留空 = 只手动执行、不自动跑；
 *  - 「立即执行」= 当场领一次，回报「几个号领到、共多少积分」。
 *
 * 补偿是「有则领、没有就跳过」的幂等动作，重复执行不会多领；国际版账号上游
 * 没有这套活动，后端会自动跳过。配置读写走 /api/config 深合并（只提交
 * schedule 下的这两个键），缓存键与设置页一致（settings:config），保存后
 * 刷新同一份缓存——在设置页切回来看到的也是新值。
 */
export function CompensationCard() {
  const t = useT();
  const {isAdmin} = useAuth();

  const cfgCache = useCachedAsync<ConfigGetResponse>(
    'settings:config',
    () => settingsApi.config(),
    {ttl: 15_000},
  );
  const cfg = cfgCache.data;
  const schedule = (cfg?.config?.schedule ?? {}) as {
    compensation_enabled?: unknown;
    compensation_hours?: unknown;
  };
  const enabled = schedule.compensation_enabled === true;
  const serverHours = hoursToText(schedule.compensation_hours);

  /** 时刻输入的本地编辑态：null = 未编辑（跟随服务端值），保存「时刻」或撤销后回到 null */
  const [hoursEdit, setHoursEdit] = useState<string | null>(null);
  /** 在途操作：转圈 + 禁用入口（同一次只允许一个动作） */
  const [busy, setBusy] = useState<null | 'toggle' | 'hours' | 'run'>(null);

  const shownHours = hoursEdit ?? serverHours;
  const hoursDirty = hoursEdit !== null && hoursEdit !== serverHours;
  const parsed = parseHoursInput(shownHours, true);
  /** 卡片内所有交互控件（开关 / 时刻输入 / 保存 / 撤销 / 立即执行）的唯一可用口径 */
  const canEdit = isAdmin && !!cfg && busy === null;

  /** 保存 schedule 下的补偿配置（深合并，只动提交的键） */
  async function save(patch: {compensation_enabled?: boolean; compensation_hours?: number[]}, kind: 'toggle' | 'hours') {
    setBusy(kind);
    try {
      await settingsApi.saveConfig({schedule: patch});
      // 只有保存「时刻」才清编辑态：保存开关不应连带丢弃正在输入的时刻
      if (kind === 'hours') setHoursEdit(null);
      await cfgCache.refresh();
      notify.ok(t('settings.saved'), t('settings.applying'));
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setBusy(null);
    }
  }

  /** 立即执行：当场领一次并回报结果 */
  async function runNow() {
    setBusy('run');
    try {
      // 字段可能缺失（老后端 / 部分失败）：一律按 0 容错，不显示 undefined
      const r = await compensationApi.run();
      const detail = t('tasks.compRunDetail', {
        accounts: fmtNumber(r?.accounts ?? 0),
        claimed: fmtNumber(r?.claimed ?? 0),
        credits: fmtNumber(r?.credits ?? 0),
      });
      if (r?.ok === false) {
        notify.err(r.message || t('tasks.compRunFailed'), detail);
      } else {
        notify.ok(t('tasks.compRunDone'), r?.message ? `${detail} · ${r.message}` : detail);
      }
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setBusy(null);
    }
  }

  return (
    <section className="rounded-[20px] bg-muted p-4">
      <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2 text-sm font-medium">
          <Coins className="h-4 w-4" />
          {t('tasks.compTitle')}
          <Badge
            variant="secondary"
            className={
              'rounded-full ' +
              (enabled ? 'text-emerald-600 dark:text-emerald-400' : 'text-muted-foreground')
            }
          >
            {enabled ? t('tasks.compAutoOn') : t('tasks.compAutoOff')}
          </Badge>
        </div>
        {isAdmin && (
          <Button
            size="sm"
            className="h-7 rounded-full text-[11px]"
            disabled={!canEdit}
            onClick={runNow}
          >
            {busy === 'run' ? <Loader2 className="animate-spin" /> : <Play />}
            {t('tasks.compRun')}
          </Button>
        )}
      </div>

      <RichText
        className="block text-[11px] leading-4 text-muted-foreground"
        text={t('tasks.compDesc')}
      />

      <div className="mt-3 grid grid-cols-1 gap-1.5 xl:grid-cols-2">
        {/* 是否每天自动跑 */}
        <div className="flex items-center justify-between gap-3 rounded-2xl bg-background/60 px-3 py-2">
          <div className="min-w-0">
            <div className="text-xs font-medium">{t('tasks.compEnabled')}</div>
            <RichText
              className="mt-0.5 block text-[11px] leading-4 text-muted-foreground"
              text={t('tasks.compEnabledDesc')}
            />
          </div>
          <Switch
            checked={enabled}
            disabled={!canEdit}
            aria-label={t('tasks.compEnabled')}
            onCheckedChange={(v) => save({compensation_enabled: v}, 'toggle')}
          />
        </div>

        {/* 自动执行的整点 */}
        <div className="flex items-center justify-between gap-3 rounded-2xl bg-background/60 px-3 py-2">
          <div className="min-w-0">
            <div className="text-xs font-medium">{t('tasks.compHours')}</div>
            <RichText
              className="mt-0.5 block text-[11px] leading-4 text-muted-foreground"
              text={t('tasks.compHoursDesc')}
            />
            {!parsed.ok && (
              <div className="mt-0.5 text-[10px] leading-3 text-destructive">{parsed.error}</div>
            )}
          </div>
          <div className="flex shrink-0 items-center gap-1.5">
            <Input
              value={shownHours}
              disabled={!canEdit}
              placeholder={t('tasks.compHoursPlaceholder')}
              onChange={(e) => setHoursEdit(e.target.value)}
              className={
                'h-8 w-28 bg-background text-right tabular-nums' +
                (parsed.ok ? '' : ' border-destructive')
              }
            />
            {hoursDirty && (
              <>
                <Button
                  variant="ghost"
                  size="sm"
                  className="h-8 rounded-full text-[11px] text-muted-foreground"
                  disabled={!canEdit}
                  onClick={() => setHoursEdit(null)}
                >
                  {t('settings.revert')}
                </Button>
                <Button
                  size="sm"
                  className="h-8 rounded-full text-[11px]"
                  disabled={!canEdit || !parsed.ok}
                  onClick={() => save({compensation_hours: parsed.hours}, 'hours')}
                >
                  {busy === 'hours' ? <Loader2 className="animate-spin" /> : <Save className="h-3.5 w-3.5" />}
                  {t('common.save')}
                </Button>
              </>
            )}
          </div>
        </div>
      </div>

      {!isAdmin && <p className="mt-2 text-[11px] text-muted-foreground">{t('settings.readonlyNote')}</p>}
    </section>
  );
}
