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
- 2026-07-09:**data_service 未正式上线**，截至本日仅用于本地开发/minikube 验证，无外部调用方，也没有需要保留的旧协议、Redis 缓存或 `player_data` 表有效数据。因此本轮 PlayerData blob→强 schema 改造按开发期例外处理，可在停 data-service 后定向清 `pandora:data:player:*` 并 `DROP pandora_player.player_data`；该例外不适用于未来正式上线或产生有效数据后的 schema 演进。
- 2026-07-06:Battle DS 空场回收拍板「回收 + 宽限窗」双层方案(对齐业界 empty-server-timeout):DS 侧空场计时器自结算为主路径(UE 仓库待实现,建议 2~3min),后端 `ds_allocator` 按 `player_count==0` 持续超 `empty_battle_timeout`(默认 5m,须 > 断线重连窗口 ~30s)心跳内判 abandoned + 回收 + 段位回滚补偿兜底(已上线,复用心跳超时补偿链路)。**[proto]** `BattleStorageRecord` 新增 `empty_since_ms=11`(存储侧字段,加字段兼容演进,客户端无感知,无需 UE 同步)。契约见 `agones-dev.md` §3.2。
- 2026-07-06:matchmaker 两道 locator 离线判定门(成局最终门 findOfflineMembers + 队列在线扫除 livenessSweep)收进开关 `match.liveness_gate_enabled`,**默认关闭**:离线判定依赖 Hub DS 心跳捎带 `player_ids`(hub/v1 HeartbeatRequest)续期 locator HUB 位置,UE Hub DS 生产端尚未实现;先上线服务端会把在线玩家 30s 后误判离线、扫掉排队票据。**待 UE Hub DS 上报 player_ids 联发后才可开启**(开启路径已完整实现并有测试)。同批:hub_allocator `RefreshHubPresence` 改 goroutine + 独立 3s 超时(同 ds_allocator.refreshBattleLocations),locator 抖动不再拖慢 Hub DS 心跳响应。→ **2026-07-08 已开启**:两真实打包客户端实机联调验证保活链(UE Hub DS→hub_allocator→locator→Redis),K8s Redis 采样在线玩家 locator TTL 稳定 25~30s 回升不衰减;`matchmaker-dev.yaml`/`matchmaker-pve.yaml` 置 `liveness_gate_enabled: true`(cluster 配置由 gen_cluster_config.ps1 从此生成)。回归失败(在线玩家 TTL 单调掉 0)先关此开关再排查。**同日端到端验收通过**:①配置生效(configmap 重建 + matchmaker/matchmaker-pve 滚动重启,两 pod Ready);②离线扫死票——无 HUB 位置玩家入队,一轮 sweep 内 `liveness_sweep_reaped_ticket` 删票,Redis ticket/claim/queue 全清;③在线不误删——synthetic HUB 位置每 5s 续期挂队列 45s,9 次采样票据完好、无 reap 日志。液门(liveness gate)正式生效。残余(非阻塞):真机 UE 登录后挂队列的纯真机复验。
- 2026-07-07:根治「重启电脑/换模式后 500xx 端口被旧 compose 业务容器劫持」:`docker-compose.services.yml` 业务容器 `restart` 由 `unless-stopped` 改 `"no"`(dev 业务容器生命周期一律由一键脚本显式管理,不随 Docker Desktop 开机自启;dev.yml 基础设施保留 unless-stopped);`k8s_envoy_bridge.ps1` 三处加固——预检 `Stop-StaleComposeContainers` 自动 stop 发布 bridge 端口的 pandora-* 容器、端口检测扩到 0.0.0.0/::(docker 发布端口监听在 0.0.0.0,旧检测只查 127.0.0.1 看不到 → 双监听并存导致 Envoy 流量去向不确定)、占用者为 com.docker.backend 等 Docker 转发进程时拒绝 Stop-Process(会杀整个 Docker)。**与不停服滚动更新(不变量 16/17)无关**:灰度升级的载体是 k8s Deployment RollingUpdate,compose 只是本地 dev 环境且本身无滚动更新能力。流量切换时序、gRPC 长连接 L7 均衡、金丝雀灰度四阶段已补进 `zero-downtime-update.md` §6。终局方向(未做):envoy 部署进 k8s,消灭宿主 500xx 桥接层。
- 2026-07-08:滚动更新流量切换两项基础能力落地(zero-downtime-update.md §6.5 前两项):① `deploy/k8s/services/services.yaml` 20 个 Deployment 全部加原生 gRPC readinessProbe(打 grpc_health_v1,Kratos 默认注册、Stop 自动 NOT_SERVING;新 Pod 必须 SERVING 才进 Endpoints 接流量);② gRPC 连接轮换:`pkg/config.Grpc` 新增 `max_conn_age`/`max_conn_age_grace`,`pkg/grpcserver` 按配置挂 keepalive MaxConnectionAge(零值=关,行为不变),20 个服务 dev yaml 全量开 15m,ds_allocator 显式 grace 90s(盖过 AllocateBattle 同步等 DS ready 的 ~60s,防 GOAWAY 砍断在途分配)。验证:pkg + 18 个服务 module 全部 go build 通过、pkg/config 测试绿、kustomize 渲染 20 个 readinessProbe。剩余待补(扩多副本前):服务间 headless/L7 均衡、RollingUpdate 策略显式化,见 §6.5。
- 2026-07-08:**角色养成五件套(角色界面/装备更换/属性加点/天赋树/背包道具使用·出售)对客户端放行 + IDOR 加固**。核心结论:这五个都是**局外(meta)系统**,与 MOBA 战斗延迟零耦合(客户端走 Envoy→player/inventory 独立 gRPC 通道,DS 战斗内绝不同步回调 Go),后端 proto/表/biz/data/service 早已实现,真正缺口只是「安全地暴露给客户端」。改动:①`player`(:50002)/`inventory`(:50015)两 cluster 接进 Envoy edge(`deploy/envoy/envoy.yaml`,STRICT_DNS/V4_ONLY/http2,host.docker.internal,k8s 复用同文件经 bridge 转发);②两服务全 RPC 加 `jwt_authn` 需 `pandora_session`(R5 player_id 以 JWT sub 为准);③系统/内部方法双保险 403——Envoy 精确 path `direct_response 403`(player:UpdateMMR/GetMMR/UnlockHero/GrantAttributePoints/GrantTalentPoints;inventory:GrantItems/GrantInstances/FreezeForOrder/SettleAuctionMatch/SettlePlayerTrade/ReleaseEscrow)+ 服务层兜底。**player 服务 IDOR(OWASP A01)修复**:原先信任请求体 `player_id`,任意登录客户端可读写他人数据;仿 inventory 的 `callerPlayerID` 模式加三个纯鉴权辅助——`selfPlayerID`(客户端自助写:身份缺失→UNAUTHORIZED,body≠caller→PERMISSION_DENY,回落 caller)、`resolvePlayerID`(读,双模式:内网直连 callerID==0 信任 body,客户端强制自身)、`systemOnly`(callerID≠0→PERMISSION_DENY)。读/写/系统三类分流已套全 handler,`s.uc.*` 一律传解析后 `playerID` 不再用 `req.GetPlayerId()`。`GetProfile` 默认自查(安全默认;跨玩家看板将来另开 `ViewProfile`)。当前无 PlayerService 内网 gRPC 调用方(grep 无 NewPlayerServiceClient),改动不破坏既有链路(battle_result 的 GetMMR reader / matchmaker·DS 的 GetLoadout 快照注入均 callerID==0 走信任分支)。验证:`go build`/`go vet`/`go test ./...` 全绿(新增 `internal/service/auth_test.go` 覆盖三辅助分流),envoy.yaml yaml.v3 解析通过。**残余(UE/人工领域)**:UMG 面板调这些 RPC(需带 player SessionToken JWT,个人数据自查)属客户端侧,按 AGENTS.md §11 交 UE/Codex。**待确认**:「技能卡」若为独立于天赋(player_talents/SetTalents/GetTalents)的系统,可能是真实未来缺口。
- 2026-07-08:**延迟不变量固化**——局外系统放 Go 零战斗延迟,是架构决定不是调优结果:①客户端→Go 大厅连接(gRPC-Web/HTTP2)与客户端→DS 战斗连接(NetDriver/UDP)物理独立、不共享带宽与故障域;②DS 帧循环里没有对 Go 的同步调用(tick 全走 GAS/Replication,DS→Go 只剩 Heartbeat 5s/GetLoadout 开局一次/battle_result 局后一次,全独立 goroutine+5s 超时不阻塞主 tick);③唯一会真拖慢延迟的错误做法 = 让 DS 战斗中同步 RPC 大厅服务,守住「开局快照 + 局后上报」边界即永不发生。红线:任何"战斗内实时读写 player/inventory/economy"需求必须改造成开局快照或局后异步上报,否则推翻。落文档 `docs/design/ds-arch.md` §0.6(配套 §0.3/§0.5)。
- 2026-07-10:**Agones DS 回调拓扑本地/线上同构**——Fleet 的 5 处回调统一指向集群内
  `pandora-envoy.pandora.svc:8444` 且默认明文 `TLS=0`;minikube 自动部署/重载 in-cluster Envoy，
  online NetworkPolicy 仅允许带 Agones GameServer 标签的 Pod 访问 8444。宿主客户端面 8443 可按模式
  对局域网开放；未鉴权的宿主 DS 面 8444、admin 9901、基础设施与 20 个业务 gRPC 发布端口默认
  固定回环（特殊 Linux dev 环境须显式覆盖）。安全残留：方法白名单和
  NetworkPolicy 不等于 DS 身份认证，生产仍需 mTLS/ext_authz/短时效 DS token 并绑定 pod/match。
