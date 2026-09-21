'use client';

import React from 'react';
import {LucideIcon} from 'lucide-react';
import {motion, useReducedMotion, type Variants} from 'motion/react';
import {cn} from '@/lib/utils';

/**
 * 空状态组件的Props接口
 */
interface EmptyStateProps {
  /** 图标组件 */
  icon?: LucideIcon;
  /** 主标题 */
  title: string;
  /** 描述文本 */
  description?: string;
  /** 自定义类名（与默认样式合并，而非整体替换） */
  className?: string;
  /** 自定义按钮或操作 */
  children?: React.ReactNode;
}

/**
 * 空状态的默认高度下限：数据到达前与到达后，空态块与实际内容块的
 * 高度差是「二次跳动」的来源之一。min-h 保证空态不会塌成一行字，
 * 传入 className 时也通过 cn 合并保留这条下限（可被覆盖）。
 */
const EMPTY_STATE_MIN_H = 'min-h-[240px]';

/**
 * 通用空状态组件
 */
export function EmptyState({
  icon: Icon,
  title,
  description,
  className,
  children,
}: EmptyStateProps) {
  const reduceMotion = useReducedMotion();

  const containerVariants: Variants = {
    hidden: {opacity: 0, y: 20},
    visible: {
      opacity: 1,
      y: 0,
      transition: {
        duration: 0.6,
        ease: 'easeOut',
        staggerChildren: 0.1,
      },
    },
  };

  const itemVariants: Variants = {
    hidden: {opacity: 0, y: 10, scale: 0.95},
    visible: {
      opacity: 1,
      y: 0,
      scale: 1,
      transition: {
        duration: 0.4,
        ease: 'easeOut',
      },
    },
  };

  const iconVariants: Variants = {
    hidden: {opacity: 0, scale: 0.8},
    visible: {
      opacity: 1,
      scale: 1,
      transition: {
        duration: 0.5,
        ease: 'backOut',
      },
    },
  };

  return (
    <motion.div
      className={cn(
        'flex h-full flex-col items-center justify-center p-8 text-center',
        EMPTY_STATE_MIN_H,
        className,
      )}
      initial={reduceMotion ? false : 'hidden'}
      animate="visible"
      variants={containerVariants}
    >
      {Icon && (
        <motion.div
          className="mx-auto w-15 h-15 bg-muted rounded-full flex items-center justify-center mb-4"
          variants={iconVariants}
        >
          <Icon className="w-8 h-8 text-muted-foreground" />
        </motion.div>
      )}
      <motion.div
        className="mb-2 text-base font-bold"
        variants={itemVariants}
      >
        {title}
      </motion.div>
      {description && (
        <motion.div
          className="mb-4 text-xs text-muted-foreground"
          variants={itemVariants}
        >
          {description}
        </motion.div>
      )}
      {children && (
        <motion.div variants={itemVariants}>
          {children}
        </motion.div>
      )}
    </motion.div>
  );
}
