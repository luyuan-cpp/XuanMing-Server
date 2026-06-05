# Pandora 项目接班手册 — 给下一个 AI

> **作用**:本文档是给**任何接班 AI**(GitHub Copilot Sonnet 4.7 / Claude Code / Cursor / 其它)的完整交接说明。
> 用户多次切换 AI 平台时,本文档保证零信息丢失。
>
> **接班 AI 第一件事**:读完本文档 → 按 §3 "立刻执行" 继续干。
> **接班 AI 第二件事**:严格遵守 §1 "铁律",不要再讨论已锁死的决策。

---

## §0 项目一句话

**Pandora** = MOBA(5v5)+ 持续在线大厅(500 人/实例,全图自由 PvP)。
后端 Go + Kratos,UE 5.7 客户端 + DS,Envoy + gRPC-Web 网关,Kafka + MySQL + Redis + etcd 基础设施。

仓库 `https://github.com/luyuancpp/Pandora`,当前分支 `codex/initial-scaffold`。

---

## §1 铁律(不能再推翻,任何 AI 想改先读 §6 反面教材)

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
| Go 框架 | **Kratos v2.9.2**(推翻 D2.1 go-zero) |
| Go 版本 | go1.26.4 |
| Log | Kratos log + **zap** 实现 |
| Config | yaml + file source(W3+ 接 etcd) |
| Edge Gateway | **Envoy v1.38.0**(grpc-web filter)|
| 服务发现 | k8s Service + DNS(W3 + Kratos registry/etcd 可选) |
| Kafka client | sarama v1.43.1 |
| Redis client | go-redis/v9 v9.16.0 |
| Proto 工具 | **buf v1.70.0** |

### 1.3 协议铁律

- **UE 有的功能 proto 里不写**(GAS / Replication / ServerRPC 都不写 proto)
- **proto 不写战斗 tick 字段**(那是 UE Replication 的事)
- **Heartbeat 用 unary 每 5s**(go-zero 不支持 stream 历史遗留,Kratos 支持但保留 unary 简化)
- **Client 不直连 gRPC 业务服**(走 Envoy → Kratos)
- **DS 不兼任业务网关**(见 `docs/design/architecture-rejected-strict-ds-only.md` 6 个后果)

### 1.4 RPC 顺序与 Response 语义(4 协议原则)

详见 `docs/design/protocol-ordering-rules.md`:

1. **原则 1**:立即完成型 RPC 的 response 必须返完整业务数据(客户端不需要等 push)
2. **原则 2**:kafka push 不发给请求发起方(发起方看 response,避免 smell)
3. **原则 3**:已受理型 RPC(StartMatch / ConfirmMatch)显式标注,客户端 UI 状态机由 push 驱动
4. **原则 4**:每个 RPC 在 proto 注释里标注"立即完成"或"已受理"语义

### 1.5 服务目录布局(已重构)

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

### 1.6 命名规则(2026-06-06 决策:全遵 buf STANDARD)

- **目录布局**:`proto/pandora/<domain>/v1/<file>.proto`(已重构,18 个 .proto 已 git mv)
- **RPC 请求/响应类型**:`XxxRequest` / `XxxResponse`(不用 Req/Resp 缩写)
- **Package**:`pandora.<domain>.v1`
- **Service**:`<Name>Service`(LoginService / TeamService)
- **字段**:`snake_case`(player_id / created_at_ms)

### 1.7 大小写规则

- **Pandora**(首字母大写):仓库名 / 路径 / 文档项目名引用 / Go module 顶级名
- **pandora**(全小写):kafka topic / mysql / redis key / docker / go module path
- **MOBA**:仅描述游戏类型(不指代项目)

---

## §2 当前进度(2026-06-06)

### 已完成 commit(在 `codex/initial-scaffold` 分支)

