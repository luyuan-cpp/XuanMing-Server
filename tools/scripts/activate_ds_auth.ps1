# Pandora DS callback auth epoch=2 激活器。
#
# 默认只审计；以下是预留的 Apply 流程（当前 -Apply 在任何写入前机械报错，待决策批准后才可启用）：
#   1) 给 pandora/default namespace 打 required epoch=2 标签，激活准入防回滚；
#   2) 把五个 Service selector 收到 writer epoch=2；
#   3) 在重新审计 K8s/Redis/synthetic 后，用 etcd CAS 把 required_writer_epoch 1→2。
# 注意：镜像/Secret 不可变、blue-green 行为激活与仓内 synthetic 决策批准前，本工具只是
# fail-closed 审计骨架，不代表生产激活已闭环。脚本不生成、不接收真实 secret。
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$KubeContext,
    [Parameter(Mandatory = $true)][string]$EtcdEndpoints,
    [Parameter(Mandatory = $true)][string]$RedisAddrs,
    [Parameter(Mandatory = $true)][string]$KeysetRevision,
    [Parameter(Mandatory = $true)][string[]]$AllowedImageDigests,
    [string]$RedisPasswordEnv = 'PANDORA_REDIS_PASSWORD',
    [string]$PandoraNamespace = 'pandora',
    [string]$DSNamespace = 'default',
    [string]$EtcdPrefix = '/pandora/ds-auth/',
    [uint32]$ExpectedEpoch = 1,
    [uint32]$TargetEpoch = 2,
    [ValidateRange(3, 100)][int]$RedisAuditCycles = 3,
    [timespan]$RedisAuditInterval = '00:00:06',
    [Parameter(Mandatory = $true)][string]$SyntheticProbeScript,
    [switch]$Apply
)

$ErrorActionPreference = 'Stop'
$ProjectRoot = (Resolve-Path "$PSScriptRoot/../..").Path
$WriterLabel = 'pandora.dev/ds-auth-writer-epoch'
$RequiredLabel = 'pandora.dev/ds-auth-required-epoch'
$DigestAnnotation = 'pandora.dev/image-digest'
$Apps = @('login', 'player-locator', 'ds-allocator', 'hub-allocator', 'battle-result')
$CapabilityNames = @{
    'login' = 'login'; 'player-locator' = 'player_locator'; 'ds-allocator' = 'ds_allocator'
    'hub-allocator' = 'hub_allocator'; 'battle-result' = 'battle_result'
}

function Invoke-Kubectl([string[]]$Args) {
    $output = & kubectl --context $KubeContext @Args 2>&1
    if ($LASTEXITCODE -ne 0) { throw "kubectl failed: $($output -join [Environment]::NewLine)" }
    return $output
}

function Assert-Digest([string]$Digest, [string]$Where) {
    if ($Digest -cnotmatch '^sha256:[0-9a-f]{64}$') { throw "$Where 缺 immutable sha256 digest annotation。" }
    if ($AllowedImageDigests -cnotcontains $Digest) { throw "$Where digest=$Digest 不在显式 allowed 清单。" }
}

function Test-PodReady($Pod) {
    return @($Pod.status.conditions | Where-Object { $_.type -eq 'Ready' -and $_.status -eq 'True' }).Count -eq 1
}

