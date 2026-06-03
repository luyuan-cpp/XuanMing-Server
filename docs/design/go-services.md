# Pandora Go 服务清单与契约

> 13 个 go 服务的职责边界、对外接口、关键状态、依赖矩阵。

## 1. 服务总览

| # | 服务 | gRPC 端口 | 状态性 | 主要存储 | 主要消费 kafka |
|---|---|---|---|---|---|
| 1 | login | 50001 | 无 | mysql + redis | (生产 login.event) |
| 2 | player | 50002 | 无 | mysql + redis | player.update |
| 3 | data_service | 50003 | 无 | mysql + redis | (写穿层) |
| 4 | friend | 50004 | 无 | mysql + redis | - |
| 5 | chat | 50005 | 弱 | redis pub/sub | chat.world |
| 6 | player_locator | 50006 | 强 | redis | locator.update |
| 7 | team | 50010 | 强 | redis | - |
| 8 | matchmaker | 50011 | 强 | redis | (生产 match.found) |
| 9 | trade | 50012 | 强 | redis + mysql | trade.audit |
| 10 | dialogue | 50013 | 无 | mysql / 配置中心 | - |
| 11 | ds_allocator | 50020 | 弱 | etcd + k8s | (生产 ds.lifecycle) |
| 12 | hub_allocator | 50021 | 弱 | etcd + k8s | (生产 ds.lifecycle) |
| 13 | battle_result | 50022 | 无 | mysql | battle.result |

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
ListFriends(player_id) → []FriendInfo
Block(player_id, target_id) → ok
```

**MOBA 早期可不做**,先留接口骨架。

---

### 2.5 chat

**职责**:频道(世界 / 队伍 / 私聊)

**对外 RPC**:
```
SendMessage(player_id, channel, content) → message_id
StreamMessages(player_id, channel) → stream Message
```

**实现**:
- 世界频道:kafka topic + 各 hub DS 消费下发
- 队伍频道:redis pub/sub
- 私聊:redis pub/sub + 离线 mysql

**反作弊**:消息内容服务端过敏感词,长度 ≤256

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
StreamTeamUpdates(team_id) → stream
```

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
StreamMatchProgress(match_id) → stream
ConfirmMatch(player_id, match_id, accept) → ok
```

**核心算法**:
1. 按 MMR 分段
2. 同段位优先,等待时间长 → 放宽 ±200 MMR
3. 队伍合并(2+3 / 2+2+1 / 5)
4. 凑齐 10 → 进入确认期(15s,任一人拒绝)
5. 全员确认 → 调 ds_allocator → 推 ds_addr 给玩家

**关键不变量**:
- 同一玩家只能在一个 match 队列
- 确认期内有人拒绝 → 其他人退回队列(保留排队时长)

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

**对话树存储**:配置中心 / mysql `dialogue_trees` 表(json blob)

**MOBA 早期**:简单 if-else 即可,不上行为树

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
- 调用 Agones K8s API:`GameServerAllocation` CRD
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
                      → 不存在?事务{insert battles + insert battle_player_stats + 发 player.update}
                      → ack
                      → 失败 3 次 → DLQ
```

**关键不变量**:
- **幂等键 = match_id**(unique index)
- **事务边界**:battles + stats 必须同一事务
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
| login | main.go + servicecontext + 健康检查 + 注册 etcd + 一个 mock Login RPC(返回固定票据) |
| ds_allocator | main.go + 健康检查 + Agones 客户端连接验证 + 一个 mock AllocateBattle RPC |
| hub_allocator | 同上 |
| 其它 10 个 | 只有空目录 + cmd/main.go 占位 + 注册 etcd |

W2 开始才正式写业务逻辑,顺序:
**login → player + data_service → team → matchmaker → ds_allocator + hub_allocator → battle_result → 其它**
