# Player / Inventory / Auction 阻断修复：Claude Code 最终复审单（2026-07-12）

> 人已明确授权 Codex 实现本批业务修复。请 Claude Code 基于当前脏工作树独立读代码、构造反例并
> 审核，不要把本文当正确性证明。未 commit/push，未碰生产；工作树含其它并行改动，禁止 reset 或
> 覆盖非本批文件。

## 1. 本批完成范围

### Player 属性点

- repo 在 `SELECT ... FOR UPDATE` 事务内用宽类型 checked-add 校验请求总和和重复属性累计值；
  `unspent_attr_points`、单属性列越界及余额不足均整笔回滚。
- 真实 MySQL 用例覆盖提交、余额不足、列溢出、强制末步失败回滚，以及 10 worker 并发恰好 5 次成功。

### Inventory 拍卖托管

- 新增 `EnsureAuctionEscrow` proto/service/biz/repo：以不可变 order 快照验证已有 escrow；缺失时在
  MySQL 事务中原子补冻，重复键竞争回滚后重读并严格校验，不把 `1062` 直接当成功。
- 卖单绑定物品与剩余数量，买单 checked-multiply 后绑定金币；owner/side/item/price/数量漂移均
  fail-closed。真实 MySQL 覆盖并发、回滚和不一致快照。

### Auction 权威、幂等与 saga

- 撮合候选改为 MySQL 按 `market_id + item_config_id + side + escrow_verified` 权威选取，Redis 仅作
  有短超时的回滚兼容缓存；同物品价格/时间优先，异物品、缓存缺失或 Redis 写失败不影响正确性。
- `ClaimOrder` 以 owner coordinator registry 产生唯一 canonical order。legacy 跨片点查与 market 行
  补写均在 coordinator 事务外；事务只锁 owner guard 并读写 registry，消除两个单连接池互等环。
  registry-only 崩溃窗没有资产副作用，同 key 重试采用 canonical ID 补行，Freeze/Ensure 绝不能使用
  本次临时生成 ID。
- 每个 shard 持久保存 topology generation/count/index/脱敏 DSN identity；片数、顺序、目标库、重复
  物理库漂移均启动失败。当前只批准单库或恰好 2 片；首次双片 marker 需临时显式 bootstrap。
- 成交先在 MySQL 原子 Reserve match 与双方订单状态/`match_pending`，再以 `match_id` 调 inventory
  幂等结算；PENDING、release、legacy escrow 校验均有持久 marker、退避和每片有界扫描。
- Complete 与 `event_pending=1` 同一更新；独立事件 worker 以 `match_id` 至少一次发 Kafka，成功才
  条件清 marker，不占 market 锁，也不阻塞 Settle/Release worker。事件查询只拦双方**终态**订单的
  `release_pending=1`；OPEN/PARTIAL 的旧兼容 marker 不阻塞。资产 worker 每轮先释放既有 ready
  escrow，再处理慢恢复/结算。
- 弱依赖 order audit 只做有界非阻塞入队，单独 worker 发 Kafka；队列满告警丢弃。match producer
  重连构造不再持与 audit 状态读取共用的 mutex。
- 单玩家活跃订单有 Redis 原子 slot 上限和 MySQL 权威惰性清理；`ListMyOrders` 为 cursor + 每片 SQL
  LIMIT + 全局上限。幂等 key 限 1..64 个安全 ASCII 字符。
- `passive_warmup=true` 拒绝 Place/Bid/Cancel，并不启动 legacy verifier、资产/事件补偿或 expiry；
  旧实例全部停写退出后必须以 false 重启，才允许一次切写。

### Schema / migration

- docker-init 与 `pandora_auction/000002` 含 saga/outbox、owner coordinator、拓扑表及价格方向索引。
- `event_pending` 用一次 `ADD ... DEFAULT 1, ALGORITHM=INSTANT`：迁移中旧列集合 INSERT 不会漏登记；
  历史 COMPLETED match 也进入每片有界重放。消费者必须按 `match_id` 幂等，发布前评估 backlog 容量。
- 不使用 `LOCK TABLES`，不执行无界 UPDATE；同表缺失二级索引合并为一次 ALTER。

