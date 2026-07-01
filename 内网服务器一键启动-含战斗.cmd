@echo off
chcp 65001 >nul
rem ============================================================
rem  Pandora 后端  内网服务器一键启动(含战斗)(双击即用)
rem ------------------------------------------------------------
rem  这台机器当「内网测试服 + 战斗版」:基础设施 + 后端 go 服务跑在本机,
rem  匹配成局后本机自动拉起 Windows DS;并自动把 Hub/Battle DS 返回给客户端
rem  的地址改成本机内网 IP,局域网内其它策划客户端可连进真实大厅 + 战斗。
rem
rem  前置(脚本会自检并提示):Go + Docker + mkcert + 一个 UE 打好的
rem  Windows Server 包(PandoraServer.exe,放到与本仓库平级的 Client 目录,自动探测)。
rem  策划机不用装 Docker/Go,只要能连内网并信任同一套 dev CA(mkcert 根证书)。
rem
rem  停止请用命令:pwsh tools\scripts\play.ps1 -Battle -Stop
rem ============================================================
setlocal
cd /d "%~dp0"

where pwsh >nul 2>nul && (set "PS=pwsh") || (set "PS=powershell")

%PS% -NoProfile -ExecutionPolicy Bypass -File "%~dp0tools\scripts\play.ps1" -Battle -Intranet
set "RC=%ERRORLEVEL%"

pause
exit /b %RC%
