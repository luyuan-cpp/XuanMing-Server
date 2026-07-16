# Pandora 项目规范

> 本文档是 Pandora 项目的"宪法",AI 协作和人类开发都须遵守。后端项目,适配 MOBA + UE DS + 双仓库架构。

## 1. 项目基本信息

- **类型**:MOBA(5v5)+ 持续在线大厅(全图自由 PvP,500 人/hub 实例)
- **后端**:Go(14 个服务 + 公共框架 pkg/)
- **客户端 + DS**:UE 5.7 + GAS + Iris,**独立仓库**(本仓库 `Pandora` 是后端)
- **DS 编排**:Agones on k8s
- **协议**:gRPC(同步) + Kafka(异步事件)
- **基础设施**:MySQL 8 + Redis 8 + Kafka 3 + etcd 3

## 2. 仓库结构与边界

```
E:/work/Pandora/                # 后端（本仓库）
UE 客户端 + DS                  # 独立仓库，工程统一为 Pandora
```

- UE 工程 / 模块 / 类命名统一为 `Pandora`
- proto cpp pb 同步目标仓库为 Pandora-Client（具体输出路径待接 buf.gen.cpp.yaml）

## 3. 中文回复

所有 AI 协作产出**用中文**。注释、commit message、文档全中文。

## 4. 提交纪律

1. commit message 格式:`<type>(<scope>): <subject>`
   - type:feat / fix / refactor / test / docs / chore / perf
   - scope:服务名(login / matchmaker)/ pkg / docs / deploy
   - 例:`feat(matchmaker): MMR 撮合算法初版`
2. proto 改动要在 commit message 标注 `[proto]`,提醒同步到 UE 仓库
3. PR 描述必须含:动机 / 改动范围 / 测试方式 / 风险点

## 5. proto 同步流程(双仓库)

1. proto 改动后由 Codex 跑 `pwsh tools/scripts/proto_gen.ps1` 生成 go pb
2. cpp pb 同步到 UE 仓库 `Source/Pandora/Generated/Proto/`,由 Codex 执行
3. UE 客户端改动跟随后端 proto 同步,由 Codex 协助
4. 字段编号:上线后**不复用**,只能 deprecate(`reserved 5;` + 注释原因);开发期已删字段可复用编号,但须重生 proto 并完整编译所有启用 module
5. `player_id` / `team_id` / `match_id` / `order_id` / `message_id` / `dialogue_id` / `hub_id` / `invite_id` 等 Snowflake 业务 ID **一律 `uint64`**,不准用 `int64` / `string`;未知/空值用 `0`,需 presence 时用 `optional uint64`
6. 配置表 / 静态表 ID **默认 `uint32`**(`npc_id` / `hero_id` / `skill_id` / `item_config_id` / `map_id` 等);易与运行时实体混淆时新协议命名为 `<entity>_config_id`
7. 状态 / 类型 / 原因等 proto 枚举**不属 ID 规则**;enum 底层是 `int32`,Go 优先用生成的 enum 类型,不因取值非负改 `uint32`
8. 新增业务数据结构**优先定义 proto message**,按下表四类各司其职,**不准手写与 proto 重复的并行 struct**:

   | 类别 | 命名 | 用途 |
   |---|---|---|
   | RPC 请求/响应 | `<Verb><Domain>Request` / `<Verb><Domain>Response` | gRPC unary/stream 出入参 |
   | 客户端可见结构 | `<Domain>` / `<Domain><Part>`(短名,如 `Team` / `TeamMember`) | RPC response、push payload 里给客户端看的字段 |
   | 服务端存储快照 | `<Domain>StorageRecord` + 子结构 `<Domain><Part>StorageRecord` | Redis value、Kafka 快照、MySQL **blob 列**里序列化成 bytes 的整块状态 |
   | 服务间事件 | `<Domain><Action>Event` | Kafka payload;可内嵌"客户端可见结构",但本身是服务内部消息,不是存储快照 |

