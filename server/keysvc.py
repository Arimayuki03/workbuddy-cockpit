"""API 密钥的生成、校验与限额判定（网关与管理端共用）。"""
from __future__ import annotations

import hashlib
import json
import secrets
import time

from . import db
from .iputil import ip_matches

TOKEN_PREFIX = 'wbk_'


def _hash(token: str) -> str:
    return hashlib.sha256(token.encode()).hexdigest()


def _norm_realm(value: object) -> str:
    """归一化密钥的版本归属；'' = 不限制（存量密钥的形态，见 db._MIGRATIONS）。"""
    v = str(value or '').strip().lower()
    return v if v in ('cn', 'global') else ''


def _norm_cidrs(items: object) -> list[str]:
    """归一化 IP 白名单：逐项去空白、丢弃空项。

    **为什么必须在写入时归一化**（而不是只在路由里校验）：路由校验的是
    `str(raw).strip()`，若存库时存了**原始值**，两者就会不一致 ——
    实测 `" 10.0.0.0/8"`（带空格）能通过校验，存进库后
    `ip_matches()` 却匹配不上任何 IP（`ip_network` 解析失败），于是这把密钥
    **对所有来源都被拒绝**，而报错只说「不在白名单内」，用户完全看不出
    是自己粘进了一个多余空格。校验与存储必须是同一份数据。
    """
    if not isinstance(items, list):
        return []
    out: list[str] = []
    for raw in items:
        s = str(raw).strip()
        if s:
            out.append(s)
    return out


def _json_list(raw: object) -> list[str]:
    """把库里存的 JSON 数组文本解析成列表；**解析不了按「该列未设置」处理**。

    为什么必须容错：`ip_allowlist` / `models` 是 JSON 文本列，而读取它的
    `_parse` 被**列表接口**用到——只要**任意一把**密钥的这两列存了非法 JSON
    （历史版本写入过、手工改过库、写入被截断、JSON 编码变更），整个
    `GET /api/keys` 就抛 JSONDecodeError → 500，界面上**一把密钥都看不到**。

    而这会伪装成「创建失败」：新建密钥的 POST 本身是成功的（数据已入库），
    紧接着前端刷新列表才炸——用户看到 500 以为没建成，再点一次就多一把重复的。
    症状极具误导性（数据明明进去了），所以这里宁可少显示一个字段，
    也绝不让一列坏数据拖垮整个列表。

    **注意这里的方向**：解析失败按空列表返回，而 `validate` 里是
    `if allow and ...` / `if key['models'] and ...` —— 空列表意味着**跳过该项
    检查**（不限制 IP / 不限制模型）。也就是说坏数据会**放宽**该密钥，不是收紧。
    这是有意的取舍，理由见下：

      * 坏数据只可能来自**已经损坏的库**，而那种库在修复前整个密钥页都打不开，
        管理员连删掉它的入口都没有——修好列表是当务之急；
      * 「不限制」不等于「无保护」：密钥本身仍要过哈希校验、启停、有效期、
        配额与限流；IP 与模型白名单是**附加**收紧项，不是唯一防线；
      * 反向选择（坏数据一律拒绝）会让那把密钥的所有调用都失败，而管理员
        看不到任何原因（列表都打不开），排查成本更高。

    所以界面上会把「解析失败」当作空列表展示——管理员看到某把密钥的白名单
    是空的，若记得自己设过，就知道要重新设置一次。
    """
    if isinstance(raw, list):
        return [str(x) for x in raw if x]
    if not isinstance(raw, str) or not raw.strip():
        return []
    try:
        parsed = json.loads(raw)
    except (ValueError, TypeError):
        return []
    if not isinstance(parsed, list):
        return []
    return [str(x) for x in parsed if x]


def _parse(row) -> dict:
    return {
        'id': row['id'],
        'name': row['name'],
        'prefix': row['prefix'],
        'enabled': bool(row['enabled']),
        'expires_at': row['expires_at'],
        'max_ips': row['max_ips'],
        'ip_allowlist': _json_list(row['ip_allowlist']),
        'models': _json_list(row['models']),
        'realm': _norm_realm(row['realm']),
        'quota': row['quota'],
        'used_tokens': row['used_tokens'],
        'created_at': row['created_at'],
        'last_used_at': row['last_used_at'],
    }


