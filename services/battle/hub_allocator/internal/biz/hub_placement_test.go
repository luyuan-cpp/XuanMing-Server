package biz

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"
	"github.com/luyuancpp/pandora/pkg/releasetrack"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/data"
)

type hubPlacementFake struct {
	mu                        sync.Mutex
	snapshot                  data.HubPlacementSnapshot
	target                    placement.Target
	beginCalls                int
	bindCalls                 int
	commitCalls               int
	retargetCalls             int
	departureCalls            int
	lastDepartureProof        placement.SourceDepartureProof
	departureProofIDs         []string
	admissionID               string
	pendingBindErr            error
	pendingCommitErr          error
	pendingReadErr            error
	pendingDepartureErr       error
	pendingDepartureCommitErr error
	// pendingBindCommitErr simulates a Bind that committed remotely but whose
	// response was lost. It is consumed once; retrying the same target is legal.
	pendingBindCommitErr error
}

type failOnceAssignmentCASRepo struct {
	data.HubRepo
	failNext bool
}

type rejectOnceAssignmentCASRepo struct {
	data.HubRepo
	rejectNext bool
}

func (r *failOnceAssignmentCASRepo) CompareAndSwapAssignment(ctx context.Context, playerID uint64,
	expected, next *hubv1.HubAssignmentStorageRecord, ttl time.Duration,
) (bool, error) {
	if r.failNext {
		r.failNext = false
		return false, errcode.New(errcode.ErrUnavailable, "injected assignment CAS response loss")
	}
	return r.HubRepo.CompareAndSwapAssignment(ctx, playerID, expected, next, ttl)
}

func (r *rejectOnceAssignmentCASRepo) CompareAndSwapAssignment(ctx context.Context, playerID uint64,
	expected, next *hubv1.HubAssignmentStorageRecord, ttl time.Duration,
) (bool, error) {
	if r.rejectNext {
		r.rejectNext = false
		return false, nil
	}
	return r.HubRepo.CompareAndSwapAssignment(ctx, playerID, expected, next, ttl)
}

func (f *hubPlacementFake) GetHubPlacement(context.Context, uint64) (data.HubPlacementSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pendingReadErr != nil {
		return data.HubPlacementSnapshot{}, f.pendingReadErr
	}
	return f.snapshot, nil
}

func (f *hubPlacementFake) BeginHubTransfer(_ context.Context, _ uint64, expected uint64,
	op, _, _ string, leaseDeadlineMs int64) (placement.Binding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beginCalls++
	if !f.snapshot.Found ||
		f.snapshot.TransitionState != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE ||
		f.snapshot.CurrentRoute != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB ||
		f.snapshot.Binding.Version != expected {
		return placement.Binding{}, errcode.New(errcode.ErrLocatorConflict, "begin source mismatch")
	}
	sourceBinding, sourceTarget := f.snapshot.Binding, f.snapshot.Target
	if sourceTarget.Equal(placement.Target{}) {
		sourceTarget = f.target
	}
	f.snapshot.Binding = placement.Binding{Version: expected + 1, OperationID: op}
	f.snapshot.SourceBinding = sourceBinding
	f.snapshot.SourceTarget = sourceTarget
	f.snapshot.CurrentRoute = locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB
	f.snapshot.TargetRoute = locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB
	f.snapshot.TransitionState = locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING
	f.snapshot.ProofType = locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_HUB_TRANSFER
	f.snapshot.SourceDepartureConfirmed = false
	f.snapshot.SourceDepartureProofType = locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_UNSPECIFIED
	f.snapshot.SourceDepartureProofID = ""
	f.snapshot.LeaseDeadlineMs = leaseDeadlineMs
	f.target = placement.Target{}
	return f.snapshot.Binding, nil
}

func (f *hubPlacementFake) BindHubTarget(_ context.Context, _ uint64, binding placement.Binding, target placement.Target, leaseDeadlineMs int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bindCalls++
	if !f.snapshot.Binding.Equal(binding) {
		return errcode.New(errcode.ErrLocatorConflict, "bind operation mismatch")
	}
	if f.snapshot.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE {
		if f.target.Equal(target) {
			return nil
		}
		return errcode.New(errcode.ErrLocatorConflict, "stable target differs")
	}
	if f.snapshot.TransitionState != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING ||
		f.snapshot.TargetRoute != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB {
		return errcode.New(errcode.ErrLocatorConflict, "not Hub pending")
	}
	if f.pendingBindErr != nil {
		return f.pendingBindErr
	}
	if !f.target.Equal(placement.Target{}) && !f.target.Equal(target) {
		return errcode.New(errcode.ErrLocatorConflict, "pending target differs")
	}
	f.target = target
	f.snapshot.Target = target
	if leaseDeadlineMs > f.snapshot.LeaseDeadlineMs {
		f.snapshot.LeaseDeadlineMs = leaseDeadlineMs
	}
	if f.pendingBindCommitErr != nil {
		err := f.pendingBindCommitErr
		f.pendingBindCommitErr = nil
		return err
	}
	return nil
}

func (f *hubPlacementFake) ConfirmHubSourceDeparture(_ context.Context,
	proof placement.SourceDepartureProof, signature string,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.departureCalls++
	f.lastDepartureProof = proof
	f.departureProofIDs = append(f.departureProofIDs, proof.ProofID)
	if f.pendingDepartureErr != nil {
		err := f.pendingDepartureErr
		f.pendingDepartureErr = nil
		return err
	}
	if signature == "" || proof.ProofType != placement.ProofHubDeparture ||
		proof.SourceRoute != placement.RouteHub || proof.SourceMatchID != 0 ||
		proof.PlayerID == 0 || proof.PlacementVersion != f.snapshot.Binding.Version ||
		proof.OperationID != f.snapshot.Binding.OperationID ||
		proof.TargetRoute != int32(f.snapshot.TargetRoute) ||
		proof.TargetMatchID != f.snapshot.TargetMatchID ||
		proof.SourcePlacementVersion != f.snapshot.SourceBinding.Version ||
		proof.SourceOperationID != f.snapshot.SourceBinding.OperationID ||
		!proof.SourceTarget.Equal(f.snapshot.SourceTarget) {
		return errcode.New(errcode.ErrLocatorConflict, "source departure identity mismatch")
	}
	f.snapshot.SourceDepartureConfirmed = true
	f.snapshot.SourceDepartureProofType = locatorv1.PlacementSourceDepartureProofType(proof.ProofType)
	f.snapshot.SourceDepartureProofID = proof.ProofID
	if f.pendingDepartureCommitErr != nil {
		err := f.pendingDepartureCommitErr
		f.pendingDepartureCommitErr = nil
		return err
	}
	return nil
}

