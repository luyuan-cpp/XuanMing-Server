# progress_drain_check — 实时成长通道回滚前**抽样自检**(审计 P1,2026-07-21;2026-07-22 降级定位)。
#
# ⚠️ 定位(审计钉死):本脚本只做数据库瞬时计数**抽样**,不是"全 fleet 已关流"的证明——
#   它无法验证每个副本的 progress_enabled 配置,也拦不住采样间隙里旧 Pod / 在途首批
#   的写入。真正的关流证明 = 配置面确认(全 fleet 生效的 progress_enabled=false)+
#   本脚本连续多次零采样作为数据面旁证。两者都过才允许回滚。
#
# 用途:battle_result / player 从"含实时进度收口逻辑"的版本回滚到旧二进制之前,
# 抽样验证实时通道残留:旧版本不认识水位表的发放权语义,可能对已实时发放的对局
# 再次写终局奖励(双发)。
#
# 回滚门禁顺序:
#   1. 运维先把全 fleet progress_enabled 置 false 并**从配置面确认已全量生效**
#      (关新流;进行中对局继续收流到结算);
#   2. 本脚本连续 -Samples 次(间隔 -IntervalSeconds)抽样,每次要求:
#      a. 活跃实时水位 = 0(battle_progress_stream 无 settled_at_ms=0 且 last_applied_seq>0 行);
#      b. battle_progress_outbox 排空(实时发放全部落到 player/inventory);
#      c. player_push_outbox 排空(经验推送全部投递);
#   3. 任一次任一项不为 0 → 退出码 1,禁止回滚;等待排空后重跑。
#
# 用法:
#   本地 docker:pwsh tools/scripts/progress_drain_check.ps1
#   k8s:       pwsh tools/scripts/progress_drain_check.ps1 -Mode k8s [-Context <ctx>] [-Namespace default]
[CmdletBinding()]
param(
    [ValidateSet('docker', 'k8s')]
    [string]$Mode = 'docker',
    # docker 模式:本地 dev MySQL 容器与 root 密码(与 dev_tools.ps1 缺省一致)。
    [string]$MysqlContainer = 'pandora-mysql',
    [string]$MysqlRootPwd = 'pandora_dev_root',
    # k8s 模式:mysql Deployment(容器内需有 MYSQL_USER / MYSQL_PASSWORD 环境变量,
    # 与 reset_data_service_schema.ps1 同约定)。
    [string]$MySqlDeployment = 'mysql',
    [string]$Namespace = 'default',
    [string]$Context = '',
    # 连续零采样次数与间隔:单次瞬时计数拦不住采样间隙写入,连续窗口抽样收窄
    # (但不消除)漏检面 —— 结论仍只是旁证,见头部定位说明。
    [ValidateRange(1, 100)][int]$Samples = 3,
    [ValidateRange(1, 3600)][int]$IntervalSeconds = 10
)

$ErrorActionPreference = 'Stop'

function Invoke-ScalarQuery([string]$Sql, [string]$What) {
    if ($Mode -eq 'docker') {
        $out = docker exec $MysqlContainer mysql -uroot "-p$MysqlRootPwd" -N -B -e $Sql 2>$null
    } else {
        $kubectlArgs = @()
        if (-not [string]::IsNullOrWhiteSpace($Context)) { $kubectlArgs += @('--context', $Context) }
        $kubectlArgs += @('-n', $Namespace, 'exec', "deployment/$MySqlDeployment", '--container', 'mysql', '--',
            'sh', '-ec', ('MYSQL_PWD="$MYSQL_PASSWORD" mysql -u"$MYSQL_USER" --batch --skip-column-names -e ' + "'" + $Sql + "'"))
        $out = kubectl @kubectlArgs 2>$null
    }
    if ($LASTEXITCODE -ne 0) { throw "[FATAL] 查询失败($What):$Sql" }
    $text = (($out | Out-String) -replace '\s', '')
    $n = 0
    if (-not [long]::TryParse($text, [ref]$n)) { throw "[FATAL] 无法解析计数($What):'$text'" }
    return $n
}

$checks = @(
    @{ What = '活跃实时水位(未结算且已开流的对局)'
       Sql = 'SELECT COUNT(*) FROM pandora_battle.battle_progress_stream WHERE settled_at_ms = 0 AND last_applied_seq > 0;'
       Hint = '仍有对局在实时通道收流:等它们结算(或按心跳超时走 ABANDONED 收口)后重跑。' }
    @{ What = '实时进度发放出箱积压'
       Sql = 'SELECT COUNT(*) FROM pandora_battle.battle_progress_outbox;'
       Hint = '实时发放尚未全部入账:确认 battle_result 出箱发布器运行、player/inventory 可达后等待排空。' }
    @{ What = '玩家经验推送出箱积压'
       Sql = 'SELECT COUNT(*) FROM pandora_player.player_push_outbox;'
       Hint = '经验推送尚未全部投递:确认 player 推送发布器与 kafka 可达后等待排空。' }
)

Write-Host "===== 实时成长通道回滚前抽样自检($Mode,$Samples 次 × ${IntervalSeconds}s)=====" -ForegroundColor Cyan
Write-Host '前提(脚本无法代验,须运维从配置面确认):全 fleet progress_enabled 已置 false 且全量生效。' -ForegroundColor Yellow
Write-Host '本脚本只是数据面瞬时抽样旁证,不构成关流证明(见脚本头部定位说明)。' -ForegroundColor Yellow

for ($round = 1; $round -le $Samples; $round++) {
    Write-Host ("--- 抽样 {0}/{1} ---" -f $round, $Samples)
    foreach ($c in $checks) {
        $n = Invoke-ScalarQuery $c.Sql $c.What
        if ($n -eq 0) {
            Write-Host ("[ OK ] {0}: 0" -f $c.What) -ForegroundColor Green
        } else {
            Write-Host ("[FAIL] {0}: {1}" -f $c.What, $n) -ForegroundColor Red
            Write-Host ("       {0}" -f $c.Hint) -ForegroundColor Yellow
            Write-Host '[结论] 通道未排空,禁止回滚到不含实时进度收口逻辑的旧版本。' -ForegroundColor Red
            exit 1
        }
    }
    if ($round -lt $Samples) { Start-Sleep -Seconds $IntervalSeconds }
}

Write-Host ("[结论] 连续 {0} 次抽样均为零:未观测到实时通道残留(数据面旁证通过)。" -f $Samples) -ForegroundColor Green
Write-Host '       回滚仍以配置面"全 fleet progress_enabled=false 已生效"的确认为前置;回滚后水位表历史行只读留存,不影响旧版本。' -ForegroundColor Yellow
exit 0
