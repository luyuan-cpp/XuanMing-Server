package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/biz"
)

func TestKubernetesDeploymentUsesRecreateSingleWriterStrategy(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test source path")
	}
	manifestPath := filepath.Clean(filepath.Join(filepath.Dir(testFile), "..", "..", "..", "..", "..",
		"deploy", "k8s", "services", "services.yaml"))
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	const deploymentMarker = "metadata: { name: hub-allocator, namespace: pandora, labels: { app: hub-allocator } }"
	start := strings.Index(string(raw), deploymentMarker)
	if start < 0 {
		t.Fatal("hub-allocator Deployment not found")
	}
	section := string(raw)[start:]
	if end := strings.Index(section, "\n---"); end >= 0 {
		section = section[:end]
	}
	if !strings.Contains(section, "replicas: 1") ||
		!strings.Contains(section, "strategy: { type: Recreate }") {
		t.Fatalf("hub-allocator must be a single-writer Recreate Deployment:\n%s", section)
	}
}

func TestHubWriterStagesCapabilityButCannotServeBeforePolicyV3(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	feature := strings.Index(source, `"hub-successor-lease-v1"`)
	gate := strings.Index(source, "fence.RequiredPolicyGeneration() != dsauthfence.RequiredPolicyGenerationV3")
	sweep := strings.Index(source, "go uc.RunHeartbeatSweep")
	serve := strings.Index(source, "app.Run()")
	if feature < 0 || gate < 0 || sweep < 0 || serve < 0 || gate > sweep || gate > serve {
		t.Fatalf("Hub must advertise V3 support but block all background/RPC writers while staged: feature=%d gate=%d sweep=%d serve=%d",
			feature, gate, sweep, serve)
	}
	stagingBlock := source[gate:sweep]
	if !strings.Contains(stagingBlock, "<-fence.Lost()") || !strings.Contains(stagingBlock, "os.Exit(0)") {
		t.Fatal("staged Hub must wait for the V2->V3 watch event and restart before serving")
	}
}

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
