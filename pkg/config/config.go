// Package config 提供 Pandora 服务的通用配置结构。
//
// 设计:
//   - 基础字段(Server/Node/Redis/Kafka/Snowflake/Locker/Registry/Timeouts)集中放 Base
//   - 各服务的 internal/conf/conf.go 嵌入 Base 并加业务字段
//   - 配置加载用 Kratos config(W2 file source,W3+ 接 etcd)
//
// 跟之前 go-zero 版本的区别(2026-06-04 重写):
//   - 删 zrpc.RpcServerConf 嵌入,改 Pandora 自定义 Server 结构
//   - go-zero LogConf 改 zap(详见 pkg/log/log.go)
//   - 字段保留 mmorpg 拷过来的语义,但风格按 Kratos 惯例(yaml + protobuf 都能映射)
//
// 文件加载示例(各服务 main.go):
//
//	c := kconfig.New(kconfig.WithSource(file.NewSource("./etc/login-dev.yaml")))
//	if err := c.Load(); err != nil { panic(err) }
//	var cfg config.Base
//	if err := c.Scan(&cfg); err != nil { panic(err) }
package config

import (
	"fmt"

	"github.com/IBM/sarama"
)

// Base 是所有 Pandora 服务的通用配置基类。
//
// 各业务服务 internal/conf/conf.go 模板:
//
//	type Config struct {
//	    config.Base `yaml:",inline"`                       // 公共
//	    BusinessKnob int    `yaml:"business_knob" json:"business_knob"` // 业务私有
//	}
type Base struct {
	// Server 监听配置(gRPC + HTTP)
	Server Server `yaml:"server" json:"server"`

	// Node 节点级配置(redis 客户端、session 超时等)
	Node NodeConfig `yaml:"node" json:"node"`

	// Snowflake 全局 ID 生成参数
	Snowflake SnowflakeConf `yaml:"snowflake,omitempty" json:"snowflake,omitempty"`

	// Locker 分布式锁默认 TTL
	Locker LockerConf `yaml:"locker,omitempty" json:"locker,omitempty"`

	// Registry 服务注册发现(W2 用 file 配置,W3+ 接 etcd)
	Registry RegistryConf `yaml:"registry,omitempty" json:"registry,omitempty"`

	// Timeouts 各种通用超时
	Timeouts TimeoutConf `yaml:"timeouts,omitempty" json:"timeouts,omitempty"`

	// Kafka 生产者/消费者通用配置
	Kafka KafkaConfig `yaml:"kafka,omitempty" json:"kafka,omitempty"`

	// KillSwitch RPC 级临时关停(Kill-Switch)
	KillSwitch KillSwitchConf `yaml:"killswitch,omitempty" json:"killswitch,omitempty"`
}

// Server Kratos 风格的 server 监听配置(替代 go-zero zrpc.RpcServerConf)。
type Server struct {
	Grpc Grpc `yaml:"grpc" json:"grpc"`
	Http Http `yaml:"http,omitempty" json:"http,omitempty"` // 可选,W2 大部分服务只暴露 gRPC
}

