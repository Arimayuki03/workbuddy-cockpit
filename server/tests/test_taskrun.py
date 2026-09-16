"""成长任务一键执行（issue #19）的回归测试。

这个功能会对腾讯发起**真实写请求**，所以测试的重点不是「跑得通」，而是
「不该发生的事不会发生」：

  1. 命令注入：账号标识直接进 argv，必须做字符白名单（`a;b`、`../x`、空串都得拒）。
  2. `full`（点亮）必须显式确认：它是唯一会伪造活跃上报的模式，手滑点到的代价
     是账号风控。接口层不加确认就等同把风险最高的操作变成一键。
  3. 定时只跑 `claim`：领奖是幂等的、不伪造行为；点亮绝不能进定时。
  4. 并发保护：一次只跑一个（叠着跑会放大风控信号，输出也会交错）。
  5. 上游脚本不存在时**前置拒绝**并说清怎么办，而不是跑到一半失败。
"""
from __future__ import annotations

import asyncio
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from fastapi.testclient import TestClient  # noqa: E402

from server import config, db, security  # noqa: E402
from server.services import taskrun  # noqa: E402


class SummarizeTest(unittest.TestCase):
    """结果汇总要取**结果行**，不是参数行。

    实测踩到过：脚本开头打 `mode=DRY-RUN accounts=[...]`（运行参数），结尾才是
    `task_runner done: ok=3 credit=+100`（结果）。按 `mode=` 匹配会把参数行当结果
    记进历史，等于没记结果。
    """

    def setUp(self) -> None:
        self._saved = dict(taskrun._state)

    def tearDown(self) -> None:
        taskrun._state.update(self._saved)

    def test_picks_done_line_not_mode_line(self) -> None:
        taskrun._state.update({
            'lines': [
                "mode=DRY-RUN accounts=['99a07e71'] only=all only_claim=False gap=1.0",
                '== 99a07e71 (测试号) ==',
                'task_runner done: accounts=1 total=0 ok=3 already=1 fail=0 credit=+100',
            ],
            'exit_code': 0, 'error': '', 'timed_out': False,
        })
        out = taskrun._summarize()
        self.assertTrue(out.startswith('task_runner done:'), out)
        self.assertNotIn('mode=', out, '抓到参数行了')
        self.assertIn('credit=+100', out)

    def test_falls_back_to_error(self) -> None:
        taskrun._state.update({'lines': [], 'exit_code': None,
                               'error': '无法启动脚本：No such file', 'timed_out': False})
        self.assertIn('无法启动脚本', taskrun._summarize())

    def test_falls_back_to_exit_code(self) -> None:
        taskrun._state.update({'lines': ['some output'], 'exit_code': 3,
                               'error': '', 'timed_out': False})
        self.assertIn('3', taskrun._summarize())


class CommandBuildTest(unittest.TestCase):
    """argv 构造：模式白名单 + 账号标识字符校验。"""

    def test_modes_map_to_expected_flags(self) -> None:
        prev = taskrun.build_command('preview', 'ALL')
        self.assertNotIn('--yes', prev, 'preview 必须是 dry-run（只读）')
        claim = taskrun.build_command('claim', 'ALL')
        self.assertIn('--yes', claim)
        self.assertIn('--only-claim', claim)
        full = taskrun.build_command('full', 'ALL')
        self.assertIn('--yes', full)
        self.assertNotIn('--only-claim', full)

    def test_account_injection_rejected(self) -> None:
        for bad in ('a;b', 'a b', '../x', 'a|b', '$(whoami)', '`id`', '', 'x' * 65,
                    'a\nb', 'a&b'):
            with self.assertRaises(ValueError, msg=repr(bad)):
                taskrun.build_command('claim', bad)

    def test_legit_account_accepted(self) -> None:
        for good in ('ALL', '99a07e71', 'abcdef01', 'u-in', 'A' * 64):
            cmd = taskrun.build_command('claim', good)
            self.assertEqual(cmd[2], good, '账号标识应作为独立 argv 传入')

    def test_unknown_mode_rejected(self) -> None:
        for bad in ('', 'FULL', 'full; rm -rf /', 'previewx', None):
            with self.assertRaises(ValueError):
                taskrun.build_command(bad, 'ALL')  # type: ignore[arg-type]


