@echo off
chcp 65001 >nul
rem ============================================================
rem  Pandora 后端 内网服务器一键启动(k8s 集群)
rem  双击运行
rem ------------------------------------------------------------
rem  在本机通过 minikube(docker driver) + Agones 启动真实 Kubernetes
rem  开发集群:基础设施 + 19 个业务服务以 k8s Deployment 运行,Battle DS
rem  走真实 Linux Agones Fleet。
rem
rem  包装命令:
rem    tools/scripts/start.ps1 -Mode k8s -BuildMode host
rem
rem  前置要求:Go / Docker Desktop / kubectl / minikube / helm 已安装且可用。
rem  本脚本默认不安装工具,避免未经授权修改本机环境。若确需安装缺失 CLI,
rem  请人工确认后在 PowerShell 7 中显式运行:
rem    pwsh tools/scripts/start.ps1 -Mode k8s -BuildMode host -Install
rem
rem  镜像构建使用宿主 Go 交叉编译 linux/amd64 静态二进制,再封装成
rem  pandora/<svc>:dev scratch 镜像并加载到 minikube。该路径不是离线
rem  docker load 导入包路径。
rem
rem  UE 战斗/大厅 DS(Linux)镜像会自动构建:脚本从【同级客户端仓库】的
rem  Packages\Server_Linux_Development\LinuxServer 取 UE Linux 打包产物
rem  (不写死路径,优先匹配同级 Pandora-Client* 仓库),同步进 deploy/ds/stage
rem  后构建 pandora/battle-ds:dev / pandora/hub-ds:dev 到 minikube。
rem  DS 起来后看 UE 日志: kubectl get gameservers; kubectl logs -f <pod>。
rem  想手动指定 DS 包路径,先设 环境变量 PANDORA_DS_LINUX_PKG 再双击本脚本。
rem
rem  注意:minikube docker driver 下 Pod IP 默认不能被其它内网机器直接访问;
rem  本入口用于在这台机器上验证真实 k8s + Agones DS 链路。面向内网/生产
rem  客户端的公开集群仍走 online 模式,由人负责 k8s 侧操作。
rem
rem  停止:双击 内网服务器一键停止-k8s集群.cmd
rem ============================================================
setlocal
cd /d "%~dp0"

where pwsh >nul 2>nul
if errorlevel 1 (
  echo.
  echo  [ERR] 未找到 PowerShell 7 pwsh。本项目脚本要求 PowerShell 7。
  echo        安装地址: https://aka.ms/powershell
  echo.
  pause
  exit /b 1
)
set "PS=pwsh"

%PS% -NoProfile -ExecutionPolicy Bypass -File "%~dp0tools\scripts\start.ps1" -Mode k8s -BuildMode host
set "RC=%ERRORLEVEL%"

pause
exit /b %RC%
