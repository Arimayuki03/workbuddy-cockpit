@echo off
REM WorkBuddy Manager —— 上游 workbuddy2api 的**原生**停止脚本模板（Windows）
REM
REM 与 start-workbuddy2api.cmd 配套（说明见那边，包括「优先用上游自带脚本」）。
REM
REM 停止方式是按**进程名**结束：上游是单个 Go 进程，没有服务注册，用 taskkill 即可。
REM 这里刻意按 /IM wb2api.exe 匹配而不是按 PID 文件 —— 本模板没有维护 PID 文件，
REM 而按名字匹配在同名进程只有一个时是可靠的。
REM
REM 已知取舍：如果机器上跑着**两个**同名的 wb2api.exe（例如从不同目录各起一个），
REM 这条会把它们全杀掉。上游自带的 stop 脚本用 PID 文件 + 进程路径双重校验避开了
REM 这个问题 —— 上游目录里有它就用它。
REM 若你的可执行文件名不同，同步改下面这行与 start 脚本里的 WB2API_EXE。

setlocal

REM 上游进程名（与 start 脚本里 WB2API_EXE 的文件名一致）
set WB2API_IMAGE=wb2api.exe

REM 先看有没有在跑（没有也返回 0：停止一个本就没运行的进程不算失败，
REM 否则管理端的「重启」会因为这个前置步骤而整体失败）
tasklist /fi "IMAGENAME eq %WB2API_IMAGE%" 2>nul | find /i "%WB2API_IMAGE%" >nul
if errorlevel 1 (
    echo [stop] %WB2API_IMAGE% 未在运行，跳过
    exit /b 0
)

taskkill /f /im "%WB2API_IMAGE%" >nul 2>&1
if errorlevel 1 (
    echo [stop] 结束 %WB2API_IMAGE% 失败 >&2
    exit /b 1
)

echo [stop] 已停止 %WB2API_IMAGE%
exit /b 0
