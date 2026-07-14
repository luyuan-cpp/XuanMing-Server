$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$ProjectRoot = (Resolve-Path "$PSScriptRoot/../../..").Path
. (Join-Path $ProjectRoot 'tools/scripts/lib/ds_auth_activation_contract.ps1')

function Assert-True([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw "ASSERT FAILED: $Message" }
}

function Assert-Throws([scriptblock]$Action, [string]$Message) {
    $threw = $false
    try { & $Action } catch { $threw = $true }
    if (-not $threw) { throw "ASSERT FAILED (expected throw): $Message" }
}

function Copy-Object($Value) { return (($Value | ConvertTo-Json -Depth 30) | ConvertFrom-Json) }

$revision = 'r7'
$digest = 'sha256:' + ('a' * 64)
$secret = [pscustomobject]@{
    metadata = [pscustomobject]@{
        name = 'pandora-ds-auth-etcd-ds-allocator-r7'
        labels = [pscustomobject]@{
            'pandora.dev/ds-auth-etcd-identity-revision' = 'r7'
            'pandora.dev/ds-auth-etcd-client-identity' = 'pandora-ds-allocator'
        }
    }
    immutable = $true
    data = [pscustomobject]@{ 'ca.crt' = 'Y2E='; 'tls.crt' = 'Y2VydA=='; 'tls.key' = 'a2V5' }
}
$secretContract = Assert-PandoraDsAuthIdentitySecretContract $secret 'ds-allocator' $revision
Assert-True (-not $secretContract.UsesPasswordAuth) 'CN-only identity should be accepted'

$badSecret = Copy-Object $secret
$badSecret.immutable = $false
Assert-Throws { Assert-PandoraDsAuthIdentitySecretContract $badSecret 'ds-allocator' $revision } 'mutable identity Secret'
$badSecret = Copy-Object $secret
$badSecret.data | Add-Member -NotePropertyName username -NotePropertyValue 'dXNlcg=='
Assert-Throws { Assert-PandoraDsAuthIdentitySecretContract $badSecret 'ds-allocator' $revision } 'half username/password identity'
$badSecret = Copy-Object $secret
$badSecret.metadata.labels.'pandora.dev/ds-auth-etcd-client-identity' = 'pandora-login'
Assert-Throws { Assert-PandoraDsAuthIdentitySecretContract $badSecret 'ds-allocator' $revision } 'wrong certificate identity label'

$envValues = [ordered]@{
    'PANDORA_DS_AUTH_ETCD_REQUIRE_MTLS' = '1'
    'PANDORA_DS_AUTH_ETCD_CA_FILE' = '/run/secrets/pandora/ds-auth-etcd/ca.crt'
    'PANDORA_DS_AUTH_ETCD_CERT_FILE' = '/run/secrets/pandora/ds-auth-etcd/tls.crt'
    'PANDORA_DS_AUTH_ETCD_KEY_FILE' = '/run/secrets/pandora/ds-auth-etcd/tls.key'
    'PANDORA_DS_AUTH_ETCD_SERVER_NAME' = 'etcd.pandora.internal'
    'PANDORA_DS_AUTH_ETCD_CLIENT_IDENTITY' = 'pandora-ds-allocator'
    'PANDORA_DS_AUTH_ETCD_IDENTITY_REVISION' = 'r7'
    'PANDORA_DS_AUTH_ETCD_REQUIRE_AUTH' = '1'
    'PANDORA_DS_AUTH_ETCD_FORBIDDEN_READ_PREFIX' = '/pandora/acl-negative/'
}
$envList = @($envValues.GetEnumerator() | ForEach-Object {
    [pscustomobject]@{ name = $_.Key; value = $_.Value; valueFrom = $null }
})
$pod = [pscustomobject]@{
    metadata = [pscustomobject]@{
        name = 'ds-allocator-green-1'; namespace = 'pandora'; uid = 'pod-1'; deletionTimestamp = $null
        labels = [pscustomobject]@{ 'istio.io/rev' = 'asm-1-22' }
        annotations = [pscustomobject]@{ 'sidecar.istio.io/rewriteAppHTTPProbers' = 'true' }
    }
    spec = [pscustomobject]@{
        serviceAccountName = 'pandora-allocator'
        containers = @([pscustomobject]@{
            name = 'ds-allocator'; env = $envList
            volumeMounts = @([pscustomobject]@{ name = 'ds-auth-etcd-identity'; mountPath = '/run/secrets/pandora/ds-auth-etcd'; readOnly = $true })
        })
        volumes = @([pscustomobject]@{ name = 'ds-auth-etcd-identity'; secret = [pscustomobject]@{ secretName = 'pandora-ds-auth-etcd-ds-allocator-r7'; defaultMode = 288 } })
    }
    status = [pscustomobject]@{
        containerStatuses = @([pscustomobject]@{ name = 'istio-proxy'; ready = $true; restartCount = 0 })
    }
}
Assert-PandoraDsAuthIdentityPodContract $pod 'ds-allocator' $revision 'etcd.pandora.internal' '/pandora/acl-negative/' $false
Assert-PandoraDsTerminalMeshPodContract $pod 'ds-allocator'
$badPod = Copy-Object $pod
($badPod.spec.containers[0].env | Where-Object name -ceq 'PANDORA_DS_AUTH_ETCD_REQUIRE_AUTH').value = '0'
Assert-Throws { Assert-PandoraDsAuthIdentityPodContract $badPod 'ds-allocator' $revision 'etcd.pandora.internal' '/pandora/acl-negative/' $false } 'auth disabled in Pod'
$badPod = Copy-Object $pod
$badPod.status.containerStatuses[0].ready = $false
Assert-Throws { Assert-PandoraDsTerminalMeshPodContract $badPod 'ds-allocator' } 'terminal mesh sidecar not ready'

