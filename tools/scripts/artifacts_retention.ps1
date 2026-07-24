<#
.SYNOPSIS
  制品保留策略:只清理 dev 快照(snapshots\),每条流保留最近 N 个;releases\ 永不触碰。

.DESCRIPTION
  两轨分仓后,发布版本(releases\)是不可变永久制品,由本脚本**完全不碰**;
  只对 dev 快照做滚动清理:
    <制品根>\snapshots\images\<gitsha>\
    <制品根>\snapshots\client\<branch>\<flavor>\r<rev>\
  每条流按修改时间保留最近 -KeepLast 个,其余删除。默认 dry-run,加 -Force 真删。
  建议构建机排期周跑(Windows 计划任务)。

.EXAMPLE
  pwsh tools/scripts/artifacts_retention.ps1                # 预览
  pwsh tools/scripts/artifacts_retention.ps1 -KeepLast 5 -Force
#>
[CmdletBinding()]
param(
    [int]$KeepLast = 10,     # 每条快照流保留的最近版本数
    [switch]$Force,          # 真删;缺省 dry-run
    [string]$ArtifactRoot
)

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'artifacts_lib.ps1')
if ($KeepLast -lt 1) { throw '-KeepLast 至少为 1。' }

$snapRoot = Get-ChannelRoot -Override $ArtifactRoot -Channel 'snapshot'   # <root>\snapshots

# ---- 组装快照流(每条流各自保留最近 N) ----
$streams = @()
$imagesRoot = Join-Path $snapRoot 'images'
if (Test-Path -LiteralPath $imagesRoot) {
    $streams += ,@(Get-ChildItem -LiteralPath $imagesRoot -Directory | Where-Object { $_.Name -notmatch '^\.tmp-' })
}
$clientRoot = Join-Path $snapRoot 'client'
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
    $toDelete += ($sorted | Select-Object -Skip $KeepLast)
}

if ($toDelete.Count -eq 0) { Write-Host '[ OK ] 快照无需清理(releases\ 永不触碰)。' -ForegroundColor Green; exit 0 }

foreach ($d in $toDelete) {
    $rel = ([System.IO.Path]::GetRelativePath((Split-Path $snapRoot -Parent), $d.FullName)) -replace '\\', '/'
    if ($Force) {
        Remove-Item -LiteralPath $d.FullName -Recurse -Force
        Write-Host "[DEL ] $rel" -ForegroundColor Red
    } else {
        Write-Host "[DRY ] 将删除:$rel(加 -Force 执行)" -ForegroundColor Cyan
    }
}
Write-Host ("[ OK ] {0} 个过期快照{1};releases\ 未触碰。" -f $toDelete.Count, ($(if ($Force) { '已删除' } else { '待删除(dry-run)' }))) -ForegroundColor Green
