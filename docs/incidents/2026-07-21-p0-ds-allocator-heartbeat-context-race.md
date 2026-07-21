# [INC-20260721-001][P0] ds_allocator Heartbeat context 逃逸导致响应 metadata 并发写崩溃

> **状态**：根因确认  
> **类型**：`crash`  
> **环境**：本机 k8s / Agones 集成环境（`pandora` namespace，非生产）  
> **首次发生时间（UTC）**：2026-07-21 07:42:40.454  
> **首次发现时间（UTC）**：2026-07-21 07:42:40.454  
> **负责人**：待指定（当前 Claude Code 在途修复；Codex 仅做独立复核与事故文档）  
> **受影响运行版本**：`ds_allocator version=9f68563`，Pod `ds-allocator-7ccbf978d-9qt8p`  
> **源码审计基线**：`HEAD 86c5584`（同型缺陷仍存在）  
> **最后更新**：2026-07-21

## 0. 一句话结论

`ds_allocator` 在 Heartbeat handler 中把入站请求 `ctx` 经 `context.WithoutCancel` 传入 fire-and-forget locator gRPC；该 context 仍携带原 Heartbeat 的 Kratos server transport 和 `ReplyHeader metadata.MD`。下游 client Trace 在同一 context 同时看到 server/client transport，写入原响应 Header Map，与 gRPC 回包阶段的 `metadata.Join` 遍历并发，触发不可恢复的 `fatal error: concurrent map iteration and map write`。allocator 重启又被同 Pod 残留的 15 秒 capability 租约挡住，Battle DS 在 20 秒权威租约到期后自我 fencing 并踢出 1 名玩家，客户端最终恢复到 Hub。

事故直接根因已经确认，但永久修复尚未提交、部署和完成 race/故障注入验证，因此不得关闭。

## 1. 影响与范围

- 玩家影响：对局 `match_id=14854423566155776` 中的 `player_id=12289834169171968` 被 Battle DS 主动 Kick，客户端重连旧 Battle ticket 被拒，随后自动回到 Hub。
- 对局影响：旧 Battle GameServer `pandora-battle-stable-tsqvg-6vvsb` 失去 allocator 权威心跳，进入 fencing/回收路径。
- 服务影响：单副本 `ds_allocator` 发生 runtime fatal；第一次重启因 capability duplicate key 再次退出，约 16.3 秒后恢复 Ready。
- 数据影响：当前证据未发现玩家持久数据损坏；对局是否有未上报尾窗事实由既有补偿语义处理，本事故未证明存在额外数据丢失。
- 安全影响：没有凭证泄漏证据；事故分析发现请求 context 会不必要地保留 metadata/鉴权头，属于必须消除的生命周期风险。
- 潜在范围：`hub_allocator` 存在完全同构的 Heartbeat → goroutine → locator gRPC 链，虽然本次未观察到 Hub fatal，但代码层可达条件完整。
- 严重级别理由：控制面单进程 fatal 直接导致在玩玩家被踢，且同型缺陷位于共享 middleware/两个 allocator 心跳旁路，符合 P0 crash/availability 级别。

## 2. 第一现场与证据

### 2.1 客户端症状

客户端日志使用 America/New_York 夏令时（EDT，UTC-4）：

- `03:42:53.853`：恢复协调器确认 Battle Admission。
- `03:42:55.062`：收到 `ConnectionLost`，开始重查权威。
- `03:42:56.559`：重连旧 Battle 失败，`ticket rejected`。
- `03:42:59.154`：使用新 Hub ticket 执行 ClientTravel。
- `03:43:00.033`：收到 Hub Admission ACK。

原始客户端日志包含完整 DSTicket，未复制进仓库；本文只保留脱敏时间和行为证据。

### 2.2 allocator runtime fatal

Loki 查询范围：

```text
pod=ds-allocator-7ccbf978d-9qt8p
UTC 2026-07-21T07:42:25Z..2026-07-21T07:43:15Z
```

最小堆栈：

