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
	if c.Server.Grpc.Addr == "" {
		c.Server.Grpc.Addr = ":50021"
	}
	if c.Server.Http.Addr == "" {
		c.Server.Http.Addr = ":51021"
	}
}
