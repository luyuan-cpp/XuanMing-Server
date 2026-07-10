# decision-revisit:data_service PlayerData 是否从 blob 升级为强类型列

> 状态：**已拍板并实施（代码/开发环境层面）**（2026-07-09 Claude 起草；同日人拍板：选 **B 变体——pb 驱动强 schema**）。
> 上线状态：**未正式上线**。截至 2026-07-09 仅用于本地开发/minikube 验证，未部署到正式环境，
> 没有需要保留的旧协议、Redis 缓存或 `player_data` 表有效数据；“已实施”不等于“已正式上线”。
> 人的决定：PlayerData 改成真实类型字段；**pb 是 schema 唯一来源**，服务启动时经
> proto2mysql `RegisterAllTables` 注册 + `SyncAllTables` 按 pb 建表/同步表结构；Redis 缓存保持 pb 二进制。
> 实施记录见 §9。以下 §1-§8 为拍板前的分析存档。
> 触发:接入 proto2mysql 后,提出「`PlayerData.data` 不该是 `bytes`,应是玩家真实字段的正确类型,让 MySQL 能看到列」。

## 1. 旧问题 / 现状

现在玩家数据有**两张表、两种设计**,是有意互补的:

| 表 | 归属 | 形态 | 说明 |
|---|---|---|---|
| `players` | player 服务(04-player-tables.sql) | **结构化列**:nickname/level/mmr/avatar/created_at/last_seen_at/total_battles/total_wins/active_hero_id/unspent_attr_points/total_talent_points + `uk_nickname` + `idx_mmr` | 权威玩家档案;`PlayerProfile` 经 profile_repo.go 手写 SQL 映射 |
| `player_data` | data_service(07-data-tables.sql) | **不透明 version blob**:player_id PK + version 乐观锁 + `data BLOB` | 中性读写网关;proto 注释明写「中性容器,不依赖 player.v1,避免循环依赖」;SQL 注释明写「与 players 表互补,players 是结构化列,player_data 是整块序列化快照」 |

设计文档 `go-services.md §2.3`:data_service 的存在理由是「玩家数据在多个服务读写(player/trade/battle_result),抽一层避免缓存不一致」——即 **cache-aside 一致性网关**,不是第二份玩家档案表。

近期把 proto2mysql v0.0.22 接入了 data_service 的 store.go。但 proto2mysql 的定位是「proto message ↔ 结构化列」,喂给它不透明 `bytes` 反而水土不服(bytes 列被序列化成 base64 文本存储,与旧 raw BLOB 不兼容)。这是本次提出改动的直接诱因。

**data_service 当前未正式上线、无任何外部调用方**（只有自身 biz/service/store + 生成桩）；
仅存在本地开发/minikube 验证环境，未产生需保留的有效历史数据，因此 RPC 与开发数据重置影响面小。

## 2. 核心矛盾

「让 MySQL 看到真实玩家字段列」这个诉求本身合理,但**放到 `PlayerData` 上会撞两堵墙**:

1. **与 `players` 表字段重复**:玩家档案字段已经是 `players` 的结构化列。data_service 再放一份同样字段 = 两张玩家档案表,权威性归属不清(违反单一事实源原则)。
2. **重新引入被设计规避的循环依赖**:data_service 要么 `import player.v1`(设计明确避免),要么手抄 PlayerProfile 字段(必然与 player 服务漂移)。

同时要注意:**proto2mysql 天生适合结构化表,不适合不透明 blob 网关**——本质是「blob 网关」这个用途和 proto2mysql 不匹配,而不是 `bytes` 字段本身有错。

## 3. 三个方向(各自的硬前置条件)

### 方向 A:proto2mysql 用到 player 服务的 `players` 表(推荐方向,但有前置 gap)
- 思路:`players` 本就是结构化列,是 proto2mysql 的甜区;用它重写 profile_repo.go 的 CRUD。data_service 的 blob 网关**回退到手写 SQL 或维持现状**,不碰。
- **硬前置 gap(必须先解决)**:`PlayerProfile` 字段名与 `players` 列名对不上——
  - `created_at_ms`(int64 毫秒)↔ 列 `created_at`(DATETIME)
  - `last_seen_ms`(int64 毫秒)↔ 列 `last_seen_at`(DATETIME)
  - proto2mysql 目前只有 `WithTableName`,**没有 `WithColumnName` / 字段→列映射**,也无「int64 毫秒 ↔ DATETIME」转换。
  - 解法三选一:(a) 给 proto2mysql 加 `WithColumnName` + 时间转换(库改动);(b) 改 `players` 列名/类型对齐 proto(动 schema,且 `player.proto` 与 UE 客户端共享,改字段名是 breaking);(c) 只对能干净映射的字段用库、时间字段仍手写(半接,违反 §14 接线完整性,不推荐)。
  - 另注:`players` 有 PlayerProfile 里没有的列(active_hero_id 等),这些列有 DEFAULT,INSERT 省略它们没问题;UpdateNickname 的 uk 冲突判定/存在性判定逻辑较特殊,未必适合套库。
- 影响面:中(player 服务 data 层 + 可能的库改动);不动 data_service 架构;不触循环依赖。

### 方向 B:重设 data_service,`PlayerData` 改成真实字段列
- 思路:PlayerData 直接映射玩家字段,data_service 变成结构化玩家数据存储。
- **硬前置(必须先解决)**:
  1. 与 `players` 表重叠 → 必须先定「data_service 是否取代 players 表 / 两者边界」,否则双写不一致。
  2. 循环依赖 → 定 data_service 是 import player.v1 还是保留独立中性 schema(独立 schema 又回到手抄漂移问题)。
  3. 若 data_service 存 pb 到 Redis/blob,改字段涉及 §9 不变量 16/17 零停机兼容演进约束。
