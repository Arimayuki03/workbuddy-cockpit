"""用量统计聚合。"""
from __future__ import annotations

import time

from fastapi import APIRouter, Depends

from .. import db, security

router = APIRouter(prefix='/api/stats', tags=['stats'])


# days 的取值范围。**必须有上限**：超大整数在 SQLite 绑定时溢出抛错
# （实测 /api/logs?days=999999999999999 返回 500）。
# 10 年足够覆盖任何正常查询，同时避免溢出与全表扫描。
_DAYS_MAX = 3650


def _clamp_days(days: int | None, default: int) -> int:
    """把 days 钳到 [1, _DAYS_MAX]；非法值退回默认。"""
    try:
        d = int(days) if days is not None else default
    except (TypeError, ValueError):
        return default
    return min(_DAYS_MAX, max(1, d))


def _since(days: int) -> str:
    d = _clamp_days(days, 30)
    return time.strftime('%Y-%m-%d', time.localtime(time.time() - (d - 1) * 86400))


@router.get('/summary')
def summary(realm: str | None = None,
            user: dict = Depends(security.current_user)) -> dict:
    """总览。

    realm（cn / global）非空时只统计该版本——界面按版本切换时用。

    附带的 `usage_health` 用于暴露「统计根本没在累计」这种情况：曾经有个缺陷
    （issue #9）让存量库上一行都写不进 usage_daily，而表现是完全静默的
    （页面照常轮询、数字只是不动），拖了几个版本才被发现。判据是**今天的
    请求日志有内容但今天的统计为 0**——正常部署不该出现这种组合。
    """
    today = time.strftime('%Y-%m-%d')
    week = _since(7)
    # 过滤片段：realm 为空不过滤（保持既有行为），非空则按列匹配
    rf = ' AND realm = ?' if realm in ('cn', 'global') else ''
    rargs: tuple = (realm,) if rf else ()

    def agg(where: str, args: tuple) -> tuple[int, int, float]:
        row = db.query_one(
            f'SELECT COALESCE(SUM(requests),0) AS r, '
            f'COALESCE(SUM(prompt_tokens + completion_tokens),0) AS t, '
            f'COALESCE(SUM(credit),0) AS c FROM usage_daily WHERE {where}{rf}',
            args + rargs,
        )
        return int(row['r']), int(row['t']), float(row['c'] or 0)

    t_req, t_tok, t_credit = agg('day = ?', (today,))
    w_req, w_tok, w_credit = agg('day >= ?', (week,))
    a_req, a_tok, a_credit = agg('1=1', ())

    # 活跃密钥：密钥本身**不分版本**（同一把密钥可调两个版本，由模型名决定路由），
    # 所以按版本时应统计「**该版本有调用**的密钥数」，而不是「属于该版本的密钥数」
    # ——后者在架构上不存在。如实标注口径，避免让人以为密钥是隔离的。
    if realm in ('cn', 'global'):
        active_keys = db.query_one(
            'SELECT COUNT(DISTINCT key_id) AS c FROM usage_daily WHERE realm = ?',
            (realm,),
        )['c']
    else:
        active_keys = db.query_one('SELECT COUNT(*) AS c FROM api_keys WHERE enabled = 1')['c']
    top = db.query_one(
        'SELECT model, SUM(requests + prompt_tokens + completion_tokens) AS score '
        f'FROM usage_daily WHERE 1=1{rf} GROUP BY model ORDER BY score DESC LIMIT 1',
        rargs,
    )
    return {
        'today_requests': t_req,
        'today_tokens': t_tok,
        # 实际扣费（上游 usage.credit 合计）；上游未返回该字段时恒为 0
        'today_credit': t_credit,
        'week_credit': w_credit,
        'total_credit': a_credit,
        'week_requests': w_req,
        'week_tokens': w_tok,
        'total_requests': a_req,
        'total_tokens': a_tok,
        'active_keys': int(active_keys),
        'top_model': top['model'] if top else None,
        'usage_health': _usage_health(today, t_req),
    }


