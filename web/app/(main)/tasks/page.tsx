'use client';

import {useCallback, useEffect, useMemo, useRef, useState} from 'react';
import {
  ClipboardList,
  Coins,
  Globe,
  History,
  Info,
  ListChecks,
  Loader2,
  Play,
  RefreshCw,
  ScanSearch,
  TriangleAlert,
  XCircle,
} from 'lucide-react';
import {notify} from '@/lib/toast';
import {taskApi, accountApi, errText, httpStatus} from '@/lib/api';
import {useCachedAsync} from '@/lib/data-cache';
import type {
  GrowthTask,
  OverviewResponse,
  QueueItem,
  ScanAllResponse,
  TaskRecordsResponse,
} from '@/lib/types';
import {fmtDateTimeMarked, fmtNumber} from '@/lib/format';
import {PageHeader} from '@/components/common/layout/PageHeader';
import {EmptyState} from '@/components/common/layout/EmptyState';
import {TableSkeleton} from '@/components/common/layout/LoadSkeleton';
import {ConfirmDialog} from '@/components/common/layout/ConfirmDialog';
import {useAuth} from '@/lib/auth-context';
import {useRealm} from '@/lib/realm-context';
import {useI18n, useT, type TFn} from '@/lib/i18n/provider';
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
    case 'cancelled': return 'text-muted-foreground/60';
    default: return 'text-muted-foreground/60';
  }
}

/** 积分流水展示条数上限（records 已按时间倒序返回，截取前 N 条即可） */
const RECORDS_SHOWN = 50;

/**
 * 队列状态文案。cancelled 的 locales 键（tasks.queue_cancelled）尚未添加
 * （本任务禁止改 locales，且 translate 不支持 defaultValue，缺键会直接回显
 * 键名）——先按当前语言本地映射，locales 补键后收敛回 t()。
 */
function queueStatusLabel(s: string, t: TFn, locale: string): string {
  if (s === 'cancelled') {
    if (locale === 'zh-CN' || locale === 'zh-TW') return '已取消';
    if (locale === 'ja') return 'キャンセル済み';
    if (locale === 'ko') return '취소됨';
    return 'Cancelled';
  }
  return t(`tasks.queue_${s}`) || s;
}

