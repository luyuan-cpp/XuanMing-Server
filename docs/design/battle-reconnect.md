# Battle DS 断线重连(登录直连)

> 玩家已匹配进入 battle DS 后掉线,重新登录应直接回到那场对局的 battle DS,而不是被丢回大厅。
> 本文记录该能力的设计与落地(服务级决策,CLAUDE.md §5/§7)。

## 1. 问题与定性

**现象**:玩家匹配成功、进入 battle DS 后网络掉线,重新登录只拿到 Hub DS 地址,被送回大厅,原对局对他而言"消失"。

**定性**:这是**已知设计缺口(gap),不是 bug**。零件都在,只是没接进登录链路:

- `player_locator` 已有 `LOCATION_STATE_BATTLE` 态,且 `battle_pod` 字段存的就是 **battle DS 地址**(matchmaker 成局时用 `ds_addr` 写入,唯一标识 DS)。
- matchmaker 已能为重连/换设备的玩家现签新 battle 票(`GetMatchProgress(match_id=0)` 重连兜底 + `SignBattleTicket`),但仅覆盖 **进战斗前**(READY 及之前)。
- **login 不查玩家当前位置**,无论玩家在不在战斗中都只返回 Hub 地址。
- **BATTLE 位置只在成局时写一次**,locator TTL 默认 30s,整局(最长 `BattleTTL`)期间无人续期 → 30s 后过期,长对局根本查不到。

## 2. 方案(选定)

**登录时检测 BATTLE + ds_allocator 心跳续期 BATTLE 位置**,两处协同改动:

### 2.1 login 侧:登录检测 BATTLE 直接下发重连信息

`LoginUsecase.Login` 鉴权成功后,调 `player_locator.GetLocation(playerID)`:

- 若 `state == BATTLE && match_id != 0 && battle_pod != ""`:
  1. 经 login 的统一 `BattleTicketAuthorizer` 从 Redis live roster（Redis authority 下再核完整
     active/projection tuple）证明 player 属于 match，并取得同一快照中的权威 `ds_addr`，然后现签
     一张**新 jti** 的 battle DS 票；不得再使用 locator 中可能属于旧 UID 的地址；
  2. `LoginResponse` 返回 `battle_ds_addr = 本次 roster authority 快照的 ds_addr`、
     `battle_ticket`、`match_id`;
  3. **跳过 hub 分配**(不调 `AssignHub`)与 **`NotifyLoginPending`**——避免把 BATTLE 位置顶成
     LOGIN_PENDING / HUB,把玩家从战斗里拉出来。
- locator 已明确 `BATTLE` 后，authorizer/Redis/签名失败返回 `Unavailable`，不得继续 Hub 链。
- 否则(不在战斗)走原有 hub 流程,`battle_*` 字段留空。

**客户端契约**:`LoginResponse.battle_ds_addr` 非空 → 直连 battle DS 重连;为空 → 走 hub。
battle DS 已结束但位置/票据尚未清理时,客户端连 battle DS 会被拒；`IssueDSTicket(hub)` 的 service
路径确会重新调用 Hub allocator 并返回地址+票，但在 §7.16.3 placement 最终门完成前，它不能证明玩家
已权威离开 Battle，不得把这条回退描述成一人一 DS 的安全闭环。

### 2.2 ds_allocator 侧:心跳续期玩家 BATTLE 位置 TTL

battle DS 每 5s 调 `ds_allocator.Heartbeat`。心跳成功且对局处于 `ready/running` 时,
ds_allocator 从 Redis 镜像 `BattleStorageRecord` 取 `player_ids` + `ds_addr`,best-effort 刷新
每个玩家的 BATTLE 位置(`SetLocation state=BATTLE, match_id, battle_pod=ds_addr`)。

- DS heartbeat 与 locator refresh 持续成功时，BATTLE 位置在整局内续期，登录重连检测对长对局有效。
- **弱依赖**:locator 不可用只 Warn,不影响心跳与对局。
- 续期用**独立 detached ctx**(不随心跳 RPC ctx 取消),fire-and-forget,不给心跳响应加尾延迟。
- 对局进入 `ended/abandoned` 后心跳走终态分支不再续期,位置约 30s 后自然过期
  (给赛后短窗重连留余量,过期后客户端连不上自动回大厅)。

