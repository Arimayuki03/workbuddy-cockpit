'use client';

import {useEffect, useMemo, useRef, useState} from 'react';
import {
  ArrowDown10,
  ArrowUp10,
  ListFilter,
  Radio,
  ScrollText,
  Terminal,
} from 'lucide-react';
import {useHeartbeat} from '@/lib/use-heartbeat';
import {logApi} from '@/lib/api';
import {useCachedAsync} from '@/lib/data-cache';
import type {RequestLog, RequestLogsResponse, SystemLogsResponse} from '@/lib/types';
import {fmtCredit, fmtDateTime, fmtLatency, fmtNumber} from '@/lib/format';
import {PageHeader} from '@/components/common/layout/PageHeader';
import {EmptyState} from '@/components/common/layout/EmptyState';
import {TableSkeleton} from '@/components/common/layout/LoadSkeleton';
import {CopyButton} from '@/components/ui/copy-button';
import {useT} from '@/lib/i18n/provider';
import {Badge} from '@/components/ui/badge';
import {Input} from '@/components/ui/input';
import {Label} from '@/components/ui/label';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';
import {
  Drawer,
  DrawerContent,
  DrawerDescription,
  DrawerHeader,
  DrawerTitle,
} from '@/components/ui/drawer';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import {Tabs, TabsContent, TabsList, TabsTrigger} from '@/components/ui/tabs';
import {Switch} from '@/components/ui/switch';

/**
 * 系统日志频道 → i18n 键。
 * Select 的「全部」哨兵值用 'all' 而不是 ''：Radix Select 把空串 value
 * 视为占位符（不渲染对应 item、触发器只显示 placeholder），这就是
 * 旧版「默认选中全部却看不到全部」的原因。
 */
const ALL_CHANNEL = 'all';
/** 请求日志模型筛选的「全部」哨兵值（同上，避免空串） */
const ALL_MODEL = 'all';
/** 条数上限选项：两个 tab 共用同一组（系统日志环形缓冲容量 500，超出即全量） */
const LIMIT_OPTIONS = [100, 200, 500, 1000] as const;
const CHANNELS = [
  {id: ALL_CHANNEL, key: 'logs.sysAll'},
  {id: 'task', key: 'logs.sysTask'},
  {id: 'chat', key: 'logs.sysChat'},
  {id: 'sys', key: 'logs.sysSystem'},
] as const;

