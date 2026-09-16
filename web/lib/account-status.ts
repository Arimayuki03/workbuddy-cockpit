/**
 * 账号可用性的**单一判定来源**（账号页与首页共用）。
 *
 * 为什么要有这个模块：同一个账号，首页说「在线」、账号列表说「未加载」——
 * 用户看到的是一张自相矛盾的面板。根因是两页各算各的：
 *
 *   · 首页的健康快照直接用了 `expiryVisual()` 的 `label`，而那个分档描述的是
 *     **Token 有效期**（绿=还有很久），并不是「账号可用」。于是只要 token 没过期
 *     就显示「在线」，账号在不在上游池里、有没有被禁用冷却，一概没看。
 *   · 账号列表走的是另一套：合并上游池状态后按 禁用→过期→状态未知→冷却→
 *     一直失败→未加载→在线 的顺序判定。
 *
 * 同一个事实被两处独立推导，迟早会不一致（这次就是）。所以把「合并上游状态」
 * 与「判定可用性分档」都收在这里，两页只负责**各自怎么画**，不再各自判断。
 *
 * 注意分档顺序**不可随意调整**，它编码了优先级：
 *   disabled → expired → unknown → cooling → neverSucceeded → notLoaded → online
 * 例如「上游状态取不到」必须早于「不在池里」——否则会把「看不到上游」
 * 误报成「账号文件坏了」（这个误报曾经真实发生过，见下）。
 */
import type {Account, UpstreamStatus} from './types';

/** 账号当前的可用性分档 */
export type AvailabilityTier =
  | 'disabled'
  | 'expired'
  /** 本次读不到上游状态，运行时字段全部未知（`upstream.connected !== true`） */
  | 'unknown'
  | 'cooling'
  /** 有累计错误且从未成功过——上游对这类 4xx「只换号不罚」，状态看着正常却一直失败 */
  | 'neverSucceeded'
  /** 上游明确没加载它（不在池里，永远选不中） */
  | 'notLoaded'
  | 'online';

/**
 * 把上游 `/status` 的账号池状态合并进本地账号列表。
 *
 * `poolKnown` 这层判断是必须的：上游连接失败时 `/status` 仍返回 200，只是
 * `connected:false` 且没有账号列表。若不看这个标记，池就是空的，于是**每个账号
 * 都被判成「不在池里」**——界面把「连不上上游」误报成「账号文件坏了」，
 * 而账号页有 30 秒心跳，任何一次抖动都会命中、30 秒后又自己恢复，用户会以为
 * 账号随机坏掉。故此时如实标成「未知」，而不是「未加载」。
 */
export function mergePoolStatus(
  accounts: Account[],
  upstream: UpstreamStatus | null,
): Account[] {
  const pool = new Map<string, Record<string, unknown>>();
  for (const item of upstream?.accounts ?? []) {
    const uid = String(
      (item as Record<string, unknown>).uid ?? (item as Record<string, unknown>).UID ?? '',
    );
    if (uid) pool.set(uid, item as Record<string, unknown>);
  }
  const poolKnown = upstream?.connected === true;
  return accounts.map((a) => {
    // 上游状态取不到 → 不判断它在不在池里，如实标成「未知」
    if (!poolKnown) return {...a, in_pool: undefined, poolUnknown: true};
    const p = pool.get(a.uid);
    // in_pool 由**本次这份上游快照**判定，而不是沿用后端那个标记：
    // 本页其余状态字段都取自这一份数据，若单独用另一时刻的标记，两者可能
    // 互相矛盾（它们是两次 /status 调用的结果）。没进池的账号保留本地字段
    // （含 invalid_reason），交给徽章如实展示。
    if (!p) return {...a, in_pool: false, poolUnknown: false};
    return {
      ...a,
      in_pool: true,
      poolUnknown: false,
      healthy: typeof p.healthy === 'boolean' ? p.healthy : null,
      disabled: typeof p.disabled === 'boolean' ? p.disabled : null,
      disabled_reason: typeof p.disabled_reason === 'string' ? p.disabled_reason : '',
      in_flight: typeof p.in_flight === 'number' ? p.in_flight : null,
      cooling: typeof p.cooling === 'boolean' ? p.cooling : null,
      last_used: typeof p.last_used === 'number' ? p.last_used : null,
    } satisfies Account;
  });
}

/** 该账号的可用性分档（顺序即优先级，见模块注释）。 */
export function availabilityOf(a: Account): AvailabilityTier {
  if (a.disabled === true) return 'disabled';
  if (a.is_expired) return 'expired';
  // 「看不到上游」必须早于「不在池里」：否则会把正常账号说成文件损坏
  if (a.poolUnknown) return 'unknown';
  if (a.cooling) return 'cooling';
  // 判据用 success_count 而不是 last_success：后者是 Go 的 time.Time 配
  // omitempty，而 omitempty 对结构体类型**不生效**——从未成功过的账号会序列化成
  // "0001-01-01T00:00:00Z"，在 JS 里是**真值**，判空永远不命中（实测确认）。
  // success_count 是 int64，omitempty 生效，0 时整个键都不出现。
  const errs = typeof a.err_total === 'number' ? a.err_total : 0;
  const oks = typeof a.success_count === 'number' ? a.success_count : 0;
  if (errs > 0 && oks === 0) return 'neverSucceeded';
  if (a.in_pool === false) return 'notLoaded';
  return 'online';
}

/**
 * 分档对应的文案键。**两页共用同一套文案**——这正是为了让两处说法逐字一致；
 * 各写各的字符串，迟早又会出现「一个叫在线、一个叫正常」这种漂移。
 */
export function availabilityLabelKey(tier: AvailabilityTier, a?: Account): string {
  switch (tier) {
    case 'disabled':
      // 上游对 11140（request illegal）是硬禁用、到期也不自愈，必须重新登录；
      // 只说「已禁用」会让人干等。
      return /11140|request illegal/i.test(String(a?.disabled_reason || ''))
        ? 'accounts.badgeDisabledRelogin'
        : 'accounts.badgeDisabled';
    case 'expired':
      return 'accounts.badgeExpired';
    case 'unknown':
      return 'accounts.badgeUnknown';
    case 'cooling':
      return 'accounts.badgeCooling';
    case 'neverSucceeded':
      return 'accounts.badgeNeverSucceeded';
    case 'notLoaded':
      return 'accounts.badgeNotLoaded';
    case 'online':
      return 'accounts.badgeOnline';
  }
}

/** 需要解释「为什么是这个状态」时对应的提示键；没有可解释的返回 null。 */
export function availabilityTitleKey(tier: AvailabilityTier): string | null {
  switch (tier) {
    case 'unknown':
      return 'accounts.badgeUnknownTitle';
    case 'neverSucceeded':
      return 'accounts.badgeNeverSucceededTitle';
    case 'notLoaded':
      return 'accounts.badgeNotLoadedTitle';
    default:
      return null;
  }
}

/** 分档对应的文字颜色（给不用 Badge 的地方，如首页快照卡片）。 */
export function availabilityClass(tier: AvailabilityTier): string {
  switch (tier) {
    case 'disabled':
    case 'expired':
      return 'text-red-600 dark:text-red-400';
    case 'unknown':
      return 'text-muted-foreground';
    case 'cooling':
      return 'text-amber-600 dark:text-amber-400';
    case 'neverSucceeded':
    case 'notLoaded':
      return 'text-rose-600 dark:text-rose-400';
    case 'online':
      return 'text-emerald-600 dark:text-emerald-400';
  }
}