- 2026-07-10:**战斗 DS 并发容量监控 + 告警通知链路**落地。①`ds_allocator`(mode=agones)新增
  Fleet 容量巡检 `internal/biz/capacity.go`:定期 GET 通用 Fleet + 各 map_fleets 专属 Fleet 的
  status,暴露指标 `pandora_ds_allocator_fleet_{replicas,ready,allocated,usage_ratio}`(label=fleet),
  `allocated/replicas ≥ capacity_warn_ratio`(默认 0.8)打 Warn `ds_fleet_capacity_near_limit`、
  `ready==0` 打 Error `ds_fleet_capacity_exhausted`,状态变化才打 + 5m 重报降噪。配置
  `agones.capacity_watch_interval`(默认 30s,负值禁用)/`capacity_warn_ratio`;dev yaml +
  gen_cluster_config.ps1 in-cluster 模板同步;复用既有 RBAC `fleets: get`。②**告警出口唯一 =
  Grafana 统一告警**,业务服务只暴露指标/打日志,绝不直连通知端点(见 `docs/design/alerting.md`)。
  新增 `deploy/grafana/provisioning/alerting/`(contact-points/templates/notification-policies/rules):
  群通知走企微(原生 wecom)/飞书(需已验证 relay),个人推送走 ntfy(compose 内置
  `binwiederhier/ntfy` 服务,开 SQLite 磁盘缓存)。secret 保留 `$__env{}` 占位,本机 `dev.env`
  被 git 忽略,受跟踪的 `dev.env.example` 只放空占位;Grafana entrypoint 仅为非空群 env
  生成 receiver。有群时 warning→群、critical→群+ntfy;无群时均回退 ntfy。首批规则消费
  上述 DS 容量指标(warning near-limit / critical exhausted),非 Agones 模式 NoData 按 OK。
  验证:ds_allocator go build/vet/test 全绿(新增 capacity_test.go + ListFleetCapacities 测试);
  compose config、Grafana 11.3.1 空群/有群两种隔离 provisioning 均通过，本机 compose 已重建
  ntfy/Grafana 并经 API 确认联系点/规则/路由，ntfy 文案模板与 SQLite 跨重启回放实测通过。**待办**:飞书 relay、
  k8s Grafana 同步、ntfy 公网鉴权、Grafana 安全版升级后再开 DingDing、扩展更多规则(见 alerting.md §9)。

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

## 2026-07-10:DS 回调服务令牌认证(审核 P1 #1,已拍板并实现)

- 已拍板决策:DS→后端回调(:8444 七个白名单方法)补服务令牌认证——allocator 签发
  scope-bound HS256 JWT(battle 绑 match_id / hub 绑 pod),经 GameServer annotation
  `pandora.dev/ds-token` / env `PANDORA_DS_TOKEN` 下发,四服务 handler 用
  `middleware.DSCallbackGuard` 校验;Envoy :8444 盖 `x-pandora-ds-gateway` 标记头区分
  DS 面与内部直连。详见 `docs/design/decision-revisit-ds-callback-auth.md`。
- 边界:后端全部接线完成(pkg + ds_allocator/hub_allocator/player_locator/battle_result),
  `ds_auth.mode` 默认 **off**(行为与现状一致);proto 零改动。
- 客户端(UE DS 仓库)待同步:读 annotation/env 拿令牌,7 个回调方法带
  `authorization: Bearer`,Hub DS 监听 annotation 续期换新令牌;接完后先 permissive 再 enforce。
- 部署待办(Codex/人):重发 `deploy/k8s/agones/10-rbac-allocator.yaml`(gameservers 补
  patch 动词)、重滚两份 Envoy 配置(均已 `envoy --mode validate` OK)。
- 验证:5 模块 build/vet/test 全绿;enforce 端到端冒烟待 UE DS 接令牌后执行。

## 2026-07-11:部署/重置阻断项 C/D/E 修复，A/B 保持待决策

- 配置生成改为独立 staging 精确校验 20 份 YAML(+生产 JWKS)、自有文件事务发布/失败回滚、
  目标目录物理文件锁；Online 在 BuildPush 前生成本次独占快照，Secret 只取这 20 份配置，堵住
  共享 `run/cluster/etc` 被 dev/mock 并发覆盖后灌入生产的窗口。
- K8s 配置载体按 Secret→Deployment 全量 rollout→控制器/存活 Pod 无旧引用→删除旧 ConfigMap
  的顺序迁移；Resume 也逐个等待 20 个业务 Deployment。删除旧 ConfigMap 是明确回退边界，
  迁移前零副本 ReplicaSet revision 不再可直接 rollout undo。
- compose data_service reset 改为 checked 容器状态 + compose label 校验；stopped/absent 都需宿主停服
  确认，stop 后复查才允许清理，`-Restart` 不再因容器不存在而静默忽略；验证失败会确认停服后
  重新清缓存/删表，停服未知或保护清理失败均组合报错。
- 文档已纠正 DS 轮换 TTL 没有上限的事实，并新增玩家 JWT 轮换待决策记录。
- **仍阻塞、本轮 Codex 未改业务代码**:当前工作树已有 Hub token-exp 代际/玩家多 key 的部分接线，
  但尚未获决策批准；Hub 仍缺 Agones⇒enforce 的启动期硬耦合，且秒精度 exp 不是无碰撞 generation。
  玩家/DS 全集群 primary+additional 权威 key-set gate 与轮换算法/密钥权属也待人拍板。生产脚本暂时
  拒绝 additional_secrets，避免未批准的半链路投产。详见 `decision-revisit-ds-callback-auth.md` §6.4 与
  `decision-revisit-player-jwt-key-rotation.md`。

## 2026-07-12:DS callback auth Model B P1 收束；生产行为激活仍阻断

- 人已批准 Redis 唯一授权权威 + active/pending 两阶段方案。当前脏工作树已接 Hub/Battle 两类
  auth record、UID/epoch/gen/jti/kid/hash/writer 完整绑定、K8s UID+RV 条件投递、跨服务 active 门、
  login assignment fence，以及 UE active+staged token/Bearer/ACK 切换。这里的“已接”仅描述当前脏
  工作树，不代表生产行为已经获准激活或真集群验收通过。
- 独立反证后追加修复：Battle result receipt 先落库后允许 ended、GSA/PATCH 未知结果 fencing、
  `allocation_uncertain` 永久隔离与 UID 条件 Release 墓碑（**不能**用 sweep 猜测恢复）、Hub assignment
  future protobuf 字段保留、同实例轮换不重复占座、
  Hub/Battle 完整 tuple 紧急 quarantine + audit-only ops CLI、etcd required value+ModRevision 注册 CAS、
  后台 writer 在 capability 成功后才启动、Redis auth↔live projection 双向三周期审计、login 在线
  VerifyDSTicket 的 Guard→active/projection→assignment/roster→JTI 顺序、battle 票签发前
  player↔match roster 权威门，以及 Redis authority 下拒绝无凭据 `pandora.battle.result` consumer。
  locator 已明确 `InBattle` 后，Battle 签票权威失败会使 Login 返回 unavailable，不再错误继续分配
  Hub/占第二个 seat/写 LOGIN_PENDING；reconnect 地址取自同一次 Redis roster/active 快照，不再把
  locator 的旧 UID 地址与当前实例的授权证明拼接。
- 本机调试只允许 Windows `mode=local + off + legacy` 的精确 `local-off-v1` profile；仍签完整
  `(pod,随机 UID,epoch,gen,jti,exp,kid,writer=2)` 凭据。profile/pod 前缀/平台任一伪造均不能关闭在线
  admission。它是明确的非生产例外，不解决生产密钥投递或 Battle DS 的客户端侧空 roster 防御。
- UE 长驻 Hub 的本地 JTI 防重放已从永久无界 `TSet` 改为按票据 exp 回收的线程安全有界 map；
  满载不驱逐未过期值而是 fail-closed，`PreLoginAsync` 仅检查/清过期且不新增、真正接纳时才消费。
- UE 心跳响应现绑定“本请求实际 Bearer snapshot + 回调时仍为 current active”：Hub/Battle 的
  missing/wrong/stale ACK 都在 stop/drain/reload 前失败；activation 只有精确 staged ACK 成功提升后
  才处理命令，active=A/staged=B 时迟到 ACK(A) 不再误报成功。`grpc-status` 同时改为 canonical
  `0..16` 有界解析，整数溢出/前导零/重复/缺失均不能冒充 status 0。
- UE admission owner 不再直接信任平台 `FGuid` 的字段布局/宽松 parser；改由 vendored OpenSSL
  CSPRNG 生成并显式编码小写 RFC4122 UUIDv4，随机源失败在网络前拒绝，validator 逐字符检查
  36 长度/连字符/version/variant。Automation 首轮正是由 UUID 反例失败并触发此修复，不能把首轮
  10/11 误报为全绿。
- 本地最终验证：UUID 修复后的 PandoraEditor UBT 714/714 Succeeded（1164.39s）；headless
  `Pandora.Net` 11/11 + Battle terminal 1/1（合计 12/12）；PandoraServer UBT 824/824，
  `PandoraServer.exe` 链接成功（1132.36s）。服务端相关 Go test/vet、`buf lint`、两套 Kustomize render、
  Envoy config validate、PowerShell AST、生成物跨仓 SHA-256 与 `git diff --check` 也已通过。
  `-race` 因 `CGO_ENABLED=0` 未执行且未改环境；真 Redis Cluster/envtest/minikube、安全 `:8444`
  synthetic 与真 Hub/Battle 往返仍未验收。
