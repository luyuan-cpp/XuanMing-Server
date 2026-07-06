// Package conf 是 matchmaker 服务的私有配置结构。
package conf

import (
	"time"

	"github.com/luyuancpp/pandora/pkg/config"
)

// Config 是 matchmaker 服务的完整配置。
type Config struct {
	config.Base `yaml:",inline" mapstructure:",squash"`

	Match MatchConf `yaml:"match" json:"match"`

	// JWT 用于给战斗 DS 票据签名(matchmaker 全员确认 → 调 ds_allocator 拉 DS →
	// 给每个玩家签一张 battle DSTicket)。secret 必须与 login / Envoy jwt_authn 一致。
	// 留空(无 ds_allocator_addr)时不签票据,仍走 StubDSAllocator。
	JWT JWTConf `yaml:"jwt,omitempty" json:"jwt,omitempty"`
}

// JWTConf 是签发 battle DSTicket 的 JWT 参数(镜像 login.JWTConf)。
//
// Issuer / Audience / Secret 必须与 login 服务和 Envoy jwt_authn provider 完全一致。
type JWTConf struct {
	Issuer      string          `yaml:"issuer,omitempty" json:"issuer,omitempty"`
	Audience    string          `yaml:"audience,omitempty" json:"audience,omitempty"`
	Secret      string          `yaml:"secret,omitempty" json:"secret,omitempty"`
	SessionTTL  config.Duration `yaml:"session_ttl,omitempty" json:"session_ttl,omitempty"`
	DSTicketTTL config.Duration `yaml:"ds_ticket_ttl,omitempty" json:"ds_ticket_ttl,omitempty"`
}

