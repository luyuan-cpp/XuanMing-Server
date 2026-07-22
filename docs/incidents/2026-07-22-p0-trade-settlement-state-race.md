# [INC-20260722-001][P0] Trade 结算与订单终态可被并发取消撕裂

> **状态**：修复实施中（代码 + 单测回归已落地，2026-07-22；真实 Redis 并发 / race / 故障注入 / E2E 未执行，未关闭）
> **类型**：`data` / `near-miss`
> **环境**：本机进程（上线前代码审计与定向回归；未确认在线上发生）
> **首次发生时间（UTC）**：未在线上发生；本地首次复现为 2026-07-22 04:46:22
> **首次发现时间（UTC）**：2026-07-22 04:46:22
> **负责人**：永久修复待最高可用 Claude 实现，项目负责人待指定
> **受影响服务/版本**：`services/economy/trade`，基线 commit `a138ff25d5fd56432b1fc2ad7d20d80c0f126b6b`；问题代码最早可追溯至 `bbb29911`
> **最后更新**：2026-07-22

## 0. 一句话结论

卖方确认时，Trade 在可重试的 Redis `WATCH` 回调里先完成 Inventory 资产结算，之后才用 `EXEC` 提交订单 `COMPLETED`。两者之间若发生并发取消、WATCH 冲突、Redis 故障或进程退出，就可能出现资产已经转移而权威订单仍为 `BUYER_CONFIRMED` 或 `CANCELED`；当前 HEAD 已能稳定本地复现，尚未修复、部署或完成玩家路径验证。

## 1. 影响与范围

- 玩家影响：潜在表现为买卖资产已发生变化，但订单显示取消或未完成；重试、补偿与客服判断会基于错误终态。
- 影响人数/对局/请求数：线上是否发生及请求数未知；本次只确认上线前本地复现，不得表述为线上事故。
- 服务影响：`trade` 的卖方确认结算路径；下游 `inventory` 已成功时，上游 Redis 订单状态可能提交失败。
- 数据与安全影响：订单权威状态与资产账本可永久不一致，属于资产数据完整性风险。
- 开始/结束时间：线上不适用；当前代码仍可复发，没有结束时间。
- 是否仍可复发：是。
- 严重级别判定理由：若上线触发会形成无法由普通重试自动闭合的资产错误，按 P0 `near-miss` 建档。

## 2. 第一现场与证据

### 2.1 症状

- 客户端症状：潜在显示订单已取消/未完成，但背包或金币已经结算；本次没有线上客户端样本。
- 服务端症状：`ConfirmOrder` 的下游结算成功一次，随后因 WATCH 冲突返回 `ErrTradeLockFailed`，权威订单可被并发 `CancelOrder` 提交为 `CANCELED`。
- K8s/Agones 状态：不适用，本次为本机上线前复现。

### 2.2 原始证据

代码证据：

- `services/economy/trade/internal/biz/trade.go:254` 把业务回调交给 `UpdateWithLock`。
- `services/economy/trade/internal/biz/trade.go:269-278` 在回调内部先调用 `u.ledger.Settle`，成功后才修改内存中的 `o.State`。
- `services/economy/trade/internal/data/trade_repo.go:162-188` 读取 WATCH 快照、执行回调，随后才通过 `TxPipelined/EXEC` 写 Redis。
- `services/economy/trade/internal/data/trade_repo.go:197-200` 对 WATCH 冲突重新执行整个回调。
- `services/economy/trade/internal/biz/trade.go:340-349` 允许任一方把任意非终态订单推进为 `CANCELED`。

本地临时定向回归的最小脱敏结果：

```text
初始订单：BUYER_CONFIRMED
并发序列：Confirm 进入 WATCH 回调 -> Inventory Settle 成功 -> Cancel 提交 CANCELED -> Confirm EXEC 冲突
观测：ledger.Settle 调用成功 1 次
Confirm 结果：ErrTradeLockFailed
最终权威订单：CANCELED
```

