@echo off
title workbuddy2api
cd /d "%~dp0"

rem ====== 预检：Go 是否安装 ======
where go >nul 2>nul
if errorlevel 1 (
    echo [错误] 未找到 go 命令，请先安装 Go 并加入 PATH。
    goto :pause_end
)

rem ====== 增量编译：go build 自带内容级缓存，源码未变时自动跳过（毫秒级） ======
rem 拉取/合并上游更新后无需手动删 exe 强制重编译：进菜单即自动检测源码变化并重编译。
echo 检查工具二进制（go build 缓存命中时开销可忽略）...
call :build credit.exe ./cmd/credit
call :build login.exe ./cmd/login
call :build signin.exe ./cmd/signin
call :build trial.exe ./cmd/trial
call :build task.exe ./cmd/task
call :build acct.exe ./cmd/acct
call :build stats.exe ./cmd/stats

:menu
cls
echo.
echo  ============================================
echo     workbuddy2api 管理菜单
echo  ============================================
echo     [1] 查询积分 / 查看账号
echo     [2] 手动签到（批量全部账号）
echo     [3] 手动执行定时任务（不影响自动排程）
echo     [4] 启动服务（后台）
echo     [5] 停止服务
echo     [6] 查看服务状态（/status 台账）
echo     [7] 查看服务日志
echo     [8] 加入用户（国内版 cn）
echo     [9] 加入国际版用户（global）
echo     [0] 领取国际版加油包（trial）
echo     [a] 请求统计（/v1/stats 按模型）
echo     [s] 账号运维（停用 / 恢复 / 复活）
echo     [q] 退出
echo  ============================================
echo.
set /p c=  请选择:
if /i "%c%"=="1" goto :query
if /i "%c%"=="2" goto :signin
if /i "%c%"=="3" goto :task
if /i "%c%"=="4" goto :start
if /i "%c%"=="5" goto :stop
if /i "%c%"=="6" goto :status
if /i "%c%"=="7" goto :log
if /i "%c%"=="8" goto :add
if /i "%c%"=="9" goto :add_global
if /i "%c%"=="0" goto :trial
if /i "%c%"=="a" goto :stats
if /i "%c%"=="s" goto :acct
if /i "%c%"=="q" goto :end
goto :menu

:task
cls
echo.
echo  ============================================
echo     手动执行定时任务
echo     （立即跑一次，不影响常驻服务的自动排程；
echo       幂等性由上游/防抖判定兜底，重复跑安全）
echo  ============================================
echo     [1] 令牌保活（按需 refresh 全部账号）
echo     [2] 每日签到
echo     [3] 猫猫旅行巡检
echo     [4] 活跃上报（N 连发 + 领猫联动）
echo     [5] 开学季任务（python）
echo     [6] 夜猫子任务（python）
echo     [7] 全部按顺序跑一遍（含小程序成长，垫底执行）
echo     [8] 小程序成长任务（python）
echo     [q] 返回主菜单
echo  ============================================
echo.
set /p t=  请选择:
if /i "%t%"=="1" (.\task.exe keepalive & goto :task_done)
if /i "%t%"=="2" (.\task.exe checkin & goto :task_done)
if /i "%t%"=="3" (.\task.exe travel & goto :task_done)
if /i "%t%"=="4" (.\task.exe activity & goto :task_done)
if /i "%t%"=="5" (.\task.exe school & goto :task_done)
if /i "%t%"=="6" (.\task.exe cat & goto :task_done)
if /i "%t%"=="7" (.\task.exe all & goto :task_done)
if /i "%t%"=="8" (.\task.exe minichat & goto :task_done)
if /i "%t%"=="q" goto :menu
goto :task

:task_done
echo.
echo  [提示] school/cat/minichat 需要 python：解释器名不是 python3 时先 set WB2A_PYTHON=python
echo  [提示] 服务日志里看不到本次执行（这是独立进程），结果直接打在上方输出中。
echo.
pause
goto :task

:query
echo.
echo  ===== 积分查询 / 账号列表 =====
.\credit.exe -pretty
echo.
pause
goto :menu

:add
echo.
echo  ===== 加入用户（国内版 cn，WorkBuddy OAuth 登录） =====
echo  将自动打开浏览器完成授权，登录后自动落盘到 auths 目录。
echo.
.\login.exe join
call :join_result
goto :menu

:add_global
echo.
echo  ===== 加入国际版用户（global，workbuddy.ai） =====
echo  将自动打开浏览器完成授权；国际版账号无每日签到，登录后自动落盘。
echo.
.\login.exe join --realm=global
call :join_result
goto :menu

:join_result
if errorlevel 2 (
    echo.
    echo  已中止，未添加账号。
) else (
    echo.
    echo  加入完成。服务运行中时 auths 目录热加载（5s 轮询）约 5 秒自动进池，无需重启；
    echo  未配置 auth_dir 的部署需重启加载（选 5 停止，再选 4 启动）。
)
echo.
pause
goto :eof

