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
	if c.Server.Grpc.Addr == "" {
		c.Server.Grpc.Addr = ":50016"
	}
	if c.Server.Http.Addr == "" {
		c.Server.Http.Addr = ":51016"
	}
}
