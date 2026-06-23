# Pandora 本地 Agones UDP 回程中继(minikube docker-driver 专用)
#
# 用法:
#   pwsh tools/scripts/udp_relay.ps1                       # 自动解析 minikube -p pandora-agones ip 作为转发目标
#   pwsh tools/scripts/udp_relay.ps1 -Profile pandora-agones
#   pwsh tools/scripts/udp_relay.ps1 -TargetHost 192.168.58.2
#   pwsh tools/scripts/udp_relay.ps1 -PortRange 7000-8000
#
# 背景(为什么需要它):
#   minikube 用 docker driver 时,GameServer Pod 的 status.address 是集群内网 IP(10.x / node IP),
#   Windows 客户端直连不到。allocator 侧用 advertise_host=127.0.0.1 把返回地址改写成本机回环,
#   再由本中继把 127.0.0.1:<port> 的 UDP 流量转发到 minikube 节点的同端口(NodePort -> Pod)。
#   端口范围对齐 Agones dynamic port 默认 [7000,8000]。
#
#   client --UDP--> 127.0.0.1:<port>  --[本脚本: tools/udp-relay]-->  <minikube ip>:<port> --> GameServer
#
# 注意:
#   - 仅本地 dev 联调使用,生产/真集群用 GameServer status.address 直连,不需要本中继。
#   - 该工具是纯标准库零依赖程序,直接 go run,不进 go.work。
#   - 监听 7000-8000 共 1001 个 UDP 端口,属本地 dev 行为,资源占用可接受。
#   - 自动解析 TARGET_HOST 必须走当前集群 profile(默认 pandora-agones / 192.168.58.x)。
#     裸 `minikube ip` 会回到默认 minikube profile(192.168.49.x 残留),导致包发错节点、
#     登录成功但 UDP 进不了 Hub DS。所以这里强制用 `minikube -p $Profile ip`。

param(
    [string]$TargetHost = "",
    [string]$PortRange  = "7000-8000",
    [Alias('MinikubeProfile')]
    [string]$Profile = "pandora-agones"
)

$ErrorActionPreference = "Stop"

$ProjectRoot = Resolve-Path "$PSScriptRoot/../.."
$RelayDir    = Join-Path $ProjectRoot "tools/udp-relay"

Write-Host "===== Pandora UDP relay (local Agones) =====" -ForegroundColor Cyan

if (-not (Test-Path (Join-Path $RelayDir "main.go"))) {
    Write-Host "[ERR] relay source not found: $RelayDir/main.go" -ForegroundColor Red
    exit 1
}

# 未显式指定时,自动解析 minikube 节点 IP 作为转发目标(避免硬编码默认值与实际不符)。
# 必须带 -p $Profile,否则裸 `minikube ip` 会回到默认 minikube profile(旧 192.168.49.x 残留),
# relay 会把包发到错误节点 —— 客户端登录成功但 UDP 进不了 Hub DS。
if ([string]::IsNullOrWhiteSpace($TargetHost)) {
    Write-Host "[1/2] Resolving minikube -p $Profile ip..." -ForegroundColor Yellow
    try {
        $TargetHost = (& minikube -p $Profile ip 2>$null | Out-String).Trim()
    } catch {
        $TargetHost = ""
    }
    if ([string]::IsNullOrWhiteSpace($TargetHost)) {
        Write-Host "[ERR] 无法自动解析 minikube -p $Profile ip。请确认 minikube profile '$Profile' 已启动,或用 -TargetHost 显式指定。" -ForegroundColor Red
        exit 1
    }
    Write-Host "      minikube -p $Profile ip = $TargetHost" -ForegroundColor Green
} else {
    Write-Host "[1/2] Using target host: $TargetHost (profile $Profile)" -ForegroundColor Yellow
}

Write-Host "[2/2] Starting relay -> ${TargetHost} ports ${PortRange} (Ctrl+C 停止)" -ForegroundColor Yellow
Write-Host ""

$env:TARGET_HOST = $TargetHost
$env:PORT_RANGE  = $PortRange

Push-Location $RelayDir
try {
    go run main.go
} finally {
    Pop-Location
}
