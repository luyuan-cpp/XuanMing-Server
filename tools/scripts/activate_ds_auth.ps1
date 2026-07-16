# Pandora DS callback auth protocol epoch=2 一次性激活器。
#
# Audit（默认）只读 K8s/etcd/Redis 和固定 synthetic 结果，绝不 patch/apply/create。
# Activate 仅在独立 green writer、revisioned immutable mTLS Secret、运行时 capability、
# Redis 投影和固定 synthetic 证据全部成立后，才执行 blue→green 切换并最终 CAS etcd 1→2。
# 本脚本不生成、接收或回显 CA、私钥、密码；这些材料必须由平台预先提供。
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$KubeContext,
    [Parameter(Mandatory = $true)][string]$EtcdEndpoints,
    [Parameter(Mandatory = $true)][string]$RedisAddrs,
    [Parameter(Mandatory = $true)][string]$KeysetRevision,
    [Parameter(Mandatory = $true)][string]$EtcdIdentityRevision,
    [Parameter(Mandatory = $true)][string[]]$AllowedImageDigests,
    [Parameter(Mandatory = $true)][string]$EtcdCAFile,
    [Parameter(Mandatory = $true)][string]$EtcdClientCertFile,
    [Parameter(Mandatory = $true)][string]$EtcdClientKeyFile,
    [Parameter(Mandatory = $true)][string]$EtcdServerName,
    [Parameter(Mandatory = $true)][string]$EtcdClientIdentity,
    [Parameter(Mandatory = $true)][string]$EtcdForbiddenReadPrefix,
    [string]$EtcdUsernameFile = '',
    [string]$EtcdPasswordFile = '',
    [Parameter(Mandatory = $true)][string]$ActivationRunId,
    [Parameter(Mandatory = $true)][string]$SyntheticProbeImageDigest,
    [Parameter(Mandatory = $true)][string]$PodUIDPreflightRedisSecretRevision,
    [ValidateSet('Audit', 'Activate')][string]$Phase = 'Audit',
    [string]$RedisPasswordEnv = 'PANDORA_REDIS_PASSWORD',
    [string]$RedisACLAdminUsernameEnv = '',
    [string]$RedisACLAdminPasswordEnv = '',
    [string]$PandoraNamespace = 'pandora',
    [string]$DSNamespace = 'default',
    [string]$EtcdPrefix = '/pandora/ds-auth/',
    [uint32]$ExpectedEpoch = 1,
    [uint32]$TargetEpoch = 2,
    [ValidateRange(3, 100)][int]$RedisAuditCycles = 3,
    [timespan]$RedisAuditInterval = '00:00:06',
    [timespan]$SyntheticMaxAge = '00:30:00'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$ProjectRoot = (Resolve-Path "$PSScriptRoot/../..").Path
. (Join-Path $PSScriptRoot 'lib/ds_auth_activation_contract.ps1')

$WriterEpochLabel = 'pandora.dev/ds-auth-writer-epoch'
$WriterSetLabel = 'pandora.dev/ds-auth-writer-set'
$RequiredEpochLabel = 'pandora.dev/ds-auth-required-epoch'
$DigestAnnotation = 'pandora.dev/image-digest'
$Apps = @($script:PandoraDsAuthWriterApps)
$CapabilityNames = @{
    'login' = 'login'; 'player-locator' = 'player_locator'; 'ds-allocator' = 'ds_allocator'
    'hub-allocator' = 'hub_allocator'; 'battle-result' = 'battle_result'
}
$ActivationLockName = "pandora-ds-auth-activation-v$TargetEpoch"
$DrainMarkerName = "pandora-ds-auth-drained-v$TargetEpoch-$ActivationRunId"
$SwitchMarkerName = "pandora-ds-auth-switch-v$TargetEpoch-$ActivationRunId"
$GreenReadyMarkerName = "pandora-ds-auth-green-ready-v$TargetEpoch-$ActivationRunId"
$PodUIDEvidenceMarkerName = "pandora-pod-uid-evidence-v$TargetEpoch-$ActivationRunId"
$PodUIDPreflightRedisSecretName = "pandora-pod-uid-preflight-redis-ro-$PodUIDPreflightRedisSecretRevision"
$PodUIDACLCleanupRequiredMarkerName = "pandora-pod-uid-acl-cleanup-required-v$TargetEpoch-$ActivationRunId"
$PodUIDACLCleanupCompleteMarkerName = "pandora-pod-uid-acl-cleanup-complete-v$TargetEpoch-$ActivationRunId"
$PodUIDPreflightACLUser = 'pandora-pod-uid-release-preflight-ro'
$RequiredPolicyV2 = 'ds-auth-v2-pod-uid-write-invariant-v1'
$ExpectedRequiredValue = '1'
$TargetRequiredValue = "2@$RequiredPolicyV2"

function Invoke-Kubectl([string[]]$Arguments) {
    $output = @(& kubectl --context $KubeContext @Arguments 2>&1)
    if ($LASTEXITCODE -ne 0) { throw "kubectl failed: $($output -join [Environment]::NewLine)" }
    return $output
}

function Invoke-KubectlJson([string[]]$Arguments) {
    $text = (Invoke-Kubectl $Arguments) -join "`n"
    if ([string]::IsNullOrWhiteSpace($text)) { throw 'kubectl 未返回 JSON。' }
    try { return $text | ConvertFrom-Json -ErrorAction Stop }
    catch { throw "kubectl 返回非法 JSON:$($_.Exception.Message)" }
}

function Get-OptionalKubectlJson([string[]]$Arguments) {
    $output = @(& kubectl --context $KubeContext @Arguments 2>&1)
    if ($LASTEXITCODE -ne 0) { throw "kubectl failed: $($output -join [Environment]::NewLine)" }
    $text = ($output -join "`n").Trim()
    if ([string]::IsNullOrWhiteSpace($text)) { return $null }
    try { return $text | ConvertFrom-Json -ErrorAction Stop }
    catch { throw "kubectl 返回非法 JSON:$($_.Exception.Message)" }
}

function Invoke-KubectlCreateObject($Object) {
    $json = $Object | ConvertTo-Json -Depth 20 -Compress
    $output = @($json | & kubectl --context $KubeContext create -f - 2>&1)
    if ($LASTEXITCODE -ne 0) { throw "kubectl create failed: $($output -join [Environment]::NewLine)" }
}

function Assert-Digest([string]$Digest, [string]$Where) {
    if ($Digest -cnotmatch '^sha256:[0-9a-f]{64}$') { throw "$Where 缺 immutable sha256 digest。" }
    if ($AllowedImageDigests -cnotcontains $Digest) { throw "$Where digest=$Digest 不在显式 allowed 清单。" }
}

function Test-PodReady($Pod) {
    return @($Pod.status.conditions | Where-Object { $_.type -ceq 'Ready' -and $_.status -ceq 'True' }).Count -eq 1
}

function Get-EndpointUIDSet([string]$ServiceName, $Service, $ExpectedPods) {
    $list = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', 'endpointslices', '-l', "kubernetes.io/service-name=$ServiceName", '-o', 'json')
    return Get-PandoraDsAuthVerifiedEndpointUIDSet $list $Service $ExpectedPods $PandoraNamespace
}

function Assert-ExactUIDSet($ExpectedUIDs, $ActualUIDs, [string]$Where) {
    $expected = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::Ordinal)
    foreach ($uid in @($ExpectedUIDs)) { $null = $expected.Add([string]$uid) }
    if ($expected.Count -ne $ActualUIDs.Count) { throw "$Where Endpoint UID 数不一致。" }
    foreach ($uid in $expected) {
        if (-not $ActualUIDs.Contains($uid)) { throw "$Where Endpoint UID 集不一致。" }
    }
}

function Get-GreenBackendContract([ValidateSet('zero', 'live', 'either')][string]$State) {
    $serviceCounts = [ordered]@{}
    $serviceUIDs = [ordered]@{}
	$desiredReplicas = [ordered]@{}
	$serviceDigests = [ordered]@{}
    $allDigests = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::Ordinal)
    $observedStates = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::Ordinal)
    foreach ($app in $Apps) {
        $deploymentName = "$app-ds-auth-green"
        $deployment = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', "deployment/$deploymentName", '-o', 'json')
        $desiredRaw = [string]$deployment.metadata.annotations.'pandora.dev/ds-auth-green-desired-replicas'
        if ($desiredRaw -cnotmatch '^[1-9][0-9]?$') { throw "$deploymentName 缺 canonical green desired replicas annotation。" }
        $want = [int]$desiredRaw
        $desiredReplicas[$app] = $want
        $current = [int]$deployment.spec.replicas
        if ($current -notin @(0, $want)) { throw "$deploymentName replicas=$current 既不是 0 也不是受审 desired=$want。" }
        $observedState = if ($current -eq 0) { 'zero' } else { 'live' }
        $null = $observedStates.Add($observedState)
        if ($State -cne 'either' -and $observedState -cne $State) { throw "$deploymentName state=$observedState，expected=$State。" }
        if ($observedState -ceq 'live' -and
            ([int]$deployment.status.observedGeneration -lt [int]$deployment.metadata.generation -or
             [int]$deployment.status.updatedReplicas -ne $want -or [int]$deployment.status.readyReplicas -ne $want -or
             [int]$deployment.status.availableReplicas -ne $want)) {
            throw "$deploymentName rollout 未稳定 updated/ready/available=$want。"
        }
        if ([string]$deployment.spec.template.metadata.labels.app -cne $app -or
            [string]$deployment.spec.template.metadata.labels.$WriterSetLabel -cne 'green' -or
            [string]$deployment.spec.template.metadata.labels.$WriterEpochLabel -cne [string]$TargetEpoch) {
            throw "$deploymentName Pod template 未固定 app/green/writer epoch。"
        }
        if ([string]$deployment.spec.selector.matchLabels.app -cne $app -or
            [string]$deployment.spec.selector.matchLabels.$WriterSetLabel -cne 'green' -or
            [string]$deployment.spec.selector.matchLabels.$WriterEpochLabel -cne [string]$TargetEpoch -or
            @($deployment.spec.selector.matchLabels.PSObject.Properties).Count -ne 3) {
            throw "$deploymentName immutable selector 不是 exact app+green+epoch=$TargetEpoch。"
        }

        $secretName = Get-PandoraDsAuthIdentitySecretName $app $EtcdIdentityRevision
        $secret = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', "secret/$secretName", '-o', 'json')
        $secretContract = Assert-PandoraDsAuthIdentitySecretContract $secret $app $EtcdIdentityRevision
        $templatePod = [pscustomobject]@{ metadata = $deployment.spec.template.metadata; spec = $deployment.spec.template.spec }
        Assert-PandoraDsAuthIdentityPodContract $templatePod $app $EtcdIdentityRevision $EtcdServerName `
            $EtcdForbiddenReadPrefix ([bool]$secretContract.UsesPasswordAuth)
        if ($app -in @('battle-result', 'ds-allocator')) { Assert-PandoraDsTerminalMeshTemplateContract $templatePod $app }
        $templateContainer = Get-PandoraNamedObject @($deployment.spec.template.spec.containers) $app 'green template container'
        $templateDigest = [string]$deployment.spec.template.metadata.annotations.$DigestAnnotation
        Assert-Digest $templateDigest "$deploymentName template"
        if ([string]$templateContainer.image -cnotmatch ('@' + [regex]::Escape($templateDigest) + '$')) {
            throw "$deploymentName template image 未固定到 annotation digest。"
        }
		$null = $allDigests.Add($templateDigest)
		$serviceDigests[$CapabilityNames[$app]] = $templateDigest
        $podsObject = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', 'pods', '-l', "app=$app,$WriterSetLabel=green", '-o', 'json')
        $pods = @($podsObject.items)
        $expectedPods = if ($observedState -ceq 'live') { $want } else { 0 }
        if ($pods.Count -ne $expectedPods) { throw "$deploymentName green Pod 数=$($pods.Count)，expected=$expectedPods。" }
        $uids = [System.Collections.Generic.List[string]]::new()
        foreach ($pod in $pods) {
            if ((Test-PandoraKubernetesObjectDeleting $pod) -or -not (Test-PodReady $pod)) {
                throw "$app/$($pod.metadata.name) 非稳定 Ready。"
            }
            if ([string]$pod.metadata.labels.$WriterEpochLabel -cne [string]$TargetEpoch) {
                throw "$app/$($pod.metadata.name) writer epoch 不等于 $TargetEpoch。"
            }
            Assert-PandoraDsAuthIdentityPodContract $pod $app $EtcdIdentityRevision $EtcdServerName `
                $EtcdForbiddenReadPrefix ([bool]$secretContract.UsesPasswordAuth)
            if ($app -in @('battle-result', 'ds-allocator')) { Assert-PandoraDsTerminalMeshPodContract $pod $app }
            $digest = [string]$pod.metadata.annotations.$DigestAnnotation
            Assert-Digest $digest "$app/$($pod.metadata.name)"
            if ($digest -cne $templateDigest) { throw "$app/$($pod.metadata.name) digest 与受审 template 不一致。" }
            $null = $allDigests.Add($digest)
            $mainStatus = @($pod.status.containerStatuses | Where-Object { $_.name -ceq $app })
            if ($mainStatus.Count -ne 1 -or -not ([string]$mainStatus[0].imageID).EndsWith($digest, [StringComparison]::Ordinal)) {
                throw "$app/$($pod.metadata.name) imageID 未命中 annotation digest。"
            }
            $uids.Add([string]$pod.metadata.uid)
        }

        $previewService = "$app-ds-auth-green"
        $service = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', "service/$previewService", '-o', 'json')
        if ([string]$service.spec.selector.app -cne $app -or
            [string]$service.spec.selector.$WriterSetLabel -cne 'green' -or
            [string]$service.spec.selector.$WriterEpochLabel -cne [string]$TargetEpoch -or
            @($service.spec.selector.PSObject.Properties).Count -ne 3) {
            throw "service/$previewService 未精确选择 green writer epoch=$TargetEpoch。"
        }
        Assert-ExactUIDSet $uids (Get-EndpointUIDSet $previewService $service $pods) "service/$previewService"
        if ($observedState -ceq 'live') {
            $capName = $CapabilityNames[$app]
            $serviceCounts[$capName] = $want
            $serviceUIDs[$capName] = (@($uids | Sort-Object) -join '|')
        }
    }
    if ($observedStates.Count -ne 1 -and $State -cne 'either') { throw 'green 五个 Deployment 不能处于混合 zero/live 状态。' }
    return [pscustomobject]@{
        State = if ($observedStates.Count -eq 1) { @($observedStates)[0] } else { 'mixed' }
        DesiredReplicas = $desiredReplicas
        Services = (($serviceCounts.Keys | ForEach-Object { "$_=$($serviceCounts[$_])" }) -join ',')
        Instances = (($serviceUIDs.Keys | ForEach-Object { "$_=$($serviceUIDs[$_])" }) -join ',')
		Digests = (@($allDigests | Sort-Object) -join ',')
		ServiceDigests = (($serviceDigests.Keys | ForEach-Object { "$_=$($serviceDigests[$_])" }) -join ',')
    }
}

