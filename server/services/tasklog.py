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


# ── 后台采集 ─────────────────────────────────────────────
_collector: asyncio.Task | None = None
_last: dict = {'at': 0, 'added': 0, 'scanned': 0, 'error': ''}


def state() -> dict:
    return dict(_last)


async def _collect_once(limit: int = 800) -> tuple[int, int]:
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
