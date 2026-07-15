# DS callback auth 一次性 epoch 激活的纯契约。
#
# 本文件只验证对象/构造只读客户端参数，不创建 CA、证书、密码、Secret、Job 或 Deployment。
# 生产信任材料和固定 synthetic Job 必须由平台在仓库外预置；证据缺失一律失败。
Set-StrictMode -Version Latest

$script:PandoraDsAuthWriterApps = @('login', 'player-locator', 'ds-allocator', 'hub-allocator', 'battle-result')
$script:PandoraDsAuthRequiredFeatures =
    'hub_allocator=hub-reservation-ledger-v1|hub-heartbeat-capacity-v1|hub-owner-cleanup-v1|hub-physical-eviction-v1,' +
    'ds_allocator=battle-release-expected-tuple-v1,' +
    'battle_result=battle-terminal-outbox-v1'
$script:PandoraDsAuthIdentityMountPath = '/run/secrets/pandora/ds-auth-etcd'

function Assert-PandoraDsAuthEtcdRevision([string]$Revision) {
    if ($Revision -cnotmatch '^r[1-9][0-9]*$') {
        throw "DS auth etcd identity revision 必须是 canonical rN，实际='$Revision'。"
    }
}

function Assert-PandoraDsAuthHttpsEndpoints([string]$Endpoints) {
    $items = @($Endpoints.Split(',') | ForEach-Object { $_.Trim() } | Where-Object { $_ })
    if ($items.Count -eq 0) { throw 'DS auth etcd endpoints 不能为空。' }
    foreach ($endpoint in $items) {
        $uri = $null
        if (-not [Uri]::TryCreate($endpoint, [UriKind]::Absolute, [ref]$uri) -or
            $uri.Scheme -cne 'https' -or [string]::IsNullOrWhiteSpace($uri.Host) -or
            -not [string]::IsNullOrEmpty($uri.UserInfo) -or $uri.AbsolutePath -cne '/' -or
            -not [string]::IsNullOrEmpty($uri.Query) -or -not [string]::IsNullOrEmpty($uri.Fragment) -or
            $endpoint -cnotmatch '^https://(?:\[[0-9A-Fa-f:]+\]|[A-Za-z0-9._-]+):[1-9][0-9]{0,4}$' -or
            $uri.Port -lt 1 -or $uri.Port -gt 65535) {
            throw "生产 DS auth etcd endpoint 必须是 canonical https://host:port，实际='$endpoint'。"
        }
    }
    return $items
}

function Get-PandoraDsAuthIdentitySecretName([string]$App, [string]$Revision) {
    Assert-PandoraDsAuthEtcdRevision $Revision
    if ($script:PandoraDsAuthWriterApps -cnotcontains $App) { throw "未知 DS auth writer app='$App'。" }
    return "pandora-ds-auth-etcd-$App-$Revision"
}

function Get-PandoraDsAuthClientIdentity([string]$App) {
    if ($script:PandoraDsAuthWriterApps -cnotcontains $App) { throw "未知 DS auth writer app='$App'。" }
    return "pandora-$App"
}

function New-PandoraDsAuthEtcdIdentityPatch([string]$App, [string]$Revision,
    [string]$ServerName, [string]$ForbiddenReadPrefix, [bool]$UsesPasswordAuth,
    [string]$DeploymentName = '') {
    $secretName = Get-PandoraDsAuthIdentitySecretName $App $Revision
    $identity = Get-PandoraDsAuthClientIdentity $App
    if ([string]::IsNullOrWhiteSpace($DeploymentName)) { $DeploymentName = $App }
    if ($DeploymentName -cnotmatch '^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$') { throw 'etcd identity patch Deployment name 非法。' }
    if ($ServerName -cnotmatch '^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$') {
        throw 'etcd TLS server name 不能安全写入 runtime patch。'
    }
    if ($ForbiddenReadPrefix -cnotmatch '^/[A-Za-z0-9._/-]{1,240}/$') {
        throw 'etcd forbidden read prefix 必须是 canonical absolute prefix。'
    }
    $passwordEnv = if ($UsesPasswordAuth) {
@"
            - { name: PANDORA_DS_AUTH_ETCD_USERNAME_FILE, value: $script:PandoraDsAuthIdentityMountPath/username }
            - { name: PANDORA_DS_AUTH_ETCD_PASSWORD_FILE, value: $script:PandoraDsAuthIdentityMountPath/password }
"@
    } else { '' }
    return @"
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $DeploymentName
  namespace: pandora
spec:
  template:
    spec:
      securityContext: { runAsNonRoot: true, runAsUser: 10001, runAsGroup: 10001, fsGroup: 10001, fsGroupChangePolicy: OnRootMismatch }
      containers:
        - name: $App
          env:
            - { name: PANDORA_DS_AUTH_ETCD_REQUIRE_MTLS, value: "1" }
            - { name: PANDORA_DS_AUTH_ETCD_CA_FILE, value: $script:PandoraDsAuthIdentityMountPath/ca.crt }
            - { name: PANDORA_DS_AUTH_ETCD_CERT_FILE, value: $script:PandoraDsAuthIdentityMountPath/tls.crt }
            - { name: PANDORA_DS_AUTH_ETCD_KEY_FILE, value: $script:PandoraDsAuthIdentityMountPath/tls.key }
            - { name: PANDORA_DS_AUTH_ETCD_SERVER_NAME, value: $ServerName }
            - { name: PANDORA_DS_AUTH_ETCD_CLIENT_IDENTITY, value: $identity }
            - { name: PANDORA_DS_AUTH_ETCD_IDENTITY_REVISION, value: $Revision }
            - { name: PANDORA_DS_AUTH_ETCD_REQUIRE_AUTH, value: "1" }
            - { name: PANDORA_DS_AUTH_ETCD_FORBIDDEN_READ_PREFIX, value: $ForbiddenReadPrefix }
$passwordEnv          volumeMounts:
            - { name: ds-auth-etcd-identity, mountPath: $script:PandoraDsAuthIdentityMountPath, readOnly: true }
      volumes:
        - name: ds-auth-etcd-identity
          secret: { secretName: $secretName, defaultMode: 0440 }
"@
}