// Grpc gRPC server 监听。
//
// EnableReflection(W3 ③,2026-06-05):
//
//   - true:保留 Kratos 默认的 grpc.reflection 注册(grpcurl list 可用,便于联调)
//
//   - false(默认):pkg/grpcserver.MustNewServer 会加 kgrpc.DisableReflection() 关掉
//
//     prod 默认不写本字段(零值 false)= 关 reflection,避免攻击面额外暴露。
//     dev yaml 显式写 enable_reflection: true 打开。
type Grpc struct {
	Network          string   `yaml:"network,omitempty" json:"network,omitempty"`                     // 默认 "tcp"
	Addr             string   `yaml:"addr" json:"addr"`                                               // 例 ":50001"
	Timeout          Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`                     // 默认 1s
	EnableReflection bool     `yaml:"enable_reflection,omitempty" json:"enable_reflection,omitempty"` // dev:true; prod:false(默认)
	EnableRateLimit  bool     `yaml:"enable_rate_limit,omitempty" json:"enable_rate_limit,omitempty"` // 第4层 BBR 自适应限流;dev:false; prod:true
}

// Http HTTP server 监听(给 protoc-gen-go-http 生成的 handler 用)。
type Http struct {
	Network string   `yaml:"network,omitempty" json:"network,omitempty"`
	Addr    string   `yaml:"addr" json:"addr"` // 例 ":51001"
	Timeout Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// NodeConfig 节点级配置。
type NodeConfig struct {
	// ZoneId 是分服 ID。Pandora 单服模式默认填 1。
	ZoneId           uint32    `yaml:"zone_id" json:"zone_id"`
	SessionExpireMin uint32    `yaml:"session_expire_min,omitempty" json:"session_expire_min,omitempty"` // 默认 1440 (24h)
	RedisClient      RedisConf `yaml:"redis_client" json:"redis_client"`
	MySQLClient      MySQLConf `yaml:"mysql_client,omitempty" json:"mysql_client,omitempty"`             // W3 ② 起接 mysql 的服务用
	LeaseTTL         int64     `yaml:"lease_ttl,omitempty" json:"lease_ttl,omitempty"`                   // 秒,默认 10
	MaxLoginDuration Duration  `yaml:"max_login_duration,omitempty" json:"max_login_duration,omitempty"` // 默认 24h
	LogoutGraceTime  Duration  `yaml:"logout_grace_time,omitempty" json:"logout_grace_time,omitempty"`   // 默认 5m
}

// MySQLConf MySQL 客户端配置(W3 ②,2026-06-05)。
//
// DSN 示例(login 服务连 pandora_account 库):
//
//	pandora:pandora_dev_pwd@tcp(127.0.0.1:3307)/pandora_account?parseTime=true&loc=UTC&charset=utf8mb4&collation=utf8mb4_0900_ai_ci
//
// W3 ⑥(2026-06-05):duration 字段改用 config.Duration 包装类型,yaml 可写 "30m"/"3s" 字符串。
type MySQLConf struct {
	DSN             string   `yaml:"dsn" json:"dsn"`
	MaxOpenConns    int      `yaml:"max_open_conns,omitempty" json:"max_open_conns,omitempty"`
	MaxIdleConns    int      `yaml:"max_idle_conns,omitempty" json:"max_idle_conns,omitempty"`
	ConnMaxLifetime Duration `yaml:"conn_max_lifetime,omitempty" json:"conn_max_lifetime,omitempty"`
	PingTimeout     Duration `yaml:"ping_timeout,omitempty" json:"ping_timeout,omitempty"`

	// Shards 是分库 DSN 列表。留空 = 单库(用 DSN)。配置 >=2 个 DSN = 分库模式,
	// 由 mysqlx.NewShardSet 按 snowflake 业务 ID 路由(shard = id % len(Shards))。
	// DAU 200万 / 千万注册量级下,单 MySQL 实例的写吞吐与单表行数都会触顶,按 player_id
	// 水平分库分表;分片数一旦定稿不可随意改(rehash 代价高),详见 docs/design/scale-dau-2m.md。
	// 池参数(MaxOpenConns 等)每个分片各自套用本结构的同名字段。
	Shards []string `yaml:"shards,omitempty" json:"shards,omitempty"`
}

// RedisConf Redis 客户端配置。
//
// W3 ⑥(2026-06-05):duration 字段改用 config.Duration,yaml 可写 "2s"/"30s" 字符串。
type RedisConf struct {
	Host         string   `yaml:"host" json:"host"`
	Password     string   `yaml:"password,omitempty" json:"password,omitempty"`
	DB           uint32   `yaml:"db,omitempty" json:"db,omitempty"`
	DefaultTTL   Duration `yaml:"default_ttl,omitempty" json:"default_ttl,omitempty"`
	DialTimeout  Duration `yaml:"dial_timeout,omitempty" json:"dial_timeout,omitempty"`
	ReadTimeout  Duration `yaml:"read_timeout,omitempty" json:"read_timeout,omitempty"`
	WriteTimeout Duration `yaml:"write_timeout,omitempty" json:"write_timeout,omitempty"`

	// Addrs 是 Redis Cluster / Sentinel 多节点地址。
	//
	// 留空 = 单实例模式(用 Host)。配置 >=1 个地址 = 集群模式,由 redisx.NewUniversalClient
	// 构造 ClusterClient(go-redis UniversalClient 会按 Addrs 数量与 MasterName 自动选型)。
	// DAU 200万 / 高 CCU 阶段单 Redis 必然成为吞吐与连接数单点,改配 Addrs 上 Redis Cluster,
	// 分片键用业务 ID(player_id / team_id);详见 docs/design/scale-dau-2m.md。
	Addrs []string `yaml:"addrs,omitempty" json:"addrs,omitempty"`

	// MasterName 非空 = 走 Sentinel 故障转移(FailoverClient);为空且 Addrs 多节点 = Cluster。
	MasterName string `yaml:"master_name,omitempty" json:"master_name,omitempty"`

	// MaintNotifications 控制 go-redis 的 CLIENT MAINT_NOTIFICATIONS 能力探测。
	//
	// 取值:"disabled" / "auto" / "enabled";留空 = "disabled"(项目默认)。
	// 自建 Redis(本地 / k8s 内 Redis 7.x)不支持该云厂商维护通知,默认关闭探测,
	// 避免 go-redis 启动时打印 "maintnotifications disabled due to handshake error" 噪音日志。
	// 仅当接 Redis Cloud / Enterprise 需要无缝故障转移时,才显式设为 "auto" / "enabled"。
	// 由 pkg/redisx.NewClient 解析,非法值安全回退到 disabled。
	MaintNotifications string `yaml:"maint_notifications,omitempty" json:"maint_notifications,omitempty"`
}

// KafkaConfig Kafka 生产/消费通用配置。
//
// W3 ⑥(2026-06-05):duration 字段改用 config.Duration,yaml 可写 "5s"/"100ms" 字符串。
type KafkaConfig struct {
	Brokers          []string `yaml:"brokers" json:"brokers"`
	GroupID          string   `yaml:"group_id,omitempty" json:"group_id,omitempty"`
	PartitionCnt     int32    `yaml:"partition_cnt,omitempty" json:"partition_cnt,omitempty"`         // 默认 4
	InitialPartition int      `yaml:"initial_partition,omitempty" json:"initial_partition,omitempty"` // 默认 4
	DialTimeout      Duration `yaml:"dial_timeout,omitempty" json:"dial_timeout,omitempty"`
	ReadTimeout      Duration `yaml:"read_timeout,omitempty" json:"read_timeout,omitempty"`
	WriteTimeout     Duration `yaml:"write_timeout,omitempty" json:"write_timeout,omitempty"`
	RetryMax         int      `yaml:"retry_max,omitempty" json:"retry_max,omitempty"`
	RetryBackoff     Duration `yaml:"retry_backoff,omitempty" json:"retry_backoff,omitempty"`
	ChannelBuffer    int      `yaml:"channel_buffer,omitempty" json:"channel_buffer,omitempty"`
	SyncInterval     Duration `yaml:"sync_interval,omitempty" json:"sync_interval,omitempty"`
	StatsInterval    Duration `yaml:"stats_interval,omitempty" json:"stats_interval,omitempty"`
	// CompressionType: "none" | "gzip" | "snappy" | "lz4" | "zstd"(默认 none)
	// 用 string 比 int 更人类可读,内部用 ParseCompression 转换。
	CompressionType string `yaml:"compression_type,omitempty" json:"compression_type,omitempty"`
	Idempotent      bool   `yaml:"idempotent,omitempty" json:"idempotent,omitempty"`               // 默认 true
	MaxOpenRequests int    `yaml:"max_open_requests,omitempty" json:"max_open_requests,omitempty"` // idempotent=true 时必须为 1
	RetentionMs     int64  `yaml:"retention_ms,omitempty" json:"retention_ms,omitempty"`           // 默认 7 天
}

// ParseCompression 把 yaml 里的字符串转成 sarama 类型。
// 不识别的值返回 sarama.CompressionNone(不报错,日志由调用方打)。
func (k KafkaConfig) ParseCompression() sarama.CompressionCodec {
	switch k.CompressionType {
	case "gzip":
		return sarama.CompressionGZIP
	case "snappy":
		return sarama.CompressionSnappy
	case "lz4":
		return sarama.CompressionLZ4
	case "zstd":
		return sarama.CompressionZSTD
	case "", "none":
		return sarama.CompressionNone
	default:
		return sarama.CompressionNone
	}
}

// SnowflakeConf 雪花算法参数。
type SnowflakeConf struct {
	Epoch    int64  `yaml:"epoch,omitempty" json:"epoch,omitempty"`         // 默认 1773446400 (2026-03-14 UTC)
	NodeBits uint32 `yaml:"node_bits,omitempty" json:"node_bits,omitempty"` // 默认 17
	StepBits uint32 `yaml:"step_bits,omitempty" json:"step_bits,omitempty"` // 默认 15

	// NodeIDSource 决定 snowflake nodeID 来源:
	//   - ""/"static":用 node.zone_id 静态分配(单副本 / dev 默认)。
	//   - "etcd":进入 k8s 多副本动态扩缩阶段,用 etcd Lease 自动抢占 nodeID。
	//     需服务在 main 里 import pkg/snowflake/etcdnode,并据 Holder.Lost() 失租退出。
	//     详见 docs/design/infra.md §8.1 与 docs/design/scale-dau-2m.md。
	NodeIDSource string `yaml:"node_id_source,omitempty" json:"node_id_source,omitempty"`

	// Etcd* 给 NodeIDSource="etcd" 用。
	EtcdEndpoints   []string `yaml:"etcd_endpoints,omitempty" json:"etcd_endpoints,omitempty"`
	EtcdPrefix      string   `yaml:"etcd_prefix,omitempty" json:"etcd_prefix,omitempty"`               // 默认 "/pandora/snowflake/node/"
	EtcdServiceName string   `yaml:"etcd_service_name,omitempty" json:"etcd_service_name,omitempty"`   // 留空用服务名;隔离各服务 nodeID 空间
	EtcdLeaseTTLSec int64    `yaml:"etcd_lease_ttl_sec,omitempty" json:"etcd_lease_ttl_sec,omitempty"` // 默认 15
}

// LockerConf 分布式锁默认 TTL。
type LockerConf struct {
	AccountLockTTL uint32 `yaml:"account_lock_ttl,omitempty" json:"account_lock_ttl,omitempty"` // 秒,默认 10
	PlayerLockTTL  uint32 `yaml:"player_lock_ttl,omitempty" json:"player_lock_ttl,omitempty"`
}

// RegistryConf 服务注册发现。
type RegistryConf struct {
	Etcd EtcdRegistryConf `yaml:"etcd,omitempty" json:"etcd,omitempty"`
}

// KillSwitchConf RPC 级临时关停(Kill-Switch)配置。
//
// 出现重大问题想临时关某个 service / RPC、修好再开,不发版不重启、秒级热生效。
// 由 pkg/svc.BaseContext 在装配时翻译成 killswitch.Config 并启动开关源,
// pkg/middleware.KillSwitch() 在 gRPC server 链上拦截命中规则的 RPC。
type KillSwitchConf struct {
	// Enabled 为 false 时不启用 Kill-Switch(全放行)。
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`

	// Source 开关源:"file"(dev 默认,改 yaml 即生效)/ "etcd"(prod,集中多实例一致)。
	// etcd 源需服务在 main 里 blank import pkg/killswitch/etcdkv。
	Source string `yaml:"source,omitempty" json:"source,omitempty"`

	// FilePath file 源监听的 yaml(默认 "etc/killswitch.yaml")。
	FilePath string `yaml:"file_path,omitempty" json:"file_path,omitempty"`

	// Etcd* 给 etcd 源用。
	EtcdEndpoints   []string `yaml:"etcd_endpoints,omitempty" json:"etcd_endpoints,omitempty"`
	EtcdPrefix      string   `yaml:"etcd_prefix,omitempty" json:"etcd_prefix,omitempty"` // 默认 "/pandora/killswitch/"
	EtcdDialTimeout Duration `yaml:"etcd_dial_timeout,omitempty" json:"etcd_dial_timeout,omitempty"`

	// FailClosed 控制源构造失败时的行为。
	// 零值 false = fail-open(放行,Kill-Switch 自身故障绝不拖垮服务,推荐默认)。
	// true = fail-closed(源建不起来则 main fatal,仅在你要求"开关系统必须在线"时用)。
	FailClosed bool `yaml:"fail_closed,omitempty" json:"fail_closed,omitempty"`
}

