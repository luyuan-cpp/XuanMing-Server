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
#   pwsh tools/scripts/gen_cluster_config.ps1 -HostAllocators                  # 混合模式:容器服务回连宿主 allocator
#   pwsh tools/scripts/gen_cluster_config.ps1 -AllocatorMode agones -Prod -Secret <玩家面密钥> -DsSecret <DS回调面密钥>
#                                                                              # 生产:注入两把独立真 HS256 密钥
#
# ⚠️ 安全(§5 审核):生产判定**只看 -Prod**(不再从 -AllocatorAdvertiseHost 推断,避免线上配了
#    advertise host 就被误判为 dev 而放行公开 dev 密钥)。-Prod 时必须提供**两把**真密钥:
#    玩家面 -Secret(login/hub/matchmaker jwt + envoy JWKS)与 DS 回调面 -DsSecret(ds_auth),
#    各自非空 / ≠dev / ≥32B、且**彼此不同**(P0 审核:同一密钥覆盖玩家 JWT 与 DS 回调 = 泄露即互通);
#    也可分别用环境变量 PANDORA_JWT_SECRET / PANDORA_DS_JWT_SECRET。注入后在 <OutDir>/envoy-jwks.json
#    产出匹配玩家面密钥的 Envoy JWKS + 校验 committed envoy.yaml。
#
# 三条链路与 allocator 模式的对应(由 start.ps1 驱动):
#   本地 windows (-Mode local)  → dev yaml 原样 mode=local,不过本生成器(宿主 exec Windows DS)
#   docker        (-Mode docker) → -AllocatorMode mock  (容器内无真 DS,假地址只测后端链路)
#   battle       (-Mode battle) → -AllocatorMode mock -HostAllocators(18 容器 + 2 宿主 allocator)
#   线上 k8s     (-Mode online) → -AllocatorMode agones(GameServer status.address 直连真 Linux DS)
#   本地 k8s     (-Mode k8s)    → -AllocatorMode agones -AllocatorAdvertiseHost 127.0.0.1 + udp_relay.ps1

[CmdletBinding()]
param(
    [string]$OutDir,
    [ValidateSet('mock', 'agones')]
    [string]$AllocatorMode = 'mock',
    [string]$AllocatorAdvertiseHost = '',
    # 混合(含战斗)模式:ds_allocator / hub_allocator 跑在宿主(要 exec Windows DS),
    # 不进容器。容器里的 matchmaker/login/battle_result 需经 host.docker.internal 回连宿主 allocator。
    [switch]$HostAllocators,
    # 玩家面 HS256 密钥(login jwt / hub_allocator jwt / matchmaker jwt / envoy JWKS 同一把,
    # 客户端 SessionToken / DSTicket 用它签验)。默认取环境变量,便于 CI/CD 注入真密钥而不落盘。
    [string]$Secret = $env:PANDORA_JWT_SECRET,
    # DS 回调面 HS256 密钥(ds_auth.secret:ds_allocator / hub_allocator / battle_result /
    # player_locator 校验 DS→后端回调令牌)。**必须与玩家面 -Secret 不同**——两把同值时,泄露玩家
    # 面密钥即可伪造 DS 回调令牌绕过范围绑定(审核 P0:生产不得用同一密钥覆盖玩家 JWT 与 DS 回调)。
    [string]$DsSecret = $env:PANDORA_DS_JWT_SECRET,
    # 玩家面轮换兼容密钥(可选,仅非生产验证):写进 login/hub/matchmaker 的
    # jwt.additional_secrets(仅验签不签发)并进 Envoy JWKS 第二把 key。阶段①它是待启用的新 key，
    # 阶段②它是待清退的旧 key，不能固定理解成“旧密钥”。生产暂由下方待决策门拒绝。
    [string]$SecretAdditional = $env:PANDORA_JWT_SECRET_ADDITIONAL,
    # DS 回调面轮换兼容密钥(可选,仅非生产验证):写进 4 个 ds_auth 服务的 additional_secrets。
    [string]$DsSecretAdditional = $env:PANDORA_DS_JWT_SECRET_ADDITIONAL,
    # ds_auth.mode 改写(二审 A#2):-Prod 只允许 'enforce'(生产 DS 回调必须鉴权,否则
    # warming→ready 只是活性信号、任意进程可伪造心跳/战果回调)。灰度 permissive/off
    # 只能在非 Prod 环境显式测试；非 -Prod 不传则保持 dev 模板值不变。
    [ValidateSet('', 'off', 'permissive', 'enforce')]
    [string]$DsAuthMode = '',
    # Model B 授权权威。-Prod 只允许 redis；非生产默认保留模板 legacy，显式 redis 时也必须
    # 同时提供 fence etcd/keyset revision，生成器会原子改写四个回调服务 + login binding。
    [ValidateSet('', 'legacy', 'redis')]
    [string]$DsAuthorityMode = '',
    [string]$DsFenceEtcdEndpoints = $env:PANDORA_DS_AUTH_FENCE_ETCD_ENDPOINTS,
    [string]$DsFenceKeysetRevision = $env:PANDORA_DS_AUTH_KEYSET_REVISION,
    # DSTicket v2(方案 B,RS256 非对称,decision-revisit-player-jwt-key-rotation.md §7)签发接线:
    # agones 链路(真 Linux DS,DS 侧只认 v2 票)必填 active kid(RFC 7638 指纹,43 字符 base64url,
    # 取自 tools/dsticketkeys 生成的 jwks.json)。生成器把 ds_ticket 段注入 4 个签发方
    # (login/matchmaker/matchmaker-pve/hub-allocator);私钥经 revisioned K8s Secret
    # pandora-dsticket-signer-rN 挂载到稳定容器路径
    # /run/secrets/pandora-dsticket/private.pem(见 deploy/k8s/services/services.yaml)。
    [string]$DsTicketActiveKid = $env:PANDORA_DSTICKET_ACTIVE_KID,
    # 公钥 JWKS 不可变 ConfigMap 的 revision。Login 的 VerifyDSTicket 诊断/兼容路径与
    # DS 挂载同一份公开 keyset 内容，并同时校验 revision + explicit active_kid。
    [string]$DsTicketKeysetRevision = $env:PANDORA_DSTICKET_KEYSET_REVISION,
    # DSTicket v2 票据 TTL(默认 120s;机械上限 180s,UE 验票侧同样强制 exp-iat ≤ 180s)。
    [string]$DsTicketTTL = '120s',
    # Stable/Canary DS 轨道：百分比按服务端确定性 cohort 分桶，seed 是发布配置而非密钥，
    # 但启用灰度后必须稳定不漂移；普通发布与两条 Fleet 共用同一 DSTicket keyset。
    [ValidateRange(0, 100)][int]$BattleCanaryPercent = 0,
    [ValidateRange(0, 100)][int]$HubCanaryPercent = 0,
    [string]$CanarySeed = $env:PANDORA_DS_CANARY_SEED,
    # -Prod:显式声明「这是生产/线上产物」。**唯一**的生产判定信号(不再从 advertise host 推断,
    # 避免线上设了 advertise host 就被误判为 dev 而放行公开 dev 密钥,§5 安全审核)。
    # 生产模式强制:必须提供两把真密钥(玩家面 -Secret + DS 回调面 -DsSecret,各自非空、≠dev、
    # ≥32B、且彼此不同),否则拒绝生成;并同步产出匹配的 Envoy JWKS。
    [switch]$Prod,
    # -AllowDevSecrets:显式声明「这是本地/开发链路,允许写入公开 dev 密钥」。agones 模式(真 Linux DS)
    # 不带 -Prod 时,必须显式加本开关才放行 dev 密钥(审核 P1:不再用「存在任意 advertise host」推断本地,
    # 生产 IP/DNS 也会配 advertise host,那样推断会让生产绕过 -Prod 写入 dev 密钥)。仅供本地 minikube 自测。
    [switch]$AllowDevSecrets
)

# 公开 dev 共享密钥:仅供本机 / docker-mock 链路,绝不能进生产产物。
$DevPublicSecret = 'pandora-dev-jwt-secret-change-me-32!'


$ErrorActionPreference = 'Stop'
$ProjectRoot = (Resolve-Path "$PSScriptRoot/../..").Path
if (-not $OutDir) { $OutDir = Join-Path $ProjectRoot 'run/cluster/etc' }
$OutDir = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($OutDir)
$OutDir = [System.IO.Path]::TrimEndingDirectorySeparator($OutDir)

if (($BattleCanaryPercent -gt 0 -or $HubCanaryPercent -gt 0) -and
    ($CanarySeed -cnotmatch '^[A-Za-z0-9._-]{8,128}$')) {
    throw '[FATAL] 启用 DS Canary 时必须提供 8..128 字符稳定 -CanarySeed / PANDORA_DS_CANARY_SEED(仅字母数字._-)。'
}
if ($AllocatorMode -ne 'agones' -and ($BattleCanaryPercent -ne 0 -or $HubCanaryPercent -ne 0)) {
    throw '[FATAL] Stable/Canary Fleet 分流仅适用于 -AllocatorMode agones。'
}

# ===== 密钥策略(§5 + P0 安全审核:禁止把公开 dev 密钥发到生产;玩家面 / DS 回调面必须分离)=====
# 生产判定**只看 -Prod**(不再从 -AllocatorAdvertiseHost 推断:真线上完全可能配 advertise host,
# 那样推断会把生产误判为 dev 而放行 dev 密钥)。
#   -Prod → 必须注入两把独立真密钥:
#            玩家面 $PlayerSecretToInject(login/hub/matchmaker jwt + envoy JWKS)
#            DS 回调面 $DsSecretToInject(ds_auth)
#          两把各自非空 / ≠dev / ≥32B、且**彼此不同**(P0:同值时泄露玩家面即可伪造 DS 回调令牌)。
#   非 -Prod(dev/mock/minikube) → 默认沿用 dev 密钥;也可分别用 -Secret / -DsSecret 覆盖成真密钥(便于本地测)。
$PlayerSecretToInject = $null
$DsSecretToInject = $null

