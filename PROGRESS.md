# Pandora 进度记录

> 本文档**只追加,永不删旧条目**。AI 新会话第一件事就是读这里。

## W1 (2026-06-03 起)

### 立项决策(Round 0)

| 项 | 决策 |
|---|---|
| 项目名 | **Pandora**(项目)/ pandora(资源命名空间) |
| 后端仓库 | https://github.com/luyuancpp/Pandora.git(public) |
| UE 仓库 | git 仓库 **Pandora-Client** https://github.com/luyuancpp/Pandora-Client.git（本地目录 `C:\work\Pandora`）；UE 工程统一为 **Pandora**（2026-06-09 仓库由 Xuanming 改名） |
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
⚠️ Go 版本统一使用当前工程锁定的 **Go 1.26.4**。早期原计划 1.23、D2 阶段曾因依赖自动升到 1.24,均为历史记录;实际编译口径以 `go.work` 和各 module `go.mod` 为准。后续升级 Go 版本时,必须同步 `go.work` / 各 `go.mod` / 相关文档并跑完整 build / test / vet。

⚠️ kafkax 是 **W1-D2 简化版**:无 retry queue / 无 DLQ / 无 plainProducer。W2 写 battle_result 时再补全。

⚠️ Phase 2 docker compose 没有 `up -d` 实跑(留给用户;镜像 pull 需要他网络)。`compose config --quiet` 已验证 yaml 语法 + 端口绑定正确。

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

#### 文档落地(13 个动作,任务块 1)

- [x] `protocol-ordering-rules.md` 加 §3.1 设计 smell 详解(CreateTeam 案例 + 5 条 smell 表现)
- [x] `gateway-decision.md` 大改:三连接 → 两连接 + Kratos + Envoy + 自研 grpc-web(W2 实现指南)
- [x] `gateway-decision.md` §11 加 UE gRPC 插件评估(为什么不用第三方插件 + 5 个共性坑 + 大厂客户端协议事实)
- [x] `architecture-rejected-strict-ds-only.md`(2026-06-03 已存在,保留作反面教材)
- [x] `pandora-arch.md` §3 服务清单 15 → 14(删 gateway,push 改 gRPC server stream)
- [x] `pandora-arch.md` §6 协议矩阵全面重写(gRPC-Web over HTTP/2 TLS + Envoy 路由)
- [x] `pandora-arch.md` §11 决策行追加 7 条(2026-06-04)
- [x] `ds-arch.md` §0.2 协议矩阵:两连接 + 强调 Client 不走 gRPC + 后台走 gRPC
- [x] `ds-arch.md` §0.3 反模式禁令加 2 条(不拉 grpc-cpp / 不装第三方 UE gRPC 插件)
- [x] `ds-arch.md` §0.3 "为什么这样设计" 精确化:gRPC 不适合 tick 同步 ≠ 不适合业务请求;两条连接物理独立故障域隔离;Battle DS 内部 gRPC 不阻塞 tick
- [x] `go-services.md` §1 总览 13 → 14(去 gateway,push 改 gRPC server stream + 50014)
- [x] `go-services.md` §4 W2 路线图 + §5 push 服务 Kratos 风格契约
- [x] `infra.md` §6.2 端口表去 gateway 8080 / push 8081 → 加 push 50014;§6.3 加 Envoy 8443
- [x] 公共框架重写决策:标 go-zero 推翻 + 加 Kratos 决策 + W2 重写清单(~4.5 天)
- [x] `PROGRESS.md` 加本节(D3 真正收尾)

#### Proto 调整(任务块 2)

- [ ] 新增 `proto/pandora/push/v1/push.proto`(PushService.Subscribe server stream)
- [ ] 13 个业务 .proto 加 `google.api.http` 注解(W2 时一起加,本期不强制)
- [ ] `buf.yaml` 加 `buf.build/googleapis/googleapis` deps(同上)

⚠️ Proto 调整本期**不全做**:加 google.api.http 注解涉及全部业务 proto,且需要 buf 实际跑通验证,留到 W2 装 buf 后一起做。本期只**新增 push.proto**(下面一步)。

#### Pkg 重写(任务块 3,留 W2 做)

- [ ] W2 第一周专注做 pkg 重写

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

### 验证(2026-06-05)

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

> Envoy 容器重启 + grpcurl -H "authorization: bearer <token>" 端到端验证作为环境联调验证。

### 踩到的坑(无新坑)

W3 ① 没踩新坑:
- Kratos config 不解 duration(坑 1):JWTConf 的 session_ttl / ds_ticket_ttl 直接 `Defaults()` 填,yaml 不写
- jwt_authn `claim_to_headers`:`sub` 是顶层 RegisteredClaim,直接 `claim_name: sub` 就行,不需要 jsonpointer
- HS256 JWKS:Envoy 接受 `kty=oct, alg=HS256, k=base64url(secret)` 的 inline_string,跟 RFC 7517 一致

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

## W3 ②/⑤ 提交前审查修复(2026-06-05)

提交前审查发现 6 个阻塞项,已修复:

| # | 问题 | 修复 |
|---|---|---|
| 1 | `login/internal/server/grpc.go::NewGRPCServer` 没接 `pmw.AuthOptional()`,带 token 调 IssueDSTicket 返 `ERR_UNAUTHORIZED` | grpc.go 增加 `pmw.AuthOptional()` 中间件(跟 push 同 pattern),注释说明 Optional 而非 Required 的理由(Login 本身无 token、Envoy 已按 path 强制 JWT) |
| 2 | `go.work` 是 `go 1.25.0`,与 HANDOFF.md `go1.26.4` 不符 | `go.work` 升 `go 1.26.4` 锁定一致,注释段补"再升级须同步 HANDOFF.md" |
| 4 | `pkg/auth.Config.Validate` 注释写 ≥32 字节但实际只校验 ≥16 | Validate 改为 `< 32` 拒,错误消息 `need >=32 for HS256`(对齐 RFC 7518 §3.2);新增 `TestValidateRejects16And31ByteSecrets`(2 子用例)+ `TestValidateAccepts32ByteSecret` |
| 5 | `VerifyDSTicket` 未对称 SignDSTicket 的 battle/match_id 防御 | VerifyDSTicket 新增 `dsType==battle && match_id==""` → `ErrLoginTicketInvalid`;新增 `TestVerifyDSTicketRejectsBattleWithoutMatchID`(用 raw jwt 库构造恶意 token 测对称防御) |

### 验证

- `go build ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/... ./services/runtime/player_locator/...` exit=0
- `go vet` 同范围 exit=0
- `go test ./pkg/... ./services/account/login/... ./services/runtime/player_locator/...` exit=0
- `pkg/auth` 测试 `-v -run "Validate|VerifyDSTicket"` 6 个新/旧用例(含子用例)全 PASS

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

## W3 ④ Opus 审查二次修复(2026-06-05)

W3 ④ 一次审查修复后又复查了一遍,本段记录二次修复。

### 风险表

| 级别 | 问题 | 修复 |
|---|---|---|
| **HIGH R1** | gRPC `ServerStream.Send` 非并发安全,KafkaConsumer.SendTo 与 RunSubscribeStream replay 循环可能同时写同一 stream → 撕坏 HTTP/2 帧(对端 RST_STREAM / 解码失败)。窗口短但正好命中"重连补推 + kafka 持续推"时刻 | 每条 stream 包成 `*StreamSlot{stream, sendMu}`,所有 Send 走 `slot.SafeSend(frame)` 串行化(连 Broadcast 也是);`ConnectionManager.Register` 返回 `*StreamSlot`,replay 循环用 slot 而不是裸 stream;`Unregister(playerID, slot)` 比对 slot 防止顶号时新 stream 删错位置 |
| **MEDIUM R2** | `offline.Append` 失败时只 log、返 nil → kafka 仍 ack offset → 客户端按 `last_seen_ms` 重连也补不回 → **静默丢消息**。对称于 R1-轮 send-fail fallback 的另一半 | 新建 `services/runtime/push/internal/biz/metrics.go` 注册 `pandora_push_offline_append_failed_total{topic}` CounterVec;`handle` 失败时 `OfflineAppendFailed.WithLabelValues(msg.Topic).Inc()` + 返 `errcode.ErrPushOfflineCorrupted` (9301)。kafka 仍 ack(W3 ④ 不引入死信队列),改为可观测告警驱动 |
| **LOW R4** | `RedisOfflineCacheRepo.Range` 写了 `if err != nil && !errors.Is(err, redis.Nil)` —— 但 `ZRangeByScoreWithScores` 对 missing key 返 `([], nil)`,不会返 redis.Nil,死代码 | 简化为 `if err != nil`,移除 `errors` import |
| **LOW R5** | `errcode.ErrPushKafkaConsumerDown=9302` 没有 caller | 加注释说明"W4 push 健康检查 / consumer group rebalance handler 触发",W3 ④ 占位 |
| LOW R3 | `encodeMember` seq 进程重启重置 | 不影响:seq 不同 → member 不同 → 不会被 ZSET 去重塌缩。原分析已覆盖,无代码改动 |
| LOW R6 | README PowerShell 命令含 bash JSON 转义 | 跳过:PowerShell 单引号字面量按字面传,联调实跑发现后再改 |

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
- **race detector 未跑**:本机无 mingw gcc,`go test -race` 报 `cgo: C compiler "gcc" not found`。reentrance detector(atomic.Bool)已能在 50×200 并发下暴露原 BUG,留 CI / Linux 环境跑 `go test -race ./services/runtime/push/internal/biz/...` 做最终把关
- 完整命令:
  ```pwsh
  go build ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/... ./services/runtime/player_locator/...
  go vet   ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/... ./services/runtime/player_locator/...
  go test  ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/... ./services/runtime/player_locator/... -count=1
  ```

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

## W4 ① matchmaker 服务(2026-06-06)

Pandora 第 5 个 Kratos 业务服,撮合 5v5。gRPC :50011 / HTTP :51011(仅 /metrics)。
4 个 RPC 全"已受理型"(协议原则 3):客户端 UI 状态机由 `pandora.match.progress` push 驱动。

### 撮合流水线

`StartMatch(team)` → 写排队票据(avg_mmr 入 ZSET)→ 后台 `RunMatchLoop`(MatchInterval 2s)
`matchOnce` 撮合 + `expireOnce` 确认期超时扫描:

```
QUEUEING → FOUND → CONFIRM → ALLOCATING → READY
                      └─ 任一拒绝 / 超时 ─→ FAILED(其余票据退回队列)
```

- `matchOnce`:`RangeQueueTickets` 按 avg_mmr 升序 → 贪心累积进组,组内 MMR 跨度在
  **动态窗口**(base 200 / +20 每等待秒 / max 2000)内,凑齐 2×TeamSize=10 人 →
  `binPack` largest-first 装箱拆成 5+5 → `formMatch`(写 match record + `ReserveTicket`
  预留票据移出队列写 match_id + 推 FOUND/CONFIRM)
- `expireOnce`:扫 `pandora:match:active` ZSET,`confirm_deadline_ms ≤ now` 的 match
  标记 FAILED,票据退回队列(rejecterID=0,无明确拒绝者全部退回)

### 4 RPC

| RPC | 语义 | 进度推送 |
|---|---|---|
| `StartMatch` | 队伍入队(captain_id 以 JWT ctx 为准) | QUEUEING(全体含发起方) |
| `CancelMatch` | 排队中→删票据释放归属;已撮合→等价拒绝确认 | FAILED(若已撮合) |
| `ConfirmMatch` | 全员 accept→READY;任一 reject→FAILED | CONFIRM/READY/FAILED |
| `GetMatchProgress` | 只读(match_id 或 ticket_id 句柄) | 无 |

### Redis key 设计

- `pandora:match:queue` = ZSET(score=avg_mmr,member=ticket_id),撮合池
- `pandora:match:ticket:%d` = `MatchTicketStorageRecord` proto bytes,TTL=TicketTTL(30min)
- `pandora:match:{%d}` = `MatchStorageRecord` proto bytes(hashtag `{}` 锁 cluster slot)
- `pandora:match:player:%d` = ticket_id string,`ClaimPlayer` 用 **SETNX** 落不变量 §1
  一人只在一个队列;StartMatch 任一成员冲突则回滚已声明的
- `pandora:match:active` = ZSET(score=confirm_deadline_ms,member=match_id),确认期超时扫描

### 并发与状态机

- match 写路径统一走 `UpdateMatchWithLock`:WATCH/MULTI/EXEC 乐观锁,CAS 失败重试至
  `OptimisticRetry` 次,耗尽返 `ERR_MATCH_CONCURRENT=4006`(新增 errcode)
- 确认失败:其余票据 `RequeueTicket` 退回队列**保留 `enqueued_at_ms`**(排队时长不丢失),
  拒绝者整队 `DeleteTicket` + 释放成员归属
- 全员确认 → `DSAllocator.AllocateBattle`(W4 ① 打桩 `StubDSAllocator` 返回固定 ds_addr +
  每玩家 mock 票据;W4 ② 接 ds_allocator gRPC)→ 写 READY 带 ds_addr,每玩家单独推
  专属 `battle_ticket`
- `MatchStorageRecord` / `MatchTicketStorageRecord` proto 直接当存储 record,克隆一律
  `proto.Clone`,禁止值拷贝

### proto 改动 [proto]

新增(match/v1):`MatchTicketStorageRecord` / `MatchStorageRecord` / `MatchMemberStorageRecord`
/ `MatchConfirmStatus` 枚举;common/v1 新增 `ERR_MATCH_CONCURRENT=4006`。已 regen go + cpp pb。

### 解耦接口(biz 不依赖 grpc/kafka 具体实现)

- `TeamReader`(弱依赖 team gRPC,team_addr 空则跳过队伍校验退化为单人票据兜底)
- `MatchEventPusher`(kafka `pandora.match.progress`,原则 3 例外 callerID=0 发全体含发起方)
- `DSAllocator`(W4 ① StubDSAllocator 打桩)
- `IDGenerator`(snowflake.Node 生成 match_id)

### 验证(2026-06-06)

7-module build / vet / test 全 PASS(新增 `./services/matchmaking/matchmaker/...`):

```pwsh
go build ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/... ./services/runtime/player_locator/... ./services/matchmaking/team/... ./services/matchmaking/matchmaker/...
```

- biz 单测 4 用例:撮合成型(10 单人票→5+5 CONFIRM)/ 全确认 READY 带 ds_addr /
  拒绝退回(拒绝者票据删、对方退回)/ 确认期超时失败
- data 单测 4 用例:票据往返 + 队列 avg_mmr 升序 / ClaimPlayer SETNX 冲突 /
  UpdateMatchWithLock 持久化 + 错误透传不写回 / active ZSET 超时扫描 + ExpireMatch 保留 record

## W4 ② — ds_allocator 服务上线 + matchmaker 接真实拉 DS(2026-06-06)

Pandora 第 6 个 Kratos 业务服。撮合全员确认后由 matchmaker 调 ds_allocator 拉一个
战斗 DS,ds_allocator 维护 DS 状态镜像 + 心跳超时补偿。W4 ② 用 Mock 分配器跑通全链路,
真 Agones GameServerAllocation CRD 接入留 W4 ③+(环境就绪步)。

### ds_allocator 服务(services/battle/ds_allocator/)

- gRPC :50020 / HTTP :51020(仅 /metrics,ds proto 无 google.api.http 注解)
- 4 RPC:
  - `AllocateBattle`:matchmaker 全员确认后调,申请 DS pod → 写镜像 → 回 ds_addr/pod。
    **幂等**:同 match_id 已有镜像直接回已分配地址(防 matchmaker 重试重复拉 DS)
  - `ReleaseBattle`:对局结束/异常,回收 pod + 删镜像(幂等,镜像不存在视为已释放)
  - `Heartbeat`:战斗 DS 每 5s 上报(单向 unary),刷新 last_heartbeat_ms + state;
    孤儿 DS(无镜像)返 command=`stop` 让其自停
  - `ListBattles`:运维/调试查询(可按 state 过滤)
- 后台 `RunHeartbeatSweep`(SweepInterval 5s):`sweepOnce` 扫
  `RangeStaleBattles(now - HeartbeatTimeout 15s)` → 标记 abandoned + GameServer.Release +
  移出 active,终态镜像保留供查(不变量 §4 DS 崩溃必有补偿;W4 ③ TODO 通知 battle_result
  做玩家段位回滚)
- `GameServerAllocator` 接口 + `MockGameServerAllocator`(W4 ②):
  pod=`pandora-battle-<match_id>`,addr=`<host>:<base + match_id%range>`

### Redis key

- `pandora:ds:battle:{<match_id>}` = `BattleStorageRecord` proto bytes(hashtag `{}` 锁
  cluster slot,TTL=BattleTTL 2h)
- `pandora:ds:active` = ZSET(score=last_heartbeat_ms,member=match_id),心跳超时扫描
- 状态写 WATCH/MULTI/EXEC 乐观锁,冲突重试耗尽返 `ERR_DS_ALLOCATION_FAILED=5002`

### matchmaker 接真实拉 DS(GrpcDSAllocator)

- 新增 `internal/data/ds_allocator.go` 的 `GrpcDSAllocator`,替换 W4 ① 的 `StubDSAllocator`
  (`ds_allocator_addr` 非空才启用,否则仍走桩,本机不起 ds_allocator 也能跑撮合骨架)
- **职责切分**:ds_allocator 只负责"拉一个 DS pod"返回 ds_addr/pod_name,**不签票据**;
  battle DSTicket 由 matchmaker 用 `pkg/auth.Signer.SignDSTicket(pid, DSTypeBattle, match_id, uuid)`
  签发(不变量 §3 短时效 5min;MMR 在 battle_result 算,DS 不可信,不变量 §6,票据须可信后端签)
- matchmaker 配置:`match.ds_allocator_addr` / `match.map_id`(uint32)/ `match.game_mode` +
  顶层 `jwt`(issuer/audience/secret/session_ttl/ds_ticket_ttl,secret 与 login/Envoy 共享)

### proto 改动 [proto]

新增(ds/v1):`BattleStorageRecord`(服务端 Redis 存储快照:match_id / ds_pod_name /
ds_addr / state / player_ids / map_id / game_mode / allocated_at_ms / last_heartbeat_ms /
player_count)。已 regen go + cpp pb。复用既有 `ERR_DS_*`(5001-5004),无新增 errcode。

### 解耦接口

- ds_allocator biz `BattleRepo`(data Redis 实现)/ `GameServerAllocator`(Mock 实现)
- matchmaker biz `DSAllocator`(StubDSAllocator 桩 / GrpcDSAllocator 真实)

### 验证(2026-06-06)

8-module build / vet / test 全 PASS(新增 `./services/battle/ds_allocator/...`):

```pwsh
go build ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/... ./services/runtime/player_locator/... ./services/matchmaking/team/... ./services/matchmaking/matchmaker/... ./services/battle/ds_allocator/...
```

- ds_allocator biz 单测 7 用例:分配 / 幂等回放 / 释放幂等 / 心跳更新状态 /
  孤儿 DS 返 stop / 列举 + state 过滤 / 心跳超时 sweep 标记 abandoned 并移出 active
- ds_allocator data 单测 6 用例:创建-读回往返 + active ZSET / get miss / stale 扫描 /
  UpdateBattleWithLock 刷新 + active score / notfound 返 ErrDSPodNotFound / 删除清 active
- matchmaker 修复:go.mod 补回 `alicebob/miniredis/v2` 直接依赖(W4 ① tidy 误删)

## W4 文档规则补充 — 客户端可见结构与存储快照隔离(2026-06-06)

