'use client';

import {useCallback, useEffect, useMemo, useState} from 'react';
import {Users, CircleCheck, TriangleAlert, Activity, Server, Coins} from 'lucide-react';
import {
  Area,
  AreaChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';
import {useHeartbeat} from '@/lib/use-heartbeat';
import {accountApi, errText, statsApi} from '@/lib/api';
import {useCachedAsync} from '@/lib/data-cache';
import {useRealm} from '@/lib/realm-context';
import type {
  Account,
  OverviewResponse,
  PackagesResponse,
  UpstreamStatus,
  UsageSnapshot,
} from '@/lib/types';
import {
  fmtCompact,
  fmtDateTime,
  fmtNumber,
} from '@/lib/format';
import {
  availabilityClass,
  availabilityLabelKey,
  availabilityOf,
  availabilityTitleKey,
  isDegraded,
  type AvailabilityTier,
} from '@/lib/account-status';
import {PageHeader} from '@/components/common/layout/PageHeader';
import {StatCard} from '@/components/common/layout/StatCard';
import {EmptyState} from '@/components/common/layout/EmptyState';
import {CardRowsSkeleton} from '@/components/common/layout/LoadSkeleton';
import {Badge} from '@/components/ui/badge';
import {useT} from '@/lib/i18n/provider';
import {notify} from '@/lib/toast';

export default function DashboardPage() {
  const {realm, label: realmName} = useRealm();
  const t = useT();
  // 到期积分按天归并（浏览器本地偏好，默认关闭——升级后看到的是原口径）。
  // 首帧与 SSR 对齐（false），水合后再从 localStorage 回填，避免 hydration mismatch
  // 与隐私模式 SecurityError（realm-context / i18n provider 同款约定）。
  const [expiryDailyMerge, setExpiryDailyMerge] = useState(false);
  useEffect(() => {
    try {
      if (window.localStorage.getItem('expiryDailyMerge') === '1') {
        setExpiryDailyMerge(true);
      }
    } catch {/* 隐私模式等存储不可用：保持默认关闭 */}
  }, []);
  const toggleExpiryDailyMerge = useCallback((on: boolean) => {
    setExpiryDailyMerge(on);
    try {
      window.localStorage.setItem('expiryDailyMerge', on ? '1' : '0');
    } catch {/* 隐私模式等存储不可用：本次会话内仍生效 */}
  }, []);
  // overview（池快照，快）与 packages（逐号查上游，慢）分两个缓存条目并行拉：
  // 快的先渲染卡片骨架外的东西，慢的（积分）拿到后再补上——切页先出缓存值，
  // 后台刷新静默替换，不再出现「整页空 1-2 秒」。
  const overviewCache = useCachedAsync<OverviewResponse>(
    'overview',
    () => accountApi.overview(),
    {ttl: 5000},
  );
  const packagesCache = useCachedAsync<PackagesResponse>(
    'packages',
    () => accountApi.packages(),
    {ttl: 5000},
  );
  const [usage, setUsage] = useState<UsageSnapshot | null>(null);
  const [upstream, setUpstream] = useState<UpstreamStatus | null>(null);

  const overview = overviewCache.data;
  const packages = packagesCache.data;

  // 切换版本后要重新取积分：两个版本的账号池不同，credits 也不能混
  //
  // 实时积分与最近到期时间都从缓存数据**渲染期派生**：心跳/后台刷新写入缓存
  // 后组件重渲染，这里自然跟随——没有独立 setState 时序，也不会出现
  // 「先渲染池快照值、约 1 秒后被实时值覆盖」的闪变。
  const liveCredits: Record<string, number> = {};
  // 已用积分（packages 查到才有）：供积分卡片 hint 展示「已用合计」。
  const liveUsed: Record<string, number> = {};
  // 到期积分明细（按时间升序收集，供「按天归并」与最近一笔两种口径共用）。
  const expiries: {at: number; amount: number}[] = [];
  if (packages) {
    for (const row of packages.accounts) {
      if (typeof row.remain === 'number' && !row.error) liveCredits[row.uid] = row.remain;
      if (typeof row.used === 'number' && !row.error) liveUsed[row.uid] = row.used;
      for (const p of row.packages ?? []) {
        if (!p.end_time) continue;
        const at = Date.parse(p.end_time);
        if (!Number.isFinite(at) || p.remain <= 0) continue;
        expiries.push({at, amount: p.remain});
      }
    }
  }
  expiries.sort((a, b) => a.at - b.at);
  // 到期积分按天归并（吸收 workbuddy-manager「到期积分按天模糊统计」思路）：
  // 同一**本地日历日**到期的多笔合并成一笔（金额求和、时刻取当天最早——宁保守，
  // 免得按"还有 12 天"安排、实际当天凌晨就作废）。浏览器本地偏好，不进配置。
  const expiryDayKey = (at: number) => {
    const d = new Date(at);
    return `${d.getFullYear()}-${d.getMonth()}-${d.getDate()}`;
  };
  const mergedExpiries: {at: number; amount: number}[] = [];
  if (expiryDailyMerge) {
    const byDay = new Map<string, {at: number; amount: number}>();
    for (const e of expiries) {
      const key = expiryDayKey(e.at);
      const cur = byDay.get(key);
      if (cur) {
        cur.amount += e.amount;
        cur.at = Math.min(cur.at, e.at);
      } else {
        byDay.set(key, {...e});
      }
    }
    mergedExpiries.push(...Array.from(byDay.values()).sort((a, b) => a.at - b.at));
  } else {
    mergedExpiries.push(...expiries);
  }
  const nextExpiry: {at: number; amount: number} | null = mergedExpiries[0] ?? null;

  // 上游健康与用量时序：这两个端点都很快，不进缓存，保持原有的一次性拉取。
  // 心跳沿用原 load：同时刷上游状态、usage 与两个缓存条目，全部数据同帧续命。
  // silent=true（心跳路径）时失败不弹错：30s 一轮的后台刷新弹错误雨毫无价值，
  // 等下一轮自愈即可；错误提示只留给手动路径（本页无手动按钮，仅首载 effect）。
  const load = useCallback(
    async (silent = false) => {
      const results = await Promise.allSettled([
        accountApi.status(),
        statsApi.usage(168),
        overviewCache.refresh(),
        packagesCache.refresh(),
      ]);
      if (results[0].status === 'fulfilled') setUpstream(results[0].value);
      if (results[1].status === 'fulfilled') setUsage(results[1].value);
      if (!silent && results.some((r) => r.status === 'rejected')) {
        const failed = results.find((r) => r.status === 'rejected');
        notify.err(errText((failed as PromiseRejectedResult).reason));
      }
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [overviewCache.refresh, packagesCache.refresh],
  );

  useEffect(() => {
    load();
  }, [load]);

  // 账号健康度与用量持续变化：心跳静默刷新全部数据源（含两个缓存条目）
  useHeartbeat(() => {
    void load(true);
  }, 30000);

  /**
   * 池快照按当前版本过滤。
   * Go 侧单实例双版本共存，账号按自身 realm 路由；存量数据无 realm 字段时视为 cn。
   */
  const scoped = useMemo(
    () => (overview?.accounts ?? []).filter((a) => (a.realm ?? 'cn') === realm),
    [overview, realm],
  );

  /** 可用性分档的汇总（只统计「能正常调用」的，与账号页说法一致） */
  const availability = useMemo(() => {
    const counts: Record<AvailabilityTier, number> = {
      disabled: 0, manualDisabled: 0, unknown: 0, cooling: 0, neverSucceeded: 0, online: 0,
    };
    for (const a of scoped) counts[availabilityOf(a)] += 1;
    return counts;
  }, [scoped]);

  /**
   * 「不可用」的账号数：冷却中 / 一直失败——这些是**用户需要处理**的。
   * unknown（读不到池状态）与 manualDisabled（主动停用）不计入：
   * 前者是我们看不到，后者是用户自己的决定。
   */
  const unusable = availability.cooling + availability.neverSucceeded;

  /**
   * 连败降权计数。上游把它并进 cooling，所以「冷却中」是个混数：既有等一会儿
   * 就好的限流退避，也有「这个号在持续失败」的降权。这里从账号明细单独数一份。
   */
  const degraded = scoped.filter(isDegraded).length;

  /** 池计数按当前版本（/status 的 realm_totals；缺失时从账号明细现数同口径回落） */
  const pool = useMemo(() => {
    const perRealm = upstream?.realm_totals?.[realm];
    if (perRealm) {
      return {known: true as const, ...perRealm};
    }
    // overview 顶层五元组是全池双 realm 计数，直接用会把另一个版本的号算进来。
    // cooling 从 scoped 账号明细现数（Account.cooling 由后端按 until/breaker/
    // degrade 三截止置位，与 CountsDetailed 同口径）；total/healthy/disabled 无法
    // 从明细重建同口径计数，只能保留全池回落——known=false 的展示路径已标注「—」。
    return {
      total: overview?.total ?? 0,
      healthy: overview?.healthy ?? 0,
      cooling: scoped.filter((a) => a.cooling).length,
      disabled: overview?.disabled ?? 0,
      known: false as const,
    };
  }, [upstream, overview, realm, scoped]);

  /**
   * series 点按当前版本过滤。桶在后端按 (realm, scope) 聚合并带 realm 标注：
   * 有标注按标注过滤；无标注只可能是历史存量（新数据恒有标注，后端 Add()
   * 已把空 realm 回落为 cn），归入 cn——不能两边都算，否则双版本重复计数。
   */
  const realmSeries = useMemo(
    () => (usage?.series ?? []).filter((p) => (p.realm ?? 'cn') === realm),
    [usage, realm],
  );

  // 时序图：近 14 个日点；今天已有小时点时改用逐小时点（更细）。
  // panel 的 series 是「日点升序 + 小时点升序」拼成的连续时序。
  const chartData = useMemo(() => {
    if (!usage) return [];
    const now = new Date();
    const todayKey = `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, '0')}-${String(now.getDate()).padStart(2, '0')}`;
    const dayPoints = realmSeries.filter((p) => p.scope === 'day').slice(-14);
    const dayData = dayPoints.map((p) => ({
      day: p.t.slice(5),
      requests: p.requests,
      tokens: p.total_tokens,
    }));
    const hourPoints = realmSeries.filter(
      (p) => p.scope === 'hour' && p.t.startsWith(todayKey),
    );
    if (!hourPoints.length) return dayData;
    const todayHourly = hourPoints.map((p) => ({
      day: p.t.slice(11) + ':00',
      requests: p.requests,
      tokens: p.total_tokens,
    }));
    return [...dayData, ...todayHourly].slice(-24);
  }, [usage, realmSeries]);

  /** 今日用量（日点或小时点聚合） */
  const todayUsage = useMemo(() => {
    if (!usage) return {requests: 0, tokens: 0, credit: 0};
    const now = new Date();
    const todayKey = `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, '0')}-${String(now.getDate()).padStart(2, '0')}`;
    const hourPoints = realmSeries.filter((p) => p.scope === 'hour' && p.t.startsWith(todayKey));
    if (hourPoints.length) {
      return hourPoints.reduce(
        (acc, p) => ({
          requests: acc.requests + p.requests,
          tokens: acc.tokens + p.total_tokens,
          credit: 0,
        }),
        {requests: 0, tokens: 0, credit: 0},
      );
    }
    const todayDay = realmSeries.find((p) => p.scope === 'day' && p.t === todayKey);
    return todayDay
      ? {requests: todayDay.requests, tokens: todayDay.total_tokens, credit: 0}
      : {requests: 0, tokens: 0, credit: 0};
  }, [usage, realmSeries]);

  const valid = scoped.filter((a) => !a.disabled && !a.manual_disabled).length;
  // 「非有效」的细分：manual_disabled 是运维手动停用——用户自己的决定，不算
  // 异常（口径同 account-status 的分档）；disabled 且非手动停用才是真异常。
  // 两集合恰好把差值切分干净，供 hint 分开说「停用」与「异常」。
  const manualN = scoped.filter((a) => a.manual_disabled).length;
  const abnormalN = scoped.filter((a) => a.disabled && !a.manual_disabled).length;
  // 积分余额合计（仅统计已同步到的账号）
  const credOf = (a: Account) => liveCredits[a.uid] ?? a.credits;
  const creditsKnown = scoped.filter((a) => typeof credOf(a) === 'number');
  const totalCredits = creditsKnown.reduce((sum, a) => sum + (credOf(a) || 0), 0);
  const creditsLow = creditsKnown.filter((a) => (credOf(a) || 0) < 200).length;
  // 已用积分合计：与余额同源（packages），只在查到至少一个账号时显示。
  const usedKnown = scoped.filter((a) => typeof liveUsed[a.uid] === 'number');
  const totalUsed = usedKnown.reduce((sum, a) => sum + (liveUsed[a.uid] || 0), 0);
  // 7 天内的到期算紧急。取渲染时刻即可：本页每 30 秒重渲染一次。
  const expiryUrgent = !!nextExpiry && nextExpiry.at - Date.now() < 7 * 86400_000;

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      {/* 本页 30 秒自动刷新，且没有任何会改变数据的操作，
          因此不再放手动刷新按钮（移动端还省下一行） */}
      <PageHeader
        title={t('dashboard.title')}
        description={t('dashboard.description', {realm: realmName})}
      />

      <section className="grid grid-cols-2 gap-3 lg:grid-cols-5 md:gap-4">
        <StatCard
          label={t('dashboard.totalAccounts')}
          value={fmtNumber(scoped.length)}
          hint={t('dashboard.totalAccountsHint', {realm: realmName})}
          icon={Users}
          tone="neutral"
          delay={0}
        />
        <StatCard
          label={t('dashboard.valid')}
          value={fmtNumber(valid)}
          hint={
            unusable > 0
              ? t('dashboard.unusable', {count: unusable, n: unusable})
              : valid === scoped.length
                ? t('dashboard.allOk')
                : manualN > 0 && abnormalN > 0
                  ? // 停用与真实异常并存：合并成一条，明确各自数量
                    t('dashboard.mixedHint', {m: manualN, n: abnormalN})
                  : manualN > 0
                    ? // 手动停用是用户自己的决定，明确说「停用」而不笼统说「异常」
                      t('dashboard.manualDisabledHint', {n: manualN})
                    : t('dashboard.abnormal', {count: abnormalN, n: abnormalN})
          }
          icon={CircleCheck}
          tone={unusable > 0 ? 'warning' : 'success'}
          hintTone={unusable > 0 ? 'warning' : (valid === scoped.length ? 'success' : 'warning')}
          delay={0.04}
        />
        <StatCard
          label={t('dashboard.cooling')}
          value={fmtNumber(pool.known ? pool.cooling : (overview?.cooling ?? 0))}
          hint={
            degraded > 0
              ? t('dashboard.degradedHint', {count: degraded, n: degraded})
              : t('dashboard.noCooling')
          }
          icon={TriangleAlert}
          tone={degraded > 0 ? 'warning' : 'neutral'}
          hintTone={degraded > 0 ? 'warning' : undefined}
          delay={0.08}
        />
        <StatCard
          label={t('metric.credits')}
          labelNode={
            <span className="inline-flex items-center gap-1">
              {/* 到期口径切换（浏览器本地偏好）：默认「最近一笔」；点开按天归并。
                  见上方 mergedExpiries 派生逻辑。labelNode 覆盖纯文本 label，
                  使切换按钮能出现在卡片标题行。 */}
              <button
                type="button"
                onClick={() => toggleExpiryDailyMerge(!expiryDailyMerge)}
                title={t('dashboard.expiryMergeToggle', {mode: expiryDailyMerge ? t('dashboard.expiryMergeDaily') : t('dashboard.expiryMergeSingle')})}
                className={
                  'rounded-full px-1.5 py-0.5 text-[9px] font-normal leading-none ' +
                  (expiryDailyMerge
                    ? 'bg-sky-500/15 text-sky-600 dark:text-sky-400'
                    : 'bg-gray-500/10 text-gray-500 dark:text-gray-400')
                }
              >
                {expiryDailyMerge ? t('dashboard.expiryMergeDaily') : t('dashboard.expiryMergeSingle')}
              </button>
            </span>
          }
          // 值里带上最近到期：额度高但下周作废，比额度低更值得注意
          value={creditsKnown.length ? fmtNumber(totalCredits) : '—'}
          // hint 分段拼接：已用合计（数据同步后恒显示）+ 警告（低余额或紧急到期，
          // 二者取一）——警告不该把「已用」整个挤掉，两者用分隔符并列。
          hint={
            !creditsKnown.length
              ? t('dashboard.waitingUpstream')
              : (() => {
                  const parts: string[] = [];
                  if (usedKnown.length) {
                    parts.push(t('dashboard.creditsUsedHint', {used: fmtNumber(totalUsed), count: usedKnown.length, n: usedKnown.length}));
                  }
                  if (creditsLow > 0) {
                    parts.push(t('dashboard.creditsLow', {count: creditsLow, n: creditsLow}));
                  } else if (expiryUrgent && nextExpiry) {
                    parts.push(
                      expiryDailyMerge && mergedExpiries.length > 1
                        ? t('dashboard.creditsExpiryDaily', {
                            time: fmtDateTime(nextExpiry.at),
                            amount: fmtNumber(nextExpiry.amount),
                          })
                        : t('dashboard.creditsExpiry', {
                            time: fmtDateTime(nextExpiry.at),
                            amount: fmtNumber(nextExpiry.amount),
                          }),
                    );
                  }
                  if (parts.length) return parts.join(t('common.listSeparator'));
                  if (nextExpiry) {
                    return expiryDailyMerge && mergedExpiries.length > 1
                      ? t('dashboard.creditsExpiryDaily', {
                          time: fmtDateTime(nextExpiry.at),
                          amount: fmtNumber(nextExpiry.amount),
                        })
                      : t('dashboard.creditsExpiry', {
                          time: fmtDateTime(nextExpiry.at),
                          amount: fmtNumber(nextExpiry.amount),
                        });
                  }
                  return t('dashboard.creditsCovered', {count: creditsKnown.length, n: creditsKnown.length});
                })()
          }
          icon={Coins}
          tone={
            !creditsKnown.length ? 'neutral' : creditsLow > 0 || expiryUrgent ? 'warning' : 'accent'
          }
          hintTone={creditsLow > 0 || expiryUrgent ? 'warning' : undefined}
          delay={0.12}
        />
        <StatCard
          label={t('dashboard.todayTokens')}
          value={fmtCompact(todayUsage.tokens)}
          hint={t('dashboard.todayRequests', {
            n: fmtNumber(todayUsage.requests),
            realm: realmName,
          })}
          icon={Activity}
          tone="info"
          delay={0.16}
        />
      </section>

      <section className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        <div className="rounded-[20px] bg-muted p-4 lg:col-span-2">
          <div className="mb-3 flex items-center justify-between">
            <div className="text-sm font-medium">{t('dashboard.trend14')}</div>
            <div className="text-[11px] text-muted-foreground">
              {t('dashboard.requestsByRealm', {realm: realmName})}
            </div>
          </div>
          <div className="h-[220px] w-full">
            {chartData.length ? (
              <ResponsiveContainer width="100%" height="100%">
                <AreaChart data={chartData} margin={{top: 4, right: 8, bottom: 0, left: -16}}>
                  <defs>
                    <linearGradient id="gReq" x1="0" y1="0" x2="0" y2="1">
                      <stop offset="0%" stopColor="var(--chart-1)" stopOpacity={0.35} />
                      <stop offset="100%" stopColor="var(--chart-1)" stopOpacity={0} />
                    </linearGradient>
                  </defs>
                  <CartesianGrid strokeDasharray="3 3" stroke="var(--border)" vertical={false} />
                  <XAxis dataKey="day" tickLine={false} axisLine={false} fontSize={11} stroke="var(--muted-foreground)" />
                  <YAxis tickLine={false} axisLine={false} fontSize={11} stroke="var(--muted-foreground)" />
                  <Tooltip
                    contentStyle={{
                      background: 'var(--popover)',
                      border: '1px solid var(--border)',
                      borderRadius: 12,
                      fontSize: 12,
                    }}
                  />
                  <Area
                    type="monotone"
                    dataKey="requests"
                    name={t('metric.requests')}
                    stroke="var(--chart-1)"
                    fill="url(#gReq)"
                    strokeWidth={2}
                  />
                </AreaChart>
              </ResponsiveContainer>
            ) : (
              <div className="grid h-full place-items-center text-xs text-muted-foreground">{t('dashboard.noCallData')}</div>
            )}
          </div>
        </div>

        <div className="rounded-[20px] bg-muted p-4">
          <div className="mb-3 flex items-center gap-2 text-sm font-medium">
            <Server className="h-4 w-4" />
            {t('dashboard.upstreamPanel')}
          </div>
          {overview ? (
            <div className="space-y-3">
              <div className="flex items-center justify-between text-xs">
                <span className="text-muted-foreground">{t('dashboard.connStatus')}</span>
                {upstream ? (
                  <Badge variant="secondary" className="rounded-full text-emerald-600 dark:text-emerald-400">
                    {t('dashboard.connected')}
                  </Badge>
                ) : (
                  <Badge variant="destructive" className="rounded-full">
                    {t('dashboard.unavailable')}
                  </Badge>
                )}
              </div>
              {([
                [t('dashboard.healthyAccounts'), pool.known ? pool.healthy : '—'],
                [t('dashboard.cooling'), pool.known ? pool.cooling : '—'],
                [t('dashboard.degraded'), degraded],
                [t('dashboard.disabled'), pool.known ? pool.disabled : '—'],
                [t('dashboard.stickySessions'), overview.sticky_sessions ?? 0],
                [t('dashboard.redisMode'), overview.redis_mode ?? '—'],
              ] as [string, string | number][]).map(([k, v]) => (
                <div key={k} className="flex items-center justify-between text-xs">
                  <span className="text-muted-foreground">{k}</span>
                  <span className="font-medium tabular-nums">{String(v)}</span>
                </div>
              ))}
            </div>
          ) : (
            <CardRowsSkeleton rows={3} />
          )}
        </div>
      </section>

      <section className="rounded-[20px] bg-muted p-4">
        <div className="mb-3 flex flex-wrap items-center gap-x-3 gap-y-1">
          <span className="text-sm font-medium">{t('dashboard.healthSnapshot')}</span>
          {/* 各档汇总：一眼看清「有几个能真用」。文案与账号页共用同一套键 */}
          {(['online', 'cooling', 'neverSucceeded', 'disabled', 'unknown'] as AvailabilityTier[])
            .filter((tier) => availability[tier] > 0)
            .map((tier) => (
              <span
                key={tier}
                className={`text-[11px] tabular-nums ${availabilityClass(tier)}`}
                title={availabilityTitleKey(tier) ? t(availabilityTitleKey(tier)!) : undefined}
              >
                {t(availabilityLabelKey(tier, scoped.find((a) => availabilityOf(a) === tier)))}
                {' '}
                {availability[tier]}
              </span>
            ))}
        </div>
        {scoped.length ? (
          <div className="grid grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-3">
            {/* 默认按实时积分多→少排序（无积分数据的排最后，同分按 uid 稳定序）；
                3 列 × 6 行 = 18 个的上限，再多看账号页 */}
            {[...scoped]
              .sort((a, b) => {
                const ca = credOf(a);
                const cb = credOf(b);
                const va = typeof ca === 'number' ? ca : -1;
                const vb = typeof cb === 'number' ? cb : -1;
                if (va !== vb) return vb - va;
                return a.uid.localeCompare(b.uid);
              })
              .slice(0, 18)
              .map((a) => {
              const tier = availabilityOf(a);
              const statusLabel = t(availabilityLabelKey(tier, a));
              const titleKey = availabilityTitleKey(tier);
              const credit = credOf(a);
              return (
                <div key={a.uid} className="rounded-2xl bg-background/60 p-3">
                  <div className="flex items-center justify-between gap-2">
                    <span
                      className={
                        'truncate text-sm font-medium ' +
                        (tier === 'disabled' ? 'text-muted-foreground' : '')
                      }
                    >
                      {a.nickname || a.uid}
                    </span>
                    <span
                      className={'shrink-0 text-[10px] font-medium ' + availabilityClass(tier)}
                      title={titleKey ? t(titleKey) : undefined}
                    >
                      {statusLabel}
                    </span>
                  </div>
                  <div className="mt-1.5 flex items-center justify-between gap-2">
                    <span className="font-mono text-[10px] text-muted-foreground">{a.uid}</span>
                    <span
                      className={
                        'text-[11px] font-medium tabular-nums ' +
                        (typeof credit !== 'number'
                          ? 'text-muted-foreground'
                          : credit <= 0
                            ? 'text-red-600 dark:text-red-400'
                            : credit < 200
                              ? 'text-amber-600 dark:text-amber-400'
                              : 'text-foreground')
                      }
                      title={t('metric.credits')}
                    >
                      {typeof credit === 'number' ? t('metric.creditAmount', {n: fmtNumber(credit)}) : ''}
                    </span>
                  </div>
                </div>
              );
            })}
          </div>
        ) : (
          <EmptyState
            icon={Users}
            title={t('dashboard.noAccounts')}
            description={t('dashboard.noAccountsHint')}
            className="flex flex-col items-center justify-center py-12 text-center"
          />
        )}
      </section>
    </div>
  );
}
