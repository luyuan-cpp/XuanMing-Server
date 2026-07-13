# decision-revisit：拍卖撮合权威、持久 Saga 与不停服迁移

> 日期：2026-07-12  
> 状态：本次业务阻断修复已获授权实施；上线前仍须 Claude Code 独立复审和人确认发布门禁  
> 取代：`decision-revisit-auction-engine.md` 中 Redis ZSET 作为撮合权威、直接结算后再记流水的旧阶段描述  
> 关联：`zero-downtime-update.md`、`infra.md`、`docs/ops/release-checklist.md`

## 1. 阻断根因

旧实现把只按 `market_id + side` 分簿的 Redis ZSET 当候选权威，但一个 market 可以包含多个
`item_config_id`。临时移出异物品/自有单再放回无法关闭进程退出、缓存失败、固定前缀饥饿和
重建竞态。更严重的是旧挂单顺序为 `INSERT OPEN → Freeze → ZADD`：进程在前两步之间退出会
留下 MySQL 活跃、实际没有 escrow 的孤儿；新 matcher 若直接读取它，会先锁住正常对手方并写
待结算成交，随后结算永久失败。

同时存在以下独立阻断：

- 双方订单推进、成交事实和外部 inventory 结算没有持久意图，崩溃窗口可超卖或永久锁资；
- 固定 oldest `LIMIT` 的补偿队列会被永久失败记录占满；
- match 先标完成再发 Kafka，二者之间退出会确定性丢成交事件；
- 单玩家可无限创建 active/pending 订单，`ListMyOrders` 又无分页；
- `uk(owner_id,idempotency_key)` 只在 market 分片内唯一，跨分片可重复冻结；
- 在线迁移若用一次无界全表 DML，会造成长事务、复制积压和不可控锁等待。

## 2. 最终状态机

### 2.1 新订单与 escrow

1. 事务外有界扫描 legacy，再在 owner coordinator 单库事务登记全局 canonical
   `owner_id + idempotency_key`；相同键不同指纹明确返回冲突，相同指纹始终映射同一个
   `order_id + market_id`。coordinator 事务期间不访问其他 shard，避免双向请求占满
   `database/sql` 连接池形成互等环；commit 后才幂等补回 market PENDING。
2. MySQL 以内部 `PENDING` 插入订单；Redis Lua 原子预留单玩家 active/pending 名额。
3. 以 `order_id` 调 inventory `FreezeForOrder`。成功后在 MySQL 条件置
   `escrow_verified=1`；失败则把 PENDING 终结并登记幂等 Release。
4. incoming 保持 PENDING 进入撮合，避免“先激活、后撮合”之间退出留下无人续跑的交叉订单；
   未完全成交时才条件激活为 OPEN。

旧二进制和历史行省略 `escrow_verified` 时默认 0，绝不能成为 resting。后台在同一 market 锁内
调用 inventory 内网 `EnsureAuctionEscrow`：

- active escrow 行必须校验 owner/order、方向、item、状态和剩余数量/金额；
- 无 escrow 时按订单剩余量幂等补冻；余额不足、closed 或指纹冲突则安全取消并 Release；
- 成功后才条件置 `escrow_verified=1`，然后准入新 matcher。

这一步依赖发布前先让所有旧 auction 实例启用同一 Redis market 锁；否则验证器可能撞上旧版
正在执行的 `INSERT OPEN → Freeze` 窗口。它是明确的发布前置门禁，不允许跳过。

### 2.2 MySQL 权威撮合

候选只从 MySQL 查询：

```text
market_id + item_config_id + opposite side + escrow_verified=1 + active status
```

卖盘按 `price ASC, order_id ASC`，买盘按 `price DESC, order_id ASC`，并排除 incoming owner。
`ReserveMatch` 在一个 market 分片本地事务内按 order_id 固定顺序 `FOR UPDATE` 两张订单，再复验
市场、物品、方向、非自撮合、verified、状态、交叉价格和剩余量；随后原子：

- 插入 `settlement_status=PENDING` 的成交意图；
- 推进双方 filled/status；
- incoming 仍有余量时置 `match_pending=1`；
- 终态订单置 `release_pending=1`，并撤销其 matchable/verified 标记。

Redis ZSET 只保留旧版本兼容双写，不参与新版本候选正确性。Redis 仍是服务强依赖，用于跨实例
market 锁和 owner 配额；该锁不是 fencing token，最终不超卖由 MySQL 行锁、条件更新和唯一键保证。

### 2.3 持久副作用与公平重试

inventory 外部调用全部晚于 MySQL 意图：

