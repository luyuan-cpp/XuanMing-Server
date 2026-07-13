# DSTicket v2(RS256)密钥材料与 K8s 投递对象的纯校验函数。
#
# 本文件不调用 kubectl、不写文件、不输出私钥。signer kid 永远由私钥公钥部分计算，
# 再与 JWKS 中同 kid 的 n/e 对账；JWKS active_kid 独立显式读取，严禁用 keys[0] 猜当前签发键。
# 普通发布要求 signer kid == active_kid；RequirePrivateKeyActive=false 只供显式密钥材料预投递，
# 当前 start.ps1 不开放该行为，也不会把预投递对象切到运行中 signer。

function ConvertTo-PandoraBase64Url {
    param([Parameter(Mandatory = $true)][byte[]]$Bytes)
    return [Convert]::ToBase64String($Bytes).TrimEnd('=').Replace('+', '-').Replace('/', '_')
}

function ConvertFrom-PandoraBase64Url {
    param([Parameter(Mandatory = $true)][string]$Value, [string]$Where = 'base64url')
    if ($Value -cnotmatch '^[A-Za-z0-9_-]+$') { throw "$Where 不是 canonical base64url(无 padding)。" }
    $padded = $Value.Replace('-', '+').Replace('_', '/')
    switch ($padded.Length % 4) {
        0 { }
        2 { $padded += '==' }
        3 { $padded += '=' }
        default { throw "$Where 的 base64url 长度非法。" }
    }
    try { return [Convert]::FromBase64String($padded) }
    catch { throw "$Where 的 base64url 解码失败:$($_.Exception.Message)" }
}

function Get-PandoraSha256Hex {
    param([Parameter(Mandatory = $true)][byte[]]$Bytes)
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try { return ([Convert]::ToHexString($sha.ComputeHash($Bytes))).ToLowerInvariant() }
    finally { $sha.Dispose() }
}

function Initialize-PandoraDSTicketPemReader {
    if ('PandoraDSTicketPemReader' -as [type]) { return }
    Add-Type -TypeDefinition @'
using System;
using System.Security.Cryptography;

public static class PandoraDSTicketPemReader
{
    public static string[] ReadPublicParameters(string privatePem)
    {
        using (RSA rsa = RSA.Create())
        {
            rsa.ImportFromPem(privatePem);
            RSAParameters p = rsa.ExportParameters(false);
            if (p.Modulus == null || p.Exponent == null)
                throw new CryptographicException("RSA public parameters missing");
            return new[] {
                Convert.ToBase64String(p.Modulus).TrimEnd('=').Replace('+', '-').Replace('/', '_'),
                Convert.ToBase64String(p.Exponent).TrimEnd('=').Replace('+', '-').Replace('/', '_'),
                rsa.KeySize.ToString(System.Globalization.CultureInfo.InvariantCulture)
            };
        }
    }
}
'@
}

function Get-PandoraDSTicketPrivateKeyPublicContract {
    param([Parameter(Mandatory = $true)][string]$PrivateKeyPem)
    if ([string]::IsNullOrWhiteSpace($PrivateKeyPem)) { throw 'DSTicket 私钥 PEM 为空。' }
    Initialize-PandoraDSTicketPemReader
    try { $parts = [PandoraDSTicketPemReader]::ReadPublicParameters($PrivateKeyPem) }
    catch { throw "DSTicket 私钥不是可用的 RSA PEM:$($_.Exception.Message)" }
    $keyBits = [int]$parts[2]
    if ($keyBits -lt 2048) { throw "DSTicket RSA 私钥过弱:$keyBits bits(至少 2048)。" }
    $n = [string]$parts[0]
    $e = [string]$parts[1]
    $canonical = '{"e":"' + $e + '","kty":"RSA","n":"' + $n + '"}'
    $kid = ConvertTo-PandoraBase64Url ([System.Security.Cryptography.SHA256]::HashData(
        [System.Text.Encoding]::UTF8.GetBytes($canonical)))
    return [pscustomobject]@{ Kid = $kid; N = $n; E = $e; KeyBits = $keyBits }
}

