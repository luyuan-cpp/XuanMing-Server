[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$ProjectRoot = (Resolve-Path "$PSScriptRoot/../../..").Path
. (Join-Path $ProjectRoot 'tools/scripts/lib/dsticket_keyset_contract.ps1')

function Assert-True([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw "ASSERT FAILED:$Message" }
}
function Assert-Throws([scriptblock]$Action, [string]$Message) {
    try { & $Action } catch { return }
    throw "ASSERT FAILED:应抛错但成功:$Message"
}

$tmpRoot = Join-Path ([System.IO.Path]::GetTempPath()) ('pandora-dsticket-contract-' + [guid]::NewGuid().ToString('N'))
$k1 = Join-Path $tmpRoot 'k1'
$k2 = Join-Path $tmpRoot 'k2'
$k2PhaseA = Join-Path $tmpRoot 'k2-phase-a'
$k2Promoted = Join-Path $tmpRoot 'k2-promoted'
$k2Only = Join-Path $tmpRoot 'k2-only'
try {
    Push-Location $ProjectRoot
    try {
        & go run ./tools/dsticketkeys -out $k1 -revision 1 *> $null
        if ($LASTEXITCODE -ne 0) { throw '测试生成 K1 失败。' }
    } finally { Pop-Location }

    $k1Pem = Get-Content (Join-Path $k1 'private.pem') -Raw
    $k1Jwks = Get-Content (Join-Path $k1 'jwks.json') -Raw
    $k1Contract = Get-PandoraDSTicketKeyMaterialContract -PrivateKeyPem $k1Pem -JwksText $k1Jwks -ExpectedRevision 1

    Push-Location $ProjectRoot
    try {
        & go run ./tools/dsticketkeys -out $k2 -revision 2 -merge (Join-Path $k1 'jwks.json') *> $null
        if ($LASTEXITCODE -ne 0) { throw '测试生成 K2+K1 keyset 失败。' }
        & go run ./tools/dsticketkeys -out $k2PhaseA -revision 2 -merge (Join-Path $k1 'jwks.json') `
            -active-kid $k1Contract.ActiveKid *> $null
        if ($LASTEXITCODE -ne 0) { throw '测试生成 Phase A keyset 失败。' }
        & go run ./tools/dsticketkeys -out $k2Promoted -revision 3 `
            -private-in (Join-Path $k2PhaseA 'private.pem') -merge (Join-Path $k2PhaseA 'jwks.json') *> $null
        if ($LASTEXITCODE -ne 0) { throw '测试复用 K2 私钥生成提升 keyset 失败。' }
        & go run ./tools/dsticketkeys -out $k2Only -revision 4 `
            -private-in (Join-Path $k2PhaseA 'private.pem') *> $null
        if ($LASTEXITCODE -ne 0) { throw '测试复用 K2 私钥生成退役 keyset 失败。' }
    } finally { Pop-Location }

    $k2Pem = Get-Content (Join-Path $k2 'private.pem') -Raw
    $k2Jwks = Get-Content (Join-Path $k2 'jwks.json') -Raw
    $contract = Get-PandoraDSTicketKeyMaterialContract -PrivateKeyPem $k2Pem -JwksText $k2Jwks -ExpectedRevision 2
    Assert-True ($contract.ActiveKid -cmatch '^[A-Za-z0-9_-]{43}$') 'active kid 由私钥推导'
    Assert-True ($contract.KeyCount -eq 2) '轮换 keyset 有两把公钥'
    $expectedPublicFields = @('alg', 'e', 'kid', 'kty', 'n', 'use')
    $generatedJwks = $k2Jwks | ConvertFrom-Json
    foreach ($i in 0..($generatedJwks.keys.Count - 1)) {
        $actualFields = @($generatedJwks.keys[$i].PSObject.Properties.Name) | Sort-Object
        Assert-True (($actualFields -join ',') -ceq ($expectedPublicFields -join ',')) `
            "生成的 JWKS key[$i] 必须只有精确 public 字段集合"
    }

    # Phase A 的 signer(K2)必须已进入 JWKS，但 active 仍为 K1；默认严格模式拒绝误挂载。
    $phaseAPem = Get-Content (Join-Path $k2PhaseA 'private.pem') -Raw
    $phaseAJwks = Get-Content (Join-Path $k2PhaseA 'jwks.json') -Raw
    $phaseA = Get-PandoraDSTicketKeyMaterialContract -PrivateKeyPem $phaseAPem -JwksText $phaseAJwks `
        -ExpectedRevision 2 -ExpectedActiveKid $k1Contract.ActiveKid -RequirePrivateKeyActive $false
    Assert-True ($phaseA.ActiveKid -ceq $k1Contract.ActiveKid) 'Phase A 保持 K1 active'
    Assert-True ($phaseA.SignerKid -cne $phaseA.ActiveKid) 'Phase A 的 K2 signer 只预投递'
    Assert-Throws {
        Get-PandoraDSTicketKeyMaterialContract -PrivateKeyPem $phaseAPem -JwksText $phaseAJwks `
            -ExpectedRevision 2 -ExpectedActiveKid $k1Contract.ActiveKid
    } '严格模式拒绝把未 active 的 K2 挂到 signer'

    $promotedPem = Get-Content (Join-Path $k2Promoted 'private.pem') -Raw
    $promotedJwks = Get-Content (Join-Path $k2Promoted 'jwks.json') -Raw
    $promoted = Get-PandoraDSTicketKeyMaterialContract -PrivateKeyPem $promotedPem -JwksText $promotedJwks -ExpectedRevision 3
    Assert-True ($promoted.SignerKid -ceq $phaseA.SignerKid) '提升阶段复用同一 K2 私钥'
    Assert-True ($promoted.KeyCount -eq 2) '提升阶段仍保留 K1+K2'
    $onlyPem = Get-Content (Join-Path $k2Only 'private.pem') -Raw
    $onlyJwks = Get-Content (Join-Path $k2Only 'jwks.json') -Raw
    $only = Get-PandoraDSTicketKeyMaterialContract -PrivateKeyPem $onlyPem -JwksText $onlyJwks -ExpectedRevision 4
    Assert-True ($only.SignerKid -ceq $phaseA.SignerKid) '退役阶段继续复用同一 K2 私钥'
    Assert-True ($only.KeyCount -eq 1) '退役阶段 JWKS 仅保留 K2'

    # 明确把 active key 放到 keys[1]，证明实现不依赖 keys[0]。
    $obj = $k2Jwks | ConvertFrom-Json
    Assert-True ([string]$obj.active_kid -ceq $contract.ActiveKid) 'JWKS 顶层 active_kid 显式声明签发键'
    $activeIndex = if ([string]$obj.keys[0].kid -ceq $contract.ActiveKid) { 0 } else { 1 }
    if ($activeIndex -eq 0) { $obj.keys = @($obj.keys[1], $obj.keys[0]) }
    $activeIndex = if ([string]$obj.keys[0].kid -ceq $contract.ActiveKid) { 0 } else { 1 }
    Assert-True ($activeIndex -eq 1) '测试前置:active key 位于 keys[1]'
    $reordered = $obj | ConvertTo-Json -Depth 10
    $again = Get-PandoraDSTicketKeyMaterialContract -PrivateKeyPem $k2Pem -JwksText $reordered -ExpectedRevision 2 `
        -ExpectedActiveKid $contract.ActiveKid
    Assert-True ($again.ActiveKid -ceq $contract.ActiveKid) 'keys 重排不改变 active kid 对账'

    $wrongFirstKid = [string]$obj.keys[0].kid
    Assert-Throws {
        Get-PandoraDSTicketKeyMaterialContract -PrivateKeyPem $k2Pem -JwksText $reordered -ExpectedRevision 2 `
            -ExpectedActiveKid $wrongFirstKid
    } '拒绝把 keys[0] 当 active kid'
    Assert-Throws {
        Get-PandoraDSTicketKeyMaterialContract -PrivateKeyPem $k1Pem -JwksText ($k1Jwks.Replace('"revision": 1', '"revision": 2')) `
            -ExpectedRevision 2 -ExpectedActiveKid $contract.ActiveKid
    } '错私钥/active kid 拒绝'
    Assert-Throws {
        Get-PandoraDSTicketKeyMaterialContract -PrivateKeyPem $k2Pem -JwksText $k2Jwks -ExpectedRevision 3
    } 'revision 漂移拒绝'
    foreach ($privateField in @('d', 'p', 'q', 'dp', 'dq', 'qi', 'oth', 'k')) {
        foreach ($privateValue in @('', $null, 'leak')) {
            $privateLeakObject = $k2Jwks | ConvertFrom-Json
            $privateLeakObject.keys[0] | Add-Member -NotePropertyName $privateField -NotePropertyValue $privateValue -Force
            $privateLeak = $privateLeakObject | ConvertTo-Json -Depth 10
            Assert-Throws {
                Get-PandoraDSTicketJwksContract -JwksText $privateLeak -ExpectedRevision 2
            } "JWKS 私钥/对称字段存在即拒绝(field=$privateField)"
        }
    }

    $cmAnnotations = [pscustomobject]@{
        'pandora.dev/dsticket-active-kid' = $contract.ActiveKid
        'pandora.dev/dsticket-keyset-revision' = '2'
        'pandora.dev/dsticket-jwks-sha256' = $contract.JwksSha256
    }
    $secretAnnotations = [pscustomobject]@{
        'pandora.dev/dsticket-signer-kid' = $contract.SignerKid
        'pandora.dev/dsticket-signer-revision' = '2'
        'pandora.dev/dsticket-private-pem-sha256' = $contract.PrivatePemSha256
    }
    $secret = [pscustomobject]@{
        kind = 'Secret'; immutable = $true
        metadata = [pscustomobject]@{ name = 'pandora-dsticket-signer-r2'; namespace = 'pandora'; annotations = $secretAnnotations }
        data = [pscustomobject]@{ 'private.pem' = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($k2Pem)) }
    }
    $cm = [pscustomobject]@{
        kind = 'ConfigMap'; immutable = $true
        metadata = [pscustomobject]@{ name = 'pandora-dsticket-jwks-r2'; namespace = 'default'; annotations = $cmAnnotations }
        data = [pscustomobject]@{ 'jwks.json' = $k2Jwks }
    }
    $live = Assert-PandoraDSTicketKubernetesObjects -SecretObject $secret -ConfigMapObject $cm -ExpectedRevision 2 `
        -ExpectedActiveKid $contract.ActiveKid
    Assert-True ($live.ActiveKid -ceq $contract.ActiveKid) 'K8s Secret/ConfigMap 对账'
    $cm.metadata.namespace = 'pandora'
    $liveLogin = Assert-PandoraDSTicketKubernetesObjects -SecretObject $secret -ConfigMapObject $cm -ExpectedRevision 2 `
        -ExpectedActiveKid $contract.ActiveKid -ExpectedConfigMapNamespace pandora
    Assert-True ($liveLogin.JwksSha256 -ceq $live.JwksSha256) 'Login/DS 两个 namespace 公钥对象同 hash'
    $secret.metadata.name = 'pandora-dsticket'
    Assert-Throws {
        Assert-PandoraDSTicketKubernetesObjects -SecretObject $secret -ConfigMapObject $cm -ExpectedRevision 2 `
            -ExpectedConfigMapNamespace pandora
    } '拒绝 legacy 非 revisioned signer Secret 名'
    $secret.metadata.name = 'pandora-dsticket-signer-r2'
    $secret.metadata.annotations.'pandora.dev/dsticket-signer-revision' = '1'
    Assert-Throws {
        Assert-PandoraDSTicketKubernetesObjects -SecretObject $secret -ConfigMapObject $cm -ExpectedRevision 2 `
            -ExpectedConfigMapNamespace pandora
    } '拒绝 signer revision annotation 漂移'
    $secret.metadata.annotations.'pandora.dev/dsticket-signer-revision' = '2'
    $cm.immutable = $false
    Assert-Throws {
        Assert-PandoraDSTicketKubernetesObjects -SecretObject $secret -ConfigMapObject $cm -ExpectedRevision 2 `
            -ExpectedConfigMapNamespace pandora
    } '拒绝 mutable ConfigMap'

    # start.ps1 的三个入口也必须遵守同一个命名契约。这里解析 AST 并只检查对应函数，
    # 避免 legacy 清理门禁中有意出现的旧名字让静态断言产生假阳性。
    $startScriptPath = Join-Path $ProjectRoot 'tools/scripts/start.ps1'
    $startTokens = $null
    $startParseErrors = $null
    $startAst = [System.Management.Automation.Language.Parser]::ParseFile(
        $startScriptPath, [ref]$startTokens, [ref]$startParseErrors)
    Assert-True ($startParseErrors.Count -eq 0) 'start.ps1 AST 可解析'
    $startFunctions = @($startAst.FindAll({
                param($node)
                $node -is [System.Management.Automation.Language.FunctionDefinitionAst]
            }, $true))
    $ensureAst = @($startFunctions | Where-Object Name -CEQ 'Ensure-DsTicketDevKeyMaterial')
    $invokeK8sAst = @($startFunctions | Where-Object Name -CEQ 'Invoke-K8s')
    $resumeAst = @($startFunctions | Where-Object Name -CEQ 'Resume-K8s')
    $onlineAst = @($startFunctions | Where-Object Name -CEQ 'Invoke-Online')
    Assert-True ($ensureAst.Count -eq 1) '唯一 local DSTicket bootstrap 函数'
    Assert-True ($invokeK8sAst.Count -eq 1) '唯一 Invoke-K8s 函数'
    Assert-True ($resumeAst.Count -eq 1) '唯一 Resume-K8s 函数'
    Assert-True ($onlineAst.Count -eq 1) '唯一 Invoke-Online 函数'

    $ensureText = $ensureAst[0].Extent.Text
    Assert-True ($ensureText -clike '*pandora-dsticket-signer-r1*') 'local bootstrap 只创建/回读 signer-r1'
    Assert-True ($ensureText -cmatch '(?m)^\s*&\s+go\s+run\s+\./tools/dsticketkeys\b[^\r\n]*\|\s*Out-Host\s*$') `
        'fresh local bootstrap 必须隔离 dsticketkeys stdout，Ensure 只能返回 active kid 字符串'
    foreach ($annotation in @(
            'pandora.dev/dsticket-signer-kid',
            'pandora.dev/dsticket-signer-revision',
            'pandora.dev/dsticket-private-pem-sha256')) {
        Assert-True ($ensureText -clike "*$annotation*") "local signer 写入独立注解 $annotation"
    }
    Assert-True ($ensureText -cnotmatch "[`"']secret/pandora-dsticket[`"']") `
        'local bootstrap 不再创建或回读 legacy fixed Secret'
    Assert-True ($ensureText -cnotmatch "name\s*=\s*[`"']pandora-dsticket[`"']") `
        'local bootstrap metadata 不再使用 legacy fixed Secret 名'

    Assert-True ($invokeK8sAst[0].Extent.Text -clike '*Ensure-DsTicketDevKeyMaterial*') `
        'local 初次启动调用 revisioned bootstrap'
    Assert-True ($resumeAst[0].Extent.Text -clike '*Ensure-DsTicketDevKeyMaterial*') `
        'Resume-K8s 调用相同 revisioned bootstrap'
    $onlineText = $onlineAst[0].Extent.Text
    Assert-True ($onlineText -clike '*$dstTicketSignerSecretName = "pandora-dsticket-signer-r$dstTicketRevision"*') `
        'online preflight 按 keyset revision 读取同 revision signer Secret'
    Assert-True ($onlineText -clike '*"secret/$dstTicketSignerSecretName"*') `
        'online preflight 使用计算出的 revisioned signer Secret 名回读集群'

    # 首次 online 部署必须先有 pandora namespace，才能 create-only 投递 signer/JWKS；keyset
    # 工具在用户显式确认 context 后负责同链路 create-only 自举，且必须发生在任何 namespaced
    # 对象读取之前，避免 start.ps1 的后置 namespace apply 形成循环依赖。
    $keysetScriptPath = Join-Path $ProjectRoot 'tools/scripts/dsticket_keyset.ps1'
    $keysetTokens = $null
    $keysetParseErrors = $null
    $keysetAst = [System.Management.Automation.Language.Parser]::ParseFile(
        $keysetScriptPath, [ref]$keysetTokens, [ref]$keysetParseErrors)
    Assert-True ($keysetParseErrors.Count -eq 0) 'dsticket_keyset.ps1 AST 可解析'
    $keysetFunctions = @($keysetAst.FindAll({
                param($node)
                $node -is [System.Management.Automation.Language.FunctionDefinitionAst]
            }, $true))
    $namespaceFunctions = @($keysetFunctions | Where-Object Name -CEQ 'Ensure-PandoraNamespace')
    Assert-True ($namespaceFunctions.Count -eq 1) '唯一 create-only namespace 自举函数'
    $namespaceText = $namespaceFunctions[0].Extent.Text
    Assert-True ($namespaceText -clike '*get namespace/pandora --ignore-not-found -o json*') `
        'namespace 自举先回读真实对象'
    Assert-True ($namespaceText -clike "*kind = 'Namespace'*") '缺失时 create-only 创建 Namespace'
    Assert-True ($namespaceText -clike '*New-ClusterObject*') 'namespace 与密钥对象共用 create-only 写路径'
    Assert-True ($namespaceText -cnotmatch '(?i)\bkubectl\b[^\r\n]*\b(apply|patch|delete)\b') `
        'namespace 自举禁止 apply/patch/delete'
    $keysetCommands = @($keysetAst.FindAll({
                param($node)
                $node -is [System.Management.Automation.Language.CommandAst]
            }, $true))
    $ensureNamespaceCalls = @($keysetCommands | Where-Object { $_.GetCommandName() -ceq 'Ensure-PandoraNamespace' })
    $getObjectCalls = @($keysetCommands | Where-Object { $_.GetCommandName() -ceq 'Get-ClusterObject' })
    Assert-True ($ensureNamespaceCalls.Count -eq 1) '主流程只调用一次 namespace 自举'
    Assert-True ($getObjectCalls.Count -ge 1) '主流程存在 namespaced 对象回读'
    Assert-True ($ensureNamespaceCalls[0].Extent.StartOffset -lt ($getObjectCalls | Measure-Object -Property { $_.Extent.StartOffset } -Minimum).Minimum) `
        'namespace 自举必须早于首个 namespaced 对象回读'
}
finally {
    if (Test-Path -LiteralPath $tmpRoot -PathType Container) {
        $resolved = [System.IO.Path]::GetFullPath($tmpRoot)
        $safeParent = [System.IO.Path]::GetFullPath([System.IO.Path]::GetTempPath())
        if (-not $resolved.StartsWith($safeParent, [StringComparison]::OrdinalIgnoreCase) -or
            (Split-Path -Leaf $resolved) -notmatch '^pandora-dsticket-contract-[0-9a-f]{32}$') {
            throw "拒绝清理未验证测试目录:$resolved"
        }
        Remove-Item -LiteralPath $resolved -Recurse -Force
    }
}

Write-Host 'dsticket_keyset_contract_test: PASS' -ForegroundColor Green
