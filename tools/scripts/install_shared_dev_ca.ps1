<#
.SYNOPSIS
  把「全队共享的 dev 根 CA」装进本机 mkcert,让本机签出的 Envoy 叶子证书都出自同一个 CA。
  目的:策划客户端只需信任这一个共享 CA 一次,连任意服务器(A/B/C…)都不用再导证书。

.DESCRIPTION
  问题:mkcert 默认每台机器各自生成一个独立根 CA,所以服务器换一台、客户端就得重新信任一次。
  解法:全队用同一个 dev 根 CA——
    - 公开 CA 证书(pandora-dev-rootCA.pem)入库,所有人可拿(deploy/dev-ca/,不含私钥);
    - CA 私钥(rootCA-key.pem)绝不入库(AGENTS.md 红线),由签发机线下分发到每台服务器机器。
  本脚本(幂等)把「公开 CA + 私钥」放进本机 mkcert 的 CAROOT,并 mkcert -install 加入系统信任。
  之后本机跑 dev_up.ps1 / play.ps1 自动签发的 Envoy 证书,就都由这个共享 CA 签。

  ⚠️ 仅限内网开发期。生产(外网/玩家)用公网 CA 签真实域名证书,玩家零配置,绝不用这个 dev CA。

.PARAMETER CaKeyPath
  共享 CA 私钥 rootCA-key.pem 的路径(线下分发,不在仓库)。必填。

.PARAMETER CaCertPath
  共享 CA 公开证书路径。默认 deploy/dev-ca/pandora-dev-rootCA.pem(仓库内)。

.PARAMETER Install
  装好 CAROOT 后是否运行 mkcert -install 加入系统信任(默认开;-Install:$false 跳过)。

.EXAMPLE
  pwsh tools/scripts/install_shared_dev_ca.ps1 -CaKeyPath \\nas\pandora-dev-ca\rootCA-key.pem
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string]$CaKeyPath,
    [string]$CaCertPath = '',
    [switch]$Install = $true
)

$ErrorActionPreference = 'Stop'
$ScriptDir   = $PSScriptRoot
$ProjectRoot = (Resolve-Path "$ScriptDir/../..").Path

. "$ScriptDir/envoy_cert.ps1"  # 复用 Test-PemFile

function Write-Info($m) { Write-Host "[INFO] $m" -ForegroundColor Cyan }
function Write-Ok($m)   { Write-Host "[ OK ] $m" -ForegroundColor Green }
function Write-Err($m)  { Write-Host "[ERR] $m" -ForegroundColor Red }

# --- 1. 定位公开 CA 证书 ---
if ([string]::IsNullOrEmpty($CaCertPath)) {
    $CaCertPath = Join-Path $ProjectRoot 'deploy/dev-ca/pandora-dev-rootCA.pem'
}
if (-not (Test-PemFile -Path $CaCertPath -BeginPattern 'BEGIN CERTIFICATE')) {
    Write-Err "共享 CA 公开证书无效或不存在:$CaCertPath"
    exit 1
}

# --- 2. 校验共享 CA 私钥(线下分发,不入库)---
if (-not (Test-PemFile -Path $CaKeyPath -BeginPattern 'BEGIN (RSA |EC )?PRIVATE KEY')) {
    Write-Err "共享 CA 私钥无效或不存在:$CaKeyPath"
    Write-Info '该私钥不入库,需由签发机线下分发(U盘/内网共享)。'
    exit 1
}

# --- 3. 确认 mkcert ---
$mkcert = Get-Command mkcert -ErrorAction SilentlyContinue
if (-not $mkcert) {
    Write-Err '未找到 mkcert。先装:winget install FiloSottile.mkcert'
    exit 1
}

# --- 4. 定位本机 mkcert CAROOT ---
$caRoot = (& mkcert -CAROOT 2>$null).Trim()
if ([string]::IsNullOrEmpty($caRoot)) {
    Write-Err '无法获取 mkcert CAROOT(mkcert -CAROOT 无输出)。'
    exit 1
}
New-Item -ItemType Directory -Force -Path $caRoot | Out-Null
Write-Info "mkcert CAROOT:$caRoot"

$destCert = Join-Path $caRoot 'rootCA.pem'
$destKey  = Join-Path $caRoot 'rootCA-key.pem'

# --- 5. 备份本机原有 CA(若与共享 CA 不同,避免静默覆盖)---
foreach ($p in @($destCert, $destKey)) {
    if (Test-Path $p) {
        $bak = "$p.bak"
        if (-not (Test-Path $bak)) {
            Copy-Item $p $bak -Force
            Write-Info "已备份原有:$p -> $bak"
        }
    }
}

# --- 6. 写入共享 CA(公开证书 + 私钥)到 CAROOT ---
Copy-Item $CaCertPath $destCert -Force
Copy-Item $CaKeyPath  $destKey  -Force
Write-Ok "已安装共享 CA 到本机 CAROOT。"

# --- 7. 加入系统信任 ---
if ($Install) {
    Write-Info '运行 mkcert -install(把共享 CA 加入本机系统信任,可能弹管理员确认)...'
    & mkcert -install
    if ($LASTEXITCODE -ne 0) {
        Write-Err "mkcert -install 失败(exit=$LASTEXITCODE)。"
        exit 1
    }
    Write-Ok 'mkcert -install 完成。'
}

Write-Host ''
Write-Ok '共享 dev CA 已就绪。下一步:'
Write-Host '  1) 让本机重新签发 Envoy 证书(改用共享 CA + 本机局域网 IP):' -ForegroundColor Green
Write-Host "       Remove-Item '$ProjectRoot/deploy/envoy/cert.pem','$ProjectRoot/deploy/envoy/key.pem' -ErrorAction SilentlyContinue" -ForegroundColor Green
Write-Host '       然后正常启动(dev_up.ps1 / play.ps1 会自动用共享 CA 重签)。' -ForegroundColor Green
Write-Host '  2) 客户端只需对这个共享 CA 跑一次 import_dev_ca.ps1,之后连任意服务器都不用再改。' -ForegroundColor Green
