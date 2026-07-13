<#
.SYNOPSIS
  生成/校验 DSTicket v2 RSA 密钥对，并以 create-only 方式投递 immutable K8s Secret/ConfigMap。

.DESCRIPTION
  普通 Stable/Canary 发布不要运行本脚本。它只负责生成/校验并 create-only 投递某个轮换阶段的
  immutable signer Secret 与 JWKS ConfigMap；-Apply 会在同一次显式 context 确认后，以 create-only
  方式补齐 pandora namespace，消除首次部署的自举循环。阶段切换、全量 GameServer 门禁及 TTL
  等待由独立轮换脚本负责。active kid 与 signer kid 分开对账，不读取 keys[0]。

.EXAMPLE
  pwsh tools/scripts/dsticket_keyset.ps1 -KeyDir D:\secure\pandora-dsticket-r1 -Revision 1 -Generate
  pwsh tools/scripts/dsticket_keyset.ps1 -KeyDir D:\secure\pandora-dsticket-r1 -Revision 1 `
    -Apply -KubeContext pandora-prod

  # Phase A:生成 K2 私钥及 K1+K2 JWKS，但仍以 K1 为 active；K2 只预投递、不挂载。
  pwsh tools/scripts/dsticket_keyset.ps1 -KeyDir D:\secure\pandora-dsticket-r2 -Revision 2 `
    -SignerRevision 2 -Generate -MergeJwks D:\secure\pandora-dsticket-r1\jwks.json `
    -ActiveKid <K1-kid> -AllowInactiveSigner
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$KeyDir,
    [Parameter(Mandatory = $true)][ValidateRange(1, 2147483647)][int]$Revision,
    [ValidateRange(0, 2147483647)][int]$SignerRevision = 0,
    [string]$ActiveKid = '',
    [string]$MergeJwks = '',
    [string]$PrivateKeyInput = '',
    [switch]$AllowInactiveSigner,
    [switch]$Generate,
    [switch]$Apply,
    [string]$KubeContext = ''
)

$ErrorActionPreference = 'Stop'
$ProjectRoot = (Resolve-Path "$PSScriptRoot/../..").Path
. (Join-Path $PSScriptRoot 'lib/dsticket_keyset_contract.ps1')

$resolvedKeyDir = [System.IO.Path]::GetFullPath($KeyDir)
$privatePath = Join-Path $resolvedKeyDir 'private.pem'
$jwksPath = Join-Path $resolvedKeyDir 'jwks.json'
if ($SignerRevision -eq 0) { $SignerRevision = $Revision }

if ($Generate) {
    if ((Test-Path -LiteralPath $privatePath) -or (Test-Path -LiteralPath $jwksPath)) {
        throw "拒绝覆盖既有 DSTicket 密钥材料:$resolvedKeyDir。请使用新目录和新 revision。"
    }
    Push-Location $ProjectRoot
    try {
        $generateArgs = @('./tools/dsticketkeys', '-out', $resolvedKeyDir, '-revision', [string]$Revision)
        if (-not [string]::IsNullOrWhiteSpace($MergeJwks)) {
            $generateArgs += @('-merge', [System.IO.Path]::GetFullPath($MergeJwks))
        }
        if (-not [string]::IsNullOrWhiteSpace($PrivateKeyInput)) {
            $generateArgs += @('-private-in', [System.IO.Path]::GetFullPath($PrivateKeyInput))
        }
        if (-not [string]::IsNullOrWhiteSpace($ActiveKid)) {
            $generateArgs += @('-active-kid', $ActiveKid.Trim())
        }
        & go run @generateArgs
        if ($LASTEXITCODE -ne 0) { throw "dsticketkeys 生成失败(exit=$LASTEXITCODE)。" }
    } finally {
        Pop-Location
    }
}

if (-not (Test-Path -LiteralPath $privatePath -PathType Leaf) -or
    -not (Test-Path -LiteralPath $jwksPath -PathType Leaf)) {
    throw "密钥目录必须同时含 private.pem 与 jwks.json:$resolvedKeyDir"
}
$privatePem = Get-Content -LiteralPath $privatePath -Raw
$jwksText = Get-Content -LiteralPath $jwksPath -Raw
$contract = Get-PandoraDSTicketKeyMaterialContract -PrivateKeyPem $privatePem -JwksText $jwksText `
    -ExpectedRevision $Revision -ExpectedActiveKid $ActiveKid `
    -RequirePrivateKeyActive (-not [bool]$AllowInactiveSigner)
Write-Host "DSTicket 密钥对账通过:keyset_revision=$Revision active_kid=$($contract.ActiveKid) signer_revision=$SignerRevision signer_kid=$($contract.SignerKid) keys=$($contract.KeyCount) jwks_sha256=$($contract.JwksSha256)" -ForegroundColor Green

if (-not $Apply) { return }
if ([string]::IsNullOrWhiteSpace($KubeContext)) {
    throw '-Apply 必须显式传 -KubeContext；禁止依赖 current-context。'
}
$KubeContext = $KubeContext.Trim()
$confirm = Read-Host "将 create-only 投递到 kube-context '$KubeContext'。请输入完整 context 名确认"
if ($confirm -cne $KubeContext) { throw 'context 确认不匹配，未写集群。' }

function Get-ClusterObject([string]$KindName, [string]$Namespace) {
    $lines = @(& kubectl --context $KubeContext get $KindName -n $Namespace --ignore-not-found -o json 2>&1)
    if ($LASTEXITCODE -ne 0) { throw "读取 $Namespace/$KindName 失败:$($lines -join [Environment]::NewLine)" }
    $text = (($lines | ForEach-Object { $_.ToString() }) -join "`n").Trim()
    if ([string]::IsNullOrWhiteSpace($text)) { return $null }
    try { return ($text | ConvertFrom-Json -ErrorAction Stop) }
    catch { throw "读取 $Namespace/$KindName 返回非法 JSON:$($_.Exception.Message)" }
}

