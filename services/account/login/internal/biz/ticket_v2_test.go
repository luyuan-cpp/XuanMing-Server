package biz

import (
	"testing"

	"github.com/luyuancpp/pandora/pkg/auth"
)

func TestVerifyDSTicketSignatureExactHS256RS256Dispatch(t *testing.T) {
	legacyCfg := auth.Config{Secret: []byte("login-ticket-v2-bridge-test-secret-32-bytes")}
	legacySigner, err := auth.NewSigner(legacyCfg)
	if err != nil {
		t.Fatal(err)
	}
	legacyVerifier, err := auth.NewVerifier(legacyCfg)
	if err != nil {
		t.Fatal(err)
	}
	uc := NewTicketUsecase(legacySigner, legacyVerifier, nil)

	privatePEM, publicKey, kid, err := auth.GenerateDSTicketKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	v2Signer, err := auth.NewDSTicketSigner(auth.DSTicketSignerConfig{PrivateKeyPEM: privatePEM, ActiveKid: kid})
	if err != nil {
		t.Fatal(err)
	}
	jwks, err := auth.MarshalDSTicketJWKS(9, kid, publicKey)
	if err != nil {
		t.Fatal(err)
	}
	v2Verifier, err := auth.NewDSTicketVerifier(auth.DSTicketVerifierConfig{JWKS: jwks})
	if err != nil {
		t.Fatal(err)
	}
	uc.SetDSTicketV2Verifier(v2Verifier)
	if _, err := uc.IssueDSTicket(t.Context(), 1001, string(auth.DSTypeHub), 0); err == nil {
		t.Fatal("RS256 verifier profile self-signed a legacy HS256 hub ticket")
	}

	v2Token, _, err := v2Signer.SignHubTicket(1001, 0, 0, 7, "v2-jti", auth.DSTicketTarget{
		DSPodName: "hub-canary-1", DSInstanceUID: "uid-canary", DSInstanceEpoch: 4,
		HubAssignmentID: "assignment-v2", ReleaseTrack: auth.ReleaseTrackCanary,
	})
	if err != nil {
		t.Fatal(err)
	}
	v2Claims, err := uc.verifyDSTicketSignature(v2Token)
	if err != nil {
		t.Fatal(err)
	}
	if v2Claims.Version != auth.DSTicketVersion2 || v2Claims.ReleaseTrack != auth.ReleaseTrackCanary ||
		v2Claims.DSPodName != "hub-canary-1" || v2Claims.DSInstanceUID != "uid-canary" ||
		v2Claims.DSInstanceEpoch != 4 || v2Claims.HubAssignmentID != "assignment-v2" {
		t.Fatalf("v2 claims=%+v", v2Claims)
	}

	legacyToken, _, err := legacySigner.SignDSTicket(1001, auth.DSTypeBattle, 9001, "legacy-jti")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uc.verifyDSTicketSignature(legacyToken); err == nil {
		t.Fatal("RS256 verifier profile accepted legacy HS256 DSTicket")
	}

	// signer 也必须独立机械激活 RS256-only，避免误接线时 verifier 尚未注入而短暂放行。
	signerOnlyUC := NewTicketUsecase(legacySigner, legacyVerifier, nil)
	signerOnlyUC.SetDSTicketV2Signer(v2Signer)
	if _, err := signerOnlyUC.verifyDSTicketSignature(legacyToken); err == nil {
		t.Fatal("RS256 signer profile accepted legacy HS256 DSTicket")
	}

	// 完全没有配置 v2 的 local/off profile 仍保留 legacy 兼容。
	legacyOnlyUC := NewTicketUsecase(legacySigner, legacyVerifier, nil)
	legacyClaims, err := legacyOnlyUC.verifyDSTicketSignature(legacyToken)
	if err != nil {
		t.Fatal(err)
	}
	if legacyClaims.Version != 1 || legacyClaims.MatchID != 9001 || legacyClaims.ReleaseTrack != "" {
		t.Fatalf("legacy claims=%+v", legacyClaims)
	}
}
