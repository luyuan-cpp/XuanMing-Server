# Pandora Go 服务清单与契约

> 16 个 go 服务的职责边界、对外接口、关键状态、依赖矩阵。
>
> ⚠️ **2026-06-04 架构终版**:
> - 框架统一 **Kratos**(替代 go-zero,详见 `gateway-decision.md` §4)
> - Edge Gateway 用 **Envoy**(替代之前规划的 pandora-gateway 自研)
> - 推送 = **集中 push 服务 + gRPC server stream**(替代之前规划的自研 WebSocket)
> - 客户端协议:**gRPC-Web over HTTP/2 TLS**(UE FHttpModule + 自研协议解析)

## 1. 服务总览

| # | 服务 | gRPC 端口 | 状态性 | 主要存储 | 主要消费 kafka | 骨架状态 |
|---|---|---|---|---|---|---|
| 1 | login | 50001 | 无 | mysql + redis | (生产 login.event) | ✅ W2 ③(mock,W3 接 mysql/redis) |
| 2 | player | 50002 | 无 | mysql + redis | player.update | ✅ W4 ④(MMR 写回 + GetMMR reader) |
| 3 | data_service | 50003 | 无 | mysql + redis | (写穿层) | ✅ 2026-06-16(player_data 版本化 blob + cache-aside 网关,内网) |
| 4 | friend | 50004 | 弱(friend.event 推送) | mysql | pandora.friend.event | ✅ 2026-06-15(好友请求/接受/列表/拉黑 + locator 在线状态) |
| 5 | chat | 50005 | 弱 | mysql(私聊历史)+ kafka | chat.{world,team,private} | ✅ 2026-06-16(三频道 + 内容校验 + 私聊落库 + team fan-out) |
| 6 | player_locator | 50006 | 强 | redis | locator.update | ✅ W3 ⑤(W4 ⑦ matchmaker 上报 MATCHING/BATTLE) |
| 7 | team | 50010 | 强 | redis | - | ✅ W3 ⑦ |
| 8 | matchmaker | 50011 | 强 | redis | (生产 match.found) | ✅ W4 ①(W4 ⑦ 接 locator 串 MATCHING/BATTLE) |
| 9 | trade | 50012 | 强 | redis | trade.audit | ✅ 2026-06-16(两阶段确认订单状态机 + 乐观锁 + 结算幂等键 + 审计) |
| 10 | dialogue | 50013 | 无 | 配置驱动(内存,留 mysql hook) | - | ✅ 2026-06-16(配置对话树 + 内存会话状态机 Start/Choose/End) |
| 11 | ds_allocator | 50020 | 弱 | redis (+k8s) | (生产 ds.lifecycle) | ✅ W4 ②(Mock 分配器,W4 ③ 发 abandoned,W4 ⑧ abandoned 可靠补偿,W4 ⑫ 真 Agones REST allocator) |
| 12 | hub_allocator | 50021 | 弱 | redis (+k8s) | (生产 ds.lifecycle) | ✅ W4 ⑤ + 自动扩缩容(2026-06-15:按在线人数控 Agones Fleet 副本) |
| 13 | battle_result | 50022 | 无 | mysql | battle.result + ds.lifecycle | ✅ W4 ③(幂等落库 + Elo MMR + abandoned 补偿),W4 ⑨(player.update 事务出箱可靠化) |
| 14 | **push** ⭐ | **50014**(gRPC server stream) | 强(连接索引) | redis(离线消息)| pandora.{team,match,chat,player,friend,system}.* | ✅ W2 ⑤(mock 5s tick,W3 接 kafka) |
| 15 | inventory | 50015 | 无 | mysql(pandora_trade) | - | ✅ W5 ③(大厅背包:货币+可堆叠道具,用/售/授予,ledger 幂等) |
| 16 | auction | 50016 | 强(per-market 串行撮合) | redis(订单簿)+ mysql(pandora_auction 权威) | (生产 auction.match/audit) | ✅ 2026-06-19(全服拍卖行/撮合引擎,两层幂等,ZSET 价格-时间优先) |

