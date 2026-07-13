# 全代码审核阻断修复交接(Claude Code,2026-07-12)

> **状态更新**：本文是修复前快照，其中“Codex 未改业务”及 P0-1/P0-2 未修的描述已过期。
> 人已明确授权 Codex 修复本批 player/inventory/auction 阻断；实现与最终独立复审入口见
> [`auction-blockers-claude-review-20260712-final.md`](auction-blockers-claude-review-20260712-final.md)。
> 本文其余跨域队列仍保留作历史审计范围，不能据此宣称全仓其它阻断已完成。

> 以下是**修复前的历史交接原文**：当时 Codex 只改部署/运维防线，尚未修改 player / auction 等
> 业务逻辑。仅用于追溯最初反例；当前状态以顶部链接的最终复审单为准。所有结论基于当前脏工作树；先重新
> `git diff` 并保留 protobuf unknown fields 与不停服兼容性。

## 1. 当前最高优先级

### P0-1:属性分配总和可 `int32` 溢出

- 入口:`services/account/player/internal/data/attribute_repo.go` 的 `AllocateAttributePoints`。
- 现状:`var sum int32; sum += a.Points`;多个正数可绕回负数,从而通过 `sum > unspent`,随后
  `newUnspent := unspent - int(sum)` 反向增加点数并执行每条 upsert。
- 修复要求:在事务内用不溢出的宽类型逐项 checked-add;校验总和、单属性累计值、
  `unspent_attr_points` 与数据库列上界;越界返回明确业务错误且零写入。不能只在 service 层限制数组,
  repo 必须自守。
- 必测:两个 `MaxInt32` 正数、总和刚好上界/上界+1、重复 attr key 导致单列溢出、并发分配、
  失败后 players/player_attributes 均不变。

### P0-2:拍卖撮合没有校验双方 `item_config_id`

- 入口:`services/economy/auction/internal/biz/auction.go` 的 `match` / `buildMatch`。
- 现状:订单簿只按 `market_id + side + price` 取 best;取出 resting 后未检查
  `resting.ItemConfigID == incoming.ItemConfigID`,而 `buildMatch` 直接采用 incoming 的物品 ID。
  异物品对手单可被当成同物品结算,造成资产错误转移。
- 修复要求:撮合索引/权威查询必须把 item 纳入市场键或在取单后 fail-closed 隔离不匹配条目;
  不能简单 remove 后永久丢单。结算请求必须绑定两张订单各自不可变快照并由 inventory 再校验。
- 必测:同 market/同价不同物品绝不成交且两单仍可见;重启重建订单簿后仍成立;恶意污染簿条目、
  并发撮合、结算重试/幂等均不转错资产。

### P0-3:online 镜像/五 writer digest 闭环(代码已落地,待 Claude 复核)

- `tools/scripts/start.ps1`、`tools/scripts/lib/online_manifest_contract.ps1` 已实现不可变 tag、
  registry digest 双向校验、20 Deployment pin、五 writer Pod annotation、Fleet 两层 annotation、
  Ready 池 imageID 回读;旧 Allocated DS 仅排空。
- 独立反证后又补:BuildPush clean tree + strict rebuild + provenance,禁止离线旧镜像冒充 HEAD;
  kubectl client 结构化检查目标主容器,initContainer digest 诱饵不能蒙混;Fleet 同时核对总容量、
  Ready/Allocated/Reserved 稳态与至少一个新 Ready 池。
- `tools/scripts/tests/online_manifest_contract_test.ps1` 有 mutable/缺 annotation/Fleet 少一层/
  signing secret/混合鉴权错误/initContainer 诱饵等反例。
- 尚未真 registry/k8s 验证;离线 tar digest 等价性未证明。
- registry HEAD 预查存在 TOCTOU;BuildPush 已再加硬门,目标平台验证 native immutable-tag/
  create-only + 发布锁之前不会 build/push。

### P0-4:DSTicket 生产验票材料未决

