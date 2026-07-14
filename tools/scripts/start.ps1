<#
.SYNOPSIS
  Pandora 后端一键启动器(策划/开发都能用)。

.DESCRIPTION
  一条命令把后端跑起来,覆盖 5 套环境(DS 分配模式随环境变):
    local    本地 windows 调试 —— 基础设施在 docker,20 个 go 服务以宿主进程跑(可断点);DS=local(Windows PandoraServer.exe)
    docker   本地 docker 启动   —— 基础设施 + 20 个 go 服务全跑在本机 docker;DS=mock(容器内无真 DS)
    intranet 内网测试服     —— 同 docker 全容器,但绑定内网 IP 供多人联调;DS=mock
    online   线上 k8s 集群   —— kustomize 部署到远端 k8s + Agones 真 Linux DS;DS=agones
                             用 -Env test|prod 区分「测试服集群」与「生产 kbs 集群」(不同 kube-context)

  还有两个本地联调辅助模式:
    battle   含战斗混合版 —— 18 个业务服务跑 docker,ds_allocator + hub_allocator 跑宿主(需 exec Windows DS);
                             进真实 Hub/Battle DS。play.ps1 -Battle[ -Intranet] 走这个;策划机不用起 go 服务。
    k8s      本地 minikube 联调 Agones —— 本机起 minikube + Agones,验证真 Linux DS 链路;DS=agones(默认 advertise 本机局域网 IP + 自动宿主桥接/UDP 中继)

  启动前会检查必要工具(go / docker / kubectl / minikube)。默认只提示缺失项,不改本机环境;
  只有显式传 -Install 才会尝试用 winget 安装。-Check 只检查不启动。

.EXAMPLE
  pwsh tools/scripts/start.ps1                      # 默认 local 模式(本地 windows 调试,全部服务)
  pwsh tools/scripts/start.ps1 -Mode docker
  pwsh tools/scripts/start.ps1 -Mode intranet                       # 内网测试服(全容器,绑内网 IP)
  pwsh tools/scripts/start.ps1 -Mode k8s                            # 本地 minikube + Agones 真 DS 联调
  pwsh tools/scripts/start.ps1 -Mode online -Env test -TestKubeContext pandora-test -Registry registry.mycorp.com -Tag v1.2.3-b5a5a95 -BattleDsImage registry.mycorp.com/pandora/battle-ds@sha256:<digest> -HubDsImage registry.mycorp.com/pandora/hub-ds@sha256:<digest> -DsGatewayAddr pandora-envoy.pandora.svc:8444  # 测试服
  pwsh tools/scripts/start.ps1 -Mode online -Env prod -ProdKubeContext pandora-prod -Registry registry.mycorp.com -Tag v1.2.3-b5a5a95 -BattleDsImage registry.mycorp.com/pandora/battle-ds@sha256:<digest> -HubDsImage registry.mycorp.com/pandora/hub-ds@sha256:<digest> -DsGatewayAddr pandora-envoy.pandora.svc:8444  # 生产(双重确认)
  pwsh tools/scripts/start.ps1 -Mode docker -Down  # 停
  pwsh tools/scripts/start.ps1 -Status             # 看状态
  pwsh tools/scripts/start.ps1 -Check              # 只检查工具
  pwsh tools/scripts/start.ps1 -Install            # 缺工具时才尝试 winget 安装

.EXAMPLE
  # 电脑重启后『快速恢复』上次的环境(不重建镜像,把停掉的集群/容器拉回来):
  pwsh tools/scripts/start.ps1 -Mode k8s    -Resume   # minikube start + 等 Pod + 自动重建宿主桥接/UDP 中继
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

    # 局域网模式默认只把【客户端面 8443】开到局域网;未鉴权的 DS 面 8444 恒绑本机。
    # 仅当真有【异机 UE DS】需要回连本机 Envoy :8444 时才加此开关显式开放(务必配合网络隔离)。
    [switch]$ExposeDsFace,

    [switch]$Down,        # 停止该模式
    [switch]$Resume,      # 电脑重启后快速恢复:不重建镜像,把上次停掉的集群/容器拉回来
    [switch]$Reset,       # 一键重置:彻底清掉旧状态再全新启动(线上 online 模式禁用)
    [switch]$Status,      # 查看状态
    [switch]$Check,       # 只检查工具链,不启动
    [switch]$BuildOnly,   # 只构建业务镜像后退出(供 export_images.ps1 离线打包用)
    [switch]$Rebuild,     # 强制重新构建镜像,忽略 deploy/offline-images 离线包(开发机改代码后用)

    # 镜像构建方式(方案 A / 方案 B,可选):
    #   incontainer 方案A:在 Docker 容器里 go build(环境隔离,无需本机 Go,冷缓存较慢)——默认,保持既有行为
    #   host        方案B:本机交叉编译 linux 二进制再塞进 scratch 镜像(享受宿主增量缓存,单服务重建秒级,需装 Go)
    [ValidateSet('incontainer', 'host')]
    [string]$BuildMode = 'incontainer',
    [string[]]$Only = @(), # 配合 -BuildOnly:只构建指定服务(连字符名,如 battle-result);留空=全部业务镜像
    [switch]$Install,     # 工具缺失时尝试 winget 安装(默认不安装;不含 Docker Desktop)
    [switch]$InstallDocker, # 显式同意用 winget 装 Docker Desktop(装前确认虚拟化;装后仍需重启+手动启动)
    [switch]$NoInstall,   # 兼容旧参数;等同于不传 -Install

    # online 模式参数
    [string]$Registry,    # 镜像仓库地址,如 registry.mycorp.com
    [string]$Tag,         # online 不可变 tag,必须含 git SHA 段,如 v1.2.3-b5a5a95
    [string]$BattleDsImage, # online:Stable 战斗 DS 镜像,最终会解析并固定为 repo@sha256:digest
    [string]$HubDsImage,    # online:Stable 大厅 DS 镜像,最终会解析并固定为 repo@sha256:digest
    [string]$CanaryBattleDsImage, # online:Canary 战斗 DS 独立镜像；启用 Battle 灰度时必填且 digest 必须不同
    [string]$CanaryHubDsImage,    # online:Canary 大厅 DS 独立镜像；启用 Hub 灰度时必填且 digest 必须不同
    [ValidateRange(0, 100)][int]$BattleCanaryPercent = 0,
    [ValidateRange(0, 100)][int]$HubCanaryPercent = 0,
    [ValidateRange(0, 1000)][int]$BattleCanaryReplicas = 1,
    [ValidateRange(0, 1000)][int]$HubCanaryReplicas = 1,
    # online:Battle FleetAutoscaler 覆写。maxReplicas = 同时最大局数护栏,设为节点池上限
    # 对应的容量(真弹性由集群 Cluster Autoscaler 加节点提供);bufferSize 支持百分比(如 10%)。
    # 0/空 = 用 25-fleetautoscaler-battle.yaml 的本地 dev 默认值。
    [ValidateRange(0, 1000000)][int]$BattleMaxReplicas = 0,
    [ValidatePattern('^([0-9]+%?)?$')][string]$BattleBufferSize = '',
    [string]$CanarySeed = $env:PANDORA_DS_CANARY_SEED,
    [string]$DsGatewayAddr, # online:DS 回调入口(如 pandora-envoy.pandora.svc:8444)
    # online 安全映射:不同环境必须显式绑定各自 kube-context；也可用同名环境变量持久配置。
    [string]$TestKubeContext = $env:PANDORA_K8S_TEST_CONTEXT,
    [string]$ProdKubeContext = $env:PANDORA_K8S_PROD_CONTEXT,
    [ValidateSet('0', '1')]
    # online:DS 回调是否 TLS。权威口径(gateway-decision.md §3.5/§16):DS 面 :8444 是集群内明文,
    # 本地/线上同构默认 0;仅当线上把 DS 面挂到集群外 TLS 边缘时才显式传 1
    # (2026-07-10 已统一旧版互相冲突的默认值)。
    [string]$DsGatewayTls = '0',
    [ValidateSet('', 'enforce')]
    # online:Redis authority 只允许 enforce；permissive/off 不再是生产逃生口。
    [string]$DsAuthMode = '',
    [ValidateSet('', 'redis')]
    [string]$DsAuthorityMode = '',
    [string]$DsFenceEtcdEndpoints = $env:PANDORA_DS_AUTH_FENCE_ETCD_ENDPOINTS,
    [string]$DsFenceKeysetRevision = $env:PANDORA_DS_AUTH_KEYSET_REVISION,
    # 生产 DS auth etcd 身份材料只接受外部文件路径；脚本绝不创建、修改或打印其内容。
    [string]$DsFenceEtcdIdentityRevision = $env:PANDORA_DS_AUTH_ETCD_IDENTITY_REVISION,
    [string]$DsFenceEtcdServerName = $env:PANDORA_DS_AUTH_ETCD_SERVER_NAME,
    [string]$DsFenceEtcdAuditorCAFile = $env:PANDORA_DS_AUTH_ETCD_AUDITOR_CA_FILE,
    [string]$DsFenceEtcdAuditorCertFile = $env:PANDORA_DS_AUTH_ETCD_AUDITOR_CERT_FILE,
    [string]$DsFenceEtcdAuditorKeyFile = $env:PANDORA_DS_AUTH_ETCD_AUDITOR_KEY_FILE,
    [string]$DsFenceEtcdAuditorIdentity = $env:PANDORA_DS_AUTH_ETCD_AUDITOR_IDENTITY,
    [string]$DsFenceEtcdForbiddenReadPrefix = $env:PANDORA_DS_AUTH_ETCD_FORBIDDEN_READ_PREFIX,
    [string]$DsFenceEtcdAuditorUsernameFile = $env:PANDORA_DS_AUTH_ETCD_AUDITOR_USERNAME_FILE,
    [string]$DsFenceEtcdAuditorPasswordFile = $env:PANDORA_DS_AUTH_ETCD_AUDITOR_PASSWORD_FILE,
    # DSTicket v2(方案 B,RS256)active kid:online 可选显式期望值；实际值永远从
    # immutable Secret 私钥 + JWKS 顶层 active_kid 对账得到，不读 keys[0]。
    [string]$DsTicketActiveKid = $env:PANDORA_DSTICKET_ACTIVE_KID,
    # 玩家 DSTicket 公钥 keyset revision，和 DS callback auth 的 keyset revision 是两套独立值。
    [string]$DsTicketKeysetRevision = $env:PANDORA_DSTICKET_KEYSET_REVISION,
    [switch]$BuildPush    # online:本地构建并推送 20 个镜像到 -Registry(远端发布动作,需人工授权)
)

$ErrorActionPreference = 'Stop'
$ScriptDir   = $PSScriptRoot
$ProjectRoot = (Resolve-Path "$ScriptDir/../..").Path
. (Join-Path $ScriptDir 'lib/online_manifest_contract.ps1')
. (Join-Path $ScriptDir 'lib/ds_auth_activation_contract.ps1')
. (Join-Path $ScriptDir 'lib/dsticket_keyset_contract.ps1')
. (Join-Path $ScriptDir 'lib/dsticket_rotation_contract.ps1')
# rotation contract 自身在加载期开启 StrictMode；start.ps1 是历史运维入口，
# 未全面满足 StrictMode Latest，因此只共享其纯函数契约后恢复本脚本既有语义。
Set-StrictMode -Off
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

# pandora-config Secret 只收录 20 份服务 YAML。envoy-jwks.json 是给外部边缘网关的产物，
# OutDir 中其它运维文件也不应被 --from-file=<目录> 意外灌进 Pod Secret。
function Apply-PandoraConfigSecret {
    param(
        [string]$KubeContext,
        [string]$ConfigDir = $ClusterEtcDir,
        [string]$Action
    )

    $kubectlContextArgs = @('--context', $KubeContext)
    $expectedNames = @()
    $fileArgs = @(
        foreach ($svc in (Get-ServiceList)) {
            $name = "$($svc.Name).yaml"
            $expectedNames += $name
            $path = Join-Path $ConfigDir $name
            if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
                throw "pandora-config Secret 缺少必需配置:$path"
            }
            "--from-file=$name=$path"
        }
    )
    if ($fileArgs.Count -ne 20) { throw "pandora-config Secret 期望 20 份服务配置,实际=$($fileArgs.Count)" }

    $manifest = @(kubectl @KubectlContextArgs create secret generic pandora-config @fileArgs `
        -n $K8sNamespace --dry-run=client -o yaml)
    Assert-LastExit "$Action(create secret manifest)"
    $manifest | kubectl @KubectlContextArgs apply -f -
    Assert-LastExit $Action

    # client-side apply 遇到缺 last-applied annotation 的人工对象时可能保留旧 data key；回读服务端
    # 严格核对，任何多余/缺失项都阻断 rollout，避免把陈旧配置伪装成“精确 20 文件”。
    $secret = Get-KubectlJsonObject -KubeContext $KubeContext `
        -Arguments @('get', 'secret/pandora-config', '-n', $K8sNamespace, '-o', 'json') `
        -Action '回读 pandora-config Secret'
    $actualNames = if ($null -eq $secret.data) { @() } else { @($secret.data.PSObject.Properties.Name | Sort-Object) }
    $keyDiff = @(Compare-Object -ReferenceObject @($expectedNames | Sort-Object) -DifferenceObject $actualNames -CaseSensitive)
    if ($keyDiff.Count -ne 0) {
        $missing = @($keyDiff | Where-Object SideIndicator -eq '<=' | ForEach-Object InputObject)
        $extra = @($keyDiff | Where-Object SideIndicator -eq '=>' | ForEach-Object InputObject)
        throw "pandora-config Secret 服务端 key 集不精确:缺少=[$($missing -join ', ')],多余=[$($extra -join ', ')]。" +
              '请先清理漂移 key 后重跑；当前不会继续 rollout。'
    }
}

function Get-KubectlJsonObject {
    param(
        [string]$KubeContext,
        [string[]]$Arguments,
        [string]$Action
    )

    # stderr 不并入 JSON，避免 kubectl warning 污染解析；退出码仍严格检查。
    $raw = @(& kubectl --context $KubeContext @Arguments 2>$null)
    if ($LASTEXITCODE -ne 0) { throw "$Action 失败(exit=$LASTEXITCODE)" }
    $text = (($raw | ForEach-Object { $_.ToString() }) -join "`n").Trim()
    if ([string]::IsNullOrWhiteSpace($text)) { throw "$Action 返回空 JSON" }
    try { return ($text | ConvertFrom-Json -ErrorAction Stop) }
    catch { throw "$Action 返回非法 JSON:$($_.Exception.Message)" }
}