9. 第 8 条的"快照用 proto bytes"**只针对快照/blob 场景**(Redis value、Kafka payload、MySQL blob 列):关系型 MySQL 表(结构化列)不强制 proto 化,列直接映射 proto 字段;临时小令牌(如 invite,2~3 字段、短 TTL)允许继续用 redis hash。核心是"消灭与 proto 漂移的并行 struct",不是"一切序列化成 bytes"
10. proto message 直接当存储 record 时:**禁止值拷贝**(`a := *rec` 会复制 state/mu/sizeCache),克隆一律 `proto.Clone`;存储与客户端结构**分两个 message**,存储侧独有字段(如 `updated_at_ms`)不外露
11. **禁止把存储快照原样返回/推送给客户端**。RPC response / push 只能用"客户端可见结构",由服务端从 `StorageRecord`/MySQL 行/Redis 状态按**最小数据单位**填充,必要时重算派生字段(ready、queue_seconds、mmr_delta、显示昵称)。例外只能是写入设计文档的运维/调试 RPC,且须鉴权、脱敏、不经 Envoy 对客户端开放。
12. **非负整型字段默认用无符号类型**:语义上不可能为负的整数(数量、计数、时长毫秒、容量、索引、金额、版本号、时间戳 ms、序号等)默认用 `uint32` / `uint64`,不用 `int32` / `int64`。选型:预期不超过 ~42 亿用 `uint32`,可能更大或属 Snowflake 业务 ID 用 `uint64`(遵循第 5、6 条)。**例外(仍用有符号)**:①语义上可能为负的差值 / 增量(如 `mmr_delta`、坐标偏移、余额变动 `delta`);②参与减法且可能下溢的字段(用有符号避免无符号回绕);③已属枚举 / 状态 / 原因(见第 7 条,保持 int32 语义)。跨语言注意:JSON / JS 场景 `uint64` 仍按字符串编码,与现有 ID 规则一致。

## 6. 服务命名 / 端口规范

详见 [`docs/design/infra.md`](./docs/design/infra.md)。**不允许 ad-hoc 起端口或 key**。

## 7. 决策记录入口

`CLAUDE.md` 只保留稳定规范和索引,不再维护长决策表,避免每次会话重复消耗 token。

- 架构级决策:见 [`docs/design/pandora-arch.md`](./docs/design/pandora-arch.md) §11。
- 服务级决策:写入对应 [`docs/design/<service>.md`](./docs/design/) 或服务 README。
- 压测结论:写入 `docs/design/stress-<round>-*.md`。
- 周期进度与流水账:追加到 [`PROGRESS.md`](./PROGRESS.md),只追加不删旧条目。

## 8. 压测纪律

详见 [`docs/design/stress-discipline.md`](./docs/design/stress-discipline.md)。**核心**:跑测前必有 `prev-summary.txt` 且清空 redis/mysql/etcd/kafka offset/k8s GameServer;至少 3 次 prom snapshot(ramp 完/稳态中/稳态末);用 summarize 脚本输出五段二维表,不手 grep raw prom;没对比表不准声明"性能提升";压期不上传日志。

## 9. 不变量(数据一致性 / 安全)

跨服务必须保持的不变量。任何改动违反这些 → PR review 直接拒。