用户确认一条协议硬规则:**不能直接把服务器的存储数据发送给客户端**。

已落文档:
- `CLAUDE.md` §5.11 / §9.14:面向客户端的 RPC response / push payload 只能使用"客户端可见结构",不得直接返回 `*StorageRecord`、数据库整行、Redis value、内部 Kafka envelope 或审计字段。
- `docs/design/pandora-arch.md` §9 / §11:作为架构不变量和决策行同步。
- `docs/design/proto-design.md` §7:写 proto 时按客户端当前需求的最小字段集填充,必要时由服务端计算派生字段。

判断:这个要求是对的,应作为硬性要求。它能减少敏感字段泄漏、避免客户端和服务端存储结构耦合、降低以后存储迁移成本,也能让 response / push 更稳定、更小。


---

## W4 ③ — battle_result 服务上线 + ds_allocator 发 abandoned 事件(2026-06-06)

Pandora 第 7 个 Kratos 业务服。对局结算落库 + MMR 计算 + DS 崩溃补偿闭环。

### 范围

- **battle_result 新服**:gRPC :50022 / HTTP :51022(仅 /metrics)
- **MySQL 强依赖**(`pandora_battle` 库,无 Redis):
  - `battles`(PK match_id,outcome NORMAL/ABANDONED,winner_team,ds_pod_name,game_mode,map_id)
  - `battle_player_stats`(uk_match_player(match_id,player_id) 幂等键,kills/deaths/assists/伤害/治疗/金币/mmr_delta)
- **消费 `pandora.battle.result`** → `ReportResult` 幂等落库(不变量 §2,SaveResult 命中 unique → alreadyRecorded 不重复写)
- **标准 Elo MMR 在此算**(不变量 §6,DS 上报 mmr_delta 一律被覆盖):
  - `expectedA = 1/(1+10^((avgB-avgA)/400))`,K=32,胜 1 / 负 0 / 平 0.5
  - 两队按 avg MMR 算 deltaA/deltaB,写回每个 stat.mmr_delta;K 相等时两队对称
  - W4 ③ player 服务未上线 → `StaticMMRReader` 全返 base_mmr 1500;`player_addr` 留作 player gRPC reader 钩子
- **消费 `pandora.ds.lifecycle` 的 ABANDONED** → `HandleAbandoned` 写 outcome=ABANDONED + delta 全 0 补偿记录(幂等,不变量 §4)
- **落库成功才发 `pandora.player.update`** `PlayerUpdateEvent`(kafka key=player_id 不变量 §9,player 服务上线后消费做幂等 UpdateMMR;弱依赖 broker 不通则静默丢)
- **RPC**:`ReportResult`(同步兜底)/`GetMatchResult`/`ListPlayerHistory`
  - 风险入口加固(复审):`ReportResult` 收到 `Outcome=ABANDONED` 时**强制 mmr_delta 全 0**(不走 assignMMR),防 DS 不可信地通过 battle.result 伪造 abandoned 改段位(不变量 §4/§6);权威 abandoned 路径仍是 ds.lifecycle → HandleAbandoned
- **ds_allocator 改动**:`sweepOnce` 心跳超时 abandoned 后,经新增 `DSLifecyclePusher`(**弱依赖**,nil-safe)发 `DSLifecycleEvent{phase=ABANDONED, player_ids/map_id/game_mode}`(key=match_id)给 battle_result 补偿,替换原 `// TODO(W4 ③)` 注释

### proto 改动 [proto]

- `proto/pandora/battle/v1/battle.proto`:新增 `enum BattleOutcome { UNSPECIFIED/NORMAL/ABANDONED }` + `BattleResult.outcome=10`(field 9 保留)
- `proto/pandora/player/v1/player.proto`:新增 `message PlayerUpdateEvent { player_id, match_id, mmr_delta, reason, ts_ms }`
- `proto/pandora/ds/v1/allocator.proto`:新增 `enum DSLifecyclePhase { UNSPECIFIED/ALLOCATED/RELEASED/ABANDONED }` + `message DSLifecycleEvent { match_id, ds_pod_name, phase, player_ids, map_id, game_mode, ts_ms }`
- `pkg/kafkax/topics.go`:新增 `TopicBattleResult = "pandora.battle.result"` / `TopicDSLifecycle = "pandora.ds.lifecycle"`
- go + cpp pb 已 regen(`pwsh tools/scripts/proto_gen.ps1` / `-Cpp`)

### errcode

- 复用已存在 `ERR_BATTLE_RESULT_DUPLICATE=6001` / `ERR_BATTLE_RESULT_DECODE=6002` / `ERR_BATTLE_RESULT_DB_WRITE=6003`(无新增)

### 验证

实际跑过的命令与结果(复审要求据实记录,不复述未跑的范围):

- **build(8 module)PASS** `BUILD=0`:
  ```pwsh
  go build ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/... ./services/runtime/player_locator/... ./services/matchmaking/team/... ./services/matchmaking/matchmaker/... ./services/battle/ds_allocator/... ./services/battle/battle_result/...
  ```
- **vet(仅本轮改动的 2 module)PASS** `VET=0`(未对全 8 module 跑 vet):
  ```pwsh
  go vet ./services/battle/battle_result/... ./services/battle/ds_allocator/...
  ```
- **test PASS** `TEST=0`:`go test ./services/battle/battle_result/...`(biz 8 用例)+ `go test ./services/battle/ds_allocator/...`(既有用例全绿)

- battle_result biz 8 用例:Elo 等分对称(+16/-16) / 平局对称(0/0) / 强队赢得少 + K 守恒、ReportResult 覆盖 DS 脏 mmr_delta + 幂等命中、**ReportResult 收到 ABANDONED 强制 delta 全 0**(风险入口加固)、HandleAbandoned outcome=ABANDONED + delta 全 0 + 幂等、ReportResult/HandleAbandoned 输入校验
- 新增 `deploy/mysql-init/03-battle-tables.sql`(`pandora_battle` 库已在 01 创建,仅建 2 张表)
- go.work 启用 `use ./services/battle/battle_result`
- 未做:8 module 全量 vet / `go test -race`(本机无 mingw gcc,-race 留 CI)/ 真 MySQL+Kafka 联调(环境步,交环境/人工)

### 已知阶段限制(W4 ③,复审澄清)

- **abandoned 补偿当前不是"必有补偿"的可靠闭环,只是 best-effort**(纠正上文遣词):
  - ds_allocator 发 `ds.lifecycle` 是**弱依赖**:Kafka publish 失败只 `Warn`,事件**会丢**;abandoned 镜像虽留在 ds_allocator 的 Redis(供查),但 battle_result 不会收到补偿事件
  - battle_result 的 player.update 也是弱依赖,同理 publish 失败静默丢
  - 因此不变量 §4「DS 崩溃必有补偿」在 W4 ③ 阶段**只在 Kafka 正常时成立**;broker 抖动 / 分区不可用时补偿可能缺失,无重试、无待补偿扫描、无死信
- **可靠补偿留后续**(任选其一,W4 ④+ 决策):
  - ds_allocator 发 lifecycle 失败时落本地"待补偿队列"(Redis ZSET),后台扫描重发(类似 sweep)
  - 或 battle_result 增一条对账路径:周期扫 ds_allocator 的 abandoned 镜像与本库 `battles` 差集补录
  - 或把 lifecycle / player.update 改成**强依赖 + 事务性 outbox**(落库与发事件原子)

## W4 ④ — player 服务上线 + battle_result 接真实 player MMRReader(2026-06-06)

Pandora 第 8 个 Kratos 业务服。闭合 MMR 写回(消费 player.update 幂等 UpdateMMR)
与读取(battle_result 经 gRPC 读真实当前 MMR)链路。

### 范围

- **player 新服**:gRPC :50002 / HTTP :51002(仅 /metrics,player.proto 无 google.api.http)
- **MySQL 强依赖**(`pandora_player` 库,无 Redis),新增 `deploy/mysql-init/04-player-tables.sql` 3 表:
  - `players`(PK player_id,uk nickname,level/mmr/avatar/total_battles/total_wins,idx mmr)
  - `player_heroes`(uk player_id+hero_id,英雄解锁池)
  - `mmr_history`(uk player_id+idempotency_key 幂等键 + idx player_id,created_at)
- **消费 `pandora.player.update`** → `UpdateMMR` 幂等(不变量 §2):
  - idempotency_key = match_id 字符串;`mmr_history` uk 命中即视为已处理,读回已记录 new_mmr,不重复改 players
  - 战绩计数:win → total_battles+1 / total_wins+1;lose / draw → total_battles+1;abandon / rollback → 不计
  - `ApplyMMRChange` 事务:`SELECT mmr FOR UPDATE` 锁行 → INSERT mmr_history(dup 即幂等) → UPDATE players;MMR clamp floor 0
- **6 RPC**:`GetProfile` / `UpdateNickname` / `ListHeroes` / `UnlockHero` / `GetMMR` / `UpdateMMR`
  - GetProfile / 写操作懒创建档案:`EnsureProfile` INSERT IGNORE 默认昵称 `Player_<player_id>`(保 uk_nickname 唯一)
  - `GetMMR` 未建档玩家返 base_mmr + OK(供 battle_result 当 reader,不为对手建行)
  - `UpdateNickname` 命中 uk → `ERR_PLAYER_NICKNAME_TAKEN`;`UnlockHero` 已拥有 → `ERR_PLAYER_HERO_ALREADY_OWN`
- **errcode**:复用既有 `ERR_PLAYER_NOT_FOUND=2001` / `ERR_PLAYER_NICKNAME_TAKEN=2003` / `ERR_PLAYER_HERO_ALREADY_OWN=2011`(无新增,无 proto regen)
- **battle_result 接真实 reader**:`StaticMMRReader` → `GrpcMMRReader`
  - 新增 `services/battle/battle_result/internal/data/mmr_reader.go`:经 `pkg/grpcclient.MustDialInsecure` 调 `player.GetMMR`
  - `battle.player_addr` 非空时启用,否则仍用 `StaticMMRReader` 兜底
  - 弱依赖:gRPC 懒连接(player 未起不阻塞启动),调用失败由 `biz.assignMMR` 回退 `BaseMMR`,不阻断落库
  - `battle_result-dev.yaml` `player_addr: "127.0.0.1:50002"`

### 验证

实际跑过的命令与结果:

- **build(9 module)PASS** `BUILD=0`:
  ```pwsh
  go build ./pkg/... ./proto/... ./services/account/login/... ./services/account/player/... ./services/runtime/push/... ./services/runtime/player_locator/... ./services/matchmaking/team/... ./services/matchmaking/matchmaker/... ./services/battle/ds_allocator/... ./services/battle/battle_result/...
  ```
- **vet(player)PASS** `VET=0`:`go vet ./services/account/player/...`
- **test PASS** `TEST=0`:
  - `go test ./services/account/player/...` biz 9 用例(delta 应用 / 幂等不双算 + 不双计场 / 缺 idempotency_key 拒 / floor clamp 到 0 / lose 计场不计胜 / abandon 不计场 / GetMMR 未建档返 base / UnlockHero 幂等返 AlreadyOwn / 昵称空与 player_id=0 校验 / battleFlags 表驱动)
  - `go test ./services/battle/battle_result/...` 回归 8 用例仍全绿(接入 GrpcMMRReader 不破坏既有 biz)
- go.work 加 `use ./services/account/player`
- **未做**:9 module 全量 vet / `go test -race`(本机无 mingw gcc,-race 留 CI)/ 真 MySQL + Kafka 联调(环境启停交环境/人工)

## W4 排期调整 — friend / chat 暂缓到最后(2026-06-06)

### 用户决策

用户明确确认:

- `chat`(:50005,社交;push 已订阅 `chat.private` / `chat.team` / `chat.world`;对 team / matchmaker 只有弱依赖模板)现在不做
- `friend`(:50004,社交好友关系;依赖 player/MySQL 模板)现在不做
- 两者最多放到最后:等 UE 客户端、Hub DS、Battle DS、Agones、核心玩法链路和其它必要系统都完成后再说

### 文档同步

- `docs/design/go-services.md`:服务总览把 `friend` / `chat` 标成"暂缓到最后";当前路线改为 hub_allocator → 可靠补偿 → UE grpc-web / DS / Agones → 核心闭环 → 其它必要系统 → friend/chat
- `docs/design/pandora-arch.md`:服务清单保留 friend/chat,但加排期说明;§11 决策行追加本决策
- `CLAUDE.md`:§7 决策行追加 friend/chat 后置
- `README.md`:入口文档里的 "13 个 go 服务" 改为当前 14 个服务口径
- `services/runtime/push/README.md`:chat/friend 相关 topic 标成后期模板占位,不作为当前实现任务

### 注意

不要删除 `friend.proto` / `chat.proto` / topic / 端口规划;它们是后期社交功能的占位。当前只改变实现顺序,不是取消服务。

---

## W4 ⑤ ✅ hub_allocator 服务上线(2026-06-06)

**目标**:落地大厅 DS 分片调度,完成"500 人/实例大厅"入口骨架(不变量 §1 一人一 hub)。
本服是 **hub 票据权威**(像 matchmaker 是 battle 票据权威):AssignHub / TransferHub 用
`pkg/auth.Signer.SignDSTicket(pid, DSTypeHub, 0, uuid)` 签 hub DSTicket(不变量 §3 5min,
secret 共享 login/envoy),因此 **JWT 是强依赖**(secret 缺失/非法直接 fatal,不同于 matchmaker 的条件签名)。


### 成果

**proto(已补,本轮复用 + regen)** —— `proto/pandora/hub/v1/allocator.proto`
- 新增 server-internal 存储快照(不外泄客户端,不变量 §14):
  `HubShardStorageRecord`(hub_pod_name/hub_addr/region/shard_id uint32/player_count int32/capacity int32/state/last_heartbeat_ms/created_at_ms)、
  `HubAssignmentStorageRecord`(player_id uint64/hub_pod_name/hub_addr/shard_id uint32/region/team_id uint64/assigned_at_ms)
- regen go + cpp(`[proto]` tag,cpp pb 须同步到 UE 仓库)
- 复用既有 errcode `ERR_HUB_NO_AVAILABLE=5101` / `ERR_HUB_TRANSFER_FAILED=5102`(无新增,无 errcode regen)

**hub_allocator 全骨架(第 9 个 Kratos 业务服)**
- `services/battle/hub_allocator/go.mod` —— module `github.com/luyuancpp/pandora/services/battle/hub_allocator`,replaces pkg/proto(go.sum 暂从 matchmaker 拷,`go mod tidy` 收尾)
- `etc/hub_allocator-dev.yaml` —— gRPC :50021 enable_reflection / HTTP :51021(仅 /metrics),redis `127.0.0.1:6380`,jwt 共享 login/envoy secret,hub 块(heartbeat_timeout 15s / sweep_interval 5s / shard_ttl 30m / assignment_ttl 30m / default_region global / default_capacity 500 / mock_shard_count 3 / mock_hub_addr_host 127.0.0.1 / mock_hub_port_base 7777)
- `internal/conf/conf.go` —— Config 嵌 config.Base + HubConf + JWTConf,全 duration 用 `config.Duration`,`Defaults()` 填默认 + 端口
- `internal/data/hub_repo.go` —— `HubRepo` interface + `RedisHubRepo`。key:`pandora:hub:shard:{<pod>}`=`HubShardStorageRecord` proto bytes(hashtag 锁 slot)/`pandora:hub:shards` SET/`pandora:hub:active` ZSET(score=last_heartbeat_ms,member=pod)/`pandora:hub:player:<id>`=`HubAssignmentStorageRecord`/`pandora:hub:team:<id>` string。`UpdateShardWithLock` WATCH/MULTI/EXEC 乐观锁(冲突耗尽 → `ErrHubNoAvailable`);`CreateShard` 写镜像 + SAdd **不进 active**(等首次心跳);`HeartbeatShard` 仅刷新已存在分片(孤儿返 found=false);`RangeStaleShards` Min `(0` 排除从未心跳的 Mock 种子(score=0)
- `internal/biz/fleet.go` —— `HubFleetProvider` interface + `MockHubFleetProvider`(pod=`pandora-hub-<region>-<i>`,addr=`host:base+i`,W4+ 接 Agones Fleet 只换实现 biz 零改)
- `internal/biz/hub.go` —— `TicketSigner` interface + `HubUsecase`。AssignHub(幂等:已分配且 ready → 重签票不重复占位 + 队友同分片 + 最空 ready 分片贪心,并列取 shard_id 小者 + lazy-seed)、ReleaseHub(自减 + 删归属,幂等)、TransferHub(先占新分片再退旧,失败不动旧;targetHubID!=0 点名 shard_id 否则最空非当前)、ListHubs(组装 HubInfo)、Heartbeat(刷新 / 孤儿返 stop)、RunHeartbeatSweep/sweepOnce(RangeStaleShards → 标 draining + 移出 active,不变量 §4)
- `internal/service/hub.go` —— 实现 `HubAllocatorServiceServer`,proto ↔ biz,`toProtoCode`
- `internal/server/grpc.go` / `http.go` —— gRPC 注册 + `pmw.AuthOptional()`;HTTP 仅 /metrics
- `cmd/hub_allocator/main.go` —— Redis 强依赖(Ping)+ JWT 强依赖(`auth.NewSigner` 失败 fatal)+ `hubTicketSigner` 适配 `biz.TicketSigner` → `SignDSTicket(pid, DSTypeHub, 0, uuid)`;装配 RedisHubRepo → MockHubFleetProvider → HubUsecase → HubService → gRPC/HTTP + `go uc.RunHeartbeatSweep(ctx)`

**测试**
- `internal/biz/hub_test.go` —— fakeRepo(内存)+ fakeSigner + Mock fleet,14 用例(lazy-seed 最空 / 幂等不双占 / 分散 / 容量满 / 队友同分片 / release 自减幂等 / transfer 跨分片 / 未入 hub 拒 / 心跳孤儿 stop / 已知不下指令 / 扫描标 draining / 扫描跳过从未心跳 / 输入校验),全绿
- `internal/data/hub_repo_test.go` —— miniredis,9 用例(分片往返 / 列举 / 乐观锁 / 心跳已知与孤儿 / stale 排除 score0 / 移除 / 归属往返 / 队伍往返),全绿

**go.work + 文档**
- `go.work` 加 `use ./services/battle/hub_allocator`(验证升 10 module)
- `CLAUDE.md §4.1` 验证命令追加 `./services/battle/hub_allocator/...`,§7 加 W4 ⑤ 决策行
- `docs/design/go-services.md` hub_allocator 状态 → ✅ W4 ⑤,路线图更新

### 验证

- `go build`(10 module 全量,见 CLAUDE.md §4.1)exit=0
- `go vet ./services/battle/hub_allocator/...` exit=0
- `go test ./services/battle/hub_allocator/...` exit=0(biz 14 + data 9 全绿;本机无 mingw gcc,`-race` 留 CI)

## W4 ⑥ ✅ login 接 hub_allocator.AssignHub 替换 mock hub_addr(2026-06-06)

打通玩家流转图(pandora-arch.md §5)step 3-5「登录 → 调 hub_allocator 分配 hub → 返回 hub_ds_addr + 票据」第一段。
W4 ⑤ hub_allocator 仅骨架不接 login;本轮把 login 真正接上,login 不再自签 hub 票据,改用 hub_allocator 这个 hub 票据权威返回的地址 + 票据。

### 改了什么

