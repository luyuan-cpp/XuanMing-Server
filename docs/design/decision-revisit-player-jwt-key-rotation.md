# decision-revisit:玩家 SessionToken / DSTicket 不停服密钥轮换

> 状态:**已拍板——方案 B**(2026-07-13 用户批准；子形态与理由见 §7 决策记录)。
> 方案 C 仅作为拍板前的临时桥,终态代码不保留 C 的在线权威验票路径。
> 关联:`pkg/auth/jwt.go`、login / hub_allocator / matchmaker / matchmaker-pve 的签发配置与 `main.go`、
> `tools/scripts/gen_cluster_config.ps1`、外部边缘 Envoy JWKS、
> `docs/design/decision-revisit-ds-key-rotation.md`、CLAUDE.md §9 不变量 16。

## 1. 拍板前审计基线与残留问题（历史记录）

以下是方案 B 拍板前发现的**历史问题**，用于说明为什么禁止把玩家 HMAC 发给 DS；当前终态契约
以 §7 为准：

- 当时 login / hub_allocator / matchmaker 的 `JWTConf` 已有 `additional_secrets`，三个 `main.go` 也传入
  `auth.Config.AdditionalSecrets`；
- `pkg/auth` verifier 已具备多密钥候选能力；hub_allocator 在自身同时可见的两组配置上做了
  玩家/DS key-set 不相交检查。

但这**不等于玩家链路已支持轮换**:

- SessionToken / DSTicket 签发仍未写与主密钥对应的 `kid`；
- 当前生成器随后加入了 `additional_secrets` 注入和双 key JWKS，但这是未经本决策批准的部分接线；
  Online Edge 探测仍不能证明 primary/additional 同时被接受；
- 外部 Edge 没有获批的 K1/K2 重叠发布管线；
- hub_allocator 的本地检查看不到其它服务可能漂移的完整配置集合，尚不能替代统一部署 gate。
- UE Dedicated Server 当时会在在线 `VerifyDSTicket` 前用 HS256 本地验 DSTicket，因而期待
  `PANDORA_DS_TICKET_SECRET`。这不是普通“少挂一个 Secret”：把玩家签名 HMAC 交给不可信 DS，就赋予
  它伪造任意玩家/对局票据的能力；当前 SessionToken/DSTicket 还共用玩家 secret，暴露面更大。
  Battle/Hub Fleet 目前未注入该值，真实生产 key 会全拒票；但**禁止**以向 Fleet 下发 HMAC 签名密钥
  作为修复。

因此直接改主密钥仍会在新旧签发方、后端 verifier 与边缘 Envoy 之间形成拒绝窗口，违反不停服
滚动更新要求。现有部分接线在决策拍板和全链路验收前不得标成“已实现玩家轮换”；生产脚本现已
临时拒绝 additional_secrets，只允许非生产验证，避免把半链路带上线。

另有独立 P0:玩家候选密钥集与 DS 回调候选密钥集必须完全不相交。只比较两把主密钥不够；若玩家
旧/新密钥误入 `ds_auth.additional_secrets`,客户端令牌可能被拿来伪造 DS 回调。

## 2. 待选方案

### 方案 A:继续 HS256,补齐多密钥全链路

- 三份玩家 `JWTConf` 增加仅校验用的额外密钥并接入各自 `auth.Config`；
- SessionToken / DSTicket 按签发主密钥写稳定 `kid`；
- Envoy JWKS 在重叠窗口同时发布 K1/K2,每把 key 使用与后端一致的 `kid`；
- 按“先扩 verifier/Edge → 再翻签发主密钥 → 等最后一枚 K1 令牌过期 → 清 K1”推进。

优点是改动较小。缺点是 `kty=oct` JWKS 本身包含签名密钥，所有持有 Envoy 验签配置的组件也拥有
签名能力；密钥分发面仍较大。

### 方案 B:玩家面迁到非对称签名(保留 DS 离线验签时的生产推荐)

- login / hub_allocator / matchmaker / matchmaker-pve 持有签名私钥；Envoy 与只验签服务只拿公钥 JWKS；
- 用 `kid` 同时发布旧/新公钥完成重叠轮换；DS 回调面继续使用独立密钥体系；
- 需要确定算法、私钥托管/注入方式以及三类签发服务是否共用同一签名身份。

优点是验签侧泄露不能伪造玩家令牌，符合 `pkg/auth/jwt.go` 已记录的生产方向。缺点是迁移与运维成本
更高，且三类签发方的私钥权限边界需要人拍板。

