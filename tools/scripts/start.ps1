<#
.SYNOPSIS
  Pandora 后端一键启动器(策划/开发都能用)。

.DESCRIPTION
  一条命令把后端跑起来,支持 4 种启动方式:
    local   本机直跑   —— 基础设施在 docker,15 个 go 服务以宿主进程运行(可断点调试,策划本地联调首选)
    docker  全容器     —— 基础设施 + 15 个 go 服务全部跑在 docker 容器里
    k8s     本地集群   —— minikube/本地 k8s,基础设施 + 服务都以 Deployment 跑(贴近线上)
    online  线上集群   —— 把服务 kustomize 部署到远端 k8s(需人工授权并确认 kube-context,谨慎)

  启动前会检查必要工具(go / docker / kubectl / minikube)。默认只提示缺失项,不改本机环境;
  只有显式传 -Install 才会尝试用 winget 安装。-Check 只检查不启动。

.EXAMPLE
  pwsh tools/scripts/start.ps1                      # 默认 local 模式,login 档(测登录/组队最小集)
  pwsh tools/scripts/start.ps1 -Mode local -Profile match
  pwsh tools/scripts/start.ps1 -Mode docker
  pwsh tools/scripts/start.ps1 -Mode k8s
  pwsh tools/scripts/start.ps1 -Mode online -Registry registry.mycorp.com -Tag v1.2.3  # 远端 apply,人工确认后使用
  pwsh tools/scripts/start.ps1 -Mode docker -Down  # 停
  pwsh tools/scripts/start.ps1 -Status             # 看状态
  pwsh tools/scripts/start.ps1 -Check              # 只检查工具
  pwsh tools/scripts/start.ps1 -Install            # 缺工具时才尝试 winget 安装
#>
[CmdletBinding()]
param(
    [ValidateSet('local', 'docker', 'k8s', 'online')]
    [string]$Mode = 'local',

    [ValidateSet('login', 'match', 'all')]
    [string]$Profile = 'login',

    [switch]$Down,        # 停止该模式
    [switch]$Status,      # 查看状态
    [switch]$Check,       # 只检查工具链,不启动
    [switch]$Install,     # 工具缺失时尝试 winget 安装(默认不安装)
    [switch]$NoInstall,   # 兼容旧参数;等同于不传 -Install

    # online 模式参数
    [string]$Registry,    # 镜像仓库地址,如 registry.mycorp.com
    [string]$Tag,         # 镜像 tag,如 v1.2.3
    [switch]$BuildPush    # online:本地构建并推送 15 个镜像到 -Registry(远端发布动作,需人工授权)
)

$ErrorActionPreference = 'Stop'
$ScriptDir   = $PSScriptRoot
$ProjectRoot = (Resolve-Path "$ScriptDir/../..").Path
$ComposeInfra    = Join-Path $ProjectRoot 'deploy/docker-compose.dev.yml'
$ComposeServices = Join-Path $ProjectRoot 'deploy/docker-compose.services.yml'
$EnvFile         = Join-Path $ProjectRoot 'deploy/env/dev.env'
$ClusterEtcDir   = Join-Path $ProjectRoot 'run/cluster/etc'
$K8sNamespace    = 'pandora'

# ===== 输出辅助 =====
function Write-Info($m) { Write-Host "[INFO] $m" -ForegroundColor Cyan }
function Write-Ok($m)   { Write-Host "[ OK ] $m" -ForegroundColor Green }
function Write-Skip($m) { Write-Host "[SKIP] $m" -ForegroundColor DarkGray }
function Write-Warn($m) { Write-Host "[WARN] $m" -ForegroundColor Yellow }
function Write-Err($m)  { Write-Host "[ERR ] $m" -ForegroundColor Red }
function Write-Step($m) { Write-Host "`n===== $m =====" -ForegroundColor Magenta }

function Test-CommandExists([string]$cmd) {
    return [bool](Get-Command $cmd -ErrorAction SilentlyContinue)
}

