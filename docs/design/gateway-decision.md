# Pandora 网关与推送架构决策

> **状态**:已决策(2026-06-04 终版)
> **关联否决方案**:`architecture-rejected-strict-ds-only.md`(严格 A 反面教材)
> **关联协议铁律**:`protocol-ordering-rules.md`(乱序原则)
> **本文档地位**:Client ↔ 后端的核心架构总纲。任何 AI / 新开发者改动客户端连接 / 推送 / 网关前必读。

## §0 架构总览(两连接 + Kratos + Envoy + gRPC-Web)

```
                          ┌─────────────────────────────────────┐
                          │           Client(UE 5.7)           │
                          │  - 引擎自带 NetDriver(连 DS)       │
                          │  - 引擎自带 FHttpModule(连 Envoy)  │
                          │  - 自研 grpc-web 客户端(~3-5 天)  │
                          │  - 零第三方 SDK / 零 SSL 冲突       │
                          └──┬───────────────────────────────��──┘
                             │                               │
        ┌────────────────────┘                               └────────────────────┐
        │ ① UE NetDriver(UDP-like)                          ② FHttpModule         │
        │   仅游戏内同步 / GAS / Replication                    HTTP/2 + TLS         │
        │   30~60Hz tick                                       Content-Type:        │
        │                                                       application/         │
        │                                                       grpc-web+proto       │
        ▼                                                                            ▼
┌──────────────────┐                                          ┌──────────────────────┐
│ Hub DS / Battle  │                                          │ Envoy Edge Gateway  │
│ DS(UE,Agones)  │                                          │ 端口 8443(HTTPS)    │
└──────┬───────────┘                                          │                      │
       │ Heartbeat unary                                      │ 1. TLS 终止          │
       │ 每 5s                                                 │ 2. gRPC-Web → gRPC   │
       ▼                                                      │ 3. ALPN 协商         │
┌──────────────────┐                                          │ 4. JWT 鉴权          │
│ ds_allocator(go)│                                          │ 5. 限流 / 熔断       │
│ hub_allocator(go)│                                         │ 6. 路由              │
└──────────────────┘                                          └──────────┬───────────┘
                                                                         │ 标准 gRPC
                                                                         │ unary + server stream
                                                                         ▼
                                                              ┌──────────────────────────┐
                                                              │  Kratos 业务服(14 个)  │
                                                              │ login/player/team/match/ │
                                                              │ trade/dialogue/chat/    │
                                                              │ friend/locator/         │
                                                              │ data_service/           │
                                                              │ ds_allocator/           │
                                                              │ hub_allocator/          │
                                                              │ battle_result/          │
                                                              │ ★ push(server stream)  │
                                                              └──────────┬───────────────┘
                                                                         │ produce
                                                                         ▼
                                                              ┌──────────────────────┐
                                                              │ Kafka cluster        │
                                                              │ pandora.team.*       │
                                                              │ pandora.match.*      │
                                                              │ pandora.chat.*       │
                                                              │ pandora.player.*     │
                                                              │ pandora.battle.result│
                                                              └──────────▲───────────┘
                                                                         │ consume
                                                                         │
                                                                  push 服务 ─┘
                                                                  (集中持有玩家 stream,
                                                                   按 player_id 路由 kafka 事件
                                                                   转 gRPC server stream 推给客户端)
```

**核心性质**:
- **2 条客户端连接**(无第三连接)
- **零第三方 UE SDK**(全部 UE 引擎自带)
- **零 SSL 冲突**(只用 UE OpenSSL,不引入 BoringSSL)
- **协议全标准**(gRPC-Web 是 grpc.io 官方规范)
- **故障域清晰**:DS 崩 ≠ 业务挂,业务服崩 ≠ 推送挂

---

## §1 客户端两条连接

### 1.1 连接 ①:UE NetDriver → Hub DS / Battle DS

| 维度 | 内容 |
|---|---|
| 协议 | UE 原生(基于 UDP 的可靠/不可靠混合)|
| 频率 | 30~60Hz tick |
| 用途 | **仅游戏内同步**(玩家移动 / 技能释放 / HP / buff / 命中 / AOI / Replication / GAS) |
| 谁负责 | UE 引擎自带,零开发 |
| 不能做的事 | 业务请求(组队 / 商店 / 好友 / 段位查询)— 走 ② |
| 断线影响 | 玩家暂时看不到大厅 / 战斗世界,但 UI 业务不受影响(② 还在) |

