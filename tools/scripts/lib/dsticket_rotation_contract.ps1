# DSTicket K1 -> K2 不停服轮换的纯契约 helper。
# 本文件不访问集群、不写文件；真实脚本只把 kubectl 回读对象交给这里做 fail-closed 校验。

Set-StrictMode -Version Latest

$keysetContract = Join-Path $PSScriptRoot 'dsticket_keyset_contract.ps1'
if (-not (Get-Command Assert-PandoraDSTicketKubernetesObjects -ErrorAction SilentlyContinue)) {
    . $keysetContract
}

$script:PandoraDSTicketFleetNames = @(
    'pandora-battle-stable',
    'pandora-battle-canary',
    'pandora-hub-stable',
    'pandora-hub-canary'
)
$script:PandoraDSTicketSignerNames = @('login', 'matchmaker', 'matchmaker-pve', 'hub-allocator')
$script:PandoraDSTicketMaxTTLSeconds = 180
$script:PandoraDSTicketLeewaySeconds = 15
$script:PandoraDSTicketRetireBufferSeconds = 30
$script:PandoraDSTicketRetireWaitSeconds = 225
$script:PandoraDSTicketOperationLockName = 'pandora-dsticket-operation-lock'

function Get-PandoraRotationProperty {
    param($Object, [Parameter(Mandatory = $true)][string]$Name, $Default = $null)
    if ($null -eq $Object) { return $Default }
    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property) { return $Default }
    return $property.Value
}

function Get-PandoraRotationNamedItems {
    param($Items, [Parameter(Mandatory = $true)][string]$Name)
    return @(@($Items) | Where-Object { [string](Get-PandoraRotationProperty $_ 'name') -ceq $Name })
}

function Get-PandoraDSTicketSignerSecretReferences {
    param([Parameter(Mandatory = $true)]$Spec)
    $references = [System.Collections.Generic.List[object]]::new()
    # 所有非 JWKS 的 pandora-dsticket* Secret 名均属于保留私钥域；规范层再要求 exact target。
    $pattern = '^pandora-dsticket(?!-jwks(?:-|$))'
    foreach ($volume in @(Get-PandoraRotationProperty $Spec 'volumes' @())) {
        $volumeName = [string](Get-PandoraRotationProperty $volume 'name')
        $directName = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $volume 'secret') 'secretName')
        if ($directName -imatch $pattern) {
            $references.Add([pscustomobject]@{ Kind = 'DirectVolume'; Name = $directName; Location = "volume/$volumeName" })
        }
        foreach ($source in @(Get-PandoraRotationProperty (Get-PandoraRotationProperty $volume 'projected') 'sources' @())) {
            $projectedName = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $source 'secret') 'name')
            if ($projectedName -imatch $pattern) {
                $references.Add([pscustomobject]@{ Kind = 'ProjectedVolume'; Name = $projectedName; Location = "volume/$volumeName/projected" })
            }
        }
    }
    foreach ($container in @(
        @(Get-PandoraRotationProperty $Spec 'containers' @()) +
        @(Get-PandoraRotationProperty $Spec 'initContainers' @()) +
        @(Get-PandoraRotationProperty $Spec 'ephemeralContainers' @())
    )) {
        $containerName = [string](Get-PandoraRotationProperty $container 'name')
        foreach ($env in @(Get-PandoraRotationProperty $container 'env' @())) {
            $secretName = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty `
                (Get-PandoraRotationProperty $env 'valueFrom') 'secretKeyRef') 'name')
            if ($secretName -imatch $pattern) {
                $references.Add([pscustomobject]@{
                    Kind = 'EnvSecretKeyRef'; Name = $secretName
                    Location = "container/$containerName/env/$([string](Get-PandoraRotationProperty $env 'name'))"
                })
            }
        }
        foreach ($envFrom in @(Get-PandoraRotationProperty $container 'envFrom' @())) {
            $secretName = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $envFrom 'secretRef') 'name')
            if ($secretName -imatch $pattern) {
                $references.Add([pscustomobject]@{ Kind = 'EnvFromSecretRef'; Name = $secretName; Location = "container/$containerName/envFrom" })
            }
        }
    }
    return [object[]]$references.ToArray()
}

function Get-PandoraDSTicketSignerConfigSecretReferences {
    param([Parameter(Mandatory = $true)]$Spec)
    $references = [System.Collections.Generic.List[object]]::new()
    $pattern = '^pandora-config(?:$|-dsticket(?:-|$))'
    foreach ($volume in @(Get-PandoraRotationProperty $Spec 'volumes' @())) {
        $volumeName = [string](Get-PandoraRotationProperty $volume 'name')
        $directName = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $volume 'secret') 'secretName')
        if ($directName -imatch $pattern) {
            $references.Add([pscustomobject]@{ Kind = 'DirectVolume'; Name = $directName; Location = "volume/$volumeName" })
        }
        foreach ($source in @(Get-PandoraRotationProperty (Get-PandoraRotationProperty $volume 'projected') 'sources' @())) {
            $projectedName = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $source 'secret') 'name')
            if ($projectedName -imatch $pattern) {
                $references.Add([pscustomobject]@{ Kind = 'ProjectedVolume'; Name = $projectedName; Location = "volume/$volumeName/projected" })
            }
        }
    }
    foreach ($containerGroup in @(
        [pscustomobject]@{ Kind = 'container'; Items = @(Get-PandoraRotationProperty $Spec 'containers' @()) },
        [pscustomobject]@{ Kind = 'initContainer'; Items = @(Get-PandoraRotationProperty $Spec 'initContainers' @()) },
        [pscustomobject]@{ Kind = 'ephemeralContainer'; Items = @(Get-PandoraRotationProperty $Spec 'ephemeralContainers' @()) }
    )) {
        foreach ($container in @($containerGroup.Items)) {
            $containerName = [string](Get-PandoraRotationProperty $container 'name')
            foreach ($env in @(Get-PandoraRotationProperty $container 'env' @())) {
                $secretName = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty `
                    (Get-PandoraRotationProperty $env 'valueFrom') 'secretKeyRef') 'name')
                if ($secretName -imatch $pattern) {
                    $references.Add([pscustomobject]@{
                        Kind = 'EnvSecretKeyRef'; Name = $secretName
                        Location = "$($containerGroup.Kind)/$containerName/env/$([string](Get-PandoraRotationProperty $env 'name'))"
                    })
                }
            }
            foreach ($envFrom in @(Get-PandoraRotationProperty $container 'envFrom' @())) {
                $secretName = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $envFrom 'secretRef') 'name')
                if ($secretName -imatch $pattern) {
                    $references.Add([pscustomobject]@{
                        Kind = 'EnvFromSecretRef'; Name = $secretName
                        Location = "$($containerGroup.Kind)/$containerName/envFrom"
                    })
                }
            }
        }
    }
    return [object[]]$references.ToArray()
}

function Get-PandoraDSTicketForbiddenPrivateReferences {
    param([Parameter(Mandatory = $true)]$Spec)
    $references = [System.Collections.Generic.List[object]]::new()
    $forbiddenEnvPattern = '^PANDORA_(?:DS_TICKET_SECRET|JWT_SECRET|PLAYER_JWT_SECRET|DSTICKET_(?:SECRET|HMAC|PRIVATE(?:_KEY)?|SIGNING(?:_KEY)?))$'
    $forbiddenValuePattern = '-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----|"kty"\s*:\s*"oct"|/run/secrets/pandora-dsticket|/etc/pandora/dsticket/private'
    $forbiddenKeyPattern = '(?:^|[-_./])(?:private(?:[-_]?key)?(?:\.pem)?|signing(?:[-_]?key)?|jwt(?:[-_]?secret)?|hmac|oct)(?:$|[-_./])'
    foreach ($containerGroup in @(
        [pscustomobject]@{ Kind = 'Container'; Items = @(Get-PandoraRotationProperty $Spec 'containers' @()) },
        [pscustomobject]@{ Kind = 'InitContainer'; Items = @(Get-PandoraRotationProperty $Spec 'initContainers' @()) },
        [pscustomobject]@{ Kind = 'EphemeralContainer'; Items = @(Get-PandoraRotationProperty $Spec 'ephemeralContainers' @()) }
    )) {
        foreach ($container in @($containerGroup.Items)) {
            $containerName = [string](Get-PandoraRotationProperty $container 'name')
            foreach ($env in @(Get-PandoraRotationProperty $container 'env' @())) {
                $envName = [string](Get-PandoraRotationProperty $env 'name')
                $envValue = [string](Get-PandoraRotationProperty $env 'value')
                if ($envName -imatch $forbiddenEnvPattern) {
                    $references.Add([pscustomobject]@{
                        Kind = "Forbidden$($containerGroup.Kind)EnvName"; Name = $envName
                        Location = "$($containerGroup.Kind)/$containerName/env/$envName"
                    })
                }
                if ($envValue -imatch $forbiddenValuePattern) {
                    $references.Add([pscustomobject]@{
                        Kind = "Forbidden$($containerGroup.Kind)EnvValue"; Name = $envName
                        Location = "$($containerGroup.Kind)/$containerName/env/$envName"
                    })
                }
                $secretKey = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty `
                    (Get-PandoraRotationProperty $env 'valueFrom') 'secretKeyRef') 'key')
                if ($secretKey -imatch $forbiddenKeyPattern) {
                    $references.Add([pscustomobject]@{
                        Kind = "Forbidden$($containerGroup.Kind)SecretKey"; Name = $secretKey
                        Location = "$($containerGroup.Kind)/$containerName/env/$envName/secretKeyRef"
                    })
                }
            }
            foreach ($mount in @(Get-PandoraRotationProperty $container 'volumeMounts' @())) {
                $mountPath = [string](Get-PandoraRotationProperty $mount 'mountPath')
                if ($mountPath -imatch '/run/secrets/pandora-dsticket|/etc/pandora/dsticket/private') {
                    $references.Add([pscustomobject]@{
                        Kind = "Forbidden$($containerGroup.Kind)Mount"; Name = [string](Get-PandoraRotationProperty $mount 'name')
                        Location = "$($containerGroup.Kind)/$containerName/mount/$mountPath"
                    })
                }
            }
        }
    }
    foreach ($volume in @(Get-PandoraRotationProperty $Spec 'volumes' @())) {
        $volumeName = [string](Get-PandoraRotationProperty $volume 'name')
        foreach ($item in @(Get-PandoraRotationProperty (Get-PandoraRotationProperty $volume 'secret') 'items' @())) {
            $key = [string](Get-PandoraRotationProperty $item 'key')
            $path = [string](Get-PandoraRotationProperty $item 'path')
            if ($key -imatch $forbiddenKeyPattern -or $path -imatch $forbiddenKeyPattern) {
                $references.Add([pscustomobject]@{
                    Kind = 'ForbiddenSecretVolumeItem'; Name = if ($key) { $key } else { $path }
                    Location = "volume/$volumeName/secret/items"
                })
            }
        }
        foreach ($source in @(Get-PandoraRotationProperty (Get-PandoraRotationProperty $volume 'projected') 'sources' @())) {
            foreach ($item in @(Get-PandoraRotationProperty (Get-PandoraRotationProperty $source 'secret') 'items' @())) {
                $key = [string](Get-PandoraRotationProperty $item 'key')
                $path = [string](Get-PandoraRotationProperty $item 'path')
                if ($key -imatch $forbiddenKeyPattern -or $path -imatch $forbiddenKeyPattern) {
                    $references.Add([pscustomobject]@{
                        Kind = 'ForbiddenProjectedSecretItem'; Name = if ($key) { $key } else { $path }
                        Location = "volume/$volumeName/projected/secret/items"
                    })
                }
            }
        }
        $secretProviderClass = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty `
            (Get-PandoraRotationProperty $volume 'csi') 'volumeAttributes') 'secretProviderClass')
        if ($secretProviderClass -imatch 'dsticket') {
            $references.Add([pscustomobject]@{
                Kind = 'ForbiddenCSIVolume'; Name = $secretProviderClass
                Location = "volume/$volumeName/csi"
            })
        }
    }
    return [object[]]$references.ToArray()
}

