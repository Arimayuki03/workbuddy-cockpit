"""对外反代网关：密钥鉴权 → IP 管控 → 模型映射 → 转发 → 计量落库。"""
from __future__ import annotations

import json
import logging
import time

import httpx
from fastapi import APIRouter, Request
from fastapi.responses import JSONResponse, StreamingResponse

from .. import config, db, iputil, keysvc
from ..config import _env_int
from ..routers.security import get_config as get_security_config

logger = logging.getLogger('workbuddy.gateway')

router = APIRouter(tags=['gateway'])


def _oai_error(message: str, status: int = 400, err_type: str = 'invalid_request_error', code: str | None = None) -> JSONResponse:
    return JSONResponse(
        {'error': {'message': message, 'type': err_type, 'code': code}},
        status_code=status,
    )


def _bearer(request: Request) -> str:
    auth = request.headers.get('authorization', '')
    if auth.lower().startswith('bearer '):
        return auth[7:].strip()
    return request.headers.get('x-api-key', '').strip()


# 请求体上限，与上游 workbuddy2api 的 8 MiB 限制对齐。
# 不设上限时，超大请求体会被完整读入内存，少量并发即可耗尽内存。
MAX_BODY_BYTES = 8 * 1024 * 1024


def _body_too_large(request: Request) -> bool:
    try:
        return int(request.headers.get('content-length') or 0) > MAX_BODY_BYTES
    except ValueError:
        return False


async def _read_json_body(request: Request) -> tuple[dict | None, JSONResponse | None]:
    """读取并解析网关请求体，带大小与格式校验。"""
    if _body_too_large(request):
        return None, _oai_error(
            f'请求体过大（上限 {MAX_BODY_BYTES // 1024 // 1024} MiB）',
            413, 'invalid_request_error', 'payload_too_large',
        )
    try:
        body = await request.json()
    except Exception:  # noqa: BLE001
        return None, _oai_error('请求体不是合法 JSON', 400)
    if not isinstance(body, dict):
        return None, _oai_error('请求体必须是 JSON 对象', 400)
    return body, None


# ── 简单的每密钥速率限制（滑动窗口）─────────────────────
# 目的：单个密钥被打爆时保护上游账号池，避免拖垮其他调用方。
# 计数放在进程内存，单实例足够；多实例部署时可换成 Redis。
_rate: dict[int, list[float]] = {}
RATE_WINDOW = 60
RATE_MAX_PER_MIN = _env_int('WB_GATEWAY_RATE_PER_MIN', 120)


def _rate_limited(key: dict) -> tuple[bool, int]:
    """返回 (是否限流, 当前窗口内计数)。"""
    kid = int(key['id'])
    if RATE_MAX_PER_MIN <= 0:
        return False, 0
    now = time.time()
    hits = [t for t in _rate.get(kid, []) if now - t < RATE_WINDOW]
    hits.append(now)
    _rate[kid] = hits
    # 顺带清理过期键，避免长期运行后字典无限增长
    if len(_rate) > 2000:
        for k in [k for k, v in _rate.items() if not v or now - v[-1] > RATE_WINDOW]:
            _rate.pop(k, None)
    return len(hits) > RATE_MAX_PER_MIN, len(hits)


def _log_ip(ip: str, path: str, blocked: bool, ua: str | None) -> None:
    db.execute(
        'INSERT INTO ip_access_logs(ts, ip, path, blocked, ua) VALUES(?, ?, ?, ?, ?)',
        (int(time.time()), ip, path, 1 if blocked else 0, ua),
    )


def _usage_credit(usage: dict | None) -> float | None:
    """从 usage 里取上游的真实扣费（credit）。

    上游从 2026-09-13 起在末帧 usage 里带 credit（本次真实扣费）。
    取不到就返回 None（存 NULL），不要退化成 0——「没数据」和「免费」
    在成本判断上是两回事，混为一谈会误判成免费号。
    """
    if not isinstance(usage, dict):
        return None
    raw = usage.get('credit')
    if raw is None or isinstance(raw, bool):
        return None
    try:
        val = float(raw)
    except (TypeError, ValueError):
        return None
    return val if val >= 0 else None