# ===== DSTicket v2(方案 B)本地 dev 钥料自举 =====
# 生成/复用 dev RSA 钥对(私钥落 run/,不入版本库,见 tools/dsticketkeys 头注),并把:
#   - 私钥 → immutable Secret pandora-dsticket-signer-r1(ns pandora，四个签发方经 services.yaml
#     挂到稳定路径 /run/secrets/pandora-dsticket)；
#   - 公钥 JWKS → 两份同 hash immutable ConfigMap pandora-dsticket-jwks-r1：default 给 Fleet，
#     pandora 给 Login 的兼容/诊断 verifier。
# 返回由 private.pem 推导、并与 JWKS 中同 kid 公钥参数对账后的 active kid；绝不依赖 keys[0]。
# 线上不走本函数:真钥对由受控密钥管线生成,按 deploy/k8s/agones/15-dsticket-jwks.yaml 的纪律建
# 不可变 Secret/ConfigMap；dev 同样只 create missing + 回读对账，绝不原地覆盖。
function Ensure-DsTicketDevKeyMaterial {
    param([Parameter(Mandatory = $true)][string]$KubeContext)
    $kubectlContextArgs = @('--context', $KubeContext)
    $keyDir = Join-Path $ProjectRoot 'run/cluster/dsticket'
    $privPath = Join-Path $keyDir 'private.pem'
    $jwksPath = Join-Path $keyDir 'jwks.json'
    if (-not ((Test-Path -LiteralPath $privPath -PathType Leaf) -and (Test-Path -LiteralPath $jwksPath -PathType Leaf))) {
        # 半套残留(只有其一)时 dsticketkeys 会拒绝覆盖 —— 这是刻意的:密钥文件不自动删,
        # 请人工确认后移走残件再重跑(防脚本静默换钥导致在跑集群票据全废)。
        Write-Info "生成 DSTicket v2 dev 钥对(run/cluster/dsticket,revision=1)..."
        Push-Location $ProjectRoot
        try {
            # Native stdout must not escape this function: callers assign the sole return value as
            # DsTicketActiveKid.  A fresh bootstrap otherwise returns [generator output, kid] and
            # PowerShell cannot bind that array to gen_cluster_config.ps1's string parameter.
            & go run ./tools/dsticketkeys -out $keyDir -revision 1 | Out-Host
            Assert-LastExit 'dsticketkeys 生成 dev 钥对'
        } finally {
            Pop-Location
        }
    }
    $privatePem = Get-Content -LiteralPath $privPath -Raw
    $jwksText = Get-Content -LiteralPath $jwksPath -Raw
    $keyContract = Get-PandoraDSTicketKeyMaterialContract -PrivateKeyPem $privatePem -JwksText $jwksText -ExpectedRevision 1
    $kid = $keyContract.ActiveKid
    $keysetAnnotations = [ordered]@{
        'pandora.dev/dsticket-active-kid' = $keyContract.ActiveKid
        'pandora.dev/dsticket-keyset-revision' = '1'
        'pandora.dev/dsticket-jwks-sha256' = $keyContract.JwksSha256
    }
    $signerAnnotations = [ordered]@{
        'pandora.dev/dsticket-signer-kid' = $keyContract.SignerKid
        'pandora.dev/dsticket-signer-revision' = '1'
        'pandora.dev/dsticket-private-pem-sha256' = $keyContract.PrivatePemSha256
    }
    $labels = [ordered]@{
        'app.kubernetes.io/part-of' = 'pandora'
        'app.kubernetes.io/component' = 'dsticket-keyset'
    }
    function New-LocalDSTicketObjectIfMissing([object]$Object, [string]$KindName, [string]$Namespace) {
        $existing = @(& kubectl @kubectlContextArgs get $KindName -n $Namespace --ignore-not-found -o name 2>&1)
        if ($LASTEXITCODE -ne 0) { throw "读取 $Namespace/$KindName 失败:$($existing -join [Environment]::NewLine)" }
        if ([string]::IsNullOrWhiteSpace((($existing | ForEach-Object { $_.ToString() }) -join '').Trim())) {
            $json = $Object | ConvertTo-Json -Depth 20 -Compress
            $created = @($json | & kubectl @kubectlContextArgs create -f - 2>&1)
            if ($LASTEXITCODE -ne 0) { throw "create-only 创建 $Namespace/$KindName 失败:$($created -join [Environment]::NewLine)" }
        }
    }

    # 开发集群也遵守 create-only + immutable：已有对象绝不 apply/patch，防止重跑
    # start 时静默换钥。ConfigMap 是 namespaced 对象，default 供 Fleet/DS，pandora 供 Login verifier；
    # 两份公钥对象的内容/hash/active kid 必须完全相同。
    $secretObject = [ordered]@{
        apiVersion = 'v1'; kind = 'Secret'; immutable = $true; type = 'Opaque'
        metadata = [ordered]@{ name = 'pandora-dsticket-signer-r1'; namespace = $K8sNamespace; labels = $labels; annotations = $signerAnnotations }
        data = [ordered]@{ 'private.pem' = [Convert]::ToBase64String([System.Text.Encoding]::UTF8.GetBytes($privatePem)) }
    }
    New-LocalDSTicketObjectIfMissing $secretObject 'secret/pandora-dsticket-signer-r1' $K8sNamespace
    foreach ($namespace in @('default', $K8sNamespace)) {
        $cmObject = [ordered]@{
            apiVersion = 'v1'; kind = 'ConfigMap'; immutable = $true
            metadata = [ordered]@{ name = 'pandora-dsticket-jwks-r1'; namespace = $namespace; labels = $labels; annotations = $keysetAnnotations }
            data = [ordered]@{ 'jwks.json' = $jwksText }
        }
        New-LocalDSTicketObjectIfMissing $cmObject 'configmap/pandora-dsticket-jwks-r1' $namespace
    }
    try {
        $liveSecret = Get-KubectlJsonObject -KubeContext $KubeContext `
            -Arguments @('get', 'secret/pandora-dsticket-signer-r1', '-n', $K8sNamespace, '-o', 'json') -Action '回读 dev DSTicket revisioned signer 私钥'
        foreach ($namespace in @('default', $K8sNamespace)) {
            $liveCM = Get-KubectlJsonObject -KubeContext $KubeContext `
                -Arguments @('get', 'configmap/pandora-dsticket-jwks-r1', '-n', $namespace, '-o', 'json') -Action "回读 $namespace DSTicket JWKS"
            $null = Assert-PandoraDSTicketKubernetesObjects -SecretObject $liveSecret -ConfigMapObject $liveCM `
                -ExpectedRevision 1 -ExpectedActiveKid $kid -ExpectedConfigMapNamespace $namespace
        }
    } catch {
        throw "本地集群存在旧版/漂移的 DSTicket 对象，拒绝原地覆盖:$($_.Exception.Message) 请显式用 -Mode k8s -Reset 重建开发集群。"
    }
    Write-Ok "DSTicket v2 dev 钥料就绪(kid=$kid,signer=r1,keyset=r1；immutable JWKS=default+pandora)。"
    return $kid
}

# 本地 minikube 的 DS callback Model-B 基线。业务配置使用集群 DNS
# etcd.pandora.svc:2379；启动器从宿主做线性预检/CAS 时通过临时 port-forward。
$script:LocalDsFenceEndpoint = 'etcd.pandora.svc:2379'
$script:LocalDsFenceKeysetRevision = 'pandora-ds-auth-v2-local-r1'

function Test-MinikubeProfileExists {
    param([Parameter(Mandatory = $true)][string]$Profile)
    $lines = @(& minikube profile list -o json 2>&1)
    if ($LASTEXITCODE -ne 0) {
        throw "无法列出 minikube profile，不能安全判定是否 fresh cluster:$($lines -join [Environment]::NewLine)"
    }
    $text = (($lines | ForEach-Object { $_.ToString() }) -join "`n").Trim()
    if ([string]::IsNullOrWhiteSpace($text)) { return $false }
    try { $profiles = $text | ConvertFrom-Json -ErrorAction Stop }
    catch { throw "minikube profile list 返回非法 JSON:$($_.Exception.Message)" }
    foreach ($entry in @($profiles.valid) + @($profiles.invalid)) {
        if ([string]$entry.Name -ceq $Profile) { return $true }
    }
    return $false
}

function Invoke-WithLocalEtcdPortForward {
    param(
        [Parameter(Mandatory = $true)][string]$KubeContext,
        [Parameter(Mandatory = $true)][scriptblock]$Action
    )
    $listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
    $listener.Start()
    try { $port = ([System.Net.IPEndPoint]$listener.LocalEndpoint).Port }
    finally { $listener.Stop() }
    $stdout = Join-Path ([System.IO.Path]::GetTempPath()) ('pandora-etcd-pf-' + [guid]::NewGuid().ToString('N') + '.out')
    $stderr = $stdout + '.err'
    $proc = $null
    try {
        $proc = Start-Process -FilePath 'kubectl' -ArgumentList @(
            '--context', $KubeContext, '-n', $K8sNamespace, 'port-forward', 'service/etcd', "${port}:2379"
        ) -PassThru -WindowStyle Hidden -RedirectStandardOutput $stdout -RedirectStandardError $stderr
        $ready = $false
        $deadline = [DateTime]::UtcNow.AddSeconds(15)
        do {
            if ($proc.HasExited) { break }
            $client = [System.Net.Sockets.TcpClient]::new()
            try {
                $task = $client.ConnectAsync([System.Net.IPAddress]::Loopback, $port)
                if ($task.Wait(250) -and $client.Connected) { $ready = $true; break }
            } catch { } finally { $client.Dispose() }
            Start-Sleep -Milliseconds 200
        } while ([DateTime]::UtcNow -lt $deadline)
        if (-not $ready) {
            $detail = @(
                if (Test-Path $stdout) { Get-Content $stdout -Raw }
                if (Test-Path $stderr) { Get-Content $stderr -Raw }
            ) -join [Environment]::NewLine
            throw "etcd port-forward 未就绪(context=$KubeContext):$detail"
        }
        & $Action "127.0.0.1:$port"
    } finally {
        if ($null -ne $proc -and -not $proc.HasExited) {
            Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
            try { $proc.WaitForExit(5000) | Out-Null } catch { }
        }
        Remove-Item -LiteralPath $stdout, $stderr -Force -ErrorAction SilentlyContinue
    }
}

function Assert-LocalDsAuthBaseline {
    param(
        [Parameter(Mandatory = $true)][string]$KubeContext,
        [Parameter(Mandatory = $true)][bool]$AllowFreshBootstrap
    )
    Invoke-WithLocalEtcdPortForward -KubeContext $KubeContext -Action {
        param($endpoint)
        Push-Location $ProjectRoot
        try {
            $readLines = @(& go run ./pkg/dsauthfence/cmd/dsauth-required --endpoints $endpoint --min-epoch 1 --max-epoch 2 2>&1)
            $readExit = $LASTEXITCODE
            if ($readExit -eq 0) {
                $readLines | ForEach-Object { Write-Host $_ }
                return
            }
            $readText = ($readLines | ForEach-Object { $_.ToString() }) -join [Environment]::NewLine
            if ($readText -notmatch '(?i)required epoch missing') {
                throw "DS auth baseline 线性读失败（非 missing，禁止 bootstrap）:$readText"
            }
            if (-not $AllowFreshBootstrap) {
                throw "DS auth required_writer_epoch 缺失；当前不是 fresh minikube，只读预检拒绝把 missing 当 1。请审计状态或显式 -Reset。"
            }
            Write-Info '检测到 fresh minikube 且 required_writer_epoch 缺失，执行唯一一次 CAS bootstrap=1...'
            # bootstrap 只初始化 required_writer_epoch baseline，不接收或伪造 capability 审计参数。
            $bootLines = @(& go run ./pkg/dsauthfence/cmd/dsauth-activate --endpoints $endpoint `
                --bootstrap --apply 2>&1)
            if ($LASTEXITCODE -ne 0) {
                throw "fresh minikube required_writer_epoch CAS bootstrap 失败:$($bootLines -join [Environment]::NewLine)"
            }
            $verifyLines = @(& go run ./pkg/dsauthfence/cmd/dsauth-required --endpoints $endpoint --min-epoch 1 --max-epoch 1 2>&1)
            if ($LASTEXITCODE -ne 0) {
                throw "bootstrap 后无法线性证明 required_writer_epoch=1:$($verifyLines -join [Environment]::NewLine)"
            }
            $bootLines | ForEach-Object { Write-Host $_ }
            $verifyLines | ForEach-Object { Write-Host $_ }
        } finally { Pop-Location }
    }
}

function Assert-NoLegacyDsFleets {
    param(
        [Parameter(Mandatory = $true)][string]$KubeContext,
        [switch]$LocalDevelopment
    )
    $legacy = [System.Collections.Generic.List[string]]::new()
    foreach ($name in @('pandora-battle', 'pandora-hub')) {
        $lines = @(& kubectl --context $KubeContext get "fleet/$name" -n default --ignore-not-found -o name 2>&1)
        if ($LASTEXITCODE -ne 0) {
            $errText = (($lines | ForEach-Object { $_.ToString() }) -join [Environment]::NewLine)
            # 全新集群(如 -Reset 后)Agones 尚未安装,Fleet CRD 不存在。CRD 都没有就不可能残留
            # legacy Fleet,视为“无”继续,而非当成读取失败中止。
            if ($errText -match '(?i)the server doesn''t have a resource type\s+"?fleet"?') {
                continue
            }
            throw "读取 legacy Fleet/$name 失败:$errText"
        }
        if (-not [string]::IsNullOrWhiteSpace((($lines | ForEach-Object { $_.ToString() }) -join '').Trim())) {
            $legacy.Add($name)
        }
    }
    if ($legacy.Count -eq 0) { return }
    if ($LocalDevelopment) {
        throw "检测到旧单轨 Fleet:$($legacy -join ',')。为防新旧 allocator 同时分配，本地不自动删除在跑 DS；请用 -Mode k8s -Reset 显式重建。"
    }
    throw "检测到旧单轨 Fleet:$($legacy -join ',')，拒绝与 Stable/Canary 四 Fleet 静默并存。" +
          'Battle 只能在确认无 Allocated 后删除；Hub 本仓没有可机械证明玩家已排空的查询，发布脚本永不自动删除。' +
          '请先走现有 drain/迁移流程，保存审计证据并由运维显式移除 legacy Fleet，然后重跑；本次尚未 apply/push。'
}

function Assert-NoLegacyDSTicketSignerSecret {
    param(
        [Parameter(Mandatory = $true)][string]$KubeContext,
        [switch]$LocalDevelopment
    )
    $lines = @(& kubectl --context $KubeContext get secret/pandora-dsticket -n $K8sNamespace `
        --ignore-not-found -o name 2>&1)
    if ($LASTEXITCODE -ne 0) {
        throw "读取 legacy Secret/pandora-dsticket 失败:$($lines -join [Environment]::NewLine)"
    }
    if ([string]::IsNullOrWhiteSpace((($lines | ForEach-Object { $_.ToString() }) -join '').Trim())) { return }
    if ($LocalDevelopment) {
        throw '检测到 legacy Secret/pandora-dsticket；新契约只接受 pandora-dsticket-signer-rN。为防旧私钥残留或误挂载，请用 -Mode k8s -Reset 显式重建本地集群。'
    }
    throw '检测到 legacy Secret/pandora-dsticket；online 发布拒绝让非 revisioned 私钥与 pandora-dsticket-signer-rN 并存。请审计迁移并由运维显式移除旧对象后重跑；本次尚未 push/apply。'
}

function Assert-ExistingLocalEtcdPersistence {
    param([Parameter(Mandatory = $true)][string]$KubeContext)
    $lines = @(& kubectl --context $KubeContext get deployment/etcd -n $K8sNamespace --ignore-not-found -o json 2>&1)
    if ($LASTEXITCODE -ne 0) { throw "读取本地 Deployment/etcd 失败:$($lines -join [Environment]::NewLine)" }
    $text = (($lines | ForEach-Object { $_.ToString() }) -join "`n").Trim()
    if ([string]::IsNullOrWhiteSpace($text)) { return }
    try { $deploy = $text | ConvertFrom-Json -ErrorAction Stop }
    catch { throw "Deployment/etcd 返回非法 JSON:$($_.Exception.Message)" }
    $container = @($deploy.spec.template.spec.containers | Where-Object name -eq 'etcd')
    $mount = @($container.volumeMounts | Where-Object { $_.name -ceq 'data' -and $_.mountPath -ceq '/etcd-data' })
    $volume = @($deploy.spec.template.spec.volumes | Where-Object {
        $_.name -ceq 'data' -and [string]$_.persistentVolumeClaim.claimName -ceq 'etcd-data'
    })
    if ([string]$deploy.spec.strategy.type -cne 'Recreate' -or $container.Count -ne 1 -or
        $mount.Count -ne 1 -or $volume.Count -ne 1) {
        throw '现有本地 etcd 仍是旧版非持久布署；直接 apply PVC 会重建 Pod 并丢失 required_writer_epoch/capability。' +
              '已在任何集群写入前中止。本地可用 -Mode k8s -Reset 显式重建；若必须保留现场，先做 etcd snapshot + restore 到 PVC 并审计 required epoch，禁止自动 missing=>1。'
    }
}

function Wait-KubeApiServerReady {
    param(
        [Parameter(Mandatory = $true)][string]$KubeContext,
        [ValidateRange(1, 60)][int]$TimeoutSeconds = 45
    )
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $lines = @(& kubectl --context $KubeContext get --raw=/readyz 2>&1)
        $readExit = $LASTEXITCODE
        $text = (($lines | ForEach-Object { $_.ToString() }) -join "`n").Trim()
        if ($readExit -eq 0 -and $text -ceq 'ok') {
            Write-Ok "kube-apiserver readyz 已就绪(context=$KubeContext)。"
            return
        }
        Start-Sleep -Milliseconds 500
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "kube-apiserver 在 ${TimeoutSeconds}s 内未通过 /readyz；Resume 尚未等待旧业务 Ready、apply 或 rollout。"
}

function Get-RegistryImageDescriptor {
    param([Parameter(Mandatory = $true)][string]$Reference)
    $lines = @(& docker buildx imagetools inspect $Reference 2>&1)
    if ($LASTEXITCODE -ne 0) {
        throw "读取 registry manifest 失败:$Reference;$($lines -join [Environment]::NewLine)"
    }
    $text = ($lines | ForEach-Object { $_.ToString() }) -join [Environment]::NewLine
    return ConvertFrom-PandoraImagetoolsInspect -Reference $Reference -Output $text
}

function Assert-RemoteImageTagAbsent {
    param([Parameter(Mandatory = $true)][string]$Reference)
    $lines = @(& docker buildx imagetools inspect $Reference 2>&1)
    if ($LASTEXITCODE -eq 0) {
        throw "不可变发布 tag 已存在，禁止覆盖:$Reference"
    }
    $text = ($lines | ForEach-Object { $_.ToString() }) -join [Environment]::NewLine
    if (-not (Test-PandoraManifestNotFoundOutput -Output $text)) {
        throw "无法证明远端 tag 不存在（可能是鉴权/TLS/网络错误），禁止继续 push:$Reference;$($lines -join [Environment]::NewLine)"
    }
}

function Push-ImageAndResolveDigest {
    param(
        [Parameter(Mandatory = $true)][string]$Local,
        [Parameter(Mandatory = $true)][string]$Remote
    )
    docker tag $Local $Remote
    Assert-LastExit "docker tag $Local -> $Remote"
    $pushLines = @(& docker push $Remote 2>&1)
    $pushExit = $LASTEXITCODE
    foreach ($line in $pushLines) { Write-Host $line }
    if ($pushExit -ne 0) { throw "推送失败:$Remote" }
    $pushText = ($pushLines | ForEach-Object { $_.ToString() }) -join [Environment]::NewLine
    $pushDigest = Get-PandoraPushDigest -Output $pushText
    $registry = Get-RegistryImageDescriptor $Remote
    if ($registry.Digest -cne $pushDigest) {
        throw "push digest 与 registry 回读不一致:$Remote push=$pushDigest registry=$($registry.Digest)"
    }
    return $registry
}

function Assert-CleanOnlineReleaseSource {
    $lines = @(& git -C $ProjectRoot status --porcelain=v1 --untracked-files=normal 2>&1)
    if ($LASTEXITCODE -ne 0) { throw "online BuildPush 无法读取 git worktree 状态:$($lines -join [Environment]::NewLine)" }
    $text = ($lines | ForEach-Object { $_.ToString() }) -join [Environment]::NewLine
    Assert-PandoraCleanGitStatus -Output $text
}

function Assert-LocalImageRevision {
    param(
        [Parameter(Mandatory = $true)][string]$Reference,
        [Parameter(Mandatory = $true)][string]$Expected
    )
    $lines = @(& docker image inspect --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}' $Reference 2>&1)
    if ($LASTEXITCODE -ne 0) { throw "无法读取本地镜像 provenance:$Reference;$($lines -join [Environment]::NewLine)" }
    $actual = (($lines | ForEach-Object { $_.ToString() }) -join [Environment]::NewLine).Trim()
    Assert-PandoraImageRevision -Reference $Reference -Actual $actual -Expected $Expected
}

function Invoke-KubectlClientContract {
    param(
        [Parameter(Mandatory = $true)][string]$Manifest,
        [Parameter(Mandatory = $true)][string]$JsonPath,
        [Parameter(Mandatory = $true)][string]$Action
    )
    $tmp = Join-Path ([System.IO.Path]::GetTempPath()) ('pandora-contract-' + [guid]::NewGuid().ToString('N') + '.yaml')
    try {
        [System.IO.File]::WriteAllText($tmp, $Manifest, [System.Text.UTF8Encoding]::new($false))
        $lines = @(& kubectl create --dry-run=client --validate=false -f $tmp -o "jsonpath=$JsonPath" 2>&1)
        if ($LASTEXITCODE -ne 0) { throw "$Action 结构化解析失败:$($lines -join [Environment]::NewLine)" }
        return @($lines | ForEach-Object { $_.ToString() })
    } finally {
        Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
    }
}

function Get-PandoraWorkloadContractRows {
    param([Parameter(Mandatory = $true)][string]$Manifest)
    $jsonPath = '{.kind}{"\t"}{.metadata.name}{"\t"}{.spec.template.spec.containers[*].name}{"\t"}{.spec.template.spec.containers[*].image}{"\t"}{.spec.template.metadata.annotations.pandora\.dev/image-digest}{"\n"}'
    return @(Invoke-KubectlClientContract -Manifest $Manifest -JsonPath $jsonPath -Action 'online Deployment manifest')
}

function Get-PandoraDSTicketSignerContractRows {
    param([Parameter(Mandatory = $true)][string]$Manifest)
    $jsonPath = '{.kind}{"\t"}{.metadata.name}{"\t"}{.spec.template.spec.volumes[?(@.name=="dsticket")].secret.secretName}{"\t"}{.spec.template.spec.volumes[?(@.name=="dsticket-jwks")].configMap.name}{"\n"}'
    return @(Invoke-KubectlClientContract -Manifest $Manifest -JsonPath $jsonPath -Action 'online DSTicket signer manifest')
}

function Get-OnlineOptionalKubectlJsonObject {
    param(
        [Parameter(Mandatory = $true)][string]$KubeContext,
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [Parameter(Mandatory = $true)][string]$Action
    )
    $raw = @(& kubectl --context $KubeContext @Arguments 2>$null)
    if ($LASTEXITCODE -ne 0) { throw "$Action 失败(exit=$LASTEXITCODE)" }
    $text = (($raw | ForEach-Object { $_.ToString() }) -join "`n").Trim()
    if ([string]::IsNullOrWhiteSpace($text)) { return $null }
    try { return ($text | ConvertFrom-Json -ErrorAction Stop) }
    catch { throw "$Action 返回非法 JSON:$($_.Exception.Message)" }
}

function Get-OnlineDSTicketKeyMaterialContract {
    param(
        [Parameter(Mandatory = $true)][string]$KubeContext,
        [Parameter(Mandatory = $true)][ValidateRange(1, 2147483647)][int]$Revision,
        [AllowEmptyString()][string]$ExpectedActiveKid = '',
        [AllowNull()]$SignerObject = $null
    )
    $dstTicketSignerSecretName = "pandora-dsticket-signer-r$Revision"
    $signer = $SignerObject
    if ($null -eq $signer) {
        $signer = Get-KubectlJsonObject -KubeContext $KubeContext `
            -Arguments @('get', "secret/$dstTicketSignerSecretName", '-n', $K8sNamespace, '-o', 'json') `
            -Action "读取 immutable DSTicket signer Secret/$dstTicketSignerSecretName"
    }
    $jwksName = "pandora-dsticket-jwks-r$Revision"
    $contract = $null
    foreach ($namespace in @('default', $K8sNamespace)) {
        $jwks = Get-KubectlJsonObject -KubeContext $KubeContext `
            -Arguments @('get', "configmap/$jwksName", '-n', $namespace, '-o', 'json') `
            -Action "读取 $namespace immutable DSTicket 公钥 JWKS ConfigMap"
        $current = Assert-PandoraDSTicketKubernetesObjects -SecretObject $signer `
            -ConfigMapObject $jwks -ExpectedRevision $Revision `
            -ExpectedActiveKid $ExpectedActiveKid -ExpectedConfigMapNamespace $namespace
        if ($null -ne $contract -and
            ($current.ActiveKid -cne $contract.ActiveKid -or $current.JwksSha256 -cne $contract.JwksSha256)) {
            throw 'default(DS) 与 pandora(Login) namespace 的 DSTicket JWKS 副本不一致。'
        }
        $contract = $current
    }
    return $contract
}

# 一次读取普通发布所需的完整 DSTicket 集群快照。不用 field selector 排除
# deletionTimestamp；terminating Pod/GameServer/marker 依然可以签票或验票，必须进契约。
function Get-OnlineLiveDSTicketRevisionRows {
    param([Parameter(Mandatory = $true)][string]$KubeContext)

    $expectedSigners = @('login', 'matchmaker', 'matchmaker-pve', 'hub-allocator')
    $expectedFleets = @('pandora-battle-stable', 'pandora-battle-canary', 'pandora-hub-stable', 'pandora-hub-canary')
    $namespace = Get-OnlineOptionalKubectlJsonObject -KubeContext $KubeContext `
        -Arguments @('get', "namespace/$K8sNamespace", '--ignore-not-found', '-o', 'json') `
        -Action "读取 Namespace/$K8sNamespace"
    if ($null -ne $namespace -and (Test-PandoraKubernetesObjectDeleting $namespace)) {
        throw "Namespace/$K8sNamespace 正在终止，拒绝普通发布。"
    }

    $allDeployments = @()
    $pandoraPods = @()
    $allReplicaSets = @()
    $activationMarkers = @()
    $terminalMarkers = @()
    $fixedConfig = $null
    if ($null -ne $namespace) {
        $deploymentList = Get-KubectlJsonObject -KubeContext $KubeContext `
            -Arguments @('get', 'deployments', '-n', $K8sNamespace, '-o', 'json') -Action '读取全部 pandora Deployment'
        $allDeployments = @($deploymentList.items)
        $podList = Get-KubectlJsonObject -KubeContext $KubeContext `
            -Arguments @('get', 'pods', '-n', $K8sNamespace, '-o', 'json') -Action '读取全部 pandora Pod（含 terminating）'
        $pandoraPods = @($podList.items)
        $replicaSetList = Get-KubectlJsonObject -KubeContext $KubeContext `
            -Arguments @('get', 'replicasets', '-n', $K8sNamespace, '-o', 'json') `
            -Action '读取全部 pandora ReplicaSet（含 terminating）'
        $allReplicaSets = @($replicaSetList.items)
        $configMaps = Get-KubectlJsonObject -KubeContext $KubeContext `
            -Arguments @('get', 'configmaps', '-n', $K8sNamespace, '-o', 'json') `
            -Action '读取全部 pandora ConfigMap（核对 DSTicket marker 集）'
        foreach ($configMap in @($configMaps.items)) {
            $name = [string]$configMap.metadata.name
            $component = [string]$configMap.metadata.labels.'app.kubernetes.io/component'
            $looksLikeMarker = $name -cmatch '^pandora-dsticket-(?:activation-signer-r|retired-r)' -or
                $component -ceq 'dsticket-rotation-audit'
            if (-not $looksLikeMarker) { continue }
            if (Test-PandoraKubernetesObjectDeleting $configMap) {
                throw "DSTicket marker ConfigMap/$name 正在终止，拒绝普通发布。"
            }
            if ($name -cmatch '^pandora-dsticket-activation-signer-r[1-9][0-9]*$') {
                $activationMarkers += $configMap
            } elseif ($name -cmatch '^pandora-dsticket-retired-r[1-9][0-9]*$') {
                $terminalMarkers += $configMap
            } else {
                throw "未知/漂移的 DSTicket marker ConfigMap/$name，拒绝普通发布。"
            }
        }
        $fixedConfig = Get-OnlineOptionalKubectlJsonObject -KubeContext $KubeContext `
            -Arguments @('get', 'secret/pandora-config', '-n', $K8sNamespace, '--ignore-not-found', '-o', 'json') `
            -Action '读取 fixed Secret/pandora-config'
    }

    $fleetList = Get-KubectlJsonObject -KubeContext $KubeContext `
        -Arguments @('get', 'fleets.agones.dev', '-n', 'default', '-o', 'json') `
        -Action '读取全部 Agones Fleet'
    # parent controller 必须在 RS/GSS/Pod 尚未生成前就完成全量判定；相关但不属于
    # 四 signer/四 Fleet 的 paused/zero 对象不能留到异步补出子对象后才发现。
    $controllerScope = Assert-PandoraOrdinaryDSTicketControllerScope `
        -DeploymentObjects $allDeployments -FleetObjects @($fleetList.items)
    $deploymentObjects = @($controllerScope.DeploymentObjects)
    $fleetObjects = @($controllerScope.FleetObjects)
    $deploymentRows = [System.Collections.Generic.List[string]]::new()
    foreach ($service in $expectedSigners) {
        $matches = @($deploymentObjects | Where-Object { [string]$_.metadata.name -ceq $service })
        if ($matches.Count -eq 0) { continue }
        if ($matches.Count -ne 1) { throw "Deployment/$service 数量=$($matches.Count)，拒绝普通发布。" }
        $volumes = @($matches[0].spec.template.spec.volumes)
        $configRefs = @($volumes | Where-Object { [string]$_.name -ceq 'conf' } | ForEach-Object { [string]$_.secret.secretName })
        $signerRefs = @($volumes | Where-Object { [string]$_.name -ceq 'dsticket' } | ForEach-Object { [string]$_.secret.secretName })
        $jwksRefs = @($volumes | Where-Object { [string]$_.name -ceq 'dsticket-jwks' } | ForEach-Object { [string]$_.configMap.name })
        $deploymentRows.Add("$service`t$($configRefs -join ',')`t$($signerRefs -join ',')`t$($jwksRefs -join ',')")
    }

    $fleetRows = [System.Collections.Generic.List[string]]::new()
    foreach ($fleet in $expectedFleets) {
        $matches = @($fleetObjects | Where-Object { [string]$_.metadata.name -ceq $fleet })
        if ($matches.Count -eq 0) { continue }
        if ($matches.Count -ne 1) { throw "Fleet/$fleet 数量=$($matches.Count)，拒绝普通发布。" }
        $containers = @($matches[0].spec.template.spec.template.spec.containers)
        $revisions = @($containers | ForEach-Object { @($_.env) } |
            Where-Object { [string]$_.name -ceq 'PANDORA_DSTICKET_KEYSET_REVISION' } | ForEach-Object { [string]$_.value })
        $volumes = @($matches[0].spec.template.spec.template.spec.volumes)
        $jwksRefs = @($volumes | Where-Object { [string]$_.name -ceq 'dsticket-jwks' } | ForEach-Object { [string]$_.configMap.name })
        $fleetRows.Add("$fleet`t$($revisions -join ',')`t$($jwksRefs -join ',')")
    }

    $gameServers = Get-KubectlJsonObject -KubeContext $KubeContext `
        -Arguments @('get', 'gameservers.agones.dev', '-n', 'default', '-o', 'json') `
        -Action '读取全部 GameServer（含 terminating）'
    $gameServerSets = Get-KubectlJsonObject -KubeContext $KubeContext `
        -Arguments @('get', 'gameserversets.agones.dev', '-n', 'default', '-o', 'json') `
        -Action '读取全部 GameServerSet（含 terminating）'
    $defaultPods = Get-KubectlJsonObject -KubeContext $KubeContext `
        -Arguments @('get', 'pods', '-n', 'default', '-o', 'json') `
        -Action '读取 default Pod（含 terminating/orphan DS）'
    $childScope = Get-PandoraOrdinaryDSTicketChildScope `
        -PandoraPodObjects $pandoraPods -ReplicaSetObjects $allReplicaSets `
        -GameServerObjects @($gameServers.items) -GameServerSetObjects @($gameServerSets.items) `
        -DefaultPodObjects @($defaultPods.items)

    return [pscustomobject]@{
        DeploymentRows = @($deploymentRows)
        FleetRows = @($fleetRows)
        DeploymentObjects = @($deploymentObjects)
        FleetObjects = @($fleetObjects)
        SignerPodObjects = @($childScope.SignerPodObjects)
        ReplicaSetObjects = @($childScope.ReplicaSetObjects)
        GameServerObjects = @($gameServers.items)
        GameServerSetObjects = @($childScope.GameServerSetObjects)
        DSPodObjects = @($childScope.DSPodObjects)
        ActivationMarkers = @($activationMarkers)
        TerminalMarkers = @($terminalMarkers)
        FixedConfigSecret = $fixedConfig
        LegacyControllerObjects = @($controllerScope.LegacyControllerObjects) + @($childScope.LegacyControllerObjects)
    }
}

function Assert-OnlineOrdinaryDSTicketState {
    param(
        [Parameter(Mandatory = $true)][string]$KubeContext,
        [Parameter(Mandatory = $true)][ValidateRange(1, 2147483647)][int]$RequestedRevision
    )
    $snapshot = Get-OnlineLiveDSTicketRevisionRows -KubeContext $KubeContext
    return Assert-PandoraOrdinaryReleaseDSTicketState `
        -DeploymentRows $snapshot.DeploymentRows -FleetRows $snapshot.FleetRows `
        -RequestedRevision $RequestedRevision -DeploymentObjects $snapshot.DeploymentObjects `
        -FleetObjects $snapshot.FleetObjects -SignerPodObjects $snapshot.SignerPodObjects `
        -ReplicaSetObjects $snapshot.ReplicaSetObjects -GameServerObjects $snapshot.GameServerObjects `
        -GameServerSetObjects $snapshot.GameServerSetObjects -DSPodObjects $snapshot.DSPodObjects `
        -ActivationMarkers $snapshot.ActivationMarkers -TerminalMarkers $snapshot.TerminalMarkers `
        -FixedConfigSecret $snapshot.FixedConfigSecret -LegacyControllerObjects $snapshot.LegacyControllerObjects
}

function Enter-OnlineDSTicketOperationLock {
    param([Parameter(Mandatory = $true)][string]$KubeContext)
    $holderId = [guid]::NewGuid().ToString('N')
    $operation = 'ordinary-online'
    $object = New-PandoraDSTicketOperationLockObject -HolderId $holderId -Operation $operation
    $json = $object | ConvertTo-Json -Depth 20 -Compress
    $createdLines = @($json | & kubectl --context $KubeContext create -f - -o json 2>$null)
    if ($LASTEXITCODE -ne 0) {
        throw '获取 DSTicket 操作互斥锁失败；可能已有普通发布/轮换正在进行，或存在崩溃残留锁。' +
              '请只读审计 pandora/ConfigMap/pandora-dsticket-operation-lock；禁止自动抢锁/按本机时间判过期。'
    }
    $createdText = (($createdLines | ForEach-Object { $_.ToString() }) -join "`n").Trim()
    try { $created = $createdText | ConvertFrom-Json -ErrorAction Stop }
    catch { throw "DSTicket 操作锁 create 返回非法 JSON:$($_.Exception.Message)" }
    $identity = Assert-PandoraDSTicketOperationLockContract -LockObject $created `
        -HolderId $holderId -Operation $operation -RequireLiveIdentity
    $live = Get-KubectlJsonObject -KubeContext $KubeContext `
        -Arguments @('get', 'configmap/pandora-dsticket-operation-lock', '-n', $K8sNamespace, '-o', 'json') `
        -Action '回读 DSTicket 操作互斥锁'
    $liveIdentity = Assert-PandoraDSTicketOperationLockContract -LockObject $live `
        -HolderId $holderId -Operation $operation -RequireLiveIdentity
    if ($liveIdentity.Uid -cne $identity.Uid -or $liveIdentity.ResourceVersion -cne $identity.ResourceVersion) {
        throw 'DSTicket 操作锁 create/回读身份漂移，拒绝进入集群写阶段。'
    }
    return [pscustomobject]@{
        HolderId = $holderId
        Operation = $operation
        Uid = $identity.Uid
        ResourceVersion = $identity.ResourceVersion
    }
}

function Assert-OnlineDSTicketOperationLockHeld {
    param(
        [Parameter(Mandatory = $true)][string]$KubeContext,
        [Parameter(Mandatory = $true)]$Identity
    )
    $live = Get-KubectlJsonObject -KubeContext $KubeContext `
        -Arguments @('get', 'configmap/pandora-dsticket-operation-lock', '-n', $K8sNamespace, '-o', 'json') `
        -Action '核对 DSTicket 操作互斥锁持有权'
    $contract = Assert-PandoraDSTicketOperationLockContract -LockObject $live `
        -HolderId ([string]$Identity.HolderId) -Operation ([string]$Identity.Operation) -RequireLiveIdentity
    if ($contract.Uid -cne [string]$Identity.Uid -or
        $contract.ResourceVersion -cne [string]$Identity.ResourceVersion) {
        throw 'DSTicket 操作锁 UID/resourceVersion 已漂移，拒绝继续任何集群写入。'
    }
    return $contract
}

function Exit-OnlineDSTicketOperationLock {
    param(
        [Parameter(Mandatory = $true)][string]$KubeContext,
        [Parameter(Mandatory = $true)]$Identity
    )
    $held = Assert-OnlineDSTicketOperationLockHeld -KubeContext $KubeContext -Identity $Identity
    # kubectl v1.34 没有 delete --preconditions flag；raw DELETE 的 body 使用 Kubernetes
    # DeleteOptions UID+resourceVersion 双前置条件，防旧进程在 ABA 场景误删后继 holder。
    $deleteOptions = [ordered]@{
        apiVersion = 'v1'
        kind = 'DeleteOptions'
        preconditions = [ordered]@{ uid = $held.Uid; resourceVersion = $held.ResourceVersion }
    } | ConvertTo-Json -Depth 10 -Compress
    $deletePath = '/api/v1/namespaces/pandora/configmaps/pandora-dsticket-operation-lock'
    $deleted = @($deleteOptions | & kubectl --context $KubeContext delete "--raw=$deletePath" -f - 2>$null)
    if ($LASTEXITCODE -ne 0) {
        throw '带 UID/resourceVersion 前置条件释放 DSTicket 操作锁失败；锁按 fail-closed 语义保留，须人工审计。'
    }
    $after = Get-OnlineOptionalKubectlJsonObject -KubeContext $KubeContext `
        -Arguments @('get', 'configmap/pandora-dsticket-operation-lock', '-n', $K8sNamespace, '--ignore-not-found', '-o', 'json') `
        -Action '复查 DSTicket 操作锁释放结果'
    if ($null -eq $after) { return }
    $afterUid = [string]$after.metadata.uid
    if ($afterUid -ceq [string]$Identity.Uid) {
        throw 'DSTicket 操作锁 DELETE 返回后同 UID 对象仍存在；拒绝把释放误报成功。'
    }
    $afterHolder = [string]$after.metadata.annotations.'pandora.dev/dsticket-lock-holder-id'
    $afterOperation = [string]$after.metadata.annotations.'pandora.dev/dsticket-lock-operation'
    $null = Assert-PandoraDSTicketOperationLockContract -LockObject $after `
        -HolderId $afterHolder -Operation $afterOperation -RequireLiveIdentity
}

function Get-PandoraFleetContractRows {
    param([Parameter(Mandatory = $true)][string]$Manifest)
    $jsonPath = '{.kind}{"\t"}{.metadata.name}{"\t"}{.spec.template.spec.template.spec.containers[*].name}{"\t"}{.spec.template.spec.template.spec.containers[*].image}{"\t"}{.spec.template.spec.template.spec.containers[*].imagePullPolicy}{"\t"}{.spec.template.metadata.annotations.pandora\.dev/image-digest}{"\t"}{.spec.template.spec.template.metadata.annotations.pandora\.dev/image-digest}{"\t"}{.metadata.labels.pandora\.dev/release-track}{"\t"}{.spec.template.metadata.labels.pandora\.dev/release-track}{"\t"}{.spec.template.spec.template.metadata.labels.pandora\.dev/release-track}{"\n"}'
    return @(Invoke-KubectlClientContract -Manifest $Manifest -JsonPath $jsonPath -Action 'Fleet manifest')
}

function New-OnlineRuntimeOverlay {
    param(
        [Parameter(Mandatory = $true)][string]$SourceOverlay,
        [Parameter(Mandatory = $true)][string]$Registry,
        [Parameter(Mandatory = $true)][hashtable]$Digests,
        [Parameter(Mandatory = $true)][string[]]$ServiceNames,
        [Parameter(Mandatory = $true)][string[]]$WriterServices,
        [Parameter(Mandatory = $true)][string]$EnvironmentName,
        [Parameter(Mandatory = $true)][ValidateRange(1, 2147483647)][int]$DSTicketKeysetRevision,
        [string]$EtcdIdentityRevision = '',
        [string]$EtcdServerName = '',
        [string]$EtcdForbiddenReadPrefix = '',
        [hashtable]$EtcdPasswordAuthByService = @{},
        [hashtable]$GreenDesiredReplicas = @{},
        [hashtable]$GreenDeploymentObjects = @{},
        [switch]$CanonicalDsAuthGreen
    )
    $parent = Split-Path -Parent $SourceOverlay
    $runtime = Join-Path $parent ('.online-runtime-' + $EnvironmentName + '-' + [guid]::NewGuid().ToString('N'))
    try {
        Copy-Item -LiteralPath $SourceOverlay -Destination $runtime -Recurse
        $kustomizationPath = Join-Path $runtime 'kustomization.yaml'
        $text = Get-Content -LiteralPath $kustomizationPath -Raw
        $text = ConvertTo-PandoraDigestKustomization -Template $text -Registry $Registry -Digests $Digests -ServiceNames $ServiceNames
        $dsticketPatchName = 'dsticket-keyset-login.yaml'
        $dsticketSignerServices = @('login', 'matchmaker', 'matchmaker-pve', 'hub-allocator')
        $dsticketSignerPatchNames = @($dsticketSignerServices | ForEach-Object { "dsticket-signer-$_.yaml" })
        $etcdIdentityPatchNames = if ([string]::IsNullOrWhiteSpace($EtcdIdentityRevision)) { @() } else {
            @($WriterServices | ForEach-Object { "ds-auth-etcd-identity-$_.yaml" })
        }
        $terminalPatchNames = if ($CanonicalDsAuthGreen) {
            @($WriterServices | ForEach-Object { "ds-auth-dormant-$_.yaml" }) +
            @($WriterServices | ForEach-Object { "ds-auth-service-green-$_.yaml" })
        } else { @() }
        $text = Add-PandoraWriterPatchEntries -Kustomization $text -WriterServices $WriterServices `
            -AdditionalPatchPaths (@($dsticketPatchName) + $dsticketSignerPatchNames + $etcdIdentityPatchNames +
                $terminalPatchNames)
        [System.IO.File]::WriteAllText($kustomizationPath, $text, [System.Text.UTF8Encoding]::new($false))
        foreach ($writer in $WriterServices) {
            $patchText = New-PandoraWriterDigestPatch -Service $writer -Digest ([string]$Digests[$writer])
            [System.IO.File]::WriteAllText((Join-Path $runtime "writer-digest-$writer.yaml"), $patchText, [System.Text.UTF8Encoding]::new($false))
        }
        $dsticketPatch = New-PandoraLoginDSTicketJWKSRevisionPatch -Revision $DSTicketKeysetRevision
        [System.IO.File]::WriteAllText((Join-Path $runtime $dsticketPatchName), $dsticketPatch, [System.Text.UTF8Encoding]::new($false))
        foreach ($signer in $dsticketSignerServices) {
            $signerPatch = New-PandoraDSTicketSignerSecretRevisionPatch -Service $signer -Revision $DSTicketKeysetRevision
            [System.IO.File]::WriteAllText((Join-Path $runtime "dsticket-signer-$signer.yaml"), $signerPatch, [System.Text.UTF8Encoding]::new($false))
        }
        if (-not [string]::IsNullOrWhiteSpace($EtcdIdentityRevision)) {
            foreach ($writer in $WriterServices) {
                if (-not $EtcdPasswordAuthByService.ContainsKey($writer)) { throw "缺 writer/$writer etcd password-auth contract。" }
                $identityPatch = New-PandoraDsAuthEtcdIdentityPatch -App $writer -Revision $EtcdIdentityRevision `
                    -ServerName $EtcdServerName -ForbiddenReadPrefix $EtcdForbiddenReadPrefix `
                    -UsesPasswordAuth ([bool]$EtcdPasswordAuthByService[$writer])
                [System.IO.File]::WriteAllText((Join-Path $runtime "ds-auth-etcd-identity-$writer.yaml"), $identityPatch, [System.Text.UTF8Encoding]::new($false))
            }
        }
        $greenPatchPaths = @{}
        if ($CanonicalDsAuthGreen) {
            if ([string]::IsNullOrWhiteSpace($EtcdIdentityRevision)) { throw 'canonical green 必须配置 etcd identity revision。' }
            foreach ($writer in $WriterServices) {
                if (-not $GreenDesiredReplicas.ContainsKey($writer)) { throw "缺 canonical green/$writer desired replicas。" }
                if (-not $GreenDeploymentObjects.ContainsKey($writer)) { throw "缺 canonical green/$writer live Deployment object。" }
                $dormantPatch = New-PandoraDsAuthDormantBluePatch -App $writer
                [System.IO.File]::WriteAllText((Join-Path $runtime "ds-auth-dormant-$writer.yaml"), $dormantPatch, [System.Text.UTF8Encoding]::new($false))
                $servicePatch = New-PandoraDsAuthGreenServicePatch -App $writer
                [System.IO.File]::WriteAllText((Join-Path $runtime "ds-auth-service-green-$writer.yaml"), $servicePatch, [System.Text.UTF8Encoding]::new($false))
                $pin = New-PandoraPinnedImageReference -Reference ($Registry.TrimEnd('/') + "/pandora/$writer") -Digest ([string]$Digests[$writer])
                $greenObject = New-PandoraDsAuthCanonicalGreenObject -LiveDeployment $GreenDeploymentObjects[$writer] `
                    -App $writer -Revision $EtcdIdentityRevision `
                    -ServerName $EtcdServerName -ForbiddenReadPrefix $EtcdForbiddenReadPrefix `
                    -UsesPasswordAuth ([bool]$EtcdPasswordAuthByService[$writer]) `
                    -DesiredReplicas ([int]$GreenDesiredReplicas[$writer]) -PinnedImage $pin -Digest ([string]$Digests[$writer])
                $greenPatchPath = Join-Path $runtime "ds-auth-green-release-$writer.json"
                $greenJSON = $greenObject | ConvertTo-Json -Depth 40
                [System.IO.File]::WriteAllText($greenPatchPath, $greenJSON, [System.Text.UTF8Encoding]::new($false))
                $greenPatchPaths[$writer] = $greenPatchPath
            }
        }
        $renderedLines = @(& kubectl kustomize $runtime 2>&1)
        if ($LASTEXITCODE -ne 0) {
            throw "最终 online overlay 渲染失败:$($renderedLines -join [Environment]::NewLine)"
        }
        $rendered = ($renderedLines | ForEach-Object { $_.ToString() }) -join [Environment]::NewLine
        $pins = @{}
        foreach ($name in $ServiceNames) {
            $pins[$name] = New-PandoraPinnedImageReference -Reference ($Registry.TrimEnd('/') + "/pandora/$name") -Digest ([string]$Digests[$name])
        }
        $contractRows = Get-PandoraWorkloadContractRows -Manifest $rendered
        Assert-PandoraRenderedOnlineContract -ContractRows $contractRows -Pins $pins -Digests $Digests -ServiceNames $ServiceNames -WriterServices $WriterServices
        $dsticketRows = Get-PandoraDSTicketSignerContractRows -Manifest $rendered
        Assert-PandoraDSTicketSignerRevisionContract -ContractRows $dsticketRows -Revision $DSTicketKeysetRevision
        return [pscustomobject]@{ Path = $runtime; Rendered = $rendered; GreenPatchPaths = $greenPatchPaths }
    } catch {
        if (Test-Path -LiteralPath $runtime -PathType Container) {
            $resolved = [System.IO.Path]::GetFullPath($runtime)
            if ((Split-Path -Parent $resolved).Equals([System.IO.Path]::GetFullPath($parent), [System.StringComparison]::OrdinalIgnoreCase) -and
                (Split-Path -Leaf $resolved) -match ('^\.online-runtime-' + [regex]::Escape($EnvironmentName) + '-[0-9a-f]{32}$')) {
                Remove-Item -LiteralPath $resolved -Recurse -Force -ErrorAction SilentlyContinue
            }
        }
        throw
    }
}

function Get-OnlineDsAuthEndpointUIDs([string]$KubeContext, [string]$ServiceName, $Service, $ExpectedPods) {
    $slices = Get-KubectlJsonObject -KubeContext $KubeContext `
        -Arguments @('get', 'endpointslices', '-n', $K8sNamespace, '-l', "kubernetes.io/service-name=$ServiceName", '-o', 'json') `
        -Action "回读 Service/$ServiceName EndpointSlice"
    return Get-PandoraDsAuthVerifiedEndpointUIDSet $slices $Service $ExpectedPods $K8sNamespace
}

function Get-OnlineDsAuthCanonicalState {
    param(
        [Parameter(Mandatory = $true)][string]$KubeContext,
        [Parameter(Mandatory = $true)][string[]]$WriterServices,
        [Parameter(Mandatory = $true)][string]$Revision,
        [Parameter(Mandatory = $true)][string]$ServerName,
        [Parameter(Mandatory = $true)][string]$ForbiddenReadPrefix,
        [hashtable]$ExpectedDigests = @{}
    )
    $capabilityNames = @{
        'login' = 'login'; 'player-locator' = 'player_locator'; 'ds-allocator' = 'ds_allocator'
        'hub-allocator' = 'hub_allocator'; 'battle-result' = 'battle_result'
    }
    $counts = [ordered]@{}
    $instances = [ordered]@{}
    $allowed = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::Ordinal)
    $serviceDigests = [ordered]@{}
    $passwordAuth = @{}
    $desiredReplicas = @{}
    $greenDeploymentObjects = @{}
    foreach ($writer in $WriterServices) {
        $secretName = Get-PandoraDsAuthIdentitySecretName $writer $Revision
        $secret = Get-KubectlJsonObject -KubeContext $KubeContext `
            -Arguments @('get', "secret/$secretName", '-n', $K8sNamespace, '-o', 'json') `
            -Action "终态回读 DS auth etcd identity Secret/$secretName"
        $contract = Assert-PandoraDsAuthIdentitySecretContract $secret $writer $Revision
        $passwordAuth[$writer] = [bool]$contract.UsesPasswordAuth

        $blue = Get-KubectlJsonObject -KubeContext $KubeContext `
            -Arguments @('get', "deployment/$writer", '-n', $K8sNamespace, '-o', 'json') `
            -Action "回读 dormant blue Deployment/$writer"
        if ([int]$blue.spec.replicas -ne 0 -or [string]$blue.spec.selector.matchLabels.app -cne $writer -or
            [string]$blue.spec.selector.matchLabels.'pandora.dev/ds-auth-writer-set' -cne 'blue' -or
            [string]$blue.spec.selector.matchLabels.'pandora.dev/ds-auth-writer-epoch' -cne '1' -or
            @($blue.spec.selector.matchLabels.PSObject.Properties).Count -ne 3) {
            throw "Deployment/$writer 不是 exact dormant blue epoch=1。"
        }
        $bluePods = Get-KubectlJsonObject -KubeContext $KubeContext `
            -Arguments @('get', 'pods', '-n', $K8sNamespace, '-l', "app=$writer,pandora.dev/ds-auth-writer-set=blue", '-o', 'json') `
            -Action "回读 dormant blue writer/$writer Pod"
        if (@($bluePods.items).Count -ne 0) { throw "writer/$writer blue Pod 必须为 0。" }

        $greenName = "$writer-ds-auth-green"
        $deployment = Get-KubectlJsonObject -KubeContext $KubeContext `
            -Arguments @('get', "deployment/$greenName", '-n', $K8sNamespace, '-o', 'json') `
            -Action "回读 canonical green Deployment/$greenName"
        $desiredRaw = [string]$deployment.metadata.annotations.'pandora.dev/ds-auth-green-desired-replicas'
        if ($desiredRaw -cnotmatch '^[1-9][0-9]?$') { throw "Deployment/$greenName desired replicas annotation 非 canonical。" }
        $want = [int]$desiredRaw
        $desiredReplicas[$writer] = $want
        $greenDeploymentObjects[$writer] = $deployment
        if ([int]$deployment.spec.replicas -ne $want -or [string]$deployment.spec.selector.matchLabels.app -cne $writer -or
            [string]$deployment.spec.selector.matchLabels.'pandora.dev/ds-auth-writer-set' -cne 'green' -or
            [string]$deployment.spec.selector.matchLabels.'pandora.dev/ds-auth-writer-epoch' -cne '2' -or
            @($deployment.spec.selector.matchLabels.PSObject.Properties).Count -ne 3) {
            throw "Deployment/$greenName immutable selector/replicas 不是 canonical green。"
        }
        $template = [pscustomobject]@{ metadata = $deployment.spec.template.metadata; spec = $deployment.spec.template.spec }
        Assert-PandoraDsAuthIdentityPodContract $template $writer $Revision $ServerName $ForbiddenReadPrefix ([bool]$contract.UsesPasswordAuth)
        if ($want -lt 1 -or [int]$deployment.status.readyReplicas -ne $want -or [int]$deployment.status.updatedReplicas -ne $want) {
            throw "writer/$greenName rollout 未稳定。"
        }
        $templateDigest = [string]$deployment.spec.template.metadata.annotations.'pandora.dev/image-digest'
        if ($templateDigest -cnotmatch '^sha256:[0-9a-f]{64}$') { throw "writer/$greenName template digest 非 immutable。" }
        if ($ExpectedDigests.Count -gt 0 -and [string]$ExpectedDigests[$writer] -cne $templateDigest) { throw "writer/$greenName 未命中本次 release digest。" }
        $podList = Get-KubectlJsonObject -KubeContext $KubeContext `
            -Arguments @('get', 'pods', '-n', $K8sNamespace, '-l', "app=$writer,pandora.dev/ds-auth-writer-set=green", '-o', 'json') `
            -Action "回读 canonical green writer/$writer Pod"
        $pods = @($podList.items | Where-Object { -not (Test-PandoraKubernetesObjectDeleting $_) })
        if ($pods.Count -ne $want) { throw "writer/$writer live Pod 数不等于 desired。" }
        $uids = [System.Collections.Generic.List[string]]::new()
        foreach ($pod in $pods) {
            Assert-PandoraDsAuthIdentityPodContract $pod $writer $Revision $ServerName $ForbiddenReadPrefix ([bool]$contract.UsesPasswordAuth)
            if ([string]$pod.metadata.labels.'pandora.dev/ds-auth-writer-epoch' -cne '2') { throw "writer/$writer Pod epoch 不是 2。" }
            if ($writer -in @('battle-result', 'ds-allocator')) { Assert-PandoraDsTerminalMeshPodContract $pod $writer }
            $status = @($pod.status.containerStatuses | Where-Object name -eq $writer)
            if ($status.Count -ne 1 -or -not ([string]$status[0].imageID).EndsWith($templateDigest, [StringComparison]::Ordinal)) {
                throw "writer/$writer Pod imageID 未命中 canonical green digest。"
            }
            $uids.Add([string]$pod.metadata.uid)
        }
        $serviceObject = Get-KubectlJsonObject -KubeContext $KubeContext `
            -Arguments @('get', "service/$writer", '-n', $K8sNamespace, '-o', 'json') `
            -Action "回读 canonical green Service/$writer"
        if ([string]$serviceObject.spec.selector.app -cne $writer -or
            [string]$serviceObject.spec.selector.'pandora.dev/ds-auth-writer-set' -cne 'green' -or
            [string]$serviceObject.spec.selector.'pandora.dev/ds-auth-writer-epoch' -cne '2' -or
            @($serviceObject.spec.selector.PSObject.Properties).Count -ne 3) { throw "Service/$writer selector 不是 exact canonical green。" }
        $endpointUIDs = Get-OnlineDsAuthEndpointUIDs -KubeContext $KubeContext -ServiceName $writer `
            -Service $serviceObject -ExpectedPods $pods
        if ($endpointUIDs.Count -ne $uids.Count) { throw "Service/$writer Endpoint UID 数不等于 green Pod。" }
        foreach ($uid in $uids) { if (-not $endpointUIDs.Contains($uid)) { throw "Service/$writer Endpoint UID 集不等于 green Pod。" } }
        $capabilityName = [string]$capabilityNames[$writer]
        $counts[$capabilityName] = $want
        $instances[$capabilityName] = (@($uids | Sort-Object) -join '|')
        $null = $allowed.Add($templateDigest)
        $serviceDigests[$capabilityName] = $templateDigest
    }
    $peer = Get-KubectlJsonObject -KubeContext $KubeContext `
        -Arguments @('get', 'peerauthentication.security.istio.io/pandora-ds-allocator-terminal-permissive', '-n', $K8sNamespace, '-o', 'json') `
        -Action '终态回读 ReleaseBattle PeerAuthentication'
    $policy = Get-KubectlJsonObject -KubeContext $KubeContext `
        -Arguments @('get', 'authorizationpolicy.security.istio.io/pandora-ds-terminal-release-exact-deny', '-n', $K8sNamespace, '-o', 'json') `
        -Action '终态回读 ReleaseBattle AuthorizationPolicy'
    $service = Get-KubectlJsonObject -KubeContext $KubeContext `
        -Arguments @('get', 'service/ds-allocator', '-n', $K8sNamespace, '-o', 'json') `
        -Action '终态回读 ds-allocator gRPC Service'
    Assert-PandoraDsTerminalMeshPolicyContract $peer $policy $service

    return [pscustomobject]@{
        ExpectedServices = (($counts.Keys | ForEach-Object { "$_=$($counts[$_])" }) -join ',')
        ExpectedInstances = (($instances.Keys | ForEach-Object { "$_=$($instances[$_])" }) -join ',')
        AllowedDigests = (@($allowed | Sort-Object) -join ',')
        ExpectedDigests = (($serviceDigests.Keys | ForEach-Object { "$_=$($serviceDigests[$_])" }) -join ',')
        PasswordAuth = $passwordAuth
        DesiredReplicas = $desiredReplicas
        GreenDeploymentObjects = $greenDeploymentObjects
    }
}

function Invoke-OnlineDsAuthCapabilityAudit {
    param(
        [Parameter(Mandatory = $true)]$State,
        [Parameter(Mandatory = $true)][string]$EtcdEndpoints,
        [Parameter(Mandatory = $true)][string]$KeysetRevision,
        [Parameter(Mandatory = $true)][string]$Revision,
        [Parameter(Mandatory = $true)][string[]]$SecureGoArgs
    )
    $deadline = [datetime]::UtcNow.AddSeconds(45)
    Push-Location $ProjectRoot
    try {
        do {
            & go run ./pkg/dsauthfence/cmd/dsauth-activate --endpoints $EtcdEndpoints `
                --expected-services $State.ExpectedServices --expected-instances $State.ExpectedInstances `
                --expected-epoch 2 --target-epoch 2 --keyset-revision $KeysetRevision `
                --etcd-identity-revision $Revision --allowed-image-digests $State.AllowedDigests `
                --expected-image-digests $State.ExpectedDigests `
                --required-features $script:PandoraDsAuthRequiredFeatures @SecureGoArgs
            if ($LASTEXITCODE -eq 0) { return }
            if ([datetime]::UtcNow -ge $deadline) { throw 'online writer exact capability/features 终态审计失败。' }
            Start-Sleep -Seconds 2
        } while ($true)
    } finally { Pop-Location }
}

function Assert-OnlineDsAuthRuntimeAndCapabilities {
    param(
        [Parameter(Mandatory = $true)][string]$KubeContext,
        [Parameter(Mandatory = $true)][string[]]$WriterServices,
        [Parameter(Mandatory = $true)][string]$Revision,
        [Parameter(Mandatory = $true)][string]$ServerName,
        [Parameter(Mandatory = $true)][string]$ForbiddenReadPrefix,
        [Parameter(Mandatory = $true)][string]$EtcdEndpoints,
        [Parameter(Mandatory = $true)][string]$KeysetRevision,
        [Parameter(Mandatory = $true)][string[]]$SecureGoArgs,
        [hashtable]$ExpectedDigests = @{}
    )
    $state = Get-OnlineDsAuthCanonicalState -KubeContext $KubeContext -WriterServices $WriterServices `
        -Revision $Revision -ServerName $ServerName -ForbiddenReadPrefix $ForbiddenReadPrefix -ExpectedDigests $ExpectedDigests
    Invoke-OnlineDsAuthCapabilityAudit -State $state -EtcdEndpoints $EtcdEndpoints -KeysetRevision $KeysetRevision `
        -Revision $Revision -SecureGoArgs $SecureGoArgs
    return $state
}

function Assert-OnlineDeploymentImageState {
    param(
        [Parameter(Mandatory = $true)][string]$KubeContext,
        [Parameter(Mandatory = $true)][hashtable]$Pins,
        [Parameter(Mandatory = $true)][hashtable]$Digests,
        [Parameter(Mandatory = $true)][string[]]$WriterServices,
        [switch]$CanonicalGreen
    )
    foreach ($svc in (Get-ServiceList)) {
        $name = [string]$svc.Name
        $deploymentName = if ($CanonicalGreen -and $WriterServices -contains $name) { "$name-ds-auth-green" } else { $name }
        $deployment = Get-KubectlJsonObject -KubeContext $KubeContext `
            -Arguments @('get', "deployment/$deploymentName", '-n', $K8sNamespace, '-o', 'json') `
            -Action "回读 Deployment/$deploymentName 镜像"
        $want = [int]$deployment.spec.replicas
        if ([int]$deployment.status.updatedReplicas -ne $want -or [int]$deployment.status.readyReplicas -ne $want -or
            [int]$deployment.status.availableReplicas -ne $want -or [int]$deployment.status.unavailableReplicas -ne 0) {
            throw "Deployment/$deploymentName rollout 未稳定到 $want 个新副本。"
        }
        $podSelector = if ($CanonicalGreen -and $WriterServices -contains $name) { "app=$name,pandora.dev/ds-auth-writer-set=green" } else { "app=$name" }
        $pods = Get-KubectlJsonObject -KubeContext $KubeContext `
            -Arguments @('get', 'pods', '-n', $K8sNamespace, '-l', $podSelector, '-o', 'json') `
            -Action "回读 Deployment/$deploymentName Pod"
        $live = @($pods.items | Where-Object { -not (Test-PandoraKubernetesObjectDeleting $_) })
        if ($live.Count -ne $want) { throw "Deployment/$name 非终止 Pod 数=$($live.Count)，expected=$want。" }
        foreach ($pod in $live) {
            $containers = @($pod.spec.containers | Where-Object name -eq $name)
            $statuses = @($pod.status.containerStatuses | Where-Object name -eq $name)
            if ($containers.Count -ne 1 -or [string]$containers[0].image -cne [string]$Pins[$name]) {
                throw "Pod/$($pod.metadata.name) spec.image 未固定到本次 pin $($Pins[$name])。"
            }
            if ($statuses.Count -ne 1 -or -not ([string]$statuses[0].imageID).EndsWith([string]$Digests[$name], [StringComparison]::Ordinal)) {
                throw "Pod/$($pod.metadata.name) imageID 未命中本次 digest $($Digests[$name])。"
            }
            if ($WriterServices -contains $name) {
                $actual = [string]$pod.metadata.annotations.'pandora.dev/image-digest'
                if ($actual -cne [string]$Digests[$name]) {
                    throw "writer Pod/$($pod.metadata.name) digest annotation=$actual，expected=$($Digests[$name])。"
                }
            }
        }
    }
}

function Wait-OnlineReadyFleetImageState {
    param(
        [Parameter(Mandatory = $true)][string]$KubeContext,
        [Parameter(Mandatory = $true)][string]$Fleet,
        [Parameter(Mandatory = $true)][string]$Container,
        [Parameter(Mandatory = $true)][string]$Pin,
        [Parameter(Mandatory = $true)][string]$Digest,
        [Parameter(Mandatory = $true)][ValidateSet('stable', 'canary')][string]$ExpectedTrack,
        [timespan]$Timeout = [timespan]::FromMinutes(5)
    )
    Assert-PandoraDigest -Digest $Digest -Where "Fleet/$Fleet"
    $deadline = [DateTime]::UtcNow.Add($Timeout)
    $lastReason = '尚未检查'
    do {
        $fleetObject = Get-KubectlJsonObject -KubeContext $KubeContext `
            -Arguments @('get', "fleet/$Fleet", '-n', 'default', '-o', 'json') `
            -Action "回读 Fleet/$Fleet 状态"
        $list = Get-KubectlJsonObject -KubeContext $KubeContext `
            -Arguments @('get', 'gameservers', '-n', 'default', '-l', "agones.dev/fleet=$Fleet", '-o', 'json') `
            -Action "回读 Fleet/$Fleet GameServer"
        $desired = [int]$fleetObject.spec.replicas
        $reportedTotal = [int]$fleetObject.status.replicas
        $reportedReady = [int]$fleetObject.status.readyReplicas
        $liveServers = @($list.items | Where-Object { -not (Test-PandoraKubernetesObjectDeleting $_) })
        $ready = @($liveServers | Where-Object { [string]$_.status.state -ceq 'Ready' })
        $allocated = @($liveServers | Where-Object { [string]$_.status.state -ceq 'Allocated' })
        $reserved = @($liveServers | Where-Object { [string]$_.status.state -ceq 'Reserved' })
        $stableCount = $ready.Count + $allocated.Count + $reserved.Count
        $wrongTrack = @($liveServers | Where-Object { [string]$_.metadata.labels.'pandora.dev/release-track' -cne $ExpectedTrack })
        if ($desired -lt 0) {
            $lastReason = "Fleet spec.replicas=$desired 非法"
        } elseif ($wrongTrack.Count -gt 0) {
            $lastReason = "GameServer release-track 漂移:$(@($wrongTrack | ForEach-Object { $_.metadata.name }) -join ',')"
        } elseif ($reportedTotal -ne $liveServers.Count -or $stableCount -ne $liveServers.Count) {
            $lastReason = "容量未稳定:desired-ready=$desired status.total=$reportedTotal live=$($liveServers.Count) ready/allocated/reserved=$($ready.Count)/$($allocated.Count)/$($reserved.Count)"
        } elseif ($reportedReady -ne $ready.Count) {
            $lastReason = "Fleet status.readyReplicas=$reportedReady 与实际 Ready=$($ready.Count) 不一致"
        } elseif ($desired -eq 0 -and $ready.Count -eq 0) {
            Write-Ok "Fleet/$Fleet 已缩到 0 Ready（不再接新分配）；旧 Allocated=$($allocated.Count) 可继续受控排空。"
            return
        } elseif ($ready.Count -lt $desired) {
            $lastReason = "Ready 池未补足:desired=$desired actual=$($ready.Count)"
        } elseif ($ready.Count -eq 0) {
            $lastReason = '没有 Ready GameServer（不能证明新分配池已切新镜像）'
        } else {
            $bad = [System.Collections.Generic.List[string]]::new()
            foreach ($gs in $ready) {
                $name = [string]$gs.metadata.name
                $gsDigest = [string]$gs.metadata.annotations.'pandora.dev/image-digest'
                if ($gsDigest -cne $Digest) { $bad.Add("GameServer/$name annotation=$gsDigest") }
                $pod = Get-KubectlJsonObject -KubeContext $KubeContext `
                    -Arguments @('get', "pod/$name", '-n', 'default', '-o', 'json') `
                    -Action "回读 Ready GameServer Pod/$name"
                $podDigest = [string]$pod.metadata.annotations.'pandora.dev/image-digest'
                $podTrack = [string]$pod.metadata.labels.'pandora.dev/release-track'
                $spec = @($pod.spec.containers | Where-Object name -eq $Container)
                $status = @($pod.status.containerStatuses | Where-Object name -eq $Container)
                if ($podDigest -cne $Digest) { $bad.Add("Pod/$name annotation=$podDigest") }
                if ($podTrack -cne $ExpectedTrack) { $bad.Add("Pod/$name release-track=$podTrack") }
                if ($spec.Count -ne 1 -or [string]$spec[0].image -cne $Pin) { $bad.Add("Pod/$name spec.image=$($spec[0].image)") }
                if ($status.Count -ne 1 -or -not ([string]$status[0].imageID).EndsWith($Digest, [StringComparison]::Ordinal)) {
                    $bad.Add("Pod/$name imageID=$($status[0].imageID)")
                }
            }
            if ($bad.Count -eq 0) {
                Write-Ok "Fleet/$Fleet 可新分配 Ready 池全部命中 $Digest（旧 Allocated 对局允许受控排空）。"
                return
            }
            $lastReason = $bad -join '; '
        }
        if ([DateTime]::UtcNow -lt $deadline) { Start-Sleep -Seconds 5 }
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Fleet/$Fleet 未在 $([int]$Timeout.TotalSeconds)s 内把 Ready 池收敛到本次 digest:$lastReason。" +
          '不会强删仍在对局的旧 Allocated GameServer；请等待其按 battle_ttl 排空后另做全收敛审计。'
}

function Get-WorkloadTemplateVolumes($item) {
    switch ([string]$item.kind) {
        'Deployment'  { return @($item.spec.template.spec.volumes) }
        'StatefulSet' { return @($item.spec.template.spec.volumes) }
        'DaemonSet'   { return @($item.spec.template.spec.volumes) }
        'Job'         { return @($item.spec.template.spec.volumes) }
        'CronJob'     { return @($item.spec.jobTemplate.spec.template.spec.volumes) }
        default       { return @() }
    }
}

function Test-HasLegacyPandoraConfigMapRef($Node) {
    # ConvertFrom-Json 会把 restartedAt 等 ISO 字符串还原为 DateTime。DateTime 不是 primitive，
    # 但它和 TimeSpan/enum 等值类型的适配属性会不断产生新的值；继续递归同样会溢出。
    if ($null -eq $Node -or $Node -is [string] -or $Node.GetType().IsValueType) { return $false }
    # PowerShell 的 [pscustomobject] 类型加速器对 Object[] 也会返回 true；旧条件因此把 JSON
    # 数组当普通对象遍历，并沿数组的 SyncRoot 自引用无限递归，最终让 Resume 在 rollout 后
    # 报 call depth overflow。字符串已在上面排除，其余 IEnumerable 必须先逐项处理。
    if ($Node -is [System.Collections.IEnumerable]) {
        foreach ($entry in $Node) {
            if (Test-HasLegacyPandoraConfigMapRef $entry) { return $true }
        }
        return $false
    }
    foreach ($property in $Node.PSObject.Properties) {
        if ($property.Name -in @('configMap', 'configMapRef', 'configMapKeyRef')) {
            $nameProperty = if ($null -eq $property.Value) { $null } else { $property.Value.PSObject.Properties['name'] }
            if ($null -ne $nameProperty -and [string]$nameProperty.Value -ceq 'pandora-config') { return $true }
        }
        if (Test-HasLegacyPandoraConfigMapRef $property.Value) { return $true }
    }
    return $false
}

function Get-LegacyPandoraConfigMapRefs([object[]]$Items) {
    return @(
        foreach ($item in $Items) {
            if ($null -ne $item -and (Test-HasLegacyPandoraConfigMapRef $item.spec)) {
                "$($item.kind)/$($item.metadata.name)"
            }
        }
    )
}

# Secret 迁移的最后一道门：当前 20 个 Deployment 都已改挂 Secret、控制器模板与存活 Pod
# 均不再引用旧 ConfigMap 后才删除。历史零副本 ReplicaSet 刻意不纳入检查；删除后迁移前 revision
# 不再可直接 rollout undo，故本次切换是明确的配置载体回退边界。
function Remove-LegacyPandoraConfigMapAfterRollout([string]$KubeContext) {
    $oldName = @(& kubectl --context $KubeContext get configmap/pandora-config -n $K8sNamespace --ignore-not-found -o name 2>$null)
    if ($LASTEXITCODE -ne 0) { throw "查询旧 pandora-config ConfigMap 失败(exit=$LASTEXITCODE)" }
    if ([string]::IsNullOrWhiteSpace((($oldName | ForEach-Object { $_.ToString() }) -join '').Trim())) {
        Write-Skip '旧 pandora-config ConfigMap 不存在,无需迁移清理。'
        return
    }

    $deployments = Get-KubectlJsonObject -KubeContext $KubeContext `
        -Arguments @('get', 'deployments', '-n', $K8sNamespace, '-o', 'json') `
        -Action '查询 Deployment 配置卷'
    $deploymentItems = @($deployments.items)
    $deploymentByName = @{}
    foreach ($deployment in $deploymentItems) { $deploymentByName[[string]$deployment.metadata.name] = $deployment }

    $missingSecretRefs = @(
        foreach ($svc in (Get-ServiceList)) {
            $name = [string]$svc.Name
            if (-not $deploymentByName.ContainsKey($name)) { "$name(Deployment 不存在)"; continue }
            $volumes = @(Get-WorkloadTemplateVolumes $deploymentByName[$name])
            $secretRefs = @($volumes | Where-Object {
                    $null -ne $_.secret -and [string]$_.secret.secretName -ceq 'pandora-config'
                })
            if ($secretRefs.Count -eq 0) { "$name(未引用 Secret/pandora-config)" }
        }
    )
    if ($missingSecretRefs.Count -ne 0) {
        throw "旧 ConfigMap 不可删除:当前业务 Deployment 尚未全部迁移到 Secret:$($missingSecretRefs -join ', ')"
    }

    $controllerRefs = @(Get-LegacyPandoraConfigMapRefs -Items $deploymentItems)
    $otherControllers = Get-KubectlJsonObject -KubeContext $KubeContext `
        -Arguments @('get', 'statefulsets,daemonsets,replicationcontrollers,jobs,cronjobs', '-n', $K8sNamespace, '-o', 'json') `
        -Action '查询其它控制器配置卷'
    $controllerRefs += @(Get-LegacyPandoraConfigMapRefs -Items @($otherControllers.items))
    $replicaSets = Get-KubectlJsonObject -KubeContext $KubeContext `
        -Arguments @('get', 'replicasets', '-n', $K8sNamespace, '-o', 'json') `
        -Action '查询 ReplicaSet 配置引用'
    foreach ($rs in @($replicaSets.items)) {
        if ($null -eq $rs -or -not (Test-HasLegacyPandoraConfigMapRef $rs.spec)) { continue }
        $deploymentOwners = @($rs.metadata.ownerReferences | Where-Object {
                [string]$_.kind -ceq 'Deployment' -and ($_.controller -eq $true)
            })
        $specReplicas = if ($null -eq $rs.spec.replicas) { 0 } else { [int]$rs.spec.replicas }
        $statusReplicas = if ($null -eq $rs.status.replicas) { 0 } else { [int]$rs.status.replicas }
        # 只豁免 Deployment 管理且 desired/status 均为 0 的历史 revision；独立或仍活跃 RS 必须阻断。
        if ($deploymentOwners.Count -eq 0 -or $specReplicas -ne 0 -or $statusReplicas -ne 0) {
            $controllerRefs += "ReplicaSet/$($rs.metadata.name)"
        }
    }
    if ($controllerRefs.Count -ne 0) {
        throw "旧 ConfigMap 不可删除:仍有控制器模板引用 pandora-config:$($controllerRefs -join ', ')"
    }

    # rollout status 返回时旧 Pod 可能仍短暂 Terminating；最多等 60s，查询失败或超时均 fail-closed。
    $deadline = (Get-Date).AddSeconds(60)
    do {
        $pods = Get-KubectlJsonObject -KubeContext $KubeContext `
            -Arguments @('get', 'pods', '-n', $K8sNamespace, '-o', 'json') `
            -Action '查询存活 Pod 配置卷'
        $livePods = @($pods.items | Where-Object { [string]$_.status.phase -notin @('Succeeded', 'Failed') })
        $podRefs = @(Get-LegacyPandoraConfigMapRefs -Items $livePods)
        if ($podRefs.Count -eq 0) { break }
        if ((Get-Date) -ge $deadline) {
            throw "旧 ConfigMap 不可删除:60s 后仍有存活 Pod 引用 pandora-config:$($podRefs -join ', ')"
        }
        Start-Sleep -Seconds 2
    } while ($true)

    kubectl --context $KubeContext delete configmap pandora-config -n $K8sNamespace --ignore-not-found
    Assert-LastExit '删除旧 pandora-config ConfigMap'
    $remaining = @(& kubectl --context $KubeContext get configmap/pandora-config -n $K8sNamespace --ignore-not-found -o name 2>$null)
    if ($LASTEXITCODE -ne 0) { throw "复查旧 pandora-config ConfigMap 失败(exit=$LASTEXITCODE)" }
    if (-not [string]::IsNullOrWhiteSpace((($remaining | ForEach-Object { $_.ToString() }) -join '').Trim())) {
        throw '旧 pandora-config ConfigMap 删除后仍存在。'
    }
    Write-Ok '20 个业务 Deployment 与存活 Pod 已迁移到 Secret;旧 pandora-config ConfigMap 已删除。'
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
    return (Ensure-Tool -Name 'Go' -CheckCmd 'go' -WingetId 'GoLang.Go' -ManualUrl 'https://go.dev/dl/ (需 1.26.5+)')
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
            # 含战斗混合版:18 业务服务跑 docker,只有 ds/hub allocator 跑宿主(需 Go build 这 2 个 + exec Windows DS)。
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
    Write-Step "local 模式:基础设施(docker) + 20 个 go 服务(宿主进程)"
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
    Write-Step "docker 模式:基础设施 + 20 个 go 服务全部容器化"

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
# 与 docker 一致(基础设施 + 20 服务全容器,DS=mock),区别只是面向局域网:
#   - 导出 PANDORA_EDGE_BIND_HOST=0.0.0.0,只让 Envoy 客户端面 8443 对局域网开放
#   - DS 面 8444 未鉴权,默认仍固定在 127.0.0.1(admin 9901 也只绑本机)
#   - 内网其它机器可直接连本机内网 IP
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

# 设置 Envoy 边缘绑定:客户端面(8443)按 $ClientHost;DS 面(8444,未鉴权)默认恒绑本机,
# 只有显式 -ExposeDsFace 才跟随客户端面开到局域网,避免未鉴权 DS 面默认暴露给整个局域网。
function Set-EdgeBindHost([string]$ClientHost) {
    $env:PANDORA_EDGE_BIND_HOST = $ClientHost
    if ($ExposeDsFace) {
        $env:PANDORA_DS_EDGE_BIND_HOST = $ClientHost
        if ($ClientHost -eq '0.0.0.0') {
            Write-Warn "已按 -ExposeDsFace 把未鉴权的 DS 面 :8444 也开到局域网($ClientHost);仅在真有异机 DS 需回连时使用,并确保网络隔离/防火墙收敛。"
        }
    } else {
        $env:PANDORA_DS_EDGE_BIND_HOST = '127.0.0.1'
        if ($ClientHost -eq '0.0.0.0') {
            Write-Info "DS 面 :8444(未鉴权)保持只绑本机;异机 DS 需回连时加 -ExposeDsFace 显式开放。"
        }
    }
}

function Invoke-Intranet {
    if ($Down) { Invoke-Docker; return }

    $lan = if (-not [string]::IsNullOrWhiteSpace($AdvertiseHost)) { $AdvertiseHost } else { Resolve-LanIp }
    Write-Step "intranet 模式:内网测试服(全容器,内网 IP = $lan)"
    if ([string]::IsNullOrWhiteSpace($lan)) {
        Write-Warn "未能自动解析内网 IPv4,可用 -AdvertiseHost 显式指定。继续以 docker 全容器方式启动。"
    }

    # 内网测试服面向局域网:让 Envoy【客户端面 8443】绑 0.0.0.0(admin 9901 仍恒绑本机)。
    # DS 面 8444 未鉴权且 intranet=mock DS 根本用不到,恒绑本机(除非 -ExposeDsFace)。
    # 该环境变量会被 dev_up.ps1 的 docker compose 继承(进程环境优先级高于 --env-file)。
    Set-EdgeBindHost '0.0.0.0'

    # 复用 docker 全容器启动路径(基础设施 + 服务容器,allocator=mock)
    Invoke-Docker

    Write-Host ""
    Write-Ok "内网测试服已启动。其它机器把客户端后端指向:"
    if (-not [string]::IsNullOrWhiteSpace($lan)) {
        Write-Host "       客户端面(TLS)  https://${lan}:8443" -ForegroundColor Green
        if ($ExposeDsFace) {
            Write-Host "       DS 面          ${lan}:8444(-ExposeDsFace 已开放)" -ForegroundColor Green
        } else {
            Write-Host "       DS 面          127.0.0.1:8444(未鉴权,默认仅本机;intranet=mock DS 无需开放)" -ForegroundColor DarkGray
        }
    }
    Write-Warn "DS=mock(无真实 DS);需真实战斗/大厅 DS 请用 -Mode online(Agones)。"
}

# ===== battle 模式(含战斗混合版:18 业务服务跑 docker + ds/hub allocator 跑宿主)=====
# 为什么混合:战斗/大厅 DS 是 Windows PandoraServer.exe,跑不进 Linux 容器;
# ds_allocator(mode=local)与 hub_allocator 要在本机 exec 这个 .exe,故这 2 个服务必须留宿主,
# 其余 18 个纯业务服务进 docker(策划服务器机器不用为它们装 Go / 逐个 build)。
# 网络:Envoy(容器)所有上游本就走 host.docker.internal:500XX,18 个容器把端口发布到宿主、
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

    Write-Step "battle 模式:18 个业务服务(docker) + ds/hub allocator(宿主,exec Windows DS)"

    # 清理可能冲突的残留:上一轮 local 的宿主 go 进程、上一轮 docker 的 allocator 容器(会抢 50020/50021)。
    Write-Info "先停掉可能残留的宿主 go 服务与业务容器(避免端口冲突)..."
    & "$ScriptDir/run_services.ps1" -Action down 2>$null
    docker compose -f $ComposeServices down 2>$null | Out-Null

    # Envoy 客户端面:仅当 DS advertise 指向局域网(非回环)时才开放 8443;
    # DS 面 8444 默认仍只绑本机。admin 9901 恒绑本机。
    $battleAdv =
        if (-not [string]::IsNullOrWhiteSpace($AdvertiseHost)) { $AdvertiseHost.Trim() }
        elseif (-not [string]::IsNullOrWhiteSpace($env:PANDORA_DS_ADVERTISE_HOST)) { $env:PANDORA_DS_ADVERTISE_HOST.Trim() }
        else { '' }
    # battle 模式真 DS 在本机 exec(DS→Envoy 走 127.0.0.1),故 DS 面 8444 无需局域网开放;
    # 仅客户端面 8443 按 advertise 是否局域网决定。DS 面默认本机(除非 -ExposeDsFace)。
    $battleClientBind = if (-not [string]::IsNullOrWhiteSpace($battleAdv) -and $battleAdv -ne '127.0.0.1') { '0.0.0.0' } else { '127.0.0.1' }
    Set-EdgeBindHost $battleClientBind

    Write-Step "[1/5] 基础设施(建 pandora-net)"
    & "$ScriptDir/dev_up.ps1"
    if ($LASTEXITCODE -ne 0) { throw "基础设施启动失败" }

    Write-Step "[2/5] 生成集群版配置(allocator 跑宿主:matchmaker/login/battle_result 经 host.docker.internal 回连)"
    & "$ScriptDir/gen_cluster_config.ps1" -HostAllocators

    Write-Step "[3/5] 构建 18 个业务服务镜像(不含 ds/hub allocator)"
    Build-AllImages -Only $containerSvcs

    Write-Step "[4/5] 启动 18 个业务服务容器"
    docker compose -f $ComposeServices up -d @containerSvcs
    if ($LASTEXITCODE -ne 0) { throw "业务服务容器启动失败" }

    Write-Step "[5/5] 宿主启动 ds_allocator + hub_allocator(mode=local,匹配成局 exec Windows DS)"
    # run_services 用下划线服务名;mode=local 从 etc/*-dev.yaml 读,DS 广告地址走 play.ps1 注入的环境变量。
    & "$ScriptDir/run_services.ps1" -Only ds_allocator, hub_allocator
    if ($LASTEXITCODE -ne 0) { throw "宿主 allocator 启动失败" }

    Write-Host ""
    Write-Ok "battle 模式已启动:18 业务容器 + 2 宿主 allocator。查看:pwsh tools/scripts/start.ps1 -Mode battle -Status"
}


# ===== 共享:apply Agones(RBAC + Fleet),可选安装 Agones(minikube 本地用)=====
# 让 agones 链路端到端可用:RBAC 给 allocator in-cluster token 调 Agones API 的权限,
# Stable/Canary 四 Fleet(pandora-battle-* / pandora-hub-*)提供真实 Linux DS。namespace 须先存在(调用方保证)。
function Apply-AgonesManifests {
    param(
        [switch]$InstallAgones,
        [string]$BattleDsImage = '',
        [string]$HubDsImage = '',
        [string]$CanaryBattleDsImage = '',
        [string]$CanaryHubDsImage = '',
        [ValidateRange(0, 1000)][int]$BattleCanaryReplicaCount = 0,
        [ValidateRange(0, 1000)][int]$HubCanaryReplicaCount = 0,
        # Battle FleetAutoscaler 覆写(online 用):0/空 = 用 yaml 里的本地 dev 默认值。
        [ValidateRange(0, 1000000)][int]$BattleMaxReplicasOverride = 0,
        [ValidatePattern('^([0-9]+%?)?$')][string]$BattleBufferSizeOverride = '',
        [ValidateRange(1, 2147483647)][int]$DSTicketKeysetRevision = 1,
        [string]$DsGatewayAddr = '',
        [string]$DsGatewayTls = '',
        # 本地 minikube dev 专用:Fleet 用固定 :dev tag,kubectl apply 报 unchanged,
        # Agones 不会滚动替换已在跑的 GameServer,导致重 build 新 DS 镜像后旧 Pod 仍跑旧镜像。
        # 开启后:apply 完 Fleet 再删掉现存 GameServer,让 Agones 按 :dev(已指向新镜像)重建。
        # 线上(online)每次是不同 tag,滚动更新自动生效,禁开此开关(强删会中断线上战斗)。
        [switch]$ForceRecreateGameServers,
        # 本地 minikube kube-context 名。ForceRecreateGameServers 删 GameServer 时,kubectl 必须
        # 显式 --context 钉在本机 minikube 上,绝不能落到用户 current-context 可能指向的远端集群。
        [string]$KubeContext = ''
    )
    $agonesDir = Join-Path $ProjectRoot 'deploy/k8s/agones'
    if ([string]::IsNullOrWhiteSpace($KubeContext)) {
        throw 'Apply-AgonesManifests 必须显式传 -KubeContext，禁止依赖可被其它终端切换的 current-context。'
    }
    $kubectlContextArgs = @('--context', $KubeContext)

    function Set-YamlEnvValue([string]$text, [string]$name, [string]$value) {
        # 用显式 ${1}/${2} 分组语法:避免 value 以数字开头时(如 TLS=1)$1+值拼成 $11 被当作第 11 组
        $pattern = '(?ms)(- name:\s*' + [regex]::Escape($name) + '\r?\n\s*value:\s*")[^"]+(")'
        return [regex]::Replace($text, $pattern, ('${1}' + $value + '${2}'))
    }

    function Set-FleetReplicaCount([string]$text, [int]$count) {
        $pattern = '(?m)^(spec:\r?\n\s{2}replicas:\s*)[0-9]+\s*$'
        if ([regex]::Matches($text, $pattern).Count -ne 1) { throw 'Fleet spec.replicas 字段数量不是 1。' }
        return [regex]::Replace($text, $pattern, ('${1}' + $count), 1)
    }

    function Apply-FleetManifest(
        [string]$fileName,
        [string]$image,
        [string]$track,
        [int]$replicaOverride,
        [string[]]$addrEnvNames,
        [string]$tlsEnvName) {
        $src = Join-Path $agonesDir $fileName
        $baseRaw = Get-Content $src -Raw
        Assert-PandoraFleetNoPlayerSigningMaterial -Manifest $baseRaw
        if ([string]::IsNullOrWhiteSpace($image) -and [string]::IsNullOrWhiteSpace($DsGatewayAddr) -and
            [string]::IsNullOrWhiteSpace($DsGatewayTls) -and $DSTicketKeysetRevision -eq 1 -and $replicaOverride -lt 0) {
            kubectl @kubectlContextArgs apply -f $src
            Assert-LastExit "kubectl apply Fleet $fileName"
            return
        }
        $raw = $baseRaw
        if (-not [string]::IsNullOrWhiteSpace($image)) {
            # online 只接受 registry digest pin；同时把 digest 写进 GameServer/Pod 两层 annotation。
            # 本地 minikube 不传 image，继续使用 base 的 :dev，不触发本分支。
            $raw = Set-PandoraFleetImagePin -Manifest $raw -PinnedImage $image
        }
        if (-not [string]::IsNullOrWhiteSpace($DsGatewayAddr)) {
            foreach ($envName in $addrEnvNames) {
                $raw = Set-YamlEnvValue $raw $envName $DsGatewayAddr
            }
        }
        if (-not [string]::IsNullOrWhiteSpace($DsGatewayTls)) {
            $raw = Set-YamlEnvValue $raw $tlsEnvName $DsGatewayTls
        }
        $raw = Set-PandoraFleetDSTicketKeysetRevision -Manifest $raw -Revision $DSTicketKeysetRevision
        if ($replicaOverride -ge 0) { $raw = Set-FleetReplicaCount $raw $replicaOverride }
        if (-not [string]::IsNullOrWhiteSpace($image)) {
            $containerName = if ($fileName -match 'battle') { 'pandora-battle-ds' } elseif ($fileName -match 'hub') { 'pandora-hub-ds' } else { throw "未知 Fleet 清单:$fileName" }
            $expectedFleetName = if ($containerName -ceq 'pandora-battle-ds') { "pandora-battle-$track" } else { "pandora-hub-$track" }
            $contractRows = Get-PandoraFleetContractRows -Manifest $raw
            Assert-PandoraFleetManifestContract -Manifest $raw -ContractRows $contractRows -PinnedImage $image `
                -ContainerName $containerName -ExpectedTrack $track -ExpectedFleetName $expectedFleetName
        }

        $tmp = Join-Path ([System.IO.Path]::GetTempPath()) ([System.IO.Path]::GetRandomFileName() + '-' + $fileName)
        [System.IO.File]::WriteAllText($tmp, $raw, (New-Object System.Text.UTF8Encoding($false)))
        try {
            kubectl @kubectlContextArgs apply -f $tmp
            Assert-LastExit "kubectl apply Fleet $fileName"
        } finally {
            Remove-Item $tmp -ErrorAction SilentlyContinue
        }
    }

    if ($InstallAgones) {
        kubectl @kubectlContextArgs get ns agones-system *> $null
        if ($LASTEXITCODE -ne 0) {
            Write-Info "安装 Agones(helm,装到 agones-system)..."
            helm repo add agones https://agones.dev/chart/stable 2>$null | Out-Null
            helm repo update 2>$null | Out-Null
            kubectl @kubectlContextArgs create namespace agones-system 2>$null | Out-Null
            helm install agones agones/agones --kube-context $KubeContext --namespace agones-system --wait
            if ($LASTEXITCODE -ne 0) { throw "Agones 安装失败" }
        } else {
            Write-Ok "Agones 已安装(agones-system 存在)"
        }
    }

    Write-Info "apply Agones RBAC(pandora-allocator)..."
    kubectl @kubectlContextArgs apply -f (Join-Path $agonesDir '10-rbac-allocator.yaml')
    Assert-LastExit 'kubectl apply Agones RBAC'

    # ===== 本地 minikube:部署 in-cluster Envoy「DS 面」网关(线上同款 pandora-envoy.pandora.svc:8444)=====
    # 线上真集群自带边缘 Envoy;本地 minikube 无边缘 Envoy 且 pod 解析不了 host.docker.internal，故集群内起等价 Envoy。
    # 只在本地(InstallAgones)部署;online 模式不带 -InstallAgones，用线上真 Envoy。
    if ($InstallAgones) {
        # 三级来源确保 Envoy 镜像进节点:节点已有 → 宿主 load → 联网 pull(断网友好,失败 fail-fast)。
        Ensure-EnvoyImageInMinikube -MinikubeProfile (Get-K8sManagedProfile)
        Write-Info "apply in-cluster Envoy(DS 面 :8444,上游=集群内 Service DNS)..."
        kubectl @kubectlContextArgs apply -f (Join-Path $agonesDir '16-ds-envoy.yaml')
        Assert-LastExit 'kubectl apply in-cluster Envoy'
        # ⚠️ Envoy 只在进程启动时读一次静态 envoy.yaml。仅改 ConfigMap(路由白名单 / 上游等)时,
        #    Deployment pod spec 不变 → kubectl apply 报 unchanged → 运行中的 Envoy 仍跑旧配置,
        #    挂载的 ConfigMap 卷即便刷新到盘上 Envoy 也不会热读(回应审核 P#4:ConfigMap 变更不触发重载)。
        #    故显式 rollout restart 让 Pod 重启重读新配置;首次部署时 restart 等价一次滚动,无害。
        kubectl @kubectlContextArgs rollout restart deploy/pandora-envoy -n pandora
        Assert-LastExit 'rollout restart pandora-envoy(强制重载 DS 面静态配置)'
        kubectl @kubectlContextArgs rollout status deploy/pandora-envoy -n pandora --timeout=120s
        if ($LASTEXITCODE -ne 0) { throw "pandora-envoy 未在 120s 内就绪;DS 心跳(:8444)打不通,已中止(排障:kubectl -n pandora describe deploy/pandora-envoy)。" }
    }

    Write-Info "apply Stable/Canary Fleet(pandora-battle-*/pandora-hub-* 真 Linux DS)..."
    Apply-FleetManifest '20-fleet-battle.yaml' $BattleDsImage 'stable' -1 @('PANDORA_DS_ALLOCATOR_ADDR', 'PANDORA_DS_ADMISSION_ADDR', 'PANDORA_PLAYER_LOCATOR_ADDR', 'PANDORA_BATTLE_RESULT_ADDR') 'PANDORA_DS_ALLOCATOR_TLS'
    Apply-FleetManifest '21-fleet-battle-canary.yaml' $CanaryBattleDsImage 'canary' $BattleCanaryReplicaCount @('PANDORA_DS_ALLOCATOR_ADDR', 'PANDORA_DS_ADMISSION_ADDR', 'PANDORA_PLAYER_LOCATOR_ADDR', 'PANDORA_BATTLE_RESULT_ADDR') 'PANDORA_DS_ALLOCATOR_TLS'
    Apply-FleetManifest '30-fleet-hub.yaml' $HubDsImage 'stable' -1 @('PANDORA_HUB_ALLOCATOR_ADDR', 'PANDORA_DS_ADMISSION_ADDR', 'PANDORA_PLAYER_LOCATOR_ADDR') 'PANDORA_DS_ALLOCATOR_TLS'
    Apply-FleetManifest '31-fleet-hub-canary.yaml' $CanaryHubDsImage 'canary' $HubCanaryReplicaCount @('PANDORA_HUB_ALLOCATOR_ADDR', 'PANDORA_DS_ADMISSION_ADDR', 'PANDORA_PLAYER_LOCATOR_ADDR') 'PANDORA_DS_ALLOCATOR_TLS'
    # Battle Stable 轨 FleetAutoscaler(Buffer 策略):维持常备 Ready 预热,分配走一个自动补一个,
    # 同时最大局数 = maxReplicas(见 25-fleetautoscaler-battle.yaml 注释)。Canary/Hub 不配,原因同文件注释。
    # online 可用 -BattleMaxReplicas/-BattleBufferSize 覆写(临时文件改写再 apply,仓库原文不脏)。
    Write-Info "apply Battle FleetAutoscaler(Buffer 策略,上限见 25-fleetautoscaler-battle.yaml)..."
    $autoscalerSrc = Join-Path $agonesDir '25-fleetautoscaler-battle.yaml'
    if ($BattleMaxReplicasOverride -gt 0 -or -not [string]::IsNullOrWhiteSpace($BattleBufferSizeOverride)) {
        $autoscalerRaw = Get-Content $autoscalerSrc -Raw
        if ($BattleMaxReplicasOverride -gt 0) {
            $maxPattern = '(?m)^(\s*maxReplicas:\s*)[0-9]+\s*$'
            if ([regex]::Matches($autoscalerRaw, $maxPattern).Count -ne 1) { throw 'FleetAutoscaler maxReplicas 字段数量不是 1。' }
            $autoscalerRaw = [regex]::Replace($autoscalerRaw, $maxPattern, ('${1}' + $BattleMaxReplicasOverride), 1)
        }
        if (-not [string]::IsNullOrWhiteSpace($BattleBufferSizeOverride)) {
            # 百分比必须带引号(yaml 字符串);纯整数裸写。
            $bufValue = if ($BattleBufferSizeOverride -like '*%') { '"' + $BattleBufferSizeOverride + '"' } else { $BattleBufferSizeOverride }
            $bufPattern = '(?m)^(\s*bufferSize:\s*)"?[0-9]+%?"?\s*$'
            if ([regex]::Matches($autoscalerRaw, $bufPattern).Count -ne 1) { throw 'FleetAutoscaler bufferSize 字段数量不是 1。' }
            $autoscalerRaw = [regex]::Replace($autoscalerRaw, $bufPattern, ('${1}' + $bufValue), 1)
        }
        $autoscalerTmp = Join-Path ([System.IO.Path]::GetTempPath()) ([System.IO.Path]::GetRandomFileName() + '-25-fleetautoscaler-battle.yaml')
        [System.IO.File]::WriteAllText($autoscalerTmp, $autoscalerRaw, (New-Object System.Text.UTF8Encoding($false)))
        try {
            kubectl @kubectlContextArgs apply -f $autoscalerTmp
            Assert-LastExit 'kubectl apply Battle FleetAutoscaler(override)'
        } finally {
            Remove-Item $autoscalerTmp -ErrorAction SilentlyContinue
        }
    } else {
        kubectl @kubectlContextArgs apply -f $autoscalerSrc
        Assert-LastExit 'kubectl apply Battle FleetAutoscaler'
    }
    Write-Warn "Fleet 用真 UE DS 镜像(pandora/battle-ds:dev / pandora/hub-ds:dev)。"
    Write-Warn "  这些镜像由 UE 侧 Tool/Server/Agones 构建;minikube 需先 minikube image load,线上需 push 到 -Registry。"

    if ($ForceRecreateGameServers) {
        # 固定 :dev tag 场景(本地 minikube dev):Fleet spec 未变 -> kubectl apply 报 unchanged,
        # Agones 不会滚动替换已在跑的 GameServer,旧 DS Pod 会继续跑旧镜像。删掉这两个 Fleet
        # 现存的 GameServer,Agones GameServerSet 会按当前 spec(:dev 已指向刚 build 的新镜像)
        # 自动补齐 replicas,新 Pod 以 imagePullPolicy=IfNotPresent 现场解析 = 最新 DS 二进制。
        # Fleet/GameServer 都在 default namespace(见 20/30-fleet yaml metadata.namespace)。
        # 安全:删 GameServer 是不可逆动作,必须显式 --context 钉在本机 minikube,绝不落远端集群。
        if ([string]::IsNullOrWhiteSpace($KubeContext)) {
            throw "ForceRecreateGameServers 需要显式 -KubeContext(本机 minikube context),缺失即中止,防止误删远端集群 GameServer。"
        }
        $fleetNs = 'default'
        Write-Info "强制重建 GameServer(context=$KubeContext,让 :dev tag 的新 DS 镜像生效)..."
        foreach ($fleet in @('pandora-battle-stable', 'pandora-battle-canary', 'pandora-hub-stable', 'pandora-hub-canary')) {
            kubectl @kubectlContextArgs delete gameservers -l "agones.dev/fleet=$fleet" -n $fleetNs --ignore-not-found
            if ($LASTEXITCODE -ne 0) {
                # 删除失败 = 旧镜像 Pod 可能仍在跑;绝不能静默继续,否则会把「旧实例还在跑」
                # 误判成「新镜像已部署成功」。直接 fail-fast,让调用方看到并处理。
                throw "删除 Fleet『$fleet』的 GameServer 失败(exit=$LASTEXITCODE);旧 DS 镜像 Pod 可能仍在跑,已中止以免误判为新镜像部署成功。请检查:kubectl --context $KubeContext get gameservers -l agones.dev/fleet=$fleet -n $fleetNs,排障后重跑。"
            }
        }
        Write-Ok "已删除旧 GameServer,Agones 将用最新 :dev 镜像重建(kubectl get gameservers 观察 Ready)。"
    }
}

# ===== 共享:宿主镜像同步进 minikube + 固定 tag 强制刷新 =====
# 背景(2026-07-07 实锤 bug):本地 dev 固定复用 :dev tag,`minikube image load` 在 minikube
# 侧已有同名 tag 时可能静默不生效(残留旧镜像),导致「宿主 docker 已是新 build,k8s Pod 却
# 还在跑旧 image」—— 例:ds-allocator 新版“等 DS ready 心跳才回地址”逻辑没生效,matchmaker
# 提前把地址发给客户端,客户端 UDP 握手 20s 超时被踢回登录。
# Docker buildx / desktop-linux 下宿主 .Id 可能是 manifest-list digest,而 minikube 节点内
# 看到的是镜像 config digest,二者不能直接相等比较。当前策略:先强制解绑旧 tag,再从宿主
# docker daemon 覆盖 load,最后确认 tag 存在;随后 rollout restart 让新 Pod 重新解析该 tag。

# ===== 共享:minikube profile / kube-context 解析 + 本地集群上下文锁 =====
# 本地 k8s 模式所有 kubectl 操作(尤其是删 GameServer 这类不可逆动作)必须锁定在本机 minikube 的
# kube-context 上,绝不能落到用户当前 kubectl current-context 指向的远端/生产集群。
# minikube 的 kube-context 名 == 其 profile 名(minikube start 会写入同名 context)。

# 返回当前 active minikube profile 名(解析失败回落 'minikube')。
function Get-ActiveMinikubeProfile {
    $p = ((& minikube profile 2>$null | Select-Object -First 1) -replace '^\*\s*', '').Trim()
    if ([string]::IsNullOrWhiteSpace($p)) { $p = 'minikube' }
    return $p
}

# 本次运行内固定的 minikube profile(解析一次后缓存)。
# 所有 minikube start / status / delete / image load 都钉在同一个 profile 上,杜绝
# 「Reset 删了 profile A,却用默认 'minikube' 重建成 B」的 profile 漂移 —— delete 目标与
# rebuild 目标必须始终一致(否则重置后跑的是另一个集群,旧集群还残留占资源)。
$script:K8sManagedProfileResolved = $false
$script:K8sManagedProfile = $null
function Get-K8sManagedProfile {
    if (-not $script:K8sManagedProfileResolved) {
        $script:K8sManagedProfile = Get-ActiveMinikubeProfile
        $script:K8sManagedProfileResolved = $true
    }
    return $script:K8sManagedProfile
}

# 校验 active minikube profile 对应的 kube-context 确实存在于 kubeconfig,返回该 context 名。
# 用于把本地 k8s 操作显式钉在 minikube 上(kubectl --context <ctx>),不依赖易被用户切走的
# current-context。context 不存在(minikube 没起/被删)时 fail-fast,绝不静默落到别的集群。
function Resolve-MinikubeKubeContext {
    $mkProfile = Get-K8sManagedProfile
    $contexts = @(kubectl config get-contexts -o name 2>$null)
    if ($LASTEXITCODE -ne 0) { throw "kubectl 不可用或 kubeconfig 读取失败,无法解析 minikube kube-context。" }
    if ($contexts -cnotcontains $mkProfile) {
        throw "kubeconfig 中找不到 minikube 的 kube-context『$mkProfile』(minikube 未启动或未创建集群?)。为防误操作远端集群,已中止。"
    }
    return $mkProfile
}

# 校验指定 kube-context 的 apiserver endpoint 确实指向本机 minikube(而不是同名的远端/生产集群)。
# 只比对 context 名不够安全:别人 kubeconfig 里完全可能存在同名 context 却指向线上集群,
# 一旦对它执行 delete 就是灾难。这里取该 context 关联 cluster 的 server URL,主机名必须是
# 回环(127.0.0.1/localhost/::1)或 minikube 节点 IP 才算本地;否则 fail-fast。
# 返回 $true 表示确认本地;不可解析或非本地一律返回 $false(调用方决定 throw/skip)。
function Test-KubeContextIsLocalMinikube([string]$Context) {
    if ([string]::IsNullOrWhiteSpace($Context)) { return $false }
    $cluster = (kubectl config view -o jsonpath="{.contexts[?(@.name==`"$Context`")].context.cluster}" 2>$null)
    if ([string]::IsNullOrWhiteSpace($cluster)) { return $false }
    $server = (kubectl config view -o jsonpath="{.clusters[?(@.name==`"$cluster`")].cluster.server}" 2>$null)
    if ([string]::IsNullOrWhiteSpace($server)) { return $false }
    $apiHost = $null
    try { $apiHost = ([System.Uri]$server).Host } catch { return $false }
    if ([string]::IsNullOrWhiteSpace($apiHost)) { return $false }
    # 回环地址 = 明确本机(minikube docker driver 常见 https://127.0.0.1:<port>)。
    if ($apiHost -in @('127.0.0.1', 'localhost', '::1', '[::1]')) { return $true }
    # 否则必须等于 minikube 节点 IP(docker/kvm driver 下的 192.168.x.x)。
    $mkIp = (& minikube -p $Context ip 2>$null)
    if ($LASTEXITCODE -eq 0 -and -not [string]::IsNullOrWhiteSpace($mkIp) -and $apiHost -eq $mkIp.Trim()) { return $true }
    return $false
}