function Get-PandoraDSTicketJwksContract {
    param(
        [Parameter(Mandatory = $true)][string]$JwksText,
        [Parameter(Mandatory = $true)][int]$ExpectedRevision,
        [string]$ExpectedActiveKid = ''
    )
    if ($ExpectedRevision -lt 1) { throw 'DSTicket keyset revision 必须 >= 1。' }
    if ([System.Text.Encoding]::UTF8.GetByteCount($JwksText) -gt 65536) { throw 'DSTicket JWKS 超过 64KiB。' }
    try { $jwks = $JwksText | ConvertFrom-Json -ErrorAction Stop }
    catch { throw "DSTicket JWKS 不是合法 JSON:$($_.Exception.Message)" }

    $topNames = @($jwks.PSObject.Properties.Name)
    foreach ($name in $topNames) {
        if ($name -cnotin @('revision', 'active_kid', 'keys')) { throw "DSTicket JWKS 含未知顶级字段 '$name'。" }
    }
    if ($topNames -cnotcontains 'revision' -or $topNames -cnotcontains 'active_kid' -or
        $topNames -cnotcontains 'keys') {
        throw 'DSTicket JWKS 必须含 revision、active_kid 与 keys。'
    }
    $revisionText = [string]$jwks.revision
    if ($revisionText -cnotmatch '^[1-9][0-9]*$' -or [int64]$revisionText -ne $ExpectedRevision) {
        throw "DSTicket JWKS revision=$revisionText，expected=$ExpectedRevision。"
    }
    $keys = @($jwks.keys)
    if ($keys.Count -lt 1 -or $keys.Count -gt 8) { throw "DSTicket JWKS keys 数量=$($keys.Count)，允许 1..8。" }
    $activeKid = [string]$jwks.active_kid
    if ($activeKid -cnotmatch '^[A-Za-z0-9_-]{43}$') { throw 'DSTicket JWKS active_kid 非法。' }

    $byKid = @{}
    for ($i = 0; $i -lt $keys.Count; $i++) {
        $key = $keys[$i]
        $names = @($key.PSObject.Properties.Name)
        $publicNames = @('kty', 'use', 'alg', 'kid', 'n', 'e')
        $privateNames = @('d', 'p', 'q', 'dp', 'dq', 'qi', 'oth', 'k')
        foreach ($name in $names) {
            # 公开 JWKS 采用精确字段集合。私钥/对称字段即使值是空串或 null，只要字段
            # 出现在 ConfigMap 中也属于错误投递，必须 fail-closed，不能把“当前没有值”
            # 当成未来序列化器仍安全的依据。
            if ($name -cin $privateNames) {
                throw "DSTicket JWKS key[$i] 出现禁止的私钥/对称密钥字段 '$name'，整组拒绝。"
            }
            if ($name -cnotin $publicNames) {
                throw "DSTicket JWKS key[$i] 含未知字段 '$name'。"
            }
        }
        foreach ($required in $publicNames) {
            if ($names -cnotcontains $required) { throw "DSTicket JWKS key[$i] 缺字段 '$required'。" }
        }
        if ($names.Count -ne $publicNames.Count) {
            throw "DSTicket JWKS key[$i] 字段集合必须精确为 kty,use,alg,kid,n,e。"
        }
        if ([string]$key.kty -cne 'RSA' -or [string]$key.use -cne 'sig' -or [string]$key.alg -cne 'RS256') {
            throw "DSTicket JWKS key[$i] 只允许 kty=RSA/use=sig/alg=RS256。"
        }
        $kid = [string]$key.kid
        if ($kid -cnotmatch '^[A-Za-z0-9_-]{43}$') { throw "DSTicket JWKS key[$i].kid 非法。" }
        if ($byKid.ContainsKey($kid)) { throw "DSTicket JWKS kid 重复:$kid。" }
        $nBytes = ConvertFrom-PandoraBase64Url -Value ([string]$key.n) -Where "JWKS key[$i].n"
        $eBytes = ConvertFrom-PandoraBase64Url -Value ([string]$key.e) -Where "JWKS key[$i].e"
        if ($nBytes.Length -lt 256 -or $nBytes[0] -eq 0) { throw "DSTicket JWKS key[$i] RSA modulus 小于 2048 bits 或非最短编码。" }
        if ($eBytes.Length -lt 1 -or $eBytes.Length -gt 4 -or ($eBytes.Length -gt 1 -and $eBytes[0] -eq 0)) {
            throw "DSTicket JWKS key[$i] RSA exponent 编码非法。"
        }
        [uint64]$exponent = 0
        foreach ($b in $eBytes) { $exponent = ($exponent -shl 8) -bor [uint64]$b }
        if ($exponent -lt 3 -or $exponent -gt 2147483647 -or ($exponent % 2) -eq 0) {
            throw "DSTicket JWKS key[$i] RSA exponent 非法:$exponent。"
        }
        $n = [string]$key.n
        $e = [string]$key.e
        $canonical = '{"e":"' + $e + '","kty":"RSA","n":"' + $n + '"}'
        $computedKid = ConvertTo-PandoraBase64Url ([System.Security.Cryptography.SHA256]::HashData(
            [System.Text.Encoding]::UTF8.GetBytes($canonical)))
        if ($computedKid -cne $kid) { throw "DSTicket JWKS key[$i] kid 与 RFC7638 指纹不符。" }
        $byKid[$kid] = [pscustomobject]@{ Kid = $kid; N = $n; E = $e }
    }
    if (-not $byKid.ContainsKey($activeKid)) {
        throw "DSTicket JWKS active_kid '$activeKid' 不在 revision $ExpectedRevision 的 keys 中。"
    }
    if (-not [string]::IsNullOrWhiteSpace($ExpectedActiveKid) -and $activeKid -cne $ExpectedActiveKid) {
        throw "DSTicket JWKS active_kid '$activeKid'，expected='$ExpectedActiveKid'。"
    }
    $jwksBytes = [System.Text.Encoding]::UTF8.GetBytes($JwksText)
    return [pscustomobject]@{
        Revision = $ExpectedRevision
        ActiveKid = $activeKid
        Keys = $byKid
        JwksSha256 = Get-PandoraSha256Hex $jwksBytes
    }
}