1. **玩家在线只能在一个 DS**(player_locator 强制)
2. **战斗结果幂等**(同一 match_id 只落库一次)
3. **DS 票据短时效**(JWT exp 5min)
4. **DS 崩溃必有补偿**(Battle DS 15s 心跳超时 → abandoned → 段位回滚;Hub DS 默认 30s 超时 → draining/停止分配)
5. **proto 字段编号上线后不复用**;开发期间已删除字段可复用编号,但必须重新生成 proto 并完整编译所有已启用 module
6. **MMR 计算在 battle_result**(DS 不可信)
7. **交易资源扣减必须原子 + 有补偿幂等键**
8. **所有写都要带 trace_id**
9. **kafka topic key = 业务实体 ID**(同一玩家 / 同一对局事件有序)
10. **Redis lock TTL ≤ 30s**,业务跑完主动释放
11. **Snowflake 业务 ID 一律 uint64**(`player_id` / `team_id` / `match_id` / `order_id` / `message_id` / `dialogue_id` / `hub_id` / `invite_id` 等),不准新增 `int64` / `string` 型业务 ID
12. **配置表 ID 默认 uint32**(`npc_id` / `hero_id` / `skill_id` / `item_config_id` / `map_id` 等),不准新增有符号配置 ID
13. **proto enum / 状态常量保持 enum/int32 语义**(`TEAM_STATE_*` / `STATE_*` / `*_REASON_*` 等),不准因枚举值非负改成 `uint32`
14. **客户端只拿客户端可见结构**:任何面向客户端的 response / push 不准直接返回 `*StorageRecord`、数据库整行、Redis value、内部 Kafka envelope 或内部审计字段;必须经服务端组装成最小视图,只包含客户端渲染 / 交互所需字段。
15. **配置表热更走标准流水线**:**版本号 + checksum + staging 目录 + reload 接口 + 加载成功才切换 + 失败保留旧配置**。新表加载+校验全过才原子替换内存指针,任一步失败保留旧表不影响线上;version 单调递增防回退;发布通知用 etcd version 键(复用 `etcdtable`/`etcdnode`,不存表体),**不引入 Apollo / Nacos**。详见 `docs/design/config-table-hotreload.md`。
16. **服务必须支持不停服更新(零停机滚动更新)**:go 服务全部 headless、无状态、可水平扩展,权威态在 Redis/MySQL/etcd,进程内只做缓存/Actor 邮箱,收到 `SIGTERM` 先摘流量→排空在途→flush write-behind 脏数据→退出,任意副本可随时被杀被替换。**任何依赖「先停服再启动」才能上线或才能读数据的设计一律拒**。
17. **Redis 二进制 pb 存储只做兼容演进,支持不停服加玩家数据**:滚动更新期间新旧版本副本同时在线,存储 pb 必须**双向兼容**——只允许**加新字段(新编号)/ 加 enum 值(带 `*_UNSPECIFIED` + fallback)/ `reserved` 删字段**;**禁止**改 field number、改类型、改基数/语义、复用 reserved 编号。**read-modify-write(读 Redis→改→写回)路径禁止 `DiscardUnknown` 丢弃 unknown fields**(否则旧副本回写会静默丢新字段)。加玩家数据 = 加 proto 字段 + 懒迁移(下次改写自然补齐或不停服 backfill),**不停服、不全量刷库**。详见 `docs/design/zero-downtime-update.md`。
18. **客户端可写入的累积列表必须有上限**:凡客户端能主动新增、可长期堆积、又会被客户端拉取展示的列表,必须同时具备**写入侧总量上限**和**读取侧分页 / 单次返回上限**。包括但不限于:好友、好友申请、公会申请、公会成员、组队 / 入群邀请、临时群成员、我所在的群、交易请求、黑名单、订阅 / 关注 / 待处理队列。只做唯一键防重复不够;只做列表分页也不够。新增此类功能时必须在服务端事务 / 原子写路径里校验单玩家、单目标或单实体的 pending / active 数量上限,配置有默认值,超限返回明确业务错误,并在 proto 列表接口提供 `cursor+limit+next_cursor` 或等价分页(列表被写入侧硬上限兜住到几百内时,单次全量返回 + 服务端 `SQL LIMIT` 兜底即算「单次返回上限」达标)。没有上限的方案一律拒。

    **现存受管列表清单**(新增同类列表照此登记 + 落上限):

    | 列表 | 写入侧总量上限 | 读取侧上限 | 超限错误码 |
    |---|---|---|---|
    | 好友列表 | `max_friends` 默认 200(AcceptFriend 事务原子校验) | `ListFriends` SQL LIMIT | `ERR_FRIEND_LIMIT` |
    | 好友申请(收件箱) | `max_incoming_requests` 默认 200(CreateRequest 事务校验 target pending 数) | `ListFriendRequests` SQL LIMIT | `ERR_FRIEND_REQUEST_LIMIT` |
    | 黑名单 | `max_blocks` 默认 200(Block 事务校验) | `ListBlocks` SQL LIMIT | `ERR_FRIEND_BLOCK_LIMIT` |
    | 公会成员 | `max_guild_members` 默认 100(ApproveJoin 事务原子校验) | `ListMembers` cursor 分页 | `ERR_GUILD_FULL` |
    | 公会申请(每公会 pending) | `max_pending_requests_per_guild` 默认 200(CreateJoinRequest 事务校验) | `ListJoinRequests` cursor 分页 | `ERR_GUILD_REQUEST_LIMIT` |
    | 临时群成员 | `max_group_members` 默认 50(建群 / AddMember 事务原子校验) | `ListGroupMembers` SQL LIMIT | `ERR_GROUP_FULL` |
    | 我所在的群 | `max_groups_per_player` 默认 50(建群 / AddMember 事务校验) | `ListMyGroups` SQL LIMIT | `ERR_GROUP_JOIN_LIMIT` |
    | 交易订单(单玩家参与,买/卖两侧各计) | `max_orders_per_player` 默认 200(CreateOrder 用 Lua SCARD+SADD 原子预留双方反查索引名额;满时惰性清理已终态/已回收成员再重试一次) | `ListMyOrders` cursor 分页 + SMEMBERS 全量被写入侧硬上限兜住 | `ERR_TRADE_ORDER_LIMIT` |
    | 拍卖订单(单玩家 PENDING + 活跃) | `max_active_orders_per_player` 默认 200(Claim PENDING 后用 Redis Lua SCARD+SADD 原子预留含 market_id+order_id 的成员;满时按 MySQL 权威状态有界惰性清理) | `ListMyOrders` 按全局 order_id cursor 分页,默认 50/最大 100 | `ERR_AUCTION_ORDER_LIMIT` |

    **受管的客户端触发型内存容器**：UE DS 的已消费 DSTicket JTI cache 虽不对客户端分页展示，也必须
    按同一有界纪律维护：`JTI→exp+leeway` 到期清理，硬上限为
    `min(effective MaxPlayers×每玩家窗口预算, configured absolute max, 65536)`；满载 fail-closed，
    禁止驱逐未过期 JTI 后重新允许重放。契约见 `docs/design/agones-dev.md §5`。

