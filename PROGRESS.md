# Pandora 进度记录

> 本文档**只追加,永不删旧条目**。AI 新会话第一件事就是读这里。

## W1 (2026-06-03 起)

### 立项决策(Round 0)

| 项 | 决策 |
|---|---|
| 项目名 | **Pandora**(项目)/ pandora(资源命名空间) |
| 后端仓库 | https://github.com/luyuancpp/Pandora.git(public) |
| UE 仓库 | 待定(暂用 Pandora-Client 占位) |
| UE 版本 | 5.7(Iris + GAS,默认 Iris,退路 Replication Graph) |
| 类型 | MOBA + 持续在线大厅 |
| 大厅 | 500 人/实例,单城镇约 1km²,**全图自由 PvP** |
| 战斗 | 5v5,~25 分钟 |
| DS 编排 | Agones on k8s(本地先 minikube,生产待定阿里云 ACK / 自建) |
| 协议 | gRPC + Kafka |
| 基础设施 | MySQL 8 / Redis 7 / Kafka 3 / etcd 3(全新搭一套,端口跟 mmorpg 错开) |
| License | MIT |
| Go 版本 | 1.23 |
| 中文回复 | 是(继承 mmorpg) |
| mmorpg 项目状态 | 封存,允许 D2 一次性拷代码作起点,之后两边独立 |
| **D2.1 框架选型** | **继续用 `go-zero`**(2026-06-03 决策)— 复用 mmorpg 90% 公共代码,D2 工作量 4~5 天 |

### 端口规划(避免与 mmorpg 冲突)

| 基础设施 | mmorpg | Pandora |
|---|---|---|
| MySQL | 3306 | **3307** |
| Redis | 6379 | **6380** |
| Kafka | 9092 | **9093** |
| etcd client | 2379 | **2380** |
| Prometheus | 9090 | **9091** |
| Grafana | 3000 | **3001** |

详见 `docs/design/infra.md` §6。

### W1 任务进度

#### 文档草稿(已落盘)
- [x] `CLAUDE.md`(项目宪法)
- [x] `PROGRESS.md`(本文)
- [x] `AGENTS.md`(AI 协作守则)
- [x] `docs/design/pandora-arch.md`(总架构)
- [x] `docs/design/proto-design.md`(协议设计)
- [x] `docs/design/pkg-copy-from-mmorpg.md`(公共框架来源)
- [x] `docs/design/infra.md`(基础设施规范)
- [x] `docs/design/go-services.md`(13 个 go 服务清单)
- [x] `docs/design/stress-discipline.md`(压测纪律)
- [x] `docs/design/ds-arch.md`(UE DS 设计)
- [x] `docs/design/pvp-rules.md`(PvP 规则待定项)

#### W1 计划

| 阶段 | 内容 | 状态 |
|---|---|---|
| **D1** | 仓库骨架 + 11 份文档落盘 | ✅ 完成(2026-06-03,commit b4f6351) |
| **D2** | 拷 mmorpg pkg + docker-compose + dev_up.ps1 | ✅ 完成(2026-06-03,commit 94045f0) |
| **D3** | 写 .proto + buf 工具链 | 🟢 进行中(2026-06-03) |
| D4 | UE 仓库初始化(用户主导) | ⏸️ |
| D5-D6 | UE DS 骨架代码(HubGameMode / BattleGameMode + Agones SDK) | ⏸️ |
| D7 | k8s + Agones + 端到端 hello world | ⏸️ |

### D3 完成清单(2026-06-03)

#### buf 工具链(3 个文件)
- [x] `proto/buf.yaml` v2 module + STANDARD lint(豁免 ENUM_VALUE_PREFIX / ENUM_ZERO_VALUE_SUFFIX)
- [x] `proto/buf.gen.go.yaml` 用 buf.build 远程插件(`protobuf/go` + `grpc/go`) + 本地 `protoc-gen-go-http`,managed.go_package_prefix 全局统一
- [x] `proto/buf.gen.cpp.yaml` 占位(D4+ UE 仓库使用)

#### proto 源(19 个文件,1373 行)

**common/v1/(4 个)**:
- [x] `errcode.proto` 全段位错误码 enum(64 个常量,跟 pkg/errcode 1:1 同步)
- [x] `timestamp.proto` TimestampMs 包装
- [x] `pagination.proto` PageRequest + PageMeta
- [x] `kafka_envelope.proto` 统一信封(topic / key / payload / trace_id / ts_ms)

