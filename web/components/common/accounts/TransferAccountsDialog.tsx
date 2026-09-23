'use client';

import {useCallback, useRef, useState, type ReactNode} from 'react';
import {AlertTriangle, CheckCircle2, Download, Loader2, Upload} from 'lucide-react';
import {notify} from '@/lib/toast';
import {useT} from '@/lib/i18n/provider';
import {accountApi, errText} from '@/lib/api';
import {Button} from '@/components/ui/button';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/animate-ui/radix/dialog';

/**
 * 导入结果一行（后端 skipped 逐项带原因）。
 * 单独渲染出来而不是拼成一长串：坏掉的文件往往不止一个，用户要逐个对号。
 */
function SkippedList({items, emptyText}: {items: {uid?: string; reason: string}[]; emptyText: string}) {
  if (!items.length) return null;
  return (
    <div className="max-h-40 space-y-1.5 overflow-y-auto rounded-lg bg-muted/60 p-3">
      {items.map((s, i) => (
        <div key={i} className="flex items-start gap-1.5 text-[11px] leading-4 text-muted-foreground">
          <AlertTriangle className="mt-0.5 h-3 w-3 shrink-0 text-amber-500" />
          <span className="min-w-0 break-all">
            {s.uid ? `${s.uid}: ${s.reason}` : s.reason}
          </span>
        </div>
      ))}
      {!items.length && <div className="text-[11px] text-muted-foreground">{emptyText}</div>}
    </div>
  );
}

/**
 * 账号凭据导入对话框：选本地 JSON（导出文件 / 单个 auth 文件 / 裸数组），
 * 原文 POST 给 /api/accounts/import。文件内容不在前端解析校验——后端
 * auth.Parse 是唯一权威，前端只管读文本与展示结果。
 */
export function ImportAccountsDialog({
  open,
  onOpenChange,
  onSuccess,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  onSuccess?: () => void;
}) {
  const t = useT();
  const fileRef = useRef<HTMLInputElement>(null);
  const [fileName, setFileName] = useState('');
  const [raw, setRaw] = useState('');
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState<{imported: number; skipped: {uid?: string; reason: string}[]} | null>(null);
  const [error, setError] = useState('');

  const reset = useCallback(() => {
    setFileName('');
    setRaw('');
    setDone(null);
    setError('');
  }, []);

  const pickFile = useCallback(async (file: File) => {
    reset();
    setFileName(file.name);
    try {
      const text = await file.text();
      if (text.length > 20 * 1024 * 1024) {
        setError(t('accounts.importTooLarge'));
        return;
      }
      setRaw(text);
    } catch {
      setError(t('accounts.importReadError'));
    }
  }, [reset, t]);

  const submit = useCallback(async () => {
    if (!raw) return;
    setBusy(true);
    setError('');
    try {
      const res = await accountApi.import(raw);
      setDone({imported: res.imported, skipped: res.skipped ?? []});
      if (res.imported > 0) {
        notify.ok(t('accounts.importDone', {n: res.imported}));
        window.dispatchEvent(new Event('workbuddy-manager:accounts-changed'));
        onSuccess?.();
      } else {
        notify.err(t('accounts.importNone'));
      }
    } catch (e) {
      setError(errText(e));
    } finally {
      setBusy(false);
    }
  }, [raw, onSuccess, t]);

  return (
    <Dialog
      open={open}
      onOpenChange={(v) => {
        if (!v) reset();
        onOpenChange(v);
      }}
    >
      <DialogContent className="max-w-[440px]" showCloseButton>
        <DialogHeader>
          <DialogTitle>{t('accounts.importTitle')}</DialogTitle>
          <DialogDescription>{t('accounts.importDesc')}</DialogDescription>
        </DialogHeader>

        <div className="flex w-full flex-col gap-3 px-6 pb-6">
          {/* 选择文件：label 关联隐藏 input，整块可点 */}
          <input
            ref={fileRef}
            type="file"
            accept=".json,application/json"
            className="hidden"
            onChange={(e) => {
              const f = e.target.files?.[0];
              if (f) void pickFile(f);
              e.target.value = '';
            }}
          />
          <button
            type="button"
            onClick={() => fileRef.current?.click()}
            className="flex w-full flex-col items-center gap-1.5 rounded-xl border border-dashed border-border px-4 py-6 text-center transition-colors hover:bg-muted/60"
          >
            <Upload className="h-5 w-5 text-muted-foreground" />
            <span className="text-xs font-medium">
              {fileName || t('accounts.importPickFile')}
            </span>
            {!fileName && (
              <span className="text-[10px] text-muted-foreground">{t('accounts.importPickHint')}</span>
            )}
          </button>

          {done && (
            <div className="flex items-start gap-2 rounded-lg bg-muted/60 p-3">
              {done.imported > 0 ? (
                <CheckCircle2 className="mt-0.5 h-4 w-4 shrink-0 text-emerald-500" />
              ) : (
                <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0 text-amber-500" />
              )}
              <div className="min-w-0 flex-1 space-y-2">
                <div className="text-xs font-medium">
                  {t('accounts.importSummary', {n: done.imported, s: done.skipped.length})}
                </div>
                {done.skipped.length > 0 && <SkippedList items={done.skipped} emptyText="" />}
              </div>
            </div>
          )}

          {error && (
            <div className="flex items-start gap-2 rounded-lg bg-red-500/10 p-3">
              <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0 text-red-500" />
              <span className="min-w-0 break-all text-xs text-red-600 dark:text-red-400">{error}</span>
            </div>
          )}

          <div className="flex w-full gap-2">
            <Button
              variant="outline"
              className="flex-1 rounded-full"
              disabled={busy}
              onClick={() => onOpenChange(false)}
            >
              {t('common.cancel')}
            </Button>
            <Button className="flex-1 rounded-full" disabled={busy || !raw} onClick={submit}>
              {busy ? <Loader2 className="animate-spin" /> : <Upload />}
              {t('accounts.importAction')}
            </Button>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  );
}

/**
 * 导出按钮：拉 blob → 触发浏览器下载。导出文件含全部账号 token，
 * 确认弹窗写明风险（ConfirmDialog 语义）后再真正请求。
 */
export function ExportAccountsButton({disabled}: {disabled?: boolean}) {
  const t = useT();
  const [busy, setBusy] = useState(false);

  const download = useCallback(async () => {
    setBusy(true);
    try {
      const {blob, filename} = await accountApi.exportAll();
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = filename;
      a.click();
      URL.revokeObjectURL(url);
      notify.ok(t('accounts.exportDone'));
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setBusy(false);
    }
  }, [t]);

  return (
    <Button
      size="sm"
      variant="outline"
      className="rounded-full"
      disabled={busy || disabled}
      onClick={() => void download()}
      title={t('accounts.exportTitle')}
    >
      {busy ? <Loader2 className="animate-spin" /> : <Download />}
      <span className="hidden sm:inline">{t('accounts.export')}</span>
    </Button>
  );
}

/** 导入按钮（包状态，供 accounts 页组合进工具栏）。 */
export function ImportAccountsButton({
  onOpen,
  disabled,
  children,
}: {
  onOpen: () => void;
  disabled?: boolean;
  children?: ReactNode;
}) {
  const t = useT();
  return (
    <Button
      size="sm"
      variant="outline"
      className="rounded-full"
      disabled={disabled}
      onClick={onOpen}
      title={t('accounts.importTitle')}
    >
      <Upload />
      <span className="hidden sm:inline">{children ?? t('accounts.import')}</span>
    </Button>
  );
}
