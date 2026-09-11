'use client';

import {useCallback, useEffect, useRef, useState} from 'react';
import {
  Settings as SettingsIcon,
  RefreshCw,
  Plus,
  Trash2,
  Save,
  Users,
  Server,
  Shuffle,
  Info,
  TriangleAlert,
  ChevronDown,
  RotateCcw,
  PlugZap,
  Power,
  Loader2,
} from 'lucide-react';
import {notify} from '@/lib/toast';
import {settingsApi, upstreamApi, errText} from '@/lib/api';
import type {ModelInfo, UpstreamConfig, UserItem} from '@/lib/types';
import {PageHeader} from '@/components/common/layout/PageHeader';
import {EmptyState} from '@/components/common/layout/EmptyState';
import {ConfirmDialog} from '@/components/common/layout/ConfirmDialog';
import {useAuth} from '@/lib/auth-context';
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
import {Tabs, TabsContent, TabsList, TabsTrigger} from '@/components/ui/tabs';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';

/* ─────────────────────────────────────────────────────────
 * 可视化配置字段定义
 * 每个字段对应 workbuddy2api config.json 中的一个键，
 * 这里给出中文名称、白话说明与安全取值范围。
 * ───────────────────────────────────────────────────────── */

interface BoolField {
  key: string;
  kind: 'bool';
  label: string;
  desc: string;
  def: boolean;
}

interface NumField {
  key: string;
  kind: 'num';
  label: string;
  desc: string;
  unit?: string;
  min: number;
  max: number;
  def: number;
}

type Field = BoolField | NumField;

const SCHEDULE_FIELDS: Field[] = [
  {
    key: 'checkin_enabled',
    kind: 'bool',
    label: '自动签到',
    desc: '每天自动领取免费额度，保持账号可用',
    def: true,
  },
  {
    key: 'checkin_hours',
    kind: 'num',
    label: '签到检查间隔',
    desc: '每隔多久检查一次是否已签到；已签到会自动跳过',
    unit: '小时',
    min: 1,
    max: 168,
    def: 6,
  },
  {
    key: 'keepalive_enabled',
    kind: 'bool',
    label: '自动保活',
    desc: '定期刷新登录令牌，避免账号因长期闲置掉线',
    def: true,
  },
  {
    key: 'keepalive_hours',
    kind: 'num',
    label: '保活间隔',
    desc: '每隔多久刷新一次令牌',
    unit: '小时',
    min: 1,
    max: 72,
    def: 1,
  },
];

const POOL_FIELDS: Field[] = [
  {
    key: 'max_in_flight',
    kind: 'num',
    label: '单账号最大并发',
    desc: '一个账号同时处理几个请求。调大能提高吞吐，但更容易触发腾讯限流',
    unit: '个',
    min: 1,
    max: 32,
    def: 3,
  },
  {
    key: 'breaker_threshold',
    kind: 'num',
    label: '连续失败熔断阈值',
    desc: '某账号连续失败多少次后，自动暂停使用它一段时间',
    unit: '次',
    min: 1,
    max: 100,
    def: 5,
  },
  {
    key: 'breaker_cooldown',
    kind: 'num',
    label: '熔断后冷却时间',
    desc: '被暂停的账号，等待多久后自动恢复使用',
    unit: '秒',
    min: 10,
    max: 3600,
    def: 60,
  },
];

type Group = 'schedule' | 'pool';
const GROUPS: {id: Group; title: string; desc: string; fields: Field[]}[] = [
  {
    id: 'schedule',
    title: '自动签到与保活',
    desc: '控制账号每天自动领额度、定期刷新令牌',
    fields: SCHEDULE_FIELDS,
  },
  {
    id: 'pool',
    title: '并发与熔断',
    desc: '控制账号池的并发能力与故障保护',
    fields: POOL_FIELDS,
  },
];

function defaultValues(fields: Field[]): Record<string, boolean | number> {
  const out: Record<string, boolean | number> = {};
  for (const f of fields) out[f.key] = f.def;
  return out;
}

