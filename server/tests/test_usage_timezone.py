"""用量统计的时区口径与重建（P0 回归）。

真实缺陷：写入用量用本地日期（time.strftime），回填却用 UTC
（SQLite 的 date(ts,'unixepoch')）。在 UTC+8 的机器上，凌晨 00:00-08:00
的调用会被算进两个不同的 day，回填时把同一次调用重复计数。
"""
from __future__ import annotations

import sys
import tempfile
import time
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from server import config, db  # noqa: E402


def local_midnight_plus(minutes: int) -> int:
    """本地今天 00:minutes —— 在 UTC+8 下其 UTC 日期是前一天。"""
    lt = time.localtime()
    return int(time.mktime((lt.tm_year, lt.tm_mon, lt.tm_mday, 0, minutes, 0, 0, 0, -1)))


class UsageTimezone(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self._orig = config.DB_PATH
        config.DB_PATH = Path(self._tmp.name) / 'u.db'
        db._conn = None
        db.connect()

    def tearDown(self) -> None:
        try:
            if db._conn is not None:
                db._conn.close()
        except Exception:  # noqa: BLE001
            pass
        db._conn = None
        config.DB_PATH = self._orig
        self._tmp.cleanup()

    def _seed(self, ts: int, pt: int = 100, ct: int = 50) -> None:
        db.execute(
            'INSERT INTO api_keys(name, key_hash, prefix, enabled, expires_at, max_ips, '
            'ip_allowlist, models, quota, used_tokens, created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)',
            ('k', 'h', 'wbk_abc', 1, None, 0, '[]', '[]', 0, 0, ts),
        )
        db.execute(
            'INSERT INTO request_logs(ts, key_id, ip, model, mapped_model, status, '
            'prompt_tokens, completion_tokens, latency_ms, ua, error, stream) '
            'VALUES(?,?,?,?,?,?,?,?,?,?,?,?)',
            (ts, 1, '1.2.3.4', 'glm-5.2', 'glm-5.2', 200, pt, ct, 10, 'ua', '', 0),
        )

    # ── 口径一致性 ──────────────────────────────────────
    def test_day_of_and_sql_agree(self) -> None:
        ts = local_midnight_plus(30)
        via_py = db.day_of(ts)
        via_sql = db.query_one(
            f'SELECT {db.day_sql("ts")} AS d FROM (SELECT {ts} AS ts)'
        )['d']
        self.assertEqual(via_py, via_sql, 'Python 与 SQL 的取日口径必须一致')

    def test_day_of_uses_local_not_utc(self) -> None:
        ts = local_midnight_plus(30)
        self.assertEqual(db.day_of(ts), time.strftime('%Y-%m-%d', time.localtime(ts)))
        # 该时刻的 UTC 日期可能不同，但我们的口径必须跟本地一致
        utc_day = time.strftime('%Y-%m-%d', time.gmtime(ts))
        self.assertEqual(db.day_of(ts), time.strftime('%Y-%m-%d', time.localtime(ts)))
        self.assertIsInstance(utc_day, str)

    # ── 不再重复计数 ────────────────────────────────────
    def test_backfill_after_bump_has_no_gap(self) -> None:
        """先按 bump_usage 语义写入本地日，再回填应无缺口。"""
        ts = local_midnight_plus(30)
        self._seed(ts)
        db.execute(
            'INSERT INTO usage_daily(day, key_id, model, requests, prompt_tokens, completion_tokens) '
            'VALUES(?,?,?,?,?,?)',
            (db.day_of(ts), 1, 'glm-5.2', 1, 100, 50),
        )
        res = db.backfill_usage_from_logs()
        self.assertEqual(res['repaired'], 0, '口径一致时不应产生回填')
        rows = db.query('SELECT day FROM usage_daily')
        self.assertEqual(len(rows), 1, '不应出现重复的 day 行')

    def test_backfill_does_not_create_second_day(self) -> None:
        ts = local_midnight_plus(30)
        self._seed(ts)
        db.backfill_usage_from_logs()
        days = [r['day'] for r in db.query('SELECT day FROM usage_daily')]
        self.assertEqual(days, [db.day_of(ts)])

    # ── 重建清理污染 ────────────────────────────────────
    def test_rebuild_removes_polluted_rows(self) -> None:
        ts = local_midnight_plus(30)
        self._seed(ts)
        utc_day = time.strftime('%Y-%m-%d', time.gmtime(ts))
        # 人为造出「本地日 + UTC 日」两行污染
        for d in {db.day_of(ts), utc_day}:
            db.execute(
                'INSERT INTO usage_daily(day, key_id, model, requests, prompt_tokens, completion_tokens) '
                'VALUES(?,?,?,?,?,?)',
                (d, 1, 'glm-5.2', 1, 100, 50),
            )
        before = db.query_one('SELECT SUM(requests) r FROM usage_daily')['r']
        self.assertGreaterEqual(before, 2)

        res = db.rebuild_usage_from_logs()
        after = db.query_one('SELECT COUNT(*) c, SUM(requests) r FROM usage_daily')
        self.assertEqual(after['c'], 1, '重建后应只剩一行')
        self.assertEqual(after['r'], 1, '重建后请求数应回落到原始值 1')
        self.assertEqual(res['requests_delta'], 1 - before)

    def test_rebuild_is_idempotent(self) -> None:
        ts = local_midnight_plus(30)
        self._seed(ts)
        db.rebuild_usage_from_logs()
        first = db.query_one('SELECT COUNT(*) c, SUM(requests) r FROM usage_daily')
        res2 = db.rebuild_usage_from_logs()
        second = db.query_one('SELECT COUNT(*) c, SUM(requests) r FROM usage_daily')
        self.assertEqual((first['c'], first['r']), (second['c'], second['r']))
        self.assertEqual(res2['requests_delta'], 0)

    def test_rebuild_keeps_multiple_days_separate(self) -> None:
        """不同两天的数据重建后仍应各占一行。"""
        now = int(time.time())
        for ts, pt in ((now, 100), (now - 86400, 200)):
            self._seed(ts, pt=pt)
        db.rebuild_usage_from_logs()
        rows = db.query('SELECT day, prompt_tokens FROM usage_daily ORDER BY day')
        self.assertEqual(len(rows), 2)
        self.assertEqual({r['prompt_tokens'] for r in rows}, {100, 200})


if __name__ == '__main__':
    unittest.main()