function Get-BackendAudit {
    $serviceCounts = [ordered]@{}
    $serviceUIDs = [ordered]@{}
    $allDigests = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::Ordinal)
    foreach ($app in $Apps) {
        $deployment = (Invoke-Kubectl @('-n', $PandoraNamespace, 'get', 'deployment', $app, '-o', 'json')) -join "`n" | ConvertFrom-Json
        $want = [int]$deployment.spec.replicas
        if ([int]$deployment.status.updatedReplicas -ne $want -or [int]$deployment.status.readyReplicas -ne $want -or
            [int]$deployment.status.availableReplicas -ne $want -or [int]$deployment.status.unavailableReplicas -ne 0) {
            throw "$app rollout 未稳定 updated/ready/available=$want。"
        }
        $pods = @(((Invoke-Kubectl @('-n', $PandoraNamespace, 'get', 'pods', '-l', "app=$app", '-o', 'json')) -join "`n" | ConvertFrom-Json).items)
        if ($pods.Count -ne $want) { throw "$app Pod 数=$($pods.Count), expected=$want（旧 Pod/终止中 Pod 必须为 0）。" }
        $uids = [System.Collections.Generic.List[string]]::new()
        foreach ($pod in $pods) {
            if ($null -ne $pod.metadata.deletionTimestamp -or -not (Test-PodReady $pod)) { throw "$app/$($pod.metadata.name) 非稳定 Ready。" }
            if ($pod.metadata.labels.$WriterLabel -cne '2') { throw "$app/$($pod.metadata.name) writer label 不是 2。" }
            $digest = [string]$pod.metadata.annotations.$DigestAnnotation
            Assert-Digest $digest "$app/$($pod.metadata.name)"
            $null = $allDigests.Add($digest)
            $mainStatus = @($pod.status.containerStatuses | Where-Object name -eq $app)
            if ($mainStatus.Count -ne 1 -or -not ([string]$mainStatus[0].imageID).EndsWith($digest, [StringComparison]::Ordinal)) {
                throw "$app/$($pod.metadata.name) imageID 未命中 annotation digest。"
            }
            $uids.Add([string]$pod.metadata.uid)
        }
        $readyEndpointUIDs = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::Ordinal)
        $slices = @(((Invoke-Kubectl @('-n', $PandoraNamespace, 'get', 'endpointslices', '-l', "kubernetes.io/service-name=$app", '-o', 'json')) -join "`n" | ConvertFrom-Json).items)
        foreach ($slice in $slices) {
            foreach ($endpoint in @($slice.endpoints)) {
                if ($endpoint.conditions.ready -eq $true -and $null -ne $endpoint.targetRef.uid) { $null = $readyEndpointUIDs.Add([string]$endpoint.targetRef.uid) }
            }
        }
        if (($uids | Where-Object { -not $readyEndpointUIDs.Contains($_) }).Count -ne 0 -or $readyEndpointUIDs.Count -ne $uids.Count) {
            throw "$app EndpointSlice UID 集与 Ready Pod UID 集不一致。"
        }
        $capName = $CapabilityNames[$app]
        $serviceCounts[$capName] = $want
        $serviceUIDs[$capName] = (@($uids | Sort-Object) -join '|')
    }
    return @{
        Services = (($serviceCounts.Keys | ForEach-Object { "$_=$($serviceCounts[$_])" }) -join ',')
        Instances = (($serviceUIDs.Keys | ForEach-Object { "$_=$($serviceUIDs[$_])" }) -join ',')
        Digests = (@($allDigests | Sort-Object) -join ',')
    }
}

