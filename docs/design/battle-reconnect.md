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
  1. 用 login 自己的 `auth.Signer` 现签一张**新 jti** 的 battle DS 票
     (`SignDSTicketWithCell(playerID, DSTypeBattle, matchID, regionID, cellID, jti)`);
  2. `LoginResponse` 返回 `battle_ds_addr = battle_pod`、`battle_ticket`、`match_id`;
  3. **跳过 hub 分配**(不调 `AssignHub`)与 **`NotifyLoginPending`**——避免把 BATTLE 位置顶成
     LOGIN_PENDING / HUB,把玩家从战斗里拉出来。
- 否则(不在战斗)走原有 hub 流程,`battle_*` 字段留空。

**客户端契约**:`LoginResponse.battle_ds_addr` 非空 → 直连 battle DS 重连;为空 → 走 hub。
battle DS 已结束但位置/票据尚未清理时,客户端连 battle DS 会被拒(票据 exp / DS 无此对局)→
回退调 `IssueDSTicket` 拿 hub 地址(既有"回大厅"路径),不会卡死。

### 2.2 ds_allocator 侧:心跳续期玩家 BATTLE 位置 TTL

battle DS 每 5s 调 `ds_allocator.Heartbeat`。心跳成功且对局处于 `ready/running` 时,
ds_allocator 从 Redis 镜像 `BattleStorageRecord` 取 `player_ids` + `ds_addr`,best-effort 刷新
每个玩家的 BATTLE 位置(`SetLocation state=BATTLE, match_id, battle_pod=ds_addr`)。

- 这样 BATTLE 位置在整局内不过期,登录重连检测对长对局也有效。
- **弱依赖**:locator 不可用只 Warn,不影响心跳与对局。
- 续期用**独立 detached ctx**(不随心跳 RPC ctx 取消),fire-and-forget,不给心跳响应加尾延迟。
- 对局进入 `ended/abandoned` 后心跳走终态分支不再续期,位置约 30s 后自然过期
  (给赛后短窗重连留余量,过期后客户端连不上自动回大厅)。

### 2.3 降级语义:查询失败 ≠ 不在战斗;hub 入口对账兜底

login 侧 BATTLE 检测是**尽力而为的快路径优化**,不是唯一保证。两种"降级走 hub"必须区分:

| 降级原因 | 含义 | 是否安全 |
|---|---|---|
| `!InBattle`(locator 权威返回) | 玩家确实不在战斗 | ✅ 正确,就该进大厅 |
| `GetBattleLocation` 查询失败 | **未知**玩家真实状态 | ⚠️ 若玩家其实在战斗,会"该跳没跳"误进 hub |

**为什么查询失败误进 hub 不会烂数据(但有 UX 缺口):**
即便误送 hub,hub DS 上报 `HUB` 带 `match_id=0`,locator 的 **BATTLE fence**(`player_locator`
的 `guardTransition`)会拒绝该上报;加上 §2.2 心跳一直续 BATTLE TTL,locator 里玩家仍是 BATTLE
(不变量 §1 保住)。坏处只是这一次登录玩家停在大厅、没回战斗——下次重登通常自愈。

**两层修法:**

1. **login 侧有界重试(已实现)**:`queryBattleLocation` 对可恢复的查询失败重试
   `battleLocationQueryRetries` 次(退避 `battleLocationQueryBackoff`)。只要任一次拿到 `InBattle`
   就照常跳去 battle;彻底挂了才降级。重试只发生在错误路径(罕见),不加正常登录延迟。
2. **hub 入口对账(权威兜底,UE hub DS 仓库)**:玩家 join hub 时查一次 locator,若为 `BATTLE`
   就回"去重连 battle"信号。这覆盖所有残余情况——login 查询彻底失败、竞态(login 之后才进战斗)、
   客户端拿着旧 hub 票据重连。**login 快路径 + hub 对账**共同构成完整的重连保证。

### 2.4 不变量合规(CLAUDE.md §9)

- **§17 零停机 / pb 兼容**:`LoginResponse` 只**新增字段**(编号 8/9/10),不改编号/类型/语义。
- **§16 不停服更新**:不引入任何"必须停服"依赖;新老副本同时在线时,旧 login 副本不填
  battle 字段(客户端回退 hub),新副本填——双向兼容。
- **§14 客户端只拿最小视图**:只回 `battle_ds_addr/battle_ticket/match_id`,不外露 `StorageRecord`。
- **§11 业务 ID uint64**:`match_id` 为 uint64。
- **§1 一人一 DS**:BATTLE→BATTLE 只允许同 match 续期(不同 match_id 的迟到写被 locator guard 拒);
  login 检测到 BATTLE 时不写 LOGIN_PENDING,不破坏单点。

