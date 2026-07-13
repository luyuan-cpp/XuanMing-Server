# DS callback auth / Hub token generation：Claude Code 独立审核交接

> 状态：已收到首轮 Claude Code **部分只读审核**：覆盖后端 `hub_allocator` Model B data/biz/service/main
> 与 auth/middleware 抽检，未发现新的鉴权 P0/P1；未覆盖 UE、Battle `SignDSTicket*` 全调用点、`-race`
> 和真集群。本文及该部分结论均不代表全链路已经闭环，也不得作为生产通过证据。

## 1. 审核纪律

1. 先按仓库规范依次完整读取 `AGENTS.md`、`PROGRESS.md`、`CLAUDE.md`、
   `docs/design/pandora-arch.md`、`docs/design/decision-revisit-ds-callback-auth.md`、
   `docs/design/decision-revisit-player-jwt-key-rotation.md`、
   `docs/design/decision-revisit-ds-key-rotation.md`、`docs/design/battle-reconnect.md`、
   `docs/design/zero-downtime-update.md`、相关服务设计以及 `git log -20 --oneline`。
2. 后端仓库是 `F:\work\XuanMing-Server`，UE 仓库是
   `F:\work\Pandora-Client-SVN`。进入 UE 仓后还须先读其 `AGENTS.md` 和要求的客户端规范。
3. 两个工作树都可能包含用户的既有脏改动。只读审核，不改文件，不安装工具，不改环境，
   不 commit/push/tag，不碰生产，不读取或输出真实 secret。
4. 不相信 `PROGRESS.md`、decision 文档或本交接中的“已修复”表述；必须从真实代码、生成物、配置、
   测试和失败路径重新证明。测试通过只能证明覆盖到的样例，不能替代状态机反证。
5. 若发现需要推翻已批准的 Redis Model B，先把它分类为“架构子决策”，不要直接建议在代码中暗改。

## 2. 必须独立反证的安全不变量

逐条寻找能打破下列结论的最小并发/半失败序列：

1. Redis auth record 是唯一授权权威；K8s annotation 只投递，不参与授权判断。
2. 凭据身份完整绑定 `(pod/GameServer UID, instance epoch, gen, jti, exp, kid,
   token_sha256, writer_epoch)`；同名 Pod 的 UID 变化永久失效旧凭据和旧 assignment。
3. 当前 V2 只接受
   `required_writer_epoch == active/pending/request.writer_epoch == 2`；合法签名的 future writer=3
   与 legacy writer 均须在任何副作用前拒绝。
4. pending 只能用激活心跳原子提升；promote、active heartbeat、ready/last_verified 投影必须同槽、
   同一线性化事务。响应丢失后的重试必须幂等。
5. 任意 stale/missing/future/wrong UID/epoch/gen/jti/kid/hash/token 的请求，在 protobuf bytes、TTL、
   SET/ZSET、presence、capacity、assignment、MySQL 等所有副作用上均为零变化。
6. AssignHub、已有 assignment、ListHubLines、reserve/release、login admission 等最终门不能只看
   `state=ready`，必须证明 auth active、UID/epoch、last_verified、服务端心跳和相应容量/归属条件。
7. Battle GSA POST 前必须先持有永久 `allocation_uncertain` fence；POST/PATCH/GET/finalize 任一未知
   结果都不能触发第二次 POST，也不能凭本地猜测 Release/Delete。特别检查进程崩溃和 Redis EXEC
   “已提交但响应丢失”。
8. Battle result 先完成 MySQL 幂等落库，再写完整 tuple receipt；credential rotation 与 receipt、
   `ended` heartbeat 的任意交错不得出现“未结算就结束”或“已结算但错误凭据结束”。
9. 紧急 quarantine 必须先吊销 auth 权威；shard/battle 投影缺失或漂移不能反过来阻止吊销，
   但也不能误改新 UID/新 allocation 的投影。
