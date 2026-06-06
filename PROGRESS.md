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
| 基础设施 | MySQL 8 / Redis 7 / Kafka 3 / etcd 3(全新搭一套) |
| License | MIT |
| Go 版本 | 1.23 |
| 中文回复 | 是 |
| **D2.1 框架选型** | **继续用 `go-zero`**(2026-06-03 历史决策,后续已切换 Kratos) |

### 端口规划

| 基础设施 | Pandora |
|---|---|
| MySQL | **3307** |
| Redis | **6380** |
| Kafka | **9093** |
| etcd client | **2380** |
| Prometheus | **9091** |
| Grafana | **3001** |

详见 `docs/design/infra.md` §6。

### W1 任务进度

#### 文档草稿(已落盘)
- [x] `CLAUDE.md`(项目宪法)
- [x] `PROGRESS.md`(本文)
- [x] `AGENTS.md`(AI 协作守则)
- [x] `docs/design/pandora-arch.md`(总架构)
- [x] `docs/design/proto-design.md`(协议设计)
- [x] `docs/design/infra.md`(基础设施规范)
- [x] `docs/design/go-services.md`(13 个 go 服务清单)
- [x] `docs/design/stress-discipline.md`(压测纪律)
- [x] `docs/design/ds-arch.md`(UE DS 设计)
- [x] `docs/design/pvp-rules.md`(PvP 规则待定项)

#### W1 计划

| 阶段 | 内容 | 状态 |
|---|---|---|
| **D1** | 仓库骨架 + 11 份文档落盘 | ✅ 完成(2026-06-03,commit b4f6351) |
| **D2** | 公共框架 pkg + docker-compose + dev_up.ps1 | ✅ 完成(2026-06-03,commit 94045f0) |
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
- [x] `pkg/snowflake/` ID 生成(82 行 + 109 行 test,7 个 test case 全绿)
- [x] `pkg/cache/` 泛型 cache-aside + singleflight(89 行)
- [x] `pkg/redislock/` Redis 分布式锁(131 行,prefix `pandora:lock:`)
- [x] `pkg/grpcstats/` gRPC 流量采集 + topN 报告(347 行)
- [x] `pkg/log/` 新写,薄包装 logx + ctx trace_id 透传
- [x] `pkg/errcode/` 新写,错误码全段位定义(0-10999)+ Code/Error/IsRetryable
- [x] `pkg/metrics/` 新写,prometheus Register 包装 + StandardBuckets
- [x] `pkg/config/` 基础配置结构;BuildTopic / BuildDLQTopic
- [x] `pkg/grpcserver/` 新写,zrpc 包装 + 4 个默认拦截器(recover/trace/metrics/grpcstats)
- [x] `pkg/grpcclient/` 新写,trace_id 出站透传 + 客户端 metrics
- [x] `pkg/kafkax/consistent.go` 一致性哈希(117 行,FNV-1a + 虚拟节点)
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
- [x] 公共框架重写决策:标 go-zero 推翻 + 加 Kratos 决策 + W2 重写清单(~4.5 天)
- [x] `PROGRESS.md` 加本节(D3 真正收尾)

#### Proto 调整(plan 块 2)

- [ ] 新增 `proto/pandora/push/v1/push.proto`(PushService.Subscribe server stream)
- [ ] 13 个业务 .proto 加 `google.api.http` 注解(W2 时一起加,本期不强制)
- [ ] `buf.yaml` 加 `buf.build/googleapis/googleapis` deps(同上)

⚠️ Proto 调整本期**不全做**:加 google.api.http 注解涉及全部业务 proto,且需要 buf 实际跑通验证,留到 W2 装 buf 后一起做。本期只**新增 push.proto**(下面一步)。

#### Pkg 重写(plan 块 3,留 W2 做)

- [ ] W2 第一周专注做 pkg 重写

### 下次会话 AI 必读(2026-06-04 终版补充)

⚠️ **不能再推翻架构**:Kratos + Envoy + gRPC-Web + 集中 push + UE FHttpModule 自研 grpc-web,这套**已锁死**。任何 AI 想再改之前**必须**读:
1. `architecture-rejected-strict-ds-only.md`(严格 A 反面教材)
2. `gateway-decision.md`(最终架构 + UE gRPC 插件评估)
3. `protocol-ordering-rules.md`(乱序原则)
4. 本 PROGRESS.md(决策演化)

⚠️ **D2 已写的 pkg/ go-zero 代码要在 W2 重写**(~4.5 天)。

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

---

## W2 ③ — login 服务骨架(2026-06-05)

### 背景

W2 ②⁺ 已 commit(`ee12479`),proto 全遵 buf STANDARD + 生成产物 OK。接班按 `HANDOFF.md §3 Step 2` 写 login 服务(Pandora 第一个 Kratos 业务服)。

### 完成内容

#### 1. 修复 buf 生成的 google.api 局部化引用

- `proto/buf.gen.go.yaml` 加 `managed.disable` 排除 `buf.build/googleapis/googleapis`(否则生成的 login.pb.go 会写出 `_ "github.com/.../proto/gen/go/google/api"` 这种我们并没产物的本地引用,导致 build 失败)
- 重新跑 `pwsh tools/scripts/proto_gen.ps1` 让 login.pb.go 改用上游 `google.golang.org/genproto/googleapis/api/annotations`

#### 2. 新增 proto module

- 新增 `proto/go.mod`(module `github.com/luyuancpp/pandora/proto`)
- 把生成的 `gen/go/...` 全部纳入这个 module(后续业务服 import `github.com/luyuancpp/pandora/proto/gen/go/pandora/<X>/v1`)

#### 3. login 服务目录结构(Kratos 标准分层)

```
services/account/login/
├── cmd/login/main.go                  入口:加载 yaml + 装配三层 + 起 Kratos App
├── etc/login-dev.yaml                 dev 配置(⚠️ 不写 duration,见下)
├── go.mod / go.sum                    module + replace pkg/proto 到本地
├── README.md                          端口/职责/启动/W3 路线
└── internal/
    ├── conf/conf.go                   嵌入 pkg/config.Base + LoginConf
    ├── data/account.go                AccountRepo 接口 + MockAccountRepo(W2)
    ├── biz/login.go                   LoginUsecase(纯业务逻辑)
    ├── service/login.go               实现 loginv1.LoginServiceServer
    └── server/
        ├── grpc.go                    grpcserver.MustNewServer + Register
        └── http.go                    phttp.MustNewServer + /metrics + Register
```

#### 4. W2 mock 行为(可联调)

- `Login(account="test", password_hash="abc", device_id=*)` → `ErrCode_OK` + uuid session_token + `127.0.0.1:7777` + uuid hub_ticket
- 账号不对 → `ErrCode_ERR_LOGIN_ACCOUNT_NOT_FOUND`
- 密码不对 → `ErrCode_ERR_LOGIN_PASSWORD_MISMATCH`
- `Logout` → 总是 OK
- `IssueDSTicket` / `VerifyDSTicket` → 返 `ErrCode_ERR_UNKNOWN`(W3 接 JWT + hub_allocator)
- player_id 用 snowflake 启动时生成,固定不变(W3 接 mysql 替换)

#### 5. 端口

| 协议 | 端口 | 用途 |
|---|---|---|
| gRPC | 50001 | 客户端经 Envoy gRPC-Web 来的主流量(W2 直连验证) |
| HTTP | 51001 | `/metrics` Prometheus + `/v1/login` `/v1/logout` `/v1/ds/ticket/*` RESTful |

#### 6. go.work / go.mod 调整

- `go.work` 启用 `use ./proto` 和 `use ./services/account/login`
- `services/account/login/go.mod` 加 `replace` 把 `pandora/pkg` 和 `pandora/proto` 指向本地路径(`go mod tidy` 不读 go.work,只认 replace)

#### 7. 验证(2026-06-05)

```
go build ./pkg/... ./proto/... ./services/account/login/...  全绿
go vet   ./pkg/... ./proto/... ./services/account/login/...  无警告
go run   ./services/account/login/cmd/login -conf services/account/login/etc/login-dev.yaml
  → [HTTP] server listening on: [::]:51001
  → [gRPC] server listening on: [::]:50001

curl POST /v1/login {test/abc/d1}      → code=OK, session_token=uuid, hub=127.0.0.1:7777  ✅
curl POST /v1/login {wrong/abc/d1}     → code=ERR_LOGIN_ACCOUNT_NOT_FOUND                  ✅
curl GET  /metrics                     → 含 pandora_rpc_duration_seconds histogram         ✅
日志带 trace_id(每请求一份 UUID)+ player_id                                                 ✅
```

### 踩到的坑(写给下一会话 AI)

#### 坑 1:Kratos config 不能解析 `"2s"` / `"24h"` 这种 duration 字符串

- 现象:`json: cannot unmarshal string into Go struct field Grpc.Base.Server.Grpc.Timeout of type time.Duration`
- 原因:Kratos config 内部走 JSON 反序列化,`time.Duration` 是 int64,JSON 期望数字,不接受字符串
- W2 解法:**yaml 完全不写 duration 字段**,全靠 `conf.Defaults()` 在代码里设定
- 后续(W3+)如要支持 ops 改 timeout:写一个 `Duration` 包装类型(同时实现 `UnmarshalJSON` 和 `UnmarshalYAML`)替换所有 `time.Duration` 字段;或者改用环境变量
- 影响范围:**所有 14 个业务服共用 `pkg/config.Base`,W3 改一次后续全受益**

#### 坑 2:buf managed mode 会覆盖 googleapis 的 go_package

- 现象:生成的 `login.pb.go` 写 `_ "github.com/luyuancpp/pandora/proto/gen/go/google/api"`,但我们并没生成 google/api 的产物
- 解法:`buf.gen.go.yaml` 加 `managed.disable` 排除 `buf.build/googleapis/googleapis` 模块,让它继续指向上游 `google.golang.org/genproto/googleapis/api/annotations`(已在 module deps)
- 影响:**只要 .proto 引 google/api/annotations.proto(即用 google.api.http 注解)就会踩,本服务 + 后续所有用 HTTP 路由的服务都受益**

#### 坑 3:go.work + replace 双写

- `go mod tidy` 不读 go.work(只读单 module 的 go.mod)
- 所以 services/account/login/go.mod 必须显式 `replace github.com/luyuancpp/pandora/pkg => ../../../pkg`(以及 `pandora/proto`)
- 否则 `go mod tidy` 会去远端找版本,失败:"invalid version: unknown revision 000000000000"
- 后续每个新服务的 go.mod 都要照抄这两条 replace(路径深度可能要调,services 下三层用 `../../../`)

### 待 commit 的改动(用户手动)

```
M  go.work                                         (加 use ./proto + use ./services/account/login)
M  go.work.sum                                     (tidy 自动更新)
M  proto/buf.gen.go.yaml                           (加 disable googleapis)
M  proto/gen/go/pandora/login/v1/login.pb.go       (重新生成,改用上游 google api 包)
D  services/account/login/.gitkeep
?? proto/go.mod / proto/go.sum                     (新 module)
?? services/account/login/README.md
?? services/account/login/cmd/login/main.go
?? services/account/login/etc/login-dev.yaml
?? services/account/login/go.mod / go.sum
?? services/account/login/internal/{conf,data,biz,service,server}/*.go
```

建议 commit:
```
feat(login): W2 ③ login 服务骨架(Pandora 第一个 Kratos 业务服)

- 标准 Kratos 分层:cmd/etc/internal/{conf,data,biz,service,server}
- W2 mock:test/abc 通过,签固定 hub 地址 + uuid session token
- IssueDSTicket / VerifyDSTicket 返 ERR_UNKNOWN,W3 接 JWT 后真实化
- gRPC :50001,HTTP :51001(同时承载 /metrics + RESTful)
- 修复 buf managed 覆盖 googleapis go_package 的 bug
- 新增 proto module(github.com/luyuancpp/pandora/proto)

接 commit ee12479(W2 ②⁺)。
```

### 下一步(W2 ④ / ⑤)

