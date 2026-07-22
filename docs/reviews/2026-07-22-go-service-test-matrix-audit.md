# Go 服务测试矩阵审计（2026-07-22）

> 审计对象：`F:\work\XuanMing-Server` 当前未提交工作树。
>
> Git 基线：`a138ff25d5fd56432b1fc2ad7d20d80c0f126b6b`（`main`）。
>
> 状态：20/20 个 Go 服务均新增了针对性回归，当前工作树的普通全模块测试、
> `go vet`、本次变更包 Linux race 及选定真实 MySQL/TiDB 集成均通过；但本轮同时确认
> 3 个未修复 P0 near-miss 和若干 P1 缺陷。本文不是上线批准，也不表示“所有可能状态均已穷举”。

## 1. 口径与结果

- 服务口径：递归扫描 `services/**/go.mod`，当前共 20 个独立 Go 服务模块。
- 本轮新增：31 个 `_test.go` 文件、58 个顶层 `Test*` 函数、24 个显式 `t.Run`
  调用点，共 3916 行；每个服务至少有 1 个本轮新增测试文件或测试场景。
- 当前服务测试总量：171 个 `_test.go` 文件、1276 个顶层 `Test*` 函数。
- “完整”按风险矩阵定义为：身份边界、非法状态、容量/溢出、幂等/回包丢失、并发/CAS、
  依赖失败、事务回滚、真实数据库语义及保留期；它不是对无限输入空间作不可证明的穷举承诺。
- 只保留验证正确契约的绿色回归。若定向反例证明生产逻辑错误，先保留脱敏证据并建档，
  不把错误行为固化成期望，也不留下必红或 `Skip` 冒充覆盖的测试。

## 2. 20 个服务新增覆盖矩阵

| 服务 | 本轮新增的关键边界 |
|---|---|
| `account/login` | Redis session JTI 轮换后，迟到 Logout 的 compare-delete 不能删除新 session。 |
| `account/player` | 所有玩家 RPC 的跨玩家身份与非法参数矩阵；满级经验 no-op 仍落稳定收据，同 key 普通重试及曲线扩容后迟到重试不重复入账（真实 MySQL）。 |
| `battle/battle_result` | 掉落溢出转邮件时回包丢失复用稳定幂等键；单玩家累计上限、stale watermark/CAS、outbox 零重复副作用（双独立真实 MySQL 连接）。 |
| `battle/ds_allocator` | 全部公开 RPC 的 nil/非法参数、业务错误到响应码映射、transport error 约定，以及 `ListBattles` 成功/不可用。 |
| `battle/hub_allocator` | 全部公开 RPC 边界及 `ListHubs` 映射；同时确认 locator UNKNOWN 仍 fail-open，已单独按 P0 建档。 |
| `data/data_service` | cache 读/写/删失败不能掩盖唯一权威仓储的读取结果或业务错误。 |
| `economy/auction` | 每个玩家 RPC 必须有 JWT 身份，取消操作拒绝 body/context 身份不一致。 |
| `economy/inventory` | 最后一格、背包已满、扩缩容、重复实例、实例指纹/槽位/账本边界；同一 journal 先合并旧堆叠、后因满包失败时，合并、新物品及水位必须整事务回滚（真实 MySQL）。 |
| `economy/trade` | 玩家身份与订单 ID 的 service 边界；同时确认结算外部副作用与 Redis 订单终态可被并发取消撕裂，已按 P0 建档。 |
| `matchmaking/matchmaker` | 队员已在 BATTLE、位置 UNKNOWN 时整队零副作用拒绝；重复与并发 `StartMatch` 收敛到同一 operation、唯一 live ticket。 |
| `matchmaking/team` | `MATCHING`/`IN_BATTLE` 状态下 `SetReady` 与换英雄拒绝且零 mutation；状态迁移未接线仍列为阻断项。 |
| `runtime/leaderboard` | limit/offset/mode 参数归一化；玩家只读、系统写身份隔离；结算重试返回 exact response。 |
| `runtime/owner` | usecase 参数、完整权威参数透传、repo 错误；service 身份/transport/冲突与屏障恢复信息；维护配置必须有界且修复非正值。 |
| `runtime/player_locator` | 依赖错误必须返回 UNKNOWN/UNAVAILABLE，不能伪装为 OFFLINE。 |
| `runtime/push` | 离线帧 `A,A,B` 重放保留重复次数与原始顺序。 |
| `social/chat` | 历史分页 limit 钳制与 cursor 原样透传。 |
| `social/dialogue` | 非法选项、过期 session 不得 mutation；所有 RPC 先鉴权，并阻止 body/context 跨玩家操作。 |
| `social/friend` | 收件箱已满时原 canonical pending 请求仍可幂等重试；真实 MySQL/TiDB 及随机测试库删除白名单。 |
| `social/guild` | 重复入会申请返回 canonical 请求、公会满员、分页/权限、全 RPC 鉴权与零 ID。 |
| `social/mail` | 背包满/发放失败时不 claim 且可重试；分页上下界、非法窗口精确错误、玩家 RPC 鉴权。 |