**新增 data 层弱依赖客户端**(复刻 W3 ⑤ `locator_client.go` 模式):
- `services/account/login/internal/data/hub_client.go`:
  - 接口 `HubAssigner`:`AssignHub(ctx, playerID, region, teamID) (*HubAssignment, error)`
  - 实现 `GrpcHubAssigner`:内嵌 `*grpc.ClientConn` + `hubv1.HubAllocatorServiceClient`,把 `AssignHubResponse` 收敛成 client 视角最小结构 `HubAssignment{HubDSAddr, HubTicket, HubPodName, ShardID}`(不外泄存储快照,符合不变量 §14)

## 社交域 ① ✅ friend 服务上线(2026-06-15)

补齐 `services/social/friend/`(此前仅 `.gitkeep`)。friend 此前在 `go-services.md` 标「🧊 暂缓到最后」,本轮按用户「补全 friend 模块」要求实现完整 Kratos 服务,模式对齐 player(MySQL 强依赖)+ team(R5 JWT + snowflake)+ battle_result(kafka producer)。第 11 个 Kratos 业务服。

### 改了什么

**proto**(`proto/pandora/friend/v1/friend.proto`,`[proto]`):
- `request_id` `string`→`uint64`(不变量 §9.11 snowflake 业务 ID;friend 未上线消费,安全)
- 新增 `FriendEventReason` 枚举(UNSPECIFIED/REQUEST_RECEIVED/REQUEST_ACCEPTED)+ `FriendEvent` message(by_player_id/to_player_id/request_id/reason/ts_ms),给 kafka `pandora.friend.event` 推送用
- 各 Request 补 R5 注释;`FriendInfo` 补客户端可见结构注释
- 已 `proto_gen.ps1` 重生(buf lint OK,go pb 33 files);无新 errcode(9101/9102/9103 已存在,免重生 errcode)

**库表**(`deploy/mysql-init/06-social-tables.sql`,pandora_social 库已在 01 建库):
- `friendships`(双向边,每对落两行,便于 ListFriends)/ `friend_requests`(PK request_id snowflake,uk requester+target,status 1234 对齐 proto 枚举)/ `blocks`(uk player+blocked)

**kafka push 接线**(`pkg/kafkax/topics.go` + push etc):
- `TopicFriendEvent` 加进 `PushTopics` 默认订阅切片;push-dev.yaml / push-prod.yaml.example topics 显式补 `pandora.friend.event`

**friend 服务**(`services/social/friend/`,gRPC :50004 / HTTP :51004):
- `go.mod`(module `.../services/social/friend`,replace ../../../pkg + ../../../proto;go.sum 暂从 battle_result 拷)
- `internal/conf/conf.go` —— Config 嵌 config.Base + `FriendConf{MaxFriends, LocatorAddr}` + Defaults(端口 50004/51004)
- `internal/data/friend_repo.go` —— `FriendRepo` interface + `MySQLFriendRepo`(AreFriends / IsBlocked 双向 / CreateRequest 事务复用-重置 pending / GetRequest / AcceptRequest 事务标 accepted+写双向边 / ListFriends / Block 事务拉黑+删边+取消 pending)
- `internal/data/locator_client.go` —— `OnlineStatusReader` interface + `GrpcOnlineStatusReader`(逐个 GetLocation 填在线;查不到/失败按离线,弱依赖)
- `internal/biz/friend.go` —— `FriendEventPusher` interface + `FriendUsecase`。AddFriend(非自身 / 未互拉黑 / 非已好友 → 建请求 → 推 REQUEST_RECEIVED 给 target)、AcceptFriend(仅 target 本人 + pending → 建好友 → 推 REQUEST_ACCEPTED 给 requester)、ListFriends(组装 FriendInfo + locator 在线;nickname 留空由客户端解析,§5.8)、Block。推送原则 2:都不发给操作者自己
- `internal/service/friend.go` —— 实现 `FriendServiceServer`,`callerID(ctx)` R5 覆盖 player_id,snowflake 生成 request_id,`toProtoCode`
- `internal/server/grpc.go`(`pmw.AuthOptional()` + 注册)/ `http.go`(仅 /metrics)
- `cmd/friend/main.go` —— MySQL 强依赖(Ping + maskDSN)+ snowflake + kafka producer 弱依赖(friendEventPusher key=to_player_id)+ locator gRPC 弱依赖,装配 → Kratos Run
- `etc/friend-dev.yaml` / `etc/friend-prod.yaml.example`(MySQL pandora_social / kafka pandora-friend / locator 50006)

**测试**:
- `internal/biz/friend_test.go` —— fakeRepo + fakePusher + fakeOnline(无 DB/kafka/locator),12 用例(AddFriend OK 推送路由 / 自加 / 拉黑 / 已好友 / 复用 pending;AcceptFriend OK 推送 / 非 target / 无请求;ListFriends 填在线 / nil reader 离线;Block 删边+取消+拉黑后拒加 / 自拉黑),全绿

**go.work + 文档**:
- `go.work` 加 `use ./services/social/friend`(升 11 module),底部注释参考表同步
- `docs/design/go-services.md` friend 状态 🧊→✅

### 验证

- `go build ./services/social/friend/...` exit=0
- `go vet ./services/social/friend/...` exit=0
- `go test ./services/social/friend/...` exit=0(biz 12 用例全绿)
- `go build ./pkg/... ./services/runtime/push/...` exit=0(kafkax topics 改动不破坏 push)

### 设计要点

- **职责切分**:hub_allocator 是 hub 票据权威(像 matchmaker 是 battle 票据权威);login 接上后不再自签 hub 票据,自签仅作为 allocator 不可用时的弱依赖回退兜底
- **弱依赖一致性**:与 locator 同模式 —— addr 未配 → nil → 跳过;拨号失败 → panic(启动期);运行期调用失败 → Warn + 回退,不阻断登录
- **无新服 / 无新 proto / 无 errcode regen**:纯 wiring + 复用既有 `hub.v1.AssignHub` RPC 和既有 JWT 工具

### 验证

- 10 module BUILD=0(`go build ./pkg/... ./proto/... ./services/...` 全 10 个已启用 module)
- login VET=0 / TEST=0:`go vet ./services/account/login/...` + `go test ./services/account/login/...`
- biz 新增 3 单测(`login_test.go`):
  - `TestLogin_HubAssignerSuccess`:AssignHub 成功 → 用 allocator 的 addr + 票据,exp 从票据解析 >0,AssignHub 入参 (pid=42, region="cn", team=0) 正确
  - `TestLogin_HubAssignerNil_FallbackSelfSign`:nil → 回退静态 addr + 自签票据,verifier 验通过且 ds_type=hub
  - `TestLogin_HubAssignerError_FallbackSelfSign`:AssignHub 报错 → 回退,登录不报错

## W4 ⑦ ✅ matchmaker 接 player_locator 串联 MATCHING/BATTLE 状态机(2026-06-06)

打通玩家流转图(pandora-arch.md §5)撮合段的「位置一致性」:玩家进撮合 → locator 标 MATCHING,
进战斗 → locator 标 BATTLE。落实不变量 §1「玩家同一时刻只能在一个 Location」在撮合生命周期内的状态流转。
无新服 / 无新 proto / 无 errcode regen,纯 wiring。

### 状态权属(关键设计)

- **matchmaker 是 MATCHING / BATTLE 两态的权威**(它掌握撮合生命周期:成局 / 确认 / 拉 DS / 就绪)
- **HUB 状态由 hub DS 上报**(W4 ⑥ 起 login 只写 LOGIN_PENDING,hub DS 接入后改 HUB)
- 撮合失败 / 取消时 matchmaker **不回写 HUB**(玩家物理上仍在 hub DS,交回 hub DS 重新上报),
  避免 matchmaker 越权写它不掌握 hub_pod 的 HUB 状态

### 状态触发点

| 触发 | locator 状态 | 必填字段(locator 校验) | matchmaker 调用点 |
|---|---|---|---|
| 撮合成局(进确认期) | MATCHING | match_id | `formMatch` CreateMatch 成功后 |
| 全员确认 + DS 就绪 | BATTLE | match_id + battle_pod | `onAllConfirmed` 写 READY 后 |

> 注:locator `SetLocation` 校验 MATCHING 需 match_id、BATTLE 需 match_id + battle_pod。
> 排队中(StartMatch 仅有 ticket_id 无 match_id)**不写 MATCHING** —— 玩家此时仍在 hub 走动,
> 语义上属 HUB;只有真正成局拿到 match_id 才进 MATCHING,正好满足校验,不是妥协。
> BATTLE 的 `battle_pod` 用 ds_addr 唯一标识 DS(AllocateBattle 只返 ds_addr,不返 pod_name)。

### 改了什么

**biz 层新增弱依赖接口**(`internal/biz/match.go`):
- `LocationNotifier`:`NotifyMatching(ctx, playerIDs, matchID)` / `NotifyBattle(ctx, playerIDs, matchID, battlePod)`
- `MatchUsecase` 加 `locator LocationNotifier` 字段(nil-able)
- `notifyMatching` / `notifyBattle` helper:nil 跳过 / 调用失败仅 Warn 不阻断撮合
- 调用点:`formMatch` 在 CreateMatch 成功后调 `notifyMatching(成员, matchID)`;
  `onAllConfirmed` 在写 READY 成功后调 `notifyBattle(成员, matchID, ds_addr)`

**data 层新增 gRPC 客户端**(`internal/data/locator_client.go`,复刻 login `locator_client.go` 模式):
- `GrpcLocationNotifier`:内嵌 `*grpc.ClientConn` + `PlayerLocatorServiceClient`
- `NotifyMatching` / `NotifyBattle` 逐玩家 best-effort `SetLocation`,单个失败继续其余、返首个错误供 biz 记 Warn

**装配 wiring**:
- `conf.go`:`MatchConf` 加 `LocatorAddr`(留空则不上报)
- `main.go`:加 locator notifier 装配(locator_addr 空 → 跳过 + Warn;非空 → 拨号 + defer Close);
  导入 `pkg/grpcclient`;`NewMatchUsecase` 签名加 `locator`(在 cfg 前)
- `matchmaker-dev.yaml`:加 `match.locator_addr: 127.0.0.1:50006`
- `NewMatchUsecase` 3 调用点同步(main 1 + faulty-repo 测试 2 传 nil)

### 设计要点

- **弱依赖一致性**:与 login locator / hub_assigner 同模式 —— addr 未配 → nil → 跳过;
  拨号失败 → panic(启动期 `MustDialInsecure`);运行期调用失败 → Warn,不阻断撮合
- **无新服 / 无新 proto / 无 errcode regen**:复用既有 `locator.v1.SetLocation` RPC + `LocationState` 枚举

### 验证

- 10 module BUILD=0(`go build ./pkg/... ./proto/... ./services/...` 全 10 个已启用 module)
- matchmaker VET=0 / TEST=0:`go vet ./services/matchmaking/matchmaker/...` + `go test ./services/matchmaking/matchmaker/...`
- biz 新增 `mockLocator`(记录每玩家 MATCHING match_id / BATTLE pod)+ 1 单测 `TestLocatorState_MatchingThenBattle`:
  - 成局后全员被标 MATCHING(match_id=999)且**未误标 BATTLE**
  - 全员确认就绪后全员被标 BATTLE(pod = match.BattleDsAddr)
  - fixture 默认挂 mockLocator,既有撮合流水线测试一并无害复跑

## W4 ⑧ ✅ ds_allocator abandoned 补偿可靠化(不变量 §4 闭环)(2026-06-06)

把 W4 ③ 遗留的「abandoned 补偿是 best-effort 弱依赖」升级为 **at-least-once 可靠闭环**,
让不变量 §4「DS 崩溃必有补偿(15s 心跳超时 → abandoned → 段位回滚)」不再只在 Kafka 正常时成立。
无新服 / 无新 proto / 无新 errcode / 无新 Redis key / 无新配置,纯 biz 改 `sweepOnce`。

### 解决的问题

W4 ③ 心跳超时标记 abandoned 后,`publishAbandoned` 直接发 `pandora.ds.lifecycle` 事件给
battle_result 做段位回滚补偿。这是 best-effort 弱依赖:Kafka 不可用时 publish 失败仅 Warn,
**事件直接丢**,玩家段位不会回滚,违反不变量 §4。W4 ③ 复审已据实把这条软化为
「仅在 broker 正常时成立,无重试 / 无待补偿扫描 / 无 outbox」。本轮补上可靠性。

### 设计:用 `active` ZSET 自身当 outbox

不引入独立 outbox 表 / 新 Redis key,而是复用已有的心跳扫描基础设施:

- abandoned 的对局在 `ds.lifecycle` 事件**成功投递前不移出 `active` ZSET**(其 score=旧
  last_heartbeat_ms 仍 ≤ 超时阈值),故下一轮 `sweepOnce` 会再次命中并**重试投递**。
- 投递成功(或未配置 kafka 的 best-effort 回退)才 `ExpireBattle` 移出 active、不再扫描。
- 配合 battle_result 幂等消费(不变量 §2,`HandleAbandoned` 同 match_id 只写一次),整条补偿链
  是 **at-least-once 闭环**,可穿越 Kafka 临时不可用(broker 恢复后下一轮 sweep 自动补发)。
- **天然上界**:battle 镜像 TTL(`BattleTTL` 2h)过期后 `GetBattle` miss → lock fn 返
  `ErrDSPodNotFound` → `RemoveActive` 清理残留 active,不会无限堆积。

### 改了什么(`internal/biz/allocator.go`)

- `sweepOnce` 重构:
  - lock fn 仅 `state==ended` 才 skip(正常结算);否则置 abandoned,并捕获 `wasAbandoned`
    (本轮之前是否已 abandoned)。
  - **仅首次转入 abandoned(`!wasAbandoned`)才 `Release` pod**:补偿重试期间不对同一 pod
    重复回收(对接真 Agones 时友好,避免对已释放 GameServer 重复 Release 报错)。
  - 调 `deliverAbandoned` 投递;返 true 才 `ExpireBattle` 移出 active,返 false 保留重试。
- `publishAbandoned`(void)→ `deliverAbandoned`(返 `bool`):
  - `lifecycle == nil`(kafka 未配置)→ 返 true,best-effort 回退直接移出 active(显式选择
    「无补偿通道」,不把对局永久卡在 active 每轮空转回收)。
  - publish 失败 → 返 false + Warn `ds_lifecycle_publish_failed_will_retry`,保留 active 重试。
  - publish 成功 → 返 true + Info `ds_lifecycle_published`。
- `DSLifecyclePusher` 接口文档:语义从「失败静默丢」改为「失败触发下一轮 sweep 重试」。

### 验证

- 10 module BUILD=0
- ds_allocator VET=0 / TEST=0(biz 7→9 用例)
- 新增 2 单测:
  - `TestSweepDeliversAbandonedFirstTry`:配 kafka 且首投成功 → 发 1 次事件、移出 active、回收 1 次。
  - `TestSweepReliableCompensation_RetryUntilDelivered`:前 2 轮投递失败保留 active、第 3 轮成功
    才移出;pod 仅在首次转 abandoned 回收 1 次、publish 共 3 次、delivered=[7]、终态镜像仍可查。
- 既有 `TestSweepMarksAbandoned`(nil lifecycle)经 best-effort 回退仍绿。

### W4 ⑧ 复审修正(2026-06-06,提交前)

复审捕获 W4 ⑧ 关键 bug,**提交前已修正**。

**问题**:`sweepOnce` 的 abandoned 标记 / 投递失败重试路径都走 `repo.UpdateBattleWithLock(...,
u.battleTTL())`,而 `RedisBattleRepo.UpdateBattleWithLock` 内部 `pipe.Set(key, payload, battleTTL)`
**每轮都把 battle key TTL 刷回 2h**。因此 W4 ⑧ 文档/注释写的「BattleTTL 是天然上界 / 镜像最终过期 →
GetBattle miss → 清理 active / 不无限堆积」**不成立**:Kafka 长期不可用时该 abandoned match 会被每轮
sweep 无限刷新 TTL、无限留在 active、无限重试。原新增的 `TestSweepReliableCompensation_RetryUntilDelivered`
只覆盖「前 2 次失败第 3 次成功」,没验证持续失败时 TTL 是否保持原始过期时间,所以没抓到。

**修正(选方案 1:保留「TTL 是上界」设计)**:

- data 层新增 `UpdateBattleKeepTTL(ctx, matchID, maxRetry, fn)`:与 `UpdateBattleWithLock` 共享新
  抽出的私有 `updateWithLock(..., expiration time.Duration)`,区别仅 `pipe.Set` 的 expiration——
  `UpdateBattleWithLock` 传 `battleTTL`(心跳/正常更新刷新 TTL 续命),`UpdateBattleKeepTTL` 传
  `redis.KeepTTL`(-1,保留原 TTL 不刷新)。`BattleRepo` 接口同步加该方法。
- biz `sweepOnce` 的 abandoned 标记 + 重试改走 `UpdateBattleKeepTTL`。故镜像 TTL 从**最后一次心跳**
  起算的 BattleTTL(2h)后过期,sweep 重试**不再延长 TTL**;Kafka 长期不可用时镜像最终过期 →
  `GetBattle` miss → lock fn 返 `ErrDSPodNotFound` → `RemoveActive` 清理残留 active。BattleTTL
  现在是补偿重试的**真实**天然上界。
- 新增单测 `TestSweepReliableCompensation_KeepsTTLOnFailure`:`mockLifecycle{failFirst: 1000}`
  始终投递失败;用 miniredis `mr.SetTTL(key, 90s)` 把 TTL 钉到已知小值,连续 3 轮 sweep 后断言
  `mr.TTL(key)` 仍 ≤ 90s(未被刷新回 2h),且对局仍在 active 重试、状态 abandoned、pod 只回收 1 次。
  若误用刷新 TTL 的路径,TTL 会回弹到 2h > 90s → 测试失败,真正守住该不变量。

**保留的既有测试**(复审要求):投递失败 active 保留 + 成功后 active 移除
(`TestSweepReliableCompensation_RetryUntilDelivered`)、已 abandoned 重试不重复 Release pod
(同测 + 新 TTL 测 `alloc.releases == 1`)。

**验证**:10 module BUILD=0;ds_allocator VET=0 / TEST=0(biz 9→10 用例,新增 TTL 测;既有
`TestSweepMarksAbandoned` nil-lifecycle 经 best-effort 回退仍绿)。

**教训**:用 outbox 语义复用既有「带 TTL 刷新」的写路径时,务必确认重试不会顺带刷新 TTL/score 把
「TTL 当上界」的前提冲掉;写「天然上界 / 不无限堆积」这类绝对保证前,必须有一条**持续失败**的测试
直接验证 TTL/堆积不增长,而不是只测「失败几次后成功」。


## W4 ⑨ ✅ battle_result player.update 事务出箱可靠化(不变量 §4 第二段闭环)(2026-06-06)

把 W4 ③ 遗留的「battle_result 落库后发 player.update 是 best-effort 弱依赖」升级为
**at-least-once 可靠闭环**,补上 HANDOFF §3 Step 2「可靠补偿收口」的最后一段。
W4 ⑧ 已让 `ds.lifecycle`(ds_allocator → battle_result)可靠;本轮让 `player.update`
(battle_result → player 段位写)可靠。新增 1 张 MySQL 表 + 2 个出箱配置,无新服 / 无新 proto /
无新 errcode。

### 解决的问题

W4 ③ `ReportResult` / `HandleAbandoned` 落库成功后调 `pushOne` 直接发 `pandora.player.update`,
这是 best-effort:Kafka 不可用时 publish 失败仅 Warn,**事件直接丢** → 玩家段位永不更新。
不变量 §4「DS 崩溃必有补偿(15s 心跳超时 → abandoned → 段位回滚)」的补偿链末段断裂——
即使 abandoned 事件可靠送达 battle_result(W4 ⑧),battle_result 再写给 player 的段位变更仍会丢。