def _record(key: dict | None, ip: str, model: str, mapped: str, status: int, pt: int, ct: int, latency: int, ua: str | None, error: str | None, stream: bool, *, credit: float | None = None) -> None:
    """记录调用日志与用量。

    credit 为上游返回的真实扣费（usage.credit）。None 表示上游没给，
    与「扣了 0」是两回事，因此用 NULL 存而不是 0。

    注意：日志/统计属于旁路，任何异常都不能影响用户请求本身
    （曾因统计函数缺失导致流式响应在收尾阶段中断，客户端看到
    内容正常但报 terminated）。因此这里整体兜底。
    """
    try:
        db.execute(
            'INSERT INTO request_logs(ts, key_id, ip, model, mapped_model, status, prompt_tokens, completion_tokens, latency_ms, ua, error, stream, credit) '
            'VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)',
            (int(time.time()), key['id'] if key else None, ip, model, mapped, status, pt, ct, latency, ua, error, 1 if stream else 0, credit),
        )
    except Exception as exc:  # noqa: BLE001
        logger.warning('写入请求日志失败（不影响请求）: %s', exc)
        return

    if key:
        try:
            total = pt + ct
            keysvc.touch(key, ip, total)
            if total or credit:
                db.bump_usage(key['id'], model, pt, ct, credit)
        except Exception as exc:  # noqa: BLE001
            logger.warning('累计用量失败（不影响请求）: %s', exc)


def _authorize(request: Request, model: str | None) -> tuple[dict | None, str, JSONResponse | None]:
    """返回 (key, ip, error_response)。"""
    ip = iputil.client_ip(request)
    ua = request.headers.get('user-agent')
    path = request.url.path

    token = _bearer(request)
    if not token:
        _log_ip(ip, path, True, ua)
        return None, ip, _oai_error('缺少 API Key，请在 Authorization 头中提供 Bearer 令牌', 401, 'authentication_error', 'missing_api_key')

    key = keysvc.resolve(token)
    if not key:
        _log_ip(ip, path, True, ua)
        return None, ip, _oai_error('API Key 无效', 401, 'authentication_error', 'invalid_api_key')

    # 全局入站 IP 规则
    sec = get_security_config()
    if sec.get('enabled'):
        rules = [
            {'kind': r['kind'], 'cidr': r['cidr']}
            for r in db.query('SELECT kind, cidr FROM ip_rules')
        ]
        if not iputil.evaluate(ip, rules, sec.get('mode', 'blacklist')):
            _log_ip(ip, path, True, ua)
            _record(key, ip, model or '', '', 403, 0, 0, 0, ua, 'IP 被拦截', False)
            return None, ip, _oai_error(f'来源 IP {ip} 被安全策略拦截', 403, 'permission_error', 'ip_blocked')

    _log_ip(ip, path, False, ua)

    reason = keysvc.validate(key, ip, model)
    if reason:
        _record(key, ip, model or '', '', 403, 0, 0, 0, ua, reason, False)
        return None, ip, _oai_error(reason, 403, 'permission_error', 'forbidden')

    limited, count = _rate_limited(key)
    if limited:
        msg = f'请求过于频繁（{RATE_WINDOW}s 内超过 {RATE_MAX_PER_MIN} 次）'
        _record(key, ip, model or '', '', 429, 0, 0, 0, ua, msg, False)
        return None, ip, _oai_error(msg, 429, 'rate_limit_error', 'rate_limit_exceeded')

    return key, ip, None


def _map_model(model: str | None) -> str | None:
    if not model:
        return model
    mapping = db.get_setting('model_map', {}) or {}
    return mapping.get(model, model)


def _upstream_headers() -> dict:
    headers = {'Content-Type': 'application/json'}
    api_key = config.upstream_api_key()
    if api_key:
        headers['Authorization'] = f'Bearer {api_key}'
    return headers


def _parse_sse_usage(pending: str, usage: dict) -> str:
    """从 SSE 文本片段中提取 usage，返回未处理完的残留缓冲。"""
    while '\n' in pending:
        line, pending = pending.split('\n', 1)
        line = line.strip()
        if not line.startswith('data:'):
            continue
        payload = line[5:].strip()
        if not payload or payload == '[DONE]':
            continue
        try:
            obj = json.loads(payload)
        except Exception:
            continue
        if isinstance(obj, dict) and isinstance(obj.get('usage'), dict):
            usage.update(obj['usage'])
    return pending


# ── 模型列表 ─────────────────────────────────────────────
@router.get('/v1/models')
async def list_models(request: Request):
    key, ip, err = _authorize(request, None)
    if err:
        return err
    started = time.time()
    try:
        async with config.http_client(30, connect=3) as client:
            resp = await client.get(f'{config.WB2API_BASE}/v1/models', headers=_upstream_headers())
        latency = int((time.time() - started) * 1000)
        _record(key, ip, '', '', resp.status_code, 0, 0, latency, request.headers.get('user-agent'), None, False)
        return JSONResponse(resp.json(), status_code=resp.status_code)
    except Exception as exc:  # noqa: BLE001
        latency = int((time.time() - started) * 1000)
        _record(key, ip, '', '', 502, 0, 0, latency, request.headers.get('user-agent'), str(exc), False)
        return _oai_error(f'上游不可用: {exc}', 502, 'api_error', 'upstream_unavailable')


