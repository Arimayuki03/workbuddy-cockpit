"""原生 workbuddy2api 的运行时兼容。"""
from __future__ import annotations

import asyncio
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from server import config  # noqa: E402
from server.services import updater, wb2api  # noqa: E402


class _Process:
    returncode = 0

    async def communicate(self):
        return b'', b''


class NativeUpstreamRuntimeTest(unittest.IsolatedAsyncioTestCase):
    async def test_restart_uses_native_stop_and_start_scripts(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            start = root / 'start-workbuddy2api.cmd'
            stop = root / 'stop-workbuddy2api.cmd'
            start.touch()
            stop.touch()
            calls: list[tuple] = []

            async def fake_exec(*args, **kwargs):
                calls.append(args)
                return _Process()

            with mock.patch.object(config, 'WB2API_MODE', 'native'), \
                    mock.patch.object(config, 'WB2API_START_SCRIPT', start), \
                    mock.patch.object(config, 'WB2API_STOP_SCRIPT', stop), \
                    mock.patch.object(asyncio, 'create_subprocess_exec', side_effect=fake_exec):
                ok, message = await wb2api.restart_container()

            self.assertTrue(ok, message)
            self.assertEqual(len(calls), 2, calls)
            self.assertEqual(Path(calls[0][-1]), stop)
            self.assertEqual(Path(calls[1][-1]), start)
            self.assertIn('原生', message)

    def test_logs_prefer_native_log_file_and_tail_limit(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            log = Path(td) / 'server.log'
            log.write_text('one\ntwo\nthree\n', encoding='utf-8')
            with mock.patch.object(config, 'WB2API_MODE', 'native'), \
                    mock.patch.object(config, 'WB2API_LOG_FILE', log), \
                    mock.patch('subprocess.run') as docker:
                lines = wb2api.read_container_logs(limit=2)

            self.assertEqual(lines, ['two', 'three'])
            docker.assert_not_called()

    async def test_hanging_script_times_out_and_is_killed(self) -> None:
        """脚本挂住时必须**超时返回失败**，不能永远等下去。

        评审发现：原实现是裸 `await proc.communicate()`，没有任何超时。脚本一旦挂住
        （等交互输入、端口被占用、启动时卡在依赖上），这个协程就永不返回，而 reload
        的状态机会一直停在 `running=True` —— 后续所有「保存配置后自动重载」都
        **静默失效**（不报错、不重试），用户只会觉得「改了配置没生效」。

        所以这里钉两件事：超时后返回失败（而不是挂住），且进程确实被结束掉
        （否则会留下孤儿进程继续占着端口）。
        """
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            start = root / 'start.cmd'
            stop = root / 'stop.cmd'
            start.touch()
            stop.touch()
            killed = {'yes': False}

            class _HangProc:
                returncode = None

                def kill(self):
                    killed['yes'] = True

                async def communicate(self):
                    await asyncio.sleep(30)      # 模拟永不结束

            async def fake_exec(*args, **kwargs):
                return _HangProc()

            with mock.patch.object(config, 'WB2API_MODE', 'native'), \
                    mock.patch.object(config, 'WB2API_START_SCRIPT', start), \
                    mock.patch.object(config, 'WB2API_STOP_SCRIPT', stop), \
                    mock.patch.object(wb2api, '_NATIVE_RESTART_TIMEOUT', 0.2), \
                    mock.patch.object(asyncio, 'create_subprocess_exec', side_effect=fake_exec):
                ok, message = await asyncio.wait_for(wb2api.restart_container(), timeout=10)

            self.assertFalse(ok, '脚本挂住却报成功')
            self.assertIn('未结束', message)
            self.assertTrue(killed['yes'], '超时后没有结束子进程（会留下孤儿进程）')

    def test_upstream_dir_has_a_single_source(self) -> None:
        """上游目录只能有一处推导口径（评审发现两处会分歧）。

        评审发现 `config.UPSTREAM_DIR`（native 模式新增，回退到 config.json 所在目录）
        与 `updater._upstream_dir()`（原有，回退到 auths 的父目录）是**两套独立推导**。
        默认配置下巧合一致，但只要用户单独调整 `WB_AUTH_DIR` 或 `WB_UPSTREAM_CONFIG`
        中的一个，两者就指向不同目录，且没有任何报错：

          · 原生启停脚本按 config 那份找；
          · 任务脚本与「更新上游」按 updater 那份找。

        结果是一半功能落在 A 目录、另一半落在 B 目录。这条测试钉住「只有一份口径」。
        """
        # 让两个来源指向不同目录，看 updater 是否仍然跟随 config
        with mock.patch.object(config, 'UPSTREAM_DIR', Path('/fake/upstream/from-config')):
            self.assertEqual(updater._upstream_dir(), Path('/fake/upstream/from-config'),
                             'updater 没有复用 config.UPSTREAM_DIR —— 又变成两套口径了')

    def test_native_restart_reports_missing_scripts(self) -> None:
        """脚本不存在时要明确报出缺哪个（而不是等到执行才报个含糊错误）。"""
        async def main():
            with mock.patch.object(config, 'WB2API_MODE', 'native'), \
                    mock.patch.object(config, 'WB2API_START_SCRIPT', Path('/nope/start.cmd')), \
                    mock.patch.object(config, 'WB2API_STOP_SCRIPT', Path('/nope/stop.cmd')):
                return await wb2api.restart_container()

        ok, message = asyncio.run(main())
        self.assertFalse(ok)
        self.assertIn('未找到原生启停脚本', message)
        self.assertIn('stop.cmd', message)

    def test_windows_rejects_linux_only_one_click_update(self) -> None:
        with mock.patch.object(updater.os, 'name', 'nt'), \
                mock.patch.object(config, 'WB2API_MODE', 'native'), \
                mock.patch.object(updater, '_lock_active', return_value=True):
            ok, message = updater.start_update('manager')

        self.assertFalse(ok)
        self.assertIn('Windows', message)
        self.assertNotIn('已有更新任务', message)


if __name__ == '__main__':
    unittest.main()
