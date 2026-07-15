package biz

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
)

type memoryPlacementRepo struct {
	mu                  sync.Mutex
	rec                 *locatorv1.PlayerPlacementStorageRecord
	battleTerminalFence map[uint64]bool
}

func (r *memoryPlacementRepo) GetPlacement(context.Context, uint64) (*locatorv1.PlayerPlacementStorageRecord, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rec == nil {
		return nil, false, nil
	}
	return proto.Clone(r.rec).(*locatorv1.PlayerPlacementStorageRecord), true, nil
}

func (r *memoryPlacementRepo) UpdatePlacement(_ context.Context, _ uint64, _ int,
	mutate func(*locatorv1.PlayerPlacementStorageRecord, bool) (*locatorv1.PlayerPlacementStorageRecord, error),
) (*locatorv1.PlayerPlacementStorageRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.rec
	found := cur != nil
	if cur != nil {
		cur = proto.Clone(cur).(*locatorv1.PlayerPlacementStorageRecord)
	}
	next, err := mutate(cur, found)
	if err != nil {
		return nil, err
	}
	r.rec = proto.Clone(next).(*locatorv1.PlayerPlacementStorageRecord)
	return proto.Clone(next).(*locatorv1.PlayerPlacementStorageRecord), nil
}

func (r *memoryPlacementRepo) UpdatePlacementWithBattleTerminalFence(ctx context.Context, playerID, matchID uint64, retry int,
	mutate func(*locatorv1.PlayerPlacementStorageRecord, bool) (*locatorv1.PlayerPlacementStorageRecord, error),
) (*locatorv1.PlayerPlacementStorageRecord, error) {
	r.mu.Lock()
	blocked := r.battleTerminalFence[matchID]
	r.mu.Unlock()
	if blocked {
		return nil, errcode.New(errcode.ErrInvalidState, "terminal fence")
	}
	return r.UpdatePlacement(ctx, playerID, retry, mutate)
}