$peer = [pscustomobject]@{
    metadata = [pscustomobject]@{ name = 'pandora-ds-allocator-terminal-permissive'; namespace = 'pandora' }
    spec = [pscustomobject]@{ selector = [pscustomobject]@{ matchLabels = [pscustomobject]@{ app = 'ds-allocator' } }; mtls = [pscustomobject]@{ mode = 'PERMISSIVE' } }
}
$releasePath = '/pandora.ds.v1.DSAllocatorService/ReleaseBattle'
function New-DenyRule([string]$Principal) {
    return [pscustomobject]@{
        from = @([pscustomobject]@{ source = [pscustomobject]@{ notPrincipals = @($Principal) } })
        to = @([pscustomobject]@{ operation = [pscustomobject]@{ methods = @('POST'); paths = @($releasePath) } })
    }
}
$policy = [pscustomobject]@{
    metadata = [pscustomobject]@{ name = 'pandora-ds-terminal-release-exact-deny'; namespace = 'pandora' }
    spec = [pscustomobject]@{
        selector = [pscustomobject]@{ matchLabels = [pscustomobject]@{ app = 'ds-allocator' } }
        action = 'DENY'
        rules = @((New-DenyRule '*'), (New-DenyRule 'cluster.local/ns/pandora/sa/pandora-battle-result'))
    }
}
$service = [pscustomobject]@{
    metadata = [pscustomobject]@{ name = 'ds-allocator'; namespace = 'pandora' }
    spec = [pscustomobject]@{ ports = @([pscustomobject]@{ name = 'grpc'; appProtocol = 'grpc'; port = 50020 }) }
}
Assert-PandoraDsTerminalMeshPolicyContract $peer $policy $service
$badPolicy = Copy-Object $policy
$badPolicy.spec.rules[1].to[0].operation.paths[0] = '/pandora.ds.v1.DSAllocatorService/Heartbeat'
Assert-Throws { Assert-PandoraDsTerminalMeshPolicyContract $peer $badPolicy $service } 'broadened terminal RPC path'

$runId = 'release-20260713'
$now = [datetimeoffset]::Parse('2026-07-13T03:00:00Z')
$completion = $now.AddMinutes(-1).ToString('o')
$jobName = "pandora-ds-auth-synthetic-v1-$runId-prepare"
$syntheticAnnotations = [pscustomobject]@{
    'pandora.dev/ds-auth-synthetic-contract' = 'v1'
    'pandora.dev/ds-auth-activation-run' = $runId
    'pandora.dev/ds-auth-synthetic-phase' = 'prepare'
    'pandora.dev/ds-auth-synthetic-mode' = 'isolated-no-writes-v1'
    'pandora.dev/image-digest' = $digest
    'pandora.dev/ds-auth-target-epoch' = '2'
    'pandora.dev/ds-auth-keyset-revision' = 'callback-r9'
    'pandora.dev/ds-auth-etcd-identity-revision' = 'r7'
    'sidecar.istio.io/inject' = 'false'
}
$syntheticCommand = @('/pandora/bin/dsauth-synthetic-v1')
$syntheticArgs = @('--contract=v1', '--namespace=pandora', "--run-id=$runId", '--phase=prepare',
    '--mode=isolated-no-writes-v1', '--target-epoch=2', '--keyset-revision=callback-r9',
    '--etcd-identity-revision=r7', '--result-file=/dev/termination-log')
