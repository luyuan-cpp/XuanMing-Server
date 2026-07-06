# Pandora 总架构

> 立项决策、玩家流转、服务清单、关键时序。本文档是 §1 必读。

## 1. 项目定位

- **类型**:MOBA + 持续在线大厅(类 Albion / New World 城镇 + LoL 战斗)
- **核心循环**:登录 → 进大厅(可走动 / 互打 / 试技能 / NPC 对话 / 交易 / 组队)→ 匹配 → 进战斗 DS 打一局 → 结算回大厅
- **关键参数**:
  - 大厅 DS:**500 人/实例**,单城镇约 1km²,**全图自由 PvP**
  - 战斗 DS:10 人(5v5),约 25 分钟/局
  - 战斗 tick:30~60 Hz / 大厅 tick:20~30 Hz

## 2. 双仓库结构

- 后端仓库:go 服务 + proto + docs + deploy
- UE 仓库:UE 5.7 客户端 + 大厅 DS + 战斗 DS

**协作纪律**:
- proto **source of truth 在 Pandora 后端仓库**(`Pandora/proto/`)
- `Pandora` CI 在 proto 改动时,自动生成 cpp .pb.h 推送到 UE 仓库的 `Source/Pandora/Generated/Proto/`
- UE 仓库不允许直接改 .proto,所有改动从后端仓库来

## 3. 服务清单(go,共 14 个)

⚠️ **架构演化记录**:
- 2026-06-03 上午:13 个业务服(login + 12 个)
- 2026-06-03 中午:推翻,加 gateway + push → 15 个(2026-06-04 再次推翻)
- **2026-06-04 终版:14 个**(13 业务服 + 1 集中 push 服务)
- gateway 不作为 go 服务(改用 **Envoy** 这个基础设施组件,详见 `gateway-decision.md`)

| # | 服务 | 职责 | 是否有状态 | 依赖 |
|---|---|---|---|---|
| 1 | **login** | 账号 / 登录 / 颁发 DS 票据 | 无 | mysql + redis |
| 2 | **player** | 玩家档案 / 段位 / 英雄池 / 皮肤 | 无(读穿 mysql) | mysql + redis |
| 3 | **data_service** | 玩家数据读写网关 / 缓存 | 无 | mysql + redis |
| 4 | **friend** | 好友 / 黑名单 | 无 | mysql + redis |
| 5 | **chat** | 频道(世界 / 队伍 / 私聊) | 弱(channel 路由) | redis pub/sub + kafka |
| 6 | **player_locator** | 玩家位置(hub_id / battle_id) | 强 | redis |
| 7 | **team** | 组队状态机 | 强 | redis |
| 8 | **matchmaker** | MMR / 队伍合并 / 排队 / bot 降级 | 强 | redis + ds_allocator |
| 9 | **trade** | 两阶段交易 / 审计 | 强 | redis + mysql + kafka |
| 10 | **dialogue** | NPC 对话树运行时 | 无(读配置) | mysql / 配置中心 |
| 11 | **ds_allocator** | 战斗 DS 调度(Agones GameServer) | 弱(etcd) | k8s + agones + etcd |
| 12 | **hub_allocator** | 大厅 DS 分片调度 | 弱(etcd) | k8s + agones + etcd |
| 13 | **battle_result** | 战斗结算消费 / 幂等落库 | 无 | kafka + mysql |
| 14 | **push** ⭐ | gRPC server stream 推送(集中持有客户端 stream + 消费 kafka 转发) | 强(连接索引) | kafka + redis(离线消息) |

⭐ = 2026-06-04 终版新增。push 是 Kratos transport/grpc 暴露的 server stream 服务,客户端通过 Envoy 连过来,详见 `gateway-decision.md` §6。

**框架统一**:13 个业务服 + push 服务**全部用 Kratos**(2026-06-04 推翻 D2.1 go-zero 决策)。Envoy 作为基础设施,不计 go 服务。