# 读 minikube 内镜像清单,返回 hashtable:镜像名(去 docker.io/ 前缀) -> 镜像 ID(sha256:config digest)。
# 该 ID 与宿主 `docker image inspect -f '{{.Id}}'` 同为镜像 config digest,可直接比对。
# minikube 版本过旧不支持 --format json 时返回 $null(调用方降级为告警)。
function Get-MinikubeImageIds([string[]]$MinikubeArgs = @()) {
    $json = (minikube @MinikubeArgs image ls --format json 2>$null | Out-String)
    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($json)) { return $null }
    try { $list = $json | ConvertFrom-Json } catch { return $null }
    $map = @{}
    foreach ($entry in $list) {
        foreach ($tag in @($entry.repoTags)) {
            if ([string]::IsNullOrWhiteSpace($tag)) { continue }
            $name = $tag -replace '^docker\.io/', ''
            $map[$name] = [string]$entry.id
        }
    }
    return $map
}

# 把一组宿主镜像 load 进 minikube。
# Docker buildx/desktop-linux 下宿主 .Id 可能是 manifest-list digest,而 minikube docker daemon 里
# 看到的是镜像 config digest,二者不能直接相等比较。这里用「强制解绑旧 tag + 从宿主 daemon 覆盖 load」
# 保证 :dev tag 指向最新构建;随后 rollout restart 会让新 Pod 重新解析该 tag。
function Sync-ImagesToMinikube {
    param(
        [string[]]$Images,
        [string[]]$MinikubeArgs = @()
    )

    # 1) 宿主没有该镜像 → 直接 fail-fast
    foreach ($img in $Images) {
        $id = (docker image inspect -f '{{.Id}}' $img 2>$null | Out-String).Trim()
        if ($LASTEXITCODE -ne 0 -or -not $id) { throw "宿主 docker 缺少镜像 $img(先构建再部署)" }
    }

    # 2) 逐个强制刷新同名 tag。旧 Pod 正在用旧镜像时,minikube image rm 会失败;节点内 docker rmi -f
    # 可以只 untag,不影响运行中容器,再 load 最新 tag。
    foreach ($img in $Images) {
        Write-Info "  minikube image load --daemon=true --overwrite=true $img"
        minikube @MinikubeArgs ssh -- docker rmi -f $img 2>$null | Out-Null
        minikube @MinikubeArgs image load --daemon=true --overwrite=true $img
        Assert-LastExit "minikube image load $img"
    }

    # 3) 校验 minikube 内 tag 存在。新 Pod 是否换新镜像由后续 rollout restart + rollout status 兜住。
    $mkIds = Get-MinikubeImageIds $MinikubeArgs
    if ($null -eq $mkIds) {
        Write-Warn "minikube image ls --format json 不可用,无法校验 tag(建议升级 minikube)。"
        return
    }
    $missing = @($Images | Where-Object { -not $mkIds.ContainsKey($_) })
    if ($missing.Count -gt 0) {
        throw "以下镜像 load 后 minikube 内仍未找到 tag:$($missing -join ', ')"
    }
    Write-Ok "镜像 tag 已刷新到 minikube($($Images.Count) 个)。"
}