| commit | 内容 |
|---|---|
| `b4f6351` | W1-D1 仓库骨架 + 11 份设计文档 |
| `94045f0` | W1-D2 pkg/ + docker-compose 基础设施 |
| `635cf01` | W1-D3 协议骨架(18 个 .proto)+ 网关决策 |
| `6f582e7` | W1-D3 收尾 push.proto + UE gRPC 插件评估 |
| `e307dd8` | W2 ⓪+①+② Kratos 切换 + services/ 重构 + buf 工具链补全 |
| `ee12479` | W2 ②⁺ proto 全遵 buf STANDARD(目录 + 命名) |

### W2 阶段任务清单

| 任务 | 状态 | 说明 |
|---|---|---|
| W2 ⓪ services/ 目录重构 | ✅ commit `e307dd8` |
| W2 ① pkg/ 重写 go-zero → Kratos v2.9.2 | ✅ commit `e307dd8`(go build/vet/test 全绿) |
| W2 ② proto 工具链(buf + Kratos plugin) | ✅ commit `e307dd8` |
| W2 ②⁺ proto 全遵 buf STANDARD | ✅ commit `ee12479` |
| **W2 ③ login 服务(Kratos 第一个业务服)** | ✅ **2026-06-05 完成,待用户 commit**(见 PROGRESS.md "W2 ③") |
| W2 ④ Envoy v1.38.0 本地 docker | ⏸️ 可并行 |
| W2 ⑤ push 服务骨架(server stream) | ⏸️ 可开始,模板复用 login |
| W2 ⑥ 端到端 hello world 测试 | ⏸️ 等 ④⑤ |
| W2 ⑦ 收尾 + 文档同步 | ⏸️ 等所有 |

### 当前 git status(未 commit 改动)

18 个 `.proto` 已 `git mv` 但**还没改 import 路径 + 没改命名 + 没改 docs**:

```
R  proto/<X>/v1/*.proto → proto/pandora/<X>/v1/*.proto    (18 个文件)
```

工作树状态见 `git status --short`。

---

## §3 立刻执行(W2 ②⁺ 剩余 + W2 ③④⑤⑥⑦)

### Step 1:完成 W2 ②⁺ proto 全遵 STANDARD(优先级 P0)

#### 1.1 改 proto import 路径(18 个文件)

由于目录从 `proto/<X>/` 改成 `proto/pandora/<X>/`,所��跨文件 import 要改:

```diff
-import "common/v1/errcode.proto";
+import "pandora/common/v1/errcode.proto";
```

涉及 16 个 .proto(除 `common/` 4 个外都引 errcode)+ `pandora/login/v1/login.proto` 还引 `google/api/annotations.proto`(不变,这是 buf deps)。

**批量命令**:
```bash
cd F:/work/Pandora
find proto/pandora -name "*.proto" | xargs sed -i 's|import "common/v1/|import "pandora/common/v1/|g'
```

#### 1.2 改 RPC Req/Resp → Request/Response(只改 RPC in/out 类型)

⚠️ **不要乱改业务字段**(如 `request_id` 字段名不能动)。

按 lint 报错精确改(参考 `git log -1 e307dd8` 时贴的 lint 完整输出,在 §5 附录有保存)。

逐个文件改:

**Pattern**:`message XxxReq {` → `message XxxRequest {`,`message XxxResp {` → `message XxxResponse {`;
RPC 声明里的 in/out 类型也跟着改。

**批量命令**(谨慎):
```bash
cd F:/work/Pandora
# 改 message 定义 + RPC 声明里的类型
find proto/pandora -name "*.proto" | xargs sed -i \
  -e 's|\bReq\b|Request|g' \
  -e 's|\bResp\b|Response|g'
```

⚠️ 这条 sed 用 `\b` 边界,**应该不会**改坏 `request_id` 这种(因为 `request_id` 里 `Req` 不在边界)。但**务必**跑完后用 grep 验:
```bash
grep -rn "request_id\|requested_at" proto/pandora/ | head    # 业务字段保留
grep -rn "\bReq\b\|\bResp\b" proto/pandora/                   # 应该 0 处
```

如果 sed 不安全,改用 Python 脚本精确 AST 替换(`message <名>Req {`)。

#### 1.3 PushFrame → 加 lint:ignore 注释(语义保留)

`proto/pandora/push/v1/push.proto` 第 48 行:

