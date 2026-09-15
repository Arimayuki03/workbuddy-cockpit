"""用量/日志的版本（realm）归属与过滤。

背景：管理端支持国内版与国际版（共用账号池，按模型名的 `cn:` / `global:`
前缀路由）。界面顶部有版本切换，但**用量统计与请求日志此前完全没有版本维度**
——切到国际版看到的还是两个版本混在一起的数据，页面上只能写一句
「含两种版本，不按版本过滤」来免责。

这次把 realm 落进数据层（`request_logs.realm` / `usage_daily.realm`），
让统计与日志真正能按版本切分。

判据来自**请求的模型名前缀**，不是调用方用哪把密钥：
网关把模型名原样转发给上游，由上游按前缀选择账号池 —— 所以「这次调用走了
哪个版本」完全由前缀决定。无前缀归 cn（存量客户端与历史数据都是这个形态，
归 cn 才能让它们落在原来的那一侧）。

本文件锁住：
  1. 前缀判定（含大小写、空白、裸名）
  2. 记录时写入正确的 realm
  3. 统计/日志按 realm 过滤真的生效
  4. 修复接口（repair / rebuild）**不会丢掉 realm**
"""
from __future__ import annotations

import os
import sys
import tempfile
import time
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

_TMP = tempfile.mkdtemp()
os.environ.setdefault('WB_DATA_DIR', _TMP)

from server import db  # noqa: E402


class RealmOfModelTest(unittest.TestCase):
    """前缀判定：上游的路由协议就是 `cn:` / `global:`。"""

    def test_global_prefix(self) -> None:
        for m in ('global:gpt-5.4', 'GLOBAL:gpt-5.4', ' global:gpt-5.4 '):
            self.assertEqual(db.realm_of_model(m), 'global', m)

    def test_cn_prefix_and_bare_name(self) -> None:
        for m in ('cn:glm-5.2', 'glm-5.2', 'deepseek-v4.1-flash'):
            self.assertEqual(db.realm_of_model(m), 'cn', m)

    def test_empty_is_cn(self) -> None:
        """缺 model 归 cn —— 与「历史数据无前缀」同一口径。"""
        for m in (None, '', '   '):
            self.assertEqual(db.realm_of_model(m), 'cn')


class RealmPersistTest(unittest.TestCase):
    """记录时写入 realm，且按 realm 能查出各自的量。"""

    def setUp(self) -> None:
        self._dir = Path(tempfile.mkdtemp())
        self._orig = (db.config.DB_PATH, db._conn)
        db.config.DB_PATH = self._dir / 'realm.db'
        db._conn = None
        db.connect()
        self.addCleanup(self._restore)

    def _restore(self) -> None:
        if db._conn is not None:
            db._conn.close()
        db._conn = self._orig[1]
        db.config.DB_PATH = self._orig[0]

    def _insert_log(self, model: str, pt: int = 10, ct: int = 5) -> None:
        realm = db.realm_of_model(model)
        db.execute(
            'INSERT INTO request_logs(ts, key_id, ip, model, mapped_model, status, '
            'prompt_tokens, completion_tokens, latency_ms, first_token_ms, ua, error, '
            'stream, credit, realm) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)',
            (int(time.time()), 1, '127.0.0.1', model, model, 200, pt, ct, 100, None,
             'ua', None, 1, 0.5, realm),
        )
        db.bump_usage(1, model, pt, ct, 0.5, realm=realm)

    def test_logs_carry_realm(self) -> None:
        self._insert_log('global:gpt-5.4')
        self._insert_log('glm-5.2')
        rows = {r['model']: r['realm'] for r in db.query('SELECT model, realm FROM request_logs')}
        self.assertEqual(rows['global:gpt-5.4'], 'global')
        self.assertEqual(rows['glm-5.2'], 'cn')

    def test_usage_split_by_realm(self) -> None:
        """两个版本走不同行累计，不互相污染。"""
        self._insert_log('global:gpt-5.4', pt=100, ct=50)
        self._insert_log('global:gpt-5.4', pt=100, ct=50)
        self._insert_log('glm-5.2', pt=7, ct=3)
        g = db.query_one("SELECT requests, prompt_tokens FROM usage_daily WHERE realm='global'")
        c = db.query_one("SELECT requests, prompt_tokens FROM usage_daily WHERE realm='cn'")
        self.assertEqual(int(g['requests']), 2)
        self.assertEqual(int(g['prompt_tokens']), 200)
        self.assertEqual(int(c['requests']), 1)
        self.assertEqual(int(c['prompt_tokens']), 7)

    def test_relogin_repair_keeps_realm(self) -> None:
        """rebuild 以日志为准重建 —— 不能把 realm 丢掉（否则版本过滤失效）。"""
        self._insert_log('global:gpt-5.4', pt=11, ct=1)
        self._insert_log('glm-5.2', pt=22, ct=2)
        db.rebuild_usage_from_logs()
        realms = {r['realm'] for r in db.query('SELECT DISTINCT realm FROM usage_daily')}
        self.assertEqual(realms, {'global', 'cn'}, '重建后 realm 丢失/错误')

    def test_backfill_keeps_realm_and_does_not_merge(self) -> None:
        """回填的两个版本同名模型必须是两行（键含 realm）。"""
        # 同名模型分别落在两个版本（理论上不同版本可以有同名模型）
        for m in ('global:shared-model', 'cn:shared-model'):
            realm = db.realm_of_model(m)
            db.execute(
                'INSERT INTO request_logs(ts, key_id, ip, model, mapped_model, status, '
                'prompt_tokens, completion_tokens, latency_ms, first_token_ms, ua, error, '
                'stream, credit, realm) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)',
                (int(time.time()), 1, '127.0.0.1', m, m, 200, 5, 5, 10, None,
                 'ua', None, 1, 0.1, realm),
            )
        db.backfill_usage_from_logs()
        rows = db.query("SELECT model, realm, requests FROM usage_daily WHERE model LIKE '%shared-model'")
        self.assertEqual(len(rows), 2, '两个版本的同名模型被合并成一行了')
        self.assertEqual({r['realm'] for r in rows}, {'global', 'cn'})


