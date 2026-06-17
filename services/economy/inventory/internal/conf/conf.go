// Package conf 是 inventory 服务的私有配置结构(W5 ③,2026-06-18)。
package conf

import (
	"fmt"

	"github.com/luyuancpp/pandora/pkg/config"
)

// Config 是 inventory 服务的完整配置。
type Config struct {
	config.Base `yaml:",inline" mapstructure:",squash"`

	Inventory InventoryConf `yaml:"inventory" json:"inventory"`
}

// ItemRule 是某配置道具的大厅经济规则(usable / sellable + 出售单价)。
//
// 说明:正式项目里这些应来自配置表服务 / 静态表;W5 ③ 先用服务配置承载,
// 避免引入完整配置表依赖。战斗内即时道具不在此表(走 GAS,ds-arch §0.1)。
type ItemRule struct {
	// ItemConfigID 配置表道具 ID(uint32,§12)。
	ItemConfigID uint32 `yaml:"item_config_id" json:"item_config_id"`
	// Usable 是否可在大厅使用(开箱 / 经验书 / 消耗品)。
	Usable bool `yaml:"usable,omitempty" json:"usable,omitempty"`
	// Sellable 是否可出售。
	Sellable bool `yaml:"sellable,omitempty" json:"sellable,omitempty"`
	// SellUnitPrice 单个出售得到的金币(Sellable=true 时生效,>=0)。
	SellUnitPrice int64 `yaml:"sell_unit_price,omitempty" json:"sell_unit_price,omitempty"`
}

// InventoryConf 是 inventory 服务私有配置。
type InventoryConf struct {
	// ItemRules 道具大厅经济规则表(按 item_config_id 索引)。
	// 留空 = 任何道具都不可大厅使用 / 出售(只能 Grant + Get,安全默认)。
	ItemRules []ItemRule `yaml:"item_rules,omitempty" json:"item_rules,omitempty"`
}

// Defaults 填默认值。
func (c *Config) Defaults() {
	if c.Server.Grpc.Addr == "" {
		c.Server.Grpc.Addr = ":50015"
	}
	if c.Server.Http.Addr == "" {
		c.Server.Http.Addr = ":51015"
	}
}

// RuleOf 返回某道具的规则(不存在 → nil)。
func (ic *InventoryConf) RuleOf(itemConfigID uint32) *ItemRule {
	for i := range ic.ItemRules {
		if ic.ItemRules[i].ItemConfigID == itemConfigID {
			return &ic.ItemRules[i]
		}
	}
	return nil
}

// Validate 校验道具规则表(启动时调,非法配置直接 fail-fast,避免上线后负价/重复规则扣币)。
//   - item_config_id 必须非 0 且不重复
//   - 可出售(Sellable=true)必须 sell_unit_price > 0;不可出售时单价必须为 0
func (ic *InventoryConf) Validate() error {
	seen := make(map[uint32]struct{}, len(ic.ItemRules))
	for i := range ic.ItemRules {
		r := &ic.ItemRules[i]
		if r.ItemConfigID == 0 {
			return fmt.Errorf("item_rules[%d]: item_config_id must not be 0", i)
		}
		if _, dup := seen[r.ItemConfigID]; dup {
			return fmt.Errorf("item_rules[%d]: duplicate item_config_id %d", i, r.ItemConfigID)
		}
		seen[r.ItemConfigID] = struct{}{}
		if r.Sellable {
			if r.SellUnitPrice <= 0 {
				return fmt.Errorf("item_rules[%d]: sellable item %d must have sell_unit_price > 0 (got %d)", i, r.ItemConfigID, r.SellUnitPrice)
			}
		} else if r.SellUnitPrice != 0 {
			return fmt.Errorf("item_rules[%d]: non-sellable item %d must have sell_unit_price == 0 (got %d)", i, r.ItemConfigID, r.SellUnitPrice)
		}
	}
	return nil
}
