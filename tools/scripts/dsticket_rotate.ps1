<#
.SYNOPSIS
  DSTicket K1 -> K2 不停服轮换；独立安全操作，普通发布不得隐式触发。

.DESCRIPTION
  stage   : r2=K1+K2/K1 active。K2 signer-r2 只校验不挂载；Login 与四 Fleet 先接 overlap verifier。
  promote : r3=K1+K2/K2 active。四 signer 原子切 revisioned config-r3 + signer-r3，全部旧 K1 Pod
            真正消失后写 immutable 激活 marker。
  retire  : marker 满 180+15+30=225 秒后，四 Fleet/全部 live DS 到 r4=K2-only；四 signer 原子切
            config-r4 + signer-r4。最后安全回填 fixed pandora-config 并把 config volume 切回 fixed，
            使后续普通发布仍满足 config/signer/Login/Fleet 全部 r4。

  每个阶段的 config Secret 都从 pandora-config 完整复制 data，严格只改四个 signer 的 ds_ticket 字段，
  create-only + immutable + hash 对账。Deployment 以单次 JSON Patch 原子切 config/signer/Login JWKS，
  并用 metadata.resourceVersion 与旧引用 test 做 CAS；不依赖 pause，也不原地改正在使用的配置。
  本脚本不 delete/强杀 GameServer，不删除旧密钥、配置或审计对象；唯一 DELETE 是以 UID+resourceVersion
  preconditions CAS 释放共享 operation-lock ConfigMap。
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][ValidateSet('stage', 'promote', 'retire')][string]$Phase,
    [Parameter(Mandatory = $true)][ValidateNotNullOrEmpty()][string]$KubeContext,
    [Parameter(Mandatory = $true)][ValidateRange(1, 2147483647)][int]$StageRevision,
    [Parameter(Mandatory = $true)][ValidateRange(1, 2147483647)][int]$PromoteRevision,
    [Parameter(Mandatory = $true)][ValidateRange(1, 2147483647)][int]$RetireRevision,
    [Parameter(Mandatory = $true)][ValidateRange(1, 2147483647)][int]$OldSignerRevision,
    [ValidateRange(60, 86400)][int]$TimeoutSec = 7200,
    [ValidateRange(2, 30)][int]$PollIntervalSec = 5
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$KubeContext = $KubeContext.Trim()
if ([string]::IsNullOrWhiteSpace($KubeContext)) { throw '-KubeContext 不能为空；禁止依赖 current-context。' }
$ProjectRoot = (Resolve-Path "$PSScriptRoot/../..").Path
. (Join-Path $PSScriptRoot 'lib/dsticket_rotation_contract.ps1')

Assert-PandoraDSTicketRotationRevisionPlan -StageRevision $StageRevision -PromoteRevision $PromoteRevision `
    -RetireRevision $RetireRevision -OldSignerRevision $OldSignerRevision

# 两次确认都早于任何 kubectl 访问。
$contextConfirmation = Read-Host "DSTicket 轮换将访问 kube-context '$KubeContext'。第一次确认：请输入完整 context 名"
if ($contextConfirmation -cne $KubeContext) { throw '第一次 context 确认不匹配；未访问集群。' }
$confirmationPhrase = "DSTICKET $($Phase.ToUpperInvariant()) $KubeContext r$StageRevision-r$PromoteRevision-r$RetireRevision"
$phaseConfirmation = Read-Host "第二次确认：请输入 '$confirmationPhrase'"
if ($phaseConfirmation -cne $confirmationPhrase) { throw '第二次阶段确认不匹配；未访问集群。' }

function Invoke-PandoraKubectl {
    param(
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [Parameter(Mandatory = $true)][string]$Action,
        [string]$InputJson = ''
    )
    if ([string]::IsNullOrEmpty($InputJson)) { $lines = @(& kubectl --context $KubeContext @Arguments 2>&1) }
    else { $lines = @($InputJson | & kubectl --context $KubeContext @Arguments 2>&1) }
    if ($LASTEXITCODE -ne 0) {
        $message = (($lines | ForEach-Object { $_.ToString() }) -join [Environment]::NewLine)
        throw "$Action 失败(exit=$LASTEXITCODE):$message"
    }
    return (($lines | ForEach-Object { $_.ToString() }) -join "`n").Trim()
}

function Get-PandoraKubeObject {
    param([string]$Resource, [string]$Namespace, [switch]$IgnoreNotFound)
    $args = @('get', $Resource, '-n', $Namespace, '-o', 'json')
    if ($IgnoreNotFound) { $args += '--ignore-not-found' }
    $text = Invoke-PandoraKubectl -Arguments $args -Action "读取 $Namespace/$Resource"
    if ([string]::IsNullOrWhiteSpace($text)) { return $null }
    try { return ($text | ConvertFrom-Json -ErrorAction Stop) }
    catch { throw "读取 $Namespace/$Resource 返回非法 JSON:$($_.Exception.Message)" }
}

