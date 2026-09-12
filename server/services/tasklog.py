"""上游自动任务日志的解析、采集与留痕。

背景：签到 / 猫猫旅行 / 活跃上报 / 保活这些任务由上游 workbuddy2api 定时执行，
结果只打在**容器日志**里，而且容器一重建（更新上游）日志就没了。
用户在界面上因此看不到「旅行领到了多少积分」这类记录。

这里做两件事：
  1. 把上游日志行解析成结构化事件（类型 / 账号 / 积分 / 成功失败）；
  2. 后台定期采集新产生的行，解析后落库长期保留。

上游日志形状（docker logs --timestamps，前面是 docker 加的 RFC3339 时间）：
    2026-09-11T17:43:44.123456789Z 2026/09/11 17:43:44 travel 89374120: claim ok record=1 reward=100
    2026-09-11T17:43:44.2Z travel 89374120: adopt ok (+300 credits)
    2026-09-11T17:43:44.3Z activity 89374120: streak days=3
    2026-09-11T17:43:44.4Z travel 89374120: status: <error>
"""
from __future__ import annotations

import asyncio
import hashlib
import re
import time
from datetime import datetime, timezone

from .. import db
from . import wb2api

# docker --timestamps 前缀
_DOCKER_TS = re.compile(r'^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z?)\s+(.*)$', re.S)
# 任务行主体：<kind> <uid>: <rest>
_TASK_LINE = re.compile(
    r'\b(travel|activity|checkin|keepalive|user-resource)\s+([0-9A-Za-z_.-]+):\s*(.*)$'
)

# 中文类型名，前端与日志里共用一套说法
KIND_LABELS = {
    'travel': '猫猫旅行',
    'activity': '活跃上报',
    'checkin': '自动签到',
    'keepalive': '令牌保活',
    'user-resource': '余额查询',
    # 余额变动流水：由 credits.record_balance 写入，
    # 用于覆盖上游不打日志的获取渠道（签到、活跃上报等）
    'credit': '积分变动',
}

# 明确表示「什么都没做，也不算失败」的前缀
_SKIP_MARKERS = ('skip', 'skipped')

_POLL_SECONDS = 45


def parse_line(line: str) -> dict | None:
    """把一行容器日志解析成结构化事件；与任务无关的行返回 None。"""
    raw = (line or '').rstrip('\r\n')
    if not raw.strip():
        return None

    ts = 0
    m_ts = _DOCKER_TS.match(raw)
    if m_ts:
        ts = _iso_to_epoch(m_ts.group(1))
        body = m_ts.group(2)
    else:
        body = raw

    m = _TASK_LINE.search(body)
    if not m:
        return None

    kind, uid, rest = m.group(1), m.group(2), m.group(3).strip()
    lower = rest.lower()

    credits = 0
    level = 'error'

    m_reward = re.search(r'reward=(\d+)', rest)
    m_adopt = re.search(r'adopt ok\s*\(\+?(\d+)\s*credits?\)', rest, re.IGNORECASE)
    if m_reward:
        credits = int(m_reward.group(1))
        level = 'credit'
    elif m_adopt:
        credits = int(m_adopt.group(1))
        level = 'credit'
    elif lower.startswith(_SKIP_MARKERS) or ('skip' in lower and 'ok' not in lower):
        level = 'info'
    elif 'silent drop' in lower or 'failed' in lower or 'unknown state' in lower:
        level = 'warn'
    elif ' ok' in lower or lower.startswith('ok') or 'days=' in lower:
        level = 'ok'
    # 其余形如 "<stage>: <error>" 保留为 error

    return {
        'ts': ts,
        'uid': uid,
        'kind': kind,
        'level': level,
        'credits': credits,
        'message': rest,
        # 同一条容器日志行的时间戳精确到纳秒，配合内容即可唯一标识，
        # 因此反复采集不会重复入库
        'dedup_key': hashlib.sha1(f'{ts}|{kind}|{uid}|{rest}'.encode('utf-8')).hexdigest(),
    }