其中 DSTicket 至少应与 SessionToken 使用独立 issuer/audience/keyset；UE DS 只接收 DSTicket 公钥 JWKS，
不能拿任何签名私钥/HMAC。Fleet 引用 revisioned immutable **public keyset**，新旧公钥重叠轮换；签发服务
的私钥由独立 Secret/KMS 身份持有。

### 方案 C:生产只做在线权威验票，DS 不持有玩家验签 key

- UE `PreLoginAsync` 把原始 DSTicket 连同 active DS credential 发给 login；在 online 响应成功前不信任
  本地解析出的任何 claim，也不执行 InitNewPlayer 副作用；
- login 已执行签名/exp/active/projection/assignment/roster/JTI 全链验证，响应返回权威 claims；
- local-off-v1 可保留开发占位离线验签，Agones/online 路径不读取 `PANDORA_DS_TICKET_SECRET`。

优点是彻底不向 DS 分发玩家签名能力，且 online admission 本来就是 Redis Model-B 必经门。缺点是 login/
DS 网关成为每次玩家入场的硬依赖，需明确超时、容量与故障预算；丢失本地廉价签名预筛，恶意流量必须由
:8444 限流/配额吸收。该方案会改变 UE 当前“本地先验签”的设计，也必须人拍板后再改。

## 3. 两种方案都必须满足的 P0 门

1. 部署输入必须同时拿到“玩家 primary+additional”与“DS primary+additional”完整集合；trim 后的
   空项、集合内重复项、两集合任一交集都启动/生成失败，不能静默过滤。
2. 只在生成器比较不够:消费两类密钥的进程须做启动期本地断言；其它服务还需由统一部署预检或
   admission/配置控制面保证同一不变量，避免手工 YAML 绕过。
3. `kid` 只是选键提示,不能成为信任依据；未知/重复 `kid`、错误算法、错误 issuer/audience 一律拒绝。
4. Edge 与后端接受集合必须可观测、可对账；任何阶段都不允许先切签发方再补 verifier。
5. 清退 K1 的等待期以“最后一次 K1 签发时间 + 实际最大令牌 TTL + 缓冲”为准。配置只有默认值，
   不能假设固定上限。

## 4. 风险与迁移成本

- 四个签发服务和外部 Edge 必须按阶段协同，任一步漂移都可能造成全量 401。
- SessionToken 默认寿命长于 DSTicket；阶段驻留时间和 Redis session 生命周期必须一起核对。
- HS256 多 key 会扩大共享秘密暴露面；非对称方案会引入私钥托管、算法迁移和回滚复杂度。
- 生产边缘网关不在本仓库，不能仅凭生成了 JWKS 就声称 Edge 已切换；仍需真实负向/正向探测。

## 5. 人需拍板

- [x] 选择方案 A(HS256 多 key 过渡)还是方案 B(直接迁非对称签名)。→ **方案 B**(2026-07-13 用户批准)。
- [x] 对 UE DS 验票单独选择 B（DSTicket 公钥离线验签）或 C（仅 online authority）。→ **B,且为纯本地
      验票子形态 B1**(见 §7);生产不得把玩家 HMAC/私钥注入 Fleet。
- [x] 确定签名 key 的权属、托管、轮换触发人与审计来源。→ login / hub_allocator / matchmaker /
      matchmaker-pve 各自经独立 `auth.DSTicketSigner` 持同一把 DSTicket 私钥(K8s Secret 文件挂载)；
      普通发布只读对账，不轮换，见 §7.4。
- [x] 确定完整玩家/DS key-set 不相交的权威配置面与不可绕过的部署 gate。→ DSTicket 面迁 RS256 后与
      DS 回调 HS256 面天然不相交(算法不同、issuer/audience 分域);Fleet manifest 机械检查禁止私钥/oct JWKS。
- [x] 确定各阶段最短驻留窗口与紧急回滚策略。→ 见 §7.4 轮换流程;常规发布不动密钥。

## 6. 验收标准(拍板后实现)

- [ ] 新旧副本 × K1/K2 玩家令牌 × Edge/后端 verifier 的阶段矩阵全部通过。
- [ ] 玩家 key-set 与 DS key-set 任一交集、空额外项、重复/未知 `kid` 均启动失败。
- [ ] 签发密钥翻转前,Edge 与全部 verifier 已可证明接受新 key；清退前旧 token 已自然过期。
- [ ] 单密钥默认配置行为兼容,滚动升级与回滚均不要求停服。
- [ ] 真实边缘链路完成无 token、错误 key、当前 key 三段探测并留存 key 指纹审计记录。
- [ ] UE DS 路径证明只持有公开验签材料或完全不持 key；攻陷 DS 不能签出可被 login/其它 DS 接受的
      SessionToken/DSTicket。

