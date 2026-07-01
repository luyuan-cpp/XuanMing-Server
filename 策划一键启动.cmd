@echo off
rem ============================================================
rem  Pandora 后端  策划一键启动(双击即用)
rem ------------------------------------------------------------
rem  机器上只要装了 Docker Desktop 即可,不需要 Go、不需要会编译。
rem  双击本文件:自动检查 Docker -> 没装引导安装 -> 没跑帮忙拉起
rem            -> 启动整套后端(基础设施 + 15 个 go 服务,全在容器里)。
rem  首次启动会在容器内编译镜像,稍慢;之后复用缓存,秒起。
rem  停止请双击:策划一键停止.cmd
rem ============================================================
setlocal
cd /d "%~dp0"

where pwsh >nul 2>nul && (set "PS=pwsh") || (set "PS=powershell")

%PS% -NoProfile -ExecutionPolicy Bypass -File "%~dp0tools\scripts\play.ps1"
set "RC=%ERRORLEVEL%"

pause
exit /b %RC%
