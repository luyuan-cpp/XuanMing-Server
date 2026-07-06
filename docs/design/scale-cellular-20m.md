# DAU 2000 万单元化(Region → Cell)三层扩容方案

> 触发:老板需求再次上调 —— DAU 目标从 200 万 → 1000 万 → **2000 万(10×)**;按上界系数 30% 估,峰值 **~600 万 CCU(~15× 当前单集群天花板)**。
> 本文档回答"为何单集群、乃至单一全局协调层都会到顶"并给出 **Region(大区) → Cell(单元) → Cell 内分片** 的三层演进路线。
> 决策级别:**架构级**(索引见 `pandora-arch.md` §11)。
> 状态:**已拍板,进入落地**(2026-06-26 人拍板 §8 全部 6 项,结论见 §8)。落地从最底层确定性路由地基(`pkg/cellroute`)起步,基础设施/多 k8s/部署部分按 `AGENTS.md` §11.1 由 Codex/人执行。

---

## 0. 一句话结论

- 原 200 万 DAU 单集群方案的四项改造只把**单一逻辑集群**推到 ~40 万 CCU 天花板。
- **Cell(单元)化**把"一整套 infra"打包成积木,可线性复制到 ~300 万 CCU(8~10 Cell)+ **单一全局协调层**。这是 2000 万的**第一层骨架**。
- **600 万 CCU 时,Cell 仍能线性堆(16~20 Cell),但"单一全局协调层"(全局 matchmaker / 跨 Cell 消息总线 / social TiDB)会先到顶** —— N×N 跨 Cell 扇出、全局撮合 QPS、好友图谱写放大,单点扛不住。
- 解法:**在 Cell 之上再抬一层 Region(大区)**,形成 **Region → Cell → Cell 内分片** 三层。全局协调层**按 region 分片**,同 region 内 N×N、跨 region 几乎不交互,把全局压力从"全网 N×N"压成"每 region 内 N×N + region 间极少量"。
- **架构基因仍不用推翻**(无状态 + uint64 snowflake + 取模/哈希路由),是**在单元化基础上再抬一层**,3 层都是同一种"先算后定位"的分片思想。

---

## 1. 容量基线:2000 万 DAU 到底要扛多少

沿用原 200 万 DAU 单集群方案口径,**CCU 系数取上界 30%(全区全服爆款 MOBA,不低球)**:

| 指标 | 含义 | 2000 万 DAU 估算 | 说明 |
|---|---|---|---|
| 注册总量 | DB 总行数 | ~1 亿~2 亿 | 行数本身无压力,压力在分布与热点 |
| DAU | 日活 | 2000 万 | 老板新目标 |
| **峰值 CCU** | 峰值同时在线 | **~600 万(上界 ~700 万)** | DAU×30%,首日/版本尖峰再 ×1.3~1.5 |
| 在大厅 | hub DS 容量 | ~400 万 | 500 人/实例 → **~8000 个 hub DS 实例** |
| 在战斗 | battle DS 容量 | ~200 万 | 10 人/局 → **~20 万个并发 battle DS pod** |
| 登录峰值 QPS | login | ~十万级/s | 早晚高峰 + 重连风暴放大 |

**关键数字:600 万 CCU、~20 万 battle pod、~8000 hub 实例,是原 200 万 DAU 单集群方案(30~40 万 CCU)的 ~15 倍、1000 万版方案的 2 倍。**

---

## 2. 两道墙:单集群到顶,单一全局协调层也到顶

### 2.1 第一道墙 —— 单逻辑集群(~40 万 CCU 触顶)

原 200 万 DAU 单集群方案前提是"**一个逻辑集群**":一个 Redis Cluster、一组 MySQL ShardSet、一个 Kafka、一个 k8s。到 ~40 万 CCU 即触顶:

| 组件 | 现方案(单逻辑集群) | 为何到顶 |
|---|---|---|
| Redis | 6 主 6 从,固定 16384 slot | 在线键膨胀、gossip 协调 / 跨 slot 限制在高量级运维不可控 |
| MySQL | `player_id % N` 单组 ShardSet | 连接数 / 迁移 / 备份 / DDL 在高量级失控 |
| Kafka | 单集群 topic 分区 | 分区数 / ISR 复制 / controller 成瓶颈 |
| push | 6~8 实例定向路由 | 单注册表 + 单路由 Kafka 成中心瓶颈 |
| Agones | 单 k8s 集群 | etcd / kube-apiserver / Agones controller 撑不住 |

→ 解法:**Cell 化**(§3),复制单集群到 8~10 Cell 上 300 万 CCU。

### 2.2 第二道墙 —— 单一全局协调层(600 万 CCU 新增,本文重点)

