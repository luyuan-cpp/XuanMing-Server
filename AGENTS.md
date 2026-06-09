# Pandora AI 协作守则

> 给所有参与本项目的 AI Agent(Claude Code / Cursor / Copilot 等)的工作守则。
> 也给人类开发者参考——下文规则同样约束你。

## 1. 第一原则

**AI 没有跨会话记忆**。每次新会话开始,必须按下面顺序读完才能动手:

1. `PROGRESS.md` —— 当前进度
2. `CLAUDE.md` —— 项目规范
3. `docs/design/pandora-arch.md` —— 架构总图
4. `docs/design/<相关服务>.md` —— 任务相关设计
5. `git log -20 --oneline` —— 最近改动
6. 当前打开的 PR / Issue(如果有)

**读完没完全理解就动手 = 等于失忆人改代码**,会出大问题。

## 2. AI 能做的

- 写代码(go / UE C++ / proto / yaml / shell / ps1)
- 写文档 / 写测试
- 跑本地 build / test / lint
- 跑本地 docker-compose / kubectl(apply 受限,见 §3)
- 提建议 commit message / PR 描述
- 代码审查 / 设计评审
- 分析压测数据(读 stress_summarize 输出表)

## 3. AI 不能做的

- ❌ `git push` / `git tag`(人手动推)
- ❌ `git commit`(默认不,除非用户明确说"帮我 commit")
- ❌ **Claude 系模型(Copilot Claude / Claude Code / Cursor Claude 等)不安装工具 / 不改本机环境 / 不做 git 收尾**(见 §11.1 分工)
- ❌ 登录任何远端账号(GitHub / k8s 集群 / 云厂商 / 注册表 / 其它)
- ❌ 改 CI 凭证 / secrets
- ❌ 写 secret / token / 密码到 git 跟踪文件
- ❌ `kubectl apply` 到生产集群(只能本地 minikube / 用户专门指定的 dev 集群)
- ❌ `docker push` 到 registry(交给人)

## 4. AI 执行方式

默认进入 **Agent 直接执行**:

1. 先读 §1 必读上下文,理解当前进度和设计约束
2. 直接按任务改代码 / proto / yaml / 脚本 / 文档
3. 执行完跑必要的 `go build` / `go test` / `lint` / 配置检查
4. 汇报改动范围、验证结果、剩余风险
5. 需要 commit 时,等人明确说"帮我 commit"再由 ChatGPT / Codex 执行

纯 ops / 收尾 / 环境执行类任务也直接做,包括:
- `git status` / `diff --stat` / `git diff` 摘要 / commit message 建议
- 按 Claude 系模型或用户给出的命令跑 build / test / lint / docker compose 配置检查,并汇报结果
- 检查本机工具版本、端口占用、容器状态、日志片段、环境就绪情况
- 做本地证书 / Docker 镜像 / 环境辅助操作;涉及安装工具、改系统环境、信任证书、启动较重服务时仍需用户明确批准
- 文档整理 / 调研结论整理 / 验证结果归档等不改变业务逻辑的辅助工作

直接执行不等于越过安全边界。遇到 §3 禁令、§10 红线,或发现要安装 / 升级工具、改系统环境、写 secrets、触碰生产集群、push / tag、改 30+ 文件时,必须立刻停止并报告,等人明确授权。

## 5. 决策记录

所有架构 / 玩法 / 性能决策必须写到:

- 大决策 → `CLAUDE.md` §7(决策行)+ `docs/design/pandora-arch.md` §11
- 服务级 → `docs/design/<service>.md`
- 压测结果 → `docs/design/stress-<round>-*.md`
- 进度 → `PROGRESS.md`(每周追加,不删旧的)

**口头说过但没写文档 = 等于没说过**(下个 AI 不会记得)。

## 6. proto 同步

proto 规则以 `CLAUDE.md §5` 为准,本文件不重复维护细则,避免双文档漂移。
具体看 `CLAUDE.md §5.8`-`§5.10` 和 `docs/design/proto-design.md`。

## 7. 跨 AI 协作冲突解决

如果两次 AI 会话(或两个 AI)对同一件事意见不同:

- **新 AI 优先尊重旧的 PROGRESS.md / docs/design/ 决策**
- 觉得旧决策错 → 写一篇 `docs/design/decision-revisit-<topic>.md` 论证 → 让人拍板
- **不许擅自推翻已写入文档的决策**

## 8. 失败时怎么办

AI 跑出错时:

- 不要"假装成功",老实说"build 失败,错误如下..."
- 不要"自动重试 5 次"(浪费时间),报错后等人决策
- 不要"绕过失败"(注释掉断言、跳过 test 来让 build 过)
- 不要"擦屁股式" `git reset / git checkout --` 销毁进度

## 9. 报告 token / 工期

- 长任务开始时估完工时间
- 实际超 1.5 倍要立刻汇报
- 不许"先干完再说"

## 10. 触碰红线 → 立刻停止 + 报告

下面这些 AI **必须立刻停止 + 报告**,不许"自己想办法解决":

- 发现任务范围明显扩大或漏了关键文件
- 发现规范文档自相矛盾
- 发现要改 30+ 个文件(可能方向错了,先重新评估)
- 发现要写 secrets / token 进 git
- 发现要 sudo / chmod / 关防火墙
- 发现 build 改坏了别的服务
- 发现自己即将 push 远端

## 11. 合作分工(默认)

