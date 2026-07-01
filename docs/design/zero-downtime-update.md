# 不停服更新(零停机)与玩家数据在线演进

> 状态:**已拍板·标准规范**。整理人:GitHub Copilot / 2026-07-01
> 关联规范:`CLAUDE.md` §5(proto)/ §9(不变量)、`AGENTS.md` §10(红线)
> 关联代码:[pkg/redisx](../../pkg/redisx)、[services/data](../../services/data)、[proto/](../../proto)
> 关联设计:[player-data-actor-serial.md](./player-data-actor-serial.md)、[proto-design.md](./proto-design.md)、[config-table-hotreload.md](./config-table-hotreload.md)

---

## 0. 结论先行

Pandora 后端**目标就是不停服更新**,分两层保障,缺一不可:

1. **服务层**:14+ 个 Go 服务全部 **headless、无状态、可水平扩展**,靠 k8s **滚动更新(rolling update)** 逐副本替换,任意时刻新旧版本副本**同时在线**,对客户端零停机。
2. **数据层**:玩家等业务数据以 **二进制 protobuf** 存 Redis(必要时 write-behind 落 MySQL blob/列)。proto **只做兼容演进**,新版本能读旧数据、旧版本能读新数据,因此**加字段/加玩家数据不需要停服、不需要全量迁移**。

只要守住下面的规则,「上线新版本」= 逐副本重启,「给玩家加数据」= proto 加字段,**都不停服**。

---

## 1. 服务层:滚动更新的前提

| 前提 | 要求 |
|---|---|
| 无状态 | 进程内不存跨请求的权威状态;权威态在 Redis/MySQL/etcd。内存只做缓存/Actor 邮箱,副本可随时被杀 |
| 优雅退出 | 收到 `SIGTERM` 先摘流量(healthz 转 not-serving)→ 排空在途请求 → flush write-behind 脏数据 → 退出;别让滚动更新丢未落库的写 |
| 幂等 | 客户端/上游可重试;重试不产生重复副作用(战斗结算、扣费走幂等键,见 §9 不变量 2/7) |
| 前后兼容 | 新旧版本副本同时在线期间,**RPC 协议**与**存储 pb**都必须双向兼容(见 §2) |
| 有状态单例例外 | Snowflake nodeID、锁、Actor 归属靠 etcd Lease / 分片路由重新分配,不靠进程常驻(见 `snowflake` / `cellroute`) |

滚动更新期间「新版本写了新字段 → 旧版本副本读到并回写」是常态,所以**兼容性是滚动更新的硬约束**,不是可选项。

---

## 2. 数据层:Redis 二进制 pb 的 schema 演进规则(硬规则)

### 2.1 只做兼容变更(允许,不停服)

- ✅ **新增字段**:分配新的、从未用过的 field number。proto3 缺省即零值,旧数据读出来新字段就是零值,代码按「老玩家没这份数据」处理即可。
- ✅ **新增 message / 新增 enum 值**:enum 必须有 `*_UNSPECIFIED = 0`,消费侧对未知 enum 值有 fallback。
- ✅ **删字段**:先 `reserved <num>;` + `reserved "<name>";` 并注释原因,**永不复用编号**(开发期未上线的字段可复用,但须重生 proto 并全量编译,见 §9 不变量 5)。

### 2.2 禁止的破坏性变更(会读坏线上数据 / 破坏不停服)

- ❌ **改 field number**(等于删旧字段+加新字段,旧数据错位)
- ❌ **改字段类型**(`int32↔int64↔uint`、`string↔bytes` 之外的换型;`sint/fixed` 与 `int` wire type 不兼容)
- ❌ **改 `optional/repeated/map/oneof` 的基数或成员归属**
- ❌ **改字段语义**(编号类型不变但含义变,新旧副本各理解一套)
- ❌ **复用已 `reserved` 的编号**

破坏性变更**只能**走「加新字段 + 双写 + 灰度 + 下线旧字段」的多版本迁移,不允许原地改。

### 2.3 未知字段必须保留(最易踩的坑)

滚动更新期间**旧版本副本会读到新版本写入、含新字段的数据**。protobuf 默认**保留 unknown fields**并在再次序列化时原样写回。因此:

- ❌ 在 **read-modify-write** 路径(读 Redis → 改 → 写回 Redis)上**禁止** `proto.UnmarshalOptions{DiscardUnknown:true}` / `protojson` 丢弃未知字段 —— 否则旧副本回写时会把新字段**丢掉,造成静默数据丢失**。
- ✅ 只读、不回写的路径(纯展示、只 Marshal 给客户端最小视图)才可视需要 DiscardUnknown。
- ✅ 面向客户端的视图本来就要按最小数据单位重新组装(§9 不变量 14),天然不回写存储,不受影响。

---

## 3. 给玩家「加数据」的标准流程(不停服)

1. 在对应 `<Domain>StorageRecord` proto message **加新字段**(新编号),`pwsh tools/scripts/proto_gen.ps1` 重生 go pb(cpp pb 同步 UE 仓库,见 §5)。
2. 新代码写:填新字段并写回;读旧数据时新字段为零值,按缺省语义处理(懒初始化)。
3. 滚动发布:k8s 逐副本替换。期间旧副本读到新字段会**原样保留回写**(§2.3),不丢数据。
4. 老玩家的旧数据**无需批量刷库**:下次被加载改写时自然带上新字段(**懒迁移 lazy migration**)。真需要统一初始化的,写一次性后台 backfill job,不停服跑。

## 4. 需要真正「数据版本迁移」时

绝大多数「加数据」靠 §3 的懒迁移即可,不需要版本号。只有当**同一字段语义要变**或**结构要重组**时,才引入迁移:

- 存储 record 可选带 `uint32 schema_version`(或 `data_version`),加载时 `if version < N { 迁移并回写; version = N }`,**单调递增防回退**。
- 迁移在**加载路径懒执行**,或用**不停服后台 backfill**,绝不停服全量刷。
- 迁移过程保证**幂等**(重复执行结果一致),配合玩家数据单写者串行(见 `player-data-actor-serial.md`)避免并发迁移竞争。

---

## 5. 验收红线(违反即 PR 拒绝 / 立刻停止报告)

- 改动导致**旧数据反序列化出错**或**旧副本回写丢字段** → 拒。
- 在 read-modify-write 路径丢弃 unknown fields → 拒。
- 改 field number / 改类型 / 复用 reserved 编号 → 拒(见 §2.2)。
- 新版本上线要求「先停服再启动」才能读数据 → 拒(违背本文核心目标)。
- 服务持有不可重建的进程内权威状态,导致副本不可随意重启 → 拒。
