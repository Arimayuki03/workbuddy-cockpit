'use client';

import {useCallback, useEffect, useMemo, useState} from 'react';
import {
  CalendarCheck,
  Cat,
  Filter,
  History,
  RefreshCw,
  Trash2,
  TriangleAlert,
} from 'lucide-react';
import {useHeartbeat} from '@/lib/use-heartbeat';
import {notify} from '@/lib/toast';
import {accountApi, errText} from '@/lib/api';
import type {CheckinLog, TaskLog, TaskLogResponse} from '@/lib/types';
import {fmtDateTime, fmtNumber} from '@/lib/format';
import {PageHeader} from '@/components/common/layout/PageHeader';
import {ConfirmDialog} from '@/components/common/layout/ConfirmDialog';
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

/** 来源说明（本端触发的签到） */
const SOURCE_LABELS: Record<string, string> = {
  manual: '手动',
  'manual-batch': '批量',
  add: '添加账号',
};

/** 结果文案：优先中文，英文原文作为悬浮提示保留 */
function resultText(l: TaskLog): string {
  return l.message_cn || l.message;
}

/** 任务日志的结果等级配色 */
const LEVEL_TONE: Record<string, string> = {
  credit: 'text-emerald-600 dark:text-emerald-400',
  ok: 'text-muted-foreground',
  info: 'text-muted-foreground',
  warn: 'text-amber-600 dark:text-amber-400',
  error: 'text-red-600 dark:text-red-400',
};

