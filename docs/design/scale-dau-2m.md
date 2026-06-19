# DAU 200 万扩容方案(单 Redis/单 MySQL 去单点、nodeID 自动分配、push 横扩、Agones 吞吐)

> 触发:产品确认全区全服(类 LoL 手游,玩家不选区);目标 **DAU 200 万**。
> 本文档回答"抗不抗得住"并给出四块改造的落地方案。
> 决策级别:架构级(索引见 `pandora-arch.md` §11)。

---

## 0. 先纠正一个概念:zone_id 不是"选区"

- 代码里 `node.zone_id`([pkg/config/config.go](../../pkg/config/config.go) `NodeConfig.ZoneId`)是 **snowflake 发号器的 node 段**(机器编号),不是玩家选区。
- 全区全服是 Pandora 既有架构(`pandora-arch.md` §1:持续在线大厅 + 全局 MMR 撮合,无选区流程),**目标天然满足,不需要为"不分区"改设计**。
- `zone_id` **不能删**:删了多副本就无法区分发号节点 → 发重号(违反 `CLAUDE.md` §9 不变量 11/§5.5)。要做的不是删,而是从"人工静态"升级为"etcd 自动分配"(见 §3)。

---

## 1. 容量基线:200 万 DAU 到底意味着多少并发

把"注册量 / DAU / CCU"三层分清,**决定抗压的是 CCU(峰值同时在线),不是注册量**:

| 指标 | 含义 | 估算 | 说明 |
|---|---|---|---|
| 注册总量 | DB 总行数 | ~1000 万 | 纯行数,MySQL 毫无压力 |
| DAU | 日活 | 200 万 | 产品给定 |
| **PCU / 峰值 CCU** | 峰值同时在线 | **~20 万~40 万** | 经验系数 DAU×(10%~20%);按 **30 万 CCU** 做容量目标 |
| 在大厅 | hub DS 容量 | ~20 万 | 500 人/实例 → **~400 个 hub DS 实例** |
| 在战斗 | battle DS 容量 | ~10 万 | 10 人/局 → **~1 万个并发 battle DS pod** |
| 登录峰值 QPS | login | ~数千/s | 早晚高峰 + 断线重连放大 |

**结论:1000 万注册轻松;30 万 CCU 可行,但必须先拆掉下面四个单点/天花板。**

---

## 2. 单 Redis / 单 MySQL 去单点(#1)

### 2.1 Redis → Redis Cluster

30 万 CCU 下 `player_locator` / `team` / `matchmaker` / `trade` / push 离线缓存全压一个 Redis,QPS 和连接数必爆。

**已落地能力(本仓库,非破坏式)**:
- [pkg/redisx/client.go](../../pkg/redisx/client.go) 新增 `NewUniversalClient(RedisConf)`:按 `addrs` / `master_name` 自动选型(单实例 / Sentinel / Cluster),返回 `redis.UniversalClient` 接口。
- [pkg/config/config.go](../../pkg/config/config.go) `RedisConf` 新增 `addrs` / `master_name` 字段。**留空 = 单实例**,完全向后兼容。
- **类型切换已完成(✅ 2026-06,非破坏)**:`BaseContext.RedisClient`([pkg/svc/base.go](../../pkg/svc/base.go))、`cache.LoadOrCache`([pkg/cache/cache.go](../../pkg/cache/cache.go))、全部 data 层 repo 与各服务 `cmd/*/main.go` 统一从 `*redis.Client` 改为 `redis.UniversalClient`,构造改走 `redisx.NewUniversalClient`。`*redis.Client` 实现 `UniversalClient`,故单实例 / Sentinel / Cluster 三态可切,旧测试(miniredis 传 `*redis.Client`)不破。`pkg/redislock` 依赖 `redis.Cmdable`,无需改。
  - 验证:`go build` 全服务 EXIT=0;`pkg/cache`、push/team data、player_locator 测试 EXIT=0。
  - **保留**:`redisx.NewClient`(返回 `*redis.Client`)仍在,供确需单实例并发类型的场景。