**排期说明(2026-06-06)**:`friend` / `chat` 保留在服务清单、端口和 topic 规划中,但当前不进入实现主线。它们等 UE 客户端、Hub DS、Battle DS、Agones 和核心玩法闭环完成后,再作为社交尾部功能实现。

### 3.1 业务建模原则:轻量 DDD,不做教科书 DDD

**决策(2026-06-27)**:Pandora 采用 DDD 的边界、聚合、事务边界和领域事件思想,但不把代码写成重型教科书 DDD。

完整口径见 `ddd-architecture.md`。

核心判断:
- **DDD 是业务建模方法,不是部署架构**。它回答的是「业务边界怎么划、哪些规则必须强一致、哪些变化可以最终一致、哪些概念应该成为聚合」。
- **微服务 + 事件不等于 DDD**。微服务是部署形态,事件是通信方式;如果边界没建模清楚,只是把 CRUD 拆成分布式 CRUD,会增加跨服务耦合和排障成本。
- **优先模块化边界,再按瓶颈拆服务**。账号、角色、背包、交易、匹配、房间、战斗、赛季、排行榜等先保持清晰模块边界;真正需要独立扩缩容、独立故障域或独立数据所有权时再拆服务。
- **事件优先用于旁路和最终一致**。统计、推送、审计、排行榜、补偿、异步建边等适合事件;交易扣减、资产变更、订单结算等强一致规则必须收在明确事务边界内。
- **战斗和网关不重 DDD 化**。战斗帧同步 / GAS / Replication 更偏状态机、性能和协议;网关连接层更偏网络会话管理。DDD 只用于外围业务边界,不进入 tick 热路径。

实践口径:
- 交易 / 背包 / 订单 / 资产:使用更严格的领域模型、幂等键、聚合边界和本地事务 / outbox。
- 匹配 / 队伍 / 房间:使用清晰状态机和事件,但避免过度抽象。
- 战斗结算:DS 不可信,结算权威在后端;结果落库、段位更新、奖励发放要有幂等和补偿边界。
- 日志 / 监控 / 运维:不套 DDD,保持工具化和可观测性优先。

## 4. UE 端模块(共 5 个,在 UE 仓库)

| 模块 | 用途 | 编译目标 |
|---|---|---|
| `Source/Pandora/` | 客户端(玩家本地运行) | Win64 Game / Linux Game |
| `Source/PandoraShared/` | 客户端 + DS 共用(GAS、proto、票据) | 全部 |
| `Source/PandoraHubServer/` | 大厅 DS 专属(GameMode、AOI、跨分片) | Linux Server |
| `Source/PandoraBattleServer/` | 战斗 DS 专属(GameMode、结算上报) | Linux Server |
| `Source/PandoraEditor/` | 编辑器扩展(技能数据 DataAsset 编辑器) | Editor |

## 5. 玩家流转图

```
┌─────────┐
│ Client  │
└────┬────┘
     │ 1. POST /login(账号 + 密码)
     ▼
┌─────────┐  2. 查 mysql 验证        ┌─────────┐
│  login  │ ◀─────────────────────▶ │  mysql  │
└────┬────┘                          └─────────┘
     │ 3. 调 hub_allocator 分配 hub
     ▼
┌──────────────┐  4. 查 etcd 选 hub  ┌──────────┐
│ hub_allocator│ ◀─────────────────▶│ Agones K8s│
└──────┬───────┘                     └──────────┘
       │ 5. 返回 hub_ds_addr + JWT 票据
       ▼
┌─────────┐
│ Client  │ 6. 直连 hub DS(UDP / Unreal NetDriver)
└────┬────┘
     ▼
┌──────────────┐  7. 校验票据(无状态 JWT)
│   Hub DS     │
│ (Linux UE)   │  8. 玩家在大厅走动 / 放技能 / 互打
└──────┬───────┘  9. NPC / 商店 / 交易 / 组队 → gRPC 调后端
       │
       │ 10. 玩家点"开始匹配" → gRPC 调 matchmaker
       ▼
┌──────────────┐  11. MMR 撮合 5v5
│  matchmaker  │
└──────┬───────┘
       │ 12. 凑齐 10 人 → 调 ds_allocator
       ▼
┌──────────────┐  13. Agones 拉起 battle DS pod
│ ds_allocator │
└──────┬───────┘
       │ 14. battle_ds_addr 推回 hub DS
       ▼
┌──────────────┐  15. hub DS 把地址发给客户端,断开连接
│   Hub DS     │
└──────┬───────┘
       │ 16. 客户端用新票据连 battle DS
       ▼
┌──────────────┐  17. 战斗(25 分钟)
│  Battle DS   │
└──────┬───────┘  18. 结束 → kafka 发 BattleResult
       │
       ▼
┌──────────────┐  19. 消费 → 幂等落库 → 段位更新
│battle_result │
└──────────────┘
       │
       ▼
       玩家从 battle DS 退出 → 重新连 hub DS(回大厅)
```