⭐ = 2026-06-04 终版新增。push 是 Kratos transport/grpc 暴露的 server stream 服务,客户端通过 Envoy 连过来,详见 `gateway-decision.md` §6。

**Edge Gateway = Envoy**(端口 8443 HTTPS),不是 go 服务,不计在表格内。**状态**:✅ W2 ④ 落地(v1.38.0 docker,login_cluster + push_cluster + grpc_web/cors/router filters,详见 `PROGRESS.md` W2 ④ 段)。

## 2. 各服务详细契约

### 2.1 login

**职责**:
- 账号注册 / 登录 / 登出
- 颁发 Session Token(给客户端)
- 颁发 DS Ticket(JWT,给 UE DS)
- 验证 DS Ticket(防重放,jti 黑名单)

**对外 RPC**:
```
Login(account, password_hash, device_id) → session_token + hub_ds_addr + hub_ticket
Logout(session_token) → ok
IssueDSTicket(session_token, ds_type, target_id) → ticket
VerifyDSTicket(ticket, ds_pod_name) → player_id + claims
```

**不该做的事**:
- ❌ 不存玩家档案(那是 player 服务)
- ❌ 不算 MMR
- ❌ 不广播大厅状态

**依赖**:
- 上游:客户端、UE DS(只用 VerifyDSTicket)
- 下游:hub_allocator(给 hub_ds_addr)、player(查档案是否存在)

---

### 2.2 player

**职责**:
- 玩家档案(昵称、头像、等级、段位)
- 英雄解锁记录
- 皮肤记录
- MMR 读写(写由 battle_result 调)

**对外 RPC**:
```
GetProfile(player_id) → PlayerProfile
UpdateNickname(player_id, nickname) → ok
ListHeroes(player_id) → []hero_id
UnlockHero(player_id, hero_id, source) → ok
GetMMR(player_id) → mmr
UpdateMMR(player_id, delta, reason, idempotency_key) → new_mmr
```

**关键不变量**:
- `UpdateMMR` 必须**幂等**(idempotency_key = match_id),防重复扣段位
- 所有读优先走 redis 缓存(5min TTL)

---

### 2.3 data_service

**职责**:
- **玩家数据统一读写网关**(保证 cache + db + kafka 三处一致)
- 缓存失效广播

**对外 RPC**:
```
ReadPlayer(player_id) → cached or db
WritePlayer(player_id, fields, version) → new_version  // 乐观锁
InvalidateCache(player_id) → ok
```

**关键设计**:
- **写流程**:DB 写成功 → kafka 发 update → 删 cache(cache-aside)
- **读流程**:cache 命中返回,miss 读 db 写 cache
- **乐观锁**:`UPDATE ... WHERE version = ?`,失败让上层重试

**为什么单独抽**:
- 玩家数据在多个服务读写(player / trade / battle_result),抽一层避免缓存不一致

---

### 2.4 friend

**职责**:好友 / 黑名单 / 拒绝列表

**对外 RPC**:
```
AddFriend(player_id, target_id) → request_id
AcceptFriend(player_id, request_id) → ok
RejectFriend(player_id, request_id) → ok
ListFriendRequests(player_id) → []FriendRequestInfo   // 待处理(收到的)请求
ListFriends(player_id) → []FriendInfo
RemoveFriend(player_id, target_player_id) → ok          // 删好友(双向,幂等)
Block(player_id, target_id) → ok
Unblock(player_id, target_player_id) → ok               // 取消拉黑(幂等)
ListBlocks(player_id) → []BlockInfo
```

**实现说明**：request_id 用 snowflake uint64（不变量 §9.11）；player_id 均以 JWT ctx 为准（R5）覆盖请求体。RejectFriend / RemoveFriend / Unblock 幂等；删好友不写黑名单(可重加)，取消拉黑不自动恢复好友关系(需重新加)。ListFriendRequests / ListBlocks 只回客户端可见结构(FriendRequestInfo / BlockInfo)，nickname 留空由客户端按 player_id 解析(§5.8)。