- **禁止宣称可生产激活**：审计证实 required=1 尚未约束数据面，新 Model-B 与旧 legacy writer 会在
  普通滚动窗口混跑。需人拍板 blue/green prepare→quiesce→active（推荐）或逐路由 stage-only 门；
  同时仍缺 immutable image/Secret、etcd/Redis TLS+ACL、仓内固定 K8s :8444 synthetic，以及
  :8444 service-mesh mTLS/等价传输身份与机密性。当前 Fleet
  还未安全注入 `PANDORA_DS_TICKET_SECRET`：真实生产 signer 与 UE tracked dev 占位会导致全拒票，
  若生产沿用占位则直接失守，因此两种情况都不可上线。也不得把现有玩家 HS256 签名 secret 直接
  注入不可信 DS；须拍板 DSTicket 公钥验签或只走 online authority。
- 仍待人拍板：Hub active A→B 后旧 A 票据的 bounded `previous_admission` grace；Battle 结算与
  MySQL 同事务的 terminal-release outbox；Hub quarantine 后 assignment/UID 持久迁移；
  `allocation_uncertain` 权威 reconciler；locator 连续查询失败时选择“未知即拒绝”或版本化
  placement lease + Hub admission 最终门。现状只保证已明确 InBattle 后不会误分 Hub；locator 状态
  不可判定仍沿用既有弱依赖行为，因此也是生产 blocker。
  `tools/scripts/activate_ds_auth.ps1 -Apply` 已主动 fail-closed，只保留 audit。
- Claude Code 首轮独立只读审核仅覆盖 `hub_allocator` Model B 后端与 auth/middleware 抽检：未发现新的
  鉴权 P0/P1，确认过期方法名注释一处（已修）。其把心跳实报覆盖 reservation 定为 P2，但后续反证确认
  assignment 默认 30min、票据默认 5min、心跳 5s，且无逐 reservation ledger/到期退座；不同玩家可跨
  多轮累计有效票据，不能证明容量上限，改列 §7.16.4 P1 生产阻断。标准修复需人拍板独立 reservation
  lease/session 容量权威，当前未暗改。另修复等值 assignment 复用重签票却不刷新 Redis TTL 的缺陷，
  统一走完整 bytes CAS SET 并补剩 1s 回归。UE、Battle 全调用点、`-race`、真集群仍未被该轮 Claude
  审核覆盖。
- 经人于 2026-07-12 明确授权，由 Codex 仅为本次范围创建本地 Git 提交；未 push/tag，未碰生产，
  未写真实 secret。部署验证时误触发 Docker 自动拉取本机 `envoyproxy/envoy:v1.38.1`，发现后已停止
  后续 Docker 操作；该镜像保留或移除仍待人指示，不能再宣称“未改本机环境”。详见
  `docs/design/decision-revisit-ds-callback-auth.md` §7.14–§7.16。

## 2026-07-12:全代码审核后的 Codex 部署防线修复；业务阻断交 Claude Code

- online 发布代码已补不可变 SHA tag、首次 push 前的全 tag 不存在证明、push/registry digest 一致性、
  20 Deployment 与 2 Fleet digest pin、五 writer/Fleet 两层 annotation、rollout 后 spec/imageID 回读；
  旧 Allocated DS 只排空不强删。纯 helper/mutant 测试与 PowerShell AST 已通过，未访问远端。
- 新增 `dsauth-required` 只读线性预检及 uint32 参数防截断；test online 下 required key 缺失/非法/
  不可读会在远端写前停止。prod 因五 writer/预检尚未统一 custom CA+mTLS+ACL 最小权限身份，已在
  明文/无身份读取前硬阻断；没有调用现有 `BootstrapRequired`，其删 key 后回退风险仍待 Claude 修。
- 生产发布仍被 DSTicket B(公钥 keyset)或 C(online authority)未决硬门拦住；Fleet 禁止注入玩家
  HMAC/私钥。BuildPush 另被 registry native immutable-tag/create-only+发布锁未验证硬门拦住；
  clean tree/严格重建/provenance 与结构化 manifest mutant 已补。离线 tar 与 registry manifest digest
  等价性也未在真实 CRI 证明。
- Codex 按 `AGENTS.md §11.1` 未修改业务逻辑。player 属性点 `int32` 求和溢出与 auction 跨物品撮合
  仍是 P0;其余跨服务 P1 修复队列及验收反例见
  `docs/design/code-audit-blockers-claude-handoff-20260712.md`,交 Claude Code 实现并独立审核。

## 2026-07-12:拍卖缓存正确性与属性 MySQL 集成测试阻断修复（待 Claude Code 复审）

- 人明确授权 Codex 仅本次修复这组业务阻断。拍卖撮合候选改由 MySQL 按
  `market_id + item_config_id + side + active status` 权威选择，查询直接排除 incoming owner，
  按价格/`order_id` 时间优先；Redis ZSET 退为 best-effort 旧版本兼容缓存。删除临时移出/放回、
  Redis reconcile SET、10,000 截断重建和扫描上限路径，缓存 `ZADD/ZREM` 失败不再让已冻结且
  已持久化订单报失败或永久不可见。128 条异物品/自有前缀、Redis 全挂、缓存缺失、撤单降级、
  价格时间优先用例均通过。
- 新增 additive-only `idx_instrument_match` 与 `pandora_auction/000002` 迁移；真实 MySQL 8.4 已验证
  init 已含索引时幂等、老库无索引时 `LOCK=NONE` 新增均成功。安全发布要求先扩索引，再蓝绿
  一次切流并排空漏洞旧实例；详见 `decision-revisit-auction-match-authority.md`。
- 属性 repo MySQL 测试改为拒绝带库名 DSN、随机临时库、严格 cleanup；用触发器在属性 upsert 后
  强制最终扣点失败，验证完整事务回滚；用外部行锁与 InnoDB 锁等待视图确定两个 writer 同时阻塞
  在首个 `SELECT ... FOR UPDATE`，真实 MySQL 8.4 验证恰一成功一余额不足。无 DSN 仍明确 SKIP，
  不再把 SKIP 宣称为真实事务通过。
- 两服务 `go test -count=1 ./...` 与 `go vet ./...` 已通过；未 commit/push、未碰生产、未写真实 secret。

## 2026-07-12:Player / Inventory / Auction 业务阻断收束（待 Claude Code 最终复审）

- 人明确授权 Codex 修复本批业务阻断。player 属性扣点改为事务内宽类型总和/重复属性/列上界校验；
  inventory 新增严格快照校验且可并发幂等补冻的 `EnsureAuctionEscrow`。两者真实 MySQL 提交、回滚、
  溢出和并发用例均实际执行通过。
- auction 撮合与幂等权威收敛到 MySQL：owner coordinator registry 返回唯一 canonical order，跨片扫描/
  补行移出 coordinator 事务；持久 shard topology 门禁阻断片数、顺序和目标库漂移。当前明确仅支持
  单库或 2 片，`N>2` 重分片仍禁止。
- 成交改为 MySQL Reserve→inventory 幂等 Settle→持久 release/event outbox；资产与 Kafka worker
  分离，ready escrow 释放前置，事件另以双方 `release_pending` 为持久屏障；弱 audit 走有界非阻塞
  队列。`passive_warmup` 保证 green 与旧 matcher 共存时不写、不补偿。
- `000002` 以单次 `event_pending DEFAULT 1` 覆盖迁移中旧写并受控重放历史成交，无表锁/无界 DML；
  MySQL 8.4 fresh/old schema 全量一致，首跑/复跑均 `version=2 dirty=false`。
- player、inventory、auction 的完整 test/vet 与真实 MySQL 用例全绿。`CGO_ENABLED=0`，race、真 Kafka/
  inventory gRPC 故障注入、生产规模 MDL/吞吐及真实蓝绿仍属外部验收门。Claude 复审清单见
  `docs/design/auction-blockers-claude-review-20260712-final.md`；未 commit/push、未碰生产、未写真实 secret。

## 2026-07-13:DSTicket v2 与不停服轮换/普通发布防线收束（生产激活仍阻断）

- 人明确授权 Codex 修复本轮本地审核问题。Login 已严格签发/校验 RS256 `dst_ver=2` 票据，绑定
  DS UID/epoch、assignment、release track 等权威声明并限制最大 TTL 180s；四 signer 私钥改为
  revisioned immutable Secret、非 root UID/GID/fsGroup 10001 与 0440，只向 Login/DS 投递对应公钥 JWKS。
- 新增独立 `stage -> promote -> retire` 轮换流程：K1/K2 overlap、四 signer 逐个 rollout、全部 Fleet/
  GameServer/Pod 与历史 controller 引用闭包、apiserver activation 时间起算 225s 后退役 K1；旧 key/
  config 不自动删除，也不强杀 GameServer。普通 online 发布与轮换共用 create-only immutable 操作锁，
  以 UID/resourceVersion CAS 释放；崩溃残留锁只允许人工审计，不按本机时间抢锁。
- 轮换/发布门禁已覆盖 direct/projected/env/envFrom/init/ephemeral/CSI、畸形保留前缀、影子 config/
  signer/JWKS consumer、YAML 真实反序列化路径、fixed/phase Secret 全量 data 投影与 marker 历史配对；
  activation/terminal/fixed handoff 的远端写入前均紧邻复证精确运行态和锁身份。