## 3. 验证证据

| 验证 | 结果 | 环境/说明 |
|---|---|---|
| 20 个服务逐模块 `go test -count=1 ./...` | 20/20 PASS | Go `1.26.5 windows/amd64`；最终工作树状态下复跑。 |
| 20 个服务逐模块 `go vet ./...` | 20/20 PASS | 与上述最终工作树同一轮执行。 |
| `go test -race` | 26/26 本轮变更包 PASS | 既有 `golang:1.26.5-bookworm` 镜像，源码只读挂载，`GOFLAGS=-mod=readonly`；覆盖 20 个服务，耗时 342.3 秒。代理最终落盘后又对 Battle Result biz/data、Player data、DS/Hub service、Mail biz 共 6 个包复跑，175.5 秒全部 PASS。宿主因 `CGO_ENABLED=0` 且无 `gcc` 不能直接运行 race，未安装工具或修改环境。 |
| 匹配并发重复 | PASS ×500 | `GOMAXPROCS=1`，验证测试自身 barrier 后无调度假设。 |
| Owner service、Push replay 等高频 | PASS ×100～200 | 覆盖错误映射、重复顺序与 RPC 矩阵稳定性。 |
| MySQL | PASS | 本地 `mysql:8.4`；覆盖 Owner CAS、Inventory Bag/冻结/保留期/事务回滚、Auction、Battle retention/progress CAS、Player 属性/经验、Guild、Friend。 |
| TiDB | PASS | 本地 `pingcap/tidb:v8.5.1`；覆盖 Guild 容量/计数/dedup/schema 与 Friend canonical pending retry。 |
| 临时测试库清理 | PASS | MySQL 未遗留符合 `_it_` 随机测试库命名的 schema。 |
| `gofmt` / `git diff --check` | PASS | 本轮新增测试、事故文档及定向修改无 whitespace error。 |

说明：race 只证明被执行包中的 Go 内存访问未被检测到数据竞争，不证明 Redis/MySQL 跨服务原子性、
脑裂门禁或玩家 E2E；真实数据库测试也只覆盖表中列出的确定场景。

## 4. 本轮确认且仍未修复的 P0

| Incident | 结论 | 当前状态 |
|---|---|---|
| [INC-20260722-001](../incidents/2026-07-22-p0-trade-settlement-state-race.md) | Trade 在可重试 Redis WATCH 回调内先执行不可回滚的 Inventory 结算，随后订单 `EXEC` 可被并发 Cancel 打断，形成资产已结算而订单 CANCELED。 | 根因确认，未修复、未关闭。 |
| [INC-20260722-002](../incidents/2026-07-22-p0-hub-allocator-locator-fail-open.md) | Hub 切线把 locator error/non-OK/key miss 当可放行，而 Owner Authority 尚未全链路接线，不能排除双 DS 归属。 | 根因确认，未修复、未关闭。 |
| [INC-20260722-003](../incidents/2026-07-22-p0-inventory-bag-journal-sweep.md) | Bag journal sweep 只按时间删除，可越过 `covered_journal_seq`，删除 checkpoint 未覆盖的唯一恢复尾部。 | 根因确认，未修复、未关闭。 |

另外，[INC-20260721-001](../incidents/2026-07-21-p0-ds-allocator-heartbeat-context-race.md)
仍处根因确认状态；本轮普通测试或局部 race 不能替代其故障注入、真实部署产物、玩家路径与观察窗口关闭门。

