'use client';

import {useCallback, useEffect, useRef, useState} from 'react';
import {
  Settings as SettingsIcon,
  RefreshCw,
  Plus,
  Trash2,
  Save,
  Server,
  Shuffle,
  Info,
  TriangleAlert,
  ChevronDown,
  RotateCcw,
  PlugZap,
  Loader2,
  Sparkles,
} from 'lucide-react';
import {notify} from '@/lib/toast';
import {useI18n} from '@/lib/i18n/provider';
import {t as tGlobal, tp as tpGlobal} from '@/lib/i18n';
import {RichText} from '@/lib/i18n/rich-text';
import {settingsApi, modelApi, errText} from '@/lib/api';
import type {ConfigGetResponse, UpdateCheck} from '@/lib/types';
import {PageHeader} from '@/components/common/layout/PageHeader';
import {EmptyState} from '@/components/common/layout/EmptyState';
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
 * 每个字段对应 workbuddy2api config.json 中的一个键（cmd/server/config.go），
 * 这里给出中文名称、白话说明与安全取值范围。保存走 POST /api/config 深合并：
 * 只提交改动过的分组，未知键由后端保留。
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
  /** 步进，缺省 1；小数参数用 0.1 */
  step?: number;
  def: number;
}

interface HoursField {
  key: string;
  /** 时刻数组：后端是 []int，如 [9, 21] 表示每天 9 点与 21 点执行 */
  kind: 'hours';
  label: string;
  desc: string;
  def: number[];
}

interface DurationField {
  key: string;
  /** 时长字符串：后端接受 30s / 10m / 2h 等 */
  kind: 'duration';
  label: string;
  desc: string;
  def: string;
  /** 允许 `0` / 空串（语义：**关闭该特性**，不是格式错误）。 */
  offWhenZero?: boolean;
}

interface SelectField {
  key: string;
  /** 枚举字符串：后端只接受给定取值 */
  kind: 'select';
  label: string;
  desc: string;
  /** 危险项：填错会导致网关启动失败，界面上给显式警示 */
  caution?: string;
  options: {value: string; label: string}[];
  def: string;
}

interface TextField {
  key: string;
  /** 自由文本（文件路径之类），不做格式假设，仅禁止换行/控制字符 */
  kind: 'text';
  label: string;
  desc: string;
  caution?: string;
  placeholder?: string;
  def: string;
}

type Field =
  | BoolField
  | NumField
  | HoursField
  | DurationField
  | SelectField
  | TextField;

/** 时长格式校验：数字 + 单位（s/m/h/d） */
const DURATION_RE = /^\d+\s*(s|m|h|d)$/i;

/** 一个时长字段的取值是否合法。 */
function durationOk(f: DurationField, raw: unknown): boolean {
  const text = String(raw ?? '').trim();
  if (f.offWhenZero && (text === '' || text === '0')) return true;
  return DURATION_RE.test(text);
}

/** 把时刻数组格式化为可读文本，如 [9,21] -> "9, 21" */
function hoursToText(v: unknown): string {
  if (Array.isArray(v)) {
    return v
      .filter((x): x is number => typeof x === 'number')
      .sort((a, b) => a - b)
      .join(', ');
  }
  return '';
}

/** 解析用户输入的时刻列表：返回 {ok, hours?, error?} */
function parseHours(text: string): {ok: boolean; hours: number[]; error?: string} {
  const parts = text.split(/[,，\s]+/).filter(Boolean);
  if (!parts.length) return {ok: false, hours: [], error: tGlobal('settings.errNeedOneHour')};
  const out: number[] = [];
  for (const p of parts) {
    const n = Number(p);
    if (!Number.isInteger(n) || n < 0 || n > 23) {
      return {ok: false, hours: [], error: tGlobal('settings.errHourRange', {v: p})};
    }
    if (!out.includes(n)) out.push(n);
  }
  return {ok: true, hours: out.sort((a, b) => a - b)};
}

const SCHEDULE_FIELDS: Field[] = [
  {
    key: 'checkin_enabled',
    kind: 'bool',
    label: '自动签到',
    desc: '每天自动领取免费额度；签到时的余额查询还能解冻被冷却的账号',
    def: true,
  },
  {
    key: 'checkin_hours',
    kind: 'hours',
    label: '签到时刻',
    desc: '在哪些整点执行签到（0-23，可多个）。签到时同时刷新余额并解冻冷却账号',
    def: [9, 21],
  },
  {
    key: 'travel_enabled',
    kind: 'bool',
    label: '猫猫旅行',
    desc: '自动推进「猫猫旅行」：领养 / 派出 / 领取到站奖励，可获得积分',
    def: true,
  },
  {
    key: 'travel_hours',
    kind: 'hours',
    label: '旅行时刻',
    desc: '在哪些整点推进旅行（0-23，可多个）。默认两趟闭环：早上领奖并派出，晚上领当日奖励',
    def: [9, 21],
  },
  {
    key: 'activity_enabled',
    kind: 'bool',
    label: '活跃上报',
    desc: '每日上报一次对话活跃，点亮连续登录并解锁领养前置任务（领猫需要）',
    def: true,
  },
  {
    key: 'activity_hours',
    kind: 'hours',
    label: '上报时刻',
    desc: '在哪些整点上报（0-23，可多个）。每号每天一次即可，重复上报无额外收益',
    def: [10],
  },
  {
    key: 'activity_report_count',
    kind: 'num',
    label: '每次上报条数',
    desc: '每个账号每次活跃上报发几条。领养猫需要 5 次对话，默认 5 条一次刷满；填 1 即旧行为',
    unit: '条',
    min: 1,
    max: 20,
    def: 5,
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
    kind: 'hours',
    label: '保活时刻',
    desc: '在哪些整点刷新令牌（0-23，可多个）',
    def: [22],
  },
  {
    key: 'school_enabled',
    kind: 'bool',
    label: '开学季任务',
    desc: '自动完成开学季任务中心的活动并抽空抽奖余额（仅国内版；国际版由内部跳过）',
    def: true,
  },
  {
    key: 'school_hours',
    kind: 'hours',
    label: '开学季时刻',
    desc: '在哪些整点执行开学季任务（0-23，可多个）。活动未开启时会自动跳过，不算失败',
    def: [12],
  },
  {
    key: 'cat_enabled',
    kind: 'bool',
    label: '夜猫任务',
    desc: '在夜猫窗口（23:00–08:00）补做一次夜猫任务；窗口外会自动跳过',
    def: true,
  },
  {
    key: 'cat_hours',
    kind: 'hours',
    label: '夜猫时刻',
    desc: '在哪些整点尝试夜猫任务（0-23，可多个）。默认凌晨 1 点；窗口内每天最多补一次',
    def: [1],
  },
];