### 设计:事务出箱(transactional outbox)

battle_result 是 MySQL-only 服务(无 Redis),不能复刻 W4 ⑧ 的「Redis ZSET 当 outbox」。
改用经典事务出箱:

- 新增表 `pandora_battle.player_update_outbox`(PK `id` 自增,`uk_match_player` 防重入,
  `payload` = `player.v1.PlayerUpdateEvent` proto bytes,`created_at_ms`)。
- `SaveResult` 在落 `battles` + `battle_player_stats` 的**同一事务**里再写出箱行,三者原子提交
  (不变量 §4:落库与待发布段位事件不会半成功)。幂等命中(dup match_id)→ 出箱也不写。
- 后台 `RunOutboxPublisher`(`OutboxPublishInterval` 默认 2s)按 `id` FIFO 取 `OutboxBatchSize`
  (默认 128)条 → 逐条投递 Kafka(key=player_id,不变量 §9 同玩家保序)→ 投递成功才
  `DeleteOutbox` 删行;投递失败立即中断本批、保留出箱行下一轮重试(保证同玩家事件按 id 顺序投递)。
- 配合 player 服务幂等消费(W4 ④ `mmr_history` uk 幂等键),整条段位写链是 **at-least-once
  可靠闭环**,可穿越 Kafka 临时不可用(broker 恢复后下一轮 publisher 自动补发)。
- **天然不堆积**:出箱表只存待发布事件,投递成功即 DELETE。DELETE-on-publish 若在「投递成功但
  删行失败」窗口崩溃 → 重发,player 幂等消费吸收重复,符合 at-least-once。

### 改了什么

- `deploy/mysql-init/05-battle-outbox.sql`:新增 `player_update_outbox` 表。
- `internal/data/battle_repo.go`:加 `OutboxRecord` 类型;`SaveResult` 签名加 `outbox []OutboxRecord`
  参数并在事务内写出箱;新增 `FetchOutbox`(id 升序取批)/ `DeleteOutbox`(删已投递行);`BattleRepo`
  接口同步。
- `internal/biz/battle_result.go`:`ReportResult` / `HandleAbandoned` 改为 MMR 算完先 `buildOutbox`
  (NORMAL → win/lose/draw;ABANDONED → delta 0 + reason "abandon")再传给 `SaveResult` 入事务;
  删除原 `pushPlayerUpdates` / `pushOne` 直推路径。新增 `RunOutboxPublisher`(后台循环)/
  `publishOutboxBatch`(取批投递,失败中断保留重试,返成功条数)/ `outboxBatchSize`。
  `PlayerUpdatePusher` 接口语义从「失败静默丢」改为「失败触发下一轮重试」;pusher nil(producer
  未配)时出箱积压不丢、等 producer 可用恢复。
- `internal/conf/conf.go`:加 `OutboxPublishInterval`(config.Duration 2s)/ `OutboxBatchSize`(128)
  + Defaults。
- `cmd/battle_result/main.go`:`go uc.RunOutboxPublisher(pubCtx)`(随进程生命周期启停);producer
  init 失败注释改为「出箱积压不丢」语义;`service_ready` 日志加 `outbox_interval`。
- `etc/battle_result-dev.yaml`:加 `battle.outbox_publish_interval` / `outbox_batch_size`,头注释
  把 player.update 从「producer 弱依赖」改为「事务出箱可靠补偿」。

### 验证

- 10 module BUILD=0
- battle_result VET=0 / TEST=0(biz 7→11 用例)
- 新增 4 单测:
  - `TestOutboxWrittenAtomicallyOnSave`:落库即入箱 4 条、publisher 未跑前 0 推送。
  - `TestOutboxReliablePublish_RetryUntilDelivered`:前 2 轮投递失败出箱保留 4 条、第 3 轮
    Kafka 恢复全投递清空、第 4 轮空批无副作用。
  - `TestOutboxPublishMidBatchFailureKeepsOrder`:一批第 3 条失败 → 前 2 条删、剩 player 3/4
    按 id 顺序保留下轮续传。
  - `TestOutboxNilPusherNoLoss`:pusher nil 时 0 投递且出箱 4 条不丢。
  - 既有 `TestReportResultAssignsMMRAndIdempotent` / `TestHandleAbandonedZeroDeltaIdempotent`
    改为 `publishOutboxBatch` 驱动后断言推送。

## W4 ⑩ player_locator 状态机守卫(2026-06-06)

补 HANDOFF §3 Step 2「可靠补偿收口」之后的不变量 §1 收口:把 player_locator 的
**覆盖式写**升级为带状态机守卫的**原子读-判-写**,落实 CLAUDE.md §9.1
「玩家在线只能在一个 DS」。

W3 ⑤ 遗留:`SetLocation` 直接 DEL+HSET 覆盖(无读、last-writer-wins),`biz` 注释
自留 TODO「W4+ 接 DS 注册表后加 Conflict 检测」。本轮兑现该 TODO。

无新服 / 无新 proto / 无新 errcode:`ERR_LOCATOR_CONFLICT=9202` 在 Go errcode 和
proto 两端 W1 早已就绪,本轮才**首次使用**。纯 data + biz 改动。

### 核心设计:用 state 本身识别写入方权威

locator 的写入方按 state 天然分两类,**无需在 proto 加 reporter/fence 字段**:

| 写入方 | 写的 state | 可信度 |
|---|---|---|
| login(控制面) | `LOGIN_PENDING` | 可信,顶号 |
| matchmaker(控制面) | `MATCHING` / `BATTLE` | 可信,撮合生命周期权威 |
| hub DS(数据面,UE 未建) | `HUB` | **可能 stale**,需守卫 |

### 守卫规则(`guardTransition`)

- 控制面写(`LOGIN_PENDING` / `MATCHING` / `BATTLE`)→ **一律放行**(顶号语义)。
- `HUB` 上报(唯一来自数据面)→ **当前状态为 `MATCHING` 时拒绝 `ErrLocatorConflict`**。
  - 玩家在撮合确认期(~15s)物理上仍连着 hub DS,hub DS 会持续上报 `HUB`;
    若放行会把 matchmaker 刚写的 `MATCHING` 顶回 `HUB`,使其他服务误判玩家仍在大厅闲逛。
- `BATTLE → HUB`(战斗结束返回大厅)是合法回流 → **放行**。

### 原子性:WATCH/MULTI/EXEC

`RedisLocationRepo.Set` → `SetGuarded(ctx, playerID, rec, ttl, maxRetry, guard)`:
对齐 team / matchmaker / ds_allocator / hub_allocator 的乐观锁惯例。

```
for attempt := 0..maxRetry:
  WATCH key
    cur = readLocation(key)        # HGETALL + parseLocationMap(复用 Get 的解析)
    if guard(cur, found) != nil: 中止,原样返回守卫错误(不重试)
    MULTI: DEL + HSET(覆盖) + EXPIRE(刷新 TTL)
  EXEC
  TxFailedErr → CAS 冲突重试;耗尽 → ErrLocatorConflict
```

读-判-写在 WATCH 内原子完成,堵住「hub DS 读到 pre-MATCHING 旧值 → matchmaker 写
MATCHING → hub DS 覆盖回 HUB」的竞态(EXEC 会因 key 变更失败 → 重试 → 重读见
MATCHING → 拒绝)。`optimisticRetry=3` 用 biz 包常量,`NewLocatorUsecase` 签名不变,
不动 conf / main / 既有测试调用点。

### 对现有调用方零影响

login 只写 `LOGIN_PENDING`、matchmaker 只写 `MATCHING` / `BATTLE`,都走放行分支,
不触发守卫;`HUB` 上报当前**无人发送**(hub DS 是 UE,未建)。本轮是把接收契约
提前就位,等 UE hub DS 落地即生效。

### 阶段限制(据实,不用绝对词)

stale hub DS 顶掉 active `BATTLE` 的极端场景(玩家已进战斗 DS,旧 hub DS 误报 HUB)
本轮**不处理**:`BATTLE → HUB` 与「stale hub 顶 BATTLE」仅凭 state 无法区分,需要
fence / 已结束 match_id 令牌,留待 UE hub DS 落地后做。当前 `BATTLE` 期间真 hub DS
已不再持有该玩家、正常不会上报,故风险窗口很小。

### 改动文件

- `services/runtime/player_locator/internal/data/location.go`:`Set` → `SetGuarded`
  (WATCH/MULTI/EXEC),抽出 `readLocation` / `parseLocationMap` 供 Get 与 SetGuarded 复用。
- `services/runtime/player_locator/internal/biz/locator.go`:`SetLocation` 走
  `SetGuarded(...,guardTransition(in))`,新增 `guardTransition` 守卫 + `optimisticRetry` 常量。
- `services/runtime/player_locator/internal/biz/locator_test.go`:stub `Set`→`SetGuarded`
  (执行 guard),新增 3 组守卫单测。

### 验证(2026-06-06)

- 10-module BUILD=0
- player_locator VET=0 / TEST=0(biz 7→10 用例:HUB-during-MATCHING 被拒且 MATCHING
  不被顶 + 控制面写恒胜 + HUB 从 OFFLINE/LOGIN_PENDING/HUB/BATTLE 放行)

## W4 ⑪ ✅ player_locator BATTLE fence(2026-06-06)

补 W4 ⑩ 阶段限制:防 stale hub DS 把 active `BATTLE` 顶回 `HUB`。

### 背景

W4 ⑩ 已用 `WATCH/MULTI/EXEC + guardTransition` 挡住 `MATCHING` 被 stale `HUB`
覆盖,但当玩家处于 `BATTLE` 时,仅凭 state 无法区分:

- 合法回流:玩家战斗结束,重新进入 hub DS,hub DS 上报 `HUB`;
- stale 覆盖:旧 hub DS 不知道玩家已进 battle DS,误报 `HUB`。

### 设计

不改 proto,复用 `Location.match_id` 作为 `HUB` 回流 fence 令牌:

- hub DS 在玩家从 battle 返回大厅时,从 battle DSTicket 取刚结束战斗的 `match_id`,
  上报 `HUB` 时一并带上。
- locator 当前为 `BATTLE` 时,仅当 `in.match_id == cur.match_id && in.match_id != 0`
  才允许 `BATTLE → HUB`。
- `match_id=0` 或不匹配时拒 `ERR_LOCATOR_CONFLICT=9202`,避免 stale hub 顶掉 active battle。
- `HUB` 报文里的 `match_id` 只作 fence,写入前清零,不持久化到 HUB 记录。

### 改动文件

- `services/runtime/player_locator/internal/biz/locator.go`:HUB 上报进入 `BATTLE` 守卫;
  `HUB` 记录持久化前清零 `match_id/battle_pod`。
- `services/runtime/player_locator/internal/biz/locator_test.go`:新增 3 个 fence 单测。
- `services/runtime/player_locator/README.md`:记录 hub DS 上报契约。
- `docs/design/go-services.md` / `CLAUDE.md`:补服务契约和决策行。

### 验证

- 10 module BUILD=0。
- player_locator VET=0 / TEST=0。


## W4 ⑫ ✅ ds_allocator 真 Agones GameServerAllocation allocator(2026-06-08)

把 W4 ② 的 `MockGameServerAllocator` 升级为可配置的真 Agones 分配器实现,
但保留 Mock 作为本地无 k8s / 无 Agones 时的 fallback。无新 proto / 无新 errcode /
无新第三方依赖。

### 设计

- 新增 `AgonesGameServerAllocator`,用标准库 `net/http` + `crypto/tls` + `encoding/json`
  直连 k8s apiserver REST,不引入 agones SDK / client-go 重依赖。
- `Allocate`:
  - `POST /apis/allocation.agones.dev/v1/namespaces/{ns}/gameserverallocations`;
  - selector:`agones.dev/fleet=<fleet_name>`;
  - metadata labels:`pandora.dev/match-id` / `map-id` / `game-mode`;
  - `status.state=="Allocated"` 时返回 `gameServerName` + `address:first_port`;
  - 非 Allocated 返 `ERR_DS_NO_AVAILABLE=5001`,HTTP / decode / status 不完整返
    `ERR_DS_ALLOCATION_FAILED=5002`。
- `Release`:
  - `DELETE /apis/agones.dev/v1/namespaces/{ns}/gameservers/{pod}`;
  - 404 视作已释放,保持幂等。
- 配置门控:
  - `agones.enabled=false`(dev 默认)→ 继续 Mock;
  - `agones.enabled=true` → 真 Agones REST allocator;
  - in-cluster 默认 `https://kubernetes.default.svc` + ServiceAccount token/CA。

### 复审补强

- 补 `sanitizeLabelValue` 首尾规则:k8s label value 必须首尾字母数字,中间允许 `-_.`;
  全非法 / 空值回 `unknown`,避免 future `game_mode` 导致 apiserver 422。
- 新增 helper 单测覆盖正常值、非法字符、全非法、63 字符截断。

### 改动文件

- `services/battle/ds_allocator/internal/data/agones_allocator.go`:真 Agones REST allocator。
- `services/battle/ds_allocator/internal/data/agones_allocator_test.go`:httptest apiserver 单测。
- `services/battle/ds_allocator/internal/conf/conf.go`:新增 `AgonesConf` + defaults。
- `services/battle/ds_allocator/cmd/ds_allocator/main.go`:按 `agones.enabled` 选 Agones / Mock。
- `services/battle/ds_allocator/etc/ds_allocator-dev.yaml`:新增 agones 配置段(dev 默认 disabled)。
- `services/battle/ds_allocator/internal/biz/gameserver.go`:更新陈旧注释。
- `CLAUDE.md` / `docs/design/go-services.md`:追加服务级决策与契约记录。

### 验证

- 10 module BUILD=0。
- ds_allocator VET=0 / TEST=0。

## W4 ⑬ ✅ 本地 Redis 镜像升级到 Redis 8.8.0 Alpine(2026-06-08)

按用户要求把开发期 docker-compose 的 Redis 从 7.4 线升级到当前 Redis 8 Alpine
小版本固定镜像。

### 决策

- 使用 `redis:8.8.0-alpine`。
- 不使用 `latest` / `8-alpine`,避免后续 Docker Hub tag 漂移导致开发机、CI、
  其他协作者拉到不同 Redis 小版本。
- `CLAUDE.md` 当前基础设施口径同步从 Redis 7 更新为 Redis 8。

### 改动文件

- `deploy/docker-compose.dev.yml`:Redis 镜像改为 `redis:8.8.0-alpine`。
- `CLAUDE.md`:基础设施版本和决策行同步。

### 验证

- `docker compose -f deploy/docker-compose.dev.yml --env-file deploy/env/dev.env config --quiet`
  通过。


## W4 ⑭ ✅ 真 Agones + Kafka + MySQL 两段补偿链验证跑通(2026-06-09)

把 W4 ⑧ / W4 ⑨ 的可靠补偿设计从单测与 stub 级别推进到本地真实基础设施联调:
Agones 分配链路、Kafka 事件链路、MySQL battle/player 落库链路都已跑通。本轮还顺带
修了一个会静默禁用 4 个 producer 服务弱依赖事件链的 kafkax 超时 bug(见下)。

### 真实 Agones 联调环境(真实环境验收)

- minikube v1.30.0 + Kubernetes v1.30.0 + Agones 1.58.0(Helm 安装);本地网络封
  Google preload,`minikube start` 必带 `--preload=false --cache-images=false` +
  阿里云 `kicbase` base-image。
- Fleet:`pandora-battle` 2/2 Ready、`pandora-hub` 3/3 Ready(`region=cn`)。
- `ds_allocator` 跑 `allocator_mode=agones`、`hub_allocator` 跑 `fleet_mode=agones`;
  `AllocateBattle` / `AssignHub` 返真实 GameServer 地址(如 `ds_addr=192.168.49.2:7929`、
  `hub_ds_addr=192.168.49.2:7136`),并写入业务 label `pandora.dev/match-id` /
  `map-id` / `game-mode`。

### 验证结果

- NORMAL 结算路径:5v5 正常战斗结果经 `battle_result` 落库,player.update 事务出箱发布
  (`outbox_published=10`),player 服务消费后段位写回(1516 / 1484),`total_battles` /
  `total_wins` 计数正确;Elo delta 为 +16 / -16,守恒(`delta_sum=0`)。
- ABANDONED 补偿路径:DS 崩溃补偿结果强制 `mmr_delta=0`,10 名玩家均不掉段(MMR 维持
  1500),`total_battles=0`,`mmr_history` 10 行 delta 全 0,outbox 清零;幂等重复提交
  (`alreadyRecorded=true`)不二次改段位。
- 两段补偿链都可复现:
  - 第一段:`tools/scripts/ds_heartbeat_stub.ps1` 验 DS 心跳超时 → abandoned
    (`battle_abandoned_heartbeat_timeout`)→ `pandora.ds.lifecycle`
    (`ds_lifecycle_published`)。
  - 第二段:`tools/scripts/battle_result_outbox_probe.ps1` 验 ReportResult →
    battle_result 事务出箱 → `pandora.player.update` → player 段位回写。

### kafkax producer 超时修复(commit d3df901)

- 现象:producer 初始化报 `Net.DialTimeout must be > 0`,初始化失败仅 Warn,导致
  `ds.lifecycle` 补偿、`player.update` 出箱、`team.update`、`match.progress` 等弱依赖
  事件链被**静默禁用**。
- 根因:`pkg/kafkax/producer.go` 在 yaml 省略 kafka 超时字段时,把 sarama 默认的 30s
  `Net.DialTimeout` / `ReadTimeout` / `WriteTimeout` 无条件覆盖为配置零值,而 sarama
  `Validate()` 要求三者都 > 0。波及全部 4 个 producer 服务(ds_allocator / battle_result
  的 kafka 段无超时字段;team / matchmaker 有 dial+write 但省 read),各自在 yaml 省略的
  那个字段上失败。基于 sarama/mocks 的单测绕过了 config 构建,故从未捕获。
- 修复:抽 `buildProducerConfig`,三个 Net 超时改为 `if d := cfg.X.Std(); d > 0 { ... }`
  正值守卫,零值时保留 sarama 30s 安全默认,不覆盖。**不改任何 dev yaml**(代码边界
  防御足够,避免范围蔓延)。新增 2 个回归单测覆盖全零回退 + 部分覆盖。
- 验证:pkg BUILD=0、kafkax TEST/VET=0、4 个 producer 服务 BUILD=0;真实环境确认
  `ds_lifecycle_producer_ready` + `player_update_producer_ready` 日志出现。

### hub_allocator 接入 Agones Fleet 发现(commit 278a2a2)

- 本轮联调的使能代码:把 hub_allocator 的 `MockHubFleetProvider` 升级为可配置的真
  Agones Fleet 发现(`internal/biz/agones_fleet.go` 250 行 + 测试 186 行),Mock 保留
  作为本地无 k8s fallback。
- 配套落地 `deploy/k8s/agones/`(rbac-allocator / fleet-battle / fleet-hub /
  allocation-example + README)与 `docs/design/agones-dev.md` 联调手册。

### 脚本修复

- 修复 `tools/scripts/battle_result_outbox_probe.ps1` 的 ABANDONED 判定:
  proto3 JSON 会省略 0 值字段,因此 `mmrDelta=0` 时看不到 `mmrDelta` 字段是正常情况。
  新逻辑改为 stats 存在且没有任何非 0 `mmrDelta` 即判定通过。

### 文档同步

