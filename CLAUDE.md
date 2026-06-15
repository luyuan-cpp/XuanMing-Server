# Pandora 项目规范

> 本文档是 Pandora 项目的"宪法",AI 协作和人类开发都必须遵守。
> Pandora 后端项目规范,适配 MOBA 玩法 + UE DS + 双仓库架构。

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

1. proto 改动后由 Codex 执行 `pwsh tools/scripts/proto_gen.ps1` 生成 go pb
2. cpp pb 同步到 UE 仓库 `Source/Pandora/Generated/Proto/` 的操作由 Codex 执行
3. UE 客户端改动跟随后端 proto 同步,由 Codex 协助处理
4. 字段编号规则:上线后**不复用**,只能 deprecate(`reserved 5;` + 注释原因);开发期间已删除字段可复用编号,但必须重新生成 proto 并完整编译所有已启用 module
5. `player_id` / `team_id` / `match_id` / `order_id` / `message_id` / `dialogue_id` / `hub_id` / `invite_id` 等 Snowflake 业务 ID **一律用 `uint64`**;不准再用 `int64` / `string` 承载这类 ID。未知 / 空值用 `0`,需要表达 presence 时用 `optional uint64`
6. 配置表 ID / 静态表 ID **默认用 `uint32`**(`npc_id` / `hero_id` / `skill_id` / `item_config_id` / `map_id` 等);如果字段名容易和运行时实体混淆,新协议优先命名为 `<entity>_config_id`
7. 状态 / 类型 / 原因等 proto 枚举常量**不属于 ID 规则**;proto enum 底层是 `int32`,Go 代码优先使用生成的 enum 类型,必要时才用 `int32`,不因取值非负改成 `uint32`
8. 新增业务数据结构**优先定义 proto message**,按下面四类各司其职,**不准手写与 proto 重复的并行 struct**:

   | 类别 | 命名 | 用途 |
   |---|---|---|
   | RPC 请求/响应 | `<Verb><Domain>Request` / `<Verb><Domain>Response` | gRPC unary/stream 出入参 |
   | 客户端可见结构 | `<Domain>` / `<Domain><Part>`(短名,如 `Team` / `TeamMember`) | RPC response、push payload 里给客户端看的字段 |
   | 服务端存储快照 | `<Domain>StorageRecord` + 子结构 `<Domain><Part>StorageRecord` | Redis value、Kafka 快照、MySQL **blob 列**里序列化成 bytes 的整块状态 |
   | 服务间事件 | `<Domain><Action>Event` | Kafka payload;可内嵌"客户端可见结构",但它本身是服务内部消息,不是存储快照 |

9. 第 8 条的"存储快照用 proto bytes"**只针对快照/blob 场景**(Redis value、Kafka payload、MySQL blob 列):
   - **关系型 MySQL 表(结构化列)不强制 proto 化**;列直接映射 proto 字段即可,不为每张表再造一个 proto bytes blob
   - 临时小令牌(如 invite,2~3 个字段、短 TTL)允许继续用 redis hash,不必升级成 proto bytes
   - 规则核心是"消灭与 proto 重复漂移的并行 struct",**不是"一切都序列化成 bytes"**
10. proto message 直接当存储 record 时:**禁止值拷贝 proto message**(`a := *rec` 会复制内部 state/mu/sizeCache),克隆一律用 `proto.Clone`;存储字段命名以 `<Domain>StorageRecord` 为准,客户端结构与存储结构**分开两个 message**,存储侧独有字段(如 `updated_at_ms`)不外泄给客户端
11. **禁止把服务端存储快照原样返回 / 推送给客户端**。RPC response / push payload 只能使用"客户端可见结构",由服务端从 `StorageRecord` / MySQL 行 / Redis 状态中按客户端当前需求的**最小数据单位**填充,必要时重新计算派生字段(如 ready 状态、queue_seconds、mmr_delta、展示用昵称),而不是把整块存储 record 暴露出去。例外只能是明确写入设计文档的运维 / 内部调试 RPC,且必须做鉴权、脱敏、不经 Envoy 对客户端开放。