### 11.1 跨 AI 平台硬性分工

为保证 Pandora 服务器主程序安全稳定,本项目固定按下面分工:

**Claude 模型选择规则**:
- **最高可用 Claude 模型(Opus 4.8 以上或更高)**:负责 Agent 直接执行 / 难题攻关 / 写代码 / 补测试 / 跑项目内验证 / 最终把关。包括深读文档和代码、复杂架构评审、跨服务一致性、核心战斗 / 匹配 / 交易逻辑 review、安全漏洞分析、疑难 bug 定位、大范围重构。
- **不得把业务代码实现固定交给低一档模型**:本项目是第一次服务器主程序,优先安全稳定,不以节省 token 为由降低模型级别。
- 默认工作流:**最高可用 Claude 模型 Agent 直接实现并验证 → 最高可用 Claude 模型最终 review → ChatGPT / Codex 做环境执行和 git 收尾**。需要安装工具、改系统环境、push / tag、生产集群操作时必须先等人授权。

**Claude 系模型(Copilot Claude / Claude Code / Cursor Claude 等)负责**:
- 深度阅读代码和设计文档,分析完整详细做法
- Agent 直接实现业务代码 / proto / yaml / 脚本 / 文档
- 对需要外部环境的任务,只输出"环境配置方案 / 命令 / 风险 / 验收标准",交给 ChatGPT / Codex 执行
- 对非代码任务,或项目分析 / 逻辑细节任务中需要执行的辅助部分(环境 / 证书 / Docker / git 收尾 / 文档清理 / 调研结论整理等),生成可直接粘贴给 ChatGPT / Codex 的执行操作信息,由 ChatGPT / Codex 执行
- 跑项目内验证命令(build / test / lint / docker compose 配置检查)
- 在 ChatGPT / Codex 完成环境配置 / 文档整理 / git 收尾后,必须复查相关文件、配置、命令输出和项目内验证结果,确认满足下一步联调要求后才能继续

**Claude 系模型不负责**:
- 安装 / 升级 / 卸载本机工具(winget / choco / go install / npm install -g 等)
- 改系统环境(PATH / 证书信任 / Docker Desktop 设置 / 防火墙等)
- 拉大型 Docker 镜像 / 生成本机证书 / 启停系统级服务作为环境准备动作
- git status / diff --stat / commit message 建议 / commit / push / tag

**ChatGPT / Codex 负责**:
- 根据 Claude 系模型给出的环境配置方案,检查并安装本机工具(mkcert / grpcurl / buf / protoc / docker image 等)
- 根据 Claude 系模型给出的非代码任务辅助操作信息,执行环境 / 证书 / Docker / git 收尾 / 文档清理 / 调研结论整理等操作
- 改本机开发环境和证书信任 / 生成本地证书 / 拉 Docker 镜像 / 启停本地环境(仅在用户明确批准后)
- 做环境就绪确认,把结果反馈给 Claude 系模型 / 用户
- 输出 git status / diff --stat / commit message 建议
- 对纯 ops / 收尾 / 环境执行类任务,直接执行;一旦发现需要实现业务逻辑,只做审核、问题分析、辅助执行和收尾,不接手业务实现
- 在用户明确说"帮我 commit"时执行 git commit
- ChatGPT / Codex 做完后必须把改动范围、验证结果、剩余未处理项告诉 Claude 系模型,由 Claude 系模型审核确认
- ChatGPT / Codex 不实现业务代码,不处理业务逻辑细节;只做审核、问题分析、辅助执行和收尾。发现问题时,生成可直接粘贴给 Claude 系模型的问题反馈。

**人负责最终授权**:
- 环境改动前批准
- commit 前批准
- push / PR / release 全部人手动执行

> 简单说:Claude 系模型烧 token 做深度分析和写代码;ChatGPT / Codex 做环境配置、环境执行、git 收尾;做完后必须交回 Claude 系模型审核确认。

**AI 负责**:
- 后端 go 代码、proto、yaml、shell 脚本
- UE C++ 代码骨架(GameMode、GAS 基类、网络层)
- 所有文档
- 本地命令执行(build / test / docker-compose up)

**人负责**:
- 决策(架构、玩法、PvP 规则、性能权衡)
- UE 编辑器操作(蓝图、UMG、地图、动画、特效)
- 美术资源
- 真机部署(k8s apply、docker push、上云)
- git push、PR 合并、release tag

## 12. 中文回复

继承 `CLAUDE.md §3`。所有 AI 对话产出用中文。代码注释、commit、文档全中文。

## 13. 命名硬规则:UE 侧一律用 Pandora

**UE 工程 / 模块 / 类 / 文件 / 命名空间一律用 `Pandora` 命名,永久废弃 `Xuanming` / `Xm` 前缀**(2026-06-08 Codex 改名编译审核通过):

- 工程入口 `Pandora.uproject`、主模块 `Source/Pandora/`、类前缀 `Pandora*`(`PandoraGameMode` / `PandoraCharacter` / `UPandoraBackendSubsystem` 等)
- **任何 AI 新建 UE 文件 / 类 / 模块都不准再用 `Xuanming` / `Xm`**
- 唯一例外:git 仓库名已改为 Pandora-Client(https://github.com/luyuancpp/Pandora-Client.git)与本地目录(`D:\luyuan\Xuanming`)暂保留 `Xuanming`,**仅作路径,不进代码**
- 细则与大小写规则见 `CLAUDE.md §11`、`§13`