按 `HANDOFF.md §3 Step 3` 起 Envoy(本地 docker),然后 `Step 4` 起 push 服务骨架,
再做 `Step 5` 端到端 hello world,最后 `Step 6` W2 收尾。

⚠️ **写 push / 其它服务时直接复用 login 的目录模板**:
- 拷 `services/account/login/{cmd,etc,internal/{conf,data,biz,service,server}}` 整层
- 改 module 路径(go.mod replace 的相对路径深度按目录层级算)
- 改端口(见 `infra.md §6.2`)
- 改 proto import(`pandora/<X>/v1`)
- yaml **不写 duration 字段**(坑 1)


---

## W2 ⑤ — push 服务骨架(2026-06-05)

### 背景

W2 ③ login 服务模板已稳(commit 待用户手动)。按 `HANDOFF.md §3 Step 4` 用 login 模板复制 push 服务(Pandora 第二个 Kratos 业务服,首个 server stream 服务)。

### 完成内容

#### 1. 目录结构(完全镜像 login,差异仅 server stream + 无 RESTful)

```
services/runtime/push/
├── cmd/push/main.go                   入口:加载 yaml + 装配三层 + 起 Kratos App
├── etc/push-dev.yaml                  dev 配置(⚠️ 不写 duration,沿用 login 经验)
├── go.mod / go.sum                    module + replace pkg/proto 到本地
├── README.md                          职责/铁律/端口/W3 路线
└── internal/
    ├── conf/conf.go                   嵌入 pkg/config.Base + PushConf
    ├── biz/
    │   ├── connection.go              ConnectionManager:player_id → stream 内存索引(顶号)
    │   └── push.go                    PushUsecase + RunMockStream(5s ticker)
    ├── service/push.go                实现 pushv1.PushServiceServer.Subscribe
    └── server/
        ├── grpc.go                    grpcserver.MustNewServer + RegisterPushServiceServer
        └── http.go                    phttp.MustNewServer + /metrics(无 RESTful handler)
```

> data/ 暂未建,等 W3 接 redis ZSET 离线缓存时再加(login 的 data/ 是给 mysql/redis 准备的)。

#### 2. W2 mock 行为(可联调)

- `Subscribe(SubscribeRequest{session_token, last_seen_ms})` server stream
  - 校验 session_token:**W2 跳过**(W3 走 Envoy jwt_authn + 冗余校验)
  - 注册 stream 到 ConnectionManager(顶号语义已实现:同 player_id 旧 stream 自动 cancel)
  - 首帧立发,之后每 5s 推一帧 `PushFrame{topic="pandora.system.notify", payload="hello", ts_ms=now, trace_id=ctx}`
  - ctx.Done(client 断 / server stop / 顶号 cancel)→ 自动反注册退出
- ConnectionManager 已实现 `Register / Unregister / SendTo / Broadcast / Size`,W3 kafka consumer 接入只改 `biz/push.go` + 新增 `biz/consumer.go`

#### 3. 端口

| 协议 | 端口 | 用途 |
|---|---|---|
| gRPC | 50014 | server stream(客户端经 Envoy gRPC-Web 来) |
| HTTP | 51014 | 仅 `/metrics`(`push.proto` 无 `google.api.http` 注解,无 RESTful 入口) |

#### 4. 调整的现有文件

- `go.work`:启用 `use ./services/runtime/push`
- `CLAUDE.md §4.1`:验证命令追加 `./services/runtime/push/...`
- `deploy/prometheus/prometheus.yml`:加 `host.docker.internal:51014 service=push` 抓取目标
- `services/runtime/push/go.mod`:照搬 login 的 `replace pandora/pkg` 和 `pandora/proto` 模式(`../../../`)

#### 5. 验证(2026-06-05)

```
go build ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/...  ✅ exit=0
go vet   ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/...  ✅ vet_exit=0

go run ./services/runtime/push/cmd/push -conf services/runtime/push/etc/push-dev.yaml
  → service_ready  grpc=:50014  http=:51014  mock_tick=5s  mock_topic=pandora.system.notify
  → [gRPC] server listening on: [::]:50014
  → [HTTP] server listening on: [::]:51014
```

> grpcurl Subscribe 流式验证待 W2 ④ Envoy 起来 + W2 ⑥ 端到端测试时一起做(也可现在直连 :50014 测,只是没经 Envoy)。

### 踩到的坑(无新坑)

login 三个坑全部复用方案,push 没踩新坑:
- 坑 1(yaml 不写 duration):etc/push-dev.yaml 完全不写 `mock_tick_interval` 等 duration,靠 `Defaults()` 给 5s
- 坑 3(go.mod replace):直接照抄 login 的 `../../../` 写法

### 待 commit 的改动(用户手动)

```
M  go.work                                                (加 use ./services/runtime/push)
M  CLAUDE.md                                              (§4.1 验证命令追加 push)
M  deploy/prometheus/prometheus.yml                       (加 51014 push 抓取目标)
M  PROGRESS.md                                            (本段)
D  services/runtime/push/.gitkeep
?? services/runtime/push/README.md
?? services/runtime/push/cmd/push/main.go
?? services/runtime/push/etc/push-dev.yaml
?? services/runtime/push/go.mod / go.sum
?? services/runtime/push/internal/{conf,biz,service,server}/*.go
```

建议 commit:
```
feat(push): W2 ⑤ push 服务骨架(Pandora 首个 server stream Kratos 业务服)

- 镜像 login 目录:cmd/etc/internal/{conf,biz,service,server}
- W2 mock:Subscribe 每 5s 推 PushFrame(topic=pandora.system.notify, payload=hello)
- ConnectionManager 已实现顶号 / SendTo / Broadcast / Size(W3 kafka 消费者直接复用)
- gRPC :50014(server stream),HTTP :51014(仅 /metrics,push.proto 无 google.api.http)
- prometheus 抓取目标 + CLAUDE.md §4.1 验证命令同步更新

接 W2 ③ login(待 commit)。
```

### 下一步(W2 ④ / ⑥ / ⑦)

按 `HANDOFF.md §3`:
- **Step 3 (W2 ④)** Envoy v1.38.0 本地 docker:加 `deploy/envoy/envoy.yaml` + `cert.pem/key.pem`(mkcert 生成,不入库)+ docker-compose envoy service。此时可同时配 login + push 两个 cluster
- **Step 5 (W2 ⑥)** 端到端 hello world:经 Envoy grpc-web 测 `LoginService/Login` + `PushService/Subscribe`
- **Step 6 (W2 ⑦)** 收尾:同步 `go-services.md` + 用户 commit/push

---

## W2 ④ — Envoy v1.38.0 边缘网关本地 docker(2026-06-05)

继 W2 ⑤ push 服务后,按 `HANDOFF.md §3 Step 3` 落地 Envoy。完成 Phase A(项目内静态验证)交付,Phase B/C(运行时验证)待 Codex 生证书 + 启 envoy 后继续。

### 完成内容

#### 1. pkg/grpcserver gRPC reflection(回退 — Kratos 已默认开)
- 最初新建 `pkg/grpcserver/reflection.go` + `MustNewServer` 末尾 `registerReflection(srv)`
- **Phase C 运行 push 时发现** `FATAL: duplicate service registration for "grpc.reflection.v1alpha.ServerReflection"`
- 根因:`go-kratos/kratos/v2@v2.9.2/transport/grpc/server.go:197` 默认就调 `reflection.Register(srv.Server)`(v1 + v1alpha),除非传 `kgrpc.DisableReflection()`
- **修复**:删 `reflection.go`,撤回 `MustNewServer` 调用,改在 grpcserver.go 加注释说明"Kratos 默认已开 reflection,W3 上线前用 `kgrpc.DisableReflection()` 关"
- 直连 `:50014` reflection list 验证:`grpc.reflection.v1` / `v1alpha` / `grpc.health.v1.Health` / `grpc.channelz.v1.Channelz` / `kratos.api.Metadata` / `pandora.push.v1.PushService` 全在

#### 2. Envoy 配置(新建 `deploy/envoy/`)
- `envoy.yaml`:listener :8443 TLS(`DownstreamTlsContext` 挂 cert/key) + HCM filters(grpc_web → cors → router) + virtual_host CORS 宽松联调策略 + 4 条 route:
  - `/pandora.login.v1.LoginService/` → login_cluster,timeout 5s / idle 60s
  - `/pandora.push.v1.PushService/`  → push_cluster,**timeout 0s / idle 0s**(server stream 铁律)
  - `/grpc.reflection.v1[alpha].ServerReflection/` → login_cluster,0s
- 两 cluster STRICT_DNS h2c(`typed_extension_protocol_options.HttpProtocolOptions.explicit_http_config.http2_protocol_options: {}` — 漏了 envoy 用 HTTP/1.1 调后端会返 415)
- **`dns_lookup_family: V4_ONLY`(Phase C 现场发现并补)**:Windows Docker Desktop 上 `host.docker.internal` 默认解析到 IPv6(本机实测 `fdc4:f303:9324::254`),envoy 连过去 `cx_connect_fail`,grpcurl 看到 `upstream connect error... remote connection failure`。强制 V4 后走通
- 上游地址 `host.docker.internal:50001 / :50014`(Windows / macOS Docker Desktop 默认解析;Linux 用 compose `extra_hosts: host-gateway` 补)
- admin :9901(本机 only,生产关)
- `.gitignore`:屏蔽 `*.pem` `*.key` `*.crt` 入库
- `README.md`:给 Codex 的证书生成步骤 + Phase B/C 验收命令 + 故障排查速查表 + W3 待办

#### 3. docker-compose 加 envoy service
- `deploy/docker-compose.dev.yml` 追加 envoy 服务:
  - `image: envoyproxy/envoy:v1.38-latest`
  - ports `8443:8443` + `9901:9901`
  - volumes 挂 `envoy.yaml` + `cert.pem` + `key.pem` 到 `/etc/envoy/`
  - `extra_hosts: host.docker.internal:host-gateway`(Linux 兼容)
  - `networks: [pandora-net]`
  - **不加 profiles**(默认随基础设施一起启,用户决策)

#### 4. dev_status.ps1 增端口
- `tools/scripts/dev_status.ps1` 端口列表追加 `8443, 9901`(并加注释说明)

### 验证结果(Phase A — Claude 跑)

| 命令 | 结果 |
|---|---|
| `go build ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/...` | ✅ exit=0(reflection 回退后再次 build 也过) |
| `docker compose -f deploy/docker-compose.dev.yml config --quiet` | ✅ exit=0(envoy service yaml 合法) |
| `docker run --rm ... --mode validate -c /etc/envoy/envoy.yaml` (Codex 生证书前) | ✅ yaml schema 全过,仅 TLS 证书加载失败(预期) |
| `docker run --rm ... --mode validate` (V4_ONLY 修复后,挂证书) | ✅ `configuration ... OK` exit=0 |

### 验证结果(Phase B — Codex 启完 envoy,Claude 跑)

| 命令 | 结果 |
|---|---|
| `docker logs pandora-envoy` | ✅ admin :9901 / loading 2 cluster(s) / 1 listener(s) / all clusters initialized / starting main dispatch loop(Codex 报告) |
| `Invoke-WebRequest http://127.0.0.1:9901/ready` | ✅ status=200 body=LIVE |
| `/clusters?format=json` 数 host_statuses | ✅ login_cluster=1, push_cluster=1 |
| `/listeners` | ✅ `pandora_listener::0.0.0.0:8443` |

### 验证结果(Phase C — Claude 跑)

