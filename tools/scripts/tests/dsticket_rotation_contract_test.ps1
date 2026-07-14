[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$ProjectRoot = (Resolve-Path "$PSScriptRoot/../../..").Path
. (Join-Path $ProjectRoot 'tools/scripts/lib/dsticket_rotation_contract.ps1')

function Assert-True([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw "ASSERT FAILED:$Message" }
}
function Assert-Throws([scriptblock]$Action, [string]$Message, [string]$ExpectedText = '') {
    try { & $Action } catch {
        if (-not [string]::IsNullOrWhiteSpace($ExpectedText) -and $_.Exception.Message -notlike "*$ExpectedText*") {
            throw "ASSERT FAILED:异常未包含 '$ExpectedText':$($_.Exception.Message)"
        }
        return
    }
    throw "ASSERT FAILED:应抛错但成功:$Message"
}
function Copy-TestObject($Object) {
    return ($Object | ConvertTo-Json -Depth 60 | ConvertFrom-Json)
}
function New-TestSecret([string]$PrivatePem, [int]$SignerRevision) {
    $private = Get-PandoraDSTicketPrivateKeyPublicContract -PrivateKeyPem $PrivatePem
    $privateHash = Get-PandoraSha256Hex ([Text.Encoding]::UTF8.GetBytes($PrivatePem))
    return [pscustomobject]@{
        apiVersion = 'v1'; kind = 'Secret'; immutable = $true
        metadata = [pscustomobject]@{
            name = "pandora-dsticket-signer-r$SignerRevision"; namespace = 'pandora'
            annotations = [pscustomobject]@{
                'pandora.dev/dsticket-signer-kid' = $private.Kid
                'pandora.dev/dsticket-signer-revision' = [string]$SignerRevision
                'pandora.dev/dsticket-private-pem-sha256' = $privateHash
            }
        }
        data = [pscustomobject]@{ 'private.pem' = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($PrivatePem)) }
    }
}
function New-TestConfigMap([string]$JwksText, [int]$Revision, [string]$Namespace) {
    $contract = Get-PandoraDSTicketJwksContract -JwksText $JwksText -ExpectedRevision $Revision
    return [pscustomobject]@{
        apiVersion = 'v1'; kind = 'ConfigMap'; immutable = $true
        metadata = [pscustomobject]@{
            name = "pandora-dsticket-jwks-r$Revision"; namespace = $Namespace
            annotations = [pscustomobject]@{
                'pandora.dev/dsticket-active-kid' = $contract.ActiveKid
                'pandora.dev/dsticket-keyset-revision' = [string]$Revision
                'pandora.dev/dsticket-jwks-sha256' = $contract.JwksSha256
            }
        }
        data = [pscustomobject]@{ 'jwks.json' = $JwksText }
    }
}
function New-TestDSPodSpec([string]$ContainerName, [int]$Revision) {
    return [pscustomobject]@{
        containers = @([pscustomobject]@{
            name = $ContainerName
            env = @(
                [pscustomobject]@{ name = 'PANDORA_DSTICKET_JWKS_FILE'; value = '/etc/pandora/dsticket/jwks.json' },
                [pscustomobject]@{ name = 'PANDORA_DSTICKET_KEYSET_REVISION'; value = [string]$Revision }
            )
            volumeMounts = @([pscustomobject]@{ name = 'dsticket-jwks'; mountPath = '/etc/pandora/dsticket'; readOnly = $true })
        })
        volumes = @([pscustomobject]@{
            name = 'dsticket-jwks'
            configMap = [pscustomobject]@{ name = "pandora-dsticket-jwks-r$Revision" }
        })
    }
}
function New-TestFleet([string]$Name, [int]$Revision) {
    $container = if ($Name.StartsWith('pandora-battle-')) { 'pandora-battle-ds' } else { 'pandora-hub-ds' }
    return [pscustomobject]@{
        kind = 'Fleet'
        metadata = [pscustomobject]@{ name = $Name; namespace = 'default'; uid = "fleet-$Name" }
        spec = [pscustomobject]@{
            replicas = 1
            template = [pscustomobject]@{
                spec = [pscustomobject]@{
                    template = [pscustomobject]@{ spec = (New-TestDSPodSpec $container $Revision) }
                }
            }
        }
    }
}
function New-TestGameServerSet([string]$Name, [string]$Fleet, [int]$Revision, [int]$Replicas = 1) {
    $container = if ($Fleet.StartsWith('pandora-battle-')) { 'pandora-battle-ds' } else { 'pandora-hub-ds' }
    return [pscustomobject]@{
        kind = 'GameServerSet'
        metadata = [pscustomobject]@{
            name = $Name; namespace = 'default'; uid = "uid-$Name"
            labels = [pscustomobject]@{ 'agones.dev/fleet' = $Fleet }
            ownerReferences = @([pscustomobject]@{
                kind = 'Fleet'; name = $Fleet; uid = "fleet-$Fleet"; controller = $true
            })
        }
        spec = [pscustomobject]@{ replicas = $Replicas; template = [pscustomobject]@{ spec = (New-TestDSPodSpec $container $Revision) } }
        status = [pscustomobject]@{
            replicas = $Replicas; readyReplicas = $Replicas; allocatedReplicas = 0; reservedReplicas = 0; currentReplicas = $Replicas
        }
    }
}
function New-TestGameServer([string]$Name, [string]$Fleet, [string]$State, [int]$Revision) {
    $container = if ($Fleet.StartsWith('pandora-battle-')) { 'pandora-battle-ds' } else { 'pandora-hub-ds' }
    return [pscustomobject]@{
        kind = 'GameServer'
        metadata = [pscustomobject]@{
            name = $Name; namespace = 'default'; uid = "uid-$Name"
            labels = [pscustomobject]@{ 'agones.dev/fleet' = $Fleet }
            ownerReferences = @([pscustomobject]@{
                kind = 'GameServerSet'; name = "$Fleet-gss"; uid = "uid-$Fleet-gss"; controller = $true
            })
        }
        spec = [pscustomobject]@{ template = [pscustomobject]@{ spec = (New-TestDSPodSpec $container $Revision) } }
        status = [pscustomobject]@{ state = $State }
    }
}
function New-TestDSPod([string]$Name, [string]$Fleet, [int]$Revision) {
    $container = if ($Fleet.StartsWith('pandora-battle-')) { 'pandora-battle-ds' } else { 'pandora-hub-ds' }
    return [pscustomobject]@{
        kind = 'Pod'
        metadata = [pscustomobject]@{
            name = $Name; namespace = 'default'
            labels = [pscustomobject]@{ 'agones.dev/gameserver' = $Name }
            ownerReferences = @([pscustomobject]@{
                kind = 'GameServer'; name = $Name; uid = "uid-$Name"; controller = $true
            })
        }
        spec = New-TestDSPodSpec $container $Revision
        status = [pscustomobject]@{
            phase = 'Running'
            conditions = @([pscustomobject]@{ type = 'Ready'; status = 'True' })
        }
    }
}
function New-TestSignerDeployment([string]$Name, [int]$SignerRevision, [int]$LoginRevision, [string]$ConfigName = 'pandora-config') {
    $volumes = @(
        [pscustomobject]@{ name = 'conf'; secret = [pscustomobject]@{ secretName = $ConfigName } },
        [pscustomobject]@{ name = 'dsticket'; secret = [pscustomobject]@{ secretName = "pandora-dsticket-signer-r$SignerRevision" } }
    )
    if ($Name -ceq 'login') {
        $volumes += [pscustomobject]@{
            name = 'dsticket-jwks'; configMap = [pscustomobject]@{ name = "pandora-dsticket-jwks-r$LoginRevision" }
        }
    }
    $mounts = @(
        [pscustomobject]@{
            name = 'conf'; mountPath = '/app/etc/cluster.yaml'; subPath = "$Name.yaml"; readOnly = $true
        },
        [pscustomobject]@{
            name = 'dsticket'; mountPath = '/run/secrets/pandora-dsticket'; readOnly = $true
        }
    )
    if ($Name -ceq 'login') {
        $mounts += [pscustomobject]@{
            name = 'dsticket-jwks'; mountPath = '/run/config/pandora-dsticket'; readOnly = $true
        }
    }
    return [pscustomobject]@{
        kind = 'Deployment'
        metadata = [pscustomobject]@{ name = $Name; namespace = 'pandora'; uid = "deployment-$Name"; generation = 7 }
        spec = [pscustomobject]@{
            replicas = 1; strategy = [pscustomobject]@{ type = 'RollingUpdate' }
            template = [pscustomobject]@{ spec = [pscustomobject]@{
                containers = @([pscustomobject]@{ name = $Name; volumeMounts = $mounts })
                volumes = $volumes
            } }
        }
        status = [pscustomobject]@{
            observedGeneration = 7; updatedReplicas = 1; readyReplicas = 1; availableReplicas = 1; unavailableReplicas = 0
        }
    }
}
function New-TestSignerReplicaSet([string]$Name, [int]$SignerRevision, [int]$LoginRevision, [string]$ConfigName = 'pandora-config', [int]$Replicas = 1) {
    $deployment = New-TestSignerDeployment $Name $SignerRevision $LoginRevision $ConfigName
    return [pscustomobject]@{
        kind = 'ReplicaSet'
        metadata = [pscustomobject]@{
            name = "$Name-rs"; namespace = 'pandora'; uid = "uid-$Name-rs"; labels = [pscustomobject]@{ app = $Name }
            ownerReferences = @([pscustomobject]@{
                kind = 'Deployment'; name = $Name; uid = "deployment-$Name"; controller = $true
            })
        }
        spec = [pscustomobject]@{ replicas = $Replicas; template = $deployment.spec.template }
        status = [pscustomobject]@{ replicas = $Replicas; readyReplicas = $Replicas; availableReplicas = $Replicas; fullyLabeledReplicas = $Replicas }
    }
}
function New-TestSignerPod([string]$Name, [int]$SignerRevision, [int]$LoginRevision, [string]$ConfigName = 'pandora-config') {
    $deployment = New-TestSignerDeployment $Name $SignerRevision $LoginRevision $ConfigName
    return [pscustomobject]@{
        kind = 'Pod'
        metadata = [pscustomobject]@{
            name = "$Name-pod"; namespace = 'pandora'; labels = [pscustomobject]@{ app = $Name }
            ownerReferences = @([pscustomobject]@{
                kind = 'ReplicaSet'; name = "$Name-rs"; uid = "uid-$Name-rs"; controller = $true
            })
        }
        spec = $deployment.spec.template.spec
        status = [pscustomobject]@{ phase = 'Running'; conditions = @([pscustomobject]@{ type = 'Ready'; status = 'True' }) }
    }
}
function New-TestConfigSecret([string]$Kid, [int]$LoginRevision) {
    $data = [ordered]@{}
    foreach ($service in @('login', 'matchmaker', 'matchmaker-pve', 'hub-allocator')) {
        $indent = if ($service -ceq 'login') { '  ' } else { '' }
        $revision = if ($service -ceq 'login') { "`n${indent}  jwks_file: `"/run/config/pandora-dsticket/jwks.json`"`n${indent}  keyset_revision: `"$LoginRevision`"" } else { '' }
        $text = if ($service -ceq 'login') { "login:`n${indent}ds_ticket:`n${indent}  private_key_file: `"/run/secrets/pandora-dsticket/private.pem`"`n${indent}  active_kid: `"$Kid`"`n${indent}  ttl: `"120s`"$revision`nserver:`n  addr: 0.0.0.0" } else { "ds_ticket:`n  private_key_file: `"/run/secrets/pandora-dsticket/private.pem`"`n  active_kid: `"$Kid`"`n  ttl: `"120s`"`nserver:`n  addr: 0.0.0.0" }
        $data["$service.yaml"] = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($text))
    }
    $data['unrelated.yaml'] = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes('keep: true'))
    return [pscustomobject]@{
        kind = 'Secret'; metadata = [pscustomobject]@{ name = 'pandora-config'; namespace = 'pandora'; resourceVersion = '100' }
        data = [pscustomobject]$data
    }
}

