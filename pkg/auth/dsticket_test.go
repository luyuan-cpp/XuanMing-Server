// DSTicket v2(RS256/JWKS,方案 B)安全回归测试。
//
// 覆盖 decision-revisit-player-jwt-key-rotation.md §7 验收面:
//   - 正向:hub/battle v2 票签发→JWKS 验签通过,claims 完整回读;
//   - 轮换:K1+K2 双钥 keyset 同时接受两把 key 签的票(重叠窗口);
//   - kid:缺失/未知/张冠李戴一律拒;
//   - 算法混淆:HS256(用公钥字节当 HMAC 密钥)/none 一律拒;
//   - 跨域:Session/DS 回调令牌在 DSTicket 域必然失败;
//   - JWKS 严格性:oct、私钥成员、重复 kid、弱 RSA、kid 指纹不符、超限全拒;
//   - TTL:超过 DSTicketMaxTTL 的长票拒收(B1 短时 capability 契约)。
package auth

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
)

var dsTicketTestNow = time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

func dsTicketTestNowFn() time.Time { return dsTicketTestNow }

// newV2TestKeyPair 生成测试密钥对(仅测试用,绝非生产密钥)。
func newV2TestKeyPair(t *testing.T) ([]byte, *rsa.PublicKey, string) {
	t.Helper()
	pemBytes, pub, kid, err := GenerateDSTicketKeyPair()
	if err != nil {
		t.Fatalf("GenerateDSTicketKeyPair: %v", err)
	}
	return pemBytes, pub, kid
}

func newV2TestSigner(t *testing.T, pemBytes []byte) *DSTicketSigner {
	t.Helper()
	key, err := parseRSAPrivateKeyPEM(pemBytes)
	if err != nil {
		t.Fatalf("parseRSAPrivateKeyPEM: %v", err)
	}
	s, err := NewDSTicketSigner(DSTicketSignerConfig{
		PrivateKeyPEM: pemBytes, ActiveKid: RSAPublicKeyThumbprint(&key.PublicKey), NowFn: dsTicketTestNowFn,
	})
	if err != nil {
		t.Fatalf("NewDSTicketSigner: %v", err)
	}
	return s
}

func newV2TestVerifier(t *testing.T, pubs ...*rsa.PublicKey) *DSTicketVerifier {
	t.Helper()
	if len(pubs) == 0 {
		t.Fatal("newV2TestVerifier requires a key")
	}
	jwks, err := MarshalDSTicketJWKS(1, RSAPublicKeyThumbprint(pubs[0]), pubs...)
	if err != nil {
		t.Fatalf("MarshalDSTicketJWKS: %v", err)
	}
	v, err := NewDSTicketVerifier(DSTicketVerifierConfig{JWKS: jwks, NowFn: dsTicketTestNowFn})
	if err != nil {
		t.Fatalf("NewDSTicketVerifier: %v", err)
	}
	return v
}

func hubTarget() DSTicketTarget {
	return DSTicketTarget{
		DSPodName: "pandora-hub-abc", DSInstanceUID: "uid-hub-1", DSInstanceEpoch: 3,
		ReleaseTrack: ReleaseTrackStable, HubAssignmentID: "assign-42",
	}
}

func battleTarget() DSTicketTarget {
	return DSTicketTarget{
		DSPodName: "pandora-battle-xyz", DSInstanceUID: "uid-battle-9", DSInstanceEpoch: 7,
		ReleaseTrack: ReleaseTrackCanary, MatchID: 555, AllocationID: "alloc-777",
	}
}

func TestDSTicketV2_HubRoundTrip(t *testing.T) {
	pemBytes, pub, kid := newV2TestKeyPair(t)
	s := newV2TestSigner(t, pemBytes)
	v := newV2TestVerifier(t, pub)

	token, expMs, err := s.SignHubTicket(1001, 1, 2, 30, "jti-hub-1", hubTarget())
	if err != nil {
		t.Fatalf("SignHubTicket: %v", err)
	}
	if expMs != dsTicketTestNow.Add(DSTicketDefaultTTL).UnixMilli() {
		t.Fatalf("expMs mismatch: %d", expMs)
	}
	claims, err := v.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.PlayerID() != 1001 || claims.DSType != "hub" || claims.DstVer != DSTicketVersion2 {
		t.Fatalf("claims mismatch: %+v", claims)
	}
	if claims.HubAssignmentID != "assign-42" || claims.DSInstanceUID != "uid-hub-1" ||
		claims.DSInstanceEpoch != 3 || claims.ReleaseTrack != ReleaseTrackStable ||
		claims.RoleID != 30 || claims.RegionID != 1 || claims.CellID != 2 {
		t.Fatalf("binding claims mismatch: %+v", claims)
	}
	if claims.MatchID != 0 || claims.AllocationID != "" {
		t.Fatalf("hub ticket must not carry battle binding: %+v", claims)
	}
	if s.Kid() != kid {
		t.Fatalf("signer kid mismatch")
	}
}

