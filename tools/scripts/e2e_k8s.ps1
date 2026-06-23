# Pandora 本地「线上等价」真 DS 闭环一键编排(minikube + Agones,无 mock)
#
# 串起 -Mode k8s 之后剩余的手工步骤,让 codex 一条命令把真 DS 链路准备好:
#   1) 校验 minikube / Agones / Fleet / 后端 allocator 就绪
#   2) 把两个真 UE Linux DS 镜像(精确 tag,从 Fleet yaml 动态解析)load 进 minikube
#   3) 起宿主 Envoy 桥接(kubectl port-forward 所有 envoy upstream + docker envoy :8443/:8444)
#   4) 等 pandora-battle / pandora-hub Fleet Ready
#   5) (可选)起容器版 UDP 回程中继(--network <minikube profile>;docker driver 下客户端连 DS 必需)
#   6) 打印端到端验收清单(用真 UE 客户端验:登录→hub→战斗→结算回 hub)
#
# 前置(由 start.ps1 -Mode k8s 完成):minikube 起、Agones 装好、RBAC/Fleet apply、16 个后端服务部署。
#   pwsh tools/scripts/start.ps1 -Mode k8s
#   pwsh tools/scripts/e2e_k8s.ps1
#
# 用法:
#   pwsh tools/scripts/e2e_k8s.ps1                 # 校验 + load 镜像 + 等 Fleet + 起中继(后台)
#   pwsh tools/scripts/e2e_k8s.ps1 -NoRelay        # 不自动起容器版中继(自己按提示起 pandora/udp-relay:dev 容器)
#   pwsh tools/scripts/e2e_k8s.ps1 -SkipImageLoad  # 镜像已 load 过,跳过
#   pwsh tools/scripts/e2e_k8s.ps1 -TimeoutSec 300 # 等 Fleet Ready 的超时(默认 240s)
#   pwsh tools/scripts/e2e_k8s.ps1 -MinikubeProfile pandora-agones # 指定 minikube profile / docker network
#   pwsh tools/scripts/e2e_k8s.ps1 -BridgeForce    # 500xx 端口被本地/compose 旧服务占用时,杀掉后重建 port-forward

[CmdletBinding()]
param(
    [switch]$NoRelay,        # 不自动起容器版 UDP 中继
    [switch]$SkipImageLoad,  # 跳过 minikube image load
    [switch]$BridgeForce,    # 端口被非 bridge 进程占用时,杀掉占用者后重建 port-forward
    [Alias('Profile')]
    [string]$MinikubeProfile = 'pandora-agones', # minikube profile;docker driver 下通常同名 docker network
    [int]$TimeoutSec = 240   # 等 Fleet Ready 超时秒
)

$ErrorActionPreference = 'Stop'
$ScriptDir   = $PSScriptRoot
$ProjectRoot = (Resolve-Path "$ScriptDir/../..").Path
$AgonesDir   = Join-Path $ProjectRoot 'deploy/k8s/agones'
$K8sNamespace = 'pandora'   # 后端服务 + allocator 所在 ns
$FleetNamespace = 'default' # Fleet / GameServer 所在 ns(见 20/30-fleet-*.yaml)

function Write-Info($m) { Write-Host "[INFO] $m" -ForegroundColor Cyan }
function Write-Ok($m)   { Write-Host "[ OK ] $m" -ForegroundColor Green }
function Write-Warn($m) { Write-Host "[WARN] $m" -ForegroundColor Yellow }
function Write-Err($m)  { Write-Host "[ERR ] $m" -ForegroundColor Red }
function Write-Step($m) { Write-Host "`n===== $m =====" -ForegroundColor Magenta }

function Test-CommandExists([string]$c) { return [bool](Get-Command $c -ErrorAction SilentlyContinue) }

function Resolve-MinikubeProfile([string]$profile) {
    if (-not [string]::IsNullOrWhiteSpace($profile)) { return $profile.Trim() }

    $ctx = (kubectl config current-context 2>$null | Out-String).Trim()
    if (-not [string]::IsNullOrWhiteSpace($ctx)) { return $ctx }

    return 'pandora-agones'
}

function ConvertTo-IPv4UInt32([string]$ip) {
    $parts = $ip.Split('.')
    if ($parts.Count -ne 4) { throw "不是 IPv4 地址: $ip" }
    $value = [uint32]0
    foreach ($part in $parts) {
        $octet = [int]$part
        if ($octet -lt 0 -or $octet -gt 255) { throw "不是 IPv4 地址: $ip" }
        $value = ($value -shl 8) -bor [uint32]$octet
    }
    return $value
}

