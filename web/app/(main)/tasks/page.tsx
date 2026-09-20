'use client';

import {useCallback, useEffect, useRef, useState} from 'react';
import {
  ClipboardList,
  Coins,
  Globe,
  ListChecks,
  Loader2,
  Play,
  ScanSearch,
  TriangleAlert,
} from 'lucide-react';
import {notify} from '@/lib/toast';
import {taskApi, errText} from '@/lib/api';
import type {
  GrowthTask,
  QueueItem,
  ScanAccountItem,
} from '@/lib/types';
import {fmtNumber} from '@/lib/format';
import {PageHeader} from '@/components/common/layout/PageHeader';
import {EmptyState} from '@/components/common/layout/EmptyState';
import {ConfirmDialog} from '@/components/common/layout/ConfirmDialog';
import {useAuth} from '@/lib/auth-context';
import {useRealm} from '@/lib/realm-context';
import {useT} from '@/lib/i18n/provider';
import {Button} from '@/components/ui/button';
import {Badge} from '@/components/ui/badge';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';

/** 任务状态徽章配色 */
function statusTone(t: GrowthTask): string {
  if (t.claimed) return 'text-muted-foreground';
  if (t.claimable) return 'text-emerald-600 dark:text-emerald-400';
  if (t.locked) return 'text-muted-foreground/60';
  if (t.target > 0 && t.current >= t.target) return 'text-emerald-600 dark:text-emerald-400';
  if (t.accept_status === 'accepted' || t.status === 'complete') return 'text-sky-600 dark:text-sky-400';
  return 'text-amber-600 dark:text-amber-400';
}

function statusLabel(t: GrowthTask): string {
  const k = 'tasks.status_' + (t.claimed
    ? 'claimed'
    : t.claimable
      ? 'claimable'
      : t.locked
        ? 'locked'
        : t.target > 0 && t.current >= t.target
          ? 'completed'
          : t.accept_status === 'accepted' || t.status === 'complete'
            ? 'inProgress'
            : 'pending');
  return k;
}

/** 队列单项的图标色 */
function queueTone(s: QueueItem['status']): string {
  switch (s) {
    case 'done': return 'text-emerald-600 dark:text-emerald-400';
    case 'running': return 'text-sky-600 dark:text-sky-400';
    case 'error': return 'text-red-600 dark:text-red-400';
    case 'skipped': return 'text-muted-foreground';
    default: return 'text-muted-foreground/60';
  }
}

