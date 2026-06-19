# decision-revisit:trade.CreateOrder 跨 slot 事务改造

> 触发:Redis 去单点(`scale-dau-2m.md` §2.1)切 Cluster 后,`trade.CreateOrder` 的
> `TxPipeline` 跨 3 个 slot,Cluster 下报 `CROSSSLOT`。本文档按 `AGENTS.md` §7 给出旧问题 /
> 新方案 / 风险 / 迁移成本 / 验收,供人拍板。
> 决策级别:服务级(trade);牵涉 `CLAUDE.md` §9 不变量 #7,故升级为 decision-revisit。

---

## 1. 旧问题(现状)

[`services/economy/trade/internal/data/trade_repo.go`](../../services/economy/trade/internal/data/trade_repo.go) `CreateOrder`
用一个 `TxPipeline` 原子写三类 key:

| key | 角色 | slot 决定因素 |
|---|---|---|
| `pandora:trade:order:{orderID}` | **订单权威主体**(proto bytes) | `{orderID}` |
| `pandora:trade:player:<sellerID>` | 卖家反查索引(SET of order_id) | 整 key(无 hashtag) |
| `pandora:trade:player:<buyerID>` | 买家反查索引(SET of order_id) | 整 key(无 hashtag) |

三个 key 分属 **不同 slot**(订单、卖家、买家是三个不同业务实体,无法共享一个 hash tag)。
单 Redis / Sentinel 下 `MULTI/EXEC` 正常;**Redis Cluster 下同一事务跨 slot → `CROSSSLOT` 直接报错**。

## 2. 关键事实:这里不涉及"资源扣减原子性"

`CLAUDE.md` §9 不变量 #7 = **"交易资源扣减必须原子 + 有补偿幂等键"**。核查 `CreateOrder` 的职责:

- 资源扣减(扣道具 / 扣金币)由 **biz 层 `ResourceLedger`** 做,幂等键 = `order_id`,**不在本事务内**。
- `CreateOrder` 只做两件事:① 持久化订单主体(`orderKey`,单键);② 给买卖双方建反查索引(`playerKey` set)。
- 订单主体是**权威**;`playerKey` set 仅服务 `ListMyOrders` 反查,是**派生的二级索引**。
  - 佐证:[`ListPlayerOrderIDs`](../../services/economy/trade/internal/data/trade_repo.go) 已对脏成员 `continue` 跳过,即索引本就按"可漂移"设计。

**结论**:把订单主体与反查索引解耦,**不触碰 #7**(#7 约束的是 ledger 资源扣减,订单主体写仍是单键原子)。

## 3. 新方案(推荐)

**权威单键原子写 + 反查索引独立命令**(Redis Cluster 下单命令按 slot 自动路由,不捆事务):

1. `SET orderKey({orderID})` —— 订单主体权威落库,**单 slot 原子**;失败 → 整体失败,无残留。
2. 卖家索引:`SADD + EXPIRE playerKey(seller)` —— 同 key 同 slot,可用一个 mini-`TxPipeline`(Cluster 合法)。
3. 买家索引(若有):`SADD + EXPIRE playerKey(buyer)` —— 同上,独立 mini-tx。

**失败语义**:任一步失败 → 返回 error。调用方重试 `CreateOrder` **完全幂等**:
- `SET` 同 payload 覆盖 = 幂等;`SADD` 集合成员 = 幂等;`EXPIRE` 刷新 TTL = 幂等。

**漂移收敛**:订单主体写成功、索引写失败的窗口内,订单可经 `GetOrder` 直接访问;
重试补齐索引;即使不重试,订单 TTL 与索引 TTL 对齐,`ListMyOrders` 跳脏成员兜底。

### 否决的备选
- **(B) 给 playerKey 加 hashtag 强制同 slot**:订单、卖家、买家是三实体,无法共享一个 tag;
  若强行 `{seller}` 则订单与买家又跨 slot,治标不治本。**否决**。
- **(C) Lua 脚本**:Lua 同样要求所有 key 同 slot,跨 slot 一样报错。**否决**。
- **(D) 反查索引改 Kafka 异步建**(对齐 friend §5 路线):可行但本场景索引 TTL 短、量小、
  重试已幂等,引入 Kafka 反而增加运维面。**暂不采用**,留作极限体量再议。

## 4. 风险

| 风险 | 等级 | 缓解 |
|---|---|---|
| 订单已落库但索引缺失(进程在两步间崩溃且调用方未重试) | 低 | 订单可经 `GetOrder` 直接访问;`ListMyOrders` 短时缺这一条,TTL 到期自然一致 |
| 索引重复 SADD / 重复 EXPIRE | 无 | 幂等,无副作用 |
| 单 Redis / Sentinel 回归 | 无 | 单实例下独立命令与原 `TxPipeline` 行为一致(顺序写) |

**净变化**:牺牲"订单主体 + 反查索引"之间的强原子性,换取 Cluster 可用性;
而该原子性本就不被任何不变量要求(订单主体自身仍原子)。

## 5. 迁移成本

- 改动仅 `trade_repo.go` `CreateOrder` 一个方法 + 一个私有 helper(`addPlayerIndex`)。
- biz / service / proto **零改动**;`TradeRepo` 接口签名不变。
- 现有 miniredis 测试不变(单实例下行为等价),无需改测试。

## 6. 验收标准

1. `go build` trade 模块 EXIT=0;`go test ./...` 全绿。
2. `CreateOrder` 不再出现跨 slot 的 `TxPipeline`(grep 确认)。
3. 单实例 / Sentinel 下:create → `ListMyOrders` 能查到双方;行为与改前一致。
4. (上 Cluster 后人工验)Cluster 下 `CreateOrder` 不报 `CROSSSLOT`;崩溃重试幂等。

---

## 决策

- [x] 人拍板:采用 §3 推荐方案(2026-06,用户“拍板后改事务”授权)
- 拍板后:代码已就绪(见 §5 改动范围),`go build ./...` + `go test ./...` 均 EXIT=0（biz 测试 PASS）。
