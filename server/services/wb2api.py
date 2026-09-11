"""workbuddy2api 上游交互：账号文件、状态、模型、容器重启。"""
from __future__ import annotations

import asyncio
import json
import time
from pathlib import Path

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


ASYNC_HEADERS = {'Content-Type': 'application/json'}


def _err_text(exc: Exception) -> str:
    """异常文本可能为空（如 AssertionError），补上类型名便于排查。"""
    detail = str(exc).strip()
    return f'{type(exc).__name__}: {detail}' if detail else type(exc).__name__


def _auth_headers() -> dict:
    key = config.upstream_api_key()
    return {'Authorization': f'Bearer {key}'} if key else {}


async def get_status() -> dict:
    # 连接超时短一些：上游未运行时快速失败，避免拖慢管理端页面
    try:
        async with config.http_client(10, connect=3) as client:
            resp = await client.get(f'{config.WB2API_BASE}/status', headers=_auth_headers())
        if resp.status_code >= 400:
            return {'connected': False, 'error': f'上游返回 {resp.status_code}'}
        data = resp.json()
        data['connected'] = True
        return data
    except Exception as exc:  # noqa: BLE001
        return {'connected': False, 'error': _err_text(exc)}


async def get_models() -> tuple[bool, list | dict]:
    try:
        async with config.http_client(15, connect=3) as client:
            resp = await client.get(f'{config.WB2API_BASE}/v1/models', headers=_auth_headers())
        if resp.status_code >= 400:
            return False, {'error': f'上游返回 {resp.status_code}'}
        body = resp.json()
        return True, body.get('data', body)
    except Exception as exc:  # noqa: BLE001
        return False, {'error': _err_text(exc)}


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


def _mask(v: str) -> str:
    if not v:
        return ''
    return v[:6] + '*' * max(0, len(v) - 10) + v[-4:] if len(v) > 12 else '******'


def load_upstream_config() -> dict:
    """读取 workbuddy2api 的 config.json，敏感字段一律掩码。

    读不到时返回 available=False 并附带原因，供前端明确提示并禁止保存，
    避免把空配置写回真实文件。

    注意：不返回原始配置对象。原始配置含上游 API Key 与 Upstash token 的
    明文，前端并不需要它们，不应通过接口下发。
    """
    path = config.UPSTREAM_CONFIG
    cfg: dict | None = None
    error: str | None = None

    if not path.is_file():
        error = f'未找到上游配置文件 {path}'
    else:
        try:
            loaded = json.loads(path.read_text(encoding='utf-8'))
            if isinstance(loaded, dict):
                cfg = loaded
            else:
                error = f'上游配置文件不是合法的 JSON 对象: {path}'
        except Exception as exc:  # noqa: BLE001
            error = f'上游配置文件解析失败: {exc}'

    if cfg is None:
        return {
            'available': False,
            'config_path': str(path),
            'auth_dir': str(config.AUTH_DIR),
            'error': error or '无法读取上游配置',
        }

    view = dict(cfg)
    if 'api_key' in view:
        view['api_key_masked'] = _mask(str(view.pop('api_key') or ''))
    # 账号列表实际读取的是管理端自己的 AUTH_DIR，以此为准；上游若声明了不同目录则一并暴露
    upstream_auth_dir = cfg.get('auth_dir')
    view['auth_dir'] = str(config.AUTH_DIR)
    if upstream_auth_dir and str(upstream_auth_dir) != str(config.AUTH_DIR):
        view['upstream_auth_dir'] = str(upstream_auth_dir)

    # Upstash：token 属敏感信息，只回传「是否已配置」，不回传内容
    up = cfg.get('upstash')
    up = up if isinstance(up, dict) else {}
    token = str(up.get('token') or '')
    view['upstash'] = {
        'url': str(up.get('url') or ''),
        'has_token': bool(token),
        'token_masked': _mask(token) if token else '',
    }

    view['available'] = True
    view['config_path'] = str(path)
    return view


