<#
.SYNOPSIS
  构建业务镜像离线包并发布到制品目录(版本库外),替代旧的"tar 入库随仓库同步"。

.DESCRIPTION
  流程:
    1) git 取版本戳(短 sha;工作区脏则默认拒绝,-AllowDirty 放行并带时间戳后缀)
    2) 调用 export_images.ps1 生成 deploy/offline-images/pandora-images.tar
       (位置不变:本机 start.ps1 的离线导入短路继续可用;该 tar 已不入库)
    3) 从 tar 的 manifest.json 提取每个镜像的 RepoTags + Config 摘要(镜像 ID),
       生成 images-manifest.json —— 部署侧可据此核验导入的镜像身份
    4) 原子发布到 <制品根>\images\<版本>\,附 sha256sums.txt + build-info.json,
       并更新 <制品根>\images\latest.json 指针

  制品根目录:-ArtifactRoot > 环境变量 PANDORA_ARTIFACT_ROOT > F:\work\artifacts

.EXAMPLE
  # 从当前源码重建并发布(推荐;宿主 Go 交叉编译)
  pwsh tools/scripts/publish_offline_images.ps1

  # 镜像/包已是最新(export 过期守卫会核验),只发布不重建
  pwsh tools/scripts/publish_offline_images.ps1 -SkipBuild
#>
[CmdletBinding()]
param(
    [switch]$SkipBuild,     # 不重建,复用现有 tar(export_images.ps1 的过期守卫仍会核验新旧)
    [switch]$AllowDirty,    # git 工作区脏时仍发布(版本号带 -dirty-时间戳;仅本机联调用,CI 禁用)
    [switch]$SkipIfExists,  # 目标版本已发布时静默成功(CI 幂等重跑用)
    [string]$ArtifactRoot,  # 制品根目录覆盖
    [string]$Version        # 发布版本号(如 v0.1.0):显式覆盖 git describe 注入镜像,免打 tag;记入 build-info.app_version
)

$ErrorActionPreference = 'Stop'
$ScriptDir   = $PSScriptRoot
$ProjectRoot = (Resolve-Path "$ScriptDir/../..").Path
. (Join-Path $ScriptDir 'artifacts_lib.ps1')

function Write-Info($m) { Write-Host "[INFO] $m" -ForegroundColor Cyan }
function Write-Ok($m)   { Write-Host "[ OK ] $m" -ForegroundColor Green }

# ---- 1) git 版本戳 ----
Push-Location $ProjectRoot
try {
    $sha = (git rev-parse --short=12 HEAD 2>$null | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or -not $sha) { throw '无法读取 git HEAD,发布必须能追溯到源码版本。' }
    $dirty = @(git status --porcelain 2>$null | Where-Object { $_ }).Count -gt 0
} finally { Pop-Location }

if ($dirty -and -not $AllowDirty) {
    throw 'git 工作区有未提交改动,发布的镜像无法追溯到确定版本。先提交,或本机联调明确加 -AllowDirty。'
}
# 注意:git 快照版本变量名不能用 $version —— PowerShell 变量名大小写不敏感,$version 与参数
# $Version 是同一变量,会覆盖用户传入的发布版本号(v0.1.0 → g<sha>),连带污染下面的 channel /
# folderVer / app_version 判定。用独立名 $snapshotVer 隔离。
$snapshotVer = if ($dirty) { "g$sha-dirty-" + (Get-Date -Format 'yyyyMMdd-HHmmss') } else { "g$sha" }

# 频道:有 -Version = 发布版本轨(releases\,目录名=版本号);否则 dev 快照轨(snapshots\,目录名=git sha)
$channel   = if ($Version) { 'release' } else { 'snapshot' }
$folderVer = if ($Version) { ($Version -replace '[^A-Za-z0-9._-]', '_') } else { $snapshotVer }