class AvailabilityTest(unittest.TestCase):
    def test_reports_reason_when_script_missing(self) -> None:
        """脚本缺失要**说明是什么、怎么办**，而不是只回「失败」。"""
        with mock.patch.object(taskrun, '_script_path', lambda: Path('/nope/task_runner.py')):
            ok, why = taskrun.available()
        self.assertFalse(ok)
        self.assertIn('task_runner.py', why)
        self.assertIn('workbuddy2api', why, '要说清这是上游的脚本')

    def test_refuses_to_start_when_unavailable(self) -> None:
        with mock.patch.object(taskrun, 'available', lambda: (False, '脚本不在')):
            ok, msg = taskrun.start('claim', 'ALL')
        self.assertFalse(ok)
        self.assertIn('脚本不在', msg)


class ScheduleTest(unittest.TestCase):
    """定时领奖配置：输入校验 + 只存 claim 所需的东西。"""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self._orig = config.DB_PATH
        config.DB_PATH = Path(self._tmp.name) / 's.db'
        db._conn = None
        db.connect()

    def tearDown(self) -> None:
        if db._conn is not None:
            db._conn.close()
        db._conn = None
        config.DB_PATH = self._orig
        try:
            self._tmp.cleanup()
        except PermissionError:
            pass

    def test_default_is_disabled(self) -> None:
        cfg = taskrun.get_schedule()
        self.assertFalse(cfg['enabled'])
        self.assertTrue(cfg['hours'])

    def test_roundtrip(self) -> None:
        taskrun.set_schedule(True, [9, 21])
        self.assertEqual(taskrun.get_schedule(), {'enabled': True, 'hours': [9, 21]})

    def test_hours_validated_and_deduped(self) -> None:
        taskrun.set_schedule(True, [21, 9, 21])
        self.assertEqual(taskrun.get_schedule()['hours'], [9, 21])
        for bad in ([24], [-1], ['9'], [True], [], 'x', None):
            with self.assertRaises(ValueError, msg=repr(bad)):
                taskrun.set_schedule(True, bad)  # type: ignore[arg-type]
        with self.assertRaises(ValueError):
            taskrun.set_schedule('yes', [10])  # type: ignore[arg-type]


