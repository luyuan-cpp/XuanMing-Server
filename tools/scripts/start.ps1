<#
.SYNOPSIS
  Pandora 后端一键启动器(策划/开发都能用)。

.DESCRIPTION
  一条命令把后端跑起来,覆盖 5 套环境(DS 分配模式随环境变):
    local    本地 windows 调试 —— 基础设施在 docker,19 个 go 服务以宿主进程跑(可断点);DS=local(Windows PandoraServer.exe)
    docker   本地 docker 启动   —— 基础设施 + 19 个 go 服务全跑在本机 docker;DS=mock(容器内无真 DS)
    intranet 内网测试服     —— 同 docker 全容器,但绑定内网 IP 供多人联调;DS=mock
    online   线上 k8s 集群   —— kustomize 部署到远端 k8s + Agones 真 Linux DS;DS=agones
                             用 -Env test|prod 区分「测试服集群」与「生产 kbs 集群」(不同 kube-context)

  还有两个本地联调辅助模式:
    battle   含战斗混合版 —— 17 个业务服务跑 docker,ds_allocator + hub_allocator 跑宿主(需 exec Windows DS);
                             进真实 Hub/Battle DS。play.ps1 -Battle[ -Intranet] 走这个;策划机不用起 go 服务。
    k8s      本地 minikube 联调 Agones —— 本机起 minikube + Agones,验证真 Linux DS 链路;DS=agones(advertise 127.0.0.1 + udp_relay.ps1 回程)

  启动前会检查必要工具(go / docker / kubectl / minikube)。默认只提示缺失项,不改本机环境;
  只有显式传 -Install 才会尝试用 winget 安装。-Check 只检查不启动。

.EXAMPLE
  pwsh tools/scripts/start.ps1                      # 默认 local 模式(本地 windows 调试,全部服务)
  pwsh tools/scripts/start.ps1 -Mode docker
  pwsh tools/scripts/start.ps1 -Mode intranet                       # 内网测试服(全容器,绑内网 IP)
  pwsh tools/scripts/start.ps1 -Mode k8s                            # 本地 minikube + Agones 真 DS 联调
    pwsh tools/scripts/start.ps1 -Mode online -Env test  -Registry registry.mycorp.com -Tag v1.2.3 -BattleDsImage registry.mycorp.com/pandora/battle-ds:v1.2.3 -HubDsImage registry.mycorp.com/pandora/hub-ds:v1.2.3 -DsGatewayAddr pandora-envoy.pandora.svc:8444  # 线上测试服集群
    pwsh tools/scripts/start.ps1 -Mode online -Env prod  -Registry registry.mycorp.com -Tag v1.2.3 -BattleDsImage registry.mycorp.com/pandora/battle-ds:v1.2.3 -HubDsImage registry.mycorp.com/pandora/hub-ds:v1.2.3 -DsGatewayAddr pandora-envoy.pandora.svc:8444  # 线上生产 kbs 集群(双重确认)
  pwsh tools/scripts/start.ps1 -Mode docker -Down  # 停
  pwsh tools/scripts/start.ps1 -Status             # 看状态
  pwsh tools/scripts/start.ps1 -Check              # 只检查工具
  pwsh tools/scripts/start.ps1 -Install            # 缺工具时才尝试 winget 安装

.EXAMPLE
  # 电脑重启后『快速恢复』上次的环境(不重建镜像,把停掉的集群/容器拉回来):
  pwsh tools/scripts/start.ps1 -Mode k8s    -Resume   # minikube start + 等 Pod;之后再跑 e2e_k8s.ps1
  pwsh tools/scripts/start.ps1 -Mode docker -Resume   # docker compose up -d(不 --build)
  pwsh tools/scripts/start.ps1 -Mode local  -Resume   # 基础设施随 Docker 恢复 + 重起宿主 go 服务

  # 环境乱了想『一键重置』再全新起(彻底清掉旧状态):
  pwsh tools/scripts/start.ps1 -Mode k8s    -Reset    # minikube delete 后全新部署
  pwsh tools/scripts/start.ps1 -Mode docker -Reset    # 容器全清后重建启动
#>
[CmdletBinding()]
param(
    [ValidateSet('local', 'docker', 'intranet', 'battle', 'k8s', 'online')]
    [string]$Mode = 'local',

    # online 环境:test=测试服集群 / prod=生产 kbs 集群(不同 kube-context,prod 双重确认)
    [ValidateSet('test', 'prod')]
    [string]$Env = 'test',

    # intranet 对外广告 IP(内网其它机器连本机用;留空自动取本机内网 IPv4)
    [string]$AdvertiseHost = '',

    [switch]$Down,        # 停止该模式
    [switch]$Resume,      # 电脑重启后快速恢复:不重建镜像,把上次停掉的集群/容器拉回来
    [switch]$Reset,       # 一键重置:彻底清掉旧状态再全新启动(线上 online 模式禁用)
    [switch]$Status,      # 查看状态
    [switch]$Check,       # 只检查工具链,不启动
    [switch]$Install,     # 工具缺失时尝试 winget 安装(默认不安装;不含 Docker Desktop)
    [switch]$InstallDocker, # 显式同意用 winget 装 Docker Desktop(装前确认虚拟化;装后仍需重启+手动启动)
    [switch]$NoInstall,   # 兼容旧参数;等同于不传 -Install

    # online 模式参数
    [string]$Registry,    # 镜像仓库地址,如 registry.mycorp.com
    [string]$Tag,         # 镜像 tag,如 v1.2.3
    [string]$BattleDsImage, # online:战斗 DS 镜像(必须是远端集群可拉取的完整镜像名)
    [string]$HubDsImage,    # online:大厅 DS 镜像(必须是远端集群可拉取的完整镜像名)
    [string]$DsGatewayAddr, # online:DS 回调入口(如 pandora-envoy.pandora.svc:8444)
    [ValidateSet('0', '1')]
    [string]$DsGatewayTls = '1', # online:DS 回调是否 TLS(线上默认 1)
    [switch]$BuildPush    # online:本地构建并推送 19 个镜像到 -Registry(远端发布动作,需人工授权)
)