```text
07:42:40.453 DEBUG trace_id=6a366247-ca06-4cac-bd45-0fa889436e54
  msg=rpc_ok op=/pandora.ds.v1.DSAllocatorService/Heartbeat latency_ms=9

07:42:40.454 fatal error: concurrent map iteration and map write
internal/runtime/maps.(*Iter).Next
google.golang.org/grpc/metadata.Join
google.golang.org/grpc/internal/transport.(*ServerStream).SetHeader
google.golang.org/grpc.SetHeader
github.com/go-kratos/kratos/v2/transport/grpc.unaryServerInterceptor
pandora.ds.v1._DSAllocatorService_Heartbeat_Handler
```

`rpc_ok` 先于 fatal 不代表请求完整发送成功：Logging middleware 已看到业务 handler 返回，fatal 发生在外层 Kratos interceptor 随后执行 `grpc.SetHeader` 的回包 Header 合并阶段。Battle DS 因此收到截断的 gRPC-Web data/trailer frame。

### 2.3 重启 fencing 证据

第一次新容器进程启动后：

```text
07:42:43.209 ds_auth_fence_acquire_failed
dsauthfence: acquire capability: capability registration fenced by duplicate key,
activation lock, or required epoch change:
/pandora/ds-auth/capabilities/ds_allocator/e5e17f83-46d0-49e5-a5df-90ac5852eba8
```

Pod UID 未变化，进程按 [main.go](../../services/battle/ds_allocator/cmd/ds_allocator/main.go) 的 fail-closed 逻辑 `os.Exit(1)`。旧进程遭 runtime fatal，无法执行 `defer fence.Close()`，旧 capability 只能等待租约或安全接管机制处理。

### 2.4 Battle DS fencing 证据

旧 Battle Pod 日志：

```text
07:42:40.503 Heartbeat failed: truncated grpc-web data/trailer frame
07:42:55.014 authority lease expired: no credential-bound heartbeat ACK within 20s
07:42:55.014 fencing all existing players
07:42:55.015 fencing complete: kicked=1 orphan_pawns=0 pending_rejected=0
07:42:56.536 PreLogin rejected: admission gate closed because authority lease expired/draining
```

### 2.5 已排除的同时出现噪声

- `aqProf.dll GetLastError=126`：UE 对可选分析器 DLL 的探测失败，不会导致本次服务器 allocator fatal。
- SonglinTown 外部 Actor 依赖资源缺失：内容资源问题，与 Go `metadata.Join` 崩溃堆栈无调用关系。
- 怪物 `Muzzle_01` 插槽缺失：战斗表现/配置问题，出事前后持续出现，不是 allocator 进程退出原因。
- `CfgDrop` 未加载：掉落配置问题，不写 gRPC metadata，也不参与 Heartbeat 回包。

## 3. 完整时间线

以下统一使用 UTC；客户端 EDT 时间已换算为 UTC。

| UTC 时间 | 组件 | 事件 | 结论 |
|---|---|---|---|
| 07:42:35.003 | ds_allocator | 上一条 Heartbeat `rpc_ok` | 最后一条已观察到的正常 ACK 周期 |
| 07:42:40.453 | ds_allocator | 出事 Heartbeat 业务 handler 返回 `rpc_ok` | 业务处理完成，尚未完成回包 Header 发送 |
| 07:42:40.454 | ds_allocator | `concurrent map iteration and map write` | Go runtime fatal，进程立即退出 |
| 07:42:40.503 | Battle DS | Heartbeat 收到截断的 gRPC-Web frame | allocator 在发送响应途中死亡 |
| 07:42:42.927 | ds_allocator | 容器进程第一次重启 | Pod UID 未变 |
| 07:42:43.209 | ds_allocator | capability duplicate key，`os.Exit(1)` | 旧 15 秒租约放大恢复时间 |
| 07:42:53.853 | Client/Battle DS | 客户端一度确认 Battle Admission | 旧 DS 尚未到本地租约截止点 |
| 07:42:55.014 | Battle DS | 20 秒授权租约到期，自我 fencing | 为防脑裂主动停止服务存量玩家 |
| 07:42:55.015 | Battle DS | `kicked=1` | 用户看到被踢出对局 |
| 07:42:55.062 | Client | `ConnectionLost`，重查权威 | 客户端恢复机制启动 |
| 07:42:56.414 | Client | 重连旧 Battle endpoint | 仍指向旧 GameServer |
| 07:42:56.536 | Battle DS | ticket/Admission 被拒 | DS 已关闭准入门 |
| 07:42:56.594 | ds_allocator | 再次启动 | 旧 capability 已可处理 |
| 07:42:56.737 | ds_allocator | `ds_auth_fence_ready`、`service_ready` | 比 DS fencing 晚约 1.7 秒 |
| 07:42:56.741 | ds_allocator | `heartbeat_sweep_started` 并立即 initial sweep | 与恢复 Heartbeat 存在竞争窗口 |
| 07:42:57.001 | ds_allocator | 对旧 GameServer 发起回收，delete status 200 | 对局进入终态/回收路径 |
| 07:42:59.154 | Client | 使用 Hub ticket ClientTravel | Battle 恢复失败后回 Hub |
| 07:43:00.033 | Client/Hub DS | Hub Admission ACK | 用户回到大厅 |

