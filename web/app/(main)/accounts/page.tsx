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
  ShieldOff,
  ArrowDown,
  ArrowUp,
  ArrowUpDown,
} from 'lucide-react';
import {useHeartbeat} from '@/lib/use-heartbeat';
import {notify} from '@/lib/toast';
import {accountApi, errText} from '@/lib/api';
import {useCachedAsync} from '@/lib/data-cache';
import type {Account, CreditPackage, OverviewResponse, PackagesResponse} from '@/lib/types';
import {fmtNumber} from '@/lib/format';
import {
  availabilityLabelKey,
  availabilityOf,
  isDegraded,
  rateLimitedModels,
} from '@/lib/account-status';
import {PageHeader} from '@/components/common/layout/PageHeader';
import {EmptyState} from '@/components/common/layout/EmptyState';
import {CardRowsSkeleton, TableSkeleton} from '@/components/common/layout/LoadSkeleton';
import {ConfirmDialog} from '@/components/common/layout/ConfirmDialog';
import {AddAccountDialog} from '@/components/common/accounts/AddAccountDialog';
import {
  ExportAccountsButton,
  ImportAccountsButton,
  ImportAccountsDialog,
} from '@/components/common/accounts/TransferAccountsDialog';
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
  const [addOpen, setAddOpen] = useState(false);
  const [importOpen, setImportOpen] = useState(false);
  const [busyUid, setBusyUid] = useState<string | null>(null);

  /* ── 列排序（浏览器本地偏好持久化）─────────────────────
   * key 对应可排序列：nickname / uid / status / credits / requests。
   * dir: 'asc' | 'desc'。默认 credits desc（积分多→少）。
   * 首帧与 SSR 对齐（默认值），水合后再回填 localStorage，避免 hydration mismatch
   * 与隐私模式 SecurityError（expiryDailyMerge / i18n locale 同款约定）。 */
  type SortKey = 'nickname' | 'uid' | 'status' | 'credits' | 'requests';
  type SortDir = 'asc' | 'desc';
  // 换列时的默认方向：数值/档位列默认降序（多→少、健康→故障），文本列默认升序（A→Z）。
  // 点击与持久化恢复共用这一份口径，避免两处各写各的产生漂移。
  const defaultDirFor = (key: SortKey): SortDir =>
    key === 'credits' || key === 'requests' || key === 'status' ? 'desc' : 'asc';
  const [sortKey, setSortKey] = useState<SortKey>('credits');
  const [sortDir, setSortDir] = useState<SortDir>('desc');
  useEffect(() => {
    try {
      const raw = window.localStorage.getItem('accountsSort');
      if (raw === 'nickname' || raw === 'uid' || raw === 'status' || raw === 'credits' || raw === 'requests') {
        setSortKey(raw);
        // dir 键缺失/损坏时回落到该列的点击默认方向，与 toggleSort 同口径
        setSortDir(defaultDirFor(raw));
      }
      const dir = window.localStorage.getItem('accountsSortDir');
      if (dir === 'asc' || dir === 'desc') setSortDir(dir);
    } catch {/* 存储不可用：保持默认 */}
    // defaultDirFor 是模块级纯函数（无外部依赖），不进依赖数组
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  // 在事件处理器里同步算好下一个状态再 setState：updater 必须是纯函数，
  // 把副作用（另一个 setState、localStorage 写入）塞进 updater 会在 StrictMode
  // 双调用下把方向翻转执行两次（相互抵消，表现为点击无效果）。
  const toggleSort = useCallback(
    (key: SortKey) => {
      let dir: SortDir;
      if (sortKey !== key) {
        dir = defaultDirFor(key);
        setSortKey(key);
      } else {
        // 同列：翻转方向
        dir = sortDir === 'asc' ? 'desc' : 'asc';
      }
      setSortDir(dir);
      try {
        window.localStorage.setItem('accountsSort', key);
        window.localStorage.setItem('accountsSortDir', dir);
      } catch {/* 忽略 */}
    },
    // defaultDirFor 是模块级纯函数（无外部依赖），不进依赖数组
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [sortKey, sortDir],
  );
  const [checkinAllBusy, setCheckinAllBusy] = useState(false);
  const [balanceAllBusy, setBalanceAllBusy] = useState(false);

  // overview 与 packages 分两个缓存条目：切页先出缓存值（积分列即刻可看），
  // 后台刷新静默替换；packages 逐号查上游慢（1-2 秒），缓存命中后不再裸等。
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
  const accounts = useMemo(
    () => overviewCache.data?.accounts ?? [],
    [overviewCache.data],
  );
  const loading = overviewCache.loading;
  const load = overviewCache.refresh;

  // 实时积分与包明细从 packages 缓存派生；packages 拉取失败时静默降级：
  // 仍显示池快照的 credits（原始语义），包明细退化为空（倒计时自然消失）。
  const packages = packagesCache.data;
  const {liveCredits, liveUsed, packs} = useMemo(() => {
    const credits: Record<string, number> = {};
    const used: Record<string, number> = {};
    const packMap: Record<string, CreditPackage[]> = {};
    if (packages) {
      for (const row of packages.accounts) {
        packMap[row.uid] = row.packages ?? [];
        if (typeof row.remain === 'number' && !row.error) credits[row.uid] = row.remain;
        if (typeof row.used === 'number' && !row.error) used[row.uid] = row.used;
      }
    }
    return {liveCredits: credits, liveUsed: used, packs: packMap};
  }, [packages]);

  // 包明细镜像到 state：CreditCountdown 只在 packages 变化时需要新值，
  // 直接用派生对象即可，不再单独 setState（避免每次渲染新引用触发子树重渲染）。
  const creditPacks = packs;

  // 池状态（冷却 / 成功计数等）会随时间变化，页面停留时定时刷新。
  // 心跳同时刷新两个缓存条目，accounts 与实时余额仍然同帧续命。
  // 周期心跳失败一律静默（与 logs 页同设计）：后台 30s 一轮的失败不该弹
  // 错误雨，等下一轮自愈；错误提示只留给用户手动触发的操作（下方各按钮）。
  useHeartbeat(
    () => {
      overviewCache.refresh().catch(() => {/* 静默，等下一轮心跳 */});
      packagesCache.refresh().catch(() => {/* packages 失败静默降级，不弹错 */});
    },
    30000,
  );

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

  /** 按当前版本过滤（Go 单实例双版本共存；存量无 realm 视为 cn），再按列排序 */
  const visible = useMemo(() => {
    const list = accounts.filter((a) => (a.realm ?? 'cn') === realm);
    // 可用性分档权重：数值越小越靠前。asc = 健康→故障；desc 反转。
    const tierWeight = (a: Account): number => {
      switch (availabilityOf(a)) {
        case 'online': return 0;
        case 'neverSucceeded': return 1;
        case 'cooling': return 2;
        case 'unknown': return 3;
        case 'manualDisabled': return 4;
        case 'disabled': return 5;
      }
    };
    const mul = sortDir === 'asc' ? 1 : -1;
    return [...list].sort((a, b) => {
      switch (sortKey) {
        case 'nickname': {
          const na = (a.nickname || '').trim();
          const nb = (b.nickname || '').trim();
          // 昵称按 locale 排；都为空时回落 uid，保持稳定
          if (na && nb) return mul * na.localeCompare(nb);
          if (na !== nb) return mul * (nb ? 1 : -1); // 空昵称排后面（asc）
          return a.uid.localeCompare(b.uid);
        }
        case 'uid':
          return mul * a.uid.localeCompare(b.uid);
        case 'status': {
          const d = tierWeight(a) - tierWeight(b);
          if (d !== 0) return mul * d;
          return a.uid.localeCompare(b.uid);
        }
        case 'requests': {
          const sa = a.success_count ?? 0;
          const sb = b.success_count ?? 0;
          if (sa !== sb) return mul * (sa - sb);
          const ea = a.err_total ?? 0;
          const eb = b.err_total ?? 0;
          if (ea !== eb) return mul * (ea - eb);
          return a.uid.localeCompare(b.uid);
        }
        case 'credits':
        default: {
          // 实时余额优先（查过余额的号用它），缺数据的排最后
          const va = liveCredits[a.uid] ?? a.credits;
          const vb = liveCredits[b.uid] ?? b.credits;
          const na = typeof va === 'number';
          const nb = typeof vb === 'number';
          if (na && nb) {
            if (va !== vb) return mul * ((va as number) - (vb as number));
          } else if (na !== nb) {
            return nb ? 1 : -1; // 无数据恒排末尾，与方向无关
          }
          return a.uid.localeCompare(b.uid);
        }
      }
    });
  }, [accounts, realm, sortKey, sortDir, liveCredits]);

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
      (ok ? notify.ok : notify.err)(res.message || res.checkin_message || okMsg);
      await load();
      window.dispatchEvent(new Event('workbuddy-manager:accounts-changed'));
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setBusyUid(null);
    }
  }

  /** 排序表头（桌面表格）：可点列头切换排序，当前列显示方向箭头 + aria-sort。
   *  手机端卡片不提供排序入口（账号数少、触屏排序收益低），仅桌面生效。 */
  function renderSortableHead(key: SortKey, label: string, extraClass?: string) {
    const active = sortKey === key;
    let Icon = ArrowUpDown;
    if (active) Icon = sortDir === 'asc' ? ArrowUp : ArrowDown;
    // 屏幕阅读器需要 aria-sort 才能感知当前排序列与方向（图标纯视觉）
    let ariaSort: 'ascending' | 'descending' | 'none' = 'none';
    if (active) ariaSort = sortDir === 'asc' ? 'ascending' : 'descending';
    return (
      <TableHead className={extraClass} aria-sort={ariaSort}>
        <button
          type="button"
          onClick={() => toggleSort(key)}
          title={t('accounts.sortToggle')}
          className={
            'inline-flex items-center gap-1 rounded-md px-1 py-0.5 text-[11px] transition-colors ' +
            (active
              ? 'font-medium text-foreground'
              : 'text-muted-foreground hover:text-foreground')
          }
        >
          {label}
          <Icon className={'h-3 w-3 ' + (active ? '' : 'opacity-50')} />
        </button>
      </TableHead>
    );
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

  /** 积分余额 + 已用 + 到期倒计时 */
  function renderCredits(a: Account) {
    const value = liveCredits[a.uid] ?? a.credits;
    const hasValue = typeof value === 'number';
    const used = liveUsed[a.uid];
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
        {/* 已用积分（packages 查到才显示）：与余额并列成「余额 / 已用」 */}
        {typeof used === 'number' && (
          <span
            className="text-xs tabular-nums text-muted-foreground"
            title={t('accounts.creditsUsedTitle')}
          >
            <span className="mx-0.5 text-muted-foreground/40">/</span>
            {t('metric.usedShort')} {fmtNumber(used)}
          </span>
        )}
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
        {/* 强制清除冷却/限流：冷却中（含 6004 模型级限流）不等自然到期立即回池。
            与复活分工：这个只清计时器，禁用/停用走右边两位。 */}
        {!off && (
          <Button variant="ghost" size="icon" className="h-7 w-7 rounded-md" title={t('accounts.clearCooldownTitle')} disabled={busy}
            onClick={() => run(a.uid, () => accountApi.clearCooldown(a.uid), t('accounts.clearCooldownDone'))}>
            <ShieldOff className="h-3.5 w-3.5" />
          </Button>
        )}
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
              <>
                <ExportAccountsButton disabled={!visible.length} />
                <ImportAccountsButton onOpen={() => setImportOpen(true)} />
                <Button size="sm" className="rounded-full" onClick={() => setAddOpen(true)}>
                  <Plus />
                  {t('accounts.addAccount')}
                </Button>
              </>
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
          {loading && !accounts.length && <CardRowsSkeleton rows={4} />}
        </div>

        {/* 桌面端：表格 */}
        <div className="hidden md:block">
        <Table>
          <TableHeader>
            <TableRow className="border-b border-border/60 hover:bg-transparent">
              {renderSortableHead('nickname', t('accounts.colNickname'), 'pl-4')}
              {renderSortableHead('uid', 'UID')}
              {renderSortableHead('status', t('accounts.colStatus'))}
              {renderSortableHead('credits', t('metric.credits'))}
              {renderSortableHead('requests', t('accounts.colRequests'))}
              {isAdmin && <TableHead className="pr-4 text-[11px] text-muted-foreground">{t('accounts.colActions')}</TableHead>}
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
        {loading && !accounts.length && <TableSkeleton rows={6} />}
      </section>

      <AddAccountDialog open={addOpen} onOpenChange={setAddOpen} onSuccess={load} />
      <ImportAccountsDialog open={importOpen} onOpenChange={setImportOpen} onSuccess={load} />
    </div>
  );
}
