# Pandora push 服务

> Pandora 第二个 Kratos 业务服(W2 ⑤ → W3 ④,2026-06-05 真实化),server stream 长连推送。

## 职责

详见 [`docs/design/go-services.md`](../../../docs/design/go-services.md) 及 [`docs/design/gateway-decision.md`](../../../docs/design/gateway-decision.md) §5/§6。

- 客户端登录后立刻 `Subscribe(server stream)` 维持长连接
- 服务端持有所有在线客户端 stream,按 `player_id` 路由 kafka 事件
- 转发推送 topics(`pandora.team.update` / `pandora.match.progress` / `pandora.chat.*` / `pandora.player.update` / `pandora.friend.event` / `pandora.system.notify`)
- 离线消息缓存 redis ZSET(5min,断线重连按 `last_seen_ms` 补推)

## 协议铁律(对齐 [`protocol-ordering-rules.md`](../../../docs/design/protocol-ordering-rules.md))

- **原则 2**:发起方不收自己触发的 push(业务服 produce kafka 时**必须用 `pkg/kafkax.PushToPlayers` helper**,helper 自动排除 caller_player_id)
- **原则 3**:已受理型 RPC(`match.StartMatch` / `ConfirmMatch`)是例外,传 `callerPlayerID=0` 让 helper 跳过排除,push 给所有人含发起方

## 架构边界

- 本服务**不是 WebSocket 服务**(2026-06-03 自研 WebSocket 已被否决)
- 本服务**不是 HTTP 网关**(那是 Envoy 的职责)
- 客户端走 gRPC-Web over HTTP/2 TLS 连 Envoy,Envoy 转标准 gRPC 给本服务
- 业务服推送事件全部走 kafka,本服务消费转 stream(不接业务服直接 gRPC 调用)

## 端口

| 协议 | 端口 | 用途 |
|---|---|---|
| gRPC | 50014 | server stream(客户端 → Envoy gRPC-Web → 本服) |
| HTTP | 51014 | 仅 `/metrics`(`push.proto` 无 `google.api.http` 注解,无 RESTful RPC) |

详见 [`docs/design/infra.md`](../../../docs/design/infra.md) §6.2。

## 目录结构(Kratos 标准分层,对齐 login)

```
cmd/push/main.go              启动入口(W3 ④:redis + kafka consumer 装配)
etc/push-dev.yaml             开发期配置(W3 ④:kafka + topics + offline_cache_ttl)
internal/
  conf/                       配置结构(嵌入 pkg/config.Base + PushConf{Topics, OfflineCacheTTL})
  service/                    RPC 入口(实现 pushv1.PushServiceServer)
  biz/                        usecase
    connection.go             player_id → stream 内存索引(顶号语义)
    push.go                   PushUsecase + RunSubscribeStream(补推 + 阻塞等推送)
    consumer.go               KafkaConsumer(每 topic 一个,共享 GroupID)
  data/
    offline.go                RedisOfflineCacheRepo(ZSET pandora:push:offline:<player_id>)
  server/                     grpc / http server 注册
```

## W3 ④ 真实化(2026-06-05)

### 数据流

```
业务服 producer
  └─ kafkax.PushToPlayers(ctx, callerPID, toPIDs, payload)
       └─ SendRaw(key=strconv.FormatUint(playerID, 10))  ← 一致性哈希,同 player_id 同 partition,partition 内保序
            ↓ kafka pandora.team.update / pandora.match.progress / pandora.chat.private
push 服务 KafkaConsumer(每 topic 一个)
  └─ handle(msg): key 非数字 → log+ack 跳过;否则 →
       ├─ 在线: ConnectionManager.SendTo(playerID, PushFrame) → stream
       └─ 离线: offline.Append(playerID, frame) → redis ZSET
                                                  (score=ts_ms, TTL=5m 每写刷新)
客户端重连
  └─ Subscribe(last_seen_ms=N)
       └─ RunSubscribeStream: offline.Range(playerID, sinceMs=N) → 补推 → 阻塞等 ctx.Done
```

### 配置示例(`etc/push-dev.yaml`)

```yaml
kafka:
  brokers: ["127.0.0.1:9093"]   # Pandora 端口规划(非 vanilla 9092)
  group_id: "pandora-push"
  partition_cnt: 4
  dial_timeout: "2s"

push:
  topics:
    - "pandora.team.update"
    - "pandora.match.progress"
    - "pandora.chat.private"
  offline_cache_ttl: "5m"
```

## 本地启动

```powershell
# 1. 基础设施(redis + kafka)
pwsh tools/scripts/dev_up.ps1

# 2. 启 push
cd e:\work\Pandora
go run ./services/runtime/push/cmd/push -conf services/runtime/push/etc/push-dev.yaml
```

## 验证(可选,需装 grpcurl + kafka-console-producer)

```powershell
# 1) 客户端 subscribe(直连,无 token)
grpcurl -plaintext -d '{\"session_token\":\"\",\"last_seen_ms\":0}' `
  127.0.0.1:50014 pandora.push.v1.PushService/Subscribe

# 2) 另起 terminal,往 kafka 写一条 key=42 的消息(进 kafka 容器)
docker exec -it pandora-kafka kafka-console-producer.sh `
  --bootstrap-server 127.0.0.1:9093 `
  --topic pandora.team.update `
  --property "parse.key=true" --property "key.separator=:"
# 输入:42:dummy-payload
# 客户端 1) 那边应立即收到一帧 PushFrame{topic=pandora.team.update}

# 3) 断开 grpcurl 后再 produce,redis 应见离线缓存
redis-cli -p 6380 ZRANGE pandora:push:offline:42 0 -1 WITHSCORES

# 4) Prometheus 抓 metrics
curl http://127.0.0.1:51014/metrics | Select-String pandora
```

## 下一步(W3 路线剩余)

- [ ] 接入业务侧 producer:team / match / player 已按各服务上线节奏推进;chat 仅保留 topic 模板,不作为当前任务
- [ ] `friend.event` / `system.notify` 保留为后期模板;friend 当前暂缓到最后,不要为了补 topic 提前实现 friend 服务
- [ ] `/metrics` 自定义指标 `pandora_push_online_streams` / `pandora_push_send_failed_total`
- [ ] 系统公告类(`pandora.system.notify`)走 `Conns().Broadcast`(本批不接,等 system 服务上线)
- [ ] DSTicket 二次校验(`pandora.ds.v1` 票据已被 Envoy 校验,但 push 服务接 DS-targeted 推送时可加冗余)

**2026-06-06 排期说明**:`chat` / `friend` 对应 topic 和订阅配置只是模板占位,服务本体等 UE 与核心业务链路全部完成后再做。