## 4. 调用链与关键变量

### 4.1 业务调用链

```text
Battle DS Heartbeat
  → AllocatorService.Heartbeat(ctx, req)
  → CheckBattleCredential
  → AllocatorUsecase.HeartbeatAuthorizedWithPlayers(...)
  → ActivateHeartbeat / 更新 Redis 权威心跳
  → refreshBattleLocations(ctx, playerIDs, matchID, dsAddr)
       → 拷贝 playerIDs
       → go func()
            → context.WithTimeout(context.WithoutCancel(ctx), 3s)
            → GrpcLocationRefresher.RefreshBattleLocations
            → 每个 player_id 调 PlayerLocatorService.SetLocation
            → grpcclient 默认 Trace + Metrics + CircuitBreaker
```

相关代码入口：

- [Heartbeat service](../../services/battle/ds_allocator/internal/service/allocator.go)
- [Heartbeat biz 与 refreshBattleLocations](../../services/battle/ds_allocator/internal/biz/allocator.go)
- [locator gRPC client](../../services/battle/ds_allocator/internal/data/locator_client.go)
- [默认 gRPC client middleware](../../pkg/grpcclient/grpcclient.go)
- [Trace middleware](../../pkg/middleware/trace.go)

### 4.2 context 与 Map 共享关系

Kratos server interceptor 为 Heartbeat 创建：

```text
ctx0
  └─ serverTransportKey → trS
       └─ ReplyHeader() → metadata.MD Map M
```

`context.WithoutCancel(ctx0)` 只移除取消/deadline，所有 Value 原样保留：

```text
rctx.Value(serverTransportKey) → 同一个 trS → 同一个 Map M
```

locator client interceptor 再加入 client transport：

```text
ctx1
  ├─ serverTransportKey → trS → ReplyHeader Map M
  └─ clientTransportKey → trC → RequestHeader Map C
```

原 Trace 在 client hop 中使用两个独立的 `if`：

```go
if trS exists {
    trS.ReplyHeader().Set(traceID)   // 写原 Heartbeat 的 Map M
}
if trC exists {
    trC.RequestHeader().Set(traceID) // 写 locator 请求 Map C
}
```

与此同时原 handler 已返回：

```text
server goroutine：grpc.SetHeader → metadata.Join → 遍历 Map M
async goroutine：client Trace → ReplyHeader.Set → 写 Map M
```

Go Map 不支持并发遍历/写入，因此 runtime 直接 fatal。

### 4.3 关键变量

