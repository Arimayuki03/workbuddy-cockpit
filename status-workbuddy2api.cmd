@echo off
setlocal
cd /d "%~dp0"

rem Report WorkBuddy2API status. The old version trusted only wb2api.pid plus a
rem path-equality check against "%~dp0wb2api.exe", so a renamed release exe
rem (wb2api-v1.2.0-windows-amd64.exe) or a double-clicked instance with no pid
rem file reported "not running" while the service was actually up (same blind
rem spot as the stop bug). Detection tiers now mirror stop-workbuddy2api.cmd:
rem pid file -> image-name prefix "wb2api" -> port listener from config.json
rem listen (default 7863). Exit 0 = running and /healthz healthy, 1 otherwise.

powershell -NoProfile -ExecutionPolicy Bypass -Command "$ErrorActionPreference='SilentlyContinue'; $found=$null; $why=''; $pf=Join-Path $PWD 'wb2api.pid'; if(Test-Path $pf){ $t=((Get-Content $pf -Raw) -replace '[^0-9]',''); if($t){ $p=Get-Process -Id ([int]$t); if($p -and $p.ProcessName -match '^wb2api'){ $found=$p; $why='pid-file' } } }; if(-not $found){ $ps=@(Get-Process | Where-Object { $_.ProcessName -match '^wb2api' }); if($ps.Count -gt 1){ Write-Host ('note: '+$ps.Count+' wb2api processes running; checking the first') }; if($ps.Count -gt 0){ $found=$ps[0]; $why='image-name' } }; $port=7863; try { $c=Get-Content (Join-Path $PWD 'config.json') -Raw | ConvertFrom-Json; if($c.listen){ $lp=([string]$c.listen).Split(':')[-1]; if($lp -match '^\d+$'){ $port=[int]$lp } } } catch { }; if(-not $found){ $conns=@(Get-NetTCPConnection -LocalPort $port -State Listen); foreach($cn in $conns){ $p=Get-Process -Id ([int]$cn.OwningProcess); if($p -and $p.ProcessName -match '^wb2api'){ $found=$p; $why=('port-'+$port); break } } }; if(-not $found){ Write-Host ('WorkBuddy2API is not running (no pid file, no wb2api* process, nothing listening on port '+$port+')'); exit 1 }; Write-Host ('WorkBuddy2API is running. PID='+$found.Id+' image='+$found.ProcessName+' detected-via='+$why); exit 0"
set "RC=%ERRORLEVEL%"
if not "%RC%"=="0" exit /b 1

rem Health probe only meaningful for the local default port; keep the old
rem WB2API_HEALTH_URL override contract.
if "%WB2API_HEALTH_URL%"=="" set "WB2API_HEALTH_URL=http://127.0.0.1:7863/healthz"
curl.exe --fail-with-body --silent --show-error --max-time 5 "%WB2API_HEALTH_URL%"
set "WB2API_STATUS=%ERRORLEVEL%"
echo.
exit /b %WB2API_STATUS%
