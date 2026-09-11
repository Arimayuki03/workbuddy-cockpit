#!/usr/bin/env python3
"""WorkBuddy Manager 一键更新 worker。

由管理端后台调用（脱离父进程运行，因此重启服务不会中断更新流程），
也可单独在命令行执行：

    python3 deploy/update.py --target both
    python3 deploy/update.py --target upstream
    python3 deploy/update.py --target manager

设计要点
--------
1. **自包含**：仅用标准库，避免「更新过程中依赖被替换」导致脚本自身失败。
2. **状态外置**：进度写入 JSON 文件，管理端读取该文件展示实时日志。
   （更新会重启管理端，若用 HTTP 流式返回会被中断）
3. **幂等与安全**：上游更新会保留账号文件与配置；并**强制把端口绑定收敛为
   127.0.0.1**，避免 upstream 仓库里的 `7863:7863` 覆盖我们的安全加固。
"""
from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path

# ── 运行环境（与 server/config.py 保持一致的默认值）──────────
INSTALL_DIR = Path(os.environ.get('WB_INSTALL_DIR') or Path(__file__).resolve().parent.parent)
UPSTREAM_DIR = Path(os.environ.get('WB_UPSTREAM_DIR') or '/opt/workbuddy2api')
UPSTREAM_PORT = int(os.environ.get('WB_UPSTREAM_PORT') or 7863)
MANAGER_PORT = int(os.environ.get('WB_MANAGER_PORT') or 7864)
MANAGER_REPO = os.environ.get('WB_MANAGER_REPO') or 'ithtelab/workbuddy-manager'
UPSTREAM_REPO = os.environ.get('WB_UPSTREAM_REPO') or 'https://github.com/Sliverkiss/workbuddy2api.git'
SERVICE_NAME = os.environ.get('WB_SERVICE_NAME') or 'workbuddy-web'
DATA_DIR = Path(os.environ.get('WB_DATA_DIR') or INSTALL_DIR / 'data')
STATUS_FILE = Path(os.environ.get('WB_UPDATE_STATUS') or DATA_DIR / 'update-status.json')

STEP_TIMEOUT = int(os.environ.get('WB_UPDATE_STEP_TIMEOUT') or 900)


# ── 状态写入 ─────────────────────────────────────────────
class Reporter:
    def __init__(self, target: str) -> None:
        self.start = time.time()
        self.state: dict = {
            'running': True,
            'ok': None,
            'target': target,
            'step': '初始化',
            'logs': [],
            'started_at': int(self.start),
            'finished_at': None,
            'duration': 0,
            'pid': os.getpid(),
        }
        self.flush()

    def log(self, message: str, level: str = 'info') -> None:
        line = f'[{time.strftime("%H:%M:%S")}] {message}'
        self.state['logs'].append({'ts': int(time.time()), 'level': level, 'text': message})
        # 终端输出便于命令行单独运行
        print(line, flush=True)
        self.flush()

    def step(self, name: str) -> None:
        self.state['step'] = name
        self.log(f'== {name} ==')
        self.flush()

    def finish(self, ok: bool) -> None:
        self.state['running'] = False
        self.state['ok'] = ok
        self.state['finished_at'] = int(time.time())
        self.state['duration'] = round(time.time() - self.start, 1)
        self.flush()

    def set_target_version(self, tag: str) -> None:
        """记录本次要更新到的版本。

        管理端重启会把本进程一并终止（见 update_manager 的说明），
        管理端读到「运行中但进程已不在」时，用这个版本号核对代码是否已就位，
        从而区分「更新成功、只是被重启带走」与「真的崩了」。
        """
        self.state['target_version'] = str(tag or '').strip().lstrip('vV')
        self.flush()

    def flush(self) -> None:
        try:
            DATA_DIR.mkdir(parents=True, exist_ok=True)
            tmp = STATUS_FILE.with_suffix('.tmp')
            tmp.write_text(json.dumps(self.state, ensure_ascii=False), encoding='utf-8')
            tmp.replace(STATUS_FILE)
        except Exception:  # noqa: BLE001
            pass


