# Pandora 集群版配置生成器
#
# 把各服务的 etc/<svc>-dev.yaml(地址都是 127.0.0.1)转换成「集群版」配置:
# mysql/redis/kafka/etcd 与同伴服务的地址改成容器/Service 短名,allocator 的
# mode: "local"(本机 exec DS)改成 "mock"(容器内无 PandoraServer.exe)。
#
# 同一份产物 docker 与 k8s 共用:
#   - docker-compose.services.yml 里服务名 = mysql/redis/kafka/etcd/login/...
#   - k8s 同 namespace 内 Service 短名 = mysql/redis/kafka/etcd/login/...
# 两边都能用短名解析,所以生成的 endpoint 一致。
#
# 用法:
#   pwsh tools/scripts/gen_cluster_config.ps1                                  # 生成到 run/cluster/etc(allocator=mock)
#   pwsh tools/scripts/gen_cluster_config.ps1 -OutDir <dir>                     # 自定义输出目录
#   pwsh tools/scripts/gen_cluster_config.ps1 -AllocatorMode agones            # 线上/Agones 链路:真 Linux DS
#   pwsh tools/scripts/gen_cluster_config.ps1 -AllocatorMode agones -AllocatorAdvertiseHost 127.0.0.1
#                                                                              # 本地 minikube(docker driver)+ udp-relay 回程
#
# 三条链路与 allocator 模式的对应(由 start.ps1 驱动):
#   本地 windows (-Mode local)  → dev yaml 原样 mode=local,不过本生成器(宿主 exec Windows DS)
#   docker        (-Mode docker) → -AllocatorMode mock  (容器内无真 DS,假地址只测后端链路)
#   线上 k8s     (-Mode online) → -AllocatorMode agones(GameServer status.address 直连真 Linux DS)
#   本地 k8s     (-Mode k8s)    → -AllocatorMode agones -AllocatorAdvertiseHost 127.0.0.1 + udp_relay.ps1

[CmdletBinding()]
param(
    [string]$OutDir,
    [ValidateSet('mock', 'agones')]
    [string]$AllocatorMode = 'mock',
    [string]$AllocatorAdvertiseHost = ''
)

$ErrorActionPreference = 'Stop'
$ProjectRoot = (Resolve-Path "$PSScriptRoot/../..").Path
if (-not $OutDir) { $OutDir = Join-Path $ProjectRoot 'run/cluster/etc' }

# ===== 服务清单(name; 相对 dev 配置路径)=====
# port 用于把同伴服务的 127.0.0.1:<port> 换成 <svc>:<port>(端口不变,只换 host)。
# Name 用「连字符」形式:同时满足 docker-compose 服务名与 k8s Service 名(k8s 禁止下划线),
# docker / k8s 两边据此短名解析,所以同一份产物通用。
$Services = @(
    @{ Name = 'login';          Conf = 'services/account/login/etc/login-dev.yaml';                Port = 50001 }
    @{ Name = 'player';         Conf = 'services/account/player/etc/player-dev.yaml';              Port = 50002 }
    @{ Name = 'data-service';   Conf = 'services/data/data_service/etc/data_service-dev.yaml';     Port = 50003 }
    @{ Name = 'friend';         Conf = 'services/social/friend/etc/friend-dev.yaml';               Port = 50004 }
    @{ Name = 'chat';           Conf = 'services/social/chat/etc/chat-dev.yaml';                   Port = 50005 }
    @{ Name = 'player-locator'; Conf = 'services/runtime/player_locator/etc/locator-dev.yaml';     Port = 50006 }
    @{ Name = 'team';           Conf = 'services/matchmaking/team/etc/team-dev.yaml';              Port = 50010 }
    @{ Name = 'matchmaker';     Conf = 'services/matchmaking/matchmaker/etc/matchmaker-dev.yaml';  Port = 50011 }
    @{ Name = 'trade';          Conf = 'services/economy/trade/etc/trade-dev.yaml';                Port = 50012 }
    @{ Name = 'dialogue';       Conf = 'services/social/dialogue/etc/dialogue-dev.yaml';           Port = 50013 }
    @{ Name = 'push';           Conf = 'services/runtime/push/etc/push-dev.yaml';                  Port = 50014 }
    @{ Name = 'inventory';      Conf = 'services/economy/inventory/etc/inventory-dev.yaml';        Port = 50015 }
    @{ Name = 'auction';        Conf = 'services/economy/auction/etc/auction-dev.yaml';            Port = 50016 }
    @{ Name = 'ds-allocator';   Conf = 'services/battle/ds_allocator/etc/ds_allocator-dev.yaml';   Port = 50020 }
    @{ Name = 'hub-allocator';  Conf = 'services/battle/hub_allocator/etc/hub_allocator-dev.yaml'; Port = 50021 }
    @{ Name = 'battle-result';  Conf = 'services/battle/battle_result/etc/battle_result-dev.yaml'; Port = 50022 }
)