$syntheticEnv = @(
    [pscustomobject]@{ name = 'PANDORA_SYNTHETIC_CONTRACT'; value = 'v1' },
    [pscustomobject]@{ name = 'PANDORA_ACTIVATION_RUN_ID'; value = $runId },
    [pscustomobject]@{ name = 'PANDORA_SYNTHETIC_PHASE'; value = 'prepare' },
    [pscustomobject]@{ name = 'PANDORA_SYNTHETIC_MODE'; value = 'isolated-no-writes-v1' },
    [pscustomobject]@{ name = 'PANDORA_TARGET_WRITER_EPOCH'; value = '2' },
    [pscustomobject]@{ name = 'PANDORA_KEYSET_REVISION'; value = 'callback-r9' },
    [pscustomobject]@{ name = 'PANDORA_ETCD_IDENTITY_REVISION'; value = 'r7' },
    [pscustomobject]@{ name = 'PANDORA_NAMESPACE'; value = 'pandora' }
)
$syntheticContainer = [pscustomobject]@{
    name = 'probe'; image = "registry/pandora/dsauth-synthetic@$digest"
    command = $syntheticCommand; args = $syntheticArgs; env = $syntheticEnv; volumeMounts = @()
    terminationMessagePath = '/dev/termination-log'; terminationMessagePolicy = 'File'
    securityContext = [pscustomobject]@{
        allowPrivilegeEscalation = $false; readOnlyRootFilesystem = $true; runAsNonRoot = $true
        capabilities = [pscustomobject]@{ drop = @('ALL') }
    }
}
$job = [pscustomobject]@{
    metadata = [pscustomobject]@{
        name = $jobName; namespace = 'pandora'; uid = 'job-uid'
        annotations = $syntheticAnnotations
    }
    spec = [pscustomobject]@{
        backoffLimit = 0; activeDeadlineSeconds = 120
        template = [pscustomobject]@{
            metadata = [pscustomobject]@{ annotations = $syntheticAnnotations }
            spec = [pscustomobject]@{
                serviceAccountName = 'pandora-ds-auth-synthetic-v1'; restartPolicy = 'Never'; containers = @($syntheticContainer)
                dnsPolicy = 'ClusterFirst'; automountServiceAccountToken = $false; enableServiceLinks = $false
                securityContext = [pscustomobject]@{ runAsNonRoot = $true; seccompProfile = [pscustomobject]@{ type = 'RuntimeDefault' } }
            }
        }
    }
    status = [pscustomobject]@{
        conditions = @([pscustomobject]@{ type = 'Complete'; status = 'True' })
        succeeded = 1; failed = 0; completionTime = $completion
    }
}
$syntheticPodSpec = Copy-Object $job.spec.template.spec
$syntheticPod = [pscustomobject]@{
    metadata = [pscustomobject]@{
        namespace = 'pandora'; deletionTimestamp = $null; annotations = $syntheticAnnotations
        ownerReferences = @([pscustomobject]@{ kind = 'Job'; uid = 'job-uid'; controller = $true })
    }
    spec = $syntheticPodSpec
    status = [pscustomobject]@{
        phase = 'Succeeded'
        containerStatuses = @([pscustomobject]@{
            name = 'probe'; imageID = "docker-pullable://registry/pandora/dsauth-synthetic@$digest"; restartCount = 0
            state = [pscustomobject]@{ terminated = [pscustomobject]@{
                exitCode = 0; signal = 0; reason = 'Completed'; startedAt = $now.AddMinutes(-2).ToString('o'); finishedAt = $now.AddMinutes(-1).AddSeconds(-5).ToString('o')
                message = (@{ contract = 'v1'; run_id = $runId; phase = 'prepare'; mode = 'isolated-no-writes-v1'; target_epoch = 2; keyset_revision = 'callback-r9'; etcd_identity_revision = 'r7'; result = 'pass' } | ConvertTo-Json -Compress)
            } }
        })
    }
}
Assert-PandoraDsAuthSyntheticContract $job $syntheticPod 'pandora' $runId 'prepare' $digest 2 'callback-r9' 'r7' ([timespan]::FromMinutes(30)) $now.AddMinutes(-5) $now
$oldJob = Copy-Object $job
$oldJob.status.completionTime = $now.AddHours(-2).ToString('o')
Assert-Throws {
    Assert-PandoraDsAuthSyntheticContract $oldJob $syntheticPod 'pandora' $runId 'prepare' $digest 2 'callback-r9' 'r7' ([timespan]::FromMinutes(30)) $now.AddHours(-3) $now
} 'stale synthetic evidence'
$badJob = Copy-Object $job
$badJob.metadata.annotations.'pandora.dev/ds-auth-synthetic-phase' = 'final'
Assert-Throws {
    Assert-PandoraDsAuthSyntheticContract $badJob $syntheticPod 'pandora' $runId 'prepare' $digest 2 'callback-r9' 'r7' ([timespan]::FromMinutes(30)) $now.AddMinutes(-5) $now
} 'synthetic phase mismatch'
$badPod = Copy-Object $syntheticPod
$badPod.spec.containers[0].command = @('/bin/true')
Assert-Throws {
    Assert-PandoraDsAuthSyntheticContract $job $badPod 'pandora' $runId 'prepare' $digest 2 'callback-r9' 'r7' ([timespan]::FromMinutes(30)) $now.AddMinutes(-5) $now
} 'synthetic command bypass'
$badPod = Copy-Object $syntheticPod
$badPod.status.containerStatuses[0].state.terminated.message = '{"contract":"v1","result":"pass"}'
Assert-Throws {
    Assert-PandoraDsAuthSyntheticContract $job $badPod 'pandora' $runId 'prepare' $digest 2 'callback-r9' 'r7' ([timespan]::FromMinutes(30)) $now.AddMinutes(-5) $now
} 'incomplete synthetic termination result'
$badPod = Copy-Object $syntheticPod
$badPod.status.containerStatuses[0].state.terminated.message = (@{ CONTRACT = 'v1'; RUN_ID = $runId; PHASE = 'prepare'; MODE = 'isolated-no-writes-v1'; TARGET_EPOCH = 2; KEYSET_REVISION = 'callback-r9'; ETCD_IDENTITY_REVISION = 'r7'; RESULT = 'pass' } | ConvertTo-Json -Compress)
Assert-Throws {
    Assert-PandoraDsAuthSyntheticContract $job $badPod 'pandora' $runId 'prepare' $digest 2 'callback-r9' 'r7' ([timespan]::FromMinutes(30)) $now.AddMinutes(-5) $now
} 'termination result property names must be canonical case'
$badJob = Copy-Object $job
$badJob.spec.template.spec.containers[0].volumeMounts = @([pscustomobject]@{ name = 'replace-root'; mountPath = '/' })
Assert-Throws {
    Assert-PandoraDsAuthSyntheticContract $badJob $syntheticPod 'pandora' $runId 'prepare' $digest 2 'callback-r9' 'r7' ([timespan]::FromMinutes(30)) $now.AddMinutes(-5) $now
} 'root volume can replace immutable probe binary'
$badJob = Copy-Object $job
$badJob.spec.template.spec | Add-Member -NotePropertyName initContainers -NotePropertyValue @([pscustomobject]@{ name = 'replace-probe' })
Assert-Throws {
    Assert-PandoraDsAuthSyntheticContract $badJob $syntheticPod 'pandora' $runId 'prepare' $digest 2 'callback-r9' 'r7' ([timespan]::FromMinutes(30)) $now.AddMinutes(-5) $now
} 'synthetic init container bypass'
$badPod = Copy-Object $syntheticPod
$badPod.spec.containers += [pscustomobject]@{ name = 'attacker-sidecar'; image = 'attacker@sha256:' + ('f' * 64) }
Assert-Throws {
    Assert-PandoraDsAuthSyntheticContract $job $badPod 'pandora' $runId 'prepare' $digest 2 'callback-r9' 'r7' ([timespan]::FromMinutes(30)) $now.AddMinutes(-5) $now
} 'admission-injected sidecar bypass'
$badPod = Copy-Object $syntheticPod
$badPod.spec | Add-Member -NotePropertyName initContainers -NotePropertyValue @([pscustomobject]@{ name = 'attacker-init' })
$badPod.spec | Add-Member -NotePropertyName hostNetwork -NotePropertyValue $true
Assert-Throws {
    Assert-PandoraDsAuthSyntheticContract $job $badPod 'pandora' $runId 'prepare' $digest 2 'callback-r9' 'r7' ([timespan]::FromMinutes(30)) $now.AddMinutes(-5) $now
} 'observed Pod init/host network bypass'
$badPod = Copy-Object $syntheticPod
$badPod.spec.containers[0].volumeMounts = @([pscustomobject]@{ name = 'attacker-config'; mountPath = '/etc' })
$badPod.spec | Add-Member -NotePropertyName volumes -NotePropertyValue @([pscustomobject]@{ name = 'attacker-config'; configMap = [pscustomobject]@{ name = 'attacker' } })
Assert-Throws {
    Assert-PandoraDsAuthSyntheticContract $job $badPod 'pandora' $runId 'prepare' $digest 2 'callback-r9' 'r7' ([timespan]::FromMinutes(30)) $now.AddMinutes(-5) $now
} 'observed Pod /etc ConfigMap bypass'
$badPod = Copy-Object $syntheticPod
$badPod.metadata.annotations | Add-Member -NotePropertyName 'k8s.v1.cni.cncf.io/networks' -NotePropertyValue 'attacker/default-route'
Assert-Throws {
    Assert-PandoraDsAuthSyntheticContract $job $badPod 'pandora' $runId 'prepare' $digest 2 'callback-r9' 'r7' ([timespan]::FromMinutes(30)) $now.AddMinutes(-5) $now
} 'observed Pod network attachment annotation bypass'
$badJob = Copy-Object $job
$badJob.spec | Add-Member -NotePropertyName parallelism -NotePropertyValue 100
$badJob.spec | Add-Member -NotePropertyName completions -NotePropertyValue 1
Assert-Throws {
    Assert-PandoraDsAuthSyntheticContract $badJob $syntheticPod 'pandora' $runId 'prepare' $digest 2 'callback-r9' 'r7' ([timespan]::FromMinutes(30)) $now.AddMinutes(-5) $now
} 'parallel synthetic winner bypass'
$omitemptyJob = Copy-Object $job
$omitemptyPod = Copy-Object $syntheticPod
$omitemptyJob.status.PSObject.Properties.Remove('failed')
$omitemptyPod.metadata.PSObject.Properties.Remove('deletionTimestamp')
$omitemptyPod.spec.containers[0].PSObject.Properties.Remove('volumeMounts')
$omitemptyPod.status.containerStatuses[0].state.terminated.PSObject.Properties.Remove('signal')
Assert-PandoraDsAuthSyntheticContract $omitemptyJob $omitemptyPod 'pandora' $runId 'prepare' $digest 2 'callback-r9' 'r7' ([timespan]::FromMinutes(30)) $now.AddMinutes(-5) $now

