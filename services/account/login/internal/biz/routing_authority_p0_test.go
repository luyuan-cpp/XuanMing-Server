package biz

import (
	"context"
	"errors"
	"testing"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
	loginv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/login/v1"
	"github.com/luyuancpp/pandora/services/account/login/internal/data"
)

type p0PlacementFake struct {
	snapshot   data.PlacementSnapshot
	beginCalls int
}

func (f *p0PlacementFake) GetPlacement(context.Context, uint64) (data.PlacementSnapshot, error) {
	return f.snapshot, nil
}
func (*p0PlacementFake) CheckHubAdmission(context.Context, data.HubPlacementAdmission) error {
	return nil
}
func (*p0PlacementFake) BootstrapHub(context.Context, uint64, string, string, string, int64) (placement.Binding, error) {
	return placement.Binding{}, nil
}
func (f *p0PlacementFake) BeginHubFromBattle(context.Context, uint64, uint64, placement.BattleExitProof, int64) (placement.Binding, error) {
	f.beginCalls++
	return placement.Binding{}, errors.New("unexpected Battle->Hub transition")
}
func (*p0PlacementFake) CommitBattleAdmission(context.Context, data.BattlePlacementAdmission) error {
	return nil
}

type p0MatchReader struct {
	resume data.MatchResumeContext
	err    error
}

func (f *p0MatchReader) ResolvePlayerMatchContext(context.Context, uint64) (data.MatchResumeContext, error) {
	return f.resume, f.err
}

type p0RoleRepo struct{ setCalls int }

func (*p0RoleRepo) GetRole(context.Context, uint64) (uint32, error) { return 0, nil }
func (f *p0RoleRepo) SetRole(context.Context, uint64, uint32) error {
	f.setCalls++
	return nil
}

func p0StableHub() data.PlacementSnapshot {
	return data.PlacementSnapshot{
		Found: true, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE,
		Version:         5, OperationID: "123e4567-e89b-42d3-a456-426614174000",
	}
}

func p0StableBattle(matchID uint64) data.PlacementSnapshot {
	return data.PlacementSnapshot{
		Found: true, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE,
		Version:         7, OperationID: "223e4567-e89b-42d3-a456-426614174001", MatchID: matchID,
	}
}

func TestCanonicalReadyBattleResumeCarriesExactPlacementBinding(t *testing.T) {
	const playerID, matchID = uint64(42), uint64(9001)
	v2Signer, v2Verifier := newV2TicketPair(t)
	snapshot := p0StableBattle(matchID)
	binding := placement.Binding{Version: snapshot.Version, OperationID: snapshot.OperationID}
	ticket, _, err := v2Signer.SignBattleTicket(playerID, 0, 0, "canonical-ready-jti",
		auth.DSTicketTarget{
			DSPodName: "battle-1", DSInstanceUID: "battle-uid-1", DSInstanceEpoch: 9,
			ReleaseTrack: auth.ReleaseTrackStable, MatchID: matchID, AllocationID: "alloc-1",
			Placement: binding,
		})
	if err != nil {
		t.Fatal(err)
	}
	uc := newTestUsecase(t, nil)
	uc.v2Verifier = v2Verifier
	uc.placementMode = placement.ModeEnforce
	uc.placementChecker = &p0PlacementFake{snapshot: snapshot}
	uc.matchResumeReader = &p0MatchReader{resume: data.MatchResumeContext{
		State: data.MatchContextActive, Stage: data.MatchStageReady, TicketID: 7001, MatchID: matchID,
		DSAddr: "10.0.0.8:7777", BattleTicket: ticket,
		PlacementVersion: binding.Version, PlacementOperationID: binding.OperationID,
	}}

	resume, err := uc.resumeContextForPlayer(context.Background(), playerID)
	if err != nil || resume.Route != loginv1.ResumeRoute_RESUME_ROUTE_BATTLE ||
		resume.MatchStage != loginv1.ResumeMatchStage_RESUME_MATCH_STAGE_RUNNING {
		t.Fatalf("resume=%+v err=%v", resume, err)
	}
	result, err := uc.buildPlacementBattleLogin(context.Background(), playerID, "device-1", "session", 1, 0, 0, resume)
	if err != nil {
		t.Fatal(err)
	}
	if result.BattleDSAddr != "10.0.0.8:7777" || result.BattleTicket != ticket || result.BattleTicketExpMs == 0 {
		t.Fatalf("Battle reconnect result=%+v", result)
	}
	claims, err := v2Verifier.Verify(result.BattleTicket)
	if err != nil || claims.PlacementVersion != binding.Version ||
		claims.PlacementOperationID != binding.OperationID || claims.MatchID != matchID {
		t.Fatalf("claims=%+v err=%v", claims, err)
	}
	addr, issued, expMs, err := uc.ResolveBattleEndpoint(context.Background(), playerID, matchID)
	if err != nil || addr != "10.0.0.8:7777" || issued != ticket || expMs == 0 {
		t.Fatalf("IssueDSTicket(battle) canonical route addr=%q ticket=%v exp=%d err=%v",
			addr, issued == ticket, expMs, err)
	}
}

