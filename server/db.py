"""SQLite 存储层：密钥、日志、用量、IP 规则与全局设置。"""
from __future__ import annotations

import json
import sqlite3
import threading
import time
from typing import Any, Iterable

from . import config

_lock = threading.RLock()
_conn: sqlite3.Connection | None = None

SCHEMA = """
CREATE TABLE IF NOT EXISTS api_keys (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  name          TEXT    NOT NULL,
  key_hash      TEXT    NOT NULL,
  prefix        TEXT    NOT NULL,
  enabled       INTEGER NOT NULL DEFAULT 1,
  expires_at    INTEGER,
  max_ips       INTEGER NOT NULL DEFAULT 0,
  ip_allowlist  TEXT    NOT NULL DEFAULT '[]',
  models        TEXT    NOT NULL DEFAULT '[]',
  quota         INTEGER NOT NULL DEFAULT 0,
  used_tokens   INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  last_used_at  INTEGER
);
CREATE INDEX IF NOT EXISTS idx_keys_prefix ON api_keys(prefix);

CREATE TABLE IF NOT EXISTS api_key_ips (
  key_id     INTEGER NOT NULL,
  ip         TEXT    NOT NULL,
  first_seen INTEGER NOT NULL,
  PRIMARY KEY (key_id, ip)
);

CREATE TABLE IF NOT EXISTS request_logs (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  ts                INTEGER NOT NULL,
  key_id            INTEGER,
  ip                TEXT,
  model             TEXT,
  mapped_model      TEXT,
  status            INTEGER DEFAULT 0,
  prompt_tokens     INTEGER DEFAULT 0,
  completion_tokens INTEGER DEFAULT 0,
  latency_ms        INTEGER DEFAULT 0,
  ua                TEXT,
  error             TEXT,
  stream            INTEGER DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_logs_ts ON request_logs(ts);

CREATE TABLE IF NOT EXISTS usage_daily (
  day               TEXT    NOT NULL,
  key_id            INTEGER NOT NULL,
  model             TEXT    NOT NULL,
  requests          INTEGER NOT NULL DEFAULT 0,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (day, key_id, model)
);

CREATE TABLE IF NOT EXISTS ip_rules (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  kind       TEXT    NOT NULL,
  cidr       TEXT    NOT NULL,
  note       TEXT    NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS ip_access_logs (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  ts      INTEGER NOT NULL,
  ip      TEXT,
  path    TEXT,
  blocked INTEGER NOT NULL DEFAULT 0,
  ua      TEXT
);
CREATE INDEX IF NOT EXISTS idx_ip_logs_ts ON ip_access_logs(ts);

CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT
);

-- 签到 / 保活结果记录。上游只在失败时打日志、成功静默，
-- 因此本表用于留下我们自己触发的签到结果，便于事后追溯。
CREATE TABLE IF NOT EXISTS checkin_logs (
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  ts       INTEGER NOT NULL,
  uid      TEXT,
  nickname TEXT,
  source   TEXT NOT NULL DEFAULT 'manual',
  kind     TEXT NOT NULL DEFAULT 'checkin',
  success  INTEGER NOT NULL DEFAULT 0,
  code     INTEGER,
  message  TEXT
);
CREATE INDEX IF NOT EXISTS idx_checkin_ts ON checkin_logs(ts);
"""


def connect() -> sqlite3.Connection:
    global _conn
    if _conn is None:
        config.ensure_dirs()
        _conn = sqlite3.connect(str(config.DB_PATH), check_same_thread=False)
        _conn.row_factory = sqlite3.Row
        _conn.execute('PRAGMA journal_mode=WAL')
        _conn.execute('PRAGMA synchronous=NORMAL')
        _conn.executescript(SCHEMA)
        _conn.commit()
    return _conn


def query(sql: str, args: Iterable[Any] = ()) -> list[sqlite3.Row]:
    with _lock:
        return list(connect().execute(sql, tuple(args)).fetchall())


def query_one(sql: str, args: Iterable[Any] = ()) -> sqlite3.Row | None:
    with _lock:
        return connect().execute(sql, tuple(args)).fetchone()


def execute(sql: str, args: Iterable[Any] = ()) -> int:
    with _lock:
        conn = connect()
        cur = conn.execute(sql, tuple(args))
        conn.commit()
        return int(cur.lastrowid or 0)


def executemany(sql: str, seq: Iterable[Iterable[Any]]) -> None:
    with _lock:
        conn = connect()
        conn.executemany(sql, [tuple(x) for x in seq])
        conn.commit()


# ── settings ─────────────────────────────────────────────
def get_setting(key: str, default: Any = None) -> Any:
    row = query_one('SELECT value FROM settings WHERE key = ?', (key,))
    if not row:
        return default
    try:
        return json.loads(row['value'])
    except Exception:
        return default