19. **DS 过渡全程不卡玩家（no-freeze，需求级硬性要求）**：玩家在「登录 → 选角 → 进 Hub DS」或
    「匹配 → READY → 进 Battle DS」的**任意时点**切后台、断网或杀进程，回到前台/重启后**必须**能在
    有界时间内自动恢复，或停在带真实可点入口（重试/放弃）的显式 UI，并最终只进入唯一权威 DS 或
    显式登录态；**任何阶段都不允许出现无人驱动的静默等待（卡死）**。落地约束：
    - 客户端所有 DS `ClientTravel` 只允许 `UMyDsRecoveryCoordinator` 单写者发起；每个异步回调必须过
      generation + request-seq + phase 三元校验，迟到回调零副作用。
    - 服务端对 UNKNOWN placement 一律 fail-closed 且零副作用（不签票、不占座、不开 spawn gate），
      客户端只能退避重查；客户端本地超时/本地状态**不得**改写权威路由。
    - 每个恢复等待状态必须有 ticker / 回调 / 前台事件继续驱动；HTTP unary 必须有界超时且保证回调
      恰好一次。无会话上下文（从未登录 / 显式登出 / 主动放弃重连）时，前台恢复不得触发任何强制
      关卡切换或静默重放登录。
    需求与验收记录见 `docs/design/battle-reconnect.md` §7（含 §7.11 复核）与
    `docs/reviews/ds-recovery-claude-audit.md`；违反本条的改动 PR review 直接拒。

