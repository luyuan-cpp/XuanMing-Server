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

	// ── 战斗装备掉落回写 W5 ④ ──

	// InventoryAddr inventory 服务 gRPC 地址(弱依赖:空 → 关闭掉落回写,不发放战斗装备掉落)。
	// 内网 insecure 直连(系统接口,无 JWT)。RunDropPublisher 用它调 GrantInstances。
	InventoryAddr string `yaml:"inventory_addr,omitempty" json:"inventory_addr,omitempty"`

	// DropWhitelist 允许作为战斗掉落落库的装备 item_config_id 白名单(DS 不可信,§12)。
	// 空 = 不放行任何掉落(安全默认:DS 上报的 dropped_item_config_ids 全被过滤掉,不发放)。
	// battle_result 写 drop 出箱前按此过滤,DS 只能触发白名单内装备落库。
	DropWhitelist []uint32 `yaml:"drop_whitelist,omitempty" json:"drop_whitelist,omitempty"`

	// DropPublishInterval 战斗掉落出箱发布轮询间隔(默认 2s)。
	DropPublishInterval config.Duration `yaml:"drop_publish_interval,omitempty" json:"drop_publish_interval,omitempty"`

	// DropBatchSize 每轮发布取多少条掉落出箱记录(默认 128)。
	DropBatchSize int `yaml:"drop_batch_size,omitempty" json:"drop_batch_size,omitempty"`

	// MailAddr mail 服务 gRPC 地址(弱依赖:空 → 背包满掉落留在出箱轮询重试,不转邮件)。
	// 内网 insecure 直连(系统接口,无 JWT)。发放遇 ErrInventoryCapacityFull(背包满)时,
	// RunDropPublisher 用它调 SendPersonalMail 把溢出装备转个人邮件(幂等键防重发),再删出箱行。
	MailAddr string `yaml:"mail_addr,omitempty" json:"mail_addr,omitempty"`
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
	if c.Battle.DropPublishInterval.Std() <= 0 {
		c.Battle.DropPublishInterval = config.Duration(2 * time.Second)
	}
	if c.Battle.DropBatchSize <= 0 {
		c.Battle.DropBatchSize = 128
	}
	if c.Server.Grpc.Addr == "" {
		c.Server.Grpc.Addr = ":50022"
	}
	if c.Server.Http.Addr == "" {
		c.Server.Http.Addr = ":51022"
	}
}

// IsDroppable 判断某 item_config_id 是否在战斗掉落白名单内(DS 不可信过滤,W5 ④)。
// 白名单为空 → 恒 false(安全默认:不放行任何掉落)。
func (b *BattleConf) IsDroppable(itemConfigID uint32) bool {
	for _, id := range b.DropWhitelist {
		if id == itemConfigID {
			return true
		}
	}
	return false
}