$ErrorActionPreference = 'Stop'
$ScriptDir   = $PSScriptRoot
$ProjectRoot = (Resolve-Path "$ScriptDir/../..").Path
$ComposeInfra    = Join-Path $ProjectRoot 'deploy/docker-compose.dev.yml'
$ComposeServices = Join-Path $ProjectRoot 'deploy/docker-compose.services.yml'
$EnvFile         = Join-Path $ProjectRoot 'deploy/env/dev.env'
$ClusterEtcDir   = Join-Path $ProjectRoot 'run/cluster/etc'
$K8sNamespace    = 'pandora'

# ===== 输出辅助 =====
function Write-Info($m) { Write-Host "[INFO] $m" -ForegroundColor Cyan }
function Write-Ok($m)   { Write-Host "[ OK ] $m" -ForegroundColor Green }
function Write-Skip($m) { Write-Host "[SKIP] $m" -ForegroundColor DarkGray }
function Write-Warn($m) { Write-Host "[WARN] $m" -ForegroundColor Yellow }
function Write-Err($m)  { Write-Host "[ERR ] $m" -ForegroundColor Red }
function Write-Step($m) { Write-Host "`n===== $m =====" -ForegroundColor Magenta }

# native 命令(kubectl/minikube/docker 等)fail-fast:执行后立刻检查 $LASTEXITCODE,
# 非 0 直接抛错中止,避免「某步骤失败但脚本继续往下跑、最后还打印 [OK]」的假成功。
function Assert-LastExit([string]$what) {
    if ($LASTEXITCODE -ne 0) { throw "$what 失败(exit=$LASTEXITCODE)" }
}

function Test-CommandExists([string]$cmd) {
    return [bool](Get-Command $cmd -ErrorAction SilentlyContinue)
}

# ===== 工具检查 + 显式安装 =====
# 返回 $true=就绪 / $false=缺失(未能装上)
function Ensure-Tool {
    param(
        [string]$Name,
        [string]$CheckCmd,
        [string]$WingetId,
        [string]$ManualUrl,
        [switch]$Required
    )
    if (Test-CommandExists $CheckCmd) {
        Write-Ok "$Name 已就绪"
        return $true
    }
    Write-Warn "$Name 未安装"
    if ($Check -or $NoInstall -or -not $Install) {
        if ($ManualUrl) { Write-Host "       手动安装:$ManualUrl" -ForegroundColor Yellow }
        if (-not $Check -and -not $NoInstall -and -not $Install) {
            Write-Host "       如需脚本尝试安装,请显式追加 -Install。" -ForegroundColor Yellow
        }
        return $false
    }
    if (-not $WingetId) {
        Write-Err "$Name 无法自动安装,请手动装:$ManualUrl"
        return $false
    }
    if (-not (Test-CommandExists 'winget')) {
        Write-Err "未找到 winget,无法自动安装 $Name;请手动装:$ManualUrl"
        return $false
    }
    Write-Info "  winget 安装 $Name ($WingetId) ..."
    winget install --id $WingetId --silent --accept-source-agreements --accept-package-agreements | Out-Null
    # winget 装完当前会话 PATH 可能没刷新
    if (Test-CommandExists $CheckCmd) {
        Write-Ok "$Name 安装成功"
        return $true
    }
    Write-Warn "$Name 已尝试安装,但当前终端还找不到命令 —— 多半是 PATH 未刷新。"
    Write-Warn "       请『新开一个终端』后重跑本脚本。"
    return $false
}

function Test-DockerRunning {
    if (-not (Test-CommandExists 'docker')) { return $false }
    docker info *> $null
    return ($LASTEXITCODE -eq 0)
}

# 确认 Docker Desktop 跑得起来的前置:CPU 虚拟化已开 + WSL2 可用。
# 返回 $true=就绪;$false=有缺项(已打印指引)。仅做只读检查,不改本机环境。
function Test-DockerPrereq {
    $ok = $true

    # ① CPU 虚拟化(BIOS/固件层)。Win32_Processor.VirtualizationFirmwareEnabled。
    try {
        $virt = (Get-CimInstance Win32_Processor -ErrorAction Stop |
                 Select-Object -First 1 -ExpandProperty VirtualizationFirmwareEnabled)
        if ($virt) {
            Write-Ok "CPU 虚拟化已开启(VirtualizationFirmwareEnabled=True)"
        } else {
            Write-Err "CPU 虚拟化未开启 —— 请进 BIOS 开 Intel VT-x / AMD-V 后再装 Docker。"
            $ok = $false
        }
    } catch {
        Write-Warn "无法读取 CPU 虚拟化状态($($_.Exception.Message));跳过该项检查。"
    }

    # ② WSL2(Docker Desktop 默认后端)。有 wsl 命令即视为可用;没有给出启用指引。
    if (Test-CommandExists 'wsl') {
        Write-Ok "WSL 命令可用(Docker Desktop 走 WSL2 后端)"
    } else {
        Write-Warn "未检测到 wsl 命令 —— Docker Desktop 首次启动会引导启用 WSL2。"
        Write-Host "       如启动失败,管理员执行:wsl --install  然后重启。" -ForegroundColor Yellow
    }

    return $ok
}

# winget 安装 Docker Desktop(仅在显式 -InstallDocker 时调用)。
# 注意:装完仍需『重启系统 + 手动启动 Docker Desktop + 新开终端』,脚本替不了这步。
function Install-DockerDesktop {
    if (-not (Test-CommandExists 'winget')) {
        Write-Err "未找到 winget,无法自动装 Docker Desktop;请手动装:https://www.docker.com/products/docker-desktop/"
        return $false
    }
    Write-Step "用 winget 安装 Docker Desktop"
    if (-not (Test-DockerPrereq)) {
        Write-Err "虚拟化前置未满足,已中止 Docker 安装(见上方指引)。"
        return $false
    }
    Write-Info "  winget install --id Docker.DockerDesktop ..."
    winget install --id Docker.DockerDesktop --silent --accept-source-agreements --accept-package-agreements | Out-Null
    if (Test-CommandExists 'docker') {
        Write-Ok "Docker Desktop 已安装。"
    } else {
        Write-Warn "Docker Desktop 已尝试安装,但当前终端还找不到 docker 命令(PATH 未刷新属正常)。"
    }
    Write-Warn "Docker Desktop 装完还需手动收尾:"
    Write-Host "       ① 重启系统(首次装基本都要)" -ForegroundColor Yellow
    Write-Host "       ② 启动 Docker Desktop,等鲸鱼图标变绿(daemon 起来)" -ForegroundColor Yellow
    Write-Host "       ③ 新开一个终端,重跑:pwsh tools/scripts/start.ps1 -Mode $Mode" -ForegroundColor Yellow
    return $false  # 本次不继续启动,交回控制权让用户重启
}

