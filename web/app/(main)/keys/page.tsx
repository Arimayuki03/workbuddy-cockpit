'use client';

import {useCallback, useEffect, useState} from 'react';
import {KeyRound, Plus, Trash2, Ban, CircleCheck, Pencil, RotateCcw} from 'lucide-react';
import {useHeartbeat} from '@/lib/use-heartbeat';
import {notify} from '@/lib/toast';
import {keyApi, errText} from '@/lib/api';
import type {ApiKey} from '@/lib/types';
import {fmtDateTime, fmtNumber} from '@/lib/format';
import {PageHeader} from '@/components/common/layout/PageHeader';
import {EmptyState} from '@/components/common/layout/EmptyState';
import {ConfirmDialog} from '@/components/common/layout/ConfirmDialog';
import {useAuth} from '@/lib/auth-context';
import {Button} from '@/components/ui/button';
import {CopyButton} from '@/components/ui/copy-button';
import {RichText} from '@/lib/i18n/rich-text';
import {useT} from '@/lib/i18n/provider';
import {Badge} from '@/components/ui/badge';
import {Input} from '@/components/ui/input';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';
import {Label} from '@/components/ui/label';
import {Textarea} from '@/components/ui/textarea';
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/animate-ui/radix/dialog';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';

interface FormState {
  name: string;
  /** 新建：0 = 永不过期；编辑：0 = 保持当前有效期不变 */
  expiresDays: string;
  /** 编辑时的有效期操作：keep 保持不变 / days 从现在起 N 天 / never 永不过期 */
  expiryMode: 'keep' | 'days' | 'never';
  maxIps: string;
  ipAllowlist: string;
  models: string;
  quota: string;
}

const emptyForm: FormState = {
  name: '',
  expiryMode: 'keep',
  expiresDays: '0',
  maxIps: '0',
  ipAllowlist: '',
  models: '',
  quota: '0',
};

function toLines(v: string): string[] {
  return v
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter(Boolean);
}

