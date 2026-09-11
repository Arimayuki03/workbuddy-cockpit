'use client';

import {useCallback, useEffect, useRef, useState} from 'react';
import {QRCodeSVG} from 'qrcode.react';
import {toast} from 'sonner';
import {Loader2, CheckCircle2, AlertTriangle, ExternalLink} from 'lucide-react';
import {accountApi, errText} from '@/lib/api';
import {Button} from '@/components/ui/button';
import {
  Dialog,
  DialogContent,
  DialogTitle,
  DialogDescription,
  DialogHeader,
} from '@/components/animate-ui/radix/dialog';

type Phase = 'loading' | 'waiting' | 'success' | 'error';

export function AddAccountDialog({
  open,
  onOpenChange,
  onSuccess,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  onSuccess?: () => void;
}) {
  const [phase, setPhase] = useState<Phase>('loading');
  const [authUrl, setAuthUrl] = useState('');
  const [message, setMessage] = useState('');
  const stateRef = useRef('');
  const timerRef = useRef<number | null>(null);

  const stopPoll = useCallback(() => {
    if (timerRef.current !== null) {
      window.clearInterval(timerRef.current);
      timerRef.current = null;
    }
  }, []);

  const start = useCallback(async () => {
    stopPoll();
    setPhase('loading');
    setMessage('正在向腾讯申请授权链接…');
    setAuthUrl('');
    try {
      const data = await accountApi.start();
      stateRef.current = data.state;
      setAuthUrl(data.authUrl);
      setPhase('waiting');
      setMessage('等待手机扫码确认…');

      timerRef.current = window.setInterval(async () => {
        try {
          const res = await accountApi.poll(stateRef.current);
          if (res.status === 'success') {
            stopPoll();
            setPhase('success');
            setMessage(`账号「${res.nickname || res.uid}」授权成功${res.updated ? '（已更新）' : ''}`);
            toast.success(`账号「${res.nickname || res.uid}」授权成功`);
            onSuccess?.();
            window.setTimeout(() => onOpenChange(false), 1600);
          } else if (res.status === 'expired' || res.status === 'invalid') {
            stopPoll();
            setPhase('error');
            setMessage('二维码已失效，请关闭后重试');
          }
        } catch {
          /* 忽略单次轮询错误，等待下次 */
        }
      }, 2000);
    } catch (e) {
      setPhase('error');
      setMessage(errText(e));
    }
  }, [onOpenChange, onSuccess, stopPoll]);

  useEffect(() => {
    if (open) {
      start();
    } else {
      stopPoll();
    }
    return stopPoll;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-[420px]" showCloseButton>
        <DialogHeader>
          <DialogTitle>添加腾讯账号</DialogTitle>
          <DialogDescription>使用微信 / QQ 扫码完成授权，成功后自动签到并纳管</DialogDescription>
        </DialogHeader>

        <div className="flex flex-col items-center gap-4 px-6 pb-6">
          {phase === 'loading' && (
            <div className="grid h-[232px] w-[232px] place-items-center rounded-2xl bg-muted">
              <Loader2 className="h-6 w-6 animate-spin text-muted-foreground" />
            </div>
          )}

          {(phase === 'waiting' || phase === 'success') && authUrl && (
            <div className="rounded-2xl bg-white p-4 ring-1 ring-black/5">
              <QRCodeSVG value={authUrl} size={200} level="M" />
            </div>
          )}

          {phase === 'error' && (
            <div className="grid h-[232px] w-[232px] place-items-center rounded-2xl bg-muted">
              <AlertTriangle className="h-6 w-6 text-amber-500" />
            </div>
          )}

          {authUrl && (
            <a
              href={authUrl}
              target="_blank"
              rel="noopener noreferrer"
              className="inline-flex max-w-full items-center gap-1 truncate text-[11px] text-blue-500 hover:underline"
            >
              <ExternalLink className="h-3 w-3 shrink-0" />
              <span className="truncate">{authUrl}</span>
            </a>
          )}

          <div
            className={
              'flex items-center gap-2 text-xs ' +
              (phase === 'success' ?
                'text-emerald-500' :
                phase === 'error' ?
                  'text-red-500' :
                  'text-muted-foreground')
            }
          >
            {phase === 'waiting' && <Loader2 className="h-3.5 w-3.5 animate-spin" />}
            {phase === 'success' && <CheckCircle2 className="h-3.5 w-3.5" />}
            <span className="text-center">{message}</span>
          </div>

          <div className="flex w-full gap-2">
            <Button variant="outline" className="flex-1" onClick={() => onOpenChange(false)}>
              取消
            </Button>
            {phase === 'error' && (
              <Button className="flex-1" onClick={start}>
                重新获取
              </Button>
            )}
          </div>
        </div>
      </DialogContent>
    </Dialog>
  );
}