**13 个业务服务(每个一个 .proto)**:
- [x] `login/v1/login.proto` LoginService(Login / Logout / IssueDSTicket / VerifyDSTicket)+ DSTicket
- [x] `player/v1/player.proto` PlayerService(GetProfile / UpdateNickname / ListHeroes / UnlockHero / GetMMR / UpdateMMR 幂等)
- [x] `data_service/v1/data_service.proto` ReadPlayer / WritePlayer / InvalidateCache + 乐观锁 version
- [x] `friend/v1/friend.proto` AddFriend / Accept / List / Block + FriendRequestStatus enum
- [x] `chat/v1/chat.proto` SendMessage / StreamMessages + ChatChannel enum
- [x] `locator/v1/locator.proto` PlayerLocatorService + Location + LocationState enum
- [x] `team/v1/team.proto` 7 个 RPC + StreamTeamUpdates 推送 + TeamState 5 状态
- [x] `match/v1/match.proto` 4 个 RPC + StreamMatchProgress + MatchStage 6 状态
- [x] `trade/v1/trade.proto` 4 个 RPC + Order + OrderState 7 状态
- [x] `dialogue/v1/dialogue.proto` Start / Choose / End + DialogueState
- [x] `ds/v1/allocator.proto` AllocateBattle / Release / Heartbeat 双向流 / ListBattles
- [x] `hub/v1/allocator.proto` Assign / Release / Transfer / List / Heartbeat
- [x] `battle/v1/battle.proto` ReportResult 幂等 / GetMatchResult / ListPlayerHistory + BattleResult + PlayerStats

**ds_runtime(2 个,UE Client ↔ UE DS)**:
- [x] `ds_runtime/v1/hub.proto` HubRuntime(TriggerNPC / OpenShop / TransferToHub / EnterBattle)+ Vector3
- [x] `ds_runtime/v1/battle.proto` BattleRuntime(ChooseHero / PurchaseItem / UpgradeAbility / VoteSurrender / Disconnect)

#### 工具脚本
- [x] `tools/scripts/proto_gen.ps1`(buf 检查 + lint + breaking + go gen + cpp gen,支持 -Lint / -Cpp / -Breaking)

#### 手工 lint(buf 未装,代替自动 lint)
- [x] 19 个文件 syntax = "proto3" 全部第一个非注释行 ✅
- [x] 15 个 package 名全部 `pandora.<domain>.v1` 规范 ✅
- [x] 15 个文件各 1 个 service 块(common 4 个无 service)✅
- [x] 字段编号无重复(awk 脚本误报已手工核对)✅
- [x] 所有时间戳字段命名 `_at_ms` / `_time_ms` / `ts_ms`(统一 int64 毫秒)✅
- [x] proto ErrCode(64) ↔ pkg/errcode(64) 数量一致 ✅

#### 待用户安装 buf 后才能验的
- [ ] `buf lint` 全绿(预期通过,但需 buf 实跑)
- [ ] `buf generate` 产 .pb.go(W2 写第一个服务前必跑)
- [ ] 生成的 pb 能 `go build`(W2 引用时验证)

#### 中途修正:ds_runtime/battle.proto 协议边界

⚠️ **用户在 D3 末尾发现错误**:"战斗用 UE GAS 啊,不用 battle proto 场景同步的 proto 吧"

修正:
- 删除 `BattleRuntimeService.PurchaseItem`(出装走 GAS Ability,不走 gRPC)
- 删除 `BattleRuntimeService.UpgradeAbility`(升级技能走 GAS Ability)
- 保留 `ChooseHero / VoteSurrender / Disconnect`(战斗外 UI 业务,非 tick 同步)

在 `docs/design/ds-arch.md §0` 加了"协议边界:GAS / Replication vs gRPC"必读章节:
- 走 GAS / Replication:移动 / 技能 / HP / buff / 命中 / 伤害 / 出装 / 升技能 / 表现层
- 走 gRPC:跨进程 / 战斗外 UI / 后端服务联动
- 反模式禁令(为下一会话 AI 立法)

**纪律**:proto 不写战斗 tick 字段;UE Replication 字段不写 proto。

#### 后续提醒(写入新一会话 AI 必读)

