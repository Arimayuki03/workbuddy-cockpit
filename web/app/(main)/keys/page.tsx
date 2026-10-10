'use client';

import {useCallback, useEffect, useMemo, useRef, useState} from 'react';
import {
  KeyRound,
  Plus,
  Trash2,
  Ban,
  CircleCheck,
  Pencil,
  RotateCcw,
  Wifi,
  RefreshCw,
  ServerCrash,
} from 'lucide-react';
import {useHeartbeat} from '@/lib/use-heartbeat';
import {useCachedAsync} from '@/lib/data-cache';
import {notify} from '@/lib/toast';
import {keyApi, errText} from '@/lib/api';
import type {ApiKey} from '@/lib/types';
import {fmtDateTime, fmtNumber, fmtRemain} from '@/lib/format';
import {PageHeader} from '@/components/common/layout/PageHeader';
import {EmptyState} from '@/components/common/layout/EmptyState';
import {TableSkeleton} from '@/components/common/layout/LoadSkeleton';
import {ConfirmDialog} from '@/components/common/layout/ConfirmDialog';
import {useAuth} from '@/lib/auth-context';
import {useRealm, type Realm} from '@/lib/realm-context';
import {Button} from '@/components/ui/button';
import {CopyButton} from '@/components/ui/copy-button';
import {RichText} from '@/lib/i18n/rich-text';
import {useT} from '@/lib/i18n/provider';
import {Badge} from '@/components/ui/badge';
import {Input} from '@/components/ui/input';
import {Progress} from '@/components/ui/progress';
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

/* ─────────────────────────────────────────────────────────
 * 导出片段格式：**前端本地渲染**，不依赖后端。
 *
 * 关键口径——Base URL 的处理：OpenAI 兼容客户端（openai-sdk / codex / curl）
 * 的 base_url 用 `${origin}/v1`；而 Claude 系客户端（Claude Code settings /
 * env / cc-switch）的 ANTHROPIC_BASE_URL **必须剥掉末尾 /v1**——SDK 自己拼
 * /v1/messages，带着 /v1 会请求到 /v1/v1/messages（最常见的接入 404）。
 * buildSnippet 按格式分流并在此口径上分流，注释写死，防止后人改错。
 * ───────────────────────────────────────────────────────── */
type ExportFormat =
  | 'claude_settings'
  | 'claude_env'
  | 'codex_toml'
  | 'cc_switch'
  | 'openai_python'
  | 'openai_node'
  | 'curl';

const EXPORT_FORMATS: {value: ExportFormat; labelKey: string}[] = [
  {value: 'claude_settings', labelKey: 'keys.exportFmtClaudeSettings'},
  {value: 'claude_env', labelKey: 'keys.exportFmtClaudeEnv'},
  {value: 'codex_toml', labelKey: 'keys.exportFmtCodexToml'},
  {value: 'cc_switch', labelKey: 'keys.exportFmtCcSwitch'},
  {value: 'openai_python', labelKey: 'keys.exportFmtOpenAiPython'},
  {value: 'openai_node', labelKey: 'keys.exportFmtOpenAiNode'},
  {value: 'curl', labelKey: 'keys.exportFmtCurl'},
];

/** JSON 值序列化（引号转义交给 JSON.stringify 本身） */
function jv(v: string): string {
  return JSON.stringify(v);
}

/**
 * 生成导出片段。
 * @param openaiBase OpenAI 兼容口径的 Base URL（带 /v1）
 * @param claudeBase Claude 系口径的 Base URL（**不带** /v1，见函数头注释）
 */
