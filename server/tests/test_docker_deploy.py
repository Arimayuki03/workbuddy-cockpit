"""容器部署形态的能力边界（issue #7：Docker 部署方案）。

容器部署不是"把宿主安装塞进镜像"就完事——有几处**本质差异**，若处理不当
会出现「界面说成功、实际没生效」这种最难排查的故障：

  1. **重启方式不同**。宿主用 `systemctl restart`；容器里没有 systemd，
     而且容器**无法重启自己**（除非挂 /var/run/docker.sock，那等于把宿主 root
     权限交给容器内进程——可挂载宿主根目录，比"少一个功能"危险得多，故刻意不做）。
     容器形态的正确做法是：替换代码 → 结束容器 → 由 compose 的 restart 策略
     用新代码拉起。

  2. **不支持更新上游**。重建上游容器需要 docker CLI。所以容器形态下该选项
     必须在**界面层就禁用并说明**，而不是让用户点了跑到一半才失败。

  3. **更新进程退出 ≠ 管理端重启**。更新进程是管理端拉起的子进程；它退出后
     管理端主进程仍在跑旧代码。容器形态必须结束整个容器，否则界面还是旧版。

本文件锁住这些判定，避免"改着改着把容器路径改成宿主假设"。
"""
from __future__ import annotations

import importlib.util
import os
import sys
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

_ROOT = Path(__file__).resolve().parents[2]


def _load_update_mod(**env):
    keys = ['WB_RUN_MODE', *env.keys()]
    old = {k: os.environ.get(k) for k in keys}
    try:
        os.environ.update({k: str(v) for k, v in env.items()})
        spec = importlib.util.spec_from_file_location('upd_docker',
                                                      str(_ROOT / 'deploy' / 'update.py'))
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
    finally:
        for k, v in old.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v
    return mod


class ContainerDetectionTest(unittest.TestCase):
    """运行形态判定：显式配置优先，否则探测。"""

    def test_explicit_docker(self) -> None:
        self.assertTrue(_load_update_mod(WB_RUN_MODE='docker').in_container())

    def test_explicit_systemd(self) -> None:
        self.assertFalse(_load_update_mod(WB_RUN_MODE='systemd').in_container())

    def test_auto_detects_via_dockerenv(self) -> None:
        mod = _load_update_mod(WB_RUN_MODE='auto')
        with mock.patch.object(mod.Path, 'exists', return_value=True):
            self.assertTrue(mod.in_container(), '/.dockerenv 存在时应判定为容器')

    def test_auto_detects_via_cgroup(self) -> None:
        mod = _load_update_mod(WB_RUN_MODE='auto')
        with mock.patch.object(mod.Path, 'exists', return_value=False), \
                mock.patch.object(mod.Path, 'read_text',
                                  return_value='0::/docker/abc123\n'):
            self.assertTrue(mod.in_container(), 'cgroup 含 docker 时应判定为容器')

    def test_auto_plain_host(self) -> None:
        mod = _load_update_mod(WB_RUN_MODE='auto')
        with mock.patch.object(mod.Path, 'exists', return_value=False), \
                mock.patch.object(mod.Path, 'read_text', return_value='0::/\n'):
            self.assertFalse(mod.in_container())


