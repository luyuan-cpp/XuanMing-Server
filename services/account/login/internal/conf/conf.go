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
	config.Base `yaml:",inline" mapstructure:",squash"`

	// Login 业务字段。
	Login LoginConf `yaml:"login" json:"login"`
}

// LoginConf 是 login 服务私有配置。
type LoginConf struct {
	// SessionTokenTTL session_token 的有效期(写到 redis,W2 mock 暂不用;W3 ① 用作 JWT exp)。
	SessionTokenTTL config.Duration `yaml:"session_token_ttl,omitempty" json:"session_token_ttl,omitempty"`

	// DSTicketTTL DS 票据有效期(JWT exp - issued_at)。
	// 不变量 §3:DS 票据短时效。默认 5 分钟。
	DSTicketTTL config.Duration `yaml:"ds_ticket_ttl,omitempty" json:"ds_ticket_ttl,omitempty"`

	// MockHubDSAddr W2 mock 阶段直接返给客户端的 hub DS 地址。
	// W3 改成调 hub_allocator.Assign 拿真实地址。
	MockHubDSAddr string `yaml:"mock_hub_ds_addr,omitempty" json:"mock_hub_ds_addr,omitempty"`

	// MockAccount / MockPasswordHash W2 mock 允许通过的固定账号(便于联调)。
	MockAccount      string `yaml:"mock_account,omitempty" json:"mock_account,omitempty"`
	MockPasswordHash string `yaml:"mock_password_hash,omitempty" json:"mock_password_hash,omitempty"`

	// JWT 设置(W3 ①,2026-06-05)。
	// dev/prod 都走 HS256,secret 要跟 deploy/envoy/envoy.yaml 的 jwt_authn provider 保持一致。
	JWT JWTConf `yaml:"jwt,omitempty" json:"jwt,omitempty"`

	// Locator W3 ⑤ 联动:登录成功后调 PlayerLocatorService.SetLocation(state=LOGIN_PENDING)。
	// addr 为空 → 不调(便于本机不起 locator 也能跑通 login)。
	Locator LocatorClientConf `yaml:"locator,omitempty" json:"locator,omitempty"`
}

// LocatorClientConf 是 login 调 player_locator 的客户端参数。
type LocatorClientConf struct {
	// Addr player_locator gRPC 端口(默认 127.0.0.1:50006)。
	// 留空 → 不调 locator,Login 走 fallback(仅 Warn 日志)。
	Addr string `yaml:"addr,omitempty" json:"addr,omitempty"`
}

// JWTConf 是 login 签发 SessionToken / DSTicket 的 JWT 参数。
//
// 与 Envoy jwt_authn 的 provider 配套:
//   - Issuer / Audience 必须跟 envoy.yaml 一致(否则 Envoy 会拒)
//   - Secret base64某种 / 明文 都可以,但 envoy.yaml 里是 base64url(secret) 填进 JWKS 的 k 字段
//   - SessionTTL 默认 24h;DSTicketTTL 默认 5min(不变量 §3)
type JWTConf struct {
	Issuer      string          `yaml:"issuer,omitempty" json:"issuer,omitempty"`
	Audience    string          `yaml:"audience,omitempty" json:"audience,omitempty"`
	Secret      string          `yaml:"secret,omitempty" json:"secret,omitempty"`
	SessionTTL  config.Duration `yaml:"session_ttl,omitempty" json:"session_ttl,omitempty"`
	DSTicketTTL config.Duration `yaml:"ds_ticket_ttl,omitempty" json:"ds_ticket_ttl,omitempty"`
}

// Defaults 把零值填成 Pandora 标准默认值(W2 mock 阶段用)。
func (c *Config) Defaults() {
	if c.Login.SessionTokenTTL == 0 {
		c.Login.SessionTokenTTL = config.Duration(24 * time.Hour)
	}
	if c.Login.DSTicketTTL == 0 {
		c.Login.DSTicketTTL = config.Duration(5 * time.Minute)
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
	// JWT(W3 ① 默认)
	if c.Login.JWT.Issuer == "" {
		c.Login.JWT.Issuer = "pandora-login"
	}
	if c.Login.JWT.Audience == "" {
		c.Login.JWT.Audience = "pandora-client"
	}
	if c.Login.JWT.Secret == "" {
		// ❗ dev 默认 secret,不要上生产。envoy.yaml 里同步这个值的 base64url。
		c.Login.JWT.Secret = "pandora-dev-jwt-secret-change-me-32!"
	}
	if c.Login.JWT.SessionTTL == 0 {
		c.Login.JWT.SessionTTL = c.Login.SessionTokenTTL // 默认跟 SessionTokenTTL一致
	}
	if c.Login.JWT.DSTicketTTL == 0 {
		c.Login.JWT.DSTicketTTL = c.Login.DSTicketTTL
	}
	if c.Server.Grpc.Addr == "" {
		c.Server.Grpc.Addr = ":50001"
	}
	if c.Server.Http.Addr == "" {
		c.Server.Http.Addr = ":51001"
	}
}
