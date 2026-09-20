'use client';

import {useCallback, useEffect, useMemo, useState} from 'react';
import {
  Boxes,
  Brain,
  Gauge,
  Layers,
  Loader2,
  Maximize2,
  RefreshCw,
  Search,
  FlaskConical,
} from 'lucide-react';

import {PageHeader} from '@/components/common/layout/PageHeader';
import {EmptyState} from '@/components/common/layout/EmptyState';
import {Button} from '@/components/ui/button';
import {Badge} from '@/components/ui/badge';
import {Input} from '@/components/ui/input';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import {modelApi, errText} from '@/lib/api';
import {useRealm} from '@/lib/realm-context';
import {notify} from '@/lib/toast';
import {useT} from '@/lib/i18n/provider';
import {cn} from '@/lib/utils';
import type {CatalogModel, ModelProbe} from '@/lib/types';

/** 上下文窗口显示：131072 → 128K；1048576 → 1M；0 → — */
function fmtCtx(n?: number): string {
  if (!n || n <= 0) return '—';
  if (n >= 1024 * 1024) {
    const m = n / (1024 * 1024);
    return `${Number.isInteger(m) ? m : m.toFixed(1)}M`;
  }
  if (n >= 1024) return `${Math.round(n / 1024)}K`;
  return String(n);
}

/** 系列归属（按 id 前缀推导，与 /v1/models 的路由协议同源） */
function seriesOf(id: string): string {
  if (id.startsWith('cn:')) {
    const bare = id.slice(3).toLowerCase();
    if (bare.startsWith('glm')) return 'glm';
    if (bare.startsWith('hunyuan')) return 'hunyuan';
    if (bare.startsWith('deepseek')) return 'deepseek';
    if (bare.startsWith('kimi')) return 'kimi';
    if (bare.startsWith('minimax')) return 'minimax';
    return 'other';
  }
  return 'global';
}

/** 一张统计卡 */
function StatCard({
  icon: Icon,
  label,
  value,
  hint,
}: {
  icon: typeof Boxes;
  label: string;
  value: string;
  hint?: string;
}) {
  return (
    <div className="rounded-[20px] bg-muted px-3.5 py-3">
      <div className="flex items-center gap-1.5 text-[11px] text-muted-foreground">
        <Icon className="h-3.5 w-3.5" />
        {label}
      </div>
      <div className="mt-1.5 text-xl font-semibold tabular-nums">{value}</div>
      {hint && <div className="mt-0.5 text-[10px] text-muted-foreground/80">{hint}</div>}
    </div>
  );
}

/** 实测上限标注（probe 数据存在时显示；ok = 整值，at_least = 下限） */
function ProbeBadge({probe}: {probe: ModelProbe}) {
  const t = useT();
  const measured = probe.measured;
  const text =
    probe.verdict === 'at_least'
      ? `≥${fmtCtx(measured ?? undefined)}`
      : measured
        ? fmtCtx(measured)
        : probe.verdict;
  const tone =
    probe.verdict === 'ok'
      ? 'border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-400'
      : probe.verdict === 'clamped'
        ? 'border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-400'
        : 'border-border bg-muted text-muted-foreground';
  return (
    <span
      className={cn(
        'inline-flex shrink-0 items-center rounded-md border px-1.5 py-0.5 text-[10px] font-medium',
        tone,
      )}
      title={[
        t('models.probeClaimed', {n: probe.claimed ?? 0}),
        probe.note || '',
      ].filter(Boolean).join('\n')}
    >
      <FlaskConical className="mr-1 h-2.5 w-2.5" />
      {text}
    </span>
  );
}