// MatchConf 是 matchmaker 服务私有配置。
type MatchConf struct {
	// TeamAddr 是 team 服务 gRPC 直连地址(StartMatch 时拉取队伍快照校验 READY)。
	// 留空则 StartMatch 跳过 team 校验(本机不起 team 也能跑撮合骨架)。
	TeamAddr string `yaml:"team_addr,omitempty" json:"team_addr,omitempty"`

	// DSAllocatorAddr 是 ds_allocator 服务 gRPC 直连地址(全员确认后拉战斗 DS)。
	// 留空则用 StubDSAllocator(W4 ① 行为,返回固定 mock 地址 + mock 票据)。
	DSAllocatorAddr string `yaml:"ds_allocator_addr,omitempty" json:"ds_allocator_addr,omitempty"`
	// LocatorAddr 是 player_locator 服务 gRPC 直连地址（撮合状态机上报玩家位置：
	// 成局→MATCHING、就绪→BATTLE，不变量 §1）。留空则不上报（本机不起 locator 也能跑撮合）。
	LocatorAddr string `yaml:"locator_addr,omitempty" json:"locator_addr,omitempty"`

	// BattleGateFailOpen 控制 StartMatch 前置"战斗中禁止匹配"检查在 player_locator 查询失败时的行为。
	//   - false（默认，生产安全 / fail-closed）：locator 查询失败时拒绝入队（返回 ERR_UNAVAILABLE 让客户端重试），
	//     只有明确查到成员非 BATTLE 才放行；避免 locator 短暂抖动 + 旧 claim 过期时绕过保护，把战斗中玩家二次塞进队列。
	//   - true（仅 dev / 弱依赖联调）：locator 查询失败时仅 Warn 后放行，兜底仍由 ClaimPlayer 的 SETNX 保证一人一队列。
	// 注意：locator 完全未配置（LocatorAddr 为空 → 注入 nil）时此开关不生效，直接跳过检查。
	BattleGateFailOpen bool `yaml:"battle_gate_fail_open,omitempty" json:"battle_gate_fail_open,omitempty"`
	// MapId 撮合成局后请求的战斗地图配置 ID(配置表 ID,uint32)。
	MapId uint32 `yaml:"map_id,omitempty" json:"map_id,omitempty"`

	// GameMode 战斗模式标识(如 "5v5_ranked"),透传给 ds_allocator。
	GameMode string `yaml:"game_mode,omitempty" json:"game_mode,omitempty"`

	// ConfirmTimeout 确认期时长,凑齐 10 人后等待全员确认的窗口(默认 15s)。
	ConfirmTimeout config.Duration `yaml:"confirm_timeout,omitempty" json:"confirm_timeout,omitempty"`

	// MatchInterval 后台撮合循环的扫描间隔(默认 2s)。
	MatchInterval config.Duration `yaml:"match_interval,omitempty" json:"match_interval,omitempty"`

	// TicketTTL 排队票据 Redis key 的 TTL(默认 30min,防僵尸票据)。
	TicketTTL config.Duration `yaml:"ticket_ttl,omitempty" json:"ticket_ttl,omitempty"`

	// MatchTTL 已撮合 match Redis key 的 TTL(默认 30min)。
	MatchTTL config.Duration `yaml:"match_ttl,omitempty" json:"match_ttl,omitempty"`

	// TeamSize 一方人数(MOBA 5v5,一方 5 人)。
	TeamSize int `yaml:"team_size,omitempty" json:"team_size,omitempty"`

	// EnableSoloMatch 仅用于本地端到端联调。开启后,单张队伍票据可以直接成局并拉起 Battle DS。
	// 生产环境必须保持 false。
	EnableSoloMatch bool `yaml:"enable_solo_match,omitempty" json:"enable_solo_match,omitempty"`

	// AutoConfirmMatch 仅用于本地端到端联调。开启后,撮合成功后跳过客户端确认期并直接拉 Battle DS。
	// 生产环境必须保持 false。
	AutoConfirmMatch bool `yaml:"auto_confirm_match,omitempty" json:"auto_confirm_match,omitempty"`

	// MmrBaseWindow 初始 MMR 撮合窗口半宽(默认 200);两张票 avg_mmr 差 ≤ 窗口才可同场。
	MmrBaseWindow int `yaml:"mmr_base_window,omitempty" json:"mmr_base_window,omitempty"`

	// MmrWidenPerSec 每等待 1 秒窗口放宽的 MMR(默认 20),等待越久越容易撮合。
	MmrWidenPerSec int `yaml:"mmr_widen_per_sec,omitempty" json:"mmr_widen_per_sec,omitempty"`

	// MmrMaxWindow MMR 窗口放宽上限(默认 2000),超过即不再放宽。
	MmrMaxWindow int `yaml:"mmr_max_window,omitempty" json:"mmr_max_window,omitempty"`

	// OptimisticRetry WATCH/MULTI/EXEC 乐观锁冲突时最大重试次数。
	// 耗尽后返回 ErrMatchConcurrent(4006)。
	OptimisticRetry int `yaml:"optimistic_retry,omitempty" json:"optimistic_retry,omitempty"`
}

// Defaults 填默认值,防止 yaml 缺字段时零值引发 panic。
func (c *Config) Defaults() {
	if c.Match.ConfirmTimeout == 0 {
		c.Match.ConfirmTimeout = config.Duration(15 * time.Second)
	}
	if c.Match.MatchInterval == 0 {
		c.Match.MatchInterval = config.Duration(2 * time.Second)
	}
	if c.Match.TicketTTL == 0 {
		c.Match.TicketTTL = config.Duration(30 * time.Minute)
	}
	if c.Match.MatchTTL == 0 {
		c.Match.MatchTTL = config.Duration(30 * time.Minute)
	}
	if c.Match.TeamSize == 0 {
		c.Match.TeamSize = 5
	}
	if c.Match.MmrBaseWindow == 0 {
		c.Match.MmrBaseWindow = 200
	}
	if c.Match.MmrWidenPerSec == 0 {
		c.Match.MmrWidenPerSec = 20
	}
	if c.Match.MmrMaxWindow == 0 {
		c.Match.MmrMaxWindow = 2000
	}
	if c.Match.OptimisticRetry == 0 {
		c.Match.OptimisticRetry = 3
	}
	if c.Match.MapId == 0 {
		c.Match.MapId = 1
	}
	if c.Match.GameMode == "" {
		c.Match.GameMode = "5v5_ranked"
	}
	if c.Server.Grpc.Addr == "" {
		c.Server.Grpc.Addr = ":50011"
	}
	if c.Server.Http.Addr == "" {
		c.Server.Http.Addr = ":51011"
	}
}