function Get-PandoraDSTicketVerifierMaterialReferences {
    param([Parameter(Mandatory = $true)]$Spec)
    $references = [System.Collections.Generic.List[object]]::new()
    # pandora-dsticket-jwks* 为保留公钥域；r0/r01/backup 等畸形名也必须先进入 related gate。
    $configMapPattern = '^pandora-dsticket-jwks(?:-|$)'
    foreach ($volume in @(Get-PandoraRotationProperty $Spec 'volumes' @())) {
        $volumeName = [string](Get-PandoraRotationProperty $volume 'name')
        $directName = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $volume 'configMap') 'name')
        if ($volumeName -ceq 'dsticket-jwks' -or $directName -cmatch $configMapPattern) {
            $references.Add([pscustomobject]@{ Kind = 'ConfigMapVolume'; Name = $directName; Location = "volume/$volumeName" })
        }
        $directSecretName = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $volume 'secret') 'secretName')
        if ($directSecretName -cmatch $configMapPattern) {
            $references.Add([pscustomobject]@{ Kind = 'JWKSSecretVolume'; Name = $directSecretName; Location = "volume/$volumeName" })
        }
        $secretProviderClass = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty `
            (Get-PandoraRotationProperty $volume 'csi') 'volumeAttributes') 'secretProviderClass')
        if ($secretProviderClass -imatch 'dsticket') {
            $references.Add([pscustomobject]@{ Kind = 'VerifierCSI'; Name = $secretProviderClass; Location = "volume/$volumeName/csi" })
        }
        foreach ($source in @(Get-PandoraRotationProperty (Get-PandoraRotationProperty $volume 'projected') 'sources' @())) {
            $projectedName = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $source 'configMap') 'name')
            if ($projectedName -cmatch $configMapPattern) {
                $references.Add([pscustomobject]@{ Kind = 'ProjectedConfigMap'; Name = $projectedName; Location = "volume/$volumeName/projected" })
            }
            $projectedSecretName = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $source 'secret') 'name')
            if ($projectedSecretName -cmatch $configMapPattern) {
                $references.Add([pscustomobject]@{ Kind = 'ProjectedJWKSSecret'; Name = $projectedSecretName; Location = "volume/$volumeName/projected" })
            }
        }
    }
    foreach ($containerGroup in @(
        [pscustomobject]@{ Kind = 'container'; Items = @(Get-PandoraRotationProperty $Spec 'containers' @()) },
        [pscustomobject]@{ Kind = 'initContainer'; Items = @(Get-PandoraRotationProperty $Spec 'initContainers' @()) },
        [pscustomobject]@{ Kind = 'ephemeralContainer'; Items = @(Get-PandoraRotationProperty $Spec 'ephemeralContainers' @()) }
    )) {
        foreach ($container in @($containerGroup.Items)) {
            $containerName = [string](Get-PandoraRotationProperty $container 'name')
            $containerLocation = "$($containerGroup.Kind)/$containerName"
            if ($containerName -cin @('pandora-battle-ds', 'pandora-hub-ds')) {
                $references.Add([pscustomobject]@{ Kind = 'ManagedContainer'; Name = $containerName; Location = $containerLocation })
            }
            foreach ($mount in @(Get-PandoraRotationProperty $container 'volumeMounts' @())) {
                if ([string](Get-PandoraRotationProperty $mount 'name') -ceq 'dsticket-jwks' -or
                    [string](Get-PandoraRotationProperty $mount 'mountPath') -imatch '/(?:etc|run)/pandora/dsticket') {
                    $references.Add([pscustomobject]@{ Kind = 'VerifierMount'; Name = [string](Get-PandoraRotationProperty $mount 'name'); Location = "$containerLocation/mount" })
                }
            }
            foreach ($env in @(Get-PandoraRotationProperty $container 'env' @())) {
                $envName = [string](Get-PandoraRotationProperty $env 'name')
                $configName = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty `
                    (Get-PandoraRotationProperty $env 'valueFrom') 'configMapKeyRef') 'name')
                $secretName = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty `
                    (Get-PandoraRotationProperty $env 'valueFrom') 'secretKeyRef') 'name')
                if ($envName -cmatch '^PANDORA_DSTICKET_' -or $configName -cmatch $configMapPattern -or $secretName -cmatch $configMapPattern) {
                    $references.Add([pscustomobject]@{
                        Kind = 'VerifierEnv'; Name = if ($configName) { $configName } else { $secretName }
                        Location = "$containerLocation/env/$envName"
                    })
                }
            }
            foreach ($envFrom in @(Get-PandoraRotationProperty $container 'envFrom' @())) {
                $configName = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $envFrom 'configMapRef') 'name')
                $secretName = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $envFrom 'secretRef') 'name')
                if ($configName -cmatch $configMapPattern -or $secretName -cmatch $configMapPattern) {
                    $references.Add([pscustomobject]@{
                        Kind = 'VerifierEnvFrom'; Name = if ($configName) { $configName } else { $secretName }
                        Location = "$containerLocation/envFrom"
                    })
                }
            }
        }
    }
    return [object[]]$references.ToArray()
}

function Test-PandoraDSTicketSignerPodSpecReferenceContract {
    param(
        [Parameter(Mandatory = $true)]$Spec,
        [Parameter(Mandatory = $true)][string]$ServiceName,
        [Parameter(Mandatory = $true)][string]$ExpectedConfigSecret,
        [Parameter(Mandatory = $true)][string]$ExpectedSignerSecret,
        [AllowEmptyString()][string]$ExpectedLoginJwks = '',
        [Parameter(Mandatory = $true)][string]$ObjectName
    )
    $failures = [System.Collections.Generic.List[string]]::new()
    $configReferences = @(Get-PandoraDSTicketSignerConfigSecretReferences $Spec)
    if ($configReferences.Count -ne 1 -or $configReferences[0].Kind -cne 'DirectVolume' -or
        $configReferences[0].Location -cne 'volume/conf' -or $configReferences[0].Name -cne $ExpectedConfigSecret) {
        $failures.Add("$ObjectName config 引用必须恰为一个 DirectVolume:volume/conf:$ExpectedConfigSecret；检测到 $($configReferences.Count) 个")
    }
    $signerReferences = @(Get-PandoraDSTicketSignerSecretReferences $Spec)
    if ($signerReferences.Count -ne 1 -or $signerReferences[0].Kind -cne 'DirectVolume' -or
        $signerReferences[0].Location -cne 'volume/dsticket' -or $signerReferences[0].Name -cne $ExpectedSignerSecret) {
        $failures.Add("$ObjectName signer 私钥引用必须恰为一个 DirectVolume:volume/dsticket:$ExpectedSignerSecret；检测到 $($signerReferences.Count) 个")
    }

    $containers = @(Get-PandoraRotationProperty $Spec 'containers' @())
    $mainContainers = @(Get-PandoraRotationNamedItems $containers $ServiceName)
    if ($mainContainers.Count -ne 1) { $failures.Add("$ObjectName 主容器 $ServiceName 数量=$($mainContainers.Count)") }
    $relatedMounts = [System.Collections.Generic.List[object]]::new()
    foreach ($containerGroup in @(
        [pscustomobject]@{ Kind = 'container'; Items = $containers },
        [pscustomobject]@{ Kind = 'initContainer'; Items = @(Get-PandoraRotationProperty $Spec 'initContainers' @()) },
        [pscustomobject]@{ Kind = 'ephemeralContainer'; Items = @(Get-PandoraRotationProperty $Spec 'ephemeralContainers' @()) }
    )) {
        foreach ($container in @($containerGroup.Items)) {
            $containerName = [string](Get-PandoraRotationProperty $container 'name')
            foreach ($mount in @(Get-PandoraRotationProperty $container 'volumeMounts' @())) {
                $mountName = [string](Get-PandoraRotationProperty $mount 'name')
                $mountPath = [string](Get-PandoraRotationProperty $mount 'mountPath')
                if ($mountName -cin @('conf', 'dsticket', 'dsticket-jwks') -or
                    $mountPath -cin @('/app/etc/cluster.yaml', '/run/secrets/pandora-dsticket', '/run/config/pandora-dsticket')) {
                    $relatedMounts.Add([pscustomobject]@{
                        Group = $containerGroup.Kind; Container = $containerName; Name = $mountName; Path = $mountPath
                        SubPath = [string](Get-PandoraRotationProperty $mount 'subPath')
                        ReadOnly = (Get-PandoraRotationProperty $mount 'readOnly' $false)
                    })
                }
            }
        }
    }
    $confMounts = @($relatedMounts | Where-Object {
        $_.Group -ceq 'container' -and $_.Container -ceq $ServiceName -and $_.Name -ceq 'conf' -and
        $_.Path -ceq '/app/etc/cluster.yaml' -and $_.SubPath -ceq "$ServiceName.yaml" -and $_.ReadOnly -eq $true
    })
    $signerMounts = @($relatedMounts | Where-Object {
        $_.Group -ceq 'container' -and $_.Container -ceq $ServiceName -and $_.Name -ceq 'dsticket' -and
        $_.Path -ceq '/run/secrets/pandora-dsticket' -and [string]::IsNullOrWhiteSpace($_.SubPath) -and $_.ReadOnly -eq $true
    })
    $jwksMounts = @($relatedMounts | Where-Object {
        $_.Group -ceq 'container' -and $_.Container -ceq $ServiceName -and $_.Name -ceq 'dsticket-jwks' -and
        $_.Path -ceq '/run/config/pandora-dsticket' -and [string]::IsNullOrWhiteSpace($_.SubPath) -and $_.ReadOnly -eq $true
    })
    $expectedMountCount = if ($ServiceName -ceq 'login') { 3 } else { 2 }
    if ($confMounts.Count -ne 1 -or $signerMounts.Count -ne 1 -or
        ($ServiceName -ceq 'login' -and $jwksMounts.Count -ne 1) -or
        ($ServiceName -cne 'login' -and $jwksMounts.Count -ne 0) -or
        $relatedMounts.Count -ne $expectedMountCount) {
        $failures.Add("$ObjectName signer consumer mount 必须只由主容器 $ServiceName 使用 canonical conf/dsticket$(if ($ServiceName -ceq 'login') { '/jwks' } else { '' })；检测到 related=$($relatedMounts.Count)")
    }

    $verifierReferences = @(Get-PandoraDSTicketVerifierMaterialReferences $Spec)
    if ($ServiceName -ceq 'login') {
        $allowedVerifier = @($verifierReferences | Where-Object {
            ($_.Kind -ceq 'ConfigMapVolume' -and $_.Name -ceq $ExpectedLoginJwks -and $_.Location -ceq 'volume/dsticket-jwks') -or
            ($_.Kind -ceq 'VerifierMount' -and $_.Name -ceq 'dsticket-jwks' -and $_.Location -ceq 'container/login/mount')
        })
        if ([string]::IsNullOrWhiteSpace($ExpectedLoginJwks) -or $verifierReferences.Count -ne 2 -or $allowedVerifier.Count -ne 2) {
            $failures.Add("$ObjectName Login verifier 只能是 target direct volume + 主容器 canonical mount；检测到 $($verifierReferences.Count) 个引用")
        }
    } elseif ($verifierReferences.Count -ne 0) {
        $failures.Add("$ObjectName 非 Login signer 禁止 verifier 引用；检测到 $($verifierReferences.Count) 个")
    }
    $forbiddenReferences = @(Get-PandoraDSTicketForbiddenPrivateReferences $Spec)
    $canonicalPrivateMount = @($forbiddenReferences | Where-Object {
        $_.Kind -ceq 'ForbiddenContainerMount' -and $_.Name -ceq 'dsticket' -and
        $_.Location -ceq "Container/$ServiceName/mount//run/secrets/pandora-dsticket"
    })
    if ($forbiddenReferences.Count -ne 1 -or $canonicalPrivateMount.Count -ne 1) {
        $failures.Add("$ObjectName 检测到 canonical 主容器 mount 之外的 signer/private/oct/CSI 输入")
    }
    return @($failures)
}

function Test-PandoraDSTicketDSPodSpecClue {
    param([Parameter(Mandatory = $true)]$Spec)
    return @(Get-PandoraDSTicketVerifierMaterialReferences $Spec).Count -gt 0 -or
        @(Get-PandoraDSTicketSignerSecretReferences $Spec).Count -gt 0 -or
        @(Get-PandoraDSTicketForbiddenPrivateReferences $Spec).Count -gt 0
}

function New-PandoraDSTicketOperationLockObject {
    param(
        [Parameter(Mandatory = $true)][ValidatePattern('^[0-9a-f]{32}$')][string]$HolderId,
        [Parameter(Mandatory = $true)][ValidateSet('ordinary-online', 'rotation-stage', 'rotation-promote', 'rotation-retire')][string]$Operation
    )
    return [ordered]@{
        apiVersion = 'v1'
        kind = 'ConfigMap'
        immutable = $true
        metadata = [ordered]@{
            name = $script:PandoraDSTicketOperationLockName
            namespace = 'pandora'
            labels = [ordered]@{
                'app.kubernetes.io/part-of' = 'pandora'
                'app.kubernetes.io/component' = 'dsticket-operation-lock'
            }
            annotations = [ordered]@{
                'pandora.dev/dsticket-lock-holder-id' = $HolderId
                'pandora.dev/dsticket-lock-operation' = $Operation
            }
        }
        data = [ordered]@{ warning = 'Fail-closed operation lock. A stale lock must be audited and removed manually.' }
    }
}

function Assert-PandoraDSTicketOperationLockContract {
    param(
        [Parameter(Mandatory = $true)]$LockObject,
        [Parameter(Mandatory = $true)][ValidatePattern('^[0-9a-f]{32}$')][string]$HolderId,
        [Parameter(Mandatory = $true)][ValidateSet('ordinary-online', 'rotation-stage', 'rotation-promote', 'rotation-retire')][string]$Operation,
        [switch]$RequireLiveIdentity
    )
    $metadata = Get-PandoraRotationProperty $LockObject 'metadata'
    if ([string](Get-PandoraRotationProperty $LockObject 'kind') -cne 'ConfigMap' -or
        [string](Get-PandoraRotationProperty $metadata 'name') -cne $script:PandoraDSTicketOperationLockName -or
        [string](Get-PandoraRotationProperty $metadata 'namespace') -cne 'pandora' -or
        (Get-PandoraRotationProperty $LockObject 'immutable' $false) -ne $true -or
        -not [string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty $metadata 'deletionTimestamp'))) {
        throw "DSTicket 操作锁必须是未删除的 immutable pandora/ConfigMap/$script:PandoraDSTicketOperationLockName。"
    }
    $labels = Get-PandoraRotationProperty $metadata 'labels'
    if ([string](Get-PandoraRotationProperty $labels 'app.kubernetes.io/part-of') -cne 'pandora' -or
        [string](Get-PandoraRotationProperty $labels 'app.kubernetes.io/component') -cne 'dsticket-operation-lock') {
        throw 'DSTicket 操作锁 labels 漂移。'
    }
    $annotations = Get-PandoraRotationProperty $metadata 'annotations'
    if ([string](Get-PandoraRotationProperty $annotations 'pandora.dev/dsticket-lock-holder-id') -cne $HolderId -or
        [string](Get-PandoraRotationProperty $annotations 'pandora.dev/dsticket-lock-operation') -cne $Operation) {
        throw 'DSTicket 操作锁 holder/operation 与当前进程不一致。'
    }
    if ($RequireLiveIdentity) {
        foreach ($field in @('uid', 'resourceVersion', 'creationTimestamp')) {
            if ([string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty $metadata $field))) {
                throw "DSTicket 操作锁缺 apiserver metadata.$field。"
            }
        }
    }
    return [pscustomobject]@{
        HolderId = $HolderId
        Operation = $Operation
        Uid = [string](Get-PandoraRotationProperty $metadata 'uid')
        ResourceVersion = [string](Get-PandoraRotationProperty $metadata 'resourceVersion')
    }
}

function Assert-PandoraDSTicketRotationRevisionPlan {
    param(
        [Parameter(Mandatory = $true)][int]$StageRevision,
        [Parameter(Mandatory = $true)][int]$PromoteRevision,
        [Parameter(Mandatory = $true)][int]$RetireRevision,
        [Parameter(Mandatory = $true)][int]$OldSignerRevision
    )
    foreach ($entry in @{
        StageRevision = $StageRevision; PromoteRevision = $PromoteRevision; RetireRevision = $RetireRevision
        OldSignerRevision = $OldSignerRevision
    }.GetEnumerator()) {
        if ([int]$entry.Value -lt 1) { throw "$($entry.Key) 必须 >= 1。" }
    }
    if (-not ($StageRevision -lt $PromoteRevision -and $PromoteRevision -lt $RetireRevision)) {
        throw "DSTicket keyset revision 必须严格递增:stage=$StageRevision promote=$PromoteRevision retire=$RetireRevision。"
    }
    if ($OldSignerRevision -ge $StageRevision) {
        throw "DSTicket signer alias 必须随阶段严格递增:old=$OldSignerRevision stage=$StageRevision promote=$PromoteRevision retire=$RetireRevision。"
    }
}

function Get-PandoraDSTicketConfigMapJwksText {
    param([Parameter(Mandatory = $true)]$ConfigMapObject)
    return [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $ConfigMapObject 'data') 'jwks.json')
}

function Get-PandoraDSTicketConfigMapKids {
    param(
        [Parameter(Mandatory = $true)]$ConfigMapObject,
        [Parameter(Mandatory = $true)][int]$ExpectedRevision
    )
    $jwks = Get-PandoraDSTicketJwksContract `
        -JwksText (Get-PandoraDSTicketConfigMapJwksText $ConfigMapObject) `
        -ExpectedRevision $ExpectedRevision
    return @($jwks.Keys.Keys | Sort-Object)
}

function Assert-PandoraDSTicketTwinConfigMaps {
    param(
        [Parameter(Mandatory = $true)]$DefaultContract,
        [Parameter(Mandatory = $true)]$PandoraContract,
        [Parameter(Mandatory = $true)][int]$Revision
    )
    if ($DefaultContract.JwksSha256 -cne $PandoraContract.JwksSha256 -or
        $DefaultContract.ActiveKid -cne $PandoraContract.ActiveKid) {
        throw "DSTicket revision $Revision 的 default/pandora JWKS 内容或 active_kid 不一致。"
    }
}

function Assert-PandoraDSTicketStageMaterialContract {
    param(
        [Parameter(Mandatory = $true)]$OldSignerSecret,
        [Parameter(Mandatory = $true)]$StageSignerSecret,
        [Parameter(Mandatory = $true)]$DefaultConfigMap,
        [Parameter(Mandatory = $true)]$PandoraConfigMap,
        [Parameter(Mandatory = $true)][int]$StageRevision,
        [Parameter(Mandatory = $true)][int]$OldSignerRevision
    )
    $oldDefault = Assert-PandoraDSTicketKubernetesObjects -SecretObject $OldSignerSecret `
        -ConfigMapObject $DefaultConfigMap -ExpectedRevision $StageRevision `
        -ExpectedSignerRevision $OldSignerRevision -ExpectedConfigMapNamespace default
    $newDefault = Assert-PandoraDSTicketKubernetesObjects -SecretObject $StageSignerSecret `
        -ConfigMapObject $DefaultConfigMap -ExpectedRevision $StageRevision `
        -ExpectedSignerRevision $StageRevision -ExpectedActiveKid $oldDefault.ActiveKid `
        -RequirePrivateKeyActive $false -ExpectedConfigMapNamespace default
    $oldPandora = Assert-PandoraDSTicketKubernetesObjects -SecretObject $OldSignerSecret `
        -ConfigMapObject $PandoraConfigMap -ExpectedRevision $StageRevision `
        -ExpectedSignerRevision $OldSignerRevision -ExpectedActiveKid $oldDefault.ActiveKid `
        -ExpectedConfigMapNamespace pandora
    $newPandora = Assert-PandoraDSTicketKubernetesObjects -SecretObject $StageSignerSecret `
        -ConfigMapObject $PandoraConfigMap -ExpectedRevision $StageRevision `
        -ExpectedSignerRevision $StageRevision -ExpectedActiveKid $oldDefault.ActiveKid `
        -RequirePrivateKeyActive $false -ExpectedConfigMapNamespace pandora
    Assert-PandoraDSTicketTwinConfigMaps -DefaultContract $oldDefault -PandoraContract $oldPandora -Revision $StageRevision
    if ($newDefault.SignerKid -ceq $oldDefault.SignerKid) {
        throw 'DSTicket stage 必须预投递一把不同于 K1 的 K2 signer。'
    }
    if ($oldDefault.KeyCount -ne 2 -or $newDefault.KeyCount -ne 2 -or
        $oldPandora.KeyCount -ne 2 -or $newPandora.KeyCount -ne 2) {
        throw "DSTicket stage revision $StageRevision 必须且只能包含 K1+K2 两把公钥。"
    }
    return [pscustomobject]@{
        OldKid = $oldDefault.SignerKid
        NewKid = $newDefault.SignerKid
        ActiveKid = $oldDefault.ActiveKid
        StageRevision = $StageRevision
        StageJwksSha256 = $oldDefault.JwksSha256
        K2PrivatePemSha256 = $newDefault.PrivatePemSha256
    }
}

function Assert-PandoraDSTicketPromoteMaterialContract {
    param(
        [Parameter(Mandatory = $true)]$OldSignerSecret,
        [Parameter(Mandatory = $true)]$StageSignerSecret,
        [Parameter(Mandatory = $true)]$PromoteSignerSecret,
        [Parameter(Mandatory = $true)]$StageDefaultConfigMap,
        [Parameter(Mandatory = $true)]$StagePandoraConfigMap,
        [Parameter(Mandatory = $true)]$PromoteDefaultConfigMap,
        [Parameter(Mandatory = $true)]$PromotePandoraConfigMap,
        [Parameter(Mandatory = $true)][int]$StageRevision,
        [Parameter(Mandatory = $true)][int]$PromoteRevision,
        [Parameter(Mandatory = $true)][int]$OldSignerRevision
    )
    if ($StageRevision -ge $PromoteRevision) { throw 'DSTicket promote revision 必须大于 stage revision。' }
    $stage = Assert-PandoraDSTicketStageMaterialContract -OldSignerSecret $OldSignerSecret `
        -StageSignerSecret $StageSignerSecret -DefaultConfigMap $StageDefaultConfigMap `
        -PandoraConfigMap $StagePandoraConfigMap -StageRevision $StageRevision `
        -OldSignerRevision $OldSignerRevision

    $newDefault = Assert-PandoraDSTicketKubernetesObjects -SecretObject $PromoteSignerSecret `
        -ConfigMapObject $PromoteDefaultConfigMap -ExpectedRevision $PromoteRevision `
        -ExpectedSignerRevision $PromoteRevision -ExpectedActiveKid $stage.NewKid `
        -ExpectedConfigMapNamespace default
    $oldDefault = Assert-PandoraDSTicketKubernetesObjects -SecretObject $OldSignerSecret `
        -ConfigMapObject $PromoteDefaultConfigMap -ExpectedRevision $PromoteRevision `
        -ExpectedSignerRevision $OldSignerRevision -ExpectedActiveKid $stage.NewKid `
        -RequirePrivateKeyActive $false -ExpectedConfigMapNamespace default
    $newPandora = Assert-PandoraDSTicketKubernetesObjects -SecretObject $PromoteSignerSecret `
        -ConfigMapObject $PromotePandoraConfigMap -ExpectedRevision $PromoteRevision `
        -ExpectedSignerRevision $PromoteRevision -ExpectedActiveKid $stage.NewKid `
        -ExpectedConfigMapNamespace pandora
    $oldPandora = Assert-PandoraDSTicketKubernetesObjects -SecretObject $OldSignerSecret `
        -ConfigMapObject $PromotePandoraConfigMap -ExpectedRevision $PromoteRevision `
        -ExpectedSignerRevision $OldSignerRevision -ExpectedActiveKid $stage.NewKid `
        -RequirePrivateKeyActive $false -ExpectedConfigMapNamespace pandora
    Assert-PandoraDSTicketTwinConfigMaps -DefaultContract $newDefault -PandoraContract $newPandora -Revision $PromoteRevision
    if ($newDefault.KeyCount -ne 2 -or $oldDefault.KeyCount -ne 2 -or
        $newPandora.KeyCount -ne 2 -or $oldPandora.KeyCount -ne 2) {
        throw "DSTicket promote revision $PromoteRevision 必须继续保留 K1+K2 两把公钥。"
    }
    if ($newDefault.SignerKid -cne $stage.NewKid -or
        $newDefault.PrivatePemSha256 -cne $stage.K2PrivatePemSha256 -or
        $newPandora.PrivatePemSha256 -cne $stage.K2PrivatePemSha256) {
        throw "DSTicket stage r$StageRevision 与 promote r$PromoteRevision signer alias 必须是完全相同的 K2 私钥/kid。"
    }
    $stageKids = @(Get-PandoraDSTicketConfigMapKids -ConfigMapObject $StageDefaultConfigMap -ExpectedRevision $StageRevision)
    $promoteKids = @(Get-PandoraDSTicketConfigMapKids -ConfigMapObject $PromoteDefaultConfigMap -ExpectedRevision $PromoteRevision)
    if (($stageKids -join ',') -cne ($promoteKids -join ',')) {
        throw 'DSTicket promote 不得更换 overlap 公钥集合；只能把 active_kid 从 K1 翻到 K2。'
    }
    return [pscustomobject]@{
        OldKid = $stage.OldKid
        NewKid = $stage.NewKid
        ActiveKid = $newDefault.ActiveKid
        StageRevision = $StageRevision
        PromoteRevision = $PromoteRevision
        StageJwksSha256 = $stage.StageJwksSha256
        PromoteJwksSha256 = $newDefault.JwksSha256
        K2PrivatePemSha256 = $newDefault.PrivatePemSha256
    }
}

function Assert-PandoraDSTicketRetireMaterialContract {
    param(
        [Parameter(Mandatory = $true)]$OldSignerSecret,
        [Parameter(Mandatory = $true)]$StageSignerSecret,
        [Parameter(Mandatory = $true)]$PromoteSignerSecret,
        [Parameter(Mandatory = $true)]$RetireSignerSecret,
        [Parameter(Mandatory = $true)]$StageDefaultConfigMap,
        [Parameter(Mandatory = $true)]$StagePandoraConfigMap,
        [Parameter(Mandatory = $true)]$PromoteDefaultConfigMap,
        [Parameter(Mandatory = $true)]$PromotePandoraConfigMap,
        [Parameter(Mandatory = $true)]$RetireDefaultConfigMap,
        [Parameter(Mandatory = $true)]$RetirePandoraConfigMap,
        [Parameter(Mandatory = $true)][int]$StageRevision,
        [Parameter(Mandatory = $true)][int]$PromoteRevision,
        [Parameter(Mandatory = $true)][int]$RetireRevision,
        [Parameter(Mandatory = $true)][int]$OldSignerRevision
    )
    if ($PromoteRevision -ge $RetireRevision) { throw 'DSTicket retire revision 必须大于 promote revision。' }
    $promote = Assert-PandoraDSTicketPromoteMaterialContract -OldSignerSecret $OldSignerSecret `
        -StageSignerSecret $StageSignerSecret -PromoteSignerSecret $PromoteSignerSecret `
        -StageDefaultConfigMap $StageDefaultConfigMap `
        -StagePandoraConfigMap $StagePandoraConfigMap -PromoteDefaultConfigMap $PromoteDefaultConfigMap `
        -PromotePandoraConfigMap $PromotePandoraConfigMap -StageRevision $StageRevision `
        -PromoteRevision $PromoteRevision -OldSignerRevision $OldSignerRevision
    $retireDefault = Assert-PandoraDSTicketKubernetesObjects -SecretObject $RetireSignerSecret `
        -ConfigMapObject $RetireDefaultConfigMap -ExpectedRevision $RetireRevision `
        -ExpectedSignerRevision $RetireRevision -ExpectedActiveKid $promote.NewKid `
        -ExpectedConfigMapNamespace default
    $retirePandora = Assert-PandoraDSTicketKubernetesObjects -SecretObject $RetireSignerSecret `
        -ConfigMapObject $RetirePandoraConfigMap -ExpectedRevision $RetireRevision `
        -ExpectedSignerRevision $RetireRevision -ExpectedActiveKid $promote.NewKid `
        -ExpectedConfigMapNamespace pandora
    Assert-PandoraDSTicketTwinConfigMaps -DefaultContract $retireDefault -PandoraContract $retirePandora -Revision $RetireRevision
    $retireKids = @(Get-PandoraDSTicketConfigMapKids -ConfigMapObject $RetireDefaultConfigMap -ExpectedRevision $RetireRevision)
    if ($retireDefault.KeyCount -ne 1 -or $retirePandora.KeyCount -ne 1 -or
        $retireKids.Count -ne 1 -or $retireKids[0] -cne $promote.NewKid) {
        throw "DSTicket retire revision $RetireRevision 必须只保留已激活的 K2。"
    }
    if ($retireDefault.PrivatePemSha256 -cne $promote.K2PrivatePemSha256 -or
        $retirePandora.PrivatePemSha256 -cne $promote.K2PrivatePemSha256) {
        throw "DSTicket retire r$RetireRevision signer alias 必须与 stage/promote 使用完全相同的 K2 私钥。"
    }
    return [pscustomobject]@{
        OldKid = $promote.OldKid
        NewKid = $promote.NewKid
        ActiveKid = $retireDefault.ActiveKid
        StageRevision = $StageRevision
        PromoteRevision = $PromoteRevision
        RetireRevision = $RetireRevision
        RetireJwksSha256 = $retireDefault.JwksSha256
        K2PrivatePemSha256 = $retireDefault.PrivatePemSha256
    }
}

