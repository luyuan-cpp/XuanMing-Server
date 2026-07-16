# DS Recovery / Placement：Claude Code 只读审核清单

> 审核对象：`F:\work\XuanMing-Server` 当前 Git 工作树与
> `F:\work\Pandora-Client-SVN` 当前 SVN 工作副本。
>
> 审核方式：**只读**。Claude Code 不得修改、格式化、生成、还原或提交任何文件；先读完整 diff，再主动构造
> 反例。若需执行会重写 generated files 的命令，交给 Codex 在记录快照后运行。
>
> 状态：实现已进入当前工作树，最终同一快照的本地 Go/Proto/UE 验证已记录；已知代码反例已修复，
> 但真实 Redis topology-change lease provider 尚未接线，所有激活/普通线上发布入口当前故意 fail-closed。
> Claude Code 独立反证与真实环境门仍待完成。本文不是上线批准，也不表示真实集群已验证。

## 1. 唯一验收不变量

任意 RPC 取消、响应丢失、客户端切后台/杀进程、服务进程崩溃、Redis/K8s/Agones 暂时不可用后：

> 玩家要么只能进入唯一的权威 DS，要么停在明确可重试/可登录状态；绝不能双 Admission，也不能把
> UNKNOWN 当成“不在 Battle”，更不能静默等待业务 TTL 猜测自愈。

审核每条路径都要回答四个问题：

1. 谁持有唯一 canonical state，线性化点在哪里？
2. 外部副作用已提交但 ACK 丢失时，重放是否仍指向同一 operation/identity？
3. stale/future/partial/corrupt identity 是否在**任何** seat、ticket、spawn、READY、claim 删除之前失败？
4. 派生索引丢失后，是否能从 canonical state 重建，而不是靠 TTL 或玩家再次请求碰运气？

## 2. Claude Code 必须重点反驳的实现

### 2.1 placement 与 physical source departure

检查：

- `proto/pandora/locator/v1/locator.proto` field 24~38、source proof enum 101/102 与
  `ConfirmPlacementSourceDeparture` 是否全部 additive，旧 tag 未复用。
- `Begin` 是否在同一 CAS 捕获 immutable source binding + exact physical target；Retarget 是否清活动确认。
- 非 bootstrap `CommitPlacementAdmission` 是否无条件要求当前 exact source confirmation；`last_*` audit 字段是否
  永远不能再次授权。
- Hub source proof 是否只由 exact reservation/session absence 或 immutable UID teardown 产生；Redis key 删除、
  heartbeat 漏报、drain grace 或 Fleet scale-in 请求不得冒充 Pawn 离场。
- Battle source proof 是否在 durable departure journal + UE 完整 world census 后产生；locator ACK 丢失是否继续
  返回 retryable，并重用稳定 `departure_id`。
- Hub 与 Battle proof domain/secret 是否不可互换；Hub allocator 不应拿到 Battle departure secret。

必须构造：旧 version、旧 operation、同名新 UID、旧 assignment/allocation、proof type 互换、Begin/Retarget 后旧
proof 重放、physical journal commit 但 locator ACK 丢失、locator confirm commit 但 RPC ACK 丢失。

### 2.2 locator 升级与密钥启动门

检查：

- `services/runtime/player_locator/cmd/placement_preflight` 是否对 Redis Cluster 使用 `ForEachMaster`；坏 key、
  SCAN/GET 错误、protobuf 损坏、审计中 key 消失是否全都非零退出且零写入。
- STABLE 是否要求完整 exact current target、无 active source marker；audit-only `last_source_departure_*` 是否
  只允许“legacy 全空”或“完整且匹配当前 committed version/op”的四元组，而不会因缺旧 admission/proof/timestamp
  把本可安全 Begin 的 legacy STABLE 永久卡死。非 bootstrap PENDING 是否要求 immutable source，且 departure
  marker 只能是“全空的 canonical Begin-before-Confirm”或“与 physical route 完全匹配的 confirmed proof”；
  bootstrap 例外是否足够窄。不得把合法未确认 PENDING 拦在每次 Pod restart 的 init gate 外。
