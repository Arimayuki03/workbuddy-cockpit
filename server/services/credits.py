"""实时积分的短期缓存。

为什么要缓存：积分需要直接调腾讯接口（上游 /status 的 credits 只在它自己的
定时任务里刷新，滞后可达数小时）。但每次打开页面都全量查询会给腾讯带来
不必要的请求，账号多时更明显，也可能触发限流。

因此加一层 TTL 缓存：TTL 内的重复查询直接命中缓存，超过 TTL 才真正请求。
手动「刷新积分」可传 force=True 绕过缓存。
"""
from __future__ import annotations

import time

from . import tencent

# uid -> (查询时间戳, 是否成功, 积分值, 消息)
_cache: dict[str, tuple[float, bool, int | float | None, str]] = {}
TTL_SECONDS = 60


def cached_credits(uid: str) -> tuple[bool, int | float | None, str] | None:
    """命中未过期的缓存则返回，否则 None。"""
    item = _cache.get(uid)
    if not item:
        return None
    ts, ok, credits, message = item
    if time.time() - ts > TTL_SECONDS:
        return None
    return ok, credits, message


async def get_credits(
    auth: dict,
    *,
    force: bool = False,
) -> tuple[bool, int | float | None, str]:
    """查询积分（带 TTL 缓存）。force=True 时跳过缓存。"""
    uid = str(auth.get('uid') or '')
    if uid and not force:
        hit = cached_credits(uid)
        if hit is not None:
            ok, credits, message = hit
            return ok, credits, f'{message}（缓存）'

    ok, credits, message = await tencent.fetch_credits(auth)
    if uid:
        # 仅缓存成功结果：失败往往是临时网络问题，不该被缓存住
        if ok:
            _cache[uid] = (time.time(), ok, credits, message)
        else:
            _cache.pop(uid, None)
    return ok, credits, message


def invalidate(uid: str | None = None) -> None:
    """清除缓存（uid 为空则全清）。签到等会改变余额的操作后调用。"""
    if uid:
        _cache.pop(uid, None)
    else:
        _cache.clear()