export default function KeysPage() {
  const t = useT();
  const {isAdmin} = useAuth();
  const [keys, setKeys] = useState<ApiKey[]>([]);
  const [loading, setLoading] = useState(true);
  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<ApiKey | null>(null);
  const [form, setForm] = useState<FormState>(emptyForm);
  const [busy, setBusy] = useState(false);
  const [issued, setIssued] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      setKeys(await keyApi.list());
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  // 密钥状态可能被下游调用改变（配额用尽、过期），心跳刷新保持同步
  useHeartbeat(load, 60000);

  function openCreate() {
    setEditing(null);
    setForm(emptyForm);
    setFormOpen(true);
  }

  function openEdit(k: ApiKey) {
    setEditing(k);
    setForm({
      name: k.name,
      // 默认不改动有效期；要续期/取消过期需显式选择
      expiryMode: 'keep',
      expiresDays: '0',
      maxIps: String(k.max_ips || 0),
      ipAllowlist: (k.ip_allowlist || []).join('\n'),
      models: (k.models || []).join(', '),
      quota: String(k.quota ?? 0),
    });
    setFormOpen(true);
  }

  async function submit() {
    if (!form.name.trim()) {
      notify.err(t('keys.nameRequired'));
      return;
    }
    setBusy(true);
    try {
      const days = Number(form.expiresDays) || 0;
      const payload: Record<string, unknown> = {
        name: form.name.trim(),
        max_ips: Number(form.maxIps) || 0,
        ip_allowlist: toLines(form.ipAllowlist),
        models: toLines(form.models),
        quota: Number(form.quota) || 0,
      };

      // 新建：填了天数才设过期（0 = 永不过期，不下发 expires_at）
      // 编辑：按显式选择处理，避免「打开就保存」把有效期重置
      if (!editing) {
        if (days > 0) payload.expires_at = Math.floor(Date.now() / 1000) + days * 86400;
      } else if (form.expiryMode === 'days') {
        if (days <= 0) {
          notify.err(t('keys.daysRequired'));
          setBusy(false);
          return;
        }
        payload.expires_at = Math.floor(Date.now() / 1000) + days * 86400;
      } else if (form.expiryMode === 'never') {
        // 后端以 null 表示「无过期时间」
        payload.expires_at = null;
      }

      if (editing) {
        await keyApi.update(editing.id, payload as Partial<ApiKey>);
        notify.ok(t('keys.keyUpdated'));
      } else {
        const created = await keyApi.create(payload as Partial<ApiKey>);
        notify.ok(t('keys.keyCreated'));
        if (created.key) setIssued(created.key);
      }
      setFormOpen(false);
      load();
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setBusy(false);
    }
  }

  async function toggle(k: ApiKey) {
    try {
      await keyApi.update(k.id, {enabled: !k.enabled});
      notify.ok(k.enabled ? t('keys.disabled') : t('keys.enabled'));
      load();
    } catch (e) {
      notify.err(errText(e));
    }
  }

  // 下游接入地址：客户端才能拿到当前 origin，静态导出阶段为空
  const baseUrl = typeof window !== 'undefined' ? window.location.origin : '';

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader
        title={t('keys.title')}
        description={t('keys.description')}
        actions={
          <>
            {isAdmin && (
              <Button size="sm" className="rounded-full" onClick={openCreate}>
                <Plus />
                {t('keys.newKey')}
              </Button>
            )}
          </>
        }
      />

      <section className="overflow-hidden rounded-[20px] bg-muted">
        <Table>
          <TableHeader>
            <TableRow className="border-b border-border/60 hover:bg-transparent">
              <TableHead className="pl-4 text-[11px] text-muted-foreground">{t('metric.name')}</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">{t('keys.colPrefix')}</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">{t('accounts.colStatus')}</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">{t('keys.expiry')}</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">{t('keys.colIpModels')}</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">{t('keys.colUsedTokens')}</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">{t('keys.colLastUsed')}</TableHead>
              {isAdmin && <TableHead className="pr-4 text-right text-[11px] text-muted-foreground">{t('accounts.colActions')}</TableHead>}
            </TableRow>
          </TableHeader>
          <TableBody>
            {keys.map((k) => {
              const expired = !!k.expires_at && k.expires_at * 1000 < Date.now();
              const overQuota = !!k.quota && k.used_tokens >= k.quota;
              return (
                <TableRow key={k.id} className="border-b border-border/40">
                  <TableCell className="pl-4 text-sm font-medium">{k.name}</TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">{k.prefix}…</TableCell>
                  <TableCell>
                    {!k.enabled ? (
                      <Badge variant="secondary" className="rounded-full text-muted-foreground">{t('keys.badgeDisabled')}</Badge>
                    ) : expired ? (
                      <Badge variant="destructive" className="rounded-full">{t('keys.badgeExpired')}</Badge>
                    ) : overQuota ? (
                      <Badge variant="destructive" className="rounded-full">{t('keys.badgeOverQuota')}</Badge>
                    ) : (
                      <Badge variant="secondary" className="rounded-full text-emerald-600 dark:text-emerald-400">{t('keys.badgeOk')}</Badge>
                    )}
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {k.expires_at ? fmtDateTime(k.expires_at) : t('keys.neverExpires')}
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {k.max_ips ? t('keys.ipLimit', {n: k.max_ips}) : t('keys.ipUnlimited')} /{' '}
                    {k.models?.length
                      ? t('keys.modelsCount', {count: k.models.length, n: k.models.length})
                      : t('keys.modelsAll')}
                  </TableCell>
                  <TableCell className="text-xs tabular-nums">
                    {(() => {
                      const ratio = k.quota ? k.used_tokens / k.quota : 0;
                      const tone = !k.quota
                        ? 'text-muted-foreground'
                        : ratio >= 1
                          ? 'text-red-600 dark:text-red-400 font-medium'
                          : ratio >= 0.8
                            ? 'text-amber-600 dark:text-amber-400 font-medium'
                            : 'text-foreground';
                      return (
                        <span className={tone}>
                          {fmtNumber(k.used_tokens)}
                          {k.quota ? ` / ${fmtNumber(k.quota)}` : ''}
                        </span>
                      );
                    })()}
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {k.last_used_at ? (
                      fmtDateTime(k.last_used_at)
                    ) : (
                      <span className="text-muted-foreground/70">{t('keys.neverUsed')}</span>
                    )}
                  </TableCell>
                  {isAdmin && (
                    <TableCell className="pr-4">
                      <div className="flex justify-end gap-1">
                        <Button variant="ghost" size="icon" className="h-7 w-7 rounded-md" title={t('keys.edit')} onClick={() => openEdit(k)}>
                          <Pencil className="h-3.5 w-3.5" />
                        </Button>
                        <Button
                          variant="ghost"
                          size="icon"
                          className="h-7 w-7 rounded-md"
                          title={k.enabled ? t('keys.disable') : t('keys.enable')}
                          onClick={() => toggle(k)}
                        >
                          {k.enabled ? <Ban className="h-3.5 w-3.5" /> : <CircleCheck className="h-3.5 w-3.5" />}
                        </Button>
                        <ConfirmDialog
                          title={t('keys.resetUsageTitle')}
                          description={t('keys.resetUsageDesc', {name: k.name})}
                          onConfirm={async () => {
                            await keyApi.resetUsage(k.id);
                            notify.ok(t('keys.resetDone'));
                            load();
                          }}
                          trigger={
                            <Button variant="ghost" size="icon" className="h-7 w-7 rounded-md" title={t('keys.resetUsage')}>
                              <RotateCcw className="h-3.5 w-3.5" />
                            </Button>
                          }
                        />
                        <ConfirmDialog
                          title={t('keys.deleteTitle', {name: k.name})}
                          description={t('keys.deleteDesc')}
                          confirmText={t('keys.delete')}
                          destructive
                          onConfirm={async () => {
                            await keyApi.remove(k.id);
                            notify.ok(t('keys.deleted'));
                            load();
                          }}
                          trigger={
                            <Button variant="ghost" size="icon" className="h-7 w-7 rounded-md text-red-500 hover:text-red-600" title={t('keys.delete')}>
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

        {!keys.length && !loading && (
          <EmptyState
            icon={KeyRound}
            title={t('keys.emptyTitle')}
            description={t('keys.emptyDesc')}
            className="flex flex-col items-center justify-center py-16 text-center"
          >
            {isAdmin && (
              <Button className="mt-4 rounded-full" onClick={openCreate}>
                <Plus />
                {t('keys.newKey')}
              </Button>
            )}
          </EmptyState>
        )}
      </section>

      {/* 新建 / 编辑 */}
      <Dialog open={formOpen} onOpenChange={setFormOpen}>
        <DialogContent className="max-w-[520px]">
          <DialogHeader>
            <DialogTitle>{editing ? t('keys.editTitle') : t('keys.createTitle')}</DialogTitle>
            <DialogDescription>
              {editing ? t('keys.editDesc') : t('keys.createDesc')}
            </DialogDescription>
          </DialogHeader>
          <DialogBody className="max-h-[min(70vh,560px)]">
            <div className="space-y-4 px-6 pb-2">
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">{t('keys.name')}</Label>
                <Input value={form.name} onChange={(e) => setForm({...form, name: e.target.value})} placeholder={t('keys.namePlaceholder')} />
              </div>
              {!editing ? (
                <div className="space-y-1.5">
                  <Label className="text-[11px] text-muted-foreground">{t('keys.expiresInDays')}</Label>
                  <Input
                    type="number"
                    min={0}
                    value={form.expiresDays}
                    onChange={(e) => setForm({...form, expiresDays: e.target.value})}
                  />
                </div>
              ) : (
                <div className="space-y-1.5">
                  <Label className="text-[11px] text-muted-foreground">
                    {t('keys.expiry')}
                    {editing.expires_at
                      ? t('keys.expiryCurrent', {at: fmtDateTime(editing.expires_at)})
                      : t('keys.expiryNever')}
                  </Label>
                  <div className="flex items-center gap-2">
                    <Select
                      value={form.expiryMode}
                      onValueChange={(v) => setForm({...form, expiryMode: v as FormState['expiryMode']})}
                    >
                      <SelectTrigger className="flex-1">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="keep">{t('keys.expiryKeep')}</SelectItem>
                        <SelectItem value="days">{t('keys.expiryFromNow')}</SelectItem>
                        <SelectItem value="never">{t('keys.expiryNeverOption')}</SelectItem>
                      </SelectContent>
                    </Select>
                    {form.expiryMode === 'days' && (
                      <Input
                        type="number"
                        min={1}
                        className="w-24"
                        placeholder={t('keys.daysPlaceholder')}
                        value={form.expiresDays}
                        onChange={(e) => setForm({...form, expiresDays: e.target.value})}
                      />
                    )}
                  </div>
                </div>
              )}
              <div className="grid grid-cols-2 gap-3">
                <div className="space-y-1.5">
                  <Label className="text-[11px] text-muted-foreground">{t('keys.maxIps')}</Label>
                  <Input
                    type="number"
                    min={0}
                    value={form.maxIps}
                    onChange={(e) => setForm({...form, maxIps: e.target.value})}
                  />
                </div>
                <div className="space-y-1.5">
                  <Label className="text-[11px] text-muted-foreground">{t('keys.quotaTokens')}</Label>
                  <Input
                    type="number"
                    min={0}
                    value={form.quota}
                    onChange={(e) => setForm({...form, quota: e.target.value})}
                  />
                </div>
              </div>
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">{t('keys.ipWhitelist')}</Label>
                <Textarea
                  rows={3}
                  value={form.ipAllowlist}
                  onChange={(e) => setForm({...form, ipAllowlist: e.target.value})}
                  placeholder={'10.0.0.0/8\n1.2.3.4'}
                />
              </div>
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">{t('keys.modelWhitelist')}</Label>
                <Input
                  value={form.models}
                  onChange={(e) => setForm({...form, models: e.target.value})}
                  placeholder="glm-5.2, global:gpt-5.4"
                />
                {/* 版本隔离就是靠这个白名单：模型名带 global: 前缀的走国际版账号池，
                    不带前缀的走国内版。所以要「一把密钥只能用国际版」，就只列
                    global: 开头的模型；想两版都能用，就分别列出各自要用的模型。
                    这不是额外的功能开关，而是上游路由协议的直接体现。 */}
                <p className="text-[10px] leading-4 text-muted-foreground">
                  {/* 反引号包住的模型名由 RichText 渲染成等宽字体 */}
                  <RichText text={t('keys.modelPrefixNote')} />
                </p>
              </div>
            </div>
          </DialogBody>
          <DialogFooter>
            <Button variant="outline" className="rounded-full" onClick={() => setFormOpen(false)}>
              {t('common.cancel')}
            </Button>
            <Button className="rounded-full" onClick={submit} disabled={busy}>
              {editing ? t('common.save') : t('keys.create')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* 一次性展示新密钥 */}
      <Dialog open={!!issued} onOpenChange={(v) => !v && setIssued(null)}>
        <DialogContent className="max-w-[520px]">
          <DialogHeader>
            <DialogTitle>{t('keys.createdTitle')}</DialogTitle>
            <DialogDescription>{t('keys.createdDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3 px-6 pb-2">
            {/* min-w-0 必不可少：flex 项默认 min-width:auto，长密钥会把
                复制按钮挤出去（移动端就点不到了） */}
            <div className="flex items-center gap-2 rounded-2xl bg-muted p-3">
              <code className="min-w-0 flex-1 break-all font-mono text-xs">{issued}</code>
              <CopyButton value={issued || ''} size="sm" showLabel label={t('keys.copyKey')} />
            </div>
            <div className="flex items-center gap-2">
              <span className="text-[11px] text-muted-foreground">Base URL</span>
              <code className="min-w-0 flex-1 break-all font-mono text-[11px]">{baseUrl}/v1</code>
              <CopyButton value={`${baseUrl}/v1`} title={t('keys.copyBaseUrl')} />
            </div>
          </div>
          <DialogFooter>
            <Button className="rounded-full" onClick={() => setIssued(null)}>
              {t('keys.savedIt')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