| 变量/对象 | 创建位置 | 所有者与生命周期 | 是否共享/可变 | 事故中的作用 |
|---|---|---|---|---|
| `ctx` / `ctx0` | Heartbeat 入站 | 仅应活到 handler 返回 | 通过 Value 引用请求级对象 | 被错误传入后台 goroutine |
| `trS` | Kratos server interceptor | 本次 Heartbeat | 指针共享 | 持有原响应 Header |
| `M` | `replyHeader := metadata.MD{}` | 本次 Heartbeat 回包阶段 | 可变 Go Map | 一个 goroutine 遍历、另一个写入 |
| `players` | `append([]uint64(nil), playerIDs...)` | 后台任务私有 | 已复制 | 安全，不是竞态根因 |
| `matchID` | Heartbeat 参数 | 值传递 | 不可变值 | 安全 |
| `dsAddr` | BattleStorageRecord | 字符串值 | 不可变值 | 安全 |
| `rctx` | `WithoutCancel(ctx)` | 后台 locator 调用 | 继承全部 Value | 错误保留 `trS/M` |
| `trC` / `C` | Kratos client interceptor | 单次 locator RPC | 本次 client 私有 Map | 正常应只写 `C` |
| `traceID` | Trace middleware/context | 请求链路 | 字符串值 | 值本身安全，错误写入目标导致 fatal |

## 5. 根因与放大因素

### 5.1 直接根因

直接根因不是 Redis、玩家列表或 locator 数据，而是同时满足以下条件：

1. 入站 Heartbeat request context 携带 server transport 和响应 `metadata.MD`。
2. fire-and-forget goroutine 使用 `context.WithoutCancel(requestCtx)`，错误地把全部 Value 延长到 handler 返回之后。
3. goroutine 发起挂有共享 Trace middleware 的下游 gRPC。
4. client context 同时含继承的 server transport 和新 client transport。
5. Trace 未按“本次 interceptor 的调用方向”互斥处理，client hop 仍写继承的 server `ReplyHeader`。
6. 原 Heartbeat 正在 `grpc.SetHeader/metadata.Join` 遍历同一个 Map。

### 5.2 触发条件与概率

- Heartbeat 必须带有需要刷新 locator 的活跃玩家。
- locator gRPC goroutine 必须与 Heartbeat 回包 Header 发送窗口重叠。
- Battle 按玩家逐个 `SetLocation`；玩家越多，下游 client middleware 执行次数越多，命中竞态窗口的概率越高。
- 即使写入相同的 trace key/value，`metadata.MD.Set` 仍然是 Go Map 写操作。

### 5.3 为什么 Recovery 没挡住

`concurrent map iteration and map write` 是 runtime `fatal/throw`，不是可由 middleware `recover()` 捕获的普通业务 panic；整个进程必须退出。

### 5.4 capability 租约放大

- [dsauthfence](../../pkg/dsauthfence/fence.go) 默认 capability TTL 为 15 秒。
- key 为 `/pandora/ds-auth/capabilities/{service}/{PodUID}`。
- runtime fatal 无法执行 `Close()`；容器重启但 Pod UID 不变，新进程注册相同 key。
- 原基线要求 `CreateRevision(key) == 0`，旧租约未消失时新进程 fail-closed 退出。
- 本次 Deployment 只有一个 allocator 副本，没有第二个 Ready writer 承接 Heartbeat。

Battle DS 权威租约上限为 20 秒；崩溃时距最后 ACK 已约 5 秒，因此只剩约 15 秒恢复预算。capability 最坏 TTL 也是 15 秒，再叠加容器启动和 CrashLoopBackOff 后没有安全余量。实际 allocator 比 DS 本地 fencing 晚约 1.7 秒恢复。

不能通过忽略 duplicate key、无条件覆盖或删除 fencing 解决；必须在保持单 writer/防脑裂的前提下实现可证明的同 Pod 安全接管，或重新闭合整体恢复时间预算。

### 5.5 启动立即 sweep 放大

[RunHeartbeatSweep](../../services/battle/ds_allocator/internal/biz/allocator.go) 启动后立即执行一次 `sweepOnce`，Heartbeat timeout 为 15 秒。allocator 恢复时，initial sweep 与 DS 的第一条恢复 Heartbeat 存在竞争：

- Heartbeat 先刷新记录，可能保住对局；
- sweep 先把旧记录推进 `ABANDONED/TERMINATING`，恢复 Heartbeat 随后只能收到 stop。

本次玩家首先在 20 秒 DS 本地租约点被 Kick，所以 sweep 不是最初踢人动作，但它会把临界恢复窗口进一步终态化。

## 6. 全仓同类问题扫描

### 6.1 扫描范围

