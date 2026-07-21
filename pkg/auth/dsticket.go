// Package auth — DSTicket v2(方案 B):RS256 非对称签发 / 验签 + 严格 JWKS。
//
// 决策依据:docs/design/decision-revisit-player-jwt-key-rotation.md §7(2026-07-13 已拍板方案 B,
// 子形态 B1 纯本地验票)。架构红线:
//   - DS(Agones Fleet 内 UE 专用服务器)只持有公钥 JWKS,永不持有私钥 / 玩家 HMAC;
//   - DSTicket 与 SessionToken / DS 回调令牌是独立信任域:独立 issuer("pandora-dsticket")、
//     独立 audience("pandora-game-ds")、独立密钥体系(RSA vs HS256),编译期用独立 Go 类型隔离;
//   - 常规发布永不轮换密钥:K1 密钥对只在首次迁移时创建;轮换是独立罕见安全操作
//     (JWKS 同时发布 K1+K2 → 签发切 K2 → 票据 TTL 过后移除 K1)。
//
// v2 claims(dst_ver=2)把票据签死到唯一 DS 实例:
//   - 通用:ds_type / ds_pod / ds_uid / ds_instance_epoch / release_track;
//   - hub:hub_assignment_id 必填,match_id 必为 0;
//   - battle:match_id / allocation_id 必填;
//   - 有意不绑定 DS 回调凭据的 ds_gen / ds_credential_jti:回调凭据轮换与玩家准入无关,
//     v1 的这一绑定会让凭据轮换误伤已签出的玩家票(v1 设计缺陷,v2 修正)。
//
// TTL 契约(B1 纯本地验票的强制补偿):票是短时一次性 capability,默认 120s、上限 180s。
// DS 侧同样强制 exp-iat ≤ 180s,双向防「长票 + 本地验签 = 吊销失效」的自相矛盾配置。
package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strconv"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// DSTicket v2 信任域常量。与 SessionToken(pandora-login → pandora-client)、
// DS 回调(pandora-ds-control → pandora-ds)严格分域,交叉使用必然验签失败。
const (
	DSTicketIssuer   = "pandora-dsticket"
	DSTicketAudience = "pandora-game-ds"

	// DSTicketVersion2 是 dst_ver claim 的当前值;验签侧精确匹配,旧 v1(无 dst_ver)一律拒。
	DSTicketVersion2 = 2

	// DSTicketDefaultTTL / DSTicketMaxTTL:B1 纯本地验票下票据即短时 capability。
	// 吊销时延上界 = TTL + DS 侧 leeway(≤15s),这是选 B1 时明确接受的取舍。
	DSTicketDefaultTTL = 2 * time.Minute
	DSTicketMaxTTL     = 3 * time.Minute

	// DSTicketMinRSABits:RSA 密钥最小位数(NIST SP 800-57 当前基线)。
	DSTicketMinRSABits = 2048

	// dsTicketJWKSMaxBytes / dsTicketJWKSMaxKeys:JWKS 文件解析上限(fail-closed,防投递事故)。
	dsTicketJWKSMaxBytes = 64 * 1024
	dsTicketJWKSMaxKeys  = 8
)

// ReleaseTrack 取值(Stable/Canary Fleet 隔离,zero-downtime-update.md §6)。
const (
	ReleaseTrackStable = "stable"
	ReleaseTrackCanary = "canary"
)