10. UE 必须严格解析完整 annotation bundle，线程安全维护 active+staged；staged 只能发激活心跳，
    ACK 完整匹配后切 active；active=A/staged=B 时 ACK(A) 必须失败，不能借 active 幂等分支误报激活；
    所有受保护 DS RPC 统一带当前 active Bearer。
11. UE 结算负载在首次发送前冻结；ReportResult 未获权威成功以及 `ended` 未获终态 ACK 前不能自行
    Stop/Shutdown。检查响应丢失、token 轮换、进程退出和无限重试是否产生新的不可回收实例。
12. `PreLoginAsync` 在线验票必须按 Guard → 当前 Redis active+projection → 玩家票 binding/assignment →
    JTI 线性化顺序执行。`admission_id` 的相同逻辑尝试应能恢复“服务端已应用但响应丢失”，不同尝试
    必须 replay 拒绝；普通 token 轮换可重认，但 quarantine、UID/epoch 变化、drain 不得借幂等旁路。
13. login 的所有 Battle 签票路径（公共 `IssueDSTicket`、登录断线重连）必须共用同一个 roster
    authorizer。legacy/local 至少证明 live projection+成员；Redis authority 还要证明完整 active
    tuple/UID/epoch/writer/high-water/last_verified。authorizer 未注入、Redis 故障或 identity 空/0 时不得
    回退 signer 直签。locator 已明确 `InBattle` 后上述失败也不得继续 AssignHub/写 LOGIN_PENDING；
    reconnect 必须返回 authorizer 同一 Redis 快照的非空 `ds_addr`，不能证明当前 B 却返回 locator 旧 A；
    请主动搜索所有 `SignDSTicket*` 调用，不能只测公开 RPC。
14. UE tracked dev DSTicket secret 只能由精确 `local-off-v1` 使用。Agones/online、空/短/占位 secret、
    profile/pod/scope 伪造和“env 非空但无效后回退 config”都必须 fail-closed，日志不得输出 secret。
15. UE 本地 JTI replay cache 必须有写入总量上限与按 ticket exp 的回收；满载时不得驱逐未过期
    JTI。`PreLoginAsync` 可清过期但不能新增/提前消费，只有 online authority 成功后真正 Init 才消费；检查/消费/清理
    必须同锁，超长/非法 JTI 与无效容量配置 fail-closed。
16. UE unary 只能接受唯一、canonical 且有界的 `grpc-status`；`4294967296`、前导零、重复、缺失、
    非数字与超范围值都不能经整数回绕或默认值冒充 status 0。
17. Hub/Battle 心跳命令必须绑定本请求实际 Bearer snapshot 的完整 ACK；普通 active 响应还要在回调时
    证明 snapshot 仍是当前 active。missing/wrong ACK，以及 A 请求发出后 B 已提升的迟到 ACK(A)，在
    stop/drain/reload 等命令副作用上必须为零。activation 只有精确 staged ACK 成功提升后才可处理命令。
18. UE admission owner 必须是真正的小写 RFC4122 UUIDv4，不能依赖平台 `FGuid` 内存布局或大小写宽松
    parser。生成必须来自 CSPRNG、只覆盖 6 个 version/variant 固定位并在随机源失败时于网络前拒绝；
    validator 必须机械检查长度、连字符、小写 hex、version 与 variant。

## 3. 强制失败矩阵

至少覆盖并报告以下真实或可执行反例，不接受只看 mock happy path：

- 两个 allocator 真并发 Get-miss/SETNX/GSA POST；SETNX 后 ZSET 失败；fence EXEC 响应未知；
  GSA POST timeout-but-applied；PATCH timeout-but-applied；409；2xx 空/坏/缺 UID/RV/allocation_id；
  严格 GET 失败；Redis finalize 响应未知。
- Hub PATCH 的九个 annotation 任一缺失/损坏/被旧响应覆盖；UID/RV 在 PATCH 与 GET 间变化；
  同一 gen 不同 jti/token；counter TTL 回退；高水位回退。
