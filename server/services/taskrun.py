"""成长任务一键执行（调用上游自带的 `task_runner.py`）。

为什么要做这个
--------------
上游 `workbuddy2api` 自带 `scripts/task_runner.py`，能覆盖约 20 种成长中心任务
（模板 / 专家 / 技能 / 自动化 / Buddy / 画布 / 各类对话…），但：

  · 它被上游定性为**辅助工具**（README「辅助工具」一节），不是定时任务；
  · 上游调度器只自动跑其中 **6 类**（签到 / 活跃地图 / 猫猫旅行 / 保活 /
    开学季 / 夜猫），成长中心那批**没有任何自动入口**；
  · 本面板此前是纯只读的（只采集展示日志），点不了任何任务。

于是这些任务只能上服务器手敲命令，这就是 issue #19 的诉求。

本模块只负责**调用上游那个脚本**，不自己实现上报协议 —— 脚本是「与官方客户端
逐字对齐」的那份参照实现，自己重写一份必然漂移。

三种模式（按风险分级，不允许任意参数）
------------------------------------
  · `preview` —— dry-run，**只读**：只查询并展示「哪些能点亮、哪些能领」，
    不发任何写请求（脚本默认就是 dry-run，此处不传 `--yes`）。
  · `claim`   —— `--yes --only-claim`：**只把已完成任务的奖励领回来**，
    是幂等的（已领过服务端返回业务错误，视为正常）。不伪造任何行为。
  · `full`    —— `--yes`：点亮 + 领奖。**会伪造活跃上报**（造画布、连发对话、
    批量使用专家等），这是风控最容易识别的模式，上游自己都标注「写操作慎用，
    请确认决策后再跑」。因此**只允许手动触发，且必须显式确认**，不参与定时。

定时只跑 `claim`：它是幂等的领奖，不产生任何伪造行为，风险最低。

安全约束
--------
  · 一次只跑一个（`_lock`）：并发跑等于把同一账号的写请求叠在一起，
    既放大风控信号，也让输出交错难读。
  · 模式走**白名单**，账号标识做字符校验后作为独立 argv 传入（不拼 shell），
    因此不存在命令注入面。
  · 执行前先确认上游目录与脚本存在，**前置拒绝并给出替代做法**，
    而不是跑到一半才失败。
  · 账号目录通过 `WB2A_AUTHS` 显式传给脚本，保证它读的就是本面板管的那批
    auth 文件（脚本自身的回落顺序与本面板的 AUTH_DIR 可能不一致）。
"""
from __future__ import annotations

import asyncio
import logging
import re
import time
from pathlib import Path

from .. import config, db

logger = logging.getLogger('workbuddy.taskrun')

# 运行模式白名单（见模块注释的风险分级）。值 = 传给脚本的参数。
_MODE_ARGS: dict[str, list[str]] = {
    'preview': [],                       # 默认 dry-run，只读
    'claim': ['--yes', '--only-claim'],  # 幂等领奖
    'full': ['--yes'],                   # 点亮 + 领奖（会伪造上报）
}

# 单次运行上限：全量跑 54 个账号时每账号要发很多上报（动作间隔 ≥1s），
# 留足时间；超时后终止并如实报告，不无限挂着。
RUN_TIMEOUT_SECONDS = 1800

# 输出缓冲上限：脚本会为每个动作打一行，全量批量可能几千行。只保留尾部，
# 避免长时间运行把内存吃满（界面也只需要看最近的）。
_MAX_LOG_LINES = 400

# 账号标识：uid 前缀（16 进制）或字面量 ALL。做白名单式字符校验，
# 因为它会作为独立 argv 传进子进程。
_ACCOUNT_RE = re.compile(r'^[0-9a-zA-Z_-]{1,64}$')

_state: dict = {
    'running': False,
    'mode': '',
    'target': '',
    'started_at': 0,
    'finished_at': 0,
    'exit_code': None,
    'timed_out': False,
    'error': '',
    'lines': [],
}
_task: asyncio.Task | None = None


def _script_path() -> Path:
    """上游脚本路径（`<上游目录>/scripts/task_runner.py`）。"""
    from . import updater  # 复用既有的上游目录推断，避免两处口径不一
    return updater._upstream_dir() / 'scripts' / 'task_runner.py'


def _python() -> str:
    """解释器：脚本依赖第三方库为零（纯标准库），用当前解释器最稳。"""
    import sys
    return sys.executable or 'python3'


def available() -> tuple[bool, str]:
    """能否执行（上游目录与脚本是否到位）。返回 (可用, 说明)。

    单独抽出来是为了让接口能在**动手前**就拒绝，并把原因说清楚 ——
    用户看到「跑不了」时最想知道的是「为什么、我该怎么办」。
    """
    script = _script_path()
    if not script.is_file():
        return False, (
            f'未找到上游任务脚本（{script}）。'
            '该功能调用的是上游 workbuddy2api 自带的 scripts/task_runner.py，'
            '请确认上游目录已挂载且版本较新（可到「设置 → 系统更新」更新上游）。'
        )
    if not config.AUTH_DIR.is_dir():
        return False, (
            f'账号目录不存在（{config.AUTH_DIR}），无法确定对哪些账号执行。'
            '请检查 WB_AUTH_DIR 配置。'
        )
    return True, ''