func mustPlacementSigner(t *testing.T) *placement.ProofSigner {
	t.Helper()
	signer, err := placement.NewProofSigner("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func TestBeginSameSignedOperationRenewsExpiredPendingLease(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	signer := mustPlacementSigner(t)
	op := "123e4567-e89b-42d3-a456-426614174000"
	proofID := "match-start:90"
	repo := &memoryPlacementRepo{rec: &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: 7, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		Version:         4, OperationId: op, TargetMatchId: 90,
		ProofType: locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_START,
		ProofId:   proofID, LeaseDeadlineMs: now.Add(-time.Minute).UnixMilli(),
	}}
	uc := NewPlacementUsecase(repo, signer)
	uc.now = func() time.Time { return now }
	in := BeginPlacementInput{PlayerID: 7, ExpectedVersion: 3,
		TargetRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE, OperationID: op,
		TargetMatchID: 90, ProofType: locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_START,
		ProofID: proofID, LeaseDeadlineMs: now.Add(10 * time.Minute).UnixMilli()}
	in.ProofSignature = signer.Sign(placement.Proof{PlayerID: 7, ExpectedVersion: 3,
		SourceRoute: placement.RouteHub, TargetRoute: placement.RouteBattle, TargetMatchID: 90,
		ProofType: placement.ProofMatchStart, ProofID: proofID, OperationID: op})

	got, err := uc.Begin(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetVersion() != 4 || got.GetOperationId() != op ||
		got.GetLeaseDeadlineMs() != in.LeaseDeadlineMs {
		t.Fatalf("renewed placement = %+v", got)
	}

	before := got.GetLeaseDeadlineMs()
	in.ProofSignature = "not-a-signature"
	in.LeaseDeadlineMs = now.Add(20 * time.Minute).UnixMilli()
	if _, err := uc.Begin(context.Background(), in); errcode.As(err) != errcode.ErrPermissionDeny {
		t.Fatalf("invalid proof code=%v err=%v", errcode.As(err), err)
	}
	stored, _, _ := repo.GetPlacement(context.Background(), 7)
	if stored.GetLeaseDeadlineMs() != before {
		t.Fatalf("invalid proof renewed lease: got=%d want=%d", stored.GetLeaseDeadlineMs(), before)
	}
}

func TestBindExactTargetRenewsExpiredLeaseButStaleIdentityCannot(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	op := "123e4567-e89b-42d3-a456-426614174000"
	target := placement.Target{PodName: "hub-a", InstanceUID: "uid-a", InstanceEpoch: 2,
		AssignmentID: "assignment-a", ReleaseTrack: "stable"}
	repo := &memoryPlacementRepo{rec: &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: 7, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE, MatchId: 90,
		TargetRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		Version: 7, OperationId: op, SourceMatchId: 90,
		DsPodName: target.PodName, DsInstanceUid: target.InstanceUID,
		DsInstanceEpoch: target.InstanceEpoch, HubAssignmentId: target.AssignmentID,
		ReleaseTrack: target.ReleaseTrack, LeaseDeadlineMs: now.Add(-time.Minute).UnixMilli(),
	}}
	uc := NewPlacementUsecase(repo)
	uc.now = func() time.Time { return now }
	renewal := now.Add(10 * time.Minute).UnixMilli()
	in := BindPlacementInput{PlayerID: 7, Version: 7, OperationID: op,
		TargetRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		PodName: target.PodName, InstanceUID: target.InstanceUID, InstanceEpoch: target.InstanceEpoch,
		AssignmentID: target.AssignmentID, ReleaseTrack: target.ReleaseTrack, LeaseDeadlineMs: renewal}
	got, err := uc.Bind(context.Background(), in)
	if err != nil || got.GetLeaseDeadlineMs() != renewal || !recordTarget(got).Equal(target) {
		t.Fatalf("exact target renewal=%+v err=%v", got, err)
	}

	in.OperationID = "223e4567-e89b-42d3-a456-426614174001"
	in.LeaseDeadlineMs = now.Add(20 * time.Minute).UnixMilli()
	if _, err := uc.Bind(context.Background(), in); errcode.As(err) != errcode.ErrLocatorConflict {
		t.Fatalf("stale operation renewed lease: code=%v err=%v", errcode.As(err), err)
	}
	stored, _, _ := repo.GetPlacement(context.Background(), 7)
	if stored.GetLeaseDeadlineMs() != renewal {
		t.Fatalf("stale operation changed deadline: got=%d want=%d", stored.GetLeaseDeadlineMs(), renewal)
	}
}

func TestCommitStableExactTargetAllowsNewAdmissionID(t *testing.T) {
	for _, tc := range []struct {
		name   string
		route  locatorv1.PlacementRoute
		match  uint64
		target placement.Target
	}{
		{name: "hub", route: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
			target: placement.Target{PodName: "hub-1", InstanceUID: "hub-uid", InstanceEpoch: 3,
				AssignmentID: "assignment-1", ReleaseTrack: "stable"}},
		{name: "battle", route: locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE, match: 91,
			target: placement.Target{PodName: "battle-1", InstanceUID: "battle-uid", InstanceEpoch: 4,
				AllocationID: "allocation-1", ReleaseTrack: "canary"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := "123e4567-e89b-42d3-a456-426614174000"
			repo := &memoryPlacementRepo{rec: &locatorv1.PlayerPlacementStorageRecord{
				PlayerId: 8, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
				TargetRoute:     tc.route,
				TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
				Version:         5, OperationId: op, TargetMatchId: tc.match,
				DsPodName: tc.target.PodName, DsInstanceUid: tc.target.InstanceUID,
				DsInstanceEpoch: tc.target.InstanceEpoch, HubAssignmentId: tc.target.AssignmentID,
				AllocationId: tc.target.AllocationID, ReleaseTrack: tc.target.ReleaseTrack,
				LeaseDeadlineMs: time.Now().Add(time.Hour).UnixMilli(),
			}}
			uc := NewPlacementUsecase(repo)
			bind := BindPlacementInput{PlayerID: 8, Version: 5, OperationID: op,
				TargetRoute: tc.route, PodName: tc.target.PodName, InstanceUID: tc.target.InstanceUID,
				AssignmentID: tc.target.AssignmentID, TargetMatchID: tc.match,
				InstanceEpoch: tc.target.InstanceEpoch, AllocationID: tc.target.AllocationID,
				ReleaseTrack: tc.target.ReleaseTrack}
			firstID := "123e4567-e89b-42d3-a456-426614174001"
			if _, err := uc.Commit(context.Background(), CommitPlacementInput{
				BindPlacementInput: bind, AdmissionID: firstID}); err != nil {
				t.Fatal(err)
			}
			secondID := "123e4567-e89b-42d3-a456-426614174002"
			got, err := uc.Commit(context.Background(), CommitPlacementInput{
				BindPlacementInput: bind, AdmissionID: secondID})
			if err != nil {
				t.Fatalf("same target new admission must recover: %v", err)
			}
			if got.GetAdmissionId() != firstID || got.GetTransitionState() !=
				locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE {
				t.Fatalf("stable audit identity mutated: %+v", got)
			}
		})
	}
}

