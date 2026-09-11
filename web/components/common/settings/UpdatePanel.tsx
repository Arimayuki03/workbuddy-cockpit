'use client';

import {useCallback, useEffect, useMemo, useRef, useState} from 'react';
import {
  AlertTriangle,
  CheckCircle2,
  DownloadCloud,
  Loader2,
  RefreshCw,
  RotateCcw,
  Server,
  Sparkles,
  Terminal,
  X,
  XCircle,
} from 'lucide-react';
import {errText, systemApi} from '@/lib/api';
import type {UpdateCheck, UpdateStatus} from '@/lib/types';
import {notify} from '@/lib/toast';
import {fmtAgo} from '@/lib/format';
import {useAuth} from '@/lib/auth-context';
import {Button} from '@/components/ui/button';
import {Badge} from '@/components/ui/badge';
import {ConfirmDialog} from '@/components/common/layout/ConfirmDialog';

/** 更新对象说明，用于确认弹窗与按钮文案 */
const TARGETS = [
  {
    id: 'both' as const,
    label: '全部更新',
    desc: '上游 workbuddy2api + 管理端',
    hint: '上游会重建容器（保留账号与配置），管理端会重启服务',
  },
  {
    id: 'upstream' as const,
    label: '仅上游',
    desc: 'workbuddy2api（账号池 / 反代核心）',
    hint: '拉取上游代码 → 重建容器。端口绑定会重新收敛为仅本机',
  },
  {
    id: 'manager' as const,
    label: '仅管理端',
    desc: '本控制台程序',
    hint: '下载最新 Release → 替换代码与前端 → 重启服务',
  },
];

/** 把检测结果拼成一句可读摘要，用于提醒 */
function describeAvailable(c: UpdateCheck): string {
  const parts: string[] = [];
  if (c.manager.has_update) parts.push(`管理端 ${c.manager.latest}`);
  if (c.upstream.has_update) parts.push(`上游 ${c.upstream.latest}`);
  return parts.join(' · ');
}

