package biz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/placement"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
	loginv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/login/v1"
	"github.com/luyuancpp/pandora/services/account/login/internal/data"
)

type resumePlacementFake struct {
	snapshot data.PlacementSnapshot
	err      error
}

func (f *resumePlacementFake) GetPlacement(context.Context, uint64) (data.PlacementSnapshot, error) {
	return f.snapshot, f.err
}
func (*resumePlacementFake) CheckHubAdmission(context.Context, data.HubPlacementAdmission) error {
	return nil
}
func (*resumePlacementFake) BootstrapHub(context.Context, uint64, string, string, string, int64) (placement.Binding, error) {
	return placement.Binding{}, nil
}
func (*resumePlacementFake) BeginHubFromBattle(context.Context, uint64, uint64, placement.BattleExitProof, int64) (placement.Binding, error) {
	return placement.Binding{}, nil
}
func (*resumePlacementFake) CommitBattleAdmission(context.Context, data.BattlePlacementAdmission) error {
	return nil
}

type matchResumeFake struct {
	context data.MatchResumeContext
	err     error
}

type terminalRecoveryPlacementFake struct {
	resumePlacementFake
	beginCalls int
	beginMatch uint64
	beginProof placement.BattleExitProof
	beginErr   error
}

type battleDepartureFake struct {
	calls      int
	transition placement.Binding
	source     placement.Binding
	target     placement.Target
	checker    *terminalRecoveryPlacementFake
}

func (f *battleDepartureFake) EnsurePlayerDeparture(_ context.Context, _, _ uint64,
	transition, source placement.Binding, target placement.Target,
) error {
	if f.checker != nil && (f.checker.snapshot.TransitionState != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING ||
		f.checker.snapshot.TargetRoute != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB) {
		return errors.New("departure called before pending Hub placement fence")
	}
	f.calls++
	f.transition, f.source, f.target = transition, source, target
	return nil
}

func recoveryBattleTarget() placement.Target {
	return placement.Target{PodName: "battle-801", InstanceUID: "battle-uid-801", InstanceEpoch: 1,
		AllocationID: "allocation-801", ReleaseTrack: "stable"}
}

func recoveryHubTarget() placement.Target {
	return placement.Target{PodName: "hub-1", InstanceUID: "hub-uid-1", InstanceEpoch: 1,
		AssignmentID: "assignment-1", ReleaseTrack: "stable"}
}

type bootstrapRecoveryPlacementFake struct {
	resumePlacementFake
	bootstrapCalls  int
	bootstrapPlayer uint64
	bootstrapOp     string
	bootstrapProof  string
	bootstrapSig    string
	bootstrapLease  int64
}

func (f *bootstrapRecoveryPlacementFake) BootstrapHub(_ context.Context, playerID uint64,
	op, proofID, signature string, leaseDeadlineMs int64,
) (placement.Binding, error) {
	f.bootstrapCalls++
	f.bootstrapPlayer, f.bootstrapOp, f.bootstrapProof = playerID, op, proofID
	f.bootstrapSig, f.bootstrapLease = signature, leaseDeadlineMs
	f.snapshot.LeaseDeadlineMs = leaseDeadlineMs
	return placement.Binding{Version: f.snapshot.Version, OperationID: f.snapshot.OperationID,
		SourceMatchID: f.snapshot.SourceMatchID}, nil
}

func (f *terminalRecoveryPlacementFake) GetPlacement(context.Context, uint64) (data.PlacementSnapshot, error) {
	return f.snapshot, f.err
}

func (f *terminalRecoveryPlacementFake) BeginHubFromBattle(_ context.Context, _ uint64, matchID uint64,
	proof placement.BattleExitProof, _ int64) (placement.Binding, error) {
	f.beginCalls++
	f.beginMatch, f.beginProof = matchID, proof
	if f.beginErr != nil {
		return placement.Binding{}, f.beginErr
	}
	old := f.snapshot
	if old.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE {
		f.snapshot.SourceBinding = placement.Binding{Version: old.Version, OperationID: old.OperationID}
		f.snapshot.SourceTarget = old.Target
	}
	f.snapshot.TargetRoute = locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB
	f.snapshot.TransitionState = locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING
	f.snapshot.Version = proof.ExpectedVersion + 1
	f.snapshot.OperationID = proof.OperationID
	f.snapshot.SourceMatchID = matchID
	f.snapshot.TargetMatchID = 0
	f.snapshot.TargetBound = false
	f.snapshot.Target = placement.Target{}
	return placement.Binding{Version: f.snapshot.Version, OperationID: proof.OperationID,
		SourceMatchID: matchID}, nil
}

