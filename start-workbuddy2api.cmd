@echo off
setlocal EnableDelayedExpansion
cd /d "%~dp0"

if not exist "wb2api.exe" (
  echo Missing wb2api.exe. Build it with: go build -tags embed_panel -o wb2api.exe ./cmd/server
  exit /b 1
)
if not exist "config.json" (
  echo Missing config.json. Copy config.example.json and configure it first.
  exit /b 1
)

set "WB2API_ROOT=%CD%"
set "WB2API_EXE=%CD%\wb2api.exe"
set "WB2API_PID_FILE=%CD%\wb2api.pid"
if exist "%WB2API_PID_FILE%" (
  set /p WB2API_PID=<"%WB2API_PID_FILE%"
  rem Image-name prefix check (not path equality): a renamed release exe
  rem (wb2api-v1.2.0-windows-amd64.exe) recorded in the pid file is still ours.
  powershell -NoProfile -Command "try { $p=Get-Process -Id ([int]$env:WB2API_PID) -ErrorAction Stop; if ($p.ProcessName -match '^wb2api') { exit 0 } } catch {}; exit 1"
  if not errorlevel 1 (
    echo WorkBuddy2API is already running. PID=!WB2API_PID!
    exit /b 0
  )
  del /q "%WB2API_PID_FILE%" >nul 2>&1
)

if not exist "data" mkdir "data"
powershell -NoProfile -Command "$p=Start-Process -FilePath $env:WB2API_EXE -ArgumentList '-config','config.json' -WorkingDirectory $env:WB2API_ROOT -WindowStyle Hidden -RedirectStandardOutput (Join-Path $env:WB2API_ROOT 'data\server.out.log') -RedirectStandardError (Join-Path $env:WB2API_ROOT 'data\server.err.log') -PassThru; [IO.File]::WriteAllText($env:WB2API_PID_FILE, [string]$p.Id)"
if errorlevel 1 exit /b 1

set /p WB2API_PID=<"%WB2API_PID_FILE%"
echo WorkBuddy2API started. PID=%WB2API_PID%
endlocal