/** 从配置中取出某个分组的已知字段（缺失或类型不符时回退到默认值） */
function pickValues(fields: Field[], source: Record<string, unknown> | undefined): Record<string, boolean | number> {
  const out: Record<string, boolean | number> = {};
  for (const f of fields) {
    const raw = source?.[f.key];
    if (f.kind === 'bool') {
      out[f.key] = typeof raw === 'boolean' ? raw : f.def;
    } else {
      const n = typeof raw === 'number' ? raw : Number(raw);
      out[f.key] = Number.isFinite(n) ? n : f.def;
    }
  }
  return out;
}

const FIELD_BY_KEY: Record<string, Field> = {};
for (const g of GROUPS) for (const f of g.fields) FIELD_BY_KEY[f.key] = f;

export default function SettingsPage() {
  const {isAdmin} = useAuth();
  const [cfg, setCfg] = useState<UpstreamConfig | null>(null);
  const [models, setModels] = useState<ModelInfo[]>([]);

  /** 可视化表单状态 */
  const [form, setForm] = useState<Record<Group, Record<string, boolean | number>>>({
    schedule: defaultValues(SCHEDULE_FIELDS),
    pool: defaultValues(POOL_FIELDS),
  });
  /** 加载时的原始值，用于只提交改动过的项 */
  const original = useRef<Record<Group, Record<string, boolean | number>>>({
    schedule: defaultValues(SCHEDULE_FIELDS),
    pool: defaultValues(POOL_FIELDS),
  });
  /** 高级模式（直接编辑 JSON） */
  const [advanced, setAdvanced] = useState(false);
  const [schedText, setSchedText] = useState('');
  const [poolText, setPoolText] = useState('');

  const [modelMap, setModelMap] = useState<Record<string, string>>({});
  const [mapAlias, setMapAlias] = useState('');
  const [mapTarget, setMapTarget] = useState('');
  const [users, setUsers] = useState<UserItem[]>([]);
  const [newUser, setNewUser] = useState({username: '', password: '', role: 'viewer'});
  const [busy, setBusy] = useState(false);
  /** Upstash（Redis 持久化）表单 */
  const [upstashForm, setUpstashForm] = useState({url: '', token: ''});
  const [upstashBusy, setUpstashBusy] = useState(false);
  const [restarting, setRestarting] = useState(false);

  const load = useCallback(async () => {
    // 本地数据很快（配置/映射/用户），先取到即渲染，不被上游探测拖慢
    const [c, mm, u] = await Promise.allSettled([
      settingsApi.upstream(),
      settingsApi.modelMap(),
      settingsApi.users(),
    ]);
    if (c.status === 'fulfilled') {
      const v = c.value;
      setCfg(v);
      if (v.available !== false) {
        const picked = {
          schedule: pickValues(SCHEDULE_FIELDS, v.schedule),
          pool: pickValues(POOL_FIELDS, v.pool),
        };
        setForm(picked);
        original.current = {
          schedule: {...picked.schedule},
          pool: {...picked.pool},
        };
        setSchedText(JSON.stringify(v.schedule ?? {}, null, 2));
        setPoolText(JSON.stringify(v.pool ?? {}, null, 2));
        // url 可回显；token 不回显明文，留空表示不修改
        setUpstashForm({url: v.upstash?.url || '', token: ''});
      }
    }
    if (mm.status === 'fulfilled') setModelMap(mm.value);
    if (u.status === 'fulfilled') setUsers(u.value);
  }, []);

  /** 上游模型列表单独拉取：上游不可达时可能较慢，不阻塞其余设置项 */
  const loadModels = useCallback(async () => {
    try {
      setModels(await upstreamApi.models());
    } catch {
      setModels([]);
    }
  }, []);

  useEffect(() => {
    load();
    loadModels();
  }, [load, loadModels]);

  const upstreamReady = !!cfg && cfg.available !== false;
  const upstreamError = cfg && cfg.available === false ? cfg.error : undefined;
  /** 配置文件中是否已保存 Upstash 地址 */
  const upstashConfigured = !!cfg?.upstash?.url;

  /** 保存 Upstash 配置（token 留空表示保持原值） */
  async function saveUpstash() {
    if (!upstashForm.url.trim()) {
      notify.err('请填写 Upstash 地址');
      return;
    }
    setUpstashBusy(true);
    try {
      await settingsApi.saveUpstream({
        upstash: {
          url: upstashForm.url.trim(),
          ...(upstashForm.token.trim() ? {token: upstashForm.token.trim()} : {}),
        },
      });
      notify.ok('Upstash 配置已保存', '需重启上游容器使其生效');
      setUpstashForm((f) => ({...f, token: ''}));
      await load();
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setUpstashBusy(false);
    }
  }

  function setField(group: Group, key: string, value: boolean | number) {
    setForm((prev) => ({...prev, [group]: {...prev[group], [key]: value}}));
  }

  /** 是否有未保存的改动 */
  function isDirty(group: Group): boolean {
    const cur = form[group];
    const org = original.current[group];
    return Object.keys(cur).some((k) => cur[k] !== org[k]);
  }

  /** 只提交改动过的字段，避免覆盖其他未展示的配置项 */
  async function saveGroup(group: Group) {
    const cur = form[group];
    const org = original.current[group];
    const patch: Record<string, boolean | number> = {};
    for (const k of Object.keys(cur)) {
      if (cur[k] !== org[k]) patch[k] = cur[k];
    }
    if (!Object.keys(patch).length) {
      notify.info('没有需要保存的改动');
      return;
    }
    setBusy(true);
    try {
      await settingsApi.saveUpstream({[group]: patch});
      notify.ok('设置已保存');
      await load();
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setBusy(false);
    }
  }

  function resetGroup(group: Group) {
    setForm((prev) => ({...prev, [group]: {...original.current[group]}}));
  }

  /** 高级模式：直接保存 JSON */
  async function saveJson(field: Group, text: string) {
    let parsed: unknown;
    try {
      parsed = JSON.parse(text);
    } catch {
      notify.err('JSON 格式有误，请检查括号与逗号');
      return;
    }
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
      notify.err('需要是一个 JSON 对象，例如 { "checkin_hours": 6 }');
      return;
    }
    setBusy(true);
    try {
      await settingsApi.saveUpstream({[field]: parsed});
      notify.ok('设置已保存');
      await load();
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setBusy(false);
    }
  }

  async function saveModelMap(next: Record<string, string>) {
    try {
      await settingsApi.saveModelMap(next);
      setModelMap(next);
      notify.ok('模型映射已保存');
    } catch (e) {
      notify.err(errText(e));
    }
  }

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader
        title="设置"
        description="上游代理配置、模型别名映射与管理端用户"
        actions={
          <Button
            variant="outline"
            size="sm"
            className="rounded-full"
            onClick={() => {
              load();
              loadModels();
            }}
          >
            <RefreshCw />
            刷新
          </Button>
        }
      />

      <Tabs defaultValue="upstream">
        <TabsList>
          <TabsTrigger value="upstream"><Server className="mr-1.5 h-3.5 w-3.5" />上游配置</TabsTrigger>
          <TabsTrigger value="models"><Shuffle className="mr-1.5 h-3.5 w-3.5" />模型映射</TabsTrigger>
          <TabsTrigger value="users"><Users className="mr-1.5 h-3.5 w-3.5" />管理用户</TabsTrigger>
          <TabsTrigger value="about"><Info className="mr-1.5 h-3.5 w-3.5" />关于</TabsTrigger>
        </TabsList>

        {/* ═══ 上游配置 ═══ */}
        <TabsContent value="upstream" className="mt-4 space-y-4">
          {upstreamError && (
            <div className="flex items-start gap-2.5 rounded-[20px] border border-amber-500/30 bg-amber-500/10 p-4">
              <TriangleAlert className="mt-0.5 h-4 w-4 shrink-0 text-amber-500" />
              <div className="space-y-1">
                <div className="text-xs font-medium">无法读取上游配置</div>
                <div className="text-[11px] text-muted-foreground">{upstreamError}</div>
                <div className="text-[11px] text-muted-foreground">
                  请确认 workbuddy2api 已部署且路径正确（环境变量 <code className="font-mono">WB_UPSTREAM_CONFIG</code>）。
                  在读取成功前，下方配置项已锁定，避免误写空配置覆盖真实文件。
                </div>
              </div>
            </div>
          )}

          {/* 账号池概况 */}
          <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
            <div className="rounded-[20px] bg-muted p-4">
              <div className="mb-3 text-sm font-medium">服务信息</div>
              <div className="space-y-2 text-xs">
                {([
                  ['上游地址', cfg?.listen ? `127.0.0.1${cfg.listen}` : '—'],
                  ['接入密钥', cfg?.api_key_masked ? '已配置（已隐藏）' : '—'],
                  ['账号目录', cfg?.auth_dir || '—'],
                ] as [string, string][]).map(([k, v]) => (
                  <div key={k} className="flex items-center justify-between gap-3">
                    <span className="shrink-0 text-muted-foreground">{k}</span>
                    <span className="truncate font-mono" title={v}>{v}</span>
                  </div>
                ))}
                {cfg?.upstream_auth_dir && (
                  <div className="flex items-start justify-between gap-3">
                    <span className="shrink-0 text-muted-foreground">上游声明目录</span>
                    <span
                      className="truncate text-right font-mono text-amber-600 dark:text-amber-400"
                      title={cfg.upstream_auth_dir}
                    >
                      {cfg.upstream_auth_dir}
                    </span>
                  </div>
                )}
              </div>
              {cfg?.upstream_auth_dir && (
                <p className="mt-2 text-[11px] leading-4 text-amber-600 dark:text-amber-400">
                  上游配置里声明的账号目录与本站读取的不一致，管理端实际以「账号目录」为准。
                </p>
              )}
            </div>

            <div className="rounded-[20px] bg-muted p-4 lg:col-span-2">
              <div className="mb-3 flex items-center justify-between">
                <div className="text-sm font-medium">可用模型</div>
                <div className="text-[11px] text-muted-foreground">来自上游实时列表</div>
              </div>
              <div className="flex flex-wrap gap-1.5">
                {models.length ? (
                  models.map((m) => (
                    <Badge key={m.id} variant="secondary" className="rounded-full font-mono text-[10px]">
                      {m.id}
                    </Badge>
                  ))
                ) : (
                  <span className="text-xs text-muted-foreground">
                    {upstreamReady ? '暂时没取到模型列表，请确认上游容器在运行' : '配置未就绪，暂无法获取模型'}
                  </span>
                )}
              </div>
            </div>
          </div>

          {/* 可视化设置卡片 */}
          {GROUPS.map((g) => {
            const dirty = isDirty(g.id);
            return (
              <div key={g.id} className="rounded-[20px] bg-muted p-4">
                <div className="mb-4 flex flex-wrap items-center justify-between gap-2">
                  <div>
                    <div className="text-sm font-medium">{g.title}</div>
                    <div className="text-[11px] text-muted-foreground">{g.desc}</div>
                  </div>
                  <div className="flex items-center gap-2">
                    {dirty && (
                      <Button
                        size="sm"
                        variant="ghost"
                        className="rounded-full text-muted-foreground"
                        disabled={busy}
                        onClick={() => resetGroup(g.id)}
                      >
                        <RotateCcw className="h-3.5 w-3.5" />
                        撤销
                      </Button>
                    )}
                    <Button
                      size="sm"
                      variant={dirty ? 'default' : 'outline'}
                      className="rounded-full"
                      disabled={!isAdmin || busy || !upstreamReady}
                      onClick={() => saveGroup(g.id)}
                    >
                      <Save className="h-3.5 w-3.5" />
                      保存
                    </Button>
                  </div>
                </div>

                <div className="space-y-2">
                  {g.fields.map((f) => (
                    <div
                      key={f.key}
                      className="flex items-center justify-between gap-4 rounded-2xl bg-background/60 px-3.5 py-3"
                    >
                      <div className="min-w-0">
                        <div className="text-xs font-medium">{f.label}</div>
                        <div className="mt-0.5 text-[11px] leading-4 text-muted-foreground">{f.desc}</div>
                      </div>

                      {f.kind === 'bool' ? (
                        <Switch
                          checked={!!form[g.id][f.key]}
                          disabled={!isAdmin || !upstreamReady}
                          onCheckedChange={(v) => setField(g.id, f.key, v)}
                        />
                      ) : (
                        <div className="flex shrink-0 items-center gap-1.5">
                          <Input
                            type="number"
                            min={f.min}
                            max={f.max}
                            value={String(form[g.id][f.key] ?? f.def)}
                            disabled={!isAdmin || !upstreamReady}
                            onChange={(e) => {
                              const n = Number(e.target.value);
                              setField(g.id, f.key, Number.isFinite(n) ? n : f.def);
                            }}
                            className="h-8 w-20 bg-background text-right tabular-nums"
                          />
                          {f.unit && (
                            <span className="w-8 text-[11px] text-muted-foreground">{f.unit}</span>
                          )}
                        </div>
                      )}
                    </div>
                  ))}
                </div>

                {!isAdmin && (
                  <p className="mt-2 text-[11px] text-muted-foreground">只读角色无法修改设置。</p>
                )}
              </div>
            );
          })}

          {/* Redis / Upstash 持久化 */}
          <div className="rounded-[20px] bg-muted p-4">
            <div className="mb-1 flex flex-wrap items-center justify-between gap-2">
              <div>
                <div className="flex items-center gap-2 text-sm font-medium">
                  Redis 持久化（Upstash）
                  {upstashConfigured ? (
                    <Badge variant="secondary" className="rounded-full text-emerald-600 dark:text-emerald-400">
                      已配置
                    </Badge>
                  ) : (
                    <Badge variant="secondary" className="rounded-full text-muted-foreground">
                      未配置
                    </Badge>
                  )}
                </div>
                <div className="mt-0.5 text-[11px] leading-4 text-muted-foreground">
                  {upstashConfigured
                    ? '会话粘性与账号状态由 Upstash 共享存储，多实例部署时状态一致'
                    : '当前为纯内存模式（noop）：状态仅存于容器内。单实例下属正常状态；扩展到多实例或希望重启后保留状态时再配置'}
                </div>
              </div>
              <div className="flex items-center gap-2">
                <Button
                  size="sm"
                  variant="outline"
                  className="rounded-full"
                  disabled={!isAdmin || upstashBusy}
                  onClick={async () => {
                    setUpstashBusy(true);
                    try {
                      const r = await settingsApi.testUpstash(upstashForm.url, upstashForm.token || undefined);
                      (r.ok ? notify.ok : notify.err)(r.message, r.ok ? 'Upstash 可用' : undefined);
                    } catch (e) {
                      notify.err(errText(e));
                    } finally {
                      setUpstashBusy(false);
                    }
                  }}
                >
                  {upstashBusy ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <PlugZap className="h-3.5 w-3.5" />}
                  测试连接
                </Button>
                <Button
                  size="sm"
                  className="rounded-full"
                  disabled={!isAdmin || upstashBusy || !upstreamReady}
                  onClick={saveUpstash}
                >
                  <Save className="h-3.5 w-3.5" />
                  保存
                </Button>
              </div>
            </div>

            <div className="mt-3 grid grid-cols-1 gap-3 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">Upstash 地址</Label>
                <Input
                  value={upstashForm.url}
                  disabled={!isAdmin || !upstreamReady}
                  onChange={(e) => setUpstashForm({...upstashForm, url: e.target.value})}
                  placeholder="https://xxx-12345.upstash.io"
                  className="bg-background font-mono text-xs"
                />
                <div className="text-[11px] text-muted-foreground">
                  在 Upstash 控制台 <span className="font-mono">REST API</span> 一栏复制端点地址
                </div>
              </div>
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">Upstash Token</Label>
                <Input
                  type="password"
                  value={upstashForm.token}
                  disabled={!isAdmin || !upstreamReady}
                  onChange={(e) => setUpstashForm({...upstashForm, token: e.target.value})}
                  placeholder={cfg?.upstash?.has_token ? `已保存：${cfg.upstash.token_masked}（留空则不修改）` : '粘贴 REST API Token'}
                  className="bg-background font-mono text-xs"
                />
                <div className="text-[11px] text-muted-foreground">
                  {cfg?.upstash?.has_token
                    ? '出于安全考虑不回显明文；留空即保持原值不变'
                    : '与上面的地址配套的 Token'}
                </div>
              </div>
            </div>

            <div className="mt-3 flex flex-wrap items-center gap-2">
              <Button
                size="sm"
                variant="outline"
                className="rounded-full"
                disabled={!isAdmin || upstashBusy}
                onClick={async () => {
                  setRestarting(true);
                  try {
                    const r = await settingsApi.reloadUpstream();
                    (r.ok ? notify.ok : notify.err)(r.message || '重启指令已发送');
                  } catch (e) {
                    notify.err(errText(e));
                  } finally {
                    setRestarting(false);
                  }
                }}
              >
                <Power className={restarting ? 'animate-spin' : 'h-3.5 w-3.5'} />
                重启上游使配置生效
              </Button>
              {upstashConfigured && (
                <ConfirmDialog
                  title="关闭 Redis 持久化？"
                  description="将清空 Upstash 地址与 Token，上游会退回纯内存模式（noop）。建议清空后重启上游容器。"
                  confirmText="清空配置"
                  destructive
                  onConfirm={async () => {
                    setUpstashBusy(true);
                    try {
                      await settingsApi.saveUpstream({upstash: {clear: true}});
                      setUpstashForm({url: '', token: ''});
                      notify.ok('已关闭 Redis 持久化', '建议重启上游容器使其生效');
                      await load();
                    } catch (e) {
                      notify.err(errText(e));
                    } finally {
                      setUpstashBusy(false);
                    }
                  }}
                  trigger={
                    <Button size="sm" variant="ghost" className="rounded-full text-red-500" disabled={!isAdmin || upstashBusy}>
                      <Trash2 className="h-3.5 w-3.5" />
                      清空配置
                    </Button>
                  }
                />
              )}
            </div>
            <div className="mt-2 text-[11px] leading-4 text-muted-foreground">
              修改后需重启上游容器才会生效（Upstash 在启动时连接）。
              配置错误时上游会自行降级为 noop 并打印警告，不会导致服务不可用。
            </div>
          </div>

          {/* 高级模式：直接编辑 JSON */}
          <div className="rounded-[20px] bg-muted p-4">
            <button
              type="button"
              onClick={() => setAdvanced((v) => !v)}
              className="flex w-full items-center justify-between gap-2 text-left"
            >
              <div>
                <div className="text-sm font-medium">高级设置</div>
                <div className="text-[11px] text-muted-foreground">
                  需要配置上面没有提到的参数时，可直接编辑原始 JSON
                </div>
              </div>
              <ChevronDown
                className={'h-4 w-4 shrink-0 text-muted-foreground transition-transform ' + (advanced ? 'rotate-180' : '')}
              />
            </button>

            {advanced && (
              <div className="mt-4 grid grid-cols-1 gap-4 lg:grid-cols-2">
                <div className="space-y-2">
                  <div className="flex items-center justify-between">
                    <div className="font-mono text-[11px] text-muted-foreground">schedule</div>
                    <Button
                      size="sm"
                      variant="outline"
                      className="h-7 rounded-full text-[11px]"
                      disabled={!isAdmin || busy || !upstreamReady}
                      onClick={() => saveJson('schedule', schedText)}
                    >
                      保存
                    </Button>
                  </div>
                  <Textarea
                    rows={8}
                    spellCheck={false}
                    disabled={!isAdmin || !upstreamReady}
                    value={upstreamReady ? schedText : ''}
                    placeholder={upstreamReady ? undefined : '未读取到上游配置，无法编辑'}
                    onChange={(e) => setSchedText(e.target.value)}
                    className="bg-background font-mono text-xs"
                  />
                </div>
                <div className="space-y-2">
                  <div className="flex items-center justify-between">
                    <div className="font-mono text-[11px] text-muted-foreground">pool</div>
                    <Button
                      size="sm"
                      variant="outline"
                      className="h-7 rounded-full text-[11px]"
                      disabled={!isAdmin || busy || !upstreamReady}
                      onClick={() => saveJson('pool', poolText)}
                    >
                      保存
                    </Button>
                  </div>
                  <Textarea
                    rows={8}
                    spellCheck={false}
                    disabled={!isAdmin || !upstreamReady}
                    value={upstreamReady ? poolText : ''}
                    placeholder={upstreamReady ? undefined : '未读取到上游配置，无法编辑'}
                    onChange={(e) => setPoolText(e.target.value)}
                    className="bg-background font-mono text-xs"
                  />
                </div>
              </div>
            )}
          </div>
        </TabsContent>

        {/* ═══ 模型映射 ═══ */}
        <TabsContent value="models" className="mt-4 space-y-4">
          <div className="rounded-[20px] bg-muted p-4">
            <div className="mb-1 text-sm font-medium">新增模型别名</div>
            <div className="mb-3 text-[11px] text-muted-foreground">
              让下游用一个自己熟悉的名字调用某个模型。例如把 <code className="font-mono">gpt-4o-mini</code> 指向{' '}
              <code className="font-mono">glm-5.2</code>。
            </div>
            <div className="grid grid-cols-1 items-end gap-3 sm:grid-cols-4">
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">下游使用的名字</Label>
                <Input value={mapAlias} onChange={(e) => setMapAlias(e.target.value)} placeholder="gpt-4o-mini" className="bg-background" disabled={!isAdmin} />
              </div>
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">实际调用模型</Label>
                <Select value={mapTarget} onValueChange={setMapTarget} disabled={!isAdmin}>
                  <SelectTrigger className="bg-background"><SelectValue placeholder="选择模型" /></SelectTrigger>
                  <SelectContent>
                    {models.map((m) => (
                      <SelectItem key={m.id} value={m.id}>{m.id}</SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="sm:col-span-2">
                <Button
                  className="rounded-full"
                  disabled={!isAdmin || !mapAlias.trim() || !mapTarget}
                  onClick={() => {
                    saveModelMap({...modelMap, [mapAlias.trim()]: mapTarget});
                    setMapAlias('');
                    setMapTarget('');
                  }}
                >
                  <Plus />
                  添加映射
                </Button>
              </div>
            </div>
          </div>

          <div className="overflow-hidden rounded-[20px] bg-muted">
            <Table>
              <TableHeader>
                <TableRow className="border-b border-border/60 hover:bg-transparent">
                  <TableHead className="pl-4 text-[11px] text-muted-foreground">下游使用的名字</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">实际调用模型</TableHead>
                  {isAdmin && <TableHead className="pr-4 text-right text-[11px] text-muted-foreground">操作</TableHead>}
                </TableRow>
              </TableHeader>
              <TableBody>
                {Object.entries(modelMap).map(([alias, target]) => (
                  <TableRow key={alias} className="border-b border-border/40">
                    <TableCell className="pl-4 font-mono text-xs">{alias}</TableCell>
                    <TableCell className="font-mono text-xs text-muted-foreground">{target}</TableCell>
                    {isAdmin && (
                      <TableCell className="pr-4 text-right">
                        <Button
                          variant="ghost"
                          size="icon"
                          className="h-7 w-7 rounded-md text-red-500 hover:text-red-600"
                          onClick={() => {
                            const next = {...modelMap};
                            delete next[alias];
                            saveModelMap(next);
                          }}
                        >
                          <Trash2 className="h-3.5 w-3.5" />
                        </Button>
                      </TableCell>
                    )}
                  </TableRow>
                ))}
              </TableBody>
            </Table>
            {!Object.keys(modelMap).length && (
              <div className="py-10 text-center text-xs text-muted-foreground">
                暂无映射，下游可直接使用上方「可用模型」中的名字
              </div>
            )}
          </div>
        </TabsContent>

        {/* ═══ 管理用户 ═══ */}
        <TabsContent value="users" className="mt-4 space-y-4">
          {isAdmin && (
            <div className="rounded-[20px] bg-muted p-4">
              <div className="mb-1 text-sm font-medium">新增登录用户</div>
              <div className="mb-3 text-[11px] text-muted-foreground">
                管理员可以增删账号与改设置；只读用户只能查看，适合给同事看状态用。
              </div>
              <div className="grid grid-cols-1 items-end gap-3 sm:grid-cols-4">
                <div className="space-y-1.5">
                  <Label className="text-[11px] text-muted-foreground">用户名</Label>
                  <Input value={newUser.username} onChange={(e) => setNewUser({...newUser, username: e.target.value})} className="bg-background" />
                </div>
                <div className="space-y-1.5">
                  <Label className="text-[11px] text-muted-foreground">密码</Label>
                  <Input type="password" value={newUser.password} onChange={(e) => setNewUser({...newUser, password: e.target.value})} className="bg-background" />
                </div>
                <div className="space-y-1.5">
                  <Label className="text-[11px] text-muted-foreground">权限</Label>
                  <Select value={newUser.role} onValueChange={(v) => setNewUser({...newUser, role: v})}>
                    <SelectTrigger className="bg-background"><SelectValue /></SelectTrigger>
                    <SelectContent>
                      <SelectItem value="admin">管理员（可修改）</SelectItem>
                      <SelectItem value="viewer">只读用户（仅查看）</SelectItem>
                    </SelectContent>
                  </Select>
                </div>
                <Button
                  className="rounded-full"
                  disabled={busy}
                  onClick={async () => {
                    if (!newUser.username.trim() || !newUser.password) {
                      notify.err('请填写用户名与密码');
                      return;
                    }
                    setBusy(true);
                    try {
                      await settingsApi.addUser(newUser);
                      notify.ok('用户已创建');
                      setNewUser({username: '', password: '', role: 'viewer'});
                      load();
                    } catch (e) {
                      notify.err(errText(e));
                    } finally {
                      setBusy(false);
                    }
                  }}
                >
                  <Plus />
                  创建
                </Button>
              </div>
            </div>
          )}

          <div className="overflow-hidden rounded-[20px] bg-muted">
            <Table>
              <TableHeader>
                <TableRow className="border-b border-border/60 hover:bg-transparent">
                  <TableHead className="pl-4 text-[11px] text-muted-foreground">用户名</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">权限</TableHead>
                  {isAdmin && <TableHead className="pr-4 text-right text-[11px] text-muted-foreground">操作</TableHead>}
                </TableRow>
              </TableHeader>
              <TableBody>
                {users.map((u) => (
                  <TableRow key={u.username} className="border-b border-border/40">
                    <TableCell className="pl-4 text-sm font-medium">{u.username}</TableCell>
                    <TableCell>
                      <Badge variant="secondary" className="rounded-full">
                        {u.role === 'admin' ? '管理员' : '只读'}
                      </Badge>
                    </TableCell>
                    {isAdmin && (
                      <TableCell className="pr-4 text-right">
                        <div className="flex justify-end gap-1">
                          <Button
                            variant="ghost"
                            size="sm"
                            className="h-7 rounded-full text-xs"
                            onClick={async () => {
                              const pwd = window.prompt(`为「${u.username}」设置新密码：`);
                              if (!pwd) return;
                              try {
                                await settingsApi.updateUser(u.username, {password: pwd});
                                notify.ok('密码已更新');
                              } catch (e) {
                                notify.err(errText(e));
                              }
                            }}
                          >
                            重置密码
                          </Button>
                          <ConfirmDialog
                            title={`删除用户「${u.username}」？`}
                            description="删除后该用户将无法登录管理端。"
                            confirmText="删除"
                            destructive
                            onConfirm={async () => {
                              try {
                                await settingsApi.removeUser(u.username);
                                notify.ok('已删除');
                                load();
                              } catch (e) {
                                notify.err(errText(e));
                              }
                            }}
                            trigger={
                              <Button variant="ghost" size="icon" className="h-7 w-7 rounded-md text-red-500 hover:text-red-600">
                                <Trash2 className="h-3.5 w-3.5" />
                              </Button>
                            }
                          />
                        </div>
                      </TableCell>
                    )}
                  </TableRow>
                ))}
              </TableBody>
            </Table>
            {!users.length && (
              <EmptyState
                icon={Users}
                title="暂无管理用户"
                description="至少保留一个管理员账号"
                className="flex flex-col items-center justify-center py-12 text-center"
              />
            )}
          </div>
        </TabsContent>

        {/* ═══ 关于 ═══ */}
        <TabsContent value="about" className="mt-4">
          <div className="rounded-[20px] bg-muted p-5 text-xs leading-6 text-muted-foreground">
            <div className="mb-2 flex items-center gap-2 text-sm font-medium text-foreground">
              <SettingsIcon className="h-4 w-4" />
              WorkBuddy Manager
            </div>
            <p>
              本项目为 workbuddy2api（腾讯 CodeBuddy → OpenAI 兼容代理）提供网页管理界面与对外接口网关。
              账号轮询、并发与熔断由 workbuddy2api 负责；本管理端负责账号纳管、密钥分发、IP 管控、请求日志与用量统计。
            </p>
            <p className="mt-3">
              界面风格参考 linux-do/cdk（MIT），特此致谢。
            </p>
            <p className="mt-3">
              下游接入：Base URL 填 <span className="font-mono">https://你的域名/v1</span>，密钥用「API 密钥」页生成的{' '}
              <span className="font-mono">wbk_...</span>。
            </p>
          </div>
        </TabsContent>
      </Tabs>
    </div>
  );
}
