<#
.SYNOPSIS
  制品目录保留策略:每条制品流保留最近 N 个版本,release 引用的版本永不删除。

.DESCRIPTION
  扫描范围:
    <制品根>\images\<版本>\
    <制品根>\client\<branch>\<flavor>\r<rev>\
  排除项:被 <制品根>\releases\*.json 引用的任何版本目录。
  默认 dry-run 只打印将删除项;确认无误后加 -Force 真删。
  建议在构建机上排期周跑(例如 Windows 计划任务每周一次)。

.EXAMPLE
  pwsh tools/scripts/artifacts_retention.ps1                # 预览
  pwsh tools/scripts/artifacts_retention.ps1 -KeepLast 5 -Force
#>
[CmdletBinding()]
param(
    [int]$KeepLast = 10,     # 每条制品流(images / 每个 client flavor)保留的最近版本数
    [switch]$Force,          # 真删;缺省 dry-run
    [string]$ArtifactRoot
)

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'artifacts_lib.ps1')
$root = Get-ArtifactRoot -Override $ArtifactRoot

if ($KeepLast -lt 1) { throw '-KeepLast 至少为 1。' }

# ---- 收集 release 引用(相对路径,'/'分隔) ----
$referenced = New-Object 'System.Collections.Generic.HashSet[string]'
$releasesDir = Join-Path $root 'releases'
if (Test-Path -LiteralPath $releasesDir) {
    foreach ($f in Get-ChildItem -LiteralPath $releasesDir -Filter '*.json' -File) {
        $rel = Get-Content -LiteralPath $f.FullName -Raw | ConvertFrom-Json
        if ($rel.images.path) { [void]$referenced.Add([string]$rel.images.path) }
        foreach ($cp in @($rel.client_packages)) {
            if ($cp.path) { [void]$referenced.Add([string]$cp.path) }
        }
    }
}

# ---- 组装各制品流的版本目录清单 ----
$streams = @()
$imagesRoot = Join-Path $root 'images'
if (Test-Path -LiteralPath $imagesRoot) {
    $streams += ,@(Get-ChildItem -LiteralPath $imagesRoot -Directory | Where-Object { $_.Name -notmatch '^\.tmp-' })
}
$clientRoot = Join-Path $root 'client'
if (Test-Path -LiteralPath $clientRoot) {
    foreach ($branch in Get-ChildItem -LiteralPath $clientRoot -Directory) {
        foreach ($flavor in Get-ChildItem -LiteralPath $branch.FullName -Directory) {
            $streams += ,@(Get-ChildItem -LiteralPath $flavor.FullName -Directory | Where-Object { $_.Name -notmatch '^\.tmp-' })
        }
    }
}

$toDelete = @()
foreach ($vers in $streams) {
    if (-not $vers -or $vers.Count -le $KeepLast) { continue }
    $sorted = $vers | Sort-Object LastWriteTimeUtc -Descending
    foreach ($old in ($sorted | Select-Object -Skip $KeepLast)) {
        $relPath = ([System.IO.Path]::GetRelativePath($root, $old.FullName)) -replace '\\', '/'
        if ($referenced.Contains($relPath)) {
            Write-Host "[KEEP] $relPath(被 release 引用)" -ForegroundColor Yellow
            continue
        }
        $toDelete += $old
    }
}

if ($toDelete.Count -eq 0) { Write-Host '[ OK ] 没有需要清理的版本。' -ForegroundColor Green; exit 0 }

foreach ($d in $toDelete) {
    $relPath = ([System.IO.Path]::GetRelativePath($root, $d.FullName)) -replace '\\', '/'
    if ($Force) {
        Remove-Item -LiteralPath $d.FullName -Recurse -Force
        Write-Host "[DEL ] $relPath" -ForegroundColor Red
    } else {
        Write-Host "[DRY ] 将删除:$relPath(加 -Force 执行)" -ForegroundColor Cyan
    }
}
Write-Host ("[ OK ] {0} 个过期版本{1}。" -f $toDelete.Count, ($(if ($Force) { '已删除' } else { '待删除(dry-run)' }))) -ForegroundColor Green
