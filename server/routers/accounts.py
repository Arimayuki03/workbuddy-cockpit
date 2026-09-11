"""账号管理：列表、扫码添加、签到、测试、刷新、删除、重启上游。"""
from __future__ import annotations

from fastapi import APIRouter, Depends, HTTPException

from .. import security
from ..services import tencent, wb2api

router = APIRouter(prefix='/api', tags=['accounts'])


@router.get('/accounts')
def list_accounts(user: dict = Depends(security.current_user)) -> dict:
    accounts = wb2api.list_auth_accounts()
    return {'total': len(accounts), 'accounts': accounts}


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
    await tencent.checkin(result['access_token'])

    filename, existed = tencent.write_auth_file(result)

    # 异步重启容器加载新账号
    await wb2api.restart_container()

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
    token = (raw.get('auth') or {}).get('accessToken', '')
    if not token:
        return {'code': -1, 'message': '该账号无有效 accessToken'}
    code, message = await tencent.checkin(token)
    return {'code': code, 'message': message}


@router.post('/accounts/{filename}/test')
async def account_test(filename: str, user: dict = Depends(security.require_admin)) -> dict:
    raw = _load(filename)
    token = (raw.get('auth') or {}).get('accessToken', '')
    if not token:
        return {'ok': False, 'message': '该账号无有效 accessToken'}
    ok, message = await tencent.probe_account(token)
    return {'ok': ok, 'message': message}


@router.post('/accounts/{filename}/refresh')
async def account_refresh(filename: str, user: dict = Depends(security.require_admin)) -> dict:
    raw = _load(filename)
    if not (raw.get('auth') or {}).get('accessToken'):
        return {'ok': False, 'message': '该账号无有效 accessToken'}
    ok, message = await wb2api.restart_container()
    return {'ok': ok, 'message': '已触发上游重载以刷新 Token' if ok else message}


@router.delete('/accounts/{filename}')
async def account_delete(filename: str, user: dict = Depends(security.require_admin)) -> dict:
    try:
        removed = wb2api.delete_auth_account(filename)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    if not removed:
        raise HTTPException(status_code=404, detail='账号文件不存在')
    await wb2api.restart_container()
    return {'success': True}


@router.post('/restart')
async def restart(user: dict = Depends(security.require_admin)) -> dict:
    ok, message = await wb2api.restart_container()
    return {'ok': ok, 'message': message}
