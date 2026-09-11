/** 秒级时长格式化：已过期 / 分钟 / 小时 / 天 */
export function fmtRemain(seconds: number): string {
  if (seconds <= 0) return '已过期';
  if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟`;
  if (seconds < 86400) return `${(seconds / 3600).toFixed(1)} 小时`;
  return `${(seconds / 86400).toFixed(1)} 天`;
}

export function fmtDateTime(ts: number | null | undefined): string {
  if (!ts) return '—';
  const ms = ts > 1e12 ? ts : ts * 1000;
  const d = new Date(ms);
  return d.toLocaleString('zh-CN', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  });
}

export function fmtDate(ts: number | null | undefined): string {
  if (!ts) return '—';
  const ms = ts > 1e12 ? ts : ts * 1000;
  return new Date(ms).toLocaleDateString('zh-CN');
}

/** 千分位数字 */
export function fmtNumber(n: number | null | undefined): string {
  if (n === null || n === undefined) return '0';
  return n.toLocaleString('zh-CN');
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

/** Token 有效期的语义分档，账号列表与仪表盘共用同一套规则 */
export type ExpiryTier = 'expired' | 'urgent' | 'soon' | 'healthy';

export interface ExpiryVisual {
  tier: ExpiryTier;
  /** 文字颜色 class */
  textClass: string;
  /** 进度条颜色（CSS 色值） */
  barColor: string;
  /** 状态短标签 */
  label: string;
}

/**
 * 按剩余有效期分档：
 * - expired 已过期      → 红（destructive）
 * - urgent  < 1 小时     → 琥珀
 * - soon    < 6 小时     → 蓝
 * - healthy 其余         → 绿
 */
export function expiryVisual(remainSeconds: number): ExpiryVisual {
  if (remainSeconds <= 0) {
    return {
      tier: 'expired',
      textClass: 'text-red-600 dark:text-red-400',
      barColor: 'var(--destructive)',
      label: '已过期',
    };
  }
  if (remainSeconds < 3600) {
    return {
      tier: 'urgent',
      textClass: 'text-amber-600 dark:text-amber-400',
      barColor: '#f59e0b',
      label: '即将过期',
    };
  }
  if (remainSeconds < 6 * 3600) {
    return {
      tier: 'soon',
      textClass: 'text-blue-600 dark:text-blue-400',
      barColor: '#3b82f6',
      label: '偏紧',
    };
  }
  return {
    tier: 'healthy',
    textClass: 'text-emerald-600 dark:text-emerald-400',
    barColor: '#10b981',
    label: '在线',
  };
}

/** 相对时间：3 分钟前 */
export function fmtAgo(ts: number | null | undefined): string {
  if (!ts) return '从未';
  const ms = ts > 1e12 ? ts : ts * 1000;
  const diff = Date.now() - ms;
  if (diff < 0) return '刚刚';
  const s = Math.floor(diff / 1000);
  if (s < 60) return `${s} 秒前`;
  if (s < 3600) return `${Math.floor(s / 60)} 分钟前`;
  if (s < 86400) return `${Math.floor(s / 3600)} 小时前`;
  return `${Math.floor(s / 86400)} 天前`;
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
