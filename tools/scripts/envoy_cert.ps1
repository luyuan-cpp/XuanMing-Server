# Pandora 本地开发 Envoy TLS 证书校验 / 自愈(共享库,dot-source 引入)
#
# 背景:
#   Envoy 把 deploy/envoy/cert.pem + key.pem 挂到 /etc/envoy 起 TLS 监听器。
#   这两个文件是 mkcert 本机生成、被 .gitignore / .dockerignore 屏蔽,不随仓库同步
#   (私钥绝不入库,AGENTS.md 红线)。换机器 / 文件损坏会让 Envoy 启动直接退出:
#       Failed to load incomplete private key from path: /etc/envoy/key.pem
#
# 本库做一件事(幂等):启动前校验本机 dev 证书,缺失 / 无效就用 mkcert 自动补齐。
#   - 不把 cert.pem / key.pem 加入 Git,不改 .gitignore。
#   - 不跑 `mkcert -install`(那是系统信任库改动、需管理员);只生成叶子证书让 Envoy 能起。
#     UE 客户端要信任该证书时,另跑 mkcert -install + tools/scripts/import_dev_ca.ps1。
#
# 用法(在调用方脚本顶部):
#   . "$PSScriptRoot/envoy_cert.ps1"
#   Confirm-EnvoyDevCert -EnvoyDir (Join-Path $ProjectRoot 'deploy/envoy')

# 校验单个 PEM 文件:存在 + 非空 + 含指定 BEGIN 标记。返回 $true/$false。
function Test-PemFile {
    param(
        [Parameter(Mandatory)] [string]$Path,
        [Parameter(Mandatory)] [string]$BeginPattern  # 正则,如 'BEGIN CERTIFICATE'
    )
    if (-not (Test-Path $Path)) { return $false }
    $item = Get-Item $Path
    if ($item.Length -le 0) { return $false }
    $content = Get-Content $Path -Raw -ErrorAction SilentlyContinue
    if ([string]::IsNullOrEmpty($content)) { return $false }
    return ($content -match $BeginPattern)
}

# 收集本机所有「私网局域网 IPv4」(192.168.* / 10.* / 172.16-31.*),
# 排除回环 127.* 和链路本地 169.254.*。策划跨机器用 IP 连服务器时,证书 SAN 必须含该 IP,
# 否则即使信任了 CA,主机名/IP 对不上仍 TLS 校验失败。返回字符串数组(可能为空)。
function Get-LanIPv4 {
    try {
        $addrs = Get-NetIPAddress -AddressFamily IPv4 -ErrorAction Stop |
            Where-Object {
                $_.IPAddress -notlike '127.*' -and
                $_.IPAddress -notlike '169.254.*' -and
                (
                    $_.IPAddress -like '192.168.*' -or
                    $_.IPAddress -like '10.*' -or
                    $_.IPAddress -match '^172\.(1[6-9]|2[0-9]|3[01])\.'
                )
            } |
            Select-Object -ExpandProperty IPAddress -Unique
        return @($addrs)
    } catch {
        return @()
    }
}

# 组装签发叶子证书用的 SAN 列表:固定基础项 + 本机局域网 IP(自动)。
function Get-EnvoyCertSanHosts {
    $base = @('localhost', '127.0.0.1', 'host.docker.internal', '::1')
    $lan  = Get-LanIPv4
    return @($base + $lan | Select-Object -Unique)
}

# 读取 PEM 证书指纹(Thumbprint),用于判断本机 mkcert CAROOT 里的根 CA 是否就是全队共享 CA。
# 读不出返回 $null。
function Get-CertThumbprint {
    param([Parameter(Mandatory)] [string]$Path)
    try {
        if (-not (Test-Path $Path)) { return $null }
        $cert = [System.Security.Cryptography.X509Certificates.X509Certificate2]::new((Resolve-Path $Path).Path)
        return $cert.Thumbprint
    } catch {
        return $null
    }
}