# ===== 共享:确保 in-cluster Envoy 镜像在 minikube 节点内(离线友好,三级来源)=====
# 顺序(2026-07-10,回应审核 P1「离线只会在 minikube 节点联网 pull,不复用宿主已有镜像」):
#   1) minikube 节点已有 → 直接用(cached);
#   2) 宿主 docker 已有 → minikube image load 灌进去(断网机 import_images -IncludeInfra 后走这条);
#   3) 都没有 → 才在节点内联网 pull(重试 6 次),仍失败 fail-fast 给出离线导入指引。
function Ensure-EnvoyImageInMinikube {
    param([string]$MinikubeProfile)
    $envoyImg = 'envoyproxy/envoy:v1.38-latest'
    $mkArgs = @()
    if (-not [string]::IsNullOrWhiteSpace($MinikubeProfile)) { $mkArgs = @('-p', $MinikubeProfile) }

    minikube @mkArgs ssh -- "docker image inspect $envoyImg >/dev/null 2>&1" 2>$null | Out-Null
    if ($LASTEXITCODE -eq 0) {
        Write-Ok "in-cluster Envoy 镜像已在 minikube 节点($envoyImg)"
        return
    }

    docker image inspect $envoyImg *> $null
    if ($LASTEXITCODE -eq 0) {
        Write-Info "宿主 docker 已有 $envoyImg,直接 load 进 minikube(免联网)..."
        minikube @mkArgs image load --daemon=true $envoyImg
        Assert-LastExit "minikube image load $envoyImg"
    } else {
        Write-Info "宿主与 minikube 均无 $envoyImg,尝试在 minikube 节点联网拉取(断网机会失败)..."
        minikube @mkArgs ssh -- "for i in 1 2 3 4 5 6; do docker pull $envoyImg && break || sleep 4; done" 2>$null | Out-Null
    }

    # 拉取/加载可能静默失败(stderr 被吞)。显式确认镜像已在节点内,否则后面 pandora-envoy Pod
    # 会一直 ImagePullBackOff,DS 心跳(:8444)永远打不通 —— fail-fast 让调用方先解决镜像。
    minikube @mkArgs ssh -- "docker image inspect $envoyImg >/dev/null 2>&1" 2>$null | Out-Null
    if ($LASTEXITCODE -ne 0) {
        throw "in-cluster Envoy 镜像『$envoyImg』未能进入 minikube(断网/限流?)。DS 面 :8444 网关无法就绪,已中止。离线办法:在能联网机器 docker pull $envoyImg && docker save -o envoy.tar $envoyImg,拷到本机 docker load -i envoy.tar 后重跑(本函数会自动从宿主 load 进 minikube)。"
    }
    Write-Ok "in-cluster Envoy 镜像已就绪($envoyImg)"
}

