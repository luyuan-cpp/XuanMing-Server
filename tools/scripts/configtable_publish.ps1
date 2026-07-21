# Pandora 配置表发布脚本(docs/design/config-table-hotreload.md §4/§6/§10.5-6)
#
# 职责:把 configtable/dist 的一批产物发布到服务端运行态目录:
#   dist → <DeployRoot>\configtable\staging(先落地,不碰线上)
#        → 逐表 sha256 校验 + 版本单调检查
#        → 旧 active 归档 history\v<版本> → staging 原子改名为 active
#   服务端进程随后经 ConfigTableAdminService.ReloadConfigTable 热加载 active
#   (加载成功才切内存指针,失败保留旧表;本脚本只负责文件面)。
#
# 用法:
#   pwsh tools/scripts/configtable_publish.ps1 -DeployRoot D:\pandora-deploy
#   pwsh tools/scripts/configtable_publish.ps1 -DeployRoot D:\pandora-deploy -ReloadAddr 127.0.0.1:50011
#
# 幂等 / 容错:
#   - staging 版本 == active 版本 → no-op 退出 0;
#   - staging 版本 <  active 版本 → 拒绝(防回退;回滚 = 重新生成更高版本);
#   - 两次改名之间进程崩溃:重跑本脚本即可恢复(active 缺失时 staging 直接补位)。

param(
    [Parameter(Mandatory = $true)][string]$DeployRoot,
    [string]$DistDir = "",
    [string]$ReloadAddr = ""   # 可选:matchmaker 等服务的 gRPC 地址,发布后用 grpcurl 触发热更
)

$ErrorActionPreference = "Stop"

$ProjectRoot = Resolve-Path "$PSScriptRoot/../.."
if ($DistDir -eq "") { $DistDir = Join-Path $ProjectRoot "configtable/dist" }

$ctRoot   = Join-Path $DeployRoot "configtable"
$staging  = Join-Path $ctRoot "staging"
$active   = Join-Path $ctRoot "active"
$history  = Join-Path $ctRoot "history"

function Read-Manifest([string]$dir) {
    $path = Join-Path $dir "manifest.json"
    if (-not (Test-Path $path)) { return $null }
    return Get-Content $path -Raw -Encoding UTF8 | ConvertFrom-Json
}

# 1. dist 检查
$distManifest = Read-Manifest $DistDir
if ($null -eq $distManifest) { Write-Host "[ERR] $DistDir 缺少 manifest.json(先跑 go run ./tools/configtable-gen)" -ForegroundColor Red; exit 1 }
$newVersion = [uint64]$distManifest.version
Write-Host "dist 批次 version = $newVersion($($distManifest.tables.Count) 张表)"

# 2. dist → staging(先清后拷,staging 永远是完整一批)
New-Item -ItemType Directory -Force $ctRoot | Out-Null
if (Test-Path $staging) { Remove-Item -Recurse -Force $staging }
Copy-Item -Recurse $DistDir $staging

# 3. staging 逐表 sha256 校验(防拷贝截断)
$stagingManifest = Read-Manifest $staging
foreach ($t in $stagingManifest.tables) {
    $f = Join-Path $staging $t.file
    if (-not (Test-Path $f)) { Write-Host "[ERR] staging 缺文件 $($t.file)" -ForegroundColor Red; exit 1 }
    $got = "sha256:" + (Get-FileHash $f -Algorithm SHA256).Hash.ToLower()
    if ($got -ne $t.checksum) {
        Write-Host "[ERR] $($t.file) checksum 不匹配`n  声明 $($t.checksum)`n  实际 $got" -ForegroundColor Red
        exit 1
    }
}
Write-Host "staging 校验通过($($stagingManifest.tables.Count) 张表)"

# 4. 版本单调检查
$activeManifest = Read-Manifest $active
if ($null -ne $activeManifest) {
    $activeVersion = [uint64]$activeManifest.version
    if ($newVersion -eq $activeVersion) {
        Write-Host "active 已是 version $activeVersion,无需发布(no-op)" -ForegroundColor Yellow
        Remove-Item -Recurse -Force $staging
        exit 0
    }
    if ($newVersion -lt $activeVersion) {
        Write-Host "[ERR] dist 版本 $newVersion 低于 active $activeVersion,拒绝回退(回滚请重新生成更高版本)" -ForegroundColor Red
        exit 1
    }
    # 5. 归档旧 active → history\v<版本>
    New-Item -ItemType Directory -Force $history | Out-Null
    $slot = Join-Path $history "v$activeVersion"
    if (Test-Path $slot) { Remove-Item -Recurse -Force $slot }
    Move-Item $active $slot
    Write-Host "旧批次 v$activeVersion 已归档 → $slot"
}

# 6. staging → active(同卷改名,近原子;两步间崩溃重跑本脚本即恢复)
Move-Item $staging $active
Write-Host "发布完成:active = version $newVersion" -ForegroundColor Green

# 7. 触发热更(可选;也可由运维手动调 ReloadConfigTable)
if ($ReloadAddr -ne "") {
    $grpcurl = Get-Command grpcurl -ErrorAction SilentlyContinue
    if ($null -eq $grpcurl) {
        Write-Host "[WARN] 未安装 grpcurl,请手动触发 reload:" -ForegroundColor Yellow
    } else {
        & grpcurl -plaintext -d "{`"expect_version`": $newVersion}" $ReloadAddr pandora.config.v1.ConfigTableAdminService/ReloadConfigTable
        if ($LASTEXITCODE -eq 0) { exit 0 }
        Write-Host "[WARN] reload 调用失败,请手动触发:" -ForegroundColor Yellow
    }
    Write-Host "  grpcurl -plaintext -d '{\"expect_version\": $newVersion}' $ReloadAddr pandora.config.v1.ConfigTableAdminService/ReloadConfigTable"
}
