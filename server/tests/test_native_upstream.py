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