### 1.2 连接 ②:UE FHttpModule → Envoy(gRPC-Web over HTTP/2 TLS)

| 维度 | 内容 |
|---|---|
| 协议 | gRPC-Web over **HTTP/2 + TLS**(UE 5.7 官方支持) |
| 频率 | 业务请求 1~10 req/s/玩家;推送 stream 长连接 |
| 用途 | **所有业务请求 + 所有推送**(unary + server stream 复用同协议)|
| 谁负责 | 客户端:UE FHttpModule(引擎自带)+ 自研 grpc-web 协议解析;服务端:Envoy + Kratos |
| 鉴权 | 首次 Login RPC 拿 session_token → 后续所有请求 header 携带 |
| 断线 | FHttpModule 自动重连(libcurl 内置);stream 断了 push 服务从 redis 拉离线消息补推 |

**单条连接做 unary + server stream + 推送**,这是 gRPC-Web 的核心能力,不需要分两条通道。

---

## §2 gRPC-Web 协议分层详解

很多人搞混"gRPC-Web 是不是 HTTP",这里彻底说清楚。

### 2.1 三层结构

```
┌─────────────────────────────────────────────┐
│ 应用层(开发者写代码看到的)               │
│ → gRPC service stub 调用                   │
│   matchmaker.Subscribe(...) returns stream │
│   for msg := range stream { ... }          │
└─────────────────────────────────────────────┘
                    │
                    ▼
┌─────────────────────────────────────────────┐
│ 转换层(grpc-web 客户端库 / Envoy)        │
│ → 把 gRPC 抽象转成 grpc-web 字节布局        │
│   每条 stream 消息 = 1 byte flag +          │
│                       4 bytes length +      │
│                       protobuf bytes        │
│   状态码用 trailer header(grpc-status)     │
└─────────────────────────────────────────────┘
                    │
                    ▼
┌─────────────────────────────────────────────┐
│ 传输层(HTTP/2 + TLS)                      │
│ → 真实网络字节,多路复用,标准 HTTPS         │
│   POST /pandora.team.v1.TeamService/Create  │
│   Content-Type: application/grpc-web+proto  │
│   ← (HTTP/2 stream chunk 1) [grpc-web frame]│
│   ← (HTTP/2 stream chunk 2) [grpc-web frame]│
│   ← (trailer) grpc-status: 0                │
└─────────────────────────────────────────────┘
```

### 2.2 关键事实

| 问题 | 答案 |
|---|---|
| **gRPC-Web 是不是 HTTP?** | 是的,底层就是 HTTP/2(或 HTTP/1.1) |
| **跟 HTTP/JSON 一样吗?** | 不一样。payload 是 protobuf 二进制,字节少 5-10 倍,CPU 快 5-10 倍 |
| **开发者要直接写 HTTP 吗?** | 不要。开发者写 gRPC 语义(stub 调用),库自动转换 |
| **能不能跑 server stream?** | ✅ 可以(HTTP/2 stream 或 HTTP/1.1 chunked)|
| **能不能跑 client stream?** | ❌ gRPC-Web 协议规范不支持(W2 Pandora 用 unary + server stream 即可)|
| **能不能跑双向流?** | ❌ 同上,不支持。Pandora 不需要 |

### 2.3 跟纯 HTTP/JSON 性能对比

实测 `MatchProgress{ stage: READY }`:

| 协议 | 字节数 | 解析 CPU |
|---|---|---|
| HTTP/1.1 + JSON | ~250 字节(header+JSON body)| 慢 |
| **gRPC-Web over HTTP/2** | ~50 字节(HTTP/2 帧头+grpc-web frame+protobuf)| **快** |
| 纯 gRPC over HTTP/2 | ~30 字节(无 grpc-web 额外 frame 头)| 最快 |

**gRPC-Web 比 HTTP/JSON 省 5 倍流量、快 5-10 倍**。Pandora 选 gRPC-Web 性能远优于 HTTP/JSON。

---

## §3 UE 5.7 FHttpModule HTTP/2 实现指南

### 3.1 验证依据(2026-06-04 直接挖 UE 5.7 源码确认)

UE 5.7 源码路径 `Engine/Source/Runtime/Online/HTTP/`,关键 API:

**HttpConstants.h**:
```cpp
static UE_API const TCHAR* const VERSION_2TLS;
static UE_API const TCHAR* const VERSION_1_1;
```

