# Pandora 项目规范

> 本文档是 Pandora 项目的"宪法",AI 协作和人类开发都必须遵守。
> Pandora 后端项目规范,适配 MOBA 玩法 + UE DS + 双仓库架构。

## 1. 项目基本信息

- **类型**:MOBA(5v5)+ 持续在线大厅(全图自由 PvP,500 人/hub 实例)
- **后端**:Go(13 个服务 + 公共框架 pkg/)
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
   - 当前阶段（W3 ⑤ 后）：验证命令为 `go build ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/... ./services/runtime/player_locator/...`
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
| 协议硬规则 | 2026-06-05 | **Snowflake 业务 ID 一律 uint64**(`player_id` / `team_id` / `match_id` / `order_id` 等);**配置表 ID 默认 uint32**(`npc_id` / `hero_id` / `skill_id` / `item_config_id` / `map_id` 等);**proto enum / 状态常量保持生成 enum 类型或 int32 语义**,不按非负常量改 `uint32`;后续 proto / Go / SQL / Redis / Kafka key 不得新增有符号业务 ID |
| AI 协作 | 2026-06-05 | **Claude 模型分工固化**:Opus 4.7 以上负责出 Plan / 审 Plan / 难题攻关 / 最终把关;Sonnet 4.6 按审过的 Plan 写代码 / 补测试 / 跑项目内验证;ChatGPT / Codex 继续负责环境执行和 git 收尾 |

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