function Get-PandoraKubeListItems {
    param([string]$Resource, [string]$Namespace)
    $text = Invoke-PandoraKubectl -Arguments @('get', $Resource, '-n', $Namespace, '-o', 'json') `
        -Action "读取 $Namespace/$Resource 列表"
    try { $list = $text | ConvertFrom-Json -ErrorAction Stop }
    catch { throw "读取 $Namespace/$Resource 列表返回非法 JSON:$($_.Exception.Message)" }
    return @($list.items)
}

function Get-PandoraRevisionConfigMaps([int]$Revision) {
    $name = "configmap/pandora-dsticket-jwks-r$Revision"
    return [pscustomobject]@{
        Default = Get-PandoraKubeObject $name default
        Pandora = Get-PandoraKubeObject $name pandora
    }
}

function Get-PandoraRotationLiveMaterialContract {
    $old = Get-PandoraKubeObject "secret/pandora-dsticket-signer-r$OldSignerRevision" pandora
    $stageSigner = Get-PandoraKubeObject "secret/pandora-dsticket-signer-r$StageRevision" pandora
    $stageMaps = Get-PandoraRevisionConfigMaps $StageRevision
    if ($Phase -ceq 'stage') {
        return Assert-PandoraDSTicketStageMaterialContract -OldSignerSecret $old -StageSignerSecret $stageSigner `
            -DefaultConfigMap $stageMaps.Default -PandoraConfigMap $stageMaps.Pandora `
            -StageRevision $StageRevision -OldSignerRevision $OldSignerRevision
    }
    $promoteSigner = Get-PandoraKubeObject "secret/pandora-dsticket-signer-r$PromoteRevision" pandora
    $promoteMaps = Get-PandoraRevisionConfigMaps $PromoteRevision
    if ($Phase -ceq 'promote') {
        return Assert-PandoraDSTicketPromoteMaterialContract -OldSignerSecret $old `
            -StageSignerSecret $stageSigner -PromoteSignerSecret $promoteSigner `
            -StageDefaultConfigMap $stageMaps.Default -StagePandoraConfigMap $stageMaps.Pandora `
            -PromoteDefaultConfigMap $promoteMaps.Default -PromotePandoraConfigMap $promoteMaps.Pandora `
            -StageRevision $StageRevision -PromoteRevision $PromoteRevision -OldSignerRevision $OldSignerRevision
    }
    $retireSigner = Get-PandoraKubeObject "secret/pandora-dsticket-signer-r$RetireRevision" pandora
    $retireMaps = Get-PandoraRevisionConfigMaps $RetireRevision
    return Assert-PandoraDSTicketRetireMaterialContract -OldSignerSecret $old `
        -StageSignerSecret $stageSigner -PromoteSignerSecret $promoteSigner -RetireSignerSecret $retireSigner `
        -StageDefaultConfigMap $stageMaps.Default -StagePandoraConfigMap $stageMaps.Pandora `
        -PromoteDefaultConfigMap $promoteMaps.Default -PromotePandoraConfigMap $promoteMaps.Pandora `
        -RetireDefaultConfigMap $retireMaps.Default -RetirePandoraConfigMap $retireMaps.Pandora `
        -StageRevision $StageRevision -PromoteRevision $PromoteRevision -RetireRevision $RetireRevision `
        -OldSignerRevision $OldSignerRevision
}

function Get-PandoraRotationMarker {
    $name = Get-PandoraDSTicketActivationMarkerName -PromoteSignerRevision $PromoteRevision
    return Get-PandoraKubeObject "configmap/$name" pandora -IgnoreNotFound
}

function Get-PandoraRotationAuditMarkerSnapshot {
    $items = @(Get-PandoraKubeListItems configmaps pandora)
    $activations = [System.Collections.Generic.List[object]]::new()
    $terminals = [System.Collections.Generic.List[object]]::new()
    foreach ($item in $items) {
        $metadata = Get-PandoraRotationProperty $item 'metadata'
        $name = [string](Get-PandoraRotationProperty $metadata 'name')
        $component = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $metadata 'labels') 'app.kubernetes.io/component')
        $looksLikeMarker = $name -cmatch '^pandora-dsticket-(?:activation-signer-r|retired-r)' -or
            $component -ceq 'dsticket-rotation-audit'
        if (-not $looksLikeMarker) { continue }
        if (-not [string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty $metadata 'deletionTimestamp'))) {
            throw "DSTicket marker ConfigMap/$name 正在终止，禁止轮换。"
        }
        if ($name -cmatch '^pandora-dsticket-activation-signer-r[1-9][0-9]*$') { $activations.Add($item) }
        elseif ($name -cmatch '^pandora-dsticket-retired-r[1-9][0-9]*$') { $terminals.Add($item) }
        else { throw "未知/漂移的 DSTicket 审计 marker ConfigMap/$name，禁止轮换。" }
    }
    return [pscustomobject]@{
        Activations = [object[]]$activations.ToArray()
        Terminals = [object[]]$terminals.ToArray()
    }
}

function Assert-PandoraPreviousRotationHistoryStable {
    param([Parameter(Mandatory = $true)]$Material, [switch]$IncludeCurrentActivation)
    $audit = Get-PandoraRotationAuditMarkerSnapshot
    $activationName = Get-PandoraDSTicketActivationMarkerName $PromoteRevision
    $terminalName = Get-PandoraDSTicketTerminalMarkerName $RetireRevision
    $currentActivations = @($audit.Activations | Where-Object { [string]$_.metadata.name -ceq $activationName })
    $currentTerminals = @($audit.Terminals | Where-Object { [string]$_.metadata.name -ceq $terminalName })
    if ($IncludeCurrentActivation) {
        $fixed = Get-PandoraKubeObject 'secret/pandora-config' pandora
        $transition = Assert-PandoraDSTicketRotationTransitionHistoryState `
            -StageRevision $StageRevision -PromoteRevision $PromoteRevision -RetireRevision $RetireRevision `
            -OldSignerRevision $OldSignerRevision -OldKid $Material.OldKid -NewKid $Material.NewKid `
            -ActivationMarkers $audit.Activations -TerminalMarkers $audit.Terminals -FixedConfigSecret $fixed `
            -AllowTerminalFixed:($Phase -ceq 'retire')
        return [pscustomobject]@{ Audit = $audit; CurrentActivation = $transition.CurrentActivation; FixedState = $transition.State }
    } elseif ($currentActivations.Count -ne 0 -or $currentTerminals.Count -ne 0) {
        throw "当前计划已有 activation/terminal marker，禁止从 stage/promote 写路径重入。"
    }
    $previousActivations = @($audit.Activations | Where-Object { [string]$_.metadata.name -cne $activationName })
    $previousTerminals = @($audit.Terminals | Where-Object { [string]$_.metadata.name -cne $terminalName })
    $fixed = Get-PandoraKubeObject 'secret/pandora-config' pandora
    $state = Assert-PandoraDSTicketOrdinaryMarkerState -RequestedRevision $OldSignerRevision `
        -ActivationMarkers $previousActivations -TerminalMarkers $previousTerminals -FixedConfigSecret $fixed
    if ($state.ActiveKid -cne $Material.OldKid) {
        throw "上一轮 terminal/fixed active kid 与当前 old signer kid 不一致:$($state.ActiveKid)/$($Material.OldKid)。"
    }
    return [pscustomobject]@{ Audit = $audit; CurrentActivation = $null }
}

function Get-PandoraSignerSnapshot {
    $deployments = @(Get-PandoraKubeListItems deployments pandora | Where-Object {
        $deployment = $_
        $name = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $deployment 'metadata') 'name')
        $podSpec = Get-PandoraRotationProperty (Get-PandoraRotationProperty `
            (Get-PandoraRotationProperty $deployment 'spec') 'template') 'spec'
        $hasDSTicketClue = Test-PandoraDSTicketDSPodSpecClue $podSpec
        return $script:PandoraDSTicketSignerNames -ccontains $name -or $hasDSTicketClue
    })
    $replicaSets = @(Get-PandoraKubeListItems replicasets pandora | Where-Object {
        $rs = $_
        $metadata = Get-PandoraRotationProperty $rs 'metadata'
        $app = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $metadata 'labels') 'app')
        $owners = @(@(Get-PandoraRotationProperty $metadata 'ownerReferences' @()) |
            Where-Object { [string](Get-PandoraRotationProperty $_ 'kind') -ceq 'Deployment' } |
            ForEach-Object { [string](Get-PandoraRotationProperty $_ 'name') })
        $podSpec = Get-PandoraRotationProperty (Get-PandoraRotationProperty `
            (Get-PandoraRotationProperty $rs 'spec') 'template') 'spec'
        $hasDSTicketClue = Test-PandoraDSTicketDSPodSpecClue $podSpec
        return $script:PandoraDSTicketSignerNames -ccontains $app -or
            @($owners | Where-Object { $script:PandoraDSTicketSignerNames -ccontains $_ }).Count -gt 0 -or
            $hasDSTicketClue
    })
    return [pscustomobject]@{
        Deployments = $deployments
        ReplicaSets = $replicaSets
        Pods = @(Get-PandoraKubeListItems pods pandora)
    }
}

function Get-PandoraLiveDSGateSnapshot {
    $fleets = @(Get-PandoraKubeListItems fleets.agones.dev default | Where-Object {
        $fleet = $_
        $name = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $fleet 'metadata') 'name')
        $podSpec = Get-PandoraRotationProperty (Get-PandoraRotationProperty (Get-PandoraRotationProperty `
            (Get-PandoraRotationProperty (Get-PandoraRotationProperty $fleet 'spec') 'template') 'spec') 'template') 'spec'
        $containerNames = @(@(Get-PandoraRotationProperty $podSpec 'containers' @()) |
            ForEach-Object { [string](Get-PandoraRotationProperty $_ 'name') })
        $jwksVolumes = @(@(Get-PandoraRotationProperty $podSpec 'volumes' @()) | Where-Object {
            [string](Get-PandoraRotationProperty $_ 'name') -ceq 'dsticket-jwks' -or
            [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $_ 'configMap') 'name') `
                -cmatch '^pandora-dsticket-jwks-r[1-9][0-9]*$'
        })
        $hasVerifierClue = Test-PandoraDSTicketDSPodSpecClue $podSpec
        return $script:PandoraDSTicketFleetNames -ccontains $name -or
            $name -cmatch '^pandora-(?:battle|hub)(?:-|$)' -or
            @($containerNames | Where-Object { $_ -cin @('pandora-battle-ds', 'pandora-hub-ds') }).Count -gt 0 -or
            $jwksVolumes.Count -gt 0 -or $hasVerifierClue
    })
    $gameServerSets = @(Get-PandoraKubeListItems gameserversets default | Where-Object {
        $gss = $_
        $metadata = Get-PandoraRotationProperty $gss 'metadata'
        $fleetLabel = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $metadata 'labels') 'agones.dev/fleet')
        $ownerFleets = @(@(Get-PandoraRotationProperty $metadata 'ownerReferences' @()) |
            Where-Object { [string](Get-PandoraRotationProperty $_ 'kind') -ceq 'Fleet' } |
            ForEach-Object { [string](Get-PandoraRotationProperty $_ 'name') })
        $name = [string](Get-PandoraRotationProperty $metadata 'name')
        $podSpec = Get-PandoraGameServerSetPodSpec $gss "GameServerSet/$name"
        $containerNames = @(@(Get-PandoraRotationProperty $podSpec 'containers' @()) |
            ForEach-Object { [string](Get-PandoraRotationProperty $_ 'name') })
        $hasVerifierClue = Test-PandoraDSTicketDSPodSpecClue $podSpec
        return $script:PandoraDSTicketFleetNames -ccontains $fleetLabel -or
            @($ownerFleets | Where-Object { $script:PandoraDSTicketFleetNames -ccontains $_ }).Count -gt 0 -or
            @($script:PandoraDSTicketFleetNames | Where-Object { $name.StartsWith($_ + '-', [StringComparison]::Ordinal) }).Count -gt 0 -or
            @($containerNames | Where-Object { $_ -cin @('pandora-battle-ds', 'pandora-hub-ds') }).Count -gt 0 -or
            $hasVerifierClue
    })
    return [pscustomobject]@{
        Fleets = $fleets
        GameServers = @(Get-PandoraKubeListItems gameservers default)
        GameServerSets = $gameServerSets
        Pods = @(Get-PandoraKubeListItems pods default)
    }
}

