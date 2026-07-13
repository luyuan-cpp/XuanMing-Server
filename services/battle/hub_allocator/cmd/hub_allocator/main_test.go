package main

import (
	"testing"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/biz"
)

func TestHubTicketSignerB1RS256CompleteBinding(t *testing.T) {
	privatePEM, publicKey, kid, err := auth.GenerateDSTicketKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewDSTicketSigner(auth.DSTicketSignerConfig{PrivateKeyPEM: privatePEM, ActiveKid: kid})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &hubTicketSigner{v2: signer}
	token, _, err := adapter.SignHubTicket(1001, 7, biz.HubTicketBinding{
		PodName: "hub-canary-1", InstanceUID: "uid-hub-canary", ProtocolEpoch: 3,
		HubAssignmentID: "assignment-1", ReleaseTrack: auth.ReleaseTrackCanary,
	})
	if err != nil {
		t.Fatal(err)
	}
	alg, err := auth.DSTicketAlgorithm(token)
	if err != nil || alg != "RS256" {
		t.Fatalf("alg=%q err=%v", alg, err)
	}
	jwks, err := auth.MarshalDSTicketJWKS(1, kid, publicKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := auth.NewDSTicketVerifier(auth.DSTicketVerifierConfig{JWKS: jwks})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := verifier.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.DstVer != auth.DSTicketVersion2 || claims.DSType != string(auth.DSTypeHub) ||
		claims.PlayerID() != 1001 || claims.DSPodName != "hub-canary-1" ||
		claims.DSInstanceUID != "uid-hub-canary" || claims.DSInstanceEpoch != 3 ||
		claims.HubAssignmentID != "assignment-1" || claims.ReleaseTrack != auth.ReleaseTrackCanary {
		t.Fatalf("claims=%+v", claims)
	}

	if _, _, err := adapter.SignHubTicket(1001, 7, biz.HubTicketBinding{
		PodName: "hub-1", InstanceUID: "uid-1", ProtocolEpoch: 1, HubAssignmentID: "assignment-2",
	}); err == nil {
		t.Fatal("missing release_track must fail closed")
	}
}