class EndpointTest(unittest.TestCase):
    """接口层：权限、模式校验、full 需确认。"""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self._orig_db = config.DB_PATH
        self._orig_users = config.USERS_FILE
        config.DB_PATH = Path(self._tmp.name) / 'e.db'
        config.USERS_FILE = Path(self._tmp.name) / 'users.json'
        db._conn = None
        db.connect()
        security.save_users({
            'secret': 'S',
            'users': [
                {'username': 'admin', 'role': 'admin', 'pwd_hash': security.make_hash('p')},
                {'username': 'viewer', 'role': 'viewer', 'pwd_hash': security.make_hash('p')},
            ],
            'api_keys': [],
        })
        from server.main import app
        self.client = TestClient(app)

    def tearDown(self) -> None:
        if db._conn is not None:
            db._conn.close()
        db._conn = None
        config.DB_PATH = self._orig_db
        config.USERS_FILE = self._orig_users
        try:
            self._tmp.cleanup()
        except PermissionError:
            pass

    def _login(self, username: str) -> None:
        r = self.client.post('/api/login', json={'username': username, 'password': 'p'})
        self.assertEqual(r.status_code, 200, r.text)

    def test_requires_admin(self) -> None:
        """只读用户不得触发（这些操作会对账号发起真实写请求）。"""
        self._login('viewer')
        self.assertEqual(self.client.get('/api/task-run').status_code, 403)
        self.assertEqual(self.client.post(
            '/api/task-run', json={'mode': 'preview', 'target': 'ALL'}).status_code, 403)
        self.assertEqual(self.client.post('/api/task-run/stop').status_code, 403)
        self.assertEqual(self.client.put(
            '/api/task-claim-schedule', json={'enabled': True, 'hours': [10]}).status_code, 403)

    def test_anonymous_requires_auth(self) -> None:
        self.assertEqual(self.client.get('/api/task-run').status_code, 401)

    def test_invalid_mode_rejected(self) -> None:
        self._login('admin')
        r = self.client.post('/api/task-run', json={'mode': 'nuke', 'target': 'ALL'})
        self.assertEqual(r.status_code, 400, r.text)

    def test_full_requires_explicit_confirm(self) -> None:
        """点亮模式必须显式确认 —— 它是唯一会伪造活跃上报的模式。"""
        self._login('admin')
        r = self.client.post('/api/task-run', json={'mode': 'full', 'target': 'ALL'})
        self.assertEqual(r.status_code, 400, r.text)
        self.assertIn('风控', r.json()['detail'])
        # 传了 confirm 才进入执行流程（脚本不存在时是 409，而非 400）
        with mock.patch.object(taskrun, 'start', lambda m, t: (True, '已开始')):
            r2 = self.client.post('/api/task-run',
                                  json={'mode': 'full', 'target': 'ALL', 'confirm': True})
        self.assertEqual(r2.status_code, 200, r2.text)

    def test_claim_and_preview_need_no_confirm(self) -> None:
        self._login('admin')
        with mock.patch.object(taskrun, 'start', lambda m, t: (True, '已开始')):
            for mode in ('preview', 'claim'):
                r = self.client.post('/api/task-run', json={'mode': mode, 'target': 'ALL'})
                self.assertEqual(r.status_code, 200, f'{mode}: {r.text}')

    def test_conflict_returns_409(self) -> None:
        """已在跑 → 409（状态冲突），与「请求有错」400 区分开。"""
        self._login('admin')
        with mock.patch.object(taskrun, 'start', lambda m, t: (False, '已有任务正在执行')):
            r = self.client.post('/api/task-run', json={'mode': 'claim', 'target': 'ALL'})
        self.assertEqual(r.status_code, 409, r.text)

    def test_schedule_validation(self) -> None:
        self._login('admin')
        r = self.client.put('/api/task-claim-schedule',
                            json={'enabled': True, 'hours': [9, 21]})
        self.assertEqual(r.status_code, 200, r.text)
        self.assertEqual(r.json(), {'enabled': True, 'hours': [9, 21]})
        bad = self.client.put('/api/task-claim-schedule',
                              json={'enabled': True, 'hours': [25]})
        self.assertEqual(bad.status_code, 400, bad.text)


class SchedulerOnlyClaimsTest(unittest.TestCase):
    """定时调度**只能**跑 claim —— 点亮绝不进定时。"""

    def test_scheduler_invokes_claim_only(self) -> None:
        calls: list[tuple[str, str]] = []

        def fake_start(mode: str, target: str):
            calls.append((mode, target))
            return True, 'ok'

        # 固定"当前时间"落在配置的整点档内
        with mock.patch.object(taskrun, 'get_schedule', lambda: {'enabled': True, 'hours': [3]}), \
             mock.patch.object(taskrun, 'start', fake_start), \
             mock.patch.object(taskrun, '_last_claim_day', ''), \
             mock.patch('time.localtime',
                        lambda *a: __import__('time').struct_time(
                            (2026, 9, 16, 3, 1, 0, 2, 259, 0))):
            taskrun._last_claim_day = ''
            asyncio.run(self._one_tick())

        modes = {m for m, _ in calls}
        self.assertTrue(modes, '调度器应触发一次执行')
        self.assertEqual(modes, {'claim'},
                         f'定时只允许跑 claim，实际触发了 {modes}')

    async def _one_tick(self) -> None:
        """跑 _claim_loop 的一轮（让它 sleep 时抛错退出，避免挂住）。"""
        class _Boom(Exception):
            pass

        orig_sleep = asyncio.sleep

        async def fake_sleep(_s):  # type: ignore[no-untyped-def]
            raise _Boom

        with mock.patch.object(asyncio, 'sleep', fake_sleep):
            try:
                await taskrun._claim_loop()
            except _Boom:
                pass
        _ = orig_sleep

    def test_disabled_schedule_does_not_run(self) -> None:
        calls: list[tuple[str, str]] = []
        with mock.patch.object(taskrun, 'get_schedule', lambda: {'enabled': False, 'hours': [3]}), \
             mock.patch.object(taskrun, 'start',
                               lambda m, t: (calls.append((m, t)), (True, 'ok'))[1]):
            asyncio.run(self._one_tick())
        self.assertEqual(calls, [], '未启用时不该触发')


if __name__ == '__main__':
    unittest.main()
