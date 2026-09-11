"""IP 解析与 CIDR 匹配工具。"""
from __future__ import annotations

import ipaddress

from fastapi import Request

from . import config


def client_ip(request: Request) -> str:
    """优先取反代透传的真实 IP（1Panel/OpenResty 会写入 X-Forwarded-For）。"""
    if config.TRUST_PROXY:
        xff = request.headers.get('x-forwarded-for')
        if xff:
            first = xff.split(',')[0].strip()
            if first:
                return first
        real = request.headers.get('x-real-ip')
        if real:
            return real.strip()
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
