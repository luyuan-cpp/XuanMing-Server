// Package conf 是 battle_result 服务的私有配置结构(W4 ③,2026-06-06)。
package conf

import (
	"time"

	"github.com/luyuancpp/pandora/pkg/config"
	"github.com/luyuancpp/pandora/pkg/kafkax"
)

// Config 是 battle_result 服务的完整配置。
type Config struct {
	config.Base `yaml:",inline" mapstructure:",squash"`

	Battle BattleConf `yaml:"battle" json:"battle"`
}

// BattleConf 是 battle_result 服务私有配置。
type BattleConf struct {
	// EloKFactor Elo K 系数(默认 32)。胜负 MMR 变化幅度上限 ≈ K。
	EloKFactor int `yaml:"elo_k_factor,omitempty" json:"elo_k_factor,omitempty"`

	// BaseMMR 玩家缺省 MMR(W4 ③ player 服务未上线 → StaticMMRReader 全返此值,默认 1500)。
	BaseMMR int `yaml:"base_mmr,omitempty" json:"base_mmr,omitempty"`

	// ConsumeTopics 本服订阅的 kafka topic(默认 [battle.result, ds.lifecycle])。
	ConsumeTopics []string `yaml:"consume_topics,omitempty" json:"consume_topics,omitempty"`

	// PlayerAddr player 服务 gRPC 地址(弱依赖:空 → 用 BaseMMR 静态 reader)。
	// W4 ③ player 未上线,留空;player 上线后填地址接真实当前 MMR。
	PlayerAddr string `yaml:"player_addr,omitempty" json:"player_addr,omitempty"`

	// MatchmakerAddr matchmaker 服务 gRPC 地址(弱依赖:空 → 不通知 matchmaker 释放撮合状态)。
	// 用于结算/废弃落库后调 matchmaker.ReleaseMatch,释放残留 player→ticket claim + 票据 +
	// match 镜像,修复"结算返回 Hub 后玩家无法再次匹配(StartMatch 4002)"。
	MatchmakerAddr string `yaml:"matchmaker_addr,omitempty" json:"matchmaker_addr,omitempty"`

	// OutboxPublishInterval player.update 出箱发布轮询间隔(W4 ⑨,默认 2s)。
	OutboxPublishInterval config.Duration `yaml:"outbox_publish_interval,omitempty" json:"outbox_publish_interval,omitempty"`

	// OutboxBatchSize 每轮发布取多少条出箱记录(默认 128)。
	OutboxBatchSize int `yaml:"outbox_batch_size,omitempty" json:"outbox_batch_size,omitempty"`
}

// Defaults 填默认值。
func (c *Config) Defaults() {
	if c.Battle.EloKFactor <= 0 {
		c.Battle.EloKFactor = 32
	}
	if c.Battle.BaseMMR <= 0 {
		c.Battle.BaseMMR = 1500
	}
	if len(c.Battle.ConsumeTopics) == 0 {
		c.Battle.ConsumeTopics = []string{kafkax.TopicBattleResult, kafkax.TopicDSLifecycle}
	}
	if c.Battle.OutboxPublishInterval.Std() <= 0 {
		c.Battle.OutboxPublishInterval = config.Duration(2 * time.Second)
	}
	if c.Battle.OutboxBatchSize <= 0 {
		c.Battle.OutboxBatchSize = 128
	}
	if c.Server.Grpc.Addr == "" {
		c.Server.Grpc.Addr = ":50022"
	}
	if c.Server.Http.Addr == "" {
		c.Server.Http.Addr = ":51022"
	}
}
