"""账号管理：列表、扫码添加、签到、测试、刷新、删除、重启上游。"""
from __future__ import annotations

import asyncio

from fastapi import APIRouter, Depends, HTTPException

from .. import db, security
from ..services import credits as creditsvc, reload, tasklog, tencent, wb2api

router = APIRouter(prefix='/api', tags=['accounts'])


@router.get('/accounts')
async def list_accounts(user: dict = Depends(security.current_user)) -> dict:
    """账号列表：本地授权信息 + 上游运行时状态（含积分余额）。

    积分（credits）优先使用上游 /status 的值：它是上游调度时写入的快照，
    与账号可用性判定一致，开销也小。前端可用「刷新积分」触发实时查询。
    """
    accounts = wb2api.list_auth_accounts()
    status = await wb2api.get_status()
    wb2api.merge_pool_status(accounts, status)
    synced = sum(1 for a in accounts if a.get('credits') is not None)
    return {
        'total': len(accounts),
        'accounts': accounts,
        'pool_synced': synced,
        'pool_available': bool(status.get('connected')),
    }


@router.get('/status')
async def upstream_status(user: dict = Depends(security.current_user)) -> dict:
    return await wb2api.get_status()


@router.get('/models')
async def models(user: dict = Depends(security.current_user)) -> list | dict:
    ok, data = await wb2api.get_models()
    if not ok:
        raise HTTPException(status_code=502, detail=str(data))
    return data


@router.post('/auth/start')
async def auth_start(user: dict = Depends(security.require_admin)) -> dict:
    try:
        return await tencent.start_login()
    except RuntimeError as exc:
        raise HTTPException(status_code=502, detail=str(exc)) from exc


@router.get('/auth/poll')
async def auth_poll(state: str, user: dict = Depends(security.require_admin)) -> dict:
    if not state:
        return {'status': 'invalid'}

    result = await tencent.poll_login(state)
    if result.get('status') != 'ready':
        return result

    # 自动签到（幂等，不阻断落盘）
    code, message = await tencent.checkin(result['access_token'])
    db.add_checkin_log(
        str(result.get('uid', '')), str(result.get('nickname', '')),
        'add', code in (0, 10001), code, message,
    )

    creditsvc.invalidate(str(result.get('uid', '')))
    filename, existed = tencent.write_auth_file(result)

    # 自动重载上游以加载新账号（后台合并执行，不阻塞本次响应）
    reload.request_restart()

    return {
        'status': 'success',
        'uid': result['uid'],
        'nickname': result['nickname'],
        'updated': existed,
        'file': filename,
    }


def _auth_dict(raw: dict) -> dict:
    """把授权文件内容整理成探测 / 查询积分所需的字段。"""
    acct = raw.get('account') or {}
    auth = raw.get('auth') or {}
    return {
        'access_token': auth.get('accessToken', ''),
        'uid': acct.get('uid', ''),
        'enterprise_id': acct.get('enterpriseId', ''),
        'domain': auth.get('domain', ''),
    }


def _load(filename: str) -> dict:
    try:
        return wb2api.read_account_file(filename)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    except FileNotFoundError as exc:
        raise HTTPException(status_code=404, detail='账号文件不存在') from exc


@router.post('/accounts/{filename}/checkin')
async def account_checkin(filename: str, user: dict = Depends(security.require_admin)) -> dict:
    raw = _load(filename)
    acct = raw.get('account') or {}
    auth = raw.get('auth') or {}
    token = auth.get('accessToken', '')
    uid = str(acct.get('uid', ''))
    nickname = str(acct.get('nickname', ''))

    if not token:
        db.add_checkin_log(uid, nickname, 'manual', False, None, '该账号无有效 accessToken')
        return {'code': -1, 'message': '该账号无有效 accessToken'}

    code, message = await tencent.checkin(token)
    # 0 = 签到成功；10001 = 今日已签到，同样视为成功
    ok = code in (0, 10001)
    db.add_checkin_log(uid, nickname, 'manual', ok, code, message)

    # 签到后顺带查实时积分：上游只在它自己的定时任务里刷新 credits，
    # 手动签到不会带动它更新，所以这里主动查一次返回给前端。
    credits: int | float | None = None
    if ok:
        # 签到会改变余额，先失效缓存再查实时值
        creditsvc.invalidate(uid)
        _, credits, _, _, _ = await creditsvc.get_credits(
            _auth_dict(raw), nickname=str(acct.get('nickname') or ''),
        )

    return {'code': code, 'message': message, 'credits': credits}


