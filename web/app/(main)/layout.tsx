'use client';

import {memo, useEffect} from 'react';
import {usePathname, useRouter} from 'next/navigation';
import {motion, useReducedMotion} from 'motion/react';
import {ManagementBar} from '@/components/common/layout/ManagementBar';
import {LanguageToggle} from '@/components/common/layout/LanguageToggle';
import {ThemeToggle} from '@/components/common/layout/ThemeToggle';
import {RealmToggle} from '@/components/common/layout/RealmToggle';
import {RealmProvider} from '@/lib/realm-context';
import {useAuth} from '@/lib/auth-context';

const MemoizedManagementBar = memo(ManagementBar);

export default function MainLayout({
  children,
}: {
  children: React.ReactNode
}) {
  const {me, loading} = useAuth();
  const router = useRouter();
  const pathname = usePathname();
  const reduceMotion = useReducedMotion();

  // 仅在确认未登录时跳转；不阻塞内容渲染，避免每次切页闪一下
  useEffect(() => {
    if (!loading && !me) router.replace('/login');
  }, [loading, me, router]);

  return (
    <RealmProvider>
      <div className="min-h-screen flex flex-col">
        <MemoizedManagementBar />
        <div className="flex flex-1 flex-col">
          <div className="@container/main flex flex-1 flex-col gap-2">
            <div className="flex min-h-0 flex-1 flex-col px-4 pt-12 py-8 sm:px-6 md:px-8 lg:px-12">
              <div className="mx-auto flex min-h-0 w-full max-w-7xl flex-1 flex-col gap-4 pb-24 md:gap-6">
                {/*
                  版本切换固定在右上角：底栏是照 LDC 原样保留的，控件不往那里加。
                  移动端只显示图标（compact），避免窄屏被它占掉一行。
                */}
                <div className="flex flex-wrap items-center justify-end gap-2">
                  <ThemeToggle />
                  <LanguageToggle />
                  <RealmToggle />
                </div>
                {/*
                  切页入场动画：key 取路径，换页即重放；只动 opacity/transform。
                  不包 ManagementBar；尊重系统"减弱动态效果"（此时直接呈现，不播动画）。
                  reduced-motion 值只用于 transition（不进 SSR 标记），不会有水合不一致。
                */}
                <motion.div
                  key={pathname}
                  initial={{opacity: 0, y: 8}}
                  animate={{opacity: 1, y: 0}}
                  transition={reduceMotion ? {duration: 0} : {duration: 0.28, ease: 'easeOut'}}
                  className="flex min-h-0 flex-1 flex-col"
                >
                  {children}
                </motion.div>
              </div>
            </div>
          </div>
        </div>
      </div>
    </RealmProvider>
  );
}
