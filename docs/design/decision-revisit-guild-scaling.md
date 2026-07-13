# decision-revisit:公会存储扩容路线 —— 走 social TiDB,禁手动分库/禁新分片库

> 状态:**方案 B 上线前代码路径已落地**(2026-07-11 提出;2026-07-13 落地代码路径)。
> 拍板走方案 B(将来跟随 social 迁 TiDB),并**在上线前(无存量数据)先把 TiDB 兼容的代码路径做完**:
> ①全部 social 表(含群表)的 TiDB DDL;②所有"计数 + 上限"校验改成 TiDB 安全写法(§3.5);
> ③opt-in 的 `guild-dev-tidb.yaml` 配置;④MySQL / TiDB 双后端并发上限集成测试。
> **仍未落地(留待将来真触发扩容时)**:§5 的在线 CDC/DM 数据搬迁 + epoch 切读切写 + 回滚闭环
> (面向**存量线上数据**的零停机迁移;当前上线前无存量,不需要),以及 `guild_rivalry` 跨公会关系表
> (等策划真引入"仇敌/联盟"玩法时再建)。运行默认仍是单 MySQL(一键启动不变),TiDB 为 opt-in。

## 1. 旧问题:公会是全服的,单 MySQL 将来扛不住?会不会被顺手分错库?

- 公会属 `pandora_social` 域,**全服共享**;权威数据现在仍在**单 MySQL 那条容器线**上
  (`guilds` / `guild_members` / `guild_join_requests`,`deploy/mysql-init/11-guild-tables.sql`),
  而同域的 friend / 黑名单 / 私聊**已切 TiDB**(`deploy/tidb-init/01-social-tidb.sql`),公会没跟过去。
- 现有 [guild_repo.go](../../services/social/guild/internal/data/guild_repo.go) 头注释的成立前提是一句关键假设:
  **"公会成员是 owner(guild_id)单键操作,无跨人事务,不撞 friend 跨人强一致难题"**。
- **触发点**:策划提出未来公会间会有**少量跨公会关系**(如"互为仇敌 / 联盟")。
  一旦引入 `guild_rivalry(guild_a, guild_b, ...)` 这种跨 `guild_id` 关系,
  上面那句"单键、无跨实体事务"的假设**被打破** —— 公会被拖进 friend 那类跨实体强一致场景。

## 2. 候选方案对比

| 方案 | 跨实体原子事务 | 业务改动 | 运维成本 | 结论 |
|---|---|---|---|---|
| **A. 维持单 MySQL** | ✅ 同库单事务 | 无 | 无(现状) | ✅ **现阶段就用它** |
| **B. 跟随 social 迁 TiDB** | ✅ TiDB 悲观事务原生跨节点 | **非零**:硬上限校验逻辑须改 TiDB 安全写法(见 §3.5),连接/DSN/DDL 需同步 | 复用已落地的 social TiDB,无新组件 | ✅ **将来唯一升级路线** |
| **C. `mysqlx.ShardSet` 手动按 guild_id 分库** | ❌ 跨 shard 无法原子(A、B 可能落不同库) | 大 | 中 | ❌ **明确禁止**(见 §4) |
| **D. 引第三方分片库(Vitess/Citus/CockroachDB/ShardingSphere)** | 各异 | 巨大(方言/驱动/DDL 全改) | 三套存储栈 | ❌ **明确禁止**(过度工程) |

> ⚠️ **勘误(2026-07-12 审核)**:初版把方案 B 写成"只迁 4 张公会表 + Go 零改动",**两处都错**,已在下方 §3.2 / §3.5 修正:
> ① guild 进程里 `GuildRepo` 与 `GroupRepo` **共用同一个 `*sql.DB`**([main.go](../../services/social/guild/cmd/guild/main.go)),
>    迁 DB 必须**连 `chat_groups` / `chat_group_members` 一起迁**,否则切 DSN 后 GroupService 直接连不到表;
> ② TiDB **无 MySQL 的间隙锁(gap lock)**,现有 `COUNT(*) ... FOR UPDATE` 上限校验在 TiDB 下**拦不住并发幻读插入**,
>    必须改成锁父聚合行 / 原子计数列,**不是零 Go 改动**。

