'use client';

import {useT} from '@/lib/i18n/provider';
import {fmtDateTime, fmtNumber} from '@/lib/format';
import {useNow} from '@/lib/use-now';
import type {CreditPackage} from '@/lib/types';

/**
 * 积分到期倒计时。
 *
 * 数据源是 /api/packages 的积分包明细（panel upstream.CreditPackage）：
 * end_time 为该包周期结束时间（上游 ExpiredTime / PackageEndTime 二者取有值者）。
 * 只显示**最近一个仍有余量**的到期套餐倒计时，全部套餐明细放在悬停提示里。
 *
 * 没有到期信息（套餐永不过期 / 尚未查到）时不渲染任何东西——包括不订阅时钟。
 */
export function CreditCountdown({packages}: {packages?: CreditPackage[] | null}) {
  const rows = (packages ?? [])
    .filter((p) => !!p.end_time)
    .map((p) => ({at: Date.parse(p.end_time as string), amount: p.remain}))
    .filter((p) => Number.isFinite(p.at))
    .sort((a, b) => a.at - b.at);
  // 徽章只认「还有积分可花」的最近一笔：已耗尽（remain=0）的包排在最前
  // （它们的到期时刻更近）但没有展示价值，显示成「0 · N 天后到期」纯属误导。
  const next = rows.find((r) => r.amount > 0);
  if (!next) return null;
  return <Countdown next={next} all={rows} />;
}

function Countdown({next, all}: {next: {at: number; amount: number}; all: {at: number; amount: number}[]}) {
  const t = useT();
  // 用渲染时刻粗算一次剩余时间，只用来决定刷新频率（精度要求 = 文案变化频率）：
  // 最后一分钟必须秒级；再往后分钟级足够。
  const roughLeft = next.at - Date.now();
  const now = useNow(roughLeft < 120_000 ? 1000 : 60_000);

  const left = Math.floor((next.at - now) / 1000);
  // 三档紧迫度：1 天内红 → 7 天内琥珀 → 更远常规色。
  const cls =
    left < 86400
      ? 'bg-red-500/15 text-red-600 dark:text-red-400'
      : left < 7 * 86400
        ? 'bg-amber-500/15 text-amber-600 dark:text-amber-400'
        : 'bg-muted text-muted-foreground';

  // 明细：每条「额度 · 到期时刻（本地时区）」。时刻用绝对时间，
  // 用户要拿它跟腾讯官网/客服对账，倒计时只解决紧迫感。
  // 已耗尽（remain=0）的包折叠成一行计数——几十个「0 · 时间」把真正
  // 要看的余量行顶出悬浮窗，没有对账价值；到期日在未来的已耗尽包
  // 对周期套餐来说额度会重置，但此刻确实无可花，一行带过即可。
  const usable = all.filter((e) => e.amount > 0);
  const exhausted = all.length - usable.length;
  const lines = usable.map((e) => `${fmtNumber(e.amount)} · ${fmtDateTime(e.at)}`);
  if (exhausted > 0) {
    lines.push(t('credit.exhaustedLine', {count: exhausted, n: exhausted}));
  }
  const total = usable.reduce((sum, e) => sum + e.amount, 0);

  return (
    <span
      className={`rounded-full px-1.5 py-0.5 text-[10px] leading-3 tabular-nums ${cls}`}
      title={[
        t('credit.expiryTipTitle', {total: fmtNumber(total)}),
        ...lines,
        '',
        t('credit.expiryTipNote'),
      ].join('\n')}
    >
      {fmtNumber(next.amount)} · {countdownText(t, left)}
    </span>
  );
}

/**
 * 倒计时文案。**向下取整**：倒计时说「还有 5.8 小时」不如「还有 5 小时」干脆。
 * 文案自带「后过期」：单元格里只有一个数字的话看不出在数什么。
 */
function countdownText(t: ReturnType<typeof useT>, left: number): string {
  if (left <= 0) return t('credit.expired');
  if (left < 60) return t('credit.expiring');
  if (left < 3600) {
    const n = Math.floor(left / 60);
    return t('credit.expiresInMinutes', {count: n, n});
  }
  if (left < 86400) {
    const n = Math.floor(left / 3600);
    return t('credit.expiresInHours', {count: n, n});
  }
  const n = Math.floor(left / 86400);
  return t('credit.expiresInDays', {count: n, n});
}