const COOLDOWN_FIELDS: Field[] = [
  {
    key: 'soft_rate',
    kind: 'duration',
    label: '软限流冷却基数',
    desc: '被腾讯限流后，账号冷却多久。数值越大越保守（格式如 600s / 10m / 1h）',
    def: '600s',
  },
  {
    key: 'soft_rate_max',
    kind: 'duration',
    label: '冷却退避上限',
    desc: '连续触发限流会逐次延长冷却，这是延长后的封顶值（格式如 2h）',
    def: '2h',
  },
];

const POOL_FIELDS: Field[] = [
  {
    key: 'max_in_flight',
    kind: 'num',
    label: '单账号最大并发',
    desc: '一个账号同时处理几个请求。调大能提高吞吐，但更容易触发腾讯限流（0 = 不限制）',
    unit: '个',
    min: 0,
    max: 32,
    def: 3,
  },
  {
    key: 'max_in_flight_global',
    kind: 'num',
    label: '国际版单账号最大并发',
    // 语义与 max_in_flight **不同**：那边 0 = 不限制，这边 0 = 未设置、回落默认 2。
    desc: '国际版单独用这个上限。国际版的风控更严，官方默认压到 2——'
      + '并发越高越容易被判为异常流量。填 0 表示用默认值 2（注意与上面那个「0 = 不限制」不同）',
    unit: '个',
    min: 0,
    max: 64,
    def: 2,
  },
  {
    key: 'breaker_threshold',
    kind: 'num',
    label: '连续失败熔断阈值',
    desc: '某账号连续失败多少次后，自动暂停使用它一段时间',
    unit: '次',
    min: 1,
    max: 100,
    def: 3,
  },
  {
    key: 'breaker_cooldown',
    kind: 'duration',
    label: '熔断基础冷却',
    desc: '被熔断的账号先等待多久（格式如 30m / 1h）',
    def: '30m',
  },
  {
    key: 'breaker_cooldown_max',
    kind: 'duration',
    label: '熔断冷却上限',
    desc: '反复熔断会指数退避延长，这是封顶值（格式如 6h）',
    def: '6h',
  },
  {
    key: 'idle_weight_per_hour',
    kind: 'num',
    label: '闲置补偿 / 小时',
    desc: '账号每闲置 1 小时增加一点调度权重，让久未使用的账号优先被选中',
    min: 0,
    max: 10,
    step: 0.1,
    def: 0.5,
  },
  {
    key: 'idle_weight_max',
    kind: 'num',
    label: '闲置补偿上限',
    desc: '闲置加成的封顶值，避免某个账号权重无限增大',
    min: 0,
    max: 100,
    step: 0.5,
    def: 5,
  },
  {
    key: 'expiring_soon',
    kind: 'duration',
    label: '快过期积分窗口',
    desc: '到期时间落在此窗口内的积分会被标记为「快过期」，选号时优先消耗掉，避免白白过期。留空或填 0 = 关闭该优化',
    offWhenZero: true,
    def: '168h',
  },
  {
    key: 'cost_explore_interval',
    kind: 'duration',
    label: '成本档位探索周期',
    desc: '选号优化：长时间只用高成本档位时，每隔这么久会改走一次低成本档位探测，'
      + '成功就留在低档（省钱）。留空 = 用默认 30m，填 0 = 关闭该优化',
    offWhenZero: true,
    def: '30m',
  },
];

const FEATURES_FIELDS: Field[] = [
  {
    key: 'sanitize_blacklist_fingerprints',
    kind: 'bool',
    label: '出站请求指纹脱敏',
    desc: '对发往上游的请求做轻量脱敏，降低被判定异常的概率。除非在排查问题，否则建议保持开启',
    def: true,
  },
];

const SESSION_FIELDS: Field[] = [
  {
    key: 'enabled',
    kind: 'bool',
    label: '会话粘性',
    desc: '同一会话的连续请求尽量路由到同一账号，多轮对话更连贯（多实例部署时依赖 Redis）',
    def: true,
  },
  {
    key: 'ttl',
    kind: 'duration',
    label: '会话保持时长',
    desc: '一次会话多久没活动就解除绑定（格式如 30m / 1h）',
    def: '30m',
  },
  {
    key: 'gc_interval',
    kind: 'duration',
    label: '会话清理周期',
    desc: '后台多久清理一次过期会话（格式如 5m / 10m）',
    def: '5m',
  },
];