function Get-PandoraNamedIndex {
    param($Items, [string]$Name, [string]$Where)
    $indexes = @()
    for ($i = 0; $i -lt @($Items).Count; $i++) {
        if ([string](Get-PandoraRotationProperty @($Items)[$i] 'name') -ceq $Name) { $indexes += $i }
    }
    if ($indexes.Count -ne 1) { throw "$Where name=$Name 数量=$($indexes.Count)，应为 1。" }
    return [int]$indexes[0]
}

function Get-PandoraDeploymentBundle {
    param([string]$Name)
    $deployment = Get-PandoraKubeObject "deployment/$Name" pandora
    Assert-PandoraDeploymentSafeRollingStrategy $deployment
    $volumes = @($deployment.spec.template.spec.volumes)
    $confIndex = Get-PandoraNamedIndex $volumes conf "Deployment/$Name volumes"
    $signerIndex = Get-PandoraNamedIndex $volumes dsticket "Deployment/$Name volumes"
    $configName = [string]$volumes[$confIndex].secret.secretName
    $signerName = [string]$volumes[$signerIndex].secret.secretName
    if ([string]::IsNullOrWhiteSpace($configName) -or [string]::IsNullOrWhiteSpace($signerName)) {
        throw "Deployment/$Name 的 conf/dsticket 必须是 Secret volume。"
    }
    $jwksName = ''
    $jwksIndex = -1
    if ($Name -ceq 'login') {
        $jwksIndex = Get-PandoraNamedIndex $volumes dsticket-jwks 'Deployment/login volumes'
        $jwksName = [string]$volumes[$jwksIndex].configMap.name
        if ([string]::IsNullOrWhiteSpace($jwksName)) { throw 'Deployment/login dsticket-jwks 必须是 ConfigMap volume。' }
    }
    $referenceFailures = @(Test-PandoraDSTicketSignerPodSpecReferenceContract -Spec $deployment.spec.template.spec `
        -ServiceName $Name -ExpectedConfigSecret $configName -ExpectedSignerSecret $signerName `
        -ExpectedLoginJwks $jwksName -ObjectName "Deployment/$Name")
    if ($referenceFailures.Count -gt 0) {
        throw "Deployment/$Name prewrite bundle 引用/consumer 门禁失败:$($referenceFailures -join '; ')"
    }
    return [pscustomobject]@{
        Deployment = $deployment; Name = $Name; Volumes = $volumes
        ConfigIndex = $confIndex; SignerIndex = $signerIndex; JwksIndex = $jwksIndex
        ConfigName = $configName; SignerName = $signerName; JwksName = $jwksName
    }
}

function Assert-PandoraConfigReferenceContract {
    param([string]$Name, [string]$ExpectedKid, [int]$ExpectedRevision)
    $secret = Get-PandoraKubeObject "secret/$Name" pandora
    if ($Name -ceq 'pandora-config') {
        Assert-PandoraDSTicketConfigSecretContract $secret $ExpectedKid $ExpectedRevision
    } else {
        # 每次 phase gate 都从当前 fixed 重投影完整 data；不只信任 immutable 对象的 self-hash，
        # 关闭 Ensure 后同名 Secret 被 delete/recreate 的 ABA 窗口。source RV 只作审计，不要求相等。
        $fixed = Get-PandoraKubeObject 'secret/pandora-config' pandora
        $fixedContract = Get-PandoraDSTicketConfigSubcontract $fixed
        $expected = New-PandoraDSTicketRevisionedConfigSecretObject -SourceSecret $fixed `
            -Revision $ExpectedRevision -ActiveKid $ExpectedKid `
            -AllowedCurrentActiveKids @($fixedContract.ActiveKid)
        $expectedHash = [string]$expected.metadata.annotations.'pandora.dev/dsticket-config-data-sha256'
        $null = Assert-PandoraDSTicketRevisionedConfigSecretContract $secret $ExpectedRevision $ExpectedKid $expectedHash
    }
}

function Test-PandoraBundleTuple {
    param($Bundle, [string]$Config, [string]$Signer, [string]$Jwks)
    return $Bundle.ConfigName -ceq $Config -and $Bundle.SignerName -ceq $Signer -and
        ($Bundle.Name -cne 'login' -or $Bundle.JwksName -ceq $Jwks)
}

function Assert-PandoraPhaseBundleState {
    param([Parameter(Mandatory = $true)]$Material)
    foreach ($name in $script:PandoraDSTicketSignerNames) {
        $bundle = Get-PandoraDeploymentBundle $name
        $baseJwks = if ($name -ceq 'login') { "pandora-dsticket-jwks-r$OldSignerRevision" } else { '' }
        $stageJwks = if ($name -ceq 'login') { "pandora-dsticket-jwks-r$StageRevision" } else { '' }
        $promoteJwks = if ($name -ceq 'login') { "pandora-dsticket-jwks-r$PromoteRevision" } else { '' }
        $retireJwks = if ($name -ceq 'login') { "pandora-dsticket-jwks-r$RetireRevision" } else { '' }
        $base = Test-PandoraBundleTuple $bundle pandora-config "pandora-dsticket-signer-r$OldSignerRevision" $baseJwks
        $stage = $name -ceq 'login' -and (Test-PandoraBundleTuple $bundle "pandora-config-dsticket-r$StageRevision" "pandora-dsticket-signer-r$OldSignerRevision" $stageJwks)
        $promote = Test-PandoraBundleTuple $bundle "pandora-config-dsticket-r$PromoteRevision" "pandora-dsticket-signer-r$PromoteRevision" $promoteJwks
        $retire = Test-PandoraBundleTuple $bundle "pandora-config-dsticket-r$RetireRevision" "pandora-dsticket-signer-r$RetireRevision" $retireJwks
        $final = Test-PandoraBundleTuple $bundle pandora-config "pandora-dsticket-signer-r$RetireRevision" $retireJwks
        switch ($Phase) {
            'stage' {
                if (-not ($base -or $stage)) { throw "stage 检测到 Deployment/$name 非法/半套 bundle:$($bundle.ConfigName),$($bundle.SignerName),$($bundle.JwksName)" }
                if ($stage) { Assert-PandoraConfigReferenceContract $bundle.ConfigName $Material.OldKid $StageRevision }
                else { Assert-PandoraConfigReferenceContract $bundle.ConfigName $Material.OldKid $OldSignerRevision }
            }
            'promote' {
                $before = if ($name -ceq 'login') { $stage } else { $base }
                if (-not ($before -or $promote)) { throw "promote 检测到 Deployment/$name 非法/半套 bundle:$($bundle.ConfigName),$($bundle.SignerName),$($bundle.JwksName)" }
                if ($promote) { Assert-PandoraConfigReferenceContract $bundle.ConfigName $Material.NewKid $PromoteRevision }
                elseif ($name -ceq 'login') { Assert-PandoraConfigReferenceContract $bundle.ConfigName $Material.OldKid $StageRevision }
                else { Assert-PandoraConfigReferenceContract $bundle.ConfigName $Material.OldKid $OldSignerRevision }
            }
            'retire' {
                if (-not ($promote -or $retire -or $final)) { throw "retire 检测到 Deployment/$name 非法/半套 bundle:$($bundle.ConfigName),$($bundle.SignerName),$($bundle.JwksName)" }
                if ($promote) { Assert-PandoraConfigReferenceContract $bundle.ConfigName $Material.NewKid $PromoteRevision }
                elseif ($retire) { Assert-PandoraConfigReferenceContract $bundle.ConfigName $Material.NewKid $RetireRevision }
                else { Assert-PandoraConfigReferenceContract $bundle.ConfigName $Material.NewKid $RetireRevision }
            }
        }
    }
}

