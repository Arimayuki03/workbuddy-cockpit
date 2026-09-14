"""版本（realm）：国内版 CN 与国际版 Global 的判定与端点分派。

为什么要单独一层：上游从 2026-09-14 起支持国际版，**单实例同时服务两套上游**
（共用账号池），按账号的 `realm` 字段或模型名前缀路由。本管理端此前把
腾讯域名与请求头写死在 config.py 里，只能对国内版；要支持切换就必须把
「这次请求该走哪套端点」变成可推导的量。

口径**严格镜像上游**（读其 Go 源码确认，非猜测）：

  - `auth.realm` 嵌在 auth 对象内（与 domain 同级），值 "cn" / "global"
  - 判定顺序：`global.enabled=false` → 恒 cn（逃生门）；
    否则 realm == "global" 或 domain 是 workbuddy.ai / *.workbuddy.ai → global
  - 存量账号没有 realm 字段，按 domain 回退；domain 也为空则判 cn，
    因此**升级后既有部署的行为不变**

端点差异（上游 client.go / headers.go）：

  | 维度        | CN                        | Global                    |
  |-------------|---------------------------|---------------------------|
  | chat base   | copilot.tencent.com       | www.workbuddy.ai          |
  | billing     | www.codebuddy.cn          | www.workbuddy.ai          |
  | Origin/Ref  | www.codebuddy.cn          | www.workbuddy.ai          |
  | UA 品牌段   | WorkBuddy                 | WorkBuddy AI              |
  | chat 路径   | /v2/chat/completions      | /console/... 404 回落 /v2 |
  | billing 路径| /v2/billing/meter/*       | /billing/meter/* 404 回落 |

注意 billing 的回落方向两边相反（CN 只有 /v2、Global 以无前缀优先），
这不是笔误，照上游实现来。
"""
from __future__ import annotations

import json
import time
from typing import Literal

from .. import config

Realm = Literal['cn', 'global']

CN: Realm = 'cn'
GLOBAL: Realm = 'global'

# 国际版默认基址（上游 config 的 global.chat_base / billing_base 留空时用它）
DEFAULT_GLOBAL_BASE = 'https://www.workbuddy.ai'
# 国际版登录与普通接口的 Origin/Referer
GLOBAL_ORIGIN = 'https://www.workbuddy.ai'
CN_ORIGIN = 'https://www.codebuddy.cn'

# 读上游 config.json 的缓存：改动要在 10 秒内生效，又不必每请求读盘
_CFG_TTL = 10
_cfg_cache: dict = {'at': 0.0, 'data': None}


def _read_global_config() -> dict:
    """读上游 config.json 的 `global` 段。读不到按默认（enabled=True、base 留空）。"""
    now = time.time()
    cached = _cfg_cache.get('data')
    if cached is not None and now - float(_cfg_cache['at']) < _CFG_TTL:
        return cached
    data = {'enabled': True, 'chat_base': '', 'billing_base': ''}
    try:
        raw = json.loads(config.UPSTREAM_CONFIG.read_text(encoding='utf-8'))
        g = raw.get('global') if isinstance(raw, dict) else None
        if isinstance(g, dict):
            if isinstance(g.get('enabled'), bool):
                data['enabled'] = g['enabled']
            for k in ('chat_base', 'billing_base'):
                v = g.get(k)
                if isinstance(v, str) and v.strip():
                    data[k] = v.strip().rstrip('/')
    except Exception:  # noqa: BLE001
        # 读不到就用默认：管理端不能因为读不到配置而无法工作
        pass
    _cfg_cache.update({'at': now, 'data': data})
    return data


def invalidate() -> None:
    """清空配置缓存（保存设置后调用，让改动立即生效）。"""
    _cfg_cache.update({'at': 0.0, 'data': None})


def global_enabled() -> bool:
    """国际版路由是否开启（镜像上游 global.enabled，缺省 true）。"""
    return bool(_read_global_config()['enabled'])


def has_global_domain(domain: str) -> bool:
    """域名是否属于国际版：workbuddy.ai 本身或其任意子域。

    镜像上游 isGlobalDomain：大小写不敏感、去空白，按后缀匹配。
    """
    d = (domain or '').strip().lower()
    return d == 'workbuddy.ai' or d.endswith('.workbuddy.ai')


def resolve_realm(explicit: str | None, domain: str | None) -> Realm:
    """纯推导，**不受逃生门影响**（对应上游 ResolveRealm）。

    显式值优先；其次按域名；都不满足则 cn。
    账号落盘时用这个，而不是 realm_of：否则一旦开了逃生门，
    会把国际版账号永久写成 cn（上游注释里专门警告过这点）。
    """
    e = (explicit or '').strip().lower()
    if e in ('cn', 'global'):
        return e  # type: ignore[return-value]
    return GLOBAL if has_global_domain(domain or '') else CN


