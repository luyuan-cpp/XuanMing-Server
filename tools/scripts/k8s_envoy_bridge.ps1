# Pandora 本地 k8s 真 DS 联调的宿主 Envoy 桥接器
#
# 为什么需要它:
#   - k8s 模式里 16 个 Go 服务都跑在 pandora namespace 的 ClusterIP Service 后面
#   - UE 客户端 / Linux DS 仍然只会打宿主机 Envoy(:8443 / :8444)
#   - 现有 deploy/envoy/envoy.yaml 的 upstream 全指向 host.docker.internal:500xx
#
# 所以这里做两件事:
#   1) 对 Envoy 会访问到的每个 k8s Service 起本地 kubectl port-forward(127.0.0.1:500xx)
#   2) 单独拉起 docker compose 里的 envoy 容器,复用现有本地开发配置
#
# 端口占用安全(P1):只有「本 bridge 自己起的 kubectl port-forward svc/<name>」才算就绪复用;
#   若 127.0.0.1:500xx 被别的进程占用(本地 go 服务 / docker-compose 业务服务),会让 Envoy
#   连到旧后端而不是 k8s Service,导致 e2e「假通过」—— 此时默认 fail-fast,
#   或加 -Force 由本脚本杀掉占用者后重建。

[CmdletBinding()]
param(
    [switch]$Force   # 端口被非 bridge 进程占用时,杀掉占用者后重建 port-forward
)

$ErrorActionPreference = 'Stop'
$ScriptDir = $PSScriptRoot
$ProjectRoot = (Resolve-Path "$ScriptDir/../..").Path
$ComposeFile = Join-Path $ProjectRoot 'deploy/docker-compose.dev.yml'
$EnvFile = Join-Path $ProjectRoot 'deploy/env/dev.env'
$StateDir = Join-Path $ProjectRoot 'run/k8s-envoy-bridge'
$K8sNamespace = 'pandora'

function Write-Info($m) { Write-Host "[INFO] $m" -ForegroundColor Cyan }
function Write-Ok($m)   { Write-Host "[ OK ] $m" -ForegroundColor Green }
function Write-Warn($m) { Write-Host "[WARN] $m" -ForegroundColor Yellow }
function Write-Step($m) { Write-Host "`n===== $m =====" -ForegroundColor Magenta }

# Essential = 登录→Hub→匹配→Battle→结算 闭环必需的服务;非必需(社交/拍卖/交易等)
# 即便 Pod 没起来,也不该让整个 bridge / e2e 直接失败(只 WARN 跳过该 port-forward)。
$Forwards = @(
    @{ Name = 'login';          Port = 50001; Essential = $true  }
    @{ Name = 'player';         Port = 50002; Essential = $true  }
    @{ Name = 'data-service';   Port = 50003; Essential = $true  }
    @{ Name = 'friend';         Port = 50004; Essential = $false }
    @{ Name = 'chat';           Port = 50005; Essential = $false }
    @{ Name = 'player-locator'; Port = 50006; Essential = $true  }
    @{ Name = 'team';           Port = 50010; Essential = $true  }
    @{ Name = 'matchmaker';     Port = 50011; Essential = $true  }
    @{ Name = 'trade';          Port = 50012; Essential = $false }
    @{ Name = 'dialogue';       Port = 50013; Essential = $false }
    @{ Name = 'push';           Port = 50014; Essential = $true  }
    @{ Name = 'inventory';      Port = 50015; Essential = $false }
    @{ Name = 'auction';        Port = 50016; Essential = $false }
    @{ Name = 'ds-allocator';   Port = 50020; Essential = $true  }
    @{ Name = 'hub-allocator';  Port = 50021; Essential = $true  }
    @{ Name = 'battle-result';  Port = 50022; Essential = $true  }
)

function Ensure-File([string]$path) {
    if (-not (Test-Path $path)) {
        throw "缺少文件: $path"
    }
}

function Get-PidFile([string]$name) { Join-Path $StateDir "$name.pid" }

# 返回 127.0.0.1:$port LISTEN 的占用进程 PID(无则 $null)
function Get-PortListenerPid([int]$port) {
    try {
        $conn = Get-NetTCPConnection -LocalAddress 127.0.0.1 -LocalPort $port -State Listen -ErrorAction Stop |
            Select-Object -First 1
        return $conn.OwningProcess
    } catch {
        return $null
    }
}

function Get-ProcCommandLine([int]$processId) {
    try {
        return (Get-CimInstance Win32_Process -Filter "ProcessId=$processId" -ErrorAction Stop).CommandLine
    } catch {
        return $null
    }
}

function Get-ProcDesc([int]$processId) {
    $proc = Get-Process -Id $processId -ErrorAction SilentlyContinue
    if ($proc) { return "$($proc.ProcessName) (PID=$processId)" }
    return "PID=$processId"
}

# 占用 $port 的进程是不是「本 bridge 期望的 kubectl port-forward svc/<name> port:port」
function Test-IsBridgePortForward([int]$processId, [string]$name, [int]$port) {
    if (-not $processId) { return $false }
    $proc = Get-Process -Id $processId -ErrorAction SilentlyContinue
    if (-not $proc -or $proc.ProcessName -ne 'kubectl') { return $false }
    $cmd = Get-ProcCommandLine $processId
    if (-not $cmd) { return $false }
    return ($cmd -like '*port-forward*' -and $cmd -like "*svc/$name*" -and $cmd -like "*${port}:${port}*")
}

