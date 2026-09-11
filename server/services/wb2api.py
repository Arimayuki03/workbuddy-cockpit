"""workbuddy2api 上游交互：账号文件、状态、模型、容器重启。"""
from __future__ import annotations

import asyncio
import json
import time
from pathlib import Path

import httpx

from .. import config


def _safe_file(filename: str) -> Path:
    if '/' in filename or '\\' in filename or '..' in filename:
        raise ValueError('非法的文件名')
    target = config.AUTH_DIR / filename
    if target.suffix != '.json':
        raise ValueError('非法的文件名')
    return target


def read_account_file(filename: str) -> dict:
    return json.loads(_safe_file(filename).read_text(encoding='utf-8'))


def list_auth_accounts() -> list[dict]:
    """读取 auths/ 目录下的本地账号（与 /status 的运行时状态互补）。"""
    out: list[dict] = []
    if not config.AUTH_DIR.is_dir():
        return out
    now = time.time()
    for path in sorted(config.AUTH_DIR.glob('workbuddy-*.json')):
        try:
            raw = json.loads(path.read_text(encoding='utf-8'))
        except Exception:
            continue
        acct = raw.get('account', {}) or {}
        auth = raw.get('auth', {}) or {}
        exp = int(auth.get('expiresAt', 0) or 0)
        out.append(
            {
                'file': path.name,
                'uid': str(acct.get('uid', '')),
                'nickname': acct.get('nickname') or '未命名',
                'enterprise_id': acct.get('enterpriseId', '') or '',
                'expires_at': exp,
                'is_expired': now >= exp,
                'remain_seconds': max(0, int(exp - now)),
                'source': 'file',
            }
        )
    return out


def delete_auth_account(filename: str) -> bool:
    target = _safe_file(filename)
    if target.exists():
        target.unlink()
        return True
    return False


def _auth_headers() -> dict:
    key = config.upstream_api_key()
    return {'Authorization': f'Bearer {key}'} if key else {}


async def get_status() -> dict:
    try:
        async with httpx.AsyncClient(timeout=10) as client:
            resp = await client.get(f'{config.WB2API_BASE}/status', headers=_auth_headers())
        if resp.status_code >= 400:
            return {'connected': False, 'error': f'上游返回 {resp.status_code}'}
        data = resp.json()
        data['connected'] = True
        return data
    except Exception as exc:  # noqa: BLE001
        return {'connected': False, 'error': str(exc)}


async def get_models() -> tuple[bool, list | dict]:
    try:
        async with httpx.AsyncClient(timeout=15) as client:
            resp = await client.get(f'{config.WB2API_BASE}/v1/models', headers=_auth_headers())
        if resp.status_code >= 400:
            return False, {'error': f'上游返回 {resp.status_code}'}
        body = resp.json()
        return True, body.get('data', body)
    except Exception as exc:  # noqa: BLE001
        return False, {'error': str(exc)}


async def restart_container() -> tuple[bool, str]:
    name = config.WB2API_CONTAINER
    try:
        proc = await asyncio.create_subprocess_exec(
            'docker', 'restart', name,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )
        _, err = await proc.communicate()
        if proc.returncode == 0:
            return True, f'容器 {name} 已重启'
        return False, (err.decode(errors='ignore').strip() or f'docker 退出码 {proc.returncode}')
    except FileNotFoundError:
        return False, '未找到 docker 命令'
    except Exception as exc:  # noqa: BLE001
        return False, str(exc)


def load_upstream_config() -> dict:
    """读取 workbuddy2api 的 config.json，API Key 做掩码。"""
    try:
        cfg = json.loads(config.UPSTREAM_CONFIG.read_text(encoding='utf-8'))
    except Exception:
        return {}

    def mask(v: str) -> str:
        if not v:
            return ''
        return v[:6] + '*' * max(0, len(v) - 10) + v[-4:] if len(v) > 12 else '******'

    view = dict(cfg)
    if 'api_key' in view:
        view['api_key_masked'] = mask(str(view.pop('api_key') or ''))
    view.setdefault('auth_dir', str(config.AUTH_DIR))
    view['raw'] = cfg
    return view


def save_upstream_config(patch: dict) -> dict:
    """仅允许改写 schedule / pool / cooldown / features 等非敏感段。"""
    try:
        cfg = json.loads(config.UPSTREAM_CONFIG.read_text(encoding='utf-8'))
    except Exception:
        cfg = {}
    for field in ('schedule', 'pool', 'cooldown', 'features'):
        if field in patch and isinstance(patch[field], dict):
            cfg.setdefault(field, {})
            cfg[field].update(patch[field])
    config.UPSTREAM_CONFIG.write_text(json.dumps(cfg, ensure_ascii=False, indent=2), encoding='utf-8')
    return load_upstream_config()