// DSTicketClaimsV2 是 dst_ver=2 票据载荷。字段命名与 v1 保持 claim key 兼容原则
// (proto 不变量 §5 精神:key 只增不改),新增 dst_ver / ds_instance_epoch / allocation_id / release_track。
type DSTicketClaimsV2 struct {
	jwt.RegisteredClaims
	DstVer          int    `json:"dst_ver"`
	DSType          string `json:"ds_type"`
	DSPodName       string `json:"ds_pod"`
	DSInstanceUID   string `json:"ds_uid"`
	DSInstanceEpoch uint32 `json:"ds_instance_epoch"`
	ReleaseTrack    string `json:"release_track,omitempty"`
	RegionID        uint32 `json:"region_id,omitempty"`
	CellID          uint32 `json:"cell_id,omitempty"`
	RoleID          uint32 `json:"role_id,omitempty"`
	MatchID         uint64 `json:"match_id,omitempty"`
	AllocationID    string `json:"allocation_id,omitempty"`
	HubAssignmentID string `json:"hub_assignment_id,omitempty"`
	// SourceMatchID:Battle→Hub 回流 fence(2026-07-21)。hub 票可带:玩家从终局
	// (ended/abandoned)对局返回大厅时,签票权威(login 三态门)把原 Battle match_id
	// 盖进来,Hub DS 用它调 SetLocation(HUB, fence=source_match_id) 通过 locator 的
	// BATTLE→HUB guard(不变量 §1),消除终局 TTL 残留导致的 4007「玩家正在战斗中」。
	// battle 票必须 0(battle 绑定走 match_id);普通登录 hub 票为 0(claim 不序列化)。
	SourceMatchID uint64 `json:"source_match_id,omitempty"`
}