⚠️ **buf 未安装,proto/gen/ 暂时空目录**。用户装完 buf 后第一次跑:
```powershell
winget install bufbuild.buf
pwsh tools/scripts/proto_gen.ps1
```
跑完后 `proto/gen/go/` 会产出 .pb.go,W2 写代码时直接 `import "github.com/luyuancpp/pandora/proto/gen/go/<domain>/v1"`。

⚠️ **errcode 双向同步纪律**:proto/pandora/common/v1/errcode.proto 和 pkg/errcode/errcode.go 数值必须一致,**改任一边必须同步改另一边**。W2+ 考虑加 pre-commit hook 自动校验。

⚠️ **协议边界(GAS vs gRPC)是最重要的不变量**(ds-arch.md §0),新会话 AI 动 proto 前必读。




### D2 完成清单(2026-06-03)

#### pkg/ 公共框架(12 个模块,~1900 行)
- [x] `pkg/snowflake/` 直接拷自 mmorpg(82 行 + 109 行 test,7 个 test case 全绿)
- [x] `pkg/cache/` 直接拷自 mmorpg(89 行,泛型 cache-aside + singleflight)
- [x] `pkg/redislock/` 直接拷自 mmorpg(131 行,prefix 改 `pandora:lock:`)
- [x] `pkg/grpcstats/` 直接拷自 mmorpg(347 行,gRPC 流量采集 + topN 报告)
- [x] `pkg/log/` 新写,薄包装 logx + ctx trace_id 透传
- [x] `pkg/errcode/` 新写,错误码全段位定义(0-10999)+ Code/Error/IsRetryable
- [x] `pkg/metrics/` 新写,prometheus Register 包装 + StandardBuckets
- [x] `pkg/config/` 改写,以 mmorpg login config 为基础,剥业务字段;BuildTopic / BuildDLQTopic
- [x] `pkg/grpcserver/` 新写,zrpc 包装 + 4 个默认拦截器(recover/trace/metrics/grpcstats)
- [x] `pkg/grpcclient/` 新写,trace_id 出站透传 + 客户端 metrics
- [x] `pkg/kafkax/consistent.go` 直接拷自 mmorpg consistent 包(117 行,FNV-1a + 虚拟节点)
- [x] `pkg/kafkax/producer.go` 改写,SyncProducer + key-ordered + 幂等
- [x] `pkg/kafkax/consumer.go` 改写,sarama ConsumerGroup,Handler 接口给业务实现
- [x] `pkg/svc/base.go` 新写,BaseContext 模板(Redis/Snowflake/Locker)

#### 验证
- [x] `go build ./pkg/...` 全绿(无输出 = 成功)
- [x] `go vet ./pkg/...` 无警告
- [x] `go test ./pkg/snowflake/...` 7 个 case 通过(0.793s)
- [x] `pkg/go.mod` 由 tidy 自动调整到 `go 1.24.0` + toolchain 1.24.5(依赖 go-zero 1.9.x 要求)
- [x] `go.work` 同步到 `go 1.24.0`

#### 基础设施(deploy/)
- [x] `deploy/docker-compose.dev.yml` 7 服务(MySQL 3307 / Redis 6380 / Zookeeper 2182 / Kafka 9093 / etcd 2380 / Prom 9091 / Grafana 3001)
- [x] `deploy/env/dev.env` 开发期凭证(MYSQL_USER=pandora / GRAFANA_USER=admin)
- [x] `deploy/mysql-init/01-create-databases.sql` 创建 6 个数据库(pandora_account / player / social / battle / trade / ops)
- [x] `deploy/prometheus/prometheus.yml` 抓 13 个 go 服务的 51001~51022 metrics 端口
- [x] `docker compose config --quiet` 验证通过

#### 工具脚本(tools/scripts/)
- [x] `dev_up.ps1`(含 -Pull 选项 + healthy 等待 + 连接信息打印)
- [x] `dev_down.ps1`(含 -Volumes 危险选项,需 yes 确认)
- [x] `dev_status.ps1`(docker compose ps + 端口监听检测)

#### 后续提醒
⚠️ Go 版本最终落到 **1.24**(原计划 1.23)— go-zero 1.9.x 等依赖要求 1.24+,被自动升级。1.24 兼容 1.23 代码,不影响计划。CLAUDE.md / docs/ 中的 "Go 1.23" 字样保留(标记历史立项决策),实际编译用 1.24。