function Get-PandoraApiServerNow {
    # dry-run=server 不持久化对象，但会经过 apiserver Create 路径并返回权威 creationTimestamp。
    $probe = [ordered]@{
        apiVersion = 'v1'; kind = 'ConfigMap'
        metadata = [ordered]@{ generateName = 'pandora-dsticket-clock-'; namespace = 'pandora' }
        data = [ordered]@{ purpose = 'server-clock-probe' }
    } | ConvertTo-Json -Depth 10 -Compress
    $text = Invoke-PandoraKubectl -Arguments @('create', '--dry-run=server', '-f', '-', '-o', 'json') `
        -Action '读取 apiserver 权威时间(dry-run Create)' -InputJson $probe
    try { $object = $text | ConvertFrom-Json -ErrorAction Stop }
    catch { throw "apiserver clock probe 返回非法 JSON:$($_.Exception.Message)" }
    $raw = Get-PandoraRotationProperty $object.metadata 'creationTimestamp'
    if ($null -eq $raw) { throw 'apiserver dry-run Create 未返回 metadata.creationTimestamp，禁止使用本机时钟退役 K1。' }
    [DateTimeOffset]$serverNow = if ($raw -is [DateTime]) {
        [DateTimeOffset]::new(([DateTime]$raw).ToUniversalTime())
    } elseif ($raw -is [DateTimeOffset]) { [DateTimeOffset]$raw } else {
        try { [DateTimeOffset]::Parse([string]$raw, [Globalization.CultureInfo]::InvariantCulture, [Globalization.DateTimeStyles]::AssumeUniversal) }
        catch { throw "apiserver creationTimestamp 非法:$raw" }
    }
    $skew = [Math]::Abs(($serverNow - [DateTimeOffset]::UtcNow).TotalSeconds)
    if ($skew -gt 30) { throw "本机与 apiserver 时钟偏差 ${skew}s > 30s；禁止轮换，先修时钟。" }
    return $serverNow
}

$script:PandoraRotationLock = $null

function Enter-PandoraDSTicketOperationLock {
    $holder = [guid]::NewGuid().ToString('N')
    $operation = "rotation-$Phase"
    $object = New-PandoraDSTicketOperationLockObject -HolderId $holder -Operation $operation
    $json = $object | ConvertTo-Json -Depth 20 -Compress
    $text = Invoke-PandoraKubectl -Arguments @('create', '-f', '-', '-o', 'json') `
        -Action "获取 DSTicket 操作互斥锁(operation=$operation holder=$holder)" -InputJson $json
    try { $live = $text | ConvertFrom-Json -ErrorAction Stop }
    catch { throw "DSTicket 操作锁 create 返回非法 JSON:$($_.Exception.Message)" }
    $contract = Assert-PandoraDSTicketOperationLockContract -LockObject $live -HolderId $holder `
        -Operation $operation -RequireLiveIdentity
    $script:PandoraRotationLock = [pscustomobject]@{
        HolderId = $holder; Operation = $operation; Uid = $contract.Uid; ResourceVersion = $contract.ResourceVersion
    }
    Write-Host "[OK] 已获取 DSTicket 全阶段互斥锁(uid=$($contract.Uid) operation=$operation)。" -ForegroundColor Green
}

function Assert-PandoraDSTicketOperationLockHeld {
    if ($null -eq $script:PandoraRotationLock) { throw '当前进程未持有 DSTicket 操作互斥锁。' }
    $live = Get-PandoraKubeObject "configmap/$script:PandoraDSTicketOperationLockName" pandora
    $contract = Assert-PandoraDSTicketOperationLockContract -LockObject $live `
        -HolderId $script:PandoraRotationLock.HolderId -Operation $script:PandoraRotationLock.Operation `
        -RequireLiveIdentity
    if ($contract.Uid -cne $script:PandoraRotationLock.Uid -or
        $contract.ResourceVersion -cne $script:PandoraRotationLock.ResourceVersion) {
        throw 'DSTicket 操作锁 UID/resourceVersion 漂移；禁止继续远端写入。'
    }
    return $live
}

function Exit-PandoraDSTicketOperationLock {
    if ($null -eq $script:PandoraRotationLock) { return }
    $null = Assert-PandoraDSTicketOperationLockHeld
    $uid = $script:PandoraRotationLock.Uid
    $resourceVersion = $script:PandoraRotationLock.ResourceVersion
    $deleteOptions = [ordered]@{
        apiVersion = 'v1'; kind = 'DeleteOptions'
        preconditions = [ordered]@{ uid = $uid; resourceVersion = $resourceVersion }
        propagationPolicy = 'Background'
    } | ConvertTo-Json -Depth 10 -Compress
    $uri = "/api/v1/namespaces/pandora/configmaps/$script:PandoraDSTicketOperationLockName"
    $null = Invoke-PandoraKubectl -Arguments @('delete', "--raw=$uri", '-f', '-') `
        -Action '以 UID/resourceVersion DeleteOptions CAS 释放 DSTicket 操作锁' -InputJson $deleteOptions
    $remaining = Get-PandoraKubeObject "configmap/$script:PandoraDSTicketOperationLockName" pandora -IgnoreNotFound
    if ($null -ne $remaining -and [string]$remaining.metadata.uid -ceq $uid) {
        throw "DSTicket 操作锁释放后同一 UID=$uid 仍存在。"
    }
    if ($null -ne $remaining) {
        Write-Host "[OK] 本进程锁已释放；后继 holder 已获取新锁(uid=$($remaining.metadata.uid))。" -ForegroundColor Green
    } else {
        Write-Host '[OK] DSTicket 操作互斥锁已 CAS 释放并确认消失。' -ForegroundColor Green
    }
    $script:PandoraRotationLock = $null
}

function Assert-PandoraPromotePreflight {
    param([Parameter(Mandatory = $true)]$Material)
    if ($null -ne (Get-PandoraRotationMarker)) {
        throw 'promote 写入前发现 marker 已存在；停止并转只读核对。'
    }
    $snapshot = Get-PandoraLiveDSGateSnapshot
    $null = Assert-PandoraDSTicketLiveDSRevisionGate $snapshot.Fleets $snapshot.GameServers `
        $snapshot.GameServerSets $snapshot.Pods $StageRevision
    $null = Assert-PandoraDSTicketDSRevisionSetGate $snapshot.Fleets $snapshot.GameServers `
        $snapshot.GameServerSets $snapshot.Pods @($StageRevision)
    $null = Assert-PandoraPreviousRotationHistoryStable -Material $Material
    Assert-PandoraPhaseBundleState $Material
}

function Assert-PandoraPhaseConcurrencyGate {
    param([Parameter(Mandatory = $true)]$Material)
    $marker = Get-PandoraRotationMarker
    $signerControllers = Get-PandoraSignerSnapshot
    $null = Assert-PandoraReplicaSetsMatchOwningDeploymentGate `
        -DeploymentObjects $signerControllers.Deployments -ReplicaSetObjects $signerControllers.ReplicaSets
    switch ($Phase) {
        'stage' {
            if ($null -ne $marker) { throw 'K2 激活 marker 已存在，禁止 stage/回退 K1。' }
            $null = Assert-PandoraPreviousRotationHistoryStable -Material $Material
            $snapshot = Get-PandoraLiveDSGateSnapshot
            $null = Assert-PandoraDSTicketDSRevisionSetGate $snapshot.Fleets $snapshot.GameServers `
                $snapshot.GameServerSets $snapshot.Pods @($OldSignerRevision, $StageRevision)
        }
        'promote' {
            Assert-PandoraPromotePreflight $Material
            return
        }
        'retire' {
            if ($null -eq $marker) { throw '缺 K2 激活 marker，禁止 retire。' }
            $null = Assert-PandoraPreviousRotationHistoryStable -Material $Material -IncludeCurrentActivation
            $snapshot = Get-PandoraLiveDSGateSnapshot
            $null = Assert-PandoraDSTicketDSRevisionSetGate $snapshot.Fleets $snapshot.GameServers `
                $snapshot.GameServerSets $snapshot.Pods @($PromoteRevision, $RetireRevision)
            $serverNow = Get-PandoraApiServerNow
            $null = Assert-PandoraDSTicketActivationMarkerContract -MarkerObject $marker `
                -StageRevision $StageRevision -PromoteRevision $PromoteRevision -RetireRevision $RetireRevision `
                -OldSignerRevision $OldSignerRevision -OldKid $Material.OldKid -NewKid $Material.NewKid `
                -ServerNow $serverNow -RequireRetireWindow
        }
    }
    Assert-PandoraPhaseBundleState $Material
}

function Invoke-PandoraGuardedWrite {
    param([string]$Action, [scriptblock]$Operation)
    # 不缓存：每笔写入前先证明仍持有同一 UID/RV 锁，再重读全部 immutable key material，
    # 并重跑 phase/bundle/live/marker/history gate。
    $null = Assert-PandoraDSTicketOperationLockHeld
    $material = Get-PandoraRotationLiveMaterialContract
    Assert-PandoraPhaseConcurrencyGate $material
    # gate 本身可能在慢集群读取大量对象；真正写入前再做一次紧邻 UID/RV holder 校验。
    $null = Assert-PandoraDSTicketOperationLockHeld
    & $Operation
    Write-Host "[OK] $Action" -ForegroundColor Green
}