## 7. 决策记录(2026-07-13,用户已批准方案 B)

### 7.1 B 子形态:选 **B1 纯本地验票(production-rs256-local)**

| 维度 | B1 纯本地验票 | B2 本地验签 + 在线最终授权 |
|---|---|---|
| PreLogin 网络依赖 | **零**(验签纯计算 + 本地状态) | 每玩家 1 次 Login RPC(保留方案 C 的可用性缺陷) |
| Login/Redis/:8444 短时故障时已签出的有效票 | **仍可进服** | 全部被拒 |
| 跨 DS jti 全局防重放 | 无(本 DS 内 `FPandoraDSTicketReplayCache`,上限 65536) | 有(Redis SETNX) |
| 已发票即时吊销 | 无 → 用短 TTL capability(90–120s)补偿 | 有 |
| 实例精确绑定 | 本地比对 claims `ds_uid`/`ds_instance_epoch`/`hub_assignment_id`/`allocation_id` 与 DS 自身身份(UE 已有 `FPandoraDSCredential.InstanceUID/InstanceEpoch`、`DoesTicketMatchInitialCredential`) | Login 侧 Redis 投影(`RedisBattleTicketAuthorizer`) |
| drain/stop 拒新 | 本地心跳 command 原子闸门 + 权威租约(心跳 ACK 续租,过期只拒新玩家) | 服务端状态 |

跨 DS 重放威胁面评估(选 B1 的关键论据):v2 票据签死到唯一实例(hub:`hub_assignment_id`+`ds_uid`;
battle:`match_id`+`allocation_id`+`ds_uid`),异地重放在验签期即被拒;同实例重启由 `ds_instance_epoch`
递增否决旧票;残余仅「同实例同 epoch 内 ≤TTL+leeway 重放」,被本地 jti 缓存覆盖。用户诉求「后端故障
时不要一直拦截玩家」是决定性权重 → B1。

B1 的强制补偿(缺一不可,不允许静默降级):
1. 票 TTL 收短:`ds_ticket.ttl` 默认 120s;UE 强制 `exp-iat ≤ 180s` + leeway ≤ 15s。hub 票即短时一次性
   capability,assignment 撤销靠自然过期 + `hub_assignment_id` 比对(明确取舍:吊销时延上界 ≈ 135s,
   不保留「5 分钟票 + 60 秒 leeway 却声称旧 assignment 及时失效」的自相矛盾)。
2. jti:PreLogin 只 `CheckAvailable`,InitNewPlayer 原子 `TryConsume`;容量满 fail-closed。
3. 权威租约:心跳 ACK(`IsAuthorizedActiveResponse`)续本地租约;租约过期只拒新 PreLogin,不踢在场玩家。
4. drain/stop 与 PreLogin 原子闸门(拒新不断旧)。

### 7.2 签票方拓扑

现有受信签票方 login / hub_allocator / matchmaker / matchmaker-pve 各自经独立 Go 类型
`auth.DSTicketSigner` 持同一把 DSTicket 私钥。revisioned `Secret/pandora-dsticket-signer-r<revision>` 以
`/run/secrets/pandora-dsticket/private.pem` 挂载，文件 mode `0440`；这四个 Deployment 固定
`runAsNonRoot=true`、UID/GID/fsGroup `10001`，由 fsGroup 赋予只读权限，不依赖 root/`0400`。
不新建集中签票服务：四者均在受信控制面，新服务只会把暴露面换成单点 Pod + 新 RPC 信道，净增复杂度。

私钥暴露面机械收敛为上述**恰好四个** Deployment。Login 因保留 `VerifyDSTicket` 兼容/诊断路径，
还只读挂载与 DS 完全相同的公开 overlap JWKS；其它三个 signer 不需要该公钥卷。Fleet 只挂
`ConfigMap/pandora-dsticket-jwks-r<revision>`，机械检查禁止出现 `PANDORA_DS_TICKET_SECRET`、
任何 `Secret/pandora-dsticket-signer-r<revision>`、`private.pem`、任何私钥成员或 `kty=oct` JWKS。

### 7.3 DSTicket v2 claims(`dst_ver=2`)

- 头:`alg=RS256` 固定、`kid`(RFC 7638 RSA 公钥指纹)。JWKS 每把 key 的 `kty/use/alg/kid`
  全部必填，分别严格为 `RSA` / `sig` / `RS256` / 该 key 的指纹。
