[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$ProjectRoot = (Resolve-Path "$PSScriptRoot/../../..").Path
$Generator = Join-Path $ProjectRoot 'tools/scripts/gen_cluster_config.ps1'
$OutDir = Join-Path ([System.IO.Path]::GetTempPath()) ('pandora-gen-b1-' + [guid]::NewGuid().ToString('N'))

function Assert-True([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw "ASSERT FAILED:$Message" }
}

try {
    & pwsh -NoProfile -File $Generator -OutDir $OutDir -AllocatorMode agones `
        -AllocatorAdvertiseHost 127.0.0.1 -AllowDevSecrets `
        -DsAuthMode enforce -DsAuthorityMode redis -DsFenceEtcdEndpoints 'etcd.pandora.svc:2379' `
        -DsFenceKeysetRevision 'pandora-ds-auth-v2-local-r1' `
        -DsTicketActiveKid ('A' * 43) -DsTicketKeysetRevision 7 `
        -BattleCanaryPercent 17 -HubCanaryPercent 23 -CanarySeed 'stable-seed-001' *> $null
    if ($LASTEXITCODE -ne 0) { throw "gen_cluster_config B1 合同生成失败(exit=$LASTEXITCODE)" }

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
    if (Test-Path -LiteralPath $OutDir -PathType Container) {
        $resolved = [System.IO.Path]::GetFullPath($OutDir)
        $temp = [System.IO.Path]::GetFullPath([System.IO.Path]::GetTempPath())
        if (-not $resolved.StartsWith($temp, [StringComparison]::OrdinalIgnoreCase) -or
            (Split-Path -Leaf $resolved) -notmatch '^pandora-gen-b1-[0-9a-f]{32}$') {
            throw "拒绝清理未验证测试目录:$resolved"
        }
        Remove-Item -LiteralPath $resolved -Recurse -Force
    }
}

Write-Host 'gen_cluster_b1_contract_test: PASS' -ForegroundColor Green