func (f *hubPlacementFake) RetargetHubTarget(_ context.Context, _ uint64,
	expected, replacement placement.Binding, expectedTarget, replacementTarget placement.Target,
	reason placement.TargetUnavailableReason, proofID, _ string, leaseDeadlineMs int64,
) (placement.Binding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.snapshot.Binding.Equal(expected) || !f.target.Equal(expectedTarget) ||
		replacement.Version != expected.Version+1 ||
		replacement.SourceMatchID != expected.SourceMatchID {
		return placement.Binding{}, errcode.New(errcode.ErrLocatorConflict, "retarget identity mismatch")
	}
	f.snapshot.Binding = replacement
	f.snapshot.Target = replacementTarget
	f.snapshot.RetargetCount++
	f.snapshot.LastRetargetProofID = proofID
	f.snapshot.LastRetargetReason = locatorv1.PlacementTargetUnavailableReason(reason)
	f.snapshot.SourceDepartureConfirmed = false
	f.snapshot.SourceDepartureProofType = locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_UNSPECIFIED
	f.snapshot.SourceDepartureProofID = ""
	f.snapshot.LeaseDeadlineMs = leaseDeadlineMs
	f.target = replacementTarget
	f.retargetCalls++
	return replacement, nil
}

func TestHubPendingTargetRetargetRequiresExactOldSeatAbsenceAndAdoptsFence(t *testing.T) {
	uc, repo, _, _ := newModelBUsecase(t, 50, 0)
	const playerID = uint64(7101)
	oldBinding := placement.Binding{Version: 7,
		OperationID: "123e4567-e89b-42d3-a456-426614174000", SourceMatchID: 90}
	oldTarget := placement.Target{PodName: "hub-old", InstanceUID: "uid-old", InstanceEpoch: 4,
		AssignmentID: "assignment-old", ReleaseTrack: releasetrack.Stable}
	assignment := &hubv1.HubAssignmentStorageRecord{
		PlayerId: playerID, HubPodName: "hub-new", HubAddr: "127.0.0.1:7780",
		AssignmentId: "assignment-new", HubInstanceUid: "uid-new", AuthEpoch: 5,
		AuthWriterEpoch: modelBTestWriterEpoch, ReleaseTrack: releasetrack.Stable,
		PlacementVersion: oldBinding.Version, PlacementOperationId: oldBinding.OperationID,
		SourceMatchId:                 oldBinding.SourceMatchID,
		TransferCleanupPending:        true,
		TransferSourceHubPodName:      oldTarget.PodName,
		TransferSourceAssignmentId:    oldTarget.AssignmentID,
		TransferSourceInstanceUid:     oldTarget.InstanceUID,
		TransferSourceAuthEpoch:       oldTarget.InstanceEpoch,
		TransferSourceAuthWriterEpoch: modelBTestWriterEpoch,
	}
	if swapped, err := repo.CompareAndSwapAssignment(context.Background(), playerID, nil, assignment, 0); err != nil || !swapped {
		t.Fatalf("persist replacement assignment: swapped=%v err=%v", swapped, err)
	}
	pf := &hubPlacementFake{snapshot: data.HubPlacementSnapshot{Found: true,
		Binding: oldBinding, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		Target:          oldTarget}, target: oldTarget}
	signer, signErr := placement.NewProofSigner("0123456789abcdef0123456789abcdef")
	if signErr != nil {
		t.Fatal(signErr)
	}
	uc.SetPlacementPolicy(placement.ModeEnforce, pf)
	uc.SetPlacementProofSigner(signer)

	bound, err := uc.bindPlacementTarget(context.Background(), playerID, assignment)
	if err != nil {
		t.Fatalf("retarget exact absent source: %v", err)
	}
	if bound.GetPlacementVersion() != oldBinding.Version+1 ||
		bound.GetPlacementOperationId() == oldBinding.OperationID ||
		bound.GetPlacementProofType() != uint32(placement.ProofHubTransfer) {
		t.Fatalf("replacement fence was not adopted: %+v", bound)
	}
	pf.mu.Lock()
	retargetCalls, locatorTarget, locatorBinding := pf.retargetCalls, pf.target, pf.snapshot.Binding
	pf.mu.Unlock()
	if retargetCalls != 1 || !locatorTarget.Equal(placementTargetFromAssignment(bound)) ||
		!locatorBinding.Equal(placementBindingFromAssignment(bound)) {
		t.Fatalf("retarget calls=%d target=%+v binding=%+v", retargetCalls, locatorTarget, locatorBinding)
	}

	// Simulate a lost allocator response/CAS by restoring the old local binding
	// while leaving locator on the accepted new target.  The next attempt must
	// adopt that exact next fence, not mint another retarget.
	stored, found, getErr := repo.GetAssignment(context.Background(), playerID)
	if getErr != nil || !found {
		t.Fatalf("read accepted assignment: found=%v err=%v", found, getErr)
	}
	stale := proto.Clone(stored).(*hubv1.HubAssignmentStorageRecord)
	bindAssignmentPlacement(stale, oldBinding)
	if swapped, casErr := repo.CompareAndSwapAssignment(context.Background(), playerID, stored, stale, 0); casErr != nil || !swapped {
		t.Fatalf("inject stale local binding: swapped=%v err=%v", swapped, casErr)
	}
	adopted, adoptErr := uc.bindPlacementTarget(context.Background(), playerID, stale)
	if adoptErr != nil || !placementBindingFromAssignment(adopted).Equal(locatorBinding) {
		t.Fatalf("lost-response adoption=%+v err=%v", adopted, adoptErr)
	}
	pf.mu.Lock()
	retargetCalls = pf.retargetCalls
	pf.mu.Unlock()
	if retargetCalls != 1 {
		t.Fatalf("adoption minted another retarget: calls=%d", retargetCalls)
	}
}