$tmpRoot = Join-Path ([IO.Path]::GetTempPath()) ('pandora-dsticket-rotation-' + [guid]::NewGuid().ToString('N'))
$k1Dir = Join-Path $tmpRoot 'k1'
$stageDir = Join-Path $tmpRoot 'stage'
$promoteDir = Join-Path $tmpRoot 'promote'
$retireDir = Join-Path $tmpRoot 'retire'
try {
    Push-Location $ProjectRoot
    try {
        & go run ./tools/dsticketkeys -out $k1Dir -revision 1 *> $null
        if ($LASTEXITCODE -ne 0) { throw '测试生成 K1 失败。' }
        $k1JwksPath = Join-Path $k1Dir 'jwks.json'
        $k1Pem = Get-Content -LiteralPath (Join-Path $k1Dir 'private.pem') -Raw
        $k1 = Get-PandoraDSTicketPrivateKeyPublicContract -PrivateKeyPem $k1Pem
        & go run ./tools/dsticketkeys -out $stageDir -revision 2 -merge $k1JwksPath -active-kid $k1.Kid *> $null
        if ($LASTEXITCODE -ne 0) { throw '测试生成 stage 材料失败。' }
        & go run ./tools/dsticketkeys -out $promoteDir -revision 3 `
            -private-in (Join-Path $stageDir 'private.pem') -merge (Join-Path $stageDir 'jwks.json') *> $null
        if ($LASTEXITCODE -ne 0) { throw '测试生成 promote 材料失败。' }
        & go run ./tools/dsticketkeys -out $retireDir -revision 4 `
            -private-in (Join-Path $stageDir 'private.pem') *> $null
        if ($LASTEXITCODE -ne 0) { throw '测试生成 retire 材料失败。' }
    } finally { Pop-Location }

    $k1Pem = Get-Content -LiteralPath (Join-Path $k1Dir 'private.pem') -Raw
    $k2Pem = Get-Content -LiteralPath (Join-Path $stageDir 'private.pem') -Raw
    $stageJwks = Get-Content -LiteralPath (Join-Path $stageDir 'jwks.json') -Raw
    $promoteJwks = Get-Content -LiteralPath (Join-Path $promoteDir 'jwks.json') -Raw
    $retireJwks = Get-Content -LiteralPath (Join-Path $retireDir 'jwks.json') -Raw
    $oldSecret = New-TestSecret $k1Pem 1
    $stageSecret = New-TestSecret $k2Pem 2
    $promoteSecret = New-TestSecret $k2Pem 3
    $retireSecret = New-TestSecret $k2Pem 4
    $stageDefault = New-TestConfigMap $stageJwks 2 default
    $stagePandora = New-TestConfigMap $stageJwks 2 pandora
    $promoteDefault = New-TestConfigMap $promoteJwks 3 default
    $promotePandora = New-TestConfigMap $promoteJwks 3 pandora
    $retireDefault = New-TestConfigMap $retireJwks 4 default
    $retirePandora = New-TestConfigMap $retireJwks 4 pandora

    # 阶段顺序与材料连续性。
    Assert-PandoraDSTicketRotationRevisionPlan -StageRevision 2 -PromoteRevision 3 -RetireRevision 4 `
        -OldSignerRevision 1
    Assert-Throws {
        Assert-PandoraDSTicketRotationRevisionPlan -StageRevision 3 -PromoteRevision 2 -RetireRevision 4 `
            -OldSignerRevision 1
    } '拒绝逆序阶段' '严格递增'
    $stage = Assert-PandoraDSTicketStageMaterialContract -OldSignerSecret $oldSecret -StageSignerSecret $stageSecret `
        -DefaultConfigMap $stageDefault -PandoraConfigMap $stagePandora -StageRevision 2 `
        -OldSignerRevision 1
    Assert-True ($stage.ActiveKid -ceq $stage.OldKid -and $stage.NewKid -cne $stage.OldKid) 'stage 保持 K1 active'
    $promote = Assert-PandoraDSTicketPromoteMaterialContract -OldSignerSecret $oldSecret `
        -StageSignerSecret $stageSecret -PromoteSignerSecret $promoteSecret `
        -StageDefaultConfigMap $stageDefault -StagePandoraConfigMap $stagePandora `
        -PromoteDefaultConfigMap $promoteDefault -PromotePandoraConfigMap $promotePandora `
        -StageRevision 2 -PromoteRevision 3 -OldSignerRevision 1
    Assert-True ($promote.ActiveKid -ceq $stage.NewKid) 'promote 激活同一 K2'
    $retire = Assert-PandoraDSTicketRetireMaterialContract -OldSignerSecret $oldSecret `
        -StageSignerSecret $stageSecret -PromoteSignerSecret $promoteSecret -RetireSignerSecret $retireSecret `
        -StageDefaultConfigMap $stageDefault -StagePandoraConfigMap $stagePandora `
        -PromoteDefaultConfigMap $promoteDefault -PromotePandoraConfigMap $promotePandora `
        -RetireDefaultConfigMap $retireDefault -RetirePandoraConfigMap $retirePandora `
        -StageRevision 2 -PromoteRevision 3 -RetireRevision 4 -OldSignerRevision 1
    Assert-True ($retire.ActiveKid -ceq $stage.NewKid) 'retire 仅保留同一 K2'
    Assert-Throws {
        Assert-PandoraDSTicketStageMaterialContract -OldSignerSecret $oldSecret -StageSignerSecret $stageSecret `
            -DefaultConfigMap $promoteDefault -PandoraConfigMap $promotePandora -StageRevision 3 `
            -OldSignerRevision 1
    } '禁止跳过 stage 直接 promote' 'active kid'
    Assert-Throws {
        Assert-PandoraDSTicketRetireMaterialContract -OldSignerSecret $oldSecret `
            -StageSignerSecret $stageSecret -PromoteSignerSecret $promoteSecret -RetireSignerSecret $retireSecret `
            -StageDefaultConfigMap $stageDefault -StagePandoraConfigMap $stagePandora `
            -PromoteDefaultConfigMap $promoteDefault -PromotePandoraConfigMap $promotePandora `
            -RetireDefaultConfigMap $promoteDefault -RetirePandoraConfigMap $promotePandora `
            -StageRevision 2 -PromoteRevision 3 -RetireRevision 3 -OldSignerRevision 1
    } '拒绝未清 K1 的 retire' '大于 promote'

    # pandora-config 只改四个 signer 的 DSTicket 字段，保留其它 data。
    $configSecret = New-TestConfigSecret $stage.OldKid 1
    $deletingConfigSecret = Copy-TestObject $configSecret
    $deletingConfigSecret.metadata | Add-Member -NotePropertyName deletionTimestamp -NotePropertyValue '2026-07-13T12:00:00Z'
    Assert-Throws {
        Get-PandoraDSTicketConfigSecretUpdatedData -SecretObject $deletingConfigSecret -ActiveKid $stage.OldKid `
            -LoginKeysetRevision 2 -AllowedCurrentActiveKids @($stage.OldKid)
    } 'fixed pandora-config deletionTimestamp 时禁止生成更新数据' '正在删除'
    Assert-Throws {
        Get-PandoraDSTicketConfigSubcontract $deletingConfigSecret
    } 'fixed pandora-config deletionTimestamp 时禁止读取子契约' '正在删除'
    Assert-Throws {
        Assert-PandoraDSTicketConfigSecretContract $deletingConfigSecret $stage.OldKid 1
    } 'fixed pandora-config deletionTimestamp 时通用 signer 配置契约也阻断' '正在删除'
    $wrongFixedKind = Copy-TestObject $configSecret; $wrongFixedKind.kind = 'ConfigMap'
    $wrongFixedName = Copy-TestObject $configSecret; $wrongFixedName.metadata.name = 'pandora-config-forged'
    $wrongFixedNamespace = Copy-TestObject $configSecret; $wrongFixedNamespace.metadata.namespace = 'default'
    foreach ($wrongIdentity in @($wrongFixedKind, $wrongFixedName, $wrongFixedNamespace)) {
        Assert-Throws { Get-PandoraDSTicketConfigSubcontract $wrongIdentity } `
            'fixed 子契约 wrong kind/name/namespace 必须阻断' '必须是 pandora/Secret/pandora-config'
        Assert-Throws {
            Assert-PandoraDSTicketOrdinaryMarkerState 1 @() @() $wrongIdentity
        } 'ordinary marker state 不得绕过 fixed identity' '必须是 pandora/Secret/pandora-config'
    }
    $revisionedProjected = New-PandoraDSTicketRevisionedConfigSecretObject -SourceSecret $configSecret `
        -Revision 2 -ActiveKid $stage.OldKid -AllowedCurrentActiveKids @($stage.OldKid)
    $revisionedProjectedObject = [pscustomobject]($revisionedProjected | ConvertTo-Json -Depth 50 | ConvertFrom-Json)
    $projectedDataHash = Get-PandoraSecretDataSha256 $revisionedProjectedObject.data
    $null = Assert-PandoraDSTicketRevisionedConfigSecretContract $revisionedProjectedObject 2 $stage.OldKid $projectedDataHash
    $staleRevisioned = Copy-TestObject $revisionedProjectedObject
    $staleRevisioned.data.'unrelated.yaml' = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes('tampered: true'))
    $staleRevisioned.metadata.annotations.'pandora.dev/dsticket-config-data-sha256' = `
        Get-PandoraSecretDataSha256 $staleRevisioned.data
    $null = Assert-PandoraDSTicketRevisionedConfigSecretContract $staleRevisioned 2 $stage.OldKid
    Assert-Throws {
        Assert-PandoraDSTicketRevisionedConfigSecretContract $staleRevisioned 2 $stage.OldKid $projectedDataHash
    } 'self-consistent stale revisioned Secret 仍须匹配当前 fixed 完整 data 投影' '当前 fixed pandora-config 投影'
    $sourceRvDrift = Copy-TestObject $revisionedProjectedObject
    $sourceRvDrift.metadata.annotations.'pandora.dev/dsticket-config-source-resource-version' = '999'
    $null = Assert-PandoraDSTicketRevisionedConfigSecretContract $sourceRvDrift 2 $stage.OldKid $projectedDataHash

    $nestedMatchmaker = Copy-TestObject $configSecret
    $matchmakerText = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($nestedMatchmaker.data.'matchmaker.yaml'))
    $nestedMatchmakerText = "unused:`n" + (($matchmakerText -split '\r?\n' | ForEach-Object { "  $_" }) -join "`n")
    $nestedMatchmaker.data.'matchmaker.yaml' = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($nestedMatchmakerText))
    Assert-Throws {
        Assert-PandoraDSTicketConfigSecretContract $nestedMatchmaker $stage.OldKid 1
    } 'matchmaker ds_ticket 藏入 unused 下必须阻断' '必须是顶级'
    $topLevelLogin = Copy-TestObject $configSecret
    $topLevelLoginText = "ds_ticket:`n  private_key_file: `"/run/secrets/pandora-dsticket/private.pem`"`n" +
        "  active_kid: `"$($stage.OldKid)`"`n  ttl: `"120s`"`n" +
        "  jwks_file: `"/run/config/pandora-dsticket/jwks.json`"`n  keyset_revision: `"1`"`nserver:`n  addr: 0.0.0.0"
    $topLevelLogin.data.'login.yaml' = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($topLevelLoginText))
    Assert-Throws {
        Assert-PandoraDSTicketConfigSecretContract $topLevelLogin $stage.OldKid 1
    } 'Login ds_ticket 移到顶级必须阻断' '顶级 login'
    $badPrivatePath = Copy-TestObject $configSecret
    $badPrivateText = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($badPrivatePath.data.'matchmaker.yaml')).Replace(
        '/run/secrets/pandora-dsticket/private.pem', '/tmp/private.pem')
    $badPrivatePath.data.'matchmaker.yaml' = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($badPrivateText))
    Assert-Throws { Assert-PandoraDSTicketConfigSecretContract $badPrivatePath $stage.OldKid 1 } `
        'private_key_file 非 canonical 路径必须阻断' 'private_key_file'
    $badTTL = Copy-TestObject $configSecret
    $badTTLText = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($badTTL.data.'hub-allocator.yaml')).Replace(
        'ttl: "120s"', 'ttl: "181s"')
    $badTTL.data.'hub-allocator.yaml' = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($badTTLText))
    Assert-Throws { Assert-PandoraDSTicketConfigSecretContract $badTTL $stage.OldKid 1 } `
        'ttl 超过 180s 必须阻断' '0 < ttl <= 180s'
    $stageData = Get-PandoraDSTicketConfigSecretUpdatedData -SecretObject $configSecret -ActiveKid $stage.OldKid `
        -LoginKeysetRevision 2 -AllowedCurrentActiveKids @($stage.OldKid)
    $configSecret.data = [pscustomobject]$stageData
    Assert-PandoraDSTicketConfigSecretContract -SecretObject $configSecret -ExpectedActiveKid $stage.OldKid `
        -ExpectedLoginKeysetRevision 2
    $promoteData = Get-PandoraDSTicketConfigSecretUpdatedData -SecretObject $configSecret -ActiveKid $stage.NewKid `
        -LoginKeysetRevision 3 -AllowedCurrentActiveKids @($stage.OldKid, $stage.NewKid)
    $configSecret.data = [pscustomobject]$promoteData
    Assert-PandoraDSTicketConfigSecretContract -SecretObject $configSecret -ExpectedActiveKid $stage.NewKid `
        -ExpectedLoginKeysetRevision 3
    Assert-True ([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($configSecret.data.'unrelated.yaml')) -ceq 'keep: true') '保留无关 Secret data'
    Assert-Throws {
        Get-PandoraDSTicketConfigSecretUpdatedData -SecretObject $configSecret -ActiveKid $stage.OldKid `
            -LoginKeysetRevision 2 -AllowedCurrentActiveKids @($stage.OldKid)
    } '拒绝未知当前 active kid' '不在本阶段允许集合'

    # 四 Fleet + 所有 live Ready/Allocated/Reserved GameServer/Pod 的纯对象门禁。
    $fleets = @()
    $gameServers = @()
    $gameServerSets = @()
    $pods = @()
    $states = @('Ready', 'Allocated', 'Reserved', 'Ready')
    for ($i = 0; $i -lt $script:PandoraDSTicketFleetNames.Count; $i++) {
        $fleetName = $script:PandoraDSTicketFleetNames[$i]
        $gsName = "gs-$i"
        $fleets += New-TestFleet $fleetName 2
        $gameServerSets += New-TestGameServerSet "$fleetName-gss" $fleetName 2
        $gameServers += New-TestGameServer $gsName $fleetName $states[$i] 2
        $pods += New-TestDSPod $gsName $fleetName 2
    }
    $gate = Assert-PandoraDSTicketLiveDSRevisionGate -FleetObjects $fleets -GameServerObjects $gameServers `
        -GameServerSetObjects $gameServerSets -PodObjects $pods -TargetRevision 2
    Assert-True ($gate.LiveGameServers.Count -eq 4) '四类 live GameServer 均纳入门禁'

    $wrongGS = Copy-TestObject $gameServers
    $wrongGS[1].spec.template.spec.containers[0].env[1].value = '1'
    $wrong = Test-PandoraDSTicketLiveDSRevisionGate -FleetObjects $fleets -GameServerObjects $wrongGS `
        -GameServerSetObjects $gameServerSets -PodObjects $pods -TargetRevision 2
    Assert-True (-not $wrong.Ok -and ($wrong.Failures -join ';') -like '*GameServer/gs-1*') 'Allocated 旧 revision 列出对象名'
    $missingPod = @($pods | Where-Object { $_.metadata.name -cne 'gs-2' })
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate -FleetObjects $fleets -GameServerObjects $gameServers `
            -GameServerSetObjects $gameServerSets -PodObjects $missingPod -TargetRevision 2
    } '缺对应 Pod fail-closed' 'GameServer/gs-2'
    $transient = Copy-TestObject $gameServers
    $transient[0].status.state = 'Creating'
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate -FleetObjects $fleets -GameServerObjects $transient `
            -GameServerSetObjects $gameServerSets -PodObjects $pods -TargetRevision 2
    } '过渡态不放行' 'GameServer/gs-0'
    $terminatingOld = New-TestGameServer 'old-draining' 'pandora-battle-stable' 'Allocated' 1
    $terminatingOld.metadata | Add-Member -NotePropertyName deletionTimestamp -NotePropertyValue '2026-07-13T00:00:00Z'
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate -FleetObjects $fleets `
            -GameServerObjects @($gameServers + $terminatingOld) -GameServerSetObjects $gameServerSets `
            -PodObjects $pods -TargetRevision 2
    } 'terminating Allocated 仍可能服务，必须等真消失且不得强杀' 'old-draining'
    $legacy = New-TestGameServer 'legacy-live' 'pandora-battle' 'Allocated' 2
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate -FleetObjects $fleets `
            -GameServerObjects @($gameServers + $legacy) -GameServerSetObjects $gameServerSets `
            -PodObjects $pods -TargetRevision 2
    } 'legacy live Fleet 拒绝' 'GameServer/legacy-live'
    $badJwksFile = Copy-TestObject $pods
    $badJwksFile[0].spec.containers[0].env[0].value = '/tmp/jwks.json'
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate -FleetObjects $fleets -GameServerObjects $gameServers `
            -GameServerSetObjects $gameServerSets -PodObjects $badJwksFile -TargetRevision 2
    } 'JWKS_FILE 必须精确路径' 'PANDORA_DSTICKET_JWKS_FILE'
    $privateInput = Copy-TestObject $pods
    $privateInput[0].spec.containers[0].env += [pscustomobject]@{ name = 'PANDORA_DSTICKET_PRIVATE_KEY'; value = 'forbidden' }
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate -FleetObjects $fleets -GameServerObjects $gameServers `
            -GameServerSetObjects $gameServerSets -PodObjects $privateInput -TargetRevision 2
    } 'DS 禁止 signer/private 输入' 'signer/private/oct'
    $forbiddenContainerMutants = @(
        [pscustomobject]@{ Group = 'initContainers'; Container = [pscustomobject]@{
            name = 'init-player-secret'; env = @([pscustomobject]@{ name = 'PANDORA_PLAYER_JWT_SECRET'; value = 'x' })
        } },
        [pscustomobject]@{ Group = 'ephemeralContainers'; Container = [pscustomobject]@{
            name = 'debug-ds-secret'; env = @([pscustomobject]@{ name = 'PANDORA_DS_TICKET_SECRET'; value = 'x' })
        } },
        [pscustomobject]@{ Group = 'initContainers'; Container = [pscustomobject]@{
            name = 'init-private-name'; env = @([pscustomobject]@{ name = 'PANDORA_DSTICKET_PRIVATE_KEY'; value = 'x' })
        } },
        [pscustomobject]@{ Group = 'initContainers'; Container = [pscustomobject]@{
            name = 'init-pem'; env = @([pscustomobject]@{ name = 'OPAQUE'; value = '-----BEGIN PRIVATE KEY-----' })
        } },
        [pscustomobject]@{ Group = 'ephemeralContainers'; Container = [pscustomobject]@{
            name = 'debug-oct'; env = @([pscustomobject]@{ name = 'OPAQUE'; value = '{"kty":"oct"}' })
        } },
        [pscustomobject]@{ Group = 'ephemeralContainers'; Container = [pscustomobject]@{
            name = 'debug-private-mount'; volumeMounts = @([pscustomobject]@{ name = 'opaque'; mountPath = '/etc/pandora/dsticket/private' })
        } }
    )
    foreach ($mutant in $forbiddenContainerMutants) {
        $mutatedPods = Copy-TestObject $pods
        $mutatedPods[0].spec | Add-Member -NotePropertyName $mutant.Group -NotePropertyValue @($mutant.Container)
        Assert-Throws {
            Assert-PandoraDSTicketLiveDSRevisionGate $fleets $gameServers $gameServerSets $mutatedPods 2
        } "$($mutant.Group) forbidden signer/private 输入必须阻断" 'signer/private/oct'
    }
    $initOldVerifier = Copy-TestObject $pods
    $initOldVerifier[0].spec.volumes += [pscustomobject]@{
        name = 'old-projected-jwks'; projected = [pscustomobject]@{ sources = @([pscustomobject]@{
            configMap = [pscustomobject]@{ name = 'pandora-dsticket-jwks-r1' }
        }) }
    }
    $initOldVerifier[0].spec | Add-Member -NotePropertyName initContainers -NotePropertyValue @([pscustomobject]@{
        name = 'old-verifier-init'; volumeMounts = @([pscustomobject]@{ name = 'old-projected-jwks'; mountPath = '/keys' })
    })
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate $fleets $gameServers $gameServerSets $initOldVerifier 2
    } 'canonical r2 不能掩盖 init projected old r1' '非 canonical verifier'
    $sidecarOldVerifier = Copy-TestObject $pods
    $sidecarOldVerifier[0].spec.containers += [pscustomobject]@{
        name = 'old-verifier-sidecar'; envFrom = @([pscustomobject]@{
            configMapRef = [pscustomobject]@{ name = 'pandora-dsticket-jwks-r1' }
        })
    }
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate $fleets $gameServers $gameServerSets $sidecarOldVerifier 2
    } 'canonical r2 不能掩盖 sidecar envFrom old r1' '非 canonical verifier'
    $csiVerifier = Copy-TestObject $pods
    $csiVerifier[0].spec.volumes += [pscustomobject]@{
        name = 'external-dsticket'; csi = [pscustomobject]@{
            driver = 'secrets-store.csi.k8s.io'
            volumeAttributes = [pscustomobject]@{ secretProviderClass = 'Pandora-DSTicket-External' }
        }
    }
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate $fleets $gameServers $gameServerSets $csiVerifier 2
    } 'external secret-store CSI DSTicket 输入必须阻断' 'ForbiddenCSIVolume'
    $opaqueDirectPrivate = Copy-TestObject $pods
    $opaqueDirectPrivate[0].spec.volumes += [pscustomobject]@{
        name = 'opaque-secret'; secret = [pscustomobject]@{
            secretName = 'unrelated-name'; items = @([pscustomobject]@{ key = 'private.pem'; path = 'renamed' })
        }
    }
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate $fleets $gameServers $gameServerSets $opaqueDirectPrivate 2
    } 'opaque direct Secret items private.pem 必须阻断' 'ForbiddenSecretVolumeItem'
    $opaqueProjectedPrivate = Copy-TestObject $pods
    $opaqueProjectedPrivate[0].spec.volumes += [pscustomobject]@{
        name = 'opaque-projected'; projected = [pscustomobject]@{ sources = @([pscustomobject]@{
            secret = [pscustomobject]@{
                name = 'unrelated-name'; items = @([pscustomobject]@{ key = 'renamed'; path = 'keys/signing_key' })
            }
        }) }
    }
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate $fleets $gameServers $gameServerSets $opaqueProjectedPrivate 2
    } 'opaque projected Secret items signing path 必须阻断' 'ForbiddenProjectedSecretItem'
    $opaqueEnvPrivate = Copy-TestObject $pods
    $opaqueEnvPrivate[0].spec | Add-Member -NotePropertyName initContainers -NotePropertyValue @([pscustomobject]@{
        name = 'opaque-key-loader'; env = @([pscustomobject]@{
            name = 'FOO'; valueFrom = [pscustomobject]@{
                secretKeyRef = [pscustomobject]@{ name = 'unrelated-name'; key = 'jwt_secret' }
            }
        })
    })
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate $fleets $gameServers $gameServerSets $opaqueEnvPrivate 2
    } 'opaque init secretKeyRef jwt_secret 必须阻断' 'ForbiddenInitContainerSecretKey'
    $malformedSignerPrefix = Copy-TestObject $pods
    $malformedSignerPrefix[0].spec.volumes += [pscustomobject]@{
        name = 'malformed-private'; secret = [pscustomobject]@{ secretName = 'pandora-dsticket-signer-r01' }
    }
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate $fleets $gameServers $gameServerSets $malformedSignerPrefix 2
    } '畸形 signer 保留前缀必须进入 DS fail-closed gate' 'pandora-dsticket-signer-r01'
    $malformedJwksPrefix = Copy-TestObject $pods
    $malformedJwksPrefix[0].spec.volumes += [pscustomobject]@{
        name = 'malformed-public'; configMap = [pscustomobject]@{ name = 'pandora-dsticket-jwks-r0' }
    }
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate $fleets $gameServers $gameServerSets $malformedJwksPrefix 2
    } '畸形 JWKS 保留前缀必须进入 DS fail-closed gate' '非 canonical verifier'
    $orphan = New-TestDSPod 'orphan-battle-ds' 'pandora-battle-stable' 2
    $orphan.metadata.labels.'agones.dev/gameserver' = 'missing-owner'
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate -FleetObjects $fleets -GameServerObjects $gameServers `
            -GameServerSetObjects $gameServerSets -PodObjects @($pods + $orphan) -TargetRevision 2
    } '阻断无受管 GS owner 的孤儿 DS Pod' 'Pod/orphan-battle-ds'

    $wrongGSSOwner = Copy-TestObject $gameServerSets
    $wrongGSSOwner[0].metadata.ownerReferences[0].uid = 'forged-fleet-uid'
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate $fleets $gameServers $wrongGSSOwner $pods 2
    } 'GSS 必须 exact owner Fleet UID' 'UID 漂移/孤儿'
    $emptyGSSUid = Copy-TestObject $gameServerSets
    $emptyGSSUid[0].metadata.uid = ''
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate $fleets $gameServers $emptyGSSUid $pods 2
    } 'GSS UID 不得为空' '缺 UID'
    $multiGSSController = Copy-TestObject $gameServerSets
    $multiGSSController[0].metadata.ownerReferences += [pscustomobject]@{
        kind = 'Deployment'; name = 'forged'; uid = 'forged'; controller = $true
    }
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate $fleets $gameServers $multiGSSController $pods 2
    } 'GSS 所有 controller refs 总数必须为一' '恰有一个'
    $zeroOldGSS = New-TestGameServerSet 'pandora-battle-stable-old-zero' 'pandora-battle-stable' 1 0
    $zeroOldGSS.spec.template.spec = [pscustomobject]@{} # 安全零容量历史对象可没有现行 tuple。
    $null = Assert-PandoraDSTicketLiveDSRevisionGate $fleets $gameServers @($gameServerSets + $zeroOldGSS) $pods 2
    $deletingZeroGSS = Copy-TestObject $zeroOldGSS
    $deletingZeroGSS.metadata | Add-Member -NotePropertyName deletionTimestamp -NotePropertyValue '2026-07-13T12:00:00Z'
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate $fleets $gameServers @($gameServerSets + $deletingZeroGSS) $pods 2
    } 'terminating 零容量 GSS 仍阻断' '正在终止'
    $wrongGSOwner = Copy-TestObject $gameServers
    $wrongGSOwner[0].metadata.ownerReferences[0].uid = 'forged-gss-uid'
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate $fleets $wrongGSOwner $gameServerSets $pods 2
    } 'GS label/同名不能替代 exact GSS owner UID' 'UID 漂移/孤儿'
    $wrongPodOwner = Copy-TestObject $pods
    $wrongPodOwner[0].metadata.ownerReferences = @()
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate $fleets $gameServers $gameServerSets $wrongPodOwner 2
    } 'Pod 同名/label 不能替代 exact GS owner UID' 'GameServer/gs-0'
    $terminalOrphan = Copy-TestObject $gameServers[0]
    $terminalOrphan.metadata.name = 'terminal-orphan'; $terminalOrphan.metadata.uid = 'uid-terminal-orphan'
    $terminalOrphan.metadata.ownerReferences = @(); $terminalOrphan.status.state = 'Shutdown'
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate $fleets @($gameServers + $terminalOrphan) $gameServerSets $pods 2
    } 'terminal GS 也必须有合法 owner 链' 'terminal-orphan'
    $rogueFleet = New-TestFleet 'pandora-battle-rogue' 2
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate @($fleets + $rogueFleet) $gameServers $gameServerSets $pods 2
    } '未知 DS parent Fleet 即使尚无 GSS/GS 也阻断' '未知 DSTicket/DS parent'
    $foreignRevisionFleets = Copy-TestObject $fleets
    $foreignRevisionFleets[0].spec.template.spec.template.spec.containers[0].env[1].value = '99'
    $foreignRevisionFleets[0].spec.template.spec.template.spec.volumes[0].configMap.name = 'pandora-dsticket-jwks-r99'
    Assert-Throws {
        Assert-PandoraDSTicketDSRevisionSetGate $foreignRevisionFleets $gameServers $gameServerSets $pods @(1, 2)
    } 'phase 写前阻断外来 r99 Fleet tuple' '不属于允许 revision'
    $projectedOrphanPod = [pscustomobject]@{
        kind = 'Pod'
        metadata = [pscustomobject]@{ name = 'renamed-projector'; namespace = 'default'; ownerReferences = @() }
        spec = [pscustomobject]@{
            containers = @([pscustomobject]@{
                name = 'renamed-worker'
                envFrom = @([pscustomobject]@{ configMapRef = [pscustomobject]@{ name = 'pandora-dsticket-jwks' } })
            })
            volumes = @([pscustomobject]@{
                name = 'opaque'
                projected = [pscustomobject]@{ sources = @([pscustomobject]@{
                    configMap = [pscustomobject]@{ name = 'pandora-dsticket-jwks' }
                }) }
            })
        }
    }
    Assert-True (Test-PandoraDSTicketDSPodSpecClue $projectedOrphanPod.spec) 'projected/envFrom 改名 Pod 仍被 verifier clue 捕获'
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate $fleets $gameServers $gameServerSets @($pods + $projectedOrphanPod) 2
    } 'projected/envFrom 改名孤儿 Pod 阻断' 'renamed-projector'
    $projectedPrivateClue = [pscustomobject]@{
        containers = @([pscustomobject]@{ name = 'renamed-private-consumer' })
        volumes = @([pscustomobject]@{
            name = 'opaque'; projected = [pscustomobject]@{ sources = @([pscustomobject]@{
                secret = [pscustomobject]@{ name = 'pandora-dsticket' }
            }) }
        })
    }
    Assert-True (Test-PandoraDSTicketDSPodSpecClue $projectedPrivateClue) 'projected legacy signer Secret 也属于 DS/parent 相关线索'
    $secretBackedVerifier = [pscustomobject]@{
        containers = @([pscustomobject]@{
            name = 'renamed-public-verifier'
            env = @([pscustomobject]@{
                name = 'OPAQUE'; valueFrom = [pscustomobject]@{
                    secretKeyRef = [pscustomobject]@{ name = 'pandora-dsticket-jwks-r1'; key = 'jwks.json' }
                }
            })
            envFrom = @([pscustomobject]@{ secretRef = [pscustomobject]@{ name = 'pandora-dsticket-jwks' } })
        })
        volumes = @(
            [pscustomobject]@{ name = 'direct-public'; secret = [pscustomobject]@{ secretName = 'pandora-dsticket-jwks-r1' } },
            [pscustomobject]@{ name = 'projected-public'; projected = [pscustomobject]@{ sources = @([pscustomobject]@{
                secret = [pscustomobject]@{ name = 'pandora-dsticket-jwks' }
            }) } }
        )
    }
    $secretVerifierReferences = @(Get-PandoraDSTicketVerifierMaterialReferences $secretBackedVerifier)
    foreach ($kind in @('JWKSSecretVolume', 'ProjectedJWKSSecret', 'VerifierEnv', 'VerifierEnvFrom')) {
        Assert-True (@($secretVerifierReferences | Where-Object { $_.Kind -ceq $kind }).Count -eq 1) "Secret-backed JWKS $kind 线索捕获"
    }
    Assert-True (Test-PandoraDSTicketDSPodSpecClue $secretBackedVerifier) 'Secret-backed public JWKS 四类引用属于 related clue'
    $forbiddenUnknownSpec = [pscustomobject]@{
        containers = @([pscustomobject]@{ name = 'renamed-worker' })
        initContainers = @([pscustomobject]@{
            name = 'private-init'; env = @([pscustomobject]@{ name = 'PANDORA_PLAYER_JWT_SECRET'; value = 'x' })
        })
        ephemeralContainers = @([pscustomobject]@{
            name = 'private-debug'; volumeMounts = @([pscustomobject]@{ name = 'opaque'; mountPath = '/run/secrets/pandora-dsticket' })
        })
        volumes = @([pscustomobject]@{
            name = 'external-private'; csi = [pscustomobject]@{
                volumeAttributes = [pscustomobject]@{ secretProviderClass = 'pandora-dsticket-private' }
            }
        })
    }
    Assert-True (Test-PandoraDSTicketDSPodSpecClue $forbiddenUnknownSpec) 'init/ephemeral/CSI forbidden 输入属于 unknown parent related clue'
    $lateFixedConsumerSpec = [pscustomobject]@{
        containers = @([pscustomobject]@{
            name = 'late-consumer'
            env = @([pscustomobject]@{ name = 'PANDORA_JWT_SECRET'; value = 'legacy' })
            envFrom = @([pscustomobject]@{ secretRef = [pscustomobject]@{ name = 'pandora-config' } })
        })
        volumes = @()
    }
    Assert-True (Test-PandoraDSTicketDSPodSpecClue $lateFixedConsumerSpec) `
        '入口检查后补出的 inline signer consumer 必须属于 broader clue'
    $lateFixedRefs = @(Get-PandoraDSTicketSignerConfigSecretReferences $lateFixedConsumerSpec | Where-Object {
        $_.Name -ceq 'pandora-config'
    })
    Assert-True ($lateFixedRefs.Count -eq 1 -and $lateFixedRefs[0].Kind -ceq 'EnvFromSecretRef') `
        '晚到 Pod 的 envFrom fixed config 必须被全引用 NoSigner gate 捕获'
    $forbiddenRogueFleet = New-TestFleet 'opaque-forbidden-parent' 2
    $forbiddenRogueFleet.spec.template.spec.template.spec = $forbiddenUnknownSpec
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate @($fleets + $forbiddenRogueFleet) $gameServers $gameServerSets $pods 2
    } '仅有 forbidden init/ephemeral/CSI 的 unknown Fleet 也必须阻断' '未知 DSTicket/DS parent'
    foreach ($capacity in @(0, 1)) {
        $projectedOrphanGSS = New-TestGameServerSet "rogue-projected-gss-$capacity" 'pandora-battle-stable' 1 $capacity
        $projectedOrphanGSS.metadata.ownerReferences = @()
        $projectedOrphanGSS.spec.template.spec.containers[0].name = 'renamed-worker'
        $projectedOrphanGSS.spec.template.spec.volumes = @([pscustomobject]@{
            name = 'opaque'; projected = [pscustomobject]@{ sources = @([pscustomobject]@{
                configMap = [pscustomobject]@{ name = 'pandora-dsticket-jwks-r1' }
            }) }
        })
        Assert-True (Test-PandoraDSTicketDSPodSpecClue $projectedOrphanGSS.spec.template.spec) '改名 GSS projected clue 捕获'
        Assert-Throws {
            Assert-PandoraDSTicketLiveDSRevisionGate $fleets $gameServers @($gameServerSets + $projectedOrphanGSS) $pods 2
        } "orphan projected GSS capacity=$capacity 均阻断" 'controller owner'
    }
    $projectedRogueFleet = New-TestFleet 'opaque-parent' 1
    $projectedRogueFleet.spec.template.spec.template.spec.containers[0].name = 'renamed-worker'
    $projectedRogueFleet.spec.template.spec.template.spec.volumes = @([pscustomobject]@{
        name = 'opaque'; projected = [pscustomobject]@{ sources = @([pscustomobject]@{
            configMap = [pscustomobject]@{ name = 'pandora-dsticket-jwks-r1' }
        }) }
    })
    Assert-True (Test-PandoraDSTicketDSPodSpecClue $projectedRogueFleet.spec.template.spec.template.spec) '未知 Fleet projected clue 捕获'
    Assert-Throws {
        Assert-PandoraDSTicketLiveDSRevisionGate @($fleets + $projectedRogueFleet) $gameServers $gameServerSets $pods 2
    } 'unknown projected Fleet parent 阻断' '未知 DSTicket/DS parent'

    # 四 signer rollout 全绿后才允许写激活 marker。
    $deployments = @()
    $replicaSets = @()
    $signerPods = @()
    foreach ($name in $script:PandoraDSTicketSignerNames) {
        $deployments += New-TestSignerDeployment $name 3 3
        $replicaSets += New-TestSignerReplicaSet $name 3 3
        $signerPods += New-TestSignerPod $name 3 3
    }
    Assert-PandoraDSTicketSignerDeploymentGate -DeploymentObjects $deployments -ReplicaSetObjects $replicaSets `
        -PodObjects $signerPods `
        -SignerRevision 3 -LoginKeysetRevision 3
    $extraSignerDeployment = Copy-TestObject $deployments
    $extraSignerDeployment[0].spec.template.spec | Add-Member -NotePropertyName initContainers -NotePropertyValue @([pscustomobject]@{
        name = 'old-signer-init'
        envFrom = @([pscustomobject]@{ secretRef = [pscustomobject]@{ name = 'pandora-dsticket-signer-r1' } })
    })
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate $extraSignerDeployment $replicaSets $signerPods 3 3
    } 'Deployment canonical r3 不能掩盖 init old signer r1' 'Deployment/login signer 私钥引用'
    $extraSignerReplicaSet = Copy-TestObject $replicaSets
    $extraSignerReplicaSet[1].spec.template.spec | Add-Member -NotePropertyName initContainers -NotePropertyValue @([pscustomobject]@{
        name = 'old-signer-init'
        env = @([pscustomobject]@{
            name = 'OPAQUE'; valueFrom = [pscustomobject]@{
                secretKeyRef = [pscustomobject]@{ name = 'pandora-dsticket-signer-r1'; key = 'private.pem' }
            }
        })
    })
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate $deployments $extraSignerReplicaSet $signerPods 3 3
    } '非零 RS canonical r3 不能掩盖 init old signer r1' '非零但未引用目标 signer Secret'
    $shadowConfigDeployment = Copy-TestObject $deployments
    $shadowConfigDeployment[0].spec.template.spec.volumes += [pscustomobject]@{
        name = 'shadow-config'; projected = [pscustomobject]@{ sources = @([pscustomobject]@{
            secret = [pscustomobject]@{ name = 'pandora-config' }
        }) }
    }
    $shadowConfigDeployment[0].spec.template.spec | Add-Member -NotePropertyName initContainers -NotePropertyValue @([pscustomobject]@{
        name = 'shadow-config-init'; envFrom = @([pscustomobject]@{ secretRef = [pscustomobject]@{ name = 'pandora-config' } })
    })
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate $shadowConfigDeployment $replicaSets $signerPods 3 3
    } 'Deployment canonical conf 不能掩盖 init envFrom/projected shadow fixed config' 'config 引用必须恰为一个'
    $shadowConfigReplicaSet = Copy-TestObject $replicaSets
    $shadowConfigReplicaSet[1].spec.template.spec.volumes += [pscustomobject]@{
        name = 'shadow-config'; projected = [pscustomobject]@{ sources = @([pscustomobject]@{
            secret = [pscustomobject]@{ name = 'pandora-config-dsticket-r1' }
        }) }
    }
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate $deployments $shadowConfigReplicaSet $signerPods 3 3
    } '非零 RS canonical conf 不能掩盖 projected old phase config' 'config 引用必须恰为一个'
    $shadowConfigPod = Copy-TestObject $signerPods
    $shadowConfigPod[2].spec.volumes += [pscustomobject]@{
        name = 'shadow-config'; secret = [pscustomobject]@{ secretName = 'pandora-config' }
    }
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate $deployments $replicaSets $shadowConfigPod 3 3
    } 'Running Pod canonical conf 不能掩盖额外 direct fixed config' 'config 引用必须恰为一个'
    $deploymentMountReuse = Copy-TestObject $deployments
    $deploymentMountReuse[0].spec.template.spec | Add-Member -NotePropertyName initContainers -NotePropertyValue @([pscustomobject]@{
        name = 'key-reuser'; volumeMounts = @([pscustomobject]@{ name = 'dsticket'; mountPath = '/tmp/key'; readOnly = $true })
    })
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate $deploymentMountReuse $replicaSets $signerPods 3 3
    } 'Deployment initContainer 复用 canonical dsticket volume 必须阻断' 'consumer mount'
    $replicaSetMountReuse = Copy-TestObject $replicaSets
    $replicaSetMountReuse[1].spec.template.spec | Add-Member -NotePropertyName ephemeralContainers -NotePropertyValue @([pscustomobject]@{
        name = 'key-reuser'; volumeMounts = @([pscustomobject]@{ name = 'dsticket'; mountPath = '/tmp/key'; readOnly = $true })
    })
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate $deployments $replicaSetMountReuse $signerPods 3 3
    } '非零 RS ephemeralContainer 复用 canonical dsticket volume 必须阻断' 'consumer mount'
    $podMountReuse = Copy-TestObject $signerPods
    $podMountReuse[3].spec.containers += [pscustomobject]@{
        name = 'key-reuser'; volumeMounts = @([pscustomobject]@{ name = 'dsticket'; mountPath = '/tmp/key'; readOnly = $true })
    }
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate $deployments $replicaSets $podMountReuse 3 3
    } 'Running Pod sidecar 复用 canonical dsticket volume 必须阻断' 'consumer mount'
    $loginOldVerifier = Copy-TestObject $deployments
    $loginOldVerifier[0].spec.template.spec.volumes += [pscustomobject]@{
        name = 'old-verifier'; projected = [pscustomobject]@{ sources = @([pscustomobject]@{
            configMap = [pscustomobject]@{ name = 'pandora-dsticket-jwks-r0' }
        }) }
    }
    $loginOldVerifier[0].spec.template.spec | Add-Member -NotePropertyName initContainers -NotePropertyValue @([pscustomobject]@{
        name = 'old-verifier-init'; envFrom = @([pscustomobject]@{
            configMapRef = [pscustomobject]@{ name = 'pandora-dsticket-jwks-r1' }
        })
    })
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate $loginOldVerifier $replicaSets $signerPods 3 3
    } 'Login canonical r3 不能掩盖 init projected/envFrom old或畸形 JWKS' 'Login verifier'
    $malformedSignerDeployment = Copy-TestObject $deployments
    $malformedSignerDeployment[1].spec.template.spec.volumes += [pscustomobject]@{
        name = 'backup-private'; secret = [pscustomobject]@{ secretName = 'pandora-dsticket-backup' }
    }
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate $malformedSignerDeployment $replicaSets $signerPods 3 3
    } 'signer 保留前缀 backup 也必须作为额外私钥引用阻断' 'signer 私钥引用'

    $runtimeConfigs = @{}
    foreach ($name in $script:PandoraDSTicketSignerNames) { $runtimeConfigs[$name] = 'pandora-config' }
    Assert-PandoraDSTicketRuntimeObjectGate $deployments $replicaSets $signerPods `
        $fleets $gameServers $gameServerSets $pods 3 3 $runtimeConfigs 2
    $mixedPromotedDeployments = Copy-TestObject $deployments
    $mixedPromotedDeployments[0].spec.template.spec.volumes[1].secret.secretName = 'pandora-dsticket-signer-r1'
    Assert-Throws {
        Assert-PandoraDSTicketRuntimeObjectGate $mixedPromotedDeployments $replicaSets $signerPods `
            $fleets $gameServers $gameServerSets $pods 3 3 $runtimeConfigs 2
    } 'activation marker 写前 before/promoted 混合 signer 状态必须阻断' 'signer'
    $terminalDriftFleets = Copy-TestObject $fleets
    $terminalDriftFleets[0].spec.template.spec.template.spec.containers[0].env[1].value = '1'
    $terminalDriftFleets[0].spec.template.spec.template.spec.volumes[0].configMap.name = 'pandora-dsticket-jwks-r1'
    Assert-Throws {
        Assert-PandoraDSTicketRuntimeObjectGate $deployments $replicaSets $signerPods `
            $terminalDriftFleets $gameServers $gameServerSets $pods 3 3 $runtimeConfigs 2
    } 'terminal marker 写前 DS 从 target 漂回旧 revision 必须阻断' 'Fleet/pandora-battle-stable'
    $unknownZeroDeployment = [pscustomobject]@{
        kind = 'Deployment'
        metadata = [pscustomobject]@{
            name = 'opaque-zero-private'; namespace = 'pandora'; uid = 'deployment-opaque-zero-private'; generation = 1
        }
        spec = [pscustomobject]@{
            replicas = 0; strategy = [pscustomobject]@{ type = 'RollingUpdate' }
            template = [pscustomobject]@{ spec = [pscustomobject]@{
                containers = @([pscustomobject]@{ name = 'opaque-worker' })
                initContainers = @([pscustomobject]@{
                    name = 'legacy-private-init'; env = @(
                        [pscustomobject]@{ name = 'PANDORA_JWT_SECRET'; value = 'legacy' },
                        [pscustomobject]@{ name = 'OPAQUE'; value = '-----BEGIN PRIVATE KEY-----' }
                    )
                })
                volumes = @()
            } }
        }
        status = [pscustomobject]@{
            observedGeneration = 1; updatedReplicas = 0; readyReplicas = 0; availableReplicas = 0; unavailableReplicas = 0
        }
    }
    Assert-True (Test-PandoraDSTicketDSPodSpecClue $unknownZeroDeployment.spec.template.spec) `
        'zero unknown Deployment 的 legacy env/inline private 属于 broader signer clue'
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate @($deployments + $unknownZeroDeployment) $replicaSets $signerPods 3 3
    } 'zero unknown Deployment 即使无标准 signer Secret 也阻断' '未知 DSTicket signer parent'
    $orphanClueReplicaSet = [pscustomobject]@{
        kind = 'ReplicaSet'
        metadata = [pscustomobject]@{
            name = 'opaque-orphan-rs'; namespace = 'pandora'; uid = 'uid-opaque-orphan-rs'
            labels = [pscustomobject]@{ app = 'opaque' }; ownerReferences = @()
        }
        spec = [pscustomobject]@{
            replicas = 0; template = [pscustomobject]@{ spec = [pscustomobject]@{
                containers = @([pscustomobject]@{ name = 'opaque-worker' })
                ephemeralContainers = @([pscustomobject]@{
                    name = 'private-debug'; volumeMounts = @([pscustomobject]@{
                        name = 'opaque-private'; mountPath = '/run/secrets/pandora-dsticket'
                    })
                })
                volumes = @([pscustomobject]@{
                    name = 'opaque-private'; csi = [pscustomobject]@{
                        volumeAttributes = [pscustomobject]@{ secretProviderClass = 'pandora-dsticket-legacy' }
                    }
                })
            } }
        }
        status = [pscustomobject]@{ replicas = 0; readyReplicas = 0; availableReplicas = 0; fullyLabeledReplicas = 0 }
    }
    Assert-True (Test-PandoraDSTicketDSPodSpecClue $orphanClueReplicaSet.spec.template.spec) `
        'orphan RS 的 CSI/private mount 属于 broader signer clue'
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate $deployments @($replicaSets + $orphanClueReplicaSet) $signerPods 3 3
    } '无标准 signer Secret 的 orphan RS 也阻断' 'controller owner'
    $orphanCluePod = [pscustomobject]@{
        kind = 'Pod'
        metadata = [pscustomobject]@{
            name = 'opaque-orphan-pod'; namespace = 'pandora'; labels = [pscustomobject]@{ app = 'opaque' }; ownerReferences = @()
        }
        spec = [pscustomobject]@{
            containers = @([pscustomobject]@{
                name = 'opaque-worker'; env = @([pscustomobject]@{ name = 'PANDORA_DS_TICKET_SECRET'; value = 'legacy' })
                envFrom = @([pscustomobject]@{ configMapRef = [pscustomobject]@{ name = 'pandora-dsticket-jwks-r1' } })
            })
            volumes = @([pscustomobject]@{
                name = 'old-public'; projected = [pscustomobject]@{ sources = @([pscustomobject]@{
                    configMap = [pscustomobject]@{ name = 'pandora-dsticket-jwks-r1' }
                }) }
            })
        }
        status = [pscustomobject]@{ phase = 'Running'; conditions = @([pscustomobject]@{ type = 'Ready'; status = 'True' }) }
    }
    Assert-True (Test-PandoraDSTicketDSPodSpecClue $orphanCluePod.spec) `
        'orphan Pod 的 legacy env/old JWKS projected-env 属于 broader signer clue'
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate $deployments $replicaSets @($signerPods + $orphanCluePod) 3 3
    } '无标准 signer Secret 的 orphan Pod 也阻断' '不是受管 signer'
    $notReady = Copy-TestObject $signerPods
    $notReady[2].status.conditions[0].status = 'False'
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate -DeploymentObjects $deployments -ReplicaSetObjects $replicaSets `
            -PodObjects $notReady `
            -SignerRevision 3 -LoginKeysetRevision 3
    } '未 Ready signer 不得记激活' 'matchmaker-pve-pod'
    $recreate = Copy-TestObject $deployments
    $recreate[0].spec.strategy.type = 'Recreate'
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate -DeploymentObjects $recreate -ReplicaSetObjects $replicaSets `
            -PodObjects $signerPods `
            -SignerRevision 3 -LoginKeysetRevision 3
    } 'Recreate 破坏不停服' 'Recreate'
    $unsafeRolling = Copy-TestObject $deployments
    $unsafeRolling[0].spec.strategy | Add-Member -NotePropertyName rollingUpdate -NotePropertyValue ([pscustomobject]@{
        maxUnavailable = 1; maxSurge = 0
    })
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate -DeploymentObjects $unsafeRolling -ReplicaSetObjects $replicaSets `
            -PodObjects $signerPods `
            -SignerRevision 3 -LoginKeysetRevision 3
    } '禁止单副本 signer 先下旧 Pod' '耗尽旧 signer'
    $paused = Copy-TestObject $deployments
    $paused[0].spec | Add-Member -NotePropertyName paused -NotePropertyValue $true
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate -DeploymentObjects $paused -ReplicaSetObjects $replicaSets `
            -PodObjects $signerPods `
            -SignerRevision 3 -LoginKeysetRevision 3
    } 'marker 前必须 resume' '仍处于 paused'
    $terminatingK1 = New-TestSignerPod login 1 2
    $terminatingK1.metadata.name = 'login-old-k1-terminating'
    $terminatingK1.metadata | Add-Member -NotePropertyName deletionTimestamp -NotePropertyValue '2026-07-13T12:00:00Z'
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate -DeploymentObjects $deployments `
            -ReplicaSetObjects $replicaSets -PodObjects @($signerPods + $terminatingK1) `
            -SignerRevision 3 -LoginKeysetRevision 3
    } 'marker 必须晚于最后一个 terminating K1 Pod 真正消失' 'login-old-k1-terminating'

    $wrongRSOwner = Copy-TestObject $replicaSets
    $wrongRSOwner[0].metadata.ownerReferences[0].uid = 'forged-deployment-uid'
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate $deployments $wrongRSOwner $signerPods 3 3
    } 'RS 必须 exact Deployment owner UID' 'UID 漂移/孤儿'
    $emptyRSUid = Copy-TestObject $replicaSets
    $emptyRSUid[0].metadata.uid = ''
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate $deployments $emptyRSUid $signerPods 3 3
    } 'RS UID 不得为空' '缺 UID'
    $multiRSController = Copy-TestObject $replicaSets
    $multiRSController[0].metadata.ownerReferences += [pscustomobject]@{
        kind = 'GameServerSet'; name = 'forged'; uid = 'forged'; controller = $true
    }
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate $deployments $multiRSController $signerPods 3 3
    } 'RS 所有 controller refs 总数必须为一' '恰有一个'
    $wrongSignerPodOwner = Copy-TestObject $signerPods
    $wrongSignerPodOwner[0].metadata.ownerReferences[0].uid = 'forged-rs-uid'
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate $deployments $replicaSets $wrongSignerPodOwner 3 3
    } 'signer Pod app label 不能替代 exact RS owner UID' 'UID 漂移/孤儿'
    $multiSignerPodController = Copy-TestObject $signerPods
    $multiSignerPodController[0].metadata.ownerReferences += [pscustomobject]@{
        kind = 'Deployment'; name = 'login'; uid = 'deployment-login'; controller = $true
    }
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate $deployments $replicaSets $multiSignerPodController 3 3
    } 'signer Pod 所有 controller refs 总数必须为一' '恰有一个'
    $zeroOldRS = New-TestSignerReplicaSet login 1 1 pandora-config 0
    $zeroOldRS.metadata.name = 'login-old-zero-rs'; $zeroOldRS.metadata.uid = 'uid-login-old-zero-rs'
    $zeroOldRS.spec.template.spec.volumes = @()
    $zeroOldRS.spec.template.spec | Add-Member -NotePropertyName initContainers -NotePropertyValue @([pscustomobject]@{
        name = 'historical-old-signer'; envFrom = @([pscustomobject]@{
            secretRef = [pscustomobject]@{ name = 'pandora-dsticket-signer-r1' }
        })
    }) # 安全零容量历史 RS 可留旧/legacy/额外引用 template。
    $null = Assert-PandoraDSTicketSignerDeploymentGate $deployments @($replicaSets + $zeroOldRS) $signerPods 3 3
    $nonzeroOldRS = New-TestSignerReplicaSet login 1 1 pandora-config 1
    $nonzeroOldRS.metadata.name = 'login-old-live-rs'; $nonzeroOldRS.metadata.uid = 'uid-login-old-live-rs'
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate $deployments @($replicaSets + $nonzeroOldRS) $signerPods 3 3
    } '非零旧 signer RS 阻断异步补出' '非零但未引用目标'
    $deletingRS = Copy-TestObject $zeroOldRS
    $deletingRS.metadata | Add-Member -NotePropertyName deletionTimestamp -NotePropertyValue '2026-07-13T12:00:00Z'
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate $deployments @($replicaSets + $deletingRS) $signerPods 3 3
    } 'terminating 零容量 RS 仍阻断' '正在终止'
    $deletingDeployment = Copy-TestObject $deployments
    $deletingDeployment[0].metadata | Add-Member -NotePropertyName deletionTimestamp -NotePropertyValue '2026-07-13T12:00:00Z'
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate $deletingDeployment $replicaSets $signerPods 3 3
    } 'deleting signer Deployment 阻断' '正在删除'
    $rogueDeployment = New-TestSignerDeployment 'rogue-signer' 3 3
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate @($deployments + $rogueDeployment) $replicaSets $signerPods 3 3
    } '未知 signer parent Deployment 即使尚无 RS/Pod 也阻断' '未知 DSTicket signer parent'
    $mislabeledSignerPod = New-TestSignerPod login 3 3
    $mislabeledSignerPod.metadata.name = 'rogue-private-pod'; $mislabeledSignerPod.metadata.labels.app = 'rogue'
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate $deployments $replicaSets @($signerPods + $mislabeledSignerPod) 3 3
    } '错标/孤儿 signer Pod 阻断' '不是受管 signer'
    $ephemeralPrivateRef = Copy-TestObject $signerPods
    $ephemeralPrivateRef[0].spec | Add-Member -NotePropertyName ephemeralContainers -NotePropertyValue @([pscustomobject]@{
        name = 'debugger'
        envFrom = @([pscustomobject]@{ secretRef = [pscustomobject]@{ name = 'pandora-dsticket-signer-r3' } })
    })
    Assert-Throws {
        Assert-PandoraDSTicketSignerDeploymentGate $deployments $replicaSets $ephemeralPrivateRef 3 3
    } 'ephemeral container 额外 signer Secret 引用阻断' '必须且只能'

    # TTL 边界固定 180 + 15 + 30 = 225 秒；224 拒绝，225 放行。
    $activatedAt = [DateTimeOffset]::Parse('2026-07-13T12:00:00Z')
    $marker = New-PandoraDSTicketActivationMarkerObject -StageRevision 2 -PromoteRevision 3 `
        -RetireRevision 4 -OldSignerRevision 1 -OldKid $stage.OldKid -NewKid $stage.NewKid
    $markerObject = [pscustomobject]($marker | ConvertTo-Json -Depth 20 | ConvertFrom-Json)
    $markerObject.metadata | Add-Member -NotePropertyName creationTimestamp -NotePropertyValue $activatedAt.UtcDateTime
    Assert-Throws {
        Assert-PandoraDSTicketActivationMarkerContract -MarkerObject $markerObject -StageRevision 2 `
            -PromoteRevision 3 -RetireRevision 4 -OldSignerRevision 1 -OldKid $stage.OldKid `
            -NewKid $stage.NewKid -ServerNow $activatedAt.AddSeconds(224) -RequireRetireWindow
    } '224 秒拒绝 retire' '还需等待 1s'
    $ttl = Assert-PandoraDSTicketActivationMarkerContract -MarkerObject $markerObject -StageRevision 2 `
        -PromoteRevision 3 -RetireRevision 4 -OldSignerRevision 1 -OldKid $stage.OldKid `
        -NewKid $stage.NewKid -ServerNow $activatedAt.AddSeconds(225) -RequireRetireWindow
    Assert-True ($ttl.WaitSeconds -eq 225) '225 秒边界放行'
    $driftMarker = Copy-TestObject $markerObject
    $driftMarker.metadata.annotations | Add-Member `
        -NotePropertyName 'pandora.dev/dsticket-retire-not-before-unix' `
        -NotePropertyValue ([string]($ttl.RetireNotBeforeUnix - 1))
    Assert-Throws {
        Assert-PandoraDSTicketActivationMarkerContract -MarkerObject $driftMarker -StageRevision 2 `
            -PromoteRevision 3 -RetireRevision 4 -OldSignerRevision 1 -OldKid $stage.OldKid `
            -NewKid $stage.NewKid -ServerNow $activatedAt.AddSeconds(225) -RequireRetireWindow
    } 'marker 禁止客户端伪造退役时间' '禁止携带客户端/预采样时间'

    $badMarkerLabel = Copy-TestObject $markerObject
    $badMarkerLabel.metadata.labels.'app.kubernetes.io/component' = 'forged'
    Assert-Throws {
        Assert-PandoraDSTicketActivationMarkerContract $badMarkerLabel 2 3 4 1 $stage.OldKid $stage.NewKid
    } 'activation marker label 漂移阻断' 'labels 漂移'
    $deletingMarker = Copy-TestObject $markerObject
    $deletingMarker.metadata | Add-Member -NotePropertyName deletionTimestamp -NotePropertyValue '2026-07-13T12:00:01Z'
    Assert-Throws {
        Assert-PandoraDSTicketActivationMarkerContract $deletingMarker 2 3 4 1 $stage.OldKid $stage.NewKid
    } 'terminating activation marker 阻断' '未删除'
    $fixedContractHash = ('a' * 64) -join ''
    $terminal = New-PandoraDSTicketTerminalMarkerObject -PromoteRevision 3 -RetireRevision 4 `
        -ActiveKid $stage.NewKid -FixedConfigContractSha256 $fixedContractHash
    $terminalObject = [pscustomobject]($terminal | ConvertTo-Json -Depth 20 | ConvertFrom-Json)
    $terminalObject.metadata | Add-Member -NotePropertyName creationTimestamp -NotePropertyValue $activatedAt.AddSeconds(225).UtcDateTime
    $terminalContract = Assert-PandoraDSTicketTerminalMarkerContract -MarkerObject $terminalObject `
        -PromoteRevision 3 -RetireRevision 4 -ActiveKid $stage.NewKid `
        -FixedConfigContractSha256 $fixedContractHash -ActivationMarkerObject $markerObject
    Assert-True ($terminalContract.CreatedAtUnix -eq $ttl.RetireNotBeforeUnix) 'terminal 只以真实 creationTimestamp 证明 225s'
    $earlyTerminal = Copy-TestObject $terminalObject
    $earlyTerminal.metadata.creationTimestamp = $activatedAt.AddSeconds(224).UtcDateTime
    Assert-Throws {
        Assert-PandoraDSTicketTerminalMarkerContract $earlyTerminal 3 4 $stage.NewKid $fixedContractHash $markerObject
    } '提前 terminal marker 阻断' '创建过早'
    $sampledTerminal = Copy-TestObject $terminalObject
    $sampledTerminal.metadata.annotations | Add-Member -NotePropertyName 'pandora.dev/dsticket-completed-at-unix' -NotePropertyValue '1'
    Assert-Throws {
        Assert-PandoraDSTicketTerminalMarkerContract $sampledTerminal 3 4 $stage.NewKid $fixedContractHash $markerObject
    } 'terminal 禁止预采样完成时间' '禁止携带客户端/预采样时间'

    # retire fixed CAS 前后都必须保持同一历史链：old r1/K1 或 terminal r4/K2，除此之外一律拒绝。
    $transitionOldFixed = New-TestConfigSecret $stage.OldKid 1
    $oldTransition = Assert-PandoraDSTicketRotationTransitionHistoryState 2 3 4 1 `
        $stage.OldKid $stage.NewKid @($markerObject) @() $transitionOldFixed -AllowTerminalFixed
    Assert-True ($oldTransition.State -ceq 'retire-old-fixed-transition') 'fixed CAS 前 old 状态放行'
    $transitionTerminalFixed = New-TestConfigSecret $stage.NewKid 4
    $transitionTerminalContract = Get-PandoraDSTicketConfigSubcontract $transitionTerminalFixed
    $transitionTerminalFixed.metadata | Add-Member -NotePropertyName annotations -NotePropertyValue ([pscustomobject]@{
        'pandora.dev/dsticket-terminal-revision' = '4'
        'pandora.dev/dsticket-terminal-active-kid' = $stage.NewKid
        'pandora.dev/dsticket-terminal-config-contract-sha256' = $transitionTerminalContract.Sha256
    })
    foreach ($handoff in 1..4) {
        $afterCAS = Assert-PandoraDSTicketRotationTransitionHistoryState 2 3 4 1 `
            $stage.OldKid $stage.NewKid @($markerObject) @() $transitionTerminalFixed -AllowTerminalFixed
        Assert-True ($afterCAS.State -ceq 'retire-terminal-fixed-transition') "fixed CAS 后第 $handoff 个 signer handoff guard 可继续"
    }
    $wrongTransitionIdentity = Copy-TestObject $transitionTerminalFixed
    $wrongTransitionIdentity.metadata.namespace = 'default'
    Assert-Throws {
        Assert-PandoraDSTicketRotationTransitionHistoryState 2 3 4 1 $stage.OldKid $stage.NewKid `
            @($markerObject) @() $wrongTransitionIdentity -AllowTerminalFixed
    } 'transition historical fixed 必须保留 exact identity' '必须是 pandora/Secret/pandora-config'
    $wrongTransitionRevision = New-TestConfigSecret $stage.NewKid 3
    Assert-Throws {
        Assert-PandoraDSTicketRotationTransitionHistoryState 2 3 4 1 $stage.OldKid $stage.NewKid `
            @($markerObject) @() $wrongTransitionRevision -AllowTerminalFixed
    } 'transition 错 revision 拒绝' '只能是 old'
    $wrongTransitionKid = New-TestConfigSecret $stage.OldKid 4
    Assert-Throws {
        Assert-PandoraDSTicketRotationTransitionHistoryState 2 3 4 1 $stage.OldKid $stage.NewKid `
            @($markerObject) @() $wrongTransitionKid -AllowTerminalFixed
    } 'transition 错 kid 拒绝' '只能是 old'
    $wrongTransitionHash = Copy-TestObject $transitionTerminalFixed
    $wrongTransitionHash.metadata.annotations.'pandora.dev/dsticket-terminal-config-contract-sha256' = (('b' * 64) -join '')
    Assert-Throws {
        Assert-PandoraDSTicketRotationTransitionHistoryState 2 3 4 1 $stage.OldKid $stage.NewKid `
            @($markerObject) @() $wrongTransitionHash -AllowTerminalFixed
    } 'transition fixed annotation hash 漂移拒绝' 'annotations 与 DSTicket 子契约不一致'

    # 第二轮 current activation 必须晚于第一轮 latest terminal，不能因 transition helper 剔除 current 而漏检。
    $previousTerminalForChain = New-PandoraDSTicketTerminalMarkerObject 3 4 $stage.NewKid $transitionTerminalContract.Sha256
    $previousTerminalForChain = [pscustomobject]($previousTerminalForChain | ConvertTo-Json -Depth 20 | ConvertFrom-Json)
    $previousTerminalForChain.metadata | Add-Member -NotePropertyName creationTimestamp `
        -NotePropertyValue $activatedAt.AddSeconds(225).UtcDateTime
    $currentRoundMarker = New-PandoraDSTicketActivationMarkerObject 5 6 7 4 $stage.NewKid $stage.OldKid
    $currentRoundMarker = [pscustomobject]($currentRoundMarker | ConvertTo-Json -Depth 20 | ConvertFrom-Json)
    $currentRoundMarker.metadata | Add-Member -NotePropertyName creationTimestamp `
        -NotePropertyValue $activatedAt.AddSeconds(224).UtcDateTime
    Assert-Throws {
        Assert-PandoraDSTicketRotationTransitionHistoryState 5 6 7 4 $stage.NewKid $stage.OldKid `
            @($markerObject, $currentRoundMarker) @($previousTerminalForChain) $transitionTerminalFixed
    } '下一轮 activation 早于上一轮 terminal 必须阻断' '时间链倒序'
    $currentRoundMarker.metadata.creationTimestamp = $activatedAt.AddSeconds(226).UtcDateTime
    $nextRoundState = Assert-PandoraDSTicketRotationTransitionHistoryState 5 6 7 4 `
        $stage.NewKid $stage.OldKid @($markerObject, $currentRoundMarker) `
        @($previousTerminalForChain) $transitionTerminalFixed
    Assert-True ($nextRoundState.State -ceq 'retire-old-fixed-transition') '下一轮 activation 晚于上一轮 terminal 放行'

    Assert-Throws {
        Assert-PandoraDSTicketOrdinaryState -RequestedRevision 1 `
            -DeploymentObjects @([pscustomobject]@{}) -FleetObjects @([pscustomobject]@{}) `
            -SignerPodObjects @() -ReplicaSetObjects @() -GameServerObjects @() -GameServerSetObjects @() `
            -DSPodObjects @() -ActivationMarkers @() -TerminalMarkers @() -FixedConfigSecret $null `
            -LegacyControllerObjects @([pscustomobject]@{ kind = 'LegacyController' })
    } 'ordinary live 分支二次阻断 legacy controller' 'legacy controller'

    $lockHolder = [guid]::NewGuid().ToString('N')
    $lock = New-PandoraDSTicketOperationLockObject $lockHolder rotation-stage
    $lockObject = [pscustomobject]($lock | ConvertTo-Json -Depth 20 | ConvertFrom-Json)
    $lockObject.metadata | Add-Member -NotePropertyName uid -NotePropertyValue 'lock-uid'
    $lockObject.metadata | Add-Member -NotePropertyName resourceVersion -NotePropertyValue '777'
    $lockObject.metadata | Add-Member -NotePropertyName creationTimestamp -NotePropertyValue $activatedAt.UtcDateTime
    $null = Assert-PandoraDSTicketOperationLockContract $lockObject $lockHolder rotation-stage -RequireLiveIdentity
    $forgedLock = Copy-TestObject $lockObject
    $forgedLock.metadata.annotations.'pandora.dev/dsticket-lock-holder-id' = ([guid]::NewGuid().ToString('N'))
    Assert-Throws {
        Assert-PandoraDSTicketOperationLockContract $forgedLock $lockHolder rotation-stage -RequireLiveIdentity
    } '共享锁 holder 漂移阻断' '不一致'

    # Go 票据上限与脚本等待常量必须机械绑定，任何一侧改动都会让此测试变红。
    $dsticketGo = Get-Content -LiteralPath (Join-Path $ProjectRoot 'pkg/auth/dsticket.go') -Raw
    $maxTTLMatches = [regex]::Matches($dsticketGo, '(?m)^\s*DSTicketMaxTTL\s*=\s*3\s*\*\s*time\.Minute\s*$')
    Assert-True ($maxTTLMatches.Count -eq 1) 'Go DSTicketMaxTTL 必须唯一且等于 3*time.Minute(180s)'
    Assert-True ($dsticketGo.Contains('leeway(≤15s)') -and
        $script:PandoraDSTicketMaxTTLSeconds -eq 180 -and $script:PandoraDSTicketLeewaySeconds -eq 15 -and
        $script:PandoraDSTicketRetireBufferSeconds -eq 30 -and $script:PandoraDSTicketRetireWaitSeconds -eq 225) `
        'Go 180s + DS leeway≤15s + buffer30s 必须严格等于 marker 225s'

    # 执行脚本只做静态/AST 审核，不连接集群：双确认必须早于 kubectl，且不存在 apply/delete/namespace 自建。
    $rotatePath = Join-Path $ProjectRoot 'tools/scripts/dsticket_rotate.ps1'
    $rotateText = Get-Content -LiteralPath $rotatePath -Raw
    $contractText = Get-Content -LiteralPath (Join-Path $ProjectRoot 'tools/scripts/lib/dsticket_rotation_contract.ps1') -Raw
    $tokens = $null
    $parseErrors = $null
    $rotateAst = [Management.Automation.Language.Parser]::ParseFile($rotatePath, [ref]$tokens, [ref]$parseErrors)
    Assert-True ($parseErrors.Count -eq 0) 'dsticket_rotate.ps1 AST 无语法错误'
    $commands = @($rotateAst.FindAll({ param($node) $node -is [Management.Automation.Language.CommandAst] }, $true))
    $readHosts = @($commands | Where-Object { $_.GetCommandName() -ceq 'Read-Host' })
    Assert-True ($readHosts.Count -eq 2) '执行脚本恰好两次显式人工确认'
    $nativeKubectl = @($commands | Where-Object { $_.GetCommandName() -ceq 'kubectl' })
    Assert-True ($nativeKubectl.Count -eq 2) 'kubectl 只封装在统一读/写入口的有/无 stdin 两支'
    Assert-True (($readHosts | Measure-Object { $_.Extent.StartLineNumber } -Maximum).Maximum -lt
        ($nativeKubectl | Measure-Object { $_.Extent.StartLineNumber } -Minimum).Minimum) '两次确认位于任何 kubectl 访问之前'
    Assert-True ($rotateText.Contains("[ValidateSet('stage', 'promote', 'retire')]") -and
        -not $rotateText.Contains("ValidateSet('bootstrap'")) '只开放 stage/promote/retire'
    Assert-True (-not [regex]::IsMatch($rotateText, "(?i)@\(\s*'apply'|rollout\s+restart|create\s+namespace")) `
        '轮换脚本不 apply/restart Fleet，不隐式创建 namespace'
    $deleteCalls = [regex]::Matches($rotateText, "(?i)@\(\s*'delete'")
    Assert-True ($deleteCalls.Count -eq 1 -and
        $rotateText.Contains('kind = ''DeleteOptions''') -and
        $rotateText.Contains('preconditions = [ordered]@{ uid = $uid; resourceVersion = $resourceVersion }') -and
        $rotateText.Contains('"--raw=$uri"')) `
        '唯一 DELETE 必须是 UID+resourceVersion CAS 释放操作锁，禁止删密钥/Fleet/GameServer/审计 marker'
    Assert-True ($rotateText.Contains('$material = Get-PandoraRotationLiveMaterialContract') -and
        $rotateText.Contains('Invoke-PandoraGuardedWrite')) '每笔写由材料回读 guard 包裹'
    $newActivationBody = [regex]::Match($rotateText, '(?ms)^function New-PandoraActivationMarker \{(?<body>.*?)^\}')
    $newTerminalBody = [regex]::Match($rotateText, '(?ms)^function New-PandoraTerminalMarker \{(?<body>.*?)^\}')
    Assert-True ($newActivationBody.Success -and $newTerminalBody.Success -and
        -not $newActivationBody.Groups['body'].Value.Contains('Get-PandoraApiServerNow') -and
        -not $newTerminalBody.Groups['body'].Value.Contains('Get-PandoraApiServerNow') -and
        -not $contractText.Contains('[Parameter(Mandatory = $true)][DateTimeOffset]$ActivatedAt') -and
        -not $contractText.Contains('[Parameter(Mandatory = $true)][DateTimeOffset]$CompletedAt')) `
        'guarded-write 延迟不得写坏 immutable marker；激活/完成只取真实 creationTimestamp'
    $activationBody = $newActivationBody.Groups['body'].Value
    $activationGuard = $activationBody.IndexOf('Invoke-PandoraGuardedWrite', [StringComparison]::Ordinal)
    $activationExact = $activationBody.IndexOf('Assert-PandoraActivationRuntimeState $Material', $activationGuard, [StringComparison]::Ordinal)
    $activationLock = $activationBody.IndexOf('Assert-PandoraDSTicketOperationLockHeld', $activationExact, [StringComparison]::Ordinal)
    $activationCreate = $activationBody.IndexOf('Invoke-PandoraKubectl', $activationLock, [StringComparison]::Ordinal)
    $activationPost = $activationBody.IndexOf('Assert-PandoraActivationRuntimeState $Material -RequireMarker', $activationCreate, [StringComparison]::Ordinal)
    Assert-True ($activationGuard -ge 0 -and $activationExact -gt $activationGuard -and
        $activationLock -gt $activationExact -and $activationCreate -gt $activationLock -and $activationPost -gt $activationCreate) `
        'activation marker 必须 exact promoted 全态→紧邻锁→create→exact postcheck'
    $terminalBodyText = $newTerminalBody.Groups['body'].Value
    $terminalGuard = $terminalBodyText.IndexOf('Invoke-PandoraGuardedWrite', [StringComparison]::Ordinal)
    $terminalExact = $terminalBodyText.IndexOf('Assert-PandoraTerminalMarkerRuntimeState $Material $FixedContractSha256', $terminalGuard, [StringComparison]::Ordinal)
    $terminalLock = $terminalBodyText.IndexOf('Assert-PandoraDSTicketOperationLockHeld', $terminalExact, [StringComparison]::Ordinal)
    $terminalCreate = $terminalBodyText.IndexOf('Invoke-PandoraKubectl', $terminalLock, [StringComparison]::Ordinal)
    $terminalPost = $terminalBodyText.IndexOf('Assert-PandoraTerminalMarkerRuntimeState $Material $FixedContractSha256 -RequireMarker', $terminalCreate, [StringComparison]::Ordinal)
    Assert-True ($terminalGuard -ge 0 -and $terminalExact -gt $terminalGuard -and
        $terminalLock -gt $terminalExact -and $terminalCreate -gt $terminalLock -and $terminalPost -gt $terminalCreate) `
        'terminal marker 必须 exact terminal 全态→紧邻锁→create→exact postcheck'
    $ensureFunction = [regex]::Match($rotateText, '(?ms)^function Ensure-PandoraRevisionedConfigSecret \{(?<body>.*?)^\}')
    $configReferenceFunction = [regex]::Match($rotateText, '(?ms)^function Assert-PandoraConfigReferenceContract \{(?<body>.*?)^\}')
    Assert-True ($ensureFunction.Success -and
        $ensureFunction.Groups['body'].Value.Contains('$guardedSource = Get-PandoraKubeObject ''secret/pandora-config'' pandora') -and
        $ensureFunction.Groups['body'].Value.Contains('$validationSource = Get-PandoraKubeObject ''secret/pandora-config'' pandora') -and
        $configReferenceFunction.Success -and
        $configReferenceFunction.Groups['body'].Value.Contains('New-PandoraDSTicketRevisionedConfigSecretObject') -and
        $configReferenceFunction.Groups['body'].Value.Contains('$expectedHash')) `
        'revisioned config existing/create/readback/每次 phase gate 均须从当前 fixed 重投影 full-data hash'
    $ensureBody = $ensureFunction.Groups['body'].Value
    $ensureSource = $ensureBody.IndexOf('$guardedSource = Get-PandoraKubeObject ''secret/pandora-config'' pandora', [StringComparison]::Ordinal)
    $ensureTargetAbsence = $ensureBody.IndexOf('$concurrent = Get-PandoraKubeObject', $ensureSource, [StringComparison]::Ordinal)
    $ensureFinalLock = $ensureBody.IndexOf('Assert-PandoraDSTicketOperationLockHeld', $ensureTargetAbsence, [StringComparison]::Ordinal)
    $ensureCreate = $ensureBody.IndexOf('Invoke-PandoraKubectl', $ensureFinalLock, [StringComparison]::Ordinal)
    Assert-True ($ensureSource -ge 0 -and $ensureTargetAbsence -gt $ensureSource -and
        $ensureFinalLock -gt $ensureTargetAbsence -and $ensureCreate -gt $ensureFinalLock) `
        'revisioned config create 必须 guarded source→target absence→紧邻锁→create'
    $noFixedFunction = [regex]::Match($rotateText, '(?ms)^function Assert-PandoraNoSignerUsesFixedConfig \{(?<body>.*?)^\}')
    $fixedTerminalFunction = [regex]::Match($rotateText, '(?ms)^function Set-PandoraFixedConfigTerminalState \{(?<body>.*?)^\}')
    Assert-True ($noFixedFunction.Success -and
        $noFixedFunction.Groups['body'].Value.Contains('Test-PandoraDSTicketDSPodSpecClue') -and
        $noFixedFunction.Groups['body'].Value.Contains('Get-PandoraDSTicketSignerConfigSecretReferences')) `
        'fixed handoff 的 Pod 重扫必须覆盖 broader DSTicket clue 与 direct/projected/env/envFrom config 引用'
    $fixedTerminalBody = $fixedTerminalFunction.Groups['body'].Value
    $fixedGuard = $fixedTerminalBody.IndexOf('Invoke-PandoraGuardedWrite', [StringComparison]::Ordinal)
    $fixedPrecheck = $fixedTerminalBody.IndexOf('Assert-PandoraNoSignerUsesFixedConfig', $fixedGuard, [StringComparison]::Ordinal)
    $fixedFinalLock = $fixedTerminalBody.IndexOf('Assert-PandoraDSTicketOperationLockHeld', $fixedPrecheck, [StringComparison]::Ordinal)
    $fixedReplace = $fixedTerminalBody.IndexOf('Invoke-PandoraKubectl', $fixedFinalLock, [StringComparison]::Ordinal)
    $fixedPostcheck = $fixedTerminalBody.IndexOf('Assert-PandoraNoSignerUsesFixedConfig', $fixedReplace, [StringComparison]::Ordinal)
    Assert-True ($fixedTerminalFunction.Success -and $fixedGuard -ge 0 -and
        $fixedPrecheck -gt $fixedGuard -and $fixedFinalLock -gt $fixedPrecheck -and
        $fixedReplace -gt $fixedFinalLock -and $fixedPostcheck -gt $fixedReplace) `
        'fixed handoff 必须 guarded write→全引用 NoSigner→紧邻锁→replace→全引用 postcheck'
    Assert-True ($rotateText.Contains('Get-PandoraKubeListItems deployments pandora') -and
        $rotateText.Contains('Get-PandoraKubeListItems fleets.agones.dev default') -and
        $rotateText.Contains('未知/漂移的 DSTicket 审计 marker')) `
        'rotation 必须全量扫描 parent controller 与未知审计 marker'
    $signerSnapshotFunction = [regex]::Match($rotateText, '(?ms)^function Get-PandoraSignerSnapshot \{(?<body>.*?)^\}')
    $signerGateFunction = [regex]::Match($contractText, '(?ms)^function Assert-PandoraDSTicketSignerDeploymentGate \{(?<body>.*?)^\}')
    Assert-True ($signerSnapshotFunction.Success -and
        [regex]::Matches($signerSnapshotFunction.Groups['body'].Value, 'Test-PandoraDSTicketDSPodSpecClue').Count -eq 2 -and
        $signerGateFunction.Success -and
        $signerGateFunction.Groups['body'].Value.Contains('Test-PandoraDSTicketDSPodSpecClue')) `
        'signer Deployment/RS snapshot 与 Pod gate 必须统一纳入 broader DSTicket clue'
    $promoteFunction = [regex]::Match($rotateText, '(?ms)^function Invoke-PandoraPromotePhase \{(?<body>.*?)^\}')
    Assert-True ($promoteFunction.Success -and
        $promoteFunction.Groups['body'].Value.IndexOf('Assert-PandoraPromotePreflight', [StringComparison]::Ordinal) -lt
        $promoteFunction.Groups['body'].Value.IndexOf('Set-PandoraDeploymentBundleCAS', [StringComparison]::Ordinal)) `
        'promote 的全量 live gate 早于 signer 写入'
    $promoteBody = $promoteFunction.Groups['body'].Value
    Assert-True ($promoteBody.IndexOf('Set-PandoraDeploymentBundleCAS', [StringComparison]::Ordinal) -lt
        $promoteBody.IndexOf('Wait-PandoraDeploymentRollout $name', [StringComparison]::Ordinal) -and
        $promoteBody.IndexOf('Wait-PandoraDeploymentRollout $name', [StringComparison]::Ordinal) -lt
        $promoteBody.IndexOf('Wait-PandoraSignerControllerQuiescence', [StringComparison]::Ordinal)) `
        'promote 每个 signer patch 后必须 rollout+RS 静默再推进下一个'
    $retireFunction = [regex]::Match($rotateText, '(?ms)^function Invoke-PandoraRetirePhase \{(?<body>.*?)^\}')
    $retireBody = $retireFunction.Groups['body'].Value
    Assert-True ($retireFunction.Success -and
        $retireBody.IndexOf('Wait-PandoraLiveDSRevision $RetireRevision', [StringComparison]::Ordinal) -lt
        $retireBody.IndexOf('Set-PandoraDeploymentBundleCAS', [StringComparison]::Ordinal)) `
        'retire 先等全量 K2-only DS，再清 Login 的 K1 verifier'
    Assert-True ([regex]::Matches($retireBody, 'Set-PandoraDeploymentBundleCAS').Count -eq 2 -and
        [regex]::Matches($retireBody, 'Wait-PandoraDeploymentRollout \$name').Count -eq 2 -and
        [regex]::Matches($retireBody, 'Wait-PandoraSignerControllerQuiescence').Count -eq 2) `
        'retire phase bundle 与 fixed handoff 均逐 signer 等 RS 静默'
}
finally {
    if (Test-Path -LiteralPath $tmpRoot -PathType Container) {
        $resolved = [IO.Path]::GetFullPath($tmpRoot)
        $safeParent = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
        if (-not $resolved.StartsWith($safeParent, [StringComparison]::OrdinalIgnoreCase) -or
            (Split-Path -Leaf $resolved) -notmatch '^pandora-dsticket-rotation-[0-9a-f]{32}$') {
            throw "拒绝清理未验证测试目录:$resolved"
        }
        Remove-Item -LiteralPath $resolved -Recurse -Force
    }
}

Write-Host 'dsticket_rotation_contract_test: PASS' -ForegroundColor Green