function New-PandoraDsAuthDormantBluePatch([string]$App) {
    if ($script:PandoraDsAuthWriterApps -cnotcontains $App) { throw "未知 DS auth writer app='$App'。" }
    return @"
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $App
  namespace: pandora
spec:
  replicas: 0
  selector:
    matchLabels: { app: $App, pandora.dev/ds-auth-writer-set: blue, pandora.dev/ds-auth-writer-epoch: "1" }
  template:
    metadata:
      labels: { app: $App, pandora.dev/ds-auth-writer-set: blue, pandora.dev/ds-auth-writer-epoch: "1" }
"@
}

function New-PandoraDsAuthGreenServicePatch([string]$App) {
    if ($script:PandoraDsAuthWriterApps -cnotcontains $App) { throw "未知 DS auth writer app='$App'。" }
    return @"
apiVersion: v1
kind: Service
metadata:
  name: $App
  namespace: pandora
spec:
  selector: { app: $App, pandora.dev/ds-auth-writer-set: green, pandora.dev/ds-auth-writer-epoch: "2" }
"@
}

function New-PandoraDsAuthCanonicalGreenObject($LiveDeployment, [string]$App, [string]$Revision,
    [string]$ServerName, [string]$ForbiddenReadPrefix, [bool]$UsesPasswordAuth,
    [ValidateRange(1, 99)][int]$DesiredReplicas, [string]$PinnedImage, [string]$Digest) {
    if ($PinnedImage -cnotmatch ('@' + [regex]::Escape($Digest) + '$') -or $Digest -cnotmatch '^sha256:[0-9a-f]{64}$') {
        throw 'canonical green image 必须固定到同一 immutable digest。'
    }
    $greenName = "$App-ds-auth-green"
    if ([string]$LiveDeployment.apiVersion -cne 'apps/v1' -or [string]$LiveDeployment.kind -cne 'Deployment' -or
        [string]$LiveDeployment.metadata.name -cne $greenName -or [string]$LiveDeployment.metadata.namespace -cne 'pandora' -or
        [string]$LiveDeployment.metadata.resourceVersion -cnotmatch '^[1-9][0-9]*$') {
        throw 'canonical green live Deployment 身份/resourceVersion 非法。'
    }
    if ([string]$LiveDeployment.metadata.annotations.'pandora.dev/ds-auth-green-desired-replicas' -cne [string]$DesiredReplicas -or
        [int]$LiveDeployment.spec.replicas -ne $DesiredReplicas -or
        [string]$LiveDeployment.spec.selector.matchLabels.app -cne $App -or
        [string]$LiveDeployment.spec.selector.matchLabels.'pandora.dev/ds-auth-writer-set' -cne 'green' -or
        [string]$LiveDeployment.spec.selector.matchLabels.'pandora.dev/ds-auth-writer-epoch' -cne '2' -or
        @($LiveDeployment.spec.selector.matchLabels.PSObject.Properties).Count -ne 3) {
        throw 'canonical green live Deployment selector/desired 非 exact contract。'
    }
    $templatePod = [pscustomobject]@{ metadata = $LiveDeployment.spec.template.metadata; spec = $LiveDeployment.spec.template.spec }
    Assert-PandoraDsAuthIdentityPodContract $templatePod $App $Revision $ServerName $ForbiddenReadPrefix $UsesPasswordAuth
    if ($App -in @('battle-result', 'ds-allocator')) { Assert-PandoraDsTerminalMeshTemplateContract $templatePod $App }

    # 从已审计 live 对象构造完整 replace 对象，保留 args/ports/probes/resources/全部 volume、SA、
    # terminal-mesh labels 与 strategy；只替换本次 immutable image/digest。resourceVersion 提供 CAS。
    $spec = (($LiveDeployment.spec | ConvertTo-Json -Depth 40) | ConvertFrom-Json)
    $containers = @($spec.template.spec.containers | Where-Object { [string]$_.name -ceq $App })
    if ($containers.Count -ne 1) { throw 'canonical green live Deployment 缺唯一业务 container。' }
    $containers[0].image = $PinnedImage
    $spec.template.metadata.annotations.'pandora.dev/image-digest' = $Digest
    return [pscustomobject]@{
        apiVersion = 'apps/v1'
        kind = 'Deployment'
        metadata = [pscustomobject]@{
            name = $greenName
            namespace = 'pandora'
            resourceVersion = [string]$LiveDeployment.metadata.resourceVersion
            labels = $LiveDeployment.metadata.labels
            annotations = $LiveDeployment.metadata.annotations
        }
        spec = $spec
    }
}

function Assert-PandoraDsAuthIdentitySecretContract($Secret, [string]$App, [string]$Revision) {
    $expectedName = Get-PandoraDsAuthIdentitySecretName $App $Revision
    $expectedIdentity = Get-PandoraDsAuthClientIdentity $App
    if ([string]$Secret.metadata.name -cne $expectedName) { throw "etcd identity Secret 名称不是 $expectedName。" }
    if ($Secret.immutable -ne $true) { throw "Secret/$expectedName 必须 immutable=true。" }
    if ([string]$Secret.metadata.labels.'pandora.dev/ds-auth-etcd-identity-revision' -cne $Revision) {
        throw "Secret/$expectedName 缺精确 identity revision label。"
    }
    if ([string]$Secret.metadata.labels.'pandora.dev/ds-auth-etcd-client-identity' -cne $expectedIdentity) {
        throw "Secret/$expectedName 缺精确 client identity label。"
    }
    $keys = @($Secret.data.PSObject.Properties.Name | Sort-Object)
    $required = @('ca.crt', 'tls.crt', 'tls.key')
    foreach ($key in $required) {
        if ($keys -cnotcontains $key -or [string]::IsNullOrWhiteSpace([string]$Secret.data.$key)) {
            throw "Secret/$expectedName 缺非空 key=$key。"
        }
    }
    $hasUsername = $keys -ccontains 'username'
    $hasPassword = $keys -ccontains 'password'
    if ($hasUsername -ne $hasPassword) { throw "Secret/$expectedName username/password 必须同时存在或同时缺失。" }
    $allowed = @($required + @('username', 'password'))
    foreach ($key in $keys) {
        if ($allowed -cnotcontains $key) { throw "Secret/$expectedName 含未批准 key=$key。" }
    }
    return [pscustomobject]@{ Name = $expectedName; ClientIdentity = $expectedIdentity; UsesPasswordAuth = $hasUsername }
}

