# Pandora DS 心跳 + locator 上报 stub 工具（W4 ⑬，2026-06-09）
#
# 用途：在真 UE Hub/Battle DS 就绪前，用 grpcurl 模拟 DS 行为，验证后端心跳链路：
#   - Hub DS  → hub_allocator.Heartbeat（:50021）周期上报，刷新分片在线数/心跳时刻
#   - Battle DS → ds_allocator.Heartbeat（:50020）周期上报，续命战斗 DS 镜像
#   - Hub DS  → player_locator.SetLocation(HUB)（:50006）上报玩家在大厅（带 fence match_id）
#
# 配合「停止心跳 → 后台 sweep 标记 abandoned/draining」可端到端验证不变量 §4 补偿链。
# 这是 docs/design/agones-dev.md §3/§4「DS 心跳 + locator 上报契约」的 stub 实现。
#
# 前置：grpcurl 已安装；目标服务已启动且 enable_reflection=true（dev 默认开）。
#
# 用法示例：
#   # 1) 先让 hub_allocator 种子分片（mock 模式分片名 = pandora-hub-<region>-<i>）
#   pwsh tools/scripts/ds_heartbeat_stub.ps1 -Role hub -AssignFirst -PlayerId 30907585389428737
#
#   # 2) 持续心跳某个 hub 分片（每 5s，Ctrl+C 停止 → 看 hub_allocator sweep 标 draining）
#   pwsh tools/scripts/ds_heartbeat_stub.ps1 -Role hub -PodName pandora-hub-global-1 -PlayerCount 42
#
#   # 3) 持续心跳某场战斗 DS（需先经 matchmaker/AllocateBattle 建镜像，mock 名 = pandora-battle-<matchId>）
#   pwsh tools/scripts/ds_heartbeat_stub.ps1 -Role battle -PodName pandora-battle-123456 -MatchId 123456 -PlayerCount 10
#
#   # 4) 模拟战斗结束玩家回大厅，带 fence match_id 上报 locator（W4 ⑪ BATTLE→HUB 合法回流）
#   pwsh tools/scripts/ds_heartbeat_stub.ps1 -Role hub -PodName pandora-hub-global-1 `
#       -LocatorPlayerId 30907585389428737 -ShardId 1 -FenceMatchId 123456 -Count 1

[CmdletBinding()]
param(
    # DS 角色：hub → hub_allocator；battle → ds_allocator
    [ValidateSet("hub", "battle")]
    [string]$Role = "hub",

    # DS pod 名（= Redis 分片/镜像 key）。mock 模式：hub=pandora-hub-<region>-<i>，battle=pandora-battle-<matchId>
    [string]$PodName = "",

    # 战斗 DS 的对局 ID（Role=battle 必填；Heartbeat 的 match_id 字段）
    [string]$MatchId = "0",

    # 上报的当前在线人数
    [int]$PlayerCount = 1,

    # 上报状态：hub ∈ ready/draining/stopping；battle ∈ warming/ready/running/ended
    [string]$State = "ready",

    # 心跳间隔秒
    [int]$IntervalSec = 5,

    # 心跳次数；0 = 无限循环（Ctrl+C 停止，便于触发心跳超时 sweep）
    [int]$Count = 0,

    # Role=hub 时的 region（AssignFirst 种子分片 / locator 上报用）
    [string]$Region = "global",

    # 启动前先调一次 AssignHub 让 hub_allocator 种子分片（Role=hub）
    [switch]$AssignFirst,

    # AssignFirst / locator 上报用的 player_id（0 = 跳过）
    [string]$PlayerId = "0",

    # ── locator HUB 上报（Role=hub，可选）──
    # 若指定 LocatorPlayerId>0：每次心跳同时 SetLocation(player, HUB, hub_pod=PodName, shard_id, match_id=FenceMatchId)
    [string]$LocatorPlayerId = "0",
    [int]$ShardId = 1,
    # fence 令牌:战斗结束回大厅填刚结束那场 matchId（W4 ⑪），全新进大厅填 0
    [string]$FenceMatchId = "0",

    # 各服务地址（dev 默认直连 gRPC）
    [string]$HubAddr     = "127.0.0.1:50021",
    [string]$BattleAddr  = "127.0.0.1:50020",
    [string]$LocatorAddr = "127.0.0.1:50006"
)

$ErrorActionPreference = "Stop"

if (-not (Get-Command grpcurl -ErrorAction SilentlyContinue)) {
    Write-Host "[ERR] 未找到 grpcurl，请先安装（见 tools/scripts/install_dev_tools.ps1）" -ForegroundColor Red
    exit 1
}