```proto
// buf:lint:ignore RPC_RESPONSE_STANDARD_NAME
rpc Subscribe(SubscribeRequest) returns (stream PushFrame);
```

**理由**:`PushFrame` 是"持续推送的事件帧",语义不是"Subscribe 的响应"。改名 `SubscribeResponse` 反而误导。用 ignore 注释豁免单条规则。

#### 1.4 改 buf.gen.go.yaml 适配新路径

```yaml
# proto/buf.gen.go.yaml
# 不需要改,managed.go_package_prefix 仍是
#   github.com/luyuancpp/pandora/proto/gen/go
# 加 pandora/ 前缀后,产物路径会变成 proto/gen/go/pandora/<X>/v1/*.pb.go
# go 服务 import 也跟着变
```

后续 go 服务的 import 路径要用:
```go
import loginpb "github.com/luyuancpp/pandora/proto/gen/go/pandora/login/v1"
```

#### 1.5 改 docs(15 处涉及 proto 路径)

批量改 docs 中 `proto/<X>/v1/` → `proto/pandora/<X>/v1/`:

```bash
cd F:/work/Pandora
for f in docs/design/*.md PROGRESS.md CLAUDE.md AGENTS.md README.md; do
  [ -f "$f" ] || continue
  sed -i 's|proto/common/v1/|proto/pandora/common/v1/|g;
          s|proto/login/v1/|proto/pandora/login/v1/|g;
          s|proto/player/v1/|proto/pandora/player/v1/|g;
          s|proto/team/v1/|proto/pandora/team/v1/|g;
          s|proto/match/v1/|proto/pandora/match/v1/|g;
          s|proto/chat/v1/|proto/pandora/chat/v1/|g;
          s|proto/friend/v1/|proto/pandora/friend/v1/|g;
          s|proto/locator/v1/|proto/pandora/locator/v1/|g;
          s|proto/dialogue/v1/|proto/pandora/dialogue/v1/|g;
          s|proto/trade/v1/|proto/pandora/trade/v1/|g;
          s|proto/data_service/v1/|proto/pandora/data_service/v1/|g;
          s|proto/ds/v1/|proto/pandora/ds/v1/|g;
          s|proto/hub/v1/|proto/pandora/hub/v1/|g;
          s|proto/battle/v1/|proto/pandora/battle/v1/|g;
          s|proto/push/v1/|proto/pandora/push/v1/|g' "$f"
done
```

同时改文档中所有 `XxxReq` / `XxxResp` 例子 → `XxxRequest` / `XxxResponse`(注意只改 RPC 类型例子,不改业务���段说明)。

#### 1.6 跑 buf lint + generate 验证

```powershell
cd F:\work\Pandora
pwsh tools/scripts/proto_gen.ps1 -Lint     # 期望全绿
pwsh tools/scripts/proto_gen.ps1            # 期望生成 .pb.go
```

预期产物:
```
proto/gen/go/pandora/login/v1/{login.pb.go, login_grpc.pb.go, login_http.pb.go}
proto/gen/go/pandora/push/v1/{push.pb.go, push_grpc.pb.go, push_http.pb.go}
... (其它 16 个 service 各 3 个文件)
```

#### 1.7 commit W2 ②⁺

```bash
cd F:/work/Pandora
git add proto/ docs/ README.md PROGRESS.md CLAUDE.md AGENTS.md
git commit -m "refactor(proto): W2 ②⁺ 全遵 buf STANDARD(目录 + 命名)

- 目录:proto/X/v1/ → proto/pandora/X/v1/(18 个文件 git mv)
- RPC 类型命名:Req/Resp → Request/Response(对齐 Google AIP)
- PushFrame 单条 lint:ignore(语义优先)
- 同步改 docs 中 proto 路径 + RPC 类型示例
- buf lint 全绿,buf generate 产 18 个 service 的 .pb.go

接 commit e307dd8(W2 ⓪+①+②)。
"
```

---

### Step 2:W2 ③ — 写 login 服务(Kratos 第一个业务服)

详细 plan 见 `C:\Users\luyua\.claude\plans\delightful-snuggling-ritchie.md` 任务 ③。

