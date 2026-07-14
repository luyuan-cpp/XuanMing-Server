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
    [ValidateSet('Audit', 'Activate')][string]$Phase = 'Audit',
    [string]$RedisPasswordEnv = 'PANDORA_REDIS_PASSWORD',
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

function Get-GoAuditArgs($Audit) {
    $args = @('run', './pkg/dsauthfence/cmd/dsauth-activate', '--endpoints', $EtcdEndpoints,
        '--prefix', $EtcdPrefix, '--expected-services', $Audit.Services, '--expected-instances', $Audit.Instances,
        '--expected-epoch', [string]$ExpectedEpoch, '--target-epoch', [string]$TargetEpoch,
        '--keyset-revision', $KeysetRevision, '--etcd-identity-revision', $EtcdIdentityRevision,
		'--allowed-image-digests', $Audit.Digests, '--expected-image-digests', $Audit.ServiceDigests,
		'--required-features', $script:PandoraDsAuthRequiredFeatures)
    $args += Get-SecureGoArgs
    return $args
}

function Get-RequiredSnapshot {
    $args = @('run', './pkg/dsauthfence/cmd/dsauth-required', '--endpoints', $EtcdEndpoints, '--prefix', $EtcdPrefix,
        '--min-epoch', [string]$ExpectedEpoch, '--max-epoch', [string]$TargetEpoch, '--output', 'json')
    $args += Get-SecureGoArgs
    $output = @(& go @args 2>&1)
    if ($LASTEXITCODE -ne 0) { throw "required_writer_epoch 只读检查失败:$($output -join [Environment]::NewLine)" }
    try { return (($output -join "`n") | ConvertFrom-Json -ErrorAction Stop) }
    catch { throw "required_writer_epoch 未返回机器 JSON:$($_.Exception.Message)" }
}

function Invoke-CapabilityAudit($Audit, [switch]$ApplyEpoch) {
    $args = @(Get-GoAuditArgs $Audit)
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
    $material += "|$($desired -join ',')|$KeysetRevision|$EtcdIdentityRevision|$ExpectedEpoch|$TargetEpoch|$SyntheticProbeImageDigest"
    $material += "|$EtcdEndpoints|$EtcdServerName|$EtcdClientIdentity|$EtcdForbiddenReadPrefix"
    $sha = [Security.Cryptography.SHA256]::Create()
    try { return ([Convert]::ToHexString($sha.ComputeHash([Text.Encoding]::UTF8.GetBytes($material)))).ToLowerInvariant() }
    finally { $sha.Dispose() }
}

function Assert-ActivationMarker($Object, [string]$Name, [string]$Kind) {
    if ([string]$Object.metadata.name -cne $Name -or $Object.immutable -ne $true -or
        [string]$Object.data.run_id -cne $ActivationRunId -or [string]$Object.data.evidence_sha256 -cne (Get-ReleaseEvidenceHash) -or
        [string]$Object.data.expected_epoch -cne [string]$ExpectedEpoch -or [string]$Object.data.target_epoch -cne [string]$TargetEpoch) {
        throw "$Kind/$Name 与本次不可变激活身份不一致。"
    }
}

function Ensure-ActivationMarker([string]$Name, [string]$Kind) {
    $object = Get-OptionalKubectlJson @('-n', $PandoraNamespace, 'get', "configmap/$Name", '--ignore-not-found', '-o', 'json')
    if ($null -eq $object) {
        $objectToCreate = [ordered]@{
            apiVersion = 'v1'; kind = 'ConfigMap'
            metadata = [ordered]@{ name = $Name; namespace = $PandoraNamespace; labels = [ordered]@{ 'pandora.dev/ds-auth-activation' = [string]$TargetEpoch } }
            immutable = $true
            data = [ordered]@{ run_id = $ActivationRunId; evidence_sha256 = (Get-ReleaseEvidenceHash); expected_epoch = [string]$ExpectedEpoch; target_epoch = [string]$TargetEpoch }
        }
        Invoke-KubectlCreateObject $objectToCreate
        $object = Invoke-KubectlJson @('-n', $PandoraNamespace, 'get', "configmap/$Name", '-o', 'json')
    }
    Assert-ActivationMarker $object $Name $Kind
    return $object
}

function Get-ExistingActivationMarker([string]$Name, [string]$Kind) {
    $object = Get-OptionalKubectlJson @('-n', $PandoraNamespace, 'get', "configmap/$Name", '--ignore-not-found', '-o', 'json')
    if ($null -eq $object) { throw "$Kind/$Name 不存在。" }
    Assert-ActivationMarker $object $Name $Kind
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

if (-not (Get-Command kubectl -ErrorAction SilentlyContinue)) { throw 'kubectl 不可用。' }
if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw 'go 不可用。' }
if ($ExpectedEpoch -ne 1 -or $TargetEpoch -ne 2) { throw '当前工具只允许一次性单调 1→2 激活。' }
if ($ActivationRunId -cnotmatch '^[a-z0-9][a-z0-9-]{7,23}$') { throw 'ActivationRunId 必须为 8..24 位小写字母/数字/连字符。' }
if ([string]::IsNullOrWhiteSpace($KeysetRevision)) { throw 'KeysetRevision 不能为空。' }
$null = Assert-PandoraDsAuthHttpsEndpoints $EtcdEndpoints
Assert-PandoraDsAuthEtcdRevision $EtcdIdentityRevision
foreach ($digest in $AllowedImageDigests) { Assert-Digest $digest 'allowed-image-digests' }
Assert-Digest $SyntheticProbeImageDigest 'synthetic probe'
$null = Get-SecureGoArgs

