'use client';

/**
 * 设置页「访问令牌」分区（wbt_ 管理面作用域 Token，后端 internal/panel/tokens.go）。
 *
 * **为什么不消费 SettingsProvider**：Provider 承载的是 config/modelMap 这类
 * 「读改存」共享配置状态（saveGroup / reloadAll / 脏检查都围绕配置快照）；
 * 令牌是独立的 CRUD 资源（list / create / delete / disable），既不读配置
 * 快照，也没有「保存分组」语义，塞进 context 只会让 SettingsContextValue
 * 多出一坨与配置无关的字段、reloadAll 也不该顺带刷令牌（顶部「重新加载」
 * 的语义是刷新配置）。因此令牌的 state 与请求完全自治在本组件内。
 *
 * 与「API 密钥」页的区别：密钥（wbk_）发给**下游调模型**的网关凭据；令牌
 * （wbt_）给**脚本 / CI 免登录调管理接口**用（Authorization: Bearer wbt_...，
 * readonly 仅 GET 类，admin 额外放开幂等运维端点）。两者都只在创建时显示
 * 一次明文。token 管理端点仅会话 cookie 可用——api.ts 的 withCredentials
 * 封装天然满足，本组件不做任何额外鉴权处理。
 */
import {useCallback, useRef, useState} from 'react';
import {
  KeyRound,
  Plus,
  Trash2,
  Ban,
  CircleCheck,
  RefreshCw,
  ServerCrash,
} from 'lucide-react';
import {notify} from '@/lib/toast';
import {tokenApi, errText} from '@/lib/api';
import type {PanelToken} from '@/lib/types';
import {fmtAgo, fmtDateTime} from '@/lib/format';
import {useI18n} from '@/lib/i18n/provider';
import {EmptyState} from '@/components/common/layout/EmptyState';
import {TableSkeleton} from '@/components/common/layout/LoadSkeleton';
import {ConfirmDialog} from '@/components/common/layout/ConfirmDialog';
import {Badge} from '@/components/ui/badge';
import {Button} from '@/components/ui/button';
import {CopyButton} from '@/components/ui/copy-button';
import {Input} from '@/components/ui/input';
import {Label} from '@/components/ui/label';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';
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

type TokenScope = PanelToken['scope'];

