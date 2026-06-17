# Pandora 业务服务一键启停 / 单服务调试
#
# 大厂本地多服务开发的"进程编排"层(等价 Procfile / goreman / tilt,但零额外依赖)。
# 基础设施(MySQL/Redis/Kafka/etcd/Envoy)由 dev_up.ps1 负责,本脚本只管 Go 业务服务。
#
# 用法:
#   # 起"登录 + 组队"测试需要的最小服务集(UE 测登录/组队用这个)
#   pwsh tools/scripts/run_services.ps1
#
#   # 起完整主链路(登录→组队→匹配→拉DS→结算)全部 9 个服务
#   pwsh tools/scripts/run_services.ps1 -Profile match
#
#   # 起全部服务
#   pwsh tools/scripts/run_services.ps1 -Profile all
#
#   # 起 profile 里除 team 外的全部服务(team 留给 VS Code 断点调试)
#   pwsh tools/scripts/run_services.ps1 -Exclude team
#
#   # 查看状态 / 看日志 / 重启单个 / 全停
#   pwsh tools/scripts/run_services.ps1 -Action status
#   pwsh tools/scripts/run_services.ps1 -Action logs    -Service team
#   pwsh tools/scripts/run_services.ps1 -Action restart -Service team
#   pwsh tools/scripts/run_services.ps1 -Action down
#
#   # 单个服务前台运行(快速看完整日志,不进 IDE;Ctrl+C 结束)
#   pwsh tools/scripts/run_services.ps1 -Service team -Foreground

[CmdletBinding()]
param(
    [ValidateSet('login', 'match', 'all')]
    [string]$Profile = 'login',

    [ValidateSet('up', 'down', 'status', 'logs', 'restart', 'build')]
    [string]$Action = 'up',

    # 起 profile 时排除的服务(留给 IDE 调试);也可配合 restart/logs/foreground 指定单个服务
    [string[]]$Exclude = @(),

    # 指定单个服务(logs / restart / -Foreground 时使用)
    [string]$Service,

    # 单服务前台运行(阻塞,Ctrl+C 退出),方便直接看日志
    [switch]$Foreground,

    # 跳过 go build(进程已是最新二进制时加速)
    [switch]$NoBuild
)

$ErrorActionPreference = 'Stop'

$ProjectRoot = (Resolve-Path "$PSScriptRoot/../..").Path
$RunDir = Join-Path $ProjectRoot 'run/dev'
$BinDir = Join-Path $RunDir 'bin'
$LogDir = Join-Path $RunDir 'logs'
New-Item -ItemType Directory -Force -Path $BinDir, $LogDir | Out-Null