Push-Location $ProjectRoot
try {
    $snapshot = Get-RequiredSnapshot
    if ($Phase -ceq 'Audit') {
        if ([uint32]$snapshot.epoch -eq $ExpectedEpoch) {
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
            Invoke-CapabilityAudit $audit
            Invoke-RedisAudit
            $null = Get-ExistingActivationMarker $ActivationLockName 'activation lock'
            $switchMarker = Get-ExistingActivationMarker $SwitchMarkerName 'switch marker'
            Get-SyntheticEvidence final ([datetimeoffset]::Parse([string]$switchMarker.metadata.creationTimestamp))
        } else { throw "required_writer_epoch=$($snapshot.epoch) 非法。" }
        Write-Host '[AUDIT ONLY] 零重叠候选/终态、Secret/mTLS/feature/Redis/synthetic 证据通过；未修改集群或 etcd。' -ForegroundColor Yellow
        exit 0
    }

    if ([uint32]$snapshot.epoch -eq $TargetEpoch) {
        $audit = Get-GreenBackendContract live
        Assert-LiveServiceTrack green
        Assert-BlueDeployments $true
        Assert-TerminalMeshPolicy
        Assert-GameServers
        Invoke-CapabilityAudit $audit
        Invoke-RedisAudit
        $null = Get-ExistingActivationMarker $ActivationLockName 'activation lock'
        $switchMarker = Get-ExistingActivationMarker $SwitchMarkerName 'switch marker'
        Get-SyntheticEvidence final ([datetimeoffset]::Parse([string]$switchMarker.metadata.creationTimestamp))
        Write-Host '[ OK ] required_writer_epoch 已是 2；终态证据通过，未重复写。' -ForegroundColor Green
        exit 0
    }
    if ([uint32]$snapshot.epoch -ne $ExpectedEpoch) { throw "required_writer_epoch=$($snapshot.epoch) 不能激活。" }

    $existingLock = Get-OptionalKubectlJson @('-n', $PandoraNamespace, 'get', "configmap/$ActivationLockName", '--ignore-not-found', '-o', 'json')
    if ($null -eq $existingLock) {
        $candidate = Get-GreenBackendContract zero
        Assert-LiveServiceTrack blue
        Assert-BlueDeployments $false
    } else {
        Assert-ActivationMarker $existingLock $ActivationLockName 'activation lock'
        $candidate = Get-GreenBackendContract either
        Assert-LiveServiceTrack either
    }
    Assert-TerminalMeshPolicy
    Assert-GameServers
    Invoke-RedisAudit
    Get-SyntheticEvidence prepare ([datetimeoffset]::MinValue)
    if ($null -eq $existingLock) { $null = Ensure-ActivationMarker $ActivationLockName 'activation lock' }

    Set-RequiredNamespaceLabels
    $drainMarker = Get-OptionalKubectlJson @('-n', $PandoraNamespace, 'get', "configmap/$DrainMarkerName", '--ignore-not-found', '-o', 'json')
    if ($null -eq $drainMarker) {
        # green 必须仍为 0；先停完 epoch=1 blue，再等所有 capability lease 消失，形成可审计零写空窗。
        $candidate = Get-GreenBackendContract zero
        Assert-LiveServiceTrack blue
        Scale-BlueDeploymentsToZero
        Invoke-EmptyCapabilityAudit $candidate
        $drainMarker = Ensure-ActivationMarker $DrainMarkerName 'drain marker'
    } else {
        Assert-ActivationMarker $drainMarker $DrainMarkerName 'drain marker'
        Assert-BlueDeployments $true
    }

    $candidate = Get-GreenBackendContract either
    $audit = Scale-GreenDeploymentsToDesired $candidate
    Assert-TerminalMeshPolicy
    Assert-BlueDeployments $true
    Assert-GameServers
    Invoke-CapabilityAudit $audit
    Invoke-RedisAudit
    Switch-LiveServicesToGreen
    $switchMarker = Ensure-ActivationMarker $SwitchMarkerName 'switch marker'
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
    $null = Get-ExistingActivationMarker $ActivationLockName 'activation lock'
    $null = Get-ExistingActivationMarker $DrainMarkerName 'drain marker'
    $null = Get-ExistingActivationMarker $SwitchMarkerName 'switch marker'
    Invoke-CapabilityAudit $audit -ApplyEpoch
    Write-Host '[ OK ] DS auth required_writer_epoch 已通过 CAS 激活为 2；blue 已排空，禁止回退。' -ForegroundColor Green
}
finally {
    Pop-Location
}