# 确保 docker 命令存在且 daemon 在跑。
# Docker Desktop 不随 -Install 自动装(需重启+引导);只有显式 -InstallDocker 才用 winget 装。
function Ensure-Docker {
    if (-not (Test-CommandExists 'docker')) {
        Write-Warn "Docker 未安装"
        if ($InstallDocker -and -not $Check) {
            return (Install-DockerDesktop)
        }
        Write-Host "       手动安装:https://www.docker.com/products/docker-desktop/" -ForegroundColor Yellow
        Write-Host "       或让脚本用 winget 装(装前确认虚拟化):追加 -InstallDocker" -ForegroundColor Yellow
        return $false
    }
    Write-Ok "Docker 已就绪"
    if ($Check) { return $true }
    if (-not (Test-DockerRunning)) {
        Write-Err "Docker 已装但 daemon 没在跑 —— 请启动 Docker Desktop 后重试。"
        return $false
    }
    Write-Ok "Docker daemon 运行中"
    return $true
}

function Ensure-Go {
    return (Ensure-Tool -Name 'Go' -CheckCmd 'go' -WingetId 'GoLang.Go' -ManualUrl 'https://go.dev/dl/ (需 1.26.4+)')
}

# 检查给定模式需要的工具;返回 $true=全就绪
function Resolve-Prerequisites([string]$mode) {
    Write-Step "检查必要工具($mode 模式)"
    $allOk = $true
    switch ($mode) {
        'local' {
            if (-not (Ensure-Go))     { $allOk = $false }
            if (-not (Ensure-Docker)) { $allOk = $false }
        }
        'docker' {
            if (-not (Ensure-Docker)) { $allOk = $false }
            # mkcert:Envoy 本地 TLS 证书自动签发 / 共享 CA 安装都靠它,缺了 dev_up 起不来 Envoy。
            if (-not (Ensure-Tool -Name 'mkcert' -CheckCmd 'mkcert' -WingetId 'FiloSottile.mkcert' -ManualUrl 'https://github.com/FiloSottile/mkcert#installation')) { $allOk = $false }
        }
        'intranet' {
            if (-not (Ensure-Docker)) { $allOk = $false }
            # 内网服务器要给局域网策划发 TLS 证书,mkcert 必备(自动签叶子证书 + 装全队共享 CA)。
            if (-not (Ensure-Tool -Name 'mkcert' -CheckCmd 'mkcert' -WingetId 'FiloSottile.mkcert' -ManualUrl 'https://github.com/FiloSottile/mkcert#installation')) { $allOk = $false }
        }
        'battle' {
            # 含战斗混合版:17 业务服务跑 docker,只有 ds/hub allocator 跑宿主(需 Go build 这 2 个 + exec Windows DS)。
            if (-not (Ensure-Go))     { $allOk = $false }
            if (-not (Ensure-Docker)) { $allOk = $false }
            # Envoy 本地 TLS 证书(客户端面 :8443 / 内网策划连接)靠 mkcert 自动签发 / 装共享 CA。
            if (-not (Ensure-Tool -Name 'mkcert' -CheckCmd 'mkcert' -WingetId 'FiloSottile.mkcert' -ManualUrl 'https://github.com/FiloSottile/mkcert#installation')) { $allOk = $false }
        }
        'k8s' {
            if (-not (Ensure-Docker)) { $allOk = $false }
            if (-not (Ensure-Tool -Name 'kubectl'  -CheckCmd 'kubectl'  -WingetId 'Kubernetes.kubectl'  -ManualUrl 'https://kubernetes.io/docs/tasks/tools/')) { $allOk = $false }
            if (-not (Ensure-Tool -Name 'minikube' -CheckCmd 'minikube' -WingetId 'Kubernetes.minikube' -ManualUrl 'https://minikube.sigs.k8s.io/docs/start/')) { $allOk = $false }
            if (-not (Ensure-Tool -Name 'helm'     -CheckCmd 'helm'     -WingetId 'Helm.Helm'           -ManualUrl 'https://helm.sh/docs/intro/install/')) { $allOk = $false }
        }
        'online' {
            if (-not (Ensure-Tool -Name 'kubectl' -CheckCmd 'kubectl' -WingetId 'Kubernetes.kubectl' -ManualUrl 'https://kubernetes.io/docs/tasks/tools/')) { $allOk = $false }
        }
    }
    return $allOk
}

# ===== local 模式(宿主 go 进程 + docker 基础设施)=====
function Invoke-Local {
    if ($Down) {
        & "$ScriptDir/dev_all.ps1" -Down
        return
    }
    Write-Step "local 模式:基础设施(docker) + 19 个 go 服务(宿主进程)"
    Write-Info "策划本地联调用这个;服务可在 VS Code 断点调试。"

    # docker 业务容器会占用同一批端口(50001-50022),与宿主 go 进程互斥。
    # 若上一轮跑过 docker / intranet(非战斗)模式,业务容器还在跑 → 宿主 hub_allocator 起来即
    # `listen tcp :50021: bind: 已被占用` 崩溃,进而拉不起本机 Hub DS(PandoraServer.exe),
    # 客户端登录后卡在连大厅。这里在起宿主进程前,先把 docker 业务容器停掉(与 Invoke-Docker 反向对称)。
    if (Test-Path $ComposeServices) {
        $svcContainers = @(docker compose -f $ComposeServices ps --quiet 2>$null | Where-Object { $_ })
        if ($svcContainers.Count -gt 0) {
            Write-Info "检测到 docker 业务容器在跑(会抢 50001-50022 端口),先停掉它们..."
            docker compose -f $ComposeServices down 2>$null | Out-Null
        }
    }

    & "$ScriptDir/dev_all.ps1"
}

