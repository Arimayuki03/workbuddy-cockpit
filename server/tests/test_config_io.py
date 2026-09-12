"""上游配置读写的回归测试：确保可视化字段不会把 config.json 写坏。

重点覆盖三个曾经的坑：
  1. *_hours 必须写成 []int（曾按数字间隔处理，会把 [9,21] 写成 6）
  2. 时长字段必须写成字符串（曾把 breaker_cooldown 当成秒数写整数）
  3. 只提交改动过的字段，不能覆盖同一段里其他未展示的键

运行：python -m unittest discover -s server -t . -p 'test_*.py'
"""
from __future__ import annotations

import json
import os
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from server import config  # noqa: E402
from server.services import wb2api  # noqa: E402


def write_cfg(path: Path, obj: dict) -> None:
    path.write_text(json.dumps(obj, ensure_ascii=False, indent=2), encoding='utf-8')


def read_cfg(path: Path) -> dict:
    return json.loads(path.read_text(encoding='utf-8'))


class UpstreamConfigRoundTrip(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.cfg_path = Path(self._tmp.name) / 'config.json'
        self._orig = config.UPSTREAM_CONFIG
        config.UPSTREAM_CONFIG = self.cfg_path

    def tearDown(self) -> None:
        config.UPSTREAM_CONFIG = self._orig
        self._tmp.cleanup()

    # ── 时刻数组 ─────────────────────────────────────────
    def test_hours_written_as_int_array(self) -> None:
        write_cfg(self.cfg_path, {'schedule': {'checkin_hours': [9, 21]}})
        wb2api.save_upstream_config({'schedule': {'travel_hours': [9, 21]}})
        got = read_cfg(self.cfg_path)['schedule']['travel_hours']
        self.assertEqual(got, [9, 21])
        self.assertIsInstance(got, list)

    def test_hours_accepts_string_of_numbers(self) -> None:
        """前端按字符串提交时也能落到正确类型（兜底解析）。"""
        write_cfg(self.cfg_path, {})
        wb2api.save_upstream_config({'schedule': {'activity_hours': [10]}})
        self.assertEqual(read_cfg(self.cfg_path)['schedule']['activity_hours'], [10])

    def test_hours_dedup_and_sort(self) -> None:
        write_cfg(self.cfg_path, {})
        wb2api.save_upstream_config({'schedule': {'keepalive_hours': [22, 9, 22]}})
        self.assertEqual(read_cfg(self.cfg_path)['schedule']['keepalive_hours'], [9, 22])

    def test_hours_reject_out_of_range(self) -> None:
        write_cfg(self.cfg_path, {'schedule': {'checkin_hours': [9]}})
        with self.assertRaises(ValueError):
            wb2api.save_upstream_config({'schedule': {'checkin_hours': [24]}})
        with self.assertRaises(ValueError):
            wb2api.save_upstream_config({'schedule': {'checkin_hours': [-1]}})
        with self.assertRaises(ValueError):
            wb2api.save_upstream_config({'schedule': {'checkin_hours': []}})
        with self.assertRaises(ValueError):
            wb2api.save_upstream_config({'schedule': {'checkin_hours': 6}})
        # 拒绝后原配置保持不变
        self.assertEqual(read_cfg(self.cfg_path)['schedule']['checkin_hours'], [9])

    # ── 时长字符串 ───────────────────────────────────────
    def test_duration_fields_stay_strings(self) -> None:
        write_cfg(self.cfg_path, {})
        wb2api.save_upstream_config(
            {
                'cooldown': {'soft_rate': '600s', 'soft_rate_max': '2h'},
                'pool': {'breaker_cooldown': '30m', 'breaker_cooldown_max': '6h'},
                'session_sticky': {'ttl': '30m', 'gc_interval': '5m'},
            }
        )
        cfg = read_cfg(self.cfg_path)
        self.assertEqual(cfg['cooldown']['soft_rate'], '600s')
        self.assertEqual(cfg['pool']['breaker_cooldown'], '30m')
        self.assertEqual(cfg['session_sticky']['ttl'], '30m')

    def test_duration_reject_bad_format(self) -> None:
        write_cfg(self.cfg_path, {'pool': {'breaker_cooldown': '30m'}})
        with self.assertRaises(ValueError):
            wb2api.save_upstream_config({'pool': {'breaker_cooldown': '600'}})
        with self.assertRaises(ValueError):
            wb2api.save_upstream_config({'cooldown': {'soft_rate': 'soon'}})
        self.assertEqual(read_cfg(self.cfg_path)['pool']['breaker_cooldown'], '30m')

    # ── 不覆盖同段其他键 ─────────────────────────────────
    def test_partial_patch_preserves_siblings(self) -> None:
        write_cfg(
            self.cfg_path,
            {
                'schedule': {'checkin_hours': [9, 21], 'keepalive_hours': [22]},
                'pool': {'max_in_flight': 3},
            },
        )
        wb2api.save_upstream_config({'schedule': {'checkin_hours': [8]}})
        cfg = read_cfg(self.cfg_path)
        self.assertEqual(cfg['schedule']['checkin_hours'], [8])
        self.assertEqual(cfg['schedule']['keepalive_hours'], [22])
        self.assertEqual(cfg['pool']['max_in_flight'], 3)

    def test_bool_switch_round_trip(self) -> None:
        write_cfg(self.cfg_path, {'schedule': {'checkin_enabled': True}})
        wb2api.save_upstream_config({'schedule': {'checkin_enabled': False}})
        self.assertIs(read_cfg(self.cfg_path)['schedule']['checkin_enabled'], False)

    def test_unknown_section_ignored(self) -> None:
        write_cfg(self.cfg_path, {'api_key': 'secret'})
        wb2api.save_upstream_config({'api_key': 'hacked'})
        self.assertEqual(read_cfg(self.cfg_path)['api_key'], 'secret')

    # ── prompt / server / upstream（新增段）─────────────────
    def test_prompt_mode_round_trip(self) -> None:
        write_cfg(self.cfg_path, {'prompt': {'mode': 'custom'}})
        wb2api.save_upstream_config({'prompt': {'mode': 'passthrough'}})
        self.assertEqual(read_cfg(self.cfg_path)['prompt']['mode'], 'passthrough')

    def test_prompt_mode_normalised_and_validated(self) -> None:
        write_cfg(self.cfg_path, {'prompt': {'mode': 'custom'}})
        wb2api.save_upstream_config({'prompt': {'mode': 'Passthrough'}})
        self.assertEqual(read_cfg(self.cfg_path)['prompt']['mode'], 'passthrough')
        with self.assertRaises(ValueError):
            wb2api.save_upstream_config({'prompt': {'mode': 'replace'}})
        # 拒绝后保持原值
        self.assertEqual(read_cfg(self.cfg_path)['prompt']['mode'], 'passthrough')

    def test_prompt_file_rejects_control_chars(self) -> None:
        write_cfg(self.cfg_path, {'prompt': {'file': ''}})
        wb2api.save_upstream_config({'prompt': {'file': '/etc/prompt.md'}})
        self.assertEqual(read_cfg(self.cfg_path)['prompt']['file'], '/etc/prompt.md')
        with self.assertRaises(ValueError):
            wb2api.save_upstream_config({'prompt': {'file': '/a\nb'}})

    def test_server_max_body_mb_validation(self) -> None:
        write_cfg(self.cfg_path, {'server': {'max_body_mb': 8}})
        wb2api.save_upstream_config({'server': {'max_body_mb': 32}})
        self.assertEqual(read_cfg(self.cfg_path)['server']['max_body_mb'], 32)
        for bad in (0, -1, 257, 'many'):
            with self.assertRaises(ValueError):
                wb2api.save_upstream_config({'server': {'max_body_mb': bad}})
        self.assertEqual(read_cfg(self.cfg_path)['server']['max_body_mb'], 32)

    def test_upstream_user_agent_round_trip(self) -> None:
        write_cfg(self.cfg_path, {})
        wb2api.save_upstream_config({'upstream': {'user_agent': 'MyClient/1.0'}})
        self.assertEqual(read_cfg(self.cfg_path)['upstream']['user_agent'], 'MyClient/1.0')
        with self.assertRaises(ValueError):
            wb2api.save_upstream_config({'upstream': {'user_agent': 'bad\nua'}})

    def test_new_sections_do_not_touch_others(self) -> None:
        """保存 prompt 不应影响 timeout 等同段的其他键。"""
        write_cfg(
            self.cfg_path,
            {'upstream': {'timeout_seconds': 120, 'idle_timeout_seconds': 300}},
        )
        wb2api.save_upstream_config({'upstream': {'user_agent': 'X/1'}})
        cfg = read_cfg(self.cfg_path)['upstream']
        self.assertEqual(cfg['timeout_seconds'], 120)
        self.assertEqual(cfg['idle_timeout_seconds'], 300)
        self.assertEqual(cfg['user_agent'], 'X/1')

    def test_missing_config_refuses_to_write(self) -> None:
        with self.assertRaises(FileNotFoundError):
            wb2api.save_upstream_config({'schedule': {'checkin_hours': [9]}})
        self.assertFalse(self.cfg_path.exists())


if __name__ == '__main__':
    unittest.main()
