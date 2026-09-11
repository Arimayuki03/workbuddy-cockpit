'use client';

import {useCallback, useEffect, useState} from 'react';
import {Activity, TrendingUp, KeyRound, Cpu, RefreshCw} from 'lucide-react';
import {
  Bar,
  BarChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';
import {statsApi, errText} from '@/lib/api';
import type {StatsSummary, UsageBreakdown, UsagePoint} from '@/lib/types';
import {fmtCompact, fmtNumber} from '@/lib/format';
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
import {toast} from 'sonner';

const CHART_COLORS = [
  'var(--chart-1)',
  'var(--chart-2)',
  'var(--chart-3)',
  'var(--chart-4)',
  'var(--chart-5)',
];

export default function StatsPage() {
  const [summary, setSummary] = useState<StatsSummary | null>(null);
  const [daily, setDaily] = useState<UsagePoint[]>([]);
  const [byModel, setByModel] = useState<UsageBreakdown[]>([]);
  const [byKey, setByKey] = useState<UsageBreakdown[]>([]);
  const [days, setDays] = useState('30');
  const [loading, setLoading] = useState(true);

  const load = useCallback(async () => {
    setLoading(true);
    const d = Number(days) || 30;
    const results = await Promise.allSettled([
      statsApi.summary(),
      statsApi.daily(d),
      statsApi.byModel(d),
      statsApi.byKey(d),
    ]);
    if (results[0].status === 'fulfilled') setSummary(results[0].value);
    if (results[1].status === 'fulfilled') setDaily(results[1].value);
    if (results[2].status === 'fulfilled') setByModel(results[2].value);
    if (results[3].status === 'fulfilled') setByKey(results[3].value);
    if (results.some((r) => r.status === 'rejected')) toast.error(errText((results.find((r) => r.status === 'rejected') as PromiseRejectedResult).reason));
    setLoading(false);
  }, [days]);

  useEffect(() => {
    load();
  }, [load]);

  const chartData = daily.map((d) => ({
    day: d.day.slice(5),
    tokens: d.prompt_tokens + d.completion_tokens,
    requests: d.requests,
  }));

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader
        title="用量统计"
        description="按时间、模型与密钥维度统计 Token 消耗与请求量"
        actions={
          <>
            <Select value={days} onValueChange={setDays}>
              <SelectTrigger className="h-8 w-[130px] rounded-full"><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value="7">近 7 天</SelectItem>
                <SelectItem value="30">近 30 天</SelectItem>
                <SelectItem value="90">近 90 天</SelectItem>
              </SelectContent>
            </Select>
            <Button variant="outline" size="sm" className="rounded-full" onClick={load} disabled={loading}>
              <RefreshCw className={loading ? 'animate-spin' : ''} />
              刷新
            </Button>
          </>
        }
      />

      <section className="grid grid-cols-2 gap-3 lg:grid-cols-4 md:gap-4">
        <StatCard
          label="今日请求"
          value={fmtNumber(summary?.today_requests)}
          hint={`${fmtCompact(summary?.today_tokens)} Token`}
          icon={Activity}
          delay={0}
        />
        <StatCard
          label="本周请求"
          value={fmtNumber(summary?.week_requests)}
          hint={`${fmtCompact(summary?.week_tokens)} Token`}
          icon={TrendingUp}
          delay={0.05}
        />
        <StatCard
          label="累计请求"
          value={fmtCompact(summary?.total_requests)}
          hint={`${fmtCompact(summary?.total_tokens)} Token`}
          icon={Activity}
          delay={0.1}
        />
        <StatCard
          label="活跃密钥"
          value={fmtNumber(summary?.active_keys)}
          hint={summary?.top_model ? `主力模型 ${summary.top_model}` : '分布中'}
          icon={KeyRound}
          delay={0.15}
        />
      </section>

      <section className="rounded-[20px] bg-muted p-4">
        <div className="mb-3 flex items-center justify-between">
          <div className="text-sm font-medium">Token 消耗趋势</div>
          <div className="text-[11px] text-muted-foreground">按天聚合</div>
        </div>
        <div className="h-[260px] w-full">
          {chartData.length ? (
            <ResponsiveContainer width="100%" height="100%">
              <BarChart data={chartData} margin={{top: 4, right: 8, bottom: 0, left: -8}}>
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
                  formatter={(value, name) => [
                    fmtNumber(Number(value)),
                    String(name) === 'tokens' ? 'Token' : '请求数',
                  ]}
                />
                <Bar dataKey="tokens" name="tokens" fill="var(--chart-1)" radius={[4, 4, 0, 0]} />
              </BarChart>
            </ResponsiveContainer>
          ) : (
            <div className="grid h-full place-items-center text-xs text-muted-foreground">暂无用量数据</div>
          )}
        </div>
      </section>

      <section className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <BreakdownPanel title="按模型" icon={Cpu} items={byModel} />
        <BreakdownPanel title="按密钥" icon={KeyRound} items={byKey} />
      </section>
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
  items: UsageBreakdown[];
}) {
  const max = Math.max(1, ...items.map((i) => i.prompt_tokens + i.completion_tokens));
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
              <TableHead className="pl-0 text-[11px] text-muted-foreground">名称</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">请求</TableHead>
              <TableHead className="pr-0 text-[11px] text-muted-foreground">Token</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {items.map((it, i) => {
              const tokens = it.prompt_tokens + it.completion_tokens;
              return (
                <TableRow key={`${it.name}-${i}`} className="border-b border-border/40">
                  <TableCell className="pl-0">
                    <div className="flex items-center gap-2">
                      <span className="h-2 w-2 rounded-full" style={{background: CHART_COLORS[i % CHART_COLORS.length]}} />
                      <span className="max-w-[140px] truncate text-xs font-medium">{it.name || '未知'}</span>
                    </div>
                    <div className="mt-1.5 h-1 w-full max-w-[140px] overflow-hidden rounded-full bg-border">
                      <div
                        className="h-full rounded-full"
                        style={{
                          width: `${(tokens / max) * 100}%`,
                          background: CHART_COLORS[i % CHART_COLORS.length],
                        }}
                      />
                    </div>
                  </TableCell>
                  <TableCell className="text-xs tabular-nums">{fmtNumber(it.requests)}</TableCell>
                  <TableCell className="pr-0 text-xs tabular-nums">{fmtCompact(tokens)}</TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      ) : (
        <EmptyState
          icon={Activity}
          title="暂无数据"
          description="该维度在所选时间范围内没有调用"
          className="flex flex-col items-center justify-center py-10 text-center"
        />
      )}
    </div>
  );
}
