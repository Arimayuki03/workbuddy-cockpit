'use client';

import {Skeleton} from '@/components/ui/skeleton';
import {cn} from '@/lib/utils';

/**
 * 加载态占位（骨架行）。

 * 各页面「数据未到」阶段若只渲染一行文字（py-16 + Loading），数据到达后
 * 整块被表格/卡片替换，高度差导致页面跳动。骨架行让加载态与数据态的
 * 高度、宽度节奏接近，替换时不再抖。行数/形态由调用处按数据态形状选。
 */

/** 表格骨架：表头一条 + 若干数据行（行内按列数拆短条） */
export function TableSkeleton({rows = 5, className}: {rows?: number; className?: string}) {
  return (
    <div className={cn('space-y-3 px-1 py-4', className)} aria-hidden>
      <Skeleton className="h-4 w-2/5" />
      {Array.from({length: rows}, (_, i) => (
        <div key={i} className="flex items-center gap-3">
          <Skeleton className="h-9 flex-1" />
          <Skeleton className="hidden h-9 w-24 sm:block" />
          <Skeleton className="hidden h-9 w-20 md:block" />
        </div>
      ))}
    </div>
  );
}

/** 卡片行骨架：账号列表/矩阵这类「一行一卡」的加载态 */
export function CardRowsSkeleton({rows = 4, className}: {rows?: number; className?: string}) {
  return (
    <div className={cn('space-y-2.5 px-3.5 py-3', className)} aria-hidden>
      {Array.from({length: rows}, (_, i) => (
        <div key={i} className="space-y-2 rounded-2xl bg-background/60 p-3">
          <div className="flex items-center justify-between gap-2">
            <Skeleton className="h-8 w-32" />
            <Skeleton className="h-5 w-16 rounded-full" />
          </div>
          <div className="flex items-center justify-between gap-3">
            <Skeleton className="h-4 w-24" />
            <Skeleton className="h-4 w-20" />
          </div>
        </div>
      ))}
    </div>
  );
}

/**
 * 模型列表骨架：models 页的表格是「名称+元信息、参数、徽章组」的宽行结构，
 * 用通栏条 + 右侧徽章组合模拟，替换为真实表格时高度节奏接近（行高约 40px）。
 * 之前该页加载态只有一行转圈文字（py-16），数据到达后整块替换，高度差明显。
 */
export function ModelRowsSkeleton({rows = 6, className}: {rows?: number; className?: string}) {
  return (
    <div className={cn('space-y-3 px-4 py-4', className)} aria-hidden>
      <Skeleton className="h-4 w-1/3" />
      {Array.from({length: rows}, (_, i) => (
        <div key={i} className="flex items-center gap-3 border-b border-border/40 pb-3 last:border-0">
          <div className="min-w-0 flex-1 space-y-1.5">
            <Skeleton className="h-3.5 w-28" />
            <Skeleton className="h-2.5 w-40" />
          </div>
          <Skeleton className="hidden h-6 w-14 md:block" />
          <Skeleton className="hidden h-6 w-14 lg:block" />
          <Skeleton className="h-6 w-16 rounded-md" />
        </div>
      ))}
    </div>
  );
}