func TestCanonicalReadyBattleResumeRejectsTicketWithAnotherPlacement(t *testing.T) {
	v2Signer, v2Verifier := newV2TicketPair(t)
	ticket, _, err := v2Signer.SignBattleTicket(42, 0, 0, "stale-placement-jti", auth.DSTicketTarget{
		DSPodName: "battle-1", DSInstanceUID: "battle-uid-1", DSInstanceEpoch: 9,
		ReleaseTrack: auth.ReleaseTrackStable, MatchID: 9001, AllocationID: "alloc-1",
		Placement: placement.Binding{Version: 8, OperationID: "323e4567-e89b-42d3-a456-426614174002"},
	})
	if err != nil {
		t.Fatal(err)
	}
	uc := newTestUsecase(t, nil)
	uc.v2Verifier = v2Verifier
	_, _, _, err = uc.verifyCanonicalBattleResumeTicket(42, ResumeContextResult{
		Route: loginv1.ResumeRoute_RESUME_ROUTE_BATTLE, MatchID: 9001,
		MatchStage:       loginv1.ResumeMatchStage_RESUME_MATCH_STAGE_READY,
		PlacementVersion: 7, OperationID: "223e4567-e89b-42d3-a456-426614174001",
		battleDSAddr: "10.0.0.8:7777", battleTicket: ticket,
	})
	if errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("stale placement ticket code=%v err=%v", errcode.As(err), err)
	}
}

func TestHubEntrypointsRejectActiveOrUnknownBeforeAnySideEffect(t *testing.T) {
	const matchID = uint64(9001)
	cases := []struct {
		name      string
		placement data.PlacementSnapshot
		match     data.MatchResumeContext
		matchErr  error
	}{
		{name: "allocating", placement: p0StableHub(), match: data.MatchResumeContext{
			State: data.MatchContextActive, Stage: data.MatchStageAllocating, TicketID: 7001, MatchID: matchID}},
		{name: "ready-running", placement: p0StableBattle(matchID), match: data.MatchResumeContext{
			State: data.MatchContextActive, Stage: data.MatchStageReady, TicketID: 7001, MatchID: matchID,
			PlacementVersion: 7, PlacementOperationID: "223e4567-e89b-42d3-a456-426614174001"}},
		{name: "unknown", placement: p0StableHub(), matchErr: errors.New("canonical Redis timeout")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := &fakeHubAssigner{res: &data.HubAssignment{HubDSAddr: "should-not-be-called"}}
			checker := &p0PlacementFake{snapshot: tc.placement}
			uc := newTestUsecase(t, hub)
			uc.placementMode = placement.ModeEnforce
			uc.placementChecker = checker
			uc.matchResumeReader = &p0MatchReader{resume: tc.match, err: tc.matchErr}
			roles := &p0RoleRepo{}
			uc.roleRepo, uc.devAllowAnyRole = roles, true

			addr, ticket, _, issueErr := uc.ResolveHubEndpoint(context.Background(), 42)
			if issueErr == nil || addr != "" || ticket != "" {
				t.Fatalf("IssueDSTicket(hub) addr=%q ticket=%q err=%v", addr, ticket, issueErr)
			}
			addr, ticket, _, roleErr := uc.SelectRole(context.Background(), 42, 1)
			if roleErr == nil || addr != "" || ticket != "" {
				t.Fatalf("SelectRole addr=%q ticket=%q err=%v", addr, ticket, roleErr)
			}
			if hub.gotPlayerID != 0 || roles.setCalls != 0 || checker.beginCalls != 0 {
				t.Fatalf("rejected route produced side effects: assign=%d role=%d begin=%d",
					hub.gotPlayerID, roles.setCalls, checker.beginCalls)
			}
		})
	}
}

func TestHubEntryAllowsEarlyMatchOnlyOnStableHub(t *testing.T) {
	for _, stage := range []data.MatchContextStage{data.MatchStageQueued, data.MatchStageConfirming} {
		match := data.MatchResumeContext{State: data.MatchContextActive, Stage: stage, TicketID: 7001}
		if stage == data.MatchStageConfirming {
			match.MatchID = 9001
		}
		uc := newTestUsecase(t, nil)
		uc.placementMode = placement.ModeEnforce
		uc.placementChecker = &p0PlacementFake{snapshot: p0StableHub()}
		uc.matchResumeReader = &p0MatchReader{resume: match}
		if _, err := uc.authorizeHubEntry(context.Background(), 42, 0, false); err != nil {
			t.Fatalf("stage=%v stable Hub should pass: %v", stage, err)
		}

		pending := p0StableHub()
		pending.CurrentRoute = locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE
		pending.TargetRoute = locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB
		pending.TransitionState = locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING
		uc.placementChecker = &p0PlacementFake{snapshot: pending}
		if _, err := uc.authorizeHubEntry(context.Background(), 42, 0, false); err == nil {
			t.Fatalf("stage=%v pending Hub must not authorize early-match Hub side effects", stage)
		}
	}
}