# ===== 工具检查 + 显式安装 =====
# 返回 $true=就绪 / $false=缺失(未能装上)
function Ensure-Tool {
    param(
        [string]$Name,
        [string]$CheckCmd,
        [string]$WingetId,
        [string]$ManualUrl,
        [switch]$Required
    )
    if (Test-CommandExists $CheckCmd) {
        Write-Ok "$Name 已就绪"
        return $true
    }
    Write-Warn "$Name 未安装"
    if ($Check -or $NoInstall -or -not $Install) {
        if ($ManualUrl) { Write-Host "       手动安装:$ManualUrl" -ForegroundColor Yellow }
        if (-not $Check -and -not $NoInstall -and -not $Install) {
            Write-Host "       如需脚本尝试安装,请显式追加 -Install。" -ForegroundColor Yellow
        }
        return $false
    }
    if (-not $WingetId) {
        Write-Err "$Name 无法自动安装,请手动装:$ManualUrl"
        return $false
    }
    if (-not (Test-CommandExists 'winget')) {
        Write-Err "未找到 winget,无法自动安装 $Name;请手动装:$ManualUrl"
        return $false
    }
    Write-Info "  winget 安装 $Name ($WingetId) ..."
    winget install --id $WingetId --silent --accept-source-agreements --accept-package-agreements | Out-Null
    # winget 装完当前会话 PATH 可能没刷新
    if (Test-CommandExists $CheckCmd) {
        Write-Ok "$Name 安装成功"
        return $true
    }
    Write-Warn "$Name 已尝试安装,但当前终端还找不到命令 —— 多半是 PATH 未刷新。"
    Write-Warn "       请『新开一个终端』后重跑本脚本。"
    return $false
}

function Test-DockerRunning {
    if (-not (Test-CommandExists 'docker')) { return $false }
    docker info *> $null
    return ($LASTEXITCODE -eq 0)
}

# 确保 docker 命令存在且 daemon 在跑(Docker Desktop 不能自动装,只能提示)
function Ensure-Docker {
    $ok = Ensure-Tool -Name 'Docker' -CheckCmd 'docker' -ManualUrl 'https://www.docker.com/products/docker-desktop/'
    if (-not $ok) { return $false }
    if ($Check) { return $true }
    if (-not (Test-DockerRunning)) {
        Write-Err "Docker 已装但 daemon 没在跑 —— 请启动 Docker Desktop 后重试。"
        return $false
    }
    Write-Ok "Docker daemon 运行中"
    return $true
}

function Ensure-Go {
    return (Ensure-Tool -Name 'Go' -CheckCmd 'go' -WingetId 'GoLang.Go' -ManualUrl 'https://go.dev/dl/ (需 1.26.4+)')
}

# 检查给定模式需要的工具;返回 $true=全就绪
function Resolve-Prerequisites([string]$mode) {
    Write-Step "检查必要工具($mode 模式)"
    $allOk = $true
    switch ($mode) {
        'local' {
            if (-not (Ensure-Go))     { $allOk = $false }
            if (-not (Ensure-Docker)) { $allOk = $false }
        }
        'docker' {
            if (-not (Ensure-Docker)) { $allOk = $false }
        }
        'k8s' {
            if (-not (Ensure-Docker)) { $allOk = $false }
            if (-not (Ensure-Tool -Name 'kubectl'  -CheckCmd 'kubectl'  -WingetId 'Kubernetes.kubectl'  -ManualUrl 'https://kubernetes.io/docs/tasks/tools/')) { $allOk = $false }
            if (-not (Ensure-Tool -Name 'minikube' -CheckCmd 'minikube' -WingetId 'Kubernetes.minikube' -ManualUrl 'https://minikube.sigs.k8s.io/docs/start/')) { $allOk = $false }
        }
        'online' {
            if (-not (Ensure-Tool -Name 'kubectl' -CheckCmd 'kubectl' -WingetId 'Kubernetes.kubectl' -ManualUrl 'https://kubernetes.io/docs/tasks/tools/')) { $allOk = $false }
        }
    }
    return $allOk
}

# ===== local 模式(宿主 go 进程 + docker 基础设施)=====
function Invoke-Local {
    if ($Down) {
        & "$ScriptDir/dev_all.ps1" -Down
        return
    }
    Write-Step "local 模式:基础设施(docker) + 15 个 go 服务(宿主进程)"
    Write-Info "策划本地联调用这个;服务可在 VS Code 断点调试。"
    & "$ScriptDir/dev_all.ps1" -Profile $Profile
}

# ===== docker 模式(全容器)=====
function Invoke-Docker {
    if ($Down) {
        Write-Step "停止 docker 业务服务"
        docker compose -f $ComposeServices down
        Write-Step "停止基础设施"
        & "$ScriptDir/dev_down.ps1"
        return
    }
    Write-Step "docker 模式:基础设施 + 15 个 go 服务全部容器化"

    # local 宿主进程会抢同一批端口,先停掉
    Write-Info "先停掉可能在跑的宿主 go 服务(避免端口冲突)..."
    & "$ScriptDir/run_services.ps1" -Action down 2>$null

    Write-Step "[1/3] 基础设施(建 pandora-net)"
    & "$ScriptDir/dev_up.ps1"
    if ($LASTEXITCODE -ne 0) { throw "基础设施启动失败" }

    Write-Step "[2/3] 生成集群版配置"
    & "$ScriptDir/gen_cluster_config.ps1"

    Write-Step "[3/3] 构建并启动业务服务容器"
    docker compose -f $ComposeServices up -d --build
    if ($LASTEXITCODE -ne 0) { throw "业务服务容器启动失败" }

    Write-Host ""
    Write-Ok "docker 模式已启动。查看:docker compose -f deploy/docker-compose.services.yml ps"
}