// TestDSTicketV2_HubSourceMatchRoundTrip:Battle→Hub 回流 fence(source_match_id,
// 2026-07-21)hub 票往返。签进 claim → 验签后原样读出;不影响 hub 票“无 battle 绑定”不变量。
func TestDSTicketV2_HubSourceMatchRoundTrip(t *testing.T) {
	pemBytes, pub, _ := newV2TestKeyPair(t)
	s := newV2TestSigner(t, pemBytes)
	v := newV2TestVerifier(t, pub)

	tg := hubTarget()
	tg.SourceMatchID = 9001
	token, _, err := s.SignHubTicket(1001, 0, 0, 0, "jti-hub-fence", tg)
	if err != nil {
		t.Fatalf("SignHubTicket with source_match_id: %v", err)
	}
	claims, err := v.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.SourceMatchID != 9001 {
		t.Fatalf("source_match_id = %d, want 9001", claims.SourceMatchID)
	}
	if claims.MatchID != 0 || claims.AllocationID != "" {
		t.Fatalf("hub ticket must not carry battle binding: %+v", claims)
	}
}

func TestDSTicketV2_BattleRoundTrip(t *testing.T) {
	pemBytes, pub, _ := newV2TestKeyPair(t)
	s := newV2TestSigner(t, pemBytes)
	v := newV2TestVerifier(t, pub)

	token, _, err := s.SignBattleTicket(2002, 0, 0, "jti-battle-1", battleTarget())
	if err != nil {
		t.Fatalf("SignBattleTicket: %v", err)
	}
	claims, err := v.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.MatchID != 555 || claims.AllocationID != "alloc-777" || claims.DSType != "battle" {
		t.Fatalf("battle claims mismatch: %+v", claims)
	}
	if claims.HubAssignmentID != "" {
		t.Fatalf("battle ticket must not carry hub_assignment_id")
	}
}

func TestDSTicketV2_SignRejectsIncompleteBinding(t *testing.T) {
	pemBytes, _, _ := newV2TestKeyPair(t)
	s := newV2TestSigner(t, pemBytes)

	cases := []struct {
		name string
		fn   func() error
	}{
		{"hub missing assignment", func() error {
			tg := hubTarget()
			tg.HubAssignmentID = ""
			_, _, err := s.SignHubTicket(1, 0, 0, 0, "j", tg)
			return err
		}},
		{"hub carrying match", func() error {
			tg := hubTarget()
			tg.MatchID = 9
			_, _, err := s.SignHubTicket(1, 0, 0, 0, "j", tg)
			return err
		}},
		{"battle missing allocation", func() error {
			tg := battleTarget()
			tg.AllocationID = ""
			_, _, err := s.SignBattleTicket(1, 0, 0, "j", tg)
			return err
		}},
		{"battle missing match", func() error {
			tg := battleTarget()
			tg.MatchID = 0
			_, _, err := s.SignBattleTicket(1, 0, 0, "j", tg)
			return err
		}},
		{"battle carrying source_match_id", func() error {
			tg := battleTarget()
			tg.SourceMatchID = 9001
			_, _, err := s.SignBattleTicket(1, 0, 0, "j", tg)
			return err
		}},
		{"missing uid", func() error {
			tg := hubTarget()
			tg.DSInstanceUID = ""
			_, _, err := s.SignHubTicket(1, 0, 0, 0, "j", tg)
			return err
		}},
		{"missing epoch", func() error {
			tg := hubTarget()
			tg.DSInstanceEpoch = 0
			_, _, err := s.SignHubTicket(1, 0, 0, 0, "j", tg)
			return err
		}},
		{"bad release track", func() error {
			tg := hubTarget()
			tg.ReleaseTrack = "beta"
			_, _, err := s.SignHubTicket(1, 0, 0, 0, "j", tg)
			return err
		}},
		{"empty jti", func() error {
			_, _, err := s.SignHubTicket(1, 0, 0, 0, "", hubTarget())
			return err
		}},
		{"zero player", func() error {
			_, _, err := s.SignHubTicket(0, 0, 0, 0, "j", hubTarget())
			return err
		}},
	}
	for _, c := range cases {
		if err := c.fn(); err == nil {
			t.Fatalf("%s: want error, got nil", c.name)
		}
	}
}