- 通用:`iss=pandora-dsticket`、`aud=pandora-game-ds`、`sub`、`jti`、`iat`、`exp`、`dst_ver=2`、
  `ds_type`、`ds_pod`、`ds_uid`、`ds_instance_epoch`、`release_track`。
- hub:`hub_assignment_id` 必填、`match_id=0`;battle:`match_id`、`allocation_id` 必填。
- **移除 v1 对回调凭据 `ds_gen`/`ds_credential_jti` 的绑定**:回调凭据轮换与玩家准入无关,v1 绑定使
  凭据轮换误伤已发玩家票。
- 迁移:确认无生产 HS256 存量(内网开发期),直接切换,终态不保留 HS256 兼容读;唯一 HS256 残留是
  Windows `local-off-v1` 档(显式 local-off-hs256,生产模式机械拒绝)。

### 7.4 密钥生命周期(与发布解耦——再次强调)

- **K1 只创建一次**:首次从共享 HS256 迁到 B 时，`tools/scripts/dsticket_keyset.ps1` 在受控目录调用
  `tools/dsticketkeys` 生成 RSA-2048 私钥 PEM + 公钥 JWKS。JWKS 顶层必须含正整数 `revision`、
  显式 `active_kid` 和 `keys`，active key 按 kid 查找，**禁止把 `keys[0]` 当 active**。helper 以
  create-only 方式投递 immutable `Secret/pandora-dsticket-signer-r<revision>`，并分别在 `default`（DS）和 `pandora`
  （Login verifier）创建内容/hash 相同的 immutable `ConfigMap/pandora-dsticket-jwks-r<revision>`；
  已存在对象只回读对账，绝不 patch/delete/覆盖。
- **普通发布(金丝雀/回滚)只换镜像 digest**:Stable/Canary Fleet 永远共用同一 keyset revision,不动密钥。
- **轮换是独立罕见安全操作**：`tools/scripts/dsticket_rotate.ps1` 已实现三个显式阶段。`stage` 先把 Login
  与四个 Fleet/全部存活 DS 切到 K1+K2 verifier，签票仍用 K1；`promote` 再把四个 signer 的完整
  config/signer bundle 原子切到 K2，确认旧 K1 signer Pod 全部消失后创建 immutable activation marker；
  `retire` 只在该 marker 的 apiserver `creationTimestamp + 180s 最大 TTL + 15s leeway + 30s buffer`
  之后切到 K2-only，并以 terminal marker 证明 fixed config、四 signer、Login/Fleet/存活 DS 已统一终态。
- 三阶段配置均为完整复制后的 revisioned immutable Secret；Deployment 以 resourceVersion + 旧引用 CAS
  原子切 config/signer/Login JWKS。阶段门禁核对 Deployment→ReplicaSet→Pod 与
  Fleet→GameServerSet→GameServer→Pod 的 controller owner UID、revision、live/ready/allocated 残量；
  脚本不强杀或删除在场 GameServer，也不删除旧密钥材料。
- 普通 `start.ps1 -Mode online` 与轮换脚本共用 create-only
  `ConfigMap/pandora-dsticket-operation-lock`。锁只按 UID/resourceVersion CAS 释放；进程崩溃遗留锁
  fail-closed，须人工只读审计后处理，禁止按客户端时钟自动过期或抢锁。普通发布继续强制
  signer/config/Login/Fleet/active kid 同 revision，不能用普通发布命令冒充轮换。
- **当前只代表编排与静态/本地契约已交付，不代表生产轮换已激活**。首次真实执行前仍须完成新旧 Pod ×
  K1/K2 矩阵、真 UE DS 正负向 E2E、真实集群 controller 观察与审计留证；本轮没有执行任何集群轮换。

### 7.5 UE Ready 门禁

JWKS 文件缺失/解析失败/空 keyset/含私钥字段/含 `kty=oct`/revision 与 `PANDORA_DSTICKET_KEYSET_REVISION`
不符 → 拒绝 Agones Ready(fail-closed);生产模式发现 `PANDORA_DS_TICKET_SECRET` 或 dev 占位密钥同样拒绝
Ready。`local-off-v1` 不受影响。

### 7.6 已知残余风险(诚实清单)

1. 同实例同 epoch 内 ≤135s 的 jti 重放依赖本 DS 内存缓存;DS 进程崩溃重启且 epoch 未变时旧票可再用一次。
2. 已签出票据的封禁即时性下降为 ≤135s;封禁主链路在 Login 拒签新票。
3. matchmaker 签 battle 票依赖 `AllocateBattleResponse` 回传实例身份(proto 兼容加字段);旧 ds_allocator
   不回填时 matchmaker fail-closed 拒签。
