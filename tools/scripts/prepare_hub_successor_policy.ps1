# Stages the fixed five V3-capable DS-auth writers under required policy V2 and
# produces the create-only immutable evidence consumed by
# activate_hub_successor_policy.ps1. No Pod UID/digest is accepted from the
# operator: both are derived from canonical live Kubernetes objects.
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$KubeContext,
    [string]$Namespace = 'pandora',
    [Parameter(Mandatory = $true)][string]$ActivationRunID,
    [Parameter(Mandatory = $true)][string]$StageManifest,
    [Parameter(Mandatory = $true)][string]$EtcdEndpoints,
    [string]$EtcdPrefix = '/pandora/ds-auth/',
    [Parameter(Mandatory = $true)][string]$CAFile,
    [Parameter(Mandatory = $true)][string]$CertFile,
    [Parameter(Mandatory = $true)][string]$KeyFile,
    [Parameter(Mandatory = $true)][string]$ServerName,
    [Parameter(Mandatory = $true)][string]$ClientIdentity,
    [Parameter(Mandatory = $true)][string]$EtcdIdentityRevision,
    [string]$UsernameFile = '', [string]$PasswordFile = '',
    [Parameter(Mandatory = $true)][string]$ForbiddenReadPrefix,
    [timespan]$Timeout = ([timespan]::FromMinutes(5))
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$ProjectRoot = (Resolve-Path (Join-Path $PSScriptRoot '../..')).Path
. (Join-Path $PSScriptRoot 'lib/ds_auth_activation_contract.ps1')
if ($Namespace -cne 'pandora' -or $ActivationRunID -cnotmatch '^[a-z0-9][a-z0-9-]{7,31}$') {
    throw 'V3 prepare requires namespace=pandora and a canonical immutable run-id.'
}
if (-not (Get-Command kubectl -ErrorAction SilentlyContinue)) { throw 'kubectl is required.' }
if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw 'go is required.' }
$null = Assert-PandoraDsAuthHttpsEndpoints $EtcdEndpoints
$securityArgs = @(Get-PandoraDsAuthSecureGoArgs $CAFile $CertFile $KeyFile $ServerName `
    $ClientIdentity $EtcdIdentityRevision $ForbiddenReadPrefix $UsernameFile $PasswordFile)
$StageManifest = (Resolve-Path $StageManifest).Path
$writerSpecs = [ordered]@{
    login = [pscustomobject]@{ App = 'login'; Deployment = 'login-ds-auth-green'; Container = 'login' }
    player_locator = [pscustomobject]@{ App = 'player-locator'; Deployment = 'player-locator-ds-auth-green'; Container = 'player-locator' }
    ds_allocator = [pscustomobject]@{ App = 'ds-allocator'; Deployment = 'ds-allocator-ds-auth-green'; Container = 'ds-allocator' }
    hub_allocator = [pscustomobject]@{ App = 'hub-allocator'; Deployment = 'hub-allocator-ds-auth-green'; Container = 'hub-allocator' }
    battle_result = [pscustomobject]@{ App = 'battle-result'; Deployment = 'battle-result-ds-auth-green'; Container = 'battle-result' }
}

function Get-Json([string[]]$Arguments, [string]$Action) {
    $lines = @(& kubectl --context $KubeContext -n $Namespace @Arguments 2>&1)
    if ($LASTEXITCODE -ne 0) { throw "$Action failed:$($lines -join [Environment]::NewLine)" }
    try { return (($lines -join "`n") | ConvertFrom-Json -ErrorAction Stop) }
    catch { throw "$Action returned invalid JSON:$($_.Exception.Message)" }
}