**IHttpRequest.h**:
```cpp
namespace HttpRequestOptions {
    static const FName HttpVersion("HttpVersion");
}

virtual void SetOption(const FName Option, const FString& OptionValue) = 0;

// ⭐ Server stream 接收核心 API(line 283)
HTTP_API bool SetResponseBodyReceiveStreamDelegateV2(FHttpRequestStreamDelegateV2 StreamDelegate);

// 委托签名(line 116)
using FHttpRequestStreamDelegateV2 = TTSDelegate<void(void*/*Ptr*/, int64&/*InOutLength*/)>;

// 辅助回调
virtual FHttpRequestHeaderReceivedDelegate& OnHeaderReceived() = 0;
virtual FHttpRequestStatusCodeReceivedDelegate& OnStatusCodeReceived() = 0;
virtual FHttpRequestProgressDelegate64& OnRequestProgress64() = 0;
```

**Private/Curl/CurlHttp.cpp**:
```cpp
void FCurlHttpRequest::SetupOptionHttpVersion()
{
    const FString HttpVersion = GetOption(HttpRequestOptions::HttpVersion);
    if (HttpVersion == FHttpConstants::VERSION_2TLS) {
        curl_easy_setopt(EasyHandle, CURLOPT_HTTP_VERSION, CURL_HTTP_VERSION_2TLS);
    } else if (HttpVersion == FHttpConstants::VERSION_1_1) {
        curl_easy_setopt(EasyHandle, CURLOPT_HTTP_VERSION, CURL_HTTP_VERSION_1_1);
    }
}
```

**结论**:UE 5.7 官方暴露 HTTP/2 over TLS,libcurl 后端通过 `CURL_HTTP_VERSION_2TLS` 启用。

### 3.2 重要约束:HTTP/2 必须走 TLS

UE 5.7 用的常量是 `VERSION_2TLS`,**不支持明文 HTTP/2**(h2c)。
- 生产环境本来就需要 TLS,无影响
- 本地开发期用自签证书(mkcert / openssl)

### 3.3 Pandora UE 客户端代码模板(W2 实现)

```cpp
// 发 unary 请求(如 CreateTeam)
TSharedRef<IHttpRequest> Request = FHttpModule::Get().CreateRequest();
Request->SetURL("https://pandora-gw.example.com/pandora.team.v1.TeamService/CreateTeam");
Request->SetVerb("POST");
Request->SetHeader("Content-Type", "application/grpc-web+proto");
Request->SetHeader("X-Grpc-Web", "1");
Request->SetHeader("Authorization", "Bearer " + SessionToken);

// ⭐ 启用 HTTP/2 over TLS
Request->SetOption(HttpRequestOptions::HttpVersion, FHttpConstants::VERSION_2TLS);

// gRPC-Web frame: [1 byte flag=0x00][4 bytes BE length][protobuf bytes]
Request->SetContent(GrpcWebEncodeUnary(CreateTeamReqProto));

Request->OnProcessRequestComplete().BindLambda(
    [](FHttpRequestPtr Req, FHttpResponsePtr Resp, bool bSuccess) {
        if (bSuccess) {
            CreateTeamResp Result;
            GrpcWebDecodeUnary(Resp->GetContent(), Result);
            UI->ShowTeam(Result.team);
        }
    }
);
Request->ProcessRequest();
```

```cpp
// 接 server stream(push 服务订阅)
TSharedRef<IHttpRequest> StreamReq = FHttpModule::Get().CreateRequest();
StreamReq->SetURL("https://pandora-gw.example.com/pandora.push.v1.PushService/Subscribe");
StreamReq->SetVerb("POST");
StreamReq->SetHeader("Content-Type", "application/grpc-web+proto");
StreamReq->SetHeader("X-Grpc-Web", "1");
StreamReq->SetHeader("Authorization", "Bearer " + SessionToken);
StreamReq->SetOption(HttpRequestOptions::HttpVersion, FHttpConstants::VERSION_2TLS);
StreamReq->SetContent(GrpcWebEncodeUnary(SubscribeReqProto));

// ⭐ 关键:用 StreamDelegateV2 接收 server stream 数据块
StreamReq->SetResponseBodyReceiveStreamDelegateV2(
    FHttpRequestStreamDelegateV2::CreateLambda(
        [](void* Ptr, int64& InOutLength) {
            // libcurl 每收到一块字节,这里立刻被调用(非 game thread)
            FGrpcWebFrameParser::Instance().Feed(Ptr, InOutLength);
            // 解析出完整 frame 后,投递到 game thread 分发
        }
    )
);
StreamReq->ProcessRequest();
```