⚠️ kafkax 是 **W1-D2 简化版**:无 retry queue / 无 DLQ / 无 plainProducer。W2 写 battle_result 时再补全。

⚠️ Phase 2 docker compose 没有 `up -d` 实跑(留给用户;镜像 pull 需要他网络)。`compose config --quiet` ��验证 yaml 语法 + 端口绑定正确。

### go.work 构建口径修正(2026-06-05)

**问题**:CLAUDE.md §4.1 原写 `go build ./...`,但仓库根没有 go.mod,go.work 多 module 模式下此命令报错:
```
pattern ./...: directory prefix . does not contain modules listed in go.work or their selected dependencies
```

**决策**:
- 确认继续 go.work 多 module 模式(pkg 一个 module、每个服务一个 go.mod)
- 根目录不加 go.mod(不回退到单根模式)
- CLAUDE.md §4.1 验证命令改为按 go.work use 列表逐 module 构建
- 当前阶段验证命令:`go build ./pkg/...`
- W2+ 新增服务 module 时同步追加到验证命令

**修改文件**:
- `CLAUDE.md` §4.1 — 改验证命令为 workspace 级
- `go.work` — 追加构建注意事项注释

### 待用户决策

#### 阻塞 D2 的(必须定)
- [x] **D2.1 框架选型**:**继续用 go-zero**(2026-06-03 决策)
- [ ] **UE 仓库名**(暂用 Pandora-Client 占位,D4 阻塞)
- [ ] **k8s 选型**:阿里云 ACK / 自建 / 先 minikube(D7 阻塞)

#### 非阻塞但要尽快定
- [ ] PvP 死亡惩罚级别(A 轻 / B 掉金币 / C 掉装备 / D 混合)
- [ ] PvP 新人保护方案
- [ ] 击杀奖励公式
- [ ] 大厅安全区方案
- [ ] MOBA 段位划分 / 赛季机制
- [ ] MMR 算法(默认 Glicko-2)
- [ ] AFK 阈值(默认 3 分钟)
- [ ] Ban / Pick 阶段

详见 `docs/design/pvp-rules.md`。建议按 §6 默认值先实现,后期策划再调。

### 后续提醒

⚠️ **W2 写代码时**:13 个 go 服务目录下的 `.gitkeep` 在 `cmd/main.go` 出现后**手动删除**(否则空目录占位污染)。

⚠️ **W2 D2.1 决策**:框架选型一旦定下来,所有 13 个服务的 `internal/svc/servicecontext.go` 模板就锁死,后期换框架成本极高。**慎重**。

### 下一会话 AI 必读清单

1. 本文(掌握当前进度)
2. `CLAUDE.md`(项目规范)
3. `docs/design/pandora-arch.md`(架构总图)
4. **`docs/design/architecture-rejected-strict-ds-only.md`**(2026-06-03 否决方案,反面教材)
5. `docs/design/gateway-decision.md`(待写,业务通道选型)
6. `git log -20 --oneline`(最近改动)
7. 当前打开的 PR(如果有)

---

## D3 阶段 — 架构推翻记录(2026-06-03)

### 背景

D3 写完 19 个 .proto + buf 工具链后,用户在收尾阶段连续提出 6 个深刻问题,导致架构反复调整:

1. "战斗用 UE GAS,不用 battle proto 场景同步" → 调整 ds_runtime/battle.proto,加 ds-arch.md §0 协议边界
2. "UE 有的功能 proto 里面不应该有" → 删除整个 `proto/ds_runtime/` 目录
3. "go-zero 不支持 gRPC 推送,我服务怎么推送" → 触发推送架构重新设计
4. "客户端只连 DS,中间不能任何跳转" → 走错"严格 A"路线
5. "大厂做法是什么" → 对齐发现严格 A 无大厂先例
6. "DS 崩了不该影响业务功能" → 否决严格 A,改走业务独立通道(类大厦方案)
7. "为什么 Hub DS 每 5s 主动请求" → 澄清心跳方向 + 频率原理
8. "Client 不走 gRPC,走 WebSocket;后台才走 gRPC" → 修正协议矩阵

### 最终决策(写入 pandora-arch.md §11 + gateway-decision.md)

#### 三连接架构(对齐大厦,故障域隔离)

