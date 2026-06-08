# 登录调试手册

> 用途:本机用 VS Code / PowerShell 调试 Pandora login 服务。
> 当前阶段先直连 login 的本地 HTTP / gRPC 端口,不走 UE 客户端,也不走 Envoy gRPC-Web。

## 1. 启动顺序

### 1.1 基础设施

在仓库根目录 `E:\work\Pandora` 执行:

```powershell
docker compose -f deploy/docker-compose.dev.yml --env-file deploy/env/dev.env up -d mysql redis
```

检查:

```powershell
docker ps --format "table {{.Names}}\t{{.Status}}\t{{.Ports}}"
```

至少应看到:

- `pandora-mysql` healthy,端口 `3307`
- `pandora-redis` healthy,端口 `6380`

### 1.2 登录依赖服务

login 当前开发配置会拨 `player_locator` 和 `hub_allocator`。分别开两个终端:

```powershell
go run ./services/runtime/player_locator/cmd/locator -conf services/runtime/player_locator/etc/locator-dev.yaml
```

```powershell
go run ./services/battle/hub_allocator/cmd/hub_allocator -conf services/battle/hub_allocator/etc/hub_allocator-dev.yaml
```

端口:

- `player_locator`:gRPC `50006`,HTTP `51006`
- `hub_allocator`:gRPC `50021`,HTTP `51021`

### 1.3 VS Code 启 login

VS Code 使用 [.vscode/launch.json](../../.vscode/launch.json) 里的 `Debug login`:

```json
{
  "name": "Debug login",
  "type": "go",
  "request": "launch",
  "mode": "auto",
  "program": "${workspaceFolder}/services/account/login/cmd/login",
  "cwd": "${workspaceFolder}",
  "args": [
    "-conf",
    "${workspaceFolder}/services/account/login/etc/login-dev.yaml"
  ]
}
```

启动日志里应看到:

```text
account_seed_done account=test ... created=false
account_repo_mysql ...
redis_connected addr=127.0.0.1:6380
locator_dial_ok addr=127.0.0.1:50006
hub_allocator_dial_ok addr=127.0.0.1:50021
service_ready ... account_repo=mysql session_repo=redis hub_assigner=grpc
```

## 2. 测登录

PowerShell 推荐用 `Invoke-RestMethod`,避免 `curl.exe` 的 JSON 引号坑:

```powershell
$body = @{
  account = "test1"
  password_hash = "abc"
  device_id = "login-debug"
} | ConvertTo-Json -Compress

Invoke-RestMethod `
  -Method Post `
  -Uri "http://127.0.0.1:51001/v1/login" `
  -ContentType "application/json" `
  -Body $body
```

成功返回应包含:

- `code = OK`
- `playerId`
- `sessionToken`
- `hubDsAddr`,例如 `127.0.0.1:7778`
- `hubTicket`

也可以用 `curl.exe`,但建议用临时变量:

```powershell
$body = '{"account":"test1","password_hash":"abc","device_id":"login-debug"}'
curl.exe -X POST "http://127.0.0.1:51001/v1/login" `
  -H "Content-Type: application/json" `
  --data-raw $body
```

## 3. 账号数据

开发配置会在 login 启动时确认 / 创建种子账号:

- 账号:`test`
- 密码:`abc`
- 配置位置:`services/account/login/etc/login-dev.yaml`
- 创建逻辑:`services/account/login/cmd/login/main.go`
- 表:`pandora_account.accounts`

本机当前额外插了 demo 账号:

- `test1` 到 `test10000`
- 密码均为 `abc`

检查账号数量:

```powershell
docker exec pandora-mysql mysql -upandora -ppandora_dev_pwd -D pandora_account -N -e "SELECT COUNT(*) FROM accounts; SELECT account, player_id FROM accounts WHERE account IN ('test','test1','test10000') ORDER BY account;"
```

## 4. 建议断点

RPC / HTTP 入口:

- `services/account/login/internal/service/login.go`
- `func (s *LoginService) Login(...)`

业务流程:

- `services/account/login/internal/biz/login.go`
- `func (u *LoginUsecase) Login(...)`
- `resolveHub(...)`

MySQL 查询:

- `services/account/login/internal/data/account.go`
- `FindByAccount(...)`
- `CheckBanned(...)`

注意:HTTP 请求有 deadline。断点停太久后继续执行,可能看到:

```text
mysql find account: context deadline exceeded
mysql check banned: context deadline exceeded
```

这不是账号不存在,而是这次请求的 `ctx` 已经过期。慢慢单步时建议重新发请求,或用 gRPC 加长超时:

```powershell
grpcurl -plaintext -max-time 300 `
  -d '{"account":"test1","password_hash":"abc","device_id":"login-debug"}' `
  127.0.0.1:50001 pandora.login.v1.LoginService/Login
```

## 5. 常见问题

### 5.1 CODEC invalid value account

典型返回:

```json
{"code":400,"reason":"CODEC","message":"body unmarshal proto: syntax error (line 1:2): invalid value account"}
```

原因通常是 PowerShell + `curl.exe` 把 JSON 双引号吃掉了。用 `Invoke-RestMethod` 或先把 JSON 放到 `$body` 变量。

### 5.2 passwordHash / deviceId 不生效

当前 HTTP 入口使用 proto 原字段名,请求体用:

```json
{"account":"test1","password_hash":"abc","device_id":"login-debug"}
```

不要写:

```json
{"account":"test1","passwordHash":"abc","deviceId":"login-debug"}
```

### 5.3 Redis maint_notifications 警告

日志:

```text
maintnotifications disabled due to handshake error
```

这是 go-redis 客户端能力探测警告,不影响登录调试。

## 6. 这次登录测的是什么连接

当前命令直连:

```text
http://127.0.0.1:51001/v1/login
```

这是本地 HTTP 短请求,不是 HTTPS,也不是长连接。

完整架构中:

- `Login` 是短请求
- 后续业务短请求用 `sessionToken` 放在 header / metadata 鉴权
- 业务推送走 push 服务的 gRPC server stream 长流
- 游戏同步走 UE NetDriver 到 Hub / Battle DS 的长连接

