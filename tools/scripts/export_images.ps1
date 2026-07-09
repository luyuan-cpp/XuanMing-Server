<#
.SYNOPSIS
  在「能联网」的机器上把 Pandora 业务镜像(+可选基础设施镜像)打包成 tar,
  拷到「拉不到镜像」的机器上用 import_images.ps1 离线导入。

.DESCRIPTION
  目标机连不上 Docker Hub / 国内加速站时,不必在目标机联网构建。流程:
    1) 本机(能联网)跑本脚本 → 构建 20 个 pandora/*:dev + 打包成 pandora-images.tar
    2) U 盘 / 共享盘 把 tar 拷到目标机
    3) 目标机跑 import_images.ps1 → docker load 进本地
    4) 目标机双击「策划一键启动-含战斗.cmd」即可(镜像已在本地,不再联网拉)

  默认只打包 20 个业务镜像(pandora/*:dev)。基础设施(mysql/redis/kafka/etcd/
  prometheus/grafana/envoy)一般目标机已经拉到过并在跑;若目标机是全新环境、
  基础设施也拉不下来,加 -IncludeInfra 一并打包。

.EXAMPLE
  # 本机:先构建再打包(推荐,保证镜像最新)
  pwsh tools/scripts/export_images.ps1 -Build

  # 本机:宿主编译再打包(快,秒级增量,需装 Go)
  pwsh tools/scripts/export_images.ps1 -Build -BuildMode host

  # 本机:只打包(镜像已构建好,不想重建)
  pwsh tools/scripts/export_images.ps1

  # 本机:业务 + 基础设施一起打包(目标机是全新环境)
  pwsh tools/scripts/export_images.ps1 -Build -IncludeInfra

  # 指定输出路径
  pwsh tools/scripts/export_images.ps1 -Build -Out D:\pandora-images.tar
#>
[CmdletBinding()]
param(
    [switch]$Build,          # 打包前先构建业务镜像(复用 start.ps1 的 Build-AllImages)
    [switch]$IncludeInfra,   # 连基础设施镜像一起打包(全新目标机才需要)
    [ValidateSet('incontainer', 'host')]
    [string]$BuildMode = 'incontainer', # 配合 -Build:incontainer=容器内编译(默认)/ host=宿主交叉编译再打包(快)
    [string]$Out             # 输出 tar 路径(默认 <仓库根>/deploy/offline-images/pandora-images.tar)
)

$ErrorActionPreference = 'Stop'
$ScriptDir   = $PSScriptRoot
$ProjectRoot = (Resolve-Path "$ScriptDir/../..").Path

function Write-Info($m) { Write-Host "[INFO] $m" -ForegroundColor Cyan }
function Write-Ok($m)   { Write-Host "[ OK ] $m" -ForegroundColor Green }
function Write-Warn($m) { Write-Host "[WARN] $m" -ForegroundColor Yellow }
function Write-Err($m)  { Write-Host "[ERR ] $m" -ForegroundColor Red }
function Write-Step($m) { Write-Host "`n===== $m =====" -ForegroundColor Magenta }

function Get-ArchiveRepoTags([string]$Archive) {
    $json = tar -xOf $Archive manifest.json
    if ($LASTEXITCODE -ne 0) { throw "无法读取镜像包 manifest.json:$Archive" }
    $manifest = $json | ConvertFrom-Json
    $tags = @()
    foreach ($entry in @($manifest)) {
        foreach ($tag in @($entry.RepoTags)) {
            if ($tag) { $tags += $tag }
        }
    }
    return $tags
}

function Copy-ArchivePayload([string]$ExtractDir, [string]$MergeDir) {
    Get-ChildItem -LiteralPath $ExtractDir -Recurse -File | ForEach-Object {
        $rel = [System.IO.Path]::GetRelativePath($ExtractDir, $_.FullName)
        if ($rel -in @('manifest.json', 'repositories', 'index.json', 'oci-layout')) { return }

        $target = Join-Path $MergeDir $rel
        $targetDir = Split-Path -Parent $target
        if (-not (Test-Path -LiteralPath $targetDir)) {
            New-Item -ItemType Directory -Path $targetDir -Force | Out-Null
        }
        if (-not (Test-Path -LiteralPath $target)) {
            Copy-Item -LiteralPath $_.FullName -Destination $target
        }
    }
}

function Save-MergedDockerArchive([string[]]$Images, [string]$OutPath) {
    $tmpRoot = Join-Path ([System.IO.Path]::GetTempPath()) ("pandora-docker-save-" + [System.Guid]::NewGuid().ToString('N'))
    $mergeDir = Join-Path $tmpRoot 'merged'
    New-Item -ItemType Directory -Path $mergeDir -Force | Out-Null

    try {
        $allManifest = @()
        $seenTags = New-Object 'System.Collections.Generic.HashSet[string]'
        $index = 0

        foreach ($img in $Images) {
            $singleTar = Join-Path $tmpRoot ("image-$index.tar")
            $extractDir = Join-Path $tmpRoot ("image-$index")
            New-Item -ItemType Directory -Path $extractDir -Force | Out-Null

            docker save -o $singleTar $img
            if ($LASTEXITCODE -ne 0) { throw "docker save 单镜像失败:$img" }

            tar -xf $singleTar -C $extractDir
            if ($LASTEXITCODE -ne 0) { throw "解包单镜像 archive 失败:$img" }

            $manifestPath = Join-Path $extractDir 'manifest.json'
            $entries = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
            foreach ($entry in @($entries)) {
                $newTags = @()
                foreach ($tag in @($entry.RepoTags)) {
                    if ($tag -and $seenTags.Add($tag)) { $newTags += $tag }
                }
                if ($newTags.Count -gt 0) {
                    $entry.RepoTags = [string[]]$newTags
                    $allManifest += $entry
                }
            }

            Copy-ArchivePayload -ExtractDir $extractDir -MergeDir $mergeDir
            $index++
        }

        $allManifest | ConvertTo-Json -Depth 20 | Set-Content -LiteralPath (Join-Path $mergeDir 'manifest.json') -Encoding UTF8
        $items = @(Get-ChildItem -LiteralPath $mergeDir | ForEach-Object { $_.Name })
        tar -cf $OutPath -C $mergeDir @items
        if ($LASTEXITCODE -ne 0) { throw "合并镜像 archive 失败。" }
    } finally {
        if (Test-Path -LiteralPath $tmpRoot) {
            Remove-Item -LiteralPath $tmpRoot -Recurse -Force
        }
    }
}

# 20 个业务服务镜像名(与 start.ps1 的 Get-ServiceList 一致)
$BusinessImages = @(
    'pandora/login:dev','pandora/player:dev','pandora/data-service:dev',
    'pandora/friend:dev','pandora/chat:dev','pandora/guild:dev','pandora/mail:dev',
    'pandora/player-locator:dev','pandora/leaderboard:dev','pandora/team:dev',
    'pandora/matchmaker:dev','pandora/matchmaker-pve:dev','pandora/trade:dev','pandora/dialogue:dev',
    'pandora/push:dev','pandora/inventory:dev','pandora/auction:dev',
    'pandora/ds-allocator:dev','pandora/hub-allocator:dev','pandora/battle-result:dev'
)

# 基础设施镜像(与 deploy/docker-compose.dev.yml 的 image: 一致)
$InfraImages = @(
    'mysql:8.4','redis:8.8.0-alpine',
    'confluentinc/cp-zookeeper:7.9.7','confluentinc/cp-kafka:7.9.7',
    'quay.io/coreos/etcd:v3.6.12','prom/prometheus:v2.55.1',
    'grafana/grafana:11.3.1','envoyproxy/envoy:v1.38-latest'
)

if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    Write-Err "未找到 docker,请先安装 Docker Desktop 并启动。"
    exit 1
}

# ---- 可选:先构建业务镜像 ----
if ($Build) {
    Write-Step "构建 20 个业务镜像(离线优先:本地 golang 基础镜像 + docker.io 源)"

    # Dockerfile 编译阶段用 golang:${GO_VERSION}(默认 1.26.5),运行阶段是 scratch(不需 alpine)。
    # 离线机器拉不到 golang 时,若本地已有等价的 golang 镜像,自动打成所需 tag 直接复用。
    $wantGo = 'golang:1.26.5'
    docker image inspect $wantGo *> $null
    if ($LASTEXITCODE -ne 0) {
        $localGo = (docker images --format '{{.Repository}}:{{.Tag}}' | Select-String '^golang:' | Select-Object -First 1)
        if ($localGo) {
            $src = "$localGo".Trim()
            Write-Warn "本地无 $wantGo,发现 $src,自动打标 $wantGo 复用(避免联网拉取)。"
            docker tag $src $wantGo
            docker tag $src "docker.io/library/golang:1.26.5"
        } else {
            Write-Warn "本地无任何 golang 基础镜像,构建时需联网拉取 golang:1.26.5;若网络受限会失败。"
        }
    } else {
        docker tag $wantGo "docker.io/library/golang:1.26.5" 2>$null
    }

    # 用本地 golang(docker.io 源,已在本地不会真的联网)+ goproxy.cn 拉 go 模块。
    if (-not $env:PANDORA_BASE_REGISTRY) { $env:PANDORA_BASE_REGISTRY = 'docker.io' }
    if (-not $env:PANDORA_GOPROXY)       { $env:PANDORA_GOPROXY       = 'https://goproxy.cn,direct' }

    $buildOnlyServices = @($BusinessImages | ForEach-Object { ($_ -replace '^pandora/', '') -replace ':dev$', '' })
    & (Join-Path $ScriptDir 'start.ps1') -Mode docker -BuildOnly -BuildMode $BuildMode -Only $buildOnlyServices
    if ($LASTEXITCODE -ne 0) { throw "业务镜像构建失败,先解决构建问题再打包。" }
}

# ---- 收集要打包的镜像,校验本地存在 ----
$images = @($BusinessImages)
if ($IncludeInfra) { $images += $InfraImages }

Write-Step "校验本地镜像是否齐全"
$missing = @()
foreach ($img in $images) {
    docker image inspect $img *> $null
    if ($LASTEXITCODE -ne 0) { $missing += $img } else { Write-Ok "已存在:$img" }
}
if ($missing.Count -gt 0) {
    Write-Err "以下镜像本地不存在,无法打包:"
    $missing | ForEach-Object { Write-Err "  - $_" }
    if (-not $Build) { Write-Warn "提示:加 -Build 先构建业务镜像;基础设施缺失请先 docker compose 起一次或加 -IncludeInfra 前先拉取。" }
    exit 1
}

# ---- 输出路径 ----
if (-not $Out) {
    $outDir = Join-Path $ProjectRoot 'deploy/offline-images'
    if (-not (Test-Path $outDir)) { New-Item -ItemType Directory -Path $outDir -Force | Out-Null }
    $Out = Join-Path $outDir 'pandora-images.tar'
}

Write-Step "导出 $($images.Count) 个镜像 → $Out(可能几分钟,镜像较大)"
docker save -o $Out @images
if ($LASTEXITCODE -ne 0) { throw "docker save 失败。" }

$exportedTags = @(Get-ArchiveRepoTags -Archive $Out)
$missingTags = @($images | Where-Object { $exportedTags -notcontains $_ })
if ($missingTags.Count -gt 0) {
    Write-Warn "docker save 产物缺少 $($missingTags.Count) 个 tag,改用逐镜像合并 archive:"
    $missingTags | ForEach-Object { Write-Warn "  - $_" }
    Save-MergedDockerArchive -Images $images -OutPath $Out

    $exportedTags = @(Get-ArchiveRepoTags -Archive $Out)
    $missingTags = @($images | Where-Object { $exportedTags -notcontains $_ })
    if ($missingTags.Count -gt 0) {
        Write-Err "合并后的镜像包仍缺少以下 tag:"
        $missingTags | ForEach-Object { Write-Err "  - $_" }
        throw "离线镜像包校验失败。"
    }
}
Write-Ok "镜像包 manifest 校验通过($($exportedTags.Count) 个 tag)。"

$sizeMB = [math]::Round((Get-Item $Out).Length / 1MB, 1)
Write-Ok "打包完成:$Out(${sizeMB} MB)"
Write-Host ""
Write-Info "下一步(拷到目标机后,在目标机上跑):"
Write-Info "  pwsh tools/scripts/import_images.ps1 -In <拷过去的 pandora-images.tar 路径>"
Write-Info "导入后目标机双击「策划一键启动-含战斗.cmd」即可(镜像已在本地,不再联网拉)。"
