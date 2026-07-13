// jwt_dscallback_test.go — DS 回调服务令牌(SignDSCallback / VerifyDSCallback)单测。
package auth

import (
	"strings"
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
)

// newDSCallbackTestPair 用 DS 回调专用 iss/aud 构造 Signer/Verifier。
func newDSCallbackTestPair(t *testing.T, now time.Time) (*Signer, *Verifier) {
	t.Helper()
	cfg := Config{
		Issuer:   DSCallbackIssuer,
		Audience: DSCallbackAudience,
		Secret:   []byte("pandora-dev-shared-secret-32bytes!!"),
		NowFn:    func() time.Time { return now },
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

func TestSignAndVerifyDSCallbackBattle(t *testing.T) {
	now := time.Unix(1_780_000_000, 0).UTC()
	s, v := newDSCallbackTestPair(t, now)

	tok, expMs, err := s.SignDSCallback(DSTypeBattle, "", 9001, 4*time.Hour)
	if err != nil {
		t.Fatalf("SignDSCallback battle: %v", err)
	}
	if strings.Count(tok, ".") != 2 {
		t.Fatalf("expected jwt, got %q", tok)
	}
	if expMs != now.Add(4*time.Hour).UnixMilli() {
		t.Fatalf("expiry: got %d", expMs)
	}
	claims, err := v.VerifyDSCallback(tok)
	if err != nil {
		t.Fatalf("VerifyDSCallback: %v", err)
	}
	if claims.DSType != string(DSTypeBattle) || claims.MatchID != 9001 || claims.Pod() != "" {
		t.Fatalf("claims mismatch: %+v", claims)
	}
}

// 审核 P1(18):Hub 令牌代际(ds_gen)必须能签入、验签后原样取回(round-trip),
// 且旧路径 SignDSCallback 的 gen 恒 0(legacy),供 enforce 代际门区分 legacy。
func TestSignAndVerifyDSCallbackHubGenRoundTrip(t *testing.T) {
	now := time.Unix(1_780_000_000, 0).UTC()
	s, v := newDSCallbackTestPair(t, now)

	// 带代际签发(Redis INCR 单调值 12345)。
	tok, _, err := s.SignDSCallbackWithGen(DSTypeHub, "pandora-hub-abc12", 0, 12345, 24*time.Hour)
	if err != nil {
		t.Fatalf("SignDSCallbackWithGen: %v", err)
	}
	claims, err := v.VerifyDSCallback(tok)
	if err != nil {
		t.Fatalf("VerifyDSCallback: %v", err)
	}
	if claims.Gen() != 12345 {
		t.Fatalf("gen round-trip: got %d want 12345", claims.Gen())
	}
	if claims.Pod() != "pandora-hub-abc12" {
		t.Fatalf("pod mismatch: %+v", claims)
	}

	// legacy 路径:SignDSCallback 不带 gen → 验签后 Gen()==0(enforce 代际门据此判 stale)。
	legacyTok, _, err := s.SignDSCallback(DSTypeHub, "pandora-hub-legacy", 0, 24*time.Hour)
	if err != nil {
		t.Fatalf("SignDSCallback legacy: %v", err)
	}
	legacyClaims, err := v.VerifyDSCallback(legacyTok)
	if err != nil {
		t.Fatalf("VerifyDSCallback legacy: %v", err)
	}
	if legacyClaims.Gen() != 0 {
		t.Fatalf("legacy gen must be 0, got %d", legacyClaims.Gen())
	}
}

func TestSignAndVerifyDSCallbackHub(t *testing.T) {
	now := time.Unix(1_780_000_000, 0).UTC()
	s, v := newDSCallbackTestPair(t, now)

	tok, _, err := s.SignDSCallback(DSTypeHub, "pandora-hub-abc12", 0, 24*time.Hour)
	if err != nil {
		t.Fatalf("SignDSCallback hub: %v", err)
	}
	claims, err := v.VerifyDSCallback(tok)
	if err != nil {
		t.Fatalf("VerifyDSCallback: %v", err)
	}
	if claims.DSType != string(DSTypeHub) || claims.Pod() != "pandora-hub-abc12" || claims.MatchID != 0 {
		t.Fatalf("claims mismatch: %+v", claims)
	}
}

func TestSignHubCredentialReturnsExactJWTExpiry(t *testing.T) {
	now := time.Unix(1_780_000_000, 123_000_000).UTC()
	s, v := newDSCallbackTestPair(t, now)
	result, err := s.SignHubCredential("pandora-hub-abc12", "uid-A", 3, 17, "credential-jti", 24*time.Hour)
	if err != nil {
		t.Fatalf("SignHubCredential: %v", err)
	}
	claims, err := v.VerifyDSCallback(result.Token)
	if err != nil {
		t.Fatalf("VerifyDSCallback: %v", err)
	}
	if claims.ExpiresAt == nil {
		t.Fatal("credential missing exp claim")
	}
	if result.ExpMs != claims.ExpiresAt.UnixMilli() {
		t.Fatalf("stored exp must equal JWT claim exactly: result=%d claim=%d", result.ExpMs, claims.ExpiresAt.UnixMilli())
	}
	if result.ExpMs == now.Add(24*time.Hour).UnixMilli() {
		t.Fatal("test clock must exercise JWT NumericDate precision truncation")
	}
}

func TestSignHubCredentialRejectsZeroProtocolEpoch(t *testing.T) {
	s, _ := newDSCallbackTestPair(t, time.Unix(1_780_000_000, 0).UTC())
	if _, err := s.SignHubCredential("pandora-hub-abc12", "uid-A", 0, 17, "credential-jti", time.Hour); err == nil {
		t.Fatal("zero protocol epoch must be rejected")
	}
}

func TestSignBattleCredentialRoundTrip(t *testing.T) {
	now := time.Unix(1_780_000_000, 321_000_000).UTC()
	s, v := newDSCallbackTestPair(t, now)
	result, err := s.SignBattleCredential(9001, "battle-pod-1", "uid-b", 4, 23, "battle-jti", 4*time.Hour)
	if err != nil {
		t.Fatalf("SignBattleCredential: %v", err)
	}
	claims, err := v.VerifyDSCallback(result.Token)
	if err != nil {
		t.Fatalf("VerifyDSCallback: %v", err)
	}
	if claims.DSType != string(DSTypeBattle) || claims.MatchID != 9001 || claims.Pod() != "battle-pod-1" ||
		claims.UID() != "uid-b" || claims.Epoch() != 4 || claims.Gen() != 23 || claims.JTI() != "battle-jti" ||
		claims.Kid() != result.Kid || claims.WriterEpoch() != DSAuthWriterEpochV2 ||
		claims.ExpiresAt == nil || claims.ExpiresAt.UnixMilli() != result.ExpMs {
		t.Fatalf("battle credential claims mismatch: %+v result=%+v", claims, result)
	}
}

func TestSignDSCallbackRejectsBadInput(t *testing.T) {
	now := time.Unix(1_780_000_000, 0).UTC()
	s, _ := newDSCallbackTestPair(t, now)

	cases := []struct {
		name    string
		dsType  DSType
		pod     string
		matchID uint64
		ttl     time.Duration
	}{
		{"battle 缺 matchID", DSTypeBattle, "", 0, time.Hour},
		{"hub 缺 pod", DSTypeHub, "", 0, time.Hour},
		{"hub 不该带 matchID", DSTypeHub, "pod-1", 7, time.Hour},
		{"非法 dsType", DSType("evil"), "pod-1", 7, time.Hour},
		{"ttl<=0", DSTypeBattle, "", 7, 0},
	}
	for _, c := range cases {
		if _, _, err := s.SignDSCallback(c.dsType, c.pod, c.matchID, c.ttl); err == nil {
			t.Fatalf("%s: expected error", c.name)
		}
	}
}

func TestVerifyDSCallbackRejectsExpired(t *testing.T) {
	now := time.Unix(1_780_000_000, 0).UTC()
	s, _ := newDSCallbackTestPair(t, now)
	tok, _, err := s.SignDSCallback(DSTypeBattle, "", 9001, time.Minute)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	// verifier 的 now 拨到 2 分钟后
	_, v := newDSCallbackTestPair(t, now.Add(2*time.Minute))
	if _, err := v.VerifyDSCallback(tok); err == nil {
		t.Fatal("expected expired error")
	}
}

// 玩家 SessionToken / DSTicket(aud=pandora-client)不可能通过 DS 回调校验(aud 分域)。
func TestVerifyDSCallbackRejectsPlayerTokens(t *testing.T) {
	now := time.Unix(1_780_000_000, 0).UTC()
	playerSigner, _ := newTestSigner(t, now) // 默认 aud=pandora-client
	_, dsVerifier := newDSCallbackTestPair(t, now)

	sess, _, err := playerSigner.SignSession(12345, "jti-x")
	if err != nil {
		t.Fatalf("SignSession: %v", err)
	}
	if _, err := dsVerifier.VerifyDSCallback(sess); err == nil {
		t.Fatal("session token must not pass ds callback verify")
	}
	ticket, _, err := playerSigner.SignDSTicket(12345, DSTypeBattle, 9001, "jti-y")
	if err != nil {
		t.Fatalf("SignDSTicket: %v", err)
	}
	if _, err := dsVerifier.VerifyDSCallback(ticket); err == nil {
		t.Fatal("ds ticket must not pass ds callback verify")
	}
}

// 恶意载荷:绕过 Sign 的 pre-check 直接签 "battle 但 match_id=0" / "hub 但 sub 空" / 未知 ds_type。
func TestVerifyDSCallbackRejectsMalformedClaims(t *testing.T) {
	now := time.Unix(1_780_000_000, 0).UTC()
	s, v := newDSCallbackTestPair(t, now)

	signRaw := func(dsType string, pod string, matchID uint64) string {
		claims := DSCallbackClaims{
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer:    s.cfg.Issuer,
				Subject:   pod,
				Audience:  jwt.ClaimStrings{s.cfg.Audience},
				IssuedAt:  jwt.NewNumericDate(now),
				ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			},
			DSType:  dsType,
			MatchID: matchID,
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		str, err := tok.SignedString(s.cfg.Secret)
		if err != nil {
			t.Fatalf("signRaw: %v", err)
		}
		return str
	}

	for _, c := range []struct {
		name string
		tok  string
	}{
		{"battle 无 match_id", signRaw("battle", "", 0)},
		{"hub 无 pod", signRaw("hub", "", 0)},
		{"未知 ds_type", signRaw("evil", "pod-1", 1)},
	} {
		if _, err := v.VerifyDSCallback(c.tok); err == nil {
			t.Fatalf("%s: expected reject", c.name)
		}
	}
}