func TestTerminalProofCancelsPendingBattleAndAtomicallyInvalidatesOldTicket(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	signer := mustPlacementSigner(t)
	oldOperation := "123e4567-e89b-42d3-a456-426614174000"
	terminalOperation := "223e4567-e89b-42d3-a456-426614174001"
	matchID := uint64(90)
	repo := &memoryPlacementRepo{rec: &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: 7, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		Version:         5, OperationId: oldOperation, TargetMatchId: matchID,
		ProofType: locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_START,
		ProofId:   "match-start:90", LeaseDeadlineMs: now.Add(time.Hour).UnixMilli(),
		DsPodName: "battle-90", DsInstanceUid: "battle-uid-90", DsInstanceEpoch: 3,
		AllocationId: "allocation-90", ReleaseTrack: "stable",
	}}
	uc := NewPlacementUsecase(repo, signer)
	uc.now = func() time.Time { return now }
	in := BeginPlacementInput{PlayerID: 7, ExpectedVersion: 5,
		TargetRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		OperationID: terminalOperation, SourceMatchID: matchID,
		ProofType: locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_TERMINAL,
		ProofID:   "result:90:match:90", LeaseDeadlineMs: now.Add(10 * time.Minute).UnixMilli()}
	in.ProofSignature = signer.Sign(placement.Proof{PlayerID: 7, ExpectedVersion: 5,
		SourceRoute: placement.RouteBattle, TargetRoute: placement.RouteHub, SourceMatchID: matchID,
		ProofType: placement.ProofMatchTerminal, ProofID: in.ProofID, OperationID: terminalOperation})

	got, err := uc.Begin(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetVersion() != 6 || got.GetOperationId() != terminalOperation ||
		got.GetCurrentRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB ||
		got.GetTargetRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB ||
		got.GetTransitionState() != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING ||
		got.GetSourceMatchId() != matchID || got.GetTargetMatchId() != 0 ||
		got.GetDsPodName() != "" || got.GetDsInstanceUid() != "" || got.GetAllocationId() != "" {
		t.Fatalf("terminal cancellation did not replace exact target: %+v", got)
	}

	// Lost Begin response is recovered by the exact proof/op, including lease
	// renewal, while the old version-bound Battle admission can no longer commit.
	in.LeaseDeadlineMs = now.Add(20 * time.Minute).UnixMilli()
	replayed, err := uc.Begin(context.Background(), in)
	if err != nil || replayed.GetVersion() != 6 || replayed.GetLeaseDeadlineMs() != in.LeaseDeadlineMs {
		t.Fatalf("exact terminal replay=%+v err=%v", replayed, err)
	}
	_, err = uc.Commit(context.Background(), CommitPlacementInput{
		BindPlacementInput: BindPlacementInput{PlayerID: 7, Version: 5, OperationID: oldOperation,
			TargetRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
			PodName:     "battle-90", InstanceUID: "battle-uid-90", InstanceEpoch: 3,
			AllocationID: "allocation-90", TargetMatchID: matchID, ReleaseTrack: "stable"},
		AdmissionID: "323e4567-e89b-42d3-a456-426614174002",
	})
	if errcode.As(err) != errcode.ErrLocatorConflict {
		t.Fatalf("old Battle ticket/version was not fenced: code=%v err=%v", errcode.As(err), err)
	}
}