function Get-PandoraNamedObject($Items, [string]$Name, [string]$Kind) {
    $matches = @($Items | Where-Object { [string]$_.name -ceq $Name })
    if ($matches.Count -ne 1) { throw "$Kind name=$Name 必须且只能出现一次，实际=$($matches.Count)。" }
    return $matches[0]
}

function Test-PandoraKubernetesObjectDeleting($Object) {
    if ($null -eq $Object -or $null -eq $Object.metadata -or
        $null -eq $Object.metadata.PSObject.Properties['deletionTimestamp']) {
        return $false
    }
    return -not [string]::IsNullOrWhiteSpace([string]$Object.metadata.deletionTimestamp)
}

function Assert-PandoraDsAuthIdentityPodContract($Pod, [string]$App, [string]$Revision,
    [string]$ServerName, [string]$ForbiddenReadPrefix, [bool]$UsesPasswordAuth) {
    $secretName = Get-PandoraDsAuthIdentitySecretName $App $Revision
    $identity = Get-PandoraDsAuthClientIdentity $App
    if ([string]::IsNullOrWhiteSpace($ServerName)) { throw 'etcd TLS server name 不能为空。' }
    if ([string]::IsNullOrWhiteSpace($ForbiddenReadPrefix)) { throw 'ACL forbidden read prefix 不能为空。' }

    $container = Get-PandoraNamedObject @($Pod.spec.containers) $App 'container'
    $volume = Get-PandoraNamedObject @($Pod.spec.volumes) 'ds-auth-etcd-identity' 'volume'
    if ([string]$volume.secret.secretName -cne $secretName -or [int]$volume.secret.defaultMode -ne 288) {
        throw "$App Pod 未以 defaultMode=0440 挂载精确 Secret/$secretName。"
    }
    $mount = Get-PandoraNamedObject @($container.volumeMounts) 'ds-auth-etcd-identity' 'volumeMount'
    if ([string]$mount.mountPath -cne $script:PandoraDsAuthIdentityMountPath -or $mount.readOnly -ne $true) {
        throw "$App Pod 的 etcd identity volume 必须只读挂载到固定路径。"
    }

    $expected = [ordered]@{
        'PANDORA_DS_AUTH_ETCD_REQUIRE_MTLS'          = '1'
        'PANDORA_DS_AUTH_ETCD_CA_FILE'               = "$script:PandoraDsAuthIdentityMountPath/ca.crt"
        'PANDORA_DS_AUTH_ETCD_CERT_FILE'             = "$script:PandoraDsAuthIdentityMountPath/tls.crt"
        'PANDORA_DS_AUTH_ETCD_KEY_FILE'              = "$script:PandoraDsAuthIdentityMountPath/tls.key"
        'PANDORA_DS_AUTH_ETCD_SERVER_NAME'           = $ServerName
        'PANDORA_DS_AUTH_ETCD_CLIENT_IDENTITY'       = $identity
        'PANDORA_DS_AUTH_ETCD_IDENTITY_REVISION'     = $Revision
        'PANDORA_DS_AUTH_ETCD_REQUIRE_AUTH'           = '1'
        'PANDORA_DS_AUTH_ETCD_FORBIDDEN_READ_PREFIX' = $ForbiddenReadPrefix
    }
    if ($UsesPasswordAuth) {
        $expected['PANDORA_DS_AUTH_ETCD_USERNAME_FILE'] = "$script:PandoraDsAuthIdentityMountPath/username"
        $expected['PANDORA_DS_AUTH_ETCD_PASSWORD_FILE'] = "$script:PandoraDsAuthIdentityMountPath/password"
    }
    foreach ($pair in $expected.GetEnumerator()) {
        $entry = Get-PandoraNamedObject @($container.env) $pair.Key 'env'
        if ([string]$entry.value -cne [string]$pair.Value -or $null -ne $entry.valueFrom) {
            throw "$App Pod env $($pair.Key) 不是固定安全值。"
        }
    }
    foreach ($name in @('PANDORA_DS_AUTH_ETCD_USERNAME_FILE', 'PANDORA_DS_AUTH_ETCD_PASSWORD_FILE')) {
        $entries = @($container.env | Where-Object { [string]$_.name -ceq $name })
        if (-not $UsesPasswordAuth -and $entries.Count -ne 0) { throw "$App Pod 不应声明 $name。" }
    }
}

function Assert-PandoraDsTerminalMeshTemplateContract($Pod, [string]$App) {
    $expectedServiceAccount = switch ($App) {
        'battle-result' { 'pandora-battle-result' }
        'ds-allocator' { 'pandora-allocator' }
        default { throw "terminal mesh 不接受 app=$App。" }
    }
    $labelProperties = @($Pod.metadata.labels.PSObject.Properties)
    $annotationProperties = if ($null -eq $Pod.metadata.annotations) { @() } else { @($Pod.metadata.annotations.PSObject.Properties) }
    $revisionEntries = @($labelProperties | Where-Object Name -CEQ 'istio.io/rev')
    $injectEntries = @($labelProperties + $annotationProperties | Where-Object Name -CEQ 'sidecar.istio.io/inject')
    $rewriteEntries = @($annotationProperties | Where-Object Name -CEQ 'sidecar.istio.io/rewriteAppHTTPProbers')
    if ([string]$Pod.spec.serviceAccountName -cne $expectedServiceAccount -or
        $revisionEntries.Count -ne 1 -or [string]::IsNullOrWhiteSpace([string]$revisionEntries[0].Value) -or
        [string]$revisionEntries[0].Value -ceq 'PANDORA_ISTIO_REVISION_REQUIRED' -or
        $injectEntries.Count -ne 0 -or $rewriteEntries.Count -ne 1 -or
        [string]$rewriteEntries[0].Value -cne 'true') {
        throw "$App Pod 未绑定固定 ServiceAccount/唯一真实 Istio revision/probe rewrite，或含双 injector inject 标记。"
    }
}