def realm_of(auth: dict | None) -> Realm:
    """按账号判定其 realm，**含逃生门**（对应上游 Auth.Realm）。

    auth 可以是 `{'realm':..,'domain':..}` 或含这两键的更大字典
    （如 _auth_dict 的产物）。
    """
    if not global_enabled():
        return CN
    if not isinstance(auth, dict):
        return CN
    return resolve_realm(auth.get('realm'), auth.get('domain'))


def client_version() -> str:
    """出站 UA 的客户端版本段（上游 config upstream.client_version，空则内置默认）。"""
    try:
        raw = json.loads(config.UPSTREAM_CONFIG.read_text(encoding='utf-8'))
        up = raw.get('upstream') if isinstance(raw, dict) else None
        v = (up or {}).get('client_version') if isinstance(up, dict) else None
        if isinstance(v, str) and v.strip():
            return v.strip()
    except Exception:  # noqa: BLE001
        pass
    return '5.5.4'


def cli_version() -> str:
    """出站 UA 的 CLI 版本段（上游 config upstream.cli_version，空则内置默认）。"""
    try:
        raw = json.loads(config.UPSTREAM_CONFIG.read_text(encoding='utf-8'))
        up = raw.get('upstream') if isinstance(raw, dict) else None
        v = (up or {}).get('cli_version') if isinstance(up, dict) else None
        if isinstance(v, str) and v.strip():
            return v.strip()
    except Exception:  # noqa: BLE001
        pass
    return '2.137.1'


def _ua(realm: Realm) -> str:
    """出站 UA。镜像上游 defaultWorkBuddyUAFor：
    `WorkBuddy/<ver> <platform>/<ver> CLI/<cli>`，国际版的平台段是 `WorkBuddy AI`。
    """
    ver = client_version()
    platform = 'WorkBuddy AI' if realm == GLOBAL else 'WorkBuddy'
    return f'WorkBuddy/{ver} {platform}/{ver} CLI/{cli_version()}'


def origin_of(realm: Realm) -> str:
    return GLOBAL_ORIGIN if realm == GLOBAL else CN_ORIGIN


def chat_base(realm: Realm) -> str:
    """该 realm 的 chat/登录/模型接口基址。"""
    if realm == GLOBAL:
        base = _read_global_config().get('chat_base') or ''
        return base or DEFAULT_GLOBAL_BASE
    return config.TENCENT_BASE


def billing_base(realm: Realm) -> str:
    """该 realm 的 billing（签到 / 积分 / trial）基址。"""
    if realm == GLOBAL:
        base = _read_global_config().get('billing_base') or ''
        return base or DEFAULT_GLOBAL_BASE
    return config.TENCENT_BILLING_BASE


def chat_paths(realm: Realm) -> list[str]:
    """聊天补全的候选路径，按尝试顺序。

    国际版以 `/console/chat/completions` 优先、404/405 时回落 `/v2`；
    国内版只有 `/v2`。（上游 chatPaths / chatPath）
    """
    if realm == GLOBAL:
        return ['/console/chat/completions', '/v2/chat/completions']
    return ['/v2/chat/completions']


def billing_paths(realm: Realm, kind: str) -> list[str]:
    """billing 相关路径候选。kind: 'user-resource' | 'daily-checkin'。

    **注意两边顺序相反**（照上游 billingMeterPaths / checkingMeterPaths）：
    国际版无 `/v2` 前缀是首选，国内版只有 `/v2` 形式。
    """
    suffix = {
        'user-resource': 'get-user-resource',
        'daily-checkin': 'daily-checkin',
    }[kind]
    if realm == GLOBAL:
        return [f'/billing/meter/{suffix}', f'/v2/billing/meter/{suffix}']
    return [f'/v2/billing/meter/{suffix}']


def headers(realm: Realm, token: str | None = None) -> dict:
    """该 realm 的通用请求头（Origin/Referer/UA 随 realm 变）。

    token 非空时附 Authorization。注意这里只给「通用头」；
    各接口特有的头（X-User-Id 等）由调用方补。
    """
    origin = origin_of(realm)
    h = {
        'Content-Type': 'application/json',
        'Accept': 'application/json, text/plain, */*',
        'X-Requested-With': 'XMLHttpRequest',
        'User-Agent': _ua(realm),
        'Origin': origin,
        'Referer': f'{origin}/',
    }
    if token:
        h['Authorization'] = f'Bearer {token}'
    return h


def supports_checkin(realm: Realm) -> bool:
    """该 realm 是否有签到体系。

    国际版**没有**签到 / 旅行 / 活跃上报（上游调度器对 global 账号直接过滤、
    不发起任何请求，理由是避免风控）。调用方据此跳过，而不是打过去吃 4xx。
    令牌保活两边都支持，不在此列。
    """
    return realm != GLOBAL
