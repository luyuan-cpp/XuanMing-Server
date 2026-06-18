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
#   pwsh tools/scripts/gen_cluster_config.ps1                # 生成到 run/cluster/etc
#   pwsh tools/scripts/gen_cluster_config.ps1 -OutDir <dir>  # 自定义输出目录

[CmdletBinding()]
param(
    [string]$OutDir
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

    # 3) allocator 本机 exec 模式 -> mock(容器/集群里没有 Windows PandoraServer.exe)
    $text = $text -replace '(?m)^(\s*mode:\s*)"local"', '$1"mock"'

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
    $dst = Join-Path $OutDir "$($s.Name).yaml"
    # 用 UTF8(无 BOM)写出,避免 yaml 解析器吃到 BOM
    [System.IO.File]::WriteAllText($dst, $out, (New-Object System.Text.UTF8Encoding($false)))
    $count++
}

Write-Host "[ OK ] 生成 $count 个集群版配置 -> $OutDir" -ForegroundColor Green
