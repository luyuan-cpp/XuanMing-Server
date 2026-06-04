# Pandora pkg 拷贝清单

> D2 执行依据。**只允许一次性拷贝**,之后 Pandora pkg 与 mmorpg shared 独立演化。

## 1. mmorpg 现状盘点(已扫描)

mmorpg 公共代码分散在 5 处:
- `F:/work/mmorpg/go/shared/`(13.9K 行,含 generated 表)
- `F:/work/mmorpg/go/common/proto/etcd`(proto 定义)
- `F:/work/mmorpg/go/contracts/proto/etcd`(proto 定义)
- `F:/work/mmorpg/go/etcd/`(框架,实际只有 proto/)
- 各服务 `internal/` 下重复出现的 config/kafka/metrics(说明应该抽公共,**这是个改造机会**)

**关键依赖**:`go-redis/v9 / sarama / go-zero / grpc / segmentio-kafka-go`,Go 版本 1.24.5(我们 1.23,需调整 require)。

## 2. 拷贝清单(分类)

### 🟢 直接拷(改 import path 即可)

| 源路径 | 目标路径 | 行数 | 说明 |
|---|---|---|---|
| `mmorpg/go/shared/snowflake/` | `Pandora/pkg/snowflake/` | 82 行 | ID 生成,纯算法零业务耦合 |
| `mmorpg/go/shared/cache/` | `Pandora/pkg/cache/` | 89 行 | 泛型 cache-aside + singleflight |
| `mmorpg/go/shared/grpcstats/` | `Pandora/pkg/grpcstats/` | 347 行 | gRPC 调用指标采集器 |
| `mmorpg/go/db/internal/locker/` | `Pandora/pkg/redislock/` | 131 行 | redis 分布式锁(SetNX + Lua release/extend) |

### 🟡 拷 + 重写(剥 MMO 业务,抽公共)

| 源路径 | 目标 | 处理 |
|---|---|---|
| `mmorpg/go/shared/kafkautil/topic_init.go` | `Pandora/pkg/kafka/topics.go` | 拷,但 topic 名全部重新设计(pandora.* topic) |
| `mmorpg/go/shared/kafkautil/expand_utils.go` | `Pandora/pkg/kafka/util.go` | 拷,通用工具 |
| `mmorpg/go/shared/kafkautil/gate_push.go` | ❌ **不拷** | 跟 cpp gate 强绑定,Pandora 没 gate |
| `mmorpg/go/login/internal/kafka/key_ordered_producer.go` | `Pandora/pkg/kafka/producer.go` | 抽公共,改名 |
| `mmorpg/go/db/internal/kafka/key_ordered_consumer.go` | `Pandora/pkg/kafka/consumer.go` | 抽公共,改名 |
| `mmorpg/go/login/internal/config/config.go`(236 行,最完整)| `Pandora/pkg/config/` | **大改**:剥 LegacyGate / SaToken / SceneManager / GateTokenSecret 等 MMO 字段;基础结构(Snowflake / Locker / Kafka / Etcd / Timeouts / RedisConf)保留 |
| 各服务 `internal/metrics/metrics.go` | `Pandora/pkg/metrics/` | 抽 prometheus 注册器 + 标准指标(rpc_duration、kafka_lag、db_query) |
| 各服务 `internal/svc/servicecontext.go` | `Pandora/pkg/svc/` 模板 | 抽服务初始化模板(connect mysql/redis/etcd/kafka) |

### 🟡 框架级(需要从服务代码倒推抽公共)

| 模块 | 来源 | 处理 |
|---|---|---|
| **gRPC server 框架** | 各服务 `internal/server/` | 抽 `Pandora/pkg/grpcserver/`(拦截器:log / metrics / tracing / recover / auth) |
| **gRPC client 框架** | 散落在各服务 client/ | 抽 `Pandora/pkg/grpcclient/`(连接池 + 重试 + 熔断) |
| **log 框架** | 各服务直接用 `go-zero logx` 或标准 log | 抽 `Pandora/pkg/log/`(zap 封装 + 结构化字段约定) |
| **错误码** | 散落 | 新建 `Pandora/pkg/errcode/`(MOBA 错误码段全新规划,见 `docs/design/proto-design.md` §4) |

### 🔴 不拷(MOBA 用不上)

