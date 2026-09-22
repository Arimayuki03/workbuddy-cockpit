"""账号页「强制退出冷却与模型限流」的回归测试。"""
from __future__ import annotations

import asyncio
import json
import pathlib
import tempfile
import unittest
from unittest import mock

from server import config
from server.services import wb2api


class ForceClearCooling(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        root = pathlib.Path(self._tmp.name)
        self._old_config = config.UPSTREAM_CONFIG
        self._old_dir = config.UPSTREAM_DIR
        config.UPSTREAM_DIR = root
        config.UPSTREAM_CONFIG = root / 'config.json'
        config.UPSTREAM_CONFIG.write_text(
            json.dumps({'state_file': './data/state.json'}), encoding='utf-8',
        )
        self.state_path = root / 'data' / 'state.json'
        self.state_path.parent.mkdir(parents=True)
        self.state_path.write_text(json.dumps({
            'accounts': {
                'target': {
                    'credits': 123,
                    'disabled': False,
                    'reason': '429 rate limit',
                    'until': '2099-01-01T04:00:00+08:00',
                    'cool_kind': 1,
                    'soft_streak': 7,
                    'model_cooldowns': {
                        'glm-5.3': {
                            'until': '2099-01-01T05:00:00+08:00',
                            'reason': '6004 model rate limit',
                        },
                    },
                    'breaker_until': '2099-01-01T06:00:00+08:00',
                    'retry_count': 3,
                    'degrade_until': '2099-01-01T07:00:00+08:00',
                    'consecutive_fails': 5,
                },
                'other': {'credits': 9, 'until': '2099-02-01T04:00:00+08:00'},
            },
        }, ensure_ascii=False), encoding='utf-8')

    def tearDown(self) -> None:
        config.UPSTREAM_CONFIG = self._old_config
        config.UPSTREAM_DIR = self._old_dir
        self._tmp.cleanup()

    def test_clears_only_target_cooling_domains(self) -> None:
        result = wb2api.clear_account_cooling_state('target')
        data = json.loads(self.state_path.read_text(encoding='utf-8'))
        target = data['accounts']['target']

        self.assertEqual(result['uid'], 'target')
        self.assertEqual(target['until'], '0001-01-01T00:00:00Z')
        self.assertEqual(target['cool_kind'], 0)
        self.assertEqual(target['soft_streak'], 0)
        self.assertNotIn('reason', target)
        self.assertNotIn('model_cooldowns', target)
        self.assertNotIn('breaker_until', target)
        self.assertEqual(target['retry_count'], 0)
        self.assertNotIn('degrade_until', target)
        self.assertEqual(target['consecutive_fails'], 0)
        self.assertEqual(target['credits'], 123)
        self.assertEqual(data['accounts']['other']['credits'], 9)
        self.assertTrue(pathlib.Path(result['backup']).is_file())

    def test_preserves_disabled_reason(self) -> None:
        data = json.loads(self.state_path.read_text(encoding='utf-8'))
        data['accounts']['target']['disabled'] = True
        data['accounts']['target']['reason'] = 'request illegal'
        self.state_path.write_text(json.dumps(data), encoding='utf-8')

        wb2api.clear_account_cooling_state('target')
        target = json.loads(self.state_path.read_text(encoding='utf-8'))['accounts']['target']
        self.assertEqual(target['reason'], 'request illegal')

    def test_force_clear_stops_edits_and_starts(self) -> None:
        initial = {'connected': True, 'accounts': [{'uid': 'target', 'cooling': True}]}
        after = {'connected': True, 'accounts': [{
            'uid': 'target', 'cooling': False, 'rate_limited_models': [],
        }]}
        with (
            mock.patch.object(wb2api, 'get_status', new=mock.AsyncMock(side_effect=[initial, after])),
            mock.patch.object(
                wb2api,
                '_set_upstream_running',
                new=mock.AsyncMock(side_effect=[(True, 'stopped'), (True, 'started')]),
            ) as control,
        ):
            ok, message, detail = asyncio.run(wb2api.force_clear_account_cooling('target'))

        self.assertTrue(ok, message)
        self.assertIn('已强制清除', message)
        self.assertEqual(detail['uid'], 'target')
        self.assertEqual(control.await_args_list[0].args, (False,))
        self.assertEqual(control.await_args_list[1].args, (True,))
        target = json.loads(self.state_path.read_text(encoding='utf-8'))['accounts']['target']
        self.assertNotIn('model_cooldowns', target)

    def test_noop_when_account_is_already_clear(self) -> None:
        status = {'connected': True, 'accounts': [{
            'uid': 'target', 'cooling': False, 'rate_limited_models': [],
        }]}
        with (
            mock.patch.object(wb2api, 'get_status', new=mock.AsyncMock(return_value=status)),
            mock.patch.object(wb2api, '_set_upstream_running', new=mock.AsyncMock()) as control,
        ):
            ok, message, _ = asyncio.run(wb2api.force_clear_account_cooling('target'))

        self.assertTrue(ok)
        self.assertIn('无需清除', message)
        control.assert_not_awaited()


if __name__ == '__main__':
    unittest.main()
