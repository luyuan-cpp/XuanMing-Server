# UE 主链路 + 本地 Agones 联调设计（W4 ⑬）

> 2026-06-09。承接「UE↔后端 gRPC-Web Login + Subscribe + Kafka Push 已通过」，推进 UE 主链路：
> **登录 → 拉/分配 Hub DS → 进大厅 → 匹配 → 拉/分配 Battle DS → 进战斗 → 结算 → 回大厅**。
>
> 本文是设计/契约层；本地 Agones 环境搭建与 apply 命令见 [`deploy/k8s/agones/README.md`](../../deploy/k8s/agones/README.md)。
> UE 侧代码在独立仓库 `Pandora-Client`（本地 `D:\luyuan\Xuanming`），命名一律 **Pandora**。

---

## 1. 主链路全景 + 各段责任

```
[UE Client] --gRPC-Web/Envoy--> login.Login
     login --gRPC--> hub_allocator.AssignHub  ──► 真实 hub_ds_addr + hub_ticket(JWT)
[UE Client] --NetDriver--> Hub DS(进大厅, 全图自由 PvP)
     Hub DS --gRPC every5s--> hub_allocator.Heartbeat
     Hub DS --gRPC--> player_locator.SetLocation(HUB)            ← 数据面上报
[UE Client] --gRPC-Web--> matchmaker.StartMatch ... ConfirmMatch
     matchmaker --gRPC--> ds_allocator.AllocateBattle ──► 真实 battle_ds_addr
     matchmaker 签 battle_ticket + player_locator.SetLocation(MATCHING/BATTLE)
     matchmaker --kafka pandora.match.progress--> push --stream--> Client(进战斗通知)
[UE Client] --NetDriver--> Battle DS(5v5 战斗)
     Battle DS --gRPC every5s--> ds_allocator.Heartbeat
     Battle DS --kafka pandora.battle.result--> battle_result(结算 + Elo MMR)
     战斗结束 → Client 回 Hub DS, Hub DS SetLocation(HUB, fence=match_id)
```

### 各段当前状态（后端 vs UE）

| 链路段 | 后端 | UE（Pandora-Client，独立仓库）|
|---|---|---|
| 登录 gRPC-Web | ✅ login（W3）| ✅ `UPandoraBackendSubsystem.Login`（已通）|
| 分配 Hub | ✅ hub_allocator.AssignHub（W4 ⑤/⑥）+ **Agones 发现（W4 ⑬）** | ⬜ Hub DS GameMode + Agones SDK |
| 进大厅 | ✅ login 返真实 hub_ds_addr（agones.enabled=true 后）| ⬜ NetDriver 连 Hub DS |
| Hub 心跳 | ✅ hub_allocator.Heartbeat | ⬜ Hub DS 每 5s 调（契约见 §3）|
| 匹配 | ✅ matchmaker（W4 ①/⑦）| ✅ gRPC-Web StartMatch/Confirm |
| 分配 Battle | ✅ ds_allocator.AllocateBattle + **真 Agones（W4 ⑫）** | ⬜ Battle DS + Agones SDK |
| 进战斗推送 | ✅ kafka match.progress → push stream | ✅ OnPushFrame 已通 |
| Battle 心跳 | ✅ ds_allocator.Heartbeat | ⬜ Battle DS 每 5s 调（契约见 §3）|
| 结算 | ✅ battle_result（W4 ③/⑨）| ⬜ Battle DS 发 pandora.battle.result |
| locator HUB/BATTLE 上报 | ✅ guard + fence（W4 ⑩/⑪）| ⬜ DS 调 SetLocation（契约见 §4）|

> **结论**：后端主链路骨架已全部就位；剩余是 (a) 本地 Agones 联调让 allocator 返回真实地址，
> (b) UE Hub/Battle DS 骨架按本文 §3/§4 契约接 allocator + locator。

---

## 2. Agones 两模型（后端已实现，详见 deploy README §0）

- **战斗 DS = 按需分配**：`ds_allocator/internal/data/agones_allocator.go`（W4 ⑫）POST GameServerAllocation。
- **大厅 Hub DS = 常驻分片**：`hub_allocator/internal/biz/agones_fleet.go`（W4 ⑬）LIST GameServer
  （`agones.dev/fleet=pandora-hub,pandora.dev/region=<region>`），lazy-seed 分片到 Redis。
- 两者 `agones.enabled=false` 默认走 Mock，`=true` 走真 Agones。**biz 逻辑零改**，只换 provider + main 装配。

---

## 3. DS 业务心跳上报契约（UE 侧实现）

⚠️ **Agones SDK health ≠ Pandora 业务 Heartbeat**。前者让 GameServer 进 Ready（Agones 调度用），
后者是 DS 向 allocator 上报负载/状态（容量判定 + 心跳超时补偿，不变量 §4）。UE DS 两者都要做。

### 3.1 Hub DS → `hub_allocator.Heartbeat`（每 5s 单向 unary）

`HeartbeatRequest`（`pandora/hub/v1/allocator.proto`）：

| 字段 | UE 填法 |
|---|---|
| `hub_pod_name` | Agones GameServer 名（环境变量 / SDK `GameServer().ObjectMeta.Name`）|
| `player_count` | 当前在线人数（hub_allocator 回写对账）|
| `cpu_pct` / `mem_mb` | 进程负载（可选，先填 0）|
| `state` | `"ready"` / `"draining"` / `"stopping"` |
| `ts_ms` | `now` 毫秒 |