function Assert-TerminalMeshPolicy {
    $peer = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', 'peerauthentication.security.istio.io/pandora-ds-allocator-terminal-permissive', '-o', 'json')
    $policy = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', 'authorizationpolicy.security.istio.io/pandora-ds-terminal-release-exact-deny', '-o', 'json')
    $service = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', 'service/ds-allocator', '-o', 'json')
    Assert-PandoraDsTerminalMeshPolicyContract $peer $policy $service
}

function Assert-GameServers {
    $list = Invoke-KubectlJson @('-n', $DSNamespace, 'get', 'pods', '-l', 'agones.dev/role=gameserver', '-o', 'json')
    $pods = @($list.items)
    if ($pods.Count -eq 0) { throw '未发现 Agones GameServer Pod；无法证明 UE DS writer epoch=2。' }
    foreach ($pod in $pods) {
        if ((Test-PandoraKubernetesObjectDeleting $pod) -or -not (Test-PodReady $pod)) { throw "GameServer $($pod.metadata.name) 非稳定 Ready。" }
        if ([string]$pod.metadata.labels.$WriterEpochLabel -cne [string]$TargetEpoch) { throw "GameServer $($pod.metadata.name) writer epoch 不匹配。" }
        $digest = [string]$pod.metadata.annotations.$DigestAnnotation
        Assert-Digest $digest "GameServer/$($pod.metadata.name)"
        $ue = @($pod.status.containerStatuses | Where-Object { $_.name -in @('pandora-battle-ds', 'pandora-hub-ds') })
        if ($ue.Count -ne 1 -or -not ([string]$ue[0].imageID).EndsWith($digest, [StringComparison]::Ordinal)) {
            throw "GameServer/$($pod.metadata.name) imageID 未命中 annotation digest。"
        }
    }
}

function Invoke-RedisAudit {
    $intervalArg = ([decimal]$RedisAuditInterval.TotalSeconds).ToString([Globalization.CultureInfo]::InvariantCulture) + 's'
    & go run ./pkg/dsauthfence/cmd/dsauth-redis-audit --addrs $RedisAddrs --password-env $RedisPasswordEnv `
        --cycles $RedisAuditCycles --interval $intervalArg --min-hubs 1 --min-battles 1
    if ($LASTEXITCODE -ne 0) { throw 'Redis active/pending/projection/heartbeat 连续审计失败。' }
}

function Get-SyntheticEvidence([ValidateSet('prepare', 'final')][string]$SyntheticPhase, [datetimeoffset]$NotBefore) {
    $name = "pandora-ds-auth-synthetic-v1-$ActivationRunId-$SyntheticPhase"
    $job = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', "job/$name", '-o', 'json')
    $podList = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', 'pods', '-l', "job-name=$name", '-o', 'json')
    $pods = @($podList.items)
    if ($pods.Count -ne 1) { throw "synthetic Job/$name 必须且只能有一个 Pod。" }
    Assert-PandoraDsAuthSyntheticContract $job $pods[0] $PandoraNamespace $ActivationRunId $SyntheticPhase `
        $SyntheticProbeImageDigest $TargetEpoch $KeysetRevision $EtcdIdentityRevision $SyntheticMaxAge $NotBefore
}

function Get-SecureGoArgs {
    return @(Get-PandoraDsAuthSecureGoArgs $EtcdCAFile $EtcdClientCertFile $EtcdClientKeyFile `
        $EtcdServerName $EtcdClientIdentity $EtcdIdentityRevision $EtcdForbiddenReadPrefix `
        $EtcdUsernameFile $EtcdPasswordFile)
}

function Get-GoAuditArgs($Audit, $ActivationEvidence = $null) {
    $args = @('run', './pkg/dsauthfence/cmd/dsauth-activate', '--endpoints', $EtcdEndpoints,
        '--prefix', $EtcdPrefix, '--expected-services', $Audit.Services, '--expected-instances', $Audit.Instances,
        '--expected-epoch', [string]$ExpectedEpoch, '--target-epoch', [string]$TargetEpoch,
        '--keyset-revision', $KeysetRevision, '--etcd-identity-revision', $EtcdIdentityRevision,
		'--allowed-image-digests', $Audit.Digests, '--expected-image-digests', $Audit.ServiceDigests,
		'--required-features', $script:PandoraDsAuthRequiredFeatures)
    if ($null -ne $ActivationEvidence) {
        if ([string]$ActivationEvidence.EvidenceSHA256 -cnotmatch '^sha256:[0-9a-f]{64}$' -or
            [int64]$ActivationEvidence.FinalCompletionTimeUnixMS -le 0) {
            throw 'etcd activation evidence args 非 canonical。'
        }
        $args += @('--activation-evidence-sha256', [string]$ActivationEvidence.EvidenceSHA256,
            '--activation-evidence-completed-at-ms', [string]$ActivationEvidence.FinalCompletionTimeUnixMS)
    }
    $args += Get-SecureGoArgs
    return $args
}

function Get-RequiredSnapshot {
    $args = @('run', './pkg/dsauthfence/cmd/dsauth-required', '--endpoints', $EtcdEndpoints, '--prefix', $EtcdPrefix,
        '--min-epoch', [string]$ExpectedEpoch, '--max-epoch', [string]$TargetEpoch,
        '--min-policy-generation', '1', '--max-policy-generation', '2', '--output', 'json')
    $args += Get-SecureGoArgs
    $output = @(& go @args 2>&1)
    if ($LASTEXITCODE -ne 0) { throw "required_writer_epoch 只读检查失败:$($output -join [Environment]::NewLine)" }
    try { $snapshot = (($output -join "`n") | ConvertFrom-Json -ErrorAction Stop) }
    catch { throw "required_writer_epoch 未返回机器 JSON:$($_.Exception.Message)" }
    if ((@($snapshot.PSObject.Properties.Name | Sort-Object) -join ',') -cne
        ((@('epoch', 'policy_generation', 'policy_id', 'raw_value', 'mod_revision') | Sort-Object) -join ',') -or
        [string]$snapshot.mod_revision -cnotmatch '^[1-9][0-9]*$') {
        throw 'required_writer_epoch snapshot 非 exact versioned contract。'
    }
    if ([uint32]$snapshot.epoch -eq $ExpectedEpoch) {
        if ([uint32]$snapshot.policy_generation -ne 1 -or
            [string]$snapshot.raw_value -cne $ExpectedRequiredValue -or
            -not [string]::IsNullOrEmpty([string]$snapshot.policy_id)) {
            throw 'baseline required_writer_epoch raw value/policy 非 canonical。'
        }
    } elseif ([uint32]$snapshot.epoch -eq $TargetEpoch) {
        if ([uint32]$snapshot.policy_generation -ne 2 -or
            [string]$snapshot.raw_value -cne $TargetRequiredValue -or
            [string]$snapshot.policy_id -cne $RequiredPolicyV2) {
            throw 'target required_writer_epoch 缺固定 rollback policy fence；naked epoch=2 被拒绝。'
        }
    } else { throw "required_writer_epoch=$($snapshot.epoch) 非受支持 policy state。" }
    return $snapshot
}

function Invoke-CapabilityAudit($Audit, [switch]$ApplyEpoch, $ActivationEvidence = $null) {
    $args = @(Get-GoAuditArgs $Audit $ActivationEvidence)
    if ($ApplyEpoch) { $args += '--apply' }
    & go @args
    if ($LASTEXITCODE -ne 0) { throw 'etcd exact capability/feature 审计或 CAS 失败。' }
}

function Invoke-EmptyCapabilityAudit($Candidate) {
    $deadline = [datetime]::UtcNow.AddSeconds(45)
    do {
        $args = @(Get-GoAuditArgs $Candidate)
        $args += '--require-empty-capabilities'
        & go @args
        if ($LASTEXITCODE -eq 0) { return }
        if ([datetime]::UtcNow -ge $deadline) { throw 'blue 排空后 capability lease 仍未全部消失。' }
        Start-Sleep -Seconds 2
    } while ($true)
}