function Assert-PandoraDsTerminalMeshPodContract($Pod, [string]$App) {
    Assert-PandoraDsTerminalMeshTemplateContract $Pod $App
    $proxy = Get-PandoraNamedObject @($Pod.status.containerStatuses) 'istio-proxy' 'terminal mesh sidecar'
    if ($proxy.ready -ne $true -or [int]$proxy.restartCount -ne 0) { throw "$App Pod istio-proxy 非稳定 Ready。" }
}

function Assert-PandoraDsTerminalMeshPolicyContract($PeerAuthentication, $AuthorizationPolicy, $Service) {
    if ([string]$PeerAuthentication.metadata.name -cne 'pandora-ds-allocator-terminal-permissive' -or
        [string]$PeerAuthentication.metadata.namespace -cne 'pandora' -or
        [string]$PeerAuthentication.spec.selector.matchLabels.app -cne 'ds-allocator' -or
        [string]$PeerAuthentication.spec.mtls.mode -cne 'PERMISSIVE') {
        throw 'terminal ReleaseBattle PeerAuthentication 实体不匹配。'
    }
    if ([string]$AuthorizationPolicy.metadata.name -cne 'pandora-ds-terminal-release-exact-deny' -or
        [string]$AuthorizationPolicy.metadata.namespace -cne 'pandora' -or
        [string]$AuthorizationPolicy.spec.selector.matchLabels.app -cne 'ds-allocator' -or
        [string]$AuthorizationPolicy.spec.action -cne 'DENY') {
        throw 'terminal ReleaseBattle AuthorizationPolicy 实体不匹配。'
    }
    $rules = @($AuthorizationPolicy.spec.rules)
    if ($rules.Count -ne 2) { throw 'terminal ReleaseBattle DENY 必须恰有两条规则。' }
    $expectedPrincipals = @('*', 'cluster.local/ns/pandora/sa/pandora-battle-result')
    $seen = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::Ordinal)
    foreach ($rule in $rules) {
        $from = @($rule.from)
        $to = @($rule.to)
        if ($from.Count -ne 1 -or $to.Count -ne 1 -or @($from[0].source.notPrincipals).Count -ne 1 -or
            @($to[0].operation.methods).Count -ne 1 -or [string]$to[0].operation.methods[0] -cne 'POST' -or
            @($to[0].operation.paths).Count -ne 1 -or [string]$to[0].operation.paths[0] -cne '/pandora.ds.v1.DSAllocatorService/ReleaseBattle') {
            throw 'terminal ReleaseBattle DENY principal/method/path 不是 exact contract。'
        }
        $null = $seen.Add([string]$from[0].source.notPrincipals[0])
    }
    if ($seen.Count -ne 2) { throw 'terminal ReleaseBattle DENY principal 集不完整。' }
    foreach ($principal in $expectedPrincipals) {
        if (-not $seen.Contains($principal)) { throw "terminal ReleaseBattle 缺 DENY principal=$principal。" }
    }
    $ports = @($Service.spec.ports | Where-Object {
        [string]$_.name -ceq 'grpc' -and [string]$_.appProtocol -ceq 'grpc' -and [int]$_.port -eq 50020
    })
    if ([string]$Service.metadata.name -cne 'ds-allocator' -or [string]$Service.metadata.namespace -cne 'pandora' -or $ports.Count -ne 1) {
        throw 'ds-allocator Service 未暴露 exact grpc/appProtocol=grpc/50020。'
    }
}

function Get-PandoraDsAuthSecureGoArgs([string]$CAFile, [string]$CertFile, [string]$KeyFile,
    [string]$ServerName, [string]$ClientIdentity, [string]$IdentityRevision,
    [string]$ForbiddenReadPrefix, [string]$UsernameFile = '', [string]$PasswordFile = '') {
    Assert-PandoraDsAuthEtcdRevision $IdentityRevision
    foreach ($file in @($CAFile, $CertFile, $KeyFile)) {
        if (-not (Test-Path -LiteralPath $file -PathType Leaf)) { throw 'etcd auditor mTLS 文件缺失。' }
    }
    if ([string]::IsNullOrWhiteSpace($ServerName) -or [string]::IsNullOrWhiteSpace($ClientIdentity) -or
        [string]::IsNullOrWhiteSpace($ForbiddenReadPrefix)) { throw 'etcd auditor server-name/client-identity/forbidden-prefix 均必填。' }
    if ([string]::IsNullOrWhiteSpace($UsernameFile) -ne [string]::IsNullOrWhiteSpace($PasswordFile)) {
        throw 'etcd auditor username/password file 必须同时提供或同时缺失。'
    }
    $args = @('--require-mtls', '--ca-file', (Resolve-Path -LiteralPath $CAFile).Path,
        '--cert-file', (Resolve-Path -LiteralPath $CertFile).Path,
        '--key-file', (Resolve-Path -LiteralPath $KeyFile).Path,
        '--server-name', $ServerName, '--client-identity', $ClientIdentity,
        '--etcd-identity-revision', $IdentityRevision, '--require-auth',
        '--forbidden-read-prefix', $ForbiddenReadPrefix)
    if (-not [string]::IsNullOrWhiteSpace($UsernameFile)) {
        foreach ($file in @($UsernameFile, $PasswordFile)) {
            if (-not (Test-Path -LiteralPath $file -PathType Leaf)) { throw 'etcd auditor username/password 文件缺失。' }
        }
        $args += @('--username-file', (Resolve-Path -LiteralPath $UsernameFile).Path,
            '--password-file', (Resolve-Path -LiteralPath $PasswordFile).Path)
    }
    return $args
}