# ===== k8s 模式(本地 minikube)=====
# ===== k8s 模式:把 UE Linux DS 打成镜像并落进 minikube =====
# DS 镜像(pandora/battle-ds:dev / pandora/hub-ds:dev)不是 20 个 go 业务镜像的一部分,
# 由本函数从【同级客户端仓库】的 Linux 打包产物构建。策略(2026-07-09 改为宿主构建 + load):
#   1) 调 build-image-minikube.ps1 -BuildOnHost:自动解析同级客户端仓库
#      <sibling>\Packages\Server_Linux_Development\LinuxServer(不写死路径),robocopy /MIR
#      同步进 deploy/ds/stage/LinuxServer,再在【宿主 docker daemon】docker build。
#   2) Sync-ImagesToMinikube 把宿主镜像 load 进 minikube(rmi+overwrite+校验,复用业务镜像同款安全路径)。
# 为什么不再在 minikube 内置 daemon 里直接 build:内网/断网机 minikube daemon 既没有 ubuntu:22.04
# 基础镜像、也拉不到公网 -> `FROM ubuntu:22.04` 报 connection refused 直接失败;宿主 docker 有
# ubuntu + apt 缓存层,离线只重跑 COPY 层,构建稳过。宿主构建用独立子进程跑,隔离 docker env。
function Build-DsImagesForMinikube {
    $dsBuild = Join-Path $ProjectRoot 'deploy/ds/build-image-minikube.ps1'
    if (-not (Test-Path $dsBuild)) { throw "找不到 DS 镜像构建脚本:$dsBuild" }
    Write-Info "在宿主 docker 构建 Battle/Hub DS 镜像(从同级客户端仓库取 Linux 包,离线友好)..."
    & pwsh -NoProfile -ExecutionPolicy Bypass -File $dsBuild -BuildOnHost
    Assert-LastExit 'DS 镜像构建(build-image-minikube.ps1 -BuildOnHost)'
    # 使用本次运行固定的 profile,避免构建期间 active profile 漂移后把镜像 load 到另一个集群。
    $mkProfile = Get-K8sManagedProfile
    Write-Info "把 DS 镜像 load 进 minikube(profile=$mkProfile,强制刷新 :dev tag)..."
    Sync-ImagesToMinikube -Images @('pandora/battle-ds:dev', 'pandora/hub-ds:dev') -MinikubeArgs @('-p', $mkProfile)
    Write-Ok "DS 镜像已就绪(pandora/battle-ds:dev / pandora/hub-ds:dev 已在 minikube)"
}

# ===== k8s 模式:宿主侧桥接/中继清理(与启动末尾自动跑的 e2e_k8s.ps1 对称)=====
# 启动会在宿主留下三类长驻资源:kubectl port-forward(pid 记录在 run/k8s-envoy-bridge/)、
# docker envoy(:8443/:8444)、pandora-udp-relay 容器。-Down 必须一并清掉,否则残留的
# port-forward/端口监听会让下次启动或其它模式(local/docker/battle)的流量走向不确定。
function Stop-K8sHostBridge {
    Write-Step "清理宿主 Envoy 桥接 + UDP 中继"

    # 1) 杀 bridge 起的 kubectl port-forward(pid 文件 + 进程名双重校验,防误杀)
    $stateDir = Join-Path $ProjectRoot 'run/k8s-envoy-bridge'
    if (Test-Path $stateDir) {
        foreach ($pidFile in @(Get-ChildItem $stateDir -Filter '*.pid' -ErrorAction SilentlyContinue)) {
            $procId = (Get-Content $pidFile.FullName -ErrorAction SilentlyContinue | Select-Object -First 1)
            if ("$procId" -match '^\d+$') {
                $proc = Get-Process -Id ([int]$procId) -ErrorAction SilentlyContinue
                if ($proc -and $proc.ProcessName -eq 'kubectl') {
                    Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
                    Write-Info "  已停 port-forward $($pidFile.BaseName)(PID=$procId)"
                }
            }
            Remove-Item $pidFile.FullName -ErrorAction SilentlyContinue
        }
    }
    # 兜底:pid 文件丢失/陈旧时,按命令行签名扫残留的「kubectl port-forward --namespace pandora」
    $strays = @(Get-CimInstance Win32_Process -Filter "Name='kubectl.exe'" -ErrorAction SilentlyContinue |
        Where-Object { $_.CommandLine -and $_.CommandLine -match 'port-forward' -and $_.CommandLine -match "--namespace\s+$K8sNamespace" })
    foreach ($p in $strays) {
        Stop-Process -Id $p.ProcessId -Force -ErrorAction SilentlyContinue
        Write-Info "  已停残留 port-forward PID=$($p.ProcessId)"
    }

    # 2) 停并删宿主 docker envoy(bridge 用 compose 单起的 envoy,不碰其它基础设施容器)
    docker compose -f $ComposeInfra --env-file $EnvFile rm -sf envoy *> $null
    if ($LASTEXITCODE -eq 0) { Write-Info "  已停 envoy 容器(:8443/:8444)" }
    else { Write-Warn "  envoy 容器停止失败或不存在(可忽略)" }

    # 3) 删 UDP 回程中继容器(e2e_k8s.ps1 起的 dockerized relay)
    docker rm -f pandora-udp-relay *> $null
    if ($LASTEXITCODE -eq 0) { Write-Info "  已删 pandora-udp-relay 容器" }

    Write-Ok "宿主侧桥接/中继已清理"
}

function Invoke-K8s {
    $servicesDir = Join-Path $ProjectRoot 'deploy/k8s/services'
    $infraYaml   = Join-Path $ProjectRoot 'deploy/k8s/infra/infra.yaml'
    $lokiYaml    = Join-Path $ProjectRoot 'deploy/k8s/infra/loki.yaml'
    $mysqlInit   = Join-Path $ProjectRoot 'deploy/mysql-init'

    if ($Down) {
        # 删除是不可逆动作:先确认本机 minikube kube-context 存在,并把所有 delete 显式钉在它上面,
        # 绝不依赖(可能被切到远端/生产的)current-context。
        # 宿主侧桥接(kubectl port-forward / envoy 容器 / UDP relay 容器)跑在本机,与 k8s 集群
        # 是否可删无关——无论 context 在不在都要先清掉,否则残留进程/容器会占端口、误导后续启动。
        Stop-K8sHostBridge
        $mkProfile = Get-K8sManagedProfile
        $contexts = @(kubectl config get-contexts -o name 2>$null)
        if ($contexts -cnotcontains $mkProfile) {
            throw "宿主桥接已清理，但 kubeconfig 中找不到 minikube context『$mkProfile』，无法证明集群侧对象已删除。为避免假报 Down 成功，已中止；请先修复该 profile/context 或确认集群已销毁。"
        }
        # 只校验 context 名不够:别人 kubeconfig 里可能存在同名 context 却指向远端/生产集群。
        # 必须确认该 context 的 apiserver endpoint 是本机 minikube(回环 IP 或 minikube 节点 IP),
        # 否则 fail-fast 且绝不 delete,避免误删同名远端集群或假报清理成功。
        if (-not (Test-KubeContextIsLocalMinikube $mkProfile)) {
            throw "宿主桥接已清理，但 kube-context『$mkProfile』的 endpoint 不是本机 minikube(可能是同名远端/生产集群)。为防误删且避免假报 Down 成功，集群侧删除未执行。"
        }
        Write-Step "删除 k8s 业务服务 + 基础设施(context=$mkProfile)"
        # Fleet 是启动时 Apply-AgonesManifests 起的,也要停干净(DS Pod 别留着空跑)。
        # 先删 FleetAutoscaler 再删 Fleet:避免删 Fleet 期间 autoscaler 还在按 buffer 补建 GameServer。
        foreach ($fleetFile in @('25-fleetautoscaler-battle.yaml', '31-fleet-hub-canary.yaml', '30-fleet-hub.yaml', '21-fleet-battle-canary.yaml', '20-fleet-battle.yaml')) {
            kubectl --context $mkProfile delete -f (Join-Path $ProjectRoot "deploy/k8s/agones/$fleetFile") --ignore-not-found 2>$null
            Assert-LastExit "kubectl delete Fleet $fleetFile"
        }
        # Down 是用户明确要求停掉整套本地 DS，因此也清理旧版单轨对象。
        kubectl --context $mkProfile delete fleet/pandora-battle fleet/pandora-hub -n default --ignore-not-found 2>$null
        Assert-LastExit 'kubectl delete legacy local Fleets'
        # in-cluster Envoy「DS 面」网关也是启动时 Apply-AgonesManifests 起的(本地专属),一并清理
        kubectl --context $mkProfile delete -f (Join-Path $ProjectRoot 'deploy/k8s/agones/16-ds-envoy.yaml') --ignore-not-found 2>$null
        Assert-LastExit 'kubectl delete in-cluster Envoy'
        kubectl --context $mkProfile delete -k $servicesDir --ignore-not-found 2>$null
        Assert-LastExit 'kubectl delete k8s services'
        kubectl --context $mkProfile delete -f $lokiYaml --ignore-not-found 2>$null
        Assert-LastExit 'kubectl delete Loki'
        kubectl --context $mkProfile delete -f $infraYaml --ignore-not-found 2>$null
        Assert-LastExit 'kubectl delete k8s infra'
        Write-Info "minikube 仍在运行;彻底关:minikube stop"
        return
    }

    Write-Step "k8s 模式:minikube 本地集群"

    # profile 钉死:本次运行的 minikube 全部操作(status/start,以及 Reset 的 delete)都用同一个 profile,
    # 避免 delete 与 rebuild 目标漂移。
    $mkProfile = Get-K8sManagedProfile
    # 只有 profile 在 start 前完全不存在才属于 fresh cluster。“已有但停止”不是 fresh，
    # 不得在 required epoch 丢失时自动回填 1。
    $profileExistedBeforeStart = Test-MinikubeProfileExists -Profile $mkProfile

    # 1) minikube 起没起
    minikube -p $mkProfile status *> $null
    if ($LASTEXITCODE -ne 0) {
        Write-Info "启动 minikube(profile=$mkProfile,driver=docker)..."
        minikube start -p $mkProfile --driver=docker --cpus=4 --memory=6144
        if ($LASTEXITCODE -ne 0) { throw "minikube 启动失败" }
    } else {
        Write-Ok "minikube 已在运行(profile=$mkProfile)"
    }

    # 上下文锁:本地 k8s 部署会 apply/删除大量对象,必须确认 current-context 就是本机 minikube,
    # 否则会把本地开发部署误发到用户 kubectl 当前指向的远端/生产集群。不匹配直接 fail-fast。
    $mkCtx = Resolve-MinikubeKubeContext
    $curCtx = (kubectl config current-context 2>$null)
    if ($curCtx -cne $mkCtx) {
        throw "当前 kubectl current-context『$curCtx』不是本机 minikube『$mkCtx』。为防止把本地 k8s 部署误发到远端/生产集群,已中止。请先执行:kubectl config use-context $mkCtx 再重跑。"
    }
    # 只比对 context 名不够:别人 kubeconfig 里可能存在同名 context 却指向远端/生产集群。
    # 必须确认该 context 的 apiserver endpoint 是本机 minikube(回环 / minikube 节点 IP),否则中止,
    # 避免后续 kubectl apply 落到同名远端集群。
    if (-not (Test-KubeContextIsLocalMinikube $mkCtx)) {
        throw "kube-context『$mkCtx』的 apiserver endpoint 不是本机 minikube(疑似同名远端/生产集群)。为防把本地部署误发到远端,已中止。"
    }
    Write-Ok "kube-context 已锁定本机 minikube:$mkCtx"
    $kubectlContextArgs = @('--context', $mkCtx)
    Assert-ExistingLocalEtcdPersistence -KubeContext $mkCtx
    Assert-NoLegacyDsFleets -KubeContext $mkCtx -LocalDevelopment
    Assert-NoLegacyDSTicketSignerSecret -KubeContext $mkCtx -LocalDevelopment

    Write-Step "[1/8] namespace"
    kubectl @kubectlContextArgs apply -f (Join-Path $servicesDir '00-namespace.yaml')
    Assert-LastExit 'kubectl apply namespace'

    # DS advertise 地址:默认取本机局域网 IP,让内网其它机器的客户端能连到本机 DS(经容器版 UDP 中继回程)。
    # minikube docker driver 下 Pod IP 局域网不可达,但「advertise=本机局域网 IP + UDP 中继监听 0.0.0.0」
    # 这条回程链路可达:client(内网) -> 本机:<port>(UDP) -[Docker publish 0.0.0.0]-> relay 容器 -> minikube 节点 -> DS。
    # 覆盖优先级:-AdvertiseHost > $env:PANDORA_DS_ADVERTISE_HOST > Resolve-LanIp;都拿不到才回退 127.0.0.1(仅本机自测)。
    $k8sAdvHost =
        if (-not [string]::IsNullOrWhiteSpace($AdvertiseHost)) { $AdvertiseHost.Trim() }
        elseif (-not [string]::IsNullOrWhiteSpace($env:PANDORA_DS_ADVERTISE_HOST)) { $env:PANDORA_DS_ADVERTISE_HOST.Trim() }
        else { Resolve-LanIp }
    if ([string]::IsNullOrWhiteSpace($k8sAdvHost)) {
        $k8sAdvHost = '127.0.0.1'
        Write-Warn "未解析到局域网 IPv4,DS advertise 回退 127.0.0.1(只有本机客户端连得到)。内网多机联调请用 -AdvertiseHost <本机内网IP> 重跑。"
    }
    # advertise 非回环 => UDP 中继必须对局域网开放(publish 到 0.0.0.0),否则其它机器的 UDP 到不了本机中继。
    $script:K8sRelayBindHost = if ($k8sAdvHost -eq '127.0.0.1') { '127.0.0.1' } else { '0.0.0.0' }
    Write-Step "[2/8] 生成集群版配置 + Secret(allocator=agones,DS advertise=$k8sAdvHost)"
    if ($script:K8sRelayBindHost -eq '0.0.0.0') {
        Write-Info "DS advertise=$k8sAdvHost(局域网),UDP 中继将监听 0.0.0.0。"
        Write-Warn "内网多机联调前置:请确认本机防火墙已放行【入站 TCP 8443 + 入站 UDP 7000-8000】,否则其它机器连不进客户端入口/DS。DS 面 8444 固定只绑本机。"
    }
    # 本地 minikube 自测:agones 真 DS 链路但沿用公开 dev 密钥,显式 -AllowDevSecrets 承认(审核 P1:
    # 不再靠 advertise host 推断本地,防生产 IP/DNS 绕过 -Prod)。线上走 online 分支的 -Prod。
    # DSTicket v2(方案 B):先自举 dev 钥料(私钥 Secret + 公钥 ConfigMap,幂等),拿 kid 注入配置生成。
    $dsTicketKid = Ensure-DsTicketDevKeyMaterial -KubeContext $mkCtx
    & "$ScriptDir/gen_cluster_config.ps1" -AllocatorMode agones -AllocatorAdvertiseHost $k8sAdvHost -AllowDevSecrets `
        -DsAuthMode enforce -DsAuthorityMode redis -DsFenceEtcdEndpoints $script:LocalDsFenceEndpoint `
        -DsFenceKeysetRevision $script:LocalDsFenceKeysetRevision -DsTicketActiveKid $dsTicketKid `
        -DsTicketKeysetRevision 1
    Assert-LastExit '生成本地 k8s enforce/redis DSTicket v2 配置'
    # 配置含 HS256 密钥(即便本地 dev 也含 ds_auth secret),用 Secret 而非 ConfigMap 承载(P0:密钥不落明文 ConfigMap)。
    Apply-PandoraConfigSecret -KubeContext $mkCtx -Action 'kubectl apply secret pandora-config'
    kubectl @kubectlContextArgs create configmap pandora-mysql-init --from-file=$mysqlInit -n $K8sNamespace `
        --dry-run=client -o yaml | kubectl @kubectlContextArgs apply -f -
    Assert-LastExit 'kubectl apply configmap pandora-mysql-init'

    Write-Step "[3/8] 基础设施(mysql/redis/zookeeper/kafka/etcd + loki/alloy 日志)"
    kubectl @kubectlContextArgs apply -f $infraYaml
    Assert-LastExit 'kubectl apply infra'
    # 日志采集(Loki + Alloy,infra.md §11.2):非关键路径,apply 失败只告警不阻断启动;
    # 也不等它 rollout(日志栈晚几十秒就绪不影响业务链路)。
    kubectl @kubectlContextArgs apply -f $lokiYaml
    if ($LASTEXITCODE -ne 0) { Write-Warn "loki/alloy 日志栈 apply 失败(不影响业务);可稍后手动 kubectl apply -f deploy/k8s/infra/loki.yaml" }
    Write-Info "等待基础设施就绪(最多 180s)..."
    kubectl @kubectlContextArgs rollout status deploy/mysql     -n $K8sNamespace --timeout=180s; Assert-LastExit 'mysql 就绪'
    kubectl @kubectlContextArgs rollout status deploy/redis     -n $K8sNamespace --timeout=120s; Assert-LastExit 'redis 就绪'
    kubectl @kubectlContextArgs rollout status deploy/etcd      -n $K8sNamespace --timeout=120s; Assert-LastExit 'etcd 就绪'
    # zookeeper / kafka 必须就绪,否则 player/push/battle-result 会因连不上 kafka:9092 CrashLoop
    kubectl @kubectlContextArgs rollout status deploy/zookeeper -n $K8sNamespace --timeout=120s; Assert-LastExit 'zookeeper 就绪'
    kubectl @kubectlContextArgs rollout status deploy/kafka     -n $K8sNamespace --timeout=180s; Assert-LastExit 'kafka 就绪'

    Write-Step '[3.5/8] DS callback auth required_writer_epoch 线性预检 / fresh-only CAS bootstrap'
    Assert-LocalDsAuthBaseline -KubeContext $mkCtx -AllowFreshBootstrap:(-not $profileExistedBeforeStart)

    Write-Step "[4/8] 安装 Agones + apply RBAC/Fleet(真 Linux DS)"
    Build-DsImagesForMinikube
    # -ForceRecreateGameServers:上一步刚把新 DS 镜像 build 进 minikube 的 :dev tag,
    # 但 Fleet spec 不变,kubectl apply 不会换掉已在跑的旧 GameServer Pod。删旧 GameServer
    # 让 Agones 用最新 :dev 重建,保证已运行集群上重跑也能换成最新 DS。
    # -KubeContext:把强删 GameServer 钉在本机 minikube,防误删远端集群。
    Apply-AgonesManifests -InstallAgones -ForceRecreateGameServers -KubeContext $mkCtx

    Write-Step "[5/8] 构建 20 个服务镜像"
    Build-AllImages

    Write-Step "[6/8] 把镜像 load 进 minikube(强制刷新固定 :dev tag)"
    # 与 DS 镜像同样显式钉死本次已校验的本地 profile。不能依赖 minikube 的
    # active profile：它可能与已锁定的 kubectl context 不同，导致新业务镜像被 load
    # 到另一个本地集群，而当前集群随后只重启出旧 :dev 镜像。
    Sync-ImagesToMinikube -Images (Get-ServiceImages) -MinikubeArgs @('-p', $mkProfile)

    Write-Step "[7/8] 部署业务服务"
    kubectl @kubectlContextArgs apply -k $servicesDir
    Assert-LastExit 'kubectl apply -k services'
    # 镜像 tag 固定为 :dev,重建/重 load 后 image 字符串不变 -> apply 报 unchanged,旧 Pod 不会换。
    # 按名强制滚动重启这 20 个业务 Deployment(不碰 infra,避免重启 kafka 又触发依赖服务 CrashLoop),
    # 确保跑的是刚 build 的新二进制。
    Write-Info "rollout restart 业务 Deployment(同 :dev tag 重建后强制换 Pod)..."
    foreach ($svc in (Get-ServiceList)) {
        kubectl @kubectlContextArgs rollout restart deploy/$($svc.Name) -n $K8sNamespace
        Assert-LastExit "rollout restart $($svc.Name)"
    }
    # 等滚动完成:确认新 Pod 真的起来了。imagePullPolicy=IfNotPresent 在 Pod 创建时按 :dev tag
    # 现场解析,上面 Sync-ImagesToMinikube 已保证 minikube 内 :dev == 宿主最新 build,
    # 故 rollout 完成 == 新 Pod 必为新镜像(校验+重启+等待三步合一堵死"旧镜像旧 Pod"链路)。
    Write-Info "等待业务 Deployment 滚动完成(每个最多 180s)..."
    foreach ($svc in (Get-ServiceList)) {
        kubectl @kubectlContextArgs rollout status deploy/$($svc.Name) -n $K8sNamespace --timeout=180s
        Assert-LastExit "rollout status $($svc.Name)(新 Pod 未就绪,查:kubectl describe/logs)"
    }
    Remove-LegacyPandoraConfigMapAfterRollout -KubeContext $mkCtx

    Write-Host ""
    Write-Ok "k8s 模式已部署。查看:kubectl get pods -n $K8sNamespace"

    Write-Step "[8/8] 宿主 Envoy 桥接 + UDP 中继(e2e_k8s.ps1)"
    # 真 DS 闭环剩余的宿主侧步骤(port-forward 桥接 + docker envoy + udp-relay)直接接进一键启动。
    # DS 镜像已由 [4/8] Build-DsImagesForMinikube 直接构建进 minikube(宿主 docker 无该 tag),
    # 故传 -SkipImageLoad 跳过 e2e 的宿主->minikube image load。
    # 用独立子进程跑,与 DS 镜像构建同一隔离理由;profile 使用本次运行固定值。
    $mkProfile = Get-K8sManagedProfile
    $relayBind = if ($script:K8sRelayBindHost) { $script:K8sRelayBindHost } else { '127.0.0.1' }
    # k8s GameServer 经集群内 pandora-envoy 回连,宿主 DS 面永远无需对局域网开放。
    # 显式覆盖父环境遗留值,不能只依赖 compose 默认值。
    $env:PANDORA_DS_EDGE_BIND_HOST = '127.0.0.1'
    & pwsh -NoProfile -ExecutionPolicy Bypass -File (Join-Path $ScriptDir 'e2e_k8s.ps1') -SkipImageLoad -MinikubeProfile $mkProfile -KubeContext $mkCtx -RelayBindHost $relayBind
    Assert-LastExit "宿主桥接/中继(e2e_k8s.ps1);集群本身已部署好,修复后可单独重跑:pwsh tools/scripts/e2e_k8s.ps1 -SkipImageLoad"

    Write-Host ""
    if ($relayBind -eq '0.0.0.0') {
        Write-Ok "真 DS 闭环就绪(局域网):内网其它机器客户端连 ${k8sAdvHost}:8443(TLS)即可登录进 Hub/战斗。"
        Write-Info "若其它机器连不进,先查本机防火墙是否放行 入站 TCP 8443 + 入站 UDP 7000-8000。宿主 DS 面 8444 仅回环可达。"
    } else {
        Write-Ok "真 DS 闭环就绪:客户端连 127.0.0.1:8443(TLS)即可登录进 Hub/战斗(仅本机)。"
    }
}