| 步骤 | 结果 |
|---|---|
| 直连 `:50014` push reflection `list` | ✅ 6 services(reflection v1/v1alpha / health / channelz / kratos.api.Metadata / pandora.push.v1.PushService)— 证实 Kratos 默认开 reflection |
| 经 envoy `:8443` `list`(走 reflection 路由 → login_cluster) | ❌ `upstream connect error`(login 服务未起,login 依赖 mysql/redis 暂没启)— W2 ⑥ 起 login 后回归 |
| 经 envoy `:8443` `PushService/Subscribe`(V4_ONLY 修复前) | ❌ `upstream connect error... remote connection failure` → 抓 envoy `/clusters` 发现 push_cluster 解析到 IPv6 `fdc4:f303:9324::254`,`cx_connect_fail=1` |
| **修复**:`dns_lookup_family: V4_ONLY` 加两 cluster + Codex restart envoy | ✅ push_cluster 解析为 `192.168.65.254:50014` HEALTHY |
| 经 envoy `:8443` `PushService/Subscribe -max-time 12` | ✅ 12s 内收 **3 帧 PushFrame**(topic=pandora.system.notify, payload=`aGVsbG8=`=hello, 间隔 5s 一帧) |
| push 服务日志 | ✅ `push_stream_open online_total=1` + grpcurl 退出后 `mock_push_stream_closed reason=context canceled`(ConnectionManager 顶号 / 流优雅 cancel 验证有效) |

**结论**:server stream `timeout: 0s / idle_timeout: 0s` 规则验证有效,12s 内 envoy 没主动断流。Pandora 客户端连接铁律的 "第 2 条 — FHttpModule → Envoy gRPC-Web over HTTP/2 TLS" 全链路打通。

### 当前 push 服务

- Claude 本地 `go run` 启了 push 服务(:50014 grpc / :51014 http),Phase C 测完已 kill
- login 服务**没起**(依赖 mysql/redis,Codex 没启基础设施 — 在 W2 ④ 范围外,W2 ⑥ 收尾时统一起)

### 待 ChatGPT / Codex 执行

Phase A/B 期间已完成(mkcert 生证书 + 启 envoy + restart 应用 V4_ONLY)。W2 ④ 范围内无剩余环境动作,接 W2 ⑥ 再统一起 mysql / redis / login 走 login 经 envoy 验证。

### 待 Claude 跑(W2 ⑥ 收尾)

```powershell
# 起 mysql + redis(login 依赖)→ 改由 Codex 执行
docker compose -f deploy/docker-compose.dev.yml up -d mysql redis

# Claude 起 login + push
cd e:\work\Pandora\services\account\login; go run ./cmd/login -conf etc/login-dev.yaml   # 终端 A
cd e:\work\Pandora\services\runtime\push;  go run ./cmd/push  -conf etc/push-dev.yaml    # 终端 B

# 直连 :50001 LoginService/Login(基线)
grpcurl -plaintext -d '{\"account\":\"test\",\"password_hash\":\"abc\",\"device_id\":\"d1\"}' `
  127.0.0.1:50001 pandora.login.v1.LoginService/Login

# 经 envoy LoginService/Login(W2 ⑥ 第一项)
grpcurl -insecure -d '{\"account\":\"test\",\"password_hash\":\"abc\",\"device_id\":\"d1\"}' `
  127.0.0.1:8443 pandora.login.v1.LoginService/Login
```

### 待 commit 改动

```
M  deploy/docker-compose.dev.yml
M  pkg/grpcserver/grpcserver.go
M  PROGRESS.md
M  tools/scripts/dev_status.ps1
?? deploy/envoy/.gitignore
?? deploy/envoy/README.md
?? deploy/envoy/envoy.yaml
```

(原计划新建的 `pkg/grpcserver/reflection.go` 已撤回,Kratos 默认已开 reflection。)

建议 commit:
```
feat(envoy): W2 ④ Envoy v1.38 边缘网关本地 docker

- 新增 deploy/envoy/{envoy.yaml,.gitignore,README.md}
  listener :8443 TLS + grpc_web/cors/router filters
  login_cluster (unary 5s) + push_cluster (server stream timeout 0s)
  上游 h2c 显式 http2_protocol_options(漏了 envoy 用 HTTP/1.1 → 415)
  dns_lookup_family V4_ONLY(Windows host.docker.internal 默认 IPv6 解析,envoy 连不上)
  reflection 路由放行(dev 联调用,W3 上线前关)
- docker-compose.dev.yml 加 envoy service(image v1.38-latest,
  extra_hosts host.docker.internal:host-gateway,不加 profile 随基础设施一起启)
- pkg/grpcserver 注释说明 Kratos 默认已开 reflection
  (W3 上线前传 kgrpc.DisableReflection() 关)
- dev_status.ps1 端口表加 8443 / 9901

Phase A 静态验证:compose config + envoy validate 全过。
Phase B 运行时:admin /ready=LIVE,两 cluster 各 1 host,listener :8443 监听。
Phase C 端到端:直连 :50014 reflection list 6 services;
经 envoy :8443 PushService/Subscribe 12s 内收 3 帧 PushFrame
(server stream timeout 0s + dns_lookup_family V4_ONLY 规则验证有效)。

接 W2 ⑤ push(已 commit)。
```

### 下一步(W2 ⑥ / ⑦)

- **W2 ⑥** 端到端 hello world(login 部分待补):Codex 起 mysql + redis,Claude 起 login,跑 grpcurl 经 envoy LoginService/Login。push 部分本轮已完成(3 帧 PushFrame 经 envoy)
- **W2 ⑦** 收尾:同步 `docs/design/go-services.md`(标 login/push/envoy 三项完成)+ 用户 commit/push
- **W3 准备**:reflection 开关化(传 `kgrpc.DisableReflection()`)/ Envoy 加 jwt_authn / 业务服 14 个全上 / 接 OpenTelemetry collector

---

## W2 ⑥ — login 经 envoy 端到端(2026-06-05)

继 W2 ④ envoy 落地后,把 Pandora 客户端连接铁律第 2 条的另一半(unary login)走通。**全 Claude 跑,不需要 Codex / 不需要起 mysql/redis**(login W2 mock 阶段不接外部存储)。

### 完成内容

无新代码改动,纯运行时验证(W2 ④ 已配好 login_cluster + V4_ONLY + reflection 路由)。

### 验证结果

| 步骤 | 命令 | 结果 |
|---|---|---|
| 1. 起 login | `go run ./cmd/login -conf etc/login-dev.yaml` | ✅ service_ready grpc=:50001 http=:51001 mock_player_id=30872216333713408 |
| 2. 直连 :50001 LoginService/Login(基线) | `grpcurl -plaintext ...` | ✅ playerId=30872216333713408 / sessionToken=92ff9dd0-... / hubDsAddr=127.0.0.1:7777 / hubTicket=mock-hub-ticket-7a2c6d97-... |
| **3. 经 envoy :8443 LoginService/Login** | `grpcurl -insecure ...` | ✅ 同字段全在,sessionToken / hubTicket 是新 uuid(证明真到了后端) |
| 4. 经 envoy :8443 reflection list | `grpcurl -insecure 127.0.0.1:8443 list` | ✅ 6 services:reflection v1 / v1alpha / health / channelz / kratos.api.Metadata / **pandora.login.v1.LoginService** |
| 5. login 日志 | tail | ✅ 两次 `trace_id=... msg=login_ok player_id=30872216333713408 device_id=d1` + `rpc_ok transport=grpc op=/pandora.login.v1.LoginService/Login latency_ms=0` |

**结论**:Pandora 客户端连接铁律第 2 条(FHttpModule → Envoy gRPC-Web over HTTP/2 TLS)**unary + server stream 全打通**。

- W2 ④ Phase C:push server stream 12s 收 3 帧 ✅
- W2 ⑥:login unary 拿 4 字段 + reflection list 6 services ✅

login_cluster 在 W2 ④ 加的 `dns_lookup_family: V4_ONLY` 同样有效(本轮直接拿到响应,无需再修)。

### 待 commit 改动

W2 ⑥ 本身**没改任何文件**(无新增 / 无修改),只是把 W2 ④ 配的 login_cluster 路径走通了。

PROGRESS.md 本段属于 W2 ④ 同批 commit 的一部分(W2 ④ 改动已含 PROGRESS.md M),建议合到 W2 ④ commit:

```
feat(envoy): W2 ④ + ⑥ Envoy v1.38 边缘网关 + login/push 经 envoy 端到端

(原 W2 ④ commit 内容)

W2 ⑥ 端到端 hello world 全过:
- 经 envoy :8443 LoginService/Login → playerId + sessionToken + hubDsAddr + hubTicket
- 经 envoy :8443 reflection list → 6 services(含 pandora.login.v1.LoginService)
- 经 envoy :8443 PushService/Subscribe(W2 ④ Phase C 已含)→ 12s 3 帧 PushFrame
- login 日志确认 trace_id 透传 + msg=login_ok player_id=30872216333713408

Pandora 客户端连接铁律第 2 条(FHttpModule → Envoy gRPC-Web over HTTP/2 TLS)
unary + server stream 全打通。
```

### 下一步(W2 ⑦)

- **W2 ⑦** 收尾:
  1. 同步 `docs/design/go-services.md`:标 login / push / envoy 三项 ✅,更新当前里程碑表
  2. CLAUDE.md §7 决策行追加 W2 ④/⑤/⑥ 行
  3. Codex 跑 git status / diff --stat / commit message 建议,用户授权后 Codex commit
  4. 用户手动 push
- **W3 准备**(收尾后立刻能起):
  - reflection 开关化(传 `kgrpc.DisableReflection()`)
  - Envoy 加 jwt_authn(login 真发 JWT,sub 注入 `x-jwt-payload-sub` header 给 push)
  - login 接真 mysql / redis(去掉 mock)
  - 第 3 个业务服:player_locator(在线管理)或 friend(社交首版)


---

## W2 ⑦ — 文档同步收尾(2026-06-05)

W2 ④/⑤/⑥ 三步骨架 + 端到端验证已完成,本段只做文档同步,不动代码,不动 yaml,不动脚本。

### 完成内容

#### 1. `docs/design/go-services.md` 同步状态

- §1 服务总览表新增"骨架状态"列(14 行 + Envoy):
  - **login** ✅ W2 ③(mock,W3 接 mysql/redis)
  - **push** ✅ W2 ⑤(mock 5s tick,W3 接 kafka)
  - **Envoy**(表外加注)✅ W2 ④ 落地(v1.38.0 docker,login_cluster + push_cluster + grpc_web/cors/router)
  - 其它 12 个业务服:⏸️ W3 / W3+ / W4 / W4+ 各按计划标记
- §4 W2 路线图改成 6 步已完成 + 6 项 W3+ 待办的有序清单(原本只有一行文字)

#### 2. `CLAUDE.md` §7 决策行追加 3 行

- W2 ④ 2026-06-05:Envoy v1.38.0 落地(含 V4_ONLY / server stream timeout 0s 关键坑)
- W2 ⑤ 2026-06-05:push 骨架完成(首个 server stream Kratos 服)
- W2 ⑥ 2026-06-05:客户端连接铁律第 2 条全链路打通

#### 3. PROGRESS.md 加本段

### 验证(Claude 跑)

```
go build ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/...
go vet   ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/...
```

两条命令 exit=0(纯文档改动,不涉及代码,只为确保动 docs 后构建仍稳)。

### 待 Codex 执行(W2 ⑦ git 收尾)

按 `AGENTS.md §11.1`,以下交给 Codex:

```powershell
# 在 E:\work\Pandora 执行
git status
git diff --stat
git diff -- CLAUDE.md docs/design/go-services.md PROGRESS.md
```

Codex 应看到 3 个文件变更:

```
 CLAUDE.md                       |  3 +++
 PROGRESS.md                     | +++++++++++++ (本段)
 docs/design/go-services.md      | +++/---       (表加状态列 + W2 路线图重写)
```

**注意**:如果 W2 ④/⑤/⑥ 之前已经 commit 过,本次 W2 ⑦ 单独 commit;如果之前未 commit(W2 ⑤ + W2 ④/⑥ 待 commit 段都在),Codex 应**整批一起 commit** 成一个 commit(3+5+4+6+7 是连续递进,合一个 commit 更清晰)。**Codex 决定后告诉用户**。

#### Codex 可直接用的 commit message 建议(W2 ④+⑤+⑥+⑦ 合一)