## 6. 协议矩阵

⚠️ **架构决策 2026-06-04 终版**:
- 客户端 **2 条连接**(① UE NetDriver / ② FHttpModule gRPC-Web over HTTP/2 TLS)
- 后端框架 **Kratos**(替代 go-zero)
- Edge Gateway 用 **Envoy**(替代 go-zero gateway)
- 推送走 **gRPC server stream**(集中 push 服务持有客户端 stream)

详见 `gateway-decision.md`。

| Caller → Callee | 协议 | 节奏 | 备注 |
|---|---|---|---|
| **Client → Envoy**(8443 HTTPS)| gRPC-Web over **HTTP/2 + TLS** | unary 1~10 req/s/玩家;stream 长连 | UE FHttpModule 自带,自研 grpc-web frame 解析 |
| **Client → Hub DS / Battle DS** | UE NetDriver(UDP-like)| 高频 30~60Hz | 仅游戏内同步,GAS / Replication |
| Envoy → 各 Kratos 业务服 | 标准 gRPC unary + server stream | 业务请求触发 / stream 长连 | k8s Service + DNS 服务发现 |
| matchmaker → ds_allocator | gRPC unary | 匹配成功一次 | 拉起战斗 DS |
| Hub DS → hub_allocator | gRPC **unary** Heartbeat | **每 5s** | 单向心跳,response 携带控制指令 |
| Battle DS → ds_allocator | gRPC **unary** Heartbeat | **每 5s** | 同上 |
| 业务服 → kafka | 生产推送事件 | 业务变更触发 | push 服务消费 |
| push → kafka | 消费推送 topics | 持续 | consumer group: pandora-push |
| Battle DS → battle_result | Kafka(at-least-once)| 战斗结束一次 | `pandora.battle.result` topic |
| 各服务 ↔ etcd | gRPC | 服务发现 / 配置 | k8s Service 也可代替 |
| 各服务 ↔ Kafka | Kafka 协议 | 异步事件 | sarama |

## 7. 关键时序

### 时序 1:玩家从 Hub 进 Battle(最复杂的链路)

```
Client    Hub DS    matchmaker    ds_allocator    Agones    Battle DS    battle_result
  │         │           │              │             │          │             │
  │ Match   │           │              │             │          │             │
  │────────▶│           │              │             │          │             │
  │         │ StartMatch│              │             │          │             │
  │         │──────────▶│              │             │          │             │
  │         │           │ (MMR 撮合)   │             │          │             │
  │         │           │ Allocate     │             │          │             │
  │         │           │─────────────▶│             │          │             │
  │         │           │              │ CreateGameSrv│         │             │
  │         │           │              │────────────▶│          │             │
  │         │           │              │             │ k8s 起 pod│            │
  │         │           │              │             │──────────▶│            │
  │         │           │              │             │          │ Ready       │
  │         │           │              │             │◀─────────│             │
  │         │           │              │  ds_addr    │          │             │
  │         │           │              │◀────────────│          │             │
  │         │           │ ds_addr+票据 │             │          │             │
  │         │           │◀─────────────│             │          │             │
  │         │  推送通知 │              │             │          │             │
  │         │◀──────────│              │             │          │             │
  │ ds_addr │           │              │             │          │             │
  │◀────────│           │              │             │          │             │
  │   断开 hub          │              │             │          │             │
  │────×    │           │              │             │          │             │
  │ 连 battle DS(带票据)                            │          │             │
  │─────────────────────────────────────────────────────────────▶│            │
  │                                                              │ 校验票据   │
  │              战斗开始(25 分钟)                              │            │
  │ ◀══════════════════════════════════════════════════════════▶ │            │
  │                                                              │            │
  │                                                              │ 战斗结束   │
  │                                                              │ Kafka 发   │
  │                                                              │──────────▶│
  │                                                              │            │ 幂等落库
  │ 客户端断开 battle DS,重连 hub DS 回大厅                                  │
```

