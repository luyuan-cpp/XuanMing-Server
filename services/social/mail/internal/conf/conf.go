// Package conf 是 mail 服务的私有配置结构(2026-06-29)。
package conf

import (
	"time"

	"github.com/luyuancpp/pandora/pkg/config"
)

// Config 是 mail 服务的完整配置。
type Config struct {
	config.Base `yaml:",inline" mapstructure:",squash"`

	Mail MailConf `yaml:"mail" json:"mail"`
}

// MailConf 是 mail 服务私有配置。
type MailConf struct {
	// DefaultSysTtlDays 系统/公会邮件默认有效期天数(end_ms 为 0 时补,默认 7)。
	DefaultSysTtlDays int `yaml:"default_sys_ttl_days,omitempty" json:"default_sys_ttl_days,omitempty"`

	// DefaultPersonalTtlDays 个人邮件默认有效期天数(expire_ms 为 0 时补,默认 30)。
	// 一切邮件生命有限是 sweep 清理的前提:没有默认 TTL,库只增不减。
	DefaultPersonalTtlDays int `yaml:"default_personal_ttl_days,omitempty" json:"default_personal_ttl_days,omitempty"`

	// MaxInboxSize 单玩家收件箱行数上限(默认 200,§9 不变量 18 写入侧上限)。
	// 满时先驱逐最旧的已领(status=claimed)邮件,仍满返回 ERR_MAIL_BOX_FULL;
	// 调用方(battle_result 掉落出箱)靠补扫重试,待旧邮件过期被 sweep 清掉后自然成功。
	MaxInboxSize int `yaml:"max_inbox_size,omitempty" json:"max_inbox_size,omitempty"`

	// SweepInterval 过期清理轮询间隔(默认 5m)。多副本各自跑,删除幂等无需锁
	// (对齐 leaderboard 发奖补扫模式)。
	SweepInterval config.Duration `yaml:"sweep_interval,omitempty" json:"sweep_interval,omitempty"`

	// SweepBatch 每轮每表清理行数上限(默认 500,小批量防长事务锁表)。
	SweepBatch int `yaml:"sweep_batch,omitempty" json:"sweep_batch,omitempty"`

	// ExpiredRetentionDays 过期后延迟清理的缓冲天数(默认 7):过期邮件先只是不可见,
	// 缓冲期后才物理删除/归档,留客诉排查窗口,也吸收时钟偏差。
	ExpiredRetentionDays int `yaml:"expired_retention_days,omitempty" json:"expired_retention_days,omitempty"`

	// ArchiveRetentionDays 归档表保留天数(默认 90,超期物理清除,归档表自身有界)。
	ArchiveRetentionDays int `yaml:"archive_retention_days,omitempty" json:"archive_retention_days,omitempty"`

	// ClaimRetentionDays 领取记录保留天数(默认 180)。发送侧把一切邮件的 end_ms 钳到
	// 「创建时刻 + 本值」以内(biz.defaultEnd),保证 claim 行存活 ≥ 邮件可领窗口:
	// 重复领取永远先被 claim 行挡住,不依赖 inventory 幂等流水兜底(其自身只保留 90 天,
	// CLAUDE.md §9.24)。本值是 §9.24「失效数据最多 90 天」的登记例外:须覆盖邮件最长寿命。
	ClaimRetentionDays int `yaml:"claim_retention_days,omitempty" json:"claim_retention_days,omitempty"`

	// MaxTitleLen 邮件标题最大长度(utf8 rune,默认 64)。
	MaxTitleLen int `yaml:"max_title_len,omitempty" json:"max_title_len,omitempty"`

	// MaxBodyLen 邮件正文最大长度(utf8 rune,默认 2048)。
	MaxBodyLen int `yaml:"max_body_len,omitempty" json:"max_body_len,omitempty"`

	// MaxAttachments 单封邮件附件上限(默认 16)。
	MaxAttachments int `yaml:"max_attachments,omitempty" json:"max_attachments,omitempty"`

	// InventoryAddr inventory 服务 gRPC 地址(host:port),领取附件入库用。
	InventoryAddr string `yaml:"inventory_addr,omitempty" json:"inventory_addr,omitempty"`

	// AllowNoopGrant inventory 不可用时允许空领(只标记 claim,不真发),仅测试环境。
	AllowNoopGrant bool `yaml:"allow_noop_grant,omitempty" json:"allow_noop_grant,omitempty"`
}

// Defaults 填默认值,防止 yaml 缺字段时零值引发非预期行为。
func (c *Config) Defaults() {
	if c.Mail.DefaultSysTtlDays <= 0 {
		c.Mail.DefaultSysTtlDays = 7
	}
	if c.Mail.DefaultPersonalTtlDays <= 0 {
		c.Mail.DefaultPersonalTtlDays = 30
	}
	if c.Mail.MaxInboxSize <= 0 {
		c.Mail.MaxInboxSize = 200
	}
	if c.Mail.SweepInterval <= 0 {
		c.Mail.SweepInterval = config.Duration(5 * time.Minute)
	}
	if c.Mail.SweepBatch <= 0 {
		c.Mail.SweepBatch = 500
	}
	if c.Mail.ExpiredRetentionDays <= 0 {
		c.Mail.ExpiredRetentionDays = 7
	}
	if c.Mail.ArchiveRetentionDays <= 0 {
		c.Mail.ArchiveRetentionDays = 90
	}
	if c.Mail.ClaimRetentionDays <= 0 {
		c.Mail.ClaimRetentionDays = 180
	}
	if c.Mail.MaxTitleLen <= 0 {
		c.Mail.MaxTitleLen = 64
	}
	if c.Mail.MaxBodyLen <= 0 {
		c.Mail.MaxBodyLen = 2048
	}
	if c.Mail.MaxAttachments <= 0 {
		c.Mail.MaxAttachments = 16
	}
	if c.Server.Grpc.Addr == "" {
		c.Server.Grpc.Addr = ":50009"
	}
	if c.Server.Http.Addr == "" {
		c.Server.Http.Addr = ":51009"
	}
}