该临时用例用于修复前证据采集；由于正确断言在生产逻辑修复前必然失败，未把失败/Skip 用例留在工作树。永久回归仍是关闭前行动项。

### 2.3 已排除的噪声

- Inventory 幂等键使用 `order_id` 可以避免同一订单的重复资产结算，但不能把已成功的资产结算回滚，也不能保证 Trade 订单终态同步提交，因此不是本事故的原子性保护。
- 普通 Trade 单测通过只证明既有顺序路径，不覆盖结算成功到 Redis `EXEC` 之间的并发窗口。
- 本机未发现线上日志或玩家样本，不能把该 near-miss 叙述为已在线上发生。

## 3. 时间线

| UTC 时间 | 组件 | 事件 | 证据 |
|---|---|---|---|
| 2026-07-22 04:46:22 | trade 审计 | 确认结算副作用位于可重试 WATCH 回调内 | 当前 HEAD 源码与 `git blame` |
| 2026-07-22 04:46:22 | 本机定向回归 | 强制在 Settle 成功后并发 Cancel，稳定得到资产已结算、订单 CANCELED | 上述最小脱敏结果 |
| 2026-07-22 04:48:00 | 事故管理 | 建立 P0 near-miss 档案，状态保持“根因确认” | 本文档与事故索引 |

## 4. 调用链与关键变量

```text
TradeService.ConfirmOrder
  -> TradeUsecase.ConfirmOrder
  -> RedisTradeRepo.UpdateWithLock
  -> Redis WATCH 读取 BUYER_CONFIRMED 快照
  -> 回调调用 Inventory SettlePlayerTrade（外部资产副作用成功）
  -> 回调把内存订单改为 COMPLETED
  -> Redis EXEC（可能因 Cancel/其他写入而冲突或因故障失败）
  -> Confirm 返回失败，但外部资产副作用无法回滚
```

| 变量/对象 | 创建位置 | 所有者与生命周期 | 是否共享/可变 | 事故中的作用 |
|---|---|---|---|---|
| `order` WATCH 快照 | `trade_repo.go:170` | 单次 WATCH 尝试 | 本地可变，提交前非权威 | 回调把它改成 `COMPLETED`，但修改可能从未提交 |
| Redis 订单 key | `orderKey(orderID)` | 跨请求权威状态 | 多请求共享可变 | Cancel 可以在结算窗口先提交 `CANCELED` |
| Inventory 结算 | `trade.go:273` | 独立下游事务/账本 | 跨服务持久副作用 | 成功后不随 Redis WATCH 冲突回滚 |
| `order_id` 幂等键 | `trade.go:273` | 单订单稳定标识 | 跨重试复用 | 只防重复结算，不能耦合订单终态 |

## 5. 根因

### 5.1 直接根因

Trade 把不可回滚的跨服务资产结算放进“可能不提交、也可能被重新执行”的 Redis 乐观锁回调中，却没有先持久化单写结算意图、结算 fencing 状态或可恢复 outbox。资产事务与订单终态不在同一原子边界，代码却按单一事务使用。

### 5.2 触发条件

- 卖方确认 `BUYER_CONFIRMED` 订单，Inventory 结算成功。
- 在随后 Redis `EXEC` 之前，Cancel 或其他写入改变订单 key，造成 WATCH 冲突；或 Redis/网络/进程在该窗口失败。

### 5.3 故障放大因素

- `CancelOrder` 只检查“非终态”，没有识别“结算中/已产生结算意图”的 fencing 状态。
- `UpdateWithLock` 遇 WATCH 冲突会重跑整个回调，回调契约没有限制外部副作用。
- 上游收到失败后会合理重试，但重试无法把已被取消的订单自动校正为与资产账本一致的终态。

### 5.4 为什么现有保护没有挡住

- 幂等：`order_id` 防止重复扣发，但无法撤销已成功结算，也无法阻止 Cancel 写入冲突终态。
- 重试：乐观锁重试只重试 Redis 更新，不能回滚已提交的 Inventory 事务。
- 超时/Recovery：请求超时或进程 Recovery 只会让调用返回/进程存活，不能建立跨服务原子性。
- 补偿：当前没有以资产账本结果为权威的确定性对账与状态收敛路径。
- 多副本：共享 Redis WATCH 能发现并发写，但发现时间晚于外部副作用，反而暴露了该窗口。