| 源路径 | 不拷的原因 |
|---|---|
| `mmorpg/go/scene_manager/` 全部 | 跟 cpp scene 强绑定,Pandora 用 ds_allocator |
| `mmorpg/go/shared/generated/table/world_table*.go` | MMO 配置表,Pandora 重新设计 |
| `mmorpg/go/login/internal/dispatcher/` | MMO 任务结果分发,Pandora 用不到 |
| `mmorpg/go/data_service/internal/store/zone_snapshot.go` | MMO zone 快照,Pandora 没 zone 概念 |
| `mmorpg/go/data_service/internal/routing/` | MMO 多 zone 路由,Pandora 用 hub/battle 分片 |
| `mmorpg/go/db/internal/stresstest/` | mmorpg 专属压测,但**压测脚本套路可以借鉴** |
| `mmorpg/go/contracts/proto/` / `mmorpg/go/common/proto/` / `mmorpg/go/proto/` 全部 .proto | MMO 协议,Pandora 完全重写 |

## 3. Pandora/pkg/ 目录最终结构

```
Pandora/pkg/
├── log/              # zap 封装,结构化日志约定
├── metrics/          # prometheus 注册器 + 标准指标
├── config/           # viper / go-zero conf + etcd 配置中心
├── grpcserver/       # gRPC server 框架(拦截器全套)
├── grpcclient/       # gRPC client(连接池/重试/熔断)
├── grpcstats/        # 直接拷 mmorpg(347 行)
├── kafka/            # producer + consumer + topic 注册
├── etcdregistry/     # 服务注册发现
├── redislock/        # redis 分布式锁(直接拷 mmorpg locker)
├── cache/            # 泛型 cache-aside(直接拷 mmorpg)
├── snowflake/        # ID 生成(直接拷 mmorpg)
├── errcode/          # 错误码框架(新写)
└── svc/              # 服务初始化模板
```

## 4. 工作量评估

| 阶段 | 工作 | 时间 |
|---|---|---|
| 直接拷 + 改 import | 4 个 🟢 模块 | 0.5 天 |
| 抽公共重写(config/metrics/log/grpc 框架) | 4 个 🟡 模块 | 2~3 天 |
| 新写(errcode 框架、proto 同步工具) | 2 个 模块 | 1 天 |
| 跑通 `go build ./pkg/...` 全绿 + 单测 | - | 0.5 天 |
| **合计** | - | **4~5 天** |

⚠️ **D2 实际可能跨 D2-D3**,文档和 proto 设计可以并行进行。

## 5. 框架决策(2026-06-04 终版:Kratos,推翻 go-zero)

### 5.0 决策演化历史(只追加,不删旧)

| 日期 | 决策 | 状态 |
|---|---|---|
| 2026-06-03 | D2.1 选 **go-zero**(复用 mmorpg 90% 代码,4~5 天)| ❌ **已推翻** |
| 2026-06-04 | 切换 **Kratos**(支持 gRPC server stream,大厂 / 最标准方案优先)| ✅ **当前决策** |

### 5.1 ❌ 旧决策(go-zero,2026-06-03,已推翻)

> 仅供历史参考,不再有效。下面整段保留是为了让未来 AI / 开发者理解为什么之前选 go-zero、为什么推翻。

**当时理由**:
- 复用 mmorpg 90% 公共代码,D2 工作量 4~5 天(原生 grpc-go 自研要 10+ 天)
- 单人开发节奏:先快速跑通 W1-W4,后期(W12+)如遇限制再考虑迁移
- mmorpg 已稳定使用 go-zero 1.9.x,踩坑期已过

**推翻原因**(2026-06-04):
1. **go-zero zrpc 不支持 gRPC server stream**(经多轮验证)
2. Pandora 推送架构本应走 gRPC server stream,go-zero 限制下要自研 WebSocket + envelope + kafka 转 ws 路由层(~6 天)
3. 自研 ws 协议**违反"协议标准化"铁律**(2026-06-04 用户明确要求"大厂 + 最标准方案")
4. 切换 Kratos 工作量(~4 天)≈ 自研 ws(~6 天),**还省一次返工**
5. UE 5.7 FHttpModule 已暴露 HTTP/2(用户挖源码验证),客户端用 gRPC-Web 协议跟后端 Kratos + Envoy 一气贯通

### 5.2 ✅ 新决策(Kratos,2026-06-04)

**Pandora 后端框架统一用 Kratos**(B 站官方维护,基于原生 grpc-go)。

**理由**:
- 完整支持 gRPC unary + server stream + client stream + bidi(go-zero 只 unary)
- 推送架构能用 gRPC server stream(替代自研 WebSocket)
- 可拔插 log / metrics / tracing(OpenTelemetry 标准)
- proto-first + protoc-gen-go-http(同 proto 生成 gRPC + HTTP 两套)
- B 站 / 米哈游游戏后端有验证
- 跟 Envoy 配合自然(都是标准 gRPC,无非标转换层)

