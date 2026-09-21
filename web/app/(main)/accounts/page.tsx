'use client';

import {useCallback, useEffect, useMemo, useState} from 'react';
import {
  Gift,
  Pause,
  Play,
  Trash2,
  Plus,
  Users,
  CalendarCheck,
  Coins,
  RefreshCw,
  HeartPulse,
} from 'lucide-react';
import {useHeartbeat} from '@/lib/use-heartbeat';
import {notify} from '@/lib/toast';
import {accountApi, errText} from '@/lib/api';
import type {Account, CreditPackage} from '@/lib/types';
import {fmtNumber} from '@/lib/format';
import {
  availabilityLabelKey,
  availabilityOf,
  isDegraded,
  rateLimitedModels,
} from '@/lib/account-status';
import {PageHeader} from '@/components/common/layout/PageHeader';
import {EmptyState} from '@/components/common/layout/EmptyState';
import {ConfirmDialog} from '@/components/common/layout/ConfirmDialog';
import {AddAccountDialog} from '@/components/common/accounts/AddAccountDialog';
import {CreditCountdown} from '@/components/common/accounts/CreditCountdown';
import {useAuth} from '@/lib/auth-context';
import {realmLabel, useRealm} from '@/lib/realm-context';
import {useT} from '@/lib/i18n/provider';
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

