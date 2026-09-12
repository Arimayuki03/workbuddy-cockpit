"""用量统计聚合。"""
from __future__ import annotations

import time

from fastapi import APIRouter, Depends

from .. import db, security

router = APIRouter(prefix='/api/stats', tags=['stats'])


def _since(days: int) -> str:
    return time.strftime('%Y-%m-%d', time.localtime(time.time() - (days - 1) * 86400))


@router.get('/summary')
def summary(user: dict = Depends(security.current_user)) -> dict:
    today = time.strftime('%Y-%m-%d')
    week = _since(7)

    def agg(where: str, args: tuple) -> tuple[int, int]:
        row = db.query_one(
            f'SELECT COALESCE(SUM(requests),0) AS r, '
            f'COALESCE(SUM(prompt_tokens + completion_tokens),0) AS t FROM usage_daily WHERE {where}',
            args,
        )
        return int(row['r']), int(row['t'])

    t_req, t_tok = agg('day = ?', (today,))
    w_req, w_tok = agg('day >= ?', (week,))
    a_req, a_tok = agg('1=1', ())

    active_keys = db.query_one('SELECT COUNT(*) AS c FROM api_keys WHERE enabled = 1')['c']
    top = db.query_one(
        'SELECT model, SUM(requests + prompt_tokens + completion_tokens) AS score '
        'FROM usage_daily GROUP BY model ORDER BY score DESC LIMIT 1'
    )
    return {
        'today_requests': t_req,
        'today_tokens': t_tok,
        'week_requests': w_req,
        'week_tokens': w_tok,
        'total_requests': a_req,
        'total_tokens': a_tok,
        'active_keys': int(active_keys),
        'top_model': top['model'] if top else None,
    }


@router.post('/repair-usage')
def repair_usage(user: dict = Depends(security.require_admin)) -> dict:
    """按请求日志回填用量统计的缺口（幂等，可重复执行）。

    用于修复历史缺陷导致的部分调用未计入统计。
    """
    return db.backfill_usage_from_logs()


@router.post('/rebuild-usage')
def rebuild_usage(user: dict = Depends(security.require_admin)) -> dict:
    """以请求日志为准重建用量统计（清理时区口径不一致造成的重复计数）。

    与 /repair-usage 的区别：repair 只补缺口（增量、幂等），
    本接口是**重建**——会替换 usage_daily 的内容，能删除此前多出来的行。
    """
    return db.rebuild_usage_from_logs()


@router.get('/daily')
def daily(days: int = 30, user: dict = Depends(security.current_user)) -> list[dict]:
    rows = db.query(
        'SELECT day, SUM(requests) AS requests, '
        'SUM(prompt_tokens) AS prompt_tokens, SUM(completion_tokens) AS completion_tokens '
        'FROM usage_daily WHERE day >= ? GROUP BY day ORDER BY day ASC',
        (_since(max(1, days)),),
    )
    return [
        {
            'day': r['day'],
            'requests': int(r['requests'] or 0),
            'prompt_tokens': int(r['prompt_tokens'] or 0),
            'completion_tokens': int(r['completion_tokens'] or 0),
        }
        for r in rows
    ]


@router.get('/by-model')
def by_model(days: int = 30, user: dict = Depends(security.current_user)) -> list[dict]:
    rows = db.query(
        'SELECT model AS name, SUM(requests) AS requests, '
        'SUM(prompt_tokens) AS prompt_tokens, SUM(completion_tokens) AS completion_tokens '
        'FROM usage_daily WHERE day >= ? GROUP BY model ORDER BY SUM(prompt_tokens + completion_tokens) DESC',
        (_since(max(1, days)),),
    )
    return [
        {
            'name': r['name'] or '未知',
            'requests': int(r['requests'] or 0),
            'prompt_tokens': int(r['prompt_tokens'] or 0),
            'completion_tokens': int(r['completion_tokens'] or 0),
        }
        for r in rows
    ]


@router.get('/by-key')
def by_key(days: int = 30, user: dict = Depends(security.current_user)) -> list[dict]:
    rows = db.query(
        'SELECT COALESCE(k.name, u.key_id || "") AS name, SUM(u.requests) AS requests, '
        'SUM(u.prompt_tokens) AS prompt_tokens, SUM(u.completion_tokens) AS completion_tokens '
        'FROM usage_daily u LEFT JOIN api_keys k ON k.id = u.key_id '
        'WHERE u.day >= ? GROUP BY u.key_id ORDER BY SUM(u.prompt_tokens + u.completion_tokens) DESC',
        (_since(max(1, days)),),
    )
    return [
        {
            'name': r['name'] or '未知',
            'requests': int(r['requests'] or 0),
            'prompt_tokens': int(r['prompt_tokens'] or 0),
            'completion_tokens': int(r['completion_tokens'] or 0),
        }
        for r in rows
    ]