- Social Guild 的 TiDB counter/schema migration 已保持新旧实例混跑兼容并补并发/自愈用例；Envoy 对
  七个 Inventory internal RPC 已精确 403。Login/Social/迁移工具及 `pkg/auth`/`dsticketkeys` 的
  test/vet/build、7 组 PowerShell 契约、38 脚本 AST、两套 Kustomize render 与 `git diff --check`
  均通过；离线业务镜像包已按 host 模式重建并复查为未过期、精确 20 tags。
- **仍禁止宣称可生产激活**：真集群/UE K1→K2→K2-only（含 K1 旧票耗尽）E2E 未完成；本轮
  post-redaction K1 尚未到达认证入口，K2 未执行，也未写真实 secret、apply 生产、commit/push。
  `pkg/auth`、`services/account/login`、`services/social/guild` 的离线 linux/amd64 `-race` 已通过；Guild
  `internal/data` 12 个真实 MySQL 集成/并发场景与 `tools/migrate` 2 个 Social v2 场景已复跑通过，临时库
  清零。Inventory 的 Istio 方案 A 已获人批准，但仅交付独立静态候选，不能冒充已完成服务间鉴权或生产激活。

## 2026-07-13:Battle Model-B 正常结算持久终态回收（方案 B）

- `battle_result` 将结算、玩家更新与完整 terminal-release proof 放入同一 MySQL 事务；DB commit
  失败不返回 OK，commit 后 Redis receipt 即时失败仍由 durable outbox 恢复。旧 credential 已提交后，
  新 active credential 的幂等重试不替换旧 proof、不增加第二行。
- worker 落地两阶段状态机：pending 先以 exact proof 建永久 Redis terminal/receipt 墓碑并执行
  Kubernetes UID precondition release，再 CAS `released_at_ms`；released 行只走
  `completed-finalize` 恢复墓碑 TTL，绝不再删 K8s，最后仅以 `released_at_ms>0` 删除 outbox。
  response loss、DB mark/delete failure、跨完整 TTL 与两个 worker 并发 CAS/finalize 均有回归测试。
- additive migration 与启动 schema gate 精确核验全部列/索引、`ENGINE=InnoDB`、
  `utf8mb4_0900_ai_ci`、`released_at_ms NOT NULL DEFAULT 0`；mutant 和隔离 MySQL 8.4 集成测试通过。
  Redis authority 生产配置拒绝无凭据 `pandora.battle.result`，match-id-only Model-B release 拒绝；
  online 内部 ReleaseBattle 另以 battle-result SPIFFE/mTLS exact method policy 收口，不暴露到 `:8444`。
- 本轮 battle_result/ds_allocator 全量 Go test/vet、并发用例 50 次、mesh/config 契约均通过；未
  commit/push、未 Apply 生产、未写真实 secret。上线前仍须先执行 migration、接入 online component，
  并通过 §7.15 blue/green/真实集群 synthetic 激活门。

## 2026-07-13:Inventory 服务身份方案 A 获批并交付独立静态候选（普通 online 未接线）

- 人已明确批准 `docs/design/decision-revisit-internal-service-auth.md` 方案 A：revision 化 Istio
  STRICT mTLS + SPIFFE + exact AuthorizationPolicy 为统一最终身份层，Inventory 是第一条落地链。
  本轮未安装 Istio、未执行 Kubernetes apply、未写真实 secret、未碰生产。
- 独立静态候选包含六个 ServiceAccount、纯 revision sidecar、Inventory `grpc/appProtocol`、
  9 个系统 allow / 26 个 system deny 补集、edge 6 个玩家 allow / 7 个 system deny 补集、STRICT 与
  observe 分层策略、Inventory 专用 L4 NetworkPolicy。内部六 workload 与外部 edge 均以
  Deployment→ReplicaSet→Pod VAP 锁 owner、受保护 SA、token/capability、sidecar 与流量截获；生产最低
  Kubernetes 1.30。candidate 安全对象由测试按审核 hash 精确比对。共享静态 component 唯一拥有
  battle-result SA；Inventory 与 DS-terminal 双候选组合只引入一次，DS-terminal 自行声明两端 revision
  patch，不依赖 Inventory identity component。
- 只读 helper/契约可覆盖完整 live Pod labels、protected SA 全量占用（含 terminating）、
  canonical green `battle-result-ds-auth-green`、edge owner/容器/image、创建时 RS labels、唯一 Istio
  injector、actual revision、MeshConfig canonical 表示、root namespace additive policy、VAP controller
  收敛、STRICT、Service 与 NetworkPolicy；但这些 helper 未接入 ordinary `start.ps1`。
- 默认 online kustomization 不引用 shared identity/Inventory/DS-terminal component，普通 online NetworkPolicy 未改，
  `start.ps1` 不加载 Inventory helper、不接收 Istio revision、不生成 runtime patch，也不执行 Inventory
  preflight。静态候选不能由普通发布误 apply；未来接线必须重新审核。
- 可伪造/重放的 `pandora.inventory-mesh-audit/v1` 已永久 hard-fail。真实 Kubernetes 尚未产生短时
  `observeEvidence` 与 active ALLOW 新 generation/RV 后的 `activeAllowEvidence`，也未逐 proxy 证明 xDS
  传播。首次激活必须完成 PERMISSIVE→identity→gate→observe→active ALLOW→STRICT 独立阶段，不能把
  单个 `workflows_ok` 布尔当验收。ordinary online 当前的全局零写门来自独立 DSTicket K1→K2→K2-only
  真实 Kubernetes/UE E2E 边界，不代表 Inventory 已接线。
- 本地通过 `inventory_mesh_contract_test.ps1`（含 wildcard、额外 policy、sidecar/token/旁加载、
  MeshConfig flow/缩进/merge、candidate broad-policy mutants）、`ds_terminal_mesh_contract_test.ps1`、
  `online_manifest_contract_test.ps1` 的 B 收口断言、Kustomize render 与 PowerShell AST；`pandora-agones`
  API server 对 7 个 VAP + 7 个 Binding 的 server dry-run/CEL 编译全部通过。`pkg/auth`、Login、Guild 的
  离线 linux/amd64 `-race`，以及 Guild 12 个真实 MySQL 集成/并发场景、Social v2 2 个迁移场景已通过；
  post-redaction K1 尚未到达认证入口，K2 未执行。
  真实 Kubernetes/外部 edge/metrics/probe/CNI/五条补偿链滚动验收尚未执行，不宣称生产可激活。

## 2026-07-14:battle 混合模式退役(Windows DS 只保留 local 断点调试)

- 决策(`docs/design/decision-revisit-retire-battle-mode.md`,推翻 2026-07-01
  `decision-revisit-battle-services-in-docker.md`):Windows DS 只在 `start.ps1 -Mode local`
  (断点调试)下由宿主 exec 启动;其他一切要真 DS 的场景一律 k8s + Agones(Linux DS,
  `-Mode k8s` / 内网服务器一键启动-k8s集群.cmd)。`docker`/`intranet` 维持 DS=mock 不变。
- 已删双击入口:`策划一键启动-含战斗.cmd`、`内网服务器一键启动-含战斗.cmd`。两个停止 .cmd
  (`策划一键停止.cmd`、`内网服务器一键停止.cmd`)保留,仅用于清理旧机器遗留的 battle 栈。
- `start.ps1 -Mode battle` 启动/Resume 一律拒绝并指路 k8s(exit 1);`-Down`/`-Status` 保留
  (清理/查看遗留环境),`-Reset` 只清理不再重启。`play.ps1 -Battle` 启动同样拒绝,仅
  `-Battle -Stop`/`-Battle -Status` 可用;play.ps1 删除 battle 专属函数
  (Resolve-LanIp 副本/Get-LocalDsExePath/Confirm-HubDsUp/Resolve-LocalDsExe/
  Ensure-GoInstalled/Test-BattlePrerequisites)。`gen_cluster_config.ps1 -HostAllocators`
  参数暂留(已无调用方,下次触碰该脚本时移除)。
- 文档同步:README、`docs/ops/planner-quickstart.md`、`deploy/offline-images/README.md`、
  `tools/scripts/README.md`、export/import_images.ps1 提示语均已改口径;旧决策文档头部已标
  「已被推翻(2026-07-14)」。
- 验证:两个 ps1 通过 AST 解析零错误;`start.ps1 -Mode battle` 与 `play.ps1 -Battle` 冒烟
  确认拒绝且 exit 1;`play.ps1 -Battle -Status` 透传正常。未 commit/push(Codex 收尾)。

## 2026-07-14:任意时点断线/切后台 DS 迁移复核(未闭环)

- 复核范围覆盖 Login/SelectRole/Hub travel+Admission、排队/确认/分配/READY、Battle 首次握手、局内
  断线、杀进程重登与结算回 Hub。已确认 Battle-aware Login、B1 locator fail-closed、Hub
  reservation/session ledger、matchmaker liveness/claim healing、allocator/outbox 和 locator fence 等
  服务端恢复骨架存在；这些机制不能等同于“任意时间点绝不卡死”。
- 发现安全阻断：`IssueDSTicket(hub)` 直接 `ResolveHubEndpoint → AssignHub`,绕过 Login 的
  locator/B1/LOGIN_PENDING 门。客户端掉线而 Battle DS 健康时,roster 会继续续租 BATTLE；当前客户端
  30s 超时却直接申请 Hub 票。Hub admission 又未核 placement,可出现玩家已进 Hub、locator 仍为 BATTLE
  的双归属/后续匹配异常。
