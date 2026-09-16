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
import {accountApi, statsApi, upstreamApi} from '@/lib/api';
import {useRealm} from '@/lib/realm-context';
import type {Account, StatsSummary, UpstreamStatus, UsagePoint} from '@/lib/types';
import {expiryBarPercent, expiryVisual, fmtCompact, fmtNumber, fmtRemain} from '@/lib/format';
import {PageHeader} from '@/components/common/layout/PageHeader';
import {StatCard} from '@/components/common/layout/StatCard';
import {EmptyState} from '@/components/common/layout/EmptyState';
import {Badge} from '@/components/ui/badge';
import {useT} from '@/lib/i18n/provider';
import {notify} from '@/lib/toast';

export default function DashboardPage() {
  const {realm, label: realmName} = useRealm();
  const t = useT();
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [summary, setSummary] = useState<StatsSummary | null>(null);
  const [daily, setDaily] = useState<UsagePoint[]>([]);
  const [upstream, setUpstream] = useState<UpstreamStatus | null>(null);
  /** 实时积分（按 uid），叠加到 accounts 上；上游 /status 的 credits 可能滞后数小时 */
  const [liveCredits, setLiveCredits] = useState<Record<string, number>>({});

  // 切换版本后要重新取实时积分：两个版本的账号池不同，credits 也不能混
  const load = useCallback(async () => {
    const results = await Promise.allSettled([
      accountApi.list(),
      statsApi.summary(),
      statsApi.daily(14),
      upstreamApi.status(),
      // force=false：命中服务端 60 秒缓存，30 秒轮询不会反复打腾讯
      accountApi.refreshCredits(false),
    ]);
    if (results[0].status === 'fulfilled') setAccounts(results[0].value.accounts);
    if (results[1].status === 'fulfilled') setSummary(results[1].value);
    if (results[2].status === 'fulfilled') setDaily(results[2].value);
    if (results[3].status === 'fulfilled') setUpstream(results[3].value);
    if (results[4].status === 'fulfilled') {
      const r = results[4].value;
      setLiveCredits(
        Object.fromEntries(
          Object.entries(r.credits).filter(([, v]) => typeof v === 'number') as [string, number][],
        ),
      );
    }
    if (results.slice(0, 4).some((r) => r.status === 'rejected')) notify.err(t('dashboard.partialLoadFailed'));
  }, [realm, t]);

  useEffect(() => {
    load();
  }, [load]);

  // 账号健康度与用量会持续变化，用心跳刷新避免展示陈旧数据
  useHeartbeat(load, 30000);

  /**
   * 按当前版本过滤。
   *
   * 账号池里两种版本的账号都有，不过滤的话切到国际版仍会看到国内版的
   * 账号数、积分与健康快照（用户反馈过这个问题）。realm 为空的存量账号
   * 视为国内版，与后端判定一致。
   */
  const scoped = useMemo(
    () => accounts.filter((a) => (a.realm ?? 'cn') === realm),
    [accounts, realm],
  );

  const valid = scoped.filter((a) => !a.is_expired).length;
  const expiring = scoped.filter((a) => a.remain_seconds > 0 && a.remain_seconds < 3600).length;
  // 积分余额合计（仅统计已同步到的账号）
  // 优先用实时查询到的积分，其次上游 /status 的（可能滞后的）值
  const credOf = (a: Account) => liveCredits[a.uid] ?? a.credits;
  const creditsKnown = scoped.filter((a) => typeof credOf(a) === 'number');
  const totalCredits = creditsKnown.reduce((sum, a) => sum + (credOf(a) || 0), 0);
  const creditsLow = creditsKnown.filter((a) => (credOf(a) || 0) < 200).length;

  /**
   * 「反代上游」面板的计数，取自上游 `/status` 的 `realm_totals`。
   *
   * 走过的弯路记在这里，免得后人重蹈：
   *   1. 最初读 `upstream.healthy` —— 那是**全局**汇总，切到国际版会显示
   *      两个版本加起来的数；
   *   2. 于是改成从 `accounts` 明细里自己数，却读了明细条目上的 healthy ——
   *      而**账号明细里根本没有 healthy 字段**（它是汇总层才有的），布尔转换
   *      恒为 false，面板因此全显示 0。
   * 上游其实已经按版本分好组了（`realm_totals.cn` / `.global`），直接用即可——
   * 口径与上游状态机完全一致，也不用我们去猜 healthy 该怎么算。
   */
  const pool = useMemo(() => {
    const perRealm = upstream?.realm_totals?.[realm];
    if (perRealm) {
      return {
        total: perRealm.total,
        healthy: perRealm.healthy,
        cooling: perRealm.cooling,
        disabled: perRealm.disabled,
        known: true,
      };
    }
    // 上游未提供 realm_totals（老版本）时退回顶层汇总，并如实说明是全局口径
    const hasTop = typeof upstream?.total === 'number';
    return {
      total: upstream?.total ?? 0,
      healthy: upstream?.healthy ?? 0,
      cooling: upstream?.cooling ?? 0,
      disabled: upstream?.disabled ?? 0,
      known: hasTop,
      /** true = 只能用全局汇总（含两个版本），界面需标注 */
      globalOnly: hasTop,
    };
  }, [upstream, realm]);

  const chartData = daily.map((d) => ({
    day: d.day.slice(5),
    requests: d.requests,
    tokens: d.prompt_tokens + d.completion_tokens,
  }));

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
            valid === scoped.length
              ? t('dashboard.allOk')
              : t('dashboard.abnormal', {count: scoped.length - valid, n: scoped.length - valid})
          }
          icon={CircleCheck}
          tone="success"
          hintTone={valid === scoped.length ? 'success' : 'warning'}
          delay={0.05}
        />
        <StatCard
          label={t('expiry.urgent')}
          value={fmtNumber(expiring)}
          hint={expiring > 0 ? t('dashboard.needRefresh') : t('dashboard.noRisk')}
          icon={TriangleAlert}
          tone={expiring > 0 ? 'warning' : 'success'}
          hintTone={expiring > 0 ? 'warning' : 'neutral'}
          delay={0.1}
        />
        <StatCard
          label={t('metric.credits')}
          value={creditsKnown.length ? fmtNumber(totalCredits) : '—'}
          hint={
            !creditsKnown.length
              ? t('dashboard.waitingUpstream')
              : creditsLow > 0
                ? t('dashboard.creditsLow', {count: creditsLow, n: creditsLow})
                : t('dashboard.creditsCovered', {count: creditsKnown.length, n: creditsKnown.length})
          }
          icon={Coins}
          tone={!creditsKnown.length ? 'neutral' : creditsLow > 0 ? 'warning' : 'accent'}
          hintTone={creditsLow > 0 ? 'warning' : undefined}
          delay={0.15}
        />
        <StatCard
          label={t('dashboard.todayTokens')}
          value={fmtCompact(summary?.today_tokens)}
          hint={t('dashboard.todayRequests', {n: fmtNumber(summary?.today_requests)})}
          icon={Activity}
          tone="info"
          delay={0.2}
        />
      </section>

      <section className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        <div className="rounded-[20px] bg-muted p-4 lg:col-span-2">
          <div className="mb-3 flex items-center justify-between">
            <div className="text-sm font-medium">{t('dashboard.trend14')}</div>
            {/* 调用记录是全局的（不按版本拆分），如实标注而不是假装已过滤 */}
            <div className="text-[11px] text-muted-foreground">{t('dashboard.requestsBothRealms')}</div>
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
          {upstream ? (
            <div className="space-y-3">
              <div className="flex items-center justify-between text-xs">
                <span className="text-muted-foreground">{t('dashboard.connStatus')}</span>
                {upstream.connected ? (
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
                // 账号类计数按当前版本（上游 realm_totals）；粘性会话与 Redis
                // 无版本之分，保持全局
                [t('dashboard.healthyAccounts'), pool.known ? pool.healthy : '—'],
                [t('dashboard.cooling'), pool.known ? pool.cooling : '—'],
                [t('dashboard.disabled'), pool.known ? pool.disabled : '—'],
                [t('dashboard.stickySessions'), upstream.sticky_sessions ?? 0],
                [t('dashboard.redisMode'), upstream.redis_mode ?? '—'],
              ] as [string, string | number][]).map(([k, v]) => (
                <div key={k} className="flex items-center justify-between text-xs">
                  <span className="text-muted-foreground">{k}</span>
                  <span className="font-medium tabular-nums">{String(v)}</span>
                </div>
              ))}
              {pool.globalOnly && !upstream.error && (
                <p className="text-[11px] text-muted-foreground">
                  {t('dashboard.globalTotalsNote')}
                </p>
              )}
              {upstream.error && <p className="text-[11px] text-red-500">{upstream.error}</p>}
            </div>
          ) : (
            <div className="grid h-[160px] place-items-center text-xs text-muted-foreground">{t('dashboard.noUpstreamStatus')}</div>
          )}
        </div>
      </section>

      <section className="rounded-[20px] bg-muted p-4">
        <div className="mb-3 text-sm font-medium">{t('dashboard.healthSnapshot')}</div>
        {scoped.length ? (
          <div className="grid grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-3">
            {scoped.slice(0, 9).map((a) => {
              const pct = expiryBarPercent(a.remain_seconds, a.ttl_seconds);
              const vis = expiryVisual(a.remain_seconds);
              return (
                <div key={a.file} className="rounded-2xl bg-background/60 p-3">
                  <div className="flex items-center justify-between gap-2">
                    <span
                      className={
                        'truncate text-sm font-medium ' +
                        (vis.tier === 'expired' ? 'text-muted-foreground' : '')
                      }
                    >
                      {a.nickname || a.uid}
                    </span>
                    <span className={'shrink-0 text-[10px] font-medium ' + vis.textClass}>
                      {vis.label}
                    </span>
                  </div>
                  <div className="mt-2 h-1.5 overflow-hidden rounded-full bg-border">
                    <div
                      className="h-full rounded-full transition-all"
                      style={{width: `${pct}%`, background: vis.barColor}}
                    />
                  </div>
                  <div className="mt-1.5 flex items-center justify-between gap-2">
                    <span className={'text-[11px] tabular-nums ' + vis.textClass}>
                      {fmtRemain(a.remain_seconds)}
                    </span>
                    <span
                      className={
                        'text-[11px] font-medium tabular-nums ' +
                        (typeof credOf(a) !== 'number'
                          ? 'text-muted-foreground'
                          : (credOf(a) as number) <= 0
                            ? 'text-red-600 dark:text-red-400'
                            : (credOf(a) as number) < 200
                              ? 'text-amber-600 dark:text-amber-400'
                              : 'text-foreground')
                      }
                      title={t('metric.credits')}
                    >
                      {typeof credOf(a) === 'number'
                        ? t('metric.creditAmount', {n: fmtNumber(credOf(a))})
                        : ''}
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
