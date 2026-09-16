@echo off
REM WorkBuddy Manager —— 双击即可启动（Windows）
REM 前台运行，关掉窗口即停止服务。
cd /d "%~dp0"
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0start.ps1"
pause