- `deploy/k8s/services/services.yaml` 中 player-locator 是否为 `Recreate`；online prod 真正承流的 canonical green
  replacement 是否也强制 Recreate + 与主容器同一 digest 的 preflight init，并在终态回读，不能只保护 dormant blue。
- locator 是否在监听端口前要求五把 authority key 全部 ≥32 bytes、pairwise distinct，且不复用 DS callback key。
  五把为 account-bootstrap、match-start、battle-exit、hub-transfer、battle-departure；Hub departure 只与
  HubTransfer 共用 owner key，但 canonical HMAC domain 必须分离。
- 生成器/manifest contract 是否只把 BattleDeparture key 投递给 ds_allocator + player-locator。

### 2.3 Battle active canonical reconciler

检查 `battle_active_reconciler.go`：

- Cluster 模式是否扫描**每个 master**，而不是 `UniversalClient.Scan` 的单节点视图。
- scan 全部成功后才补 derived ZSET，坏 record 不应留下半轮 repair。
- `ZADD NX` 是否保留已有 score=0 的 quarantine/release 立即唤醒。
- permanent recovery fences、allocation uncertain、release pending、empty tombstone、preactive release 是否重建；
  ended 与已转有界 TTL 的完成 audit 是否不复活；future state 是否 fail-closed。
- 服务启动是否立即跑一次，不等首个 ticker。

### 2.4 Matchmaker durable formation/allocation/release

逐个反证：

1. auto-confirm/solo 是否先落 fully-accepted CONFIRM；ticket union 与 canonical roster 完全相等、全部 player claim
   durable 前，不能进入 ALLOCATING，更不能外调 AllocateBattle。
2. 已存在的 legacy/半写 ALLOCATING 是否在 worker 外调前再次经过 exact discovery gate。
3. ds_allocator 返回 `ERR_UNAVAILABLE` 是否保持 unknown/retryable；只有明确
   `ERR_DS_ALLOCATION_FAILED` / `ERR_DS_NO_AVAILABLE` 才能进入 definitive failure 逻辑。
4. Redis transient error 是否保留/re建 active index；partial graph 即使 repair 报错也必须重新出现在 active，供
   durable fail/requeue，而不是永久失联。
5. Release 的 ticket phase 是否先全部 compare-delete 成功才进入 claim phase；canonical match 缺失时是否只接受
   fallback roster + claim→ticket→match/member 的 exact proof，缺 ticket/歧义 fail-closed。
6. claim 清理是否 compare-delete，确保旧 release 与新 StartMatch ABA 竞争时不删除新 claim。
7. ticket canonical 已不存在但前次 queue `ZREM` 失败时，`DeleteTicketIfMatch` 重放是否仍幂等移除 stale queue member。
8. BattleResult match-release outbox 在 downstream commit-but-ACK-lost 时是否保留并重放；幂等 result replay 是否
   恢复缺失 row/把 deferred row 置为立即 due，而不改 immutable operation payload。

### 2.5 Hub capacity ledger（§7.16.4）

检查：

- 容量是否由逐 assignment reservation + connected ownership 条数决定；heartbeat reported count/list 只作审计。
- reservation 的绝对期限是否覆盖 ticket TTL+leeway，整键 TTL 是否覆盖最晚 item；较短 shard TTL 不得提前删。
- Reserve、Admission consume、Departure、exact release 是否与 auth/shard/ledger 在同 `{pod}` CAS；同 identity 重放
  幂等，不同 identity/sequence 零变更。
- connected owner 是否无时间 TTL；Heartbeat 报 0、Logout map 已清或 assignment release 都不能直接删除。
- live owner Release 是否只返回 DepartureRequired；exact Departure 或 immutable UID teardown 才能清。
- 路由门是否要求 reported `MaxPlayers == shard.capacity`；实报连接多于 ledger ownership 时是否阻断新分配，
  不能把未知旧连接当空位。

