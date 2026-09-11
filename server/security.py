"""管理端鉴权：PBKDF2 密码、HMAC 签名 Cookie 会话、登录防爆破。"""
from __future__ import annotations

import base64
import hashlib
import hmac
import json
import secrets
import time

from fastapi import Depends, HTTPException, Request

from . import config

_fail: dict[str, list] = {}
MAX_FAILS = 5
LOCK_SECONDS = 600


# ── 密码哈希 ─────────────────────────────────────────────
def make_hash(pwd: str, iterations: int = 260000) -> str:
    salt = secrets.token_hex(16)
    digest = hashlib.pbkdf2_hmac('sha256', pwd.encode(), bytes.fromhex(salt), iterations)
    return f'pbkdf2_sha256${iterations}${salt}${digest.hex()}'


def verify_pwd(pwd: str, stored: str) -> bool:
    try:
        algo, iters, salt, expect = stored.split('$')
        if algo != 'pbkdf2_sha256':
            return False
        digest = hashlib.pbkdf2_hmac('sha256', pwd.encode(), bytes.fromhex(salt), int(iters))
        return hmac.compare_digest(digest.hex(), expect)
    except Exception:
        return False


# ── 用户存储 ─────────────────────────────────────────────
def load_users() -> dict:
    config.ensure_dirs()
    if not config.USERS_FILE.exists():
        return bootstrap_users()
    try:
        return json.loads(config.USERS_FILE.read_text(encoding='utf-8'))
    except Exception:
        return bootstrap_users()


def save_users(data: dict) -> None:
    config.ensure_dirs()
    config.USERS_FILE.write_text(json.dumps(data, ensure_ascii=False, indent=2), encoding='utf-8')


def bootstrap_users() -> dict:
    """首次启动：创建 admin 用户，密码取 WB_ADMIN_PASSWORD 或随机生成并打印一次。"""
    import os
    import sys

    pwd = os.environ.get('WB_ADMIN_PASSWORD', '').strip() or secrets.token_urlsafe(9)
    data = {
        'secret': config.new_secret(),
        'users': [{'username': 'admin', 'role': 'admin', 'pwd_hash': make_hash(pwd)}],
        'api_keys': [],
    }
    save_users(data)
    if not os.environ.get('WB_ADMIN_PASSWORD', '').strip():
        print('=' * 60, file=sys.stderr)
        print('[WorkBuddy Manager] 已生成初始管理员账号', file=sys.stderr)
        print(f'  用户名: admin', file=sys.stderr)
        print(f'  密码  : {pwd}', file=sys.stderr)
        print('  请登录后立即在「设置 → 管理用户」中修改密码。', file=sys.stderr)
        print('=' * 60, file=sys.stderr)
    return data


# ── 会话签名 ─────────────────────────────────────────────
def _sign(payload: str, secret: str) -> str:
    sig = hmac.new(secret.encode(), payload.encode(), hashlib.sha256).hexdigest()
    return base64.urlsafe_b64encode(f'{payload}|{sig}'.encode()).decode()


def _unsign(token: str, secret: str) -> dict | None:
    try:
        raw = base64.urlsafe_b64decode(token.encode()).decode()
        payload, sig = raw.rsplit('|', 1)
        expect = hmac.new(secret.encode(), payload.encode(), hashlib.sha256).hexdigest()
        if not hmac.compare_digest(expect, sig):
            return None
        obj = json.loads(payload)
        if obj.get('exp', 0) < time.time():
            return None
        return obj
    except Exception:
        return None


def issue_token(username: str, role: str) -> str:
    cfg = load_users()
    payload = json.dumps(
        {'username': username, 'role': role, 'exp': int(time.time()) + config.SESSION_DAYS * 86400}
    )
    return _sign(payload, cfg['secret'])


def cookie_secure(request: Request) -> bool:
    setting = config.SECURE_COOKIE.lower()
    if setting in ('true', '1'):
        return True
    if setting in ('false', '0'):
        return False
    proto = request.headers.get('x-forwarded-proto', request.url.scheme)
    return proto == 'https'


# ── 登录防爆破 ───────────────────────────────────────────
# 按 IP 与按用户名双维度计数：
#   - 按 IP：防单机爆破（配合修正后的真实 IP 解析才有效）
#   - 按用户名：防「换 IP 打同一账号」的分布式爆破
# 注意：两个字典都设有容量上限，避免被大量不同 IP/用户名撑爆内存。
_fail: dict[str, list] = {}
_user_fail: dict[str, list] = {}
MAX_FAILS = 5
LOCK_SECONDS = 600
MAX_TRACKED = 5000


def _prune(store: dict[str, list]) -> None:
    """超限时清掉已过期条目；仍超限则整体清空（宁可放宽，不可被撑爆）。"""
    if len(store) <= MAX_TRACKED:
        return
    now = time.time()
    for k in [k for k, v in store.items() if (now - v[1]) >= LOCK_SECONDS]:
        store.pop(k, None)
    if len(store) > MAX_TRACKED:
        store.clear()


def login_blocked(ip: str, username: str | None = None) -> bool:
    now = time.time()
    cnt, first = _fail.get(ip, [0, 0.0])
    if cnt >= MAX_FAILS and (now - first) < LOCK_SECONDS:
        return True
    if username:
        key = username.lower()
        ucnt, ufirst = _user_fail.get(key, [0, 0.0])
        if ucnt >= MAX_FAILS and (now - ufirst) < LOCK_SECONDS:
            return True
    return False


def record_fail(ip: str, username: str | None = None) -> None:
    now = time.time()
    cnt, first = _fail.get(ip, [0, now])
    if (now - first) >= LOCK_SECONDS:
        cnt, first = 0, now
    _fail[ip] = [cnt + 1, first]

    if username:
        key = username.lower()
        ucnt, ufirst = _user_fail.get(key, [0, now])
        if (now - ufirst) >= LOCK_SECONDS:
            ucnt, ufirst = 0, now
        _user_fail[key] = [ucnt + 1, ufirst]

    _prune(_fail)
    _prune(_user_fail)


def clear_fail(ip: str, username: str | None = None) -> None:
    _fail.pop(ip, None)
    if username:
        _user_fail.pop(username.lower(), None)


# ── FastAPI 依赖 ─────────────────────────────────────────
def current_user(request: Request) -> dict:
    cfg = load_users()
    token = request.cookies.get(config.COOKIE_NAME)
    if token:
        obj = _unsign(token, cfg['secret'])
        if obj:
            return {'username': obj.get('username'), 'role': obj.get('role', 'viewer')}
    api_key = request.headers.get('x-api-key')
    if api_key and api_key in cfg.get('api_keys', []):
        return {'username': 'api', 'role': 'admin'}
    raise HTTPException(status_code=401, detail='未登录')


def require_admin(user: dict = Depends(current_user)) -> dict:
    if user.get('role') != 'admin':
        raise HTTPException(status_code=403, detail='需要管理员权限')
    return user