**2026-06-06 排期决策**（已提前）：friend 原定暂缓到最后。

**2026-06-15 实现**：按「补全 friend 模块」要求提前落地完整 Kratos 服务（第 11 个业务服）：好友图落 pandora_social（friendships / friend_requests / blocks），好友请求 / 接受经 kafka pandora.friend.event → push 推送给接收方，ListFriends 经 player_locator 填在线状态（弱依赖）。见 PROGRESS.md「社交域 ①」。

**2026-06-18 分布式好友图决策**：当前 friend 服务是**单 MySQL / 单库 `pandora_social` 方案**，`AcceptFriend` 能成立的前提是 `friend_requests`、`blocks`、`friendships` 三张表都在同一个 MySQL 实例内。当前实现依赖本地 ACID 事务：锁 `friend_requests` 行（`FOR UPDATE`）→ 同事务校验 block / 好友上限 → 标记 accepted → `INSERT IGNORE` 写双向 `friendships`。这只能保证单实例内的原子性和防 TOCTOU。

全区全服千万级玩家时，好友边会到十亿级，必须按 `player_id` 对好友图分库分表；此时 `requester` 与 `target` 的双向边大概率落在不同分片，当前跨玩家事务不再成立。把主存直接换成 Redis Cluster 也不能解决：`MULTI/EXEC` 和 Lua 只能操作同 slot key，`request:{request_id}`、`friends:{requester_id}`、`friends:{target_id}`、`block:{requester_id}`、`block:{target_id}` 会跨 slot；Redis 事务也没有逻辑回滚。因此禁止把当前 MySQL `FOR UPDATE` 事务原样搬到 Redis Cluster 或分片 MySQL。

目标形态采用 **request 单点权威 CAS + Kafka 异步幂等建边**：
- `friend_request` 是唯一权威实体，`pending -> accepted` 通过单行 / 单 key CAS 完成（MySQL `UPDATE ... WHERE status=pending` 或 Redis 单 key Lua 均可），谁 CAS 成功谁才算接受成功；重复 accept 天然幂等。
- CAS 成功后发布 `FriendshipEstablished(requester_id, target_id, request_id)` 事件，topic key 用业务实体 ID（优先 `request_id`，对齐 CLAUDE.md §9.9 的同实体有序）。
- 建边消费者分别按 owner 分片写 `requester -> target` 和 `target -> requester`，各自 `INSERT IGNORE` / `SETNX`，单分片幂等，失败重试。
- 好友上限改为软约束：建边时按 owner 本地校验，超限发补偿事件删边；或接受最终一致软上限。分片下无法强一致保证“双方同时不超限”，不引入 2PC / XA。
- block 校验也按 owner 分片本地执行；如果建边期间发现一侧已拉黑，发补偿事件清理另一侧边，补偿必须幂等。
- 好友图权威主存仍推荐分片 MySQL（按 owner `player_id` 分片），Redis 只做在线玩家 / 热好友列表缓存，不做十亿级好友边权威存储。

迁移边界：当前 W5 以内继续保留单 MySQL 事务实现；进入全服社交扩展前，必须先补 `friend` 的 outbox / 事件消费 / 补偿幂等键设计，再拆分 `AcceptFriend`。该迁移属于服务级架构改造，不允许只改存储连接串。

**2026-06-18 闭环补全**：补 RejectFriend / ListFriendRequests / RemoveFriend / Unblock / ListBlocks 五个 RPC，关闭"离线玩家无法处理请求(无待处理查询)、无法拒绝/删好友/取消拉黑"的功能缺口。RejectFriend 不向请求方推送(避免被拒尴尬)。存储切 TiDB,跨节点好友 A/B 仍由 Percolator 2PC 保证强一致(见 friend-distributed-scaling.md §8.5)。

---

### 2.5 chat

**职责**:频道(世界 / 队伍 / 私聊)

**对外 RPC**:
```
SendMessage(player_id, channel, content) → message_id
PullHistory(player_id, channel) → []ChatMessage
```

