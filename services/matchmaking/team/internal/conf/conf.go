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

	// InvitePushMode 邀请推送模式(金丝雀灰度用)。老客户端只认 TeamUpdateEvent(reason=INVITE_SENT),
	// 新客户端只认独立的 TeamInviteEvent(event_type=INVITE=1、已不再从 TeamUpdateEvent 读邀请)。
	// 两代客户端各认各的 payload,单一模式无法同时喂饱两代 → 灰度共存期必须"双发"。
	//   - "dual"(默认):两条都发。老客户端用 legacy 弹框、把独立事件当 TeamUpdateEvent 误解→
	//     InviteId/TeamId=0→护栏不过→仅多一次无害快照,不误弹;新客户端忽略 legacy(纯化后不读)、
	//     只在独立事件弹框。**各弹一次、不双弹**(客户端已纯化,故双发安全,金丝雀期首选)。
	//   - "dedicated":只发独立事件。全量铺完新客户端后切到此值,停发冗余 legacy。老客户端会漏弹。
	//   - "legacy":只发旧 TeamUpdateEvent。回退用;新客户端会漏弹邀请框。
	// 空串按 "dual" 处理(见 Defaults)。
	InvitePushMode string `yaml:"invite_push_mode,omitempty" json:"invite_push_mode,omitempty"`
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
	if c.Team.InvitePushMode == "" {
		// 金丝雀期默认双发:老/新客户端各认各的 payload,互不干扰,各弹一次。
		c.Team.InvitePushMode = "dual"
	}
	if c.Server.Grpc.Addr == "" {
		c.Server.Grpc.Addr = ":50010"
	}
	if c.Server.Http.Addr == "" {
		c.Server.Http.Addr = ":51010"
	}
}