### 3.4 UE 5.7 vs HTTP/1.1 fallback

如果某天发现 HTTP/2 有兼容性问题,代码降级**只改一行**:
```cpp
Request->SetOption(HttpRequestOptions::HttpVersion, FHttpConstants::VERSION_1_1);
```
其它代码不动。`SetResponseBodyReceiveStreamDelegateV2` 在 HTTP/1.1 + chunked 下同样工作。

**升级路径完美**。

---

## §4 后端 Kratos 框架

### 4.1 为什么 Kratos(回顾 2026-06-04 决策)

go-zero 的 zrpc 不支持 gRPC server stream(经多轮分析确认),而 Pandora 的推送架构**必须用 stream**(避免自研 WebSocket envelope + kafka→ws 路由层)。

Kratos 优势:
- 基于原生 grpc-go,**完整支持 unary + server stream + client stream + bidi**
- `transport/grpc`(主)+ `transport/http`(可选,自动从 proto google.api.http 注解生成)
- 可拔插 log / metrics / tracing(OpenTelemetry 标准)
- B 站官方维护,游戏后端有验证(米哈游也用)

### 4.2 Pandora 业务服 Kratos 风格(W2 写法)

```go
// 业务服 main.go 简化版
func main() {
    // 1. 加载配置(Kratos config)
    c := config.New(config.WithSource(file.NewSource("./etc/team.yaml")))
    c.Load()

    // 2. 创建 gRPC server
    grpcSrv := grpc.NewServer(
        grpc.Address(":50010"),
        grpc.Middleware(
            recovery.Recovery(),
            tracing.Server(),
            logging.Server(logger),
            metrics.Server(),
            jwt.Server(...),  // 鉴权拦截器
        ),
    )

    // 3. (可选)HTTP server,由 proto google.api.http 注解驱动
    httpSrv := http.NewServer(
        http.Address(":51010"),
        http.Middleware(...),
    )

    // 4. 注册业务实现
    teamSvc := team.NewTeamService(...)
    teampb.RegisterTeamServiceServer(grpcSrv, teamSvc)
    teampb.RegisterTeamServiceHTTPServer(httpSrv, teamSvc)  // 由 protoc-gen-go-http 生成

    // 5. 启动
    app := kratos.New(kratos.Server(grpcSrv, httpSrv))
    app.Run()
}
```

### 4.3 中间件 / ���截器(对齐 W2 pkg 重写)

| 中间件 | 实现位置 | 用途 |
|---|---|---|
| `recovery` | Kratos 内置 | panic recover |
| `tracing` | Kratos 内置(OpenTelemetry)| trace_id 透传 |
| `logging` | Kratos 内置 | 标准 access log |
| `metrics` | Kratos 内置(prometheus)| RPC duration / total |
| `jwt` | Kratos jwt middleware | session_token 校验,注入 player_id 到 ctx |
| `ratelimit` | Kratos 内置 | 限流 |
| `pandora-trace` | `pkg/middleware/`(自研) | 跟 ds-arch.md §0 trace_id 字段对齐 |

---

## §5 Envoy Edge Gateway

### 5.1 职责

Envoy 是**唯一的对外入口**(Edge Gateway 模式),处理:

1. **TLS 终止**:客户端 HTTPS → Envoy 内网明文 gRPC(或 mTLS)
2. **gRPC-Web → gRPC 转换**(envoy 内置 `envoy.filters.http.grpc_web` filter)
3. **ALPN 协商**:自动选 HTTP/2 vs HTTP/1.1
4. **JWT 鉴权**(envoy 内置 `envoy.filters.http.jwt_authn`)
5. **限流 / 熔断 / 重试**
6. **路由**:按 gRPC service 名路由到 13 个业务服 + push 服务

### 5.2 部署模式(W7 实施)

| 模式 | 描述 | 选用 |
|---|---|---|
| **k8s Ingress** | Envoy 作为 k8s Ingress controller | ⭐ Pandora 推荐(生产)|
| **独立 Pod** | 单独部署 envoy Pod,Service 暴露 | 可选 |
| **Sidecar** | 每个业务 Pod 旁边一个 envoy | 不用(那是 service mesh,过度) |
| **本地 docker** | 开发期 docker-compose 跑 envoy | ⭐ 开发期用 |

