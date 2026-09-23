'use client';

import {useCallback, useState} from 'react';
import {
  GraduationCap,
  Loader2,
  Play,
  RefreshCw,
  Ticket,
  Gift,
} from 'lucide-react';
import {useHeartbeat} from '@/lib/use-heartbeat';
import {notify} from '@/lib/toast';
import {schoolApi, errText} from '@/lib/api';
import {useCachedAsync} from '@/lib/data-cache';
import type {SchoolStatusResponse, SchoolTaskView, VoucherRow} from '@/lib/types';
import {fmtDateTime, fmtNumber} from '@/lib/format';
import {PageHeader} from '@/components/common/layout/PageHeader';
import {EmptyState} from '@/components/common/layout/EmptyState';
import {CardRowsSkeleton} from '@/components/common/layout/LoadSkeleton';
import {ConfirmDialog} from '@/components/common/layout/ConfirmDialog';
import {CopyButton} from '@/components/ui/copy-button';
import {useRealm} from '@/lib/realm-context';
import {useAuth} from '@/lib/auth-context';
import {useT} from '@/lib/i18n/provider';
import {Button} from '@/components/ui/button';
import {Badge} from '@/components/ui/badge';
import {QRCodeSVG} from 'qrcode.react';
import {
  Drawer,
  DrawerContent,
  DrawerDescription,
  DrawerHeader,
  DrawerTitle,
} from '@/components/ui/drawer';

/** 开学季任务码 → 显示名（缺省回退任务码本体） */
const TASK_LABEL_KEYS: Record<string, string> = {
  daily_task_1: 'activity.taskDaily1',
  daily_task_2: 'activity.taskDaily2',
  desktop_chat_1_time: 'activity.taskDesktopChat',
  school_season: 'activity.taskSchoolSeason',
  sequential_tasks_1: 'activity.taskSequential',
  task_student_verify: 'activity.taskStudentVerify',
};

function taskLabel(code: string, t: (k: string) => string): string {
  const key = TASK_LABEL_KEYS[code];
  return key ? t(key) : code;
}

/** 任务状态徽章 */
function TaskBadge({task}: {task: SchoolTaskView}) {
  const t = useT();
  const done = task.status === 'claimed' || (task.target_count > 0 && task.progress >= task.target_count);
  const tone = task.status === 'claimed'
    ? 'text-muted-foreground'
    : done
      ? 'text-emerald-600 dark:text-emerald-400'
      : task.progress > 0
        ? 'text-sky-600 dark:text-sky-400'
        : 'text-amber-600 dark:text-amber-400';
  return (
    <Badge
      variant="secondary"
      className={'rounded-md font-mono text-[10px] ' + tone}
      title={`${task.task_code} · ${task.status}`}
    >
      {taskLabel(task.task_code, t)}
      {task.target_count > 0 && ` ${task.progress}/${task.target_count}`}
    </Badge>
  );
}

