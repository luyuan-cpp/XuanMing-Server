# player_locator(W3 ⑤,2026-06-05)

Pandora 第 3 个 Kratos 业务服。镜像 push 服务目录结构,只暴露 gRPC + /metrics。

## 职责

`docs/design/go-services.md §2.6`:维护 `player_id → Location` 的映射,
实现 `CLAUDE.md §9.1` 不变量 "玩家同一时刻只能在一个 Location"。

## 端口(`docs/design/infra.md §6.3`)

| 协议 | 端口 |
|------|------|
| gRPC | `:50006` |
| HTTP | `:51006`(仅 `/metrics`) |

## 对外 RPC(`proto/pandora/locator/v1/locator.proto`)

```
PlayerLocatorService.SetLocation(player_id, Location) → ok
PlayerLocatorService.GetLocation(player_id)           → Location
PlayerLocatorService.ClearLocation(player_id)         → ok
```

## 存储

| Key | 类型 | TTL | 用途 |
|------|------|-----|------|
| `pandora:locator:<player_id>` | hash | 30s heartbeat(SetLocation 时刷新) | 玩家位置 |

字段:
- `state`         LocationState 枚举的字符串名(`hub`/`battle`/...,便于人读)
- `state_code`    int32 枚举值(便于程序判断)
- `hub_pod`       HUB 时填
- `shard_id`      HUB 时填(uint32 to string)
- `match_id`      MATCHING / BATTLE 时填
- `battle_pod`    BATTLE 时填
- `updated_at_ms` 服务端记录的写入时刻

## W3 ⑤ 范围

- Redis 单一真源(无 mysql)
- 不消费 `pandora.locator.update` topic(W3 ④ 完成 push 接 kafka 后再补 hub_ds / battle_ds 上报通道)
- 不做 leader / 集群拓扑(本服务是无状态的,horizontally scalable)
- 不做 Conflict 检测(W4+ 接 DS 注册表后补:同一玩家被两个 DS 上报 → ErrLocatorConflict)

## W3 联调命令

```powershell
# 起 redis(若未起)
docker compose -f deploy/docker-compose.dev.yml up -d redis

# 起 locator
go run ./services/runtime/player_locator/cmd/locator -conf services/runtime/player_locator/etc/locator-dev.yaml

# SetLocation(直连 :50006)
grpcurl -plaintext -d '{\"player_id\":10086,\"location\":{\"state\":3,\"hub_pod\":\"hub-0\",\"shard_id\":1}}' \
  127.0.0.1:50006 pandora.locator.v1.PlayerLocatorService/SetLocation

grpcurl -plaintext -d '{\"player_id\":10086}' 127.0.0.1:50006 pandora.locator.v1.PlayerLocatorService/GetLocation

grpcurl -plaintext -d '{\"player_id\":10086}' 127.0.0.1:50006 pandora.locator.v1.PlayerLocatorService/ClearLocation
```
