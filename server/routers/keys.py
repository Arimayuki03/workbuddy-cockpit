"""API 密钥管理接口。"""
from __future__ import annotations

from fastapi import APIRouter, Depends, HTTPException, Request
from pydantic import BaseModel, Field

from .. import keysvc, security
from ..iputil import client_ip

router = APIRouter(prefix='/api/keys', tags=['keys'])


class KeyIn(BaseModel):
    name: str = Field(min_length=1, max_length=64)
    expires_at: int | None = None
    max_ips: int = 0
    ip_allowlist: list[str] = Field(default_factory=list)
    models: list[str] = Field(default_factory=list)
    quota: int = 0
    # 版本归属：'' = 不限制（存量密钥的形态）。非 cn/global 的值由
    # keysvc._norm_realm 归一化成 ''——不报错，免得旧前端（不带该字段）被拒。
    realm: str = Field(default='', max_length=16)


class KeyPatch(BaseModel):
    name: str | None = None
    enabled: bool | None = None
    expires_at: int | None = None
    max_ips: int | None = None
    ip_allowlist: list[str] | None = None
    models: list[str] | None = None
    quota: int | None = None
    realm: str | None = None


@router.get('')
def list_keys(user: dict = Depends(security.current_user)) -> list[dict]:
    return keysvc.list_keys()


@router.post('')
def create_key(body: KeyIn, request: Request,
               user: dict = Depends(security.require_admin)) -> dict:
    created = keysvc.create_key(
        name=body.name,
        expires_at=body.expires_at,
        max_ips=body.max_ips,
        ip_allowlist=body.ip_allowlist,
        models=body.models,
        quota=body.quota,
        realm=body.realm,
    )
    # 密钥是拿额度用的凭证，发放必须留痕（含来源 IP）
    security.audit(user, 'create_key', str(created.get('name') or ''),
                   f"id={created.get('id')}；来源 {client_ip(request)}")
    return created


@router.patch('/{key_id}')
def update_key(key_id: int, body: KeyPatch, user: dict = Depends(security.require_admin)) -> dict:
    updated = keysvc.update_key(key_id, body.model_dump(exclude_unset=True))
    if not updated:
        raise HTTPException(status_code=404, detail='密钥不存在')
    return updated


@router.post('/{key_id}/reset-usage')
def reset_usage(key_id: int, user: dict = Depends(security.require_admin)) -> dict:
    keysvc.reset_usage(key_id)
    return {'ok': True}


@router.delete('/{key_id}')
def delete_key(key_id: int, request: Request,
               user: dict = Depends(security.require_admin)) -> dict:
    if not keysvc.delete_key(key_id):
        raise HTTPException(status_code=404, detail='密钥不存在')
    security.audit(user, 'delete_key', str(key_id), f'来源 {client_ip(request)}')
    return {'ok': True}