function Assert-PandoraExactStringList($Actual, [string[]]$Expected, [string]$Where) {
    $items = @($Actual)
    if ($items.Count -ne $Expected.Count) { throw "$Where 数量不匹配。" }
    for ($index = 0; $index -lt $Expected.Count; $index++) {
        if ([string]$items[$index] -cne $Expected[$index]) { throw "$Where 顺序或内容不匹配。" }
    }
}

function Get-PandoraDsAuthVerifiedEndpointUIDSet($SliceList, $Service, $ExpectedPods, [string]$Namespace) {
    if ([string]$Service.metadata.namespace -cne $Namespace -or [string]::IsNullOrWhiteSpace([string]$Service.metadata.name) -or
        [string]::IsNullOrWhiteSpace([string]$Service.metadata.uid)) {
        throw 'Service identity 非 canonical，不能验证 EndpointSlice。'
    }
    $serviceName = [string]$Service.metadata.name
    $expectedByUID = @{}
    $expectedAddressPairs = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::Ordinal)
    foreach ($pod in @($ExpectedPods)) {
        $uid = [string]$pod.metadata.uid
        $name = [string]$pod.metadata.name
        if ([string]$pod.metadata.namespace -cne $Namespace -or [string]::IsNullOrWhiteSpace($uid) -or
            [string]::IsNullOrWhiteSpace($name) -or $expectedByUID.ContainsKey($uid)) {
            throw "Service/$serviceName expected Pod identity 非 canonical/重复。"
        }
        $deleting = $null
        if ($null -ne $pod.metadata.PSObject.Properties['deletionTimestamp']) { $deleting = $pod.metadata.deletionTimestamp }
        if ($null -ne $deleting) { throw "Service/$serviceName expected Pod 正在删除。" }
        $podIPs = @()
        if ($null -ne $pod.status.PSObject.Properties['podIPs']) {
            $podIPs = @($pod.status.podIPs | ForEach-Object { [string]$_.ip })
        } elseif ($null -ne $pod.status.PSObject.Properties['podIP']) {
            $podIPs = @([string]$pod.status.podIP)
        }
        if ($podIPs.Count -eq 0) { throw "Service/$serviceName expected Pod 缺 podIP。" }
        $expectedByUID[$uid] = [pscustomobject]@{ Name = $name; IPs = $podIPs }
        foreach ($ip in $podIPs) { $null = $expectedAddressPairs.Add("$uid|$ip") }
    }

    $expectedPorts = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::Ordinal)
    foreach ($port in @($Service.spec.ports)) {
        $name = ''
        if ($null -ne $port.PSObject.Properties['name']) { $name = [string]$port.name }
        $protocol = if ($null -ne $port.PSObject.Properties['protocol']) { [string]$port.protocol } else { 'TCP' }
        $target = [string]$port.targetPort
        if ($target -cnotmatch '^[1-9][0-9]{0,4}$' -or [int]$target -gt 65535 -or
            -not $expectedPorts.Add("$name|$protocol|$target")) {
            throw "Service/$serviceName port contract 非 canonical/重复。"
        }
    }
    if ($expectedPorts.Count -eq 0) { throw "Service/$serviceName 无可验证端口。" }

    $seenUIDs = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::Ordinal)
    $seenAddressPairs = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::Ordinal)
    foreach ($slice in @($SliceList.items)) {
        if ([string]$slice.metadata.namespace -cne $Namespace -or
            [string]$slice.metadata.labels.'kubernetes.io/service-name' -cne $serviceName -or
            [string]$slice.metadata.labels.'endpointslice.kubernetes.io/managed-by' -cne 'endpointslice-controller.k8s.io' -or
            [string]$slice.addressType -notin @('IPv4', 'IPv6')) {
            throw "Service/$serviceName EndpointSlice identity/manager/addressType 非 canonical。"
        }
        $owners = @($slice.metadata.ownerReferences | Where-Object {
            [string]$_.kind -ceq 'Service' -and [string]$_.name -ceq $serviceName -and
            [string]$_.uid -ceq [string]$Service.metadata.uid -and $_.controller -eq $true
        })
        if ($owners.Count -ne 1 -or @($slice.metadata.ownerReferences).Count -ne 1) {
            throw "Service/$serviceName EndpointSlice owner 非唯一真实 Service。"
        }
        $slicePorts = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::Ordinal)
        foreach ($port in @($slice.ports)) {
            $name = ''
            if ($null -ne $port.PSObject.Properties['name']) { $name = [string]$port.name }
            $protocol = if ($null -ne $port.PSObject.Properties['protocol']) { [string]$port.protocol } else { 'TCP' }
            $value = [string]$port.port
            if (-not $slicePorts.Add("$name|$protocol|$value")) { throw "Service/$serviceName EndpointSlice port 重复。" }
        }
        if ($slicePorts.Count -ne $expectedPorts.Count) { throw "Service/$serviceName EndpointSlice port 数不匹配。" }
        foreach ($portKey in $expectedPorts) {
            if (-not $slicePorts.Contains($portKey)) { throw "Service/$serviceName EndpointSlice port 被篡改。" }
        }
        foreach ($endpoint in @($slice.endpoints)) {
            $serving = $true
            if ($null -ne $endpoint.conditions.PSObject.Properties['serving']) { $serving = [bool]$endpoint.conditions.serving }
            $terminating = $false
            if ($null -ne $endpoint.conditions.PSObject.Properties['terminating']) { $terminating = [bool]$endpoint.conditions.terminating }
            if ($endpoint.conditions.ready -ne $true -or -not $serving -or $terminating -or $null -eq $endpoint.targetRef) {
                throw "Service/$serviceName 含非 Ready/Serving 或无 Pod targetRef 的 Endpoint。"
            }
            $uid = [string]$endpoint.targetRef.uid
            $expectedPod = $expectedByUID[$uid]
            if ($null -eq $expectedPod -or [string]$endpoint.targetRef.kind -cne 'Pod' -or
                [string]$endpoint.targetRef.namespace -cne $Namespace -or
                [string]$endpoint.targetRef.name -cne [string]$expectedPod.Name) {
                throw "Service/$serviceName Endpoint targetRef 不属于 exact green Pod。"
            }
            $addresses = @($endpoint.addresses)
            if ($addresses.Count -eq 0) { throw "Service/$serviceName Endpoint 缺地址。" }
            foreach ($address in $addresses) {
                $pair = "$uid|$([string]$address)"
                if (-not $expectedAddressPairs.Contains($pair) -or -not $seenAddressPairs.Add($pair)) {
                    throw "Service/$serviceName Endpoint 地址不属于 Pod 或重复。"
                }
            }
            $null = $seenUIDs.Add($uid)
        }
    }
    if ($seenUIDs.Count -ne $expectedByUID.Count) { throw "Service/$serviceName Endpoint UID 数不匹配。" }
    foreach ($uid in $expectedByUID.Keys) {
        if (-not $seenUIDs.Contains([string]$uid)) { throw "Service/$serviceName 缺 expected Pod Endpoint。" }
    }
    return ,$seenUIDs
}

