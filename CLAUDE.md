# Pandora 项目规范

> 本文档是 Pandora 项目的"宪法",AI 协作和人类开发都必须遵守。
> Pandora 后端项目规范,适配 MOBA 玩法 + UE DS + 双仓库架构。

## 1. 项目基本信息

- **类型**:MOBA(5v5)+ 持续在线大厅(全图自由 PvP,500 人/hub 实例)
- **后端**:Go(14 个服务 + 公共框架 pkg/)
- **客户端 + DS**:UE 5.7 + GAS + Iris,**独立仓库**(本仓库 `Pandora` 是后端)
- **DS 编排**:Agones on k8s
- **协议**:gRPC(同步) + Kafka(异步事件)
- **基础设施**:MySQL 8 + Redis 7 + Kafka 3 + etcd 3

## 2. 仓库结构与边界

```
F:/work/Pandora/                # 后端(本仓库)
F:/work/Pandora-Client/         # UE 客户端 + DS(待定名,独立仓库)
```

## 3. 中文回复

所有 AI 协作产出**用中文**。注释、commit message、文档全中文。

## 4. 提交纪律

1. 不准在没有跑通 **所有已启用 module 的构建** 的情况下 commit
   - 本项目采用 `go.work` 多 module 模式,仓库根没有 `go.mod`,**不能**在根目录跑 `go build ./...`
   - 当前阶段（W4 ⑤ 后）：验证命令为 `go build ./pkg/... ./proto/... ./services/account/login/... ./services/account/player/... ./services/runtime/push/... ./services/runtime/player_locator/... ./services/matchmaking/team/... ./services/matchmaking/matchmaker/... ./services/battle/ds_allocator/... ./services/battle/hub_allocator/... ./services/battle/battle_result/...`
   - W2+ 每个服务 module 启用后,追加对应路径
   - 完整命令参考 `go.work` 文件中的 `use` 列表
2. commit message 格式:`<type>(<scope>): <subject>`
   - type:feat / fix / refactor / test / docs / chore / perf
   - scope:服务名(login / matchmaker)/ pkg / docs / deploy
   - 例:`feat(matchmaker): MMR 撮合算法初版`
3. proto 改动要在 commit message 标注 `[proto]`,提醒同步到 UE 仓库
4. **永远不准 force push main**
5. PR 描述必须含:动机 / 改动范围 / 测试方式 / 风险点

## 5. proto 同步流程(双仓库)

1. 改完跑 `pwsh tools/scripts/proto_gen.ps1` 生成 go pb
2. 同时生成 cpp pb 推送到 UE 仓库的 `Source/Pandora/Generated/Proto/`(CI 自动 PR)
3. UE 客户端改动跟在后端 PR 之后合并
4. 字段编号规则:上线后**不复用**,只能 deprecate(`reserved 5;` + 注释原因);开发期间已删除字段可复用编号,但必须重新生成 proto 并完整编译所有已启用 module
5. `player_id` / `team_id` / `match_id` / `order_id` / `message_id` / `dialogue_id` / `hub_id` / `invite_id` 等 Snowflake 业务 ID **一律用 `uint64`**;不准再用 `int64` / `string` 承载这类 ID。未知 / 空值用 `0`,需要表达 presence 时用 `optional uint64`
6. 配置表 ID / 静态表 ID **默认用 `uint32`**(`npc_id` / `hero_id` / `skill_id` / `item_config_id` / `map_id` 等);如果字段名容易和运行时实体混淆,新协议优先命名为 `<entity>_config_id`
7. 状态 / 类型 / 原因等 proto 枚举常量**不属于 ID 规则**;proto enum 底层是 `int32`,Go 代码优先使用生成的 enum 类型,必要时才用 `int32`,不因取值非负改成 `uint32`
8. 新增业务数据结构**优先定义 proto message**,按下面四类各司其职,**不准手写与 proto 重复的并行 struct**:

   | 类别 | 命名 | 用途 |
   |---|---|---|
   | RPC 请求/响应 | `<Verb><Domain>Request` / `<Verb><Domain>Response` | gRPC unary/stream 出入参 |
   | 客户端可见结构 | `<Domain>` / `<Domain><Part>`(短名,如 `Team` / `TeamMember`) | RPC response、push payload 里给客户端看的字段 |
   | 服务端存储快照 | `<Domain>StorageRecord` + 子结构 `<Domain><Part>StorageRecord` | Redis value、Kafka 快照、MySQL **blob 列**里序列化成 bytes 的整块状态 |
   | 服务间事件 | `<Domain><Action>Event` | Kafka payload;可内嵌"客户端可见结构",但它本身是服务内部消息,不是存储快照 |

