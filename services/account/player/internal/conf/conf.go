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