function Test-IPv4InCidr([string]$ip, [string]$cidr) {
    $cidrParts = $cidr.Split('/')
    if ($cidrParts.Count -ne 2) { return $false }

    $maskBits = [int]$cidrParts[1]
    if ($maskBits -lt 0 -or $maskBits -gt 32) { return $false }

    $ipValue = ConvertTo-IPv4UInt32 $ip
    $networkValue = ConvertTo-IPv4UInt32 $cidrParts[0]
    if ($maskBits -eq 0) { return $true }

    $mask = ([uint32]::MaxValue) -shl (32 - $maskBits)
    return (($ipValue -band $mask) -eq ($networkValue -band $mask))
}

# 从 Fleet yaml 里解析 image:(精确 tag,避免脚本写死过时)
function Get-FleetImage([string]$yamlRelPath) {
    $path = Join-Path $ProjectRoot $yamlRelPath
    if (-not (Test-Path $path)) { throw "找不到 Fleet 文件: $yamlRelPath" }
    $line = Select-String -Path $path -Pattern '^\s*image:\s*(\S+)' | Select-Object -First 1
    if (-not $line) { throw "未在 $yamlRelPath 解析到 image:" }
    return $line.Matches[0].Groups[1].Value
}

function Start-K8sEnvoyBridge {
    $bridgeScript = Join-Path $ScriptDir 'k8s_envoy_bridge.ps1'
    if (-not (Test-Path $bridgeScript)) { throw "缺少桥接脚本: $bridgeScript" }
    if ($BridgeForce) { & $bridgeScript -Force } else { & $bridgeScript }
    if ($LASTEXITCODE -ne 0) { throw "k8s Envoy 桥接启动失败" }
}

Write-Host ""
Write-Host "============================================" -ForegroundColor Magenta
Write-Host " Pandora 真 DS 闭环编排 (minikube + Agones)" -ForegroundColor Magenta
Write-Host "============================================" -ForegroundColor Magenta

# ── 0) 工具与集群就绪校验 ───────────────────────────────────────────────
Write-Step "[0/6] 校验 minikube / kubectl / Agones / 后端就绪"
foreach ($c in @('kubectl', 'minikube', 'docker')) {
    if (-not (Test-CommandExists $c)) { Write-Err "$c 未安装。先跑 start.ps1 -Mode k8s -Install。"; exit 1 }
}
$MinikubeProfile = Resolve-MinikubeProfile $MinikubeProfile
$MinikubeDockerNetwork = $MinikubeProfile
Write-Info "minikube profile: $MinikubeProfile"
minikube -p $MinikubeProfile status *> $null
if ($LASTEXITCODE -ne 0) { Write-Err "minikube profile '$MinikubeProfile' 未在运行。先跑:pwsh tools/scripts/start.ps1 -Mode k8s"; exit 1 }
Write-Ok "minikube profile '$MinikubeProfile' 运行中"

$currentContext = (kubectl config current-context 2>$null | Out-String).Trim()
if ($currentContext -ne $MinikubeProfile) {
    Write-Warn "当前 kubectl context='$currentContext',切换到 minikube profile '$MinikubeProfile'"
    kubectl config use-context $MinikubeProfile *> $null
    if ($LASTEXITCODE -ne 0) { Write-Err "无法切换 kubectl context 到 '$MinikubeProfile'"; exit 1 }
}

kubectl get ns agones-system *> $null
if ($LASTEXITCODE -ne 0) { Write-Err "未检测到 Agones(agones-system)。先跑:pwsh tools/scripts/start.ps1 -Mode k8s"; exit 1 }
Write-Ok "Agones 已安装"

# Fleet 在不在(start.ps1 -Mode k8s 已 apply)
kubectl get fleet pandora-battle -n $FleetNamespace *> $null
$battleFleetOk = ($LASTEXITCODE -eq 0)
kubectl get fleet pandora-hub -n $FleetNamespace *> $null
$hubFleetOk = ($LASTEXITCODE -eq 0)
if (-not $battleFleetOk -or -not $hubFleetOk) {
    Write-Warn "Fleet 不全(battle=$battleFleetOk hub=$hubFleetOk),尝试 apply..."
    kubectl apply -f (Join-Path $AgonesDir '10-rbac-allocator.yaml')
    kubectl apply -f (Join-Path $AgonesDir '20-fleet-battle.yaml')
    kubectl apply -f (Join-Path $AgonesDir '30-fleet-hub.yaml')
}