function New-ClusterObject([object]$Object, [string]$Description) {
    $json = $Object | ConvertTo-Json -Depth 20 -Compress
    $lines = @($json | & kubectl --context $KubeContext create -f - 2>&1)
    if ($LASTEXITCODE -ne 0) { throw "创建 $Description 失败:$($lines -join [Environment]::NewLine)" }
}

function Ensure-PandoraNamespace {
    # 密钥材料是 online 首次启动的前置条件，而 start.ps1 只有在完成密钥预检后才会进入业务清单
    # apply。这里与 Secret/ConfigMap 一样只做 create-only；不 patch/delete 已有 namespace，竞态或
    # apiserver 异常均 fail-closed，避免把“namespace 尚未创建”变成首次部署自举死循环。
    $lines = @(& kubectl --context $KubeContext get namespace/pandora --ignore-not-found -o json 2>&1)
    if ($LASTEXITCODE -ne 0) { throw "读取 namespace/pandora 失败:$($lines -join [Environment]::NewLine)" }
    $text = (($lines | ForEach-Object { $_.ToString() }) -join "`n").Trim()
    if ([string]::IsNullOrWhiteSpace($text)) {
        New-ClusterObject -Description 'Namespace/pandora(create-only)' -Object ([ordered]@{
            apiVersion = 'v1'; kind = 'Namespace'
            metadata = [ordered]@{
                name = 'pandora'
                labels = [ordered]@{ 'app.kubernetes.io/part-of' = 'pandora' }
            }
        })
        Write-Host "已 create-only 创建 namespace/pandora。" -ForegroundColor Green
        return
    }
    try { $namespace = $text | ConvertFrom-Json -ErrorAction Stop }
    catch { throw "读取 namespace/pandora 返回非法 JSON:$($_.Exception.Message)" }
    if ([string]$namespace.kind -cne 'Namespace' -or [string]$namespace.metadata.name -cne 'pandora') {
        throw 'namespace/pandora 回读对象身份异常，拒绝继续写入密钥材料。'
    }
}