9. 第 8 条的"存储快照用 proto bytes"**只针对快照/blob 场景**(Redis value、Kafka payload、MySQL blob 列):
   - **关系型 MySQL 表(结构化列)不强制 proto 化**;列直接映射 proto 字段即可,不为每张表再造一个 proto bytes blob
   - 临时小令牌(如 invite,2~3 个字段、短 TTL)允许继续用 redis hash,不必升级成 proto bytes
   - 规则核心是"消灭与 proto 重复漂移的并行 struct",**不是"一切都序列化成 bytes"**
10. proto message 直接当存储 record 时:**禁止值拷贝 proto message**(`a := *rec` 会复制内部 state/mu/sizeCache),克隆一律用 `proto.Clone`;存储字段命名以 `<Domain>StorageRecord` 为准,客户端结构与存储结构**分开两个 message**,存储侧独有字段(如 `updated_at_ms`)不外泄给客户端
11. **禁止把服务端存储快照原样返回 / 推送给客户端**。RPC response / push payload 只能使用"客户端可见结构",由服务端从 `StorageRecord` / MySQL 行 / Redis 状态中按客户端当前需求的**最小数据单位**填充,必要时重新计算派生字段(如 ready 状态、queue_seconds、mmr_delta、展示用昵称),而不是把整块存储 record 暴露出去。例外只能是明确写入设计文档的运维 / 内部调试 RPC,且必须做鉴权、脱敏、不经 Envoy 对客户端开放。

## 6. 服务命名 / 端口规范

详见 [`docs/design/infra.md`](./docs/design/infra.md)。**不允许 ad-hoc 起端口或 key**。

## 7. 当前里程碑(决策行)

