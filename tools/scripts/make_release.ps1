<#
.SYNOPSIS
  生成版本化 release:语义版本号 + 修复内容(CHANGELOG) + 制品事实清单(build once, promote many)。

.DESCRIPTION
  在 <制品根>\releases\ 下产出两份:
    <版本>.json  —— release manifest(机器可读):版本号、修复内容、镜像版本+每镜像 image_id、
                    UE 包引用(含来源戳)、配置表 dist 摘要
    <版本>.md    —— release notes(人可读):版本号 + 修复内容 + 制品清单
  manifest 不可变(重名拒绝);被任何 release 引用的制品版本,保留清理脚本永不删除。

  版本号(-Version)遵循语义化版本 vMAJOR.MINOR.PATCH 或日历版本;必须与
  客户端 DefaultGame.ini 的 ProjectVersion、后端 git tag 一致。
  修复内容来源优先级:-Notes(行内) > -NotesFile > CHANGELOG.md 的对应版本段落。

.EXAMPLE
  # 版本段已写进 CHANGELOG.md,引用最新镜像 + 指定 5 个 UE 包
  pwsh tools/scripts/make_release.ps1 -Version v0.1.0 `
      -ClientPackages 'client/trunk_Client/Client_Win64_Development/r1416-...', '...'

  # 修复内容用单独文件
  pwsh tools/scripts/make_release.ps1 -Version v0.1.0 -NotesFile .\notes-0.1.0.md
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)][Alias('Name')][string]$Version,  # 语义版本,如 v0.1.0 / 2026.07.24
    [string]$Notes,                        # 行内修复内容(最高优先级)
    [string]$NotesFile,                    # 修复内容文件(次优先)
    [string]$Changelog,                    # CHANGELOG.md 路径(默认后端仓根 CHANGELOG.md)
    [string]$ImagesVersion,                # 缺省取 images\latest.json
    [string[]]$ClientPackages = @(),       # 制品根下相对路径(client/<branch>/<flavor>/<ver>)
    [string]$ArtifactRoot,
    [switch]$AllowDirty                    # 允许引用 dirty 来源的制品(默认拒绝,守正规发布)
)

$ErrorActionPreference = 'Stop'
$ScriptDir   = $PSScriptRoot
$ProjectRoot = (Resolve-Path "$ScriptDir/../..").Path
. (Join-Path $ScriptDir 'artifacts_lib.ps1')

# ---- 版本号校验(语义化 vX.Y.Z / X.Y.Z(-pre) 或日历 YYYY.MM.DD(.N)) ----
if ($Version -notmatch '^[vV]?\d+(\.\d+){1,3}([.-][0-9A-Za-z][0-9A-Za-z.-]*)?$') {
    throw "版本号非法:$Version(应为语义化 v0.1.0 / 1.2.3-rc1 或日历 2026.07.24)"
}
if ($Version -notmatch '^[A-Za-z0-9][A-Za-z0-9._-]*$') { throw "版本号含非法文件名字符:$Version" }
$verNoV = $Version -replace '^[vV]', ''   # CHANGELOG 段落用不带 v 的号

$root = Get-ArtifactRoot -Override $ArtifactRoot
$releasesDir = Join-Path $root 'releases'
if (-not (Test-Path -LiteralPath $releasesDir)) { New-Item -ItemType Directory -Path $releasesDir -Force | Out-Null }
$manifestPath = Join-Path $releasesDir "$Version.json"
$notesMdPath  = Join-Path $releasesDir "$Version.md"
if (Test-Path -LiteralPath $manifestPath) { throw "release 已存在且不可变:$manifestPath(新发布用新版本号)" }

# ---- 修复内容:-Notes > -NotesFile > CHANGELOG.md 段落 ----
function Get-ChangelogSection([string]$Path, [string]$Ver) {
    if (-not (Test-Path -LiteralPath $Path)) { return $null }
    $lines = Get-Content -LiteralPath $Path
    $out = @(); $inSec = $false
    foreach ($ln in $lines) {
        if ($ln -match '^\#\#\s') {
            if ($inSec) { break }   # 到下一个版本段,结束
            # 匹配 "## [0.1.0] ..." 或 "## 0.1.0 ..."(带不带 v 都认)
            if ($ln -match ('^\#\#\s*\[?[vV]?' + [regex]::Escape($Ver) + '[\]\s]')) { $inSec = $true }
            continue
        }
        if ($inSec) { $out += $ln }
    }
    if (-not $inSec) { return $null }
    return ($out -join "`n").Trim()
}

$resolvedNotes = $null
if ($Notes) {
    $resolvedNotes = $Notes
} elseif ($NotesFile) {
    if (-not (Test-Path -LiteralPath $NotesFile)) { throw "找不到修复内容文件:$NotesFile" }
    $resolvedNotes = (Get-Content -LiteralPath $NotesFile -Raw).Trim()
} else {
    if (-not $Changelog) { $Changelog = Join-Path $ProjectRoot 'CHANGELOG.md' }
    $resolvedNotes = Get-ChangelogSection -Path $Changelog -Ver $verNoV
    if (-not $resolvedNotes) {
        throw "CHANGELOG.md 里找不到版本 [$verNoV] 段落。先在 CHANGELOG.md 顶部新增该版本的修复内容,或用 -NotesFile/-Notes 提供。"
    }
}