:signin
echo.
echo  ===== 手动签到（批量全部账号，国际版账号自动判为不适用） =====
echo  遍历 auths\ 下所有 workbuddy-*.json 账号签到。
echo.
.\signin.exe auths
if errorlevel 1 (
    echo.
    echo  [提示] 签到失败或 auths 目录无账号。请先选 8 加入用户。
)
echo.
pause
goto :menu

:trial
echo.
echo  ===== 领取国际版加油包（trial，仅 global 账号） =====
echo  遍历 auths\ 全部账号，CN 账号自动跳过（N/A）。
echo.
.\trial.exe auths
echo.
pause
goto :menu

:stats
echo.
echo  ===== 请求统计（按模型聚合，数据源 /v1/stats，需服务运行中） =====
.\stats.exe
echo.
echo  [提示] 原地刷新: .\stats.exe -watch 5s     按扣费排序: .\stats.exe -sort credits
echo         JSON 透传: .\stats.exe -json
echo.
pause
goto :menu

:acct
rem acct 经网关管理端点操作运行中进程的内存状态（外部直接改 state.json 会被 5s flush
rem 覆盖），故需服务运行中；管理端点还需 config.json 的 admin.enabled=true 显式打开。
curl -s -m 2 http://127.0.0.1:7863/healthz | findstr /i "workbuddy2api" >nul 2>nul
if errorlevel 1 (
    echo  [提示] 服务未运行或未就绪。acct 走网关管理端点，请先选 4 启动服务。
    echo.
    pause
    goto :menu
)
set "ADMIN_ON=no"
for /f "usebackq delims=" %%i in (`powershell -NoProfile -Command "try { $c = ConvertFrom-Json -InputObject (Get-Content -Raw -Encoding UTF8 -LiteralPath 'config.json') } catch { $c = $null }; if ($c -and $c.admin -and $c.admin.enabled) { 'yes' } else { 'no' }"`) do set "ADMIN_ON=%%i"
if "%ADMIN_ON%"=="yes" goto :acct_menu
echo  [提示] config.json 的 admin.enabled 当前未开启（false 或缺省），管理端点不可用。
echo         把 "admin": { "enabled": true } 写入 config.json 后重试。
echo.
pause
goto :menu

:acct_menu
echo.
echo  ============================================
echo     账号运维（停用=对话流量摘除，签到保活照常）
echo  ============================================
echo     [1] 列出账号与状态（acct list）
echo     [2] 临时停用（disable uid [原因]）
echo     [3] 解除手动停用（enable uid）
echo     [4] 解除系统自动禁用（revive uid）
echo     [q] 返回主菜单
echo  ============================================
echo.
set "a="
set /p a=  请选择:
if not defined a goto :acct_menu
if /i "%a%"=="1" (.\acct.exe list & goto :acct_done)
if /i "%a%"=="2" goto :acct_disable
if /i "%a%"=="3" goto :acct_enable
if /i "%a%"=="4" goto :acct_revive
if /i "%a%"=="q" goto :menu
goto :acct_menu

:acct_disable
set "UID="
set /p UID=  请输入 uid（选 1 可先查列表）:
if not defined UID goto :acct_menu
set "REASON="
set /p REASON=  停用原因（可留空）:
.\acct.exe disable "%UID%" "%REASON%"
goto :acct_done

:acct_enable
set "UID="
set /p UID=  请输入 uid:
if not defined UID goto :acct_menu
.\acct.exe enable "%UID%"
goto :acct_done

:acct_revive
set "UID="
set /p UID=  请输入 uid:
if not defined UID goto :acct_menu
.\acct.exe revive "%UID%"
goto :acct_done

:acct_done
echo.
echo  [说明] 手动停用（disable/enable）与系统自动禁用（revive 清除）是两个独立状态位，
echo         enable 不解除自动禁用、revive 不解除手动停用，都清空才回到选号池；
echo         停用状态随池状态落盘，重启保留。
echo.
pause
goto :acct_menu

:status
echo.
echo  ===== 服务状态 =====
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0scripts\status-report.ps1"
echo.
echo  ===== 实时积分余额（credit.exe 直查上游） =====
.\credit.exe -pretty
echo.
pause
goto :menu

:start
echo.
echo  ===== 检查服务状态... =====
set PID=
netstat -ano | findstr ":7863" | findstr "LISTENING" >nul 2>nul
if errorlevel 1 goto :run
for /f "tokens=5" %%p in ('netstat -ano ^| findstr ":7863" ^| findstr "LISTENING"') do set PID=%%p

