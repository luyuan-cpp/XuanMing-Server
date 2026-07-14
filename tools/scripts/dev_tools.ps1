# Pandora 压测前状态清理工具(dev_tools.ps1)
#
# 用途(对齐 docs/design/stress-discipline.md §4.1):压测前把单 Cell 后端依赖的
#   redis / mysql / etcd / kafka offset 恢复到干净基线,避免上一轮残留数据污染本轮
#   prom 指标对比。(k8s GameServer 由人单独 kubectl delete,本脚本不碰 k8s)
#
# ⚠️ 破坏性操作:会清空 dev 环境的 Redis、压测相关 MySQL 表、etcd 压测前缀、
#   重置消费者组 offset。默认需要 -Confirm 二次确认;-Force 跳过确认。
#   只作用于本机 docker compose dev 环境(pandora-mysql/redis/kafka/etcd 容器),
#   不碰任何远端 / 生产。
#
# 用法(命令名对齐 stress-discipline.md §4.1 引用):
#   pwsh tools/scripts/dev_tools.ps1 -Command status
#   pwsh tools/scripts/dev_tools.ps1 -Command redis-flush         -Confirm
#   pwsh tools/scripts/dev_tools.ps1 -Command db-reset            -Confirm
#   pwsh tools/scripts/dev_tools.ps1 -Command kafka-offset-reset  -Confirm
#   pwsh tools/scripts/dev_tools.ps1 -Command etcd-clear          -Confirm
#   pwsh tools/scripts/dev_tools.ps1 -Command all                 -Force
#
# Command:
#   status              打印各依赖当前规模(redis dbsize / 关键表行数 / kafka group),不修改
#   redis-flush         FLUSHALL 清空 Redis(端口 6380)
#   db-reset            TRUNCATE 压测相关业务表(保留库 / 表结构)
#   kafka-offset-reset  重置压测消费者组 offset 到 earliest(不删 topic)
#   etcd-clear          删除 etcd 压测相关前缀 key(雪花 / killswitch / cellroute 保留)
#   all                 依次执行 redis-flush + db-reset + etcd-clear + kafka-offset-reset

param(
    [ValidateSet("status", "redis-flush", "db-reset", "kafka-offset-reset", "etcd-clear", "all")]
    [string]$Command = "status",
    [switch]$Confirm,
    [switch]$Force
)

$ErrorActionPreference = "Stop"

# ---- 容器 / 连接参数(对齐 deploy/docker-compose.dev.yml + deploy/env/dev.env)----
$MysqlContainer = "pandora-mysql"
$RedisContainer = "pandora-redis"
$KafkaContainer = "pandora-kafka"
$EtcdContainer  = "pandora-etcd"
$MysqlRootPwd   = "pandora_dev_root"

# 压测会写入的业务表(库.表)。只 TRUNCATE 数据,不动结构 / 不删库。
$StressTables = @(
    "pandora_account.accounts",
    "pandora_account.account_devices",
    "pandora_account.account_bans",
    "pandora_player.players",
    "pandora_player.player_heroes",
    "pandora_player.mmr_history",
    "pandora_player.player_attributes",
    "pandora_player.attr_point_grants",
    "pandora_player.player_equipment",
    "pandora_player.player_talents",
    "pandora_player.talent_point_grants",
    "pandora_social.friendships",
    "pandora_social.friend_requests",
    "pandora_social.blocks",
    "pandora_social.chat_private_messages",
    "pandora_battle.battles",
    "pandora_battle.battle_player_stats",
    "pandora_battle.player_update_outbox",
    "pandora_battle.battle_drop_outbox",
    "pandora_battle.terminal_release_outbox",
    "pandora_trade.player_currency",
    "pandora_trade.player_items",
    "pandora_trade.inventory_ledger",
    "pandora_trade.auction_escrow",
    "pandora_auction.auction_orders",
    "pandora_auction.auction_matches"
)

# 压测相关消费者组(重置 offset 用)。
$StressConsumerGroups = @(
    "pandora-push",
    "pandora-battle-result",
    "pandora-social"
)

function Assert-Container([string]$name) {
    $running = docker ps --filter "name=$name" --filter "status=running" --format "{{.Names}}"
    if (-not ($running -split "`n" | Where-Object { $_ -eq $name })) {
        throw "容器 $name 未在运行,请先 pwsh tools/scripts/dev_up.ps1 启动 dev 基础设施。"
    }
}

function Confirm-Destructive([string]$desc) {
    if ($Force) { return $true }
    if (-not $Confirm) {
        Write-Host "[跳过] $desc —— 需要 -Confirm 或 -Force 才会执行。" -ForegroundColor Yellow
        return $false
    }
    $ans = Read-Host "确认执行【$desc】?该操作不可逆 (yes/no)"
    return ($ans -eq "yes")
}

