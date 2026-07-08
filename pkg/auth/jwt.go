// Package auth 提供 Pandora 统一的 JWT 签发 / 校验工具。
//
// W3 ① 落地(2026-06-05):
//   - login 服务签:
//   - SessionToken:玩家登录后的会话凭证(sub=player_id, exp=24h)
//   - DSTicket:玩家进入 hub / battle DS 前的短期票据(exp=5min)
//   - Envoy 边缘网关用 jwt_authn filter 校验 SessionToken,把 sub claim 提到
//     `x-pandora-player-id` 头(给业务服 middleware 用)
//   - 业务服(push / 后续 13 服)收到请求时 player_id 已在 header 里,
//     不需要再解 JWT;但 DSTicket 还得在 login.VerifyDSTicket 里二次校验(防重放 jti)
//
// 选型:
//   - 算法 HS256(对称 HMAC):dev 期最简单;Envoy jwt_authn 用 `local_jwks` inline
//     一份 `kty=oct` 的 JWKS 即可。**生产期切 RS256**(login 私钥签 / Envoy 公钥验,
//     防 Envoy 被攻破后能签任意 token)。
//   - 库 github.com/golang-jwt/jwt/v5:维护活跃、API 稳。
//
// 不变量(CLAUDE.md §9):
//   - 不变量 §3:DS 票据短时效(本包 DSTicketTTL 默认 5min)
//   - 不变量 §5:proto 字段编号永不复用(本包 claim key 也照此原则,新增不删旧)
//   - jti 必须每次唯一(uuid v4),防重放靠 redis 黑名单(W3 ② 接入,见 TODO)
package auth

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// DSType 区分票据签的是哪种 DS。
type DSType string

const (
	DSTypeHub    DSType = "hub"
	DSTypeBattle DSType = "battle"
)

// SessionClaims 是 SessionToken 的载荷。
//
// 标准 RegisteredClaims:iss / sub / aud / exp / iat / jti
//   - sub:player_id 的十进制字符串(JWT 规范 sub 是 string)
//   - aud:固定 "pandora-client"(Envoy jwt_authn 校验)
//   - iss:固定 "pandora-login"
//   - exp:发行时刻 + SessionTTL(默认 24h)
//   - jti:uuid v4,W3 ② redis 加黑名单可吊销
type SessionClaims struct {
	jwt.RegisteredClaims
}