export function TokensTab() {
  const {t} = useI18n();

  const [tokens, setTokens] = useState<PanelToken[]>([]);
  const [loading, setLoading] = useState(true);
  /**
   * 首载错误态：与「暂无令牌」空态严格区分——取不到 ≠ 没有，渲染成空态
   * 会诱导用户把「还没加载完」当成「一个都没有」。
   */
  const [loadError, setLoadError] = useState<string | null>(null);
  /** 行级操作互斥（停用/恢复），避免快速连点发出乱序请求 */
  const [rowBusy, setRowBusy] = useState<string | null>(null);

  // 创建弹窗
  const [formOpen, setFormOpen] = useState(false);
  const [name, setName] = useState('');
  const [scope, setScope] = useState<TokenScope>('readonly');
  const [busy, setBusy] = useState(false);
  /** 提交锁（同步生效）：disabled 要等重渲染才生效，ref 在当前调用栈立即拦住连点 */
  const submitting = useRef(false);
  /** 一次性明文（创建成功才有）；null = 无 */
  const [issued, setIssued] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const res = await tokenApi.list();
      setTokens(res.tokens ?? []);
      setLoadError(null);
    } catch (e) {
      setLoadError(errText(e));
    } finally {
      setLoading(false);
    }
  }, []);

  const submit = useCallback(async () => {
    if (submitting.current) return;
    if (!name.trim()) {
      notify.err(t('settings.tokens.nameRequired'));
      return;
    }
    submitting.current = true;
    setBusy(true);
    try {
      const created = await tokenApi.create({name: name.trim(), scope});
      setIssued(created.token);
      setFormOpen(false);
      setName('');
      setScope('readonly');
      // 刷新失败不能把这次创建判成失败：写操作已经成功了
      await load();
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setBusy(false);
      submitting.current = false;
    }
  }, [name, scope, load, t]);

  async function toggle(tk: PanelToken) {
    if (rowBusy) return;
    setRowBusy(tk.id);
    try {
      await tokenApi.setDisabled(tk.id, !tk.disabled);
      notify.ok(tk.disabled ? t('settings.tokens.enabled') : t('settings.tokens.disabled'));
      await load();
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setRowBusy(null);
    }
  }

  const canSubmit = !!name.trim() && !busy;

  return (
    <div className="mt-4 flex flex-col gap-4">
      {/* ── 工具条：说明 + 新建入口 ── */}
      <div className="flex flex-wrap items-center justify-between gap-3 rounded-[20px] bg-muted p-4">
        <div className="min-w-0">
          <div className="text-sm font-medium">{t('settings.tokens.title')}</div>
          <div className="mt-1 text-[11px] leading-4 text-muted-foreground">
            {t('settings.tokens.description')}
          </div>
        </div>
        <Button size="sm" className="rounded-full" onClick={() => setFormOpen(true)}>
          <Plus />
          {t('settings.tokens.create')}
        </Button>
      </div>

      {/* ── 首载失败且无数据：错误态（与「暂无令牌」空态严格区分）── */}
      {loadError && !loading && !tokens.length ? (
        <section className="overflow-hidden rounded-[20px] bg-muted">
          <EmptyState
            icon={ServerCrash}
            title={t('settings.tokens.loadError')}
            description={loadError}
            className="flex flex-col items-center justify-center py-16 text-center"
          >
            <Button className="mt-4 rounded-full" onClick={load}>
              <RefreshCw />
              {t('common.refresh')}
            </Button>
          </EmptyState>
        </section>
      ) : loading && !tokens.length ? (
        <TableSkeleton rows={4} />
      ) : (
        <section className="overflow-hidden rounded-[20px] bg-muted">
          <Table>
            <TableHeader>
              <TableRow className="border-b border-border/60 hover:bg-transparent">
                <TableHead className="pl-4 text-[11px] text-muted-foreground">
                  {t('settings.tokens.name')}
                </TableHead>
                <TableHead className="text-[11px] text-muted-foreground">
                  {t('settings.tokens.prefix')}
                </TableHead>
                <TableHead className="text-[11px] text-muted-foreground">
                  {t('settings.tokens.scope')}
                </TableHead>
                <TableHead className="text-[11px] text-muted-foreground">
                  {t('settings.tokens.createdAt')}
                </TableHead>
                <TableHead className="text-[11px] text-muted-foreground">
                  {t('settings.tokens.lastUsed')}
                </TableHead>
                <TableHead className="text-[11px] text-muted-foreground">
                  {t('accounts.colStatus')}
                </TableHead>
                <TableHead className="pr-4 text-right text-[11px] text-muted-foreground">
                  {t('accounts.colActions')}
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {tokens.map((tk) => (
                <TableRow key={tk.id} className="border-b border-border/40">
                  <TableCell className="pl-4 text-sm font-medium">
                    <div className="max-w-[200px] truncate" title={tk.name}>{tk.name}</div>
                  </TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">
                    {tk.prefix}…
                  </TableCell>
                  <TableCell>
                    {tk.scope === 'admin' ? (
                      <Badge className="rounded-full text-[10px]">{t('settings.tokens.scopeAdmin')}</Badge>
                    ) : (
                      // readonly 灰：最小权限默认档，视觉上低调
                      <Badge variant="secondary" className="rounded-full text-muted-foreground text-[10px]">
                        {t('settings.tokens.scopeReadonly')}
                      </Badge>
                    )}
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {fmtDateTime(tk.created_at)}
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {tk.last_used_at ? (
                      fmtAgo(tk.last_used_at)
                    ) : (
                      <span className="text-muted-foreground/70">{t('settings.tokens.neverUsed')}</span>
                    )}
                  </TableCell>
                  <TableCell>
                    {tk.disabled ? (
                      <Badge variant="secondary" className="rounded-full text-muted-foreground">
                        {t('settings.tokens.disabled')}
                      </Badge>
                    ) : (
                      <Badge variant="secondary" className="rounded-full text-emerald-600 dark:text-emerald-400">
                        {t('settings.tokens.enabled')}
                      </Badge>
                    )}
                  </TableCell>
                  <TableCell className="pr-4">
                    <div className="flex justify-end gap-1">
                      <Button
                        variant="ghost"
                        size="icon"
                        className="h-7 w-7 rounded-md"
                        disabled={rowBusy === tk.id}
                        title={tk.disabled ? t('settings.tokens.enable') : t('settings.tokens.disable')}
                        onClick={() => toggle(tk)}
                      >
                        {tk.disabled ? <CircleCheck className="h-3.5 w-3.5" /> : <Ban className="h-3.5 w-3.5" />}
                      </Button>
                      <ConfirmDialog
                        title={t('settings.tokens.confirmDelete')}
                        description={t('settings.tokens.confirmDeleteDesc', {name: tk.name})}
                        confirmText={t('keys.delete')}
                        destructive
                        onConfirm={async () => {
                          try {
                            await tokenApi.remove(tk.id);
                            notify.ok(t('keys.deleted'));
                            await load();
                          } catch (e) {
                            notify.err(errText(e));
                          }
                        }}
                        trigger={
                          <Button
                            variant="ghost"
                            size="icon"
                            className="h-7 w-7 rounded-md text-red-500 hover:text-red-600"
                            title={t('keys.delete')}
                          >
                            <Trash2 className="h-3.5 w-3.5" />
                          </Button>
                        }
                      />
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>

          {/* 「暂无令牌」是一条都没有的断言：取不到时走上面的错误态，不进这里 */}
          {!tokens.length && (
            <EmptyState
              icon={KeyRound}
              title={t('settings.tokens.empty')}
              description={t('settings.tokens.emptyDesc')}
              className="flex flex-col items-center justify-center py-16 text-center"
            >
              <Button className="mt-4 rounded-full" onClick={() => setFormOpen(true)}>
                <Plus />
                {t('settings.tokens.create')}
              </Button>
            </EmptyState>
          )}
        </section>
      )}

      {/* ── 创建弹窗 ── */}
      <Dialog open={formOpen} onOpenChange={setFormOpen}>
        <DialogContent className="max-w-[440px]">
          <DialogHeader>
            <DialogTitle>{t('settings.tokens.create')}</DialogTitle>
            <DialogDescription>{t('settings.tokens.createDesc')}</DialogDescription>
          </DialogHeader>
          <DialogBody>
            <div className="space-y-4 px-6 pb-2">
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">{t('settings.tokens.name')}</Label>
                <Input
                  value={name}
                  maxLength={64}
                  onChange={(e) => setName(e.target.value)}
                  placeholder={t('settings.tokens.namePlaceholder')}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' && canSubmit) void submit();
                  }}
                />
              </div>
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">{t('settings.tokens.scope')}</Label>
                <Select value={scope} onValueChange={(v) => setScope(v as TokenScope)}>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="readonly">{t('settings.tokens.scopeReadonly')}</SelectItem>
                    <SelectItem value="admin">{t('settings.tokens.scopeAdmin')}</SelectItem>
                  </SelectContent>
                </Select>
                <p className="text-[10px] leading-4 text-muted-foreground">
                  {scope === 'admin'
                    ? t('settings.tokens.scopeAdminHint')
                    : t('settings.tokens.scopeReadonlyHint')}
                </p>
              </div>
            </div>
          </DialogBody>
          <DialogFooter>
            <Button variant="outline" className="rounded-full" onClick={() => setFormOpen(false)}>
              {t('common.cancel')}
            </Button>
            <Button className="rounded-full" onClick={submit} disabled={!canSubmit}>
              {t('settings.tokens.create')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* ── 明文一次性弹窗。wbt_ 只存 SHA-256 摘要，关掉后连服务端也拿不回
          完整令牌——警示与单一出口（已保存按钮）都在这里。 ── */}
      <Dialog open={!!issued} onOpenChange={(v) => !v && setIssued(null)}>
        <DialogContent className="max-w-[560px]">
          <DialogHeader>
            <DialogTitle>{t('settings.tokens.createdTitle')}</DialogTitle>
            <DialogDescription>{t('settings.tokens.createdDesc')}</DialogDescription>
          </DialogHeader>
          <DialogBody>
            <div className="space-y-3 px-6 pb-3">
              {/* 大字明文：wbt_ 比网关密钥更值得放大——它是管理凭据，抄错一位就 401。
                  min-w-0 必不可少：flex 项默认 min-width:auto，长令牌会把复制按钮挤出去。 */}
              <div className="flex items-center gap-2 rounded-2xl bg-muted p-3">
                <code className="min-w-0 flex-1 break-all font-mono text-sm font-medium leading-relaxed">
                  {issued}
                </code>
                <CopyButton value={issued || ''} size="sm" showLabel label={t('settings.tokens.copy')} />
              </div>
              <p className="flex items-start gap-1.5 text-xs leading-5 text-amber-600 dark:text-amber-400">
                <span aria-hidden>⚠</span>
                {t('settings.tokens.onceNotice')}
              </p>
            </div>
          </DialogBody>
          <DialogFooter>
            <Button className="rounded-full" onClick={() => setIssued(null)}>
              {t('settings.tokens.savedIt')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
