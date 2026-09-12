'use client';

import {useCallback, useEffect, useMemo, useState} from 'react';
import {
  Gift,
  Zap,
  KeyRound,
  Trash2,
  RefreshCw,
  Plus,
  Users,
  Power,
  History,
  CalendarCheck,
  TriangleAlert,
  Coins,
  Cat,
  Filter,
} from 'lucide-react';
import {notify} from '@/lib/toast';
import {accountApi, upstreamApi, errText} from '@/lib/api';
import type {Account, CheckinLog, CreditsMeta, TaskLog, TaskLogResponse, UpstreamStatus} from '@/lib/types';
import {expiryVisual, fmtDateTime, fmtNumber, fmtRemain} from '@/lib/format';
import {PageHeader} from '@/components/common/layout/PageHeader';
import {EmptyState} from '@/components/common/layout/EmptyState';
import {ConfirmDialog} from '@/components/common/layout/ConfirmDialog';
import {AddAccountDialog} from '@/components/common/accounts/AddAccountDialog';
import {useAuth} from '@/lib/auth-context';
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

const TOKEN_TTL = 72 * 3600;

export default function AccountsPage() {
  const {isAdmin} = useAuth();
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [upstream, setUpstream] = useState<UpstreamStatus | null>(null);
  const [loading, setLoading] = useState(true);
  const [addOpen, setAddOpen] = useState(false);
  const [busyFile, setBusyFile] = useState<string | null>(null);
  const [checkinLogs, setCheckinLogs] = useState<CheckinLog[]>([]);
  const [checkinAllBusy, setCheckinAllBusy] = useState(false);
  const [upstreamLines, setUpstreamLines] = useState<string[]>([]);
  /** 上游自动任务留痕（旅行/活跃/签到/保活）+ 积分收益统计 */
  const [taskLogs, setTaskLogs] = useState<TaskLog[]>([]);
  const [taskStats, setTaskStats] = useState<TaskLogResponse['stats'] | null>(null);
  const [kindLabels, setKindLabels] = useState<Record<string, string>>({});
  const [taskFilter, setTaskFilter] = useState<string>('all');
  /** 每个账号积分是实时查询还是命中缓存（含缓存已存在秒数） */
  const [creditsMeta, setCreditsMeta] = useState<Record<string, CreditsMeta>>({});
  /**
   * 查到的积分按 uid 单独存一份，渲染时再叠加到账号上。
   * 不能直接改写 accounts：积分请求与账号列表是并发的，
   * 积分常常先返回，那时 accounts 还是空的，就地改写会落空。
   */
  const [liveCredits, setLiveCredits] = useState<Record<string, number>>({});
  const [collectBusy, setCollectBusy] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    const [accRes, upRes, logRes, ulRes, taskRes] = await Promise.allSettled([
      accountApi.list(),
      upstreamApi.status(),
      accountApi.checkinLogs(100),
      accountApi.upstreamLogs(200),
      accountApi.taskLogs(300),
    ]);
    if (accRes.status === 'fulfilled') setAccounts(accRes.value.accounts);
    else notify.err(errText(accRes.reason));
    if (upRes.status === 'fulfilled') setUpstream(upRes.value);
    if (logRes.status === 'fulfilled') setCheckinLogs(logRes.value);
    if (ulRes.status === 'fulfilled') setUpstreamLines(ulRes.value.lines);
    if (taskRes.status === 'fulfilled') {
      setTaskLogs(taskRes.value.logs);
      setTaskStats(taskRes.value.stats);
      setKindLabels(taskRes.value.kinds);
    }
    setLoading(false);
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  // 打开页面时自动拉一次实时积分：上游 /status 的 credits 可能滞后数小时，
  // 首次进入应展示真实余额。服务端有 TTL 缓存，重复进入不会频繁请求。
  useEffect(() => {
    let alive = true;
    (async () => {
      try {
        // force=false：60 秒内重复打开页面直接命中服务端缓存，
        // 不再每次都全量请求腾讯；命中时界面会明确标注「缓存」
        const r = await accountApi.refreshCredits(false);
        if (!alive) return;
        setLiveCredits(
          Object.fromEntries(
            Object.entries(r.credits).filter(([, v]) => typeof v === 'number') as [string, number][],
          ),
        );
        setCreditsMeta(r.meta ?? {});
      } catch {
        /* 静默失败：仍显示上游缓存值 */
      }
    })();
    return () => {
      alive = false;
    };
  }, []);

  // 上游状态（冷却 / 成功计数等）会随时间变化，页面停留时定时刷新，
  // 否则会一直显示打开页面那一刻的旧数据。
  const REFRESH_MS = 30000;
  useEffect(() => {
    const timer = window.setInterval(() => {
      void load();
    }, REFRESH_MS);
    return () => window.clearInterval(timer);
  }, [load]);

  /** 立即采集一次上游任务日志（不等后台 45 秒轮询） */
  const collectTasks = useCallback(async () => {
    setCollectBusy(true);
    try {
      const r = await accountApi.collectTaskLogs();
      await load();
      if (r.added > 0) notify.ok('已采集新记录', `新增 ${r.added} 条任务日志`);
      else notify.info('暂无新记录', '上游还没有产生新的自动任务日志');
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setCollectBusy(false);
    }
  }, [load]);

  /** 刷新所有账号的实时积分（直接向腾讯查询，非上游缓存值） */
  const [creditsBusy, setCreditsBusy] = useState(false);
  const refreshCredits = useCallback(async () => {
    setCreditsBusy(true);
    try {
      const r = await accountApi.refreshCredits(true);
      setLiveCredits(
        Object.fromEntries(
          Object.entries(r.credits).filter(([, v]) => typeof v === 'number') as [string, number][],
        ),
      );
      setCreditsMeta(r.meta ?? {});
      if (r.failed.length === 0) {
        notify.ok('积分已刷新', `${r.succeeded}/${r.total} 个账号`);
      } else {
        notify.warn('部分账号积分未取到', `${r.succeeded}/${r.total} 成功，其余见账号状态`);
      }
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setCreditsBusy(false);
    }
  }, []);

  /** 批量签到：逐账号记录结果 */
  const checkinAll = useCallback(async () => {
    setCheckinAllBusy(true);
    try {
      const r = await accountApi.checkinAll();
      const failed = r.total - r.succeeded;
      if (r.total === 0) {
        notify.info('没有可签到的账号');
      } else if (failed === 0) {
        notify.ok(`全部签到完成`, `${r.succeeded}/${r.total} 个账号成功`);
      } else {
        notify.warn(`签到完成，${failed} 个失败`, `${r.succeeded}/${r.total} 个账号成功，详见下方签到记录`);
      }
      await load();
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setCheckinAllBusy(false);
    }
  }, [load]);

  /** 将本地 auths 文件与上游账号池状态按 uid 合并 */
  const merged = useMemo(() => {
    const pool = new Map<string, Record<string, unknown>>();
    for (const item of upstream?.accounts ?? []) {
      const uid = String((item as Record<string, unknown>).uid ?? (item as Record<string, unknown>).UID ?? '');
      if (uid) pool.set(uid, item as Record<string, unknown>);
    }
    return accounts.map((a) => {
      const p = pool.get(a.uid);
      if (!p) return a;
      return {
        ...a,
        healthy: typeof p.healthy === 'boolean' ? p.healthy : null,
        disabled: typeof p.disabled === 'boolean' ? p.disabled : null,
        in_flight: typeof p.in_flight === 'number' ? p.in_flight : null,
        cooling: typeof p.cooling === 'boolean' ? p.cooling : null,
        last_used: typeof p.last_used === 'number' ? p.last_used : null,
      } satisfies Account;
    });
  }, [accounts, upstream]);

  /** 执行单账号操作（签到 / 测活 / 刷新 / 删除），成功后同步底栏计数 */
  async function run(file: string, fn: () => Promise<unknown>, okMsg: string) {
    setBusyFile(file);
    try {
      const res = (await fn()) as {message?: string; ok?: boolean; credits?: number | null};
      const ok = res.ok !== false;
      // 签到会返回刷新后的实时积分，直接就地更新，省一次请求
      if (typeof res.credits === 'number') {
        setAccounts((prev) => prev.map((a) => (a.file === file ? {...a, credits: res.credits} : a)));
      }
      (ok ? notify.ok : notify.err)(res.message || okMsg);
      await load();
      window.dispatchEvent(new Event('workbuddy-manager:accounts-changed'));
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setBusyFile(null);
    }
  }

  /** 仅重启上游容器，不涉及单个账号，因此单独处理 */
  const [restarting, setRestarting] = useState(false);
  async function restartUpstream() {
    setRestarting(true);
    try {
      const res = await accountApi.restart();
      (res.ok ? notify.ok : notify.err)(res.message || '已重启上游');
      await load();
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setRestarting(false);
    }
  }

  /** 账号状态徽章（表格与移动端卡片共用） */
  function renderStatus(a: Account) {
    if (a.disabled === true) return <Badge variant="destructive" className="rounded-full">● 已禁用</Badge>;
    if (a.is_expired) return <Badge variant="destructive" className="rounded-full">● 已过期</Badge>;
    if (a.cooling)
      return (
        <Badge variant="secondary" className="rounded-full text-amber-600 dark:text-amber-400">
          ● 冷却中
        </Badge>
      );
    return (
      <Badge variant="secondary" className="rounded-full text-emerald-600 dark:text-emerald-400">
        ● 在线
      </Badge>
    );
  }

  /** 积分余额 + 数据来源标注。
   *  明确区分「实时」与「缓存 x 秒前」，避免把滞后的数字当成刚查到的。 */
  function renderCredits(a: Account) {
    // 优先用刚查到的实时值，其次上游 /status 的缓存值
    const value = liveCredits[a.uid] ?? a.credits;
    if (value === null || value === undefined) {
      return (
        <span
          className="text-xs text-muted-foreground"
          title="上游尚未返回该账号的积分（可能是刚添加、或上游不可达）"
        >
          —
        </span>
      );
    }
    const meta = creditsMeta[a.uid];
    const tone =
      value <= 0
        ? 'text-red-600 dark:text-red-400'
        : value < 200
          ? 'text-amber-600 dark:text-amber-400'
          : 'text-foreground';
    return (
      <span className="inline-flex items-center gap-1.5">
        <span className={`text-xs font-medium tabular-nums ${tone}`} title="当前可花费积分余额（所有套餐剩余额度合计）">
          {fmtNumber(value)}
        </span>
        {meta &&
          (meta.cached ? (
            <span
              className="rounded-full bg-amber-500/15 px-1.5 py-0.5 text-[10px] leading-3 text-amber-600 dark:text-amber-400"
              title="60 秒内已查过，直接用了服务端缓存；点「刷新积分」可强制重新查询"
            >
              {meta.cache_age != null ? `缓存 ${meta.cache_age}s 前` : '缓存'}
            </span>
          ) : (
            <span
              className="rounded-full bg-emerald-500/15 px-1.5 py-0.5 text-[10px] leading-3 text-emerald-600 dark:text-emerald-400"
              title="刚刚向腾讯查询的实时值"
            >
              实时
            </span>
          ))}
      </span>
    );
  }

  /** Token 有效期进度条 */
  function renderExpiry(a: Account) {
    const pct = Math.min(100, Math.max(0, (a.remain_seconds / TOKEN_TTL) * 100));
    const vis = expiryVisual(a.remain_seconds);
    return (
      <div className="w-[150px]">
        <div className={`mb-1 text-[11px] font-medium tabular-nums ${vis.textClass}`}>
          {fmtRemain(a.remain_seconds)}
        </div>
        <div className="h-1.5 overflow-hidden rounded-full bg-border">
          <div className="h-full rounded-full transition-all" style={{width: `${pct}%`, background: vis.barColor}} />
        </div>
      </div>
    );
  }

  /** 单账号操作按钮组 */
  function renderActions(a: Account) {
    const busy = busyFile === a.file;
    return (
      <div className="flex justify-end gap-1">
        <Button variant="ghost" size="icon" className="h-7 w-7 rounded-md" title="签到" disabled={busy}
          onClick={() => run(a.file, () => accountApi.checkin(a.file), '操作完成')}>
          <Gift className="h-3.5 w-3.5" />
        </Button>
        <Button variant="ghost" size="icon" className="h-7 w-7 rounded-md" title="连通性测试" disabled={busy}
          onClick={() => run(a.file, () => accountApi.test(a.file), '测试完成')}>
          <Zap className="h-3.5 w-3.5" />
        </Button>
        <Button variant="ghost" size="icon" className="h-7 w-7 rounded-md" title="刷新 Token" disabled={busy}
          onClick={() => run(a.file, () => accountApi.refresh(a.file), '刷新完成')}>
          <KeyRound className="h-3.5 w-3.5" />
        </Button>
        <ConfirmDialog
          title={`删除账号「${a.nickname || a.uid}」？`}
          description="将删除本地授权文件，并自动重载上游使其生效。此操作不可撤销。"
          confirmText="删除"
          destructive
          onConfirm={() => run(a.file, () => accountApi.remove(a.file), '已删除')}
          trigger={
            <Button variant="ghost" size="icon" className="h-7 w-7 rounded-md text-red-500 hover:text-red-600" title="删除">
              <Trash2 className="h-3.5 w-3.5" />
            </Button>
          }
        />
      </div>
    );
  }

  /** 头像（首字母） */
  function renderAvatar(a: Account) {
    return (
      <div
        className={
          'grid h-7 w-7 shrink-0 place-items-center rounded-full text-[11px] font-semibold ' +
          (a.is_expired ? 'bg-muted-foreground/20 text-muted-foreground' : 'bg-primary text-primary-foreground')
        }
      >
        {(a.nickname || '?').charAt(0)}
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader
        title="账号管理"
        description="腾讯 CodeBuddy 账号池：Token 有效期、签到与连通性"
        actions={
          <>
            <Button variant="outline" size="sm" className="rounded-full" onClick={load} disabled={loading}>
              <RefreshCw className={loading ? 'animate-spin' : ''} />
              刷新
            </Button>
            {isAdmin && (
              <ConfirmDialog
                title="强制重启上游容器？"
                description="通常无需手动执行：添加或删除账号后会自动重载。仅当上游状态异常、需要强制重载时才使用。重启约 0.5 秒，在途请求会正常完成。"
                confirmText="重启"
                onConfirm={restartUpstream}
                trigger={
                  <Button variant="outline" size="sm" className="rounded-full" disabled={restarting}>
                    <Power className={restarting ? 'animate-spin' : ''} />
                    <span className="hidden sm:inline">强制重启</span>
                    <span className="sm:hidden">重启</span>
                  </Button>
                }
              />
            )}
            <Button
              size="sm"
              variant="outline"
              className="rounded-full"
              onClick={refreshCredits}
              disabled={creditsBusy || !merged.length}
              title="直接向腾讯查询各账号当前积分（上游缓存的积分可能滞后数小时）"
            >
              <Coins className={creditsBusy ? 'animate-pulse' : ''} />
              <span className="hidden sm:inline">刷新积分</span>
              <span className="sm:hidden">积分</span>
            </Button>
            {isAdmin && (
              <Button
                size="sm"
                variant="outline"
                className="rounded-full"
                onClick={checkinAll}
                disabled={checkinAllBusy || !merged.length}
              >
                <CalendarCheck className={checkinAllBusy ? 'animate-pulse' : ''} />
                全部签到
              </Button>
            )}
            {isAdmin && (
              <Button size="sm" className="rounded-full" onClick={() => setAddOpen(true)}>
                <Plus />
                添加账号
              </Button>
            )}
          </>
        }
      />

      <section className="overflow-hidden rounded-[20px] bg-muted">
        {/* 手机端：卡片列表。表格 6 列在窄屏需要横向滚动，读一行要来回拖，
            改为纵向卡片后信息一眼可见 */}
        <div className="divide-y divide-border/40 md:hidden">
          {merged.map((a) => (
            <div key={a.file} className="space-y-2.5 px-3.5 py-3">
              <div className="flex items-center justify-between gap-2">
                <div className="flex min-w-0 items-center gap-2.5">
                  {renderAvatar(a)}
                  <div className="min-w-0">
                    <div
                      className={
                        'truncate text-sm font-medium ' + (a.is_expired ? 'text-muted-foreground' : '')
                      }
                    >
                      {a.nickname || '未命名'}
                    </div>
                    <div className="truncate font-mono text-[10px] text-muted-foreground">{a.uid}</div>
                  </div>
                </div>
                {renderStatus(a)}
              </div>

              <div className="flex items-center justify-between gap-3">
                <div className="flex shrink-0 items-center gap-1.5">
                  <Coins className="h-3.5 w-3.5 text-muted-foreground" />
                  {renderCredits(a)}
                </div>
                {renderExpiry(a)}
              </div>

              {isAdmin && renderActions(a)}
            </div>
          ))}
          {!merged.length && !loading && (
            <div className="px-4 py-12 text-center text-xs text-muted-foreground">暂无账号</div>
          )}
          {loading && !merged.length && (
            <div className="px-4 py-12 text-center text-xs text-muted-foreground">加载中…</div>
          )}
        </div>

        {/* 桌面端：表格 */}
        <div className="hidden md:block">
        <Table>
          <TableHeader>
            <TableRow className="border-b border-border/60 hover:bg-transparent">
              <TableHead className="pl-4 text-[11px] text-muted-foreground">昵称</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">UID</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">状态</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">积分余额</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">Token 有效期</TableHead>
              {isAdmin && <TableHead className="pr-4 text-right text-[11px] text-muted-foreground">操作</TableHead>}
            </TableRow>
          </TableHeader>
          <TableBody>
            {merged.map((a) => (
              <TableRow key={a.file} className="border-b border-border/40">
                <TableCell className="pl-4">
                  <div className="flex items-center gap-2.5">
                    {renderAvatar(a)}
                    <span className={'truncate text-sm font-medium ' + (a.is_expired ? 'text-muted-foreground' : '')}>
                      {a.nickname || '未命名'}
                    </span>
                  </div>
                </TableCell>
                <TableCell className="font-mono text-xs text-muted-foreground">{a.uid}</TableCell>
                <TableCell>{renderStatus(a)}</TableCell>
                <TableCell>{renderCredits(a)}</TableCell>
                <TableCell>{renderExpiry(a)}</TableCell>
                {isAdmin && <TableCell className="pr-4">{renderActions(a)}</TableCell>}
              </TableRow>
            ))}
          </TableBody>
        </Table>
        </div>

        {!merged.length && !loading && (
          <EmptyState
            icon={Users}
            title="暂无账号"
            description={isAdmin ? '点击右上角「添加账号」扫码授权' : '请联系管理员添加账号'}
            className="flex flex-col items-center justify-center py-16 text-center"
          >
            {isAdmin && (
              <Button className="mt-4 rounded-full" onClick={() => setAddOpen(true)}>
                <Plus />
                添加账号
              </Button>
            )}
          </EmptyState>
        )}
        {loading && !merged.length && (
          <div className="py-16 text-center text-xs text-muted-foreground">加载中…</div>
        )}
      </section>

      {/* 签到记录：上游自动签到成功时静默、失败才打日志，
          因此这里同时呈现「本端触发记录」与「上游容器签到/保活日志」 */}
      <section className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <div className="overflow-hidden rounded-[20px] bg-muted">
          <div className="flex items-center justify-between px-4 py-3">
            <div className="flex items-center gap-2 text-sm font-medium">
              <CalendarCheck className="h-4 w-4" />
              签到记录
              <span className="text-[11px] font-normal text-muted-foreground">
                （本端触发：手动 / 批量 / 添加账号）
              </span>
            </div>
            {isAdmin && checkinLogs.length > 0 && (
              <ConfirmDialog
                title="清空签到记录？"
                description="仅删除本端的签到历史记录，不影响账号与上游数据。"
                confirmText="清空"
                destructive
                onConfirm={async () => {
                  await accountApi.clearCheckinLogs();
                  notify.ok('已清空');
                  await load();
                }}
                trigger={
                  <Button variant="ghost" size="sm" className="h-7 rounded-full text-red-500">
                    <Trash2 className="h-3.5 w-3.5" />
                    清空
                  </Button>
                }
              />
            )}
          </div>
          <Table>
            <TableHeader>
              <TableRow className="border-b border-border/60 hover:bg-transparent">
                <TableHead className="pl-4 text-[11px] text-muted-foreground">时间</TableHead>
                <TableHead className="text-[11px] text-muted-foreground">账号</TableHead>
                <TableHead className="text-[11px] text-muted-foreground">来源</TableHead>
                <TableHead className="pr-4 text-[11px] text-muted-foreground">结果</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {checkinLogs.slice(0, 20).map((l) => (
                <TableRow key={l.id} className="border-b border-border/40">
                  <TableCell className="pl-4 text-xs text-muted-foreground">{fmtDateTime(l.ts)}</TableCell>
                  <TableCell className="max-w-[120px] truncate text-xs">{l.nickname || l.uid || '—'}</TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {{'manual': '手动', 'manual-batch': '批量', 'add': '添加账号'}[l.source] || l.source}
                  </TableCell>
                  <TableCell className="pr-4">
                    {l.success ? (
                      <Badge variant="secondary" className="rounded-full text-emerald-600 dark:text-emerald-400">
                        {l.message || '成功'}
                      </Badge>
                    ) : (
                      <Badge variant="destructive" className="rounded-full" title={l.message}>
                        {l.message || '失败'}
                      </Badge>
                    )}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          {!checkinLogs.length && (
            <div className="py-10 text-center text-xs text-muted-foreground">
              暂无签到记录。点「全部签到」或单个账号的 🎁 会在此留痕。
            </div>
          )}
        </div>

        <div className="overflow-hidden rounded-[20px] bg-muted">
          <div className="flex items-center justify-between px-4 py-3">
            <div className="flex items-center gap-2 text-sm font-medium">
              <History className="h-4 w-4" />
              上游签到 / 保活日志
            </div>
            <span className="text-[11px] text-muted-foreground">来自容器日志</span>
          </div>
          {upstreamLines.length ? (
            <div className="max-h-[300px] overflow-auto px-4 pb-3">
              <pre className="whitespace-pre-wrap break-all font-mono text-[11px] leading-5 text-muted-foreground">
                {upstreamLines.slice(-40).join('\n')}
              </pre>
            </div>
          ) : (
            <div className="px-4 py-10 text-center text-xs leading-5 text-muted-foreground">
              <TriangleAlert className="mx-auto mb-2 h-4 w-4 text-amber-500" />
              上游自动签到<b>成功时不会打日志</b>（源码里仅在失败时记录），
              因此这里通常是空的 —— 没有记录即代表没有失败。
              想看成功记录，请用上方的「全部签到」，结果会记入左侧列表。
            </div>
          )}
        </div>
      </section>

      {/* 自动任务与积分记录：上游把结果打在容器日志里且重建即丢，
          这里展示后台采集器落库后的长期留痕，便于核对积分收益 */}
      <section className="overflow-hidden rounded-[20px] bg-muted">
        <div className="flex flex-wrap items-center justify-between gap-2 px-4 py-3">
          <div className="flex items-center gap-2 text-sm font-medium">
            <Cat className="h-4 w-4" />
            自动任务与积分记录
            <span className="text-[11px] font-normal text-muted-foreground">
              猫猫旅行 / 活跃上报 / 自动签到 / 保活
            </span>
          </div>
          <div className="flex items-center gap-2">
            {taskStats && taskStats.total > 0 && (
              <Badge variant="secondary" className="rounded-full tabular-nums">
                共 {fmtNumber(taskStats.total)} 条
                {taskStats.total_credits > 0 && (
                  <span className="ml-1 text-emerald-600 dark:text-emerald-400">
                    +{fmtNumber(taskStats.total_credits)} 积分
                  </span>
                )}
              </Badge>
            )}
            <Button
              variant="outline"
              size="sm"
              className="h-7 rounded-full text-[11px]"
              disabled={collectBusy}
              onClick={collectTasks}
            >
              <RefreshCw className={collectBusy ? 'animate-spin' : ''} />
              立即采集
            </Button>
            {isAdmin && taskLogs.length > 0 && (
              <ConfirmDialog
                title="清空任务记录？"
                description="仅删除管理端采集留存的记录，不影响账号与上游运行。"
                confirmText="清空"
                destructive
                onConfirm={async () => {
                  await accountApi.clearTaskLogs();
                  notify.ok('已清空');
                  await load();
                }}
                trigger={
                  <Button variant="ghost" size="sm" className="h-7 rounded-full text-red-500">
                    <Trash2 className="h-3.5 w-3.5" />
                    清空
                  </Button>
                }
              />
            )}
          </div>
        </div>

        {taskLogs.length > 0 && (
          <div className="flex flex-wrap items-center gap-1.5 px-4 pb-3">
            <Filter className="h-3 w-3 text-muted-foreground" />
            {[
              {id: 'all', label: '全部'},
              ...Object.entries(kindLabels).map(([id, label]) => ({id, label})),
            ].map((t) => {
              const active = taskFilter === t.id;
              return (
                <button
                  key={t.id}
                  type="button"
                  onClick={() => setTaskFilter(t.id)}
                  className={
                    'rounded-full px-2.5 py-1 text-[11px] transition-colors ' +
                    (active
                      ? 'bg-foreground text-background'
                      : 'bg-background/60 text-muted-foreground hover:text-foreground')
                  }
                >
                  {t.label}
                  {taskStats?.by_kind?.[t.id]?.count ? ` ${taskStats.by_kind[t.id].count}` : ''}
                </button>
              );
            })}
          </div>
        )}

        {taskLogs.length ? (
          <div className="max-h-[360px] overflow-auto px-4 pb-4">
            {/* 手机端：卡片式；桌面：表格 */}
            <div className="space-y-1.5 md:hidden">
              {taskLogs
                .filter((l) => taskFilter === 'all' || l.kind === taskFilter)
                .slice(0, 60)
                .map((l) => (
                  <div key={l.id} className="rounded-xl bg-background/60 px-3 py-2">
                    <div className="flex items-center justify-between gap-2">
                      <span className="text-xs font-medium">{kindLabels[l.kind] || l.kind}</span>
                      <span className="text-xs font-semibold tabular-nums">
                        {l.credits > 0 ? (
                          <span className="text-emerald-600 dark:text-emerald-400">+{l.credits}</span>
                        ) : (
                          <span className="text-muted-foreground">—</span>
                        )}
                      </span>
                    </div>
                    <div className="mt-1 break-words text-[11px] leading-4 text-muted-foreground">
                      {l.message}
                    </div>
                    <div className="mt-1 flex items-center justify-between text-[10px] text-muted-foreground">
                      <span className="font-mono">{l.uid}</span>
                      <span className="tabular-nums">{fmtDateTime(l.ts)}</span>
                    </div>
                  </div>
                ))}
            </div>
            <div className="hidden md:block">
            <Table>
              <TableHeader>
                <TableRow className="border-b border-border/60 hover:bg-transparent">
                  <TableHead className="pl-0 text-[11px] text-muted-foreground">时间</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">类型</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">账号</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">结果</TableHead>
                  <TableHead className="pr-0 text-right text-[11px] text-muted-foreground">积分</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {taskLogs
                  .filter((l) => taskFilter === 'all' || l.kind === taskFilter)
                  .slice(0, 60)
                  .map((l) => {
                    const tone: Record<string, string> = {
                      credit: 'text-emerald-600 dark:text-emerald-400',
                      ok: 'text-muted-foreground',
                      info: 'text-muted-foreground',
                      warn: 'text-amber-600 dark:text-amber-400',
                      error: 'text-red-600 dark:text-red-400',
                    };
                    return (
                      <TableRow key={l.id} className="border-b border-border/40">
                        <TableCell className="pl-0 text-xs tabular-nums text-muted-foreground">
                          {fmtDateTime(l.ts)}
                        </TableCell>
                        <TableCell className="text-xs">
                          {kindLabels[l.kind] || l.kind}
                        </TableCell>
                        <TableCell className="text-xs tabular-nums">{l.uid || '—'}</TableCell>
                        <TableCell className={`max-w-[380px] truncate text-xs ${tone[l.level] || ''}`} title={l.message}>
                          {l.message}
                        </TableCell>
                        <TableCell className="pr-0 text-right text-xs font-medium tabular-nums">
                          {l.credits > 0 ? (
                            <span className="text-emerald-600 dark:text-emerald-400">+{l.credits}</span>
                          ) : (
                            <span className="text-muted-foreground">—</span>
                          )}
                        </TableCell>
                      </TableRow>
                    );
                  })}
              </TableBody>
            </Table>
            </div>
            {taskLogs.filter((l) => taskFilter === 'all' || l.kind === taskFilter).length === 0 && (
              <div className="py-8 text-center text-xs text-muted-foreground">
                该类型下暂无记录。
              </div>
            )}
          </div>
        ) : (
          <div className="px-4 py-10 text-center text-xs leading-5 text-muted-foreground">
            <Cat className="mx-auto mb-2 h-4 w-4" />
            暂无自动任务记录。上游的签到 / 猫猫旅行 / 活跃上报 / 保活结果会打在容器日志里，
            后台每 45 秒采集一次并长期保留（容器重建也不会丢）。
            <br />
            想立刻看到结果，点右上角「立即采集」。
          </div>
        )}
      </section>

      <AddAccountDialog open={addOpen} onOpenChange={setAddOpen} onSuccess={load} />
    </div>
  );
}