function Ensure-PandoraRevisionedConfigSecret {
    param([int]$Revision, [string]$ActiveKid, [string[]]$AllowedSourceKids)
    $name = "pandora-config-dsticket-r$Revision"
    $source = Get-PandoraKubeObject 'secret/pandora-config' pandora
    $expected = New-PandoraDSTicketRevisionedConfigSecretObject -SourceSecret $source -Revision $Revision `
        -ActiveKid $ActiveKid -AllowedCurrentActiveKids $AllowedSourceKids
    $expectedHash = [string]$expected.metadata.annotations.'pandora.dev/dsticket-config-data-sha256'
    $existing = Get-PandoraKubeObject "secret/$name" pandora -IgnoreNotFound
    if ($null -ne $existing) {
        $null = Assert-PandoraDSTicketRevisionedConfigSecretContract $existing $Revision $ActiveKid $expectedHash
        return $existing
    }
    Invoke-PandoraGuardedWrite -Action "create/核对 immutable Secret/$name（当前 fixed 完整 data 投影）" -Operation {
        # guarded gate 之后重新读取 fixed 并投影，避免 source read→create 窗口把陈旧/篡改 data 固化。
        $guardedSource = Get-PandoraKubeObject 'secret/pandora-config' pandora
        $guardedObject = New-PandoraDSTicketRevisionedConfigSecretObject -SourceSecret $guardedSource -Revision $Revision `
            -ActiveKid $ActiveKid -AllowedCurrentActiveKids $AllowedSourceKids
        $guardedHash = [string]$guardedObject.metadata.annotations.'pandora.dev/dsticket-config-data-sha256'
        $concurrent = Get-PandoraKubeObject "secret/$name" pandora -IgnoreNotFound
        if ($null -ne $concurrent) {
            $null = Assert-PandoraDSTicketRevisionedConfigSecretContract $concurrent $Revision $ActiveKid $guardedHash
            return
        }
        $null = Assert-PandoraDSTicketOperationLockHeld
        $json = $guardedObject | ConvertTo-Json -Depth 100 -Compress
        $null = Invoke-PandoraKubectl -Arguments @('create', '-f', '-', '-o', 'name') `
            -Action "create Secret/$name" -InputJson $json
    }
    $live = Get-PandoraKubeObject "secret/$name" pandora
    $validationSource = Get-PandoraKubeObject 'secret/pandora-config' pandora
    $validationExpected = New-PandoraDSTicketRevisionedConfigSecretObject -SourceSecret $validationSource -Revision $Revision `
        -ActiveKid $ActiveKid -AllowedCurrentActiveKids $AllowedSourceKids
    $validationHash = [string]$validationExpected.metadata.annotations.'pandora.dev/dsticket-config-data-sha256'
    $null = Assert-PandoraDSTicketRevisionedConfigSecretContract $live $Revision $ActiveKid $validationHash
    return $live
}

function Set-PandoraDeploymentBundleCAS {
    param(
        [string]$Name,
        [string]$ConfigSecretName,
        [string]$SignerSecretName,
        [string]$LoginJwksName,
        [string]$RolloutToken
    )
    $bundle = Get-PandoraDeploymentBundle $Name
    $deployment = $bundle.Deployment
    $rv = [string]$deployment.metadata.resourceVersion
    if ([string]::IsNullOrWhiteSpace($rv)) { throw "Deployment/$Name 缺 resourceVersion，不能 CAS。" }
    $ops = [System.Collections.Generic.List[object]]::new()
    $ops.Add([ordered]@{ op = 'test'; path = '/metadata/resourceVersion'; value = $rv })
    $confPath = "/spec/template/spec/volumes/$($bundle.ConfigIndex)/secret/secretName"
    $signerPath = "/spec/template/spec/volumes/$($bundle.SignerIndex)/secret/secretName"
    $ops.Add([ordered]@{ op = 'test'; path = $confPath; value = $bundle.ConfigName })
    $ops.Add([ordered]@{ op = 'test'; path = $signerPath; value = $bundle.SignerName })
    if ($Name -ceq 'login') {
        $jwksPath = "/spec/template/spec/volumes/$($bundle.JwksIndex)/configMap/name"
        $ops.Add([ordered]@{ op = 'test'; path = $jwksPath; value = $bundle.JwksName })
    }
    if ($bundle.ConfigName -cne $ConfigSecretName) { $ops.Add([ordered]@{ op = 'replace'; path = $confPath; value = $ConfigSecretName }) }
    if ($bundle.SignerName -cne $SignerSecretName) { $ops.Add([ordered]@{ op = 'replace'; path = $signerPath; value = $SignerSecretName }) }
    if ($Name -ceq 'login' -and $bundle.JwksName -cne $LoginJwksName) {
        $ops.Add([ordered]@{ op = 'replace'; path = $jwksPath; value = $LoginJwksName })
    }
    $annotations = Get-PandoraRotationProperty $deployment.spec.template.metadata 'annotations'
    if ($null -eq $annotations) {
        $ops.Add([ordered]@{ op = 'add'; path = '/spec/template/metadata/annotations'; value = [ordered]@{ 'pandora.dev/dsticket-rotation' = $RolloutToken } })
    } else {
        $currentToken = Get-PandoraRotationProperty $annotations 'pandora.dev/dsticket-rotation'
        if ($null -ne $currentToken) {
            $ops.Add([ordered]@{ op = 'test'; path = '/spec/template/metadata/annotations/pandora.dev~1dsticket-rotation'; value = [string]$currentToken })
        }
        $ops.Add([ordered]@{ op = 'add'; path = '/spec/template/metadata/annotations/pandora.dev~1dsticket-rotation'; value = $RolloutToken })
    }
    $patch = @($ops) | ConvertTo-Json -Depth 20 -Compress
    Invoke-PandoraGuardedWrite -Action "CAS Deployment/$Name bundle(config=$ConfigSecretName signer=$SignerSecretName jwks=$LoginJwksName)" -Operation {
        $null = Invoke-PandoraKubectl -Arguments @('patch', "deployment/$Name", '-n', 'pandora', '--type=json', '-p', $patch, '-o', 'name') `
            -Action "CAS patch Deployment/$Name bundle"
    }
}

function Set-PandoraFleetKeysetRevisionCAS {
    param([string]$FleetName, [int]$Revision)
    $fleet = Get-PandoraKubeObject "fleet/$FleetName" default
    $rv = [string]$fleet.metadata.resourceVersion
    if ([string]::IsNullOrWhiteSpace($rv)) { throw "Fleet/$FleetName 缺 resourceVersion，不能 CAS。" }
    $containerName = if ($FleetName.StartsWith('pandora-battle-', [StringComparison]::Ordinal)) { 'pandora-battle-ds' } else { 'pandora-hub-ds' }
    $podSpec = $fleet.spec.template.spec.template.spec
    $ci = Get-PandoraNamedIndex @($podSpec.containers) $containerName "Fleet/$FleetName containers"
    $ei = Get-PandoraNamedIndex @($podSpec.containers[$ci].env) PANDORA_DSTICKET_KEYSET_REVISION "Fleet/$FleetName env"
    $vi = Get-PandoraNamedIndex @($podSpec.volumes) dsticket-jwks "Fleet/$FleetName volumes"
    $oldRevision = [string]$podSpec.containers[$ci].env[$ei].value
    $oldConfigMap = [string]$podSpec.volumes[$vi].configMap.name
    if ($oldRevision -cnotmatch '^[1-9][0-9]*$' -or [string]::IsNullOrWhiteSpace($oldConfigMap)) {
        throw "Fleet/$FleetName 当前 DSTicket env/ConfigMap 非法。"
    }
    $envPath = "/spec/template/spec/template/spec/containers/$ci/env/$ei/value"
    $cmPath = "/spec/template/spec/template/spec/volumes/$vi/configMap/name"
    $ops = [System.Collections.Generic.List[object]]::new()
    $ops.Add([ordered]@{ op = 'test'; path = '/metadata/resourceVersion'; value = $rv })
    $ops.Add([ordered]@{ op = 'test'; path = $envPath; value = $oldRevision })
    $ops.Add([ordered]@{ op = 'test'; path = $cmPath; value = $oldConfigMap })
    if ($oldRevision -cne [string]$Revision) { $ops.Add([ordered]@{ op = 'replace'; path = $envPath; value = [string]$Revision }) }
    $targetCM = "pandora-dsticket-jwks-r$Revision"
    if ($oldConfigMap -cne $targetCM) { $ops.Add([ordered]@{ op = 'replace'; path = $cmPath; value = $targetCM }) }
    if ($ops.Count -eq 3) { return }
    $patch = @($ops) | ConvertTo-Json -Depth 20 -Compress
    Invoke-PandoraGuardedWrite -Action "CAS Fleet/$FleetName public JWKS r$Revision" -Operation {
        $null = Invoke-PandoraKubectl -Arguments @('patch', "fleet/$FleetName", '-n', 'default', '--type=json', '-p', $patch, '-o', 'name') `
            -Action "CAS patch Fleet/$FleetName"
    }
}