function Get-PandoraDSTicketConfigSectionContract {
    param(
        [Parameter(Mandatory = $true)][string]$Text,
        [Parameter(Mandatory = $true)][ValidateSet('login', 'matchmaker', 'matchmaker-pve', 'hub-allocator')][string]$ServiceName,
        [switch]$RequireKeysetRevision
    )
    if ([string]::IsNullOrWhiteSpace($Text)) { throw '服务配置为空。' }
    if ($Text.Contains("`t")) { throw 'DSTicket 配置不接受 tab 缩进。' }
    $newline = if ($Text.Contains("`r`n")) { "`r`n" } else { "`n" }
    $lines = [string[]]([regex]::Split($Text, '\r?\n'))
    $sectionIndexes = @(for ($i = 0; $i -lt $lines.Count; $i++) {
        if ($lines[$i] -cmatch '^(?<indent> *)ds_ticket:\s*(?:#.*)?$') { $i }
    })
    if ($sectionIndexes.Count -ne 1) { throw "服务配置 ds_ticket 节数量=$($sectionIndexes.Count)，应为 1。" }
    $start = [int]$sectionIndexes[0]
    $baseIndent = [regex]::Match($lines[$start], '^ *').Value.Length
    if ($ServiceName -ceq 'login') {
        $loginIndexes = @(for ($i = 0; $i -lt $lines.Count; $i++) {
            if ($lines[$i] -cmatch '^login:\s*(?:#.*)?$') { $i }
        })
        if ($loginIndexes.Count -ne 1 -or $baseIndent -ne 2) {
            throw 'login.yaml 的 ds_ticket 必须恰为顶级 login: 的 2 空格直接子节。'
        }
        $loginStart = [int]$loginIndexes[0]
        $loginEnd = $lines.Count
        for ($i = $loginStart + 1; $i -lt $lines.Count; $i++) {
            if ([string]::IsNullOrWhiteSpace($lines[$i]) -or $lines[$i] -cmatch '^\s*#') { continue }
            if ([regex]::Match($lines[$i], '^ *').Value.Length -eq 0) { $loginEnd = $i; break }
        }
        if ($start -le $loginStart -or $start -ge $loginEnd) {
            throw 'login.yaml 的 ds_ticket 不在顶级 login: 节内。'
        }
    } elseif ($baseIndent -ne 0) {
        throw "$ServiceName.yaml 的 ds_ticket 必须是顶级 0 缩进节。"
    }
    $childIndent = ' ' * ($baseIndent + 2)
    $end = $lines.Count
    for ($i = $start + 1; $i -lt $lines.Count; $i++) {
        if ([string]::IsNullOrWhiteSpace($lines[$i]) -or $lines[$i] -cmatch '^\s*#') { continue }
        $indent = [regex]::Match($lines[$i], '^ *').Value.Length
        if ($indent -le $baseIndent) { $end = $i; break }
    }
    $activePattern = '^' + [regex]::Escape($childIndent) + 'active_kid:\s*"(?<value>[A-Za-z0-9_-]{43})"(?<suffix>\s*(?:#.*)?)$'
    $revisionPattern = '^' + [regex]::Escape($childIndent) + 'keyset_revision:\s*"(?<value>[1-9][0-9]*)"(?<suffix>\s*(?:#.*)?)$'
    $privatePattern = '^' + [regex]::Escape($childIndent) + 'private_key_file:\s*"(?<value>[^"]+)"\s*(?:#.*)?$'
    $ttlPattern = '^' + [regex]::Escape($childIndent) + 'ttl:\s*"(?<value>[^"]+)"\s*(?:#.*)?$'
    $jwksPattern = '^' + [regex]::Escape($childIndent) + 'jwks_file:\s*"(?<value>[^"]+)"\s*(?:#.*)?$'
    $active = @()
    $revision = @()
    $private = @()
    $ttl = @()
    $jwks = @()
    for ($i = $start + 1; $i -lt $end; $i++) {
        if ($lines[$i] -cmatch $activePattern) { $active += $i }
        if ($lines[$i] -cmatch $revisionPattern) { $revision += $i }
        if ($lines[$i] -cmatch $privatePattern) { $private += $i }
        if ($lines[$i] -cmatch $ttlPattern) { $ttl += $i }
        if ($lines[$i] -cmatch $jwksPattern) { $jwks += $i }
    }
    if ($active.Count -ne 1) { throw "ds_ticket.active_kid 字段数量=$($active.Count)，应为 1 个直接子字段。" }
    if ($RequireKeysetRevision -and $revision.Count -ne 1) {
        throw "ds_ticket.keyset_revision 字段数量=$($revision.Count)，应为 1 个直接子字段。"
    }
    if (-not $RequireKeysetRevision -and $revision.Count -gt 1) {
        throw 'ds_ticket.keyset_revision 重复。'
    }
    if ($private.Count -ne 1 -or
        [regex]::Match($lines[[int]$private[0]], $privatePattern).Groups['value'].Value -cne '/run/secrets/pandora-dsticket/private.pem') {
        throw 'ds_ticket.private_key_file 必须恰为 /run/secrets/pandora-dsticket/private.pem。'
    }
    if ($ttl.Count -ne 1) { throw 'ds_ticket.ttl 必须恰有一个显式 duration。' }
    $ttlText = [regex]::Match($lines[[int]$ttl[0]], $ttlPattern).Groups['value'].Value
    if ($ttlText -cnotmatch '^(?:[0-9]+(?:\.[0-9]+)?(?:ns|us|µs|ms|s|m|h))+$') {
        throw "ds_ticket.ttl=$ttlText 不是受支持的 Go duration。"
    }
    [decimal]$ttlSeconds = 0
    foreach ($part in [regex]::Matches($ttlText, '(?<number>[0-9]+(?:\.[0-9]+)?)(?<unit>ns|us|µs|ms|s|m|h)')) {
        $number = [decimal]::Parse($part.Groups['number'].Value, [Globalization.CultureInfo]::InvariantCulture)
        [decimal]$factor = switch ($part.Groups['unit'].Value) {
            'ns' { 0.000000001 }; 'us' { 0.000001 }; 'µs' { 0.000001 }; 'ms' { 0.001 }
            's' { 1 }; 'm' { 60 }; 'h' { 3600 }
        }
        $ttlSeconds += $number * $factor
    }
    if ($ttlSeconds -le 0 -or $ttlSeconds -gt 180) {
        throw "ds_ticket.ttl=$ttlText 超出 0 < ttl <= 180s。"
    }
    if ($ServiceName -ceq 'login' -and ($jwks.Count -ne 1 -or
        [regex]::Match($lines[[int]$jwks[0]], $jwksPattern).Groups['value'].Value -cne '/run/config/pandora-dsticket/jwks.json')) {
        throw 'login.ds_ticket.jwks_file 必须恰为 /run/config/pandora-dsticket/jwks.json。'
    }
    $activeMatch = [regex]::Match($lines[[int]$active[0]], $activePattern)
    $revisionValue = 0
    $revisionIndex = -1
    if ($revision.Count -eq 1) {
        $revisionIndex = [int]$revision[0]
        $revisionValue = [int][regex]::Match($lines[$revisionIndex], $revisionPattern).Groups['value'].Value
    }
    return [pscustomobject]@{
        Lines = $lines
        Newline = $newline
        ChildIndent = $childIndent
        ActiveIndex = [int]$active[0]
        ActiveKid = $activeMatch.Groups['value'].Value
        ActiveSuffix = $activeMatch.Groups['suffix'].Value
        RevisionIndex = $revisionIndex
        KeysetRevision = $revisionValue
        RevisionSuffix = if ($revisionIndex -ge 0) { [regex]::Match($lines[$revisionIndex], $revisionPattern).Groups['suffix'].Value } else { '' }
    }
}

function Set-PandoraDSTicketConfigText {
    param(
        [Parameter(Mandatory = $true)][string]$Text,
        [Parameter(Mandatory = $true)][ValidateSet('login', 'matchmaker', 'matchmaker-pve', 'hub-allocator')][string]$ServiceName,
        [Parameter(Mandatory = $true)][string]$ActiveKid,
        [ValidateRange(0, 2147483647)][int]$KeysetRevision = 0
    )
    if ($ActiveKid -cnotmatch '^[A-Za-z0-9_-]{43}$') { throw '目标 DSTicket active kid 非法。' }
    $section = Get-PandoraDSTicketConfigSectionContract -Text $Text -ServiceName $ServiceName `
        -RequireKeysetRevision:($KeysetRevision -gt 0)
    $lines = [string[]]$section.Lines.Clone()
    $lines[$section.ActiveIndex] = $section.ChildIndent + 'active_kid: "' + $ActiveKid + '"' + $section.ActiveSuffix
    if ($KeysetRevision -gt 0) {
        $lines[$section.RevisionIndex] = $section.ChildIndent + 'keyset_revision: "' + $KeysetRevision + '"' + $section.RevisionSuffix
    }
    return ($lines -join $section.Newline)
}

function Get-PandoraDSTicketConfigSecretUpdatedData {
    param(
        [Parameter(Mandatory = $true)]$SecretObject,
        [Parameter(Mandatory = $true)][string]$ActiveKid,
        [Parameter(Mandatory = $true)][int]$LoginKeysetRevision,
        [Parameter(Mandatory = $true)][string[]]$AllowedCurrentActiveKids
    )
    if ([string]$SecretObject.kind -cne 'Secret' -or [string]$SecretObject.metadata.name -cne 'pandora-config' -or
        [string]$SecretObject.metadata.namespace -cne 'pandora') {
        throw '服务配置对象必须是 pandora/Secret/pandora-config。'
    }
    if (-not [string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty `
        (Get-PandoraRotationProperty $SecretObject 'metadata') 'deletionTimestamp'))) {
        throw 'pandora/Secret/pandora-config 正在删除，禁止生成更新数据。'
    }
    if ($AllowedCurrentActiveKids.Count -lt 1) { throw 'AllowedCurrentActiveKids 不能为空。' }
    $source = Get-PandoraRotationProperty $SecretObject 'data'
    $updated = [ordered]@{}
    foreach ($property in @($source.PSObject.Properties)) { $updated[$property.Name] = [string]$property.Value }
    foreach ($service in $script:PandoraDSTicketSignerNames) {
        $key = "$service.yaml"
        if (-not $updated.Contains($key) -or [string]::IsNullOrWhiteSpace([string]$updated[$key])) {
            throw "Secret/pandora-config 缺 $key。"
        }
        try { $text = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String([string]$updated[$key])) }
        catch { throw "Secret/pandora-config/$key 不是合法 base64:$($_.Exception.Message)" }
        $section = Get-PandoraDSTicketConfigSectionContract -Text $text -ServiceName $service `
            -RequireKeysetRevision:($service -ceq 'login')
        if ($AllowedCurrentActiveKids -cnotcontains $section.ActiveKid) {
            throw "Secret/pandora-config/$key 当前 active_kid=$($section.ActiveKid) 不在本阶段允许集合内。"
        }
        $next = Set-PandoraDSTicketConfigText -Text $text -ServiceName $service -ActiveKid $ActiveKid `
            -KeysetRevision $(if ($service -ceq 'login') { $LoginKeysetRevision } else { 0 })
        $updated[$key] = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($next))
    }
    return $updated
}

function Assert-PandoraDSTicketConfigSecretContract {
    param(
        [Parameter(Mandatory = $true)]$SecretObject,
        [Parameter(Mandatory = $true)][string]$ExpectedActiveKid,
        [Parameter(Mandatory = $true)][int]$ExpectedLoginKeysetRevision,
        [string]$ExpectedName = 'pandora-config'
    )
    $metadata = Get-PandoraRotationProperty $SecretObject 'metadata'
    if ([string](Get-PandoraRotationProperty $SecretObject 'kind') -cne 'Secret' -or
        [string](Get-PandoraRotationProperty $metadata 'name') -cne $ExpectedName -or
        [string](Get-PandoraRotationProperty $metadata 'namespace') -cne 'pandora') {
        throw "配置对象必须是 pandora/Secret/$ExpectedName。"
    }
    if (-not [string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty $metadata 'deletionTimestamp'))) {
        throw "pandora/Secret/$ExpectedName 正在删除，禁止作为 signer 配置。"
    }
    $data = Get-PandoraRotationProperty $SecretObject 'data'
    foreach ($service in $script:PandoraDSTicketSignerNames) {
        $key = "$service.yaml"
        $encoded = [string](Get-PandoraRotationProperty $data $key)
        if ([string]::IsNullOrWhiteSpace($encoded)) { throw "Secret/pandora-config 缺 $key。" }
        try { $text = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($encoded)) }
        catch { throw "Secret/pandora-config/$key 不是合法 base64:$($_.Exception.Message)" }
        $section = Get-PandoraDSTicketConfigSectionContract -Text $text -ServiceName $service `
            -RequireKeysetRevision:($service -ceq 'login')
        if ($section.ActiveKid -cne $ExpectedActiveKid) {
            throw "Secret/pandora-config/$key active_kid=$($section.ActiveKid)，expected=$ExpectedActiveKid。"
        }
        if ($service -ceq 'login' -and $section.KeysetRevision -ne $ExpectedLoginKeysetRevision) {
            throw "Secret/pandora-config/login.yaml keyset_revision=$($section.KeysetRevision)，expected=$ExpectedLoginKeysetRevision。"
        }
    }
}

function Get-PandoraDSTicketConfigSubcontract {
    param([Parameter(Mandatory = $true)]$SecretObject)
    $metadata = Get-PandoraRotationProperty $SecretObject 'metadata'
    if ([string](Get-PandoraRotationProperty $SecretObject 'kind') -cne 'Secret' -or
        [string](Get-PandoraRotationProperty $metadata 'name') -cne 'pandora-config' -or
        [string](Get-PandoraRotationProperty $metadata 'namespace') -cne 'pandora') {
        throw 'DSTicket fixed 子契约对象必须是 pandora/Secret/pandora-config。'
    }
    if (-not [string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty `
        $metadata 'deletionTimestamp'))) {
        throw 'fixed pandora/Secret/pandora-config 正在删除，禁止读取 DSTicket 子契约。'
    }
    $data = Get-PandoraRotationProperty $SecretObject 'data'
    $rows = [System.Collections.Generic.List[string]]::new()
    $kids = [System.Collections.Generic.List[string]]::new()
    $loginRevision = 0
    foreach ($service in @($script:PandoraDSTicketSignerNames | Sort-Object)) {
        $encoded = [string](Get-PandoraRotationProperty $data "$service.yaml")
        if ([string]::IsNullOrWhiteSpace($encoded)) { throw "配置 Secret 缺 $service.yaml。" }
        try { $text = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($encoded)) }
        catch { throw "配置 Secret/$service.yaml base64 非法:$($_.Exception.Message)" }
        $section = Get-PandoraDSTicketConfigSectionContract $text -ServiceName $service `
            -RequireKeysetRevision:($service -ceq 'login')
        $kids.Add($section.ActiveKid)
        $rows.Add("$service.active_kid=$($section.ActiveKid)")
        if ($service -ceq 'login') {
            $loginRevision = $section.KeysetRevision
            $rows.Add("login.keyset_revision=$loginRevision")
        }
    }
    $uniqueKids = @($kids | Sort-Object -Unique)
    if ($uniqueKids.Count -ne 1) { throw "四 signer active_kid 不一致:$($uniqueKids -join ',')。" }
    $canonical = (@($rows | Sort-Object) -join "`n") + "`n"
    return [pscustomobject]@{
        ActiveKid = $uniqueKids[0]
        LoginKeysetRevision = $loginRevision
        Sha256 = Get-PandoraSha256Hex ([Text.Encoding]::UTF8.GetBytes($canonical))
        Canonical = $canonical
    }
}

function Get-PandoraSecretDataSha256 {
    param([Parameter(Mandatory = $true)]$Data)
    $entries = [System.Collections.Generic.List[object]]::new()
    if ($Data -is [System.Collections.IDictionary]) {
        foreach ($key in $Data.Keys) { $entries.Add([pscustomobject]@{ Name = [string]$key; Value = [string]$Data[$key] }) }
    } else {
        foreach ($property in @($Data.PSObject.Properties)) {
            if ($property.MemberType -in @('NoteProperty', 'Property')) {
                $entries.Add([pscustomobject]@{ Name = [string]$property.Name; Value = [string]$property.Value })
            }
        }
    }
    if ($entries.Count -lt 1) { throw 'Secret data 为空，不能生成 revisioned config。' }
    $canonical = [Text.StringBuilder]::new()
    foreach ($entry in @($entries | Sort-Object Name)) {
        try { $null = [Convert]::FromBase64String($entry.Value) }
        catch { throw "Secret data/$($entry.Name) 不是合法 base64:$($_.Exception.Message)" }
        $null = $canonical.Append($entry.Name.Length).Append(':').Append($entry.Name).Append(':')
        $null = $canonical.Append($entry.Value.Length).Append(':').Append($entry.Value).Append("`n")
    }
    return Get-PandoraSha256Hex ([Text.Encoding]::UTF8.GetBytes($canonical.ToString()))
}

function New-PandoraDSTicketRevisionedConfigSecretObject {
    param(
        [Parameter(Mandatory = $true)]$SourceSecret,
        [Parameter(Mandatory = $true)][int]$Revision,
        [Parameter(Mandatory = $true)][string]$ActiveKid,
        [Parameter(Mandatory = $true)][string[]]$AllowedCurrentActiveKids
    )
    if ($Revision -lt 1) { throw 'revisioned config revision 必须 >= 1。' }
    $updated = Get-PandoraDSTicketConfigSecretUpdatedData -SecretObject $SourceSecret -ActiveKid $ActiveKid `
        -LoginKeysetRevision $Revision -AllowedCurrentActiveKids $AllowedCurrentActiveKids
    $data = [pscustomobject]$updated
    $hash = Get-PandoraSecretDataSha256 -Data $data
    $sourceRV = [string](Get-PandoraRotationProperty $SourceSecret.metadata 'resourceVersion')
    if ([string]::IsNullOrWhiteSpace($sourceRV)) { throw 'pandora-config 缺 resourceVersion，不能留下可审计复制来源。' }
    return [ordered]@{
        apiVersion = 'v1'
        kind = 'Secret'
        immutable = $true
        type = if ([string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty $SourceSecret 'type'))) { 'Opaque' } else { [string]$SourceSecret.type }
        metadata = [ordered]@{
            name = "pandora-config-dsticket-r$Revision"
            namespace = 'pandora'
            labels = [ordered]@{
                'app.kubernetes.io/part-of' = 'pandora'
                'app.kubernetes.io/component' = 'dsticket-rotation-config'
            }
            annotations = [ordered]@{
                'pandora.dev/dsticket-config-revision' = [string]$Revision
                'pandora.dev/dsticket-config-active-kid' = $ActiveKid
                'pandora.dev/dsticket-config-data-sha256' = $hash
                'pandora.dev/dsticket-config-source-resource-version' = $sourceRV
            }
        }
        data = $data
    }
}

function Assert-PandoraDSTicketRevisionedConfigSecretContract {
    param(
        [Parameter(Mandatory = $true)]$SecretObject,
        [Parameter(Mandatory = $true)][int]$ExpectedRevision,
        [Parameter(Mandatory = $true)][string]$ExpectedActiveKid,
        [string]$ExpectedDataSha256 = ''
    )
    $name = "pandora-config-dsticket-r$ExpectedRevision"
    if ([string]$SecretObject.kind -cne 'Secret' -or [string]$SecretObject.metadata.name -cne $name -or
        [string]$SecretObject.metadata.namespace -cne 'pandora' -or $SecretObject.immutable -ne $true) {
        throw "DSTicket 阶段配置必须是 immutable pandora/Secret/$name。"
    }
    Assert-PandoraDSTicketConfigSecretContract -SecretObject $SecretObject -ExpectedActiveKid $ExpectedActiveKid `
        -ExpectedLoginKeysetRevision $ExpectedRevision -ExpectedName $name
    $hash = Get-PandoraSecretDataSha256 -Data $SecretObject.data
    $annotations = $SecretObject.metadata.annotations
    if ([string](Get-PandoraRotationProperty $annotations 'pandora.dev/dsticket-config-revision') -cne [string]$ExpectedRevision -or
        [string](Get-PandoraRotationProperty $annotations 'pandora.dev/dsticket-config-active-kid') -cne $ExpectedActiveKid -or
        [string](Get-PandoraRotationProperty $annotations 'pandora.dev/dsticket-config-data-sha256') -cne $hash -or
        [string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty $annotations 'pandora.dev/dsticket-config-source-resource-version'))) {
        throw "Secret/$name annotation/hash 与完整 data 不一致。"
    }
    if (-not [string]::IsNullOrWhiteSpace($ExpectedDataSha256) -and
        ($ExpectedDataSha256 -cnotmatch '^[0-9a-f]{64}$' -or $hash -cne $ExpectedDataSha256)) {
        throw "Secret/$name 完整 data hash=$hash 与当前 fixed pandora-config 投影=$ExpectedDataSha256 不一致。"
    }
    return [pscustomobject]@{ Revision = $ExpectedRevision; ActiveKid = $ExpectedActiveKid; DataSha256 = $hash }
}