function Invoke-Status {
    Write-Host "===== Pandora 压测依赖状态 =====" -ForegroundColor Cyan
    Assert-Container $RedisContainer
    $dbsize = docker exec $RedisContainer redis-cli DBSIZE
    Write-Host ("  Redis DBSIZE      : {0}" -f $dbsize)

    Assert-Container $MysqlContainer
    foreach ($t in $StressTables) {
        $parts = $t.Split(".")
        $cnt = docker exec $MysqlContainer mysql -uroot "-p$MysqlRootPwd" -N -B -e "SELECT COUNT(*) FROM $($parts[0]).$($parts[1]);" 2>$null
        if ($LASTEXITCODE -ne 0) {
            Write-Host ("  {0,-38}: ERROR/MISSING" -f $t) -ForegroundColor Yellow
            continue
        }
        Write-Host ("  {0,-38}: {1}" -f $t, (($cnt | Out-String) -replace "\s", ""))
    }

    Assert-Container $KafkaContainer
    Write-Host "  Kafka 消费者组:"
    foreach ($g in $StressConsumerGroups) {
        Write-Host ("    - {0}" -f $g)
    }
}

function Invoke-RedisFlush {
    if (-not (Confirm-Destructive "FLUSHALL 清空 Redis(:6380)")) { return }
    Assert-Container $RedisContainer
    docker exec $RedisContainer redis-cli FLUSHALL | Out-Null
    Write-Host "[OK] Redis 已清空。" -ForegroundColor Green
}

function Invoke-DbReset {
    if (-not (Confirm-Destructive "TRUNCATE 压测相关 MySQL 表")) { return }
    Assert-Container $MysqlContainer
    $sql = "SET FOREIGN_KEY_CHECKS=0;`n"
    foreach ($t in $StressTables) {
        $parts = $t.Split(".")
        $exists = docker exec $MysqlContainer mysql -uroot "-p$MysqlRootPwd" -N -B -e "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='$($parts[0])' AND table_name='$($parts[1])';" 2>$null
        if ($LASTEXITCODE -ne 0 -or (($exists | Out-String).Trim()) -ne "1") {
            Write-Host ("  [skip] {0} 不存在,跳过" -f $t) -ForegroundColor Yellow
            continue
        }
        $sql += "TRUNCATE TABLE $t;`n"
    }
    $sql += "SET FOREIGN_KEY_CHECKS=1;`n"
    $sql | docker exec -i $MysqlContainer mysql -uroot "-p$MysqlRootPwd"
    Write-Host "[OK] 压测相关 MySQL 表已 TRUNCATE。" -ForegroundColor Green
}

function Invoke-EtcdClear {
    if (-not (Confirm-Destructive "删除 etcd 压测前缀 key(/pandora/stress/)")) { return }
    Assert-Container $EtcdContainer
    # 只删压测命名空间前缀,保留雪花 node / killswitch / cellroute 拓扑等长期配置。
    docker exec $EtcdContainer etcdctl del --prefix "/pandora/stress/" | Out-Null
    Write-Host "[OK] etcd 压测前缀已清。(雪花/killswitch/cellroute 保留)" -ForegroundColor Green
}

function Invoke-KafkaOffsetReset {
    if (-not (Confirm-Destructive "重置压测消费者组 offset 到 earliest")) { return }
    Assert-Container $KafkaContainer
    foreach ($g in $StressConsumerGroups) {
        # 重置 offset 需要组内无活跃消费者:压测前先停服(run_services.ps1 -Action stop)。
        docker exec $KafkaContainer kafka-consumer-groups `
            --bootstrap-server localhost:9092 `
            --group $g --reset-offsets --all-topics --to-earliest --execute 2>$null | Out-Null
        Write-Host ("  [OK] group {0} offset → earliest" -f $g) -ForegroundColor Green
    }
}

switch ($Command) {
    "status"             { Invoke-Status }
    "redis-flush"        { Invoke-RedisFlush }
    "db-reset"           { Invoke-DbReset }
    "kafka-offset-reset" { Invoke-KafkaOffsetReset }
    "etcd-clear"         { Invoke-EtcdClear }
    "all" {
        Invoke-RedisFlush
        Invoke-DbReset
        Invoke-EtcdClear
        Invoke-KafkaOffsetReset
    }
}

Write-Host ""
Write-Host "提示:压测前停服请用 pwsh tools/scripts/run_services.ps1 -Action down(stress-discipline.md 里的 go_svc_stop.ps1 是旧名,本仓库统一用 run_services.ps1)。" -ForegroundColor DarkGray