# ── 命令执行 ─────────────────────────────────────────────
def run(cmd: list[str], cwd: Path | None = None, rep: Reporter | None = None,
        timeout: int = STEP_TIMEOUT, check: bool = True) -> tuple[int, str]:
    if rep:
        rep.log(f'$ {" ".join(cmd)}')
    try:
        proc = subprocess.run(
            cmd, cwd=str(cwd) if cwd else None,
            capture_output=True, text=True, timeout=timeout,
        )
    except FileNotFoundError as exc:
        if rep:
            rep.log(f'命令不存在: {exc}', 'error')
        if check:
            raise
        return 127, str(exc)
    except subprocess.TimeoutExpired:
        if rep:
            rep.log(f'命令超时（{timeout}s）', 'error')
        if check:
            raise
        return 124, 'timeout'

    out = (proc.stdout or '') + (proc.stderr or '')
    if rep and out.strip():
        for line in out.strip().splitlines()[-30:]:
            rep.log(f'  {line}')
    if proc.returncode != 0 and check:
        raise RuntimeError(f'命令失败（exit {proc.returncode}）: {" ".join(cmd)}')
    return proc.returncode, out


def http_json(url: str, timeout: int = 20) -> dict:
    req = urllib.request.Request(url, headers={
        'Accept': 'application/vnd.github+json',
        'User-Agent': 'workbuddy-manager-updater',
    })
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read().decode('utf-8'))


def download(url: str, dest: Path, rep: Reporter) -> None:
    rep.log(f'下载 {url}')
    req = urllib.request.Request(url, headers={'User-Agent': 'workbuddy-manager-updater'})
    with urllib.request.urlopen(req, timeout=120) as resp, open(dest, 'wb') as fh:
        shutil.copyfileobj(resp, fh)
    size = dest.stat().st_size
    rep.log(f'  完成（{size / 1024 / 1024:.2f} MB）')
    if size < 100_000:
        raise RuntimeError('下载内容异常偏小，可能是错误页面')


# ── 上游更新 ─────────────────────────────────────────────
def enforce_local_bind(rep: Reporter) -> None:
    """把 compose 的端口绑定收敛为仅本机。

    upstream 仓库里是 `7863:7863`（公网可达）；我们的安全基线要求
    `127.0.0.1:7863:7863`。每次更新后都要重新施加，否则会悄悄回退。
    """
    compose = UPSTREAM_DIR / 'docker-compose.yml'
    if not compose.is_file():
        rep.log('未找到 docker-compose.yml，跳过端口收敛', 'warn')
        return
    text = compose.read_text(encoding='utf-8')
    wide = f'"{UPSTREAM_PORT}:{UPSTREAM_PORT}"'
    bound = f'"127.0.0.1:{UPSTREAM_PORT}:{UPSTREAM_PORT}"'
    if bound in text:
        rep.log('端口绑定已是仅本机（127.0.0.1）')
        return
    if wide in text:
        compose.write_text(text.replace(wide, bound), encoding='utf-8')
        rep.log(f'已将端口绑定收敛为 {bound}（安全基线）')
    else:
        rep.log('未匹配到端口绑定行，请人工确认 compose 配置', 'warn')


def update_upstream(rep: Reporter) -> None:
    rep.step('更新上游 workbuddy2api')

    if not (UPSTREAM_DIR / '.git').is_dir():
        rep.log(f'{UPSTREAM_DIR} 不是 git 仓库，跳过上游更新', 'warn')
        return

    which = shutil.which('git')
    if not which:
        raise RuntimeError('未安装 git，无法更新上游')

    # 1) 若存在本地改动，先备份再暂存，避免 pull 冲突
    rc, status = run(['git', 'status', '--porcelain'], cwd=UPSTREAM_DIR, rep=rep, check=False)
    dirty = [l for l in status.splitlines() if l.strip()]
    if dirty:
        rep.log(f'检测到 {len(dirty)} 处本地改动，先备份')
        backup = DATA_DIR / 'upstream-local-changes.patch'
        rc, diff = run(['git', 'diff'], cwd=UPSTREAM_DIR, rep=rep, check=False)
        if diff.strip():
            backup.write_text(diff, encoding='utf-8')
            rep.log(f'  已备份到 {backup}')
        # 丢弃本地改动：其中包含我们的端口收敛，稍后会重新施加
        run(['git', 'checkout', '--', '.'], cwd=UPSTREAM_DIR, rep=rep, check=False)

    # 2) 拉取
    rep.log('拉取上游最新代码…')
    rc, out = run(['git', 'fetch', '--depth', '1', 'origin'], cwd=UPSTREAM_DIR, rep=rep, check=False)
    if rc != 0:
        rep.log('git fetch 失败（网络问题？）', 'warn')
    branch = 'master'
    rc, out = run(['git', 'rev-parse', '--abbrev-ref', 'HEAD'], cwd=UPSTREAM_DIR, rep=rep, check=False)
    if rc == 0 and out.strip():
        branch = out.strip()
    before = ''
    rc, out = run(['git', 'rev-parse', 'HEAD'], cwd=UPSTREAM_DIR, rep=rep, check=False)
    if rc == 0:
        before = out.strip()[:8]

    rc, out = run(['git', 'pull', '--ff-only', 'origin', branch], cwd=UPSTREAM_DIR, rep=rep, check=False)
    if rc != 0:
        rep.log('fast-forward 失败，尝试硬重置到远端（本地改动已备份）', 'warn')
        run(['git', 'reset', '--hard', f'origin/{branch}'], cwd=UPSTREAM_DIR, rep=rep, check=False)

    after = ''
    rc, out = run(['git', 'rev-parse', 'HEAD'], cwd=UPSTREAM_DIR, rep=rep, check=False)
    if rc == 0:
        after = out.strip()[:8]
    if before and after and before == after:
        rep.log(f'上游已是最新（{after}）')
    else:
        rep.log(f'上游代码更新：{before or "?"} → {after or "?"}')

    # 3) 恢复安全基线
    enforce_local_bind(rep)

    # 4) 重建并启动
    rep.log('重建并启动上游容器（首次可能需数分钟）…')
    compose_cmd = ['docker', 'compose'] if _has_compose_v2() else ['docker-compose']
    run(compose_cmd + ['up', '-d', '--build'], cwd=UPSTREAM_DIR, rep=rep)

    # 5) 等待就绪
    rep.log('等待上游就绪…')
    if wait_health(f'http://127.0.0.1:{UPSTREAM_PORT}/healthz', 90, rep):
        rep.log('上游已就绪')
    else:
        rep.log('上游未在预期时间内就绪，请查看容器日志', 'warn')


