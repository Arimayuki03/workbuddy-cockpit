'use client';

import {motion} from 'motion/react';
import type {ReactNode} from 'react';
import type {LucideIcon} from 'lucide-react';
import {cn} from '@/lib/utils';

/**
 * 统计卡片 —— 完全沿用 LDC StatCard 范式：
 * min-h-[88px] rounded-[20px] bg-muted，无边框、靠底色区分。
 */
export function StatCard({
  label,
  value,
  hint,
  icon: Icon,
  valueClassName,
  delay = 0,
}: {
  label: string;
  value: ReactNode;
  hint?: string;
  icon?: LucideIcon;
  valueClassName?: string;
  delay?: number;
}) {
  return (
    <motion.div
      initial={{opacity: 0, y: 12}}
      animate={{opacity: 1, y: 0}}
      transition={{duration: 0.35, delay}}
      className="min-h-[88px] sm:min-h-[96px] rounded-[20px] bg-muted px-3.5 py-3 sm:px-4"
    >
      <div className="flex items-start justify-between gap-2">
        <div className="text-[11px] font-medium text-gray-500 dark:text-gray-400 truncate">
          {label}
        </div>
        {Icon && (
          <div className="h-6 w-6 shrink-0 rounded-full bg-white/70 dark:bg-white/[0.05] text-gray-500 grid place-items-center">
            <Icon className="h-3.5 w-3.5" />
          </div>
        )}
      </div>
      <div
        className={cn(
          'mt-3 text-xl sm:text-2xl font-semibold tracking-[-0.03em] tabular-nums text-gray-900 dark:text-gray-100',
          valueClassName,
        )}
      >
        {value}
      </div>
      {hint && (
        <div className="mt-2 text-[11px] text-gray-500 dark:text-gray-400">{hint}</div>
      )}
    </motion.div>
  );
}