## 6. 全仓同类问题扫描

- 扫描基线 commit：`a138ff25d5fd56432b1fc2ad7d20d80c0f126b6b`。
- 扫描目录和文件类型：本轮已审计 `services/economy/trade/**/*.go`；经济域其他服务只完成初步边界扫描。
- 搜索模式/工具：`rg` 搜索 `UpdateWithLock`、回调、`Settle`、`CancelOrder`，并用 `git blame` 追溯引入 commit。
- Confirmed 同型命中：当前确认 1 处，即本文 Trade 卖方确认结算路径。
- 结构性隐患：Inventory `FreezeForOrder` 的重复幂等键未校验原 payload；Bag journal 清理和堆叠溢出为独立问题，不并入本事故根因。
- 已排除项及理由：买方从 `PENDING` 到 `BUYER_CONFIRMED` 的回调只改 Redis 订单对象，没有跨服务不可回滚副作用。
- 未覆盖边界：全仓所有乐观锁/事务回调内下游 RPC、Kafka、文件或其他持久副作用仍需完成扫描；真实 Redis、多副本、网络分区与进程退出尚未注入。

## 7. 处置与永久修复

### 7.1 临时止血

| 动作 | 状态 | 证据 | 风险/回滚 |
|---|---|---|---|
| 建档并阻止把现有测试通过误报为可上线 | 已完成 | 本文档 | 不消除运行时风险 |
| 线上禁用/限流 Trade 卖方确认 | 未执行，需人决策且本次未触碰生产 | 无 | 会影响交易可用性 |

### 7.2 永久修复

| 项目 | 状态 | 代码/配置 | 验证 |
|---|---|---|---|
| 设计并实现可持久恢复的结算状态机/意图，确保只由持有 fencing 的单写者驱动下游结算 | **已实现(2026-07-22)**：复用 proto 既有 `ORDER_STATE_SELLER_CONFIRMED` 作结算意图态——卖方确认先经 WATCH/EXEC 原子提交意图(WATCH 回调内零外部副作用)，提交成功后锁外调 Settle(幂等键 order_id)，成功再 CAS→COMPLETED；瞬时失败/回包丢失停留意图态由任一方重试 Confirm 幂等驱动(多驱动安全：Settle 幂等 + 终态 CAS 幂等) | `services/economy/trade/internal/biz/trade.go` ConfirmOrder/driveSettlement | 单测回归 PASS；真实 Redis 并发与故障注入待执行 |
| Cancel 对结算中/已结算订单 fail-closed，并由稳定 `order_id` 收敛原操作 | **已实现(2026-07-22)**：CancelOrder / 惰性过期 / 配额清理对 SELLER_CONFIRMED 一律拒(ERR_TRADE_WRONG_STATE)，订单只向 COMPLETED/FAILED 收敛 | 同上 + proto 枚举注释(pb 待 Codex 重生) | 单测 PASS |
| 增加资产账本与订单状态对账/恢复路径 | **部分实现**：账本为权威的收敛路径已实现(结算成功后即使状态被异常改动也强制收敛 COMPLETED 并 Errorw 告警；`trade_mark_completed_failed`/`trade_settlement_inflight_retryable` 监控收敛积压)；独立运维对账工具仍待建 | 同上 | 重启/回包丢失/超时的单测矩阵 PASS；真实故障注入待执行 |
| 增加修复前失败、修复后通过的永久回归 | **已落地(2026-07-22)**：`TestConfirmOrder_ConcurrentCancelDuringSettlementCannotTear`(事故场景本体,修复前必失败) + 瞬时失败收敛/已入账回包丢失恰好一次/意图态不过期/不足置 FAILED 共 5 用例 | `trade_test.go` | `go test -count=1` PASS |

### 7.3 防复发规则