Cell 本身能线性堆,但 1000 万版方案里的 **"单一全局协调层"** 不是无限线性的。Cell 数从 ~10 翻到 ~20 时,以下全局组件先扛不住:

| 全局组件 | 300 万 CCU(~10 Cell) | 600 万 CCU(~20 Cell)的新问题 |
|---|---|---|
| 全局 matchmaker | 单全局 MMR 池可撑 | 撮合 QPS / 跨 Cell 拉人翻倍,单全局池成中心点 → **必须按 region/段位再分片** |
| 全局消息总线(跨 Cell Kafka) | 单总线可用 | 跨 Cell N×N 扇出,Cell 数翻倍 → 边数 ~4×;全网广播不可行 → **收敛成区域总线** |
| social 全局 TiDB | 单 TiDB 集群可扛 | 好友图谱写放大,单 TiDB 写热点 → **按 region 分 TiDB 集群** |
| etcd `logical_cell→physical_cell` 映射 | 单 etcd 可放 | 路由读 QPS 高 → **本地缓存 + watch**,别每次路由打 etcd |

**共性**:全局协调面在 ~20 Cell 量级又变成"全局单点"。解法:**水平切分全局协调面本身** → 引入 Region。

---

## 3. 第一层骨架:单元化(Cell)

### 3.1 什么是 Cell

一个 **Cell = 一整套自洽的 infra + 服务实例**,容量对齐"一个集群":

```
Cell-k = {
  Redis Cluster(6 主 6 从)
  MySQL ShardSet(player_id % N)
  Kafka(本 Cell topic)
  k8s 集群(Agones Fleet:hub + battle)
  全套无状态 go 服务(login / player / matchmaker / team / trade / push / ...)
  独立 snowflake nodeID 命名空间
}
单 Cell 容量目标 ≈ 30~40 万 CCU(= 原 200 万 DAU 单集群方案的天花板)
```

- 600 万 CCU → **16~20 个 Cell**;留首日尖峰 + 故障冗余 → **部署 20~24 个 Cell**(分布在 3 个 Region,每 region ~5~6 Cell)。
- 每个 Cell 内部**就是原 200 万 DAU 单集群方案已设计好的那套**,几乎零新设计——价值在"把已验证的单集群当积木复制"。

### 3.2 玩家 → Cell 路由(确定性,不查表)

沿用"`player_id` 算落点"的同一思想,**上升一层**:

```
cell_id      = cell_route(player_id)          // 第 2 层:定 Cell
redis_slot   = CRC16(player_id) % 16384        // 第 3 层:Cell 内定 Redis 节点
mysql_shard  = player_id % N                   // 第 3 层:Cell 内定 MySQL 库
```

- **`cell_route` 用"逻辑 Cell + 映射表"而非裸取模**:`logical_cell = player_id % 4096`,再用一张 `logical_cell → physical_cell` 映射(存 etcd,全服务监听 + 本地缓存)。
  - 4096 逻辑分片 ≫ 24 物理 Cell,每 Cell 管 ~170 个逻辑分片;加 Cell 时只迁移部分区间,**不 rehash 全量**。
- **承接前几轮问答**:"怎么定位玩家在哪个 redis/mysql"的答案到这个量级多两层 —— **先算 Region、再算 Cell、再算 Cell 内分片,全程还是算、不是查**(查的只有小映射表,可全量缓存 + watch 更新)。

### 3.3 边缘接入层

- 边缘网关按**登录返回的 `region_id` + `cell_id`** 把后续连接(gRPC-Web ②、push stream)路由到该 Cell 的服务入口。
- login 做成**全局/区域薄层**:认证 → 算 `region_id`/`cell_id` → 返回接入地址 + ticket。
- DS 票据(hub/battle JWT)绑定 `region_id` + `cell_id`,防跨单元串号。

---

## 4. 第二层骨架:Region(大区)三层化(2000 万新增核心)

### 4.1 为什么要 Region 这一层

到 600 万 CCU,§2.2 的全局协调面触顶。引入 **Region** 把"全局"收敛成"区域内全局":

```
Region-r = {
  一组 Cell(例如 8~12 个 Cell,~240~360 万 CCU)
  区域全局协调层:
    region matchmaker(区域内全局 MMR 池,按段位分桶)
    region 消息总线(区域内跨 Cell Kafka)
    region social TiDB(区域内好友/聊天)
    region auction(可区域市场,或并入跨 region 全局市场,见 §4.3)
  区域 etcd(logical_cell→physical_cell 映射 + 路由缓存源)
}
600 万 CCU → **3 个 Region**(已拍板;每 region ~200 万 CCU / ~5~6 Cell,留冗余,可对齐地理)
```

