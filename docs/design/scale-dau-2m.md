# DAU 200 万扩容方案(历史索引)

> 2026-07-02 收敛说明:DAU 目标已升级到 2000 万,主设计入口改为
> [`scale-cellular-20m.md`](./scale-cellular-20m.md)。本文只保留历史章节锚点,
> 避免旧决策文档断链;详细实现以当前专项文档和 `pandora-arch.md` 决策行为准。

## 1. 容量基线

原 200 万 DAU 方案的容量口径已被 `scale-cellular-20m.md` 继承:单 Cell 容量目标约
30-40 万 CCU,作为 2000 万 DAU 三层 Region/Cell 架构里的最小复制单元。

## 2. 单 Redis / 单 MySQL 去单点

单 Cell 内仍沿用 Redis Cluster / MySQL ShardSet / TiDB 社交库的路线。跨 slot 风险和
服务级改造记录见:

- [`decision-revisit-hub-crossslot.md`](./decision-revisit-hub-crossslot.md)
- [`decision-revisit-trade-crossslot.md`](./decision-revisit-trade-crossslot.md)
- [`friend-distributed-scaling.md`](./friend-distributed-scaling.md)

### 2.1 Redis -> Redis Cluster

当前实现以 `redisx.NewUniversalClient` 和服务级 hash tag / 全局索引自愈改造为准。
部署说明见 [`deploy/redis/README.md`](../../deploy/redis/README.md)。

### 2.2 MySQL 水平分库

当前实现以 `mysqlx.ShardSet` 为 owner 数据分库底座;friend/chat/guild/mail 所在社交库
继续按 TiDB 路线处理跨玩家关系,不套 ShardSet。

## 3. snowflake nodeID 静态 -> etcd 自动分配

当前实现以 `pkg/snowflake/etcdnode` 和各服务 `etcdnode.MustProvideSnowflake` 接线为准。
关键约束不变:失租必须停发并退出,避免双活发重号。

## 4. push 服务横向扩展

方向仍是定向路由:玩家长连接归属注册到路由表,业务事件按玩家路由到持有 stream 的 push 实例。
细化实现以未来 push 横扩专项设计为准。

## 5. Agones DS 编排吞吐

方向仍是 Ready 池化、镜像预热、分配吞吐压测和节点池分层。2000 万 DAU 口径下详见
[`scale-cellular-20m.md`](./scale-cellular-20m.md) 与 [`agones-dev.md`](./agones-dev.md)。
