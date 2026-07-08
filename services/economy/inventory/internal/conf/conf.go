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

	// Capacity 是装备实例背包格子容量(W5 ④)。<=0 = 未启用实例背包(GrantInstances 拒),
	// 安全默认。分配格 / 发放实例时按此上限校验(超出 → ErrInventoryCapacityFull)。
	Capacity int32 `yaml:"capacity,omitempty" json:"capacity,omitempty"`

	// IdentifyRules 装备鉴定随机属性规则表(按 item_config_id 索引,W5 ④)。
	// 留空 / 无匹配 = 鉴定只把 identified 置真、无随机属性(安全默认,不阻断)。
	IdentifyRules []IdentifyRule `yaml:"identify_rules,omitempty" json:"identify_rules,omitempty"`
}

// IdentifyAttrRoll 是鉴定属性池里的一条候选属性(值在 [Min,Max] 均匀 roll)。
type IdentifyAttrRoll struct {
	// AttrID 属性配置 ID(uint32,§12)。
	AttrID uint32 `yaml:"attr_id" json:"attr_id"`
	// Min / Max 该属性 roll 的闭区间(Min<=Max;均为该属性的数值)。
	Min int64 `yaml:"min" json:"min"`
	Max int64 `yaml:"max" json:"max"`
}

// IdentifyRule 是某配置装备鉴定时的随机属性规则(W5 ④)。
//
// 鉴定 = 从 Pool 里不放回抽 AttrCount 条属性,每条在 [Min,Max] 均匀 roll 数值。
// AttrCount>len(Pool) 时取 len(Pool)(全出);Pool 空 = 无属性(只置 identified)。
type IdentifyRule struct {
	// ItemConfigID 配置表道具 ID(uint32,§12)。
	ItemConfigID uint32 `yaml:"item_config_id" json:"item_config_id"`
	// AttrCount 鉴定产出的属性条数(>0)。
	AttrCount int `yaml:"attr_count" json:"attr_count"`
	// Pool 候选属性池。
	Pool []IdentifyAttrRoll `yaml:"pool" json:"pool"`
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

// IdentifyRuleOf 返回某装备的鉴定随机属性规则(不存在 → nil,鉴定退化为只置 identified 无属性)。
func (ic *InventoryConf) IdentifyRuleOf(itemConfigID uint32) *IdentifyRule {
	for i := range ic.IdentifyRules {
		if ic.IdentifyRules[i].ItemConfigID == itemConfigID {
			return &ic.IdentifyRules[i]
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
	// 校验鉴定规则表(W5 ④):item_config_id 非 0 不重复;attr_count>0;pool 每条 min<=max。
	seenID := make(map[uint32]struct{}, len(ic.IdentifyRules))
	for i := range ic.IdentifyRules {
		r := &ic.IdentifyRules[i]
		if r.ItemConfigID == 0 {
			return fmt.Errorf("identify_rules[%d]: item_config_id must not be 0", i)
		}
		if _, dup := seenID[r.ItemConfigID]; dup {
			return fmt.Errorf("identify_rules[%d]: duplicate item_config_id %d", i, r.ItemConfigID)
		}
		seenID[r.ItemConfigID] = struct{}{}
		if r.AttrCount <= 0 {
			return fmt.Errorf("identify_rules[%d]: attr_count must be > 0 (got %d)", i, r.AttrCount)
		}
		seenAttr := make(map[uint32]struct{}, len(r.Pool))
		for j := range r.Pool {
			p := &r.Pool[j]
			if p.AttrID == 0 {
				return fmt.Errorf("identify_rules[%d].pool[%d]: attr_id must not be 0", i, j)
			}
			if _, dup := seenAttr[p.AttrID]; dup {
				return fmt.Errorf("identify_rules[%d].pool[%d]: duplicate attr_id %d", i, j, p.AttrID)
			}
			seenAttr[p.AttrID] = struct{}{}
			if p.Min > p.Max {
				return fmt.Errorf("identify_rules[%d].pool[%d]: min %d must be <= max %d", i, j, p.Min, p.Max)
			}
		}
	}
	return nil
}