| Round | 日期 | 关键决策 / 数据 |
|---|---|---|
| R0 | 2026-06-03 | 立项,新建 Pandora 项目 |
| R0 | 2026-06-03 | 大厅 DS 化,500 人/实例,全图自由 PvP |
| R0 | 2026-06-03 | UE 5.7 + Iris + GAS,Agones 调度 |
| R0 | 2026-06-03 | 双仓库:后端 Pandora,UE 独立仓库 |
| R0 | 2026-06-03 | License MIT,Go 1.23,基础设施全新 |
| R0 | 2026-06-03 | **后端框架继续用 go-zero**(历史决策,后续已切换 Kratos) |
| W2 ④ | 2026-06-05 | **Envoy v1.38.0 边缘网关本地 docker 落地**(listener :8443 TLS + grpc_web/cors/router,login_cluster unary 5s + push_cluster server stream timeout 0s,`dns_lookup_family: V4_ONLY` 修 Windows host.docker.internal IPv6 坑) |
| W2 ⑤ | 2026-06-05 | **push 服务骨架完成**(Pandora 首个 server stream Kratos 服,5s mock tick,ConnectionManager 顶号语义,gRPC :50014 / HTTP :51014) |
| W2 ⑥ | 2026-06-05 | **客户端连接铁律第 2 条全链路打通**(经 Envoy :8443 LoginService/Login unary + PushService/Subscribe server stream 12s 收 3 帧,reflection list 6 services) |
| W3 ① | 2026-06-05 | **JWT 真实化 + Envoy jwt_authn 落地**(pkg/auth HS256 SessionToken 24h + DSTicket 5min,login.Login/IssueDSTicket/VerifyDSTicket 全部接 pkg/auth.Signer/Verifier,Envoy jwt_authn provider pandora_session 用 local_jwks inline 共享 secret,`claim_to_headers: sub → x-pandora-player-id`,push 加 `pmw.AuthOptional()` 中间件读 header) |
| W3 ② | 2026-06-05 | **login 接 MySQL + Redis 真实化**(pkg/mysqlx 标准 database/sql 工厂 + pkg/passwd bcrypt 封装,`pandora_account` 库 3 张表 accounts/account_devices/account_bans,MySQLAccountRepo+SeedAccount 自动种 dev 账号,RedisSessionRepo `pandora:sess:<player_id>` 顶号 TxPipeline,RedisTicketJTIRepo `pandora:ticket:<jti>` SETNX 防 replay,Logout 真验 session.Delete,Kratos config JSON 不解 duration 所以 yaml 不写时长字段) |
| W3 ⑤ | 2026-06-05 | **player_locator 服务上线**(Kratos unary,gRPC :50006 / HTTP :51006,Redis hash `pandora:locator:<player_id>` 30s TTL,SetLocation TxPipeline(Del+HSet+Expire) 防字段残留,不变量 §1 入口落地,login.Login 成功后调 SetLocation(state=LOGIN_PENDING) 失败仅 Warn,locator biz 单测 7 用例覆盖输入校验 / OFFLINE 占位 / 默认 TTL) |
| W3 ⑥/③ | 2026-06-05 | **Duration 包装类型 + gRPC reflection 开关化**(pkg/config.Duration 实现 UnmarshalJSON/MarshalJSON 解 "5s"/"24h" + 数字 ns 向后兼容,pkg/config.Base 全部 15+ 个 `time.Duration` 字段切换,业务读取处统一 `.Std()`;Grpc 新增 `EnableReflection bool` 默认 false → prod 关 reflection 防 schema 泄露,pkg/grpcserver `if !c.Grpc.EnableReflection { kgrpc.DisableReflection() }`;已启用 3 服(login/push/locator)`etc/*-dev.yaml` 全部直接写 duration 字符串 + `enable_reflection: true`,删除"yaml 不写时长字段"长篇约束注释;Duration 单测 8 用例 + Kratos config e2e 加载链路验证) |
| W3 ④ | 2026-06-05 | **push 接 kafka + redis ZSET 离线 5min**(push 删 mock tick → KafkaConsumer 每 topic 一个共享 GroupID,Handler 按 `key=strconv.FormatUint(player_id,10)` 不变量 §9 路由 → 在线 ConnectionManager.SendTo / 离线 RedisOfflineCacheRepo Append;ZSET `pandora:push:offline:%d`,score=ts_ms,member=PushFrame proto bytes+seq 后缀防同 ms 去重塌陷,TxPipeline ZAdd+Expire 每写刷 TTL;`pkg/kafkax.PushToPlayers` helper 把原则 2 排除 caller 工程化(callerID=0 = 原则 3 例外全发);`pkg/kafkax/topics.go` 集中 6 个 push topic 常量;新增 `ERR_PUSH_OFFLINE_CORRUPTED=9301` / `ERR_PUSH_KAFKA_CONSUMER_DOWN=9302`;W3 ④ 仅订阅 proto 已就绪的 3 个(team.update/match.progress/chat.private),余 3 个等业务服上线补;单测 11 用例(producer 4 / offline 4 / consumer 3)) |
| W3 ⑦.0 | 2026-06-05 | **业务 ID 全量 int64/string → uint64 迁移**(14 个业务 proto 按规则迁移并 regen go/cpp pb;`pkg/auth` JWT sub 改 `FormatUint/ParseUint`,`pkg/middleware` 拒绝负数 player_id,`pkg/kafkax.PushToPlayers` 改 `uint64` + kafka key `FormatUint`;login/push/player_locator 三服全链路同步;配置表 ID 如 `npc_id/hero_id/map_id` 保持/改为 `uint32`;`request_id/device_id/trace_id` 保持 string) |
| W3 ⑦ | 2026-06-05 | **team 服务上线**(第 4 个 Kratos 业务服,gRPC :50010 / HTTP :51010 仅 /metrics;7 RPC `CreateTeam`/`Invite`/`AcceptInvite`/`LeaveTeam`/`Kick`/`SetReady`/`GetTeam` 全"立即完成型"(`GetTeam` 只读快照);Redis WATCH/MULTI/EXEC 乐观锁,冲突重试 `OptimisticRetry` 次耗尽返 `ERR_TEAM_CONCURRENT=3007`;key `pandora:team:{<team_id>}`=`TeamStorageRecord` proto bytes(hashtag `{}` 锁 cluster slot)+ `pandora:team:player:<player_id>` `ClaimPlayer` SETNX 保不变量 §1 一人一队 + `pandora:team:invite:<invite_id>` hash InviteTTL 60s;状态机 FORMING/READY/MATCHING/IN_BATTLE/DISBANDED,DISBANDED 拒绝写;写路径发 `pandora.team.update` kafka `TeamUpdateEvent`(push 已订阅),协议原则 2 push 不发 caller;`TeamStorageRecord` proto 直接做存储 record,克隆用 `proto.Clone` 不值拷贝;duration 全 `config.Duration`(ActiveTTL 60min/InviteTTL 60s/DisbandedRetention 5min/MaxMembers 5);biz+data 单测覆盖) |
| 协议硬规则 | 2026-06-05 | **Snowflake 业务 ID 一律 uint64**(`player_id` / `team_id` / `match_id` / `order_id` 等);**配置表 ID 默认 uint32**(`npc_id` / `hero_id` / `skill_id` / `item_config_id` / `map_id` 等);**proto enum / 状态常量保持生成 enum 类型或 int32 语义**,不按非负常量改 `uint32`;后续 proto / Go / SQL / Redis / Kafka key 不得新增有符号业务 ID |
| AI 协作 | 2026-06-05 | **Claude 模型分工固化**:最高可用 Claude 模型(Opus 4.8 以上或更高)负责 Agent 直接执行 / 难题攻关 / 写代码 / 补测试 / 跑项目内验证 / 最终把关;不得把业务代码实现固定交给低一档模型;ChatGPT / Codex 继续负责环境执行和 git 收尾 |
| AI 协作 | 2026-06-06 | **Claude / Agent 默认直接做,不再要求先走前置流程**;ChatGPT / Codex 纯 ops / 收尾 / 环境执行也直接做。涉及安装工具、改系统环境、写 secrets、生产集群、push / tag、30+ 文件大改等红线时仍必须停止并等人授权 |
| W4 ① | 2026-06-06 | **matchmaker 服务上线**(第 5 个 Kratos 业务服,gRPC :50011 / HTTP :51011 仅 /metrics;4 RPC `StartMatch`/`CancelMatch`/`ConfirmMatch`/`GetMatchProgress` 全"已受理型",客户端状态机由 `pandora.match.progress` push 驱动;撮合流水线 QUEUEING→FOUND→CONFIRM→ALLOCATING→READY/FAILED;后台 `RunMatchLoop`(MatchInterval 2s):`matchOnce` 按 avg_mmr 升序 + 动态 MMR 窗口(base 200 / +20/s / max 2000)贪心凑 2×TeamSize → largest-first 装箱拆 5+5 → `formMatch`;`expireOnce` 扫 active ZSET 确认期(15s)超时 → FAILED;Redis key `pandora:match:queue` ZSET(score=avg_mmr)/`pandora:match:active` ZSET(score=confirm_deadline_ms)/`pandora:match:ticket:%d` `MatchTicketStorageRecord` proto bytes/`pandora:match:{%d}` `MatchStorageRecord`(hashtag 锁 slot)/`pandora:match:player:%d` SETNX 落不变量 §1 一人一队列;match 写用 WATCH/MULTI/EXEC 乐观锁,冲突重试耗尽返 `ERR_MATCH_CONCURRENT=4006`;确认失败其余票据退回队列保留 `enqueued_at_ms`,拒绝者整队删除;全员确认 → `DSAllocator.AllocateBattle`(W4 ① 打桩 `StubDSAllocator`,W4 ② 接 ds_allocator gRPC)→ READY 带 ds_addr + 每玩家 battle_ticket;新增 proto `MatchTicketStorageRecord`/`MatchStorageRecord`/`MatchMemberStorageRecord`/`MatchConfirmStatus`;`TeamReader` 弱依赖 team gRPC(team_addr 空则跳过队伍校验,单人票据兜底);push 原则 3 例外 callerID=0 发全体含发起方;biz+data 单测 8 用例(撮合成型/全确认 READY/拒绝退回/超时失败 + 票据往返/队列排序/SETNX/乐观锁/active 扫描)) |
| W4 ② | 2026-06-06 | **ds_allocator 服务上线 + matchmaker 接真实拉 DS**(第 6 个 Kratos 业务服,gRPC :50020 / HTTP :51020 仅 /metrics;4 RPC `AllocateBattle`/`ReleaseBattle`/`Heartbeat`/`ListBattles`;`AllocateBattle` 幂等(同 match_id 已有镜像直接回已分配 ds_addr 防 matchmaker 重试重复拉 DS);W4 ② 用 `MockGameServerAllocator`(pod=`pandora-battle-<match_id>`,addr=`<host>:<base + match_id%range>`),真 Agones GameServerAllocation CRD 留 W4 ③+;Redis key `pandora:ds:battle:{<match_id>}` `BattleStorageRecord` proto bytes(hashtag 锁 slot,TTL BattleTTL 2h)/`pandora:ds:active` ZSET(score=last_heartbeat_ms,member=match_id)心跳超时扫描;状态写 WATCH/MULTI/EXEC 乐观锁,冲突耗尽返 `ERR_DS_ALLOCATION_FAILED=5002`;后台 `RunHeartbeatSweep`(SweepInterval 5s):`sweepOnce` 扫 `RangeStaleBattles(now - HeartbeatTimeout 15s)` → 标记 abandoned + `GameServer.Release` + 移出 active,终态镜像保留供查(不变量 §4;W4 ③ TODO 通知 battle_result 段位回滚);`Heartbeat` 孤儿 DS(无镜像)返 command=`stop`;新增 proto `BattleStorageRecord`;matchmaker 用 `GrpcDSAllocator` 替换 `StubDSAllocator`(ds_allocator_addr 非空才启用),ds_allocator 只返 ds_addr/pod 不签票据,**battle DSTicket 由 matchmaker 用 `pkg/auth.Signer.SignDSTicket(pid, DSTypeBattle, match_id, uuid)` 签**(不变量 §3 5min;MMR 在 battle_result 算,DS 不可信,不变量 §6);matchmaker jwt 配置共享 login/envoy secret;biz 7 用例(分配/幂等/释放幂等/心跳更新/孤儿 stop/列举过滤/超时 abandoned)+ data 6 用例(往返/miss/stale 扫描/乐观锁更新/notfound/删除)) |
| W4 ③ | 2026-06-06 | **battle_result 服务上线 + ds_allocator 发 abandoned 事件**(第 7 个 Kratos 业务服,gRPC :50022 / HTTP :51022 仅 /metrics;MySQL 强依赖 `pandora_battle` 库 2 张表 `battles`(PK match_id,outcome NORMAL/ABANDONED)/`battle_player_stats`(uk_match_player 幂等键),无 Redis;消费 `pandora.battle.result`→`ReportResult` 幂等落库(不变量 §2,SaveResult 命中 unique → alreadyRecorded 不重复写)+ **标准 Elo MMR 在此算**(不变量 §6,`expectedA=1/(1+10^((avgB-avgA)/400))`,K=32,胜负对称,DS 上报的 mmr_delta 一律被覆盖,两队按 avg MMR 算 deltaA/deltaB 写回 stat.mmr_delta);消费 `pandora.ds.lifecycle` 的 `ABANDONED`→`HandleAbandoned` 写 outcome=ABANDONED + delta 全 0 补偿记录(幂等,不变量 §4 DS 崩溃必有补偿);落库成功才发 `pandora.player.update` `PlayerUpdateEvent`(kafka key=player_id 不变量 §9,player 服务上线后消费做幂等 UpdateMMR;弱依赖 broker 不通则静默丢);RPC `ReportResult`(同步兜底)/`GetMatchResult`/`ListPlayerHistory`;MMR reader W4 ③ 用 `StaticMMRReader`(全返 base_mmr 1500,player 未上线),`player_addr` 留作 player gRPC reader 钩子;新增 proto `BattleOutcome` enum + `BattleResult.outcome=10` + `player.v1.PlayerUpdateEvent` + `ds.v1.DSLifecyclePhase`/`DSLifecycleEvent`,kafkax 新增 `TopicBattleResult`/`TopicDSLifecycle` 常量;ds_allocator `sweepOnce` 心跳超时 abandoned 后经新增 `DSLifecyclePusher`(**弱依赖**,nil-safe)发 `DSLifecycleEvent{phase=ABANDONED, player_ids/map_id/game_mode}`(key=match_id)给 battle_result 做 best-effort 补偿(publish 失败仅 Warn,W4 ③ **未做重试/待补偿扫描/outbox**,不变量 §4「DS 崩溃必有补偿」当前仅在 Kafka 正常时成立,可靠闭环留 W4 ④+);`ReportResult` 收到 `Outcome=ABANDONED` 强制 mmr_delta 全 0(Codex 复审风险入口加固,防 DS 经 battle.result 伪造 abandoned 改段位);复用已存在 errcode `ERR_BATTLE_RESULT_DUPLICATE=6001`/`ERR_BATTLE_RESULT_DECODE=6002`/`ERR_BATTLE_RESULT_DB_WRITE=6003`;biz 单测 8 用例(Elo 等分对称/平局/强队赢得少 + K 守恒、ReportResult MMR 赋值 + 幂等、ReportResult 收 ABANDONED 强制 delta 0、HandleAbandoned delta 0 + 幂等、输入校验);真 Agones CRD + player 服务消费 player.update + 可靠补偿留后续) |
| 协议硬规则 | 2026-06-06 | **客户端可见结构与服务端存储快照硬隔离**:面向客户端的 response / push 不得直接返回 `*StorageRecord`、数据库整行、Redis value、内部 Kafka envelope 或审计字段;服务端必须按客户端最小需求组装 / 计算视图 |
| W4 ④ | 2026-06-06 | **player 服务上线 + battle_result 接真实 player MMRReader**(第 8 个 Kratos 业务服,gRPC :50002 / HTTP :51002 仅 /metrics;MySQL 强依赖 `pandora_player` 库 3 张表 `players`(PK player_id,uk nickname,mmr/total_battles/total_wins)/`player_heroes`(uk player_id+hero_id)/`mmr_history`(uk player_id+idempotency_key 幂等键),无 Redis;消费 `pandora.player.update`→`UpdateMMR` 幂等(不变量 §2,idempotency_key=match_id,`mmr_history` uk 命中即视为已处理、读回已记录 new_mmr 不重复改 players)+ 战绩计数(win→battle+1/win+1,lose/draw→battle+1,abandon/rollback→不计);`ApplyMMRChange` 事务 `SELECT mmr FOR UPDATE`+INSERT history+UPDATE players,MMR clamp floor 0;6 RPC `GetProfile`/`UpdateNickname`/`ListHeroes`/`UnlockHero`/`GetMMR`/`UpdateMMR`,GetProfile/写操作懒创建档案(EnsureProfile INSERT IGNORE 默认昵称=`Player_<player_id>` 保 uk_nickname 唯一),`GetMMR` 未建档返 base_mmr+OK 供 battle_result 当 reader;复用既有 errcode `ERR_PLAYER_NOT_FOUND=2001`/`ERR_PLAYER_NICKNAME_TAKEN=2003`/`ERR_PLAYER_HERO_ALREADY_OWN=2011`(无新增,无 proto regen);**battle_result `StaticMMRReader`→`GrpcMMRReader`**(新增 `internal/data/mmr_reader.go` 经 grpcclient 调 player.GetMMR,`battle.player_addr` 非空启用,弱依赖:gRPC 懒连接 + 调用失败 biz 回退 BaseMMR 不阻断落库),battle_result-dev.yaml `player_addr: 127.0.0.1:50002`;biz 9 单测(delta 应用/幂等不双算/缺 key 拒/floor clamp/lose 计场不计胜/abandon 不计场/GetMMR 未建档返 base/UnlockHero 幂等/昵称校验/battleFlags);新增 `deploy/mysql-init/04-player-tables.sql`;go.work 加 `use ./services/account/player`,验证升 9 module) |
| 排期决策 | 2026-06-06 | **friend / chat 暂缓到最后**:社交好友(:50004)和聊天(:50005)当前不进入后端主线,只保留 proto / 端口 / topic / push 订阅模板;等 UE 客户端、Hub DS、Battle DS、Agones、登录→进大厅→匹配→进战斗→结算→回大厅核心闭环以及必要经济/对话/数据补齐后,再作为社交尾部功能实现 |
| W4 ⑤ | 2026-06-06 | **hub_allocator 服务上线**(第 9 个 Kratos 业务服,gRPC :50021 / HTTP :51021 仅 /metrics;大厅 DS 分片调度,完成 500 人/实例大厅入口骨架;5 RPC `AssignHub`/`ReleaseHub`/`TransferHub`/`ListHubs`/`Heartbeat`;Redis 强依赖 + JWT 强依赖(本服是 hub 票据权威,AssignHub/TransferHub 用 `pkg/auth.Signer.SignDSTicket(pid, DSTypeHub, 0, uuid)` 签 hub DSTicket,不变量 §3 5min,secret 共享 login/envoy);`AssignHub` 幂等(已分配且分片 ready → 重签票不重复占位,落不变量 §1 一人一 hub)+ 队友同分片(`pandora:hub:team:<team_id>` 提示)+ 最空 ready 分片贪心(并列取 shard_id 小者);`TransferHub` 先占新分片再退旧分片(targetHubID!=0 点名 shard_id,否则最空非当前),失败不动旧分片;Redis key `pandora:hub:shard:{<pod>}`=`HubShardStorageRecord` proto bytes(hashtag 锁 slot,TTL ShardTTL 30min)/`pandora:hub:shards` SET/`pandora:hub:active` ZSET(score=last_heartbeat_ms,member=pod)/`pandora:hub:player:<id>`=`HubAssignmentStorageRecord`/`pandora:hub:team:<id>` string;分片 player_count 写用 WATCH/MULTI/EXEC 乐观锁,冲突耗尽返 `ERR_HUB_NO_AVAILABLE=5101`;W4 ⑤ 用 `MockHubFleetProvider`(pod=`pandora-hub-<region>-<i>`,addr=`host:base+i`,真 Agones Fleet 留 W4+),分片由 Fleet lazy-seed(种子 last_heartbeat_ms=0,**不进 active**);`Heartbeat` 刷新已存在分片 player_count/state/last_heartbeat_ms 并入 active,孤儿 DS(无镜像)返 command=`stop`(HeartbeatRequest 不含 addr/region,不在心跳路径建档);后台 `RunHeartbeatSweep`(SweepInterval 5s)`RangeStaleShards`(Min `(0` 排除从未心跳的 Mock 种子,Max=now-HeartbeatTimeout 15s)→ 标记 draining + 移出 active(不变量 §4);player_count 由 allocator 维护为容量判定基准,Heartbeat 上报回写对账;新增 proto `HubShardStorageRecord`/`HubAssignmentStorageRecord`(server-internal 存储快照,不外泄客户端,不变量 §14);复用既有 errcode `ERR_HUB_NO_AVAILABLE=5101`/`ERR_HUB_TRANSFER_FAILED=5102`(无新增,无 errcode regen);biz 14 单测(lazy-seed 最空/幂等不双占/分散/容量满/队友同分片/release 自减幂等/transfer 跨分片/未入 hub 拒/心跳孤儿 stop/已知不下指令/扫描标 draining/扫描跳过从未心跳/输入校验)+ data 9 单测(分片往返/列举/乐观锁/心跳已知与孤儿/stale 排除 score0/移除/归属往返/队伍往返);go.work 加 `use ./services/battle/hub_allocator`,验证升 10 module;**接 login.Login 替换 mock hub_addr 留后续(本轮仅骨架,如 ds_allocator 先于 matchmaker wiring)**) |