export default function LogsPage() {
  const t = useT();
  // 请求日志与用量是全局记录（Go 网关单实例双版本），不按版本过滤
  const [tab, setTab] = useState('requests');

  /* ── 请求日志 ─────────────────────────────────────── */
  const [detail, setDetail] = useState<RequestLog | null>(null);
  const [model, setModel] = useState('');
  const [status, setStatus] = useState('all');
  const [limit, setLimit] = useState('200');
  /** 全文搜索关键词：输入即时过滤（model / uid / 错误信息），不点按钮 */
  const [query, setQuery] = useState('');
  /** 实时刷新开关（请求日志与系统日志共用）：默认开，5 秒一轮 */
  const [live, setLive] = useState(true);

  // 两个日志源接缓存：切页先出上次的列表再静默续新（日志是环形缓冲快照，
  // 缓存值与新值形状一致）。key 带 limit——改条数上限等于换数据集。
  const requestsCache = useCachedAsync<RequestLogsResponse>(
    `logs:requests:${limit}`,
    () => logApi.requestLogs(Number(limit) || 200),
    {ttl: 4000},
  );
  const logs = useMemo(() => requestsCache.data?.items ?? [], [requestsCache.data]);
  const loading = requestsCache.loading;
  const loadRequests = requestsCache.refresh;

  // 环形缓冲只有最近 N 条，筛选全部在前端做（数据量有界）；
  // 模型筛选是下拉（来自当前日志里真实出现过的模型），搜索框做全文即时过滤
  const modelOptions = useMemo(
    () => Array.from(new Set(logs.map((l) => l.model).filter(Boolean))).sort(),
    [logs],
  );

  const filteredLogs = useMemo(() => {
    let items = logs;
    if (model) items = items.filter((l) => l.model === model);
    if (status === 'ok') items = items.filter((l) => l.status >= 200 && l.status < 300);
    if (status === 'error') items = items.filter((l) => !(l.status >= 200 && l.status < 300));
    const kw = query.trim().toLowerCase();
    if (kw) {
      items = items.filter(
        (l) =>
          l.model?.toLowerCase().includes(kw) ||
          l.uid?.toLowerCase().includes(kw) ||
          l.error?.toLowerCase().includes(kw),
      );
    }
    return items;
  }, [logs, model, status, query]);

  /* ── 系统日志（三频道）──────────────────────────────── */
  const [channel, setChannel] = useState(ALL_CHANNEL);
  /** 时间方向：desc（新→旧，默认）/ asc（旧→新，追排到底部看实时） */
  const [sysOrder, setSysOrder] = useState<'desc' | 'asc'>('desc');
  const [sysQuery, setSysQuery] = useState('');
  const logEndRef = useRef<HTMLDivElement>(null);
  /** 正序追日志时用户是否钉在底部：只有钉底才自动跟随，往上翻阅旧日志时不拽人 */
  const pinnedRef = useRef(true);

  const systemCache = useCachedAsync<SystemLogsResponse>(
    'logs:system',
    () => logApi.system('', 500),
    {ttl: 4000},
  );
  const sysEntries = useMemo(() => systemCache.data?.entries ?? [], [systemCache.data]);
  const sysLoading = systemCache.loading;
  const loadSystem = systemCache.refresh;

  useEffect(() => {
    if (tab === 'requests') loadRequests();
    else loadSystem();
    // 依赖是「会改变查询范围」的项；筛选/搜索/排序在前端即时生效
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tab, limit]);

  // 实时刷新：默认开（5 秒一轮），开关关掉即暂停——冻结当前视图，
  // 不再发请求也不弹错；重新打开后下一轮心跳立即恢复。心跳直接走 refresh()：
  // 它拿到新数据后 setData 更新界面（putCache 只写缓存、不会触发重渲染），
  // 缓存已有数据时不进 loading、不闪骨架屏；失败静默吞掉（心跳稍后再试），
  // 与「网关临时无响应时不刷错误弹窗雨」的意图一致。
  useHeartbeat(
    () => {
      if (tab === 'requests') loadRequests().catch(() => {/* 静默，等下一轮 */});
      else loadSystem().catch(() => {/* 静默，等下一轮 */});
    },
    live ? 5000 : 0,
  );

  // 系统日志展示条目：频道 → 关键词 → 条数截取 → 时间方向，纯前端即时过滤。
  // 条数与请求日志共用 limit 状态：环形缓冲固定容量 500（服务端全量返回），
  // 上限在前端截取——倒序取最新 N 条，正序取最旧 N 条（滚动追实时语义不变）。
  const sysFiltered = useMemo(() => {
    let entries = sysEntries;
    if (channel !== ALL_CHANNEL) entries = entries.filter((e) => e.ch === channel);
    const kw = sysQuery.trim().toLowerCase();
    if (kw) entries = entries.filter((e) => e.text?.toLowerCase().includes(kw) || e.ch?.includes(kw));
    const capped =
      Number(limit) >= 1000 || entries.length <= Number(limit)
        ? entries
        : sysOrder === 'asc'
          ? entries.slice(-Number(limit)) // 正序保留最旧 N 条（追实时）
          : entries.slice(0, Number(limit)); // 倒序保留最新 N 条
    const sorted = [...capped];
    sorted.reverse(); // ring 快照是时间升序；倒序即新→旧
    return sysOrder === 'asc' ? sorted.reverse() : sorted;
  }, [sysEntries, channel, sysQuery, limit, sysOrder]);

  // 时间正序时钉底自动跟随最新（用户上滚看旧日志就暂停跟随，滚回底部恢复）；
  // 倒序时最新在顶部，无需滚动
  useEffect(() => {
    if (tab === 'system' && sysOrder === 'asc' && pinnedRef.current && logEndRef.current) {
      logEndRef.current.scrollTop = logEndRef.current.scrollHeight;
    }
  }, [sysFiltered, tab, sysOrder]);

  /** 时间显示：RFC3339 → 本地时间 */
  function fmtLogTime(ts: string): string {
    const at = Date.parse(ts);
    return Number.isFinite(at) ? fmtDateTime(at / 1000) : ts;
  }

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader
        title={t('logs.title')}
        description={t('logs.description')}
        actions={
          <div className="flex items-center gap-2 rounded-full border border-border/60 bg-muted px-3 py-1.5">
            {/* 实时开关：两个 tab 共用——日志页的价值就是「不断有新内容看」，
                所以放页头而不塞进某个 tab 的筛选区 */}
            <Radio
              className={
                'h-3.5 w-3.5 ' + (live ? 'text-emerald-600 dark:text-emerald-400' : 'text-muted-foreground/60')
              }
            />
            <Label className="text-xs font-normal text-muted-foreground">
              {t('logs.liveToggle')}
            </Label>
            <Switch checked={live} onCheckedChange={setLive} aria-label={t('logs.liveToggle')} />
          </div>
        }
      />

      <Tabs value={tab} onValueChange={setTab}>
        <TabsList className="w-max">
          <TabsTrigger value="requests">
            <ScrollText className="mr-1.5 h-3.5 w-3.5" />
            {t('logs.tabRequests')}
          </TabsTrigger>
          <TabsTrigger value="system">
            <Terminal className="mr-1.5 h-3.5 w-3.5" />
            {t('logs.tabSystem')}
          </TabsTrigger>
        </TabsList>

        {/* ═══ 请求日志 ═══ */}
        <TabsContent value="requests" className="mt-3 space-y-3">
          <section className="rounded-[20px] bg-muted p-4">
            {/* 与系统日志 tab 同构的四列筛选区：下拉/输入统一高度，搜索框不设上限 */}
            <div className="grid grid-cols-2 items-end gap-3 md:grid-cols-[1fr_1fr_1fr_2fr]">
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">{t('nav.models')}</Label>
                {/* 模型筛选：下拉列出本页日志里真实出现过的模型（不用手输拼错名） */}
                <Select value={model || ALL_MODEL} onValueChange={(v) => setModel(v === ALL_MODEL ? '' : v)}>
                  <SelectTrigger className="w-full bg-background"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value={ALL_MODEL}>{t('common.all')}</SelectItem>
                    {modelOptions.map((m) => (
                      <SelectItem key={m} value={m} className="text-xs font-mono">{m}</SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">{t('accounts.colStatus')}</Label>
                <Select value={status} onValueChange={setStatus}>
                  <SelectTrigger className="w-full bg-background"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all">{t('common.all')}</SelectItem>
                    <SelectItem value="ok">{t('common.success')}</SelectItem>
                    <SelectItem value="error">{t('common.failure')}</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">{t('logs.limitLabel')}</Label>
                <Select value={limit} onValueChange={setLimit}>
                  <SelectTrigger className="w-full bg-background"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    {LIMIT_OPTIONS.map((n) => (
                      <SelectItem key={n} value={String(n)}>{fmtNumber(n)}</SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="col-span-2 space-y-1.5 md:col-span-1">
                <Label className="text-[11px] text-muted-foreground">{t('common.search')}</Label>
                {/* 全文搜索（模型 / 账号 / 错误信息）：输入即时过滤，无需按钮 */}
                <Input
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  placeholder={t('logs.searchRequests')}
                  className="bg-background"
                />
              </div>
            </div>
            <p className="mt-2 text-[10px] leading-4 text-muted-foreground/70">
              {t('logs.requestsNote')}
              {live ? ` ${t('logs.liveOnNote')}` : ` ${t('logs.liveOffNote')}`}
            </p>
          </section>

          <section className="overflow-hidden rounded-[20px] bg-muted">
            <Table>
              <TableHeader>
                <TableRow className="border-b border-border/60 hover:bg-transparent">
                  <TableHead className="pl-4 text-[11px] text-muted-foreground">{t('logs.colTime')}</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">{t('tasks.colAccount')}</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">{t('nav.models')}</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">{t('accounts.colStatus')}</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">{t('logs.colFirstToken')}</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">Token</TableHead>
                  <TableHead className="pr-4 text-[11px] text-muted-foreground">{t('metric.paid')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {filteredLogs.map((l, i) => (
                  <TableRow
                    key={`${l.time}-${i}`}
                    className="cursor-pointer border-b border-border/40"
                    onClick={() => setDetail(l)}
                  >
                    <TableCell className="pl-4 text-xs text-muted-foreground">{fmtLogTime(l.time)}</TableCell>
                    <TableCell className="font-mono text-xs text-muted-foreground">{l.uid || '—'}</TableCell>
                    <TableCell className="text-xs">{l.model || '—'}</TableCell>
                    <TableCell>
                      {l.status >= 200 && l.status < 300 ? (
                        <Badge variant="secondary" className="rounded-full text-emerald-600 dark:text-emerald-400">{l.status}</Badge>
                      ) : l.status >= 400 && l.status < 500 ? (
                        <Badge variant="secondary" className="rounded-full bg-amber-500/12 text-amber-600 dark:text-amber-400">
                          {l.status || 'ERR'}
                        </Badge>
                      ) : (
                        <Badge variant="destructive" className="rounded-full">{l.status || 'ERR'}</Badge>
                      )}
                    </TableCell>
                    {/* 首字延迟：反映「上游多久开始回话」。回答越长总耗时越大，
                        所以判断上游快慢只看这一列。 */}
                    <TableCell
                      className={
                        'text-xs tabular-nums ' +
                        (l.ttfb_ms >= 3000
                          ? 'font-medium text-amber-600 dark:text-amber-400'
                          : 'text-muted-foreground')
                      }
                    >
                      {l.ttfb_ms ? fmtLatency(l.ttfb_ms) : <span className="text-muted-foreground/50">—</span>}
                    </TableCell>
                    <TableCell className="text-xs tabular-nums">
                      {l.tokens > 0 ? (
                        fmtNumber(l.tokens)
                      ) : (
                        <span className="text-muted-foreground/70">—</span>
                      )}
                    </TableCell>
                    <TableCell className="pr-4 text-xs tabular-nums">
                      {l.has_credit ? (
                        <span className={l.credit > 0 ? 'text-amber-600 dark:text-amber-400' : ''}>
                          {fmtCredit(l.credit)}
                        </span>
                      ) : (
                        <span className="text-muted-foreground/70" title={t('logs.noCreditTitle')}>—</span>
                      )}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>

            {!filteredLogs.length && !loading && (
              <EmptyState
                icon={ScrollText}
                title={t('logs.emptyTitle')}
                description={t('logs.emptyDesc')}
                className="flex flex-col items-center justify-center py-16 text-center"
              />
            )}
            {loading && !logs.length && <TableSkeleton rows={6} />}
          </section>
        </TabsContent>

        {/* ═══ 系统日志（三频道）═══ */}
        <TabsContent value="system" className="mt-3 space-y-3">
          <section className="rounded-[20px] bg-muted p-4">
            {/* 与请求日志 tab 同构的四列筛选区：列宽/控件高度/Label 全一致 */}
            <div className="grid grid-cols-2 items-end gap-3 md:grid-cols-[1fr_1fr_1fr_2fr]">
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">{t('logs.sysChannel')}</Label>
                <Select value={channel} onValueChange={setChannel}>
                  <SelectTrigger className="w-full bg-background"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    {CHANNELS.map((c) => (
                      <SelectItem key={c.id} value={c.id} className="text-xs">{t(c.key)}</SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">{t('logs.sortTime')}</Label>
                {/* 时间方向：正序 = 旧→新（底部自动跟随最新），倒序 = 新→旧（顶部即最新） */}
                <Select value={sysOrder} onValueChange={(v) => setSysOrder(v as 'asc' | 'desc')}>
                  <SelectTrigger className="w-full bg-background">
                    <ListFilter className="mr-0.5 h-3.5 w-3.5 opacity-70" />
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="desc" className="text-xs">
                      <span className="flex items-center gap-1.5"><ArrowUp10 className="h-3.5 w-3.5" />{t('logs.sortDesc')}</span>
                    </SelectItem>
                    <SelectItem value="asc" className="text-xs">
                      <span className="flex items-center gap-1.5"><ArrowDown10 className="h-3.5 w-3.5" />{t('logs.sortAsc')}</span>
                    </SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">{t('logs.limitLabel')}</Label>
                {/* 条数上限与请求日志共用同一组选项和状态（环形缓冲全量 500 行，前端截取） */}
                <Select value={limit} onValueChange={setLimit}>
                  <SelectTrigger className="w-full bg-background"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    {LIMIT_OPTIONS.map((n) => (
                      <SelectItem key={n} value={String(n)}>{fmtNumber(n)}</SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="col-span-2 space-y-1.5 md:col-span-1">
                <Label className="text-[11px] text-muted-foreground">{t('common.search')}</Label>
                {/* 内容搜索：输入即时过滤，无需按钮 */}
                <Input
                  value={sysQuery}
                  onChange={(e) => setSysQuery(e.target.value)}
                  placeholder={t('logs.searchPlaceholder')}
                  className="bg-background"
                />
              </div>
            </div>
            <p className="mt-2 text-[10px] leading-4 text-muted-foreground/70">
              {t('logs.systemNote')}
              {live ? ` ${t('logs.liveOnNote')}` : ` ${t('logs.liveOffNote')}`}
            </p>
          </section>

          <section className="overflow-hidden rounded-[20px] bg-muted">
            {sysFiltered.length ? (
              <div
                ref={logEndRef}
                onScroll={(e) => {
                  const el = e.currentTarget;
                  // 距底 24px 内算「钉在底部」（含 0：滚到底的一瞬也是钉底）
                  pinnedRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
                }}
                className="scroll-slim max-h-[70vh] overflow-auto p-4"
              >
                <pre className="whitespace-pre-wrap break-all font-mono text-[11px] leading-5 text-foreground/80">
                  {sysFiltered
                    .map((e) => `[${fmtLogTime(e.ts)}] [${e.ch}] ${e.text}`)
                    .join('\n')}
                </pre>
              </div>
            ) : sysLoading ? (
              <TableSkeleton rows={5} />
            ) : (
              <EmptyState
                icon={Terminal}
                title={t('logs.sysEmptyTitle')}
                description={t('logs.sysEmptyDesc')}
                className="flex flex-col items-center justify-center py-16 text-center"
              />
            )}
          </section>
        </TabsContent>
      </Tabs>

      {/* 请求日志详情抽屉 */}
      <Drawer open={!!detail} onOpenChange={(v) => !v && setDetail(null)}>
        <DrawerContent>
          <DrawerHeader>
            <DrawerTitle>{t('logs.detailTitle', {id: detail?.model ?? ''})}</DrawerTitle>
            <DrawerDescription>{detail ? fmtLogTime(detail.time) : ''}</DrawerDescription>
          </DrawerHeader>
          {detail && (
            <div className="space-y-3 px-4 pb-8 text-xs">
              {([
                ['uid', t('tasks.colAccount'), detail.uid || '—'],
                ['model', t('logs.rowModel'), detail.model || '—'],
                ['status', t('logs.rowStatus'), String(detail.status)],
                ['ttfb', t('logs.rowFirstToken'), detail.ttfb_ms ? fmtLatency(detail.ttfb_ms) : t('logs.notCollected')],
                ['tokens', 'Token', detail.tokens < 0 ? t('logs.notCollected') : fmtNumber(detail.tokens)],
                [
                  'credit',
                  t('logs.rowCredit'),
                  detail.has_credit
                    ? fmtCredit(detail.credit) + (detail.credit > 0 ? '' : t('logs.unbilled'))
                    : t('logs.noCredit'),
                ],
                ['error', t('logs.rowError'), detail.error || '—'],
              ] as [string, string, string][]).map(([id, k, v]) => {
                // 这些字段内容较长且常需要贴出来（排查 / 反馈），给出复制入口
                const copyable = ['uid', 'error'].includes(id) && v !== '—';
                return (
                  <div key={id} className="flex items-start gap-3">
                    <div className="w-32 shrink-0 text-muted-foreground">{k}</div>
                    <div className="min-w-0 flex-1 break-all font-mono">{v}</div>
                    {copyable && <CopyButton value={v} title={t('logs.copyField', {field: k})} className="-mt-1" />}
                  </div>
                );
              })}
            </div>
          )}
        </DrawerContent>
      </Drawer>
    </div>
  );
}
