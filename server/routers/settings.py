"""上游配置、模型映射与管理端用户。"""
from __future__ import annotations

from fastapi import APIRouter, Depends, HTTPException
from pydantic import BaseModel, Field

from .. import db, security
from ..services import reload, wb2api

router = APIRouter(prefix='/api', tags=['settings'])


# ── 上游配置 ─────────────────────────────────────────────
@router.get('/settings/upstream')
def get_upstream(user: dict = Depends(security.current_user)) -> dict:
    return wb2api.load_upstream_config()


@router.post('/settings/upstream')
async def save_upstream(body: dict, user: dict = Depends(security.require_admin)) -> dict:
    # 必须是 async：同步路由会被 FastAPI 放进线程池执行，那里没有事件循环，
    # 无法调度后台重载任务（request_restart 将拿不到 running loop）。
    try:
        result = wb2api.save_upstream_config(body)
    except FileNotFoundError as exc:
        raise HTTPException(status_code=409, detail=str(exc)) from exc
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    # 上游只在启动时读 config.json，保存后自动重载使其生效
    result['reload_scheduled'] = reload.request_restart()
    return result


@router.get('/upstream/reload-state')
def reload_state(user: dict = Depends(security.current_user)) -> dict:
    """上游重载状态，供前端展示「正在应用配置」。"""
    return reload.state()


class UpstashTestIn(BaseModel):
    url: str = ''
    # 留空则使用配置文件中已保存的 token
    token: str | None = None


@router.post('/settings/upstash/test')
async def test_upstash(body: UpstashTestIn, user: dict = Depends(security.require_admin)) -> dict:
    """探测 Upstash 是否可用（走其 REST 接口 PING）。"""
    ok, message = await wb2api.test_upstash(body.url, body.token)
    return {'ok': ok, 'message': message}


@router.post('/settings/upstash/reload')
async def reload_upstream(user: dict = Depends(security.require_admin)) -> dict:
    """立即重启上游容器（等待结果）。一般无需手动调用——保存配置会自动重载。"""
    ok, message = await reload.restart_now()
    return {'ok': ok, 'message': message}


# ── 模型别名映射 ─────────────────────────────────────────
@router.get('/settings/model-map')
def get_model_map(user: dict = Depends(security.current_user)) -> dict:
    return db.get_setting('model_map', {}) or {}


@router.post('/settings/model-map')
def save_model_map(body: dict[str, str], user: dict = Depends(security.require_admin)) -> dict:
    clean = {str(k): str(v) for k, v in body.items() if str(k).strip() and str(v).strip()}
    db.set_setting('model_map', clean)
    return clean


# ── 管理端用户 ───────────────────────────────────────────
class UserIn(BaseModel):
    username: str = Field(min_length=1, max_length=32)
    password: str = Field(min_length=1)
    role: str = Field(pattern='^(admin|viewer)$')


class UserPatch(BaseModel):
    password: str | None = None
    role: str | None = Field(default=None, pattern='^(admin|viewer)$')


def _public(cfg: dict) -> list[dict]:
    return [{'username': u['username'], 'role': u.get('role', 'viewer')} for u in cfg.get('users', [])]


@router.get('/users')
def list_users(user: dict = Depends(security.current_user)) -> list[dict]:
    return _public(security.load_users())


@router.post('/users')
def add_user(body: UserIn, user: dict = Depends(security.require_admin)) -> dict:
    cfg = security.load_users()
    if any(u.get('username') == body.username for u in cfg.get('users', [])):
        raise HTTPException(status_code=409, detail='用户名已存在')
    cfg.setdefault('users', []).append(
        {'username': body.username, 'role': body.role, 'pwd_hash': security.make_hash(body.password)}
    )
    security.save_users(cfg)
    return {'username': body.username, 'role': body.role}


@router.patch('/users/{username}')
def update_user(username: str, body: UserPatch, user: dict = Depends(security.require_admin)) -> dict:
    cfg = security.load_users()
    target = next((u for u in cfg.get('users', []) if u.get('username') == username), None)
    if not target:
        raise HTTPException(status_code=404, detail='用户不存在')

    if body.password:
        target['pwd_hash'] = security.make_hash(body.password)
    if body.role and body.role != target.get('role'):
        admins = [u for u in cfg.get('users', []) if u.get('role') == 'admin']
        if target.get('role') == 'admin' and len(admins) <= 1:
            raise HTTPException(status_code=400, detail='至少保留一个管理员')
        target['role'] = body.role

    security.save_users(cfg)
    return {'username': target['username'], 'role': target.get('role', 'viewer')}


@router.delete('/users/{username}')
def delete_user(username: str, user: dict = Depends(security.require_admin)) -> dict:
    cfg = security.load_users()
    users = cfg.get('users', [])
    target = next((u for u in users if u.get('username') == username), None)
    if not target:
        raise HTTPException(status_code=404, detail='用户不存在')
    admins = [u for u in users if u.get('role') == 'admin']
    if target.get('role') == 'admin' and len(admins) <= 1:
        raise HTTPException(status_code=400, detail='至少保留一个管理员')
    if target.get('username') == user.get('username'):
        raise HTTPException(status_code=400, detail='不能删除当前登录用户')
    cfg['users'] = [u for u in users if u.get('username') != username]
    security.save_users(cfg)
    return {'ok': True}