func TestTerminalProofCannotCancelAnotherPendingBattle(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	signer := mustPlacementSigner(t)
	repo := &memoryPlacementRepo{rec: &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: 7, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		Version:         5, OperationId: "123e4567-e89b-42d3-a456-426614174000", TargetMatchId: 91,
		ProofType: locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_START,
		ProofId:   "match-start:91", LeaseDeadlineMs: now.Add(time.Hour).UnixMilli(),
	}}
	uc := NewPlacementUsecase(repo, signer)
	uc.now = func() time.Time { return now }
	in := BeginPlacementInput{PlayerID: 7, ExpectedVersion: 5,
		TargetRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		OperationID: "223e4567-e89b-42d3-a456-426614174001", SourceMatchID: 90,
		ProofType: locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_TERMINAL,
		ProofID:   "result:90:match:90", LeaseDeadlineMs: now.Add(10 * time.Minute).UnixMilli()}
	in.ProofSignature = signer.Sign(placement.Proof{PlayerID: 7, ExpectedVersion: 5,
		SourceRoute: placement.RouteBattle, TargetRoute: placement.RouteHub, SourceMatchID: 90,
		ProofType: placement.ProofMatchTerminal, ProofID: in.ProofID, OperationID: in.OperationID})
	if _, err := uc.Begin(context.Background(), in); errcode.As(err) != errcode.ErrLocatorConflict {
		t.Fatalf("wrong match cancellation code=%v err=%v", errcode.As(err), err)
	}
	stored, _, _ := repo.GetPlacement(context.Background(), 7)
	if stored.GetVersion() != 5 || stored.GetTargetMatchId() != 91 || stored.GetOperationId() != "123e4567-e89b-42d3-a456-426614174000" {
		t.Fatalf("wrong match proof mutated placement: %+v", stored)
	}
}