## 3. 落地清单

| 位置 | 改动 |
|---|---|
| `proto/pandora/login/v1/login.proto` | `LoginResponse` 加 `battle_ds_addr=8` / `battle_ticket=9` / `match_id=10` |
| `services/account/login/internal/data/locator_client.go` | `LocationNotifier` 加 `GetLocation`;实现查询 |
| `services/account/login/internal/biz/login.go` | `Login` 检测 BATTLE → 签 battle 票、跳过 hub / login-pending |
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
- 不卡 liveness——`NotifyLoginPending` 失败在 login 只 Warn 非阻塞;对局真结束后心跳停续,
  BATTLE 位置 ~30s 自然过期,后续登录恢复正常;
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

### 6.2 直连 battle DS 失败的回退(必须)

`battle_ds_addr` 非空但连不上(对局刚结束 / 票据过期 / DS 已回收)时:调 `IssueDSTicket(ds_type="hub")`
拿"当前有效"的 hub 地址回大厅(见 proto `IssueDSTicketResponse.hub_ds_addr`),**不要**用登录时缓存的
旧 hub 地址(Hub DS 可能被 Agones 重建 / 换端口)。

### 6.3 断线重连 timer(建议,提升体验)

掉线后定时**重新登录**(重登 = 再调 `Login`),直到某次 `LoginResponse` 带回 `battle_ds_addr` 就连回去:

- **指数退避**:1s → 2s → 4s,封顶 ~8–10s。**禁止定长每秒**(防登录风暴)。
- **总窗口 ~30s**(对齐 battle 位置 TTL)+ 赛后短窗;超时则停 timer,走 §6.3.1 回到大厅。
- **幂等**:`Login` 可安全重复调(同 account 稳定 player_id)。
- **发不发都安全**:不发只坑自己(battle DS 15s 心跳超时判 `abandoned` + 段位回滚);猛发也不作弊
  (服务端 §5 fence 挡住,不会一人两处)。故 timer 是纯"诚实玩家便利",非安全依赖。

#### 6.3.1 重连超时后如何真回到大厅(必须)

超时时玩家在服务端**已非 BATTLE 态**(battle DS 15s 心跳超时判 `abandoned` + 段位回滚,§4),
locator 里的 BATTLE 落点已清。**"回到大厅"= 复用 §6.1 的大厅分流那条路,只是触发时机变成重连超时后。**
UI 提示和实际连接**必须同时做**:先发起下面的连接,连上大厅后再切场景 / 关提示,**不要只弹文字不连接**
(那才会卡黑屏)。

**标准做法(路径 A,session_token 仍有效)**:停 timer 后不再重登 battle,直接
`IssueDSTicket(ds_type="hub")` 拿"当前有效"的 `hub_ds_addr` + `hub_ticket` → `ConnectHubDS(...)` → 弹
"已离开对局,回到大厅"UI。这是最快路径(免走完整登录),客户端优先走此路。

```cpp
// 重连 timer 超时(标准路径 A)
auto R = IssueDSTicket(/*ds_type=*/"hub");   // R.hub_ds_addr / R.ticket
ConnectHubDS(R.hub_ds_addr(), R.ticket());
ShowLeftMatchReturnedToHubUI();               // 连上大厅后再切场景 / 关提示
```

**兜底(路径 B,仅当 session_token 已失效 / `IssueDSTicket` 返回鉴权错)**:再调一次 `Login`;此时
服务端已非 BATTLE → `LoginResponse.battle_ds_addr` 为空 → 走 §6.1 的 `ConnectHubDS(hub_ds_addr, hub_ticket)`。
仅作安全网,正常不会走到。

### 6.4 客户端不需要改的

- **proto**:后端已 regen;cpp pb 同步到 UE `Source/Pandora/Generated/Proto/` 由 Codex 执行,
  客户端不手改 proto,只是 regen 后多出 `battle_ds_addr/battle_ticket/match_id` 三个可读字段。
- **鉴权 / 连接框架**:battle_ticket 与 hub_ticket 同一套 JWT 握手机制,走现有通道即可。

### 6.5 UE 侧落地清单(交接给客户端窗口)

1. `LoginResponse` 处理:按 §6.1 分流(battle_ds_addr 非空 → 连 battle,否则连 hub)。
2. battle DS 握手改用 `battle_ticket`;透传 `match_id` 供 HUD / 重连对账。
3. 直连 battle 失败 → §6.2 回退(重新 `IssueDSTicket(hub)` 拿新地址回大厅)。
4. 断线重连 timer:§6.3 指数退避 + 30s 总窗口 + 超时兜底 UI。
5. 老版本兼容:字段为空时行为与今天完全一致(纯进大厅),无需为兼容做额外分支。