- 基线：`HEAD 86c5584`，与 Claude Code 在途工作树分开审计。
- 目录：`pkg/`、`services/`、`tools/`、`robot/` 的生产 `.go` 文件。
- 检查项：全部 `context.WithoutCancel`、生产 goroutine、gRPC client 接线、Kratos transport Header 写入、raw metadata 操作、Trace/Metrics/Logging 的 server/client 方向判断。
- 原基线共 18 个实际 `context.WithoutCancel` 调用，另有 1 个注释命中。

### 6.2 Confirmed 同型 P0

1. `ds_allocator.refreshBattleLocations`：本次实际事故链。
2. `hub_allocator.RefreshHubPresence`：完全同构：

```text
HubService.Heartbeat
  → RefreshHubPresence
  → goroutine + WithoutCancel(request ctx)
  → PlayerLocatorService.RefreshHubLocations
  → client Trace
  → 可能并发写原 Hub Heartbeat ReplyHeader
```

Hub 路径尚无本次运行事故证据，但代码级必要条件完整，按同型 P0 处理。

### 6.3 结构性隐患

- `ds_allocator.killStrandedDS`：请求 context 进入 goroutine；当前 Release 只走本地/Agones HTTP，不会进入 Trace gRPC，所以不是本次 fatal，但违反 detached context 规则。
- Auction `RedisMarketLocker`：把 `context.WithoutCancel(requestCtx)` 保存为 lease `logCtx` 并进入续租 goroutine；当前续租/释放只走 Redis，继承 context 只用于日志/failStop，尚不具备本次 fatal 条件，但未来增加 gRPC 上报会转为同型触发点。
- Metrics 原基线 server-first：嵌套 client RPC 会被错误标记为父 server operation，污染请求数和延迟指标。
- Logging 原基线同样 server-first；当前默认 client 链未挂 Logging，但自定义使用时会误标方向。

### 6.4 已排除项

其余 14 个 `WithoutCancel` 当前均为同步 Redis/MySQL/Kafka/K8s HTTP 调用，或调用来源本身是 `context.Background()`；handler 会等待完成，未形成“请求已回包 + 后台 Trace gRPC”完整链。

raw metadata 审计结果：

- 生产代码中通过 Kratos transport 修改 `ReplyHeader/RequestHeader` 的公共位置只有 Trace。
- `internalrpcauth` 修改 outgoing metadata 前先复制，Verifier 对 incoming metadata 只读。
- Hub locator 使用 `metadata.AppendToOutgoingContext` 创建新 context，不原地修改父 metadata。

没有发现 Battle/Hub 之外第三条当前可达的完整致命链。

## 7. 处置与永久修复状态

以下是 2026-07-21 最后审计到的共享工作树快照。所有代码项均属于 Claude Code 在途改动，未提交、未部署，不能视为线上/集成环境已经修好。

| 项目 | 当前状态 | 说明 |
|---|---|---|
| Trace client/server 分支互斥 | 在途 | client transport 存在时只写本次 `RequestHeader` 并 early return |
| `plog.Detach` | 在途 | 从 `context.Background()` 派生，只白名单复制 trace/player/match/team 日志字段 |
| Battle locator 异步 ctx | 在途 | `refreshBattleLocations` 改用 `plog.Detach` |
| Hub locator 异步 ctx | 在途 | `RefreshHubPresence` 改用 `plog.Detach`，bearer token 继续显式字符串传值 |
| Metrics/Logging 方向 | 在途 | 改为 client transport 优先，server 只作兜底 |
| Trace/Detach/真实 gRPC 回环测试 | 在途 | 普通测试通过，race 未执行 |
| 同 Pod capability 安全接管 | 在途、未完成审核 | 工作树已出现实现与测试改动，尚无完整测试/部署/故障注入结论 |
| `killStrandedDS` detached ctx | 未完成 | 仍存在 `WithoutCancel(requestCtx)` |
| Auction market lock detached log ctx | 未完成 | 仍保存完整请求 context |
| initial sweep 恢复竞争 | 未完成 | 尚无设计/测试闭环 |

## 8. 验证矩阵