必须覆盖 capacity=1 并发、reservation 后多轮 0-player heartbeat、ACK loss、旧 Logout、新 admission takeover、
UID 重建、quarantine/drain、assignment CAS 失败补偿与 future unknown protobuf fields。

### 2.6 UE Coordinator 与物理 census

检查 SVN diff，不只看后端生成头：

- `MyAccountModel.cpp`、`MyMatchModel.cpp` 中不得调用 DS `ClientTravel`；唯一调用点应在
  `MyDsRecoveryCoordinator.cpp`。
- 所有异步回调是否同时核 generation、request sequence、expected phase；前台、World BeginPlay、
  OnNetworkFailure、OnTravelFailure、push 重订阅是否回到权威 `ResumeContext`。
- 30s timer 是否只改变 UI/退避；不得直接 Issue Hub ticket 或清 Battle fence。
- Battle READY 无 PlayerController 是否保留 pending target，而不是提前去重。
- Hub fence/recovery attempt 是否保持到 exact Admission committed ACK；World BeginPlay 不能冒充 placement commit。
- Battle census 是否包含 pending/admitted claims、全部 Controller、全部 Pawn、World iterator 与 weak Pawn ledger；任一
  无法归因对象是否让整份 census fail-closed。
- exact eviction 是否先完整 ABA 检查，再取消/Kick/Destroy；旧 version/op/UID/allocation 不得踢新 admission。

### 2.7 StartMatch preflight、allocation abort 与 Model-B 物理回收

检查：

- `StartMatch` 是否在建 operation、claim、ticket/queue 前先从 durable placement 证明 exact
  `STABLE_HUB`；allocation worker 在外调 Agones 前是否再做一次同样的权威预检。不得把 placement
  缺失、partial PENDING、已绑 Battle 或 Redis error 当作可分配。
- Match 的 `REQUESTING → ABORTING → FAILED` 是否使用 exact operation + full target CAS；READY 只能从
  exact REQUESTING 发布，Placement/HubDeparture/签票前各自是否重新过 CAS fence。abort RPC 结果
  UNKNOWN 时必须保留 ALLOCATING+ABORTING、ticket/claim/active，不能抢先 FAILED/requeue。
- `AbortPreactiveBattle` 是否使用独立生产 HMAC key，canonical 是否绑 full payload；无 digest 的旧
  metadata 不得与新验签互通，nonce 只能在 payload 比对成功后消费。该 key 不得缺失、使用
  dev 默认值或与任何 placement/login/DS callback key 复用。
- allocator 是否先持久 exact abort/preactive fence，再按 GameServer UID + Pod UID 物理回收；只有 K8s
  明确证明旧两个 UID 均已消失才能写 teardown proof。late abort 必须同时看到 exact teardown
  proof 与 Kafka ACK 后的 full-target lifecycle marker，不得凭单份证明或 TTL 合成成功。
- `RELEASED` journal 是否只表示物理回收已证明，而不表示 Redis authority cleanup ACK 已收到；重放
  必须重读 exact auth+battle，剩余 key 相等才补 TTL/清 active，任何漂移都 fail-closed。
- 新 allocation 是否在 usecase 与 durable finalize 两层都拒绝空 `pod_uid`；旧记录回填只能
  使用 exact GameServer name+UID+allocation_id 与 owned Pod GET。completed、empty heartbeat、stale sweep、
  abort、preactive 都必须在 terminal/permanent transition 之前完成该回填。
- pre-Prepare `instance_epoch=0` 不得调用要求 credential epoch 的通用 release；专用路径仍必须
  先有 exact preactive fence 与持久 Pod UID，unknown/ACK loss 保留 fence，明确成功才 purge，且不能
  伪造未曾存在的玩家 teardown proof。
- 搜索所有 `BattleStorageRecord` Redis content write：23 个生产写点（包括独立
  `battle_auth_quarantine` 命令）是否都进入不可逆 strict mutation gate；新/重写是否必须带完整 PodUID，
  legacy 是否只允许 exact PodUID backfill，PodUID 是否不可变且 unknown protobuf bytes 保留。构造从另一个
  repo constructor/CLI 绕过主进程初始化的反例。
