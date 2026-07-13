<#
.SYNOPSIS
  重置本地开发环境中的 data_service 存储结构(支持 k8s minikube 与 docker compose 两套本地栈)。

.DESCRIPTION
  仅用于开发期推翻 PlayerData schema 后的破坏性重置:
    1. 停止 data-service(k8s 缩容到 0 / compose 停容器),等其退出;
    2. 只删除 Redis 中 pandora:data:player:* 缓存键;
    3. 只 DROP pandora_player.player_data 表;
    4. 默认保持 data-service 停止,避免旧镜像立即按旧 schema 重建表;
    5. 显式传 -Restart 时恢复 data-service,并验证新服务已按当前 PlayerData proto 重建 player_data 表。

  脚本不会执行 FLUSHALL、不会删除 pandora_player 数据库,也不会修改 players 等其它表。
  这是通用 dev reset:即使 player_data 已是新 schema,确认执行仍会清空该表和对应缓存。

  两种 -Mode:
    k8s      本地 minikube(默认)。所有 kubectl 调用固定到 -MinikubeProfile 指定的 context,不使用当前 context。
    compose  本地 docker compose 栈(-Mode docker/intranet/battle/local 用的 pandora-mysql / pandora-redis /
             pandora-data-service 容器)。这是修复「compose/local 旧 `data BLOB NOT NULL` 表」的路径。
              data-service 容器不存在或仅留下 stopped 容器时,都无法排除仍有宿主进程运行；脚本 fail-closed:
              必须先自行停掉宿主进程,再显式传 -HostDataServiceStopped 才会继续。
              compose 模式显式传 -Restart 时,统一由 compose 启动或创建 data-service,不会因当前无容器而静默忽略。

  -Restart 的重建结果必须与本仓库 PlayerData proto 的字段集合完全一致,否则重新清理并保持停服。

.EXAMPLE
  pwsh tools/scripts/reset_data_service_schema.ps1 -Mode k8s -MinikubeProfile pandora-agones -Confirm

.EXAMPLE
  # 确认当前 minikube 中已经装入新 data-service 镜像后,重置并立即恢复服务。
  pwsh tools/scripts/reset_data_service_schema.ps1 -Mode k8s -MinikubeProfile pandora-agones -Restart -Confirm

.EXAMPLE
  # docker compose 本地栈:清掉旧 data BLOB 表 + 缓存,并用新镜像重建校验。
  pwsh tools/scripts/reset_data_service_schema.ps1 -Mode compose -Restart -Confirm

.EXAMPLE
  # 仅用于明确的非交互开发自动化;仍受 profile/资源校验保护。
  pwsh tools/scripts/reset_data_service_schema.ps1 -Mode k8s -MinikubeProfile pandora-agones -Force
