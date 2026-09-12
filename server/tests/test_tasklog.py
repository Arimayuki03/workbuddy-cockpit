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

    # ── 上游 2026-09-12 起的日志格式变化 ────────────────
    def test_uid_truncated_to_8_chars(self) -> None:
        """上游把日志里的 uid 截成前 8 位。"""
        ev = tasklog.parse_line(
            f'{DOCKER}travel 9b212d8c: claim ok record=12 reward=100'
        )
        assert ev is not None
        self.assertEqual(ev['uid'], '9b212d8c')
        self.assertEqual(ev['credits'], 100)

    def test_warn_prefix_captured(self) -> None:
        ev = tasklog.parse_line(
            f'{DOCKER}WARN: activity 9b212d8c: report OK but streak.days=0 (silent drop?)'
        )
        assert ev is not None
        self.assertEqual(ev['kind'], 'activity')
        self.assertEqual(ev['uid'], '9b212d8c')
        self.assertEqual(ev['level'], 'warn')

    def test_err_prefix_captured(self) -> None:
        ev = tasklog.parse_line(
            f'{DOCKER}ERR: checkin 9b212d8c: context deadline exceeded'
        )
        assert ev is not None
        self.assertEqual(ev['level'], 'error')

    def test_activity_burst_lines(self) -> None:
        """活跃上报改 5 连发：report N/M ok / report N/M: <err>。"""
        ok = tasklog.parse_line(f'{DOCKER}activity 9b212d8c: report 1/5 ok')
        assert ok is not None
        self.assertEqual(ok['level'], 'ok')
        self.assertEqual(ok['kind'], 'activity')

        bad = tasklog.parse_line(f'{DOCKER}activity 9b212d8c: report 3/5: timeout')
        assert bad is not None
        self.assertEqual(bad['level'], 'error')

    def test_new_failure_wording_is_error(self) -> None:
        """WARN:/ERR: 之外，未加前缀的失败行仍应判为 error。"""
        for line in (
            'activity 9b212d8c: report 2/5: connection reset',
            'activity 9b212d8c: buddy-info: timeout',
        ):
            ev = tasklog.parse_line(f'{DOCKER}{line}')
            assert ev is not None, line
            self.assertEqual(ev['level'], 'error', line)

    def test_report_burst_translated(self) -> None:
        self.assertEqual(
            tasklog.translate_message('report 1/5 ok'), '活跃上报成功（第 1/5 条）'
        )
        self.assertEqual(
            tasklog.translate_message('report 3/5: timeout'), '活跃上报失败（第 3/5 条）'
        )

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

    # ── 结果文案中文化 ──────────────────────────────────
    def test_credit_messages_translated(self) -> None:
        self.assertEqual(
            tasklog.translate_message('claim ok record=12 reward=100'),
            '领奖成功：第 12 次行程，获得 100 积分',
        )
        self.assertEqual(
            tasklog.translate_message('adopt ok (+300 credits)'),
            '领养成功：获得 300 积分',
        )

    def test_skip_and_streak_translated(self) -> None:
        self.assertEqual(
            tasklog.translate_message('skip (daily limit reached)'),
            '跳过：今日次数已达上限',
        )
        self.assertEqual(tasklog.translate_message('streak days=3'), '连续登录 3 天')
        self.assertIn('静默丢弃', tasklog.translate_message(
            'report OK but streak.days=0 (silent drop?)'))

    def test_technical_error_phrases_translated(self) -> None:
        self.assertEqual(tasklog.translate_message('checkin ok code=0'), '签到成功')
        self.assertIn('请求超时', tasklog.translate_message('status: context deadline exceeded'))
        self.assertIn('JSON 解析失败', tasklog.translate_message('unexpected end of JSON input'))

    def test_own_chinese_ledger_message_untouched(self) -> None:
        msg = '余额 +100（1300 → 1400） · 黑天鹅'
        self.assertEqual(tasklog.translate_message(msg), msg)

    def test_unknown_message_falls_back_to_original(self) -> None:
        msg = 'some brand new upstream wording'
        self.assertEqual(tasklog.translate_message(msg), msg)

    def test_truncated_marker_preserved(self) -> None:
        out = tasklog.translate_message('adopt ok (+300 credits) ... （已截断）')
        self.assertEqual(out, '领养成功：获得 300 积分 …（已截断）')

    def test_strip_docker_ts(self) -> None:
        self.assertEqual(
            tasklog.strip_docker_ts(f'{DOCKER}travel 1: claim ok record=1 reward=100'),
            'travel 1: claim ok record=1 reward=100',
        )
        # 没有前缀时原样返回
        self.assertEqual(tasklog.strip_docker_ts('travel 1: hello'), 'travel 1: hello')


if __name__ == '__main__':
    unittest.main()