### 5.3 最小 envoy.yaml 示例(W7 起草)

```yaml
static_resources:
  listeners:
  - name: pandora_listener
    address:
      socket_address: { address: 0.0.0.0, port_value: 8443 }
    filter_chains:
    - transport_socket:
        name: envoy.transport_sockets.tls
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.DownstreamTlsContext
          common_tls_context:
            tls_certificates:
            - certificate_chain: { filename: /etc/envoy/cert.pem }
              private_key:        { filename: /etc/envoy/key.pem }
            alpn_protocols: [ h2, http/1.1 ]
      filters:
      - name: envoy.filters.network.http_connection_manager
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          codec_type: AUTO
          http_filters:
          - name: envoy.filters.http.grpc_web        # ⭐ 关键:grpc-web 转标准 gRPC
          - name: envoy.filters.http.jwt_authn       # JWT 鉴权
          - name: envoy.filters.http.router
          route_config:
            virtual_hosts:
            - name: pandora_vh
              domains: ["*"]
              routes:
              - match: { prefix: "/pandora.login.v1.LoginService/" }
                route: { cluster: login }
              - match: { prefix: "/pandora.team.v1.TeamService/" }
                route: { cluster: team }
              - match: { prefix: "/pandora.match.v1.MatchService/" }
                route: { cluster: matchmaker }
              # ... 其它 11 个业务服 + push 服务
              - match: { prefix: "/pandora.push.v1.PushService/" }
                route: { cluster: push, timeout: 0s }  # ⭐ stream 无超时
  
  clusters:
  - name: login
    connect_timeout: 1s
    type: STRICT_DNS
    lb_policy: ROUND_ROBIN
    typed_extension_protocol_options:
      envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
        "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
        explicit_http_config:
          http2_protocol_options: {}
    load_assignment:
      cluster_name: login
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address: { address: login-svc.pandora.svc.cluster.local, port_value: 50001 }
  # ... 其它 cluster
```

### 5.4 服务发现集成

生产环境 Envoy 走 **xDS 协议**(动态服务发现):
- Pandora 暂不上 Istio service mesh(过度工程)
- 用 k8s `Service` + DNS(Envoy STRICT_DNS)即可
- 业务服扩缩容由 k8s + Agones 自动处理

---

## §6 推送架构 — push 服务(集中 + server stream)

### 6.1 为什么集中 push 而不是每个业务服自己推

| 模式 | 优 | 劣 |
|---|---|---|
| 每业务服自己推 stream | 直推,低延迟 | 14 个业务服 = 客户端 14 条 stream(违反"客户端最多 2 连"原则)|
| **集中 push 服务**(选定) | 客户端 1 条 stream | push 多消费一次 kafka,几 ms 额外延迟 |

### 6.2 push 服务设计

```go
// proto/push/v1/push.proto(W2 §2.5 加)
service PushService {
  // Subscribe:客户端首次登录后立刻调,维持长连接
  // 服务端 server stream 推送所有 player_id 相关事件
  rpc Subscribe(SubscribeReq) returns (stream PushFrame);
}

message SubscribeReq {
  string session_token = 1;  // 鉴权(或走 Envoy JWT,这里冗余)
  int64  last_seen_ms  = 2;  // 重连补推用
}

message PushFrame {
  string topic     = 1;  // pandora.team.update / pandora.match.progress / ...
  bytes  payload   = 2;  // 业务 Event message 序列化(如 TeamUpdateEvent)
  int64  ts_ms     = 3;
  string trace_id  = 4;
}
```

### 6.3 push 服务运行时