**目录结构(Kratos 标准)**:
```
F:/work/Pandora/services/account/login/
├── cmd/login/main.go              # Kratos kratos.New(...).Run()
├── etc/login-dev.yaml             # 配置文件
├── internal/
│   ├── conf/conf.go               # 嵌入 config.Base
│   ├── service/login.go           # 实现 LoginServiceServer
│   ├── biz/login.go               # 业务逻辑(W2 mock)
│   ├── data/account.go            # MySQL/Redis(W2 mock 返回固定值)
│   └── server/
│       ├── grpc.go                # 注册 LoginService 到 grpc Server
│       └── http.go                # 注册 HTTP handler
├── go.mod                         # module github.com/luyuancpp/pandora/services/account/login
└── README.md
```

**W2 mock 实现范围**:
- `Login(account, password_hash, device_id)`:
  - 校验 account=`test` password_hash=`abc`
  - 返回固定 session_token / hub_ds_addr=`127.0.0.1:7777` / hub_ticket
- `Logout`:固定 OK
- `IssueDSTicket / VerifyDSTicket`:**W2 不实现**(W3 hub_allocator 时一起做),先返回 `errcode.ErrUnknown`

**端口**:50001(gRPC) + 51001(HTTP)

**main.go 模板**(参考 Kratos kratos-layout 项目):
```go
package main

import (
    "github.com/go-kratos/kratos/v2"
    "github.com/go-kratos/kratos/v2/config"
    "github.com/go-kratos/kratos/v2/config/file"

    "github.com/luyuancpp/pandora/pkg/log"
    "github.com/luyuancpp/pandora/pkg/grpcserver"
    "github.com/luyuancpp/pandora/pkg/transport/http"
    "github.com/luyuancpp/pandora/services/account/login/internal/conf"
    loginpb "github.com/luyuancpp/pandora/proto/gen/go/pandora/login/v1"
)

func main() {
    logger := log.Setup("login")

    c := config.New(config.WithSource(file.NewSource("etc/login-dev.yaml")))
    defer c.Close()
    if err := c.Load(); err != nil { panic(err) }

    var cfg conf.Config
    if err := c.Scan(&cfg); err != nil { panic(err) }

    grpcSrv := grpcserver.MustNewServer(cfg.Server)
    httpSrv := http.MustNewServer(cfg.Server.Http)

    svc := newLoginService(/*deps*/)
    loginpb.RegisterLoginServiceServer(grpcSrv, svc)
    loginpb.RegisterLoginServiceHTTPServer(httpSrv, svc)

    app := kratos.New(
        kratos.Name("login"),
        kratos.Logger(logger),
        kratos.Server(grpcSrv, httpSrv),
    )
    if err := app.Run(); err != nil { panic(err) }
}
```

**验证**:
```powershell
cd F:\work\Pandora
go run ./services/account/login/cmd/login -conf services/account/login/etc/login-dev.yaml

# 另一个终端
grpcurl -plaintext -d '{"account":"test","password_hash":"abc","device_id":"d1"}' `
  127.0.0.1:50001 pandora.login.v1.LoginService/Login
```

**别忘了**:
- `go.work` 加 `use ./services/account/login`
- `services/account/login/.gitkeep` 删除(替换成 cmd/main.go)

---

### Step 3:W2 ④ — Envoy v1.38.0 本地 docker(可与 ③ 并行)

详细 plan 见 plan 文件任务 ④。

**关键文件**:
1. `deploy/docker-compose.dev.yml` 加 envoy service(image: `envoyproxy/envoy:v1.38-latest`)
2. `deploy/envoy/envoy.yaml`(grpc_web filter + cors + login cluster)
3. `deploy/envoy/cert.pem` + `key.pem`(用 mkcert 生成,**不入库**)

**自签证书**(用户机器跑):
```powershell
cd F:\work\Pandora\deploy\envoy
mkcert -cert-file cert.pem -key-file key.pem localhost 127.0.0.1
```

**envoy.yaml 模板**见 `docs/design/gateway-decision.md` §5.3,W2 阶段只配 login 一个 cluster。

**验证**:
```powershell
pwsh tools/scripts/dev_up.ps1
docker logs pandora-envoy --tail 20    # 期望:"starting main dispatch loop"

