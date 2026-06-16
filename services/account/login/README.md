# Pandora login 服务

> Pandora 第一个 Kratos 业务服(W2 ③,2026-06-05)

## 职责

详见 [`docs/design/go-services.md`](../../../docs/design/go-services.md) §2.1。

- 账号登录 / 登出
- 颁发 Session Token + Hub DS 票据
- 验证 DS 票据(W3 接入,本服 W2 mock 返 `ErrCode_ERR_UNKNOWN`)

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
  data/                       repository(W2 mock,W3 接 mysql + redis)
  server/                     grpc / http server 注册
```

## W2 mock 行为

- `Login(account, password_hash, ...)`:
  - 账号 = `test` 且 password_hash = `abc` → 返 OK + session_token (uuid) + hub_ds_addr = `127.0.0.1:7777`
  - 否则返 `ErrCode_ERR_LOGIN_ACCOUNT_NOT_FOUND` / `ErrCode_ERR_LOGIN_PASSWORD_MISMATCH`
- `Logout`:总是返 OK
- `IssueDSTicket` / `VerifyDSTicket`:返 `ErrCode_ERR_UNKNOWN`(W3 接 JWT + hub_allocator 后真实化)

## 本地启动

```powershell
# 1. 基础设施(redis 可选,W2 不连也能跑)
pwsh tools/scripts/dev_up.ps1

# 2. 启 login
cd F:\work\Pandora
go run ./services/account/login/cmd/login -conf services/account/login/etc/login-dev.yaml
```

## 验证(可选,需装 grpcurl)

```powershell
# 直连 gRPC(W2 没经 Envoy)
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

> 注:`mock` 模式（未配 MySQL DSN 时的 fallback）仍是单账号，“任意账号”懒注册只在接了 MySQL 的路径生效。

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
- [ ] 接 MySQL pandora_account 库(替 MockAccountRepo)
- [ ] 接 Redis session 缓存
- [ ] 调 hub_allocator.Assign 拿真实 hub_ds_addr
- [ ] 实现 JWT 签发(IssueDSTicket)+ 校验(VerifyDSTicket)+ jti 黑名单
- [ ] 生产 `pandora.login.event` topic(登入登出事件,给风控 / 审计)
