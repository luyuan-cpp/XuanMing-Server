# Pandora login 服务

> Pandora 第一个 Kratos 业务服(W2 ③,2026-06-05)

## 职责

详见 [`docs/design/go-services.md`](../../../docs/design/go-services.md) §2.1。

- 账号登录 / 登出
- 颁发 Session Token + Hub DS 票据
- 验证 DS 票据(JWT + Redis JTI 防重放)

## 端口

| 协议 | 端口 | 用途 |
|---|---|---|
| gRPC | 50001 | 主流量(客户端 → Envoy gRPC-Web → 本服) |
| HTTP | 51001 | `/metrics` Prometheus + RESTful `/v1/login` 等 |

详见 [`docs/design/infra.md`](../../../docs/design/infra.md) §6.2。

## 目录结构(Kratos 标准分层)

```
cmd/login/main.go             启动入口
etc/login-dev.yaml            开发期配置
internal/
  conf/                       配置结构(嵌入 pkg/config.Base)
  service/                    RPC 入口(实现 loginv1.LoginServiceServer)
  biz/                        usecase(纯业务逻辑,不依赖 grpc/redis)
  data/                       repository(MySQL 账号 + Redis session / 票据防重放)
  server/                     grpc / http server 注册
```

## 当前登录行为

- `Login(account, password_hash, ...)`:
  - 默认走 MySQL `pandora_account.accounts` 查询账号,用 bcrypt 校验密码。
  - `dev_skip_password` / `dev_auto_register` 只供本机联调,见下文。
  - hub_allocator 可用时返回真实 hub 地址和票据;兼容栅栏关闭时，不可用会回退静态
    `mock_hub_ds_addr` + login 自签 hub 票据。
- `Logout`:删除 Redis session(未启 Redis 时幂等返回)。
- `IssueDSTicket` / `VerifyDSTicket`:JWT 签发 / 校验,Redis JTI repo 启用时做票据防重放。
  公共 RPC 与登录断线重连的 Battle 签票共用一个 issuer：签名前必须从 Redis 证明玩家属于 live
  match roster；Redis/坏 protobuf/空 roster/陈旧心跳/Model-B 漂移均 fail-closed，不再直接相信
  `target_id` 或 locator。locator 已明确 `InBattle` 后授权失败返回 `Unavailable`，不再继续分配 Hub
  或写 LOGIN_PENDING；重连地址使用 authorizer 同一 Redis 快照中的 live `ds_addr`，不回退 locator
  旧地址。带 Model B 绑定的 Hub 票会在消费 jti 前严格核对 Redis 当前玩家 assignment。
- Redis authority 下的 UE DS 在线 `VerifyDSTicket` 只走独立 Envoy `:8444` exact route，固定执行
  Guard → Redis active/projection → ticket binding/assignment/roster → admission JTI marker；`:8443`
  对该 path 精确 403。相同小写 UUIDv4 `admission_id` 仅在 30s 短窗内幂等恢复响应丢失。

## Hub 归属绑定激活栅栏

`login.require_hub_assignment_binding` 默认 `false`，供新旧副本滚动兼容。切为 `true` 后：

- 无完整 `(assignment_id, pod, uid, epoch, gen, credential_jti, writer_epoch)` 绑定的 Hub 票一律拒绝；
- login 禁止自签 Hub 票和静态地址回退，只接受 hub_allocator 权威签发结果；
- Redis 或 `login.hub.addr` 未配置会在启动期直接失败；
- assignment missing/mismatch 不消费票据 jti，Redis 故障或坏 protobuf 返回 `Unavailable`。

> 生产阻断：上述代码门不等于 DS auth 已可 Apply。blue/green 行为激活、跨普通轮换旧 Hub 票据 grace、
> Battle terminal outbox、revisioned immutable Secret/keyset、digest 与集群内 synthetic 尚未闭环；详见
> `docs/design/decision-revisit-ds-callback-auth.md` §7.15–§7.16。

## 本地启动

```powershell
# 1. 基础设施(MySQL / Redis)
pwsh tools/scripts/dev_up.ps1

# 2. 启 login
cd F:\work\XuanMing-Server
go run ./services/account/login/cmd/login -conf services/account/login/etc/login-dev.yaml
```

## 验证(可选,需装 grpcurl)

```powershell
# 直连 gRPC
grpcurl -plaintext -d '{\"account\":\"test\",\"password_hash\":\"abc\",\"device_id\":\"d1\"}' `
  127.0.0.1:50001 pandora.login.v1.LoginService/Login

# 走 HTTP RESTful
curl -X POST http://127.0.0.1:51001/v1/login `
  -H "Content-Type: application/json" `
  -d '{"account":"test","password_hash":"abc","device_id":"d1"}'

# Prometheus 抓 metrics
curl http://127.0.0.1:51001/metrics | Select-String pandora
```

## 开发期免密登录开关 `login.dev_skip_password`

> ⚠️ **纯 dev / 联调开关,默认 `false`,绝不能上生产。**

为了让客户端联调期“随便填个账号名就能进”,login 提供一个免密 + 懒注册开关:

```yaml
login:
  dev_skip_password: true   # 默认 false（生产必须留 false）
```

开启后（`true`）行为:

1. **跳过 bcrypt 密码校验** —— 任意 `password_hash` 都放行。
2. **账号不存在时自动懒注册** —— 用 snowflake 生成 `player_id` 写入 `accounts`表
   （靠 `uk_account` 唯一），同一账号名以后每次登录都拿到**同一个稳定 `player_id`**
   （持久化在 MySQL，不是临时算的）。
3. 启动时打 `DEV_SKIP_PASSWORD_ENABLED` 警告日志，`service_ready` 日志带 `dev_skip_password` 字段。

用途:客户端随便填个账号名就能登录拿到对应 `player_id`，无需独立注册流程/RPC。

⚠️ **绝不能上生产** —— 否则任意账号名都能登录任意 `player_id`。
生产环境留 `false`（默认），走正常 bcrypt 校验。

> 注:login 当前不再支持未配 MySQL DSN 的内存 mock fallback;DSN 为空会启动失败。

## 开发期“假注册”开关 `login.dev_auto_register`

> ⚠️ **纯 dev / 联调开关，默认 `false`，绝不能上生产。**

注册不属于 login 服务的正式职责;为联调方便提供一个“首登即注册”的 dev 开关:

```yaml
login:
  dev_auto_register: true   # 默认 false（生产必须留 false）
```

开启后（`true`）:账号不存在时**首次登录自动注册**一条 `accounts` 记录
（snowflake 分配 `player_id`，密码存入本次客户端所发 `password_hash` 的 bcrypt 哈希），
启动时打 `DEV_AUTO_REGISTER_ENABLED` 警告日志。

与 `dev_skip_password` 正交组合:

| dev_auto_register | dev_skip_password | 行为 |
|---|---|---|
| false | false | 正常:账号必须存在 + 密码必须匹配 |
| **true** | false | **假注册**:未知账号首登即注册并存本次密码,后续用同密码走正常 bcrypt 校验（错密码仍拦） |
| false | true | 免密:已存在账号任意密码放行;未知账号也会被懒注册 |
| true | true | 最宽松:任意账号名 + 任意密码都能进 |

⚠️ **绝不能上生产** —— 生产留 `false`（默认），账号不存在直接返 `ErrLoginAccountNotFound`。

## 后续待办

- [ ] 生产 `pandora.login.event` topic(登入登出事件,给风控 / 审计)