const PROMPT_FIELDS: Field[] = [
  {
    key: 'mode',
    kind: 'select',
    label: '系统提示词模式',
    desc: 'passthrough：原样透传客户端传来的 system 消息；custom：网关用自有提示词替换它；'
      + 'append：两者并用——在开头连续的 system/developer 块之后插入网关 system，既有消息逐字不动',
    caution:
      '默认 passthrough 会把下游的 system prompt 原样送给上游。若你依赖网关自己的提示词来稳定行为（或避免 system 指纹被判异常），请改为 custom；'
      + '若既要保留客户端原始 system、又要网关的提示词生效，用 append。',
    options: [
      {value: 'passthrough', label: 'passthrough（透传客户端 system，默认）'},
      {value: 'append', label: 'append（保留客户端 system，其后插入网关提示词）'},
      {value: 'custom', label: 'custom（替换为网关提示词）'},
    ],
    def: 'passthrough',
  },
  {
    key: 'file',
    kind: 'text',
    label: '自定义提示词文件',
    desc: '在 custom 与 append 模式下生效。留空使用内置默认提示词',
    caution: '路径必须存在于容器内且可读；写错会导致网关启动失败、反代不可用。不确定就留空。',
    placeholder: '留空 = 使用内置默认',
    def: '',
  },
];

const UPSTREAM_FIELDS: Field[] = [
  {
    key: 'user_agent',
    kind: 'text',
    label: '出站 User-Agent',
    desc: '网关向腾讯发起请求时使用的客户端标识，留空用内置默认（已对齐官方 WorkBuddy）',
    placeholder: '留空 = 内置默认',
    def: '',
  },
  {
    key: 'client_name',
    kind: 'text',
    label: '客户端名称',
    desc: '用量归属头（X-Product / X-IDE-Name / X-IDE-Type / X-IDE-Version）的取值，影响官网「使用端」显示。留空 = 对齐官方桌面端（WorkBuddy）；填 SaaS 可还原旧行为',
    placeholder: '留空 = WorkBuddy（对齐官方桌面端）',
    def: '',
  },
  {
    key: 'client_version',
    kind: 'text',
    label: '客户端版本',
    desc: '出站 UA 里 WorkBuddy/<版本> 这段，也用于 X-IDE-Version 头。留空 = 内置默认（对齐官方分发包）',
    placeholder: '留空 = 内置默认',
    def: '',
  },
  {
    key: 'cli_version',
    kind: 'text',
    label: 'CLI 版本',
    desc: '出站 UA 里 CLI/<版本> 这段。留空 = 内置默认',
    placeholder: '留空 = 内置默认',
    def: '',
  },
  {
    key: 'device_token_file',
    kind: 'text',
    label: '设备 Token 文件',
    desc: '宿主上存放 device token 的文件路径，留空则不读文件。每 5 分钟读一次，读失败自动忽略',
    placeholder: '留空 = 不读文件',
    def: '',
  },
  {
    key: 'passthrough_ip',
    kind: 'bool',
    label: '透传客户端 IP',
    desc: '是否把客户端 IP（X-Forwarded-For / X-Real-IP）透传给上游。缺省关闭——不把内网/代理 IP 暴露给上游',
    def: false,
  },
];

/**
 * 国际版（global）配置。
 * global.enabled=false 是「锁死纯 CN」的逃生门。两个 base 留空即用 https://www.workbuddy.ai。
 */
const GLOBAL_FIELDS: Field[] = [
  {
    key: 'enabled',
    kind: 'bool',
    label: '启用国际版路由',
    desc: '开启后，realm=global 的账号走 workbuddy.ai（国际版模型与端点）。关闭 = 锁死纯 CN：此时国际版账号会被当成国内版账号发往国内端点，属配置错误',
    def: true,
  },
  {
    key: 'chat_base',
    kind: 'text',
    label: '国际版 Chat 基址',
    desc: '国际版的聊天 / 登录 / 模型接口基址，留空用内置默认 https://www.workbuddy.ai',
    placeholder: '留空 = https://www.workbuddy.ai',
    def: '',
  },
  {
    key: 'billing_base',
    kind: 'text',
    label: '国际版 Billing 基址',
    desc: '国际版的积分 / trial 接口基址，留空用内置默认 https://www.workbuddy.ai',
    placeholder: '留空 = https://www.workbuddy.ai',
    def: '',
  },
];

/** 表单分组 id（前端 state 键名） */
type Section = 'schedule' | 'cooldown' | 'pool' | 'features' | 'session' | 'prompt' | 'upstream' | 'global';

/** config.json 段名（保存目标；与 id 不同时由此映射） */
type SectionWire = 'schedule' | 'cooldown' | 'pool' | 'features' | 'session_sticky' | 'prompt' | 'upstream' | 'global';

interface GroupDef {
  id: Section;
  /** 对应的 config.json 段名（UI 名与段名不一致时由此映射） */
  section: SectionWire;
  title: string;
  desc: string;
  fields: Field[];
}

