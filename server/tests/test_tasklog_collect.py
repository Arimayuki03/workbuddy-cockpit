"""任务日志采集入库与去重的回归测试（使用独立临时数据库）。"""
from __future__ import annotations

import asyncio
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from server import config, db  # noqa: E402
from server.services import tasklog, wb2api  # noqa: E402

SAMPLE = [
    'listening on :7863',
    '2026-09-11T09:00:01.111111111Z 2026/09/11 09:00:01 travel 89374120: depart ok location=7',
    '2026-09-11T09:00:02.222222222Z 2026/09/11 09:00:02 travel 89374120: claim ok record=12 reward=100',
    '2026-09-11T09:00:03.333333333Z 2026/09/11 09:00:03 travel 89374120: adopt ok (+300 credits)',
    '2026-09-11T10:00:00.444444444Z 2026/09/11 10:00:00 activity 89374120: streak days=3',
    '2026-09-11T21:00:00.555555555Z 2026/09/11 21:00:00 travel 91203877: skip (daily limit reached)',
    '2026-09-11T22:00:00.666666666Z 2026/09/11 22:00:00 keepalive 91203877: 连续 3 次 12153 session dead — 禁用',
    '2026-09-11T22:30:00.777777777Z 2026/09/11 22:30:00 checkin 91203877: context deadline exceeded',
]


class CollectPipeline(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self._orig_db = config.DB_PATH
        config.DB_PATH = Path(self._tmp.name) / 'test.db'
        db._conn = None          # 强制用新的临时库
        db.connect()
        self._orig_reader = wb2api.read_container_logs
        wb2api.read_container_logs = lambda limit=200, timestamps=True: SAMPLE

    def tearDown(self) -> None:
        wb2api.read_container_logs = self._orig_reader
        try:
            if db._conn is not None:
                db._conn.close()
        except Exception:  # noqa: BLE001
            pass
        db._conn = None
        config.DB_PATH = self._orig_db
        self._tmp.cleanup()

    def test_first_collect_parses_and_persists(self) -> None:
        parsed, added = asyncio.run(tasklog._collect_once())
        # 7 行任务日志（'listening on' 被忽略）
        self.assertEqual(parsed, 7)
        self.assertEqual(added, 7)

    def test_second_collect_is_deduplicated(self) -> None:
        asyncio.run(tasklog._collect_once())
        parsed, added = asyncio.run(tasklog._collect_once())
        self.assertEqual(parsed, 7)
        self.assertEqual(added, 0, '重复采集同一条日志不应重复入库')
        self.assertEqual(db.task_log_stats()['total'], 7)

    def test_credit_totals(self) -> None:
        asyncio.run(tasklog._collect_once())
        stats = db.task_log_stats()
        # 100 (claim) + 300 (adopt)
        self.assertEqual(stats['total_credits'], 400)
        self.assertEqual(stats['by_kind']['travel']['credits'], 400)
        self.assertEqual(stats['by_kind']['travel']['count'], 4)

    def test_levels_classified(self) -> None:
        asyncio.run(tasklog._collect_once())
        by_msg = {l['message']: l['level'] for l in db.list_task_logs(limit=50)}
        self.assertEqual(by_msg['claim ok record=12 reward=100'], 'credit')
        self.assertEqual(by_msg['adopt ok (+300 credits)'], 'credit')
        self.assertEqual(by_msg['depart ok location=7'], 'ok')
        self.assertEqual(by_msg['streak days=3'], 'ok')
        self.assertEqual(by_msg['skip (daily limit reached)'], 'info')
        self.assertEqual(
            by_msg['连续 3 次 12153 session dead — 禁用'], 'error'
        )

    def test_filter_by_kind_and_uid(self) -> None:
        asyncio.run(tasklog._collect_once())
        travel = db.list_task_logs(limit=50, kind='travel')
        self.assertEqual(len(travel), 4)
        self.assertTrue(all(l['kind'] == 'travel' for l in travel))

        one = db.list_task_logs(limit=50, uid='89374120')
        self.assertTrue(all(l['uid'] == '89374120' for l in one))
        self.assertEqual(len(one), 4)

    def test_clear(self) -> None:
        asyncio.run(tasklog._collect_once())
        db.clear_task_logs()
        self.assertEqual(db.task_log_stats()['total'], 0)


class NicknameResolution(unittest.TestCase):
    """上游日志只带 uid 前 8 位；接口要能解析出昵称。"""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self._orig_db = config.DB_PATH
        config.DB_PATH = Path(self._tmp.name) / 'n.db'
        db._conn = None
        db.connect()
        self._orig_list = wb2api.list_auth_accounts

    def tearDown(self) -> None:
        wb2api.list_auth_accounts = self._orig_list
        try:
            if db._conn is not None:
                db._conn.close()
        except Exception:  # noqa: BLE001
            pass
        db._conn = None
        config.DB_PATH = self._orig_db
        self._tmp.cleanup()

    def _resolve(self, row_uid: str, accounts: list[dict]) -> str:
        wb2api.list_auth_accounts = lambda: accounts  # type: ignore[assignment]
        db.clear_task_logs()
        db.add_task_logs([{
            'ts': 1, 'uid': row_uid, 'kind': 'travel', 'level': 'ok',
            'credits': 0, 'message': 'depart ok location=1',
            'dedup_key': f'nick-{row_uid}',
        }])
        # 复用路由里的解析逻辑
        from server.routers import accounts as accounts_router
        res = accounts_router.task_logs(limit=10, uid=None, kind=None, user={})
        return res['logs'][0].get('nickname', '')

    def test_full_uid_matches(self) -> None:
        got = self._resolve(
            '9b212d8c-f5f7-4ad6-aa20-1d576508c8c1',
            [{'uid': '9b212d8c-f5f7-4ad6-aa20-1d576508c8c1', 'nickname': '黑天鹅'}],
        )
        self.assertEqual(got, '黑天鹅')

    def test_truncated_prefix_matches(self) -> None:
        """上游截断后的 8 位前缀也要能对上昵称。"""
        got = self._resolve(
            '9b212d8c',
            [{'uid': '9b212d8c-f5f7-4ad6-aa20-1d576508c8c1', 'nickname': '黑天鹅'}],
        )
        self.assertEqual(got, '黑天鹅')

    def test_unknown_uid_returns_empty(self) -> None:
        got = self._resolve('ffffffff', [{'uid': 'aaaaaaaa-1111', 'nickname': '别人'}])
        self.assertEqual(got, '')


if __name__ == '__main__':
    unittest.main()