def _has_compose_v2() -> bool:
    try:
        return subprocess.run(['docker', 'compose', 'version'],
                              capture_output=True, timeout=15).returncode == 0
    except Exception:  # noqa: BLE001
        return False


def wait_health(url: str, tries: int, rep: Reporter) -> bool:
    for _ in range(tries):
        try:
            with urllib.request.urlopen(url, timeout=3) as resp:
                if resp.status == 200:
                    return True
        except Exception:  # noqa: BLE001
            pass
        time.sleep(2)
    return False


# ── 管理端更新 ───────────────────────────────────────────
def update_manager(rep: Reporter) -> None:
    rep.step('更新管理端')

    # 1) 取最新 Release
    try:
        rel = http_json(f'https://api.github.com/repos/{MANAGER_REPO}/releases/latest')
    except urllib.error.HTTPError as exc:
        raise RuntimeError(f'获取 Release 失败：HTTP {exc.code}') from exc
    except Exception as exc:  # noqa: BLE001
        raise RuntimeError(f'无法访问 GitHub API：{exc}') from exc

    tag = rel.get('tag_name') or ''
    rep.log(f'最新版本：{tag or "(未知)"}')
    rep.set_target_version(tag)

    current = read_local_version()
    if current and tag and current == tag.lstrip('v'):
        rep.log(f'当前已是最新版本（{tag}）')
        return

    assets = rel.get('assets') or []
    pkg = next((a for a in assets if str(a.get('name', '')).endswith('.tar.gz')), None)
    if not pkg:
        raise RuntimeError('该 Release 未提供 .tar.gz 产物')

    # 2) 下载并解压到临时目录
    with tempfile.TemporaryDirectory(prefix='wbm-update-') as tmpdir:
        tmp = Path(tmpdir)
        archive = tmp / pkg['name']
        download(pkg['browser_download_url'], archive, rep)

        rep.log('解压…')
        with tarfile.open(archive, 'r:gz') as tf:
            _safe_extract(tf, tmp)
        roots = [p for p in tmp.iterdir() if p.is_dir()]
        if len(roots) != 1:
            raise RuntimeError('压缩包结构异常（应含单一顶层目录）')
        new_root = roots[0]

        new_server = new_root / 'server'
        new_web_out = new_root / 'web' / 'out'
        if not (new_server / 'main.py').is_file():
            raise RuntimeError('新包缺少 server/main.py，中止更新')
        if not (new_web_out / 'index.html').is_file():
            raise RuntimeError('新包缺少 web/out/index.html，中止更新')

        # 3) 备份当前代码后替换
        ts = time.strftime('%Y%m%d-%H%M%S')
        backup = INSTALL_DIR / f'backup-{ts}'
        backup.mkdir(parents=True, exist_ok=True)
        rep.log(f'备份当前版本到 {backup}')
        shutil.copytree(INSTALL_DIR / 'server', backup / 'server', dirs_exist_ok=True)
        if (INSTALL_DIR / 'web' / 'out').is_dir():
            shutil.copytree(INSTALL_DIR / 'web' / 'out', backup / 'web-out', dirs_exist_ok=True)

        rep.log('替换 server/')
        shutil.rmtree(INSTALL_DIR / 'server', ignore_errors=True)
        shutil.copytree(new_server, INSTALL_DIR / 'server')
        # 清掉旧字节码，避免加载到过期模块
        for pc in (INSTALL_DIR / 'server').rglob('__pycache__'):
            shutil.rmtree(pc, ignore_errors=True)

        rep.log('替换 web/out/')
        target_web = INSTALL_DIR / 'web' / 'out'
        if target_web.is_dir():
            shutil.rmtree(target_web, ignore_errors=True)
        shutil.copytree(new_web_out, target_web)

        # deploy/ 里的脚本也可能更新（如 systemd 单元）
        if (new_root / 'deploy').is_dir():
            shutil.copytree(new_root / 'deploy', INSTALL_DIR / 'deploy', dirs_exist_ok=True)
            rep.log('同步 deploy/')

        # 版本标记：界面「当前版本」与更新提醒都以它为准，必须一并替换，
        # 否则更新后仍显示旧版本，并一直提示「发现新版本可用」
        new_marker = new_root / '.version'
        if new_marker.is_file():
            shutil.copyfile(new_marker, INSTALL_DIR / '.version')
            rep.log(f'更新版本标记：{new_marker.read_text(encoding="utf-8").strip()}')

    # 4) 依赖有变化则重装
    req = INSTALL_DIR / 'server' / 'requirements.txt'
    if req.is_file():
        py = sys.executable
        rep.log('检查 / 安装 Python 依赖…')
        run([py, '-m', 'pip', 'install', '-q', '-r', str(req)], rep=rep, check=False)

    rep.log(f'管理端已更新到 {tag}，重启服务以生效')

    # 清掉版本检测缓存：缓存里存的是「更新前」查到的 latest，留着会让界面
    # 拿旧 latest 跟新版本比较，出现「v1.0.5 → v1.0.4」这类把降级当更新的提示，
    # 也会让刚发布的新版本最长 6 小时才被发现。
    try:
        (DATA_DIR / 'version-check.json').unlink(missing_ok=True)
        rep.log('已清除版本检测缓存')
    except Exception:  # noqa: BLE001
        pass

    # 先把终态落盘，再重启：systemd 默认 KillMode=control-group，restart 会连同
    # 本进程一起终止（start_new_session 只脱离终端会话，并未脱离 service 的 cgroup），
    # 若等重启之后再写状态就永远写不到了。
    rep.finish(True)
    rc, _ = run(['systemctl', 'restart', SERVICE_NAME], rep=rep, check=False)
    if rc != 0:
        raise RuntimeError(f'重启服务失败（systemctl 返回 {rc}），请手动执行 systemctl status {SERVICE_NAME}')
    rep.log('服务已重启')


