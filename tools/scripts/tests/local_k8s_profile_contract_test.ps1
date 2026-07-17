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
$null = [System.Management.Automation.Language.Parser]::ParseFile(
    $startPath, [ref]$tokens, [ref]$errors)
Assert-True (@($errors).Count -eq 0) 'start.ps1 必须可解析'

$bridgeTokens = $null
$bridgeErrors = $null
$bridgeAst = [System.Management.Automation.Language.Parser]::ParseFile(
    $bridgePath, [ref]$bridgeTokens, [ref]$bridgeErrors)
Assert-True (@($bridgeErrors).Count -eq 0) 'k8s_envoy_bridge.ps1 必须可解析'

$source = Get-Content -LiteralPath $startPath -Raw
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