function Assert-LiveServiceTrack([ValidateSet('blue', 'green', 'either')][string]$ExpectedTrack) {
    foreach ($app in $Apps) {
        $service = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', "service/$app", '-o', 'json')
        $track = [string]$service.spec.selector.$WriterSetLabel
        if ([string]$service.spec.selector.app -cne $app -or $track -notin @('blue', 'green') -or
            ($ExpectedTrack -cne 'either' -and $track -cne $ExpectedTrack) -or
            @($service.spec.selector.PSObject.Properties).Count -ne 3) {
            throw "service/$app writer-set=$track，expected=$ExpectedTrack。"
        }
        $expectedEpoch = if ($track -ceq 'green') { $TargetEpoch } else { $ExpectedEpoch }
        if ([string]$service.spec.selector.$WriterEpochLabel -cne [string]$expectedEpoch) { throw "service/$app $track selector epoch 不匹配。" }
        $trackPods = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', 'pods', '-l', "app=$app,$WriterSetLabel=$track", '-o', 'json')
        Assert-ExactUIDSet @($trackPods.items | ForEach-Object { [string]$_.metadata.uid }) `
            (Get-EndpointUIDSet $app $service @($trackPods.items)) "service/$app"
    }
}

function Assert-BlueDeployments([bool]$MustBeScaledDown) {
    foreach ($app in $Apps) {
        $deployment = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', "deployment/$app", '-o', 'json')
        if ([string]$deployment.spec.template.metadata.labels.app -cne $app -or
            [string]$deployment.spec.template.metadata.labels.$WriterSetLabel -cne 'blue' -or
            [string]$deployment.spec.template.metadata.labels.$WriterEpochLabel -cne [string]$ExpectedEpoch) {
            throw "deployment/$app 不是 exact blue writer epoch=$ExpectedEpoch。"
        }
        if ([string]$deployment.spec.selector.matchLabels.app -cne $app -or
            [string]$deployment.spec.selector.matchLabels.$WriterSetLabel -cne 'blue' -or
            [string]$deployment.spec.selector.matchLabels.$WriterEpochLabel -cne [string]$ExpectedEpoch -or
            @($deployment.spec.selector.matchLabels.PSObject.Properties).Count -ne 3) {
            throw "deployment/$app immutable selector 不是 exact app+blue+epoch=$ExpectedEpoch。"
        }
        $replicas = [int]$deployment.spec.replicas
        $pods = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', 'pods', '-l', "app=$app,$WriterSetLabel=blue", '-o', 'json')
        $bluePods = @($pods.items)
        if ($MustBeScaledDown) {
            if ($replicas -ne 0 -or $bluePods.Count -ne 0) { throw "deployment/$app 尚未完全 scale=0。" }
        } elseif ($replicas -lt 1) {
            throw "deployment/$app blue baseline 不能为 0。"
        } else {
            if ($bluePods.Count -ne $replicas) { throw "deployment/$app blue Pod 数不等于 desired。" }
            foreach ($pod in $bluePods) {
                if ((Test-PandoraKubernetesObjectDeleting $pod) -or -not (Test-PodReady $pod) -or
                    [string]$pod.metadata.labels.$WriterEpochLabel -cne [string]$ExpectedEpoch) {
                    throw "deployment/$app 含非 Ready 或非 epoch=$ExpectedEpoch blue Pod。"
                }
            }
        }
    }
}

function Get-ReleaseEvidenceHash {
    $material = @($AllowedImageDigests | Sort-Object) -join ','
    $desired = foreach ($app in $Apps) {
        $deployment = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', "deployment/$app-ds-auth-green", '-o', 'json')
        $value = [string]$deployment.metadata.annotations.'pandora.dev/ds-auth-green-desired-replicas'
        if ($value -cnotmatch '^[1-9][0-9]?$') { throw "deployment/$app-ds-auth-green desired replicas 非 canonical。" }
        "$app=$value"
    }
    $material += "|$($desired -join ',')|$KeysetRevision|$EtcdIdentityRevision|$ExpectedEpoch|$TargetEpoch|$ExpectedRequiredValue|$TargetRequiredValue|$RequiredPolicyV2|$SyntheticProbeImageDigest"
    $material += "|$EtcdEndpoints|$EtcdServerName|$EtcdClientIdentity|$EtcdForbiddenReadPrefix"
    $sha = [Security.Cryptography.SHA256]::Create()
    try { return ([Convert]::ToHexString($sha.ComputeHash([Text.Encoding]::UTF8.GetBytes($material)))).ToLowerInvariant() }
    finally { $sha.Dispose() }
}

function Get-SHA256DigestFromBytes([byte[]]$Bytes) {
    $sha = [Security.Cryptography.SHA256]::Create()
    try { return 'sha256:' + ([Convert]::ToHexString($sha.ComputeHash($Bytes))).ToLowerInvariant() }
    finally { $sha.Dispose() }
}

function Get-PodUIDConfigCompareSourceSHA256 {
    $roots = @(
        (Join-Path $ProjectRoot 'services/battle/ds_allocator'),
        (Join-Path $ProjectRoot 'pkg'),
        (Join-Path $ProjectRoot 'proto/gen/go')
    )
    $files = [System.Collections.Generic.List[System.IO.FileInfo]]::new()
    foreach ($root in $roots) {
        foreach ($file in Get-ChildItem -LiteralPath $root -Recurse -File) {
            if (($file.Extension -ceq '.go' -and $file.Name -cnotmatch '_test\.go$') -or
                $file.Name -cin @('go.mod', 'go.sum')) {
                $files.Add($file)
            }
        }
    }
    foreach ($name in @('go.work', 'go.work.sum')) {
        $path = Join-Path $ProjectRoot $name
        if (Test-Path -LiteralPath $path -PathType Leaf) { $files.Add((Get-Item -LiteralPath $path)) }
    }
    $ordered = @($files | Sort-Object { [IO.Path]::GetRelativePath($ProjectRoot, $_.FullName) })
    if ($ordered.Count -eq 0) { throw 'pod_uid config compare source set 为空。' }
    $hash = [Security.Cryptography.IncrementalHash]::CreateHash(
        [Security.Cryptography.HashAlgorithmName]::SHA256)
    try {
        foreach ($file in $ordered) {
            $relative = ([IO.Path]::GetRelativePath($ProjectRoot, $file.FullName)).Replace('\', '/')
            $pathBytes = [Text.Encoding]::UTF8.GetBytes($relative)
            $body = [IO.File]::ReadAllBytes($file.FullName)
            $hash.AppendData([BitConverter]::GetBytes([int]$pathBytes.Length))
            $hash.AppendData($pathBytes)
            $hash.AppendData([BitConverter]::GetBytes([int64]$body.LongLength))
            $hash.AppendData($body)
        }
        return 'sha256:' + ([Convert]::ToHexString($hash.GetHashAndReset())).ToLowerInvariant()
    } finally { $hash.Dispose() }
}

function Get-SecretDataBytes($Secret, [string]$Key, [string]$Where) {
    $property = if ($null -eq $Secret.data) { $null } else { $Secret.data.PSObject.Properties[$Key] }
    if ($null -eq $property -or [string]::IsNullOrWhiteSpace([string]$property.Value)) {
        throw "$Where 缺非空 data/$Key。"
    }
    try { return [Convert]::FromBase64String([string]$property.Value) }
    catch { throw "$Where data/$Key 不是 canonical base64。" }
}

function Get-LivePandoraConfigSourceEvidence {
    $secret = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', 'secret/pandora-config', '-o', 'json')
    $bytes = Get-SecretDataBytes $secret 'ds-allocator.yaml' 'Secret/pandora-config'
    $uid = [string]$secret.metadata.uid
    $rv = [string]$secret.metadata.resourceVersion
    if ([string]$secret.metadata.name -cne 'pandora-config' -or
        [string]$secret.metadata.namespace -cne $PandoraNamespace -or
        $uid -cnotmatch '^[0-9a-f][0-9a-f-]{7,127}$' -or
        $rv -cnotmatch '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$' -or
        (Test-PandoraKubernetesObjectDeleting $secret)) {
        throw 'Secret/pandora-config UID/resourceVersion/namespace/deletion 非 canonical。'
    }
    return [pscustomobject][ordered]@{
        Name = 'pandora-config'; UID = $uid; ResourceVersion = $rv
        ConfigSHA256 = Get-SHA256DigestFromBytes $bytes
        EncodedConfig = [Convert]::ToBase64String($bytes)
    }
}

function Assert-RawPodUIDConfigSnapshot($Secret, $SourceEvidence) {
    $expectedName = "pandora-dsa-cfg-v$TargetEpoch-$ActivationRunId-$(([string]$SourceEvidence.ConfigSHA256).Substring(7, 12))"
    if ([string]$Secret.apiVersion -cne 'v1' -or [string]$Secret.kind -cne 'Secret' -or
        [string]$Secret.metadata.name -cne $expectedName -or [string]$Secret.metadata.namespace -cne $PandoraNamespace -or
        $Secret.immutable -ne $true -or [string]$Secret.type -cne 'Opaque' -or
        [string]$Secret.metadata.annotations.'pandora.dev/source-secret-name' -cne 'pandora-config' -or
        [string]$Secret.metadata.annotations.'pandora.dev/source-secret-uid' -cne [string]$SourceEvidence.UID -or
        [string]$Secret.metadata.annotations.'pandora.dev/source-secret-resource-version' -cne [string]$SourceEvidence.ResourceVersion -or
        [string]$Secret.metadata.annotations.'pandora.dev/source-ds-allocator-sha256' -cne [string]$SourceEvidence.ConfigSHA256 -or
        (Test-PandoraKubernetesObjectDeleting $Secret) -or
        @($Secret.data.PSObject.Properties).Count -ne 1) {
        throw "raw config snapshot Secret/$expectedName 身份/source binding/immutable contract 非 canonical。"
    }
    $bytes = Get-SecretDataBytes $Secret 'ds-allocator.yaml' "Secret/$expectedName"
    $uid = [string]$Secret.metadata.uid
    $rv = [string]$Secret.metadata.resourceVersion
    if ((Get-SHA256DigestFromBytes $bytes) -cne [string]$SourceEvidence.ConfigSHA256 -or
        $uid -cnotmatch '^[0-9a-f][0-9a-f-]{7,127}$' -or
        $rv -cnotmatch '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$') {
        throw "raw config snapshot Secret/$expectedName bytes/UID/resourceVersion 漂移。"
    }
    return [pscustomobject][ordered]@{
        Name = $expectedName; UID = $uid; ResourceVersion = $rv
        ConfigSHA256 = [string]$SourceEvidence.ConfigSHA256
    }
}

function Ensure-RawPodUIDConfigSnapshot($SourceEvidence) {
    $name = "pandora-dsa-cfg-v$TargetEpoch-$ActivationRunId-$(([string]$SourceEvidence.ConfigSHA256).Substring(7, 12))"
    $secret = Get-OptionalKubectlJson @('-n', $PandoraNamespace, 'get', "secret/$name", '--ignore-not-found', '-o', 'json')
    if ($null -eq $secret) {
        $object = [ordered]@{
            apiVersion = 'v1'; kind = 'Secret'; type = 'Opaque'; immutable = $true
            metadata = [ordered]@{
                name = $name; namespace = $PandoraNamespace
                labels = [ordered]@{
                    'pandora.dev/ds-auth-activation' = [string]$TargetEpoch
                    'pandora.dev/activation-run-id' = $ActivationRunId
                    'pandora.dev/config-snapshot' = 'ds-allocator-raw'
                }
                annotations = [ordered]@{
                    'pandora.dev/source-secret-name' = 'pandora-config'
                    'pandora.dev/source-secret-uid' = [string]$SourceEvidence.UID
                    'pandora.dev/source-secret-resource-version' = [string]$SourceEvidence.ResourceVersion
                    'pandora.dev/source-ds-allocator-sha256' = [string]$SourceEvidence.ConfigSHA256
                }
            }
            data = [ordered]@{ 'ds-allocator.yaml' = [string]$SourceEvidence.EncodedConfig }
        }
        Invoke-KubectlCreateObject $object
        $secret = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', "secret/$name", '-o', 'json')
    }
    $result = Assert-RawPodUIDConfigSnapshot $secret $SourceEvidence
    return $result
}

function Get-PodUIDPreflightROSecretEvidence {
    $secret = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get',
        "secret/$PodUIDPreflightRedisSecretName", '-o', 'json')
    if ([string]$secret.metadata.name -cne $PodUIDPreflightRedisSecretName -or
        [string]$secret.metadata.namespace -cne $PandoraNamespace -or $secret.immutable -ne $true -or
        [string]$secret.type -cne 'Opaque' -or (Test-PandoraKubernetesObjectDeleting $secret) -or
        @($secret.data.PSObject.Properties).Count -ne 3) {
        throw "RO preflight Secret/$PodUIDPreflightRedisSecretName 非 immutable exact three-key contract。"
    }
    foreach ($required in @('ds-allocator-preflight.yaml', 'username', 'password')) {
        $null = Get-SecretDataBytes $secret $required "Secret/$PodUIDPreflightRedisSecretName"
    }
    $username = [Text.Encoding]::UTF8.GetString(
        (Get-SecretDataBytes $secret 'username' "Secret/$PodUIDPreflightRedisSecretName"))
    if ($username -cne 'pandora-pod-uid-release-preflight-ro') {
        throw 'RO preflight Secret username 不是固定最小权限身份。'
    }
    $configBytes = Get-SecretDataBytes $secret 'ds-allocator-preflight.yaml' "Secret/$PodUIDPreflightRedisSecretName"
    $uid = [string]$secret.metadata.uid
    $rv = [string]$secret.metadata.resourceVersion
    if ($uid -cnotmatch '^[0-9a-f][0-9a-f-]{7,127}$' -or
        $rv -cnotmatch '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$') {
        throw 'RO preflight Secret UID/resourceVersion 非 canonical。'
    }
    return [pscustomobject][ordered]@{
        Name = $PodUIDPreflightRedisSecretName; UID = $uid; ResourceVersion = $rv
        ConfigSHA256 = Get-SHA256DigestFromBytes $configBytes
        EncodedConfig = [Convert]::ToBase64String($configBytes)
    }
}

function Invoke-PodUIDRedisConfigIdentityCompare($SourceEvidence, $ROSecretEvidence) {
    if ([string]::IsNullOrWhiteSpace([string]$SourceEvidence.EncodedConfig) -or
        [string]::IsNullOrWhiteSpace([string]$ROSecretEvidence.EncodedConfig)) {
        throw 'Redis config identity compare 缺内存中的 source/RO base64 输入。'
    }
    $inputObject = [ordered]@{
        writer_config_base64 = [string]$SourceEvidence.EncodedConfig
        read_only_config_base64 = [string]$ROSecretEvidence.EncodedConfig
    }
    $inputJSON = $inputObject | ConvertTo-Json -Compress
    $sourceSHA256 = Get-PodUIDConfigCompareSourceSHA256
    $oldGoProxy = $env:GOPROXY
    $oldGoSumDB = $env:GOSUMDB
    $oldGoToolchain = $env:GOTOOLCHAIN
    $env:GOPROXY = 'off'
    $env:GOSUMDB = 'off'
    $env:GOTOOLCHAIN = 'local'
    Push-Location (Join-Path $ProjectRoot 'services/battle/ds_allocator')
    try {
        $output = @($inputJSON | & go run -mod=readonly ./cmd/ds_allocator `
            -pod-uid-release-preflight-compare-configs 2>&1)
        $compareExit = $LASTEXITCODE
    } finally {
        Pop-Location
        if ($null -eq $oldGoProxy) { Remove-Item Env:GOPROXY -ErrorAction SilentlyContinue } else { $env:GOPROXY = $oldGoProxy }
        if ($null -eq $oldGoSumDB) { Remove-Item Env:GOSUMDB -ErrorAction SilentlyContinue } else { $env:GOSUMDB = $oldGoSumDB }
        if ($null -eq $oldGoToolchain) { Remove-Item Env:GOTOOLCHAIN -ErrorAction SilentlyContinue } else { $env:GOTOOLCHAIN = $oldGoToolchain }
    }
    if ($compareExit -ne 0) {
        throw "pinned release source Redis config identity compare 失败（输入未回显）:$($output -join [Environment]::NewLine)"
    }
    try { $result = (($output -join "`n") | ConvertFrom-Json -ErrorAction Stop) }
    catch { throw 'Redis config identity compare 未返回唯一机器 JSON。' }
    if ($result.matched -ne $true -or
        [string]$result.redis_config_identity -cnotmatch '^sha256:[0-9a-f]{64}$' -or
        [string]$result.redis_topology -cnotin @('standalone', 'sentinel', 'cluster') -or
        @($result.PSObject.Properties).Count -ne 3) {
        throw 'Redis source/raw 与 sanitized RO config normalized target 不一致。'
    }
    if ((Get-PodUIDConfigCompareSourceSHA256) -cne $sourceSHA256) {
        throw 'Redis config compare 执行期间 release source 漂移。'
    }
    return [pscustomobject][ordered]@{
        Identity = [string]$result.redis_config_identity
        Topology = [string]$result.redis_topology
        HelperSourceSHA256 = $sourceSHA256
    }
}

function New-PodUIDConfigEvidence($Source, $RawSnapshot, $ROSecret,
    $RedisConfigContract, [string]$RedisTargetIdentity = 'pending-prepare') {
    return [pscustomobject][ordered]@{
        SourceSecretName = [string]$Source.Name
        SourceSecretUID = [string]$Source.UID
        SourceSecretResourceVersion = [string]$Source.ResourceVersion
        RawConfigSHA256 = [string]$Source.ConfigSHA256
        RawSnapshotName = [string]$RawSnapshot.Name
        RawSnapshotUID = [string]$RawSnapshot.UID
        RawSnapshotResourceVersion = [string]$RawSnapshot.ResourceVersion
        RawSnapshotSHA256 = [string]$RawSnapshot.ConfigSHA256
        ROSecretName = [string]$ROSecret.Name
        ROSecretUID = [string]$ROSecret.UID
        ROSecretResourceVersion = [string]$ROSecret.ResourceVersion
        ROConfigSHA256 = [string]$ROSecret.ConfigSHA256
        RedisConfigIdentity = [string]$RedisConfigContract.Identity
        RedisConfigTopology = [string]$RedisConfigContract.Topology
        HelperSourceSHA256 = [string]$RedisConfigContract.HelperSourceSHA256
        RedisTargetIdentity = $RedisTargetIdentity
    }
}

function Assert-PodUIDConfigEvidenceCurrent($Evidence, [bool]$RequireLiveSource) {
    if ($RequireLiveSource -and
        (Get-PodUIDConfigCompareSourceSHA256) -cne [string]$Evidence.HelperSourceSHA256) {
        throw 'activation 窗口内 pod_uid config compare release source 已漂移。'
    }
    $source = [pscustomobject]@{
        Name = [string]$Evidence.SourceSecretName
        UID = [string]$Evidence.SourceSecretUID
        ResourceVersion = [string]$Evidence.SourceSecretResourceVersion
        ConfigSHA256 = [string]$Evidence.RawConfigSHA256
    }
    if ($RequireLiveSource) {
        $live = Get-LivePandoraConfigSourceEvidence
        if ([string]$live.Name -cne [string]$source.Name -or [string]$live.UID -cne [string]$source.UID -or
            [string]$live.ResourceVersion -cne [string]$source.ResourceVersion -or
            [string]$live.ConfigSHA256 -cne [string]$source.ConfigSHA256) {
            throw 'activation 窗口内 Secret/pandora-config UID/resourceVersion/ds-allocator raw bytes 已变化。'
        }
    }
    $raw = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get',
        "secret/$($Evidence.RawSnapshotName)", '-o', 'json')
    $rawEvidence = Assert-RawPodUIDConfigSnapshot $raw $source
    if ([string]$rawEvidence.UID -cne [string]$Evidence.RawSnapshotUID -or
        [string]$rawEvidence.ResourceVersion -cne [string]$Evidence.RawSnapshotResourceVersion) {
        throw 'immutable raw config snapshot UID/resourceVersion 与 activation evidence 不一致。'
    }
    $ro = Get-PodUIDPreflightROSecretEvidence
    if ([string]$ro.Name -cne [string]$Evidence.ROSecretName -or
        [string]$ro.UID -cne [string]$Evidence.ROSecretUID -or
        [string]$ro.ResourceVersion -cne [string]$Evidence.ROSecretResourceVersion -or
        [string]$ro.ConfigSHA256 -cne [string]$Evidence.ROConfigSHA256) {
        throw 'immutable RO preflight Secret 与 activation evidence 不一致。'
    }
}

