# Pandora 项目规范

> 本文档是 Pandora 项目的"宪法",AI 协作和人类开发都必须遵守。
> 继承 mmorpg CLAUDE.md 的纪律,适配 MOBA 玩法 + UE DS + 双仓库架构。

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
F:/work/mmorpg/                 # ⚠️ 封存项目,只读参考,严禁修改
```

**永远不要修改 `F:/work/mmorpg/` 下的任何文件**,那是封存项目。
**允许从 mmorpg 拷代码**(D2 一次性拷,之后两边独立演化),拷贝清单见 `docs/design/pkg-copy-from-mmorpg.md`。

## 3. 中文回复

所有 AI 协作产出**用中文**。注释、commit message、文档全中文。

## 4. 提交纪律

1. 不准在没有跑通 **所有已启用 module 的构建** 的情况下 commit
   - 本项目采用 `go.work` 多 module 模式,仓库根没有 `go.mod`,**不能**在根目录跑 `go build ./...`
   - 当前阶段（W2 ⑤ 后）：验证命令为 `go build ./pkg/... ./proto/... ./services/account/login/... ./services/runtime/push/...`
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

1. proto 只在 **`Pandora` 后端仓库**改
2. 改完跑 `pwsh tools/scripts/proto_gen.ps1` 生成 go pb
3. 同时生成 cpp pb 推送到 UE 仓库的 `Source/Pandora/Generated/Proto/`(CI 自动 PR)
4. UE 客户端改动跟在后端 PR 之后合并
5. 字段编号**永不复用**,只能 deprecate(`reserved 5;` + 注释原因)

## 6. 服务命名 / 端口规范

详见 [`docs/design/infra.md`](./docs/design/infra.md)。**不允许 ad-hoc 起端口或 key**。

## 7. 当前里程碑(决策行)

| Round | 日期 | 关键决策 / 数据 |
|---|---|---|
| R0 | 2026-06-03 | 立项,推倒 mmorpg,新建 Pandora 项目 |
| R0 | 2026-06-03 | 大厅 DS 化,500 人/实例,全图自由 PvP |
| R0 | 2026-06-03 | UE 5.7 + Iris + GAS,Agones 调度 |
| R0 | 2026-06-03 | 双仓库:后端 Pandora,UE 独立仓库 |
| R0 | 2026-06-03 | License MIT,Go 1.23,基础设施全新 |
| R0 | 2026-06-03 | **后端框架继续用 go-zero**(复用 mmorpg 公共代码) |

后续每轮压测 / 大决策追加一行,**永不删旧行**。

## 8. 压测纪律(继承 mmorpg §8/§9)

详见 [`docs/design/stress-discipline.md`](./docs/design/stress-discipline.md)。**核心规则**:

- 跑测前必有 `prev-summary.txt`,否则不许开下一轮
- **跑测前清空** redis / mysql / etcd / kafka offset / k8s GameServer
- 至少 3 次 prom snapshot:ramp 完成 / 稳态中段 / 稳态末
- summarize 脚本输出五段二维表,**不许手 grep raw prom**
- **没有对比表不许声明"性能提升"**
- 压期间不上传日志
- **每次登录压测把所有 redis/mysql/etcd 数据全部删除再开新一轮**(对应 mmorpg §9.6)

## 9. 不变量(数据一致性 / 安全)

跨服务必须保持的不变量。任何改动违反这些 → PR review 直接拒。

1. **玩家在线只能在一个 DS**(player_locator 强制)
2. **战斗结果幂等**(同一 match_id 只落库一次)
3. **DS 票据短时效**(JWT exp 5min)
4. **DS 崩溃必有补偿**(15s 心跳超时 → abandoned → 段位回滚)
5. **proto 字段编号永不复用**
6. **MMR 计算在 battle_result**(DS 不可信)
7. **交易资源扣减必须原子 + 有补偿幂等键**
8. **所有写都要带 trace_id**
9. **kafka topic key = 业务实体 ID**(同一玩家 / 同一对局事件有序)
10. **Redis lock TTL ≤ 30s**,业务跑完主动释放

## 10. AI 协作约定

详见 [`AGENTS.md`](./AGENTS.md)。核心:

1. **AI 没有跨会话记忆**,每次新会话先读 `PROGRESS.md` + `docs/design/*.md`
2. **AI 不操作远端仓库**(不 push、不改 GitHub settings、不登账号)
3. **AI 操作前先开 plan 模式**,列动作清单给人审,审过批量执行
4. **AI 不擅自删除文件**,删除请求必须人确认
5. **AI 写代码必须遵循本项目规范**(端口 / 命名 / 不变量 / 中文注释)
6. **跨 AI 分工硬规则**:Claude 系模型(Copilot Claude / Claude Code / Cursor Claude 等)负责深度分析 / plan / 改代码 / 项目内验证;ChatGPT / Codex 负责本机工具安装 / 环境准备 / 环境确认 / git status / diff --stat / commit message 建议 / 用户明确授权后的 commit。Claude 系模型不安装工具、不改系统环境、不做 git 收尾。

## 11. UE 工程约束(写给 UE 仓库的开发者参考)

1. 类前缀统一 `Pandora*`(GameMode / Character / PlayerController)
2. 服务端逻辑统一在 `PandoraHubServer` / `PandoraBattleServer` 模块,不在 `Source/Pandora/` 客户端模块
3. 蓝图只做"胶水"(挂技能动画 / UMG 绑定),逻辑在 C++
4. 资源走 Git LFS(`.uasset / .umap / .fbx / .png / .wav / .ogg`)
5. **永远不要在 git 里提交** `Binaries/ Intermediate/ DerivedDataCache/ Saved/`

## 12. 不要做的事

- ❌ 不要读 `F:/work/mmorpg/client/`(client 子目录始终不动,继承 mmorpg §9.7)
- ❌ 不要在 main 分支直接开发(走 feature/<name> + PR)
- ❌ 不要在 docs/design/ 之外随便建 README(集中维护)
- ❌ 不要 import 第三方 GUI 库到 go 服务(go 服务都是 headless gRPC)
- ❌ 不要把 player_id 当 prometheus label(高基数会爆)
- ❌ 不要在 W1 写业务逻辑,只搭骨架
- ❌ 不要混用 `Pandora` / `pandora` / `MOBA` / `moba` 命名 — 见 §2 大小写规则

## 13. 命名大小写规则(强制)

- **Pandora**(首字母大写):仓库名 / 本地路径 / 工程类前缀 / 文档项目名引用
- **pandora**(全小写):kafka topic / mysql / redis key / docker 镜像 / go module
- **MOBA**:仅描述游戏类型时使用("Pandora 是一款 MOBA"),**不能**指代项目本身