function Assert-PandoraDsAuthSyntheticProbeContainer($Container, [string]$Namespace,
    [string]$RunId, [string]$Phase, [string]$Mode, [uint32]$TargetEpoch,
    [string]$KeysetRevision, [string]$EtcdIdentityRevision) {
    Assert-PandoraExactStringList @($Container.command) @('/pandora/bin/dsauth-synthetic-v1') 'synthetic command'
    $expectedArgs = @(
        '--contract=v1', "--namespace=$Namespace", "--run-id=$RunId", "--phase=$Phase", "--mode=$Mode",
        "--target-epoch=$TargetEpoch", "--keyset-revision=$KeysetRevision",
        "--etcd-identity-revision=$EtcdIdentityRevision", '--result-file=/dev/termination-log'
    )
    Assert-PandoraExactStringList @($Container.args) $expectedArgs 'synthetic args'
    if ([string]$Container.terminationMessagePath -cne '/dev/termination-log' -or
        [string]$Container.terminationMessagePolicy -cne 'File') {
        throw 'synthetic termination result 必须来自固定 /dev/termination-log 文件。'
    }
    $expectedEnv = [ordered]@{
        'PANDORA_SYNTHETIC_CONTRACT' = 'v1'
        'PANDORA_ACTIVATION_RUN_ID' = $RunId
        'PANDORA_SYNTHETIC_PHASE' = $Phase
        'PANDORA_SYNTHETIC_MODE' = $Mode
        'PANDORA_TARGET_WRITER_EPOCH' = [string]$TargetEpoch
        'PANDORA_KEYSET_REVISION' = $KeysetRevision
        'PANDORA_ETCD_IDENTITY_REVISION' = $EtcdIdentityRevision
        'PANDORA_NAMESPACE' = $Namespace
    }
    $envItems = @($Container.env)
    if ($envItems.Count -ne $expectedEnv.Count) { throw 'synthetic env 必须是 exact contract 集。' }
    foreach ($pair in $expectedEnv.GetEnumerator()) {
        $matches = @($envItems | Where-Object { [string]$_.name -ceq [string]$pair.Key })
        if ($matches.Count -ne 1 -or [string]$matches[0].value -cne [string]$pair.Value -or
            $null -ne $matches[0].PSObject.Properties['valueFrom']) {
            throw "synthetic env $($pair.Key) 不匹配或不是固定 literal。"
        }
    }
    $mounts = @()
    if ($null -ne $Container.PSObject.Properties['volumeMounts']) { $mounts = @($Container.volumeMounts) }
    if ($mounts.Count -ne 0) { throw 'synthetic probe 不允许任何 volumeMount。' }
    $envFrom = @()
    if ($null -ne $Container.PSObject.Properties['envFrom']) { $envFrom = @($Container.envFrom) }
    if ($envFrom.Count -ne 0) { throw 'synthetic probe 不允许 envFrom 注入。' }
    if ($null -ne $Container.PSObject.Properties['lifecycle']) { throw 'synthetic probe 不允许 lifecycle hook。' }
    $security = $Container.securityContext
    $drop = @($security.capabilities.drop)
    $add = @()
    if ($null -ne $security.capabilities.PSObject.Properties['add']) { $add = @($security.capabilities.add) }
    $privileged = $false
    if ($null -ne $security.PSObject.Properties['privileged']) { $privileged = [bool]$security.privileged }
    if (@($security.PSObject.Properties).Count -ne 4 -or
        @($security.capabilities.PSObject.Properties).Count -ne 1 -or
        $security.allowPrivilegeEscalation -ne $false -or $privileged -or
        $security.readOnlyRootFilesystem -ne $true -or $security.runAsNonRoot -ne $true -or
        $drop.Count -ne 1 -or [string]$drop[0] -cne 'ALL' -or $add.Count -ne 0) {
        throw 'synthetic probe 必须 non-root/read-only/no-escalation/drop ALL。'
    }
}

