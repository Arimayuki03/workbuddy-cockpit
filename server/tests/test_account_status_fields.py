"""账号运行状态字段的透传（冷却剩余时间 / 被限流模型清单）。

背景：上游 `/status` 一直提供 `cool_remaining_sec`（冷却剩余秒数）与
`rate_limited_models`（被限流的模型清单），但管理端只取了 `cooling` 布尔值，
界面上只显示「冷却中」——用户既不知道要等多久，也不知道是哪个模型被限。

上游 2026-09-15 的改动让冷却时长**对齐上游明说的重置时刻**（不再靠固定基数
+ 指数退避猜），这个值的可信度进一步提高，值得展示出来。

本文件锁住：这两个字段要如实透传，且**缺省/异常输入不崩**（上游旧版本或
字段缺失时界面仍要正常）。
"""
from __future__ import annotations

import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from server.services import wb2api  # noqa: E402


class CoolRemainingPassthroughTest(unittest.TestCase):
    """cool_remaining_sec 应透传为正数秒；缺失/0/负数 → None（不显示倒计时）。"""

    def _merge(self, pool_item: dict) -> dict:
        accounts = [{'uid': 'u1'}]
        wb2api.merge_pool_status(accounts, {'accounts': [dict(pool_item, uid='u1')]})
        return accounts[0]

    def test_positive_seconds_passthrough(self) -> None:
        a = self._merge({'cooling': True, 'cool_remaining_sec': 1800})
        self.assertTrue(a['cooling'])
        self.assertEqual(a['cool_remaining_sec'], 1800)

    def test_missing_field_is_none(self) -> None:
        """上游旧版本没有这个字段 → None，界面不显示倒计时（而不是显示 0 分钟）。"""
        a = self._merge({'cooling': True})
        self.assertIsNone(a['cool_remaining_sec'])

    def test_zero_and_negative_are_none(self) -> None:
        """0/负数没有展示意义（已到期），统一成 None 交给界面判断。"""
        for v in (0, -5):
            a = self._merge({'cooling': True, 'cool_remaining_sec': v})
            self.assertIsNone(a['cool_remaining_sec'], f'{v} 应归一为 None')

    def test_garbage_is_none_not_crash(self) -> None:
        """上游字段类型异常时不能让整页崩掉。"""
        for v in ('abc', None, [], {}):
            a = self._merge({'cooling': True, 'cool_remaining_sec': v})
            self.assertIsNone(a['cool_remaining_sec'], f'{v!r} 应归一为 None')

    def test_float_seconds_truncated_to_int(self) -> None:
        a = self._merge({'cooling': True, 'cool_remaining_sec': 90.7})
        self.assertEqual(a['cool_remaining_sec'], 90)


class RateLimitedModelsPassthroughTest(unittest.TestCase):
    """被限流的模型清单要原样透传（多模型时各自恢复时刻不同）。"""

    def _merge(self, pool_item: dict) -> dict:
        accounts = [{'uid': 'u1'}]
        wb2api.merge_pool_status(accounts, {'accounts': [dict(pool_item, uid='u1')]})
        return accounts[0]

    def test_list_passthrough(self) -> None:
        models = [
            {'model': 'glm-5.2', 'until': '2026-09-15T10:00:00+08:00'},
            {'model': 'deepseek-v4', 'reset_at': '2026-09-15T11:00:00+08:00'},
        ]
        a = self._merge({'cooling': True, 'rate_limited_models': models})
        self.assertEqual(len(a['rate_limited_models']), 2)
        self.assertEqual(a['rate_limited_models'][0]['model'], 'glm-5.2')

    def test_missing_is_empty_list(self) -> None:
        """缺失时给空列表而非 None —— 前端直接 map 不会炸。"""
        a = self._merge({'cooling': True})
        self.assertEqual(a['rate_limited_models'], [])

    def test_garbage_is_empty_list(self) -> None:
        for v in ('x', None, 5, {}):
            a = self._merge({'cooling': True, 'rate_limited_models': v})
            self.assertEqual(a['rate_limited_models'], [], f'{v!r} 应归一为 []')


class NotInPoolTest(unittest.TestCase):
    """账号没进上游池时必须能识别出来（用户报的「面板全绿却报没有健康账号」）。

    我们读的是 auths/ 目录下的**文件**，上游读的才是**池**。两者不总一致：
    上游 `LoadDir` 对解析失败的 auth 文件静默跳过（`Parse` 在 accessToken 为
    空时报错），那个文件永远进不了池、永远选不中。

    此前这种账号在面板上走兜底分支显示「● 在线」——于是出现「面板全绿、调用
    却报没有健康账号」的矛盾（用户实测反馈）。
    """

    def _merge_pool(self, pool_items: list[dict]) -> dict:
        accounts = [{'uid': 'u1'}]
        wb2api.merge_pool_status(accounts, {'accounts': pool_items})
        return accounts[0]

    def test_account_absent_from_pool_is_marked(self) -> None:
        """上游没返回它 → in_pool 为 False，供界面标出「未加载」。"""
        self.assertIs(self._merge_pool([])['in_pool'], False)

    def test_account_present_in_pool_is_marked(self) -> None:
        self.assertIs(self._merge_pool([{'uid': 'u1', 'credits': 5}])['in_pool'], True)

    def _read_auths(self, payload: dict) -> list[dict]:
        import json
        import tempfile
        from pathlib import Path

        from server import config

        tmp = Path(tempfile.mkdtemp())
        (tmp / 'auths').mkdir()
        orig = config.AUTH_DIR
        config.AUTH_DIR = tmp / 'auths'
        try:
            (config.AUTH_DIR / 'workbuddy-t.json').write_text(
                json.dumps(payload), encoding='utf-8')
            return wb2api.list_auth_accounts()
        finally:
            config.AUTH_DIR = orig

    def test_missing_token_flagged_as_invalid(self) -> None:
        """accessToken 为空 → 上游 Parse 必拒；本地如实给出原因。"""
        got = self._read_auths({
            'auth': {'accessToken': '', 'expiresAt': 4102444800,
                     'domain': 'www.codebuddy.cn'},
            'account': {'uid': 'uid-x', 'nickname': 'x'},
        })
        self.assertEqual(len(got), 1)
        self.assertTrue(got[0]['invalid_reason'], '空 accessToken 应给出原因')
        self.assertIn('accessToken', got[0]['invalid_reason'])

    def test_valid_token_has_no_invalid_reason(self) -> None:
        """正常账号不该被误标 —— 否则所有账号都会显示成「未加载」。"""
        got = self._read_auths({
            'auth': {'accessToken': 'at-ok', 'expiresAt': 4102444800,
                     'domain': 'www.codebuddy.cn'},
            'account': {'uid': 'uid-y', 'nickname': 'y'},
        })
        self.assertEqual(got[0]['invalid_reason'], '')


if __name__ == '__main__':
    unittest.main()