export default function ActivityPage() {
  const t = useT();
  const {isAdmin} = useAuth();
  const {realm} = useRealm();
  // 开学季状态逐账号查上游（秒级），接缓存：切页先出上次的矩阵，后台静默刷新
  const statusCache = useCachedAsync<SchoolStatusResponse>(
    'activity:status',
    async () => {
      const r = await schoolApi.status();
      // 后端对 global 账号、任务查询出错的账号返回的 tasks 是 null（JSON 序列化成 null，
      // 类型声明上仍是数组），先归一化成空数组，避免渲染层 a.tasks.every 抛 TypeError 崩页
      return {...r, accounts: (r.accounts ?? []).map((a) => ({...a, tasks: a.tasks ?? [], chances: a.chances ?? 0}))};
    },
    {ttl: 10_000},
  );
  const accounts = statusCache.data?.accounts ?? [];
  const loading = statusCache.loading;
  const load = statusCache.refresh;
  const [runBusy, setRunBusy] = useState(false);
  /** 券码抽屉：当前查看的账号 uid */
  const [voucherUid, setVoucherUid] = useState<string | null>(null);
  const [vouchers, setVouchers] = useState<VoucherRow[]>([]);
  const [vouchersBusy, setVouchersBusy] = useState(false);
  /** 抽奖余额合计（渲染期派生，数据刷新后自动跟随） */
  const totalChances = accounts.reduce((sum, a) => sum + (a.chances ?? 0), 0);
  const doneCount = accounts.filter((a) =>
    a.tasks.every((x) => x.status === 'claimed' || (x.target_count > 0 && x.progress >= x.target_count)),
  ).length;

  // 心跳是后台自愈型刷新：失败时静默等下一轮，不要 60s 一轮弹错误雨
  // （stats 页与 logs 页同口径）。用户手动触发的「刷新」在按钮路径单独报错。
  useHeartbeat(
    () => {
      statusCache.refresh().catch(() => {/* 下一轮自愈 */});
    },
    60000,
  );

  /** 一键执行全部账号开学季闭环（异步，进度看任务频道日志） */
  const runAll = useCallback(async () => {
    setRunBusy(true);
    try {
      await schoolApi.runAll();
      notify.ok(t('activity.runAllStarted'), t('activity.runAllStartedDetail'));
      // 闭环是异步任务，状态不会立刻变化：强制刷新缓存（绕过 TTL）拉最新矩阵
      statusCache.refresh().catch(() => {/* 心跳稍后会再试 */});
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setRunBusy(false);
    }
  }, [statusCache, t]);

  /** 打开券码抽屉时拉一次券码列表 */
  const openVouchers = useCallback(async () => {
    setVoucherUid('');
    setVouchersBusy(true);
    try {
      const r = await schoolApi.vouchers();
      setVouchers(r.accounts ?? []);
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setVouchersBusy(false);
    }
  }, []);

  const isCN = (realm ?? 'cn') === 'cn';
  const currentVouchers = vouchers;

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader
        title={t('activity.title')}
        description={t('activity.description')}
        actions={
          <>
            <Button
              variant="outline"
              size="sm"
              className="rounded-full"
              onClick={openVouchers}
            >
              <Ticket />
              {t('activity.vouchers')}
            </Button>
            <Button
              variant="outline"
              size="sm"
              className="rounded-full"
              disabled={loading}
              onClick={load}
            >
              <RefreshCw className={loading ? 'animate-spin' : ''} />
              {t('common.refresh')}
            </Button>
            {isAdmin && isCN && (
              <ConfirmDialog
                title={t('activity.runAllConfirmTitle')}
                description={t('activity.runAllConfirmDesc')}
                confirmText={t('activity.runAll')}
                onConfirm={runAll}
                trigger={
                  <Button size="sm" className="rounded-full" disabled={runBusy}>
                    {runBusy ? <Loader2 className="animate-spin" /> : <Play />}
                    {t('activity.runAll')}
                  </Button>
                }
              />
            )}
          </>
        }
      />

      {/* 国际版没有开学季活动：Go 侧对 global 账号不发起任何上游调用 */}
      {!isCN && (
        <div className="flex items-start gap-2.5 rounded-[20px] border border-sky-500/30 bg-sky-500/10 p-4 text-xs">
          <GraduationCap className="mt-0.5 h-4 w-4 shrink-0 text-sky-500" />
          <div className="space-y-1">
            <div className="font-medium">{t('activity.globalNoTitle')}</div>
            <div className="text-muted-foreground">{t('activity.globalNoDesc')}</div>
          </div>
        </div>
      )}

      {/* 汇总卡 */}
      {isCN && !!accounts.length && (
        <section className="grid grid-cols-2 gap-3 lg:grid-cols-3 md:gap-4">
          <div className="rounded-[20px] bg-muted px-3.5 py-3">
            <div className="flex items-center gap-1.5 text-[11px] text-muted-foreground">
              <GraduationCap className="h-3.5 w-3.5" />
              {t('activity.accountsTotal')}
            </div>
            <div className="mt-1.5 text-xl font-semibold tabular-nums">{fmtNumber(accounts.length)}</div>
          </div>
          <div className="rounded-[20px] bg-muted px-3.5 py-3">
            <div className="flex items-center gap-1.5 text-[11px] text-muted-foreground">
              <Gift className="h-3.5 w-3.5" />
              {t('activity.doneAccounts')}
            </div>
            <div className="mt-1.5 text-xl font-semibold tabular-nums">{fmtNumber(doneCount)}</div>
          </div>
          <div className="rounded-[20px] bg-muted px-3.5 py-3">
            <div className="flex items-center gap-1.5 text-[11px] text-muted-foreground">
              <Ticket className="h-3.5 w-3.5" />
              {t('activity.chances')}
            </div>
            <div className="mt-1.5 text-xl font-semibold tabular-nums">{fmtNumber(totalChances)}</div>
          </div>
        </section>
      )}

      {/* 状态矩阵 */}
      <section className="overflow-hidden rounded-[20px] bg-muted">
        {loading && !accounts.length ? (
          <CardRowsSkeleton rows={4} />
        ) : accounts.length ? (
          <div className="divide-y divide-border/40">
            {accounts.map((a) => (
              <div key={a.uid} className="space-y-2 px-4 py-3">
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <div className="flex min-w-0 items-center gap-2">
                    <div className="grid h-7 w-7 shrink-0 place-items-center rounded-full bg-primary text-[11px] font-semibold text-primary-foreground">
                      {(a.nickname || '?').charAt(0)}
                    </div>
                    <div className="min-w-0">
                      <div className="truncate text-sm font-medium">{a.nickname || a.uid}</div>
                      <div className="truncate font-mono text-[10px] text-muted-foreground">{a.uid}</div>
                    </div>
                    {!a.in_period && (
                      <Badge variant="secondary" className="rounded-full text-muted-foreground">
                        {t('activity.notInPeriod')}
                      </Badge>
                    )}
                    {a.error && (
                      <Badge variant="secondary" className="max-w-[240px] truncate rounded-full text-red-600 dark:text-red-400" title={a.error}>
                        {a.error}
                      </Badge>
                    )}
                  </div>
                  <div className="flex shrink-0 items-center gap-1.5">
                    {(a.chances ?? 0) > 0 && (
                      <Badge variant="secondary" className="rounded-full text-amber-600 dark:text-amber-400">
                        <Ticket className="mr-1 h-3 w-3" />
                        {t('activity.chancesN', {n: fmtNumber(a.chances)})}
                      </Badge>
                    )}
                  </div>
                </div>

                {/* 5 任务状态矩阵 */}
                {a.tasks.length ? (
                  <div className="flex flex-wrap gap-1.5">
                    {a.tasks.map((task) => (
                      <TaskBadge key={task.task_code} task={task} />
                    ))}
                  </div>
                ) : !a.error ? (
                  <span className="text-[11px] text-muted-foreground">{t('activity.noTasks')}</span>
                ) : null}
              </div>
            ))}
          </div>
        ) : (
          <EmptyState
            icon={GraduationCap}
            title={t('activity.emptyTitle')}
            description={t('activity.emptyDesc')}
            className="flex flex-col items-center justify-center py-16 text-center"
          />
        )}
      </section>

      {/* 券码抽屉：逐账号券码 + 二维码（复制给店员核销） */}
      <Drawer open={voucherUid !== null} onOpenChange={(v) => !v && setVoucherUid(null)}>
        <DrawerContent>
          <DrawerHeader>
            <DrawerTitle>{t('activity.voucherTitle')}</DrawerTitle>
            <DrawerDescription>{t('activity.voucherDesc')}</DrawerDescription>
          </DrawerHeader>
          <div className="max-h-[60vh] overflow-auto px-4 pb-8">
            {vouchersBusy ? (
              <div className="flex items-center justify-center gap-2 py-10 text-xs text-muted-foreground">
                <Loader2 className="h-4 w-4 animate-spin" />
                {t('common.loading')}
              </div>
            ) : currentVouchers.length ? (
              <div className="space-y-4">
                {currentVouchers.map((row) => (
                  <div key={row.uid} className="rounded-2xl bg-muted p-3">
                    <div className="mb-2 flex items-center justify-between gap-2">
                      <span className="truncate text-sm font-medium">{row.nickname || row.uid}</span>
                      {row.error && (
                        <span className="shrink-0 text-[10px] text-red-600 dark:text-red-400">{row.error}</span>
                      )}
                    </div>
                    {row.vouchers?.length ? (
                      <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
                        {row.vouchers.map((v) => (
                          <div key={v.grant_id} className="rounded-xl bg-background/60 p-3">
                            <div className="flex items-start justify-between gap-2">
                              <div className="min-w-0">
                                <div className="truncate text-xs font-medium">{v.prize_name || v.sku_code || '—'}</div>
                                <div className="mt-0.5 break-all font-mono text-[11px] text-muted-foreground" title={v.code}>
                                  {v.code}
                                </div>
                                {v.valid_to && (
                                  <div className="mt-0.5 text-[10px] text-muted-foreground/70">
                                    {t('activity.validTo', {date: v.valid_to})}
                                  </div>
                                )}
                              </div>
                              <div className="shrink-0 rounded-lg bg-white p-1.5 ring-1 ring-black/5">
                                <QRCodeSVG value={v.code} size={72} level="M" />
                              </div>
                            </div>
                            <div className="mt-2 flex items-center justify-between gap-2">
                              <span className="text-[10px] text-muted-foreground">
                                {v.granted_at ? fmtDateTime(v.granted_at) : ''}
                              </span>
                              <CopyButton value={v.code} title={t('activity.copyVoucher')} className="h-6 w-6" />
                            </div>
                          </div>
                        ))}
                      </div>
                    ) : (
                      <div className="py-3 text-center text-[11px] text-muted-foreground">
                        {t('activity.noVouchers')}
                      </div>
                    )}
                  </div>
                ))}
              </div>
            ) : (
              <div className="py-10 text-center text-xs text-muted-foreground">
                {t('activity.noVouchers')}
              </div>
            )}
          </div>
        </DrawerContent>
      </Drawer>
    </div>
  );
}