// EtcdRegistryConf etcd 注册中心(W3+ 接入)。
type EtcdRegistryConf struct {
	Hosts       []string `yaml:"hosts" json:"hosts"`
	Key         string   `yaml:"key,omitempty" json:"key,omitempty"`                   // service path,默认按服务名构造
	DialTimeout Duration `yaml:"dial_timeout,omitempty" json:"dial_timeout,omitempty"` // 默认 5s
}

// TimeoutConf 各种公共超时。
type TimeoutConf struct {
	EtcdDialTimeout         Duration `yaml:"etcd_dial_timeout,omitempty" json:"etcd_dial_timeout,omitempty"`
	ServiceDiscoveryTimeout Duration `yaml:"service_discovery_timeout,omitempty" json:"service_discovery_timeout,omitempty"`
	TaskWaitTimeout         Duration `yaml:"task_wait_timeout,omitempty" json:"task_wait_timeout,omitempty"`
	RoleCacheExpire         Duration `yaml:"role_cache_expire,omitempty" json:"role_cache_expire,omitempty"`
}

// BuildTopic 按 docs/design/infra.md §4 规范构造 kafka topic。
//
//	BuildTopic("battle", "result")          → "pandora.battle.result"
//	BuildTopic("login",  "event")           → "pandora.login.event"
func BuildTopic(domain, event string) string {
	return fmt.Sprintf("pandora.%s.%s", domain, event)
}

// BuildDLQTopic 构造死信队列 topic(infra.md §4.4)。
//
//	BuildDLQTopic("pandora.battle.result") → "pandora.dlq.battle.result"
func BuildDLQTopic(originalTopic string) string {
	const prefix = "pandora."
	if len(originalTopic) > len(prefix) && originalTopic[:len(prefix)] == prefix {
		return "pandora.dlq." + originalTopic[len(prefix):]
	}
	return "pandora.dlq." + originalTopic
}
