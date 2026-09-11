"""IP 解析与 CIDR 匹配工具。"""
from __future__ import annotations

import ipaddress

from fastapi import Request

from . import config


def _clean_ip(value: str) -> str:
    """去掉端口与方括号，兼容 IPv6 形如 [::1]:1234。"""
    raw = (value or '').strip()
    if not raw:
        return ''
    if raw.startswith('['):  # [::1]:1234
        end = raw.find(']')
        return raw[1:end] if end > 0 else raw
    # 纯 IPv6 不处理；IPv4:port 去掉端口
    if raw.count(':') == 1:
        host, _, port = raw.partition(':')
        if port.isdigit():
            return host
    return raw


def client_ip(request: Request) -> str:
    """解析真实客户端 IP。

    安全要点（曾存在漏洞）：`X-Forwarded-For` 是**客户端可伪造**的 ——
    反向代理通常用 `$proxy_add_x_forwarded_for` **追加**而非覆盖，
    因此第一个元素完全由请求方控制。若直接取第一个值，攻击者可冒充任意
    IP，从而绕过 IP 白/黑名单、每密钥 IP 限制与登录失败锁定。

    取值优先级（按可信度）：
      1) `X-Real-IP` —— 本机反代以 `$remote_addr` **覆盖**写入，不可伪造
      2) `X-Forwarded-For` —— 从**右往左**数第 N 个（N 为可信代理跳数）；
         右侧是本机反代追加的真实地址，左侧才是可伪造部分
      3) TCP 对端地址

    若前面还挂了 CDN，请把 `WB_TRUSTED_PROXY_HOPS` 调成 CDN + 反代的层数。
    """
    if not config.TRUST_PROXY:
        return request.client.host if request.client else '0.0.0.0'

    real = _clean_ip(request.headers.get('x-real-ip', ''))
    if real:
        return real

    xff = request.headers.get('x-forwarded-for', '')
    if xff:
        parts = [p for p in (_clean_ip(p) for p in xff.split(',')) if p]
        if parts:
            hops = max(1, config.TRUSTED_PROXY_HOPS)
            idx = len(parts) - hops
            return parts[idx] if idx >= 0 else parts[0]

    return request.client.host if request.client else '0.0.0.0'


def ip_matches(ip: str, cidr: str) -> bool:
    """支持单 IP 与 CIDR；非法输入一律不匹配。"""
    try:
        addr = ipaddress.ip_address(ip)
        try:
            net = ipaddress.ip_network(cidr, strict=False)
        except ValueError:
            return False
        return addr in net
    except ValueError:
        return False


def evaluate(ip: str, rules: list[dict], mode: str) -> bool:
    """返回 True 表示放行。mode: whitelist（默认拒绝）| blacklist（默认放行）。"""
    if not rules:
        return mode != 'whitelist'
    allow = [r for r in rules if r['kind'] == 'allow']
    deny = [r for r in rules if r['kind'] == 'deny']
    if any(ip_matches(ip, r['cidr']) for r in deny):
        return False
    if mode == 'whitelist':
        return any(ip_matches(ip, r['cidr']) for r in allow)
    return True