type terminalResumeIssuer struct {
	state data.BattleRouteState
	proof placement.BattleExitProof
	err   error
}

func (*terminalResumeIssuer) IssueBattleDSTicketAtCell(context.Context, uint64, uint64, uint32, uint32) (*DSTicketResult, error) {
	return nil, errors.New("not used")
}

func (f *terminalResumeIssuer) InspectBattleRoute(context.Context, uint64, uint64) (data.BattleRouteState, error) {
	return f.state, f.err
}

func (f *terminalResumeIssuer) InspectBattleRouteProof(context.Context, uint64, uint64) (data.BattleRouteState, placement.BattleExitProof, error) {
	return f.state, f.proof, f.err
}

func (f *matchResumeFake) ResolvePlayerMatchContext(context.Context, uint64) (data.MatchResumeContext, error) {
	return f.context, f.err
}

func stableHubPlacement() data.PlacementSnapshot {
	return data.PlacementSnapshot{Found: true,
		CurrentRoute:    locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE,
		Version:         3, OperationID: "123e4567-e89b-42d3-a456-426614174000"}
}

func TestResumeContextUsesTicketHandleBeforeMatchFormation(t *testing.T) {
	for _, stage := range []data.MatchContextStage{data.MatchStageStarting, data.MatchStageQueued} {
		uc := &LoginUsecase{placementMode: placement.ModeEnforce,
			placementChecker: &resumePlacementFake{snapshot: stableHubPlacement()},
			matchResumeReader: &matchResumeFake{context: data.MatchResumeContext{
				State: data.MatchContextActive, Stage: stage, TicketID: 701,
			}}}
		got, err := uc.resumeContextForPlayer(context.Background(), 9)
		if err != nil {
			t.Fatal(err)
		}
		if got.MatchID != 701 || got.Route != loginv1.ResumeRoute_RESUME_ROUTE_HUB ||
			got.MatchStage != loginv1.ResumeMatchStage_RESUME_MATCH_STAGE_QUEUED {
			t.Fatalf("stage=%v resume=%+v", stage, got)
		}
	}
}

func TestResumeContextConfirmingAndReadyUseCanonicalMatchID(t *testing.T) {
	t.Run("confirming", func(t *testing.T) {
		uc := &LoginUsecase{placementMode: placement.ModeEnforce,
			placementChecker: &resumePlacementFake{snapshot: stableHubPlacement()},
			matchResumeReader: &matchResumeFake{context: data.MatchResumeContext{
				State: data.MatchContextActive, Stage: data.MatchStageConfirming,
				TicketID: 701, MatchID: 801,
			}}}
		got, err := uc.resumeContextForPlayer(context.Background(), 9)
		if err != nil || got.MatchID != 801 ||
			got.MatchStage != loginv1.ResumeMatchStage_RESUME_MATCH_STAGE_CONFIRMING {
			t.Fatalf("resume=%+v err=%v", got, err)
		}
	})

	t.Run("ready-and-running", func(t *testing.T) {
		for _, stable := range []bool{false, true} {
			snapshot := data.PlacementSnapshot{Found: true, Version: 4,
				OperationID: "123e4567-e89b-42d3-a456-426614174000", TargetBound: true}
			if stable {
				snapshot.CurrentRoute = locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE
				snapshot.TransitionState = locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE
				snapshot.MatchID = 801
			} else {
				snapshot.TargetRoute = locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE
				snapshot.TransitionState = locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING
				snapshot.TargetMatchID = 801
			}
			uc := &LoginUsecase{placementMode: placement.ModeEnforce,
				placementChecker: &resumePlacementFake{snapshot: snapshot},
				matchResumeReader: &matchResumeFake{context: data.MatchResumeContext{
					State: data.MatchContextActive, Stage: data.MatchStageReady,
					TicketID: 701, MatchID: 801, PlacementVersion: 4,
					PlacementOperationID: snapshot.OperationID,
				}}}
			got, err := uc.resumeContextForPlayer(context.Background(), 9)
			wantStage := loginv1.ResumeMatchStage_RESUME_MATCH_STAGE_READY
			if stable {
				wantStage = loginv1.ResumeMatchStage_RESUME_MATCH_STAGE_RUNNING
			}
			if err != nil || got.MatchID != 801 || got.Route != loginv1.ResumeRoute_RESUME_ROUTE_BATTLE ||
				got.MatchStage != wantStage {
				t.Fatalf("stable=%v resume=%+v err=%v", stable, got, err)
			}
		}
	})
}

