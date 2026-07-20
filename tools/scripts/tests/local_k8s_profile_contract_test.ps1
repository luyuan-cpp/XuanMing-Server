$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

function Assert-True([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw $Message }
}

$ProjectRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..\..')).Path
$startPath = Join-Path $ProjectRoot 'tools\scripts\start.ps1'
$bridgePath = Join-Path $ProjectRoot 'tools\scripts\k8s_envoy_bridge.ps1'
$tokens = $null
$errors = $null
$startAst = [System.Management.Automation.Language.Parser]::ParseFile(
    $startPath, [ref]$tokens, [ref]$errors)
Assert-True (@($errors).Count -eq 0) 'start.ps1 必须可解析'

$bridgeTokens = $null
$bridgeErrors = $null
$bridgeAst = [System.Management.Automation.Language.Parser]::ParseFile(
    $bridgePath, [ref]$bridgeTokens, [ref]$bridgeErrors)
Assert-True (@($bridgeErrors).Count -eq 0) 'k8s_envoy_bridge.ps1 必须可解析'

$e2ePath = Join-Path $ProjectRoot 'tools\scripts\e2e_k8s.ps1'
$e2eTokens = $null
$e2eErrors = $null
$e2eAst = [System.Management.Automation.Language.Parser]::ParseFile(
    $e2ePath, [ref]$e2eTokens, [ref]$e2eErrors)
Assert-True (@($e2eErrors).Count -eq 0) 'e2e_k8s.ps1 必须可解析'
$e2eSource = Get-Content -LiteralPath $e2ePath -Raw

function Assert-Throws([scriptblock]$Action, [string]$Message) {
    $didThrow = $false
    try { & $Action | Out-Null } catch { $didThrow = $true }
    Assert-True $didThrow $Message
}

function Assert-DoesNotThrow([scriptblock]$Action, [string]$Message) {
    try { & $Action | Out-Null } catch { throw "$Message：$($_.Exception.Message)" }
}

foreach ($functionName in @('Get-AgonesAdvertiseHostFromText', 'Assert-RelayBindingMatchesAdvertiseHosts')) {
    $definitions = @($e2eAst.FindAll({
        param($node)
        $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and
            $node.Name -ceq $functionName
    }, $true))
    Assert-True ($definitions.Count -eq 1) "必须有唯一 $functionName"
    Invoke-Expression $definitions[0].Extent.Text
}

$allocatorYaml = @'
agones:
  enabled: true
  advertise_host: "192.168.2.28"
local_hub:
  advertise_host: "127.0.0.1"
'@
Assert-True ((Get-AgonesAdvertiseHostFromText $allocatorYaml 'test.yaml') -ceq '192.168.2.28') `
    '必须只读取 agones.advertise_host，不能误取 local_hub/local_ds'
$missingAgonesHostYaml = @'
agones:
  enabled: true
local_hub:
  advertise_host: "127.0.0.1"
'@
Assert-Throws { Get-AgonesAdvertiseHostFromText $missingAgonesHostYaml 'test.yaml' } `
    'agones 段缺少 advertise_host 时必须拒绝，不能回退到 local_hub/local_ds'
$duplicateAgonesHostYaml = @'
agones:
  advertise_host: ""
  advertise_host: "192.168.2.28"