# allocator 部署在不在 + 是否 agones 模式(读 configmap)
kubectl get deploy ds-allocator hub-allocator -n $K8sNamespace *> $null
if ($LASTEXITCODE -ne 0) {
    Write-Warn "ds-allocator / hub-allocator 未部署。先跑:pwsh tools/scripts/start.ps1 -Mode k8s"
}

# ── 1) load 两个真 UE Linux DS 镜像(精确 tag) ──────────────────────────
$battleImg = Get-FleetImage 'deploy/k8s/agones/20-fleet-battle.yaml'
$hubImg    = Get-FleetImage 'deploy/k8s/agones/30-fleet-hub.yaml'
Write-Step "[1/6] load 真 DS 镜像进 minikube"
Write-Info "battle: $battleImg"
Write-Info "hub:    $hubImg"

if ($SkipImageLoad) {
    Write-Warn "已传 -SkipImageLoad,跳过 image load"
} else {
    foreach ($img in @($battleImg, $hubImg)) {
        # 校验宿主 docker 里有该镜像(否则 minikube image load 会失败)
        docker image inspect $img *> $null
        if ($LASTEXITCODE -ne 0) {
            Write-Err "宿主 docker 没有镜像 $img"
            Write-Warn "  该镜像由 UE 侧 Linux DS 包构建(deploy/ds/build-image.sh)。"
            Write-Warn "  构建后重跑;或若 tag 不同,改 Fleet yaml 的 image: 再重试。"
            exit 1
        }
        Write-Info "  minikube -p $MinikubeProfile image load $img ..."
        minikube -p $MinikubeProfile image load $img
        if ($LASTEXITCODE -ne 0) { Write-Err "load 失败: $img"; exit 1 }
    }
    Write-Ok "两个 DS 镜像已 load"
}

# ── 2) 起宿主 Envoy 桥接 ────────────────────────────────────────────────
Write-Step "[2/6] 起宿主 Envoy 桥接(k8s Service -> host port-forward -> Envoy)"
Start-K8sEnvoyBridge

# ── 3) 等 Fleet Ready ──────────────────────────────────────────────────
Write-Step "[3/6] 等 Fleet Ready(超时 ${TimeoutSec}s)"
function Wait-FleetReady([string]$fleet, [int]$timeoutSec) {
    $deadline = (Get-Date).AddSeconds($timeoutSec)
    while ((Get-Date) -lt $deadline) {
        $ready = (kubectl get fleet $fleet -n $FleetNamespace -o jsonpath='{.status.readyReplicas}' 2>$null)
        if ([string]::IsNullOrWhiteSpace($ready)) { $ready = '0' }
        if ([int]$ready -ge 1) { Write-Ok "$fleet Ready=$ready"; return $true }
        Write-Host "  $fleet readyReplicas=$ready ..." -ForegroundColor DarkGray
        Start-Sleep -Seconds 5
    }
    Write-Err "$fleet 在 ${timeoutSec}s 内未就绪"
    kubectl get gameservers -n $FleetNamespace -L agones.dev/fleet 2>$null
    Write-Warn "排查:kubectl describe fleet $fleet -n $FleetNamespace;kubectl logs <gs-pod> -n $FleetNamespace -c $fleet-ds"
    return $false
}
$bOk = Wait-FleetReady 'pandora-battle' $TimeoutSec
$hOk = Wait-FleetReady 'pandora-hub' $TimeoutSec
if (-not $bOk -or -not $hOk) {
    Write-Err "Fleet 未全就绪,中止(DS 镜像未进 Ready 多半是 Agones SDK 未调通,看上面排查指引)。"
    exit 1
}