def list_keys() -> list[dict]:
    rows = db.query('SELECT * FROM api_keys ORDER BY id DESC')
    return [_parse(r) for r in rows]


def create_key(
    name: str,
    expires_at: int | None = None,
    max_ips: int = 0,
    ip_allowlist: list[str] | None = None,
    models: list[str] | None = None,
    quota: int = 0,
    realm: str = '',
) -> dict:
    token = TOKEN_PREFIX + secrets.token_urlsafe(32)
    key_id = db.execute(
        'INSERT INTO api_keys(name, key_hash, prefix, enabled, expires_at, max_ips, ip_allowlist, models, realm, quota, used_tokens, created_at) '
        'VALUES(?, ?, ?, 1, ?, ?, ?, ?, ?, ?, 0, ?)',
        (
            name,
            _hash(token),
            token[:12],
            expires_at,
            max_ips,
            json.dumps(_norm_cidrs(ip_allowlist)),
            json.dumps(models or []),
            _norm_realm(realm),
            quota,
            int(time.time()),
        ),
    )
    row = db.query_one('SELECT * FROM api_keys WHERE id = ?', (key_id,))
    out = _parse(row)
    out['key'] = token  # 仅此一次返回明文
    return out


def update_key(key_id: int, patch: dict) -> dict | None:
    row = db.query_one('SELECT * FROM api_keys WHERE id = ?', (key_id,))
    if not row:
        return None
    fields: dict[str, object] = {}
    if 'name' in patch and patch['name']:
        fields['name'] = str(patch['name'])
    if 'enabled' in patch:
        fields['enabled'] = 1 if patch['enabled'] else 0
    if 'expires_at' in patch:
        fields['expires_at'] = patch['expires_at']
    if 'max_ips' in patch:
        fields['max_ips'] = int(patch['max_ips'] or 0)
    if 'ip_allowlist' in patch:
        # 与创建同一条归一化路径：校验与存储必须是同一份数据（见 _norm_cidrs）
        fields['ip_allowlist'] = json.dumps(_norm_cidrs(patch['ip_allowlist']))
    if 'models' in patch:
        fields['models'] = json.dumps(patch['models'] or [])
    if 'realm' in patch:
        # 显式传 null/'' 是**允许**的：管理员可以把旧密钥改回「不限制」。
        # 所以这里不能用 `patch['realm'] or ...` 之类的真值判断。
        fields['realm'] = _norm_realm(patch['realm'])
    if 'quota' in patch:
        fields['quota'] = int(patch['quota'] or 0)
    if fields:
        assignments = ', '.join(f'{k} = ?' for k in fields)
        db.execute(f'UPDATE api_keys SET {assignments} WHERE id = ?', (*fields.values(), key_id))
    return _parse(db.query_one('SELECT * FROM api_keys WHERE id = ?', (key_id,)))


def delete_key(key_id: int) -> bool:
    if not db.query_one('SELECT id FROM api_keys WHERE id = ?', (key_id,)):
        return False
    db.execute('DELETE FROM api_keys WHERE id = ?', (key_id,))
    db.execute('DELETE FROM api_key_ips WHERE key_id = ?', (key_id,))
    return True


def reset_usage(key_id: int) -> bool:
    """把已用 Token 归零。返回是否真的命中了密钥。

    返回布尔值是为了让路由能对「不存在的 id」报 404 —— 原来静默成功会让
    前端提示「已重置」，而实际什么都没发生。
    """
    if not db.query_one('SELECT id FROM api_keys WHERE id = ?', (key_id,)):
        return False
    db.execute('UPDATE api_keys SET used_tokens = 0 WHERE id = ?', (key_id,))
    return True


