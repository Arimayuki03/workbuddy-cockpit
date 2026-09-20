'use client';

import {useCallback, useEffect, useMemo, useState} from 'react';
import {Activity, TrendingUp, Cpu, Users, Coins} from 'lucide-react';
import {
  Bar,
  CartesianGrid,
  ComposedChart,
  Line,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';
import {useHeartbeat} from '@/lib/use-heartbeat';
import {statsApi, errText} from '@/lib/api';
import type {MetricsSnapshot, UsageKeyedAgg, UsageSnapshot} from '@/lib/types';
import {fmtCompact, fmtNumber, fmtCredit} from '@/lib/format';
import {PageHeader} from '@/components/common/layout/PageHeader';
import {StatCard} from '@/components/common/layout/StatCard';
import {EmptyState} from '@/components/common/layout/EmptyState';
import {Button} from '@/components/ui/button';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import {notify} from '@/lib/toast';
import {useT} from '@/lib/i18n/provider';
import {useRealm} from '@/lib/realm-context';

const CHART_COLORS = [
  'var(--chart-1)',
  'var(--chart-2)',
  'var(--chart-3)',
  'var(--chart-4)',
  'var(--chart-5)',
];

export default function StatsPage() {
  // 统计是全局记录（Go 网关单实例双版本），不随 realm 过滤；usage 分桶自带 realm 维度
  const {label: realmName} = useRealm();
  const t = useT();
  const [usage, setUsage] = useState<UsageSnapshot | null>(null);
  const [native, setNative] = useState<MetricsSnapshot | null>(null);
  const [hours, setHours] = useState('72');
  const [saveBusy, setSaveBusy] = useState(false);
  const load = useCallback(async () => {
    const results = await Promise.allSettled([
      statsApi.usage(Number(hours) || 72),
      statsApi.native(),
    ]);
    if (results[0].status === 'fulfilled') setUsage(results[0].value);
    if (results[1].status === 'fulfilled') setNative(results[1].value);
    if (results.every((r) => r.status === 'rejected')) {
      notify.err(errText((results[0] as PromiseRejectedResult).reason));
    }
  }, [hours]);

  useEffect(() => {
    load();
  }, [load]);

  // 用量随调用持续累计，心跳刷新让页面保持接近实时
  useHeartbeat(load, 60000);

  /** 立即落盘（正常由后台 30s 防抖负责） */
  const saveNow = useCallback(async () => {
    setSaveBusy(true);
    try {
      await statsApi.saveUsage();
      notify.ok(t('stats.savedToDisk'));
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setSaveBusy(false);
    }
  }, [t]);

  // 时序：小时点与日点混排（日点在前、小时点在后）；scope=hour 的 t 形如 2026-09-21T14
  const chartData = useMemo(() => {
    if (!usage) return [];
    return usage.series.map((p) => ({
      day: p.scope === 'hour'
        ? (p.t.slice(5, 10) + ' ' + p.t.slice(11) + ':00')
        : p.t.slice(5),
      tokens: p.total_tokens,
      requests: p.requests,
      errors: p.errors,
    }));
  }, [usage]);

  /** 今日用量：今天的全部小时点聚合；无小时点时回退今天的日点 */
  const todayUsage = useMemo(() => {
    if (!usage) return {requests: 0, tokens: 0, errors: 0};
    const now = new Date();
    const todayKey = `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, '0')}-${String(now.getDate()).padStart(2, '0')}`;
    const hourPoints = usage.series.filter((p) => p.scope === 'hour' && p.t.startsWith(todayKey));
    if (hourPoints.length) {
      return hourPoints.reduce(
        (acc, p) => ({
          requests: acc.requests + p.requests,
          tokens: acc.tokens + p.total_tokens,
          errors: acc.errors + p.errors,
        }),
        {requests: 0, tokens: 0, errors: 0},
      );
    }
    const todayDay = usage.series.find((p) => p.scope === 'day' && p.t === todayKey);
    return todayDay
      ? {requests: todayDay.requests, tokens: todayDay.total_tokens, errors: todayDay.errors}
      : {requests: 0, tokens: 0, errors: 0};
  }, [usage]);

  const byModel = usage?.by_model ?? [];
  const byAccount = usage?.by_account ?? [];

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader
        title={t('stats.title')}
        description={t('stats.description', {realm: realmName})}
        actions={
          <>
            <Select value={hours} onValueChange={setHours}>
              <SelectTrigger className="h-8 w-[150px] rounded-full"><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value="24">{t('stats.hours24')}</SelectItem>
                <SelectItem value="72">{t('stats.hours72')}</SelectItem>
                <SelectItem value="168">{t('stats.days7')}</SelectItem>
                <SelectItem value="720">{t('stats.days30')}</SelectItem>
              </SelectContent>
            </Select>
            <Button
              variant="outline"
              size="sm"
              className="rounded-full"
              disabled={saveBusy}
              onClick={saveNow}
            >
              <Coins className={saveBusy ? 'animate-pulse' : ''} />
              {t('stats.saveNow')}
            </Button>
          </>
        }
      />

      <section className="grid grid-cols-2 gap-3 lg:grid-cols-4 md:gap-4">
        <StatCard
          label={t('stats.todayRequests')}
          value={fmtNumber(todayUsage.requests)}
          hint={
            todayUsage.errors > 0
              ? t('stats.failedHint', {n: fmtNumber(todayUsage.errors)})
              : t('stats.tokenHint', {v: fmtCompact(todayUsage.tokens)})
          }
          icon={Activity}
          tone={todayUsage.errors > 0 ? 'warning' : 'info'}
          hintTone={todayUsage.errors > 0 ? 'warning' : undefined}
          delay={0}
        />
        <StatCard
          label={t('stats.totalRequests')}
          value={fmtNumber(usage?.totals.requests ?? 0)}
          hint={t('stats.tokenHint', {v: fmtCompact(usage?.totals.total_tokens ?? 0)})}
          icon={TrendingUp}
          tone="accent"
          delay={0.05}
        />
        <StatCard
          label={t('stats.avgLatency')}
          value={
            usage?.totals.avg_latency_ms
              ? fmtCompact(Math.round(usage.totals.avg_latency_ms)) + 'ms'
              : '—'
          }
          hint={
            usage?.totals.avg_tokens_per_second
              ? t('stats.tpsHint', {v: usage.totals.avg_tokens_per_second.toFixed(1)})
              : undefined
          }
          icon={Cpu}
          tone="neutral"
          delay={0.1}
        />
        <StatCard
          label={t('stats.nativeTotal')}
          value={fmtNumber(native?.total.requests ?? 0)}
          hint={
            native?.total.total_tokens
              ? t('stats.tokenHint', {v: fmtCompact(native.total.total_tokens)})
              : t('stats.nativeSince', {time: native ? native.since.slice(0, 16).replace('T', ' ') : '—'})
          }
          icon={Activity}
          tone="success"
          delay={0.15}
        />
      </section>

      <section className="rounded-[20px] bg-muted p-4">
        <div className="mb-3 flex items-center justify-between">
          <div className="text-sm font-medium">{t('stats.tokenTrend')}</div>
          <div className="text-[11px] text-muted-foreground">
            {t('stats.seriesNote', {
              hours: fmtNumber(usage?.series.filter((p) => p.scope === 'hour').length ?? 0),
            })}
          </div>
        </div>
        <div className="h-[260px] w-full">
          {chartData.length ? (
            <ResponsiveContainer width="100%" height="100%">
              {/* 用 ComposedChart 而不是 BarChart：BarChart 会忽略非 Bar 子组件 */}
              <ComposedChart data={chartData} margin={{top: 4, right: 8, bottom: 0, left: -8}} barCategoryGap="20%">
                <CartesianGrid strokeDasharray="3 3" stroke="var(--border)" vertical={false} />
                <XAxis dataKey="day" tickLine={false} axisLine={false} fontSize={11} stroke="var(--muted-foreground)" />
                <YAxis tickLine={false} axisLine={false} fontSize={11} stroke="var(--muted-foreground)" tickFormatter={(v) => fmtCompact(Number(v))} />
                <Tooltip
                  cursor={{fill: 'var(--accent)'}}
                  contentStyle={{
                    background: 'var(--popover)',
                    border: '1px solid var(--border)',
                    borderRadius: 12,
                    fontSize: 12,
                  }}
                  formatter={(value, name) => {
                    const label = String(name);
                    if (label === 'tokens') return [fmtNumber(Number(value)), 'Token'];
                    if (label === 'errors') {
                      return [fmtNumber(Number(value)), t('dashboard.failedRequests')];
                    }
                    return [fmtNumber(Number(value)), t('metric.requests')];
                  }}
                />
                <Bar
                  dataKey="tokens"
                  name="tokens"
                  fill="var(--chart-1)"
                  radius={[4, 4, 0, 0]}
                  /* 限制柱宽：只有一两天数据时，柱子不会被拉伸占满整个图表 */
                  maxBarSize={48}
                />
                {/* 失败数用一条线叠在同一张图上：它与 token 柱不同量级，做成柱子
                    会把柱形压扁。线只作「那天出过事」的信号，具体数值看悬停。 */}
                <Line
                  type="monotone"
                  dataKey="errors"
                  name="errors"
                  stroke="var(--destructive)"
                  strokeWidth={1.5}
                  strokeDasharray="4 3"
                  dot={false}
                />
              </ComposedChart>
            </ResponsiveContainer>
          ) : (
            <div className="grid h-full place-items-center text-xs text-muted-foreground">{t('stats.noUsageData')}</div>
          )}
        </div>
      </section>

      <section className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <BreakdownPanel
          title={t('stats.byModel')}
          icon={Cpu}
          items={byModel}
        />
        <BreakdownPanel
          title={t('stats.byAccount')}
          icon={Users}
          items={byAccount}
        />
      </section>

      {/* 原生 /v1/stats：按模型聚合（重启即清，长期趋势看上面的分桶） */}
      {!!native?.models?.length && (
        <section className="rounded-[20px] bg-muted p-4">
          <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
            <div className="text-sm font-medium">{t('stats.nativeByModel')}</div>
            <div className="text-[11px] text-muted-foreground">{t('stats.nativeNote')}</div>
          </div>
          <Table>
            <TableHeader>
              <TableRow className="border-b border-border/60 hover:bg-transparent">
                <TableHead className="pl-0 text-[11px] text-muted-foreground">{t('metric.name')}</TableHead>
                <TableHead className="text-[11px] text-muted-foreground">{t('metric.requestsShort')}</TableHead>
                <TableHead className="text-[11px] text-muted-foreground">{t('logs.colFirstToken')}</TableHead>
                <TableHead className="text-[11px] text-muted-foreground">Token</TableHead>
                <TableHead className="text-[11px] text-muted-foreground">{t('stats.cacheHit')}</TableHead>
                <TableHead className="pr-0 text-[11px] text-muted-foreground">{t('metric.paid')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {native.models.slice(0, 20).map((m) => (
                <TableRow key={m.model} className="border-b border-border/40">
                  <TableCell className="pl-0 font-mono text-xs">{m.model}</TableCell>
                  <TableCell className="text-xs tabular-nums">
                    {fmtNumber(m.requests)}
                    {m.failed > 0 && (
                      <span className="ml-1 text-[10px] text-red-600 dark:text-red-400">-{fmtNumber(m.failed)}</span>
                    )}
                  </TableCell>
                  <TableCell className="text-xs tabular-nums text-muted-foreground">
                    {m.avg_ttfb_ms ? Math.round(m.avg_ttfb_ms) + 'ms' : '—'}
                  </TableCell>
                  <TableCell className="text-xs tabular-nums">{fmtCompact(m.total_tokens)}</TableCell>
                  <TableCell className="text-xs tabular-nums">
                    {(m.cache_hit_rate * 100).toFixed(1)}%
                  </TableCell>
                  <TableCell className="pr-0 text-xs tabular-nums">
                    {m.credit > 0 ? fmtCredit(m.credit) : <span className="text-muted-foreground/70">—</span>}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </section>
      )}
    </div>
  );
}

function BreakdownPanel({
  title,
  icon: Icon,
  items,
}: {
  title: string;
  icon: typeof Cpu;
  items: UsageKeyedAgg[];
}) {
  const t = useT();
  const max = Math.max(1, ...items.map((i) => i.total_tokens));
  return (
    <div className="rounded-[20px] bg-muted p-4">
      <div className="mb-3 flex items-center gap-2 text-sm font-medium">
        <Icon className="h-4 w-4" />
        {title}
      </div>
      {items.length ? (
        <Table>
          <TableHeader>
            <TableRow className="border-b border-border/60 hover:bg-transparent">
              <TableHead className="pl-0 text-[11px] text-muted-foreground">{t('metric.name')}</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">{t('metric.requestsShort')}</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">Token</TableHead>
              <TableHead className="pr-0 text-[11px] text-muted-foreground">{t('stats.avgLatency')}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {items.map((it, i) => (
              <TableRow key={`${it.key}-${i}`} className="border-b border-border/40">
                <TableCell className="pl-0">
                  <div className="flex items-center gap-2">
                    <span className="h-2 w-2 rounded-full" style={{background: CHART_COLORS[i % CHART_COLORS.length]}} />
                    <span className="max-w-[140px] truncate text-xs font-medium">
                      {it.extra || it.key || t('metric.unknown')}
                      {it.realm && (
                        <span className="ml-1 text-[10px] text-muted-foreground">{it.realm}</span>
                      )}
                    </span>
                  </div>
                  <div className="mt-1.5 h-1 w-full max-w-[140px] overflow-hidden rounded-full bg-border">
                    <div
                      className="h-full rounded-full"
                      style={{
                        width: `${(it.total_tokens / max) * 100}%`,
                        background: CHART_COLORS[i % CHART_COLORS.length],
                      }}
                    />
                  </div>
                </TableCell>
                <TableCell className="text-xs tabular-nums">
                  {fmtNumber(it.requests)}
                  {it.errors > 0 && (
                    <span className="ml-1 text-[10px] text-red-600 dark:text-red-400">-{fmtNumber(it.errors)}</span>
                  )}
                </TableCell>
                <TableCell className="text-xs tabular-nums">{fmtCompact(it.total_tokens)}</TableCell>
                <TableCell className="pr-0 text-xs tabular-nums text-muted-foreground">
                  {it.avg_latency_ms ? Math.round(it.avg_latency_ms) + 'ms' : '—'}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      ) : (
        <EmptyState
          icon={Activity}
          title={t('stats.emptyTitle')}
          description={t('stats.emptyDesc')}
          className="flex flex-col items-center justify-center py-10 text-center"
        />
      )}
    </div>
  );
}