| 通道 | 协议 | 用途 | 谁实现 |
|---|---|---|---|
| ① Client → DS | UE NetDriver(UDP-like)| 仅游戏内同步 / GAS / Replication | UE 引擎自带 |
| ② Client → gateway | **HTTP/JSON** | 所有业务请求(登录/组队/匹配/商店/...) | UE HttpModule(引擎自带)|
| ③ Client → push | **WebSocket** | 推送接收(组队邀请/匹配进度/聊天) | UE WebSocketsModule(引擎自带)|

⭐ **Client 不走 gRPC,gRPC 仅存在于后台服务之间**(gateway → 业务服 / DS → allocator / 服务互调)。

#### 新增 2 个 go 服务(13 → 15)

| 服务 | 实现 | 端口 |
|---|---|---|
| **gateway** | go-zero gateway 官方组件 + yaml 配置 | 8080(HTTP)/ 51014(metrics) |
| **push** | gorilla/websocket + sarama + redis | 8081(WebSocket)/ 51015(metrics) |

#### Heartbeat 改造

DS Heartbeat 从 gRPC 双向流 → **unary 每 5s 主动调**:
- DS 是 client,allocator 是 server(go-zero 友好)
- DS 上报状态 + allocator 通过 response 下发指令(stop/drain/reload)
- 5s 是 k8s / Agones 标准心跳频率,故障检测延迟 = 3 个周期(15s)

#### 被否决方案(反面教材)

- ❌ **严格 A:客户端只连 DS** — 详见 `docs/design/architecture-rejected-strict-ds-only.md`,6 个不可接受后果(Hub DS 兼网关、500 人 PvP 预算崩、UE 代码量翻 2~3 倍、单点故障、登录死锁、大厦无先例)

### D3 最终产出清单

#### proto 源(17 个文件,~1300 行)