@router.get('/accounts/{filename}/credits')
async def account_credits(
    filename: str,
    force: bool = False,
    user: dict = Depends(security.current_user),
) -> dict:
    """查询单个账号的实时积分余额（直接向腾讯查询，带 60s 缓存）。

    force=true 可绕过缓存强制查询。
    """
    raw = _load(filename)
    acct = raw.get('account') or {}
    ok, value, message, cached, age = await creditsvc.get_credits(
        _auth_dict(raw), force=force, nickname=str(acct.get('nickname') or ''),
    )
    return {'ok': ok, 'credits': value, 'message': message, 'cached': cached, 'cache_age': age}


@router.post('/accounts/refresh-credits')
async def refresh_all_credits(
    force: bool = True,
    user: dict = Depends(security.current_user),
) -> dict:
    """并发查询所有账号的积分，返回 {uid: credits} 与每条是否来自缓存。

    上游 /status 的 credits 只在它定时任务时更新，可能滞后数小时；
    本接口直接向腾讯查询。force=true（默认）用于「刷新积分」按钮，
    强制绕过 60 秒缓存；force=false 用于页面加载，命中缓存时不重复请求腾讯。
    无论哪种，都回传 cached / cache_age，前端据此标注「实时 / 缓存」。
    """
    accounts = wb2api.list_auth_accounts()

    async def one(acc: dict) -> tuple[str, int | float | None, str, bool, int | None]:
        try:
            raw = wb2api.read_account_file(acc['file'])
        except Exception as exc:  # noqa: BLE001
            return acc['uid'], None, f'读取失败: {exc}', False, None
        ok, value, message, cached, age = await creditsvc.get_credits(
            _auth_dict(raw), force=force, nickname=str(acc.get('nickname') or ''),
        )
        return acc['uid'], value if ok else None, message, cached, age

    results = await asyncio.gather(*(one(a) for a in accounts)) if accounts else []

    credits_map: dict[str, int | float | None] = {}
    meta: dict[str, dict] = {}
    failed: list[str] = []
    for uid, credits, message, cached, age in results:
        credits_map[uid] = credits
        meta[uid] = {'cached': cached, 'cache_age': age, 'message': message}
        if credits is None:
            failed.append(f'{uid[:12]}: {message}')

    return {
        'total': len(accounts),
        'succeeded': len(accounts) - len(failed),
        'credits': credits_map,
        'meta': meta,
        'failed': failed,
    }


@router.post('/accounts/checkin-all')
async def checkin_all(user: dict = Depends(security.require_admin)) -> dict:
    """对所有账号执行一次签到，并逐条记录结果。

    上游的自动签到只在失败时打日志、成功静默，且没有可触发的 HTTP 接口；
    这里用管理端自己的签到实现补齐「可手动触发 + 可追溯」。
    """
    results: list[dict] = []
    for acc in wb2api.list_auth_accounts():
        filename = acc['file']
        uid = acc.get('uid', '')
        nickname = acc.get('nickname', '')
        try:
            raw = wb2api.read_account_file(filename)
            token = (raw.get('auth') or {}).get('accessToken', '')
        except Exception as exc:  # noqa: BLE001
            db.add_checkin_log(uid, nickname, 'manual-batch', False, None, f'读取失败: {exc}')
            results.append({'nickname': nickname, 'ok': False, 'message': f'读取失败: {exc}'})
            continue

        if not token:
            msg = '无有效 accessToken'
            db.add_checkin_log(uid, nickname, 'manual-batch', False, None, msg)
            results.append({'nickname': nickname, 'ok': False, 'message': msg})
            continue

        code, message = await tencent.checkin(token)
        ok = code in (0, 10001)
        db.add_checkin_log(uid, nickname, 'manual-batch', ok, code, message)
        results.append({'nickname': nickname, 'ok': ok, 'code': code, 'message': message})

    succeeded = sum(1 for r in results if r['ok'])
    return {'total': len(results), 'succeeded': succeeded, 'results': results}