// PlayerID 把 sub 字符串解成 uint64。失败返回 0。
func (s *SessionClaims) PlayerID() uint64 {
	if s.Subject == "" {
		return 0
	}
	id, err := strconv.ParseUint(s.Subject, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// DSTicketClaims 是 DSTicket 的载荷(短时效,5min)。
//
// 自定义 claim:
//   - ds_type:"hub" / "battle"
//   - match_id:battle DS 才有(hub 为 0)
//   - region_id / cell_id:玩家确定性路由落点(docs/design/scale-cellular-20m.md §3.3)。
//     把 DS 票据绑定到 Region+Cell,防跨单元串号(stale / 伪造票据把玩家从 A 单元的 DS
//     接进 B 单元)。omitempty:单 Cell / dev(0)时不序列化该 claim,与历史票据完全兼容。
//     uint32 拓扑维度(非 snowflake 业务 ID,CLAUDE.md §9.12)。
type DSTicketClaims struct {
	jwt.RegisteredClaims
	DSType   string `json:"ds_type"`
	MatchID  uint64 `json:"match_id,omitempty"`
	RegionID uint32 `json:"region_id,omitempty"`
	CellID   uint32 `json:"cell_id,omitempty"`
	// RoleID:玩家已选角色配置 ID(CfgRole.Id,选角权威化 2026-07-08)。hub 票据携带,
	// DS 验签后直接用它 spawn 角色,不再信任客户端 URL ?role= 自报。
	// omitempty:0(未选角 / battle 票走 roster)时不序列化,与历史票据兼容。
	// uint32 配置表 ID(非 snowflake 业务 ID,CLAUDE.md §9.12)。
	RoleID uint32 `json:"role_id,omitempty"`
}

// PlayerID 把 sub 字符串解成 uint64。失败返回 0。
func (t *DSTicketClaims) PlayerID() uint64 {
	if t.Subject == "" {
		return 0
	}
	id, err := strconv.ParseUint(t.Subject, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// Config 是 JWT signer / verifier 公共配置。
//
// Secret 必须 ≥ 32 字节(HS256 推荐安全长度);生产期换 RS256 时本字段废弃,
// 改为 PrivateKeyPEM / PublicKeyPEM。
type Config struct {
	// Issuer 固定 "pandora-login"(JWT iss 字段)。
	Issuer string

	// Audience 固定 "pandora-client"(JWT aud 字段,Envoy jwt_authn 校验)。
	Audience string

	// Secret HS256 共享密钥;dev 期 login 服务跟 Envoy 各持一份(同一字符串)。
	Secret []byte

	// SessionTTL SessionToken 有效期,默认 24h。
	SessionTTL time.Duration

	// DSTicketTTL DSTicket 有效期,默认 5min(不变量 §3)。
	DSTicketTTL time.Duration

	// NowFn 可注入(测试用),默认 time.Now。
	NowFn func() time.Time
}

// Defaults 把零值填默认。
func (c *Config) Defaults() {
	if c.Issuer == "" {
		c.Issuer = "pandora-login"
	}
	if c.Audience == "" {
		c.Audience = "pandora-client"
	}
	if c.SessionTTL == 0 {
		c.SessionTTL = 24 * time.Hour
	}
	if c.DSTicketTTL == 0 {
		c.DSTicketTTL = 5 * time.Minute
	}
	if c.NowFn == nil {
		c.NowFn = time.Now
	}
}

// Validate 拒绝弱配置。
//
// HS256 推荐 secret 长度 ≥ 算法输出长度 (256 bit = 32 byte),RFC 7518 §3.2 明说
// “A key of the same size as the hash output ... is considered sufficient”。低于 32 字节
// 会被 golang-jwt/jwt/v5 未来版本拒签 (已结论为 CVE 倒退)。
func (c *Config) Validate() error {
	if len(c.Secret) < 32 {
		return fmt.Errorf("auth.Config: Secret too short (got %d bytes, need >=32 for HS256)", len(c.Secret))
	}
	return nil
}

// Signer 签发 SessionToken / DSTicket。线程安全(无可变状态)。
type Signer struct {
	cfg Config
}

// Verifier 校验 SessionToken / DSTicket。线程安全。
type Verifier struct {
	cfg Config
}

// NewSigner 构造 Signer,Validate 失败 panic(只该在 main 启动期调用)。
func NewSigner(cfg Config) (*Signer, error) {
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Signer{cfg: cfg}, nil
}

// NewVerifier 构造 Verifier。
func NewVerifier(cfg Config) (*Verifier, error) {
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Verifier{cfg: cfg}, nil
}

// SessionTTL 暴露 SessionTTL 给调用方(login.biz 用来设置 redis session TTL,
// 保证 redis 过期跟 JWT exp 对齐)。
func (s *Signer) SessionTTL() time.Duration { return s.cfg.SessionTTL }

// DSTicketTTL 暴露 DSTicketTTL,用于 jti 防重放 TTL(verifier 校验通过后 SETNX)。
func (s *Signer) DSTicketTTL() time.Duration { return s.cfg.DSTicketTTL }

// DSTicketTTL 同上,verifier 侧也提供一个(调 jti SETNX 时常在 verify 处)。
func (v *Verifier) DSTicketTTL() time.Duration { return v.cfg.DSTicketTTL }

// SignSession 签发 SessionToken。jti 由调用方传(uuid v4)。
//
// 返回:JWT 字符串 / 过期时刻(unix ms,给客户端展示用)/ error。
func (s *Signer) SignSession(playerID uint64, jti string) (token string, expiresAtMs int64, err error) {
	if playerID == 0 {
		return "", 0, errors.New("auth.SignSession: playerID must be > 0")
	}
	if jti == "" {
		return "", 0, errors.New("auth.SignSession: jti must be non-empty")
	}
	now := s.cfg.NowFn()
	exp := now.Add(s.cfg.SessionTTL)
	claims := SessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.cfg.Issuer,
			Subject:   strconv.FormatUint(playerID, 10),
			Audience:  jwt.ClaimStrings{s.cfg.Audience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        jti,
		},
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	str, err := t.SignedString(s.cfg.Secret)
	if err != nil {
		return "", 0, fmt.Errorf("auth.SignSession: %w", err)
	}
	return str, exp.UnixMilli(), nil
}

// SignDSTicket 签发 DS 票据。dsType / matchID 按 docs/design/proto-design.md DSTicket message。
//
// 不变量 §3:本方法默认 TTL=5min。
//
// 单 Cell / dev 语义(region/cell = 0):本方法等价于 SignDSTicketWithCell(...,0,0,...)。
// 多 Cell 部署请用 SignDSTicketWithCell 把玩家落点绑进票据(§3.3 防跨单元串号)。
func (s *Signer) SignDSTicket(playerID uint64, dsType DSType, matchID uint64, jti string) (token string, expiresAtMs int64, err error) {
	return s.SignDSTicketWithCell(playerID, dsType, matchID, 0, 0, jti)
}

// SignDSTicketWithCell 签发绑定 Region+Cell 的 DS 票据(docs/design/scale-cellular-20m.md §3.3)。
//
// regionID / cellID 是玩家的确定性路由落点(由调用方经 cellroute.Router 算出);单 Cell / dev
// 传 0(此时与 SignDSTicket 行为一致,claim 不序列化)。把落点签进票据后,DS 侧可校验
// "票据 Cell == 本 DS 所在 Cell",拒绝 stale / 伪造票据跨单元串号。
//
// 不变量 §3:默认 TTL=5min。
func (s *Signer) SignDSTicketWithCell(playerID uint64, dsType DSType, matchID uint64, regionID, cellID uint32, jti string) (token string, expiresAtMs int64, err error) {
	return s.SignDSTicketFull(playerID, dsType, matchID, regionID, cellID, 0, jti)
}

// SignDSTicketFull 签发携带全部可选 claim(region/cell/role)的 DS 票据。
//
// roleID:玩家已选角色(选角权威化 2026-07-08)。hub 票据传已选角;battle 票据传 0
// (对局角色走 match roster,不走票据)。0 时 claim 不序列化(omitempty),与旧票完全兼容。
//
// 其余语义同 SignDSTicketWithCell。不变量 §3:默认 TTL=5min。
func (s *Signer) SignDSTicketFull(playerID uint64, dsType DSType, matchID uint64, regionID, cellID, roleID uint32, jti string) (token string, expiresAtMs int64, err error) {
	if playerID == 0 {
		return "", 0, errors.New("auth.SignDSTicket: playerID must be > 0")
	}
	if dsType != DSTypeHub && dsType != DSTypeBattle {
		return "", 0, fmt.Errorf("auth.SignDSTicket: invalid dsType %q", dsType)
	}
	if jti == "" {
		return "", 0, errors.New("auth.SignDSTicket: jti must be non-empty")
	}
	if dsType == DSTypeBattle && matchID == 0 {
		return "", 0, errors.New("auth.SignDSTicket: battle DSTicket requires matchID")
	}
	now := s.cfg.NowFn()
	exp := now.Add(s.cfg.DSTicketTTL)
	claims := DSTicketClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.cfg.Issuer,
			Subject:   strconv.FormatUint(playerID, 10),
			Audience:  jwt.ClaimStrings{s.cfg.Audience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        jti,
		},
		DSType:   string(dsType),
		MatchID:  matchID,
		RegionID: regionID,
		CellID:   cellID,
		RoleID:   roleID,
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	str, err := t.SignedString(s.cfg.Secret)
	if err != nil {
		return "", 0, fmt.Errorf("auth.SignDSTicket: %w", err)
	}
	return str, exp.UnixMilli(), nil
}

// VerifySession 校验 SessionToken,返回 claims。
//
// 失败返回 *errcode.Error(ErrLoginTicketExpired / ErrLoginTicketInvalid),
// 业务侧用 errcode.As 转 proto code。
func (v *Verifier) VerifySession(token string) (*SessionClaims, error) {
	var claims SessionClaims
	if err := v.parseInto(token, &claims); err != nil {
		return nil, err
	}
	if claims.PlayerID() == 0 {
		return nil, errcode.New(errcode.ErrLoginTicketInvalid, "session sub not a valid player_id")
	}
	return &claims, nil
}

// VerifyDSTicket 校验 DSTicket,返回 claims。
//
// 校验项:
//   - 签名 / exp / iss / aud(parseInto)
//   - sub 为有效 player_id
//   - ds_type 必在 "hub" / "battle"
//   - dsType=battle 时 match_id 必非 0(与 SignDSTicket 防御性检查对称,防伪造 token 跳过 sign 分支)
//
// 防重放(jti 黑名单)需要调用方再走一次 redis SET NX EX 检查;本方法只验签 + exp。
func (v *Verifier) VerifyDSTicket(token string) (*DSTicketClaims, error) {
	var claims DSTicketClaims
	if err := v.parseInto(token, &claims); err != nil {
		return nil, err
	}
	if claims.PlayerID() == 0 {
		return nil, errcode.New(errcode.ErrLoginTicketInvalid, "ds ticket sub not a valid player_id")
	}
	if claims.DSType != string(DSTypeHub) && claims.DSType != string(DSTypeBattle) {
		return nil, errcode.New(errcode.ErrLoginTicketInvalid, "ds ticket dsType invalid: %q", claims.DSType)
	}
	if claims.DSType == string(DSTypeBattle) && claims.MatchID == 0 {
		return nil, errcode.New(errcode.ErrLoginTicketInvalid, "battle ds ticket missing match_id")
	}
	return &claims, nil
}

// parseInto 把 token 解到 dst claims;统一翻译标准 jwt 错误到 errcode。
func (v *Verifier) parseInto(token string, dst jwt.Claims) error {
	if token == "" {
		return errcode.New(errcode.ErrLoginTicketInvalid, "empty token")
	}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(v.cfg.Issuer),
		jwt.WithAudience(v.cfg.Audience),
		jwt.WithTimeFunc(v.cfg.NowFn),
	)
	_, err := parser.ParseWithClaims(token, dst, func(t *jwt.Token) (interface{}, error) {
		return v.cfg.Secret, nil
	})
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return errcode.New(errcode.ErrLoginTicketExpired, "token expired: %v", err)
	case errors.Is(err, jwt.ErrTokenNotValidYet),
		errors.Is(err, jwt.ErrTokenSignatureInvalid),
		errors.Is(err, jwt.ErrTokenInvalidIssuer),
		errors.Is(err, jwt.ErrTokenInvalidAudience),
		errors.Is(err, jwt.ErrTokenMalformed):
		return errcode.New(errcode.ErrLoginTicketInvalid, "token invalid: %v", err)
	default:
		return errcode.New(errcode.ErrLoginTicketInvalid, "token parse failed: %v", err)
	}
}

// JWKSInlineHS256 用 Config.Secret 输出一份 Envoy jwt_authn local_jwks 可直接 inline 的 JSON。
//
// Envoy jwt_authn 接受 RFC 7517 JWKS 格式,HS256 用 `kty=oct` + `k=base64url(secret)`。
//
// 用法(deploy/envoy/envoy.yaml):
//
//	providers:
//	  pandora_session:
//	    issuer: pandora-login
//	    audiences: [pandora-client]
//	    local_jwks:
//	      inline_string: |
//	        {"keys":[{"kty":"oct","alg":"HS256","kid":"pandora-dev","k":"<base64url>"}]}
//
// 本函数返回上面 inline_string 的内容,便于把 secret 跟 envoy.yaml 同步起来时只改一处。
func JWKSInlineHS256(secret []byte, kid string) string {
	if kid == "" {
		kid = "pandora-dev"
	}
	k := base64.RawURLEncoding.EncodeToString(secret)
	return fmt.Sprintf(`{"keys":[{"kty":"oct","alg":"HS256","kid":%q,"k":%q}]}`, kid, k)
}

// SecretEqual 常量时间比较两个 secret;dev / 配置检查用。
func SecretEqual(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}