响应 `command`：`""`=继续；`"drain"`=停止接新；`"stop"`=自行停机（孤儿分片）。

### 3.2 Battle DS → `ds_allocator.Heartbeat`（每 5s 单向 unary）

`HeartbeatRequest`（`pandora/ds/v1/allocator.proto`）：

| 字段 | UE 填法 |
|---|---|
| `ds_pod_name` | Agones GameServer 名 |
| `match_id` | 本对局 match_id（从 battle_ticket / 分配时下发取）|
| `player_count` | 当前战斗内人数 |
| `state` | `"warming"` / `"ready"` / `"running"` / `"ended"` |
| `ts_ms` | `now` 毫秒 |

响应 `command`：`""`=继续；`"stop"`=自行停机（孤儿 DS）。

> **心跳超时（默认 15s）→ allocator sweep 标记 abandoned/draining**，Battle DS abandoned 经
> `pandora.ds.lifecycle` 触发 battle_result 段位回滚补偿（W4 ⑧ at-least-once 闭环）。

---

## 4. player_locator HUB/BATTLE 上报闭环契约（UE 侧实现）

后端守卫（W4 ⑩/⑪）已就位：用 state 识别写入方权威，HUB 上报带 `match_id` 作 fence。
**MATCHING/BATTLE 由 matchmaker 写（控制面），HUB 由 Hub DS 写（数据面）**。UE Hub DS 负责 HUB 上报：

### 4.1 玩家进 Hub DS → `player_locator.SetLocation(HUB)`

`SetLocationRequest{ player_id, Location{ state=LOCATION_STATE_HUB, hub_pod, shard_id, match_id } }`：

| 场景 | `match_id` 填法（fence 令牌）|
|---|---|
| 全新登录进大厅 | `0`（无来源对局）|
| **战斗结束回大厅** | **填刚结束那场的 match_id**（从 battle DSTicket 取）|

- 后端 guard：`cur=BATTLE` 时，仅当 `in.match_id == cur.match_id && != 0` 才允许 `BATTLE→HUB`
  回流（合法）；不匹配/为 0 = stale hub DS 顶 active BATTLE → 拒 `ERR_LOCATOR_CONFLICT=9202`。
- 后端持久化 HUB 记录时**清零 match_id/battle_pod**（fence 仅供判定，进 HUB 后无活跃对局）。
- `cur=MATCHING`（确认期 ~15s）时 HUB 上报一律拒（玩家物理上还连着 hub，但权威态是 MATCHING）。

### 4.2 时序要点（避免顶号冲突）

- Hub DS 进大厅那刻才 SetLocation(HUB)，不要在 matchmaker 写 MATCHING 后还重复刷 HUB。
- 战斗结束回流必须带 fence match_id，否则会被后端正确拒绝（这是防 stale 的设计，不是 bug）。

---

## 5. UE Hub DS / Battle DS 骨架要点（Pandora-Client 仓库，命名 Pandora）

> 仅列后端联调相关的骨架职责；GAS / Iris / Replication 细节见 `ds-arch.md`。

- **模块**：服务端逻辑放 `PandoraHubServer` / `PandoraBattleServer`（不在客户端模块 `Source/Pandora/`，CLAUDE §11.3）。
- **Agones SDK**：DS 进程起来后 `SDK.Ready()`，周期 `SDK.Health()`；接 `SDK.WatchGameServer` 拿分配元数据。
- **后端 gRPC 客户端**：DS → allocator / locator 走**标准 gRPC**（DS 在集群内，不走 Envoy/gRPC-Web）。
  心跳/SetLocation 是 unary，可复用一个轻量 gRPC 通道。
- **GameServer 名**：从 Agones SDK `GameServer().ObjectMeta.Name` 取，作 `hub_pod_name`/`ds_pod_name`。
- **match_id 下发**：matchmaker AllocateBattle 时把 match_id 打到 GameServer label（`pandora.dev/match-id`），
  Battle DS 经 SDK 读 label 拿 match_id；或经 battle_ticket（JWT claim）取。
- **占位验证**：UE DS 就绪前，先用 `deploy/k8s/agones` 的 simple-game-server 占位 Fleet 验
  Agones 分配链路（见 README §4 第一步）；心跳 / locator 链路用 `tools/scripts/ds_heartbeat_stub.ps1`
  当 stub（grpcurl 周期调 Heartbeat + SetLocation，第二步），真 UE DS 就绪后替换。

---

## 6. 阶段限制（留后续）

- hub_allocator `AgonesHubFleetProvider` 只在 region 首次无分片时 lazy-seed，Fleet 扩缩容后新
  GameServer 不自动发现（周期性 reconcile 留后续）。
- 占位镜像不发业务心跳，心跳超时 sweep / locator 上报闭环须真 UE DS 或 stub 才能端到端验。
- D7（k8s 选型：ACK / 自建 / minikube）仍未拍板；当前按 minikube + Agones dev 推进，代码 provider 无关不受阻。
- 真集群（指向真 Agones）联调 + UE DS 落地后，更新本文与 PROGRESS。
