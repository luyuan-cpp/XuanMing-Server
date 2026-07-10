<#
.SYNOPSIS
  重置本地 minikube 开发环境中的 data_service 存储结构。

.DESCRIPTION
  仅用于开发期推翻 PlayerData schema 后的破坏性重置：
    1. 把 pandora/data-service Deployment 缩容到 0 并等待 Pod 完全退出；
    2. 只删除 Redis 中 pandora:data:player:* 缓存键；
    3. 只 DROP pandora_player.player_data 表；
    4. 默认保持 data-service 为 0，避免旧镜像立即按旧 schema 重建表；
    5. 显式传 -Restart 时恢复原副本数（原为 0 时启动 1 个），并验证新服务已重建 player_data 表。

  脚本不会执行 FLUSHALL、不会删除 pandora_player 数据库，也不会修改 players 等其它表。
  这是通用 dev reset：即使 player_data 已是新 schema，确认执行仍会清空该表和对应缓存。
  所有 kubectl 调用都固定到 -MinikubeProfile 指定的 context，不使用当前 context。
  -Restart 的重建结果必须与本仓库 PlayerData proto 的字段集合完全一致，否则重新清理并保持停服。

.EXAMPLE
  pwsh tools/scripts/reset_data_service_schema.ps1 -Mode k8s -MinikubeProfile pandora-agones -Confirm

.EXAMPLE
  # 确认当前 minikube 中已经装入新 data-service 镜像后，重置并立即恢复服务。
  pwsh tools/scripts/reset_data_service_schema.ps1 -Mode k8s -MinikubeProfile pandora-agones -Restart -Confirm

.EXAMPLE
  # 仅用于明确的非交互开发自动化；仍受 minikube/profile/resource 校验保护。
  pwsh tools/scripts/reset_data_service_schema.ps1 -Mode k8s -MinikubeProfile pandora-agones -Force
