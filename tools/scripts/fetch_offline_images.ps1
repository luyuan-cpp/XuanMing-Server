<#
.SYNOPSIS
  从制品目录拉取业务镜像离线包到 deploy/offline-images/,替代旧的"svn update / git pull 拿 tar"。

.DESCRIPTION
  拉取 <制品根>\images\<版本>\pandora-images.tar,先按 sha256sums.txt 校验完整性,
  再落到 deploy/offline-images/pandora-images.tar —— 之后的流程完全不变:
  一键启动 .cmd / start.ps1 的离线导入短路、import_images.ps1 都从该路径读。

  制品根目录:-ArtifactRoot > 环境变量 PANDORA_ARTIFACT_ROOT > F:\work\artifacts
  (目标机通过共享盘/映射盘访问制品根时,把 PANDORA_ARTIFACT_ROOT 设成对应 UNC 路径即可)

.EXAMPLE
  # 拉最新版
  pwsh tools/scripts/fetch_offline_images.ps1

  # 拉指定版本(版本号 = images\ 下的目录名,如 g1a2b3c4d5e6)
  pwsh tools/scripts/fetch_offline_images.ps1 -Version g1a2b3c4d5e6
#>
[CmdletBinding()]
param(
    [string]$Version,       # 发布轨=版本号(如 v0.1.0,必填);快照轨=指定 git sha,缺省读 latest 指针
    [ValidateSet('snapshot', 'release')]
    [string]$Channel = 'snapshot',  # 默认拉 dev 快照;拉正式版用 -Channel release -Version v0.1.0
    [string]$ArtifactRoot
)

$ErrorActionPreference = 'Stop'
$ScriptDir   = $PSScriptRoot
$ProjectRoot = (Resolve-Path "$ScriptDir/../..").Path
. (Join-Path $ScriptDir 'artifacts_lib.ps1')

$channelRoot = Get-ChannelRoot -Override $ArtifactRoot -Channel $Channel

if (-not $Version) {
    if ($Channel -eq 'release') { throw '发布轨必须指定 -Version(如 v0.1.0)。' }
    $latest = Join-Path $channelRoot 'images\latest.json'
    if (-not (Test-Path -LiteralPath $latest)) {
        throw "未指定 -Version 且找不到快照指针 $latest;先在构建机跑 publish_offline_images.ps1。"
    }
    $Version = (Get-Content -LiteralPath $latest -Raw | ConvertFrom-Json).version
    if (-not $Version) { throw "latest.json 内容非法:$latest" }
}
$folderVer = if ($Channel -eq 'release') { ($Version -replace '[^A-Za-z0-9._-]', '_') } else { $Version }

$verDir = Join-Path $channelRoot "images\$folderVer"
if (-not (Test-Path -LiteralPath $verDir)) { throw "制品不存在:$verDir(频道 $Channel)" }

Write-Host "[INFO] 校验制品完整性:$verDir" -ForegroundColor Cyan
Test-Sha256Sums -Dir $verDir

$src = Join-Path $verDir 'pandora-images.tar'
$dstDir = Join-Path $ProjectRoot 'deploy/offline-images'
if (-not (Test-Path -LiteralPath $dstDir)) { New-Item -ItemType Directory -Path $dstDir -Force | Out-Null }
$dst = Join-Path $dstDir 'pandora-images.tar'

# 先落临时名再改名,避免半截文件被启动脚本读走
$tmp = "$dst.fetching"
Copy-Item -LiteralPath $src -Destination $tmp -Force
Move-Item -LiteralPath $tmp -Destination $dst -Force

$sizeMB = [math]::Round((Get-Item -LiteralPath $dst).Length / 1MB, 1)
Write-Host "[ OK ] 已拉取 $Version → $dst(${sizeMB} MB)" -ForegroundColor Green
Write-Host "[INFO] 之后直接双击一键启动 .cmd 即可(启动脚本自动 docker load 本 tar)。" -ForegroundColor Cyan
