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
	// SessionTokenTTL session_token 的有效期(写到 Redis,也用作 JWT exp)。
	SessionTokenTTL config.Duration `yaml:"session_token_ttl,omitempty" json:"session_token_ttl,omitempty"`

	// DSTicketTTL DS 票据有效期(JWT exp - issued_at)。
	// 不变量 §3:DS 票据短时效。默认 5 分钟。
	DSTicketTTL config.Duration `yaml:"ds_ticket_ttl,omitempty" json:"ds_ticket_ttl,omitempty"`

	// MockHubDSAddr 是 hub_allocator 不可用时的本地回退 hub DS 地址。
	MockHubDSAddr string `yaml:"mock_hub_ds_addr,omitempty" json:"mock_hub_ds_addr,omitempty"`

	// DevSkipPassword 开发期免密登录开关(默认 false)。
	//
	// 为 true 时(仅供本机 / 联调,⚠️ 严禁上生产):
	//   - 跳过 bcrypt 密码校验,任意 password_hash 都放行
	//   - 账号不存在时自动懒注册一条 accounts 记录(snowflake 分配 player_id)
	//     → 同一 account 名每次登录拿到稳定 player_id(持久化在 MySQL,靠 uk_account 唯一)
	// 这样客户端随便填一个账号名即可进入,无需独立注册流程。
	DevSkipPassword bool `yaml:"dev_skip_password,omitempty" json:"dev_skip_password,omitempty"`

	// DevAutoRegister 开发期“假注册”开关(默认 false)。
	//
	// 为 true 时(仅供本机 / 联调,⚠️ 严禁上生产):账号不存在时首次登录
	// 自动注册一条 accounts 记录(snowflake 分配 player_id,存入本次客户端所发密码的 bcrypt 哈希)。
	//
	// 与 DevSkipPassword 正交:
	//   - 仅 DevAutoRegister:首登即注册,后续用同密码走正常 bcrypt 校验(真实“首登即注”语义)
	//   - 仅 DevSkipPassword:跳过密码校验(未知账号也会被懒注册,保持原行为)
	//   - 两者都开:任意账号名 + 任意密码都能进(最宽松 dev 模式)
	DevAutoRegister bool `yaml:"dev_auto_register,omitempty" json:"dev_auto_register,omitempty"`

	// JWT 设置(W3 ①,2026-06-05)。
	// dev/prod 都走 HS256,secret 要跟 deploy/envoy/envoy.yaml 的 jwt_authn provider 保持一致。
	JWT JWTConf `yaml:"jwt,omitempty" json:"jwt,omitempty"`

	// Locator W3 ⑤ 联动:登录成功后调 PlayerLocatorService.SetLocation(state=LOGIN_PENDING)。
	// addr 为空 → 不调(便于本机不起 locator 也能跑通 login)。
	Locator LocatorClientConf `yaml:"locator,omitempty" json:"locator,omitempty"`

	// Hub W4 ⑥ 联动:登录成功后调 HubAllocatorService.AssignHub 拿真实 hub_ds_addr + hub_ticket。
	// addr 为空 → 不调,回退自签 hub 票据 + MockHubDSAddr(便于本机不起 hub_allocator 也能跑通 login)。
	Hub HubClientConf `yaml:"hub,omitempty" json:"hub,omitempty"`

	// AllowedRoleIDs 是选角白名单(选角权威化 2026-07-08,SelectRole RPC 服务端校验)。
	// 对齐客户端 CfgMisc.DefaultRoleIDs(选角界面可选列表)。
	// 非空 = 严格白名单;空 = fail-closed,SelectRole 一律拒绝(防改包客户端签任意 role_id
	// 进 hub 票据)。dev 宽松(空白名单只校非 0)需显式开 DevAllowAnyRole。
	AllowedRoleIDs []uint32 `yaml:"allowed_role_ids,omitempty" json:"allowed_role_ids,omitempty"`

	// DevAllowAnyRole 开发期选角宽松开关(默认 false,⚠️ 严禁上生产)。
	// 为 true 且 AllowedRoleIDs 为空时,SelectRole 只校验 role_id 非 0(配合客户端配置表快速迭代)。
	DevAllowAnyRole bool `yaml:"dev_allow_any_role,omitempty" json:"dev_allow_any_role,omitempty"`
}

// LocatorClientConf 是 login 调 player_locator 的客户端参数。
type LocatorClientConf struct {
	// Addr player_locator gRPC 端口(默认 127.0.0.1:50006)。
	// 留空 → 不调 locator,Login 走 fallback(仅 Warn 日志)。
	Addr string `yaml:"addr,omitempty" json:"addr,omitempty"`
}

// HubClientConf 是 login 调 hub_allocator 的客户端参数(W4 ⑥)。
type HubClientConf struct {
	// Addr hub_allocator gRPC 端口(默认 127.0.0.1:50021)。
	// 留空 → 不调 hub_allocator,Login 回退自签 hub 票据 + MockHubDSAddr。
	Addr string `yaml:"addr,omitempty" json:"addr,omitempty"`

	// Region 传给 AssignHub 的大厅区服(空 = 让 hub_allocator 选最空分片)。
	Region string `yaml:"region,omitempty" json:"region,omitempty"`
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

// Defaults 把零值填成 Pandora 标准默认值。
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
