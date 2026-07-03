@echo off
chcp 936 >nul
rem ============================================================
rem  Pandora 后端  内网服务器一键停止(双击即用)
rem ------------------------------------------------------------
rem  停止由「内网服务器一键启动-含战斗.cmd」拉起的整套后端(17 业务容器 + 2 个宿主 allocator + 本机 Windows DS)。
rem  数据卷(MySQL/Redis 等)会保留,下次启动数据还在。
rem ============================================================
setlocal
cd /d "%~dp0"

rem 本项目脚本要求 PowerShell 7(pwsh)。缺失则明确报错退出, 不回退 Windows PowerShell 5.1。
where pwsh >nul 2>nul
if errorlevel 1 (
  echo.
  echo  [ERR] 未找到 PowerShell 7 pwsh。本脚本需要 PowerShell 7。
  echo        下载安装: https://aka.ms/powershell  或  winget install Microsoft.PowerShell
  echo.
  pause
  exit /b 1
)
set "PS=pwsh"

%PS% -NoProfile -ExecutionPolicy Bypass -File "%~dp0tools\scripts\play.ps1" -Battle -Stop
set "RC=%ERRORLEVEL%"

pause
exit /b %RC%
