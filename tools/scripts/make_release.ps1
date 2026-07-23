<#
.SYNOPSIS
  生成 release manifest:一次发布"到底发的是什么"的唯一事实清单(build once, promote many)。

.DESCRIPTION
  在 <制品根>\releases\<名称>.json 记录本次发布引用的全部制品版本:
    - 业务镜像离线包版本(images\<版本>,含每镜像 image_id)
    - UE 包(client\<branch>\<flavor>\r<rev>,由客户端仓 PublishPackages.ps1 发布;可选)
    - 配置表 dist manifest 摘要(configtable/dist/manifest.json;存在即记录)
  manifest 本身不可变(重名拒绝);被任何 release 引用的制品版本,保留清理脚本永不删除。
  离线交付 = 按本 manifest 从制品目录取对应版本拷走,不再把 tar 常驻版本库。

.EXAMPLE
  # 引用最新镜像版本出一份 release
  pwsh tools/scripts/make_release.ps1 -Name v0.1.0

  # 显式指定镜像版本 + 关联 UE 包
  pwsh tools/scripts/make_release.ps1 -Name v0.1.0 -ImagesVersion g1a2b3c4d5e6 `
      -ClientPackages 'client/trunk/Client_Win64_Development/r1343','client/trunk/Server_Linux_Shipping/r1343'
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$Name,   # 发布名,如 v0.1.0;只允许字母数字点横线
    [string]$ImagesVersion,                # 缺省取 images\latest.json
    [string[]]$ClientPackages = @(),       # 制品根下的相对路径(client/<branch>/<flavor>/r<rev>)
    [string]$ArtifactRoot
)

$ErrorActionPreference = 'Stop'
$ScriptDir   = $PSScriptRoot
$ProjectRoot = (Resolve-Path "$ScriptDir/../..").Path
. (Join-Path $ScriptDir 'artifacts_lib.ps1')

if ($Name -notmatch '^[A-Za-z0-9][A-Za-z0-9._-]*$') { throw "发布名非法:$Name" }
$root = Get-ArtifactRoot -Override $ArtifactRoot

$releasesDir = Join-Path $root 'releases'
if (-not (Test-Path -LiteralPath $releasesDir)) { New-Item -ItemType Directory -Path $releasesDir -Force | Out-Null }
$manifestPath = Join-Path $releasesDir "$Name.json"
if (Test-Path -LiteralPath $manifestPath) { throw "release 已存在且不可变:$manifestPath(新发布用新名称)" }

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

# ---- UE 包引用(存在性 + 版本目录内 build-info 摘要;不重算全量哈希,发布时已有 sha256sums) ----
$clientRefs = @(foreach ($rel in $ClientPackages) {
    $dir = Join-Path $root ($rel -replace '/', '\')
    if (-not (Test-Path -LiteralPath $dir)) { throw "引用的 UE 包不存在:$dir" }
    if (-not (Test-Path -LiteralPath (Join-Path $dir 'sha256sums.txt'))) { throw "UE 包缺少校验文件(非规范发布?):$dir" }
    $bi = $null
    $biPath = Join-Path $dir 'build-info.json'
    if (Test-Path -LiteralPath $biPath) { $bi = Get-Content -LiteralPath $biPath -Raw | ConvertFrom-Json }
    [pscustomobject]@{ path = $rel; build_info = $bi }
})

# ---- 配置表 dist(部署契约,入库文件;记录其 manifest 摘要便于对账) ----
$cfg = $null
$cfgManifest = Join-Path $ProjectRoot 'configtable/dist/manifest.json'
if (Test-Path -LiteralPath $cfgManifest) {
    $cfg = [pscustomobject]@{
        manifest_sha256 = (Get-FileHash -LiteralPath $cfgManifest -Algorithm SHA256).Hash.ToLowerInvariant()
        manifest        = (Get-Content -LiteralPath $cfgManifest -Raw | ConvertFrom-Json)
    }
}

# ---- 写 manifest(先临时名再改名,原子) ----
$release = [pscustomobject]@{
    name            = $Name
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
if ($imagesInfo.git_dirty) {
    Write-Host '[WARN] 引用的镜像版本来自脏工作区构建,正式发布不应使用 dirty 版本。' -ForegroundColor Yellow
}
$tmp = "$manifestPath.tmp"
$release | ConvertTo-Json -Depth 10 | Set-Content -LiteralPath $tmp -Encoding utf8NoBOM
Move-Item -LiteralPath $tmp -Destination $manifestPath

Write-Host "[ OK ] release manifest 已生成:$manifestPath" -ForegroundColor Green
Write-Host '[INFO] 离线交付:按 manifest 里的 path 从制品根拷对应版本目录到目标机即可(自带 sha256sums)。' -ForegroundColor Cyan