# 经 Envoy 测 login
grpcurl -insecure -d '{"account":"test","password_hash":"abc","device_id":"d1"}' `
  localhost:8443 pandora.login.v1.LoginService/Login
```

---

### Step 4:W2 ⑤ — push 服务骨架(server stream)

详细 plan 见 plan 文件任务 ⑤。

目录同 login,加 `internal/biz/connection.go`(玩家 stream 索引)+ `internal/biz/consumer.go`(kafka mock)。

**W2 mock 行为**:Subscribe RPC 接受 stream,启动 5s timer 自动 `stream.Send(PushFrame{topic:"pandora.system.notify", payload:[]byte("hello")})`。

W3 实现真实 kafka consumer + redis ZSET 离线消息。

**端口**:50014(gRPC server stream)

**验证**:
```powershell
grpcurl -insecure -H "x-player-id: 1001" `
  -d '{"session_token":"mock","last_seen_ms":0}' `
  localhost:8443 pandora.push.v1.PushService/Subscribe

# 期望:每 5s 收到一帧
```

---

### Step 5:W2 ⑥ — 端到端 hello world 测试

3 个测试全过 = W2 架构验证通过:

```powershell
# 1. 启动基础设施 + envoy
pwsh tools/scripts/dev_up.ps1

# 2. 启动 login + push(各自一个终端)
go run ./services/account/login/cmd/login -conf services/account/login/etc/login-dev.yaml
go run ./services/runtime/push/cmd/push -conf services/runtime/push/etc/push-dev.yaml

# 3. 三个 grpcurl 全过
grpcurl -plaintext :50001 ... LoginService/Login                # 直连 login
grpcurl -insecure :8443 ... LoginService/Login                  # 经 Envoy
grpcurl -insecure :8443 ... PushService/Subscribe               # 经 Envoy 接 stream
```

---

### Step 6:W2 ⑦ — 收尾

- 更新 `PROGRESS.md`:加 W2 完成段(写明 commit 哈希)
- 更新 `docs/design/go-services.md`:login + push 实现状态
- 更新 `docs/design/pkg-copy-from-mmorpg.md` §5.3:把"W2 待重写"改"W2 已完成"
- 用户 commit + push

---

## §4 接班 AI 工作守则

### 4.1 必读文档(按顺序)

1. **本文档**(完整接班说明)
2. `PROGRESS.md`(项目进度,只追加)
3. `CLAUDE.md`(项目规范)
4. `docs/design/pandora-arch.md`(总架构 + §11 决策行)
5. `docs/design/gateway-decision.md`(Kratos + Envoy + gRPC-Web 终版)
6. `docs/design/protocol-ordering-rules.md`(4 ��议原则)
7. `docs/design/architecture-rejected-strict-ds-only.md`(反面教材)
8. `docs/design/ds-arch.md`(UE DS + 协议铁律 §0)
9. `docs/design/go-services.md`(14 个服务清单)
10. `docs/design/infra.md`(端口 / topic / 命名规范)
11. `AGENTS.md`(AI 协作守则,**重点**)

### 4.2 必须遵守的工作流

按 `AGENTS.md` 全部规则,**重点强调**:
- **每个编码任务前**:用 plan 模式列动作清单给用户审,审过批量执行
- **不擅自 commit**(用户手动)
- **不擅自 push**(用户手动)
- **不操作远端仓库**
- **用中文回复**(项目规则)
- **任务前先查 task tool**(看历史进度)

### 4.2.1 跨 AI 平台硬性分工(节省 token)

**Claude 系模型(Copilot Claude / Claude Code / Cursor Claude 等)只负责脑力和代码**:
- 深度分析完整详细做法
- 开 plan 模式给用户审
- 审过后改代码 / proto / yaml / 脚本 / 文档
- 跑项目内验证(build / test / lint / docker compose config)

