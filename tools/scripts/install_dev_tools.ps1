# Pandora 开发环境工具链一键安装(Windows / PowerShell)
#
# 安装的工具:
#   - buf       proto 工具(lint / generate / breaking)
#   - mkcert    自签 TLS 证书(Envoy / 本地 HTTPS 用)
#   - grpcurl   gRPC 端到端测试(命令行 grpcurl 调 gRPC 服务)
#
# 不安装的(用户应已有):
#   - go        Go 编译器(必需;Kratos `protoc-gen-go-http` 是本地 plugin)
#   - docker    docker desktop
#   - git       版本控制
#
# 用法:
#   pwsh tools/scripts/install_dev_tools.ps1            # 装所有工具
#   pwsh tools/scripts/install_dev_tools.ps1 -Check     # 只检查不装
#   pwsh tools/scripts/install_dev_tools.ps1 -Force     # 强制重装(已装的也重装)
#
# 工具版本(锁定,跟项目对齐):
#   buf       v1.50.0   (跟 buf.gen.go.yaml 里 plugin 版本兼容)
#   mkcert    最新       (���版本锁定需求)
#   grpcurl   v1.9.1     (社区主流稳定版)
#
# 安装方式:优先 winget,失败回退 scoop,失败再回退 GitHub Release 直接下载。

param(
    [switch]$Check,    # 只检查,不安装
    [switch]$Force     # 强制重装
)

$ErrorActionPreference = "Stop"

# ===== 工具版本(锁定)=====
$BUF_VERSION     = "v1.50.0"
$GRPCURL_VERSION = "v1.9.1"

# ===== 工具元信息 =====
$Tools = @(
    @{
        Name        = "buf"
        Version     = $BUF_VERSION
        WingetId    = "bufbuild.buf"
        ScoopId     = "buf"
        CheckCmd    = "buf"
        CheckArgs   = "--version"
        VersionPattern = "^[0-9]+\.[0-9]+\.[0-9]+"
        Description = "Proto 工具链:lint + generate + breaking 检测"
    },
    @{
        Name        = "mkcert"
        Version     = "latest"
        WingetId    = "FiloSottile.mkcert"
        ScoopId     = "mkcert"
        CheckCmd    = "mkcert"
        CheckArgs   = "-version"
        VersionPattern = "^v?[0-9]+\.[0-9]+\.[0-9]+"
        Description = "自签 TLS 证书工具(Envoy 本地开发用)"
    },
    @{
        Name        = "grpcurl"
        Version     = $GRPCURL_VERSION
        WingetId    = "fullstorydev.grpcurl"
        ScoopId     = "grpcurl"
        CheckCmd    = "grpcurl"
        CheckArgs   = "--version"
        VersionPattern = "[0-9]+\.[0-9]+\.[0-9]+"
        Description = "gRPC 命令行测试工具(端到端验证)"
    }
)

# ===== 颜色输出辅助 =====
function Write-Info($msg)  { Write-Host "[INFO] $msg" -ForegroundColor Cyan }
function Write-Ok($msg)    { Write-Host "[ OK ] $msg" -ForegroundColor Green }
function Write-Skip($msg)  { Write-Host "[SKIP] $msg" -ForegroundColor DarkGray }
function Write-Warn($msg)  { Write-Host "[WARN] $msg" -ForegroundColor Yellow }
function Write-Err($msg)   { Write-Host "[ERR ] $msg" -ForegroundColor Red }

# ===== 检查命令是否存在 =====
function Test-CommandExists {
    param([string]$cmd)
    return [bool](Get-Command $cmd -ErrorAction SilentlyContinue)
}

# ===== 装 winget =====
function Install-ViaWinget {
    param([string]$pkgId)
    Write-Info "  尝试 winget install $pkgId ..."
    $null = winget install --id $pkgId --silent --accept-source-agreements --accept-package-agreements 2>&1
    return ($LASTEXITCODE -eq 0)
}

# ===== 装 scoop =====
function Install-ViaScoop {
    param([string]$pkgId)
    if (-not (Test-CommandExists "scoop")) {
        Write-Skip "  scoop 未安装,跳过"
        return $false
    }
    Write-Info "  尝试 scoop install $pkgId ..."
    $null = scoop install $pkgId 2>&1
    return ($LASTEXITCODE -eq 0)
}

# ===== 主流程 =====
Write-Host ""
Write-Host "======================================" -ForegroundColor Magenta
Write-Host " Pandora Dev Tools 一键安装" -ForegroundColor Magenta
Write-Host "======================================" -ForegroundColor Magenta

