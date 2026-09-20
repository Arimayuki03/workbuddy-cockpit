/**
 * 账号可用性的**单一判定来源**（账号页与首页共用）。
 *
 * 数据源是 Go 网关的池状态（/api/overview 与 /status 的 accounts，同一份
 * `pool.Status`），没有 manager Python 中间层那种「本地文件 + 上游池」双源合并
 * ——所以这里的判定只剩单源分档，但优先级顺序仍然重要：
 *
 *   disabled → manualDisabled → cooling（含降权） → neverSucceeded → online
 *
 * 上游把连败降权计入 cooling（Go 侧 entry.healthy() 把 until / breakerUntil /
 * degradeUntil 取或），所以「冷却中」里混着两类原因：限流退避（等一会儿就好）
 * 与连败降权（这个号在持续失败）。isDegraded 单独判，让用户分得清该等还是该查。
 */
import type {Account, UpstreamStatus} from './types';

/** 账号当前的可用性分档 */
export type AvailabilityTier =
  | 'disabled'
  /** 上游手动停用位：还在池里、任务照常，只是不被选中转发 */
  | 'manualDisabled'
  /** 本次读不到池状态，运行时字段全部未知 */
  | 'unknown'
  | 'cooling'
  /** 有累计错误且从未成功过——状态看着正常却一直失败 */
  | 'neverSucceeded'
  | 'online';

/**
 * 账号列表直接来自池快照（/status 或 /api/overview 的 accounts）。
 *
 * 保留此函数是为了让账号页与首页继续共用同一份排序/过滤入口；
 * 数据是同一份，这里只做浅拷贝，不再有「本地文件与上游池合并」的语义。
 * `poolKnown` 判定被 upstreamStatus 是否为 null 取代。
 */
export function mergePoolStatus(
  accounts: Account[],
  upstream: UpstreamStatus | null,
): Account[] {
  // 上游状态取不到 → 每个账号都标成「未知」，而不是「不在池里」：
  // 连不上网关时把正常账号说成故障，比显示「未知」误导性大得多。
  const poolKnown = upstream !== null;
  return accounts.map((a) =>
    poolKnown ? a : {...a, pool_unknown: true},
  );
}

/** 该账号是否正被**连败降权**。判据是「截止时间在未来」而不是「字段存在」：
 *  前端拿到的可能是几十秒前的快照，字段还在、窗口已过。 */
export function isDegraded(a: Account): boolean {
  const until = a.degrade_until;
  if (typeof until !== 'string' || !until) return false;
  const at = Date.parse(until);
  return Number.isFinite(at) && at > Date.now();
}

/**
 * 该账号是否正被**模型级**限流（6004）——账号在线、但某个模型暂时用不了。
 *
 * 不能用 `cooling` 判：那是账号级状态，模型级限流不会置位它。读台账
 * （rate_limited_models），且与 isDegraded 同口径——以「截止时间是否在未来」为准。
 */
export function rateLimitedModels(a: Account): {model: string; reason: string}[] {
  const rows = a.rate_limited_models ?? [];
  const now = Date.now();
  return rows
    .filter((m) => {
      const until = m.until;
      if (typeof until !== 'string' || !until) return false;
      const at = Date.parse(until);
      return Number.isFinite(at) && at > now;
    })
    .map((m) => ({model: m.model, reason: String(m.reason ?? '')}));
}

/** 该账号的可用性分档（顺序即优先级，见模块注释）。 */
export function availabilityOf(a: Account): AvailabilityTier {
  if (a.disabled) return 'disabled';
  if (a.manual_disabled) return 'manualDisabled';
  if (a.pool_unknown) return 'unknown';
  if (a.cooling) return 'cooling';
  // 判据用 success_count 而不是 last_success：Go 的 time.Time 配 omitempty 时
  // 零值仍序列化为 "0001-01-01T00:00:00Z"，在 JS 里是真值，判空永远不命中；
  // success_count 是 int64，omitempty 生效，0 时整个键不出现。
  const errs = typeof a.err_total === 'number' ? a.err_total : 0;
  const oks = typeof a.success_count === 'number' ? a.success_count : 0;
  if (errs > 0 && oks === 0) return 'neverSucceeded';
  return 'online';
}

/**
 * 分档对应的文案键。**两页共用同一套文案**——为了让两处说法逐字一致；
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
    case 'manualDisabled':
      return 'accounts.badgeManualDisabled';
    case 'unknown':
      return 'accounts.badgeUnknown';
    case 'cooling':
      // 降权与限流退避在上游同属 cooling（口径如此），但含义差很多：前者是
      // 「这个号连续失败已达阈值」，后者是「等一会儿就好」。分开说，用户才知道
      // 该干等还是该去查这个号为什么一直失败。传了账号才分得出来。
      return a && isDegraded(a) ? 'accounts.badgeDegraded' : 'accounts.badgeCooling';
    case 'neverSucceeded':
      return 'accounts.badgeNeverSucceeded';
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
    case 'manualDisabled':
      return 'accounts.badgeManualDisabledTitle';
    default:
      return null;
  }
}

/** 分档对应的文字颜色（给不用 Badge 的地方，如首页快照卡片）。 */
export function availabilityClass(tier: AvailabilityTier): string {
  switch (tier) {
    case 'disabled':
      return 'text-red-600 dark:text-red-400';
    // 主动停用是**用户自己的选择**，用中性灰而不是告警红——
    // 它不是故障，标红会让人以为出了问题。
    case 'manualDisabled':
      return 'text-muted-foreground';
    case 'unknown':
      return 'text-muted-foreground';
    case 'cooling':
      return 'text-amber-600 dark:text-amber-400';
    case 'neverSucceeded':
      return 'text-rose-600 dark:text-rose-400';
    case 'online':
      return 'text-emerald-600 dark:text-emerald-400';
  }
}