func TestDSTicketV2_RotationOverlapWindow(t *testing.T) {
	pem1, pub1, _ := newV2TestKeyPair(t)
	pem2, pub2, _ := newV2TestKeyPair(t)
	s1 := newV2TestSigner(t, pem1)
	s2 := newV2TestSigner(t, pem2)
	// 重叠窗口:JWKS 同时含 K1+K2。
	v := newV2TestVerifier(t, pub1, pub2)

	tok1, _, err := s1.SignHubTicket(1, 0, 0, 0, "j1", hubTarget())
	if err != nil {
		t.Fatalf("sign K1: %v", err)
	}
	tok2, _, err := s2.SignHubTicket(1, 0, 0, 0, "j2", hubTarget())
	if err != nil {
		t.Fatalf("sign K2: %v", err)
	}
	if _, err := v.Verify(tok1); err != nil {
		t.Fatalf("K1 ticket must verify during overlap: %v", err)
	}
	if _, err := v.Verify(tok2); err != nil {
		t.Fatalf("K2 ticket must verify during overlap: %v", err)
	}
	// 清退 K1 后:K1 票必拒。
	vOnly2 := newV2TestVerifier(t, pub2)
	if _, err := vOnly2.Verify(tok1); err == nil {
		t.Fatalf("K1 ticket must be rejected after K1 removed from keyset")
	}
}

func TestDSTicketV2_ActiveKidMismatchRejected(t *testing.T) {
	pem1, _, _ := newV2TestKeyPair(t)
	_, _, kid2 := newV2TestKeyPair(t)
	if _, err := NewDSTicketSigner(DSTicketSignerConfig{PrivateKeyPEM: pem1, ActiveKid: kid2}); err == nil {
		t.Fatalf("active_kid mismatch must fail startup")
	}
}

func TestDSTicketV2_AlgConfusionRejected(t *testing.T) {
	pemBytes, pub, kid := newV2TestKeyPair(t)
	s := newV2TestSigner(t, pemBytes)
	v := newV2TestVerifier(t, pub)

	good, _, err := s.SignHubTicket(1, 0, 0, 0, "j", hubTarget())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	parts := strings.Split(good, ".")
	if len(parts) != 3 {
		t.Fatalf("bad jwt")
	}

	// alg=none。
	noneHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT","kid":"` + kid + `"}`))
	if _, err := v.Verify(noneHeader + "." + parts[1] + "."); err == nil {
		t.Fatalf("alg=none must be rejected")
	}

	// alg=HS256(把公钥 JWKS n 字节当 HMAC 密钥的经典混淆)。
	hsClaims := jwt.MapClaims{}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if err := json.Unmarshal(payload, &hsClaims); err != nil {
		t.Fatalf("payload: %v", err)
	}
	hsTok := jwt.NewWithClaims(jwt.SigningMethodHS256, hsClaims)
	hsTok.Header["kid"] = kid
	hsStr, err := hsTok.SignedString(pub.N.Bytes())
	if err != nil {
		t.Fatalf("hs sign: %v", err)
	}
	if _, err := v.Verify(hsStr); err == nil {
		t.Fatalf("HS256 confusion must be rejected")
	}

	// 篡改 payload(签名失配)。
	tampered, _ := json.Marshal(map[string]any{"sub": "9999"})
	if _, err := v.Verify(parts[0] + "." + base64.RawURLEncoding.EncodeToString(tampered) + "." + parts[2]); err == nil {
		t.Fatalf("tampered payload must be rejected")
	}
}

