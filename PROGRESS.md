# Pandora 进度记录

> 2026-07-01 整理版。旧版里大量 RPC 表、Redis key、命令流水、重复验证清单已经压缩/删除。
> 本文件只保留新会话接棒必须知道的进度、重要决策、破坏性变更、风险和待办。
>
> 细节归档规则:
> - 架构和长期决策:看 `docs/design/pandora-arch.md`
> - 服务契约和端口:看 `docs/design/go-services.md` / `docs/design/infra.md`
> - proto 规则:看 `docs/design/proto-design.md` 与 `CLAUDE.md` §5
> - 协议 response/push 语义:看 `docs/design/protocol-ordering-rules.md`
> - 压测纪律和报告:看 `docs/design/stress-discipline.md` 与 `docs/design/stress-*.md`

## 当前状态(截至 2026-06-30)

- 后端主路线:Go + Kratos + Envoy + gRPC-Web over HTTP/2 TLS + 集中 push gRPC server stream。
- UE 侧命名统一为 Pandora,`Xuanming` / `Xm` 已废弃;proto source of truth 在后端仓库。
- UE Hub/Battle DS 骨架已完成,包含 `APandoraHubGameMode` / `APandoraBattleGameMode` 与 Agones SDK 接入。
- `services/` 已按业务域分组,当前 workspace 有 19 个 Go 服务:
  - account:`login`,`player`
  - data:`data_service`
  - social:`friend`,`chat`,`dialogue`,`guild`,`mail`
  - matchmaking:`team`,`matchmaker`
  - runtime:`player_locator`,`push`,`leaderboard`
  - economy:`trade`,`inventory`,`auction`
  - battle:`ds_allocator`,`hub_allocator`,`battle_result`
- 基础设施:MySQL / Redis / Kafka / etcd / Prometheus / Grafana 走本地 compose;Envoy 作为 edge gateway。
- 当前最新业务进度:mail 服务上线;player 领奖记录底座上线;cellroute 装配层已接进主要服务;配置表热更路线已拍板。

## 2026-07-01 追加

- 开发编排:含战斗混合模式第一版落地。`start.ps1 -Mode battle` / `play.ps1 -Battle` 现在走
  17 个业务服务容器 + 宿主 `ds_allocator`/`hub_allocator` 的形态,用于本机/内网真实 Windows DS
  联调;`local` 保留 19 个宿主 Go 进程断点调试口径。
- 已做项目内轻量验证:默认 cluster 配置仍指向容器服务名,`-HostAllocators` 才把 allocator RPC
  改为 `host.docker.internal:50020/50021`。真 Docker + Windows DS + UE 客户端端到端跑一局仍待人工验收。

## 重要决策索引