def _iso_to_epoch(s: str) -> int:
    txt = s.strip()
    if txt.endswith('Z'):
        txt = txt[:-1] + '+00:00'
    # 截掉纳秒到微秒（datetime 只支持 6 位）
    m = re.match(r'^(.*\.\d{6})\d*(.*)$', txt)
    if m:
        txt = m.group(1) + m.group(2)
    try:
        dt = datetime.fromisoformat(txt)
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        return int(dt.timestamp())
    except Exception:  # noqa: BLE001
        return 0


def parse_lines(lines: list[str]) -> list[dict]:
    out: list[dict] = []
    for ln in lines:
        ev = parse_line(ln)
        if ev:
            out.append(ev)
    return out


def strip_docker_ts(line: str) -> str:
    """去掉 docker --timestamps 加的时间前缀，只留应用自己的日志内容。"""
    m = _DOCKER_TS.match((line or '').rstrip('\r\n'))
    return m.group(2) if m else line


# ── 结果文案中文化 ───────────────────────────────────────
# 上游日志是英文原文，直接展示对中文用户不友好。这里在**展示层**翻译，
# 不动数据库里存的原文——排查问题时要能看到上游原话。
#   (匹配文本, 中文模板)  模板里的 {n} 会被捕获组依次填充
_MESSAGE_RULES: tuple[tuple[re.Pattern[str], str], ...] = (
    (re.compile(r'claim ok record=(\d+) reward=(\d+)'), '领奖成功：第 {0} 次行程，获得 {1} 积分'),
    (re.compile(r'adopt ok \(\+(\d+) credits?\)'), '领养成功：获得 {0} 积分'),
    (re.compile(r'depart ok location=(\d+)'), '已派出旅行（目的地 {0}）'),
    (re.compile(r'claim skipped \(arrived but no record_id\)'), '领奖跳过：已到站但无记录 ID'),
    (re.compile(r'claim record=(\d+):'), '领奖失败（行程 {0}）'),
    (re.compile(r'skip \(daily limit reached\)'), '跳过：今日次数已达上限'),
    (re.compile(r'skip \(traveling record=(\d+)\)'), '跳过：旅行进行中（行程 {0}）'),
    (re.compile(r'skip \(unknown state "([^"]*)"\)'), '跳过：状态未知（{0}）'),
    (re.compile(r'adopt skipped \(conversation threshold not reached, retry tomorrow\)'),
     '领养跳过：对话数未达门槛，明天重试'),
    (re.compile(r'report OK but streak\.days=0 \(silent drop\?\)'),
     '上报成功但连续天数仍为 0（疑似被静默丢弃）'),
    (re.compile(r'streak check failed \(report OK\):'), '连续天数校验失败（上报本身成功）'),
    (re.compile(r'streak days=(\d+)'), '连续登录 {0} 天'),
    (re.compile(r'keepalive ok expires=(\S+)'), '令牌保活成功（有效期 {0}）'),
    (re.compile(r'连续 (\d+) 次 12153 session dead — 禁用'), '连续 {0} 次会话失效，账号已禁用'),
    (re.compile(r'连续 (\d+) 次 12153 session dead'), '连续 {0} 次会话失效'),
)

_MESSAGE_EXACT = {
    'checkin ok code=0': '签到成功',
    'keepalive ok': '令牌保活成功',
    'buddy-info': '获取 Buddy 信息失败',
    'agreement': '签署协议失败',
    'adopt': '领养失败',
    'depart': '派出失败',
    'status': '查询旅行状态失败',
    'claim': '领奖失败',
}

# 「阶段名: 错误详情」的失败行：把阶段名换成中文，错误详情保留原文
_STAGE_LABELS = {
    'buddy-info': '获取 Buddy 信息失败',
    'agreement': '签署协议失败',
    'adopt': '领养失败',
    'depart': '派出失败',
    'status': '查询旅行状态失败',
    'claim': '领奖失败',
    'save': '保存令牌失败',
}