function Wait-PandoraDeploymentRollout([string]$Name) {
    $null = Invoke-PandoraKubectl -Arguments @('rollout', 'status', "deployment/$Name", '-n', 'pandora', "--timeout=${TimeoutSec}s") `
        -Action "等待 Deployment/$Name rollout"
}

function Wait-PandoraSignerControllerQuiescence {
    $deadline = [DateTimeOffset]::UtcNow.AddSeconds($TimeoutSec)
    $last = '尚未采样'
    while ([DateTimeOffset]::UtcNow -lt $deadline) {
        try {
            $snapshot = Get-PandoraSignerSnapshot
            $null = Assert-PandoraReplicaSetsMatchOwningDeploymentGate `
                -DeploymentObjects $snapshot.Deployments -ReplicaSetObjects $snapshot.ReplicaSets
            Write-Host '[OK] signer ReplicaSet 已与各 owning Deployment 当前模板静默一致。' -ForegroundColor Green
            return
        } catch {
            $last = $_.Exception.Message
            Write-Host "[WAIT] signer ReplicaSet 仍在滚动/终止:$last" -ForegroundColor Yellow
            Start-Sleep $PollIntervalSec
        }
    }
    throw "等待 signer ReplicaSet owner/template 静默一致超时:$last"
}

function Wait-PandoraLiveDSRevision([int]$Revision) {
    $deadline = [DateTimeOffset]::UtcNow.AddSeconds($TimeoutSec)
    $last = @('尚未采样')
    while ([DateTimeOffset]::UtcNow -lt $deadline) {
        $snapshot = Get-PandoraLiveDSGateSnapshot
        $gate = Test-PandoraDSTicketLiveDSRevisionGate $snapshot.Fleets $snapshot.GameServers `
            $snapshot.GameServerSets $snapshot.Pods $Revision
        if ($gate.Ok) {
            Write-Host "[OK] 四 Fleet/全部 live DS/Pod 已收敛到 r$Revision（live=$($gate.LiveGameServers.Count)）。" -ForegroundColor Green
            return
        }
        $last = @($gate.Failures)
        Write-Host "[WAIT] live DS r${Revision}:$($last -join '; ')" -ForegroundColor Yellow
        Start-Sleep $PollIntervalSec
    }
    throw "等待 live DS r$Revision 超时:$($last -join '; ')。脚本不会 delete/强杀旧 Allocated/Reserved/terminating 对象。"
}

function Wait-PandoraSignerBundleState {
    param(
        [string]$ActiveKid,
        [int]$SignerRevision,
        [int]$LoginRevision,
        [hashtable]$ConfigByDeployment,
        [hashtable]$ConfigRevisionByDeployment
    )
    $deadline = [DateTimeOffset]::UtcNow.AddSeconds($TimeoutSec)
    $last = '尚未采样'
    while ([DateTimeOffset]::UtcNow -lt $deadline) {
        try {
            foreach ($name in $script:PandoraDSTicketSignerNames) {
                Assert-PandoraConfigReferenceContract ([string]$ConfigByDeployment[$name]) $ActiveKid `
                    ([int]$ConfigRevisionByDeployment[$name])
            }
            $snapshot = Get-PandoraSignerSnapshot
            Assert-PandoraDSTicketSignerDeploymentGate -DeploymentObjects $snapshot.Deployments `
                -ReplicaSetObjects $snapshot.ReplicaSets -PodObjects $snapshot.Pods `
                -SignerRevision $SignerRevision -LoginKeysetRevision $LoginRevision `
                -ExpectedConfigSecretByDeployment $ConfigByDeployment
            Write-Host "[OK] 四 signer bundle 全绿:signer=r$SignerRevision login=r$LoginRevision。" -ForegroundColor Green
            return
        } catch {
            $last = $_.Exception.Message
            Write-Host "[WAIT] signer bundle 尚未收敛:$last" -ForegroundColor Yellow
            Start-Sleep $PollIntervalSec
        }
    }
    throw "等待 signer bundle 全绿超时:$last"
}

function Assert-PandoraNoSignerUsesFixedConfig {
    $snapshot = Get-PandoraSignerSnapshot
    foreach ($deployment in $snapshot.Deployments) {
        $bundle = Get-PandoraDeploymentBundle ([string]$deployment.metadata.name)
        if ($bundle.ConfigName -ceq 'pandora-config') { throw "Deployment/$($bundle.Name) 仍引用 fixed pandora-config，不能安全回填。" }
    }
    foreach ($pod in $snapshot.Pods) {
        $meta = $pod.metadata
        $podSpec = Get-PandoraRotationProperty $pod 'spec'
        if (-not (Test-PandoraDSTicketDSPodSpecClue $podSpec)) { continue }
        $fixedRefs = @(Get-PandoraDSTicketSignerConfigSecretReferences $podSpec | Where-Object {
            $_.Name -ceq 'pandora-config'
        })
        if ($fixedRefs.Count -gt 0) {
            throw "Pod/$($meta.name)（含 terminating/错标/全引用 consumer）同时使用 DSTicket material 与 fixed pandora-config，不能安全回填。"
        }
    }
}

function Set-PandoraFixedConfigTerminalState {
    param([Parameter(Mandatory = $true)]$Material)
    Assert-PandoraNoSignerUsesFixedConfig
    $secret = Get-PandoraKubeObject 'secret/pandora-config' pandora
    $updated = Get-PandoraDSTicketConfigSecretUpdatedData -SecretObject $secret -ActiveKid $Material.NewKid `
        -LoginKeysetRevision $RetireRevision -AllowedCurrentActiveKids @($Material.OldKid, $Material.NewKid)
    foreach ($service in $script:PandoraDSTicketSignerNames) {
        $key = "$service.yaml"
        $secret.data.PSObject.Properties[$key].Value = [string]$updated[$key]
    }
    $subcontract = Get-PandoraDSTicketConfigSubcontract $secret
    if ($null -eq $secret.metadata.annotations) { $secret.metadata | Add-Member -NotePropertyName annotations -NotePropertyValue ([pscustomobject]@{}) }
    foreach ($entry in ([ordered]@{
        'pandora.dev/dsticket-terminal-revision' = [string]$RetireRevision
        'pandora.dev/dsticket-terminal-active-kid' = $Material.NewKid
        'pandora.dev/dsticket-terminal-config-contract-sha256' = $subcontract.Sha256
    }).GetEnumerator()) {
        $property = $secret.metadata.annotations.PSObject.Properties[$entry.Key]
        if ($null -eq $property) { $secret.metadata.annotations | Add-Member -NotePropertyName $entry.Key -NotePropertyValue $entry.Value }
        else { $property.Value = $entry.Value }
    }
    $null = $secret.metadata.PSObject.Properties.Remove('managedFields')
    $json = $secret | ConvertTo-Json -Depth 100 -Compress
    Invoke-PandoraGuardedWrite -Action "CAS replace fixed pandora-config 到 terminal r$RetireRevision" -Operation {
        # 入口检查后仍可能异步补出 Pod；replace 前按 direct/projected/env/envFrom 全引用重扫。
        Assert-PandoraNoSignerUsesFixedConfig
        $null = Assert-PandoraDSTicketOperationLockHeld
        $null = Invoke-PandoraKubectl -Arguments @('replace', '-f', '-', '-o', 'name') `
            -Action 'replace fixed pandora-config(resourceVersion CAS)' -InputJson $json
    }
    Assert-PandoraNoSignerUsesFixedConfig
    $live = Get-PandoraKubeObject 'secret/pandora-config' pandora
    Assert-PandoraDSTicketConfigSecretContract $live $Material.NewKid $RetireRevision
    $liveContract = Get-PandoraDSTicketConfigSubcontract $live
    if ([string]$live.metadata.annotations.'pandora.dev/dsticket-terminal-revision' -cne [string]$RetireRevision -or
        [string]$live.metadata.annotations.'pandora.dev/dsticket-terminal-active-kid' -cne $Material.NewKid -or
        [string]$live.metadata.annotations.'pandora.dev/dsticket-terminal-config-contract-sha256' -cne $liveContract.Sha256) {
        throw 'fixed pandora-config terminal annotation/hash 对账失败。'
    }
    return $liveContract.Sha256
}

function Assert-PandoraActivationRuntimeState {
    param([Parameter(Mandatory = $true)]$Material, [switch]$RequireMarker)
    $marker = Get-PandoraRotationMarker
    if ($RequireMarker) {
        if ($null -eq $marker) { throw 'promote exact runtime 缺 activation marker。' }
        $null = Assert-PandoraDSTicketActivationMarkerContract -MarkerObject $marker `
            -StageRevision $StageRevision -PromoteRevision $PromoteRevision -RetireRevision $RetireRevision `
            -OldSignerRevision $OldSignerRevision -OldKid $Material.OldKid -NewKid $Material.NewKid
    } elseif ($null -ne $marker) {
        throw 'promote exact runtime 写前发现 activation marker 已存在。'
    }
    $configs = Get-PandoraConfigMapForAll "pandora-config-dsticket-r$PromoteRevision"
    foreach ($name in $script:PandoraDSTicketSignerNames) {
        Assert-PandoraConfigReferenceContract $configs[$name] $Material.NewKid $PromoteRevision
    }
    $snapshot = Get-PandoraSignerSnapshot
    $ds = Get-PandoraLiveDSGateSnapshot
    Assert-PandoraDSTicketRuntimeObjectGate -DeploymentObjects $snapshot.Deployments `
        -ReplicaSetObjects $snapshot.ReplicaSets -SignerPodObjects $snapshot.Pods `
        -FleetObjects $ds.Fleets -GameServerObjects $ds.GameServers `
        -GameServerSetObjects $ds.GameServerSets -DSPodObjects $ds.Pods `
        -SignerRevision $PromoteRevision -LoginKeysetRevision $PromoteRevision `
        -ExpectedConfigSecretByDeployment $configs -DSRevision $StageRevision
    return $marker
}

function New-PandoraActivationMarker {
    param([Parameter(Mandatory = $true)]$Material)
    $existing = Get-PandoraRotationMarker
    if ($null -ne $existing) {
        $null = Assert-PandoraActivationRuntimeState $Material -RequireMarker
        return
    }
    $marker = New-PandoraDSTicketActivationMarkerObject -StageRevision $StageRevision `
        -PromoteRevision $PromoteRevision -RetireRevision $RetireRevision -OldSignerRevision $OldSignerRevision `
        -OldKid $Material.OldKid -NewKid $Material.NewKid
    $json = $marker | ConvertTo-Json -Depth 30 -Compress
    Invoke-PandoraGuardedWrite -Action "create immutable activation marker/$($marker.metadata.name)" -Operation {
        $null = Assert-PandoraActivationRuntimeState $Material
        # exact runtime 全量读取可能较慢；真正 create 前再次紧邻证明仍持有同一 UID/RV 锁。
        $null = Assert-PandoraDSTicketOperationLockHeld
        $null = Invoke-PandoraKubectl -Arguments @('create', '-f', '-', '-o', 'name') `
            -Action 'create DSTicket activation marker' -InputJson $json
    }
    $null = Assert-PandoraActivationRuntimeState $Material -RequireMarker
}

function Get-PandoraTerminalMarker {
    $name = Get-PandoraDSTicketTerminalMarkerName $RetireRevision
    return Get-PandoraKubeObject "configmap/$name" pandora -IgnoreNotFound
}

function New-PandoraTerminalMarker {
    param([Parameter(Mandatory = $true)]$Material, [Parameter(Mandatory = $true)][string]$FixedContractSha256)
    $existing = Get-PandoraTerminalMarker
    $activation = Get-PandoraRotationMarker
    if ($null -eq $activation) { throw '缺 activation marker，禁止创建/核对 terminal marker。' }
    if ($null -ne $existing) {
        $null = Assert-PandoraTerminalMarkerRuntimeState $Material $FixedContractSha256 -RequireMarker
        return
    }
    $marker = New-PandoraDSTicketTerminalMarkerObject -PromoteRevision $PromoteRevision `
        -RetireRevision $RetireRevision -ActiveKid $Material.NewKid `
        -FixedConfigContractSha256 $FixedContractSha256
    $json = $marker | ConvertTo-Json -Depth 30 -Compress
    Invoke-PandoraGuardedWrite -Action "create immutable terminal marker/$($marker.metadata.name)" -Operation {
        $null = Assert-PandoraTerminalMarkerRuntimeState $Material $FixedContractSha256
        # exact terminal 全态读取后再次紧邻核同一锁，避免慢 gate 后锁已漂移仍写 immutable marker。
        $null = Assert-PandoraDSTicketOperationLockHeld
        $null = Invoke-PandoraKubectl -Arguments @('create', '-f', '-', '-o', 'name') `
            -Action 'create DSTicket terminal marker' -InputJson $json
    }
    $null = Assert-PandoraTerminalMarkerRuntimeState $Material $FixedContractSha256 -RequireMarker
}

function Get-PandoraConfigMapForAll([string]$Name) {
    $map = @{}
    foreach ($service in $script:PandoraDSTicketSignerNames) { $map[$service] = $Name }
    return $map
}

function Get-PandoraRevisionMapForAll([int]$Revision) {
    $map = @{}
    foreach ($service in $script:PandoraDSTicketSignerNames) { $map[$service] = $Revision }
    return $map
}

function Assert-PandoraTerminalRuntimeState {
    param([Parameter(Mandatory = $true)]$Material, [switch]$RequireMarker)
    $fixed = Get-PandoraKubeObject 'secret/pandora-config' pandora
    Assert-PandoraDSTicketConfigSecretContract $fixed $Material.NewKid $RetireRevision
    $subcontract = Get-PandoraDSTicketConfigSubcontract $fixed
    $snapshot = Get-PandoraSignerSnapshot
    $configMap = Get-PandoraConfigMapForAll pandora-config
    Assert-PandoraDSTicketSignerDeploymentGate $snapshot.Deployments $snapshot.ReplicaSets `
        $snapshot.Pods $RetireRevision $RetireRevision $configMap
    $ds = Get-PandoraLiveDSGateSnapshot
    $null = Assert-PandoraDSTicketLiveDSRevisionGate $ds.Fleets $ds.GameServers `
        $ds.GameServerSets $ds.Pods $RetireRevision
    if ($RequireMarker) {
        $activation = Get-PandoraRotationMarker
        if ($null -eq $activation) { throw 'terminal runtime 缺 activation marker。' }
        $terminal = Get-PandoraTerminalMarker
        if ($null -eq $terminal) { throw 'terminal runtime 已收敛但缺 terminal marker。' }
        $null = Assert-PandoraDSTicketTerminalMarkerContract $terminal $PromoteRevision $RetireRevision `
            $Material.NewKid $subcontract.Sha256 $activation
        $audit = Get-PandoraRotationAuditMarkerSnapshot
        $null = Assert-PandoraDSTicketOrdinaryMarkerState -RequestedRevision $RetireRevision `
            -ActivationMarkers $audit.Activations -TerminalMarkers $audit.Terminals -FixedConfigSecret $fixed
    }
    return $subcontract
}

function Assert-PandoraTerminalMarkerRuntimeState {
    param(
        [Parameter(Mandatory = $true)]$Material,
        [Parameter(Mandatory = $true)][string]$FixedContractSha256,
        [switch]$RequireMarker
    )
    $activation = Get-PandoraRotationMarker
    if ($null -eq $activation) { throw 'terminal exact runtime 缺 activation marker。' }
    $terminal = Get-PandoraTerminalMarker
    if ($RequireMarker) {
        if ($null -eq $terminal) { throw 'terminal exact runtime 缺 terminal marker。' }
    } elseif ($null -ne $terminal) {
        throw 'terminal exact runtime 写前发现 terminal marker 已存在。'
    }
    $subcontract = Assert-PandoraTerminalRuntimeState $Material -RequireMarker:$RequireMarker
    if ($subcontract.Sha256 -cne $FixedContractSha256) {
        throw "terminal exact runtime fixed 子契约 hash=$($subcontract.Sha256) expected=$FixedContractSha256。"
    }
    $fixed = Get-PandoraKubeObject 'secret/pandora-config' pandora
    Assert-PandoraDSTicketFixedTerminalAnnotations -FixedConfigSecret $fixed -ConfigContract $subcontract -Require
    return $terminal
}

function Invoke-PandoraStagePhase {
    $material = Get-PandoraRotationLiveMaterialContract
    Assert-PandoraPhaseConcurrencyGate $material
    $null = Ensure-PandoraRevisionedConfigSecret $StageRevision $material.OldKid @($material.OldKid)
    $token = "stage-r$StageRevision-$([guid]::NewGuid().ToString('N'))"
    Set-PandoraDeploymentBundleCAS login "pandora-config-dsticket-r$StageRevision" `
        "pandora-dsticket-signer-r$OldSignerRevision" "pandora-dsticket-jwks-r$StageRevision" $token
    Wait-PandoraDeploymentRollout login
    Wait-PandoraSignerControllerQuiescence
    foreach ($fleet in $script:PandoraDSTicketFleetNames) { Set-PandoraFleetKeysetRevisionCAS $fleet $StageRevision }
    Wait-PandoraLiveDSRevision $StageRevision
    $configs = Get-PandoraConfigMapForAll pandora-config
    $revisions = Get-PandoraRevisionMapForAll $OldSignerRevision
    $configs['login'] = "pandora-config-dsticket-r$StageRevision"
    $revisions['login'] = $StageRevision
    Wait-PandoraSignerBundleState $material.OldKid $OldSignerRevision $StageRevision $configs $revisions
    Write-Host "DSTicket stage 完成:r$StageRevision overlap 已覆盖 Login/四 Fleet/全部 live DS；K2 signer-r$StageRevision 未挂载。" -ForegroundColor Green
}

function Invoke-PandoraPromotePhase {
    $material = Get-PandoraRotationLiveMaterialContract
    $marker = Get-PandoraRotationMarker
    if ($null -ne $marker) {
        $null = Assert-PandoraDSTicketActivationMarkerContract -MarkerObject $marker `
            -StageRevision $StageRevision -PromoteRevision $PromoteRevision -RetireRevision $RetireRevision `
            -OldSignerRevision $OldSignerRevision -OldKid $material.OldKid -NewKid $material.NewKid
        $null = Assert-PandoraPreviousRotationHistoryStable -Material $material -IncludeCurrentActivation
        $ds = Get-PandoraLiveDSGateSnapshot
        $null = Assert-PandoraDSTicketLiveDSRevisionGate $ds.Fleets $ds.GameServers `
            $ds.GameServerSets $ds.Pods $StageRevision
        $configs = Get-PandoraConfigMapForAll "pandora-config-dsticket-r$PromoteRevision"
        $revisions = Get-PandoraRevisionMapForAll $PromoteRevision
        $snapshot = Get-PandoraSignerSnapshot
        Assert-PandoraDSTicketSignerDeploymentGate $snapshot.Deployments $snapshot.ReplicaSets `
            $snapshot.Pods $PromoteRevision $PromoteRevision $configs
        foreach ($name in $script:PandoraDSTicketSignerNames) {
            Assert-PandoraConfigReferenceContract $configs[$name] $material.NewKid $revisions[$name]
        }
        Write-Host 'DSTicket promote 已完成且 activation marker/live r3 bundle 一致；幂等只读退出。' -ForegroundColor Green
        return
    }
    Assert-PandoraPromotePreflight $material
    $null = Ensure-PandoraRevisionedConfigSecret $PromoteRevision $material.NewKid @($material.OldKid)
    $token = "promote-r$PromoteRevision-$([guid]::NewGuid().ToString('N'))"
    foreach ($name in $script:PandoraDSTicketSignerNames) {
        $jwks = if ($name -ceq 'login') { "pandora-dsticket-jwks-r$PromoteRevision" } else { '' }
        Set-PandoraDeploymentBundleCAS $name "pandora-config-dsticket-r$PromoteRevision" `
            "pandora-dsticket-signer-r$PromoteRevision" $jwks $token
        Wait-PandoraDeploymentRollout $name
        Wait-PandoraSignerControllerQuiescence
    }
    $configs = Get-PandoraConfigMapForAll "pandora-config-dsticket-r$PromoteRevision"
    $revisions = Get-PandoraRevisionMapForAll $PromoteRevision
    Wait-PandoraSignerBundleState $material.NewKid $PromoteRevision $PromoteRevision $configs $revisions
    New-PandoraActivationMarker $material
    Write-Host "DSTicket promote 完成:四 signer 原子 bundle r$PromoteRevision 全绿且最后一个 K1 Pod 已消失；marker 由 apiserver creationTimestamp 起算。" -ForegroundColor Green
}