def set_setting(key: str, value: Any) -> None:
    execute(
        'INSERT INTO settings(key, value) VALUES(?, ?) '
        'ON CONFLICT(key) DO UPDATE SET value = excluded.value',
        (key, json.dumps(value, ensure_ascii=False)),
    )


# ── 用量累计 ─────────────────────────────────────────────
def bump_usage(key_id: int, model: str, prompt_tokens: int, completion_tokens: int) -> None:
    day = time.strftime('%Y-%m-%d')
    execute(
        'INSERT INTO usage_daily(day, key_id, model, requests, prompt_tokens, completion_tokens) '
        'VALUES(?, ?, ?, 1, ?, ?) '
        'ON CONFLICT(day, key_id, model) DO UPDATE SET '
        '  requests = requests + 1, '
        '  prompt_tokens = prompt_tokens + excluded.prompt_tokens, '
        '  completion_tokens = completion_tokens + excluded.completion_tokens',
        (day, key_id, model, prompt_tokens, completion_tokens),
    )


# ── 签到 / 保活记录 ──────────────────────────────────────
def add_checkin_log(
    uid: str,
    nickname: str,
    source: str,
    success: bool,
    code: int | None = None,
    message: str = '',
    kind: str = 'checkin',
) -> None:
    execute(
        'INSERT INTO checkin_logs(ts, uid, nickname, source, kind, success, code, message) '
        'VALUES(?, ?, ?, ?, ?, ?, ?, ?)',
        (int(time.time()), uid or '', nickname or '', source, kind, 1 if success else 0, code, message),
    )


def list_checkin_logs(limit: int = 200, uid: str | None = None) -> list[dict]:
    if uid:
        rows = query(
            'SELECT * FROM checkin_logs WHERE uid = ? ORDER BY id DESC LIMIT ?',
            (uid, min(1000, max(1, limit))),
        )
    else:
        rows = query('SELECT * FROM checkin_logs ORDER BY id DESC LIMIT ?', (min(1000, max(1, limit)),))
    return [
        {
            'id': r['id'],
            'ts': r['ts'],
            'uid': r['uid'],
            'nickname': r['nickname'],
            'source': r['source'],
            'kind': r['kind'],
            'success': bool(r['success']),
            'code': r['code'],
            'message': r['message'],
        }
        for r in rows
    ]


def clear_checkin_logs() -> None:
    execute('DELETE FROM checkin_logs')


# ── 用量回填 ─────────────────────────────────────────────
def backfill_usage_from_logs() -> dict:
    """把 request_logs 里尚未计入 usage_daily 的用量补进统计。

    用途：修复历史缺陷（曾因统计函数缺失，导致部分调用的用量没有累计）。
    以「已记录的调用」推算应有用量，再把差额写入 usage_daily，
    因此可重复执行而不会重复计数。
    """
    # 应有用量（按天 × 密钥 × 模型）
    expected = query(
        "SELECT date(ts,'unixepoch') AS day, key_id, COALESCE(model,'') AS model, "
        "COUNT(*) AS requests, COALESCE(SUM(prompt_tokens),0) AS pt, COALESCE(SUM(completion_tokens),0) AS ct "
        "FROM request_logs WHERE key_id IS NOT NULL GROUP BY day, key_id, model"
    )
    current = {
        (r['day'], r['key_id'], r['model']): r
        for r in query('SELECT day, key_id, model, requests, prompt_tokens, completion_tokens FROM usage_daily')
    }

    fixed = 0
    added_requests = added_tokens = 0
    for row in expected:
        key = (row['day'], row['key_id'], row['model'])
        cur = current.get(key)
        cur_req = int(cur['requests']) if cur else 0
        cur_pt = int(cur['prompt_tokens']) if cur else 0
        cur_ct = int(cur['completion_tokens']) if cur else 0

        d_req = int(row['requests']) - cur_req
        d_pt = int(row['pt']) - cur_pt
        d_ct = int(row['ct']) - cur_ct
        if d_req <= 0 and d_pt <= 0 and d_ct <= 0:
            continue
        execute(
            'INSERT INTO usage_daily(day, key_id, model, requests, prompt_tokens, completion_tokens) '
            'VALUES(?, ?, ?, ?, ?, ?) '
            'ON CONFLICT(day, key_id, model) DO UPDATE SET '
            '  requests = MAX(requests, excluded.requests), '
            '  prompt_tokens = MAX(prompt_tokens, excluded.prompt_tokens), '
            '  completion_tokens = MAX(completion_tokens, excluded.completion_tokens)',
            (row['day'], row['key_id'], row['model'], int(row['requests']), int(row['pt']), int(row['ct'])),
        )
        fixed += 1
        added_requests += max(0, d_req)
        added_tokens += max(0, d_pt) + max(0, d_ct)

    return {
        'repaired': fixed,
        'requests': added_requests,
        'tokens': added_tokens,
    }