func TestDSTicketV2_KidRequiredAndKnown(t *testing.T) {
	pemBytes, pub, _ := newV2TestKeyPair(t)
	v := newV2TestVerifier(t, pub)

	// 手工构造无 kid 的 RS256 票(同一把私钥):必须拒。
	key, err := parseRSAPrivateKeyPEM(pemBytes)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	claims := DSTicketClaimsV2{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: DSTicketIssuer, Subject: "1", Audience: jwt.ClaimStrings{DSTicketAudience},
			IssuedAt: jwt.NewNumericDate(dsTicketTestNow), ExpiresAt: jwt.NewNumericDate(dsTicketTestNow.Add(time.Minute)), ID: "j",
		},
		DstVer: 2, DSType: "hub", DSPodName: "p", DSInstanceUID: "u", DSInstanceEpoch: 1, HubAssignmentID: "a",
	}
	noKid := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	str, err := noKid.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := v.Verify(str); err == nil {
		t.Fatalf("missing kid must be rejected")
	}
	// 未知 kid。
	unknown := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	unknown.Header["kid"] = "kid-not-in-keyset"
	str2, err := unknown.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := v.Verify(str2); err == nil {
		t.Fatalf("unknown kid must be rejected")
	}
}

func TestDSTicketV2_CrossDomainTokensRejected(t *testing.T) {
	_, pub, _ := newV2TestKeyPair(t)
	v := newV2TestVerifier(t, pub)

	// 玩家面 HS256 Signer 签出的 Session / v1 DSTicket / DS 回调令牌全部必拒。
	legacy, err := NewSigner(Config{Secret: []byte("pandora-test-only-secret-32-bytes!!"), NowFn: dsTicketTestNowFn})
	if err != nil {
		t.Fatalf("legacy signer: %v", err)
	}
	sess, _, err := legacy.SignSession(1, "js")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	v1Ticket, _, err := legacy.SignDSTicket(1, DSTypeHub, 0, "jt")
	if err != nil {
		t.Fatalf("v1 ticket: %v", err)
	}
	cbSigner, err := NewDSCallbackSigner(Config{Issuer: DSCallbackIssuer, Audience: DSCallbackAudience, Secret: []byte("pandora-test-only-ds-secret-32-b!!!!"), NowFn: dsTicketTestNowFn})
	if err != nil {
		t.Fatalf("cb signer: %v", err)
	}
	cb, _, err := cbSigner.SignDSCallback(DSTypeHub, "pod-1", 0, time.Minute)
	if err != nil {
		t.Fatalf("cb: %v", err)
	}
	for name, tok := range map[string]string{"session": sess, "v1-dsticket": v1Ticket, "ds-callback": cb} {
		if _, err := v.Verify(tok); err == nil {
			t.Fatalf("%s token must be rejected in DSTicket v2 domain", name)
		}
	}
}

func TestDSTicketV2_ExpiredAndOverlongRejected(t *testing.T) {
	pemBytes, pub, _ := newV2TestKeyPair(t)
	v := newV2TestVerifier(t, pub)
	key, _ := parseRSAPrivateKeyPEM(pemBytes)
	kid := RSAPublicKeyThumbprint(pub)

	mk := func(iat, exp time.Time) string {
		claims := DSTicketClaimsV2{
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer: DSTicketIssuer, Subject: "1", Audience: jwt.ClaimStrings{DSTicketAudience},
				IssuedAt: jwt.NewNumericDate(iat), ExpiresAt: jwt.NewNumericDate(exp), ID: "j",
			},
			DstVer: 2, DSType: "hub", DSPodName: "p", DSInstanceUID: "u", DSInstanceEpoch: 1, HubAssignmentID: "a",
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = kid
		str, err := tok.SignedString(key)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return str
	}
	// 过期票。
	if _, err := v.Verify(mk(dsTicketTestNow.Add(-10*time.Minute), dsTicketTestNow.Add(-5*time.Minute))); err == nil {
		t.Fatalf("expired ticket must be rejected")
	}
	// 长寿命票(exp-iat > MaxTTL):即使未过期也拒(B1 短时 capability 契约)。
	if _, err := v.Verify(mk(dsTicketTestNow.Add(-time.Minute), dsTicketTestNow.Add(time.Hour))); err == nil {
		t.Fatalf("overlong ticket must be rejected")
	}
}

