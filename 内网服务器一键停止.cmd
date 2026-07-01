@echo off
chcp 65001 >nul
rem ============================================================
rem  Pandora 后端  内网服务器一键停止(双击即用)
rem ------------------------------------------------------------
rem  停止由「内网服务器一键启动.cmd」拉起的整套后端容器。
rem  数据卷(MySQL/Redis 等)会保留,下次启动数据还在。
rem ============================================================
setlocal
cd /d "%~dp0"

where pwsh >nul 2>nul && (set "PS=pwsh") || (set "PS=powershell")

%PS% -NoProfile -ExecutionPolicy Bypass -File "%~dp0tools\scripts\play.ps1" -Intranet -Stop
set "RC=%ERRORLEVEL%"

pause
exit /b %RC%