**Claude 系模型不做环境和 git 收尾**:
- 不安装 / 升级 / 卸载本机工具
- 不改 PATH / 证书信任 / Docker Desktop 设置 / 防火墙等系统环境
- 不拉大型 Docker 镜像作为环境准备动作
- 不执行 git status / diff --stat / commit message 建议 / commit / push / tag

**ChatGPT / Codex 负责环境和提交收尾**:
- 检查并安装本机工具(mkcert / grpcurl / buf / protoc / docker image 等)
- 在用户批准后改本机开发环境和证书信任
- 做环境就绪确认,把结果反馈给 Claude Code / 用户
- 输出 git status / diff --stat / commit message 建议
- 在用户明确说"帮我 commit"时执行 git commit

原则:Claude 系模型 token 用在分析和改代码;安装工具、环境准备、git 收尾交给 ChatGPT / Codex。

### 4.3 触碰红线立即停止

- 改 30+ 个文件(可能方向错了)
- 写 secret 进 git
- 即将 push 远端
- 发现规范文档自相矛盾

### 4.4 失败回报

- 不"假装成功",老实说 build 失败
- 不"自动重试 5 次"浪费时间
- 不"绕过失败"(注释掉断言、跳过 test)
- 不擅自 git reset / git checkout 销毁进度

---

## §5 附录:历史 lint 错误(W2 ② 跑 buf lint 输出,做修复参考)

完整 buf lint 错误(115+ 处)在用户 commit `e307dd8` 之后跑 `pwsh tools/scripts/proto_gen.ps1` 时贴出来过。**核心两类**:

1. **PACKAGE_DIRECTORY_MATCH 违规(18 处)**
   - 形如 `Files with package "pandora.battle.v1" must be within a directory "pandora\battle\v1" relative to root but were in directory "battle\v1"`
   - **修法**:目录重构 `proto/X/v1/` → `proto/pandora/X/v1/`(已 git mv,见 §3 Step 1.1)

2. **RPC_REQUEST_STANDARD_NAME / RPC_RESPONSE_STANDARD_NAME 违规(96+ 处)**
   - 形如 `RPC request type "LoginReq" should be named "LoginRequest" or "LoginServiceLoginRequest"`
   - **修法**:批量 sed `Req` → `Request`,`Resp` → `Response`(见 §3 Step 1.2)
   - **例外**:`PushFrame` 用 lint:ignore 注释(见 §3 Step 1.3)

---

## §6 反面教材(已否决方案,任何想再讨论的人必读)

### 6.1 严格 A:客户端只连 DS

详见 `docs/design/architecture-rejected-strict-ds-only.md`。6 个不可接受后果:
1. Hub DS 兼任业务网关
2. Hub DS 崩 → 所有功能挂
3. Hub DS 重启 → 业务挂
4. UE C++ 代码量翻 2~3 倍
5. 500 人 PvP 性能预算被破
6. 登录入口死锁

### 6.2 D2.1 选 go-zero

详见 `docs/design/pkg-copy-from-mmorpg.md` §5.1。
**推翻原因**:go-zero zrpc 不支持 gRPC server stream,推送架构受限,要自研 WebSocket 替代,违反"协议标准化"铁律。

### 6.3 自研 WebSocket gateway

详见 `docs/design/gateway-decision.md` §10 演化记录。
**推翻原因**:WebSocket envelope 协议自研化,违反"大厂 + 最标准方案"铁律。改用 Envoy + gRPC-Web。

### 6.4 客户端 3 连接(NetDriver + HTTP + WebSocket)

**推翻原因**:gateway + push 分离冗余,WebSocket 一条复用更优雅。改用 2 连接(NetDriver + gRPC-Web)。

---

## §7 当前未决项

- ⏸️ UE 仓库名(暂用 `Pandora-Client` 占位,D4 阻塞)
- ⏸️ k8s 选型:阿里云 ACK / 自建 / 先 minikube(D7 阻塞)
- ⏸️ Envoy 跑模式:k8s Ingress / 独立 Pod(D7 决定)
- ⏸️ JWT 鉴权细节(login 服务签发 + Envoy jwt_authn filter)(W3 写 login 时定)
- ⏸️ PvP 死亡惩罚 / 新人保护 / 段位 / MMR(都在 `pvp-rules.md` backlog,后期定)