function buildSnippet(fmt: ExportFormat, apiKey: string, openaiBase: string, claudeBase: string): string {
  switch (fmt) {
    case 'claude_settings':
      // Claude Code 的 ~/.claude/settings.json：env 段注入。
      // ANTHROPIC_BASE_URL 不带 /v1（SDK 自动拼接）。
      return JSON.stringify(
        {
          env: {
            ANTHROPIC_AUTH_TOKEN: apiKey,
            ANTHROPIC_BASE_URL: claudeBase,
          },
        },
        null,
        2,
      );
    case 'claude_env':
      // 启动 Claude Code 前 export 的 shell 片段
      return [
        `export ANTHROPIC_AUTH_TOKEN=${apiKey}`,
        // 同上：Claude 系 base url 不带 /v1
        `export ANTHROPIC_BASE_URL=${claudeBase}`,
      ].join('\n');
    case 'codex_toml':
      // OpenAI Codex CLI 的 ~/.codex/config.toml
      return [
        'model_provider = "workbuddy"',
        '',
        '[model_providers.workbuddy]',
        `base_url = ${jv(openaiBase)}`, // OpenAI 兼容口径：带 /v1
        'env_key = "WORKBUDDY_API_KEY"',
        'wire_api = "chat"',
      ].join('\n');
    case 'cc_switch':
      // cc-switch 供应商导入（JSON）
      return JSON.stringify(
        {
          name: 'workbuddy2api',
          claude: {
            ANTHROPIC_AUTH_TOKEN: apiKey,
            // Claude 系：剥掉 /v1
            ANTHROPIC_BASE_URL: claudeBase,
          },
        },
        null,
        2,
      );
    case 'openai_python':
      return [
        'from openai import OpenAI',
        '',
        'client = OpenAI(',
        `    api_key=${jv(apiKey)},`,
        `    base_url=${jv(openaiBase)},  # OpenAI 兼容口径：带 /v1`,
        ')',
      ].join('\n');
    case 'openai_node':
      return [
        "import OpenAI from 'openai';",
        '',
        'const client = new OpenAI({',
        `  apiKey: ${jv(apiKey)},`,
        `  baseURL: ${jv(openaiBase)}, // OpenAI 兼容口径：带 /v1`,
        '});',
      ].join('\n');
    case 'curl':
      return [
        `curl ${jv(`${openaiBase}/chat/completions`)} \\`,
        '  -H "Content-Type: application/json" \\',
        `  -H "Authorization: Bearer ${apiKey}" \\`,
        '  -d \'{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}]}\'',
      ].join('\n');
  }
}

interface FormState {
  name: string;
  /** 'never' = 永不过期；'date' = 自定义过期日期（当天 23:59:59） */
  expiryMode: 'never' | 'date';
  /** 自定义过期日期（yyyy-MM-dd） */
  expiryDate: string;
  maxIps: string;
  ipWhitelist: string;
  models: string;
  tokenQuota: string;
  creditQuota: string;
  rateLimit: string;
  /** 'cn' | 'global' | ''（不限） */
  realm: Realm | '';
}

function emptyForm(realm: Realm): FormState {
  return {
    name: '',
    expiryMode: 'never',
    expiryDate: '',
    maxIps: '0',
    ipWhitelist: '',
    models: '',
    tokenQuota: '0',
    creditQuota: '0',
    rateLimit: '0',
    // 新建默认跟随当前所在版本（在哪个版本界面里建，就是哪个版本的密钥）
    realm,
  };
}

/** 多行/逗号分隔文本 → 去重后的非空数组 */
function toLines(v: string): string[] {
  const seen = new Set<string>();
  for (const raw of v.split(/[\n,]/)) {
    const s = raw.trim();
    if (s) seen.add(s);
  }
  return [...seen];
}

/** yyyy-MM-dd → 当天 23:59:59 的 Unix 秒；非法输入返回 0 */
function dateToUnixSec(date: string): number {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(date)) return 0;
  const at = new Date(`${date}T23:59:59`).getTime();
  return Number.isFinite(at) ? Math.floor(at / 1000) : 0;
}

/** Date → 本地时区的 yyyy-MM-dd（编辑回填用）。不能用 toISOString()：那是
 *  UTC，负偏移时区会把本地当天截回前一天；而提交（dateToUnixSec）按本地
 *  时区的 T23:59:59 换算——两边口径不一致时，用户没改日期直接保存就会漂移。 */