**实现**:
- 世界频道:kafka topic + 各 hub DS 消费下发
- 队伍频道:redis pub/sub
- 私聊:redis pub/sub + 离线 mysql

**反作弊**:消息内容服务端过敏感词,长度 ≤256

**2026-06-06 排期决策**:chat 不进入当前后端主线。push 可以继续保留 `pandora.chat.*` topic 和订阅模板,但 chat 服务本体等 UE 与所有核心功能完成后再做。

---

### 2.6 player_locator

**职责**:**玩家当前在哪**(hub_id / battle_id)

**对外 RPC**:
```
SetLocation(player_id, location)
GetLocation(player_id) → Location
ClearLocation(player_id)
```

**Location 状态枚举**:
```
LOCATION_OFFLINE
LOCATION_LOGIN_PENDING
LOCATION_HUB { hub_pod, shard_id }
LOCATION_MATCHING { match_id }
LOCATION_BATTLE { match_id, battle_pod }
```

**关键不变量**:
- 一个玩家**同一时刻只能在一个 Location**
- `HUB` 上报来自 hub DS,可能 stale;当前为 `MATCHING` 时拒绝覆盖。
- 当前为 `BATTLE` 时,`HUB` 回流上报必须携带刚结束战斗的 `match_id` 作 fence,且必须等于当前 `BATTLE.match_id`;通过后不持久化该 `match_id`。
- 所有 DS 上线 5s 内必须上报,否则 ds_allocator 视为僵死回收

---

### 2.7 team

**职责**:组队(5 人队)

**对外 RPC**:
```
CreateTeam(player_id) → team_id
Invite(team_id, target_player_id) → ok
AcceptInvite(player_id, team_id) → ok
LeaveTeam(team_id, player_id) → ok
Kick(team_id, target_id) → ok
SetReady(team_id, player_id, ready)
GetTeam(team_id) → Team 完整快照(只读)
GetMyTeam() → has_team_msg + Team 完整快照(只读;登录后进大厅时调一次,队伍主界面直接渲染;player_id 以 JWT 为准,查 pandora:team:player:<id> 索引;没队伍返 OK+has_team_msg=false;索引命中但队伍已过期/解散时按无队伍处理并清脏索引。带宽:一次性 unary,5 人队 ~200 字节,比拆两次 RPC 更省)
```
队伍状态变更推送走 kafka `pandora.team.update` → push 服务 server stream,**不提供** StreamTeamUpdates RPC。

**客户端同步约定(2026-06-15)**:
- `TeamUpdateEvent.team` 服务端已填充完整 `Team` 客户端可见快照,不是空信号;该快照来自 `TeamStorageRecord` 经 `recordToProto` 组装,不暴露存储侧字段。
- 常规队伍状态变更(`MEMBER_JOINED` / `MEMBER_LEFT` / `READY_CHANGED` / `CAPTAIN_CHANGED` / `DISBANDED` 等)客户端仍把 push 当"有变化"信号,收到后防抖合并调用 `GetMyTeam` 读取当前权威态,只在 `GetMyTeam` 回包路径写本地 `CurrentTeamSnapshot`。原因是 kafka → push → client stream 是 at-least-once 链路,可能重复、乱序或客户端处理时已过期;`GetMyTeam` 从 Redis 当前索引/队伍记录读取,并带脏索引清理逻辑,保证 UI 最终收敛到服务端权威态。
- `INVITE_SENT` 是例外:被邀请人此时还没入队,`GetMyTeam` 查不到这条邀请,客户端应直接读取 push 里的 `reason` / `invite_id` / `team` 展示邀请 UI。
- 客户端侧对 push 驱动的快照请求做短窗口防抖(当前 UE 为约 0.5s),避免多名队员同时变更或批量 push 时触发 `GetMyTeam` 请求风暴。5 人队完整 `Team` 约 200 字节,防抖后重拉一次 unary 成本很低,换来单一写入路径和抗乱序能力。

