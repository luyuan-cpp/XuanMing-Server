# Pandora Go 服务清单与契约

> 14 个 go 服务的职责边界、对外接口、关键状态、依赖矩阵。
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
| 2 | player | 50002 | 无 | mysql + redis | player.update | ⏸️ W3 |
| 3 | data_service | 50003 | 无 | mysql + redis | (写穿层) | ⏸️ W3 |
| 4 | friend | 50004 | 无 | mysql + redis | - | ⏸️ W3+ |
| 5 | chat | 50005 | 弱 | redis pub/sub | chat.world | ⏸️ W3+ |
| 6 | player_locator | 50006 | 强 | redis | locator.update | ⏸️ W3 |
| 7 | team | 50010 | 强 | redis | - | ⏸️ W3 |
| 8 | matchmaker | 50011 | 强 | redis | (生产 match.found) | ⏸️ W3 |
| 9 | trade | 50012 | 强 | redis + mysql | trade.audit | ⏸️ W4+ |
| 10 | dialogue | 50013 | 无 | mysql / 配置中心 | - | ⏸️ W4+ |
| 11 | ds_allocator | 50020 | 弱 | redis (+k8s) | (生产 ds.lifecycle) | ✅ W4 ②(Mock 分配器,W4 ③ 发 abandoned;真 Agones 留后续) |
| 12 | hub_allocator | 50021 | 弱 | etcd + k8s | (生产 ds.lifecycle) | ⏸️ W3 |
| 13 | battle_result | 50022 | 无 | mysql | battle.result + ds.lifecycle | ✅ W4 ③(幂等落库 + Elo MMR + abandoned 补偿) |
| 14 | **push** ⭐ | **50014**(gRPC server stream) | 强(连接索引) | redis(离线消息)| pandora.{team,match,chat,player,friend,system}.* | ✅ W2 ⑤(mock 5s tick,W3 接 kafka) |

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
7. ⏸️ UE 客户端 grpc-web(W3+,FHttpModule 自研 grpc-web 解析)
8. ⏸️ player + data_service(W3)
9. ⏸️ team → matchmaker(W3)
10. ✅ ds_allocator(W4 ②,Mock 分配器)+ ⏸️ hub_allocator(W3)
11. ✅ battle_result(W4 ③,幂等落库 + Elo MMR + abandoned 补偿)
12. ⏸️ 其它(friend / chat / trade / dialogue,W4+)

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
