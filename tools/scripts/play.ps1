<#
.SYNOPSIS
  Pandora 后端「策划一键启动」(只要装 Docker,双击即用)。

.DESCRIPTION
  面向策划的极简入口:默认 docker 模式不需要装 Go、不需要会编译,机器上只要有 Docker Desktop。
  做的事:
    1) 检查 Docker —— 没装就引导安装(能 winget 就自动装),没在跑就帮忙把 Docker Desktop 拉起来并等待就绪。
    2) Docker 就绪后,把整套后端跑起来(基础设施 + 19 个 go 服务全在容器里)。
       首次会在容器内编译镜像(稍慢),之后复用缓存秒起。
  -Battle 是含战斗混合模式:17 个纯业务服务进 docker,ds_allocator / hub_allocator 留宿主
  exec Windows DS;服务器机器当前仍需 Go 构建这两个 allocator。

.EXAMPLE
  双击 仓库根目录\策划一键启动-含战斗.cmd        # 本地战斗版(17 业务容器 + 2 个宿主 allocator + 本机 Windows DS)
  双击 仓库根目录\策划一键停止.cmd               # 停止(含战斗版)
  双击 仓库根目录\内网服务器一键启动-含战斗.cmd  # 内网服务器战斗版(17 容器 + 2 个宿主 allocator + Windows DS + 绑内网 IP)
  双击 仓库根目录\内网服务器一键停止.cmd         # 停止(内网服务器战斗版)
  pwsh tools/scripts/play.ps1 -Battle       # 本地战斗版
  pwsh tools/scripts/play.ps1 -Battle -Intranet    # 内网服务器战斗版(绑内网 IP,供局域网策划连)
  pwsh tools/scripts/play.ps1 -Battle -OpenEditor  # 启动后端后打开 UE Editor 当客户端
  pwsh tools/scripts/play.ps1 -Battle -OpenClient  # 启动后端后打开已打包 Windows 客户端
  pwsh tools/scripts/play.ps1 -Battle -Stop # 停止
  pwsh tools/scripts/play.ps1 -Status       # 看状态
#>
[CmdletBinding()]
param(
    [switch]$Stop,     # 停止整套后端
    [switch]$Status,   # 只看状态,不启动
    [switch]$Battle,   # 本地战斗模式:17 业务容器 + 2 个宿主 allocator + Windows DS(进 hub→匹配→battle 战斗)
    [switch]$Intranet, # 内网服务器模式:全容器 + 绑内网 IP,供局域网内策划客户端连(DS=mock)
    [switch]$OpenEditor, # 启动完成后打开发行版 UE Editor,用 PIE/Standalone 当客户端进服
    [switch]$OpenClient  # 启动完成后打开已打包 Windows 客户端
)

$ErrorActionPreference = 'Stop'
$ScriptDir   = $PSScriptRoot
$ProjectRoot = (Resolve-Path "$ScriptDir/../..").Path
$StartPs1    = Join-Path $ScriptDir 'start.ps1'
$DsAllocConf = Join-Path $ProjectRoot 'services/battle/ds_allocator/etc/ds_allocator-dev.yaml'
$UeProject   = 'C:\work\Pandora-Client-SVN\Pandora\Pandora.uproject'
$UeEditorExe = 'F:\UnrealEngine-5.8.0-release\Engine\Binaries\Win64\UnrealEditor.exe'
$PackagedClientExe = 'C:\work\Pandora-Client-SVN\Pandora\Saved\StagedBuilds\Windows\Pandora.exe'

# ===== 输出辅助 =====
function Write-Info($m) { Write-Host "[INFO] $m" -ForegroundColor Cyan }
function Write-Ok($m)   { Write-Host "[ OK ] $m" -ForegroundColor Green }
function Write-Warn($m) { Write-Host "[WARN] $m" -ForegroundColor Yellow }
function Write-Err($m)  { Write-Host "[ERR ] $m" -ForegroundColor Red }
function Write-Step($m) { Write-Host "`n===== $m =====" -ForegroundColor Magenta }

function Test-CommandExists([string]$cmd) {
    return [bool](Get-Command $cmd -ErrorAction SilentlyContinue)
}

