package auth

import (
	"strconv"
	"strings"
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
)

func newTestSigner(t *testing.T, now time.Time) (*Signer, *Verifier) {
	t.Helper()
	cfg := Config{
		Secret:      []byte("pandora-dev-shared-secret-32bytes!!"),
		SessionTTL:  time.Hour,
		DSTicketTTL: 5 * time.Minute,
		NowFn:       func() time.Time { return now },
	}
	s, err := NewSigner(cfg)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	v, err := NewVerifier(cfg)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return s, v
}

// signRawDSTicketForTest 直接用底层 jwt 库签 token,绕过 Signer.SignDSTicket 的 pre-check,
// 用于构造 "dsType=battle 但 match_id=空" 这种恶意载荷,验证 VerifyDSTicket 防御。
func signRawDSTicketForTest(t *testing.T, s *Signer, playerID uint64, dsType string, matchID uint64) string {
	t.Helper()
	now := s.cfg.NowFn()
	claims := DSTicketClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.cfg.Issuer,
			Subject:   strconv.FormatUint(playerID, 10),
			Audience:  jwt.ClaimStrings{s.cfg.Audience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.cfg.DSTicketTTL)),
			ID:        "jti-raw",
		},
		DSType:  dsType,
		MatchID: matchID,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	str, err := tok.SignedString(s.cfg.Secret)
	if err != nil {
		t.Fatalf("signRawDSTicketForTest: %v", err)
	}
	return str
}

func TestSignAndVerifySession(t *testing.T) {
	now := time.Unix(1_780_000_000, 0).UTC()
	s, v := newTestSigner(t, now)

	tok, expMs, err := s.SignSession(12345, "jti-abc")
	if err != nil {
		t.Fatalf("SignSession: %v", err)
	}
	if tok == "" || strings.Count(tok, ".") != 2 {
		t.Fatalf("expected jwt with 2 dots, got %q", tok)
	}
	if expMs != now.Add(time.Hour).UnixMilli() {
		t.Fatalf("expiry: got %d", expMs)
	}

	c, err := v.VerifySession(tok)
	if err != nil {
		t.Fatalf("VerifySession: %v", err)
	}
	if c.PlayerID() != 12345 {
		t.Fatalf("PlayerID: %d", c.PlayerID())
	}
	if c.ID != "jti-abc" {
		t.Fatalf("jti: %q", c.ID)
	}
	if c.Issuer != "pandora-login" {
		t.Fatalf("iss: %q", c.Issuer)
	}
}

func TestSessionExpired(t *testing.T) {
	now := time.Unix(1_780_000_000, 0).UTC()
	s, _ := newTestSigner(t, now)

	tok, _, err := s.SignSession(1, "jti")
	if err != nil {
		t.Fatalf("SignSession: %v", err)
	}

	// 用一个 2 小时后的 verifier 校验同一个 token,应 expired。
	cfgLater := Config{
		Secret: []byte("pandora-dev-shared-secret-32bytes!!"),
		NowFn:  func() time.Time { return now.Add(2 * time.Hour) },
	}
	v, err := NewVerifier(cfgLater)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if _, err := v.VerifySession(tok); err == nil {
		t.Fatal("expected expired error, got nil")
	}
}

func TestSignAndVerifyDSTicket(t *testing.T) {
	now := time.Unix(1_780_000_000, 0).UTC()
	s, v := newTestSigner(t, now)

	tok, _, err := s.SignDSTicket(7777, DSTypeBattle, 9001, "jti-1")
	if err != nil {
		t.Fatalf("SignDSTicket: %v", err)
	}
	c, err := v.VerifyDSTicket(tok)
	if err != nil {
		t.Fatalf("VerifyDSTicket: %v", err)
	}
	if c.PlayerID() != 7777 {
		t.Fatalf("PlayerID: %d", c.PlayerID())
	}
	if c.DSType != "battle" || c.MatchID != 9001 {
		t.Fatalf("ds_type=%q match_id=%d", c.DSType, c.MatchID)
	}
}

func TestSignDSTicketRequiresMatchIDForBattle(t *testing.T) {
	now := time.Unix(1_780_000_000, 0).UTC()
	s, _ := newTestSigner(t, now)

	if _, _, err := s.SignDSTicket(1, DSTypeBattle, 0, "jti"); err == nil {
		t.Fatal("expected error when battle ticket missing match_id")
	}
	if _, _, err := s.SignDSTicket(1, DSTypeHub, 0, "jti"); err != nil {
		t.Fatalf("hub ticket without match_id should be OK: %v", err)
	}
}