# ===== 服务清单(数组顺序 = 依赖启动顺序:leaf 依赖在前,login 最后)=====
# Profiles:
#   login = 测登录/组队最小集(player_locator + hub_allocator + push + team + login)
#   match = 完整主链路(在 login 基础上 + player + ds_allocator + battle_result + matchmaker)
#   all   = 全部 15 个服务(含 social/friend、social/chat、social/dialogue、data/data_service、economy/trade、economy/inventory)
$Services = @(
    @{ Name = 'player_locator'; Dir = 'services/runtime/player_locator';   Cmd = 'locator';        Conf = 'etc/locator-dev.yaml';        Port = 50006; Profiles = @('login', 'match', 'all') }
    @{ Name = 'hub_allocator';  Dir = 'services/battle/hub_allocator';      Cmd = 'hub_allocator';  Conf = 'etc/hub_allocator-dev.yaml';  Port = 50021; Profiles = @('login', 'match', 'all') }
    @{ Name = 'player';         Dir = 'services/account/player';            Cmd = 'player';         Conf = 'etc/player-dev.yaml';         Port = 50002; Profiles = @('match', 'all') }
    @{ Name = 'ds_allocator';   Dir = 'services/battle/ds_allocator';       Cmd = 'ds_allocator';   Conf = 'etc/ds_allocator-dev.yaml';   Port = 50020; Profiles = @('match', 'all') }
    @{ Name = 'push';           Dir = 'services/runtime/push';              Cmd = 'push';           Conf = 'etc/push-dev.yaml';           Port = 50014; Profiles = @('login', 'match', 'all') }
    @{ Name = 'team';           Dir = 'services/matchmaking/team';          Cmd = 'team';           Conf = 'etc/team-dev.yaml';           Port = 50010; Profiles = @('login', 'match', 'all') }
    @{ Name = 'friend';         Dir = 'services/social/friend';             Cmd = 'friend';         Conf = 'etc/friend-dev.yaml';         Port = 50004; Profiles = @('all') }
    @{ Name = 'chat';           Dir = 'services/social/chat';               Cmd = 'chat';           Conf = 'etc/chat-dev.yaml';           Port = 50005; Profiles = @('all') }
    @{ Name = 'dialogue';       Dir = 'services/social/dialogue';           Cmd = 'dialogue';       Conf = 'etc/dialogue-dev.yaml';       Port = 50013; Profiles = @('all') }
    @{ Name = 'data_service';   Dir = 'services/data/data_service';         Cmd = 'data_service';   Conf = 'etc/data_service-dev.yaml';   Port = 50003; Profiles = @('all') }
    @{ Name = 'trade';          Dir = 'services/economy/trade';             Cmd = 'trade';          Conf = 'etc/trade-dev.yaml';          Port = 50012; Profiles = @('all') }
    @{ Name = 'inventory';      Dir = 'services/economy/inventory';         Cmd = 'inventory';      Conf = 'etc/inventory-dev.yaml';      Port = 50015; Profiles = @('all') }
    @{ Name = 'battle_result';  Dir = 'services/battle/battle_result';      Cmd = 'battle_result';  Conf = 'etc/battle_result-dev.yaml';  Port = 50022; Profiles = @('match', 'all') }
    @{ Name = 'matchmaker';     Dir = 'services/matchmaking/matchmaker';    Cmd = 'matchmaker';     Conf = 'etc/matchmaker-dev.yaml';     Port = 50011; Profiles = @('match', 'all') }
    @{ Name = 'login';          Dir = 'services/account/login';             Cmd = 'login';          Conf = 'etc/login-dev.yaml';          Port = 50001; Profiles = @('login', 'match', 'all') }
)

function Get-Service([string]$name) {
    $svc = $Services | Where-Object { $_.Name -eq $name }
    if (-not $svc) {
        Write-Host "[ERR] 未知服务: $name" -ForegroundColor Red
        Write-Host "可用服务: $(( $Services | ForEach-Object { $_.Name }) -join ', ')" -ForegroundColor Yellow
        exit 1
    }
    return $svc
}

function Get-ProfileServices {
    $Services | Where-Object { $_.Profiles -contains $Profile -and $Exclude -notcontains $_.Name }
}

function Get-PidFile($svc) { Join-Path $LogDir "$($svc.Name).pid" }
function Get-LogFile($svc) { Join-Path $LogDir "$($svc.Name).log" }
function Get-ErrFile($svc) { Join-Path $LogDir "$($svc.Name).err.log" }

function Get-RunningProcess($svc) {
    $pidFile = Get-PidFile $svc
    if (-not (Test-Path $pidFile)) { return $null }
    $svcPid = (Get-Content $pidFile -Raw).Trim()
    if (-not $svcPid) { return $null }
    $proc = Get-Process -Id $svcPid -ErrorAction SilentlyContinue
    if (-not $proc) {
        Remove-Item $pidFile -Force -ErrorAction SilentlyContinue
        return $null
    }

    $expectedExe = Join-Path $BinDir "$($svc.Name).exe"
    $actualExe = $null
    try { $actualExe = $proc.Path } catch { $actualExe = $null }
    if ($actualExe -and ([System.IO.Path]::GetFullPath($actualExe) -ne [System.IO.Path]::GetFullPath($expectedExe))) {
        Remove-Item $pidFile -Force -ErrorAction SilentlyContinue
        return $null
    }

    if (-not $actualExe -and $proc.ProcessName -ne $svc.Name) {
        Remove-Item $pidFile -Force -ErrorAction SilentlyContinue
        return $null
    }

    return $proc
}