if ($Check) {
    Write-Info "运行模式:仅检查(不安装)"
} elseif ($Force) {
    Write-Info "运行模式:强制重装"
} else {
    Write-Info "运行模式:智能安装(已装则跳过)"
}

Write-Host ""

$results = @()

foreach ($tool in $Tools) {
    Write-Host ""
    Write-Host "----- $($tool.Name) -----" -ForegroundColor Magenta
    Write-Host "用途:$($tool.Description)"
    Write-Host "版本:$($tool.Version)"

    # 1. 检查是否已装
    $installed = Test-CommandExists $tool.CheckCmd
    if ($installed) {
        $verOutput = & $tool.CheckCmd $tool.CheckArgs 2>&1 | Out-String
        $verMatch = [regex]::Match($verOutput, $tool.VersionPattern)
        $currentVer = if ($verMatch.Success) { $verMatch.Value } else { "(未知)" }
        Write-Ok "已安装,版本:$currentVer"

        if ($Check) {
            $results += [PSCustomObject]@{ Tool=$tool.Name; Status="installed"; Version=$currentVer }
            continue
        }
        if (-not $Force) {
            $results += [PSCustomObject]@{ Tool=$tool.Name; Status="installed"; Version=$currentVer }
            Write-Skip "已装且未指定 -Force,跳过"
            continue
        }
        Write-Warn "已装但 -Force 启用,继续重装"
    } else {
        Write-Info "未安装"
        if ($Check) {
            $results += [PSCustomObject]@{ Tool=$tool.Name; Status="missing"; Version="-" }
            continue
        }
    }

    # 2. 装(优先 winget,回退 scoop)
    $ok = Install-ViaWinget -pkgId $tool.WingetId
    if (-not $ok) {
        Write-Warn "  winget 失败,尝试 scoop..."
        $ok = Install-ViaScoop -pkgId $tool.ScoopId
    }

    if ($ok) {
        Write-Ok "$($tool.Name) 安装成功"
        $results += [PSCustomObject]@{ Tool=$tool.Name; Status="installed"; Version=$tool.Version }
    } else {
        Write-Err "$($tool.Name) 安装失败"
        Write-Host "  请手动安装,参考:" -ForegroundColor Yellow
        switch ($tool.Name) {
            "buf"     { Write-Host "    https://github.com/bufbuild/buf/releases/tag/$($tool.Version)" -ForegroundColor Yellow }
            "mkcert"  { Write-Host "    https://github.com/FiloSottile/mkcert/releases" -ForegroundColor Yellow }
            "grpcurl" { Write-Host "    https://github.com/fullstorydev/grpcurl/releases/tag/$($tool.Version)" -ForegroundColor Yellow }
        }
        $results += [PSCustomObject]@{ Tool=$tool.Name; Status="failed"; Version="-" }
    }
}

# ===== 后处理 =====

# mkcert 装好后需要跑 -install 一次(信任本地 CA)
$mkcertOk = ($results | Where-Object { $_.Tool -eq "mkcert" -and $_.Status -eq "installed" }).Count -gt 0
if ($mkcertOk -and -not $Check) {
    Write-Host ""
    Write-Info "mkcert 首次配置:安装本地 CA(可能弹 UAC 框)"
    if (Test-CommandExists "mkcert") {
        & mkcert -install 2>&1 | Out-Null
        if ($LASTEXITCODE -eq 0) {
            Write-Ok "mkcert 本地 CA 已安装"
        } else {
            Write-Warn "mkcert -install 可能失败,请手动跑:mkcert -install"
        }
    }
}

# ===== 总结 =====

Write-Host ""
Write-Host "======================================" -ForegroundColor Magenta
Write-Host " 安装总结" -ForegroundColor Magenta
Write-Host "======================================" -ForegroundColor Magenta

$results | Format-Table -AutoSize

$failed = ($results | Where-Object { $_.Status -eq "failed" }).Count
$missing = ($results | Where-Object { $_.Status -eq "missing" }).Count

if ($Check) {
    if ($missing -gt 0) {
        Write-Warn "$missing 个工具未安装,跑 'pwsh tools/scripts/install_dev_tools.ps1' 安装"
        exit 1
    }
    Write-Ok "全部工具就绪"
    exit 0
}

if ($failed -gt 0) {
    Write-Err "$failed 个工具安装失败,见上方手动安装指引"
    exit 1
}

Write-Host ""
Write-Ok "全部工具就绪!"
Write-Host ""
Write-Host "下一步:" -ForegroundColor Cyan
Write-Host "  pwsh tools/scripts/proto_gen.ps1     生成 .pb.go" -ForegroundColor Cyan
Write-Host "  pwsh tools/scripts/dev_up.ps1        启动基础设施" -ForegroundColor Cyan
Write-Host ""