// PlayerID 把 sub 字符串解成 uint64。失败返回 0。
func (t *DSTicketClaimsV2) PlayerID() uint64 {
	if t.Subject == "" {
		return 0
	}
	id, err := strconv.ParseUint(t.Subject, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// DSTicketTarget 是签票时的目标 DS 实例身份(全部来自受信控制面的权威快照,
// 例如 hub_allocator 的 assignment 记录 / ds_allocator 的 ReadyAuthorized 快照)。
type DSTicketTarget struct {
	// DSPodName / DSInstanceUID / DSInstanceEpoch:目标实例三元组,三者必填。
	DSPodName       string
	DSInstanceUID   string
	DSInstanceEpoch uint32
	// ReleaseTrack:"stable" / "canary"，必填；单 Fleet 部署也显式写 stable，
	// 禁止把空值解释成隐式默认轨道。
	ReleaseTrack string
	// HubAssignmentID:hub 票必填(玩家当前归属版本);battle 票必空。
	HubAssignmentID string
	// MatchID / AllocationID:battle 票必填;hub 票必空/0。
	MatchID      uint64
	AllocationID string
	// SourceMatchID:Battle→Hub 回流 fence(见 DSTicketClaimsV2.SourceMatchID)。
	// 仅 hub 票可带(>0 表示终局回流);battle 票必须 0。
	SourceMatchID uint64
}

func (t DSTicketTarget) validate(dsType DSType) error {
	if t.DSPodName == "" || t.DSInstanceUID == "" || t.DSInstanceEpoch == 0 {
		return errors.New("auth.DSTicketTarget: pod/uid/instance_epoch must be complete")
	}
	if t.ReleaseTrack != ReleaseTrackStable && t.ReleaseTrack != ReleaseTrackCanary {
		return fmt.Errorf("auth.DSTicketTarget: invalid release_track %q", t.ReleaseTrack)
	}
	switch dsType {
	case DSTypeHub:
		if t.HubAssignmentID == "" {
			return errors.New("auth.DSTicketTarget: hub ticket requires hub_assignment_id")
		}
		if t.MatchID != 0 || t.AllocationID != "" {
			return errors.New("auth.DSTicketTarget: hub ticket must not carry match/allocation binding")
		}
	case DSTypeBattle:
		if t.MatchID == 0 || t.AllocationID == "" {
			return errors.New("auth.DSTicketTarget: battle ticket requires match_id and allocation_id")
		}
		if t.HubAssignmentID != "" {
			return errors.New("auth.DSTicketTarget: battle ticket must not carry hub_assignment_id")
		}
		if t.SourceMatchID != 0 {
			return errors.New("auth.DSTicketTarget: battle ticket must not carry source_match_id (battle binding is match_id)")
		}
	default:
		return fmt.Errorf("auth.DSTicketTarget: invalid dsType %q", dsType)
	}
	return nil
}

// ── 签发器 ────────────────────────────────────────────────────────────────────

// DSTicketSignerConfig 是 RS256 签发器配置。与 Config(HS256 玩家面)完全独立:
// 编译期类型隔离,防止 Session 密钥 / DS 回调密钥被误接进 DSTicket 信任域。
type DSTicketSignerConfig struct {
	// Issuer / Audience:零值取 DSTicketIssuer / DSTicketAudience。
	Issuer   string
	Audience string
	// PrivateKeyPEM:RSA 私钥(PKCS#1 或 PKCS#8 PEM)。调用方以非 root UID/GID/fsGroup 10001
	// 从 mode 0440 Secret 文件读入，绝不进 ConfigMap / 命令行参数 / 明文环境变量。
	PrivateKeyPEM []byte
	// ActiveKid:期望的签发密钥指纹(RFC 7638)。必填且必须与私钥推导指纹一致——
	// 轮换切换 active_kid 时若挂错私钥文件,启动即拒,而不是签出一堆验不过的票。
	ActiveKid string
	// TTL:票据有效期,零值取 DSTicketDefaultTTL,超过 DSTicketMaxTTL 启动即拒。
	TTL time.Duration
	// NowFn 可注入(测试),默认 time.Now。
	NowFn func() time.Time
}

// DSTicketSigner 签发 dst_ver=2 RS256 票据。线程安全(无可变状态)。
type DSTicketSigner struct {
	key      *rsa.PrivateKey
	kid      string
	issuer   string
	audience string
	ttl      time.Duration
	nowFn    func() time.Time
}

// NewDSTicketSigner 构造签发器。弱密钥 / TTL 超上限 / kid 不匹配一律启动即拒。
func NewDSTicketSigner(cfg DSTicketSignerConfig) (*DSTicketSigner, error) {
	key, err := parseRSAPrivateKeyPEM(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("auth.NewDSTicketSigner: %w", err)
	}
	if bits := key.N.BitLen(); bits < DSTicketMinRSABits {
		return nil, fmt.Errorf("auth.NewDSTicketSigner: RSA key too weak (%d bits, need >=%d)", bits, DSTicketMinRSABits)
	}
	kid := RSAPublicKeyThumbprint(&key.PublicKey)
	if cfg.ActiveKid == "" {
		return nil, errors.New("auth.NewDSTicketSigner: active_kid is required (implicit key selection forbidden)")
	}
	if cfg.ActiveKid != kid {
		return nil, fmt.Errorf("auth.NewDSTicketSigner: active_kid %q does not match private key thumbprint %q (wrong key file mounted?)", cfg.ActiveKid, kid)
	}
	ttl := cfg.TTL
	if ttl == 0 {
		ttl = DSTicketDefaultTTL
	}
	if ttl < 0 || ttl > DSTicketMaxTTL {
		return nil, fmt.Errorf("auth.NewDSTicketSigner: ttl %v out of range (0, %v] — B1 纯本地验票要求短时票", ttl, DSTicketMaxTTL)
	}
	issuer := cfg.Issuer
	if issuer == "" {
		issuer = DSTicketIssuer
	}
	audience := cfg.Audience
	if audience == "" {
		audience = DSTicketAudience
	}
	nowFn := cfg.NowFn
	if nowFn == nil {
		nowFn = time.Now
	}
	return &DSTicketSigner{key: key, kid: kid, issuer: issuer, audience: audience, ttl: ttl, nowFn: nowFn}, nil
}

// Kid 返回签发密钥指纹(RFC 7638),供启动日志与部署对账。
func (s *DSTicketSigner) Kid() string { return s.kid }

// TTL 返回票据有效期(调用方对齐 Redis 记录 TTL 时用)。
func (s *DSTicketSigner) TTL() time.Duration { return s.ttl }

// SignHubTicket 签发绑定唯一 Hub DS 实例 + 玩家归属版本的 hub 票据。
func (s *DSTicketSigner) SignHubTicket(playerID uint64, regionID, cellID, roleID uint32, jti string, target DSTicketTarget) (token string, expiresAtMs int64, err error) {
	return s.sign(playerID, DSTypeHub, regionID, cellID, roleID, jti, target)
}

// SignBattleTicket 签发绑定唯一 Battle DS 实例 + 对局/分配 ID 的 battle 票据。
func (s *DSTicketSigner) SignBattleTicket(playerID uint64, regionID, cellID uint32, jti string, target DSTicketTarget) (token string, expiresAtMs int64, err error) {
	return s.sign(playerID, DSTypeBattle, regionID, cellID, 0, jti, target)
}

func (s *DSTicketSigner) sign(playerID uint64, dsType DSType, regionID, cellID, roleID uint32, jti string, target DSTicketTarget) (string, int64, error) {
	if playerID == 0 {
		return "", 0, errors.New("auth.DSTicketSigner: playerID must be > 0")
	}
	if jti == "" {
		return "", 0, errors.New("auth.DSTicketSigner: jti must be non-empty")
	}
	if err := target.validate(dsType); err != nil {
		return "", 0, err
	}
	now := s.nowFn()
	exp := now.Add(s.ttl)
	claims := DSTicketClaimsV2{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.issuer,
			Subject:   strconv.FormatUint(playerID, 10),
			Audience:  jwt.ClaimStrings{s.audience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        jti,
		},
		DstVer:          DSTicketVersion2,
		DSType:          string(dsType),
		DSPodName:       target.DSPodName,
		DSInstanceUID:   target.DSInstanceUID,
		DSInstanceEpoch: target.DSInstanceEpoch,
		ReleaseTrack:    target.ReleaseTrack,
		RegionID:        regionID,
		CellID:          cellID,
		RoleID:          roleID,
		MatchID:         target.MatchID,
		AllocationID:    target.AllocationID,
		HubAssignmentID: target.HubAssignmentID,
		SourceMatchID:   target.SourceMatchID,
	}
	t := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	t.Header["kid"] = s.kid
	str, err := t.SignedString(s.key)
	if err != nil {
		return "", 0, fmt.Errorf("auth.DSTicketSigner: %w", err)
	}
	return str, exp.UnixMilli(), nil
}

// ── 验签器 ────────────────────────────────────────────────────────────────────

// DSTicketVerifierConfig 是 RS256 验签器配置(后端侧,如 login 在线核销/审计;
// UE DS 侧的对应实现在客户端仓库 FPandoraTicketVerifier,契约必须与本文件一致)。
type DSTicketVerifierConfig struct {
	Issuer   string
	Audience string
	// JWKS:公钥 keyset 文件原始内容(strict 解析,见 ParseDSTicketJWKS)。
	JWKS []byte
	// NowFn 可注入(测试),默认 time.Now。
	NowFn func() time.Time
}

// DSTicketVerifier 验 dst_ver=2 RS256 票据。线程安全。
type DSTicketVerifier struct {
	keys     map[string]*rsa.PublicKey
	issuer   string
	audience string
	nowFn    func() time.Time
}

// DSTicketAlgorithm 只解析 JOSE header 以选择严格 verifier。返回值只可用于分发，
// 不能作为鉴权结果；HS256/RS256 分支随后都会再次执行完整签名和 claims 校验。
// 其余算法（含 none）在分发前直接拒绝，避免 legacy/v2 迁移期算法混淆。
func DSTicketAlgorithm(token string) (string, error) {
	if token == "" {
		return "", errcode.New(errcode.ErrLoginTicketInvalid, "empty ds ticket")
	}
	parsed, _, err := jwt.NewParser().ParseUnverified(token, jwt.MapClaims{})
	if err != nil || parsed == nil {
		return "", errcode.New(errcode.ErrLoginTicketInvalid, "ds ticket header invalid")
	}
	alg, _ := parsed.Header["alg"].(string)
	if alg != jwt.SigningMethodHS256.Alg() && alg != jwt.SigningMethodRS256.Alg() {
		return "", errcode.New(errcode.ErrLoginTicketInvalid, "ds ticket alg %q unsupported", alg)
	}
	return alg, nil
}

// NewDSTicketVerifier 构造验签器。JWKS 解析失败 / 空 keyset 启动即拒(fail-closed)。
func NewDSTicketVerifier(cfg DSTicketVerifierConfig) (*DSTicketVerifier, error) {
	keys, err := ParseDSTicketJWKS(cfg.JWKS)
	if err != nil {
		return nil, fmt.Errorf("auth.NewDSTicketVerifier: %w", err)
	}
	issuer := cfg.Issuer
	if issuer == "" {
		issuer = DSTicketIssuer
	}
	audience := cfg.Audience
	if audience == "" {
		audience = DSTicketAudience
	}
	nowFn := cfg.NowFn
	if nowFn == nil {
		nowFn = time.Now
	}
	return &DSTicketVerifier{keys: keys, issuer: issuer, audience: audience, nowFn: nowFn}, nil
}

// Verify 验签 + 结构校验。校验项与 UE 侧 FPandoraTicketVerifier 的 RS256 路径一一对应:
//   - alg 固定 RS256(WithValidMethods,天然拒 none / HS256 混淆);
//   - kid 必须存在且在 keyset 内(kid 只是选键提示,签名仍必须验过);
//   - iss / aud / exp 必须匹配且存在;iat 必须存在且 exp-iat ≤ DSTicketMaxTTL;
//   - dst_ver == 2;ds_type ∈ {hub,battle};实例绑定按类型完整。
func (v *DSTicketVerifier) Verify(token string) (*DSTicketClaimsV2, error) {
	if token == "" {
		return nil, errcode.New(errcode.ErrLoginTicketInvalid, "empty ds ticket")
	}
	var claims DSTicketClaimsV2
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithTimeFunc(v.nowFn),
	)
	_, err := parser.ParseWithClaims(token, &claims, func(t *jwt.Token) (interface{}, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("ds ticket v2 requires kid header")
		}
		key, ok := v.keys[kid]
		if !ok {
			return nil, fmt.Errorf("unknown kid %q (keyset stale or foreign ticket)", kid)
		}
		return key, nil
	})
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, errcode.New(errcode.ErrLoginTicketExpired, "ds ticket expired")
		}
		return nil, errcode.New(errcode.ErrLoginTicketInvalid, "ds ticket verify: %v", err)
	}
	if claims.DstVer != DSTicketVersion2 {
		return nil, errcode.New(errcode.ErrLoginTicketInvalid, "ds ticket dst_ver %d unsupported (want %d)", claims.DstVer, DSTicketVersion2)
	}
	if claims.PlayerID() == 0 {
		return nil, errcode.New(errcode.ErrLoginTicketInvalid, "ds ticket sub not a valid player_id")
	}
	if claims.ID == "" {
		return nil, errcode.New(errcode.ErrLoginTicketInvalid, "ds ticket missing jti")
	}
	if claims.IssuedAt == nil || claims.ExpiresAt == nil {
		return nil, errcode.New(errcode.ErrLoginTicketInvalid, "ds ticket missing iat/exp")
	}
	if !claims.ExpiresAt.After(claims.IssuedAt.Time) {
		return nil, errcode.New(errcode.ErrLoginTicketInvalid, "ds ticket exp must be after iat")
	}
	if claims.NotBefore != nil && claims.NotBefore.After(claims.ExpiresAt.Time) {
		return nil, errcode.New(errcode.ErrLoginTicketInvalid, "ds ticket nbf must not be after exp")
	}
	if claims.ExpiresAt.Sub(claims.IssuedAt.Time) > DSTicketMaxTTL {
		return nil, errcode.New(errcode.ErrLoginTicketInvalid, "ds ticket lifetime exceeds max ttl %v (long-lived capability forbidden under B1)", DSTicketMaxTTL)
	}
	target := DSTicketTarget{
		DSPodName:       claims.DSPodName,
		DSInstanceUID:   claims.DSInstanceUID,
		DSInstanceEpoch: claims.DSInstanceEpoch,
		ReleaseTrack:    claims.ReleaseTrack,
		HubAssignmentID: claims.HubAssignmentID,
		MatchID:         claims.MatchID,
		AllocationID:    claims.AllocationID,
		SourceMatchID:   claims.SourceMatchID,
	}
	if err := target.validate(DSType(claims.DSType)); err != nil {
		return nil, errcode.New(errcode.ErrLoginTicketInvalid, "ds ticket binding invalid: %v", err)
	}
	return &claims, nil
}

