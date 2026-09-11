"""账号管理：列表、扫码添加、签到、测试、刷新、删除、重启上游。"""
from __future__ import annotations

from fastapi import APIRouter, Depends, HTTPException

from .. import db, security
from ..services import reload, tencent, wb2api

router = APIRouter(prefix='/api', tags=['accounts'])


@router.get('/accounts')
async def list_accounts(user: dict = Depends(security.current_user)) -> dict:
    """账号列表：本地授权信息 + 上游运行时状态（含积分余额）。"""
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
    token = (raw.get('auth') or {}).get('accessToken', '')
    if not token:
        db.add_checkin_log(
            str(acct.get('uid', '')), str(acct.get('nickname', '')),
            'manual', False, None, '该账号无有效 accessToken',
        )
        return {'code': -1, 'message': '该账号无有效 accessToken'}

    code, message = await tencent.checkin(token)
    # 0 = 签到成功；10001 = 今日已签到，同样视为成功
    ok = code in (0, 10001)
    db.add_checkin_log(
        str(acct.get('uid', '')), str(acct.get('nickname', '')),
        'manual', ok, code, message,
    )
    return {'code': code, 'message': message}


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


@router.get('/upstream/logs')
def upstream_logs(limit: int = 200, user: dict = Depends(security.current_user)) -> dict:
    """上游容器日志中与签到/保活相关的行。

    上游成功签到不打日志，因此这里主要呈现失败与保活记录；
    成功与否可结合「签到记录」中的手动结果与账号 credits 判断。
    """
    lines = wb2api.read_container_logs(limit=limit)
    keywords = ('checkin', 'keepalive', 'user-resource')
    interesting = [
        ln for ln in lines
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