- 继续执行 `CLAUDE.md` §15 的最简单标准方案与 §16 的分布式边界审计。
- 新增代码审查规则候选：可重试的数据库/Redis 乐观锁回调不得直接执行不可回滚的外部副作用；需先落持久意图并用稳定幂等键、单调状态和 fencing 驱动。
- 规则正式写入 `CLAUDE.md`/设计文档前需由项目负责人审批；本次不擅自改架构决策。

## 8. 验证矩阵

| 验证 | 修复前结果 | 修复后结果 | 环境/命令 | 证据 |
|---|---|---|---|---|
| 针对性单测 | FAIL：资产结算成功 1 次、Confirm 锁冲突、订单最终 CANCELED | 未执行 | 本机临时定向回归 | §2.2 脱敏结果；永久用例待修复时落地 |
| 既有模块回归 | PASS | 未执行 | `go test -count=1 ./...`（trade 模块） | 只证明既有路径，不构成关闭证据 |
| 真实 Redis 并发集成 | 未执行 | 未执行 | 待建立可控 WATCH 冲突 | 阻断关闭 |
| `go test -race` | PASS（仅 `trade/internal/service` 本轮变更包） | 未执行 | 既有 `golang:1.26.5-bookworm` 镜像，源码只读挂载 | 未覆盖发生撕裂的 biz/Redis/Inventory 并发窗口，仍阻断关闭 |
| Redis/Inventory 超时与回包丢失 | 未执行 | 未执行 | 待故障注入 | 阻断关闭 |
| fatal/OOM/SIGKILL 重启注入 | 未执行 | 未执行 | 待结算窗口进程退出注入 | 阻断关闭 |
| 玩家 E2E | 未执行 | 未执行 | 待真实交易双方流程 | 阻断关闭 |

未执行、失败或受阻的验证均保留；当前状态不得提升为“已修复”或“已关闭”。

## 9. 部署、回滚与观察

- 修复 commit：无。
- 构建产物/镜像 digest：无。
- 部署时间与目标环境：未部署。
- 实际 Pod `imageID` / GameServer provenance：不适用。
- 回滚条件和步骤：尚无修复产物；当前生产是否启用该未提交/当前代码需另行核实。
- 观察窗口、指标与结果：未开始；后续至少监控结算幂等命中、订单/账本不一致、WATCH 冲突、恢复积压和人工对账结果。

## 10. 剩余风险与行动项

| ID | 严重级别 | 行动项 | 负责人 | 状态 | 目标/关联 Incident |
|---|---|---|---|---|---|
| A-001 | P0 | 实现持久结算意图/状态机、Cancel fencing 与确定性恢复 | 最高可用 Claude / 待指定 | 待处理 | 本 Incident |
| A-002 | P0 | 落永久并发回归，覆盖 Cancel、WATCH 重试、超时、回包丢失与进程退出 | 最高可用 Claude / Codex 执行环境验证 | 待处理 | 本 Incident |
| A-003 | P0 | 完成全仓“可重试回调内外部副作用”同类扫描 | 最高可用 Claude | 进行中 | 本 Incident |
| A-004 | P0 | 核实目标环境是否包含受影响版本及是否有历史不一致数据 | 人/运维 | 待处理 | 本 Incident |
| A-005 | P1 | 为 Inventory 重复幂等键增加 payload 指纹一致性验证 | 最高可用 Claude | 待处理 | 独立问题 |

## 11. 关闭审核

- [x] 直接根因和放大因素均有证据
- [ ] 修复前失败、修复后通过的回归存在
- [ ] race/集成/故障注入达到本事故风险要求
- [ ] 同类代码扫描完成
- [ ] 目标环境已加载可追溯的新产物
- [ ] 玩家路径、恢复和补偿路径验证通过
- [ ] 观察窗口无复发
- [ ] 剩余风险已解决或另建 Incident/任务
- [x] 文档已脱敏且时间线时区明确

**关闭结论与审批人**：未关闭；当前仅完成根因确认和上线前复现。