export default function AccountsPage() {
  const {realm} = useRealm();
  const t = useT();
  const {isAdmin} = useAuth();
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [loading, setLoading] = useState(true);
  const [addOpen, setAddOpen] = useState(false);
  const [busyUid, setBusyUid] = useState<string | null>(null);
  const [checkinAllBusy, setCheckinAllBusy] = useState(false);
  const [balanceAllBusy, setBalanceAllBusy] = useState(false);
  /** 实时积分（按 uid），叠加到账号上；packages 查询失败时用池快照值 */
  const [liveCredits, setLiveCredits] = useState<Record<string, number>>({});
  /** 每账号的积分包明细（到期倒计时用） */
  const [creditPacks, setCreditPacks] = useState<Record<string, CreditPackage[]>>({});

  /**
   * 首次加载：overview（池快照）与 packages（实时余额）并行发、都落定后
   * 一次性写入。分开写会让积分先渲染池快照值、约 1 秒后被实时值覆盖，
   * 界面上数字闪一下——快照只是 packages 失败时的降级，不该先出来。
   */
  const load = useCallback(async () => {
    setLoading(true);
    try {
      const [ov, pk] = await Promise.allSettled([accountApi.overview(), accountApi.packages()]);
      // packages 拉取失败时静默降级：仍显示池快照的 credits（原始语义）
      if (pk.status === 'fulfilled') {
        const credits: Record<string, number> = {};
        const packs: Record<string, CreditPackage[]> = {};
        for (const row of pk.value.accounts) {
          packs[row.uid] = row.packages ?? [];
          if (typeof row.remain === 'number' && !row.error) credits[row.uid] = row.remain;
        }
        setLiveCredits(credits);
        setCreditPacks(packs);
      }
      if (ov.status === 'fulfilled') {
        setAccounts(ov.value.accounts ?? []);
      } else {
        throw ov.reason;
      }
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  // 池状态（冷却 / 成功计数等）会随时间变化，页面停留时定时刷新。
  // 心跳沿用同一个 load：packages 会跟着重拉，accounts 与实时余额仍然
  // 同帧落定，不会出现「先渲染旧积分再替换」的闪变路径。
  useHeartbeat(load, 30000);

  /** 全量刷新余额：同步等待（完成后池内 credits 即最新值） */
  const refreshBalanceAll = useCallback(async () => {
    setBalanceAllBusy(true);
    try {
      await accountApi.balanceAll();
      notify.ok(t('accounts.balanceAllDone'));
      await load();
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setBalanceAllBusy(false);
    }
  }, [load, t]);

  /** 批量签到：异步触发（进度看日志频道） */
  const checkinAll = useCallback(async () => {
    setCheckinAllBusy(true);
    try {
      await accountApi.checkinAll();
      notify.ok(t('accounts.checkinAllStarted'), t('accounts.checkinAllStartedDetail'));
      await load();
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setCheckinAllBusy(false);
    }
  }, [load, t]);

  /** 按当前版本过滤（Go 单实例双版本共存；存量无 realm 视为 cn） */
  const visible = useMemo(
    () => accounts.filter((a) => (a.realm ?? 'cn') === realm),
    [accounts, realm],
  );

  /** 执行单账号操作（签到 / 余额 / 复活 / 停用 / 删除），成功后刷新列表 */
  async function run(uid: string, fn: () => Promise<unknown>, okMsg: string) {
    setBusyUid(uid);
    try {
      const res = (await fn()) as {
        message?: string;
        ok?: boolean;
        credits?: number | null;
        checkin_message?: string;
      };
      const ok = res.ok !== false;
      // 签到会返回刷新后的实时积分，直接就地更新，省一次请求
      if (typeof res.credits === 'number') {
        setLiveCredits((prev) => ({...prev, [uid]: res.credits as number}));
      }
      (ok ? notify.ok : notify.err)(res.message || res.checkin_message || okMsg);
      await load();
      window.dispatchEvent(new Event('workbuddy-manager:accounts-changed'));
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setBusyUid(null);
    }
  }

  /** 账号状态徽章（表格与移动端卡片共用）。
   *  分档顺序与文案都取自 `lib/account-status`——首页的健康快照用同一套判定。 */
  function renderStatus(a: Account) {
    const tier = availabilityOf(a);
    const label = t(availabilityLabelKey(tier, a));

    if (tier === 'disabled') {
      const reason = String(a.disabled_reason || '');
      return (
        <Badge
          variant="destructive"
          className="rounded-full"
          title={reason ? t('accounts.disabledReason', {reason}) : undefined}
        >
          {label}
        </Badge>
      );
    }
    if (tier === 'manualDisabled') {
      // 上游状态位停用：中性灰。它「还在池里、签到与保活照常」，用户看不出
      // 机制差别，提示文案必须写清楚。停用原因也一并带上。
      const reason = String(a.manual_reason || '');
      const nl = String.fromCharCode(10);
      return (
        <Badge
          variant="secondary"
          className="rounded-full text-muted-foreground"
          title={t('accounts.badgeManualDisabledTitle') + (reason ? nl + reason : '')}
        >
          {label}
        </Badge>
      );
    }
    if (tier === 'cooling') {
      // 带上「还要等多久」：只写「冷却中」的话用户不知道是几秒还是几小时。
      const secs = a.cool_remaining_sec;
      const left = typeof secs === 'number' && secs > 0 ? fmtNumber(secs) + 's' : '';
      // 按**原因**分组展示：6004 → 这个模型被限流了；11102 → 这个账号没有该模型。
      const limited: string[] = [];
      const missing: string[] = [];
      const sep = t('common.listSeparator');
      for (const m of a.rate_limited_models ?? []) {
        const r = m.reason ?? '';
        (r.startsWith('11102') ? missing : limited).push(m.model);
      }
      const tip = [
        isDegraded(a) ? t('accounts.degradedReason', {n: a.consecutive_fails ?? 0}) : '',
        left ? t('accounts.etaRecovery', {left}) : '',
        limited.length ? t('accounts.limitedModels', {models: limited.join(sep)}) : '',
        missing.length ? t('accounts.missingModels', {models: missing.join(sep)}) : '',
      ]
        .filter(Boolean)
        .join('\n');
      const total = limited.length + missing.length;
      return (
        <Badge
          variant="secondary"
          className="rounded-full text-amber-600 dark:text-amber-400"
          title={tip || undefined}
        >
          {label}{left ? ` · ${left}` : ''}
          {total > 0 && <span className="ml-1 opacity-70">{t('accounts.modelsCount', {count: total, n: total})}</span>}
        </Badge>
      );
    }
    // 「一直在失败，但状态看着正常」——上游对未命中规则的 4xx 只「换号不罚」。
    if (tier === 'neverSucceeded') {
      const errs = typeof a.err_total === 'number' ? a.err_total : 0;
      return (
        <Badge
          variant="secondary"
          className="rounded-full text-rose-600 dark:text-rose-400"
          title={t('accounts.badgeNeverSucceededTitle', {errs})}
        >
          {label}
        </Badge>
      );
    }
    return (
      <Badge variant="secondary" className="rounded-full text-emerald-600 dark:text-emerald-400">
        {label}
      </Badge>
    );
  }

  /** 模型级限流标记：账号本身在线，只是某个模型暂时被腾讯限流（6004）。 */
  function renderModelLimit(a: Account) {
    const limited = rateLimitedModels(a);
    if (!limited.length) return null;
    const lines = limited.map((m) => {
      const until = a.rate_limited_models?.find((x) => x.model === m.model)?.until;
      const at = until ? new Date(until).toLocaleTimeString() : '';
      const why = m.reason.startsWith('11102')
        ? t('accounts.modelMissing')
        : at
          ? t('accounts.modelLimitUntil', {at})
          : t('accounts.modelLimited');
      return `${m.model} · ${why}`;
    });
    const first = limited[0];
    const shown = limited.length === 1
      ? first.model
      : t('accounts.modelsCount', {count: limited.length, n: limited.length});
    return (
      <Badge
        variant="secondary"
        className="rounded-full text-amber-600 dark:text-amber-400"
        title={[t('accounts.modelLimitTitle'), ...lines].join(String.fromCharCode(10))}
      >
        {t('accounts.modelLimitBadge', {models: shown})}
      </Badge>
    );
  }

  /** 积分余额 + 到期倒计时 */
  function renderCredits(a: Account) {
    const value = liveCredits[a.uid] ?? a.credits;
    const hasValue = typeof value === 'number';
    return (
      <span className="inline-flex items-center gap-1.5">
        <span
          className={
            'text-xs font-medium tabular-nums ' +
            (!hasValue
              ? 'text-muted-foreground'
              : (value as number) <= 0
                ? 'text-red-600 dark:text-red-400'
                : (value as number) < 200
                  ? 'text-amber-600 dark:text-amber-400'
                  : 'text-foreground')
          }
          title={t('accounts.creditsTitle')}
        >
          {hasValue ? fmtNumber(value as number) : '—'}
        </span>
        <CreditCountdown packages={creditPacks[a.uid]} />
      </span>
    );
  }

  /** 单账号操作按钮组 */
  function renderActions(a: Account) {
    const busy = busyUid === a.uid;
    // 国际版没有签到体系（Go 侧调度对 global 账号直接过滤）。
    const canCheckin = (a.realm ?? 'cn') === 'cn';
    const off = a.disabled === true;
    const manualOff = a.manual_disabled === true;
    return (
      <div className="flex justify-end gap-1">
        {/* 签到：手动触发单号签到 + 余额查询解冻 */}
        {canCheckin && !off && (
          <Button variant="ghost" size="icon" className="h-7 w-7 rounded-md" title={t('accounts.checkin')} disabled={busy}
            onClick={() => run(a.uid, () => accountApi.checkin(a.uid), t('accounts.opDone'))}>
            <Gift className="h-3.5 w-3.5" />
          </Button>
        )}
        {/* 查余额：直接向腾讯查询并写回池内 credits */}
        <Button variant="ghost" size="icon" className="h-7 w-7 rounded-md" title={t('accounts.balance')} disabled={busy}
          onClick={() => run(a.uid, () => accountApi.balance(a.uid), t('accounts.balanceDone'))}>
          <Coins className="h-3.5 w-3.5" />
        </Button>
        {/* 停用 / 复活。复活是运维口径：清禁用 + 冷却 + 熔断 */}
        {off ? (
          <Button
            variant="ghost"
            size="icon"
            className="h-7 w-7 rounded-md text-emerald-600 hover:text-emerald-600"
            title={t('accounts.reviveTitle')}
            disabled={busy}
            onClick={() => run(a.uid, () => accountApi.revive(a.uid), t('accounts.revived'))}
          >
            <HeartPulse className="h-3.5 w-3.5" />
          </Button>
        ) : (
          <Button
            variant="ghost"
            size="icon"
            className={'h-7 w-7 rounded-md ' + (manualOff ? 'text-emerald-600 hover:text-emerald-600' : 'text-amber-600 hover:text-amber-600')}
            title={manualOff ? t('accounts.enableTitle') : t('accounts.disableTitle')}
            disabled={busy}
            onClick={() =>
              manualOff
                ? run(a.uid, () => accountApi.revive(a.uid), t('accounts.enabled'))
                : run(a.uid, () => accountApi.disable(a.uid), t('accounts.disabled'))
            }
          >
            {manualOff ? <Play className="h-3.5 w-3.5" /> : <Pause className="h-3.5 w-3.5" />}
          </Button>
        )}
        <ConfirmDialog
          title={t('accounts.deleteTitle', {name: a.nickname || a.uid})}
          description={t('accounts.deleteDesc')}
          confirmText={t('accounts.delete')}
          destructive
          onConfirm={() => run(a.uid, () => accountApi.remove(a.uid), t('accounts.deleted'))}
          trigger={
            <Button variant="ghost" size="icon" className="h-7 w-7 rounded-md text-red-500 hover:text-red-600" title={t('accounts.delete')}>
              <Trash2 className="h-3.5 w-3.5" />
            </Button>
          }
        />
      </div>
    );
  }

  /** 头像（首字母） */
  function renderAvatar(a: Account) {
    return (
      <div
        className={
          'grid h-7 w-7 shrink-0 place-items-center rounded-full text-[11px] font-semibold ' +
          (a.disabled ? 'bg-muted-foreground/20 text-muted-foreground' : 'bg-primary text-primary-foreground')
        }
      >
        {(a.nickname || '?').charAt(0)}
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader
        title={t('accounts.title')}
        description={
          realm === 'global'
            ? t('accounts.descGlobal')
            : t('accounts.descCn')
        }
        actions={
          <>
            <Button
              size="sm"
              variant="outline"
              className="rounded-full"
              onClick={refreshBalanceAll}
              disabled={balanceAllBusy || !visible.length}
              title={t('accounts.refreshBalanceTitle')}
            >
              <RefreshCw className={balanceAllBusy ? 'animate-spin' : ''} />
              <span className="hidden sm:inline">{t('accounts.refreshBalance')}</span>
              <span className="sm:hidden">{t('accounts.balanceShort')}</span>
            </Button>
            {/* 「全部签到」仅国内版显示：国际版没有签到体系 */}
            {isAdmin && realm === 'cn' && (
              <Button
                size="sm"
                variant="outline"
                className="rounded-full"
                onClick={checkinAll}
                disabled={checkinAllBusy || !visible.length}
              >
                <CalendarCheck className={checkinAllBusy ? 'animate-pulse' : ''} />
                {t('accounts.checkinAll')}
              </Button>
            )}
            {isAdmin && (
              <Button size="sm" className="rounded-full" onClick={() => setAddOpen(true)}>
                <Plus />
                {t('accounts.addAccount')}
              </Button>
            )}
          </>
        }
      />

      <section className="overflow-hidden rounded-[20px] bg-muted">
        {/* 手机端：卡片列表。表格 6 列在窄屏需要横向滚动，改为纵向卡片 */}
        <div className="divide-y divide-border/40 md:hidden">
          {visible.map((a) => (
            <div key={a.uid} className="space-y-2.5 px-3.5 py-3">
              <div className="flex items-center justify-between gap-2">
                <div className="flex min-w-0 items-center gap-2.5">
                  {renderAvatar(a)}
                  <div className="min-w-0">
                    <div
                      className={
                        'truncate text-sm font-medium ' + (a.disabled ? 'text-muted-foreground' : '')
                      }
                    >
                      {a.nickname || t('accounts.unnamed')}
                    </div>
                    <div className="truncate font-mono text-[10px] text-muted-foreground">{a.uid}</div>
                  </div>
                </div>
                <div className="flex flex-wrap items-center justify-end gap-1.5">
                  {renderStatus(a)}
                  {renderModelLimit(a)}
                </div>
              </div>

              <div className="flex items-center justify-between gap-3">
                <div className="flex shrink-0 items-center gap-1.5">
                  <Coins className="h-3.5 w-3.5 text-muted-foreground" />
                  {renderCredits(a)}
                </div>
                {isAdmin && renderActions(a)}
              </div>
            </div>
          ))}
          {!visible.length && !loading && (
            <div className="px-4 py-12 text-center text-xs text-muted-foreground">{t('accounts.tableEmpty')}</div>
          )}
          {loading && !accounts.length && (
            <div className="px-4 py-12 text-center text-xs text-muted-foreground">{t('common.loading')}</div>
          )}
        </div>

        {/* 桌面端：表格 */}
        <div className="hidden md:block">
        <Table>
          <TableHeader>
            <TableRow className="border-b border-border/60 hover:bg-transparent">
              <TableHead className="pl-4 text-[11px] text-muted-foreground">{t('accounts.colNickname')}</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">UID</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">{t('accounts.colStatus')}</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">{t('metric.credits')}</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">{t('accounts.colRequests')}</TableHead>
              {isAdmin && <TableHead className="pr-4 text-right text-[11px] text-muted-foreground">{t('accounts.colActions')}</TableHead>}
            </TableRow>
          </TableHeader>
          <TableBody>
            {visible.map((a) => (
              <TableRow key={a.uid} className="border-b border-border/40">
                <TableCell className="pl-4">
                  <div className="flex items-center gap-2.5">
                    {renderAvatar(a)}
                    <span className={'truncate text-sm font-medium ' + (a.disabled ? 'text-muted-foreground' : '')}>
                      {a.nickname || t('accounts.unnamed')}
                    </span>
                    <Badge
                      variant="secondary"
                      className={
                        'shrink-0 rounded-md px-1.5 py-0 text-[10px] ' +
                        ((a.realm ?? 'cn') === 'global'
                          ? 'bg-sky-500/10 text-sky-700 dark:text-sky-400'
                          : '')
                      }
                    >
                      {realmLabel(a.realm)}
                    </Badge>
                  </div>
                </TableCell>
                <TableCell className="font-mono text-xs text-muted-foreground">{a.uid}</TableCell>
                <TableCell>
                  <div className="flex flex-wrap items-center gap-1.5">
                    {renderStatus(a)}
                    {renderModelLimit(a)}
                  </div>
                </TableCell>
                <TableCell>{renderCredits(a)}</TableCell>
                <TableCell className="text-xs tabular-nums">
                  <span className="text-emerald-600 dark:text-emerald-400">{fmtNumber(a.success_count ?? 0)}</span>
                  <span className="mx-1 text-muted-foreground/40">/</span>
                  <span className="text-red-600 dark:text-red-400">{fmtNumber(a.err_total ?? 0)}</span>
                </TableCell>
                {isAdmin && <TableCell className="pr-4">{renderActions(a)}</TableCell>}
              </TableRow>
            ))}
          </TableBody>
        </Table>
        </div>

        {!visible.length && !loading && (
          <EmptyState
            icon={Users}
            title={t('accounts.emptyTitle')}
            description={t('accounts.emptyDescAdmin')}
            className="flex flex-col items-center justify-center py-16 text-center"
          >
            {isAdmin && (
              <Button className="mt-4 rounded-full" onClick={() => setAddOpen(true)}>
                <Plus />
                {t('accounts.addAccount')}
              </Button>
            )}
          </EmptyState>
        )}
        {loading && !accounts.length && (
          <div className="py-16 text-center text-xs text-muted-foreground">{t('common.loading')}</div>
        )}
      </section>

      <AddAccountDialog open={addOpen} onOpenChange={setAddOpen} onSuccess={load} />
    </div>
  );
}