**common/v1/**(4 个,不动):errcode / timestamp / pagination / kafka_envelope

**13 个业务服务**(原本不动,仅 5 个改造):
- ~~team.StreamTeamUpdates~~ → 删除 stream RPC,加 `GetTeam`(unary)+ 保留 `TeamUpdateEvent`(给 push 消费)
- ~~match.StreamMatchProgress~~ → 同上,`GetMatchProgress` + `MatchProgressEvent`
- ~~chat.StreamMessages~~ → 同上,`PullHistory` + `ChatPushEvent`
- ~~ds.allocator.Heartbeat 双向流~~ → 改 unary
- ~~hub.allocator.Heartbeat 双向流~~ → 改 unary

**ds_runtime/**:整个目录已删除(UE 有的功能 proto 不写)

#### 文档(新增 / 大改 4 份)

- ✅ `docs/design/gateway-decision.md`(**新建**,~470 行)三连接架构 + go-zero gateway + push 服务详细设计 + 端到端时序 + 故障域分析
- ✅ `docs/design/architecture-rejected-strict-ds-only.md`(**新建**,~120 行)反面教材,严格 A 6 个后果
- ✅ `docs/design/ds-arch.md` §0(**重写**)从"GAS vs gRPC 协议边界" → "客户端三连接 + 后端 gRPC"协议矩阵 + 反模式禁令补强
- ✅ `docs/design/pandora-arch.md` §3 / §6 / §11 服务清单 13→15 + 协议矩阵重写 + 决策行 +3
- ✅ `docs/design/go-services.md` §1 / §4 / §5 加 gateway / push 详细契约
- ✅ `docs/design/infra.md` §4.2 / §6.2 加推送 topics + gateway/push 端口

#### 服务骨架

- ✅ 新增 `gateway/.gitkeep` 和 `push/.gitkeep` 占位目录
- ✅ `go.work` use 列表加 gateway / push 注释行(W2+ 启用)

#### proto.gen 工具链(D3 原产出,保持)

- ✅ `proto/buf.yaml` / `buf.gen.go.yaml` / `buf.gen.cpp.yaml`
- ✅ `tools/scripts/proto_gen.ps1`
- ⚠️ buf 未安装,proto/gen/ 暂时空目录(用户装完 buf 跑一次)

### W2 路线图(收尾时定)

W2 写代码顺序(`go-services.md §4` 已更新):
**login → gateway(配 yaml)→ push(WebSocket 服骨架)→ player + data_service → team → matchmaker → ds_allocator + hub_allocator → battle_result → 其它**

### 下一会话 AI 必读补充

⚠️ **三连接架构是根基,不能再推翻** — 这次推翻成本已经很高(D3 改了一整天)。任何 AI 想"简化成两条连接"或"DS 兼任网关"之前,**必须读完 `architecture-rejected-strict-ds-only.md`**。

⚠️ **"Client 不走 gRPC"是硬规则**:proto 里不允许出现 `Client → XxxService` 这种直接面向客户端的 service。所有客户端业务通过 gateway 转发 → 业务服。

⚠️ **gateway 完全不需要"为每个业务写代码"**:配 yaml 把 HTTP path 映射到 gRPC method 即可。push 也几乎不需要随业务改动(只是消费 kafka + 转 WebSocket)。

⚠️ **错误码三向同步**:proto/pandora/common/v1/errcode.proto ↔ pkg/errcode/errcode.go(W2 时也要考虑给 UE 客户端生成一份 ErrCode 枚举)

⚠️ **协议顺序规则**(2026-06-03 末追加,见 `docs/design/protocol-ordering-rules.md`):

RPC response 与 kafka push 是两条独立异步通道,无法保证顺序。**乱序问题靠协议设计层面解决,不靠架构**:

1. **原则 1**:立即完成型 RPC 的 response 必须返完整业务数据(客户端不需要等 push)
2. **原则 2**:kafka push 不发给请求发起方 — 强制使用 `PushToPlayers` helper(W2 实现)
3. **原则 3**:已受理型 RPC(如 StartMatch / ConfirmMatch)显式标注,客户端 UI 状态机由 push 驱动
4. **原则 4**:每个 RPC 在 proto 注释里标注"立即完成"或"已受理"语义

⚠️ **下次会话 AI 写 RPC 前必须**:确定语义 → 写注释 → check response 完整性 → 决定 push 收件人(排除 caller)→ 调对应 helper 函数。



---

## D3 阶段 — 真正收尾(2026-06-04)

### 背景

D3 经过 10+ 轮架构反复,2026-06-03 最终选了 "三连接 + go-zero + 自研 ws gateway + push" 方案。但 2026-06-04 用户提出多个深层问题,**再次推翻**最终方案:

1. "为什么 Hub DS 每 5s 主动请求" → 心跳方向 + 频率原理(已答,不改)
2. "Client 只连一条业务,业务 HTTP 比 gRPC 包大效率低,推送有顺序问题吗" → 触发 B0/B1/Envoy 三方案对比
3. "Kratos 支持 stream gRPC,他和 go-zero 对比?" → 框架选型大讨论
4. "我们要大厂 + 最标准方案,工作量不是决策依据" → 协议铁律,优先标准化
5. "Envoy 是个网关吗 vs go-kratos/gateway" → 评估后**用户拍板 Envoy**

### 最终决策(2026-06-04 终版)

#### 架构(两连接 + Kratos + Envoy + gRPC-Web)

```
Client(UE 5.7)
  ├── ① UE NetDriver → Hub/Battle DS         仅游戏内同步
  └── ② FHttpModule → Envoy(8443 HTTPS)     gRPC-Web over HTTP/2 TLS
                                              (业务请求 unary + 推送 server stream)
                       ↓
                       Envoy gRPC-Web ↔ gRPC 转换
                       ↓
                14 个 Kratos 业务服(13 业务 + 1 push)
```

#### 关键决策(写入 pandora-arch.md §11)

| 决策 | 详情 |
|---|---|
| 切换框架 | **go-zero → Kratos**(2026-06-03 D2.1 决策被推翻) |
| Edge Gateway | **Envoy**(替代之前规划的 pandora-gateway 自研) |
| 客户端协议 | **gRPC-Web over HTTP/2 TLS**(UE 5.7 FHttpModule + 自研 grpc-web 解析) |
| 推送架构 | **集中 push 服务 + gRPC server stream**(替代之前规划的自研 WebSocket) |
| 客户端实现 | **自研 grpc-web 客户端基于 FHttpModule**(零额外依赖,不引入 grpc-cpp) |
| 服务清单 | 14 个 go 服务(13 业务 + push;Envoy 是基础设施不计) |
| 客户端连接 | **2 条**(NetDriver + FHttpModule)|

### UE 5.7 FHttpModule HTTP/2 验证(铁证)

用户直接挖 UE 5.7 源码 `Engine/Source/Runtime/Online/HTTP/`,确认:

```cpp
// HttpConstants.h
static UE_API const TCHAR* const VERSION_2TLS;

// IHttpRequest.h
Request->SetOption(HttpRequestOptions::HttpVersion, FHttpConstants::VERSION_2TLS);

// 关键:server stream 接收 API
HTTP_API bool SetResponseBodyReceiveStreamDelegateV2(FHttpRequestStreamDelegateV2 StreamDelegate);
using FHttpRequestStreamDelegateV2 = TTSDelegate<void(void*/*Ptr*/, int64&/*InOutLength*/)>;

