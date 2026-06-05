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

## 下一步(W3 真实化路线)

- [ ] 接 MySQL pandora_account 库(替 MockAccountRepo)
- [ ] 接 Redis session 缓存
- [ ] 调 hub_allocator.Assign 拿真实 hub_ds_addr
- [ ] 实现 JWT 签发(IssueDSTicket)+ 校验(VerifyDSTicket)+ jti 黑名单
- [ ] 生产 `pandora.login.event` topic(登入登出事件,给风控 / 审计)