export default function TasksPage() {
  const {isAdmin} = useAuth();
  const {realm, label: realmName} = useRealm();
  const t = useT();
  const {locale} = useI18n();

  /** 全账号扫描结果（待办清单）。
   *  scan_all 要逐账号向上游扫任务（1-2 秒起），接缓存：切页先出上次的
   *  扫描结果，后台静默刷新；手动「扫描」按钮仍即时触发。 */
  const scanCache = useCachedAsync<ScanAllResponse>(
    'tasks:scan',
    () => taskApi.scanAll(),
    {ttl: 10_000},
  );
  const scan = scanCache.data?.accounts ?? null;
  const pendingCount = scanCache.data?.pending_count ?? 0;
  const scanBusy = scanCache.loading || scanCache.refreshing;
  const [selectedUid, setSelectedUid] = useState<string>('all');
  /** 选中账号的任务明细（仅单号视图时拉取） */
  const [accountTasks, setAccountTasks] = useState<GrowthTask[]>([]);
  const [accountTasksBusy, setAccountTasksBusy] = useState(false);

  /** 执行队列状态 */
  const [queue, setQueue] = useState<{running: boolean; total: number; conc: number; items: QueueItem[]} | null>(null);
  const [queueBusy, setQueueBusy] = useState(false);
  const [concurrency, setConcurrency] = useState('2');
  /** 取消队列请求进行中（按钮禁用 + 转圈） */
  const [cancelBusy, setCancelBusy] = useState(false);
  /** 一键自动完成的进度反馈（单号 auto_all 是长请求，busy 即转圈） */
  const [autoAllBusyUid, setAutoAllBusyUid] = useState<string | null>(null);
  const autoAllBusyRef = useRef(false);

  /* ── 积分流水 ─────────────────────────────────────────────
   * records 本体走缓存（切页先出旧数据）；501「未启用」是**部署形态**而非
   * 错误——fetcher 里就地识别转成 unavailable 标记，页面显示常驻提示条，
   * 不弹 toast 错误。uid 筛选变化 = 换一份快照（换 key 重新拉取）。 */
  const [recordsUid, setRecordsUid] = useState<string>('all');
  const [recordsUnavailable, setRecordsUnavailable] = useState(false);
  const recordsCache = useCachedAsync<TaskRecordsResponse>(
    `tasks:records:${recordsUid}`,
    () =>
      taskApi.records(recordsUid === 'all' ? undefined : recordsUid).catch((e) => {
        if (httpStatus(e) === 501) {
          // 未启用：置标记 + 回落空响应（不进错误提示路径）
          setRecordsUnavailable(true);
          return {ok: true, total: 0, records: []} as TaskRecordsResponse;
        }
        throw e;
      }),
    {ttl: 10_000},
  );
  const records = recordsCache.data?.records ?? null;
  const recordsBusy = recordsCache.loading || recordsCache.refreshing;

  /** 积分流水筛选下拉的账号名单：扫一份池总览（有缓存，几乎零开销）。
   *  与待办扫描的名单不同源——流水覆盖全部账号，不只扫出待办的那些。 */
  const overviewCache = useCachedAsync<OverviewResponse>(
    'overview',
    () => accountApi.overview(),
    {ttl: 10_000},
  );
  // useMemo 稳定引用：把裸数组喂给 useCallback 依赖会让 lint 警告每次渲染都在变
  const overviewAccounts = useMemo(
    () => overviewCache.data?.accounts ?? [],
    [overviewCache.data],
  );
  /** uid → 昵称（流水行渲染用；records.message 已含昵称，这里只做筛选下拉） */
  const nicknameOf = useCallback(
    (uid: string) => overviewAccounts.find((a) => a.uid === uid)?.nickname || uid,
    [overviewAccounts],
  );

  const loadRecords = useCallback(async () => {
    setRecordsUnavailable(false); // 重新评估：后端可能已启用
    try {
      await recordsCache.refresh();
    } catch (e) {
      notify.err(errText(e));
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [recordsCache.refresh]);

  const loadScan = useCallback(async () => {
    try {
      await scanCache.refresh();
    } catch (e) {
      notify.err(errText(e));
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [scanCache.refresh]);

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
    loadRecords();
  }, [loadScan, loadQueue, loadRecords]);

  // 队列执行中每 2 秒轮询，空闲时 30 秒一次（与心跳同频）。
  // tick 末尾**无条件**安排下一次：hidden 只跳过本轮请求——若在 return 前
  // 就不重排，定时器会随一次切标签页永久死亡，切回来后再无轮询。
  useEffect(() => {
    let alive = true;
    const tick = async () => {
      if (!alive) return;
      let running = false;
      if (!document.hidden) {
        const q = await loadQueue();
        running = !!q?.running;
      }
      if (!alive) return;
      timer = window.setTimeout(tick, running ? 2000 : 30000);
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

  /** 取消执行队列：剩余待办停止调度，进行中的条目自然完成后停止 */
  async function cancelQueue() {
    setCancelBusy(true);
    try {
      await taskApi.cancelQueue();
      notify.ok(t('tasks.queueCancelledTitle'), t('tasks.queueCancelledDetail'));
      await loadQueue();
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setCancelBusy(false);
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
  const scanOptions = (scan ?? []).filter((it) => it.growth?.length);
  const filteredScan = selectedUid === 'all' ? scanOptions : scanOptions.filter((it) => it.uid === selectedUid);

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
            <div className="flex items-center gap-2">
              {isAdmin && queue.running && (
                <ConfirmDialog
                  title={t('tasks.queueCancelConfirmTitle')}
                  description={t('tasks.queueCancelConfirmDesc')}
                  confirmText={t('tasks.queueCancel')}
                  onConfirm={cancelQueue}
                  trigger={
                    <Button
                      variant="outline"
                      size="sm"
                      className="h-7 rounded-full text-[11px]"
                      disabled={cancelBusy}
                    >
                      {cancelBusy ? (
                        <Loader2 className="animate-spin" />
                      ) : (
                        <XCircle />
                      )}
                      {cancelBusy ? t('tasks.queueCancelling') : t('tasks.queueCancel')}
                    </Button>
                  }
                />
              )}
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
                    <TableCell className="text-xs text-muted-foreground">{t('tasks.kindGrowth')}</TableCell>
                    <TableCell className="font-mono text-[11px]">{it.code}</TableCell>
                    <TableCell className={'text-xs font-medium ' + queueTone(it.status)}>
                      {queueStatusLabel(it.status, t, locale)}
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
                          // 再点一次同一账号视为收起：回到全部视图（与下拉选择器的行为互补）
                          setSelectedUid((prev) => (prev === it.uid ? 'all' : it.uid));
                        }}
                      >
                        {selectedUid === it.uid ? t('tasks.collapseDetail') : t('tasks.viewDetail')}
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
            <TableSkeleton rows={4} />
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

      {/* 积分流水（签到/任务入账与余额跳变；后端已按时间倒序，截取展示） */}
      <section className="rounded-[20px] bg-muted p-4">
        <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
          <div className="flex items-center gap-2 text-sm font-medium">
            <History className="h-4 w-4" />
            {t('tasks.recordsTitle')}
            {records && !recordsUnavailable && (
              <Badge variant="secondary" className="rounded-full tabular-nums">
                {t('tasks.recordsCount', {n: fmtNumber(Math.min(records.length, RECORDS_SHOWN))})}
              </Badge>
            )}
          </div>
          <div className="flex items-center gap-1.5">
            <Select
              value={recordsUid}
              onValueChange={(v) => {
                setRecordsUid(v);
                setRecordsUnavailable(false); // 新 key 重新评估「未启用」
              }}
            >
              <SelectTrigger className="h-7 w-[180px] rounded-full text-[11px]">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all" className="text-xs">{t('common.all')}</SelectItem>
                {overviewAccounts.map((a) => (
                  <SelectItem key={a.uid} value={a.uid} className="text-xs">
                    {a.nickname || a.uid}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Button
              variant="ghost"
              size="sm"
              className="h-7 rounded-full text-[11px]"
              disabled={recordsBusy || recordsUnavailable}
              onClick={loadRecords}
              title={t('tasks.recordsRefreshTitle')}
            >
              <RefreshCw className={recordsBusy ? 'animate-spin' : ''} />
              {t('common.refresh')}
            </Button>
          </div>
        </div>

        {recordsUnavailable ? (
          /* 未启用态：提示而非报错（积分记录整体关闭的部署形态） */
          <div className="flex items-start gap-2.5 rounded-2xl bg-background/60 p-4 text-xs">
            <Info className="mt-0.5 h-4 w-4 shrink-0 text-muted-foreground" />
            <div className="space-y-1">
              <div className="font-medium">{t('tasks.recordsUnavailableTitle')}</div>
              <div className="text-muted-foreground">{t('tasks.recordsUnavailableDesc')}</div>
            </div>
          </div>
        ) : recordsBusy && !records ? (
          <TableSkeleton rows={4} />
        ) : records && records.length ? (
          <div className="scroll-slim max-h-[360px] overflow-auto">
            <Table>
              <TableHeader>
                <TableRow className="border-b border-border/60 hover:bg-transparent">
                  <TableHead className="pl-2 text-[11px] text-muted-foreground">{t('tasks.colTime')}</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">{t('tasks.colAccount')}</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">{t('tasks.colChange')}</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">{t('tasks.colBalance')}</TableHead>
                  <TableHead className="pr-2 text-[11px] text-muted-foreground">{t('tasks.colMessage')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {records.slice(0, RECORDS_SHOWN).map((r) => (
                  <TableRow
                    key={r.dedup_key || `${r.uid}-${r.ts}-${r.prev}-${r.new}`}
                    className={'border-b border-border/40' + (r.jump ? ' bg-amber-500/5' : '')}
                  >
                    <TableCell className="whitespace-nowrap pl-2 text-[11px] text-muted-foreground">
                      {fmtDateTimeMarked(r.ts)}
                    </TableCell>
                    <TableCell className="max-w-[140px] truncate text-xs">{nicknameOf(r.uid)}</TableCell>
                    <TableCell>
                      {r.jump ? (
                        /* 跳变记录：delta 恒 0，语义是「余额跳变 A → B」，不渲染 +N 徽章 */
                        <Badge
                          variant="secondary"
                          className="rounded-full text-[10px] text-amber-600 dark:text-amber-400"
                          title={t('tasks.recordsJumpTitle')}
                        >
                          {t('tasks.recordsJump', {prev: fmtNumber(r.prev), next: fmtNumber(r.new)})}
                        </Badge>
                      ) : (
                        <span className="inline-flex items-center gap-1">
                          {/* 签到到账（签到前后精确差值）带来源徽章；普通差额记录无徽章 */}
                          {r.source === 'checkin' && (
                            <Badge
                              variant="secondary"
                              className="rounded-full px-1.5 text-[10px] text-orange-600 dark:text-orange-400"
                              title={t('tasks.recordsCheckinTitle')}
                            >
                              {t('tasks.recordsCheckin')}
                            </Badge>
                          )}
                          <Badge
                            variant="secondary"
                            className={
                              'rounded-full text-[10px] tabular-nums ' +
                              (r.delta > 0
                                ? 'text-emerald-600 dark:text-emerald-400'
                                : r.delta < 0
                                  ? 'text-red-600 dark:text-red-400'
                                  : 'text-muted-foreground')
                            }
                          >
                            {r.delta > 0 ? `+${fmtNumber(r.delta)}` : fmtNumber(r.delta)}
                          </Badge>
                        </span>
                      )}
                    </TableCell>
                    <TableCell className="whitespace-nowrap text-xs tabular-nums text-muted-foreground">
                      {fmtNumber(r.prev)} <span className="text-muted-foreground/40">→</span>{' '}
                      <span className="font-medium text-foreground">{fmtNumber(r.new)}</span>
                    </TableCell>
                    <TableCell className="max-w-[320px] truncate pr-2 text-[11px] text-muted-foreground" title={r.message}>
                      {r.message || '—'}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
            {records.length > RECORDS_SHOWN && (
              <div className="pt-2 text-center text-[10px] text-muted-foreground">
                {t('tasks.recordsShownLimit', {shown: RECORDS_SHOWN, total: fmtNumber(records.length)})}
              </div>
            )}
          </div>
        ) : (
          <EmptyState
            icon={History}
            title={t('tasks.recordsEmptyTitle')}
            description={t('tasks.recordsEmptyDesc')}
            className="flex flex-col items-center justify-center py-10 text-center"
          />
        )}
      </section>
    </div>
  );
}
