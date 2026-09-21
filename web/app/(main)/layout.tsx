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
                  切页过渡动画：key 取路径，换页即重放；只动 opacity/transform。
                  不包 ManagementBar；尊重系统"减弱动态效果"（此时直接呈现，不播动画）。
                  reduced-motion 值只用于 transition（不进 SSR 标记），不会有水合不一致。

                  入场 0.22s + easeOutQuint 类曲线（[0.22,1,0.36,1]），前段提速、后段
                  长尾收敛；y 位移 6px 只做"落定"暗示，不做明显位移。容器向下入场
                  （y: -6 → 0），页内 PageHeader/StatCard 也统一向下入场，方向一致
                  才不会互相抵消（此前容器向上、标题向下，入场感发糊）。
                  退场 0.12s 快速淡出且不位移：切页的观感瓶颈在"等待"，让旧内容
                  尽快让位，新页入场从感知上提前；不做位移避免新旧页上下对跳
                  （退场位移动画需要 AnimatePresence 双节点占位，滚动容器会抖）。

                  min-h：内容区在骨架/空态阶段不至于塌陷——数据后到导致的高度突变
                  （二次跳动）是抖动根因，这里先给容器一个稳定的最小高度兜底，
                  让短内容页面与长内容页面之间的切换不至于引发滚动位置突变。
                  flex-1 负责拉伸到占满，min-h 只是下限，不会压高正常内容。

                  will-change 只在动画进行期间由 motion 自动挂除，不常驻。
                */}
                <motion.div
                  key={pathname}
                  initial={reduceMotion ? false : {opacity: 0, y: -6}}
                  animate={{opacity: 1, y: 0}}
                  transition={reduceMotion ? {duration: 0} : {duration: 0.22, ease: [0.22, 1, 0.36, 1]}}
                  className="content-stable flex min-h-0 flex-1 flex-col will-change-[transform,opacity]"
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