func TestDSTicketV2_VerifyRejectsWrongVersionAndBinding(t *testing.T) {
	pemBytes, pub, _ := newV2TestKeyPair(t)
	v := newV2TestVerifier(t, pub)
	key, _ := parseRSAPrivateKeyPEM(pemBytes)
	kid := RSAPublicKeyThumbprint(pub)

	mk := func(mut func(*DSTicketClaimsV2)) string {
		claims := DSTicketClaimsV2{
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer: DSTicketIssuer, Subject: "1", Audience: jwt.ClaimStrings{DSTicketAudience},
				IssuedAt: jwt.NewNumericDate(dsTicketTestNow), ExpiresAt: jwt.NewNumericDate(dsTicketTestNow.Add(time.Minute)), ID: "j",
			},
			DstVer: 2, DSType: "hub", DSPodName: "p", DSInstanceUID: "u", DSInstanceEpoch: 1, HubAssignmentID: "a",
		}
		mut(&claims)
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = kid
		str, err := tok.SignedString(key)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return str
	}
	cases := map[string]func(*DSTicketClaimsV2){
		"dst_ver=1":              func(c *DSTicketClaimsV2) { c.DstVer = 1 },
		"dst_ver=3":              func(c *DSTicketClaimsV2) { c.DstVer = 3 },
		"bad ds_type":            func(c *DSTicketClaimsV2) { c.DSType = "lobby" },
		"hub missing assignment": func(c *DSTicketClaimsV2) { c.HubAssignmentID = "" },
		"hub with match binding": func(c *DSTicketClaimsV2) { c.MatchID = 5; c.AllocationID = "x" },
		"missing pod":            func(c *DSTicketClaimsV2) { c.DSPodName = "" },
		"missing uid":            func(c *DSTicketClaimsV2) { c.DSInstanceUID = "" },
		"zero epoch":             func(c *DSTicketClaimsV2) { c.DSInstanceEpoch = 0 },
		"missing jti":            func(c *DSTicketClaimsV2) { c.ID = "" },
		"bad sub":                func(c *DSTicketClaimsV2) { c.Subject = "not-a-number" },
		"wrong issuer":           func(c *DSTicketClaimsV2) { c.Issuer = "pandora-login" },
		"wrong audience":         func(c *DSTicketClaimsV2) { c.Audience = jwt.ClaimStrings{"pandora-client"} },
		"invalid release track":  func(c *DSTicketClaimsV2) { c.ReleaseTrack = "beta" },
		"battle missing binding": func(c *DSTicketClaimsV2) { c.DSType = "battle"; c.HubAssignmentID = "" },
	}
	for name, mut := range cases {
		if _, err := v.Verify(mk(mut)); err == nil {
			t.Fatalf("%s: must be rejected", name)
		}
	}
}