### 2.3 降级语义:查询失败 ≠ 不在战斗(B1 Login 已 fail-closed)

两种结果必须区分:

| 结果/配置 | 含义 | 行为与安全性 |
|---|---|---|
| locator 返回明确 HUB/LOGIN_PENDING 等非 Battle 状态 | 玩家当前不在战斗 | 进入 Hub 链;B1 下先成功写 LOGIN_PENDING,再 AssignHub |
| locator 返回空记录/不完整 Battle 记录 | 可能已离线,也可能是 best-effort 续租连续失败 | 当前也映射成 `!InBattle` 并走 Hub;**未闭环**,见 §7.3 J |
| locator 查询失败/未配置 + B1 开启 | 玩家真实状态**未知** | 返回 unavailable;Hub assigner/seat/ticket/LOGIN_PENDING 均不得产生 |
| locator 查询失败 + B1 关闭 | 本地/off profile 的显式可用性降级 | 可能误进 Hub,不能证明一人一 DS,不得用于生产安全结论 |

2026-07-14 状态:`require_hub_assignment_binding=true` 已实现“未知即拒绝”,Redis authority 的集群配置
生成器会开启该门;对应负例测试断言 Hub assigner 零调用。查询使用有界重试,瞬态恢复后仍可返回 Battle。

该修复只覆盖 `Login`。`IssueDSTicket(hub)` 仍可绕过 locator/B1 直接 AssignHub,Hub admission 也尚未核
placement,所以整条 DS 迁移仍未闭环;见 §6.2、§7.3 A 及
`decision-revisit-ds-callback-auth.md` §7.16.3。locator 的 BATTLE fence 只能防止值被覆盖,不能撤销已经
产生并可准入的 Hub assignment/ticket。

### 2.4 不变量合规(CLAUDE.md §9)

- **§17 零停机 / pb 兼容**:`LoginResponse` 只**新增字段**(编号 8/9/10),不改编号/类型/语义。
- **§16 不停服更新**:不引入任何"必须停服"依赖;新老副本同时在线时,旧 login 副本不填
  battle 字段(客户端回退 hub),新副本填——双向兼容。
- **§14 客户端只拿最小视图**:只回 `battle_ds_addr/battle_ticket/match_id`,不外露 `StorageRecord`。
- **§1 一人一 DS**:B1 Login 对 active/unknown placement 均 fail-closed,且 BATTLE→BATTLE 只接受同
  match 续期；但 `IssueDSTicket(hub)`/Hub admission 旁路仍是 §7.3 A blocker,本节不得宣称完整满足。
- **§11 业务 ID uint64**:`match_id` 为 uint64。

## 3. 落地清单

| 位置 | 改动 |
|---|---|
| `proto/pandora/login/v1/login.proto` | `LoginResponse` 加 `battle_ds_addr=8` / `battle_ticket=9` / `match_id=10` |
| `services/account/login/internal/data/locator_client.go` | `LocationNotifier` 加 `GetLocation`;实现查询 |
| `services/account/login/internal/biz/login.go` | `Login` 检测 BATTLE → 经统一 roster issuer 签票；明确 InBattle 后失败即 Unavailable，跳过 hub / login-pending |
| `services/account/login/internal/service/login.go` | 映射新字段到 `LoginResponse` |
| `services/battle/ds_allocator/internal/biz/allocator.go` | `Heartbeat` 成功后续期玩家 BATTLE 位置(`LocationRefresher` 弱依赖) |
| `services/battle/ds_allocator/internal/data/locator_client.go` | 新增 locator 客户端实现 `LocationRefresher` |
| 两个 `cmd/.../main.go` | 注入 locator 依赖 |

## 4. 被否方案

- **专门 `BattleReconnect` RPC**:多一次往返 + 客户端多一步状态机;`LoginResponse` 本就是
  "立即完成型必须含完整业务数据"(protocol-ordering 原则 1),直接塞进登录响应更简洁。
  留作未来精细化(如需要重连专属鉴权 / 二次校验成员名单)的空间。
- **调大 locator 全局 TTL**:BATTLE 位置续期问题不该用放大全局 TTL 解决(会拖长离线判定、
  放大好友在线态误差),用心跳精确续期更干净。