- 2026-06-04:后端框架从 go-zero 切到 Kratos;Edge Gateway 选 Envoy;客户端业务通道走 gRPC-Web over HTTP/2 TLS。
- 2026-06-04:客户端两条连接固定:UE NetDriver 只承载游戏内同步,FHttpModule 承载业务请求和推送。
- 2026-06-04:push 架构固定为集中 push 服务 + gRPC server stream,不做自研 WebSocket gateway。
- 2026-06-03 起:RPC response 与 push 乱序是协议语义问题,不是单连接能解决的问题;具体原则见 `protocol-ordering-rules.md`。
- 2026-06-06:客户端可见结构与服务端存储快照强隔离,不准直接把 StorageRecord / Redis value / DB row 推给客户端。
- 2026-06-11:Snowflake 继续本地发号;多副本 nodeID 走 etcd Lease 分配,拒绝 Redis INCR 发号。
- 2026-06-18:friend / chat 所在 `pandora_social` 拍板切 TiDB 路线;Go 业务尽量保持 MySQL 协议兼容。
- 2026-06-19:trade 不承载全服拍卖行;全服拍卖和撮合独立为 `auction` 服务。
- 2026-06-26:DAU 目标从 200 万上调到 2000 万,采用 Region -> Cell -> Cell 内分片三层化路线。
- 2026-06-27:采用轻量 DDD 思想,不把“微服务 + 事件”误认为 DDD。
- 2026-06-30:配置表热更走自研轻量流水线:版本号 + checksum + staging + reload + 原子切换 + 失败保留旧配置。
- 2026-07-01:确立不停服更新(零停机)为硬约束:go 服务无状态滚动更新 + Redis 二进制 pb 存储双向兼容演进(只加字段/懒迁移,禁改编号类型、禁 read-modify-write 丢 unknown fields)。见 `CLAUDE.md` §9 不变量 16/17、`docs/design/zero-downtime-update.md`。
- 2026-07-06:Battle DS 空场回收拍板「回收 + 宽限窗」双层方案(对齐业界 empty-server-timeout):DS 侧空场计时器自结算为主路径(UE 仓库待实现,建议 2~3min),后端 `ds_allocator` 按 `player_count==0` 持续超 `empty_battle_timeout`(默认 5m,须 > 断线重连窗口 ~30s)心跳内判 abandoned + 回收 + 段位回滚补偿兜底(已上线,复用心跳超时补偿链路)。**[proto]** `BattleStorageRecord` 新增 `empty_since_ms=11`(存储侧字段,加字段兼容演进,客户端无感知,无需 UE 同步)。契约见 `agones-dev.md` §3.2。
- 2026-07-06:matchmaker 两道 locator 离线判定门(成局最终门 findOfflineMembers + 队列在线扫除 livenessSweep)收进开关 `match.liveness_gate_enabled`,**默认关闭**:离线判定依赖 Hub DS 心跳捎带 `player_ids`(hub/v1 HeartbeatRequest)续期 locator HUB 位置,UE Hub DS 生产端尚未实现;先上线服务端会把在线玩家 30s 后误判离线、扫掉排队票据。**待 UE Hub DS 上报 player_ids 联发后才可开启**(开启路径已完整实现并有测试)。同批:hub_allocator `RefreshHubPresence` 改 goroutine + 独立 3s 超时(同 ds_allocator.refreshBattleLocations),locator 抖动不再拖慢 Hub DS 心跳响应。→ **2026-07-08 已开启**:两真实打包客户端实机联调验证保活链(UE Hub DS→hub_allocator→locator→Redis),K8s Redis 采样在线玩家 locator TTL 稳定 25~30s 回升不衰减;`matchmaker-dev.yaml`/`matchmaker-pve.yaml` 置 `liveness_gate_enabled: true`(cluster 配置由 gen_cluster_config.ps1 从此生成)。回归失败(在线玩家 TTL 单调掉 0)先关此开关再排查。**同日端到端验收通过**:①配置生效(configmap 重建 + matchmaker/matchmaker-pve 滚动重启,两 pod Ready);②离线扫死票——无 HUB 位置玩家入队,一轮 sweep 内 `liveness_sweep_reaped_ticket` 删票,Redis ticket/claim/queue 全清;③在线不误删——synthetic HUB 位置每 5s 续期挂队列 45s,9 次采样票据完好、无 reap 日志。液门(liveness gate)正式生效。残余(非阻塞):真机 UE 登录后挂队列的纯真机复验。
- 2026-07-07:根治「重启电脑/换模式后 500xx 端口被旧 compose 业务容器劫持」:`docker-compose.services.yml` 业务容器 `restart` 由 `unless-stopped` 改 `"no"`(dev 业务容器生命周期一律由一键脚本显式管理,不随 Docker Desktop 开机自启;dev.yml 基础设施保留 unless-stopped);`k8s_envoy_bridge.ps1` 三处加固——预检 `Stop-StaleComposeContainers` 自动 stop 发布 bridge 端口的 pandora-* 容器、端口检测扩到 0.0.0.0/::(docker 发布端口监听在 0.0.0.0,旧检测只查 127.0.0.1 看不到 → 双监听并存导致 Envoy 流量去向不确定)、占用者为 com.docker.backend 等 Docker 转发进程时拒绝 Stop-Process(会杀整个 Docker)。**与不停服滚动更新(不变量 16/17)无关**:灰度升级的载体是 k8s Deployment RollingUpdate,compose 只是本地 dev 环境且本身无滚动更新能力。流量切换时序、gRPC 长连接 L7 均衡、金丝雀灰度四阶段已补进 `zero-downtime-update.md` §6。终局方向(未做):envoy 部署进 k8s,消灭宿主 500xx 桥接层。
- 2026-07-08:滚动更新流量切换两项基础能力落地(zero-downtime-update.md §6.5 前两项):① `deploy/k8s/services/services.yaml` 20 个 Deployment 全部加原生 gRPC readinessProbe(打 grpc_health_v1,Kratos 默认注册、Stop 自动 NOT_SERVING;新 Pod 必须 SERVING 才进 Endpoints 接流量);② gRPC 连接轮换:`pkg/config.Grpc` 新增 `max_conn_age`/`max_conn_age_grace`,`pkg/grpcserver` 按配置挂 keepalive MaxConnectionAge(零值=关,行为不变),20 个服务 dev yaml 全量开 15m,ds_allocator 显式 grace 90s(盖过 AllocateBattle 同步等 DS ready 的 ~60s,防 GOAWAY 砍断在途分配)。验证:pkg + 18 个服务 module 全部 go build 通过、pkg/config 测试绿、kustomize 渲染 20 个 readinessProbe。剩余待补(扩多副本前):服务间 headless/L7 均衡、RollingUpdate 策略显式化,见 §6.5。
- 2026-07-08:**角色养成五件套(角色界面/装备更换/属性加点/天赋树/背包道具使用·出售)对客户端放行 + IDOR 加固**。核心结论:这五个都是**局外(meta)系统**,与 MOBA 战斗延迟零耦合(客户端走 Envoy→player/inventory 独立 gRPC 通道,DS 战斗内绝不同步回调 Go),后端 proto/表/biz/data/service 早已实现,真正缺口只是「安全地暴露给客户端」。改动:①`player`(:50002)/`inventory`(:50015)两 cluster 接进 Envoy edge(`deploy/envoy/envoy.yaml`,STRICT_DNS/V4_ONLY/http2,host.docker.internal,k8s 复用同文件经 bridge 转发);②两服务全 RPC 加 `jwt_authn` 需 `pandora_session`(R5 player_id 以 JWT sub 为准);③系统/内部方法双保险 403——Envoy 精确 path `direct_response 403`(player:UpdateMMR/GetMMR/UnlockHero/GrantAttributePoints/GrantTalentPoints;inventory:GrantItems/GrantInstances/FreezeForOrder/SettleAuctionMatch/SettlePlayerTrade/ReleaseEscrow)+ 服务层兜底。**player 服务 IDOR(OWASP A01)修复**:原先信任请求体 `player_id`,任意登录客户端可读写他人数据;仿 inventory 的 `callerPlayerID` 模式加三个纯鉴权辅助——`selfPlayerID`(客户端自助写:身份缺失→UNAUTHORIZED,body≠caller→PERMISSION_DENY,回落 caller)、`resolvePlayerID`(读,双模式:内网直连 callerID==0 信任 body,客户端强制自身)、`systemOnly`(callerID≠0→PERMISSION_DENY)。读/写/系统三类分流已套全 handler,`s.uc.*` 一律传解析后 `playerID` 不再用 `req.GetPlayerId()`。`GetProfile` 默认自查(安全默认;跨玩家看板将来另开 `ViewProfile`)。当前无 PlayerService 内网 gRPC 调用方(grep 无 NewPlayerServiceClient),改动不破坏既有链路(battle_result 的 GetMMR reader / matchmaker·DS 的 GetLoadout 快照注入均 callerID==0 走信任分支)。验证:`go build`/`go vet`/`go test ./...` 全绿(新增 `internal/service/auth_test.go` 覆盖三辅助分流),envoy.yaml yaml.v3 解析通过。**残余(UE/人工领域)**:UMG 面板调这些 RPC(需带 player SessionToken JWT,个人数据自查)属客户端侧,按 AGENTS.md §11 交 UE/Codex。**待确认**:「技能卡」若为独立于天赋(player_talents/SetTalents/GetTalents)的系统,可能是真实未来缺口。
- 2026-07-08:**延迟不变量固化**——局外系统放 Go 零战斗延迟,是架构决定不是调优结果:①客户端→Go 大厅连接(gRPC-Web/HTTP2)与客户端→DS 战斗连接(NetDriver/UDP)物理独立、不共享带宽与故障域;②DS 帧循环里没有对 Go 的同步调用(tick 全走 GAS/Replication,DS→Go 只剩 Heartbeat 5s/GetLoadout 开局一次/battle_result 局后一次,全独立 goroutine+5s 超时不阻塞主 tick);③唯一会真拖慢延迟的错误做法 = 让 DS 战斗中同步 RPC 大厅服务,守住「开局快照 + 局后上报」边界即永不发生。红线:任何"战斗内实时读写 player/inventory/economy"需求必须改造成开局快照或局后异步上报,否则推翻。落文档 `docs/design/ds-arch.md` §0.6(配套 §0.3/§0.5)。