// ── 严格 JWKS ────────────────────────────────────────────────────────────────

// dsTicketJWK 是 JWKS 内单把公钥(只接受 RSA 公开成员)。
type dsTicketJWK struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
	// 以下私钥成员只用于检测输入中是否“出现”过；RawMessage 能区分字段缺失与
	// 空字符串/null。omitempty 保证 MarshalDSTicketJWKS 的公开输出绝不携带这些成员。
	D   json.RawMessage `json:"d,omitempty"`
	P   json.RawMessage `json:"p,omitempty"`
	Q   json.RawMessage `json:"q,omitempty"`
	DP  json.RawMessage `json:"dp,omitempty"`
	DQ  json.RawMessage `json:"dq,omitempty"`
	QI  json.RawMessage `json:"qi,omitempty"`
	Oth json.RawMessage `json:"oth,omitempty"`
	K   json.RawMessage `json:"k,omitempty"` // oct 对称密钥成员
}

// dsTicketJWKS 是 keyset 文件结构。revision 与 active_kid 都是必填发布元数据；
// active_kid 只声明签发端当前使用哪把 key，每张票仍必须携带精确 kid。
type dsTicketJWKS struct {
	Revision  int           `json:"revision"`
	ActiveKid string        `json:"active_kid"`
	Keys      []dsTicketJWK `json:"keys"`
}