- 不可信 Fleet 永久禁止注入玩家 HS256 secret/私钥。必须由人选择并由 Claude+UE 完整实现:
  B=revisioned 公钥 keyset 离线验签;C=只走 online authority。
- `start.ps1` 当前在任何 registry/k8s 写入前无条件 fail-closed;这不是可用运维 ACK 绕过的开关。

## 2. P1 修复队列(按依赖顺序)

1. **DS auth 激活/回退**:`pkg/dsauthfence/activate.go` 的 baseline 只检查 required key 缺失,
   epoch 2 后误删 key 可重建成 1。需 genesis 永久审计记录、锁 token + required/history 的单 Txn、
   删除后拒绝回退;五 writer 在激活窗口不得与 legacy writer 混写。生产还须让五 writer 与
   `dsauth-required` 共享 custom CA/mTLS/ACL 最小权限能力;当前 prod 在读取 required 前硬阻断。
2. **Hub/Battle 分配权威**:Hub reservation 不能被心跳总数覆盖;assignment/UID 要持久迁移;
   locator unknown 不能再分第二个 Hub;`allocation_uncertain` 需要权威 reconciler,不能 sweep 猜测;
   Battle terminal release 需要与结果同事务的 outbox。
3. **交易/拍卖 saga**:逐项复核“外部资产已转移、订单/事件落库失败”和 best-effort release 窗口;
   必须有持久 intent/outbox、幂等状态机与可审计补偿,不能只记日志后返回。
4. **奖励领取**:`ClaimReward` 相关 player/rewardclaim/调用方要保证业务条件校验、领取标记与资产发放
   原子或可恢复;客户端不能直接构造系统奖励来源/索引绕过上游条件。
5. **系统 RPC 身份**:player、inventory、mail、leaderboard、battle-result 等后端写 RPC 不能仅靠注释/
   player_id 参数区分系统调用;统一核对内网 service identity/scope,客户端 JWT 必须被拒绝。
6. **Team/Matchmaker**:Team 加入/踢人/队长变更/入队匹配状态要在同一权威事务或 CAS 中校验;
   禁止绕过队伍容量/状态门,取消匹配不能留下半状态。
7. **受管列表上限与分页**:friend 请求/好友、mail、push offline、chat、dialogue、leaderboard、
   matchmaker 队列等逐项对照 `CLAUDE.md §9` 不变量 18;写入事务/原子路径要有总量上限,
   读取必须 cursor/SQL LIMIT,TiDB 路线也不能以“以后扩容”替代当前硬门。
8. **protobuf/缓存兼容**:所有 Redis pb read-modify-write 路径必须保留 unknown fields;禁止改变已存字段
   编号/类型/语义。新增存储版本只能双读/先读旧写新并满足不停服滚动。

## 3. Claude Code 审核顺序与完成定义

1. 先独立复现 P0-1/P0-2,补失败测试后修;不要以当前文档结论代替代码审查。
2. 复核本轮 ops diff,尤其确认 `start.ps1` 的硬门位于第一次远端写之前、任何错误均 fail-closed、
   Fleet 没有玩家 signing material。
3. DSTicket B/C 属会改变 UE/后端拓扑的决策;未获人选择前只审核,不要擅自落其中一案。
4. P1 每组完成时补服务级设计与回归;跨服务 saga 必须跑故障注入(超时、成功但响应丢失、重启、重复)。
5. 完成定义:相关模块 test/vet/build 全绿,proto/buf 与 PowerShell/Kustomize 静态检查通过,
   没有 TODO/空实现;真实 registry/k8s/UE DS 验收未跑时必须明确保留阻断。

## 4. Codex 本轮验证

- `pwsh -NoProfile -File tools/scripts/tests/online_manifest_contract_test.ps1`:PASS(含结构化 mutant)。
- 三个 PowerShell 文件 AST:PASS。
- `go test ./...`、`go vet ./...`(`pkg/dsauthfence`):PASS。
- 未 push、未 apply、未写 secret、未 commit;没有访问生产。