## 已完成里程碑

### 基础骨架

- 仓库文档、proto 工具链、公共 `pkg/` 框架、dev compose 和脚本已搭好。
- proto 已覆盖核心业务域,并经历过多轮规则收紧:业务 ID 用 `uint64`,配置表 ID 默认 `uint32`,枚举保持 enum/int32 语义。
- 服务目录已从根目录平铺改为 `services/<domain>/<service>`。
- `go.work` 多 module 模式为当前构建口径;根目录不加单根 `go.mod`。

### 协议与网关

- Envoy + gRPC-Web 架构已落文档,dev TLS / 生产 TLS 策略已明确。
- UE 5.7 FHttpModule 已确认支持 HTTP/2 TLS 与流式接收,客户端可自研 gRPC-Web 解析,不引入第三方 UE gRPC 插件。
- JWT session / DS ticket 已真实化,Envoy `jwt_authn` 已接入。
- push 服务已接 Kafka + Redis ZSET 离线 5min,订阅核心 push topics。

### 核心服务闭环

- `login`:MySQL / Redis 真实化,接 `hub_allocator.AssignHub`,支持 dev skip password / auto register。
- `player`:档案、MMR、出战养成、装备预设、天赋树、领奖记录底座已上线。
- `player_locator`:MATCHING / BATTLE 状态机守卫和 BATTLE fence 已补。
- `team`:组队服务上线,已补 `GetMyTeam`;客户端同步约定已记录到 `go-services.md`。
- `matchmaker`:5v5 撮合、auto-confirm 语义、两级跨 region 撮合基础已落地。
- `ds_allocator`:真实 Agones GameServerAllocation、abandoned 补偿链路已打通。
- `hub_allocator`:大厅分配、自动扩缩容、强制整合与玩家迁移通知已落地。
- `battle_result`:战斗结果幂等落库、MMR 更新、player.update 事务出箱可靠化已落地。
- 2026-06-09:真 Agones + Kafka + MySQL 两段补偿链验证跑通。

