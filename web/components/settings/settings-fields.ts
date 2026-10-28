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
  /**
   * 成对区间校验：本值不得大于同组该键的值（本字段是「下限」，如账号间隔下限）。
   * 单字段的 min/max 看不到同伴的值，跨字段规则见 pairError()。
   */
  notAboveKey?: string;
  /** 成对区间校验：本值不得小于同组该键的值（本字段是「上限」）。 */
  notBelowKey?: string;
}

export interface HoursField {
  key: string;
  /** 时刻数组：后端是 []int，如 [9, 21] 表示每天 9 点与 21 点执行 */
  kind: 'hours';
  label: string;
  desc: string;
  def: number[];
  /**
   * 留空合法（保存为 []）。语义是「该项不自动跑、只手动触发」，
   * 因此默认空列表的开关类字段必须打开它，否则空值会被当成格式错误。
   */
  allowEmpty?: boolean;
  /** 输入框占位提示（中文源文，经短语表翻译）；缺省 9, 21 */
  placeholder?: string;
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
  /** 留空合法（语义：**保持原行为 / 回落默认**，不是格式错误）；与 offWhenZero 二选一。 */
  allowEmpty?: boolean;
  /** 零时长（`0` / `0m` / `0s`）非法：此时 0 不是「关闭」而是错误配置，单独给提示。 */
  noZero?: boolean;
  /** 输入框占位提示（字面量，不参与翻译）；缺省 600s */
  placeholder?: string;
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

export interface ListField {
  key: string;
  /** 字符串数组（一行一条，保存时拆分；后端是 []string） */
  kind: 'list';
  label: string;
  desc: string;
  placeholder?: string;
  def: string[];
}

export type Field =
  | BoolField
  | NumField
  | HoursField
  | DurationField
  | SelectField
  | TextField
  | ListField;

/** 时长格式校验：数字 + 单位（s/m/h/d） */
const DURATION_RE = /^\d+\s*(s|m|h|d)$/i;

/** 零时长（0 / 0s / 0m / 0h / 0d）：数值部分为 0（裸 0 也算，便于给出「不能填 0」的专属提示） */
const ZERO_DURATION_RE = /^0+\s*(s|m|h|d)?$/i;

/** 是否为零时长写法，用于 noZero 字段的专属提示。 */
function isZeroDuration(raw: unknown): boolean {
  return ZERO_DURATION_RE.test(String(raw ?? '').trim());
}

/** 一个时长字段的取值是否合法。 */
export function durationOk(f: DurationField, raw: unknown): boolean {
  const text = String(raw ?? '').trim();
  if (f.offWhenZero && (text === '' || text === '0')) return true;
  // allowEmpty 字段：留空 = 保持原行为（合法）；其余字段留空仍算格式错误
  if (text === '') return !!f.allowEmpty;
  if (!DURATION_RE.test(text)) return false;
  // noZero 字段：0m / 0s 这类零时长不接受（0 会被当作「立刻重试」，不是「关闭」）
  return !(f.noZero && isZeroDuration(text));
}

/** 把时刻数组格式化为可读文本，如 [9,21] -> "9, 21" */
export function hoursToText(v: unknown): string {
  if (Array.isArray(v)) {
    return v
      .filter((x): x is number => typeof x === 'number')
      .sort((a, b) => a - b)
      .join(', ');
  }
  return '';
}

/** 解析用户输入的时刻列表：返回 {ok, hours?, error?} */
function parseHours(text: string, allowEmpty = false): {ok: boolean; hours: number[]; error?: string} {
  const parts = text.split(/[,，\s]+/).filter(Boolean);
  if (!parts.length) {
    // allowEmpty 字段：空 = 不自动跑（合法）；其余字段空值仍是格式错误
    return allowEmpty ? {ok: true, hours: []} : {ok: false, hours: [], error: tGlobal('settings.errNeedOneHour')};
  }
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

/**
 * 时刻列表输入的校验与解析（设置页与任务页共用同一套规则与文案）。
 * allowEmpty=true 时空串合法（保存为 []）。
 */
export function parseHoursInput(
  text: string,
  allowEmpty = false,
): {ok: boolean; hours: number[]; error?: string} {
  return parseHours(text, allowEmpty);
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
  {
    key: 'random_delay_minutes',
    kind: 'num',
    label: '整点随机延后',
    desc: '整点自动任务随机延后 0-60 分钟：每个任务每天取一个固定偏移（重启后仍不变），'
      + '避免所有任务挤在同一秒打上游。0 = 关闭',
    unit: '分钟',
    min: 0,
    max: 60,
    def: 0,
  },
  {
    key: 'account_delay_min_seconds',
    kind: 'num',
    label: '账号间隔下限',
    // 与「账号间隔上限」成对：下限 > 上限时 pairError 会拦住保存
    desc: '同一批任务里账号之间随机等待的秒数区间下限（模拟真人节奏，降低批量特征）。0 = 关闭',
    unit: '秒',
    min: 0,
    max: 600,
    def: 0,
    notAboveKey: 'account_delay_max_seconds',
  },
  {
    key: 'account_delay_max_seconds',
    kind: 'num',
    label: '账号间隔上限',
    desc: '上面那个随机等待区间的上限；必须不小于下限，填反了会被拒绝保存。0 = 关闭',
    unit: '秒',
    min: 0,
    max: 600,
    def: 0,
    notBelowKey: 'account_delay_min_seconds',
  },
  {
    key: 'compensation_enabled',
    kind: 'bool',
    label: '每日领补偿',
    desc: '每天自动领取平台发给账号的活动补偿（如故障补偿）。默认关闭——'
      + '补偿公告常带截止日期，手动去任务页点「立即执行」更稳妥',
    def: false,
  },
  {
    key: 'compensation_hours',
    kind: 'hours',
    label: '补偿领取时刻',
    desc: '在哪些整点自动领补偿（0-23，可多个）。留空 = 只手动执行，不自动跑；'
      + '补偿有则领、没有就跳过，重复执行不会多领',
    allowEmpty: true,
    placeholder: '留空 = 不自动跑',
    def: [],
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
  {
    key: 'credit_exhausted',
    kind: 'duration',
    label: '积分耗尽冷却',
    desc: '账号积分耗尽被移出账号池后冷却多久。留空 = 保持原行为（冷却到次日 04:00 等签到恢复）；'
      + '填 **10m / 1h** 则只退出这一小段时间，到期自动回池——中途充值或额度刷新就能马上再用。'
      + '**不能填 0**（零时长会被拒绝）',
    allowEmpty: true,
    noZero: true,
    placeholder: '10m',
    def: '',
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

/**
 * 渠道校验守卫（features.channel_check_guard，channel_guard.go）：上游 11-128
 * 「没通过渠道校验」的抓包留证 + 本地拦截。全部字段保存即热生效（saveConfig
 * 重建守卫整体换入，无需重启）。
 *
 * fail_fast / channel_check_cooldown 已废弃（11-128 改为请求级，不冷却/不轮转），
 * 不再渲染；旧配置里的值会被后端忽略，保留仅为兼容。
 */
export const CHANNEL_GUARD_FIELDS: Field[] = [
  {
    key: 'enabled',
    kind: 'bool',
    label: '启用渠道校验守卫',
    desc: '上游以强渠道文案拒绝、且 system→user 抢救仍失败时：抓取原始出站请求体留证，并把同 body / 同模板的后续请求在网关本地直接拒绝，不再消耗账号。裸 11-128 / unapproved channel 走内容拦截路径，不在此抓包',
    def: true,
  },
  {
    key: 'mode',
    kind: 'select',
    label: '拦截模式',
    desc: 'block = 本地预检命中即 400；warn = 本地只记日志照常出站（观察模式）；off = 关闭本地守卫。上游 11-128 始终按请求级处理（一次 system→user 抢救后仍拒即 400），不冷却/不换号/不禁用，不受本模式影响',
    options: [
      {value: 'block', label: 'block（本地 400，默认）'},
      {value: 'warn', label: 'warn（只记日志）'},
      {value: 'off', label: 'off（关闭）'},
    ],
    def: 'block',
  },
  {
    key: 'block_ttl',
    kind: 'duration',
    label: '同请求拦截时长',
    desc: '同一份请求体（逐字节相同）被拒后，多久内直接本地拒绝。下游重试同一请求的场景靠它止损',
    def: '10m',
    noZero: true,
  },
  {
    key: 'template_ttl',
    kind: 'duration',
    label: '同模板拦截时长',
    desc: '同一模板（system/developer + tools 定义）被拒后，多久内同模板的新请求也本地拒绝。窗口过长可能误伤偶发成功的模板，默认与上项同量级',
    def: '10m',
    noZero: true,
  },
  {
    key: 'scan_messages',
    kind: 'bool',
    label: '拦截短语扫描全部消息',
    desc: '开启时拦截短语也扫用户/助手消息；关闭只扫 system 与工具定义（避免用户内容误伤）',
    def: true,
  },
  {
    key: 'capture_dir',
    kind: 'text',
    label: '抓包目录',
    desc: '命中时请求体落盘位置（容器内路径）。留空 = data/channel_check',
    placeholder: '留空 = data/channel_check',
    def: '',
  },
  {
    key: 'capture_max_mb',
    kind: 'num',
    label: '抓包容量上限',
    desc: '抓包目录总容量，超出后自动删除最旧的文件',
    unit: 'MB',
    min: 1,
    max: 4096,
    def: 256,
  },
  {
    key: 'block_patterns',
    kind: 'list',
    label: '拦截短语',
    desc: '一行一条（大小写不敏感子串）。命中的请求在出站前直接本地拒绝——先用抓包看清触发指纹再填，留空 = 不猜',
    placeholder: '例如：You are Claude Code',
    def: [],
  },
];

/**
 * 日志观测（logging 段）：请求日志归档（JSONL 持久化）与调用来源采集。
 * request_client_info 保存即热生效；归档三件套需重启（装配期创建 recorder）。
 */
export const LOGGING_FIELDS: Field[] = [
  {
    key: 'request_archive',
    kind: 'bool',
    label: '请求日志持久化',
    desc: '按日轮转写 data/requests/requests-YYYYMMDD.jsonl（只记元数据：模型/状态/token/思考档位/思考 token/耗时/错误摘要，不含提示词与响应正文），重启后可查历史',
    def: false,
  },
  {
    key: 'request_retention_days',
    kind: 'num',
    label: '归档保留天数',
    desc: '超过天数的归档文件自动删除',
    unit: '天',
    min: 1,
    max: 365,
    def: 7,
  },
  {
    key: 'request_archive_max_mb',
    kind: 'num',
    label: '归档容量上限',
    desc: '全部归档文件的总容量，超出后删最旧',
    unit: 'MB',
    min: 1,
    max: 4096,
    def: 200,
  },
  {
    key: 'request_client_info',
    kind: 'bool',
    label: '记录调用来源',
    desc: '把客户端 IP 与 User-Agent 记进请求日志（排查「哪个下游触发了风控」时打开；默认关闭不采集）',
    def: false,
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
export type Section =
  | 'schedule'
  | 'cooldown'
  | 'pool'
  | 'features'
  | 'session'
  | 'prompt'
  | 'upstream'
  | 'global'
  | 'channelGuard'
  | 'logging';

/** config.json 段名（保存目标；与 id 不同时由此映射） */
export type SectionWire =
  | 'schedule'
  | 'cooldown'
  | 'pool'
  | 'features'
  | 'session_sticky'
  | 'prompt'
  | 'upstream'
  | 'global'
  | 'logging';

export interface GroupDef {
  id: Section;
  /** 对应的 config.json 段名（UI 名与段名不一致时由此映射） */
  section: SectionWire;
  /**
   * 配置 JSON 里的完整路径（从根开始）。缺省 [section]；嵌套段（如
   * features.channel_check_guard）用它定位取值与写回——saveGroup 据此构造
   * 嵌套 patch，深合并语义与平铺段一致。
   */
  path?: string[];
  title: string;
  desc: string;
  fields: Field[];
}

export const GROUPS: GroupDef[] = [
  {
    id: 'schedule',
    section: 'schedule',
    title: '定时任务',
    desc: '七类任务各自独立排程：签到 / 猫猫旅行 / 活跃上报 / 保活 / 夜猫 / 任务队列 / 每日领补偿。'
      + '可分别开关并设置执行时刻，还支持整点随机延后与账号之间的人工节奏。'
      + '签到、旅行、活跃上报、夜猫、任务队列、领补偿**只对国内版账号生效**——国际版没有这些体系，只有保活照常执行',
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
  {
    id: 'channelGuard',
    section: 'features',
    path: ['features', 'channel_check_guard'],
    title: '渠道校验守卫（防风控）',
    desc: '上游以 11-128「没通过渠道校验」拒绝请求时的自保：**抓包留证 + 本地拦截 + 账号短冷却**。'
      + '保存即热生效；拦截短语建议先看抓包（data/channel_check/ 目录，.meta.json 带模型/账号/上游原文）再填',
    fields: CHANNEL_GUARD_FIELDS,
  },
  {
    id: 'logging',
    section: 'logging',
    title: '日志观测',
    desc: '请求日志的持久化归档与调用来源采集。「请求日志持久化」开启后按日落盘 data/requests/'
      + '（含模型/状态/token/思考档位（入站>出站）/思考 token/耗时/错误摘要）；改归档参数需重启，来源采集保存即生效',
    fields: LOGGING_FIELDS,
  },
];

export type FieldValue = boolean | number | string;

/** 沿 path 逐层取配置里的嵌套对象（任一层缺失/非对象返回 undefined）。 */
export function configAtPath(
  root: Record<string, unknown> | undefined,
  path: string[],
): Record<string, unknown> | undefined {
  let cur: unknown = root;
  for (const seg of path) {
    if (typeof cur !== 'object' || cur === null || Array.isArray(cur)) return undefined;
    cur = (cur as Record<string, unknown>)[seg];
  }
  return typeof cur === 'object' && cur !== null && !Array.isArray(cur)
    ? (cur as Record<string, unknown>)
    : undefined;
}

/** 把 {key: value} 平铺 patch 包装成 path 指向的嵌套对象（单键链，深合并友好）。 */
export function nestPatch(
  path: string[],
  patch: Record<string, boolean | number | number[] | string | string[]>,
): Record<string, unknown> {
  if (!path.length) return {...patch};
  const out: Record<string, unknown> = {};
  let cur = out;
  for (let i = 0; i < path.length - 1; i++) {
    const next: Record<string, unknown> = {};
    cur[path[i]] = next;
    cur = next;
  }
  cur[path[path.length - 1]] = {...patch};
  return out;
}

export function defaultValues(fields: Field[]): Record<string, FieldValue> {
  const out: Record<string, FieldValue> = {};
  for (const f of fields) {
    out[f.key] =
      f.kind === 'hours' ? hoursToText(f.def) : f.kind === 'list' ? f.def.join('\n') : f.def;
  }
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
        // 缺失/非法回退默认；allowEmpty 字段允许留空（String(undefined) 会变成
        // 'undefined'，所以先兜成空串再 trim）
        out[f.key] = durationOk(f, raw) ? String(raw ?? '').trim() : f.def;
        break;
      case 'select':
        // 只接受枚举内的取值；配置里是别的值（改过枚举）时回退默认
        out[f.key] = typeof raw === 'string' && f.options.some((o) => o.value === raw) ? raw : f.def;
        break;
      case 'text':
        out[f.key] = typeof raw === 'string' ? raw : f.def;
        break;
      case 'list':
        // 后端 []string；表单里一行一条承载，保存时拆回数组
        out[f.key] = Array.isArray(raw)
          ? raw.filter((x): x is string => typeof x === 'string').join('\n')
          : f.def.join('\n');
        break;
    }
  }
  return out;
}

/** 文本类字段的即时校验（用于输入框下方提示，不阻塞输入） */
export function fieldError(f: Field, raw: FieldValue): string | undefined {
  if (f.kind === 'num') {
    // 输入框的 min/max 只是浏览器属性，不参与提交校验，这里显式检查。
    // 取值口径（含空串按 0）见 numValue()，跨字段区间比较与之共用。
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
    const r = parseHours(String(raw), !!f.allowEmpty);
    return r.ok ? undefined : r.error;
  }
  if (f.kind === 'duration') {
    if (durationOk(f, raw)) return undefined;
    // 零时长单独提示「不能填 0」：通用格式提示会让人以为补个单位就好
    return f.noZero && isZeroDuration(raw)
      ? tGlobal('settings.errDurationZero')
      : tGlobal('settings.errDuration');
  }
  if (f.kind === 'text' && /[\r\n\u0000-\u001f]/.test(String(raw))) {
    return tGlobal('settings.errNoNewline');
  }
  if (f.kind === 'list') {
    // 一行一条；单条里的控制字符（换行除外）与超长行后端会拒，前端先拦
    for (const line of String(raw ?? '').split('\n')) {
      if (/[\u0000-\u0009\u000b-\u001f]/.test(line) || line.length > 512) {
        return tGlobal('settings.errListLine');
      }
    }
  }
  return undefined;
}

/**
 * 数字字段的统一取值口径：**空串按 0**（= 关闭 / 未设置，与后端同义），
 * 非数字（"-"、"1e" 这类输入中间态）返回 undefined。
 *
 * 必须与 fieldError / toWire 的 `Number(raw)` 同口径：它们把空串当 0，
 * 这里若把空串当「还没填」跳过比较，就会出现「两个输入框都不亮错、保存却提交
 * 坏区间」的双标——清空「账号间隔上限」（下限仍为 100）时提交 min=100 > max=0，
 * 只能等后端 validateDelays 拒绝。
 */
function numValue(raw: FieldValue | undefined): number | undefined {
  if (raw === undefined) return undefined;
  const n = Number(raw);
  return Number.isFinite(n) ? n : undefined;
}

/**
 * 成对区间字段的跨字段校验：「账号间隔下限 ≤ 上限」这类规则。
 *
 * fieldError 只拿到单个字段的值，看不到同伴——渲染层（即时提示）与保存层
 * （拒绝提交）都要带上整组表单调用本函数。两侧字段都标注了配对键，所以
 * 无论用户改哪一个，两个输入框都会一起亮出同一条错误。
 *
 * 取值口径见 numValue()：空串按 0 参与比较，所以「下限有值、上限被清空」
 * 会就地报区间错，不再依赖后端拒绝；只有非数字中间态才跳过（那种情况由
 * fieldError 报「请填写数字」）。
 */
export function pairError(f: Field, group: Record<string, FieldValue>): string | undefined {
  if (f.kind !== 'num') return undefined;
  const otherKey = f.notAboveKey ?? f.notBelowKey;
  if (!otherKey) return undefined;
  const self = numValue(group[f.key]);
  const other = numValue(group[otherKey]);
  // 非数字中间态交给 fieldError 的「请填写数字」，这里不重复报区间错
  if (self === undefined || other === undefined) return undefined;
  if (f.notAboveKey && self > other) return tGlobal('settings.errRangeOrder', {min: self, max: other});
  if (f.notBelowKey && self < other) return tGlobal('settings.errRangeOrder', {min: other, max: self});
  return undefined;
}

/** 把表单值转换成要写入 config.json 的值 */
export function toWire(
  f: Field,
  raw: FieldValue,
): {ok: true; value: boolean | number | number[] | string | string[]} | {ok: false; error: string} {
  if (f.kind === 'hours') {
    const r = parseHours(String(raw), !!f.allowEmpty);
    return r.ok ? {ok: true, value: r.hours} : {ok: false, error: r.error ?? tGlobal('settings.errHourFormat')};
  }
  if (f.kind === 'duration') {
    const t = String(raw).trim();
    if (durationOk(f, t)) return {ok: true, value: t};
    return {
      ok: false,
      error: f.noZero && isZeroDuration(t)
        ? tGlobal('settings.errDurationZero')
        : tGlobal('settings.errDuration'),
    };
  }
  if (f.kind === 'num') {
    // 口径与 fieldError / pairError(numValue) 一致：空串按 0 提交
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
  if (f.kind === 'list') {
    const arr = String(raw ?? '')
      .split('\n')
      .map((x) => x.trim())
      .filter(Boolean);
    for (const line of arr) {
      if (line.length > 512) return {ok: false, error: tGlobal('settings.errListLine')};
    }
    return {ok: true, value: arr};
  }
  return {ok: true, value: raw};
}

/**
 * 按分组解析字段定义（保存/校验入口统一走这里）。
 *
 * **必须**走本函数做分组内查找，禁止「按字段名全局映射」：分组间存在同名字段——
 * `mode` 同时在系统提示词与渠道校验守卫、`enabled` 同时在国际版/会话粘性/渠道校验
 * 守卫——全局键名映射会被 GROUPS 里靠后的分组覆盖。历史 bug：FIELD_BY_KEY['mode']
 * 被守卫分组的枚举（block/warn/off）接管，切换提示词模式提交 'custom' 恒报
 * 「取值不在允许范围内」，保存永远失败。
 */
export function fieldOf(def: GroupDef, key: string): Field | undefined {
  return def.fields.find((f) => f.key === key);
}

// 开发期防回归：字段名跨分组重复时控制台告警。运行期无害（字段解析已全部走
// fieldOf 分组内查找，重名只影响可读性），但新分组加字段时应优先起不冲突的名。
if (process.env.NODE_ENV !== 'production') {
  const seen = new Map<string, string>();
  for (const g of GROUPS) {
    for (const f of g.fields) {
      const prev = seen.get(f.key);
      if (prev) {
        console.error(
          `[settings] 字段名跨分组重复: "${f.key}" 同时存在于 ${prev} 与 ${g.id}`
            + '（字段解析必须走 fieldOf 分组内查找，勿按字段名建全局映射）',
        );
      } else {
        seen.set(f.key, g.id);
      }
    }
  }
}

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
    channelGuard: defaultValues(CHANNEL_GUARD_FIELDS),
    logging: defaultValues(LOGGING_FIELDS),
  };
}

/** 装配期**段**：整段字段都在装配期被读取，改了就要重启（键是 config.json 段名）。
 *  session_sticky 同属此类（ttl / gc_interval 被 session.Router 在装配期捕获），
 *  审查补漏：此前不在集合里，与后端 restartRequiredFields 的清单对不上。 */
const RESTART_SECTIONS = new Set(['prompt', 'upstream', 'global', 'session_sticky']);

/**
 * 装配期**字段**：所在段大多可热改，只有个别字段绑定在装配期对象上。
 *
 * 为什么不能把这些段整段并进 RESTART_SECTIONS——schedule 段就是反例：
 * 各任务的小时（scheduler.SetHours）与开关（SetEnabled）都是热改的，
 * 整段并入会让每次改「签到时刻」都误报「需重启」。
 *
 * schedule.random_delay_minutes / schedule.account_delay_min_seconds /
 * schedule.account_delay_max_seconds 三项在 scheduler 装配期就被读进 Config
 * （与 activity_report_count 同口径），只能重启生效；后端 restartRequiredFields
 * 也把它们列入清单。
 */
const RESTART_FIELDS = [
  'schedule.random_delay_minutes',
  'schedule.account_delay_min_seconds',
  'schedule.account_delay_max_seconds',
];

/** 后端字段名是否落在某个登记项下：整串相等，或其后跟 `.` 前缀。 */
function hitsRestartEntry(name: string, entry: string): boolean {
  return name === entry || name.startsWith(entry + '.');
}

/**
 * 保存响应的 restart_required 是否命中本页的装配期段 / 字段。
 * 后端（panel_config.go restartRequiredFields）返回的是**字段级**名字：
 * 'prompt.mode' / 'upstream.timeout_seconds' / 'global.enabled'…——整串
 * 匹配段名 Set 恒为 false，会漏报「需重启」。这里按「段名本身或段名.
 * 前缀」匹配，再补一遍字段级清单（见 RESTART_FIELDS）。
 */
export function needsRestartBySection(saved: string[] | undefined): boolean {
  return (saved ?? []).some(
    (s) => [...RESTART_SECTIONS].some((p) => hitsRestartEntry(s, p))
      || RESTART_FIELDS.some((f) => hitsRestartEntry(s, f)),
  );
}