function Test-PortOpen([int]$port) {
    $client = [System.Net.Sockets.TcpClient]::new()
    try {
        $conn = $client.BeginConnect('127.0.0.1', $port, $null, $null)
        $ok = $conn.AsyncWaitHandle.WaitOne(400, $false)
        if ($ok) { $client.EndConnect($conn) }
        return $ok
    } catch {
        return $false
    } finally {
        $client.Close()
    }
}

function Build-Service($svc) {
    $svcDir = Join-Path $ProjectRoot $svc.Dir
    $exe = Join-Path $BinDir "$($svc.Name).exe"
    Write-Host "  [build] $($svc.Name) ..." -ForegroundColor DarkGray
    Push-Location $svcDir
    try {
        & go build -o $exe "./cmd/$($svc.Cmd)"
        if ($LASTEXITCODE -ne 0) {
            Write-Host "[ERR] build 失败: $($svc.Name)" -ForegroundColor Red
            exit 1
        }
    } finally {
        Pop-Location
    }
    return $exe
}

function Start-Service($svc) {
    $existing = Get-RunningProcess $svc
    if ($existing) {
        Write-Host "  [skip] $($svc.Name) 已在运行 (PID $($existing.Id))" -ForegroundColor Yellow
        return
    }

    $exe = Join-Path $BinDir "$($svc.Name).exe"
    if (-not $NoBuild -or -not (Test-Path $exe)) {
        $exe = Build-Service $svc
    }

    $svcDir = Join-Path $ProjectRoot $svc.Dir
    $log = Get-LogFile $svc
    $err = Get-ErrFile $svc

    $proc = Start-Process -FilePath $exe `
        -ArgumentList '-conf', $svc.Conf `
        -WorkingDirectory $svcDir `
        -RedirectStandardOutput $log `
        -RedirectStandardError $err `
        -WindowStyle Hidden `
        -PassThru

    $proc.Id | Out-File -FilePath (Get-PidFile $svc) -Encoding ascii

    # 端口探活
    $ready = $false
    for ($i = 0; $i -lt 30; $i++) {
        if ($proc.HasExited) { break }
        if (Test-PortOpen $svc.Port) { $ready = $true; break }
        Start-Sleep -Milliseconds 400
    }

    if ($proc.HasExited) {
        Write-Host "  [FAIL] $($svc.Name) 启动后立即退出 (exit $($proc.ExitCode)),看日志: $err" -ForegroundColor Red
    } elseif ($ready) {
        Write-Host "  [ OK ] $($svc.Name)  PID $($proc.Id)  :$($svc.Port)" -ForegroundColor Green
    } else {
        Write-Host "  [WARN] $($svc.Name) PID $($proc.Id) 已起但 :$($svc.Port) 未就绪,看日志: $log" -ForegroundColor Yellow
    }
}

function Stop-Service($svc) {
    $proc = Get-RunningProcess $svc
    $pidFile = Get-PidFile $svc
    if ($proc) {
        Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
        Write-Host "  [stop] $($svc.Name) (PID $($proc.Id))" -ForegroundColor DarkGray
    } else {
        Write-Host "  [----] $($svc.Name) 未运行" -ForegroundColor DarkGray
    }
    if (Test-Path $pidFile) { Remove-Item $pidFile -Force }
}

function Show-Status {
    Write-Host "===== Pandora 业务服务状态 =====" -ForegroundColor Cyan
    Write-Host ("{0,-16} {1,-8} {2,-8} {3,-8} {4}" -f 'SERVICE', 'PID', 'PORT', 'PORT-UP', 'STATE')
    foreach ($svc in $Services) {
        $proc = Get-RunningProcess $svc
        $portUp = Test-PortOpen $svc.Port
        if ($proc -and $portUp) { $state = 'running'; $color = 'Green' }
        elseif ($proc) { $state = 'starting?'; $color = 'Yellow' }
        elseif ($portUp) { $state = 'port-busy'; $color = 'Yellow' }  # 端口被别的进程占,或 IDE 在调试
        else { $state = 'stopped'; $color = 'DarkGray' }
        $svcPid = if ($proc) { $proc.Id } else { '-' }
        Write-Host ("{0,-16} {1,-8} {2,-8} {3,-8} {4}" -f $svc.Name, $svcPid, $svc.Port, $(if ($portUp) { 'yes' } else { 'no' }), $state) -ForegroundColor $color
    }
}

