/**
 * 「成长任务一键执行」面板输出行的本地化（issue #19 · 多语言补漏）。
 *
 * 面板回显的是**上游脚本 task_runner.py 的原始 stdout**：\`/api/task-run\` 直接把
 * 子进程输出原样塞进 lines，界面上就是 <pre> 一贴。那批日志文案写死在上游脚本里，
 * 而它是**另一个仓库**（以 volume 挂载进来）：改上游会被它的升级覆盖，把译文写回
 * 数据库留痕更是错的。所以本地化放在**展示层** —— 这里按「模板 → 译文」逐行匹配，
 * 命中就按当前语言重排，输出与存储的原文都不动。
 *
 * 三点取舍：
 *   · 未命中原样返回：上游改文案或新增行时，最坏是继续显示中文原文，
 *     而不会出现空行、键名或丢日志 —— 日志完整性优先于翻译率；
 *   · 动态片段（uid8 / 任务码 / 计数 / id 来源 / 异常文本）原样保留：
 *     它们是数据，翻过去反而不便与上游日志对照；
 *   · 简体中文（源语言）下译文模板就是上游原文，输出与改造前逐字一致。
 *
 * 只匹配**完整一行**（^…$）：task_code 与上游 msg 都是自由文本，宽松匹配有把行内
 * 普通内容当模板吞掉的风险。
 */
import {t as tStatic, tp as tpStatic} from './index';

/** 一行日志的匹配规则：命中即按 build 重排；顺序有意义（具体者在前）。 */
type LineRule = [RegExp, (m: RegExpMatchArray) => string];

/** 上游 task_runner 的通用行前缀：\`[task_runner] <uid8> <task_code>: \`。 */
const RUNNER = '\\[task_runner\\] (\\S+) (\\S+): ';

/** 补上 \`^…$\` 与通用前缀，避免每条规则重复一长串转义。 */
function runner(tail: string): RegExp {
  return new RegExp('^' + RUNNER + tail + '$');
}