$endpointService = [pscustomobject]@{
    metadata = [pscustomobject]@{ name = 'login'; namespace = 'pandora'; uid = 'service-uid' }
    spec = [pscustomobject]@{ ports = @([pscustomobject]@{ port = 50001; protocol = 'TCP'; targetPort = 50001 }) }
}
$endpointPod = [pscustomobject]@{
    metadata = [pscustomobject]@{ name = 'login-green-abc'; namespace = 'pandora'; uid = 'pod-uid' }
    status = [pscustomobject]@{ podIP = '10.0.0.8'; podIPs = @([pscustomobject]@{ ip = '10.0.0.8' }) }
}
$endpointSlice = [pscustomobject]@{
    metadata = [pscustomobject]@{
        namespace = 'pandora'
        labels = [pscustomobject]@{
            'kubernetes.io/service-name' = 'login'
            'endpointslice.kubernetes.io/managed-by' = 'endpointslice-controller.k8s.io'
        }
        ownerReferences = @([pscustomobject]@{ kind = 'Service'; name = 'login'; uid = 'service-uid'; controller = $true })
    }
    addressType = 'IPv4'
    ports = @([pscustomobject]@{ name = ''; protocol = 'TCP'; port = 50001 })
    endpoints = @([pscustomobject]@{
        addresses = @('10.0.0.8')
        conditions = [pscustomobject]@{ ready = $true; serving = $true; terminating = $false }
        targetRef = [pscustomobject]@{ kind = 'Pod'; namespace = 'pandora'; name = 'login-green-abc'; uid = 'pod-uid' }
    })
}
$sliceList = [pscustomobject]@{ items = @($endpointSlice) }
$verifiedUIDs = Get-PandoraDsAuthVerifiedEndpointUIDSet $sliceList $endpointService @($endpointPod) 'pandora'
Assert-True ($verifiedUIDs.Count -eq 1 -and $verifiedUIDs.Contains('pod-uid')) 'exact controller EndpointSlice maps to exact Pod UID/IP/port'
$badSlices = Copy-Object $sliceList
$badSlices.items[0].endpoints[0].targetRef = $null
Assert-Throws { Get-PandoraDsAuthVerifiedEndpointUIDSet $badSlices $endpointService @($endpointPod) 'pandora' } 'ready endpoint without targetRef'
$badSlices = Copy-Object $sliceList
$badSlices.items[0].endpoints[0].addresses[0] = '203.0.113.8'
Assert-Throws { Get-PandoraDsAuthVerifiedEndpointUIDSet $badSlices $endpointService @($endpointPod) 'pandora' } 'endpoint address outside exact Pod IPs'
$badSlices = Copy-Object $sliceList
$badSlices.items[0].endpoints += Copy-Object $badSlices.items[0].endpoints[0]
Assert-Throws { Get-PandoraDsAuthVerifiedEndpointUIDSet $badSlices $endpointService @($endpointPod) 'pandora' } 'duplicate endpoint address cannot collapse through UID set'
$badSlices = Copy-Object $sliceList
$badSlices.items[0].ports[0].port = 50099
Assert-Throws { Get-PandoraDsAuthVerifiedEndpointUIDSet $badSlices $endpointService @($endpointPod) 'pandora' } 'EndpointSlice target port drift'