20. **任何代码都不能让玩家卡死或进不去场景（全局硬性要求）**：本条覆盖所有登录、选角、匹配、组队、传送、加载、重连、前后台切换、地图切换以及进入 / 返回 Hub DS、Battle DS 的路径，不限于第 19 条已有流程。任何异步状态都必须有持续驱动和有界超时；失败、断网、依赖不可用、回调丢失 / 迟到、进程重启或状态不一致时，必须在有界时间内自动恢复，或进入带明确原因和**真实可用**重试 / 返回 / 放弃入口的 UI。禁止永久黑屏、无限 loading、无超时等待、按钮不可用、只能杀进程恢复、服务端残留状态导致永远无法重新进场，以及客户端和服务端互相等待。新增或修改进场相关代码必须验证成功、超时、断网、重复回调、迟到回调、切后台、重启和部分失败路径；无法证明满足本条的改动不得合入。

21. **所有 Go 服务和 Hub / Battle DS 都必须支持金丝雀发布新版本且不停服（全局硬性要求）**：Go 服务通过 stable / canary 新旧副本并存、readiness、按比例或按人群分流、优雅摘流量和在途排空发布；RPC、事件和存储格式必须在共存窗口双向兼容，异常时能立即把 Canary 权重归零，Stable 持续服务。DS 通过 Agones Stable / Canary 独立 Fleet 发布：Canary 必须先预热 Ready 后才接小比例**新分配**，同一玩家 / 对局固定 release track；回滚先停止 Canary 新分配，禁止删除或强杀仍承载玩家的 Allocated DS。旧 Battle DS 必须把当前对局跑完，旧 Hub DS 必须停止接新并安全迁移 / 排空玩家，确认空场后才退役。任何升级都不得要求全服停机、踢掉在场玩家、打断对局或让玩家无法进入场景；详见 `docs/design/zero-downtime-update.md` §6.3。

22. **状态优先查询唯一权威，不重复存储影子状态（全局硬性要求）**：先明确每个状态的唯一权威源，只在该处保存跨请求 / 重启仍影响正确性且无法可靠重建的**最小权威事实或短期租约**；其他服务需要状态时查询权威接口，不得各自持久化并信任一份副本。可由权威事实计算的展示状态、聚合状态和派生字段按需查询 / 计算；缓存与物化视图必须明确为非权威、带 TTL / version、可失效且可从权威源重建，不能参与扣减、准入、归属切换等权威写决策。关键迁移禁止普通“先查再存”（存在 TOCTOU 竞态），必须通过事务、CAS、条件更新、唯一键或等价原子机制完成检查与修改。查询缺失只有在契约明确时才能推导状态；查询超时、依赖故障或结果不确定必须返回 `UNKNOWN` / `UNAVAILABLE` 并重试或 fail-closed，禁止冒充 `OFFLINE`、空闲、成功或其它默认状态。例：`LOCATION_STATE_HUB` 只由 Hub DS 上报，在 player_locator 以 `HUB + hub_pod + shard_id + TTL` 保存为短期权威位置租约；从 Battle 回流时携带的 `match_id` fence 只参与原子迁移校验，通过后不持久化。其他服务一律调用 `GetLocation` / `BatchGetLocation`，不得把 `HUB` 复制到自身数据库长期信任。详见 `docs/design/go-services.md` §2.6。

## 10. AI 协作约定

AI 协作规则以 [`AGENTS.md`](./AGENTS.md) 为准,本文件不重复维护细则,避免双文档漂移。

## 11. UE 工程约束(写给 UE 仓库开发者参考)

