@echo off
setlocal
cd /d "%~dp0"

rem Stop WorkBuddy2API. The old version only accepted a PID whose path equalled
rem exactly "%~dp0wb2api.exe" and exited "not running" whenever wb2api.pid was
rem missing — so a renamed release exe (wb2api-v1.2.0-windows-amd64.exe), a
rem double-clicked instance, or one started from another directory could never
rem be stopped (the reported bug). New logic kills by image-name prefix "wb2api"
rem (covers wb2api.exe and any wb2api-* release name, and can never match an
rem unrelated process), with the pid file as a fast path and a port fallback
rem (port read from config.json listen, default 7863) that re-validates the
rem image name before killing. Exit 0 when stopped or nothing was running.

powershell -NoProfile -ExecutionPolicy Bypass -Command "$ErrorActionPreference='SilentlyContinue'; $killed=$false; $failed=$false; function KillIt([int]$id,[string]$why){ $proc=Get-Process -Id $id; if(-not $proc){ return }; if($proc.ProcessName -notmatch '^wb2api'){ Write-Host ('skipped PID '+$id+': image '+$proc.ProcessName+' is not a wb2api process'); return }; & taskkill /F /T /PID $id | Out-Null; if($LASTEXITCODE -eq 0){ Write-Host ('stopped '+$proc.ProcessName+' (PID '+$id+') via '+$why); $script:killed=$true } else { Write-Host ('taskkill failed for PID '+$id); $script:failed=$true } }; $pf=Join-Path $PWD 'wb2api.pid'; if(Test-Path $pf){ $t=((Get-Content $pf -Raw) -replace '[^0-9]',''); if($t){ KillIt ([int]$t) 'pid-file' }; Remove-Item $pf -Force }; if(-not $killed -and -not $failed){ @(Get-Process | Where-Object { $_.ProcessName -match '^wb2api' }) | ForEach-Object { KillIt $_.Id 'image-name' } }; if(-not $killed -and -not $failed){ $port=7863; try { $c=Get-Content (Join-Path $PWD 'config.json') -Raw | ConvertFrom-Json; if($c.listen){ $lp=([string]$c.listen).Split(':')[-1]; if($lp -match '^\d+$'){ $port=[int]$lp } } } catch { }; $conns=@(Get-NetTCPConnection -LocalPort $port -State Listen); if($conns.Count -eq 0){ Write-Host ('no wb2api process found; nothing listening on port '+$port+' either') } else { foreach($cn in $conns){ KillIt ([int]$cn.OwningProcess) ('port-'+$port) } } }; if($failed){ exit 1 }; exit 0"
set "RC=%ERRORLEVEL%"

if not "%RC%"=="0" echo Failed to stop WorkBuddy2API.
exit /b %RC%