| 验证 | 当前结果 | 环境/命令 | 关闭影响 |
|---|---|---|---|
| 公共包普通测试 | 通过 | `go test -count=1 ./pkg/internalrpcauth ./pkg/grpcclient ./pkg/grpcserver ./pkg/log ./pkg/middleware` | 仅证明普通路径 |
| Trace 双 transport 单测 | 通过（在途文件） | `pkg/middleware/trace_test.go` | 未提交 |
| Detach transport 剥离测试 | 通过（在途文件） | `pkg/log/detach_test.go` | 未提交 |
| 真实 Kratos server/client 回环 | 普通模式通过（在途文件） | `pkg/middleware/trace_integration_test.go` | 必须再跑 race |
| `go test -race` | 阻断 | 本机 `CGO_ENABLED=0`，报 `-race requires cgo` | P0 不得关闭，须在 Linux/CI 完成 |
| Battle/Hub service 调用点回归 | 未完成 | 需断言 detached ctx 不含 server/client transport，且 trace 保留 | P0 不得关闭 |
| runtime fatal/OOM/SIGKILL 重启注入 | 未完成 | 需验证 capability 接管、DS lease、sweep 时间线 | P0 不得关闭 |
| 玩家 Battle E2E | 未完成 | 修复镜像部署后重新匹配、在玩、注入 allocator 重启并验证不被踢 | P0 不得关闭 |

## 9. 部署、回滚与观察

- 修复 commit：未生成。
- 镜像 digest：未生成。
- 部署时间：未部署。
- 当前运行 Pod 仍为事故时旧构建语义，不能用“Pod 当前 Ready”证明修复已生效。
- 观察窗口：未开始。
- 回滚要求：任何 capability 安全接管改动若无法证明单 writer、CAS 精确匹配、旧进程失租退出和跨版本 fail-closed，禁止部署；不得以删 key/覆盖 key 作为回滚或止血。

## 10. 防复发规则

- [AGENTS.md](../../AGENTS.md)：禁止请求 context 逃逸到后台任务；崩溃/P0 强制建档。
- [CLAUDE.md](../../CLAUDE.md)：context/transport 生命周期、writer 租约恢复预算、事故关闭门。
- [Go 服务契约](../design/go-services.md)：全服务 RPC context 与异步边界。
- [Linux DS 崩溃观测手册](../ops/linux-ds-observability.md)：第一现场取证与 Incident 建档入口。
- [事故目录索引](index.md)：统一状态、模板和关闭要求。

## 11. 剩余行动项

| ID | 严重级别 | 行动项 | 状态 |
|---|---|---|---|
| A1 | P0 | 完成并独立审核 Trace + Battle/Hub 双层修复 | 在途 |
| A2 | P0 | 在支持 CGO 的 Linux/CI 跑真实回环 `go test -race` | 未开始 |
| A3 | P0 | 完成同 Pod capability 安全接管证明、单元测试和 fatal/SIGKILL 故障注入 | 在途 |
| A4 | P0 | 验证 capability TTL、容器重启、20 秒 DS lease、15 秒 heartbeat timeout、initial sweep 的统一时间预算 | 未开始 |
| A5 | P1 | 修复 `killStrandedDS` 和 Auction market lock 的请求 context 泄漏 | 未开始 |
| A6 | P1 | 为 Metrics/Logging 增加独立双 transport 方向测试 | 未开始 |
| A7 | P0 | 构建、部署新镜像并核对 commit/imageID/Pod provenance | 未开始 |
| A8 | P0 | 完成玩家 Battle E2E、故障路径和观察窗口验证 | 未开始 |

## 12. 关闭审核

- [x] 直接根因和触发条件有源码、堆栈和运行日志闭环
- [x] 全仓同类模式扫描完成
- [ ] 永久修复已提交并独立审核
- [ ] 修复前失败、修复后通过的回归完整
- [ ] Linux/CI race 通过
- [ ] fatal/OOM/SIGKILL 重启故障注入通过
- [ ] 目标环境已加载可追溯的新镜像
- [ ] 玩家路径、恢复和补偿路径通过
- [ ] 观察窗口无复发
- [ ] 剩余风险已解决或另建 Incident/任务

**关闭结论**：未关闭；当前仅达到“根因确认”。