**切 Cluster 的待办(改造期)**:
1. ~~各 data 层把依赖类型从 `*redis.Client` 改为 `redis.UniversalClient`~~(✅ 已完成,见上)。
2. **hash tag 审计结果**:Cluster 下跨 slot 的多键事务 / Lua / `MULTI` / `WATCH` 受限,同一原子操作涉及的 key 必须用 `{}` 绑同一 slot。逐服务核对:
   - **单键安全,可直接上 Cluster**(✅ 无跨 slot 风险):
     - `player_locator` 单 key `pandora:locator:{player}`(HSET/HGETALL/WATCH 同键)。
     - `push` 离线缓存单 key `pandora:push:offline:{player}`。
     - `login` session / ticket-JTI 均单 key。
     - `team` teamKey / playerKey、`matchmaker` matchKey:各操作单键或同实体,核对后无跨实体事务。
     - `trade.UpdateWithLock`:WATCH/MULTI 仅围绕单 orderKey。
   - **跨 slot 阻塞项(✅ 2026-06 已改造 + 编译测试通过,可上 Cluster)**:碰 `CLAUDE.md` §9 原子性不变量,已逐服务出 decision-revisit 拍板,未静默重写。
     - **`trade.CreateOrder`**(✅ 见 [decision-revisit-trade-crossslot.md](./decision-revisit-trade-crossslot.md)):原 `TxPipeline` 跨 order/seller/buyer 三 slot。改为 **order 单键 `SET` 为权威** + 卖家/买家反查索引各自独立提交(`addPlayerOrderIndex` 用同键 mini-tx 做 SADD+EXPIRE);反查索引漂移由 `ListPlayerOrderIDs` 自愈跳脏成员。资源扣减不变量 #7 落在 biz 层 `ResourceLedger`,本改动不碰,故不破 #7。
     - **`hub_allocator`**(✅ 见 [decision-revisit-hub-crossslot.md](./decision-revisit-hub-crossslot.md)):4 个方法(CreateShard/UpdateShardWithLock/HeartbeatShard/RemoveShard)原把 pod 镜像 `shardKey{pod}` 与全局 `shards-set`/`active-zset` 捆在一事务。改为 **pod 镜像单 slot 事务为权威** + 全局索引拆成独立命令(故障转移/扫描加速器,漂移容忍,`ListShards` 自愈)。
3. 分片数(✅ 拍板):Redis Cluster 固定 16384 slot,生产 **6 主 6 从**(≥3 物理机/可用区,主从异机),每主 ≤8 GB,合计 48 GB 物理可用,承载 30 万 CCU(在线键 ~30 GB + 50% 余量;峰值写 ~50 万 ops/s,单主 ~8 万安全水位有冗余),触顶在线加主重分 slot。详见 [deploy/redis/README.md](../../deploy/redis/README.md) §4。
4. **部署配置已就绪(✅,起实例交 Codex/人)**:
   - Sentinel(一主两从 + 三哨兵,立即去单点、零业务改动):[deploy/docker-compose.redis-sentinel.yml](../../deploy/docker-compose.redis-sentinel.yml)。
   - Cluster 本地验证(3 主 3 从最小集群):[deploy/docker-compose.redis-cluster.yml](../../deploy/docker-compose.redis-cluster.yml);生产 6 主 6 从用 k8s redis-operator。
   - 业务侧只改 `node.redis_client`(`master_name`+哨兵 addrs 走 Sentinel;多 `addrs` 留空 master_name 走 Cluster),代码零改动(`redisx.NewUniversalClient` 自动选型)。配置片段见 [deploy/redis/README.md](../../deploy/redis/README.md) §3。
   - **推进顺序**:先 Sentinel 拿可用性 → 压测确认单主写吞吐触顶 → 切 Cluster(改造已完成,换 addrs 即可)。

### 2.2 MySQL 水平分库

**已落地能力(本仓库,非破坏式)**:
- [pkg/mysqlx/sharded.go](../../pkg/mysqlx/sharded.go) 新增 `ShardSet`:按 `player_id` 路由 `shard = id % N`,提供 `For(id)` / `All()` / `Shard(i)`。
- `MySQLConf` 新增 `shards` DSN 列表,**留空 = 单库**,向后兼容。