- Redis unavailable、坏 protobuf、future unknown fields、future writer=3、旧 writer=1；所有拒绝路径
  对原始 bytes/PTTL/index/assignment/receipt/command queue 做前后对比。
- active/pending/receipt/ReportResult/ended 的所有关键交错；DB 已成功但响应丢失；receipt 已写后轮换；
  ended ACK 丢失；UE staged 到达时正处于 terminal handshake。
- quarantine 与 heartbeat/AssignHub/release/UID 重建并发；投影缺失、投影已属于新实例、auth 已是
  QUARANTINED 的幂等重试。
- VerifyDSTicket 的无 Bearer、坏 Bearer、错 pod/type/match/UID/epoch/gen/jti/hash、过期、future writer、
  stale heartbeat、Hub shard draining/missing/wrong last_verified、assignment 在 A1/H/A2 间变化；
  上述失败均不得提前写 JTI。
- admission 首次 Redis 已写但响应丢失；同 admission 并发；不同 admission；legacy value；首次响应
  丢失同时 active gen/jti 轮换；UID 重建/quarantine/drain 后拿同 marker；总 deadline 超时；UE callback
  exactly once。
- UE Automation 加完整 Dedicated Server UBT；不要用 Editor-only 编译代替 DS target。
- Battle 签票额外覆盖：公共 RPC 与 login reconnect 两条入口、authorizer nil/deny/unavailable、三处
  UID 同为空且 epoch 同为 0、legacy live/空 roster/非成员/陈旧心跳、Model-B drift；所有拒绝路径
  对 auth/projection 两键原始 bytes 与 PTTL 做前后对比。
- UE secret policy 覆盖 exact local-off-v1 占位允许、Agones/profile伪造/online 占位拒绝、环境变量
  占位不得回退到配置中的另一个值，以及 active/staged/在线准入三个调用点使用同一策略。
- UE replay cache 覆盖 exp+leeway 边界回收、满载时零新增/不驱逐、同 JTI 并发仅一次成功、unique
  并发不超过硬上限、PreLogin 不新增与 online completion 后 Init 才消费。
- UE trailer 覆盖 `0/16/17/00/4294967296/超长/负数/重复/缺失`，并证明 overflow trailer 的完整 unary
  不成功；TryPromote 覆盖 active=A/staged=B 下 ACK(A)/ACK(B)/wrong ACK 与过期 staged 清理。
- Hub/Battle 分别覆盖 stop/drain/reload 的 missing/wrong ACK，以及 A 请求→B promote→迟到自洽 ACK(A)；
  每一拒绝分支必须直接断言命令副作用计数为零。
- admission owner 覆盖至少 1000 个生成样本全部 canonical/无碰撞，以及空串、35/37 长度、大写、
  wrong version/variant、错连字符和非法 hex；并静态核对 Win/Linux 目标均链接实际调用的 CSPRNG 实现。

当前本地执行记录仅供定位，**不得替代独立复跑和代码反证**：最终统一版本 Editor UBT
714/714 Succeeded，`Pandora.Net` 11/11 + Battle terminal 1/1，Server UBT 824/824 Succeeded；
相关 Go test/vet、proto lint、Kustomize render 与 diff whitespace 也通过。首轮 Automation 的
`OnlineAdmissionPolicy` 曾失败并促成 UUID 修复，说明不能只相信“已有测试”。真集群与 `-race` 的未执行
范围见下一节。

## 4. 已知待决策/部署阻断（不要误报为已完成）

以下项目即使业务代码全绿，也仍禁止宣称“可生产激活”：

1. `required=1` 尚不能机械隔离 legacy 与 Model-B 业务行为；生产需要另行批准并交付
   blue/green prepare → quiesce → active（当前推荐）或完整 stage-only 行为门。