**状态机**:
```
FORMING → READY(全员 ready)→ MATCHING(进入匹配)→ IN_BATTLE → DISBANDED
```

**关键不变量**:
- 一人只能在一个队
- READY 状态下任意成员退出,自动回 FORMING
- DISBANDED 5min 后清理

---

### 2.8 matchmaker

**职责**:撮合 5v5

**对外 RPC**:
```
StartMatch(team_id) → match_id
CancelMatch(match_id) → ok
ConfirmMatch(player_id, match_id, accept) → ok
GetMatchProgress(match_id) → MatchProgress   # match_id 可为 0(重连兜底)
```
> 无 `StreamMatchProgress`:go-zero zrpc 不支持 server stream;进度变化经 kafka
> `pandora.match.progress`(key=player_id)推给 Hub DS 转 UE,客户端按需 `GetMatchProgress` 拉一次。

**核心算法**:
1. 按 MMR 分段
2. 同段位优先,等待时间长 → 放宽 ±200 MMR
3. 队伍合并(2+3 / 2+2+1 / 5)
4. 凑齐 10 → 进入确认期(15s,任一人拒绝)
5. 全员确认 → 调 ds_allocator → 推 ds_addr 给玩家

**关键不变量**:
- 同一玩家只能在一个 match 队列
- 确认期内有人拒绝 → 其他人退回队列(保留排队时长)
- **`GetMatchProgress` 鉴权以 JWT player_id 为准,`match_id` 不是授权凭证**
  (不变量 §14):`match_id`/`ticket_id` 是 Snowflake、非秘密,谁拿到都能传。
  服务端必须校验 caller 在该 match/ticket 成员里,否则一律按 `ErrMatchNotFound`
  返回(不暴露他人对局存在性),杜绝外挂用任意 ID 拉别人对局的双方名单 / DS 地址。
- **重连兜底**:`match_id=0` 时服务端用 JWT player_id 反查本人当前票据
  (`GetPlayerTicket`),解决重新登录 / 换设备丢句柄后拿不到自己进度;READY 阶段
  额外为本人现签新 battle DSTicket(新 jti,sub 锁定本人)。

---

### 2.9 trade

**职责**:玩家间交易(两阶段)

**对外 RPC**:
```
CreateOrder(seller_id, buyer_id, items, price) → order_id
ConfirmOrder(player_id, order_id) → ok
CancelOrder(player_id, order_id) → ok
ListMyOrders(player_id) → []Order
```

**两阶段流程**:
1. seller 创建 → status=PENDING
2. buyer 看到 → 确认 → status=BUYER_CONFIRMED
3. seller 再确认 → status=SELLER_CONFIRMED → 原子扣双方资源 → status=COMPLETED
4. 任一阶段超时(5min)→ status=EXPIRED

**关键不变量**:
- 资源扣减必须**原子**(redis lua + mysql 两阶段或 saga)
- 每步都写 trade.audit topic
- 失败回滚必须有补偿幂等 key

---

### 2.10 dialogue

**职责**:NPC 对话树运行时

**对外 RPC**:
```
StartDialogue(player_id, npc_id) → DialogueState
ChooseOption(player_id, dialogue_id, option_id) → DialogueState
EndDialogue(player_id, dialogue_id) → ok
```

**对话树存储**:当前最小版本对话树内联在 `dialogue-dev.yaml`(配置驱动,`ConfigTreeProvider` 内存只读);后续接配置中心 / mysql `dialogue_trees` 表(json blob)只换 `TreeProvider` 实现,biz/service 不动。

**会话状态机**:`StartDialogue` 服务端分配 `dialogue_id` 建会话 → `ChooseOption` 按 `option_id` 推进节点 → `EndDialogue` 关闭;当前 `MemorySessionStore` 单实例内存会话(`session_ttl` 默认 5m),多实例部署改 `SessionStore` 接 Redis,biz/service 不动。

**MOBA 早期**:简单 if-else 即可,不上行为树。对话选项当前无副作用(领奖励 / 改任务等留后续接 trade / player 服务,届时在服务端权威判定 `visible` 前置条件)。