# 校验单把密钥的强度(生产:非空 + ≠dev + ≥32B)。返回校验后的密钥。
function Assert-ProdSecret([string]$val, [string]$flag, [string]$envName) {
    if ([string]::IsNullOrWhiteSpace($val)) {
        throw "[FATAL] -Prod 生产模式必须提供 $flag:传 $flag 或设环境变量 $envName。" +
              " 拒绝把公开 dev 密钥 '$DevPublicSecret' 写进生产配置(§5/P0 安全审核)。"
    }
    if ($val -eq $DevPublicSecret) {
        throw "[FATAL] -Prod 生产模式的 $flag 不能等于公开 dev 密钥。请换成 CI/CD 注入的真密钥。"
    }
    if ([System.Text.Encoding]::UTF8.GetByteCount($val) -lt 32) {
        throw "[FATAL] $flag 至少需要 32 字节(HS256),当前长度不足。"
    }
    # 拒绝控制字符(审核 P1 #8):密钥会被注入双引号 YAML 字符串。换行 / 制表 / 其它控制字符
    # 多半是误带(如 $(cat secret) 的尾部换行),静默转义会掩盖错误 → 直接拒绝并要求清理。
    if ($val -match '[\x00-\x1F\x7F-\x9F]') {
        throw "[FATAL] $flag 含控制字符(换行 / 制表 / NUL 等),多为误带的尾部空白。请清理后再注入生产配置。"
    }
    return $val
}

if ($Prod) {
    $PlayerSecretToInject = Assert-ProdSecret $Secret '-Secret(玩家面)' 'PANDORA_JWT_SECRET'
    $DsSecretToInject = Assert-ProdSecret $DsSecret '-DsSecret(DS 回调面)' 'PANDORA_DS_JWT_SECRET'
    if ($PlayerSecretToInject -eq $DsSecretToInject) {
        throw "[FATAL] -Secret(玩家面)与 -DsSecret(DS 回调面)不得相同(P0 审核:同一密钥覆盖玩家 JWT" +
              " 与 DS 回调令牌 —— 泄露玩家面密钥即可伪造 DS 回调令牌绕过范围绑定)。请用两把独立真密钥。"
    }
} else {
    # 非生产也允许分别显式覆盖(便于本地测真密钥),但不强制;≥32B 仍要求。
    if (-not [string]::IsNullOrWhiteSpace($Secret) -and $Secret -ne $DevPublicSecret) {
        if ([System.Text.Encoding]::UTF8.GetByteCount($Secret) -lt 32) { throw "[FATAL] -Secret 至少需要 32 字节(HS256)。" }
        if ($Secret -match '[\x00-\x1F\x7F-\x9F]') { throw '[FATAL] -Secret 含 C0/C1 控制字符,不能安全写入 YAML。' }
        $PlayerSecretToInject = $Secret
    }
    if (-not [string]::IsNullOrWhiteSpace($DsSecret) -and $DsSecret -ne $DevPublicSecret) {
        if ([System.Text.Encoding]::UTF8.GetByteCount($DsSecret) -lt 32) { throw "[FATAL] -DsSecret 至少需要 32 字节(HS256)。" }
        if ($DsSecret -match '[\x00-\x1F\x7F-\x9F]') { throw '[FATAL] -DsSecret 含 C0/C1 控制字符,不能安全写入 YAML。' }
        $DsSecretToInject = $DsSecret
    }
}

# ===== 轮换旧密钥(additional_secrets,可选)=====
# 只在对应主密钥已注入真密钥时才允许(dev 主密钥 + 真 additional 是配置事故);与主密钥同标准校验,
# 且四把密钥(玩家主/玩家旧/DS 主/DS 旧)两两不同 —— 跨面交叉 = 泄露一面即可伪造另一面(P0)。
$PlayerAdditionalToInject = $null
$DsAdditionalToInject = $null
function Assert-AdditionalSecret([string]$val, [string]$flag, [string]$primaryVal, [string]$primaryFlag) {
    if ($null -eq $primaryVal) {
        throw "[FATAL] 提供了 $flag 但对应主密钥 $primaryFlag 未注入真密钥;轮换旧密钥只能与真主密钥搭配。"
    }
    if ($val -eq $DevPublicSecret) { throw "[FATAL] $flag 不能等于公开 dev 密钥。" }
    if ([System.Text.Encoding]::UTF8.GetByteCount($val) -lt 32) { throw "[FATAL] $flag 至少需要 32 字节(HS256)。" }
    if ($val -match '[\x00-\x1F\x7F-\x9F]') { throw "[FATAL] $flag 含 C0/C1 控制字符,多为误带的尾部空白,已拒绝。" }
    return $val
}
if (-not [string]::IsNullOrWhiteSpace($SecretAdditional)) {
    $PlayerAdditionalToInject = Assert-AdditionalSecret $SecretAdditional '-SecretAdditional(玩家面兼容密钥)' $PlayerSecretToInject '-Secret'
}
if (-not [string]::IsNullOrWhiteSpace($DsSecretAdditional)) {
    $DsAdditionalToInject = Assert-AdditionalSecret $DsSecretAdditional '-DsSecretAdditional(DS 回调面兼容密钥)' $DsSecretToInject '-DsSecret'
}
# 两份轮换决策仍为“待人拍板”。允许非生产验证现有部分接线，但生产生成必须 fail-closed，
# 不能在 Edge/阶段流程与权威 key-set gate 尚未批准时把 additional 带入线上产物。
if ($Prod -and ($null -ne $PlayerAdditionalToInject -or $null -ne $DsAdditionalToInject)) {
    throw '[FATAL] -Prod 暂不允许玩家面/DS 面 additional_secrets：轮换决策仍待人拍板，现有代码仅可非生产验证。' +
          '请先批准并更新 decision-revisit-player-jwt-key-rotation.md / decision-revisit-ds-key-rotation.md，再移除此生产门。'
}
$allInjected = @(
    @{ n = '-Secret(玩家面)';               v = $PlayerSecretToInject },
    @{ n = '-SecretAdditional(玩家面兼容)';    v = $PlayerAdditionalToInject },
    @{ n = '-DsSecret(DS 回调面)';           v = $DsSecretToInject },
    @{ n = '-DsSecretAdditional(DS 回调面兼容)'; v = $DsAdditionalToInject }
) | Where-Object { $null -ne $_.v }
for ($i = 0; $i -lt $allInjected.Count; $i++) {
    for ($j = $i + 1; $j -lt $allInjected.Count; $j++) {
        if ($allInjected[$i].v -ceq $allInjected[$j].v) {
            throw "[FATAL] $($allInjected[$i].n) 与 $($allInjected[$j].n) 不得相同(P0:玩家面/DS 回调面/新旧轮换密钥必须各自独立)。"
        }
    }
}

# ===== ds_auth.mode 改写决策(二审 A#2 + 三审 P1-3)=====
# 目标姿态:生产 DS 回调必须 enforce(否则 warming→ready 只是活性信号,任意进程可伪造心跳/战果回调)。
# 但**当前 UE DS 尚未读取 pandora.dev/ds-token、不发 Bearer**;直接对线上硬制 enforce 会让七类
# DS→后端回调全部 401(三审 P1-3 阻塞)。故 -Prod 默认 enforce,但保留**显式灰度逃生门**:
#   -DsAuthMode enforce(或不传)→ enforce(目标姿态,要求 DS 已接令牌)。
#   -DsAuthMode permissive       → permissive(仅记录不拒绝)。**DS 未接令牌期间的过渡窗口**用它:
#                                  回调照常放行、同时观测「带令牌比例」,等 DS 全量发令牌再切 enforce。
#   -DsAuthMode off              → 生产**永远拒绝**(off = 完全不校验,连观测都没有,绝不允许上线)。
# 非 -Prod:不传保持模板值;显式传了则改写(本地 minikube 测链路用)。
$DsAuthModeToInject = $null
if ($Prod) {
    if ($DsAuthMode -eq 'off') {
        throw "[FATAL] -Prod 不允许 ds_auth.mode=off(完全不校验 DS 回调,任意进程可伪造心跳/战果)。" +
              " 目标用 enforce;DS 未接令牌的过渡窗口用 permissive(仍记录并观测),绝不用 off。"
    }
    if (-not [string]::IsNullOrWhiteSpace($DsAuthMode) -and $DsAuthMode -ne 'enforce') {
        # 走到这里只可能是 permissive(off 已拒、enforce 是目标)。
        Write-Host "[WARN] ⚠️ -Prod 但 ds_auth.mode=permissive:DS 回调**不强制**鉴权,仅记录。" -ForegroundColor Yellow
        Write-Host "       仅限『UE DS 尚未发送令牌』的过渡灰度窗口使用;DS 全量带令牌后必须去掉 -DsAuthMode 回 enforce。" -ForegroundColor Yellow
        Write-Host "       permissive 期间线上任意能连到服务的进程都可伪造 Hub 心跳/战果回调,务必尽快收敛。" -ForegroundColor Yellow
        $DsAuthModeToInject = $DsAuthMode
    } else {
        $DsAuthModeToInject = 'enforce'
    }
} elseif (-not [string]::IsNullOrWhiteSpace($DsAuthMode)) {
    $DsAuthModeToInject = $DsAuthMode
}

# ===== Redis 单一授权权威 + 机械 fence 原子配置门 =====
$DsAuthorityModeToInject = $null
if ($Prod) {
    if ($DsAuthorityMode -ne 'redis') {
        throw '[FATAL] -Prod 必须显式传 -DsAuthorityMode redis；生产不再生成 legacy/K8s-first 授权配置。'
    }
    if ($DsAuthModeToInject -ne 'enforce') {
        throw '[FATAL] authority_mode=redis 只允许 ds_auth.mode=enforce；permissive/off 不能作为授权闭环。'
    }
    $DsAuthorityModeToInject = 'redis'
} elseif (-not [string]::IsNullOrWhiteSpace($DsAuthorityMode)) {
    $DsAuthorityModeToInject = $DsAuthorityMode
}

$DsFenceEndpoints = @()
if (-not [string]::IsNullOrWhiteSpace($DsFenceEtcdEndpoints)) {
    $DsFenceEndpoints = @($DsFenceEtcdEndpoints.Split(',') | ForEach-Object { $_.Trim() } | Where-Object { $_ -ne '' })
}
if ($DsAuthorityModeToInject -eq 'redis') {
    if ($AllocatorMode -ne 'agones') { throw '[FATAL] authority_mode=redis 只允许 allocator mode=agones。' }
    if ($DsAuthModeToInject -ne 'enforce') { throw '[FATAL] authority_mode=redis 必须同时显式使用 ds_auth.mode=enforce。' }
    if ($DsFenceEndpoints.Count -eq 0) { throw '[FATAL] authority_mode=redis 必须提供 -DsFenceEtcdEndpoints。' }
    foreach ($endpoint in $DsFenceEndpoints) {
        if ($endpoint -cnotmatch '^[A-Za-z0-9._:\-\[\]]+$') { throw "[FATAL] 非法 fence etcd endpoint:$endpoint" }
    }
    if ([string]::IsNullOrWhiteSpace($DsFenceKeysetRevision) -or $DsFenceKeysetRevision -cnotmatch '^[A-Za-z0-9._-]+$') {
        throw '[FATAL] authority_mode=redis 必须提供不可变 -DsFenceKeysetRevision(仅字母数字._-)。'
    }
} elseif ($Prod) {
    throw '[FATAL] -Prod 未进入 redis authority，拒绝生成。'
}