rem 端口被占用 → 健康检查（上游 /healthz 响应带 service:"workbuddy2api" 身份字段）
curl -s -m 3 http://127.0.0.1:7863/healthz | findstr /i "workbuddy2api" >nul 2>nul
if not errorlevel 1 goto :healthy

rem 端口占用但 healthz 不通 → 查进程归属
set PROCLINE=
for /f "delims=" %%n in ('tasklist /FI "PID eq %PID%" 2^>nul') do set "PROCLINE=%%n"
echo %PROCLINE% | findstr /i "wb2api" >nul 2>nul
if not errorlevel 1 goto :stale

rem 其他程序占用
echo  [状态] 端口 7863 被其他程序占用（PID %PID%），非本项目。
echo  [处理] 不会强行结束别家程序；直接启动可能因端口冲突而失败。
echo.
set /p r=  仍然启动? (y/n):
if /i not "%r%"=="y" goto :menu
goto :run

:healthy
echo  [状态] 服务已在运行且健康（PID %PID%，/healthz 正常）。
echo  [处理] y=强制重启（先停掉旧进程再启动）  其它=返回菜单
echo.
set /p r=  请选择 (y/n):
if /i not "%r%"=="y" goto :menu
echo 正在停止旧服务...
call :stop_service
timeout /t 1 /nobreak >nul 2>nul
goto :run

:stale
echo  [状态] 端口 7863 被占用，但 /healthz 无响应（疑似残留僵死进程，PID %PID%）。
echo  [处理] y=强制清理并重启  其它=返回菜单
echo.
set /p r=  请选择 (y/n):
if /i not "%r%"=="y" goto :menu
echo 正在清理残留进程...
call :stop_service
taskkill /F /PID %PID% >nul 2>nul
timeout /t 1 /nobreak >nul 2>nul
goto :run

:run
rem 前端面板（v1.2.0）：只在源码/依赖比产物（web\out\index.html）新时才重建静态导出，
rem 平时启动零构建开销；无 Node / 无 web 目录时同样跳过，沿用上一次成功构建的产物。
where node >nul 2>nul
if errorlevel 1 (
    echo [提示] 未检测到 Node.js，跳过面板前端构建（使用上次构建产物）。
    goto :skip_panel_build
)
if not exist web\package.json (
    echo [提示] web 目录不存在，跳过面板前端构建。
    goto :skip_panel_build
)
rem PowerShell 时间戳比较：源码任一文件比产物新 → exit 1（需构建）；产物已最新 → exit 0（跳过）。
set PANEL_BUILD=0
powershell -NoProfile -Command "$out=Get-Item 'web\out\index.html' -ErrorAction SilentlyContinue; if(-not $out){exit 1}; $new=Get-ChildItem 'web\app','web\components','web\lib','web\hooks','web\public','web\package.json','web\next.config.ts' -Recurse -File -ErrorAction SilentlyContinue | Where-Object {$_.LastWriteTime -gt $out.LastWriteTime}; if($new){exit 1} else {exit 0}"
if errorlevel 1 set PANEL_BUILD=1
if "%PANEL_BUILD%"=="0" (
    echo [提示] 面板前端产物已是最新，跳过构建。
    goto :skip_panel_build
)
echo 构建面板前端（npm run build:export）...
pushd web
call npm run build:export
popd
if errorlevel 1 (
    rem pushd/popd 不破坏 errorlevel，此处仍可判构建结果。
    echo [Warning] 前端构建失败，继续使用上次成功构建的产物。
    goto :skip_panel_build
)
rem 构建成功 → 同步产物到 internal\panel\dist（go:embed 的嵌入源，漏拷则编译进 exe 的仍是旧面板）。
rem robocopy 退出码 0/1 算成功，>=8 才是失败（2-7 是“有文件拷贝/跳过”的正常组合）。
robocopy web\out internal\panel\dist /MIR /NFL /NDL /NJH /NJS >nul
if errorlevel 8 echo [Warning] 同步 web\out 到 internal\panel\dist 失败，本次编译可能内嵌旧面板。
:skip_panel_build
rem 编译服务（go build 缓存命中秒级；源码更新后自动生效，无需手动删 exe）
call :build wb2api.exe ./cmd/server
if not exist wb2api.exe (
    echo  [错误] wb2api.exe 不存在且编译失败，无法启动；请检查上方编译错误。
    pause
    goto :menu
)
if not exist logs mkdir logs
echo.
echo  ===== 后台启动服务 =====
echo  日志文件 : logs\server.log
echo.
rem 与 start-workbuddy2api.cmd 对齐：Start-Process 后台拉起 + 带 -config 参数 + 写 wb2api.pid，
rem 两套脚本共享同一 pid 文件（Go 日志走 stderr，重定向到 server.log 即可）。
powershell -NoProfile -Command "try { $p=Start-Process -FilePath '%~dp0wb2api.exe' -ArgumentList '-config','config.json' -WorkingDirectory '%~dp0' -WindowStyle Hidden -RedirectStandardError '%~dp0logs\server.log' -PassThru -ErrorAction Stop } catch { exit 1 }; [IO.File]::WriteAllText('%~dp0wb2api.pid', [string]$p.Id); exit 0"
if errorlevel 1 (
    echo  [错误] 服务进程启动失败，请选 7 查看日志。
    echo.
    pause
    goto :menu
)
echo  服务已后台启动，正在等待端口 7863 就绪...
rem 最多等 10 秒，轮询 healthz（匹配 service 身份字段，注意服务无账号时 /healthz 返回 503 也算就绪）
set /a n=0
:waithealth
curl -s -m 2 http://127.0.0.1:7863/healthz | findstr /i "workbuddy2api" >nul 2>nul
if not errorlevel 1 goto :health_ok
set /a n+=1
if %n% geq 10 goto :health_timeout
timeout /t 1 /nobreak >nul 2>nul
goto :waithealth
:health_ok
echo  [成功] 服务已就绪，可正常使用。
goto :run_done
:health_timeout
echo  [警告] 端口未在 10 秒内就绪，请选 7 查看日志确认。
:run_done
echo.
echo  服务在后台运行，窗口可继续操作。
echo  查看日志选 7，停止服务选 5。
echo.
pause
goto :menu


