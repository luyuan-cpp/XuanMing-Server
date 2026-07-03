@echo off
chcp 936 >nul
rem ============================================================
rem  Pandora 后端  策划一键启动-含战斗(双击即用)
rem ------------------------------------------------------------
rem  本地完整战斗版:能进大厅、匹配、进 battle DS 打一局。
rem  后端:17 个业务服务跑 docker,ds/hub allocator 跑宿主 go 进程,
rem  匹配成局后由宿主 allocator 拉起本机的
rem  Windows DS(PandoraServer.exe)进战斗。
rem  (docker 全容器版 DS=mock,跑不进真实战斗 DS,故不再单独提供入口脚本。)
rem
rem  这是「完全本地一键版」:脚本会自动准备环境(缺 Go / Docker 自动
rem  winget 安装)、起基础设施(docker)、起 17 个业务服务容器 + 2 个宿主 allocator、并拉起战斗 DS。
rem
rem  前置条件(脚本会自动检查并清晰提示):
rem    1) Go(仅构建 2 个 allocator)和 Docker Desktop:没装会自动 winget 安装
rem       (装完可能要新开终端重跑一次)。
rem    2) 一个 UE 打好的 Windows Server 包(PandoraServer.exe)。
rem       路径无需手动改:把客户端 Client 仓库放到与本服务器仓库「平级」
rem       的目录(同一父目录),脚本会自动探测 Packages\Server_Win64_*\
rem       WindowsServer\PandoraServer.exe 并注入,换机器、换盘符都不用改配置。
rem
rem  停止请双击:策划一键停止.cmd
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

%PS% -NoProfile -ExecutionPolicy Bypass -File "%~dp0tools\scripts\play.ps1" -Battle
set "RC=%ERRORLEVEL%"

pause
exit /b %RC%