### 社交与运行时

- `friend`:好友、黑名单、请求闭环 RPC 已补;分布式好友图路线文档化,本地 TiDB 验收通过。
- `chat`:世界 / 队伍 / 私聊 / 公会 / 临时群五频道扩展已落地。
- `dialogue`:NPC 对话树运行时服务上线。
- `guild`:公会 + 临时群聊服务上线,chat 已接 guild/group fan-out。
- `mail`:系统/公会邮件 channel + watermark 拉取,个人邮件写扩散,附件领取幂等,claim 越权问题已修。
- `leaderboard`:通用排行榜服务上线,支持 Redis ZSET 实时榜 + MySQL 结算归档。

### 经济与资产

- `inventory`:大厅背包上线,覆盖货币、可堆叠道具、使用/出售/授予和 ledger 幂等。
- `trade`:玩家交易上线,后续接 inventory 真实 P2P 原子对转,替换 NoopResourceLedger。
- `auction`:全服拍卖行/跨玩家撮合引擎上线,含 escrow 冻结、per-market 单写者、过期清扫和 inventory 结算。
- `auction` 真依赖本机冒烟通过;buyer/seller 资产变更链路已验证。

### 扩容与平台能力

- `pkg/snowflake/etcdnode`:etcd Lease 分配 nodeID 底座已落。
- `redisx.NewUniversalClient` 与 `mysqlx.ShardSet` 已作为 Redis Cluster / MySQL 分片底座。
- `pkg/cellroute`:Region/Cell/Cell 内分片确定性路由、热更新和 etcdtable 子 module 已落。
- cellroute 装配层已接入主要服务 main;默认 `mode=off` 保持单 Cell 行为。
- 本地 k8s + Agones + 端到端 hello world 已完成;生产 k8s 形态另行定稿。
- UE DS D5-D6 骨架代码已完成;GAS/Iris 深度玩法联调继续按 UE 主线推进。
- Kill-Switch RPC 级临时关停与自动防护四层方案已落地。
- 配置表热更方案已形成文档:不接 Apollo/Nacos,先复用 etcd 做版本通知。