- `Settle(match_id)` 成功后条件把 match 置 COMPLETED；
- 只有不再被 PENDING match 引用的终态订单才可 `Release(order_id)`；
- `Settle`、`Release` 均以业务 ID 幂等，调用成功但清 marker 失败时可安全重放；
- PENDING 冻结恢复、`match_pending` 续跑、结算、释放各自持久保存下次就绪时间。失败记录退避，
  后续记录不会被固定批次永久饿死；所有分片每轮各取有界批次。

成交事件使用 match 行 outbox marker：Complete 与 `event_pending=1` 同一更新，后台以 match_id 为
Kafka key 至少一次投递，成功后条件清 marker。同步撮合临界区不调用 Kafka；补偿循环先处理可释放
escrow，事件由独立 worker 处理，因此底层 producer 的长网络超时不会拖住 market 锁或资产释放。
投递成功后清 marker 前退出只会产生可幂等消费的重复，不再产生确定性丢失窗口。迁移直接以默认 1
新增 `event_pending`：仍运行的旧 `RecordMatch` 省略列时也会自动登记，同时历史 COMPLETED 成交按
有界批次受控重放；事件扫描只以双方**终态**订单的 `release_pending` 为持久屏障，escrow 未释放时
不可见；兼容旧默认 1，OPEN/PARTIAL 即使 marker=1 也不阻塞。资产 worker 每轮先处理既有 ready
release，再做可能耗时的冻结恢复/结算。
消费者必须按 `match_id` 幂等。订单 audit 仍是弱依赖观测流，经有界内存队列异步
发送，队列满时告警丢弃，不承担资产正确性，也不能阻塞 market 锁或资产补偿。

### 2.4 有界列表

- `max_active_orders_per_player` 默认 200；Redis Lua 以 owner SET 原子 `SCARD + SADD` 预留，满额时
  每次先从所有 MySQL 分片按最老 `order_id` 有界读取最多 201 个 PENDING/活跃单并原子预热，
  因此 Redis 重建或升级后的 legacy 活跃单不会漏计。满额时只在最多 200 个成员内按 MySQL 状态
  惰性清理终态/不存在记录，再重试一次；仍满返回
  `ERR_AUCTION_ORDER_LIMIT`。
- slot member 同时编码 `market_id + order_id`，可精确回查 market 分片；订单终态立即尽力释放，
  进程退出遗留由满额清理回收。
- `ListMyOrders` 使用 `order_id` 降序 cursor；每个分片都带 SQL `LIMIT max+1`，全局归并后最多返回
  100 条。旧客户端传零值也按默认 50 收敛。

### 2.5 owner 幂等分片容量边界

registry miss 时必须在 owner guard 事务前对所有 market 分片做一次 `(owner_id,idempotency_key)`
索引点查，才能兼容升级前已散落在任意 market 的订单。因而当前只批准单库或 2 分片，启动时对
`N>2` fail-fast；并明确接受任一分片故障时新建拍卖单 fail-closed，已持久订单的补偿仍按可用分片继续。
扩到更多分片前必须先完成旧订单 registry 全量回填、跨分片冲突审计和持久“回填完成”标记，
使常态新 key miss 不再广播；不得只改配置绕过门禁。

`N<=2` 不是可重分片许可。每个物理库的 `auction_shard_topology` 持久记录 generation、总片数、
逻辑下标、有序 topology hash 和脱敏 DSN 身份 hash；启动 exact-match。单库首次升级自动登记，
双分片首次登记必须临时显式 `allow_shard_topology_bootstrap=true`，成功后恢复 false。已有 marker 时，
改 `1↔2`、交换 DSN 顺序、重复指向同库或换目标库都会 fail-fast，bootstrap 也不能覆盖。

## 3. Schema 与索引

`000002` 是 additive expand migration，主要新增：

- order：`release_pending`、`match_pending`、`escrow_verified`、各补偿 next-attempt 字段；
- match：`settlement_status`、结算/事件 next-attempt、`event_pending`；
- owner coordinator：`auction_owner_guards`、`auction_idempotency_keys`；
- topology gate：`auction_shard_topology`；
- verified 价格顺序、各补偿就绪队列和 pending-match 引用索引。

撮合索引以等值 `escrow_verified=1` 放在 price 之前；卖盘使用普通升序索引，买盘另有
`price DESC, order_id ASC` 混合方向索引：

```sql
(market_id, item_config_id, side, escrow_verified, price, order_id)
```

避免旧 `(status, price)` 在 `status IN (...)` 后无法直接保持全局价格顺序。查询仍复验 status；旧版
混跑产生的 verified 终态由有界清扫撤销标记。

