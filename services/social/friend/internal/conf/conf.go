// Package conf 是 friend 服务的私有配置结构(2026-06-15)。
package conf

import (
	"github.com/luyuancpp/pandora/pkg/config"
)

// Config 是 friend 服务的完整配置。
type Config struct {
	config.Base `yaml:",inline" mapstructure:",squash"`

	Friend FriendConf `yaml:"friend" json:"friend"`
}

// FriendConf 是 friend 服务私有配置。
type FriendConf struct {
	// MaxFriends 单玩家好友数量上限(默认 200)。
	// AddFriend 时对 requester 提前失败;权威校验在 AcceptFriend 事务内对双方原子执行。
	MaxFriends int `yaml:"max_friends,omitempty" json:"max_friends,omitempty"`

	// LocatorAddr player_locator gRPC 地址(host:port)。
	// 空 → ListFriends 不查在线状态(is_online 全 false,弱依赖)。
	LocatorAddr string `yaml:"locator_addr,omitempty" json:"locator_addr,omitempty"`

	// RecommendLimit 单次推荐好友数量(默认 10,硬上限 20,超界收敛到 20)。
	RecommendLimit int `yaml:"recommend_limit,omitempty" json:"recommend_limit,omitempty"`

	// RecommendStrategies 推荐策略链(按序召回直到凑够 limit)。空 → ["mutual","random"]。
	// 已支持:mutual(熟人,共同好友数)/ random(随机兜底)。
	// 未来扩展:similar_power(实力相当)/ same_region(同区域),待 player 服务接画像后接。
	RecommendStrategies []string `yaml:"recommend_strategies,omitempty" json:"recommend_strategies,omitempty"`
}

// Defaults 填默认值,防止 yaml 缺字段时零值引发非预期行为。
func (c *Config) Defaults() {
	if c.Friend.MaxFriends <= 0 {
		c.Friend.MaxFriends = 200
	}
	if c.Friend.RecommendLimit <= 0 {
		c.Friend.RecommendLimit = 10
	}
	if c.Friend.RecommendLimit > 20 {
		c.Friend.RecommendLimit = 20
	}
	if c.Server.Grpc.Addr == "" {
		c.Server.Grpc.Addr = ":50004"
	}
	if c.Server.Http.Addr == "" {
		c.Server.Http.Addr = ":51004"
	}
}
