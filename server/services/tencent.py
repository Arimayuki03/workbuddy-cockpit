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
    async with config.http_client(config.TENCENT_TIMEOUT, connect=5) as client:
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

    async with config.http_client(config.TENCENT_TIMEOUT, connect=5) as client:
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
        async with config.http_client(config.TENCENT_TIMEOUT, connect=5) as client:
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


async def fetch_credits(auth: dict) -> tuple[bool, int | float | None, str]:
    """查询账号**实时**积分余额。

    为什么必须由管理端自己查：workbuddy2api 只在它的定时任务
    （签到 / 保活时刻）刷新 credits，之后 /status 里一直是旧值；
    手动签到也不会触发它刷新。因此要拿到当前余额只能直接调腾讯接口。

    口径与上游保持一致：优先取套餐的 CycleCapacityRemain，
    无 Cycle 字段时退回 CapacityRemain（见 upstream.UserResource）。
    返回 (ok, credits, message)。
    """
    access_token = str(auth.get('access_token') or '')
    if not access_token:
        return False, None, '该账号无有效 accessToken'

    now = time.time()
    body = {
        'PageNumber': 1,
        'PageSize': 100,
        'ProductCode': 'p_tcaca',
        'Status': [0, 3],
        'PackageEndTimeRangeBegin': time.strftime('%Y-%m-%d %H:%M:%S', time.localtime(now)),
        'PackageEndTimeRangeEnd': time.strftime(
            '%Y-%m-%d %H:%M:%S', time.localtime(now + 365 * 101 * 24 * 3600)
        ),
    }
    headers = {
        **config.TENCENT_HEADERS,
        'Accept': 'application/json',
        'Authorization': f'Bearer {access_token}',
    }

    try:
        async with config.http_client(config.TENCENT_TIMEOUT, connect=5) as client:
            resp = await client.post(config.TENCENT_BILLING, json=body, headers=headers)
        code, data = _envelope(resp)
        if code != 0 or not data:
            return False, None, f'查询失败 code={code}'

        accounts = _extract_resource_accounts(data)
        if accounts is None:
            return False, None, '响应结构无法识别'

        total = 0.0
        for item in accounts:
            if not isinstance(item, dict):
                continue
            total += _package_remain(item)
        # 上游会把负值钳为 0；保留小数以贴近官方展示
        total = max(0.0, total)
        return True, (int(total) if total.is_integer() else round(total, 2)), '查询成功'
    except Exception as exc:  # noqa: BLE001
        return False, None, f'查询异常: {exc}'


async def fetch_models(access_token: str) -> tuple[bool, list | str]:
    """拉取该账号可用的 CLI 模型（含显示名、上下文、最大输出、推理档位）。

    为什么要管理端自己拉、而不是用上游的 /v1/models：上游把腾讯返回的
    `name`（显示名）和 `reasoning.supportedEfforts`（推理档位）**丢掉了**，
    只暴露 id / context_length / max_output_tokens。要做「模型中心」这类
    带显示名与能力的展示，只能照上游约定直连腾讯接口（同一路径、同一信封）。

    口径与上游 FetchModels 保持一致：
      - 只取 agents 里名为 `cli` 的模型 id 列表（那才是对 CLI 暴露的）
      - `disabled` 的条目不收录
    返回 (ok, models 或错误信息)。不含任何凭据。
    """
    if not access_token:
        return False, '该账号无有效 accessToken'
    url = f'{config.TENCENT_BASE}/console/enterprises/personal/models'
    headers = {**config.TENCENT_HEADERS, 'Authorization': f'Bearer {access_token}'}
    try:
        async with config.http_client(config.TENCENT_TIMEOUT, connect=5) as client:
            resp = await client.get(url, headers=headers)
        code, data = _envelope(resp)
        if code != 0 or not isinstance(data, dict):
            return False, f'模型接口返回 code={code}'
    except Exception as exc:  # noqa: BLE001
        return False, f'模型接口异常: {exc}'

    raw_models = data.get('models') if isinstance(data.get('models'), list) else []
    agents = data.get('agents') if isinstance(data.get('agents'), list) else []

    cli_ids: list[str] = []
    for ag in agents:
        if isinstance(ag, dict) and ag.get('name') == 'cli':
            ids = ag.get('models')
            if isinstance(ids, list):
                cli_ids = [str(x) for x in ids if x]
            break

    info: dict[str, dict] = {}
    for m in raw_models:
        if not isinstance(m, dict) or not m.get('id'):
            continue
        reasoning = m.get('reasoning') if isinstance(m.get('reasoning'), dict) else {}
        efforts = reasoning.get('supportedEfforts')
        info[str(m['id'])] = {
            'id': str(m['id']),
            'name': str(m.get('name') or '').strip(),
            'context_length': _as_int(m.get('maxInputTokens')),
            'max_output_tokens': _as_int(m.get('maxOutputTokens')),
            'disabled': bool(m.get('disabled')),
            'efforts': [str(x) for x in efforts if x] if isinstance(efforts, list) else [],
        }

    # cli 列表为空时退回全部未禁用模型：上游此时直接报错，但管理端只是展示，
    # 给个可用列表比整页空白更有用（来源会在 UI 上如实标注）。
    ids = cli_ids or list(info.keys())
    out = [info[i] for i in ids if i in info and not info[i]['disabled']]
    if not out:
        return False, '模型接口未返回任何可用模型'
    return True, out