class StatsRealmFilterTest(unittest.TestCase):
    """统计接口按 realm 过滤（端到端，走 HTTP）。"""

    def setUp(self) -> None:
        import server.security as sec
        from fastapi.testclient import TestClient
        from server.main import app

        self._dir = Path(tempfile.mkdtemp())
        self._orig = (db.config.DB_PATH, db._conn)
        db.config.DB_PATH = self._dir / 'stats.db'
        db._conn = None
        db.connect()
        db.execute("INSERT INTO api_keys(name, key_hash, prefix, enabled, created_at) "
                   "VALUES('k','h','p',1,?)", (int(time.time()),))
        # 两个版本各写 3 次 / 1 次
        for _ in range(3):
            self._log('global:gpt-5.4', 100, 50)
        self._log('glm-5.2', 7, 3)
        app.dependency_overrides[sec.current_user] = lambda: {'username': 't', 'role': 'admin'}
        self.c = TestClient(app)
        self.addCleanup(self._restore)
        self.addCleanup(app.dependency_overrides.clear)

    def _restore(self) -> None:
        if db._conn is not None:
            db._conn.close()
        db._conn = self._orig[1]
        db.config.DB_PATH = self._orig[0]

    def _log(self, model: str, pt: int, ct: int) -> None:
        realm = db.realm_of_model(model)
        db.execute(
            'INSERT INTO request_logs(ts, key_id, ip, model, mapped_model, status, '
            'prompt_tokens, completion_tokens, latency_ms, first_token_ms, ua, error, '
            'stream, credit, realm) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)',
            (int(time.time()), 1, '127.0.0.1', model, model, 200, pt, ct, 10, None,
             'ua', None, 1, 0.2, realm),
        )
        db.bump_usage(1, model, pt, ct, 0.2, realm=realm)

    def test_summary_filters_by_realm(self) -> None:
        all_ = self.c.get('/api/stats/summary').json()
        gl = self.c.get('/api/stats/summary?realm=global').json()
        cn = self.c.get('/api/stats/summary?realm=cn').json()
        self.assertEqual(all_['today_requests'], 4)
        self.assertEqual(gl['today_requests'], 3)
        self.assertEqual(cn['today_requests'], 1)
        # token：global 3×(100+50)=450；cn 10
        self.assertEqual(gl['today_tokens'], 450)
        self.assertEqual(cn['today_tokens'], 10)

    def test_by_model_filters_by_realm(self) -> None:
        gl = self.c.get('/api/stats/by-model?realm=global').json()
        cn = self.c.get('/api/stats/by-model?realm=cn').json()
        self.assertEqual([m['name'] for m in gl], ['global:gpt-5.4'])
        self.assertEqual([m['name'] for m in cn], ['glm-5.2'])

    def test_by_key_includes_realm_scoped_amounts(self) -> None:
        """密钥不分版本，但按版本时统计的是「该版本上的调用量」——两侧都出现。"""
        gl = self.c.get('/api/stats/by-key?realm=global').json()
        cn = self.c.get('/api/stats/by-key?realm=cn').json()
        self.assertEqual(sum(k['requests'] for k in gl), 3)
        self.assertEqual(sum(k['requests'] for k in cn), 1)

    def test_daily_filters_by_realm(self) -> None:
        gl = self.c.get('/api/stats/daily?days=7&realm=global').json()
        cn = self.c.get('/api/stats/daily?days=7&realm=cn').json()
        self.assertEqual(sum(d['requests'] for d in gl), 3)
        self.assertEqual(sum(d['requests'] for d in cn), 1)

    def test_logs_filter_by_realm(self) -> None:
        gl = self.c.get('/api/logs?realm=global').json()
        cn = self.c.get('/api/logs?realm=cn').json()
        self.assertEqual(gl['total'], 3)
        self.assertEqual(cn['total'], 1)
        # 每条记录带回 realm，供界面标注
        self.assertTrue(all(i['realm'] == 'global' for i in gl['items']))

    def test_null_realm_counts_as_cn(self) -> None:
        """历史记录 realm 为 NULL → 按 cn 归类（与 realm_of_model 一致）。"""
        db.execute(
            'INSERT INTO request_logs(ts, key_id, ip, model, mapped_model, status, '
            'prompt_tokens, completion_tokens, latency_ms, first_token_ms, ua, error, '
            'stream, credit, realm) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL)',
            (int(time.time()), 1, '127.0.0.1', 'old-model', 'old-model', 200, 1, 1,
             10, None, 'ua', None, 1, 0.0),
        )
        cn = self.c.get('/api/logs?realm=cn').json()
        gl = self.c.get('/api/logs?realm=global').json()
        self.assertEqual(cn['total'], 2, 'NULL 的历史记录应算作国内版')
        self.assertEqual(gl['total'], 3)

    def test_no_realm_returns_everything(self) -> None:
        """不传 realm 时保持既有行为（全量），避免破坏旧的调用方。"""
        all_ = self.c.get('/api/logs').json()
        self.assertEqual(all_['total'], 4)