'@
Assert-Throws { Get-AgonesAdvertiseHostFromText $duplicateAgonesHostYaml 'test.yaml' } `
    '首个值为空时也必须拒绝重复的 agones.advertise_host'

Assert-DoesNotThrow {
    Assert-RelayBindingMatchesAdvertiseHosts '127.0.0.1' '127.0.0.1' '127.0.0.1' $false
} '回环 advertise + 回环 relay 应通过'
Assert-DoesNotThrow {
    Assert-RelayBindingMatchesAdvertiseHosts '192.168.2.28' '192.168.2.28' '0.0.0.0' $true
} 'LAN advertise + 显式开放 relay 应通过'
Assert-Throws {
    Assert-RelayBindingMatchesAdvertiseHosts '192.168.2.28' '192.168.2.28' '127.0.0.1' $false
} 'LAN advertise + 默认回环 relay 必须拒绝'
Assert-Throws {
    Assert-RelayBindingMatchesAdvertiseHosts '192.168.2.28' '192.168.2.28' '0.0.0.0' $false
} 'LAN relay 即使请求 0.0.0.0 也必须确认是显式传参'
Assert-Throws {
    Assert-RelayBindingMatchesAdvertiseHosts '127.0.0.1' '127.0.0.1' '0.0.0.0' $true
} '回环 advertise 不得无必要开放 LAN relay'
Assert-Throws {
    Assert-RelayBindingMatchesAdvertiseHosts '192.168.2.28' '192.168.2.29' '0.0.0.0' $true
} 'DS/Hub allocator advertise_host 漂移必须拒绝'
Assert-Throws {
    Assert-RelayBindingMatchesAdvertiseHosts '' '192.168.2.28' '0.0.0.0' $true
} 'allocator advertise_host 缺失必须拒绝'
Assert-Throws {
    Assert-RelayBindingMatchesAdvertiseHosts 'not-an-ip' 'not-an-ip' '0.0.0.0' $true
} 'allocator advertise_host 非法必须拒绝'

Assert-True ($e2eSource.Contains("[ValidateSet('127.0.0.1', '0.0.0.0')]")) `
    'RelayBindHost 只能是两种受支持的安全绑定'
Assert-True ($e2eSource.Contains('$PSBoundParameters.ContainsKey(''RelayBindHost'')')) `
    'LAN 开放必须区分参数是否由调用方显式传入'
foreach ($contractText in @('get secret pandora-config', 'ds-allocator.yaml', 'hub-allocator.yaml')) {
    Assert-True ($e2eSource.Contains($contractText)) "live allocator guard 缺少 $contractText"
}

function Test-IsInsideFunctionDefinition($Node) {
    $cursor = $Node.Parent
    while ($null -ne $cursor) {
        if ($cursor -is [System.Management.Automation.Language.FunctionDefinitionAst]) { return $true }
        $cursor = $cursor.Parent
    }
    return $false
}

$topCommands = @($e2eAst.FindAll({
    param($node)
    $node -is [System.Management.Automation.Language.CommandAst]
}, $true) | Where-Object { -not (Test-IsInsideFunctionDefinition $_) })

function Find-TopCommand([string]$Name, [string]$Fragment = '') {
    return @($topCommands | Where-Object {
        $_.GetCommandName() -ceq $Name -and
            ([string]::IsNullOrEmpty($Fragment) -or $_.Extent.Text.Contains($Fragment))
    })
}

$initialGuards = @(Find-TopCommand 'Get-LiveAllocatorAdvertiseContract')
Assert-True ($initialGuards.Count -eq 1) '主流程必须且只能有一个初始 live allocator guard'
$guardOffset = $initialGuards[0].Extent.StartOffset
$mutations = @(
    @{ Name = 'kubectl'; Fragment = 'apply -f'; Label = 'Fleet apply' },
    @{ Name = 'minikube'; Fragment = 'image load'; Label = '节点镜像 load' },
    @{ Name = 'kubectl'; Fragment = 'delete gameservers'; Label = 'GameServer 删除' },
    @{ Name = 'Start-K8sEnvoyBridge'; Fragment = ''; Label = 'Envoy 重建' },
    @{ Name = 'Stop-HostUdpRelay'; Fragment = ''; Label = '宿主 relay 停止' },
    @{ Name = 'docker'; Fragment = 'rm -f'; Label = 'relay 容器删除' },
    @{ Name = 'docker'; Fragment = 'build -t'; Label = 'relay 镜像构建' },
    @{ Name = 'docker'; Fragment = 'run -d --name'; Label = 'relay 容器启动' }
)
foreach ($mutation in $mutations) {
    $hits = @(Find-TopCommand $mutation.Name $mutation.Fragment)
    Assert-True ($hits.Count -ge 1) "缺少受保护操作：$($mutation.Label)"
    foreach ($hit in $hits) {
        Assert-True ($guardOffset -lt $hit.Extent.StartOffset) "live allocator guard 必须早于：$($mutation.Label)"
    }
}

$bridgeCalls = @(Find-TopCommand 'Start-K8sEnvoyBridge')
$stopRelayCalls = @(Find-TopCommand 'Stop-HostUdpRelay')
$rechecks = @(Find-TopCommand 'Assert-LiveAllocatorContractUnchanged')
$dockerBuildCalls = @(Find-TopCommand 'docker' 'build -t')
$dockerRunCalls = @(Find-TopCommand 'docker' 'run -d --name')
Assert-True ($bridgeCalls.Count -eq 1 -and $stopRelayCalls.Count -eq 1) '桥接和旧 relay 停止调用必须唯一'
Assert-True ($dockerBuildCalls.Count -eq 1 -and $dockerRunCalls.Count -eq 1) 'relay build/run 调用必须唯一'
Assert-True ($rechecks.Count -ge 2) 'Secret 版本必须在桥接前和停止旧 relay 前复核'
$bridgeOffset = $bridgeCalls[0].Extent.StartOffset
$stopRelayOffset = $stopRelayCalls[0].Extent.StartOffset
$dockerBuildOffset = $dockerBuildCalls[0].Extent.StartOffset
$dockerRunOffset = $dockerRunCalls[0].Extent.StartOffset
$recheckOffsets = @($rechecks | ForEach-Object { $_.Extent.StartOffset })
Assert-True (@($recheckOffsets | Where-Object { $_ -lt $bridgeOffset }).Count -ge 1) `
    '必须在 Envoy 重建前复核 live allocator 合约'
