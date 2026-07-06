# decision-revisit:hub_allocator 跨 slot 事务改造

> 触发:Redis 去单点(`scale-cellular-20m.md` 单 Cell 口径)切 Cluster 后,`hub_allocator` 数据层 4 个方法
> 把 per-pod 镜像键与全局索引键捆进同一事务,跨 slot → `CROSSSLOT`。本文档按 `AGENTS.md` §7
> 给出旧问题 / 新方案 / 风险 / 迁移成本 / 验收,供人拍板。
> 决策级别:服务级(hub_allocator);牵涉 §1 一人一 hub 不变量边界,故升级为 decision-revisit。

---

## 1. 旧问题(现状)

[`services/battle/hub_allocator/internal/data/hub_repo.go`](../../services/battle/hub_allocator/internal/data/hub_repo.go)
的 key 拓扑:

| key | 角色 | slot 决定因素 |
|---|---|---|
| `pandora:hub:shard:{<pod>}` | **分片镜像权威**(proto bytes) | `{pod}` |
| `pandora:hub:shard:members:{<pod>}` | 分片成员反查(SET) | `{pod}`(与镜像同 slot) |
| `pandora:hub:shards` | **全局**分片注册(SET) | 固定整 key |
| `pandora:hub:active` | **全局**心跳扫描(ZSET) | 固定整 key |
| `pandora:hub:player:<id>` | 玩家归属(单键) | 整 key(独立,不在问题事务内) |

4 个方法把 per-pod 键(slot=`{pod}`)和全局键(`shards`/`active`,各自固定 slot)捆进一个事务:

| 方法 | 事务内 key | 跨 slot |
|---|---|---|
| `CreateShard` | `SET shard{pod}` + `SAdd shards` | 2 slot ❌ |
| `UpdateShardWithLock` | `WATCH+SET shard{pod}` + `SAdd shards` | 2 slot ❌ |
| `HeartbeatShard` | `WATCH+SET shard{pod}` + `SAdd shards` + `ZAdd active` | 3 slot ❌ |
| `RemoveShard` | `Del shard{pod}` + `Del members{pod}` + `SRem shards` + `ZRem active` | 3 slot ❌ |

## 2. 关键事实:全局索引本就按"可漂移"设计

- `shardKey{pod}` 是分片**权威**,带 TTL,Hub DS 心跳续命;镜像在 = 分片在。
- `shards` SET / `active` ZSET 是**派生扫描加速器**,非权威:
  - [`ListShards`](../../services/battle/hub_allocator/internal/data/hub_repo.go) 已自愈:"镜像已过期但 SET 残留 → 顺手 `SRem` 清理"。
  - `active` ZSET 仅供 `RangeStaleShards` 扫超时;真正过期由 `shardKey` TTL 决定。
  - `members{pod}` 注释明写 "best-effort:漂移不影响正确性"(双通道中 Hub DS drain 心跳兜底)。
- 玩家归属 `assignKey`(§1 一人一 hub 不变量的真正载体)是**单键**,不在这 4 个事务里,**本改造不碰它**。

**结论**:把权威镜像与全局索引解耦,不破坏任何不变量(权威镜像 + 同 slot 成员仍原子)。

## 3. 新方案(推荐)

原则同 trade:**权威/同 slot 键保持原子事务;全局索引拆成独立命令**(Cluster 自动路由)。

| 方法 | 改后 |
|---|---|
| `CreateShard` | ① `SET shard{pod}`(权威,原子)→ ② `SAdd shards`(独立命令) |
| `UpdateShardWithLock` | `WATCH+SET shard{pod}` 只围单 slot;成功后 `SAdd shards` 独立 re-ensure(幂等,membership 已在 create 建立,best-effort) |
| `HeartbeatShard` | `WATCH+SET shard{pod}` 只围单 slot;成功后 `SAdd shards` + `ZAdd active` 独立命令 |
| `RemoveShard` | `Del shard{pod}`+`Del members{pod}` 同 slot mini-tx;再 `SRem shards`+`ZRem active` 独立命令 |

**幂等性**:`SET`/`SAdd`/`ZAdd`/`Del`/`SRem`/`ZRem` 全幂等;任一步失败返回 error,调用方重试
(DS 心跳本就周期重试,allocator 操作可重入)整体收敛。`members{pod}` 与 `shard{pod}` 同 hashtag
`{pod}`,保持同 slot,可继续 mini-tx。

### 否决的备选
- **(B) 全局键加 hashtag 绑到某 pod 的 slot**:`shards`/`active` 是**全局单例**,绑任一 `{pod}`
  都破坏"全局"语义且仍与其他 pod 跨 slot。**否决**。
- **(C) 全局索引也改 per-pod 分片键**:会把"一次扫所有分片"放大成"遍历所有 pod key",
  失去 SET/ZSET 一次扫描的意义。**否决**。

## 4. 风险

| 风险 | 等级 | 缓解 |
|---|---|---|
| 镜像写成功、`shards` SAdd 失败 → `ListShards` 暂时漏这个分片 | 中 | 调用方重试;`Create` 必建 membership(返 error);`Update/Heartbeat` 是 re-ensure,下次心跳补 |
| 镜像写成功、`active` ZAdd 失败 → `RangeStaleShards` 暂时漏扫 | 低 | 心跳高频(~10s)下次即补;`shardKey` TTL 仍会过期;双通道 drain 兜底 |
| `RemoveShard` 删镜像成功、`SRem/ZRem` 失败 → 残留索引成员 | 低 | `ListShards` 自愈清理;`active` 残留项指向已删镜像,扫到后 `GetShard` miss 跳过 |
| 单 Redis / Sentinel 回归 | 无 | 单实例下独立命令与原事务行为一致 |

## 5. 迁移成本

- 改动仅 `hub_repo.go` 4 个方法;`HubRepo` 接口签名、biz / service / proto **零改动**。
- 现有 miniredis 测试(`hub_repo_test.go`)行为不变(单实例等价),无需改测试。

## 6. 验收标准

1. `go build` hub_allocator 模块 EXIT=0;`go test ./...` 全绿。
2. 4 个方法不再出现 per-pod 键与全局键混在一个 `TxPipeline`(grep 确认)。
3. 单实例 / Sentinel:create→list、heartbeat→active 扫描、remove→清理 行为与改前一致。
4. (上 Cluster 后人工验)4 个方法不报 `CROSSSLOT`;索引漂移由自愈 + 重试收敛。

---

## 决策

- [x] 人拍板:采用 §3 推荐方案(2026-06,用户“拍板后改事务”授权)
- 拍板后:代码已就绪(见 §5 改动范围),`go build ./...` + `go test ./...` 均 EXIT=0（data 层测试实跑 PASS 0.063s）。