# ===== online 模式(远端 k8s:-Env test 测试服集群 / prod 生产 kbs 集群)=====
function Invoke-Online {
    $overlay     = Join-Path $ProjectRoot 'deploy/k8s/overlays/online'

    # 安全:Env 与预先配置的 context 一一绑定，不能只信用户临时传入的 -Env。
    # 这样即使人在生产 context 上漏写 -Env prod，也会在任何变更前失败。
    $ctx = (kubectl config current-context 2>$null | Out-String).Trim()
    $expectedCtx = if ($Env -eq 'prod') { $ProdKubeContext } else { $TestKubeContext }
    if ([string]::IsNullOrWhiteSpace($expectedCtx)) {
        $paramName = if ($Env -eq 'prod') { '-ProdKubeContext / PANDORA_K8S_PROD_CONTEXT' } else { '-TestKubeContext / PANDORA_K8S_TEST_CONTEXT' }
        throw "online -Env $Env 必须配置期望 context($paramName)，禁止仅凭 current-context 猜目标集群。"
    }
    $expectedCtx = $expectedCtx.Trim()
    if ($ctx -cne $expectedCtx) {
        throw "online -Env $Env 只允许 kube-context『$expectedCtx』，当前却是『$ctx』，已在变更前中止。"
    }
    if (-not [string]::IsNullOrWhiteSpace($TestKubeContext) -and
        -not [string]::IsNullOrWhiteSpace($ProdKubeContext) -and
        $TestKubeContext.Trim() -ceq $ProdKubeContext.Trim()) {
        throw 'TestKubeContext 与 ProdKubeContext 不得相同，否则环境隔离与生产二次确认可被绕过。'
    }
    # 后续所有命令直接使用映射值，而不是再次依赖可变 current-context。
    $ctx = $expectedCtx
    $kubectlContextArgs = @('--context', $ctx)
    Write-Step "online 模式:-Env $Env  目标 kube-context = $ctx"
    if ($Env -eq 'prod') {
        Write-Warn "⚠️ 这是【生产 kbs 集群】部署。请确认当前 context『$ctx』确为生产集群。"
    } else {
        Write-Info "这是【测试服集群】部署。"
    }
    Write-Warn "这会对『$ctx』集群做变更。确认无误请输入该 context 名字以继续:"
    $confirm = Read-Host "  输入 context 名"
    if ($confirm -cne $ctx) {
        Write-Err "输入与当前 context 不一致,已中止(防误操作)。"
        return
    }
    if ($Env -eq 'prod') {
        $p = Read-Host "  生产环境二次确认,请输入大写 PROD 继续"
        if ($p -cne 'PROD') { Write-Err "生产二次确认失败,已中止。"; return }
    }

    if ($Down) {
        Write-Step "删除 online 业务服务 + Agones Fleet/RBAC($Env)"
        # 业务 overlay(pandora 命名空间 20 个 Deployment/Service/netpol 等)
        kubectl @kubectlContextArgs delete -k $overlay --ignore-not-found
        Assert-LastExit 'kubectl delete -k overlays/online'
        # 启动时 Apply-AgonesManifests 还 apply 了 Fleet(default ns)与 allocator RBAC,否则 Down 后
        # DS GameServer 继续占集群资源、孤儿 RBAC 残留(回应审核 P1:Down 只删业务 overlay)。
        # ⚠️ 删 Fleet 会终止在跑的战斗/大厅 DS —— Down 本身就是下线动作,语义一致。
        $downAgonesDir = Join-Path $ProjectRoot 'deploy/k8s/agones'
        # 25-fleetautoscaler 放最前:先停 autoscaler 再删 Fleet,避免删除期间 buffer 补建 GameServer。
        foreach ($f in @('25-fleetautoscaler-battle.yaml', '20-fleet-battle.yaml', '21-fleet-battle-canary.yaml', '30-fleet-hub.yaml', '31-fleet-hub-canary.yaml', '10-rbac-allocator.yaml')) {
            kubectl @kubectlContextArgs delete -f (Join-Path $downAgonesDir $f) --ignore-not-found
            Assert-LastExit "kubectl delete $f"
        }
        Write-Ok "online($Env)已下线(业务 overlay + Fleet + RBAC)。"
        return
    }

    # 任何 Secret/Fleet/Deployment/registry 写入前先拒绝旧单轨 Fleet。Hub 无可靠的自动排空
    # 证明，所以这里只 fail-closed，永不在普通发布中代删。
    Assert-NoLegacyDsFleets -KubeContext $ctx
    Assert-NoLegacyDSTicketSignerSecret -KubeContext $ctx

    if (-not $Registry -or -not $Tag) {
        throw "online 模式必须指定 -Registry 和 -Tag(Go 服务镜像来源)。"
    }
    if (-not $BattleDsImage -or -not $HubDsImage -or -not $DsGatewayAddr) {
        throw "online 模式必须指定 -BattleDsImage / -HubDsImage / -DsGatewayAddr，避免把本地 dev 镜像或错误网关地址带到远端集群。"
    }
    if ($BattleCanaryPercent -gt 0 -and ([string]::IsNullOrWhiteSpace($CanaryBattleDsImage) -or $BattleCanaryReplicas -lt 1)) {
        throw 'BattleCanaryPercent > 0 时必须提供 -CanaryBattleDsImage 且 -BattleCanaryReplicas >= 1。'
    }
    if ($HubCanaryPercent -gt 0 -and ([string]::IsNullOrWhiteSpace($CanaryHubDsImage) -or $HubCanaryReplicas -lt 1)) {
        throw 'HubCanaryPercent > 0 时必须提供 -CanaryHubDsImage 且 -HubCanaryReplicas >= 1。'
    }
    # cohort seed 是发布状态，不是密钥。从已有 pandora-config 只读回收它，防普通
    # 发布因漏传参数把 seed 清空、导致灰度 cohort 漂移。旧权重>0 时禁止换 seed；
    # 先降两轨权重到 0 并验收后，下一次 campaign 才可显式换 seed。
    $existingConfig = $null
    $existingConfigLines = @(& kubectl @kubectlContextArgs get secret/pandora-config -n $K8sNamespace --ignore-not-found -o json 2>&1)
    if ($LASTEXITCODE -ne 0) { throw "读取现有 pandora-config 检查 Canary cohort 失败:$($existingConfigLines -join [Environment]::NewLine)" }
    $existingConfigText = (($existingConfigLines | ForEach-Object { $_.ToString() }) -join "`n").Trim()
    if (-not [string]::IsNullOrWhiteSpace($existingConfigText)) {
        try { $existingConfig = $existingConfigText | ConvertFrom-Json -ErrorAction Stop }
        catch { throw "pandora-config 回读不是合法 JSON:$($_.Exception.Message)" }
        try {
            $oldBattleConfig = [System.Text.Encoding]::UTF8.GetString([Convert]::FromBase64String([string]$existingConfig.data.'ds-allocator.yaml'))
            $oldHubConfig = [System.Text.Encoding]::UTF8.GetString([Convert]::FromBase64String([string]$existingConfig.data.'hub-allocator.yaml'))
        } catch { throw "pandora-config 的 allocator 配置 base64 非法:$($_.Exception.Message)" }
        $oldCanary = Get-PandoraCanaryConfigContract -BattleConfig $oldBattleConfig -HubConfig $oldHubConfig
        if ([string]::IsNullOrWhiteSpace($CanarySeed) -and -not [string]::IsNullOrWhiteSpace($oldCanary.Seed)) {
            $CanarySeed = $oldCanary.Seed
            Write-Info '未显式传 CanarySeed，已从现有集群配置只读继承（不打印 seed）。'
        }
        if (($oldCanary.BattlePercent -gt 0 -or $oldCanary.HubPercent -gt 0) -and
            $CanarySeed -cne $oldCanary.Seed) {
            throw '现有 Canary 权重非 0 时禁止更换 cohort seed。请先用原 seed 将 Battle/Hub 权重降为 0 并验收，再开新 campaign。'
        }
    }
    if (($BattleCanaryPercent -gt 0 -or $HubCanaryPercent -gt 0) -and
        $CanarySeed -cnotmatch '^[A-Za-z0-9._-]{8,128}$') {
        throw '启用 DS Canary 时必须提供稳定的 -CanarySeed / PANDORA_DS_CANARY_SEED(8..128 字符，仅字母数字._-)。'
    }
    $currentCommit = ((& git -C $ProjectRoot rev-parse --short=7 HEAD 2>$null) | Out-String).Trim().ToLowerInvariant()
    if ($LASTEXITCODE -ne 0) { throw 'online 无法读取当前 git commit，不能生成不可变发布 tag。' }
    Assert-PandoraImmutableReleaseTag -Tag $Tag -CurrentCommit $currentCommit -RequireCurrentCommit:$BuildPush

    # ===== 生产密钥预检(P0 安全审核)=====
    # 必须在 BuildPush(推镜像到远端 registry)之前就确认两把真密钥齐全 —— 否则可能镜像已推、稍后
    # gen -Prod 才因缺密钥失败,留下「半推 + 未部署」的脏状态。玩家面 / DS 回调面必须分离:
    # 同一把密钥覆盖玩家 JWT 与 DS 回调令牌时,泄露玩家面即可伪造 DS 回调绕过范围绑定。
    $devPubSecret = 'pandora-dev-jwt-secret-change-me-32!'
    $playerSec = $env:PANDORA_JWT_SECRET
    $dsSec = $env:PANDORA_DS_JWT_SECRET
    # additional_secrets 部分接线仅供非生产验证；两份轮换决策未拍板前 online 直接拒绝。
    $playerSecAdd = $env:PANDORA_JWT_SECRET_ADDITIONAL
    $dsSecAdd = $env:PANDORA_DS_JWT_SECRET_ADDITIONAL
    if (-not [string]::IsNullOrWhiteSpace($playerSecAdd) -or -not [string]::IsNullOrWhiteSpace($dsSecAdd)) {
        throw 'online 暂不允许 PANDORA_*_SECRET_ADDITIONAL：玩家/DS 轮换决策仍待人拍板，Edge 双 key 证明与统一 key-set gate 尚未获批准。' +
              '当前只允许在非生产环境验证部分接线；批准决策并补齐阶段验收后再开放生产轮换。'
    }
    $secretChecks = @(
        @{ n = 'PANDORA_JWT_SECRET(玩家面)';    v = $playerSec; required = $true },
        @{ n = 'PANDORA_DS_JWT_SECRET(DS 回调面)'; v = $dsSec; required = $true },
        @{ n = 'PANDORA_JWT_SECRET_ADDITIONAL(玩家面轮换兼容密钥)';    v = $playerSecAdd; required = $false },
        @{ n = 'PANDORA_DS_JWT_SECRET_ADDITIONAL(DS 回调面轮换兼容密钥)'; v = $dsSecAdd; required = $false })
    foreach ($p in $secretChecks) {
        if ([string]::IsNullOrWhiteSpace($p.v)) {
            if ($p.required) {
                throw "online -Prod 部署必须先设环境变量 $($p.n)(真 HS256 密钥);缺失已中止,不推镜像不部署(P0)。"
            }
            continue
        }
        if ($p.v -eq $devPubSecret) { throw "$($p.n) 不能等于公开 dev 密钥,请换成真密钥。" }
        if ([System.Text.Encoding]::UTF8.GetByteCount($p.v) -lt 32) { throw "$($p.n) 至少需要 32 字节(HS256)。" }
        # C0/C1 控制字符防线(二审 #12):换行/回车/制表等混进密钥会被 YAML 双引号转义后原样进服务,
        # 与运维手里的密钥「看起来相同实际不同」,导致全端验签静默失败。
        if ($p.v -match '[\x00-\x1F\x7F-\x9F]') {
            throw "$($p.n) 含控制字符(换行/回车/制表等),多半是复制粘贴事故;已中止(P0)。"
        }
    }
    # 四把密钥两两不同 + 玩家面/DS 面跨面不相交(P0:任一交叉 = 泄露一面即可伪造另一面)。
    $secretPairs = @($secretChecks | Where-Object { -not [string]::IsNullOrWhiteSpace($_.v) })
    for ($i = 0; $i -lt $secretPairs.Count; $i++) {
        for ($j = $i + 1; $j -lt $secretPairs.Count; $j++) {
            if ($secretPairs[$i].v -ceq $secretPairs[$j].v) {
                throw "$($secretPairs[$i].n) 与 $($secretPairs[$j].n) 不得相同(P0:玩家面/DS 回调面/新旧轮换密钥必须各自独立)。"
            }
        }
    }
    # Online 使用本次调用独占的不可复用快照。共享 run/cluster/etc 在 Edge 探测/BuildPush 的长窗口内
    # 可能被 docker/k8s/Resume 重新生成成 dev/mock 配置，不能作为生产预检后的发布源。
    $onlineSnapshotRoot = [System.IO.Path]::GetFullPath((Join-Path $ProjectRoot 'run/cluster'))
    $onlineConfigDir = Join-Path $onlineSnapshotRoot "online-$Env-$([guid]::NewGuid().ToString('N'))"
    $runtimeOverlayDir = ''
    $dsticketOperationLock = $null
    try {
    # 所有本地确定性检查必须先于 BuildPush。生成器在独立 staging 中完成 20 文件精确校验，
    # 发布失败会回滚旧集合；因此源配置缺失、磁盘错误、JWKS/密钥异常都不会等到推镜像后才暴露。
    Write-Step "预生成并完整校验线上集群配置(allocator=agones)"
    $genArgs = @('-OutDir', $onlineConfigDir, '-AllocatorMode', 'agones', '-Prod')
    if (-not [string]::IsNullOrWhiteSpace($DsAuthMode)) {
        $genArgs += @('-DsAuthMode', $DsAuthMode)
    }
    if ($DsAuthorityMode -ne 'redis') { throw 'online 必须显式传 -DsAuthorityMode redis。' }
    if ([string]::IsNullOrWhiteSpace($DsFenceEtcdEndpoints)) { throw 'online 必须提供 -DsFenceEtcdEndpoints 或 PANDORA_DS_AUTH_FENCE_ETCD_ENDPOINTS。' }
    if ([string]::IsNullOrWhiteSpace($DsFenceKeysetRevision)) { throw 'online 必须提供 -DsFenceKeysetRevision 或 PANDORA_DS_AUTH_KEYSET_REVISION。' }
    # DSTicket v2(方案 B):普通发布只读核对集群里已 bootstrap 的当前 immutable keyset，不在发布流程生成/改钥。
    # 当前普通发布门禁要求 JWKS 顶层 active_kid 与同 revision signer Secret 私钥一致，不接收
    # public-only stage；独立轮换工具即使预投递了其它 revision，也不能借普通 online 发布切换。
    # active kid 由 Secret 私钥推导，再与 revisioned JWKS 中同 kid 的 n/e 对账；keys[0] 顺序没有语义。
    if ($DsTicketKeysetRevision -cnotmatch '^[1-9][0-9]*$' -or [int64]$DsTicketKeysetRevision -gt [int]::MaxValue) {
        throw 'online 必须提供正整数 -DsTicketKeysetRevision / PANDORA_DSTICKET_KEYSET_REVISION。'
    }
    $dstTicketRevision = [int]$DsTicketKeysetRevision
    $dstTicketSignerSecretName = "pandora-dsticket-signer-r$dstTicketRevision"
    $dstTicketSecret = Get-KubectlJsonObject -KubeContext $ctx `
        -Arguments @('get', "secret/$dstTicketSignerSecretName", '-n', $K8sNamespace, '-o', 'json') `
        -Action "读取 immutable DSTicket signer Secret/$dstTicketSignerSecretName"
    $dstTicketKeyContract = Get-OnlineDSTicketKeyMaterialContract -KubeContext $ctx `
        -Revision $dstTicketRevision -ExpectedActiveKid $DsTicketActiveKid -SignerObject $dstTicketSecret
    $DsTicketActiveKid = $dstTicketKeyContract.ActiveKid
    Write-Ok "DSTicket immutable keyset 对账通过:signer=$dstTicketSignerSecretName keyset=r$dstTicketRevision kid=$DsTicketActiveKid keys=$($dstTicketKeyContract.KeyCount)。"
    $ordinaryDSTicketState = Assert-OnlineOrdinaryDSTicketState -KubeContext $ctx -RequestedRevision $dstTicketRevision
    if (-not [string]::IsNullOrWhiteSpace([string]$ordinaryDSTicketState.ActiveKid) -and
        [string]$ordinaryDSTicketState.ActiveKid -cne $DsTicketActiveKid) {
        throw '普通发布早期状态的 fixed/terminal active kid 与 immutable key material 不一致。'
    }
    Write-Ok "普通发布 DSTicket 早期只读门禁通过:r$dstTicketRevision state=$($ordinaryDSTicketState.State)。集群写入前将在互斥锁内重新采样。"
    $genArgs += @(
        '-DsAuthorityMode', 'redis', '-DsFenceEtcdEndpoints', $DsFenceEtcdEndpoints,
        '-DsFenceKeysetRevision', $DsFenceKeysetRevision, '-DsTicketActiveKid', $DsTicketActiveKid,
        '-DsTicketKeysetRevision', [string]$dstTicketRevision,
        '-BattleCanaryPercent', [string]$BattleCanaryPercent, '-HubCanaryPercent', [string]$HubCanaryPercent)
    if (-not [string]::IsNullOrWhiteSpace($CanarySeed)) { $genArgs += @('-CanarySeed', $CanarySeed) }
    & "$ScriptDir/gen_cluster_config.ps1" @genArgs
    Assert-LastExit '生成 online 候选配置'

    # P1:普通发布不是换钥流程。必须比较 live Secret/pandora-config 与本次生成结果中实际落盘的
    # 玩家 Session / DS callback HMAC 指纹及 additional keyset；只比较完整 SHA256，不打印密钥
    # 或指纹。这样环境变量误换、生成器漂移、additional 被普通发布增删都会在 push/apply 前中止。
    $hmacServiceNames = @((Get-ServiceList) | ForEach-Object { [string]$_.Name })
    if ($null -ne $existingConfig) {
        $liveHmacConfigs = ConvertFrom-PandoraConfigSecretObject -SecretObject $existingConfig `
            -ExpectedServiceNames $hmacServiceNames
        $candidateHmacConfigs = Get-PandoraGeneratedConfigObject -ConfigDir $onlineConfigDir `
            -ExpectedServiceNames $hmacServiceNames
        Assert-PandoraOnlineHmacContinuity -LiveConfigs $liveHmacConfigs `
            -CandidateConfigs $candidateHmacConfigs | Out-Null
        Write-Ok '普通发布 HMAC 连续性门禁通过（玩家 Session / DS callback / additional keyset 均未变化；不打印指纹）。'
    } else {
        # 首次 bootstrap 没有 live baseline 可比较；后续普通发布一律走上面的连续性门禁。
        # 若运行中的业务对象仍在但 Secret 被删，不能把它误判成首次 bootstrap。
        $liveServiceLines = @(& kubectl @kubectlContextArgs get deployments -n $K8sNamespace --ignore-not-found -o name 2>&1)
        if ($LASTEXITCODE -ne 0) {
            $namespaceLines = @(& kubectl @kubectlContextArgs get namespace/$K8sNamespace --ignore-not-found -o name 2>&1)
            if ($LASTEXITCODE -ne 0 -or
                -not [string]::IsNullOrWhiteSpace((($namespaceLines | ForEach-Object { $_.ToString() }) -join '').Trim())) {
                throw 'live Secret/pandora-config 缺失且无法证明这是空的新集群；拒绝普通发布，须走独立恢复/换钥流程。'
            }
            $liveServiceLines = @()
        }
        $expectedLiveDeploymentNames = @($hmacServiceNames | ForEach-Object { "deployment.apps/$_" })
        $knownDeployments = @($liveServiceLines | ForEach-Object { $_.ToString().Trim() } |
            Where-Object { $expectedLiveDeploymentNames -ccontains $_ })
        if ($knownDeployments.Count -gt 0) {
            throw 'live Secret/pandora-config 缺失但业务 Deployment 已存在；拒绝把密钥基线丢失误判为首次部署，须走独立恢复/换钥流程。'
        }
        Write-Warn 'live Secret/pandora-config 不存在且未发现既有业务 Deployment：按首次 bootstrap 继续；本次生成结果将成为后续普通发布的 HMAC 基线。'
    }
    # 边缘网关 JWKS 校验门(P0/P1:gen 产出的 JWKS 无仓库内 Envoy 消费,生产客户端面 :8443 由外部边缘
    # 网关校验)。若边缘网关未同步玩家面密钥派生的 JWKS,login 用新密钥签的 SessionToken 会被边缘网关
    # 全拒。分两级 —— 优先**真实探测**(证明当前 edge 确实在用当前密钥),否则退回**运维承诺**(布尔):
    #   1) 设 PANDORA_EDGE_PROBE_URL(edge 上一条受 pandora_session JWT 保护的路由)→ 三段现签探测:
    #        a. 无 token          应 401(证明该路由确受 JWT 保护)
    #        b. 错误密钥签的 token 应 401(证明 edge 真在**验签**,而非只看 token 是否存在 → 排除假阳性)
    #        c. 当前玩家面密钥签的 token 应**非 401**(证明当前 edge 就在用当前密钥)
    #      三者缺一即 fail-closed 中止(任一不符 = 无法证明 edge 已用当前密钥)。
    #   2) 未提供探测 URL 时退回 PANDORA_EDGE_JWKS_ACK=1(仅运维书面承诺,不构成证明,打印当前密钥指纹供审计)。
    #   3) 两者都无 → fail-closed 中止。
    # 当前玩家面密钥指纹(SHA256 前 12 hex):用于日志审计,让「真实探测通过」与「运维 ACK」都能对上具体密钥。
    $playerKeyFp = ([System.BitConverter]::ToString(
            [System.Security.Cryptography.SHA256]::Create().ComputeHash(
                [System.Text.Encoding]::UTF8.GetBytes($playerSec))) -replace '-', '').Substring(0, 12).ToLower()
    $edgeProbeUrl = $env:PANDORA_EDGE_PROBE_URL
    if (-not [string]::IsNullOrWhiteSpace($edgeProbeUrl)) {
        # fail-closed(审核 P1 #7):生产探测链路必须 HTTPS。HTTP 明文探测可被中间人伪造出
        # 预期的 401/401/非401 响应,让脚本误判「edge 已切当前密钥」。非 https 直接中止。
        if ($edgeProbeUrl -notmatch '^(?i)https://') {
            throw "边缘 JWKS 探测:PANDORA_EDGE_PROBE_URL 必须是 https:// 链路(当前=$edgeProbeUrl)。" +
                  "生产不接受 HTTP 明文探测(可被 MITM 伪造 401/非401 响应制造假阳性)。"
        }
        # base64url(bytes):无填充、+/→-_。
        function ConvertTo-B64Url([byte[]]$b) { [Convert]::ToBase64String($b).TrimEnd('=').Replace('+', '-').Replace('/', '_') }
        # 用给定密钥现签一枚 60s 有效的 HS256 探测 JWT(iss/aud 对齐 login SessionToken 与 envoy provider)。
        function New-ProbeJwt([string]$secret) {
            $nowSec = [int][double]::Parse((Get-Date -UFormat %s))
            $hdrJson = '{"alg":"HS256","typ":"JWT"}'
            $plJson = '{"iss":"pandora-login","aud":"pandora-client","sub":"edge-jwks-probe","iat":' + $nowSec + ',"exp":' + ($nowSec + 60) + '}'
            $enc = [System.Text.Encoding]::UTF8
            $signingInput = (ConvertTo-B64Url $enc.GetBytes($hdrJson)) + '.' + (ConvertTo-B64Url $enc.GetBytes($plJson))
            $hmac = [System.Security.Cryptography.HMACSHA256]::new($enc.GetBytes($secret))
            try { $sig = $hmac.ComputeHash($enc.GetBytes($signingInput)) } finally { $hmac.Dispose() }
            return $signingInput + '.' + (ConvertTo-B64Url $sig)
        }
        $probeJwt = New-ProbeJwt $playerSec
        # 错误密钥:与真密钥不同的另一把(用于负向探测,证明 edge 真在验签)。
        $wrongJwt = New-ProbeJwt ($playerSec + '-WRONG-KEY-EDGE-PROBE-NEGATIVE-CONTROL')

        $reqArgs = @{ Uri = $edgeProbeUrl; Method = 'Get'; TimeoutSec = 10; SkipHttpErrorCheck = $true; ErrorAction = 'Stop' }
        if ($env:PANDORA_EDGE_PROBE_INSECURE -eq '1') {
            # fail-closed(审核 P1 #7):生产严禁跳过 TLS 证书校验。跳过后 MITM 可伪造 edge 响应制造
            # 假阳性,让脚本误判 edge 已切当前密钥。online -Prod 路径遇到该开关直接中止,不降级。
            throw "边缘 JWKS 探测:online -Prod 不允许 PANDORA_EDGE_PROBE_INSECURE=1(跳过 TLS 校验会被 MITM 伪造响应)。" +
                  "请让边缘网关提供受信任证书,或在受控内网用可验证的证书链;跳过校验仅限非生产链路。"
        }
        try {
            $noTok = Invoke-WebRequest @reqArgs
            $wrongTok = Invoke-WebRequest @reqArgs -Headers @{ Authorization = "Bearer $wrongJwt" }
            $withTok = Invoke-WebRequest @reqArgs -Headers @{ Authorization = "Bearer $probeJwt" }
        } catch {
            throw "边缘 JWKS 探测请求失败(URL=$edgeProbeUrl):$($_.Exception.Message)。修正 PANDORA_EDGE_PROBE_URL/网络后重试;不带证明不部署。"
        }
        if ($noTok.StatusCode -ne 401) {
            throw "边缘 JWKS 探测:无 token 访问 $edgeProbeUrl 返回 $($noTok.StatusCode)(应为 401)。该路由未受 pandora_session JWT 保护,无法据此验证 edge 密钥。请把 PANDORA_EDGE_PROBE_URL 指向受 JWT 保护的 edge 路由。"
        }
        if ($wrongTok.StatusCode -ne 401) {
            throw "边缘 JWKS 探测:错误密钥签的 token 访问 $edgeProbeUrl 返回 $($wrongTok.StatusCode)(应为 401)。edge 未对签名做校验(疑似只判 token 是否存在),探测结论不可信 → 中止。请检查边缘 Envoy 的 jwt_authn 配置。"
        }
        if ($withTok.StatusCode -eq 401) {
            throw "边缘 JWKS 探测:当前玩家面密钥(指纹 $playerKeyFp)现签 token 访问 $edgeProbeUrl 仍返回 401 —— 当前边缘 Envoy 用的不是 PANDORA_JWT_SECRET 派生的 JWKS。请先通过受控密钥管线或单独运行 gen_cluster_config.ps1 -Prod 生成并同步 JWKS 后再部署(本次独占快照退出时会清理；否则 login 新密钥签发的 SessionToken 会被全拒)。"
        }
        Write-Ok "边缘 JWKS 真实探测通过(密钥指纹 $playerKeyFp):无 token→401、错误密钥→401、当前密钥→$($withTok.StatusCode),证明当前 edge 就在用当前玩家面密钥且确在验签。"
    } elseif ($env:PANDORA_EDGE_JWKS_ACK -eq '1') {
        Write-Warn "PANDORA_EDGE_JWKS_ACK=1 仅为运维书面承诺(未做真实探测,不能证明当前 edge 已用当前密钥)。"
        Write-Warn "本次玩家面密钥指纹:$playerKeyFp —— 请人工核对边缘 Envoy 当前 JWKS 与该指纹一致。"
        Write-Warn "强烈建议改设 PANDORA_EDGE_PROBE_URL(受 pandora_session JWT 保护的 edge 路由)让脚本现签探测 token 实测。"
    } else {
        throw "online -Prod 需验证边缘 Envoy(客户端 :8443 JWT 校验)的 JWKS 已同步为 PANDORA_JWT_SECRET 派生值(当前密钥指纹 $playerKeyFp)。" +
              " 本仓库不含生产边缘网关(外部自带同名 pandora-envoy Service)。请先通过受控密钥管线或单独运行 gen_cluster_config.ps1 -Prod 生成并同步 JWKS" +
              " (本次独占快照退出时会清理),再设 PANDORA_EDGE_PROBE_URL 做真实探测(推荐),或确认后设 PANDORA_EDGE_JWKS_ACK=1;" +
              " 否则 login 新密钥签发的 SessionToken 会被边缘网关全部拒绝。"
    }
    Write-Ok "生产密钥预检通过:玩家面 / DS 回调面为两把独立真密钥,边缘 JWKS 已校验。"

    $writerServices = @('login', 'player-locator', 'ds-allocator', 'hub-allocator', 'battle-result')
    $secureDsAuthGoArgs = @()
    $onlineDsAuthState = $null
    Write-Step '只读验证 DS auth required_writer_epoch 已显式建立'
    $requiredMin = 1
    $requiredMax = 2
    if ($Env -eq 'prod') {
        $null = Assert-PandoraDsAuthHttpsEndpoints $DsFenceEtcdEndpoints
        Assert-PandoraDsAuthEtcdRevision $DsFenceEtcdIdentityRevision
        $secureDsAuthGoArgs = @(Get-PandoraDsAuthSecureGoArgs $DsFenceEtcdAuditorCAFile `
            $DsFenceEtcdAuditorCertFile $DsFenceEtcdAuditorKeyFile $DsFenceEtcdServerName `
            $DsFenceEtcdAuditorIdentity $DsFenceEtcdIdentityRevision $DsFenceEtcdForbiddenReadPrefix `
            $DsFenceEtcdAuditorUsernameFile $DsFenceEtcdAuditorPasswordFile)
        # 普通生产发布只允许复用已完成一次性 1→2 激活的终态，绝不在发布流程调用 activate/换身份。
        $requiredMin = 2
        $requiredMax = 2
    }
    Push-Location $ProjectRoot
    try {
        & go run ./pkg/dsauthfence/cmd/dsauth-required --endpoints $DsFenceEtcdEndpoints `
            --min-epoch $requiredMin --max-epoch $requiredMax @secureDsAuthGoArgs
        $requiredExit = $LASTEXITCODE
    } finally {
        Pop-Location
    }
    if ($requiredExit -ne 0) {
        throw 'DS auth required_writer_epoch 不存在、非法或 etcd 不可线性读取；已在 BuildPush/Secret/Fleet/Deployment 前停止。' +
              '禁止把缺 key 默认成 1。fresh 集群须先走经 Claude 审核的显式 baseline bootstrap；当前 BootstrapRequired 仍有删除后回退风险，不能自动调用。'
    }
    # 2026-07-13 的真实本地验收只证明了旧 K1 镜像上的正向准入与篡改拒绝；加入票据日志
    # 脱敏入口后，重试的 UE 未到达 DS 认证入口，K1→K2→K2-only（含 K1 旧票耗尽）没有完成。
    # 同轮 Inventory / DS-terminal Istio 素材也仅为独立候选，未安装或激活。不得把静态清单、
    # 单阶段结果或 ready Fleet 冒充完整生产验收；在补齐真链路前保持所有 online 写入为零。
    throw 'online 零写阻断：DSTicket K1→K2→K2-only 真 Kubernetes/UE E2E 尚未完成，' +
          'Inventory/DS-terminal mesh 也未独立激活。当前不得 BuildPush、写 Secret/Fleet/Deployment 或发布生产。'

    if ($Env -eq 'prod') {
        Write-Step '任何 registry/K8s 写前只读验证 canonical green、blue=0、Endpoint UID 与运行时 capability/features'
        $onlineDsAuthState = Assert-OnlineDsAuthRuntimeAndCapabilities -KubeContext $ctx `
            -WriterServices $writerServices -Revision $DsFenceEtcdIdentityRevision `
            -ServerName $DsFenceEtcdServerName -ForbiddenReadPrefix $DsFenceEtcdForbiddenReadPrefix `
            -EtcdEndpoints $DsFenceEtcdEndpoints -KeysetRevision $DsFenceKeysetRevision `
            -SecureGoArgs $secureDsAuthGoArgs
        Write-Ok '生产 DS auth mTLS/CN/AuthStatus/ACL、immutable identity、canonical green 与 capability/features 前置审计通过。'
    }

    # 上述真链路闭环并由 Claude 复审后，才允许移除零写门并进入以下 immutable digest 管线。
    $serviceNames = @((Get-ServiceList) | ForEach-Object { [string]$_.Name })
    $registryRoot = $Registry.Trim().TrimEnd('/')
    $goDigests = @{}
    $goPins = @{}
    $remoteRefs = @{}
    foreach ($name in $serviceNames) { $remoteRefs[$name] = "$registryRoot/pandora/${name}:$Tag" }

    if ($BuildPush) {
        Assert-CleanOnlineReleaseSource
        if ($env:PANDORA_OFFLINE -eq '1') {
            throw 'online BuildPush 禁止 PANDORA_OFFLINE=1：发布必须从当前 clean commit 严格重建，不能导入/复用离线包。'
        }
        throw 'online BuildPush 仍阻断：必须先在目标 registry 启用并由平台验证 native immutable-tag/create-only 策略与发布锁。HEAD 预检存在 TOCTOU，不能作为不可变证明；本次未 build、未 push。'
        Write-Step '预检 20 个不可变 tag 均不存在（任一鉴权/网络不确定即阻断）'
        foreach ($name in $serviceNames) { Assert-RemoteImageTagAbsent -Reference $remoteRefs[$name] }
        Write-Step "从当前 clean commit 严格重建并推送 20 个 Go 服务镜像到 $Registry"
        Build-AllImages -StrictRelease
        foreach ($name in $serviceNames) {
            Assert-LocalImageRevision -Reference "pandora/${name}:dev" -Expected $currentCommit
        }
        foreach ($svc in (Get-ServiceList)) {
            $local  = "pandora/$($svc.Name):dev"
            $remote = [string]$remoteRefs[$svc.Name]
            $descriptor = Push-ImageAndResolveDigest -Local $local -Remote $remote
            $goDigests[$svc.Name] = $descriptor.Digest
            $goPins[$svc.Name] = $descriptor.Pinned
        }
    } else {
        Write-Step '从 registry 解析 20 个 Go 服务镜像 digest（不按 tag 部署）'
        foreach ($name in $serviceNames) {
            $descriptor = Get-RegistryImageDescriptor -Reference $remoteRefs[$name]
            $goDigests[$name] = $descriptor.Digest
            $goPins[$name] = $descriptor.Pinned
        }
    }

    $battleDescriptor = Get-RegistryImageDescriptor -Reference $BattleDsImage
    $hubDescriptor = Get-RegistryImageDescriptor -Reference $HubDsImage
    $battleCanaryDescriptor = if ([string]::IsNullOrWhiteSpace($CanaryBattleDsImage)) {
        $battleDescriptor
    } else {
        Get-RegistryImageDescriptor -Reference $CanaryBattleDsImage
    }
    $hubCanaryDescriptor = if ([string]::IsNullOrWhiteSpace($CanaryHubDsImage)) {
        $hubDescriptor
    } else {
        Get-RegistryImageDescriptor -Reference $CanaryHubDsImage
    }
    foreach ($pair in @(
        @{ Input = $BattleDsImage; Desc = $battleDescriptor },
        @{ Input = $HubDsImage; Desc = $hubDescriptor },
        @{ Input = $CanaryBattleDsImage; Desc = $battleCanaryDescriptor },
        @{ Input = $CanaryHubDsImage; Desc = $hubCanaryDescriptor })) {
        if ([string]::IsNullOrWhiteSpace([string]$pair.Input)) { continue }
        $inputDigest = Get-PandoraImageDigestFromReference -Reference $pair.Input
        if (-not [string]::IsNullOrWhiteSpace($inputDigest) -and $inputDigest -cne $pair.Desc.Digest) {
            throw "DS 输入 pin 与 registry 回读不一致:$($pair.Input) input=$inputDigest registry=$($pair.Desc.Digest)"
        }
    }
    $battlePin = $battleDescriptor.Pinned
    $hubPin = $hubDescriptor.Pinned
    $battleCanaryPin = $battleCanaryDescriptor.Pinned
    $hubCanaryPin = $hubCanaryDescriptor.Pinned
    if (-not [string]::IsNullOrWhiteSpace($CanaryBattleDsImage) -and
        $battleCanaryDescriptor.Digest -ceq $battleDescriptor.Digest) {
        throw 'Battle Canary 必须使用与 Stable 不同的独立 digest；同 digest 无法证明灰度轨道。'
    }
    if (-not [string]::IsNullOrWhiteSpace($CanaryHubDsImage) -and
        $hubCanaryDescriptor.Digest -ceq $hubDescriptor.Digest) {
        throw 'Hub Canary 必须使用与 Stable 不同的独立 digest；同 digest 无法证明灰度轨道。'
    }
    # 不传 Canary 镜像 = 仅创建 0 Ready 的休眠轨道，清单仍用 Stable pin 便于结构验证。
    # 显式传图则允许 percent=0 预热，等 Ready 后再通过后续配置 rollout 放量。
    $battleCanaryReplicaApply = if ([string]::IsNullOrWhiteSpace($CanaryBattleDsImage)) { 0 } else { $BattleCanaryReplicas }
    $hubCanaryReplicaApply = if ([string]::IsNullOrWhiteSpace($CanaryHubDsImage)) { 0 } else { $HubCanaryReplicas }

    Write-Step '生成独占 runtime overlay，并验证 20 个 digest pin + 5 个 writer annotation'
    $runtimeOverlayArgs = @{
        SourceOverlay = $overlay; Registry = $registryRoot; Digests = $goDigests
        ServiceNames = $serviceNames; WriterServices = $writerServices; EnvironmentName = $Env
        DSTicketKeysetRevision = $dstTicketRevision
    }
    if ($Env -eq 'prod') {
        $runtimeOverlayArgs.EtcdIdentityRevision = $DsFenceEtcdIdentityRevision
        $runtimeOverlayArgs.EtcdServerName = $DsFenceEtcdServerName
        $runtimeOverlayArgs.EtcdForbiddenReadPrefix = $DsFenceEtcdForbiddenReadPrefix
        $runtimeOverlayArgs.EtcdPasswordAuthByService = $onlineDsAuthState.PasswordAuth
        $runtimeOverlayArgs.GreenDesiredReplicas = $onlineDsAuthState.DesiredReplicas
        $runtimeOverlayArgs.GreenDeploymentObjects = $onlineDsAuthState.GreenDeploymentObjects
        $runtimeOverlayArgs.CanonicalDsAuthGreen = $true
    }
    $runtimeOverlay = New-OnlineRuntimeOverlay @runtimeOverlayArgs
    $runtimeOverlayDir = $runtimeOverlay.Path
    foreach ($fleetCheck in @(
        @{ File = '20-fleet-battle.yaml'; Pin = $battlePin; Container = 'pandora-battle-ds'; Track = 'stable'; Name = 'pandora-battle-stable' },
        @{ File = '21-fleet-battle-canary.yaml'; Pin = $battleCanaryPin; Container = 'pandora-battle-ds'; Track = 'canary'; Name = 'pandora-battle-canary' },
        @{ File = '30-fleet-hub.yaml'; Pin = $hubPin; Container = 'pandora-hub-ds'; Track = 'stable'; Name = 'pandora-hub-stable' },
        @{ File = '31-fleet-hub-canary.yaml'; Pin = $hubCanaryPin; Container = 'pandora-hub-ds'; Track = 'canary'; Name = 'pandora-hub-canary' })) {
        $fleetRaw = Get-Content -LiteralPath (Join-Path $ProjectRoot "deploy/k8s/agones/$($fleetCheck.File)") -Raw
        $fleetRendered = Set-PandoraFleetImagePin -Manifest $fleetRaw -PinnedImage $fleetCheck.Pin
        $fleetRendered = Set-PandoraFleetDSTicketKeysetRevision -Manifest $fleetRendered -Revision $dstTicketRevision
        $fleetContractRows = Get-PandoraFleetContractRows -Manifest $fleetRendered
        Assert-PandoraFleetManifestContract -Manifest $fleetRendered -ContractRows $fleetContractRows `
            -PinnedImage $fleetCheck.Pin -ContainerName $fleetCheck.Container -ExpectedTrack $fleetCheck.Track `
            -ExpectedFleetName $fleetCheck.Name
    }

    # 本地构建/registry 解析均在锁外；通过全部生产硬门后，在第一笔 DSTicket
    # 相关集群写前原子 create-only 抢锁。rotation 同样使用此锁，消除 preflight->apply TOCTOU。
    $dsticketOperationLock = Enter-OnlineDSTicketOperationLock -KubeContext $ctx
    $null = Assert-OnlineDSTicketOperationLockHeld -KubeContext $ctx -Identity $dsticketOperationLock
    $lockedKeyContract = Get-OnlineDSTicketKeyMaterialContract -KubeContext $ctx `
        -Revision $dstTicketRevision -ExpectedActiveKid $DsTicketActiveKid
    if ($lockedKeyContract.ActiveKid -cne $dstTicketKeyContract.ActiveKid -or
        $lockedKeyContract.JwksSha256 -cne $dstTicketKeyContract.JwksSha256 -or
        $lockedKeyContract.PrivatePemSha256 -cne $dstTicketKeyContract.PrivatePemSha256) {
        throw '锁内重读 DSTicket key material 与早期预检不一致，拒绝写集群。'
    }
    $lockedOrdinaryState = Assert-OnlineOrdinaryDSTicketState -KubeContext $ctx -RequestedRevision $dstTicketRevision
    if (-not [string]::IsNullOrWhiteSpace([string]$lockedOrdinaryState.ActiveKid) -and
        [string]$lockedOrdinaryState.ActiveKid -cne $lockedKeyContract.ActiveKid) {
        throw '锁内 ordinary state active kid 与 immutable key material 不一致。'
    }
    if ([string]$lockedOrdinaryState.State -cne [string]$ordinaryDSTicketState.State) {
        throw "早期预检到锁内重验期间 DSTicket state 已变化:$($ordinaryDSTicketState.State) -> $($lockedOrdinaryState.State)；拒绝用旧候选覆盖。"
    }
    $earlyFixedRV = if ($null -eq $existingConfig) { '' } else { [string]$existingConfig.metadata.resourceVersion }
    $lockedFixedConfig = Get-OnlineOptionalKubectlJsonObject -KubeContext $ctx `
        -Arguments @('get', 'secret/pandora-config', '-n', $K8sNamespace, '--ignore-not-found', '-o', 'json') `
        -Action '锁内重读 fixed Secret/pandora-config'
    $lockedFixedRV = if ($null -eq $lockedFixedConfig) { '' } else { [string]$lockedFixedConfig.metadata.resourceVersion }
    if ($earlyFixedRV -cne $lockedFixedRV -or
        ($null -ne $existingConfig -and [string]::IsNullOrWhiteSpace($earlyFixedRV)) -or
        ($null -ne $lockedFixedConfig -and [string]::IsNullOrWhiteSpace($lockedFixedRV))) {
        throw '早期预检到锁内重验期间 fixed pandora-config resourceVersion/存在性已改变；拒绝覆盖新发布。'
    }
    if ($null -ne $lockedFixedConfig) {
        $lockedHmacConfigs = ConvertFrom-PandoraConfigSecretObject -SecretObject $lockedFixedConfig `
            -ExpectedServiceNames $hmacServiceNames
        $lockedCandidateHmacConfigs = Get-PandoraGeneratedConfigObject -ConfigDir $onlineConfigDir `
            -ExpectedServiceNames $hmacServiceNames
        Assert-PandoraOnlineHmacContinuity -LiveConfigs $lockedHmacConfigs `
            -CandidateConfigs $lockedCandidateHmacConfigs | Out-Null
    }
    $null = Assert-OnlineDSTicketOperationLockHeld -KubeContext $ctx -Identity $dsticketOperationLock
    Write-Ok "DSTicket 锁内权威门禁通过:r$dstTicketRevision state=$($lockedOrdinaryState.State)。"
    Write-Step "应用已校验的 namespace 基线($K8sNamespace)"
    kubectl @kubectlContextArgs apply -f (Join-Path $ProjectRoot 'deploy/k8s/services/00-namespace.yaml')
    Assert-LastExit 'kubectl apply 00-namespace'
    $null = Assert-OnlineDSTicketOperationLockHeld -KubeContext $ctx -Identity $dsticketOperationLock
    # 生产配置含两把真 HS256 密钥,用 Secret 承载(P0:严禁把真密钥写进明文 ConfigMap)。
    Apply-PandoraConfigSecret -KubeContext $ctx -ConfigDir $onlineConfigDir -Action 'kubectl apply secret pandora-config'

    Write-Step "apply Agones RBAC + Fleet(真 Linux DS)"
    # 线上 Agones 通常已由集群管理员预装;此处不自动 helm install,只 apply 业务 RBAC/Fleet
    kubectl @kubectlContextArgs get ns agones-system *> $null
    if ($LASTEXITCODE -ne 0) {
        Write-Warn "未检测到 agones-system —— 线上 Agones 须由集群管理员预先安装,否则 Fleet/分配不可用。"
    }
    $null = Assert-OnlineDSTicketOperationLockHeld -KubeContext $ctx -Identity $dsticketOperationLock
    Apply-AgonesManifests -BattleDsImage $battlePin -HubDsImage $hubPin `
        -CanaryBattleDsImage $battleCanaryPin -CanaryHubDsImage $hubCanaryPin `
        -BattleCanaryReplicaCount $battleCanaryReplicaApply -HubCanaryReplicaCount $hubCanaryReplicaApply `
        -BattleMaxReplicasOverride $BattleMaxReplicas -BattleBufferSizeOverride $BattleBufferSize `
        -DSTicketKeysetRevision $dstTicketRevision -DsGatewayAddr $DsGatewayAddr -DsGatewayTls $DsGatewayTls `
        -KubeContext $ctx

    Write-Step "kubectl apply -k runtime online overlay($Env,digest pinned)"
    $null = Assert-OnlineDSTicketOperationLockHeld -KubeContext $ctx -Identity $dsticketOperationLock
    kubectl @kubectlContextArgs apply -k $runtimeOverlayDir
    Assert-LastExit 'kubectl apply -k runtime online overlay'
    if ($Env -eq 'prod') {
        # green 是 epoch=2 后唯一 canonical writer；blue 已由 runtime overlay 固定 replicas=0。
        # patch 携带预检 resourceVersion + exact immutable selector/desired，冲突即停，不回切 blue。
        foreach ($writer in $writerServices) {
            $null = Assert-OnlineDSTicketOperationLockHeld -KubeContext $ctx -Identity $dsticketOperationLock
            $greenPatchPath = [string]$runtimeOverlay.GreenPatchPaths[$writer]
            if ([string]::IsNullOrWhiteSpace($greenPatchPath) -or -not (Test-Path -LiteralPath $greenPatchPath -PathType Leaf)) {
                throw "缺 canonical green/$writer release patch。"
            }
            kubectl @kubectlContextArgs replace -f $greenPatchPath
            Assert-LastExit "CAS replace canonical green Deployment/$writer-ds-auth-green"
        }
    }

    # Secret 传播(审核 P1 #4):pandora-config 以 subPath 挂载,Secret 内容更新不会热感知;
    # 且镜像 tag 不变时 apply -k 判定 pod 模板 unchanged 不触发 rollout → Pod 继续用旧密钥/旧配置。
    # 故显式按名 rollout restart 20 个业务 Deployment,强制重挂最新 Secret(服务支持 SIGTERM 排空,
    # 滚动重启零停机,见 CLAUDE.md §9 不变量 16),再等关键服务就绪确认新配置生效。
    Write-Step "rollout restart 业务 Deployment(传播更新后的 pandora-config Secret)"
    foreach ($svc in (Get-ServiceList)) {
        $null = Assert-OnlineDSTicketOperationLockHeld -KubeContext $ctx -Identity $dsticketOperationLock
        $deploymentName = if ($Env -eq 'prod' -and $writerServices -contains [string]$svc.Name) { "$($svc.Name)-ds-auth-green" } else { [string]$svc.Name }
        kubectl @kubectlContextArgs rollout restart "deploy/$deploymentName" -n $K8sNamespace
        Assert-LastExit "rollout restart $deploymentName(Secret 传播)"
    }
    foreach ($svc in (Get-ServiceList)) {
        $deploymentName = if ($Env -eq 'prod' -and $writerServices -contains [string]$svc.Name) { "$($svc.Name)-ds-auth-green" } else { [string]$svc.Name }
        kubectl @kubectlContextArgs rollout status "deploy/$deploymentName" -n $K8sNamespace --timeout=180s
        Assert-LastExit "rollout status $deploymentName(Secret 传播后未就绪,查:kubectl describe/logs)"
    }
    Assert-OnlineDeploymentImageState -KubeContext $ctx -Pins $goPins -Digests $goDigests `
        -WriterServices $writerServices -CanonicalGreen:($Env -eq 'prod')
    if ($Env -eq 'prod') {
        Write-Step '终态复核 canonical green/blue=0/Endpoint UID 与 runtime capability/features'
        $onlineDsAuthState = Assert-OnlineDsAuthRuntimeAndCapabilities -KubeContext $ctx `
            -WriterServices $writerServices -Revision $DsFenceEtcdIdentityRevision `
            -ServerName $DsFenceEtcdServerName -ForbiddenReadPrefix $DsFenceEtcdForbiddenReadPrefix `
            -EtcdEndpoints $DsFenceEtcdEndpoints -KeysetRevision $DsFenceKeysetRevision `
            -SecureGoArgs $secureDsAuthGoArgs -ExpectedDigests $goDigests
        Write-Ok 'canonical green 普通发布终态审计通过；无 blue writer、无额外 capability。'
    }
    Wait-OnlineReadyFleetImageState -KubeContext $ctx -Fleet 'pandora-battle-stable' -Container 'pandora-battle-ds' `
        -Pin $battlePin -Digest $battleDescriptor.Digest -ExpectedTrack stable
    Wait-OnlineReadyFleetImageState -KubeContext $ctx -Fleet 'pandora-battle-canary' -Container 'pandora-battle-ds' `
        -Pin $battleCanaryPin -Digest $battleCanaryDescriptor.Digest -ExpectedTrack canary
    Wait-OnlineReadyFleetImageState -KubeContext $ctx -Fleet 'pandora-hub-stable' -Container 'pandora-hub-ds' `
        -Pin $hubPin -Digest $hubDescriptor.Digest -ExpectedTrack stable
    Wait-OnlineReadyFleetImageState -KubeContext $ctx -Fleet 'pandora-hub-canary' -Container 'pandora-hub-ds' `
        -Pin $hubCanaryPin -Digest $hubCanaryDescriptor.Digest -ExpectedTrack canary
    $null = Assert-OnlineDSTicketOperationLockHeld -KubeContext $ctx -Identity $dsticketOperationLock
    $finalKeyContract = Get-OnlineDSTicketKeyMaterialContract -KubeContext $ctx `
        -Revision $dstTicketRevision -ExpectedActiveKid $DsTicketActiveKid
    if ($finalKeyContract.JwksSha256 -cne $lockedKeyContract.JwksSha256 -or
        $finalKeyContract.PrivatePemSha256 -cne $lockedKeyContract.PrivatePemSha256) {
        throw '普通发布验收时 DSTicket immutable key material 漂移。'
    }
    $finalOrdinaryState = Assert-OnlineOrdinaryDSTicketState -KubeContext $ctx -RequestedRevision $dstTicketRevision
    if ([string]::IsNullOrWhiteSpace([string]$finalOrdinaryState.ActiveKid) -or
        [string]$finalOrdinaryState.ActiveKid -cne $finalKeyContract.ActiveKid) {
        throw '普通发布终态 fixed/terminal active kid 与 immutable key material 不一致。'
    }
    $null = Assert-OnlineDSTicketOperationLockHeld -KubeContext $ctx -Identity $dsticketOperationLock
    Write-Ok "DSTicket 普通发布终态验收通过:r$dstTicketRevision state=$($finalOrdinaryState.State)。"
    Remove-LegacyPandoraConfigMapAfterRollout -KubeContext $ctx

    } finally {
        try {
        if (-not [string]::IsNullOrWhiteSpace($runtimeOverlayDir) -and (Test-Path -LiteralPath $runtimeOverlayDir -PathType Container)) {
            $resolvedRuntime = [System.IO.Path]::GetFullPath($runtimeOverlayDir)
            $runtimeParent = [System.IO.Path]::GetFullPath((Join-Path $ProjectRoot 'deploy/k8s/overlays'))
            $safeRuntimeParent = (Split-Path -Parent $resolvedRuntime).Equals($runtimeParent, [System.StringComparison]::OrdinalIgnoreCase)
            $safeRuntimeLeaf = (Split-Path -Leaf $resolvedRuntime) -match "^\.online-runtime-$([regex]::Escape($Env))-[0-9a-f]{32}$"
            if (-not $safeRuntimeParent -or -not $safeRuntimeLeaf) {
                throw "拒绝清理未经验证的 runtime overlay 路径:$resolvedRuntime"
            }
            try { Remove-Item -LiteralPath $resolvedRuntime -Recurse -Force }
            catch { throw "online 流程已结束,但 runtime overlay 清理失败:$resolvedRuntime;$($_.Exception.Message)" }
        }
        if (Test-Path -LiteralPath $onlineConfigDir -PathType Container) {
            $resolvedSnapshot = [System.IO.Path]::GetFullPath($onlineConfigDir)
            $resolvedParent = Split-Path -Parent $resolvedSnapshot
            $safeParent = $resolvedParent.Equals($onlineSnapshotRoot, [System.StringComparison]::OrdinalIgnoreCase)
            $safeLeaf = (Split-Path -Leaf $resolvedSnapshot) -match "^online-$([regex]::Escape($Env))-[0-9a-f]{32}$"
            if (-not $safeParent -or -not $safeLeaf) {
                throw "拒绝清理未经验证的 online 配置快照路径:$resolvedSnapshot"
            }
            try { Remove-Item -LiteralPath $resolvedSnapshot -Recurse -Force }
            catch { throw "online 流程已结束,但含生产密钥的临时配置快照清理失败:$resolvedSnapshot;$($_.Exception.Message)" }
        }
        } finally {
            if ($null -ne $dsticketOperationLock) {
                Exit-OnlineDSTicketOperationLock -KubeContext $ctx -Identity $dsticketOperationLock
                $dsticketOperationLock = $null
            }
        }
    }
    Write-Host ""
    Write-Ok "online($Env)部署已提交且临时密钥快照已清理。查看:kubectl --context $ctx get pods -n $K8sNamespace"
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
        # PVE 直进匹配实例:与 matchmaker 同目录同二进制(镜像层全缓存,构建零成本),
        # 仅配置不同(etc/matchmaker-pve.yaml,gen_cluster_config.ps1 生成 matchmaker-pve.yaml)。
        @{ Name = 'matchmaker-pve'; Dir = 'services/matchmaking/matchmaker';   Cmd = 'matchmaker' }
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
    param([string[]]$Only = @(), [switch]$StrictRelease)
    $dockerfile = Join-Path $ProjectRoot 'deploy/services/Dockerfile'
    $v = Get-VersionInfo
    Write-Info "  版本烙印:version=$($v.Version) commit=$($v.Commit) built=$($v.BuildTime)"
    Write-Info "  构建方式:$BuildMode(incontainer=容器内 go build / host=宿主交叉编译再打包)"
    $list = Get-ServiceList
    # -Only 非空时只构建指定服务(含战斗混合模式不构建 ds/hub allocator 镜像,它们跑宿主)。
    if ($Only.Count -gt 0) {
        $known = @($list | ForEach-Object { $_.Name })
        $unknown = @($Only | Where-Object { $known -notcontains $_ })
        if ($unknown.Count -gt 0) {
            throw "未知服务名:$($unknown -join ',')。可选:$($known -join ',')"
        }
        $list = @($list | Where-Object { $Only -contains $_.Name })
    }

    # ===== 离线镜像优先(免联网,拉不到 Docker Hub/加速站的机器双击一键脚本即用)=====
    # deploy/offline-images/pandora-images.tar 由能联网机器 export_images.ps1 -Build 生成并随仓库同步。
    # 判定:本机缺业务镜像 + 无 golang 构建基础镜像(= 这台机器多半构建不了)→ 自动 docker load 离线包,
    # 导入后齐全就跳过 docker build 直接起服务。开发机(有 golang 基础镜像)照常构建最新;强制构建加 -Rebuild。
    $offlineTar = Join-Path $ProjectRoot 'deploy/offline-images/pandora-images.tar'

    # ===== 打包机/运行机专用:强制纯离线,直接用离线包、完全不 docker build =====
    # 这台机器不改代码(打包机 / 内网运行机),不该每次先试构建、DNS 抖了再兜底。
    # 设环境变量 PANDORA_OFFLINE=1(打包机一次性 `setx PANDORA_OFFLINE 1`)即走此路径:
    # 导入离线包 → 校验齐全 → 直接返回,不联网、不构建、不受 Docker DNS 抖动影响。
    if (($env:PANDORA_OFFLINE -eq '1') -and -not $Rebuild -and -not $StrictRelease) {
        if (-not (Test-Path $offlineTar)) {
            throw "PANDORA_OFFLINE=1 但找不到离线包:$offlineTar(请在能联网机跑 export_images.ps1 -Build 生成并同步过来)。"
        }
        Write-Step "纯离线模式(PANDORA_OFFLINE=1):直接导入离线镜像包,跳过所有 docker build"
        Write-Info "  $offlineTar"
        docker load -i $offlineTar
        if ($LASTEXITCODE -ne 0) { throw "离线镜像导入失败:$offlineTar" }
        $missing = @($list | Where-Object { docker image inspect "pandora/$($_.Name):dev" *> $null; $LASTEXITCODE -ne 0 })
        if ($missing.Count -gt 0) {
            Write-Err "离线包导入后仍缺以下镜像:"
            $missing | ForEach-Object { Write-Err "  - pandora/$($_.Name):dev" }
            throw "离线包与服务清单不一致,请在能联网机跑 export_images.ps1 -Build 重新生成并同步。"
        }
        Write-Ok "已用离线镜像包起服务(纯离线,未构建)。要改代码后重建请在开发机做,或临时 -Rebuild。"
        return
    }

    # 离线镜像优先:本机缺业务镜像 + 无「构建能力」时自动导入离线包(免联网)。
    # 「构建能力」两种模式判定不同:host 方式需本机装 Go;incontainer 方式需 golang 基础镜像。
    if (-not $StrictRelease -and -not $Rebuild -and (Test-Path $offlineTar)) {
        $missing = @($list | Where-Object { docker image inspect "pandora/$($_.Name):dev" *> $null; $LASTEXITCODE -ne 0 })
        if ($BuildMode -eq 'host') {
            $canBuild = [bool](Test-CommandExists 'go')
        } else {
            $canBuild = [bool](docker images --format '{{.Repository}}' 2>$null | Select-String -Quiet '(^|/)golang$')
        }
        if ($missing.Count -gt 0 -and -not $canBuild) {
            Write-Step "离线镜像优先:本机缺 $($missing.Count) 个业务镜像且无构建能力(host 缺 Go / incontainer 缺 golang 基础镜像),自动导入离线包(免联网)"
            Write-Info "  $offlineTar"
            docker load -i $offlineTar
            if ($LASTEXITCODE -ne 0) { throw "离线镜像导入失败:$offlineTar" }
            $missing = @($list | Where-Object { docker image inspect "pandora/$($_.Name):dev" *> $null; $LASTEXITCODE -ne 0 })
        }
        if (-not $canBuild) {
            if ($missing.Count -eq 0) {
                Write-Ok "业务镜像已齐全(离线导入/已存在),本机无构建能力,跳过构建直接使用。要强制构建加 -Rebuild。"
                return
            }
            Write-Err "离线导入后仍缺以下镜像,且本机无构建能力(host 缺 Go / incontainer 缺 golang 基础镜像):"
            $missing | ForEach-Object { Write-Err "  - pandora/$($_.Name):dev" }
            throw "请在能联网的机器跑 export_images.ps1 -Build 重新生成 deploy/offline-images/pandora-images.tar 并同步过来。"
        }
    }

    # 分派:方案 B(宿主编译 → 打包)/ 方案 A(容器内 go build)。
    if ($BuildMode -eq 'host') {
        Build-Images-Host -List $list -Version $v
    } else {
        Build-Images-InContainer -List $list -Version $v -Dockerfile $dockerfile -OfflineTar $offlineTar -StrictRelease:$StrictRelease
    }
}

# ===== 方案 A:容器内 go build(deploy/services/Dockerfile,环境隔离,无需本机 Go)=====
function Build-Images-InContainer {
    param([array]$List, $Version, [string]$Dockerfile, [string]$OfflineTar, [switch]$StrictRelease)
    $v = $Version

    # 国内镜像:基础镜像仓库前缀 + go 模块代理,避免卡在 Docker Hub / proxy.golang.org。
    # 默认走国内加速;可用 PANDORA_BASE_REGISTRY / PANDORA_GOPROXY 覆盖(官方仓库填 docker.io)。
    $baseRegistry = $env:PANDORA_BASE_REGISTRY
    if (-not $baseRegistry) {
        # 本机已有 golang 基础镜像时,优先用本地(打成 Dockerfile 需要的 docker.io/library/golang:<ver> tag),
        # 彻底免联网拉基础镜像;本地没有才回退国内加速站(需网络)。
        $goVer = '1.26.5'
        $m = Select-String -Path $Dockerfile -Pattern '^ARG\s+GO_VERSION=(\S+)' -ErrorAction SilentlyContinue | Select-Object -First 1
        if ($m) { $goVer = $m.Matches[0].Groups[1].Value }
        $wantGo = "docker.io/library/golang:$goVer"
        docker image inspect $wantGo *> $null
        if ($LASTEXITCODE -eq 0) {
            $baseRegistry = 'docker.io'
        } else {
            $localGo = (docker images --format '{{.Repository}}:{{.Tag}}' 2>$null | Select-String '(^|/)golang:' | Select-Object -First 1)
            if ($localGo) {
                $src = "$localGo".Trim()
                Write-Info "  本机无 $wantGo,发现 $src,自动打标复用(免联网拉基础镜像)。"
                docker tag $src $wantGo 2>$null
                $baseRegistry = 'docker.io'
            } else {
                $baseRegistry = 'docker.m.daocloud.io'
            }
        }
    }
    $goproxy = $env:PANDORA_GOPROXY
    if (-not $goproxy) {
        if ($env:GOPROXY -and $env:GOPROXY -notmatch 'proxy\.golang\.org') { $goproxy = $env:GOPROXY }
        else { $goproxy = 'https://goproxy.cn,direct' }
    }
    Write-Info "  基础镜像仓库:$baseRegistry(可用 PANDORA_BASE_REGISTRY 覆盖;官方用 docker.io)"
    Write-Info "  Go 模块代理:$goproxy(可用 PANDORA_GOPROXY 覆盖)"

    $offlineFallbackDone = $false
    foreach ($svc in $List) {
        Write-Info "  docker build pandora/$($svc.Name):dev ..."
        docker build -f $Dockerfile `
            --build-arg "SERVICE_DIR=$($svc.Dir)" `
            --build-arg "CMD_NAME=$($svc.Cmd)" `
            --build-arg "VERSION=$($v.Version)" `
            --build-arg "GIT_COMMIT=$($v.Commit)" `
            --build-arg "BUILD_TIME=$($v.BuildTime)" `
            --build-arg "BASE_REGISTRY=$baseRegistry" `
            --build-arg "GOPROXY=$goproxy" `
            --label "org.opencontainers.image.revision=$($v.Commit)" `
            -t "pandora/$($svc.Name):dev" $ProjectRoot
        if ($LASTEXITCODE -ne 0) {
            # 构建失败兜底:有离线包就自动导入(拉不到基础镜像/goproxy 挂时),导入后该镜像存在则继续。
            # 导入只做一次(offlineFallbackDone),但之后每个失败服务都复查一次 inspect,避免第 2 个服务误报失败。
            if (-not $StrictRelease -and -not $Rebuild -and (Test-Path $OfflineTar)) {
                if (-not $offlineFallbackDone) {
                    Write-Warn "  构建 $($svc.Name) 失败(多半拉不到基础镜像)。检测到离线镜像包,自动导入兜底..."
                    docker load -i $OfflineTar
                    $offlineFallbackDone = $true
                }
                docker image inspect "pandora/$($svc.Name):dev" *> $null
                if ($LASTEXITCODE -eq 0) { Write-Ok "  使用离线镜像:pandora/$($svc.Name):dev"; continue }
            }
            throw "镜像构建失败:$($svc.Name)"
        }
    }
}

