'use client';

/**
 * 设置页可视化配置的字段定义（从原 settings/page.tsx **原样搬移**，只拆不重构）。
 *
 * 每个字段对应 workbuddy2api config.json 中的一个键（cmd/server/config.go），
 * 这里给出中文名称、白话说明与安全取值范围。保存走 POST /api/config 深合并：
 * 只提交改动过的分组，未知键由后端保留。
 */
import {t as tGlobal, tp as tpGlobal} from '@/lib/i18n';

export interface BoolField {
  key: string;
  kind: 'bool';
  label: string;
  desc: string;
  def: boolean;
}

export interface NumField {
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

export interface HoursField {
  key: string;
  /** 时刻数组：后端是 []int，如 [9, 21] 表示每天 9 点与 21 点执行 */
  kind: 'hours';
  label: string;
  desc: string;
  def: number[];
}

export interface DurationField {
  key: string;
  /** 时长字符串：后端接受 30s / 10m / 2h 等 */
  kind: 'duration';
  label: string;
  desc: string;
  def: string;
  /** 允许 `0` / 空串（语义：**关闭该特性**，不是格式错误）。 */
  offWhenZero?: boolean;
}

export interface SelectField {
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

export interface TextField {
  key: string;
  /** 自由文本（文件路径之类），不做格式假设，仅禁止换行/控制字符 */
  kind: 'text';
  label: string;
  desc: string;
  caution?: string;
  placeholder?: string;
  def: string;
}

export type Field =
  | BoolField
  | NumField
  | HoursField
  | DurationField
  | SelectField
  | TextField;

/** 时长格式校验：数字 + 单位（s/m/h/d） */
const DURATION_RE = /^\d+\s*(s|m|h|d)$/i;

/** 一个时长字段的取值是否合法。 */
export function durationOk(f: DurationField, raw: unknown): boolean {
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

export const SCHEDULE_FIELDS: Field[] = [
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
  {
    key: 'queue_enabled',
    kind: 'bool',
    label: '自动执行任务队列',
    desc: '到点自动执行任务中心的「执行队列」（全账号成长任务，'
      + '等同手动点一次启动）。会真实消耗上游配额，默认关闭；手动开过且未跑完时本轮跳过',
    def: false,
  },
  {
    key: 'queue_hours',
    kind: 'hours',
    label: '队列时刻',
    desc: '在哪些整点自动执行任务队列（0-23，可多个）。默认 10 点；'
      + '建议避开签到/上报时刻，错峰执行',
    def: [10],
  },
];

export const COOLDOWN_FIELDS: Field[] = [
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

export const POOL_FIELDS: Field[] = [
  {
    key: 'pick_strategy',
    kind: 'select',
    label: '选号策略',
    desc: 'weighted（默认）：按积分、快过期积分与闲置时长三因子加权随机，多账号分摊流量；'
      + 'credits_desc：始终优先选积分余额最大的账号，用尽/不可用才顺延次高——先把大余额号用掉，'
      + '其余账号近乎闲置。仅改变「选谁」，冷却、熔断、并发上限与会话粘性照常生效',
    options: [
      {value: 'weighted', label: 'weighted（智能加权，默认）'},
      {value: 'credits_desc', label: 'credits_desc（余额优先，从大到小）'},
    ],
    def: 'weighted',
  },
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
    desc: '账号每闲置 1 小时增加一点调度权重，让久未使用的账号优先被选中。'
      + '该值最小 0.1（后端不允许为 0，想关闭闲置补偿请调小至下限）',
    min: 0.1,
    max: 10,
    step: 0.1,
    def: 0.5,
  },
  {
    key: 'idle_weight_max',
    kind: 'num',
    label: '闲置补偿上限',
    desc: '闲置加成的封顶值，避免某个账号权重无限增大。该值最小 0.5（后端不允许为 0）',
    min: 0.5,
    max: 100,
    step: 0.5,
    def: 5,
  },
  {
    key: 'expiring_soon',
    kind: 'duration',
    label: '快过期积分窗口',
    desc: '到期时间落在此窗口内的积分会被标记为「快过期」，选号时优先消耗掉，避免白白过期。填 0 = 关闭该优化（留空 = 恢复默认 7 天）',
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

export const FEATURES_FIELDS: Field[] = [
  {
    key: 'sanitize_blacklist_fingerprints',
    kind: 'bool',
    label: '出站请求指纹脱敏',
    desc: '对发往上游的请求做轻量脱敏，降低被判定异常的概率。除非在排查问题，否则建议保持开启',
    def: true,
  },
];

export const SESSION_FIELDS: Field[] = [
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

export const PROMPT_FIELDS: Field[] = [
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

export const UPSTREAM_FIELDS: Field[] = [
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
export const GLOBAL_FIELDS: Field[] = [
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
export type Section = 'schedule' | 'cooldown' | 'pool' | 'features' | 'session' | 'prompt' | 'upstream' | 'global';

/** config.json 段名（保存目标；与 id 不同时由此映射） */
export type SectionWire = 'schedule' | 'cooldown' | 'pool' | 'features' | 'session_sticky' | 'prompt' | 'upstream' | 'global';

export interface GroupDef {
  id: Section;
  /** 对应的 config.json 段名（UI 名与段名不一致时由此映射） */
  section: SectionWire;
  title: string;
  desc: string;
  fields: Field[];
}

export const GROUPS: GroupDef[] = [
  {
    id: 'schedule',
    section: 'schedule',
    title: '定时任务',
    desc: '六类任务各自独立排程：签到 / 猫猫旅行 / 活跃上报 / 保活 / 夜猫 / 任务队列。可分别开关并设置执行时刻。签到、旅行、活跃上报、夜猫、任务队列**只对国内版账号生效**——国际版没有这些体系，只有保活照常执行',
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

export type FieldValue = boolean | number | string;

export function defaultValues(fields: Field[]): Record<string, FieldValue> {
  const out: Record<string, FieldValue> = {};
  for (const f of fields) out[f.key] = f.kind === 'hours' ? hoursToText(f.def) : f.def;
  return out;
}

/** 从配置中取出某个分组的已知字段（缺失或类型不符时回退到默认值） */
export function pickValues(fields: Field[], source: Record<string, unknown> | undefined): Record<string, FieldValue> {
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
export function fieldError(f: Field, raw: FieldValue): string | undefined {
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
export function toWire(
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

export const FIELD_BY_KEY: Record<string, Field> = {};
for (const g of GROUPS) for (const f of g.fields) FIELD_BY_KEY[f.key] = f;

/** 默认的空表单（所有分组） */
export function emptyForm(): Record<Section, Record<string, FieldValue>> {
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

/** 装配期字段：后端保存后会带回「需重启才生效」的字段名集合 */
const RESTART_SECTIONS = new Set(['prompt', 'upstream', 'global']);

/**
 * 保存响应的 restart_required 是否命中本页的装配期段。
 * 后端（panel_config.go restartRequiredFields）返回的是**字段级**名字：
 * 'prompt.mode' / 'upstream.timeout_seconds' / 'global.enabled'…——整串
 * 匹配段名 Set 恒为 false，会漏报「需重启」。这里按「段名本身或段名.
 * 前缀」匹配。
 */
export function needsRestartBySection(saved: string[] | undefined): boolean {
  return (saved ?? []).some((s) =>
    [...RESTART_SECTIONS].some((p) => s === p || s.startsWith(p + '.')),
  );
}