func TestResumeContextAuthorityErrorIsUnknownNotHub(t *testing.T) {
	uc := &LoginUsecase{placementMode: placement.ModeEnforce,
		placementChecker:  &resumePlacementFake{snapshot: stableHubPlacement()},
		matchResumeReader: &matchResumeFake{err: errors.New("redis timeout")}}
	got, err := uc.resumeContextForPlayer(context.Background(), 9)
	if err == nil || got.Route != loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN {
		t.Fatalf("resume=%+v err=%v", got, err)
	}
}

func TestAuthoritativeLoginResumeConsumesExactTerminalProofAfterMatchRelease(t *testing.T) {
	proof := placement.BattleExitProof{ExpectedVersion: 6,
		OperationID: "223e4567-e89b-42d3-a456-426614174001",
		ProofType:   placement.ProofMatchTerminal, ProofID: "result:801", Signature: "signed"}
	checker := &terminalRecoveryPlacementFake{resumePlacementFake: resumePlacementFake{snapshot: data.PlacementSnapshot{
		Found: true, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE,
		Version:         6, OperationID: "123e4567-e89b-42d3-a456-426614174000", MatchID: 801,
		Target: recoveryBattleTarget(),
	}}}
	departure := &battleDepartureFake{checker: checker}
	uc := &LoginUsecase{placementMode: placement.ModeEnforce, placementChecker: checker,
		matchResumeReader:  &matchResumeFake{context: data.MatchResumeContext{State: data.MatchContextNone}},
		battleTicketIssuer: &terminalResumeIssuer{state: data.BattleRouteTerminal, proof: proof},
		battleDeparture:    departure}

	got, err := uc.authoritativeLoginResume(context.Background(), 9)
	if err != nil {
		t.Fatal(err)
	}
	if got.Route != loginv1.ResumeRoute_RESUME_ROUTE_HUB || got.MatchID != 801 ||
		got.PlacementVersion != 7 || got.OperationID != proof.OperationID || checker.beginCalls != 1 ||
		checker.beginMatch != 801 || checker.beginProof != proof || departure.calls != 1 ||
		departure.transition.Version != 7 || departure.transition.OperationID != proof.OperationID ||
		departure.source.Version != 6 || departure.source.OperationID != "123e4567-e89b-42d3-a456-426614174000" ||
		departure.target != recoveryBattleTarget() {
		t.Fatalf("resume=%+v checker=%+v", got, checker)
	}
}

func TestAuthoritativeLoginResumeCancelsPendingBattleAfterMatchRelease(t *testing.T) {
	proof := placement.BattleExitProof{ExpectedVersion: 7,
		OperationID: "223e4567-e89b-42d3-a456-426614174001",
		ProofType:   placement.ProofMatchTerminal, ProofID: "result:801", Signature: "signed"}
	checker := &terminalRecoveryPlacementFake{resumePlacementFake: resumePlacementFake{snapshot: data.PlacementSnapshot{
		Found: true, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		Version:         7, OperationID: "123e4567-e89b-42d3-a456-426614174000",
		TargetMatchID: 801, TargetBound: true, Target: recoveryBattleTarget(),
		SourceBinding: placement.Binding{Version: 6, OperationID: "023e4567-e89b-42d3-a456-426614174000"},
		SourceTarget:  recoveryHubTarget(),
	}}}
	uc := &LoginUsecase{placementMode: placement.ModeEnforce, placementChecker: checker,
		matchResumeReader:  &matchResumeFake{context: data.MatchResumeContext{State: data.MatchContextNone}},
		battleTicketIssuer: &terminalResumeIssuer{state: data.BattleRouteTerminal, proof: proof}}

	got, err := uc.authoritativeLoginResume(context.Background(), 9)
	if err != nil {
		t.Fatal(err)
	}
	if got.Route != loginv1.ResumeRoute_RESUME_ROUTE_HUB || got.MatchID != 801 ||
		got.PlacementVersion != 8 || got.OperationID != proof.OperationID || checker.beginCalls != 1 ||
		checker.beginMatch != 801 || checker.beginProof != proof ||
		checker.snapshot.CurrentRoute != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB ||
		checker.snapshot.TargetRoute != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB {
		t.Fatalf("pending Battle was not redirected: resume=%+v checker=%+v", got, checker)
	}
}