- 发现客户端阻断：READY 在无 LocalPlayerController 时先写去重态并停轮询,后续同地址不再连接；
  杀进程后 Login 直连 Battle 未恢复 MatchModel 的 match_id/Hub 上下文,结算回 Hub 可直接失败；
  `ReturnToHubDs` 在请求前清匹配态,失败后重试会丢 fence。前后台/HTTP 黑洞、真实 UDP Admission、
  杀进程全矩阵仍无 UE 自动化 E2E。
- 发现匹配持久恢复阻断：StartMatch 三步写的失败回滚复用玩家请求 ctx,可留未入队 body+claim；最后一人
  Confirm 后的 DS 分配/READY/locator/push 仍同步绑在该玩家 RPC ctx；allocator error 未先 CAS FAILED,
  expire scanner 遇瞬态锁错误却移除 active。上述窗口会让 QUEUEING/ALLOCATING 状态脱离恢复索引直到
  30min TTL。结算后的 match claim 释放又是 best-effort,DB 已提交而释放失败时幂等重报不会重试清理,
  玩家回 Hub 后仍可能被 AlreadyMatching 挡住。
- B1 只覆盖 locator 查询报错的 fail-closed；若 DS→locator best-effort 刷新连续失败到 key 正常过期,
  Login 会把“未找到 BATTLE”当成可进 Hub,不会用 live roster 作第二证明。签 Hub 票与 Hub Admission 仍需
  统一的权威 placement/terminal 最终门。
- 验证：account login、player locator、matchmaker、hub allocator、ds allocator、battle result 六个相关
  Go module 的现有测试均通过；本轮另复跑相关 biz/data/service 目标包也全绿。现有测试未覆盖上述
  ctx-cancel、active-index、release-response-loss 与 Hub/Battle 双归属反例,所以不能据此改判为已闭环。
- 已纠正 `battle-reconnect.md` §6/§7 的旧绝对结论,补“不卡死”定义、阻断反例和必跑矩阵；同步更新
  `ds-arch.md` §9.2 与 `decision-revisit-ds-callback-auth.md` §7.16.3。当前结论明确为**代码尚未全部完成**；
  本轮仅做审计与文档收口,未修改服务端/客户端业务代码,未 commit/push。

## 2026-07-15：任意时点 DS 恢复代码收口（本地全绿，真环境仍阻断发布）

- 服务端已以版本化 placement + immutable source-departure proof 形成 Hub/Battle 唯一准入门：
  UNKNOWN 始终 retryable 且零 seat/ticket/spawn，Hub/Battle Admission 必须在 spawn/READY 前提交 exact
  version/operation/target/source 证明。Login `ResumeContext` 覆盖 Hub、排队、确认、分配、Ready 和对局。
- Matchmaker 已把 formation/allocation/release 改为可重放持久 saga；StartMatch 在任何副作用前与
  Agones 外调前都检查 STABLE_HUB。allocation 新增 exact `REQUESTING→ABORTING→FAILED`，
  payload-bound 独立 HMAC 与持久 abort journal；UNKNOWN/ACK loss 不抢先 FAILED/requeue。BattleResult
  release outbox、compare-delete claim/ticket 与 active index reconciler 已收口。
- Model-B 回收必须同时持有真实 GameServer UID + Pod UID；新分配在 usecase/finalize 双硬门
  拒绝空 PodUID。正常结算、empty/stale、abort、preactive 的旧记录都在 terminal fence 前做
  exact K8s 回填。pre-Prepare `instance_epoch=0` 改为专用 fenced release，unknown 保留永久 fence，
  成功才 purge，不伪造玩家 teardown proof。late abort 同时要求 teardown 与 Kafka ACK 后 lifecycle
  两份 full-target proof。
- 发布流程新增只读 legacy PodUID 三阶段证明：`prepare` 在排空前审计存量，`drained` 在 blue writer=0 +
  capability empty + drain marker 后审计零写窗口，`final` 在 green exact capability/strict writer 已启动但
  Service 尚未切换前再次审计。三份 Job 绑定同一 RunId、immutable image/config、Redis identity/topology；
  final PASS 前不切 Service、不 CAS epoch，epoch=2 审计禁止事后创建证据。临时只读 Redis ACL 身份在
  CAS 后必须由独立控制身份精确 `DELUSER` 并回读 absent，cleanup pending 不算发布完成。
- Redis `BattleStorageRecord` 的 23 个生产写点（含独立 quarantine 命令）统一进入不可逆 strict Model-B
  mutation gate：新/重写记录必须带完整 PodUID，legacy 只允许 exact PodUID backfill，未知 protobuf bytes
  保留且 PodUID 不可变。etcd target 值固定为 `2@ds-auth-v2-pod-uid-write-invariant-v1`，五个 writer 的
  exact feature policy 同时绑定初读、capability 注册事务、watch 与 activation record；旧 numeric epoch=2
  binary 和新 binary 遇裸 `2` 都 fail-closed，关闭 feature-only rollback 窗口。
- UE 已以 `UMyDsRecoveryCoordinator` 作唯一 DS `ClientTravel` writer，generation/request-seq/phase 拒绝
  迟到回调；30s 只改 UI/退避。Battle DS 的 Controller/Pawn/World/weak-ledger census 与 exact ABA eviction
  已落地，无法归因的物理对象 fail-closed。
- 最终本地验证：`go.work` 29/29 module test + vet；Proto lint 与两次生成 diff 确定；
  activation/cluster 合同、PowerShell AST、services/online kustomize、`git diff --check` 通过；
  PandoraEditor 725/725、PandoraServer 577/577；UE DsRecovery 5/5、DSAuth 11/11。race 因 CGO=0/无 gcc
  未跑；online manifest live API 因本机 kubeconfig `127.0.0.1:59751` 不可达而 BLOCKED-ENV。
- 仍不允许宣称生产已证明“任意时点绝不卡死”：仓库尚未接入能覆盖整个 preflight→CAS 窗口的真实
  Redis topology-change/failover/reshard lease provider 与信任根。这不是本机环境偶发失败，而是外部
  控制面能力尚未接线；fresh/retry Activate、Go CLI/core CAS 与普通 online release 当前都在任何
  create/patch/scale/build/push/apply 前 fail-closed。产线 placement/PodUID preflight 以及真 Redis
  Cluster/K8s/Agones/UDP/移动端前后台故障矩阵也尚未执行。如果旧空 PodUID record 的 K8s 不可变证据
  已丢失，代码只会保留明确 retryable fence，发布前需可审计迁移/清退，禁止猜 UID。

## 2026-07-15：Hub successor policy V3 发布交接

- successor lease 新 writer 由库自动写 `supported_policy_generation/id=V3`，并把实际初读 required 写入
  `acquired_policy_generation/id`；V3 activation 对五类 writer 的 compiled support、V2 staging acquire、
  exact features/Pod UID/digest 与 Hub count=1 全部 fail-closed。V2→V3 是同 writer epoch 的 policy-only
  CAS；fresh local 使用 missing→V3 zero-writer 单事务 genesis，Resume 必须验证同事务 immutable record。
- 平台必须通过强制 HTTPS+mTLS+auth 的 `prepare_hub_successor_policy.ps1` 完成 V2 staging：真实 apply 前
  server-side dry-run 并逐个验证五个合并后 Deployment 的 identity/template/image/selector/count，强制
  locator preflight 与 Hub `replicas=1 + Recreate`，再 create-only 写 exact immutable
  `pandora-ds-auth-policy-v3-evidence`。prepare/activate 都会在 Endpoint 0/1 门前验证 Hub Service 的 exact
  green selector/ClusterIP/50021，避免 selector 漂移伪造零 Endpoint；既有 marker 只能精确回读，不能覆盖。
- `activate_hub_successor_policy.ps1` 已支持崩溃续跑：required=V2 才执行 pre-CAS 门/CAS；required=V3
  从 record-only proof 继续。post-CAS 从固定五个 canonical Deployment/selector/owner chain 重新派生当前
  live UID（不把历史 marker UID 当永久 allowlist），要求唯一业务 container Running+imageID、所有 capability
  acquired=V3、Hub ready Endpoint 精确 1 个且 UID 唯一匹配；最后再次 capability audit 并写 immutable
  completion marker。required=V3 的只读 Audit 与普通 online release 也必须验证该 completion marker 并把其
  UID/resourceVersion 纳入窗口内不可漂移基线，缺失不得假绿。完整契约见 `docs/design/battle-reconnect.md` §7.8。

## 2026-07-16：no-freeze 需求固化 + 双仓库复核 + 前台恢复缺陷修复

- 需求固化：「进 Hub DS / 匹配进 Battle DS 任意阶段切后台或断网，回来不卡死且能正确回到唯一权威
  DS」升级为需求级不变量，写入 `CLAUDE.md §9.19` 与 `battle-reconnect.md §7.11`；违反直接拒 PR。
- 复核范围：服务端 2026-07-14 起全部提交（8ab6c59→1c21311）+ 未提交 §7.10 修复；UE 客户端
  r1090–r1130（luhailong）+ 未提交 Coordinator/OnlineSession/超时兜底工作树。服务端
  login / player_locator / ds_allocator / hub_allocator / matchmaker 与 `pkg/placement`、
  `pkg/battleabort` 全部 `go build + go test` 绿（§7.10 `exactSamePreparedBattlePending` 修复含
  正/负例）。客户端 unary 超时钳制 5–300s、完成回调恰好一次、Coordinator 各 phase 均有驱动源，
  OnlineSession exact-driver 清理复核通过。
