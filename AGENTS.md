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

- ❌ `git push` / `git tag`(人审过手动推)
- ❌ `git commit`(默认不,除非用户明确说"帮我 commit")
- ❌ **Claude 系模型(Copilot Claude / Claude Code / Cursor Claude 等)不安装工具 / 不改本机环境 / 不做 git 收尾**(见 §11.1 分工)
- ❌ 登录任何远端账号(GitHub / k8s 集群 / 云厂商 / 注册表 / 其它)
- ❌ 删除文件不经人确认
- ❌ 改 main 分支保护规则
- ❌ 改 CI 凭证 / secrets
- ❌ 修改 `F:/work/mmorpg/`(封存项目)
- ❌ 读 `F:/work/mmorpg/client/`(继承 mmorpg §9.7)
- ❌ 写 secret / token / 密码到 git 跟踪文件
- ❌ 在 main 直接 commit(走 feature 分支)
- ❌ `kubectl apply` 到生产集群(只能本地 minikube / 用户专门指定的 dev 集群)
- ❌ `docker push` 到 registry(交给人)

## 4. AI 写代码前必做

1. 开 **plan 模式**(EnterPlanMode),列文件清单和动作
2. 给人审
3. 审过(ExitPlanMode 被 approve)批量执行
4. 执行完跑 `go build` / `go test` / `lint` 验证
5. 跑 `git status` / `git diff --stat` 给人看
6. 等人 commit

**不要**直接动手,**不要**改超出 plan 范围的文件。

## 5. 决策记录

所有架构 / 玩法 / 性能决策必须写到:

- 大决策 → `CLAUDE.md` §7(决策行)+ `docs/design/pandora-arch.md` §11
- 服务级 → `docs/design/<service>.md`
- 压测结果 → `docs/design/stress-<round>-*.md`
- 进度 → `PROGRESS.md`(每周追加,不删旧的)

**口头说过但没写文档 = 等于没说过**(下个 AI 不会记得)。

## 6. proto 同步

详见 `CLAUDE.md §5`。

要点:
- proto 只在本仓库改
- 改完必须跑 `pwsh tools/scripts/proto_gen.ps1`
- commit message 加 `[proto]` 标记
- 字段编号永不复用

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

- 每个 plan 估完工时间
- 实际超 1.5 倍要立刻汇报
- 不许"先干完再说"

## 10. 触碰红线 → 立刻停止 + 报告

下面这些 AI **必须立刻停止 + 报告**,不许"自己想办法解决":

- 发现 plan 漏了关键文件
- 发现规范文档自相矛盾
- 发现要改 30+ 个文件(可能方向错了,先重新评估)
- 发现要写 secrets / token 进 git
- 发现要 sudo / chmod / 关防火墙
- 发现 build 改坏了别的服务
- 发现自己即将操作 `F:/work/mmorpg/`
- 发现自己即将 push 远端

## 11. 合作分工(默认)

### 11.1 跨 AI 平台硬性分工

为节省 Claude 系模型 token,本项目固定按下面分工:

**Claude 系模型(Copilot Claude / Claude Code / Cursor Claude 等)负责**:
- 深度阅读代码和设计文档,分析完整详细做法
- 开 plan 模式列文件清单 / 动作 / 风险 / 工期,给人审
- 对需要外部环境的任务,只输出"环境配置方案 / 命令 / 风险 / 验收标准",交给 ChatGPT / Codex 执行
- 审过后改代码 / proto / yaml / 脚本 / 文档
- 跑项目内验证命令(build / test / lint / docker compose 配置检查)
- 在 ChatGPT / Codex 配好环境后,基于项目命令复查并确认是否满足代码联调要求

**Claude 系模型不负责**:
- 安装 / 升级 / 卸载本机工具(winget / choco / go install / npm install -g 等)
- 改系统环境(PATH / 证书信任 / Docker Desktop 设置 / 防火墙等)
- 拉大型 Docker 镜像 / 生成本机证书 / 启停系统级服务作为环境准备动作
- git status / diff --stat / commit message 建议 / commit / push / tag

**ChatGPT / Codex 负责**:
- 根据 Claude 系模型给出的环境配置方案,检查并安装本机工具(mkcert / grpcurl / buf / protoc / docker image 等)
- 改本机开发环境和证书信任 / 生成本地证书 / 拉 Docker 镜像 / 启停本地环境(仅在用户明确批准后)
- 做环境就绪确认,把结果反馈给 Claude 系模型 / 用户
- 输出 git status / diff --stat / commit message 建议
- 在用户明确说"帮我 commit"时执行 git commit

**人负责最终授权**:
- 环境改动前批准
- commit 前批准
- push / PR / release 全部人手动执行

> 简单说:Claude 系模型烧 token 做深度分析和写代码;ChatGPT / Codex 做环境配置、环境执行、git 收尾;配好后再由 Claude 系模型用项目验证确认。

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