// ParseDSTicketJWKS 严格解析公钥 keyset。任何一把 key 不合规即整文件拒绝(fail-closed):
//   - kty 必须 "RSA"(kty=oct = 把对称密钥当公钥投递,直接判事故);
//   - use / alg 均必填且必须分别为 "sig" / "RS256";
//   - kid 必填、全 set 内唯一、且必须等于该公钥的 RFC 7638 指纹(防 kid 张冠李戴);
//   - 私钥成员(d/p/q/dp/dq/qi/oth/k)只要出现即拒绝（包括空字符串/null）;
//   - n ≥ 2048 bit;e 为常见公开指数;
//   - keyset 非空、≤ dsTicketJWKSMaxKeys、文件 ≤ dsTicketJWKSMaxBytes。
func ParseDSTicketJWKS(data []byte) (map[string]*rsa.PublicKey, error) {
	if len(data) == 0 {
		return nil, errors.New("jwks empty")
	}
	if len(data) > dsTicketJWKSMaxBytes {
		return nil, fmt.Errorf("jwks too large (%d bytes > %d)", len(data), dsTicketJWKSMaxBytes)
	}
	var set dsTicketJWKS
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&set); err != nil {
		return nil, fmt.Errorf("jwks parse: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("jwks parse: trailing JSON value")
		}
		return nil, fmt.Errorf("jwks parse trailing data: %w", err)
	}
	if set.Revision < 1 {
		return nil, errors.New("jwks revision must be >= 1")
	}
	if set.ActiveKid == "" {
		return nil, errors.New("jwks active_kid required")
	}
	if len(set.Keys) == 0 {
		return nil, errors.New("jwks has no keys")
	}
	if len(set.Keys) > dsTicketJWKSMaxKeys {
		return nil, fmt.Errorf("jwks has %d keys (max %d)", len(set.Keys), dsTicketJWKSMaxKeys)
	}
	out := make(map[string]*rsa.PublicKey, len(set.Keys))
	for i, k := range set.Keys {
		if k.Kty != "RSA" {
			return nil, fmt.Errorf("jwks key[%d]: kty %q rejected (only RSA public keys allowed; oct = 对称密钥投递事故)", i, k.Kty)
		}
		if k.D != nil || k.P != nil || k.Q != nil || k.DP != nil || k.DQ != nil || k.QI != nil || k.Oth != nil || k.K != nil {
			return nil, fmt.Errorf("jwks key[%d]: private key material present — refusing entire keyset", i)
		}
		if k.Use != "sig" {
			return nil, fmt.Errorf("jwks key[%d]: use %q rejected (want sig)", i, k.Use)
		}
		if k.Alg != "RS256" {
			return nil, fmt.Errorf("jwks key[%d]: alg %q rejected (want RS256)", i, k.Alg)
		}
		if k.Kid == "" {
			return nil, fmt.Errorf("jwks key[%d]: kid required", i)
		}
		if _, dup := out[k.Kid]; dup {
			return nil, fmt.Errorf("jwks key[%d]: duplicate kid %q", i, k.Kid)
		}
		pub, err := rsaPublicKeyFromJWK(k.N, k.E)
		if err != nil {
			return nil, fmt.Errorf("jwks key[%d]: %w", i, err)
		}
		if pub.N.BitLen() < DSTicketMinRSABits {
			return nil, fmt.Errorf("jwks key[%d]: RSA modulus %d bits too weak (need >=%d)", i, pub.N.BitLen(), DSTicketMinRSABits)
		}
		if got := RSAPublicKeyThumbprint(pub); got != k.Kid {
			return nil, fmt.Errorf("jwks key[%d]: kid %q does not match RFC 7638 thumbprint %q", i, k.Kid, got)
		}
		out[k.Kid] = pub
	}
	if _, ok := out[set.ActiveKid]; !ok {
		return nil, fmt.Errorf("jwks active_kid %q is not present in keys", set.ActiveKid)
	}
	return out, nil
}