# 取本机对外那张网卡的内网 IPv4,供内网机把「返回给客户端的地址」
# (Hub/Battle DS advertise_host)自动改成局域网 IP。与 start.ps1 的 Resolve-LanIp 同逻辑。
# 关键:按默认路由(0.0.0.0/0)选网卡,避开 Docker/WSL/Hyper-V 虚拟网卡的
# 172.*/10.*/192.168.* 地址——否则局域网策划客户端会拿到不可达的 DS 地址。
function Resolve-LanIp {
    $isUsable = { $_.IPAddress -notmatch '^(127\.|169\.254\.)' -and $_.PrefixOrigin -ne 'WellKnown' }
    # 1) 默认路由所在网卡 = 真正对外那张(按路由跃点 + 接口跃点升序取最优)
    $best = Get-NetRoute -DestinationPrefix '0.0.0.0/0' -AddressFamily IPv4 -ErrorAction SilentlyContinue |
        Sort-Object -Property RouteMetric, @{ Expression = { (Get-NetIPInterface -InterfaceIndex $_.InterfaceIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue).InterfaceMetric } } |
        Select-Object -First 1
    if ($best) {
        $ip = Get-NetIPAddress -AddressFamily IPv4 -InterfaceIndex $best.InterfaceIndex -ErrorAction SilentlyContinue |
            Where-Object $isUsable | Select-Object -First 1 -ExpandProperty IPAddress
        if (-not [string]::IsNullOrWhiteSpace($ip)) { return $ip }
    }
    # 2) 回退:排除常见虚拟网卡后取第一个
    $virtual = 'vEthernet|WSL|Hyper-V|Docker|VirtualBox|VMware|Loopback|TAP-|VPN|tun'
    $ip = Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue |
        Where-Object $isUsable | Where-Object { $_.InterfaceAlias -notmatch $virtual } |
        Sort-Object -Property SkipAsSource | Select-Object -First 1 -ExpandProperty IPAddress
    if (-not [string]::IsNullOrWhiteSpace($ip)) { return $ip }
    # 3) 最后兜底:旧启发式(至少返回点东西)
    return Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue |
        Where-Object $isUsable | Sort-Object -Property SkipAsSource |
        Select-Object -First 1 -ExpandProperty IPAddress
}