1. 类前缀统一 `Pandora*`(GameMode / Character / PlayerController)
2. 服务端逻辑统一在 `PandoraHubServer` / `PandoraBattleServer`,不在 `Source/Pandora/` 客户端模块
3. 蓝图只做"胶水"(挂技能动画 / UMG 绑定),逻辑在 C++
4. 资源走 Git LFS(`.uasset / .umap / .fbx / .png / .wav / .ogg`)
5. **永远不要提交** `Binaries/ Intermediate/ DerivedDataCache/ Saved/`
6. **UE 编译由用户本人执行**：I can compile UE myself. AI 改完 UE 代码后不必代跑 UE 编译（本机编辑器常开 Live Coding 会阻塞 UBT），把 UE 编译/测试验证交给用户。

## 12. 不要做的事

- ❌ 不要在 docs/design/ 之外随便建 README(集中维护)
- ❌ 不要 import 第三方 GUI 库到 go 服务(go 服务都是 headless gRPC)
- ❌ 不要把 player_id 当 prometheus label(高基数会爆)
- ❌ 不要在 W1 写业务逻辑,只搭骨架
- ❌ 不要混用 `Pandora` / `pandora` / `MOBA` / `moba` 命名 — 见 §13 命名大小写规则
- ❌ **不要做破坏不停服更新的改动**:不要给 Redis pb 存储改字段编号/类型/语义、不要在 read-modify-write 路径丢弃 unknown fields、不要设计「必须停服才能上线/读数据」的方案 — 见 §9 不变量 16/17、`docs/design/zero-downtime-update.md`
- ❌ **UE 侧不要再用 `Xuanming` / `Xm` 命名任何工程 / 模块 / 类 / 文件 — 一律 `Pandora`**(见 §11.1)
- ❌ **不要交半成品**(TODO 占位 / 空实现 / “先留个钩子以后接”)— 见 §14 接线完整性铁律

## 13. 命名大小写规则(强制)

- **Pandora**(首字母大写):仓库名 / 本地路径 / 工程类前缀 / 文档项目名引用 / **UE 工程 / 模块 / 类前缀**
- **pandora**(全小写):kafka topic / mysql / redis key / docker 镜像 / go module
- **`Pandora-Client`**(CapitalCase,带连字符):UE 客户端仓库名。⚠️ **不要和 JWT audience `pandora-client`(全小写)混淆** —— 后者是 envoy / login / auth 配置里的鉴权受众,改仓库名时**绝不能**动它
- **`Xuanming` / `Xm`**:**已废弃命名**,**代码 / 工程 / 类 / 模块一律不再使用**

## 14. 接线完整性铁律（强制）

新功能 / 新能力接线**一次做到最终可上线版本**，不准留半成品。

1. **不准留 TODO 占位 / 空实现 / “以后再接”**：要么不动，要动就把功能链路全部接完（static 与目标模式两条路径都可用）。
2. **允许用配置开关默认关闭新行为**（临时开关 / feature flag），但默认值必须保证现有行为不变 / 一键启动不坏；开关打开后的分支必须是**完整可用的真实实现**，不是空壳。
   - 例：snowflake nodeID 由 `snowflake.node_id_source` 开关驱动：`""`/`static` 静态（默认，单副本/dev），`etcd` 自动抢占（多副本）。两条路径均已在 `etcdnode.MustProvideSnowflake` 内完整实现（含失租 fencing 退出），各服务 main.go 一行接入。
3. **隔离重依赖的独立 pkg module（如 `pkg/snowflake/etcdnode` / `pkg/killswitch/etcdkv` / `pkg/cellroute/etcdtable`）被服务 import 后**：Claude 负责写代码 + 补 go.mod 的 require/replace；`go mod tidy` 拉依赖生成 go.sum 是 **Codex 的活**（AGENTS.md §11.1）。接线后必须在交接里列出“哪些服务需 tidy”，不准默声留着让下个 AI 碰到 build 红才发现。

## 15. 设计简单性与标准化（强制）

设计与实现必须优先选择**简单、标准、直达、可维护**的方案。在满足正确性、安全性、性能、数据一致性、不停服更新等硬约束的前提下，按以下顺序取舍：