# ── 4) UDP 回程中继(docker driver 必需:容器版,挂 minikube profile 对应网络) ──────────
# 为什么必须用容器版而不是 Windows 进程版:
#   Windows + Docker Desktop + minikube docker driver 下,minikube 节点是跑在 Docker Desktop
#   Linux VM 里的容器,IP 在 profile 对应 docker 网络(如 pandora-agones / 192.168.58.0/24)。Windows 宿主进程的
#   UDP 直发 192.168.49.2:<port> 不可路由 —— relay 收到包但 minikube 节点 hostPort DNAT 不增长,
#   DS 收不到连接。把 relay 跑成容器并 `--network <profile>`,它才能直连 minikube 节点 IP;再用
#   `-p 127.0.0.1:7000-8000:.../udp` 让 Docker Desktop 把宿主 127.0.0.1 的 UDP 转进容器。
#     client -> 127.0.0.1:<port>(Win) -[Docker Desktop]-> relay 容器 -[minikube net]-> <minikube ip>:<port> -> DS
Write-Step "[4/6] UDP 回程中继(dockerized,--network $MinikubeDockerNetwork)"

function Stop-HostUdpRelay {
    # 杀掉旧的 Windows 进程版中继(udp_relay.ps1 / go run tools/udp-relay),避免占 127.0.0.1:7000-8000
    $procs = Get-CimInstance Win32_Process -ErrorAction SilentlyContinue |
        Where-Object { $_.CommandLine -and ($_.CommandLine -match 'udp_relay\.ps1' -or $_.CommandLine -match 'udp-relay') }
    foreach ($p in $procs) {
        # 别误杀本脚本自己/编辑器
        if ($p.ProcessId -eq $PID) { continue }
        Write-Warn "  停掉旧 Windows 进程版中继 PID=$($p.ProcessId)"
        Stop-Process -Id $p.ProcessId -Force -ErrorAction SilentlyContinue
    }
}

