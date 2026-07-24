# Pandora 制品目录公共函数(被 publish_offline_images.ps1 / fetch_offline_images.ps1 /
# make_release.ps1 / artifacts_retention.ps1 dot-source 引用,不单独运行)。
#
# 制品目录(artifact root)是"版本库外"的构建产物归档地:
#   <root>\client\<branch>\<flavor>\r<rev>\      UE 包(由客户端仓 Tool\Build\PublishPackages.ps1 发布)
#   <root>\images\<version>\pandora-images.tar   业务镜像离线包
#   <root>\images\latest.json                    可变指针:最近一次镜像发布
#   <root>\releases\<name>.json                  release manifest(发布事实清单)
# 三条铁律:版本目录不可变(已存在即拒绝覆盖)、发布原子(staging+rename)、保留策略由
# artifacts_retention.ps1 执行且 release 引用的版本永不清理。
# 设计文档:docs/design/release-pipeline.md

function Get-ArtifactRoot {
    param([string]$Override)
    $root = if ($Override) { $Override }
            elseif ($env:PANDORA_ARTIFACT_ROOT) { $env:PANDORA_ARTIFACT_ROOT }
            else { 'F:\work\artifacts' }
    $root = [System.IO.Path]::GetFullPath($root)
    if (-not (Test-Path -LiteralPath $root)) {
        New-Item -ItemType Directory -Path $root -Force | Out-Null
    }
    return $root
}

# 两轨分仓:snapshot(dev 快照,激进清理)/ release(发布版本,不可变永久保留)。
#   <root>\snapshots\images|client\...
#   <root>\releases\images|client\...  + <root>\releases\manifests\<版本>.json
function Get-ChannelRoot {
    param(
        [string]$Override,
        [Parameter(Mandatory)][ValidateSet('snapshot', 'release')][string]$Channel
    )
    $root = Get-ArtifactRoot -Override $Override
    $sub  = if ($Channel -eq 'release') { 'releases' } else { 'snapshots' }
    $dir  = Join-Path $root $sub
    if (-not (Test-Path -LiteralPath $dir)) { New-Item -ItemType Directory -Path $dir -Force | Out-Null }
    return $dir
}

# 对目录内全部文件生成 sha256sums.txt(相对路径,'/'分隔,与 sha256sum -c 格式兼容)。
function New-Sha256Sums {
    param([Parameter(Mandatory)][string]$Dir)
    $out = Join-Path $Dir 'sha256sums.txt'
    $lines = Get-ChildItem -LiteralPath $Dir -Recurse -File |
        Where-Object { $_.Name -ne 'sha256sums.txt' } |
        ForEach-Object {
            $rel = [System.IO.Path]::GetRelativePath($Dir, $_.FullName) -replace '\\', '/'
            $hash = (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
            "$hash  $rel"
        }
    $lines | Set-Content -LiteralPath $out -Encoding utf8NoBOM
    return $out
}

# 校验目录与 sha256sums.txt 一致;不一致抛异常。
function Test-Sha256Sums {
    param([Parameter(Mandatory)][string]$Dir)
    $sums = Join-Path $Dir 'sha256sums.txt'
    if (-not (Test-Path -LiteralPath $sums)) { throw "缺少校验文件:$sums" }
    $bad = @()
    foreach ($line in Get-Content -LiteralPath $sums) {
        if ($line -notmatch '^([0-9a-f]{64})  (.+)$') { continue }
        $want = $Matches[1]; $rel = $Matches[2]
        $file = Join-Path $Dir ($rel -replace '/', '\')
        if (-not (Test-Path -LiteralPath $file)) { $bad += "缺失 $rel"; continue }
        $got = (Get-FileHash -LiteralPath $file -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($got -ne $want) { $bad += "哈希不符 $rel" }
    }
    if ($bad.Count -gt 0) { throw "制品校验失败($($bad.Count) 项):`n$($bad -join "`n")" }
}

# 原子发布:调用方把内容写进本函数返回的 staging 目录,再调 Complete-AtomicDir 一次 rename 上线。
# 目标已存在时按不可变原则拒绝(除非调用方先用 Test-Path 走 -SkipIfExists 分支)。
function New-AtomicStaging {
    param([Parameter(Mandatory)][string]$FinalDir)
    if (Test-Path -LiteralPath $FinalDir) {
        throw "制品版本目录已存在,不可变原则禁止覆盖:$FinalDir(需要新版本请提升版本号)"
    }
    $parent = Split-Path -Parent $FinalDir
    if (-not (Test-Path -LiteralPath $parent)) { New-Item -ItemType Directory -Path $parent -Force | Out-Null }
    $staging = Join-Path $parent (".tmp-" + (Split-Path -Leaf $FinalDir) + "-" + $PID)
    if (Test-Path -LiteralPath $staging) { Remove-Item -LiteralPath $staging -Recurse -Force }
    New-Item -ItemType Directory -Path $staging -Force | Out-Null
    return $staging
}

function Complete-AtomicDir {
    param(
        [Parameter(Mandatory)][string]$Staging,
        [Parameter(Mandatory)][string]$FinalDir
    )
    if (Test-Path -LiteralPath $FinalDir) {
        Remove-Item -LiteralPath $Staging -Recurse -Force
        throw "发布竞争:目标在 staging 期间被他人发布,已丢弃本次 staging:$FinalDir"
    }
    Move-Item -LiteralPath $Staging -Destination $FinalDir
}