class RestartBehaviorTest(unittest.TestCase):
    """重启方式：宿主走 systemctl，容器交给编排层。"""

    def test_container_restart_does_not_call_systemctl(self) -> None:
        """容器里**绝不能**调用 systemctl —— 那必然失败。"""
        mod = _load_update_mod(WB_RUN_MODE='docker')
        calls: list[list[str]] = []

        class Rep:
            def __init__(self): self.lines = []
            def log(self, m, level='info'): self.lines.append((level, m))

        with mock.patch.object(mod, 'run', side_effect=lambda cmd, **k: (calls.append(cmd), (0, ''))[1]):
            mod.restart_service(Rep())
        self.assertEqual(calls, [], f'容器形态不应执行任何命令，实际：{calls}')

    def test_host_restart_calls_systemctl(self) -> None:
        mod = _load_update_mod(WB_RUN_MODE='systemd')
        calls: list[list[str]] = []

        class Rep:
            def __init__(self): self.lines = []
            def log(self, m, level='info'): self.lines.append((level, m))

        with mock.patch.object(mod, 'run', side_effect=lambda cmd, **k: (calls.append(cmd), (0, ''))[1]):
            mod.restart_service(Rep())
        self.assertTrue(any('systemctl' in c for c in calls),
                        f'宿主形态应调用 systemctl，实际：{calls}')

    def test_exit_for_restart_signals_pid1(self) -> None:
        """容器形态：向 PID 1 发 SIGTERM（让编排层用新代码拉起整个容器）。

        只结束更新进程是不够的——管理端主进程仍在跑旧代码，界面还是旧版。
        """
        import signal as _signal
        mod = _load_update_mod(WB_RUN_MODE='docker')
        killed: list[tuple[int, int]] = []

        class Rep:
            def __init__(self): self.lines = []
            def log(self, m, level='info'): self.lines.append((level, m))

        with mock.patch.object(mod.os, 'kill',
                               side_effect=lambda pid, sig: killed.append((pid, sig))), \
                mock.patch.object(mod.time, 'sleep'):
            mod._exit_for_restart(Rep())
        self.assertEqual(killed, [(1, _signal.SIGTERM)],
                         f'应向 PID 1 发 SIGTERM，实际：{killed}')


class UpstreamUpdateGuardTest(unittest.TestCase):
    """容器形态必须**前置拒绝**更新上游，并给出替代做法。"""

    def _start(self, target: str, mode: str):
        from server.services import updater
        with mock.patch.object(updater, 'in_container', return_value=(mode == 'docker')):
            return updater.start_update(target)

    def test_container_rejects_upstream(self) -> None:
        ok, msg = self._start('upstream', 'docker')
        self.assertFalse(ok, '容器里不应允许更新上游')
        self.assertIn('docker compose', msg, '应给出宿主机替代做法')

    def test_container_rejects_both(self) -> None:
        ok, msg = self._start('both', 'docker')
        self.assertFalse(ok)
        self.assertIn('docker compose', msg)

    def test_container_allows_manager(self) -> None:
        """仅更新管理端在容器里是可行的（替换代码 + 容器重启）。"""
        from server.services import updater
        with mock.patch.object(updater, 'in_container', return_value=True), \
                mock.patch.object(updater, '_lock_active', return_value=True):
            ok, msg = updater.start_update('manager')
        # 走到"已有任务在跑"这一步说明前置校验放行了（没有卡在容器拒绝上）
        self.assertFalse(ok)
        self.assertIn('已有更新任务', msg, '被容器校验挡住了 —— 管理端应放行')

    def test_status_reports_capability(self) -> None:
        """能力标志要透出给界面，否则界面无法隐藏/禁用。"""
        from server.services import updater
        for mode, can in (('docker', False), ('systemd', True)):
            with mock.patch.object(updater, 'in_container', return_value=(mode == 'docker')):
                st = updater.read_status()
            self.assertEqual(st['in_container'], mode == 'docker')
            self.assertEqual(st['can_update_upstream'], can)

    def test_frontend_hides_upstream_in_container(self) -> None:
        """界面必须读这两个字段来禁用选项（否则用户能点、然后失败）。"""
        src = (_ROOT / 'web' / 'components' / 'common' / 'settings'
               / 'UpdatePanel.tsx').read_text(encoding='utf-8')
        self.assertIn('can_update_upstream', src,
                      '更新面板没读容器能力标志 —— 用户会点到不支持的操作')
        self.assertIn('docker compose', src, '应给出宿主机替代做法')


