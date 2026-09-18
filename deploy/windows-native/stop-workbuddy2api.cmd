@echo off
REM WorkBuddy Manager —— 上游 workbuddy2api 的**原生**停止脚本模板（Windows）
REM
REM 与 start-workbuddy2api.cmd 配套（说明见那边）。停止方式是按进程名结束：
REM 上游是单个 Go 进程，没有服务注册，所以用 taskkill 即可。
REM
REM 注意用 /IM wb2api.exe 按**镜像名**匹配，而不是按 PID 文件：
REM 原生模式下上游可能被手动重启过，PID 文件会过期；按名字匹配更可靠。
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