Assert-True (@(Assert-PandoraDsAuthHttpsEndpoints 'https://etcd-a.internal:2379,https://etcd-b.internal:2379').Count -eq 2) 'canonical etcd endpoints'
Assert-Throws { Assert-PandoraDsAuthHttpsEndpoints 'http://etcd.internal:2379' } 'plaintext etcd endpoint'
Assert-Throws { Assert-PandoraDsAuthHttpsEndpoints 'https://etcd.internal' } 'etcd endpoint without explicit port'
Assert-Throws { Assert-PandoraDsAuthHttpsEndpoints 'https://etcd.internal:0' } 'zero etcd port'
Assert-Throws { Assert-PandoraDsAuthHttpsEndpoints 'https://etcd.internal:02379' } 'non-canonical etcd port'
Assert-Throws { Assert-PandoraDsAuthHttpsEndpoints 'https://etcd.internal:65536' } 'out-of-range etcd port'
Assert-Throws { Assert-PandoraDsAuthEtcdRevision '7' } 'non-canonical identity revision'
$identityPatch = New-PandoraDsAuthEtcdIdentityPatch 'ds-allocator' 'r7' 'etcd.pandora.internal' '/pandora/acl-negative/' $false
Assert-True ($identityPatch.Contains('secretName: pandora-ds-auth-etcd-ds-allocator-r7')) 'runtime patch exact immutable Secret name'
Assert-True ($identityPatch.Contains('PANDORA_DS_AUTH_ETCD_CLIENT_IDENTITY, value: pandora-ds-allocator')) 'runtime patch exact client identity'
Assert-True ($identityPatch.Contains('PANDORA_DS_AUTH_ETCD_REQUIRE_AUTH, value: "1"')) 'runtime patch auth enabled'
Assert-True (-not $identityPatch.Contains('PANDORA_DS_AUTH_ETCD_PASSWORD_FILE')) 'CN-only runtime patch has no password path'
$passwordPatch = New-PandoraDsAuthEtcdIdentityPatch 'login' 'r7' 'etcd.pandora.internal' '/pandora/acl-negative/' $true
Assert-True ($passwordPatch.Contains('PANDORA_DS_AUTH_ETCD_USERNAME_FILE')) 'optional username/password runtime patch'
Assert-Throws { New-PandoraDsAuthEtcdIdentityPatch 'login' 'r7' 'bad:2379' '/pandora/acl-negative/' $false } 'unsafe TLS server name in YAML patch'
$greenTemplateSpec = Copy-Object $pod.spec
$greenTemplateSpec.containers[0] | Add-Member -NotePropertyName image -NotePropertyValue ('registry/pandora/ds-allocator@' + $digest)
$greenTemplateSpec.containers[0] | Add-Member -NotePropertyName args -NotePropertyValue @('-conf', 'etc/cluster.yaml')
$greenTemplateSpec.containers[0] | Add-Member -NotePropertyName ports -NotePropertyValue @([pscustomobject]@{ name = 'grpc'; containerPort = 50020 })
$greenTemplateSpec.containers[0] | Add-Member -NotePropertyName readinessProbe -NotePropertyValue ([pscustomobject]@{ grpc = [pscustomobject]@{ port = 50020 } })
$greenTemplateSpec.containers[0] | Add-Member -NotePropertyName resources -NotePropertyValue ([pscustomobject]@{ requests = [pscustomobject]@{ cpu = '25m' } })
$liveGreen = [pscustomobject]@{
    apiVersion = 'apps/v1'; kind = 'Deployment'
    metadata = [pscustomobject]@{
        name = 'ds-allocator-ds-auth-green'; namespace = 'pandora'; resourceVersion = '123'
        labels = [pscustomobject]@{ app = 'ds-allocator' }
        annotations = [pscustomobject]@{ 'pandora.dev/ds-auth-green-desired-replicas' = '2' }
    }
    spec = [pscustomobject]@{
        replicas = 2
        selector = [pscustomobject]@{ matchLabels = [pscustomobject]@{
            app = 'ds-allocator'; 'pandora.dev/ds-auth-writer-set' = 'green'; 'pandora.dev/ds-auth-writer-epoch' = '2'
        } }
        strategy = [pscustomobject]@{ type = 'RollingUpdate'; rollingUpdate = [pscustomobject]@{ maxUnavailable = 0; maxSurge = 1 } }
        template = [pscustomobject]@{
            metadata = [pscustomobject]@{
                labels = [pscustomobject]@{
                    app = 'ds-allocator'; 'pandora.dev/ds-auth-writer-set' = 'green'; 'pandora.dev/ds-auth-writer-epoch' = '2'
                    'istio.io/rev' = 'asm-1-22'
                }
                annotations = [pscustomobject]@{
                    'pandora.dev/image-digest' = $digest
                    'sidecar.istio.io/rewriteAppHTTPProbers' = 'true'
                }
            }
            spec = $greenTemplateSpec
        }
    }
}
$newDigest = 'sha256:' + ('b' * 64)
$greenObject = New-PandoraDsAuthCanonicalGreenObject $liveGreen 'ds-allocator' 'r7' `
    'etcd.pandora.internal' '/pandora/acl-negative/' $false 2 ('registry/pandora/ds-allocator@' + $newDigest) $newDigest
Assert-True ($greenObject.metadata.resourceVersion -ceq '123') 'canonical green full object keeps CAS resourceVersion'
Assert-True ($greenObject.spec.replicas -eq 2 -and $greenObject.spec.selector.matchLabels.'pandora.dev/ds-auth-writer-set' -ceq 'green') 'canonical green keeps desired/immutable selector'
$greenContainer = @($greenObject.spec.template.spec.containers | Where-Object name -ceq 'ds-allocator')[0]
Assert-True ($greenContainer.args[0] -ceq '-conf' -and $greenContainer.ports[0].containerPort -eq 50020 -and
    $greenContainer.readinessProbe.grpc.port -eq 50020 -and $greenContainer.resources.requests.cpu -ceq '25m') `
    'canonical green full object keeps args/ports/probes/resources'
