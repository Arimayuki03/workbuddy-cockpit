"""运行期配置：全部通过环境变量覆盖，默认值适配 1Panel 单机部署。"""
from __future__ import annotations

import json
import os
import secrets
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent  # 仓库根目录


def _env(name: str, default: str) -> str:
    v = os.environ.get(name, '').strip()
    return v or default


def _env_int(name: str, default: int) -> int:
    try:
        return int(os.environ.get(name, '').strip() or default)
    except ValueError:
        return default


PORT = _env_int('WB_MANAGER_PORT', 7864)
HOST = _env('WB_MANAGER_HOST', '0.0.0.0')

# 上游 workbuddy2api（Go）
WB2API_BASE = _env('WB2API_BASE', 'http://127.0.0.1:7863').rstrip('/')
WB2API_KEY = _env('WB2API_KEY', '')
WB2API_CONTAINER = _env('WB2API_CONTAINER', 'workbuddy2api')

# 上游数据文件（与 workbuddy2api 共享）
AUTH_DIR = Path(_env('WB_AUTH_DIR', '/opt/workbuddy2api/auths'))
UPSTREAM_CONFIG = Path(_env('WB_UPSTREAM_CONFIG', '/opt/workbuddy2api/config.json'))

# 本管理端数据
DATA_DIR = Path(_env('WB_DATA_DIR', str(ROOT / 'data')))
DB_PATH = Path(_env('WB_DB', str(DATA_DIR / 'manager.db')))
USERS_FILE = Path(_env('WB_USERS_FILE', str(DATA_DIR / 'users.json')))
STATIC_DIR = Path(_env('WB_STATIC_DIR', str(ROOT / 'web' / 'out')))

# 网络
UPSTREAM_TIMEOUT = _env_int('WB_UPSTREAM_TIMEOUT', 120)
TENCENT_TIMEOUT = _env_int('WB_TENCENT_TIMEOUT', 15)
# 「全部签到」的并发上限：太低会拖到前端超时（几十个账号时），
# 太高又容易触发腾讯风控。5 是保守且够快的取值。
CHECKIN_CONCURRENCY = max(1, _env_int('WB_CHECKIN_CONCURRENCY', 5))
TRUST_PROXY = _env('WB_TRUST_PROXY', '1') == '1'
# 可信反向代理跳数：用于从 X-Forwarded-For 右侧取真实客户端 IP。
# 前面直接是 1Panel/OpenResty 时保持 1；若还挂了 CDN 则设为 CDN+反代的层数。
TRUSTED_PROXY_HOPS = _env_int('WB_TRUSTED_PROXY_HOPS', 1)
# 是否暴露 /docs、/openapi.json、/redoc。生产环境建议关闭（默认关闭）。
ENABLE_DOCS = _env('WB_ENABLE_DOCS', '0') == '1'
# 显式出口代理（可选，如 http://127.0.0.1:7890）。
# 留空时所有请求都不使用任何代理：httpx 默认 trust_env=True 会读取系统/环境代理，
# 会把内网请求（如 127.0.0.1:7863）也交给系统代理，导致连接被劫持或长时间超时。
HTTP_PROXY = _env('WB_HTTP_PROXY', '')
SESSION_DAYS = _env_int('WB_SESSION_DAYS', 7)
COOKIE_NAME = 'wb_session'
SECURE_COOKIE = _env('WB_SECURE_COOKIE', 'auto')  # auto | true | false

# 允许的 CORS 来源（同源部署时留空即可）
CORS_ORIGINS = [o for o in _env('WB_CORS_ORIGINS', '').split(',') if o]

TENCENT_BASE = 'https://copilot.tencent.com'
TENCENT_CHECKIN = 'https://www.codebuddy.cn/v2/billing/meter/daily-checkin'
# 积分余额查询（与上游 BillingBaseCN 一致）
TENCENT_BILLING = 'https://www.codebuddy.cn/v2/billing/meter/get-user-resource'
TENCENT_HEADERS = {
    'Content-Type': 'application/json',
    'Accept': 'application/json, text/plain, */*',
    'X-Requested-With': 'XMLHttpRequest',
    'User-Agent': 'CLI/2.63.2 CodeBuddy/2.63.2',
    'Origin': 'https://www.codebuddy.cn',
    'Referer': 'https://www.codebuddy.cn/',
}


def ensure_dirs() -> None:
    DATA_DIR.mkdir(parents=True, exist_ok=True)
    DB_PATH.parent.mkdir(parents=True, exist_ok=True)
    USERS_FILE.parent.mkdir(parents=True, exist_ok=True)


def upstream_api_key() -> str:
    """优先环境变量，其次读取 workbuddy2api 的 config.json。"""
    if WB2API_KEY:
        return WB2API_KEY
    try:
        cfg = json.loads(UPSTREAM_CONFIG.read_text(encoding='utf-8'))
        return str(cfg.get('api_key') or '')
    except Exception:
        return ''


def new_secret() -> str:
    return secrets.token_urlsafe(48)


def http_client(timeout, *, connect: float | None = None):
    """统一的 httpx 客户端：默认忽略系统/环境代理，避免内网请求被代理劫持。

    需要走代理时显式设置 WB_HTTP_PROXY。

    timeout 可以传数字，也可以传已构造好的 httpx.Timeout。
    注意：httpx 不允许「Timeout 实例 + connect 关键字」同时传，
    因此传入实例时忽略 connect，避免 AssertionError。
    """
    import httpx

    if isinstance(timeout, httpx.Timeout):
        tmo = timeout
    elif connect is not None:
        tmo = httpx.Timeout(timeout, connect=connect)
    else:
        tmo = httpx.Timeout(timeout)
    kwargs = {'timeout': tmo, 'trust_env': False}
    if HTTP_PROXY:
        kwargs['proxy'] = HTTP_PROXY
    return httpx.AsyncClient(**kwargs)
