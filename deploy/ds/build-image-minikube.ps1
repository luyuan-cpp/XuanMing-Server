# 直接在 minikube 的 Docker daemon 里构建 Pandora DS 镜像（避开 minikube image load 这一步）。
#
# 为什么不用 `minikube image build`：
#   PowerShell 下 `minikube image build` 对 Windows 路径有解析 bug，会把 `F:\...` 的盘符
#   当成 `F:` 目录、把上下文路径切坏。最稳的做法是用 `minikube docker-env --shell powershell`
#   把当前会话的 docker CLI 指到 minikube 内置 daemon，再跑普通 `docker build`——镜像直接落在
#   minikube 里，省掉宿主 build + `minikube image load` 的两步。
#
# 前置：
#   1) minikube 已起（start-minikube-agones.ps1）。
#   2) deploy/ds/stage/LinuxServer 已就绪（客户端仓库 build-linux-ds.ps1 拷好）。
#   3) 装了 docker CLI（minikube docker-env 只是改 DOCKER_HOST 等 env，仍需本机 docker 客户端）。
#
# 用法（后端仓库根目录）：
#   pwsh deploy/ds/build-image-minikube.ps1                       # 默认建 battle + hub 两个 :dev
#   pwsh deploy/ds/build-image-minikube.ps1 -Tag dev             # 自定义 tag（默认 dev，与 Fleet yaml 一致）
#   pwsh deploy/ds/build-image-minikube.ps1 -Image battle        # 只建 battle
#   pwsh deploy/ds/build-image-minikube.ps1 -Profile pandora-agones # 指定 minikube profile
#
# 构建完镜像已在 minikube 里，Fleet yaml 的 imagePullPolicy=IfNotPresent 直接命中；
# 跑 e2e_k8s.ps1 时加 -SkipImageLoad 跳过 minikube image load。

param(
    [ValidateSet('both', 'battle', 'hub')]
    [string]$Image = 'both',
    [string]$Tag = 'dev',
    [string]$Profile = '',
    [string]$BaseImage = 'ubuntu:22.04'
)

$ErrorActionPreference = 'Stop'

$ScriptDir = $PSScriptRoot
$StageDir = Join-Path $ScriptDir 'stage\LinuxServer'
$Dockerfile = Join-Path $ScriptDir 'Dockerfile'

if (-not (Test-Path $StageDir)) {
    throw "缺少 $StageDir，请先在客户端仓库跑 Tool/Server/Agones/build-linux-ds.ps1。"
}
if (-not (Test-Path $Dockerfile)) {
    throw "找不到 Dockerfile：$Dockerfile"
}
if (-not (Get-Command minikube -ErrorAction SilentlyContinue)) {
    throw "找不到 minikube，可先跑 deploy/ds/install-minikube-windows.ps1。"
}
if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    throw "找不到 docker CLI；minikube docker-env 只改 env，仍需本机 docker 客户端。"
}

if ([string]::IsNullOrWhiteSpace($Profile)) {
    # 优先用当前 minikube profile；解析不到时 fallback 到 'pandora-agones'（本地 Agones 联调用 profile）。
    # 不要 fallback 到 'minikube'：旧默认 profile（192.168.49.x docker network）残留会让镜像落错 daemon，
    # 后续 relay/DS 走错网络，登录成功但 UDP 进不了 Hub DS。
    $Profile = (& minikube profile 2>$null | Select-Object -First 1).Trim()
    if ([string]::IsNullOrWhiteSpace($Profile)) {
        $Profile = 'pandora-agones'
        Write-Host "[build-image-minikube] 未指定 -Profile 且无法解析当前 minikube profile，fallback 到 '$Profile'" -ForegroundColor Yellow
    } else {
        Write-Host "[build-image-minikube] 未指定 -Profile，使用当前 minikube profile '$Profile'" -ForegroundColor DarkGray
    }
}
Write-Host "[build-image-minikube] 目标 minikube profile = '$Profile'" -ForegroundColor Cyan

# 校验 minikube profile 在跑（Running 才有可连的内置 daemon）。
$status = & minikube -p $Profile status --format '{{.Host}}' 2>$null
if ($LASTEXITCODE -ne 0 -or $status -notmatch 'Running') {
    throw "minikube profile '$Profile' 未在运行（status='$status'）。先跑 deploy/ds/start-minikube-agones.ps1。"
}

# 把当前 PowerShell 会话的 docker env 指到 minikube 内置 daemon。
# `minikube docker-env --shell powershell` 会输出一串 Set-Item env:... 语句，Invoke-Expression 应用即可。
Write-Host "[build-image-minikube] 切换 docker daemon -> minikube profile '$Profile'" -ForegroundColor Cyan
$envScript = & minikube -p $Profile docker-env --shell powershell
if ($LASTEXITCODE -ne 0) {
    throw "minikube docker-env 失败；确认 profile '$Profile' 用 docker driver 且在运行。"
}
$envScript | Invoke-Expression

# 确认现在连的是 minikube 里的 daemon（而不是宿主 Docker Desktop）。
$dockerCtx = & docker info --format '{{.Name}}' 2>$null
Write-Host "[build-image-minikube] 当前 docker daemon node = $dockerCtx" -ForegroundColor DarkGray

function Build-One {
    param([string]$Name)
    $fullTag = "pandora/$Name-ds:$Tag"
    Write-Host "[build-image-minikube] docker build -t $fullTag --build-arg BASE_IMAGE=$BaseImage" -ForegroundColor Green
    & docker build --build-arg "BASE_IMAGE=$BaseImage" -f $Dockerfile -t $fullTag $ScriptDir
    if ($LASTEXITCODE -ne 0) {
        throw "docker build 失败：$fullTag"
    }
    Write-Host "[build-image-minikube] 完成（已落在 minikube）：$fullTag" -ForegroundColor Green
}

switch ($Image) {
    'battle' { Build-One 'battle' }
    'hub' { Build-One 'hub' }
    'both' { Build-One 'battle'; Build-One 'hub' }
}

Write-Host ""
Write-Host "[build-image-minikube] 全部完成。镜像已在 minikube profile '$Profile' 内。" -ForegroundColor Green
Write-Host "[build-image-minikube] 下一步：pwsh tools/scripts/e2e_k8s.ps1 -SkipImageLoad" -ForegroundColor Yellow
Write-Host "[build-image-minikube] 注意：本会话的 docker env 已指向 minikube；要回宿主 Docker，重开终端或运行 minikube docker-env -u | Invoke-Expression。" -ForegroundColor DarkGray