if ($NoRelay) {
    Write-Warn "已传 -NoRelay,未自动起中继。需手动起【容器版】中继(挂 $MinikubeDockerNetwork 网络):"
    Write-Warn "  docker build -t pandora/udp-relay:dev tools/udp-relay"
    Write-Warn "  docker run -d --name pandora-udp-relay --network $MinikubeDockerNetwork -p 127.0.0.1:7000-8000:7000-8000/udp -e TARGET_HOST=`$(minikube -p $MinikubeProfile ip) -e PORT_RANGE=7000-8000 pandora/udp-relay:dev"
} else {
    $relayImage = 'pandora/udp-relay:dev'
    $relayName  = 'pandora-udp-relay'
    $relayRange = '7000-8000'
    $relayDir   = Join-Path $ProjectRoot 'tools/udp-relay'

    # 4.1 清理:旧 Windows 进程版中继 + 旧容器(都是 127.0.0.1:7000-8000 端口冲突来源)
    Stop-HostUdpRelay
    docker rm -f $relayName *> $null

    # 4.2 解析 minikube 节点 IP 作为转发目标(容器在 profile 对应 docker 网络内可达)
    $relayTarget = (& minikube -p $MinikubeProfile ip 2>$null | Out-String).Trim()
    if ([string]::IsNullOrWhiteSpace($relayTarget)) {
        Write-Err "无法解析 minikube -p $MinikubeProfile ip,容器版中继无法确定 TARGET_HOST。先确认 minikube profile 在跑。"
        exit 1
    }
    Write-Info "  TARGET_HOST(minikube 节点)= $relayTarget"

    # 4.3 确认 profile 对应 docker 网络存在,且 target IP 落在该网络 subnet 内
    $networkJson = (docker network inspect $MinikubeDockerNetwork 2>$null | Out-String)
    if ($LASTEXITCODE -ne 0) {
        Write-Err "未找到 docker 网络 '$MinikubeDockerNetwork' —— 当前 minikube profile '$MinikubeProfile' 可能不是 docker driver,或 Docker 里残留/切到了其它 profile。容器版中继不适用。"
        exit 1
    }
    $network = ($networkJson | ConvertFrom-Json | Select-Object -First 1)
    $subnets = @($network.IPAM.Config | ForEach-Object { $_.Subnet } | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
    $ipv4Subnets = @($subnets | Where-Object { $_ -match '^\d+\.\d+\.\d+\.\d+/\d+$' })
    if ($subnets.Count -eq 0) {
        Write-Warn "docker 网络 '$MinikubeDockerNetwork' 没有可校验的 IPAM subnet,跳过 TARGET_HOST/subnet 校验。"
    } elseif ($ipv4Subnets.Count -eq 0) {
        Write-Warn "docker 网络 '$MinikubeDockerNetwork' 没有可校验的 IPv4 subnet,跳过 TARGET_HOST/subnet 校验。原始 subnet: $($subnets -join ', ')"
    } else {
        $targetInNetwork = $false
        foreach ($subnet in $ipv4Subnets) {
            if (Test-IPv4InCidr $relayTarget $subnet) { $targetInNetwork = $true; break }
        }
        if (-not $targetInNetwork) {
            Write-Err "TARGET_HOST $relayTarget 不属于 docker 网络 '$MinikubeDockerNetwork' 的 IPv4 subnet: $($ipv4Subnets -join ', ')。"
            Write-Warn "这通常表示 relay 将挂到错误 Docker 网络,客户端会登录成功但 UDP 进不了 Hub DS。"
            Write-Warn "请确认 minikube profile '$MinikubeProfile' 与 docker network '$MinikubeDockerNetwork' 匹配,必要时清理旧 minikube network 后重启。"
            exit 1
        }
        Write-Ok "docker 网络 '$MinikubeDockerNetwork' subnet 校验通过: TARGET_HOST $relayTarget ∈ $($ipv4Subnets -join ', ')"
    }

    # 4.4 构建中继镜像(纯标准库,很快)
    Write-Info "  docker build $relayImage ..."
    docker build -t $relayImage $relayDir
    if ($LASTEXITCODE -ne 0) { Write-Err "中继镜像构建失败"; exit 1 }

    # 4.5 起容器:挂 profile 对应 docker 网络 + 发布宿主 127.0.0.1:7000-8000/udp
    Write-Info "  docker run --network $MinikubeDockerNetwork -p 127.0.0.1:${relayRange}:${relayRange}/udp(发布 1001 个 UDP 端口,稍慢)..."
    docker run -d --name $relayName --network $MinikubeDockerNetwork `
        -p "127.0.0.1:${relayRange}:${relayRange}/udp" `
        -e "TARGET_HOST=$relayTarget" `
        -e "PORT_RANGE=$relayRange" `
        $relayImage *> $null
    if ($LASTEXITCODE -ne 0) {
        Write-Err "中继容器启动失败(端口被占用 / docker 网络 '$MinikubeDockerNetwork' 不存在)。日志:"
        docker logs $relayName 2>$null
        exit 1
    }
    Start-Sleep -Seconds 2
    $relayRunning = (docker inspect -f '{{.State.Running}}' $relayName 2>$null)
    if ($relayRunning -ne 'true') {
        Write-Err "中继容器未处于 Running。日志:"
        docker logs $relayName 2>$null
        exit 1
    }
    Write-Ok "UDP 中继(dockerized)已启动:容器 $relayName,--network $MinikubeDockerNetwork,转发 127.0.0.1:$relayRange/udp -> ${relayTarget}:$relayRange"
    Write-Info "  停止:docker rm -f $relayName    查看:docker logs -f $relayName"
    Write-Info "  验证:发 UDP 到 127.0.0.1:<port> 后,minikube ssh 里 'sudo iptables -t nat -L -n -v | grep dpt:<port>' 的 DNAT 计数应增长。"
}

# ── 5) 现状打印 ────────────────────────────────────────────────────────
Write-Step "[5/6] 当前 Fleet / GameServer"
kubectl get fleet -n $FleetNamespace
kubectl get gameservers -n $FleetNamespace -L agones.dev/fleet,pandora.dev/region

# ── 6) 端到端验收清单 ──────────────────────────────────────────────────
Write-Step "[6/6] 真 DS 闭环验收(用真 UE 客户端)"
Write-Host @"
  后端入口(Envoy):客户端面 127.0.0.1:8443 / DS 面 127.0.0.1:8444
  确认客户端 DefaultGame.ini 后端指向本机 Envoy,然后:

  [ ] 登录 -> AssignHub 返回真 hub-ds 地址 -> ClientTravel 进大厅地图(MainCity)
  [ ] 匹配 ConfirmMatch -> matchmaker 调 AllocateBattle -> Agones 分配真 battle-ds -> 进战斗地图(MobaLevel)
  [ ] 打完 -> DS 上报 ReportResult 结算 -> ClientTravel 回大厅 hub-ds
  [ ] allocator 日志:allocator_mode=agones / fleet_mode=agones(不是 mock)

  实时观察:
    kubectl get gameservers -n $FleetNamespace -w
    kubectl logs deploy/ds-allocator  -n $K8sNamespace -f
    kubectl logs deploy/hub-allocator -n $K8sNamespace -f
"@ -ForegroundColor Gray

Write-Host ""
Write-Ok "真 DS 闭环环境就绪。按上面清单用真 UE 客户端验证。"
