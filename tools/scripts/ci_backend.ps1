<#
.SYNOPSIS
  后端 CI 构建入口:按 go.work 的 use 清单逐模块 go build + go test。

.DESCRIPTION
  供 Jenkins(仓库根 Jenkinsfile)或本机手工调用。不做镜像构建/发布 —— 那是
  publish_offline_images.ps1 的职责,由流水线在测试全绿后单独调用。
  任何模块失败立即整体失败,不吞错(AGENTS.md §8)。

.EXAMPLE
  pwsh tools/scripts/ci_backend.ps1
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$ProjectRoot = (Resolve-Path "$PSScriptRoot/../..").Path

# ---- 解析 go.work 的 use 清单(支持单行 use 与 use ( ... ) 块) ----
$goWork = Join-Path $ProjectRoot 'go.work'
if (-not (Test-Path -LiteralPath $goWork)) { throw "找不到 go.work:$goWork" }
$modules = @()
$inBlock = $false
foreach ($line in Get-Content -LiteralPath $goWork) {
    $t = ($line -replace '//.*$', '').Trim()
    if (-not $t) { continue }
    if ($t -match '^use\s*\($') { $inBlock = $true; continue }
    if ($inBlock) {
        if ($t -eq ')') { $inBlock = $false; continue }
        $modules += $t
        continue
    }
    if ($t -match '^use\s+(\S+)$') { $modules += $Matches[1] }
}
if ($modules.Count -eq 0) { throw 'go.work 未解析到任何 use 模块。' }

Write-Host "[INFO] go.work 模块数:$($modules.Count)" -ForegroundColor Cyan
$goVersion = (go env GOVERSION 2>$null | Out-String).Trim()
Write-Host "[INFO] Go:$goVersion" -ForegroundColor Cyan

$failed = @()
foreach ($m in $modules) {
    $dir = Join-Path $ProjectRoot ($m -replace '^\./', '' -replace '/', '\')
    if (-not (Test-Path -LiteralPath $dir)) { $failed += "$m(目录不存在)"; continue }
    Write-Host "`n===== $m =====" -ForegroundColor Magenta
    Push-Location $dir
    try {
        go build ./...
        if ($LASTEXITCODE -ne 0) { $failed += "$m(build)"; continue }
        go test ./... -count=1
        if ($LASTEXITCODE -ne 0) { $failed += "$m(test)"; continue }
    } finally { Pop-Location }
}

if ($failed.Count -gt 0) {
    Write-Host "`n[ERR ] 以下模块未通过:" -ForegroundColor Red
    $failed | ForEach-Object { Write-Host "  - $_" -ForegroundColor Red }
    exit 1
}
Write-Host "`n[ OK ] 全部 $($modules.Count) 个模块 build + test 通过。" -ForegroundColor Green