## 6. 服务命名 / 端口规范

详见 [`docs/design/infra.md`](./docs/design/infra.md)。**不允许 ad-hoc 起端口或 key**。

## 7. 决策记录入口

`CLAUDE.md` 只保留稳定规范和索引,不再维护长决策表,避免每次会话重复消耗 token。

- 架构级决策:见 [`docs/design/pandora-arch.md`](./docs/design/pandora-arch.md) §11。
- 服务级决策:写入对应 [`docs/design/<service>.md`](./docs/design/) 或服务 README。
- 压测结论:写入 `docs/design/stress-<round>-*.md`。
- 周期进度与流水账:追加到 [`PROGRESS.md`](./PROGRESS.md),只追加不删旧条目。

## 8. 压测纪律

详见 [`docs/design/stress-discipline.md`](./docs/design/stress-discipline.md)。**核心规则**:

- 跑测前必有 `prev-summary.txt`,否则不许开下一轮
- **跑测前清空** redis / mysql / etcd / kafka offset / k8s GameServer
- 至少 3 次 prom snapshot:ramp 完成 / 稳态中段 / 稳态末
- summarize 脚本输出五段二维表,**不许手 grep raw prom**
- **没有对比表不许声明"性能提升"**
- 压期间不上传日志
- **每次登录压测把所有 redis/mysql/etcd 数据全部删除再开新一轮**

## 9. 不变量(数据一致性 / 安全)

跨服务必须保持的不变量。任何改动违反这些 → PR review 直接拒。

1. **玩家在线只能在一个 DS**(player_locator 强制)
2. **战斗结果幂等**(同一 match_id 只落库一次)
3. **DS 票据短时效**(JWT exp 5min)
4. **DS 崩溃必有补偿**(15s 心跳超时 → abandoned → 段位回滚)
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

## 10. AI 协作约定

AI 协作规则以 [`AGENTS.md`](./AGENTS.md) 为准,本文件不重复维护细则,避免双文档漂移。

## 11. UE 工程约束(写给 UE 仓库的开发者参考)

1. 类前缀统一 `Pandora*`(GameMode / Character / PlayerController)
2. 服务端逻辑统一在 `PandoraHubServer` / `PandoraBattleServer` 模块,不在 `Source/Pandora/` 客户端模块
3. 蓝图只做"胶水"(挂技能动画 / UMG 绑定),逻辑在 C++
4. 资源走 Git LFS(`.uasset / .umap / .fbx / .png / .wav / .ogg`)
5. **永远不要在 git 里提交** `Binaries/ Intermediate/ DerivedDataCache/ Saved/`

## 12. 不要做的事

- ❌ 不要在 docs/design/ 之外随便建 README(集中维护)
- ❌ 不要 import 第三方 GUI 库到 go 服务(go 服务都是 headless gRPC)
- ❌ 不要把 player_id 当 prometheus label(高基数会爆)
- ❌ 不要在 W1 写业务逻辑,只搭骨架
- ❌ 不要混用 `Pandora` / `pandora` / `MOBA` / `moba` 命名 — 见 §2 大小写规则
- ❌ **UE 侧不要再用 `Xuanming` / `Xm` 命名任何工程 / 模块 / 类 / 文件 — 一律 `Pandora`**(见 §11.1)

## 13. 命名大小写规则(强制)

- **Pandora**(首字母大写):仓库名 / 本地路径 / 工程类前缀 / 文档项目名引用 / **UE 工程 / 模块 / 类前缀**
- **pandora**(全小写):kafka topic / mysql / redis key / docker 镜像 / go module
- **`Pandora-Client`**(CapitalCase,带连字符):UE 客户端仓库名。⚠️ **不要和 JWT audience `pandora-client`(全小写)混淆** —— 后者是 envoy / login / auth 配置里的鉴权受众,改仓库名时**绝不能**动它
- **`Xuanming` / `Xm`**:**已废弃命名**,**代码 / 工程 / 类 / 模块一律不再使用**
