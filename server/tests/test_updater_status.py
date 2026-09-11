"""更新状态判定的回归测试。

背景：管理端更新最后一步是 `systemctl restart`，而 systemd 默认
KillMode=control-group 会把更新进程（连同一个 cgroup 里的所有进程）一起终止。
旧逻辑一看到「标记运行中但进程没了」就判为「异常中断」，
于是更新明明成功、界面却显示「更新未完成」。

这里覆盖两条判定证据：
  1. 状态里记了目标版本，且已部署版本等于它
  2. 日志里出现「管理端已更新到 X，重启服务以生效」且 X 等于已部署版本

运行：python -m unittest discover -s server/tests -t . -v
"""
from __future__ import annotations

import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from server.services import updater  # noqa: E402


class UpdateLanded(unittest.TestCase):
    def setUp(self) -> None:
        self._orig = updater.current_version

    def tearDown(self) -> None:
        updater.current_version = self._orig

    def _at(self, version: str) -> None:
        updater.current_version = lambda: version  # type: ignore[assignment]

    def test_target_version_matches(self) -> None:
        self._at('v1.0.5')
        self.assertTrue(updater._update_landed({'target_version': '1.0.5', 'logs': []}))

    def test_target_version_mismatch(self) -> None:
        self._at('v1.0.4')
        self.assertFalse(updater._update_landed({'target_version': '1.0.5', 'logs': []}))

    def test_legacy_worker_without_target_version_uses_log(self) -> None:
        """旧版脚本不写 target_version，靠日志行兜底（过渡兼容）。"""
        self._at('v1.0.5')
        status = {
            'target_version': '',
            'logs': [
                {'text': '== 更新管理端 =='},
                {'text': '管理端已更新到 v1.0.5，重启服务以生效'},
                {'text': '$ systemctl restart workbuddy-web'},
            ],
        }
        self.assertTrue(updater._update_landed(status))

    def test_log_version_does_not_match_current(self) -> None:
        """日志说更新到了 X，但磁盘上还是旧版本 → 仍算失败。"""
        self._at('v1.0.4')
        status = {
            'logs': [{'text': '管理端已更新到 v1.0.5，重启服务以生效'}],
        }
        self.assertFalse(updater._update_landed(status))

    def test_no_evidence_is_failure(self) -> None:
        self._at('v1.0.4')
        self.assertFalse(updater._update_landed({'logs': []}))


if __name__ == '__main__':
    unittest.main()