export default function ModelsPage() {
  const t = useT();
  const {realm, label: realmName} = useRealm();
  const [models, setModels] = useState<CatalogModel[]>([]);
  const [probes, setProbes] = useState<Record<string, ModelProbe>>({});
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState('');

  const [q, setQ] = useState('');
  const [series, setSeries] = useState('all');
  /** 能力筛选：全部 / 支持推理 / 大上下文 / 多模态 */
  const [cap, setCap] = useState<'all' | 'reasoning' | 'large' | 'vision'>('all');
  /**
   * 排序：默认按原顺序；「积分倍率从低到高」用于挑省积分的模型。
   */
  const [sort, setSort] = useState<'default' | 'credits'>('default');

  // panel models 是双域独立探测的结果（每条 id 自带 cn:/global: 前缀），
  // realm 过滤在前端做：切版本只筛本域条目。
  const load = useCallback(async (force = false) => {
    if (force) setRefreshing(true);
    else setLoading(true);
    try {
      const [mRes, pRes] = await Promise.allSettled([
        modelApi.models(realm),
        modelApi.probes(),
      ]);
      if (mRes.status === 'fulfilled') {
        const list = mRes.value.models ?? [];
        setModels(list.map((m) => ({
          id: m.id,
          name: m.name,
          context_length: m.context_length ?? 0,
          max_output_tokens: m.max_output_tokens ?? 0,
          efforts: m.supported_efforts ?? [],
          default_effort: m.default_effort ?? '',
          description: m.description,
          credits: m.credits,
          vendor: m.vendor,
          tags: m.tags,
          is_default: m.is_default,
          supports_reasoning: m.supports_reasoning,
          supports_tool_call: m.supports_tool_call,
          only_reasoning: m.only_reasoning,
          reasoning_summary: m.reasoning_summary,
          supports_images: !!m.supports_images,
          series: seriesOf(m.id),
          probe: null,
        })));
      } else {
        setModels([]);
        setError(errText(mRes.reason));
      }
      if (pRes.status === 'fulfilled') setProbes(pRes.value.probes ?? {});
      else setProbes({});
      setError('');
    } catch (e) {
      setError(errText(e));
    } finally {
      setLoading(false);
      setRefreshing(false);
    }
  }, [realm]);

  useEffect(() => {
    // 切版本时清掉筛选状态，避免「上一版的系列筛选把新版过滤成空」
    setSeries('all');
    setCap('all');
    setQ('');
    load();
  }, [load]);

  /** 实际显示的模型：按 realm 前缀过滤 + 合并探测标注 */
  const scoped = useMemo(
    () =>
      models.map((m) => {
        const prefix = realm === 'global' ? 'global:' : 'cn:';
        const bare = m.id.startsWith('cn:') || m.id.startsWith('global:') ? m.id.slice(3) : m.id;
        const probe = probes[bare] ?? probes[m.id] ?? null;
        return {...m, id: m.id.startsWith(prefix) ? m.id : (prefix + bare), probe};
      }).filter((m) => m.id.startsWith(realm === 'global' ? 'global:' : 'cn:')),
    [models, probes, realm],
  );

  const filtered = useMemo(() => {
    const kw = q.trim().toLowerCase();
    const list = scoped.filter((m) => {
      if (series !== 'all' && m.series !== series) return false;
      if (cap === 'reasoning' && m.efforts.length === 0) return false;
      if (cap === 'large' && (m.context_length || 0) < 131072) return false;
      if (cap === 'vision' && !m.supports_images) return false;
      if (!kw) return true;
      return (
        m.id.toLowerCase().includes(kw) ||
        (m.name || '').toLowerCase().includes(kw) ||
        m.series.toLowerCase().includes(kw)
      );
    });
    if (sort === 'credits') {
      // 倍率从低到高（越省越靠前）。没有倍率的排在最后——不是 0，不能当「免费」。
      const num = (v?: string) => {
        const m = /x?\s*([0-9]+(?:\.[0-9]+)?)/i.exec(v || '');
        return m ? Number(m[1]) : null;
      };
      return [...list].sort((a, b) => {
        const na = num(a.credits);
        const nb = num(b.credits);
        if (na === null && nb === null) return 0;
        if (na === null) return 1;
        if (nb === null) return -1;
        return na - nb;
      });
    }
    return list;
  }, [scoped, q, series, cap, sort]);

  const summary = useMemo(() => {
    const seriesSet = new Set(scoped.map((m) => m.series));
    return {
      total: scoped.length,
      reasoning: scoped.filter((m) => m.efforts.length > 0).length,
      large_context: scoped.filter((m) => (m.context_length || 0) >= 131072).length,
      max_context: scoped.reduce((mx, m) => Math.max(mx, m.context_length || 0), 0),
      series: [...seriesSet].sort(),
      unique_ids: new Set(scoped.map((m) => m.id)).size,
    };
  }, [scoped]);

  const probedCount = scoped.filter((m) => m.probe).length;

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader
        title={t('models.title')}
        description={t('models.description', {realm: realmName})}
        actions={
          <Button
            variant="outline"
            size="sm"
            className="rounded-full"
            disabled={refreshing}
            title={t('models.refetchTitle')}
            onClick={() => {
              load(true);
              notify.info(t('models.refetching'));
            }}
          >
            {refreshing ? <Loader2 className="animate-spin" /> : <RefreshCw />}
            {t('models.refetch')}
          </Button>
        }
      />

      {/* 统计卡 */}
      {!loading && (
        <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
          <StatCard icon={Boxes} label={t('models.available')} value={String(summary.total)} />
          <StatCard
            icon={Brain}
            label={t('models.reasoning')}
            value={String(summary.reasoning)}
            hint={t('models.reasoningHint')}
          />
          <StatCard
            icon={Maximize2}
            label={t('models.largeContext')}
            value={String(summary.large_context)}
            hint="≥128K"
          />
          <StatCard
            icon={Gauge}
            label={t('models.maxContext')}
            value={fmtCtx(summary.max_context)}
            hint={
              probedCount > 0
                ? t('models.probedCount', {count: probedCount, n: probedCount})
                : undefined
            }
          />
        </div>
      )}

      {/* 搜索与筛选 */}
      <section className="rounded-[20px] bg-muted p-3.5">
        <div className="flex flex-col gap-3 lg:flex-row lg:items-center">
          <div className="relative flex-1">
            <Search className="pointer-events-none absolute left-3 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
            <Input
              value={q}
              onChange={(e) => setQ(e.target.value)}
              placeholder={t('models.searchPlaceholder')}
              className="h-9 bg-background pl-8"
            />
          </div>
          <div className="-mx-0.5 flex flex-wrap items-center gap-1.5 overflow-x-auto px-0.5 pb-0.5">
            <span className="flex shrink-0 items-center gap-1 text-[11px] text-muted-foreground">
              <Layers className="h-3.5 w-3.5" />
              {t('models.series')}
            </span>
            <Button
              variant={series === 'all' ? 'default' : 'outline'}
              size="sm"
              className="h-7 shrink-0 rounded-full px-2.5 text-[11px]"
              onClick={() => setSeries('all')}
            >
              {t('common.all')}
            </Button>
            {summary.series.map((s) => (
              <Button
                key={s}
                variant={series === s ? 'default' : 'outline'}
                size="sm"
                className="h-7 shrink-0 rounded-full px-2.5 text-[11px]"
                onClick={() => setSeries(s)}
              >
                {t(`models.series_${s}`, {defaultValue: ''}) || s}
              </Button>
            ))}
            <span className="ml-1 h-4 w-px shrink-0 bg-border" />
            {(
              [
                ['all', t('common.all')],
                ['reasoning', t('models.reasoning')],
                ['large', t('models.largeContext')],
                ['vision', t('models.vision')],
              ] as const
            ).map(([k, label]) => (
              <Button
                key={k}
                variant={cap === k ? 'default' : 'outline'}
                size="sm"
                className="h-7 shrink-0 rounded-full px-2.5 text-[11px]"
                onClick={() => setCap(k)}
              >
                {label}
              </Button>
            ))}
            <span className="ml-1 h-4 w-px shrink-0 bg-border" />
            <Button
              variant={sort === 'credits' ? 'default' : 'outline'}
              size="sm"
              className="h-7 shrink-0 rounded-full px-2.5 text-[11px]"
              title={t('models.sortByRatioTitle')}
              onClick={() => setSort((v) => (v === 'credits' ? 'default' : 'credits'))}
            >
              {t('models.sortByRatio')}
            </Button>
          </div>
        </div>
      </section>

      {/* 列表 */}
      <section className="overflow-hidden rounded-[20px] bg-muted">
        {loading ? (
          <div className="flex items-center justify-center gap-2 py-16 text-xs text-muted-foreground">
            <Loader2 className="h-4 w-4 animate-spin" />
            {t('models.loadingList')}
          </div>
        ) : error ? (
          <EmptyState icon={Boxes} title={t('models.loadFailed')} description={error} />
        ) : scoped.length === 0 ? (
          <EmptyState
            icon={Boxes}
            title={t('models.noModels')}
            description={t('models.noModelsDesc')}
          />
        ) : filtered.length === 0 ? (
          <EmptyState icon={Search} title={t('models.noMatch')} description={t('models.noMatchDesc')} />
        ) : (
          <>
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow className="border-b border-border/60 hover:bg-transparent">
                    <TableHead className="pl-4 text-[11px] text-muted-foreground">{t('models.colModel')}</TableHead>
                    <TableHead className="text-[11px] text-muted-foreground">{t('models.colContext')}</TableHead>
                    <TableHead className="text-[11px] text-muted-foreground">{t('models.colMaxOutput')}</TableHead>
                    <TableHead className="text-[11px] text-muted-foreground">{t('models.colEfforts')}</TableHead>
                    <TableHead className="text-[11px] text-muted-foreground">{t('models.colProbe')}</TableHead>
                    <TableHead className="pr-4 text-[11px] text-muted-foreground">{t('models.series')}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {filtered.map((m: CatalogModel) => (
                    <TableRow key={m.id} className="border-b border-border/40">
                      <TableCell className="pl-4">
                        {/* 模型描述挂在名称上做悬浮提示：
                            铺在表格里会把行高撑开 */}
                        <div className="flex flex-col gap-0.5 py-0.5" title={m.description || undefined}>
                          {m.name ? (
                            <>
                              <span className="text-xs font-medium">{m.name}</span>
                              <span className="font-mono text-[10px] text-muted-foreground">{m.id}</span>
                            </>
                          ) : (
                            <span className="font-mono text-xs font-medium">{m.id}</span>
                          )}
                        </div>
                      </TableCell>
                      <TableCell className="text-xs tabular-nums text-muted-foreground">
                        {fmtCtx(m.context_length)}
                      </TableCell>
                      <TableCell className="text-xs tabular-nums text-muted-foreground">
                        {fmtCtx(m.max_output_tokens)}
                      </TableCell>
                      <TableCell>
                        {m.efforts.length ? (
                          <div className="flex flex-wrap items-center gap-1">
                            {m.efforts.map((e) => (
                              <Badge key={e} variant="secondary" className="rounded-md font-mono text-[10px]">
                                {e}
                              </Badge>
                            ))}
                            {m.default_effort && (
                              <span
                                className="text-[10px] text-muted-foreground"
                                title={t('models.defaultEffortTitle', {effort: m.default_effort})}
                              >
                                {t('models.defaultEffort', {effort: m.default_effort})}
                              </span>
                            )}
                          </div>
                        ) : (
                          <span className="text-[11px] text-muted-foreground/60">—</span>
                        )}
                      </TableCell>
                      <TableCell>
                        {m.probe ? (
                          <ProbeBadge probe={m.probe} />
                        ) : (
                          <span className="text-[11px] text-muted-foreground/50">—</span>
                        )}
                      </TableCell>
                      <TableCell className="pr-4">
                        <div className="flex flex-wrap items-center justify-end gap-1.5">
                          {/* 积分倍率：同一 prompt 在不同模型上的扣费倍率 */}
                          {m.credits && (
                            <Badge
                              variant="secondary"
                              className="rounded-md font-mono text-[10px]"
                              title={t('models.creditRatioTitle')}
                            >
                              {m.credits}
                            </Badge>
                          )}
                          {m.supports_images && (
                            <Badge variant="secondary" className="rounded-md text-[10px]" title={t('models.visionTitle')}>
                              {t('models.vision')}
                            </Badge>
                          )}
                          {m.only_reasoning && (
                            <Badge
                              variant="secondary"
                              className="rounded-md text-[10px]"
                              title={t('models.onlyReasoningTitle')}
                            >
                              {t('models.onlyReasoning')}
                            </Badge>
                          )}
                          {m.is_default && (
                            <Badge variant="secondary" className="rounded-md text-[10px]" title={t('models.defaultTitle')}>
                              {t('models.default')}
                            </Badge>
                          )}
                          <Badge variant="secondary" className="rounded-md text-[10px]">
                            {t(`models.series_${m.series}`, {defaultValue: ''}) || m.series}
                          </Badge>
                        </div>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
            {(filtered.length !== scoped.length || q.trim() || series !== 'all' || cap !== 'all') && (
              <div className="border-t border-border/40 px-4 py-2 text-[11px] text-muted-foreground">
                {t('models.filteredCount', {n: filtered.length, total: scoped.length})}
              </div>
            )}
          </>
        )}
      </section>

      <p className="text-[10px] leading-4 text-muted-foreground/70">
        {t('models.footnote')}
      </p>
    </div>
  );
}