Ensure-PandoraNamespace

$keysetAnnotations = [ordered]@{
    'pandora.dev/dsticket-active-kid' = $contract.ActiveKid
    'pandora.dev/dsticket-keyset-revision' = [string]$Revision
    'pandora.dev/dsticket-jwks-sha256' = $contract.JwksSha256
}
$signerAnnotations = [ordered]@{
    'pandora.dev/dsticket-signer-kid' = $contract.SignerKid
    'pandora.dev/dsticket-signer-revision' = [string]$SignerRevision
    'pandora.dev/dsticket-private-pem-sha256' = $contract.PrivatePemSha256
}
$labels = [ordered]@{
    'app.kubernetes.io/part-of' = 'pandora'
    'app.kubernetes.io/component' = 'dsticket-keyset'
}
$cmName = "pandora-dsticket-jwks-r$Revision"
$secretName = "pandora-dsticket-signer-r$SignerRevision"
$secret = Get-ClusterObject -KindName "secret/$secretName" -Namespace 'pandora'
$configMaps = @{
    default = Get-ClusterObject -KindName "configmap/$cmName" -Namespace 'default'
    pandora = Get-ClusterObject -KindName "configmap/$cmName" -Namespace 'pandora'
}

if ($null -eq $secret) {
    New-ClusterObject -Description "immutable Secret/$secretName" -Object ([ordered]@{
        apiVersion = 'v1'; kind = 'Secret'; immutable = $true; type = 'Opaque'
        metadata = [ordered]@{ name = $secretName; namespace = 'pandora'; labels = $labels; annotations = $signerAnnotations }
        stringData = [ordered]@{ 'private.pem' = $privatePem }
    })
}
foreach ($namespace in @('default', 'pandora')) {
    if ($null -eq $configMaps[$namespace]) {
        New-ClusterObject -Description "immutable ConfigMap/$cmName(namespace=$namespace)" -Object ([ordered]@{
            apiVersion = 'v1'; kind = 'ConfigMap'; immutable = $true
            metadata = [ordered]@{ name = $cmName; namespace = $namespace; labels = $labels; annotations = $keysetAnnotations }
            data = [ordered]@{ 'jwks.json' = $jwksText }
        })
    }
}

# 无论本轮新建还是复用，都从 apiserver 回读真实对象并重新对账；已存在但内容/annotation 漂移时拒绝，
# 永不 apply/patch/delete immutable 密钥对象。
$secret = Get-ClusterObject -KindName "secret/$secretName" -Namespace 'pandora'
$live = $null
foreach ($namespace in @('default', 'pandora')) {
    $configMap = Get-ClusterObject -KindName "configmap/$cmName" -Namespace $namespace
    $live = Assert-PandoraDSTicketKubernetesObjects -SecretObject $secret -ConfigMapObject $configMap `
        -ExpectedRevision $Revision -ExpectedSignerRevision $SignerRevision `
        -ExpectedActiveKid $contract.ActiveKid -ExpectedSignerKid $contract.SignerKid `
        -RequirePrivateKeyActive (-not [bool]$AllowInactiveSigner) -ExpectedConfigMapNamespace $namespace
    if ($live.PrivatePemSha256 -cne $contract.PrivatePemSha256 -or $live.JwksSha256 -cne $contract.JwksSha256) {
        throw "集群内 DSTicket 密钥材料与本地受控源不一致(namespace=$namespace)；拒绝覆盖，须按独立轮换流程创建新 revision。"
    }
}
Write-Host "集群 DSTicket immutable 投递对账通过:$KubeContext keyset_revision=$Revision active_kid=$($live.ActiveKid) signer_revision=$SignerRevision signer_kid=$($live.SignerKid) JWKS=default+pandora" -ForegroundColor Green