function Test-PandoraDSTicketWorkloadSpecRevision {
    param(
        [Parameter(Mandatory = $true)]$Spec,
        [Parameter(Mandatory = $true)][string]$ContainerName,
        [Parameter(Mandatory = $true)][int]$Revision,
        [Parameter(Mandatory = $true)][string]$ObjectName
    )
    $failures = [System.Collections.Generic.List[string]]::new()
    $containers = @(Get-PandoraRotationProperty $Spec 'containers' @())
    $containerMatches = @($containers | Where-Object { [string](Get-PandoraRotationProperty $_ 'name') -ceq $ContainerName })
    if ($containerMatches.Count -ne 1) {
        $failures.Add("$ObjectName 主容器 $ContainerName 数量=$($containerMatches.Count)")
        return @($failures)
    }
    $container = $containerMatches[0]
    $env = @(Get-PandoraRotationProperty $container 'env' @())
    $jwksFileEnv = @(Get-PandoraRotationNamedItems -Items $env -Name 'PANDORA_DSTICKET_JWKS_FILE')
    if ($jwksFileEnv.Count -ne 1 -or [string](Get-PandoraRotationProperty $jwksFileEnv[0] 'value') -cne '/etc/pandora/dsticket/jwks.json') {
        $actual = if ($jwksFileEnv.Count -eq 1) { [string](Get-PandoraRotationProperty $jwksFileEnv[0] 'value') } else { "count=$($jwksFileEnv.Count)" }
        $failures.Add("$ObjectName PANDORA_DSTICKET_JWKS_FILE=$actual expected=/etc/pandora/dsticket/jwks.json")
    }
    $revisionEnv = @(Get-PandoraRotationNamedItems -Items $env -Name 'PANDORA_DSTICKET_KEYSET_REVISION')
    if ($revisionEnv.Count -ne 1 -or [string](Get-PandoraRotationProperty $revisionEnv[0] 'value') -cne [string]$Revision) {
        $actual = if ($revisionEnv.Count -eq 1) { [string](Get-PandoraRotationProperty $revisionEnv[0] 'value') } else { "count=$($revisionEnv.Count)" }
        $failures.Add("$ObjectName env revision=$actual expected=$Revision")
    }
    $mounts = @(Get-PandoraRotationProperty $container 'volumeMounts' @())
    $mount = @(Get-PandoraRotationNamedItems -Items $mounts -Name 'dsticket-jwks')
    if ($mount.Count -ne 1 -or [string](Get-PandoraRotationProperty $mount[0] 'mountPath') -cne '/etc/pandora/dsticket' -or
        (Get-PandoraRotationProperty $mount[0] 'readOnly' $false) -ne $true) {
        $failures.Add("$ObjectName 缺只读 /etc/pandora/dsticket 挂载")
    }
    $volumes = @(Get-PandoraRotationProperty $Spec 'volumes' @())
    $volume = @(Get-PandoraRotationNamedItems -Items $volumes -Name 'dsticket-jwks')
    $expectedConfigMap = "pandora-dsticket-jwks-r$Revision"
    $actualConfigMap = if ($volume.Count -eq 1) {
        [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $volume[0] 'configMap') 'name')
    } else { "count=$($volume.Count)" }
    if ($volume.Count -ne 1 -or $actualConfigMap -cne $expectedConfigMap) {
        $failures.Add("$ObjectName ConfigMap=$actualConfigMap expected=$expectedConfigMap")
    }
    foreach ($reference in @(Get-PandoraDSTicketVerifierMaterialReferences $Spec)) {
        $isCanonical =
            ($reference.Kind -ceq 'ConfigMapVolume' -and $reference.Name -ceq $expectedConfigMap -and
                $reference.Location -ceq 'volume/dsticket-jwks') -or
            ($reference.Kind -ceq 'ManagedContainer' -and $reference.Name -ceq $ContainerName -and
                $reference.Location -ceq "container/$ContainerName") -or
            ($reference.Kind -ceq 'VerifierMount' -and $reference.Name -ceq 'dsticket-jwks' -and
                $reference.Location -ceq "container/$ContainerName/mount") -or
            ($reference.Kind -ceq 'VerifierEnv' -and [string]::IsNullOrEmpty([string]$reference.Name) -and
                $reference.Location -cin @(
                    "container/$ContainerName/env/PANDORA_DSTICKET_JWKS_FILE",
                    "container/$ContainerName/env/PANDORA_DSTICKET_KEYSET_REVISION"
                ))
        if (-not $isCanonical) {
            $failures.Add("$ObjectName 检测到非 canonical verifier 引用($($reference.Kind):$($reference.Name):$($reference.Location))")
        }
    }
    foreach ($reference in @(Get-PandoraDSTicketForbiddenPrivateReferences $Spec)) {
        $failures.Add("$ObjectName 检测到禁止的 DSTicket signer/private/oct 输入($($reference.Kind):$($reference.Location))")
    }
    foreach ($candidateVolume in $volumes) {
        $secretName = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $candidateVolume 'secret') 'secretName')
        if ($secretName -imatch '^pandora-dsticket(?:-signer-r[1-9][0-9]*)?$') {
            $failures.Add("$ObjectName 检测到禁止的 signer Secret/$secretName")
        }
    }
    foreach ($reference in @(Get-PandoraDSTicketSignerSecretReferences $Spec)) {
        $failures.Add("$ObjectName 检测到禁止的 signer Secret/$($reference.Name) 引用($($reference.Kind):$($reference.Location))")
    }
    return @($failures)
}

function Get-PandoraDSTicketDeclaredWorkloadRevision {
    param(
        [Parameter(Mandatory = $true)]$Spec,
        [Parameter(Mandatory = $true)][string]$ContainerName,
        [Parameter(Mandatory = $true)][string]$ObjectName
    )
    $containers = @(Get-PandoraRotationProperty $Spec 'containers' @())
    $containerMatches = @(Get-PandoraRotationNamedItems $containers $ContainerName)
    if ($containerMatches.Count -ne 1) { throw "$ObjectName 主容器 $ContainerName 数量=$($containerMatches.Count)。" }
    $revisionEntries = @(Get-PandoraRotationNamedItems `
        (Get-PandoraRotationProperty $containerMatches[0] 'env' @()) 'PANDORA_DSTICKET_KEYSET_REVISION')
    [int]$revision = 0
    if ($revisionEntries.Count -ne 1 -or
        -not [int]::TryParse([string](Get-PandoraRotationProperty $revisionEntries[0] 'value'), [ref]$revision) -or
        $revision -lt 1) {
        throw "$ObjectName PANDORA_DSTICKET_KEYSET_REVISION 必须恰有一个正整数值。"
    }
    $failures = @(Test-PandoraDSTicketWorkloadSpecRevision -Spec $Spec -ContainerName $ContainerName `
        -Revision $revision -ObjectName $ObjectName)
    if ($failures.Count -gt 0) { throw ($failures -join '; ') }
    return $revision
}

function Get-PandoraGameServerSetPodSpec {
    param([Parameter(Mandatory = $true)]$GameServerSet, [Parameter(Mandatory = $true)][string]$Where)
    $gameServerSpec = Get-PandoraRotationProperty (Get-PandoraRotationProperty `
        (Get-PandoraRotationProperty $GameServerSet 'spec') 'template') 'spec'
    $nested = Get-PandoraRotationProperty (Get-PandoraRotationProperty $gameServerSpec 'template') 'spec'
    if ($null -ne $nested) { return $nested }
    if ($null -ne (Get-PandoraRotationProperty $gameServerSpec 'containers')) { return $gameServerSpec }
    throw "$Where 缺 GameServer/Pod template spec。"
}

function Get-PandoraNonNegativeControllerCount {
    param($Object, [Parameter(Mandatory = $true)][string]$Name, [Parameter(Mandatory = $true)][string]$Where)
    $raw = Get-PandoraRotationProperty $Object $Name 0
    [int]$value = 0
    if (-not [int]::TryParse([string]$raw, [ref]$value) -or $value -lt 0) { throw "$Where $Name 非法:$raw。" }
    return $value
}

function Test-PandoraDSTicketGameServerSetRevisionGate {
    param(
        [Parameter(Mandatory = $true)][object[]]$FleetObjects,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$GameServerSetObjects,
        [Parameter(Mandatory = $true)][int[]]$AllowedActiveRevisions
    )
    $allowed = @($AllowedActiveRevisions | Sort-Object -Unique)
    if ($allowed.Count -eq 0 -or @($allowed | Where-Object { $_ -lt 1 }).Count -gt 0) {
        throw 'GameServerSet AllowedActiveRevisions 必须是非空正整数集合。'
    }
    $failures = [System.Collections.Generic.List[string]]::new()
    $fleetUidByName = @{}
    $fleetByName = @{}
    $activeGameServerSetCountByFleet = @{}
    foreach ($fleet in @($FleetObjects)) {
        $fleetMetadata = Get-PandoraRotationProperty $fleet 'metadata'
        $fleetName = [string](Get-PandoraRotationProperty $fleetMetadata 'name')
        if ($script:PandoraDSTicketFleetNames -cnotcontains $fleetName) { continue }
        if ($fleetUidByName.ContainsKey($fleetName)) { $failures.Add("Fleet/$fleetName 重复，无法验证 GameServerSet owner UID"); continue }
        $fleetUid = [string](Get-PandoraRotationProperty $fleetMetadata 'uid')
        if ([string]::IsNullOrWhiteSpace($fleetUid)) { $failures.Add("Fleet/$fleetName 缺 UID，无法验证 GameServerSet owner"); continue }
        $fleetUidByName[$fleetName] = $fleetUid
        $fleetByName[$fleetName] = $fleet
        $activeGameServerSetCountByFleet[$fleetName] = 0
    }
    $seen = @{}
    foreach ($gss in @($GameServerSetObjects)) {
        $metadata = Get-PandoraRotationProperty $gss 'metadata'
        $name = [string](Get-PandoraRotationProperty $metadata 'name')
        $where = "GameServerSet/$name"
        if ([string]::IsNullOrWhiteSpace($name) -or $seen.ContainsKey($name)) {
            $failures.Add("GameServerSet name 为空或重复:$name"); continue
        }
        $seen[$name] = $true
        if ([string](Get-PandoraRotationProperty $metadata 'namespace') -cne 'default') {
            $failures.Add("$where namespace 不是 default")
        }
        if ([string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty $metadata 'uid'))) {
            $failures.Add("$where 缺 UID")
        }
        if (-not [string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty $metadata 'deletionTimestamp'))) {
            $failures.Add("$where 正在终止；必须等对象真正消失以关闭异步补出窗口")
        }
        $owners = @(@(Get-PandoraRotationProperty $metadata 'ownerReferences' @()) | Where-Object {
            (Get-PandoraRotationProperty $_ 'controller' $false) -eq $true
        })
        if ($owners.Count -ne 1 -or [string](Get-PandoraRotationProperty $owners[0] 'kind') -cne 'Fleet') {
            $failures.Add("$where 必须恰有一个且类型为 Fleet 的 controller owner"); continue
        }
        $fleetName = [string](Get-PandoraRotationProperty $owners[0] 'name')
        if ($script:PandoraDSTicketFleetNames -cnotcontains $fleetName) {
            $failures.Add("$where owner Fleet/$fleetName 不受管"); continue
        }
        $ownerUid = [string](Get-PandoraRotationProperty $owners[0] 'uid')
        if ([string]::IsNullOrWhiteSpace($fleetName) -or [string]::IsNullOrWhiteSpace($ownerUid) -or
            -not $fleetUidByName.ContainsKey($fleetName) -or $ownerUid -cne [string]$fleetUidByName[$fleetName]) {
            $failures.Add("$where owner Fleet/$fleetName UID 漂移/孤儿"); continue
        }
        $labelFleet = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $metadata 'labels') 'agones.dev/fleet')
        if (-not [string]::IsNullOrWhiteSpace($labelFleet) -and $labelFleet -cne $fleetName) {
            $failures.Add("$where label Fleet/$labelFleet 与 owner Fleet/$fleetName 不一致")
        }
        $spec = Get-PandoraRotationProperty $gss 'spec'
        $status = Get-PandoraRotationProperty $gss 'status'
        $counts = [System.Collections.Generic.List[int]]::new()
        try {
            $counts.Add((Get-PandoraNonNegativeControllerCount $spec 'replicas' "$where spec"))
            foreach ($field in @('replicas', 'readyReplicas', 'allocatedReplicas', 'reservedReplicas', 'currentReplicas')) {
                $counts.Add((Get-PandoraNonNegativeControllerCount $status $field "$where status"))
            }
        } catch { $failures.Add($_.Exception.Message); continue }
        $hasCapacity = @($counts | Where-Object { $_ -gt 0 }).Count -gt 0
        if ($hasCapacity) { $activeGameServerSetCountByFleet[$fleetName] = [int]$activeGameServerSetCountByFleet[$fleetName] + 1 }
        if (-not $hasCapacity) { continue }
        $containerName = if ($fleetName.StartsWith('pandora-battle-', [StringComparison]::Ordinal)) { 'pandora-battle-ds' } else { 'pandora-hub-ds' }
        try {
            $revision = Get-PandoraDSTicketDeclaredWorkloadRevision `
                (Get-PandoraGameServerSetPodSpec $gss $where) $containerName $where
            if ($allowed -cnotcontains $revision) {
                $failures.Add("$where 仍有 desired/status/ready/allocated/reserved capacity，但 template revision=r$revision 不在 {$($allowed -join ',')}")
            }
        } catch { $failures.Add($_.Exception.Message) }
    }
    foreach ($fleetName in $fleetByName.Keys) {
        $replicas = Get-PandoraNonNegativeControllerCount `
            (Get-PandoraRotationProperty $fleetByName[$fleetName] 'spec') 'replicas' "Fleet/$fleetName spec"
        if ($replicas -gt 0 -and [int]$activeGameServerSetCountByFleet[$fleetName] -lt 1) {
            $failures.Add("Fleet/$fleetName replicas=$replicas 但没有非零 owned GameServerSet，无法关闭异步补出窗口")
        }
    }
    return [pscustomobject]@{ Ok = ($failures.Count -eq 0); Failures = [string[]]$failures.ToArray() }
}

function Assert-PandoraDSTicketGameServerSetRevisionGate {
    param(
        [Parameter(Mandatory = $true)][object[]]$FleetObjects,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$GameServerSetObjects,
        [Parameter(Mandatory = $true)][int[]]$AllowedActiveRevisions
    )
    $result = Test-PandoraDSTicketGameServerSetRevisionGate $FleetObjects $GameServerSetObjects $AllowedActiveRevisions
    if (-not $result.Ok) { throw "DSTicket GameServerSet revision 门禁失败:$($result.Failures -join '; ')" }
    return $result
}

function Test-PandoraPodMatchesGameServer {
    param([Parameter(Mandatory = $true)]$Pod, [Parameter(Mandatory = $true)]$GameServer)
    $podMeta = Get-PandoraRotationProperty $Pod 'metadata'
    $gsMeta = Get-PandoraRotationProperty $GameServer 'metadata'
    $gsName = [string](Get-PandoraRotationProperty $gsMeta 'name')
    $gsUid = [string](Get-PandoraRotationProperty $gsMeta 'uid')
    if ([string]::IsNullOrWhiteSpace($gsName) -or [string]::IsNullOrWhiteSpace($gsUid)) { return $false }
    $owners = @(@(Get-PandoraRotationProperty $podMeta 'ownerReferences' @()) | Where-Object {
        (Get-PandoraRotationProperty $_ 'controller' $false) -eq $true
    })
    return $owners.Count -eq 1 -and [string](Get-PandoraRotationProperty $owners[0] 'kind') -ceq 'GameServer' -and
        [string](Get-PandoraRotationProperty $owners[0] 'name') -ceq $gsName -and
        [string](Get-PandoraRotationProperty $owners[0] 'uid') -ceq $gsUid
}

function Test-PandoraGameServerControllerOwnerChain {
    param(
        [Parameter(Mandatory = $true)]$GameServer,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$GameServerSetObjects
    )
    $metadata = Get-PandoraRotationProperty $GameServer 'metadata'
    $name = [string](Get-PandoraRotationProperty $metadata 'name')
    $owners = @(@(Get-PandoraRotationProperty $metadata 'ownerReferences' @()) | Where-Object {
        (Get-PandoraRotationProperty $_ 'controller' $false) -eq $true
    })
    if ($owners.Count -ne 1 -or [string](Get-PandoraRotationProperty $owners[0] 'kind') -cne 'GameServerSet') {
        return "GameServer/$name 必须恰有一个且类型为 GameServerSet 的 controller owner"
    }
    $ownerName = [string](Get-PandoraRotationProperty $owners[0] 'name')
    $ownerUid = [string](Get-PandoraRotationProperty $owners[0] 'uid')
    $matches = @($GameServerSetObjects | Where-Object {
        [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $_ 'metadata') 'name') -ceq $ownerName
    })
    if ([string]::IsNullOrWhiteSpace($ownerName) -or [string]::IsNullOrWhiteSpace($ownerUid) -or
        $matches.Count -ne 1 -or [string](Get-PandoraRotationProperty `
        (Get-PandoraRotationProperty $matches[0] 'metadata') 'uid') -cne $ownerUid) {
        return "GameServer/$name owner GameServerSet/$ownerName UID 漂移/孤儿"
    }
    $gssOwners = @(@(Get-PandoraRotationProperty (Get-PandoraRotationProperty $matches[0] 'metadata') 'ownerReferences' @()) |
        Where-Object { (Get-PandoraRotationProperty $_ 'controller' $false) -eq $true })
    $fleetLabel = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $metadata 'labels') 'agones.dev/fleet')
    if ($gssOwners.Count -ne 1 -or [string](Get-PandoraRotationProperty $gssOwners[0] 'kind') -cne 'Fleet' -or
        [string](Get-PandoraRotationProperty $gssOwners[0] 'name') -cne $fleetLabel) {
        return "GameServer/$name Fleet label 与 GameServerSet/$ownerName owner 链不一致"
    }
    return ''
}

function Get-PandoraDSTicketWorkloadRevisionFromAllowedSet {
    param(
        [Parameter(Mandatory = $true)]$Spec,
        [Parameter(Mandatory = $true)][string]$ContainerName,
        [Parameter(Mandatory = $true)][int[]]$AllowedRevisions,
        [Parameter(Mandatory = $true)][string]$ObjectName
    )
    $allowed = @($AllowedRevisions | Sort-Object -Unique)
    if ($allowed.Count -eq 0 -or @($allowed | Where-Object { $_ -lt 1 }).Count -gt 0) {
        throw "$ObjectName AllowedRevisions 必须是非空正整数集合。"
    }
    $matches = [System.Collections.Generic.List[int]]::new()
    foreach ($revision in $allowed) {
        $failures = @(Test-PandoraDSTicketWorkloadSpecRevision -Spec $Spec -ContainerName $ContainerName `
            -Revision $revision -ObjectName $ObjectName)
        if ($failures.Count -eq 0) { $matches.Add($revision) }
    }
    if ($matches.Count -ne 1) {
        $probe = @(Test-PandoraDSTicketWorkloadSpecRevision -Spec $Spec -ContainerName $ContainerName `
            -Revision $allowed[0] -ObjectName $ObjectName)
        throw "$ObjectName DSTicket tuple 不属于允许 revision {$($allowed -join ',')}：$($probe -join '; ')"
    }
    return $matches[0]
}