# 确保本机 mkcert 使用「全队共享 dev CA」(而非各机器独立 CA)。幂等,且绝不阻塞启动:
#   - 本机 CAROOT 的根 CA 已等于共享 CA        -> 静默通过;
#   - 未装,但能在约定位置找到共享 CA 私钥      -> 自动调用 install_shared_dev_ca.ps1 装好,
#                                                 并清掉旧叶子证书让后续用共享 CA 重签;
#   - 未装且找不到私钥                          -> 打印一行提示(退回本机独立 CA),返回 $false,不报错。
# 私钥查找顺序:-SharedCaKeyPath 参数 > 环境变量 PANDORA_DEV_CA_KEY > deploy/dev-ca/local/rootCA-key.pem。
# 返回 $true 表示本机已就绪为共享 CA。
function Confirm-SharedDevCa {
    param(
        [Parameter(Mandatory)] [string]$ProjectRoot,
        [string]$SharedCaCertPath = '',
        [string]$SharedCaKeyPath  = ''
    )

    if ([string]::IsNullOrEmpty($SharedCaCertPath)) {
        $SharedCaCertPath = Join-Path $ProjectRoot 'deploy/dev-ca/pandora-dev-rootCA.pem'
    }
    # 仓库未提供共享 CA 公开证书 -> 没有共享 CA 这回事,走各机器独立 CA 老路。
    if (-not (Test-PemFile -Path $SharedCaCertPath -BeginPattern 'BEGIN CERTIFICATE')) {
        return $false
    }

    $mkcert = Get-Command mkcert -ErrorAction SilentlyContinue
    if (-not $mkcert) { return $false }  # 没 mkcert,Confirm-EnvoyDevCert 会给安装指引

    $caRoot = (& mkcert -CAROOT 2>$null)
    if ($caRoot) { $caRoot = $caRoot.Trim() }
    if ([string]::IsNullOrEmpty($caRoot)) { return $false }
    $installedCaCert = Join-Path $caRoot 'rootCA.pem'

    # 本机 CAROOT 根 CA 指纹 == 共享 CA 指纹 -> 已就绪,静默通过。
    $want = Get-CertThumbprint -Path $SharedCaCertPath
    $have = Get-CertThumbprint -Path $installedCaCert
    if ($want -and $have -and ($want -eq $have)) {
        Write-Host "[ OK ] 本机 mkcert 已是全队共享 dev CA。" -ForegroundColor Green
        return $true
    }

    # 未装共享 CA -> 找私钥(线下分发,约定位置)。
    if ([string]::IsNullOrEmpty($SharedCaKeyPath)) {
        $candidates = @()
        if (-not [string]::IsNullOrEmpty($env:PANDORA_DEV_CA_KEY)) { $candidates += $env:PANDORA_DEV_CA_KEY }
        $candidates += (Join-Path $ProjectRoot 'deploy/dev-ca/local/rootCA-key.pem')
        foreach ($c in $candidates) {
            if (Test-PemFile -Path $c -BeginPattern 'BEGIN (RSA |EC )?PRIVATE KEY') {
                $SharedCaKeyPath = $c
                break
            }
        }
    }

    $keyOk = (-not [string]::IsNullOrEmpty($SharedCaKeyPath)) -and
             (Test-PemFile -Path $SharedCaKeyPath -BeginPattern 'BEGIN (RSA |EC )?PRIVATE KEY')
    if (-not $keyOk) {
        # 找不到私钥 -> 提示但不阻塞,本机继续用现有(独立)CA。
        Write-Host "[INFO] 未安装全队共享 dev CA,本机将用独立 CA 签发(客户端需对本机单独导一次 CA)。" -ForegroundColor Yellow
        Write-Host "       想「客户端导一次连所有服务器」,把共享 CA 私钥放到下列任一位置后重跑启动:" -ForegroundColor Yellow
        Write-Host "         - 设环境变量 PANDORA_DEV_CA_KEY 指向 rootCA-key.pem" -ForegroundColor Yellow
        Write-Host "         - 放到 $ProjectRoot/deploy/dev-ca/local/rootCA-key.pem(该目录已 gitignore,私钥不会入库)" -ForegroundColor Yellow
        return $false
    }

    # 找到私钥 -> 自动安装共享 CA。子进程调用,隔离其内部 exit,失败不拖垮启动脚本。
    Write-Host "[INFO] 检测到共享 dev CA 私钥,自动安装到本机 mkcert..." -ForegroundColor Cyan
    $installScript = Join-Path $PSScriptRoot 'install_shared_dev_ca.ps1'
    & pwsh -NoProfile -ExecutionPolicy Bypass -File $installScript -CaKeyPath $SharedCaKeyPath -CaCertPath $SharedCaCertPath
    if ($LASTEXITCODE -ne 0) {
        Write-Host "[WARN] 共享 CA 自动安装失败(exit=$LASTEXITCODE),退回本机独立 CA。" -ForegroundColor Yellow
        return $false
    }

    # 换了根 CA -> 作废旧叶子证书,强制后续用共享 CA 重签(SAN 仍含本机局域网 IP)。
    foreach ($p in @((Join-Path $ProjectRoot 'deploy/envoy/cert.pem'), (Join-Path $ProjectRoot 'deploy/envoy/key.pem'))) {
        if (Test-Path $p) { Remove-Item $p -Force -ErrorAction SilentlyContinue }
    }
    Write-Host "[ OK ] 已安装全队共享 dev CA,并清除旧叶子证书(稍后自动重签)。" -ForegroundColor Green
    return $true
}