const GROUPS: GroupDef[] = [
  {
    id: 'schedule',
    section: 'schedule',
    title: '定时任务',
    desc: '六类任务各自独立排程：签到 / 猫猫旅行 / 活跃上报 / 保活 / 开学季 / 夜猫。可分别开关并设置执行时刻。签到、旅行、活跃上报、开学季、夜猫**只对国内版账号生效**——国际版没有这些体系，只有保活照常执行',
    fields: SCHEDULE_FIELDS,
  },
  {
    id: 'prompt',
    section: 'prompt',
    title: '系统提示词',
    desc: '网关如何对待客户端传来的 system / developer 消息',
    fields: PROMPT_FIELDS,
  },
  {
    id: 'cooldown',
    section: 'cooldown',
    title: '限流与冷却',
    desc: '被腾讯限流后的冷却策略',
    fields: COOLDOWN_FIELDS,
  },
  {
    id: 'features',
    section: 'features',
    title: '功能开关',
    desc: '网关的进阶行为开关',
    fields: FEATURES_FIELDS,
  },
  {
    id: 'session',
    section: 'session_sticky',
    title: '会话粘性',
    desc: '多轮对话的路由粘性与清理策略',
    fields: SESSION_FIELDS,
  },
  {
    id: 'pool',
    section: 'pool',
    title: '并发与熔断',
    desc: '控制账号池的并发能力与故障保护',
    fields: POOL_FIELDS,
  },
  {
    id: 'upstream',
    section: 'upstream',
    title: '出站标识',
    desc: '网关向腾讯发起请求时的客户端标识',
    fields: UPSTREAM_FIELDS,
  },
  {
    id: 'global',
    section: 'global',
    title: '国际版',
    desc: '国际版（workbuddy.ai）路由开关与基址。关闭即锁死纯 CN',
    fields: GLOBAL_FIELDS,
  },
];

type FieldValue = boolean | number | string;

function defaultValues(fields: Field[]): Record<string, FieldValue> {
  const out: Record<string, FieldValue> = {};
  for (const f of fields) out[f.key] = f.kind === 'hours' ? hoursToText(f.def) : f.def;
  return out;
}

/** 从配置中取出某个分组的已知字段（缺失或类型不符时回退到默认值） */
function pickValues(fields: Field[], source: Record<string, unknown> | undefined): Record<string, FieldValue> {
  const out: Record<string, FieldValue> = {};
  for (const f of fields) {
    const raw = source?.[f.key];
    switch (f.kind) {
      case 'bool':
        out[f.key] = typeof raw === 'boolean' ? raw : f.def;
        break;
      case 'num': {
        const n = typeof raw === 'number' ? raw : Number(raw);
        out[f.key] = Number.isFinite(n) ? n : f.def;
        break;
      }
      case 'hours':
        // 后端为 []int；表单里用 "9, 21" 这样的字符串承载，保存时再解析回数组
        out[f.key] = Array.isArray(raw) ? hoursToText(raw) : hoursToText(f.def);
        break;
      case 'duration':
        out[f.key] = durationOk(f, raw) ? String(raw).trim() : f.def;
        break;
      case 'select':
        // 只接受枚举内的取值；配置里是别的值（改过枚举）时回退默认
        out[f.key] = typeof raw === 'string' && f.options.some((o) => o.value === raw) ? raw : f.def;
        break;
      case 'text':
        out[f.key] = typeof raw === 'string' ? raw : f.def;
        break;
    }
  }
  return out;
}

/** 文本类字段的即时校验（用于输入框下方提示，不阻塞输入） */
function fieldError(f: Field, raw: FieldValue): string | undefined {
  if (f.kind === 'num') {
    // 输入框的 min/max 只是浏览器属性，不参与提交校验，这里显式检查
    const n = Number(raw);
    if (!Number.isFinite(n)) return tGlobal('settings.errNumber');
    if (n < f.min || n > f.max) {
      return tGlobal('settings.errRange', {
        min: f.min ?? '',
        max: f.max ?? '',
        unit: f.unit ? tpGlobal(f.unit) : '',
      });
    }
    return undefined;
  }
  if (f.kind === 'hours') {
    const r = parseHours(String(raw));
    return r.ok ? undefined : r.error;
  }
  if (f.kind === 'duration') {
    return durationOk(f, raw) ? undefined : tGlobal('settings.errDuration');
  }
  if (f.kind === 'text' && /[\r\n\u0000-\u001f]/.test(String(raw))) {
    return tGlobal('settings.errNoNewline');
  }
  return undefined;
}

/** 把表单值转换成要写入 config.json 的值 */
function toWire(
  f: Field,
  raw: FieldValue,
): {ok: true; value: boolean | number | number[] | string} | {ok: false; error: string} {
  if (f.kind === 'hours') {
    const r = parseHours(String(raw));
    return r.ok ? {ok: true, value: r.hours} : {ok: false, error: r.error ?? tGlobal('settings.errHourFormat')};
  }
  if (f.kind === 'duration') {
    const t = String(raw).trim();
    return durationOk(f, t)
      ? {ok: true, value: t}
      : {ok: false, error: tGlobal('settings.errDuration')};
  }
  if (f.kind === 'num') {
    const n = Number(raw);
    if (!Number.isFinite(n)) return {ok: false, error: tGlobal('settings.errNumber')};
    if (n < f.min || n > f.max) {
      return {
        ok: false,
        error: tGlobal('settings.errRange', {
          min: f.min ?? '',
          max: f.max ?? '',
          unit: f.unit ? tpGlobal(f.unit) : '',
        }),
      };
    }
    return {ok: true, value: n};
  }
  if (f.kind === 'select') {
    const v = String(raw);
    return f.options.some((o) => o.value === v)
      ? {ok: true, value: v}
      : {ok: false, error: tGlobal('settings.errEnum')};
  }
  if (f.kind === 'text') {
    const t = String(raw).trim();
    if (/[\r\n\u0000-\u001f]/.test(t)) return {ok: false, error: tGlobal('settings.errNoNewline')};
    return {ok: true, value: t};
  }
  return {ok: true, value: raw};
}

