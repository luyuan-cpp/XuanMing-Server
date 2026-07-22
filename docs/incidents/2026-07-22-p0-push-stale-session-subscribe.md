# [INC-20260722-004][P0] 旧/被顶号会话 token 仍能订阅私有推送流

> **状态**：修复实施中（会话现行性门 + 流内复查 + 生成器强制档已落码测试绿，2026-07-22；R4 复审发现两处仍可达窗口——建流校验/注册 TOCTOU、写者 Send 阻塞时复查饿死——已于同日修复并补确定性回归；真实 Envoy/Redis 环境验收未跑，未关闭）
> **类型**：`security` / `session-fencing` / `near-miss`
> **环境**：本机源码与单测审计（未确认在线上发生）
> **首次发生时间（UTC）**：未在线上确认；受影响行为自 push Subscribe 上线（W3 ④）即存在
> **首次发现时间（UTC）**：2026-07-22（外部只读审计 R4 轮）
> **负责人**：修复由 Claude 实施，项目负责人待指定
> **受影响服务/版本**：`services/runtime/push`（Subscribe 建流路径）、`deploy/envoy/envoy.yaml` push 路由、`tools/scripts/gen_cluster_config.ps1` 生产产物
> **最后更新**：2026-07-22

## 0. 一句话结论

push `Subscribe` 只依赖 Envoy jwt_authn 验签取 `player_id`，不校验会话现行性（jti）：被顶号/已重登的**旧 token 在 exp 前仍能重建订阅流收取该玩家全部私有推送**（邮件、好友、战斗结果、经验入账等），且已建立的流在 JWT 过期后**无限期存活**（server stream 永不重验）。按项目"安全突破"口径属 P0（CLAUDE.md §9.23 会话 fencing：旧 session 不得再获得任何按 player_id 定向的服务端能力）。

## 1. 影响与范围

- 玩家影响：账号被盗后即使受害者重新登录顶号，攻击者持有的旧 token 仍能持续窃听该账号的私有推送（含资产、社交、对局数据）；同时旧设备建流会触发顶号语义，可反复顶掉新设备的合法连接（骚扰向拒绝服务）。
- 影响人数/请求数：线上是否被利用未知；本次为上线前静态审计确认，代码路径自 push 真实化起存在。
- 服务影响：`services/runtime/push` `service/push.go Subscribe` 全路径。
- 数据与安全影响：私有推送越权可读 = 会话 fencing 突破；不涉及写路径（push 只读投递）。
- 是否仍可复发：代码修复已落地但未经真实 Envoy + Redis 环境验收，视为可复发。
- 严重级别判定理由：破坏 §9.23 "旧 session 不得再签 DS 票/获得会话能力" 的安全不变量，越权面为全部定向推送内容。

## 2. 第一现场与证据

### 2.1 症状

- 无线上样本；审计静态证据如下。

### 2.2 原始证据（修复前基线）

```text
services/runtime/push/internal/service/push.go(修复前)
  Subscribe 仅 pmw.PlayerIDFromContext(ctx) 取 player_id 即 Register + RunSubscribeStream,
  无任何 jti 现行性校验;login 侧 pandora:sess:<pid> 的 jti 权威(2026-07-15 P0 修复引入)
  未被 push 消费。

deploy/envoy/envoy.yaml push 路由(修复前)
  timeout: 0s / idle_timeout: 0s 且无 max_stream_duration:流一旦建立永不重验 JWT,
  token 到期/被顶号后流无限期存活。

攻击路径:旧 token(exp 前)→ Envoy jwt_authn 验签通过 → Subscribe 建流成功
  → 顶掉新设备 slot(Register 顶号语义)→ 持续收取私有推送。
```

### 2.3 已排除的噪声

- Envoy `x-pandora-jwt-payload` 头由 jwt_authn 验签后重写、入站无条件剥离，客户端无法伪造 jti——头信道本身可信，缺的是"现行性"校验而不是"真实性"校验。
- 其它 unary 服务走 pmw 中间件链已有会话校验路径；本缺陷特定于 server stream 不跑 unary 中间件链。

## 3. 根因