# 默认 pod 名（mock 命名规则）
if ([string]::IsNullOrEmpty($PodName)) {
    if ($Role -eq "hub") { $PodName = "pandora-hub-$Region-1" }
    else { $PodName = "pandora-battle-$MatchId" }
}

# grpcurl 调用封装：JSON body 从 stdin 读取（PowerShell native exe 引号坑，见 team-debug.md §3）
function Invoke-Grpc {
    param([string]$Addr, [string]$Method, [hashtable]$Body)
    $json = $Body | ConvertTo-Json -Compress
    $resp = $json | grpcurl -plaintext -d '@' $Addr $Method 2>&1
    return $resp
}

function Send-Heartbeat {
    $tsMs = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
    if ($Role -eq "hub") {
        $body = @{
            hubPodName  = $PodName
            playerCount = $PlayerCount
            cpuPct      = 0
            memMb       = 0
            state       = $State
            tsMs        = $tsMs.ToString()
        }
        return Invoke-Grpc -Addr $HubAddr -Method "pandora.hub.v1.HubAllocatorService/Heartbeat" -Body $body
    }
    else {
        $body = @{
            dsPodName   = $PodName
            matchId     = $MatchId
            playerCount = $PlayerCount
            cpuPct      = 0
            memMb       = 0
            state       = $State
            tsMs        = $tsMs.ToString()
        }
        return Invoke-Grpc -Addr $BattleAddr -Method "pandora.ds.v1.DSAllocatorService/Heartbeat" -Body $body
    }
}

function Send-LocatorHub {
    $tsMs = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
    $body = @{
        playerId = $LocatorPlayerId
        location = @{
            state       = "LOCATION_STATE_HUB"
            hubPod      = $PodName
            shardId     = $ShardId
            matchId     = $FenceMatchId   # fence 令牌；后端进 HUB 后清零
            updatedAtMs = $tsMs.ToString()
        }
    }
    return Invoke-Grpc -Addr $LocatorAddr -Method "pandora.locator.v1.PlayerLocatorService/SetLocation" -Body $body
}

# 可选:先种子分片(Role=hub)
if ($AssignFirst -and $Role -eq "hub") {
    if ($PlayerId -eq "0") {
        Write-Host "[WARN] -AssignFirst 需要 -PlayerId，跳过种子分片" -ForegroundColor Yellow
    }
    else {
        Write-Host "===== AssignHub 种子分片 (player=$PlayerId region=$Region) =====" -ForegroundColor Cyan
        $assignBody = @{ playerId = $PlayerId; region = $Region; teamId = "0" }
        $assignResp = Invoke-Grpc -Addr $HubAddr -Method "pandora.hub.v1.HubAllocatorService/AssignHub" -Body $assignBody
        Write-Host $assignResp
        Write-Host "（用返回的 hubPodName 作 -PodName 继续心跳）" -ForegroundColor DarkGray
        Write-Host ""
    }
}

Write-Host "===== DS 心跳 stub 启动 =====" -ForegroundColor Cyan
Write-Host "  role=$Role pod=$PodName state=$State interval=${IntervalSec}s count=$(if ($Count -eq 0) {'∞'} else {$Count})" -ForegroundColor Cyan
if ($LocatorPlayerId -ne "0" -and $Role -eq "hub") {
    Write-Host "  locator: player=$LocatorPlayerId shard=$ShardId fenceMatchId=$FenceMatchId" -ForegroundColor Cyan
}
Write-Host "  Ctrl+C 停止（停止后等心跳超时 → 后台 sweep 标记 abandoned/draining）" -ForegroundColor DarkGray
Write-Host ""

$i = 0
while ($true) {
    $i++
    $now = (Get-Date).ToString("HH:mm:ss")
    $hbResp = Send-Heartbeat
    # 提取 command 字段（"" / stop / drain）便于观察
    $cmd = ($hbResp | Select-String -Pattern '"command"\s*:\s*"([^"]*)"').Matches.Groups[1].Value
    $codeLine = ($hbResp | Select-String -Pattern '"code"').Line
    Write-Host "[$now] #$i heartbeat $Role/$PodName → command='$cmd' $codeLine" -ForegroundColor Green

    if ($LocatorPlayerId -ne "0" -and $Role -eq "hub") {
        $locResp = Send-LocatorHub
        $locCode = ($locResp | Select-String -Pattern '"code"').Line
        Write-Host "        locator SetLocation(HUB) → $locCode" -ForegroundColor DarkCyan
    }

    if ($Count -ne 0 -and $i -ge $Count) { break }
    Start-Sleep -Seconds $IntervalSec
}

Write-Host ""
Write-Host "===== 心跳 stub 结束（共 $i 次）=====" -ForegroundColor Cyan
