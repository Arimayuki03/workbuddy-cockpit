"""管理端鉴权与会话。"""
from __future__ import annotations

from fastapi import APIRouter, Depends, HTTPException, Request
from fastapi.responses import JSONResponse

from .. import config, security
from ..iputil import client_ip

router = APIRouter(prefix='/api', tags=['auth'])


@router.get('/healthz')
def healthz() -> dict:
    return {'ok': True, 'service': 'workbuddy-manager'}


@router.post('/login')
async def login(request: Request) -> JSONResponse:
    ip = client_ip(request)
    if security.login_blocked(ip):
        raise HTTPException(status_code=429, detail='失败次数过多，请 10 分钟后再试')

    body = await request.json()
    username = str(body.get('username', '')).strip()
    password = str(body.get('password', ''))

    cfg = security.load_users()
    user = next((u for u in cfg.get('users', []) if u.get('username') == username), None)
    if not user or not security.verify_pwd(password, user.get('pwd_hash', '')):
        security.record_fail(ip)
        raise HTTPException(status_code=401, detail='用户名或密码错误')

    security.clear_fail(ip)
    token = security.issue_token(username, user.get('role', 'viewer'))
    resp = JSONResponse({'ok': True, 'username': username, 'role': user.get('role', 'viewer')})
    resp.set_cookie(
        config.COOKIE_NAME,
        token,
        max_age=config.SESSION_DAYS * 86400,
        httponly=True,
        samesite='lax',
        secure=security.cookie_secure(request),
        path='/',
    )
    return resp


@router.post('/logout')
def logout() -> JSONResponse:
    resp = JSONResponse({'ok': True})
    resp.delete_cookie(config.COOKIE_NAME, path='/')
    return resp


@router.get('/me')
def me(user: dict = Depends(security.current_user)) -> dict:
    return {'username': user.get('username'), 'role': user.get('role', 'viewer')}
