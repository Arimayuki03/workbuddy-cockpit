'use client';

import {motion, useReducedMotion} from 'motion/react';
import type {ReactNode} from 'react';

export function PageHeader({
  title,
  description,
  actions,
}: {
  title: string;
  description?: string;
  actions?: ReactNode;
}) {
  const reduceMotion = useReducedMotion();
  return (
    // 与 (main)/layout 容器同向向下入场（y: -6 → 0）且时长对齐容器的 0.22s：
    // 之前容器向上、标题向下，两个位移互相抵消，入场感发糊。
    <motion.div
      initial={reduceMotion ? false : {opacity: 0, y: -6}}
      animate={{opacity: 1, y: 0}}
      transition={{duration: 0.22, ease: [0.22, 1, 0.36, 1]}}
      className="flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between"
    >
      <div className="space-y-1">
        <h1 className="text-lg font-semibold tracking-[-0.01em]">{title}</h1>
        {description && (
          <p className="text-xs text-muted-foreground">{description}</p>
        )}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </motion.div>
  );
}
