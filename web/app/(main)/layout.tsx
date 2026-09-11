'use client';

import {memo, useEffect} from 'react';
import {useRouter} from 'next/navigation';
import {ManagementBar} from '@/components/common/layout/ManagementBar';
import {useAuth} from '@/lib/auth-context';

const MemoizedManagementBar = memo(ManagementBar);

export default function MainLayout({
  children,
}: {
  children: React.ReactNode
}) {
  const {me, loading} = useAuth();
  const router = useRouter();

  useEffect(() => {
    if (!loading && !me) router.replace('/login');
  }, [loading, me, router]);

  if (loading || !me) {
    return (
      <div className="grid min-h-screen place-items-center text-xs text-muted-foreground">
        正在校验登录态…
      </div>
    );
  }

  return (
    <div className="min-h-screen flex flex-col">
      <MemoizedManagementBar />
      <div className="flex flex-1 flex-col">
        <div className="@container/main flex flex-1 flex-col gap-2">
          <div className="flex min-h-0 flex-1 flex-col px-4 pt-12 py-8 sm:px-6 md:px-8 lg:px-12">
            <div className="mx-auto flex min-h-0 w-full max-w-7xl flex-1 flex-col gap-4 pb-24 md:gap-6">
              {children}
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