def status() -> dict:
    """当前/上次运行状态（供界面轮询）。"""
    snap = dict(_state)
    snap['lines'] = list(_state['lines'])
    snap['available'], snap['unavailable_reason'] = available()
    snaps = db.get_setting('task_claim_schedule') or {}
    snap['schedule'] = snaps if isinstance(snaps, dict) else {}
    return snap


def build_command(mode: str, target: str) -> list[str]:
    """构造 argv（不经过 shell，因此不做任何转义）。"""
    if mode not in _MODE_ARGS:
        raise ValueError(f'不支持的模式：{mode}')
    if not _ACCOUNT_RE.match(target or ''):
        raise ValueError('账号标识不合法（只允许 uid 前缀或 ALL）')
    return [_python(), str(_script_path()), target, *_MODE_ARGS[mode]]


def start(mode: str, target: str = 'ALL') -> tuple[bool, str]:
    """启动一次执行（后台）。返回 (是否已启动, 说明)。"""
    ok, why = available()
    if not ok:
        return False, why
    if _state['running']:
        return False, f'已有任务正在执行（{_state["mode"]} / {_state["target"]}），请等它结束'

    try:
        argv = build_command(mode, target)
    except ValueError as exc:
        return False, str(exc)

    global _task
    try:
        loop = asyncio.get_running_loop()
    except RuntimeError:
        return False, '当前环境没有事件循环，无法后台执行'

    _state.update({
        'running': True, 'mode': mode, 'target': target,
        'started_at': int(time.time()), 'finished_at': 0,
        'exit_code': None, 'timed_out': False, 'error': '', 'lines': [],
    })
    _task = loop.create_task(_run(argv, mode, target))
    return True, f'已开始执行（{mode}）'


async def _run(argv: list[str], mode: str, target: str) -> None:
    """跑子进程并把输出累积到状态里（尾部保留）。"""
    env = dict(os_environ())
    # 显式指定账号目录：脚本自身的回落顺序（WB2A_AUTHS > 仓库内 auths/ >
    # /root/...）不一定指向本面板管的那批文件，传错会「跑了个寂寞」还看不出来。
    env['WB2A_AUTHS'] = str(config.AUTH_DIR)
    # 上游目录作为 cwd：脚本以 __file__ 自定位，但仍按上游惯例从仓库根运行。
    cwd = str(_script_path().parent.parent)

    proc = None
    try:
        proc = await asyncio.create_subprocess_exec(
            *argv, cwd=cwd, env=env,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.STDOUT,
            stdin=asyncio.subprocess.DEVNULL,
        )
        try:
            await asyncio.wait_for(_pump(proc), timeout=RUN_TIMEOUT_SECONDS)
        except asyncio.TimeoutError:
            _state['timed_out'] = True
            _append(f'!! 超过 {RUN_TIMEOUT_SECONDS}s 未结束，已终止')
            try:
                proc.kill()
            except ProcessLookupError:
                pass
        code = await proc.wait()
        _state['exit_code'] = code
    except FileNotFoundError as exc:
        _state['error'] = f'无法启动脚本：{exc}'
        logger.warning('任务脚本启动失败: %s', exc)
    except Exception as exc:  # noqa: BLE001
        _state['error'] = str(exc)[:300]
        logger.warning('任务执行异常: %s', exc)
    finally:
        if proc is not None and proc.returncode is None:
            try:
                proc.kill()
            except ProcessLookupError:
                pass
        _state['running'] = False
        _state['finished_at'] = int(time.time())
        _record_history(mode, target)


async def _pump(proc: asyncio.subprocess.Process) -> None:
    """逐行读输出（脚本按行打印进度）。"""
    assert proc.stdout is not None
    while True:
        raw = await proc.stdout.readline()
        if not raw:
            break
        line = raw.decode('utf-8', errors='replace').rstrip('\r\n')
        if line:
            _append(line)


def _append(line: str) -> None:
    lines = _state['lines']
    lines.append(line[:400])
    if len(lines) > _MAX_LOG_LINES:
        # 丢最旧的，保留尾部（界面只展示最近的）
        del lines[:len(lines) - _MAX_LOG_LINES]


