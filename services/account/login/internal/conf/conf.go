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
	"fmt"
	"slices"
	"time"

	"github.com/luyuancpp/pandora/pkg/config"
)

// Config 是 login 服务的完整配置。
type Config struct {
	// Base 公共字段(Server/Node/Snowflake/Locker/Registry/Timeouts/Kafka)。
	config.Base `yaml:",inline" mapstructure:",squash"`

	// Login 业务字段。
	Login LoginConf `yaml:"login" json:"login"`

	// DSAuth 是 UE DS 经 :8444 调 VerifyDSTicket 时的服务身份与 Redis active 权威配置。
	// 默认 off/legacy，保持既有内部 Verify 行为；仅 redis+enforce 启用在线入场门。
	DSAuth config.DSAuthConf `yaml:"ds_auth,omitempty" json:"ds_auth,omitempty"`
}

// LoginConf 是 login 服务私有配置。
type LoginConf struct {
	// SessionTokenTTL session_token 的有效期(写到 Redis,也用作 JWT exp)。
	SessionTokenTTL config.Duration `yaml:"session_token_ttl,omitempty" json:"session_token_ttl,omitempty"`

	// DSTicketTTL DS 票据有效期(JWT exp - issued_at)。
	// 不变量 §3:DS 票据短时效。默认 5 分钟。
	DSTicketTTL config.Duration `yaml:"ds_ticket_ttl,omitempty" json:"ds_ticket_ttl,omitempty"`

	// RequireHubAssignmentBinding 是 Hub DSTicket 归属绑定的机械激活栅栏。
	// false(默认):滚动兼容旧的无绑定 hub 票，但带绑定票仍会严格查 Redis 当前归属。
	// true:拒绝所有无绑定 hub 票，并禁止 login 在 hub_allocator 缺失/失败时自签回退。
	// 开启前必须同时配置 Redis 与 hub_allocator，否则启动失败。
	RequireHubAssignmentBinding bool `yaml:"require_hub_assignment_binding,omitempty" json:"require_hub_assignment_binding,omitempty"`

	// HubAssignmentFence 与 DS Redis authority 共用全局 required writer/capability lease。
	// binding=true 时必填，确保旧 login writer 激活后不能回滚接流量。
	HubAssignmentFence config.DSAuthFenceConf `yaml:"hub_assignment_fence,omitempty" json:"hub_assignment_fence,omitempty"`

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

	// DSTicket 是玩家 DSTicket v2(RS256 非对称,方案 B)签发配置。private_key_file 非空
	// 即启用:login 重连/公共签发的 battle 票改走 v2 实例绑定签发(roster 权威门同一
	// Redis 快照必须能提供 pod/uid/epoch/allocation,缺失 fail-closed 拒签);hub 票一律
	// 拒签(v2 hub 票只能由 hub_allocator 签)。留空 = 沿用 legacy HS256(dev 行为不变)。
	DSTicket config.DSTicketConf `yaml:"ds_ticket,omitempty" json:"ds_ticket,omitempty"`

	// Locator W3 ⑤ 联动:登录成功后调 PlayerLocatorService.SetLocation(state=LOGIN_PENDING)。
	// addr 为空仅允许 local/off；RequireHubAssignmentBinding=true 时它是 Hub 分配前的
	// 权威门，必须配置且查询/LOGIN_PENDING 写入失败均 fail-closed。
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
	// 留空仅允许 local/off；Hub assignment binding 激活时 Validate 会拒绝启动。
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
	Issuer   string `yaml:"issuer,omitempty" json:"issuer,omitempty"`
	Audience string `yaml:"audience,omitempty" json:"audience,omitempty"`
	Secret   string `yaml:"secret,omitempty" json:"secret,omitempty"`
	// AdditionalSecrets 是**仅用于校验**的额外可接受密钥(不用于签发),支持玩家面
	// JWT 不停服密钥轮换(三段式,同 ds_auth.additional_secrets;注意 Envoy JWKS 也要
	// 同步包含全部 key,gen_cluster_config.ps1 -SecretAdditional 一道注入)。默认空。
	AdditionalSecrets []string        `yaml:"additional_secrets,omitempty" json:"additional_secrets,omitempty"`
	SessionTTL        config.Duration `yaml:"session_ttl,omitempty" json:"session_ttl,omitempty"`
	DSTicketTTL       config.Duration `yaml:"ds_ticket_ttl,omitempty" json:"ds_ticket_ttl,omitempty"`
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
	c.DSAuth.Defaults()
}