func TestStableHubVersionFreeTerminalTombstoneBlocksOldBeginBindAndAdmission(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	signer := mustPlacementSigner(t)
	matchID := uint64(90)
	operationID := "123e4567-e89b-42d3-a456-426614174000"
	repo := &memoryPlacementRepo{
		rec: &locatorv1.PlayerPlacementStorageRecord{
			PlayerId: 7, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
			TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE,
			Version:         4,
		},
		battleTerminalFence: map[uint64]bool{matchID: true},
	}
	uc := NewPlacementUsecase(repo, signer)
	uc.now = func() time.Time { return now }
	begin := BeginPlacementInput{PlayerID: 7, ExpectedVersion: 4,
		TargetRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		OperationID: operationID, TargetMatchID: matchID,
		ProofType: locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_START,
		ProofID:   "match-start:90", LeaseDeadlineMs: now.Add(time.Hour).UnixMilli()}
	begin.ProofSignature = signer.Sign(placement.Proof{PlayerID: 7, ExpectedVersion: 4,
		SourceRoute: placement.RouteHub, TargetRoute: placement.RouteBattle, TargetMatchID: matchID,
		ProofType: placement.ProofMatchStart, ProofID: begin.ProofID, OperationID: operationID})
	if _, err := uc.Begin(context.Background(), begin); errcode.As(err) != errcode.ErrInvalidState {
		t.Fatalf("terminal-fenced Begin code=%v err=%v", errcode.As(err), err)
	}
	stable, _, _ := repo.GetPlacement(context.Background(), 7)
	if stable.GetVersion() != 4 || stable.GetTransitionState() != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE {
		t.Fatalf("terminal-fenced Begin mutated placement: %+v", stable)
	}

	// Even if Begin won just before the terminal proof was relayed, subsequent
	// target binding and final Admission are independently fenced.
	repo.mu.Lock()
	repo.rec = &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: 7, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		Version:         5, OperationId: operationID, TargetMatchId: matchID,
		LeaseDeadlineMs: now.Add(time.Hour).UnixMilli(),
		ProofType:       locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_START,
		ProofId:         "match-start:90",
	}
	repo.mu.Unlock()
	target := BindPlacementInput{PlayerID: 7, Version: 5, OperationID: operationID,
		TargetRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE, TargetMatchID: matchID,
		PodName: "battle-90", InstanceUID: "battle-uid-90", InstanceEpoch: 3,
		AllocationID: "allocation-90", ReleaseTrack: "stable"}
	if _, err := uc.Bind(context.Background(), target); errcode.As(err) != errcode.ErrInvalidState {
		t.Fatalf("terminal-fenced Bind code=%v err=%v", errcode.As(err), err)
	}

	repo.mu.Lock()
	repo.rec.DsPodName = target.PodName
	repo.rec.DsInstanceUid = target.InstanceUID
	repo.rec.DsInstanceEpoch = target.InstanceEpoch
	repo.rec.AllocationId = target.AllocationID
	repo.rec.ReleaseTrack = target.ReleaseTrack
	repo.mu.Unlock()
	if _, err := uc.Commit(context.Background(), CommitPlacementInput{
		BindPlacementInput: target, AdmissionID: "223e4567-e89b-42d3-a456-426614174001",
	}); errcode.As(err) != errcode.ErrInvalidState {
		t.Fatalf("terminal-fenced Admission code=%v err=%v", errcode.As(err), err)
	}
	stillPending, _, _ := repo.GetPlacement(context.Background(), 7)
	if stillPending.GetTransitionState() != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING ||
		stillPending.GetCurrentRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB {
		t.Fatalf("terminal-fenced Admission committed Battle: %+v", stillPending)
	}
}

func TestRetargetUnavailableHubTargetAdvancesFenceAndIsIdempotent(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	signer := mustPlacementSigner(t)
	oldOperation := "123e4567-e89b-42d3-a456-426614174000"
	newOperation := "223e4567-e89b-42d3-a456-426614174001"
	oldTarget := placement.Target{PodName: "hub-a", InstanceUID: "uid-a", InstanceEpoch: 2,
		AssignmentID: "assignment-a", ReleaseTrack: "stable"}
	newTarget := placement.Target{PodName: "hub-b", InstanceUID: "uid-b", InstanceEpoch: 3,
		AssignmentID: "assignment-b", ReleaseTrack: "stable"}
	repo := &memoryPlacementRepo{rec: &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: 7, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE, MatchId: 90,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		Version:         7, OperationId: oldOperation, SourceMatchId: 90,
		DsPodName: oldTarget.PodName, DsInstanceUid: oldTarget.InstanceUID,
		DsInstanceEpoch: oldTarget.InstanceEpoch, HubAssignmentId: oldTarget.AssignmentID,
		ReleaseTrack: oldTarget.ReleaseTrack, LeaseDeadlineMs: now.Add(time.Minute).UnixMilli(),
		ProofType: locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_TERMINAL,
		ProofId:   "result:90",
	}}
	uc := NewPlacementUsecase(repo, signer)
	uc.now = func() time.Time { return now }
	in := RetargetPlacementInput{PlayerID: 7, Version: 7, OperationID: oldOperation,
		TargetRoute:    locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		ExpectedTarget: oldTarget, ReplacementVersion: 8, ReplacementOperationID: newOperation,
		ReplacementTarget: newTarget,
		ProofType:         locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_HUB_TRANSFER,
		Reason:            locatorv1.PlacementTargetUnavailableReason_PLACEMENT_TARGET_UNAVAILABLE_REASON_INSTANCE_TERMINATED,
		ProofID:           "hub-target-unavailable:uid-a", LeaseDeadlineMs: now.Add(10 * time.Minute).UnixMilli()}
	in.ProofSignature = signer.SignTargetUnavailable(retargetProof(in))

	got, err := uc.Retarget(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetVersion() != 8 || got.GetOperationId() != newOperation ||
		got.GetCurrentRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE || got.GetMatchId() != 90 ||
		got.GetSourceMatchId() != 90 || !recordTarget(got).Equal(newTarget) || got.GetRetargetCount() != 1 {
		t.Fatalf("retarget result = %+v", got)
	}
	in.LeaseDeadlineMs = now.Add(20 * time.Minute).UnixMilli()
	replayed, err := uc.Retarget(context.Background(), in)
	if err != nil || replayed.GetVersion() != 8 || replayed.GetRetargetCount() != 1 ||
		replayed.GetLeaseDeadlineMs() != in.LeaseDeadlineMs {
		t.Fatalf("retarget replay=%+v err=%v", replayed, err)
	}
	_, err = uc.Commit(context.Background(), CommitPlacementInput{BindPlacementInput: BindPlacementInput{
		PlayerID: 7, Version: 7, OperationID: oldOperation,
		TargetRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		PodName:     oldTarget.PodName, InstanceUID: oldTarget.InstanceUID,
		AssignmentID: oldTarget.AssignmentID, InstanceEpoch: oldTarget.InstanceEpoch,
		ReleaseTrack: oldTarget.ReleaseTrack}, AdmissionID: "323e4567-e89b-42d3-a456-426614174002"})
	if errcode.As(err) != errcode.ErrLocatorConflict {
		t.Fatalf("old target ticket was not fenced: code=%v err=%v", errcode.As(err), err)
	}
}