_TRUNC_RE = re.compile(r'^(.*?)\s*\.\.\.\s*（已截断）$')

# 常见技术错误的短语替换（作用在展示文案上，覆盖「阶段: <英文错误>」的详情部分）
_PHRASES: tuple[tuple[str, str], ...] = (
    ('context deadline exceeded', '请求超时'),
    ('Client.Timeout exceeded while awaiting headers', '等待响应头超时'),
    ('unexpected end of JSON input', '响应内容不完整（JSON 解析失败）'),
    ('connection refused', '连接被拒绝'),
    ('no such host', '域名解析失败'),
    ('i/o timeout', '网络超时'),
    ('EOF', '连接被提前关闭'),
)


def _apply_phrases(text: str) -> str:
    out = text
    for en, cn in _PHRASES:
        if en in out:
            out = out.replace(en, cn)
    return out


def translate_message(message: str) -> str:
    """把上游英文结果翻成中文；认不出的原样返回（并保留截断标记）。"""
    raw = (message or '').strip()
    if not raw:
        return raw

    # 被截断时先剥掉标记，翻完再补回；否则标记里的中文会干扰判断
    m_trunc = _TRUNC_RE.match(raw)
    body = m_trunc.group(1).strip() if m_trunc else raw
    suffix = ' …（已截断）' if m_trunc else ''

    # 本管理端自己写的中文流水（如「余额 +100（…）」）无需翻译
    if body.startswith('余额 '):
        return raw

    if body in _MESSAGE_EXACT:
        return _MESSAGE_EXACT[body] + suffix

    for pattern, tpl in _MESSAGE_RULES:
        m = pattern.search(body)
        if m:
            try:
                return tpl.format(*m.groups()) + suffix
            except (IndexError, KeyError):
                return tpl + suffix

    # 「阶段: 详情」形式（详情里的常见英文错误一并转中文）
    m_stage = re.match(r'^([a-z-]+):\s*(.+)$', body)
    if m_stage and m_stage.group(1) in _STAGE_LABELS:
        return _apply_phrases(f'{_STAGE_LABELS[m_stage.group(1)]}：{m_stage.group(2)}') + suffix

    # 单行无参数文案
    if body in _STAGE_LABELS:
        return _STAGE_LABELS[body] + suffix

    return _apply_phrases(raw)


# ── 后台采集 ─────────────────────────────────────────────
_collector: asyncio.Task | None = None
_last: dict = {'at': 0, 'added': 0, 'scanned': 0, 'error': ''}


def state() -> dict:
    return dict(_last)


# 每次回看的日志行数：上游会为每个请求打日志，行数消耗很快，
# 太小可能在两次轮询之间漏掉任务行；这里取一个明显大于 45 秒产出量的值。
TAIL_LINES = 3000


async def _collect_once(limit: int = TAIL_LINES) -> tuple[int, int]:
    """读一次容器日志并入库，返回 (解析到的任务行数, 新增条数)。"""
    lines = await asyncio.to_thread(wb2api.read_container_logs, limit, True)
    events = parse_lines(lines)
    if not events:
        return 0, 0
    added = await asyncio.to_thread(db.add_task_logs, events)
    return len(events), added


async def _loop() -> None:
    while True:
        try:
            parsed, added = await _collect_once()
            _last.update({
                'at': int(time.time()),
                'parsed': parsed,
                'added': added,
                'error': '',
            })
        except Exception as exc:  # noqa: BLE001
            _last.update({'at': int(time.time()), 'error': str(exc)[:200]})
        await asyncio.sleep(_POLL_SECONDS)


def start_collector() -> bool:
    """启动后台采集任务（幂等）。需要运行中的事件循环。"""
    global _collector
    try:
        loop = asyncio.get_running_loop()
    except RuntimeError:
        return False
    if _collector is None or _collector.done():
        _collector = loop.create_task(_loop())
    return True


def stop_collector() -> None:
    global _collector
    if _collector and not _collector.done():
        _collector.cancel()
    _collector = None