function Invoke-PandoraRetirePhase {
    $material = Get-PandoraRotationLiveMaterialContract
    $activation = Get-PandoraRotationMarker
    if ($null -eq $activation) { throw '缺 activation marker，禁止 retire。' }
    $terminal = Get-PandoraTerminalMarker
    if ($null -ne $terminal) {
        $null = Assert-PandoraTerminalRuntimeState $material -RequireMarker
        Write-Host 'DSTicket retire 已处于 terminal fixed rN，ordinary 发布可在完整门禁后继续；幂等只读退出。' -ForegroundColor Green
        return
    }
    Assert-PandoraPhaseConcurrencyGate $material
    $null = Ensure-PandoraRevisionedConfigSecret $RetireRevision $material.NewKid @($material.OldKid, $material.NewKid)
    foreach ($fleet in $script:PandoraDSTicketFleetNames) { Set-PandoraFleetKeysetRevisionCAS $fleet $RetireRevision }
    Wait-PandoraLiveDSRevision $RetireRevision

    $token = "retire-r$RetireRevision-$([guid]::NewGuid().ToString('N'))"
    foreach ($name in $script:PandoraDSTicketSignerNames) {
        $jwks = if ($name -ceq 'login') { "pandora-dsticket-jwks-r$RetireRevision" } else { '' }
        Set-PandoraDeploymentBundleCAS $name "pandora-config-dsticket-r$RetireRevision" `
            "pandora-dsticket-signer-r$RetireRevision" $jwks $token
        Wait-PandoraDeploymentRollout $name
        Wait-PandoraSignerControllerQuiescence
    }
    $phaseConfigs = Get-PandoraConfigMapForAll "pandora-config-dsticket-r$RetireRevision"
    $phaseRevisions = Get-PandoraRevisionMapForAll $RetireRevision
    Wait-PandoraSignerBundleState $material.NewKid $RetireRevision $RetireRevision $phaseConfigs $phaseRevisions

    $fixedContractHash = Set-PandoraFixedConfigTerminalState $material
    $handoffToken = "terminal-fixed-r$RetireRevision-$([guid]::NewGuid().ToString('N'))"
    foreach ($name in $script:PandoraDSTicketSignerNames) {
        $jwks = if ($name -ceq 'login') { "pandora-dsticket-jwks-r$RetireRevision" } else { '' }
        Set-PandoraDeploymentBundleCAS $name pandora-config "pandora-dsticket-signer-r$RetireRevision" $jwks $handoffToken
        Wait-PandoraDeploymentRollout $name
        Wait-PandoraSignerControllerQuiescence
    }
    $fixedConfigs = Get-PandoraConfigMapForAll pandora-config
    $fixedRevisions = Get-PandoraRevisionMapForAll $RetireRevision
    Wait-PandoraSignerBundleState $material.NewKid $RetireRevision $RetireRevision $fixedConfigs $fixedRevisions
    $terminalState = Assert-PandoraTerminalRuntimeState $material
    if ($terminalState.Sha256 -cne $fixedContractHash) { throw 'terminal handoff 前后 DSTicket 子契约 hash 漂移。' }
    New-PandoraTerminalMarker $material $fixedContractHash
    $null = Assert-PandoraTerminalRuntimeState $material -RequireMarker
    Write-Host "DSTicket retire 完成:四 Fleet/DS/signer/Login/fixed config 全部 r$RetireRevision；旧对象保留审计，未 delete/强杀。" -ForegroundColor Green
}

Write-Host "DSTicket rotation:phase=$Phase context=$KubeContext stage=r$StageRevision promote=r$PromoteRevision retire=r$RetireRevision old_signer=r$OldSignerRevision" -ForegroundColor Cyan
Enter-PandoraDSTicketOperationLock
try {
    switch ($Phase) {
        'stage' { Invoke-PandoraStagePhase }
        'promote' { Invoke-PandoraPromotePhase }
        'retire' { Invoke-PandoraRetirePhase }
        default { throw "不支持 phase:$Phase" }
    }
} finally {
    Exit-PandoraDSTicketOperationLock
}