- `docs/design/agones-dev.md`:补充两段补偿链 stub 验证说明。
- `deploy/k8s/agones/README.md`:在本地 Agones 验证步骤中并列引用
  `ds_heartbeat_stub.ps1` 与 `battle_result_outbox_probe.ps1`。

## 开发期免密登录开关 login.dev_skip_password(2026-06-09)

为让客户端联调期“随便填个账号名就能进”,login 新增一个纯 dev 开关 `login.dev_skip_password`
(默认 `false`),避免为此再搭一套正式注册 UI/RPC。

### 行为

- 默认 `false`:走正常 bcrypt 密码校验(未变)。
- `true`(⚠️ 仅本机 / 联调,绝不上生产):
  1. 跳过 bcrypt 密码校验,任意 `password_hash` 都放行;
  2. 账号不存在时自动懒注册(snowflake 生 `player_id` 写 `accounts`,靠 `uk_account` 唯一),
     同一账号名每次登录拿到**同一个稳定 `player_id`**(持久化在 MySQL,非临时算);
  3. 并发建号竞争走 `ErrAlreadyExists` → 回查,保证稳定。

### 改动

- `internal/conf/conf.go`:`LoginConf` 加 `DevSkipPassword bool`。
- `internal/biz/login.go`:`Login` 加免密分支 + `ensureAccount` 懒注册;`NewLoginUsecase` 签名加 `devSkipPassword`。
- `cmd/login/main.go`:透传配置 + 启动打 `DEV_SKIP_PASSWORD_ENABLED` 警告日志 + `service_ready` 加 `dev_skip_password` 字段。
- `etc/login-dev.yaml`:加 `login.dev_skip_password: true`(dev 默认开,便于客户端随便登录)。
- `internal/biz/login_test.go`:新增 2 用例(自动建号+稳定 ID、已存在账号错密码仍放行)。

### 安全

⚠️ **绝不能上生产** —— 开启后任意账号名都能登录任意 `player_id`。生产环境留 `false`,
启动时有 `DEV_SKIP_PASSWORD_ENABLED` 警告日志提醒。

### 验证

- login 模块 BUILD=0 / VET=0 / TEST=0。

## 开发期“假注册”开关 login.dev_auto_register(2026-06-16)

注册不属于 login 正式职责;为客户端联调补一个**正交的**“首登即注册”dev 开关
`login.dev_auto_register`(默认 `false`),与既有 `login.dev_skip_password` 解耦。

### 行为

- 默认 `false`:账号不存在直接返 `ErrLoginAccountNotFound`(未变)。
- `true`(⚠️ 仅本机 / 联调,绝不上生产):账号不存在时**首登自动注册**一条 accounts
  记录(snowflake 分配 player_id,**密码存入本次客户端所发 password_hash 的 bcrypt 哈希**),
  后续用同密码走正常 bcrypt 校验(错密码仍拦)→ 真实“首登即注”语义。

### 与 dev_skip_password 正交组合

| dev_auto_register | dev_skip_password | 行为 |
|---|---|---|
| false | false | 正常:账号必须存在 + 密码必须匹配 |
| true  | false | 假注册:未知账号首登注册存本次密码,后续正常校验 |
| false | true  | 免密:已存在账号任意密码放行;未知账号也被懒注册 |
| true  | true  | 最宽松:任意账号名 + 任意密码都能进 |

### 改动

- `internal/conf/conf.go`:`LoginConf` 加 `DevAutoRegister bool`。
- `internal/biz/login.go`:`Login` 账号不存在分支改为 `devAutoRegister || devSkipPassword` 触发懒注册;
  `ensureAccount` 改为**存入本次密码的 bcrypt 哈希**(原存空串),签名加 `passwordHash`;
  `NewLoginUsecase` 签名加 `devAutoRegister`。
- `cmd/login/main.go`:透传 + 启动打 `DEV_AUTO_REGISTER_ENABLED` 警告日志 + `service_ready` 加 `dev_auto_register` 字段。
- `etc/login-dev.yaml`:加 `login.dev_auto_register: true`。
- `internal/biz/login_test.go`:devFakeRepo 改为存密码哈希;新增 `TestLogin_DevAutoRegister_FirstLoginRegisters`
  (首登注册→同密码复登验证通过→错密码 `ErrLoginPasswordMismatch`)。

### 安全

⚠️ **绝不能上生产** —— 生产留 `false`,启动有 `DEV_AUTO_REGISTER_ENABLED` 警告日志提醒。

### 验证

- login 模块 BUILD=0 / VET=0 / TEST=0(biz -count=10 稳定)。

## TLS 证书策略 + 发布前预检(2026-06-10)

收口 UE 客户端连本地 Envoy `:8443` 的 TLS 信任问题,并把 dev/生产边界写成发布门禁。

### 决策

