'use client';

import {useCallback, useEffect, useMemo, useState} from 'react';
import {
  ShieldAlert,
  RefreshCw,
  Save,
  Lock,
  ScrollText,
  ServerCrash,
} from 'lucide-react';
import {useHeartbeat} from '@/lib/use-heartbeat';
import {useCachedAsync} from '@/lib/data-cache';
import {notify} from '@/lib/toast';
import {securityApi, errText} from '@/lib/api';
import type {
  BlockedLogEntry,
  ModelLockRow,
  SecurityRules,
} from '@/lib/types';
import {fmtDateTime, fmtRemain} from '@/lib/format';
import {PageHeader} from '@/components/common/layout/PageHeader';
import {EmptyState} from '@/components/common/layout/EmptyState';
import {TableSkeleton} from '@/components/common/layout/LoadSkeleton';
import {useAuth} from '@/lib/auth-context';
import {useT} from '@/lib/i18n/provider';
import {RichText} from '@/lib/i18n/rich-text';
import {Button} from '@/components/ui/button';
import {Badge} from '@/components/ui/badge';
import {Input} from '@/components/ui/input';
import {Label} from '@/components/ui/label';
import {Switch} from '@/components/ui/switch';
import {Textarea} from '@/components/ui/textarea';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';

/** 拦截原因短码 → i18n 键；未收录的短码原样展示（不编造解释） */
const REASON_KEYS: Record<string, string> = {
  missing_key: 'security.reasonMissingKey',
  invalid_key: 'security.reasonInvalidKey',
  ip_blocked: 'security.reasonIpBlocked',
  ip_not_whitelisted: 'security.reasonIpNotWhitelisted',
};

function reasonText(t: (k: string) => string, reason: string): string {
  const key = REASON_KEYS[reason];
  return key ? t(key) : reason;
}

/** 多行 CIDR 文本 → 去重非空数组 */
function toLines(v: string): string[] {
  const seen = new Set<string>();
  for (const raw of v.split(/[\n,]/)) {
    const s = raw.trim();
    if (s) seen.add(s);
  }
  return [...seen];
}

/** 把表单草稿还原成多行文本 */
function fromLines(list: string[] | undefined): string {
  return (list ?? []).join('\n');
}

/** 表单草稿：textarea 原文 + 未保存标记 */
interface RulesDraft {
  blacklist: string;
  whitelist: string;
  whitelistMode: boolean;
  trustedProxies: string;
  trustedHops: string;
}

function draftFromRules(rules: SecurityRules): RulesDraft {
  return {
    blacklist: fromLines(rules.ip_blacklist),
    whitelist: fromLines(rules.ip_whitelist),
    whitelistMode: !!rules.ip_whitelist_mode,
    trustedProxies: fromLines(rules.trusted_proxy_cidrs),
    trustedHops: String(rules.trusted_proxy_hops ?? 0),
  };
}

