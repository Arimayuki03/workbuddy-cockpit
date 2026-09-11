"""腾讯 CodeBuddy 登录 / 签到协议客户端（对齐 workbuddy2api cmd/login）。"""
from __future__ import annotations

import json
import time
from typing import Any

import httpx

from .. import config

_state_cache: dict[str, float] = {}
STATE_TTL = 300


def _envelope(resp: httpx.Response) -> tuple[int, Any]:
    """腾讯接口统一信封 {code, msg, data}；HTTP 4xx 也可能是业务信封。"""
    try:
        env = resp.json()
    except Exception:
        return resp.status_code, None
    if isinstance(env, dict) and 'code' in env:
        return int(env.get('code', -1)), env.get('data')
    return resp.status_code, env


async def start_login() -> dict:
    async with httpx.AsyncClient(timeout=config.TENCENT_TIMEOUT) as client:
        resp = await client.post(
            f'{config.TENCENT_BASE}/v2/plugin/auth/state',
            params={'platform': 'CLI'},
            json={},
            headers=config.TENCENT_HEADERS,
        )
    code, data = _envelope(resp)
    if code != 0 or not data:
        raise RuntimeError(f'获取授权链接失败 code={code}')
    state = data.get('state') or ''
    if state:
        _state_cache[state] = time.time()
    return {'state': state, 'authUrl': data.get('authUrl') or ''}


def is_pending(state: str) -> bool:
    return state in _state_cache


def drop_state(state: str) -> None:
    _state_cache.pop(state, None)


async def poll_login(state: str) -> dict:
    """轮询扫码结果。waiting / expired / ready(含 token 与账号信息)。"""
    created = _state_cache.get(state)
    if created is None:
        return {'status': 'invalid'}
    if time.time() - created > STATE_TTL:
        drop_state(state)
        return {'status': 'expired'}

    async with httpx.AsyncClient(timeout=config.TENCENT_TIMEOUT) as client:
        resp = await client.get(
            f'{config.TENCENT_BASE}/v2/plugin/auth/token',
            params={'state': state},
            headers=config.TENCENT_HEADERS,
        )
        code, data = _envelope(resp)
        if code != 0 or not data or not data.get('accessToken'):
            return {'status': 'waiting'}

        access_token = data['accessToken']
        refresh_token = data.get('refreshToken', '')
        expires_in = int(data.get('expiresIn', 3600) or 3600)
        domain = data.get('domain', '')

        acct_resp = await client.get(
            f'{config.TENCENT_BASE}/v2/plugin/login/account',
            params={'state': state},
            headers={**config.TENCENT_HEADERS, 'Authorization': f'Bearer {access_token}'},
        )
    _, acct = _envelope(acct_resp)
    acct = acct or {}
    uid = acct.get('uid')
    if not uid:
        return {'status': 'waiting'}

    drop_state(state)
    return {
        'status': 'ready',
        'uid': str(uid),
        'nickname': acct.get('nickname', '') or '',
        'enterprise_id': acct.get('enterpriseId', '') or '',
        'access_token': access_token,
        'refresh_token': refresh_token,
        'expires_at': int(time.time()) + expires_in,
        'domain': domain,
    }


def write_auth_file(account: dict) -> tuple[str, bool]:
    """严格按 workbuddy2api 的嵌套结构落盘，返回 (文件名, 是否覆盖)。"""
    uid = account['uid']
    config.AUTH_DIR.mkdir(parents=True, exist_ok=True)
    target = config.AUTH_DIR / f'workbuddy-{uid}.json'
    existed = target.exists()
    payload = {
        'account': {
            'uid': uid,
            'enterpriseId': account.get('enterprise_id', ''),
            'nickname': account.get('nickname', ''),
        },
        'auth': {
            'accessToken': account['access_token'],
            'refreshToken': account.get('refresh_token', ''),
            'expiresAt': account['expires_at'],
            'domain': account.get('domain', ''),
        },
    }
    target.write_text(json.dumps(payload, ensure_ascii=False, indent=1), encoding='utf-8')
    return target.name, existed


async def checkin(access_token: str) -> tuple[int, str]:
    """每日签到。10001 = 今日已签到，属正常幂等。"""
    try:
        async with httpx.AsyncClient(timeout=config.TENCENT_TIMEOUT) as client:
            resp = await client.post(
                config.TENCENT_CHECKIN,
                json={},
                headers={**config.TENCENT_HEADERS, 'Authorization': f'Bearer {access_token}'},
            )
        code, _ = _envelope(resp)
        if code == 0:
            return 0, '签到成功'
        if code == 10001:
            return 10001, '今日已签到'
        return code, f'签到返回 code={code}'
    except Exception as exc:  # noqa: BLE001
        return -1, f'签到异常: {exc}'


async def probe_account(access_token: str, model: str = 'glm-5.2') -> tuple[bool, str]:
    """以最小对话请求探测账号可用性。"""
    payload = {'model': model, 'messages': [{'role': 'user', 'content': 'ping'}], 'max_tokens': 1}
    try:
        async with httpx.AsyncClient(timeout=config.TENCENT_TIMEOUT) as client:
            resp = await client.post(
                f'{config.TENCENT_BASE}/v2/chat/completions',
                json=payload,
                headers={**config.TENCENT_HEADERS, 'Authorization': f'Bearer {access_token}'},
            )
        code, _ = _envelope(resp)
        if code == 0 or resp.status_code == 200:
            return True, '连通正常'
        return False, f'上游返回 code={code}'
    except Exception as exc:  # noqa: BLE001
        return False, f'请求异常: {exc}'