export function UpdatePanel() {
  const {isAdmin} = useAuth();
  const [status, setStatus] = useState<UpdateStatus | null>(null);
  const [busy, setBusy] = useState(false);
  const [versions, setVersions] = useState<{manager: string; upstream_connected: boolean; upstream_accounts: number | null} | null>(null);
  const [check, setCheck] = useState<UpdateCheck | null>(null);
  const [checking, setChecking] = useState(false);
  /**
   * 结果横幅是否已被关闭。用「是否正在运行」的边沿来复位：
   * 状态从 running 变为结束、或再次发起更新时会重新出现，
   * 不依赖 finished_at 是否存在，避免时间戳缺失时关不掉。
   */
  const [resultDismissed, setResultDismissed] = useState(false);
  const logRef = useRef<HTMLDivElement>(null);

  const load = useCallback(async () => {
    try {
      const [st, vs, ck] = await Promise.allSettled([
        systemApi.updateStatus(),
        systemApi.versions(),
        systemApi.checkUpdate(),
      ]);
      if (st.status === 'fulfilled') setStatus(st.value);
      if (vs.status === 'fulfilled') setVersions(vs.value);
      if (ck.status === 'fulfilled') setCheck(ck.value);
    } catch {
      /* 更新期间服务重启，忽略瞬时失败 */
    }
  }, []);

  /** 强制重新检测（绕过服务端 6 小时缓存） */
  const recheck = useCallback(async () => {
    setChecking(true);
    try {
      const r = await systemApi.checkUpdate(true);
      setCheck(r);
      if (r.has_any) {
        notify.warn('发现新版本', describeAvailable(r));
      } else {
        notify.ok('已是最新版本', '管理端与上游均无更新');
      }
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setChecking(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  // 运行中高频轮询，空闲时低频（服务可能在更新中短暂不可用）
  const running = !!status?.running;
  useEffect(() => {
    const interval = running ? 1500 : 15000;
    const timer = window.setInterval(() => {
      void load();
    }, interval);
    return () => window.clearInterval(timer);
  }, [load, running]);

  // running 一旦变为 true，就复位「已关闭」，使本轮结果在结束后重新可见
  useEffect(() => {
    if (running) setResultDismissed(false);
  }, [running]);

  // 日志自动滚到底
  useEffect(() => {
    if (logRef.current) {
      logRef.current.scrollTop = logRef.current.scrollHeight;
    }
  }, [status?.logs]);

  const logs = status?.logs ?? [];
  const logText = useMemo(
    () => (logs.length ? logs.map((l) => l.text).join('\n') : status?.log_tail || ''),
    [logs, status?.log_tail],
  );

  async function start(target: 'manager' | 'upstream' | 'both') {
    setBusy(true);
    try {
      const r = await systemApi.startUpdate(target);
      notify.ok('更新已开始', r.message);
      // 立即拉一次，进入高频轮询
      await load();
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setBusy(false);
    }
  }

  const done = !!status && !status.running && status.ok !== null && !resultDismissed;

  return (
    <div className="space-y-4">
      {/* 当前版本 */}
      <div className="rounded-[20px] bg-muted p-4">
        <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
          <div className="flex items-center gap-2 text-sm font-medium">
            <Server className="h-4 w-4" />
            当前版本
          </div>
          <div className="flex items-center gap-2">
            <Button
              variant="outline"
              size="sm"
              className="rounded-full"
              onClick={recheck}
              disabled={checking || running}
            >
              <RefreshCw className={checking ? 'animate-spin' : ''} />
              检测更新
            </Button>
            <Button variant="outline" size="sm" className="rounded-full" onClick={load} disabled={running}>
              <RefreshCw className={running ? 'animate-spin' : ''} />
              刷新
            </Button>
          </div>
        </div>
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
          <div className="rounded-2xl bg-background/60 px-3.5 py-3">
            <div className="text-[11px] text-muted-foreground">管理端</div>
            <div className="mt-1 text-sm font-semibold tabular-nums">{versions?.manager || status?.version || '—'}</div>
          </div>
          <div className="rounded-2xl bg-background/60 px-3.5 py-3">
            <div className="text-[11px] text-muted-foreground">上游连接</div>
            <div className="mt-1 flex items-center gap-1.5 text-sm font-semibold">
              {versions?.upstream_connected ? (
                <>
                  <span className="text-emerald-600 dark:text-emerald-400">正常</span>
                  {typeof versions.upstream_accounts === 'number' && (
                    <span className="text-xs font-normal text-muted-foreground">({versions.upstream_accounts} 个账号)</span>
                  )}
                </>
              ) : (
                <span className="text-red-600 dark:text-red-400">不可用</span>
              )}
            </div>
          </div>
          <div className="rounded-2xl bg-background/60 px-3.5 py-3">
            <div className="text-[11px] text-muted-foreground">上次更新</div>
            <div className="mt-1 text-sm font-semibold">
              {status?.finished_at ? fmtAgo(status.finished_at) : '从未'}
            </div>
          </div>
        </div>
        {status?.upstream_dir && (
          <div className="mt-2 truncate font-mono text-[11px] text-muted-foreground" title={status.upstream_dir}>
            上游目录：{status.upstream_dir}
          </div>
        )}
      </div>

      {/* 新版本提醒 */}
      {check?.has_any && !status?.running && (
        <div className="rounded-[20px] border border-blue-500/40 bg-blue-500/10 p-4">
          <div className="flex flex-wrap items-start justify-between gap-3">
            <div className="flex items-start gap-2.5">
              <Sparkles className="mt-0.5 h-4 w-4 shrink-0 text-blue-500" />
              <div className="space-y-1.5">
                <div className="text-xs font-medium">发现新版本可用</div>
                {check.manager.has_update && (
                  <div className="text-[11px] text-muted-foreground">
                    管理端：{check.manager.current} →{' '}
                    <span className="font-medium text-foreground">{check.manager.latest}</span>
                  </div>
                )}
                {check.upstream.has_update && (
                  <div className="space-y-0.5 text-[11px] text-muted-foreground">
                    <div>
                      上游：{check.upstream.current || '未知'} →{' '}
                      <span className="font-medium text-foreground">{check.upstream.latest}</span>
                    </div>
                    {check.upstream.subject && (
                      <div className="break-all">
                        最新提交：{check.upstream.subject}
                        {check.upstream.date && `（${check.upstream.date.slice(0, 10)}）`}
                      </div>
                    )}
                  </div>
                )}
                <div className="text-[11px] text-muted-foreground">
                  点下方按钮即可更新，账号与配置会自动保留。
                </div>
              </div>
            </div>
            <div className="flex gap-2">
              {check.upstream.has_update && (
                <Button
                  size="sm"
                  variant="outline"
                  className="rounded-full"
                  disabled={!isAdmin || busy || running}
                  onClick={async () => {
                    setBusy(true);
                    try {
                      const r = await systemApi.startUpdate('upstream');
                      notify.ok('更新已开始', r.message);
                      await load();
                    } catch (e) {
                      notify.err(errText(e));
                    } finally {
                      setBusy(false);
                    }
                  }}
                >
                  更新上游
                </Button>
              )}
              {check.manager.has_update && (
                <Button
                  size="sm"
                  className="rounded-full"
                  disabled={!isAdmin || busy || running}
                  onClick={async () => {
                    setBusy(true);
                    try {
                      const r = await systemApi.startUpdate('manager');
                      notify.ok('更新已开始', r.message);
                      await load();
                    } catch (e) {
                      notify.err(errText(e));
                    } finally {
                      setBusy(false);
                    }
                  }}
                >
                  <DownloadCloud />
                  更新管理端
                </Button>
              )}
            </div>
          </div>
        </div>
      )}

      {check && !check.has_any && !check.manager.error && !check.upstream.error && !status?.running && (
        <div className="flex items-center gap-2 rounded-[20px] border border-emerald-500/30 bg-emerald-500/10 px-4 py-3">
          <CheckCircle2 className="h-4 w-4 shrink-0 text-emerald-500" />
          <span className="text-xs">管理端与上游均为最新版本</span>
          {check.checked_at > 0 && (
            <span className="text-[11px] text-muted-foreground">（{fmtAgo(check.checked_at)}检测）</span>
          )}
        </div>
      )}

      {/* 更新状态 */}
      {status?.running && (
        <div className="flex items-start gap-2.5 rounded-[20px] border border-blue-500/30 bg-blue-500/10 p-4">
          <Loader2 className="mt-0.5 h-4 w-4 shrink-0 animate-spin text-blue-500" />
          <div className="min-w-0 flex-1 space-y-1">
            <div className="text-xs font-medium">正在更新：{status.step || '执行中'}</div>
            <div className="text-[11px] text-muted-foreground">
              更新期间服务可能短暂重启（页面自动重连），请勿关闭服务器。
              进度每 1.5 秒刷新。
            </div>
          </div>
        </div>
      )}

      {done && (
        <div
          className={
            'flex items-start gap-2.5 rounded-[20px] border p-4 ' +
            (status?.ok
              ? 'border-emerald-500/30 bg-emerald-500/10'
              : 'border-red-500/30 bg-red-500/10')
          }
        >
          {status?.ok ? (
            <CheckCircle2 className="mt-0.5 h-4 w-4 shrink-0 text-emerald-500" />
          ) : (
            <XCircle className="mt-0.5 h-4 w-4 shrink-0 text-red-500" />
          )}
          <div className="min-w-0 flex-1 space-y-1">
            <div className="text-xs font-medium">{status?.ok ? '更新完成' : '更新未完成'}</div>
            <div className="text-[11px] text-muted-foreground">
              {status?.ok
                ? (status?.duration ? `耗时 ${status.duration} 秒。` : '') +
                  '服务已重启，建议刷新页面确认版本号。'
                : '请查看下方日志排查；账号与配置未受影响。'}
            </div>
          </div>
          <button
            type="button"
            aria-label="关闭提示"
            title="关闭提示"
            className="-m-1 shrink-0 rounded-full p-1 text-muted-foreground transition-colors hover:bg-foreground/10 hover:text-foreground"
            onClick={() => setResultDismissed(true)}
          >
            <X className="h-4 w-4" />
          </button>
        </div>
      )}

      {status && status.updater_found === false && (
        <div className="flex items-start gap-2.5 rounded-[20px] border border-amber-500/30 bg-amber-500/10 p-4">
          <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0 text-amber-500" />
          <div className="space-y-1">
            <div className="text-xs font-medium">未找到更新脚本</div>
            <div className="text-[11px] text-muted-foreground">
              需要 <code className="font-mono">deploy/update.py</code> 存在。
              若使用旧版安装包，请先用新包重新部署一次。
            </div>
          </div>
        </div>
      )}

      {/* 更新操作 */}
      <div className="rounded-[20px] bg-muted p-4">
        <div className="mb-1 text-sm font-medium">一键更新</div>
        <div className="mb-3 text-[11px] leading-4 text-muted-foreground">
          从 GitHub 拉取最新代码并自动重建/重启。
          账号授权文件、上游配置、密钥与日志数据都会保留。
        </div>
        <div className="grid grid-cols-1 gap-2 sm:grid-cols-3">
          {TARGETS.map((t, i) => (
            <ConfirmDialog
              key={t.id}
              title={`确认${t.label}？`}
              description={`${t.desc}。${t.hint}。更新过程中服务会短暂中断，已完成的任务不受影响。`}
              confirmText="开始更新"
              onConfirm={() => start(t.id)}
              trigger={
                <button
                  type="button"
                  disabled={!isAdmin || busy || running}
                  className={
                    'flex flex-col items-start gap-1 rounded-2xl px-3.5 py-3 text-left transition-colors ' +
                    'bg-background/60 hover:bg-background disabled:cursor-not-allowed disabled:opacity-50'
                  }
                >
                  <div className="flex items-center gap-1.5 text-xs font-medium">
                    {i === 0 ? <DownloadCloud className="h-3.5 w-3.5" /> : <RotateCcw className="h-3.5 w-3.5" />}
                    {t.label}
                  </div>
                  <div className="text-[11px] leading-4 text-muted-foreground">{t.desc}</div>
                </button>
              }
            />
          ))}
        </div>
        {!isAdmin && (
          <p className="mt-2 text-[11px] text-muted-foreground">只读角色无法执行更新。</p>
        )}
      </div>

      {/* 日志 */}
      <div className="rounded-[20px] bg-muted p-4">
        <div className="mb-2 flex items-center justify-between">
          <div className="flex items-center gap-2 text-sm font-medium">
            <Terminal className="h-4 w-4" />
            更新日志
          </div>
          {logs.length > 0 && (
            <Badge variant="secondary" className="rounded-full text-[10px]">
              {logs.length} 行
            </Badge>
          )}
        </div>
        {logText ? (
          <div
            ref={logRef}
            className="max-h-[320px] overflow-auto rounded-2xl bg-background/60 p-3"
          >
            <pre className="whitespace-pre-wrap break-all font-mono text-[11px] leading-5 text-muted-foreground">
              {logText}
            </pre>
          </div>
        ) : (
          <div className="rounded-2xl bg-background/60 px-3 py-8 text-center text-xs text-muted-foreground">
            暂无日志。执行更新后这里会实时显示进度。
          </div>
        )}
      </div>
    </div>
  );
}