def _record_history(mode: str, target: str) -> None:
    """把这次执行的结果写进「任务记录」，让它在历史里能查到。

    只在**真正可能产生写入**的模式下记录（claim / full），且只记一行结果 ——
    逐行上报日志交给采集器读容器日志，这里记的是「面板发起的这次操作」。
    """
    if mode not in ('claim', 'full'):
        return
    summary = _summarize()
    label = {'claim': '一键领奖', 'full': '一键做任务'}[mode]
    level = 'ok'
    if _state['error'] or _state['timed_out'] or (_state['exit_code'] not in (0, None)):
        level = 'fail'
    try:
        db.add_task_logs([{
            'ts': _state['finished_at'] or int(time.time()),
            'uid': '' if target == 'ALL' else target,
            'kind': 'taskrun',
            'level': level,
            'credits': 0,
            'message': f'{label}（{target}）{summary}',
            # dedup_key 必须唯一：同一次执行只记一条，用结束时间戳即可
            'dedup_key': f'taskrun:{mode}:{target}:{_state["finished_at"]}',
        }])
    except Exception as exc:  # noqa: BLE001
        logger.warning('写入任务执行记录失败（不影响执行）: %s', exc)


def _summarize() -> str:
    """从脚本输出里挑出**结果汇总**行。

    注意别抓错行：脚本开头会打一行 `mode=DRY-RUN accounts=[...]`，那只是**运行
    参数**（不是结果）；真正的结果在结尾的 `task_runner done: accounts=1 ok=3 …`。
    早先按 `mode=` 匹配，抓到的是参数行 —— 记录进历史的「结果」就变成了
    「mode=DRY-RUN …」，等于没记结果（实测发现）。
    """
    for line in reversed(_state['lines']):
        if 'done:' in line or 'task_runner done' in line:
            return line[:200]
    # 没有结果行（例如启动就失败）：用错误/退出码兜底
    if _state['error']:
        return f'失败：{_state["error"][:140]}'
    if _state['timed_out']:
        return '超时终止'
    code = _state['exit_code']
    return '完成' if code == 0 else f'退出码 {code}'


def os_environ() -> dict:
    """单独抽成函数便于测试替换（测试里不需要真实环境变量）。"""
    import os
    return dict(os.environ)


async def stop() -> bool:
    """终止正在跑的进程（界面上的「停止」）。"""
    global _task
    if not _state['running'] or _task is None or _task.done():
        return False
    _task.cancel()
    _state['running'] = False
    _state['finished_at'] = int(time.time())
    _append('!! 已手动停止')
    return True


# ── 定时领奖（仅 claim 模式）────────────────────────────────
# 只定时跑**幂等领奖**：它不伪造任何行为，只是把账号已完成任务的奖励领回来。
# 「点亮」（full 模式）会产生伪造活跃上报，明确排除在定时之外。

_CLAIM_TICK_SECONDS = 60
_last_claim_day: str = ''
_claim_task: asyncio.Task | None = None


def get_schedule() -> dict:
    """定时领奖的配置（enabled + 整点数组）。"""
    raw = db.get_setting('task_claim_schedule')
    if not isinstance(raw, dict):
        raw = {}
    hours = raw.get('hours')
    if not isinstance(hours, list):
        hours = [10]
    hours = sorted({int(h) for h in hours
                    if isinstance(h, int) and not isinstance(h, bool) and 0 <= h <= 23})
    return {'enabled': bool(raw.get('enabled')), 'hours': hours or [10]}


def set_schedule(enabled: bool, hours: list[int]) -> dict:
    """保存定时领奖配置（时间点做合法性校验）。"""
    if not isinstance(enabled, bool):
        raise ValueError('enabled 必须是布尔值')
    if not isinstance(hours, list) or not hours:
        raise ValueError('hours 必须是非空数组')
    clean: list[int] = []
    for h in hours:
        if isinstance(h, bool) or not isinstance(h, int) or not 0 <= h <= 23:
            raise ValueError('hours 必须是 0-23 的整点')
        if h not in clean:
            clean.append(h)
    cfg = {'enabled': enabled, 'hours': sorted(clean)}
    db.set_setting('task_claim_schedule', cfg)
    return cfg


async def _claim_loop() -> None:
    """到点跑一次 claim（每天每个整点最多一次）。"""
    global _last_claim_day
    while True:
        try:
            cfg = get_schedule()
            if cfg['enabled']:
                now = time.localtime()
                # 本地时区与用户一致（面板按用户所在时区显示时间）
                if now.tm_hour in cfg['hours'] and now.tm_min < 2:
                    stamp = f'{now.tm_year}-{now.tm_mon}-{now.tm_mday}-{now.tm_hour}'
                    if stamp != _last_claim_day:
                        _last_claim_day = stamp
                        if not _state['running']:
                            logger.info('定时领奖触发（%s 点档）', now.tm_hour)
                            start('claim', 'ALL')
        except Exception as exc:  # noqa: BLE001
            logger.warning('定时领奖检查失败: %s', exc)
        await asyncio.sleep(_CLAIM_TICK_SECONDS)


def start_scheduler() -> bool:
    """启动定时领奖检查（幂等）。"""
    global _claim_task
    try:
        loop = asyncio.get_running_loop()
    except RuntimeError:
        return False
    if _claim_task is None or _claim_task.done():
        _claim_task = loop.create_task(_claim_loop())
    return True


def stop_scheduler() -> None:
    global _claim_task
    if _claim_task and not _claim_task.done():
        _claim_task.cancel()
