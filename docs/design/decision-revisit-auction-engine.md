# decision-revisit:全服拍卖行 / 撮合引擎独立服务(auction)

> 触发:`trade` 现为双方已知的两阶段 P2P 托管交易;业务新增需求 **全服拍卖行 / 跨人撮合**
> (无界挂单 + 价格-时间优先撮合 + 资产转移幂等)。该模型与 `trade` 的状态机、争用、分片
> 维度均不同,且碰 `CLAUDE.md` §9 不变量 #7(资源扣减原子 + 补偿幂等键)、#2(成交只落库一次)、
> #9(kafka key = 业务实体保序),按 `AGENTS.md` §7 升级为 decision-revisit。
> 决策级别:服务级(新增 `auction`)+ 存储路线(MySQL ShardSet vs TiDB)。
> 用户已于 2026-06-19 确认"确实需要全服拍卖行 / 跨人撮合 + 幂等",本文档定方案供拍板,
> 实现由另一窗口(另一 AI / 人)按 §5 执行。

---

## 1. 旧问题(现状)

- [`services/economy/trade`](../../services/economy/trade) 是 **P2P 托管交易**:买卖双方已知 → 卖家挂托管
  → 买家确认 → saga 扣减(幂等键 = `order_id`)。订单各自独立 key,无共享热点,无撮合。
- 拍卖行需求与之根本不同:

  | 维度 | `trade`(P2P 托管,保留) | 拍卖行 / 撮合(新需求) |
  |---|---|---|
  | 模型 | 双方已知 → 托管 → 确认(状态机) | 全服订单簿 + 撮合(价格-时间优先) |
  | 参与者 | 有界(买卖两人) | 无界(全服挂单 / 吃单) |
  | 热点 | 每订单独立 key,无共享热点 | 热门道具同一市场 = 单点高争用 |
  | 一致性边界 | 单边 owner,saga 补偿 | 同一市场内撮合必须原子(不超卖 / 不重复成交) |
  | 分片维度 | `order_id` | `market_id`(道具品类 / 市场) |

- 若把撮合塞进 `trade`:状态机混杂、争用模型冲突、分片维度打架,违反 `CLAUDE.md` §5.8
  "新业务优先独立 proto message,不造并行 struct"的精神,且会污染已稳定的 trade 服务。

## 2. 关键事实:撮合是"按市场分片的单写者",不是好友图那种任意人强一致

好友图走 TiDB(`friend-distributed-scaling.md`)是因为 **任意人↔任意人双向强一致**(A 加 B 要
同事务建反向边),无法靠分片消除跨分片事务。**拍卖撮合不是这样**:

- 每个 `market_id`(道具品类)由 **唯一一个撮合分片独占串行处理**(交易所 / LMAX 单写者模型);
- 同一市场内"匹配 + 双方资产转移"天然落在 **一个分片一个本地事务**,**不跨分片** → 不需要分布式 ACID;
- 正是 §9 #9(kafka key = 业务实体保序)同款思路:`key = market_id`,同市场事件有序、单写者消费。

**结论**:分片维度选 `market_id`(而非 `player_id`),把"跨人强一致"降维成"分片内本地事务",
**普通 MySQL `mysqlx.ShardSet`(按 market_id 分片)即可,不必上 TiDB**。

## 3. 新方案(推荐)

### 3.1 拆独立服务 `services/economy/auction`(与 trade 平级)

| 资源 | 取值 | 依据 |
|---|---|---|
| 服务名 | `auction`(economy 域) | 与 `trade` / `inventory` 平级 |
| gRPC / metrics 端口 | **50016 / 51016** | `infra.md` §6.2 economy 段空档(trade 50012、inventory 50015 之后) |
| proto 包 | `pandora/auction/v1/auction.proto` | `proto-design.md` §1 目录规范 |
| 错误码段 | **12000-12999** | `proto-design.md` §4(7000-7999=trade,11000+ 预留,取新段) |
| MySQL 库 | **`pandora_auction`** | `infra.md` §2.1 "按职能分库",撮合表与 trade 解耦 |
| Kafka topic | `pandora.auction.match`(成交)、`pandora.auction.audit`(审计 append-only) | `proto-design.md` §5 `pandora.<domain>.<event>` |

