[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$ProjectRoot = (Resolve-Path "$PSScriptRoot/../../..").Path
$InfraPath = Join-Path $ProjectRoot 'deploy/k8s/infra/infra.yaml'
$LokiPath = Join-Path $ProjectRoot 'deploy/k8s/infra/loki.yaml'

function Assert-True([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw "ASSERT FAILED:$Message" }
}

function Assert-Throws([scriptblock]$Action, [string]$Message) {
    try { & $Action } catch { return }
    throw "ASSERT FAILED:应抛错但成功:$Message"
}

function Assert-ResumeAuditOrdering([string]$ResumeSource) {
    $apiReady = $ResumeSource.IndexOf('Wait-KubeApiServerReady -KubeContext $mkCtx', [StringComparison]::Ordinal)
    $auditMarkers = @(
        'Assert-ExistingLocalEtcdPersistence -KubeContext $mkCtx',
        'Assert-NoLegacyDsFleets -KubeContext $mkCtx -LocalDevelopment',
        'Assert-NoLegacyDSTicketSignerSecret -KubeContext $mkCtx -LocalDevelopment',
        'Assert-LocalDsAuthBaseline -KubeContext $mkCtx -MinikubeProfile $mkProfile -AllowFreshBootstrap:$false'
    )
    Assert-True ($apiReady -ge 0) 'Resume 必须先等待 apiserver /readyz'
    $etcdReady = $ResumeSource.IndexOf('rollout status deploy/etcd', [StringComparison]::Ordinal)
    Assert-True ($etcdReady -gt $apiReady) 'Resume 必须在 apiserver 后仅等待 infra etcd，才能线性读取 required policy'
    $auditPositions = @()
    foreach ($marker in $auditMarkers) {
        $position = $ResumeSource.IndexOf($marker, [StringComparison]::Ordinal)
        Assert-True ($position -ge 0) "Resume 缺最早只读审计:$marker"
        Assert-True ($position -gt $etcdReady) "审计必须在 apiserver 与 infra etcd ready 之后:$marker"
        $auditPositions += $position
    }
    $firstBusinessReady = $ResumeSource.IndexOf('rollout status deploy/$($svc.Name)', [StringComparison]::Ordinal)
    $firstConfigWrite = $ResumeSource.IndexOf('Apply-PandoraConfigSecret', [StringComparison]::Ordinal)
    $firstApply = $ResumeSource.IndexOf('kubectl --context $mkCtx apply -k', [StringComparison]::Ordinal)
    $firstRestart = $ResumeSource.IndexOf('kubectl --context $mkCtx rollout restart', [StringComparison]::Ordinal)
    foreach ($barrier in @($firstBusinessReady, $firstConfigWrite, $firstApply, $firstRestart)) {
        Assert-True ($barrier -ge 0) 'Resume 顺序测试缺业务等待/写入 marker'
        foreach ($auditPosition in $auditPositions) {
            Assert-True ($auditPosition -lt $barrier) '所有只读审计必须早于业务 writer Ready、apply 与 rollout；infra etcd Ready 是线性读前置'
        }
    }
}

function Assert-EtcdPersistenceContract([string]$Manifest) {
    $tmp = Join-Path ([System.IO.Path]::GetTempPath()) ('pandora-infra-contract-' + [guid]::NewGuid().ToString('N') + '.yaml')
    try {
        [System.IO.File]::WriteAllText($tmp, $Manifest, [System.Text.UTF8Encoding]::new($false))
        $jsonPath = '{.kind}{"\t"}{.metadata.name}{"\t"}{.spec.strategy.type}{"\t"}{.spec.accessModes[*]}{"\t"}{.spec.resources.requests.storage}{"\t"}{.spec.template.spec.containers[?(@.name=="etcd")].volumeMounts[?(@.name=="data")].mountPath}{"\t"}{.spec.template.spec.volumes[?(@.name=="data")].persistentVolumeClaim.claimName}{"\n"}'
        $lines = @(& kubectl create --dry-run=client --validate=false -f $tmp -o "jsonpath=$jsonPath" 2>&1)
        if ($LASTEXITCODE -ne 0) { throw "kubectl client parse 失败:$($lines -join [Environment]::NewLine)" }
        $objects = @{}
        foreach ($row in $lines) {
            if ([string]::IsNullOrWhiteSpace($row)) { continue }
            $fields = @([regex]::Split($row.ToString(), "`t"))
            if ($fields.Count -ne 7) { throw "infra contract 列数=$($fields.Count)，应为 7:$row" }
            $objects[([string]$fields[0] + '/' + [string]$fields[1])] = $fields
        }
        Assert-True ($objects.ContainsKey('PersistentVolumeClaim/etcd-data')) '缺 PVC/etcd-data'
        Assert-True ($objects.ContainsKey('Deployment/etcd')) '缺 Deployment/etcd'
        $pvc = $objects['PersistentVolumeClaim/etcd-data']
        Assert-True ([string]$pvc[3] -ceq 'ReadWriteOnce') 'etcd PVC 必须 ReadWriteOnce'
        Assert-True ([string]$pvc[4] -ceq '1Gi') 'etcd PVC 请求应为 1Gi'
        $deploy = $objects['Deployment/etcd']
        Assert-True ([string]$deploy[2] -ceq 'Recreate') '单副本 etcd 必须 Recreate'
        Assert-True ([string]$deploy[5] -ceq '/etcd-data') 'etcd data 必须挂到 /etcd-data'
        Assert-True ([string]$deploy[6] -ceq 'etcd-data') 'etcd data 卷必须来自 PVC/etcd-data'
        Assert-True ($Manifest -cmatch '(?m)^\s*- --data-dir=/etcd-data\s*$') 'etcd --data-dir 必须对齐 PVC mount'
    } finally {
        Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
    }
}

function Assert-DeploymentProgressDeadlines(
    [string]$Manifest,
    [string[]]$DeploymentNames,
    [string]$ContractName) {
    $tmp = Join-Path ([System.IO.Path]::GetTempPath()) ('pandora-progress-deadline-' + [guid]::NewGuid().ToString('N') + '.yaml')
    try {
        [System.IO.File]::WriteAllText($tmp, $Manifest, [System.Text.UTF8Encoding]::new($false))
        $jsonPath = '{.kind}{"\t"}{.metadata.name}{"\t"}{.spec.progressDeadlineSeconds}{"\n"}'
        $lines = @(& kubectl create --dry-run=client --validate=false -f $tmp -o "jsonpath=$jsonPath" 2>&1)
        if ($LASTEXITCODE -ne 0) { throw "kubectl client parse 失败:$($lines -join [Environment]::NewLine)" }
        $deadlines = @{}
        foreach ($row in $lines) {
            if ([string]::IsNullOrWhiteSpace($row)) { continue }
            $fields = @([regex]::Split($row.ToString(), "`t"))
            if ($fields.Count -ne 3) { throw "progress deadline contract 列数=$($fields.Count)，应为 3:$row" }
            if ([string]$fields[0] -ceq 'Deployment') {
                $deadlines[[string]$fields[1]] = [string]$fields[2]
            }
        }
        foreach ($name in $DeploymentNames) {
            Assert-True ($deadlines.ContainsKey($name)) "$ContractName 缺 Deployment/$name"
            Assert-True ([string]$deadlines[$name] -ceq '1800') `
                "$ContractName Deployment/$name 的 progressDeadlineSeconds 必须为 1800"
        }
    } finally {
        Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
    }
}

$manifest = Get-Content -LiteralPath $InfraPath -Raw
Assert-EtcdPersistenceContract $manifest
Assert-DeploymentProgressDeadlines -Manifest $manifest `
    -DeploymentNames @('mysql', 'redis', 'etcd', 'zookeeper', 'kafka') -ContractName '关键第三方基础设施'
$lokiManifest = Get-Content -LiteralPath $LokiPath -Raw
Assert-DeploymentProgressDeadlines -Manifest $lokiManifest `
    -DeploymentNames @('loki', 'alloy') -ContractName 'Grafana 日志基础设施'
Assert-Throws {
    Assert-EtcdPersistenceContract ($manifest.Replace('persistentVolumeClaim: { claimName: etcd-data }', 'emptyDir: {}'))
} '拒绝 etcd 回退 emptyDir'
Assert-Throws {
    Assert-EtcdPersistenceContract ($manifest.Replace('strategy: { type: Recreate }', 'strategy: { type: RollingUpdate }'))
} '拒绝单节点 etcd RollingUpdate'

$startTokens = $null
$startErrors = $null
$startAst = [System.Management.Automation.Language.Parser]::ParseFile(
    (Join-Path $ProjectRoot 'tools/scripts/start.ps1'), [ref]$startTokens, [ref]$startErrors)
Assert-True ($startErrors.Count -eq 0) 'start.ps1 必须可解析'
$functions = @($startAst.FindAll({
            param($node)
            $node -is [System.Management.Automation.Language.FunctionDefinitionAst]
        }, $true))
$resume = @($functions | Where-Object Name -CEQ 'Resume-K8s')
$apiWait = @($functions | Where-Object Name -CEQ 'Wait-KubeApiServerReady')
$removeInfra = @($functions | Where-Object Name -CEQ 'Remove-K8sManifestObjectsPreserving')
$identities = @($functions | Where-Object Name -CEQ 'Get-K8sManifestObjectIdentities')
$preservedAssert = @($functions | Where-Object Name -CEQ 'Assert-LocalEtcdDataPreservedAfterDown')
$existingPersistence = @($functions | Where-Object Name -CEQ 'Assert-ExistingLocalEtcdPersistence')
$invokeK8s = @($functions | Where-Object Name -CEQ 'Invoke-K8s')
Assert-True ($resume.Count -eq 1) '必须有唯一 Resume-K8s'
Assert-True ($apiWait.Count -eq 1) '必须有唯一 Wait-KubeApiServerReady'
Assert-True ($removeInfra.Count -eq 1) '必须有唯一按谓词保留对象的清单删除函数'
Assert-True ($identities.Count -eq 1) '必须有唯一逐文档识别 kind/name/namespace 的函数'
Assert-True ($preservedAssert.Count -eq 1) '必须有唯一 Down 后 namespace+PVC 保留校验函数'
Assert-True ($existingPersistence.Count -eq 1) '必须有唯一现有 etcd 持久化门禁函数'
Assert-True ($invokeK8s.Count -eq 1) '必须有唯一 Invoke-K8s'
Assert-True ($apiWait[0].Extent.Text.Contains('get --raw=/readyz')) 'apiserver 等待必须只读 /readyz'
$apiWaitCommands = @($apiWait[0].FindAll({
            param($node)
            $node -is [System.Management.Automation.Language.CommandAst]
        }, $true))
$apiKubectlCommands = @($apiWaitCommands | Where-Object { $_.GetCommandName() -ceq 'kubectl' })
Assert-True ($apiKubectlCommands.Count -eq 1 -and
    $apiKubectlCommands[0].Extent.Text -cnotmatch '\b(?:apply|rollout|create|delete|patch|replace)\b') `
    'apiserver 等待只能执行 kubectl get /readyz，不得等待旧业务或写集群'
$resumeSource = $resume[0].Extent.Text
Assert-ResumeAuditOrdering -ResumeSource $resumeSource
$lateAuditMutant = $resumeSource.Replace(
    'Assert-NoLegacyDsFleets -KubeContext $mkCtx -LocalDevelopment',
    'Deferred-NoLegacyDsFleets -KubeContext $mkCtx -LocalDevelopment') +
    "`nAssert-NoLegacyDsFleets -KubeContext `$mkCtx -LocalDevelopment"
Assert-Throws {
    Assert-ResumeAuditOrdering -ResumeSource $lateAuditMutant
} '任一审计移到业务 Ready/apply/rollout 后必须被 mutant 契约阻断'

$identitiesSource = $identities[0].Extent.Text
Assert-True ($identitiesSource.Contains('create --dry-run=client -f -') -and
    $identitiesSource.Contains('jsonpath={.kind}|{.metadata.name}|{.metadata.namespace}')) `
    '逐文档识别必须走客户端 dry-run，避免多文档 JSON 流解析失败'

$removeSource = $removeInfra[0].Extent.Text
Assert-True ($removeSource.Contains('kubectl --context $KubeContext delete -f - --ignore-not-found')) `
    '按谓词删除必须把非保留对象合并成一份清单批量删除'

$preservedAssertSource = $preservedAssert[0].Extent.Text
Assert-True ($preservedAssertSource.Contains("namespace/$K8sNamespace") -or
    $preservedAssertSource.Contains('namespace/$K8sNamespace')) `
    'Down 后必须回读 namespace 仍在（删 namespace 会级联清空 etcd PVC）'
Assert-True ($preservedAssertSource.Contains("get pvc/etcd-data -n `$K8sNamespace")) `
    'Down 后必须回读 PVC/etcd-data 仍在'

$existingPersistenceSource = $existingPersistence[0].Extent.Text
Assert-True ($existingPersistenceSource.Contains('get pvc/etcd-data')) `
    '现有 Deployment/etcd 必须在 infra apply 前回读其 PVC，禁止同名空盘被自动重建'
Assert-True ($existingPersistenceSource.Contains("pvcPhase -cne 'Bound'")) `
    '现有 PVC/etcd-data 默认必须为 Bound 且 UID 非空'
Assert-True ($existingPersistenceSource.Contains("AllowPendingPvcForPreinfra -and `$pvcPhase -ceq 'Pending'")) `
    '只有调用方已验证 preinfra marker 时才可暂时等待 Pending PVC'
Assert-True ($existingPersistenceSource.Contains('权威数据盘丢失')) `
    '现有 Deployment 有声明但 PVC 缺失时必须按数据丢失 fail closed'

$invokeK8sSource = $invokeK8s[0].Extent.Text
$markerReadPosition = $invokeK8sSource.IndexOf('$initialGenesisMarker = Get-LocalFreshGenesisIntent', [StringComparison]::Ordinal)
$markerValidatePosition = $invokeK8sSource.IndexOf('$initialGenesisMarkerState = Assert-LocalFreshGenesisIntent', [StringComparison]::Ordinal)
$persistencePosition = $invokeK8sSource.IndexOf('Assert-ExistingLocalEtcdPersistence -KubeContext $mkCtx', [StringComparison]::Ordinal)
Assert-True ($markerReadPosition -ge 0 -and $markerReadPosition -lt $markerValidatePosition -and
    $markerValidatePosition -lt $persistencePosition) `
    '入口必须先读取并验证 marker，再决定 persistence guard 是否可等待 Pending PVC'
Assert-True ($invokeK8sSource.Contains("-AllowPendingPvcForPreinfra:(`$initialGenesisMarkerState -ceq 'preinfra')")) `
    'Pending PVC 放宽必须且只能来源于已验证的 preinfra state'
Assert-True (-not $invokeK8sSource.Contains('-AllowPendingPvcForPreinfra:$true')) `
    'legacy/pending/complete 现场不得无条件放宽 Pending PVC'
Assert-True (-not $invokeK8sSource.Contains('delete -k $servicesDir')) `
    'Invoke-K8s -Down 禁止用 delete -k 删业务（会连 00-namespace.yaml 一起删掉 namespace，级联清空 etcd PVC）'
Assert-True ($invokeK8sSource.Contains('kubectl --context $mkProfile kustomize $servicesDir')) `
    'Invoke-K8s -Down 必须先渲染 services kustomize 再按谓词删除，保留 namespace'
Assert-True ($invokeK8sSource.Contains("([string]`$o.Kind -ceq 'Namespace') -and ([string]`$o.Name -ceq `$K8sNamespace)")) `
    'Invoke-K8s -Down 必须保留 namespace/pandora'
Assert-True ($invokeK8sSource.Contains("([string]`$o.Name -ceq 'etcd-data')")) `
    'Invoke-K8s -Down 必须保留 PVC/etcd-data'
Assert-True ($invokeK8sSource.Contains('Assert-LocalEtcdDataPreservedAfterDown -KubeContext $mkProfile')) `
    'Invoke-K8s -Down 结束前必须校验 namespace+PVC 仍在'
Assert-True (-not $invokeK8sSource.Contains('delete -f $infraYaml')) `
    'Invoke-K8s -Down 禁止再把含 etcd-data PVC 的完整 infra 清单直接删除'

Write-Host 'infra_etcd_persistence_contract_test: PASS' -ForegroundColor Green
