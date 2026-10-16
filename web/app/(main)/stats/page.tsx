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
import {useCachedAsync} from '@/lib/data-cache';
import type {MetricsSnapshot, UsageKeyedAgg, UsageSnapshot} from '@/lib/types';
import {fmtCompact, fmtNumber, fmtCredit, localInputToSec} from '@/lib/format';
import {PageHeader} from '@/components/common/layout/PageHeader';
import {StatCard} from '@/components/common/layout/StatCard';
import {EmptyState} from '@/components/common/layout/EmptyState';
import {TableSkeleton} from '@/components/common/layout/LoadSkeleton';
import {Button} from '@/components/ui/button';
import {Input} from '@/components/ui/input';
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

/** Date → datetime-local 的取值（本地时间，不带时区，精确到分钟） */
function toLocalInput(d: Date): string {
  const p = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}

/** 自定义区间的预填值：最近 24 小时（整分钟）——一选中就有合法区间，图表不会先空一拍 */
function defaultCustomRange(): {from: string; to: string} {
  const to = new Date();
  to.setSeconds(0, 0);
  const from = new Date(to.getTime() - 24 * 3600 * 1000);
  return {from: toLocalInput(from), to: toLocalInput(to)};
}

export default function StatsPage() {
  // 统计是全局记录（Go 网关单实例双版本），但分桶/时序自带 realm 维度：
  // 卡片、趋势图、账号表、模型表全部按当前版本过滤后展示（无 realm 标注的历史存量按 cn 归属）；
  // 唯一保留全局口径的是原生 /v1/stats 卡片与其下表（重启即清的另一套数据）。
  const {realm, label: realmName} = useRealm();
  const t = useT();
  // 'all' = 自记录以来全量：按模型/按账号恢复全量排行，时序按日归并（后端契约 hours<=0）。
  // 'custom' = 自定义区间：起/止以 unix 秒传给 /api/usage（两端同时给出时后端优先于 hours）。
  const [hours, setHours] = useState<string>('72');
  const [fromInput, setFromInput] = useState('');
  const [toInput, setToInput] = useState('');
  /** 自定义区间里「已生效」的范围：只在输入构成合法区间时更新 */
  const [appliedRange, setAppliedRange] = useState<{from: number; to: number} | null>(null);
  const [saveBusy, setSaveBusy] = useState(false);

  // 自定义区间解析：缺一端或起 ≥ 止都算「还没定好」——此时图表继续停在
  // 上一次生效的区间（输入框下方给即时提示），不让半成品区间把图和表清空。
  const customRange = useMemo(() => {
    if (hours !== 'custom') return null;
    const from = localInputToSec(fromInput);
    const to = localInputToSec(toInput);
    if (from === undefined || to === undefined || from >= to) return null;
    return {from, to};
  }, [hours, fromInput, toInput]);

  useEffect(() => {
    if (customRange) setAppliedRange(customRange);
  }, [customRange]);

  const rangeInvalid = hours === 'custom' && !customRange;
  const effRange = hours === 'custom' ? customRange ?? appliedRange : null;

  // usage/native 都进缓存：切页先出上次的图表与卡片，后台静默刷新。
  // key 带时间窗（既有档位 hours 或自定义区间的两端）：切档 = 换一份快照，
  // 各自缓存互不覆盖——图与下面的表都挂在这份快照上，切换必然一起变。
  const usageCache = useCachedAsync<UsageSnapshot>(
    effRange ? `stats:usage:${effRange.from}-${effRange.to}` : `stats:usage:${hours}`,
    () =>
      effRange
        ? statsApi.usage({from: effRange.from, to: effRange.to})
        : statsApi.usage(hours === 'all' ? 'all' : Number(hours) || 72),
    {ttl: 5000},
  );
  const nativeCache = useCachedAsync<MetricsSnapshot>(
    'stats:native',
    () => statsApi.native(),
    {ttl: 5000},
  );
  // 「今日请求」卡专用快照：窗口恒为「本地日历日零点 → 现在」，与上面那份随选档
  // 变化的窗口快照完全独立。不能拿窗口快照聚合今天——用户查历史区间时窗口里根本
  // 没有今天的点，卡片会显示 0（看着像「今天没流量」），而同屏其它卡与表都在讲
  // 那个历史区间。key 不带时间戳：本地零点在会话内是常量，若把秒级 now 拼进 key，
  // key 每帧都变会触发无休止重取；to 在 fetcher 里现算，每轮心跳刷新都拿到最新「现在」。
  const todayCache = useCachedAsync<UsageSnapshot>(
    'stats:usage:today',
    () => {
      const now = new Date();
      const todayStart = new Date(now.getFullYear(), now.getMonth(), now.getDate(), 0, 0, 0);
      return statsApi.usage({
        from: Math.floor(todayStart.getTime() / 1000),
        to: Math.floor(now.getTime() / 1000),
      });
    },
    {ttl: 5000},
  );
  const usage = usageCache.data;
  const native = nativeCache.data;

  // 心跳同时刷三个缓存条目（用量持续累计，保持接近实时）。今日快照也要跟心跳：
  // 它不进图表/表格，靠这一轮轮刷新把「今日」窗口的右端一路推到当下。
  // 周期心跳失败静默（与 logs 页同设计）：60s 一轮的后台刷新弹错误雨毫无
  // 价值，等下一轮自愈；错误提示只留给用户手动操作（下方「立即落盘」）。
  useHeartbeat(
    () => {
      usageCache.refresh().catch(() => {/* 静默，等下一轮心跳 */});
      nativeCache.refresh().catch(() => {/* 静默，等下一轮心跳 */});
      todayCache.refresh().catch(() => {/* 静默，等下一轮心跳 */});
    },
    60000,
  );

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

  /** 切时间档；仅当起止输入都还空着（真正「首次切到」自定义区间）才预填最近 24 小时，
   *  保证一选中就有合法区间。已有输入一律保留——旧判据 `!customRange` 在进入本分支时
   *  恒为真（此时 hours 还不是 'custom'，customRange 必为 null），导致每次切回自定义
   *  都用默认区间覆盖用户上次填的起止，与「首次」的意图相反。 */
  function pickWindow(v: string) {
    if (v === 'custom' && !fromInput && !toInput) {
      const d = defaultCustomRange();
      setFromInput(d.from);
      setToInput(d.to);
    }
    setHours(v);
  }

  // 时序：小时点与日点混排（日点在前、小时点在后）；scope=hour 的 t 形如 2026-09-21T14。
  // label 留完整时间供 tooltip 用，axis 只在 XAxis 按 interval 抽样显示，窄屏不再挤成一团。
  const chartData = useMemo(() => {
    if (!usage) return [];
    // 先按当前版本过滤再出图：无 realm 标注 = 历史存量，按 cn 归属（与后端 Add() 回落口径一致）
    // ?? [] 兜 series 为 null：后端零桶时的历史契约（现已修复恒空数组，此处纵深防御）
    return (usage.series ?? [])
      .filter((p) => (p.realm ?? 'cn') === realm)
      .map((p) => {
        const isHour = p.scope === 'hour';
        return {
          label: isHour ? p.t.slice(5, 10) + ' ' + p.t.slice(11) + ':00' : p.t.slice(5),
          tokens: p.total_tokens,
          requests: p.requests,
          errors: p.errors,
        };
      });
  }, [usage, realm]);

  // Token（左轴）与请求数（右轴）量级不同：混在一张双轴图里比各自缩放更直观，
  // 也省掉「请求数柱子矮到看不见」的问题；失败数继续用虚线叠在右轴上。
  const chartHeight = 280;
  const chart = chartData.length ? (
    <ResponsiveContainer width="100%" height={chartHeight}>
      {/* 用 ComposedChart 而不是 BarChart：BarChart 会忽略非 Bar 子组件 */}
      <ComposedChart data={chartData} margin={{top: 8, right: 4, bottom: 0, left: 0}} barCategoryGap="20%">
        <defs>
          {/* 柱体纵向渐变 + 顶部圆角：纯色平涂在深色主题下发闷，渐变让趋势的「形状」更突出 */}
          <linearGradient id="tokenBarGradient" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor="var(--chart-1)" stopOpacity={0.95} />
            <stop offset="100%" stopColor="var(--chart-1)" stopOpacity={0.45} />
          </linearGradient>
        </defs>
        <CartesianGrid strokeDasharray="3 3" stroke="var(--border)" vertical={false} />
        {/* X 轴按像素密度自动抽样标签（minTickGap），整点时间不再重叠；preserveStartEnd 保证首尾时间可见 */}
        <XAxis
          dataKey="label"
          tickLine={false}
          axisLine={false}
          fontSize={11}
          minTickGap={28}
          stroke="var(--muted-foreground)"
        />
        <YAxis
          yAxisId="tokens"
          tickLine={false}
          axisLine={false}
          width={60}
          fontSize={11}
          stroke="var(--muted-foreground)"
          tickFormatter={(v) => fmtCompact(Number(v))}
        />
        <YAxis
          yAxisId="requests"
          orientation="right"
          tickLine={false}
          axisLine={false}
          width={40}
          fontSize={11}
          stroke="var(--muted-foreground)"
          tickFormatter={(v) => fmtCompact(Number(v))}
        />
        <Tooltip
          cursor={{fill: 'var(--accent)'}}
          contentStyle={{
            background: 'var(--popover)',
            border: '1px solid var(--border)',
            borderRadius: 12,
            fontSize: 12,
          }}
          labelFormatter={(label) => String(label)}
          formatter={(value, name) => {
            const key = String(name);
            if (key === 'tokens') return [fmtNumber(Number(value)), t('stats.legendTokens')];
            if (key === 'errors') return [fmtNumber(Number(value)), t('stats.legendErrors')];
            return [fmtNumber(Number(value)), t('stats.legendRequests')];
          }}
        />
        <Bar
          yAxisId="tokens"
          dataKey="tokens"
          name="tokens"
          fill="url(#tokenBarGradient)"
          radius={[4, 4, 0, 0]}
          /* 限制柱宽：只有一两天数据时，柱子不会被拉伸占满整个图表 */
          maxBarSize={48}
        />
        <Line
          yAxisId="requests"
          type="monotone"
          dataKey="requests"
          name="requests"
          stroke="var(--chart-2)"
          strokeWidth={1.5}
          dot={false}
        />
        {/* 失败数：与请求数同轴（同为次数），红色虚线只作「那时出过事」的信号，具体数值看悬停 */}
        <Line
          yAxisId="requests"
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
  ) : null;

  const hasErrors = useMemo(() => chartData.some((p) => p.errors > 0), [chartData]);

  /** 今日用量：先按当前版本过滤，再聚合今天的全部小时点；无小时点时回退今天的日点。
   *  数据源是上面那份「今日」专用快照（todayCache），不是当前窗口快照：卡片口径恒定是
   *  真实今日，用户选历史区间时不会因为窗口里没有今天的点而误报 0。聚合与派生口径
   *  逐字沿用（小时点优先、无小时点回退日点），只换数据源。 */
  const todayUsage = useMemo(() => {
    const snap = todayCache.data;
    if (!snap) return {requests: 0, tokens: 0, errors: 0};
    const now = new Date();
    const todayKey = `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, '0')}-${String(now.getDate()).padStart(2, '0')}`;
    const realmSeries = (snap.series ?? []).filter((p) => (p.realm ?? 'cn') === realm);
    const hourPoints = realmSeries.filter((p) => p.scope === 'hour' && p.t.startsWith(todayKey));
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
    const todayDay = realmSeries.find((p) => p.scope === 'day' && p.t === todayKey);
    return todayDay
      ? {requests: todayDay.requests, tokens: todayDay.total_tokens, errors: todayDay.errors}
      : {requests: 0, tokens: 0, errors: 0};
  }, [todayCache.data, realm]);

  // 域内汇总：totals 是全局口径无法按域拆，改取 by_realm 当前域那一行
  //（key 恒为 'cn'|'global'；realm 字段为空串的历史行用 key 兜底匹配）。
  const realmAgg = (usage?.by_realm ?? []).find((r) => (r.key || r.realm) === realm);

  /**
   * 模型行按当前版本过滤：后端 ByModel 按 (realm, model) 拆分并带 realm 标注，
   * 无标注只可能是历史存量（新数据恒有标注，后端 Add() 把空 realm 回落为 cn），
   * 归入 cn——同一裸模型名在双域是两行，不按版本过滤会重复展示。
   */
  const byModel = (usage?.by_model ?? []).filter((it) => (it.realm ?? 'cn') === realm);
  // 账号行同样按当前版本过滤（后端 ByAccount 按 (realm, account) 拆分并带标注），
  // 不再作全局对照；行内 realm 小标注保留。
  const byAccount = (usage?.by_account ?? []).filter((it) => (it.realm ?? 'cn') === realm);

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader
        title={t('stats.title')}
        description={t('stats.description', {realm: realmName})}
        actions={
          <>
            <Select value={hours} onValueChange={pickWindow}>
              <SelectTrigger className="h-8 w-[150px] rounded-full"><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value="24">{t('stats.hours24')}</SelectItem>
                <SelectItem value="72">{t('stats.hours72')}</SelectItem>
                <SelectItem value="168">{t('stats.days7')}</SelectItem>
                <SelectItem value="720">{t('stats.days30')}</SelectItem>
                <SelectItem value="all">{t('stats.sinceStart')}</SelectItem>
                <SelectItem value="custom">{t('stats.customRange')}</SelectItem>
              </SelectContent>
            </Select>
            {/* 自定义区间：起/止（本地时间），换算成 unix 秒传给后端；
                切换行为与既有档位一致——图与下面的表一起变 */}
            {hours === 'custom' && (
              <div className="flex items-center gap-1.5">
                <Input
                  type="datetime-local"
                  value={fromInput}
                  aria-label={t('stats.rangeFrom')}
                  onChange={(e) => setFromInput(e.target.value)}
                  className="h-8 w-[11.5rem] rounded-full bg-muted text-xs"
                />
                <span className="text-xs text-muted-foreground">→</span>
                <Input
                  type="datetime-local"
                  value={toInput}
                  aria-label={t('stats.rangeTo')}
                  onChange={(e) => setToInput(e.target.value)}
                  className="h-8 w-[11.5rem] rounded-full bg-muted text-xs"
                />
              </div>
            )}
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
          value={fmtNumber(realmAgg?.requests ?? 0)}
          hint={t('stats.tokenHint', {v: fmtCompact(realmAgg?.total_tokens ?? 0)})}
          icon={TrendingUp}
          tone="accent"
          delay={0.04}
        />
        <StatCard
          label={t('stats.avgLatency')}
          value={
            realmAgg?.avg_latency_ms
              ? fmtCompact(Math.round(realmAgg.avg_latency_ms)) + 'ms'
              : '—'
          }
          hint={
            realmAgg?.avg_tokens_per_second
              ? t('stats.tpsHint', {v: realmAgg.avg_tokens_per_second.toFixed(1)})
              : undefined
          }
          icon={Cpu}
          tone="neutral"
          delay={0.08}
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
          delay={0.12}
        />
      </section>

      <section className="rounded-[20px] bg-muted p-4">
        <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
          <div className="text-sm font-medium">{t('stats.tokenTrend')}</div>
          <div className="text-[11px] text-muted-foreground">
            {t('stats.seriesNote', {
              hours: fmtNumber((usage?.series ?? []).filter((p) => p.scope === 'hour').length ?? 0),
            })}
          </div>
        </div>
        {/* 区间还没填好：图表停在上一段有效区间，这里说明「为什么没变」 */}
        {rangeInvalid && (
          <div className="mb-2 rounded-xl bg-amber-500/10 px-3 py-1.5 text-[11px] text-amber-600 dark:text-amber-400">
            {t('stats.customInvalid')}
          </div>
        )}
        {chart}
        {!chart && (
          // 占位高度与图表等高（280px）：数据到达后不再有 160px 的高度跳变。
          // usage 未到（首载）用脉动骨架占位，明确「在加载」；usage 已到但确实
          // 无数据时保留原来的文案空态——两种情形不能混同，否则像「没有数据」。
          (usage ? (
            <div className="grid h-[280px] place-items-center text-xs text-muted-foreground">{t('stats.noUsageData')}</div>
          ) : (
            <div className="h-[280px]" aria-hidden>
              <div className="mt-1 h-full w-full animate-pulse rounded-xl bg-background/60" />
            </div>
          ))
        )}
        {/* 图例自绘（recharts 默认图例在窄屏会换行错位）：线样与图上一致 */}
        <div className="mt-2 flex flex-wrap items-center justify-center gap-x-4 gap-y-1 text-[11px] text-muted-foreground">
          <span className="inline-flex items-center gap-1.5">
            <span className="h-2 w-3 rounded-sm" style={{background: 'var(--chart-1)'}} />
            {t('stats.legendTokens')}
          </span>
          <span className="inline-flex items-center gap-1.5">
            <span className="inline-block h-0 w-3 border-t-2" style={{borderColor: 'var(--chart-2)'}} />
            {t('stats.legendRequests')}
          </span>
          {hasErrors && (
            <span className="inline-flex items-center gap-1.5">
              <span
                className="inline-block h-0 w-3 border-t-2 border-dashed"
                style={{borderColor: 'var(--destructive)'}}
              />
              {t('stats.legendErrors')}
            </span>
          )}
        </div>
      </section>

      <section className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <BreakdownPanel
          title={t('stats.byModel')}
          icon={Cpu}
          items={byModel}
          loading={!usage}
        />
        <BreakdownPanel
          title={t('stats.byAccount')}
          icon={Users}
          items={byAccount}
          loading={!usage}
        />
      </section>

      {/* 原生 /v1/stats：按模型聚合（重启即清，长期趋势看上面的分桶） */}
      {!!native?.models?.length && (
        <section className="rounded-[20px] bg-muted p-4">
          <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
            <div className="text-sm font-medium">{t('stats.nativeByModel')}</div>
            {/* 该区（含上面的原生卡片）是页面里唯一保留全局口径的数据：无 realm 维度 */}
            <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-muted-foreground">
              <span>{t('stats.globalNote')}</span>
              <span>{t('stats.nativeNote')}</span>
            </div>
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
  loading = false,
}: {
  title: string;
  icon: typeof Cpu;
  items: UsageKeyedAgg[];
  /** 首次加载（缓存未命中）：展示与表格同节奏的骨架行，而不是先出空态再跳表格 */
  loading?: boolean;
}) {
  const t = useT();
  const max = Math.max(1, ...items.map((i) => i.total_tokens));
  return (
    <div className="rounded-[20px] bg-muted p-4">
      <div className="mb-3 flex items-center gap-2 text-sm font-medium">
        <Icon className="h-4 w-4" />
        {title}
      </div>
      {loading ? (
        <TableSkeleton rows={4} className="py-2" />
      ) : items.length ? (
        <Table>
          <TableHeader>
            <TableRow className="border-b border-border/60 hover:bg-transparent">
              <TableHead className="pl-0 text-[11px] text-muted-foreground">{t('metric.name')}</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">{t('metric.requestsShort')}</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">Token</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">{t('stats.creditCol')}</TableHead>
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
                {/* 积分列：实测扣费（credit_samples>0 才有观测，缺失≠0 显示 —），
                    行内小字标注单价「积分/1M Token」（配对 token 折算） */}
                <TableCell className="text-xs tabular-nums">
                  {(it.credit_samples ?? 0) > 0 ? (
                    <>
                      {fmtCredit(it.credit)}
                      {(it.credits_per_1m_tokens ?? 0) > 0 && (
                        <div className="text-[10px] text-muted-foreground">
                          {fmtCredit(it.credits_per_1m_tokens)}/1M
                        </div>
                      )}
                    </>
                  ) : (
                    <span className="text-muted-foreground/70">—</span>
                  )}
                </TableCell>
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