@router.get('/checkin-logs')
def checkin_logs(
    limit: int = 200,
    uid: str | None = None,
    user: dict = Depends(security.current_user),
) -> list[dict]:
    return db.list_checkin_logs(limit=limit, uid=uid)


@router.post('/checkin-logs/clear')
def clear_checkin_logs(user: dict = Depends(security.require_admin)) -> dict:
    db.clear_checkin_logs()
    return {'ok': True}


@router.get('/task-logs')
def task_logs(
    limit: int = 200,
    uid: str | None = None,
    kind: str | None = None,
    user: dict = Depends(security.current_user),
) -> dict:
    """上游自动任务留痕（猫猫旅行 / 活跃上报 / 自动签到 / 保活）。

    上游把这些结果打在容器日志里，容器重建即丢失；本接口读取的是
    后台采集器解析后落库的记录，因此能长期保留并统计积分收益。
    """
    logs = db.list_task_logs(limit=limit, uid=uid, kind=kind)
    # 结果文案中文化：数据库留英文原文（排查要看上游原话），
    # 接口额外给出 message_cn 供界面展示
    for row in logs:
        row['message_cn'] = tasklog.translate_message(row.get('message', ''))
    return {
        'logs': logs,
        'stats': db.task_log_stats(),
        'kinds': tasklog.KIND_LABELS,
        'collector': tasklog.state(),
    }


@router.post('/task-logs/collect')
async def collect_task_logs(user: dict = Depends(security.require_admin)) -> dict:
    """立即采集一次（不等后台轮询），便于刚跑完任务就看结果。"""
    parsed, added = await tasklog._collect_once()
    return {'ok': True, 'parsed': parsed, 'added': added}


@router.post('/task-logs/clear')
def clear_task_logs(user: dict = Depends(security.require_admin)) -> dict:
    db.clear_task_logs()
    return {'ok': True}


@router.get('/upstream/logs')
def upstream_logs(limit: int = 200, user: dict = Depends(security.current_user)) -> dict:
    """上游容器日志中与自动任务相关的行（原始日志）。

    上游只在失败时打日志、成功大多静默（旅行与活跃上报除外，它们会记录
    积分与服务端判据）。结构化、可长期保留的记录见 /api/task-logs。
    """
    lines = wb2api.read_container_logs(limit=limit)
    keywords = ('checkin', 'keepalive', 'user-resource', 'travel', 'activity')
    interesting = [
        # 去掉 docker --timestamps 前缀（上游自己已带秒级时间，重复显示反而难读）
        tasklog.strip_docker_ts(ln)
        for ln in lines
        if any(k in ln.lower() for k in keywords)
    ]
    return {'available': lines != [], 'lines': interesting, 'total': len(lines)}


@router.post('/accounts/{filename}/test')
async def account_test(filename: str, user: dict = Depends(security.require_admin)) -> dict:
    raw = _load(filename)
    acct = raw.get('account') or {}
    auth = raw.get('auth') or {}
    # 探测需要 uid / enterpriseId / domain 以复刻上游请求头
    ok, message = await tencent.probe_account({
        'access_token': auth.get('accessToken', ''),
        'uid': acct.get('uid', ''),
        'enterprise_id': acct.get('enterpriseId', ''),
        'domain': auth.get('domain', ''),
    })
    return {'ok': ok, 'message': message}


@router.post('/accounts/{filename}/refresh')
async def account_refresh(filename: str, user: dict = Depends(security.require_admin)) -> dict:
    raw = _load(filename)
    if not (raw.get('auth') or {}).get('accessToken'):
        return {'ok': False, 'message': '该账号无有效 accessToken'}
    ok, message = await reload.restart_now()
    return {'ok': ok, 'message': '已触发上游重载以刷新 Token' if ok else message}


@router.delete('/accounts/{filename}')
async def account_delete(filename: str, user: dict = Depends(security.require_admin)) -> dict:
    try:
        removed = wb2api.delete_auth_account(filename)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    if not removed:
        raise HTTPException(status_code=404, detail='账号文件不存在')
    reload.request_restart()
    return {'success': True}


@router.post('/restart')
async def restart(user: dict = Depends(security.require_admin)) -> dict:
    ok, message = await reload.restart_now()
    return {'ok': ok, 'message': message}
