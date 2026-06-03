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

## 5. D2.1 框架决策(已定:go-zero)

**2026-06-03 决策:Pandora 后端框架继续用 `go-zero`**

理由:
- 复用 mmorpg 90% 公共代码,D2 工作量 4~5 天(原生 grpc-go 自研要 10+ 天)
- 单人开发节奏:先快速跑通 W1-W4,后期(W12+)如遇限制再考虑迁移
- mmorpg 已稳定使用 go-zero 1.9.x,踩坑期已过

后期如要迁移,补偿成本:
- ~13 个服务 servicecontext.go 重写
- log 从 logx 迁 zap
- config 从 zrpc.RpcServerConf 改成自定义结构
- 估 1~2 个月返工(可控)

**所以 W2 起所有 13 个服务的 servicecontext 模板锁定 go-zero 风格**。

## 6. 落地步骤(D2 执行清单,基于 go-zero 决策)

1. ~~决策 D2.1~~(已定:go-zero)
2. 拷 4 个 🟢 模块,改 import,跑 `go build`
   - `mmorpg/go/shared/snowflake/` → `Pandora/pkg/snowflake/`
   - `mmorpg/go/shared/cache/` → `Pandora/pkg/cache/`
   - `mmorpg/go/shared/grpcstats/` → `Pandora/pkg/grpcstats/`
   - `mmorpg/go/db/internal/locker/` → `Pandora/pkg/redislock/`
3. 抽 `pkg/config/`(以 mmorpg login config 为骨架,**保留 `zrpc.RpcServerConf` 嵌入**,剥 LegacyGate / SaToken / SceneManager 等 MMO 字段)
4. `pkg/log/`(直接用 `go-zero/core/logx`,不抽包,服务直接 import)
5. 抽 `pkg/metrics/`(prometheus 注册器 + 标准指标声明工具)
6. 抽 `pkg/grpcserver/`(go-zero zrpc + 自定义拦截器:trace_id 注入 / metrics / panic recover)
7. 抽 `pkg/grpcclient/`(go-zero zrpc.MustNewClient 包装 + 重试 + 熔断)
8. 抽 `pkg/kafka/`(producer + consumer 包装 sarama)
9. `pkg/etcdregistry/`(直接用 go-zero zrpc 内置 etcd discovery,这一项可以不抽)
10. 写 `pkg/errcode/`(对照 `proto-design.md` §4 错误码段)
11. 写 `pkg/svc/`(服务初始化模板:连接 mysql/redis/etcd/kafka 的标准流程)
12. 跑 `go build ./pkg/...` 全绿
13. 写 `pkg/<each>/<each>_test.go` 至少冒烟测试
14. 在 `PROGRESS.md` 追加 D2 完成记录
