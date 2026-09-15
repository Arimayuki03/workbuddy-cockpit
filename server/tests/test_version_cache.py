"""更新后必须清除版本检测缓存（用户报过的问题）。

现象：上游已经更新到最新，面板却**一直**显示「有更新」。

根因：`check_updates` 会把「远端最新提交」缓存 6 小时；更新完成后若不清缓存，
界面就会拿**更新前查到的** sha 去比本地 HEAD —— 两者当然不同，于是继续提示
有更新，最长持续 6 小时。

`update_manager`（管理端更新）一直有清缓存，`update_upstream`（上游更新）
漏了——所以这个现象在上游更新后必然出现。

本文件锁住两件事：
  1. 两条更新路径都必须清缓存；
  2. 清缓存用的是同一个实现（避免将来只改一处、另一处再次漏掉）。
"""
from __future__ import annotations

import importlib.util
import re
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

_ROOT = Path(__file__).resolve().parents[2]


def _load_update_mod():
    spec = importlib.util.spec_from_file_location('upd_cache', str(_ROOT / 'deploy' / 'update.py'))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


class VersionCacheClearTest(unittest.TestCase):
    """源码级断言：两条更新路径都要清缓存。

    这里读源码而不是跑更新（跑更新要 docker/systemctl）——这类"某条分支漏了
    一个收尾动作"的缺陷，源码断言是最直接的守卫。
    """

    def setUp(self) -> None:
        self.src = (_ROOT / 'deploy' / 'update.py').read_text(encoding='utf-8')

    def _body_of(self, func: str) -> str:
        m = re.search(rf'^def {func}\(.*?(?=^def |\Z)', self.src, re.S | re.M)
        self.assertIsNotNone(m, f'找不到函数 {func}')
        return m.group(0)

    def test_upstream_update_clears_cache(self) -> None:
        """上游更新后清缓存 —— 这条就是用户报的问题。"""
        body = self._body_of('update_upstream')
        self.assertIn('_clear_version_cache(', body,
                      '上游更新没清版本缓存：界面会把已装好的版本继续当新版提示 6 小时')

    def test_manager_update_clears_cache(self) -> None:
        body = self._body_of('update_manager')
        self.assertIn('_clear_version_cache(', body, '管理端更新没清版本缓存')

    def test_both_paths_share_one_implementation(self) -> None:
        """两条路径必须走同一个清理函数，不能各写一份。"""
        for fn in ('update_upstream', 'update_manager'):
            body = self._body_of(fn)
            self.assertNotIn('version-check.json', body,
                             f'{fn} 里出现了内联的缓存清理，应改用 _clear_version_cache()')

    def test_helper_removes_the_right_file(self) -> None:
        """清理函数删的必须是 check_updates 实际使用的那份缓存文件。"""
        mod = _load_update_mod()
        helper = self._body_of('_clear_version_cache')
        self.assertIn('version-check.json', helper)

        # 与消费端（updater.py）对照：两边必须是同一个文件名
        updater_src = (_ROOT / 'server' / 'services' / 'updater.py').read_text(encoding='utf-8')
        m = re.search(r"_VERSION_CACHE_FILE\s*=\s*config\.DATA_DIR\s*/\s*'([^']+)'", updater_src)
        self.assertIsNotNone(m, '找不到 _VERSION_CACHE_FILE 定义')
        self.assertIn(m.group(1), helper,
                      f'清理的文件名与 updater 读取的（{m.group(1)}）不一致')


class UpdateCheckSemanticsTest(unittest.TestCase):
    """补充：确认「缓存导致的误报」这个机制本身成立（问题的另一半）。"""

    def test_stale_cache_causes_false_positive(self) -> None:
        """用更新前的旧 sha 去比已更新的本地 HEAD —— 会误报有更新。

        这不是要"修"的行为（缓存本身是必要的），而是说明为什么更新后
        必须清缓存。
        """
        cache_latest = 'b5077d5'   # 更新前查到的远端 sha（缓存里的）
        local_head = 'd5f509e'     # 更新完成后本地 HEAD
        has_update = (bool(cache_latest) and bool(local_head)
                      and not cache_latest.startswith(local_head)
                      and not local_head.startswith(cache_latest))
        self.assertTrue(has_update, '旧缓存确实会造成误报 —— 所以更新后要清')


if __name__ == '__main__':
    unittest.main()