# ===== docker 模式(全容器)=====
function Invoke-Docker {
    if ($Down) {
        Write-Step "停止 docker 业务服务"
        docker compose -f $ComposeServices down
        Write-Step "停止基础设施"
        & "$ScriptDir/dev_down.ps1"
        return
    }
    Write-Step "docker 模式:基础设施 + 19 个 go 服务全部容器化"

    # local 宿主进程会抢同一批端口,先停掉
    Write-Info "先停掉可能在跑的宿主 go 服务(避免端口冲突)..."
    & "$ScriptDir/run_services.ps1" -Action down 2>$null

    Write-Step "[1/3] 基础设施(建 pandora-net)"
    & "$ScriptDir/dev_up.ps1"
    if ($LASTEXITCODE -ne 0) { throw "基础设施启动失败" }

    Write-Step "[2/3] 生成集群版配置(allocator=mock:容器内无真 DS)"
    & "$ScriptDir/gen_cluster_config.ps1" -AllocatorMode mock

    Write-Step "[3/3] 构建带版本烙印的镜像并启动业务服务容器"
    # 走 Build-AllImages(带 git 版本 build-arg),再用已构建镜像编排,
    # 避免 compose --build 绕过版本烙印。镜像 tag 与 compose image: 一致。
    Build-AllImages
    docker compose -f $ComposeServices up -d
    if ($LASTEXITCODE -ne 0) { throw "业务服务容器启动失败" }

    Write-Host ""
    Write-Ok "docker 模式已启动。查看:docker compose -f deploy/docker-compose.services.yml ps"
}