**分片纪律(硬约束)**:
- 选库键统一 `player_id`(snowflake 已均匀分布,见 `CLAUDE.md` §5.5);同一玩家相关行必落同一库,**单次业务不跨库**。
- `N` 一旦定稿不可随意改(改 N → 历史数据 rehash 代价极高)。扩容走"翻倍 + 双写迁移"或预分配逻辑分片(建议起步逻辑分片 256,物理库 4,逻辑→物理可平滑搬迁)。
- 跨玩家聚合(排行榜等)**不走分库扫描**,另起离线/缓存路径。
- 雪花主键须 `AUTO_RANDOM` / hash 打散,避免分库内尾部热点(见 `friend-distributed-scaling.md` §8.2)。

**社交库特例**:`friend` / `chat`(`pandora_social`)涉及**跨玩家强一致**(加好友双向建边),已决策走 **TiDB**(见 `pandora-arch.md` §11 2026-06-18 两条 + `friend-distributed-scaling.md` §14),**不套 `ShardSet`**。`ShardSet` 适用的是 owner 单键、无跨人事务的库(player 档案、inventory、trade 单边等)。

---

## 3. snowflake nodeID 静态 → etcd 自动分配(#2,已实现)

### 问题
`zone_id` 现为 yaml 写死。上 k8s 多副本后同一服务 N 个 pod,人工排号必撞 → 发重号。

### 已落地(本仓库)
新增独立 module [pkg/snowflake/etcdnode](../../pkg/snowflake/etcdnode/etcdnode.go)(沿用 `pkg/killswitch/etcdkv` 的"独立 module 隔离重型 etcd 依赖"模式,核心 `pkg` 不背 etcd):

- `etcdnode.Acquire(ctx, Config)`:在 `[0, MaxNodeID)` 用 etcd 事务(`CreateRevision==0` 守卫)抢占独占 nodeID,绑定 lease,启动 KeepAlive 续租。
- 返回 `*Holder`:`Node()` 给 `*snowflake.Node`;`Lost()` 在失租时关闭;`Close()` 主动 revoke。
- `SnowflakeConf` 新增 `node_id_source`(`static`/`etcd`)+ `etcd_*` 字段。

### fencing 契约(接线时必须遵守)
`Lost()` 关闭 = lease 丢失。服务 main 必须:
```go
go func() { <-holder.Lost(); log.Error("nodeID lease lost"); os.Exit(1) }()
```
**收到 Lost 立即停发并退出进程**,交 k8s 重新拉起重新抢号;绝不能只打日志继续 `Generate`,否则与领走同 nodeID 的新 holder 双活发重号。

### 接线清单(进入多副本阶段时)
1. 服务 `main.go`:当 `cfg.Snowflake.NodeIDSource == "etcd"` 时用 `etcdnode.Acquire` 替代 `snowflake.NewNode(cfg.Node.ZoneId)`,并挂 `Lost()` 退出 goroutine。
2. 单副本 / dev 仍走 `static`(零改动)。
3. **Codex 待办**:`use ./pkg/snowflake/etcdnode` 加入根 `go.work`;在该目录 `go mod tidy` 生成 `go.sum`(本 module 引 etcd client,Claude 不跑 tidy,见 `AGENTS.md` §11.1)。

---

## 4. push 服务横向扩展(#3,设计,待实现)

### 现状
[services/runtime/push](../../services/runtime/push) 单实例:`ConnectionManager` 内存索引 `player_id → stream`,Kafka consumer 收事件后 `cm.SendTo(player_id)` 推到本地 stream。30 万长连接单进程扛不住(fd / 内存 / goroutine)。

### 难点
长连接是**强状态**:某玩家的 stream 只在某一个 push 实例上,业务事件必须路由到**正确的实例**。

### 方案对比
| 方案 | 做法 | 优点 | 缺点 |
|---|---|---|---|
| A 全量广播 | 每个 push 实例**独立 consumer group**,消费全量 push topic,只有持有该 stream 的实例 `SendTo` 成功 | 实现最简单,无需路由表 | Kafka 与 CPU 放大 N 倍;实例越多越浪费,~10 实例后不可接受 |
| **B 定向路由(推荐)** | 复用 `player_locator` 思路加一张 `player_id → push_instance_id` 注册表(Redis);producer/转发层按目标实例分区投递,实例只消费自己分区 | 无放大,水平可扩 | 需要注册表 + 路由层 + 顶号一致性 |

