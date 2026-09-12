"""上游任务日志解析的回归测试。

这些日志行来自真实上游源码（travel.go / scheduler.go）的 log.Printf，
容器里带 docker --timestamps 前缀。解析必须准确提取类型、账号与积分，
否则界面上要么看不到收益，要么把跳过当成功。
"""
from __future__ import annotations

import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from server.services import tasklog  # noqa: E402

DOCKER = '2026-09-11T17:43:44.123456789Z '


class ParseTaskLines(unittest.TestCase):
    def test_travel_claim_reward(self) -> None:
        ev = tasklog.parse_line(
            f'{DOCKER}2026/09/11 17:43:44 travel 89374120: claim ok record=12 reward=100'
        )
        assert ev is not None
        self.assertEqual(ev['kind'], 'travel')
        self.assertEqual(ev['uid'], '89374120')
        self.assertEqual(ev['credits'], 100)
        self.assertEqual(ev['level'], 'credit')

    def test_travel_adopt_reward(self) -> None:
        ev = tasklog.parse_line(f'{DOCKER}travel 89374120: adopt ok (+300 credits)')
        assert ev is not None
        self.assertEqual(ev['kind'], 'travel')
        self.assertEqual(ev['credits'], 300)
        self.assertEqual(ev['level'], 'credit')

    def test_travel_depart_is_not_a_credit(self) -> None:
        ev = tasklog.parse_line(
            f'{DOCKER}2026/09/11 09:00:00 travel 89374120: depart ok location=7'
        )
        assert ev is not None
        self.assertEqual(ev['credits'], 0)
        self.assertEqual(ev['level'], 'ok')

    def test_travel_skip_is_info_not_error(self) -> None:
        for line in (
            'travel 89374120: skip (daily limit reached)',
            'travel 89374120: skip (traveling record=3)',
        ):
            ev = tasklog.parse_line(f'{DOCKER}{line}')
            assert ev is not None
            self.assertEqual(ev['level'], 'info', line)
            self.assertEqual(ev['credits'], 0)

    def test_activity_streak_ok(self) -> None:
        ev = tasklog.parse_line(f'{DOCKER}2026/09/11 10:00:00 activity 89374120: streak days=3')
        assert ev is not None
        self.assertEqual(ev['kind'], 'activity')
        self.assertEqual(ev['level'], 'ok')
        self.assertIn('streak', ev['message'])

    def test_activity_silent_drop_is_warn(self) -> None:
        ev = tasklog.parse_line(
            f'{DOCKER}activity 89374120: report OK but streak.days=0 (silent drop?)'
        )
        assert ev is not None
        self.assertEqual(ev['level'], 'warn')

    def test_failure_lines_are_errors(self) -> None:
        for line in (
            '2026/09/11 22:00:00 travel 89374120: status: context deadline exceeded',
            'checkin 89374120: rpc error: code = DeadlineExceeded',
            'keepalive 89374120: 连续 3 次 12153 session dead — 禁用',
            'user-resource 89374120: unexpected end of JSON input',
        ):
            ev = tasklog.parse_line(f'{DOCKER}{line}')
            assert ev is not None, line
            self.assertEqual(ev['level'], 'error', line)

    def test_unrelated_lines_ignored(self) -> None:
        for line in (
            '',
            '   ',
            'listening on :7863',
            'INFO server started',
            '2026/09/11 17:00:00 some other module: hello',
        ):
            self.assertIsNone(tasklog.parse_line(line), repr(line))

    def test_timestamp_parsed_from_docker_prefix(self) -> None:
        ev = tasklog.parse_line(
            f'{DOCKER}travel 89374120: claim ok record=1 reward=50'
        )
        assert ev is not None
        # 2026-09-11T17:43:44Z
        self.assertEqual(ev['ts'], 1789148624)

    def test_timestamp_missing_still_parses(self) -> None:
        """没有 docker 前缀（直接跑脚本）时也应能解析，时间留 0 由上层兜底。"""
        ev = tasklog.parse_line('travel 89374120: claim ok record=1 reward=50')
        assert ev is not None
        self.assertEqual(ev['ts'], 0)
        self.assertEqual(ev['credits'], 50)

    def test_dedup_key_is_stable_and_unique(self) -> None:
        line = f'{DOCKER}travel 89374120: claim ok record=1 reward=50'
        a = tasklog.parse_line(line)
        b = tasklog.parse_line(line)
        assert a and b
        self.assertEqual(a['dedup_key'], b['dedup_key'])

        other = tasklog.parse_line(
            '2026-09-11T17:43:45.1Z travel 89374120: claim ok record=2 reward=50'
        )
        assert other is not None
        self.assertNotEqual(a['dedup_key'], other['dedup_key'])

    def test_parse_lines_mixed(self) -> None:
        lines = [
            'listening on :7863',
            f'{DOCKER}travel 1: claim ok record=1 reward=100',
            f'{DOCKER}activity 2: streak days=1',
            f'{DOCKER}travel 3: skip (daily limit reached)',
        ]
        events = tasklog.parse_lines(lines)
        self.assertEqual(len(events), 3)
        self.assertEqual([e['kind'] for e in events], ['travel', 'activity', 'travel'])

    def test_strip_docker_ts(self) -> None:
        self.assertEqual(
            tasklog.strip_docker_ts(f'{DOCKER}travel 1: claim ok record=1 reward=100'),
            'travel 1: claim ok record=1 reward=100',
        )
        # 没有前缀时原样返回
        self.assertEqual(tasklog.strip_docker_ts('travel 1: hello'), 'travel 1: hello')


if __name__ == '__main__':
    unittest.main()
