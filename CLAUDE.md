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

    **受管的客户端触发型内存容器**：UE DS 的已消费 DSTicket JTI cache 虽不对客户端分页展示，也必须
    按同一有界纪律维护：`JTI→exp+leeway` 到期清理，硬上限为
    `min(effective MaxPlayers×每玩家窗口预算, configured absolute max, 65536)`；满载 fail-closed，
    禁止驱逐未过期 JTI 后重新允许重放。契约见 `docs/design/agones-dev.md §5`。

## 10. AI 协作约定

AI 协作规则以 [`AGENTS.md`](./AGENTS.md) 为准,本文件不重复维护细则,避免双文档漂移。

## 11. UE 工程约束(写给 UE 仓库开发者参考)

1. 类前缀统一 `Pandora*`(GameMode / Character / PlayerController)
2. 服务端逻辑统一在 `PandoraHubServer` / `PandoraBattleServer`,不在 `Source/Pandora/` 客户端模块
3. 蓝图只做"胶水"(挂技能动画 / UMG 绑定),逻辑在 C++
4. 资源走 Git LFS(`.uasset / .umap / .fbx / .png / .wav / .ogg`)
5. **永远不要提交** `Binaries/ Intermediate/ DerivedDataCache/ Saved/`

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