export default function TasksPage() {
  const {isAdmin} = useAuth();
  const [checkinLogs, setCheckinLogs] = useState<CheckinLog[]>([]);
  const [upstreamLines, setUpstreamLines] = useState<string[]>([]);
  const [taskLogs, setTaskLogs] = useState<TaskLog[]>([]);
  const [taskStats, setTaskStats] = useState<TaskLogResponse['stats'] | null>(null);
  const [kindLabels, setKindLabels] = useState<Record<string, string>>({});
  const [taskFilter, setTaskFilter] = useState<string>('all');
  const [collectBusy, setCollectBusy] = useState(false);

  const load = useCallback(async () => {
    const [logRes, ulRes, taskRes] = await Promise.allSettled([
      accountApi.checkinLogs(200),
      accountApi.upstreamLogs(300),
      accountApi.taskLogs(500),
    ]);
    if (logRes.status === 'fulfilled') setCheckinLogs(logRes.value);
    if (ulRes.status === 'fulfilled') setUpstreamLines(ulRes.value.lines);
    if (taskRes.status === 'fulfilled') {
      setTaskLogs(taskRes.value.logs);
      setTaskStats(taskRes.value.stats);
      setKindLabels(taskRes.value.kinds);
    }
    if (logRes.status === 'rejected' && taskRes.status === 'rejected') {
      notify.err(errText(logRes.reason ?? taskRes.reason));
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  // 上游任务按点执行，停留期间心跳刷新，避免一直看旧记录
  useHeartbeat(load, 30000);

  /** 立即采集一次上游任务日志 */
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

  function accountLabel(l: TaskLog) {
    const uid = l.uid || '';
    if (!uid) return '—';
    // 昵称由后端解析：上游 2026-09-12 起只打 uid 前 8 位，前端拿不到完整 uid
    return l.nickname || uid;
  }

  const filteredTasks = useMemo(
    () => taskLogs.filter((l) => taskFilter === 'all' || l.kind === taskFilter),
    [taskLogs, taskFilter],
  );

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader
        title="任务记录"
        description="签到结果、上游自动任务与积分收益（每 30 秒自动刷新）"
      />

      {/* 签到记录 + 上游原始日志 */}
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

          {checkinLogs.length ? (
            <>
              {/* 手机端：卡片 */}
              <div className="space-y-1.5 px-3.5 pb-4 md:hidden">
                {checkinLogs.slice(0, 60).map((l) => (
                  <div key={l.id} className="rounded-xl bg-background/60 px-3 py-2">
                    <div className="flex items-center justify-between gap-2">
                      <span className="truncate text-xs font-medium">{l.nickname || l.uid || '—'}</span>
                      {l.success ? (
                        <Badge variant="secondary" className="shrink-0 rounded-full text-emerald-600 dark:text-emerald-400">
                          {l.message || '成功'}
                        </Badge>
                      ) : (
                        <Badge variant="destructive" className="shrink-0 rounded-full" title={l.message}>
                          {l.message || '失败'}
                        </Badge>
                      )}
                    </div>
                    <div className="mt-1 flex items-center justify-between text-[10px] text-muted-foreground">
                      <span>{SOURCE_LABELS[l.source] || l.source}</span>
                      <span className="tabular-nums">{fmtDateTime(l.ts)}</span>
                    </div>
                  </div>
                ))}
              </div>

              <div className="hidden md:block">
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
                    {checkinLogs.slice(0, 60).map((l) => (
                      <TableRow key={l.id} className="border-b border-border/40">
                        <TableCell className="pl-4 text-xs tabular-nums text-muted-foreground">
                          {fmtDateTime(l.ts)}
                        </TableCell>
                        <TableCell className="max-w-[120px] truncate text-xs">{l.nickname || l.uid || '—'}</TableCell>
                        <TableCell className="text-xs text-muted-foreground">
                          {SOURCE_LABELS[l.source] || l.source}
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
              </div>
            </>
          ) : (
            <div className="px-4 py-10 text-center text-xs text-muted-foreground">
              暂无签到记录。在「账号管理」点「全部签到」或单个账号的 🎁 会在此留痕。
            </div>
          )}
        </div>

        <div className="overflow-hidden rounded-[20px] bg-muted">
          <div className="flex items-center justify-between px-4 py-3">
            <div className="flex items-center gap-2 text-sm font-medium">
              <History className="h-4 w-4" />
              上游原始日志
              <span className="text-[11px] font-normal text-muted-foreground">
                （签到 / 保活 / 旅行 / 活跃）
              </span>
            </div>
            <span className="text-[11px] text-muted-foreground">来自容器日志</span>
          </div>
          {upstreamLines.length ? (
            <div className="max-h-[320px] overflow-auto px-4 pb-3">
              <pre className="whitespace-pre-wrap break-all font-mono text-[11px] leading-5 text-muted-foreground">
                {upstreamLines.slice(-60).join('\n')}
              </pre>
            </div>
          ) : (
            <div className="px-4 py-10 text-center text-xs leading-5 text-muted-foreground">
              <TriangleAlert className="mx-auto mb-2 h-4 w-4 text-amber-500" />
              上游只在<b>失败</b>与<b>旅行 / 活跃</b>时打日志：签到成功、
              保活正常都是静默的，所以这里没有记录不代表没执行。
              结构化、可长期保留（容器重建也不丢）的记录见下方「自动任务与积分记录」。
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
          <div className="pb-4">
            {/* 手机端：卡片式；桌面：表格 */}
            <div className="space-y-1.5 px-3.5 md:hidden">
              {filteredTasks.map((l) => (
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
                  <div
                    className={'mt-1 break-words text-[11px] leading-4 ' + (LEVEL_TONE[l.level] || 'text-muted-foreground')}
                    title={l.message}
                  >
                    {resultText(l)}
                  </div>
                  <div className="mt-1 flex items-center justify-between gap-2 text-[10px] text-muted-foreground">
                    <span className="truncate" title={l.uid}>
                      {accountLabel(l)}
                    </span>
                    <span className="shrink-0 tabular-nums">{fmtDateTime(l.ts)}</span>
                  </div>
                </div>
              ))}
            </div>

            <div className="hidden md:block">
              <Table>
                <TableHeader>
                  <TableRow className="border-b border-border/60 hover:bg-transparent">
                    <TableHead className="pl-4 text-[11px] text-muted-foreground">时间</TableHead>
                    <TableHead className="text-[11px] text-muted-foreground">类型</TableHead>
                    <TableHead className="text-[11px] text-muted-foreground">账号</TableHead>
                    <TableHead className="text-[11px] text-muted-foreground">结果</TableHead>
                    <TableHead className="pr-4 text-right text-[11px] text-muted-foreground">积分</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {filteredTasks.map((l) => (
                    <TableRow key={l.id} className="border-b border-border/40">
                      <TableCell className="pl-4 text-xs tabular-nums text-muted-foreground">
                        {fmtDateTime(l.ts)}
                      </TableCell>
                      <TableCell className="text-xs">{kindLabels[l.kind] || l.kind}</TableCell>
                      <TableCell className="max-w-[180px] truncate text-xs" title={l.uid}>
                        {accountLabel(l)}
                      </TableCell>
                      <TableCell
                        className={`max-w-[520px] truncate text-xs ${LEVEL_TONE[l.level] || ''}`}
                        title={l.message}
                      >
                        {resultText(l)}
                      </TableCell>
                      <TableCell className="pr-4 text-right text-xs font-medium tabular-nums">
                        {l.credits > 0 ? (
                          <span className="text-emerald-600 dark:text-emerald-400">+{l.credits}</span>
                        ) : (
                          <span className="text-muted-foreground">—</span>
                        )}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>

            {!filteredTasks.length && (
              <div className="py-8 text-center text-xs text-muted-foreground">该类型下暂无记录。</div>
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
    </div>
  );
}