function Test-PandoraDSTicketDSRevisionSetGate {
    param(
        [Parameter(Mandatory = $true)][object[]]$FleetObjects,
        [Parameter(Mandatory = $true)][object[]]$GameServerObjects,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$GameServerSetObjects,
        [Parameter(Mandatory = $true)][object[]]$PodObjects,
        [Parameter(Mandatory = $true)][int[]]$AllowedRevisions
    )
    $allowed = @($AllowedRevisions | Sort-Object -Unique)
    if ($allowed.Count -eq 0 -or @($allowed | Where-Object { $_ -lt 1 }).Count -gt 0) {
        throw 'AllowedRevisions 必须是非空正整数集合。'
    }
    $failures = [System.Collections.Generic.List[string]]::new()
    $gssGate = Test-PandoraDSTicketGameServerSetRevisionGate -FleetObjects $FleetObjects `
        -GameServerSetObjects $GameServerSetObjects -AllowedActiveRevisions $allowed
    foreach ($failure in @($gssGate.Failures)) { $failures.Add($failure) }
    $fleetByName = @{}
    foreach ($fleet in @($FleetObjects)) {
        $metadata = Get-PandoraRotationProperty $fleet 'metadata'
        $name = [string](Get-PandoraRotationProperty $metadata 'name')
        if ($script:PandoraDSTicketFleetNames -cnotcontains $name) { $failures.Add("检测到未知 DSTicket/DS parent Fleet/$name"); continue }
        if ($fleetByName.ContainsKey($name)) { $failures.Add("Fleet/$name 重复") } else { $fleetByName[$name] = $fleet }
    }
    foreach ($fleetName in $script:PandoraDSTicketFleetNames) {
        if (-not $fleetByName.ContainsKey($fleetName)) { $failures.Add("Fleet/$fleetName 缺失"); continue }
        $fleet = $fleetByName[$fleetName]
        $metadata = Get-PandoraRotationProperty $fleet 'metadata'
        if ([string](Get-PandoraRotationProperty $metadata 'namespace') -cne 'default' -or
            -not [string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty $metadata 'deletionTimestamp'))) {
            $failures.Add("Fleet/$fleetName namespace 非 default 或正在终止")
        }
        $containerName = if ($fleetName.StartsWith('pandora-battle-', [StringComparison]::Ordinal)) { 'pandora-battle-ds' } else { 'pandora-hub-ds' }
        $podSpec = Get-PandoraRotationProperty (Get-PandoraRotationProperty (Get-PandoraRotationProperty `
            (Get-PandoraRotationProperty (Get-PandoraRotationProperty $fleet 'spec') 'template') 'spec') 'template') 'spec'
        try { $null = Get-PandoraDSTicketWorkloadRevisionFromAllowedSet $podSpec $containerName $allowed "Fleet/$fleetName" }
        catch { $failures.Add($_.Exception.Message) }
    }

    $terminalStates = @('Shutdown', 'Error', 'Unhealthy')
    $liveStates = @('Ready', 'Allocated', 'Reserved')
    $managedNonterminal = [System.Collections.Generic.List[object]]::new()
    $gsRevisionByName = @{}
    foreach ($gameServer in @($GameServerObjects)) {
        $metadata = Get-PandoraRotationProperty $gameServer 'metadata'
        if ([string](Get-PandoraRotationProperty $metadata 'namespace') -cne 'default') { continue }
        $name = [string](Get-PandoraRotationProperty $metadata 'name')
        $state = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $gameServer 'status') 'state')
        $fleetName = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $metadata 'labels') 'agones.dev/fleet')
        if (-not [string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty $metadata 'deletionTimestamp'))) {
            $failures.Add("GameServer/$name 正在终止；revision 集合门禁必须等对象真正消失")
        }
        $ownerFailure = Test-PandoraGameServerControllerOwnerChain $gameServer $GameServerSetObjects
        if (-not [string]::IsNullOrWhiteSpace($ownerFailure)) { $failures.Add($ownerFailure) }
        if ($script:PandoraDSTicketFleetNames -cnotcontains $fleetName) {
            $failures.Add("GameServer/$name 属于未受管 Fleet '$fleetName'(state=$state)")
            continue
        }
        if ($terminalStates -ccontains $state) { continue }
        $managedNonterminal.Add($gameServer)
        $containerName = if ($fleetName.StartsWith('pandora-battle-', [StringComparison]::Ordinal)) { 'pandora-battle-ds' } else { 'pandora-hub-ds' }
        $podSpec = Get-PandoraRotationProperty (Get-PandoraRotationProperty `
            (Get-PandoraRotationProperty $gameServer 'spec') 'template') 'spec'
        try {
            $gsRevisionByName[$name] = Get-PandoraDSTicketWorkloadRevisionFromAllowedSet $podSpec $containerName $allowed "GameServer/$name"
        } catch { $failures.Add($_.Exception.Message) }
        if ($liveStates -ccontains $state) {
            $matches = @($PodObjects | Where-Object {
                [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $_ 'metadata') 'namespace') -ceq 'default' -and
                (Test-PandoraPodMatchesGameServer -Pod $_ -GameServer $gameServer)
            })
            if ($matches.Count -ne 1) { $failures.Add("GameServer/$name 对应 Pod 数量=$($matches.Count)，revision 集合门禁要求 live GS 恰有一个 Pod") }
        }
    }

    foreach ($pod in @($PodObjects)) {
        $metadata = Get-PandoraRotationProperty $pod 'metadata'
        if ([string](Get-PandoraRotationProperty $metadata 'namespace') -cne 'default') { continue }
        $podName = [string](Get-PandoraRotationProperty $metadata 'name')
        $containerNames = @(@(Get-PandoraRotationProperty (Get-PandoraRotationProperty $pod 'spec') 'containers' @()) |
            ForEach-Object { [string](Get-PandoraRotationProperty $_ 'name') } |
            Where-Object { $_ -cin @('pandora-battle-ds', 'pandora-hub-ds') })
        $hasClue = Test-PandoraDSTicketDSPodSpecClue (Get-PandoraRotationProperty $pod 'spec')
        if (-not $hasClue) { continue }
        if ($containerNames.Count -ne 1) { $failures.Add("Pod/$podName DS 主容器数量=$($containerNames.Count)"); continue }
        if (-not [string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty $metadata 'deletionTimestamp'))) {
            $failures.Add("Pod/$podName 正在终止；revision 集合门禁必须等对象真正消失")
        }
        $podRevision = $null
        try {
            $podRevision = Get-PandoraDSTicketWorkloadRevisionFromAllowedSet (Get-PandoraRotationProperty $pod 'spec') `
                $containerNames[0] $allowed "Pod/$podName"
        } catch { $failures.Add($_.Exception.Message) }
        $owners = @($managedNonterminal | Where-Object { Test-PandoraPodMatchesGameServer -Pod $pod -GameServer $_ })
        if ($owners.Count -ne 1) {
            $failures.Add("Pod/$podName 含 DS 主容器但受管非终态 GameServer owner 数量=$($owners.Count)")
            continue
        }
        $gsName = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $owners[0] 'metadata') 'name')
        if ($null -ne $podRevision -and $gsRevisionByName.ContainsKey($gsName) -and
            [int]$podRevision -ne [int]$gsRevisionByName[$gsName]) {
            $failures.Add("GameServer/$gsName 与 Pod/$podName revision 不一致($($gsRevisionByName[$gsName])/$podRevision)")
        }
    }
    return [pscustomobject]@{ Ok = ($failures.Count -eq 0); Failures = [string[]]$failures.ToArray(); AllowedRevisions = [int[]]$allowed }
}

function Assert-PandoraDSTicketDSRevisionSetGate {
    param(
        [Parameter(Mandatory = $true)][object[]]$FleetObjects,
        [Parameter(Mandatory = $true)][object[]]$GameServerObjects,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$GameServerSetObjects,
        [Parameter(Mandatory = $true)][object[]]$PodObjects,
        [Parameter(Mandatory = $true)][int[]]$AllowedRevisions
    )
    $result = Test-PandoraDSTicketDSRevisionSetGate -FleetObjects $FleetObjects `
        -GameServerObjects $GameServerObjects -GameServerSetObjects $GameServerSetObjects `
        -PodObjects $PodObjects -AllowedRevisions $AllowedRevisions
    if (-not $result.Ok) { throw "DSTicket DS revision 集合门禁失败:$($result.Failures -join '; ')" }
    return $result
}

function Test-PandoraDSTicketLiveDSRevisionGate {
    param(
        [Parameter(Mandatory = $true)][object[]]$FleetObjects,
        [Parameter(Mandatory = $true)][object[]]$GameServerObjects,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$GameServerSetObjects,
        [Parameter(Mandatory = $true)][object[]]$PodObjects,
        [Parameter(Mandatory = $true)][int]$TargetRevision
    )
    if ($TargetRevision -lt 1) { throw 'TargetRevision 必须 >= 1。' }
    $failures = [System.Collections.Generic.List[string]]::new()
    $gssGate = Test-PandoraDSTicketGameServerSetRevisionGate -FleetObjects $FleetObjects `
        -GameServerSetObjects $GameServerSetObjects -AllowedActiveRevisions @($TargetRevision)
    foreach ($failure in @($gssGate.Failures)) { $failures.Add($failure) }
    $fleetByName = @{}
    foreach ($fleet in @($FleetObjects)) {
        $metadata = Get-PandoraRotationProperty $fleet 'metadata'
        $name = [string](Get-PandoraRotationProperty $metadata 'name')
        if ($script:PandoraDSTicketFleetNames -cnotcontains $name) { $failures.Add("检测到未知 DSTicket/DS parent Fleet/$name"); continue }
        if ($fleetByName.ContainsKey($name)) { $failures.Add("Fleet/$name 重复") } else { $fleetByName[$name] = $fleet }
    }
    foreach ($fleetName in $script:PandoraDSTicketFleetNames) {
        if (-not $fleetByName.ContainsKey($fleetName)) { $failures.Add("Fleet/$fleetName 缺失"); continue }
        $fleet = $fleetByName[$fleetName]
        $metadata = Get-PandoraRotationProperty $fleet 'metadata'
        if ([string](Get-PandoraRotationProperty $metadata 'namespace') -cne 'default') {
            $failures.Add("Fleet/$fleetName namespace 不是 default")
        }
        if (-not [string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty $metadata 'deletionTimestamp'))) {
            $failures.Add("Fleet/$fleetName 正在终止")
        }
        $containerName = if ($fleetName.StartsWith('pandora-battle-', [StringComparison]::Ordinal)) { 'pandora-battle-ds' } else { 'pandora-hub-ds' }
        $spec = Get-PandoraRotationProperty (Get-PandoraRotationProperty (Get-PandoraRotationProperty `
            (Get-PandoraRotationProperty $fleet 'spec') 'template') 'spec') 'template'
        $podSpec = Get-PandoraRotationProperty $spec 'spec'
        foreach ($failure in @(Test-PandoraDSTicketWorkloadSpecRevision -Spec $podSpec -ContainerName $containerName `
            -Revision $TargetRevision -ObjectName "Fleet/$fleetName")) { $failures.Add($failure) }
    }

    $liveStates = @('Ready', 'Allocated', 'Reserved')
    $terminalStates = @('Shutdown', 'Error', 'Unhealthy')
    $liveCounts = @{}
    foreach ($name in $script:PandoraDSTicketFleetNames) { $liveCounts[$name] = 0 }
    $liveNames = [System.Collections.Generic.List[string]]::new()
    $matchedPodNames = @{}
    foreach ($gameServer in @($GameServerObjects)) {
        $metadata = Get-PandoraRotationProperty $gameServer 'metadata'
        if ([string](Get-PandoraRotationProperty $metadata 'namespace') -cne 'default') { continue }
        $isDeleting = -not [string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty $metadata 'deletionTimestamp'))
        $name = [string](Get-PandoraRotationProperty $metadata 'name')
        $state = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $gameServer 'status') 'state')
        $fleetName = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $metadata 'labels') 'agones.dev/fleet')
        $ownerFailure = Test-PandoraGameServerControllerOwnerChain $gameServer $GameServerSetObjects
        if (-not [string]::IsNullOrWhiteSpace($ownerFailure)) { $failures.Add($ownerFailure) }
        if ($isDeleting) { $failures.Add("GameServer/$name 正在终止；终态对象也必须等真正消失") }
        if ($script:PandoraDSTicketFleetNames -cnotcontains $fleetName) {
            $failures.Add("GameServer/$name 属于未受管 Fleet '$fleetName'(state=$state)")
            continue
        }
        if ($terminalStates -ccontains $state) { continue }
        if ($liveStates -cnotcontains $state) {
            $failures.Add("GameServer/$name 状态=$state 尚未稳定到 Ready/Allocated/Reserved")
            continue
        }
        $liveCounts[$fleetName] = [int]$liveCounts[$fleetName] + 1
        $liveNames.Add($name)
        $containerName = if ($fleetName.StartsWith('pandora-battle-', [StringComparison]::Ordinal)) { 'pandora-battle-ds' } else { 'pandora-hub-ds' }
        $gameServerPodSpec = Get-PandoraRotationProperty (Get-PandoraRotationProperty `
            (Get-PandoraRotationProperty $gameServer 'spec') 'template') 'spec'
        foreach ($failure in @(Test-PandoraDSTicketWorkloadSpecRevision -Spec $gameServerPodSpec -ContainerName $containerName `
            -Revision $TargetRevision -ObjectName "GameServer/$name")) { $failures.Add($failure) }

        $podMatches = @($PodObjects | Where-Object {
            [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $_ 'metadata') 'namespace') -ceq 'default' -and
            (Test-PandoraPodMatchesGameServer -Pod $_ -GameServer $gameServer)
        })
        if ($podMatches.Count -ne 1) {
            $failures.Add("GameServer/$name 对应非终止 Pod 数量=$($podMatches.Count)")
            continue
        }
        $pod = $podMatches[0]
        $podName = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $pod 'metadata') 'name')
        $matchedPodNames[$podName] = $true
        if (-not [string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $pod 'metadata') 'deletionTimestamp'))) {
            $failures.Add("Pod/$podName 仍在 termination grace；终态门禁必须等对象消失")
        }
        if ([string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $pod 'status') 'phase') -cne 'Running') {
            $failures.Add("Pod/$podName phase 不是 Running")
        }
        $ready = @(@(Get-PandoraRotationProperty (Get-PandoraRotationProperty $pod 'status') 'conditions' @()) |
            Where-Object { [string](Get-PandoraRotationProperty $_ 'type') -ceq 'Ready' -and [string](Get-PandoraRotationProperty $_ 'status') -ceq 'True' })
        if ($ready.Count -ne 1) { $failures.Add("Pod/$podName 未 Ready") }
        foreach ($failure in @(Test-PandoraDSTicketWorkloadSpecRevision -Spec (Get-PandoraRotationProperty $pod 'spec') `
            -ContainerName $containerName -Revision $TargetRevision -ObjectName "Pod/$podName")) { $failures.Add($failure) }
    }
    foreach ($fleetName in $script:PandoraDSTicketFleetNames) {
        if (-not $fleetByName.ContainsKey($fleetName)) { continue }
        $replicasText = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $fleetByName[$fleetName] 'spec') 'replicas' '0')
        [int]$replicas = 0
        if (-not [int]::TryParse($replicasText, [ref]$replicas) -or $replicas -lt 0) {
            $failures.Add("Fleet/$fleetName spec.replicas 非法:$replicasText")
        } elseif ($replicas -gt 0 -and [int]$liveCounts[$fleetName] -lt 1) {
            $failures.Add("Fleet/$fleetName replicas=$replicas 但无稳定 live GameServer")
        }
    }
    foreach ($pod in @($PodObjects)) {
        $podMeta = Get-PandoraRotationProperty $pod 'metadata'
        if ([string](Get-PandoraRotationProperty $podMeta 'namespace') -cne 'default') { continue }
        $podName = [string](Get-PandoraRotationProperty $podMeta 'name')
        if ((Test-PandoraDSTicketDSPodSpecClue (Get-PandoraRotationProperty $pod 'spec')) -and
            -not $matchedPodNames.ContainsKey($podName)) {
            $failures.Add("Pod/$podName 含 DSTicket DS/verifier material 线索但没有受管 live GameServer owner（孤儿/改名 Pod）")
        }
    }
    return [pscustomobject]@{
        Ok = ($failures.Count -eq 0)
        Failures = [string[]]$failures.ToArray()
        LiveGameServers = [string[]]$liveNames.ToArray()
        TargetRevision = $TargetRevision
    }
}

function Assert-PandoraDSTicketLiveDSRevisionGate {
    param(
        [Parameter(Mandatory = $true)][object[]]$FleetObjects,
        [Parameter(Mandatory = $true)][object[]]$GameServerObjects,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$GameServerSetObjects,
        [Parameter(Mandatory = $true)][object[]]$PodObjects,
        [Parameter(Mandatory = $true)][int]$TargetRevision
    )
    $result = Test-PandoraDSTicketLiveDSRevisionGate -FleetObjects $FleetObjects `
        -GameServerObjects $GameServerObjects -GameServerSetObjects $GameServerSetObjects `
        -PodObjects $PodObjects -TargetRevision $TargetRevision
    if (-not $result.Ok) {
        throw "DSTicket live DS revision r$TargetRevision 门禁失败:$($result.Failures -join '; ')"
    }
    return $result
}