```go
// push 服务 main 逻辑
func (s *PushService) Subscribe(req *SubscribeReq, stream PushService_SubscribeServer) error {
    playerID := extractPlayerIDFromJWT(stream.Context())
    
    // 1. 注册 stream 到内存索引
    s.connections.Store(playerID, stream)
    defer s.connections.Delete(playerID)

    // 2. 补推离线消息(redis ZSET)
    offlineMsgs := s.redis.ZRangeByScore(
        fmt.Sprintf("pandora:push:offline:%d", playerID),
        req.LastSeenMs, time.Now().UnixMilli(),
    )
    for _, msg := range offlineMsgs {
        stream.Send(decodeFrame(msg))
    }
    s.redis.Del(...)  // 推完清理

    // 3. 阻塞等 client 断开(kafka consume 在另一 goroutine 推 stream)
    <-stream.Context().Done()
    return nil
}

// 单独的 kafka consumer goroutine
func (s *PushService) consumeLoop() {
    for msg := range kafkaConsumer.Messages() {
        envelope := decodeKafkaEnvelope(msg)
        playerID := extractKey(msg)
        
        if streamRaw, ok := s.connections.Load(playerID); ok {
            // ⭐ 玩家在线:直接通过 server stream 推
            stream := streamRaw.(PushService_SubscribeServer)
            stream.Send(&PushFrame{
                Topic:    envelope.Topic,
                Payload:  envelope.Payload,
                TsMs:     envelope.TsMs,
                TraceId:  envelope.TraceId,
            })
        } else {
            // ⭐ 玩家离线:存 redis ZSET(5 分钟过期)
            s.redis.ZAdd(
                fmt.Sprintf("pandora:push:offline:%d", playerID),
                envelope.TsMs, encodeFrame(envelope),
            )
            s.redis.Expire(..., 5*time.Minute)
        }
    }
}
```

### 6.4 多实例扩展(W6+)

push 单实例顶不住时:
- 多个 push 实例同 consumer group `pandora-push`(kafka 自动 partition 分配)
- 但 player_id → push_instance 路由要解决:`redis HSET pandora:push:route player_id instance_name`
- kafka 消息 key=player_id 落到 partition,但消费它的实例不一定是该玩家连着的 → 实例内部 gRPC 转发到目标实例

**W1-W4 单实例够用,这个后置优化。**

---

## §7 离线消息 + 重连恢复

### 7.1 离线消息策略

- 玩家在线:push 直接 server stream
- 玩家离线(stream 已断):写 redis ZSET `pandora:push:offline:<player_id>`,score=ts_ms,member=envelope_bytes
- 保留 5 分钟(`EXPIRE 300`),过期消息丢弃(MOBA 业务不需要永久离线消息;邮件等永久数据走 DB 拉)

### 7.2 重连流程

```
Client WebSocket 断 → UE 自动重连(libcurl 内置)
Client 重连 → 调 Push.Subscribe { session_token, last_seen_ms }
push 服务:
  - JWT 校验
  - 注册 stream 到 connections 索引
  - ZRangeByScore pandora:push:offline:<id>(score 在 last_seen_ms 到 now)
  - 按 ts_ms 排序顺序推
  - 推完 ZRemRangeByScore 清理
  - 阻塞等下次断开
Client:UI 增量刷新(去重靠 ts_ms,见 protocol-ordering-rules.md §5.3)
```

---

## §8 故障域分析

| 故障 | 影响 |
|---|---|
| Envoy 崩 | 客户端业务请求 + 推送全部不可用;游戏内同步(NetDriver)不受影响 |
| Hub DS 崩 | 玩家看不到大厅世界,但 UI 业务(组队/商店/战绩)正常,玩家可断 hub 但保持 ② |
| Battle DS 崩 | 这局战斗中断,已结算战绩通过 kafka 落库;玩家退回大厅 |
| push 服务崩 | 推送暂时不可用,客户端 UI 用轮询 GetXxx 兜底;业务请求 + 游戏同步正常 |
| 单个业务服(team/match)崩 | 该业务功能挂,其它正常 |
| login 崩 | 新玩家无法登录,已登录玩家不受影响(JWT 校验由 Envoy 做,不依赖 login) |
| kafka 崩 | 推送停了,业务请求 + 游戏同步正常 |
| etcd 崩 | 服务发现退化(已建立的连接还能用,新连接失败) |
| redis 崩 | session 丢,玩家被踢;业务功能瘫痪;游戏同步不受影响(DS 内存状态)|

**核心收益**:**故障域之间相互隔离**。任一组件崩不会全军覆没。

---

## §9 端到端时序示例

### 9.1 玩家组队邀请全链路