func TestRejectWrongIssuer(t *testing.T) {
	now := time.Unix(1_780_000_000, 0).UTC()
	wrongSigner, err := NewSigner(Config{
		Issuer:     "evil-issuer",
		Secret:     []byte("pandora-dev-shared-secret-32bytes!!"),
		SessionTTL: time.Hour,
		NowFn:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	_, v := newTestSigner(t, now)

	tok, _, err := wrongSigner.SignSession(1, "jti")
	if err != nil {
		t.Fatalf("SignSession: %v", err)
	}
	if _, err := v.VerifySession(tok); err == nil {
		t.Fatal("expected reject wrong issuer")
	}
}

func TestJWKSInlineHS256(t *testing.T) {
	js := JWKSInlineHS256([]byte("hello"), "")
	if !strings.Contains(js, `"kty":"oct"`) {
		t.Fatalf("missing kty: %s", js)
	}
	if !strings.Contains(js, `"alg":"HS256"`) {
		t.Fatalf("missing alg: %s", js)
	}
	if !strings.Contains(js, `"kid":"pandora-dev"`) {
		t.Fatalf("missing default kid: %s", js)
	}
	// base64url(hello) == "aGVsbG8"
	if !strings.Contains(js, `"k":"aGVsbG8"`) {
		t.Fatalf("missing k: %s", js)
	}
}

func TestValidateRejectsShortSecret(t *testing.T) {
	if _, err := NewSigner(Config{Secret: []byte("short")}); err == nil {
		t.Fatal("expected short secret rejected")
	}
}

// TestValidateRejects16And31ByteSecrets 守住 HS256 推荐 secret \u2265 32 byte 的红线
// (RFC 7518 \u00a73.2:secret should be >= hash output size = 256bit = 32 byte)。
// 16 字节(过去的最低阈值)+ 31 字节(边界临界值)都应被拒。
func TestValidateRejects16And31ByteSecrets(t *testing.T) {
	cases := []struct {
		name string
		size int
	}{
		{"16-byte (old minimum, now rejected)", 16},
		{"31-byte (just below 32)", 31},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			secret := make([]byte, c.size)
			for i := range secret {
				secret[i] = 'x'
			}
			if _, err := NewSigner(Config{Secret: secret}); err == nil {
				t.Fatalf("expected %d-byte secret rejected", c.size)
			}
			if _, err := NewVerifier(Config{Secret: secret}); err == nil {
				t.Fatalf("expected %d-byte secret rejected for verifier", c.size)
			}
		})
	}
}

// TestValidateAccepts32ByteSecret 32 字节边界值应放行。
func TestValidateAccepts32ByteSecret(t *testing.T) {
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = 'y'
	}
	if _, err := NewSigner(Config{Secret: secret}); err != nil {
		t.Fatalf("32-byte secret should be accepted: %v", err)
	}
}

// TestVerifyDSTicketRejectsBattleWithoutMatchID 防御:
// 即便攻击者用同一 secret 伪造一个 dsType=battle 但 match_id 缺失的 token,
// VerifyDSTicket 也应该直接拒。SignDSTicket 路径已校验,这里补 verify 侧对称防御。
func TestVerifyDSTicketRejectsBattleWithoutMatchID(t *testing.T) {
	now := time.Unix(1_780_000_000, 0).UTC()
	s, v := newTestSigner(t, now)

	// 直接构造一个绕过 SignDSTicket 校验的"恶意"battle token:
	// 用底层 jwt 库手工签,确保 ds_type=battle 但 match_id 为空。
	tok := signRawDSTicketForTest(t, s, 42, "battle", 0)
	if _, err := v.VerifyDSTicket(tok); err == nil {
		t.Fatal("expected reject battle ticket without match_id")
	}

	// 对照:hub ticket 不需要 match_id,应放行
	hubTok, _, err := s.SignDSTicket(42, DSTypeHub, 0, "jti-hub")
	if err != nil {
		t.Fatalf("hub SignDSTicket: %v", err)
	}
	if _, err := v.VerifyDSTicket(hubTok); err != nil {
		t.Fatalf("hub ticket should verify: %v", err)
	}

	// 对照:battle + match_id 应放行
	battleTok, _, err := s.SignDSTicket(42, DSTypeBattle, 1001, "jti-b")
	if err != nil {
		t.Fatalf("battle SignDSTicket: %v", err)
	}
	if _, err := v.VerifyDSTicket(battleTok); err != nil {
		t.Fatalf("battle ticket with match_id should verify: %v", err)
	}
}

func TestVerifySessionRejectsNegativeSub(t *testing.T) {
	now := time.Unix(1_780_000_000, 0).UTC()
	s, v := newTestSigner(t, now)

	claims := SessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.cfg.Issuer,
			Subject:   "-1",
			Audience:  jwt.ClaimStrings{s.cfg.Audience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.cfg.SessionTTL)),
			ID:        "jti-negative",
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	str, err := tok.SignedString(s.cfg.Secret)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	if _, err := v.VerifySession(str); err == nil {
		t.Fatal("expected negative sub rejected")
	}
}

// TestVerifySessionRejectsOverflowSub 验证 sub 超出 uint64 上限时被拒绝。
func TestVerifySessionRejectsOverflowSub(t *testing.T) {
	now := time.Unix(1_780_000_000, 0).UTC()
	s, v := newTestSigner(t, now)

	// "99999999999999999999" 超过 uint64 最大值(18446744073709551615),ParseUint 报错
	claims := SessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.cfg.Issuer,
			Subject:   "99999999999999999999",
			Audience:  jwt.ClaimStrings{s.cfg.Audience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.cfg.SessionTTL)),
			ID:        "jti-overflow",
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	str, err := tok.SignedString(s.cfg.Secret)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	if _, err := v.VerifySession(str); err == nil {
		t.Fatal("expected overflow sub rejected")
	}
}

// TestVerifySessionRejectsNonNumericSub 验证 sub 为非数字字符串时被拒绝。
func TestVerifySessionRejectsNonNumericSub(t *testing.T) {
	now := time.Unix(1_780_000_000, 0).UTC()
	s, v := newTestSigner(t, now)

	claims := SessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.cfg.Issuer,
			Subject:   "evil",
			Audience:  jwt.ClaimStrings{s.cfg.Audience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.cfg.SessionTTL)),
			ID:        "jti-non-numeric",
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	str, err := tok.SignedString(s.cfg.Secret)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	if _, err := v.VerifySession(str); err == nil {
		t.Fatal("expected non-numeric sub rejected")
	}
}