Push-Location $ProjectRoot
try {
    $requiredArgs = @('run', './pkg/dsauthfence/cmd/dsauth-required', '--endpoints', $EtcdEndpoints,
        '--prefix', $EtcdPrefix, '--min-epoch', '2', '--max-epoch', '2',
        '--min-policy-generation', '2', '--max-policy-generation', '2') + $securityArgs
    & go @requiredArgs
    if ($LASTEXITCODE -ne 0) { throw 'V3 prepare is allowed only against exact required policy V2.' }

    $expectedObjects = @($writerSpecs.Values | ForEach-Object { "deployment.apps/$($_.Deployment)" } | Sort-Object)
    $preview = @(& kubectl --context $KubeContext -n $Namespace apply --server-side --dry-run=server `
        --validate=strict --field-manager=pandora-ds-auth-v3-prepare -f $StageManifest -o name 2>&1)
    if ($LASTEXITCODE -ne 0 -or (@($preview | Sort-Object) -join ',') -cne ($expectedObjects -join ',')) {
        throw "StageManifest server preview must contain only the exact five green Deployments:$($preview -join ',')"
    }

    # Names alone are not a safe preview: server-side apply merges omitted
    # fields with the live object.  Materialize each merged candidate as JSON
    # and validate the full writer/identity/image/strategy contract before the
    # first mutating apply.  The exact-name gate above ensures the selector
    # cannot hide an additional object.
    foreach ($service in $writerSpecs.Keys) {
        $spec = $writerSpecs[$service]
        $candidateOutput = Get-Json @('apply', '--server-side', '--dry-run=server', '--validate=strict',
            '--field-manager=pandora-ds-auth-v3-prepare', '-f', $StageManifest,
            '--selector', "app=$($spec.App)", '-o', 'json') `
            "server-preview merged Deployment/$($spec.Deployment)"
        $candidates = if ([string]$candidateOutput.kind -ceq 'List') {
            @($candidateOutput.items)
        } else {
            @($candidateOutput)
        }
        if ($candidates.Count -ne 1) {
            throw "StageManifest must yield exactly one server-preview candidate for app=$($spec.App)."
        }
        $secretName = Get-PandoraDsAuthIdentitySecretName $spec.App $EtcdIdentityRevision
        $identitySecret = Get-Json @('get', "secret/$secretName", '-o', 'json') `
            "read immutable identity Secret/$secretName"
        $identityContract = Assert-PandoraDsAuthIdentitySecretContract $identitySecret $spec.App $EtcdIdentityRevision
        $null = Assert-PandoraDsAuthV3GreenStageDeploymentContract $candidates[0] $spec.App `
            $EtcdIdentityRevision $ServerName $ForbiddenReadPrefix ([bool]$identityContract.UsesPasswordAuth)
    }
    $preStageHubService = Get-Json @('get', 'service/hub-allocator', '-o', 'json') `
        'read pre-stage canonical Hub Service'
    Assert-PandoraHubAllocatorGreenServiceContract $preStageHubService $Namespace
    & kubectl --context $KubeContext -n $Namespace apply --server-side `
        --validate=strict --field-manager=pandora-ds-auth-v3-prepare -f $StageManifest
    if ($LASTEXITCODE -ne 0) { throw 'Server-side stage apply failed.' }

    $deadline = [datetimeoffset]::UtcNow.Add($Timeout)
    do {
        try {
            $serviceParts = [System.Collections.Generic.List[string]]::new()
            $instanceParts = [System.Collections.Generic.List[string]]::new()
            $digestParts = [System.Collections.Generic.List[string]]::new()
            $hubUID = ''
            foreach ($service in $writerSpecs.Keys) {
                $spec = $writerSpecs[$service]
                $deployment = Get-Json @('get', "deployment/$($spec.Deployment)", '-o', 'json') `
                    "read Deployment/$($spec.Deployment)"
                $selector = $deployment.spec.selector.matchLabels
                $desired = [int]$deployment.spec.replicas
                if ($desired -le 0 -or [string]$deployment.metadata.namespace -cne $Namespace -or
                    [string]$selector.app -cne $spec.App -or
                    [string]$selector.'pandora.dev/ds-auth-writer-set' -cne 'green' -or
                    [string]$selector.'pandora.dev/ds-auth-writer-epoch' -cne '2' -or
                    @($selector.PSObject.Properties).Count -ne 3 -or
                    [int64]$deployment.metadata.generation -ne [int64]$deployment.status.observedGeneration -or
                    [int]$deployment.status.replicas -ne $desired -or
                    [int]$deployment.status.updatedReplicas -ne $desired) {
                    throw "Deployment/$($spec.Deployment) has not converged to its canonical selector/replicas."
                }
                if ($service -ceq 'hub_allocator' -and
                    ($desired -ne 1 -or [string]$deployment.spec.strategy.type -cne 'Recreate')) {
                    throw 'Staged Hub must be replicas=1 and Recreate.'
                }
                $selectorText = "app=$($spec.App),pandora.dev/ds-auth-writer-set=green,pandora.dev/ds-auth-writer-epoch=2"
                $podList = Get-Json @('get', 'pods', '-l', $selectorText, '-o', 'json') "read $service Pods"
                $pods = @($podList.items | Where-Object {
                    $p = $_.metadata.PSObject.Properties['deletionTimestamp']
                    $null -eq $p -or [string]::IsNullOrWhiteSpace([string]$p.Value)
                })
                if ($pods.Count -ne $desired) { throw "$service non-deleting Pod count is not exact=$desired." }
                $uids = [System.Collections.Generic.List[string]]::new()
                $digests = [System.Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
                foreach ($pod in $pods) {
                    $uid = [string]$pod.metadata.uid
                    $owners = @($pod.metadata.ownerReferences | Where-Object { $_.controller -eq $true -and [string]$_.kind -ceq 'ReplicaSet' })
                    if ([string]$pod.status.phase -cne 'Running' -or $owners.Count -ne 1) {
                        throw "$service/$uid is not a canonical Running Deployment Pod."
                    }
                    $rs = Get-Json @('get', "replicaset/$($owners[0].name)", '-o', 'json') "read $service ReplicaSet"
                    $depOwners = @($rs.metadata.ownerReferences | Where-Object {
                        $_.controller -eq $true -and [string]$_.kind -ceq 'Deployment' -and
                        [string]$_.name -ceq $spec.Deployment -and [string]$_.uid -ceq [string]$deployment.metadata.uid })
                    $status = @($pod.status.containerStatuses | Where-Object { [string]$_.name -ceq $spec.Container })
                    if ($depOwners.Count -ne 1 -or $status.Count -ne 1 -or $null -eq $status[0].state.running -or
                        [string]$status[0].imageID -cnotmatch '(sha256:[0-9a-f]{64})$') {
                        throw "$service/$uid target writer container/owner/imageID is not canonical."
                    }
                    $uids.Add($uid); $null = $digests.Add([string]$Matches[1])
                }
                if ($digests.Count -ne 1) { throw "$service staged replicas do not use one exact digest." }
                $sortedUIDs = @($uids | Sort-Object)
                $serviceParts.Add("$service=$desired")
                $instanceParts.Add("$service=$($sortedUIDs -join '|')")
                $digestParts.Add("$service=$(@($digests)[0])")
                if ($service -ceq 'hub_allocator') { $hubUID = $sortedUIDs[0] }
            }
            $hubPods = Get-Json @('get', 'pods', '-l',
                'app=hub-allocator,pandora.dev/ds-auth-writer-set=green,pandora.dev/ds-auth-writer-epoch=2',
                '-o', 'json') 'read staged Hub Pod'
            $hubPod = @($hubPods.items | Where-Object { [string]$_.metadata.uid -ceq $hubUID })[0]
            $hubReady = @($hubPod.status.conditions | Where-Object { [string]$_.type -ceq 'Ready' -and [string]$_.status -ceq 'True' }).Count
            $hubService = Get-Json @('get', 'service/hub-allocator', '-o', 'json') `
                'read canonical Hub Service'
            Assert-PandoraHubAllocatorGreenServiceContract $hubService $Namespace
            $slices = Get-Json @('get', 'endpointslices', '-l', 'kubernetes.io/service-name=hub-allocator', '-o', 'json') 'read Hub EndpointSlices'
            $readyEndpoints = @($slices.items.endpoints | Where-Object { $_.conditions.ready -eq $true })
            if ($hubReady -ne 0 -or $readyEndpoints.Count -ne 0) {
                throw 'Staged Hub must be Running but NotReady with zero ready Service endpoints.'
            }
            $expectedServices = $serviceParts -join ','
            $expectedInstances = $instanceParts -join ','
            $expectedDigests = $digestParts -join ','
            break
        } catch {
            if ([datetimeoffset]::UtcNow -ge $deadline) { throw }
            Write-Warning "V3 stage has not converged: $($_.Exception.Message)"
            Start-Sleep -Seconds 2
        }
    } while ($true)

    # Kubernetes creationTimestamp is second precision; keep proof<=create.
    $completedMS = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds() * 1000
    $data = [pscustomobject][ordered]@{
        contract = 'ds-auth-policy-v3-activation-evidence-v1'; run_id = $ActivationRunID
        kube_context = $KubeContext; namespace = $Namespace
        from_policy_generation = '2'; to_policy_generation = '3'
        from_required_value = '2@ds-auth-v2-pod-uid-write-invariant-v1'
        to_required_value = '2@ds-auth-v2-hub-successor-lease-v1'
        required_policy_id = 'ds-auth-v2-hub-successor-lease-v1'
        staging_contract = 'capability-lease-not-service-endpoint-v1'
        expected_services = $expectedServices; expected_instances = $expectedInstances
        expected_image_digests = $expectedDigests
        required_features = $script:PandoraDsAuthRequiredFeaturesV3
        evidence_sha256 = ('sha256:' + ('0' * 64)); completed_at_unix_ms = [string]$completedMS
    }
    $data.evidence_sha256 = Get-PandoraDsAuthPolicyV3EvidenceSHA256 $data
    $name = $script:PandoraDsAuthPolicyV3EvidenceName
    $existing = @(& kubectl --context $KubeContext -n $Namespace get "configmap/$name" --ignore-not-found -o json 2>&1)
    if ($LASTEXITCODE -ne 0) { throw 'Read existing V3 evidence failed.' }
    if ([string]::IsNullOrWhiteSpace(($existing -join "`n").Trim())) {
        $object = [pscustomobject][ordered]@{ apiVersion = 'v1'; kind = 'ConfigMap';
            metadata = [pscustomobject]@{ name = $name; namespace = $Namespace };
            immutable = $true; data = $data }
        $json = $object | ConvertTo-Json -Depth 12 -Compress
        $created = @($json | & kubectl --context $KubeContext -n $Namespace create --validate=strict -f - 2>&1)
        if ($LASTEXITCODE -ne 0) { throw "Create-only V3 evidence failed:$($created -join [Environment]::NewLine)" }
    }
    $marker = Get-Json @('get', "configmap/$name", '-o', 'json') 'read back immutable V3 evidence'
    $proof = Assert-PandoraDsAuthPolicyV3EvidenceContract $marker $expectedServices $expectedInstances `
        $expectedDigests $KubeContext $Namespace
    Write-Host "V3 staging evidence ready: run_id=$($proof.RunID) uid=$($proof.UID) sha256=$($proof.EvidenceSHA256)"
    Write-Host 'Next: run activate_hub_successor_policy.ps1; it derives all counts/UIDs/digests from this marker.'
} finally { Pop-Location }