function Get-PandoraDSTicketKeyMaterialContract {
    param(
        [Parameter(Mandatory = $true)][string]$PrivateKeyPem,
        [Parameter(Mandatory = $true)][string]$JwksText,
        [Parameter(Mandatory = $true)][int]$ExpectedRevision,
        [string]$ExpectedActiveKid = '',
        [bool]$RequirePrivateKeyActive = $true
    )
    $private = Get-PandoraDSTicketPrivateKeyPublicContract -PrivateKeyPem $PrivateKeyPem
    $jwks = Get-PandoraDSTicketJwksContract -JwksText $JwksText -ExpectedRevision $ExpectedRevision `
        -ExpectedActiveKid $ExpectedActiveKid
    if ($RequirePrivateKeyActive -and $jwks.ActiveKid -cne $private.Kid) {
        throw "DSTicket active kid '$($jwks.ActiveKid)' 与私钥推导 kid '$($private.Kid)' 不一致。"
    }
    if (-not $jwks.Keys.ContainsKey($private.Kid)) {
        throw "DSTicket signer kid '$($private.Kid)' 不在 revision $ExpectedRevision 的 JWKS 中。"
    }
    $public = $jwks.Keys[$private.Kid]
    if ([string]$public.N -cne $private.N -or [string]$public.E -cne $private.E) {
        throw 'DSTicket 私钥公钥参数与 JWKS 中 signer kid 的 n/e 不一致。'
    }
    return [pscustomobject]@{
        ActiveKid = $jwks.ActiveKid
        SignerKid = $private.Kid
        Revision = $jwks.Revision
        JwksSha256 = $jwks.JwksSha256
        PrivatePemSha256 = Get-PandoraSha256Hex ([System.Text.Encoding]::UTF8.GetBytes($PrivateKeyPem))
        KeyCount = $jwks.Keys.Count
    }
}

function Assert-PandoraDSTicketKubernetesObjects {
    param(
        [Parameter(Mandatory = $true)]$SecretObject,
        [Parameter(Mandatory = $true)]$ConfigMapObject,
        [Parameter(Mandatory = $true)][int]$ExpectedRevision,
        [int]$ExpectedSignerRevision = 0,
        [string]$ExpectedActiveKid = '',
        [string]$ExpectedSignerKid = '',
        [bool]$RequirePrivateKeyActive = $true,
        [ValidateSet('default', 'pandora')][string]$ExpectedConfigMapNamespace = 'default'
    )
    if ($ExpectedSignerRevision -eq 0) { $ExpectedSignerRevision = $ExpectedRevision }
    if ($ExpectedSignerRevision -lt 1) { throw 'DSTicket signer revision 必须 >= 1。' }
    $secretName = "pandora-dsticket-signer-r$ExpectedSignerRevision"
    $cmName = "pandora-dsticket-jwks-r$ExpectedRevision"
    if ([string]$SecretObject.kind -cne 'Secret' -or [string]$SecretObject.metadata.name -cne $secretName) {
        throw "DSTicket 签发私钥对象必须是 Secret/$secretName。"
    }
    if ([string]$ConfigMapObject.kind -cne 'ConfigMap' -or [string]$ConfigMapObject.metadata.name -cne $cmName) {
        throw "DSTicket 公钥对象必须是 ConfigMap/$cmName。"
    }
    if ([string]$SecretObject.metadata.namespace -cne 'pandora' -or
        [string]$ConfigMapObject.metadata.namespace -cne $ExpectedConfigMapNamespace) {
        throw "DSTicket 对象 namespace 不合规:Secret 必须在 pandora，ConfigMap 必须在 $ExpectedConfigMapNamespace。"
    }
    if ($SecretObject.immutable -ne $true -or $ConfigMapObject.immutable -ne $true) {
        throw 'DSTicket Secret 与 JWKS ConfigMap 必须都 immutable=true；禁止原地换钥。'
    }
    $privateB64 = [string]$SecretObject.data.'private.pem'
    $jwksText = [string]$ConfigMapObject.data.'jwks.json'
    if ([string]::IsNullOrWhiteSpace($privateB64) -or [string]::IsNullOrWhiteSpace($jwksText)) {
        throw 'DSTicket K8s 对象缺 private.pem 或 jwks.json。'
    }
    try { $privatePem = [System.Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($privateB64)) }
    catch { throw "DSTicket Secret/private.pem base64 非法:$($_.Exception.Message)" }
    $contract = Get-PandoraDSTicketKeyMaterialContract -PrivateKeyPem $privatePem -JwksText $jwksText `
        -ExpectedRevision $ExpectedRevision -ExpectedActiveKid $ExpectedActiveKid `
        -RequirePrivateKeyActive $RequirePrivateKeyActive
    if (-not [string]::IsNullOrWhiteSpace($ExpectedSignerKid) -and $contract.SignerKid -cne $ExpectedSignerKid) {
        throw "DSTicket signer kid '$($contract.SignerKid)'，expected='$ExpectedSignerKid'。"
    }
    $secretAnnotations = $SecretObject.metadata.annotations
    if ([string]$secretAnnotations.'pandora.dev/dsticket-signer-kid' -cne $contract.SignerKid -or
        [string]$secretAnnotations.'pandora.dev/dsticket-signer-revision' -cne [string]$ExpectedSignerRevision -or
        [string]$secretAnnotations.'pandora.dev/dsticket-private-pem-sha256' -cne $contract.PrivatePemSha256) {
        throw "Secret/$secretName 的 signer 对账 annotation 与真实私钥不一致。"
    }
    $cmAnnotations = $ConfigMapObject.metadata.annotations
    if ([string]$cmAnnotations.'pandora.dev/dsticket-active-kid' -cne $contract.ActiveKid -or
        [string]$cmAnnotations.'pandora.dev/dsticket-keyset-revision' -cne [string]$ExpectedRevision -or
        [string]$cmAnnotations.'pandora.dev/dsticket-jwks-sha256' -cne $contract.JwksSha256) {
        throw "ConfigMap/$cmName 的 keyset 对账 annotation 与真实 JWKS 不一致。"
    }
    return $contract
}