function Test-PandoraDeploymentVolumeReference {
    param(
        [Parameter(Mandatory = $true)]$Deployment,
        [Parameter(Mandatory = $true)][string]$VolumeName,
        [Parameter(Mandatory = $true)][ValidateSet('secret', 'configMap')][string]$ReferenceKind,
        [Parameter(Mandatory = $true)][string]$ExpectedName
    )
    $podSpec = Get-PandoraRotationProperty (Get-PandoraRotationProperty `
        (Get-PandoraRotationProperty (Get-PandoraRotationProperty $Deployment 'spec') 'template') 'spec') 'volumes' @()
    $volumes = @(Get-PandoraRotationNamedItems -Items $podSpec -Name $VolumeName)
    if ($volumes.Count -ne 1) { return $false }
    $reference = Get-PandoraRotationProperty $volumes[0] $ReferenceKind
    $field = if ($ReferenceKind -ceq 'secret') { 'secretName' } else { 'name' }
    return [string](Get-PandoraRotationProperty $reference $field) -ceq $ExpectedName
}

function Get-PandoraDeploymentRollingValue {
    param(
        $Value,
        [Parameter(Mandatory = $true)][int]$Replicas,
        [Parameter(Mandatory = $true)][bool]$RoundUp,
        [Parameter(Mandatory = $true)][string]$Where
    )
    if ($null -eq $Value -or [string]::IsNullOrWhiteSpace([string]$Value)) { throw "$Where 为空。" }
    $text = [string]$Value
    if ($text -cmatch '^(?<percent>[0-9]{1,3})%$') {
        $percent = [int]$Matches['percent']
        if ($percent -gt 100) { throw "$Where 百分比超出 0..100:$text。" }
        $raw = [double]$Replicas * [double]$percent / 100.0
        return [int]$(if ($RoundUp) { [Math]::Ceiling($raw) } else { [Math]::Floor($raw) })
    }
    [int]$number = 0
    if (-not [int]::TryParse($text, [ref]$number) -or $number -lt 0) { throw "$Where 非法:$text。" }
    return $number
}

function Assert-PandoraDeploymentSafeRollingStrategy {
    param([Parameter(Mandatory = $true)]$Deployment)
    $name = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $Deployment 'metadata') 'name')
    $spec = Get-PandoraRotationProperty $Deployment 'spec'
    $replicas = [int](Get-PandoraRotationProperty $spec 'replicas' 1)
    $strategy = Get-PandoraRotationProperty $spec 'strategy'
    $type = [string](Get-PandoraRotationProperty $strategy 'type' 'RollingUpdate')
    if ([string]::IsNullOrWhiteSpace($type)) { $type = 'RollingUpdate' }
    if ($name -ceq 'hub-allocator') {
        # hub-allocator writes the assignment/capacity ledger.  During the
        # successor-lease rollout an old writer cannot overlap a new writer,
        # so this one signer is deliberately unavailable for the short
        # Recreate window.  Keep the exception exact; it must never become a
        # general escape hatch from the signer availability contract.
        if ($replicas -ne 1 -or $type -cne 'Recreate') {
            throw "Deployment/$name 必须是 replicas=1 + Recreate 的唯一单写者；检测到 replicas=$replicas strategy=$type。"
        }
        if ($null -ne (Get-PandoraRotationProperty $strategy 'rollingUpdate')) {
            throw "Deployment/$name strategy=Recreate 禁止携带 rollingUpdate。"
        }
        return
    }
    if ($replicas -lt 1) { throw "Deployment/$name replicas=$replicas，不能不停服轮换。" }
    if ($type -cne 'RollingUpdate') { throw "Deployment/$name strategy=$type，不能不停服轮换。" }
    $rolling = Get-PandoraRotationProperty $strategy 'rollingUpdate'
    $maxUnavailableRaw = Get-PandoraRotationProperty $rolling 'maxUnavailable' '25%'
    $maxSurgeRaw = Get-PandoraRotationProperty $rolling 'maxSurge' '25%'
    $maxUnavailable = Get-PandoraDeploymentRollingValue -Value $maxUnavailableRaw -Replicas $replicas `
        -RoundUp $false -Where "Deployment/$name maxUnavailable"
    $maxSurge = Get-PandoraDeploymentRollingValue -Value $maxSurgeRaw -Replicas $replicas `
        -RoundUp $true -Where "Deployment/$name maxSurge"
    if ($maxUnavailable -ge $replicas -or $maxSurge -lt 1) {
        throw "Deployment/$name 滚动策略会先耗尽旧 signer 或无法拉起新 Pod:replicas=$replicas maxUnavailable=$maxUnavailable maxSurge=$maxSurge。"
    }
}

function Get-PandoraWorkloadVolumeReferenceName {
    param(
        [Parameter(Mandatory = $true)]$PodSpec,
        [Parameter(Mandatory = $true)][string]$VolumeName,
        [Parameter(Mandatory = $true)][ValidateSet('secret', 'configMap')][string]$ReferenceKind,
        [Parameter(Mandatory = $true)][string]$Where
    )
    $matches = @(Get-PandoraRotationNamedItems (Get-PandoraRotationProperty $PodSpec 'volumes' @()) $VolumeName)
    if ($matches.Count -ne 1) { throw "$Where volume/$VolumeName 数量=$($matches.Count)。" }
    $reference = Get-PandoraRotationProperty $matches[0] $ReferenceKind
    $field = if ($ReferenceKind -ceq 'secret') { 'secretName' } else { 'name' }
    $name = [string](Get-PandoraRotationProperty $reference $field)
    if ([string]::IsNullOrWhiteSpace($name)) { throw "$Where volume/$VolumeName 缺 $ReferenceKind/$field。" }
    return $name
}

function Test-PandoraReplicaSetsMatchOwningDeploymentGate {
    param(
        [Parameter(Mandatory = $true)][object[]]$DeploymentObjects,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$ReplicaSetObjects
    )
    $failures = [System.Collections.Generic.List[string]]::new()
    $deploymentByName = @{}
    $activeByDeployment = @{}
    foreach ($deployment in @($DeploymentObjects)) {
        $metadata = Get-PandoraRotationProperty $deployment 'metadata'
        $name = [string](Get-PandoraRotationProperty $metadata 'name')
        if ($script:PandoraDSTicketSignerNames -cnotcontains $name) { $failures.Add("检测到未知 DSTicket signer parent Deployment/$name"); continue }
        if ($deploymentByName.ContainsKey($name)) { $failures.Add("Deployment/$name 重复"); continue }
        if ([string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty $metadata 'uid'))) {
            $failures.Add("Deployment/$name 缺 UID")
        }
        $deploymentByName[$name] = $deployment
        $activeByDeployment[$name] = 0
    }
    $seen = @{}
    foreach ($rs in @($ReplicaSetObjects)) {
        $metadata = Get-PandoraRotationProperty $rs 'metadata'
        $name = [string](Get-PandoraRotationProperty $metadata 'name')
        $where = "ReplicaSet/$name"
        if ([string]::IsNullOrWhiteSpace($name) -or $seen.ContainsKey($name)) { $failures.Add("ReplicaSet name 为空或重复:$name"); continue }
        $seen[$name] = $true
        if ([string](Get-PandoraRotationProperty $metadata 'namespace') -cne 'pandora' -or
            -not [string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty $metadata 'deletionTimestamp'))) {
            $failures.Add("$where namespace 非 pandora 或正在终止")
        }
        if ([string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty $metadata 'uid'))) { $failures.Add("$where 缺 UID") }
        $owners = @(@(Get-PandoraRotationProperty $metadata 'ownerReferences' @()) | Where-Object {
            (Get-PandoraRotationProperty $_ 'controller' $false) -eq $true
        })
        if ($owners.Count -ne 1 -or [string](Get-PandoraRotationProperty $owners[0] 'kind') -cne 'Deployment') {
            $failures.Add("$where 必须恰有一个且类型为 Deployment 的 controller owner"); continue
        }
        $ownerName = [string](Get-PandoraRotationProperty $owners[0] 'name')
        $ownerUid = [string](Get-PandoraRotationProperty $owners[0] 'uid')
        if ([string]::IsNullOrWhiteSpace($ownerName) -or [string]::IsNullOrWhiteSpace($ownerUid) -or
            -not $deploymentByName.ContainsKey($ownerName) -or $ownerUid -cne
            [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $deploymentByName[$ownerName] 'metadata') 'uid')) {
            $failures.Add("$where owner Deployment/$ownerName UID 漂移/孤儿"); continue
        }
        $spec = Get-PandoraRotationProperty $rs 'spec'
        $status = Get-PandoraRotationProperty $rs 'status'
        try {
            $counts = @(
                Get-PandoraNonNegativeControllerCount $spec 'replicas' "$where spec"
                Get-PandoraNonNegativeControllerCount $status 'replicas' "$where status"
                Get-PandoraNonNegativeControllerCount $status 'readyReplicas' "$where status"
                Get-PandoraNonNegativeControllerCount $status 'availableReplicas' "$where status"
            )
        } catch { $failures.Add($_.Exception.Message); continue }
        if (@($counts | Where-Object { $_ -gt 0 }).Count -eq 0) { continue }
        $activeByDeployment[$ownerName] = [int]$activeByDeployment[$ownerName] + 1
        try {
            $deploymentPodSpec = Get-PandoraRotationProperty (Get-PandoraRotationProperty `
                (Get-PandoraRotationProperty $deploymentByName[$ownerName] 'spec') 'template') 'spec'
            $rsPodSpec = Get-PandoraRotationProperty (Get-PandoraRotationProperty $spec 'template') 'spec'
            foreach ($volumeName in @('conf', 'dsticket')) {
                $expected = Get-PandoraWorkloadVolumeReferenceName $deploymentPodSpec $volumeName secret "Deployment/$ownerName"
                $actual = Get-PandoraWorkloadVolumeReferenceName $rsPodSpec $volumeName secret $where
                if ($actual -cne $expected) { throw "$where $volumeName=$actual 与 Deployment/$ownerName 当前模板=$expected 不一致。" }
            }
            if ($ownerName -ceq 'login') {
                $expected = Get-PandoraWorkloadVolumeReferenceName $deploymentPodSpec dsticket-jwks configMap 'Deployment/login'
                $actual = Get-PandoraWorkloadVolumeReferenceName $rsPodSpec dsticket-jwks configMap $where
                if ($actual -cne $expected) { throw "$where Login JWKS=$actual 与 Deployment/login 当前模板=$expected 不一致。" }
            }
        } catch { $failures.Add($_.Exception.Message) }
    }
    foreach ($name in $deploymentByName.Keys) {
        $replicas = Get-PandoraNonNegativeControllerCount `
            (Get-PandoraRotationProperty $deploymentByName[$name] 'spec') 'replicas' "Deployment/$name spec"
        if ($replicas -gt 0 -and [int]$activeByDeployment[$name] -lt 1) {
            $failures.Add("Deployment/$name replicas=$replicas 但没有非零 owned ReplicaSet")
        }
    }
    return [pscustomobject]@{ Ok = ($failures.Count -eq 0); Failures = [string[]]$failures.ToArray() }
}

function Assert-PandoraReplicaSetsMatchOwningDeploymentGate {
    param(
        [Parameter(Mandatory = $true)][object[]]$DeploymentObjects,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$ReplicaSetObjects
    )
    $result = Test-PandoraReplicaSetsMatchOwningDeploymentGate $DeploymentObjects $ReplicaSetObjects
    if (-not $result.Ok) { throw "DSTicket ReplicaSet owner/template 门禁失败:$($result.Failures -join '; ')" }
    return $result
}

function Test-PandoraDSTicketReplicaSetRevisionGate {
    param(
        [Parameter(Mandatory = $true)][object[]]$DeploymentObjects,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$ReplicaSetObjects,
        [Parameter(Mandatory = $true)][int]$SignerRevision,
        [Parameter(Mandatory = $true)][int]$LoginKeysetRevision,
        [hashtable]$ExpectedConfigSecretByDeployment = @{}
    )
    $failures = [System.Collections.Generic.List[string]]::new()
    $deploymentUidByName = @{}
    $deploymentByName = @{}
    $activeReplicaSetCountByDeployment = @{}
    $expectedSignerSecret = "pandora-dsticket-signer-r$SignerRevision"
    foreach ($deployment in @($DeploymentObjects)) {
        $metadata = Get-PandoraRotationProperty $deployment 'metadata'
        $name = [string](Get-PandoraRotationProperty $metadata 'name')
        if ($script:PandoraDSTicketSignerNames -cnotcontains $name) { $failures.Add("检测到未知 DSTicket signer parent Deployment/$name"); continue }
        if ($deploymentUidByName.ContainsKey($name)) { $failures.Add("Deployment/$name 重复，无法验证 ReplicaSet owner UID"); continue }
        $uid = [string](Get-PandoraRotationProperty $metadata 'uid')
        if ([string]::IsNullOrWhiteSpace($uid)) { $failures.Add("Deployment/$name 缺 UID，无法验证 ReplicaSet owner"); continue }
        $deploymentUidByName[$name] = $uid
        $deploymentByName[$name] = $deployment
        $activeReplicaSetCountByDeployment[$name] = 0
        $deploymentPodSpec = Get-PandoraRotationProperty (Get-PandoraRotationProperty `
            (Get-PandoraRotationProperty $deployment 'spec') 'template') 'spec'
        $signerReferences = @(Get-PandoraDSTicketSignerSecretReferences $deploymentPodSpec)
        if ($signerReferences.Count -ne 1 -or $signerReferences[0].Kind -cne 'DirectVolume' -or
            $signerReferences[0].Location -cne 'volume/dsticket' -or
            $signerReferences[0].Name -cne $expectedSignerSecret) {
            $failures.Add("Deployment/$name signer 私钥引用必须恰为一个 DirectVolume:volume/dsticket:$expectedSignerSecret；检测到 $($signerReferences.Count) 个")
        }
        $expectedConfig = if ($ExpectedConfigSecretByDeployment.ContainsKey($name)) {
            [string]$ExpectedConfigSecretByDeployment[$name]
        } else { 'pandora-config' }
        $expectedJwks = if ($name -ceq 'login') { "pandora-dsticket-jwks-r$LoginKeysetRevision" } else { '' }
        foreach ($failure in @(Test-PandoraDSTicketSignerPodSpecReferenceContract -Spec $deploymentPodSpec `
            -ServiceName $name -ExpectedConfigSecret $expectedConfig -ExpectedSignerSecret $expectedSignerSecret `
            -ExpectedLoginJwks $expectedJwks -ObjectName "Deployment/$name")) {
            $failures.Add($failure)
        }
    }
    $seen = @{}
    foreach ($replicaSet in @($ReplicaSetObjects)) {
        $metadata = Get-PandoraRotationProperty $replicaSet 'metadata'
        $name = [string](Get-PandoraRotationProperty $metadata 'name')
        $where = "ReplicaSet/$name"
        if ([string]::IsNullOrWhiteSpace($name) -or $seen.ContainsKey($name)) {
            $failures.Add("ReplicaSet name 为空或重复:$name"); continue
        }
        $seen[$name] = $true
        if ([string](Get-PandoraRotationProperty $metadata 'namespace') -cne 'pandora') { $failures.Add("$where namespace 不是 pandora") }
        if ([string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty $metadata 'uid'))) { $failures.Add("$where 缺 UID") }
        if (-not [string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty $metadata 'deletionTimestamp'))) {
            $failures.Add("$where 正在终止；必须等对象真正消失以关闭异步补出窗口")
        }
        $owners = @(@(Get-PandoraRotationProperty $metadata 'ownerReferences' @()) | Where-Object {
            (Get-PandoraRotationProperty $_ 'controller' $false) -eq $true
        })
        if ($owners.Count -ne 1 -or [string](Get-PandoraRotationProperty $owners[0] 'kind') -cne 'Deployment') {
            $failures.Add("$where 必须恰有一个且类型为 Deployment 的 controller owner"); continue
        }
        $deploymentName = [string](Get-PandoraRotationProperty $owners[0] 'name')
        $ownerUid = [string](Get-PandoraRotationProperty $owners[0] 'uid')
        if ([string]::IsNullOrWhiteSpace($deploymentName) -or [string]::IsNullOrWhiteSpace($ownerUid) -or
            $script:PandoraDSTicketSignerNames -cnotcontains $deploymentName -or
            -not $deploymentUidByName.ContainsKey($deploymentName) -or
            $ownerUid -cne [string]$deploymentUidByName[$deploymentName]) {
            $failures.Add("$where owner Deployment/$deploymentName UID 漂移/孤儿"); continue
        }
        $spec = Get-PandoraRotationProperty $replicaSet 'spec'
        $status = Get-PandoraRotationProperty $replicaSet 'status'
        $counts = [System.Collections.Generic.List[int]]::new()
        try {
            $counts.Add((Get-PandoraNonNegativeControllerCount $spec 'replicas' "$where spec"))
            foreach ($field in @('replicas', 'readyReplicas', 'availableReplicas', 'fullyLabeledReplicas')) {
                $counts.Add((Get-PandoraNonNegativeControllerCount $status $field "$where status"))
            }
        } catch { $failures.Add($_.Exception.Message); continue }
        if (@($counts | Where-Object { $_ -gt 0 }).Count -eq 0) { continue }
        $activeReplicaSetCountByDeployment[$deploymentName] = [int]$activeReplicaSetCountByDeployment[$deploymentName] + 1
        $expectedConfig = if ($ExpectedConfigSecretByDeployment.ContainsKey($deploymentName)) {
            [string]$ExpectedConfigSecretByDeployment[$deploymentName]
        } else { 'pandora-config' }
        $replicaSetTemplate = Get-PandoraRotationProperty $spec 'template'
        $replicaSetPodSpec = Get-PandoraRotationProperty $replicaSetTemplate 'spec'
        $asDeployment = [pscustomobject]@{
            spec = [pscustomobject]@{ template = [pscustomobject]@{ spec = $replicaSetPodSpec } }
        }
        if (-not (Test-PandoraDeploymentVolumeReference $asDeployment conf secret $expectedConfig)) {
            $failures.Add("$where 非零但未引用目标配置 Secret/$expectedConfig")
        }
        $signerReferences = @(Get-PandoraDSTicketSignerSecretReferences $replicaSetPodSpec)
        if ($signerReferences.Count -ne 1 -or $signerReferences[0].Kind -cne 'DirectVolume' -or
            $signerReferences[0].Location -cne 'volume/dsticket' -or
            $signerReferences[0].Name -cne $expectedSignerSecret) {
            $failures.Add("$where 非零但未引用目标 signer Secret/$expectedSignerSecret 或存在额外引用；必须恰为一个 DirectVolume:volume/dsticket，检测到 $($signerReferences.Count) 个")
        }
        if ($deploymentName -ceq 'login' -and
            -not (Test-PandoraDeploymentVolumeReference $asDeployment dsticket-jwks configMap "pandora-dsticket-jwks-r$LoginKeysetRevision")) {
            $failures.Add("$where 非零但未引用目标 Login JWKS r$LoginKeysetRevision")
        }
        $expectedJwks = if ($deploymentName -ceq 'login') { "pandora-dsticket-jwks-r$LoginKeysetRevision" } else { '' }
        foreach ($failure in @(Test-PandoraDSTicketSignerPodSpecReferenceContract -Spec $replicaSetPodSpec `
            -ServiceName $deploymentName -ExpectedConfigSecret $expectedConfig -ExpectedSignerSecret $expectedSignerSecret `
            -ExpectedLoginJwks $expectedJwks -ObjectName $where)) {
            $failures.Add($failure)
        }
    }
    foreach ($deploymentName in $deploymentByName.Keys) {
        $replicas = Get-PandoraNonNegativeControllerCount `
            (Get-PandoraRotationProperty $deploymentByName[$deploymentName] 'spec') 'replicas' "Deployment/$deploymentName spec"
        if ($replicas -gt 0 -and [int]$activeReplicaSetCountByDeployment[$deploymentName] -lt 1) {
            $failures.Add("Deployment/$deploymentName replicas=$replicas 但没有非零 owned ReplicaSet，无法关闭异步补出窗口")
        }
    }
    return [pscustomobject]@{ Ok = ($failures.Count -eq 0); Failures = [string[]]$failures.ToArray() }
}

function Assert-PandoraDSTicketReplicaSetRevisionGate {
    param(
        [Parameter(Mandatory = $true)][object[]]$DeploymentObjects,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$ReplicaSetObjects,
        [Parameter(Mandatory = $true)][int]$SignerRevision,
        [Parameter(Mandatory = $true)][int]$LoginKeysetRevision,
        [hashtable]$ExpectedConfigSecretByDeployment = @{}
    )
    $result = Test-PandoraDSTicketReplicaSetRevisionGate $DeploymentObjects $ReplicaSetObjects `
        $SignerRevision $LoginKeysetRevision $ExpectedConfigSecretByDeployment
    if (-not $result.Ok) { throw "DSTicket ReplicaSet revision 门禁失败:$($result.Failures -join '; ')" }
    return $result
}