## 5. 严重 bug 记录:LOGIN_PENDING 顶掉 active BATTLE(一人两处)

> 级别:**严重**(破坏不变量 §1「玩家只能在一个 DS」)。发现于本次 battle-reconnect 评审,
> 由"客户端定时重登"设想暴露。**已修复**(见 §5.3)。

### 5.1 根因

`player_locator` 的状态机守卫 `guardTransition`(`services/runtime/player_locator/internal/biz/locator.go`)
原本**只守卫 HUB 上报**——开头即 `if in.State != LocationStateHub { return nil }`,把所有
控制面写(`LOGIN_PENDING` / `MATCHING` / `BATTLE`)**无条件顶号放行**。

W4⑪ 的 BATTLE fence 当初只堵了"stale hub DS 把玩家从战斗顶回大厅",因为那时 login 还没有
重连逻辑、重登必然经过 hub。**本次新增 login 重连后**,重登在"未检测到战斗"(locator 抖动/
查询失败降级)时会调 `NotifyLoginPending` 写 `LOGIN_PENDING`,而这条路径 guard **从未设防**。

### 5.2 触发时序(一人两处)

前提:玩家正打 match X,locator = `BATTLE(match_id=X)`,ds_allocator 每 5s 续期。玩家掉线,
客户端**每秒重登一次**。只要有一次重登恰好撞上 locator 抖动:

```
T0.0  重登 #N → login.GetBattleLocation → locator 抖动返回 err
T0.1  login 降级走 hub 分支 → NotifyLoginPending
T0.1  locator 写 LOGIN_PENDING,guard 放行 → BATTLE 被冲成 LOGIN_PENDING   ← BUG
T0.3  matchmaker 读到该玩家 = LOGIN_PENDING(空闲)→ 放行进匹配队列
      → 玩家既在 match X 的 battle DS,又进新匹配 → 一人两处,破坏 §1
T5.0  ds_allocator 心跳把 locator 改回 BATTLE(但抖动窗口已被利用)
```

重登频率越高(每秒),撞上 locator 抖动的概率越大,`BATTLE↔LOGIN_PENDING` 抖动窗口越频繁。
login 侧 §2.3 的短重试能**抑制**(拉高首查成功率、走 battle 分支不写 LOGIN_PENDING),
但抑制 ≠ 根除;把重试全交客户端猛重登会放大触发概率。**根因在 locator guard,必须在 locator 修。**

### 5.3 修复:BATTLE fence 扩展到"非对局写一律拒"

`guardTransition` 在 `cur.State == BATTLE` 时,只接受两类写,其余(含 `LOGIN_PENDING`)一律
`ErrLocatorConflict`:

1. **对局生命周期控制面写**:`BATTLE` 同 match 心跳续期 / 推进(不同 match_id 视为迟到旧写被拒)、`MATCHING`(下一局撮合决策);
2. **带正确 `match_id` 令牌的 HUB 回流**(玩家打完回大厅,W4⑪ 原逻辑)。

裸登录 / 断线重登降级写的 `LOGIN_PENDING` 无对局上下文,落入拒绝分支 → **再也顶不掉 active BATTLE**。

**为何安全**:
- 不误伤正常重连——login 检测到 BATTLE 走重连分支,**根本不调 NotifyLoginPending**;
- 不卡 liveness——历史/local-off 路径的 `NotifyLoginPending` 失败只 Warn;当前 B1 Login 在该写失败时
  fail-closed 返回 unavailable,待 locator 恢复后重试。对局真结束后心跳停续,BATTLE 位置约 30s 过期;
- 权威出口不受影响——matchmaker 写 MATCHING/BATTLE、hub DS 带令牌上报 HUB 两条合法路径照常放行;
  不同 match_id 的迟到 BATTLE 写会被拒,避免旧对局心跳覆盖新对局位置。
  "一次裸登录"本就不该有权终止一场进行中的战斗。

修复后:客户端**无论不重登、还是每秒猛重登**,都不会把玩家顶出战斗 → 可放心把重试压力交给
客户端 timer(见 §2.3)追求 login 吞吐。

### 5.4 落地