const RULES: LineRule[] = [
  // ── 上游拉取失败回落内置表（场景 / 专家 / 技能 / 案例 / 主题）──────
  // 主语（场景清单 / 专家市场 …）与 id 来源一样是**数据**，走短语表。
  [/^\[warn\] (.+?)拉取失败\((.*)\)，回落内置表$/, (m) =>
    tStatic('tasks.runLogWarnFallback', {what: tpStatic(m[1]), err: m[2]})],
  [/^\[warn\] COS 专家清单失败\((.*)\)$/, (m) =>
    tStatic('tasks.runLogWarnCosExperts', {err: m[1]})],

  // ── 领奖 400 降级 web 域 ────────────────────────────────────────
  [runner('claim (\\d+) (.*?) -> 降级 web 域'), (m) =>
    tStatic('tasks.runLogClaimFallback', {uid: m[1], code: m[2], status: m[3], msg: m[4]})],

  // ── query 阶段的跳过分支 ────────────────────────────────────────
  // 「已领，跳过(非映射任务)」与下面带计数的「已领，跳过」同前缀不同尾，
  // 放在前面是为了以后放宽规则时不会互相抢占（两条现在都能各自命中）。
  [runner('query claimed -> 已领，跳过\\(非映射任务\\)'), (m) =>
    tStatic('tasks.runLogClaimedUnmapped', {uid: m[1], code: m[2]})],
  [runner('query 非映射任务，skip'), (m) =>
    tStatic('tasks.runLogNotMapped', {uid: m[1], code: m[2]})],
  [runner('query (\\S+) -> 非映射任务，skip'), (m) =>
    tStatic('tasks.runLogNotMappedStatus', {uid: m[1], code: m[2], status: m[3]})],
  [runner('query 任务不存在'), (m) =>
    tStatic('tasks.runLogTaskMissing', {uid: m[1], code: m[2]})],
  [runner('query (\\S+)\\((\\d+)/(\\d+)\\) -> 不可伪造\\((.*)\\)，skip'), (m) =>
    tStatic('tasks.runLogUnforgeable', {
      uid: m[1], code: m[2], status: m[3], cur: m[4], target: m[5],
      reason: tpStatic(m[6]),
    })],
  [runner('query (\\S+)\\((\\d+)/(\\d+)\\) -> 未 completed，only_claim 跳过'), (m) =>
    tStatic('tasks.runLogOnlyClaimNotDone', {
      uid: m[1], code: m[2], status: m[3], cur: m[4], target: m[5],
    })],
  [runner('query (\\S+)\\((\\d+)/(\\d+)\\) -> only_claim dry-run 跳过'), (m) =>
    tStatic('tasks.runLogOnlyClaimDryRun', {
      uid: m[1], code: m[2], status: m[3], cur: m[4], target: m[5],
    })],
  [runner('query claimed\\((\\d+)/(\\d+)\\) -> 已领，跳过'), (m) =>
    tStatic('tasks.runLogClaimed', {uid: m[1], code: m[2], cur: m[3], target: m[4]})],
  [runner('query (\\S+)\\((\\d+)/(\\d+)\\) -> 可领\\(claim\\)，dry-run 跳过'), (m) =>
    tStatic('tasks.runLogClaimableDryRun', {
      uid: m[1], code: m[2], status: m[3], cur: m[4], target: m[5],
    })],
  [runner('query in_progress\\((\\d+)/(\\d+)\\) -> 非夜猫窗口\\(23-08 CST\\)，skip pending'), (m) =>
    tStatic('tasks.runLogNightCatClosed', {uid: m[1], code: m[2], cur: m[3], target: m[4]})],
  [runner('query in_progress\\((\\d+)/(\\d+)\\) -> 夜猫窗口内，可补 1 次，dry-run 跳过'), (m) =>
    tStatic('tasks.runLogNightCatOpen', {uid: m[1], code: m[2], cur: m[3], target: m[4]})],
  [runner('query (\\S+)\\((\\d+)/(\\d+)\\) -> 可点亮 need=(\\d+) id源=(.+?) ids=(\\[.*\\])，dry-run 跳过'), (m) =>
    tStatic('tasks.runLogLightUpDryRun', {
      uid: m[1], code: m[2], status: m[3], cur: m[4], target: m[5],
      need: m[6], src: tpStatic(m[7]), ids: m[8],
    })],

  // ── 点亮（写操作）阶段 ──────────────────────────────────────────
  [runner('accept (\\d+) (.*)'), (m) =>
    tStatic('tasks.runLogAccept', {uid: m[1], code: m[2], status: m[3], msg: m[4]})],
  [runner('report 无需上报（(\\d+)/(\\d+)）'), (m) =>
    tStatic('tasks.runLogReportNotNeeded', {uid: m[1], code: m[2], cur: m[3], target: m[4]})],
  [runner('report (\\d+)/(\\d+) (\\d+) code=(\\S+) buddy5 失败 -> 降级单发 (\\d+) code=(\\S+)'), (m) =>
    tStatic('tasks.runLogBuddy5Fallback', {
      uid: m[1], code: m[2], i: m[3], need: m[4], status: m[5],
      sc: m[6], status2: m[7], sc2: m[8],
    })],
  [runner('report 无可用对象 id，skip'), (m) =>
    tStatic('tasks.runLogNoObjectId', {uid: m[1], code: m[2]})],
  [runner('report 未达 target（(\\d+)/(\\d+)），WARN 待下次'), (m) =>
    tStatic('tasks.runLogBelowTarget', {uid: m[1], code: m[2], cur: m[3], target: m[4]})],

  // ── 账号级：任务列表拉取失败 / 全量批量警告 / 国际版跳过 ──────────
  [/^ERR: \[task_runner\] (\S+) query list_tasks 失败: (.*)$/, (m) =>
    tStatic('tasks.runLogListTasksFailed', {uid: m[1], err: m[2]})],
  [/^WARN: 全量批量 \+ --yes 未限定 --only，注意 54 号批量——请确认 Hermes 决策后再跑$/, () =>
    tStatic('tasks.runLogBulkWarning')],
  [/^\[skip\] (\S+) global realm 不适用 CN 任务$/, (m) =>
    tStatic('tasks.runLogGlobalSkip', {uid: m[1]})],

  // ── 后端子进程看护自己追加的行（server/services/taskrun.py）──────
  // 后端保持中文原文（\`test_taskrun.py\` 的卡死用例按「卡死」断言），
  // 翻译只发生在展示层。
  [/^!! 运行超过 (\d+) 小时上限，已终止$/, (m) =>
    tStatic('tasks.runLogKilledTotal', {hours: m[1]})],
  [/^!! 已 (\d+)s 无输出，判定卡死并终止$/, (m) =>
    tStatic('tasks.runLogKilledIdle', {idle: m[1]})],
  [/^!! 已手动停止$/, () => tStatic('tasks.runLogManuallyStopped')],
];

/**
 * 把一行面板输出翻译成当前语言；不是已知模板就原样返回。
 *
 * 行首缩进单独保留：上游的 [warn] 行带两空格缩进，译文模板里不带，
 * 免得每种语言都要重复一遍空白。
 */
export function translateRunLine(line: string): string {
  const m = /^(\s*)([\s\S]*)$/.exec(line);
  const indent = m ? m[1] : '';
  const body = m ? m[2] : line;
  for (const [pattern, build] of RULES) {
    const hit = pattern.exec(body);
    if (hit) return indent + build(hit);
  }
  return line;
}