function Assert-PandoraDSTicketSignerDeploymentGate {
    param(
        [Parameter(Mandatory = $true)][object[]]$DeploymentObjects,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$ReplicaSetObjects,
        [Parameter(Mandatory = $true)][object[]]$PodObjects,
        [Parameter(Mandatory = $true)][int]$SignerRevision,
        [Parameter(Mandatory = $true)][int]$LoginKeysetRevision,
        [hashtable]$ExpectedConfigSecretByDeployment = @{}
    )
    $failures = [System.Collections.Generic.List[string]]::new()
    $secretName = "pandora-dsticket-signer-r$SignerRevision"
    $loginConfigMap = "pandora-dsticket-jwks-r$LoginKeysetRevision"
    $rsGate = Test-PandoraDSTicketReplicaSetRevisionGate -DeploymentObjects $DeploymentObjects `
        -ReplicaSetObjects $ReplicaSetObjects -SignerRevision $SignerRevision `
        -LoginKeysetRevision $LoginKeysetRevision -ExpectedConfigSecretByDeployment $ExpectedConfigSecretByDeployment
    foreach ($failure in @($rsGate.Failures)) { $failures.Add($failure) }
    $deploymentUidByName = @{}
    foreach ($deployment in @($DeploymentObjects)) {
        $deploymentMeta = Get-PandoraRotationProperty $deployment 'metadata'
        $deploymentUidByName[[string](Get-PandoraRotationProperty $deploymentMeta 'name')] = `
            [string](Get-PandoraRotationProperty $deploymentMeta 'uid')
    }
    $pandoraPods = @($PodObjects | Where-Object {
        [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $_ 'metadata') 'namespace') -ceq 'pandora'
    })
    $signerLikePods = @($pandoraPods | Where-Object {
        $pod = $_
        $app = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty `
            (Get-PandoraRotationProperty $pod 'metadata') 'labels') 'app')
        $hasDSTicketClue = Test-PandoraDSTicketDSPodSpecClue (Get-PandoraRotationProperty $pod 'spec')
        return $script:PandoraDSTicketSignerNames -ccontains $app -or $hasDSTicketClue
    })
    foreach ($pod in $signerLikePods) {
        $podMeta = Get-PandoraRotationProperty $pod 'metadata'
        $podName = [string](Get-PandoraRotationProperty $podMeta 'name')
        $app = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $podMeta 'labels') 'app')
        if ($script:PandoraDSTicketSignerNames -cnotcontains $app) {
            $failures.Add("Pod/$podName 挂载 DSTicket signer Secret 但 app='$app' 不是受管 signer（孤儿/错标 Pod）")
            continue
        }
        $expectedConfig = if ($ExpectedConfigSecretByDeployment.ContainsKey($app)) {
            [string]$ExpectedConfigSecretByDeployment[$app]
        } else { 'pandora-config' }
        $expectedJwks = if ($app -ceq 'login') { $loginConfigMap } else { '' }
        foreach ($failure in @(Test-PandoraDSTicketSignerPodSpecReferenceContract `
            -Spec (Get-PandoraRotationProperty $pod 'spec') -ServiceName $app `
            -ExpectedConfigSecret $expectedConfig -ExpectedSignerSecret $secretName `
            -ExpectedLoginJwks $expectedJwks -ObjectName "Pod/$podName")) {
            $failures.Add($failure)
        }
        $privateReferences = @(Get-PandoraDSTicketSignerSecretReferences (Get-PandoraRotationProperty $pod 'spec'))
        if ($privateReferences.Count -ne 1 -or $privateReferences[0].Kind -cne 'DirectVolume' -or
            $privateReferences[0].Location -cne 'volume/dsticket' -or $privateReferences[0].Name -cne $secretName) {
            $failures.Add("Pod/$podName signer 私钥必须且只能恰为一个 DirectVolume:volume/dsticket:$secretName；检测到 $($privateReferences.Count) 个引用")
        }
        if (-not [string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty $podMeta 'deletionTimestamp'))) {
            $failures.Add("Pod/$podName 正在终止；必须等对象真正消失以关闭旧 signer 窗口")
        }
        $podOwners = @(@(Get-PandoraRotationProperty $podMeta 'ownerReferences' @()) | Where-Object {
            (Get-PandoraRotationProperty $_ 'controller' $false) -eq $true
        })
        if ($podOwners.Count -ne 1 -or [string](Get-PandoraRotationProperty $podOwners[0] 'kind') -cne 'ReplicaSet') {
            $failures.Add("Pod/$podName 必须恰有一个且类型为 ReplicaSet 的 controller owner"); continue
        }
        $rsName = [string](Get-PandoraRotationProperty $podOwners[0] 'name')
        $rsUid = [string](Get-PandoraRotationProperty $podOwners[0] 'uid')
        $rsMatches = @($ReplicaSetObjects | Where-Object {
            [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $_ 'metadata') 'name') -ceq $rsName
        })
        if ([string]::IsNullOrWhiteSpace($rsName) -or [string]::IsNullOrWhiteSpace($rsUid) -or
            $rsMatches.Count -ne 1 -or [string](Get-PandoraRotationProperty `
            (Get-PandoraRotationProperty $rsMatches[0] 'metadata') 'uid') -cne $rsUid) {
            $failures.Add("Pod/$podName owner ReplicaSet/$rsName UID 漂移/孤儿"); continue
        }
        $rsOwners = @(@(Get-PandoraRotationProperty (Get-PandoraRotationProperty $rsMatches[0] 'metadata') 'ownerReferences' @()) |
            Where-Object { (Get-PandoraRotationProperty $_ 'controller' $false) -eq $true })
        if ($rsOwners.Count -ne 1 -or [string](Get-PandoraRotationProperty $rsOwners[0] 'kind') -cne 'Deployment' -or
            [string](Get-PandoraRotationProperty $rsOwners[0] 'name') -cne $app -or
            -not $deploymentUidByName.ContainsKey($app) -or
            [string](Get-PandoraRotationProperty $rsOwners[0] 'uid') -cne [string]$deploymentUidByName[$app]) {
            $failures.Add("Pod/$podName 的 ReplicaSet→Deployment owner UID/app 链不一致")
            continue
        }
        $rsSpec = Get-PandoraRotationProperty $rsMatches[0] 'spec'
        $rsStatus = Get-PandoraRotationProperty $rsMatches[0] 'status'
        $rsCapacity = (Get-PandoraNonNegativeControllerCount $rsSpec 'replicas' "ReplicaSet/$rsName spec") +
            (Get-PandoraNonNegativeControllerCount $rsStatus 'replicas' "ReplicaSet/$rsName status")
        if ($rsCapacity -lt 1) {
            $failures.Add("Pod/$podName 存在但 owner ReplicaSet/$rsName desired/status 均为 0")
        }
    }
    foreach ($name in $script:PandoraDSTicketSignerNames) {
        $expectedConfigSecret = if ($ExpectedConfigSecretByDeployment.ContainsKey($name)) {
            [string]$ExpectedConfigSecretByDeployment[$name]
        } else { 'pandora-config' }
        $matches = @($DeploymentObjects | Where-Object { [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $_ 'metadata') 'name') -ceq $name })
        if ($matches.Count -ne 1) { $failures.Add("Deployment/$name 数量=$($matches.Count)"); continue }
        $deployment = $matches[0]
        $metadata = Get-PandoraRotationProperty $deployment 'metadata'
        if ([string](Get-PandoraRotationProperty $metadata 'namespace') -cne 'pandora') { $failures.Add("Deployment/$name namespace 不是 pandora") }
        if (-not [string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty $metadata 'deletionTimestamp'))) {
            $failures.Add("Deployment/$name 正在删除")
        }
        try { Assert-PandoraDeploymentSafeRollingStrategy -Deployment $deployment }
        catch { $failures.Add($_.Exception.Message) }
        if (-not (Test-PandoraDeploymentVolumeReference -Deployment $deployment -VolumeName dsticket `
            -ReferenceKind secret -ExpectedName $secretName)) { $failures.Add("Deployment/$name 未引用 Secret/$secretName") }
        if (-not (Test-PandoraDeploymentVolumeReference -Deployment $deployment -VolumeName conf `
            -ReferenceKind secret -ExpectedName $expectedConfigSecret)) { $failures.Add("Deployment/$name 未引用配置 Secret/$expectedConfigSecret") }
        if ($name -ceq 'login') {
            if (-not (Test-PandoraDeploymentVolumeReference -Deployment $deployment -VolumeName dsticket-jwks `
                -ReferenceKind configMap -ExpectedName $loginConfigMap)) { $failures.Add("Deployment/login 未引用 ConfigMap/$loginConfigMap") }
        }
        $spec = Get-PandoraRotationProperty $deployment 'spec'
        $status = Get-PandoraRotationProperty $deployment 'status'
        if ((Get-PandoraRotationProperty $spec 'paused' $false) -eq $true) {
            $failures.Add("Deployment/$name 仍处于 paused，不能记为 rollout 全绿")
        }
        $replicas = [int](Get-PandoraRotationProperty $spec 'replicas' 1)
        $generation = [int64](Get-PandoraRotationProperty $metadata 'generation' 0)
        $observed = [int64](Get-PandoraRotationProperty $status 'observedGeneration' 0)
        $updated = [int](Get-PandoraRotationProperty $status 'updatedReplicas' 0)
        $ready = [int](Get-PandoraRotationProperty $status 'readyReplicas' 0)
        $available = [int](Get-PandoraRotationProperty $status 'availableReplicas' 0)
        $unavailable = [int](Get-PandoraRotationProperty $status 'unavailableReplicas' 0)
        if ($replicas -lt 1 -or $observed -ne $generation -or $updated -ne $replicas -or
            $ready -ne $replicas -or $available -ne $replicas -or $unavailable -ne 0) {
            $failures.Add("Deployment/$name rollout 未全绿(replicas=$replicas observed=$observed/$generation updated=$updated ready=$ready available=$available unavailable=$unavailable)")
        }
        $allSignerPods = @($signerLikePods | Where-Object {
            $podMeta = Get-PandoraRotationProperty $_ 'metadata'
            [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $podMeta 'labels') 'app') -ceq $name
        })
        # marker 的时钟必须晚于最后一个 K1 Pod 真正消失；仅有 deletionTimestamp 还可能在
        # termination grace 内完成一笔 K1 签发，所以 terminating 旧 Pod 也要检查卷引用。
        foreach ($pod in $allSignerPods) {
            $podName = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $pod 'metadata') 'name')
            $podAsDeployment = [pscustomobject]@{ spec = [pscustomobject]@{ template = [pscustomobject]@{ spec = (Get-PandoraRotationProperty $pod 'spec') } } }
            if (-not (Test-PandoraDeploymentVolumeReference -Deployment $podAsDeployment -VolumeName dsticket `
                -ReferenceKind secret -ExpectedName $secretName)) { $failures.Add("Pod/$podName 仍引用非目标 signer Secret（含 terminating Pod）") }
            if (-not (Test-PandoraDeploymentVolumeReference -Deployment $podAsDeployment -VolumeName conf `
                -ReferenceKind secret -ExpectedName $expectedConfigSecret)) { $failures.Add("Pod/$podName 仍引用非目标配置 Secret/$expectedConfigSecret（含 terminating Pod）") }
            if ($name -ceq 'login' -and -not (Test-PandoraDeploymentVolumeReference -Deployment $podAsDeployment `
                -VolumeName dsticket-jwks -ReferenceKind configMap -ExpectedName $loginConfigMap)) {
                $failures.Add("Pod/$podName 仍引用非目标 Login JWKS（含 terminating Pod）")
            }
        }
        $pods = @($allSignerPods | Where-Object {
            [string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $_ 'metadata') 'deletionTimestamp'))
        })
        if ($pods.Count -ne $replicas) { $failures.Add("Deployment/$name 非终止 Pod 数量=$($pods.Count) expected=$replicas") }
        foreach ($pod in $pods) {
            $podName = [string](Get-PandoraRotationProperty (Get-PandoraRotationProperty $pod 'metadata') 'name')
            $podStatus = Get-PandoraRotationProperty $pod 'status'
            $readyCondition = @(@(Get-PandoraRotationProperty $podStatus 'conditions' @()) | Where-Object {
                [string](Get-PandoraRotationProperty $_ 'type') -ceq 'Ready' -and [string](Get-PandoraRotationProperty $_ 'status') -ceq 'True'
            })
            if ([string](Get-PandoraRotationProperty $podStatus 'phase') -cne 'Running' -or $readyCondition.Count -ne 1) {
                $failures.Add("Pod/$podName 未 Running/Ready")
            }
        }
    }
    if ($failures.Count -gt 0) { throw "DSTicket signer rollout 门禁失败:$($failures -join '; ')" }
}

function Assert-PandoraDSTicketRuntimeObjectGate {
    param(
        [Parameter(Mandatory = $true)][object[]]$DeploymentObjects,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$ReplicaSetObjects,
        [Parameter(Mandatory = $true)][object[]]$SignerPodObjects,
        [Parameter(Mandatory = $true)][object[]]$FleetObjects,
        [Parameter(Mandatory = $true)][object[]]$GameServerObjects,
        [Parameter(Mandatory = $true)][object[]]$GameServerSetObjects,
        [Parameter(Mandatory = $true)][object[]]$DSPodObjects,
        [Parameter(Mandatory = $true)][int]$SignerRevision,
        [Parameter(Mandatory = $true)][int]$LoginKeysetRevision,
        [Parameter(Mandatory = $true)][hashtable]$ExpectedConfigSecretByDeployment,
        [Parameter(Mandatory = $true)][int]$DSRevision
    )
    Assert-PandoraDSTicketSignerDeploymentGate -DeploymentObjects $DeploymentObjects `
        -ReplicaSetObjects $ReplicaSetObjects -PodObjects $SignerPodObjects `
        -SignerRevision $SignerRevision -LoginKeysetRevision $LoginKeysetRevision `
        -ExpectedConfigSecretByDeployment $ExpectedConfigSecretByDeployment
    $null = Assert-PandoraDSTicketLiveDSRevisionGate -FleetObjects $FleetObjects `
        -GameServerObjects $GameServerObjects -GameServerSetObjects $GameServerSetObjects `
        -PodObjects $DSPodObjects -TargetRevision $DSRevision
}

function Get-PandoraDSTicketMarkerCreationTime {
    param([Parameter(Mandatory = $true)]$MarkerObject, [Parameter(Mandatory = $true)][string]$Where)
    $raw = Get-PandoraRotationProperty (Get-PandoraRotationProperty $MarkerObject 'metadata') 'creationTimestamp'
    if ($null -eq $raw -or [string]::IsNullOrWhiteSpace([string]$raw)) {
        throw "$Where 缺 apiserver metadata.creationTimestamp。"
    }
    if ($raw -is [DateTime]) { return [DateTimeOffset]::new(([DateTime]$raw).ToUniversalTime()) }
    if ($raw -is [DateTimeOffset]) { return ([DateTimeOffset]$raw).ToUniversalTime() }
    try {
        return [DateTimeOffset]::Parse([string]$raw, [Globalization.CultureInfo]::InvariantCulture,
            [Globalization.DateTimeStyles]::AssumeUniversal).ToUniversalTime()
    } catch { throw "$Where creationTimestamp 非法:$raw" }
}

function Assert-PandoraDSTicketAuditMarkerMetadata {
    param([Parameter(Mandatory = $true)]$MarkerObject, [Parameter(Mandatory = $true)][string]$ExpectedName)
    $metadata = Get-PandoraRotationProperty $MarkerObject 'metadata'
    if ([string](Get-PandoraRotationProperty $MarkerObject 'kind') -cne 'ConfigMap' -or
        [string](Get-PandoraRotationProperty $metadata 'name') -cne $ExpectedName -or
        [string](Get-PandoraRotationProperty $metadata 'namespace') -cne 'pandora' -or
        (Get-PandoraRotationProperty $MarkerObject 'immutable' $false) -ne $true -or
        -not [string]::IsNullOrWhiteSpace([string](Get-PandoraRotationProperty $metadata 'deletionTimestamp'))) {
        throw "DSTicket 审计 marker 必须是未删除的 immutable pandora/ConfigMap/$ExpectedName。"
    }
    $labels = Get-PandoraRotationProperty $metadata 'labels'
    if ([string](Get-PandoraRotationProperty $labels 'app.kubernetes.io/part-of') -cne 'pandora' -or
        [string](Get-PandoraRotationProperty $labels 'app.kubernetes.io/component') -cne 'dsticket-rotation-audit') {
        throw "ConfigMap/$ExpectedName 审计 labels 漂移。"
    }
    return Get-PandoraDSTicketMarkerCreationTime -MarkerObject $MarkerObject -Where "ConfigMap/$ExpectedName"
}

function Get-PandoraDSTicketActivationMarkerName {
    param([Parameter(Mandatory = $true)][int]$PromoteSignerRevision)
    if ($PromoteSignerRevision -lt 1) { throw 'PromoteSignerRevision 必须 >= 1。' }
    return "pandora-dsticket-activation-signer-r$PromoteSignerRevision"
}

function New-PandoraDSTicketActivationMarkerObject {
    param(
        [Parameter(Mandatory = $true)][int]$StageRevision,
        [Parameter(Mandatory = $true)][int]$PromoteRevision,
        [Parameter(Mandatory = $true)][int]$RetireRevision,
        [Parameter(Mandatory = $true)][int]$OldSignerRevision,
        [Parameter(Mandatory = $true)][string]$OldKid,
        [Parameter(Mandatory = $true)][string]$NewKid
    )
    if ($OldKid -cnotmatch '^[A-Za-z0-9_-]{43}$' -or $NewKid -cnotmatch '^[A-Za-z0-9_-]{43}$' -or $OldKid -ceq $NewKid) {
        throw '激活 marker 的 K1/K2 kid 非法或相同。'
    }
    return [ordered]@{
        apiVersion = 'v1'
        kind = 'ConfigMap'
        immutable = $true
        metadata = [ordered]@{
            name = Get-PandoraDSTicketActivationMarkerName -PromoteSignerRevision $PromoteRevision
            namespace = 'pandora'
            labels = [ordered]@{
                'app.kubernetes.io/part-of' = 'pandora'
                'app.kubernetes.io/component' = 'dsticket-rotation-audit'
            }
            annotations = [ordered]@{
                'pandora.dev/dsticket-stage-revision' = [string]$StageRevision
                'pandora.dev/dsticket-promote-revision' = [string]$PromoteRevision
                'pandora.dev/dsticket-old-signer-revision' = [string]$OldSignerRevision
                'pandora.dev/dsticket-activated-signer-revision' = [string]$PromoteRevision
                'pandora.dev/dsticket-terminal-signer-revision' = [string]$RetireRevision
                'pandora.dev/dsticket-old-kid' = $OldKid
                'pandora.dev/dsticket-active-kid' = $NewKid
                'pandora.dev/dsticket-max-ttl-seconds' = [string]$script:PandoraDSTicketMaxTTLSeconds
                'pandora.dev/dsticket-leeway-seconds' = [string]$script:PandoraDSTicketLeewaySeconds
                'pandora.dev/dsticket-retire-buffer-seconds' = [string]$script:PandoraDSTicketRetireBufferSeconds
            }
        }
        data = [ordered]@{
            summary = "K2 signer alias r$PromoteRevision activated after all four signer Deployments rolled out; terminal alias is r$RetireRevision; retirement is measured only from this object's apiserver creationTimestamp."
        }
    }
}

function Assert-PandoraDSTicketActivationMarkerContract {
    param(
        [Parameter(Mandatory = $true)]$MarkerObject,
        [Parameter(Mandatory = $true)][int]$StageRevision,
        [Parameter(Mandatory = $true)][int]$PromoteRevision,
        [Parameter(Mandatory = $true)][int]$RetireRevision,
        [Parameter(Mandatory = $true)][int]$OldSignerRevision,
        [Parameter(Mandatory = $true)][string]$OldKid,
        [Parameter(Mandatory = $true)][string]$NewKid,
        [DateTimeOffset]$ServerNow = [DateTimeOffset]::MinValue,
        [switch]$RequireRetireWindow
    )
    $name = Get-PandoraDSTicketActivationMarkerName -PromoteSignerRevision $PromoteRevision
    $createdAt = Assert-PandoraDSTicketAuditMarkerMetadata -MarkerObject $MarkerObject -ExpectedName $name
    $annotations = $MarkerObject.metadata.annotations
    $expected = [ordered]@{
        'pandora.dev/dsticket-stage-revision' = [string]$StageRevision
        'pandora.dev/dsticket-promote-revision' = [string]$PromoteRevision
        'pandora.dev/dsticket-old-signer-revision' = [string]$OldSignerRevision
        'pandora.dev/dsticket-activated-signer-revision' = [string]$PromoteRevision
        'pandora.dev/dsticket-terminal-signer-revision' = [string]$RetireRevision
        'pandora.dev/dsticket-old-kid' = $OldKid
        'pandora.dev/dsticket-active-kid' = $NewKid
        'pandora.dev/dsticket-max-ttl-seconds' = [string]$script:PandoraDSTicketMaxTTLSeconds
        'pandora.dev/dsticket-leeway-seconds' = [string]$script:PandoraDSTicketLeewaySeconds
        'pandora.dev/dsticket-retire-buffer-seconds' = [string]$script:PandoraDSTicketRetireBufferSeconds
    }
    foreach ($entry in $expected.GetEnumerator()) {
        if ([string](Get-PandoraRotationProperty $annotations $entry.Key) -cne [string]$entry.Value) {
            throw "ConfigMap/$name annotation $($entry.Key) 漂移。"
        }
    }
    foreach ($deprecatedTime in @(
        'pandora.dev/dsticket-activated-at-unix', 'pandora.dev/dsticket-activated-at-rfc3339',
        'pandora.dev/dsticket-retire-not-before-unix'
    )) {
        if ($null -ne (Get-PandoraRotationProperty $annotations $deprecatedTime)) {
            throw "ConfigMap/$name 禁止携带客户端/预采样时间 annotation $deprecatedTime；只信任 metadata.creationTimestamp。"
        }
    }
    $creationUnix = $createdAt.ToUnixTimeSeconds()
    $authoritativeNotBefore = $creationUnix + $script:PandoraDSTicketRetireWaitSeconds
    if ($RequireRetireWindow -and $ServerNow -eq [DateTimeOffset]::MinValue) {
        throw '退役门禁必须显式传 apiserver ServerNow；禁止回落本机 UtcNow。'
    }
    if ($RequireRetireWindow -and $ServerNow.ToUnixTimeSeconds() -lt $authoritativeNotBefore) {
        $remaining = $authoritativeNotBefore - $ServerNow.ToUnixTimeSeconds()
        throw "DSTicket K1 退役窗口尚未满足:还需等待 ${remaining}s（180s TTL + 15s leeway + 30s buffer = 225s）。"
    }
    return [pscustomobject]@{
        ActivatedAtUnix = $creationUnix
        RetireNotBeforeUnix = $authoritativeNotBefore
        WaitSeconds = $script:PandoraDSTicketRetireWaitSeconds
    }
}

function Get-PandoraDSTicketTerminalMarkerName {
    param([Parameter(Mandatory = $true)][int]$RetireRevision)
    if ($RetireRevision -lt 1) { throw 'RetireRevision 必须 >= 1。' }
    return "pandora-dsticket-retired-r$RetireRevision"
}

function New-PandoraDSTicketTerminalMarkerObject {
    param(
        [Parameter(Mandatory = $true)][int]$PromoteRevision,
        [Parameter(Mandatory = $true)][int]$RetireRevision,
        [Parameter(Mandatory = $true)][string]$ActiveKid,
        [Parameter(Mandatory = $true)][string]$FixedConfigContractSha256
    )
    if ($ActiveKid -cnotmatch '^[A-Za-z0-9_-]{43}$' -or $FixedConfigContractSha256 -cnotmatch '^[0-9a-f]{64}$') {
        throw 'terminal marker active kid/config hash 非法。'
    }
    return [ordered]@{
        apiVersion = 'v1'; kind = 'ConfigMap'; immutable = $true
        metadata = [ordered]@{
            name = Get-PandoraDSTicketTerminalMarkerName $RetireRevision
            namespace = 'pandora'
            labels = [ordered]@{
                'app.kubernetes.io/part-of' = 'pandora'
                'app.kubernetes.io/component' = 'dsticket-rotation-audit'
            }
            annotations = [ordered]@{
                'pandora.dev/dsticket-activation-marker' = (Get-PandoraDSTicketActivationMarkerName $PromoteRevision)
                'pandora.dev/dsticket-terminal-signer-revision' = [string]$RetireRevision
                'pandora.dev/dsticket-active-kid' = $ActiveKid
                'pandora.dev/dsticket-fixed-config-contract-sha256' = $FixedConfigContractSha256
            }
        }
        data = [ordered]@{ summary = "DSTicket rotation reached terminal fixed-config/signer/JWKS/Fleet revision r$RetireRevision." }
    }
}

function Assert-PandoraDSTicketTerminalMarkerContract {
    param(
        [Parameter(Mandatory = $true)]$MarkerObject,
        [Parameter(Mandatory = $true)][int]$PromoteRevision,
        [Parameter(Mandatory = $true)][int]$RetireRevision,
        [Parameter(Mandatory = $true)][string]$ActiveKid,
        [Parameter(Mandatory = $true)][string]$FixedConfigContractSha256,
        [Parameter(Mandatory = $true)]$ActivationMarkerObject
    )
    $name = Get-PandoraDSTicketTerminalMarkerName $RetireRevision
    $createdAt = Assert-PandoraDSTicketAuditMarkerMetadata -MarkerObject $MarkerObject -ExpectedName $name
    $activationName = Get-PandoraDSTicketActivationMarkerName $PromoteRevision
    $activationCreatedAt = Assert-PandoraDSTicketAuditMarkerMetadata -MarkerObject $ActivationMarkerObject -ExpectedName $activationName
    $annotations = $MarkerObject.metadata.annotations
    $expected = [ordered]@{
        'pandora.dev/dsticket-activation-marker' = $activationName
        'pandora.dev/dsticket-terminal-signer-revision' = [string]$RetireRevision
        'pandora.dev/dsticket-active-kid' = $ActiveKid
        'pandora.dev/dsticket-fixed-config-contract-sha256' = $FixedConfigContractSha256
    }
    foreach ($entry in $expected.GetEnumerator()) {
        if ([string](Get-PandoraRotationProperty $annotations $entry.Key) -cne [string]$entry.Value) {
            throw "ConfigMap/$name annotation $($entry.Key) 漂移。"
        }
    }
    foreach ($deprecatedTime in @('pandora.dev/dsticket-completed-at-unix', 'pandora.dev/dsticket-completed-at-rfc3339')) {
        if ($null -ne (Get-PandoraRotationProperty $annotations $deprecatedTime)) {
            throw "ConfigMap/$name 禁止携带客户端/预采样时间 annotation $deprecatedTime；只信任 metadata.creationTimestamp。"
        }
    }
    $activationUnix = $activationCreatedAt.ToUnixTimeSeconds()
    $createdUnix = $createdAt.ToUnixTimeSeconds()
    if ($createdUnix -lt $activationUnix + $script:PandoraDSTicketRetireWaitSeconds) {
        throw "ConfigMap/$name 创建过早：terminal creationTimestamp 必须不早于 activation + $script:PandoraDSTicketRetireWaitSeconds 秒。"
    }
    return [pscustomobject]@{
        RetireRevision = $RetireRevision
        ActivationCreatedAtUnix = $activationUnix
        CreatedAtUnix = $createdUnix
    }
}

function Get-PandoraPositiveAnnotationInt {
    param($Annotations, [string]$Name, [string]$Where)
    $text = [string](Get-PandoraRotationProperty $Annotations $Name)
    [int]$value = 0
    if (-not [int]::TryParse($text, [ref]$value) -or $value -lt 1) { throw "$Where annotation $Name 非法:$text。" }
    return $value
}

function Assert-PandoraDSTicketOrdinaryMarkerState {
    param(
        [Parameter(Mandatory = $true)][int]$RequestedRevision,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$ActivationMarkers,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$TerminalMarkers,
        [Parameter(Mandatory = $true)]$FixedConfigSecret
    )
    $fixed = Get-PandoraDSTicketConfigSubcontract $FixedConfigSecret
    if ($fixed.LoginKeysetRevision -ne $RequestedRevision) {
        throw "fixed pandora-config DSTicket revision=$($fixed.LoginKeysetRevision)，requested=$RequestedRevision。"
    }
    if ($ActivationMarkers.Count -eq 0) {
        if ($TerminalMarkers.Count -ne 0) { throw '存在 orphan terminal marker 但无 activation marker。' }
        return [pscustomobject]@{
            State = 'pre-rotation-stable'; Revision = $RequestedRevision; ActiveKid = $fixed.ActiveKid
            TerminalCreatedAtUnix = [int64]0
        }
    }
    $activationByName = @{}
    $activationPromoteRevisions = @{}
    $activationRetireRevisions = @{}
    foreach ($marker in $ActivationMarkers) {
        $name = [string]$marker.metadata.name
        if ($activationByName.ContainsKey($name)) { throw "activation marker 重复:$name。" }
        $a = $marker.metadata.annotations
        $stage = Get-PandoraPositiveAnnotationInt $a 'pandora.dev/dsticket-stage-revision' $name
        $promote = Get-PandoraPositiveAnnotationInt $a 'pandora.dev/dsticket-promote-revision' $name
        $retire = Get-PandoraPositiveAnnotationInt $a 'pandora.dev/dsticket-terminal-signer-revision' $name
        $oldSigner = Get-PandoraPositiveAnnotationInt $a 'pandora.dev/dsticket-old-signer-revision' $name
        $oldKid = [string](Get-PandoraRotationProperty $a 'pandora.dev/dsticket-old-kid')
        $newKid = [string](Get-PandoraRotationProperty $a 'pandora.dev/dsticket-active-kid')
        Assert-PandoraDSTicketRotationRevisionPlan -StageRevision $stage -PromoteRevision $promote `
            -RetireRevision $retire -OldSignerRevision $oldSigner
        if ($activationPromoteRevisions.ContainsKey($promote)) { throw "多个 activation 宣称同一 promote revision r$promote。" }
        if ($activationRetireRevisions.ContainsKey($retire)) { throw "多个 activation 宣称同一 terminal revision r$retire。" }
        $activationPromoteRevisions[$promote] = $true
        $activationRetireRevisions[$retire] = $true
        $activationContract = Assert-PandoraDSTicketActivationMarkerContract -MarkerObject $marker -StageRevision $stage `
            -PromoteRevision $promote -RetireRevision $retire -OldSignerRevision $oldSigner `
            -OldKid $oldKid -NewKid $newKid
        $activationByName[$name] = [pscustomobject]@{
            Marker = $marker; Stage = $stage; Promote = $promote; Retire = $retire
            OldSigner = $oldSigner; OldKid = $oldKid; ActiveKid = $newKid
            ActivatedAtUnix = $activationContract.ActivatedAtUnix
            TerminalCreatedAtUnix = [int64]0
        }
    }
    $paired = @{}
    $terminalNames = @{}
    $latest = $null
    foreach ($terminal in $TerminalMarkers) {
        $terminalName = [string]$terminal.metadata.name
        if ($terminalNames.ContainsKey($terminalName)) { throw "terminal marker metadata.name 重复:$terminalName。" }
        $terminalNames[$terminalName] = $true
        $annotations = $terminal.metadata.annotations
        $activationName = [string](Get-PandoraRotationProperty $annotations 'pandora.dev/dsticket-activation-marker')
        if (-not $activationByName.ContainsKey($activationName)) { throw "orphan terminal marker/$($terminal.metadata.name) 引用不存在的 activation:$activationName。" }
        if ($paired.ContainsKey($activationName)) { throw "activation marker/$activationName 被多个 terminal marker 配对。" }
        $activation = $activationByName[$activationName]
        $retire = Get-PandoraPositiveAnnotationInt $annotations 'pandora.dev/dsticket-terminal-signer-revision' ([string]$terminal.metadata.name)
        if ($retire -ne $activation.Retire) { throw "terminal/$($terminal.metadata.name) revision 与 activation 计划不一致。" }
        $contractHash = [string](Get-PandoraRotationProperty $annotations 'pandora.dev/dsticket-fixed-config-contract-sha256')
        $terminalContract = Assert-PandoraDSTicketTerminalMarkerContract -MarkerObject $terminal `
            -PromoteRevision $activation.Promote -RetireRevision $retire -ActiveKid $activation.ActiveKid `
            -FixedConfigContractSha256 $contractHash -ActivationMarkerObject $activation.Marker
        $activation.TerminalCreatedAtUnix = $terminalContract.CreatedAtUnix
        $paired[$activationName] = $true
        if ($null -eq $latest -or $retire -gt $latest.Retire) {
            $latest = [pscustomobject]@{
                Retire = $retire; ActiveKid = $activation.ActiveKid; ContractHash = $contractHash
                TerminalCreatedAtUnix = $terminalContract.CreatedAtUnix
            }
        }
    }
    $unpaired = @($activationByName.Keys | Where-Object { -not $paired.ContainsKey($_) })
    if ($unpaired.Count -gt 0) { throw "存在未 terminalize 的 DSTicket activation marker:$($unpaired -join ',')。普通发布必须阻断。" }
    $chain = @($activationByName.Values | Sort-Object Retire)
    for ($i = 1; $i -lt $chain.Count; $i++) {
        $previous = $chain[$i - 1]
        $current = $chain[$i]
        if ($current.OldSigner -ne $previous.Retire -or $current.OldKid -cne $previous.ActiveKid -or
            $current.Stage -le $previous.Retire) {
            throw "DSTicket marker 历史链断裂:上一轮 terminal=r$($previous.Retire)/kid=$($previous.ActiveKid)，" +
                   "下一轮 old=r$($current.OldSigner)/kid=$($current.OldKid)/stage=r$($current.Stage)。"
        }
        if ($previous.TerminalCreatedAtUnix -lt 1 -or $current.ActivatedAtUnix -lt $previous.TerminalCreatedAtUnix) {
            throw "DSTicket marker 时间链倒序:下一轮 activation=$($current.ActivatedAtUnix) 早于上一轮 terminal=$($previous.TerminalCreatedAtUnix)。"
        }
    }
    if ($null -eq $latest -or $latest.Retire -ne $RequestedRevision) {
        $latestText = if ($null -eq $latest) { '<none>' } else { [string]$latest.Retire }
        throw "最新 terminal revision=$latestText，requested=$RequestedRevision。"
    }
    if ($latest.ActiveKid -cne $fixed.ActiveKid -or $latest.ContractHash -cne $fixed.Sha256) {
        throw '最新 terminal marker 与 fixed pandora-config DSTicket 子契约 hash/active kid 不一致。'
    }
    return [pscustomobject]@{
        State = 'terminal-stable'; Revision = $latest.Retire; ActiveKid = $latest.ActiveKid
        ContractHash = $latest.ContractHash; TerminalCreatedAtUnix = $latest.TerminalCreatedAtUnix
    }
}