| 位置 | 改动 |
|---|---|
| `services/runtime/player_locator/internal/biz/locator.go` | `guardTransition`:`cur==BATTLE` 时非对局写(`LOGIN_PENDING` 等)拒绝顶号,且 `BATTLE→BATTLE` 必须同 match |
| `services/runtime/player_locator/internal/biz/locator_test.go` | 补测:`LOGIN_PENDING`/无令牌 `HUB` 遇 `BATTLE` 被拒;同 match `BATTLE`/`MATCHING`/带令牌 `HUB` 放行,不同 match `BATTLE` 被拒 |

### 5.5 遗留(次要,待评估)

`LOGIN_PENDING` 顶掉 `MATCHING`(确认期)同类洞仍在,但危害小(确认期短、掉线确认失败会
abandoned 补偿)。本次聚焦 BATTLE,MATCHING 保持"仅拦 stale HUB"现状,后续按需收紧。

## 6. 客户端对接契约(UE 仓库 Pandora-Client 实现)

> 后端已把重连所需数据全部塞进 `LoginResponse`,客户端**不自己判断在不在战斗**,严格照字段走。
> 所有安全性(不作弊 / 不一人两处)由服务端 fence 保证,客户端只负责"照字段连 + 便利重连"。

### 6.1 登录后按字段分流(必须)

`LoginResponse`(proto `pandora/login/v1/login.proto`)相关字段:

| 字段 | 号 | 含义 |
|---|---|---|
| `hub_ds_addr` / `hub_ticket` | 4/5 | 进大厅:地址 + hub JWT |
| `battle_ds_addr` | 8 | **非空 = 玩家在战斗中**,直连该 battle DS 地址 |
| `battle_ticket` | 9 | battle DS 握手用 JWT(新签,绑定 player_id + match_id) |
| `match_id` | 10 | 重连对局 ID(uint64),本地对账 / 显示用 |

**三字段要么全空、要么全填**;battle 字段非空时 `hub_ds_addr/hub_ticket` 必为空。分流伪码:

```cpp
if (!Resp.battle_ds_addr().empty()) {
    // 断线重连:直连 battle DS,握手带 battle_ticket
    ConnectBattleDS(Resp.battle_ds_addr(), Resp.battle_ticket(), Resp.match_id());
} else {
    // 正常进大厅(既有流程),握手带 hub_ticket
    ConnectHubDS(Resp.hub_ds_addr(), Resp.hub_ticket());
}
```

铁律:**battle DS 握手必须用 `battle_ticket`,不能用 `hub_ticket`**(票据类型不同,battle DS 会校验
`ds_type=="battle"` 且 `match_id` 匹配)。客户端不得凭本地状态自判走 hub 还是 battle,一切以字段为准。

### 6.2 直连 battle DS 失败后的权威重判(必须)

`battle_ds_addr` 非空但本次握手失败,**不能据此推断对局已经结束**,也不能无条件
`IssueDSTicket(ds_type="hub")`。客户端断线、票据过期、准入短暂失败时,Battle DS 仍可能健康并按整局
roster 续租玩家的 `BATTLE(match_id)` 位置。正确恢复顺序是:

1. 重新 `Login`(或调用等价的权威 placement 查询)取得当前路由和新票。
2. 返回 battle 三字段时,继续连接该 Battle DS;不得同时签发/使用 Hub 票。
3. 只有服务端明确判定已非 BATTLE,或显式离局事务已经用 `match_id` fence 完成
   `BATTLE → HUB_PENDING/HUB`,才允许签发新的 Hub 地址和一次性票据。

`IssueDSTicket(hub)` 自身也必须执行同一 placement 门,不能把安全性只寄托在调用方。当前实现尚未满足,
见 §7.3 阻断项 A。

### 6.3 断线重连 timer(建议,提升体验)

掉线后定时**重新登录**(重登 = 再调 `Login`),直到某次 `LoginResponse` 带回 `battle_ds_addr` 就连回去:

- **指数退避**:1s → 2s → 4s,封顶 ~8–10s。**禁止定长每秒**(防登录风暴)。
- **前台重试提示窗口 ~30s**;它只是客户端体验阈值,不是 BATTLE 权威状态的 TTL,超时后按 §6.3.1
  重新判定去向。