# ===== 方案 B:宿主交叉编译 → 只打包(deploy/services/Dockerfile.prebuilt)=====
# 本机用 GOOS=linux CGO_ENABLED=0 交叉编译 linux/amd64 静态二进制,享受宿主 go build 增量缓存
# (单服务重建秒级、不重下依赖),再用轻量 Dockerfile.prebuilt 把产物 + etc/ 塞进 scratch 镜像。
# 与方案 A 产出同名镜像 pandora/<name>:dev,版本烙印一致,可无缝喂给 compose / export_images。
function Build-Images-Host {
    param([array]$List, $Version)
    if (-not (Test-CommandExists 'go')) {
        throw "host 构建方式需要本机安装 Go(1.26.5+)。装好后重试,或改用 -BuildMode incontainer(容器内编译,无需本机 Go)。"
    }
    $v = $Version
    $prebuiltDockerfile = Join-Path $ProjectRoot 'deploy/services/Dockerfile.prebuilt'
    $stageRoot = Join-Path $ProjectRoot 'run/docker-build/prebuilt'
    if (-not (Test-Path $stageRoot)) { New-Item -ItemType Directory -Path $stageRoot -Force | Out-Null }

    # Dockerfile.prebuilt 只用 golang 镜像取 CA 证书 / 时区(不编译,层缓存命中);沿用本地已有基础镜像。
    $baseRegistry = $env:PANDORA_BASE_REGISTRY
    if (-not $baseRegistry) {
        $goVer = '1.26.5'
        $m = Select-String -Path $prebuiltDockerfile -Pattern '^ARG\s+GO_VERSION=(\S+)' -ErrorAction SilentlyContinue | Select-Object -First 1
        if ($m) { $goVer = $m.Matches[0].Groups[1].Value }
        $wantGo = "docker.io/library/golang:$goVer"
        docker image inspect $wantGo *> $null
        if ($LASTEXITCODE -eq 0) {
            $baseRegistry = 'docker.io'
        } else {
            $localGo = (docker images --format '{{.Repository}}:{{.Tag}}' 2>$null | Select-String '(^|/)golang:' | Select-Object -First 1)
            if ($localGo) {
                $src = "$localGo".Trim()
                Write-Info "  本机无 $wantGo,发现 $src,自动打标复用(免联网拉基础镜像)。"
                docker tag $src $wantGo 2>$null
                $baseRegistry = 'docker.io'
            } else {
                $baseRegistry = 'docker.m.daocloud.io'
            }
        }
    }
    # 宿主 go build 拉模块用的代理(默认 goproxy.cn;尊重已自定义的非公有 GOPROXY)。
    $goproxy = $env:PANDORA_GOPROXY
    if (-not $goproxy) {
        if ($env:GOPROXY -and $env:GOPROXY -notmatch 'proxy\.golang\.org') { $goproxy = $env:GOPROXY }
        else { $goproxy = 'https://goproxy.cn,direct' }
    }
    Write-Info "  基础镜像仓库:$baseRegistry(仅用于取 CA/时区,不编译)"
    Write-Info "  Go 模块代理:$goproxy(宿主交叉编译用)"

    $ld = "-s -w " +
        "-X github.com/luyuancpp/pandora/pkg/version.Version=$($v.Version) " +
        "-X github.com/luyuancpp/pandora/pkg/version.Commit=$($v.Commit) " +
        "-X github.com/luyuancpp/pandora/pkg/version.BuildTime=$($v.BuildTime)"

    foreach ($svc in $List) {
        $stage  = Join-Path $stageRoot $svc.Name
        $etcSrc = Join-Path $ProjectRoot "$($svc.Dir)/etc"
        if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
        New-Item -ItemType Directory -Path $stage -Force | Out-Null

        Write-Info "  [host] go build $($svc.Name)(GOOS=linux CGO_ENABLED=0)..."
        Push-Location (Join-Path $ProjectRoot $svc.Dir)
        $rc = 1
        try {
            $env:GOOS = 'linux'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
            if ($goproxy) { $env:GOPROXY = $goproxy }
            & go build -trimpath -ldflags $ld -o (Join-Path $stage 'app') "./cmd/$($svc.Cmd)"
            $rc = $LASTEXITCODE
        } finally {
            Pop-Location
            Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
        }
        if ($rc -ne 0) { throw "宿主编译失败:$($svc.Name)(可改用 -BuildMode incontainer 在容器内编译)。" }

        # 拷该服务 etc/ 进 stage(镜像 COPY etc/;主配置运行期被集群版覆盖)。
        if (Test-Path $etcSrc) {
            Copy-Item -Recurse -Force $etcSrc (Join-Path $stage 'etc')
        } else {
            New-Item -ItemType Directory -Path (Join-Path $stage 'etc') -Force | Out-Null
        }

        Write-Info "  [host] docker build pandora/$($svc.Name):dev(打包预编译产物,秒级)..."
        docker build -f $prebuiltDockerfile `
            --build-arg "BASE_REGISTRY=$baseRegistry" `
            --label "org.opencontainers.image.revision=$($v.Commit)" `
            -t "pandora/$($svc.Name):dev" $stage
        if ($LASTEXITCODE -ne 0) { throw "镜像打包失败:$($svc.Name)" }
    }
    Write-Ok "宿主编译 + 打包完成($($List.Count) 个镜像)。"
}

# ===== 电脑重启后快速恢复(不重建镜像,尽量把上次状态拉回来)=====
# k8s:minikube stop/重启只是停容器,集群状态+已 load 镜像都还在磁盘,minikube start 即恢复,Pod 自动重建。
function Resume-K8s {
    Write-Step "k8s 快速恢复(电脑重启后:只拉起 minikube + 等 Pod,不重建镜像)"
    $mkProfile = Get-K8sManagedProfile
    minikube -p $mkProfile status *> $null
    if ($LASTEXITCODE -ne 0) {
        Write-Info "minikube 已停,minikube start 中(profile=$mkProfile;集群状态/镜像都在磁盘上,Pod 会自动恢复)..."
        minikube start -p $mkProfile --driver=docker --cpus=4 --memory=6144
        if ($LASTEXITCODE -ne 0) { throw "minikube 启动失败(若集群已损坏,改用 -Reset 全新部署)" }
    } else {
        Write-Ok "minikube 已在运行(profile=$mkProfile)"
    }
    # 上下文锁:恢复流程也会 rollout restart/apply,必须确认 current-context 是本机 minikube。
    $mkCtx = Resolve-MinikubeKubeContext
    $curCtx = (kubectl config current-context 2>$null)
    if ($curCtx -cne $mkCtx) {
        throw "当前 kubectl current-context『$curCtx』不是本机 minikube『$mkCtx』。为防误操作远端集群,-Resume 已中止。请先 kubectl config use-context $mkCtx 再重试。"
    }
    if (-not (Test-KubeContextIsLocalMinikube $mkCtx)) {
        throw "kube-context『$mkCtx』的 apiserver endpoint 不是本机 minikube(疑似同名远端/生产集群)。为防 -Resume 把清单发到远端,已中止。"
    }

    # minikube start 返回与 apiserver 真正可读之间仍可能有短暂竞态；这里只等待 control-plane
    # /readyz，绝不先等待旧 login/allocator Ready。apiserver 一可读就完成所有 fail-closed 审计，
    # 审计通过后才允许任何业务 Ready 等待、apply 或 rollout。
    Wait-KubeApiServerReady -KubeContext $mkCtx
    Write-Step 'Resume 最早只读审计（先于旧业务 Ready/apply/rollout）'
    Assert-ExistingLocalEtcdPersistence -KubeContext $mkCtx
    Assert-NoLegacyDsFleets -KubeContext $mkCtx -LocalDevelopment
    Assert-NoLegacyDSTicketSignerSecret -KubeContext $mkCtx -LocalDevelopment
    Assert-LocalDsAuthBaseline -KubeContext $mkCtx -AllowFreshBootstrap:$false

    Write-Info "等待关键业务 Pod 就绪..."
    try {
        kubectl --context $mkCtx rollout status deploy/etcd          -n $K8sNamespace --timeout=120s; Assert-LastExit 'etcd 恢复就绪'
        kubectl --context $mkCtx rollout status deploy/login         -n $K8sNamespace --timeout=180s; Assert-LastExit 'login 恢复就绪'
        kubectl --context $mkCtx rollout status deploy/ds-allocator  -n $K8sNamespace --timeout=120s; Assert-LastExit 'ds-allocator 恢复就绪'
        kubectl --context $mkCtx rollout status deploy/hub-allocator -n $K8sNamespace --timeout=120s; Assert-LastExit 'hub-allocator 恢复就绪'
    } catch {
        Write-Err "关键 Deployment 未就绪/不存在($($_.Exception.Message))。"
        Write-Err "集群多半未部署过或被清过,-Resume 无可恢复对象。请改用全新部署:"
        Write-Err "  pwsh tools/scripts/start.ps1 -Mode k8s"
        exit 1
    }
    Write-Host ""
    Write-Ok "集群已恢复。"

    # advertise 地址重解析(-AdvertiseHost > env > 局域网 IP)。DHCP 换址后本机 IP 可能已变,
    # 但 Secret 里仍是旧地址 —— 必须重生配置 + 覆盖 Secret + 重启读该地址的 Deployment,
    # 否则 allocator 会继续把旧 IP 发给客户端,导致「恢复成功但连不进 DS」。
    $resumeAdvHost =
        if (-not [string]::IsNullOrWhiteSpace($AdvertiseHost)) { $AdvertiseHost.Trim() }
        elseif (-not [string]::IsNullOrWhiteSpace($env:PANDORA_DS_ADVERTISE_HOST)) { $env:PANDORA_DS_ADVERTISE_HOST.Trim() }
        else { Resolve-LanIp }
    if ([string]::IsNullOrWhiteSpace($resumeAdvHost)) { $resumeAdvHost = '127.0.0.1' }

    Write-Step "刷新 DS advertise 到 Secret(防 DHCP 换址后 allocator 继续发旧地址):$resumeAdvHost"
    # 本地 minikube resume(context=$mkCtx),沿用公开 dev 密钥,显式 -AllowDevSecrets(审核 P1)。
    # DSTicket v2:复用/补齐 dev 钥料(旧集群若缺 Secret/ConfigMap 会在此幂等补上,Fleet 挂载不悬空)。
    $resumeDsTicketKid = Ensure-DsTicketDevKeyMaterial -KubeContext $mkCtx
    & "$ScriptDir/gen_cluster_config.ps1" -AllocatorMode agones -AllocatorAdvertiseHost $resumeAdvHost -AllowDevSecrets `
        -DsAuthMode enforce -DsAuthorityMode redis -DsFenceEtcdEndpoints $script:LocalDsFenceEndpoint `
        -DsFenceKeysetRevision $script:LocalDsFenceKeysetRevision -DsTicketActiveKid $resumeDsTicketKid `
        -DsTicketKeysetRevision 1
    Assert-LastExit '恢复生成本地 k8s enforce/redis DSTicket v2 配置'
    Apply-PandoraConfigSecret -KubeContext $mkCtx -Action 'kubectl apply secret pandora-config(advertise 刷新)'
    # 重新 apply 业务清单(审核 P1 #5):-Resume 若恢复的是「旧版本(挂 ConfigMap)部署的集群」,
    # 光 create Secret 不会让 Pod 改用它 —— 卷源仍指向旧 ConfigMap,新 Secret 完全被忽略。
    # 重 apply 当前清单把卷源纠正为 Secret(:dev tag 不变,未变的 Deployment 报 unchanged 不churn;
    # 卷源从 ConfigMap 改成 Secret 的旧集群会被自动 rollout 迁移),确保新 Secret 真正被挂载。
    $resumeServicesDir = Join-Path $ProjectRoot 'deploy/k8s/services'
    kubectl --context $mkCtx apply -k $resumeServicesDir
    Assert-LastExit 'kubectl apply -k services(Resume:纠正卷源为 Secret)'
    # Secret 以 subPath 挂载不会热感知:按名 rollout restart 全部 20 个业务 Deployment,
    # 让刷新后的 pandora-config Secret(advertise + 其它配置)在每个 Pod 生效(滚动重启零停机)。
    # 必须逐个等待全部 20 个 Deployment；只等 allocator 会把其余服务的失败误报成“全部传播完成”。
    Write-Info "rollout restart 全部业务 Deployment(传播刷新后的 pandora-config Secret)..."
    foreach ($svc in (Get-ServiceList)) {
        kubectl --context $mkCtx rollout restart deploy/$($svc.Name) -n $K8sNamespace
        Assert-LastExit "rollout restart $($svc.Name)(Secret 刷新)"
    }
    foreach ($svc in (Get-ServiceList)) {
        kubectl --context $mkCtx rollout status deploy/$($svc.Name) -n $K8sNamespace --timeout=180s
        Assert-LastExit "rollout status $($svc.Name)(Secret 刷新后未就绪)"
    }
    Remove-LegacyPandoraConfigMapAfterRollout -KubeContext $mkCtx
    Write-Ok "pandora-config Secret 已刷新为 advertise=$resumeAdvHost 并传播到全部业务 Pod(卷源已确保为 Secret)。"

    Write-Step "补部署 DS 面 Envoy + Fleet(让磁盘上的清单改动在恢复后生效)"
    # -Resume 靠 minikube start 拉回的是「停机前」集群内对象;若期间改过 16-ds-envoy.yaml(如 :8444
    # 路由白名单)或 Fleet 清单,光恢复只会拿回旧版本。这里幂等重 apply 补齐(回应审核 P#4:
    # -Resume 不补 Envoy/Fleet)。Envoy 只在启动读一次静态配置,故 rollout restart 强制重读;
    # Fleet 用 :dev 固定 tag,spec 不变则 apply=unchanged,不会打断在跑的 GameServer。
    $resumeAgonesDir = Join-Path $ProjectRoot 'deploy/k8s/agones'
    # 旧集群(部署于 16-ds-envoy 引入前)节点内可能还没有 Envoy 镜像;断网下 Pod pull 必失败。
    # 先走三级来源(节点已有→宿主 load→联网 pull)把镜像备齐,再 apply(回应审核 P1:Resume 断网失败)。
    Ensure-EnvoyImageInMinikube -MinikubeProfile $mkProfile
    kubectl --context $mkCtx apply -f (Join-Path $resumeAgonesDir '16-ds-envoy.yaml'); Assert-LastExit 'apply 16-ds-envoy(恢复)'
    kubectl --context $mkCtx rollout restart deploy/pandora-envoy -n $K8sNamespace; Assert-LastExit 'rollout restart pandora-envoy(恢复:重载 DS 面配置)'
    kubectl --context $mkCtx rollout status  deploy/pandora-envoy -n $K8sNamespace --timeout=120s; Assert-LastExit 'pandora-envoy 恢复就绪'
    # Fleet 清单自带 namespace: default,不加 -n,让清单里的命名空间生效。
    foreach ($resumeFleet in @('20-fleet-battle.yaml', '21-fleet-battle-canary.yaml', '30-fleet-hub.yaml', '31-fleet-hub-canary.yaml')) {
        kubectl --context $mkCtx apply -f (Join-Path $resumeAgonesDir $resumeFleet); Assert-LastExit "apply Fleet $resumeFleet(恢复)"
    }
    Write-Ok "DS 面 Envoy 已重载配置,Fleet 清单已同步。"

    Write-Step "宿主 Envoy 桥接 + UDP 中继(e2e_k8s.ps1)"
    # 重启后宿主 port-forward/envoy/relay 必然全丢,自动重建(DS 镜像还在 minikube 磁盘,跳过 load)。
    $relayBind = if (-not [string]::IsNullOrWhiteSpace($resumeAdvHost) -and $resumeAdvHost -ne '127.0.0.1') { '0.0.0.0' } else { '127.0.0.1' }
    # 恢复路径同样锁死宿主 DS 面,避免继承用户环境中的 0.0.0.0。
    $env:PANDORA_DS_EDGE_BIND_HOST = '127.0.0.1'
    & pwsh -NoProfile -ExecutionPolicy Bypass -File (Join-Path $ScriptDir 'e2e_k8s.ps1') -SkipImageLoad -MinikubeProfile $mkProfile -KubeContext $mkCtx -RelayBindHost $relayBind
    Assert-LastExit "宿主桥接/中继(e2e_k8s.ps1);修复后可单独重跑:pwsh tools/scripts/e2e_k8s.ps1 -SkipImageLoad"
    if ($relayBind -eq '0.0.0.0') {
        Write-Ok "真 DS 闭环已恢复(局域网):内网其它机器客户端连 ${resumeAdvHost}:8443 即可(需防火墙放行 TCP 8443 + UDP 7000-8000;宿主 8444 仅回环)。"
    } else {
        Write-Ok "真 DS 闭环已恢复:客户端连 127.0.0.1:8443 即可(仅本机)。"
    }
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
            Write-Info "含战斗混合版恢复:重新拉起 18 业务容器 + 2 宿主 allocator。"
            Invoke-Battle
        }
        { $_ -in 'docker', 'intranet' } {
            Write-Info "重启已停的容器(不加 --build,不重建镜像)..."
            # 边缘绑定必须与全新启动一致,否则恢复后 Envoy 退回默认回环:
            #   intranet → 0.0.0.0(对局域网开放,admin 9901 仍恒绑本机);docker → 127.0.0.1(仅本机)。
            # 该环境变量要在 dev_up.ps1(起 envoy 的 infra compose)之前导出才生效。
            $lan = if (-not [string]::IsNullOrWhiteSpace($AdvertiseHost)) { $AdvertiseHost } else { Resolve-LanIp }
            if ($Mode -eq 'intranet') {
                Set-EdgeBindHost '0.0.0.0'
                if ([string]::IsNullOrWhiteSpace($lan)) {
                    Write-Warn "未能解析内网 IPv4;Envoy 客户端面已绑 0.0.0.0,但请用 -AdvertiseHost 显式告知客户端连哪个地址。"
                }
            } else {
                Set-EdgeBindHost '127.0.0.1'
            }
            & "$ScriptDir/dev_up.ps1"
            if ($LASTEXITCODE -ne 0) { throw "基础设施恢复失败(dev_up)" }
            docker compose -f $ComposeServices up -d
            if ($LASTEXITCODE -ne 0) { throw "业务服务容器恢复失败" }
            Write-Ok "$Mode 容器已恢复(Envoy 绑定 $env:PANDORA_EDGE_BIND_HOST)。"
            if ($Mode -eq 'intranet' -and -not [string]::IsNullOrWhiteSpace($lan)) {
                Write-Host "       内网地址  https://${lan}:8443(客户端面)" -ForegroundColor Green
                if ($ExposeDsFace) { Write-Host "       DS 面      ${lan}:8444(-ExposeDsFace 已开放)" -ForegroundColor Green }
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
            # 只销毁本次运行钉死的 minikube profile(与 Invoke-K8s 重建目标严格一致,避免删 A 建 B)。
            # minikube delete -p 只作用于本地 minikube,不触碰远端集群;但仍先确认该 profile 的 kube-context
            # 指向本机 minikube(而非同名远端集群),再删,防「Get-ActiveMinikubeProfile 回退 minikube」误删默认集群。
            $mkProfile = Get-K8sManagedProfile
            $contexts = @(kubectl config get-contexts -o name 2>$null)
            if ($contexts -ccontains $mkProfile -and -not (Test-KubeContextIsLocalMinikube $mkProfile)) {
                throw "reset 目标『$mkProfile』的 kube-context 指向的不是本机 minikube(疑似同名远端/生产集群),为防误删已中止。"
            }
            # kube-context 缺失不等于本地 profile 已不存在(损坏/半删除集群常见)。minikube delete -p
            # 只操作本机 profile,此时仍要执行，才能兑现 Reset 的“彻底清掉再重建”语义。
            Write-Warn "将 minikube delete -p $mkProfile(仅销毁本机集群『$mkProfile』,已 load 镜像一并清掉),然后全新部署。"
            minikube delete -p $mkProfile 2>$null
            Assert-LastExit "minikube delete -p $mkProfile"
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
            Write-Step "业务服务容器(18 个,不含 ds/hub allocator)"
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

if ($BuildOnly) {
    Write-Step "只构建业务镜像(离线打包用,不启动任何服务;构建方式=$BuildMode$(if ($Only.Count -gt 0) { ";只构建 $($Only -join ',')" }))"
    Build-AllImages -Only $Only
    Write-Ok "业务镜像构建完成。可用 tools/scripts/export_images.ps1 打包导出。"
    exit 0
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
