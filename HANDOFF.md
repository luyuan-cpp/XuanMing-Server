# Pandora 接班手册

## §0 项目一句话

**Pandora** = MOBA(5v5)+ 持续在线大厅(500 人/实例,全图自由 PvP)。
后端 Go + Kratos,UE 5.7 客户端 + DS,Envoy + gRPC-Web 网关,Kafka + MySQL + Redis + etcd 基础设施。

---

## §1 铁律

### 1.1 客户端连接(2 条,锁死)

```
Client(UE 5.7)
├── ① UE NetDriver → Hub/Battle DS         仅游戏内同步 / GAS / Replication
└── ② FHttpModule → Envoy(8443 HTTPS)     gRPC-Web over HTTP/2 TLS
                                            业务请求 unary + 推送 server stream
```

- **Client 不走 gRPC 原生**(走 gRPC-Web,UE 自研基于 FHttpModule)
- **客户端零额外依赖**(不拉 grpc-cpp 80MB,不装第三方 UE gRPC 插件)
- **2 条连接,不是 3 条**(2026-06-04 推翻 gateway+push 分离方案)

### 1.2 后端框架

| 项 | 锁定值 |
|---|---|
| Go 框架 | **Kratos v2.9.2** |
| Go 版本 | go1.26.4 |
| Log | Kratos log + **zap** 实现 |
| Config | yaml + file source(W3+ 接 etcd) |
| Edge Gateway | **Envoy v1.38.0**(grpc-web filter) |
| 服务发现 | k8s Service + DNS(W3 + Kratos registry/etcd 可选) |
| Kafka client | sarama v1.43.1 |
| Redis client | go-redis/v9 v9.16.0 |
| Proto 工具 | **buf v1.70.0** |

### 1.3 协议铁律

- **UE 有的功能 proto 里不写**(GAS / Replication / ServerRPC 都不写 proto)
- **proto 不写战斗 tick 字段**(那是 UE Replication 的事)
- **Heartbeat 用 unary 每 5s**
- **Client 不直连 gRPC 业务服**(走 Envoy → Kratos)
- **DS 不兼任业务网关**

### 1.4 RPC 顺序与 Response 语义(4 协议原则)

详见 `docs/design/protocol-ordering-rules.md`:

1. **原则 1**:立即完成型 RPC 的 response 必须返完整业务数据(客户端不需要等 push)
2. **原则 2**:kafka push 不发给请求发起方(发起方看 response,避免 smell)
3. **原则 3**:已受理型 RPC(StartMatch / ConfirmMatch)显式标注,客户端 UI 状态机由 push 驱动
4. **原则 4**:每个 RPC 在 proto 注释里标注"立即完成"或"已受理"语义

### 1.5 服务目录布局

```
F:/work/Pandora/
├── services/
│   ├── account/      (login, player)
│   ├── social/       (friend, chat, dialogue)
│   ├── matchmaking/  (team, matchmaker)
│   ├── battle/       (ds_allocator, hub_allocator, battle_result)
│   ├── economy/      (trade)
│   ├── data/         (data_service)
│   └── runtime/      (player_locator, push)
```

Module 路径:`github.com/luyuancpp/pandora/services/<域>/<服务>`

### 1.6 命名规则

- **目录布局**:`proto/pandora/<domain>/v1/<file>.proto`
- **RPC 请求/响应类型**:`XxxRequest` / `XxxResponse`(不用 Req/Resp 缩写)
- **Package**:`pandora.<domain>.v1`
- **Service**:`<Name>Service`(LoginService / TeamService)
- **字段**:`snake_case`(player_id / created_at_ms)

### 1.7 大小写规则

- **Pandora**(首字母大写):仓库名 / 路径 / 文档项目名引用 / Go module 顶级名
- **pandora**(全小写):kafka topic / mysql / redis key / docker / go module path
- **MOBA**:仅描述游戏类型(不指代项目)

---

## §2 当前进度

接班 AI 必须自己根据 `PROGRESS.md`、代码目录和最近提交确认当前状态。
不要依赖本文记录的服务完成情况。
当前下一步见 §3。

---

## §3 当前下一步

### Step 1:W4 ⑤ — hub_allocator 服务实现

目标:

1. 基于已生成的 `proto/pandora/hub/v1/allocator.proto` 与 `HubShardStorageRecord` / `HubAssignmentStorageRecord` 落地 Kratos 服务
2. 接 Redis 维护 hub 分片镜像与玩家归属,容量默认 500 人/实例
3. 实现 `AssignHub` / `ReleaseHub` / `TransferHub` / `ListHubs` / `Heartbeat`
4. login 调 hub_allocator 拿真实 `hub_ds_addr` + hub ticket
5. 当前无真实 Agones/Hub DS 时可先用 mock seed hub,但协议和存储边界必须按最终形态写

### Step 2:可靠补偿收口