```
feat(envoy,push,login): W2 ④⑤⑥⑦ 边缘网关 + push 骨架 + 端到端验证 + 文档同步

W2 ⑤ push 服务骨架(Pandora 首个 server stream Kratos 服)
- services/runtime/push/{cmd,etc,internal/{conf,biz,service,server}}
- Subscribe server stream + 5s mock tick(topic=pandora.system.notify, payload=hello)
- ConnectionManager 顶号 / SendTo / Broadcast(W3 kafka 消费者直接复用)
- gRPC :50014 / HTTP :51014(仅 /metrics)
- prometheus 抓取目标 + go.work + CLAUDE.md §4.1 验证命令同步

W2 ④ Envoy v1.38 边缘网关本地 docker
- deploy/envoy/{envoy.yaml,.gitignore,README.md}
  listener :8443 TLS + grpc_web/cors/router filters
  login_cluster (unary 5s) + push_cluster (server stream timeout 0s)
  上游 h2c 显式 http2_protocol_options(漏了 envoy 用 HTTP/1.1 → 415)
  dns_lookup_family V4_ONLY(Windows host.docker.internal 默认 IPv6,envoy 连不上)
  reflection 路由放行(dev 联调用,W3 上线前关)
- docker-compose.dev.yml 加 envoy service(image v1.38-latest,
  extra_hosts host.docker.internal:host-gateway,默认随基础设施一起启)
- pkg/grpcserver 注释说明 Kratos 默认已开 reflection
  (W3 上线前传 kgrpc.DisableReflection() 关)
- dev_status.ps1 端口表加 8443 / 9901

W2 ⑥ 端到端 hello world(客户端连接铁律第 2 条全打通)
- 经 envoy :8443 LoginService/Login → playerId + sessionToken + hubDsAddr + hubTicket
- 经 envoy :8443 reflection list → 6 services(含 pandora.login.v1.LoginService)
- 经 envoy :8443 PushService/Subscribe -max-time 12 → 收 3 帧 PushFrame
- login 日志确认 trace_id 透传 + msg=login_ok

W2 ⑦ 文档同步收尾
- docs/design/go-services.md §1 服务表加骨架状态列(login/push ✅,Envoy 表外加注 ✅)
- docs/design/go-services.md §4 W2 路线图改有序清单(6 步完成 + 6 项 W3+ 待办)
- CLAUDE.md §7 决策行追加 W2 ④/⑤/⑥
- PROGRESS.md 加 W2 ⑦ 段

Pandora 客户端连接铁律第 2 条(FHttpModule → Envoy gRPC-Web over HTTP/2 TLS)
unary + server stream 全打通。W3 起接 mysql/redis + JWT。

接 commit ee12479(W2 ②⁺)。
```

> 如果 W2 ④/⑤/⑥ 已经 commit,W2 ⑦ 单独 commit message:
>
> ```
> docs(w2-7): W2 ⑦ 收尾 — 同步 go-services / CLAUDE / PROGRESS
>
> - go-services.md §1 服务表加骨架状态列(login/push ✅,Envoy 表外加注 ✅)
> - go-services.md §4 W2 路线图改有序清单(6 步完成 + 6 项 W3+ 待办)
> - CLAUDE.md §7 决策行追加 W2 ④/⑤/⑥
> - PROGRESS.md 加 W2 ⑦ 段
>
> 纯文档,不动代码 / yaml / 脚本。Claude 已跑 build + vet 验证仍稳。
> ```

### 下一步(W3 起点)

W2 收官,W3 真正开始接外部存储 + 鉴权 + 第 3 个业务服。建议 W3 路线:

1. **W3 ①** Envoy `jwt_authn` filter + login 发真 JWT(替换 uuid session_token)
2. **W3 ②** login 接 mysql(account 表)+ redis(session 缓存),去掉 mock
3. **W3 ③** `kgrpc.DisableReflection()` 开关化(`etc/*-dev.yaml` 含 reflection: true,prod 关)
4. **W3 ④** push 接 kafka(消费 `pandora.{team,match,chat,player,friend,system}.*` 6 个 topic)+ redis ZSET 离线 5min
5. **W3 ⑤** 第 3 个业务服:`player_locator`(在线管理,locator.update topic,login 登录时调 SetLocation)
6. **W3 ⑥** Kratos config Duration 包装类型(同时实现 UnmarshalJSON / UnmarshalYAML),解决 yaml 不能写 `"5s"` 这个限制
7. **W3 ⑦** UE 客户端首版:FHttpModule + 自研 grpc-web 解析(参考 `gateway-decision.md` §7)


---

## W3 ① — JWT 真实化 + Envoy jwt_authn 落地(2026-06-05)

### 背景

W2 ⑦ 收尾后,push / login 全 mock(uuid token、未实现 ds 票据)。按 PROGRESS.md 末尾 W3 路线第 ① 项,落地 SessionToken / DSTicket 的真实 JWT,Envoy 接 jwt_authn filter 强制校验。

### 完成内容

#### 1. `pkg/auth` 新包(JWT signer / verifier)

- `pkg/auth/jwt.go` — HS256 实现,公开 API:
  - `Signer.SignSession(playerID, jti) → (token, expMs, err)` — 24h
  - `Signer.SignDSTicket(playerID, dsType, matchID, jti) → (token, expMs, err)` — 5min(不变量 §3)
  - `Verifier.VerifySession(token) → *SessionClaims`
  - `Verifier.VerifyDSTicket(token) → *DSTicketClaims`
  - `JWKSInlineHS256(secret, kid)` — 给 envoy.yaml `local_jwks.inline_string` 算 JWKS
- `pkg/auth/jwt_test.go` — 6 个用例覆盖签 / 验 / 过期 / iss 不对 / 弱 secret 拒绝 / battle 票据缺 match_id
- `pkg/go.mod` 加 `github.com/golang-jwt/jwt/v5 v5.2.1`
- 错误码映射:复用 `errcode.ErrLoginTicketExpired` / `ErrLoginTicketInvalid`,不新增

#### 2. login 服务接入

- `internal/conf/conf.go` — `LoginConf` 加 `JWT JWTConf` 子结构(issuer / audience / secret / session_ttl / ds_ticket_ttl),`Defaults()` 填默认
- `internal/biz/login.go` — `LoginUsecase` 持 `*auth.Signer`;`Login()` 用 `SignSession` 签 session_token,`SignDSTicket(DSTypeHub)` 签 hub_ticket(原来的 uuid + 固定字符串两路全替换)
- `internal/biz/ticket.go` — 新增 `TicketUsecase`,实现 `IssueDSTicket` / `VerifyDSTicket` 业务逻辑
- `internal/service/login.go` — 注入 `TicketUsecase`;`IssueDSTicket` 从 ctx 取 player_id(由 middleware 从 `x-pandora-player-id` 注入)→ 签票;`VerifyDSTicket` 验签 → 翻译成 proto `DSTicket` message
- `cmd/login/main.go` — 装配 `auth.NewSigner / NewVerifier`,失败 `os.Exit(1)`;`service_ready` 日志加 `jwt_issuer` / `jwt_audience` / `jwt_session_ttl` / `jwt_ds_ticket_ttl`
- `etc/login-dev.yaml` — 加 `login.jwt: { issuer, audience, secret }`;⚠️ session_ttl / ds_ticket_ttl 不写 yaml(坑 1 复用)

#### 3. Envoy 配置

- `deploy/envoy/envoy.yaml` — 在 `cors` 后 `router` 前插 `envoy.filters.http.jwt_authn`:
  - `providers.pandora_session`:issuer=pandora-login, audiences=[pandora-client], local_jwks inline HS256 + secret base64url(`cGFuZG9yYS1kZXYtand0LXNlY3JldC1jaGFuZ2UtbWUtMzIh`)
  - `forward_payload_header: x-pandora-jwt-payload`(payload base64url 转发给上游,业务侧暂不用)
  - `claim_to_headers: [{ header_name: x-pandora-player-id, claim_name: sub }]`(上游 pkg/middleware 读这个头注入 ctx)
  - `rules`:
    - `path=/pandora.login.v1.LoginService/Logout` → 必须带 JWT
    - `path=/pandora.login.v1.LoginService/IssueDSTicket` → 必须带 JWT
    - `prefix=/pandora.push.v1.PushService/` → 必须带 JWT
    - 其它(Login 自身 / VerifyDSTicket 内网调 / grpc reflection)默认放行
- 共享 secret 同步:envoy.yaml 内联 JWKS 的 `k` 字段 = base64url(login-dev.yaml 的 `login.jwt.secret`),改一个要改另一个

#### 4. push 服务接入

- `internal/server/grpc.go` — `MustNewServer` 加 `pmw.AuthOptional()` 中间件;Envoy 转发的 `x-pandora-player-id` 头由该中间件解到 `ctx.Value(plog.CtxKeyPlayerID)`
- `internal/service/push.go` — 注释更新(`extractPlayerID` 行为已统一从 ctx 读,不再 W2 兜底直接拿 header)

### 验证(2026-06-05,Claude)

```
go build ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/...   exit=0
go vet   ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/...   exit=0
go test  ./pkg/auth/...                                                                   ok (6 tests)

# Login HTTP 冒烟
POST /v1/login {account:test,password_hash:abc,device_id:d1}
  → code=OK
  → sessionToken=<HS256 JWT>(3 段 dot 分隔,header={alg:HS256,typ:JWT},
     payload 含 iss=pandora-login / aud=pandora-client / sub=30890... / exp=now+24h / jti=uuid)
  → hubTicket=<HS256 JWT>(同上 + ds_type=hub,exp=now+5min)
```

> Envoy 容器重启 + grpcurl -H "authorization: bearer <token>" 端到端验证按 AGENTS.md §11.1 交给 Codex 做(Claude 不重启 docker 容器)。

### 踩到的坑(无新坑)

W3 ① 没踩新坑:
- Kratos config 不解 duration(坑 1):JWTConf 的 session_ttl / ds_ticket_ttl 直接 `Defaults()` 填,yaml 不写
- jwt_authn `claim_to_headers`:`sub` 是顶层 RegisteredClaim,直接 `claim_name: sub` 就行,不需要 jsonpointer
- HS256 JWKS:Envoy 接受 `kty=oct, alg=HS256, k=base64url(secret)` 的 inline_string,跟 RFC 7517 一致

### 待 Codex 执行(W3 ① 收尾)

按 `AGENTS.md §11.1`,以下交 Codex:

```powershell
# 1. 重启 envoy 让新 yaml 生效(jwt_authn 是 listener 级,SIGHUP 不够,要 restart)
cd E:\work\Pandora
docker compose -f deploy/docker-compose.dev.yml restart envoy
docker logs --tail 80 pandora-envoy 2>&1 | Select-String -Pattern 'jwt_authn|provider|error|warn'

# 2. 起 login + push,端到端验证 Envoy + JWT
$loginP = Start-Process -FilePath go -ArgumentList 'run','./services/account/login/cmd/login','-conf','services/account/login/etc/login-dev.yaml' -RedirectStandardOutput .\.tmp-login.log -RedirectStandardError .\.tmp-login.err -PassThru -NoNewWindow
$pushP  = Start-Process -FilePath go -ArgumentList 'run','./services/runtime/push/cmd/push','-conf','services/runtime/push/etc/push-dev.yaml'   -RedirectStandardOutput .\.tmp-push.log  -RedirectStandardError .\.tmp-push.err  -PassThru -NoNewWindow
Start-Sleep 4

# Phase A: Login 经 Envoy 8443(Login 路由不需要 JWT)
$resp = grpcurl -insecure -d '{\"account\":\"test\",\"password_hash\":\"abc\",\"device_id\":\"d1\"}' localhost:8443 pandora.login.v1.LoginService/Login | ConvertFrom-Json
$tok = $resp.sessionToken
Write-Output "sessionToken=$tok"

# Phase B: Subscribe 不带 token → 期望 401(jwt_authn 直接拒,不到 push)
grpcurl -insecure -d '{\"session_token\":\"\",\"last_seen_ms\":0}' -max-time 5 localhost:8443 pandora.push.v1.PushService/Subscribe

# Phase C: Subscribe 带 token → 期望 12s 收 3 帧
grpcurl -insecure -H "authorization: bearer $tok" -d "{\"session_token\":\"$tok\",\"last_seen_ms\":0}" -max-time 12 localhost:8443 pandora.push.v1.PushService/Subscribe

# Phase D: IssueDSTicket 带 token → 期望 OK + 返 hub ticket
grpcurl -insecure -H "authorization: bearer $tok" -d '{\"ds_type\":\"hub\",\"target_id\":\"\"}' localhost:8443 pandora.login.v1.LoginService/IssueDSTicket

# 清理
Stop-Process -Id $loginP.Id -Force; Stop-Process -Id $pushP.Id -Force
Remove-Item .\.tmp-login.log,.\.tmp-login.err,.\.tmp-push.log,.\.tmp-push.err -ErrorAction SilentlyContinue
```

