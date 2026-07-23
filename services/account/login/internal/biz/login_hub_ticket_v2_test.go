package biz

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/services/account/login/internal/data"
)

type hubV2TestKeys struct {
	private  *rsa.PrivateKey
	verifier *auth.DSTicketVerifier
	kid      string
	now      time.Time
}

func newHubV2TestKeys(t *testing.T) hubV2TestKeys {
	t.Helper()
	now := time.Unix(1_784_000_000, 0).UTC()
	privatePEM, public, kid, err := auth.GenerateDSTicketKeyPair()
	if err != nil {
		t.Fatalf("GenerateDSTicketKeyPair: %v", err)
	}
	block, _ := pem.Decode(privatePEM)
	if block == nil {
		t.Fatal("decode test private key: nil block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("ParsePKCS8PrivateKey: %v", err)
	}
	private, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		t.Fatalf("test private key type = %T, want *rsa.PrivateKey", parsed)
	}
	jwks, err := auth.MarshalDSTicketJWKS(1, kid, public)
	if err != nil {
		t.Fatalf("MarshalDSTicketJWKS: %v", err)
	}
	verifier, err := auth.NewDSTicketVerifier(auth.DSTicketVerifierConfig{
		JWKS: jwks,
		NowFn: func() time.Time {
			return now
		},
	})
	if err != nil {
		t.Fatalf("NewDSTicketVerifier: %v", err)
	}
	return hubV2TestKeys{private: private, verifier: verifier, kid: kid, now: now}
}

func signHubV2ForResolve(
	t *testing.T,
	keys hubV2TestKeys,
	headerKid string,
	mutate func(*auth.DSTicketClaimsV2),
) (string, int64) {
	t.Helper()
	claims := auth.DSTicketClaimsV2{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    auth.DSTicketIssuer,
			Subject:   "42",
			Audience:  jwt.ClaimStrings{auth.DSTicketAudience},
			IssuedAt:  jwt.NewNumericDate(keys.now),
			ExpiresAt: jwt.NewNumericDate(keys.now.Add(2 * time.Minute)),
			ID:        "hub-entry-v2-jti",
		},
		DstVer:          auth.DSTicketVersion2,
		DSType:          string(auth.DSTypeHub),
		DSPodName:       "hub-stable-1",
		DSInstanceUID:   "uid-hub-stable-1",
		DSInstanceEpoch: 7,
		ReleaseTrack:    auth.ReleaseTrackStable,
		HubAssignmentID: "assignment-42-v7",
	}
	if mutate != nil {
		mutate(&claims)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	if headerKid == "" {
		headerKid = keys.kid
	}
	token.Header["kid"] = headerKid
	signed, err := token.SignedString(keys.private)
	if err != nil {
		t.Fatalf("sign test hub v2 ticket: %v", err)
	}
	expMs := int64(0)
	if claims.ExpiresAt != nil {
		expMs = claims.ExpiresAt.UnixMilli()
	}
	return signed, expMs
}

func newHubV2ResolveUsecase(t *testing.T, hub data.HubAssigner, verifier *auth.DSTicketVerifier) *LoginUsecase {
	t.Helper()
	legacySigner, legacyVerifier := newTicketTestPair(t)
	uc := NewLoginUsecase(nil, nil, nil, hub, nil, nil, "127.0.0.1:7777", "cn",
		legacySigner, legacyVerifier, verifier, false, false, nil, false)
	uc.SetRequireHubAssignmentBinding(true)
	return uc
}

func TestResolveHubAcceptsValidDSTicketV2AndUsesVerifiedExpiry(t *testing.T) {
	keys := newHubV2TestKeys(t)
	ticket, wantExpMs := signHubV2ForResolve(t, keys, "", nil)
	hub := &fakeHubAssigner{res: &data.HubAssignment{
		HubDSAddr: "10.0.0.9:7777", HubTicket: ticket, HubPodName: "hub-stable-1", ShardID: 7,
	}}
	uc := newHubV2ResolveUsecase(t, hub, keys.verifier)

	addr, gotTicket, gotExpMs, err := uc.resolveHub(context.Background(), 42, 0, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("resolveHub: %v", err)
	}
	if addr != hub.res.HubDSAddr || gotTicket != ticket || gotExpMs != wantExpMs {
		t.Fatalf("addr=%q ticket_match=%v exp=%d, want addr=%q ticket_match=true exp=%d",
			addr, gotTicket == ticket, gotExpMs, hub.res.HubDSAddr, wantExpMs)
	}
}