#>
[CmdletBinding()]
param(
    [ValidateSet('k8s', 'compose')]
    [string]$Mode = 'k8s',

    [ValidatePattern('^[A-Za-z0-9][A-Za-z0-9._-]*$')]
    [string]$MinikubeProfile = 'pandora-agones',

    [switch]$Restart,
    [switch]$Confirm,
    [switch]$Force,

    # compose 模式下 data-service 容器 absent/stopped 时,必须显式声明已停掉可能存在的宿主进程。
    # stopped 容器不能证明宿主进程也已停；未传本开关直接拒绝,不做任何破坏性清理。
    [switch]$HostDataServiceStopped
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

# 规范化文件系统路径,便于跨来源(docker 标签 vs PowerShell)做 worktree 归属比较:
# 统一分隔符为 `\`、去尾部分隔符;不解析 symlink(docker 标签存的是原始路径,解析反而会漂移)。
# 调用方用 -ine 做 Windows 大小写不敏感比较。
function ConvertTo-CanonicalPath([string]$p) {
    if ([string]::IsNullOrWhiteSpace($p)) { return '' }
    $n = $p.Trim().Replace('/', '\')
    return [System.IO.Path]::TrimEndingDirectorySeparator($n)
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

# ===== compose 模式:docker compose 本地栈的 data_service 重置 =====
# 与 k8s 分支等价的破坏性重置,但作用于 pandora-mysql / pandora-redis / pandora-data-service 容器。
# 这是修复「-Mode docker/intranet/battle/local 用的旧 `data BLOB NOT NULL` 表」的路径。
function Invoke-ComposeReset {
    $projectRoot   = (Resolve-Path (Join-Path $PSScriptRoot '../..')).Path
    $composeSvcs   = Join-Path $projectRoot 'deploy/docker-compose.services.yml'
    $composeInfra  = Join-Path $projectRoot 'deploy/docker-compose.dev.yml'
    $envFile       = Join-Path $projectRoot 'deploy/env/dev.env'
    $mysqlContainer = 'pandora-mysql'
    $redisContainer = 'pandora-redis'
    $dataContainer  = 'pandora-data-service'

    # 本 worktree 的 compose 工作目录(= 编排文件所在的 deploy 目录绝对路径)与两个编排文件绝对路径。
    # start.ps1 / dev_up.ps1 都用 `docker compose -f <root>/deploy/...` 不带 -p:
    #   - 基础设施(pandora-mysql / pandora-redis)由 docker-compose.dev.yml 起 → config_files=dev.yml;
    #   - data-service 由 docker-compose.services.yml 起 → config_files=services.yml。
    # 不带 -p 时 compose 的默认 project 名 = 编排文件所在目录的基名(sanitize 后),此处即 'deploy'。
    # 破坏性清理前必须【正向证明】容器属于本 worktree,采用 fail-closed:
    #   ① com.docker.compose.project == 本 worktree 默认 project 名(封堵 `-p other` 复用同名容器);且
    #   ② com.docker.compose.project.config_files 含本 worktree 的 dev.yml 或 services.yml 绝对路径
    #      (worktree 专属路径,天然区分另一 checkout;同时修掉旧版仅认 services.yml 致 mysql/redis
    #       只能靠 working_dir 兜底的漏洞——同目录自定义 compose 会被误认)。
    # 两条件缺一即拒(不再用 working_dir OR config_files 放行)。compose v2 必写这些标签。
    $expectedComposeWorkingDir = ConvertTo-CanonicalPath (Split-Path $composeSvcs -Parent)
    $expectedComposeFiles      = @(
        (ConvertTo-CanonicalPath $composeSvcs),
        (ConvertTo-CanonicalPath $composeInfra)
    )
    $expectedComposeProject    = ((Split-Path $expectedComposeWorkingDir -Leaf).ToLowerInvariant() -replace '[^a-z0-9_-]', '')
    if ([string]::IsNullOrWhiteSpace($expectedComposeProject)) {
        throw "无法从 compose 工作目录 '$expectedComposeWorkingDir' 推导预期 project 名。"
    }

    if (-not (Get-Command 'docker' -ErrorAction SilentlyContinue)) {
        throw '未找到 docker。compose 模式需要 Docker Desktop 并已启动。'
    }

    function Invoke-DockerCapture([string[]]$ArgumentList, [string]$Action) {
        return @(Invoke-NativeCapture -FilePath 'docker' -ArgumentList $ArgumentList -Action $Action)
    }
    # 返回 Absent / Running / ActiveNonRunning / Stopped。Docker 查询失败、非法 JSON、
    # dead/removing/未知状态一律 throw，绝不能把“无法确认”折叠成 false(已停止)。
    function Get-ContainerState([string]$name, [string]$expectedComposeService) {
        $found = @(Invoke-DockerCapture `
            -ArgumentList @('container', 'ls', '-a', '--filter', "name=^/$name$", '--format', '{{.Names}}') `
            -Action "查询容器 $name 是否存在" |
            ForEach-Object { $_.ToString().Trim() } | Where-Object { $_ })
        if ($found.Count -eq 0) { return 'Absent' }
        if ($found.Count -ne 1 -or $found[0] -cne $name) {
            throw "容器名称查询结果异常(name=$name,result=[$($found -join ', ')])。"
        }

        $inspectRaw = @(Invoke-DockerCapture -ArgumentList @('container', 'inspect', $name) -Action "检查容器 $name 状态")
        $inspectText = (($inspectRaw | ForEach-Object { $_.ToString() }) -join "`n").Trim()
        try { $inspect = @($inspectText | ConvertFrom-Json -ErrorAction Stop) }
        catch { throw "无法解析容器 $name 的 inspect JSON:$($_.Exception.Message)" }
        if ($inspect.Count -ne 1) { throw "容器 $name 的 inspect 结果数量异常:$($inspect.Count)" }
        $container = $inspect[0]
        if ([string]$container.Name -cne "/$name") { throw "inspect 返回了非目标容器:$($container.Name)" }

        $labels = $container.Config.Labels
        if (-not [string]::IsNullOrWhiteSpace($expectedComposeService)) {
            $labelProperty = if ($null -eq $labels) { $null } else { $labels.PSObject.Properties['com.docker.compose.service'] }
            $actualService = if ($null -eq $labelProperty) { '' } else { [string]$labelProperty.Value }
            if ($actualService -cne $expectedComposeService) {
                throw "同名容器 $name 不属于预期 compose service '$expectedComposeService'(实际='$actualService'),拒绝操作。"
            }
        }

        # worktree 归属校验(三审 P1-6/P1-7/P1-15):破坏性清理前必须【正向证明】容器属于本 worktree。
        # fail-closed,两条件同时成立才放行(缺一即拒):
        #   ① com.docker.compose.project == 本 worktree 默认 project 名(封堵 `-p other` 复用同名容器);
        #   ② com.docker.compose.project.config_files 含本 worktree 的 dev.yml 或 services.yml 绝对路径。
        # 兼记 working_dir 供报错定位。旧版用 working_dir OR config_files(且仅认 services.yml),
        # mysql/redis(dev.yml 起)只能靠 working_dir 兜底、且不校验 project 名 → 现予收紧。
        $projProperty = if ($null -eq $labels) { $null } else { $labels.PSObject.Properties['com.docker.compose.project'] }
        $actualProject = if ($null -eq $projProperty) { '' } else { [string]$projProperty.Value }
        $projectMatched = (-not [string]::IsNullOrWhiteSpace($actualProject)) -and ($actualProject -ceq $expectedComposeProject)

        $wdProperty = if ($null -eq $labels) { $null } else { $labels.PSObject.Properties['com.docker.compose.project.working_dir'] }
        $actualWorkingDir = if ($null -eq $wdProperty) { '' } else { ConvertTo-CanonicalPath ([string]$wdProperty.Value) }

        $cfProperty = if ($null -eq $labels) { $null } else { $labels.PSObject.Properties['com.docker.compose.project.config_files'] }
        $actualConfigFilesRaw = if ($null -eq $cfProperty) { '' } else { [string]$cfProperty.Value }
        $configFileMatched = $false
        if (-not [string]::IsNullOrWhiteSpace($actualConfigFilesRaw)) {
            foreach ($cf in ($actualConfigFilesRaw -split ',')) {
                $cfCanon = ConvertTo-CanonicalPath $cf
                foreach ($expected in $expectedComposeFiles) {
                    if ($cfCanon -ieq $expected) { $configFileMatched = $true; break }
                }
                if ($configFileMatched) { break }
            }
        }

        if (-not ($projectMatched -and $configFileMatched)) {
            throw ("拒绝破坏性清理:同名容器 $name 无法证明属于本 worktree。" +
                "期望 project='$expectedComposeProject' 且 config_files 含本 worktree 的 dev.yml/services.yml;" +
                "实际 project='$actualProject',working_dir='$actualWorkingDir',config_files='$actualConfigFilesRaw'。" +
                "该容器可能属于另一 checkout/worktree、以 `-p` 自定义 project 名或为手工创建,请人工确认后再处理。")
        }

        switch ([string]$container.State.Status) {
            'running' { return 'Running' }
            { $_ -in @('paused', 'restarting') } { return 'ActiveNonRunning' }
            { $_ -in @('created', 'exited') } { return 'Stopped' }
            default { throw "容器 $name 处于无法安全判定的状态 '$($container.State.Status)'(dead/removing/未知均需人工处理)。" }
        }
    }
    function Stop-DataContainerAndVerify {
        $state = Get-ContainerState $dataContainer 'data-service'
        if ($state -in @('Running', 'ActiveNonRunning')) {
            $null = Invoke-DockerCapture -ArgumentList @('stop', $dataContainer) -Action "停止容器 $dataContainer"
        }
        $deadline = (Get-Date).AddSeconds(30)
        do {
            $state = Get-ContainerState $dataContainer 'data-service'
            if ($state -in @('Stopped', 'Absent')) { return $state }
            if ((Get-Date) -ge $deadline) { throw "30s 内未确认容器 $dataContainer 已停止(当前=$state)。" }
            Start-Sleep -Seconds 1
        } while ($true)
    }
    function Assert-DataContainerIsolated {
        $state = Get-ContainerState $dataContainer 'data-service'
        if ($state -in @('Running', 'ActiveNonRunning')) { throw "容器 $dataContainer 在破坏性清理前又变为 $state,已中止。" }
        return $state
    }
    # 通过 mysql 容器内 shell 展开 MYSQL_USER/MYSQL_PASSWORD(与 k8s 分支同款,凭据不进 PowerShell)。
    function Invoke-ComposeMysql([string]$Sql, [string]$Action) {
        $cmd = 'MYSQL_PWD="$MYSQL_PASSWORD" mysql -u"$MYSQL_USER" --batch --skip-column-names -e ' + ('"' + ($Sql -replace '"', '\"') + '"')
        return @(Invoke-DockerCapture -ArgumentList @('exec', $mysqlContainer, 'sh', '-ec', $cmd) -Action $Action)
    }
    function Get-ComposePlayerDataColumns {
        $out = Invoke-ComposeMysql "SELECT column_name FROM information_schema.columns WHERE table_schema='pandora_player' AND table_name='player_data' ORDER BY ordinal_position;" '查询 player_data 列'
        return @($out | ForEach-Object { $_.ToString().Trim() } | Where-Object { $_ })
    }
    function Get-ComposePlayerDataTableCount {
        $out = Invoke-ComposeMysql "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='pandora_player' AND table_name='player_data';" '查询 player_data 表是否存在'
        $text = (($out | ForEach-Object { $_.ToString() }) -join '').Trim()
        $count = 0
        if (-not [int]::TryParse($text, [ref]$count)) { throw "无法解析 player_data 表数量:'$text'" }
        return $count
    }
    function Remove-ComposePlayerCache {
        $keys = @(Invoke-DockerCapture -ArgumentList @('exec', $redisContainer, 'redis-cli', '--raw', '--scan', '--pattern', $RedisKeyPattern) -Action "扫描 Redis 键 $RedisKeyPattern" |
            ForEach-Object { $_.ToString().Trim() } | Where-Object { $_ })
        if ($keys.Count -eq 0) {
            Write-Host "  Redis:没有匹配 $RedisKeyPattern 的键。" -ForegroundColor DarkGray
            return
        }
        for ($offset = 0; $offset -lt $keys.Count; $offset += $RedisDeleteBatchSize) {
            $last = [Math]::Min($offset + $RedisDeleteBatchSize - 1, $keys.Count - 1)
            [string[]]$batch = $keys[$offset..$last]
            $null = Invoke-DockerCapture -ArgumentList (@('exec', $redisContainer, 'redis-cli', '--raw', 'UNLINK') + $batch) -Action '批量删除 data_service Redis 缓存'
        }
        $remaining = @(Invoke-DockerCapture -ArgumentList @('exec', $redisContainer, 'redis-cli', '--raw', '--scan', '--pattern', $RedisKeyPattern) -Action "复查 Redis 键 $RedisKeyPattern" |
            ForEach-Object { $_.ToString().Trim() } | Where-Object { $_ })
        if ($remaining.Count -ne 0) { throw "Redis 定向清理后仍有 $($remaining.Count) 个 $RedisKeyPattern 键。" }
        Write-Host "  Redis:匹配到 $($keys.Count) 个键,已 UNLINK。" -ForegroundColor Green
    }
    function Remove-ComposePlayerDataTable {
        $null = Invoke-ComposeMysql 'DROP TABLE IF EXISTS pandora_player.player_data;' 'DROP pandora_player.player_data'
        if ((Get-ComposePlayerDataTableCount) -ne 0) { throw 'DROP 完成后 pandora_player.player_data 仍存在。' }
        Write-Host '  MySQL:pandora_player.player_data 已删除;其它库表未修改。' -ForegroundColor Green
    }

    $expectedPlayerDataColumns = @(Get-ExpectedPlayerDataColumns)

    Write-Step 'compose 模式:校验基础设施容器'
    $mysqlState = Get-ContainerState $mysqlContainer 'mysql'
    if ($mysqlState -ne 'Running') {
        throw "MySQL 容器 $mysqlContainer 未明确运行(状态=$mysqlState)。先起本地栈:pwsh tools/scripts/start.ps1 -Mode docker"
    }
    $redisState = Get-ContainerState $redisContainer 'redis'
    if ($redisState -ne 'Running') {
        throw "Redis 容器 $redisContainer 未明确运行(状态=$redisState)。先起本地栈:pwsh tools/scripts/start.ps1 -Mode docker"
    }
    $dataInitialState = Get-ContainerState $dataContainer 'data-service'
    $currentColumns = @(Get-ComposePlayerDataColumns)

    Write-Host "  目标:docker compose 本地栈(容器 $mysqlContainer / $redisContainer)" -ForegroundColor White
    if ($dataInitialState -in @('Running', 'ActiveNonRunning')) {
        Write-Host "  将停止并复查:容器 $dataContainer" -ForegroundColor White
    } else {
        if ($dataInitialState -eq 'Stopped') {
            Write-Host "  检测到 stopped compose 容器 $dataContainer；它不能证明宿主 data-service 已停。" -ForegroundColor Yellow
        } else {
            Write-Host "  data-service 无容器(疑似 -Mode local 宿主进程)。" -ForegroundColor Yellow
        }
        Write-Host "  ⚠️ 请先自行停掉宿主 data-service(pwsh tools/scripts/run_services.ps1 -Action down 或结束对应进程)," -ForegroundColor Yellow
        Write-Host "     否则它会在 DROP 后按旧 schema 立刻重建表,重置无效。" -ForegroundColor Yellow
        if (-not $HostDataServiceStopped) {
            throw "data-service 容器状态为 $dataInitialState,无法排除宿主进程仍在运行。请先停掉宿主进程," +
                  "再加 -HostDataServiceStopped 重跑;否则拒绝破坏性清理。"
        }
        Write-Host "  已确认宿主 data-service 已停止(-HostDataServiceStopped)。" -ForegroundColor Green
    }
    Write-Host "  将删除:Redis $RedisKeyPattern" -ForegroundColor White
    Write-Host '  将删除:MySQL pandora_player.player_data' -ForegroundColor White
    if ($currentColumns.Count -gt 0) { Write-Host "  当前 player_data 列:$($currentColumns -join ', ')" -ForegroundColor DarkGray }
    else { Write-Host '  当前 player_data 表不存在。' -ForegroundColor DarkGray }
    Write-Host "  当前仓库 PlayerData 期望列:$($expectedPlayerDataColumns -join ', ')" -ForegroundColor DarkGray
    Write-Host '  明确保留:其它 Redis 键、pandora_player 数据库、players 及其它 MySQL 表' -ForegroundColor Green
    $composeArgs = @()
    if ($Restart) {
        if (-not (Test-Path -LiteralPath $composeSvcs -PathType Leaf)) { throw "找不到 compose 文件:$composeSvcs" }
        $composeArgs = @('compose', '-f', $composeSvcs)
        if (Test-Path -LiteralPath $envFile -PathType Leaf) { $composeArgs += @('--env-file', $envFile) }
        $composeServices = @(Invoke-DockerCapture -ArgumentList ($composeArgs + @('config', '--services')) -Action '校验 compose 服务清单' |
            ForEach-Object { $_.ToString().Trim() } | Where-Object { $_ })
        if ($composeServices -cnotcontains 'data-service') {
            throw 'compose 服务清单不含 data-service,拒绝先清理后无法恢复。'
        }
        Write-Host '  完成后:通过 compose 启动/创建 data-service 并验证表已自动重建' -ForegroundColor Yellow
    } else {
        Write-Host '  完成后:data-service 保持停止,等待新镜像/新二进制就位' -ForegroundColor Yellow
    }

    if (-not $Force) {
        if (-not $Confirm) {
            Write-Host "`n[未执行] 这是不可逆开发数据重置。请显式传 -Confirm 后重试。" -ForegroundColor Yellow
            exit 2
        }
        $answer = Read-Host "输入 $ConfirmPhrase 确认执行"
        if ($answer -cne $ConfirmPhrase) {
            Write-Host '[取消] 确认短语不匹配,未执行任何写操作。' -ForegroundColor Yellow
            exit 0
        }
    }

    Write-Step '停止 data-service 容器并确认隔离'
    $isolatedState = Stop-DataContainerAndVerify
    Write-Host "  data-service 已明确隔离(容器状态=$isolatedState)。" -ForegroundColor Green

    Write-Step '删除 data_service Redis 玩家缓存'
    $null = Assert-DataContainerIsolated
    Remove-ComposePlayerCache

    Write-Step '删除 MySQL player_data 表'
    $null = Assert-DataContainerIsolated
    Remove-ComposePlayerDataTable

    if ($Restart) {
        Write-Step '重启 data-service 容器并验证 schema'
        try {
            $null = Invoke-DockerCapture -ArgumentList ($composeArgs + @('up', '-d', 'data-service')) -Action '启动 data-service 容器'

            $deadline = (Get-Date).AddSeconds(180)
            $count = 0
            $lastTableCheckError = ''
            while ((Get-Date) -lt $deadline) {
                if ((Get-ContainerState $dataContainer 'data-service') -ne 'Running') { Start-Sleep -Seconds 2; continue }
                try {
                    $count = Get-ComposePlayerDataTableCount
                    $lastTableCheckError = ''
                } catch {
                    $count = 0
                    $lastTableCheckError = $_.Exception.Message
                }
                if ($count -eq 1) { break }
                Start-Sleep -Seconds 2
            }
            if ($count -ne 1) {
                $detail = if ([string]::IsNullOrWhiteSpace($lastTableCheckError)) { '' } else { "最后一次查询错误:$lastTableCheckError" }
                throw "data-service 重启后 180s 内仍未重建 pandora_player.player_data。$detail"
            }

            $rebuiltColumns = @(Get-ComposePlayerDataColumns)
            if ($rebuiltColumns -contains 'data' -and $expectedPlayerDataColumns -notcontains 'data') {
                throw "重建后的 player_data 仍含旧 data 列;当前很可能仍在运行旧镜像。列:$($rebuiltColumns -join ', ')"
            }
            $missingColumns = @($expectedPlayerDataColumns | Where-Object { $rebuiltColumns -notcontains $_ })
            $unexpectedColumns = @($rebuiltColumns | Where-Object { $expectedPlayerDataColumns -notcontains $_ })
            if ($missingColumns.Count -ne 0 -or $unexpectedColumns.Count -ne 0) {
                throw "重建表与当前 PlayerData proto 不一致;缺少列=[$($missingColumns -join ', ')],多余列=[$($unexpectedColumns -join ', ')],实际列=[$($rebuiltColumns -join ', ')]"
            }
            $finalState = Get-ContainerState $dataContainer 'data-service'
            if ($finalState -ne 'Running') {
                throw "schema 虽已重建,但 data-service 最终状态不是 Running(当前=$finalState)。"
            }
            Write-Host "`n[完成] player_data 已按当前镜像 schema 重建,data-service 容器运行中。" -ForegroundColor Green
        }
        catch {
            $originalError = $_.Exception.Message
            Write-Host "  [fail-closed] 重启/验证失败;将先确认停服,确认后重新清缓存和删表。" -ForegroundColor Yellow
            $protectionErrors = [System.Collections.Generic.List[string]]::new()
            $isolated = $false
            try {
                $protectedState = Stop-DataContainerAndVerify
                $isolated = $true
                Write-Host "  已确认 data-service 隔离(容器状态=$protectedState),开始保护性重清。" -ForegroundColor Yellow
            } catch {
                $protectionErrors.Add("停服确认失败:$($_.Exception.Message)")
            }
            if ($isolated) {
                $stillIsolated = $true
                try { $null = Assert-DataContainerIsolated }
                catch {
                    $stillIsolated = $false
                    $protectionErrors.Add("Redis 重清前停服复查失败:$($_.Exception.Message)")
                }
                if ($stillIsolated) {
                    try { Remove-ComposePlayerCache }
                    catch { $protectionErrors.Add("Redis 保护性重清失败:$($_.Exception.Message)") }
                    try {
                        $null = Assert-DataContainerIsolated
                        Remove-ComposePlayerDataTable
                    } catch { $protectionErrors.Add("MySQL 重清/停服复查失败:$($_.Exception.Message)") }
                }
            }
            if ($protectionErrors.Count -ne 0) {
                throw "data-service 重启/验证失败:$originalError;fail-closed 保护未完整完成:$($protectionErrors -join '; ')。" +
                      "若停服状态未知,禁止继续清理,需人工确认容器/宿主进程。"
            }
            throw "data-service 重启/验证失败:$originalError;已确认停服并重新清空对应 Redis 缓存与 player_data 表。"
        }
    }
    else {
        Write-Host "`n[完成] player_data 表与对应 Redis 缓存已清理。" -ForegroundColor Green
        Write-Host 'data-service 当前保持停止;装入新镜像/新二进制后再启动(或带 -Restart 重跑校验)。' -ForegroundColor Yellow
    }
}

if ($Mode -eq 'compose') {
    try {
        Invoke-ComposeReset
        exit 0
    }
    catch {
        Write-Host "[失败] compose 重置未完成:$($_.Exception.Message)" -ForegroundColor Red
        exit 1
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