def _usage_health(today: str, today_requests: int) -> dict:
    """检测「用量统计没有在累计」——今天有请求日志但统计为 0。

    为什么值得单独做：统计写入是旁路（失败只记 warning，不影响转发），
    所以一旦写入路径坏了，**用户侧完全看不出异常**：页面照常刷新、数字只是
    停着不动。issue #9 就是这个形态，拖了几个版本才有人发现。

    正常部署不可能出现「今天有请求但今天零统计」——两者写在同一段代码里，
    前者成功后者必成功（bump_usage 紧随日志落库之后）。
    只在**两边都非零**时才下结论，避免刚部署/刚清库的误报。
    """
    try:
        logs_today = db.query_one(
            f'SELECT COUNT(*) AS c FROM request_logs WHERE {db.day_sql("ts")} = ?',
            (today,),
        )
        n = int(logs_today['c']) if logs_today else 0
    except Exception:  # noqa: BLE001
        return {'ok': True, 'detail': ''}
    if n > 0 and today_requests == 0:
        return {
            'ok': False,
            'detail': (f'今天已有 {n} 次调用记录，但用量统计为 0——统计可能没有 '
                       f'正常写入。可尝试「修复统计」；若仍为 0，请查看服务端日志。'),
        }
    return {'ok': True, 'detail': ''}


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
def daily(days: int = 30, realm: str | None = None,
          user: dict = Depends(security.current_user)) -> list[dict]:
    """按天聚合。realm 非空时只统计该版本。"""
    rf = ' AND realm = ?' if realm in ('cn', 'global') else ''
    args = (_since(max(1, days)),) + ((realm,) if rf else ())
    rows = db.query(
        'SELECT day, SUM(requests) AS requests, '
        'SUM(prompt_tokens) AS prompt_tokens, SUM(completion_tokens) AS completion_tokens, '
        'COALESCE(SUM(credit),0) AS credit '
        f'FROM usage_daily WHERE day >= ?{rf} GROUP BY day ORDER BY day ASC',
        args,
    )
    return [
        {
            'day': r['day'],
            'requests': int(r['requests'] or 0),
            'prompt_tokens': int(r['prompt_tokens'] or 0),
            'completion_tokens': int(r['completion_tokens'] or 0),
            'credit': float(r['credit'] or 0),
        }
        for r in rows
    ]


@router.get('/by-model')
def by_model(days: int = 30, realm: str | None = None,
             user: dict = Depends(security.current_user)) -> list[dict]:
    """按模型聚合。realm 非空时只统计该版本。"""
    rf = ' AND realm = ?' if realm in ('cn', 'global') else ''
    args = (_since(max(1, days)),) + ((realm,) if rf else ())
    rows = db.query(
        'SELECT model AS name, SUM(requests) AS requests, '
        'SUM(prompt_tokens) AS prompt_tokens, SUM(completion_tokens) AS completion_tokens, '
        'COALESCE(SUM(credit),0) AS credit '
        f'FROM usage_daily WHERE day >= ?{rf} GROUP BY model ORDER BY SUM(prompt_tokens + completion_tokens) DESC',
        args,
    )
    return [
        {
            'name': r['name'] or '未知',
            'requests': int(r['requests'] or 0),
            'prompt_tokens': int(r['prompt_tokens'] or 0),
            'completion_tokens': int(r['completion_tokens'] or 0),
            'credit': float(r['credit'] or 0),
        }
        for r in rows
    ]


@router.get('/by-key')
def by_key(days: int = 30, realm: str | None = None,
           user: dict = Depends(security.current_user)) -> list[dict]:
    """按密钥聚合。realm 非空时只统计该版本。

    密钥本身不分版本（同一把密钥可调两个版本），这里筛的是「该密钥在该版本上
    的调用量」——所以同一把密钥在两个版本下都会出现，数字各是各的。这是如实
    反映架构，不是重复计数。
    """
    rf = ' AND u.realm = ?' if realm in ('cn', 'global') else ''
    args = (_since(max(1, days)),) + ((realm,) if rf else ())
    rows = db.query(
        'SELECT COALESCE(k.name, u.key_id || "") AS name, SUM(u.requests) AS requests, '
        'SUM(u.prompt_tokens) AS prompt_tokens, SUM(u.completion_tokens) AS completion_tokens, '
        'COALESCE(SUM(u.credit),0) AS credit '
        'FROM usage_daily u LEFT JOIN api_keys k ON k.id = u.key_id '
        f'WHERE u.day >= ?{rf} GROUP BY u.key_id ORDER BY SUM(u.prompt_tokens + u.completion_tokens) DESC',
        args,
    )
    return [
        {
            'name': r['name'] or '未知',
            'requests': int(r['requests'] or 0),
            'prompt_tokens': int(r['prompt_tokens'] or 0),
            'completion_tokens': int(r['completion_tokens'] or 0),
            'credit': float(r['credit'] or 0),
        }
        for r in rows
    ]