1. **标准能力优先**：先用 Go / UE、Kratos、gRPC、Envoy、MySQL、Redis、Kafka、etcd、Kubernetes 等现有技术栈的官方能力和业界通行模式；已有成熟标准能解决时，不自研协议、框架或基础设施。
2. **最少复杂度优先**：能用一个清晰模块解决，不拆成多层；能同步完成，不引入异步链路；能用本地事务完成，不升级为分布式事务；能复用现有组件，不新增中间件。这里的选择必须同时满足既有架构边界和硬性不变量，不能以“简单”为由牺牲正确性。
3. **拒绝预设性复杂化**：不得只因“以后可能扩展”“看起来更高级”而提前增加接口层、适配层、工厂层、事件总线、状态机、缓存、分片、兼容层或配置开关。出现真实需求、压测数据、故障证据或明确扩容门槛后再引入。
4. **复杂度必须举证**：确需采用更复杂方案时，设计说明必须写清简单方案不满足的已确认约束、复杂方案新增的组件和失败模式、验证方法及回退路径；无法说明则采用更简单的标准方案。
5. **代码路径保持直达**：命名表达业务含义，控制流和调用链尽量短，避免无意义封装、重复抽象和层层转发。抽象必须消除真实重复、隔离明确变化或守住架构边界，不能只为形式上的“分层”。

简单不等于简陋，也不允许绕过 `CLAUDE.md §9` 不变量、测试、错误处理、可观测性或 `§14` 接线完整性。目标是在完整可靠的前提下，用最少必要机制解决当前已确认的问题。

## 16. 隐蔽 bug 与分布式正确性（强制）

设计、实现、测试和 review 不能只覆盖正常路径，必须以“故障一定会发生、请求一定会重复、消息可能乱序、多副本会并发”为前提，主动寻找不易复现的边界 bug。目标是交付时没有已知 bug；任何未验证路径不得凭感觉声称“无 bug”或“分布式安全”。

1. **并发与原子性**：检查竞态、丢失更新、重复执行、ABA、检查后执行（TOCTOU）、锁过期和跨 Redis slot / 跨库非原子问题。共享写必须明确事务或原子边界；锁只能降低冲突时，最终正确性仍须由数据库条件更新、唯一键、CAS、Lua 或等价权威机制保证。
2. **幂等与顺序**：所有可能重试的写入、消息消费、回调和补偿必须有稳定幂等键；明确同一实体的顺序保证、版本号 / revision / fence 校验及迟到消息处理。不能假设 RPC、Kafka、定时任务或客户端回调只执行一次、严格有序或永不丢失。
3. **超时与部分失败**：逐项考虑请求已生效但响应丢失、跨服务只完成一半、依赖超时后迟到成功、补偿本身失败等情况；明确重试上限、退避、可恢复状态、补偿闭环和人工审计入口，禁止静默吞错或留下永久中间态。
4. **多副本与故障恢复**：覆盖进程崩溃、重启、主从切换、网络分区、租约丢失、时钟偏差、多实例重复调度和滚动升级中新旧版本共存。进程内状态不能被当作唯一权威；失去租约 / ownership 后必须 fencing，旧实例不得继续写。
5. **容量与退化边界**：检查空值、零值、最大值、溢出 / 下溢、分页边界、集合硬上限、队列积压、缓存击穿 / 雪崩、热点 key 和依赖不可用时的行为。降级不得破坏数据正确性、安全边界或 `§9` 不变量。
6. **验证必须对应风险**：每个已识别风险至少有一种可复现验证：单元测试、并发测试、race 检测、集成测试、故障注入、重启恢复测试或端到端测试。修 bug 必须补能在修复前失败、修复后通过的针对性回归测试；无法本地验证的路径必须在交接中明确列为剩余风险和人工验收项。

分布式正确性不能靠增加层数或复杂框架自动获得。仍须遵守 `§15`：优先用最简单的标准机制建立可证明的权威、原子、幂等、fencing 和恢复闭环。