# ===== intranet 模式(内网测试服:全容器,绑内网 IP 供多人联调)=====
# 与 docker 一致(基础设施 + 19 服务全容器,DS=mock),区别只是面向局域网:
#   - compose 端口已绑 0.0.0.0,内网其它机器可直接连本机内网 IP
#   - 打印内网访问地址,客户端把后端指向 <内网IP>:<port> 即可
function Resolve-LanIp {
    # 取本机对外那张网卡的 IPv4。关键:按默认路由(0.0.0.0/0)选网卡,
    # 避开 Docker/WSL/Hyper-V 虚拟网卡的 172.*/10.*/192.168.* 地址——否则内网客户端拿到不可达地址。
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

function Invoke-Intranet {
    if ($Down) { Invoke-Docker; return }

    $lan = if (-not [string]::IsNullOrWhiteSpace($AdvertiseHost)) { $AdvertiseHost } else { Resolve-LanIp }
    Write-Step "intranet 模式:内网测试服(全容器,内网 IP = $lan)"
    if ([string]::IsNullOrWhiteSpace($lan)) {
        Write-Warn "未能自动解析内网 IPv4,可用 -AdvertiseHost 显式指定。继续以 docker 全容器方式启动。"
    }

    # 复用 docker 全容器启动路径(基础设施 + 服务容器,allocator=mock)
    Invoke-Docker

    Write-Host ""
    Write-Ok "内网测试服已启动。其它机器把客户端后端指向:"
    if (-not [string]::IsNullOrWhiteSpace($lan)) {
        Write-Host "       客户端面(TLS)  https://${lan}:8443" -ForegroundColor Green
        Write-Host "       DS 面          ${lan}:8444" -ForegroundColor Green
    }
    Write-Warn "DS=mock(无真实 DS);需真实战斗/大厅 DS 请用 -Mode online(Agones)。"
}

# ===== battle 模式(含战斗混合版:17 业务服务跑 docker + ds/hub allocator 跑宿主)=====
# 为什么混合:战斗/大厅 DS 是 Windows PandoraServer.exe,跑不进 Linux 容器;
# ds_allocator(mode=local)与 hub_allocator 要在本机 exec 这个 .exe,故这 2 个服务必须留宿主,
# 其余 17 个纯业务服务进 docker(策划服务器机器不用为它们装 Go / 逐个 build)。
# 网络:Envoy(容器)所有上游本就走 host.docker.internal:500XX,17 个容器把端口发布到宿主、
# 2 个 allocator 直接绑宿主端口 —— 从 Envoy / 跨边界视角两者等价。容器里的 matchmaker/login/
# battle_result 经 gen_cluster_config.ps1 -HostAllocators 把 allocator 地址改指 host.docker.internal。
# DS 广告地址(PANDORA_DS_ADVERTISE_HOST,本机自测=127.0.0.1 / 内网=局域网 IP)由 play.ps1 注入环境变量,
# 宿主 allocator 子进程继承 —— 与旧 local 战斗链路一致。
function Invoke-Battle {
    # compose 服务名(连字符)。这 2 个改跑宿主,不进容器。
    $hostAllocCompose = @('ds-allocator', 'hub-allocator')
    $containerSvcs = @((Get-ServiceList | Where-Object { $hostAllocCompose -notcontains $_.Name }).Name)

    if ($Down) {
        Write-Step "停止含战斗版(宿主 allocator + 业务容器 + 基础设施)"
        & "$ScriptDir/run_services.ps1" -Action down 2>$null
        docker compose -f $ComposeServices down
        & "$ScriptDir/dev_down.ps1"
        return
    }

    Write-Step "battle 模式:17 个业务服务(docker) + ds/hub allocator(宿主,exec Windows DS)"

    # 清理可能冲突的残留:上一轮 local 的宿主 go 进程、上一轮 docker 的 allocator 容器(会抢 50020/50021)。
    Write-Info "先停掉可能残留的宿主 go 服务与业务容器(避免端口冲突)..."
    & "$ScriptDir/run_services.ps1" -Action down 2>$null
    docker compose -f $ComposeServices down 2>$null | Out-Null

    Write-Step "[1/5] 基础设施(建 pandora-net)"
    & "$ScriptDir/dev_up.ps1"
    if ($LASTEXITCODE -ne 0) { throw "基础设施启动失败" }

    Write-Step "[2/5] 生成集群版配置(allocator 跑宿主:matchmaker/login/battle_result 经 host.docker.internal 回连)"
    & "$ScriptDir/gen_cluster_config.ps1" -HostAllocators

    Write-Step "[3/5] 构建 17 个业务服务镜像(不含 ds/hub allocator)"
    Build-AllImages -Only $containerSvcs

    Write-Step "[4/5] 启动 17 个业务服务容器"
    docker compose -f $ComposeServices up -d @containerSvcs
    if ($LASTEXITCODE -ne 0) { throw "业务服务容器启动失败" }

    Write-Step "[5/5] 宿主启动 ds_allocator + hub_allocator(mode=local,匹配成局 exec Windows DS)"
    # run_services 用下划线服务名;mode=local 从 etc/*-dev.yaml 读,DS 广告地址走 play.ps1 注入的环境变量。
    & "$ScriptDir/run_services.ps1" -Only ds_allocator, hub_allocator
    if ($LASTEXITCODE -ne 0) { throw "宿主 allocator 启动失败" }

    Write-Host ""
    Write-Ok "battle 模式已启动:17 业务容器 + 2 宿主 allocator。查看:pwsh tools/scripts/start.ps1 -Mode battle -Status"
}


# ===== 共享:apply Agones(RBAC + Fleet),可选安装 Agones(minikube 本地用)=====
# 让 agones 链路端到端可用:RBAC 给 allocator in-cluster token 调 Agones API 的权限,
# Fleet(pandora-battle / pandora-hub)提供真实 Linux DS。namespace 须先存在(调用方保证)。
function Apply-AgonesManifests {
    param(
        [switch]$InstallAgones,
        [string]$BattleDsImage = '',
        [string]$HubDsImage = '',
        [string]$DsGatewayAddr = '',
        [string]$DsGatewayTls = ''
    )
    $agonesDir = Join-Path $ProjectRoot 'deploy/k8s/agones'

    function Set-YamlEnvValue([string]$text, [string]$name, [string]$value) {
        # 用显式 ${1}/${2} 分组语法:避免 value 以数字开头时(如 TLS=1)$1+值拼成 $11 被当作第 11 组
        $pattern = '(?ms)(- name:\s*' + [regex]::Escape($name) + '\r?\n\s*value:\s*")[^"]+(")'
        return [regex]::Replace($text, $pattern, ('${1}' + $value + '${2}'))
    }

    function Apply-FleetManifest([string]$fileName, [string]$image, [string[]]$addrEnvNames, [string]$tlsEnvName) {
        $src = Join-Path $agonesDir $fileName
        if ([string]::IsNullOrWhiteSpace($image) -and [string]::IsNullOrWhiteSpace($DsGatewayAddr) -and [string]::IsNullOrWhiteSpace($DsGatewayTls)) {
            kubectl apply -f $src
            Assert-LastExit "kubectl apply Fleet $fileName"
            return
        }

        $raw = Get-Content $src -Raw
        if (-not [string]::IsNullOrWhiteSpace($image)) {
            # Fleet 容器只有一行 image:(imagePullPolicy 不含 image:),全文唯一,直接替换
            $raw = [regex]::Replace($raw, 'image:[^\S\r\n]*\S+', ('image: ' + $image))
        }
        if (-not [string]::IsNullOrWhiteSpace($DsGatewayAddr)) {
            foreach ($envName in $addrEnvNames) {
                $raw = Set-YamlEnvValue $raw $envName $DsGatewayAddr
            }
        }
        if (-not [string]::IsNullOrWhiteSpace($DsGatewayTls)) {
            $raw = Set-YamlEnvValue $raw $tlsEnvName $DsGatewayTls
        }

        $tmp = Join-Path ([System.IO.Path]::GetTempPath()) ([System.IO.Path]::GetRandomFileName() + '-' + $fileName)
        [System.IO.File]::WriteAllText($tmp, $raw, (New-Object System.Text.UTF8Encoding($false)))
        try {
            kubectl apply -f $tmp
            Assert-LastExit "kubectl apply Fleet $fileName"
        } finally {
            Remove-Item $tmp -ErrorAction SilentlyContinue
        }
    }

    if ($InstallAgones) {
        kubectl get ns agones-system *> $null
        if ($LASTEXITCODE -ne 0) {
            Write-Info "安装 Agones(helm,装到 agones-system)..."
            helm repo add agones https://agones.dev/chart/stable 2>$null | Out-Null
            helm repo update 2>$null | Out-Null
            kubectl create namespace agones-system 2>$null | Out-Null
            helm install agones agones/agones --namespace agones-system --wait
            if ($LASTEXITCODE -ne 0) { throw "Agones 安装失败" }
        } else {
            Write-Ok "Agones 已安装(agones-system 存在)"
        }
    }

    Write-Info "apply Agones RBAC(pandora-allocator)..."
    kubectl apply -f (Join-Path $agonesDir '10-rbac-allocator.yaml')
    Assert-LastExit 'kubectl apply Agones RBAC'
    Write-Info "apply Fleet(pandora-battle / pandora-hub 真 Linux DS)..."
    Apply-FleetManifest '20-fleet-battle.yaml' $BattleDsImage @('PANDORA_DS_ALLOCATOR_ADDR', 'PANDORA_PLAYER_LOCATOR_ADDR', 'PANDORA_BATTLE_RESULT_ADDR') 'PANDORA_DS_ALLOCATOR_TLS'
    Apply-FleetManifest '30-fleet-hub.yaml' $HubDsImage @('PANDORA_HUB_ALLOCATOR_ADDR', 'PANDORA_PLAYER_LOCATOR_ADDR') 'PANDORA_DS_ALLOCATOR_TLS'
    Write-Warn "Fleet 用真 UE DS 镜像(pandora/battle-ds:dev / pandora/hub-ds:dev)。"
    Write-Warn "  这些镜像由 UE 侧 Tool/Server/Agones 构建;minikube 需先 minikube image load,线上需 push 到 -Registry。"
}

# ===== k8s 模式(本地 minikube)=====
function Invoke-K8s {
    $servicesDir = Join-Path $ProjectRoot 'deploy/k8s/services'
    $infraYaml   = Join-Path $ProjectRoot 'deploy/k8s/infra/infra.yaml'
    $mysqlInit   = Join-Path $ProjectRoot 'deploy/mysql-init'

    if ($Down) {
        Write-Step "删除 k8s 业务服务 + 基础设施"
        kubectl delete -k $servicesDir --ignore-not-found 2>$null
        kubectl delete -f $infraYaml --ignore-not-found 2>$null
        Write-Info "minikube 仍在运行;彻底关:minikube stop"
        return
    }

    Write-Step "k8s 模式:minikube 本地集群"

    # 1) minikube 起没起
    minikube status *> $null
    if ($LASTEXITCODE -ne 0) {
        Write-Info "启动 minikube(driver=docker)..."
        minikube start --driver=docker --cpus=4 --memory=6144
        if ($LASTEXITCODE -ne 0) { throw "minikube 启动失败" }
    } else {
        Write-Ok "minikube 已在运行"
    }

    Write-Step "[1/7] namespace"
    kubectl apply -f (Join-Path $servicesDir '00-namespace.yaml')
    Assert-LastExit 'kubectl apply namespace'

    Write-Step "[2/7] 生成集群版配置 + ConfigMap(allocator=agones,advertise 127.0.0.1)"
    # 本地 minikube docker driver:Pod IP 客户端连不到,advertise 127.0.0.1 + udp_relay.ps1 回程
    & "$ScriptDir/gen_cluster_config.ps1" -AllocatorMode agones -AllocatorAdvertiseHost 127.0.0.1
    kubectl create configmap pandora-config --from-file=$ClusterEtcDir -n $K8sNamespace `
        --dry-run=client -o yaml | kubectl apply -f -
    Assert-LastExit 'kubectl apply configmap pandora-config'
    kubectl create configmap pandora-mysql-init --from-file=$mysqlInit -n $K8sNamespace `
        --dry-run=client -o yaml | kubectl apply -f -
    Assert-LastExit 'kubectl apply configmap pandora-mysql-init'

    Write-Step "[3/7] 基础设施(mysql/redis/zookeeper/kafka/etcd)"
    kubectl apply -f $infraYaml
    Assert-LastExit 'kubectl apply infra'
    Write-Info "等待基础设施就绪(最多 180s)..."
    kubectl rollout status deploy/mysql     -n $K8sNamespace --timeout=180s; Assert-LastExit 'mysql 就绪'
    kubectl rollout status deploy/redis     -n $K8sNamespace --timeout=120s; Assert-LastExit 'redis 就绪'
    kubectl rollout status deploy/etcd      -n $K8sNamespace --timeout=120s; Assert-LastExit 'etcd 就绪'
    # zookeeper / kafka 必须就绪,否则 player/push/battle-result 会因连不上 kafka:9092 CrashLoop
    kubectl rollout status deploy/zookeeper -n $K8sNamespace --timeout=120s; Assert-LastExit 'zookeeper 就绪'
    kubectl rollout status deploy/kafka     -n $K8sNamespace --timeout=180s; Assert-LastExit 'kafka 就绪'

    Write-Step "[4/7] 安装 Agones + apply RBAC/Fleet(真 Linux DS)"
    Apply-AgonesManifests -InstallAgones

    Write-Step "[5/7] 构建 19 个服务镜像"
    Build-AllImages

    Write-Step "[6/7] 把镜像 load 进 minikube"
    foreach ($img in (Get-ServiceImages)) {
        Write-Info "  minikube image load $img"
        minikube image load $img
        Assert-LastExit "minikube image load $img"
    }

    Write-Step "[7/7] 部署业务服务"
    kubectl apply -k $servicesDir
    Assert-LastExit 'kubectl apply -k services'
    # 镜像 tag 固定为 :dev,重建/重 load 后 image 字符串不变 -> apply 报 unchanged,旧 Pod 不会换。
    # 按名强制滚动重启这 19 个业务 Deployment(不碰 infra,避免重启 kafka 又触发依赖服务 CrashLoop),
    # 确保跑的是刚 build 的新二进制。
    Write-Info "rollout restart 业务 Deployment(同 :dev tag 重建后强制换 Pod)..."
    foreach ($svc in (Get-ServiceList)) {
        kubectl rollout restart deploy/$($svc.Name) -n $K8sNamespace
        Assert-LastExit "rollout restart $($svc.Name)"
    }

    Write-Host ""
    Write-Ok "k8s 模式已部署。查看:kubectl get pods -n $K8sNamespace"
    Write-Warn "真 DS 闭环还需起宿主 Envoy 桥接 + UDP 中继:pwsh tools/scripts/e2e_k8s.ps1"
}

# ===== online 模式(远端 k8s:-Env test 测试服集群 / prod 生产 kbs 集群)=====
function Invoke-Online {
    $overlay     = Join-Path $ProjectRoot 'deploy/k8s/overlays/online'
    $overlayFile = Join-Path $overlay 'kustomization.yaml'

    # 安全:确认当前 kube-context(线上误操作代价高;prod 再加一道确认)
    $ctx = (kubectl config current-context) 2>$null
    Write-Step "online 模式:-Env $Env  目标 kube-context = $ctx"
    if ($Env -eq 'prod') {
        Write-Warn "⚠️ 这是【生产 kbs 集群】部署。请确认当前 context『$ctx』确为生产集群。"
    } else {
        Write-Info "这是【测试服集群】部署。"
    }
    Write-Warn "这会对『$ctx』集群做变更。确认无误请输入该 context 名字以继续:"
    $confirm = Read-Host "  输入 context 名"
    if ($confirm -ne $ctx) {
        Write-Err "输入与当前 context 不一致,已中止(防误操作)。"
        return
    }
    if ($Env -eq 'prod') {
        $p = Read-Host "  生产环境二次确认,请输入大写 PROD 继续"
        if ($p -ne 'PROD') { Write-Err "生产二次确认失败,已中止。"; return }
    }

    if ($Down) {
        Write-Step "删除 online 业务服务($Env)"
        kubectl delete -k $overlay --ignore-not-found
        return
    }

    if (-not $Registry -or -not $Tag) {
        throw "online 模式必须指定 -Registry 和 -Tag(Go 服务镜像来源)。"
    }
    if (-not $BattleDsImage -or -not $HubDsImage -or -not $DsGatewayAddr) {
        throw "online 模式必须指定 -BattleDsImage / -HubDsImage / -DsGatewayAddr，避免把本地 Fleet 的 image/host.docker.internal 带到远端集群。"
    }

    if ($BuildPush) {
        Write-Step "构建并推送 19 个 Go 服务镜像到 $Registry"
        Build-AllImages
        foreach ($svc in (Get-ServiceList)) {
            $local  = "pandora/$($svc.Name):dev"
            $remote = "$Registry/pandora/$($svc.Name):$Tag"
            docker tag $local $remote
            docker push $remote
            if ($LASTEXITCODE -ne 0) { throw "推送失败:$remote" }
        }
    }

    Write-Step "生成集群版配置 + ConfigMap(namespace $K8sNamespace,allocator=agones)"
    # 线上真集群:Pod IP 可路由,advertise 留空用 GameServer status.address 直连
    & "$ScriptDir/gen_cluster_config.ps1" -AllocatorMode agones
    kubectl apply -f (Join-Path $ProjectRoot 'deploy/k8s/services/00-namespace.yaml')
    kubectl create configmap pandora-config --from-file=$ClusterEtcDir -n $K8sNamespace `
        --dry-run=client -o yaml | kubectl apply -f -

    Write-Step "apply Agones RBAC + Fleet(真 Linux DS)"
    # 线上 Agones 通常已由集群管理员预装;此处不自动 helm install,只 apply 业务 RBAC/Fleet
    kubectl get ns agones-system *> $null
    if ($LASTEXITCODE -ne 0) {
        Write-Warn "未检测到 agones-system —— 线上 Agones 须由集群管理员预先安装,否则 Fleet/分配不可用。"
    }
    Apply-AgonesManifests -BattleDsImage $BattleDsImage -HubDsImage $HubDsImage -DsGatewayAddr $DsGatewayAddr -DsGatewayTls $DsGatewayTls

    # 用 -Registry/-Tag 临时覆盖 overlay 占位镜像(try/finally 还原,保持仓库干净)
    $orig = Get-Content $overlayFile -Raw
    try {
        $patched = $orig.Replace('registry.example.com', $Registry) -replace 'newTag: latest', "newTag: $Tag"
        [System.IO.File]::WriteAllText($overlayFile, $patched, (New-Object System.Text.UTF8Encoding($false)))
        Write-Step "kubectl apply -k overlays/online($Env)"
        kubectl apply -k $overlay
    } finally {
        [System.IO.File]::WriteAllText($overlayFile, $orig, (New-Object System.Text.UTF8Encoding($false)))
    }

    Write-Host ""
    Write-Ok "online($Env)部署已提交。查看:kubectl get pods -n $K8sNamespace"
}

# ===== 共享:服务清单 / 镜像构建 =====
function Get-ServiceList {
    @(
        @{ Name = 'login';          Dir = 'services/account/login';            Cmd = 'login' }
        @{ Name = 'player';         Dir = 'services/account/player';           Cmd = 'player' }
        @{ Name = 'data-service';   Dir = 'services/data/data_service';        Cmd = 'data_service' }
        @{ Name = 'friend';         Dir = 'services/social/friend';            Cmd = 'friend' }
        @{ Name = 'chat';           Dir = 'services/social/chat';              Cmd = 'chat' }
        @{ Name = 'guild';          Dir = 'services/social/guild';             Cmd = 'guild' }
        @{ Name = 'mail';           Dir = 'services/social/mail';              Cmd = 'mail' }
        @{ Name = 'player-locator'; Dir = 'services/runtime/player_locator';   Cmd = 'locator' }
        @{ Name = 'leaderboard';    Dir = 'services/runtime/leaderboard';      Cmd = 'leaderboard' }
        @{ Name = 'team';           Dir = 'services/matchmaking/team';         Cmd = 'team' }
        @{ Name = 'matchmaker';     Dir = 'services/matchmaking/matchmaker';   Cmd = 'matchmaker' }
        @{ Name = 'trade';          Dir = 'services/economy/trade';            Cmd = 'trade' }
        @{ Name = 'dialogue';       Dir = 'services/social/dialogue';          Cmd = 'dialogue' }
        @{ Name = 'push';           Dir = 'services/runtime/push';             Cmd = 'push' }
        @{ Name = 'inventory';      Dir = 'services/economy/inventory';        Cmd = 'inventory' }
        @{ Name = 'auction';        Dir = 'services/economy/auction';          Cmd = 'auction' }
        @{ Name = 'ds-allocator';   Dir = 'services/battle/ds_allocator';      Cmd = 'ds_allocator' }
        @{ Name = 'hub-allocator';  Dir = 'services/battle/hub_allocator';     Cmd = 'hub_allocator' }
        @{ Name = 'battle-result';  Dir = 'services/battle/battle_result';     Cmd = 'battle_result' }
    )
}

function Get-ServiceImages {
    Get-ServiceList | ForEach-Object { "pandora/$($_.Name):dev" }
}

# 从 git 推导版本烙印信息(编译期注入二进制,实现「线上跑的 ↔ git 某次提交」可追溯)。
# git 不可用 / 不是 git 仓库时回退占位值,不阻断构建。
function Get-VersionInfo {
    $ver    = 'dev'
    $commit = 'unknown'
    $built  = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
    if (Test-CommandExists 'git') {
        Push-Location $ProjectRoot
        try {
            $d = (git describe --tags --always --dirty 2>$null)
            if ($LASTEXITCODE -eq 0 -and $d) { $ver = $d.Trim() }
            $c = (git rev-parse --short HEAD 2>$null)
            if ($LASTEXITCODE -eq 0 -and $c) { $commit = $c.Trim() }
        } finally {
            Pop-Location
        }
    }
    return [pscustomobject]@{ Version = $ver; Commit = $commit; BuildTime = $built }
}

function Build-AllImages {
    param([string[]]$Only = @())
    $dockerfile = Join-Path $ProjectRoot 'deploy/services/Dockerfile'
    $v = Get-VersionInfo
    Write-Info "  版本烙印:version=$($v.Version) commit=$($v.Commit) built=$($v.BuildTime)"
    $list = Get-ServiceList
    # -Only 非空时只构建指定服务(含战斗混合模式不构建 ds/hub allocator 镜像,它们跑宿主)
    if ($Only.Count -gt 0) { $list = $list | Where-Object { $Only -contains $_.Name } }
    foreach ($svc in $list) {
        Write-Info "  docker build pandora/$($svc.Name):dev ..."
        docker build -f $dockerfile `
            --build-arg "SERVICE_DIR=$($svc.Dir)" `
            --build-arg "CMD_NAME=$($svc.Cmd)" `
            --build-arg "VERSION=$($v.Version)" `
            --build-arg "GIT_COMMIT=$($v.Commit)" `
            --build-arg "BUILD_TIME=$($v.BuildTime)" `
            -t "pandora/$($svc.Name):dev" $ProjectRoot
        if ($LASTEXITCODE -ne 0) { throw "镜像构建失败:$($svc.Name)" }
    }
}

# ===== 电脑重启后快速恢复(不重建镜像,尽量把上次状态拉回来)=====
# k8s:minikube stop/重启只是停容器,集群状态+已 load 镜像都还在磁盘,minikube start 即恢复,Pod 自动重建。
function Resume-K8s {
    Write-Step "k8s 快速恢复(电脑重启后:只拉起 minikube + 等 Pod,不重建镜像)"
    minikube status *> $null
    if ($LASTEXITCODE -ne 0) {
        Write-Info "minikube 已停,minikube start 中(集群状态/镜像都在磁盘上,Pod 会自动恢复)..."
        minikube start --driver=docker --cpus=4 --memory=6144
        if ($LASTEXITCODE -ne 0) { throw "minikube 启动失败(若集群已损坏,改用 -Reset 全新部署)" }
    } else {
        Write-Ok "minikube 已在运行"
    }
    Write-Info "等待关键业务 Pod 就绪..."
    try {
        kubectl rollout status deploy/login         -n $K8sNamespace --timeout=180s; Assert-LastExit 'login 恢复就绪'
        kubectl rollout status deploy/ds-allocator  -n $K8sNamespace --timeout=120s; Assert-LastExit 'ds-allocator 恢复就绪'
        kubectl rollout status deploy/hub-allocator -n $K8sNamespace --timeout=120s; Assert-LastExit 'hub-allocator 恢复就绪'
    } catch {
        Write-Err "关键 Deployment 未就绪/不存在($($_.Exception.Message))。"
        Write-Err "集群多半未部署过或被清过,-Resume 无可恢复对象。请改用全新部署:"
        Write-Err "  pwsh tools/scripts/start.ps1 -Mode k8s"
        exit 1
    }
    Write-Host ""
    Write-Ok "集群已恢复。接着跑真 DS 闭环:pwsh tools/scripts/e2e_k8s.ps1"
}

function Invoke-Resume {
    Write-Step "$Mode 快速恢复(电脑重启后:尽量不重建,直接把上次的状态拉回来)"
    switch ($Mode) {
        'k8s' { Resume-K8s }
        'local' {
            Write-Info "基础设施容器随 Docker Desktop 自动恢复;这里重新拉起宿主 go 服务。"
            Invoke-Local
        }
        'battle' {
            Write-Info "含战斗混合版恢复:重新拉起 17 业务容器 + 2 宿主 allocator。"
            Invoke-Battle
        }
        { $_ -in 'docker', 'intranet' } {
            Write-Info "重启已停的容器(不加 --build,不重建镜像)..."
            & "$ScriptDir/dev_up.ps1"
            docker compose -f $ComposeServices up -d
            Write-Ok "$Mode 容器已恢复。"
            if ($Mode -eq 'intranet') {
                $lan = if (-not [string]::IsNullOrWhiteSpace($AdvertiseHost)) { $AdvertiseHost } else { Resolve-LanIp }
                if (-not [string]::IsNullOrWhiteSpace($lan)) {
                    Write-Host "       内网地址  https://${lan}:8443 / ${lan}:8444" -ForegroundColor Green
                }
            }
        }
        'online' { Write-Err "online 是远端集群,Pod 由集群自管,无需本机恢复。"; exit 1 }
    }
}

# ===== 一键重置:彻底清掉旧状态,再全新启动 =====
function Invoke-Reset {
    Write-Step "$Mode 一键重置:先彻底清理,再全新启动"
    switch ($Mode) {
        'k8s' {
            Write-Warn "将 minikube delete(销毁整个本地集群,已 load 镜像一并清掉),然后全新部署。"
            minikube delete 2>$null
            Invoke-K8s
        }
        'local' {
            & "$ScriptDir/dev_all.ps1" -Down 2>$null
            Invoke-Local
        }
        'battle' {
            & "$ScriptDir/run_services.ps1" -Action down 2>$null
            docker compose -f $ComposeServices down -v 2>$null
            & "$ScriptDir/dev_down.ps1" 2>$null
            Invoke-Battle
        }
        'docker' {
            docker compose -f $ComposeServices down -v 2>$null
            & "$ScriptDir/dev_down.ps1" 2>$null
            Invoke-Docker
        }
        'intranet' {
            docker compose -f $ComposeServices down -v 2>$null
            & "$ScriptDir/dev_down.ps1" 2>$null
            Invoke-Intranet
        }
        'online' { Write-Err "online 模式禁用 -Reset(线上集群不做销毁式重置);如需重发请用正常部署流程。"; exit 1 }
    }
}

# ===== 状态 =====
function Show-Status {
    switch ($Mode) {
        'local'  { & "$ScriptDir/run_services.ps1" -Action status }
        'battle' {
            Write-Step "业务服务容器(17 个,不含 ds/hub allocator)"
            docker compose -f $ComposeServices ps
            Write-Step "宿主 allocator + 其它宿主 go 进程"
            & "$ScriptDir/run_services.ps1" -Action status
        }
        { $_ -in 'docker', 'intranet' } {
            Write-Step "docker 业务服务"
            docker compose -f $ComposeServices ps
            Write-Step "基础设施"
            docker compose -f $ComposeInfra --env-file $EnvFile ps
        }
        { $_ -in 'k8s', 'online' } {
            kubectl get pods,svc -n $K8sNamespace
        }
    }
}

# ===== 主流程 =====
Write-Host ""
Write-Host "============================================" -ForegroundColor Magenta
Write-Host " Pandora 后端一键启动器  ( $Mode )" -ForegroundColor Magenta
Write-Host "============================================" -ForegroundColor Magenta

if ($Status) { Show-Status; exit 0 }

$prereqOk = Resolve-Prerequisites $Mode

if ($Check) {
    Write-Host ""
    if ($prereqOk) { Write-Ok "$Mode 模式所需工具全部就绪。"; exit 0 }
    else { Write-Warn "$Mode 模式有工具缺失,见上方提示。"; exit 1 }
}

if (-not $prereqOk) {
    Write-Err "工具未就绪,已中止。装好后重跑(或新开终端刷新 PATH)。"
    exit 1
}

if ($Reset)  { Invoke-Reset;  exit 0 }
if ($Resume) { Invoke-Resume; exit 0 }

switch ($Mode) {
    'local'    { Invoke-Local }
    'docker'   { Invoke-Docker }
    'intranet' { Invoke-Intranet }
    'battle'   { Invoke-Battle }
    'k8s'      { Invoke-K8s }
    'online'   { Invoke-Online }
}