Assert-True ($greenObject.spec.template.spec.serviceAccountName -ceq 'pandora-allocator' -and
    $greenObject.spec.template.metadata.labels.'istio.io/rev' -ceq 'asm-1-22' -and
    @($greenObject.spec.template.metadata.labels.PSObject.Properties | Where-Object Name -CEQ 'sidecar.istio.io/inject').Count -eq 0) `
    'canonical green full object keeps SA/single revision selector without dual injector marker'
$badGreen = Copy-Object $liveGreen
$badGreen.spec.selector.matchLabels | Add-Member -NotePropertyName extra -NotePropertyValue smuggled
Assert-Throws {
    New-PandoraDsAuthCanonicalGreenObject $badGreen 'ds-allocator' 'r7' 'etcd.pandora.internal' '/pandora/acl-negative/' $false 2 ('registry/pandora/ds-allocator@' + $newDigest) $newDigest
} 'canonical green rejects broadened immutable selector'
$dormantPatch = New-PandoraDsAuthDormantBluePatch 'ds-allocator'
$greenServicePatch = New-PandoraDsAuthGreenServicePatch 'ds-allocator'
Assert-True ($dormantPatch.Contains('replicas: 0') -and $dormantPatch.Contains('pandora.dev/ds-auth-writer-epoch: "1"')) 'dormant blue fixed epoch=1/zero'
Assert-True ($greenServicePatch.Contains('pandora.dev/ds-auth-writer-set: green')) 'canonical Service fixed green selector'

$activationSource = Get-Content -LiteralPath (Join-Path $ProjectRoot 'tools/scripts/activate_ds_auth.ps1') -Raw
Assert-True ($activationSource.Contains("[ValidateSet('Audit', 'Activate')][string]`$Phase = 'Audit'")) 'Audit must be default'
Assert-True (-not $activationSource.Contains('SyntheticProbeScript')) 'arbitrary synthetic script entry removed'
Assert-True ($activationSource.Contains("'--required-features', `$script:PandoraDsAuthRequiredFeatures")) 'required features must have one library source'
Assert-True ($activationSource.Contains("'--expected-image-digests', `$Audit.ServiceDigests")) `
    'capability audit must bind each service to its own live template digest'
Assert-True ($activationSource.Contains('ServiceDigests = (($serviceDigests.Keys')) `
    'green contract must emit exact service-to-digest mapping'