### 推荐落地(方案 B)
1. push 实例启动注册自身 `instance_id` + 地址到 Redis;玩家 Subscribe 成功时写 `route:{player_id} = instance_id`(随 stream 生命周期增删,顶号时覆盖)。
2. Kafka push topic **按 `instance_id`(或一致性哈希桶)分区**,每个 push 实例只消费自己的分区 → 无放大。
   - 业务发 push 前先查 `route:{player_id}` 得目标实例,投到对应分区(key = instance/bucket)。
   - 路由表 miss(玩家离线/迁移中)→ 落离线 ZSET(已有 `RedisOfflineCacheRepo`),上线时补推。
3. 保持 `CLAUDE.md` §9 不变量 9(topic key = 业务实体 ID,保证同一玩家有序)与"同账号一条 push 长连"顶号语义。
4. 单实例连接上限压测定容(目标 ~5 万长连/实例 → 30 万需 ~6~8 实例 + 余量)。

> 注:push 的客户端侧在 UE 仓库(`gateway-decision.md` §15),路由改造**只动后端 push + 业务 producer**,客户端无感(仍连 Envoy)。

---

## 5. Agones DS 编排吞吐(#4,设计 + 压测,基础设施)

### 规模
30 万 CCU ≈ **~1 万并发 battle DS pod + ~400 hub DS 实例**。这是巨大的 k8s 规模,瓶颈在编排吞吐与弹性,而非 go 代码。

### 要点
1. **DS 池化预热(必须)**:`ds_allocator` / `hub_allocator` 维护 Agones `Ready` GameServer **缓冲池**,匹配成功直接从池里取(亚秒级),而不是临时拉起 pod(分钟级、还要拉镜像)。按峰值开局速率定 `Fleet` `bufferSize` + `FleetAutoscaler` 水位。
2. **镜像与启动**:DS 镜像分层瘦身 + 节点本地预拉(DaemonSet 预热),消除冷启动镜像拉取尖峰。
3. **分配吞吐**:`AllocateBattle` / hub `Assign` 走 Agones Allocator gRPC(批量 + 重试 + 背压),**专门压测分配 QPS**;`ds_allocator` 状态镜像 + 心跳超时补偿(已有,见 `PROGRESS.md` W4②)保持。
4. **节点弹性**:cluster-autoscaler 多节点池(battle 计算型 / hub 常驻型分离),按 GameServer 水位扩缩;留逐出保护避免在局中回收。
5. **不变量保持**:DS 票据短时效 JWT exp 5min、DS 崩溃 15s 心跳超时 → abandoned 补偿(`CLAUDE.md` §9 不变量 3/4)在大规模下仍须成立,压测覆盖 DS 批量崩溃场景。

> 本块主要是 k8s / Agones 配置 + 压测,落地与上线由 Codex / 人执行(`AGENTS.md` §11.1);go 侧改动集中在两个 allocator 的池化与批量分配。

---

## 6. 落地优先级与状态

| 项 | 状态 | 性质 |
|---|---|---|
| #2 snowflake etcd nodeID | ✅ 代码已落地(`pkg/snowflake/etcdnode`)+ 编译通过;待 Codex `go.work`+`tidy`,待服务 main 接线 | 后端代码 |
| #1 Redis Cluster 能力 | ✅ `redisx.NewUniversalClient` + 配置已落地;待 data 层切接口 + hash tag 审计 + 起集群 | 后端代码 + 基础设施 |
| #1 MySQL 分库能力 | ✅ `mysqlx.ShardSet` + 配置已落地;社交库走 TiDB(已决策);其余库待定分片数 + 迁移 | 后端代码 + 基础设施 |
| #3 push 横扩 | 📄 方案 B 设计就绪,待实现(注册表 + 分区路由) | 后端代码(本仓库) |
| #4 Agones 吞吐 | 📄 方案就绪,待 k8s/Agones 配置 + 压测 + allocator 池化 | 基础设施 + 部分代码 |

**结论**:30 万 CCU(DAU 200 万)抗得住,前提是上述四点全部落地。架构方向本就正确(无状态优先 + 统一 uint64 snowflake + Kafka 分区有序 + 客户端最小视图)。