- **幂等**:`Login` 可安全重复调(同 account 稳定 player_id)。
- **只重试 Login 才是安全入口**:B1 模式会按 locator+roster 判定 Battle/Hub;客户端不得在本地超时后
  自行把目的地改成 Hub。

#### 6.3.1 重连超时后如何真回到大厅(必须)

30s 只是客户端本地计时。若 Battle DS 健康,它仍会心跳并按 roster(包含暂时掉线者)维持
`BATTLE(match_id)`;只有 **Battle DS 业务心跳**停止约 15s 才会触发 allocator abandon。两者不能混为一谈。

超时后的标准流程:

1. 降低重试频率并显示可取消的恢复 UI,再次 `Login` 读取权威路由。
2. `LoginResponse` 仍带 battle 三字段:用新票继续回原局。
3. battle 三字段为空:使用同一响应的 Hub 地址/新票回大厅。
4. 若产品允许玩家主动放弃,必须先由服务端执行带 `match_id` fence 的显式离局/结算事务;事务成功后
   才能 `IssueDSTicket(hub)`。仅在客户端本地停止 timer 不算离局。
5. 所有 in-flight Login/IssueTicket 回调必须按 recovery generation 丢弃迟到结果,避免 Battle/Hub 两次
   `ClientTravel` 竞态。

当前客户端超时后直接 `IssueDSTicket(hub)`;当前服务端该接口又未检查 active BATTLE,因此本节是
**目标契约,不是已完成事实**。

### 6.4 客户端不需要改的

- **proto**:后端已 regen;cpp pb 同步到 UE `Source/Pandora/Generated/Proto/` 由 Codex 执行,
  客户端不手改 proto,只是 regen 后多出 `battle_ds_addr/battle_ticket/match_id` 三个可读字段。
- **鉴权 / 连接框架**:battle_ticket 与 hub_ticket 同一套 JWT 握手机制,走现有通道即可。

### 6.5 UE 侧落地清单(交接给客户端窗口)

1. `LoginResponse` 处理:按 §6.1 分流(battle_ds_addr 非空 → 连 battle,否则连 hub)。
2. battle DS 握手改用 `battle_ticket`;透传 `match_id` 供 HUD / 重连对账。
3. 直连 battle 失败 → §6.2 权威重判;active BATTLE 继续回原局,不得无条件切 Hub。
4. 断线重连 timer:§6.3 指数退避 + 前后台恢复 + generation 防迟到回调;30s 只升级 UI,不改权威去向。
5. 老版本兼容:字段为空时行为与今天完全一致(纯进大厅),无需为兼容做额外分支。

## 7. 全链路断线/切后台审计:任意时间点掉线会不会卡死(2026-07-14 复核)

> 审计问题:玩家在「登录 → 选角 → 进 Hub DS」或「匹配 → READY → 进 Battle DS」的
> **任意时间点**切后台、断网或杀进程,回来后能否自动/可操作地恢复,并且最终只进入一个权威 DS?
>
> **当前结论:未通过,代码尚未全部做完。** 正常重登、服务端清理/幂等和多数故障恢复机制已经存在,
> 但下面 A~J 有可复现的静态代码反例,真实 UE 任意时点故障注入 E2E 也未完成。不得再宣称
> “每个断点都不会卡死”或“最终一定能切进 DS”。

这里的“不卡死”必须同时满足:恢复动作有界、切后台再前台后继续推进、杀进程后可由服务端权威态恢复、
失败后存在可见且可重复的入口、迟到回调不触发相互竞争的 travel,以及任何时刻最多只有一个可准入 DS。

### 7.1 逐断点审计表

