package biz

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"
	"github.com/luyuancpp/pandora/services/account/login/internal/data"
)

type battlePlacementCheckerFake struct {
	replayRepo *admissionReplayRepo
	commitErr  error
	calls      atomic.Int32
	sawMarker  atomic.Bool
	last       data.BattlePlacementAdmission
}

func (*battlePlacementCheckerFake) CheckHubAdmission(context.Context, data.HubPlacementAdmission) error {
	return nil
}
func (*battlePlacementCheckerFake) GetPlacement(context.Context, uint64) (data.PlacementSnapshot, error) {
	return data.PlacementSnapshot{}, nil
}
func (*battlePlacementCheckerFake) BootstrapHub(context.Context, uint64, string, string, string, int64) (placement.Binding, error) {
	return placement.Binding{}, nil
}
func (*battlePlacementCheckerFake) BeginHubFromBattle(context.Context, uint64, uint64, placement.BattleExitProof, int64) (placement.Binding, error) {
	return placement.Binding{}, nil
}
func (f *battlePlacementCheckerFake) CommitBattleAdmission(_ context.Context, in data.BattlePlacementAdmission) error {
	f.calls.Add(1)
	f.last = in
	if f.replayRepo != nil {
		_, marks := f.replayRepo.counts()
		f.sawMarker.Store(marks != 0)
	}
	return f.commitErr
}

func newV2TicketPair(t *testing.T) (*auth.DSTicketSigner, *auth.DSTicketVerifier) {
	t.Helper()
	privatePEM, publicKey, kid, err := auth.GenerateDSTicketKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewDSTicketSigner(auth.DSTicketSignerConfig{PrivateKeyPEM: privatePEM, ActiveKid: kid})
	if err != nil {
		t.Fatal(err)
	}
	jwks, err := auth.MarshalDSTicketJWKS(1, kid, publicKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := auth.NewDSTicketVerifier(auth.DSTicketVerifierConfig{JWKS: jwks})
	if err != nil {
		t.Fatal(err)
	}
	return signer, verifier
}

func TestBattlePlacementCommitPrecedesJTIMarkerAndFailsClosed(t *testing.T) {
	v2Signer, v2Verifier := newV2TicketPair(t)
	legacyCfg := auth.Config{Secret: []byte("battle-placement-test-secret-32-bytes!")}
	legacySigner, _ := auth.NewSigner(legacyCfg)
	legacyVerifier, _ := auth.NewVerifier(legacyCfg)
	binding := placement.Binding{Version: 7,
		OperationID: "123e4567-e89b-42d3-a456-426614174000"}
	ticket, _, err := v2Signer.SignBattleTicket(1001, 0, 0, "placement-battle-jti", auth.DSTicketTarget{
		DSPodName: "battle-1", DSInstanceUID: "battle-uid", DSInstanceEpoch: 4,
		ReleaseTrack: auth.ReleaseTrackStable, MatchID: 9001, AllocationID: "allocation-1",
		Placement: binding,
	})
	if err != nil {
		t.Fatal(err)
	}
	admission := admissionBinding(auth.DSTypeBattle, 9001, "battle-1", "battle-uid", 4, 8, "cred-b")
	admission.AllocationID = "allocation-1"
	admission.ReleaseTrack = auth.ReleaseTrackStable
	const admissionID = "123e4567-e89b-42d3-a456-426614174001"

	t.Run("commit-conflict-has-zero-marker", func(t *testing.T) {
		repo := &admissionReplayRepo{}
		checker := &battlePlacementCheckerFake{replayRepo: repo,
			commitErr: errcode.New(errcode.ErrLocatorConflict, "stale placement")}
		uc := NewTicketUsecase(legacySigner, legacyVerifier, repo)
		uc.SetDSTicketV2Verifier(v2Verifier)
		uc.SetPlacementAdmissionPolicy(placement.ModeEnforce, checker)
		if _, err := uc.VerifyDSTicketForAdmission(context.Background(), ticket, admission.PodName,
			admissionID, admission); errcode.As(err) != errcode.ErrLocatorConflict {
			t.Fatalf("code=%v err=%v", errcode.As(err), err)
		}
		_, marks := repo.counts()
		if marks != 0 || checker.sawMarker.Load() {
			t.Fatalf("placement conflict wrote JTI marker: marks=%d saw=%v", marks, checker.sawMarker.Load())
		}
	})

	t.Run("commit-success-then-marker", func(t *testing.T) {
		repo := &admissionReplayRepo{}
		checker := &battlePlacementCheckerFake{replayRepo: repo}
		uc := NewTicketUsecase(legacySigner, legacyVerifier, repo)
		uc.SetDSTicketV2Verifier(v2Verifier)
		uc.SetPlacementAdmissionPolicy(placement.ModeEnforce, checker)
		if _, err := uc.VerifyDSTicketForAdmission(context.Background(), ticket, admission.PodName,
			admissionID, admission); err != nil {
			t.Fatal(err)
		}
		_, marks := repo.counts()
		if checker.calls.Load() != 1 || checker.sawMarker.Load() || marks != 1 {
			t.Fatalf("calls=%d sawMarker=%v marks=%d", checker.calls.Load(), checker.sawMarker.Load(), marks)
		}
		if checker.last.Binding != binding || checker.last.MatchID != 9001 ||
			checker.last.Target.AllocationID != "allocation-1" {
			t.Fatalf("commit identity=%+v", checker.last)
		}
	})
}