### 压测与工具

- `robot/stress` 机群和压测三脚本已落地。
- P0 本机 80 VU harness 已跑通,并完成多轮修复:
  - error 调用点归因
  - shutdown canceled 与真实 error 分流
  - auto-confirm 竞态修复
- 最新 P0 结论:80 VU 冒烟可跑通,真实 RPC error 已归零;但这不是单 Cell 40 万 CCU 验收。

## 重要破坏性变更 / 客户端需同步

- trade proto 已从实例道具 `item_uid` 语义切到可堆叠 `item_config_id + count` 模型,并支持 `buyer_items`。UE 客户端必须按新模型同步;若产品坚持实例道具交易,需要另起设计复议。
- player 领奖记录新增 `ClaimReward` / `GetRewardClaims` 与 `RewardClaimStorageRecord`,已生成 Go/C++ pb。
- mail proto 已上线,需确认 UE 侧 C++ pb 与 UI/红点逻辑同步情况。
- Region/Cell 字段曾随 DS ticket / login 路由接线发生 proto 变更,继续改 proto 时必须跑完整生成和启用 module 编译。

## 当前风险与待办

- player 领奖记录目前只记录“已领取状态”,还未把奖励发到 inventory;完整领奖链路需接 `inventory.GrantItems` 或货币变更。
- leaderboard 仍有一个业务问题待修:同一 `settle_idempotency_key` 复调不会重复发奖,但 `reset_after=true` 后响应未从 MySQL snapshot 回放 winners。
- trade -> inventory 的真实 P2P 原子对转已有代码和单测,但仍需真 MySQL / gRPC 端到端冒烟,并确认 UE 接受 trade item 模型变更。
- 蜂窝扩容代码地基已到 cellroute 装配层,但 24 Cell / 3 Region 物理部署、多 k8s、分库分表、跨 region Kafka 桥仍未落地。
- 单 Cell 满载压测未启动;目前只有本机 P0 80 VU 冒烟,不能声明性能达标或进入多 Cell。
- 本地 Windows + `ds_mode=stub` 下 `AllocateBattle` 慢路径属于假慢;接真 DS 后需重新测量。
- k8s 生产形态仍未最终定稿;本地以 minikube / Agones dev 验证为主。
- push 横扩、Agones 池化、TiDB/Kafka 集群化属于后续 infra 工作,按 AGENTS §11.1 由 Codex/人执行环境侧动作。

## 后续记录方式

以后往本文件追加时只写这几类:

- 已拍板且会影响后续实现的决策。
- 新服务/新能力是否真正上线,一句话说明边界。
- proto/API 破坏性变更和客户端同步要求。
- 真实验证结论,尤其是压测、端到端冒烟、生产风险。
- 当前 blocker、未修 bug、需要人拍板的问题。

不要再写:

- 单个 RPC 的逐项语义表。
- Redis key / SQL 字段 / 配置项流水账。
- 完整命令清单和每条命令输出。
- 与 `docs/design/` 已有内容重复的大段解释。
- 每个文件逐条列“新增/修改”清单,除非它是破坏性变更索引。