### 时序 2:大厅内的技能命中(500 人 PvP 关键路径)

```
Client A          Hub DS                Client B (在 A 50 米内)
   │                │                       │
   │ CastAbility    │                       │
   │───────────────▶│                       │
   │                │ GAS Predict(本地)    │
   │                │                       │
   │                │ Activate Ability      │
   │                │ (服务端权威)          │
   │                │                       │
   │                │ 执行 Cost / CD        │
   │                │ 命中判定(网格 trace) │
   │                │                       │
   │                │ ApplyGameplayEffect   │
   │                │ to Target B           │
   │                │                       │
   │                │ AOI 广播 GameplayCue  │
   │                │──────────────────────▶│
   │                │                       │ 表现层(特效/音效)
   │                │ Replicate B 血量      │
   │                │──────────────────────▶│
   │ Replicate A    │                       │
   │ ability state  │                       │
   │◀───────────────│                       │
```

## 8. 部署拓扑(本地开发期)

```
开发机 (Windows F:)
├── docker-compose:
│   ├── mysql       :3307
│   ├── redis       :6380
│   ├── kafka       :9093
│   ├── etcd        :2380
│   ├── prometheus  :9091
│   └── grafana     :3001
│
├── go services(各自一个进程,monorepo go.work):
│   ├── login           :50001
│   ├── player          :50002
│   ├── data_service    :50003
│   ├── friend          :50004
│   ├── chat            :50005
│   ├── player_locator  :50006
│   ├── team            :50010
│   ├── matchmaker      :50011
│   ├── trade           :50012
│   ├── auction         :50016 (全服拍卖行 / 撮合)
│   ├── dialogue        :50013
│   ├── ds_allocator    :50020
│   ├── hub_allocator   :50021
│   └── battle_result   :50022 (kafka consumer)
│
├── minikube(本地 k8s):
│   ├── agones-system
│   └── pandora-ds:
│       ├── hub-fleet     (Hub DS pods, replicas=N)
│       └── battle-fleet  (Battle DS pods, allocate on demand)
│
└── UE 编辑器(C:/work/Pandora/)
    ├── Editor 跑客户端(PIE)
    └── Linux Server target → docker image → minikube
```

## 9. 关键不变量(任何改动都要满足,继承 CLAUDE.md §9)

1. **玩家在线只能在一个 DS**(hub 或 battle,不能两个)— player_locator 强制
2. **战斗结果幂等**(同一 match_id 只能落库一次)— battle_result 用 mysql unique key
3. **DS 票据短时效**(JWT exp 5 分钟,防止泄漏)— login 颁发,DS 校验
4. **DS 崩溃必有补偿**(Battle DS 15s 心跳超时 → `ds_allocator` 标记 abandoned → 玩家段位回滚;Hub DS 默认 30s 超时 → `hub_allocator` 标记 draining/停止分配)
5. **proto 字段编号上线后不复用**(上线后 deprecate 不删除;开发期间已删除字段可复用编号,但必须重新生成 proto 并完整编译所有已启用 module)
6. **MMR 计算在 battle_result**(不在 DS 算,DS 不可信)
7. **Snowflake 业务 ID 一律 uint64,配置表 ID 默认 uint32,proto enum / 状态常量保持生成 enum 类型或 int32 语义**;ID unsigned 规则不扩展到 `TEAM_STATE_*` / `STATE_*` / `*_REASON_*` 等枚举常量
8. **客户端只拿客户端可见结构**:不得把服务端存储快照 / 数据库整行 / Redis value 原样返回或推送给客户端;服务端按客户端当前需求的最小数据单位组装视图,必要时重新计算派生字段。