- PodUID preflight 是否用 exact ACL GETUSER contract 证明唯一只读用户/唯一 password hash/仅
  `%R~pandora:ds:battle:*`，并拒绝未认证 PING；Cluster 是否证明完整 16384-slot master 集、扫描前后同
  topology、跨 master 同 key；standalone 是否同时证明 `cluster_enabled:0` 和 `role:master`。不要把
  DRYRUN、一次 `CLUSTER SLOTS` 或 ConfigMap marker 当权限/拓扑执行锁。
- required target raw value 是否只能为 `2@ds-auth-v2-pod-uid-write-invariant-v1`，五个生产 writer 的 exact
  feature policy 是否同时绑定初读、capability CAS、watch 和 activation record；旧 numeric epoch=2 binary
  与新 binary 面对裸 `2` 是否都 fail-closed。
- CAS 后临时 ACL 用户是否由独立 admin identity 固定 DELUSER，并以 WHOAMI/GETUSER absent 双遍回读；
  部分节点已删、响应丢失或进程崩溃后是否可用同 RunId 幂等继续，cleanup pending 是否禁止发布完成。
- 当前没有真实 topology-change/failover/reshard lease provider 时，fresh/retry Activate 是否在任何
  create/patch/scale 前零写阻断，CAS 前是否再阻断；Go CLI/core CAS 与普通 online build/push/apply 是否
  都没有直调旁路。这个外部阻断不得被审成“已有 ConfigMap/topology digest 所以可放行”。

必须构造：StartMatch preflight 后 placement 变更、Allocate ACK 丢失后 abort 与 READY 并发、abort RPC ACK
丢失、teardown proof/Kafka ACK/lifecycle marker 任意单独缺失、RELEASED 后 authority cleanup ACK 丢失、旧
`pod_uid` 空且 K8s 对象缺失/同名重建，以及 epoch=0 DELETE 超时。最后一类不得猜 UID；
如果无法从不可变证据恢复，必须保留明确 retryable fence，并在发布前迁移审计中清零，不得
把它记为自动收敛。

## 3. 明确拒绝的“假修复”

- 把 30 秒改成更长，或把 presence/assignment/claim TTL 当业务终态。
- locator key 缺失、短 TTL presence 缺失、Redis error 或 roster 漂移时默认可进 Hub。
- 先 AssignHub/签票/READY/spawn，再异步补 placement 或 physical departure。
- Redis connected ledger 删除、GameServer DELETE accepted、Fleet scale-in 或 heartbeat player_count=0 当作物理离场。
- READY 无 PC 时清 pending；ReturnHub 申请票前清 match/fence；共享 multicast 迟到回调仍可 travel。
- 仅给 RPC 套 `context.Background()` 而没有 durable operation/worker/index repair。
- Redis transient 时移除 active；allocator unavailable 直接 FAILED；Release 失败返回 OK 等 TTL。
- canonical ticket 已删就跳过 stale queue ZSET 清理。
- 用两个容量整数代替逐 reservation identity，或让 heartbeat 覆盖 reservation。
- 用人工滚动顺序替代 locator `Recreate + preflight`，或让五把 placement key 复用一把方便配置。

## 4. 发布顺序硬门

下列顺序是安全协议的一部分，不能并行交换：

1. **冻结协议与产物**：完成 additive proto 生成、Go/C++/UE codec 同步；旧 tag 不复用。先发布只读 reader 与
   能理解新字段但尚未开启 strict writer 的版本。
2. **先升级物理 source DS 能力**：部署 Hub/Battle census、exact eviction、Departure ACK 与 allocator 客户端；
   排空不支持新 proof 的旧 Hub/Battle DS。严格门开启后旧 DS 只能造成 unavailable，不能获准 bypass。
3. **部署 durable worker/reconciler/outbox**：确保 Matchmaker、ds_allocator、BattleResult 的 canonical worker
   ownership 唯一；新旧推进器不能同时作为同一 operation writer。先观察 shadow 指标与 derived-index repair。