func (f *hubPlacementFake) CommitHubAdmission(_ context.Context, _ uint64, binding placement.Binding,
	target placement.Target, admissionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commitCalls++
	if !f.snapshot.Binding.Equal(binding) || !f.target.Equal(target) {
		return errcode.New(errcode.ErrLocatorConflict, "commit identity mismatch")
	}
	if f.pendingCommitErr != nil {
		err := f.pendingCommitErr
		f.pendingCommitErr = nil
		return err
	}
	if f.snapshot.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE {
		// Same version/op/full target is a legal re-admission even with a new id.
		return nil
	}
	if f.snapshot.TransitionState != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING {
		return errcode.New(errcode.ErrLocatorConflict, "commit is not pending")
	}
	bootstrap := f.snapshot.Binding.Version == 1 &&
		f.snapshot.ProofType == locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_ACCOUNT_BOOTSTRAP &&
		f.snapshot.SourceBinding.Empty() && f.snapshot.SourceTarget.Equal(placement.Target{})
	if !bootstrap && !f.snapshot.SourceDepartureConfirmed {
		return errcode.New(errcode.ErrLocatorConflict, "physical source departure is not confirmed")
	}
	f.snapshot.TransitionState = locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE
	f.snapshot.CurrentRoute = locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB
	f.snapshot.CurrentMatchID = 0
	f.snapshot.TargetRoute = locatorv1.PlacementRoute_PLACEMENT_ROUTE_UNSPECIFIED
	f.snapshot.TargetMatchID = 0
	f.snapshot.LeaseDeadlineMs = 0
	if f.snapshot.SourceDepartureConfirmed {
		f.snapshot.LastSourceDepartureProofType = f.snapshot.SourceDepartureProofType
		f.snapshot.LastSourceDepartureProofID = f.snapshot.SourceDepartureProofID
		f.snapshot.LastSourceDeparturePlacementVersion = f.snapshot.Binding.Version
		f.snapshot.LastSourceDepartureOperationID = f.snapshot.Binding.OperationID
	}
	f.snapshot.SourceBinding = placement.Binding{}
	f.snapshot.SourceTarget = placement.Target{}
	f.snapshot.SourceDepartureConfirmed = false
	f.snapshot.SourceDepartureProofType = locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_UNSPECIFIED
	f.snapshot.SourceDepartureProofID = ""
	f.admissionID = admissionID
	return nil
}

type shadowConnectedHubFixture struct {
	uc         *HubUsecase
	repo       *data.RedisHubRepo
	placement  *hubPlacementFake
	assignment *hubv1.HubAssignmentStorageRecord
	credential *HubCredential
	binding    placement.Binding
	pod        string
	playerID   uint64
}