# ===== k8s 模式(本地 minikube)=====
function Invoke-K8s {
    $servicesDir = Join-Path $ProjectRoot 'deploy/k8s/services'
    $infraYaml   = Join-Path $ProjectRoot 'deploy/k8s/infra/infra.yaml'
    $mysqlInit   = Join-Path $ProjectRoot 'deploy/mysql-init'

    if ($Down) {
        Write-Step "删除 k8s 业务服务 + 基础设施"
        kubectl delete -k $servicesDir --ignore-not-found 2>$null
        kubectl delete -f $infraYaml --ignore-not-found 2>$null
        Write-Info "minikube 仍在运行;彻底关:minikube stop"
        return
    }

    Write-Step "k8s 模式:minikube 本地集群"

    # 1) minikube 起没起
    minikube status *> $null
    if ($LASTEXITCODE -ne 0) {
        Write-Info "启动 minikube(driver=docker)..."
        minikube start --driver=docker --cpus=4 --memory=6144
        if ($LASTEXITCODE -ne 0) { throw "minikube 启动失败" }
    } else {
        Write-Ok "minikube 已在运行"
    }

    Write-Step "[1/6] namespace"
    kubectl apply -f (Join-Path $servicesDir '00-namespace.yaml')

    Write-Step "[2/6] 生成集群版配置 + ConfigMap"
    & "$ScriptDir/gen_cluster_config.ps1"
    kubectl create configmap pandora-config --from-file=$ClusterEtcDir -n $K8sNamespace `
        --dry-run=client -o yaml | kubectl apply -f -
    kubectl create configmap pandora-mysql-init --from-file=$mysqlInit -n $K8sNamespace `
        --dry-run=client -o yaml | kubectl apply -f -

    Write-Step "[3/6] 基础设施(mysql/redis/kafka/etcd)"
    kubectl apply -f $infraYaml
    Write-Info "等待基础设施就绪(最多 180s)..."
    kubectl rollout status deploy/mysql -n $K8sNamespace --timeout=180s
    kubectl rollout status deploy/redis -n $K8sNamespace --timeout=120s
    kubectl rollout status deploy/etcd  -n $K8sNamespace --timeout=120s

    Write-Step "[4/6] 构建 15 个服务镜像"
    Build-AllImages

    Write-Step "[5/6] 把镜像 load 进 minikube"
    foreach ($img in (Get-ServiceImages)) {
        Write-Info "  minikube image load $img"
        minikube image load $img
    }

    Write-Step "[6/6] 部署业务服务"
    kubectl apply -k $servicesDir

    Write-Host ""
    Write-Ok "k8s 模式已部署。查看:kubectl get pods -n $K8sNamespace"
}