class RealmColumnMigrationTest(unittest.TestCase):
    """老库升级：加列必须成功，且历史行有合理的默认归类。"""

    def test_migration_adds_columns(self) -> None:
        import sqlite3
        d = Path(tempfile.mkdtemp())
        old_path = db.config.DB_PATH
        old_conn = db._conn
        try:
            # 造一个「没有 realm 列」的老库
            db.config.DB_PATH = d / 'old.db'
            db._conn = None
            conn = sqlite3.connect(str(db.config.DB_PATH))
            conn.executescript(
                'CREATE TABLE request_logs (id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, '
                'key_id INTEGER, ip TEXT, model TEXT, mapped_model TEXT, status INTEGER, '
                'prompt_tokens INTEGER, completion_tokens INTEGER, latency_ms INTEGER, '
                'ua TEXT, error TEXT, stream INTEGER, credit REAL);'
                'CREATE TABLE usage_daily (day TEXT NOT NULL, key_id INTEGER NOT NULL, '
                'model TEXT NOT NULL, requests INTEGER DEFAULT 0, prompt_tokens INTEGER DEFAULT 0, '
                'completion_tokens INTEGER DEFAULT 0, credit REAL DEFAULT 0, '
                'PRIMARY KEY (day, key_id, model));'
            )
            conn.commit()
            conn.close()

            db.connect()  # 触发迁移
            log_cols = {r[1] for r in db._conn.execute('PRAGMA table_info(request_logs)')}
            use_cols = {r[1] for r in db._conn.execute('PRAGMA table_info(usage_daily)')}
            self.assertIn('realm', log_cols, 'request_logs 未加上 realm 列')
            self.assertIn('realm', use_cols, 'usage_daily 未加上 realm 列')
        finally:
            if db._conn is not None:
                db._conn.close()
            db._conn = old_conn
            db.config.DB_PATH = old_path


if __name__ == '__main__':
    unittest.main()


class BumpUsageRealmOverrideTest(unittest.TestCase):
    """`bump_usage(realm=...)` 的显式传参必须优先于按模型名推导。

    为什么这值得单独测：在当前调用链里两者**恰好一致**（网关切传的就是
    db.realm_of_model 的结果），所以「忽略参数、只按模型推导」的实现也能通过
    其余全部用例 —— 这一点是被反证试出来的。而语义上「显式指定优先」是有
    意义的：将来若改成按密钥或请求头决定版本，调用方就得能覆盖推导值。
    """

    def setUp(self) -> None:
        self._dir = Path(tempfile.mkdtemp())
        self._orig = (db.config.DB_PATH, db._conn)
        db.config.DB_PATH = self._dir / 'override.db'
        db._conn = None
        db.connect()
        self.addCleanup(self._restore)

    def _restore(self) -> None:
        if db._conn is not None:
            db._conn.close()
        db._conn = self._orig[1]
        db.config.DB_PATH = self._orig[0]

    def test_explicit_realm_wins_over_prefix(self) -> None:
        # 模型名没有前缀（推导=cn），但显式要求记到 global
        db.bump_usage(1, 'glm-5.2', 10, 5, 0.1, realm='global')
        rows = db.query('SELECT realm FROM usage_daily')
        self.assertEqual([r['realm'] for r in rows], ['global'],
                         '显式传入的 realm 被忽略了 —— 应以参数为准')

    def test_fallback_to_prefix_when_omitted(self) -> None:
        """不传 realm 时按模型前缀兜底（调用方遗漏时不至于全部归 cn）。"""
        db.bump_usage(1, 'global:gpt-5.4', 10, 5, 0.1)
        rows = db.query('SELECT realm FROM usage_daily')
        self.assertEqual([r['realm'] for r in rows], ['global'])

    def test_invalid_realm_falls_back_to_prefix(self) -> None:
        """非法值（非 cn/global）不能写进库 —— 退回推导值。"""
        db.bump_usage(1, 'global:gpt-5.4', 10, 5, 0.1, realm='weird')
        rows = db.query('SELECT realm FROM usage_daily')
        self.assertEqual([r['realm'] for r in rows], ['global'])