预期:
- Phase A:返 sessionToken / hubTicket(两段 HS256 JWT)
- Phase B:gRPC code 16(UNAUTHENTICATED)— Envoy jwt_authn 直接拒,不到 push
- Phase C:收 3 帧 PushFrame(每 5s 一帧),push 日志含 `player_id` 不为 0
- Phase D:返 ds 票据(5min 短期)

### 待 commit 的改动(待 Codex 输出 git status / diff --stat / commit message 建议)

```
?? pkg/auth/jwt.go                                          (新)
?? pkg/auth/jwt_test.go                                     (新)
?? services/account/login/internal/biz/ticket.go            (新)
M  pkg/go.mod / pkg/go.sum                                  (加 golang-jwt/jwt/v5)
M  services/account/login/internal/conf/conf.go             (加 JWTConf)
M  services/account/login/internal/biz/login.go             (接 Signer)
M  services/account/login/internal/service/login.go         (接 TicketUsecase + 真实 Issue/Verify)
M  services/account/login/cmd/login/main.go                 (装配 Signer/Verifier)
M  services/account/login/etc/login-dev.yaml                (加 login.jwt 配置)
M  services/account/login/go.mod / go.sum                   (拉 jwt 间接依赖)
M  services/runtime/push/internal/server/grpc.go            (加 pmw.AuthOptional)
M  services/runtime/push/internal/service/push.go           (注释更新)
M  services/runtime/push/go.mod / go.sum                    (拉 jwt 间接依赖)
M  deploy/envoy/envoy.yaml                                  (加 jwt_authn filter + rules)
M  CLAUDE.md                                                (§7 W3 ① 决策行)
M  PROGRESS.md                                              (本段)
```

建议 commit message:

```
feat(auth,login,push,envoy): W3 ① JWT 真实化 + Envoy jwt_authn 落地

新增 pkg/auth(HS256 SessionToken 24h + DSTicket 5min,golang-jwt/jwt/v5)
- Signer.SignSession / SignDSTicket;Verifier.VerifySession / VerifyDSTicket
- 错误码映射 errcode.ErrLoginTicketExpired / ErrLoginTicketInvalid
- 6 个单元测试(签/验/过期/iss 拒/弱 secret 拒/battle 缺 match_id)
- JWKSInlineHS256 工具(给 envoy.yaml 同步密钥)

login 服务真 JWT 化
- LoginConf 加 JWT 子结构(issuer/audience/secret + 默认 24h/5min)
- Login() 签 session JWT(sub=player_id, exp=24h)+ hub_ticket(ds_type=hub, exp=5min)
- IssueDSTicket / VerifyDSTicket 接 pkg/auth 真实化(原 W2 返 ErrUnknown)
- main.go 装配 Signer/Verifier;etc/login-dev.yaml 加 login.jwt 节

Envoy jwt_authn 落地
- provider pandora_session(HS256 local_jwks inline,issuer=pandora-login, aud=pandora-client)
- claim_to_headers: sub → x-pandora-player-id(上游业务服直接读)
- rules: push.Subscribe / login.Logout / login.IssueDSTicket 必须带 JWT;
  login.Login / login.VerifyDSTicket / grpc reflection 放行

push 服务接 Envoy 注入的 player_id
- 加 pmw.AuthOptional() 中间件,Subscribe.extractPlayerID 从 ctx 拿 player_id

验证:
- go build / go vet / go test ./pkg/auth/...  全绿
- POST /v1/login → sessionToken / hubTicket 都是合法 HS256 JWT

Envoy 容器重启 + 端到端 grpcurl 验证由 Codex 执行(AGENTS.md §11.1)。
接 commit <W2 ⑦ commit>(W3 起点)。
```

### 下一步(W3 ②)

W3 ① 收尾后,按 PROGRESS.md 末尾 W3 路线第 ② 项做 **login 接 mysql/redis**:

- `services/account/login/internal/data/account_mysql.go` 替换 MockAccountRepo
- `services/account/login/internal/data/session_redis.go` 写 `pandora:sess:<player_id>` hash + jti 黑名单
- Logout() 真实化(DEL session)
- IssueDSTicket() 加 jti SETNX EX 5min 防重放
- `deploy/mysql-init/` 加 `02-account-table.sql`(accounts / account_devices / account_bans)

之后是 W3 ⑤ player_locator(第 3 个 Kratos 业务服)。

---

## W3 ② ✅ login 接 MySQL + Redis 真实化(2026-06-05)

**目标**:把 W2 MockAccountRepo / mock session / mock jti 全部换成 MySQL + Redis 真实实现,
满足不变量 §3(DS 票据短时效)+ §10(锁 TTL ≤ 30s)+ §1 配套(后续 W3 ⑤ 接 locator)。

### 成果

**pkg 复用层(新增,跨服务用)**
- `pkg/mysqlx/mysqlx.go` —— `database/sql + mysql-driver` 工厂,`MustNewClient(c MySQLConf)` 3s PingContext,默认 MaxOpenConns=32 / MaxIdleConns=8 / ConnMaxLifetime=30m
- `pkg/passwd/passwd.go` + `passwd_test.go` —— bcrypt 封装,`Hash(clientDigest, cost)` / `Verify(stored, clientDigest)` `ErrMismatch`,cost 越界自动 clamp 到 DevCost(4),4 个单测全绿
- `pkg/config/config.go` 加 `MySQLConf{DSN, MaxOpenConns, MaxIdleConns, ConnMaxLifetime, PingTimeout}` 和 `NodeConfig.MySQLClient`(注意 duration 字段不写 yaml,留给 Defaults())
- `pkg/auth/jwt.go` 加 accessor:`Signer.SessionTTL()` / `Signer.DSTicketTTL()` / `Verifier.DSTicketTTL()`(让 redis TTL 和 JWT exp 对齐)

**deploy / 基础设施**
- `deploy/mysql-init/02-account-tables.sql` —— `pandora_account` 库 3 表:
  - `accounts(player_id BIGINT UNSIGNED PK, account VARCHAR(64) UK, password_hash VARCHAR(80), status TINYINT, created_at, updated_at)`
  - `account_devices(id AUTO_INCREMENT, player_id, device_id, last_login_at, last_login_ip, UK(player_id,device_id))`
  - `account_bans(id, player_id NULL, device_id NULL, reason, banned_at, expires_at NULL=永久)`

**login data 层重写**
- `services/account/login/internal/data/account.go` —— `AccountRepo` interface(FindByAccount / CreateAccount / CheckBanned / TouchDevice)+ `MockAccountRepo` fallback + `MySQLAccountRepo` 真实实现(`isDupErr` 用字符串匹配 `1062` / `Duplicate entry`,不强依赖 mysql driver type)
- `SessionRepo` interface + `RedisSessionRepo` —— key `pandora:sess:<player_id>`,**TxPipeline 顶号语义**(Del + HSet 字段 token/jti/device_id/exp_ms + Expire),跟 push.ConnectionManager 一致
- `TicketJTIRepo` interface + `RedisTicketJTIRepo` —— key `pandora:ticket:<jti>`,**SETNX EX** 防 replay,冲突返 `ErrLoginTicketReplayed`
- `SeedAccount(ctx, db, account, bcryptHash, fallbackPlayerID)` —— dev 期自动注册 mock_account(`(id, created, err)` 返回值)

**login biz / service**
- `biz/login.go::LoginUsecase` 字段加 `sessions data.SessionRepo` + `verifier *auth.Verifier`;`Login` 用 `passwd.Verify` 替代字符串比较,`sessions.Set(ctx, playerID, token, jti, deviceID, signer.SessionTTL())`,`repo.TouchDevice` 失败仅 Warn 不阻断
- `biz/login.go::Logout` 真实化:`verifier.VerifySession(token)` → `sessions.Delete(playerID)`;签名失败也返 OK(允许 fire-and-forget logout)
- `biz/ticket.go::TicketUsecase` 加 `jtiRepo data.TicketJTIRepo`(可空);`VerifyDSTicket` 在签名 OK 后调 `jtiRepo.MarkUsed(ctx, claims.ID, verifier.DSTicketTTL())`

**login main / etc**
- `cmd/login/main.go` 拆出 `mustBuildAccountRepo(cfg, helper, sf) → (repo, mode, db)`(mysql DSN 非空 → 接 mysql + SeedAccount,否则 mock)和 `mustBuildRedisRepos(cfg, helper) → (sessionRepo, jtiRepo, rdb)`(redis 强依赖,Ping 失败 exit)
- `kratosHelper` interface 包 `*klog.Helper`,`maskDSN()` 把 user:pass 段脱敏后再上日志
- `etc/login-dev.yaml` 加 `node.mysql_client.{dsn, max_open_conns, max_idle_conns}`(**duration 字段不写**,Kratos JSON 不解时长)

### 验证

- `go build ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/...` exit=0
- `go vet ./pkg/... ./services/account/login/...` exit=0
- `go test ./pkg/...` 全绿(`passwd` 4 用例 + 旧 `auth` `snowflake`)

### Codex 需要做的(W3 ② 收尾)

1. `docker compose -f deploy/docker-compose.dev.yml up -d mysql redis` —— 启基础设施
2. 进 mysql 容器:`docker exec -it pandora-mysql mysql -upandora -ppandora_dev_pwd pandora_account -e "SHOW TABLES;"` 应见 3 张表
3. 启 login:`Push-Location services/account/login; go run ./cmd/login -conf etc/login-dev.yaml; Pop-Location`
4. 预期日志:`service_ready ... account_repo=mysql session_repo=redis jti_repo=redis ...`
5. grpcurl 联调 `LoginService/Login` `{account:"test", password:"abc"}` 应返 `session_token` / `hub_ticket`(都是 HS256 JWT)
6. `redis-cli -p 6380 HGETALL pandora:sess:<player_id>` 应见 token/jti/exp_ms 字段
7. 提 commit:`feat(login): W3 ② MySQL+Redis 接入,passwd bcrypt 验证 + session 顶号 + ticket jti 防 replay`

---

## W3 ⑤ ✅ player_locator 服务上线(2026-06-05)

**目标**:落地不变量 §1"玩家只能在一个 Location"。Redis hash `pandora:locator:<player_id>` 30s TTL,
SetLocation 用 **TxPipeline(Del+HSet+Expire)** 写,避免切状态时旧字段残留(MATCHING→HUB 时 match_id 还在)。

### 成果

**proto 复用** —— `proto/pandora/locator/v1/locator.proto`(W1 时已 gen go pb,本次直接复用)

