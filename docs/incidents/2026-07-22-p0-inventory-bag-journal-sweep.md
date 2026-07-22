# [INC-20260722-003][P0] Bag journal 清理可删除 checkpoint 未覆盖的恢复尾部

> **状态**：修复实施中（覆盖门 + LoadBag 尾部校验 + 集成测试已落地，2026-07-22；真 MySQL 执行与并发注入待环境，未关闭）
> **类型**：`data` / `near-miss`
> **环境**：本机未提交工作树代码审计（未部署、未确认在线上发生）
> **首次发生时间（UTC）**：未在线上发生；本地首次确认 2026-07-22 04:54:39
> **首次发现时间（UTC）**：2026-07-22 04:54:39
> **负责人**：永久修复待最高可用 Claude 实现，项目负责人待指定
> **受影响服务/版本**：`services/economy/inventory` 未提交 Bag 实现；Git HEAD `a138ff25d5fd56432b1fc2ad7d20d80c0f126b6b` 不包含该未跟踪文件
> **最后更新**：2026-07-22

## 0. 一句话结论

Bag 恢复依赖“checkpoint + `covered_journal_seq` 之后的 journal 尾部”，但当前未提交实现的 `SweepJournal` 只按 `created_at` 删除，没有要求待删行已被该玩家 checkpoint 覆盖。玩家长时间未产生新 checkpoint 时，90 天清理可物理删除唯一恢复尾部，之后加载会静默少物品；这是上线前发现的数据丢失 near-miss，尚未修复。

## 1. 影响与范围

- 玩家影响：久未登录/旧 owner 未成功落最终 checkpoint 的玩家，再次 checkout 时可能少恢复一段已确认 journal，表现为物品、转移或领取结果丢失。
- 影响人数/对局/请求数：当前实现未提交且未确认部署，线上影响为 0/未知；不能表述为已发生线上丢物。
- 服务影响：inventory 承载的 Bag 后台 retention sweep 与 `LoadBag` 恢复链。
- 数据与安全影响：已 ACK 的同步 journal 可能被提前物理删除，破坏“零丢失”和可重放保证。
- 开始/结束时间：仅存在于当前未提交工作树；仍可复发。
- 是否仍可复发：是，只要清理阈值覆盖到 checkpoint 之后的尾部行。
- 严重级别判定理由：若该实现上线，会把已确认资产事件从唯一恢复源删除，按 P0 数据 near-miss 建档。

## 2. 第一现场与证据

### 2.1 症状

- 客户端症状：潜在在重新登录/owner 切换恢复后少物品；本次没有客户端样本。
- 服务端症状：sweep 返回成功和删除行数，不会识别被删行是否仍是恢复必需尾部；后续 LoadBag 只会得到一个有缺口的尾部结果。
- K8s/Agones 状态：不适用；未部署、未触碰集群。

### 2.2 原始证据

```text
bag_repo.go:140-149
  LoadBag 读取 checkpoint.covered_journal_seq，随后查询 journal_seq > coveredSeq 的全部尾部。

bag_repo.go:368-388
  SaveCheckpoint 单调维护 covered_journal_seq。

bag_repo.go:434-445
  DELETE FROM bag_journal
  WHERE created_at < (NOW() - INTERVAL ? SECOND)
  LIMIT ?
```

清理 SQL 没有与 `bag_checkpoint` 联结，也没有 `journal_seq <= covered_journal_seq` 条件；因此以下最小状态必然被错误删除：

```text
player checkpoint covered_journal_seq = 10
bag_journal row: journal_seq = 11, created_at 早于 retention cutoff
SweepJournal: 删除 seq=11
LoadBag: checkpoint 只覆盖到 10，且尾部 11 已不存在
结果: 已确认的 seq=11 无恢复来源
```

`docs/design/bag-domain.md:112-114` 明确恢复必须重放 checkpoint 之后的完整有序尾部，冲突时应 fail-closed，不能静默丢条目。

### 2.3 已排除的噪声

- 90 天 retention 本身符合只增表有界要求；问题不是“是否清理”，而是清理资格没有与 checkpoint 覆盖水位绑定。
- `last_journal_seq` 只记录权威写水位，不能重建已物理删除的 payload。
- journal 的幂等键/指纹用于重复写防护，不能恢复已删除的唯一事件内容。
- 当前 Bag 文件未跟踪且没有部署证据，所以本事故是 near-miss，不是线上丢物声明。

## 3. 时间线

| UTC 时间 | 组件 | 事件 | 证据 |
|---|---|---|---|
| 2026-07-21 | inventory/bag | Bag 设计与未提交实现进入工作树 | 设计文件、当前工作树状态 |
| 2026-07-22 04:54:39 | 测试审计 | 对照恢复水位与 sweep SQL，确认未覆盖尾部可被删除 | 源码与设计交叉检查 |
| 2026-07-22 04:56:00 | 事故管理 | 建立 P0 near-miss 档案 | 本文档与索引 |