4. **先上线 PodUID 兼容 reader/backfill，再做三阶段发布证明**：严格激活前的 additive/epoch=1
   rollout 必须已运行新 ds_allocator，让 heartbeat/result/stale/abort/preactive 路径用 exact K8s 身份
   主动回填存量 `pod_uid`。activation 先跑同 digest/RunId 的 `prepare` 只读 Job；旧 blue writer=0、
   capability 清空且 drain marker 已建后，必须再跑 `drained` Job，且 creation/completion 都不早于
   marker；green exact capability/strict writer 启动后、Service 仍指向 blue 时再跑 `final` Job。
   final PASS 之前不得 switch/CAS；epoch=2 审计只能读原三份证据，禁止事后建 Job。CAS 后必须完成临时
   Redis ACL 用户的 exact cleanup。当前真实 topology-change lease provider 未接线，所以该步骤在首个写入前
   必须阻断，不能人工跳过。
5. **停旧 locator writer并做迁移审计**：暂停会产生新 transition 的入口，运行全 master
   `placement_preflight`。任何 finding 均阻断；先让遗留 operation 正常完成/补偿，禁止手工伪造 proof。
6. **Recreate player-locator**：注入五把独立 authority key，确认坏/缺/复用 key 会启动失败；确认旧 Pod 已为 0
   后再启动新 writer。availability 可短暂下降，不能回退 RollingUpdate。
7. **开启服务端 strict gate**：Hub/Battle ticket、READY 与 Admission 必须看到当前 physical confirmation；
   UNKNOWN 全部 retryable、零 seat/ticket/spawn。先服务端，后客户端。
8. **灰度 UE Coordinator**：验证唯一 travel writer、foreground/network/travel recovery、Hub/Battle physical
   census；旧客户端即使重新登录也不能绕服务端门。
9. **真实故障矩阵全绿后再放量**：Redis Cluster master 故障、K8s/Agones UID teardown、allocator/locator ACK
   loss、真实 UDP Admission、移动端前后台/断网/杀进程。之后才能删除旧 fallback/共享 callback。

若任一步需要回滚，必须保持“不识别新 proof 的 writer 不再接流量”。回滚不能恢复一个会绕 Commit
source-departure 门的旧 locator，也不能把已经启用的新 placement record 交给旧 writer。

## 5. 必跑命令与结果记录

所有结果必须来自**最终合并后的同一 Git/SVN 快照**。不要把此前某轮局部 PASS 复制为本轮 PASS。

### 5.1 Server targeted tests

```powershell
Set-Location F:\work\XuanMing-Server

go test ./pkg/placement/... ./services/runtime/player_locator/... ./services/account/login/... -count=1
go vet  ./pkg/placement/... ./services/runtime/player_locator/... ./services/account/login/...

go test ./services/battle/ds_allocator/... -count=1
go vet  ./services/battle/ds_allocator/...

go test ./services/battle/hub_allocator/... -count=1
go vet  ./services/battle/hub_allocator/...

go test ./services/matchmaking/matchmaker/... ./services/battle/battle_result/... -count=1
go vet  ./services/matchmaking/matchmaker/... ./services/battle/battle_result/...
```

结果：`PASS`。上述核心模块与新增 abort/PodUID/epoch=0 回归均通过 `go test -count=1`
与 `go vet`；高风险并发定向用例另以 `-count=30` 通过。

### 5.2 全 Go workspace

仓库根没有 `go.mod`，不能把根目录 `go test ./...` 当全量验证。按 `go.work` 的每个 module 执行：

```powershell
Set-Location F:\work\XuanMing-Server
$workspace = go work edit -json | ConvertFrom-Json
foreach ($use in $workspace.Use) {
    Push-Location $use.DiskPath
    try {
        go test ./... -count=1
        if ($LASTEXITCODE -ne 0) { throw "go test failed: $($use.DiskPath)" }
        go vet ./...
        if ($LASTEXITCODE -ne 0) { throw "go vet failed: $($use.DiskPath)" }
    }
    finally { Pop-Location }
}
```

