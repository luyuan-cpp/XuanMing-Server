[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$ProjectRoot = (Resolve-Path "$PSScriptRoot/../../..").Path
$InfraPath = Join-Path $ProjectRoot 'deploy/k8s/infra/infra.yaml'

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
        'Assert-LocalDsAuthBaseline -KubeContext $mkCtx -AllowFreshBootstrap:$false'
    )
    Assert-True ($apiReady -ge 0) 'Resume 必须先等待 apiserver /readyz'
    $auditPositions = @()
    foreach ($marker in $auditMarkers) {
        $position = $ResumeSource.IndexOf($marker, [StringComparison]::Ordinal)
        Assert-True ($position -ge 0) "Resume 缺最早只读审计:$marker"
        Assert-True ($position -gt $apiReady) "审计必须在 apiserver ready 之后:$marker"
        $auditPositions += $position
    }
    $firstBusinessReady = $ResumeSource.IndexOf('rollout status deploy/etcd', [StringComparison]::Ordinal)
    $firstConfigWrite = $ResumeSource.IndexOf('Apply-PandoraConfigSecret', [StringComparison]::Ordinal)
    $firstApply = $ResumeSource.IndexOf('kubectl --context $mkCtx apply -k', [StringComparison]::Ordinal)
    $firstRestart = $ResumeSource.IndexOf('kubectl --context $mkCtx rollout restart', [StringComparison]::Ordinal)
    foreach ($barrier in @($firstBusinessReady, $firstConfigWrite, $firstApply, $firstRestart)) {
        Assert-True ($barrier -ge 0) 'Resume 顺序测试缺业务等待/写入 marker'
        foreach ($auditPosition in $auditPositions) {
            Assert-True ($auditPosition -lt $barrier) '所有只读审计必须早于业务 Ready、apply 与 rollout'
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

$manifest = Get-Content -LiteralPath $InfraPath -Raw
Assert-EtcdPersistenceContract $manifest
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
Assert-True ($resume.Count -eq 1) '必须有唯一 Resume-K8s'
Assert-True ($apiWait.Count -eq 1) '必须有唯一 Wait-KubeApiServerReady'
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

Write-Host 'infra_etcd_persistence_contract_test: PASS' -ForegroundColor Green