$startSource = Get-Content -LiteralPath (Join-Path $ProjectRoot 'tools/scripts/start.ps1') -Raw
Assert-True ($startSource.Contains('--expected-image-digests $State.ExpectedDigests')) `
    'ordinary online release final audit must preserve service-level digest binding'
Assert-True (-not $startSource.Contains('--allowed-image-digests $devDigest')) `
    'fresh local bootstrap must not fabricate capability digest evidence'
$auditBranch = $activationSource.IndexOf("if (`$Phase -ceq 'Audit')", [StringComparison]::Ordinal)
$firstApply = $activationSource.IndexOf('Invoke-CapabilityAudit $audit -ApplyEpoch', [StringComparison]::Ordinal)
Assert-True ($auditBranch -ge 0 -and $firstApply -gt $auditBranch) 'CAS apply must be after Audit early exit'
$drain = $activationSource.LastIndexOf('Scale-BlueDeploymentsToZero', [StringComparison]::Ordinal)
$empty = $activationSource.LastIndexOf('Invoke-EmptyCapabilityAudit $candidate', [StringComparison]::Ordinal)
$green = $activationSource.LastIndexOf('Scale-GreenDeploymentsToDesired $candidate', [StringComparison]::Ordinal)
$switch = $activationSource.LastIndexOf('Switch-LiveServicesToGreen', [StringComparison]::Ordinal)
Assert-True ($drain -lt $empty -and $empty -lt $green -and $green -lt $switch -and $switch -lt $firstApply) 'activation must be zero-overlap blue drain -> empty capability -> green -> service -> CAS'
Assert-True ([regex]::Matches($activationSource,
    '@\(\$service\.spec\.selector\.PSObject\.Properties\)\.Count\s+-ne\s+3').Count -ge 3) `
    'preview/live/switch Service gates must reject extra selectors'
Assert-True ($activationSource.Contains("op = 'replace'; path = '/spec/selector'") -and
    $activationSource.Contains("op = 'test'; path = '/metadata/resourceVersion'") -and
    $activationSource.Contains("'--type=json'")) `
    'live Service switch must CAS-replace the complete selector map'
Assert-True (-not $activationSource.Contains("`$selectorPatch = @{ `$WriterSetLabel = 'green'")) `
    'live Service switch must not merge a partial selector'

Write-Host 'ds_auth_activation_contract_test: PASS'