# 校验 Envoy dev 证书对(cert.pem + key.pem);缺失 / 无效则用 mkcert 自动重生。
# 校验失败且 mkcert 不可用时,抛出带明确修复指引的异常(由调用方决定退出)。
function Confirm-EnvoyDevCert {
    param(
        [Parameter(Mandatory)] [string]$EnvoyDir
    )

    $certPath = Join-Path $EnvoyDir 'cert.pem'
    $keyPath  = Join-Path $EnvoyDir 'key.pem'

    # cert 必含 BEGIN CERTIFICATE;key 必含 BEGIN [RSA/EC] PRIVATE KEY。
    $certOk = Test-PemFile -Path $certPath -BeginPattern 'BEGIN CERTIFICATE'
    $keyOk  = Test-PemFile -Path $keyPath  -BeginPattern 'BEGIN (RSA |EC )?PRIVATE KEY'

    if ($certOk -and $keyOk) {
        Write-Host "[ OK ] Envoy dev 证书有效:$certPath" -ForegroundColor Green
        return
    }

    # 报清楚到底哪个文件坏了(便于排障)。
    $reason = @()
    if (-not $certOk) { $reason += "cert.pem 缺失/空/非 PEM 证书" }
    if (-not $keyOk)  { $reason += "key.pem 缺失/空/非 PEM 私钥" }
    Write-Host "[WARN] Envoy dev 证书无效:$($reason -join '; ')" -ForegroundColor Yellow

    $sanHosts = Get-EnvoyCertSanHosts
    Write-Host "[INFO] 证书 SAN 将包含:$($sanHosts -join ' ')" -ForegroundColor Cyan

    $mkcert = Get-Command mkcert -ErrorAction SilentlyContinue
    if (-not $mkcert) {
        $sanLine = $sanHosts -join ' '
        $msg = @"
Envoy 本地 TLS 证书无效,且未找到 mkcert,无法自动修复。
原因:$($reason -join '; ')
证书目录:$EnvoyDir

请先装 mkcert(任选其一):
    winget install FiloSottile.mkcert
    choco install mkcert

然后生成本机 dev 证书:
    cd "$EnvoyDir"
    mkcert -cert-file cert.pem -key-file key.pem $sanLine

注:cert.pem / key.pem 是本机私有文件,绝不入库(.gitignore 已屏蔽),换机器需各自生成。
   想让全队客户端「导一次 CA 连任意服务器」,先跑 tools/scripts/install_shared_dev_ca.ps1
   把全队共享 CA 装进本机 mkcert,再由本脚本签发(这样各服务器证书都出自同一个 CA)。
"@
        throw $msg
    }

    # mkcert 在 → 自动重生叶子证书(损坏的先改名留档,不静默删)。
    Write-Host "[INFO] 检测到 mkcert,自动重新生成 Envoy dev 证书..." -ForegroundColor Cyan
    foreach ($p in @($certPath, $keyPath)) {
        if (Test-Path $p) {
            $bad = "$p.bad"
            if (Test-Path $bad) { Remove-Item $bad -Force -ErrorAction SilentlyContinue }
            Rename-Item $p $bad -ErrorAction SilentlyContinue
        }
    }

    Push-Location $EnvoyDir
    try {
        & mkcert -cert-file cert.pem -key-file key.pem @sanHosts
        $rc = $LASTEXITCODE
    } finally {
        Pop-Location
    }
    if ($rc -ne 0) {
        throw "mkcert 生成证书失败(exit=$rc),证书目录:$EnvoyDir"
    }

    # 复检,确保真生成了有效文件。
    $certOk = Test-PemFile -Path $certPath -BeginPattern 'BEGIN CERTIFICATE'
    $keyOk  = Test-PemFile -Path $keyPath  -BeginPattern 'BEGIN (RSA |EC )?PRIVATE KEY'
    if (-not ($certOk -and $keyOk)) {
        throw "mkcert 生成后证书仍无效,请手动检查:$EnvoyDir"
    }
    Write-Host "[ OK ] 已用 mkcert 重新生成 Envoy dev 证书:$certPath" -ForegroundColor Green
    Write-Host "[INFO] 若已装全队共享 CA(install_shared_dev_ca.ps1),客户端导一次 CA 即可连任意服务器;" -ForegroundColor Cyan
    Write-Host "       否则本机是独立 CA,客户端需对本机单独跑 import_dev_ca.ps1。" -ForegroundColor Cyan
}