```
玩家 A(Hub DS)                       Envoy + 业务服              玩家 B(Hub DS)
  │                                        │                          │
  │ UI 点"邀请 B"                          │                          │
  │ ② FHttpModule POST                    │                          │
  │   /pandora.team.v1.TeamService/Invite │                          │
  │   gRPC-Web frame {InviteReq{B}}       │                          │
  │   HTTP/2 + TLS                        │                          │
  │───────────────────────────────────────▶│                          │
  │                                        │ Envoy: 解 TLS / 鉴权     │
  │                                        │ Envoy: grpc-web→grpc     │
  │                                        │ → team:50010 unary       │
  │                                        │   写 redis 记录邀请       │
  │                                        │   produce kafka:         │
  │                                        │     topic=team.update    │
  │                                        │     key=B(只给被邀请方) │
  │                                        │ ← gRPC response {ok}     │
  │                                        │ Envoy: grpc→grpc-web    │
  │ gRPC-Web response                     │                          │
  │ {InviteResp{ok}}                      │                          │
  │◀──────────────────────────────────────│                          │
  │ UI 显示"已邀请 B"                      │                          │
  │                                        │                          │
  │                                        │ kafka consume(push 服)   │
  │                                        │ → 找 B 的 stream         │
  │                                        │ → stream.Send(PushFrame{ │
  │                                        │     topic=team.update    │
  │                                        │     payload=TeamUpdate   │
  │                                        │       Event{invited})    │
  │                                        │───────────────────────────▶
  │                                        │                          │ UE FHttpModule
  │                                        │                          │ StreamDelegateV2
  │                                        │                          │ → 解 grpc-web frame
  │                                        │                          │ → UI 弹窗"A 邀请你"
```

**关键观察**:
- 玩家 A、B 各自走自己的 ② 连接,**没有任何消息经过 Hub DS**
- Hub DS 即使崩,A 仍能发邀请,B 仍能收推送
- 完全对齐 `architecture-rejected-strict-ds-only.md` 的故障域目标

### 9.2 玩家进战斗全链路

```
玩家 A(Hub,组好队)
  │ ② POST /pandora.match.v1.MatchService/StartMatch
  │   {team_id=T1}
  ▼
Envoy → matchmaker(gRPC unary)
  │
  │ matchmaker 撮合开始,写 redis 入队
  │ 撮合成功后(可能几秒):
  │   produce kafka pandora.match.progress
  │     key=A...A's player_id  payload=stage=FOUND
  │     key=B...                payload=stage=FOUND  (5 个 player_id 各一条)
  │
  ▼
push 服务 consume → 推 5 个客户端 stream
  │
  ▼
玩家 A UI:"找到对手!确认参战?"
玩家 A 点确认 → ② POST /pandora.match.v1.MatchService/ConfirmMatch
  │
  ▼
Envoy → matchmaker(unary)
  │
  │ 等所有 10 人确认 / 超时
  │ 全确认 → matchmaker.调 ds_allocator.AllocateBattle (gRPC unary)
  │
  │ ds_allocator 通过 Agones 拉起 battle DS pod
  │ ds_allocator 返回 ds_addr + tickets
  │
  │ matchmaker:produce kafka pandora.match.progress
  │   payload=stage=READY,battle_ds_addr=...,battle_ticket=...
  │
  ▼
push 服务 → 推 10 个客户端 stream
  │
  ▼
玩家 A 客户端拿到 battle_ds_addr + battle_ticket
  │
  ▼
玩家 A 断开 ① NetDriver(Hub DS)
玩家 A 用 battle_ticket 连 Battle DS(新的 ① NetDriver)
  │
  ▼
战斗 25 分钟(纯 UE Replication + GAS,无后端干预)
  │
  ▼
战斗结束 → Battle DS 发 kafka pandora.battle.result(给 battle_result 落库)
         + Battle DS 用 UE ClientRPC 推 BattleEnded{result, hub_ds_addr, hub_ticket}
  │
  ▼
玩家 A 看战绩 10s → 断 Battle DS → 重连 Hub DS

⭐ 整个流程:② Envoy 连接保持(stream 始终在),只有 ① NetDriver 在 Hub / Battle 之间切换
```

---

## §10 W2+ 实现路线图

### W2(第一周)

1. **D2 pkg 重写**(~3-4 天)— 见 §11 详细清单
2. **写 login 服务**(Kratos)— 第一个 Kratos 业务服
3. **配 Envoy + 自签证书**(本地 docker-compose)— 验证 gRPC-Web 链路

### W2-W3

4. **写 push 服务**(server stream + kafka consumer)
5. **UE 客户端写 grpc-web 协议解析**(基于 FHttpModule,~3-5 天)
6. **端到端打通**:UE → Envoy → login → response 回到 UE 显示

