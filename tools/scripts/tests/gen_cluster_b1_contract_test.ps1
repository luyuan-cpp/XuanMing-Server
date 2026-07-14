[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$ProjectRoot = (Resolve-Path "$PSScriptRoot/../../..").Path
$Generator = Join-Path $ProjectRoot 'tools/scripts/gen_cluster_config.ps1'
$HmacContractLib = Join-Path $ProjectRoot 'tools/scripts/lib/online_manifest_contract.ps1'
$OutDir = Join-Path ([System.IO.Path]::GetTempPath()) ('pandora-gen-b1-' + [guid]::NewGuid().ToString('N'))
$OutDirRerun = Join-Path ([System.IO.Path]::GetTempPath()) ('pandora-gen-b1-rerun-' + [guid]::NewGuid().ToString('N'))
$OutDirRotation = Join-Path ([System.IO.Path]::GetTempPath()) ('pandora-gen-b1-rotation-' + [guid]::NewGuid().ToString('N'))
$OutDirDrift = Join-Path ([System.IO.Path]::GetTempPath()) ('pandora-gen-b1-drift-' + [guid]::NewGuid().ToString('N'))
$OutDirOverlap = Join-Path ([System.IO.Path]::GetTempPath()) ('pandora-gen-b1-overlap-' + [guid]::NewGuid().ToString('N'))
$OutDirs = @($OutDir, $OutDirRerun, $OutDirRotation, $OutDirDrift, $OutDirOverlap)

. $HmacContractLib

function Assert-True([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw "ASSERT FAILED:$Message" }
}

function Assert-Throws([scriptblock]$Action, [string]$Message) {
    $thrown = $false
    try { & $Action } catch { $thrown = $true }
    Assert-True $thrown $Message
}

function Invoke-B1Generator {
    param(
        [Parameter(Mandatory = $true)][string]$TargetDir,
        [string]$PlayerSecret = '',
        [string]$DsSecret = '',
        [string]$PlayerAdditional = '',
        [string]$DsAdditional = '',
        [switch]$ExpectFailure
    )
    & pwsh -NoProfile -File $Generator -OutDir $TargetDir -AllocatorMode agones `
        -AllocatorAdvertiseHost 127.0.0.1 -AllowDevSecrets `
        -Secret $PlayerSecret -DsSecret $DsSecret `
        -SecretAdditional $PlayerAdditional -DsSecretAdditional $DsAdditional `
        -DsAuthMode enforce -DsAuthorityMode redis -DsFenceEtcdEndpoints 'etcd.pandora.svc:2379' `
        -DsFenceKeysetRevision 'pandora-ds-auth-v2-local-r1' `
        -DsTicketActiveKid ('A' * 43) -DsTicketKeysetRevision 7 `
        -BattleCanaryPercent 17 -HubCanaryPercent 23 -CanarySeed 'stable-seed-001' *> $null
    if ($ExpectFailure) {
        if ($LASTEXITCODE -eq 0) { throw 'gen_cluster_config 应拒绝跨域重叠 HMAC keyset，但生成成功。' }
        return
    }
    if ($LASTEXITCODE -ne 0) { throw "gen_cluster_config B1 合同生成失败(exit=$LASTEXITCODE)" }
}

function Get-B1HmacConfigs([string]$TargetDir) {
    $configs = [ordered]@{}
    foreach ($service in @('login', 'matchmaker', 'matchmaker-pve', 'hub-allocator',
            'ds-allocator', 'battle-result', 'player-locator')) {
        $configs[$service] = Get-Content -LiteralPath (Join-Path $TargetDir "$service.yaml") -Raw
    }
    return $configs
}

try {
    Invoke-B1Generator -TargetDir $OutDir
    Invoke-B1Generator -TargetDir $OutDirRerun

    # 默认 dev 必须生成两套稳定、互不相交的 HMAC keyset；普通发布连续性门对幂等重跑应放行。
    $devConfigs = Get-B1HmacConfigs $OutDir
    $rerunConfigs = Get-B1HmacConfigs $OutDirRerun
    $devHmac = Get-PandoraOnlineHmacContract -Configs $devConfigs
    Assert-True ($devHmac.Player.PrimarySha256 -cne $devHmac.DsCallback.PrimarySha256) `
        '默认 dev 玩家 Session 与 DS callback primary 必须不同'
    Assert-True ($devHmac.Player.AdditionalSha256.Count -eq 0 -and
        $devHmac.DsCallback.AdditionalSha256.Count -eq 0) '默认 dev 不应凭空生成 additional key'
    Assert-PandoraOnlineHmacContinuity -LiveConfigs $devConfigs -CandidateConfigs $rerunConfigs | Out-Null

    # 非生产轮换验证也必须覆盖 primary/additional 的完整跨域不相交集合。
    $testPlayerPrimary = 'test-player-primary-hmac-0123456789abcdef'
    $testDsPrimary = 'test-ds-callback-primary-hmac-0123456789abcdef'
    $testPlayerAdditional = 'test-player-additional-hmac-0123456789abcdef'
    $testDsAdditional = 'test-ds-callback-additional-hmac-0123456789abcdef'
    Invoke-B1Generator -TargetDir $OutDirRotation -PlayerSecret $testPlayerPrimary -DsSecret $testDsPrimary `
        -PlayerAdditional $testPlayerAdditional -DsAdditional $testDsAdditional
    $rotationHmac = Get-PandoraOnlineHmacContract -Configs (Get-B1HmacConfigs $OutDirRotation)
    $playerSet = @($rotationHmac.Player.PrimarySha256) + @($rotationHmac.Player.AdditionalSha256)
    $dsSet = @($rotationHmac.DsCallback.PrimarySha256) + @($rotationHmac.DsCallback.AdditionalSha256)
    Assert-True ($playerSet.Count -eq 2 -and $dsSet.Count -eq 2) '轮换验证必须保留两域各自 primary+additional'
    Assert-True (@($playerSet | Where-Object { $dsSet -contains $_ }).Count -eq 0) `
        '玩家与 DS callback primary/additional keyset 不得有交集'

    # 两个交叉方向都必须在生成前失败；测试不回显任何 HMAC 值。
    Invoke-B1Generator -TargetDir $OutDirOverlap -PlayerSecret $testPlayerPrimary -DsSecret $testDsPrimary `
        -PlayerAdditional $testDsPrimary -ExpectFailure
    Invoke-B1Generator -TargetDir $OutDirOverlap -PlayerSecret $testPlayerPrimary -DsSecret $testDsPrimary `
        -DsAdditional $testPlayerPrimary -ExpectFailure

    # 候选只改 DS callback primary 时，普通发布漂移门必须继续拒绝。
    $testDsDrift = 'test-ds-callback-drift-hmac-0123456789abcdef'
    Invoke-B1Generator -TargetDir $OutDirDrift -DsSecret $testDsDrift
    $driftConfigs = Get-B1HmacConfigs $OutDirDrift
    $driftHmac = Get-PandoraOnlineHmacContract -Configs $driftConfigs
    Assert-True ($driftHmac.Player.PrimarySha256 -ceq $devHmac.Player.PrimarySha256) `
        '漂移候选只能改变 DS callback 域'
    Assert-True ($driftHmac.DsCallback.PrimarySha256 -cne $devHmac.DsCallback.PrimarySha256) `
        '漂移候选必须实际改变 DS callback primary'
    Assert-Throws {
        Assert-PandoraOnlineHmacContinuity -LiveConfigs $devConfigs -CandidateConfigs $driftConfigs | Out-Null
    } '普通发布必须拒绝 DS callback HMAC 漂移'

    $signers = @('login', 'matchmaker', 'matchmaker-pve', 'hub-allocator')
    foreach ($service in $signers) {
        $yaml = Get-Content -LiteralPath (Join-Path $OutDir "$service.yaml") -Raw
        Assert-True ($yaml.Contains('private_key_file: "/run/secrets/pandora-dsticket/private.pem"')) "$service 缺 DSTicket 私钥路径"
        Assert-True ($yaml.Contains('active_kid: "' + ('A' * 43) + '"')) "$service 缺 explicit active_kid"
        if ($service -ceq 'login') {
            Assert-True ($yaml.Contains('jwks_file: "/run/config/pandora-dsticket/jwks.json"')) 'Login 缺公开 overlap JWKS verifier'
            Assert-True ($yaml.Contains('keyset_revision: "7"')) 'Login 缺 DSTicket keyset revision'
        } else {
            Assert-True (-not $yaml.Contains('jwks_file: "/run/config/pandora-dsticket/jwks.json"')) "$service 不应误开 Login-only verifier"
        }
    }

    foreach ($service in @('login', 'ds-allocator', 'hub-allocator', 'battle-result', 'player-locator')) {
        $yaml = Get-Content -LiteralPath (Join-Path $OutDir "$service.yaml") -Raw
        Assert-True ([regex]::IsMatch($yaml, '(?m)^\s*mode:\s*"?enforce"?\s*$')) "$service ds_auth.mode 不是 enforce"
        Assert-True ([regex]::IsMatch($yaml, '(?m)^\s*authority_mode:\s*"?redis"?\s*$')) "$service authority_mode 不是 redis"
        Assert-True ($yaml.Contains('etcd_endpoints: ["etcd.pandora.svc:2379"]')) "$service 缺集群内 etcd fence endpoint"
        Assert-True ($yaml.Contains('keyset_revision: "pandora-ds-auth-v2-local-r1"')) "$service callback keyset revision 漂移"
    }

    $battle = Get-Content -LiteralPath (Join-Path $OutDir 'ds-allocator.yaml') -Raw
    foreach ($needle in @(
        'fleet_name: "pandora-battle-stable"', 'canary_fleet_name: "pandora-battle-canary"',
        'canary_percent: 17', 'canary_seed: "stable-seed-001"')) {
        Assert-True ($battle.Contains($needle)) "Battle allocator 缺字段:$needle"
    }
    $hub = Get-Content -LiteralPath (Join-Path $OutDir 'hub-allocator.yaml') -Raw
    foreach ($needle in @(
        'fleet_name: "pandora-hub-stable"', 'canary_fleet_name: "pandora-hub-canary"',
        'canary_percent: 23', 'canary_seed: "stable-seed-001"')) {
        Assert-True ($hub.Contains($needle)) "Hub allocator 缺字段:$needle"
    }
} finally {
    foreach ($dir in $OutDirs) {
        if (-not (Test-Path -LiteralPath $dir -PathType Container)) { continue }
        $resolved = [System.IO.Path]::GetFullPath($dir)
        $temp = [System.IO.Path]::GetFullPath([System.IO.Path]::GetTempPath())
        if (-not $resolved.StartsWith($temp, [StringComparison]::OrdinalIgnoreCase) -or
            (Split-Path -Leaf $resolved) -notmatch '^pandora-gen-b1(?:-(?:rerun|rotation|drift|overlap))?-[0-9a-f]{32}$') {
            throw "拒绝清理未验证测试目录:$resolved"
        }
        Remove-Item -LiteralPath $resolved -Recurse -Force
    }
}

Write-Host 'gen_cluster_b1_contract_test: PASS' -ForegroundColor Green