class DockerAssetsTest(unittest.TestCase):
    """部署资产存在且关键约定正确（这些错了用户装不起来）。"""

    def test_dockerfile_present_and_non_root(self) -> None:
        df = (_ROOT / 'Dockerfile').read_text(encoding='utf-8')
        self.assertIn('FROM python:3.12', df)
        self.assertIn('USER app', df, '不应以 root 运行容器')
        self.assertIn('WB_RUN_MODE=docker', df,
                      '镜像里没设运行形态 —— 容器内会误判成宿主、去调 systemctl')
        # 前端产物必须来自静态导出（server 构建会产出 server 版，FastAPI 托管不了）
        self.assertIn('COPY web/out', df)

    def test_compose_has_restart_policy(self) -> None:
        """restart 策略是容器版「一键更新」能生效的前提：
        更新进程结束容器后，靠它用新代码拉起。"""
        import yaml
        dc = yaml.safe_load((_ROOT / 'docker-compose.yml').read_text(encoding='utf-8'))
        svc = dc['services']['workbuddy-manager']
        self.assertIn(svc.get('restart'), ('unless-stopped', 'always'),
                      'restart 策略缺失 —— 容器更新后将不会自动恢复')
        # 默认只监听本机：管理端持有全部账号凭据，不该直接暴露公网
        ports = svc.get('ports') or []
        self.assertTrue(any('127.0.0.1' in str(p) for p in ports),
                        '端口未绑定到 127.0.0.1 —— 管理端不应默认暴露公网')

    def test_compose_persists_data(self) -> None:
        import yaml
        dc = yaml.safe_load((_ROOT / 'docker-compose.yml').read_text(encoding='utf-8'))
        vols = dc['services']['workbuddy-manager'].get('volumes') or []
        self.assertTrue(any('/app/data' in str(v) for v in vols),
                        '未持久化 data 卷 —— 重建容器会丢失统计与审计记录')

    def test_no_docker_socket_mount(self) -> None:
        """绝不挂 docker.sock：那等于把宿主 root 权限交给容器内进程。

        只检查 **volumes 段**，不搜全文——注释里正说明"为什么不挂"，
        全文搜索会命中那段说明（第一版就这样误报过）。
        """
        import yaml
        dc = yaml.safe_load((_ROOT / 'docker-compose.yml').read_text(encoding='utf-8'))
        vols = dc['services']['workbuddy-manager'].get('volumes') or []
        offenders = [v for v in vols if 'docker.sock' in str(v)]
        self.assertEqual(offenders, [],
                         f'挂载了 docker 套接字 —— 容器内进程可获得宿主 root 权限：{offenders}')


if __name__ == '__main__':
    unittest.main()


class ContainerReloadHintTest(unittest.TestCase):
    """容器部署下保存上游配置：必须**如实告知需要手动重启上游**。

    为什么这是必要的：上游只在进程启动时读一次 config.json，保存后必须重启
    上游容器才生效。宿主部署时管理端能直接 `docker restart`，但容器里**没有
    docker 命令**（我们刻意不挂 docker.sock —— 那等于把宿主 root 交给容器内
    进程）。若此时仍显示「正在自动应用到上游…」，用户会以为改完就生效了，
    然后对着不生效的配置排查半天。

    修法：容器形态下把 reload_scheduled 置 False 并带回 reload_hint，
    界面改用醒目提示转达。
    """

    @staticmethod
    def _save(container: bool) -> dict:
        """调用 save_upstream（异步），返回响应体。"""
        import asyncio
        from server.routers import settings as st
        with mock.patch.object(st.updater, 'in_container', return_value=container),                 mock.patch.object(st.wb2api, 'save_upstream_config',
                                  return_value={'available': True}),                 mock.patch.object(st.security, 'audit'),                 mock.patch.object(st, 'client_ip', return_value='127.0.0.1'),                 mock.patch.object(st.reload, 'request_restart', return_value=True):
            return asyncio.run(st.save_upstream(
                {'schedule': {'checkin_hours': [9]}}, None,
                {'username': 't', 'role': 'admin'}))

    def test_hint_present_in_container(self) -> None:
        res = self._save(container=True)
        self.assertFalse(res['reload_scheduled'], '容器里不应声称已调度重载')
        self.assertIn('reload_hint', res, '容器里必须给出手动重启指引')
        self.assertIn('docker compose', res['reload_hint'])

    def test_no_hint_on_host(self) -> None:
        res = self._save(container=False)
        self.assertTrue(res['reload_scheduled'], '宿主部署应正常调度自动重载')
        self.assertNotIn('reload_hint', res)

    def test_frontend_surfaces_hint(self) -> None:
        """界面必须把 reload_hint 显示出来（用醒目提示而不是"自动应用"）。"""
        src = (_ROOT / 'web' / 'app' / '(main)' / 'settings' / 'page.tsx'
               ).read_text(encoding='utf-8')
        self.assertIn('reload_hint', src,
                      '设置页没读 reload_hint —— 容器用户会以为配置已生效')
        self.assertIn('notify.warn', src, '应以醒目提示（warn）转达')
