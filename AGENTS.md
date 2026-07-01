# Pandora AI 协作守则

> 给本项目所有 AI Agent(Claude Code / Cursor / Copilot 等)的工作守则,人类开发者同样遵守。

## 1. 第一原则

**AI 没有跨会话记忆**。每次新会话动手前必须按序读完:

1. `PROGRESS.md` —— 当前进度
2. `CLAUDE.md` —— 项目规范
3. `docs/design/pandora-arch.md` —— 架构总图
4. `docs/design/<相关服务>.md` —— 任务相关设计
5. `git log -20 --oneline` —— 最近改动
6. 当前打开的 PR / Issue

**没读懂就动手 = 失忆人改代码**,会出大问题。

## 2. AI 能做的

写代码(go / UE C++ / proto / yaml / shell / ps1)/ 文档 / 测试;跑本地 build/test/lint;跑本地 docker-compose / kubectl(apply 受限,见 §3);建议 commit message 与 PR 描述;代码审查 / 设计评审;分析 stress_summarize 输出表。

## 3. AI 不能做的

- ❌ `git push` / `git tag`(人手动推);`git commit`(除非用户明说"帮我 commit")
- ❌ **Claude 系模型不装工具 / 不改本机环境 / 不做 git 收尾**(见 §11.1)
- ❌ 登录任何远端账号(GitHub / k8s / 云厂商 / 注册表);改 CI 凭证 / secrets
- ❌ 写 secret / token / 密码到 git 跟踪文件
- ❌ `kubectl apply` 到生产(只能本地 minikube / 指定 dev 集群);`docker push` 到 registry

## 4. AI 执行方式

默认 **直接执行**:读 §1 → 改代码/proto/yaml/脚本/文档 → 跑 build/test/lint → 汇报改动范围+验证+剩余风险 → 需 commit 时等人发话再由 Codex 执行(分工见 §11.1)。

遇 §3 禁令、§10 红线,或要装/升级工具、改系统环境、写 secrets、碰生产、push/tag、改 30+ 文件 → **立刻停止报告**,等授权。

**接线做最终版,不留半成品**(细则见 `CLAUDE.md §14`):新功能一次接到可上线版本,不准 TODO 占位 / 空实现;允许配置开关默认关闭(如 `snowflake.node_id_source` 默认 static),但开关打开后的分支必须是完整真实实现。引入隔离重依赖的独立 pkg module(如 `pkg/snowflake/etcdnode` 的 etcd client)到服务时,Claude 写代码 + 补 go.mod 的 require/replace,`go mod tidy` 生成 go.sum 由 Codex 执行(§11.1);接线后必须在交接里**列出需 tidy 的服务清单**,不准默声留着让下个 AI 撞 build 红。

## 5. 决策记录

- 大决策 → `docs/design/pandora-arch.md` §11
- 服务级 → `docs/design/<service>.md`
- 压测结果 → `docs/design/stress-<round>-*.md`
- 进度 → `PROGRESS.md`(每周追加,不删旧的)

**没写文档 = 没说过**(下个 AI 不会记得)。

## 6. proto 同步

以 `CLAUDE.md §5`(尤其 §5.8-§5.10)和 `docs/design/proto-design.md` 为准,本文件不重复细则。

## 7. 跨 AI 冲突解决

- **新 AI 默认尊重旧的 PROGRESS.md / docs/design/ 决策**
- 有更优/更标准/更安全方案可提推翻,但须先写 `docs/design/decision-revisit-<topic>.md`(旧问题/新方案/风险/迁移成本/验收标准),人拍板后再改

## 8. 失败时怎么办

不"假装成功"(老实报错)、不自动重试 5 次(报错后等决策)、不绕过失败(注释断言/跳 test)、不擦屁股式 `git reset / checkout --` 销毁进度。

## 9. 报告 token / 工期

长任务开始时估完工时间;实际超 1.5 倍立刻汇报;不许"先干完再说"。

## 10. 触碰红线 → 立刻停止 + 报告

任务范围明显扩大或漏关键文件 / 规范文档自相矛盾 / 要改 30+ 文件 / 要写 secrets 进 git / 要 sudo / chmod / 关防火墙 / build 改坏别的服务 / 即将 push 远端 / **破坏不停服更新(给 Redis pb 存储改字段编号·类型·语义、read-modify-write 路径丢弃 unknown fields、设计「必须停服才能上线或读数据」的方案)**(见 `CLAUDE.md` §9 不变量 16/17、`docs/design/zero-downtime-update.md`)。

## 11. 合作分工

### 11.1 跨 AI 平台硬性分工

首版服务器主程序优先安全稳定,**不以省 token 为由降级模型**(业务代码不固定交给低一档模型)。

| 角色 | 负责 | 不负责 |
|---|---|---|
| **最高可用 Claude**(Opus 4.8+) | 实现+验证业务代码/proto/yaml/脚本/文档;深读代码与设计;架构/安全/跨服务一致性/核心战斗-匹配-交易/疑难 bug review;项目内验证(build/test/lint/compose);最终把关 | 装/升级/卸载工具;改系统环境(PATH/证书/Docker/防火墙);拉大镜像/生成证书/启停系统级服务;git status/diff/commit/push/tag |
| **ChatGPT / Codex** | 按 Claude 方案做环境配置/工具安装/证书/Docker/就绪确认/文档清理/调研归档;查版本/端口/容器/日志;git status/diff/commit message 建议;经授权执行 commit;纯 ops 直接做,回报交 Claude 审核 | 不实现业务逻辑(需写逻辑时只做审核/问题分析,反馈给 Claude) |
| **人** | 决策(架构/玩法/PvP/性能);UE 编辑器(蓝图/UMG/地图/动画/特效);美术;真机部署(k8s apply/docker push/上云);git push/PR 合并/release tag;环境改动与 commit 前授权 | — |

工作流:**Claude 实现+验证 → Claude review → Codex 环境执行+git 收尾(回报)→ Claude 复查**。装工具/改环境/信任证书/启重服务/push/tag/生产操作前等人批准。

## 12. 中文回复

继承 `CLAUDE.md §3`:所有对话产出、注释、commit、文档全中文。

## 13. 命名硬规则:UE 侧一律用 Pandora

**UE 工程 / 模块 / 类 / 文件 / 命名空间一律 `Pandora`,永久废弃 `Xuanming` / `Xm`**(2026-06-08 Codex 改名编译审核通过):

- 入口 `Pandora.uproject`、主模块 `Source/Pandora/`、类前缀 `Pandora*`
- 新建 UE 文件 / 类 / 模块不准再用 `Xuanming` / `Xm`;历史路径名仅作记录,不进代码
- 细则见 `CLAUDE.md §11`、`§13`