## 10. 风险登记册

| 风险 | 级别 | 缓解 |
|---|---|---|
| 500 人 hub Replication 性能 | 🔴 高 | Iris + AOI 网格 + 限流;早压测 |
| GAS + Iris 兼容性坑 | 🔴 高 | 留 2 周 buffer;不行回退 RepGraph |
| DS 崩溃数据丢失 | 🟡 中 | kafka at-least-once + 幂等 + 死信 |
| 跨 hub 分片可见性 | 🟡 中 | 先做"看不到"最简方案 |
| 防作弊 | 🟡 中 | 服务端权威 + 移动速度校验 + 审计日志 |
| UE 5.7 API 不稳定 | 🟡 中 | 关注 release notes,必要时降到 5.6 |
| 单人开发节奏 | 🟡 中 | 严格遵守 PROGRESS.md + 每日 commit |

## 11. 决策行(只追加)

| Round | 日期 | 决策 | 数据 |
|---|---|---|---|
| 0 | 2026-06-03 | 立项,新建 Pandora 项目 | - |
| 0 | 2026-06-03 | 后端 monorepo go.work,UE 独立仓库 | - |
| 0 | 2026-06-03 | 大厅 DS 化,500 人/实例,全图自由 PvP | - |
| 0 | 2026-06-03 | UE 5.7 + Iris + GAS,Agones 调度 | - |
| 0 | 2026-06-03 | License MIT,Go 1.23,基础设施全新搭一套 | - |
| 0 | 2026-06-03 | 后端框架继续用 go-zero(历史决策,后续已切换 Kratos) | - |
| 0 | 2026-06-03 | **否决"严格 A:客户端只连 DS"** | 见 `architecture-rejected-strict-ds-only.md`,6 个不可接受后果(故障域过大 / 500 人 PvP 性能预算被破 / UE 代码量爆炸 / 大厂无先例) |
| 0 | 2026-06-03 | 业务请求走独立通道(不经过 DS),具体方案待定 | 候选:WebSocket gateway / 客户端直连各 go 服务 / 专用 push 服务,详见 `gateway-decision.md`(待写) |
| 0 | 2026-06-03 | 推送方案选定 P3:**专用 push 服务** | 业务 → kafka → push(go,新增第 14 个服务)→ 客户端;Hub DS 不兼任推送中转 |
| 0 | 2026-06-03 | **RPC response 与 kafka push 乱序问题确认 = 协议设计问题**(非架构问题) | 见 `protocol-ordering-rules.md`,固化 4 个原则 |
| 0 | 2026-06-03 | 4 协议原则 | Response 同步完整 / push 不发给 caller / 已受理型显式标注 / proto 注释强制 |
| 0 | **2026-06-04** | **切换后端框架:go-zero → Kratos**(推翻 D2.1)| go-zero 不支持 gRPC stream,推送架构受限;Kratos 基于原生 grpc-go,完整支持 unary + stream |
| 0 | 2026-06-04 | 引入 **Envoy 作为 Edge Gateway** | 标准 gRPC-Web ↔ gRPC 协议转换,替代 go-zero/gateway |
| 0 | 2026-06-04 | 客户端协议:**gRPC-Web over HTTP/2 TLS** | UE 5.7 FHttpModule 已暴露(`SetOption("HttpVersion","2TLS")`),源码挖掘验证 |
| 0 | 2026-06-04 | 推送架构:**集中 push 服务 + gRPC server stream** | 替代之前规划的 WebSocket 自研 + envelope,标准 gRPC 协议 |
| W3 ⑦.0 | 2026-06-05 | **协议类型边界固化** | Snowflake 业务 ID 一律 `uint64`;配置表 ID 默认 `uint32`;proto enum / 状态常量保持生成 enum 类型或 `int32` 语义,不按非负常量改 `uint32` |
| W4 文档 | 2026-06-06 | **客户端可见结构与服务端存储快照硬隔离** | 面向客户端的 response / push 不得直接返回 `*StorageRecord`、数据库整行、Redis value、内部 Kafka envelope 或审计字段;由服务端按客户端最小需求组装 / 计算视图 |
| 0 | 2026-06-04 | 客户端实现:**自研 grpc-web 客户端基于 FHttpModule** | 不引入第三方 UE gRPC 插件(80MB+ / SSL 冲突 / UE 5.x 兼容性差);大厂(米哈游/腾讯/网易/Riot/Epic)客户端都不直连 gRPC |
| 0 | 2026-06-04 | 服务清单 13 → **14**(新增 push)| Envoy 作为基础设施不计 go 服务 |
| 0 | 2026-06-04 | 客户端连接最终值 = **2 条**(NetDriver + FHttpModule)| 用户铁律确认 |
| 排期 | 2026-06-06 | **friend / chat 暂缓到最后** | 社交好友(:50004)和聊天(:50005)当前只保留协议/端口/topic规划;实现等 UE 与核心链路全部完成后再做 |
| TLS/发布 | 2026-06-10 | **生产连接 ② TLS 使用公网 CA + 真实域名;dev mkcert 自签只通过 DebuggingCertificatePath 叠加公开 dev CA** | 玩家设备默认信任公网 CA,零配置握手;dev 的 mkcert 信任问题不带到生产。详见 `gateway-decision.md` §14 |
| ID 生成 | 2026-06-11 | **拒绝 Redis INCR 发号;当前继续静态 `node.node_id` + 本地 snowflake,未来动态多副本用 etcd Lease 分配 nodeID** | Redis INCR 慢 4~5 个数量级且有持久化/主从切换计数回退发重号风险;Redis `SETNX+TTL+看门狗` 不能可靠 fencing。etcd 方案仍需 KeepAlive/session monitor,失租必须停发并退出。详见 `infra.md` §8.1 |
| UE push | 2026-06-15 | **push stream 当前保持 AsyncTask 回传成品帧;解析器锁只保护 StreamParser 生命周期** | push 是低频事件流,双缓冲队列不能替代解析器生命周期同步;若未来追求零锁,改为每条 HTTP stream 闭包独占解析器 + 队列回传帧。详见 `gateway-decision.md` §15 |
| friend 扩展 | 2026-06-18 | **全服分片好友图不做跨玩家分布式事务,改为 request 单点权威 CAS + Kafka 异步幂等建边** | 当前 `AcceptFriend` 仅在单 MySQL `pandora_social` 内成立;Redis Cluster / 分片 MySQL 都不能原样承载跨 requester/target 原子事务。好友图权威主存推荐按 owner `player_id` 分片 MySQL,Redis 只做热点缓存。详见 `go-services.md` §2.4 |
| 存储扩容 | 2026-06-18 | **好友图扩容存储路线选 (A) TiDB 过渡;否决 (B) 分片 MySQL + dtm、(C) 其他分布式 ACID 库** | 阶段 2(千万级早期)TiDB 代码改动最小、保跨人强一致与硬上限;现阶段仍单 MySQL 不提前引入;阶段 3 极限体量再把热路径拆 §5 CAS + Kafka 异步建边卸 2PC。雪花主键热点须 `AUTO_RANDOM` 打散。详见 `friend-distributed-scaling.md` §8 / §14 |
| 存储扩容 | 2026-06-18 | **人工拍板推翻“不提前引入”:现就把 friend(及同库 chat)切 TiDB** | 项目内已落地:TiDB 版 `pandora_social` DDL(§8.2 热点调优)+ friend TiDB 连接配置;Go 业务零改动(TiDB 兼容 MySQL 协议)。起集群 / 装载 DDL / 数据迁移 = Codex / 人(§11.1)。详见 `friend-distributed-scaling.md` §14 “落地修订” + `deploy/tidb-init/README.md` |
| 全服扩容 | 2026-06-19 | **DAU 200万全区全服:node_id 是 snowflake 机器号非选区(不删);单 Redis→Cluster、单 MySQL→分库、nodeID etcd 自动分配、push 横扩、Agones 池化** | 抗压取决于 CCU(~30万)非注册量(1000万)。已落地能力:`pkg/snowflake/etcdnode`(etcd Lease 抢 nodeID)、`redisx.NewUniversalClient`(Cluster)、`mysqlx.ShardSet`(分库),均非破坏式。push 横扩走定向路由(注册表+分区),Agones 走 Ready 池化。社交库仍走 TiDB 不套 ShardSet。后续被 `scale-cellular-20m.md` 继承为单 Cell 扩容口径。 || 拍卖行 | 2026-06-19 | **trade 需要全服拍卖行 / 跨人撮合 → 新增独立 `auction` 服务(economy 域,与 trade 平级),不塞进 trade** | 拍卖是「每 market 单写者」交易所模型:MySQL `pandora_auction` 按 market_id 分片为权威(不跨分片事务,故不需 TiDB),Redis ZSET 订单簿做活跃撮合索引;两层幂等(挂单 idempotency_key + 结算 match_id);进程内 per-market 互斥串行不超卖,跨实例一致性哈希路由留扩容。端口 50016/51016,errcode 12000-12999,topic pandora.auction.match/audit。详见 `decision-revisit-auction-engine.md` |
| 全服扩容 | 2026-06-26 | **【已拍板·落地起步】DAU 目标上调 200万→2000万(10×,峰值 ~600万 CCU/~15×):Region→Cell→Cell 内分片 三层** | 两道墙:单逻辑集群 ~40万 CCU + 单一全局协调层(~20 Cell 时)。**人拍板 6 项**:单 Cell 锚 40万 CCU / 3 个 Region / 逻辑分片 cell 4096+region 64 / 允许跨 region 匹配(两级撮合,结算回 owner cell)/ auction 跨 region 全局市场(按 market_id 分片)/ 一步到位。玩家路由三层 `region_route→cell_route→Cell 内 CRC16·player_id%N`,全程算不查;**region 由 cell 派生**结构性保证「同一 player owner 落同一 region+cell」。已落地 `pkg/cellroute`(确定性路由地基 + 静态映射表 + 校验,build/vet/test=0)+ `pkg/cellroute` 热更新(AtomicTable 原子整表替换 + 纯解码)+ 隔离子 module `pkg/cellroute/etcdtable`(etcd watch,镜像 etcdnode,待 Codex tidy)+ login 接线(LoginUsecase nil-safe Router,登录算 region/cell;login.proto 加 region_id/cell_id 待 Codex proto_gen)。跨 region 撮合边界已细化 `decision-revisit-global-matchmaker.md`(两级撮合:region 内 MMR 池 + 跨 region 段位桶溢出池,结算回 owner cell)。基础设施(多 k8s/Agones 池化/push 横扩/TiDB·Kafka 集群)按 §11.1 由 Codex/人接。详见 `scale-cellular-20m.md` |
| 内网安全 | 2026-07-03 | **玩家链路(连接 ②)确认全程 TLS 无明文;内网东西向保持明文 h2c,加密下沉基础设施层;生产 online overlay 加分层 NetworkPolicy(default-deny-ingress + 业务层 mesh + 存储层按端口收敛)** | 内网明文由基础设施兜底是主流实践(应用零改动、无证书运维);本次落地分层 NetworkPolicy 作 K8s 生产最低门槛,收敛「跨 ns」与「存储层」横向(业务服之间暂全通待 mesh),只做 Ingress default-deny(Egress 暂全通),只挂 online 不挂 dev(避免挡宿主 Envoy 联调),需 CNI 强制(Calico/Cilium)。未来内网加密走 mesh sidecar mTLS 而非应用层手写。详见 `gateway-decision.md` §16 |
| 架构建模 | 2026-06-27 | **采用轻量 DDD 思想,不把“微服务 + 事件”误认为 DDD** | DDD 是业务建模方法,不是部署架构;微服务是部署形态,事件是通信方式。Pandora 保持模块化边界优先,交易/背包/资产等强一致模块严肃建模,匹配/队伍/房间用状态机和事件,战斗 tick/网关连接层不重 DDD 化。详见 §3.1 |
| 配置热更 | 2026-06-30 | **配置表热更走自研轻量流水线,Apollo / Nacos 现在不接** | **核心做法(标准):版本号 + checksum + staging 目录 + reload 接口 + 加载成功才切换 + 失败保留旧配置**。`Table → JSON → 校验 → staging → 通知 → 原子热加载 → 失败回退`,本质是配置发布流水线非分布式配置中心。发布通知用 **etcd**(复用现有 `etcdtable`/`etcdnode`,只存 version 键不存表体;单机/dev 直接调 reload RPC)。Apollo/Nacos 仅在「多环境平台化 / 权限审批 / Web 控制台回滚灰度 / 多机统一刷新」触发时再评估,且只管发布通知/版本号,不存大量表 JSON。proto 读 JSON 需钉死「字段名对齐 / 64 位整数字符串 / 未知字段策略」三件事(配置表 ID 默认 `uint32` 不踩 64 位字符串坑)。详见 `config-table-hotreload.md` |
| 镜像分发 | 2026-07-02 | **运行镜像改 scratch 基底;受限内网机器走「离线镜像包随仓库同步」过渡方案(非大厂标准,后续迁 Harbor)** | 业务运行阶段由 alpine 改 `scratch`(builder 拷 ca 证书+时区,免联网 `apk`,离线可 build,镜像 29~42MB)。拉不到 Docker Hub/加速站的内网机不联网起服务:`export_images.ps1 -Build` 打 17 业务镜像 tar(~144MB)→ 放 `deploy/offline-images/` 随 git/svn 同步 → 目标机 `import_images.ps1` `docker load`。**这是「无内网 registry + 目标机拉不到公网镜像」时的务实过渡,非长期正规做法**;正规做法是内网 Harbor/Nexus + `docker pull`,源码仓只放文本。有 Harbor 后本方案退役。纪律:只覆盖同名 tar 不堆版本;仅放 scratch 小镜像,基础设施大镜像不入库。详见 `deploy/offline-images/README.md` |
| 镜像构建 | 2026-07-03 | **镜像构建保留两种可选方式:A=容器内 `go build`(默认)/ B=宿主交叉编译再打包;由 `-BuildMode` 选,产出同名同烙印镜像** | 痛点:方案 A 的 `deploy/services/Dockerfile` 把 `COPY . .` + `go build` 放同一 `RUN`,改任意源码即失效该层、重下依赖重编全仓(单服务重建~分钟级);且本仓刻意不用 BuildKit `RUN --mount`(内网离线怕拉 dockerfile frontend)。**方案 B(`Build-Images-Host`)**:宿主 `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath`(享受宿主增量缓存,单服务重建秒级)→ 产物+etc/ 放 `run/docker-build/prebuilt/<name>/` → 用 `deploy/services/Dockerfile.prebuilt`(两段:golang 取 CA/时区 + scratch 只 COPY,极小上下文秒级)打成 `pandora/<name>:dev`。**两方式产镜像同名、版本烙印(`-ldflags -X version.*`)一致**,可无缝喂 compose / `export_images.ps1 -BuildMode host`。入口:`start.ps1 -BuildMode incontainer\|host [-Only <连字符名>]`、`export_images.ps1 -Build -BuildMode host`、双击 `重建镜像-选打包方式.cmd`(选方式+服务)。默认仍 `incontainer` 保持既有行为;host 需本机装 Go。离线短路判定按模式泛化:host 判本机有 `go`,incontainer 判有 golang 基础镜像。**BuildKit cache mount / 内网私有 GOPROXY 是后续更彻底优化(未做)**。详见 `deploy/offline-images/README.md`、`/memories/repo/image-build-modes.md` |
