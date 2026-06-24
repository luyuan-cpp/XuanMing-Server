// Package conf 是 auction 服务的私有配置结构(2026-06-19)。
package conf

import (
	"github.com/luyuancpp/pandora/pkg/config"
)

// Config 是 auction 服务的完整配置。
type Config struct {
	config.Base `yaml:",inline" mapstructure:",squash"`

	Auction AuctionConf `yaml:"auction" json:"auction"`
}

// AuctionConf 是 auction 服务私有配置。
type AuctionConf struct {
	// MaxQuantityPerOrder 单挂单 / 出价最大数量(默认 1_000_000)。防一次挂天量。
	MaxQuantityPerOrder int64 `yaml:"max_quantity_per_order,omitempty" json:"max_quantity_per_order,omitempty"`

	// MaxPrice 单价上限(默认 1_000_000_000)。防溢出 / 异常价。
	MaxPrice int64 `yaml:"max_price,omitempty" json:"max_price,omitempty"`

	// DefaultListLimit ListMarket 默认返回条数(默认 50)。
	DefaultListLimit int `yaml:"default_list_limit,omitempty" json:"default_list_limit,omitempty"`

	// MaxListLimit ListMarket 单次返回上限(默认 200)。
	MaxListLimit int `yaml:"max_list_limit,omitempty" json:"max_list_limit,omitempty"`

	// InventoryAddr 是 inventory 服务的内网 gRPC 地址(host:port,如 127.0.0.1:50015)。
	// 配了 → 成交走真实结算(SettleAuctionMatch:卖↔买资产原子对转 + match_id 幂等);
	// 留空 → 退回 NoopSettlementLedger(占位,总成功),仅供无交易联调 / 单测环境用。
	InventoryAddr string `yaml:"inventory_addr,omitempty" json:"inventory_addr,omitempty"`

	// AllowNoopSettlement 显式允许在 InventoryAddr 为空时退回 NoopSettlementLedger(占位,不真实扣转资产)。
	// 默认 false:InventoryAddr 缺失即 fail-fast,防止生产漏配 inventory 地址后仍静默以「成交不结算」启动。
	// 仅无交易联调 / 单测环境显式置 true。
	AllowNoopSettlement bool `yaml:"allow_noop_settlement,omitempty" json:"allow_noop_settlement,omitempty"`

	// OrderTTLSeconds 挂单存活时长(秒)。> 0 → 启用过期清扫:创建超过 TTL 仍未成交的挂单
	// 自动置 EXPIRED、移出订单簿并退还 escrow(限制#1 补偿)。<= 0 → 不过期(永久挂单)。
	OrderTTLSeconds int64 `yaml:"order_ttl_seconds,omitempty" json:"order_ttl_seconds,omitempty"`

	// ExpirySweepIntervalSeconds 过期清扫扫描间隔(秒,默认 60)。仅 OrderTTLSeconds > 0 时生效。
	ExpirySweepIntervalSeconds int64 `yaml:"expiry_sweep_interval_seconds,omitempty" json:"expiry_sweep_interval_seconds,omitempty"`

	// ExpirySweepBatch 单次清扫最多处理的过期订单数(默认 200)。防一次扫太多阻塞。
	ExpirySweepBatch int `yaml:"expiry_sweep_batch,omitempty" json:"expiry_sweep_batch,omitempty"`

	// CrossInstanceLock 启用跨实例 per-market 单写者 Redis 锁(限制#2 多实例一致性)。
	// 单实例部署留 false(仅进程内 striped lock);多实例部署置 true。需配 Redis(复用订单簿 Redis)。
	CrossInstanceLock bool `yaml:"cross_instance_lock,omitempty" json:"cross_instance_lock,omitempty"`

	// MarketLockTTLSeconds 跨实例 market 锁 TTL(秒,默认 30,上限 30,不变量 §10)。
	MarketLockTTLSeconds int64 `yaml:"market_lock_ttl_seconds,omitempty" json:"market_lock_ttl_seconds,omitempty"`

	// MarketLockMaxWaitMs 抢 market 锁最大等待(毫秒,默认 3000);超时返回 ERR_AUCTION_MARKET_BUSY。
	MarketLockMaxWaitMs int64 `yaml:"market_lock_max_wait_ms,omitempty" json:"market_lock_max_wait_ms,omitempty"`
}

// Defaults 填默认值,防止 yaml 缺字段时零值引发非预期行为。
func (c *Config) Defaults() {
	if c.Auction.MaxQuantityPerOrder <= 0 {
		c.Auction.MaxQuantityPerOrder = 1_000_000
	}
	if c.Auction.MaxPrice <= 0 {
		c.Auction.MaxPrice = 1_000_000_000
	}
	if c.Auction.DefaultListLimit <= 0 {
		c.Auction.DefaultListLimit = 50
	}
	if c.Auction.MaxListLimit <= 0 {
		c.Auction.MaxListLimit = 200
	}
	if c.Auction.ExpirySweepIntervalSeconds <= 0 {
		c.Auction.ExpirySweepIntervalSeconds = 60
	}
	if c.Auction.ExpirySweepBatch <= 0 {
		c.Auction.ExpirySweepBatch = 200
	}
	if c.Auction.MarketLockTTLSeconds <= 0 || c.Auction.MarketLockTTLSeconds > 30 {
		c.Auction.MarketLockTTLSeconds = 30 // 不变量 §10:Redis lock TTL ≤ 30s
	}
	if c.Auction.MarketLockMaxWaitMs <= 0 {
		c.Auction.MarketLockMaxWaitMs = 3000
	}
	if c.Server.Grpc.Addr == "" {
		c.Server.Grpc.Addr = ":50016"
	}
	if c.Server.Http.Addr == "" {
		c.Server.Http.Addr = ":51016"
	}
}