1. **建流门缺失**：JWT 验签只证明"曾经登录过"，不证明"未被顶号"。login 已维护 `pandora:sess:<player_id>` hash `jti` 作为会话现行性权威（2026-07-15 INC 修复），但 push 从未消费该权威。
2. **流内复查缺失**：server stream 只在建流时过一次 Envoy 验签；顶号、登出、token 到期都不会中断已建立的流。跨 Pod 场景下新建流发生在另一副本，旧副本上的旧流无人踢。
3. **放大因素**：push 路由 `timeout: 0` 且无 `max_stream_duration`，流寿命无上界；Register 顶号语义让旧 token 还能主动断新设备连接。

## 4. 修复

### 4.1 代码修复（已落地，2026-07-22）

- `pkg/middleware/auth.go`：新增 `SessionClaimsFromContext` / `ParseJWTPayloadClaims`，从 Envoy 验签后重写的 `x-pandora-jwt-payload` 头解析 `jti` + `exp`（毫秒）。
- `services/runtime/push/internal/data/session_gate.go`（新增）：`RedisSessionGate` 只读 login 会话权威 `pandora:sess:<pid>` hash `jti`（key 格式为跨服务契约，与 `login internal/data/account.go` 同步维护）；Redis 错误返回 `ErrUnavailable`，调用方 fail-closed。
- `services/runtime/push/internal/biz/push.go`：
  - `AuthorizeSubscribe`（建流门，Register **之前**执行——旧 token 不再有机会顶掉新设备连接）：jti ≠ 权威当前一代 → 拒；无会话 → 拒；权威不可达 → fail-closed 拒；`require_session_gate: true` 档下缺 jti（绕网关）→ 拒。
  - `recheckSession`（流内 30s 周期复查）：jti 被轮换（顶号，含跨 Pod 旧流）/ 登出 / `exp` 已过 → 关流；权威查询失败按可重试计连败，连续 3 次 → fail-closed 关流（短抖动不误杀，持续故障不裸奔）。
- `services/runtime/push/internal/conf/conf.go` + `etc/push-dev.yaml`：`require_session_gate` 开关，dev 缺省 false（§14.2 直连联调不变；有 jti 仍校验），**生产由生成器机械置 true**。
- `tools/scripts/gen_cluster_config.ps1`：`Set-ProdPushSessionGateOn`，`-Prod` 产物强制 `require_session_gate: true`，模板锚点异常拒绝生成。
- `deploy/envoy/envoy.yaml`：push 路由加 `max_stream_duration: 3600s`（独立兜底层：流寿命有界，到期强制带新 token 重建再过建流门）。

### 4.1.1 R4 复审补修（2026-07-22，同日）

首轮修复存在两处仍可达窗口，复审确认后补修：

1. **建流校验/注册 TOCTOU**：`AuthorizeSubscribe` 与 `Register` 分离执行——旧会话校验
   通过后暂停、新会话完成校验+注册、旧会话恢复再注册，会反过来取消新设备连接并接管
   槽位（“旧 token 不再能顶掉新设备”不成立）。补修：`AuthorizeAndRegister` 在同玩家
   64 条带锁内串行执行「校验 → 注册」；任一交错都收敛到新会话持有连接（旧会话校验排
   在新会话注册后必读到已轮换 jti 被拒；排在前则旧流短暂注册后立即被新注册顶掉）。
   回归：`TestAuthorizeAndRegister_StaleSessionCannotDisplaceNewer`（用可阻塞 gate 确定
   性复现原交错）。
2. **“30 秒内关闭旧流”无时限保证**：流内复查与写者共用同一 select，写者阻塞在
   `stream.Send`（慢客户端流控）或首轮 replay 期间，复查永远轮不到；实际最坏收敛由
   gRPC `max_connection_age`(15m)+grace 决定。补修：会话复查改为**独立看门狗 goroutine**
   （不受写者阻塞影响），失效后 ≤30s 取消流上下文；写者每次 Send 前检查取消，不再投
   递任何新帧，并以映射后的 gRPC 状态（UNAUTHENTICATED/UNAVAILABLE，见
   `pkg/errcode/grpc.go`）关流。**诚实契约**：30s 界定的是「停止投递 + 发起关流」；
   写者已阻塞在传输层的至多一帧、以及 TCP/流句柄的物理回收，仍由 keepalive/
   max_connection_age/Envoy max_stream_duration 有界收敛，不是 ≤30s。
   回归：`TestRunSubscribeStream_WatchdogClosesBlockedWriter`（写者阻塞在 Send 时顶号，
   断言解除阻塞后零新帧投递并以 ErrUnauthorized 关流）。

