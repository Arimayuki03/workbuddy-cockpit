@echo off
REM WorkBuddy Manager —— 上游 workbuddy2api 的**原生**启动脚本模板（Windows）
REM
REM 用途：当上游不以 Docker 运行、而是作为本机进程运行时，管理端需要一对启停脚本
REM （在 .env 里用 WB2API_START_SCRIPT / WB2API_STOP_SCRIPT 指向它们）。
REM
REM **先看上游自带的**：上游 workbuddy2api 2026-09-18 起已自带
REM start/stop/status-workbuddy2api.cmd，功能更全（PID 文件 + 进程路径校验，不会误杀
REM 同名进程；另有 status 脚本会打一次 /healthz）。上游目录里有那三个文件就别用本模板。
REM 本文件是给**旧版上游**（那时官方只提供 Docker 部署）用的。
REM
REM 用法：
REM   1) 复制到你的上游目录，按下面两处 TODO 改成实际路径
REM   2) .env 里设置：
REM        WB2API_MODE=native
REM        WB2API_START_SCRIPT=C:/path/to/workbuddy2api/start-workbuddy2api.cmd
REM        WB2API_STOP_SCRIPT=C:/path/to/workbuddy2api/stop-workbuddy2api.cmd
REM        WB2API_LOG_FILE=C:/path/to/workbuddy2api/data/server.err.log
REM   3) 管理端「设置」页保存后会调用它们重启上游
REM
REM 要求：
REM   · 必须**立即返回**（脚本会等待它结束）：用 start /b 或后台方式拉起，
REM     不要在前台一直运行 —— 前台运行会让「重启」卡住直到超时。
REM   · 上游的 stdout/stderr 建议重定向到 WB2API_LOG_FILE 指向的文件，
REM     管理端的「任务记录」就是从那里读日志的。

setlocal
cd /d "%~dp0"

REM TODO 1：上游可执行文件（由上游源码 `go build -o wb2api.exe ./cmd/server` 得到）
set WB2API_EXE=%~dp0wb2api.exe

REM TODO 2：上游配置文件与数据目录
set WB2API_CONFIG=%~dp0config.json
set WB2API_DATADIR=%~dp0data

if not exist "%WB2API_EXE%" (
    echo [start] 未找到上游可执行文件：%WB2API_EXE% >&2
    echo [start] 请先在上游目录执行：go build -o wb2api.exe ./cmd/server >&2
    exit /b 1
)
if not exist "%WB2API_DATADIR%" mkdir "%WB2API_DATADIR%"

REM `start /b` = 后台启动并**立即返回**；日志重定向到文件供管理端读取
start "workbuddy2api" /b "%WB2API_EXE%" -config "%WB2API_CONFIG%" >> "%WB2API_DATADIR%\server.out.log" 2>> "%WB2API_DATADIR%\server.err.log"

echo [start] 已启动 workbuddy2api
exit /b 0