- 修复一处 UE 前台恢复缺陷：前台事件原来无条件重启权威重查，从未登录（登录页/离线本地流程）或
  显式登出/放弃重连后会经 `RenewSessionForRecovery` 无凭据分支强制打开登录关卡，把玩家从当前界面
  踢走。新增 `ShouldRestartRecoveryOnForeground` 纯判定门（session/重连中/缓存凭据/ReturnHub fence
  全空则跳过），`ReturnToLogin`/`AbandonBattleReconnect` 显式清 session+缓存凭据；新增 Automation 用例
  `DsRecovery.ForegroundRestartRequiresRecoverableContext`。PandoraServer Win64 编译通过（Pandora
  runtime module 含全部修复）；PandoraEditor 目标因本机编辑器 Live Coding 占用未能本轮编译，
  PandoraTests 新用例待编辑器空闲后随 Editor 构建验证。真实移动端前后台/断网矩阵仍是发布前验收项。

## 2026-07-17：脑裂根治落地 — DS 授权租约 fencing + 服务端再入屏障

- 定义并落地「一人两 DS」的标准最简根治（lease-shorter-than-failure-detector fencing，
  完整契约见 `battle-reconnect.md §8`，常量集中 `pkg/placement`）：
  ① DS 短租约自我 fencing：连续 20s 拿不到绑定 active 凭据的权威心跳响应 → DS 除拒新玩家外，
  Kick 全部存量已准入玩家、销毁玩家 Pawn、拒 pending 准入（UE `PandoraDSBackendSubsystem`
  1s watchdog + `OnAuthorityLeaseLost`，`PandoraDSGameModeBase::FenceAllAdmittedPlayersForAuthorityLoss`，
  Hub/Battle 共用）；② 服务端再入屏障：`abandoned` 只有在 `last_heartbeat + 25s` 后才算 Terminal
  （login `InspectBattleRoute`，四个 Hub 再入门全部继承）；locator TTL 与 hub `heartbeat_timeout`
  机械下限 ≥ 25s。`ended`（DS 自报正常终局）不经屏障，正常结算零延迟。
- 迟到响应防护：DS 心跳 RPC 有界超时 4s（迟到响应不得刷新租约锚点）；心跳间隔钳 [1,5]s；
  UE Automation 硬断言不等式 `20(租约)+1(检测)+4(在途) ≤ 25(屏障)`。
- 测试：服务端 login/hub_allocator/player_locator/ds_allocator/matchmaker/pkg 全 `go test` 绿
  （屏障四态正负例、两处下限地板、locator TTL 地板修正既有用例）；UE 新增
  `Pandora.Auth.DSTicketV2.AuthorityFencePolicy`，编译与真机分区注入由用户执行。
- 边界如实记录：CLAUDE.md §9.22 的每玩家 `owner_epoch` 线性一致 owner authority 仍是文档化
  目标；当前由 session JTI 围栏 + DSTicket v2 exact 实例绑定 + locator match fence 提供迟到写
  防护。可达传输层的秒级转移重叠窗口依赖 eviction order 送达，列为已知边界与发布前验收项。

## 2026-07-18：全量双规则审计 + 三项已知边界闭环（面向上线加固，battle-reconnect.md §8.6）

- 全量 inline 审计两条需求级规则（任意时点不卡死+安全重连；严格单会话/根除脑裂）：四个再入门、
  两个机械地板、DS 侧 fencing/GameMode Kick、PostLogin 同机顶号、会话 JTI 单写者、matchmaker
  入队门（BattleGateFailOpen 默认 fail-closed）、ShrinkHubTTL Lua 守卫逐一读码核实；五服务 +
  全 pkg `go test -count=1` 全绿。多 agent workflow 因订阅 session limit 全灭，结论均来自 inline 取证。
- 边界 1 闭环——时钟漂移零预留：`DSFenceSkewMarginSeconds` 5→7（屏障 25s→27s），显式留出
  ≥2s 服务间时钟漂移预算；UE 镜像常量 `ServerFenceReentryBarrierSeconds=27` /
  `InterServiceClockSkewReserveSeconds=2`，Automation 断言升级为 `20+1+4+2 ≤ 27`；
  屏障边界测试改 27s 并新增「25s(旧屏障值)仍须 UNKNOWN」防回退负例。代价：abandoned 再入 +2s。
- 边界 2 闭环——SelectRole 会话现行性：免 proto 字段，从 Envoy jwt_authn 验签后重写的
  `x-pandora-jwt-payload`（入站无条件剥离，不可伪造）提取 jti，`RequireCurrentSessionJTI` 与
  IssueDSTicket 同门判定；jti 缺失时 B1 严格档 fail-closed。`pkg/middleware` 纯函数解析 +
  login biz 七态表驱动测试。顶号后旧设备四条拿票路径（Login/Resume/IssueDSTicket/SelectRole）
  现全部封死。
- 边界 3 闭环——挂起恢复首帧积压输入（§9.22）：`FWorldDelegates::OnWorldTickStart` 先于
  NetDriver TickDispatch（引擎 LevelTick.cpp 顺序），DS 订阅后每帧在分发积压包前复查租约；
  1s watchdog 保留兜底，共用 edge-trigger。
- 文档：battle-reconnect.md §8 全量改 27s，新增 §8.6 加固记录与 §8.5 分区注入验收执行清单
  （NetworkPolicy 步骤 + T0 时序断言 + 顶号矩阵）。go-services.md §2.6 同步。
- 交接：UE 编译 + `Pandora.Auth.DSTicketV2.AuthorityFencePolicy` 由用户执行（Live Coding 约束）；
  分区注入清单为发布前必跑验收，代码级不能替代。git/svn 提交由用户复核后执行。

## 2026-07-20：队伍邀请恢复（Kafka 启动门禁 + push/team 协议同版滚动）

- 现场根因闭环：旧 team 在 Kafka 尚未就绪时 producer 初始化失败后仍以 `pusher=nil` Ready，后续
  Invite RPC 虽返回 OK 但通知永久静默丢弃；同时运行中的 team/push 仍为 `4193897`，落后于已发布
  的 `event_type=1 + TeamInviteEvent` 客户端协议，单独重启旧 team 也无法恢复新客户端邀请 UI。
- 代码修复：配置了 `kafka.brokers` 时，team producer 改为启动强依赖，失败在 gRPC Ready 前退出，
  交给编排器重试；只有显式空 brokers 才进入有醒目标志的纯 RPC dev-only 模式。补齐空配置、构造
  失败、nil producer、成功装配/事件类型透传测试；team Deployment 显式 `maxUnavailable=0`、
  `maxSurge=1`，保证失败的新 Pod 不替换仍可服务的旧 Pod。push 补 header 缺失为 0、header=1 原样
  透传两条回归断言，协议与发布文档同步为严格 `push reader → team dual writer → 新客户端` 顺序。
- 验证：team、push、`pkg` 各自 `go test -count=1 ./...` 全绿，K8s manifest server dry-run 通过；
  不可达 Kafka 的隔离进程以 exit 1 退出，出现 `kafka_producer_required_but_unavailable`、无
  `service_ready`、全程 0 个 TCP listener。仅重建/替换 push 与 team，运行版本均为
  `6aff5dd-dirty`，两 Pod imageID 与 minikube 新镜像一致，其余 18 个业务 Pod UID 未变化。
- 真链路验收：被邀请方先建立 Push Subscribe，随后 CreateTeam/Invite 经 team → Kafka → push 实际
  收到同一 topic 的 `eventType=1` 专属帧和 `eventType=0` legacy 帧；AcceptInvite 与 GetTeam 均确认
  双成员，测试队伍随后解散且两玩家 `GetMyTeam.hasTeamMsg=false`。滚动导致失效的宿主 50010/50014
  port-forward 已替换并分别反射到新 `TeamInviteEvent` / `PushFrame.event_type`。
- 已知边界如实保留：本次根治的是“启动时 producer 失败后永久 nil”和协议版本错配；运行期间
  Kafka send 重试耗尽仍只有错误日志。若要把邀请承诺为端到端 durable delivery，仍需 outbox 或
  被邀请方可查询的权威邀请列表，不能用本次启动门禁代替。

## 2026-07-20:组队匹配 READY 通知闭环(matchmaker Kafka 启动门禁 + READY 补推 + UE 等待 watchdog)

- 现场根因闭环:matchmaker-pve 启动时 Kafka 未就绪,producer 一次性初始化失败后以 `pusher=nil`
  继续服务,整个 Pod 生命周期 `pandora.match.progress` 静默丢弃(现场 4 个 partition end offset
  均为 0)。组队匹配只有队长持有 StartMatch 返回的 match_id 可轮询兜底,非队长成员唯一通道就是
  该推送 → 队长进 Battle、队员永远停在 Hub(match 14537609598533632)。
- 修复一(启动门禁,与 2026-07-20 team 同口径):配置 `kafka.brokers` 时 matchmaker producer 为
  启动强依赖,初始化失败在对外 Ready 前 exit;显式空 brokers 保留 dev 纯轮询模式。新增
  `initializeMatchPublication` + cmd 层 4 条测试。
- 修复二(READY 推送 at-least-once):新机械不变量「READY ∈ active ZSET ⟺ READY 推送交付
  未确认」。READY CAS 后推送改 `pushReadyStrict`(聚合错误),全员成功才 RemoveActive;滞留
  READY 由撮合循环 `finalizeReadyMatch` 幂等补推(全员重签新 jti,与 refreshBattleTicket 同
  口径),覆盖「READY 提交后、推送前崩溃」与「推送时 Kafka 不可用」两个窗口,上限 match TTL。
  `expireOnce`(keepActive)与 `reconcileActiveOnce`(不清不建)同步改语义。回归:
  `ready_push_saga_test.go` 两条(推送失败保留 active + 重启后补推带个人票据;过期扫描不误清)。
  matchmaker 全包 `go test` 全绿。