结果：`PASS`。`go.work` 共 29 个 module，`go test ./... -count=1 -mod=readonly` 与
`go vet -mod=readonly ./...` 均为 29/29 通过，无遗漏/失败模块。Go 为 `go1.26.5 windows/amd64`。

Race 只在 `go env CGO_ENABLED` 为 `1` 且本机 race toolchain 可用时执行高风险模块；否则明确写
“未执行：环境限制”，不能写 PASS：

```powershell
go test -race ./services/runtime/player_locator/... ./services/battle/ds_allocator/... `
  ./services/battle/hub_allocator/... ./services/matchmaking/matchmaker/... `
  ./services/battle/battle_result/... -count=1
```

结果：`BLOCKED-ENV`。`CGO_ENABLED=0`，且本机无 `gcc`；未执行 race，不记为 PASS。

### 5.3 Proto、生成物与脚本合同

```powershell
Set-Location F:\work\XuanMing-Server
pwsh tools/scripts/proto_gen.ps1 -Lint
pwsh tools/scripts/tests/gen_cluster_b1_contract_test.ps1
pwsh tools/scripts/tests/online_manifest_contract_test.ps1
kubectl kustomize deploy/k8s/services | Out-Null
kubectl kustomize deploy/k8s/overlays/online | Out-Null
git diff --check
```

需要重生时，先保存 generated diff 文本，执行 `pwsh tools/scripts/proto_gen.ps1 -Cpp`，再确认重生前后 diff
完全相同；不要用“工作树本来就有 diff”误判漂移：

```powershell
$before = git diff --no-ext-diff -- proto/gen/go proto/gen/cpp | Out-String
pwsh tools/scripts/proto_gen.ps1 -Cpp
$after = git diff --no-ext-diff -- proto/gen/go proto/gen/cpp | Out-String
if ($before -cne $after) { throw "proto generated output drifted" }
```

结果：

- proto lint/generate determinism：`PASS`。`-Cpp` 连续两次生成 48 个 Go/28 个 C++ 产物，
  生成前/两次后的 diff SHA-1 均为 `5a9dec0964714f104165d6997a0208ebf0e13688`。
- `gen_cluster_b1_contract_test.ps1`：`PASS`。
- `ds_auth_activation_contract_test.ps1`：`PASS`。
- online manifest contract：`BLOCKED-ENV`。当前 kubeconfig 指向 `https://127.0.0.1:59751`，
  `kubectl create --dry-run=client --validate=false` 仍尝试连该 API 并被拒；未观察到合同断言失败。
- services/online kustomize：`PASS`，两个 overlay 均成功渲染。
- PowerShell AST：`PASS`，8 个本次修改/新增脚本全部解析成功。
- diff check：`PASS`，只有 CRLF/LF 转换 warning，无 whitespace error。

### 5.4 生产发布前只读 preflight

#### 5.4.1 placement

只对明确指定的目标环境配置运行；命令会读生产 Redis，但不写数据：

```powershell
Push-Location F:\work\XuanMing-Server\services\runtime\player_locator
try {
    go run ./cmd/placement_preflight `
      -conf <rendered-production-locator-config.yaml> `
      -timeout 10m -scan-count 1000
}
finally { Pop-Location }
```

通过条件：输出 `PASSED`、visited nodes > 0、findings = 0，并保存完整审计日志/时间/Redis cluster identity。

结果：`[未在本地替生产执行；发布硬门待运维记录]`

#### 5.4.2 Model-B legacy PodUID