def save_upstream_config(patch: dict) -> dict:
    """仅允许改写 schedule / pool / cooldown / features / upstash 等非敏感段。

    配置读不到时直接拒绝，绝不基于空 dict 生成新文件覆盖真实配置。
    """
    path = config.UPSTREAM_CONFIG
    if not path.is_file():
        raise FileNotFoundError(f'未找到上游配置文件 {path}，已取消保存')

    try:
        cfg = json.loads(path.read_text(encoding='utf-8'))
    except Exception as exc:  # noqa: BLE001
        raise ValueError(f'上游配置文件解析失败，已取消保存: {exc}') from exc
    if not isinstance(cfg, dict):
        raise ValueError('上游配置文件不是合法的 JSON 对象，已取消保存')

    for field in ('schedule', 'pool', 'cooldown', 'features'):
        if field in patch and isinstance(patch[field], dict):
            cfg.setdefault(field, {})
            cfg[field].update(patch[field])

    if 'upstash' in patch and isinstance(patch['upstash'], dict):
        incoming = patch['upstash']
        current = cfg.get('upstash')
        current = current if isinstance(current, dict) else {}

        if incoming.get('clear'):
            # 显式关闭：清空 url 与 token
            current = {'url': '', 'token': ''}
        else:
            if 'url' in incoming:
                current['url'] = str(incoming.get('url') or '').strip()
            # token 只在传入非空值时替换：前端回显的是掩码，
            # 留空即表示「保持不变」，避免误清空已配置的凭据
            if str(incoming.get('token') or '').strip():
                current['token'] = str(incoming['token']).strip()

        cfg['upstash'] = {
            'url': str(current.get('url') or ''),
            'token': str(current.get('token') or ''),
        }

    path.write_text(json.dumps(cfg, ensure_ascii=False, indent=2), encoding='utf-8')
    return load_upstream_config()


# ── Upstash 连通性检测 ───────────────────────────────────
def _upstash_rest_base(url: str) -> str | None:
    """把各种写法归一化为 Upstash REST 根地址。

    支持：https://xxx.upstash.io / xxx.upstash.io / rediss://default:tok@xxx.upstash.io:6379
    与 workbuddy2api 的 normalizeURL 保持一致的思路。
    """
    raw = (url or '').strip()
    if not raw:
        return None
    # 去掉 scheme
    if '://' in raw:
        scheme, rest = raw.split('://', 1)
        if scheme.lower() in ('rediss', 'redis'):
            # rediss://user:pass@host:port -> 取 host
            host = rest.rsplit('@', 1)[-1]
            host = host.split(':', 1)[0]
            return f'https://{host}' if host else None
        # https://host/... -> 取 host
        host = rest.split('/', 1)[0].split(':', 1)[0]
        return f'https://{host}' if host else None
    host = raw.split('/', 1)[0].split(':', 1)[0]
    return f'https://{host}' if host else None


async def test_upstash(url: str, token: str | None = None) -> tuple[bool, str]:
    """用 Upstash REST 接口探测连通性（PING）。token 留空时取配置文件中的值。"""
    base = _upstash_rest_base(url)
    if not base:
        return False, '请先填写 Upstash 地址'

    if not token:
        try:
            cfg = json.loads(config.UPSTREAM_CONFIG.read_text(encoding='utf-8'))
            token = str((cfg.get('upstash') or {}).get('token') or '')
        except Exception:  # noqa: BLE001
            token = ''
    if not token:
        return False, '缺少 Upstash Token'

    try:
        async with config.http_client(10, connect=5) as client:
            resp = await client.post(
                f'{base}/ping',
                headers={'Authorization': f'Bearer {token}'},
            )
        if resp.status_code == 401:
            return False, 'Token 无效（401）'
        if resp.status_code >= 400:
            return False, f'Upstash 返回 {resp.status_code}'
        body = resp.text.strip()
        if 'PONG' in body.upper():
            return True, '连接正常（PONG）'
        return True, f'已连通，响应：{body[:60]}'
    except Exception as exc:  # noqa: BLE001
        return False, f'无法连接：{_err_text(exc)}'