- 影响面:大(proto + 新表 + biz + cache + 可能牵动 players 表归属)。**属推翻既有「中性网关」设计,必须本文档拍板后再动。**

### 方向 C:data_service 维持 blob,但退出 proto2mysql
- 思路:承认 blob 网关 ≠ proto2mysql 适用场景,store.go 回退到手写 SQL(即接入前的版本),字段维持 `bytes`。
- 好处:立刻消除 base64 与 raw BLOB 不兼容的隐患;不动架构;完全可逆。
- 代价:没满足「MySQL 看到真实字段」的诉求(但 blob 网关本就不需要)。
- 影响面:小(仅 data_service store.go + go.mod 去依赖)。

## 4. 推荐

- **架构上推荐 A 的精神**:proto2mysql 属于结构化表(`players`),不属于 blob 网关。真正想「MySQL 看到玩家字段列」,`players` 表早已满足,且是权威源。
- **但 A 有列名/类型映射 gap**,建议先给 proto2mysql 补 `WithColumnName` + 时间字段转换(库改动,方向标准、可复用),再重写 profile_repo.go。
- **data_service 侧建议先走 C**(回退手写 SQL),消除 base64 隐患;是否长期保留 blob 网关另行评估。
- **不推荐 B**:在 players 表已存在的前提下,把 data_service 变成第二份玩家档案表,收益不明且引入双写一致性与循环依赖风险;若确要走 B,需先定清 data_service 与 players 的边界/归属。

## 5. 风险

- A 若走「改 players 列名对齐 proto」→ `player.proto` 是与 UE 共享协议,改字段名是 breaking,风险高,不建议。
- B 双写玩家档案 → 缓存/DB 不一致,踩 §9 数据一致性红线。
- C 回退 → 无新风险,反而修掉 base64 隐患。

## 6. 迁移成本

- A:proto2mysql 加 `WithColumnName`(小)+ profile_repo.go 重写(中)+ player 服务 go mod tidy(Codex)。
- B:proto + 新表 + biz + cache 重写(大),且需先解决表归属决策。
- C:store.go 回退 + go.mod 去 proto2mysql 依赖 + tidy(小)。dev 环境已写入的 base64 行需清 `player_data`(dev 可清)。

## 7. 验收标准

- 选定方向后:相关服务 `go build ./...` 绿;若涉及库改动,proto2mysql 新增能力有单测;若动 schema,mysql-init 与 proto/代码字段一致;不破坏 §9 不变量(单一事实源、零停机兼容);data_service 若保留,cache-aside 一致性语义不变。

## 8. 待人决策项（已决）

1. 选 A / B / C？→ **人拍板：B 变体（pb 驱动强 schema）**。
2. 循环依赖？→ 字段内联镜像 PlayerProfile，不 import player.v1（嵌套 message 在 proto2mysql 中会退化成 MEDIUMBLOB 列，也不可取）。
3. 与 players 表边界？→ 未合并；两表共存，players 仍归 player 服务，player_data 归 data_service 网关。后续若要合并另起 decision-revisit。

## 9. 实施记录（2026-07-09）

- `proto/pandora/data_service/v1/data_service.proto`：`bytes data = 3` 删除，编号 3-10 改为强类型字段（nickname/level/mmr/avatar/created_at_ms/last_seen_ms/total_battles/total_wins，镜像 PlayerProfile，开发期允许复用编号，已全量重生 proto）。
- `store.go`：启动时 `RegisterAllTables()` 自动扫描已链接描述符注册 + `SyncAllTables()` 建表/同步（DBName 经 `SELECT DATABASE()` 从连接取）；`Write(ctx, pd, updateFields)` 写入——`version=0` 走整条 INSERT（忽略 updateFields），`version>0` 走 CAS `UpdateFieldsIfVersion`：updateFields 必须非空，仅 SET 掩码列（空掩码 → `ERR_INVALID_ARG`，避免全量覆盖清零未知新列）；掩码合法列由 `playerDataUpdatableSet`（PlayerData 描述符动态推导，排除 player_id/version，新增 proto 字段自动纳入无需手改）校验。
- `WritePlayerRequest.update_mask`（`google.protobuf.FieldMask`）驱动部分更新：更新（version>0）**必须带非空掩码**，仅更新指定列，避免滚动升级期旧调用方空掩码全量覆盖把新增列清零（空掩码更新 → biz/store 双层校验回 `ERR_INVALID_ARG`）；新建（version==0）忽略掩码整条 INSERT；掩码含 `player_id`/`version`/未知列 → `ERR_INVALID_ARG`。
- `deploy/mysql-init/07-data-tables.sql`：DDL 移除，schema 归服务自管。
- Redis 缓存不变（cache.go 本就存 pb 二进制）。
- 乐观锁/错误码语义不变（version=0 新建；CAS 失配 → ErrDataVersionMismatch）。
- **dev 迁移**：因 data_service 未正式上线且没有需保留的有效历史数据，允许开发期破坏性重置。旧表含 `data BLOB NOT NULL` 孤儿列（自动同步不删列且无默认值会堵 INSERT），切换前需 `DROP TABLE pandora_player.player_data` 并清 Redis `pandora:data:player:*`，由服务重建；此例外不适用于任何已正式上线或存在有效数据的环境。
- 验证：data_service `go build` / `go vet` / 单测全绿（fake store 适配新签名）。