**架构组合**:
```
Client(UE FHttpModule)→ gRPC-Web over HTTP/2 TLS
                          ↓
Envoy(Edge Gateway)→ gRPC-Web ↔ gRPC 协议转换
                       ↓
14 个 Kratos 业务服 ↔ 14 个 gRPC unary + push 服务 server stream
```

**详见**:`gateway-decision.md`(Kratos + Envoy + gRPC-Web 完整设计)

### 5.3 D2 已写的 pkg/ 怎么处理(W2 第一周做)

D2 已写代码用 go-zero,需要部分重写:

| 模块 | 操作 | 估时 |
|---|---|---|
| `pkg/snowflake` | 不动(纯算法零依赖)| 0 |
| `pkg/cache` | 不动 | 0 |
| `pkg/errcode` | 不动 | 0 |
| `pkg/metrics` | 不动(prometheus 通用)| 0 |
| `pkg/redislock` | 改 `logx` → `kratos log` | 0.2 天 |
| `pkg/grpcstats` | 改 `logx` → `kratos log`,可能用 Kratos middleware 重新实现 | 0.5 天 |
| `pkg/kafkax` | 改 `logx` → `kratos log` | 0.2 天 |
| `pkg/svc` | 改 BaseContext 嵌入的 config 类型 | 0.2 天 |
| `pkg/log` | **重写**:基于 Kratos log 接口 + zap 实现 | 0.5 天 |
| `pkg/config` | **重写**:基于 Kratos config(viper / 文件 / etcd) | 0.5 天 |
| `pkg/grpcserver` | **重写**:基于 Kratos `transport/grpc` + middleware | 0.7 天 |
| `pkg/grpcclient` | **重写**:基于 Kratos `transport/grpc` client + middleware | 0.5 天 |
| 新增 `pkg/transport/http` | Kratos `transport/http` 包装(给 Envoy 转过来的请求用)| 0.5 天 |
| 新增 `pkg/middleware` | trace / auth / metrics / recover 拦截器 Kratos 风格 | 0.7 天 |

**总计:~4.5 天**(W2 第一周专注做这件事)。

**所以 W2 起所有 14 个服务的初始化模板锁定 Kratos 风格**。

## 6. ❌ 旧落地步骤(基于 go-zero 决策,已废弃)

> 下面段落保留作历史参考。新落地步骤见 §5.3 表格 + W2 plan(W2 开工时另开 plan 模式定具体执行清单)。

1. ~~决策 D2.1~~(已推翻,见 §5.0)
2. 拷 4 个 🟢 模块,改 import,跑 `go build`
   - `mmorpg/go/shared/snowflake/` → `Pandora/pkg/snowflake/`
   - `mmorpg/go/shared/cache/` → `Pandora/pkg/cache/`
   - `mmorpg/go/shared/grpcstats/` → `Pandora/pkg/grpcstats/`
   - `mmorpg/go/db/internal/locker/` → `Pandora/pkg/redislock/`
3. ~~抽 `pkg/config/`(保留 `zrpc.RpcServerConf` 嵌入)~~(改 Kratos config,见 §5.3)
4. ~~`pkg/log/`(直接用 `go-zero/core/logx`)~~(改 Kratos log + zap)
5. 抽 `pkg/metrics/`(prometheus 注册器 + 标准指标声明工具)
6. ~~抽 `pkg/grpcserver/`(go-zero zrpc 拦截器)~~(改 Kratos transport/grpc + middleware)
7. ~~抽 `pkg/grpcclient/`(go-zero zrpc.MustNewClient)~~(改 Kratos transport/grpc client)
8. 抽 `pkg/kafka/`(producer + consumer 包装 sarama)— 仍有效,改 log 即可
9. ~~`pkg/etcdregistry/`(go-zero zrpc 内置 etcd discovery)~~(Kratos 也有 registry/etcd)
10. 写 `pkg/errcode/`(对照 `proto-design.md` §4 错误码段)— 仍有效
11. ~~写 `pkg/svc/`(servicecontext 模板)~~(改 Kratos 风格 BaseContext)
12. 跑 `go build ./pkg/...` 全绿
13. 写 `pkg/<each>/<each>_test.go` 至少冒烟测试
14. 在 `PROGRESS.md` 追加 D2 完成记录