function Start-PortForward([string]$name, [int]$port, [bool]$essential = $true) {
    $ownerPid = Get-PortListenerPid $port
    if ($ownerPid) {
        if (Test-IsBridgePortForward $ownerPid $name $port) {
            Write-Ok "port-forward 已在(bridge 自身)$name :127.0.0.1:$port (PID=$ownerPid)"
            Set-Content -Path (Get-PidFile $name) -Value $ownerPid -Encoding ASCII
            return
        }

        # 端口被「非 bridge」进程占用 —— 会让 Envoy 连到旧后端,必须处理
        $desc = Get-ProcDesc $ownerPid
        if (-not $Force) {
            throw @"
端口 $port 已被非 bridge 进程占用:$desc
这通常是本地 go 服务(run_services.ps1)或 docker-compose 业务服务还在跑,
会导致 Envoy 连到旧后端而不是 k8s Service —— e2e 可能"假通过"。
请先停掉它们:
  pwsh tools/scripts/start.ps1 -Mode local  -Down
  pwsh tools/scripts/start.ps1 -Mode docker -Down
或给本脚本加 -Force(经 e2e_k8s.ps1 -BridgeForce 透传),让它杀掉占用者后重建。
"@
        }

        Write-Warn "端口 $port 被非 bridge 进程占用:$desc —— -Force 杀掉后重建"
        Stop-Process -Id $ownerPid -Force -ErrorAction SilentlyContinue
        for ($i = 0; $i -lt 10 -and (Get-PortListenerPid $port); $i++) { Start-Sleep -Milliseconds 300 }
        if (Get-PortListenerPid $port) {
            throw "端口 $port 释放失败(占用者 $desc),请手动停掉后重试"
        }
    }

    $log = Join-Path $StateDir "$name.log"
    $err = Join-Path $StateDir "$name.err.log"
    $proc = Start-Process kubectl -PassThru -WindowStyle Hidden -RedirectStandardOutput $log -RedirectStandardError $err -ArgumentList @(
        'port-forward',
        '--namespace', $K8sNamespace,
        '--address', '127.0.0.1',
        "svc/$name",
        "${port}:${port}"
    )
    Set-Content -Path (Get-PidFile $name) -Value $proc.Id -Encoding ASCII

    for ($i = 0; $i -lt 10; $i++) {
        if ($proc.HasExited) {
            $stderr = if (Test-Path $err) { (Get-Content $err -Raw) } else { '' }
            $msg = "port-forward 启动失败 svc/${name}:$port`n$stderr"
            # 后端 Pod 没在 Running(Pending / ImagePullBackOff / CrashLoop)时 kubectl 会立刻退出。
            # 必需服务 → 直接失败;非必需服务 → 只 WARN 跳过,别拖垮整个 bridge / e2e。
            Remove-Item (Get-PidFile $name) -ErrorAction SilentlyContinue
            if ($essential) { throw $msg }
            Write-Warn "非必需服务 $name 不可用,跳过其 port-forward(不影响登录/Hub/匹配/Battle/结算闭环):`n$msg"
            return
        }
        # 确认占用 $port 的就是我们刚起的这个进程(而不是别的进程抢先 LISTEN)
        $nowPid = Get-PortListenerPid $port
        if ($nowPid -eq $proc.Id) {
            Write-Ok "port-forward 就绪 $name :127.0.0.1:$port (PID=$($proc.Id))"
            return
        }
        if ($nowPid -and -not (Test-IsBridgePortForward $nowPid $name $port)) {
            Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
            throw "端口 $port 被其它进程($(Get-ProcDesc $nowPid))抢占,bridge port-forward 未能绑定"
        }
        Start-Sleep -Milliseconds 500
    }

    # 超时未绑定:必需服务报错;非必需服务杀掉残留 kubectl 后跳过
    $msg = "port-forward 超时未就绪 svc/${name}:$port"
    if ($essential) { throw $msg }
    Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
    Remove-Item (Get-PidFile $name) -ErrorAction SilentlyContinue
    Write-Warn "非必需服务 $name 不可用,跳过其 port-forward(不影响登录/Hub/匹配/Battle/结算闭环):$msg"
}

Write-Host ""
Write-Host "============================================" -ForegroundColor Magenta
Write-Host " Pandora k8s Envoy bridge" -ForegroundColor Magenta
Write-Host "============================================" -ForegroundColor Magenta

Ensure-File $ComposeFile
Ensure-File $EnvFile
Ensure-File (Join-Path $ProjectRoot 'deploy/envoy/envoy.yaml')
Ensure-File (Join-Path $ProjectRoot 'deploy/envoy/cert.pem')
Ensure-File (Join-Path $ProjectRoot 'deploy/envoy/key.pem')
New-Item -ItemType Directory -Force -Path $StateDir | Out-Null

Write-Step "[1/2] 启本地 kubectl port-forward"
foreach ($forward in $Forwards) {
    Start-PortForward $forward.Name $forward.Port $forward.Essential
}

Write-Step "[2/2] 启 docker envoy(:8443 / :8444)"
docker compose -f $ComposeFile --env-file $EnvFile up -d envoy
if ($LASTEXITCODE -ne 0) { throw 'envoy 容器启动失败' }

Write-Host ""
Write-Ok '宿主 Envoy 桥接已就绪。UE 客户端/DS 现在可经 127.0.0.1:8443 / :8444 回到 k8s 服务。'