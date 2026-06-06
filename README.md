# Pandora

> 一款 MOBA 类型游戏的后端工程。
> 客户端与 DS 工程在独立仓库(UE 5.7),本仓库只负责 go 后端 + proto + 部署 + 设计文档。

## 项目特点

- **5v5 MOBA 战斗**:固定 25 分钟一局,UE 战斗 DS 一局一进程
- **持续在线大厅**:UE 大厅 DS 常驻,500 人/实例,**全图自由 PvP**(玩家在大厅也能放技能、互打、对话 NPC、组队、交易)
- **基础设施**:MySQL 8 + Redis 7 + Kafka + etcd
- **DS 编排**:Agones on k8s

## 仓库结构

```
Pandora/
├── pkg/                   # Go 公共框架(log/metrics/grpc/kafka/redis lock 等)
├── proto/                 # 协议定义与生成产物
├── services/              # 14 个 go 服务(按 account/runtime/matchmaking/battle/social 等域分组)
├── deploy/                # docker-compose / k8s / Agones yaml
├── tools/scripts/         # 开发与压测脚本
├── docs/design/           # 架构与设计文档(必读)
└── robot/                 # 压测客户端
```

## 必读文档

新人来到本项目,**第一周阅读顺序**:

1. [`CLAUDE.md`](./CLAUDE.md) — 项目宪法(规范、压测纪律、不变量)
2. [`docs/design/pandora-arch.md`](./docs/design/pandora-arch.md) — 总架构图与玩家流转
3. [`docs/design/go-services.md`](./docs/design/go-services.md) — 14 个 go 服务的职责边界
4. [`docs/design/ds-arch.md`](./docs/design/ds-arch.md) — UE DS(Hub / Battle)架构
5. [`docs/design/infra.md`](./docs/design/infra.md) — MySQL / Redis / Kafka / etcd 命名规范
6. [`docs/design/proto-design.md`](./docs/design/proto-design.md) — 协议设计
7. [`docs/design/stress-discipline.md`](./docs/design/stress-discipline.md) — 压测纪律
8. [`docs/design/pvp-rules.md`](./docs/design/pvp-rules.md) — PvP 规则待定项
9. [`AGENTS.md`](./AGENTS.md) — AI 协作守则
10. [`PROGRESS.md`](./PROGRESS.md) — 当前进度

## 快速启动

### 1. 装开发工具链(首次)

一键装齐 buf / mkcert / grpcurl(已装的会自动跳过):

```powershell
pwsh tools/scripts/install_dev_tools.ps1
```

只检查不装:
```powershell
pwsh tools/scripts/install_dev_tools.ps1 -Check
```

强制重装:
```powershell
pwsh tools/scripts/install_dev_tools.ps1 -Force
```

工具版本锁定见脚本头部,**所有开发者用同一版本**避免环境漂移。

要自己装的(脚本不管):**Go 1.24+ / Docker Desktop / Git**。

### 2. 启动基础设施

```powershell
pwsh tools/scripts/dev_up.ps1
```

启动 MySQL 3307 / Redis 6380 / Kafka 9093 / etcd 2380 / Prometheus 9091 / Grafana 3001 / Envoy 8443(W2 起)。

### 3. 生成 proto 代码

```powershell
pwsh tools/scripts/proto_gen.ps1
```

第一次跑会从 buf.build 拉远程插件(`protocolbuffers/go` / `grpc/go`),需要外网。
Kratos HTTP 代码生成走本地 `protoc-gen-go-http`,需要先装 Go 并执行:

```powershell
go install github.com/go-kratos/kratos/cmd/protoc-gen-go-http/v2@latest
```

### 4. 编译 + 启动服务

```powershell
# 编译当前已启用 module(完整口径见 CLAUDE.md §4.1 / go.work)
go build ./pkg/... ./proto/... ./services/account/login/... ./services/account/player/... ./services/runtime/push/... ./services/runtime/player_locator/... ./services/matchmaking/team/... ./services/matchmaking/matchmaker/... ./services/battle/ds_allocator/... ./services/battle/battle_result/...

# 启动 login(W2)
go run ./services/account/login/cmd/login -conf services/account/login/etc/login-dev.yaml
```

### 5. 端到端验证

```powershell
# 直连 login(绕过 Envoy)
grpcurl -plaintext -d '{"account":"test","password_hash":"abc","device_id":"d1"}' `
  127.0.0.1:50001 pandora.login.v1.LoginService/Login

# 经 Envoy(模拟客户端 gRPC-Web 路径)
grpcurl -insecure -d '{"account":"test","password_hash":"abc","device_id":"d1"}' `
  localhost:8443 pandora.login.v1.LoginService/Login
```

## 关联仓库

- **后端(本仓库)**:`https://github.com/luyuancpp/Pandora.git`
- **UE 客户端 + DS**:(待定,暂用 `Pandora-Client` 占位)

## License

MIT,见 [LICENSE](./LICENSE)。