// CurlHttp.cpp
curl_easy_setopt(EasyHandle, CURLOPT_HTTP_VERSION, CURL_HTTP_VERSION_2TLS);
```

**结论**:UE 5.7 完整支持 HTTP/2 over TLS + 流式接收,gRPC-Web 客户端**完全可自研基于 FHttpModule,零额外依赖**。

### D3 真正完成清单(2026-06-04)

#### 文档落地(13 个动作,plan 块 1)

- [x] `protocol-ordering-rules.md` 加 §3.1 设计 smell 详解(CreateTeam 案例 + 5 条 smell 表现)
- [x] `gateway-decision.md` 大改:三连接 → 两连接 + Kratos + Envoy + 自研 grpc-web(W2 实现指南)
- [x] `gateway-decision.md` §11 加 UE gRPC 插件评估(为什么不用第三方插件 + 5 个共性坑 + 大厂客户端协议事实)
- [x] `architecture-rejected-strict-ds-only.md`(2026-06-03 已存在,保留作反面教材)
- [x] `pandora-arch.md` §3 服务清单 15 → 14(删 gateway,push 改 gRPC server stream)
- [x] `pandora-arch.md` §6 协议矩阵全面重写(gRPC-Web over HTTP/2 TLS + Envoy 路由)
- [x] `pandora-arch.md` §11 决策行追加 7 条(2026-06-04)
- [x] `ds-arch.md` §0.2 协议矩阵��两连接 + 强调 Client 不走 gRPC + 后台走 gRPC
- [x] `ds-arch.md` §0.3 反模式禁令加 2 条(不拉 grpc-cpp / 不装第三方 UE gRPC 插件)
- [x] `ds-arch.md` §0.3 "为什么这样设计" 精确化:gRPC 不适合 tick 同步 ≠ 不适合业务请求;两条连接物理独立故障域隔离;Battle DS 内部 gRPC 不阻塞 tick
- [x] `go-services.md` §1 总览 13 → 14(去 gateway,push 改 gRPC server stream + 50014)
- [x] `go-services.md` §4 W2 路线图 + §5 push 服务 Kratos 风格契约
- [x] `infra.md` §6.2 端口表去 gateway 8080 / push 8081 → 加 push 50014;§6.3 加 Envoy 8443
- [x] `pkg-copy-from-mmorpg.md` §5 大改决策:标 go-zero 推翻 + 加 Kratos 决策 + W2 重写清单(~4.5 天)
- [x] `PROGRESS.md` 加本节(D3 真正收尾)

#### Proto 调整(plan 块 2)

- [ ] 新增 `proto/pandora/push/v1/push.proto`(PushService.Subscribe server stream)
- [ ] 13 个业务 .proto 加 `google.api.http` 注解(W2 时一起加,本期不强制)
- [ ] `buf.yaml` 加 `buf.build/googleapis/googleapis` deps(同上)

⚠️ Proto 调整本期**不全做**:加 google.api.http 注解涉及全部业务 proto,且需要 buf 实际跑通验证,留到 W2 装 buf 后一起做。本期只**新增 push.proto**(下面一步)。

#### Pkg 重写(plan 块 3,留 W2 做)

- [ ] W2 第一周专注做 pkg 重写,详见 `pkg-copy-from-mmorpg.md` §5.3 表格

### 下次会话 AI 必读(2026-06-04 终版补充)

⚠️ **不能再推翻架构**:Kratos + Envoy + gRPC-Web + 集中 push + UE FHttpModule 自研 grpc-web,这套**已锁死**。任何 AI 想再改之前**必须**读:
1. `architecture-rejected-strict-ds-only.md`(严格 A 反面教材)
2. `gateway-decision.md`(最终架构 + UE gRPC 插件评估)
3. `protocol-ordering-rules.md`(乱序原则)
4. 本 PROGRESS.md(决策演化)

⚠️ **D2 已写的 pkg/ go-zero 代码要在 W2 重写**(~4.5 天,见 `pkg-copy-from-mmorpg.md` §5.3)。

⚠️ **客户端协议**:gRPC-Web over HTTP/2 TLS,用 UE FHttpModule + 自研 grpc-web 解析(~3-5 天)。**绝对不拉 grpc-cpp 大依赖**,**绝对不装第三方 UE gRPC 插件**(5 个共性坑详见 `gateway-decision.md` §11)。

⚠️ **Battle DS 内部 gRPC 调用必须在独立 goroutine + 5s 超时**,不阻塞 UE 主 tick 线程(W5-W6 实现约束)。


---

## W2 ⓪ — 后端目录按业务域重构(2026-06-05)

### 背景

W2 任务 ① 写 pkg 时发现:**14 个业务服在仓库根目录平铺**,加上未来扩展(guild / mail / payment 等可能 30+),根目录会非常乱。用户提出担忧,授权我用"最好最标准"方案。

### 决策(2026-06-05)

按**业务域分组**到 `services/` 下,对齐:
1. 大厂业务级项目惯例(米哈游 / 字节 / 腾讯内部)
2. Kafka topic 域(`pandora.<domain>.<event>`)
3. DDD 风格的微服务架构

### 新目录结构

```
F:/work/Pandora/
├── services/
│   ├── account/         (login, player)              ← 账号身份
│   ├── social/          (friend, chat, dialogue)     ← 社交
│   ├── matchmaking/     (team, matchmaker)           ← 匹配组队
│   ├── battle/          (ds_allocator,               ← 战斗调度
│   │                     hub_allocator,
│   │                     battle_result)
│   ├── economy/         (trade)                       ← 经济(后期 +shop/payment)
│   ├── data/            (data_service)                ← 数据层
│   └── runtime/         (player_locator, push)        ← 运行时基础设施
├── pkg/                                               ← 公共框架(不变)
├── proto/                                             ← 协议(不变)
├── deploy/                                            ← 部署(不变)
├── tools/                                             ← 工具脚本(不变)
├── docs/                                              ← 文档(不变)
└── robot/                                             ← 压测(不变)
```

### Module 路径

| 旧 | 新 |
|---|---|
| `github.com/luyuancpp/pandora/login` | `github.com/luyuancpp/pandora/services/account/login` |
| `github.com/luyuancpp/pandora/team` | `github.com/luyuancpp/pandora/services/matchmaking/team` |
| ... | ... |

### 删除

- ❌ `gateway/`(D3 推翻,Envoy 替代,目录无意义)

### 已完成的动作

- [x] 创建 7 个业务域目录
- [x] `git mv` 13 个空业务服到对应域(.gitkeep 保留)
- [x] `git rm -r gateway/`
- [x] 改 `go.work` 注释里 13 个 use 行的路径
- [x] 批量替换 docs / PROGRESS / CLAUDE / AGENTS / README 中 14 个旧服务路径(`F:/work/Pandora/<svc>/` → `F:/work/Pandora/services/<group>/<svc>/`)
- [x] PROGRESS.md 加本节

### 为什么按域分组(决策理由)

- **未来 30+ 服务也清晰**:每域 3-5 个服务,扫一眼 10 个域 vs 平铺 30 行
- **新人定位快**:"商店在哪?" → 直接进 `services/economy/`
- **协议-代码对齐**:`pandora.team.update` topic ↔ `services/matchmaking/team/` 目录
- **业内惯例**:大厂业务级项目都按域分,平铺是 demo 风格
- **改动一次永久受益**:module 路径锁死,后期不返工

### 路径变更的连锁影响(W2 写代码时按新路径)

1. ⚠️ 服务 module 路径变长(`pandora/services/account/login`),import 时 IDE 自动补全无差别
2. ⚠️ 配置文件路径(`etc/`)跟服务在同目录,无影响
3. ⚠️ Envoy cluster 名仍按服务名不带域(`login` / `team` / `push`),路由 prefix 仍按 proto package(`/pandora.login.v1.LoginService/`)— 不受目录影响
4. ⚠️ docker image 名仍按服务名(`pandora-login:latest`)— 不受目录影响

### 下次会话 AI 必读

⚠️ **2026-06-05 起服务在 `services/<域>/<服务>/`**,任何 AI 看到"login/" 根目录平铺的内容时,要意识到那是历史路径,实际位置在 `services/account/login/`。

