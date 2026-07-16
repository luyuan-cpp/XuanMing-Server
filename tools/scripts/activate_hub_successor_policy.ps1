# Dedicated immutable DS-auth policy V2 -> V3 activation entry point.
#
# The candidate hub-allocator intentionally registers a leased capability but
# stays NotReady and does not open RPC while required policy is V2. Therefore
# this command audits the exact etcd capability set supplied by Pod UID; it
# MUST NOT discover Hub through Service Endpoints or wait for Hub readiness.
[CmdletBinding()]
param(
    [ValidateSet('Audit', 'Activate')][string]$Phase = 'Audit',
    [Parameter(Mandatory = $true)][string]$KubeContext,
    [string]$Namespace = 'pandora',
    [Parameter(Mandatory = $true)][string]$EtcdEndpoints,
    [string]$EtcdPrefix = '/pandora/ds-auth/',
    [Parameter(Mandatory = $true)][string]$KeysetRevision,
    [Parameter(Mandatory = $true)][string]$EtcdIdentityRevision,
    [Parameter(Mandatory = $true)][string]$CAFile,
    [Parameter(Mandatory = $true)][string]$CertFile,
    [Parameter(Mandatory = $true)][string]$KeyFile,
    [Parameter(Mandatory = $true)][string]$ServerName,
    [Parameter(Mandatory = $true)][string]$ClientIdentity,
    [string]$UsernameFile = '', [string]$PasswordFile = '',
    [Parameter(Mandatory = $true)][string]$ForbiddenReadPrefix,
    [timespan]$Timeout = ([timespan]::FromSeconds(30))
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$ProjectRoot = (Resolve-Path (Join-Path $PSScriptRoot '../..')).Path
. (Join-Path $PSScriptRoot 'lib/ds_auth_activation_contract.ps1')

if ($Timeout.TotalSeconds -lt 1 -or $Timeout.TotalSeconds -gt 300) {
    throw 'Timeout must be between 1 and 300 seconds.'
}
function Convert-ExactMap([string]$Raw, [string]$Name) {
    $out = @{}
    foreach ($item in $Raw.Split(',')) {
        $parts = $item.Split('=', 2)
        if ($parts.Count -ne 2 -or [string]::IsNullOrWhiteSpace($parts[0]) -or
            [string]::IsNullOrWhiteSpace($parts[1]) -or $out.ContainsKey($parts[0])) {
            throw "$Name is not a canonical unique service map."
        }
        $out[$parts[0]] = $parts[1]
    }
    return $out
}

function Get-ClusterJson([string[]]$Arguments, [string]$Action) {
    $lines = @(& kubectl --context $KubeContext -n $Namespace @Arguments 2>&1)
    if ($LASTEXITCODE -ne 0) { throw "$Action failed:$($lines -join [Environment]::NewLine)" }
    try { return (($lines -join "`n") | ConvertFrom-Json -ErrorAction Stop) }
    catch { throw "$Action did not return JSON:$($_.Exception.Message)" }
}

function Assert-OrCreateV3CompletionMarker([string]$FinalInstances, [bool]$AllowCreate) {
    $name = "pandora-ds-auth-policy-v3-complete-$($policyEvidence.RunID)"
    $existingLines = @(& kubectl --context $KubeContext -n $Namespace get "configmap/$name" `
        --ignore-not-found -o json 2>&1)
    if ($LASTEXITCODE -ne 0) { throw "Read V3 completion marker failed:$($existingLines -join [Environment]::NewLine)" }
    $existingText = ($existingLines -join "`n").Trim()
    if ([string]::IsNullOrWhiteSpace($existingText)) {
        if (-not $AllowCreate) {
            throw "Immutable V3 completion marker ConfigMap/$name is required for an existing V3 Audit."
        }
        # Kubernetes creationTimestamp has second precision. Floor evidence to
        # the same precision so a valid create cannot appear to predate proof.
        $nowMS = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds() * 1000
        $object = [pscustomobject][ordered]@{
            apiVersion = 'v1'; kind = 'ConfigMap'
            metadata = [pscustomobject]@{ name = $name; namespace = $Namespace }
            immutable = $true
            data = [pscustomobject][ordered]@{
                contract = 'ds-auth-policy-v3-completion-v1'
                run_id = $policyEvidence.RunID
                required_value = '2@ds-auth-v2-hub-successor-lease-v1'
                policy_generation = '3'
                evidence_uid = $policyEvidence.UID
                evidence_resource_version = $policyEvidence.ResourceVersion
                evidence_sha256 = $policyEvidence.EvidenceSHA256
                final_instances = $FinalInstances
                completed_at_unix_ms = [string]$nowMS
            }
        }
        $json = $object | ConvertTo-Json -Depth 12 -Compress
        $createLines = @($json | & kubectl --context $KubeContext -n $Namespace create -f - 2>&1)
        if ($LASTEXITCODE -ne 0) {
            throw "Create immutable V3 completion marker failed:$($createLines -join [Environment]::NewLine)"
        }
        $existingLines = @(& kubectl --context $KubeContext -n $Namespace get "configmap/$name" -o json 2>&1)
        if ($LASTEXITCODE -ne 0) { throw 'Cannot read back immutable V3 completion marker.' }
        $existingText = $existingLines -join "`n"
    }
    try { $marker = ($existingText | ConvertFrom-Json -ErrorAction Stop) }
    catch { throw "V3 completion marker is not JSON:$($_.Exception.Message)" }
    return Assert-PandoraDsAuthV3CompletionContract $marker $name $Namespace $policyEvidence $serviceCountMap
}

$writerSpecs = [ordered]@{
    login = [pscustomobject]@{ App = 'login'; Deployment = 'login-ds-auth-green'; Container = 'login' }
    player_locator = [pscustomobject]@{ App = 'player-locator'; Deployment = 'player-locator-ds-auth-green'; Container = 'player-locator' }
    ds_allocator = [pscustomobject]@{ App = 'ds-allocator'; Deployment = 'ds-allocator-ds-auth-green'; Container = 'ds-allocator' }
    hub_allocator = [pscustomobject]@{ App = 'hub-allocator'; Deployment = 'hub-allocator-ds-auth-green'; Container = 'hub-allocator' }
    battle_result = [pscustomobject]@{ App = 'battle-result'; Deployment = 'battle-result-ds-auth-green'; Container = 'battle-result' }
}

function Get-CanonicalWriterInstances([bool]$PreCAS, [bool]$RequireHubRoute) {
    $canonical = [System.Collections.Generic.List[string]]::new()
    $currentHubUID = ''
    foreach ($service in $writerSpecs.Keys) {
        $spec = $writerSpecs[$service]
        $desired = 0
        if (-not [int]::TryParse([string]$serviceCountMap[$service], [ref]$desired) -or $desired -le 0) {
            throw "ExpectedServices count for $service is invalid."
        }
        $deployment = Get-ClusterJson @('get', "deployment/$($spec.Deployment)", '-o', 'json') `
            "read canonical writer Deployment/$($spec.Deployment)"
        $selector = $deployment.spec.selector.matchLabels
        if ([string]$deployment.metadata.name -cne $spec.Deployment -or
            [string]$deployment.metadata.namespace -cne $Namespace -or
            [string]$selector.app -cne $spec.App -or
            [string]$selector.'pandora.dev/ds-auth-writer-set' -cne 'green' -or
            [string]$selector.'pandora.dev/ds-auth-writer-epoch' -cne '2' -or
            @($selector.PSObject.Properties).Count -ne 3) {
            throw "Deployment/$($spec.Deployment) is not the canonical exact green writer selector."
        }
        if ([int64]$deployment.metadata.generation -ne [int64]$deployment.status.observedGeneration -or
            [int]$deployment.spec.replicas -ne $desired -or
            [int]$deployment.status.replicas -ne $desired -or
            [int]$deployment.status.updatedReplicas -ne $desired) {
            throw "Deployment/$($spec.Deployment) desired/observed/updated replicas are not exact=$desired."
        }
        if ($service -ceq 'hub_allocator' -and
            ([int]$deployment.spec.replicas -ne 1 -or [string]$deployment.spec.strategy.type -cne 'Recreate')) {
            throw 'Hub V3 candidate must be replicas=1 with Recreate strategy.'
        }
        $selectorText = "app=$($spec.App),pandora.dev/ds-auth-writer-set=green,pandora.dev/ds-auth-writer-epoch=2"
        $selected = Get-ClusterJson @('get', 'pods', '-l', $selectorText, '-o', 'json') `
            "read selector Pods for $service"
        $pods = @($selected.items | Where-Object {
            $property = $_.metadata.PSObject.Properties['deletionTimestamp']
            $null -eq $property -or [string]::IsNullOrWhiteSpace([string]$property.Value)
        })
        $currentUIDs = @($pods | ForEach-Object { [string]$_.metadata.uid })
        $canonicalUIDs = Resolve-PandoraDsAuthV3CanonicalUIDSet $service $currentUIDs $desired `
            @($instanceMap[$service].Split('|')) -PreCAS:$PreCAS
        $currentUIDs = @($canonicalUIDs.Split('|'))
        foreach ($pod in $pods) {
            $uid = [string]$pod.metadata.uid
            if ([string]$pod.status.phase -cne 'Running') {
                throw "Writer $service/$uid is not in Running phase."
            }
            foreach ($label in @($selector.PSObject.Properties)) {
                if ([string]$pod.metadata.labels.($label.Name) -cne [string]$label.Value) {
                    throw "Pod $service/$uid does not match the canonical Deployment selector."
                }
            }
            $podOwners = @($pod.metadata.ownerReferences | Where-Object {
                [string]$_.kind -ceq 'ReplicaSet' -and $_.controller -eq $true })
            if ($podOwners.Count -ne 1) { throw "Pod $service/$uid lacks one exact controller ReplicaSet owner." }
            $rs = Get-ClusterJson @('get', "replicaset/$($podOwners[0].name)", '-o', 'json') `
                "read owner ReplicaSet for $service/$uid"
            if ([string]$rs.metadata.uid -cne [string]$podOwners[0].uid) {
                throw "Pod $service/$uid ReplicaSet owner UID drifted."
            }
            $deploymentOwners = @($rs.metadata.ownerReferences | Where-Object {
                [string]$_.kind -ceq 'Deployment' -and $_.controller -eq $true -and
                [string]$_.name -ceq $spec.Deployment -and
                [string]$_.uid -ceq [string]$deployment.metadata.uid })
            if ($deploymentOwners.Count -ne 1) {
                throw "Pod $service/$uid is not owned by canonical Deployment/$($spec.Deployment)."
            }
            $statuses = @($pod.status.containerStatuses | Where-Object { [string]$_.name -ceq $spec.Container })
            if ($statuses.Count -ne 1 -or $null -eq $statuses[0].state.running -or
                [string]$statuses[0].imageID -cnotmatch ('@?' + [regex]::Escape($digestMap[$service]) + '$')) {
                throw "Writer container $service/$uid/$($spec.Container) must be uniquely Running with the expected imageID digest."
            }
        }
        $canonical.Add("$service=$($currentUIDs -join '|')")
        if ($service -ceq 'hub_allocator') { $currentHubUID = $currentUIDs[0] }
    }
    $hubPodList = Get-ClusterJson @('get', 'pods', '-l',
        'app=hub-allocator,pandora.dev/ds-auth-writer-set=green,pandora.dev/ds-auth-writer-epoch=2',
        '-o', 'json') 'read canonical Hub Pod'
    $hubPod = @($hubPodList.items | Where-Object { [string]$_.metadata.uid -ceq $currentHubUID })[0]
    $hubReady = @($hubPod.status.conditions | Where-Object {
        [string]$_.type -ceq 'Ready' -and [string]$_.status -ceq 'True' }).Count -eq 1
    # Endpoint cardinality is meaningful only if the Service itself still
    # selects the exact canonical green Hub.  A drifted/empty selector could
    # otherwise manufacture a false pre-CAS zero-endpoint proof.
    $hubService = Get-ClusterJson @('get', 'service/hub-allocator', '-o', 'json') `
        'read canonical Hub Service'
    Assert-PandoraHubAllocatorGreenServiceContract $hubService $Namespace
    $sliceList = Get-ClusterJson @('get', 'endpointslices', '-l',
        'kubernetes.io/service-name=hub-allocator', '-o', 'json') 'read Hub EndpointSlices'
    $readyEndpoints = @($sliceList.items.endpoints | Where-Object { $_.conditions.ready -eq $true })
    if ($PreCAS -and ($hubReady -or $readyEndpoints.Count -ne 0)) {
        throw 'Pre-CAS staged Hub must be Running with a capability lease but NotReady; Hub ready EndpointSlice cardinality must be 0.'
    }
    if ($RequireHubRoute -and (-not $hubReady -or $readyEndpoints.Count -ne 1 -or
        [string]($readyEndpoints[0].targetRef.uid) -cne $currentHubUID)) {
        throw 'Post-CAS Hub must be Ready with exactly one ready EndpointSlice target whose UID is the expected Hub.'
    }
    return ($canonical -join ',')
}

if (-not (Get-Command kubectl -ErrorAction SilentlyContinue)) { throw 'kubectl is required.' }
if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw 'go is required.' }
if ([string]::IsNullOrWhiteSpace($KeysetRevision)) { throw 'KeysetRevision is required.' }
$null = Assert-PandoraDsAuthHttpsEndpoints $EtcdEndpoints
$securityArgs = @(Get-PandoraDsAuthSecureGoArgs $CAFile $CertFile $KeyFile $ServerName `
    $ClientIdentity $EtcdIdentityRevision $ForbiddenReadPrefix $UsernameFile $PasswordFile)

$snapshotArgs = @('run', './pkg/dsauthfence/cmd/dsauth-required',
    '--endpoints', $EtcdEndpoints, '--prefix', $EtcdPrefix,
    '--min-epoch', '2', '--max-epoch', '2', '--min-policy-generation', '2',
    '--max-policy-generation', '3', '--output', 'json',
    '--timeout', ('{0}s' -f [int]$Timeout.TotalSeconds))
$snapshotArgs += $securityArgs

$recordArgs = @('run', './pkg/dsauthfence/cmd/dsauth-required',
    '--endpoints', $EtcdEndpoints, '--prefix', $EtcdPrefix,
    '--min-epoch', '2', '--max-epoch', '2', '--min-policy-generation', '3',
    '--max-policy-generation', '3', '--require-v3-activation-record',
    '--timeout', ('{0}s' -f [int]$Timeout.TotalSeconds))
$recordArgs += $securityArgs

function New-V3AuditArgs([string]$Instances, [bool]$ApplyPolicy) {
    $result = @('run', './pkg/dsauthfence/cmd/dsauth-activate',
        '--endpoints', $EtcdEndpoints, '--prefix', $EtcdPrefix,
        '--policy-v3', '--expected-services', $ExpectedServices,
        '--expected-instances', $Instances,
        '--keyset-revision', $KeysetRevision,
        '--allowed-image-digests', $AllowedImageDigests,
        '--expected-image-digests', $ExpectedImageDigests,
        '--required-features', $script:PandoraDsAuthRequiredFeaturesV3,
        '--activation-evidence-sha256', $policyEvidence.EvidenceSHA256,
        '--activation-evidence-completed-at-ms', [string]$policyEvidence.CompletedAtUnixMS,
        '--timeout', ('{0}s' -f [int]$Timeout.TotalSeconds))
    $result += $securityArgs
    if ($ApplyPolicy) { $result += '--apply' }
    return $result
}

Push-Location $ProjectRoot
try {
    $snapshotLines = @(& go @snapshotArgs 2>&1)
    if ($LASTEXITCODE -ne 0) { throw "Cannot linearly read required V2/V3:$($snapshotLines -join [Environment]::NewLine)" }
    try { $snapshot = (($snapshotLines -join "`n") | ConvertFrom-Json -ErrorAction Stop) }
    catch { throw "Required policy snapshot is not JSON:$($_.Exception.Message)" }
    $markerJSON = @(& kubectl --context $KubeContext -n $Namespace get `
        "configmap/$script:PandoraDsAuthPolicyV3EvidenceName" -o json 2>&1)
    if ($LASTEXITCODE -ne 0) {
        throw "Platform-precreated immutable V3 evidence ConfigMap is required:$($markerJSON -join [Environment]::NewLine)"
    }
    try { $marker = (($markerJSON -join "`n") | ConvertFrom-Json -ErrorAction Stop) }
    catch { throw "V3 policy evidence is not JSON:$($_.Exception.Message)" }
    $policyEvidence = Assert-PandoraDsAuthPolicyV3EvidenceContract $marker `
        ([string]$marker.data.expected_services) ([string]$marker.data.expected_instances) `
        ([string]$marker.data.expected_image_digests) $KubeContext $Namespace
    $ExpectedServices = $policyEvidence.ExpectedServices
    $ExpectedInstances = $policyEvidence.ExpectedInstances
    $ExpectedImageDigests = $policyEvidence.ExpectedImageDigests
    $instanceMap = Convert-ExactMap $ExpectedInstances 'marker expected_instances'
    $digestMap = Convert-ExactMap $ExpectedImageDigests 'marker expected_image_digests'
    $serviceCountMap = Convert-ExactMap $ExpectedServices 'marker expected_services'
    if ((@($instanceMap.Keys | Sort-Object) -join ',') -cne (@($writerSpecs.Keys | Sort-Object) -join ',') -or
        (@($digestMap.Keys | Sort-Object) -join ',') -cne (@($writerSpecs.Keys | Sort-Object) -join ',') -or
        (@($serviceCountMap.Keys | Sort-Object) -join ',') -cne (@($writerSpecs.Keys | Sort-Object) -join ',') -or
        @($instanceMap['hub_allocator'].Split('|')).Count -ne 1) {
        throw 'Immutable V3 evidence does not contain the exact five-service/single-Hub maps.'
    }
    $AllowedImageDigests = @($digestMap.Values | Sort-Object -Unique) -join ','
    if ([uint32]$snapshot.policy_generation -eq 2) {
        $stagedInstances = Get-CanonicalWriterInstances $true $false
        $preCASArgs = @(New-V3AuditArgs $stagedInstances ($Phase -ceq 'Activate'))
        & go @preCASArgs
        if ($LASTEXITCODE -ne 0) {
            throw 'V3 exact staging capability audit/policy CAS failed; pre-CAS Hub readiness is not used as positive evidence.'
        }
        if ($Phase -ceq 'Audit') { return }
    } elseif ([uint32]$snapshot.policy_generation -eq 3) {
        # Crash/retry after CAS: never rerun the V2-only NotReady/no-endpoint gate
        # or attempt another CAS. Start from the durable record proof.
        & go @recordArgs
        if ($LASTEXITCODE -ne 0) { throw 'Existing V3 required+record provenance verification failed.' }
    } else {
        throw "Unsupported required policy generation=$($snapshot.policy_generation)."
    }

    if ($Phase -ceq 'Activate') {
        # First verify only the linear required+record pair. Do not rerun the
        # volatile pre-CAS staging audit before this durable proof is established.
        & go @recordArgs
        if ($LASTEXITCODE -ne 0) { throw 'V3 required+immutable record linear verification failed.' }

        # A policy watch terminates every old capability. Retry until all five
        # replacement processes have acquired their leases against exact V3;
        # AuditCapabilities checks acquired_policy_generation/id, not rollout alone.
        $deadline = [datetimeoffset]::UtcNow.Add($Timeout)
        do {
            $verified = $false
            try {
                $currentInstances = Get-CanonicalWriterInstances $false $false
                $verifyArgs = @(New-V3AuditArgs $currentInstances $false)
                & go @verifyArgs
                $verified = $LASTEXITCODE -eq 0
            } catch {
                Write-Warning "V3 capability reacquisition not converged: $($_.Exception.Message)"
            }
            if ($verified) { break }
            if ([datetimeoffset]::UtcNow -ge $deadline) {
                throw 'V3 post-CAS writer capability reacquisition audit timed out.'
            }
            Start-Sleep -Seconds 2
        } while ($true)
        foreach ($service in $writerSpecs.Keys) {
            $deployment = [string]$writerSpecs[$service].Deployment
            & kubectl --context $KubeContext -n $Namespace rollout status "deployment/$deployment" `
                --timeout ('{0}s' -f [int]$Timeout.TotalSeconds)
            if ($LASTEXITCODE -ne 0) {
                throw "Writer Deployment/$deployment did not restart/serve under exact V3 after CAS."
            }
        }
        $finalInstances = Get-CanonicalWriterInstances $false $true
        $finalArgs = @(New-V3AuditArgs $finalInstances $false)
        # Final audit occurs after the unique Hub endpoint is visible; it proves
        # no writer lost/replaced its exact acquired-policy V3 capability while
        # readiness and routing converged.
        & go @finalArgs
        if ($LASTEXITCODE -ne 0) { throw 'Final post-endpoint acquired-policy V3 capability audit failed.' }
        $null = Assert-OrCreateV3CompletionMarker $finalInstances $true
    } else {
        # Audit against an already-V3 cluster is a post-CAS audit and must not
        # stop at the record proof.
        $currentInstances = Get-CanonicalWriterInstances $false $true
        $currentArgs = @(New-V3AuditArgs $currentInstances $false)
        & go @currentArgs
        if ($LASTEXITCODE -ne 0) { throw 'Existing V3 exact acquired-policy capability audit failed.' }
        # Existing V3 is healthy only after activation finished all routing and
        # recorded the immutable completion marker.  Audit is read-only here.
        $null = Assert-OrCreateV3CompletionMarker $currentInstances $false
    }
} finally { Pop-Location }
