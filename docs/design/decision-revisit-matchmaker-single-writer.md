# 决策复审:matchmaker 撮合循环单写者 + 分模式池隔离(根治多副本重复成局)

> 决策级别:服务级(matchmaker)+ 跨服务约定(新增 `pkg/leader/etcdleader` 通用选举原语)。
> 触发:code review 发现 `RunMatchLoop` 在多副本部署下无单写者保护,会重复成局。
> 日期:2026-07-06。状态:**已拍板,落地中**。本文档定方案,实现见 §6。

## 1. 问题(为什么现状在多副本下是错的)

matchmaker 设计为 **headless 无状态、可水平扩展**(不变量 §16)。但当前 [`cmd/matchmaker/main.go`](../../services/matchmaking/matchmaker/cmd/matchmaker/main.go) **每个副本都无条件** `go uc.RunMatchLoop()`:

- `RunMatchLoop` → `matchOnce` 读**全局** `pandora:match:queue` ZSET,多个副本同时扫同一批票据、各自贪心凑局。
- 致命点:`RedisMatchRepo.ReserveTicket` 是**无条件 `SET` + `ZREM`**(非 CAS)。Cluster 兼容拆事务后,`ticketKey` 与 `queueKey` 分属不同 slot,无法 Lua 原子 CAS。两个副本同时选中同一张票 `T` → 两边 `ReserveTicket(T)` 都成功 → **同一玩家进两场 match** → 确认期 / 拉 DS / locator BATTLE 上报全部双份 → 直接违反不变量 §1(玩家同一时刻只在一个 Location)。
- `matchOnce` 内 `used` map 是**进程内**的,副本之间不生效。

第二个隐患:`queueKey` / `activeKey` 是**全局常量、无模式维度**。按 `scale-cellular-20m.md`,**每个 Cell = 一整套基础设施(Redis Cluster + MySQL ShardSet + Kafka + k8s),Cell 内所有服务共享**。同一 Cell 内若部署多个 `game_mode` 的 matchmaker(5v5_ranked / 3v3_casual …),它们共享同一套 Redis,会读到**同一个** `queue` ZSET,把不同模式的票混进同一场 match。这与锁无关,是**池本身没有模式维度**。今天未暴露仅因实际只部署了一个模式。

## 2. 为什么"给 ReserveTicket 加 CAS 让多循环并行"不可取

- Cluster 下 `ticketKey`(`ticket:%d`)与全局 `queueKey` 不同 slot,做不到单条 Lua 原子 CAS,须先把两 key 拉到同 slot,牵动全数据层 key schema。
- 即便解决,一次成局要**跨 10 人的多张票**做预留;多个 proposer 并行时部分预留失败的补偿回滚会互相打架(livelock / 高 churn)。
- 贪心批量撮合本质是**在一个按 MMR 排序的共享池上做全局优化**,天然是"单写者最优"问题。业界(LoL / Dota / Open Match pool 模型)对单个池都是单写者;要并行只靠**把池切碎**(分片),不是在同一个池上堆 loop。

## 3. 采用方案:etcd 选举单写者 + 分模式池隔离

### 3.1 单写者(每分片一个 leader 跑 loop)

- **分片键 = `game_mode`(进程级配置)× `region`(`cellroute.SelfRegion`)**。同一 `(mode, region)` 部署的 N 个副本竞选,仅当选者跑 `RunMatchLoop`,其余副本继续服务 RPC + 热备。
- **机制 = etcd `concurrency.Election`**(session + lease + fencing),封装成通用 `pkg/leader/etcdleader`。选择 etcd 而非 Redis 锁的理由**与 `infra.md §8` 拒绝 Redis 拼租约一致**:Redis 看门狗不能证明旧 holder 已停手(GC 停顿 / 网络分区 / 进程卡死),而 `infra.md §Leader Election` 已为 `ds_allocator` / `hub_allocator` 指定 etcd 选举作为**单例协调的既定机制**。matchmaker 沿用同一机制,架构一致。
- **失主语义与 snowflake nodeID 不同**:snowflake 失租必须 `os.Exit`(防双活发号);matchmaker 失去领导权**只取消 loop 的 ctx、进程不退出**——本副本继续服务 RPC,新 leader 在 lease TTL 内接管。这正是**不停机滚动更新**(不变量 §16):任意副本可随时被杀被替换,leader 自动转移。
- `RunMatchLoop` **一行不改**:main 把它交给 `etcdleader.Run(ctx, cfg, uc.RunMatchLoop)`,当选时 ctx 活、失主时 ctx 取消 loop 自然退出、重新当选再起。

### 3.2 分模式池隔离(防同 Cell 多模式串池)