## 4. 调用链与关键变量

```text
已确认 Bag journal append(seq=N)
  -> checkpoint 仍只覆盖到 N-1（崩溃、离线、迁移失败或尚未到快照时点）
  -> RetentionSweep.Run
  -> MySQLBagRepo.SweepJournal
  -> 仅按 created_at 删除 seq=N
  -> 下次 LoadBag 读取 checkpoint(N-1) + 查询尾部
  -> seq=N 不存在，恢复静默缺项
```

| 变量/对象 | 创建位置 | 所有者与生命周期 | 是否共享/可变 | 事故中的作用 |
|---|---|---|---|---|
| `last_journal_seq` | `bag_meta` | 单玩家写水位 | 单调可变 | 证明曾确认到 N，但不含恢复 payload |
| `covered_journal_seq` | `bag_checkpoint` | 快照覆盖水位 | 单调可变 | 只有 `seq <= covered` 的 journal 才具备清理资格 |
| journal payload | `bag_journal` | 直到被 checkpoint 覆盖前是唯一恢复尾部 | 只增后清理 | 当前 SQL 可提前删除它 |
| retention cutoff | sweep 配置 | 全局时间阈值 | 周期变化 | 不能单独决定单玩家 journal 是否安全可删 |

## 5. 根因

### 5.1 直接根因

清理实现只满足“按时间有界删除”的容量目标，没有同时实现恢复正确性的必要条件：每条待删 journal 必须已经被同一玩家的 checkpoint 水位覆盖。Retention 与 recovery watermark 被当作互不相关的机制，导致物理删除越过恢复安全点。

### 5.2 触发条件

- 某玩家存在早于 retention cutoff、但 `journal_seq > covered_journal_seq` 的已确认 journal。
- 后台 sweep 扫描到该行并删除。
- 玩家随后从旧 checkpoint 恢复。

### 5.3 故障放大因素

- SQL 是跨玩家批量 `LIMIT`，没有玩家级水位条件，任何落后 checkpoint 都可能中招。
- LoadBag 查询尾部没有连续性/缺口校验，删除后可能静默恢复，而不是 fail-closed 报警。
- 当前测试只有 sweep 参数传递/循环和 Bag happy path，缺少真实 MySQL 的“未覆盖不可删、已覆盖可删”矩阵。

### 5.4 为什么现有保护没有挡住

- owner_epoch fencing：只拒绝旧 owner 写，不能保护 retention 删除。
- checkpoint 单调性：防水位回退，但 sweep 没读取该水位。
- 幂等与序号唯一键：防重复/乱序插入，不能防合法行被提前清理。
- 90 天保留期：只降低触发频率，不构成恢复安全证明。

## 6. 全仓同类问题扫描

- 扫描基线：Git HEAD `a138ff25d5fd56432b1fc2ad7d20d80c0f126b6b` + 当前未提交 Bag 工作树。
- 扫描目录和文件类型：`services/economy/inventory/internal/{biz,data}`、`deploy/mysql-init/14-bag-tables.sql`、`docs/design/bag-domain.md`。
- 搜索模式/工具：`rg` 搜索 `covered_journal_seq`、`journal_seq`、`SweepJournal`、`retention`、`checkpoint`。
- Confirmed 同型命中：当前确认 `MySQLBagRepo.SweepJournal` 一处。
- 结构性隐患：LoadBag 尚缺 journal 序号连续性检查；纯幂等回放与小时额度顺序、堆叠 `uint32` 溢出是独立问题。
- 已排除项及理由：后端驻留 `bag_section` 当前 sweep 不走此 SQL；checkpoint/section 为覆盖行，不属于本 journal 删除路径。
- 未覆盖边界：真实 MySQL 多玩家批次、并发 SaveCheckpoint/SweepJournal、崩溃与事务隔离级别尚未验证。

## 7. 处置与永久修复

### 7.1 临时止血

| 动作 | 状态 | 证据 | 风险/回滚 |
|---|---|---|---|
| 阻止把当前未提交 Bag 实现作为可上线完成态 | 已完成 | 本文档 | 不改变代码 |
| 若目标环境误含该实现，暂停 Bag journal sweep | 未执行，需人核实和授权 | 无部署证据 | 会增加表增长，需容量监控 |

### 7.2 永久修复