2. mutable image tag、immutable/revisioned Secret、旧 ReplicaSet/Fleet/GameServerSet 回滚保留审计、
   etcd/Redis TLS+ACL、仓内固定 digest 的集群内 `:8444` synthetic 尚未闭环。当前两个 Fleet 未安全
   注入 `PANDORA_DS_TICKET_SECRET`，生产 signer 真 key 会使 UE 全拒票，沿用 tracked dev 占位则失守；
   这是当前确定的不可上线 blocker，不是“以后优化轮换”。同时，直接把玩家 HS256 签名 secret 注入
   不可信 DS 会赋予造票能力，也不是修复；须独立审核公钥验签或仅 online authority 的待拍板选择。
   另须单独反证 :8444 当前 `TLS=0`：active Bearer、玩家票、GM 命令的 mTLS/服务端身份/机密性均未
   交付，NetworkPolicy 与 response ACK 不得被当成替代。
3. `allocation_uncertain` 当前是安全隔离而非自动恢复；若要恢复可用性，需要另行设计 GSA 幂等请求或
   UID/allocation_id 权威 controller，不得用 sweep 猜测。
4. Hub emergency quarantine 后的 assignment 迁移和 GameServer 精确 UID 退役尚无持久 outbox/controller
   证明；是否增加同槽 outbox + UID precondition delete 属于需要人确认的运维状态机扩展。
5. 人已在最终收尾阶段授权创建服务端本地 Git 提交；UE 的正式 client-proto lock 应在该提交生成后，
   由 UE SVN 流程复核并绑定真实 commit hash。本次服务端提交不等于远程 `svn commit`，也不得预填或
   伪造尚未生成的 Git 指纹。
6. Hub active A→B 后，首次使用仍携带 A 的未过期票会被当前 strict admission 拒绝；是否增加仅 admission
   可见的 bounded `previous_admission` grace 是 §7.16.1 待拍板子决策，不能用强制重取票冒充零停机。
7. Battle result 已落 MySQL/receipt 但 callback credential 随后过期时，终态释放仍可能卡死；推荐把
   terminal-release outbox 与结果放进同一 MySQL 事务并由 relay 驱动，属 §7.16.2 待拍板，尚未实施。
8. locator 连续查询失败时，现有 Login 会先 AssignHub/签票、后 best-effort Notify；BATTLE fence 只能
   挡 placement 覆盖，撤不回 seat/ticket。最小“未知即拒绝”和标准“版本化 placement lease + Hub
   admission 最终门”见 §7.16.3，均待人拍板；当前不能宣称一人一 DS 已在 unknown 路径闭环。
9. 首轮 Claude 审核把“心跳覆盖 allocator reservation”列为 P2 既有观察，并认为影响只持续一个心跳
   周期。后续独立反证发现 assignment 默认保留 30min、到期无对应 reservation ledger 退座，且 admission
   只验 assignment/auth、不验独立容量 lease；不同玩家可跨多轮心跳累计有效票据。因此改列为
   §7.16.4 **P1 生产阻断**，容量模型修复须人拍板，不能把该首轮 P2 定级当成闭环。
10. 复核同时发现完全相同 assignment 的复用快路径不刷新 TTL，却会签发新的 5min 票。该非架构缺陷
    已改为完整 bytes CAS SET 刷新 TTL，并补 miniredis 剩 1s 后重签回归；仍需 Claude 复核此新增修复。

## 5. 输出格式

只输出审查结论，不复述实现说明。每条发现必须包含：

- 严重级别：P0 / P1 / P2；
- 精确文件和行号；
- 最小并发或半失败复现序列；
- 违反的不变量与可观察副作用；
- 建议修复和需要新增的测试；
- 分类：代码缺陷 / 验证缺口 / 需要人拍板的架构或部署项。

若没有代码级阻断，也必须明确列出实际执行的命令和未能执行的验证；不得写笼统的“看起来没问题”。