func TestDSTicketJWKS_StrictParsing(t *testing.T) {
	_, pub, kid := newV2TestKeyPair(t)
	good, err := MarshalDSTicketJWKS(3, kid, pub)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := ParseDSTicketJWKS(good); err != nil {
		t.Fatalf("good jwks must parse: %v", err)
	}
	var marshaled struct {
		Keys []map[string]json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(good, &marshaled); err != nil {
		t.Fatalf("unmarshal marshaled jwks: %v", err)
	}
	if len(marshaled.Keys) != 1 {
		t.Fatalf("marshaled jwks keys=%d, want 1", len(marshaled.Keys))
	}
	wantPublicFields := map[string]struct{}{
		"alg": {}, "e": {}, "kid": {}, "kty": {}, "n": {}, "use": {},
	}
	if len(marshaled.Keys[0]) != len(wantPublicFields) {
		t.Fatalf("marshaled jwks key fields=%v, want exactly alg,e,kid,kty,n,use", marshaled.Keys[0])
	}
	for field := range marshaled.Keys[0] {
		if _, ok := wantPublicFields[field]; !ok {
			t.Fatalf("marshaled jwks contains non-public key field %q", field)
		}
	}
	if rev, err := DSTicketJWKSRevision(good); err != nil || rev != 3 {
		t.Fatalf("revision: %d %v", rev, err)
	}

	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1})
	base := `"revision":3,"active_kid":"` + kid + `",`
	keyMeta := `"use":"sig","alg":"RS256",`
	bad := map[string]string{
		"empty":          ``,
		"no keys":        `{"keys":[]}`,
		"oct key":        `{` + base + `"keys":[{"kty":"oct",` + keyMeta + `"kid":"k1","k":"c2VjcmV0"}]}`,
		"private d":      `{` + base + `"keys":[{"kty":"RSA",` + keyMeta + `"kid":"` + kid + `","n":"` + n + `","e":"` + e + `","d":"cHJpdg"}]}`,
		"missing kid":    `{` + base + `"keys":[{"kty":"RSA",` + keyMeta + `"n":"` + n + `","e":"` + e + `"}]}`,
		"missing use":    `{` + base + `"keys":[{"kty":"RSA","alg":"RS256","kid":"` + kid + `","n":"` + n + `","e":"` + e + `"}]}`,
		"missing alg":    `{` + base + `"keys":[{"kty":"RSA","use":"sig","kid":"` + kid + `","n":"` + n + `","e":"` + e + `"}]}`,
		"dup kid":        `{` + base + `"keys":[{"kty":"RSA",` + keyMeta + `"kid":"` + kid + `","n":"` + n + `","e":"` + e + `"},{"kty":"RSA",` + keyMeta + `"kid":"` + kid + `","n":"` + n + `","e":"` + e + `"}]}`,
		"kid mismatch":   `{` + base + `"keys":[{"kty":"RSA",` + keyMeta + `"kid":"wrong-kid","n":"` + n + `","e":"` + e + `"}]}`,
		"bad use":        `{` + base + `"keys":[{"kty":"RSA","use":"enc","alg":"RS256","kid":"` + kid + `","n":"` + n + `","e":"` + e + `"}]}`,
		"bad alg":        `{` + base + `"keys":[{"kty":"RSA","use":"sig","alg":"RS512","kid":"` + kid + `","n":"` + n + `","e":"` + e + `"}]}`,
		"unknown field":  `{` + base + `"keys":[{"kty":"RSA",` + keyMeta + `"kid":"` + kid + `","n":"` + n + `","e":"` + e + `","x5c":["a"]}]}`,
		"even e":         `{` + base + `"keys":[{"kty":"RSA",` + keyMeta + `"kid":"` + kid + `","n":"` + n + `","e":"` + base64.RawURLEncoding.EncodeToString([]byte{2}) + `"}]}`,
		"missing active": `{"revision":3,"keys":[{"kty":"RSA",` + keyMeta + `"kid":"` + kid + `","n":"` + n + `","e":"` + e + `"}]}`,
		"unknown active": `{"revision":3,"active_kid":"missing","keys":[{"kty":"RSA",` + keyMeta + `"kid":"` + kid + `","n":"` + n + `","e":"` + e + `"}]}`,
	}
	for name, raw := range bad {
		if _, err := ParseDSTicketJWKS([]byte(raw)); err == nil {
			t.Fatalf("jwks %s: must be rejected", name)
		}
	}

	validKeyPrefix := `{"kty":"RSA",` + keyMeta + `"kid":"` + kid + `","n":"` + n + `","e":"` + e + `"`
	for _, field := range []string{"d", "p", "q", "dp", "dq", "qi", "oth", "k"} {
		for valueName, value := range map[string]string{
			"empty": `""`,
			"value": `"private"`,
			"null":  `null`,
		} {
			t.Run("private member "+field+"/"+valueName, func(t *testing.T) {
				raw := `{` + base + `"keys":[` + validKeyPrefix + `,"` + field + `":` + value + `}]}`
				if _, err := ParseDSTicketJWKS([]byte(raw)); err == nil {
					t.Fatalf("private member %s=%s must be rejected", field, value)
				}
			})
		}
	}
}

func TestDSTicketSigner_TTLBounds(t *testing.T) {
	pemBytes, _, kid := newV2TestKeyPair(t)
	if _, err := NewDSTicketSigner(DSTicketSignerConfig{PrivateKeyPEM: pemBytes, ActiveKid: kid, TTL: 10 * time.Minute}); err == nil {
		t.Fatalf("ttl above max must fail startup")
	}
	if _, err := NewDSTicketSigner(DSTicketSignerConfig{PrivateKeyPEM: pemBytes, ActiveKid: kid, TTL: 90 * time.Second}); err != nil {
		t.Fatalf("valid ttl must pass: %v", err)
	}
	if _, err := NewDSTicketSigner(DSTicketSignerConfig{PrivateKeyPEM: pemBytes, TTL: 90 * time.Second}); err == nil {
		t.Fatalf("missing active_kid must fail startup")
	}
}
