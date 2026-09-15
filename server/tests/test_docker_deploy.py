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
    """「能否更新上游」按**实际能力**判定，不按是否容器。

    这是本项目的一处**判断修正**，值得写下来：
      初版按「在容器里就不允许更新上游」实现，理由写成"挂 docker.sock 等于把
      宿主 root 交给容器，比少一个功能危险得多"。这个理由**不成立**——宿主
      部署时本服务本来就是 root 运行（systemd 单元无 User=、安装脚本要求 root），
      而 root 进程本来就能 `docker run -v /:/host` 拿到宿主文件系统。也就是说
      宿主部署的权限**已经等价于**挂 docker.sock。

    结论：按能力判定才正确 —— 容器挂了 socket 就能（与宿主部署对齐），宿主没装
    docker 反而不能。按「是否容器」判会把可用场景误判为不可用。
    """

    def _start(self, target: str, docker_ok: bool):
        from server.services import updater
        with mock.patch.object(updater, 'can_control_docker', return_value=docker_ok),                 mock.patch.object(updater, '_lock_active', return_value=True):
            return updater.start_update(target)

    def test_no_docker_rejects_upstream(self) -> None:
        ok, msg = self._start('upstream', False)
        self.assertFalse(ok, '没有 docker 能力时不应允许更新上游')
        self.assertIn('docker', msg, '应说明原因并给出替代做法')
        self.assertIn('docker compose', msg)

    def test_no_docker_rejects_both(self) -> None:
        ok, msg = self._start('both', False)
        self.assertFalse(ok)
        self.assertIn('docker compose', msg)

    def test_docker_available_allows_upstream(self) -> None:
        """有 docker 能力时（含挂了 socket 的容器）应放行到下一步。"""
        ok, msg = self._start('upstream', True)
        # 走到「已有更新任务」说明前置校验放行了
        self.assertFalse(ok)
        self.assertIn('已有更新任务', msg, '有 docker 能力却被前置校验挡住了')

    def test_manager_always_allowed(self) -> None:
        """仅更新管理端不需要 docker —— 任何形态都应放行。"""
        ok, msg = self._start('manager', False)
        self.assertFalse(ok)
        self.assertIn('已有更新任务', msg)

    def test_status_reports_capability(self) -> None:
        """能力标志要透出给界面（界面据此禁用/提示）。"""
        from server.services import updater
        for docker_ok in (False, True):
            with mock.patch.object(updater, 'can_control_docker', return_value=docker_ok):
                st = updater.read_status()
            self.assertEqual(st['can_update_upstream'], docker_ok)

    def test_capability_not_derived_from_container(self) -> None:
        """关键断言：能力**不看**是否容器。

        容器挂了 socket 就能更新上游；按容器判定会把这种（我们推荐的默认
        配置）误判为不可用。
        """
        from server.services import updater
        with mock.patch.object(updater, 'in_container', return_value=True),                 mock.patch.object(updater, 'can_control_docker', return_value=True):
            self.assertTrue(updater.read_status()['can_update_upstream'],
                            '容器 + 有 docker 能力时应可用 —— 不要按容器判定')
        with mock.patch.object(updater, 'in_container', return_value=False),                 mock.patch.object(updater, 'can_control_docker', return_value=False):
            self.assertFalse(updater.read_status()['can_update_upstream'],
                             '宿主但没装 docker 时应不可用')

    def test_frontend_uses_capability_flag(self) -> None:
        src = (_ROOT / 'web' / 'components' / 'common' / 'settings'
               / 'UpdatePanel.tsx').read_text(encoding='utf-8')
        self.assertIn('can_update_upstream', src,
                      '更新面板没读能力标志 —— 用户会点到不支持的操作')


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

    def test_upstream_dir_mounted(self) -> None:
        """必须挂载上游仓库目录。

        没挂的话容器内既拿不到上游的 docker-compose.yml（端口收敛无从下手），
        也无法在容器内 git pull —— 「更新上游」直接做不到。
        （这正是初版的问题：只挂了 auths 与 config.json 两条子路径。）
        """
        import yaml
        dc = yaml.safe_load((_ROOT / 'docker-compose.yml').read_text(encoding='utf-8'))
        vols = [str(v) for v in (dc['services']['workbuddy-manager'].get('volumes') or [])]
        self.assertTrue(any('/opt/workbuddy2api' in v for v in vols),
                        '未挂载上游目录 —— 容器版将无法更新上游')

    def test_docker_socket_mounted_for_full_capability(self) -> None:
        """默认挂载 docker.sock，使容器版与宿主部署能力对齐。

        这是**判断修正**：初版刻意不挂，理由写成"挂了等于把宿主 root 交给容器"。
        但宿主部署本来就是 root（systemd 无 User=），而 root 进程本来就能
        `docker run -v /:/host` —— 两者权限等价，不挂只是让功能残缺。
        若不想要，注释掉即可（功能会自动降级并如实提示）。
        """
        import yaml
        dc = yaml.safe_load((_ROOT / 'docker-compose.yml').read_text(encoding='utf-8'))
        vols = [str(v) for v in (dc['services']['workbuddy-manager'].get('volumes') or [])]
        self.assertTrue(any('docker.sock' in v for v in vols),
                        '未挂 docker.sock —— 容器版将无法重载/更新上游')