---

### 2.11 ds_allocator

**职责**:战斗 DS 调度(Agones GameServer)

**对外 RPC**:
```
AllocateBattle(match_id, player_ids, map_id) → ds_addr + tickets
ReleaseBattle(match_id) → ok
Heartbeat(stream) → bidirectional
ListBattles(filter) → []BattleInfo
```

**实现**:
- 调用 Agones K8s API:`GameServerAllocation` CRD。W4 ⑫ 已实现标准库 REST allocator,
  `agones.enabled=true` 时经 k8s apiserver POST `allocation.agones.dev/v1` 分配,
  `enabled=false` 时保留 Mock fallback 供本地无集群联调。
- 维护 redis 中的 DS 状态镜像
- 心跳超时 15s → 标记 abandoned + 通知 battle_result(玩家段位回滚)

---

### 2.12 hub_allocator

**职责**:大厅 DS 分片调度

**对外 RPC**:
```
AssignHub(player_id, region) → hub_ds_addr + ticket
ReleaseHub(player_id) → ok
TransferHub(player_id, target_hub_id) → new_ds_addr
ListHubs() → []HubInfo
```

**实现**:
- Hub DS Fleet 常驻 N 个 pod,每个 200~500 人上限
- 新玩家进来 → 选最空 + 同 region + 队友所在 hub
- 队友所在 hub 已满 → 加入 hub waitlist 或换 hub

**自动扩缩容策略**(2026-06-15,`hub.autoscale_enabled=true` + `agones.enabled=true` 生效):
- 走 Agones Fleet 副本控制(读/改 Fleet `spec.replicas`),不引入 FleetAutoscaler CRD
- 开服默认拉起 `hub.min_replicas`(默认 1)个大厅
- 后台 reconcile(复用 `hub.sweep_interval`)按 `desired = ceil(total_players / players_per_hub)`
  **只扩不缩**,受 `hub.max_replicas`(默认 20)上限约束(`players_per_hub` 默认 500)
- 总在线人数为 0 → 回收到 `hub.min_replicas`(空大厅自动回收)
- `AssignHub` 遇分片全满(`ErrHubNoAvailable`)→ 立即兜底 `+1` 扩容,上游重试进新大厅
- 配置项:`hub.autoscale_enabled` / `players_per_hub` / `min_replicas` / `max_replicas`

**强制整合 + 玩家迁移通知**(2026-06-15,`hub.consolidation_enabled=true` 生效):
- 不再只「空大厅自动回收」,而是低负载时**主动排空人少的大厅 + 服务端权威搬迁玩家**
- reconcile 发现 ready 分片多于负载所需(`need = ceil(total/players_per_hub)`)→ 按负载升序
  把**最空的多余分片**标 `draining` 并盖 `draining_since_ms`,逐分片每 tick 最多搬
  `consolidation_batch`(默认 50)人到同 region 最空 ready 分片,搬迁顺序镜像 TransferHub
  (占新位 → 切归属 → 退旧位)并重签 hub 票据
- **切换前提示走双通道**:
  - 通道 A:draining 分片的 Hub DS `Heartbeat` 收 `command="drain"` + `grace_seconds`(默认 30)
    → 场内 UMG 倒计时提示 → 到点重连(重连 `AssignHub` 幂等返回迁移后新分片)
  - 通道 B:后端按 `key=player_id` 推 `pandora.hub.migrate`/`HubMigrateEvent`(新地址 + 新票据 +
    倒计时)→ push 服务转发 → 客户端无缝倒计时切大厅
- 排空(`player_count=0`)且过 `migrate_grace_seconds` 后才 `RemoveShard` + 缩 Fleet 副本,
  避免提前杀 pod 打断在场玩家
- 配置项:`hub.consolidation_enabled` / `migrate_grace_seconds` / `consolidation_batch` + kafka
  producer 块;契约见 [`docs/design/agones-dev.md`](./agones-dev.md)