---

## §8 关键文件索引(快速跳转)

| 想了解什么 | 看哪个文件 |
|---|---|
| 项目进度 + 演化历史 | `PROGRESS.md` |
| 项目规范 + 决策行 | `CLAUDE.md` / `docs/design/pandora-arch.md` §11 |
| 终版架构(Kratos + Envoy + gRPC-Web)| `docs/design/gateway-decision.md` |
| RPC 协议铁律 | `docs/design/protocol-ordering-rules.md` |
| UE DS 协议边界 | `docs/design/ds-arch.md` §0 |
| 13 + push 服务清单 | `docs/design/go-services.md` |
| 端口 / topic / 命名 | `docs/design/infra.md` |
| pkg 模块重写记录 | `docs/design/pkg-copy-from-mmorpg.md` §5 |
| 反面教材 | `docs/design/architecture-rejected-strict-ds-only.md` |
| 已生效 18 个 proto | `proto/pandora/<X>/v1/*.proto`(git mv 后) |
| pkg 公共框架代码 | `pkg/{log,config,middleware,grpcserver,grpcclient,...}/` |
| 一键装工具 | `tools/scripts/install_dev_tools.ps1` |
| proto 生成脚本 | `tools/scripts/proto_gen.ps1` |
| 基础设施启停 | `tools/scripts/{dev_up,dev_down,dev_status}.ps1` |
| docker compose | `deploy/docker-compose.dev.yml` |
| MySQL 初始化 SQL | `deploy/mysql-init/01-create-databases.sql` |
| Prometheus 抓取配置 | `deploy/prometheus/prometheus.yml` |

---

## §9 接班 AI 给用户的第一条消息建议

```
我接班了(我是 <你的 AI 名字 / 平台>)。
已读完 HANDOFF.md / PROGRESS.md / CLAUDE.md / AGENTS.md /
gateway-decision.md / protocol-ordering-rules.md。

当前状态:W2 ②⁺ 进行中,18 个 .proto 已 git mv 到 proto/pandora/,
但 import 路径 / RPC 命名 / docs 还没改。

我建议立刻按 HANDOFF.md §3 Step 1 顺序做:
1. 改 import 路径(sed 批量)
2. 改 RPC Req/Resp → Request/Response(sed 批量,带 \b 边界)
3. PushFrame 加 lint:ignore 注释
4. 改 docs 中 proto 路径引用
5. 跑 buf lint 验全绿
6. 跑 buf generate 产 .pb.go
7. commit W2 ②⁺(用户手动 commit)

要我开 plan 模式列具体动作给你审,还是直接动手?
```

---

## §10 跨 AI 平台兼容性说明

本项目历经多次 AI 平台切换,**任何 AI 接班都应能继续工作**。已验证兼容:

- ✅ Claude Code(Anthropic 官方 CLI,Haiku / Sonnet / Opus)
- ✅ GitHub Copilot Chat(Claude Sonnet 4.x / GPT-4.x)
- ✅ Cursor(任意模型)
- ✅ Anthropic API direct(任意 Claude 模型)

**关键约束**(任何平台都要遵守):
1. 中文回复(`CLAUDE.md` §3)
2. 不操作远端仓库(`AGENTS.md` §3)
3. 不擅自 commit / push(`AGENTS.md` §3)
4. 编码任务前开 plan 模式(`AGENTS.md` §4)— 如果平台不支持 plan 模式,改成"先列动作清单等用户审"
5. 不擅自 commit / push(`AGENTS.md` §3)

**特别提醒** Copilot Chat / Cursor 用户:
- 把本文件 + `AGENTS.md` 加进 Copilot 的 system instructions / `.cursorrules`
- 否则 AI 默认行为可能违反"不擅自 commit"等规则

---

**祝接班顺利**。本项目经过多轮架构演化(D3 推翻 10+ 轮),所有暗坑都挖出来定死了。
**接下来按部就班做即可,不会再有大的方向问题**。
