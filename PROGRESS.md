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
| **D1** | 仓库骨架 + 11 份文档落盘 | 🟢 进行中(2026-06-03) |
| D2 | 拷 mmorpg pkg + docker-compose + dev_up.ps1 | ⏸️ |
| D3 | 写 .proto + buf 工具链 | ⏸️ |
| D4 | UE 仓库初始化(用户主导) | ⏸️ |
| D5-D6 | UE DS 骨架代码(HubGameMode / BattleGameMode + Agones SDK) | ⏸️ |
| D7 | k8s + Agones + 端到端 hello world | ⏸️ |

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
4. `git log -20 --oneline`(最近改动)
5. 当前打开的 PR(如果有)