func TestResolveHubRejectsInvalidDSTicketV2FailClosed(t *testing.T) {
	keys := newHubV2TestKeys(t)
	tests := []struct {
		name      string
		headerKid string
		mutate    func(*auth.DSTicketClaimsV2)
		rawTicket string
	}{
		{name: "wrong-player-uid", mutate: func(c *auth.DSTicketClaimsV2) { c.Subject = "43" }},
		{name: "wrong-pod", mutate: func(c *auth.DSTicketClaimsV2) { c.DSPodName = "hub-stable-2" }},
		{name: "wrong-type", mutate: func(c *auth.DSTicketClaimsV2) {
			c.DSType = string(auth.DSTypeBattle)
			c.HubAssignmentID = ""
			c.MatchID = 9001
			c.AllocationID = "allocation-9001"
		}},
		{name: "wrong-issuer", mutate: func(c *auth.DSTicketClaimsV2) { c.Issuer = "pandora-login" }},
		{name: "wrong-audience", mutate: func(c *auth.DSTicketClaimsV2) {
			c.Audience = jwt.ClaimStrings{"pandora-client"}
		}},
		{name: "unknown-kid", headerKid: "foreign-kid"},
		{name: "expired", mutate: func(c *auth.DSTicketClaimsV2) {
			c.IssuedAt = jwt.NewNumericDate(keys.now.Add(-2 * time.Minute))
			c.ExpiresAt = jwt.NewNumericDate(keys.now.Add(-time.Minute))
		}},
		{name: "missing-pod-binding", mutate: func(c *auth.DSTicketClaimsV2) { c.DSPodName = "" }},
		{name: "missing-instance-uid-binding", mutate: func(c *auth.DSTicketClaimsV2) { c.DSInstanceUID = "" }},
		{name: "missing-instance-epoch-binding", mutate: func(c *auth.DSTicketClaimsV2) { c.DSInstanceEpoch = 0 }},
		{name: "missing-assignment-binding", mutate: func(c *auth.DSTicketClaimsV2) { c.HubAssignmentID = "" }},
		{name: "missing-release-track-binding", mutate: func(c *auth.DSTicketClaimsV2) { c.ReleaseTrack = "" }},
		{name: "invalid-release-track-binding", mutate: func(c *auth.DSTicketClaimsV2) { c.ReleaseTrack = "beta" }},
		{name: "malformed-jose", rawTicket: "not-a-jwt"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ticket := tc.rawTicket
			if ticket == "" {
				ticket, _ = signHubV2ForResolve(t, keys, tc.headerKid, tc.mutate)
			}
			hub := &fakeHubAssigner{res: &data.HubAssignment{
				HubDSAddr: "10.0.0.9:7777", HubTicket: ticket, HubPodName: "hub-stable-1", ShardID: 7,
			}}
			uc := newHubV2ResolveUsecase(t, hub, keys.verifier)
			addr, gotTicket, expMs, err := uc.resolveHub(context.Background(), 42, 0, 0, 0, 0, "")
			if err == nil {
				t.Fatalf("resolveHub accepted invalid v2 ticket: addr=%q ticket_len=%d exp=%d",
					addr, len(gotTicket), expMs)
			}
			if addr != "" || gotTicket != "" || expMs != 0 {
				t.Fatalf("rejected ticket leaked partial result: addr=%q ticket_len=%d exp=%d",
					addr, len(gotTicket), expMs)
			}
		})
	}
}

func TestResolveHubRejectsDSTicketV2WithoutIndependentVerifier(t *testing.T) {
	keys := newHubV2TestKeys(t)
	ticket, _ := signHubV2ForResolve(t, keys, "", nil)
	hub := &fakeHubAssigner{res: &data.HubAssignment{
		HubDSAddr: "10.0.0.9:7777", HubTicket: ticket, HubPodName: "hub-stable-1", ShardID: 7,
	}}
	uc := newHubV2ResolveUsecase(t, hub, nil)
	if _, _, _, err := uc.resolveHub(context.Background(), 42, 0, 0, 0, 0, ""); err == nil {
		t.Fatal("v2 ticket without independent verifier must fail closed")
	}
}

func TestResolveHubRS256ProfileRejectsLegacyHS256DSTicket(t *testing.T) {
	keys := newHubV2TestKeys(t)
	legacySigner, legacyVerifier := newTicketTestPair(t)
	legacyTicket, _, err := legacySigner.SignDSTicket(42, auth.DSTypeHub, 0, "legacy-hub-jti")
	if err != nil {
		t.Fatal(err)
	}
	hub := &fakeHubAssigner{res: &data.HubAssignment{
		HubDSAddr: "10.0.0.8:7777", HubTicket: legacyTicket, HubPodName: "hub-legacy-1", ShardID: 8,
	}}

	// 只要装了 v2 verifier，即使 binding 激活栅栏尚未打开，也不得按票据 alg 降级到 HS256。
	v2Profile := NewLoginUsecase(nil, nil, nil, hub, nil, nil, "127.0.0.1:7777", "cn",
		legacySigner, legacyVerifier, keys.verifier, false, false, nil, false)
	if _, _, _, err := v2Profile.resolveHub(context.Background(), 42, 0, 0, 0, 0, ""); err == nil {
		t.Fatal("RS256 Login profile accepted allocator-issued legacy HS256 hub ticket")
	}
	v2Profile.hubAssigner = &fakeHubAssigner{err: context.DeadlineExceeded}
	if _, _, _, err := v2Profile.resolveHub(context.Background(), 42, 0, 0, 0, 0, ""); err == nil {
		t.Fatal("RS256 Login profile fell back to a self-signed HS256 hub ticket")
	}

	// 完全未配置 v2 的 local/off profile 仍可在兼容窗内验证 HS256。
	legacyProfile := NewLoginUsecase(nil, nil, nil, hub, nil, nil, "127.0.0.1:7777", "cn",
		legacySigner, legacyVerifier, nil, false, false, nil, false)
	if _, got, _, err := legacyProfile.resolveHub(context.Background(), 42, 0, 0, 0, 0, ""); err != nil || got != legacyTicket {
		t.Fatalf("legacy profile rejected HS256 ticket: ticket_match=%v err=%v", got == legacyTicket, err)
	}
}