function dateToLocalYMD(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

export default function KeysPage() {
  const t = useT();
  const {isAdmin} = useAuth();
  const {realm} = useRealm();

  /**
   * 首载错误态：与 dashboard 同一模式——捕获挂进 fetcher。
   * useCachedAsync 内部的挂载刷新会先占住请求锁并把失败静默吞掉，
   * 页面层对 refresh 的 .catch 收不到 rejection。取不到密钥 ≠ 没有密钥，
   * 不能渲染成「暂无密钥」诱导用户重建（密钥是凭据，重建就是重复分发）。
   */
  const [loadError, setLoadError] = useState<string | null>(null);
  const keysCache = useCachedAsync(
    'keys',
    () =>
      keyApi.list().catch((e) => {
        setLoadError((prev) => prev ?? errText(e));
        throw e;
      }),
  );
  const keys: ApiKey[] = keysCache.data?.keys ?? [];
  const loading = keysCache.loading;

  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<ApiKey | null>(null);
  const [form, setForm] = useState<FormState>(() => emptyForm('cn'));
  const [busy, setBusy] = useState(false);
  /** 一次性明文（创建成功才有）；null = 无 */
  const [issued, setIssued] = useState<string | null>(null);
  /** 导出片段目标格式 + Base URL 输入值（挂载后取 window.location.origin） */
  const [exportFmt, setExportFmt] = useState<ExportFormat>('claude_settings');
  const [baseUrlDraft, setBaseUrlDraft] = useState('');
  /** 查看该密钥绑定过的 IP 弹窗 */
  const [ipsFor, setIpsFor] = useState<ApiKey | null>(null);
  const [ips, setIps] = useState<{ip: string; first_seen: number}[] | null>(null);
  const [ipsLoading, setIpsLoading] = useState(false);

  /**
   * 提交锁（同步生效）：setBusy 要等重渲染才反映到 disabled，
   * 快速连点的第二次提交可能在那之前进来——用 ref 在当前调用栈立即生效。
   */
  const submitting = useRef(false);

  // 客户端挂载后才有 origin：静态导出阶段 SSR 为空
  useEffect(() => {
    setBaseUrlDraft(window.location.origin);
  }, []);

  // 密钥状态可能被调用侧改变（配额用尽、过期），心跳刷新保持同步。
  // useCachedAsync 的 refresh 会 rethrow（fetcher 失败原样抛给调用方），
  // 心跳 tick 不捕获会产生 unhandled rejection——这里自行 catch 静默。
  useHeartbeat(() => {
    keysCache.refresh().catch(() => {/* 轮询失败静默，下轮重试 */});
  }, 60000);

  const retryLoad = useCallback(() => {
    setLoadError(null);
    keysCache.refresh().catch((e) => {
      setLoadError(errText(e));
    });
  }, [keysCache]);

  const openCreate = useCallback(() => {
    setEditing(null);
    // 新建默认跟随当前所在版本
    setForm(emptyForm(realm));
    setFormOpen(true);
  }, [realm]);

  const openEdit = useCallback((k: ApiKey) => {
    setEditing(k);
    setForm({
      name: k.name,
      expiryMode: k.expires_at ? 'date' : 'never',
      expiryDate: k.expires_at ? dateToLocalYMD(new Date(k.expires_at * 1000)) : '',
      maxIps: String(k.max_ips ?? 0),
      ipWhitelist: (k.ip_whitelist ?? []).join('\n'),
      models: (k.model_whitelist ?? []).join('\n'),
      tokenQuota: String(k.token_quota ?? 0),
      creditQuota: String(k.credit_quota ?? 0),
      rateLimit: String(k.rate_limit ?? 0),
      realm: (k.realm as FormState['realm']) ?? '',
    });
    setFormOpen(true);
  }, []);

  const openIps = useCallback(async (k: ApiKey) => {
    setIpsFor(k);
    setIps(null);
    setIpsLoading(true);
    try {
      const res = await keyApi.ips(k.id);
      setIps(res.ips ?? []);
    } catch (e) {
      notify.err(errText(e));
      setIpsFor(null);
    } finally {
      setIpsLoading(false);
    }
  }, []);

  const submit = useCallback(async () => {
    if (submitting.current) return;
    if (!form.name.trim()) {
      notify.err(t('keys.nameRequired'));
      return;
    }
    let expiresAt = 0;
    if (form.expiryMode === 'date') {
      expiresAt = dateToUnixSec(form.expiryDate);
      if (!expiresAt) {
        notify.err(t('keys.expiryDateRequired'));
        return;
      }
    }
    submitting.current = true;
    setBusy(true);
    try {
      const payload = {
        name: form.name.trim(),
        // '' 是有意义的取值（不限制版本），照传——后端以它区分
        // 「不限定版本」与「限定了某一版」
        realm: form.realm,
        // 0 = 永不过期
        expires_at: expiresAt,
        max_ips: Math.max(0, Number(form.maxIps) || 0),
        ip_whitelist: toLines(form.ipWhitelist),
        model_whitelist: toLines(form.models),
        token_quota: Math.max(0, Number(form.tokenQuota) || 0),
        credit_quota: Math.max(0, Number(form.creditQuota) || 0),
        rate_limit: Math.max(0, Number(form.rateLimit) || 0),
      };
      if (editing) {
        await keyApi.update(editing.id, payload);
        notify.ok(t('keys.keyUpdated'));
      } else {
        const created = await keyApi.create(payload);
        notify.ok(t('keys.keyCreated'));
        if (created.plaintext) setIssued(created.plaintext);
      }
      setFormOpen(false);
      // 刷新失败**不能**把这次创建/保存判成失败：写操作已经成功了
      keysCache.refresh().catch(() => {
        notify.warn(t('keys.createdButRefreshFailed'), t('keys.createdButRefreshFailedHint'));
      });
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setBusy(false);
      submitting.current = false;
    }
  }, [form, editing, keysCache, t]);

  const toggle = useCallback(async (k: ApiKey) => {
    try {
      await keyApi.update(k.id, {enabled: !k.enabled});
      notify.ok(k.enabled ? t('keys.disabled') : t('keys.enabled'));
      keysCache.refresh();
    } catch (e) {
      notify.err(errText(e));
    }
  }, [keysCache, t]);

  /** 当前一次性弹窗里的导出片段（useMemo：输入不变不重算） */
  const snippet = useMemo(() => {
    if (!issued || !baseUrlDraft) return null;
    const origin = baseUrlDraft.trim().replace(/\/+$/, '');
    if (!origin) return null;
    // OpenAI 兼容口径：补全 /v1；若用户已带 /v1 则不重复
    const openaiBase = /\/v1$/.test(origin) ? origin : `${origin}/v1`;
    // ⚠️ Claude 系口径：必须剥掉末尾 /v1——ANTHROPIC_BASE_URL 会被 SDK
    // 自动拼上 /v1/messages，带着 /v1 会出现 /v1/v1/messages 的 404。
    const claudeBase = origin.replace(/\/v1$/, '');
    return buildSnippet(exportFmt, issued, openaiBase, claudeBase);
  }, [issued, exportFmt, baseUrlDraft]);

  const header = (
    <PageHeader
      title={t('keys.title')}
      description={t('keys.description')}
      actions={
        isAdmin ? (
          <Button size="sm" className="rounded-full" onClick={openCreate}>
            <Plus />
            {t('keys.newKey')}
          </Button>
        ) : null
      }
    />
  );

  // 首载失败且无缓存可展示：错误态（与「暂无密钥」空态严格区分）
  if (loadError && !loading && !keysCache.data) {
    return (
      <div className="flex flex-col gap-4 md:gap-6">
        {header}
        <section className="overflow-hidden rounded-[20px] bg-muted">
          <EmptyState
            icon={ServerCrash}
            title={t('keys.loadErrorTitle')}
            description={loadError}
            className="flex flex-col items-center justify-center py-16 text-center"
          >
            <Button className="mt-4 rounded-full" onClick={retryLoad}>
              <RefreshCw />
              {t('common.refresh')}
            </Button>
          </EmptyState>
        </section>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      {header}

      {loading && !keysCache.data ? (
        <TableSkeleton rows={6} />
      ) : (
        <>
          <section className="overflow-hidden rounded-[20px] bg-muted">
            <Table>
              <TableHeader>
                <TableRow className="border-b border-border/60 hover:bg-transparent">
                  <TableHead className="pl-4 text-[11px] text-muted-foreground">{t('metric.name')}</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">{t('keys.colPrefix')}</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">{t('keys.realm')}</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">{t('keys.expiry')}</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">{t('keys.colIps')}</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">{t('keys.colQuota')}</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">{t('keys.colRateLimit')}</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">{t('keys.colLastUsed')}</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">{t('accounts.colStatus')}</TableHead>
                  {isAdmin && <TableHead className="pr-4 text-right text-[11px] text-muted-foreground">{t('accounts.colActions')}</TableHead>}
                </TableRow>
              </TableHeader>
              <TableBody>
                {keys.map((k) => {
                  const expired = !!k.expires_at && k.expires_at * 1000 < Date.now();
                  const remainSec = k.expires_at ? Math.floor(k.expires_at - Date.now() / 1000) : 0;
                  // 任一配额超限都算超额——与网关拒绝口径保持一致，
                  // 否则「列表正常、调用 429」会被当成网关坏了。
                  const overQuota =
                    (!!k.token_quota && k.used_tokens >= k.token_quota) ||
                    (!!k.credit_quota && k.used_credits >= k.credit_quota);
                  // 进度条取两个配额里更紧的那个
                  const ratios = [
                    k.token_quota ? k.used_tokens / k.token_quota : 0,
                    k.credit_quota ? k.used_credits / k.credit_quota : 0,
                  ].filter(Boolean);
                  const ratio = ratios.length ? Math.min(1, Math.max(...ratios)) : 0;
                  return (
                    <TableRow key={k.id} className="border-b border-border/40">
                      <TableCell className="pl-4 text-sm font-medium">
                        <div className="max-w-[180px] truncate" title={k.name}>{k.name}</div>
                      </TableCell>
                      <TableCell className="font-mono text-xs text-muted-foreground">{k.prefix}…</TableCell>
                      <TableCell>
                        {k.realm === 'global' ? (
                          <Badge variant="secondary" className="rounded-full text-[10px]">{t('realm.global')}</Badge>
                        ) : k.realm === 'cn' ? (
                          <Badge variant="secondary" className="rounded-full text-[10px]">{t('realm.cn')}</Badge>
                        ) : (
                          // 存量密钥（未限定）：单独标注，不默认显示成国内版
                          <span className="text-[10px] text-amber-600 dark:text-amber-400" title={t('keys.realmUnsetTitle')}>
                            {t('keys.realmUnset')}
                          </span>
                        )}
                      </TableCell>
                      <TableCell className="text-xs text-muted-foreground">
                        {k.expires_at ? (
                          <span className={expired ? 'text-red-600 dark:text-red-400' : undefined}>
                            {fmtDateTime(k.expires_at)}
                            {!expired && remainSec > 0 && (
                              <span className="ml-1 text-[10px]">({fmtRemain(remainSec)})</span>
                            )}
                          </span>
                        ) : (
                          t('keys.neverExpires')
                        )}
                      </TableCell>
                      <TableCell className="text-xs text-muted-foreground">
                        {k.max_ips ? t('keys.ipLimit', {n: k.max_ips}) : t('keys.ipUnlimited')}
                        {(k.ip_count ?? 0) > 0 && (
                          <span className="ml-1 tabular-nums">({k.ip_count})</span>
                        )}
                      </TableCell>
                      <TableCell className="w-[150px] text-xs tabular-nums">
                        {(() => {
                          const tone =
                            ratio >= 1
                              ? 'text-red-600 dark:text-red-400 font-medium'
                              : ratio >= 0.8
                                ? 'text-amber-600 dark:text-amber-400 font-medium'
                                : 'text-foreground';
                          return (
                            <span className="flex flex-col gap-1">
                              <span className={tone}>
                                {fmtNumber(k.used_tokens)}
                                {k.token_quota ? ` / ${fmtNumber(k.token_quota)}` : ''}
                              </span>
                              {!!k.credit_quota && (
                                <span className="text-[10px] text-muted-foreground" title={t('keys.quotaCredit')}>
                                  {t('keys.creditUsed', {used: fmtNumber(k.used_credits), quota: fmtNumber(k.credit_quota)})}
                                </span>
                              )}
                              {(!!k.token_quota || !!k.credit_quota) && (
                                <Progress value={ratio * 100} className="h-1.5" />
                              )}
                            </span>
                          );
                        })()}
                      </TableCell>
                      <TableCell className="text-xs text-muted-foreground">
                        {k.rate_limit ? t('keys.rateLimitValue', {n: k.rate_limit}) : t('keys.rateLimitDefault')}
                      </TableCell>
                      <TableCell className="text-xs text-muted-foreground">
                        {k.last_used_at ? (
                          fmtDateTime(k.last_used_at)
                        ) : (
                          <span className="text-muted-foreground/70">{t('keys.neverUsed')}</span>
                        )}
                      </TableCell>
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
                              title={t('keys.viewIps')}
                              onClick={() => openIps(k)}
                            >
                              <Wifi className="h-3.5 w-3.5" />
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
                                try {
                                  await keyApi.resetUsage(k.id);
                                  notify.ok(t('keys.resetDone'));
                                  keysCache.refresh().catch(() => {/* 已有 toast 提示 */});
                                } catch (e) {
                                  notify.err(errText(e));
                                }
                              }}
                              trigger={
                                <Button variant="ghost" size="icon" className="h-7 w-7 rounded-md" title={t('keys.resetUsage')}>
                                  <RotateCcw className="h-3.5 w-3.5" />
                                </Button>
                              }
                            />
                            {/* 密钥名可能很长，放说明里（与账号页删除弹窗同一处理） */}
                            <ConfirmDialog
                              title={t('keys.deleteTitle')}
                              description={t('keys.deleteDesc', {name: k.name})}
                              confirmText={t('keys.delete')}
                              destructive
                              onConfirm={async () => {
                                try {
                                  await keyApi.remove(k.id);
                                  notify.ok(t('keys.deleted'));
                                  keysCache.refresh().catch(() => {/* 已有 toast 提示 */});
                                } catch (e) {
                                  notify.err(errText(e));
                                }
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

            {/* 「暂无密钥」是一条都没有的断言，取不到时走上面的错误态，不进这里 */}
            {!keys.length && (
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
        </>
      )}

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
              <div className="grid grid-cols-2 gap-3">
                <div className="space-y-1.5">
                  <Label className="text-[11px] text-muted-foreground">{t('keys.realmLimit')}</Label>
                  <Select
                    value={form.realm === '' ? '__all__' : form.realm}
                    onValueChange={(v) => setForm({...form, realm: (v === '__all__' ? '' : v) as FormState['realm']})}
                  >
                    <SelectTrigger>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="cn">{t('keys.realmCnOnly')}</SelectItem>
                      <SelectItem value="global">{t('keys.realmGlobalOnly')}</SelectItem>
                      <SelectItem value="__all__">{t('keys.realmAll')}</SelectItem>
                    </SelectContent>
                  </Select>
                  {form.realm === '' && (
                    <p className="text-[10px] leading-4 text-muted-foreground">{t('keys.realmHintUnlimited')}</p>
                  )}
                </div>
                <div className="space-y-1.5">
                  <Label className="text-[11px] text-muted-foreground">{t('keys.expiry')}</Label>
                  <Select
                    value={form.expiryMode}
                    onValueChange={(v) => setForm({...form, expiryMode: v as FormState['expiryMode']})}
                  >
                    <SelectTrigger>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="never">{t('keys.expiryNeverOption')}</SelectItem>
                      <SelectItem value="date">{t('keys.expiryCustomDate')}</SelectItem>
                    </SelectContent>
                  </Select>
                  {form.expiryMode === 'date' && (
                    <Input
                      type="date"
                      value={form.expiryDate}
                      min={new Date().toISOString().slice(0, 10)}
                      onChange={(e) => setForm({...form, expiryDate: e.target.value})}
                    />
                  )}
                </div>
              </div>
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
                  <Label className="text-[11px] text-muted-foreground">{t('keys.rateLimitLabel')}</Label>
                  <Input
                    type="number"
                    min={0}
                    value={form.rateLimit}
                    onChange={(e) => setForm({...form, rateLimit: e.target.value})}
                  />
                  <p className="text-[10px] leading-4 text-muted-foreground">{t('keys.rateLimitHint')}</p>
                </div>
                <div className="space-y-1.5">
                  <Label className="text-[11px] text-muted-foreground">{t('keys.quotaTokens')}</Label>
                  <Input
                    type="number"
                    min={0}
                    value={form.tokenQuota}
                    onChange={(e) => setForm({...form, tokenQuota: e.target.value})}
                  />
                </div>
                <div className="space-y-1.5">
                  {/* 积分配额：上游按倍率扣费，值可能是小数，step=any 不限整数 */}
                  <Label className="text-[11px] text-muted-foreground">{t('keys.quotaCredit')}</Label>
                  <Input
                    type="number"
                    min={0}
                    step="any"
                    value={form.creditQuota}
                    onChange={(e) => setForm({...form, creditQuota: e.target.value})}
                  />
                  <p className="text-[10px] leading-4 text-muted-foreground">{t('keys.quotaCreditHint')}</p>
                </div>
              </div>
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">{t('keys.ipWhitelist')}</Label>
                <Textarea
                  rows={3}
                  value={form.ipWhitelist}
                  onChange={(e) => setForm({...form, ipWhitelist: e.target.value})}
                  placeholder={'10.0.0.0/8\n1.2.3.4'}
                />
              </div>
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">{t('keys.modelWhitelist')}</Label>
                <Textarea
                  rows={3}
                  value={form.models}
                  onChange={(e) => setForm({...form, models: e.target.value})}
                  placeholder={'glm-5.2\nglobal:gpt-5.4'}
                />
                {/* 模型白名单与版本归属是两道检查：版本拦跨域调用，
                    白名单在版本之内再收窄到具体模型。 */}
                <p className="text-[10px] leading-4 text-muted-foreground">
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

      {/* 一次性展示新密钥 + 导出片段。明文仅此一次：网关只存哈希，关掉后
          连服务端也拿不回完整密钥，警示文案与「我已保存」单一出口都在这里。 */}
      <Dialog open={!!issued} onOpenChange={(v) => !v && setIssued(null)}>
        <DialogContent className="max-h-[calc(100dvh-2rem)] w-[min(680px,calc(100vw-2rem))] max-w-[min(680px,calc(100vw-2rem))] sm:max-w-[min(680px,calc(100vw-2rem))]">
          <DialogHeader>
            <DialogTitle>{t('keys.createdTitle')}</DialogTitle>
            <DialogDescription>{t('keys.createdDesc')}</DialogDescription>
          </DialogHeader>
          <DialogBody className="max-h-[min(560px,calc(100dvh-14rem))]">
            <div className="space-y-3 px-6 pb-3">
              {/* min-w-0 必不可少：flex 项默认 min-width:auto，长密钥会把复制按钮挤出去 */}
              <div className="flex items-center gap-2 rounded-2xl bg-muted p-3">
                <code className="min-w-0 flex-1 break-all font-mono text-xs">{issued}</code>
                <CopyButton value={issued || ''} size="sm" showLabel label={t('keys.copyKey')} />
              </div>

              {/* 导出片段（前端本地渲染，无后端调用） */}
              <div className="space-y-2 rounded-2xl border p-3">
                <div className="flex flex-wrap items-center justify-between gap-x-2 gap-y-1.5">
                  <span className="text-xs font-medium">{t('keys.exportTitle')}</span>
                  <Select
                    value={exportFmt}
                    onValueChange={(v) => setExportFmt(v as ExportFormat)}
                  >
                    <SelectTrigger className="h-7 w-[190px] rounded-full text-xs">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {EXPORT_FORMATS.map((f) => (
                        <SelectItem key={f.value} value={f.value}>
                          {t(f.labelKey)}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>
                <div className="space-y-1.5">
                  <Label className="text-[11px] text-muted-foreground">{t('keys.exportBaseUrl')}</Label>
                  <Input
                    value={baseUrlDraft}
                    onChange={(e) => setBaseUrlDraft(e.target.value)}
                    placeholder="https://your-domain.example"
                    className="h-8 font-mono text-xs"
                  />
                  <p className="text-[10px] leading-4 text-muted-foreground">
                    <RichText text={t('keys.exportBaseUrlHint')} />
                  </p>
                </div>
                {snippet ? (
                  <div className="flex items-start gap-2 rounded-xl bg-muted p-2">
                    {/* wrap="off"：配置片段是给机器读的，折行会把一行拆成两行，
                        复制出去就是坏的。宁可横向滚动也不要看似可读实则粘坏。 */}
                    <Textarea
                      readOnly
                      wrap="off"
                      value={snippet}
                      className="max-h-[min(240px,28dvh)] min-h-[120px] flex-1 min-w-0 field-sizing-fixed overflow-auto font-mono text-[11px] leading-relaxed"
                    />
                    <CopyButton value={snippet} size="sm" showLabel label={t('common.copy')} />
                  </div>
                ) : null}
                <p className="text-[10px] leading-4 text-muted-foreground">{t('keys.exportHint')}</p>
              </div>
            </div>
          </DialogBody>
          <DialogFooter>
            <Button className="rounded-full" onClick={() => setIssued(null)}>
              {t('keys.savedIt')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* 查看该密钥绑定过的来源 IP */}
      <Dialog open={!!ipsFor} onOpenChange={(v) => !v && setIpsFor(null)}>
        <DialogContent className="max-w-[440px]">
          <DialogHeader>
            <DialogTitle>{t('keys.ipsTitle', {name: ipsFor?.name ?? ''})}</DialogTitle>
            <DialogDescription>{t('keys.ipsDesc')}</DialogDescription>
          </DialogHeader>
          <DialogBody className="max-h-[min(50vh,400px)]">
            <div className="space-y-1.5 px-6 pb-2">
              {ipsLoading && <p className="py-6 text-center text-xs text-muted-foreground">{t('common.loading')}</p>}
              {!ipsLoading && ips && ips.length === 0 && (
                <p className="py-6 text-center text-xs text-muted-foreground">{t('keys.ipsEmpty')}</p>
              )}
              {!ipsLoading && (ips?.length ?? 0) > 0 && (
                <div className="divide-y divide-border/40 rounded-xl bg-muted">
                  {ips!.map((row) => (
                    <div key={row.ip} className="flex items-center justify-between gap-3 px-3 py-2">
                      <code className="min-w-0 flex-1 truncate font-mono text-xs">{row.ip}</code>
                      <span className="shrink-0 text-[10px] text-muted-foreground">{fmtDateTime(row.first_seen)}</span>
                    </div>
                  ))}
                </div>
              )}
            </div>
          </DialogBody>
          <DialogFooter>
            <Button variant="outline" className="rounded-full" onClick={() => setIpsFor(null)}>
              {t('common.close')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