function Assert-RedisTopologyChangeLockProvider($ConfigEvidence, [string]$Checkpoint) {
    if ([string]$ConfigEvidence.RedisConfigTopology -cnotin @('standalone', 'sentinel', 'cluster') -or
        [string]$ConfigEvidence.RedisConfigIdentity -cnotmatch '^sha256:[0-9a-f]{64}$' -or
        $Checkpoint -cnotin @('before-prepare', 'before-cas')) {
        throw 'Redis topology-change lock gate 收到非 canonical activation evidence。'
    }
    # 本仓库没有 Redis/云厂商控制平面的锁 API、信任根或可验证 lease。
    # ConfigMap 只能记录证据，不能阻止 failover/reshard/Redis 8.4 atomic slot
    # migration，因此绝不能把自签 marker 当执行锁。所有 topology 先 fail closed；
    # 将来接入 provider 后，必须在 prepare 前取得、在各阶段和 CAS 前在线续验，
    # 并在 CAS 后释放/回读。
    Assert-PandoraRedisTopologyChangeLockProvider $Checkpoint `
        ([string]$ConfigEvidence.RedisConfigTopology) ([string]$ConfigEvidence.RedisConfigIdentity)
}

function Assert-PodUIDACLCleanupAdminInputs {
    foreach ($name in @($RedisACLAdminUsernameEnv, $RedisACLAdminPasswordEnv, $RedisPasswordEnv)) {
        if ([string]$name -cnotmatch '^[A-Z][A-Z0-9_]{2,127}$') {
            throw 'Redis ACL cleanup credential env 名称必须为 canonical 大写标识符。'
        }
    }
    if ($RedisACLAdminUsernameEnv -ceq $RedisACLAdminPasswordEnv -or
        $RedisACLAdminUsernameEnv -ceq $RedisPasswordEnv -or
        $RedisACLAdminPasswordEnv -ceq $RedisPasswordEnv) {
        throw 'Redis ACL cleanup admin username/password/runtime env 名称必须两两不同。'
    }
    $adminUsername = [Environment]::GetEnvironmentVariable($RedisACLAdminUsernameEnv, 'Process')
    $adminPassword = [Environment]::GetEnvironmentVariable($RedisACLAdminPasswordEnv, 'Process')
    $runtimePassword = [Environment]::GetEnvironmentVariable($RedisPasswordEnv, 'Process')
    if ([string]$adminUsername -cnotmatch '^[A-Za-z0-9][A-Za-z0-9._-]{2,63}$' -or
        [string]$adminUsername -cin @('default', $PodUIDPreflightACLUser) -or
        [string]::IsNullOrEmpty($adminPassword) -or [string]$adminPassword.Length -lt 32 -or
        [string]::IsNullOrEmpty($runtimePassword) -or $adminPassword -ceq $runtimePassword) {
        throw 'Redis ACL cleanup control-plane identity 缺失、非独立或不满足最小 secret contract。'
    }
}

function Get-PodUIDACLCleanupRequiredData($ActivationEvidence) {
    return [ordered]@{
        contract = 'pod-uid-acl-cleanup-required-v1'
        run_id = $ActivationRunId
        target_epoch = [string]$TargetEpoch
        target_required_value = $TargetRequiredValue
        required_policy_id = $RequiredPolicyV2
        evidence_sha256 = [string]$ActivationEvidence.EvidenceSHA256
        method = 'DELUSER'
        target_user = $PodUIDPreflightACLUser
        redis_config_identity = [string]$ActivationEvidence.Config.RedisConfigIdentity
        redis_topology = [string]$ActivationEvidence.Config.RedisConfigTopology
        helper_source_sha256 = [string]$ActivationEvidence.Config.HelperSourceSHA256
        ro_secret_uid = [string]$ActivationEvidence.Config.ROSecretUID
        ro_secret_resource_version = [string]$ActivationEvidence.Config.ROSecretResourceVersion
    }
}

function Assert-PodUIDACLCleanupRequiredMarker($Marker, $ActivationEvidence) {
    $expected = Get-PodUIDACLCleanupRequiredData $ActivationEvidence
    if ([string]$Marker.apiVersion -cne 'v1' -or [string]$Marker.kind -cne 'ConfigMap' -or
        [string]$Marker.metadata.name -cne $PodUIDACLCleanupRequiredMarkerName -or
        [string]$Marker.metadata.namespace -cne $PandoraNamespace -or $Marker.immutable -ne $true -or
        [string]$Marker.metadata.uid -cnotmatch '^[0-9a-f][0-9a-f-]{7,127}$' -or
        [string]$Marker.metadata.resourceVersion -cnotmatch '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$' -or
        (Test-PandoraKubernetesObjectDeleting $Marker)) {
        throw 'pod_uid ACL cleanup required marker 身份/immutable/deletion 非 canonical。'
    }
    if ((@($Marker.data.PSObject.Properties.Name | Sort-Object) -join ',') -cne
        (@($expected.Keys | Sort-Object) -join ',')) {
        throw 'pod_uid ACL cleanup required marker data keys 非 exact contract。'
    }
    foreach ($entry in $expected.GetEnumerator()) {
        if ([string]$Marker.data.$($entry.Key) -cne [string]$entry.Value) {
            throw "pod_uid ACL cleanup required marker field=$($entry.Key) 漂移。"
        }
    }
    return $Marker
}

function Ensure-PodUIDACLCleanupRequiredMarker($ActivationEvidence, [bool]$AllowCreate) {
    $marker = Get-OptionalKubectlJson @('-n', $PandoraNamespace, 'get',
        "configmap/$PodUIDACLCleanupRequiredMarkerName", '--ignore-not-found', '-o', 'json')
    if ($null -eq $marker) {
        if (-not $AllowCreate) {
            throw 'pod_uid ACL cleanup required marker 缺失且已越过 switch；禁止事后补造。'
        }
        $object = [ordered]@{
            apiVersion = 'v1'; kind = 'ConfigMap'; immutable = $true
            metadata = [ordered]@{
                name = $PodUIDACLCleanupRequiredMarkerName; namespace = $PandoraNamespace
                labels = [ordered]@{
                    'pandora.dev/ds-auth-activation' = [string]$TargetEpoch
                    'pandora.dev/activation-run-id' = $ActivationRunId
                    'pandora.dev/post-cas-cleanup' = 'required'
                }
            }
            data = Get-PodUIDACLCleanupRequiredData $ActivationEvidence
        }
        Invoke-KubectlCreateObject $object
        $marker = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get',
            "configmap/$PodUIDACLCleanupRequiredMarkerName", '-o', 'json')
    }
    return Assert-PodUIDACLCleanupRequiredMarker $marker $ActivationEvidence
}

function Assert-PodUIDACLCleanupCompleteMarker($Marker, $RequiredMarker, $ActivationEvidence, $SwitchMarker) {
    $requiredCreated = [datetimeoffset]::Parse([string]$RequiredMarker.metadata.creationTimestamp)
    $switchCreated = [datetimeoffset]::Parse([string]$SwitchMarker.metadata.creationTimestamp)
    $completedAt = [int64]0
    $expected = [ordered]@{
        contract = 'pod-uid-acl-cleanup-complete-v1'
        run_id = $ActivationRunId
        target_epoch = [string]$TargetEpoch
        target_required_value = $TargetRequiredValue
        required_policy_id = $RequiredPolicyV2
        evidence_sha256 = [string]$ActivationEvidence.EvidenceSHA256
        required_marker_uid = [string]$RequiredMarker.metadata.uid
        required_marker_resource_version = [string]$RequiredMarker.metadata.resourceVersion
        method = 'DELUSER'
        target_user = $PodUIDPreflightACLUser
        redis_config_identity = [string]$ActivationEvidence.Config.RedisConfigIdentity
        redis_topology = [string]$ActivationEvidence.Config.RedisConfigTopology
        helper_source_sha256 = [string]$ActivationEvidence.Config.HelperSourceSHA256
    }
    if ([string]$Marker.apiVersion -cne 'v1' -or [string]$Marker.kind -cne 'ConfigMap' -or
        [string]$Marker.metadata.name -cne $PodUIDACLCleanupCompleteMarkerName -or
        [string]$Marker.metadata.namespace -cne $PandoraNamespace -or $Marker.immutable -ne $true -or
        [string]$Marker.metadata.uid -cnotmatch '^[0-9a-f][0-9a-f-]{7,127}$' -or
        [string]$Marker.metadata.resourceVersion -cnotmatch '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$' -or
        [string]$Marker.data.proof_sha256 -cnotmatch '^sha256:[0-9a-f]{64}$' -or
        [string]$Marker.data.visited_nodes -cnotmatch '^[1-9][0-9]*$' -or
        [string]$Marker.data.completed_at_unix_ms -cnotmatch '^[1-9][0-9]{12}$' -or
        -not [int64]::TryParse([string]$Marker.data.completed_at_unix_ms, [ref]$completedAt) -or
        (Test-PandoraKubernetesObjectDeleting $Marker)) {
        throw 'pod_uid ACL cleanup complete marker 身份/proof/immutable 非 canonical。'
    }
    $expectedKeys = @($expected.Keys) + @('proof_sha256', 'visited_nodes', 'completed_at_unix_ms')
    if ((@($Marker.data.PSObject.Properties.Name | Sort-Object) -join ',') -cne
        (@($expectedKeys | Sort-Object) -join ',')) {
        throw 'pod_uid ACL cleanup complete marker data keys 非 exact contract。'
    }
    foreach ($entry in $expected.GetEnumerator()) {
        if ([string]$Marker.data.$($entry.Key) -cne [string]$entry.Value) {
            throw "pod_uid ACL cleanup complete marker field=$($entry.Key) 漂移。"
        }
    }
    $markerCreated = [datetimeoffset]::Parse([string]$Marker.metadata.creationTimestamp)
    $proofCompleted = [datetimeoffset]::FromUnixTimeMilliseconds($completedAt)
    Assert-PandoraPodUIDACLCleanupTimeline $requiredCreated $switchCreated $proofCompleted $markerCreated
    return $Marker
}

function Get-OptionalPodUIDACLCleanupCompleteMarker($RequiredMarker, $ActivationEvidence, $SwitchMarker) {
    $marker = Get-OptionalKubectlJson @('-n', $PandoraNamespace, 'get',
        "configmap/$PodUIDACLCleanupCompleteMarkerName", '--ignore-not-found', '-o', 'json')
    if ($null -eq $marker) { return $null }
    return Assert-PodUIDACLCleanupCompleteMarker $marker $RequiredMarker $ActivationEvidence $SwitchMarker
}

function Invoke-PodUIDACLCleanup($ActivationEvidence) {
    Assert-PodUIDACLCleanupAdminInputs
    if ((Get-PodUIDConfigCompareSourceSHA256) -cne [string]$ActivationEvidence.Config.HelperSourceSHA256) {
        throw 'pod_uid ACL cleanup pinned release source 已漂移。'
    }
    $secret = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get',
        "secret/$PodUIDPreflightRedisSecretName", '-o', 'json')
    $ro = Get-PodUIDPreflightROSecretEvidence
    if ([string]$secret.metadata.uid -cne [string]$ro.UID -or
        [string]$secret.metadata.resourceVersion -cne [string]$ro.ResourceVersion -or
        [string]$secret.data.'ds-allocator-preflight.yaml' -cne [string]$ro.EncodedConfig -or
        [string]$ro.UID -cne [string]$ActivationEvidence.Config.ROSecretUID -or
        [string]$ro.ResourceVersion -cne [string]$ActivationEvidence.Config.ROSecretResourceVersion -or
        [string]$ro.ConfigSHA256 -cne [string]$ActivationEvidence.Config.ROConfigSHA256) {
        throw 'pod_uid ACL cleanup RO Secret 与 original activation evidence 不一致。'
    }
    $passwordProperty = $secret.data.PSObject.Properties['password']
    if ($null -eq $passwordProperty -or [string]::IsNullOrWhiteSpace([string]$passwordProperty.Value)) {
        throw 'pod_uid ACL cleanup RO Secret 缺 credential comparison input。'
    }
    $inputJSON = ([ordered]@{
        read_only_config_base64 = [string]$ro.EncodedConfig
        preflight_password_base64 = [string]$passwordProperty.Value
    } | ConvertTo-Json -Compress)
    $sourceSHA256 = Get-PodUIDConfigCompareSourceSHA256
    $oldGoProxy = $env:GOPROXY
    $oldGoSumDB = $env:GOSUMDB
    $oldGoToolchain = $env:GOTOOLCHAIN
    $env:GOPROXY = 'off'; $env:GOSUMDB = 'off'; $env:GOTOOLCHAIN = 'local'
    Push-Location (Join-Path $ProjectRoot 'services/battle/ds_allocator')
    try {
        $output = @($inputJSON | & go run -mod=readonly ./cmd/pod_uid_acl_cleanup `
            --admin-username-env $RedisACLAdminUsernameEnv `
            --admin-password-env $RedisACLAdminPasswordEnv `
            --runtime-password-env $RedisPasswordEnv `
            --expected-config-identity ([string]$ActivationEvidence.Config.RedisConfigIdentity) `
            --expected-topology ([string]$ActivationEvidence.Config.RedisConfigTopology) 2>&1)
        $cleanupExit = $LASTEXITCODE
    } finally {
        Pop-Location
        if ($null -eq $oldGoProxy) { Remove-Item Env:GOPROXY -ErrorAction SilentlyContinue } else { $env:GOPROXY = $oldGoProxy }
        if ($null -eq $oldGoSumDB) { Remove-Item Env:GOSUMDB -ErrorAction SilentlyContinue } else { $env:GOSUMDB = $oldGoSumDB }
        if ($null -eq $oldGoToolchain) { Remove-Item Env:GOTOOLCHAIN -ErrorAction SilentlyContinue } else { $env:GOTOOLCHAIN = $oldGoToolchain }
    }
    if ($cleanupExit -ne 0) { throw 'temporary Redis ACL cleanup command failed closed（credential/output 未回显）。' }
    $identityPattern = [regex]::Escape([string]$ActivationEvidence.Config.RedisConfigIdentity)
    $topologyPattern = [regex]::Escape([string]$ActivationEvidence.Config.RedisConfigTopology)
    $pattern = "^pod_uid preflight ACL cleanup PASSED: method=DELUSER target_user=$PodUIDPreflightACLUser redis_config_identity=$identityPattern redis_topology=$topologyPattern visited_nodes=([1-9][0-9]*) state=absent`r?$"
    if ($output.Count -ne 1 -or [string]$output[0] -cnotmatch $pattern) {
        throw 'temporary Redis ACL cleanup 缺唯一 exact PASSED/readback proof。'
    }
    if ((Get-PodUIDConfigCompareSourceSHA256) -cne $sourceSHA256) {
        throw 'temporary Redis ACL cleanup 执行期间 pinned release source 漂移。'
    }
    return [pscustomobject][ordered]@{
        ProofSHA256 = Get-SHA256DigestFromBytes ([Text.Encoding]::UTF8.GetBytes([string]$output[0]))
        VisitedNodes = [string]$Matches[1]
        CompletedAtUnixMS = [datetimeoffset]::UtcNow.ToUnixTimeMilliseconds()
    }
}

function Ensure-PodUIDACLCleanupCompleteMarker($RequiredMarker, $ActivationEvidence, $SwitchMarker, $CleanupResult) {
    $marker = Get-OptionalPodUIDACLCleanupCompleteMarker $RequiredMarker $ActivationEvidence $SwitchMarker
    if ($null -ne $marker) { return $marker }
    if ($null -eq $CleanupResult) { throw 'pod_uid ACL cleanup completion proof 缺失。' }
    $data = [ordered]@{
        contract = 'pod-uid-acl-cleanup-complete-v1'; run_id = $ActivationRunId
        target_epoch = [string]$TargetEpoch; evidence_sha256 = [string]$ActivationEvidence.EvidenceSHA256
        target_required_value = $TargetRequiredValue; required_policy_id = $RequiredPolicyV2
        required_marker_uid = [string]$RequiredMarker.metadata.uid
        required_marker_resource_version = [string]$RequiredMarker.metadata.resourceVersion
        method = 'DELUSER'; target_user = $PodUIDPreflightACLUser
        redis_config_identity = [string]$ActivationEvidence.Config.RedisConfigIdentity
        redis_topology = [string]$ActivationEvidence.Config.RedisConfigTopology
        helper_source_sha256 = [string]$ActivationEvidence.Config.HelperSourceSHA256
        proof_sha256 = [string]$CleanupResult.ProofSHA256
        visited_nodes = [string]$CleanupResult.VisitedNodes
        completed_at_unix_ms = [string]$CleanupResult.CompletedAtUnixMS
    }
    $object = [ordered]@{
        apiVersion = 'v1'; kind = 'ConfigMap'; immutable = $true
        metadata = [ordered]@{
            name = $PodUIDACLCleanupCompleteMarkerName; namespace = $PandoraNamespace
            labels = [ordered]@{
                'pandora.dev/ds-auth-activation' = [string]$TargetEpoch
                'pandora.dev/activation-run-id' = $ActivationRunId
                'pandora.dev/post-cas-cleanup' = 'complete'
            }
        }
        data = $data
    }
    Invoke-KubectlCreateObject $object
    $marker = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get',
        "configmap/$PodUIDACLCleanupCompleteMarkerName", '-o', 'json')
    return Assert-PodUIDACLCleanupCompleteMarker $marker $RequiredMarker $ActivationEvidence $SwitchMarker
}