func TestGetResumeContextConsumesExactTerminalProofWithoutFullLogin(t *testing.T) {
	cfg := auth.Config{Secret: []byte(testSecret)}
	signer, err := auth.NewSigner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := auth.NewVerifier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := signer.SignSession(9, "resume-terminal-proof-jti")
	if err != nil {
		t.Fatal(err)
	}
	proof := placement.BattleExitProof{ExpectedVersion: 6,
		OperationID: "223e4567-e89b-42d3-a456-426614174001",
		ProofType:   placement.ProofMatchTerminal, ProofID: "result:801", Signature: "signed"}
	checker := &terminalRecoveryPlacementFake{resumePlacementFake: resumePlacementFake{snapshot: data.PlacementSnapshot{
		Found: true, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE,
		Version:         6, OperationID: "123e4567-e89b-42d3-a456-426614174000", MatchID: 801,
		Target: recoveryBattleTarget(),
	}}}
	departure := &battleDepartureFake{checker: checker}
	uc := &LoginUsecase{verifier: verifier, placementMode: placement.ModeEnforce, placementChecker: checker,
		matchResumeReader:  &matchResumeFake{context: data.MatchResumeContext{State: data.MatchContextNone}},
		battleTicketIssuer: &terminalResumeIssuer{state: data.BattleRouteTerminal, proof: proof},
		battleDeparture:    departure}

	got, err := uc.GetResumeContext(context.Background(), session)
	if err != nil || got.Route != loginv1.ResumeRoute_RESUME_ROUTE_HUB || got.MatchID != 801 ||
		got.PlacementVersion != 7 || got.OperationID != proof.OperationID || checker.beginCalls != 1 {
		t.Fatalf("resume=%+v begin_calls=%d err=%v", got, checker.beginCalls, err)
	}
}

func TestAuthoritativeLoginResumeRenewsPendingBattleExitBeforeHubAssignment(t *testing.T) {
	proof := placement.BattleExitProof{ExpectedVersion: 6,
		OperationID: "223e4567-e89b-42d3-a456-426614174001",
		ProofType:   placement.ProofMatchTerminal, ProofID: "result:801", Signature: "signed"}
	checker := &terminalRecoveryPlacementFake{resumePlacementFake: resumePlacementFake{snapshot: data.PlacementSnapshot{
		Found: true, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		Version:         7, OperationID: proof.OperationID, SourceMatchID: 801,
		SourceBinding: placement.Binding{Version: 6, OperationID: "123e4567-e89b-42d3-a456-426614174000"},
		SourceTarget:  recoveryBattleTarget(),
	}}}
	departure := &battleDepartureFake{checker: checker}
	uc := &LoginUsecase{placementMode: placement.ModeEnforce, placementChecker: checker,
		matchResumeReader:  &matchResumeFake{context: data.MatchResumeContext{State: data.MatchContextNone}},
		battleTicketIssuer: &terminalResumeIssuer{state: data.BattleRouteTerminal, proof: proof},
		battleDeparture:    departure}

	got, err := uc.authoritativeLoginResume(context.Background(), 9)
	if err != nil || got.Route != loginv1.ResumeRoute_RESUME_ROUTE_HUB || got.MatchID != 801 ||
		got.PlacementVersion != 7 || got.OperationID != proof.OperationID || checker.beginCalls != 1 ||
		checker.beginMatch != 801 || checker.beginProof != proof {
		t.Fatalf("resume=%+v checker=%+v err=%v", got, checker, err)
	}
}

