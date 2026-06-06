// Package conf 是 ds_allocator 服务的私有配置结构。
package conf

import (
	"time"

	"github.com/luyuancpp/pandora/pkg/config"
)

// Config 是 ds_allocator 服务的完整配置。
type Config struct {
	config.Base `yaml:",inline" mapstructure:",squash"`

	Allocator AllocatorConf `yaml:"allocator" json:"allocator"`
}

// AllocatorConf 是 ds_allocator 服务私有配置。
type AllocatorConf struct {
	// HeartbeatTimeout DS 心跳超时阈值(默认 15s,不变量 §4)。
	// 超过此时长没收到 Heartbeat → 标记 abandoned + 释放(W4 ② 仅释放,补偿留 W4 ③)。
	HeartbeatTimeout config.Duration `yaml:"heartbeat_timeout,omitempty" json:"heartbeat_timeout,omitempty"`

	// SweepInterval 心跳超时扫描间隔(默认 5s)。
	SweepInterval config.Duration `yaml:"sweep_interval,omitempty" json:"sweep_interval,omitempty"`

	// BattleTTL 战斗 DS 镜像 Redis key 的 TTL(默认 2h,防僵尸镜像)。
	BattleTTL config.Duration `yaml:"battle_ttl,omitempty" json:"battle_ttl,omitempty"`

	// MockDSAddrHost W4 ② MockGameServerAllocator 返回的假 DS host(默认 127.0.0.1)。
	// W4 ③ 接 Agones 后此字段废弃,addr 由 GameServerAllocation status 返回。
	MockDSAddrHost string `yaml:"mock_ds_addr_host,omitempty" json:"mock_ds_addr_host,omitempty"`

	// MockDSPortBase W4 ② MockGameServerAllocator 端口基址(默认 30000)。
	// 每场 match 端口 = MockDSPortBase + (match_id % MockDSPortRange)。
	MockDSPortBase int `yaml:"mock_ds_port_base,omitempty" json:"mock_ds_port_base,omitempty"`

	// MockDSPortRange Mock 端口取模范围(默认 1000)。
	MockDSPortRange int `yaml:"mock_ds_port_range,omitempty" json:"mock_ds_port_range,omitempty"`
}

// Defaults 填默认值。
func (c *Config) Defaults() {
	if c.Allocator.HeartbeatTimeout == 0 {
		c.Allocator.HeartbeatTimeout = config.Duration(15 * time.Second)
	}
	if c.Allocator.SweepInterval == 0 {
		c.Allocator.SweepInterval = config.Duration(5 * time.Second)
	}
	if c.Allocator.BattleTTL == 0 {
		c.Allocator.BattleTTL = config.Duration(2 * time.Hour)
	}
	if c.Allocator.MockDSAddrHost == "" {
		c.Allocator.MockDSAddrHost = "127.0.0.1"
	}
	if c.Allocator.MockDSPortBase == 0 {
		c.Allocator.MockDSPortBase = 30000
	}
	if c.Allocator.MockDSPortRange == 0 {
		c.Allocator.MockDSPortRange = 1000
	}
	if c.Server.Grpc.Addr == "" {
		c.Server.Grpc.Addr = ":50020"
	}
	if c.Server.Http.Addr == "" {
		c.Server.Http.Addr = ":51020"
	}
}