- **同 Region 内**:Cell 间走区域消息总线,N×N 限制在单 region 的 Cell 数(~10),边数可控。
- **跨 Region**:玩家几乎不交互(匹配、好友、交易默认同 region 内);极少量跨 region 操作走**最小跨 region 通道**(§4.4),不做全网广播。

### 4.2 Region 路由

```
region_id    = region_route(player_id)        // 第 1 层:定 Region
cell_id      = cell_route(player_id)           // 第 2 层:定 Cell(region 内)
redis_slot   / mysql_shard                      // 第 3 层:Cell 内分片
```

- `region_route` 同样走"逻辑 region + 映射表"(逻辑 region 数固定,如 64,映射到物理 region),加 region 时只迁区间。
- **owner 不变量升级**:同一 `player_id` 的所有 owner 数据(档案/背包/段位/好友)必落**同一 `region_id` 同一 `cell_id`**;region 是 owner 边界的最外层。
- region 维度可与**地理(海外/国服)**对齐:既解决容量,又解决跨洋延迟。

### 4.3 全局协调层按 Region 分片

把 1000 万版的"单一全局层"按 region 拆开:

| 功能 | 1000 万版(单一全局) | 2000 万版(region 分片) |
|---|---|---|
| matchmaker | 单全局 MMR 池 | **每 region 一个 MMR 池**(同 region 玩家撮合,公平且低延迟) |
| 消息总线 | 单全局 Kafka | **每 region 区域总线**;跨 region 仅必要事件走全局桥 |
| social | 单全局 TiDB | **每 region 一个 TiDB 集群**;好友默认同 region |
| auction | 按 market_id 全局分片 | **【已拍板=方案②跨 region 全局市场】**:`auction` 服务 + `pandora_auction` 自成**跨 region 全局集群**,独立于 region/cell,按 `market_id` 全局分片,流动性最好;代价是挂单/成交是跨 region 写,须保持「每 market 单写者」串行 + 两层幂等(挂单 idempotency_key + 结算 match_id),与现有 `decision-revisit-auction-engine.md` 一致,只是部署拓扑提为全局 |

### 4.4 最小跨 Region 通道

**【已拍板=允许跨 region 匹配】**。跨 region 仍禁止强一致 owner 写,但放开匹配与社交/拍卖三类跨 region 交互,均异步、最终一致:

- **跨 region 匹配(已开放)**:matchmaker 升级为**两级撮合**——① 各 region MMR 池本地撮合(绝大多数对局同 region,低延迟);② 同段位长时间凑不齐时,溢出到**跨 region 撮合层**(全局 MMR 溢出池,key=段位桶)跨 region 拉人。对局在「参战玩家多数所在 region」的 Cell 拉起 battle DS;**结算仍回各自 owner cell**(match_id 幂等,不变量 §2/§6 不破)。跨 region 玩家承担稍高 RTT,撮合层须带 region 亲和度权重,优先同 region。
- **跨 region 加好友 / 私聊**:走全局桥(跨 region Kafka topic,key=接收方 player_id),投递对方 region 的 social/push。频率低,秒级延迟可接受。
- **跨 region 拍卖**:见 §4.3 方案②,auction 全局集群按 market_id 分片,所有 region 共享一个市场。

> 原则:owner 数据仍 region 内强一致;**匹配/社交/拍卖三类跨 region 交互走异步桥 + 全局溢出层**,优先同 region 亲和,跨 region 是兜底而非常态。

---

## 5. DS 编排:多 k8s 集群(按 Region × Cell)

- ~20 万 battle pod + ~8000 hub 远超单 k8s;**每 Cell 一个(或每 region 多个)k8s 集群**,各自 Agones controller。
- `ds_allocator` / `hub_allocator` 先按 region 再按 cell 选目标 k8s,**池化预热**在每 Cell 内独立成立。
- region matchmaker 选 battle DS 时,优先选**参战玩家多数所在 Cell** 的 k8s 拉起对局(同 region 内),几乎无跨 region 数据回流。
- 节点弹性:battle 计算型 / hub 常驻型节点池分离,cluster-autoscaler 按 GameServer 水位扩缩(每 Cell 独立)。

---

## 6. 不变量影响评估(对照 `CLAUDE.md` §9)