// DSTicketJWKSMetadata 读取并严格校验发布元数据。它复用完整 JWKS 解析，避免
// 部署对账接受 verifier 实际会拒绝的半成品 keyset。
func DSTicketJWKSMetadata(data []byte) (revision int, activeKid string, err error) {
	if _, err := ParseDSTicketJWKS(data); err != nil {
		return 0, "", err
	}
	var set dsTicketJWKS
	if err := json.Unmarshal(data, &set); err != nil {
		return 0, "", fmt.Errorf("jwks parse: %w", err)
	}
	return set.Revision, set.ActiveKid, nil
}

// DSTicketJWKSRevision 返回严格 keyset 的 revision。
func DSTicketJWKSRevision(data []byte) (int, error) {
	revision, _, err := DSTicketJWKSMetadata(data)
	return revision, err
}

// MarshalDSTicketJWKS 把一组 RSA 公钥编码成严格 JWKS(kid=RFC 7638 指纹)。
// 供 tools/dsticketkeys 与测试生成 keyset;输出保证能被 ParseDSTicketJWKS 接受。
func MarshalDSTicketJWKS(revision int, activeKid string, pubs ...*rsa.PublicKey) ([]byte, error) {
	if revision < 1 {
		return nil, errors.New("auth.MarshalDSTicketJWKS: revision must be >= 1")
	}
	if activeKid == "" {
		return nil, errors.New("auth.MarshalDSTicketJWKS: active_kid is required")
	}
	if len(pubs) == 0 {
		return nil, errors.New("auth.MarshalDSTicketJWKS: at least one key required")
	}
	set := dsTicketJWKS{Revision: revision, ActiveKid: activeKid, Keys: make([]dsTicketJWK, 0, len(pubs))}
	for _, pub := range pubs {
		if pub == nil || pub.N == nil {
			return nil, errors.New("auth.MarshalDSTicketJWKS: nil key")
		}
		set.Keys = append(set.Keys, dsTicketJWK{
			Kty: "RSA",
			Use: "sig",
			Alg: "RS256",
			Kid: RSAPublicKeyThumbprint(pub),
			N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		})
	}
	// kid 排序,输出确定性(同一组 key 恒得同一文件,便于 ConfigMap 内容对账)。
	sort.Slice(set.Keys, func(i, j int) bool { return set.Keys[i].Kid < set.Keys[j].Kid })
	foundActive := false
	for _, key := range set.Keys {
		if key.Kid == activeKid {
			foundActive = true
			break
		}
	}
	if !foundActive {
		return nil, fmt.Errorf("auth.MarshalDSTicketJWKS: active_kid %q is not present in keys", activeKid)
	}
	return json.MarshalIndent(set, "", "  ")
}