export default function SecurityPage() {
  const t = useT();
  const {isAdmin} = useAuth();

  /* ── 规则 + 拦截日志（同一端点一次取回）────────────────────── */
  // 首载错误态捕获进 fetcher（useCachedAsync 吞 rejection，见 dashboard 同款注释）
  const [loadError, setLoadError] = useState<string | null>(null);
  const secCache = useCachedAsync(
    'security',
    () =>
      securityApi.status().catch((e) => {
        setLoadError((prev) => prev ?? errText(e));
        throw e;
      }),
  );
  const rules: SecurityRules | null = secCache.data?.rules ?? null;
  const blockedLogs: BlockedLogEntry[] = useMemo(
    () => secCache.data?.blocked_logs ?? [],
    [secCache.data],
  );

  /** 表单草稿：首次拿到 rules 时初始化；此后与缓存解耦（编辑中不被刷新覆盖） */
  const [draft, setDraft] = useState<RulesDraft | null>(null);
  useEffect(() => {
    if (rules && !draft) setDraft(draftFromRules(rules));
    // 仅在 rules 首次到达时初始化一次
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rules]);

  const [saving, setSaving] = useState(false);
  /** 后端校验错误清单（errs 非空 = 整体未生效） */
  const [ruleErrs, setRuleErrs] = useState<string[] | null>(null);

  /* ── 模型锁池：独立缓存条目 + 30s 轮询 ───────────────────── */
  const locksCache = useCachedAsync('security-model-locks', () => securityApi.modelLocks());
  const locks: ModelLockRow[] = useMemo(
    () => locksCache.data?.locks ?? [],
    [locksCache.data],
  );
  useHeartbeat(locksCache.refresh, 30000);

  const retryLoad = useCallback(() => {
    setLoadError(null);
    secCache.refresh().catch((e) => {
      setLoadError(errText(e));
    });
  }, [secCache]);

  const saveRules = useCallback(async () => {
    if (!draft) return;
    setSaving(true);
    setRuleErrs(null);
    try {
      const res = await securityApi.saveRules({
        ip_blacklist: toLines(draft.blacklist),
        ip_whitelist: toLines(draft.whitelist),
        ip_whitelist_mode: draft.whitelistMode,
        trusted_proxy_cidrs: toLines(draft.trustedProxies),
        trusted_proxy_hops: Math.max(0, Math.floor(Number(draft.trustedHops) || 0)),
      });
      // errs 非空 = 校验未通过、整体未生效：逐条展示，不清草稿
      if (res.errs?.length) {
        setRuleErrs(res.errs);
        notify.err(t('security.rulesRejected'));
        return;
      }
      notify.ok(t('security.configSaved'));
      // 成功后重新拉权威值（后端规范化后的列表），草稿跟随刷新
      setDraft(null);
      secCache.refresh().catch(() => {/* 心跳/下轮兜底 */});
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setSaving(false);
    }
  }, [draft, secCache, t]);

  const header = (
    <PageHeader
      title={t('security.title')}
      description={t('security.description')}
      actions={
        isAdmin ? (
          <Button size="sm" className="rounded-full" onClick={retryLoad}>
            <RefreshCw />
            {t('common.refresh')}
          </Button>
        ) : null
      }
    />
  );

  // 首载失败且无缓存可展示：错误态（与「暂无记录」空态严格区分）
  if (loadError && !secCache.loading && !secCache.data) {
    return (
      <div className="flex flex-col gap-4 md:gap-6">
        {header}
        <section className="overflow-hidden rounded-[20px] bg-muted">
          <EmptyState
            icon={ServerCrash}
            title={t('security.loadErrorTitle')}
            description={loadError}
            className="flex flex-col items-center justify-center py-16 text-center"
          >
            <Button className="mt-4 rounded-full" onClick={retryLoad}>
              <RefreshCw />
              {t('common.refresh')}
            </Button>
          </EmptyState>
        </section>
      </div>
    );
  }

  const loading = secCache.loading && !secCache.data;

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      {header}

      {loading ? (
        <TableSkeleton rows={5} />
      ) : (
        <>
          {/* ── 区块 1：入站 IP 规则 ─────────────────────────── */}
          <section className="rounded-[20px] bg-muted p-4 md:p-5">
            <div className="mb-4 flex items-center gap-2">
              <ShieldAlert className="h-4 w-4 text-muted-foreground" />
              <h2 className="text-sm font-semibold">{t('security.policy')}</h2>
            </div>
            {draft && isAdmin ? (
              <div className="space-y-4">
                <div className="grid gap-4 md:grid-cols-2">
                  <div className="space-y-1.5">
                    <Label className="text-[11px] text-muted-foreground">{t('security.ipBlacklist')}</Label>
                    <Textarea
                      rows={4}
                      value={draft.blacklist}
                      onChange={(e) => setDraft({...draft, blacklist: e.target.value})}
                      placeholder={'203.0.113.0/24\n1.2.3.4'}
                    />
                    <p className="text-[10px] leading-4 text-muted-foreground">{t('security.cidrHint')}</p>
                  </div>
                  <div className="space-y-1.5">
                    <Label className="text-[11px] text-muted-foreground">{t('security.ipWhitelist')}</Label>
                    <Textarea
                      rows={4}
                      value={draft.whitelist}
                      onChange={(e) => setDraft({...draft, whitelist: e.target.value})}
                      placeholder={'10.0.0.0/8\n192.168.1.0/24'}
                    />
                    <div className="flex items-center gap-2">
                      <Switch
                        id="sec-whitelist-mode"
                        checked={draft.whitelistMode}
                        onCheckedChange={(v) => setDraft({...draft, whitelistMode: v})}
                      />
                      <Label htmlFor="sec-whitelist-mode" className="cursor-pointer text-xs">
                        {t('security.whitelistMode')}
                      </Label>
                    </div>
                    <p className="text-[10px] leading-4 text-muted-foreground">
                      {draft.whitelistMode ? t('security.whitelistModeOn') : t('security.whitelistModeOff')}
                    </p>
                  </div>
                </div>

                {/* 可信代理设置：只在网关处于反代/CDN 之后时才配置，
                    否则 X-Forwarded-For 可被客户端伪造绕过 IP 管控。 */}
                <div className="rounded-2xl border p-3">
                  <div className="grid gap-3 md:grid-cols-[1fr_140px]">
                    <div className="space-y-1.5">
                      <Label className="text-[11px] text-muted-foreground">{t('security.trustedProxies')}</Label>
                      <Textarea
                        rows={2}
                        value={draft.trustedProxies}
                        onChange={(e) => setDraft({...draft, trustedProxies: e.target.value})}
                        placeholder={'173.245.48.0/20\n10.0.0.0/8'}
                      />
                    </div>
                    <div className="space-y-1.5">
                      <Label className="text-[11px] text-muted-foreground">{t('security.trustedHops')}</Label>
                      <Input
                        type="number"
                        min={0}
                        value={draft.trustedHops}
                        onChange={(e) => setDraft({...draft, trustedHops: e.target.value})}
                      />
                    </div>
                  </div>
                  {/* 警示：不设可信代理时伪造头可绕过 IP 管控——这条必须让用户看到 */}
                  <p className="mt-2 text-[10px] leading-4 text-amber-600 dark:text-amber-400">
                    <RichText text={t('security.trustedProxyWarning')} />
                  </p>
                </div>

                {/* 后端校验错误清单：errs 非空则整体未生效，逐条原样展示 */}
                {ruleErrs && ruleErrs.length > 0 && (
                  <div className="space-y-1 rounded-2xl border border-red-500/40 bg-red-500/5 p-3">
                    <p className="text-xs font-medium text-red-600 dark:text-red-400">{t('security.rulesRejected')}</p>
                    <ul className="list-inside list-disc space-y-0.5">
                      {ruleErrs.map((err, i) => (
                        <li key={i} className="text-[11px] text-red-600 dark:text-red-400">{err}</li>
                      ))}
                    </ul>
                  </div>
                )}

                <div className="flex justify-end">
                  <Button className="rounded-full" onClick={saveRules} disabled={saving}>
                    <Save />
                    {saving ? t('common.loading') : t('common.save')}
                  </Button>
                </div>
              </div>
            ) : (
              // 只读视图（viewer 角色 / 草稿未就绪）：直接展示当前生效列表
              <div className="space-y-3 text-xs text-muted-foreground">
                <div>
                  <span className="font-medium text-foreground">{t('security.ipBlacklist')}</span>
                  <span className="ml-2 font-mono">{rules?.ip_blacklist?.length ? rules.ip_blacklist.join('  ') : '—'}</span>
                </div>
                <div>
                  <span className="font-medium text-foreground">{t('security.ipWhitelist')}</span>
                  <span className="ml-2 font-mono">{rules?.ip_whitelist?.length ? rules.ip_whitelist.join('  ') : '—'}</span>
                  <Badge variant="secondary" className="ml-2 rounded-full text-[10px]">
                    {rules?.ip_whitelist_mode ? t('security.whitelistModeOn') : t('security.whitelistModeOff')}
                  </Badge>
                </div>
                <div>
                  <span className="font-medium text-foreground">{t('security.trustedProxies')}</span>
                  <span className="ml-2 font-mono">{rules?.trusted_proxy_cidrs?.length ? rules.trusted_proxy_cidrs.join('  ') : '—'}</span>
                  <span className="ml-2">hops={rules?.trusted_proxy_hops ?? 0}</span>
                </div>
                {!isAdmin && <p className="text-[10px]">{t('security.readonlyNote')}</p>}
              </div>
            )}
          </section>

          {/* ── 区块 2：拦截日志 ─────────────────────────────── */}
          <section className="overflow-hidden rounded-[20px] bg-muted">
            <div className="flex items-center justify-between px-4 pt-4 md:px-5">
              <div className="flex items-center gap-2">
                <ScrollText className="h-4 w-4 text-muted-foreground" />
                <h2 className="text-sm font-semibold">{t('security.blockedLogTitle')}</h2>
              </div>
              <span className="text-[10px] text-muted-foreground">
                {t('security.blockedLogCount', {n: Math.min(blockedLogs.length, 200)})}
              </span>
            </div>
            <p className="px-4 pt-1 text-[10px] text-muted-foreground md:px-5">{t('security.blockedLogNote')}</p>
            {blockedLogs.length === 0 ? (
              <EmptyState
                icon={ScrollText}
                title={t('security.noBlockedLogs')}
                description={t('security.noBlockedLogsDesc')}
                className="flex flex-col items-center justify-center py-12 text-center"
              />
            ) : (
              <div className="p-2 md:p-4">
                <Table>
                  <TableHeader>
                    <TableRow className="border-b border-border/60 hover:bg-transparent">
                      <TableHead className="pl-4 text-[11px] text-muted-foreground">{t('security.colTime')}</TableHead>
                      <TableHead className="text-[11px] text-muted-foreground">{t('security.colIp')}</TableHead>
                      <TableHead className="text-[11px] text-muted-foreground">{t('security.path')}</TableHead>
                      <TableHead className="pr-4 text-[11px] text-muted-foreground">{t('security.colReason')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {/* 后端环形缓冲上限 512，界面只显示最近 200 条 */}
                    {blockedLogs.slice(0, 200).map((entry, i) => (
                      <TableRow key={`${entry.ts}-${i}`} className="border-b border-border/40">
                        <TableCell className="pl-4 text-xs tabular-nums text-muted-foreground">
                          {fmtDateTime(entry.ts)}
                        </TableCell>
                        <TableCell className="font-mono text-xs">{entry.ip}</TableCell>
                        <TableCell className="max-w-[280px] truncate font-mono text-xs text-muted-foreground" title={entry.path}>
                          {entry.path}
                        </TableCell>
                        <TableCell className="pr-4">
                          <Badge variant="destructive" className="rounded-full text-[10px] font-normal">
                            {reasonText(t, entry.reason)}
                          </Badge>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
            )}
          </section>

          {/* ── 区块 3：模型锁池 ─────────────────────────────── */}
          <section className="rounded-[20px] bg-muted p-4 md:p-5">
            <div className="mb-1 flex items-center gap-2">
              <Lock className="h-4 w-4 text-muted-foreground" />
              <h2 className="text-sm font-semibold">{t('security.modelLocksTitle')}</h2>
            </div>
            <p className="mb-4 text-[10px] text-muted-foreground">{t('security.modelLocksNote')}</p>
            {locks.length === 0 ? (
              <EmptyState
                icon={Lock}
                title={t('security.noModelLocks')}
                description={t('security.noModelLocksDesc')}
                className="flex flex-col items-center justify-center py-10 text-center"
              />
            ) : (
              <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
                {locks.map((lock) => (
                  <ModelLockCard key={`${lock.realm}:${lock.model}`} lock={lock} />
                ))}
              </div>
            )}
          </section>
        </>
      )}
    </div>
  );
}

/** 模型锁池单卡。state 徽章配色：locked 红 / starved 琥珀 / partial 绿。 */
function ModelLockCard({lock}: {lock: ModelLockRow}) {
  const t = useT();
  // 倒计时文案随刷新（30s 轮询 + 心跳）自然更新；这里不做秒级 ticker——
  // 锁的粒度是分钟级，秒级重渲染纯浪费。
  const remain = lock.fully_unlock_at ? Math.max(0, lock.fully_unlock_at - Date.now() / 1000) : 0;
  const badgeCls =
    lock.state === 'locked'
      ? 'bg-red-500/10 text-red-600 dark:text-red-400'
      : lock.state === 'starved'
        ? 'bg-amber-500/10 text-amber-600 dark:text-amber-400'
        : 'bg-emerald-500/10 text-emerald-600 dark:text-emerald-400';
  return (
    <div className="rounded-2xl border bg-background/60 p-3.5">
      <div className="flex items-center justify-between gap-2">
        <code className="min-w-0 flex-1 truncate font-mono text-sm font-medium" title={lock.model}>
          {lock.model}
        </code>
        <Badge className={`rounded-full border-0 text-[10px] ${badgeCls}`}>
          {t(`security.lockState_${lock.state}`)}
        </Badge>
      </div>
      <div className="mt-2 flex items-center justify-between gap-2 text-[11px] text-muted-foreground">
        <span>
          {t('security.lockServable', {servable: lock.servable, total: lock.total, locked: lock.locked})}
        </span>
        <span className="shrink-0 text-[10px]">{t('realm.' + (lock.realm === 'global' ? 'global' : 'cn'))}</span>
      </div>
      {lock.locked > 0 && lock.unlock_at > 0 && (
        <div className="mt-2 flex items-center justify-between gap-2 text-[11px]">
          <span className="text-muted-foreground">{t('security.lockUnlockAt')}</span>
          <span className="tabular-nums">
            {fmtDateTime(lock.unlock_at)}
            {remain > 0 && <span className="ml-1 text-[10px] text-muted-foreground">({fmtRemain(remain)})</span>}
          </span>
        </div>
      )}
      {lock.reason && (
        <p className="mt-1.5 truncate text-[10px] text-muted-foreground" title={lock.reason}>
          {t('security.lockReason', {reason: lock.reason})}
        </p>
      )}
    </div>
  );
}