## 3. 决策

1. **现在什么都不做**:公会规模远小于好友图(公会数约玩家数 1/100~1/50,几万~几十万行;写低频)。
   单 MySQL 单机能顶到很后期。跨公会仇敌关系"少量",放**同库同事务**天然原子,无需分片。
2. **将来升级 = 整个 guild 进程的 social 表集体迁进同一个 TiDB**(不是新库,也不是只迁 4 张):
   guild 进程同时承载 GuildService + GroupService,两者**共用一个 `*sql.DB`**。迁移单元必须是
   **全部同库表**:`guilds` / `guild_members` / `guild_join_requests` / `chat_groups` / `chat_group_members`
   + 新增 `guild_rivalry`。只迁公会 4 表就切 DSN 会让 GroupService 连不到群表(#10)。
3. **跨公会关系表归一化成单行**:`guild_rivalry` 用 `(least_id, greatest_id)` 存储
   (`least_id = min(gA,gB)`,`greatest_id = max(gA,gB)`),`PK(least_id, greatest_id)`,
   避免 A↔B / B↔A 双向重复;查询按任一 guild_id 走两个二级索引 union。
4. **列表上限对齐不变量 §9.18**:`guild_rivalry` 是客户端可触发新增、可堆积、会被拉取的累积列表,
   必须落 `max_rivalries_per_guild`(建议默认几十),写入侧事务原子校验超限回业务错误(校验须用 §3.5 的
   TiDB 安全写法),读取侧 LIMIT/cursor 兜底。
5. **硬上限校验必须改 TiDB 安全写法(#11,方案 B 的核心改动)**:现有 `checkGuildPendingLimit`
   ([guild_repo.go](../../services/social/guild/internal/data/guild_repo.go))用 `SELECT COUNT(*) ... WHERE guild_id=? AND status=pending FOR UPDATE`
   拦并发,**依赖 MySQL 的间隙锁锁住"尚不存在但即将插入"的区间**。TiDB 悲观事务**不提供间隙锁**
   (只锁命中的已存在行),多个不同玩家可同时读到"未满"再各自插入 → **突破上限**
   ([TiDB SELECT 语义](https://docs.pingcap.com/tidb/stable/sql-statement-select/))。迁 TiDB 时所有
   "计数 + 上限"路径(公会 pending 申请数、成员数、群成员数、我所在的群数、rivalry 数)必须改成二选一:
   - **(推荐)锁稳定的父聚合行**:`SELECT ... FROM guilds WHERE guild_id=? FOR UPDATE` 先锁公会行,
     把该公会的 pending/member 计数收敛到"锁单行 → 读计数列 → 校验 → 写"串行化(成员数已有
     `guilds.member_count`,pending 数需加 `guilds.pending_request_count` 计数列并在增删申请时同步维护);
   - 或**唯一约束兜底 + 应用层计数列**:计数列 `+1` 与明细行插入同事务,读计数列判满。
   这条同样适用于 GroupService 的 `max_group_members` / `max_groups_per_player`。

   > **✅ 已落地(2026-07-13)**:`checkGuildPendingLimit` 已删除,pending 路径固定锁 `guilds` 父行，
   > 再以 pending 明细 `COUNT...FOR UPDATE` 为权威绝对值校正 `pending_request_count` 后判限/写入；
   > `checkPlayerGroupLimit` 已替换为兼容式 `reservePlayerGroupSlot` / `releasePlayerGroupSlot`，锁顺序固定为
   > `player_group_counts` 单行 → `chat_group_members(player_id)` 明细范围，按实际明细绝对值校正
   > `group_count`，新增写 `actual+1`，删除后重算 actual，不再对可能脏的 counter 盲目 `±1`。
   > 这既保留 TiDB 新/新写的稳定计数行串行化点，也在 MySQL 旧/新混跑时复用旧版明细索引锁；
   > 多玩家操作继续按 player_id 升序逐个取得「计数行→明细范围」，避免 ABBA。成员数上限本就锁 `guilds` / `chat_groups` 父行读
   > `member_count`,TiDB 安全,未改。见 [guild_repo.go](../../services/social/guild/internal/data/guild_repo.go) /
   > [group_repo.go](../../services/social/guild/internal/data/group_repo.go);双后端并发上限测试见 §6.1。

## 4. 明确禁止(写给下一个 AI)

- ❌ **不要用 `mysqlx.ShardSet` 按 `guild_id` 手动分库**。它只支持"每 key 单写者、永不跨 key"
  (拍卖行 per-market 那种);公会一旦有跨公会关系,分库后 A、B 落不同物理库,建立/解除敌对的
  原子写**做不到**,且无法回头。这是本决策**唯一会把项目锁死**的错误选项。
- ❌ **不要为公会引入新的分片数据库**(Vitess/Citus/CockroachDB/ShardingSphere 等)。
  项目栈已是 MySQL + TiDB 两套,TiDB 是团队拍板 + 落地验证过的分布式 ACID 库,再加第三套 = 栈碎片化、
  违反"不过度工程"。同类已有选定赢家,不再重新选型。

## 5. 迁移方案(方案 B 真触发时;必须零停机 —— §9 不变量 16)

> **纠错(#12)**:初版"停写窗口短或双写过渡"与不变量 16(零停机滚动更新)冲突,已废弃。迁移必须全程可写。

1. **DDL(全表,含群表)**:把 `guilds` / `guild_members` / `guild_join_requests` /
   `chat_groups` / `chat_group_members` / `guild_rivalry` 的 CREATE 放进 social 的 TiDB 初始化脚本。
   - 雪花主键(`guild_id` / `group_id` / `request_id`)须用**主键 NONCLUSTERED + `SHARD_ROW_ID_BITS` + `PRE_SPLIT_REGIONS`**
     打散时间序写热点(业务 ID 是 uint64 显式雪花,不能用 `AUTO_RANDOM`,与 `01-social-tidb.sql` 里
     `friend_requests` 同款处理);纯代理主键表可用 `AUTO_RANDOM`。
   - **`guilds.name` 唯一键的 collation 语义必须与现网 MySQL 一致**:现 MySQL 库默认 collation
     决定公会名"是否大小写/口音敏感"。friend TiDB 用 `utf8mb4_bin`(因其键全是数值,无所谓),
     **但公会名是字符串 uk**,若照搬 `utf8mb4_bin` 会把"Guild"与"guild"从冲突变成不冲突,
     改变重名判定。迁移前先确认现网 `guilds` 的 name collation,TiDB 侧显式声明**同款 collation**,
     并加一条迁移测试断言大小写重名判定不变。
2. **在线数据搬迁(不停写)**:用 TiDB DM / CDC 做"全量 + 增量"同步(单 MySQL → TiDB),
   业务持续读写旧 MySQL;增量追平后进入切读窗口。量小也不得停写(Codex / 人执行工具链)。
3. **切读 + 切写 + 回滚(必须闭环,不能破坏 read-after-write / 不能丢写)**:分四步,每步都有明确
   数据流向与栅栏(#13 纠错:初版"读切 TiDB、写仍旧库、一键回滚"会破坏 RAW 且回滚丢写,废弃)。

   - **步骤 A — 影子读校验(shadow-read,不改用户可见来源)**:写仍**只写旧 MySQL**,正向同步
     `MySQL → TiDB` 持续追平。读路径**双读**:以旧 MySQL 结果为**唯一对外来源**,同时异步读 TiDB
     比对并打点差异率(不影响响应,不破坏 RAW)。差异率稳定为 0 且延迟达标才进步骤 B。
   - **步骤 B — 双写过渡(dual-write,消除 RAW 断点)**:写路径改为**同事务双写旧 MySQL(权威)+
     TiDB**;读仍走旧 MySQL。**绝不允许"读切 TiDB 但写仍只写旧 MySQL"**——那会在 TiDB 落后正向
     同步的窗口内读到陈旧数据,破坏 read-after-write。双写以旧 MySQL 提交成功为准,TiDB 侧失败仅
     告警 + 由对账补偿(见步骤 D),不阻塞用户写。
   - **步骤 C — 切写栅栏(cutover fence,原子翻转权威库)**:引入单调递增的 `guild_authoritative_epoch`
     控制键(etcd,复用 `etcdtable`/`etcddecl` 发布通道,不存表体)。所有 `guild` 副本启动即 watch;
     翻转前**先短暂 single-writer 栅栏**(拒绝一小段写窗口,毫秒级,不算停服)确保无跨库分裂写,再把
     权威库从 MySQL 切到 TiDB(epoch++)。切写后**权威 = TiDB**,并**立即启动反向同步 `TiDB → MySQL`**
     (CDC)保持旧库热备追平。读随之切 TiDB(此时 TiDB 已是权威,RAW 天然成立)。
   - **步骤 D — 回滚闭环(rollback,不丢切换后写)**:回滚**只有在反向同步 `TiDB → MySQL` 已追平时**
     才允许(否则回旧库会丢步骤 C 之后写入 TiDB 的数据 —— 初版"一键回旧 MySQL"的根本缺陷)。
     回滚同样走 epoch 栅栏原子翻回,翻回前校验反向同步 lag=0,否则**拒绝回滚并告警**,由运维等追平。
     切换/回滚两侧的同步都用**幂等 upsert(业务雪花 ID 为键)+ `updated_at` 版本裁决(last-writer)**,
     并有对账 job 双向 diff 告警,保证补偿语义可收敛。

   全程无"必须停服"步骤(仅毫秒级 single-writer 栅栏),对齐不变量 16。
4. **Go 业务:非零改动**:硬上限校验按 §3.5 改 TiDB 安全写法后才允许切库(见 §6 验收)。
   连接层面 `guild` 服务 DB 配置从"单 MySQL 线"切到"social TiDB 线"。双写/切写/反向同步的权威库
   选择统一由 `guild_authoritative_epoch` 驱动,不散落在各 handler。

## 6. 验收标准(将来落地方案 B 时)

### 6.1 上线前代码路径(2026-07-13 已落地)

- [x] TiDB DDL 覆盖 guild 进程**全部现存 social 表**:`guilds` / `guild_members` / `guild_join_requests` /
      `chat_groups` / `chat_group_members` + 新增计数表 `player_group_counts`,切 DSN 后 GroupService 正常(#10)
      —— [deploy/tidb-init/01-social-tidb.sql](../../deploy/tidb-init/01-social-tidb.sql)
- [x] `guilds.name` / `chat_groups.name` 列级 collation 显式 `utf8mb4_0900_ai_ci`,与现网 MySQL 一致;
      重名判定(大小写/口音)迁移前后不变,有双后端断言测试 `TestGuildNameCaseInsensitiveDedup`
      (锁死 TiDB 默认 `utf8mb4_bin` 会让重名从冲突变不冲突的漂移,§5.1)
- [x] 所有"计数 + 上限"路径改用 §3.5 的兼容自愈写法:稳定父行 / 计数行负责 TiDB 新/新串行，
      MySQL 滚动窗口继续执行旧版同款明细 locking COUNT；每次触碰都以明细为权威绝对值校正 counter。
      旧 Pod 只改明细也不会使兼容新版信任陈旧计数；成员数上限本就锁父行读 `member_count`,TiDB 安全
- [x] **data 包新增 MySQL 与 TiDB 双后端并发集成测试**,多协程并发申请/入群在 TiDB 下**不突破**各上限
      (`TestConcurrentPendingRequestLimitEnforced` / `TestConcurrentPlayerGroupLimitEnforced` /
      `TestGroupCountReleasedOnLeave`,`forEachBackend` 按 `PANDORA_TEST_MYSQL_DSN` / `PANDORA_TEST_TIDB_DSN` 双跑)(#11)
- [x] opt-in `guild-dev-tidb.yaml`(DSN 指向 :4000,collation utf8mb4_bin);运行默认仍单 MySQL,
      一键启动管线(docker-compose / k8s / gen_cluster_config)**不改**,TiDB 为显式启用项
- [x] 版本化旧库升级补 `pandora_social/000002_guild_counter_tables`:additive 增加
      `guilds.pending_request_count` / `player_group_counts`,并分别从 pending
      `guild_join_requests` / `chat_group_members` 确定性 backfill；fresh schema 已含结构时同样幂等。
      guild 启动在装配业务前校验必需列的 type/unsigned/NULL/default 与 `player_group_counts`
      精确单列主键，旧/错 schema 会 fail-closed 提示先迁至 version 2
- [x] **当前上线前、无存量的零停机升级顺序仍不可颠倒**:先在旧 MySQL 执行 v2 expand/backfill；
      再把旧 guild Pod 滚成上述兼容自愈版（旧/新可在 MySQL 混跑）；确认所有旧 Pod 排空并核对
      counter=明细后，才允许整体切到 TiDB。**旧版 guild 绝不能运行在 TiDB**，因为它只靠 MySQL
      gap lock；迁移期间也不能把“一次 backfill”误当成后续旧写会自动维护 counter
- [x] 旧写 raw SQL→兼容新版反例覆盖高/低错误 counter、自愈、满额 N+1 拒绝和删后重算：
      `TestLegacyGuildWriterCounterDriftSelfHeals` / `TestLegacyGroupWriterCounterDriftSelfHeals`
- [x] 无任何 `ShardSet` / 第三方分片库引入

### 6.2 真触发扩容时(面向存量线上数据,仍待将来)

- [ ] `guild_rivalry` DDL 进 TiDB,归一化单行 + `max_rivalries_per_guild` 上限 + 读取分页
      (等策划真引入"仇敌/联盟"玩法再建)
- [ ] 建立/解除跨公会关系在**单 TiDB 事务**内原子完成,并发下无重复行、无孤儿行
- [ ] 迁移全程零停机(在线 CDC/DM + 切读/切写 + 一键回滚),无"停写窗口",对齐不变量 16
- [ ] **切换闭环无 RAW 破坏 / 无丢写**(#13):影子读校验期写只写旧 MySQL、读以旧库为唯一来源;
      双写过渡期同事务双写(旧库权威);切写经 `guild_authoritative_epoch` 栅栏原子翻转 + 启动反向
      同步;回滚前强制校验反向同步 lag=0,未追平拒绝回滚。有正向/反向同步 + 双向对账 job 测试,
      断言切写后回滚不丢 TiDB 侧新写、切读后无读到陈旧数据
- [x] 本文档从"设计待拍板"改为(上线前代码路径)"已落地",并在 `pandora-arch.md §11` 补一行决策索引

## 7. 关联

- `docs/design/friend-distributed-scaling.md`(social 选 TiDB 的原始论证)
- `docs/design/decision-revisit-chat-group.md` §3.2 / 末尾"owner 维度按 guild_id,全服千万级再谈分片"
- `docs/design/scale-cellular-20m.md`(极限体量 social 按 region 分 TiDB 集群)
- `CLAUDE.md §9` 不变量 18(累积列表上限)、`AGENTS.md §7`(跨 AI 冲突:改设计先写 decision-revisit)