def read_local_version() -> str:
    """读取当前部署版本（优先取部署时留下的标记，否则读代码里的版本号）。"""
    marker = INSTALL_DIR / '.version'
    if marker.is_file():
        return marker.read_text(encoding='utf-8').strip().lstrip('v')
    main_py = INSTALL_DIR / 'server' / 'main.py'
    if main_py.is_file():
        for line in main_py.read_text(encoding='utf-8').splitlines():
            line = line.strip()
            if line.startswith('version='):
                return line.split('=', 1)[1].strip().strip("'\"")
    return ''


def _safe_extract(tf: tarfile.TarFile, dest: Path) -> None:
    """防目录穿越：拒绝绝对路径与 .. 路径。"""
    base = dest.resolve()
    for member in tf.getmembers():
        target = (dest / member.name).resolve()
        if not str(target).startswith(str(base)):
            raise RuntimeError(f'压缩包含非法路径：{member.name}')
    tf.extractall(dest)


# ── 入口 ─────────────────────────────────────────────────
def main() -> int:
    parser = argparse.ArgumentParser(description='WorkBuddy Manager 更新')
    parser.add_argument('--target', choices=['manager', 'upstream', 'both'], default='both')
    args = parser.parse_args()

    rep = Reporter(args.target)
    rep.log(f'开始更新（target={args.target}，安装目录={INSTALL_DIR}）')

    ok = True
    try:
        if args.target in ('upstream', 'both'):
            update_upstream(rep)
        if args.target in ('manager', 'both'):
            update_manager(rep)
    except Exception as exc:  # noqa: BLE001
        rep.log(f'更新失败：{exc}', 'error')
        ok = False

    rep.log('更新完成' if ok else '更新未完成，请检查上方日志')
    rep.finish(ok)
    return 0 if ok else 1


if __name__ == '__main__':
    raise SystemExit(main())