Assert-True (@($recheckOffsets | Where-Object { $_ -gt $bridgeOffset -and $_ -lt $stopRelayOffset }).Count -ge 1) `
    '必须在停止旧 relay 前再次复核 live allocator 合约'
Assert-True (@($recheckOffsets | Where-Object { $_ -gt $dockerBuildOffset -and $_ -lt $stopRelayOffset }).Count -ge 1) `
    '必须在 relay 镜像构建完成后、停止旧 relay 前做最终 Secret 复核'
Assert-True ($stopRelayOffset -lt $dockerRunOffset) '必须在最终复核后紧邻替换 relay，缩小 TOCTOU 窗口'

$source = Get-Content -LiteralPath $startPath -Raw
$invokeK8sFunctions = @($startAst.FindAll({
    param($node)
    $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and
        $node.Name -ceq 'Invoke-K8s'
}, $true))
Assert-True ($invokeK8sFunctions.Count -eq 1) '必须有唯一 Invoke-K8s'
$mysqlRolloutCommands = @($invokeK8sFunctions[0].FindAll({
    param($node)
    $node -is [System.Management.Automation.Language.CommandAst] -and
        $node.GetCommandName() -ceq 'kubectl' -and
        $node.Extent.Text -cmatch '\brollout\s+status\s+deploy/mysql\b'
}, $true))
Assert-True ($mysqlRolloutCommands.Count -eq 1) 'Invoke-K8s 必须有唯一 MySQL rollout 等待命令'
$mysqlRolloutSource = $mysqlRolloutCommands[0].Extent.Text
Assert-True ($mysqlRolloutSource -cmatch '(?:^|\s)--timeout=1800s(?:\s|$)') `
    'MySQL 首次冷拉实际等待必须为 1800s，与 infra.yaml 的 progressDeadlineSeconds 对齐'
Assert-True (-not ($mysqlRolloutSource -cmatch '(?:^|\s)--timeout=180s(?:\s|$)')) `
    'MySQL rollout 不得退回 180s'
Assert-True ($source.Contains('MySQL 首次冷拉最多 1800s/30 分钟')) `
    '启动提示必须准确说明 MySQL 最长等待时间'
$retryCommand = 'pwsh tools/scripts/e2e_k8s.ps1 -SkipImageLoad -MinikubeProfile $mkProfile -KubeContext $mkCtx -RelayBindHost $relayBind'
Assert-True ([regex]::Matches($source, [regex]::Escape($retryCommand)).Count -ge 2) `
    'start.ps1 两处失败提示都必须保留 profile/context/bind，不能诱导回环绑定回归'
$pathRefreshFunction = $source.IndexOf('function Sync-ProcessPathFromRegistry', [StringComparison]::Ordinal)
$pathRefreshCall = $source.IndexOf("Sync-ProcessPathFromRegistry`r`n", $pathRefreshFunction + 1, [StringComparison]::Ordinal)
$prerequisiteCall = $source.LastIndexOf('$prereqOk = Resolve-Prerequisites $Mode', [StringComparison]::Ordinal)
Assert-True ($pathRefreshFunction -ge 0 -and $pathRefreshCall -gt $pathRefreshFunction) `
    'start.ps1 必须在每次运行时合并 Windows 机器/用户 PATH，兼容长期运行 web 的旧环境快照'
Assert-True ($source.Contains("[Environment]::GetEnvironmentVariable('Path', 'Machine')")) `
    'PATH 刷新必须读取 Windows 机器级 PATH'