版本化 SQL 不执行无界全表 UPDATE；同一物理表的所有缺失二级索引合并为一次在线 ALTER，避免
旧大表重复扫描。`event_pending` 以兼容默认 1 一次 INSTANT DDL 新增，不依赖应用写门或
`LOCK TABLES`，从而不存在 default 切换窗口；历史 COMPLETED 行会由独立事件 worker 按每片有界
批次重放，发布前必须评估 backlog 与 Kafka/消费者容量。新增列的兼容 default 覆盖受支持的旧基线；
`000002` 尚未发布，早期 default=0
草案不属于可迁移基线。已在任何共享环境执行的 migration 文件永久 immutable；后续修正只能
增加更高版本。

## 4. 不停服发布顺序

这不是普通滚动，必须按以下蓝绿阶段执行：

1. **R0 旧版安全配置**：所有旧 auction 实例先改用 etcd Snowflake node ID，并强制
   `cross_instance_lock=true`；确认全部重启、生效且没有 static node 冲突。
2. **R1 inventory 先行**：滚动发布含 `EnsureAuctionEscrow` 的 inventory，保持旧 RPC 兼容。
3. **R2 expand migration**：对清单中的每个 auction shard 跑版本化迁移；runner 必须 exact-match
   `name:migration_set:database`，TLS、dirty、版本和超时门禁全绿。`LOCK=NONE` 不等于没有 MDL，
   仍须在真实规模评估建索引时间与锁等待。
4. **R3 green 被动预热**：以 `passive_warmup=true` 启动不接写流量的 green。该门禁同时拒绝
   Place/Bid/Cancel，并禁用 legacy verifier、side-effect reconciler 和 expiry sweeper；此阶段只能做
   连接、读路径、拓扑和健康验证，绝不能把 old 活跃单提前置 verified。确认配置为单库或恰好 2 分片；
   双分片首次 marker 登记经人工核对临时打开 bootstrap，成功后立即恢复 false。
5. **R4 写门一次切流**：入口先暂停 `PlaceOrder/Bid` 新写（读流量可继续），排空 old 在途写并
   等待至少一个旧 market-lock TTL；随后下线 old、把写流量一次切到 green 再恢复入口。禁止 green
   已接新写时 old 仍可写 market 分片，否则 owner coordinator 无法约束旧二进制，跨 market 同 key
   仍可能双插单/双冻结。old 全部退出后，将 green 以 `passive_warmup=false` 重启，确认补偿器已启动，
   才恢复写入口。若业务不能接受这段短写冻结，必须先滚一版也使用 owner coordinator 的兼容旧版，
   再做完整 green 切换。切流后继续扫描直到 active unverified、PENDING side effect 和旧 owner slot
   均收敛。
6. **R5 扩容**：只扩 green。不得回滚到仍从 Redis 选候选或不认识持久意图的旧二进制；应用回滚
   必须使用保留本 schema/状态机的 forward-fix。

仓库只提供迁移 Job 模板和配置生成器，不创建生产 Secret、不切 Service、不 apply 生产，也没有
自动替代人工确认 R0/R4 的权限。缺少任一门禁时必须阻断发布，而不是要求停服绕过。

## 5. 验收

- 同 market 异物品、自有最优单、陈旧 Redis 条目均不影响精确价格时间撮合；
- 并发 Reserve 只有一笔成功，不超卖；PENDING match 完成前双方 escrow 不释放；
- Claim/Freeze/Reserve/Settle/Release/Event 任一点退出后由持久 marker 收敛；
- legacy OPEN 无 escrow 会补冻或取消，未经验证永不进入 Reserve；
- 永久失败记录退避后，batch=1 仍能处理后续就绪记录；
- 跨 market 复用 owner idem key 返回原订单或明确冲突；
- 两个 shard 各仅一条池连接的反向 Claim 不互等；片数、顺序、重复 DSN 漂移均拒绝启动；
- 第 201 张 active/pending 单原子拒绝，列表跨分片 cursor 分页且单次不超过 100；
- migration 在新库、旧基线、多 shard、重复执行和 dirty/version 异常下均按预期；
- auction、inventory、player 的 test/vet，全量 proto lint/generate，真实 MySQL 8.4 仓储与迁移回归通过。

尚需上线前外部完成：真实规模 `EXPLAIN ANALYZE`/索引建造评估、auction↔inventory gRPC + Kafka
故障注入、启用 CGO 后的 `-race`、生产蓝绿 Service 切换与监控 runbook。它们不能被本地单测冒充。