# ===== online 模式(远端 k8s)=====
function Invoke-Online {
    $overlay     = Join-Path $ProjectRoot 'deploy/k8s/overlays/online'
    $overlayFile = Join-Path $overlay 'kustomization.yaml'
    $mysqlInit   = Join-Path $ProjectRoot 'deploy/mysql-init'

    # 安全:确认当前 kube-context(线上误操作代价高)
    $ctx = (kubectl config current-context) 2>$null
    Write-Step "online 模式:目标 kube-context = $ctx"
    Write-Warn "这会对『$ctx』集群做变更。确认无误请输入该 context 名字以继续:"
    $confirm = Read-Host "  输入 context 名"
    if ($confirm -ne $ctx) {
        Write-Err "输入与当前 context 不一致,已中止(防误操作)。"
        return
    }

    if ($Down) {
        Write-Step "删除 online 业务服务"
        kubectl delete -k $overlay --ignore-not-found
        return
    }

    if (-not $Registry -or -not $Tag) {
        throw "online 模式必须指定 -Registry 和 -Tag(镜像来源)。"
    }

    if ($BuildPush) {
        Write-Step "构建并推送 15 个镜像到 $Registry"
        Build-AllImages
        foreach ($svc in (Get-ServiceList)) {
            $local  = "pandora/$($svc.Name):dev"
            $remote = "$Registry/pandora/$($svc.Name):$Tag"
            docker tag $local $remote
            docker push $remote
            if ($LASTEXITCODE -ne 0) { throw "推送失败:$remote" }
        }
    }

    Write-Step "生成集群版配置 + ConfigMap(namespace $K8sNamespace)"
    & "$ScriptDir/gen_cluster_config.ps1"
    kubectl apply -f (Join-Path $ProjectRoot 'deploy/k8s/services/00-namespace.yaml')
    kubectl create configmap pandora-config --from-file=$ClusterEtcDir -n $K8sNamespace `
        --dry-run=client -o yaml | kubectl apply -f -

    # 用 -Registry/-Tag 临时覆盖 overlay 占位镜像(try/finally 还原,保持仓库干净)
    $orig = Get-Content $overlayFile -Raw
    try {
        $patched = $orig.Replace('registry.example.com', $Registry) -replace 'newTag: latest', "newTag: $Tag"
        [System.IO.File]::WriteAllText($overlayFile, $patched, (New-Object System.Text.UTF8Encoding($false)))
        Write-Step "kubectl apply -k overlays/online"
        kubectl apply -k $overlay
    } finally {
        [System.IO.File]::WriteAllText($overlayFile, $orig, (New-Object System.Text.UTF8Encoding($false)))
    }

    Write-Host ""
    Write-Ok "online 部署已提交。查看:kubectl get pods -n $K8sNamespace"
}

# ===== 共享:服务清单 / 镜像构建 =====
function Get-ServiceList {
    @(
        @{ Name = 'login';          Dir = 'services/account/login';            Cmd = 'login' }
        @{ Name = 'player';         Dir = 'services/account/player';           Cmd = 'player' }
        @{ Name = 'data-service';   Dir = 'services/data/data_service';        Cmd = 'data_service' }
        @{ Name = 'friend';         Dir = 'services/social/friend';            Cmd = 'friend' }
        @{ Name = 'chat';           Dir = 'services/social/chat';              Cmd = 'chat' }
        @{ Name = 'player-locator'; Dir = 'services/runtime/player_locator';   Cmd = 'locator' }
        @{ Name = 'team';           Dir = 'services/matchmaking/team';         Cmd = 'team' }
        @{ Name = 'matchmaker';     Dir = 'services/matchmaking/matchmaker';   Cmd = 'matchmaker' }
        @{ Name = 'trade';          Dir = 'services/economy/trade';            Cmd = 'trade' }
        @{ Name = 'dialogue';       Dir = 'services/social/dialogue';          Cmd = 'dialogue' }
        @{ Name = 'push';           Dir = 'services/runtime/push';             Cmd = 'push' }
        @{ Name = 'inventory';      Dir = 'services/economy/inventory';        Cmd = 'inventory' }
        @{ Name = 'ds-allocator';   Dir = 'services/battle/ds_allocator';      Cmd = 'ds_allocator' }
        @{ Name = 'hub-allocator';  Dir = 'services/battle/hub_allocator';     Cmd = 'hub_allocator' }
        @{ Name = 'battle-result';  Dir = 'services/battle/battle_result';     Cmd = 'battle_result' }
    )
}

function Get-ServiceImages {
    Get-ServiceList | ForEach-Object { "pandora/$($_.Name):dev" }
}

function Build-AllImages {
    $dockerfile = Join-Path $ProjectRoot 'deploy/services/Dockerfile'
    foreach ($svc in (Get-ServiceList)) {
        Write-Info "  docker build pandora/$($svc.Name):dev ..."
        docker build -f $dockerfile `
            --build-arg "SERVICE_DIR=$($svc.Dir)" `
            --build-arg "CMD_NAME=$($svc.Cmd)" `
            -t "pandora/$($svc.Name):dev" $ProjectRoot
        if ($LASTEXITCODE -ne 0) { throw "镜像构建失败:$($svc.Name)" }
    }
}

# ===== 状态 =====
function Show-Status {
    switch ($Mode) {
        'local'  { & "$ScriptDir/run_services.ps1" -Action status }
        'docker' {
            Write-Step "docker 业务服务"
            docker compose -f $ComposeServices ps
            Write-Step "基础设施"
            docker compose -f $ComposeInfra --env-file $EnvFile ps
        }
        { $_ -in 'k8s', 'online' } {
            kubectl get pods,svc -n $K8sNamespace
        }
    }
}

# ===== 主流程 =====
Write-Host ""
Write-Host "============================================" -ForegroundColor Magenta
Write-Host " Pandora 后端一键启动器  ( $Mode )" -ForegroundColor Magenta
Write-Host "============================================" -ForegroundColor Magenta

if ($Status) { Show-Status; exit 0 }

$prereqOk = Resolve-Prerequisites $Mode

if ($Check) {
    Write-Host ""
    if ($prereqOk) { Write-Ok "$Mode 模式所需工具全部就绪。"; exit 0 }
    else { Write-Warn "$Mode 模式有工具缺失,见上方提示。"; exit 1 }
}

if (-not $prereqOk) {
    Write-Err "工具未就绪,已中止。装好后重跑(或新开终端刷新 PATH)。"
    exit 1
}

switch ($Mode) {
    'local'  { Invoke-Local }
    'docker' { Invoke-Docker }
    'k8s'    { Invoke-K8s }
    'online' { Invoke-Online }
}