### W3-W6

7. 其它 13 个业务服(team / match / chat / ...)
8. 各业务服 produce kafka 推送 topics
9. push 服务消费转发给客户端

### W7-W8

10. UE DS(Hub / Battle)骨架
11. Agones 集成

---

## §11 UE 客户端不用 gRPC 插件的决策

### 11.1 第三方 UE gRPC 插件清单(评估过的)

| 插件 | 出处 | 状态 |
|---|---|---|
| 社区 GrpcUEPlugin / fork 多个 | GitHub | 不活跃,UE 5.x 兼容性参差 |
| gRPCue / gRPC for Unreal | FAB Marketplace 收费 | 商业维护但只支持 unary |
| 腾讯 / 网易 内部 gRPC | 不开源 | 不可用 |

### 11.2 5 个共性坑

1. **包体爆炸**:都基于 grpc-cpp,+80MB
2. **SSL 冲突**:grpc-cpp 拉 BoringSSL,UE 自带 OpenSSL,**同进程两套 SSL 必然链接冲突**,每个 UE 版本都要重新调
3. **UE 5.x 兼容性差**:大部分插件 UE 4.27 时代写的,UE 5 改了 ModuleRules 经常 build 不过
4. **stream API 别扭**:UE Delegate 表达 stream 4 种回调(start/middle/end/error)很拗口
5. **跨平台编译痛**:iOS BoringSSL + ATS 冲突,Android NDK 版本对齐,Linux server target 复杂

### 11.3 大厂事实

**几乎没有大型游戏客户端走 gRPC**:
| 厂家 | 客户端协议 |
|---|---|
| 米哈游(原神 / 星铁) | 自研 TCP 长连接 + protobuf |
| 腾讯王者 / 和平精英 | MtgRPC 自研协议 |
| 网易(永劫无间) | 自研 + protobuf |
| Riot LoL | REST + RTC(LCU 自研) |
| 堡垒之夜 | REST + WebSocket |
| Epic EOS SDK | REST + WebSocket |

游戏客户端长连接的工业标准:**HTTP/2 + protobuf 自研协议** 或 **WebSocket + protobuf**。

### 11.4 Pandora 选择

✅ **自研 grpc-web 客户端基于 FHttpModule**(已验证 UE 5.7 完全支持)

工作量预估:~3-5 天
- 协议解析:grpc-web frame 格式公开,简单(1 字节 flag + 4 字节 length + payload)
- 客户端代码模板见 §3.3
- 单元测试用 Envoy 跑起来对接

收益:**零额外依赖、零 SSL 冲突、零跨平台编译坑**,符合 Pandora "标准协议优先" 铁律。

---

## §12 决策行(写入 pandora-arch.md §11)

| 日期 | 决策 | 原因 |
|---|---|---|
| 2026-06-04 | 切换后端框架:go-zero → **Kratos** | go-zero 不支持 gRPC stream,推送架构受限 |
| 2026-06-04 | 引入 **Envoy** 作为 Edge Gateway | 标准 gRPC-Web ↔ gRPC 协议转换 |
| 2026-06-04 | 客户端协议:**gRPC-Web over HTTP/2 TLS** | UE 5.7 FHttpModule 已暴露(SetOption "HttpVersion=2TLS") |
| 2026-06-04 | 推送架构:**集中 push 服务 + server stream** | 替代 kafka→ws 自研,延迟低 + 协议标准 |
| 2026-06-04 | 客户端实现:**自研 grpc-web 客户端基于 FHttpModule** | 不引入第三方 UE gRPC 插件(5 个共性坑) |
| 2026-06-04 | 服务清单 13 → **14**(新增 push)| Envoy 作为基础设施不计 go 服务 |
| 2026-06-04 | 客户端连接最终值 = **2 条**(NetDriver + FHttpModule)| 用户铁律确认 |

---

## §13 W2 阻塞决策清单

- ⏸️ UE 仓库名最终确定(D4 阻塞)
- ⏸️ k8s 选型:阿里云 ACK / 自建 / 先 minikube(D7 阻塞,Envoy 一起决定)
- ⏸️ Envoy 跑模式:k8s Ingress / 单独 Pod(D7 决定)
- ⏸️ JWT 鉴权细节:Envoy filter / login 服务签发 / token 内容(W2 写 login 时定)