**player_locator 全骨架(第 3 个 Kratos 业务服)**
- `services/runtime/player_locator/go.mod` —— module `github.com/luyuancpp/pandora/services/runtime/player_locator`,replaces pkg/proto
- `services/runtime/player_locator/etc/locator-dev.yaml` —— gRPC :50006 / HTTP :51006,redis_client `127.0.0.1:6380`,**duration 字段不写**(Kratos JSON 不解 duration)
- `internal/conf/conf.go` —— `LocatorConf.LocationTTL` 默认 30s(由 `Defaults()` 提供)
- `internal/data/location.go` —— `LocationRecord{State, HubPod, ShardID, MatchID, BattlePod, UpdatedAtMs}`,`LocationRepo` interface,`RedisLocationRepo` key 模板 `pandora:locator:%d`,Set = TxPipeline(Del+HSet+Expire),Get = HGetAll + strconv 反解,Delete = Unlink
- `internal/biz/locator.go` —— 状态常量 0-5(对齐 proto enum),`LocatorUsecase{repo, ttl}`,SetLocation 按状态分支校验(HUB→hub_pod,MATCHING→match_id,BATTLE→match_id+battle_pod),GetLocation miss 返 `{State: LocationStateOffline=1}`,ClearLocation Delete
- `internal/biz/locator_test.go` —— 7 个用例覆盖输入校验各分支 / Set→Get 闭环 / miss=OFFLINE / Clear 后 Get=OFFLINE / 默认 TTL fallback,**全绿**
- `internal/service/locator.go` —— 实现 `PlayerLocatorServiceServer`,Location ↔ biz 翻译,`toProtoCode` 跟 login 同 pattern
- `internal/server/grpc.go` —— `grpcserver.MustNewServer(cfg.Server)`,**无 AuthRequired**(内部 RPC,W3+ Envoy ext_authz 限制外部调用)
- `internal/server/http.go` —— 只 `/metrics`
- `cmd/locator/main.go` —— Redis 强依赖(Ping 失败 exit),LocatorUsecase ttl 从 `cfg.Locator.LocationTTL`

**login → locator 集成(不变量 §1 入口)**
- `pkg/grpcclient` 已有 `MustDialInsecure` 直接复用
- `services/account/login/internal/data/locator_client.go` 新建 —— `LocationNotifier` interface(只暴露 `NotifyLoginPending(ctx, playerID, deviceID)`,biz 不依赖 grpc / proto)+ `GrpcLocationNotifier` 用 `*grpc.ClientConn` 包 `PlayerLocatorServiceClient`,调 SetLocation(state=LOGIN_PENDING)
- `biz/login.go::LoginUsecase` 加 `notifier data.LocationNotifier` 字段;Login 在 sessions.Set 之后调 `notifier.NotifyLoginPending(ctx, playerID, deviceID)`,**失败仅 Warn 不阻断**(locator 不可用不能影响登录)
- `conf/conf.go::LoginConf` 加 `Locator LocatorClientConf{Addr}`;`cmd/login/main.go::mustBuildLocatorNotifier` 拨号失败 panic(grpcclient.MustDialInsecure 内部 panic),addr 空 → 返回 nil(biz 检查跳过)
- `etc/login-dev.yaml` 加 `login.locator.addr: "127.0.0.1:50006"`(可改空禁用)

**go.work + 文档**
- `go.work` 加 `use ./services/runtime/player_locator`,注释段更新启用标记
- `CLAUDE.md §4.1` 验证命令追加 `./services/runtime/player_locator/...`
- `CLAUDE.md §7` 加 W3 ②/⑤ 决策行

### 验证

- `go build ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/... ./services/runtime/player_locator/...` exit=0
- `go vet` 同上四组 exit=0
- `go test` 同上四组 exit=0(locator biz 7 用例 + 旧测试全绿)

### Codex 需要做的(W3 ⑤ 收尾)

1. 确保 W3 ② 的 redis 已启(`docker compose up -d redis`)
2. 启 locator:`Push-Location services/runtime/player_locator; go run ./cmd/locator -conf etc/locator-dev.yaml; Pop-Location`
3. 预期日志:`service_ready ... grpc=:50006 http=:51006 redis_addr=127.0.0.1:6380`
4. grpcurl 联调:
   ```powershell
   grpcurl -plaintext -d '{\"player_id\":\"42\",\"location\":{\"state\":\"LOCATION_STATE_HUB\",\"hub_pod\":\"hub-pod-7\"}}' 127.0.0.1:50006 pandora.locator.v1.PlayerLocatorService/SetLocation
   grpcurl -plaintext -d '{\"player_id\":\"42\"}' 127.0.0.1:50006 pandora.locator.v1.PlayerLocatorService/GetLocation
   ```
5. `redis-cli -p 6380 HGETALL pandora:locator:42` 应见 state=3 / hub_pod=hub-pod-7 / updated_at_ms
6. 验联动:再启 login,grpcurl Login 后 `redis-cli HGETALL pandora:locator:<player_id>` 应见 state=2(LOGIN_PENDING)
7. 提 commit:`feat(player_locator): W3 ⑤ 第 3 个 Kratos 业务服上线,redis 30s TTL,login 集成 LOGIN_PENDING 上报`

### 下一步(W3 路线剩余)

- **W3 ③** trade 服务骨架(经济链路第 1 服)
- **W3 ④** kafka 异步事件骨架(player.login event → push.broadcast)
- **W3 ⑥** Envoy ext_authz 把 player_locator / 内部 RPC 拒外

---

## W3 ②/⑤ Codex 审查修复(2026-06-05)

Codex 在 W3 ②/⑤ commit 前审查发现 6 个阻塞项,Claude 按 AGENTS.md §11.1 分工修复(代码 / 文档 / 验证由 Claude 做,git 收尾仍交 Codex):

| # | 问题 | 修复 |
|---|---|---|
| 1 | `login/internal/server/grpc.go::NewGRPCServer` 没接 `pmw.AuthOptional()`,带 token 调 IssueDSTicket 返 `ERR_UNAUTHORIZED` | grpc.go 增加 `pmw.AuthOptional()` 中间件(跟 push 同 pattern),注释说明 Optional 而非 Required 的理由(Login 本身无 token、Envoy 已按 path 强制 JWT) |
| 2 | `go.work` 是 `go 1.25.0`,与 HANDOFF.md `go1.26.4` 不符 | `go.work` 升 `go 1.26.4` 锁定一致,注释段补"再升级须同步 HANDOFF.md" |
| 3 | 改动范围混 W3 ① / ② / ⑤ + AGENTS/CLAUDE 协作规则,跨度大 | 本轮 commit 仍单批(用户已批 plan),但通过 PROGRESS.md 拆段 + commit message 明确 scope = "W3 ②+⑤ + Codex 审查修复"。下次最高可用 Claude 模型按规则一段 plan 一次 commit |
| 4 | `pkg/auth.Config.Validate` 注释写 ≥32 字节但实际只校验 ≥16 | Validate 改为 `< 32` 拒,错误消息 `need >=32 for HS256`(对齐 RFC 7518 §3.2);新增 `TestValidateRejects16And31ByteSecrets`(2 子用例)+ `TestValidateAccepts32ByteSecret` |
| 5 | `VerifyDSTicket` 未对称 SignDSTicket 的 battle/match_id 防御 | VerifyDSTicket 新增 `dsType==battle && match_id==""` → `ErrLoginTicketInvalid`;新增 `TestVerifyDSTicketRejectsBattleWithoutMatchID`(用 raw jwt 库构造恶意 token 测对称防御) |
| 6 | `.tmp-locator.err` Codex 联调时 redirect 的临时 906B 日志 | `.gitignore` 加 `.tmp-*` 兜底;经用户授权后 `Remove-Item` 删除文件(已不在 working tree) |

### 验证

- `go build ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/... ./services/runtime/player_locator/...` exit=0
- `go vet` 同范围 exit=0
- `go test ./pkg/... ./services/account/login/... ./services/runtime/player_locator/...` exit=0
- `pkg/auth` 测试 `-v -run "Validate|VerifyDSTicket"` 6 个新/旧用例(含子用例)全 PASS

### Codex 端到端复测建议(W3 ① 四阶段)

1. `LoginService/Login` `{account:"test", password:"abc"}` 无 token → 应返 `session_token`/`hub_ticket`
2. `PushService/Subscribe` 无 Authorization → 应被 Envoy 401
3. `PushService/Subscribe` 带 `Authorization: Bearer <session_token>` → 应在 12s 内收 ≥3 帧
4. `LoginService/IssueDSTicket` 带 `Authorization: Bearer <session_token>` `{ds_type:"hub"}` → 应返 `ticket`(本次修复前会返 `ERR_UNAUTHORIZED`,修复后通过 `pmw.AuthOptional()` 注入 ctx player_id 解决)

### 提交 commit 建议(交 Codex)

`fix(login,auth): W3 ②/⑤ Codex 审查修复 — login gRPC 加 AuthOptional, auth Secret 强制 >=32 + VerifyDSTicket 防 battle/match_id 缺失, go.work 锁 1.26.4, .gitignore 加 .tmp-*`

---

## W3 ��/�� ? Duration ��װ���� + gRPC reflection ���ػ�(2026-06-05)