# ===== agones 链路必须显式声明生产或本地(审核 P1:agones 不带 -Prod 会写入公开 dev 密钥)=====
# -AllocatorMode agones 指向**真 Linux DS**(线上 k8s 或本地 minikube),不是 docker-mock 假链路。
# 生产/本地必须**显式**二选一,不再从 advertise host 推断(真线上也会配 advertise host/DNS,推断会让
# 生产绕过 -Prod 写入公开 dev 密钥):
#   -Prod            → 注入两把真密钥(见上);
#   -AllowDevSecrets → 显式承认本地/dev 链路,允许沿用公开 dev 密钥(仅限本地 minikube 自测);
#   两者都没有        → 拒绝生成(deny-by-default,防 dev 密钥被静默带上真集群)。
if ($AllocatorMode -eq 'agones' -and -not $Prod) {
    if (-not $AllowDevSecrets) {
        throw "[FATAL] -AllocatorMode agones 指向真 Linux DS 链路,必须显式二选一:线上加 -Prod 并提供两把真密钥;" +
              " 本地 minikube 自测加 -AllowDevSecrets 显式承认沿用公开 dev 密钥 '$DevPublicSecret'。" +
              " 不再从 -AllocatorAdvertiseHost 推断本地(生产也会配 advertise host,推断会让生产绕过 -Prod 泄露 dev 密钥)。"
    }
    Write-Host "[WARN] agones + -AllowDevSecrets:沿用公开 dev 密钥,仅限本地 minikube 自测,勿部署到真集群。" -ForegroundColor Yellow
}

# ===== DSTicket v2(方案 B)机械门 =====
# agones = 真 Linux DS 链路:DS 侧终态只认 RS256 v2 票(2026-07-14 拍板),4 个签发方必须注入
# ds_ticket 段,缺 kid 直接拒绝生成(直接切换,不保留「agones 但仍只签 HS256 票」的半配置形态)。
# mock 链路无真 DS 验票,禁止注入(防半套配置漂移进 docker 链路)。
if ($AllocatorMode -eq 'agones') {
    if ([string]::IsNullOrWhiteSpace($DsTicketActiveKid)) {
        throw '[FATAL] -AllocatorMode agones 必须提供 -DsTicketActiveKid(或环境变量 PANDORA_DSTICKET_ACTIVE_KID):' +
              ' DSTicket v2(RS256)active kid = tools/dsticketkeys 生成的 jwks.json 里的 kid(RFC 7638 指纹)。' +
              ' 方案 B 已拍板为生产终态,agones 链路不再生成纯 HS256 玩家票配置。'
    }
    if ($DsTicketActiveKid -cnotmatch '^[A-Za-z0-9_-]{43}$') {
        throw "[FATAL] -DsTicketActiveKid 必须是 43 字符 base64url(RFC 7638 SHA-256 指纹),实际=$DsTicketActiveKid"
    }
    if ($DsTicketKeysetRevision -cnotmatch '^[1-9][0-9]*$' -or
        [int64]$DsTicketKeysetRevision -gt [int]::MaxValue) {
        throw '[FATAL] -AllocatorMode agones 必须提供正整数 -DsTicketKeysetRevision / PANDORA_DSTICKET_KEYSET_REVISION。'
    }
    $ttlMatch = [regex]::Match($DsTicketTTL, '^([0-9]+)s$')
    if (-not $ttlMatch.Success -or [int]$ttlMatch.Groups[1].Value -lt 30 -or [int]$ttlMatch.Groups[1].Value -gt 180) {
        throw "[FATAL] -DsTicketTTL 必须是 30s..180s 的整数秒(UE 验票机械上限 exp-iat ≤ 180s),实际=$DsTicketTTL"
    }
} elseif (-not [string]::IsNullOrWhiteSpace($DsTicketActiveKid) -or
          -not [string]::IsNullOrWhiteSpace($DsTicketKeysetRevision)) {
    throw '[FATAL] 非 agones 链路(mock)不注入 DSTicket v2 配置,请去掉 active kid / keyset revision。'
}

# base64url(secret):HS256 的 JWKS `k` 字段编码(与 pkg/auth.JWKSInlineHS256 / envoy local_jwks 一致)。
function ConvertTo-Base64Url([string]$s) {
    $bytes = [System.Text.Encoding]::UTF8.GetBytes($s)
    return [Convert]::ToBase64String($bytes).TrimEnd('=').Replace('+', '-').Replace('/', '_')
}