后续每轮压测 / 大决策追加一行,**永不删旧行**。

## 8. 压测纪律

详见 [`docs/design/stress-discipline.md`](./docs/design/stress-discipline.md)。**核心规则**:

- 跑测前必有 `prev-summary.txt`,否则不许开下一轮
- **跑测前清空** redis / mysql / etcd / kafka offset / k8s GameServer
- 至少 3 次 prom snapshot:ramp 完成 / 稳态中段 / 稳态末
- summarize 脚本输出五段二维表,**不许手 grep raw prom**
- **没有对比表不许声明"性能提升"**
- 压期间不上传日志
- **每次登录压测把所有 redis/mysql/etcd 数据全部删除再开新一轮**

## 9. 不变量(数据一致性 / 安全)

跨服务必须保持的不变量。任何改动违反这些 → PR review 直接拒。

1. **玩家在线只能在一个 DS**(player_locator 强制)
2. **战斗结果幂等**(同一 match_id 只落库一次)
3. **DS 票据短时效**(JWT exp 5min)
4. **DS 崩溃必有补偿**(15s 心跳超时 → abandoned → 段位回滚)
5. **proto 字段编号上线后不复用**;开发期间已删除字段可复用编号,但必须重新生成 proto 并完整编译所有已启用 module
6. **MMR 计算在 battle_result**(DS 不可信)
7. **交易资源扣减必须原子 + 有补偿幂等键**
8. **所有写都要带 trace_id**
9. **kafka topic key = 业务实体 ID**(同一玩家 / 同一对局事件有序)
10. **Redis lock TTL ≤ 30s**,业务跑完主动释放
11. **Snowflake 业务 ID 一律 uint64**(`player_id` / `team_id` / `match_id` / `order_id` / `message_id` / `dialogue_id` / `hub_id` / `invite_id` 等),不准新增 `int64` / `string` 型业务 ID
12. **配置表 ID 默认 uint32**(`npc_id` / `hero_id` / `skill_id` / `item_config_id` / `map_id` 等),不准新增有符号配置 ID
13. **proto enum / 状态常量保持 enum/int32 语义**(`TEAM_STATE_*` / `STATE_*` / `*_REASON_*` 等),不准因枚举值非负改成 `uint32`
14. **客户端只拿客户端可见结构**:任何面向客户端的 response / push 不准直接返回 `*StorageRecord`、数据库整行、Redis value、内部 Kafka envelope 或内部审计字段;必须经服务端组装成最小视图,只包含客户端渲染 / 交互所需字段。