**关键不变量**:
- 同队伍优先在同一 hub
- 跨分片切换"先连新,后断旧",2 秒内完成

---

### 2.13 battle_result

**职责**:消费 `pandora.battle.result` topic,幂等落库

**对外 RPC**(查询用):
```
GetMatchResult(match_id) → BattleResult
ListPlayerHistory(player_id, limit) → []BattleResult
```

**核心流程**(消费者):
```
kafka msg → 验证签名 → 检查 mysql.battles WHERE match_id=? 
                      → 已存在?跳过(幂等)
                      → 不存在?事务{insert battles + insert battle_player_stats + insert player_update_outbox}
                      → ack
                      → 失败 3 次 → DLQ
后台 RunOutboxPublisher(2s):FetchOutbox(FIFO) → 投递 pandora.player.update → 成功才 DeleteOutbox
                      → 投递失败保留出箱行下轮重试(at-least-once,W4 ⑨ 不变量 §4)
```

**关键不变量**:
- **幂等键 = match_id**(unique index)
- **事务边界**:battles + stats + player_update_outbox 必须同一事务(W4 ⑨ 落库与待发布段位事件原子)
- **MMR 计算在这里**(不在 DS 算,DS 不可信)

---

## 3. 服务依赖矩阵

```
                    ┌── login
                    │     │ 验证票据
   client ──────────┤     ▼
                    │   hub DS / battle DS
                    │     │
                    └── hub_allocator / ds_allocator
                              │
                              ▼
                          team / matchmaker
                              │
                              ▼
                          player / data_service
                              │
                              ▼
                          mysql / redis / kafka
                              ▲
                              │
                          battle_result
                              ▲
                              │
                          kafka(battle.result topic)
                              ▲
                              │
                          battle DS 上报
```


## 4. W1 真正要写的服务(只写骨架)

W1 不写业务逻辑,只搭框架:

| 服务 | W1 范围 |
|---|---|
| login | main.go(Kratos)+ kratos.App 启动 + 健康检查 + 注册 etcd + 一个 mock Login RPC(返回固定票据) |
| ds_allocator | main.go + 健康检查 + Agones 客户端连接验证 + 一个 mock AllocateBattle RPC |
| hub_allocator | 同上 |
| 其它 10 个 | 只有空目录 + cmd/main.go 占位 + 注册 etcd |

W2 开始才正式写业务逻辑,顺序:

1. ✅ pkg 重写(Kratos)— W2 ①(commit 见 PROGRESS.md)
2. ✅ proto 全 buf STANDARD + 生成产物 — W2 ②⁺(commit `ee12479`)
3. ✅ **login** 骨架(Kratos 标准分层,mock 行为可联调)— W2 ③
4. ✅ **Envoy** v1.38 边缘网关(login_cluster + push_cluster + grpc_web/cors/router)— W2 ④
5. ✅ **push** 骨架(首个 server stream,5s mock tick)— W2 ⑤
6. ✅ 经 Envoy 端到端 hello world(login unary + push server stream + reflection)— W2 ⑥
7. ✅ player_locator / team / matchmaker / ds_allocator / battle_result / player 核心链路已完成到 W4 ④
8. ✅ **hub_allocator** 骨架(W4 ⑤):Mock Fleet 分片调度 + AssignHub/ReleaseHub/TransferHub/ListHubs/Heartbeat + 心跳超时扫描 + 签 hub DSTicket(接 login 待做)
9. ⏭️ login 接 hub_allocator.AssignHub(替换 mock hub_addr),补不变量 §1 的大厅入口闭环
10. 🟢 可靠补偿 / outbox:W4 ⑧ ds.lifecycle(Redis ZSET 当 outbox)+ W4 ⑨ player.update(MySQL 事务出箱)均已 at-least-once 可靠化;余真 Agones CRD / locator HUB 对账
11. ⏭️ UE 客户端 grpc-web(FHttpModule 自研解析)+ Envoy 全业务路由接入
12. ⏭️ UE Hub DS / Battle DS 骨架 + GAS / Iris / Agones 联调,打通登录→进大厅→匹配→进战斗→结算→回大厅
13. ⏭️ trade / dialogue / data_service 按 UE 主链路需要补最小版本
14. 🧊 chat 暂缓到最后：UE 与核心业务全部完成后再做完整实现（friend 已于 2026-06-15 提前上线，见 §2.4）