function Open-UeEditorClient {
    if (-not (Test-Path $UeEditorExe)) {
        Write-Warn "找不到 UE Editor:$UeEditorExe"
        Write-Warn '       可手动打开同版本发行版 Editor,再打开 Pandora.uproject。'
        return
    }
    if (-not (Test-Path $UeProject)) {
        Write-Warn "找不到 UE 工程:$UeProject"
        return
    }
    Write-Info '打开 UE Editor。进工程后用 Play/Standalone 作为客户端登录;不要用 Listen Server。'
    Start-Process -FilePath $UeEditorExe -ArgumentList "`"$UeProject`"" | Out-Null
}

function Open-PackagedClient {
    if (-not (Test-Path $PackagedClientExe)) {
        Write-Warn "找不到已打包 Windows 客户端:$PackagedClientExe"
        Write-Warn '       可先用 UE 打 Windows Client 包,或直接用 -OpenEditor 进 Editor 测。'
        return
    }
    Write-Info '打开已打包 Windows 客户端。'
    Start-Process -FilePath $PackagedClientExe -WorkingDirectory (Split-Path -Parent $PackagedClientExe) | Out-Null
}

function Maybe-OpenUeClient {
    if ($OpenEditor -and $OpenClient) {
        Write-Warn '同时传了 -OpenEditor / -OpenClient;只打开 Editor。'
        Open-UeEditorClient
        return
    }
    if ($OpenEditor) { Open-UeEditorClient; return }
    if ($OpenClient) { Open-PackagedClient; return }
}

function Test-DockerRunning {
    if (-not (Test-CommandExists 'docker')) { return $false }
    docker info *> $null
    return ($LASTEXITCODE -eq 0)
}

# 尝试找到 Docker Desktop 可执行文件
function Get-DockerDesktopExe {
    $candidates = @(
        (Join-Path $Env:ProgramFiles 'Docker\Docker\Docker Desktop.exe'),
        (Join-Path ${Env:ProgramFiles(x86)} 'Docker\Docker\Docker Desktop.exe')
    )
    foreach ($p in $candidates) {
        if ($p -and (Test-Path $p)) { return $p }
    }
    return $null
}

# 装 Docker 前确认 CPU 虚拟化已开(没开装了也起不来,先拦住省得白折腾)。
# 只做只读检查:返回 $false 表示明确未开启;读不到状态时放行(返回 $true)。
function Test-VirtualizationOn {
    try {
        $virt = (Get-CimInstance Win32_Processor -ErrorAction Stop |
                 Select-Object -First 1 -ExpandProperty VirtualizationFirmwareEnabled)
        if ($virt) {
            Write-Ok 'CPU 虚拟化已开启'
            return $true
        }
        Write-Err 'CPU 虚拟化没开 —— Docker Desktop 装了也起不来。'
        Write-Host '       请重启进 BIOS 开启 Intel VT-x / AMD-V(虚拟化),再双击本脚本。' -ForegroundColor Yellow
        return $false
    } catch {
        # 读不到就别拦策划,放行交给 Docker Desktop 自己引导
        return $true
    }
}

# 确保 Docker 命令存在
function Ensure-DockerInstalled {
    if (Test-CommandExists 'docker') {
        Write-Ok 'Docker 已安装'
        return $true
    }
    Write-Warn 'Docker 未安装'
    if (-not (Test-VirtualizationOn)) {
        return $false
    }
    if (Test-CommandExists 'winget') {
        Write-Info '尝试用 winget 安装 Docker Desktop(可能要几分钟)...'
        winget install --id Docker.DockerDesktop --silent --accept-source-agreements --accept-package-agreements | Out-Null
    } else {
        Write-Err '未找到 winget,无法自动安装。'
    }
    Write-Host ''
    Write-Warn '请完成 Docker Desktop 安装(可能需要重启电脑),'
    Write-Warn '然后『启动 Docker Desktop』、等右下角鲸鱼图标变绿,再重新双击本脚本。'
    Write-Host '       手动下载:https://www.docker.com/products/docker-desktop/' -ForegroundColor Yellow
    return $false
}

# 确保 Docker daemon 在跑(没跑就尝试拉起 Docker Desktop 并等待就绪)
function Ensure-DockerRunning {
    if (Test-DockerRunning) {
        Write-Ok 'Docker 正在运行'
        return $true
    }
    Write-Warn 'Docker 已装但还没运行,尝试启动 Docker Desktop...'
    $exe = Get-DockerDesktopExe
    if ($exe) {
        Start-Process -FilePath $exe | Out-Null
    } else {
        Write-Warn '没找到 Docker Desktop 程序,请手动从开始菜单启动它。'
    }

    Write-Info 'Docker 首次启动较慢,正在等待就绪(最多约 3 分钟)...'
    $maxTries = 90          # 90 * 2s = 180s
    for ($i = 1; $i -le $maxTries; $i++) {
        if (Test-DockerRunning) {
            Write-Ok 'Docker 已就绪'
            return $true
        }
        Start-Sleep -Seconds 2
        if ($i % 10 -eq 0) { Write-Info "  仍在等待 Docker 启动... ($($i*2)s)" }
    }
    Write-Err 'Docker 还没就绪。请确认 Docker Desktop 已启动(右下角鲸鱼图标变绿)后重试。'
    return $false
}

# 从 ds_allocator-dev.yaml 读出本机 Windows DS 可执行文件路径(local_ds.executable_path)。
function Get-LocalDsExePath {
    if (-not (Test-Path $DsAllocConf)) { return $null }
    foreach ($line in (Get-Content $DsAllocConf)) {
        if ($line -match '^\s*executable_path:\s*"(.+?)"') {
            return $Matches[1].Trim()
        }
    }
    return $null
}

# 起完后端后,主动确认「常驻大厅 Hub DS(PandoraServer.exe,UDP :7777)」真的被 hub_allocator 拉起来了。
# 为什么要单独确认:Hub DS 是 hub_allocator 首个心跳 sweep(~5s)时懒拉起的独立 Windows 进程,
# 拉起失败(cooked 内容缺失 / 关卡名错 / 打包不完整)只写日志、不影响后端"已启动",客户端登录后
# 却拿到一个连不上的 :7777 地址,在 "Game client on port 7777" 处一直卡住。这里探测最多 ~40s,
# 起不来就把 Hub DS 自己的日志尾部打出来,直接暴露真实崩溃原因,而不是让策划干等。
function Confirm-HubDsUp {
    $hubLog = Join-Path $ProjectRoot 'run/dev/logs/ds/pandora-hub-local.log'
    for ($i = 0; $i -lt 40; $i++) {
        $proc = Get-Process -Name 'PandoraServer' -ErrorAction SilentlyContinue
        $udp = $null
        $udpChecked = $true
        try {
            $udp = Get-NetUDPEndpoint -LocalPort 7777 -ErrorAction SilentlyContinue | Where-Object { $_.OwningProcess -ne 0 }
        } catch {
            $udp = $null
            $udpChecked = $false
        }
        if ($udp) {
            Write-Ok "大厅 Hub DS 已就绪(UDP :7777 已监听)。"
            return $true
        }
        if (-not $udpChecked -and $proc) {
            Write-Warn '无法检查 UDP :7777,但检测到 PandoraServer 进程在跑;按兼容模式继续。'
            return $true
        }
        Start-Sleep -Seconds 1
    }
    Write-Err '等了 40s 仍没检测到大厅 Hub DS(PandoraServer.exe / UDP :7777)进程。'
    Write-Warn '客户端登录后会拿到 hub 地址但连不上,卡在 "Game client on port 7777"。常见原因:'
    Write-Warn '  - UE 打的 Windows Server 包不完整 / cooked 内容缺失(不能指向 Binaries 裸 exe);'
    Write-Warn '  - hub_allocator 没起来或崩溃(端口 :50021 被残留实例占,已在启动阶段自动清理);'
    Write-Warn '  - local_hub.map_name 关卡名与打包内容不匹配。'
    if (Test-Path $hubLog) {
        Write-Info "Hub DS 日志尾部($hubLog):"
        Get-Content $hubLog -Tail 25 | ForEach-Object { Write-Host "    $_" -ForegroundColor DarkGray }
    } else {
        Write-Warn "  没有找到 Hub DS 日志 $hubLog —— 说明 hub_allocator 根本没尝试拉起 DS,优先查 hub_allocator 日志。"
        Write-Warn '  查:  pwsh tools/scripts/run_services.ps1 -Action logs -Service hub_allocator'
    }
    return $false
}

# 自动探测本机 Windows DS 可执行文件(策划机场景:配置里写死的盘符不在本机)。
# 约定:服务器仓与客户端 Client 仓平级(同一父目录),DS 是 Client 仓的
#   Packages\Server_Win64_*\WindowsServer\PandoraServer.exe(SVN 提交后也在 Packages 下)。
# 两级探测,命中多个取最新打包的那个,返回 @{ Exe, Root } 或 $null:
#   ① 固定深度 glob(最快,标准布局直接命中);
#   ② 兜底:递归扫兄弟目录里的 Packages 子树(构建产物目录,比 UE 资源树小很多,够快),
#      应对 SVN 提交后 Server_Win64_* / WindowsServer 层级略有出入的情况。
function Resolve-LocalDsExe {
    $repoParent = Split-Path $ProjectRoot -Parent
    if (-not $repoParent -or -not (Test-Path $repoParent)) { return $null }
    $siblings = Get-ChildItem -Path $repoParent -Directory -ErrorAction SilentlyContinue

    # ① 固定深度:Packages\Server_Win64_*\WindowsServer\PandoraServer.exe
    $hits = @()
    foreach ($dir in $siblings) {
        $glob = Join-Path $dir.FullName 'Packages\Server_Win64_*\WindowsServer\PandoraServer.exe'
        $hits += Get-ChildItem -Path $glob -ErrorAction SilentlyContinue | ForEach-Object {
            [pscustomobject]@{ Exe = $_.FullName; Root = $dir.FullName; LastWriteTime = $_.LastWriteTime }
        }
    }
    if ($hits.Count -gt 0) {
        return $hits | Sort-Object LastWriteTime -Descending | Select-Object -First 1
    }

    # ② 兜底:递归扫兄弟目录的 Packages 子树(只进 Packages,避免遍历 Content 大资源树)
    foreach ($dir in $siblings) {
        $pkgRoot = Join-Path $dir.FullName 'Packages'
        if (-not (Test-Path $pkgRoot)) { continue }
        $hits += Get-ChildItem -Path $pkgRoot -Recurse -Filter 'PandoraServer.exe' -File -ErrorAction SilentlyContinue | ForEach-Object {
            [pscustomobject]@{ Exe = $_.FullName; Root = $dir.FullName; LastWriteTime = $_.LastWriteTime }
        }
    }
    if ($hits.Count -eq 0) { return $null }
    return $hits | Sort-Object LastWriteTime -Descending | Select-Object -First 1
}

# 确保 Go 已安装(本地战斗版 go 服务跑宿主进程需要)。没装就 winget 自动装。
# 返回 $true=已就绪可继续;$false=刚装上但当前终端 PATH 没刷新,需新开终端重跑。
function Ensure-GoInstalled {
    if (Test-CommandExists 'go') {
        Write-Ok 'Go 已就绪'
        return $true
    }
    Write-Warn 'Go 未安装'
    if (-not (Test-CommandExists 'winget')) {
        Write-Err '未找到 winget,无法自动安装 Go;请手动装:https://go.dev/dl/ (需 1.26.4+)'
        return $false
    }
    Write-Info '尝试用 winget 安装 Go(GoLang.Go,可能要几分钟)...'
    winget install --id GoLang.Go --silent --accept-source-agreements --accept-package-agreements | Out-Null
    if (Test-CommandExists 'go') {
        Write-Ok 'Go 安装成功'
        return $true
    }
    Write-Warn 'Go 已装好,但当前终端还找不到 go 命令(PATH 未刷新属正常)。'
    Write-Warn '       请『新开一个终端』(或重新双击本 .cmd)后再运行一次。'
    return $false
}

# 确保 mkcert 已安装(Envoy 本地 TLS 证书的自动签发 / 共享 CA 安装都靠它;
# 内网服务器要给局域网策划发 TLS 证书,缺了 dev_up 会在 Envoy 证书检查处报错退出)。
# 没装就 winget 自动装。返回 $true=已就绪;$false=刚装上但当前终端 PATH 没刷新,需新开终端重跑。
function Ensure-MkcertInstalled {
    if (Test-CommandExists 'mkcert') {
        Write-Ok 'mkcert 已就绪'
        return $true
    }
    Write-Warn 'mkcert 未安装(Envoy 本地 TLS 证书需要它)'
    if (-not (Test-CommandExists 'winget')) {
        Write-Err '未找到 winget,无法自动安装 mkcert;请手动装:https://github.com/FiloSottile/mkcert#installation'
        return $false
    }
    Write-Info '尝试用 winget 安装 mkcert(FiloSottile.mkcert,可能要几分钟)...'
    winget install --id FiloSottile.mkcert --silent --accept-source-agreements --accept-package-agreements | Out-Null
    if (Test-CommandExists 'mkcert') {
        Write-Ok 'mkcert 安装成功'
        return $true
    }
    Write-Warn 'mkcert 已装好,但当前终端还找不到 mkcert 命令(PATH 未刷新属正常)。'
    Write-Warn '       请『新开一个终端』(或重新双击本 .cmd)后再运行一次。'
    return $false
}

# 本地战斗模式预检:需要 Go(宿主进程)+ 打包好的 Windows DS。返回 $true=可继续。
function Test-BattlePrerequisites {
    $ok = $true

    if (-not (Ensure-GoInstalled)) {
        $ok = $false
    }

    # 先探测平级 Client 仓:只要探到就自动设 PANDORA_DS_ROOT(策划零操作,
    # 配置可用 ${PANDORA_DS_ROOT}/Packages/... 拼接;该环境变量仅本进程及其子 go 服务可见)。
    $detected = Resolve-LocalDsExe
    if ($detected) {
        $env:PANDORA_DS_ROOT = $detected.Root
        Write-Info "已自动解析 PANDORA_DS_ROOT=$($detected.Root)"
    }

    $exe = Get-LocalDsExePath
    if ((-not [string]::IsNullOrEmpty($exe)) -and (Test-Path $exe)) {
        # 配置里的路径在本机存在(开发机场景),直接用。
        Write-Ok "Windows DS 已就绪:$exe"
    } else {
        # 配置路径不在本机(策划机场景:Client 目录不在配置写死的盘符)。
        # 除了上面的 PANDORA_DS_ROOT,再注入精确的 EXE/DIR 给 go 服务(无需改配置)。
        if ($detected) {
            $env:PANDORA_DS_EXE = $detected.Exe
            $env:PANDORA_DS_DIR = Split-Path $detected.Exe -Parent
            Write-Ok "自动探测到 Windows DS:$($detected.Exe)"
            Write-Info '       已注入 PANDORA_DS_ROOT / PANDORA_DS_EXE / PANDORA_DS_DIR,go 服务会优先用它(无需改仓库配置)。'
        } elseif ([string]::IsNullOrEmpty($exe)) {
            Write-Warn '没在 ds_allocator-dev.yaml 找到 local_ds.executable_path。'
            Write-Warn '       请让 UE 同学打一个 Windows Server 包,再把 executable_path 指向 PandoraServer.exe。'
            $ok = $false
        } else {
            Write-Err "找不到 Windows DS 可执行文件:$exe"
            Write-Warn '       这是 UE 打包产物(PandoraServer.exe),不在本仓库;也没在平级 Client 目录探测到。'
            Write-Warn '       需要先让 UE 同学打一个 Windows Server 包(产出 Packages\Server_Win64_*\WindowsServer\PandoraServer.exe),'
            Write-Warn '       放到与本服务器仓平级的 Client 目录下;或把 ds_allocator-dev.yaml 的 executable_path 指到它。'
            $ok = $false
        }
    }

    return $ok
}

# ===== 主流程 =====
Write-Host ''
Write-Host '============================================' -ForegroundColor Magenta
Write-Host '  Pandora 后端  策划一键启动' -ForegroundColor Magenta
Write-Host '============================================' -ForegroundColor Magenta

if (-not (Test-Path $StartPs1)) {
    Write-Err "找不到 start.ps1:$StartPs1。请确认仓库完整(git pull)。"
    exit 1
}

# 看状态:不需要 daemon 也能看(看不到就提示)
if ($Status) {
    if ($Battle) {
        & $StartPs1 -Mode battle -Status
    } elseif ($Intranet) {
        & $StartPs1 -Mode intranet -Status
    } else {
        & $StartPs1 -Mode docker -Status
    }
    exit $LASTEXITCODE
}

# 停止
if ($Stop) {
    if (-not (Test-CommandExists 'docker')) {
        Write-Warn 'Docker 未安装,无需停止。'
        exit 0
    }
    Write-Step '停止整套后端'
    if ($Battle) {
        & $StartPs1 -Mode battle -Down
    } elseif ($Intranet) {
        & $StartPs1 -Mode intranet -Down
    } else {
        & $StartPs1 -Mode docker -Down
    }
    Write-Ok '已停止。'
    exit $LASTEXITCODE
}

# ===== 本地战斗版:17 业务容器 + 2 个宿主 allocator + Windows DS(进 hub → 匹配 → battle 战斗)=====
# -Battle 单独:本机自测,DS/Hub advertise_host=127.0.0.1(只本机客户端连得到)。
# -Battle -Intranet:内网测试服,自动探测本机内网 IPv4 并经 PANDORA_DS_ADVERTISE_HOST 注入,
#   Hub/Battle DS 把返回给客户端的地址改成局域网 IP,局域网内其它策划客户端可连进真实大厅+战斗。
if ($Battle) {
    $lanIp = $null
    if ($Intranet) {
        Write-Step '内网战斗版(17 容器 + 2 个宿主 allocator + Windows DS,绑内网 IP 供多人进真实战斗)'
        # 允许手动覆盖:预先设了 PANDORA_DS_ADVERTISE_HOST 就用它,不再自动探测
        # (多网卡/自动探测选错网卡时可用 $env:PANDORA_DS_ADVERTISE_HOST=<IP> 兜底)。
        if (-not [string]::IsNullOrWhiteSpace($env:PANDORA_DS_ADVERTISE_HOST)) {
            $lanIp = $env:PANDORA_DS_ADVERTISE_HOST.Trim()
            Write-Ok "使用手动指定的内网 IP=$lanIp(来自 PANDORA_DS_ADVERTISE_HOST)。"
        } else {
            $lanIp = Resolve-LanIp
            if ([string]::IsNullOrWhiteSpace($lanIp)) {
                Write-Err '未能自动解析本机内网 IPv4;请确认已连内网,或用 $env:PANDORA_DS_ADVERTISE_HOST=<IP> 手动指定后重试。'
                exit 1
            }
            $env:PANDORA_DS_ADVERTISE_HOST = $lanIp
            Write-Ok "已自动解析内网 IP=$lanIp,Hub/Battle DS 将把连接地址返回为该 IP(策划客户端连得到)。"
        }
    }

    Write-Step '本地战斗版预检(需 Go + Windows DS)'
    Write-Info 'docker 版跑 17 个纯业务服务;ds/hub allocator 跑宿主 go 进程(要 exec Windows DS,'
    Write-Info '战斗/大厅 DS 是 Windows 程序、跑不进 Linux 容器)。策划机只是客户端,不用起任何 go 服务。'
    if (-not (Test-BattlePrerequisites)) {
        Write-Err '本地战斗版前置条件不满足,见上方提示。'
        exit 1
    }

    Write-Step '检查 Docker(基础设施 + 17 业务服务都跑在 docker)'
    if (-not (Ensure-DockerInstalled)) { exit 1 }
    if (-not (Ensure-DockerRunning))   { exit 1 }

    # 内网战斗版:Envoy 客户端面(:8443)是 TLS,局域网策划要用同一套证书,mkcert 必备
    # (证书 SAN 已含本机内网 IP,见 envoy_cert.ps1)。本机自测(非 -Intranet)时 dev_up 内部会自查,这里不强制。
    if ($Intranet) {
        Write-Step '检查 mkcert(内网 Envoy TLS 证书,策划客户端要信任同一 CA)'
        if (-not (Ensure-MkcertInstalled)) { exit 1 }
    }

    # battle 模式:基础设施(docker) + 17 个业务服务(docker) + ds/hub allocator(宿主进程),
    # ds_allocator 走 mode=local,匹配成局后直接 exec 本机 Windows DS。
    & $StartPs1 -Mode battle
    $rc = $LASTEXITCODE
    Write-Host ''
    if ($rc -eq 0) {
        # 后端服务起来了,但大厅 Hub DS 是 hub_allocator 懒拉起的独立进程,单独确认它真起来了,
        # 否则客户端登录后会拿到 hub 地址却连不上(卡在 "Game client on port 7777")。
        Write-Step '确认大厅 Hub DS 已拉起(PandoraServer.exe / UDP :7777)'
        $hubOk = Confirm-HubDsUp
        if ($Intranet) {
            Write-Ok "内网战斗版后端已启动!把下面的连接地址发给局域网内策划:"
            Write-Host "  - 客户端后端地址(Envoy TLS): https://${lanIp}:8443" -ForegroundColor Green
            Write-Host '  - 策划机不用装 Docker / Go,只要能连内网、并信任同一套 dev CA(mkcert 根证书)。' -ForegroundColor Green
            Write-Host '  - 登录 → 进大厅 → 匹配,成局后本机自动拉起 Windows DS,策划一起进真实战斗 DS。' -ForegroundColor Green
            Write-Host '  - 看状态:      pwsh tools/scripts/play.ps1 -Battle -Intranet -Status' -ForegroundColor DarkGray
            Write-Host '  - 停止:        pwsh tools/scripts/play.ps1 -Battle -Stop' -ForegroundColor DarkGray
        } else {
            Write-Ok '本地战斗版后端已启动!'
            Write-Host '  - 客户端网关(Envoy): https://127.0.0.1:8443' -ForegroundColor Green
            Write-Host '  - 可以直接用发行版 UE Editor 当客户端: Play/New Editor Window/Standalone 后登录即可进 Hub DS。' -ForegroundColor Green
            Write-Host '  - 不必须起已打包 client;打包 client 只用于更接近发行环境的最终验证。' -ForegroundColor Green
            Write-Host '  - 现在用 UE 客户端登录 → 进大厅 → 匹配,成局后会自动拉起本机 Windows DS 进战斗。' -ForegroundColor Green
            Write-Host '  - 一键打开:    pwsh tools/scripts/play.ps1 -Battle -OpenEditor  或  -OpenClient' -ForegroundColor DarkGray
            Write-Host '  - 供内网多人进战斗: pwsh tools/scripts/play.ps1 -Battle -Intranet' -ForegroundColor DarkGray
            Write-Host '  - 停止:        pwsh tools/scripts/play.ps1 -Battle -Stop' -ForegroundColor DarkGray
        }
        if (-not $hubOk) {
            Write-Warn '⚠️ 大厅 Hub DS 尚未就绪:客户端现在登录会卡在连大厅,请先按上方 Hub DS 日志排查。'
        }
        Maybe-OpenUeClient
    } else {
        Write-Err '启动过程中出错了,请把上面的红色 [ERR] 信息发给后端同学。'
    }
    exit $rc
}

# ===== 内网服务器版:全容器 + 绑内网 IP,供局域网内策划客户端连(DS=mock)=====
if ($Intranet) {
    Write-Step '内网服务器版(全容器,绑内网 IP,供多人连)'
    Write-Info '这台机器当「内网测试服」:基础设施 + 后端服务都跑在本机容器里,'
    Write-Info '策划只需在各自客户端填本机内网 IP 就能连进来,他们不用装 Docker。'

    Write-Step '检查 Docker'
    if (-not (Ensure-DockerInstalled)) { exit 1 }
    if (-not (Ensure-DockerRunning))   { exit 1 }

    # 内网服务器要给局域网策划发 TLS 证书,mkcert 必备(Envoy 证书自动签发 / 共享 CA 安装都靠它)。
    Write-Step '检查 mkcert(Envoy 本地 TLS 证书)'
    if (-not (Ensure-MkcertInstalled)) { exit 1 }

    # 委托 intranet 模式:同 docker 全容器(DS=mock),但绑 0.0.0.0 并打印内网地址。
    & $StartPs1 -Mode intranet
    $rc = $LASTEXITCODE
    Write-Host ''
    if ($rc -eq 0) {
        Write-Ok '内网测试服已启动!上面绿色行里的「内网 IP 地址」发给策划,'
        Write-Host '  - 策划把 UE 客户端后端地址指向那个 https://<内网IP>:8443 即可登录。' -ForegroundColor Green
        Write-Host '  - 策划机不用装 Docker / 不用装 Go,只要能连内网。' -ForegroundColor Green
        Write-Host '  - DS=mock:能测登录/大厅/业务,但进不了真实战斗 DS。' -ForegroundColor Yellow
        Write-Host '  - 看状态:        pwsh tools/scripts/play.ps1 -Intranet -Status' -ForegroundColor DarkGray
        Write-Host '  - 停止:          pwsh tools/scripts/play.ps1 -Intranet -Stop' -ForegroundColor DarkGray
    } else {
        Write-Err '启动过程中出错了,请把上面的红色 [ERR] 信息发给后端同学。'
    }
    exit $rc
}

# 启动:先把 Docker 准备好
Write-Step '检查 Docker'
if (-not (Ensure-DockerInstalled)) { exit 1 }
if (-not (Ensure-DockerRunning))   { exit 1 }

# Envoy 本地 TLS 证书需要 mkcert(自动签发 / 校验),没装先自动补上。
Write-Step '检查 mkcert(Envoy 本地 TLS 证书)'
if (-not (Ensure-MkcertInstalled)) { exit 1 }

# 委托给已验证的 docker 模式:基础设施 + 19 个 go 服务全容器化
# (首次会在容器内编译镜像,稍慢;之后复用缓存。策划本机不需要装 Go。)
& $StartPs1 -Mode docker
$rc = $LASTEXITCODE

Write-Host ''
if ($rc -eq 0) {
    Write-Ok '后端已启动!'
    Write-Host '  - 客户端网关(Envoy): https://127.0.0.1:8443' -ForegroundColor Green
    Write-Host '  - docker 模式 DS=mock,只能测登录/业务;要进真实 Hub/Battle DS 请用 -Battle。' -ForegroundColor Yellow
    Write-Host '  - 看运行状态:  pwsh tools/scripts/play.ps1 -Status' -ForegroundColor DarkGray
    Write-Host '  - 停止:        pwsh tools/scripts/play.ps1 -Stop' -ForegroundColor DarkGray
    Maybe-OpenUeClient
} else {
    Write-Err '启动过程中出错了,请把上面的红色 [ERR] 信息发给后端同学。'
}
exit $rc
