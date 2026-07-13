// Package auth — 信任域拆分(方案 B,decision-revisit-player-jwt-key-rotation.md §7)。
//
// 历史上 SessionToken / DSTicket / DS 回调令牌共用一个 *Signer / *Verifier(同一 Config、
// 同一 HS256 密钥面),密钥/方法混用只能靠约定防。本文件引入按信任域收窄方法集的
// 独立类型:每个域只暴露自己的 Sign/Verify 方法,把「拿玩家密钥签 DS 回调」这类
// 串域错误从运行时约定升级为编译错误。
//
// 域清单(密钥/issuer/audience 互不复用):
//   - SessionSigner / SessionVerifier:玩家会话(HS256,pandora-login → pandora-client)。
//   - DSTicketSigner / DSTicketVerifier:玩家入场票 v2(RS256,pandora-dsticket →
//     pandora-game-ds,见 dsticket.go;DS 只持公钥 JWKS)。
//   - DSCallbackSigner / DSCallbackVerifier:DS→后端回调(HS256,pandora-ds-control →
//     pandora-ds;DS 持有令牌本身属正常,签名密钥仍只在控制面)。
//
// 迁移策略:旧 *Signer / *Verifier 保留(大量既有接线与测试仍在使用),新接线一律
// 用域类型;等全部调用方迁完后再收缩旧类型的方法集。
package auth

import "time"

// ── Session 域 ───────────────────────────────────────────────────────────────

// SessionSigner 只签玩家会话令牌。
type SessionSigner struct {
	s *Signer
}

// NewSessionSigner 用玩家面 HS256 配置构造会话签发器。
func NewSessionSigner(cfg Config) (*SessionSigner, error) {
	s, err := NewSigner(cfg)
	if err != nil {
		return nil, err
	}
	return &SessionSigner{s: s}, nil
}

// SignSession 同 Signer.SignSession。
func (s *SessionSigner) SignSession(playerID uint64, jti string) (string, int64, error) {
	return s.s.SignSession(playerID, jti)
}

// SessionTTL 同 Signer.SessionTTL。
func (s *SessionSigner) SessionTTL() (d time.Duration) { return s.s.SessionTTL() }

// SessionVerifier 只验玩家会话令牌。
type SessionVerifier struct {
	v *Verifier
}

// NewSessionVerifier 用玩家面 HS256 配置构造会话验签器。
func NewSessionVerifier(cfg Config) (*SessionVerifier, error) {
	v, err := NewVerifier(cfg)
	if err != nil {
		return nil, err
	}
	return &SessionVerifier{v: v}, nil
}

// VerifySession 同 Verifier.VerifySession。
func (v *SessionVerifier) VerifySession(token string) (*SessionClaims, error) {
	return v.v.VerifySession(token)
}

// ── DS 回调域 ────────────────────────────────────────────────────────────────

// DSCallbackSigner 只签 DS→后端回调令牌 / Model B 凭据。
type DSCallbackSigner struct {
	s *Signer
}

// NewDSCallbackSigner 用 DS 回调面 HS256 配置(ds_auth,与玩家面密钥集合必须不相交)构造。
func NewDSCallbackSigner(cfg Config) (*DSCallbackSigner, error) {
	s, err := NewSigner(cfg)
	if err != nil {
		return nil, err
	}
	return &DSCallbackSigner{s: s}, nil
}

// SignDSCallback 同 Signer.SignDSCallback。
func (s *DSCallbackSigner) SignDSCallback(dsType DSType, pod string, matchID uint64, ttl time.Duration) (string, int64, error) {
	return s.s.SignDSCallback(dsType, pod, matchID, ttl)
}

// SignDSCallbackWithGen 同 Signer.SignDSCallbackWithGen。
func (s *DSCallbackSigner) SignDSCallbackWithGen(dsType DSType, pod string, matchID, gen uint64, ttl time.Duration) (string, int64, error) {
	return s.s.SignDSCallbackWithGen(dsType, pod, matchID, gen, ttl)
}

// SignHubCredential 同 Signer.SignHubCredential。
func (s *DSCallbackSigner) SignHubCredential(pod, instanceUID string, epoch uint32, gen uint64, jti string, ttl time.Duration) (HubCredentialResult, error) {
	return s.s.SignHubCredential(pod, instanceUID, epoch, gen, jti, ttl)
}

// SignBattleCredential 同 Signer.SignBattleCredential。
func (s *DSCallbackSigner) SignBattleCredential(matchID uint64, pod, instanceUID string, epoch uint32, gen uint64, jti string, ttl time.Duration) (HubCredentialResult, error) {
	return s.s.SignBattleCredential(matchID, pod, instanceUID, epoch, gen, jti, ttl)
}

// DSCallbackVerifier 只验 DS→后端回调令牌。
type DSCallbackVerifier struct {
	v *Verifier
}

// NewDSCallbackVerifier 用 DS 回调面 HS256 配置构造。
func NewDSCallbackVerifier(cfg Config) (*DSCallbackVerifier, error) {
	v, err := NewVerifier(cfg)
	if err != nil {
		return nil, err
	}
	return &DSCallbackVerifier{v: v}, nil
}

// VerifyDSCallback 同 Verifier.VerifyDSCallback。
func (v *DSCallbackVerifier) VerifyDSCallback(token string) (*DSCallbackClaims, error) {
	return v.v.VerifyDSCallback(token)
}
