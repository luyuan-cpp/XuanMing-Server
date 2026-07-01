@echo off
rem ============================================================
rem  Pandora 后端  内网服务器一键启动(双击即用)
rem ------------------------------------------------------------
rem  全容器模式 + 打印本机内网 IP,供同一局域网内的策划客户端连接。
rem  DS=mock,适合登录/业务联调;真实大厅/战斗 DS 仍走 k8s/Agones 或本地战斗版。
rem
rem  停止请双击:内网服务器一键停止.cmd
rem ============================================================
setlocal
cd /d "%~dp0"

where pwsh >nul 2>nul && (set "PS=pwsh") || (set "PS=powershell")

%PS% -NoProfile -ExecutionPolicy Bypass -File "%~dp0tools\scripts\play.ps1" -Intranet
set "RC=%ERRORLEVEL%"

pause
exit /b %RC%