**Ŀ��**:��� W2 �����ᴩ etc/*.yaml �� 'Kratos JSON ���� duration' ��ʷ��(��ǰ����ʱ���ֶα��� yaml ���� + Defaults() ����),���� W3 �� ��¶�� 'gRPC reflection Ĭ�Ͽ� �� schema й¶����' �ĳ������ɹء�dev �ɿ��Ŀ��ء�

### Phase 1: pkg/config.Duration ��װ����

- ���� `pkg/config/duration.go`:`type Duration time.Duration` + `Std()` / `String()` / `UnmarshalJSON` / `MarshalJSON`;UnmarshalJSON �����ַ����� `time.ParseDuration`,ʧ������ `int64` ns(�����ݾ� yaml/json);�Ƿ�ֵ�� input ������
- ���� `pkg/config/duration_test.go`:8 �� `TestDuration_*` ����,��:
  - �ַ���·��:'5s' / '1h30m' / '0s' / '500ms' / '-2m' / ''
  - ����������:5000000000 �� 5s
  - null no-op / �Ƿ� 'abc' / '5'(�޵�λ) / {} / [] ȫ���� error
  - MarshalJSON ����������ַ��� + Round-trip �ջ�
  - **���� e2e**: `TestDuration_E2E_KratosConfig` ��ʵ�� `file.NewSource �� kconfig.New �� Load �� Scan`,���� yaml д 'dial_timeout: `\`\`\2s\\`\`' �� `cfg.Redis.DialTimeout.Std() == 2*time.Second`(��ǰ���� yaml ���� '������ Defaults' ��,������һ������֤����ͨ·����)

### Phase 2: pkg/config + grpcserver + pkg ҵ�������

- `pkg/config/config.go`:15+ �� `time.Duration` �ֶ�ȫ������ `config.Duration`,���� Grpc.Timeout / Http.Timeout / NodeConfig.MaxLoginDuration & LogoutGraceTime / RedisConf 4 �� timeout / MySQLConf 2 �� / KafkaConfig 5 �� / EtcdRegistryConf.DialTimeout / TimeoutConf 4 ��;`time` import �Ƴ���
- `pkg/config.Grpc` ���� `EnableReflection bool`(`yaml:enable_reflection,omitempty`),Ĭ�� false��
- `pkg/grpcserver/grpcserver.go`:`c.Grpc.Timeout` ���ô� `.Std()`;���� `if !c.Grpc.EnableReflection { opts = append(opts, kgrpc.DisableReflection()) }` ���� prod Ĭ����ֵ �� �� reflection,dev yaml ��ʽ `enable_reflection: true` �� �� Kratos Ĭ����Ϊ��
- ҵ��� `.Std()` ����(�� `go build` �����������ҵ�):`pkg/mysqlx`(ConnMaxLifetime / PingTimeout)/ `pkg/transport/http`(Timeout)/ `pkg/kafkax/producer.go`(DialTimeout / ReadTimeout / WriteTimeout / RetryBackoff)/ `pkg/svc/base.go`(RedisClient 3 �� timeout)��
- `pkg/auth` ����(������ lib,�� Kratos config ����,SessionTTL/DSTicketTTL �� time.Duration,���÷�װ��ʱ `.Std()` ��)��

### Phase 3: 3 �������÷���Ǩ��

- **login** `services/account/login`:
  - `internal/conf/conf.go`: `SessionTokenTTL` / `DSTicketTTL` / `JWTConf.SessionTTL` / `JWTConf.DSTicketTTL` 4 ���ֶθ� `config.Duration`;`Defaults()` Ĭ��ֵ�� `config.Duration(24*time.Hour)` д����
  - `cmd/login/main.go`: `auth.Config` װ�䴦 `cfg.Login.JWT.SessionTTL.Std()` / `DSTicketTTL.Std()`;`rc.DialTimeout` �� redis 3 �� `.Std()`��
  - `etc/login-dev.yaml`:
    - ɾ������ 'Kratos JSON ���� duration / ��дʱ���ֶ�' ��ƪע��;
    - �� `server.grpc.enable_reflection: true`;
    - ��ʵд duration �ַ���ʾ��:`redis_client.dial_timeout: '2s'` / `mysql_client.conn_max_lifetime: '30m'` / `mysql_client.ping_timeout: '3s'` / `login.session_token_ttl: '24h'` / `login.ds_ticket_ttl: '5m'` / `login.jwt.session_ttl: '24h'` / `login.jwt.ds_ticket_ttl: '5m'`��
- **push** `services/runtime/push`:`MockTickInterval` / `OfflineCacheTTL` �� Duration;`cmd/push/main.go` ���� `biz.NewPushUsecase` �� `.Std()`;`etc/push-dev.yaml` �� `enable_reflection: true` + `mock_tick_interval: '5s'` + `redis.dial_timeout: '2s'` + `offline_cache_ttl: '5m'`��
- **player_locator** `services/runtime/player_locator`:`LocationTTL` �� Duration;`cmd/locator/main.go` ���� `biz.NewLocatorUsecase` �� `.Std()` + rc 3 �� `.Std()`;`etc/locator-dev.yaml` �� `enable_reflection: true` + `location_ttl: '30s'` + `redis.dial_timeout: '2s'`��
- **���� 11 ��δ���÷���**(account/player / social/{friend,chat,dialogue} / matchmaking/{team,matchmaker} / battle/{ds_allocator,hub_allocator,battle_result} / economy/trade / data/data_service):W2/W3 ����ʱֱ�Ӱ��� Phase 3 ģ��Ǩ�ơ�

### Phase 4: �ĵ�ͬ��

- CLAUDE.md ��7 �� W3 ��/�� ������(���ĵ�ͬ���� PROGRESS.md ���)��
- /memories/repo/pandora.md �� `config.Duration` �÷� / Ĭ��ֵд�� / reflection ���ط�ʽ / Std() �������� 4 ��Ҫ�㡣

### ��֤(build/vet/test ������ EXIT=0)

```powershell
Push-Location e:\work\Pandora
go build ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/... ./services/runtime/player_locator/...   # EXIT=0
go vet   ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/... ./services/runtime/player_locator/...   # EXIT=0
go test  ./pkg/... ./services/account/login/... ./services/runtime/player_locator/...   # PASS:pkg/auth pkg/config pkg/passwd pkg/snowflake locator/internal/biz
Pop-Location
```

`pkg/config` ���� 8 �� `TestDuration_*` ȫ�� PASS(�� `TestDuration_E2E_KratosConfig` ��ʵ Kratos config Load+Scan ��·)��

### Codex ��β����

- `git status` / `git diff --stat`:Ԥ�� 14 ���ļ��Ķ�(pkg/config 2 �� + pkg/config 1 �� + pkg/grpcserver/mysqlx/transport-http/svc/kafkax 5 �� + 3 �� conf/main/yaml 9 �� + CLAUDE.md / PROGRESS.md)��
- ���� commit message:`feat(config,grpcserver): W3 ��/�� Duration ��װ���� + reflection ���ػ� �� pkg/config.Duration �� \5s\/\24h\, server.grpc.enable_reflection Ĭ�� false, ������ 3 �� etc yaml Ǩ��`��
- �������û�������׼��

### ��һ��

�� PROGRESS.md ����·�߽��� **W3 �� hub_allocator / W3 �� token ����** �� **W3 �� Kafka ����**(�� Opus 4.8 �� Plan)��


---

## W3 ④ ✅ push 接 kafka + redis ZSET 离线 5min(2026-06-05)

**目标**:落地不变量 §9(kafka topic key = player_id,同一玩家事件落同一 partition 保序)+ 协议铁律原则 2(发起方不收自己触发的 push,工程化为 `kafkax.PushToPlayers` helper)。push 服务从 W2 ⑤ mock tick 退役,真实消费 3 个 push topic,在线直推 / 离线 redis ZSET 5min 缓存,客户端断线重连按 `last_seen_ms` 补推。

### 成果

**pkg/kafkax 公共能力**
- `pkg/kafkax/topics.go` —— 集中 6 个 push topic 常量(`TopicTeamUpdate` / `TopicMatchProgress` / `TopicChatWorld` / `TopicChatTeam` / `TopicChatPrivate` / `TopicPlayerUpdate` / `TopicFriendEvent` / `TopicSystemNotify`),`PushTopics = []string{TopicTeamUpdate, TopicMatchProgress, TopicChatPrivate}` 为 W3 ④ 默认订阅清单(余 3 个等 proto Event 定义补)
- `pkg/kafkax/producer.go::KeyOrderedProducer.PushToPlayers(ctx, callerPlayerID, toPlayerIDs, payload) (sent int, lastErr error)` —— 循环 `SendRaw(key=strconv.Itoa(playerID))`,`callerPlayerID` 匹配则跳过(原则 2),`callerPlayerID=0` 表示原则 3 例外全发,单条失败 log+continue 不中断整批
- `pkg/kafkax/producer_test.go` —— 4 用例(sarama/mocks):`TestPushToPlayers_SkipsCaller` / `CallerZeroSendsAll` / `PartialFailureContinues` / `AllCallerNoSend`,全 PASS

**push 服务 W3 ④ 真实化**
- `services/runtime/push/internal/conf/conf.go`:删 `MockTickInterval` / `MockTopic` / `MockPayload`,加 `Topics []string` + `OfflineCacheTTL config.Duration`;`Defaults()`:Topics 空时复制 `kafkax.PushTopics`、`OfflineCacheTTL` 默认 `5m`、`Kafka.GroupID` 默认 `pandora-push`
- `services/runtime/push/etc/push-dev.yaml` 重写:加 `kafka.brokers=["127.0.0.1:9093"]` / `group_id="pandora-push"` / `partition_cnt=4` / `dial_timeout="2s"` / `idempotent=true`;`push.topics` 3 个 + `offline_cache_ttl: "5m"`
- `services/runtime/push/internal/data/offline.go`(新):`OfflineCacheRepo` interface(Append / Range)+ `RedisOfflineCacheRepo{rdb, ttl, seq atomic.Uint64}`;key 模板 `pandora:push:offline:%d`;`encodeMember(payload)` 追加 `0x1F + seq` 后缀防同 ts_ms 多帧被 ZSET 去重塌陷;Append 用 `TxPipeline(ZAdd + Expire)` 每写刷 TTL;Range 用 `ZRangeByScoreWithScores`,`sinceMs>0` 用 `(<sinceMs>` 开区间
- `services/runtime/push/internal/data/offline_test.go`(新)—— 4 用例(miniredis):`AppendRangeRoundTrip` / `RangeSinceMsOpenInterval` / `TTLRefreshOnAppend` / `SameTsMultipleFrames`,全 PASS
- `services/runtime/push/internal/biz/consumer.go`(新):`FrameSender` interface(`SendTo(playerID, *PushFrame) (online, err)`),`*ConnectionManager` 满足;`KafkaConsumer{topic, cm, offline, consumer *kafkax.KeyOrderedConsumer}` + `NewKafkaConsumer(brokers, groupID, topic, partitionCnt, cm, offline)`;`handle(ctx, msg)`:`strconv.ParseInt(msg.Key)` 非数字 → log+ack 跳过;构 `PushFrame{Topic, Payload=msg.Value, TsMs=msg.Timestamp.UnixMilli(), TraceId=headerStr("trace_id")}`;在线 `cm.SendTo` 成功 → ack;在线 SendTo 失败（stream 断）或玩家离线 → 一律 `offline.Append` 后 ack，依靠客户端重连时 `last_seen_ms` 补推，幂等判重由客户端按 ts_ms + trace_id 处理
- `services/runtime/push/internal/biz/consumer_test.go`(新)—— 4 用例(mockSender + mockOffline):`HandleOnline` / `HandleOffline` / `HandleInvalidKey` / `HandleOnlineSendFail`,全 PASS
- `services/runtime/push/internal/biz/push.go` 重写:删 `RunMockStream`,`PushUsecase{conns, offline}` + `NewPushUsecase(conns, offline)`;`RunSubscribeStream(ctx, stream, playerID, sinceMs)`:`sinceMs>0 && playerID>0` 时 `offline.Range` 拉补推 → `for f := range frames { stream.Send(f); 检查 ctx.Err }` → 阻塞 `<-ctx.Done()`
- `services/runtime/push/internal/service/push.go`:Subscribe 内调 `s.uc.RunSubscribeStream(subCtx, stream, playerID, req.GetLastSeenMs())`
- `services/runtime/push/cmd/push/main.go` 重写:`mustBuildRedis` Ping 失败 exit、`mustBuildConsumers` 遍历 `cfg.Push.Topics` 每 topic 一个 `KafkaConsumer`;装配 ConnectionManager + RedisOfflineCacheRepo + PushUsecase + PushService;启动期 `for _, kc := range consumers { kc.Start() }`,defer Close;`service_ready` 日志含 `kafka_brokers / kafka_group / topics / offline_ttl`

**新错误码(双向同步)**
- `pkg/errcode/errcode.go` 加 `ErrPushOfflineCorrupted Code = 9301` / `ErrPushKafkaConsumerDown Code = 9302`
- `proto/pandora/common/v1/errcode.proto` 加 `ERR_PUSH_OFFLINE_CORRUPTED = 9301;` / `ERR_PUSH_KAFKA_CONSUMER_DOWN = 9302;`,跑 `pwsh tools/scripts/proto_gen.ps1` 重生成 `proto/gen/go/pandora/common/v1/errcode.pb.go`(buf 1.70.0 验通过)

**依赖**
- `services/runtime/push/go.mod`:`IBM/sarama v1.43.1` / `redis/go-redis/v9 v9.16.0` / `alicebob/miniredis/v2 v2.33.0`(测试用)/ `google.golang.org/protobuf v1.36.11` 提升为 direct;`go mod tidy` 通过

### 验证(全部 EXIT=0)

```powershell
Push-Location e:\work\Pandora
go build ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/... ./services/runtime/player_locator/...
go vet   ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/... ./services/runtime/player_locator/...
go test  ./pkg/... ./services/account/login/... ./services/runtime/push/... ./services/runtime/player_locator/...
Pop-Location
```

W3 ④ 新增 12 个单测全 PASS:`kafkax` 4(PushToPlayers) + `push/internal/data` 4(offline ZSET) + `push/internal/biz` 4(KafkaConsumer handle)。

### 风险 / 已知 not-yet

- 仅订阅 3 个 topic(team.update / match.progress / chat.private),`pandora.player.update` / `pandora.friend.event` / `pandora.system.notify` 等业务服上线 + Event message proto 定义后再加进 `kafkax.PushTopics` + push yaml
- `KafkaConsumer.handle` 在线 SendTo 成功 → ack；在线 SendTo 失败（stream 断）或玩家离线 → 一律 `offline.Append` 后 ack，客户端重连时按 `last_seen_ms` 拉补推，幂等判重由客户端按 ts_ms + trace_id 处理（二次修复 R2 后，`offline.Append` 失败会 Inc `pandora_push_offline_append_failed_total{topic}` + 返 errcode 9301，kafka 仍 ack，靠告警发现）
- 离线 ZSET TTL 每写刷新 = 长期在线但偶发掉线的玩家最多可能跨 5min 内丢消息,本期接受

### Codex 端到端联调步骤(W3 ④ 收尾)

1. 起基础设施:`docker compose -f deploy/docker-compose.dev.yml up -d kafka redis mysql`
2. 启 push:`Push-Location services/runtime/push; go run ./cmd/push -conf etc/push-dev.yaml; Pop-Location`,日志应见 `service_ready ... topics=[pandora.team.update pandora.match.progress pandora.chat.private]`
3. 启 login + grpcurl 拿 `session_token`(同 W3 ② 流程)
4. grpcurl 带 `Authorization: Bearer <session_token>` 调 `pandora.push.v1.PushService/Subscribe`,挂住等推送
5. 进 kafka 容器 produce:`docker exec -it pandora-kafka kafka-console-producer.sh --bootstrap-server 127.0.0.1:9093 --topic pandora.team.update --property "parse.key=true" --property "key.separator=:"` 输入 `42:dummy-payload` → grpcurl 那边应立即收到一帧 PushFrame
6. 断 grpcurl → produce 一条 key=42 → `redis-cli -p 6380 ZRANGE pandora:push:offline:42 0 -1 WITHSCORES` 应见 1 条
7. 重连带 `last_seen_ms` 比补推前小 → 应即时收到补推帧
8. 提 commit:`feat(push,kafkax): [proto] W3 ④ push 接 kafka + redis ZSET 离线 5min — PushToPlayers helper + 3 个默认 topic + offline encodeMember 0x1F+seq 防 ZSET 去重 + 错误码 9301/9302`

### 下一步

按 PROGRESS.md 既有路线进入 **W3 ⑦ hub_allocator**(把 §1 不变量补全:玩家分配 hub 实例)、**W3 ⑧ matchmaker 骨架** 或 **W3 ⑨ team 服务**(由 Opus 4.8 出 Plan)。

---

## W3 ④ Opus 审查二次修复(2026-06-05)

Codex 审过 W3 ④ 一次修复后,Opus 4.8 又审了一遍,最高可用 Claude 模型按 Plan 执行的二次修复。

### 风险表

| 级别 | 问题 | 修复 |
|---|---|---|
| **HIGH R1** | gRPC `ServerStream.Send` 非并发安全,KafkaConsumer.SendTo 与 RunSubscribeStream replay 循环可能同时写同一 stream → 撕坏 HTTP/2 帧(对端 RST_STREAM / 解码失败)。窗口短但正好命中"重连补推 + kafka 持续推"时刻 | 每条 stream 包成 `*StreamSlot{stream, sendMu}`,所有 Send 走 `slot.SafeSend(frame)` 串行化(连 Broadcast 也是);`ConnectionManager.Register` 返回 `*StreamSlot`,replay 循环用 slot 而不是裸 stream;`Unregister(playerID, slot)` 比对 slot 防止顶号时新 stream 删错位置 |
| **MEDIUM R2** | `offline.Append` 失败时只 log、返 nil → kafka 仍 ack offset → 客户端按 `last_seen_ms` 重连也补不回 → **静默丢消息**。对称于 R1-轮 send-fail fallback 的另一半 | 新建 `services/runtime/push/internal/biz/metrics.go` 注册 `pandora_push_offline_append_failed_total{topic}` CounterVec;`handle` 失败时 `OfflineAppendFailed.WithLabelValues(msg.Topic).Inc()` + 返 `errcode.ErrPushOfflineCorrupted` (9301)。kafka 仍 ack(W3 ④ 不引入死信队列),改为可观测告警驱动 |
| **LOW R4** | `RedisOfflineCacheRepo.Range` 写了 `if err != nil && !errors.Is(err, redis.Nil)` —— 但 `ZRangeByScoreWithScores` 对 missing key 返 `([], nil)`,不会返 redis.Nil,死代码 | 简化为 `if err != nil`,移除 `errors` import |
| **LOW R5** | `errcode.ErrPushKafkaConsumerDown=9302` 没有 caller | 加注释说明"W4 push 健康检查 / consumer group rebalance handler 触发",W3 ④ 占位 |
| LOW R3 | `encodeMember` seq 进程重启重置 | 不影响:seq 不同 → member 不同 → 不会被 ZSET 去重塌缩。原 Plan 已分析过,无代码改动 |
| LOW R6 | README PowerShell 命令含 bash JSON 转义 | 跳过:PowerShell 单引号字面量按字面传,Codex 联调实跑会发现再改 |

### 改动文件

- `services/runtime/push/internal/biz/connection.go` —— `StreamSlot` + `SafeSend`,`Register` 返回 slot,`Unregister(playerID, slot)` 比对 slot
- `services/runtime/push/internal/biz/push.go` —— `RunSubscribeStream` 签名改 `(ctx, slot *StreamSlot, ...)`,补推循环走 `slot.SafeSend`
- `services/runtime/push/internal/service/push.go` —— `Subscribe` 接 `slot := Register(...)`,传 slot 给 RunSubscribeStream;defer Unregister 用 slot
- `services/runtime/push/internal/biz/metrics.go` —— **新增**,`pandora_push_offline_append_failed_total{topic}` CounterVec,init 调 `metrics.Register`
- `services/runtime/push/internal/biz/consumer.go` —— `handle` 末段 `Inc + errcode.New(9301)`
- `services/runtime/push/internal/data/offline.go` —— `Range` 删 `errors.Is(redis.Nil)`,删 `errors` import
- `pkg/errcode/errcode.go` —— 9301 注释补"offline.Append 写 redis 失败(W3 ④ 二次修复)",9302 加 TODO 占位说明
- `services/runtime/push/internal/biz/connection_test.go` —— **新增**,3 用例:`TestSendTo_ConcurrentSafe`(50 goroutine × 200 iter)、`TestBroadcast_ConcurrentSafe`(500 + 500 混跑)、`TestRegister_TopOff`(顶号 + Unregister 误判保护);用 `atomic.Bool` reentrance detector 在无 -race 时也能查并发 Send
- `services/runtime/push/internal/biz/consumer_test.go` —— mockOffline 加 `appendErr` 字段;新增 `TestKafkaConsumer_HandleOfflineFail` 断言返 9301 + err 含原因

### 验证

- 5-module build / vet / test 全 PASS,push biz 测试用例增至 8(原 4 + 新 4)
- **race detector 未跑**:本机无 mingw gcc,`go test -race` 报 `cgo: C compiler "gcc" not found`。reentrance detector(atomic.Bool)已能在 50×200 并发下暴露原 BUG,留 Codex 在 mingw / linux 环境跑 `go test -race ./services/runtime/push/internal/biz/...` 做最终把关
- 完整命令:
  ```pwsh
  go build ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/... ./services/runtime/player_locator/...
  go vet   ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/... ./services/runtime/player_locator/...
  go test  ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/... ./services/runtime/player_locator/... -count=1
  ```

### Codex 收尾

1. 安装 gcc(如 `winget install msys2` → `pacman -S mingw-w64-x86_64-gcc`,把 `C:\msys64\mingw64\bin` 加 PATH),跑 `$env:CGO_ENABLED="1"; go test -race ./services/runtime/push/internal/biz/... -count=1`,确认无 race
2. (可选)产线接 prometheus 后,Grafana / Alertmanager 加规则:`rate(pandora_push_offline_append_failed_total[5m]) > 0` 即告警
3. 提 commit:
   ```
   fix(push): Opus W3 ④ 审查二次修复 — ConnectionManager StreamSlot.sendMu 防 stream.Send 并发 + offline.Append 失败计数器/9301 errcode + offline.Range 死代码清理

   - HIGH R1: StreamSlot.sendMu 串行化 SafeSend,Register 返回 slot,Unregister 比对 slot;新增 3 个并发安全单测(reentrance detector)
   - MED  R2: 新建 biz/metrics.go pandora_push_offline_append_failed_total{topic};handle 失败 Inc + errcode 9301 返回;新增 TestKafkaConsumer_HandleOfflineFail
   - LOW  R4: offline.Range 删除 redis.Nil 死代码 + errors import
   - LOW  R5: errcode 9302 加 TODO 占位说明
   ```
   (本次无 proto 改动,不带 `[proto]` 标记)

---

## W3 ⑦ ✅ team 服务上线(2026-06-05)

Pandora 第 4 个 Kratos 业务服(login / push / player_locator 之后),首个"多写 RPC + 乐观锁 + kafka 广播"组合服。`go.work` 已 `use ./services/matchmaking/team`。

### 职责与端口

- gRPC **:50010** / HTTP **:51010**(HTTP 仅挂 `/metrics`,team.proto 无 `google.api.http` 注解)
- Redis **强依赖**(队伍状态 WATCH/MULTI/EXEC 乐观锁)
- Kafka producer 发布 `pandora.team.update`(push 服务已订阅消费)
- dev:`enable_reflection: true` 便于 grpcurl 联调;prod 零值关闭

### 7 个 RPC(全"立即完成型",协议原则 1)

| RPC | 语义 | push |
|---|---|---|
| `CreateTeam` | 建队,playerID 为队长 | 推队长自己(创建快照确认) |
| `Invite` | 邀请目标玩家 | 不发 inviter(原则 2) |
| `AcceptInvite` | 接受邀请入队 | 广播队内 |
| `LeaveTeam` | 离队 | 广播队内 |
| `Kick` | 队长踢人 | 不发 captain(原则 2) |
| `SetReady` | 设置准备状态 | 广播队内 |
| `GetTeam` | 只读拉完整快照(进大厅时一次) | 无 |

### Redis key 设计

- `pandora:team:{<team_id>}` = `TeamStorageRecord` proto bytes;hashtag `{}` 括住 team_id 保同队所有 key 落同一 cluster slot(兜底)
- `pandora:team:player:<player_id>` = `team_id` string,`ClaimPlayer` 用 **SETNX** 原子声明归属,落不变量 §1(一人只能在一个队);CreateTeam 写 team 失败时回滚 claim(`DeletePlayerIndex`)避免玩家被永久锁死
- `pandora:team:invite:<invite_id>` = hash(`team_id` / `target_player_id`),TTL=`InviteTTL`(60s);2 字段短令牌按 CLAUDE.md §5.9 保留 hash 不升级 proto bytes

### 并发与状态机

- 写路径统一走 `UpdateWithLock`:WATCH → GET(proto 反序列化)→ fn(modify) → MULTI/SET/EXEC;EXEC=nil(CAS 失败)重试至 `OptimisticRetry` 次,耗尽返 `ERR_TEAM_CONCURRENT=3007`
- fn 自身业务错误不重试,直接透传;非 CAS 的其他 redis 错误也不重试
- 状态机:`FORMING` → `READY`(全员 ready)/ `READY` → `FORMING`(任一成员 leave/kick);`MATCHING` / `IN_BATTLE` 由后续撮合/战斗服驱动;`DISBANDED` 拒绝任何写(`ErrTeamWrongState`),解散后改短 TTL(`DisbandedRetention` 5min)供客户端查最终态
- `TeamStorageRecord` proto 直接当存储 record,克隆一律 `proto.Clone`,**禁止值拷贝**(`a := *rec` 会复制内部 state/mu/sizeCache)

### kafka 广播

- 写路径成功后发 `pandora.team.update` 的 `TeamUpdateEvent`(`proto.Marshal` → `PushToPlayers`),key=player_id 落不变量 §9 同 partition 保序
- biz 通过 `TeamEventPusher` 接口解耦,不直接依赖 kafka/grpc;`callerPlayerID != 0` 时排除发起者自身(协议原则 2)

### 配置(全 `config.Duration`)

`ActiveTTL` 60min(活跃队伍生命周期,防僵尸队)/ `InviteTTL` 60s / `DisbandedRetention` 5min / `MaxMembers` 5(MOBA 5v5)/ `OptimisticRetry` 乐观锁重试次数。`Defaults()` 兜零值防 panic。

### 验证

- 6-module build / vet / test 全 PASS(新增 `./services/matchmaking/team/...`):
  ```pwsh
  go build ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/... ./services/runtime/player_locator/... ./services/matchmaking/team/...
  go vet   ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/... ./services/runtime/player_locator/... ./services/matchmaking/team/...
  go test  ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/... ./services/runtime/player_locator/... ./services/matchmaking/team/... -count=1
  ```
- biz + data 单测覆盖(状态机迁移 / 乐观锁冲突 / ClaimPlayer 一人一队 / invite TTL)

### 后续路线(W4)

`hub_allocator`(补不变量 §1:玩家分配 hub 实例)/ `matchmaker` 骨架 / UE 客户端首版,均为 W3 路线图末尾"可选下一步",顺延 W4,由 Opus 4.8 出 Plan。