- `queueKey` / `activeKey`(loop 扫描的两个索引 ZSET)按 `game_mode` 命名空间化:
  - `pandora:match:<game_mode>:queue`
  - `pandora:match:<game_mode>:active`
- `ticketKey` / `matchKey`(记录本体)**保持全局**:它们由**全局唯一 snowflake ID** 定址,不会跨模式碰撞;只有 loop 扫描的**索引**需要按模式分桶,才不会让 A 模式的 loop 捞到 B 模式的票 / 误超时 B 模式的 match。
- `playerKey`(player→ticket 归属 claim)**保持全局(不带模式)**:这样玩家在 A 模式排队就**占掉**全局 claim,B 模式再入队会被 `ClaimPlayer` SETNX 撞上 → 落实"一个玩家同一时刻只在一个队列(跨所有模式)"(不变量 §1)。跨模式一人一队列的兜底权威仍是 player_locator。

### 3.3 两级撮合分片(已实现算法,本决策只补运行时保护)

`matchOnce` 里的 `router` / `partitionTicketsByRegion` / `selectOverflowTickets` / `withinCrossRegionCap`(见 `scale-cellular-20m.md §4.4`、`decision-revisit-global-matchmaker.md`)已实现"region 内优先 + 跨 region 溢出"两级撮合算法。本决策补的是它缺的**运行时单写者保护**与**池按模式隔离**,不改撮合算法本身。

## 4. 兼容 / 默认行为(不破坏一键启动)

- **feature flag 默认关闭**(不变量 §16 / CLAUDE.md §14):`match.leader.enabled=false`(默认)→ 本副本直接 `RunMatchLoop`(单副本 / dev 行为**逐字节不变**)。多副本部署置 `true` → 经选举单写者。开关打开后的分支是**完整真实实现**,非空壳。
- 池按 `game_mode` 命名空间化**始终生效**(默认 `game_mode=5v5_ranked`);记录本体与 player claim 仍全局,现有单测(构造 repo 传空 namespace)行为不变。

## 5. 风险与缓解

| 风险 | 级别 | 缓解 |
|---|---|---|
| 失主到接管的空窗(lease TTL) | 低 | TTL 15s;空窗内只是本轮不撮合,票据留队列下一轮补,无正确性损失 |
| 失主瞬间旧 leader 在途 tick 仍提交一次 ReserveTicket | 极低 | `RunMatchLoop` 每 tick 检查 ctx.Done();`matchOnce` 加载票据时 `t.MatchId != 0` 跳过已预留者;一次 tick 极短,实际不产生双活撮合 |
| 新 etcd module 需 tidy 才能 build | — | 按 AGENTS §11.1:Claude 写代码 + go.mod require/replace;Codex 跑 `go mod tidy` + go.work。交接见 §7 |
| etcd 不可用 | 低 | `leader.enabled=false` 退回单副本直跑;enabled 且 etcd 挂 → 本副本不跑 loop(不误撮合),etcd 恢复自动竞选 |

## 6. 落地清单

1. `pkg/leader/etcdleader`(新独立 module,隔离 etcd client 重依赖,镜像 `pkg/snowflake/etcdnode` 的 go.mod / replace 写法):`Run(ctx, Config, run func(ctx))` 竞选 + 失主取消 + 重连重选。
2. `internal/data/match.go`:`NewRedisMatchRepo(rdb, namespace)`;`queueKey` / `activeKey` 改实例字段按 namespace 拼装;记录本体 / player claim 保持全局。
3. `internal/conf/conf.go`:`MatchConf.Leader LeaderConf`(enabled / etcd_endpoints / prefix / lease_ttl_sec)+ Defaults。
4. `cmd/matchmaker/main.go`:repo 传 `cfg.Match.GameMode` 作 namespace;撮合循环按 `leader.enabled` 走 `etcdleader.Run` 或直跑。
5. 单测:data 层 namespace key 断言;（选举逻辑集成测试留 Codex tidy 后补,或以 miniredis 无法覆盖 etcd 为由用 embed etcd,后续)。

## 7. 交接(Codex,AGENTS §11.1)

新 module `pkg/leader/etcdleader` 引入 `go.etcd.io/etcd/client/v3`(含 `concurrency` 子包),需 Codex:

1. 根 `go.work` 加 `use ./pkg/leader/etcdleader`(本次已由 Claude 写入,复核即可)。
2. `pkg/leader/etcdleader` 目录 `go mod tidy` 生成 go.sum。
3. `services/matchmaking/matchmaker` 目录 `go mod tidy`(已加 require + replace,拉 sum)。
4. tidy 后 `go build ./... && go test ./...` 复验。

**需 tidy 的模块**:`pkg/leader/etcdleader`、`services/matchmaking/matchmaker`。