Assert-True ($source.Contains("[Environment]::GetEnvironmentVariable('Path', 'User')")) `
    'PATH 刷新必须读取 Windows 用户级 PATH'
Assert-True ($pathRefreshCall -lt $prerequisiteCall) `
    'PATH 必须在工具前置检查前刷新'
$profileBoundLoad = "Sync-ImagesToMinikube -Images (Get-ServiceImages) -MinikubeArgs @('-p', `$mkProfile)"
Assert-True ($source.Contains($profileBoundLoad)) `
    '本地 K8s 业务镜像 load 必须显式绑定本次已校验的 minikube profile'
Assert-True (-not $source.Contains('Sync-ImagesToMinikube -Images (Get-ServiceImages)' + [Environment]::NewLine)) `
    '不得恢复依赖 active profile 的业务镜像 load 调用'

# Windows Docker Desktop 偶尔在 --force-recreate 时延迟释放 8443/8444/9901。桥接脚本必须先
# 精确核验 service/config-file 标签、按容器 ID 停本 compose 的 envoy、等待三个端口释放，
# 再启动新容器；禁止通过进程级命令杀 Docker backend。
$bridgeSource = Get-Content -LiteralPath $bridgePath -Raw
$bridgeFunctions = @($bridgeAst.FindAll({
    param($node)
    $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and
        $node.Name -ceq 'Stop-EnvoyForRecreate'
}, $true))
Assert-True ($bridgeFunctions.Count -eq 1) '必须有唯一 Stop-EnvoyForRecreate'
$stopFunction = $bridgeFunctions[0].Extent.Text
Assert-True ($stopFunction.Contains('com.docker.compose.service')) '必须核验 compose service 标签'
Assert-True ($stopFunction.Contains('com.docker.compose.project.config_files')) '必须核验 compose config_files 标签'
Assert-True ($stopFunction.Contains('[IO.Path]::GetFullPath')) 'config_files 标签必须规范化后比较'
Assert-True ($stopFunction.Contains("`$serviceLabel -cne 'envoy'")) 'service 标签不匹配必须进入拒绝条件'
Assert-True ($stopFunction.Contains('-not $actualConfigFile.Equals($expectedConfigFile, [StringComparison]::OrdinalIgnoreCase)')) `
    'config_files 规范化结果必须真正参与拒绝条件'
Assert-True ($stopFunction.Contains('throw "拒绝停止不属于当前 compose 文件的容器:')) `
    '任一归属标签不匹配都必须 fail closed'
Assert-True ($stopFunction.Contains('docker stop --time 10 $containerId')) '只能按已验证的不可变容器 ID stop'
Assert-True (-not $stopFunction.Contains('stop envoy')) '不得只凭可能跨 checkout 冲突的 service 名 stop'
Assert-True ($stopFunction.Contains('@(8443, 8444, 9901)')) '必须等待 8443/8444/9901 三个 listener 释放'
Assert-True ($stopFunction.Contains('Get-PortListenerPid')) '端口释放必须观察真实 listener'
Assert-True (-not $stopFunction.Contains('Stop-Process')) '不得为释放 Envoy 端口杀 Docker backend 进程'
Assert-True (-not $stopFunction.Contains('docker kill')) '不得用 docker kill 绕过受控停止'
Assert-True (-not $stopFunction.Contains('docker rm -f')) '不得强制删除未经完整停止的容器'
Assert-True (-not $stopFunction.Contains('taskkill')) '不得用 taskkill 杀 Docker backend'
Assert-True (-not $stopFunction.Contains('wsl --shutdown')) '不得关闭整个 WSL/Docker 运行时释放端口'
$stopCall = $bridgeSource.LastIndexOf('Stop-EnvoyForRecreate', [StringComparison]::Ordinal)
$upCall = $bridgeSource.IndexOf('up -d --force-recreate envoy', [StringComparison]::Ordinal)
Assert-True ($stopCall -ge 0 -and $upCall -gt $stopCall) '必须先等待旧 Envoy 端口释放，再 force-recreate'

Write-Host 'local_k8s_profile_contract_test: PASS' -ForegroundColor Green
