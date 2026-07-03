@echo off
chcp 936 >nul
rem ============================================================
rem  Pandora 后端  内网服务器一键启动(含战斗)(双击即用)
rem ------------------------------------------------------------
rem  这台机器当「内网测试服 + 战斗版」:基础设施 + 17 个业务服务跑 docker,
rem  ds/hub allocator 跑宿主 go 进程,匹配成局后由宿主 allocator 拉起 Windows DS;
rem  并自动把 Hub/Battle DS 返回给客户端
rem  的地址改成本机内网 IP,局域网内其它策划客户端可连进真实大厅 + 战斗。
rem
rem  前置(脚本会自检并提示):Go(仅构建 2 个 allocator) + Docker + mkcert + 一个 UE 打好的
rem  Windows Server 包(PandoraServer.exe,放到与本仓库平级的 Client 目录,自动探测)。
rem  策划机不用装 Docker/Go,只要能连内网并信任同一套 dev CA(mkcert 根证书)。
rem
rem  停止请双击:内网服务器一键停止.cmd
rem ============================================================
setlocal
cd /d "%~dp0"

rem 这台机器不改代码,只跑离线包:强制纯离线,直接 docker load 离线镜像,跳过所有 docker build,
rem 不联网、不受 Docker DNS 抖动影响。需要临时构建最新代码时,在开发机做或手动加 -Rebuild。
set "PANDORA_OFFLINE=1"

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

%PS% -NoProfile -ExecutionPolicy Bypass -File "%~dp0tools\scripts\play.ps1" -Battle -Intranet
set "RC=%ERRORLEVEL%"

pause
exit /b %RC%