def _as_int(v: object) -> int:
    try:
        return int(v)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        return 0


def _extract_resource_accounts(data: object) -> list | None:
    """不同层级的信封包装，尽量把套餐数组取出来。"""
    cur = data
    for _ in range(4):
        if isinstance(cur, dict):
            if 'Accounts' in cur and isinstance(cur['Accounts'], list):
                return cur['Accounts']
            # 逐层下钻：Response / Data / data 等
            for key in ('Response', 'Data', 'data', 'response'):
                if isinstance(cur.get(key), (dict, list)):
                    cur = cur[key]
                    break
            else:
                return None
        elif isinstance(cur, list):
            return cur
        else:
            return None
    return None


def _package_remain(item: dict) -> float:
    """单个套餐的剩余额度，口径与上游 UserResource 一致。"""
    def num(key: str) -> float:
        v = item.get(key)
        return float(v) if isinstance(v, (int, float)) else 0.0

    cycle_size = num('CycleCapacitySize')
    cycle_remain = num('CycleCapacityRemain')
    cycle_used = num('CycleCapacityUsed')
    if cycle_size > 0 or cycle_remain > 0 or cycle_used > 0:
        return cycle_remain
    return num('CapacityRemain')


async def probe_account(auth: dict, model: str = 'glm-5.2') -> tuple[bool, str]:
    """以最小**流式**对话请求探测账号可用性。

    必须用流式：上游强制要求 stream=true，非流式会返回
    code=11101「Non-stream chat request is currently not supported」
    （见 workbuddy2api payload.go 中强制 obj["stream"]=true 的处理）。
    因此这里发起流式请求，读到首个数据块即判定可用，随即断开。

    请求头复刻上游 ChatHeaders：缺失字段用 X-No-* 约定，
    并带上 X-Product: SaaS 与 X-User-Id，避免因请求不完整被拒。
    """
    import time as _time

    access_token = str(auth.get('access_token') or '')
    if not access_token:
        return False, '该账号无有效 accessToken'

    uid = str(auth.get('uid') or '')
    enterprise_id = str(auth.get('enterprise_id') or '')
    domain = str(auth.get('domain') or '')
    base = domain if domain.startswith('http') else config.TENCENT_BASE

    headers = dict(config.TENCENT_HEADERS)
    headers['Authorization'] = f'Bearer {access_token}'
    headers['X-User-Id' if uid else 'X-No-User-Id'] = uid or '1'
    headers['X-Enterprise-Id' if enterprise_id else 'X-No-Enterprise-Id'] = enterprise_id or '1'
    headers['X-Domain' if domain else 'X-No-Department-Info'] = domain or '1'
    headers['X-Product'] = 'SaaS'

    payload = {
        'model': model,
        'messages': [{'role': 'user', 'content': 'ping'}],
        'max_tokens': 1,
        'stream': True,
    }

    started = _time.time()
    try:
        async with config.http_client(config.TENCENT_TIMEOUT, connect=5) as client:
            async with client.stream(
                'POST', f'{base}/v2/chat/completions', json=payload, headers=headers,
            ) as resp:
                if resp.status_code >= 400:
                    raw = (await resp.aread()).decode('utf-8', errors='replace')
                    code, msg = _parse_error_body(raw, resp.status_code)
                    return False, _explain_code(code, msg)
                # 读到首个非空数据块即可确认账号可用，无需等流结束
                async for chunk in resp.aiter_bytes():
                    if chunk:
                        elapsed = int((_time.time() - started) * 1000)
                        return True, f'连通正常（{model}，{elapsed}ms）'
                return False, '上游未返回任何数据'
    except Exception as exc:  # noqa: BLE001
        return False, f'请求异常: {exc}'


def _parse_error_body(raw: str, status: int) -> tuple[int | str, str]:
    """错误响应可能是 {code,msg} 信封，也可能是纯文本。"""
    try:
        env = json.loads(raw)
        if isinstance(env, dict):
            return env.get('code', status), str(env.get('msg') or env.get('message') or '')
    except Exception:  # noqa: BLE001
        pass
    return status, raw.strip()[:200]


# 已知业务码 -> 可读说明（来源：workbuddy2api 源码与实测）
_CODE_HINTS: dict[int, str] = {
    0: '成功',
    10001: '今日已签到',
    11101: '上游不接受非流式请求（协议问题，非账号问题）',
    11128: 'developer 角色需归一化为 system（上游拒绝）',
    12153: '会话已失效，需重新登录',
}


def _explain_code(code: int | str, msg: str = '') -> str:
    try:
        hint = _CODE_HINTS.get(int(code))
    except (TypeError, ValueError):
        hint = None
    parts = [f'上游返回 code={code}']
    if msg:
        parts.append(msg)
    if hint:
        parts.append(f'（{hint}）')
    return ' '.join(parts)