- 修 W4 ③ 已记录的阶段限制:`ds.lifecycle` / `player.update` 仍是 best-effort,需要 outbox、待补偿队列或 battle_result 对账路径三选一
- 目标是让 `CLAUDE.md §9.4 DS 崩溃必有补偿` 从"Kafka 正常时成立"升级为可靠闭环

### Step 3:UE 主链路

- UE 客户端 grpc-web(FHttpModule 自研解析)接 Envoy
- UE Hub DS / Battle DS 骨架 + GAS / Iris / Agones 联调
- 打通登录 → 进大厅 → 匹配 → 进战斗 → 结算 → 回大厅

### 明确暂缓

`friend`(:50004) 和 `chat`(:50005) 现在不做;保留 proto / 端口 / topic 规划,等 UE 与核心链路全部完成后,再作为社交尾部功能实现。

---

## §4 接班 AI 工作守则

### 4.1 必读文档

1. `CLAUDE.md`
2. `AGENTS.md`
3. `docs/design/pandora-arch.md`
4. `docs/design/gateway-decision.md`
5. `docs/design/infra.md`
6. `PROGRESS.md` 最新 W2 段落

### 4.2 工作流

- Claude / Agent 默认直接执行,不再要求先走前置流程。
- 编码 / 配置任务直接按 `AGENTS.md` 和设计文档约束实现。
- 跑项目内验证命令。
- 不做 git 收尾,把验证结果交给 ChatGPT / Codex。
- ChatGPT / Codex 做完环境 / 文档 / git 收尾后,Claude 必须审核相关产物和验证结果。
- 非代码任务,或项目分析 / 逻辑细节任务中需要执行的辅助部分,由 Claude 生成执行操作信息,用户复制给 ChatGPT / Codex 执行。
- 涉及安装工具、改系统环境、写 secrets、生产集群、push / tag、30+ 文件大改等红线时必须停止并等人授权。

### 4.3 跨 AI 分工

**Claude 系模型负责**:

- 深度分析
- Agent 直接执行
- 输出关键做法和验证结果
- 生成可直接粘贴给 ChatGPT / Codex 的非代码任务辅助执行操作信息
- 改代码 / proto / yaml / 脚本 / 文档
- 跑项目内验证
- 审核 ChatGPT / Codex 完成的环境配置、文档整理、git 收尾结果

**Claude 系模型不负责**:

- 安装工具
- 改系统环境
- 生成证书
- 拉 Docker 镜像
- 启停本机环境
- git status / diff / commit message / commit / push / tag

如果需要环境配置,Claude 只输出:

- 环境配置方案
- 命令
- 风险
- 验收标准

**ChatGPT / Codex 负责**:

- 本机工具和环境配置
- 执行 Claude 系模型生成的非代码任务辅助操作信息
- 证书 / Docker 镜像 / 本地环境启停
- 环境确认
- git status / diff / commit message 建议
- 用户明确授权后的 commit
- 完成后把改动范围、验证结果、剩余未处理项交给 Claude 系模型复查
- 不实现业务代码,不处理业务逻辑细节;只做审核、问题分析、辅助执行和收尾。发现问题时,生成可直接粘贴给 Claude 系模型的问题反馈。

### 4.4 失败和红线

- 不假装成功。
- 不跳过失败。
- 不绕过测试。
- 发现要改 30+ 文件、写 secret、push 远端、规范冲突时立即停止报告。

---

## §5 当前未决项

- ✅ UE 仓库名（D4，2026-06-08 解除）：**Xuanming** https://github.com/luyuancpp/Xuanming.git，本地 `D:\luyuan\Xuanming`（UE 5.7 源码版，M0–M1.5 FPS PoC 已完成：DS 联机 / 角色 / EnhancedInput / 武器 / MVVM HUD / GAS）
- ⏸️ k8s 选型:阿里云 ACK / 自建 / 先 minikube(D7 阻塞)
- ⏸️ Envoy 跑模式:k8s Ingress / 独立 Pod(D7 决定)
- ⏸️ JWT 鉴权细节(login 服务签发 + Envoy jwt_authn filter)(W3 写 login 时定)

---

## §6 关键文件索引

| 想了解什么 | 看哪个文件 |
|---|---|
| 当前进度 | `PROGRESS.md` |
| 项目规范 | `CLAUDE.md` / `AGENTS.md` |
| 总架构 | `docs/design/pandora-arch.md` |
| Envoy + gRPC-Web | `docs/design/gateway-decision.md` |
| 端口 / topic / 命名 | `docs/design/infra.md` |
| 服务清单 | `docs/design/go-services.md` |
| proto 源文件 | `proto/pandora/<domain>/v1/*.proto` |
| proto 生成脚本 | `tools/scripts/proto_gen.ps1` |
| docker compose | `deploy/docker-compose.dev.yml` |
| Prometheus 配置 | `deploy/prometheus/prometheus.yml` |