| # | 故障注入点 | 已有机制 | 复核状态 |
|---|---|---|---|
| 1 | Login / SelectRole 请求在飞时断网或挂起 | UE 正常 HTTP backend 有连接/活动超时并走完成回调;模型回调会复位 in-flight | **部分**:项目没有前后台恢复接线和黑洞/挂起 Automation;`SendUnary` 也未检查 `ProcessRequest()` 返回值,不能用“~30s 必回调”作全平台绝对证明 |
| 2 | Hub `ClientTravel` / PreLogin / Admission 中断 | Hub 连接失败会复位 `ConnectingTarget`;任意新 World BeginPlay 再兜底复位;Hub reservation/session ledger 防重复占座 | **部分**:静态恢复路径存在,缺真实 UE UDP/Admission 故障注入 |
| 3 | 已在 Hub 后断线 | locator TTL/fence、Hub reservation/session ledger 有单测 | **部分**:真 UE Logout/Admission response-loss E2E 仍缺 |
| 4 | StartMatch 三步写任一点请求取消 | ticket body → player claims → queue 的顺序避免正常并发双票 | **未通过,见 G**:回滚复用已取消 ctx,可留下“有 body/claim、无 queue”的 30min 幽灵票 |
| 5 | 最后一人 Confirm 后断线/切后台 | ALLOCATING 有 60s 扫描宽限,allocator/失败路径有部分单测 | **未通过,见 G/H**:DS 分配和 READY finalize 仍绑玩家 RPC ctx;错误路径可把 ALLOCATING 移出恢复索引 |
| 6 | READY push 丢失或后台恢复 | push + 3s polling;轮询 READY 会刷新 Battle 票 jti | **未通过,见 C/G**:无 PC 时同地址永久去重;locator BATTLE 写和 push 也可能随最后确认者 ctx 取消 |
| 7 | 匹配驱动的 Battle 首次握手失败 | `FMyBattleConnectRetryPolicy` 对部分握手失败透明重试 | **部分**:有纯策略测试,没有跨 world/前后台真连接测试;耗尽后仍依赖重新登录恢复 |
| 8 | 登录驱动的 Battle 直连失败 | P0(2026-07-14)已改权威重查循环;服务端 Hub 签票门已封旁路 | **部分,见 A/J**:签票旁路已封,但 admission 最终门与 TTL 缺口待候选 B |
| 9 | Battle 局内掉线 | GameInstance 级 timer 指数退避 Login;服务端按 roster 保持 BATTLE 并现签重连票;P0 后超窗口只升级 UI 不自切 Hub | **部分,见 F/J**:迟到回调仍可竞争 travel;locator key 缺失仍可误判 Hub |
| 10 | Battle 中杀进程再启动 | Login 在 locator BATTLE 时再核 roster并直回原 Battle | **入口部分覆盖,出口未闭环,见 D/J**:locator 缺失会漏判;直连分支未恢复 MatchModel 上下文 |
| 11 | 结算、回 Hub、再次匹配 | 正常链会带 `fence_match_id`;battle terminal release 有持久 outbox | **未通过,见 D/E/I**:客户端回 Hub 不可安全重试;match claim 释放仍是 best-effort,失败可挡住新匹配 30min |
| 12 | push stream 本身断开 | 匹配阶段多数时候可由 polling 兜底 | **残余**:`OnStreamClosed` 无自动重订;非轮询消费者依赖重登/重进 Hub |

### 7.2 已确认存在的恢复基础

1. **Battle-aware Login 的已覆盖部分**:B1(`require_hub_assignment_binding=true`)下 locator 查询报错会
   fail-closed;返回 active BATTLE 后再核 roster并现签 Battle 票,后续任一步失败都不会继续 AssignHub。
   locator key 正常缺失的权威性仍是 J 的缺口。
2. **Hub 容量/准入账本**:assignment、reservation、session 幂等复用和过期释放已有并发/故障单测。
3. **匹配服务端已有自愈零件**:入队 liveness、部分 stale claim、确认超时、ALLOCATING grace、
   空局/DS 心跳丢失及 terminal outbox 均有超时、幂等或补偿路径;但 G~I 证明恢复索引/持久化仍不完整。
4. **票据刷新**:READY polling 与 Battle 重登录都会现签新 jti,不要求复用已消费/已过期票。
5. **locator fence**:stale Hub logout 不能覆盖 MATCHING/BATTLE;正常结算路径把 `match_id` 作为
   `fence_match_id` 交给 Hub 更新位置。

这些机制证明“主路径有恢复骨架”,不等于证明“任意时间点绝不卡死”。

### 7.3 当前阻断项(必须完成后才能改成“已闭环”)