## 5. 定向反例确认的 P1/实现缺口

以下项目不能靠把当前错误行为写成绿色断言“解决”。临时复现文件均已删除，工作树未留下必红/Skip 用例：

1. Friend 硬 pending 上限在真实并发下不成立：`max=3`、16 个不同 requester 时，MySQL 出现
   `Error 1213 Deadlock found when trying to get lock` 且未转换为可重试业务结果；TiDB 一轮得到
   `rows=15, succeeded=15, max=3`。`COUNT(*) ... FOR UPDATE` 既会在 InnoDB 死锁，也不能依赖
   TiDB gap lock 保证上限。
2. Team 的 `READY -> MATCHING -> IN_BATTLE` 状态迁移尚未接线；`Invite/AcceptInvite/LeaveTeam/Kick`
   只拒绝 `DISBANDED`，活动匹配/战斗队伍仍可能被修改。
3. Mail `limit=100` 时把 101 条 system 与 101 条 guild 合并后可返回 202 条，跨 channel 总量无硬上限；
   重复 claim 当前返回附件加 `ERR_MAIL_ALREADY_CLAIMED`，与 proto 的“返回首次结果”幂等契约不一致；
   `mail_id=0` 的 Read/Delete/Claim 错误语义也不统一。
4. Bag 可堆叠数量使用 `uint32` 加法，`MaxUint32 + 1` 可回绕到 0；必须用溢出前检查修复并留永久回归。
5. Inventory `FreezeForOrder` 对相同 `(player_id, order_id)` 的重试未校验原 payload 指纹，错误重放可被当作幂等成功。
6. Dialogue `Update` 没有 version/CAS，并发双选择可同时基于同一旧节点成功。
7. Leaderboard 尚未统一拒绝 `entity_id=0`、限制 `top_n`、防 `offset+limit` 溢出，并需决定未知 enum 的 fail-closed 契约。
8. Chat cursor 只有 `send_time_ms`；同一毫秒多条消息跨页时缺少稳定第二排序键，存在漏读边界。
9. Matchmaker 对已配置 locator 的 RPC error 已 fail-closed，但 key miss/残缺 presence 仍不能替代 Owner Authority；
   必须随全链路 owner 接线修复。
10. Owner service 对非屏障 repo 错误仍直接转发 `retry_after`；协议约定只允许屏障等待携带该值，应在 service 映射层收窄。

## 6. 变更边界与交接

- 本轮主体为测试：新增 31 个测试文件，并定向修正 1 个既有真实 MySQL 锁等待观察器，避免
  `information_schema.INNODB_TRX.trx_query` 的采样假阴性。
- 为使当前 proto 工作树可编译，运行了 Go-only `tools/scripts/proto_gen.ps1`；同步了当前 proto 对应的
  Go generated 文件，没有生成/覆盖 C++。既有 C++、proto、业务实现及大量其他工作树修改均按原样保留。
- `services/runtime/owner/cmd/owner/main.go` 只做了 Kratos `log.Helper` import alias 的编译修正，
  没有改变业务逻辑。
- 新增 3 份 P0 near-miss 文档并登记 `docs/incidents/index.md`。P0 状态保持“根因确认”。
- 未执行 `git add`、`git commit`、`git push`、部署、镜像推送或生产操作；进入任务前已有的大量本地修改
  和未跟踪文件均未还原、清理或覆盖。

## 7. 下一验收门

1. 由最高可用 Claude 先修复 3 个 P0，并在修复提交中落“修复前失败、修复后通过”的永久并发/真实数据库回归。
2. 修复 Friend/Team/Mail/Bag 等 P1 后，把上述临时反例转成强制绿色回归；不得通过放宽断言或 `Skip` 绕过。
3. 完成 Owner Authority 在 login/allocator/DS/battle_result 的全链路接线，再做旧 DS 分区恢复、Admission ACK
   丢失、迟到 Logout/Heartbeat、Stable/Canary 混跑和玩家 E2E。
4. 同一最终快照复跑 20 个模块普通测试、全量相关包 race、MySQL/TiDB 集成、故障注入与目标产物 provenance；
   这些门全部满足前，不能把 P0 置为关闭或把当前工作树称为可上线。
