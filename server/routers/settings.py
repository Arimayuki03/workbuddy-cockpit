"""上游配置、模型映射与管理端用户。"""
from __future__ import annotations

from fastapi import APIRouter, Depends, HTTPException
from pydantic import BaseModel, Field

from .. import db, security
from ..services import wb2api

router = APIRouter(prefix='/api', tags=['settings'])


# ── 上游配置 ─────────────────────────────────────────────
@router.get('/settings/upstream')
def get_upstream(user: dict = Depends(security.current_user)) -> dict:
    return wb2api.load_upstream_config()


@router.post('/settings/upstream')
def save_upstream(body: dict, user: dict = Depends(security.require_admin)) -> dict:
    try:
        return wb2api.save_upstream_config(body)
    except FileNotFoundError as exc:
        raise HTTPException(status_code=409, detail=str(exc)) from exc
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc


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