def resolve(token: str) -> dict | None:
    """按前缀定位后比对哈希，避免全表扫描。"""
    if not token or not token.startswith(TOKEN_PREFIX):
        return None
    rows = db.query('SELECT * FROM api_keys WHERE prefix = ?', (token[:12],))
    digest = _hash(token)
    for row in rows:
        if secrets.compare_digest(row['key_hash'], digest):
            return _parse(row)
    return None


def validate(key: dict, ip: str, model: str | None,
             *, is_model_list: bool = False) -> str | None:
    """返回 None 表示放行，否则返回拒绝原因。

    is_model_list：请求是 `/v1/models`（模型发现，不带 model）。版本归属在这种
    请求上不拦——它没有版本可言，拦了会让限定版本的密钥连「我有哪些模型」都
    问不到；真正的隔离由调用时的模型名把关（见下）。
    """
    if not key['enabled']:
        return '密钥已停用'
    if key['expires_at'] and key['expires_at'] < time.time():
        return '密钥已过期'
    if key['quota'] and key['used_tokens'] >= key['quota']:
        return '密钥配额已用尽'

    allow = key['ip_allowlist']
    if allow and not any(ip_matches(ip, c) for c in allow):
        return f'来源 IP {ip} 不在密钥白名单内'

    if key['max_ips']:
        known = db.query('SELECT ip FROM api_key_ips WHERE key_id = ?', (key['id'],))
        ips = {r['ip'] for r in known}
        if ip not in ips and len(ips) >= key['max_ips']:
            return f'密钥已绑定 {len(ips)} 个 IP，超出上限 {key["max_ips"]}'

    # 版本归属：密钥限定版本后，只能调用该版本的模型。
    #
    # 判定依据与实际路由**同一来源**：上游按模型名的 `global:` 前缀选账号池，
    # 所以「这次请求走哪个版本」由 model 决定（见 db.realm_of_model，日志与
    # 统计也用它）。用别的东西判（比如当前界面切到哪版）会与真实流量对不上。
    #
    # 缺 model 时不放行：那会走上游默认模型，而默认模型属于国内版——限定
    # 国际版的密钥反而能借此打到国内池，隔离就成了摆设。（模型白名单同理，
    # 下面那段是同一个道理的另一处。）
    want = _norm_realm(key.get('realm'))
    if want and not is_model_list:
        if not isinstance(model, str) or not model.strip():
            return f'该密钥限定了{"国际版" if want == "global" else "国内版"}模型，请求必须指定 model'
        got = 'global' if model.strip().lower().startswith('global:') else 'cn'
        if got != want:
            label = {'cn': '国内版', 'global': '国际版'}[want]
            other = '国际版' if want == 'cn' else '国内版'
            hint = '模型名需带 global: 前缀' if want == 'global' else '请去掉 global: 前缀'
            return f'该密钥仅限{label}模型，当前请求是{other}模型（{hint}）'

    # 模型白名单：**不能因为 model 缺失就跳过检查**。
    # 原写法 `if key['models'] and model and model not in ...` 在 body 不带 model
    # （或传空串/非字符串）时整段跳过，于是限定单模型的密钥可用「不带 model」的
    # 请求走上游默认模型——白名单形同虚设。请求侧已在网关把缺失/非法的 model
    # 拦成 400；这里再兜一层，任何非字符串或空值一律拒绝。
    if key['models'] and not is_model_list:
        if not isinstance(model, str) or not model.strip():
            return '请求未指定 model，而该密钥启用了模型白名单'
        if model not in key['models']:
            return f'模型 {model} 不在密钥白名单内'
    return None


def touch(key: dict, ip: str, tokens: int = 0) -> None:
    db.execute(
        'INSERT OR IGNORE INTO api_key_ips(key_id, ip, first_seen) VALUES(?, ?, ?)',
        (key['id'], ip, int(time.time())),
    )
    db.execute('UPDATE api_keys SET last_used_at = ? WHERE id = ?', (int(time.time()), key['id']))
    if tokens:
        db.execute('UPDATE api_keys SET used_tokens = used_tokens + ? WHERE id = ?', (tokens, key['id']))