- 修复三(UE 侧最后闭环,Pandora-Client-SVN):`UMyMatchModel` 新增组队匹配等待 watchdog——
  订阅 `UMyTeamModel::OnTeamSnapshotChanged`,本队 `TEAM_STATE_MATCHING` 且本地无匹配归属期间
  以 `TeamMatchStandbyCheckIntervalSeconds`(默认 5s)循环检查,到期仍无归属且 Coordinator 空闲
  (Phase==Idle,不抢流不换代)则触发 `RestartAuthoritativeRecovery(Resume)`——与「收到无归属
  推送」同一条权威恢复路径,由 ResumeContext 决定 QUEUED/CONFIRM/READY 或明确 Hub。World 切换
  后 OnWorldBeginPlay 幂等重挂 ticker(§9.19 有界驱动)。切后台场景本就由 Coordinator 前台
  `RestartAuthoritativeRecovery(Resume)` 覆盖,未改动。
- 场景矩阵:启动窗口 Kafka 未就绪 → 门禁拒 Ready(编排器重试);运行中 Kafka 宕机 → READY 滞留
  active 持续补推,恢复即达,UE watchdog 兜底前台等待;matchmaker 崩溃/换 leader → durable saga
  重放 + 补推;客户端切后台 → 前台恢复权威重查;推送链路全灭 → watchdog 周期 ResumeContext。
- 交接:UE 编译与真机验证由用户执行(Live Coding 约束);建议复验事故时序(先起 matchmaker 后起
  Kafka → CrashLoop 至就绪;双人组队 → 两端都收到 READY 并进 Battle;匹配中 kill matchmaker-pve
  Pod → 重启后队员仍能进场)。git/svn 提交由用户复核后执行。文档:go-services.md §2.8 新增
  READY at-least-once 不变量条目。
- 追加(同日,用户质询「Kafka 恢复后人已在别的副本,补推岂不是有问题」触发的审查):
  ①服务端侧确认无害——ReleaseMatch 前成员被 claim + locator BATTLE 门锁死进不了别局,
  ReleaseMatch 后 match 记录删除、补推循环即停,不存在"跨局迟到补推";②客户端侧查出真缺口:
  `UMyDsRecoveryCoordinator::TryDriveTravel` 缺 §23 要求的 Battle 幂等 no-op 守卫(Hub 有
  CanReuseCurrentHubAdmission,Battle 没有),战斗内收到重复/迟到 READY(补推使之常态化)会
  对同一 DS 重复 ClientTravel 把玩家拽出重载地图——先于本次改动即存在,补推放大。已补:
  Battle 目标且当前 live connection 端点精确一致时不再 Travel,转入与 World BeginPlay 后验
  同款的 post-travel 权威复核(bPostTravelAuthorityCheck + RetryBackoff + ScheduleAuthorityRetry),
  由 ResumeContext 按 route+match 确认 admission 收口;漂移则照常权威重查。验收补充:战斗内
  手动重发 READY 推送(或制造部分成员推送失败触发补推)→ 在局玩家无地图重载,日志出现
  "already connected to target battle endpoint"。

## 2026-07-20 ~ 07-21 实时成长入账通道(玩家经验 + 掉落即时到账,Claude)

- 需求拍板:击杀怪物/完成任务**即时**加经验(Lv15 封顶 MAX,连升多级),金品质+掉落同队广播;
  DS 崩溃**已入账部分保住**;§0.6 红线不删。设计:`docs/design/realtime-progression.md`(已拍板);
  契约修订已合入 `ds-arch.md` §0.5 ③ / §0.6;决策已登记 `pandora-arch.md` §11。
- proto(`[proto]`,已本地 buf lint + go/cpp 双生成,cpp pb 已拷入 UE 仓库):
  `battle.proto` 新增 `ReportProgress`(BattleProgressEvent oneof MonsterKill/ItemPickup,
  seq 幂等)+ `ReportResultRequest.final_progress_seq`;`player.proto` 新增 `AddExperience`
  (系统 RPC)、`PlayerPushEventType`(0=旧 MMR 事件,1=EXPERIENCE)、`PlayerExperienceEvent`、
  `PlayerProfile.exp_in_level/is_max_level`(取自 stats 预留段 12/13,reserved 收窄为 14-49)。
- player 服务:`players.exp` 列 + `exp_history`(uk player_id+idempotency_key)+
  `player_push_outbox`(与入账同事务);`AddExperience` 幂等入账 + 等级曲线结算
  (`AdvanceExperience` 纯函数:连升多级/升满清零/满级 no-op 不消费幂等键不出箱);
  推送出箱发布器 `RunPushOutboxPublisher`(SendRawWithEventType,kafka header 路由);
  `exp_curve` 配置与客户端 `j_玩家等级经验.xlsx` 同源(dev/prod 样例已填 1000..11400 占位曲线,
  空=功能关闭);MMR 消费者按 event_type header 跳过非 0 事件(防经验事件进 DLQ,有回归测试)。
- battle_result 服务:`ReportProgress`(复用 ReportResult 的 Guard+Redis active 鉴权,
  roster 越权拒)→ 校验/上限(单批 256/单场 seq 10 万/单事实 count 上限)→ 怪物经验表换算 +
  掉落白名单过滤(未知怪/非白名单跳过告警,水位照常推进,不卡流)→ `battle_progress_stream`
  水位乐观 CAS 与 `battle_progress_outbox` 同事务;出箱 worker 幂等调 player.AddExperience /
  inventory.GrantInstances(幂等键 progress:{match}:{seq}:{player}:{kind},背包满转邮件同 drop);
  SaveResult 事务内打终局标记(僵尸 DS fencing:结算后进度一律 ERR_INVALID_STATE)+
  水位>0 抑制结算路径掉落发放(单一权威路径防双发,不信 DS 声明)+ final_progress_seq 对账
  (缺口只告警,§9 尾窗残余风险);killswitch `progress_disabled`。ABANDONED 不回滚已入账。
- SQL:mysql-init 04/05 更新 + `tools/migrate` pandora_player 000002_experience、
  pandora_battle 000005_battle_progress(纯 additive,不停服)。Envoy:AddExperience 加入
  player.v1 403 精确拦截清单(与 UpdateMMR/Grant* 同双保险)。
- 验证:player/battle_result `go build + go test` 全绿(新增等级曲线 10 例、AddExperience 6 例、
  consumer 路由 2 例、progress 校验矩阵/聚合/重放/结算拒收/防双发/发布器 8 例);全 go.work
  模块构建通过(顺手修了 matchmaker 上会话遗留的 buildProgress 调用点漏传 m.MapId 编译错)。
  无新增依赖,无需 go mod tidy。
- UE 侧(Pandora-Client-SVN)由并行会话按同一设计实施中(已见:PandoraBattleProgressReporter、
  Loot 掉落模块/品质色阶(1白..5金6红)、battle/player wire+codec+ReportBattleProgress 传输层、
  GameMode 接线进行中)。**交接/待验收清单**:①怪物击杀事实(MonsterKillFact)上报接线与
  battle_result `monster_exp` 表(dev yaml 目前空表,需按怪物配置 ID 填值);②客户端经验适配
  (player.update event_type=1 → SetExperienceDisplay/PlayLevelUpPresentation、登录/重连
  GetProfile 刷新、player codec 的 GetProfile/PlayerExperienceEvent 解码)当前尚未见落地;
  ③`j_玩家等级经验.xlsx` 与怪物经验列进服务端导表管线(当前以服务 yaml 为权威配置);
  ④UE 编译/联调由用户执行;⑤验收矩阵见 realtime-progression.md §8(幂等/连升/封顶/故障/
  崩溃保住/防双发/断线补推/广播可见域/killswitch)。

## 2026-07-21:关卡内规则功能补齐(UE 侧,基于既有 Drop/实时进度系统扩展)