详细发布不变量见
[`decision-revisit-auction-match-authority.md`](decision-revisit-auction-match-authority.md) 与
[`release-checklist.md`](../ops/release-checklist.md)。

## 2. 请重点反证

1. registry 已存在但 market 行缺失时，返回值、Freeze/Ensure、响应是否全程使用 canonical ID；崩溃在
   registry commit、market insert、Freeze、Confirm 任一点能否重复冻结或留下不可恢复资产。
2. 两 owner 在相反 market shard、每池 `MaxOpenConns=1` 并发 Claim 是否仍可能形成跨池环；同 owner
   同 key 的同/不同指纹是否分别收敛/冲突。
3. shard `1↔2`、交换顺序、换 DB、重复 DSN、改 generation、部分 marker 缺失时能否绕过 topology gate。
4. old 与 green 混跑时，passive 门、R4 停写顺序和 legacy `escrow_verified=0` 恢复是否可能双 Reserve。
5. inventory 返回成功但响应丢失、Settle/Release 成功后 marker 清理失败、进程在每个事务边界退出时，
   是否都只产生可幂等重放；终态订单是否可能早于 PENDING match 被 Release。
6. Kafka broker 初始化或 Send 长时间阻塞时，market 锁、资产 worker、audit 调用者是否仍能继续；关闭
   audit worker 与 producer 的 defer 顺序是否安全，队列满是否确实非阻塞；终态 escrow marker 未清时
   match event 是否仍可能抢跑。
7. `000001→000002` 与 fresh init 的 column/default/comment/index direction/table/check 是否完全一致；
   历史行与迁移后旧列 INSERT 的 `event_pending` 是否都为 1，新 Reserve 是否显式为 0。
8. owner slot 在 Redis 丢失、含终态陈旧成员、某片不可读时是否分别重建/清理/fail-closed；列表是否有
   任何无 LIMIT 或无 cursor 的路径。
9. 属性点总和、重复 key、数据库列边界和并发扣点；Inventory escrow 的 1062、溢出与快照漂移反例。

## 3. 已执行验证

- player、inventory、auction：`go test -count=1 ./...` 与 `go vet ./...` 全绿。
- 设置仓外 `PANDORA_TEST_MYSQL_DSN` 后，三服务真实 MySQL 用例实际执行并通过（不是 Skip）。
- MySQL 8.4 fresh init 与 old `000001→000002` 完整 `information_schema` 一致；迁移首跑、幂等复跑均
  `version=2 dirty=false`；历史 match 与迁移后旧列 INSERT 均为 `event_pending=1`。
- `tools/migrate` 的 readonly test/vet/build、proto lint/breaking/Go+C++ 生成，以及 20 份集群配置的
  临时目录生成与 auction 安全字段断言均通过；临时输出已清理。
- 三路独立只读复审分别覆盖 Claim/topology、saga/Kafka/worker 与 schema/migration，最终均未再发现
  P0/P1；这不替代 Claude Code 的独立复核。

复现真实库测试时只从仓外注入临时 DSN，禁止把账号或密码写进仓库：

```powershell
$env:PANDORA_TEST_MYSQL_DSN = '<仓外临时 MySQL DSN>'
go test -count=1 ./...
go vet ./...
```

## 4. 尚需外部环境验收（不是已解决声明）

- 本机 `CGO_ENABLED=0`，未跑 `go test -race`，也未改系统环境。
- 未用真实 Kafka 做 broker 黑洞/恢复、历史 backlog 容量与消费者幂等故障注入。
- 未做 auction→inventory 真 gRPC 的“成功但响应丢失/重启/重复”端到端故障注入。
- 未在生产规模表上测索引 MDL、复制延迟、Claim p99，也未执行真实 R0-R4 蓝绿切流。
- `N>2` 重分片仍明确禁止；需 owner registry 全量回填、冲突审计和完成 marker 后另立决策。
- registry-only canonical 目前靠同 key 请求重试补 market 行；它没有资产副作用，但没有后台主动扫描。

Claude Code 若发现新的 P0/P1，请给出最小反例、精确文件/行和不变量破坏链；不要只报风格问题。