`ds_allocator` serving image 新增 one-shot `-pod-uid-release-preflight`，它在 repo/worker/listener 接线前
退出。它从 credential-free immutable 配置读取目标，以独立高熵只读 ACL 用户运行；ACL GETUSER 必须证明
exact flags/password/commands/read-key-pattern/channels/selectors，未认证 PING 必须 NOAUTH。Cluster 必须证明
完整 16384-slot master 拓扑并在每个 master 做 PING/SCAN/GET，前后 topology digest/master set 相同；
standalone 必须证明 `cluster_enabled:0 + role:master`。坏 key、跨 master 重复 key、key 审计中消失、
SCAN/GET 错误、protobuf 损坏/unknown fields、future state、partial identity、非 RFC4122 canonical UUIDv4
allocation_id，或已有 exact GameServer 身份但 `pod_uid` 空，都使命令非零。仅有 allocation_id 而尚无
任何物理字段的 canonical `allocation_uncertain` 单独计数，不伪装成 exact 记录。

该 ACL 身份能执行 ACL GETUSER 并扫描 key name，安全边界要求 activation-only 高熵凭据、同信任域或专用
Redis，并在 CAS 后强制删除；不能把它当常驻运行身份。

生产证据不能用本地 `go run` 代替。`tools/scripts/activate_ds_auth.ps1` 设计为用受审的 green immutable
image/config 创建同 RunId 的 `prepare`、`drained`、`final` 三个 Job：drained 必须在 blue writer 归零和
drain marker 后、green 启动前完成；final 必须在 green exact capability/strict writer 已启动但 Service 仍
指向 blue 时完成。每一阶段都输出 exact phase：

```text
pod_uid release preflight PASSED: run_id=<run> phase=<prepare|drained|final> image_digest=sha256:<digest> redis_config_identity=sha256:<digest> redis_target_identity=sha256:<digest> redis_topology=<standalone|sentinel|cluster> redis_acl_user=pandora-pod-uid-release-preflight-ro visited_masters=<n> visited_keys=<n> decoded_records=<n> allocation_uncertain=<n> findings=0; no data was modified
```

epoch=2 Audit/幂等重跑只能验证同 RunId/digest/config/Redis identity 的既有三份 Job，缺失时必须拒绝，
不得事后创建证据。Job 只做审计，不修复；可恢复的旧 record 必须由激活前的 additive/epoch=1 新代码用
exact K8s 身份回填。对象已丢失的记录需可审计迁移/清退，不得伪造 UID。target etcd raw value 固定为
`2@ds-auth-v2-pod-uid-write-invariant-v1`；CAS 后临时 ACL 用户必须被独立 admin 删除并回读 absent，
cleanup-complete marker 缺失时普通发布仍阻断。

本地结果：`PASS`。分类/只读/全 master 单测、ds_allocator 全包 test/vet、PowerShell AST 与
`ds_auth_activation_contract_test.ps1` 全通过。生产三阶段 Job 未执行。更重要的是，仓库目前没有能覆盖
preflight→CAS 全窗口的真实 Redis topology-change/failover/reshard lease provider 与信任根；Activate、
Go CLI/core CAS 和 ordinary online release 已全部 fail-closed。这是尚未接线的外部控制面发布硬阻断，
不是 `BLOCKED-ENV` 测试，也不能用 ConfigMap 代替。

### 5.5 UE build 与 Automation

```powershell
Set-Location F:\work\Pandora-Client-SVN
cmd /c Tool\Build\BuildEditor.bat

& "$env:UE_ENGINE_DIR\Engine\Build\BatchFiles\Build.bat" `
  PandoraServer Win64 Development `
  "-project=$PWD\Pandora\Pandora.uproject" -WaitMutex -FromMsBuild

& "$env:UE_ENGINE_DIR\Engine\Binaries\Win64\UnrealEditor-Cmd.exe" `
  "$PWD\Pandora\Pandora.uproject" -unattended -nop4 -NullRHI `
  '-ExecCmds=Automation RunTests Pandora.Module.Account.DsRecovery;Automation RunTests Pandora.Net.DSAuth;Quit' `
  '-TestExit=Automation Test Queue Empty'