| 项目 | 状态 | 代码/配置 | 验证 |
|---|---|---|---|
| 清理只删除同时满足 retention 与 `journal_seq <= covered_journal_seq` 的行 | **已实现(2026-07-22)**：SweepJournal 改为派生表选主键 + `JOIN bag_checkpoint` 条件删；无 checkpoint 玩家 INNER JOIN 不命中,任何流水不删 | `bag_repo.go` SweepJournal | `SweepRespectsCheckpointCoverage` 用例已落地(含 batch=1 排空);本机无 MySQL DSN 未执行,待环境跑 |
| 定义 SaveCheckpoint 与 sweep 并发时的锁/事务边界，保证不越过线性观察到的覆盖水位 | **已实现(2026-07-22,免锁论证)**：covered_journal_seq 由 SaveCheckpoint 强制单调(回退拒),sweep 子查询一致性读读到旧值只会**少删**(安全方向),无需额外锁;论证写入代码注释 | `bag_repo.go` SweepJournal 注释 + SaveCheckpoint 既有单调 CAS | 单调性既有用例 PASS;并发注入待环境 |
| LoadBag 校验 `(covered, last]` 尾部连续，缺口 fail-closed 并告警 | **已实现(2026-07-22)**：尾部逐 seq 连续校验 + 末端水位对齐校验,缺口/截断拒绝加载(ErrInternal 带完整上下文,绝不静默少资产) | `bag_repo.go` LoadBag | `LoadBagTailGapFailsClosed` 用例已落地(挖中间/挖末尾两路);待环境跑 |
| 将清理资格和必要索引同步到 Bag 设计及 dbcheck 门禁 | **已同步(2026-07-22)**：bag-domain.md 表说明 + 14-bag-tables.sql idx_created_at 注释补覆盖条件;dbcheck 登记本已含 checkpoint 条件说明 | `docs/design/bag-domain.md` / `deploy/mysql-init/14-bag-tables.sql` / `tools/migrate/cmd/dbcheck` | schema 门禁既有 |

### 7.3 防复发规则

- 继续执行 `CLAUDE.md` §9.24：只增表必须有界，但清理不能破坏幂等重试/恢复窗口。
- 对所有 snapshot + journal 模型新增统一审核项：retention 删除资格必须由 snapshot 覆盖水位证明，时间阈值只能作为附加条件。

## 8. 验证矩阵

| 验证 | 修复前结果 | 修复后结果 | 环境/命令 | 证据 |
|---|---|---|---|---|
| 静态最小反例 | FAIL：SQL 必然删除未覆盖的过期行 | 未执行 | 当前源码/SQL 推演 | §2.2 |
| Bag 容量边界单测 | PASS：最后一格、已满、实例满格、扩缩容 | 不适用 | inventory data 新增测试 | 不覆盖恢复清理 |
| 既有 inventory 模块回归 | PASS | 未执行 | `go test -count=1 ./...` | 真 MySQL Bag 用例默认可跳过 |
| 真 MySQL 未覆盖/已覆盖清理矩阵 | 未执行 | 未执行 | 待永久回归 | 阻断关闭 |
| SaveCheckpoint/Sweep 并发 | 未执行 | 未执行 | 待并发集成 | 阻断关闭 |
| `go test -race` | PASS（`inventory/internal/data` 本轮变更包） | 未执行 | 既有 `golang:1.26.5-bookworm` 镜像，源码只读挂载 | race 不证明 checkpoint/sweep 的跨事务安全，仍阻断关闭 |
| 崩溃/重启恢复与玩家 E2E | 未执行 | 未执行 | 待故障注入 | 阻断关闭 |

## 9. 部署、回滚与观察

- 修复 commit：无。
- 构建产物/镜像 digest：无；当前问题文件未跟踪。
- 部署时间与目标环境：未部署，目标环境是否包含等价实现待人核实。
- 实际 Pod `imageID` / GameServer provenance：未核实。
- 回滚条件和步骤：若误部署，优先由人暂停 sweep 并保留表数据，再评估回滚；本次未执行环境变更。
- 观察窗口、指标与结果：未开始；后续需监控 checkpoint lag、未覆盖且接近 retention 的 journal 数、序号缺口和 sweep 行数。

## 10. 剩余风险与行动项

| ID | 严重级别 | 行动项 | 负责人 | 状态 | 目标/关联 Incident |
|---|---|---|---|---|---|
| A-001 | P0 | 实现基于 checkpoint 覆盖水位的安全清理 SQL/事务 | 最高可用 Claude | 待处理 | 本 Incident |
| A-002 | P0 | 增加真 MySQL 未覆盖不可删、已覆盖可删和并发 checkpoint/sweep 回归 | 最高可用 Claude / Codex 环境执行 | 待处理 | 本 Incident |
| A-003 | P0 | 增加 LoadBag 尾部连续性 fail-closed 检查与缺口告警 | 最高可用 Claude | 待处理 | 本 Incident |
| A-004 | P0 | 核实目标环境/离线镜像是否包含该未提交实现 | 人/运维 | 待处理 | 本 Incident |
| A-005 | P1 | 修复可堆叠数量 `uint32` 加法溢出并补边界回归 | 最高可用 Claude | 待处理 | 独立缺陷 |

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

**关闭结论与审批人**：未关闭；当前仅完成上线前根因确认，生产修复与验证均未开始。