func TestRetargetRejectsUnsignedOrLeaseOnlyTargetChange(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	signer := mustPlacementSigner(t)
	oldTarget := placement.Target{PodName: "hub-a", InstanceUID: "uid-a", InstanceEpoch: 2,
		AssignmentID: "assignment-a", ReleaseTrack: "stable"}
	repo := &memoryPlacementRepo{rec: &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: 7, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		Version:         2, OperationId: "123e4567-e89b-42d3-a456-426614174000",
		DsPodName: oldTarget.PodName, DsInstanceUid: oldTarget.InstanceUID,
		DsInstanceEpoch: oldTarget.InstanceEpoch, HubAssignmentId: oldTarget.AssignmentID,
		ReleaseTrack: oldTarget.ReleaseTrack, LeaseDeadlineMs: now.Add(-time.Hour).UnixMilli(),
	}}
	uc := NewPlacementUsecase(repo, signer)
	uc.now = func() time.Time { return now }
	in := RetargetPlacementInput{PlayerID: 7, Version: 2,
		OperationID: "123e4567-e89b-42d3-a456-426614174000",
		TargetRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB, ExpectedTarget: oldTarget,
		ReplacementVersion: 3, ReplacementOperationID: "223e4567-e89b-42d3-a456-426614174001",
		ReplacementTarget: placement.Target{PodName: "hub-b", InstanceUID: "uid-b", InstanceEpoch: 3,
			AssignmentID: "assignment-b", ReleaseTrack: "stable"},
		ProofType: locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_HUB_TRANSFER,
		Reason:    locatorv1.PlacementTargetUnavailableReason_PLACEMENT_TARGET_UNAVAILABLE_REASON_RESERVATION_EXPIRED_UNUSED,
		ProofID:   "reservation-expired:assignment-a", ProofSignature: "",
		LeaseDeadlineMs: now.Add(time.Hour).UnixMilli()}
	if _, err := uc.Retarget(context.Background(), in); errcode.As(err) != errcode.ErrPermissionDeny {
		t.Fatalf("unsigned lease-only retarget code=%v err=%v", errcode.As(err), err)
	}
	stored, _, _ := repo.GetPlacement(context.Background(), 7)
	if stored.GetVersion() != 2 || !recordTarget(stored).Equal(oldTarget) {
		t.Fatalf("rejected retarget mutated placement: %+v", stored)
	}
}
