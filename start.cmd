@echo off
rem ============================================================
rem  Pandora 后端一键启动器(双击即用)
rem ------------------------------------------------------------
rem  双击本文件:默认本机 local 模式(基础设施 docker + go 服务宿主进程),
rem             策划本地联调首选,可在 VS Code 断点调试。
rem
rem  5 套环境(DS 分配模式随环境变):
rem     local    本地 windows 调试    DS=local(宿主 exec Windows DS)
rem     docker   本地全容器           DS=mock
rem     intranet 内网测试服(绑内网IP) DS=mock
rem     k8s      本机 minikube+Agones  DS=agones(真 Linux DS,线上等价)
rem     online   线上 k8s 集群         DS=agones(-Env test 测试服 / prod 生产 kbs)
rem
rem  命令行用法(可传参,转发给 start.ps1):
rem     start.cmd -Mode docker
rem     start.cmd -Mode k8s
rem     start.cmd -Mode local -Profile match
rem     start.cmd -Status
rem     start.cmd -Check
rem     start.cmd -Mode docker -Down
rem
rem  电脑重启后快速恢复 / 一键重置(见 deploy/k8s/agones/README.md):
rem     start.cmd -Mode k8s -Resume      rem 不重建镜像,拉回上次状态
rem     start.cmd -Mode k8s -Reset       rem minikube delete 后全新部署
rem
rem  本机真 DS 闭环(minikube+Agones,无 mock):
rem     start.cmd -Mode k8s              rem 起集群+Agones+Fleet+16 服务
rem     pwsh tools\scripts\e2e_k8s.ps1   rem load DS 镜像+Envoy 桥接+等 Fleet+UDP 中继
rem
rem  线上真集群(Fleet 镜像/回调必须按环境注入,缺参直接 fail-fast):
rem     start.cmd -Mode online -Env test -Registry registry.mycorp.com -Tag v1.2.3 ^
rem        -BattleDsImage registry.mycorp.com/pandora/battle-ds:v1.2.3 ^
rem        -HubDsImage    registry.mycorp.com/pandora/hub-ds:v1.2.3 ^
rem        -DsGatewayAddr pandora-envoy.pandora.svc:8444
rem ============================================================
setlocal
cd /d "%~dp0"

rem 优先 PowerShell 7(pwsh),没有则回退 Windows PowerShell
where pwsh >nul 2>nul && (set "PS=pwsh") || (set "PS=powershell")

%PS% -NoProfile -ExecutionPolicy Bypass -File "%~dp0tools\scripts\start.ps1" %*
set "RC=%ERRORLEVEL%"

rem 双击(无参数)时停住窗口,方便看输出
if "%~1"=="" pause
exit /b %RC%