const FIELD_BY_KEY: Record<string, Field> = {};
for (const g of GROUPS) for (const f of g.fields) FIELD_BY_KEY[f.key] = f;

/** 默认的空表单（所有分组） */
function emptyForm(): Record<Section, Record<string, FieldValue>> {
  return {
    schedule: defaultValues(SCHEDULE_FIELDS),
    prompt: defaultValues(PROMPT_FIELDS),
    cooldown: defaultValues(COOLDOWN_FIELDS),
    pool: defaultValues(POOL_FIELDS),
    features: defaultValues(FEATURES_FIELDS),
    session: defaultValues(SESSION_FIELDS),
    upstream: defaultValues(UPSTREAM_FIELDS),
    global: defaultValues(GLOBAL_FIELDS),
  };
}

/** 装配期字段：后端保存后会带回「需重启才生效」的段名集合 */
const RESTART_SECTIONS = new Set(['prompt', 'upstream', 'global']);

export default function SettingsPage() {
  const {t, tp} = useI18n();
  const {isAdmin} = useAuth();
  const [cfg, setCfg] = useState<ConfigGetResponse | null>(null);
  const [models, setModels] = useState<string[]>([]);

  /** 可视化表单状态 */
  const [form, setForm] = useState<Record<Section, Record<string, FieldValue>>>(emptyForm());
  /** 加载时的原始值，用于只提交改动过的项 */
  const original = useRef<Record<Section, Record<string, FieldValue>>>(emptyForm());
  /** 高级模式（直接编辑整个配置 JSON） */
  const [advanced, setAdvanced] = useState(false);
  const [rawText, setRawText] = useState('');

  const [modelMap, setModelMap] = useState<Record<string, string>>({});
  const [mapAlias, setMapAlias] = useState('');
  const [mapTarget, setMapTarget] = useState('');
  const [busy, setBusy] = useState(false);
  /** Upstash（Redis 持久化）表单 */
  const [upstashForm, setUpstashForm] = useState({url: '', token: ''});
  const [upstashBusy, setUpstashBusy] = useState(false);
  /** 版本检查 */
  const [update, setUpdate] = useState<UpdateCheck | null>(null);
  const [updateBusy, setUpdateBusy] = useState(false);

  const load = useCallback(async () => {
    // 本地数据很快（配置/映射/版本），先取到即渲染，不被上游探测拖慢
    const [c, mm, up] = await Promise.allSettled([
      settingsApi.config(),
      settingsApi.modelMap(),
      settingsApi.checkUpdate(),
    ]);
    if (c.status === 'fulfilled') {
      const v = c.value;
      setCfg(v);
      const root = (v.config ?? {}) as Record<string, Record<string, unknown>>;
      const picked: Record<Section, Record<string, FieldValue>> = {
        schedule: pickValues(SCHEDULE_FIELDS, root.schedule),
        prompt: pickValues(PROMPT_FIELDS, root.prompt),
        cooldown: pickValues(COOLDOWN_FIELDS, root.cooldown),
        pool: pickValues(POOL_FIELDS, root.pool),
        features: pickValues(FEATURES_FIELDS, root.features),
        session: pickValues(SESSION_FIELDS, root.session_sticky),
        upstream: pickValues(UPSTREAM_FIELDS, root.upstream),
        global: pickValues(GLOBAL_FIELDS, root.global),
      };
      setForm(picked);
      original.current = Object.fromEntries(
        Object.entries(picked).map(([k, v2]) => [k, {...v2}]),
      ) as Record<Section, Record<string, FieldValue>>;
      setRawText(JSON.stringify(v.config ?? {}, null, 2));
      // url 可回显；token 不回显明文，留空表示不修改
      const upstash = root.upstash as {url?: string; has_token?: boolean; token_masked?: string} | undefined;
      setUpstashForm({url: upstash?.url || '', token: ''});
    }
    if (mm.status === 'fulfilled') setModelMap(mm.value ?? {});
    if (up.status === 'fulfilled') setUpdate(up.value);
  }, []);

  /** 模型列表（模型映射的目标下拉用） */
  const loadModels = useCallback(async () => {
    try {
      const res = await modelApi.models();
      setModels((res.models ?? []).map((m) => m.id));
    } catch {
      setModels([]);
    }
  }, []);

  useEffect(() => {
    load();
    loadModels();
  }, [load, loadModels]);

  const cfgReady = !!cfg?.config;

  /** 保存 Upstash 配置（token 留空表示保持原值） */
  async function saveUpstash() {
    if (!upstashForm.url.trim()) {
      notify.err(t('settings.upstashUrlRequired'));
      return;
    }
    setUpstashBusy(true);
    try {
      const patch: Record<string, unknown> = {
        upstash: {
          url: upstashForm.url.trim(),
          ...(upstashForm.token.trim() ? {token: upstashForm.token.trim()} : {}),
        },
      };
      await settingsApi.saveConfig(patch);
      notify.ok(t('settings.upstashSaved'), t('settings.applying'));
      setUpstashForm((f) => ({...f, token: ''}));
      await load();
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setUpstashBusy(false);
    }
  }

  function setField(group: Section, key: string, value: FieldValue) {
    setForm((prev) => ({...prev, [group]: {...prev[group], [key]: value}}));
  }

  /** 是否有未保存的改动 */
  function isDirty(group: Section): boolean {
    const cur = form[group];
    const org = original.current[group];
    return Object.keys(cur).some((k) => cur[k] !== org[k]);
  }

  /** 只提交改动过的字段，避免覆盖其他未展示的配置项 */
  async function saveGroup(group: Section) {
    const cur = form[group];
    const org = original.current[group];
    const patch: Record<string, boolean | number | number[] | string> = {};
    for (const k of Object.keys(cur)) {
      if (cur[k] === org[k]) continue;
      const f = FIELD_BY_KEY[k];
      if (!f) continue;
      const w = toWire(f, cur[k]);
      if (!w.ok) {
        notify.err(t('settings.fieldInvalid', {label: tp(f.label)}), w.error);
        return;
      }
      patch[k] = w.value;
    }
    if (!Object.keys(patch).length) {
      notify.info(t('settings.noChanges'));
      return;
    }
    setBusy(true);
    try {
      const def = GROUPS.find((g) => g.id === group);
      if (!def) return;
      const saved = await settingsApi.saveConfig({[def.section]: patch});
      const needRestart = (saved?.restart_required ?? []).some((s) => RESTART_SECTIONS.has(s));
      if (needRestart) {
        notify.warn(t('settings.savedRestart'), t('settings.restartHint'));
      } else {
        notify.ok(t('settings.saved'), t('settings.applying'));
      }
      await load();
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setBusy(false);
    }
  }

  function resetGroup(group: Section) {
    setForm((prev) => ({...prev, [group]: {...original.current[group]}}));
  }

  /** 高级模式：直接保存整个配置 JSON（后端深合并 + 原子替换 + 保留未知键） */
  async function saveJson(text: string) {
    let parsed: unknown;
    try {
      parsed = JSON.parse(text);
    } catch {
      notify.err(t('settings.jsonInvalid'));
      return;
    }
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
      notify.err(t('settings.jsonNotObject'));
      return;
    }
    setBusy(true);
    try {
      const saved = await settingsApi.saveConfig(parsed as Record<string, unknown>);
      const needRestart = (saved?.restart_required ?? []).length > 0;
      if (needRestart) {
        notify.warn(t('settings.savedRestart'), t('settings.restartHint'));
      } else {
        notify.ok(t('settings.saved'), t('settings.applying'));
      }
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
      notify.ok(t('settings.modelMapSaved'));
    } catch (e) {
      notify.err(errText(e));
    }
  }

  /** 版本检查（只读，不做自更新） */
  async function recheck() {
    setUpdateBusy(true);
    try {
      const r = await settingsApi.checkUpdate();
      setUpdate(r);
      if (r.has_update) {
        notify.warn(t('updatePanel.newVersion'), t('updatePanel.hasUpdate', {v: r.latest}));
      } else {
        notify.ok(t('updatePanel.upToDate'));
      }
    } catch (e) {
      notify.err(errText(e));
    } finally {
      setUpdateBusy(false);
    }
  }

  return (
    <div className="flex flex-col gap-4 md:gap-6">
      <PageHeader
        title={t('settings.title')}
        description={t('settings.description')}
        actions={
          <Button
            variant="outline"
            size="sm"
            className="rounded-full"
            title={t('settings.reloadTitle')}
            onClick={() => {
              load();
              loadModels();
            }}
          >
            <RefreshCw />
            {t('settings.reload')}
          </Button>
        }
      />

      <Tabs defaultValue="config">
        {/* 标签较多，手机上会撑破容器，这里允许横向滚动 */}
        <div className="-mx-1 overflow-x-auto px-1 pb-1">
        <TabsList className="w-max">
          <TabsTrigger value="config"><Server className="mr-1.5 h-3.5 w-3.5" />{t('settings.tabUpstream')}</TabsTrigger>
          <TabsTrigger value="models"><Shuffle className="mr-1.5 h-3.5 w-3.5" />{t('settings.tabModels')}</TabsTrigger>
          <TabsTrigger value="system"><Sparkles className="mr-1.5 h-3.5 w-3.5" />{t('settings.tabSystem')}</TabsTrigger>
          <TabsTrigger value="about"><Info className="mr-1.5 h-3.5 w-3.5" />{t('settings.tabAbout')}</TabsTrigger>
        </TabsList>
        </div>

        {/* ═══ 配置热编辑 ═══ */}
        <TabsContent value="config" className="mt-3 space-y-3">
          {cfg?.path && (
            <div className="flex items-center gap-2 rounded-[16px] bg-muted/60 px-3.5 py-2 text-[11px] text-muted-foreground">
              <span>{t('settings.configPath')}</span>
              <span className="min-w-0 flex-1 truncate font-mono">{cfg.path}</span>
              <CopyButton value={cfg.path} title={t('settings.copyAuthDir')} className="h-6 w-6" />
            </div>
          )}

          {/* 可视化设置卡片 */}
          {GROUPS.map((g) => {
            const dirty = isDirty(g.id);
            return (
              <div key={g.id} className="rounded-[20px] bg-muted px-3.5 py-3">
                <div className="mb-2.5 flex flex-wrap items-center justify-between gap-2">
                  <div>
                    <div className="text-sm font-medium">{tp(g.title)}</div>
                    {/* 分组说明里带加粗强调（如「只对国内版账号生效」），走 RichText 渲染 */}
                    <RichText className="text-[11px] text-muted-foreground" text={tp(g.desc)} />
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
                        {t('settings.revert')}
                      </Button>
                    )}
                    <Button
                      size="sm"
                      variant={dirty ? 'default' : 'outline'}
                      className="rounded-full"
                      disabled={!isAdmin || busy || !cfgReady}
                      onClick={() => saveGroup(g.id)}
                    >
                      <Save className="h-3.5 w-3.5" />
                      {t('common.save')}
                    </Button>
                  </div>
                </div>

                {/* 宽屏两列：开关与它对应的时刻/数值字段天然成对，行数减半 */}
                <div className="grid grid-cols-1 gap-1.5 xl:grid-cols-2">
                  {g.fields.map((f) => {
                    const err = fieldError(f, form[g.id][f.key]);
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
                            checked={!!form[g.id][f.key]}
                            disabled={!isAdmin || !cfgReady}
                            onCheckedChange={(v) => setField(g.id, f.key, v)}
                          />
                        ) : f.kind === 'num' ? (
                          <div className="flex shrink-0 items-center gap-1.5">
                            <Input
                              type="number"
                              min={f.min}
                              max={f.max}
                              step={f.step ?? 1}
                              value={String(form[g.id][f.key] ?? f.def)}
                              disabled={!isAdmin || !cfgReady}
                              onChange={(e) => {
                                const n = Number(e.target.value);
                                setField(g.id, f.key, Number.isFinite(n) ? n : f.def);
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
                            value={String(form[g.id][f.key] ?? f.def)}
                            disabled={!isAdmin || !cfgReady}
                            onValueChange={(v) => setField(g.id, f.key, v)}
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
                            value={String(form[g.id][f.key] ?? '')}
                            disabled={!isAdmin || !cfgReady}
                            placeholder={f.placeholder ? tp(f.placeholder) : undefined}
                            onChange={(e) => setField(g.id, f.key, e.target.value)}
                            className={
                              'h-8 shrink-0 bg-background text-xs ' +
                              (caution ? 'w-56' : 'w-40') +
                              (err ? ' border-destructive' : '')
                            }
                          />
                        ) : (
                          <Input
                            value={String(form[g.id][f.key] ?? '')}
                            disabled={!isAdmin || !cfgReady}
                            placeholder={f.kind === 'hours' ? '9, 21' : '600s'}
                            onChange={(e) => setField(g.id, f.key, e.target.value)}
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
          })}

          {/* Redis / Upstash 持久化 */}
          <div className="rounded-[20px] bg-muted p-4">
            <div className="mb-1 flex flex-wrap items-center justify-between gap-2">
              <div>
                <div className="flex items-center gap-2 text-sm font-medium">
                  {t('settings.upstashTitle')}
                  {upstashForm.url ? (
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
                  {upstashForm.url
                    ? t('settings.upstashDescOn')
                    : t('settings.upstashDescOff')}
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
                      (r.ok ? notify.ok : notify.err)(r.message, r.ok ? t('settings.upstashOk') : undefined);
                    } catch (e) {
                      notify.err(errText(e));
                    } finally {
                      setUpstashBusy(false);
                    }
                  }}
                >
                  {upstashBusy ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <PlugZap className="h-3.5 w-3.5" />}
                  {t('settings.testConnection')}
                </Button>
                <Button
                  size="sm"
                  className="rounded-full"
                  disabled={!isAdmin || upstashBusy || !cfgReady}
                  onClick={saveUpstash}
                >
                  <Save className="h-3.5 w-3.5" />
                  {t('common.save')}
                </Button>
              </div>
            </div>

            <div className="mt-3 grid grid-cols-1 gap-3 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">{t('settings.upstashUrl')}</Label>
                <Input
                  value={upstashForm.url}
                  disabled={!isAdmin || !cfgReady}
                  onChange={(e) => setUpstashForm({...upstashForm, url: e.target.value})}
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
                  value={upstashForm.token}
                  disabled={!isAdmin || !cfgReady}
                  onChange={(e) => setUpstashForm({...upstashForm, token: e.target.value})}
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
              onClick={() => setAdvanced((v) => !v)}
              className="flex w-full items-center justify-between gap-2 text-left"
            >
              <div>
                <div className="text-sm font-medium">{t('settings.advanced')}</div>
                <div className="text-[11px] text-muted-foreground">
                  {t('settings.advancedDesc')}
                </div>
              </div>
              <ChevronDown
                className={'h-4 w-4 shrink-0 text-muted-foreground transition-transform ' + (advanced ? 'rotate-180' : '')}
              />
            </button>

            {advanced && (
              <div className="mt-4 space-y-2">
                <div className="flex items-center justify-between">
                  <div className="font-mono text-[11px] text-muted-foreground">config.json</div>
                  <Button
                    size="sm"
                    variant="outline"
                    className="h-7 rounded-full text-[11px]"
                    disabled={!isAdmin || busy || !cfgReady}
                    onClick={() => saveJson(rawText)}
                  >
                    {t('common.save')}
                  </Button>
                </div>
                <Textarea
                  rows={16}
                  spellCheck={false}
                  disabled={!isAdmin || !cfgReady}
                  value={cfgReady ? rawText : ''}
                  placeholder={cfgReady ? undefined : t('settings.cfgNotLoaded')}
                  onChange={(e) => setRawText(e.target.value)}
                  className="bg-background font-mono text-xs"
                />
                <p className="text-[11px] leading-4 text-muted-foreground">
                  {t('settings.advancedNote')}
                </p>
              </div>
            )}
          </div>
        </TabsContent>

        {/* ═══ 模型映射 ═══ */}
        <TabsContent value="models" className="mt-4 space-y-4">
          <div className="rounded-[20px] bg-muted p-4">
            <div className="mb-1 text-sm font-medium">{t('settings.mapNewAlias')}</div>
            <div className="mb-3 text-[11px] text-muted-foreground">
              <RichText text={t('settings.mapNewAliasDesc')} />
            </div>
            <div className="grid grid-cols-1 items-end gap-3 sm:grid-cols-4">
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">{t('settings.mapAlias')}</Label>
                <Input value={mapAlias} onChange={(e) => setMapAlias(e.target.value)} placeholder="gpt-4o-mini" className="bg-background" disabled={!isAdmin} />
              </div>
              <div className="space-y-1.5">
                <Label className="text-[11px] text-muted-foreground">{t('settings.mapTarget')}</Label>
                <Select value={mapTarget} onValueChange={setMapTarget} disabled={!isAdmin}>
                  <SelectTrigger className="bg-background"><SelectValue placeholder={t('chat.selectModel')} /></SelectTrigger>
                  <SelectContent>
                    {models.map((m) => (
                      <SelectItem key={m} value={m}>{m}</SelectItem>
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
                  {t('settings.mapAdd')}
                </Button>
              </div>
            </div>
          </div>

          <div className="overflow-hidden rounded-[20px] bg-muted">
            <Table>
              <TableHeader>
                <TableRow className="border-b border-border/60 hover:bg-transparent">
                  <TableHead className="pl-4 text-[11px] text-muted-foreground">{t('settings.mapAlias')}</TableHead>
                  <TableHead className="text-[11px] text-muted-foreground">{t('settings.mapTarget')}</TableHead>
                  {isAdmin && <TableHead className="pr-4 text-right text-[11px] text-muted-foreground">{t('accounts.colActions')}</TableHead>}
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
                {t('settings.mapEmpty')}
              </div>
            )}
          </div>
        </TabsContent>

        {/* ═══ 版本检查（只读，不做自更新）═══ */}
        <TabsContent value="system" className="mt-4 space-y-4">
          <div className="rounded-[20px] bg-muted p-4">
            <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
              <div className="flex items-center gap-2 text-sm font-medium">
                <Sparkles className="h-4 w-4" />
                {t('updatePanel.currentVersion')}
              </div>
              <Button
                variant="outline"
                size="sm"
                className="rounded-full"
                onClick={recheck}
                disabled={updateBusy}
              >
                <RefreshCw className={updateBusy ? 'animate-spin' : ''} />
                {t('updatePanel.checkUpdate')}
              </Button>
            </div>
            {update ? (
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <div className="rounded-2xl bg-background/60 px-3.5 py-3">
                  <div className="text-[11px] text-muted-foreground">{t('updatePanel.manager')}</div>
                  <div className="mt-1 text-sm font-semibold tabular-nums">{update.current || '—'}</div>
                </div>
                <div className="rounded-2xl bg-background/60 px-3.5 py-3">
                  <div className="text-[11px] text-muted-foreground">{t('updatePanel.latest')}</div>
                  <div className="mt-1 flex items-center gap-2 text-sm font-semibold tabular-nums">
                    {update.latest || '—'}
                    {update.has_update ? (
                      <Badge variant="secondary" className="rounded-full text-amber-600 dark:text-amber-400">
                        {t('updatePanel.hasUpdateBadge', {v: update.latest})}
                      </Badge>
                    ) : (
                      <Badge variant="secondary" className="rounded-full text-emerald-600 dark:text-emerald-400">
                        {t('updatePanel.upToDate')}
                      </Badge>
                    )}
                  </div>
                  {update.has_update && update.changelog_url && (
                    <a
                      href={update.changelog_url}
                      target="_blank"
                      rel="noopener noreferrer"
                      className="mt-1 inline-block text-[11px] text-blue-500 hover:underline"
                    >
                      {t('updatePanel.changelogLink')}
                    </a>
                  )}
                </div>
              </div>
            ) : (
              <EmptyState
                icon={Sparkles}
                title={t('updatePanel.notChecked')}
                description={t('updatePanel.notCheckedDesc')}
                className="flex flex-col items-center justify-center py-8 text-center"
              />
            )}
            <p className="mt-3 text-[11px] leading-4 text-muted-foreground">
              {t('updatePanel.readonlyNote')}
            </p>
          </div>
        </TabsContent>

        {/* ═══ 关于 ═══ */}
        <TabsContent value="about" className="mt-4">
          <div className="rounded-[20px] bg-muted p-5 text-xs leading-6 text-muted-foreground">
            <div className="mb-2 flex items-center gap-2 text-sm font-medium text-foreground">
              <SettingsIcon className="h-4 w-4" />
              WorkBuddy Cockpit
            </div>
            <p>
              {t('settings.aboutDesc')}
            </p>
            <p className="mt-3">
              {t('settings.aboutCredits')}
            </p>
            <p className="mt-3">
              <RichText text={t('settings.aboutUsage')} />
            </p>
          </div>
        </TabsContent>
      </Tabs>
    </div>
  );
}