:stop
echo.
echo  ===== 停止服务 =====
call :stop_service
echo.
pause
goto :menu

rem 停止 wb2api：优先按 wb2api.pid 精确停止（与 start-workbuddy2api.cmd 共享同一 pid 文件，
rem 校验映像名以 wb2api 开头防 PID 复用误杀）；pid 缺失/失效时回退按映像名通配 wb2api*
rem（覆盖改名发行 exe），最后按端口 7863 兜底。成功置 STOPPED=1，未发现则保持 0。
:stop_service
set STOPPED=0
set STOP_PID=
if exist wb2api.pid set /p STOP_PID=<wb2api.pid
set STOP_PID_NUM=
if defined STOP_PID set /a STOP_PID_NUM=STOP_PID 2>nul
if not "%STOP_PID_NUM%"=="%STOP_PID%" set STOP_PID=
if defined STOP_PID (
    tasklist /FI "PID eq %STOP_PID%" 2>nul | findstr /i "wb2api" >nul 2>nul
    if not errorlevel 1 (
        taskkill /PID %STOP_PID% /T /F >nul 2>nul
        if not errorlevel 1 set STOPPED=1
    )
    del /q wb2api.pid >nul 2>nul
)
if "%STOPPED%"=="1" goto :stop_service_done
taskkill /F /IM "wb2api*" >nul 2>nul
if not errorlevel 1 (
    set STOPPED=1
    goto :stop_service_done
)
rem 按映像名没杀到 → 端口 7863 仍有 LISTENING 则按端口兜底。
set PORT_PID=
for /f "tokens=5" %%p in ('netstat -ano ^| findstr ":7863" ^| findstr "LISTENING" 2^>nul') do set PORT_PID=%%p
if not defined PORT_PID goto :stop_service_done
taskkill /F /PID %PORT_PID% /T >nul 2>nul
if not errorlevel 1 set STOPPED=1
:stop_service_done
if "%STOPPED%"=="1" (
    echo  服务已停止。
) else (
    echo  未检测到运行中的服务（wb2api*.exe）。
)
goto :eof

:log
echo.
echo  ===== 服务日志 (最后 200 行，完整见 logs\server.log) =====
if not exist "%~dp0logs\server.log" (
    echo  尚无日志文件（服务可能未启动过）。
) else (
    rem type 在 GBK 代码页控制台会把 UTF-8 日志解成乱码；走 PowerShell 显式 UTF-8 读，
    rem 其输出经 Unicode 控制台 API 渲染，任何代码页下都正确。
    powershell -NoProfile -Command "Get-Content -LiteralPath '%~dp0logs\server.log' -Encoding UTF8 -Tail 200"
)
echo.
pause
goto :menu

:build
rem %~1=目标 exe  %~2=包路径。go build 缓存命中时无实质重编译；
rem 编译失败不会破坏旧产物（go build 失败时不写出目标文件），由调用方继续用旧版本。
rem wb2api.exe（./cmd/server）必须带 embed_panel 标签内嵌前端面板；
rem 其它工具 exe 不吃该标签（无 embed 声明，加了也无害，统一省事）。
set "GOTAGS="
if "%~1"=="wb2api.exe" set "GOTAGS=-tags embed_panel"
go build %GOTAGS% -o %~1 %~2
if errorlevel 1 echo [Warning] %~1 编译失败，若已存在则继续使用旧版本。
goto :eof

:pause_end
pause
goto :end

:end
exit /b 0
