// Package conf 是 hub_allocator 服务的私有配置结构。
package conf

import (
	"time"

	"github.com/luyuancpp/pandora/pkg/config"
)

// Config 是 hub_allocator 服务的完整配置。
type Config struct {
	config.Base `yaml:",inline" mapstructure:",squash"`

	Hub HubConf `yaml:"hub" json:"hub"`

	// JWT 用于给玩家签发 hub DSTicket(AssignHub / TransferHub 返回 hub_ticket)。
	// Issuer / Audience / Secret 必须与 login / Envoy jwt_authn provider 完全一致。
	JWT JWTConf `yaml:"jwt,omitempty" json:"jwt,omitempty"`

	// Agones 真 Hub DS Fleet 发现配置(W4 ⑬)。enabled=false(默认)→ MockHubFleetProvider。
	Agones AgonesConf `yaml:"agones" json:"agones"`
}

// AgonesConf 是真 Agones Hub DS Fleet 发现配置(W4 ⑬,镜像 ds_allocator.AgonesConf)。
//
// Enabled=false(默认)→ 用 MockHubFleetProvider;Enabled=true → 用
// AgonesHubFleetProvider(经 k8s apiserver REST 查 agones.dev/v1 GameServer 列表,
// 按 agones.dev/fleet=<FleetName> + pandora.dev/region=<region> 标签过滤)。
//
// 集群内运行时 token_path / ca_path / api_server / namespace 留空即用 in-cluster 默认;
// 集群外联调(本机进程 → minikube)可显式指定 api_server + token_path(或 kubectl proxy 不带 token)。
type AgonesConf struct {
	// Enabled 打开真 Agones 分片发现(默认 false → Mock)。
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`

	// APIServer k8s apiserver 地址(默认 https://kubernetes.default.svc,in-cluster)。
	APIServer string `yaml:"api_server,omitempty" json:"api_server,omitempty"`

	// Namespace GameServer 所在命名空间(默认 default)。
	Namespace string `yaml:"namespace,omitempty" json:"namespace,omitempty"`

	// FleetName 选择 Hub DS GameServer 的 Fleet 名(selector agones.dev/fleet=<FleetName>)。
	// Enabled=true 时必填,否则构造失败。
	FleetName string `yaml:"fleet_name,omitempty" json:"fleet_name,omitempty"`

	// TokenPath ServiceAccount bearer token 文件路径
	// (默认 /var/run/secrets/kubernetes.io/serviceaccount/token;留 "-" 显式禁用 token)。
	TokenPath string `yaml:"token_path,omitempty" json:"token_path,omitempty"`

	// CAPath apiserver CA 证书路径
	// (默认 /var/run/secrets/kubernetes.io/serviceaccount/ca.crt)。
	CAPath string `yaml:"ca_path,omitempty" json:"ca_path,omitempty"`

	// InsecureSkipTLSVerify 跳过 apiserver TLS 校验(仅 dev,生产禁用)。
	InsecureSkipTLSVerify bool `yaml:"insecure_skip_tls_verify,omitempty" json:"insecure_skip_tls_verify,omitempty"`

	// ListTimeout 单次 LIST GameServer REST 调用超时(默认 5s)。
	ListTimeout config.Duration `yaml:"list_timeout,omitempty" json:"list_timeout,omitempty"`
}

// JWTConf 是签发 hub DSTicket 的 JWT 参数(镜像 login.JWTConf / matchmaker.JWTConf)。
type JWTConf struct {
	Issuer      string          `yaml:"issuer,omitempty" json:"issuer,omitempty"`
	Audience    string          `yaml:"audience,omitempty" json:"audience,omitempty"`
	Secret      string          `yaml:"secret,omitempty" json:"secret,omitempty"`
	SessionTTL  config.Duration `yaml:"session_ttl,omitempty" json:"session_ttl,omitempty"`
	DSTicketTTL config.Duration `yaml:"ds_ticket_ttl,omitempty" json:"ds_ticket_ttl,omitempty"`
}

// HubConf 是 hub_allocator 服务私有配置。
type HubConf struct {
	// HeartbeatTimeout Hub DS 心跳超时阈值(默认 15s,不变量 §4)。
	// 超过此时长没收到 Heartbeat → 分片标记 draining 并移出可分配集。
	HeartbeatTimeout config.Duration `yaml:"heartbeat_timeout,omitempty" json:"heartbeat_timeout,omitempty"`

	// SweepInterval 心跳超时扫描间隔(默认 5s)。
	SweepInterval config.Duration `yaml:"sweep_interval,omitempty" json:"sweep_interval,omitempty"`

	// ShardTTL 分片镜像 Redis key TTL(默认 30min,每次 Assign/Heartbeat 刷新)。
	ShardTTL config.Duration `yaml:"shard_ttl,omitempty" json:"shard_ttl,omitempty"`

	// AssignmentTTL 玩家→分片归属 Redis key TTL(默认 30min,每次 Assign/Transfer 刷新)。
	AssignmentTTL config.Duration `yaml:"assignment_ttl,omitempty" json:"assignment_ttl,omitempty"`

	// DefaultRegion AssignHub 未指定 region 时的兜底分区(默认 "global")。
	DefaultRegion string `yaml:"default_region,omitempty" json:"default_region,omitempty"`

	// DefaultCapacity 单分片人数上限(默认 500,大厅 500 人/实例)。
	DefaultCapacity int32 `yaml:"default_capacity,omitempty" json:"default_capacity,omitempty"`

	// OptimisticRetry WATCH/MULTI/EXEC 乐观锁冲突最大重试次数,耗尽返 ErrHubNoAvailable。
	OptimisticRetry int `yaml:"optimistic_retry,omitempty" json:"optimistic_retry,omitempty"`

	// MockShardCount W4 ⑤ MockHubFleetProvider 每 region 种的假分片数(默认 3)。
	// 真 Agones Fleet 接入后此字段废弃,分片拓扑由 Fleet 查询返回。
	MockShardCount int `yaml:"mock_shard_count,omitempty" json:"mock_shard_count,omitempty"`

	// MockHubAddrHost W4 ⑤ Mock 分片返回的假 Hub DS host(默认 127.0.0.1)。
	MockHubAddrHost string `yaml:"mock_hub_addr_host,omitempty" json:"mock_hub_addr_host,omitempty"`

	// MockHubPortBase W4 ⑤ Mock 分片端口基址(默认 7777;分片 port = base + shard_id)。
	MockHubPortBase int `yaml:"mock_hub_port_base,omitempty" json:"mock_hub_port_base,omitempty"`

	// AutoScaleEnabled 是否开启 Hub Fleet 自动扩缩容(默认 false)。
	// 开启条件:建议配合 agones.enabled=true(真 Fleet Provider),否则仅记录日志不生效。
	AutoScaleEnabled bool `yaml:"autoscale_enabled,omitempty" json:"autoscale_enabled,omitempty"`

	// PlayersPerHub 自动扩容阈值:单 Hub 目标承载人数(默认 500)。
	// 例:总在线 501 → 期望副本 ceil(501/500)=2。
	PlayersPerHub int32 `yaml:"players_per_hub,omitempty" json:"players_per_hub,omitempty"`

	// MinReplicas 开服默认保底大厅副本数(默认 1)。
	MinReplicas int32 `yaml:"min_replicas,omitempty" json:"min_replicas,omitempty"`

	// MaxReplicas 大厅副本上限(默认 20)。
	MaxReplicas int32 `yaml:"max_replicas,omitempty" json:"max_replicas,omitempty"`

	// ConsolidationEnabled 是否开启强制整合(低负载时把人换到该去的分片,排空分片后缩容,默认 false)。
	// 依赖 autoscale_enabled=true + kafka.brokers 非空(推迁迁移通知);任一缺失只记日志不生效。
	ConsolidationEnabled bool `yaml:"consolidation_enabled,omitempty" json:"consolidation_enabled,omitempty"`

	// MigrateGraceSeconds 迁移优雅倒计时(秒,默认 30)。
	// 下发给客户端/Hub DS 的提示倒计时;也是排空分片可被缩容回收的最短等待(避免提前杀 pod)。
	MigrateGraceSeconds int32 `yaml:"migrate_grace_seconds,omitempty" json:"migrate_grace_seconds,omitempty"`

	// ConsolidationBatch 单次 reconcile 每个排空分片最多迁移的玩家数(默认 50,防撑死)。
	// 超过部分留给下一个 sweep 周期继续排。
	ConsolidationBatch int `yaml:"consolidation_batch,omitempty" json:"consolidation_batch,omitempty"`
}

// Defaults 填默认值。
func (c *Config) Defaults() {
	if c.Hub.HeartbeatTimeout == 0 {
		c.Hub.HeartbeatTimeout = config.Duration(15 * time.Second)
	}
	if c.Hub.SweepInterval == 0 {
		c.Hub.SweepInterval = config.Duration(5 * time.Second)
	}
	if c.Hub.ShardTTL == 0 {
		c.Hub.ShardTTL = config.Duration(30 * time.Minute)
	}
	if c.Hub.AssignmentTTL == 0 {
		c.Hub.AssignmentTTL = config.Duration(30 * time.Minute)
	}
	if c.Hub.DefaultRegion == "" {
		c.Hub.DefaultRegion = "global"
	}
	if c.Hub.DefaultCapacity == 0 {
		c.Hub.DefaultCapacity = 500
	}
	if c.Hub.OptimisticRetry == 0 {
		c.Hub.OptimisticRetry = 3
	}
	if c.Hub.MockShardCount == 0 {
		c.Hub.MockShardCount = 3
	}
	if c.Hub.MockHubAddrHost == "" {
		c.Hub.MockHubAddrHost = "127.0.0.1"
	}
	if c.Hub.MockHubPortBase == 0 {
		c.Hub.MockHubPortBase = 7777
	}
	if c.Hub.PlayersPerHub == 0 {
		c.Hub.PlayersPerHub = 500
	}
	if c.Hub.MinReplicas == 0 {
		c.Hub.MinReplicas = 1
	}
	if c.Hub.MaxReplicas == 0 {
		c.Hub.MaxReplicas = 20
	}
	if c.Hub.MaxReplicas < c.Hub.MinReplicas {
		c.Hub.MaxReplicas = c.Hub.MinReplicas
	}
	if c.Hub.MigrateGraceSeconds == 0 {
		c.Hub.MigrateGraceSeconds = 30
	}
	if c.Hub.ConsolidationBatch == 0 {
		c.Hub.ConsolidationBatch = 50
	}
	if c.Agones.APIServer == "" {
		c.Agones.APIServer = "https://kubernetes.default.svc"
	}
	if c.Agones.Namespace == "" {
		c.Agones.Namespace = "default"
	}
	if c.Agones.TokenPath == "" {
		c.Agones.TokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	}
	if c.Agones.CAPath == "" {
		c.Agones.CAPath = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	}
	if c.Agones.ListTimeout == 0 {
		c.Agones.ListTimeout = config.Duration(5 * time.Second)
	}
	if c.Server.Grpc.Addr == "" {
		c.Server.Grpc.Addr = ":50021"
	}
	if c.Server.Http.Addr == "" {
		c.Server.Http.Addr = ":51021"
	}
}