---

## 5. push 服务详细契约(2026-06-04 终版)

> ⚠️ 之前 2026-06-03 规划的 "pandora-gateway"(go-zero/gateway)已被否决,Edge Gateway 改用 **Envoy**(基础设施,不是 go 服务)。
> 之前规划的 "WebSocket pandora-push" 已被否决,改用 **gRPC server stream + Kratos**。

### 5.1 push 服务(Kratos transport/grpc + server stream)

**职责**:
- 客户端通过 Envoy 连过来,调 `PushService.Subscribe`(server stream)维持长连
- 集中持有所有在线客户端的 stream(内存索引 `player_id → grpc.ServerStream`)
- 消费多个推送 kafka topics,按 player_id 路由到对应 stream
- 离线消息缓存(redis ZSET,5min)
- 重连补推

**对外 API**(详见 `proto/pandora/push/v1/push.proto`,W2 时创建):

```proto
service PushService {
  // 客户端登录后立刻调,一直保持连接
  // 服务端通过 stream.Send(PushFrame) 持续推送 player_id 相关的所有事件
  rpc Subscribe(SubscribeRequest) returns (stream PushFrame);
}

message SubscribeRequest {
  string session_token = 1;  // JWT,Envoy 已校验,这里冗余检查
  int64  last_seen_ms  = 2;  // 重连补推用
}

message PushFrame {
  string topic    = 1;  // pandora.team.update / pandora.match.progress / ...
  bytes  payload  = 2;  // 业务 Event message 序列化(如 TeamUpdateEvent)
  int64  ts_ms    = 3;
  string trace_id = 4;
}
```

**实现**(Kratos 风格):
- 框架:Kratos `transport/grpc`(支持 server stream,go-zero zrpc 不支持是切换主因)
- WebSocket 库:**不用**(走标准 gRPC,不要自研 ws frame)
- kafka:`sarama` 消费推送 topics,复用 `pkg/kafkax`
- 内存索引:`sync.Map[playerID]*PushService_SubscribeServer`
- 离线消息:redis ZSET,score=ts_ms,member=encoded PushFrame
- 客户端连接:经 Envoy 转发(Envoy 处理 gRPC-Web ↔ gRPC),push 服务只看到标准 gRPC stream

**依赖**:
- 上游:Envoy(转发 gRPC-Web → gRPC stream)
- 下游:kafka(消费推送 topics)+ redis(离线消息 + 玩家在线索引)+ login(JWT 校验,可选)

**关键不变量**:
- 同一玩家同一时刻只有一条 stream(新 Subscribe 挤掉旧 stream)
- 推送至少送达一次(kafka at-least-once,客户端按 PushFrame.ts_ms 去重)
- 重连后自动补推最近 5min 离线消息
- push 重启不丢业务事件(kafka offset commit 保证)

**多实例扩展(W6+)**:
- 同一 consumer group `pandora-push`,kafka 按 partition 分配
- player_id → push_instance 索引存 redis,跨实例 gRPC 转发
- W1-W4 单实例够用,后置优化

**为什么不用自研 WebSocket envelope(2026-06-04 决策)**:
1. gRPC-Web 是 grpc.io 官方规范,Envoy 内置 grpc-web filter 转发
2. UE FHttpModule 已暴露 HTTP/2 + TLS(用户验证过源码,见 `gateway-decision.md` §3)
3. Kratos transport/grpc 原生支持 server stream,代码量比自研 WebSocket 少
4. 调试用 grpcurl 等标准工具,不用自己写 ws 调试器
5. 协议层标准化是 Pandora 铁律(大厂 / 最标准方案)

详见 `gateway-decision.md` §6 / §10。