func TestAuthoritativeLoginResumeRenewsExactPendingAccountBootstrap(t *testing.T) {
	const (
		playerID = uint64(9)
		opID     = "123e4567-e89b-42d3-a456-426614174000"
		proofID  = "account-created:9"
		secret   = "0123456789abcdef0123456789abcdef"
	)
	checker := &bootstrapRecoveryPlacementFake{resumePlacementFake: resumePlacementFake{
		snapshot: data.PlacementSnapshot{Found: true,
			TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
			TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
			Version:         1, OperationID: opID,
			LeaseDeadlineMs: time.Now().Add(-time.Hour).UnixMilli(),
			ProofType:       locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_ACCOUNT_BOOTSTRAP,
			ProofID:         proofID,
		}}}
	signer, err := placement.NewProofSigner(secret)
	if err != nil {
		t.Fatal(err)
	}
	uc := &LoginUsecase{placementMode: placement.ModeEnforce, placementChecker: checker,
		placementProofSigner: signer,
		matchResumeReader:    &matchResumeFake{context: data.MatchResumeContext{State: data.MatchContextNone}}}

	before := time.Now().UnixMilli()
	got, err := uc.authoritativeLoginResume(context.Background(), playerID)
	wantSig := signer.Sign(placement.Proof{PlayerID: playerID, TargetRoute: placement.RouteHub,
		ProofType: placement.ProofAccountBootstrap, ProofID: proofID, OperationID: opID})
	if err != nil || got.Route != loginv1.ResumeRoute_RESUME_ROUTE_HUB || got.PlacementVersion != 1 ||
		got.OperationID != opID || checker.bootstrapCalls != 1 || checker.bootstrapPlayer != playerID ||
		checker.bootstrapOp != opID || checker.bootstrapProof != proofID || checker.bootstrapSig != wantSig ||
		checker.bootstrapLease <= before {
		t.Fatalf("resume=%+v checker=%+v err=%v", got, checker, err)
	}
}

func TestAuthoritativeLoginResumeDoesNotBootstrapRenewHubTransfer(t *testing.T) {
	checker := &bootstrapRecoveryPlacementFake{resumePlacementFake: resumePlacementFake{
		snapshot: data.PlacementSnapshot{Found: true,
			CurrentRoute:    locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
			TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
			TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
			Version:         2, OperationID: "123e4567-e89b-42d3-a456-426614174000",
			LeaseDeadlineMs: time.Now().Add(-time.Hour).UnixMilli(),
			ProofType:       locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_HUB_TRANSFER,
			ProofID:         "hub-transfer:assignment-1",
		}}}
	signer, err := placement.NewProofSigner("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	uc := &LoginUsecase{placementMode: placement.ModeEnforce, placementChecker: checker,
		placementProofSigner: signer,
		matchResumeReader:    &matchResumeFake{context: data.MatchResumeContext{State: data.MatchContextNone}}}

	got, err := uc.authoritativeLoginResume(context.Background(), 9)
	if err != nil || got.Route != loginv1.ResumeRoute_RESUME_ROUTE_HUB || checker.bootstrapCalls != 0 {
		t.Fatalf("Hub-transfer must remain allocator-owned: resume=%+v bootstrap_calls=%d err=%v",
			got, checker.bootstrapCalls, err)
	}
}

func TestAuthoritativeLoginResumeNeverCollapsesMissingExitProofToHub(t *testing.T) {
	checker := &terminalRecoveryPlacementFake{resumePlacementFake: resumePlacementFake{snapshot: data.PlacementSnapshot{
		Found: true, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE,
		Version:         6, OperationID: "123e4567-e89b-42d3-a456-426614174000", MatchID: 801,
	}}}
	uc := &LoginUsecase{placementMode: placement.ModeEnforce, placementChecker: checker,
		matchResumeReader: &matchResumeFake{context: data.MatchResumeContext{State: data.MatchContextNone}},
		battleTicketIssuer: &terminalResumeIssuer{state: data.BattleRouteUnknown,
			err: errors.New("proof unavailable")}}

	got, err := uc.authoritativeLoginResume(context.Background(), 9)
	if err == nil || got.Route != loginv1.ResumeRoute_RESUME_ROUTE_UNKNOWN || checker.beginCalls != 0 {
		t.Fatalf("resume=%+v begin_calls=%d err=%v", got, checker.beginCalls, err)
	}
}
