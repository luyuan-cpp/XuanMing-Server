<#
.SYNOPSIS
  在「拉不到镜像」的目标机上,把 export_images.ps1 打好的 tar 离线导入 Docker。

.DESCRIPTION
  配合 export_images.ps1 使用:
    1) 本机(能联网):pwsh tools/scripts/export_images.ps1 -Build  → 得到 pandora-images.tar
    2) 拷 tar 到目标机
    3) 目标机(本脚本):pwsh tools/scripts/import_images.ps1 -In <tar 路径>
    4) 目标机双击「策划一键启动-含战斗.cmd」即可(镜像已在本地,不再联网拉)

.EXAMPLE
  pwsh tools/scripts/import_images.ps1 -In D:\pandora-images.tar
  pwsh tools/scripts/import_images.ps1                 # 不传 -In 时默认找 deploy/offline-images/pandora-images.tar
#>
[CmdletBinding()]
param(
    [string]$In   # tar 路径(默认 <仓库根>/deploy/offline-images/pandora-images.tar)
)

$ErrorActionPreference = 'Stop'
$ScriptDir   = $PSScriptRoot
$ProjectRoot = (Resolve-Path "$ScriptDir/../..").Path

function Write-Info($m) { Write-Host "[INFO] $m" -ForegroundColor Cyan }
function Write-Ok($m)   { Write-Host "[ OK ] $m" -ForegroundColor Green }
function Write-Err($m)  { Write-Host "[ERR ] $m" -ForegroundColor Red }
function Write-Step($m) { Write-Host "`n===== $m =====" -ForegroundColor Magenta }

if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    Write-Err "未找到 docker,请先安装 Docker Desktop 并启动。"
    exit 1
}

if (-not $In) {
    $In = Join-Path $ProjectRoot 'deploy/offline-images/pandora-images.tar'
}
if (-not (Test-Path $In)) {
    Write-Err "找不到镜像包:$In"
    Write-Err "请确认已把本机 export_images.ps1 生成的 pandora-images.tar 拷到此路径,或用 -In 指定。"
    exit 1
}

$sizeMB = [math]::Round((Get-Item $In).Length / 1MB, 1)
Write-Step "离线导入镜像:$In(${sizeMB} MB,可能几分钟)"
docker load -i $In
if ($LASTEXITCODE -ne 0) { throw "docker load 失败。" }

Write-Ok "导入完成。当前 pandora/* 镜像:"
docker images "pandora/*"
Write-Host ""
Write-Info "接着双击「策划一键启动-含战斗.cmd」即可(镜像已在本地,不会再联网拉业务镜像)。"