> ⚠️ 上述端口 / 错误码段 / 库名 / topic 需同步登记进 `infra.md` §2.1/§4/§6.2 与
> `proto-design.md` §4/§5,实现窗口落地时一并更新(不允许 ad-hoc,见 `infra.md` 总则)。

### 3.2 单写者分市场撮合

- 每个 `market_id` 路由到一个撮合 worker(分区一致性哈希,沿用 `pkg/kafkax/consistent.go` 思路);
- 挂单 / 出价 / 撤单进 **同市场单写者队列**,串行撮合,杜绝并发超卖;
- 撮合命中 → 生成 `match_id`(雪花 uint64)→ 走资产转移结算。

### 3.3 存储分层

| 层 | 介质 | key / 表 | 说明 |
|---|---|---|---|
| 活跃订单簿 | Redis ZSET | `pandora:auction:book:{<market_id>}` (score=价格×时间优先) | hashtag `{market_id}` 锁同 slot,`ZADD`/`ZRANGE`/`ZREM` 撮合;Cluster 安全 |
| 权威订单 | MySQL `ShardSet`(按 `market_id` 分片) | `auction_orders` uniq(order_id), idx(market_id, status), idx(owner_id) | 订单事实源 |
| 成交流水 | MySQL 同分片 | `auction_matches` uniq(match_id), idx(market_id, created_at) | 撮合 + 资产转移落同一本地事务 |
| 审计 | Kafka append-only | `pandora.auction.audit` | 风控 / 对账 |

> 通用字段(`created_at/updated_at/deleted_at/version`)按 `infra.md` §2.2;
> 雪花主键热点须 `AUTO_RANDOM` 打散(沿用 friend §8.2 经验)。

### 3.4 两层幂等(回应用户"也要幂等")

1. **提交幂等**:挂单 / 出价 / 撤单带客户端 `idempotency_key`,重试不重复挂单、不重复冻结资产
   —— 对应 §9 #7"补偿幂等键"。冻结(下架 / 锁金币)由结算原语保证。
2. **结算幂等**:撮合成交以 `match_id`(雪花 uint64)为幂等键,资产转移**只落一次**
   —— 对应 §9 #2(同 match_id 只落库一次)同款不变量。
3. **结算复用 trade 的 saga / `ResourceLedger` 原语**,不重造:auction 撮合出 `match` →
   调统一结算路径(扣冻结 + 入账 + 补偿),幂等键 = `match_id`。

### 3.5 ID 类型(`CLAUDE.md` §5.5/§5.6)

| 字段 | 类型 | 说明 |
|---|---|---|
| `order_id` / `match_id` | `uint64`(雪花) | 运行时业务实体 |
| `owner_id` / `bidder_id` | `uint64`(雪花) | 玩家 ID |
| `item_config_id` | `uint32` | 配置表道具 ID |
| `market_id` | `uint32`(配置:道具品类 / 市场) | 撮合分片维度;若运行时动态市场则改 `uint64` 并文档说明 |
| 状态 / 撮合原因 | proto enum(int32 语义) | 不因非负改 uint32 |

### 否决的备选

- **(A) 塞进 `trade` 服务**:状态机 / 争用 / 分片维度冲突,污染已稳定服务。**否决**(见 §1)。
- **(B) 撮合库走 TiDB**:撮合是单写者分市场、无跨分片事务,TiDB 的跨人强一致能力用不上,
  徒增运维面与雪花热点风险。**否决**(见 §2);留作"全服统一订单簿且必须跨市场强一致"的极限场景再议。