function Assert-PandoraDsAuthSyntheticPodSpec($Spec, [string]$Where) {
    $containers = @($Spec.containers)
    $initContainers = @()
    if ($null -ne $Spec.PSObject.Properties['initContainers']) { $initContainers = @($Spec.initContainers) }
    $ephemeralContainers = @()
    if ($null -ne $Spec.PSObject.Properties['ephemeralContainers']) { $ephemeralContainers = @($Spec.ephemeralContainers) }
    $volumes = @()
    if ($null -ne $Spec.PSObject.Properties['volumes']) { $volumes = @($Spec.volumes) }
    $hostAliases = @()
    if ($null -ne $Spec.PSObject.Properties['hostAliases']) { $hostAliases = @($Spec.hostAliases) }
    foreach ($flag in @('hostNetwork', 'hostPID', 'hostIPC', 'shareProcessNamespace', 'setHostnameAsFQDN')) {
        if ($null -ne $Spec.PSObject.Properties[$flag] -and [bool]$Spec.$flag) {
            throw "$Where 不允许 $flag。"
        }
    }
    foreach ($field in @('dnsConfig', 'hostname', 'subdomain')) {
        if ($null -ne $Spec.PSObject.Properties[$field]) { throw "$Where 不允许 $field。" }
    }
    if ($containers.Count -ne 1 -or [string]$containers[0].name -cne 'probe' -or
        $initContainers.Count -ne 0 -or $ephemeralContainers.Count -ne 0 -or $volumes.Count -ne 0 -or
        $hostAliases.Count -ne 0 -or [string]$Spec.serviceAccountName -cne 'pandora-ds-auth-synthetic-v1' -or
        [string]$Spec.restartPolicy -cne 'Never' -or [string]$Spec.dnsPolicy -cne 'ClusterFirst' -or
        $Spec.automountServiceAccountToken -ne $false -or $Spec.enableServiceLinks -ne $false -or
        @($Spec.securityContext.PSObject.Properties).Count -ne 2 -or
        $Spec.securityContext.runAsNonRoot -ne $true -or
        [string]$Spec.securityContext.seccompProfile.type -cne 'RuntimeDefault' -or
        @($Spec.securityContext.seccompProfile.PSObject.Properties).Count -ne 1) {
        throw "$Where 必须是无注入容器/卷/host 绕过的 exact isolated Pod spec。"
    }
}