# ── 对话补全（v1 / v2）──────────────────────────────────
async def _chat(request: Request, upstream_path: str):
    body, err = await _read_json_body(request)
    if err:
        return err

    requested_model = body.get('model') if isinstance(body, dict) else None
    key, ip, err = _authorize(request, requested_model)
    if err:
        return err

    mapped = _map_model(requested_model)
    if mapped:
        body['model'] = mapped

    stream = bool(isinstance(body, dict) and body.get('stream'))
    if stream:
        # 让上游在最后一个 chunk 返回 usage，便于精确计量
        body.setdefault('stream_options', {})
        if isinstance(body['stream_options'], dict):
            body['stream_options'].setdefault('include_usage', True)

    url = f'{config.WB2API_BASE}{upstream_path}'
    ua = request.headers.get('user-agent')
    started = time.time()

    if not stream:
        try:
            async with config.http_client(config.UPSTREAM_TIMEOUT, connect=5) as client:
                resp = await client.post(url, json=body, headers=_upstream_headers())
            latency = int((time.time() - started) * 1000)
            usage = {}
            try:
                data = resp.json()
                usage = data.get('usage') or {}
            except Exception:
                data = None
            pt = int(usage.get('prompt_tokens') or 0)
            ct = int(usage.get('completion_tokens') or 0)
            error = None if resp.status_code < 400 else (str(data)[:500] if data is not None else resp.text[:500])
            _record(
                key, ip, requested_model or '', mapped or '', resp.status_code, pt, ct,
                latency, ua, error, False, credit=_usage_credit(usage),
            )
            if data is not None:
                return JSONResponse(data, status_code=resp.status_code)
            return JSONResponse({'error': {'message': resp.text[:1000], 'type': 'api_error'}}, status_code=resp.status_code)
        except Exception as exc:  # noqa: BLE001
            latency = int((time.time() - started) * 1000)
            _record(key, ip, requested_model or '', mapped or '', 502, 0, 0, latency, ua, str(exc), False)
            return _oai_error(f'上游不可用: {exc}', 502, 'api_error', 'upstream_unavailable')

    # 流式转发
    client = config.http_client(config.UPSTREAM_TIMEOUT, connect=5)
    try:
        req = client.build_request('POST', url, json=body, headers=_upstream_headers())
        resp = await client.send(req, stream=True)
    except Exception as exc:  # noqa: BLE001
        await client.aclose()
        latency = int((time.time() - started) * 1000)
        _record(key, ip, requested_model or '', mapped or '', 502, 0, 0, latency, ua, str(exc), True)
        return _oai_error(f'上游不可用: {exc}', 502, 'api_error', 'upstream_unavailable')

    status_code = resp.status_code
    content_type = resp.headers.get('content-type', 'text/event-stream')

    async def generator():
        usage: dict = {}
        pending = ''
        error_text: str | None = None
        try:
            async for chunk in resp.aiter_bytes():
                if status_code >= 400:
                    pending += chunk.decode('utf-8', errors='ignore')
                    if len(pending) > 4000:
                        error_text = pending[:500]
                    yield chunk
                    continue
                pending += chunk.decode('utf-8', errors='ignore')
                pending = _parse_sse_usage(pending, usage)
                yield chunk
        finally:
            await resp.aclose()
            await client.aclose()
            latency = int((time.time() - started) * 1000)
            pt = int(usage.get('prompt_tokens') or 0)
            ct = int(usage.get('completion_tokens') or 0)
            _record(
                key, ip, requested_model or '', mapped or '', status_code, pt, ct,
                latency, ua, error_text, True, credit=_usage_credit(usage),
            )

    return StreamingResponse(generator(), status_code=status_code, media_type=content_type)


@router.post('/v1/chat/completions')
async def chat_v1(request: Request):
    return await _chat(request, '/v1/chat/completions')


@router.post('/v2/chat/completions')
async def chat_v2(request: Request):
    return await _chat(request, '/v2/chat/completions')


# ── 存活探测 ─────────────────────────────────────────────
@router.get('/healthz')
async def gateway_health() -> dict:
    try:
        async with config.http_client(5, connect=2) as client:
            resp = await client.get(f'{config.WB2API_BASE}/healthz')
        body = resp.json() if resp.headers.get('content-type', '').startswith('application/json') else {}
        return {'service': 'workbuddy-manager', 'upstream_ok': resp.status_code == 200, **body}
    except Exception as exc:  # noqa: BLE001
        return {'service': 'workbuddy-manager', 'upstream_ok': False, 'error': str(exc)}
