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
$OutDirPlacement = Join-Path ([System.IO.Path]::GetTempPath()) ('pandora-gen-b1-placement-' + [guid]::NewGuid().ToString('N'))
$OutDirMatchAuth = Join-Path ([System.IO.Path]::GetTempPath()) ('pandora-gen-b1-matchauth-' + [guid]::NewGuid().ToString('N'))
$OutDirs = @($OutDir, $OutDirRerun, $OutDirRotation, $OutDirDrift, $OutDirOverlap, $OutDirPlacement, $OutDirMatchAuth)

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
        [string]$PlacementBootstrap = '',
        [string]$PlacementMatchStart = '',
        [string]$PlacementBattleExit = '',
        [string]$PlacementHubTransfer = '',
        [string]$MatchResumeAuth = '',
        [switch]$ExpectFailure
    )
    & pwsh -NoProfile -File $Generator -OutDir $TargetDir -AllocatorMode agones `
        -AllocatorAdvertiseHost 127.0.0.1 -AllowDevSecrets `
        -Secret $PlayerSecret -DsSecret $DsSecret `
        -SecretAdditional $PlayerAdditional -DsSecretAdditional $DsAdditional `
        -PlacementAccountBootstrapSecret $PlacementBootstrap `
        -PlacementMatchStartSecret $PlacementMatchStart `
        -PlacementBattleExitSecret $PlacementBattleExit `
        -PlacementHubTransferSecret $PlacementHubTransfer `
        -MatchResumeAuthSecret $MatchResumeAuth `
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
    $devPlacement = Get-PandoraOnlinePlacementContract -Configs $devConfigs
    Assert-PandoraOnlinePlacementContinuity -LiveConfigs $devConfigs -CandidateConfigs $rerunConfigs | Out-Null
    $devMatchAuth = Get-PandoraOnlineMatchResumeAuthContract -Configs $devConfigs
    Assert-PandoraOnlineMatchResumeAuthContinuity -LiveConfigs $devConfigs -CandidateConfigs $rerunConfigs | Out-Null

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

    # Agones 真实 DS 链默认开启严格 placement，且 writer 只拿自身 key、locator 拿四把。
    $loginPlacement = Get-Content -LiteralPath (Join-Path $OutDir 'login.yaml') -Raw
    $hubPlacement = Get-Content -LiteralPath (Join-Path $OutDir 'hub-allocator.yaml') -Raw
    Assert-True ($loginPlacement.Contains('placement_mode: "enforce"')) 'Login placement_mode 未严格开启'
    Assert-True ($hubPlacement.Contains('placement_mode: "enforce"')) 'Hub placement_mode 未严格开启'
    $placementKeys = [ordered]@{
        Bootstrap = 'placement-bootstrap-test-key-0123456789abcdef'
        MatchStart = 'placement-match-start-test-key-0123456789abcdef'
        BattleExit = 'placement-battle-exit-test-key-0123456789abcdef'
        HubTransfer = 'placement-hub-transfer-test-key-0123456789abcdef'
    }
    Invoke-B1Generator -TargetDir $OutDirPlacement `
        -PlacementBootstrap $placementKeys.Bootstrap -PlacementMatchStart $placementKeys.MatchStart `
        -PlacementBattleExit $placementKeys.BattleExit -PlacementHubTransfer $placementKeys.HubTransfer
    $placementExpected = @{
        'login.yaml' = @($placementKeys.Bootstrap)
        'matchmaker.yaml' = @($placementKeys.MatchStart)
        'matchmaker-pve.yaml' = @($placementKeys.MatchStart)
        'battle-result.yaml' = @($placementKeys.BattleExit)
        'hub-allocator.yaml' = @($placementKeys.HubTransfer)
        'player-locator.yaml' = @($placementKeys.Bootstrap, $placementKeys.MatchStart,
            $placementKeys.BattleExit, $placementKeys.HubTransfer)
    }
    foreach ($entry in $placementExpected.GetEnumerator()) {
        $yaml = Get-Content -LiteralPath (Join-Path $OutDirPlacement $entry.Key) -Raw
        foreach ($key in $entry.Value) { Assert-True ($yaml.Contains($key)) "$($entry.Key) 缺 placement 分权 key" }
    }
    $explicitPlacementConfigs = Get-B1HmacConfigs $OutDirPlacement
    $explicitPlacement = Get-PandoraOnlinePlacementContract -Configs $explicitPlacementConfigs
    Assert-True ($explicitPlacement.AccountBootstrap -cne $devPlacement.AccountBootstrap) `
        '显式 placement 候选必须实际改变 account-bootstrap key'
    Assert-Throws {
        Assert-PandoraOnlinePlacementContinuity -LiveConfigs $devConfigs `
            -CandidateConfigs $explicitPlacementConfigs | Out-Null
    } '普通发布必须拒绝 placement proof key 漂移'

    # 即使生成器产物被外部流程单点改写，writer/locator 不一致也必须在 apply 前失败。
    $mismatchedPlacementConfigs = [ordered]@{}
    foreach ($entry in $explicitPlacementConfigs.GetEnumerator()) {
        $mismatchedPlacementConfigs[$entry.Key] = [string]$entry.Value
    }
    $mismatchedPlacementConfigs['matchmaker'] = $mismatchedPlacementConfigs['matchmaker'].Replace(
        $placementKeys.MatchStart, 'placement-match-start-drift-0123456789abcdef')
    Assert-Throws {
        Get-PandoraOnlinePlacementContract -Configs $mismatchedPlacementConfigs | Out-Null
    } '普通发布必须拒绝 placement writer/locator 单点漂移'
    Invoke-B1Generator -TargetDir $OutDirOverlap `
        -PlacementBootstrap $placementKeys.Bootstrap -PlacementMatchStart $placementKeys.Bootstrap `
        -PlacementBattleExit $placementKeys.BattleExit -PlacementHubTransfer $placementKeys.HubTransfer -ExpectFailure

    # Login 与两个 Matchmaker writer 必须拿到同一把独立服务身份 key；普通发布禁止静默换钥。
    $explicitMatchAuth = 'match-resume-auth-test-key-0123456789abcdef'
    Invoke-B1Generator -TargetDir $OutDirMatchAuth -MatchResumeAuth $explicitMatchAuth
    $matchAuthConfigs = Get-B1HmacConfigs $OutDirMatchAuth
    $matchAuthContract = Get-PandoraOnlineMatchResumeAuthContract -Configs $matchAuthConfigs
    Assert-True ($matchAuthContract -cne $devMatchAuth) '显式 Match resume service key 必须实际改变候选指纹'
    foreach ($service in @('login', 'matchmaker', 'matchmaker-pve')) {
        $yaml = Get-Content -LiteralPath (Join-Path $OutDirMatchAuth "$service.yaml") -Raw
        Assert-True ($yaml.Contains($explicitMatchAuth)) "$service 缺 Match resume service identity key"
    }
    Assert-Throws {
        Assert-PandoraOnlineMatchResumeAuthContinuity -LiveConfigs $devConfigs `
            -CandidateConfigs $matchAuthConfigs | Out-Null
    } '普通发布必须拒绝 Match resume service identity key 漂移'
    Invoke-B1Generator -TargetDir $OutDirOverlap -PlayerSecret $testPlayerPrimary `
        -MatchResumeAuth $testPlayerPrimary -ExpectFailure
    Invoke-B1Generator -TargetDir $OutDirOverlap -PlacementBootstrap $placementKeys.Bootstrap `
        -MatchResumeAuth $placementKeys.Bootstrap -ExpectFailure

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
            (Split-Path -Leaf $resolved) -notmatch '^pandora-gen-b1(?:-(?:rerun|rotation|drift|overlap|placement|matchauth))?-[0-9a-f]{32}$') {
            throw "拒绝清理未验证测试目录:$resolved"
        }
        Remove-Item -LiteralPath $resolved -Recurse -Force
    }
}

Write-Host 'gen_cluster_b1_contract_test: PASS' -ForegroundColor Green