function Complete-PodUIDACLPostCASCleanup($ActivationEvidence, $SwitchMarker) {
    $required = Ensure-PodUIDACLCleanupRequiredMarker $ActivationEvidence $false
    $requiredAt = [datetimeoffset]::Parse([string]$required.metadata.creationTimestamp)
    $switchAt = [datetimeoffset]::Parse([string]$SwitchMarker.metadata.creationTimestamp)
    if ($requiredAt -gt $switchAt) {
        throw 'pod_uid ACL cleanup required marker 未在线性化于 switch 之前。'
    }
    $complete = Get-OptionalPodUIDACLCleanupCompleteMarker $required $ActivationEvidence $SwitchMarker
    if ($null -ne $complete) { return $complete }
    try {
        $result = Invoke-PodUIDACLCleanup $ActivationEvidence
        return Ensure-PodUIDACLCleanupCompleteMarker $required $ActivationEvidence $SwitchMarker $result
    } catch {
        throw ('required_writer_epoch 已完成 CAS，但临时 Redis ACL cleanup 仍为 PENDING；' +
            '请使用相同 ActivationRunId 和 Phase=Activate、独立 admin env 重试。')
    }
}

function Assert-PodUIDACLPostCASCleanupComplete($ActivationEvidence, $SwitchMarker) {
    $required = Ensure-PodUIDACLCleanupRequiredMarker $ActivationEvidence $false
    $requiredAt = [datetimeoffset]::Parse([string]$required.metadata.creationTimestamp)
    $switchAt = [datetimeoffset]::Parse([string]$SwitchMarker.metadata.creationTimestamp)
    if ($requiredAt -gt $switchAt) {
        throw 'pod_uid ACL cleanup required marker 未在线性化于 switch 之前。'
    }
    $complete = Get-OptionalPodUIDACLCleanupCompleteMarker $required $ActivationEvidence $SwitchMarker
    if ($null -eq $complete) {
        throw ('required_writer_epoch=2 但临时 Redis ACL cleanup=PENDING；' +
            'Audit 保持只读，请使用相同 ActivationRunId 和 Phase=Activate 完成幂等 cleanup。')
    }
    return $complete
}

function Set-DsAllocatorGreenRawConfigSnapshot($Evidence) {
    $deploymentName = 'ds-allocator-ds-auth-green'
    $deployment = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', "deployment/$deploymentName", '-o', 'json')
    $volumes = @($deployment.spec.template.spec.volumes)
    $indexes = @()
    for ($index = 0; $index -lt $volumes.Count; $index++) {
        if ([string]$volumes[$index].name -ceq 'conf') { $indexes += $index }
    }
    if ($indexes.Count -ne 1) { throw "$deploymentName 必须有唯一 conf volume。" }
    $volumeIndex = [int]$indexes[0]
    $currentName = [string]$volumes[$volumeIndex].secret.secretName
    if ($currentName -cnotin @('pandora-config', [string]$Evidence.RawSnapshotName)) {
        throw "$deploymentName conf Secret 已漂移到未审计对象。"
    }
    if ($currentName -cne [string]$Evidence.RawSnapshotName) {
        if ([int]$deployment.spec.replicas -ne 0) {
            throw "$deploymentName 只有 replicas=0 时才能首次绑定 activation raw config snapshot。"
        }
        $patch = @(
            [ordered]@{ op = 'test'; path = '/metadata/resourceVersion'; value = [string]$deployment.metadata.resourceVersion }
            [ordered]@{ op = 'test'; path = "/spec/template/spec/volumes/$volumeIndex/name"; value = 'conf' }
            [ordered]@{ op = 'test'; path = "/spec/template/spec/volumes/$volumeIndex/secret/secretName"; value = 'pandora-config' }
            [ordered]@{ op = 'replace'; path = "/spec/template/spec/volumes/$volumeIndex/secret/secretName"; value = [string]$Evidence.RawSnapshotName }
        ) | ConvertTo-Json -Depth 12 -Compress
        Invoke-Kubectl @('-n', $PandoraNamespace, 'patch', "deployment/$deploymentName", '--type=json', '-p', $patch) | Out-Null
    }
    $deployment = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', "deployment/$deploymentName", '-o', 'json')
    $conf = @($deployment.spec.template.spec.volumes | Where-Object { [string]$_.name -ceq 'conf' })
    if ($conf.Count -ne 1 -or
        [string]$conf[0].secret.secretName -cne [string]$Evidence.RawSnapshotName) {
        throw "$deploymentName raw config snapshot 终态回读失败。"
    }
    Assert-PodUIDConfigEvidenceCurrent $Evidence $true
}

function Assert-DsAllocatorGreenRawConfigSnapshot($Evidence) {
    $deployment = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get',
        'deployment/ds-allocator-ds-auth-green', '-o', 'json')
    $conf = @($deployment.spec.template.spec.volumes | Where-Object { [string]$_.name -ceq 'conf' })
    if ($conf.Count -ne 1 -or [string]$conf[0].secret.secretName -cne [string]$Evidence.RawSnapshotName) {
        throw 'ds-allocator green 未绑定本次 immutable raw config snapshot。'
    }
    Assert-PodUIDConfigEvidenceCurrent $Evidence $true
}

function Get-PodUIDPhaseEvidenceDigest($PhaseEvidence, $ConfigEvidence) {
    $material = [ordered]@{
        version = 'pod-uid-preflight-evidence-v1'
        run_id = $ActivationRunId
        target_epoch = [string]$TargetEpoch
        target_required_value = $TargetRequiredValue
        required_policy_id = $RequiredPolicyV2
        phase = [string]$PhaseEvidence.Phase
        job_name = [string]$PhaseEvidence.JobName
        job_uid = [string]$PhaseEvidence.JobUID
        pod_name = [string]$PhaseEvidence.PodName
        pod_uid = [string]$PhaseEvidence.PodUID
        completion_time = [string]$PhaseEvidence.CompletionTime
        completion_time_unix_ms = [string]$PhaseEvidence.CompletionTimeUnixMS
        image_digest = [string]$PhaseEvidence.ImageDigest
        image_id = [string]$PhaseEvidence.ImageID
        log_sha256 = [string]$PhaseEvidence.LogSHA256
        redis_target_identity = [string]$PhaseEvidence.RedisTargetIdentity
        redis_topology = [string]$PhaseEvidence.RedisTopology
        redis_acl_user = [string]$PhaseEvidence.RedisACLUser
        source_secret_uid = [string]$ConfigEvidence.SourceSecretUID
        source_secret_resource_version = [string]$ConfigEvidence.SourceSecretResourceVersion
        raw_config_sha256 = [string]$ConfigEvidence.RawConfigSHA256
        raw_snapshot_name = [string]$ConfigEvidence.RawSnapshotName
        raw_snapshot_uid = [string]$ConfigEvidence.RawSnapshotUID
        raw_snapshot_resource_version = [string]$ConfigEvidence.RawSnapshotResourceVersion
        ro_secret_name = [string]$ConfigEvidence.ROSecretName
        ro_secret_uid = [string]$ConfigEvidence.ROSecretUID
        ro_secret_resource_version = [string]$ConfigEvidence.ROSecretResourceVersion
        ro_config_sha256 = [string]$ConfigEvidence.ROConfigSHA256
        redis_config_identity = [string]$ConfigEvidence.RedisConfigIdentity
        redis_config_topology = [string]$ConfigEvidence.RedisConfigTopology
        config_helper_source_sha256 = [string]$ConfigEvidence.HelperSourceSHA256
    }
    return Get-SHA256DigestFromBytes ([Text.Encoding]::UTF8.GetBytes(
        ($material | ConvertTo-Json -Depth 8 -Compress)))
}

function Get-PodUIDActivationEvidenceData($PrepareEvidence, $DrainedEvidence, $FinalEvidence,
    $ConfigEvidence) {
    $prepareDigest = Get-PodUIDPhaseEvidenceDigest $PrepareEvidence $ConfigEvidence
    $drainedDigest = Get-PodUIDPhaseEvidenceDigest $DrainedEvidence $ConfigEvidence
    $finalDigest = Get-PodUIDPhaseEvidenceDigest $FinalEvidence $ConfigEvidence
    $material = [ordered]@{
        contract = 'pod-uid-activation-evidence-v1'
        run_id = $ActivationRunId
        target_epoch = [string]$TargetEpoch
        target_required_value = $TargetRequiredValue
        required_policy_id = $RequiredPolicyV2
        source_secret_uid = [string]$ConfigEvidence.SourceSecretUID
        source_secret_resource_version = [string]$ConfigEvidence.SourceSecretResourceVersion
        raw_config_sha256 = [string]$ConfigEvidence.RawConfigSHA256
        raw_snapshot_name = [string]$ConfigEvidence.RawSnapshotName
        raw_snapshot_uid = [string]$ConfigEvidence.RawSnapshotUID
        raw_snapshot_resource_version = [string]$ConfigEvidence.RawSnapshotResourceVersion
        ro_secret_name = [string]$ConfigEvidence.ROSecretName
        ro_secret_uid = [string]$ConfigEvidence.ROSecretUID
        ro_secret_resource_version = [string]$ConfigEvidence.ROSecretResourceVersion
        ro_config_sha256 = [string]$ConfigEvidence.ROConfigSHA256
        redis_config_identity = [string]$ConfigEvidence.RedisConfigIdentity
        redis_config_topology = [string]$ConfigEvidence.RedisConfigTopology
        config_helper_source_sha256 = [string]$ConfigEvidence.HelperSourceSHA256
        redis_target_identity = [string]$FinalEvidence.RedisTargetIdentity
        prepare_evidence_sha256 = $prepareDigest
        drained_evidence_sha256 = $drainedDigest
        final_evidence_sha256 = $finalDigest
        final_job_uid = [string]$FinalEvidence.JobUID
        final_pod_uid = [string]$FinalEvidence.PodUID
        final_completion_time = [string]$FinalEvidence.CompletionTime
        final_completion_time_unix_ms = [string]$FinalEvidence.CompletionTimeUnixMS
    }
    $digest = Get-SHA256DigestFromBytes ([Text.Encoding]::UTF8.GetBytes(
        ($material | ConvertTo-Json -Depth 8 -Compress)))
    $data = [ordered]@{}
    foreach ($entry in $material.GetEnumerator()) { $data[$entry.Key] = [string]$entry.Value }
    $data['evidence_sha256'] = $digest
    return [pscustomobject]$data
}

function Assert-PodUIDActivationEvidenceMarker($Marker, $PrepareEvidence, $DrainedEvidence,
    $FinalEvidence, $ConfigEvidence) {
    $expected = Get-PodUIDActivationEvidenceData $PrepareEvidence $DrainedEvidence $FinalEvidence $ConfigEvidence
    if ([string]$Marker.apiVersion -cne 'v1' -or [string]$Marker.kind -cne 'ConfigMap' -or
        [string]$Marker.metadata.name -cne $PodUIDEvidenceMarkerName -or
        [string]$Marker.metadata.namespace -cne $PandoraNamespace -or $Marker.immutable -ne $true -or
        (Test-PandoraKubernetesObjectDeleting $Marker)) {
        throw 'pod_uid activation evidence marker 身份/immutable/deletion 非 canonical。'
    }
    $actualKeys = @($Marker.data.PSObject.Properties.Name | Sort-Object)
    $expectedKeys = @($expected.PSObject.Properties.Name | Sort-Object)
    if (($actualKeys -join ',') -cne ($expectedKeys -join ',')) {
        throw 'pod_uid activation evidence marker data keys 非 exact contract。'
    }
    foreach ($key in $expectedKeys) {
        if ([string]$Marker.data.$key -cne [string]$expected.$key) {
            throw "pod_uid activation evidence marker field=$key 漂移。"
        }
    }
    return $expected
}

function Ensure-PodUIDActivationEvidenceMarker($PrepareEvidence, $DrainedEvidence,
    $FinalEvidence, $ConfigEvidence) {
    $expectedData = Get-PodUIDActivationEvidenceData $PrepareEvidence $DrainedEvidence $FinalEvidence $ConfigEvidence
    $marker = Get-OptionalKubectlJson @('-n', $PandoraNamespace, 'get',
        "configmap/$PodUIDEvidenceMarkerName", '--ignore-not-found', '-o', 'json')
    if ($null -eq $marker) {
        $object = [ordered]@{
            apiVersion = 'v1'; kind = 'ConfigMap'; immutable = $true
            metadata = [ordered]@{
                name = $PodUIDEvidenceMarkerName; namespace = $PandoraNamespace
                labels = [ordered]@{
                    'pandora.dev/ds-auth-activation' = [string]$TargetEpoch
                    'pandora.dev/activation-run-id' = $ActivationRunId
                    'pandora.dev/activation-evidence' = 'pod-uid-v1'
                }
            }
            data = [ordered]@{}
        }
        foreach ($entry in $expectedData.PSObject.Properties) {
            $object.data[$entry.Name] = [string]$entry.Value
        }
        Invoke-KubectlCreateObject $object
        $marker = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get',
            "configmap/$PodUIDEvidenceMarkerName", '-o', 'json')
    }
    $null = Assert-PodUIDActivationEvidenceMarker $marker $PrepareEvidence $DrainedEvidence $FinalEvidence $ConfigEvidence
    return $marker
}

function Get-PodUIDConfigEvidenceFromMarker($Marker) {
    return [pscustomobject][ordered]@{
        SourceSecretName = 'pandora-config'
        SourceSecretUID = [string]$Marker.data.source_secret_uid
        SourceSecretResourceVersion = [string]$Marker.data.source_secret_resource_version
        RawConfigSHA256 = [string]$Marker.data.raw_config_sha256
        RawSnapshotName = [string]$Marker.data.raw_snapshot_name
        RawSnapshotUID = [string]$Marker.data.raw_snapshot_uid
        RawSnapshotResourceVersion = [string]$Marker.data.raw_snapshot_resource_version
        RawSnapshotSHA256 = [string]$Marker.data.raw_config_sha256
        ROSecretName = [string]$Marker.data.ro_secret_name
        ROSecretUID = [string]$Marker.data.ro_secret_uid
        ROSecretResourceVersion = [string]$Marker.data.ro_secret_resource_version
        ROConfigSHA256 = [string]$Marker.data.ro_config_sha256
        RedisConfigIdentity = [string]$Marker.data.redis_config_identity
        RedisConfigTopology = [string]$Marker.data.redis_config_topology
        HelperSourceSHA256 = [string]$Marker.data.config_helper_source_sha256
        RedisTargetIdentity = [string]$Marker.data.redis_target_identity
    }
}