# ---- 2) 生成/校验离线 tar(复用既有脚本与其过期守卫,不另造构建逻辑) ----
# 发布版本号覆盖:设 PANDORA_RELEASE_VERSION 后,start.ps1 的 Get-VersionInfo 用它注入 -ldflags,
# 免打 git tag(标准 CI 做法:显式 VERSION 优先于 git describe)。Commit 字段仍记 git sha 作溯源。
if ($Version) {
    $env:PANDORA_RELEASE_VERSION = $Version
    Write-Info "镜像版本注入为 $Version(覆盖 git describe;git sha $sha 仍作溯源)"
}
# 显式传参:数组 splat 传 switch 参数(-Build)会被 PowerShell 误绑成 -BuildMode 的值,必须显式写。
$exportScript = Join-Path $ScriptDir 'export_images.ps1'
if ($SkipBuild) { & $exportScript } else { & $exportScript -Build -BuildMode host }
if ($LASTEXITCODE -ne 0) { throw "export_images.ps1 失败(exit=$LASTEXITCODE),不发布。" }

$tar = Join-Path $ProjectRoot 'deploy/offline-images/pandora-images.tar'
if (-not (Test-Path -LiteralPath $tar)) { throw "找不到离线包:$tar" }

# ---- 3) 从 tar manifest 提取镜像身份(tag + 镜像 ID),不重复维护镜像清单 ----
$manifestJson = tar -xOf $tar manifest.json
if ($LASTEXITCODE -ne 0) { throw "无法读取离线包 manifest.json:$tar" }
$entries = $manifestJson | ConvertFrom-Json
$imagesManifest = @(foreach ($e in @($entries)) {
    [pscustomobject]@{
        repo_tags = @($e.RepoTags)
        # docker archive 的 Config 即镜像 ID blob(如 blobs/sha256/<id>),据此核验导入结果
        image_id  = ($e.Config -replace '^.*/', '' -replace '\.json$', '')
    }
})
if ($imagesManifest.Count -eq 0) { throw '离线包 manifest 为空,拒绝发布。' }

# ---- 4) 原子发布(按频道分仓) ----
$channelRoot = Get-ChannelRoot -Override $ArtifactRoot -Channel $channel
$finalDir = Join-Path $channelRoot "images\$folderVer"
if (Test-Path -LiteralPath $finalDir) {
    if ($SkipIfExists) { Write-Ok "版本已发布,跳过(不可变):$finalDir"; exit 0 }
    throw "版本已发布且不可覆盖:$finalDir($channel 轨制品不可变)"
}

Write-Info "发布 [$channel] $folderVer → $finalDir"
$staging = New-AtomicStaging -FinalDir $finalDir
try {
    Copy-Item -LiteralPath $tar -Destination (Join-Path $staging 'pandora-images.tar')
    $imagesManifest | ConvertTo-Json -Depth 5 | Set-Content (Join-Path $staging 'images-manifest.json') -Encoding utf8NoBOM
    [pscustomobject]@{
        version      = $folderVer
        channel      = $channel     # snapshot(dev) / release(发布版本)
        app_version  = $Version    # 注入镜像的发布版本号(v0.1.0);空=未指定,走 git describe 默认
        git_sha      = $sha
        git_dirty    = $dirty
        image_count  = $imagesManifest.Count
        published_at = (Get-Date -Format 'o')
        machine      = $env:COMPUTERNAME
        publisher    = $env:USERNAME
    } | ConvertTo-Json | Set-Content (Join-Path $staging 'build-info.json') -Encoding utf8NoBOM
    New-Sha256Sums -Dir $staging | Out-Null
    Complete-AtomicDir -Staging $staging -FinalDir $finalDir
} catch {
    if (Test-Path -LiteralPath $staging) { Remove-Item -LiteralPath $staging -Recurse -Force }
    throw
}

# latest.json 是唯一可变文件(指针,类比 registry 的 latest tag);版本目录本身不可变。每个频道各一份。
[pscustomobject]@{ version = $folderVer; channel = $channel; published_at = (Get-Date -Format 'o') } |
    ConvertTo-Json | Set-Content (Join-Path $channelRoot 'images\latest.json') -Encoding utf8NoBOM

Write-Ok "镜像离线包已发布[$channel]:$finalDir($($imagesManifest.Count) 个镜像)"
Write-Info '目标机拉取:pwsh tools/scripts/fetch_offline_images.ps1(之后一键启动 cmd 照常)'
