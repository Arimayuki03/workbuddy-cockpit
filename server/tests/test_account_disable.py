"""临时禁用账号（issue #21）：改文件名实现，可逆、不碰凭证。

上游**没有**禁用/启用的 HTTP 接口——它内部有 `Disable`/`ReviveDisabled`，但只被
自身的错误处理调用，没有对外暴露；`state.json` 又每 5 秒被上位机覆盖，改它没有
意义（改了立刻被内存状态刷回去）。

可行路径是**改文件名**：上游用 glob `workbuddy*.json` 收集账号文件
（`internal/auth/auth.go` 的 `AuthFileGlob`），所以把文件改名成
`workbuddy-xxx.json.disabled` 之后它就不再被加载、从池里消失；启用就是改回原名。

这里钉住的几条，每条都对应一个真实风险：

  1. **可逆**：凭证一个字节都不能动 —— 删除会丢 token（只能重新扫码），改名不会。
  2. **面板仍能看到**：禁用的账号必须还在列表里（否则「禁用」体验上等同于「删除」，
     用户没法再启用它）。
  3. **显示成「已停用」而不是「未加载」**：禁用后账号必然不在池里，若不单独分档，
     界面会按「上游没加载它」报成「账号文件可能有问题」——把用户自己的操作说成故障。
  4. **幂等**：重复禁用/启用不报错（界面可能重试、连点）。
  5. **文件名校验不放水**：新增的 `.disabled` 形态不能成为目录穿越的新入口。
"""
from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from server import config  # noqa: E402
from server.services import wb2api  # noqa: E402

UID = '99a07e71deadbeef'


def _auth_doc(uid: str, nickname: str = '测试号') -> dict:
    return {
        'account': {'uid': uid, 'nickname': nickname, 'enterpriseId': 'e'},
        'auth': {'accessToken': 'tok-' + uid, 'refreshToken': 'ref-' + uid,
                 'expiresAt': 4102444800, 'domain': 'copilot.tencent.com'},
    }


class DisableToggleTest(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.dir = Path(self._tmp.name)
        self._orig = config.AUTH_DIR
        config.AUTH_DIR = self.dir
        self.fname = f'workbuddy-{UID}.json'
        (self.dir / self.fname).write_text(
            json.dumps(_auth_doc(UID), ensure_ascii=False), encoding='utf-8')

    def tearDown(self) -> None:
        config.AUTH_DIR = self._orig
        try:
            self._tmp.cleanup()
        except PermissionError:
            pass

    def _names(self) -> list[str]:
        return sorted(p.name for p in self.dir.iterdir())

    def test_disable_renames_and_keeps_bytes(self) -> None:
        before = (self.dir / self.fname).read_bytes()
        r = wb2api.set_account_disabled(self.fname, True)
        self.assertTrue(r['changed'])
        self.assertTrue(r['disabled'])
        self.assertEqual(r['file'], self.fname + '.disabled')
        # 原文件不在了、改名后的文件在，且**内容逐字节一致**（凭证没动）
        self.assertEqual(self._names(), [self.fname + '.disabled'])
        self.assertEqual((self.dir / (self.fname + '.disabled')).read_bytes(), before)

    def test_enable_restores_original_name(self) -> None:
        wb2api.set_account_disabled(self.fname, True)
        r = wb2api.set_account_disabled(self.fname + '.disabled', False)
        self.assertTrue(r['changed'])
        self.assertFalse(r['disabled'])
        self.assertEqual(r['file'], self.fname)
        self.assertEqual(self._names(), [self.fname])

    def test_idempotent(self) -> None:
        """重复调用不报错、不重复改名（界面可能重试/连点）。"""
        first = wb2api.set_account_disabled(self.fname, True)
        second = wb2api.set_account_disabled(self.fname, True)
        self.assertTrue(first['changed'])
        self.assertFalse(second['changed'], '重复禁用不该再改一次名')
        # 用禁用后的名字再禁用一次也要幂等
        third = wb2api.set_account_disabled(self.fname + '.disabled', True)
        self.assertFalse(third['changed'])
        # 启用同理
        wb2api.set_account_disabled(self.fname, False)
        fourth = wb2api.set_account_disabled(self.fname, False)
        self.assertFalse(fourth['changed'])

    def test_missing_file_raises(self) -> None:
        """文件不存在要报错，不能给出「禁用成功」的假象。"""
        with self.assertRaises(ValueError):
            wb2api.set_account_disabled('workbuddy-nosuchuid.json', True)
        with self.assertRaises(ValueError):
            wb2api.set_account_disabled('workbuddy-nosuchuid.json.disabled', False)

    def test_illegal_filenames_still_rejected(self) -> None:
        """`.disabled` 形态不能成为目录穿越的新入口。"""
        for bad in ('../workbuddy-x.json', 'workbuddy-x.json/../y',
                    '/etc/passwd', 'workbuddy-x.json.disabled/../z',
                    'other.json', 'workbuddy-x.txt', 'workbuddy-x.json.disabledx'):
            with self.assertRaises(ValueError, msg=repr(bad)):
                wb2api.set_account_disabled(bad, True)

    def test_disabled_file_still_listed(self) -> None:
        """禁用后必须仍在面板列表里，否则用户没法再启用它。"""
        wb2api.set_account_disabled(self.fname, True)
        rows = {a['uid']: a for a in wb2api.list_auth_accounts()}
        self.assertIn(UID, rows, '禁用后账号从列表里消失了（等同删除）')
        self.assertTrue(rows[UID]['disabled_by_panel'])
        self.assertEqual(rows[UID]['file'], self.fname + '.disabled')

    def test_enabled_file_not_flagged(self) -> None:
        rows = {a['uid']: a for a in wb2api.list_auth_accounts()}
        self.assertFalse(rows[UID]['disabled_by_panel'])

    def test_glob_no_longer_matches_upstream_pattern(self) -> None:
        """改名后的文件**不匹配**上游的 `workbuddy*.json` —— 这是本方案的全部依据。

        一旦这个假设不成立（上游改了 glob），禁用就会失效（账号仍被加载），
        所以钉住它：用与上游相同的 glob 去匹配，必须匹配不到。
        """
        wb2api.set_account_disabled(self.fname, True)
        matched = [p.name for p in self.dir.glob('workbuddy*.json')]
        self.assertEqual(matched, [], f'改名后仍被上游 glob 匹配到：{matched}')


class PoolMergeReasonTest(unittest.TestCase):
    """禁用账号必然不在池里：合并状态时要写清原因，不能报成「文件有问题」。"""

    def test_reason_mentions_panel_disable(self) -> None:
        accounts = [{'uid': UID, 'file': 'workbuddy-x.json.disabled',
                     'disabled_by_panel': True, 'invalid_reason': ''}]
        # 上游返回空池（禁用后它确实不加载）
        wb2api.merge_pool_status(accounts, {'accounts': []})
        self.assertFalse(accounts[0]['in_pool'])
        self.assertIn('禁用', accounts[0]['invalid_reason'],
                      '禁用导致的「不在池里」没写清原因，界面会误报成文件损坏')

    def test_existing_reason_not_overwritten(self) -> None:
        """已有具体原因（如缺 accessToken）时不要被通用文案覆盖。"""
        accounts = [{'uid': UID, 'file': 'workbuddy-x.json.disabled',
                     'disabled_by_panel': True, 'invalid_reason': '缺少 accessToken'}]
        wb2api.merge_pool_status(accounts, {'accounts': []})
        self.assertEqual(accounts[0]['invalid_reason'], '缺少 accessToken')


if __name__ == '__main__':
    unittest.main()
