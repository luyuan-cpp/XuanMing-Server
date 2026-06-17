// Package conf 是 player 服务的私有配置结构(W4 ④,2026-06-06)。
package conf

import (
	"github.com/luyuancpp/pandora/pkg/config"
	"github.com/luyuancpp/pandora/pkg/kafkax"
)

// Config 是 player 服务的完整配置。
type Config struct {
	config.Base `yaml:",inline" mapstructure:",squash"`

	Player PlayerConf `yaml:"player" json:"player"`
}

// PlayerConf 是 player 服务私有配置。
type PlayerConf struct {
	// BaseMMR 新玩家缺省 MMR(EnsureProfile / GetMMR 未建档兜底,默认 1500,与 battle_result 对齐)。
	BaseMMR int `yaml:"base_mmr,omitempty" json:"base_mmr,omitempty"`

	// MMRFloor MMR 下限(UpdateMMR 后 clamp,默认 0)。
	MMRFloor int `yaml:"mmr_floor,omitempty" json:"mmr_floor,omitempty"`

	// DefaultNicknamePrefix 默认昵称前缀(EnsureProfile 建档时 nickname=prefix+player_id,保证 uk 唯一,默认 "Player_")。
	DefaultNicknamePrefix string `yaml:"default_nickname_prefix,omitempty" json:"default_nickname_prefix,omitempty"`

	// MaxNicknameLen 昵称最大长度(UpdateNickname 校验,默认 32)。
	MaxNicknameLen int `yaml:"max_nickname_len,omitempty" json:"max_nickname_len,omitempty"`

	// HeroSelectionEnabled 出战英雄选择功能开关(默认 false,demo 阶段跳过选英雄,
	// 与 login demo-skip 风格一致;关闭时 SelectHero 返回 ERR_PLAYER_FEATURE_DISABLED)。
	HeroSelectionEnabled bool `yaml:"hero_selection_enabled,omitempty" json:"hero_selection_enabled,omitempty"`

	// LoadoutCustomizeEnabled 出战装备预设 / 天赋树自定义功能开关(默认 false,demo 阶段跳过;
	// 关闭时 SetEquipment / SetTalents / ResetTalents 返回 ERR_PLAYER_FEATURE_DISABLED;
	// 授予类 GrantTalentPoints 由系统驱动不受此开关影响)。
	//
	// ⚠️ 安全(2026-06-17 审查):SetEquipment 目前**只校验槽位不重复 + item_config_id 非 0**,
	// 未校验玩家是否拥有该装备 / item 是否为装备 / 槽位是否匹配。因 GetLoadout 会把装备转成
	// Battle DS 初始 GameplayEffect,启用后等于客户端可给自己配任意装备。
	// **在接 inventory/配置表做拥有权 + 类型 + 槽位校验前,严禁对客户端开放**:
	// (1) 生产保持 false;(2) 不在 Envoy 暴露 player.v1.PlayerService 路由(当前未暴露)。
	LoadoutCustomizeEnabled bool `yaml:"loadout_customize_enabled,omitempty" json:"loadout_customize_enabled,omitempty"`

	// ConsumeTopics 本服订阅的 kafka topic(默认 [player.update])。
	ConsumeTopics []string `yaml:"consume_topics,omitempty" json:"consume_topics,omitempty"`
}

// Defaults 填默认值。
func (c *Config) Defaults() {
	if c.Player.BaseMMR <= 0 {
		c.Player.BaseMMR = 1500
	}
	if c.Player.MMRFloor < 0 {
		c.Player.MMRFloor = 0
	}
	if c.Player.DefaultNicknamePrefix == "" {
		c.Player.DefaultNicknamePrefix = "Player_"
	}
	if c.Player.MaxNicknameLen <= 0 {
		c.Player.MaxNicknameLen = 32
	}
	if len(c.Player.ConsumeTopics) == 0 {
		c.Player.ConsumeTopics = []string{kafkax.TopicPlayerUpdate}
	}
	if c.Server.Grpc.Addr == "" {
		c.Server.Grpc.Addr = ":50002"
	}
	if c.Server.Http.Addr == "" {
		c.Server.Http.Addr = ":51002"
	}
}