func newShadowConnectedHubFixture(t *testing.T) shadowConnectedHubFixture {
	t.Helper()
	uc, repo, authRepo, _ := newModelBUsecase(t, 50, 1)
	ctx := context.Background()
	const pod = "pandora-hub-global-1"
	const playerID = uint64(1001)
	now := time.Now().UnixMilli()
	seedWarming(t, repo, pod, 1, 50, now)
	if err := repo.UpdateShardWithLock(ctx, pod, 8, func(s *hubv1.HubShardStorageRecord) error {
		s.ReleaseTrack = releasetrack.Stable
		return nil
	}, modelBAuthTTL); err != nil {
		t.Fatalf("set shadow release track: %v", err)
	}
	epoch := activate(t, uc, authRepo, pod, "uid-shadow", 42, "j-shadow", now)
	binding := placement.Binding{Version: 1, OperationID: "123e4567-e89b-42d3-a456-426614174000"}
	pf := &hubPlacementFake{snapshot: data.HubPlacementSnapshot{Found: true, Binding: binding,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		ProofType:       locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_ACCOUNT_BOOTSTRAP}}
	proofSigner, err := placement.NewProofSigner("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	uc.SetPlacementPolicy(placement.ModeShadow, pf)
	uc.SetPlacementProofSigner(proofSigner)
	if assigned, err := uc.AssignHubWithPlacement(ctx, playerID, "global", 0, 0, binding); err != nil || assigned.HubTicket == "" {
		t.Fatalf("shadow initial assign=%+v err=%v", assigned, err)
	}
	assignment, found, err := repo.GetAssignment(ctx, playerID)
	if err != nil || !found {
		t.Fatalf("shadow assignment found=%v err=%v", found, err)
	}
	credential := &HubCredential{InstanceUID: "uid-shadow", ProtocolEpoch: epoch, Gen: 42, JTI: "j-shadow",
		TokenSHA256: "sha-j-shadow", Kid: "kid-test", WriterEpoch: modelBTestWriterEpoch}
	if admitted, err := uc.AcknowledgeAdmissionWithPlacement(ctx, playerID, assignment.GetAssignmentId(), pod,
		uuid.NewString(), 1, binding, credential); err != nil || !admitted.Admitted {
		t.Fatalf("shadow initial admission=%+v err=%v", admitted, err)
	}
	return shadowConnectedHubFixture{uc: uc, repo: repo, placement: pf, assignment: assignment,
		credential: credential, binding: binding, pod: pod, playerID: playerID}
}

func (f shadowConnectedHubFixture) driftCanonicalTarget(t *testing.T) {
	t.Helper()
	other := placementTargetFromAssignment(f.assignment)
	other.AssignmentID = uuid.NewString()
	f.placement.mu.Lock()
	f.placement.target = other
	f.placement.snapshot.Target = other
	f.placement.mu.Unlock()
}

func (f shadowConnectedHubFixture) assertLastPublishedBinding(t *testing.T, expected placement.Binding) {
	t.Helper()
	signer, ok := f.uc.signer.(*fakeSigner)
	if !ok || !signer.lastBinding.Placement.Equal(expected) {
		t.Fatalf("published ticket placement=%+v expected=%+v", signer.lastBinding.Placement, expected)
	}
}

func TestPlacementShadowResignRotatesSuccessorToPublishedBinding(t *testing.T) {
	t.Run("caller-binding-advance", func(t *testing.T) {
		f := newShadowConnectedHubFixture(t)
		next := placement.Binding{Version: f.binding.Version + 1,
			OperationID: "22222222-2222-4222-8222-222222222222", SourceMatchID: f.binding.SourceMatchID}
		if resigned, err := f.uc.AssignHubWithPlacement(context.Background(), f.playerID, "global", 0, 0, next); err != nil || resigned.HubTicket == "" {
			t.Fatalf("shadow caller-binding re-sign=%+v err=%v", resigned, err)
		}
		current, found, err := f.repo.GetAssignment(context.Background(), f.playerID)
		if err != nil || !found || !placementBindingFromAssignment(current).Equal(next) {
			t.Fatalf("shadow caller-binding owner=%+v found=%v err=%v", current, found, err)
		}
		f.assertLastPublishedBinding(t, next)
		if admitted, err := f.uc.AcknowledgeAdmissionWithPlacement(context.Background(), f.playerID,
			current.GetAssignmentId(), f.pod, uuid.NewString(), 2, next, f.credential); err != nil || !admitted.Admitted {
			t.Fatalf("shadow caller-binding re-admission=%+v err=%v", admitted, err)
		}
	})

	t.Run("bind-advances-assign", func(t *testing.T) {
		f := newShadowConnectedHubFixture(t)
		f.driftCanonicalTarget(t)
		if resigned, err := f.uc.AssignHubWithPlacement(context.Background(), f.playerID, "global", 0, 0, f.binding); err != nil || resigned.HubTicket == "" {
			t.Fatalf("shadow bind-advance re-sign=%+v err=%v", resigned, err)
		}
		current, found, err := f.repo.GetAssignment(context.Background(), f.playerID)
		finalBinding := placementBindingFromAssignment(current)
		if err != nil || !found || finalBinding.Version != f.binding.Version+1 || !finalBinding.Complete() {
			t.Fatalf("shadow bind-advance owner=%+v found=%v err=%v", current, found, err)
		}
		f.assertLastPublishedBinding(t, finalBinding)
		if admitted, err := f.uc.AcknowledgeAdmissionWithPlacement(context.Background(), f.playerID,
			current.GetAssignmentId(), f.pod, uuid.NewString(), 2, finalBinding, f.credential); err != nil || !admitted.Admitted {
			t.Fatalf("shadow bind-advance re-admission=%+v err=%v", admitted, err)
		}
	})

	t.Run("bind-advances-same-pod-transfer", func(t *testing.T) {
		f := newShadowConnectedHubFixture(t)
		f.driftCanonicalTarget(t)
		if transferred, err := f.uc.TransferHub(context.Background(), f.playerID, uint64(f.assignment.GetShardId())); err != nil || transferred.NewHubTicket == "" || transferred.NewHubPodName != f.pod {
			t.Fatalf("shadow same-pod transfer=%+v err=%v", transferred, err)
		}
		current, found, err := f.repo.GetAssignment(context.Background(), f.playerID)
		finalBinding := placementBindingFromAssignment(current)
		if err != nil || !found || finalBinding.Version != f.binding.Version+1 || !finalBinding.Complete() {
			t.Fatalf("shadow same-pod owner=%+v found=%v err=%v", current, found, err)
		}
		f.assertLastPublishedBinding(t, finalBinding)
		if admitted, err := f.uc.AcknowledgeAdmissionWithPlacement(context.Background(), f.playerID,
			current.GetAssignmentId(), f.pod, uuid.NewString(), 2, finalBinding, f.credential); err != nil || !admitted.Admitted {
			t.Fatalf("shadow same-pod re-admission=%+v err=%v", admitted, err)
		}
	})
}

func TestHubTicketPlacementReadyRejectsMalformedStableAndExpiredPending(t *testing.T) {
	const playerID = uint64(7199)
	const nowMs = int64(1_000_000)
	binding := placement.Binding{Version: 1,
		OperationID: "123e4567-e89b-42d3-a456-426614174000"}
	target := placement.Target{
		PodName:       "hub-1",
		InstanceUID:   "hub-instance-1",
		InstanceEpoch: 7,
		AssignmentID:  "assignment-1",
		ReleaseTrack:  releasetrack.Stable,
	}
	stable := data.HubPlacementSnapshot{
		Found:           true,
		Binding:         binding,
		CurrentRoute:    locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE,
		Target:          target,
	}
	if !hubTicketPlacementReady(playerID, stable, binding, target, nowMs) {
		t.Fatal("canonical stable Hub placement must be admitted")
	}

	malformedStable := stable
	malformedStable.SourceBinding = placement.Binding{Version: 1,
		OperationID: "123e4567-e89b-42d3-a456-426614174001"}
	if hubTicketPlacementReady(playerID, malformedStable, binding, target, nowMs) {
		t.Fatal("stable placement with pending source lineage must fail closed")
	}

	bootstrap := stable
	bootstrap.CurrentRoute = locatorv1.PlacementRoute_PLACEMENT_ROUTE_UNSPECIFIED
	bootstrap.TargetRoute = locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB
	bootstrap.TransitionState = locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING
	bootstrap.ProofType = locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_ACCOUNT_BOOTSTRAP
	bootstrap.LeaseDeadlineMs = nowMs + 1
	if !hubTicketPlacementReady(playerID, bootstrap, binding, target, nowMs) {
		t.Fatal("unexpired canonical account bootstrap must be admitted")
	}
	bootstrap.LeaseDeadlineMs = nowMs
	if hubTicketPlacementReady(playerID, bootstrap, binding, target, nowMs) {
		t.Fatal("expired pending placement must fail closed")
	}
}

func TestPlacementAdmissionCommitConflictWaitsForPhysicalDeparture(t *testing.T) {
	uc, repo, authRepo, mr := newModelBUsecase(t, 500, 1)
	ctx := context.Background()
	const pod, playerID = "pandora-hub-global-1", uint64(7201)
	now := time.Now().UnixMilli()
	seedWarming(t, repo, pod, 1, 500, now)
	if err := repo.UpdateShardWithLock(ctx, pod, 8, func(s *hubv1.HubShardStorageRecord) error {
		s.ReleaseTrack = releasetrack.Stable
		return nil
	}, modelBAuthTTL); err != nil {
		t.Fatalf("set stable release track: %v", err)
	}
	epoch := activate(t, uc, authRepo, pod, "uid-A", 42, "j42", now)
	binding := placement.Binding{Version: 1,
		OperationID: "123e4567-e89b-42d3-a456-426614174000"}
	pf := &hubPlacementFake{snapshot: data.HubPlacementSnapshot{Found: true, Binding: binding,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		ProofType:       locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_ACCOUNT_BOOTSTRAP}}
	uc.SetPlacementPolicy(placement.ModeEnforce, pf)
	proofSigner, err := placement.NewProofSigner("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	uc.SetPlacementProofSigner(proofSigner)
	if _, err = uc.AssignHubWithPlacement(ctx, playerID, "global", 0, 0, binding); err != nil {
		t.Fatalf("assign: %v", err)
	}
	assignment, found, err := repo.GetAssignment(ctx, playerID)
	if err != nil || !found {
		t.Fatalf("assignment found=%v err=%v", found, err)
	}
	pf.mu.Lock()
	pf.pendingCommitErr = errcode.New(errcode.ErrLocatorConflict, "injected definite commit conflict")
	pf.mu.Unlock()
	cred := &HubCredential{InstanceUID: "uid-A", ProtocolEpoch: epoch, Gen: 42, JTI: "j42",
		TokenSHA256: "sha-j42", Kid: "kid-test", WriterEpoch: modelBTestWriterEpoch}
	admissionID := uuid.NewString()
	got, admissionErr := uc.AcknowledgeAdmissionWithPlacement(ctx, playerID,
		assignment.GetAssignmentId(), pod, admissionID, 1, binding, cred)
	if got != nil || errcode.As(admissionErr) != errcode.ErrLocatorConflict {
		t.Fatalf("commit conflict admission=%+v err=%v", got, admissionErr)
	}
	if sessions, _ := mr.HKeys("pandora:hub:sessions:{" + pod + "}"); len(sessions) != 1 || sessions[0] != assignment.GetAssignmentId() {
		t.Fatalf("server self-departed a still-connected PC: %v", sessions)
	}
	departed, departureErr := uc.AcknowledgeDeparture(ctx, playerID, assignment.GetAssignmentId(), pod,
		admissionID, 1, cred)
	if departureErr != nil || !departed.Departed || departed.Conflict {
		t.Fatalf("physical departure=%+v err=%v", departed, departureErr)
	}
}

func TestPlacementAdmissionOwnerCASConflictWaitsForPhysicalDeparture(t *testing.T) {
	uc, repo, authRepo, mr := newModelBUsecase(t, 500, 1)
	ctx := context.Background()
	const pod, playerID = "pandora-hub-global-1", uint64(7202)
	now := time.Now().UnixMilli()
	seedWarming(t, repo, pod, 1, 500, now)
	if err := repo.UpdateShardWithLock(ctx, pod, 8, func(s *hubv1.HubShardStorageRecord) error {
		s.ReleaseTrack = releasetrack.Stable
		return nil
	}, modelBAuthTTL); err != nil {
		t.Fatalf("set stable release track: %v", err)
	}
	epoch := activate(t, uc, authRepo, pod, "uid-A", 42, "j42", now)
	binding := placement.Binding{Version: 1,
		OperationID: "123e4567-e89b-42d3-a456-426614174001"}
	pf := &hubPlacementFake{snapshot: data.HubPlacementSnapshot{Found: true, Binding: binding,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		ProofType:       locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_ACCOUNT_BOOTSTRAP}}
	uc.SetPlacementPolicy(placement.ModeEnforce, pf)
	proofSigner, err := placement.NewProofSigner("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	uc.SetPlacementProofSigner(proofSigner)
	if _, err = uc.AssignHubWithPlacement(ctx, playerID, "global", 0, 0, binding); err != nil {
		t.Fatalf("assign: %v", err)
	}
	assignment, found, err := repo.GetAssignment(ctx, playerID)
	if err != nil || !found {
		t.Fatalf("assignment found=%v err=%v", found, err)
	}
	uc.repo = &rejectOnceAssignmentCASRepo{HubRepo: uc.repo, rejectNext: true}
	cred := &HubCredential{InstanceUID: "uid-A", ProtocolEpoch: epoch, Gen: 42, JTI: "j42",
		TokenSHA256: "sha-j42", Kid: "kid-test", WriterEpoch: modelBTestWriterEpoch}
	admissionID := uuid.NewString()
	got, admissionErr := uc.AcknowledgeAdmissionWithPlacement(ctx, playerID,
		assignment.GetAssignmentId(), pod, admissionID, 1, binding, cred)
	if got != nil || errcode.As(admissionErr) != errcode.ErrInvalidState {
		t.Fatalf("owner CAS conflict admission=%+v err=%v", got, admissionErr)
	}
	if sessions, _ := mr.HKeys("pandora:hub:sessions:{" + pod + "}"); len(sessions) != 1 || sessions[0] != assignment.GetAssignmentId() {
		t.Fatalf("owner CAS conflict self-departed a live PC: %v", sessions)
	}
	departed, departureErr := uc.AcknowledgeDeparture(ctx, playerID, assignment.GetAssignmentId(), pod,
		admissionID, 1, cred)
	if departureErr != nil || !departed.Departed || departed.Conflict {
		t.Fatalf("physical departure=%+v err=%v", departed, departureErr)
	}
}

func TestPlacementEnforcedHubTransferPersistsOperationAndAllowsReadmission(t *testing.T) {
	uc, repo, authRepo, mr := newModelBUsecase(t, 500, 2)
	ctx := context.Background()
	const pod1, pod2, playerID = "pandora-hub-global-1", "pandora-hub-global-2", uint64(1001)
	now := time.Now().UnixMilli()
	seedWarming(t, repo, pod1, 1, 500, now)
	seedWarming(t, repo, pod2, 2, 500, now)
	for _, pod := range []string{pod1, pod2} {
		if err := repo.UpdateShardWithLock(ctx, pod, 8, func(s *hubv1.HubShardStorageRecord) error {
			s.ReleaseTrack = releasetrack.Stable
			return nil
		}, modelBAuthTTL); err != nil {
			t.Fatalf("set %s release track: %v", pod, err)
		}
	}
	epoch1 := activate(t, uc, authRepo, pod1, "uid-A", 42, "j42", now)
	epoch2 := activate(t, uc, authRepo, pod2, "uid-B", 52, "j52", now)
	initial := placement.Binding{Version: 1, OperationID: "123e4567-e89b-42d3-a456-426614174000"}
	pf := &hubPlacementFake{snapshot: data.HubPlacementSnapshot{Found: true, Binding: initial,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		ProofType:       locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_ACCOUNT_BOOTSTRAP}}
	proofSigner, err := placement.NewProofSigner("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	uc.SetPlacementPolicy(placement.ModeEnforce, pf)
	uc.SetPlacementProofSigner(proofSigner)

	if _, err := uc.AssignHubWithPlacement(ctx, playerID, "global", 0, 0, initial); err != nil {
		t.Fatalf("initial assign: %v", err)
	}
	first, found, _ := repo.GetAssignment(ctx, playerID)
	if !found {
		t.Fatal("initial assignment missing")
	}
	assignmentKey := "pandora:hub:player:1001"
	if ttl := mr.TTL(assignmentKey); ttl != 0 {
		t.Fatalf("pending placement assignment must be persistent, ttl=%s", ttl)
	}
	for _, pod := range []string{pod1, pod2} {
		if err := repo.UpdateShardWithLock(ctx, pod, 8, func(*hubv1.HubShardStorageRecord) error {
			return nil
		}, modelBAuthTTL); err != nil {
			t.Fatalf("extend %s for long-background test: %v", pod, err)
		}
	}
	// Simulate an app outage longer than the legacy assignment TTL (and long
	// enough for the first reservation to disappear). The exact assignment/op
	// must still be present and resumable; recovery may recreate only the seat,
	// never a new placement owner.
	mr.FastForward(2 * uc.assignTTL())
	if _, err := uc.AssignHubWithPlacement(ctx, playerID, "global", 0, 0, initial); err != nil {
		t.Fatalf("long-background exact assignment recovery: %v", err)
	}
	resumed, found, err := repo.GetAssignment(ctx, playerID)
	if err != nil || !found || resumed.GetAssignmentId() != first.GetAssignmentId() ||
		!placementBindingFromAssignment(resumed).Equal(initial) {
		t.Fatalf("long-background recovery changed exact owner: resumed=%+v found=%v err=%v", resumed, found, err)
	}
	first = resumed
	if ttl := mr.TTL(assignmentKey); ttl != 0 {
		t.Fatalf("resumed pending assignment must remain persistent, ttl=%s", ttl)
	}
	cred1 := &HubCredential{InstanceUID: "uid-A", ProtocolEpoch: epoch1, Gen: 42, JTI: "j42",
		TokenSHA256: "sha-j42", Kid: "kid-test", WriterEpoch: modelBTestWriterEpoch}
	firstAdmission := uuid.NewString()
	// Lose the response/result while re-persisting the stable canonical owner
	// after both seat admission and placement Commit. Replaying the same
	// admission id must finish safely without a second session.
	uc.repo = &failOnceAssignmentCASRepo{HubRepo: uc.repo, failNext: true}
	if got, err := uc.AcknowledgeAdmissionWithPlacement(ctx, playerID, first.GetAssignmentId(), pod1,
		firstAdmission, 1, initial, cred1); errcode.As(err) != errcode.ErrUnavailable || got != nil {
		t.Fatalf("lost owner-persist response must stay retryable: admission=%+v err=%v", got, err)
	}
	if ttl := mr.TTL(assignmentKey); ttl != 0 {
		t.Fatalf("unknown owner-persist result must retain durable owner, ttl=%s", ttl)
	}
	if got, err := uc.AcknowledgeAdmissionWithPlacement(ctx, playerID, first.GetAssignmentId(), pod1,
		firstAdmission, 1, initial, cred1); err != nil || !got.Admitted || !got.PlacementCommitted {
		t.Fatalf("exact admission retry=%+v err=%v", got, err)
	}
	if ttl := mr.TTL(assignmentKey); ttl != 0 {
		t.Fatalf("committed stable assignment must remain persistent, ttl=%s", ttl)
	}
	memberKey := "pandora:hub:shard:members:{" + pod1 + "}"
	if ttl := mr.TTL(memberKey); ttl != 0 {
		t.Fatalf("strict connected member index must survive until explicit cleanup, ttl=%s", ttl)
	}
	// Keep the DS discovery/auth records alive as heartbeats would in production;
	// this clock jump is intended to age only the player's canonical assignment.
	for _, pod := range []string{pod1, pod2} {
		if err := repo.UpdateShardWithLock(ctx, pod, 8, func(*hubv1.HubShardStorageRecord) error {
			return nil
		}, modelBAuthTTL); err != nil {
			t.Fatalf("extend %s while player is backgrounded: %v", pod, err)
		}
	}
	// Stable connected ownership is also non-expiring. Advancing beyond the old
	// 30m assignment TTL must preserve the same canonical assignment; otherwise
	// a relogin could reserve/admit a second owner while the old session remains.
	mr.FastForward(2 * uc.assignTTL())
	if _, found, err := repo.GetAssignment(ctx, playerID); err != nil || !found {
		t.Fatalf("stable canonical assignment expired: found=%v err=%v", found, err)
	}
	if _, err := uc.AssignHubWithPlacement(ctx, playerID, "global", 0, 0, initial); err != nil {
		t.Fatalf("stable long-background relogin: %v", err)
	}
	stableRelogin, found, err := repo.GetAssignment(ctx, playerID)
	if err != nil || !found || stableRelogin.GetAssignmentId() != first.GetAssignmentId() {
		t.Fatalf("stable relogin created a second owner: assignment=%+v found=%v err=%v",
			stableRelogin, found, err)
	}
	if sessions, _ := mr.HKeys("pandora:hub:sessions:{" + pod1 + "}"); len(sessions) != 1 || sessions[0] != first.GetAssignmentId() {
		t.Fatalf("stable relogin changed connected owner cardinality: %v", sessions)
	}
	if members, _ := repo.ListShardMembers(ctx, pod1); len(members) != 1 || members[0] != playerID {
		t.Fatalf("long-connected player disappeared from drain index: %v", members)
	}

	transfer, err := uc.TransferHub(ctx, playerID, 2)
	if errcode.As(err) != errcode.ErrUnavailable || transfer != nil {
		t.Fatalf("transfer must wait for physical source departure: transfer=%+v err=%v", transfer, err)
	}
	second, found, _ := repo.GetAssignment(ctx, playerID)
	if !found || !second.GetTransferCleanupPending() || !second.GetTransferTargetBound() {
		t.Fatalf("physical departure wait was not durably fenced: %+v", second)
	}
	cred2 := &HubCredential{InstanceUID: "uid-B", ProtocolEpoch: epoch2, Gen: 52, JTI: "j52",
		TokenSHA256: "sha-j52", Kid: "kid-test", WriterEpoch: modelBTestWriterEpoch}
	secondBinding := placementBindingFromAssignment(second)
	if got, admissionErr := uc.AcknowledgeAdmissionWithPlacement(ctx, playerID, second.GetAssignmentId(), pod2,
		uuid.NewString(), 2, secondBinding, cred2); errcode.As(admissionErr) != errcode.ErrUnavailable || got != nil {
		t.Fatalf("target admission opened before physical source departure: got=%+v err=%v", got, admissionErr)
	}
	if sessions, _ := mr.HKeys("pandora:hub:sessions:{" + pod2 + "}"); len(sessions) != 0 {
		t.Fatalf("blocked target admission created a physical owner: %v", sessions)
	}
	heartbeat, err := uc.HeartbeatWithCredential(ctx, pod1, 1, []uint64{playerID}, 500,
		"ready", 0, cred1)
	if err != nil || len(heartbeat.EvictionOrders) != 1 {
		t.Fatalf("source heartbeat eviction orders=%+v err=%v", heartbeat, err)
	}
	order := heartbeat.EvictionOrders[0]
	if order.PlayerID != playerID || order.AssignmentID != first.GetAssignmentId() ||
		order.AdmissionID != firstAdmission || order.AdmissionSeq != 1 ||
		order.CleanupAssignmentID != second.GetAssignmentId() {
		t.Fatalf("source heartbeat emitted non-exact eviction: %+v", order)
	}
	departure, err := uc.AcknowledgeDeparture(ctx, playerID, first.GetAssignmentId(), pod1,
		firstAdmission, 1, cred1)
	if err != nil || !departure.Departed {
		t.Fatalf("source physical departure=%+v err=%v", departure, err)
	}
	// The app was already backgrounded beyond the legacy assignment TTL above.
	// Physical disconnect removes only the connected seat; the durable source
	// member remains discoverable until the exact transfer phase removes it.
	if members, memberErr := repo.ListShardMembers(ctx, pod1); memberErr != nil ||
		len(members) != 1 || members[0] != playerID {
		t.Fatalf("offline transfer owner disappeared from drain index: members=%v err=%v", members, memberErr)
	}
	transfer, err = uc.TransferHub(ctx, playerID, 2)
	if err != nil {
		t.Fatalf("transfer after exact physical departure: %v", err)
	}
	if transfer.NewHubPodName != pod2 || transfer.NewHubTicket == "" {
		t.Fatalf("transfer result=%+v", transfer)
	}
	if members, memberErr := repo.ListShardMembers(ctx, pod1); memberErr != nil || len(members) != 0 {
		t.Fatalf("completed exact transfer retained source drain member: members=%v err=%v", members, memberErr)
	}
	if members, memberErr := repo.ListShardMembers(ctx, pod2); memberErr != nil ||
		len(members) != 1 || members[0] != playerID {
		t.Fatalf("completed exact transfer lost target drain member: members=%v err=%v", members, memberErr)
	}
	second, found, _ = repo.GetAssignment(ctx, playerID)
	if !found || second.GetPlacementVersion() != 2 ||
		!placement.ValidOperationID(second.GetPlacementOperationId()) {
		t.Fatalf("transfer operation was not persisted: %+v", second)
	}
	if ttl := mr.TTL(assignmentKey); ttl != 0 {
		t.Fatalf("pending Hub transfer assignment must survive arbitrary outage, ttl=%s", ttl)
	}
	pf.mu.Lock()
	beginCalls, target := pf.beginCalls, pf.target
	pf.mu.Unlock()
	if beginCalls != 1 || target.PodName != pod2 || target.InstanceUID != "uid-B" ||
		target.InstanceEpoch != epoch2 {
		t.Fatalf("beginCalls=%d target=%+v", beginCalls, target)
	}

	secondBinding = placementBindingFromAssignment(second)
	if got, err := uc.AcknowledgeAdmissionWithPlacement(ctx, playerID, second.GetAssignmentId(), pod2,
		uuid.NewString(), 2, secondBinding, cred2); err != nil || !got.PlacementCommitted {
		t.Fatalf("new target admission=%+v err=%v", got, err)
	}
	if ttl := mr.TTL(assignmentKey); ttl != 0 {
		t.Fatalf("committed transfer assignment must remain persistent, ttl=%s", ttl)
	}
	// A new connection/admission id to the same stable target first reissues a
	// ticket.  That path prepares the exact successor lease; a live session by
	// itself is intentionally not an Admission capability.
	if resigned, resignErr := uc.AssignHubWithPlacement(ctx, playerID, "global", 0, 0,
		secondBinding); resignErr != nil || resigned.HubTicket == "" {
		t.Fatalf("same target ticket reissue=%+v err=%v", resigned, resignErr)
	}
	if got, err := uc.AcknowledgeAdmissionWithPlacement(ctx, playerID, second.GetAssignmentId(), pod2,
		uuid.NewString(), 3, secondBinding, cred2); err != nil || !got.PlacementCommitted {
		t.Fatalf("same target re-admission=%+v err=%v", got, err)
	}
}

func TestPlacementEnforcedDrainMigrationPublishesOnlyAfterDurableBind(t *testing.T) {
	for _, tc := range []struct {
		name            string
		failPendingBind bool
	}{
		{name: "bind-success"},
		{name: "bind-unavailable-retains-old-owner", failPendingBind: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uc, repo, authRepo, mr := newModelBUsecase(t, 500, 2)
			ctx := context.Background()
			const pod1, pod2, playerID = "pandora-hub-global-1", "pandora-hub-global-2", uint64(1001)
			now := time.Now().UnixMilli()
			seedWarming(t, repo, pod1, 1, 500, now)
			seedWarming(t, repo, pod2, 2, 500, now)
			for _, pod := range []string{pod1, pod2} {
				if err := repo.UpdateShardWithLock(ctx, pod, 8, func(s *hubv1.HubShardStorageRecord) error {
					s.ReleaseTrack = releasetrack.Stable
					return nil
				}, modelBAuthTTL); err != nil {
					t.Fatalf("set %s release track: %v", pod, err)
				}
			}
			epoch1 := activate(t, uc, authRepo, pod1, "uid-A", 42, "j42", now)
			activate(t, uc, authRepo, pod2, "uid-B", 52, "j52", now)
			initial := placement.Binding{Version: 1, OperationID: "123e4567-e89b-42d3-a456-426614174000"}
			pf := &hubPlacementFake{snapshot: data.HubPlacementSnapshot{Found: true, Binding: initial,
				TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
				TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
				ProofType:       locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_ACCOUNT_BOOTSTRAP}}
			proofSigner, err := placement.NewProofSigner("0123456789abcdef0123456789abcdef")
			if err != nil {
				t.Fatal(err)
			}
			pusher := &fakeMigratePusher{}
			uc.SetPlacementPolicy(placement.ModeEnforce, pf)
			uc.SetPlacementProofSigner(proofSigner)
			uc.SetMigratePusher(pusher)

			if _, err := uc.AssignHubWithPlacement(ctx, playerID, "global", 0, 0, initial); err != nil {
				t.Fatalf("initial assign: %v", err)
			}
			first, found, err := repo.GetAssignment(ctx, playerID)
			if err != nil || !found || first.GetHubPodName() != pod1 {
				t.Fatalf("initial assignment=%+v found=%v err=%v", first, found, err)
			}
			cred1 := &HubCredential{InstanceUID: "uid-A", ProtocolEpoch: epoch1, Gen: 42, JTI: "j42",
				TokenSHA256: "sha-j42", Kid: "kid-test", WriterEpoch: modelBTestWriterEpoch}
			sourceAdmissionID := uuid.NewString()
			if got, err := uc.AcknowledgeAdmissionWithPlacement(ctx, playerID, first.GetAssignmentId(), pod1,
				sourceAdmissionID, 1, initial, cred1); err != nil || !got.PlacementCommitted {
				t.Fatalf("initial admission=%+v err=%v", got, err)
			}
			if tc.failPendingBind {
				pf.mu.Lock()
				pf.pendingBindErr = errcode.New(errcode.ErrUnavailable, "injected bind timeout")
				pf.mu.Unlock()
			}
			pf.mu.Lock()
			beginBefore, bindBefore := pf.beginCalls, pf.bindCalls
			pf.mu.Unlock()
			from, _, _ := repo.GetShard(ctx, pod1)
			target, _, _ := repo.GetShard(ctx, pod2)
			migrated := uc.migratePlayer(ctx, playerID, from, target)

			current, found, err := repo.GetAssignment(ctx, playerID)
			if err != nil || !found || current.GetHubPodName() != pod2 || current.GetPlacementVersion() != 2 ||
				current.GetPlacementProofType() != uint32(placement.ProofHubTransfer) ||
				!placement.ValidOperationID(current.GetPlacementOperationId()) {
				t.Fatalf("durable migration assignment=%+v found=%v err=%v", current, found, err)
			}
			if ttl := mr.TTL("pandora:hub:player:1001"); ttl != 0 {
				t.Fatalf("uncommitted migration owner must be persistent, ttl=%s", ttl)
			}
			pf.mu.Lock()
			beginCalls, bindCalls := pf.beginCalls, pf.bindCalls
			pf.mu.Unlock()
			if beginCalls-beginBefore != 1 || bindCalls-bindBefore != 2 {
				t.Fatalf("migration must persist then Begin+Bind: begin=%d->%d bind=%d->%d",
					beginBefore, beginCalls, bindBefore, bindCalls)
			}
			fromAfter, _, _ := repo.GetShard(ctx, pod1)
			targetAfter, _, _ := repo.GetShard(ctx, pod2)
			if tc.failPendingBind {
				heartbeat, heartbeatErr := uc.HeartbeatWithCredential(ctx, pod1, 1,
					[]uint64{playerID}, 500, "ready", 0, cred1)
				if heartbeatErr != nil || len(heartbeat.EvictionOrders) != 0 {
					t.Fatalf("unbound target must not evict source: heartbeat=%+v err=%v", heartbeat, heartbeatErr)
				}
				if migrated || pusher.count() != 0 || fromAfter.GetPlayerCount() != 1 || targetAfter.GetPlayerCount() != 1 {
					t.Fatalf("failed bind escaped/released owner: migrated=%v pushes=%d from=%d target=%d",
						migrated, pusher.count(), fromAfter.GetPlayerCount(), targetAfter.GetPlayerCount())
				}
				members, _ := repo.ListShardMembers(ctx, pod1)
				if len(members) != 1 || members[0] != playerID {
					t.Fatalf("failed bind removed old recovery member: %v", members)
				}
				return
			}
			if migrated || pusher.count() != 0 || fromAfter.GetPlayerCount() != 1 || targetAfter.GetPlayerCount() != 1 {
				t.Fatalf("bound target escaped before physical departure: migrated=%v pushes=%d from=%d target=%d",
					migrated, pusher.count(), fromAfter.GetPlayerCount(), targetAfter.GetPlayerCount())
			}
			heartbeat, heartbeatErr := uc.HeartbeatWithCredential(ctx, pod1, 1,
				[]uint64{playerID}, 500, "ready", 0, cred1)
			if heartbeatErr != nil || len(heartbeat.EvictionOrders) != 1 ||
				heartbeat.EvictionOrders[0].AdmissionID != sourceAdmissionID {
				t.Fatalf("drain exact eviction heartbeat=%+v err=%v", heartbeat, heartbeatErr)
			}
			departure, departureErr := uc.AcknowledgeDeparture(ctx, playerID, first.GetAssignmentId(), pod1,
				sourceAdmissionID, 1, cred1)
			if departureErr != nil || !departure.Departed {
				t.Fatalf("drain physical departure=%+v err=%v", departure, departureErr)
			}
			from, _, _ = repo.GetShard(ctx, pod1)
			target, _, _ = repo.GetShard(ctx, pod2)
			if migrated = uc.migratePlayer(ctx, playerID, from, target); !migrated || pusher.count() != 1 {
				t.Fatalf("drain did not publish recovered exact target: migrated=%v pushes=%d",
					migrated, pusher.count())
			}
			fromAfter, _, _ = repo.GetShard(ctx, pod1)
			targetAfter, _, _ = repo.GetShard(ctx, pod2)
			if fromAfter.GetPlayerCount() != 0 || targetAfter.GetPlayerCount() != 1 {
				t.Fatalf("drain physical cleanup projection from=%d target=%d",
					fromAfter.GetPlayerCount(), targetAfter.GetPlayerCount())
			}
		})
	}
}