**A. `IssueDSTicket(hub)` 绕过 active-BATTLE 门。** login service 的 hub 分支直接
`ResolveHubEndpoint → AssignHub`,不执行 Login 的 locator/B1/`NotifyLoginPending` 顺序,也不核
`match_id`。Hub admission 只核 assignment/DS credential;UE Hub 又在放开 spawn gate 后 best-effort
`SetLocation(HUB,fence=0)`,失败仅记日志。于是玩家可以物理进入 Hub,locator 却仍是 BATTLE,形成双归属并
导致后续重登/匹配异常。必须把 placement/fence 校验放进签 Hub 票和最终 admission,不能靠客户端规避。

> **P0 止血已落地(2026-07-14;2026-07-15 Codex 复审修正)**:`ResolveHubEndpoint` 与 `SelectRole` 前新增
> `guardHubRouteAgainstActiveBattle` 显式三态权威门(`InspectBattleRoute`:ACTIVE →
> `ErrInvalidState` 零副作用拒绝;TERMINAL=投影记录显式 state ∈ {ended, abandoned} 且 match_id
> 一致,唯一放行路径;其余一切——roster 漂移/非成员 PermissionDeny、记录缺失、stale、错误——
> UNKNOWN 一律 fail-closed。复审前曾把通用 `ErrPermissionDeny` 当终态证明,roster 漂移会被误放行,
> 已修正),负例测试见 `login/internal/biz/hub_route_gate_test.go`(含 TOCTOU 并发终局切换、
> SelectRole 零 role 落库)。签票旁路已封;但 Hub 最终 admission 的
> placement/fence 最终门与 J 的 TTL 缺口仍待候选 B(已拍板,见 decision 文档 §7.16.3),本项未全闭环。
> 取舍:对局进行中的“主动退出回大厅”会被本门拒绝,需候选 B 的显式离局事务;正常结算不受影响。

**B. 30s 本地超时被误当成 Battle 已结束。** 客户端掉线不影响健康 Battle DS 的业务心跳;roster 仍含
该玩家,所以 locator 可在整局内保持 BATTLE。超时只能升级 UI/降低重试频率,不能直接改变 DS 去向。

> **P0 止血已落地(2026-07-14,客户端)**:`MyAccountModel` 删除 `FallbackToHubViaIssueDSTicket`;
> 窗口到期只触发一次 `OnBattleReconnectTimedOut`(可取消恢复面板)并降频到封顶间隔继续退避重登;
> battle 直连失败/无 PC 同样转权威重查;重连中收到无 battle 的 LoginResponse 视为服务端权威
> 大厅路由,接受并走正常登录分流;新增 `AbandonBattleReconnect` 供玩家主动放弃(回登录,不绕路由)。
> 待 UE 编译验证;J 的服务端 TTL 缺口仍待候选 B。

**C. READY 时无 PlayerController 会永久去重。** `HandleBattleReady` 在实际 `ClientTravel` 前先停 polling、
写 `PendingBattleDsAddr`;`ConnectToBattleDs` 无 PC 时只返回。之后相同 READY 被去重,World BeginPlay 也不
补发连接。必须只在成功发起 travel 后提交去重态,或保留可跨 world/foreground 的待连接任务。

**D. 重启后直连 Battle 的结算出口缺上下文。** AccountModel 的 battle Login 分支没有把
`match_id` 写回 MatchModel,也没有 Hub endpoint;`ReturnToHubDs` 因 `CurrentMatchId==0` 且 Hub 地址为空可
直接返回 false。恢复上下文必须来自权威 Login/placement,不能依赖进程重启前内存。

**E. 回 Hub 失败后不可安全重试。** `ReturnToHubDs` 在申请票前就 `ResetMatchState`;失败只停在当前关卡。
再次调用会用已清零的 `CurrentMatchId` 覆盖保存的 fence。必须把 recovery operation/fence 保持到 Hub
Admission ACK,并为 HTTP、无 PC、Travel/Admission 失败提供幂等重试。

**F. 迟到 RPC 可触发相互竞争的 travel。** UE `HttpTotalTimeout` 默认 0;30s 重连截止不会取消/失效仍在飞的
Login。客户端没有 recovery generation,Account/Match 还共享全局 `OnIssueDSTicketComplete` multicast。
因此 Hub ticket 回调与旧 Login(Battle 或 RoleSelect)可先后改场景。每次恢复操作必须有 epoch/cancel token,
回调只允许当前 epoch 提交一次目的地。

