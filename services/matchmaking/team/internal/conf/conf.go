// Package conf 是 team 服务的私有配置结构。
package conf

import (
	"time"

	"github.com/luyuancpp/pandora/pkg/config"
)

// Config 是 team 服务的完整配置。
type Config struct {
	config.Base `yaml:",inline" mapstructure:",squash"`

	Team TeamConf `yaml:"team" json:"team"`
}

// TeamConf 是 team 服务私有配置。
type TeamConf struct {
	// InviteTTL 邀请令牌 Redis key 的 TTL,客户端须在此时间内 AcceptInvite。
	InviteTTL config.Duration `yaml:"invite_ttl,omitempty" json:"invite_ttl,omitempty"`

	// DisbandedRetention 队伍解散后 Redis key 的保留时长,供客户端查询最终状态。
	DisbandedRetention config.Duration `yaml:"disbanded_retention,omitempty" json:"disbanded_retention,omitempty"`

	// ActiveTTL 活跃队伍(未解散)Redis key 的生命周期。
	// 队伍在此时间内无任何写操作则整体过期消失,防止僵尸队伍长期占用 Redis。
	ActiveTTL config.Duration `yaml:"active_ttl,omitempty" json:"active_ttl,omitempty"`

	// MaxMembers MOBA 5v5,一队最多允许多少成员。
	MaxMembers int `yaml:"max_members,omitempty" json:"max_members,omitempty"`

	// OptimisticRetry WATCH/MULTI/EXEC 乐观锁冲突时最大重试次数。
	// 耗尽后返回 ErrTeamConcurrent(3007)。
	OptimisticRetry int `yaml:"optimistic_retry,omitempty" json:"optimistic_retry,omitempty"`

	// MatchmakerAddr matchmaker 服务 gRPC 直连地址(host:port,内网 insecure)。
	// 成员离队/被踢时联动撤销其所在的匹配票据(弱依赖):留空 → 不联动,
	// 行为与历史一致(本机不起 matchmaker 的骨架联调路径)。
	MatchmakerAddr string `yaml:"matchmaker_addr,omitempty" json:"matchmaker_addr,omitempty"`
}

// Defaults 填默认值,防止 yaml 缺字段时零值引发 panic。
func (c *Config) Defaults() {
	if c.Team.InviteTTL == 0 {
		c.Team.InviteTTL = config.Duration(60 * time.Second)
	}
	if c.Team.DisbandedRetention == 0 {
		c.Team.DisbandedRetention = config.Duration(5 * time.Minute)
	}
	if c.Team.ActiveTTL == 0 {
		c.Team.ActiveTTL = config.Duration(60 * time.Minute)
	}
	if c.Team.MaxMembers == 0 {
		c.Team.MaxMembers = 5
	}
	if c.Team.OptimisticRetry == 0 {
		c.Team.OptimisticRetry = 3
	}
	if c.Server.Grpc.Addr == "" {
		c.Server.Grpc.Addr = ":50010"
	}
	if c.Server.Http.Addr == "" {
		c.Server.Http.Addr = ":51010"
	}
}