function Assert-GameServers {
    $pods = @(((Invoke-Kubectl @('-n', $DSNamespace, 'get', 'pods', '-l', 'agones.dev/role=gameserver', '-o', 'json')) -join "`n" | ConvertFrom-Json).items)
    if ($pods.Count -eq 0) { throw '未发现 Agones GameServer Pod；无法证明 UE DS writer epoch=2。' }
    foreach ($pod in $pods) {
        if ($null -ne $pod.metadata.deletionTimestamp -or -not (Test-PodReady $pod)) { throw "GameServer $($pod.metadata.name) 非稳定 Ready。" }
        if ($pod.metadata.labels.$WriterLabel -cne '2') { throw "GameServer $($pod.metadata.name) writer label 不是 2。" }
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

function Get-GoAuditArgs($Audit) {
    return @('run', './pkg/dsauthfence/cmd/dsauth-activate', '--endpoints', $EtcdEndpoints,
        '--prefix', $EtcdPrefix, '--expected-services', $Audit.Services, '--expected-instances', $Audit.Instances,
        '--expected-epoch', "$ExpectedEpoch", '--target-epoch', "$TargetEpoch", '--keyset-revision', $KeysetRevision,
        '--allowed-image-digests', $Audit.Digests)
}

function Invoke-SyntheticProbe {
    $probe = (Resolve-Path -LiteralPath $SyntheticProbeScript).Path
    & pwsh -NoProfile -File $probe -KubeContext $KubeContext -PandoraNamespace $PandoraNamespace -DSNamespace $DSNamespace
    if ($LASTEXITCODE -ne 0) { throw '集群内 :8444 synthetic probe 失败。' }
}

if (-not (Get-Command kubectl -ErrorAction SilentlyContinue)) { throw 'kubectl 不可用。' }
if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw 'go 不可用。' }
if (-not (Test-Path -LiteralPath $SyntheticProbeScript -PathType Leaf)) { throw '必须提供真实 synthetic probe 脚本，不能用人工 ACK 代替。' }
if ($TargetEpoch -ne 2 -or $ExpectedEpoch -ge $TargetEpoch) { throw '当前工具只允许单调 1→2 激活。' }
if ($Apply) {
    throw '生产 Apply 暂停：required epoch 尚未接入业务行为门，blue/green 与 immutable image/Secret/synthetic 决策未批准；当前只允许审计。'
}
foreach ($digest in $AllowedImageDigests) { Assert-Digest $digest 'allowed-image-digests' }

Push-Location $ProjectRoot
try {
    $audit = Get-BackendAudit
    Assert-GameServers
    Invoke-RedisAudit
    Invoke-SyntheticProbe
    $goArgs = Get-GoAuditArgs $audit
    if (-not $Apply) {
        & go @goArgs
        if ($LASTEXITCODE -ne 0) { throw 'etcd capability 审计失败。' }
        Write-Host '[AUDIT ONLY] 全部门通过；未修改 namespace/Service/etcd。-Apply 当前被机械锁死，待 blue/green 与部署子决策批准后另行启用。' -ForegroundColor Yellow
        exit 0
    }

    # 任何 K8s 写前先线性读取 etcd required + exact capability。未来 epoch/异常状态先失败，
    # 禁止旧版激活器先把 namespace/Service 防线覆盖回 2 再发现 etcd 已前进。
    & go @goArgs
    if ($LASTEXITCODE -ne 0) { throw 'K8s 写前 etcd capability/required 审计失败。' }

    # 先激活 admission 防回滚；随后立即重审，捕捉审计与打标签间新出现的旧 Pod。
    foreach ($ns in @($PandoraNamespace, $DSNamespace)) {
        $namespace = (Invoke-Kubectl @('get', 'namespace', $ns, '-o', 'json')) -join "`n" | ConvertFrom-Json
        $currentRaw = [string]$namespace.metadata.labels.$RequiredLabel
        if ($currentRaw -and ([uint32]$currentRaw -gt $TargetEpoch -or [uint32]$currentRaw -lt $ExpectedEpoch)) {
            throw "namespace/$ns required epoch=$currentRaw 禁止删除/回退/跨代覆盖。"
        }
        $patch = '{"metadata":{"resourceVersion":"' + $namespace.metadata.resourceVersion + '","labels":{"' + $RequiredLabel + '":"' + $TargetEpoch + '"}}}'
        Invoke-Kubectl @('patch', 'namespace', $ns, '--type=merge', '-p', $patch) | Out-Null
    }
    $audit = Get-BackendAudit
    Assert-GameServers
    $goArgs = Get-GoAuditArgs $audit

    # selector 收到 v2；原 app selector 保留，JSON merge patch 只新增 capability 维度。
    foreach ($app in $Apps) {
        $service = (Invoke-Kubectl @('-n', $PandoraNamespace, 'get', 'service', $app, '-o', 'json')) -join "`n" | ConvertFrom-Json
        $currentRaw = [string]$service.spec.selector.$WriterLabel
        if ($currentRaw -and ([uint32]$currentRaw -gt $TargetEpoch -or [uint32]$currentRaw -lt $ExpectedEpoch)) {
            throw "service/$app writer selector=$currentRaw 禁止回退/跨代覆盖。"
        }
        $patch = '{"metadata":{"resourceVersion":"' + $service.metadata.resourceVersion + '"},"spec":{"selector":{"' + $WriterLabel + '":"' + $TargetEpoch + '"}}}'
        Invoke-Kubectl @('-n', $PandoraNamespace, 'patch', 'service', $app, '--type=merge', '-p', $patch) | Out-Null
    }
    $audit = Get-BackendAudit
    Assert-GameServers
    Invoke-RedisAudit
    Invoke-SyntheticProbe
    $goArgs = Get-GoAuditArgs $audit

    $applyArgs = @($goArgs + '--apply')
    & go @applyArgs
    if ($LASTEXITCODE -ne 0) { throw 'etcd required_writer_epoch CAS 未完成；保留 v2-only 安全状态，禁止回退旧 Pod。' }
    Write-Host '[ OK ] DS auth required_writer_epoch 已机械激活为 2。' -ForegroundColor Green
}
finally {
    Pop-Location
}