## 10. AI 协作约定

AI 协作规则以 [`AGENTS.md`](./AGENTS.md) 为准,本文件不重复维护细则,避免双文档漂移。

## 11. UE 工程约束(写给 UE 仓库的开发者参考)

1. 类前缀统一 `Pandora*`(GameMode / Character / PlayerController)
2. 服务端逻辑统一在 `PandoraHubServer` / `PandoraBattleServer` 模块,不在 `Source/Pandora/` 客户端模块
3. 蓝图只做"胶水"(挂技能动画 / UMG 绑定),逻辑在 C++
4. 资源走 Git LFS(`.uasset / .umap / .fbx / .png / .wav / .ogg`)
5. **永远不要在 git 里提交** `Binaries/ Intermediate/ DerivedDataCache/ Saved/`

## 12. 不要做的事

- ❌ 不要在 docs/design/ 之外随便建 README(集中维护)
- ❌ 不要 import 第三方 GUI 库到 go 服务(go 服务都是 headless gRPC)
- ❌ 不要把 player_id 当 prometheus label(高基数会爆)
- ❌ 不要在 W1 写业务逻辑,只搭骨架
- ❌ 不要混用 `Pandora` / `pandora` / `MOBA` / `moba` 命名 — 见 §2 大小写规则

## 13. 命名大小写规则(强制)

- **Pandora**(首字母大写):仓库名 / 本地路径 / 工程类前缀 / 文档项目名引用
- **pandora**(全小写):kafka topic / mysql / redis key / docker 镜像 / go module
- **MOBA**:仅描述游戏类型时使用("Pandora 是一款 MOBA"),**不能**指代项目本身