// RSAPublicKeyThumbprint 计算 RFC 7638 JWK 指纹(SHA-256,base64url 无填充)。
// 成员按字典序 e,kty,n 构造 canonical JSON 后哈希——与语言无关的稳定 kid。
func RSAPublicKeyThumbprint(pub *rsa.PublicKey) string {
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	canonical := fmt.Sprintf(`{"e":"%s","kty":"RSA","n":"%s"}`, e, n)
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// GenerateDSTicketKeyPair 生成 RSA-2048 密钥对并返回(私钥 PKCS#8 PEM, 公钥, kid)。
// 只供 tools/dsticketkeys(首次迁移的 K1 / 罕见轮换的 K2)与测试使用;
// 服务运行期绝不调用(密钥生命周期与发布解耦)。
func GenerateDSTicketKeyPair() (privatePEM []byte, pub *rsa.PublicKey, kid string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, DSTicketMinRSABits)
	if err != nil {
		return nil, nil, "", fmt.Errorf("auth.GenerateDSTicketKeyPair: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, "", fmt.Errorf("auth.GenerateDSTicketKeyPair: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	return pemBytes, &key.PublicKey, RSAPublicKeyThumbprint(&key.PublicKey), nil
}

func parseRSAPrivateKeyPEM(pemBytes []byte) (*rsa.PrivateKey, error) {
	if len(pemBytes) == 0 {
		return nil, errors.New("private key PEM empty")
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("private key PEM decode failed")
	}
	switch block.Type {
	case "PRIVATE KEY": // PKCS#8
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS#8: %w", err)
		}
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("PKCS#8 key is not RSA")
		}
		return rsaKey, nil
	case "RSA PRIVATE KEY": // PKCS#1
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS#1: %w", err)
		}
		return key, nil
	default:
		return nil, fmt.Errorf("unsupported PEM block type %q", block.Type)
	}
}

func rsaPublicKeyFromJWK(nB64, eB64 string) (*rsa.PublicKey, error) {
	if nB64 == "" || eB64 == "" {
		return nil, errors.New("n/e required")
	}
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, fmt.Errorf("n decode: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, fmt.Errorf("e decode: %w", err)
	}
	if len(nBytes) == 0 || nBytes[0] == 0 {
		return nil, errors.New("n must be minimal big-endian without leading zero")
	}
	if len(eBytes) == 0 || len(eBytes) > 4 {
		return nil, errors.New("e out of range")
	}
	e := new(big.Int).SetBytes(eBytes)
	// 只接受常见公开指数(奇数、3 ≤ e ≤ 2^31-1);离谱指数一律拒。
	if !e.IsInt64() || e.Int64() < 3 || e.Int64() > int64(1<<31-1) || e.Int64()%2 == 0 {
		return nil, errors.New("e not an acceptable public exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: int(e.Int64())}, nil
}