# ---- 镜像离线包引用 ----
if (-not $ImagesVersion) {
    $latest = Join-Path $root 'images\latest.json'
    if (-not (Test-Path -LiteralPath $latest)) { throw '没有已发布的镜像版本,先跑 publish_offline_images.ps1。' }
    $ImagesVersion = (Get-Content -LiteralPath $latest -Raw | ConvertFrom-Json).version
}
$imagesDir = Join-Path $root "images\$ImagesVersion"
if (-not (Test-Path -LiteralPath $imagesDir)) { throw "镜像版本不存在:$imagesDir" }
Write-Host "[INFO] 校验镜像制品:$imagesDir" -ForegroundColor Cyan
Test-Sha256Sums -Dir $imagesDir
$imagesManifest = Get-Content -LiteralPath (Join-Path $imagesDir 'images-manifest.json') -Raw | ConvertFrom-Json
$imagesInfo     = Get-Content -LiteralPath (Join-Path $imagesDir 'build-info.json') -Raw | ConvertFrom-Json

# ---- UE 包引用 ----
$clientRefs = @(foreach ($rel in $ClientPackages) {
    $dir = Join-Path $root ($rel -replace '/', '\')
    if (-not (Test-Path -LiteralPath $dir)) { throw "引用的 UE 包不存在:$dir" }
    if (-not (Test-Path -LiteralPath (Join-Path $dir 'sha256sums.txt'))) { throw "UE 包缺少校验文件(非规范发布?):$dir" }
    $bi = $null
    $biPath = Join-Path $dir 'build-info.json'
    if (Test-Path -LiteralPath $biPath) { $bi = Get-Content -LiteralPath $biPath -Raw | ConvertFrom-Json }
    [pscustomobject]@{ path = $rel; build_info = $bi }
})

# ---- dirty 守卫(正规发布不应引用 dirty 来源) ----
$dirtySources = @()
if ($imagesInfo.git_dirty) { $dirtySources += "镜像 $ImagesVersion(git dirty)" }
foreach ($c in $clientRefs) { if ($c.build_info -and $c.build_info.dirty) { $dirtySources += "UE 包 $($c.path)(svn dirty)" } }
if ($dirtySources.Count -gt 0 -and -not $AllowDirty) {
    Write-Host '[ERR ] 以下制品来自 dirty 工作副本,正规发布不应引用:' -ForegroundColor Red
    $dirtySources | ForEach-Object { Write-Host "  - $_" -ForegroundColor Red }
    throw '清干净工作副本后重新出包再发布;确需用 dirty 制品(内测)显式加 -AllowDirty。'
}

# ---- 配置表 dist ----
$cfg = $null
$cfgManifest = Join-Path $ProjectRoot 'configtable/dist/manifest.json'
if (Test-Path -LiteralPath $cfgManifest) {
    $cfg = [pscustomobject]@{
        manifest_sha256 = (Get-FileHash -LiteralPath $cfgManifest -Algorithm SHA256).Hash.ToLowerInvariant()
        manifest        = (Get-Content -LiteralPath $cfgManifest -Raw | ConvertFrom-Json)
    }
}

# ---- 写 manifest(原子) ----
$release = [pscustomobject]@{
    version         = $Version
    name            = $Version
    notes           = $resolvedNotes
    dirty_sources   = $dirtySources
    created_at      = (Get-Date -Format 'o')
    machine         = $env:COMPUTERNAME
    publisher       = $env:USERNAME
    images          = [pscustomobject]@{
        version    = $ImagesVersion
        path       = "images/$ImagesVersion"
        git_sha    = $imagesInfo.git_sha
        git_dirty  = $imagesInfo.git_dirty
        image_list = $imagesManifest
    }
    client_packages = $clientRefs
    configtable     = $cfg
}
$tmp = "$manifestPath.tmp"
$release | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $tmp -Encoding utf8NoBOM
Move-Item -LiteralPath $tmp -Destination $manifestPath

# ---- 写人可读 release notes ----
$md = @()
$md += "# Pandora $Version"
$md += ""
$md += "- 发布时间: $(Get-Date -Format 'yyyy-MM-dd HH:mm')"
$md += "- 镜像版本: $ImagesVersion (git $($imagesInfo.git_sha)$(if($imagesInfo.git_dirty){' dirty'}))"
if ($clientRefs.Count -gt 0) {
    $md += "- UE 包:"
    foreach ($c in $clientRefs) { $md += "  - $($c.path)" }
}
if ($dirtySources.Count -gt 0) { $md += "- ⚠️ 含 dirty 来源制品(内测): $($dirtySources -join '; ')" }
$md += ""
$md += "## 修复内容"
$md += ""
$md += $resolvedNotes
$mdTmp = "$notesMdPath.tmp"
($md -join "`n") | Set-Content -LiteralPath $mdTmp -Encoding utf8NoBOM
Move-Item -LiteralPath $mdTmp -Destination $notesMdPath

Write-Host "[ OK ] release $Version 已生成:" -ForegroundColor Green
Write-Host "  manifest: $manifestPath"
Write-Host "  notes   : $notesMdPath"
Write-Host '[INFO] 离线交付:按 manifest 里的 path 从制品根拷对应版本目录到目标机(自带 sha256sums)。' -ForegroundColor Cyan
Write-Host "[INFO] 别忘了后端打 tag 让镜像自报版本:git tag $Version <commit> (由你/Codex 执行)。" -ForegroundColor Cyan