# 把要注入双引号 YAML 字符串的密钥做转义(审核 P1 #8):`\` → `\\`,`"` → `\"`。
# 控制字符已在 Assert-ProdSecret 阶段拒绝,这里只需处理 YAML 双引号里必须转义的两个字符,
# 保证含 `"` / `\` 的合法密钥注入后仍是有效 YAML 且字节不被 yaml.v3 改写。
function ConvertTo-YamlDoubleQuoted([string]$s) {
    return $s.Replace('\', '\\').Replace('"', '\"')
}

# 当前 dev 模板中两类密钥节点的权威服务集合。新增/移动节点必须显式更新本清单；发现未登记节点
# 直接失败，避免正则或目录扫描把玩家密钥误写进 DS 域。
$PlayerSecretServiceNames = @('login', 'matchmaker', 'matchmaker-pve', 'hub-allocator')
$DsSecretServiceNames = @('login', 'ds-allocator', 'hub-allocator', 'battle-result', 'player-locator')
# DSTicket v2(方案 B)签发方清单(与决策文档 §7.2 私钥暴露面一致):恰好等于玩家面 jwt 清单。
$DsTicketServiceNames = @('login', 'matchmaker', 'matchmaker-pve', 'hub-allocator')

# 精确定位 YAML 节点的直接子项(默认 `secret`,也用于 `mode`)。不使用跨段 `.*?`：那会在 jwt 缺 secret 时越过同级
# ds_auth 段，把玩家密钥写进 DS 域。这里只接受 dev 模板约定的双引号标量；格式漂移必须人工审查。
function Get-YamlSectionSecretLocation {
    param(
        [string]$ServiceName,
        [string]$Text,
        [string]$SectionName,
        [string]$ChildName = 'secret'
    )

    $newline = if ($Text.Contains("`r`n")) { "`r`n" } else { "`n" }
    [string[]]$lines = [regex]::Split($Text, '\r?\n')
    $sectionPattern = '^([ ]*)' + [regex]::Escape($SectionName) + ':[ \t]*(?:#.*)?$'
    $sectionIndexes = @(
        for ($i = 0; $i -lt $lines.Count; $i++) {
            if ($lines[$i] -match $sectionPattern) { $i }
        }
    )
    if ($sectionIndexes.Count -ne 1) {
        throw "[FATAL] $ServiceName 期望且只能有一个 $SectionName 节点,实际=$($sectionIndexes.Count)。"
    }

    $sectionIndex = [int]$sectionIndexes[0]
    $sectionMatch = [regex]::Match($lines[$sectionIndex], $sectionPattern)
    $sectionIndent = $sectionMatch.Groups[1].Value.Length
    $end = $lines.Count
    for ($i = $sectionIndex + 1; $i -lt $lines.Count; $i++) {
        if ($lines[$i] -match '^\s*$' -or $lines[$i] -match '^[ ]*#') { continue }
        $indentMatch = [regex]::Match($lines[$i], '^([ ]*)')
        if ($indentMatch.Groups[1].Value.Length -le $sectionIndent) { $end = $i; break }
    }

    $directIndent = [int]::MaxValue
    for ($i = $sectionIndex + 1; $i -lt $end; $i++) {
        if ($lines[$i] -match '^\s*$' -or $lines[$i] -match '^[ ]*#') { continue }
        $indentMatch = [regex]::Match($lines[$i], '^([ ]*)')
        $indent = $indentMatch.Groups[1].Value.Length
        if ($indent -gt $sectionIndent -and $indent -lt $directIndent) { $directIndent = $indent }
    }
    if ($directIndent -eq [int]::MaxValue) { throw "[FATAL] $ServiceName.$SectionName 是空节点。" }

    $childPattern = '^([ ]*)' + [regex]::Escape($ChildName) + '[ \t]*:'
    $secretIndexes = @(
        for ($i = $sectionIndex + 1; $i -lt $end; $i++) {
            $m = [regex]::Match($lines[$i], $childPattern)
            if ($m.Success -and $m.Groups[1].Value.Length -eq $directIndent) { $i }
        }
    )
    if ($secretIndexes.Count -ne 1) {
        throw "[FATAL] $ServiceName.$SectionName 期望且只能有一个直接子项 $ChildName,实际=$($secretIndexes.Count)。"
    }
    $secretIndex = [int]$secretIndexes[0]
    $valuePattern = '^([ ]*' + [regex]::Escape($ChildName) + '[ \t]*:[ \t]*)"((?:\\.|[^"])*)"([ \t]*(?:#.*)?)$'
    $valueMatch = [regex]::Match($lines[$secretIndex], $valuePattern)
    if (-not $valueMatch.Success) {
        throw "[FATAL] $ServiceName.$SectionName.$ChildName 必须是单行双引号标量,拒绝模糊替换:$($lines[$secretIndex])"
    }
    return [pscustomobject]@{
        Lines        = $lines
        Newline      = $newline
        SecretIndex  = $secretIndex
        Prefix       = $valueMatch.Groups[1].Value
        RawValue     = $valueMatch.Groups[2].Value
        Suffix       = $valueMatch.Groups[3].Value
        SectionIndex = $sectionIndex
        SectionEnd   = $end
        DirectIndent = $directIndent
    }
}

function Assert-YamlSectionSecret {
    param(
        [string]$ServiceName,
        [string]$Text,
        [string]$SectionName,
        [string]$ExpectedValue
    )
    $location = Get-YamlSectionSecretLocation $ServiceName $Text $SectionName
    $expectedRaw = ConvertTo-YamlDoubleQuoted $ExpectedValue
    if ($location.RawValue -cne $expectedRaw) {
        throw "[FATAL] $ServiceName.$SectionName.secret 与期望密钥域不一致。"
    }
}

function Set-YamlSectionSecret {
    param(
        [string]$ServiceName,
        [string]$Text,
        [string]$SectionName,
        [string]$NewValue
    )
    $location = Get-YamlSectionSecretLocation $ServiceName $Text $SectionName
    if ($location.RawValue -cne $DevPublicSecret) {
        throw "[FATAL] $ServiceName.$SectionName.secret 不等于权威 dev 模板值,拒绝把未知旧值静默覆盖。"
    }
    $location.Lines[$location.SecretIndex] = $location.Prefix + '"' + (ConvertTo-YamlDoubleQuoted $NewValue) + '"' + $location.Suffix
    return ($location.Lines -join $location.Newline)
}

# 在节段 secret 行之后插入 `additional_secrets: ["<旧密钥>"]`(同缩进,flow 列表)。
# dev 模板不带此字段;若节段内已存在直接子项 additional_secrets 则拒绝(防重复注入/模板漂移)。
function Add-YamlSectionAdditionalSecrets {
    param(
        [string]$ServiceName,
        [string]$Text,
        [string]$SectionName,
        [string]$AdditionalValue
    )
    $location = Get-YamlSectionSecretLocation $ServiceName $Text $SectionName
    $addPattern = '^([ ]*)additional_secrets[ \t]*:'
    for ($i = $location.SectionIndex + 1; $i -lt $location.SectionEnd; $i++) {
        $m = [regex]::Match($location.Lines[$i], $addPattern)
        if ($m.Success -and $m.Groups[1].Value.Length -eq $location.DirectIndent) {
            throw "[FATAL] $ServiceName.$SectionName 已存在 additional_secrets,拒绝重复注入。"
        }
    }
    $indent = ' ' * $location.DirectIndent
    $newLine = $indent + 'additional_secrets: ["' + (ConvertTo-YamlDoubleQuoted $AdditionalValue) + '"]'
    $lines = [System.Collections.Generic.List[string]]::new($location.Lines)
    $lines.Insert($location.SecretIndex + 1, $newLine)
    return (@($lines) -join $location.Newline)
}

function Assert-YamlSectionAdditionalSecrets {
    param(
        [string]$ServiceName,
        [string]$Text,
        [string]$SectionName,
        [string]$ExpectedValue
    )
    $location = Get-YamlSectionSecretLocation $ServiceName $Text $SectionName
    $expectedLine = (' ' * $location.DirectIndent) + 'additional_secrets: ["' + (ConvertTo-YamlDoubleQuoted $ExpectedValue) + '"]'
    $found = $false
    for ($i = $location.SectionIndex + 1; $i -lt $location.SectionEnd; $i++) {
        if ($location.Lines[$i] -cmatch '^[ ]*additional_secrets[ \t]*:') {
            if ($location.Lines[$i] -cne $expectedLine) {
                throw "[FATAL] $ServiceName.$SectionName.additional_secrets 与期望轮换旧密钥不一致。"
            }
            $found = $true
        }
    }
    if (-not $found) { throw "[FATAL] $ServiceName.$SectionName 缺少期望的 additional_secrets。" }
}

# P0(三审 #1):断言节段内**不存在**任何 additional_secrets 直接子项。
# 生产/任意不注入 additional 的路径都必须调用它:dev 模板此刻虽不带该字段,但生成器绝不能
# 依赖模板“恰好没写”。若将来有人往 dev 模板预置 additional_secrets:["<dev 公钥>"],
# 而 -Prod 又(因轮换决策未拍板)跳过 additional 注入,旧逻辑会把该行原样带进线上产物 =
# dev 公钥泄漏进生产。这里一律 fail-closed:发现未经本次注入的 additional_secrets 直接拒绝生成。
function Assert-NoYamlSectionAdditionalSecrets {
    param(
        [string]$ServiceName,
        [string]$Text,
        [string]$SectionName
    )
    $location = Get-YamlSectionSecretLocation $ServiceName $Text $SectionName
    for ($i = $location.SectionIndex + 1; $i -lt $location.SectionEnd; $i++) {
        if ($location.Lines[$i] -cmatch '^[ ]*additional_secrets[ \t]*:') {
            throw "[FATAL] $ServiceName.$SectionName 存在未经本次注入的 additional_secrets(模板预置?),拒绝生成。" +
                  " 生产轮换密钥只能由 -SecretAdditional / -DsSecretAdditional 显式注入,不得由模板携带(防 dev 公钥泄漏进线上)。"
        }
    }
}

# 改写 ds_auth.mode(二审 A#2)。只接受已知合法旧值(off/permissive/enforce),防模板漂移静默覆盖。
function Set-YamlDsAuthMode {
    param(
        [string]$ServiceName,
        [string]$Text,
        [string]$NewMode
    )
    $location = Get-YamlSectionSecretLocation $ServiceName $Text 'ds_auth' 'mode'
    if ($location.RawValue -cnotin @('off', 'permissive', 'enforce')) {
        throw "[FATAL] $ServiceName.ds_auth.mode 旧值非法(=$($location.RawValue)),拒绝改写。"
    }
    $location.Lines[$location.SecretIndex] = $location.Prefix + '"' + $NewMode + '"' + $location.Suffix
    return ($location.Lines -join $location.Newline)
}

function Assert-YamlDsAuthMode {
    param(
        [string]$ServiceName,
        [string]$Text,
        [string]$ExpectedMode
    )
    $location = Get-YamlSectionSecretLocation $ServiceName $Text 'ds_auth' 'mode'
    if ($location.RawValue -cne $ExpectedMode) {
        throw "[FATAL] $ServiceName.ds_auth.mode=$($location.RawValue) 与期望值 $ExpectedMode 不一致。"
    }
}

function Set-YamlDsAuthorityMode {
    param([string]$ServiceName, [string]$Text, [string]$NewMode)
    $location = Get-YamlSectionSecretLocation $ServiceName $Text 'ds_auth' 'authority_mode'
    if ($location.RawValue -cnotin @('legacy', 'redis')) {
        throw "[FATAL] $ServiceName.ds_auth.authority_mode 旧值非法(=$($location.RawValue))。"
    }
    $location.Lines[$location.SecretIndex] = $location.Prefix + '"' + $NewMode + '"' + $location.Suffix
    return ($location.Lines -join $location.Newline)
}

function Add-YamlDsAuthFence {
    param([string]$ServiceName, [string]$Text)
    $location = Get-YamlSectionSecretLocation $ServiceName $Text 'ds_auth' 'secret'
    for ($i = $location.SectionIndex + 1; $i -lt $location.SectionEnd; $i++) {
        if ($location.Lines[$i] -cmatch ('^' + (' ' * $location.DirectIndent) + 'fence[ \t]*:')) {
            throw "[FATAL] $ServiceName.ds_auth 已有活动 fence 节点，拒绝重复/模糊覆盖。"
        }
    }
    $indent = ' ' * $location.DirectIndent
    $nested = ' ' * ($location.DirectIndent + 2)
    $endpointValues = @($DsFenceEndpoints | ForEach-Object { '"' + (ConvertTo-YamlDoubleQuoted $_) + '"' })
    $lines = [System.Collections.Generic.List[string]]::new($location.Lines)
    $insertAt = $location.SecretIndex + 1
    [string[]]$newFenceLines = @(
        ($indent + 'fence:')
        ($nested + 'etcd_endpoints: [' + ($endpointValues -join ', ') + ']')
        ($nested + 'etcd_prefix: "/pandora/ds-auth/"')
        ($nested + 'etcd_lease_ttl_sec: 15')
        ($nested + 'etcd_dial_timeout: "5s"')
        ($nested + 'keyset_revision: "' + (ConvertTo-YamlDoubleQuoted $DsFenceKeysetRevision) + '"')
    )
    foreach ($line in $newFenceLines) {
        $lines.Insert($insertAt, $line)
        $insertAt++
    }
    return (@($lines) -join $location.Newline)
}

# 精确定位 battle.consume_topics 的 block list。Redis authority 下无凭据的
# pandora.battle.result 会绕过 Guard/active/receipt，故生成器必须机械删掉，不能靠人工改 YAML。
function Get-BattleResultConsumeTopicsLocation {
    param([string]$Text)

    $newline = if ($Text.Contains("`r`n")) { "`r`n" } else { "`n" }
    [string[]]$lines = [regex]::Split($Text, '\r?\n')
    $sectionIndexes = @(
        for ($i = 0; $i -lt $lines.Count; $i++) {
            if ($lines[$i] -cmatch '^battle:[ \t]*(?:#.*)?$') { $i }
        }
    )
    if ($sectionIndexes.Count -ne 1) {
        throw "[FATAL] battle-result 期望且只能有一个顶级 battle 节点,实际=$($sectionIndexes.Count)。"
    }
    $sectionIndex = [int]$sectionIndexes[0]
    $sectionEnd = $lines.Count
    for ($i = $sectionIndex + 1; $i -lt $lines.Count; $i++) {
        if ($lines[$i] -cmatch '^\S') { $sectionEnd = $i; break }
    }
    $headerIndexes = @(
        for ($i = $sectionIndex + 1; $i -lt $sectionEnd; $i++) {
            if ($lines[$i] -cmatch '^[ ]{2}consume_topics:[ \t]*(?:#.*)?$') { $i }
        }
    )
    if ($headerIndexes.Count -ne 1) {
        throw "[FATAL] battle-result.battle.consume_topics 期望唯一 block list,实际=$($headerIndexes.Count)。"
    }
    $headerIndex = [int]$headerIndexes[0]
    $entryIndexes = [System.Collections.Generic.List[int]]::new()
    $values = [System.Collections.Generic.List[string]]::new()
    for ($i = $headerIndex + 1; $i -lt $sectionEnd; $i++) {
        $match = [regex]::Match($lines[$i], '^[ ]{4}-[ \t]*"([^"]+)"[ \t]*(?:#.*)?$')
        if (-not $match.Success) { break }
        $entryIndexes.Add($i)
        $values.Add($match.Groups[1].Value)
    }
    if ($entryIndexes.Count -eq 0) {
        throw '[FATAL] battle-result.battle.consume_topics 必须至少有一个双引号列表项。'
    }
    return [pscustomobject]@{
        Lines        = $lines
        Newline      = $newline
        HeaderIndex  = $headerIndex
        EntryIndexes = @($entryIndexes)
        Values       = @($values)
    }
}

function Set-BattleResultRedisAuthorityIngress {
    param([string]$Text)
    $location = Get-BattleResultConsumeTopicsLocation $Text
    $allowed = @('pandora.battle.result', 'pandora.ds.lifecycle')
    foreach ($value in $location.Values) {
        if ($value -cnotin $allowed) {
            throw "[FATAL] battle-result.consume_topics 含未知旧值 $value,拒绝模糊改写。"
        }
    }
    if (@($location.Values | Group-Object | Where-Object Count -gt 1).Count -ne 0) {
        throw '[FATAL] battle-result.consume_topics 存在重复值,拒绝改写。'
    }
    if ($location.Values -cnotcontains 'pandora.ds.lifecycle') {
        throw '[FATAL] battle-result.consume_topics 缺 pandora.ds.lifecycle,不能生成 Redis authority。'
    }
    $lines = [System.Collections.Generic.List[string]]::new($location.Lines)
    $first = [int]$location.EntryIndexes[0]
    $lines.RemoveRange($first, $location.EntryIndexes.Count)
    $lines.Insert($first, '    - "pandora.ds.lifecycle"')
    return (@($lines) -join $location.Newline)
}

function Assert-BattleResultConsumeTopics {
    param([string]$Text, [string[]]$Expected)
    $location = Get-BattleResultConsumeTopicsLocation $Text
    if ($location.Values.Count -ne $Expected.Count) {
        throw "[FATAL] battle-result.consume_topics 数量错误:actual=[$($location.Values -join ',')] expected=[$($Expected -join ',')]"
    }
    for ($i = 0; $i -lt $Expected.Count; $i++) {
        if ($location.Values[$i] -cne $Expected[$i]) {
            throw "[FATAL] battle-result.consume_topics 错误:actual=[$($location.Values -join ',')] expected=[$($Expected -join ',')]"
        }
    }
}

function Set-LoginHubAssignmentBinding {
    param([string]$Text, [bool]$Enabled)
    $location = Get-YamlSectionSecretLocation 'login' $Text 'login' 'mock_hub_ds_addr'
    $pattern = '^([ ]*)require_hub_assignment_binding[ \t]*:[ \t]*(true|false)([ \t]*(?:#.*)?)$'
    $hits = @()
    for ($i = $location.SectionIndex + 1; $i -lt $location.SectionEnd; $i++) {
        $match = [regex]::Match($location.Lines[$i], $pattern)
        if ($match.Success -and $match.Groups[1].Value.Length -eq $location.DirectIndent) { $hits += $i }
    }
    if ($hits.Count -ne 1) { throw "[FATAL] login.require_hub_assignment_binding 期望唯一，实际=$($hits.Count)。" }
    $index = [int]$hits[0]
    $match = [regex]::Match($location.Lines[$index], $pattern)
    $location.Lines[$index] = $match.Groups[1].Value + 'require_hub_assignment_binding: ' + $(if ($Enabled) { 'true' } else { 'false' }) + $match.Groups[3].Value
    $fencePattern = '^' + (' ' * $location.DirectIndent) + 'hub_assignment_fence[ \t]*:'
    $activeFence = @($location.Lines | Where-Object { $_ -cmatch $fencePattern })
    if ($activeFence.Count -ne 0) { throw '[FATAL] login 模板已含活动 hub_assignment_fence，拒绝模糊覆盖。' }
    if (-not $Enabled) { return ($location.Lines -join $location.Newline) }
    $nested = ' ' * ($location.DirectIndent + 2)
    $endpointValues = @($DsFenceEndpoints | ForEach-Object { '"' + (ConvertTo-YamlDoubleQuoted $_) + '"' })
    $lines = [System.Collections.Generic.List[string]]::new($location.Lines)
    $insertAt = $index + 1
    [string[]]$newFenceLines = @(
        ((' ' * $location.DirectIndent) + 'hub_assignment_fence:')
        ($nested + 'etcd_endpoints: [' + ($endpointValues -join ', ') + ']')
        ($nested + 'etcd_prefix: "/pandora/ds-auth/"')
        ($nested + 'etcd_lease_ttl_sec: 15')
        ($nested + 'etcd_dial_timeout: "5s"')
        ($nested + 'keyset_revision: "' + (ConvertTo-YamlDoubleQuoted $DsFenceKeysetRevision) + '"')
    )
    foreach ($line in $newFenceLines) {
        $lines.Insert($insertAt, $line)
        $insertAt++
    }
    return (@($lines) -join $location.Newline)
}

function Assert-YamlRedisAuthorityBundle {
    param([string]$ServiceName, [string]$Text)
    $authority = Get-YamlSectionSecretLocation $ServiceName $Text 'ds_auth' 'authority_mode'
    if ($authority.RawValue -cne 'redis') { throw "[FATAL] $ServiceName authority_mode 未原子切到 redis。" }
    foreach ($needle in @(
        'fence:', 'etcd_prefix: "/pandora/ds-auth/"', 'etcd_lease_ttl_sec: 15',
        'etcd_dial_timeout: "5s"', 'keyset_revision: "' + (ConvertTo-YamlDoubleQuoted $DsFenceKeysetRevision) + '"'
    )) {
        if (-not $Text.Contains($needle)) { throw "[FATAL] $ServiceName 缺少 fence 字段:$needle" }
    }
    foreach ($endpoint in $DsFenceEndpoints) {
        if (-not $Text.Contains('"' + (ConvertTo-YamlDoubleQuoted $endpoint) + '"')) {
            throw "[FATAL] $ServiceName fence 缺 endpoint:$endpoint"
        }
    }
    $direct = ' ' * $authority.DirectIndent
    $nested = ' ' * ($authority.DirectIndent + 2)
    # 只在 ds_auth section 内计数；login 同时有 login.hub_assignment_fence，若对整文件
    # 数缩进相同的 etcd_endpoints 会把合法双 fence 误判成重复。
    $sectionLines = $authority.Lines[$authority.SectionIndex..($authority.SectionEnd - 1)]
    $sectionText = $sectionLines -join $authority.Newline
    if (([regex]::Matches($sectionText, '(?m)^' + [regex]::Escape($direct) + 'fence:[ \t]*\r?$')).Count -ne 1 -or
        ([regex]::Matches($sectionText, '(?m)^' + [regex]::Escape($nested) + 'etcd_endpoints:[ \t]*\[.+\][ \t]*\r?$')).Count -ne 1) {
        throw "[FATAL] $ServiceName fence 必须是唯一的嵌套 YAML 节点，拒绝同一行/错误缩进。"
    }
}

function Assert-LoginHubAssignmentBinding {
    param([string]$Text, [bool]$Expected)
    $want = 'require_hub_assignment_binding: ' + $(if ($Expected) { 'true' } else { 'false' })
    if (([regex]::Matches($Text, '(?m)^[ ]*require_hub_assignment_binding[ \t]*:[ \t]*(?:true|false)[ \t]*(?:#.*)?$')).Count -ne 1 -or -not $Text.Contains($want)) {
        throw "[FATAL] login assignment binding 与 authority bundle 不一致，期望=$Expected。"
    }
    if ($Expected) {
        foreach ($needle in @('hub_assignment_fence:', 'etcd_prefix: "/pandora/ds-auth/"', 'keyset_revision: "' + (ConvertTo-YamlDoubleQuoted $DsFenceKeysetRevision) + '"')) {
            if (-not $Text.Contains($needle)) { throw "[FATAL] login 缺少 assignment fence 字段:$needle" }
        }
        if (([regex]::Matches($Text, '(?m)^[ ]{2}hub_assignment_fence:[ \t]*\r?$')).Count -ne 1 -or
            ([regex]::Matches($Text, '(?m)^[ ]{4}etcd_endpoints:[ \t]*\[.+\][ \t]*\r?$')).Count -lt 1) {
            throw '[FATAL] login hub_assignment_fence 必须是嵌套 YAML 节点。'
        }
    }
}

# ===== DSTicket v2(方案 B,RS256)签发段注入 =====
# 在唯一 jwt: 节段之后、同缩进插入 ds_ticket 段(matchmaker/hub-allocator 顶级;login 在 login: 段内,
# 与其 jwt 同级,与 Go 侧 conf 结构一致)。dev 模板不带 ds_ticket(本地/docker 链路沿用 legacy HS256
# + local-off);agones 链路由本生成器机械注入,不靠人工改 YAML。
function Add-YamlDsTicketSection {
    param([string]$ServiceName, [string]$Text)
    if ([regex]::IsMatch($Text, '(?m)^[ ]*ds_ticket:[ \t]*(?:#.*)?\r?$')) {
        throw "[FATAL] $ServiceName 模板已存在 ds_ticket 节点,拒绝重复/模糊注入。"
    }
    $location = Get-YamlSectionSecretLocation $ServiceName $Text 'jwt' 'secret'
    $sectionIndent = [regex]::Match($location.Lines[$location.SectionIndex], '^([ ]*)').Groups[1].Value
    $nested = $sectionIndent + '  '
    $lines = [System.Collections.Generic.List[string]]::new($location.Lines)
    $insertAt = [int]$location.SectionEnd
    [string[]]$newLines = @(
        ($sectionIndent + 'ds_ticket:')
        ($nested + 'private_key_file: "/run/secrets/pandora-dsticket/private.pem"')
        ($nested + 'active_kid: "' + $DsTicketActiveKid + '"')
        ($nested + 'ttl: "' + $DsTicketTTL + '"')
    )
    if ($ServiceName -ceq 'login') {
        # Login 仍是签票方，但 VerifyDSTicket 诊断/兼容路径只读公开 overlap JWKS。
        # ConfigMap 为 namespaced 对象：Login 在 pandora，Fleet/DS 在 default，bootstrap 会在
        # 两个 namespace 建立内容和 hash 完全相同的 immutable ConfigMap。
        $newLines += @(
            ($nested + 'jwks_file: "/run/config/pandora-dsticket/jwks.json"')
            ($nested + 'keyset_revision: "' + $DsTicketKeysetRevision + '"')
        )
    }
    foreach ($line in $newLines) { $lines.Insert($insertAt, $line); $insertAt++ }
    return (@($lines) -join $location.Newline)
}

function Assert-YamlDsTicketSection {
    param([string]$ServiceName, [string]$Text)
    if (([regex]::Matches($Text, '(?m)^[ ]*ds_ticket:[ \t]*\r?$')).Count -ne 1) {
        throw "[FATAL] $ServiceName 期望唯一 ds_ticket 节段。"
    }
    foreach ($needle in @(
        'private_key_file: "/run/secrets/pandora-dsticket/private.pem"',
        ('active_kid: "' + $DsTicketActiveKid + '"'),
        ('ttl: "' + $DsTicketTTL + '"')
    )) {
        if (-not $Text.Contains($needle)) { throw "[FATAL] $ServiceName ds_ticket 段缺字段:$needle" }
    }
    if ($ServiceName -ceq 'login') {
        foreach ($needle in @(
            'jwks_file: "/run/config/pandora-dsticket/jwks.json"',
            ('keyset_revision: "' + $DsTicketKeysetRevision + '"')
        )) {
            if (-not $Text.Contains($needle)) { throw "[FATAL] login ds_ticket verifier 缺字段:$needle" }
        }
    } elseif ($Text.Contains('jwks_file: "/run/config/pandora-dsticket/jwks.json"')) {
        throw "[FATAL] $ServiceName 不应启用 Login-only DSTicket verifier 挂载。"
    }
}

function Assert-NoYamlDsTicketSection {
    param([string]$ServiceName, [string]$Text)
    if ([regex]::IsMatch($Text, '(?m)^[ ]*ds_ticket:[ \t]*(?:#.*)?\r?$')) {
        throw "[FATAL] $ServiceName 非 agones 链路不得携带 ds_ticket 节点(模板漂移?),拒绝生成。"
    }
}

function Convert-Secret([string]$ServiceName, [string]$Text) {
    $hasPlayerSection = [regex]::IsMatch($Text, '(?m)^[ ]*jwt:[ \t]*(?:#.*)?\r?$')
    $hasDsSection = [regex]::IsMatch($Text, '(?m)^[ ]*ds_auth:[ \t]*(?:#.*)?\r?$')
    $expectsPlayer = $PlayerSecretServiceNames -contains $ServiceName
    $expectsDs = $DsSecretServiceNames -contains $ServiceName
    if ($hasPlayerSection -ne $expectsPlayer) { throw "[FATAL] $ServiceName 的 jwt 节点与权威服务清单不一致。" }
    if ($hasDsSection -ne $expectsDs) { throw "[FATAL] $ServiceName 的 ds_auth 节点与权威服务清单不一致。" }

    if ($expectsPlayer) {
        if ($null -ne $PlayerSecretToInject) { $Text = Set-YamlSectionSecret $ServiceName $Text 'jwt' $PlayerSecretToInject }
        else { Assert-YamlSectionSecret $ServiceName $Text 'jwt' $DevPublicSecret }
        if ($null -ne $PlayerAdditionalToInject) { $Text = Add-YamlSectionAdditionalSecrets $ServiceName $Text 'jwt' $PlayerAdditionalToInject }
        else { Assert-NoYamlSectionAdditionalSecrets $ServiceName $Text 'jwt' }
    }
    if ($expectsDs) {
        if ($null -ne $DsSecretToInject) { $Text = Set-YamlSectionSecret $ServiceName $Text 'ds_auth' $DsSecretToInject }
        else { Assert-YamlSectionSecret $ServiceName $Text 'ds_auth' $DevPublicSecret }
        if ($null -ne $DsAdditionalToInject) { $Text = Add-YamlSectionAdditionalSecrets $ServiceName $Text 'ds_auth' $DsAdditionalToInject }
        else { Assert-NoYamlSectionAdditionalSecrets $ServiceName $Text 'ds_auth' }
        if ($null -ne $DsAuthModeToInject) { $Text = Set-YamlDsAuthMode $ServiceName $Text $DsAuthModeToInject }
        if ($null -ne $DsAuthorityModeToInject) { $Text = Set-YamlDsAuthorityMode $ServiceName $Text $DsAuthorityModeToInject }
        if ($DsAuthorityModeToInject -eq 'redis') { $Text = Add-YamlDsAuthFence $ServiceName $Text }
    }
    if ($ServiceName -eq 'login' -and $null -ne $DsAuthorityModeToInject) {
        $Text = Set-LoginHubAssignmentBinding $Text ($DsAuthorityModeToInject -eq 'redis')
    }
    if ($DsTicketServiceNames -contains $ServiceName) {
        if ($AllocatorMode -eq 'agones') { $Text = Add-YamlDsTicketSection $ServiceName $Text }
        else { Assert-NoYamlDsTicketSection $ServiceName $Text }
    }
    return $Text
}
# Sync-EnvoyJwks:注入真密钥时,必须同步 Envoy 客户端面(:8443)的 local_jwks,否则 login 用新密钥
# 签的 SessionToken 会被 envoy(仍是 dev JWKS)全部拒掉。做两件事(§5 审核:生成器要同步/校验 JWKS):
#   1) 把匹配的 JWKS(base64url(secret))写进 <OutDir>/envoy-jwks.json(run/ 已 gitignore,不落库),
#      供运维把边缘 Envoy 的 inline_string 换成它 / 或挂载覆盖。
#   2) 校验仓库内 deploy/envoy/envoy.yaml:若它仍内联 dev JWKS 而我们注入了新密钥,大声告警
#      (不自动改committed 文件:严禁把真密钥写进 git 跟踪文件,AGENTS.md §3/§10)。
# 密钥指纹(与 pkg/auth.keyFingerprint 同源:hex(SHA256(secret)[:8]),16 个小写 hex)。
# 多 key JWKS 的 kid 用它:与 Go 侧 SignDSCallback 写入的 kid 一致,日志/审计可对应到具体密钥。
function Get-KeyFingerprint([string]$s) {
    $h = [System.Security.Cryptography.SHA256]::HashData([System.Text.Encoding]::UTF8.GetBytes($s))
    return ([System.BitConverter]::ToString($h[0..7]) -replace '-', '').ToLowerInvariant()
}

# 组装玩家面 JWKS 文本:单 key 保持历史格式(kid=pandora-dev)不变;带轮换旧密钥时双 key,
# kid 用各自密钥指纹(HS256 验签不依赖 kid 匹配,Envoy 会逐 key 尝试;kid 仅供审计)。
function Get-PlayerJwksText {
    $k = ConvertTo-Base64Url $PlayerSecretToInject
    if ($null -eq $PlayerAdditionalToInject) {
        return '{"keys":[{"kty":"oct","alg":"HS256","kid":"pandora-dev","k":"' + $k + '"}]}'
    }
    $kAdd = ConvertTo-Base64Url $PlayerAdditionalToInject
    return '{"keys":[' +
        '{"kty":"oct","alg":"HS256","kid":"' + (Get-KeyFingerprint $PlayerSecretToInject) + '","k":"' + $k + '"},' +
        '{"kty":"oct","alg":"HS256","kid":"' + (Get-KeyFingerprint $PlayerAdditionalToInject) + '","k":"' + $kAdd + '"}]}'
}

function Sync-EnvoyJwks([string]$TargetDir) {
    # Envoy :8443 客户端面校验的是**玩家面**令牌(SessionToken / DSTicket),故 JWKS 用玩家面密钥。
    $jwksPath = Join-Path $TargetDir 'envoy-jwks.json'
    if ($null -eq $PlayerSecretToInject) {
        # staging 中不生成 JWKS；发布事务会删除最终目录中由本生成器拥有的陈旧 JWKS。
        return
    }
    $jwks = Get-PlayerJwksText
    [System.IO.File]::WriteAllText($jwksPath, $jwks + "`n", (New-Object System.Text.UTF8Encoding($false)))
    Write-Host "[ OK ] staging 已生成匹配的 Envoy JWKS(keys=$(if ($null -ne $PlayerAdditionalToInject) { 2 } else { 1 }));事务发布成功后路径=$(Join-Path $OutDir 'envoy-jwks.json')" -ForegroundColor Green

    # 校验 committed envoy.yaml 是否仍是 dev JWKS。
    $envoyYaml = Join-Path $ProjectRoot 'deploy/envoy/envoy.yaml'
    if (Test-Path $envoyYaml) {
        $devK = ConvertTo-Base64Url $DevPublicSecret
        $k = ConvertTo-Base64Url $PlayerSecretToInject
        $ec = Get-Content $envoyYaml -Raw
        if ($ec.Contains($devK) -and -not $ec.Contains($k)) {
            Write-Host "[WARN] deploy/envoy/envoy.yaml 仍内联 dev JWKS(k=$devK)。生产必须改用上面的 envoy-jwks.json," -ForegroundColor Yellow
            Write-Host "       否则 login 用新密钥签的 SessionToken 会被 Envoy 全部拒绝。(不自动改 committed 文件:严禁把真密钥写进 git)" -ForegroundColor Yellow
        }
    }
}


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
    @{ Name = 'leaderboard';    Conf = 'services/runtime/leaderboard/etc/leaderboard-dev.yaml';    Port = 50007 }
    @{ Name = 'guild';          Conf = 'services/social/guild/etc/guild-dev.yaml';                 Port = 50008 }
    @{ Name = 'mail';           Conf = 'services/social/mail/etc/mail-dev.yaml';                   Port = 50009 }
    @{ Name = 'team';           Conf = 'services/matchmaking/team/etc/team-dev.yaml';              Port = 50010 }
    @{ Name = 'matchmaker';     Conf = 'services/matchmaking/matchmaker/etc/matchmaker-dev.yaml';  Port = 50011 }
    # PVE 直进匹配实例:同 matchmaker 二进制、不同配置(game_mode=pve_coop + enable_solo_match)。
    @{ Name = 'matchmaker-pve'; Conf = 'services/matchmaking/matchmaker/etc/matchmaker-pve.yaml';  Port = 50018 }
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

# 混合(含战斗)模式:ds/hub allocator 跑宿主而非容器,把它们的同伴地址从 docker 服务名
# (ds-allocator/hub-allocator)改指 host.docker.internal —— 容器内经该名回连宿主发布端口。
# 只影响调用方(matchmaker/battle_result→50020、login→50021)的地址改写,不改 allocator 自身。
if ($HostAllocators) {
    $PortToHost['50020'] = 'host.docker.internal'
    $PortToHost['50021'] = 'host.docker.internal'
}

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

# auction 只要进入 compose / k8s 集群就可能与旧副本或扩容副本并行运行。
# 集群产物必须使用 etcd 独占 snowflake nodeID,并开启跨实例 market 锁；dev 源配置仍保留
# static + 单实例锁,避免本机只启动一个服务时额外依赖 etcd。
function Set-AuctionClusterSafety([string]$text) {
    $snowflakeHeaderCount = [regex]::Matches($text, '(?m)^snowflake:[ \t]*$').Count
    $stepBitsCount = [regex]::Matches($text, '(?m)^[ \t]{2}step_bits:[ \t]*\d+[ \t]*$').Count
    $crossLockCount = [regex]::Matches($text, '(?m)^[ \t]{2}cross_instance_lock:[ \t]*(?:true|false)[ \t]*$').Count
    if ($snowflakeHeaderCount -ne 1 -or $stepBitsCount -ne 1 -or $crossLockCount -ne 1) {
        throw "[FATAL] auction 集群安全配置锚点异常:snowflake=$snowflakeHeaderCount step_bits=$stepBitsCount cross_instance_lock=$crossLockCount"
    }

    if ([regex]::IsMatch($text, '(?m)^\s{2}node_id_source:')) {
        throw '[FATAL] auction dev 配置已显式设置 node_id_source,请人工确认集群改写规则后再生成。'
    }

    $text = [regex]::Replace(
        $text,
        '(?m)^([ \t]{2}step_bits:[ \t]*\d+[ \t]*)$',
        "`$1`n  node_id_source: etcd`n  etcd_endpoints: [`"etcd:2379`"]`n  etcd_prefix: `"/pandora/snowflake/node/`"`n  etcd_service_name: `"auction`"`n  etcd_lease_ttl_sec: 15",
        1)
    $text = [regex]::Replace(
        $text,
        '(?m)^([ \t]{2}cross_instance_lock:)[ \t]*(?:true|false)[ \t]*$',
        '$1 true',
        1)
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
        $fleet = 'pandora-hub-stable'
        $canaryFleet = 'pandora-hub-canary'
        $canaryPercent = $HubCanaryPercent
        $timeoutLine = '  list_timeout: "5s"'
    } else {
        $fleet = 'pandora-battle-stable'
        $canaryFleet = 'pandora-battle-canary'
        $canaryPercent = $BattleCanaryPercent
        $timeoutLine = '  allocate_timeout: "5s"'
    }

    # 组装 in-cluster agones 段(投影 token/ca 自动轮转,allocator 每次请求重读 token 文件)
    $lines = @(
        'agones:'
        '  enabled: true'
        '  api_server: "https://kubernetes.default.svc"'
        '  namespace: "default"'
        "  fleet_name: `"$fleet`""
        "  canary_fleet_name: `"$canaryFleet`""
        "  canary_percent: $canaryPercent"
        ('  canary_seed: "' + $CanarySeed + '"')
        '  token_path: "/var/run/secrets/kubernetes.io/serviceaccount/token"'
        '  ca_path: "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"'
        '  insecure_skip_tls_verify: false'
    )
    # advertise_host:本地 minikube(docker driver)用 127.0.0.1 + udp-relay 回程;线上留空用 status.address
    if (-not [string]::IsNullOrWhiteSpace($AllocatorAdvertiseHost)) {
        $lines += "  advertise_host: `"$AllocatorAdvertiseHost`""
    }
    $lines += $timeoutLine
    # ds-allocator 专属:Fleet 容量巡检(快到上限预警),hub-allocator 无此项
    if ($svcName -ne 'hub-allocator') {
        $lines += '  capacity_watch_interval: "30s"'
        $lines += '  capacity_warn_ratio: 0.8'
    }
    $agonesBlock = ($lines -join "`n") + "`n`n"

    # 把原 dev 的整个 agones: 段(直到下一个顶级注释块「# 本机拉起」)整块替换
    $text = [regex]::Replace($text, '(?ms)^agones:\r?\n.*?(?=^# 本机拉起)', $agonesBlock)
    return $text
}

function Assert-GeneratedSet {
    param(
        [string]$StageDir,
        [string[]]$ExpectedNames
    )

    $duplicates = @($ExpectedNames | Group-Object | Where-Object Count -gt 1)
    if ($duplicates.Count -ne 0) {
        throw "[FATAL] 生成器期望文件名重复:$($duplicates.Name -join ', ')"
    }

    $items = @(Get-ChildItem -LiteralPath $StageDir -Force)
    $directories = @($items | Where-Object { $_.PSIsContainer })
    if ($directories.Count -ne 0) {
        throw "[FATAL] staging 中出现非预期目录:$($directories.Name -join ', ')"
    }
    $actualNames = @($items | ForEach-Object Name | Sort-Object)
    $expectedSorted = @($ExpectedNames | Sort-Object)
    $setDiff = @(Compare-Object -ReferenceObject $expectedSorted -DifferenceObject $actualNames -CaseSensitive)
    if ($setDiff.Count -ne 0) {
        $missing = @($setDiff | Where-Object SideIndicator -eq '<=' | ForEach-Object InputObject)
        $extra = @($setDiff | Where-Object SideIndicator -eq '=>' | ForEach-Object InputObject)
        throw "[FATAL] staging 文件集不完整:缺少=[$($missing -join ', ')],多余=[$($extra -join ', ')]"
    }
    foreach ($name in $ExpectedNames) {
        $path = Join-Path $StageDir $name
        if ((Get-Item -LiteralPath $path).Length -le 0) { throw "[FATAL] 生成文件为空:$name" }
    }

    if ($ExpectedNames -contains 'envoy-jwks.json') {
        $jwksPath = Join-Path $StageDir 'envoy-jwks.json'
        try { $jwks = Get-Content -LiteralPath $jwksPath -Raw | ConvertFrom-Json -ErrorAction Stop }
        catch { throw "[FATAL] Envoy JWKS 不是合法 JSON:$($_.Exception.Message)" }
        $keys = @($jwks.keys)
        $expectedKs = @(ConvertTo-Base64Url $PlayerSecretToInject)
        if ($null -ne $PlayerAdditionalToInject) { $expectedKs += ConvertTo-Base64Url $PlayerAdditionalToInject }
        if ($keys.Count -ne $expectedKs.Count) { throw '[FATAL] Envoy JWKS key 数量与本次玩家面密钥集不一致。' }
        for ($i = 0; $i -lt $expectedKs.Count; $i++) {
            if ($keys[$i].kty -cne 'oct' -or $keys[$i].alg -cne 'HS256' -or $keys[$i].k -cne $expectedKs[$i]) {
                throw '[FATAL] Envoy JWKS 与本次玩家面密钥不一致。'
            }
        }
    }

    # 不只搜索 dev 文本：逐节点核对最终 jwt/ds_auth 值，防格式漂移、替换 0 次或跨段误写仍通过。
    $expectedPlayerValue = if ($null -ne $PlayerSecretToInject) { $PlayerSecretToInject } else { $DevPublicSecret }
    $expectedDsValue = if ($null -ne $DsSecretToInject) { $DsSecretToInject } else { $DevPublicSecret }
    foreach ($svc in $Services) {
        $yaml = Get-Content -LiteralPath (Join-Path $StageDir "$($svc.Name).yaml") -Raw
        if ($PlayerSecretServiceNames -contains $svc.Name) {
            Assert-YamlSectionSecret $svc.Name $yaml 'jwt' $expectedPlayerValue
            if ($null -ne $PlayerAdditionalToInject) { Assert-YamlSectionAdditionalSecrets $svc.Name $yaml 'jwt' $PlayerAdditionalToInject }
            else { Assert-NoYamlSectionAdditionalSecrets $svc.Name $yaml 'jwt' }
        }
        if ($DsSecretServiceNames -contains $svc.Name) {
            Assert-YamlSectionSecret $svc.Name $yaml 'ds_auth' $expectedDsValue
            if ($null -ne $DsAdditionalToInject) { Assert-YamlSectionAdditionalSecrets $svc.Name $yaml 'ds_auth' $DsAdditionalToInject }
            else { Assert-NoYamlSectionAdditionalSecrets $svc.Name $yaml 'ds_auth' }
            if ($null -ne $DsAuthModeToInject) { Assert-YamlDsAuthMode $svc.Name $yaml $DsAuthModeToInject }
            if ($DsAuthorityModeToInject -eq 'redis') { Assert-YamlRedisAuthorityBundle $svc.Name $yaml }
        }
        if ($svc.Name -eq 'login' -and $null -ne $DsAuthorityModeToInject) {
            Assert-LoginHubAssignmentBinding $yaml ($DsAuthorityModeToInject -eq 'redis')
        }
        if ($DsTicketServiceNames -contains $svc.Name) {
            if ($AllocatorMode -eq 'agones') { Assert-YamlDsTicketSection $svc.Name $yaml }
            else { Assert-NoYamlDsTicketSection $svc.Name $yaml }
        }
        if ($svc.Name -eq 'battle-result') {
            if ($DsAuthorityModeToInject -eq 'redis') {
                Assert-BattleResultConsumeTopics $yaml @('pandora.ds.lifecycle')
            } else {
                Assert-BattleResultConsumeTopics $yaml @('pandora.battle.result', 'pandora.ds.lifecycle')
            }
        }
    }
}

function Publish-GeneratedSet {
    param(
        [string]$StageDir,
        [string]$TargetDir,
        [string[]]$ExpectedNames,
        [string[]]$OwnedNames,
        [string]$BackupDir
    )

    $targetDirCreated = $false
    if (Test-Path -LiteralPath $TargetDir) {
        if (-not (Test-Path -LiteralPath $TargetDir -PathType Container)) {
            throw "[FATAL] 输出路径已存在但不是目录:$TargetDir"
        }
    } else {
        [System.IO.Directory]::CreateDirectory($TargetDir) | Out-Null
        $targetDirCreated = $true
    }

    foreach ($name in $OwnedNames) {
        $target = Join-Path $TargetDir $name
        if (Test-Path -LiteralPath $target) {
            $item = Get-Item -LiteralPath $target -Force
            if ($item.PSIsContainer -or (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0)) {
                throw "[FATAL] 生成器自有目标名不是普通文件:$target"
            }
        }
    }

    $originalNames = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    try {
        [System.IO.Directory]::CreateDirectory($BackupDir) | Out-Null
        foreach ($name in $OwnedNames) {
            $target = Join-Path $TargetDir $name
            if (Test-Path -LiteralPath $target -PathType Leaf) {
                Copy-Item -LiteralPath $target -Destination (Join-Path $BackupDir $name)
                $null = $originalNames.Add($name)
            }
        }
    } catch {
        $backupError = $_.Exception.Message
        if (Test-Path -LiteralPath $BackupDir -PathType Container) { Remove-Item -LiteralPath $BackupDir -Recurse -Force }
        if ($targetDirCreated -and @(Get-ChildItem -LiteralPath $TargetDir -Force).Count -eq 0) {
            [System.IO.Directory]::Delete($TargetDir)
        }
        throw "[FATAL] 发布前备份旧配置失败,最终目录尚未修改:$backupError"
    }

    $publishId = [guid]::NewGuid().ToString('N')
    $preparedTemps = [System.Collections.Generic.List[string]]::new()
    $rollbackFailed = $false
    $publishCommitted = $false
    try {
        # 先把本次完整集合复制到目标目录内的唯一临时文件；到这里失败时最终集合完全未动。
        foreach ($name in $ExpectedNames) {
            $temp = Join-Path $TargetDir ".$name.pandora-gen-$publishId.tmp"
            $preparedTemps.Add($temp)
            Copy-Item -LiteralPath (Join-Path $StageDir $name) -Destination $temp
        }

        foreach ($name in $ExpectedNames) {
            $temp = Join-Path $TargetDir ".$name.pandora-gen-$publishId.tmp"
            $target = Join-Path $TargetDir $name
            if (Test-Path -LiteralPath $target -PathType Leaf) {
                [System.IO.File]::Move($temp, $target, $true)
            } else {
                [System.IO.File]::Move($temp, $target)
            }
        }
        foreach ($name in @($OwnedNames | Where-Object { $ExpectedNames -notcontains $_ })) {
            $stale = Join-Path $TargetDir $name
            if (Test-Path -LiteralPath $stale -PathType Leaf) { Remove-Item -LiteralPath $stale -Force }
        }
        $publishCommitted = $true
    }
    catch {
        $publishError = $_.Exception.Message
        $rollbackErrors = [System.Collections.Generic.List[string]]::new()
        foreach ($name in $OwnedNames) {
            try {
                $target = Join-Path $TargetDir $name
                if ($originalNames.Contains($name)) {
                    $restoreTemp = Join-Path $TargetDir ".$name.pandora-rollback-$publishId.tmp"
                    $preparedTemps.Add($restoreTemp)
                    Copy-Item -LiteralPath (Join-Path $BackupDir $name) -Destination $restoreTemp
                    if (Test-Path -LiteralPath $target -PathType Leaf) {
                        [System.IO.File]::Move($restoreTemp, $target, $true)
                    } elseif (Test-Path -LiteralPath $target) {
                        throw '目标被外部改成了非文件。'
                    } else {
                        [System.IO.File]::Move($restoreTemp, $target)
                    }
                } elseif (Test-Path -LiteralPath $target -PathType Leaf) {
                    Remove-Item -LiteralPath $target -Force
                } elseif (Test-Path -LiteralPath $target) {
                    throw '目标被外部改成了非文件。'
                }
            } catch {
                $rollbackErrors.Add("$name=$($_.Exception.Message)")
            }
        }
        if ($rollbackErrors.Count -ne 0) {
            $rollbackFailed = $true
            throw "[FATAL] 发布失败:$publishError;回滚也失败:$($rollbackErrors -join '; ')。已保留含旧配置的备份:$BackupDir,需人工处理。"
        }
        if ($targetDirCreated -and @(Get-ChildItem -LiteralPath $TargetDir -Force).Count -eq 0) {
            [System.IO.Directory]::Delete($TargetDir)
        }
        throw "[FATAL] 发布失败:$publishError;最终目录已回滚到旧完整集合。"
    }
    finally {
        $cleanupErrors = [System.Collections.Generic.List[string]]::new()
        foreach ($temp in $preparedTemps) {
            if (Test-Path -LiteralPath $temp -PathType Leaf) {
                try { Remove-Item -LiteralPath $temp -Force }
                catch { $cleanupErrors.Add("临时文件 $temp=$($_.Exception.Message)") }
            }
        }
        if (-not $rollbackFailed -and (Test-Path -LiteralPath $BackupDir -PathType Container)) {
            try { Remove-Item -LiteralPath $BackupDir -Recurse -Force }
            catch { $cleanupErrors.Add("备份目录 $BackupDir=$($_.Exception.Message)") }
        }
        if ($cleanupErrors.Count -ne 0) {
            if ($publishCommitted) {
                throw "[FATAL] 新配置集合已成功发布到 $TargetDir,但含配置材料的临时项清理失败:$($cleanupErrors -join '; ')。最终目录可用,需人工清理上述路径。"
            }
            Write-Host "[WARN] 发布失败路径还有临时项未清理:$($cleanupErrors -join '; ')" -ForegroundColor Yellow
        }
    }
}

$yamlNames = @($Services | ForEach-Object { "$($_.Name).yaml" })
$ownedNames = @($yamlNames + 'envoy-jwks.json')
$expectedNames = @($yamlNames)
if ($null -ne $PlayerSecretToInject) { $expectedNames += 'envoy-jwks.json' }

# 缺任一源配置就拒绝生成；不能再用 WARN + continue 把半套目录交给 compose/k8s。
$missingSources = @(
    foreach ($s in $Services) {
        $src = Join-Path $ProjectRoot $s.Conf
        if (-not (Test-Path -LiteralPath $src -PathType Leaf)) { $s.Conf }
    }
)
if ($missingSources.Count -ne 0) {
    throw "[FATAL] 缺少必需 dev 配置,未发布任何产物:$($missingSources -join ', ')"
}

$outParent = Split-Path -Parent $OutDir
$outLeaf = Split-Path -Leaf $OutDir
if ([string]::IsNullOrWhiteSpace($outParent) -or [string]::IsNullOrWhiteSpace($outLeaf)) {
    throw "[FATAL] -OutDir 必须是普通目录路径,不能是文件系统根:$OutDir"
}
[System.IO.Directory]::CreateDirectory($outParent) | Out-Null
$pathHashBytes = [System.Security.Cryptography.SHA256]::HashData([System.Text.Encoding]::UTF8.GetBytes($OutDir.ToUpperInvariant()))
$pathHash = ([System.BitConverter]::ToString($pathHashBytes) -replace '-', '').Substring(0, 16)
$mutex = [System.Threading.Mutex]::new($false, "PandoraGenClusterConfig_$pathHash")
$lockTaken = $false
$fileLock = $null
$stageDir = Join-Path $outParent ".$outLeaf.pandora-stage-$([guid]::NewGuid().ToString('N'))"
$backupDir = Join-Path $outParent ".$outLeaf.pandora-backup-$([guid]::NewGuid().ToString('N'))"
$publicationSucceeded = $false
try {
    try { $lockTaken = $mutex.WaitOne(0) }
    catch [System.Threading.AbandonedMutexException] { $lockTaken = $true }
    if (-not $lockTaken) { throw "[FATAL] 已有另一个生成器正在发布到同一 OutDir:$OutDir" }

    # named mutex 只覆盖同名路径/当前会话；目标目录内的独占文件锁还能让 junction、8.3/大小写别名
    # 与不同登录 session 最终竞争同一个物理文件。锁文件永久保留为空文件，避免 Unix unlink 后新 inode 绕锁。
    if ((Test-Path -LiteralPath $OutDir) -and -not (Test-Path -LiteralPath $OutDir -PathType Container)) {
        throw "[FATAL] 输出路径已存在但不是目录:$OutDir"
    }
    [System.IO.Directory]::CreateDirectory($OutDir) | Out-Null
    $fileLockPath = Join-Path $OutDir '.pandora-gen.lock'
    try {
        $fileLock = [System.IO.File]::Open(
            $fileLockPath,
            [System.IO.FileMode]::OpenOrCreate,
            [System.IO.FileAccess]::ReadWrite,
            [System.IO.FileShare]::None)
    } catch {
        throw "[FATAL] 无法取得 OutDir 独占发布锁 $fileLockPath(可能有并发生成器):$($_.Exception.Message)"
    }

    [System.IO.Directory]::CreateDirectory($stageDir) | Out-Null
    foreach ($s in $Services) {
        $src = Join-Path $ProjectRoot $s.Conf
        $raw = Get-Content -LiteralPath $src -Raw
        $out = Convert-DevToCluster $raw
        if ($s.Name -eq 'auction') { $out = Set-AuctionClusterSafety $out }
        $out = Convert-Secret $s.Name $out
        if ($s.Name -eq 'battle-result' -and $DsAuthorityModeToInject -eq 'redis') {
            $out = Set-BattleResultRedisAuthorityIngress $out
        }
        if ($s.Name -in @('ds-allocator', 'hub-allocator')) { $out = Rewrite-Allocator $s.Name $out }
        $dst = Join-Path $stageDir "$($s.Name).yaml"
        [System.IO.File]::WriteAllText($dst, $out, (New-Object System.Text.UTF8Encoding($false)))
    }
    Sync-EnvoyJwks -TargetDir $stageDir
    Assert-GeneratedSet -StageDir $stageDir -ExpectedNames $expectedNames
    Publish-GeneratedSet -StageDir $stageDir -TargetDir $OutDir -ExpectedNames $expectedNames -OwnedNames $ownedNames -BackupDir $backupDir
    $publicationSucceeded = $true
}
finally {
    $stageCleanupError = $null
    if (Test-Path -LiteralPath $stageDir -PathType Container) {
        try { Remove-Item -LiteralPath $stageDir -Recurse -Force }
        catch { $stageCleanupError = $_.Exception.Message }
    }
    if ($null -ne $fileLock) { $fileLock.Dispose() }
    if ($lockTaken) { $mutex.ReleaseMutex() }
    $mutex.Dispose()
    if ($null -ne $stageCleanupError) {
        if ($publicationSucceeded) {
            throw "[FATAL] 新配置集合已成功发布到 $OutDir,但 staging 清理失败:$stageDir;$stageCleanupError。最终目录可用,需人工清理 staging。"
        }
        throw "[FATAL] 生成失败且 staging 清理也失败:$stageDir;$stageCleanupError"
    }
}

Write-Host "[ OK ] 生成并事务发布 $($yamlNames.Count) 个集群版配置(allocator=$AllocatorMode, host_allocators=$HostAllocators, player_secret=$(if ($null -ne $PlayerSecretToInject) { '真密钥' } else { 'dev' }), ds_secret=$(if ($null -ne $DsSecretToInject) { '真密钥' } else { 'dev' })) -> $OutDir" -ForegroundColor Green
