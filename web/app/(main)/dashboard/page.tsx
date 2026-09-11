'use client';

import {useCallback, useEffect, useState} from 'react';
import {Users, CircleCheck, TriangleAlert, Activity, Server, RefreshCw} from 'lucide-react';
import {
  Area,
  AreaChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';
import {accountApi, statsApi, upstreamApi} from '@/lib/api';
import type {Account, StatsSummary, UpstreamStatus, UsagePoint} from '@/lib/types';
import {expiryVisual, fmtCompact, fmtNumber, fmtRemain} from '@/lib/format';
import {PageHeader} from '@/components/common/layout/PageHeader';
import {StatCard} from '@/components/common/layout/StatCard';
import {EmptyState} from '@/components/common/layout/EmptyState';
import {Button} from '@/components/ui/button';
import {Badge} from '@/components/ui/badge';
import {notify} from '@/lib/toast';

export default function DashboardPage() {
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [summary, setSummary] = useState<StatsSummary | null>(null);
  const [daily, setDaily] = useState<UsagePoint[]>([]);
  const [upstream, setUpstream] = useState<UpstreamStatus | null>(null);
  const [loading, setLoading] = useState(true);

  const load = useCallback(async () => {
    setLoading(true);
    const results = await Promise.allSettled([
      accountApi.list(),
      statsApi.summary(),
      statsApi.daily(14),
      upstreamApi.status(),
    ]);
    if (results[0].status === 'fulfilled') setAccounts(results[0].value.accounts);
    if (results[1].status === 'fulfilled') setSummary(results[1].value);
    if (results[2].status === 'fulfilled') setDaily(results[2].value);
    if (results[3].status === 'fulfilled') setUpstream(results[3].value);
    if (results.some((r) => r.status === 'rejected')) notify.err('部分数据加载失败');
    setLoading(false);
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const valid = accounts.filter((a) => !a.is_expired).length;
  const expiring = accounts.filter((a) => a.remain_seconds > 0 && a.remain_seconds < 3600).length;

  const chartData = daily.map((d) => ({
    day: d.day.slice(5),
    requests: d.requests,
    tokens: d.prompt_tokens + d.completion_tokens,
  }));

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader
        title="仪表盘"
        description="账号池健康度、反代网关与今日用量总览"
        actions={
          <Button variant="outline" size="sm" className="rounded-full" onClick={load} disabled={loading}>
            <RefreshCw className={loading ? 'animate-spin' : ''} />
            刷新
          </Button>
        }
      />

      <section className="grid grid-cols-2 gap-3 lg:grid-cols-4 md:gap-4">
        <StatCard
          label="账号总数"
          value={fmtNumber(accounts.length)}
          hint="已纳管账号"
          icon={Users}
          tone="neutral"
          delay={0}
        />
        <StatCard
          label="有效期内"
          value={fmtNumber(valid)}
          hint={valid === accounts.length ? '全部正常' : `${accounts.length - valid} 个异常`}
          icon={CircleCheck}
          tone="success"
          hintTone={valid === accounts.length ? 'success' : 'warning'}
          delay={0.05}
        />
        <StatCard
          label="即将过期"
          value={fmtNumber(expiring)}
          hint={expiring > 0 ? '<1h 需刷新' : '暂无风险'}
          icon={TriangleAlert}
          tone={expiring > 0 ? 'warning' : 'success'}
          hintTone={expiring > 0 ? 'warning' : 'neutral'}
          delay={0.1}
        />
        <StatCard
          label="今日 Token"
          value={fmtCompact(summary?.today_tokens)}
          hint={`${fmtNumber(summary?.today_requests)} 次请求`}
          icon={Activity}
          tone="info"
          delay={0.15}
        />
      </section>

      <section className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        <div className="rounded-[20px] bg-muted p-4 lg:col-span-2">
          <div className="mb-3 flex items-center justify-between">
            <div className="text-sm font-medium">近 14 天调用趋势</div>
            <div className="text-[11px] text-muted-foreground">请求数 / Token</div>
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
                    name="请求数"
                    stroke="var(--chart-1)"
                    fill="url(#gReq)"
                    strokeWidth={2}
                  />
                </AreaChart>
              </ResponsiveContainer>
            ) : (
              <div className="grid h-full place-items-center text-xs text-muted-foreground">暂无调用数据</div>
            )}
          </div>
        </div>

        <div className="rounded-[20px] bg-muted p-4">
          <div className="mb-3 flex items-center gap-2 text-sm font-medium">
            <Server className="h-4 w-4" />
            反代上游
          </div>
          {upstream ? (
            <div className="space-y-3">
              <div className="flex items-center justify-between text-xs">
                <span className="text-muted-foreground">连接状态</span>
                {upstream.connected ? (
                  <Badge variant="secondary" className="rounded-full text-emerald-600 dark:text-emerald-400">
                    ● 正常
                  </Badge>
                ) : (
                  <Badge variant="destructive" className="rounded-full">
                    ● 不可用
                  </Badge>
                )}
              </div>
              {([
                ['健康账号', upstream.healthy ?? 0],
                ['冷却中', upstream.cooling ?? 0],
                ['已禁用', upstream.disabled ?? 0],
                ['粘性会话', upstream.sticky_sessions ?? 0],
                ['Redis 模式', upstream.redis_mode ?? '—'],
              ] as [string, string | number][]).map(([k, v]) => (
                <div key={k} className="flex items-center justify-between text-xs">
                  <span className="text-muted-foreground">{k}</span>
                  <span className="font-medium tabular-nums">{String(v)}</span>
                </div>
              ))}
              {upstream.error && <p className="text-[11px] text-red-500">{upstream.error}</p>}
            </div>
          ) : (
            <div className="grid h-[160px] place-items-center text-xs text-muted-foreground">未获取到上游状态</div>
          )}
        </div>
      </section>

      <section className="rounded-[20px] bg-muted p-4">
        <div className="mb-3 text-sm font-medium">账号健康快照</div>
        {accounts.length ? (
          <div className="grid grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-3">
            {accounts.slice(0, 9).map((a) => {
              const pct = Math.min(100, Math.max(0, (a.remain_seconds / (72 * 3600)) * 100));
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
                  <div className={'mt-1.5 text-[11px] tabular-nums ' + vis.textClass}>
                    {fmtRemain(a.remain_seconds)}
                  </div>
                </div>
              );
            })}
          </div>
        ) : (
          <EmptyState
            icon={Users}
            title="暂无账号"
            description="点击底栏「快速添加」扫码授权腾讯账号"
            className="flex flex-col items-center justify-center py-12 text-center"
          />
        )}
      </section>
    </div>
  );
}