| 不变量 | 三层化后是否仍成立 | 说明 |
|---|---|---|
| 1 玩家只在一个 DS | ✅ | player_locator 升级为"region+cell 内权威 + 前缀",仍单点 |
| 2 战斗结果幂等 | ✅ | match_id 全局唯一(snowflake),结算回 owner cell 幂等落库 |
| 3 DS 票据短时效 | ✅ | JWT 增加 `region_id`+`cell_id` claim,exp 不变 |
| 4 DS 崩溃补偿 | ✅ | 每 Cell 内 allocator 心跳补偿独立成立 |
| 5 proto 字段不复用 | ✅ | 不涉及 |
| 6 MMR 在 battle_result | ✅ | 结算回各自 cell,MMR 写 owner cell;region MMR 池只撮合不持久化 |
| 9 kafka key=实体 ID | ✅ | Cell 内 + 区域总线 + 跨 region 桥均保持 key=实体 ID 有序 |
| 11 业务 ID uint64 | ✅ | `region_id` / `cell_id` 建议 `uint32`(拓扑维度,非 snowflake 业务 ID) |
| 14 客户端最小视图 | ✅ | region/cell 是部署维度,不外露内部拓扑 |

> **新增不变量候选**:
> ① 同一 `player_id` 的所有 owner 数据必落同一 `region_id` 同一 `cell_id`(跨 region/cell owner 写禁止);
> ② `logical_cell→physical_cell` / `logical_region→physical_region` 映射变更必须经迁移流程(双写 + 灰度),不可热改裸取模;
> ③ 跨 region 仅允许白名单内的弱实时事件(好友/私聊),禁止跨 region 强一致 owner 写。

---

## 7. 迁移路径(避免一步到位)

| 阶段 | CCU | 形态 | 动作 |
|---|---|---|---|
| 现状 | < 5 万 | 单实例 infra | 当前 dev/小规模,零改动 |
| 阶段 1 | ~40 万 | **单 Cell** | 落地单 Cell 四项扩容 + 压测验证一个 Cell 满载 |
| 阶段 2 | ~300 万 | **单 Region 多 Cell + 单一全局层** | Cell 切分 + cell_route + 全局 matchmaker/auction/social + 多 k8s |
| 阶段 3 | ~600 万 | **多 Region + 区域全局层** | 本文新增:region_route + 全局层按 region 分片 + 最小跨 region 桥 |
| 阶段 4 | 更高 | 自动扩缩 + 多地理 region | logical 映射平滑迁移自动化 + 跨地域部署 |

**纪律**:
- 阶段 1 不验证通过(单 Cell 真压到 ~40 万 CCU 且有对比表),**不进阶段 2**。
- 阶段 2 不验证通过(单 Region 多 Cell + 单一全局层真压到 ~300 万 CCU),**不进阶段 3 多 Region**。
- 对照 `stress-discipline.md`:每个阶段没有满载压测对比表,不许声明该阶段"可行"。

---

## 8. 决策结论(2026-06-26 人拍板)

| # | 决策点 | 结论 | 工程影响 |
|---|---|---|---|
| 1 | 单 Cell 容量目标 | **40 万 CCU** | 600 万 CCU → 取下限 ~16 Cell(冗余 20~24) |
| 2 | Region 数量 | **3 个** | 每 Region ~5~6 Cell;可与地理(国服/海外)对齐 |
| 3 | cell_route / region_route 分片数 | **采纳:Cell 4096 / Region 64** | 逻辑分片常量定死,`pkg/cellroute` 即按此实现 |
| 4 | 跨 region 匹配 | **允许**(见 §4.4 两级撮合) | matchmaker 加跨 region 溢出层 + region 亲和度,结算仍回 owner cell |
| 5 | auction 形态 | **方案② 跨 region 全局市场**(见 §4.3) | auction 自成全局集群,按 market_id 分片 |
| 6 | 推进节奏 | **一步到位**(目标按完整三层设计) | 不做单 region 半成品;但代码仍从地基增量落地,见 §7 阶段纪律 |

> 注:第 4 项放开跨 region 匹配后,「region 内/跨 region 撮合边界、溢出阈值、亲和度权重」细节单独出 `decision-revisit-global-matchmaker.md` 再细化,不阻塞地基层落地。

---

## 9. 落地优先级(人拍板后)

| 项 | 性质 | 依赖 |
|---|---|---|
| 单 Cell 满载压测(阶段 1 验收) | 压测 | 先于一切 |
| `cell_route` + etcd 映射表 + 边缘按 cell_id 路由 | 后端代码 + 基础设施 | 拍板 §8.1/§8.3 |
| 单 Region 全局协调层(matchmaker/auction/social) | 后端代码(新增全局服务) | 拍板 §8.4 |
| `region_route` + 全局层按 region 分片 + 跨 region 最小桥 | 后端代码 + 基础设施 | 拍板 §8.2,阶段 3 |
| 多 k8s + 每 Cell Agones | 基础设施 | Codex/人(§11.1) |

> 本文档仅设计。任何代码 / 部署改动等 §8 拍板后另起任务,按 `AGENTS.md` §4 直接执行流程推进,架构级再改写回 `pandora-arch.md` §11。