- 盘点结论(对照策划三图,除场景缩略图):主流程/大厅五件套/交易行/组队匹配/结算服务端均已上线;
  怪物掉落+拾取入包+实时入账(CfgDrop/AMyDropItemActor/PandoraBattleProgressReporter)已由前序
  工作树实现。本轮补齐其余关卡内规则,全部为 UE 仓库改动,后端零改动:
  ①拾取分配规则强化:AMyDropItemActor 新增专属归属 OwnerPlayerId(0=公共先到先得,>0 仅归属者
  可拾)、死亡玩家不可拾取、bPersistOnPickup(false=拾取只入战斗背包不入账)、品质查询与
  BP 外观钩子;ItemConfigId/Count 开放 EditAnywhere,关卡直摆=初始场景物品。
  ②玩家死亡掉落:APandoraBattleGameMode::SpawnPlayerDeathDrops,撤离制副本死亡时战斗背包
  (人物背包,装备可配)整包散布为 persist=false 公共掉落——原持有者拾取时已即时入账,再分配
  拾取只入战斗背包,防后端双发;PVP 保持原行为。开关在 UMyLootSetting(DefaultMyGameSetting.ini)。
  ③宝箱开锁拾取规则:AMyLootChestActor(站桩开锁 UnlockSeconds/离开重置/开锁权继承/掉落表
  独立 roll/可配开锁者专属/RespawnSeconds 重刷),状态+进度复制,蓝图挂外观。
  ④撤离点显示和撤离规则:AMyExtractionPointActor(bAlwaysRelevant 全程可见,可配延迟开放,
  进圈蓄力 ExtractSeconds、离圈/死亡重置,多人并行蓄力),蓄力满走
  APandoraBattleGameMode::HandlePlayerExtracted(可信身份+防重复+终局快照冻结前受理)。
  ⑤终局规则策略化:UPandoraBattleSettlementRule 新增 SupportsExtraction/EvaluateTerminal;
  PVP 行为不变(任一死亡立即结算);PVE 撤离制=全员死亡或撤离才结算,任一人撤离成功=通关(0),
  全灭=未通关(1);单人 PVE 语义与旧行为兼容。GameMode 维护 Dead/Extracted 终态集合。
  ⑥公屏信息广播规则(realtime-progression §7 掉落广播):拾取品质≥门槛(默认金=5,可配)时
  NotifyItemPickedUp 内经可信 ActivePlayers 解析拾取者,向同阵营在场玩家控制器逐个
  ClientReceivePublicBroadcast;客户端本地解析道具名/品质色后送 UMyMainView::EnqueuePublicBroadcast。
  ⑦副本场景道具使用规则(食物/水):FCfgItem 新增「使用回血量 UseHealHp」;
  UMyBagComponent::ServerUseBagItem 服务端权威校验(可使用+回血>0+存活)→四元组精确扣 1
  →ASC ApplyModToAttribute 回血(PreAttributeChange 钳制);抽光格子即时清空防幽灵条目。
  ⑧装备穿戴规则:FCfgItem 新增「装备部位 EquipSlot」;ServerEquipItem(背包→装备栏,同部位
  自动替换,替换后背包无位整体回滚,强制堆叠上限=1 保 guid/动态属性跟随)/ServerUnequipItem
  (背包满则失败保持穿戴)。装备栏全员复制,查看他人出装天然支持。
  ⑨入场随机选择出生点:PandoraDSGameModeBase 新增 bRandomizeSpawnPointSelection
  (Battle 构造置 true,Hub 保持 SlotIndex 确定性),同筛选条件空闲候选内随机,占用互斥不变。
- 小修:UMyBagComponent::GetMaxStackSize 在 UCfgSystem 缺失(无表测试环境)时回退
  SetDefaultMaxStackSize 的默认上限(此前该字段写而不读);具体道具配置缺失仍 fail-closed 0。
- 明确未做(有既有拍板/另行立项):PVE 匹配补人与 AI 填充(decision-dungeon-entry-modes §5
  拍板当前不做);NPC 对话客户端 UI(dialogue 服务端已上线);可破坏物=「无行为树怪物实体+
  CfgDrop 配掉落」内容配置路径,无需新代码;鉴定/背包/交易行等大厅系统服务端已上线且
  inventory/player/battle_result `go test ./...` 本轮复跑全绿。
- 交接(用户/编辑器侧):UE 编译+PIE 验证(Live Coding 约束,AI 不代跑);掉落物/宝箱/撤离点
  蓝图子类外观与关卡摆放;CfgItem 表新增「使用回血量/装备部位」两列导表;DefaultMyGameSetting.ini
  可选配置 UMyLootSetting(默认值即可跑通)。git/svn 提交由用户复核后执行。

## 2026-07-21:配置表热更流水线落地(旧项目读表移植 + §9.15 标准化,Claude)

- 背景:把 D:\luyuan\mmorpg 的 Go 读表(go/shared/generated/table 单例 TableManager 模式)移植到本仓库,
  并按 config-table-hotreload.md §0 标准流水线加固。旧实现三处不达标,移植时全部修正:
  ①每表 `m.snap = snap` 普通指针赋值(热更下数据竞争)→ 全批快照 + `atomic.Pointer` 一次切换;
  ②`LoadTables` 失败 `log.Fatalf` 杀进程 → 返回 error,失败保留旧批次;
  ③逐表独立加载(批内跨表版本可能撕裂)→ manifest 驱动整批 all-or-nothing。
- 新增(全链 `go build`/`go test` 绿):
  - `proto/pandora/config/v1`:`level.proto`(LevelRow/LevelTableData,对齐 g_关卡.xlsx)+
    `configtable.proto`(ConfigTableAdminService.ReloadConfigTable,幂等/失败保留旧表);
    errcode 增 `ERR_MATCH_INVALID_MAP=4008`(proto 与 pkg/errcode 同步)。[proto]
  - `pkg/configtable`:manifest(version+sha256+rows)校验、protojson 加载(运行时 DiscardUnknown,
    滚动窗口容忍新字段)、version 单调防回退、expect_version、未知新表跳过告警、脏文件告警、
    LevelTable 只读视图(ByID/IsBattleLevel);测试覆盖 9 类失败路径「失败保留旧表」+ 并发读切换。
  - `tools/configtable-gen`(独立 module,已入 go.work):读 Pandora-Client-SVN/Table 源表
    (中文表头版式:1 列名/2-4 注释/5+ 数据),§7 严格校验(表头精确对齐/主键唯一/枚举越界/
    布尔 0-1/路径前缀),protojson 确定性序列化(Compact→Indent 消除 protojson 随机空白),
    生成回读严格校验;version 自动单调(YYYYMMDD*1000+seq),同内容幂等不写盘。
    xlsx 解析用 stdlib 自实现最小读取器(zip+xml,fail-closed),不引第三方、无需 Python。
  - `configtable/dist`:首批产物 v20260721001(level 7 行,git 跟踪)。
  - matchmaker 接线:`config_table.dir` 开关(空=不启用,现行为不变;非空=启动强依赖 fail-closed,
    并校验兜底 `match.map_id` 必须战斗类关卡);StartMatch 增关卡表准入门——客户端 map_id
    (含 0→默认兜底后)必须存在于关卡表且 category=战斗,否则 `ERR_MATCH_INVALID_MAP`
    (此前任意 map_id 可一路透传成 DS `PANDORA_MAP_ID`);同 gRPC 端口挂 ReloadConfigTable
    (内部接口,callerID!=0 一律拒;信任模型同 ReleaseMatch)。热更后新批次对后续 StartMatch 立即生效,
    已入队票据在 ticket TTL 内自然流完不回溯。`pkg/config.Base` 增 `config_table` 段(全服务可复用)。
  - `tools/scripts/configtable_publish.ps1`:dist→staging→sha256 校验→版本单调→旧 active 归档
    history/v<ver>→改名切 active(+可选 grpcurl 触发 reload);已实测发布/no-op/升级归档/回退拒绝。
- 决策记录:「匹配列表显示」列不做服务端准入(是客户端 UI 展示位,dev/GM 直进测试关卡需放行),
  服务端只卡「存在 + category=战斗」;生成器与 pkg/configtable 不互相 import,以 §5 manifest JSON
  为共同契约。产物 JSON 口径:proto 原名 snake_case + 枚举数字 + 零值省略。
- 剩余(待排期,不阻塞现网):etcd 版本键 watch 多机统一刷新(§6 方式 1,单机 reload RPC 已够);
  其余表(道具/技能/Buff…)按「proto 容器 + tablegen 表规格 + specByName 注册 + Tables 加字段」四步扩;
  本机无 gcc,`-race` 未跑(读路径 atomic.Pointer + 不可变快照,竞态面已设计消除)。
- 交接:①`go.work` 新增 `use ./tools/configtable-gen`(如需 `go work sync`/tidy 由 Codex 收尾;
  该 module 仅依赖 proto+protobuf,本机 build/test 已绿);②proto 有改动已重生 pb,UE 侧无需同步
  (纯服务端协议);③dev 启用方式见 matchmaker-dev.yaml `config_table` 注释段。

## 2026-07-21(续):Go 表访问代码改为生成(移植旧项目表代码模板,Claude)

- 补齐读表移植缺口:旧项目 mmorpg 的「每表一份生成 Go 代码」能力(go_config.go.j2 /
  go_all_table.go.j2)移入 `tools/configtable-gen/internal/gogen`(text/template + go/format)。
  旧生成物 48 个 `*_table.go` 不直接拷(表集与 pb 包都是旧项目的),生成能力对 Pandora 表规格重放。
- 结构:`pkg/configtable/<name>_table.gen.go`(视图 + All/ByID/Exists/Count/ByIDs/RandOne/Where/First,
  即旧 TableManager API 去单例化)+ `tables.gen.go`(Tables 快照结构 + specByName 注册,替代旧
  all_table.go 的 LoadTables);表私有校验/域方法留手写伴生文件(level.go:validateLevelRow +
  IsBattleLevel)。生成/手写边界:gen 文件头 DO NOT EDIT,钩子由生成代码显式调用。
- 表规格单一事实源收拢到 `internal/tablegen/registry.go` `Sources()`(数据产物与代码生成共用);
  main 增 `-go-out`(默认 pkg/configtable,空=跳过),内容不变不写盘。
- 守护:`gogen.TestGeneratedFilesUpToDate`(规格改了不重跑生成器 / 手改 gen 文件 → 测试红)、
  确定性测试、生成 API 与钩子接线测试。pkg/configtable 既有全部测试 + matchmaker 全量回归绿
  (API 兼容,消费方零改动)。
- 未移植并记录原因:comp(ECS 组件结构,Go 侧无消费方)、fk(外键 helper,现有表无外键列)、
  multi-key 复合主键(无此类表);出现真实需求再加(§15.3)。加新表五步见 hotreload doc §10。