### 4.2 回归测试（已落地，全绿）

- `internal/biz/replay_duplicate_test.go`：`TestAuthorizeSubscribe_SessionCurrency`（现行放行/旧 jti 拒/登出拒/require 档缺 jti 拒/权威故障 fail-closed/dev 档语义）、`TestRecheckSession_ExpiryClosesStream`（token 到期关流）、`TestRecheckSession_SupersededAndRetryable`（顶号关流含跨 Pod 语义/权威故障可重试）。
- `internal/data/session_gate_test.go`：miniredis 验证跨服务 key 契约、无会话、空 jti 防御、权威不可达必须报错。
- `tools/scripts/tests/gen_cluster_prod_progress_contract_test.ps1`：`-Prod` 产物恰好一处 `require_session_gate: true`、dev 产物保持 false（PASS）。

## 5. 验收矩阵（审计要求，关闭前必须逐项打勾）

| 场景 | 期望 | 状态 |
|---|---|---|
| 旧 token 重连（已被新登录顶号） | 建流拒绝 `session superseded`（UNAUTHENTICATED） | 单测绿；真实 Envoy 链路未验 |
| **建流并发交错（R4 复审①）**：旧会话校验通过后暂停 → 新会话注册 → 旧会话再注册 | 任意交错下新会话持有连接槽，旧会话不得取消新设备 | 确定性交错回归绿；真实并发/多 Pod 实测未跑 |
| 跨 Pod 顶号（旧流在 A Pod，新登录经 B Pod） | 旧流 ≤30s 停止投递并发起关流 | 单测绿（逻辑层）；多 Pod 实测未跑 |
| **写者阻塞在 Send（R4 复审②）**：慢客户端流控期间顶号 | 看门狗独立裁决；解除阻塞后零新帧、以 UNAUTHENTICATED 关流；句柄回收由 keepalive/max_conn_age/Envoy 1h 有界兜底 | 阻塞写者回归绿；真实流控/慢客户端实测未跑 |
| 会话轮换（同设备重登） | 旧 jti 流关闭，新 jti 建流成功 | 单测绿；E2E 未跑 |
| token 到期 | ≤30s 停止投递并发起关流；Envoy 1h 流寿命兜底 | 单测绿；Envoy max_stream_duration 未实测 |
| Redis 故障 | 建流 fail-closed 拒；在流连续 3 次失败关流 | 单测绿；真实故障注入未跑 |
| dev 直连联调 | 缺省档行为不变（无 jti 放行，有 jti 仍校验） | 单测绿 |

## 6. 剩余风险与关闭条件

- **未关闭原因**：真实 Envoy(:8443 jwt_authn + payload 头) + 共享 Redis + 多 Pod 环境的验收矩阵未执行；建流并发交错与阻塞写者两条 R4 复审场景目前只有确定性单测，缺真实并发/流控实测；`go test -race` 需 CGO/Linux CI。
- 会话权威与 push 必须指向**同一** Redis（infra 单实例部署契约）；分库部署会让门失效——部署清单 review 项。
- 30s 复查窗口内旧 token 仍可短暂收推送（有界暴露，契约已注释）；缩窗需以 Redis QPS 为代价，当前取 30s。**表述修正（R4 复审）**：30s 界定的是「停止投递 + 发起关流」，不是流句柄消亡；句柄物理回收由 gRPC keepalive/max_connection_age(15m)+grace 与 Envoy max_stream_duration(1h) 有界收敛。修复前的准确表述是「已建立的流不能随会话失效及时关闭」（修复前另有 Envoy 1h 流寿命上界，非字面「无限期」）。
- 关闭条件：验收矩阵全绿 + 生产产物 dbcheck/发布门禁通过 + 观察窗口无复发。