function Get-PodUIDActivationEvidenceChain($LockMarker, $DrainMarker, $GreenReadyMarker,
    $SwitchMarker, [bool]$RequireLiveSource) {
    $marker = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get',
        "configmap/$PodUIDEvidenceMarkerName", '-o', 'json')
    if ([string]$marker.metadata.name -cne $PodUIDEvidenceMarkerName -or $marker.immutable -ne $true -or
        (Test-PandoraKubernetesObjectDeleting $marker)) {
        throw 'pod_uid activation evidence marker 不存在、可变或正在删除。'
    }
    $configEvidence = Get-PodUIDConfigEvidenceFromMarker $marker
    Assert-PodUIDConfigEvidenceCurrent $configEvidence $RequireLiveSource
    $lockAt = [datetimeoffset]::Parse([string]$LockMarker.metadata.creationTimestamp)
    $drainAt = [datetimeoffset]::Parse([string]$DrainMarker.metadata.creationTimestamp)
    $greenReadyAt = [datetimeoffset]::Parse([string]$GreenReadyMarker.metadata.creationTimestamp)
    $switchAt = [datetimeoffset]::Parse([string]$SwitchMarker.metadata.creationTimestamp)
    if ($lockAt -gt $drainAt -or $drainAt -gt $greenReadyAt -or $greenReadyAt -gt $switchAt) {
        throw 'pod_uid activation marker 时间链 lock<=drain<=green-ready<=switch 非法。'
    }
    $prepareConfig = (($configEvidence | ConvertTo-Json -Depth 10) | ConvertFrom-Json)
    $prepareConfig.RedisTargetIdentity = 'pending-prepare'
    $prepare = Assert-ExistingPodUIDReleasePreflightJob 'prepare' `
        ([datetimeoffset]::MinValue) $lockAt $prepareConfig $RequireLiveSource
    if ([string]$prepare.RedisTargetIdentity -cne [string]$configEvidence.RedisTargetIdentity) {
        throw 'prepare Redis target identity 与不可变 activation evidence 不一致。'
    }
    $drained = Assert-ExistingPodUIDReleasePreflightJob 'drained' `
        $drainAt $greenReadyAt $configEvidence $RequireLiveSource
    $final = Assert-ExistingPodUIDReleasePreflightJob 'final' `
        $greenReadyAt $switchAt $configEvidence $RequireLiveSource
    if ([string]$drained.RedisTargetIdentity -cne [string]$prepare.RedisTargetIdentity -or
        [string]$final.RedisTargetIdentity -cne [string]$prepare.RedisTargetIdentity) {
        throw 'prepare/drained/final Redis target identity 不一致。'
    }
    $expected = Assert-PodUIDActivationEvidenceMarker $marker $prepare $drained $final $configEvidence
    $markerAt = [datetimeoffset]::Parse([string]$marker.metadata.creationTimestamp)
    $finalCompleted = [datetimeoffset]::Parse([string]$final.CompletionTime)
    if ($markerAt -lt $finalCompleted -or $markerAt -gt $switchAt) {
        throw 'pod_uid immutable evidence marker 未在线性 final<=evidence<=switch 窗口创建。'
    }
    return [pscustomobject][ordered]@{
        Config = $configEvidence; Prepare = $prepare; Drained = $drained; Final = $final
        EvidenceSHA256 = [string]$expected.evidence_sha256
        FinalCompletionTimeUnixMS = [int64]$final.CompletionTimeUnixMS
    }
}

function Assert-ActivationMarker($Object, [string]$Name, [string]$Kind, $ConfigEvidence = $null) {
    $createdAt = [datetimeoffset]::MinValue
    if ([string]$Object.apiVersion -cne 'v1' -or [string]$Object.kind -cne 'ConfigMap' -or
        [string]$Object.metadata.name -cne $Name -or
        [string]$Object.metadata.namespace -cne $PandoraNamespace -or
        [string]$Object.metadata.uid -cnotmatch '^[0-9a-f][0-9a-f-]{7,127}$' -or
        [string]$Object.metadata.resourceVersion -cnotmatch '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$' -or
        -not [datetimeoffset]::TryParse([string]$Object.metadata.creationTimestamp, [ref]$createdAt) -or
        (Test-PandoraKubernetesObjectDeleting $Object) -or $Object.immutable -ne $true -or
        [string]$Object.data.run_id -cne $ActivationRunId -or [string]$Object.data.evidence_sha256 -cne (Get-ReleaseEvidenceHash) -or
        [string]$Object.data.expected_epoch -cne [string]$ExpectedEpoch -or [string]$Object.data.target_epoch -cne [string]$TargetEpoch -or
        [string]$Object.data.expected_required_value -cne $ExpectedRequiredValue -or
        [string]$Object.data.target_required_value -cne $TargetRequiredValue -or
        [string]$Object.data.required_policy_id -cne $RequiredPolicyV2) {
        throw "$Kind/$Name 与本次不可变激活身份不一致。"
    }
    if ($null -ne $ConfigEvidence) {
        $expected = [ordered]@{
            pod_uid_source_secret_uid = [string]$ConfigEvidence.SourceSecretUID
            pod_uid_source_secret_resource_version = [string]$ConfigEvidence.SourceSecretResourceVersion
            pod_uid_raw_config_sha256 = [string]$ConfigEvidence.RawConfigSHA256
            pod_uid_raw_snapshot_name = [string]$ConfigEvidence.RawSnapshotName
            pod_uid_raw_snapshot_uid = [string]$ConfigEvidence.RawSnapshotUID
            pod_uid_raw_snapshot_resource_version = [string]$ConfigEvidence.RawSnapshotResourceVersion
            pod_uid_ro_secret_name = [string]$ConfigEvidence.ROSecretName
            pod_uid_ro_secret_uid = [string]$ConfigEvidence.ROSecretUID
            pod_uid_ro_secret_resource_version = [string]$ConfigEvidence.ROSecretResourceVersion
            pod_uid_ro_config_sha256 = [string]$ConfigEvidence.ROConfigSHA256
            pod_uid_redis_config_identity = [string]$ConfigEvidence.RedisConfigIdentity
            pod_uid_redis_config_topology = [string]$ConfigEvidence.RedisConfigTopology
            pod_uid_config_helper_source_sha256 = [string]$ConfigEvidence.HelperSourceSHA256
        }
        foreach ($entry in $expected.GetEnumerator()) {
            if ([string]$Object.data.$($entry.Key) -cne [string]$entry.Value) {
                throw "$Kind/$Name 的 pod_uid config binding field=$($entry.Key) 漂移。"
            }
        }
        $expectedKeys = @('run_id', 'evidence_sha256', 'expected_epoch', 'target_epoch',
            'expected_required_value', 'target_required_value', 'required_policy_id') +
            @($expected.Keys)
        $actualKeys = @($Object.data.PSObject.Properties.Name | Sort-Object)
        if (($actualKeys -join ',') -cne (@($expectedKeys | Sort-Object) -join ',')) {
            throw "$Kind/$Name data keys 非 exact activation contract。"
        }
    }
}

function Get-EarliestLiveGreenPodCreationTime {
    $times = [System.Collections.Generic.List[datetimeoffset]]::new()
    foreach ($app in $Apps) {
        $pods = @( (Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', 'pods',
            '-l', "app=$app,$WriterSetLabel=green", '-o', 'json')).items )
        foreach ($pod in $pods) {
            $createdAt = [datetimeoffset]::MinValue
            if ((Test-PandoraKubernetesObjectDeleting $pod) -or
                -not [datetimeoffset]::TryParse([string]$pod.metadata.creationTimestamp, [ref]$createdAt)) {
                throw 'green Pod creationTimestamp 缺失或 Pod 正在删除。'
            }
            $times.Add($createdAt)
        }
    }
    if ($times.Count -eq 0) { throw '无 live green Pod，不能建立 drained evidence 上界。' }
    return @($times | Sort-Object)[0]
}

function Ensure-ActivationMarker([string]$Name, [string]$Kind, $ConfigEvidence = $null) {
    $object = Get-OptionalKubectlJson @('-n', $PandoraNamespace, 'get', "configmap/$Name", '--ignore-not-found', '-o', 'json')
    if ($null -eq $object) {
        $objectToCreate = [ordered]@{
            apiVersion = 'v1'; kind = 'ConfigMap'
            metadata = [ordered]@{ name = $Name; namespace = $PandoraNamespace; labels = [ordered]@{ 'pandora.dev/ds-auth-activation' = [string]$TargetEpoch } }
            immutable = $true
            data = [ordered]@{
                run_id = $ActivationRunId; evidence_sha256 = (Get-ReleaseEvidenceHash)
                expected_epoch = [string]$ExpectedEpoch; target_epoch = [string]$TargetEpoch
                expected_required_value = $ExpectedRequiredValue
                target_required_value = $TargetRequiredValue
                required_policy_id = $RequiredPolicyV2
            }
        }
        if ($null -ne $ConfigEvidence) {
            $objectToCreate.data['pod_uid_source_secret_uid'] = [string]$ConfigEvidence.SourceSecretUID
            $objectToCreate.data['pod_uid_source_secret_resource_version'] = [string]$ConfigEvidence.SourceSecretResourceVersion
            $objectToCreate.data['pod_uid_raw_config_sha256'] = [string]$ConfigEvidence.RawConfigSHA256
            $objectToCreate.data['pod_uid_raw_snapshot_name'] = [string]$ConfigEvidence.RawSnapshotName
            $objectToCreate.data['pod_uid_raw_snapshot_uid'] = [string]$ConfigEvidence.RawSnapshotUID
            $objectToCreate.data['pod_uid_raw_snapshot_resource_version'] = [string]$ConfigEvidence.RawSnapshotResourceVersion
            $objectToCreate.data['pod_uid_ro_secret_name'] = [string]$ConfigEvidence.ROSecretName
            $objectToCreate.data['pod_uid_ro_secret_uid'] = [string]$ConfigEvidence.ROSecretUID
            $objectToCreate.data['pod_uid_ro_secret_resource_version'] = [string]$ConfigEvidence.ROSecretResourceVersion
            $objectToCreate.data['pod_uid_ro_config_sha256'] = [string]$ConfigEvidence.ROConfigSHA256
            $objectToCreate.data['pod_uid_redis_config_identity'] = [string]$ConfigEvidence.RedisConfigIdentity
            $objectToCreate.data['pod_uid_redis_config_topology'] = [string]$ConfigEvidence.RedisConfigTopology
            $objectToCreate.data['pod_uid_config_helper_source_sha256'] = [string]$ConfigEvidence.HelperSourceSHA256
        }
        Invoke-KubectlCreateObject $objectToCreate
        $object = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', "configmap/$Name", '-o', 'json')
    }
    Assert-ActivationMarker $object $Name $Kind $ConfigEvidence
    return $object
}

function Get-ExistingActivationMarker([string]$Name, [string]$Kind, $ConfigEvidence = $null) {
    $object = Get-OptionalKubectlJson @('-n', $PandoraNamespace, 'get', "configmap/$Name", '--ignore-not-found', '-o', 'json')
    if ($null -eq $object) { throw "$Kind/$Name 不存在。" }
    Assert-ActivationMarker $object $Name $Kind $ConfigEvidence
    return $object
}

function Set-RequiredNamespaceLabels {
    foreach ($namespaceName in @($PandoraNamespace, $DSNamespace)) {
        $namespace = Invoke-KubectlJson @('get', "namespace/$namespaceName", '-o', 'json')
        $current = [string]$namespace.metadata.labels.$RequiredEpochLabel
        if ($current -and $current -notin @([string]$ExpectedEpoch, [string]$TargetEpoch)) { throw "namespace/$namespaceName required epoch=$current 非法。" }
        if ($current -cne [string]$TargetEpoch) {
            $patch = @{ metadata = @{ resourceVersion = [string]$namespace.metadata.resourceVersion; labels = @{ $RequiredEpochLabel = [string]$TargetEpoch } } } | ConvertTo-Json -Compress
            Invoke-Kubectl @('patch', "namespace/$namespaceName", '--type=merge', '-p', $patch) | Out-Null
        }
    }
}

function Switch-LiveServicesToGreen {
    foreach ($app in $Apps) {
        $service = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', "service/$app", '-o', 'json')
        $track = [string]$service.spec.selector.$WriterSetLabel
        if ([string]$service.spec.selector.app -cne $app -or $track -notin @('blue', 'green') -or
            @($service.spec.selector.PSObject.Properties).Count -ne 3) {
            throw "service/$app 未处于 exact app+blue/green+epoch 轨道。"
        }
        $currentEpoch = if ($track -ceq 'green') { $TargetEpoch } else { $ExpectedEpoch }
        if ([string]$service.spec.selector.$WriterEpochLabel -cne [string]$currentEpoch) {
            throw "service/$app 当前 writer epoch selector 不匹配。"
        }
        if ($track -ceq 'blue') {
            # RFC6902 replace 整张 selector map；test resourceVersion 提供 CAS。merge patch 会
            # 保留未知 selector，可能把流量同时导向未审计 Pod，禁止使用。
            $selector = [ordered]@{
                app = $app
                $WriterSetLabel = 'green'
                $WriterEpochLabel = [string]$TargetEpoch
            }
            $patch = @(
                [ordered]@{ op = 'test'; path = '/metadata/resourceVersion'; value = [string]$service.metadata.resourceVersion }
                [ordered]@{ op = 'replace'; path = '/spec/selector'; value = $selector }
            ) | ConvertTo-Json -Depth 10 -Compress
            Invoke-Kubectl @('-n', $PandoraNamespace, 'patch', "service/$app", '--type=json', '-p', $patch) | Out-Null
        }
    }
    Assert-LiveServiceTrack green
}

function Scale-BlueDeploymentsToZero {
    foreach ($app in $Apps) {
        $deployment = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', "deployment/$app", '-o', 'json')
        if ([string]$deployment.spec.template.metadata.labels.$WriterSetLabel -cne 'blue' -or
            [string]$deployment.spec.template.metadata.labels.$WriterEpochLabel -cne [string]$ExpectedEpoch) {
            throw "deployment/$app 不是 epoch=$ExpectedEpoch blue writer。"
        }
        if ([int]$deployment.spec.replicas -ne 0) {
            $patch = @{ metadata = @{ resourceVersion = [string]$deployment.metadata.resourceVersion }; spec = @{ replicas = 0 } } | ConvertTo-Json -Compress
            Invoke-Kubectl @('-n', $PandoraNamespace, 'patch', "deployment/$app", '--type=merge', '-p', $patch) | Out-Null
        }
    }
    $deadline = [datetime]::UtcNow.AddMinutes(3)
    do {
        try { Assert-BlueDeployments $true; return }
        catch {
            if ([datetime]::UtcNow -ge $deadline) { throw }
            Start-Sleep -Seconds 2
        }
    } while ($true)
}

function Scale-GreenDeploymentsToDesired($Candidate) {
    foreach ($app in $Apps) {
        $deploymentName = "$app-ds-auth-green"
        $deployment = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', "deployment/$deploymentName", '-o', 'json')
        $desired = [int]$Candidate.DesiredReplicas[$app]
        if ([int]$deployment.spec.replicas -notin @(0, $desired)) { throw "$deploymentName replicas 漂移。" }
        if ([int]$deployment.spec.replicas -eq 0) {
            $patch = @{ metadata = @{ resourceVersion = [string]$deployment.metadata.resourceVersion }; spec = @{ replicas = $desired } } | ConvertTo-Json -Compress
            Invoke-Kubectl @('-n', $PandoraNamespace, 'patch', "deployment/$deploymentName", '--type=merge', '-p', $patch) | Out-Null
        }
    }
    $deadline = [datetime]::UtcNow.AddMinutes(3)
    do {
        try { return Get-GreenBackendContract live }
        catch {
            if ([datetime]::UtcNow -ge $deadline) { throw }
            Start-Sleep -Seconds 2
        }
    } while ($true)
}

function Invoke-PodUIDReleasePreflightJobGate(
    [ValidateSet('prepare', 'drained', 'final')][string]$PreflightPhase,
    [bool]$AllowCreate, [bool]$WaitForCompletion, [datetimeoffset]$NotBefore,
    [datetimeoffset]$NotAfter, $ConfigEvidence, [bool]$RequireLiveSource) {
    # This is the first mutating action of a fresh Activate run. The one-shot
    # prepare uses the exact dormant green image/config and exits before service
    # wiring; it assumes this additive binary already ran as epoch=1 blue in an
    # earlier rollout and had a chance to backfill. The Job is audit-only and
    # never repairs missing K8s history. A failed prepare scan leaves blue
    # serving and creates no activation lock/namespace label/green writer.
    $green = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get',
        'deployment/ds-allocator-ds-auth-green', '-o', 'json')
    Assert-PodUIDConfigEvidenceCurrent $ConfigEvidence $RequireLiveSource
    $expected = New-PandoraPodUIDReleasePreflightJobObject $green $ActivationRunId `
        $PreflightPhase $TargetEpoch $ConfigEvidence
    $jobName = [string]$expected.metadata.name
    $expectedImage = [string]$expected.spec.template.spec.containers[0].image
    $job = Get-OptionalKubectlJson @('-n', $PandoraNamespace, 'get', "job/$jobName",
        '--ignore-not-found', '-o', 'json')
    if ($null -eq $job -and -not $AllowCreate) {
        throw "Model-B pod_uid release preflight Job/$jobName 不存在；已激活终态禁止事后创建证据。"
    }
    if ($null -eq $job) {
        Invoke-KubectlCreateObject $expected
        $job = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', "job/$jobName", '-o', 'json')
    }
    Assert-PandoraPodUIDReleasePreflightJobContract $job $expectedImage $ActivationRunId `
        $PreflightPhase $TargetEpoch $ConfigEvidence
    $created = [datetimeoffset]::MinValue
    if (-not [datetimeoffset]::TryParse([string]$job.metadata.creationTimestamp, [ref]$created) -or
        $created -lt $NotBefore -or $created -gt $NotAfter) {
        throw "Model-B pod_uid $PreflightPhase Job creationTimestamp 越过 activation 阶段窗口。"
    }

    $deadline = [datetime]::UtcNow.AddMinutes(12)
    do {
        $job = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', "job/$jobName", '-o', 'json')
        Assert-PodUIDConfigEvidenceCurrent $ConfigEvidence $RequireLiveSource
        Assert-PandoraPodUIDReleasePreflightJobContract $job $expectedImage $ActivationRunId `
            $PreflightPhase $TargetEpoch $ConfigEvidence
        $conditions = @()
        if ($null -ne $job.PSObject.Properties['status'] -and
            $null -ne $job.status.PSObject.Properties['conditions']) {
            $conditions = @($job.status.conditions)
        }
        $failed = @($conditions | Where-Object { [string]$_.type -ceq 'Failed' -and [string]$_.status -ceq 'True' })
        $complete = @($conditions | Where-Object { [string]$_.type -ceq 'Complete' -and [string]$_.status -ceq 'True' })
        $pods = @( (Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', 'pods',
            '-l', "job-name=$jobName", '-o', 'json')).items )
        if ($failed.Count -ne 0) {
            $logs = if ($pods.Count -eq 1) {
                (Invoke-Kubectl @('-n', $PandoraNamespace, 'logs', "pod/$($pods[0].metadata.name)",
                    '-c', 'pod-uid-release-preflight')) -join "`n"
            } else { '[exact failed Pod unavailable]' }
            throw "Model-B pod_uid release preflight Job failed closed:`n$logs"
        }
        if ($complete.Count -eq 1) {
            if ($pods.Count -ne 1) { throw "Model-B pod_uid Job/$jobName 必须且只能有一个 exact Pod。" }
            $runtime = Assert-PandoraPodUIDReleasePreflightRuntimeContract $job $pods[0] `
                $expectedImage $ActivationRunId $PreflightPhase $TargetEpoch $ConfigEvidence $NotBefore $NotAfter
            $logs = (Invoke-Kubectl @('-n', $PandoraNamespace, 'logs', "pod/$($pods[0].metadata.name)",
                '-c', 'pod-uid-release-preflight')) -join "`n"
            $runPattern = [regex]::Escape($ActivationRunId)
            $phasePattern = [regex]::Escape($PreflightPhase)
            $digestPattern = [regex]::Escape(([string]$job.metadata.annotations.'pandora.dev/image-digest'))
            $configIdentityPattern = [regex]::Escape([string]$ConfigEvidence.RedisConfigIdentity)
            $passPattern = "(?m)^pod_uid release preflight PASSED: run_id=$runPattern phase=$phasePattern image_digest=$digestPattern redis_config_identity=($configIdentityPattern) redis_target_identity=(sha256:[0-9a-f]{64}) redis_topology=(standalone|sentinel|cluster) redis_acl_user=(pandora-pod-uid-release-preflight-ro) visited_masters=[1-9][0-9]* visited_keys=[0-9]+ decoded_records=[0-9]+ allocation_uncertain=[0-9]+ findings=0; no data was modified\r?$"
            $matches = [regex]::Matches($logs, $passPattern)
            if ($matches.Count -ne 1) {
                throw 'Model-B pod_uid release preflight Job Complete 但缺 exact PASSED/findings=0 证据。'
            }
            $targetIdentity = [string]$matches[0].Groups[2].Value
            if ($PreflightPhase -cne 'prepare' -and
                $targetIdentity -cne [string]$ConfigEvidence.RedisTargetIdentity) {
                throw "Model-B pod_uid $PreflightPhase Redis target identity 与 prepare 证据不一致。"
            }
            if ([string]$matches[0].Groups[3].Value -cne [string]$ConfigEvidence.RedisConfigTopology) {
                throw "Model-B pod_uid $PreflightPhase Redis runtime topology 与 normalized config 不一致。"
            }
            Assert-PodUIDConfigEvidenceCurrent $ConfigEvidence $RequireLiveSource
            $runtime | Add-Member -NotePropertyName Phase -NotePropertyValue $PreflightPhase
            $runtime | Add-Member -NotePropertyName LogSHA256 -NotePropertyValue `
                (Get-SHA256DigestFromBytes ([Text.Encoding]::UTF8.GetBytes($logs)))
            $runtime | Add-Member -NotePropertyName RedisTargetIdentity -NotePropertyValue $targetIdentity
            $runtime | Add-Member -NotePropertyName RedisConfigIdentity -NotePropertyValue ([string]$matches[0].Groups[1].Value)
            $runtime | Add-Member -NotePropertyName RedisTopology -NotePropertyValue ([string]$matches[0].Groups[3].Value)
            $runtime | Add-Member -NotePropertyName RedisACLUser -NotePropertyValue ([string]$matches[0].Groups[4].Value)
            Write-Host '[ OK ] Model-B legacy pod_uid 全 master 只读发布门通过。' -ForegroundColor Green
            return $runtime
        }
        if (-not $WaitForCompletion) {
            throw "Model-B pod_uid release preflight Job/$jobName 尚无 Complete+PASSED 证据。"
        }
        if ([datetime]::UtcNow -ge $deadline) {
            throw 'Model-B pod_uid release preflight Job 等待超时；activation 未开始。'
        }
        Start-Sleep -Seconds 2
    } while ($true)
}

function Ensure-PodUIDReleasePreflightJob {
    param($ConfigEvidence)
    return Invoke-PodUIDReleasePreflightJobGate 'prepare' $true $true `
        ([datetimeoffset]::MinValue) ([datetimeoffset]::MaxValue) $ConfigEvidence $true
}