```

至少核对测试包含：

- `Pandora.Module.Account.DsRecovery.SingleClientTravelWriter`
- callback generation/request-seq/phase 与 ReturnHub fence lifetime
- Admission world/endpoint identity 与 canonical recovery attempt UUIDv4
- `Pandora.Net.DSAuth.BattleEvictionPlacementABA`
- Hub/Battle missing/wrong/stale ACK、physical census attribution 与 source eviction

结果：

- PandoraEditor full build：`PASS`，725/725 actions（当前 SVN 源码快照两次全量通过，
  525.07s / 543.62s）。
- PandoraServer full build：`PASS`，577/577 actions，退出码 0，386.25s。
- DsRecovery Automation：`PASS`，5/5 Success，Failed 0，NotRun 0，退出码 0。
- Pandora.Net.DSAuth Automation：`PASS`，11/11 Success，Failed 0，NotRun 0，退出码 0；
  `BattleEvictionPlacementABA` 与 `BattlePawnWorldCensusLifecycle` 均为 Success。

### 5.6 真环境故障注入（本地单测不能替代）

必须保存每个注入点的 before/after canonical placement、source/target physical census、assignment/session、
Battle active index、match/outbox 与实际可连接 DS：

- Redis Cluster：单 master 断开、MOVED/ASK、SCAN 中途失败、canonical 存在但 derived index 丢失。
- locator/allocator：Confirm/Commit 已提交但 ACK 丢失；重启后 proof id/operation 不变。
- K8s/Agones：同名 GameServer UID 重建、DELETE accepted 但 Pod 尚存、allocator timeout-but-applied。
- Hub/Battle：真实 UDP PreLogin/Admission 每一步断线；Controller Logout 先于 Pawn Destroy；旧 eviction 迟到。
- 客户端：Login/SelectRole/Start/Confirm/READY/Travel/Admission/Result/ReturnHub 任一点切后台、断网、杀进程。
- 容量：reservation 后连续 heartbeat=0、connected 漏报、MaxPlayers 漂移、capacity=1 并发。

结果：`[未执行——生产/灰度发布阻断，不得写 PASS]`

## 6. Claude Code 回报格式

Claude Code 只返回审核意见，不改文件。按严重度排序，每项必须包含：

1. `P0/P1/P2` 与一句反例；
2. 文件 + 行号；
3. 最小可复现时序（谁先提交、哪个 ACK 丢失、哪份旧 identity 仍生效）；
4. 违反本文哪条不变量；
5. 现有测试为何没覆盖；
6. 建议新增的 barrier/故障注入断言。

若没有发现，也不能只写“LGTM”。必须逐项明确回答 §2.1~§2.7，列出已读文件和实际执行的命令，并把
“代码未发现反例”与“真实环境未验证”分开。禁止把测试未运行、环境失败或超时写成通过。

## 7. 当前结果汇总

| 门 | 当前记录 | 是否允许上线 |
|---|---|---|
| 代码实现/静态 diff | 当前工作树已接线；本地独立反例复审未发现剩余 P0/P1 旁路，待 Claude 独立反证 | 否，待独立审核与外部门 |
| targeted Go test/vet | PASS（含关键并发用例 `-count=30`） | 仅本地门通过 |
| 全 Go workspace | PASS（29/29 test + vet）；race BLOCKED-ENV | 仅本地门通过 |
| proto/codegen/contracts/kustomize | PASS；online live API 步骤 BLOCKED-ENV | 仅静态门通过 |
| UE Editor/Server build + Automation | PASS（725/725、577/577、5/5、11/11） | 仅本地门通过 |
| production placement preflight | 未执行 | 否 |
| production PodUID prepare+drained+final preflight/ACL cleanup | 未执行（本地命令/Job/顺序合同 PASS） | 否 |
| Redis topology-change lease provider | **未实现/未接线；所有 Activate/CAS/ordinary online release 当前 fail-closed** | **否，发布硬阻断** |
| 真 Redis Cluster/K8s/Agones/UDP/移动端矩阵 | 未执行 | **否，发布阻断** |

最终拍板只能在本表全部必需门有可追溯证据后进行。即使本地全部通过，也只能说“当前代码快照未发现已知
反例”；真环境矩阵未完成时，不能说“任意时间点生产已经不会卡死”。
