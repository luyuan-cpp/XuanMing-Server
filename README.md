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
├── proto/                 # 协议(全新设计,不复用 mmorpg)
├── login/                 # 13 个 go 服务(W1 仅骨架,W2+ 实现)
├── player/
├── data_service/
├── team/
├── matchmaker/
├── ds_allocator/
├── hub_allocator/
├── battle_result/
├── trade/
├── dialogue/
├── chat/
├── friend/
├── player_locator/
├── deploy/                # docker-compose / k8s / Agones yaml
├── tools/scripts/         # 开发与压测脚本
├── docs/design/           # 架构与设计文档(必读)
└── robot/                 # 压测客户端
```

## 必读文档

新人来到本项目,**第一周阅读顺序**:

1. [`CLAUDE.md`](./CLAUDE.md) — 项目宪法(规范、压测纪律、不变量)
2. [`docs/design/pandora-arch.md`](./docs/design/pandora-arch.md) — 总架构图与玩家流转
3. [`docs/design/go-services.md`](./docs/design/go-services.md) — 13 个 go 服务的职责边界
4. [`docs/design/ds-arch.md`](./docs/design/ds-arch.md) — UE DS(Hub / Battle)架构
5. [`docs/design/infra.md`](./docs/design/infra.md) — MySQL / Redis / Kafka / etcd 命名规范
6. [`docs/design/proto-design.md`](./docs/design/proto-design.md) — 协议设计
7. [`docs/design/pkg-copy-from-mmorpg.md`](./docs/design/pkg-copy-from-mmorpg.md) — 公共框架来源
8. [`docs/design/stress-discipline.md`](./docs/design/stress-discipline.md) — 压测纪律(继承 mmorpg §8/§9)
9. [`docs/design/pvp-rules.md`](./docs/design/pvp-rules.md) — PvP 规则待定项
10. [`AGENTS.md`](./AGENTS.md) — AI 协作守则
11. [`PROGRESS.md`](./PROGRESS.md) — 当前进度

## 快速启动(待 W1-D2 完成后填充)

```powershell
# 1. 启动基础设施(MySQL / Redis / Kafka / etcd)
pwsh tools/scripts/dev_up.ps1

# 2. 编译所有服务
pwsh tools/scripts/build.ps1

# 3. 启动开发模式
pwsh tools/scripts/dev_start.ps1
```

## 关联仓库

- **后端(本仓库)**:`https://github.com/luyuancpp/Pandora.git`
- **UE 客户端 + DS**:(待定,暂用 `Pandora-Client` 占位)

## License

MIT,见 [LICENSE](./LICENSE)。