#>
[CmdletBinding()]
param(
    [ValidateSet('k8s')]
    [string]$Mode = 'k8s',

    [ValidatePattern('^[A-Za-z0-9][A-Za-z0-9._-]*$')]
    [string]$MinikubeProfile = 'pandora-agones',

    [switch]$Restart,
    [switch]$Confirm,
    [switch]$Force
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$Namespace = 'pandora'
$DataServiceDeployment = 'data-service'
$MySqlDeployment = 'mysql'
$RedisDeployment = 'redis'
$RedisKeyPattern = 'pandora:data:player:*'
$ConfirmPhrase = 'RESET-DATA-SERVICE'
$RedisDeleteBatchSize = 100

function Write-Step([string]$Message) {
    Write-Host "`n==> $Message" -ForegroundColor Cyan
}

function Invoke-NativeCapture {
    param(
        [Parameter(Mandatory)]
        [string]$FilePath,

        [Parameter(Mandatory)]
        [string[]]$ArgumentList,

        [Parameter(Mandatory)]
        [string]$Action
    )

    $output = @(& $FilePath @ArgumentList 2>&1)
    $exitCode = $LASTEXITCODE
    if ($exitCode -ne 0) {
        $detail = (($output | ForEach-Object { $_.ToString() }) -join "`n").Trim()
        if ([string]::IsNullOrWhiteSpace($detail)) {
            $detail = "exit code $exitCode"
        }
        throw "$Action 失败：$detail"
    }
    return $output
}

function Invoke-KubectlCapture {
    param(
        [Parameter(Mandatory)]
        [string[]]$ArgumentList,

        [Parameter(Mandatory)]
        [string]$Action
    )

    $kubectlArgs = @('--context', $MinikubeProfile, '--namespace', $Namespace) + $ArgumentList
    return @(Invoke-NativeCapture -FilePath 'kubectl' -ArgumentList $kubectlArgs -Action $Action)
}

function Invoke-Kubectl {
    param(
        [Parameter(Mandatory)]
        [string[]]$ArgumentList,

        [Parameter(Mandatory)]
        [string]$Action
    )

    $null = Invoke-KubectlCapture -ArgumentList $ArgumentList -Action $Action
}

function Get-DataServicePodNames {
    $output = @(Invoke-KubectlCapture `
        -ArgumentList @('get', 'pods', '--selector', 'app=data-service', '--output', 'name') `
        -Action '查询 data-service Pod')
    return @($output | ForEach-Object { $_.ToString().Trim() } | Where-Object { $_ })
}

function Wait-DataServiceStopped {
    for ($attempt = 0; $attempt -lt 30; $attempt++) {
        $pods = @(Get-DataServicePodNames)
        if ($pods.Count -eq 0) {
            return
        }
        Start-Sleep -Seconds 2
    }

    $remaining = @(Get-DataServicePodNames)
    throw "等待 data-service Pod 退出超时，仍存在：$($remaining -join ', ')"
}

function Get-PlayerCacheKeys {
    $output = @(Invoke-KubectlCapture `
        -ArgumentList @(
            'exec', "deployment/$RedisDeployment", '--container', 'redis', '--',
            'redis-cli', '--raw', '--scan', '--pattern', $RedisKeyPattern
        ) `
        -Action "扫描 Redis 键 $RedisKeyPattern")

    return @(
        $output |
            ForEach-Object { $_.ToString().TrimEnd("`r") } |
            Where-Object { -not [string]::IsNullOrWhiteSpace($_) }
    )
}

function Remove-PlayerCacheKeys {
    $keys = @(Get-PlayerCacheKeys)
    if ($keys.Count -eq 0) {
        Write-Host "  Redis：没有匹配 $RedisKeyPattern 的键。" -ForegroundColor DarkGray
        return 0
    }

    $deleted = 0
    for ($offset = 0; $offset -lt $keys.Count; $offset += $RedisDeleteBatchSize) {
        $last = [Math]::Min($offset + $RedisDeleteBatchSize - 1, $keys.Count - 1)
        [string[]]$batch = $keys[$offset..$last]
        $arguments = @(
            'exec', "deployment/$RedisDeployment", '--container', 'redis', '--',
            'redis-cli', '--raw', 'UNLINK'
        ) + $batch

        $result = @(Invoke-KubectlCapture -ArgumentList $arguments -Action '批量删除 data_service Redis 缓存')
        foreach ($line in $result) {
            $value = 0
            if ([int]::TryParse($line.ToString().Trim(), [ref]$value)) {
                $deleted += $value
            }
        }
    }

    $remaining = @(Get-PlayerCacheKeys)
    if ($remaining.Count -ne 0) {
        throw "Redis 定向清理后仍有 $($remaining.Count) 个 $RedisKeyPattern 键；data-service 保持停服。"
    }

    Write-Host "  Redis：匹配到 $($keys.Count) 个键，UNLINK 成功 $deleted 个。" -ForegroundColor Green
    return $deleted
}

function Get-PlayerDataTableCount {
    $mysqlQuery = 'MYSQL_PWD="$MYSQL_PASSWORD" mysql -u"$MYSQL_USER" --batch --skip-column-names -e "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=''pandora_player'' AND table_name=''player_data'';"'
    $output = @(Invoke-KubectlCapture `
        -ArgumentList @(
            'exec', "deployment/$MySqlDeployment", '--container', 'mysql', '--',
            'sh', '-ec', $mysqlQuery
        ) `
        -Action '查询 pandora_player.player_data 表')

    $text = (($output | ForEach-Object { $_.ToString() }) -join '').Trim()
    $count = 0
    if (-not [int]::TryParse($text, [ref]$count)) {
        throw "无法解析 player_data 表数量：'$text'"
    }
    return $count
}

function Get-PlayerDataColumns {
    $mysqlQuery = 'MYSQL_PWD="$MYSQL_PASSWORD" mysql -u"$MYSQL_USER" --batch --skip-column-names -e "SELECT column_name FROM information_schema.columns WHERE table_schema=''pandora_player'' AND table_name=''player_data'' ORDER BY ordinal_position;"'
    $output = @(Invoke-KubectlCapture `
        -ArgumentList @(
            'exec', "deployment/$MySqlDeployment", '--container', 'mysql', '--',
            'sh', '-ec', $mysqlQuery
        ) `
        -Action '查询 pandora_player.player_data 列')

    return @(
        $output |
            ForEach-Object { $_.ToString().Trim() } |
            Where-Object { -not [string]::IsNullOrWhiteSpace($_) }
    )
}

function Get-ExpectedPlayerDataColumns {
    $protoPath = Join-Path $PSScriptRoot '../../proto/pandora/data_service/v1/data_service.proto'
    if (-not (Test-Path -LiteralPath $protoPath -PathType Leaf)) {
        throw "找不到 PlayerData schema 源文件：$protoPath"
    }

    $insidePlayerData = $false
    $foundPlayerData = $false
    $columns = [System.Collections.Generic.List[string]]::new()
    foreach ($line in Get-Content -LiteralPath $protoPath) {
        $code = ($line -split '//', 2)[0]
        if (-not $insidePlayerData) {
            if ($code -match '^\s*message\s+PlayerData\s*\{') {
                $insidePlayerData = $true
                $foundPlayerData = $true
            }
            continue
        }

        if ($code -match '^\s*}') {
            break
        }

        # PlayerData 按设计只能是平铺标量字段；这里提取字段名，用于核对新镜像实际建出的列集合。
        if ($code -match '^\s*(?:(?:optional|required|repeated)\s+)?(?:map\s*<[^>]+>|[.A-Za-z_][.A-Za-z0-9_]*)\s+([A-Za-z_][A-Za-z0-9_]*)\s*=\s*\d+') {
            $columns.Add($Matches[1])
        }
    }

    if (-not $foundPlayerData -or $columns.Count -eq 0) {
        throw "无法从 $protoPath 解析 PlayerData 字段"
    }
    foreach ($requiredColumn in @('player_id', 'version')) {
        if (-not $columns.Contains($requiredColumn)) {
            throw "PlayerData proto 缺少必要字段 $requiredColumn"
        }
    }
    if ($columns.Count -ne @($columns | Select-Object -Unique).Count) {
        throw 'PlayerData proto 解析出重复字段名，拒绝继续'
    }

    return $columns.ToArray()
}

function Remove-PlayerDataTable {
    $mysqlDrop = 'MYSQL_PWD="$MYSQL_PASSWORD" mysql -u"$MYSQL_USER" -e "DROP TABLE IF EXISTS pandora_player.player_data;"'
    Invoke-Kubectl `
        -ArgumentList @(
            'exec', "deployment/$MySqlDeployment", '--container', 'mysql', '--',
            'sh', '-ec', $mysqlDrop
        ) `
        -Action 'DROP pandora_player.player_data'

    if ((Get-PlayerDataTableCount) -ne 0) {
        throw 'DROP 完成后 pandora_player.player_data 仍存在；data-service 保持停服。'
    }
    Write-Host '  MySQL：pandora_player.player_data 已删除；其它库表未修改。' -ForegroundColor Green
}

function Stop-DataServiceSafely {
    Invoke-Kubectl `
        -ArgumentList @('scale', "deployment/$DataServiceDeployment", '--replicas=0') `
        -Action '把 data-service 缩容到 0'
    Wait-DataServiceStopped
    Write-Host '  data-service Pod 已全部退出。' -ForegroundColor Green
}

function Keep-DataServiceStoppedAfterFailure([string]$Reason) {
    try {
        Invoke-Kubectl `
            -ArgumentList @('scale', "deployment/$DataServiceDeployment", '--replicas=0') `
            -Action '失败保护：保持 data-service 为 0'
        Wait-DataServiceStopped
        Write-Host "[保护] $Reason；data-service desired replicas=0 且 Pod 已全部退出。修复/装入新镜像后请重新运行本脚本，不要直接扩容。" -ForegroundColor Yellow
    }
    catch {
        Write-Host "[警告] 已请求 data-service desired replicas=0，但未确认 Pod 全部退出：$($_.Exception.Message)" -ForegroundColor Red
    }
}

foreach ($command in @('minikube', 'kubectl')) {
    if (-not (Get-Command $command -ErrorAction SilentlyContinue)) {
        Write-Host "[失败] 未找到 $command。请先安装项目 k8s 开发工具链。" -ForegroundColor Red
        exit 1
    }
}

Write-Step "校验本地 minikube profile '$MinikubeProfile'"
try {
    $expectedPlayerDataColumns = @(Get-ExpectedPlayerDataColumns)
    $statusRaw = ((Invoke-NativeCapture `
        -FilePath 'minikube' `
        -ArgumentList @('-p', $MinikubeProfile, 'status', '--output=json') `
        -Action "读取 minikube profile '$MinikubeProfile' 状态") | Out-String).Trim()
    $status = $statusRaw | ConvertFrom-Json
    if ($status.Host -ne 'Running' -or $status.APIServer -ne 'Running' -or $status.Kubeconfig -ne 'Configured') {
        throw "profile 未完全就绪：host=$($status.Host), apiserver=$($status.APIServer), kubeconfig=$($status.Kubeconfig)"
    }

    $kubeConfigRaw = ((Invoke-NativeCapture `
        -FilePath 'kubectl' `
        -ArgumentList @('config', 'view', '--raw', '--output=json') `
        -Action '读取 kubeconfig') | Out-String).Trim()
    $kubeConfig = $kubeConfigRaw | ConvertFrom-Json
    $contextEntries = @($kubeConfig.contexts | Where-Object { $_.name -eq $MinikubeProfile })
    if ($contextEntries.Count -ne 1) {
        throw "kubeconfig 中没有唯一的 context '$MinikubeProfile'"
    }
    if ($contextEntries[0].context.cluster -ne $MinikubeProfile) {
        throw "context '$MinikubeProfile' 指向 cluster '$($contextEntries[0].context.cluster)'，不是同名 minikube cluster"
    }

    Invoke-Kubectl -ArgumentList @('get', 'namespace', $Namespace) -Action "校验 namespace/$Namespace"
    Invoke-Kubectl -ArgumentList @('get', "deployment/$DataServiceDeployment") -Action '校验 deployment/data-service'
    Invoke-Kubectl -ArgumentList @('wait', '--for=condition=Available', "deployment/$MySqlDeployment", '--timeout=30s') -Action '等待 MySQL 可用'
    Invoke-Kubectl -ArgumentList @('wait', '--for=condition=Available', "deployment/$RedisDeployment", '--timeout=30s') -Action '等待 Redis 可用'

    $nodeNamesText = ((Invoke-KubectlCapture `
        -ArgumentList @('get', 'nodes', '--output', 'jsonpath={.items[*].metadata.name}') `
        -Action '读取 minikube 节点名') -join ' ').Trim()
    $nodeNames = @($nodeNamesText -split '\s+' | Where-Object { $_ })
    if ($nodeNames -notcontains $MinikubeProfile) {
        throw "cluster 节点 '$($nodeNames -join ', ')' 不包含 minikube 主节点 '$MinikubeProfile'"
    }

    $dataServiceImage = ((Invoke-KubectlCapture `
        -ArgumentList @('get', "deployment/$DataServiceDeployment", '--output', 'jsonpath={.spec.template.spec.containers[?(@.name=="data-service")].image}') `
        -Action '读取 data-service 镜像') -join '').Trim()
    if ($dataServiceImage -ne 'pandora/data-service:dev') {
        throw "data-service 镜像为 '$dataServiceImage'，不是开发镜像 pandora/data-service:dev"
    }

    $mysqlDeploymentRaw = ((Invoke-KubectlCapture `
        -ArgumentList @('get', "deployment/$MySqlDeployment", '--output', 'json') `
        -Action '读取 MySQL Deployment') | Out-String).Trim()
    $mysqlDeploymentObject = $mysqlDeploymentRaw | ConvertFrom-Json
    $dataVolumes = @($mysqlDeploymentObject.spec.template.spec.volumes | Where-Object { $_.name -eq 'data' })
    if ($dataVolumes.Count -ne 1 -or $dataVolumes[0].PSObject.Properties.Name -notcontains 'emptyDir') {
        throw 'MySQL data volume 不是预期的 emptyDir；拒绝在非 dev k8s 存储上执行重置'
    }

    $replicaText = ((Invoke-KubectlCapture `
        -ArgumentList @('get', "deployment/$DataServiceDeployment", '--output', 'jsonpath={.spec.replicas}') `
        -Action '读取 data-service 原副本数') -join '').Trim()
    $originalReplicas = 0
    if (-not [int]::TryParse($replicaText, [ref]$originalReplicas) -or $originalReplicas -lt 0) {
        throw "无法解析 data-service 原副本数：'$replicaText'"
    }
    $currentColumns = @(Get-PlayerDataColumns)
}
catch {
    Write-Host "[失败] 安全预检未通过：$($_.Exception.Message)" -ForegroundColor Red
    exit 1
}

$restartReplicas = if ($originalReplicas -gt 0) { $originalReplicas } else { 1 }
Write-Host "  目标：minikube/$MinikubeProfile，namespace/$Namespace" -ForegroundColor White
Write-Host "  将停止：deployment/$DataServiceDeployment（当前副本数：$originalReplicas）" -ForegroundColor White
Write-Host "  将删除：Redis $RedisKeyPattern" -ForegroundColor White
Write-Host '  将删除：MySQL pandora_player.player_data' -ForegroundColor White
if ($currentColumns.Count -gt 0) {
    Write-Host "  当前 player_data 列：$($currentColumns -join ', ')" -ForegroundColor DarkGray
}
else {
    Write-Host '  当前 player_data 表不存在。' -ForegroundColor DarkGray
}
Write-Host "  当前仓库 PlayerData 期望列：$($expectedPlayerDataColumns -join ', ')" -ForegroundColor DarkGray
Write-Host '  明确保留：其它 Redis 键、pandora_player 数据库、players 及其它 MySQL 表' -ForegroundColor Green
if ($Restart) {
    Write-Host "  完成后：恢复 data-service 到 $restartReplicas 个副本，并验证表已自动重建" -ForegroundColor Yellow
}
else {
    Write-Host '  完成后：data-service 保持 0 副本，等待新镜像就位' -ForegroundColor Yellow
}

if (-not $Force) {
    if (-not $Confirm) {
        Write-Host "`n[未执行] 这是不可逆开发数据重置。请显式传 -Confirm 后重试。" -ForegroundColor Yellow
        exit 2
    }

    $answer = Read-Host "输入 $ConfirmPhrase 确认执行"
    if ($answer -cne $ConfirmPhrase) {
        Write-Host '[取消] 确认短语不匹配，未执行任何写操作。' -ForegroundColor Yellow
        exit 0
    }
}

$shutdownStarted = $false
$restartAttempted = $false
try {
    Write-Step '停止 data-service'
    $shutdownStarted = $true
    Stop-DataServiceSafely

    Write-Step '删除 data_service Redis 玩家缓存'
    $null = Remove-PlayerCacheKeys

    Write-Step '删除 MySQL player_data 表'
    Remove-PlayerDataTable

    if ($Restart) {
        Write-Step '启动 1 个 data-service 副本重建并验证 schema'
        $restartAttempted = $true
        Invoke-Kubectl `
            -ArgumentList @('scale', "deployment/$DataServiceDeployment", '--replicas=1') `
            -Action '启动 data-service schema 引导副本'
        Invoke-Kubectl `
            -ArgumentList @('rollout', 'status', "deployment/$DataServiceDeployment", '--timeout=180s') `
            -Action '等待 data-service schema 引导副本 Ready'

        if ((Get-PlayerDataTableCount) -ne 1) {
            throw 'data-service Ready 后仍未重建 pandora_player.player_data。'
        }
        $rebuiltColumns = @(Get-PlayerDataColumns)
        if ($rebuiltColumns -contains 'data' -and $expectedPlayerDataColumns -notcontains 'data') {
            throw "重建后的 player_data 仍含旧 data 列；当前 profile 很可能仍在运行旧镜像。列：$($rebuiltColumns -join ', ')"
        }
        $missingColumns = @($expectedPlayerDataColumns | Where-Object { $rebuiltColumns -notcontains $_ })
        $unexpectedColumns = @($rebuiltColumns | Where-Object { $expectedPlayerDataColumns -notcontains $_ })
        if ($missingColumns.Count -ne 0 -or $unexpectedColumns.Count -ne 0) {
            throw "重建表与当前 PlayerData proto 不一致；缺少列=[$($missingColumns -join ', ')]，多余列=[$($unexpectedColumns -join ', ')]，实际列=[$($rebuiltColumns -join ', ')]"
        }

        if ($restartReplicas -ne 1) {
            Write-Step "恢复 data-service 到原来的 $restartReplicas 个副本"
            Invoke-Kubectl `
                -ArgumentList @('scale', "deployment/$DataServiceDeployment", "--replicas=$restartReplicas") `
                -Action '恢复 data-service 原副本数'
            Invoke-Kubectl `
                -ArgumentList @('rollout', 'status', "deployment/$DataServiceDeployment", '--timeout=180s') `
                -Action '等待全部 data-service 副本 Ready'
        }

        $shutdownStarted = $false
        Write-Host "`n[完成] player_data 已按当前镜像 schema 重建，data-service Ready。" -ForegroundColor Green
    }
    else {
        Write-Host "`n[完成] player_data 表与对应 Redis 缓存已清理。" -ForegroundColor Green
        Write-Host 'data-service 当前保持 0 副本；装入新镜像后请重新运行带 -Restart 的完整验收，不要直接 kubectl scale：' -ForegroundColor Yellow
        Write-Host "  pwsh tools/scripts/reset_data_service_schema.ps1 -Mode k8s -MinikubeProfile $MinikubeProfile -Restart -Confirm" -ForegroundColor White
        Write-Host "  或 tools\scripts\reset_data_service_schema_k8s.bat $MinikubeProfile restart" -ForegroundColor White
    }
}
catch {
    $message = $_.Exception.Message
    if ($restartAttempted) {
        try {
            Write-Step '失败保护：重新停止服务并恢复“无旧缓存、无 player_data 表”的干净状态'
            Stop-DataServiceSafely
            $null = Remove-PlayerCacheKeys
            Remove-PlayerDataTable
            Write-Host '[保护] 已重新清空目标缓存并删除 player_data。修复/装入新镜像后请重新运行本脚本。' -ForegroundColor Yellow
        }
        catch {
            $cleanupMessage = $_.Exception.Message
            Keep-DataServiceStoppedAfterFailure -Reason '重启验收失败，且自动回收未完整成功'
            Write-Host "[警告] 自动恢复干净状态失败：$cleanupMessage。修复后必须重新运行完整重置，不能直接扩容。" -ForegroundColor Red
        }
    }
    elseif ($shutdownStarted) {
        Keep-DataServiceStoppedAfterFailure -Reason '重置流程失败'
    }
    Write-Host "[失败] $message" -ForegroundColor Red
    exit 1
}
