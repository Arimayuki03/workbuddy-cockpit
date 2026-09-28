'use client';

import {useCallback, useState} from 'react';
import {GraduationCap, Loader2, Ticket} from 'lucide-react';
import {notify} from '@/lib/toast';
import {schoolApi, errText} from '@/lib/api';
import type {VoucherRow} from '@/lib/types';
import {fmtDateTime} from '@/lib/format';
import {PageHeader} from '@/components/common/layout/PageHeader';
import {CopyButton} from '@/components/ui/copy-button';
import {useT} from '@/lib/i18n/provider';
import {Button} from '@/components/ui/button';
import {QRCodeSVG} from 'qrcode.react';
import {
  Drawer,
  DrawerContent,
  DrawerDescription,
  DrawerHeader,
  DrawerTitle,
} from '@/components/ui/drawer';

export default function ActivityPage() {
  const t = useT();
  /** 券码抽屉开关（券码列表抽屉打开时一次性拉取） */
  const [voucherOpen, setVoucherOpen] = useState(false);
  const [vouchers, setVouchers] = useState<VoucherRow[]>([]);
  const [vouchersBusy, setVouchersBusy] = useState(false);

  /** 打开券码抽屉时拉一次券码列表 */
  const openVouchers = useCallback(async () => {
    setVoucherOpen(true);
    setVouchersBusy(true);
    try {
      const r = await schoolApi.vouchers();
      setVouchers(r.accounts ?? []);
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setVouchersBusy(false);
    }
  }, []);


  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader
        title={t('activity.title')}
        description={t('activity.description')}
        actions={
          <Button
            variant="outline"
            size="sm"
            className="rounded-full"
            onClick={openVouchers}
          >
            <Ticket />
            {t('activity.vouchers')}
          </Button>
        }
      />

      {/* 开学季活动（2026-09-13 ~ 09-24）已结束：任务视图与一键闭环已下线，
          页面只保留历史券码查询。国际版无开学季活动，本来就没有券码可查。 */}
      <div className="flex items-start gap-2.5 rounded-[20px] border border-sky-500/30 bg-sky-500/10 p-4 text-xs">
        <GraduationCap className="mt-0.5 h-4 w-4 shrink-0 text-sky-500" />
        <div className="space-y-1">
          <div className="font-medium">{t('activity.endedTitle')}</div>
          <div className="text-muted-foreground">{t('activity.endedDesc')}</div>
        </div>
      </div>

      <section className="overflow-hidden rounded-[20px] bg-muted">
        <div className="px-4 py-6 text-center text-xs text-muted-foreground">
          {t('activity.endedVoucherHint')}
        </div>
      </section>

      {/* 券码抽屉：逐账号券码 + 二维码（复制给店员核销） */}
      <Drawer open={voucherOpen} onOpenChange={(v) => !v && setVoucherOpen(false)}>
        <DrawerContent>
          <DrawerHeader>
            <DrawerTitle>{t('activity.voucherTitle')}</DrawerTitle>
            <DrawerDescription>{t('activity.voucherDesc')}</DrawerDescription>
          </DrawerHeader>
          <div className="max-h-[60vh] overflow-auto px-4 pb-8">
            {vouchersBusy ? (
              <div className="flex items-center justify-center gap-2 py-10 text-xs text-muted-foreground">
                <Loader2 className="h-4 w-4 animate-spin" />
                {t('common.loading')}
              </div>
            ) : vouchers.length ? (
              <div className="space-y-4">
                {vouchers.map((row) => (
                  <div key={row.uid} className="rounded-2xl bg-muted p-3">
                    <div className="mb-2 flex items-center justify-between gap-2">
                      <span className="truncate text-sm font-medium">{row.nickname || row.uid}</span>
                      {row.error && (
                        <span className="shrink-0 text-[10px] text-red-600 dark:text-red-400">{row.error}</span>
                      )}
                    </div>
                    {row.vouchers?.length ? (
                      <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
                        {row.vouchers.map((v) => (
                          <div key={v.grant_id} className="rounded-xl bg-background/60 p-3">
                            <div className="flex items-start justify-between gap-2">
                              <div className="min-w-0">
                                <div className="truncate text-xs font-medium">{v.prize_name || v.sku_code || '—'}</div>
                                <div className="mt-0.5 break-all font-mono text-[11px] text-muted-foreground" title={v.code}>
                                  {v.code}
                                </div>
                                {v.valid_to && (
                                  <div className="mt-0.5 text-[10px] text-muted-foreground/70">
                                    {t('activity.validTo', {date: v.valid_to})}
                                  </div>
                                )}
                              </div>
                              <div className="shrink-0 rounded-lg bg-white p-1.5 ring-1 ring-black/5">
                                <QRCodeSVG value={v.code} size={72} level="M" />
                              </div>
                            </div>
                            <div className="mt-2 flex items-center justify-between gap-2">
                              <span className="text-[10px] text-muted-foreground">
                                {v.granted_at ? fmtDateTime(v.granted_at) : ''}
                              </span>
                              <CopyButton value={v.code} title={t('activity.copyVoucher')} className="h-6 w-6" />
                            </div>
                          </div>
                        ))}
                      </div>
                    ) : (
                      <div className="py-3 text-center text-[11px] text-muted-foreground">
                        {t('activity.noVouchers')}
                      </div>
                    )}
                  </div>
                ))}
              </div>
            ) : (
              <div className="py-10 text-center text-xs text-muted-foreground">
                {t('activity.noVouchers')}
              </div>
            )}
          </div>
        </DrawerContent>
      </Drawer>
    </div>
  );
}