function Ensure-DrainedPodUIDReleasePreflightJob([datetimeoffset]$NotBefore, $ConfigEvidence) {
    return Invoke-PodUIDReleasePreflightJobGate 'drained' $true $true $NotBefore `
        ([datetimeoffset]::MaxValue) $ConfigEvidence $true
}

function Ensure-FinalPodUIDReleasePreflightJob([datetimeoffset]$NotBefore, $ConfigEvidence) {
    return Invoke-PodUIDReleasePreflightJobGate 'final' $true $true $NotBefore `
        ([datetimeoffset]::MaxValue) $ConfigEvidence $true
}

function Assert-ExistingPodUIDReleasePreflightJob(
    [ValidateSet('prepare', 'drained', 'final')][string]$PreflightPhase,
    [datetimeoffset]$NotBefore, [datetimeoffset]$NotAfter, $ConfigEvidence,
    [bool]$RequireLiveSource) {
    # required_writer_epoch=2 is already irreversible. Audit/idempotent runs
    # may only read the original same-RunId evidence; creating a new Job after
    # activation would fabricate a retroactive release proof.
    return Invoke-PodUIDReleasePreflightJobGate $PreflightPhase $false $false `
        $NotBefore $NotAfter $ConfigEvidence $RequireLiveSource
}

if (-not (Get-Command kubectl -ErrorAction SilentlyContinue)) { throw 'kubectl 不可用。' }
if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw 'go 不可用。' }
if ($ExpectedEpoch -ne 1 -or $TargetEpoch -ne 2) { throw '当前工具只允许一次性单调 1→2 激活。' }
if ($ActivationRunId -cnotmatch '^[a-z0-9][a-z0-9-]{7,23}$') { throw 'ActivationRunId 必须为 8..24 位小写字母/数字/连字符。' }
if ([string]::IsNullOrWhiteSpace($KeysetRevision)) { throw 'KeysetRevision 不能为空。' }
$null = Assert-PandoraDsAuthHttpsEndpoints $EtcdEndpoints
Assert-PandoraDsAuthEtcdRevision $EtcdIdentityRevision
Assert-PandoraDsAuthEtcdRevision $PodUIDPreflightRedisSecretRevision
foreach ($digest in $AllowedImageDigests) { Assert-Digest $digest 'allowed-image-digests' }
Assert-Digest $SyntheticProbeImageDigest 'synthetic probe'
$null = Get-SecureGoArgs

Push-Location $ProjectRoot
try {
    $snapshot = Get-RequiredSnapshot
    if ($Phase -ceq 'Audit') {
        if ([uint32]$snapshot.epoch -eq $ExpectedEpoch) {
            Write-Warning ('RELEASE BLOCKER: 当前仓库未接入可执行 Redis topology-change lock provider；' +
                'Audit 继续只读检查，但任何 topology 的 Activate 都会在 prepare 前 fail closed。')
            $candidate = Get-GreenBackendContract zero
            Assert-LiveServiceTrack blue
            Assert-BlueDeployments $false
            Assert-TerminalMeshPolicy
            Assert-GameServers
            Invoke-RedisAudit
            Get-SyntheticEvidence prepare ([datetimeoffset]::MinValue)
        } elseif ([uint32]$snapshot.epoch -eq $TargetEpoch) {
            $audit = Get-GreenBackendContract live
            Assert-LiveServiceTrack green
            Assert-BlueDeployments $true
            Assert-TerminalMeshPolicy
            Assert-GameServers
            $lockMarker = Get-ExistingActivationMarker $ActivationLockName 'activation lock'
            $drainMarker = Get-ExistingActivationMarker $DrainMarkerName 'drain marker'
            $greenReadyMarker = Get-ExistingActivationMarker $GreenReadyMarkerName 'green-ready marker'
            $switchMarker = Get-ExistingActivationMarker $SwitchMarkerName 'switch marker'
            $activationEvidence = Get-PodUIDActivationEvidenceChain $lockMarker $drainMarker `
                $greenReadyMarker $switchMarker $false
            $null = Get-ExistingActivationMarker $ActivationLockName 'activation lock' $activationEvidence.Config
            $null = Get-ExistingActivationMarker $DrainMarkerName 'drain marker' $activationEvidence.Config
            $null = Get-ExistingActivationMarker $GreenReadyMarkerName 'green-ready marker' $activationEvidence.Config
            $null = Get-ExistingActivationMarker $SwitchMarkerName 'switch marker' $activationEvidence.Config
            $null = Assert-PodUIDACLPostCASCleanupComplete $activationEvidence $switchMarker
            Invoke-CapabilityAudit $audit -ActivationEvidence $activationEvidence
            Invoke-RedisAudit
            Get-SyntheticEvidence final ([datetimeoffset]::Parse([string]$switchMarker.metadata.creationTimestamp))
        } else { throw "required_writer_epoch=$($snapshot.epoch) 非法。" }
        throw ('AUDIT RESULT=BLOCKED: 可执行 Redis topology-change lock provider 未接线；' +
            '已完成其余只读审计，但不能把当前状态声明为可发布。')
    }

    if ([uint32]$snapshot.epoch -eq $TargetEpoch) {
        $audit = Get-GreenBackendContract live
        Assert-LiveServiceTrack green
        Assert-BlueDeployments $true
        Assert-TerminalMeshPolicy
        Assert-GameServers
        $lockMarker = Get-ExistingActivationMarker $ActivationLockName 'activation lock'
        $drainMarker = Get-ExistingActivationMarker $DrainMarkerName 'drain marker'
        $greenReadyMarker = Get-ExistingActivationMarker $GreenReadyMarkerName 'green-ready marker'
        $switchMarker = Get-ExistingActivationMarker $SwitchMarkerName 'switch marker'
        $activationEvidence = Get-PodUIDActivationEvidenceChain $lockMarker $drainMarker `
            $greenReadyMarker $switchMarker $false
        $null = Get-ExistingActivationMarker $ActivationLockName 'activation lock' $activationEvidence.Config
        $null = Get-ExistingActivationMarker $DrainMarkerName 'drain marker' $activationEvidence.Config
        $null = Get-ExistingActivationMarker $GreenReadyMarkerName 'green-ready marker' $activationEvidence.Config
        $null = Get-ExistingActivationMarker $SwitchMarkerName 'switch marker' $activationEvidence.Config
        Invoke-CapabilityAudit $audit -ActivationEvidence $activationEvidence
        Invoke-RedisAudit
        Get-SyntheticEvidence final ([datetimeoffset]::Parse([string]$switchMarker.metadata.creationTimestamp))
        $null = Complete-PodUIDACLPostCASCleanup $activationEvidence $switchMarker
        throw ('RECOVERY COMPLETE, RELEASE STILL BLOCKED: required_writer_epoch 已是 2，临时 Redis ACL 用户已幂等删除；' +
            '但该历史 activation 未绑定可验证 topology-change lock provider proof，禁止声明完整终态。')
    }
    if ([uint32]$snapshot.epoch -ne $ExpectedEpoch) { throw "required_writer_epoch=$($snapshot.epoch) 不能激活。" }

    $existingLock = Get-OptionalKubectlJson @('-n', $PandoraNamespace, 'get', "configmap/$ActivationLockName", '--ignore-not-found', '-o', 'json')
    if ($null -eq $existingLock) {
        $candidate = Get-GreenBackendContract zero
        Assert-LiveServiceTrack blue
        Assert-BlueDeployments $false
        $sourceEvidence = Get-LivePandoraConfigSourceEvidence
        $roSecretEvidence = Get-PodUIDPreflightROSecretEvidence
        $redisConfigContract = Invoke-PodUIDRedisConfigIdentityCompare $sourceEvidence $roSecretEvidence
        $topologyGateEvidence = [pscustomobject][ordered]@{
            RedisConfigIdentity = [string]$redisConfigContract.Identity
            RedisConfigTopology = [string]$redisConfigContract.Topology
        }
        # This must precede the first create/patch/scale/marker operation. With
        # no real provider wired, Activate exits here with zero external writes.
        Assert-RedisTopologyChangeLockProvider $topologyGateEvidence 'before-prepare'
        $rawSnapshotEvidence = Ensure-RawPodUIDConfigSnapshot $sourceEvidence
        $prepareConfigEvidence = New-PodUIDConfigEvidence $sourceEvidence $rawSnapshotEvidence `
            $roSecretEvidence $redisConfigContract
    } else {
        $prepareConfigEvidence = [pscustomobject][ordered]@{
            SourceSecretName = 'pandora-config'
            SourceSecretUID = [string]$existingLock.data.pod_uid_source_secret_uid
            SourceSecretResourceVersion = [string]$existingLock.data.pod_uid_source_secret_resource_version
            RawConfigSHA256 = [string]$existingLock.data.pod_uid_raw_config_sha256
            RawSnapshotName = [string]$existingLock.data.pod_uid_raw_snapshot_name
            RawSnapshotUID = [string]$existingLock.data.pod_uid_raw_snapshot_uid
            RawSnapshotResourceVersion = [string]$existingLock.data.pod_uid_raw_snapshot_resource_version
            RawSnapshotSHA256 = [string]$existingLock.data.pod_uid_raw_config_sha256
            ROSecretName = [string]$existingLock.data.pod_uid_ro_secret_name
            ROSecretUID = [string]$existingLock.data.pod_uid_ro_secret_uid
            ROSecretResourceVersion = [string]$existingLock.data.pod_uid_ro_secret_resource_version
            ROConfigSHA256 = [string]$existingLock.data.pod_uid_ro_config_sha256
            RedisConfigIdentity = [string]$existingLock.data.pod_uid_redis_config_identity
            RedisConfigTopology = [string]$existingLock.data.pod_uid_redis_config_topology
            HelperSourceSHA256 = [string]$existingLock.data.pod_uid_config_helper_source_sha256
            RedisTargetIdentity = 'pending-prepare'
        }
        Assert-ActivationMarker $existingLock $ActivationLockName 'activation lock' $prepareConfigEvidence
        Assert-PodUIDConfigEvidenceCurrent $prepareConfigEvidence $true
        $retrySourceEvidence = Get-LivePandoraConfigSourceEvidence
        $retryROSecretEvidence = Get-PodUIDPreflightROSecretEvidence
        $retryRedisConfigContract = Invoke-PodUIDRedisConfigIdentityCompare $retrySourceEvidence $retryROSecretEvidence
        if ([string]$retryRedisConfigContract.Identity -cne [string]$prepareConfigEvidence.RedisConfigIdentity -or
            [string]$retryRedisConfigContract.Topology -cne [string]$prepareConfigEvidence.RedisConfigTopology -or
            [string]$retryRedisConfigContract.HelperSourceSHA256 -cne [string]$prepareConfigEvidence.HelperSourceSHA256) {
            throw 'activation retry 的 normalized Redis config identity/topology 与 lock 不一致。'
        }
        $candidate = Get-GreenBackendContract either
        Assert-LiveServiceTrack either
        Assert-RedisTopologyChangeLockProvider $prepareConfigEvidence 'before-prepare'
    }
    Assert-TerminalMeshPolicy
    Assert-GameServers
    Invoke-RedisAudit
    Get-SyntheticEvidence prepare ([datetimeoffset]::MinValue)
    if ($null -eq $existingLock) {
        $prepareEvidence = Ensure-PodUIDReleasePreflightJob $prepareConfigEvidence
    } else {
        # activation lock 是 prepare 证据的不可逆上界。lock 一旦存在，丢失的
        # prepare Job 绝不允许事后同名补造，否则重试可能先 drain 再在末端失败。
        $lockNotAfter = [datetimeoffset]::Parse([string]$existingLock.metadata.creationTimestamp)
        $prepareEvidence = Assert-ExistingPodUIDReleasePreflightJob 'prepare' `
            ([datetimeoffset]::MinValue) $lockNotAfter $prepareConfigEvidence $true
    }
    $configEvidence = (($prepareConfigEvidence | ConvertTo-Json -Depth 10) | ConvertFrom-Json)
    $configEvidence.RedisTargetIdentity = [string]$prepareEvidence.RedisTargetIdentity
    if ($null -eq $existingLock) {
        $existingLock = Ensure-ActivationMarker $ActivationLockName 'activation lock' $configEvidence
    } else {
        Assert-ActivationMarker $existingLock $ActivationLockName 'activation lock' $configEvidence
    }

    # activation lock 已存在后，ordinary release 必须拒绝替换 pandora-config/green；
    # 当前 ds-allocator green 只绑定本次 immutable raw snapshot 后才允许继续 drain。
    Set-DsAllocatorGreenRawConfigSnapshot $configEvidence

    Set-RequiredNamespaceLabels
    $drainMarker = Get-OptionalKubectlJson @('-n', $PandoraNamespace, 'get', "configmap/$DrainMarkerName", '--ignore-not-found', '-o', 'json')
    $greenReadyBeforeDrain = Get-OptionalKubectlJson @('-n', $PandoraNamespace, 'get',
        "configmap/$GreenReadyMarkerName", '--ignore-not-found', '-o', 'json')
    $evidenceBeforeDrain = Get-OptionalKubectlJson @('-n', $PandoraNamespace, 'get',
        "configmap/$PodUIDEvidenceMarkerName", '--ignore-not-found', '-o', 'json')
    $switchBeforeDrain = Get-OptionalKubectlJson @('-n', $PandoraNamespace, 'get',
        "configmap/$SwitchMarkerName", '--ignore-not-found', '-o', 'json')
    if ($null -eq $drainMarker) {
        if ($null -ne $greenReadyBeforeDrain -or $null -ne $evidenceBeforeDrain -or
            $null -ne $switchBeforeDrain) {
            throw 'drain marker 缺失但存在下游 marker；禁止补造历史 drain 证据。'
        }
        # green 必须仍为 0；先停完 epoch=1 blue，再等所有 capability lease 消失，形成可审计零写空窗。
        $candidate = Get-GreenBackendContract zero
        Assert-LiveServiceTrack blue
        Scale-BlueDeploymentsToZero
        Invoke-EmptyCapabilityAudit $candidate
        $drainMarker = Ensure-ActivationMarker $DrainMarkerName 'drain marker' $configEvidence
    } else {
        Assert-ActivationMarker $drainMarker $DrainMarkerName 'drain marker' $configEvidence
        Assert-BlueDeployments $true
    }
    $drainNotBefore = [datetimeoffset]::Parse([string]$drainMarker.metadata.creationTimestamp)
    if ($null -ne $greenReadyBeforeDrain) {
        Assert-ActivationMarker $greenReadyBeforeDrain $GreenReadyMarkerName 'green-ready marker' $configEvidence
        $greenReadyUpperBound = [datetimeoffset]::Parse([string]$greenReadyBeforeDrain.metadata.creationTimestamp)
        $drainedEvidence = Assert-ExistingPodUIDReleasePreflightJob 'drained' `
            $drainNotBefore $greenReadyUpperBound $configEvidence $true
    } else {
        if ($null -ne $evidenceBeforeDrain -or $null -ne $switchBeforeDrain) {
            throw 'green-ready marker 缺失但存在下游 marker；禁止补造 drained 证据。'
        }
        $preScaleCandidate = Get-GreenBackendContract either
        if ([string]$preScaleCandidate.State -ceq 'zero') {
            $drainedEvidence = Ensure-DrainedPodUIDReleasePreflightJob $drainNotBefore $configEvidence
        } else {
            # 进程可在 scale green 后、green-ready marker 前崩溃。这时只能接受
            # 早于首个 live green Pod 创建的原 drained Job，不能事后新建。
            $firstGreenPodAt = Get-EarliestLiveGreenPodCreationTime
            $drainedEvidence = Assert-ExistingPodUIDReleasePreflightJob 'drained' `
                $drainNotBefore $firstGreenPodAt $configEvidence $true
        }
    }
    if ([string]$drainedEvidence.RedisTargetIdentity -cne [string]$prepareEvidence.RedisTargetIdentity) {
        throw 'drained Redis target identity 与 prepare 不一致。'
    }

    $candidate = Get-GreenBackendContract either
    $audit = Scale-GreenDeploymentsToDesired $candidate
    Assert-TerminalMeshPolicy
    Assert-BlueDeployments $true
    Assert-GameServers
    Invoke-CapabilityAudit $audit
    Invoke-RedisAudit
    Assert-DsAllocatorGreenRawConfigSnapshot $configEvidence
    $greenReadyMarker = Ensure-ActivationMarker $GreenReadyMarkerName 'green-ready marker' $configEvidence
    $greenReadyNotBefore = [datetimeoffset]::Parse([string]$greenReadyMarker.metadata.creationTimestamp)
    $evidenceBeforeFinal = Get-OptionalKubectlJson @('-n', $PandoraNamespace, 'get',
        "configmap/$PodUIDEvidenceMarkerName", '--ignore-not-found', '-o', 'json')
    $switchBeforeFinal = Get-OptionalKubectlJson @('-n', $PandoraNamespace, 'get',
        "configmap/$SwitchMarkerName", '--ignore-not-found', '-o', 'json')
    if ($null -ne $switchBeforeFinal -and $null -eq $evidenceBeforeFinal) {
        throw 'switch marker 已存在但 immutable evidence marker 缺失；禁止事后补造。'
    }
    if ($null -ne $evidenceBeforeFinal) {
        $evidenceNotAfter = [datetimeoffset]::Parse([string]$evidenceBeforeFinal.metadata.creationTimestamp)
        $finalEvidence = Assert-ExistingPodUIDReleasePreflightJob 'final' `
            $greenReadyNotBefore $evidenceNotAfter $configEvidence $true
    } else {
        # final/evidence 都必须在生产 Service 仍全部指向 blue 时建立。
        # 若已有 green/mixed Service，就无法证明新证据早于真实 switch。
        Assert-LiveServiceTrack blue
        $finalEvidence = Ensure-FinalPodUIDReleasePreflightJob $greenReadyNotBefore $configEvidence
    }
    if ([string]$finalEvidence.RedisTargetIdentity -cne [string]$prepareEvidence.RedisTargetIdentity) {
        throw 'final Redis target identity 与 prepare/drained 不一致。'
    }
    if ($null -eq $evidenceBeforeFinal) { Assert-LiveServiceTrack blue }
    $podUIDEvidenceMarker = Ensure-PodUIDActivationEvidenceMarker $prepareEvidence $drainedEvidence $finalEvidence $configEvidence
    Assert-PodUIDConfigEvidenceCurrent $configEvidence $true
    $preSwitchActivationEvidence = [pscustomobject][ordered]@{
        Config = $configEvidence
        EvidenceSHA256 = [string]$podUIDEvidenceMarker.data.evidence_sha256
        FinalCompletionTimeUnixMS = [int64]$finalEvidence.CompletionTimeUnixMS
    }
    $switchBeforeCleanupRequired = Get-OptionalKubectlJson @('-n', $PandoraNamespace, 'get',
        "configmap/$SwitchMarkerName", '--ignore-not-found', '-o', 'json')
    $null = Ensure-PodUIDACLCleanupRequiredMarker $preSwitchActivationEvidence `
        ($null -eq $switchBeforeCleanupRequired)
    Switch-LiveServicesToGreen
    $switchMarker = Ensure-ActivationMarker $SwitchMarkerName 'switch marker' $configEvidence
    $switchNotBefore = [datetimeoffset]::Parse([string]$switchMarker.metadata.creationTimestamp)

    # final synthetic 必须由固定外部探针在 switch marker 之后完成；若尚未运行，
    # 本次安全停在唯一 green + required=1，可用相同 RunId 重试，绝不拉回 epoch=1 blue。
    $audit = Get-GreenBackendContract live
    Assert-TerminalMeshPolicy
    Assert-LiveServiceTrack green
    Assert-BlueDeployments $true
    Assert-GameServers
    Invoke-CapabilityAudit $audit
    Invoke-RedisAudit
    Get-SyntheticEvidence final $switchNotBefore
    $lockMarker = Get-ExistingActivationMarker $ActivationLockName 'activation lock' $configEvidence
    $drainMarker = Get-ExistingActivationMarker $DrainMarkerName 'drain marker' $configEvidence
    $greenReadyMarker = Get-ExistingActivationMarker $GreenReadyMarkerName 'green-ready marker' $configEvidence
    $switchMarker = Get-ExistingActivationMarker $SwitchMarkerName 'switch marker' $configEvidence
    $activationEvidence = Get-PodUIDActivationEvidenceChain $lockMarker $drainMarker `
        $greenReadyMarker $switchMarker $true
    Assert-DsAllocatorGreenRawConfigSnapshot $configEvidence
    Assert-PodUIDACLCleanupAdminInputs
    Assert-RedisTopologyChangeLockProvider $activationEvidence.Config 'before-cas'
    Invoke-CapabilityAudit $audit -ApplyEpoch -ActivationEvidence $activationEvidence
    $null = Complete-PodUIDACLPostCASCleanup $activationEvidence $switchMarker
    Write-Host '[ OK ] DS auth required_writer_epoch 已通过 CAS 激活为 2；临时 Redis ACL 用户已删除并回读 absent。' -ForegroundColor Green
}
finally {
    Pop-Location
}
