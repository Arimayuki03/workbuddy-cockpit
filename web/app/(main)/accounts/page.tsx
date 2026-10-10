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
  CheckCircle2,
  ChevronLeft,
  ChevronRight,
  NotebookPen,
  Search,
  ServerCrash,
  SquarePen,
  TimerReset,
  TriangleAlert,
  Waypoints,
} from 'lucide-react';
import {useHeartbeat} from '@/lib/use-heartbeat';
import {notify} from '@/lib/toast';
import {accountApi, errText, httpStatus} from '@/lib/api';
import {useCachedAsync} from '@/lib/data-cache';
import type {Account, CreditPackage, OverviewResponse, PackagesResponse, ProxyRoutesResponse} from '@/lib/types';
import {fmtAgo, fmtNumber} from '@/lib/format';
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
import {Input} from '@/components/ui/input';
import {Popover, PopoverContent, PopoverTrigger} from '@/components/ui/popover';
import {Textarea} from '@/components/ui/textarea';
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
import {useDebounce} from '@/hooks/use-debounce';

export default function AccountsPage() {
  const {realm} = useRealm();
  const t = useT();
  const {isAdmin} = useAuth();
  const [addOpen, setAddOpen] = useState(false);
  const [importOpen, setImportOpen] = useState(false);
  const [busyUid, setBusyUid] = useState<string | null>(null);

  /* ── 账号备注（后端化）──────────────────────────────────
   * 数据源切换：Account.note（池状态，state.json 落盘，换浏览器不丢）为主，
   * localStorage（key accountsNotes，按 uid 的历史版本数据）为兜底——后端
   * note 为空且本地有旧值时展示旧值。编辑保存调 POST /api/accounts/{uid}/note，
   * 成功后本地同步。首载做一次性迁移：对「后端为空且本地有值」的账号批量推送
   * 后端（逐个 POST，失败静默），全部推送成功（或已推过）后清掉 localStorage
   * key，避免反复推。与列排序同约定：首帧默认空、挂载后回填，避免 hydration
   * mismatch。 */
  const [localNotes, setLocalNotes] = useState<Record<string, string>>({});
  /** 首载失败信息：fetcher 里捕获，UI 显示错误态而非「空列表」 */
  const [loadError, setLoadError] = useState<string | null>(null);
  useEffect(() => {
    try {
      const raw = window.localStorage.getItem('accountsNotes');
      if (raw) setLocalNotes(JSON.parse(raw) as Record<string, string>);
    } catch {/* 存储不可用/损坏：保持空 */}
  }, []);
  /** 备注展示值：后端 note 优先，为空时回落 localStorage 旧值（迁移前窗口期） */
  const notesOf = useCallback(
    (a: Account) => a.note || localNotes[a.uid] || '',
    [localNotes],
  );
  /**
   * 保存备注：调后端（成功后同步本地兜底缓存——后端已有值，兜底不再生效；
   * 失败时落 localStorage 保底，避免用户写的备注因网络问题直接丢失）。
   * 后端为权威数据源：成功后由 overview 轮询带回最新值。
   */
  const syncLocalNote = useCallback((uid: string, text: string) => {
    setLocalNotes((prev) => {
      const next = {...prev};
      if (text.trim()) next[uid] = text;
      else delete next[uid];
      try {
        window.localStorage.setItem('accountsNotes', JSON.stringify(next));
      } catch {/* 忽略 */}
      return next;
    });
  }, []);
  const setNote = useCallback(
    async (uid: string, text: string) => {
      try {
        await accountApi.setNote(uid, text);
        syncLocalNote(uid, text); // 后端已落，本地兜底同步（清空即删，不积累空串）
        notify.ok(t('accounts.noteSaved'));
      } catch {
        // 后端失败：本地兜底保存（下次打开页面/迁移重试仍可找回）
        syncLocalNote(uid, text);
        notify.err(t('accounts.noteSaveFailed'));
      }
    },
    [t, syncLocalNote],
  );
  /** 一次性迁移是否已跑过（跑过即不再推，防失败循环反复打端点）。
   *  迁移 effect 本体在 overviewCache 声明之后（依赖其 loading/data）。 */

  /* ── 出口代理线路（workbuddy-manager proxy_routes 吸收件）──────────────
   * 线路表经 GET /api/proxy_routes 拉取（密码已脱敏，仅展示名字）；绑定写
   * POST /api/accounts/{uid}/proxy_route（落 auth 文件 + 内存即时生效，无需重启）。
   * 未配置任何线路时按钮隐藏（零配置零噪声）。空 selection = 直连（解绑）。 */
  const proxyRoutesCache = useCachedAsync<ProxyRoutesResponse>(
    'proxy_routes',
    () => accountApi.proxyRoutes(),
    {ttl: 30000},
  );
  const proxyRouteNames = useMemo(
    () => Object.keys(proxyRoutesCache.data?.routes ?? {}).sort(),
    [proxyRoutesCache.data],
  );
  const routeOf = useCallback(
    (uid: string) => proxyRoutesCache.data?.accounts?.[uid] ?? '',
    [proxyRoutesCache.data],
  );
  const setRoute = useCallback(
    async (uid: string, route: string) => {
      try {
        await accountApi.setProxyRoute(uid, route);
        notify.ok(route ? t('accounts.proxyRouteSaved', {name: route}) : t('accounts.proxyRouteCleared'));
        await proxyRoutesCache.refresh();
      } catch (e) {
        notify.err(errText(e));
      }
    },
    [t, proxyRoutesCache],
  );

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
  const [renewAllBusy, setRenewAllBusy] = useState(false);

  /* ── 搜索（本地过滤，防抖 300ms）──────────────────────────
   * 覆盖 uid / 昵称 / 备注；纯前端过滤，不改 API 调用。 */
  const [query, setQuery] = useState('');
  const debouncedQuery = useDebounce(query, 300);

  /* ── 客户端分页（本地偏好持久化，与列排序同约定）────────────── */
  type PageSize = 20 | 50 | 100;
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState<PageSize>(20);
  useEffect(() => {
    try {
      const raw = Number(window.localStorage.getItem('accountsPageSize'));
      if (raw === 20 || raw === 50 || raw === 100) setPageSize(raw);
    } catch {/* 存储不可用：保持默认 */}
  }, []);
  const changePageSize = useCallback((next: PageSize) => {
    setPageSize(next);
    setPage(1); // 换每页条数后旧页码可能越界，直接回第一页
    try {
      window.localStorage.setItem('accountsPageSize', String(next));
    } catch {/* 忽略 */}
  }, []);

  // overview 与 packages 分两个缓存条目：切页先出缓存值（积分列即刻可看），
  // 后台刷新静默替换；packages 逐号查上游慢（1-2 秒），缓存命中后不再裸等。
  // 首载失败转成 loadError（区分「加载失败」与「真的没有账号」）。错误在
  // fetcher 里捕获：useCachedAsync 内部的挂载刷新会先占住请求锁并把失败
  // 静默吞掉，页面层对同一个 refresh 的 .catch 收不到 rejection——把捕获
  // 挂进 fetcher 才是唯一可靠的位置（dashboard / logs 同此口径）。
  const overviewCache = useCachedAsync<OverviewResponse>(
    'overview',
    () =>
      accountApi.overview().catch((e) => {
        setLoadError((prev) => prev ?? errText(e)); // 已有错误不覆盖
        throw e; // 继续抛给 hook 的常规错误路径（静默/调用方处理）
      }),
    {ttl: 5000},
  );
  /** 手动重试：清错误重新拉取，失败则再次落错误态 */
  const clearLoadError = useCallback(() => {
    setLoadError(null);
    overviewCache.refresh().catch((e) => {
      setLoadError(errText(e));
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
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
  // 错误雨，等下一轮自愈；错误条保留到手动重试成功，避免反复闪烁。
  useHeartbeat(
    () => {
      // 心跳失败静默（等下一轮），错误态的清除只发生在手动重试成功后
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

  /* ── 签到按钮「今日已签」态（会话内）──────────────────────
   * 上游对重复签到返回幂等拒绝（msg 含「已签到」/already），签到端点把它放进
   * checkin_message 且 ok 恒 true。后端没有签到日期字段（刻意不加），因此该态
   * 仅在本会话内保留：点签到遇「已签到」回复 → 按钮打勾禁用；刷新页面回到
   * 未知态（可再点，上游幂等拒绝无害）。 */
  const [checkedIn, setCheckedIn] = useState<Record<string, boolean>>({});

  /** 单号签到：区分「成功签到」「今日已签」两种成功形态，后者置会话内已签态 */
  const runCheckin = useCallback(
    async (uid: string) => {
      setBusyUid(uid);
      try {
        const res = await accountApi.checkin(uid);
        // 已签判定：上游幂等拒绝的 checkin_message（「今天已签到」/"already ..."）。
        // balance_error 不在此列（那是余额查询失败，签到本身可能成功）。
        const msg = res.checkin_message ?? '';
        if (msg && (msg.includes('已签到') || /already/i.test(msg))) {
          setCheckedIn((prev) => ({...prev, [uid]: true}));
          notify.info(t('accounts.checkinAlready'), msg);
        } else {
          if (msg) notify.err(msg);
          else notify.ok(t('accounts.checkinDone'));
          await load();
          window.dispatchEvent(new Event('workbuddy-manager:accounts-changed'));
        }
      } catch (e) {
        notify.err(errText(e));
      } finally {
        setBusyUid(null);
      }
    },
    [load, t],
  );

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

  /** 续期巡检：手动触发全账号续期（异步执行，仅临期账号）。
   *  409 = 上一轮还在跑（提示进行中）；501 = 巡检功能未开启（提示而非报错）。 */
  const renewAll = useCallback(async () => {
    setRenewAllBusy(true);
    try {
      await accountApi.renewAll();
      notify.ok(t('accounts.renewAllStarted'), t('accounts.renewAllStartedDetail'));
      await load();
    } catch (e) {
      const status = httpStatus(e);
      if (status === 409) notify.info(t('accounts.renewAllBusy'), t('accounts.renewAllBusyDetail'));
      else if (status === 501) notify.info(t('accounts.renewAllUnavailable'), t('accounts.renewAllUnavailableDetail'));
      else notify.err(errText(e));
    } finally {
      setRenewAllBusy(false);
    }
  }, [load, t]);

  /* ── 备注一次性迁移（localStorage → 后端）────────────────
   * 触发条件：overview 首载完成（accounts 才有 note 可比对）。对「后端 note
   * 为空且本地有值」的账号逐个推送后端（失败静默，本地值保留下次重试）；
   * 全部成功或无可迁数据 → 清掉 localStorage key，避免反复推。 */
  const [noteMigrated, setNoteMigrated] = useState(false);
  useEffect(() => {
    if (noteMigrated) return;
    // 首载失败时 accounts 是空数组，误判「无旧数据」会直接清掉 localStorage，
    // 本地备注不可逆丢失——必须等成功加载后再判（loadError 由页面 fetcher 写入）。
    if (loading || loadError) return;
    setNoteMigrated(true); // 无论结果如何本轮只跑一次
    const pending = Object.entries(localNotes).filter(([uid, text]) => {
      if (!text.trim()) return false;
      const acct = accounts.find((a) => a.uid === uid);
      // 账号已不在池里（被删）：没有推送目标，跳过
      return acct != null && !acct.note;
    });
    if (!pending.length) {
      // 没有可迁的旧数据：直接清 key（含「全部已迁完」的后续打开）
      try {
        window.localStorage.removeItem('accountsNotes');
      } catch {/* 忽略 */}
      setLocalNotes({});
      return;
    }
    // 逐个推送（失败静默——本地兜底值仍在，下次重试）
    let okCount = 0;
    Promise.all(
      pending.map(([uid, text]) =>
        accountApi
          .setNote(uid, text)
          .then(() => {
            okCount += 1;
          })
          .catch(() => {/* 静默：本地值保留 */}),
      ),
    ).then(() => {
      // 全部成功才算迁完清 key；有失败的保留旧值，下次打开再试
      if (okCount === pending.length) {
        try {
          window.localStorage.removeItem('accountsNotes');
        } catch {/* 忽略 */}
        setLocalNotes({});
      }
    });
  }, [noteMigrated, loading, loadError, localNotes, accounts]);

  /** 按当前版本过滤（Go 单实例双版本共存；存量无 realm 视为 cn），
   *  再按关键词过滤（uid/昵称/备注），最后按列排序。
   *  注意过滤后的列表同时供分页、表格与手机卡片使用，单一数据源避免两处分叉。 */
  const visible = useMemo(() => {
    const list = accounts.filter((a) => (a.realm ?? 'cn') === realm);
    // 搜索：uid / 昵称 / 备注（后端 note 为主，localStorage 旧值兜底；小写不区分大小写）
    const kw = debouncedQuery.trim().toLowerCase();
    const filtered = kw
      ? list.filter((a) => {
          const haystack =
            `${a.uid} ${a.nickname ?? ''} ${a.note ?? ''} ${localNotes[a.uid] ?? ''}`.toLowerCase();
          return haystack.includes(kw);
        })
      : list;
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
    return [...filtered].sort((a, b) => {
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
  }, [accounts, realm, debouncedQuery, localNotes, sortKey, sortDir, liveCredits]);

  /* ── 分页派生：过滤+排序后的全量 → 当前页切片 ──────────────── */
  const totalVisible = visible.length;
  const totalPages = Math.max(1, Math.ceil(totalVisible / pageSize));
  // 数据缩水（搜索/删号/切版本）时页码自动收回有效范围
  const safePage = Math.min(page, totalPages);
  const pageItems = useMemo(
    () => visible.slice((safePage - 1) * pageSize, safePage * pageSize),
    [visible, safePage, pageSize],
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

  /** 最近续期徽章：相对时间（「3 小时前」）或「从未续期」；
   *  renew_last_error 非空时转红色并在 tooltip 里显示失败原因。 */
  function renderRenewed(a: Account) {
    // Go 零值 time.Time 序列化成 "0001-01-01T00:00:00Z" = 从未续期
    const raw = a.last_renewed ?? '';
    const never = !raw || raw.startsWith('0001-01-01');
    const err = String(a.renew_last_error || '');
    if (never && !err) {
      return (
        <span className="text-xs text-muted-foreground/50" title={t('accounts.lastRenewedNeverTitle')}>
          {t('accounts.lastRenewedNever')}
        </span>
      );
    }
    const ago = never ? t('accounts.renewFailedNoTime') : fmtAgo(raw);
    return (
      <span
        className={
          'inline-flex items-center gap-1 text-xs tabular-nums ' +
          (err
            ? 'text-red-600 dark:text-red-400'
            : 'text-muted-foreground')
        }
        title={err ? t('accounts.renewErrorTitle', {err}) : t('accounts.lastRenewedTitle', {ago})}
      >
        {err && <TriangleAlert className="h-3 w-3 shrink-0" />}
        {ago}
      </span>
    );
  }

  /** 备注编辑入口：Popover 内 textarea，保存调后端（见 setNote 注释） */
  function renderNoteButton(a: Account) {
    return (
      <Popover>
        <PopoverTrigger asChild>
          <Button
            variant="ghost"
            size="icon"
            className={
              'h-7 w-7 rounded-md ' +
              (notesOf(a)
                ? 'text-sky-600 hover:text-sky-600 dark:text-sky-400'
                : 'text-muted-foreground hover:text-foreground')
            }
            title={t('accounts.noteTitle')}
          >
            <SquarePen className="h-3.5 w-3.5" />
          </Button>
        </PopoverTrigger>
        <PopoverContent className="w-72 rounded-2xl p-3" align="end">
          <NoteEditor uid={a.uid} initial={notesOf(a)} onSave={setNote} />
        </PopoverContent>
      </Popover>
    );
  }

  /** 出口线路选择入口（管理员）：Popover 内下拉，列出全部已配置线路 + 「不绑定」。
   *  仅在配置了至少一条线路时渲染（未配置 = 隐藏，零配置零噪声）。 */
  function renderRouteButton(a: Account) {
    if (!isAdmin || !proxyRouteNames.length) return null;
    const current = routeOf(a.uid);
    return (
      <Popover>
        <PopoverTrigger asChild>
          <Button
            variant="ghost"
            size="icon"
            className={
              'h-7 w-7 rounded-md ' +
              (current
                ? 'text-violet-600 hover:text-violet-600 dark:text-violet-400'
                : 'text-muted-foreground hover:text-foreground')
            }
            title={current ? t('accounts.proxyRouteTitle', {name: current}) : t('accounts.proxyRouteNoneTitle')}
          >
            <Waypoints className="h-3.5 w-3.5" />
          </Button>
        </PopoverTrigger>
        <PopoverContent className="w-56 rounded-2xl p-3" align="end">
          <RouteSelector uid={a.uid} current={current} routes={proxyRouteNames} onSelect={setRoute} />
        </PopoverContent>
      </Popover>
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
        {/* 签到：手动触发单号签到 + 余额查询解冻。
            「今日已签」态（会话内）：图标打勾 + 禁用，防当天重复点击白打上游。 */}
        {canCheckin && !off && (
          checkedIn[a.uid] ? (
            <Button variant="ghost" size="icon" className="h-7 w-7 rounded-md text-emerald-600 hover:text-emerald-600"
              title={t('accounts.checkinAlreadyTitle')} disabled>
              <CheckCircle2 className="h-3.5 w-3.5" />
            </Button>
          ) : (
            <Button variant="ghost" size="icon" className="h-7 w-7 rounded-md" title={t('accounts.checkin')} disabled={busy}
              onClick={() => runCheckin(a.uid)}>
              <Gift className="h-3.5 w-3.5" />
            </Button>
          )
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
            {/* 续期巡检：与批量签到同款样式；两版账号都可能有临期 token，不限 realm */}
            {isAdmin && (
              <Button
                size="sm"
                variant="outline"
                className="rounded-full"
                onClick={renewAll}
                disabled={renewAllBusy || !visible.length}
                title={t('accounts.renewAllTitle')}
              >
                <TimerReset className={renewAllBusy ? 'animate-pulse' : ''} />
                {t('accounts.renewAll')}
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

      {/* 搜索 + 分页工具条：本地过滤/分页，纯前端行为不改 API */}
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="relative w-full max-w-xs">
          <Search className="pointer-events-none absolute left-3 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
          <Input
            type="search"
            value={query}
            onChange={(e) => {
              setQuery(e.target.value);
              setPage(1); // 新关键词从第一页看起
            }}
            placeholder={t('accounts.searchPlaceholder')}
            className="h-8 pl-9 text-xs [&::-webkit-search-cancel-button]:hidden"
          />
        </div>
        <div className="flex items-center gap-2">
          <span className="text-[11px] text-muted-foreground">{t('accounts.totalCount', {n: totalVisible, count: totalVisible})}</span>
          <Select value={String(pageSize)} onValueChange={(v) => changePageSize(Number(v) as PageSize)}>
            <SelectTrigger size="sm" className="h-8 w-[108px] rounded-full text-[11px]" aria-label={t('accounts.pageSizeLabel')}>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {([20, 50, 100] as const).map((n) => (
                <SelectItem key={n} value={String(n)}>
                  {t('accounts.pageSizeItem', {n})}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <div className="flex items-center gap-0.5">
            <Button
              variant="ghost"
              size="icon"
              className="h-7 w-7 rounded-md"
              title={t('accounts.prevPage')}
              disabled={safePage <= 1}
              onClick={() => setPage(safePage - 1)}
            >
              <ChevronLeft className="h-3.5 w-3.5" />
            </Button>
            {/* min-w-14 装不下中文「第 12 / 13 页」（7 字 × 11px ≈ 77px），
                用 whitespace-nowrap 防换行 + 足量最小宽防遮挡 */}
            <span className="min-w-20 whitespace-nowrap text-center text-[11px] tabular-nums text-muted-foreground">
              {t('accounts.pageIndicator', {page: safePage, total: totalPages})}
            </span>
            <Button
              variant="ghost"
              size="icon"
              className="h-7 w-7 rounded-md"
              title={t('accounts.nextPage')}
              disabled={safePage >= totalPages}
              onClick={() => setPage(safePage + 1)}
            >
              <ChevronRight className="h-3.5 w-3.5" />
            </Button>
          </div>
        </div>
      </div>

      {/* 首载失败且无缓存可展示：错误态（与「暂无账号」空态严格区分） */}
      {loadError && !loading && !accounts.length ? (
        <section className="overflow-hidden rounded-[20px] bg-muted">
          <EmptyState
            icon={ServerCrash}
            title={t('accounts.loadErrorTitle')}
            description={loadError}
            className="flex flex-col items-center justify-center py-16 text-center"
          >
            <Button className="mt-4 rounded-full" onClick={clearLoadError}>
              <RefreshCw />
              {t('common.refresh')}
            </Button>
          </EmptyState>
        </section>
      ) : (
      <section className="overflow-hidden rounded-[20px] bg-muted">
        {/* 手机端：卡片列表。表格 6 列在窄屏需要横向滚动，改为纵向卡片 */}
        <div className="divide-y divide-border/40 md:hidden">
          {pageItems.map((a) => (
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

              {notesOf(a) && (
                <div className="flex items-start gap-1.5 rounded-lg bg-background/60 px-2.5 py-1.5">
                  <NotebookPen className="mt-0.5 h-3 w-3 shrink-0 text-muted-foreground" />
                  <span className="whitespace-pre-wrap break-all text-[11px] leading-relaxed text-muted-foreground">
                    {notesOf(a)}
                  </span>
                </div>
              )}

              <div className="flex items-center justify-between gap-3">
                <div className="flex shrink-0 items-center gap-1.5">
                  <Coins className="h-3.5 w-3.5 text-muted-foreground" />
                  {renderCredits(a)}
                </div>
                <div className="flex items-center gap-1">
                  {renderRenewed(a)}
                  {renderRouteButton(a)}
                  {renderNoteButton(a)}
                  {isAdmin && renderActions(a)}
                </div>
              </div>
            </div>
          ))}
          {!visible.length && !loading && (
            <div className="px-4 py-12 text-center text-xs text-muted-foreground">
              {debouncedQuery ? t('accounts.searchEmptyTitle') : t('accounts.tableEmpty')}
            </div>
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
              <TableHead className="text-[11px] text-muted-foreground">{t('accounts.colLastRenewed')}</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">{t('accounts.colNote')}</TableHead>
              {isAdmin && <TableHead className="pr-4 text-[11px] text-muted-foreground">{t('accounts.colActions')}</TableHead>}
            </TableRow>
          </TableHeader>
          <TableBody>
            {pageItems.map((a) => (
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
                <TableCell className="whitespace-nowrap">{renderRenewed(a)}</TableCell>
                <TableCell className="max-w-[180px]">
                  {notesOf(a) ? (
                    <span className="line-clamp-1 text-xs text-muted-foreground" title={notesOf(a)}>
                      {notesOf(a)}
                    </span>
                  ) : (
                    <span className="text-xs text-muted-foreground/40">—</span>
                  )}
                </TableCell>
                {isAdmin && <TableCell className="pr-4"><div className="flex justify-end gap-1">{renderRouteButton(a)}{renderNoteButton(a)}{renderActions(a)}</div></TableCell>}
              </TableRow>
            ))}
          </TableBody>
        </Table>
        </div>

        {!visible.length && !loading && !debouncedQuery && (
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
        {/* 搜索无命中（与「池里没有账号」区分开） */}
        {!visible.length && !loading && debouncedQuery && (
          <EmptyState
            icon={Search}
            title={t('accounts.searchEmptyTitle')}
            description={t('accounts.searchEmptyDesc', {q: debouncedQuery})}
            className="flex flex-col items-center justify-center py-16 text-center"
          />
        )}
        {loading && !accounts.length && <TableSkeleton rows={6} />}
      </section>
      )}

      <AddAccountDialog open={addOpen} onOpenChange={setAddOpen} onSuccess={load} />
      <ImportAccountsDialog open={importOpen} onOpenChange={setImportOpen} onSuccess={load} />
    </div>
  );
}

/**
 * 出口线路选择器（Popover 内）：单选下拉列出全部已配置线路 + 「不绑定（直连）」。
 * 独立组件（照 NoteEditor 模式）：选择即提交（无需保存按钮——绑定端点幂等且
 * 可反复切换），请求期间整组禁用防重复提交。
 */
function RouteSelector({
  uid,
  current,
  routes,
  onSelect,
}: {
  uid: string;
  current: string;
  routes: string[];
  onSelect: (uid: string, route: string) => Promise<void>;
}) {
  const t = useT();
  const [saving, setSaving] = useState(false);
  const [pending, setPending] = useState<string | null>(null);
  const select = async (route: string) => {
    if (route === current || saving) return;
    setSaving(true);
    setPending(route);
    try {
      await onSelect(uid, route);
    } finally {
      setSaving(false);
      setPending(null);
    }
  };
  return (
    <div className="flex flex-col gap-2">
      <div className="text-xs font-medium">{t('accounts.proxyRouteEditorTitle')}</div>
      <Select value={current} onValueChange={(v) => void select(v)} disabled={saving}>
        <SelectTrigger className="h-8 text-xs">
          <SelectValue placeholder={t('accounts.proxyRouteNone')} />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="">
            {t('accounts.proxyRouteNone')}
          </SelectItem>
          {routes.map((r) => (
            <SelectItem key={r} value={r}>
              {r}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      <div className="text-[10px] leading-relaxed text-muted-foreground">
        {t('accounts.proxyRouteHint')}
      </div>
      {saving && <div className="text-[10px] text-muted-foreground">{pending ? t('accounts.proxyRouteSaving') : ''}</div>}
    </div>
  );
}

/**
 * 备注编辑器（Popover 内）：本地草稿 + 保存/清空。
 * 独立组件：把草稿 state 隔离在弹层里，避免每敲一个字都重渲染整个账号页。
 * 保存走后端（setNote 异步）：请求期间按钮禁用，防止重复提交。
 */
function NoteEditor({
  uid,
  initial,
  onSave,
}: {
  uid: string;
  initial: string;
  onSave: (uid: string, text: string) => Promise<void>;
}) {
  const t = useT();
  const [draft, setDraft] = useState(initial);
  const [saving, setSaving] = useState(false);

  /** 保存包装：异步请求期间置 saving（异常已在 setNote 内 toast，这里只吞掉） */
  const save = async (text: string) => {
    setSaving(true);
    try {
      await onSave(uid, text);
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="flex flex-col gap-2">
      <div className="text-xs font-medium">{t('accounts.noteEditorTitle')}</div>
      <Textarea
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        placeholder={t('accounts.notePlaceholder')}
        rows={3}
        className="text-xs"
        maxLength={200}
        autoFocus
      />
      <div className="flex items-center justify-between">
        <span className="text-[10px] text-muted-foreground">{t('accounts.noteStoredServer')}</span>
        <div className="flex gap-1.5">
          {draft.trim() && (
            <Button
              variant="ghost"
              size="sm"
              className="h-7 rounded-full text-xs"
              disabled={saving}
              onClick={() => {
                setDraft('');
                void save('');
              }}
            >
              {t('common.clear')}
            </Button>
          )}
          <Button
            size="sm"
            className="h-7 rounded-full text-xs"
            disabled={saving || draft === initial}
            onClick={() => void save(draft)}
          >
            {saving ? t('accounts.noteSaving') : t('common.save')}
          </Button>
        </div>
      </div>
    </div>
  );
}