**G. Matchmaker 的持久状态推进绑在玩家请求 ctx。** `StartMatch` 的 body→claims→queue 三步写在失败时用
原请求 ctx 回滚;若 ctx 已取消,可留下未入队的 body+claim,liveness sweep 看不到,重试返回 AlreadyMatching。
最后一人 `ConfirmMatch` 又在同一 ctx 内同步 AllocateBattle、写 READY、写 locator、发 push;切后台/断线可在
已经 CAS ALLOCATING 后掐断 finalize。必须用独立有界 ctx + durable work item/worker,并扫描未入队 claim。

**H. ALLOCATING/超时恢复索引可丢。** allocator error 直接 `onMatchFailed` 而未先 CAS
`ALLOCATING→FAILED`;`failMatch` 会尝试移出 active。`expireOnce` 的 `UpdateMatchWithLock` 遇 Redis 瞬态错误
也直接 removeActive。记录若仍为 CONFIRM/ALLOCATING,后续无人扫描,客户端可一直看到旧阶段直到 30min TTL。
只有观测终态或成功移交 durable worker 后才能移除 active;瞬态错误必须保留并重试。

**I. 赛后 match claim 释放不可靠。** Matchmaker `ReleaseMatch` 吞掉 Redis 清理错误仍返回 nil;
battle_result 的调用也是 best-effort。若 DB 已提交而 release 失败,幂等重报命中 `alreadyRecorded` 后直接返回,
不再释放旧 claim,玩家回 Hub 后可被 `AlreadyMatching` 挡到 TTL。match release 必须进入与结算提交关联的
durable outbox,幂等 replay 持续修复到 ACK。

**J. locator 缺失不等于不在 Battle。** B1 已做到“locator 查询报错→拒绝”,但如果 DS→locator 的
best-effort 刷新连续失败到 key 正常过期,查询会成功返回非 BATTLE;Login 不会再查 live roster,仍可走 Hub。
需要权威 placement lease,或在非 BATTLE/缺失时用可证明的 roster/terminal 状态排除 active Battle;
Hub 签票与 admission 必须执行相同最终门。

### 7.4 必跑验收矩阵

| 注入阶段 | 最低通过条件 |
|---|---|
| Login、SelectRole 请求前/中/响应丢失 | foreground 后自动恢复或按钮可重试;所有 in-flight 有界释放;只产生一个 session/seat |
| Hub ticket 已签、UDP/PreLogin/Admission 任一点失败 | 不提前生成 Pawn/写 HUB;重登获得唯一新路由;旧 reservation 有界回收 |
| StartMatch body/每个 claim/queue 写入前后取消 ctx | 不留未入队 claim;重试可复用或清理原 operation;恢复扫描不只依赖 queue ZSET |
| CONFIRMING→ALLOCATING→READY 任一点取消/allocator error/Redis error | durable worker 独立完成或 CAS FAILED;active 索引不因瞬态错误丢失;客户端有界看到 READY/FAILED |
| READY 后切后台、换 world、暂无 PC | 待连接任务跨生命周期保留;恢复 PC 后继续 travel,或权威失败后明确重排 |
| Battle 首次握手、局内任一点断网/切后台 | active Battle 只发 Battle 票并回原局;绝不并发 Hub travel |
| 30s 后 Battle 仍健康 / 已崩溃两种分支 | 健康时继续 Battle;崩溃补偿完成后才进 Hub;两种状态都只有一个可准入 DS |
| Battle 中杀进程、重启、结算、回 Hub | `match_id`/fence 可由服务端恢复;Hub 票/Travel 任一步失败均可幂等重试 |
| Result DB commit 后 match release 丢包/Redis 故障/进程重启 | outbox 持续重试至旧 claim/票据/match 精确清理;玩家可立即开始新匹配 |
| push stream、HTTP、locator、Redis、DS 心跳分别断开 | 最多暂时不可用,恢复后继续推进;未知 placement fail-closed,无 Hub/Battle 双归属 |

验收必须包含真 UE 客户端的前后台/断网/杀进程自动化与真实 UDP Admission;现有纯策略/Go 单测不能替代。
