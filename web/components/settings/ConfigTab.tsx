'use client';

/**
 * 设置页「上游配置」分区（原 TabsContent value="config"，原样搬移，只拆不重构）：
 * 可视化配置热编辑（8 个分组）+ Redis/Upstash 持久化 + 高级模式 JSON 直编。
 */
import {
  Save,
  TriangleAlert,
  ChevronDown,
  RotateCcw,
  PlugZap,
  Loader2,
  RefreshCw,
} from 'lucide-react';
import {notify} from '@/lib/toast';
import {useI18n} from '@/lib/i18n/provider';
import {errText, settingsApi} from '@/lib/api';
import {RichText} from '@/lib/i18n/rich-text';
import {EmptyState} from '@/components/common/layout/EmptyState';
import {Skeleton} from '@/components/ui/skeleton';
import {useAuth} from '@/lib/auth-context';
import {CopyButton} from '@/components/ui/copy-button';
import {Button} from '@/components/ui/button';
import {Badge} from '@/components/ui/badge';
import {Input} from '@/components/ui/input';
import {Label} from '@/components/ui/label';
import {Switch} from '@/components/ui/switch';
import {Textarea} from '@/components/ui/textarea';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';
import {useSettings} from './settings-context';
import {GROUPS, fieldError} from './settings-fields';