# 同伴服务 host 映射:127.0.0.1:<port> -> <svc>:<port>
$PortToHost = @{}
foreach ($s in $Services) { $PortToHost[[string]$s.Port] = $s.Name }

function Convert-DevToCluster([string]$text) {
    # 1) 基础设施地址(host:port 都变)
    $text = $text.Replace('127.0.0.1:3307', 'mysql:3306')
    $text = $text.Replace('127.0.0.1:6380', 'redis:6379')
    $text = $text.Replace('127.0.0.1:9093', 'kafka:9092')
    $text = $text.Replace('localhost:9093', 'kafka:9092')
    $text = $text.Replace('127.0.0.1:2380', 'etcd:2379')

    # 2) 同伴服务地址:host 换成服务短名,端口不变(容器内仍监听同端口)
    foreach ($port in $PortToHost.Keys) {
        $svc = $PortToHost[$port]
        $text = $text.Replace("127.0.0.1:$port", "${svc}:$port")
        $text = $text.Replace("localhost:$port", "${svc}:$port")
    }

    return $text
}

# allocator(ds-allocator / hub-allocator)专用改写:根据 -AllocatorMode 把 dev 的
# mode: "local"(宿主 exec Windows DS)改成集群里能跑的模式。
#   mock   → mode: "mock"(容器/集群内无 Windows PandoraServer.exe,返回确定性假地址)
#   agones → mode: "agones" + 把整个 agones: 段替换成 in-cluster 确定性模板(真 Linux DS)
function Rewrite-Allocator([string]$svcName, [string]$text) {
    if ($AllocatorMode -eq 'mock') {
        return ($text -replace '(?m)^(\s*mode:\s*)"local"', '$1"mock"')
    }

    # agones 模式:mode 改 agones
    $text = $text -replace '(?m)^(\s*mode:\s*)"local"', '$1"agones"'

    # 按服务选 fleet 与 timeout 键(ds=分配超时,hub=列表超时)
    if ($svcName -eq 'hub-allocator') {
        $fleet = 'pandora-hub'
        $timeoutLine = '  list_timeout: "5s"'
    } else {
        $fleet = 'pandora-battle'
        $timeoutLine = '  allocate_timeout: "5s"'
    }

    # 组装 in-cluster agones 段(投影 token/ca 自动轮转,allocator 每次请求重读 token 文件)
    $lines = @(
        'agones:'
        '  enabled: true'
        '  api_server: "https://kubernetes.default.svc"'
        '  namespace: "default"'
        "  fleet_name: `"$fleet`""
        '  token_path: "/var/run/secrets/kubernetes.io/serviceaccount/token"'
        '  ca_path: "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"'
        '  insecure_skip_tls_verify: false'
    )
    # advertise_host:本地 minikube(docker driver)用 127.0.0.1 + udp-relay 回程;线上留空用 status.address
    if (-not [string]::IsNullOrWhiteSpace($AllocatorAdvertiseHost)) {
        $lines += "  advertise_host: `"$AllocatorAdvertiseHost`""
    }
    $lines += $timeoutLine
    $agonesBlock = ($lines -join "`n") + "`n`n"

    # 把原 dev 的整个 agones: 段(直到下一个顶级注释块「# 本机拉起」)整块替换
    $text = [regex]::Replace($text, '(?ms)^agones:\r?\n.*?(?=^# 本机拉起)', $agonesBlock)
    return $text
}

New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$count = 0
foreach ($s in $Services) {
    $src = Join-Path $ProjectRoot $s.Conf
    if (-not (Test-Path $src)) {
        Write-Host "[WARN] 缺少 dev 配置: $($s.Conf)" -ForegroundColor Yellow
        continue
    }
    $raw = Get-Content $src -Raw
    $out = Convert-DevToCluster $raw
    if ($s.Name -in @('ds-allocator', 'hub-allocator')) {
        $out = Rewrite-Allocator $s.Name $out
    }
    $dst = Join-Path $OutDir "$($s.Name).yaml"
    # 用 UTF8(无 BOM)写出,避免 yaml 解析器吃到 BOM
    [System.IO.File]::WriteAllText($dst, $out, (New-Object System.Text.UTF8Encoding($false)))
    $count++
}

Write-Host "[ OK ] 生成 $count 个集群版配置(allocator=$AllocatorMode) -> $OutDir" -ForegroundColor Green