export default function TasksPage() {
  const {isAdmin} = useAuth();
  const {realm, label: realmName} = useRealm();
  const t = useT();

  /** 全账号扫描结果（待办清单） */
  const [scan, setScan] = useState<ScanAccountItem[] | null>(null);
  const [pendingCount, setPendingCount] = useState(0);
  const [scanBusy, setScanBusy] = useState(false);
  const [selectedUid, setSelectedUid] = useState<string>('all');
  /** 选中账号的任务明细（仅单号视图时拉取） */
  const [accountTasks, setAccountTasks] = useState<GrowthTask[]>([]);
  const [accountTasksBusy, setAccountTasksBusy] = useState(false);

  /** 执行队列状态 */
  const [queue, setQueue] = useState<{running: boolean; total: number; conc: number; items: QueueItem[]} | null>(null);
  const [queueBusy, setQueueBusy] = useState(false);
  const [concurrency, setConcurrency] = useState('2');
  /** 一键自动完成的进度反馈（单号 auto_all 是长请求，busy 即转圈） */
  const [autoAllBusyUid, setAutoAllBusyUid] = useState<string | null>(null);
  const autoAllBusyRef = useRef(false);

  const loadScan = useCallback(async () => {
    setScanBusy(true);
    try {
      const r = await taskApi.scanAll();
      setScan(r.accounts ?? []);
      setPendingCount(r.pending_count ?? 0);
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setScanBusy(false);
    }
  }, []);

  const loadQueue = useCallback(async () => {
    try {
      const q = await taskApi.queue();
      setQueue({running: q.running, total: q.total, conc: q.conc, items: q.items ?? []});
      return q;
    } catch {
      return null; // 拉状态失败不打扰用户
    }
  }, []);

  useEffect(() => {
    loadScan();
    loadQueue();
  }, [loadScan, loadQueue]);

  // 队列执行中每 2 秒轮询，空闲时 30 秒一次（与心跳同频）
  useEffect(() => {
    let alive = true;
    const tick = async () => {
      if (document.hidden) return;
      const q = await loadQueue();
      if (!alive) return;
      timer = window.setTimeout(tick, q?.running ? 2000 : 30000);
    };
    let timer = window.setTimeout(tick, 3000);
    return () => {
      alive = false;
      window.clearTimeout(timer);
    };
  }, [loadQueue]);

  // 选中单号时拉该账号任务明细
  useEffect(() => {
    if (selectedUid === 'all' || !selectedUid) {
      setAccountTasks([]);
      return;
    }
    let alive = true;
    setAccountTasksBusy(true);
    taskApi.accountTasks(selectedUid)
      .then((r) => {
        if (alive) setAccountTasks(r.tasks ?? []);
      })
      .catch((e) => {
        if (alive) {
          setAccountTasks([]);
          notify.err(errText(e));
        }
      })
      .finally(() => {
        if (alive) setAccountTasksBusy(false);
      });
    return () => {
      alive = false;
    };
  }, [selectedUid]);

  /** 启动执行队列：把全部待办按账号分组排队（账号内串行、账号间并发） */
  async function startQueue() {
    setQueueBusy(true);
    try {
      const r = await taskApi.runQueue({concurrency: Number(concurrency) || 1});
      if (r.started) {
        notify.ok(t('tasks.queueStarted'), t('tasks.queueStartedDetail', {n: fmtNumber(r.total ?? 0)}));
      } else {
        notify.info(t('tasks.queueNothing'), r.message || t('tasks.queueNothingDetail'));
      }
      await loadQueue();
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setQueueBusy(false);
    }
  }

  /** 一键完成单账号全部可自动任务（同步流水线，最长 5 分钟） */
  async function autoAll(uid: string, nickname: string) {
    if (autoAllBusyRef.current) return;
    autoAllBusyRef.current = true;
    setAutoAllBusyUid(uid);
    try {
      const r = await taskApi.autoAll(uid);
      const results = r.results ?? [];
      const okN = results.filter((x) => x.status === 'done').length;
      notify.ok(
        t('tasks.autoAllDone', {name: nickname}),
        t('tasks.autoAllDoneDetail', {ok: okN, total: results.length, n: results.length}),
      );
    } catch (e) {
      notify.err(errText(e));
    } finally {
      autoAllBusyRef.current = false;
      setAutoAllBusyUid(null);
      loadScan();
      loadQueue();
    }
  }

  /** 领取单任务奖励 */
  async function claim(uid: string, code: string) {
    try {
      const r = await taskApi.claim(uid, code);
      if (r.already_claimed) {
        notify.info(t('tasks.claimAlready'));
      } else {
        notify.ok(
          t('tasks.claimDone'),
          `+${fmtNumber(r.credit ?? 0)} / +${fmtNumber(r.energy ?? 0)}`,
        );
      }
      if (selectedUid === uid) {
        const res = await taskApi.accountTasks(uid);
        setAccountTasks(res.tasks ?? []);
      }
      loadScan();
    } catch (e) {
      notify.err(errText(e));
    }
  }

  /** 一键完成单个任务 */
  async function auto(uid: string, code: string) {
    try {
      const r = await taskApi.auto(uid, code);
      if (r.skipped) {
        notify.info(t('tasks.autoSkipped'), r.message);
      } else {
        notify.ok(t('tasks.autoDone'), r.message || '');
      }
      if (selectedUid === uid) {
        const res = await taskApi.accountTasks(uid);
        setAccountTasks(res.tasks ?? []);
      }
      loadScan();
    } catch (e) {
      notify.err(errText(e));
    }
  }

  const isCN = (realm ?? 'cn') === 'cn';
  const scanOptions = (scan ?? []).filter((it) => it.growth?.length || it.school?.length);
  const filteredScan = selectedUid === 'all' ? scanOptions : scanOptions.filter((it) => it.uid === selectedUid);
  const hasSchoolPending = filteredScan.some((it) => it.school?.length);

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader
        title={t('tasks.title')}
        description={t('tasks.description', {realm: realmName})}
        actions={
          <>
            <Button
              variant="outline"
              size="sm"
              className="rounded-full"
              disabled={scanBusy || !isAdmin || !isCN}
              onClick={loadScan}
            >
              <ScanSearch className={scanBusy ? 'animate-pulse' : ''} />
              {t('tasks.scanAll')}
            </Button>
            {isAdmin && isCN && (
              <ConfirmDialog
                title={t('tasks.queueConfirmTitle')}
                description={t('tasks.queueConfirmDesc')}
                confirmText={t('tasks.queueStart')}
                onConfirm={startQueue}
                trigger={
                  <Button size="sm" className="rounded-full" disabled={queueBusy || queue?.running}>
                    {queueBusy || queue?.running ? (
                      <Loader2 className="animate-spin" />
                    ) : (
                      <Play />
                    )}
                    {t('tasks.queueStart')}
                  </Button>
                }
              />
            )}
          </>
        }
      />

      {/* 国际版没有成长任务体系：上游对 global 账号不发起任何任务调用 */}
      {!isCN && (
        <div className="flex items-start gap-2.5 rounded-[20px] border border-sky-500/30 bg-sky-500/10 p-4 text-xs">
          <Globe className="mt-0.5 h-4 w-4 shrink-0 text-sky-500" />
          <div className="space-y-1">
            <div className="font-medium">{t('tasks.globalNoTitle')}</div>
            <div className="text-muted-foreground">{t('tasks.globalNoDesc')}</div>
          </div>
        </div>
      )}

      {/* 执行队列状态 */}
      {queue && queue.items.length > 0 && (
        <section className="rounded-[20px] bg-muted p-4">
          <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
            <div className="flex items-center gap-2 text-sm font-medium">
              <ListChecks className="h-4 w-4" />
              {t('tasks.queueTitle')}
              {queue.running && (
                <Badge variant="secondary" className="rounded-full text-[10px]">
                  <Loader2 className="mr-1 size-3 animate-spin" />
                  {t('tasks.queueRunning', {n: queue.total, conc: queue.conc})}
                </Badge>
              )}
            </div>
            <Select value={concurrency} onValueChange={setConcurrency}>
              <SelectTrigger className="h-7 w-[110px] rounded-full text-[11px]">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {[1, 2, 3, 4].map((n) => (
                  <SelectItem key={n} value={String(n)} className="text-xs">
                    {t('tasks.concurrencyN', {n})}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="scroll-slim max-h-[280px] overflow-auto">
            <Table>
              <TableHeader>
                <TableRow className="border-b border-border/60 hover:bg-transparent">
                  <TableHead className="pl-2 text-[11px] text-muted-foreground">{t('tasks.colAccount')}</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">{t('tasks.colKind')}</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">{t('tasks.colCode')}</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">{t('tasks.colStatus')}</TableHead>
                  <TableHead className="pr-2 text-[11px] text-muted-foreground">{t('tasks.colResult')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {queue.items.map((it, i) => (
                  <TableRow key={`${it.uid}-${it.kind}-${it.code}-${i}`} className="border-b border-border/40">
                    <TableCell className="max-w-[140px] truncate pl-2 text-xs">{it.nickname || it.uid}</TableCell>
                    <TableCell className="text-xs text-muted-foreground">
                      {it.kind === 'school' ? t('tasks.kindSchool') : t('tasks.kindGrowth')}
                    </TableCell>
                    <TableCell className="font-mono text-[11px]">{it.code}</TableCell>
                    <TableCell className={'text-xs font-medium ' + queueTone(it.status)}>
                      {t(`tasks.queue_${it.status}`, {defaultValue: ''}) || it.status}
                    </TableCell>
                    <TableCell className="max-w-[280px] truncate pr-2 text-[11px] text-muted-foreground" title={it.message}>
                      {it.message || '—'}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        </section>
      )}

      {/* 扫描结果（待办清单） */}
      <section className="rounded-[20px] bg-muted p-4">
        <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
          <div className="flex items-center gap-2 text-sm font-medium">
            <ClipboardList className="h-4 w-4" />
            {t('tasks.scanTitle')}
            <Badge variant="secondary" className="rounded-full tabular-nums">
              {t('tasks.pendingCount', {n: fmtNumber(pendingCount)})}
            </Badge>
          </div>
          <Select value={selectedUid} onValueChange={setSelectedUid}>
            <SelectTrigger className="h-7 w-[180px] rounded-full text-[11px]">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all" className="text-xs">{t('common.all')}</SelectItem>
              {scanOptions.map((it) => (
                <SelectItem key={it.uid} value={it.uid} className="text-xs">
                  {it.nickname || it.uid}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>

        {filteredScan.length ? (
          <div className="space-y-3">
            {filteredScan.map((it) => (
              <div key={it.uid} className="rounded-2xl bg-background/60 p-3">
                <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
                  <div className="flex min-w-0 items-center gap-2">
                    <span className="truncate text-sm font-medium">{it.nickname || it.uid}</span>
                    {it.growth_error && (
                      <Badge variant="secondary" className="rounded-full text-red-600 dark:text-red-400">
                        {t('tasks.growthError')}
                      </Badge>
                    )}
                  </div>
                  {isAdmin && (
                    <div className="flex items-center gap-1.5">
                      <Button
                        variant="ghost"
                        size="sm"
                        className="h-7 rounded-full text-[11px]"
                        disabled={autoAllBusyUid !== null}
                        onClick={() => autoAll(it.uid, it.nickname || it.uid)}
                      >
                        {autoAllBusyUid === it.uid ? (
                          <Loader2 className="mr-1 size-3 animate-spin" />
                        ) : (
                          <Play className="mr-1 size-3" />
                        )}
                        {t('tasks.autoAll')}
                      </Button>
                      <Button
                        variant="ghost"
                        size="sm"
                        className="h-7 rounded-full text-[11px]"
                        onClick={() => {
                          setSelectedUid(it.uid);
                        }}
                      >
                        {t('tasks.viewDetail')}
                      </Button>
                    </div>
                  )}
                </div>

                {/* 成长任务待办 */}
                {!!it.growth?.length && (
                  <div className="flex flex-wrap gap-1.5">
                    {it.growth.map((g) => (
                      <Badge
                        key={g.task_code}
                        variant="secondary"
                        className={'rounded-md font-mono text-[10px] ' + statusTone(g)}
                        title={`${g.title || g.task_code} · ${t(statusLabel(g))}${g.credit ? ` · +${g.credit}` : ''}`}
                      >
                        {g.task_code}
                        {g.target > 0 && ` ${g.current}/${g.target}`}
                      </Badge>
                    ))}
                  </div>
                )}

                {/* 开学季待办 */}
                {!!it.school?.length && (
                  <div className="mt-2 flex flex-wrap items-center gap-1.5">
                    <span className="text-[10px] text-muted-foreground">{t('tasks.schoolPending')}</span>
                    {it.school.map((s) => (
                      <Badge key={s.task_code} variant="secondary" className="rounded-md font-mono text-[10px]">
                        {s.task_code} {s.progress}/{s.target_count}
                      </Badge>
                    ))}
                  </div>
                )}
              </div>
            ))}
          </div>
        ) : (
          <EmptyState
            icon={ScanSearch}
            title={t('tasks.scanEmptyTitle')}
            description={t('tasks.scanEmptyDesc')}
            className="flex flex-col items-center justify-center py-10 text-center"
          />
        )}
      </section>

      {/* 单号任务明细（选中账号时显示） */}
      {selectedUid !== 'all' && selectedUid && (
        <section className="rounded-[20px] bg-muted p-4">
          <div className="mb-3 flex items-center gap-2 text-sm font-medium">
            <Coins className="h-4 w-4" />
            {t('tasks.detailTitle')}
          </div>
          {accountTasksBusy ? (
            <div className="flex items-center justify-center gap-2 py-10 text-xs text-muted-foreground">
              <Loader2 className="h-4 w-4 animate-spin" />
              {t('common.loading')}
            </div>
          ) : accountTasks.length ? (
            <Table>
              <TableHeader>
                <TableRow className="border-b border-border/60 hover:bg-transparent">
                  <TableHead className="pl-2 text-[11px] text-muted-foreground">{t('tasks.colCode')}</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">{t('tasks.colProgress')}</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">{t('tasks.colStatus')}</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">{t('tasks.colReward')}</TableHead>
                  {isAdmin && <TableHead className="pr-2 text-right text-[11px] text-muted-foreground">{t('accounts.colActions')}</TableHead>}
                </TableRow>
              </TableHeader>
              <TableBody>
                {accountTasks.map((g) => (
                  <TableRow key={g.task_code} className="border-b border-border/40">
                    <TableCell className="pl-2">
                      <div className="text-xs font-medium">{g.title || g.task_code}</div>
                      <div className="font-mono text-[10px] text-muted-foreground">{g.task_code}</div>
                    </TableCell>
                    <TableCell className="text-xs tabular-nums">
                      {g.target > 0 ? `${g.current}/${g.target}` : '—'}
                    </TableCell>
                    <TableCell>
                      <Badge
                        variant="secondary"
                        className={'rounded-full text-[10px] ' + statusTone(g)}
                      >
                        {t(statusLabel(g))}
                      </Badge>
                    </TableCell>
                    <TableCell className="text-xs tabular-nums">
                      {g.credit ? <span className="text-emerald-600 dark:text-emerald-400">+{fmtNumber(g.credit)}</span> : '—'}
                      {g.energy ? <span className="ml-1 text-muted-foreground">+{fmtNumber(g.energy)}e</span> : null}
                    </TableCell>
                    {isAdmin && (
                      <TableCell className="pr-2 text-right">
                        <div className="flex justify-end gap-1">
                          {g.claimable && (
                            <Button
                              variant="ghost"
                              size="sm"
                              className="h-7 rounded-full text-[11px]"
                              onClick={() => claim(selectedUid, g.task_code)}
                            >
                              {t('tasks.claim')}
                            </Button>
                          )}
                          {!g.claimed && !g.locked && (
                            <Button
                              variant="ghost"
                              size="sm"
                              className="h-7 rounded-full text-[11px]"
                              title={t('tasks.autoHint')}
                              onClick={() => auto(selectedUid, g.task_code)}
                            >
                              {t('tasks.auto')}
                            </Button>
                          )}
                        </div>
                      </TableCell>
                    )}
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          ) : (
            <div className="py-10 text-center text-xs text-muted-foreground">
              <TriangleAlert className="mx-auto mb-2 h-4 w-4 text-amber-500" />
              {t('tasks.detailEmpty')}
            </div>
          )}
        </section>
      )}

      {/* 开学季入口提示（活动页有完整视图） */}
      {hasSchoolPending && (
        <div className="flex items-start gap-2.5 rounded-[20px] border border-amber-500/30 bg-amber-500/[0.07] px-3.5 py-3 text-xs">
          <TriangleAlert className="mt-0.5 h-4 w-4 shrink-0 text-amber-600 dark:text-amber-400" />
          <span className="text-[11px] leading-5 text-muted-foreground">{t('tasks.schoolSeeActivity')}</span>
        </div>
      )}
    </div>
  );
}