function Assert-PandoraDSTicketFixedTerminalAnnotations {
    param(
        [Parameter(Mandatory = $true)]$FixedConfigSecret,
        [Parameter(Mandatory = $true)]$ConfigContract,
        [switch]$Require
    )
    $annotations = Get-PandoraRotationProperty (Get-PandoraRotationProperty $FixedConfigSecret 'metadata') 'annotations'
    $revision = [string](Get-PandoraRotationProperty $annotations 'pandora.dev/dsticket-terminal-revision')
    $kid = [string](Get-PandoraRotationProperty $annotations 'pandora.dev/dsticket-terminal-active-kid')
    $hash = [string](Get-PandoraRotationProperty $annotations 'pandora.dev/dsticket-terminal-config-contract-sha256')
    $present = @(@($revision, $kid, $hash) | Where-Object { -not [string]::IsNullOrWhiteSpace([string]$_) }).Count
    if ($Require -and $present -ne 3) { throw 'fixed pandora-config terminal annotations 缺失/半套。' }
    if ($present -gt 0 -and ($present -ne 3 -or $revision -cne [string]$ConfigContract.LoginKeysetRevision -or
        $kid -cne $ConfigContract.ActiveKid -or $hash -cne $ConfigContract.Sha256)) {
        throw 'fixed pandora-config terminal revision/kid/hash annotations 与 DSTicket 子契约不一致。'
    }
}

function Assert-PandoraDSTicketRotationTransitionHistoryState {
    param(
        [Parameter(Mandatory = $true)][int]$StageRevision,
        [Parameter(Mandatory = $true)][int]$PromoteRevision,
        [Parameter(Mandatory = $true)][int]$RetireRevision,
        [Parameter(Mandatory = $true)][int]$OldSignerRevision,
        [Parameter(Mandatory = $true)][string]$OldKid,
        [Parameter(Mandatory = $true)][string]$NewKid,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$ActivationMarkers,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$TerminalMarkers,
        [Parameter(Mandatory = $true)]$FixedConfigSecret,
        [switch]$AllowTerminalFixed
    )
    Assert-PandoraDSTicketRotationRevisionPlan $StageRevision $PromoteRevision $RetireRevision $OldSignerRevision
    $activationName = Get-PandoraDSTicketActivationMarkerName $PromoteRevision
    $terminalName = Get-PandoraDSTicketTerminalMarkerName $RetireRevision
    $currentActivations = @($ActivationMarkers | Where-Object { [string]$_.metadata.name -ceq $activationName })
    $currentTerminals = @($TerminalMarkers | Where-Object { [string]$_.metadata.name -ceq $terminalName })
    if ($currentActivations.Count -ne 1 -or $currentTerminals.Count -ne 0) {
        throw "当前轮换必须恰有 activation/$activationName 且尚无 terminal/$terminalName。"
    }
    $currentActivationContract = Assert-PandoraDSTicketActivationMarkerContract -MarkerObject $currentActivations[0] `
        -StageRevision $StageRevision -PromoteRevision $PromoteRevision -RetireRevision $RetireRevision `
        -OldSignerRevision $OldSignerRevision -OldKid $OldKid -NewKid $NewKid

    $previousActivations = @($ActivationMarkers | Where-Object { [string]$_.metadata.name -cne $activationName })
    $previousTerminals = @($TerminalMarkers | Where-Object { [string]$_.metadata.name -cne $terminalName })
    $actual = Get-PandoraDSTicketConfigSubcontract $FixedConfigSecret
    $isOld = $actual.LoginKeysetRevision -eq $OldSignerRevision -and $actual.ActiveKid -ceq $OldKid
    $isTerminal = $AllowTerminalFixed -and $actual.LoginKeysetRevision -eq $RetireRevision -and $actual.ActiveKid -ceq $NewKid
    if (-not ($isOld -or $isTerminal)) {
        throw "retire 过渡 fixed pandora-config 只能是 old r$OldSignerRevision/$OldKid 或 terminal r$RetireRevision/$NewKid；实际 r$($actual.LoginKeysetRevision)/$($actual.ActiveKid)。"
    }
    Assert-PandoraDSTicketFixedTerminalAnnotations -FixedConfigSecret $FixedConfigSecret -ConfigContract $actual `
        -Require:($isTerminal -or $previousTerminals.Count -gt 0)

    $historicalFixed = $FixedConfigSecret
    if ($isTerminal) {
        $oldData = Get-PandoraDSTicketConfigSecretUpdatedData -SecretObject $FixedConfigSecret -ActiveKid $OldKid `
            -LoginKeysetRevision $OldSignerRevision -AllowedCurrentActiveKids @($NewKid)
        $historicalFixed = [pscustomobject]@{
            kind = 'Secret'
            metadata = [pscustomobject]@{ name = 'pandora-config'; namespace = 'pandora' }
            data = [pscustomobject]$oldData
        }
    }
    $previous = Assert-PandoraDSTicketOrdinaryMarkerState -RequestedRevision $OldSignerRevision `
        -ActivationMarkers $previousActivations -TerminalMarkers $previousTerminals -FixedConfigSecret $historicalFixed
    if ($previous.ActiveKid -cne $OldKid) {
        throw "上一轮 marker/fixed active kid=$($previous.ActiveKid) 与当前 old kid=$OldKid 不一致。"
    }
    if ($previous.TerminalCreatedAtUnix -gt 0 -and
        $currentActivationContract.ActivatedAtUnix -lt $previous.TerminalCreatedAtUnix) {
        throw "DSTicket marker 时间链倒序:当前 activation=$($currentActivationContract.ActivatedAtUnix) 早于上一轮 terminal=$($previous.TerminalCreatedAtUnix)。"
    }
    return [pscustomobject]@{
        State = if ($isTerminal) { 'retire-terminal-fixed-transition' } else { 'retire-old-fixed-transition' }
        FixedRevision = $actual.LoginKeysetRevision
        ActiveKid = $actual.ActiveKid
        PreviousState = $previous.State
        CurrentActivation = $currentActivations[0]
    }
}

function Assert-PandoraDSTicketOrdinaryState {
    param(
        [Parameter(Mandatory = $true)][int]$RequestedRevision,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$DeploymentObjects,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$FleetObjects,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$SignerPodObjects,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$ReplicaSetObjects,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$GameServerObjects,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$GameServerSetObjects,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$DSPodObjects,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$ActivationMarkers,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$TerminalMarkers,
        [Parameter(Mandatory = $true)][AllowNull()]$FixedConfigSecret,
        [AllowEmptyCollection()][object[]]$LegacyControllerObjects = @()
    )
    $controllersEmpty = $DeploymentObjects.Count -eq 0 -and $FleetObjects.Count -eq 0
    if ($controllersEmpty) {
        $runtimeResidual = $SignerPodObjects.Count + $ReplicaSetObjects.Count + $GameServerObjects.Count +
            $GameServerSetObjects.Count + $DSPodObjects.Count + $LegacyControllerObjects.Count
        $allResidual = $runtimeResidual + $ActivationMarkers.Count + $TerminalMarkers.Count
        if ($null -eq $FixedConfigSecret) {
            if ($allResidual -ne 0) { throw "ordinary bootstrap controllers 双空但仍有 marker/Pod/GameServer/controller 残留(count=$allResidual)。" }
            return [pscustomobject]@{ State = 'bootstrap-empty'; Revision = $RequestedRevision; ActiveKid = '' }
        }
        # fixed pandora-config 存在就代表集群已经越过真正 bootstrap；必须先按正常 marker/fixed
        # 子契约核对 requested revision，不能借 controllers 双空绕过 revision 线性历史。
        if ($runtimeResidual -ne 0) {
            throw "ordinary config-only recovery 仍有 Pod/GameServer/ReplicaSet/GameServerSet/legacy controller 残留(count=$runtimeResidual)。"
        }
        $markerState = Assert-PandoraDSTicketOrdinaryMarkerState -RequestedRevision $RequestedRevision `
            -ActivationMarkers $ActivationMarkers -TerminalMarkers $TerminalMarkers -FixedConfigSecret $FixedConfigSecret
        return [pscustomobject]@{
            State = 'config-only-recovery'; Revision = $RequestedRevision; ActiveKid = $markerState.ActiveKid
            MarkerState = $markerState.State
        }
    }
    if ($DeploymentObjects.Count -eq 0 -or $FleetObjects.Count -eq 0) {
        throw 'ordinary state 只有 Deployment/Fleet 一侧为空，拒绝按 bootstrap 猜测。'
    }
    if ($LegacyControllerObjects.Count -ne 0) {
        throw "ordinary live state 检测到未分类 legacy controller 残留(count=$($LegacyControllerObjects.Count))。"
    }
    if ($null -eq $FixedConfigSecret) { throw 'ordinary live state 缺 fixed pandora-config，拒绝发布。' }
    $markerState = Assert-PandoraDSTicketOrdinaryMarkerState -RequestedRevision $RequestedRevision `
        -ActivationMarkers $ActivationMarkers -TerminalMarkers $TerminalMarkers -FixedConfigSecret $FixedConfigSecret
    $configMap = @{}
    foreach ($name in $script:PandoraDSTicketSignerNames) { $configMap[$name] = 'pandora-config' }
    Assert-PandoraDSTicketSignerDeploymentGate -DeploymentObjects $DeploymentObjects `
        -ReplicaSetObjects $ReplicaSetObjects -PodObjects $SignerPodObjects `
        -SignerRevision $RequestedRevision -LoginKeysetRevision $RequestedRevision `
        -ExpectedConfigSecretByDeployment $configMap
    $null = Assert-PandoraDSTicketLiveDSRevisionGate -FleetObjects $FleetObjects `
        -GameServerObjects $GameServerObjects -GameServerSetObjects $GameServerSetObjects `
        -PodObjects $DSPodObjects -TargetRevision $RequestedRevision
    return $markerState
}
