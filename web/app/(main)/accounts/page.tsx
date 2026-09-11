'use client';

import {useCallback, useEffect, useMemo, useState} from 'react';
import {
  Gift,
  Zap,
  KeyRound,
  Trash2,
  RefreshCw,
  Plus,
  Users,
  Power,
} from 'lucide-react';
import {notify} from '@/lib/toast';
import {accountApi, upstreamApi, errText} from '@/lib/api';
import type {Account, UpstreamStatus} from '@/lib/types';
import {expiryVisual, fmtRemain} from '@/lib/format';
import {PageHeader} from '@/components/common/layout/PageHeader';
import {EmptyState} from '@/components/common/layout/EmptyState';
import {ConfirmDialog} from '@/components/common/layout/ConfirmDialog';
import {AddAccountDialog} from '@/components/common/accounts/AddAccountDialog';
import {useAuth} from '@/lib/auth-context';
import {Button} from '@/components/ui/button';
import {Badge} from '@/components/ui/badge';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';

const TOKEN_TTL = 72 * 3600;

export default function AccountsPage() {
  const {isAdmin} = useAuth();
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [upstream, setUpstream] = useState<UpstreamStatus | null>(null);
  const [loading, setLoading] = useState(true);
  const [addOpen, setAddOpen] = useState(false);
  const [busyFile, setBusyFile] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    const [accRes, upRes] = await Promise.allSettled([accountApi.list(), upstreamApi.status()]);
    if (accRes.status === 'fulfilled') setAccounts(accRes.value.accounts);
    else notify.err(errText(accRes.reason));
    if (upRes.status === 'fulfilled') setUpstream(upRes.value);
    setLoading(false);
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  /** 将本地 auths 文件与上游账号池状态按 uid 合并 */
  const merged = useMemo(() => {
    const pool = new Map<string, Record<string, unknown>>();
    for (const item of upstream?.accounts ?? []) {
      const uid = String((item as Record<string, unknown>).uid ?? (item as Record<string, unknown>).UID ?? '');
      if (uid) pool.set(uid, item as Record<string, unknown>);
    }
    return accounts.map((a) => {
      const p = pool.get(a.uid);
      if (!p) return a;
      return {
        ...a,
        healthy: typeof p.healthy === 'boolean' ? p.healthy : null,
        disabled: typeof p.disabled === 'boolean' ? p.disabled : null,
        in_flight: typeof p.in_flight === 'number' ? p.in_flight : null,
        cooling: typeof p.cooling === 'boolean' ? p.cooling : null,
        last_used: typeof p.last_used === 'number' ? p.last_used : null,
      } satisfies Account;
    });
  }, [accounts, upstream]);

  /** 执行单账号操作（签到 / 测活 / 刷新 / 删除），成功后同步底栏计数 */
  async function run(file: string, fn: () => Promise<unknown>, okMsg: string) {
    setBusyFile(file);
    try {
      const res = (await fn()) as {message?: string; ok?: boolean};
      const ok = res.ok !== false;
      (ok ? notify.ok : notify.err)(res.message || okMsg);
      await load();
      window.dispatchEvent(new Event('workbuddy-manager:accounts-changed'));
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setBusyFile(null);
    }
  }

  /** 仅重启上游容器，不涉及单个账号，因此单独处理 */
  const [restarting, setRestarting] = useState(false);
  async function restartUpstream() {
    setRestarting(true);
    try {
      const res = await accountApi.restart();
      (res.ok ? notify.ok : notify.err)(res.message || '已重启上游');
      await load();
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setRestarting(false);
    }
  }

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader
        title="账号管理"
        description="腾讯 CodeBuddy 账号池：Token 有效期、签到与连通性"
        actions={
          <>
            <Button variant="outline" size="sm" className="rounded-full" onClick={load} disabled={loading}>
              <RefreshCw className={loading ? 'animate-spin' : ''} />
              刷新
            </Button>
            {isAdmin && (
              <ConfirmDialog
                title="强制重启上游容器？"
                description="通常无需手动执行：添加或删除账号后会自动重载。仅当上游状态异常、需要强制重载时才使用。重启约 0.5 秒，在途请求会正常完成。"
                confirmText="重启"
                onConfirm={restartUpstream}
                trigger={
                  <Button variant="outline" size="sm" className="rounded-full" disabled={restarting}>
                    <Power className={restarting ? 'animate-spin' : ''} />
                    强制重启
                  </Button>
                }
              />
            )}
            {isAdmin && (
              <Button size="sm" className="rounded-full" onClick={() => setAddOpen(true)}>
                <Plus />
                添加账号
              </Button>
            )}
          </>
        }
      />

      <section className="overflow-hidden rounded-[20px] bg-muted">
        <Table>
          <TableHeader>
            <TableRow className="border-b border-border/60 hover:bg-transparent">
              <TableHead className="pl-4 text-[11px] text-muted-foreground">昵称</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">UID</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">状态</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">Token 有效期</TableHead>
              {isAdmin && <TableHead className="pr-4 text-right text-[11px] text-muted-foreground">操作</TableHead>}
            </TableRow>
          </TableHeader>
          <TableBody>
            {merged.map((a) => {
              const pct = Math.min(100, Math.max(0, (a.remain_seconds / TOKEN_TTL) * 100));
              const disabled = a.disabled === true;
              const busy = busyFile === a.file;
              // 有效期分档改用共享规则（与仪表盘一致）
              const vis = expiryVisual(a.remain_seconds);
              const remainTone = vis.textClass;
              const barTone = vis.barColor;
              return (
                <TableRow key={a.file} className="border-b border-border/40">
                  <TableCell className="pl-4">
                    <div className="flex items-center gap-2.5">
                      <div
                        className={
                          'grid h-7 w-7 shrink-0 place-items-center rounded-full text-[11px] font-semibold ' +
                          (a.is_expired
                            ? 'bg-muted-foreground/20 text-muted-foreground'
                            : 'bg-primary text-primary-foreground')
                        }
                      >
                        {(a.nickname || '?').charAt(0)}
                      </div>
                      <span className={'truncate text-sm font-medium ' + (a.is_expired ? 'text-muted-foreground' : '')}>
                        {a.nickname || '未命名'}
                      </span>
                    </div>
                  </TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">{a.uid}</TableCell>
                  <TableCell>
                    {disabled ? (
                      <Badge variant="destructive" className="rounded-full">● 已禁用</Badge>
                    ) : a.is_expired ? (
                      <Badge variant="destructive" className="rounded-full">● 已过期</Badge>
                    ) : a.cooling ? (
                      <Badge variant="secondary" className="rounded-full text-amber-600 dark:text-amber-400">● 冷却中</Badge>
                    ) : (
                      <Badge variant="secondary" className="rounded-full text-emerald-600 dark:text-emerald-400">● 在线</Badge>
                    )}
                  </TableCell>
                  <TableCell>
                    <div className="w-[150px]">
                      <div className={'mb-1 text-[11px] font-medium tabular-nums ' + remainTone}>
                        {fmtRemain(a.remain_seconds)}
                      </div>
                      <div className="h-1.5 overflow-hidden rounded-full bg-border">
                        <div
                          className="h-full rounded-full transition-all"
                          style={{width: `${pct}%`, background: barTone}}
                        />
                      </div>
                    </div>
                  </TableCell>
                  {isAdmin && (
                    <TableCell className="pr-4">
                      <div className="flex justify-end gap-1">
                        <Button
                          variant="ghost"
                          size="icon"
                          className="h-7 w-7 rounded-md"
                          title="签到"
                          disabled={busy}
                          onClick={() => run(a.file, () => accountApi.checkin(a.file), '操作完成')}
                        >
                          <Gift className="h-3.5 w-3.5" />
                        </Button>
                        <Button
                          variant="ghost"
                          size="icon"
                          className="h-7 w-7 rounded-md"
                          title="连通性测试"
                          disabled={busy}
                          onClick={() => run(a.file, () => accountApi.test(a.file), '测试完成')}
                        >
                          <Zap className="h-3.5 w-3.5" />
                        </Button>
                        <Button
                          variant="ghost"
                          size="icon"
                          className="h-7 w-7 rounded-md"
                          title="刷新 Token"
                          disabled={busy}
                          onClick={() => run(a.file, () => accountApi.refresh(a.file), '刷新完成')}
                        >
                          <KeyRound className="h-3.5 w-3.5" />
                        </Button>
                        <ConfirmDialog
                          title={`删除账号「${a.nickname || a.uid}」？`}
                          description="将删除本地授权文件，并自动重载上游使其生效。此操作不可撤销。"
                          confirmText="删除"
                          destructive
                          onConfirm={() => run(a.file, () => accountApi.remove(a.file), '已删除')}
                          trigger={
                            <Button
                              variant="ghost"
                              size="icon"
                              className="h-7 w-7 rounded-md text-red-500 hover:text-red-600"
                              title="删除"
                            >
                              <Trash2 className="h-3.5 w-3.5" />
                            </Button>
                          }
                        />
                      </div>
                    </TableCell>
                  )}
                </TableRow>
              );
            })}
          </TableBody>
        </Table>

        {!merged.length && !loading && (
          <EmptyState
            icon={Users}
            title="暂无账号"
            description={isAdmin ? '点击右上角「添加账号」扫码授权' : '请联系管理员添加账号'}
            className="flex flex-col items-center justify-center py-16 text-center"
          >
            {isAdmin && (
              <Button className="mt-4 rounded-full" onClick={() => setAddOpen(true)}>
                <Plus />
                添加账号
              </Button>
            )}
          </EmptyState>
        )}
        {loading && !merged.length && (
          <div className="py-16 text-center text-xs text-muted-foreground">加载中…</div>
        )}
      </section>

      <AddAccountDialog open={addOpen} onOpenChange={setAddOpen} onSuccess={load} />
    </div>
  );
}
