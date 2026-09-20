/**
 * 本地化格式化工具。
 *
 * 这些函数是纯函数（非组件），语言取自 lib/i18n 的模块级当前语言，
 * 由 I18nProvider 在渲染期同步——调用它们的组件会随语言切换重渲染。
 * 因此本文件里不要写死任何语言：日期、数字、时长、相对时间都跟随界面语言。
 */
import {intlLocale, t} from '@/lib/i18n';

/** 秒级时长格式化：分钟 / 小时 / 天 */
export function fmtRemain(seconds: number): string {
  if (seconds <= 0) return t('format.expired');
  if (seconds < 3600) {
    const minutes = Math.floor(seconds / 60);
    return t('format.minutes', {count: minutes, n: minutes});
  }
  // count 传数值（决定英文单复数），n 传展示串（与原实现一致，保留一位小数）
  if (seconds < 86400) {
    const hrs = Math.round((seconds / 3600) * 10) / 10;
    return t('format.hours', {count: hrs, n: hrs.toFixed(1)});
  }
  const ds = Math.round((seconds / 86400) * 10) / 10;
  return t('format.days', {count: ds, n: ds.toFixed(1)});
}

/** RFC3339 / 秒 / 毫秒 时间戳 → 本地化日期时间 */
export function fmtDateTime(ts: number | string | null | undefined): string {
  if (!ts) return '—';
  if (typeof ts === 'string') {
    const at = Date.parse(ts);
    if (!Number.isFinite(at)) return '—';
    return new Date(at).toLocaleString(intlLocale(), {
      year: 'numeric',
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
    });
  }
  const ms = ts > 1e12 ? ts : ts * 1000;
  const d = new Date(ms);
  return d.toLocaleString(intlLocale(), {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  });
}

export function fmtDate(ts: number | string | null | undefined): string {
  if (!ts) return '—';
  if (typeof ts === 'string') {
    const at = Date.parse(ts);
    return Number.isFinite(at) ? new Date(at).toLocaleDateString(intlLocale()) : '—';
  }
  const ms = ts > 1e12 ? ts : ts * 1000;
  return new Date(ms).toLocaleDateString(intlLocale());
}

/**
 * 日期时间 + 相对「今天」的标记：`2026/09/20 14:03:11`。
 *
 * 为什么需要（issue #44）：任务页与日志页都有「近 24 小时」这个范围，它**必然跨天**
 * ——这时候列表里今天的 14:03 与昨天的 14:03 长得一模一样，扫一眼分不出哪条是
 * 今天的，只能挨个去数字段里的日期。用户的原话是「翻看 24 小时的时候会翻到前一天
 * 的记录，不方便观察各个账号运行状态」。
 *
 * 所以对**非今天**的行把日期顶到最前面并标注「昨天 / 更早」，今天的行保持简短。
 * 不直接写死「今天」的原因：列表里绝大多数行都是今天的，逐行标一遍只是噪音；
 * 真正需要区分的是那几条**不是今天**的。
 *
 * 跨天（今天/昨天）按**本地日历日**判，不是「距今 24 小时内」——用户看的是日历，
 * 凌晨 1 点看 23 小时前的记录会认为那是「昨天」，按日历判才与直觉一致。
 */
export function fmtDateTimeMarked(ts: number | null | undefined): string {
  if (!ts) return '—';
  const ms = ts > 1e12 ? ts : ts * 1000;
  const d = new Date(ms);
  const full = fmtDateTime(ts);
  const dayDiff = calendarDaysAgo(d);
  if (dayDiff === 0) return full;
  if (dayDiff === 1) return `${t('format.yesterday')} ${full}`;
  return full;
}

/** 目标时间距「今天」的本地日历天数（今天=0，昨天=1，未来=负数）。 */
function calendarDaysAgo(d: Date): number {
  const now = new Date();
  // 用 Date.UTC 把两个本地日期归一到 UTC 零点再相减：这样得到的是**日历天**之差，
  // 不受夏令时（某些时区一天是 23/25 小时）与具体时刻影响。
  const a = Date.UTC(d.getFullYear(), d.getMonth(), d.getDate());
  const b = Date.UTC(now.getFullYear(), now.getMonth(), now.getDate());
  return Math.round((b - a) / 86400000);
}

/** 千分位数字 */
export function fmtNumber(n: number | null | undefined): string {
  if (n === null || n === undefined) return '0';
  return n.toLocaleString(intlLocale());
}

/** 大数紧凑显示：1.2k / 3.4M */
export function fmtCompact(n: number | null | undefined): string {
  if (!n) return '0';
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(n < 10_000 ? 1 : 0)}k`;
  return `${(n / 1_000_000).toFixed(1)}M`;
}

export function fmtLatency(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(2)}s`;
}

/** 相对时间：3 分钟前（接受秒/毫秒时间戳或 RFC3339 字符串） */
export function fmtAgo(ts: number | string | null | undefined): string {
  if (!ts) return t('format.never');
  const ms = typeof ts === 'string' ? Date.parse(ts) : ts > 1e12 ? ts : ts * 1000;
  if (!Number.isFinite(ms)) return t('format.never');
  const diff = Date.now() - ms;
  if (diff < 0) return t('format.justNow');
  const s = Math.floor(diff / 1000);
  if (s < 60) return t('format.secondsAgo', {count: s, n: s});
  if (s < 3600) {
    const minutes = Math.floor(s / 60);
    return t('format.minutesAgo', {count: minutes, n: minutes});
  }
  if (s < 86400) {
    const hrs = Math.floor(s / 3600);
    return t('format.hoursAgo', {count: hrs, n: hrs});
  }
  const ds = Math.floor(s / 86400);
  return t('format.daysAgo', {count: ds, n: ds});
}

export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    try {
      const ta = document.createElement('textarea');
      ta.value = text;
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.select();
      document.execCommand('copy');
      document.body.removeChild(ta);
      return true;
    } catch {
      return false;
    }
  }
}

/**
 * 扣费金额格式化（上游 usage.credit）。
 * 单次调用常是 0.0x 量级，直接 toLocaleString 会显示成 0，因此小数值保留
 * 最多 4 位有效小数；整数则按千分位显示。
 */
export function fmtCredit(v: number | null | undefined): string {
  if (v === null || v === undefined || !Number.isFinite(v)) return '—';
  if (v === 0) return '0';
  if (Math.abs(v) >= 100) return Math.round(v).toLocaleString();
  if (Math.abs(v) >= 1) return v.toFixed(2);
  return v.toFixed(4);
}