// Validate 校验不能靠运行期降级修复的配置冲突。
func (c *Config) Validate() error {
	if c.Login.DSTicket.SignerEnabled() && c.Login.DSTicket.ActiveKid == "" {
		return fmt.Errorf("login.ds_ticket signer requires explicit active_kid")
	}
	if c.Login.DSTicket.VerifierEnabled() &&
		(c.Login.DSTicket.ActiveKid == "" || c.Login.DSTicket.KeysetRevision == "") {
		return fmt.Errorf("login.ds_ticket verifier requires explicit active_kid and keyset_revision")
	}
	switch c.DSAuth.AuthorityMode {
	case "", "legacy", "redis":
	default:
		return fmt.Errorf("ds_auth.authority_mode invalid: %q (want legacy|redis)", c.DSAuth.AuthorityMode)
	}
	if err := c.DSAuth.ValidateRedisFence(); err != nil {
		return err
	}
	if c.DSAuth.AuthorityModeRedis() {
		if !c.Login.RequireHubAssignmentBinding {
			return fmt.Errorf("ds_auth.authority_mode=redis requires login.require_hub_assignment_binding=true")
		}
		if !sameFence(c.DSAuth.Fence, c.Login.HubAssignmentFence) {
			return fmt.Errorf("login ds_auth.fence and hub_assignment_fence must be identical (single capability lease)")
		}
	}
	if c.Login.RequireHubAssignmentBinding {
		if c.Node.RedisClient.Host == "" && len(c.Node.RedisClient.Addrs) == 0 {
			return fmt.Errorf("login.require_hub_assignment_binding=true requires node.redis_client")
		}
		if c.Login.Hub.Addr == "" {
			return fmt.Errorf("login.require_hub_assignment_binding=true requires login.hub.addr")
		}
		if c.Login.Locator.Addr == "" {
			return fmt.Errorf("login.require_hub_assignment_binding=true requires login.locator.addr")
		}
		if len(c.Login.HubAssignmentFence.EtcdEndpoints) == 0 || c.Login.HubAssignmentFence.KeysetRevision == "" {
			return fmt.Errorf("login.require_hub_assignment_binding=true requires login.hub_assignment_fence etcd endpoints/keyset revision")
		}
	}
	return nil
}

// CapabilityFence 返回 login 唯一应注册的 capability 配置。Redis admission 与既有
// Hub assignment fence 同时开启时 Validate 已要求二者完全一致，因此 main 只 Acquire 一次。
func (c *Config) CapabilityFence() (config.DSAuthFenceConf, bool) {
	if c.DSAuth.AuthorityModeRedis() {
		return c.DSAuth.Fence, true
	}
	if c.Login.RequireHubAssignmentBinding {
		return c.Login.HubAssignmentFence, true
	}
	return config.DSAuthFenceConf{}, false
}

func sameFence(a, b config.DSAuthFenceConf) bool {
	return slices.Equal(a.EtcdEndpoints, b.EtcdEndpoints) && a.EtcdPrefix == b.EtcdPrefix &&
		a.EtcdLeaseTTLSec == b.EtcdLeaseTTLSec && a.EtcdDialTimeout == b.EtcdDialTimeout &&
		a.KeysetRevision == b.KeysetRevision
}