- **(C) 分片维度用 `player_id`**:会让"同一市场撮合"跨分片,反而制造跨分片事务。**否决**。
- **(D) 用 `macrozheng/mall` 等 Java 电商替代**:违反 `CLAUDE.md` §12(go 服务 headless、禁 GUI 库)
  与 proto / 雪花 / Kafka / k8s 契约,引入第二语言栈。**否决**;其订单状态机 / 防超卖思路可借鉴,
  实现留在 Go gRPC 体系。

## 4. 风险

| 风险 | 等级 | 缓解 |
|---|---|---|
| 热门市场单写者成为吞吐瓶颈 | 中 | 单写者只串行"撮合判定",资产转移异步幂等;必要时按价格档 / 子市场再分片 |
| Redis 订单簿与 MySQL 权威漂移 | 中 | MySQL 为事实源,Redis 簿为加速结构;启动 / 定时对账重建簿,撮合前校验权威态 |
| 冻结资产未释放(挂单后崩溃) | 中 | 冻结带 `idempotency_key` + TTL;撤单 / 过期回收走补偿幂等 |
| `match_id` 重复结算 | 无 | §9 #2 幂等键,唯一约束 uniq(match_id) 兜底 |
| 雪花主键写热点 | 低 | `AUTO_RANDOM` 打散 |

## 5. 迁移成本 / 实现范围(给实现窗口)

**纯新增,不改 trade**:

1. `proto/pandora/auction/v1/auction.proto`:`AuctionService`(ListMarket / PlaceOrder / Bid /
   CancelOrder / ListMyOrders)+ message(`AuctionOrder` 客户端可见结构、`AuctionOrderStorageRecord`
   存储快照若有独有字段、`AuctionMatchEvent` 事件)。proto 改动 commit 标 `[proto]`(`CLAUDE.md` §4)。
2. `services/economy/auction/`:cmd + internal(biz / data / service / server / conf),Kratos,
   骨架对齐 trade 目录结构。data 层 `redis.UniversalClient` + `mysqlx.ShardSet`。
3. `deploy/mysql-init/NN-auction-tables.sql`:`pandora_auction`.{`auction_orders`,`auction_matches`}。
4. 文档登记:`infra.md`(§2.1 库、§4 topic、§6.2 端口 50016/51016)、`proto-design.md`(§4 错误码 12000、§5 topic)、
   `pandora-arch.md` §3 服务清单加一行 + §11 决策表追加 + `PROGRESS.md` 追加。
5. 结算复用 trade `ResourceLedger` / saga;若该原语在 trade 内部,需抽到 `pkg/` 或 economy 公共层供两服务共用(实现窗口评估,避免复制)。

**不变**:trade 服务、其 proto、其库表零改动。

## 6. 验收标准

1. `go build ./...` + `go test ./...` auction 模块 EXIT=0 全绿;不破坏 trade / 其他模块(workspace 编译通过)。
2. 撮合单元测试:同市场并发挂单 / 吃单,断言无超卖、价格-时间优先正确、`match_id` 唯一。
3. 幂等测试:重复 PlaceOrder(同 idempotency_key)不重复挂单;重复结算(同 match_id)资产只转一次。
4. Redis Cluster 兼容:订单簿操作均在 `{market_id}` 同 slot,无 `CROSSSLOT`(grep 确认无跨 slot 多键事务)。
5. 文档登记齐全(§5.4),无 ad-hoc 端口 / topic / 库名。

---

## 决策

- [x] 方向确认:用户 2026-06-19 确认需要"全服拍卖行 / 跨人撮合 + 幂等",**独立 `auction` 服务**、
  **MySQL ShardSet 按 market_id 分片(不上 TiDB)**、**两层幂等(idempotency_key + match_id)**。
- [ ] 实现细节(proto 字段 / 端口 / 库表 / 错误码段最终值)审定后由实现窗口落地,落地时同步更新 §5.4 各文档。
- [ ] 若实现中发现"必须跨市场强一致"场景(推翻 §2 单写者前提),停下重走 decision-revisit。
