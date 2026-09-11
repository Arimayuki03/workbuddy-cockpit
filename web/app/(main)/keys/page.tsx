'use client';

import {useCallback, useEffect, useState} from 'react';
import {KeyRound, Plus, Trash2, Copy, RefreshCw, Ban, CircleCheck, Pencil, RotateCcw} from 'lucide-react';
import {toast} from 'sonner';
import {keyApi, errText} from '@/lib/api';
import type {ApiKey} from '@/lib/types';
import {copyText, fmtDateTime, fmtNumber} from '@/lib/format';
import {PageHeader} from '@/components/common/layout/PageHeader';
import {EmptyState} from '@/components/common/layout/EmptyState';
import {ConfirmDialog} from '@/components/common/layout/ConfirmDialog';
import {useAuth} from '@/lib/auth-context';
import {Button} from '@/components/ui/button';
import {Badge} from '@/components/ui/badge';
import {Input} from '@/components/ui/input';
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
  expiresDays: string;
  maxIps: string;
  ipAllowlist: string;
  models: string;
  quota: string;
}

const emptyForm: FormState = {
  name: '',
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
      toast.error(errText(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  function openCreate() {
    setEditing(null);
    setForm(emptyForm);
    setFormOpen(true);
  }

  function openEdit(k: ApiKey) {
    setEditing(k);
    setForm({
      name: k.name,
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
      toast.error('请填写密钥名称');
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
      if (days > 0) payload.expires_at = Math.floor(Date.now() / 1000) + days * 86400;

      if (editing) {
        await keyApi.update(editing.id, payload as Partial<ApiKey>);
        toast.success('密钥已更新');
      } else {
        const created = await keyApi.create(payload as Partial<ApiKey>);
        toast.success('密钥已创建');
        if (created.key) setIssued(created.key);
      }
      setFormOpen(false);
      load();
    } catch (e) {
      toast.error(errText(e));
    } finally {
      setBusy(false);
    }
  }

  async function toggle(k: ApiKey) {
    try {
      await keyApi.update(k.id, {enabled: !k.enabled});
      toast.success(k.enabled ? '已停用' : '已启用');
      load();
    } catch (e) {
      toast.error(errText(e));
    }
  }

  async function copyKey(text: string) {
    const ok = await copyText(text);
    if (ok) {
      toast.success('已复制到剪贴板');
    } else {
      toast.error('复制失败');
    }
  }

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader
        title="API 密钥"
        description="对外反代网关的分发密钥，支持有效期、IP 白名单、模型白名单与配额"
        actions={
          <>
            <Button variant="outline" size="sm" className="rounded-full" onClick={load} disabled={loading}>
              <RefreshCw className={loading ? 'animate-spin' : ''} />
              刷新
            </Button>
            {isAdmin && (
              <Button size="sm" className="rounded-full" onClick={openCreate}>
                <Plus />
                新建密钥
              </Button>
            )}
          </>
        }
      />

      <section className="overflow-hidden rounded-[20px] bg-muted">
        <Table>
          <TableHeader>
            <TableRow className="border-b border-border/60 hover:bg-transparent">
              <TableHead className="pl-4 text-[11px] text-muted-foreground">名称</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">密钥前缀</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">状态</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">有效期</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">IP / 模型</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">已用 Token</TableHead>
              <TableHead className="text-[11px] text-muted-foreground">最近使用</TableHead>
              {isAdmin && <TableHead className="pr-4 text-right text-[11px] text-muted-foreground">操作</TableHead>}
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
                      <Badge variant="secondary" className="rounded-full text-muted-foreground">已停用</Badge>
                    ) : expired ? (
                      <Badge variant="destructive" className="rounded-full">已过期</Badge>
                    ) : overQuota ? (
                      <Badge variant="destructive" className="rounded-full">超配额</Badge>
                    ) : (
                      <Badge variant="secondary" className="rounded-full text-emerald-600 dark:text-emerald-400">正常</Badge>
                    )}
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {k.expires_at ? fmtDateTime(k.expires_at) : '永不过期'}
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {k.max_ips ? `≤${k.max_ips} IP` : '不限 IP'} /{' '}
                    {k.models?.length ? `${k.models.length} 模型` : '全部模型'}
                  </TableCell>
                  <TableCell className="text-xs tabular-nums">
                    {fmtNumber(k.used_tokens)}
                    {k.quota ? ` / ${fmtNumber(k.quota)}` : ''}
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">{fmtDateTime(k.last_used_at)}</TableCell>
                  {isAdmin && (
                    <TableCell className="pr-4">
                      <div className="flex justify-end gap-1">
                        <Button variant="ghost" size="icon" className="h-7 w-7 rounded-md" title="编辑" onClick={() => openEdit(k)}>
                          <Pencil className="h-3.5 w-3.5" />
                        </Button>
                        <Button
                          variant="ghost"
                          size="icon"
                          className="h-7 w-7 rounded-md"
                          title={k.enabled ? '停用' : '启用'}
                          onClick={() => toggle(k)}
                        >
                          {k.enabled ? <Ban className="h-3.5 w-3.5" /> : <CircleCheck className="h-3.5 w-3.5" />}
                        </Button>
                        <ConfirmDialog
                          title="重置用量？"
                          description={`将把密钥「${k.name}」的已用 Token 归零。`}
                          onConfirm={async () => {
                            await keyApi.resetUsage(k.id);
                            toast.success('已重置');
                            load();
                          }}
                          trigger={
                            <Button variant="ghost" size="icon" className="h-7 w-7 rounded-md" title="重置用量">
                              <RotateCcw className="h-3.5 w-3.5" />
                            </Button>
                          }
                        />
                        <ConfirmDialog
                          title={`删除密钥「${k.name}」？`}
                          description="删除后使用该密钥的调用将立即失效，此操作不可撤销。"
                          confirmText="删除"
                          destructive
                          onConfirm={async () => {
                            await keyApi.remove(k.id);
                            toast.success('已删除');
                            load();
                          }}
                          trigger={
                            <Button variant="ghost" size="icon" className="h-7 w-7 rounded-md text-red-500 hover:text-red-600" title="删除">
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
            title="暂无 API 密钥"
            description="创建一个密钥，即可用 OpenAI SDK 调用本网关"
            className="flex flex-col items-center justify-center py-16 text-center"
          >
            {isAdmin && (
              <Button className="mt-4 rounded-full" onClick={openCreate}>
                <Plus />
                新建密钥
              </Button>
            )}
          </EmptyState>
        )}
      </section>

      {/* 新建 / 编辑 */}
      <Dialog open={formOpen} onOpenChange={setFormOpen}>
        <DialogContent className="max-w-[520px]">
          <DialogHeader>
            <DialogTitle>{editing ? '编辑密钥' : '新建密钥'}</DialogTitle>
            <DialogDescription>
              {editing ? '修改名称、IP 白名单、模型白名单与配额' : '密钥仅在创建时完整展示一次，请妥善保存'}
            </DialogDescription>
          </DialogHeader>
          <DialogBody className="max-h-[min(70vh,560px)]">
            <div className="space-y-4 px-6 pb-2">
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">名称</Label>
                <Input value={form.name} onChange={(e) => setForm({...form, name: e.target.value})} placeholder="例如：客服组" />
              </div>
              {!editing && (
                <div className="space-y-1.5">
                  <Label className="text-[11px] text-muted-foreground">有效期（天，0 = 永不过期）</Label>
                  <Input
                    type="number"
                    min={0}
                    value={form.expiresDays}
                    onChange={(e) => setForm({...form, expiresDays: e.target.value})}
                  />
                </div>
              )}
              <div className="grid grid-cols-2 gap-3">
                <div className="space-y-1.5">
                  <Label className="text-[11px] text-muted-foreground">最大 IP 数（0 = 不限）</Label>
                  <Input
                    type="number"
                    min={0}
                    value={form.maxIps}
                    onChange={(e) => setForm({...form, maxIps: e.target.value})}
                  />
                </div>
                <div className="space-y-1.5">
                  <Label className="text-[11px] text-muted-foreground">配额 Token（0 = 不限）</Label>
                  <Input
                    type="number"
                    min={0}
                    value={form.quota}
                    onChange={(e) => setForm({...form, quota: e.target.value})}
                  />
                </div>
              </div>
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">IP 白名单（每行一个，支持 CIDR，留空 = 不限制）</Label>
                <Textarea
                  rows={3}
                  value={form.ipAllowlist}
                  onChange={(e) => setForm({...form, ipAllowlist: e.target.value})}
                  placeholder={'10.0.0.0/8\n1.2.3.4'}
                />
              </div>
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">模型白名单（逗号分隔，留空 = 全部模型）</Label>
                <Input
                  value={form.models}
                  onChange={(e) => setForm({...form, models: e.target.value})}
                  placeholder="glm-5.2, kimi-k2.7"
                />
              </div>
            </div>
          </DialogBody>
          <DialogFooter>
            <Button variant="outline" className="rounded-full" onClick={() => setFormOpen(false)}>
              取消
            </Button>
            <Button className="rounded-full" onClick={submit} disabled={busy}>
              {editing ? '保存' : '创建'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* 一次性展示新密钥 */}
      <Dialog open={!!issued} onOpenChange={(v) => !v && setIssued(null)}>
        <DialogContent className="max-w-[520px]">
          <DialogHeader>
            <DialogTitle>密钥创建成功</DialogTitle>
            <DialogDescription>请立即复制保存，关闭后将无法再次查看完整密钥</DialogDescription>
          </DialogHeader>
          <div className="space-y-3 px-6 pb-2">
            <div className="flex items-center gap-2 rounded-2xl bg-muted p-3">
              <code className="flex-1 overflow-x-auto whitespace-nowrap font-mono text-xs">{issued}</code>
              <Button variant="ghost" size="icon" className="h-7 w-7 rounded-md" onClick={() => issued && copyKey(issued)}>
                <Copy className="h-3.5 w-3.5" />
              </Button>
            </div>
            <p className="text-[11px] text-muted-foreground">
              Base URL：<code className="font-mono">{typeof window !== 'undefined' ? window.location.origin : ''}/v1</code>
            </p>
          </div>
          <DialogFooter>
            <Button className="rounded-full" onClick={() => setIssued(null)}>
              我已保存
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
