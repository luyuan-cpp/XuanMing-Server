// Package conf 是 login 服务的私有配置结构。
//
// 内嵌 pkg/config.Base 拿公共字段,再加 login 自有字段。
//
// 加载方式(见 cmd/login/main.go):
//
//	c := kconfig.New(kconfig.WithSource(file.NewSource("./etc/login-dev.yaml")))
//	c.Load()
//	var cfg conf.Config
//	c.Scan(&cfg)
package conf

import (
	"time"

	"github.com/luyuancpp/pandora/pkg/config"
)

// Config 是 login 服务的完整配置。
type Config struct {
	// Base 公共字段(Server/Node/Snowflake/Locker/Registry/Timeouts/Kafka)。
	config.Base `yaml:",inline"`

	// Login 业务字段。
	Login LoginConf `yaml:"login"`
}

// LoginConf 是 login 服务私有配置。
type LoginConf struct {
	// SessionTokenTTL session_token 的有效期(写到 redis,W2 mock 暂不用)。
	SessionTokenTTL time.Duration `yaml:"session_token_ttl,omitempty"`

	// DSTicketTTL DS 票据有效期(JWT exp - issued_at)。
	// 不变量 §3:DS 票据短时效。默认 5 分钟。
	DSTicketTTL time.Duration `yaml:"ds_ticket_ttl,omitempty"`

	// MockHubDSAddr W2 mock 阶段直接返给客户端的 hub DS 地址。
	// W3 改成调 hub_allocator.Assign 拿真实地址。
	MockHubDSAddr string `yaml:"mock_hub_ds_addr,omitempty"`

	// MockAccount / MockPasswordHash W2 mock 允许通过的固定账号(便于联调)。
	MockAccount      string `yaml:"mock_account,omitempty"`
	MockPasswordHash string `yaml:"mock_password_hash,omitempty"`
}

// Defaults 把零值填成 Pandora 标准默认值(W2 mock 阶段用)。
func (c *Config) Defaults() {
	if c.Login.SessionTokenTTL == 0 {
		c.Login.SessionTokenTTL = 24 * time.Hour
	}
	if c.Login.DSTicketTTL == 0 {
		c.Login.DSTicketTTL = 5 * time.Minute
	}
	if c.Login.MockHubDSAddr == "" {
		c.Login.MockHubDSAddr = "127.0.0.1:7777"
	}
	if c.Login.MockAccount == "" {
		c.Login.MockAccount = "test"
	}
	if c.Login.MockPasswordHash == "" {
		c.Login.MockPasswordHash = "abc"
	}
	if c.Server.Grpc.Addr == "" {
		c.Server.Grpc.Addr = ":50001"
	}
	if c.Server.Http.Addr == "" {
		c.Server.Http.Addr = ":51001"
	}
}