# ===== 主流程 =====
switch ($Action) {

    'status' { Show-Status; break }

    'logs' {
        if (-not $Service) { Write-Host "[ERR] -Action logs 需要 -Service <name>" -ForegroundColor Red; exit 1 }
        $svc = Get-Service $Service
        $log = Get-LogFile $svc
        if (-not (Test-Path $log)) { Write-Host "[ERR] 无日志文件: $log" -ForegroundColor Red; exit 1 }
        Write-Host "===== tail $($svc.Name) 日志 (Ctrl+C 退出) =====" -ForegroundColor Cyan
        Get-Content $log -Tail 40 -Wait
        break
    }

    'down' {
        Write-Host "===== 停止业务服务 =====" -ForegroundColor Cyan
        if ($Service) { Stop-Service (Get-Service $Service) }
        else { foreach ($svc in $Services) { Stop-Service $svc } }
        break
    }

    'build' {
        $targets = if ($Service) { @(Get-Service $Service) } else { Get-ProfileServices }
        Write-Host "===== 构建 ($($targets.Count) 个) =====" -ForegroundColor Cyan
        foreach ($svc in $targets) { Build-Service $svc | Out-Null }
        Write-Host "[done] 构建完成" -ForegroundColor Green
        break
    }

    'restart' {
        if (-not $Service) { Write-Host "[ERR] -Action restart 需要 -Service <name>" -ForegroundColor Red; exit 1 }
        $svc = Get-Service $Service
        Write-Host "===== 重启 $($svc.Name) =====" -ForegroundColor Cyan
        Stop-Service $svc
        Start-Sleep -Milliseconds 300
        Start-Service $svc
        break
    }

    'up' {
        # 单服务前台运行
        if ($Foreground) {
            if (-not $Service) { Write-Host "[ERR] -Foreground 需要 -Service <name>" -ForegroundColor Red; exit 1 }
            $svc = Get-Service $Service
            $running = Get-RunningProcess $svc
            if ($running) {
                Write-Host "[!] $($svc.Name) 已在后台运行 (PID $($running.Id)),先停掉它" -ForegroundColor Yellow
                Stop-Service $svc
            }
            $svcDir = Join-Path $ProjectRoot $svc.Dir
            Write-Host "===== 前台运行 $($svc.Name) (:$($svc.Port),Ctrl+C 退出) =====" -ForegroundColor Cyan
            Push-Location $svcDir
            try { & go run "./cmd/$($svc.Cmd)" -conf $svc.Conf } finally { Pop-Location }
            break
        }

        $targets = if ($Service) { @(Get-Service $Service) } else { Get-ProfileServices }
        if ($targets.Count -eq 0) { Write-Host "[!] profile '$Profile' 排除后无服务可启动" -ForegroundColor Yellow; break }

        Write-Host "===== 启动业务服务 (profile=$Profile, $($targets.Count) 个) =====" -ForegroundColor Cyan
        if ($Exclude.Count -gt 0) { Write-Host "排除: $($Exclude -join ', ')  (留给 IDE 调试)" -ForegroundColor Yellow }
        Write-Host ""

        foreach ($svc in $targets) { Start-Service $svc }

        Write-Host ""
        Show-Status
        Write-Host ""
        Write-Host "客户端入口 (UE 连这个): Envoy https://localhost:8443" -ForegroundColor Green
        Write-Host "看日志:  pwsh tools/scripts/run_services.ps1 -Action logs -Service <name>" -ForegroundColor DarkGray
        Write-Host "全停止:  pwsh tools/scripts/run_services.ps1 -Action down" -ForegroundColor DarkGray
        break
    }
}