- 生产连接 ②(UE FHttpModule → Envoy)使用**公网 CA(Let's Encrypt / 商业)+ 真实域名**,
  证书 SAN 不写 IP;玩家设备出厂信任公网 CA,零配置握手。
- dev 自签 mkcert 证书只用于本机/团队联调,通过 UE `[SSL] DebuggingCertificatePath`
  叠加公开 dev CA 到 OpenSSL 信任链,不改引擎 `cacert.pem`,不把证书放 `Content/`。
- `deploy/dev-ca/pandora-dev-rootCA.pem` 是公开 CA 证书(`BEGIN CERTIFICATE`),可入库;
  `rootCA-key.pem` / `*.key` 等私钥继续由 `.gitignore` 阻止入库。
- 真实生产配置 `services/**/etc/*-prod.yaml` 被 `services/.gitignore` 忽略;只提交
  `*-prod.yaml.example` 占位符模板。

### 改动

- `docs/design/gateway-decision.md` §14:记录 dev vs 生产 TLS 证书策略、OpenSSL 排查结论、
  域名/公网 CA 成本与 FAQ。
- `deploy/dev-ca/`:新增公开 dev CA、README、局部 `.gitignore`。
- `tools/scripts/import_dev_ca.ps1`:把公开 dev CA 导入 UE 客户端工程 `Config/Certificates/`,
  并维护 `DefaultEngine.ini` 的 `DebuggingCertificatePath`。
- `tools/scripts/release_preflight.ps1`:发布前检查 UE GatewayHost/dev 自动登录、后端
  `dev_skip_password`/reflection、生产 TLS 证书 issuer/SAN。
- `docs/ops/release-checklist.md`:发布前人工清单。
- 9 个现役服务新增 `*-prod.yaml.example` 生产配置模板。

### 验证

- `import_dev_ca.ps1` / `release_preflight.ps1` PowerShell 语法检查通过。
- 9 个 `*-prod.yaml.example` 未开启 `dev_skip_password:true` 或 `enable_reflection:true`。
- staged 文件名检查未发现调试二进制、私钥、真实 `*-prod.yaml`。
- `release_preflight.ps1` 对当前 dev 态按预期 FAIL:UE 未配置生产域名、dev CA 为 mkcert;
  同时确认后端模板危险开关均 PASS。
- 10 module `go build` 通过。

## Snowflake 发号与 nodeID 分配决策(2026-06-11)

记录 ID 生成方案结论,不改代码:

- **拒绝 Redis INCR 每次发号**:比本地 CAS snowflake 慢 4~5 个数量级,单 Redis 会成为全服共享吞吐上限和可用性单点;RDB/AOF 持久化窗口、主从复制滞后或故障切换可能导致计数回退,重启后发重复 ID。
- **当前不动**:14 个服务 + 静态 `node.zone_id` 规划可控,17 bit node 段足够,没有必要为当前阶段引入自动分配。
- **未来 k8s 多副本动态扩缩时再做**:用 etcd Lease + KeepAlive 分配 snowflake nodeID;仍需后台 KeepAlive/session monitor,但它不是 Redis 自拼看门狗,只监听 etcd 原生 lease 状态;KeepAlive channel 关闭、lease revoke 或 session done 视为租约丢失,进程必须停发并主动退出,避免同 nodeID 双活。
- **不用 Redis 租约分配 nodeID**:`SETNX + TTL + 看门狗` 需要自己拼 fencing,GC 停顿、网络分区或进程卡死但业务线程仍跑时会出现租约过期但旧进程继续发号,另一个进程领走同 nodeID 后形成双活;etcd KeepAlive 丢失时必须停发并主动退出。
- 设计落点写入 `docs/design/infra.md` §8.1,决策索引同步到 `CLAUDE.md` §7 和 `docs/design/pandora-arch.md` §11。

## team 新增 GetMyTeam RPC(登录后查自己队伍主界面信息)(2026-06-12)

解决"登录时客户端不知道自己有没有队伍/不知道 team_id"的入口问题。

### 决策

- 放 team 服务做只读 RPC GetMyTeam,**不**塞进 login 返回(login 不再耦合 team;队伍权威在 team 服务;客户端进大厅 UI 时调一次最准)。
- 响应返**完整 Team 快照**,队伍主界面直接渲染。曾考虑 TeamBrief 简短视图后**当日复议废弃**:带宽顾虑不成立——一次性 unary,5 人队 Team 序列化 ~200 字节,真正吃带宽的是高频推送/轮询;且 Brief 会逼客户端多发一次 GetTeam 才能渲染主界面,两次往返反而更费。Team 本身已是 §5.11 客户端最小视图(不含 updated_at_ms 等存储字段)。
- **没队伍是正常态**:返 OK + has_team=false,不用 errcode 表达。

### 改动

- proto:`team.proto` 加 GetMyTeam RPC + GetMyTeamRequest/Response(response.team 为完整 Team);regen go pb 33 files + cpp pb 20 files([proto] 需同步 UE 仓库)。
- biz:GetMyTeam(ctx, playerID) → (record, hasTeam, err),查 pandora:team:player:<id> 索引 → 读队伍记录;索引命中但记录已过期/已解散(TTL 竞态)时按无队伍处理并顺手清脏索引(否则玩家被 ClaimPlayer SETNX 挡住无法再建队)。
- service:GetMyTeam player_id 以 JWT ctx 为准(R5),复用 biz.RecordToProto 组装客户端 Team。
- docs:go-services.md §2.7 RPC 列表修正(删掉早已不存在的 StreamTeamUpdates,补 GetTeam/GetMyTeam)。

### 验证

- 10 module BUILD=0,team VET=0 / TEST=0(biz 新增 4 用例:有队伍/无队伍/脏索引清理后可重建队/DISBANDED 残留索引按无队伍并清理)。

## team push / GetMyTeam 客户端同步约定(2026-06-15)

记录 UE 客户端组队模型与后端 team push 的协作语义,不改代码:

- 后端 `TeamUpdateEvent.team` 已携带完整 `Team` 客户端可见快照,由
  `TeamStorageRecord` 经 `recordToProto` 组装,不是空信号。
- 常规队伍变更 push 在客户端只作为"有变化"信号;客户端收到后防抖合并调用
  `GetMyTeam`,只在 `GetMyTeam` 回包路径写本地 `CurrentTeamSnapshot`。
- 这样做是为了抗 kafka/push 链路的 at-least-once、重复、乱序、处理时快照过期等问题;
  `GetMyTeam` 从 Redis 当前权威态读取,并顺手清理脏 player→team 索引,保证 UI 最终收敛。
- `INVITE_SENT` 例外:被邀请人还没入队,`GetMyTeam` 查不到邀请,客户端应直接读取 push
  里的 `reason` / `invite_id` / `team` 展示邀请 UI。
- UE 当前对 push 驱动的 `GetMyTeam` 做约 0.5s 防抖,避免批量 team push 造成请求风暴。

设计落点已补到 `docs/design/go-services.md` §2.7 team 的"客户端同步约定"。


## hub_allocator 大厅自动扩缩容策略（2026-06-15）

把 hub_allocator 从「固定 Mock/Agones 分片拓扑发现」推进到「按在线人数自动扩缩容 Hub
Fleet 副本」。落地用户要求的策略：开服默认拉起大厅 → 人数超阈值自动新起大厅 → 大厅没人
自动回收。走 Agones Fleet 副本控制（直接读/改 Fleet `spec.replicas`），保持现有 Linux
Agones DS 架构不变，biz 逻辑沿用既有 lazy-seed + 心跳超时 sweep。

### 策略行为

1. **开服默认拉起大厅**:可配置 `hub.min_replicas`(默认 1),Hub Fleet 至少保底 1 个大厅。
2. **人数超阈值自动扩容**:可配置 `hub.players_per_hub`(默认 500),后台 reconcile 按
   `desired = ceil(total_players / players_per_hub)` 算期望副本,受 `hub.max_replicas`
   (默认 20)上限约束,**只扩不缩**(稳态扩容,避免抖动)。
3. **大厅没人自动回收**:总在线人数为 0 时,回收到 `hub.min_replicas`。
4. **兜底扩容**:`AssignHub` 分配时若当前 region 所有分片都满(`ErrHubNoAvailable`),立即触发
   一次 `+1` 扩容,上游重试即可进新大厅。

### 改动

- `internal/conf/conf.go`:`HubConf` 加 `AutoScaleEnabled`(默认 false)/`PlayersPerHub`
  (默认 500)/`MinReplicas`(默认 1)/`MaxReplicas`(默认 20)+ Defaults,并保证
  `MaxReplicas >= MinReplicas`。
- `internal/biz/fleet.go`:新增 `HubFleetScaler` 接口(`GetFleetReplicas`/`SetFleetReplicas`);
  `MockHubFleetProvider` 实现为 no-op(Get 返 MockShardCount,Set 空操作),保持本地无 k8s
  fallback。
- `internal/biz/agones_fleet.go`:`AgonesHubFleetProvider` 实现 scaler — `GetFleetReplicas`
  GET Fleet 读 `spec.replicas`;`SetFleetReplicas` 经 `application/merge-patch+json` PATCH
  `{"spec":{"replicas":N}}`(标准库 net/http,零新增依赖,沿用 W4 ⑬ REST client)。
- `internal/biz/hub.go`:`NewHubUsecase` 自动探测 fleet provider 是否实现 `HubFleetScaler`
  (类型断言)→ 注入 `scaler` 字段;`RunHeartbeatSweep` 每轮 sweep 后追加调用
  `reconcileFleetReplicas`(按总在线算期望副本只扩 / 归零回收 min);`AssignHub` 在
  `ErrHubNoAvailable` 时调 `tryScaleOutOnNoCapacity` 兜底 +1。`autoScaleEnabled()` 门控
  (需 `AutoScaleEnabled=true` 且 scaler 非空,即建议配合 `agones.enabled=true`)。
- `etc/hub_allocator-dev.yaml` / `etc/hub_allocator-prod.yaml.example`:加 `hub.autoscale_enabled`
  / `players_per_hub` / `min_replicas` / `max_replicas` 配置项。
- `deploy/k8s/agones/30-fleet-hub.yaml`:Hub Fleet 默认 `replicas: 1`(对齐「开服默认拉起
  + 按人数扩」,原 3 改 1)。

### 验证

- `hub_allocator` 全量 `go build ./...` + `go test ./...` 通过(biz / data 单测全绿)。

### 阶段限制 / 后续

- 当前「空大厅回收」是「总在线=0 → 回收到 min_replicas」的粗粒度策略;若要「单个大厅空闲
  N 分钟后回收」需再加一个可配置空闲时间阈值 + 逐分片空闲计时(留后续)。
- 真集群联调(指向真 Agones Hub Fleet 验 PATCH replicas 扩缩容)需 `agones.enabled=true` +
  minikube/Agones 环境,交环境/人工。
- reconcile 周期复用 `hub.sweep_interval`(默认 5s),与心跳超时扫描同节拍。

## hub_allocator 强制整合 + 玩家迁移通知（2026-06-15）

把上面「空大厅回收」从「只标 draining、等没人」升级成「**主动把人少的大厅排空、服务端权威
搬迁玩家到该去的大厅、切换前给玩家提示**」的完整强制整合(consolidation)。补齐了用户提的
两件事:① 低负载时强制把玩家换到应切换的 Hub DS;② 切换前给玩家提示/公告的机制。

### 策略行为

1. **强制整合(排空 + 搬迁)**:`hub.consolidation_enabled`(默认 false)开启后,reconcile
   发现 ready 分片数多于负载所需(`need = ceil(total/players_per_hub)`)时,按负载升序把
   **最空的多余分片**标 `draining` 并盖 `draining_since_ms` 时间戳,然后把分片上在册玩家
   做**服务端权威搬迁**(镜像 TransferHub 的「占新位 → 切归属 → 退旧位」顺序)到目标分片
   (同 region 最空 ready 分片),重签新 hub 票据。单 tick 每分片最多搬 `consolidation_batch`
   (默认 50)人,防抢占,剩余下个周期续搬。
2. **切换前提示(双通道)**:
   - **通道 A:Hub DS drain 心跳指令**。draining 分片的 Hub DS 下次 `Heartbeat` 会收到
     `command="drain"` + `grace_seconds`(默认 30),由 Hub DS 在场内弹「N 秒后切换大厅」UMG
     提示,倒计时到点强制重连。
   - **通道 B:Kafka 推送 `pandora.hub.migrate`**。后端搬迁完成后,把
     `HubMigrateEvent{新分片地址 + 新 hub 票据 + 倒计时}` 按 `key=player_id` 推给玩家本人
     (push 服务转发),客户端可无缝倒计时后用新票据重连新大厅。两通道互为兜底:漏听推送的
     玩家靠 DS drain 指令重连,重连走 `AssignHub`(幂等已返回迁移后的新分片)。
3. **排空后缩容回收**:draining 分片**已排空(player_count=0)且过 `migrate_grace_seconds`**
   后才 `RemoveShard` 删镜像并把 Fleet 副本降到仍需存活的分片数,避免提前杀 pod 打断在场
   玩家倒计时。

### 改动

- `proto/pandora/hub/v1/allocator.proto`【proto】:`HeartbeatResponse` 加 `grace_seconds`
  (字段 3,`reserved` 收窄为 4-9);`HubShardStorageRecord` 加 `draining_since_ms`(字段 10);
  新增客户端可见推送结构 `HubMigrateEvent`(player_id / from_hub_pod / to_hub_ds_addr /
  to_hub_ticket / to_hub_pod_name / to_shard_id / grace_seconds / reason / ts_ms)。已重新
  生成 go + cpp pb(需同步到 UE 仓库)。
- `pkg/kafkax/topics.go`:加 `TopicHubMigrate = "pandora.hub.migrate"` 常量并注册进
  `PushTopics`(push 默认订阅)。
- `internal/data/hub_repo.go`:加分片成员反向索引 `pandora:hub:shard:members:{<pod>}`(SET,
  hashtag 同 slot)及 `AddShardMember`/`RemoveShardMember`/`ListShardMembers`;`RemoveShard`
  连带删成员索引;**修复 `HeartbeatShard` 状态降级 bug** — DS 上报的 `ready` 不再把 allocator
  标的 `draining`/`stopping` 冲掉(用 `drainRank` 只允许升级)。
- `internal/conf/conf.go`:`HubConf` 加 `ConsolidationEnabled`(默认 false)/`MigrateGraceSeconds`
  (默认 30)/`ConsolidationBatch`(默认 50)+ Defaults。
- `internal/biz/hub.go`:加 `HubMigratePusher` 接口 + `SetMigratePusher` setter(弱依赖,**不改
  `NewHubUsecase` 签名**,不破现有测试);`reconcileFleetReplicas` 重构为「① 立即扩容 → ②
  强制整合排空搬迁 → ③ 回收过 grace 的空 draining 分片 + 缩容」;新增 `consolidateOnce` /
  `drainAndMigrate` / `migratePlayer` / `pushMigrate` / `reclaimDrainedShards`;`Heartbeat`
  对 draining/stopping 分片下发 `drain`/`stop` + `grace_seconds`;Assign/Release/Transfer 维护
  成员反向索引。
- `internal/service/hub.go`:`Heartbeat` 响应透传 `grace_seconds`。
- `cmd/hub_allocator/main.go`:弱依赖装配 kafka producer(`brokers` 非空才起,失败仅 warn)→
  `kafkaMigratePusher` 适配器 → `uc.SetMigratePusher`;`service_ready` 日志加
  `autoscale_enabled` / `consolidation_enabled`。
- `etc/hub_allocator-dev.yaml` / `*-prod.yaml.example`:加 `consolidation_enabled` /
  `migrate_grace_seconds` / `consolidation_batch` + kafka producer 配置块。
- `services/runtime/push/etc/push-dev.yaml`:订阅补 `pandora.hub.migrate`。

### 验证

- `hub_allocator` `go build ./...` + `go vet ./...` + `go test ./...` 全绿;新增 4 个用例:
  整合搬迁(最空分片排空、玩家归属迁到目标、推送 1 条)、draining 心跳返 drain+grace 且不被
  DS ready 降级、过 grace 回收空 draining 分片、未过 grace 保留分片。
- `pkg`(含 kafkax)、`proto`、`push` 服务均 `go build` 通过。

### 阶段限制 / 后续

- **成员反向索引是 best-effort**(TTL=assignment_ttl,可能漂移):双通道设计下不影响正确性 —
  即便成员集漂移,Hub DS drain 心跳指令仍兜底让玩家重连。
- **缩容删哪个 pod 由 Agones 决定**:降 Fleet `spec.replicas` 后 Agones 自行挑 GameServer 删,
  不保证就是被排空那个。当前只在 draining 分片已排空且过 grace 后才缩容(被删 pod 已无在场
  玩家),精确按 pod 删除待接 Agones game-server-shutdown SDK 再细化。
- UE 侧 Hub DS 处理 `command="drain"` + `grace_seconds`(场内 UMG 倒计时提示 + 到点重连)、
  客户端消费 `HubMigrateEvent`(无缝倒计时切大厅)由 UE 仓库实现,本仓库只定契约。

## hub_allocator 整合复审修复（2026-06-16）

接上面两段(自动扩缩容 + 强制整合)多轮复审捕获的问题,逐个核查代码后修复。
无新服 / 无新 proto / 无新 errcode / 无新 Redis key。

### ① Mock/scaler 语义不一致(P 级)

- **问题**:`MockHubFleetProvider` 恰好实现了 `HubFleetScaler`(`GetFleetReplicas` 返固定
  `MockShardCount`、`SetFleetReplicas` no-op),`NewHubUsecase` 类型断言把它当 scaler 注入 →
  dev yaml `autoscale_enabled:true` + `agones.enabled:false`(Mock)下 `autoScaleEnabled()`
  误判为 true → 每轮 reconcile 都跑(Set 是 no-op 实际不变),还对假分片 / 假玩家跑 consolidation
  搬迁,误导。
- **修复(autoscale 要求真 Agones scaler)**:`fleet.go` 删掉 `MockHubFleetProvider` 的
  `GetFleetReplicas` / `SetFleetReplicas` → Mock 变拓扑-only(不实现 `HubFleetScaler`)→
  Mock 模式 `scaler==nil` → `autoScaleEnabled()==false`,autoscale / consolidation 恒不跑;
  `main.go` Mock 分支加 `autoscale_inert_under_mock` 告警;dev yaml `autoscale_enabled` /
  `consolidation_enabled` 改 `false` + 注释说明需 `agones.enabled=true` 才生效;prod.example
  补「仅 agones.enabled=true 生效」注释;测试加 `memFleetScaler`(嵌 `*MockHubFleetProvider`
  + 可变 replicas + 真 Get/Set)替代 Mock 退化 scaler,保 4 个整合测试仍能检测 scaler 启用治理。
- **教训**:别让打桩 provider 意外满足可选能力接口(degenerate no-op 实现比不实现更危险——
  会让门控误判 enabled)。

### ② 生产 push 漏订阅 `pandora.hub.migrate`(P1)

- **问题**:`push-prod.yaml.example` 的 `topics` 显式列表只补了 `friend.event`,漏了
  `hub.migrate`。生产按模板部署时 hub 迁移推送(通道 B)直接失效。显式 `topics` 列表会**覆盖**
  `kafkax.PushTopics` 默认,必须显式补全。
- **修复**:`push-prod.yaml.example` topics 补 `- "pandora.hub.migrate"` + 对齐 dev 的注释。

### ③ 老在线玩家无成员反向索引 → 首次整合不搬不推(P1/P2)

- **问题**:成员反向索引只在 `AssignHub` / `TransferHub` 时写入,部署 / 上线整合功能**之前**
  就已在线、已有 assignment 的老玩家不在 set 里。`drainAndMigrate` 只枚举 set 成员,**不会对
  这些老玩家做通道 B 的服务端权威搬迁 + 推送**。
- **决策:文档化降级而非冒险 backfill**(AGENTS.md 不过度工程)。靠**通道 A**(Hub DS drain
  心跳 → 客户端重连 `AssignHub`)兜底:幂等路径发现旧分片非 `ready` → 释放旧位重分到 ready 分片,
  旧分片 `player_count` 随之递减 → **最终一致 + 分片可回收**,只是少了无缝推送体验。降级窗口受
  set TTL(=assignment_ttl,默认 30min)约束 —— 活跃老玩家每次 `AssignHub`(含重连自愈)都会补回
  索引。无 pod→players 索引(只有成员 set 本身,鸡生蛋),`assignKey` 未 hashtag 故 cluster SCAN
  复杂 —— 不做 SCAN backfill / proto 改。
- **修复**:`drainAndMigrate` 在 `len(members) < player_count` 时打 `drain_members_index_incomplete`
  告警便于观测降级范围 + 加注释说明;`agones-dev.md §2.2` 补「首次整合降级」阶段限制条目。

### ④ `totalPlayers==0` 直接缩 Fleet 留不可回收 stale 镜像(P2)

- **问题**:原 reconcile 总在线=0 时 `desired = minReplicas` 盲缩 Fleet,未给待删 ready 分片盖
  `draining_since_ms` → Agones 杀 pod 后,心跳超时 `sweepOnce` 只标普通 `draining`(无戳),
  `reclaimDrainedShards`(要求 `DrainingSinceMs > 0`)跳过它 → 镜像变成不可回收的 stale shard
  永久残留在 `pandora:hub:shards` 集合里。
- **修复**:新增 `drainEmptyShards(ctx, shards, keep)` 把超出 `min_replicas` 的空 ready 分片
  (保留 shard_id 最小 keep 个)标 `draining` + 盖戳走回收路径;reconcile ② 分支 `totalPlayers==0`
  调它,③ 统一 desired 计算(删 `totalPlayers==0 → desired=minReplicas` 特例,改为
  `target=live` floor `min`/`need`、cap `max`,只在 `target < current` 时缩)。这样 **Fleet 只在
  镜像回收后才降,保持 Fleet↔镜像一致**,代价是空大厅缩容延迟一个 grace(可接受)。
- 新增 `TestReconcile_ZeroPlayersDrainsEmptySurplusForReclaim`(3 个空 ready 分片,min=1 → 排空
  2 个带戳、保留 hub-1);原有 3 个 reconcile 测试用新逻辑重验通过。

### 验证

- `hub_allocator` `go build ./...` + `go vet ./...` + `go test ./...` 全绿(整合测试 4→5 个,
  新增 `TestReconcile_ZeroPlayersDrainsEmptySurplusForReclaim`)。
- `push-prod.yaml.example` 为纯 yaml 改动(无 go 改动)。

## chat / trade / data_service 三服务补全(2026-06-16)

把 `services/` 下剩余空桩(此前仅 `.gitkeep`)补成完整 Kratos 服务。**不含 `social/dialogue`(NPC 对话由另一窗口实现)**。

### chat(社交聊天,gRPC :50005 / HTTP :51005)

- MySQL 强依赖 `pandora_social`(`deploy/mysql-init/06-social-tables.sql` 加 `chat_private_messages` 表);kafka 弱依赖(`pandora.chat.{world,team,private}` → push);team gRPC 弱依赖(队伍频道成员解析)。
- 三频道 `WORLD` / `TEAM` / `PRIVATE`(`SYSTEM` / `UNSPECIFIED` 拒 `ErrChatChannelInvalid`);内容 utf8 rune ≤ `MaxContentLen`(默认 256,超 `ErrChatMessageTooLong`)+ 敏感词等长 `*` 屏蔽。
- 私聊落库支持离线 `PullHistory`(仅 `PRIVATE` 返历史,世界 / 队伍即时不持久化);队伍 fan-out 排除发送者(原则 2),世界频道 key 空广播(原则 3 例外)。

### trade(玩家交易,gRPC :50012 / HTTP :51012)

- Redis 强依赖(订单状态机 `WATCH/MULTI/EXEC` 乐观锁,`Order` proto bytes 存 `pandora:trade:order:{<id>}` + `pandora:trade:player:<id>` SET 反查);kafka 弱依赖(`pandora.trade.audit`,key=order_id)。
- 两阶段确认:买方 + `PENDING` → `BUYER_CONFIRMED`;卖方 + `BUYER_CONFIRMED` → `ResourceLedger.Settle`(幂等键 = order_id,不变量 §9.7)→ `COMPLETED`;结算失败 → `FAILED`;惰性过期 `OrderExpire`(默认 5m)→ `ErrTradeOrderExpired`。
- W1 用 `NoopResourceLedger` 占位(总成功),真实背包 / 货币原子事务接入后替换。

### data_service(玩家数据网关,gRPC :50003 / HTTP :51003,内网不经 Envoy)

- MySQL 强依赖 `pandora_player`(`deploy/mysql-init/07-data-tables.sql` 加 `player_data` 表:`player_id` PK / `version` 乐观锁 / `data` BLOB);Redis 弱依赖(cache-aside 旁路,Ping 失败降级直连 MySQL);**不接 kafka**(避免与 `player.update` 语义重复)。
- `ReadPlayer` = 缓存命中直返 / miss 读 MySQL 回填;`WritePlayer` = 乐观锁 `UPDATE ... WHERE version=?`(`version==0` → INSERT 起始 1,`rows==0` → `ErrDataVersionMismatch`)写后删缓存;`InvalidateCache` = 删缓存。
- service 层取请求体 `player_id`(内网服务-to-服务,非客户端直连,不从 JWT override),gRPC server 不挂 `AuthRequired`(对齐 player_locator 内网 pattern)。

### 接线 / 验证

- `go.work` 加 `use ./services/economy/trade` + `use ./services/data/data_service`(升 13 module)。
- `deploy/envoy/envoy.yaml` 给 chat + trade 加 `jwt_authn` rule + route(15s)+ STRICT_DNS h2c cluster(:50005 / :50012,客户端面 :8443);**data_service 内网不进 Envoy**。
- `tools/scripts/run_services.ps1` 服务数组加 3 服(均 `Profiles=@('all')`,注释 10 → 13)。
- `deploy/prometheus/prometheus.yml` 已含 51003 / 51005 / 51012 label,无需改。
- go.sum:chat 拷 friend、trade 拷 team、data_service 拷 login(mysql+redis+kafka 全集)。
- 验证:三服 `BUILD=0 / VET=0 / TEST=0`(chat biz ~15 测 / trade biz ~11 测 / data biz 10 测,全内存 fake 无真依赖)。`docs/design/go-services.md` data_service / chat / trade 状态 → ✅。

## dialogue 服务上线(NPC 对话树运行时,2026-06-16)

补 `chat / trade / data_service 三服务补全` 当时显式留给「另一窗口」的 `social/dialogue`。第 14 个 Kratos 业务服,社交域最后一服,`services/` 下不再有空桩。

### dialogue(NPC 对话,gRPC :50013 / HTTP :51013)

- **为什么在服务端做**:有副作用 / 有条件判定 / 影响存档的对话必须服务端权威(客户端不可信,不变量 §9 / §5.11 「客户端只拿客户端可见结构」「DS 不可信」);纯氛围台词才放 UE 本地。`visible` 前置条件、领奖励 / 改任务等副作用都要服务端用权威玩家数据算。
- **零中间件最小版本**:无 MySQL / 无 Redis / 无 Kafka。对话树内联在 `dialogue-dev.yaml`(配置驱动),`ConfigTreeProvider` 内存只读;会话 `MemorySessionStore` 单实例内存(`session_ttl` 默认 5m)。
- **会话状态机**:`StartDialogue(player_id, npc_id)` 服务端分配 `dialogue_id`(snowflake)建会话 → `ChooseOption(player_id, dialogue_id, option_id)` 按 `option_id` 推进节点 → `EndDialogue` 关闭;节点推进由服务端驱动,客户端只渲染 `DialogueState`(speaker/text/options)。非法 npc → `ErrDialogueNotFound`(8001),非法选项 → `ErrDialogueOptionInvalid`(8002),均 W1 已就绪复用无新增 errcode。
- **领域类型不复用 proto**:对话树 `DialogueTree`/`DialogueNode`/`DialogueOption`(data 层领域类型,带 `NextNode` 跳转,proto 不外泄此字段)与客户端可见 `DialogueState`(proto)分离;main.go 把 `conf.TreeConf` 转 `*DialogueTree` 注入 `ConfigTreeProvider`。

### 接线 / 验证

- `go.work` 的 `use ./services/social/dialogue` 启用(升 14 module)。
- gRPC server 挂 `pmw.AuthOptional()`(Envoy jwt_authn 注入 `x-pandora-player-id`);dev `enable_reflection: true` 便于 grpcurl 联调。HTTP server 仅 `/metrics`(dialogue proto 无 google.api.http 注解)。
- `tools/scripts/run_services.ps1` 服务数组加 dialogue(`Profiles=@('all')`,注释 13 → 14)。
- go.sum:dialogue 拷同依赖集服务(无 mysql/redis/kafka 直接依赖,纯 Kratos + snowflake)。
- 验证:`BUILD=0 / VET=0 / TEST=0`(biz 单测覆盖 Start/Choose/End 状态机 + 非法 npc/选项 + 会话过期,全内存无真依赖)。`docs/design/go-services.md` dialogue 状态 ⏸️ → ✅。

### 阶段限制(留后续)

- 内存会话不跨实例、进程重启即丢;多实例部署需把 `SessionStore` 换 Redis 版(biz/service 接口不动)。
- 对话树配置驱动,改文案需重启;接配置中心 / mysql `dialogue_trees`(json blob)热更只换 `TreeProvider` 实现。
- 对话选项当前无副作用;领奖励 / 改任务等接 trade / player 服务后,在服务端权威判定 `visible` 前置条件并执行写操作。

## W5 ① ✅ player 出战养成 Batch 1:选英雄 + 属性加点 + 出战快照(2026-06-09)

把养成域(选英雄 / 属性加点 / 天赋 / 背包)落地的第一批,扩展既有 player 服务,不新建服务。
范围严格遵守 ds-arch.md §0 边界:**只管大厅态持久化与配置**;纯战斗内逻辑(技能 / 出装 / 道具
即时使用)走 UE GAS / Replication,不经 gRPC(MOBA 延迟敏感)。本批提供「开战前快照」GetLoadout,
供匹配 / 进战下发。无新服 / 无新 module。

### 边界决策(用户确认)

- **道具即时使用 = UE GAS**:用户明确「对 MOBA 来说有延迟就是最为致命」,战斗内道具使用零后端往返。
  后端只在后续批次处理大厅态道具(开箱 / 经验书)+ 出售(经济、事务防作弊)。
- **选英雄带功能开关**:`hero_selection_enabled`(dev 默认 false 跳过、prod 默认 true),与 login
  demo-skip 风格一致;关闭时 `SelectHero` 返回 `ERR_PLAYER_FEATURE_DISABLED`。

### 改了什么

- **proto** `player/v1/player.proto`:PlayerService 加 7 RPC(`SelectHero` / `GetActiveHero` /
  `GrantAttributePoints` / `AllocateAttributePoints` / `ResetAttributes` / `GetAttributes` /
  `GetLoadout`)+ 配套 message;新增客户端可见结构 `AttributeAllocation` / `PlayerLoadout`
  (不变量 §14:不外泄存储 record)。`pwsh proto_gen.ps1` 重生成,go pb 33 files,buf lint OK。
- **errcode** 双向加 `ERR_PLAYER_FEATURE_DISABLED=2020` / `ERR_PLAYER_INSUFFICIENT_POINTS=2021`
  (pkg/errcode + common/v1/errcode.proto + 生成 pb);选未拥有英雄复用 `ErrPlayerHeroLocked=2010`。
- **MySQL** `04-player-tables.sql`:`players` 加列 `active_hero_id`(uint32 配置 ID,§12)+
  `unspent_attr_points`;新表 `player_attributes`(uk player_id+attr_key)+ `attr_point_grants`
  (uk player_id+idempotency_key,授予幂等)。
- **data 层** `player_repo.go`:加 7 方法。授予 / 分配 / 洗点走事务;分配前 `SELECT ... FOR UPDATE`
  锁 players 行校验 unspent>=sum,不足 → `ErrPlayerInsufficientPoints`;授予用 `attr_point_grants`
  唯一键防重复授予(命中 1062 → 读回当前 unspent 返回 already=true)。
- **biz 层** `player.go`:7 usecase,出战养成前 `EnsureProfile` 懒建档;`SelectHero` 校验功能开关
  + 英雄已拥有;`GetLoadout` 组装快照(GetActiveHero + GetAttributes)。
- **service 层** `player.go`:7 handler,沿用 player_id==0 → ERR_INVALID_ARG + toProtoCode 模式。
- **conf / yaml**:`PlayerConf` 加 `HeroSelectionEnabled`;dev=false / prod.example=true。
- **测试** `player_test.go`:fakeRepo 扩 7 方法 + 8 用例(开关关 / 英雄未拥有 / 选英雄成功 /
  授予幂等 / 授予校验 / 分配点不足 / 分配+洗点回退 / 分配校验 / 快照组装)。

### 验证

- proto_gen.ps1:go pb 33 files,buf lint OK,buf generate OK。
- BUILD:`go build ./pkg/... ./proto/... ./services/account/player/...` = 0。
- VET=0;TEST=0(biz 用例 13→21)。errcode 改动纯加常量,其它服务不受影响。
- cpp pb 需同步到 UE 仓库 `Source/Pandora/Generated/Proto/`(本轮含 proto 改动,[proto] 标记)。

## W5 ② ✅ Kill-Switch RPC 级临时关停 + 自动防护四层方案(2026-06-17)

为了解决「某个 RPC / service 出重大问题时,不发版不重启就能临时关停、修好再秒级恢复」的问题,
补齐 Pandora 的四层防护:

1. **第 1 层 Envoy 整组挡流**:`deploy/envoy/envoy.yaml` 路由表顶部加注释态
   `direct_response 503` 维护示例,用于客户端入口整组紧急挡流。
2. **第 3 层 Kill-Switch RPC 级关停**:`pkg/killswitch` 新增 Manager(atomic.Pointer 快照热更)、
   file 源(fsnotify 热加载)、driver 注册模式、全局 Default、feature 组注册、fail-open 装配。
   匹配粒度统一为单 RPC / `<service>/*` / `feature/<name>` / `*`。
3. **中间件接线**:`pkg/middleware.KillSwitch()` 进入 `pkg/grpcserver` 默认链;
   server stream 通过 `KillSwitchStreamCheck` 手动接入,`push.Subscribe` 已补入口检查。
4. **第 4 层自动防护**:`pkg/middleware.RateLimit()` 用 Kratos BBR server 侧限流,
   `pkg/middleware.CircuitBreaker()` 用 SRE client 侧熔断并默认挂进 `pkg/grpcclient`。
5. **etcd 源 opt-in**:`pkg/killswitch/etcdkv` 独立 module 隔离 etcd client 重依赖,
   服务 blank import 后可启用 `source: "etcd"`。
6. **错误码 / 文档 / 示例配置**:`ERR_SERVICE_DISABLED=13` 同步到 proto + pkg/errcode;
   login dev 示例启用 file 源并新增 `etc/killswitch.yaml`;
   `docs/ops/service-killswitch.md` 记录单 RPC、整服关停、feature、Envoy、etcd、限流/熔断操作。

### 验证

- `go test ./pkg/...`、`go vet ./pkg/...`、`go build ./pkg/...` 通过。
- `pkg/killswitch/etcdkv`: `go test ./...`、`go vet ./...` 通过。
- `proto`: `go test ./...` 通过。
- `services/runtime/push`: `go test ./...`、`go vet ./...`、`go build ./...` 通过。
- `services/account/login`: `go build ./...`、`go vet ./...`、`go test ./...` 通过。
- 受同一工作树影响的 `player` / `trade` / `dialogue` 也已补跑 build/vet/test 相关验证并通过。

### 阶段限制 / 后续

- 第 2 层注册中心 deregister + 第 5 层优雅 drain 仍待 etcd registry 真接入;当前整服关停用
  `<service>/*` Kill-Switch 或 Envoy direct_response 暂代。
- metrics 当前保持低基数粗分类 label,不会直接用 `13` / `9` 作为 Prometheus label;排障时结合
  access log、启动日志和业务返回码判断。
## W5 ② ✅ player 养成 Batch 2 — 装备预设 + 天赋树(2026-06-18）

接 W5 ①（Batch 1 选英雄 + 属性加点 + GetLoadout）。本批扩 `player` 服务，不新建服务，加「装备预设」「天赋树」两套养成数据，并把 `GetLoadout` 出战快照补全。

### 改动范围

1. **proto [proto]**：`proto/pandora/player/v1/player.proto` +6 RPC
   （`SetEquipment` / `GetEquipment` / `GrantTalentPoints` / `SetTalents` / `ResetTalents` / `GetTalents`），
   累计 13 个养成 RPC。新增客户端可见结构 `LoadoutEquipment{slot, item_config_id uint32}`、
   `TalentNode{talent_id uint32, level int32}`；`PlayerLoadout` 扩 field 5/6/7
   （`equipment` / `talents` / `unspent_talent_points`），仍守 §14 不外泄存储 record。
2. **errcode**：复用 `ERR_PLAYER_FEATURE_DISABLED=2020`（功能开关关闭）、
   `ERR_PLAYER_INSUFFICIENT_POINTS=2021`（天赋点不足），无新增错误码。
3. **MySQL**：`deploy/mysql-init/04-player-tables.sql` `players` 表 +`total_talent_points`；
   新表 `player_equipment`（uk `player_id`+`slot`）、`player_talents`（uk `player_id`+`talent_id`）、
   `talent_point_grants`（uk `player_id`+`idempotency_key` 授予幂等）。
4. **data**：`player_repo.go` 加 `EquipmentSlot` / `TalentLevel` 类型 + 6 方法 +
   helper `talentUnspent`（`total_talent_points` - SUM(level)）+ `rowQueryer` 接口（抽象
   `*sql.DB` / `*sql.Tx`）；`SetTalents` / `ResetTalents` / `GrantTalentPoints` 走事务 `FOR UPDATE`。
5. **biz**：+6 usecase。`SetEquipment` / `SetTalents` / `ResetTalents` 查
   `LoadoutCustomizeEnabled` 开关，关闭返 `ErrPlayerFeatureDisabled`；
   `GrantTalentPoints` 系统驱动无开关。`GetLoadout` 组装 `equipment` + `talents`。
6. **conf**：+`LoadoutCustomizeEnabled`（dev=false demo 跳过 / prod.example=true，沿用 login demo-skip 风格）。
7. **test**：`fakeRepo` 扩 6 方法 + 8 用例。

### 验证结果

- `go build ./pkg/... ./proto/... ./services/account/player/...` = BUILD OK。
- `go vet` / `go test ./services/account/player/...` = VET OK / TEST OK。

## W5 ③ ✅ inventory 服务上线 — 大厅背包（用 / 售 / 授予）(2026-06-18）

第 15 个 Go 业务服，落在 economy 域（与 trade 同级）。处理大厅态背包持久化：货币（gold）+ 可堆叠道具（按 `item_config_id` 聚合，MVP 不做 per-instance item_uid）。**战斗内道具即时使用仍走 UE GAS，后端不介入**（ds-arch.md §0 边界铁律）。

### 改动范围

1. **proto [proto]**：`proto/pandora/inventory/v1/inventory.proto`，package `pandora.inventory.v1`。
   `InventoryService` 4 RPC（`GetInventory` / `GrantItems` / `UseItem` / `SellItem`）。
   message `ItemStack{item_config_id uint32, count int64}`、`ItemGrant`、`CurrencyKind` enum
   （UNSPECIFIED=0 / GOLD=1）、`Inventory{player_id, gold int64, items[]}`。
   全部写 RPC 带 `idempotency_key`。
2. **端口**：gRPC `50015` / HTTP `51015`（HTTP 仅 `/metrics`，inventory.proto 无
   google.api.http 注解）。落在 push（50014）与 ds_allocator（50020）之间空档；
   `docs/design/infra.md §6.2` 已登记，「14 个 go 服务」改 15。
3. **errcode**：新增 `ERR_INVENTORY_ITEM_NOT_FOUND=7010` /
   `ERR_INVENTORY_INSUFFICIENT=7011` / `ERR_INVENTORY_ITEM_NOT_USABLE=7012` /
   `ERR_INVENTORY_NOT_SELLABLE=7013` / `ERR_INVENTORY_LOCK_FAILED=7014`，
   双向同步（`pkg/errcode/errcode.go` + `proto/pandora/common/v1/errcode.proto`，已 regen）。
4. **MySQL**：`deploy/mysql-init/08-inventory-tables.sql` USE `pandora_trade`。
   `player_currency`（PK `player_id`，`gold` BIGINT）、
   `player_items`（uk `player_id`+`item_config_id`，`count` BIGINT）、
   `inventory_ledger`（uk `player_id`+`idempotency_key`，`op` grant|use|sell）。
5. **data**：`inventory_repo.go` `MySQLInventoryRepo`。ledger-first 幂等（`insertLedger`
   命中 1062 → already=true）；`deductItemTx` `SELECT...FOR UPDATE` 防超扣
   （不足 / 缺失返 `ErrInventoryInsufficient` / `ErrInventoryItemNotFound`）；全事务。
6. **conf**：`ItemRule{ItemConfigID, Usable, Sellable, SellUnitPrice}` + `RuleOf(id)`。
   `biz`：`UseItem` 查 `rule.Usable` → `ErrInventoryItemNotUsable`；
   `SellItem` 查 `rule.Sellable` → `ErrInventoryNotSellable`，算 `gold = SellUnitPrice * count`。
7. **依赖**：纯 MySQL（无 kafka / redis）。`go.mod` replace `../../../{pkg,proto}`；
   `go.work` 加 `use ./services/economy/inventory`；`go mod tidy` 完成。
8. **test**：`biz` 9 用例（fakeRepo 内存：幂等 / 校验 / 不可用 / 不足 / 成功 / 不可售 / 售出给币 / 售出幂等）。

### 验证结果

- `go build ./pkg/... ./proto/... ./services/economy/inventory/...` = BUILD OK。
- `go vet` / `go test ./services/economy/inventory/...` = VET OK / TEST OK（biz 9 测全过）。
- 全量回归：其余 12 个服务 `go build` 全绿，proto 重生（common errcode + 新 inventory pb）未破坏任何服务。

## W5 Batch 4 ✅ DS 开战前养成快照契约（文档）(2026-06-18）

- `docs/design/ds-arch.md` 新增 §0.5「开战前养成快照下发契约（W5 养成 / 背包）」：
  匹配成功 → ds_allocator 票据 → 客户端连 Battle DS → Battle DS 调 `player.GetLoadout`
  → 快照初始化 GAS。6 条不变量：快照只读一次 / 只用客户端可见结构 / DS 不可信 /
  空值降级 / 超时 5s / 装备天赋只影响初始属性。

## W5 UE 前端骨架 — player + inventory 客户端（F:\work\Pandora-Client-SVN）(2026-06-18）

为养成域两批后端补 UE 5.7 客户端 C++ 骨架（SVN 仓库，非本 workspace）。沿用既有两段式分层：`PandoraProto` 模块（wire 结构 + codec，protobuf 头只出现在 per-domain codec .cpp）+ `Pandora` 模块（BP USTRUCT + per-domain UObject client + `UPandoraBackendSubsystem` 入口）。

### 改动范围

1. **`PandoraWireTypes.h`**：加 player + inventory 的请求 / 响应 wire struct（纯 POD，无 protobuf / UObject）。
2. **`PandoraMessageCodec.h`**：加 player + inventory 的 Encode/Decode 声明（全 `PANDORAPROTO_API`，不含 protobuf 头）。
3. **新 `PandoraMessageCodec_Player.cpp` / `PandoraMessageCodec_Inventory.cpp`**：
   `push_macro/pop_macro("verify")` 包 protobuf include，`ParseFromArray` / `Serialize`，
   repeated 字段用 `Reset(size)` + `AddDefaulted_GetRef`。13 + 4 套 Encode/Decode。
4. **`PandoraBackendTypes.h`**：加 BP USTRUCT（`FPandoraPlayerLoadout` / `FPandoraInventory` 等，
   uint64→int64 / uint32→int32 适配蓝图）+ 多形态结果委托。
5. **新 `PandoraPlayerClient`**（13 BlueprintCallable RPC + 7 委托，CreateUObject payload 绑定专用 handler）、
   **新 `PandoraInventoryClient`**（4 RPC + 4 委托）。
6. **`PandoraBackendSubsystem.h/.cpp`**：加 `GetPlayerClient()` / `GetInventoryClient()` 访问器 +
   `NewObject<>(this)` + `SetOwner(this)`。

### 阶段限制 / 委派（Codex / 人）

- **UE codec .cpp 当前编不过是预期状态**（decision-revisit-ue-proto-codec.md）：
  `player.pb.h` 缺（仅 .pb.cc）、`inventory.pb.h/.pb.cc` 全缺（全新 proto）。
  需 Codex 跑 `buf generate --template buf.gen.cpp.yaml`（protobuf **v35.0**）生成后同步到
  `Source/ThirdParty/PandoraProtoGenerated/` + `Source/PandoraProto/Public/Generated/Proto/`。
  **⚠️ 须核对 UE ThirdParty protobuf 运行时版本与 v35.0 一致**，否则 ABI 冲突。
- **后端收尾（Codex / 人）**：`player` + `inventory` `go mod tidy` 固化 go.sum；
  cpp pb 同步 UE 两处目录（[proto]）；inventory 加 `prometheus.yml` 51015 label +
  `run_services.ps1` +1 服 + 视客户端是否直连决定 Envoy 路由（当前未加 inventory route）。
- **git**：等用户「帮我 commit」（建议 scope 拆 player / inventory / docs，
  AGENTS.md §11.1，避免 friend/hub 混 commit 教训）。Claude 不做 git。
- **其它排期项（环境 / DS 类，非本次 Claude 业务实现范围）**：
  真 Agones 联调（需 minikube / Agones 集群 → Codex / 人）；
  locator HUB 上报（DS 侧 `PandoraHubServer` 模块逻辑，client-only 仓可能缺）；
  第 2 / 5 层优雅 drain（需 etcd registry 真接入，现用 `<svc>/*` Kill-Switch 暂代）。

## W5 friend 分布式好友图决策记录（文档）(2026-06-18）

- `docs/design/go-services.md` §2.4 追加「分布式好友图决策」：明确当前 `AcceptFriend`
  是单 MySQL `pandora_social` 本地事务方案，依赖 `friend_requests FOR UPDATE` +
  同事务 block / 好友上限 / 双向 `friendships` 写入；该保证不能原样迁移到 Redis Cluster
  或按 `player_id` 分片后的 MySQL。
- 目标形态记录为：`friend_request` 单点权威 CAS（`pending -> accepted`）+
  Kafka `FriendshipEstablished` 事件 + 按 owner 分片异步幂等建双向边 + 软好友上限 +
  block 补偿幂等。
- `docs/design/pandora-arch.md` §11 追加决策行索引。当前 W5 以内不改代码，进入全服社交扩展前
  必须先补 friend outbox / 事件消费 / 补偿幂等键设计，再拆 `AcceptFriend`。

## W5 决策 ✅ 好友图扩容存储路线拍板 = (A) TiDB (2026-06-18）

`friend-distributed-scaling.md` §14「待拍板」三选一拍板结论：扩容存储路线选 **(A) TiDB 过渡**，否决 (B) 分片 MySQL + dtm 编排、(C) 其他分布式 ACID 库（Yugabyte/Cockroach 等留作备选）。

### 决策要点

- **选 TiDB 的理由**：阶段 2（千万级早期）代码改动最小——`AcceptRequest` 的
  `BEGIN / FOR UPDATE / 多表写 / COMMIT` 在 TiDB 跨节点原生可跑（Percolator 2PC），
  **跨人强一致 + `maxFriends` 硬上限语义都保留**，不必为提前分片重写成 CAS + Kafka 异步建边。
- **不立即落地**：现阶段按 §13.1 保持单 MySQL，TiDB 是「扩容触发信号出现后」的目标形态，不提前引入。
- **逃生通道**：阶段 3 极限体量、TiDB 2PC 热路径成本显现时，再把好友接受热路径拆成
  §5「单点 CAS + Kafka 异步幂等建边」卸掉跨节点事务。
- **TiDB 必知代价**（§8.2）：雪花 `request_id` 单调主键热点须 `AUTO_RANDOM` / `SHARD_ROW_ID_BITS` 打散；
  跨节点 2PC 热路径延迟；PD + TiKV + TiDB Server 运维成本重一个量级。

### 改动范围（仅文档）

1. `docs/design/friend-distributed-scaling.md` §14：勾选全部待拍板项 + 新增「拍板结论（2026-06-18）」段。
2. `docs/design/pandora-arch.md` §11 决策行：追加「存储扩容 / 2026-06-18 / 选 (A) TiDB」一行。
3. 本条 PROGRESS。

无代码 / proto / 配置改动，不触发 build。

## W5 落地 ✅ friend 服务切 TiDB（项目内部分，人工拍板覆盖"不提前引入"）(2026-06-18）

承上条拍板。人工决策**主动推翻"不立即落地"**，确认现在就把 friend（及同库 chat）切到 TiDB。本次完成**项目内可落地部分**（DDL + 配置 + 文档）；起集群 / 装载 / 数据迁移按 AGENTS.md §11.1 交 Codex / 人。

### 改动范围

1. **DDL（新）**：`deploy/tidb-init/01-social-tidb.sql` —— `pandora_social` 的 TiDB 版 schema，
   与单 MySQL 的 `deploy/mysql-init/06-social-tables.sql` 是**两条独立线**。§8.2 热点处理：
   - `friendships` / `blocks` 代理主键 `id`：`AUTO_INCREMENT` → `AUTO_RANDOM`（打散写热点；
     friend_repo.go 全走 `INSERT IGNORE` + player_id 查询，不读 id / 不依赖 LastInsertId，零副作用）；
   - `friend_requests` / `chat_private_messages` 显式雪花主键（业务 ID 不变量 §9.11 不能改）：
     `NONCLUSTERED PK + SHARD_ROW_ID_BITS=4 + PRE_SPLIT_REGIONS=4`（行按随机 _tidb_rowid 落盘，
     避雪花时间序写热点；代价：按 ID 点查多一次回表，这两表点查频率低可接受）；
   - collation `utf8mb4_0900_ai_ci` → `utf8mb4_bin`（业务键全 BIGINT 数值，大小写不敏感无意义；
     `utf8mb4_bin` 全 TiDB 版本可用）。
2. **配置（新）**：`services/social/friend/etc/friend-dev-tidb.yaml` —— 仅 `node.mysql_client.dsn`
   指向 TiDB（:4000，collation utf8mb4_bin），其余同 `friend-dev.yaml`。原 dev.yaml 不动（不破坏单 MySQL 流程）。
3. **README（新）**：`deploy/tidb-init/README.md` —— 落地状态表 + Codex/人交接步骤（起集群 / 授权 / 装载 / 迁移）。
4. **决策文档**：`friend-distributed-scaling.md` §14 加「落地修订（2026-06-18）」段；
   `pandora-arch.md` §11 加一行「人工拍板推翻'不提前引入'：现就切 TiDB」。

### Go 业务代码：零改动

TiDB 兼容 MySQL 协议，`pkg/mysqlx` + `database/sql` + `go-sql-driver/mysql` 直连；
[friend_repo.go](services/social/friend/internal/data/friend_repo.go) 的事务 / `FOR UPDATE` /
`INSERT IGNORE` / `maxFriends` 硬上限校验全部不变（这正是选 TiDB 而非分片 MySQL 的核心收益，§8.1）。

### 待办（环境，Codex / 人，§11.1，Claude 不起重服务）

- 起 TiDB 集群（PD + TiKV + TiDB Server；本机 `tiup playground` 或自建 compose）；
- 建 `pandora` 账号授权 `pandora_social`；装载 `01-social-tidb.sql`；
- 单 MySQL → TiDB 数据迁移（Dumpling + Lightning / DM，在线双写灰度，如已有数据）；
- friend 改用 `friend-dev-tidb.yaml` 启动验证。

### 验证

- 纯 DDL / yaml / 文档，**无 Go 改动**，未触发 build；现有 `go build ./services/social/friend/...` 不受影响。
- git 收尾等用户「帮我 commit」（建议 scope：`feat(friend): 好友图迁 TiDB schema + 配置`），Claude 不做 git。

## W5 落地 ✅ friend TiDB turnkey 化 + 全链路自检（交付冲刺）(2026-06-18）

用户特殊需求：下周交付，要求把好友 TiDB 相关一次做到位。本批把「起 TiDB 集群」从手工多步收敛成**一条命令**，并对 friend 服务做全层自检确认代码侧无遗留。

### 全层自检结论（friend 服务代码已完整）

- **biz**：`AddFriend` / `AcceptFriend` / `ListFriends` / `Block` 全实现；不变量到位
  （加自己拒、互拉黑拒、已好友拒、AcceptRequest 事务内权威校验 target+block+上限+状态、
  推送原则 2 收发方向正确、弱依赖 pusher/online 降级）。
- **service/grpc**：4 RPC 全实现，R5 用 JWT ctx 的 player_id override 请求体，errcode→proto 1:1。
- **test**：`go test ./services/social/friend/...` = OK（biz 用例全过）；`go vet` = OK。
- 结论：**friend 业务代码无需为 TiDB 改任何一行**（TiDB 兼容 MySQL 协议，§8.1 兑现）。

### 新增 turnkey 资产

1. **`deploy/docker-compose.tidb.yml`**：本地 TiDB 集群（PD :2379 / TiKV :20160 / TiDB :4000，
   单副本开发用），独立 `pandora-tidb-net`，只对宿主暴露 :4000 + :10080，**与单 MySQL 并存不冲突**。
   `docker compose config` 校验通过。
2. **`tools/scripts/tidb_up.ps1`**：一键 `起集群 → 等 :4000 就绪 → 建 pandora 账号授权 →
   装载 01-social-tidb.sql`；`-Pull` 刷镜像、`-Down [-Volumes]` 停/清。装载用一次性
   `mysql:8.4` client 容器走 `pandora-tidb-net`，不要求本机装 mysql client。
3. README 更新交接表 + 一条命令路径。

### 待办（环境，Codex / 人，§11.1，Claude 不起重服务 / 不拉镜像）

- 跑 `pwsh tools/scripts/tidb_up.ps1`（首次拉 pingcap/{pd,tikv,tidb} 镜像，需联网）；
- 单 MySQL → TiDB 数据迁移（如已有数据，Dumpling+Lightning/DM）；
- friend 用 `friend-dev-tidb.yaml` 起，grpcurl 跑 AddFriend/AcceptFriend/ListFriends/Block 验收。

### 验证

- `docker compose -f deploy/docker-compose.tidb.yml config` = OK；
  `go test ./services/social/friend/...` / `go vet` = OK；`go build ./services/social/friend/...` = OK。
- 起集群本身属环境动作，按 §11.1 交 Codex/人，Claude 未执行。
- git 收尾等用户「帮我 commit」。

## W5 Codex 验收 ✅ TiDB 起集群 + friend 四 RPC 联调通过（2026-06-18）

承上条 Claude turnkey 交接，Codex 已按 AGENTS.md §11.1 执行环境侧动作并完成联调。

### 修正

- `tools/scripts/tidb_up.ps1` 固定 Compose project name 为 `pandora-tidb`。原因：`deploy/docker-compose.dev.yml`
  默认 project name 也是 `deploy`，不固定时 `docker compose ps/up` 会混入 dev compose 状态，甚至留下半创建 TiDB 容器。
- `deploy/tidb-init/README.md` 补诊断命令与 Codex 实跑记录，并修正文件列表换行。

### 实跑结果

- `pwsh tools/scripts/tidb_up.ps1` 成功：拉取/使用 `pingcap/pd:v8.5.1`、`pingcap/tikv:v8.5.1`、
  `pingcap/tidb:v8.5.1`，启动 `pd` / `tikv` / `tidb`，创建 `pandora` 账号并装载 `01-social-tidb.sql`。
- `docker compose -p pandora-tidb -f deploy/docker-compose.tidb.yml ps`：PD / TiDB healthy，TiKV running，
  TiDB 暴露 `127.0.0.1:4000` 与 status `10080`。
- TiDB 回查：`blocks` / `chat_private_messages` / `friend_requests` / `friendships` 四表存在。
- friend 使用 `services/social/friend/etc/friend-dev-tidb.yaml` 启动成功，日志确认连接
  `127.0.0.1:4000/pandora_social`，Kafka producer ready，gRPC 监听 `:50004`。

### RPC 验收

用 `grpcurl -plaintext -H "x-pandora-player-id: <pid>"` 模拟 Envoy 鉴权后的 player_id 注入：

- `AddFriend(1001 -> 1002)` 返回 `request_id=2633442017705984`。
- `AcceptFriend(1002, request_id)` 返回 OK；TiDB 回查 `friend_requests.status=2`，`friendships`
  有 `1001->1002` 与 `1002->1001` 两条边。
- `ListFriends(1001)` / `ListFriends(1002)` 均能读到对方。
- `Block(1001 -> 1002)` 返回 OK；TiDB 回查 `blocks` 有 `1001->1002`，`friendships` 已清空；
  两边 `ListFriends` 返回空。
- 验收结束后已清理 1001/1002 测试数据，`friend_requests` / `friendships` / `blocks` 当前为空。

### 当前状态

- TiDB 集群保留运行，供后续继续联调。
- 临时 friend 测试进程已停止，避免占用 `50004/51004`。
- 剩余：如已有单 MySQL 旧数据，仍需单独做 Dumpling + Lightning / DM 迁移；本轮只验证空库 schema 与 RPC 链路。

## W5 好友闭环 RPC 补全(2026-06-18)

按「交付完整功能、不能只为交付不实现完整」要求,补全 friend 服务功能缺口,新增 5 个 RPC 关闭闭环:

- **RejectFriend(player_id, request_id)** — 拒绝好友请求;不向请求方推送(避免被拒尴尬);非目标人/已处理返回 ErrFriendNotFound。
- **ListFriendRequests(player_id)** — 查待处理(收到的)好友请求。**关键缺口**:此前离线玩家无法处理请求(push 弱依赖且无查询入口),现可主动拉取。
- **RemoveFriend(player_id, target_player_id)** — 删好友,双向边,幂等;不写黑名单(可重加)。
- **Unblock(player_id, target_player_id)** — 取消拉黑,幂等;不自动恢复好友关系(需重新加)。
- **ListBlocks(player_id)** — 查黑名单,回客户端可见结构 BlockInfo。

### 分层落地
- proto: `proto/pandora/friend/v1/friend.proto` 加 5 RPC + 对应 message(FriendRequestInfo / BlockInfo 等),`buf lint` + `buf generate` EXIT=0,go pb 已重生。**[proto] 改动,cpp pb 待 Codex 同步到 UE 仓库 `Source/Pandora/Generated/Proto/`(CLAUDE.md §5)**。
- data: `internal/data/friend_repo.go` 加 RejectRequest / ListIncomingRequests / RemoveFriend / Unblock / ListBlocks 五方法 + IncomingRequestRow / BlockRow 两 row 类型;RejectRequest 用事务 + FOR UPDATE,并发只有一方成功(rejected=false)。
- biz: `internal/biz/friend.go` 加 5 usecase,复用现有 errcode(无新增码);幂等操作返回 OK,校验用 ErrInvalidArg,找不到用 ErrFriendNotFound。
- service: `internal/service/friend.go` 加 5 handler,player_id 一律以 JWT ctx 为准(R5)。
- grpc 注册无需改(FriendService 实现接口即自动注册)。

### 验证
- `go build ./proto/... ./services/social/friend/...` EXIT=0
- `go vet ./services/social/friend/...` EXIT=0
- `go test ./services/social/friend/...` ok(biz 新增 RejectFriend / ListFriendRequests / RemoveFriend / Unblock / ListBlocks 用例:OK/非目标/无请求/幂等/可重加/解黑重加)

### 交接 Codex
- [proto] cpp pb 同步到 UE Pandora-Client 仓库。
- commit 建议:`feat(friend): 好友闭环 RPC(拒绝/查待处理/删好友/取消拉黑/查黑名单)[proto]`。

## W5 Codex 收尾 ✅ DAU 200 万扩容底座 module 入 workspace（2026-06-19）

承接 Claude 本轮「DAU 200 万扩容底座」交付，Codex 按 AGENTS.md §11.1 做环境 / workspace 收尾，不改业务逻辑、不改单机默认配置。

### 完成内容

- `go.work` 追加 `use ./pkg/snowflake/etcdnode`，让新独立 module 纳入根 workspace。
- 在 `pkg/snowflake/etcdnode` 执行 `go mod tidy`，生成 `go.sum`，固化 `go.etcd.io/etcd/client/v3` 等依赖。
- 保留该 module 独立边界：核心 `pkg/snowflake` 与业务服务不会无条件引入 etcd client；只有显式 import `pkg/snowflake/etcdnode` 的调用方承担依赖。

### 验证

- `go mod tidy`（目录：`pkg/snowflake/etcdnode`）EXIT=0。
- `go test ./...`（目录：`pkg/snowflake/etcdnode`）EXIT=0（no test files）。
- `go build ./pkg/snowflake/etcdnode/...`（目录：仓库根）EXIT=0。
- `go build ./pkg/... ./pkg/snowflake/etcdnode/...`（目录：仓库根）EXIT=0。
- `gofmt` 覆盖本轮新增 / 触达 Go 文件。

### 剩余接力

- 按具体服务选择样板接入 `SnowflakeConf.node_id_source="etcd"`，调用方必须监听 `Holder.Lost()`，失租立即停发并退出。
- Redis Cluster / MySQL ShardSet / push 定向路由 / Agones Ready 池化仍按 `docs/design/scale-dau-2m.md` 分阶段施工。

## W5 Codex 验收 ✅ Redis Sentinel / Cluster 本地实例已起并验证（2026-06-19）

承接 Claude 本轮「Redis 去单点部署配置」交接，Codex 按 AGENTS.md §11.1 执行环境侧动作。

### 启动与验证

- `docker compose -f deploy/docker-compose.redis-sentinel.yml config --quiet` EXIT=0。
- `docker compose -f deploy/docker-compose.redis-cluster.yml config --quiet` EXIT=0。
- Docker Desktop daemon 初始未运行；Codex 启动 `com.docker.service` 与 Docker Desktop 后，`docker info` ready。
- Sentinel 路线已启动：`docker compose -f deploy/docker-compose.redis-sentinel.yml up -d` EXIT=0。
  - `pandora-sentinel-1 redis-cli -p 26379 sentinel master pandora-master` 返回 `flags=master`、`num-slaves=2`、`num-other-sentinels=2`、`quorum=2`。
  - `pandora-redis-master` health=healthy，两个 replica 与三个 sentinel 均 running。
- Cluster 路线已启动：`docker compose -f deploy/docker-compose.redis-cluster.yml up -d` EXIT=0。
  - `pandora-rc-init` EXIT=0，日志显示 3 master + 3 replica，`[OK] All 16384 slots covered`。
  - `redis-cli -c -p 6379 cluster info` 返回 `cluster_state:ok`、`cluster_slots_assigned=16384`、`cluster_known_nodes=6`、`cluster_size=3`。
  - `cluster shards` 显示 3 个 master 分片区间：`0-5460`、`5461-10922`、`10923-16383`，各 1 个 replica，health=online。

### CROSSSLOT 验证

- 跨 slot 正例验证：`MGET pandora:cross:a pandora:cross:b` 返回 `CROSSSLOT Keys in request don't hash to the same slot`，证明本地环境确为 Redis Cluster 行为。
- 同 hash tag 正例验证：`MGET pandora:ok:{demo}:a pandora:ok:{demo}:b` 返回 `1` / `2`。

### 未执行项

- 未把强随机 Redis 密码写入 `*-prod.yaml`：当前仓库未找到 `services/**/**-prod.yaml`，且真实生产密码不能写入 git 跟踪文件。生产应通过 secret/部署系统注入 `node.redis_client.password`，配置片段继续以占位符记录在 `deploy/redis/README.md`。
- 生产 6 主 6 从仍按 `deploy/redis/README.md` §4 由 k8s redis-operator/helm 落地；本轮只验证本地 Sentinel 与最小 Cluster。

## W5 ✅ auction 服务上线 — 全服拍卖行 / 跨玩家撮合引擎(2026-06-19)

按 `docs/design/decision-revisit-auction-engine.md`(并行窗口拍板的权威设计文档)实现独立
`auction` 服务,落在 economy 域(与 trade、inventory 同级)。第 16 个 Go 业务服。
解决 trade「两阶段点对点交易」覆盖不到的「全服挂单 + 价格撮合」场景。无新第三方依赖。

### 为什么独立服而非塞进 trade

- trade 是「买卖双方先约定再两阶段确认」的点对点模型;拍卖行是「卖方挂单进全服订单簿 →
  任意买方按价撮合」的交易所模型,撮合循环 / 订单簿 / 价格时间优先级与 trade 状态机正交。
- 撮合是「每 market 单写者」串行语义,与 trade 的 per-order 乐观锁是不同并发模型,合到一起
  会互相污染。决策文档拍板拆独立服。

### 端口 / 资源(决策文档固定)

- gRPC **:50016** / HTTP **:51016**(HTTP 仅 `/metrics`,auction.proto 无 google.api.http 注解)
- proto package `pandora.auction.v1`;errcode 段 **12000-12999**
- MySQL 库 `pandora_auction`(权威订单 + 成交,ShardSet 按 market_id 分片,W1 单库可跑)
- kafka topic `pandora.auction.match`(成交事件,key=match_id)+ `pandora.auction.audit`
  (挂单流转审计,key=order_id)
- Redis ZSET 订单簿 `pandora:auction:book:{<market_id>}:ask` / `:bid`(hashtag 锁同 slot)

### proto [proto]

`proto/pandora/auction/v1/auction.proto`:`AuctionService` 5 RPC(`PlaceOrder` 卖 /
`Bid` 买 / `CancelOrder` / `ListMarket` / `ListMyOrders`);enum `OrderSide`(SELL=1/BUY=2)、
`AuctionOrderStatus`(OPEN=1/PARTIALLY_FILLED=2/FILLED=3/CANCELED=4/EXPIRED=5);客户端可见
结构 `AuctionOrder`、kafka 结构 `AuctionMatchEvent`。所有写 RPC 带 `idempotency_key`;
owner/buyer 一律以 JWT ctx 的 player_id 为准(不信客户端请求体)。已 `proto_gen.ps1` 重生
go pb(37 files,buf lint OK)。cpp pb 待 Codex 同步 UE 仓库。

### errcode(双向同步)

`pkg/errcode/errcode.go` + `proto/pandora/common/v1/errcode.proto` 加段 12000-12999:
`ERR_AUCTION_ORDER_NOT_FOUND=12001` / `ERR_AUCTION_WRONG_STATE=12002` /
`ERR_AUCTION_NOT_OWNER=12003` / `ERR_AUCTION_INSUFFICIENT=12004` /
`ERR_AUCTION_IDEMPOTENCY_CONFLICT=12005`。已 regen。

### MySQL

`deploy/mysql-init/01-create-databases.sql` 加建 `pandora_auction` 库 + GRANT;
新 `deploy/mysql-init/09-auction-tables.sql`:
- `auction_orders`(PK order_id,uk `owner_id`+`idempotency_key` 挂单幂等键,idx
  market/side/status + owner/status)
- `auction_matches`(PK match_id,idx market/time + sell_order / buy_order)
- 雪花 ID 用 BIGINT UNSIGNED(§9.11),配置 ID(item_config_id / market_id)用 INT UNSIGNED(§9.12)

### 撮合引擎设计(核心)

- **每 market 单写者串行**:`AuctionUsecase` 持 `locks map[uint32]*sync.Mutex` 条带锁
  (per-market mutex),`submit` 全程持该 market 锁,撮合 + 落簿原子串行,杜绝超卖。
  选 mutex 而非 goroutine+channel:功能等价的串行化 + 更易单测;跨实例一致性哈希路由
  (同 market 落同实例)留扩容后续。
- **价格时间优先级用 ZSET 编码**:ask 簿 score=price(ZRANGE 升序取最低价)、bid 簿
  score=-price(ZRANGE 升序取最高价);member = 零填充 20 位 order_id(snowflake 时序 →
  字典序=时间序),同价取最早单。避开「价格+时间合成单 score」的 float64 精度损失。
- **撮合价 = 簿上被动单价格**(passive order price,挂单方让价),符合交易所惯例。
- **两层幂等**(不变量 §2 / §7):① 挂单 `ClaimOrder` 用 `owner_id+idempotency_key` 唯一键,
  命中重复 → 读回已存在单做指纹比对(market/side/item/quantity/price 一致 → 幂等回放返回
  原单;不一致 → `ErrAuctionIdempotencyConflict`,防 key 复用);② 结算 `RecordMatch` 用
  match_id 唯一键,同一撮合只落库一次。
- **结算解耦**:`SettlementLedger.Settle(match)` 接口(W1 `NoopSettlementLedger` 占位,
  接 inventory 货币/道具原子扣减后替换);`AuctionEventPusher`(PushMatch+PushAudit)弱依赖,
  kafka 不通仅 Warn。
- **存储/客户端结构分离**(§5.10 / §14):`OrderRecord` / `MatchRecord` 是 data 内部 struct
  (含 idempotency_key 等存储独有字段),不外泄;service 层从 record 组装客户端可见
  `AuctionOrder`。

### 服务骨架(services/economy/auction/)

- `go.mod`(module `.../services/economy/auction`,replace `../../../{pkg,proto}`;
  依赖集并自 trade+inventory:redis+kafka+mysql+miniredis,`go mod tidy` 完成)
- `internal/conf`(AuctionConf:MaxQuantityPerOrder / MaxPrice / DefaultListLimit /
  MaxListLimit + Defaults 端口 50016/51016)
- `internal/data/auction_repo.go`(`AuctionRepo` 接口 + `MySQLAuctionRepo`;`DBRouter`
  抽 `SingleDB`/`ShardedDB` 按 market_id 分片;ClaimOrder INSERT→1062→读回指纹比对)
- `internal/data/book.go`(`BookStore` 接口 + `RedisBookStore`,ZSET 订单簿)
- `internal/biz/auction.go`(撮合引擎:submit/match/crosses/opposite,per-market 条带锁)
- `internal/service/auction.go`(实现 `AuctionServiceServer`,callerID 从 ctx,errcode→proto)
- `internal/server/{grpc,http}.go`(grpc `pmw.AuthOptional()` + 注册;http 仅 /metrics)
- `cmd/auction/main.go`(MySQL ShardSet/单库 + Redis 强依赖 Ping + snowflake + kafka 双
  producer 弱依赖 + NoopSettlementLedger,装配 → Kratos Run)
- `etc/auction-dev.yaml`(:50016/:51016,mysql pandora_auction:3307,redis 6380,kafka 9093)

### 接线 / 文档登记

- `go.work` 加 `use ./services/economy/auction`
- `deploy/prometheus/prometheus.yml` 加 auction 51016 target
- `tools/scripts/run_services.ps1` 加 auction(Port 50016,Profiles all),服务数注释 15→16
- `deploy/envoy/envoy.yaml` 加 auction jwt_authn rule + route(15s)+ STRICT_DNS h2c cluster
  (:50016,客户端面 :8443)
- `docs/design/infra.md`(pandora_auction 库 + 两表 + 两 topic + 端口 + Redis 订单簿键)、
  `proto-design.md`(errcode 段 12000-12999 + 两 topic)、`pandora-arch.md`(§服务树 +
  §11 决策行)、`go-services.md`(服务总览第 16 行 + inventory 补登第 15 行 + 计数 14→16)

### 验证

- auction 模块 BUILD=0 / VET=0 / TEST=0:
  ```pwsh
  Push-Location services/economy/auction; go build ./...; go vet ./...; go test ./... -count=1; Pop-Location
  ```
- biz 7 单测(全内存 fake + miniredis 真 RedisBookStore):无对手挂单落簿 / 全额撮合 /
  并发不超卖(1000 单序 snowflake)/ 挂单幂等回放 / 价格时间优先级 / 部分成交 / 撤单。
- `pkg/errcode` + `proto` BUILD=0(errcode 段加常量不破坏其它服务)。

### 待办 / 阶段限制

- **Codex 收尾**:cpp pb 同步 UE 仓库 `Source/Pandora/Generated/Proto/`([proto]);
  `go mod tidy` 已跑但 go.sum 固化以 Codex 复核为准;git 收尾等用户「帮我 commit」
  (建议 scope `feat(auction): 全服拍卖行撮合引擎 [proto]`,AGENTS.md §11.1,Claude 不做 git)。
- **结算占位**:`NoopSettlementLedger` 总成功;接 inventory 货币/道具原子扣减 + 补偿幂等
  后替换(不变量 §7)。
- **单实例串行**:per-market 条带锁仅在单实例内成立;多实例部署需一致性哈希把同 market
  路由到同实例(或换 Redis 单写者 token),留扩容后续。
- **真 MySQL + Kafka 联调**:环境启停交 Codex / 人(§11.1),本轮单测全内存 fake,未跑真依赖。