function Assert-PandoraDsAuthSyntheticContract($Job, $Pod, [string]$Namespace,
    [string]$RunId, [ValidateSet('prepare', 'final')][string]$Phase,
    [string]$ImageDigest, [uint32]$TargetEpoch,
    [string]$KeysetRevision, [string]$EtcdIdentityRevision, [timespan]$MaxAge,
    [datetimeoffset]$NotBefore = [datetimeoffset]::MinValue,
    [datetimeoffset]$Now = [datetimeoffset]::UtcNow) {
    if ($RunId -cnotmatch '^[a-z0-9][a-z0-9-]{7,23}$') { throw 'synthetic activation run id 非法。' }
    if ($ImageDigest -cnotmatch '^sha256:[0-9a-f]{64}$') { throw 'synthetic image 必须是 immutable sha256 digest。' }
    Assert-PandoraDsAuthEtcdRevision $EtcdIdentityRevision
    if ($MaxAge -le [timespan]::Zero -or $MaxAge -gt [timespan]::FromHours(1)) { throw 'synthetic MaxAge 必须在 (0,1h]。' }
    $expectedJobName = "pandora-ds-auth-synthetic-v1-$RunId-$Phase"
    if ([string]$Job.metadata.name -cne $expectedJobName -or [string]$Job.metadata.namespace -cne $Namespace) {
        throw "只接受固定 namespace 内 Job/$expectedJobName。"
    }
    $annotations = $Job.metadata.annotations
    $syntheticMode = if ($Phase -ceq 'prepare') { 'isolated-no-writes-v1' } else { 'live-final-v1' }
    $expectedAnnotations = [ordered]@{
        'pandora.dev/ds-auth-synthetic-contract'      = 'v1'
        'pandora.dev/ds-auth-activation-run'          = $RunId
        'pandora.dev/ds-auth-synthetic-phase'         = $Phase
        'pandora.dev/ds-auth-synthetic-mode'          = $syntheticMode
        'pandora.dev/image-digest'                    = $ImageDigest
        'pandora.dev/ds-auth-target-epoch'            = [string]$TargetEpoch
        'pandora.dev/ds-auth-keyset-revision'         = $KeysetRevision
        'pandora.dev/ds-auth-etcd-identity-revision'  = $EtcdIdentityRevision
        'sidecar.istio.io/inject'                    = 'false'
    }
    foreach ($pair in $expectedAnnotations.GetEnumerator()) {
        if ([string]$annotations.($pair.Key) -cne [string]$pair.Value) { throw "synthetic Job annotation $($pair.Key) 不匹配。" }
        if ([string]$Job.spec.template.metadata.annotations.($pair.Key) -cne [string]$pair.Value -or
            [string]$Pod.metadata.annotations.($pair.Key) -cne [string]$pair.Value) {
            throw "synthetic template/Pod annotation $($pair.Key) 不匹配。"
        }
    }
    $expectedAnnotationNames = @($expectedAnnotations.Keys)
    foreach ($source in @(
        [pscustomobject]@{ Value = $Job.metadata.annotations; Where = 'Job'; AllowRuntime = $false },
        [pscustomobject]@{ Value = $Job.spec.template.metadata.annotations; Where = 'Job template'; AllowRuntime = $false },
        [pscustomobject]@{ Value = $Pod.metadata.annotations; Where = 'observed Pod'; AllowRuntime = $true }
    )) {
        foreach ($property in @($source.Value.PSObject.Properties)) {
            if ($expectedAnnotationNames -ccontains [string]$property.Name) { continue }
            $runtimeCNI = @('cni.projectcalico.org/containerID', 'cni.projectcalico.org/podIP',
                'cni.projectcalico.org/podIPs', 'k8s.v1.cni.cncf.io/network-status')
            if ($source.AllowRuntime -and $runtimeCNI -ccontains [string]$property.Name) { continue }
            throw "synthetic $($source.Where) 含未批准 annotation=$($property.Name)。"
        }
        foreach ($expectedName in $expectedAnnotationNames) {
            if (@($source.Value.PSObject.Properties.Name) -cnotcontains [string]$expectedName) {
                throw "synthetic $($source.Where) annotation 大小写/字段集不精确。"
            }
        }
    }
    $jobPodSpec = $Job.spec.template.spec
    Assert-PandoraDsAuthSyntheticPodSpec $jobPodSpec 'synthetic Job template'
    Assert-PandoraDsAuthSyntheticPodSpec $Pod.spec 'synthetic observed Pod'
    $parallelism = if ($null -ne $Job.spec.PSObject.Properties['parallelism']) { [int]$Job.spec.parallelism } else { 1 }
    $completions = if ($null -ne $Job.spec.PSObject.Properties['completions']) { [int]$Job.spec.completions } else { 1 }
    $completionMode = if ($null -ne $Job.spec.PSObject.Properties['completionMode']) { [string]$Job.spec.completionMode } else { 'NonIndexed' }
    $manualSelector = $false
    if ($null -ne $Job.spec.PSObject.Properties['manualSelector']) { $manualSelector = [bool]$Job.spec.manualSelector }
    $suspend = $false
    if ($null -ne $Job.spec.PSObject.Properties['suspend']) { $suspend = [bool]$Job.spec.suspend }
    if ([int]$Job.spec.backoffLimit -ne 0 -or [int]$Job.spec.activeDeadlineSeconds -lt 1 -or
        $parallelism -ne 1 -or $completions -ne 1 -or $completionMode -cne 'NonIndexed' -or
        $manualSelector -or $suspend -or $null -ne $Job.spec.PSObject.Properties['successPolicy'] -or
        $null -ne $Job.spec.PSObject.Properties['podFailurePolicy'] -or
        [int]$Job.spec.activeDeadlineSeconds -gt 300) {
        throw 'synthetic Job 必须使用固定 SA、Never、backoff=0、deadline<=300s。'
    }
    $complete = @($Job.status.conditions | Where-Object { $_.type -ceq 'Complete' -and $_.status -ceq 'True' })
    $failed = @($Job.status.conditions | Where-Object { $_.type -ceq 'Failed' -and $_.status -ceq 'True' })
    $failedCount = 0
    if ($null -ne $Job.status.PSObject.Properties['failed']) { $failedCount = [int]$Job.status.failed }
    if ($complete.Count -ne 1 -or $failed.Count -ne 0 -or [int]$Job.status.succeeded -ne 1 -or $failedCount -ne 0) {
        throw 'synthetic Job 未唯一成功或曾失败。'
    }
    $completion = [datetimeoffset]::Parse([string]$Job.status.completionTime)
    if ($completion -lt $NotBefore -or $completion -gt $Now -or ($Now - $completion) -gt $MaxAge) {
        throw 'synthetic Job 成功证据早于门槛、已过期或来自未来。'
    }

    $deletionTimestamp = $null
    if ($null -ne $Pod.metadata.PSObject.Properties['deletionTimestamp']) { $deletionTimestamp = $Pod.metadata.deletionTimestamp }
    if ([string]$Pod.metadata.namespace -cne $Namespace -or [string]$Pod.status.phase -cne 'Succeeded' -or
        $null -ne $deletionTimestamp) { throw 'synthetic Pod 非固定 namespace 的稳定 Succeeded Pod。' }
    $owners = @($Pod.metadata.ownerReferences | Where-Object { $_.kind -ceq 'Job' -and $_.uid -ceq $Job.metadata.uid -and $_.controller -eq $true })
    if ($owners.Count -ne 1) { throw 'synthetic Pod owner UID 与固定 Job 不一致。' }
    $jobContainers = @($Job.spec.template.spec.containers)
    if ($jobContainers.Count -ne 1) { throw 'synthetic Job template 必须且只能声明 probe container。' }
    $jobContainer = Get-PandoraNamedObject $jobContainers 'probe' 'synthetic Job container'
    $container = Get-PandoraNamedObject @($Pod.spec.containers) 'probe' 'synthetic Pod container'
    foreach ($probe in @($jobContainer, $container)) {
        if ([string]$probe.image -cnotmatch ('@' + [regex]::Escape($ImageDigest) + '$')) { throw 'synthetic probe image 未固定到批准 digest。' }
        Assert-PandoraDsAuthSyntheticProbeContainer $probe $Namespace $RunId $Phase $syntheticMode `
            $TargetEpoch $KeysetRevision $EtcdIdentityRevision
    }
    $status = Get-PandoraNamedObject @($Pod.status.containerStatuses) 'probe' 'synthetic containerStatus'
    if (@($Pod.status.containerStatuses).Count -ne 1) { throw 'synthetic Pod 不允许额外 containerStatus。' }
    if ([int]$status.restartCount -ne 0 -or -not ([string]$status.imageID).EndsWith($ImageDigest, [StringComparison]::Ordinal)) {
        throw 'synthetic probe imageID/restartCount 不满足固定成功证据。'
    }
    $terminated = $status.state.terminated
    $signal = 0
    if ($null -ne $terminated -and $null -ne $terminated.PSObject.Properties['signal']) { $signal = [int]$terminated.signal }
    if ($null -eq $terminated -or [int]$terminated.exitCode -ne 0 -or $signal -ne 0 -or
        [string]$terminated.reason -cne 'Completed') {
        throw 'synthetic probe 没有固定的 exitCode=0/Completed 终态。'
    }
    $startedAt = [datetimeoffset]::Parse([string]$terminated.startedAt)
    $finishedAt = [datetimeoffset]::Parse([string]$terminated.finishedAt)
    if ($startedAt -gt $finishedAt -or $finishedAt -gt $completion -or ($completion - $finishedAt) -gt [timespan]::FromMinutes(2)) {
        throw 'synthetic probe 终态时间与 Job completion 不一致。'
    }
    $terminationMessage = ''
    if ($null -ne $terminated.PSObject.Properties['message']) { $terminationMessage = [string]$terminated.message }
    try { $result = $terminationMessage | ConvertFrom-Json -ErrorAction Stop }
    catch { throw 'synthetic probe 未返回结构化 termination result。' }
    $expectedResult = [ordered]@{
        contract = 'v1'; run_id = $RunId; phase = $Phase; mode = $syntheticMode
        target_epoch = [string]$TargetEpoch; keyset_revision = $KeysetRevision
        etcd_identity_revision = $EtcdIdentityRevision; result = 'pass'
    }
    if (@($result.PSObject.Properties).Count -ne $expectedResult.Count) { throw 'synthetic termination result 字段集不精确。' }
    foreach ($pair in $expectedResult.GetEnumerator()) {
        if (@($result.PSObject.Properties.Name) -cnotcontains [string]$pair.Key -or
            [string]$result.($pair.Key) -cne [string]$pair.Value) {
            throw "synthetic termination result $($pair.Key) 大小写或值不匹配。"
        }
    }
}