if __name__ == '__main__':
    unittest.main()


class ContainerReloadHintTest(unittest.TestCase):
    """无法自动重载上游时必须**如实告知**（而不是显示"正在自动应用"）。

    上游只在进程启动时读 config.json，改完必须重启上游容器才生效。宿主部署时
    管理端能直接 `docker restart`；但若环境**没有 docker 能力**（宿主没装
    docker，或容器没挂 docker.sock），这一步就做不了。此时若仍显示「正在自动
    应用到上游…」，用户会以为生效了，然后对着不生效的配置排查半天。

    判据同样是**实际能力**，不是"是否容器"。
    """

    @staticmethod
    def _save(docker_ok: bool) -> dict:
        import asyncio
        from server.routers import settings as st
        with mock.patch.object(st.updater, 'can_control_docker', return_value=docker_ok),                 mock.patch.object(st.wb2api, 'save_upstream_config',
                                  return_value={'available': True}),                 mock.patch.object(st.security, 'audit'),                 mock.patch.object(st, 'client_ip', return_value='127.0.0.1'),                 mock.patch.object(st.reload, 'request_restart', return_value=True):
            return asyncio.run(st.save_upstream(
                {'schedule': {'checkin_hours': [9]}}, None,
                {'username': 't', 'role': 'admin'}))

    def test_hint_when_docker_unavailable(self) -> None:
        res = self._save(docker_ok=False)
        self.assertFalse(res['reload_scheduled'], '不应声称已调度重载')
        self.assertIn('reload_hint', res, '必须给出手动重启指引')
        self.assertIn('docker compose', res['reload_hint'])

    def test_no_hint_when_docker_available(self) -> None:
        res = self._save(docker_ok=True)
        self.assertTrue(res['reload_scheduled'], '有 docker 能力时应正常自动重载')
        self.assertNotIn('reload_hint', res)

    def test_frontend_surfaces_hint(self) -> None:
        src = (_ROOT / 'web' / 'app' / '(main)' / 'settings' / 'page.tsx'
               ).read_text(encoding='utf-8')
        self.assertIn('reload_hint', src,
                      '设置页没读 reload_hint —— 用户会以为配置已生效')
        self.assertIn('notify.warn', src, '应以醒目提示（warn）转达')


class ReleasePackageIncludesDockerAssetsTest(unittest.TestCase):
    """发布包必须包含容器部署资产。

    实测漏过：打包步骤只复制了 server/ web/out deploy/ docs/ 与几个文档，
    **Dockerfile 与 docker-compose.yml 没打进去** —— 用户下载发布包后用不了
    容器部署（得回仓库另取这两个文件）。

    这条测试直接断言打包步骤的 cp 列表，防止再次漏掉。
    """

    def test_workflow_packages_docker_assets(self) -> None:
        wf = (_ROOT / '.github' / 'workflows' / 'release.yml').read_text(encoding='utf-8')
        # 找到「组装发布目录」那一步的内容
        start = wf.find('组装发布目录')
        self.assertGreater(start, 0, '找不到打包步骤')
        end = wf.find('- name:', start + 10)
        block = wf[start:end if end > 0 else len(wf)]
        for asset in ('Dockerfile', 'docker-compose.yml'):
            self.assertIn(asset, block,
                          f'发布包没打进去 {asset} —— 用户拿到包也用不了容器部署')
