'use client';

import {toast} from 'sonner';

/**
 * 统一的提示封装：气泡弹跳动画 + 语义化图标，让提醒更醒目。
 * 动画在 globals.css 中通过 `[data-sonner-toast]` 定义，所有提示自动生效。
 */
type Variant = 'success' | 'error' | 'warning' | 'info';

const ICON: Record<Variant, string> = {
  success: '🎉',
  error: '⛔',
  warning: '⚠️',
  info: '💡',
};

function show(variant: Variant, message: string, description?: string, duration = 3400) {
  const fn =
    variant === 'success' ? toast.success
    : variant === 'error' ? toast.error
    : variant === 'warning' ? toast.warning
    : toast.info;
  return fn(message, {
    description,
    duration,
    icon: ICON[variant],
  });
}

export const notify = {
  /** 操作成功，例如保存完成、授权成功 */
  ok: (message: string, description?: string) => show('success', message, description),
  /** 失败，需要用户处理 */
  err: (message: string, description?: string) => show('error', message, description, 5000),
  /** 需要注意但不阻断，例如即将过期、配额将满 */
  warn: (message: string, description?: string) => show('warning', message, description, 5000),
  /** 中性提示 */
  info: (message: string, description?: string) => show('info', message, description),
};

export {toast};