export function ConfigTab() {
  const {t, tp} = useI18n();
  const {isAdmin} = useAuth();
  const s = useSettings();
  const {busy, cfgReady, configLoading} = s;

  return (
    <div className="mt-3 space-y-3">
      {s.cfg?.path && (
        <div className="flex items-center gap-2 rounded-[16px] bg-muted/60 px-3.5 py-2 text-[11px] text-muted-foreground">
          <span>{t('settings.configPath')}</span>
          <span className="min-w-0 flex-1 truncate font-mono">{s.cfg.path}</span>
          <CopyButton value={s.cfg.path} title={t('settings.copyAuthDir')} className="h-6 w-6" />
        </div>
      )}

      {/* 可视化设置卡片。首载未拿到配置时整块显示骨架分组占位：
          之前会先用默认值渲染真实开关（def: true 等），配置到达后表单
          集体翻转，看起来像「设置自己变了」。加载结束仍未拿到配置
          （接口失败/不可达）则显示错误态 + 重试，绝不渲染默认值表单。 */}
      {!cfgReady && configLoading ? (
        <>
          {GROUPS.slice(0, 4).map((g) => (
            <div key={g.id} className="rounded-[20px] bg-muted px-3.5 py-3" aria-hidden>
              <Skeleton className="h-4 w-24" />
              <div className="mt-3 grid grid-cols-1 gap-1.5 xl:grid-cols-2">
                {g.fields.slice(0, 2).map((f) => (
                  <div
                    key={f.key}
                    className="flex items-center justify-between gap-3 rounded-2xl bg-background/60 px-3 py-2"
                  >
                    <div className="min-w-0 flex-1 space-y-1.5">
                      <Skeleton className="h-3 w-28" />
                      <Skeleton className="h-2.5 w-44" />
                    </div>
                    <Skeleton className="h-5 w-9 rounded-full" />
                  </div>
                ))}
              </div>
            </div>
          ))}
        </>
      ) : !cfgReady ? (
        <div className="rounded-[20px] bg-muted p-4">
          <EmptyState
            icon={TriangleAlert}
            title={t('settings.cfgLoadFailed')}
            description={t('settings.cfgLoadFailedDesc')}
            className="flex flex-col items-center justify-center py-8 text-center"
          >
            <Button variant="outline" size="sm" className="rounded-full" disabled={busy} onClick={s.reloadAll}>
              <RefreshCw className={busy ? 'animate-spin' : ''} />
              {t('settings.retryLoad')}
            </Button>
          </EmptyState>
        </div>
      ) : (
        GROUPS.map((g) => {
          const dirty = s.isDirty(g.id);
          return (
            <div key={g.id} className="rounded-[20px] bg-muted px-3.5 py-3">
              <div className="mb-2.5 flex flex-wrap items-center justify-between gap-2">
                <div>
                  <div className="text-sm font-medium">{tp(g.title)}</div>
                  {/* 分组说明里带加粗强调（如「只对国内版账号生效」），走 RichText 渲染 */}
                  <RichText className="text-[11px] text-muted-foreground" text={tp(g.desc)} />
                </div>
                {/* 有改动才出现「撤销 / 保存」：没改过时按钮点了只会弹「无改动」，直接隐藏 */}
                {dirty && (
                  <div className="flex items-center gap-2">
                    <Button
                      size="sm"
                      variant="ghost"
                      className="rounded-full text-muted-foreground"
                      disabled={busy}
                      onClick={() => s.resetGroup(g.id)}
                    >
                      <RotateCcw className="h-3.5 w-3.5" />
                      {t('settings.revert')}
                    </Button>
                    <Button
                      size="sm"
                      className="rounded-full"
                      disabled={!isAdmin || busy || !cfgReady}
                      onClick={() => s.saveGroup(g.id)}
                    >
                      <Save className="h-3.5 w-3.5" />
                      {t('common.save')}
                    </Button>
                  </div>
                )}
              </div>

              {/* 宽屏两列：开关与它对应的时刻/数值字段天然成对，行数减半 */}
              <div className="grid grid-cols-1 gap-1.5 xl:grid-cols-2">
                {g.fields.map((f) => {
                  const err = fieldError(f, s.form[g.id][f.key]);
                  const caution = 'caution' in f ? f.caution : undefined;
                  return (
                    <div
                      key={f.key}
                      className="flex items-center justify-between gap-3 rounded-2xl bg-background/60 px-3 py-2"
                    >
                      <div className="min-w-0">
                        <div className="text-xs font-medium">{tp(f.label)}</div>
                        <RichText
                          className="mt-0.5 block text-[11px] leading-4 text-muted-foreground"
                          text={tp(f.desc)}
                        />
                        {f.kind !== 'bool' && err && (
                          <div className="mt-0.5 text-[10px] leading-3 text-destructive">{err}</div>
                        )}
                      </div>

                      {f.kind === 'bool' ? (
                        <Switch
                          checked={!!s.form[g.id][f.key]}
                          disabled={!isAdmin || !cfgReady}
                          onCheckedChange={(v) => s.setField(g.id, f.key, v)}
                        />
                      ) : f.kind === 'num' ? (
                        <div className="flex shrink-0 items-center gap-1.5">
                          <Input
                            type="number"
                            min={f.min}
                            max={f.max}
                            step={f.step ?? 1}
                            value={String(s.form[g.id][f.key] ?? f.def)}
                            disabled={!isAdmin || !cfgReady}
                            onChange={(e) => {
                              // 中间态（"-"、清空）保留原字符串让用户继续输，
                              // 顶掉成默认值会打断输入；非法值由 fieldError
                              // 即时提示、toWire 提交时拒绝。
                              const v = e.target.value;
                              s.setField(
                                g.id,
                                f.key,
                                v === '' ? '' : Number.isFinite(Number(v)) ? Number(v) : v,
                              );
                            }}
                            className={
                              'h-8 w-20 bg-background text-right tabular-nums' +
                              (err ? ' border-destructive' : '')
                            }
                          />
                          {f.unit && (
                            <span className="w-8 text-[11px] text-muted-foreground">{tp(f.unit)}</span>
                          )}
                        </div>
                      ) : f.kind === 'select' ? (
                        <Select
                          value={String(s.form[g.id][f.key] ?? f.def)}
                          disabled={!isAdmin || !cfgReady}
                          onValueChange={(v) => s.setField(g.id, f.key, v)}
                        >
                          <SelectTrigger className="h-8 w-[168px] shrink-0 bg-background text-xs">
                            <SelectValue />
                          </SelectTrigger>
                          <SelectContent>
                            {f.options.map((o) => (
                              <SelectItem key={o.value} value={o.value} className="text-xs">
                                {tp(o.label)}
                              </SelectItem>
                            ))}
                          </SelectContent>
                        </Select>
                      ) : f.kind === 'text' ? (
                        <Input
                          value={String(s.form[g.id][f.key] ?? '')}
                          disabled={!isAdmin || !cfgReady}
                          placeholder={f.placeholder ? tp(f.placeholder) : undefined}
                          onChange={(e) => s.setField(g.id, f.key, e.target.value)}
                          className={
                            'h-8 shrink-0 bg-background text-xs ' +
                            (caution ? 'w-56' : 'w-40') +
                            (err ? ' border-destructive' : '')
                          }
                        />
                      ) : (
                        <Input
                          value={String(s.form[g.id][f.key] ?? '')}
                          disabled={!isAdmin || !cfgReady}
                          placeholder={f.kind === 'hours' ? '9, 21' : '600s'}
                          onChange={(e) => s.setField(g.id, f.key, e.target.value)}
                          className={
                            'h-8 shrink-0 bg-background text-right tabular-nums ' +
                            (f.kind === 'hours' ? 'w-32' : 'w-24') +
                            (err ? ' border-destructive' : '')
                          }
                        />
                      )}
                    </div>
                  );
                })}
              </div>

              {/* 危险项警示：填错会导致网关启动失败，单独占一行说明 */}
              {g.fields.some((f) => 'caution' in f && f.caution) && (
                <div className="mt-1.5 space-y-1">
                  {g.fields.map((f) =>
                    'caution' in f && f.caution ? (
                      <div
                        key={f.key}
                        className="flex items-start gap-1.5 rounded-xl bg-amber-500/10 px-3 py-2 text-[11px] leading-4 text-amber-600 dark:text-amber-400"
                      >
                        <TriangleAlert className="mt-0.5 h-3 w-3 shrink-0" />
                        <RichText text={tp(f.caution)} />
                      </div>
                    ) : null,
                  )}
                </div>
              )}

              {!isAdmin && (
                <p className="mt-2 text-[11px] text-muted-foreground">{t('settings.readonlyNote')}</p>
              )}
            </div>
          );
        })
      )}

      {/* Redis / Upstash 持久化 */}
      <div className="rounded-[20px] bg-muted p-4">
        <div className="mb-1 flex flex-wrap items-center justify-between gap-2">
          <div>
            <div className="flex items-center gap-2 text-sm font-medium">
              {t('settings.upstashTitle')}
              {s.upstashForm.url ? (
                <Badge variant="secondary" className="rounded-full text-emerald-600 dark:text-emerald-400">
                  {t('settings.upstashConfigured')}
                </Badge>
              ) : (
                <Badge variant="secondary" className="rounded-full text-muted-foreground">
                  {t('settings.upstashNotConfigured')}
                </Badge>
              )}
            </div>
            <div className="mt-0.5 text-[11px] leading-4 text-muted-foreground">
              {s.upstashForm.url
                ? t('settings.upstashDescOn')
                : t('settings.upstashDescOff')}
            </div>
          </div>
          <div className="flex items-center gap-2">
            <Button
              size="sm"
              variant="outline"
              className="rounded-full"
              disabled={!isAdmin || s.upstashBusy}
              onClick={async () => {
                s.setUpstashBusy(true);
                try {
                  const r = await settingsApi.testUpstash(s.upstashForm.url, s.upstashForm.token || undefined);
                  (r.ok ? notify.ok : notify.err)(r.message, r.ok ? t('settings.upstashOk') : undefined);
                } catch (e) {
                  notify.err(errText(e));
                } finally {
                  s.setUpstashBusy(false);
                }
              }}
            >
              {s.upstashBusy ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <PlugZap className="h-3.5 w-3.5" />}
              {t('settings.testConnection')}
            </Button>
            {s.upstashDirty && (
              <Button
                size="sm"
                className="rounded-full"
                disabled={!isAdmin || s.upstashBusy || !cfgReady}
                onClick={s.saveUpstash}
              >
                <Save className="h-3.5 w-3.5" />
                {t('common.save')}
              </Button>
            )}
          </div>
        </div>

        <div className="mt-3 grid grid-cols-1 gap-3 sm:grid-cols-2">
          <div className="space-y-1.5">
            <Label className="text-[11px] text-muted-foreground">{t('settings.upstashUrl')}</Label>
            <Input
              value={s.upstashForm.url}
              disabled={!isAdmin || !cfgReady}
              onChange={(e) => s.setUpstashForm({...s.upstashForm, url: e.target.value})}
              placeholder="https://xxx-12345.upstash.io"
              className="bg-background font-mono text-xs"
            />
            <div className="text-[11px] text-muted-foreground">
              {t('settings.upstashUrlHint')}
            </div>
          </div>
          <div className="space-y-1.5">
            <Label className="text-[11px] text-muted-foreground">Upstash Token</Label>
            <Input
              type="password"
              value={s.upstashForm.token}
              disabled={!isAdmin || !cfgReady}
              onChange={(e) => s.setUpstashForm({...s.upstashForm, token: e.target.value})}
              placeholder={t('settings.upstashTokenPlaceholder')}
              className="bg-background font-mono text-xs"
            />
            <div className="text-[11px] text-muted-foreground">
              {t('settings.upstashTokenHint')}
            </div>
          </div>
        </div>

        <div className="mt-2 text-[11px] leading-4 text-muted-foreground">
          {t('settings.upstashFooter')}
        </div>
      </div>

      {/* 高级模式：直接编辑整个配置 JSON */}
      <div className="rounded-[20px] bg-muted p-4">
        <button
          type="button"
          onClick={() => s.setAdvanced(!s.advanced)}
          className="flex w-full items-center justify-between gap-2 text-left"
        >
          <div>
            <div className="text-sm font-medium">{t('settings.advanced')}</div>
            <div className="text-[11px] text-muted-foreground">
              {t('settings.advancedDesc')}
            </div>
          </div>
          <ChevronDown
            className={'h-4 w-4 shrink-0 text-muted-foreground transition-transform ' + (s.advanced ? 'rotate-180' : '')}
          />
        </button>

        {s.advanced && (
          <div className="mt-4 space-y-2">
            <div className="flex items-center justify-between">
              <div className="font-mono text-[11px] text-muted-foreground">config.json</div>
              {s.rawDirty && (
                <Button
                  size="sm"
                  className="h-7 rounded-full text-[11px]"
                  disabled={!isAdmin || busy || !cfgReady}
                  onClick={() => s.saveJson(s.rawText)}
                >
                  {t('common.save')}
                </Button>
              )}
            </div>
            <Textarea
              rows={16}
              spellCheck={false}
              disabled={!isAdmin || !cfgReady}
              value={cfgReady ? s.rawText : ''}
              placeholder={cfgReady ? undefined : t('settings.cfgNotLoaded')}
              onChange={(e) => s.setRawText(e.target.value)}
              className="bg-background font-mono text-xs"
            />
            <p className="text-[11px] leading-4 text-muted-foreground">
              {t('settings.advancedNote')}
            </p>
          </div>
        )}
      </div>
    </div>
  );
}
