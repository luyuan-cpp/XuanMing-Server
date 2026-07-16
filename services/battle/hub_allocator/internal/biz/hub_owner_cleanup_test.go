package biz

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"
	"github.com/luyuancpp/pandora/pkg/releasetrack"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/data"
)

type ownerCleanupFixture struct {
	uc                *HubUsecase
	repo              *data.RedisHubRepo
	authRepo          *data.RedisHubAuthRepo
	mr                *miniredis.Miniredis
	placement         *hubPlacementFake
	proofSigner       *placement.ProofSigner
	source            *hubv1.HubAssignmentStorageRecord
	sourceCredential  *HubCredential
	targetCredential  *HubCredential
	sourceAdmissionID string
}

func newOwnerCleanupFixture(t *testing.T) *ownerCleanupFixture {
	t.Helper()
	uc, repo, authRepo, mr := newModelBUsecase(t, 500, 2)
	ctx := context.Background()
	const pod1, pod2 = "pandora-hub-global-1", "pandora-hub-global-2"
	now := time.Now().UnixMilli()
	seedWarming(t, repo, pod1, 1, 500, now)
	seedWarming(t, repo, pod2, 2, 500, now)
	for _, pod := range []string{pod1, pod2} {
		if err := repo.UpdateShardWithLock(ctx, pod, 8, func(s *hubv1.HubShardStorageRecord) error {
			s.ReleaseTrack = releasetrack.Stable
			return nil
		}, modelBAuthTTL); err != nil {
			t.Fatal(err)
		}
	}
	epoch1 := activate(t, uc, authRepo, pod1, "uid-A", 42, "j42", now)
	epoch2 := activate(t, uc, authRepo, pod2, "uid-B", 52, "j52", now)
	binding := placement.Binding{Version: 1, OperationID: "123e4567-e89b-42d3-a456-426614174000"}
	placementFake := &hubPlacementFake{snapshot: data.HubPlacementSnapshot{
		Found: true, Binding: binding,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		ProofType:       locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_ACCOUNT_BOOTSTRAP,
	}}
	proofSigner, err := placement.NewProofSigner("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	uc.SetPlacementPolicy(placement.ModeEnforce, placementFake)
	uc.SetPlacementProofSigner(proofSigner)
	if _, err := uc.AssignHubWithPlacement(ctx, 1001, "global", 0, 0, binding); err != nil {
		t.Fatalf("source assignment: %v", err)
	}
	source, found, err := repo.GetAssignment(ctx, 1001)
	if err != nil || !found || source.GetHubPodName() != pod1 {
		t.Fatalf("source=%+v found=%v err=%v", source, found, err)
	}
	sourceCredential := &HubCredential{InstanceUID: "uid-A", ProtocolEpoch: epoch1, Gen: 42,
		JTI: "j42", TokenSHA256: "sha-j42", Kid: "kid-test", WriterEpoch: modelBTestWriterEpoch}
	targetCredential := &HubCredential{InstanceUID: "uid-B", ProtocolEpoch: epoch2, Gen: 52,
		JTI: "j52", TokenSHA256: "sha-j52", Kid: "kid-test", WriterEpoch: modelBTestWriterEpoch}
	sourceAdmissionID := uuid.NewString()
	if got, err := uc.AcknowledgeAdmissionWithPlacement(ctx, 1001, source.GetAssignmentId(), pod1,
		sourceAdmissionID, 1, binding, sourceCredential); err != nil || !got.PlacementCommitted {
		t.Fatalf("source admission=%+v err=%v", got, err)
	}
	return &ownerCleanupFixture{uc: uc, repo: repo, authRepo: authRepo, mr: mr,
		placement: placementFake, proofSigner: proofSigner, source: source,
		sourceCredential: sourceCredential, targetCredential: targetCredential,
		sourceAdmissionID: sourceAdmissionID}
}

func (f *ownerCleanupFixture) restart(repo data.HubRepo, authRepo data.HubAuthRepo) *HubUsecase {
	if repo == nil {
		repo = f.repo
	}
	if authRepo == nil {
		authRepo = f.authRepo
	}
	uc := NewHubUsecase(repo, NewMockHubFleetProvider(f.uc.cfg), &fakeSigner{}, f.uc.cfg)
	uc.SetAuthRepo(authRepo)
	uc.SetAuthTTL(modelBAuthTTL)
	uc.SetPlacementPolicy(placement.ModeEnforce, f.placement)
	uc.SetPlacementProofSigner(f.proofSigner)
	return uc
}

func (f *ownerCleanupFixture) beginBattleDeparture(t *testing.T, matchID uint64) placement.Binding {
	t.Helper()
	binding := placement.Binding{Version: f.source.GetPlacementVersion() + 1,
		OperationID: "223e4567-e89b-42d3-a456-426614174000"}
	sourceBinding := placement.Binding{Version: f.source.GetPlacementVersion(),
		OperationID: f.source.GetPlacementOperationId()}
	sourceTarget := placementTargetFromAssignment(f.source)
	f.placement.mu.Lock()
	f.placement.snapshot = data.HubPlacementSnapshot{
		Found: true, Binding: binding, SourceBinding: sourceBinding, SourceTarget: sourceTarget,
		CurrentRoute:    locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		Target: placement.Target{PodName: "battle-pod", InstanceUID: "battle-uid",
			InstanceEpoch: 9, AllocationID: "battle-allocation", ReleaseTrack: releasetrack.Stable},
		TargetMatchID: matchID,
	}
	f.placement.mu.Unlock()
	return binding
}

type commitThenErrorAssignmentRepo struct {
	data.HubRepo
	shouldFail func(expected, next *hubv1.HubAssignmentStorageRecord) bool
	fired      bool
}

func (r *commitThenErrorAssignmentRepo) CompareAndSwapAssignment(ctx context.Context, playerID uint64,
	expected, next *hubv1.HubAssignmentStorageRecord, ttl time.Duration,
) (bool, error) {
	if !r.fired && r.shouldFail != nil && r.shouldFail(expected, next) {
		swapped, err := r.HubRepo.CompareAndSwapAssignment(ctx, playerID, expected, next, ttl)
		if err != nil || !swapped {
			return swapped, err
		}
		r.fired = true
		return false, errcode.New(errcode.ErrUnavailable, "injected committed assignment CAS response loss")
	}
	return r.HubRepo.CompareAndSwapAssignment(ctx, playerID, expected, next, ttl)
}

type departureCommitThenErrorAuthRepo struct {
	data.HubAuthRepo
	failNext bool
}

func (r *departureCommitThenErrorAuthRepo) AcknowledgeDeparture(ctx context.Context, pod string,
	credential data.CredentialIdentity, reservation data.ReservationIdentity, admissionID string,
	admissionSeq uint64, nowMs int64, shardTTL time.Duration,
) (data.DepartureResult, error) {
	if r.failNext {
		r.failNext = false
		result, err := r.HubAuthRepo.AcknowledgeDeparture(ctx, pod, credential, reservation,
			admissionID, admissionSeq, nowMs, shardTTL)
		if err != nil {
			return result, err
		}
		return data.DepartureResult{}, errcode.New(errcode.ErrUnavailable,
			"injected committed departure response loss")
	}
	return r.HubAuthRepo.AcknowledgeDeparture(ctx, pod, credential, reservation,
		admissionID, admissionSeq, nowMs, shardTTL)
}

func assertExactSourceEviction(t *testing.T, f *ownerCleanupFixture, uc *HubUsecase,
	target *hubv1.HubAssignmentStorageRecord) {
	t.Helper()
	heartbeat, err := uc.HeartbeatWithCredential(context.Background(), f.source.GetHubPodName(), 1,
		[]uint64{1001}, 500, "ready", 0, f.sourceCredential)
	if err != nil || len(heartbeat.EvictionOrders) != 1 {
		t.Fatalf("heartbeat=%+v err=%v", heartbeat, err)
	}
	order := heartbeat.EvictionOrders[0]
	if order.PlayerID != 1001 || order.AssignmentID != f.source.GetAssignmentId() ||
		order.AdmissionID != f.sourceAdmissionID || order.AdmissionSeq != 1 ||
		order.CleanupAssignmentID != target.GetAssignmentId() {
		t.Fatalf("non-exact eviction order: %+v", order)
	}
}

func finishSourceDeparture(t *testing.T, f *ownerCleanupFixture, uc *HubUsecase) {
	t.Helper()
	result, err := uc.AcknowledgeDeparture(context.Background(), 1001, f.source.GetAssignmentId(),
		f.source.GetHubPodName(), f.sourceAdmissionID, 1, f.sourceCredential)
	if err != nil || !result.Departed {
		t.Fatalf("source departure=%+v err=%v", result, err)
	}
}

func admitRecoveredTarget(t *testing.T, f *ownerCleanupFixture, uc *HubUsecase,
	target *hubv1.HubAssignmentStorageRecord) {
	t.Helper()
	got, err := uc.AcknowledgeAdmissionWithPlacement(context.Background(), 1001,
		target.GetAssignmentId(), target.GetHubPodName(), uuid.NewString(), 2,
		placementBindingFromAssignment(target), f.targetCredential)
	if err != nil || !got.Admitted || !got.PlacementCommitted {
		t.Fatalf("target admission=%+v err=%v", got, err)
	}
	if sessions, _ := f.mr.HKeys("pandora:hub:sessions:{" + f.source.GetHubPodName() + "}"); len(sessions) != 0 {
		t.Fatalf("source physical owner survived: %v", sessions)
	}
	if sessions, _ := f.mr.HKeys("pandora:hub:sessions:{" + target.GetHubPodName() + "}"); len(sessions) != 1 || sessions[0] != target.GetAssignmentId() {
		t.Fatalf("target physical owner cardinality=%v", sessions)
	}
}

func TestHubOwnerCleanupRestartRecoversPublicationAndBindResponseLoss(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inject func(*ownerCleanupFixture)
	}{
		{name: "assignment-cas-committed-response-lost", inject: func(f *ownerCleanupFixture) {
			f.uc.repo = &commitThenErrorAssignmentRepo{HubRepo: f.repo,
				shouldFail: func(expected, next *hubv1.HubAssignmentStorageRecord) bool {
					return expected != nil && next != nil && next.GetTransferCleanupPending() &&
						expected.GetAssignmentId() != next.GetAssignmentId()
				}}
		}},
		{name: "locator-bind-committed-response-lost", inject: func(f *ownerCleanupFixture) {
			f.placement.pendingBindCommitErr = errcode.New(errcode.ErrUnavailable,
				"injected committed Bind response loss")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOwnerCleanupFixture(t)
			tc.inject(f)
			if got, err := f.uc.TransferHub(context.Background(), 1001, 2); errcode.As(err) != errcode.ErrUnavailable || got != nil {
				t.Fatalf("faulted transfer=%+v err=%v", got, err)
			}
			target, found, err := f.repo.GetAssignment(context.Background(), 1001)
			if err != nil || !found || !target.GetTransferCleanupPending() {
				t.Fatalf("durable target=%+v found=%v err=%v", target, found, err)
			}
			restarted := f.restart(nil, nil)
			if err := restarted.reconcileOwnerCleanups(context.Background()); errcode.As(err) != errcode.ErrUnavailable {
				t.Fatalf("reconcile must wait for physical departure: %v", err)
			}
			target, _, _ = f.repo.GetAssignment(context.Background(), 1001)
			if !target.GetTransferTargetBound() {
				t.Fatalf("target Bind was not durably recovered: %+v", target)
			}
			assertExactSourceEviction(t, f, restarted, target)
			finishSourceDeparture(t, f, restarted)
			if err := restarted.reconcileOwnerCleanups(context.Background()); err != nil {
				t.Fatalf("cleanup after physical departure: %v", err)
			}
			f.placement.mu.Lock()
			confirmed, confirmCalls := f.placement.snapshot.SourceDepartureConfirmed,
				f.placement.departureCalls
			f.placement.mu.Unlock()
			if !confirmed || confirmCalls != 1 {
				t.Fatalf("restart cleanup did not confirm locator before publication: confirmed=%v calls=%d",
					confirmed, confirmCalls)
			}
			target, found, err = f.repo.GetAssignment(context.Background(), 1001)
			if err != nil || !found || target.GetTransferCleanupPending() ||
				target.GetAssignmentId() == f.source.GetAssignmentId() {
				t.Fatalf("cleanup did not preserve exact target: %+v found=%v err=%v", target, found, err)
			}
			refs, _ := f.repo.ListTransferCleanups(context.Background(), f.source.GetHubPodName())
			if len(refs) != 0 {
				t.Fatalf("cleanup index survived completion: %+v", refs)
			}
			admitRecoveredTarget(t, f, restarted, target)
		})
	}
}

func TestHubOwnerCleanupResponseLossNeverReleasesNewOwner(t *testing.T) {
	f := newOwnerCleanupFixture(t)
	ctx := context.Background()
	if got, err := f.uc.TransferHub(ctx, 1001, 2); errcode.As(err) != errcode.ErrUnavailable || got != nil {
		t.Fatalf("transfer=%+v err=%v", got, err)
	}
	target, _, _ := f.repo.GetAssignment(ctx, 1001)
	assertExactSourceEviction(t, f, f.uc, target)

	faultAuth := &departureCommitThenErrorAuthRepo{HubAuthRepo: f.authRepo, failNext: true}
	restarted := f.restart(nil, faultAuth)
	if got, err := restarted.AcknowledgeDeparture(ctx, 1001, f.source.GetAssignmentId(),
		f.source.GetHubPodName(), f.sourceAdmissionID, 1, f.sourceCredential); errcode.As(err) != errcode.ErrUnavailable || got != nil {
		t.Fatalf("committed departure response loss=%+v err=%v", got, err)
	}
	if sessions, _ := f.mr.HKeys("pandora:hub:sessions:{" + f.source.GetHubPodName() + "}"); len(sessions) != 0 {
		t.Fatalf("committed physical departure did not remove source: %v", sessions)
	}
	if got, err := restarted.AcknowledgeDeparture(ctx, 1001, f.source.GetAssignmentId(),
		f.source.GetHubPodName(), f.sourceAdmissionID, 1, f.sourceCredential); err != nil || !got.Departed {
		t.Fatalf("departure response-loss replay=%+v err=%v", got, err)
	}

	clearFault := &commitThenErrorAssignmentRepo{HubRepo: f.repo,
		shouldFail: func(expected, next *hubv1.HubAssignmentStorageRecord) bool {
			return expected != nil && next != nil && expected.GetTransferCleanupPending() &&
				expected.GetTransferTargetBound() && !next.GetTransferCleanupPending() &&
				expected.GetAssignmentId() == next.GetAssignmentId()
		}}
	restarted = f.restart(clearFault, f.authRepo)
	if err := restarted.reconcileOwnerCleanups(ctx); errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("committed phase-clear response loss=%v", err)
	}
	current, found, err := f.repo.GetAssignment(ctx, 1001)
	if err != nil || !found || current.GetTransferCleanupPending() ||
		current.GetAssignmentId() != target.GetAssignmentId() {
		t.Fatalf("committed phase clear damaged target: current=%+v found=%v err=%v", current, found, err)
	}
	// The response loss intentionally left only the exact index ref. A fresh
	// process recognizes it as an orphan; it must never touch the target seat.
	fresh := f.restart(nil, nil)
	if err := fresh.reconcileOwnerCleanups(ctx); err != nil {
		t.Fatalf("orphan index cleanup: %v", err)
	}
	refs, _ := f.repo.ListTransferCleanups(ctx, f.source.GetHubPodName())
	if len(refs) != 0 {
		t.Fatalf("phase-clear orphan ref remained: %+v", refs)
	}
	if reservations, _ := f.mr.HKeys("pandora:hub:reservations:{" + target.GetHubPodName() + "}"); len(reservations) != 1 || reservations[0] != target.GetAssignmentId() {
		t.Fatalf("orphan cleanup released new target reservation: %v", reservations)
	}
	admitRecoveredTarget(t, f, fresh, current)
}

func TestBattleDepartureMissingAssignmentReconstructsExactPhysicalCleanup(t *testing.T) {
	f := newOwnerCleanupFixture(t)
	ctx := context.Background()
	const matchID = uint64(801)
	binding := f.beginBattleDeparture(t, matchID)
	if swapped, err := f.repo.CompareAndSwapAssignment(ctx, 1001, f.source, nil, 0); err != nil || !swapped {
		t.Fatalf("remove canonical assignment: swapped=%v err=%v", swapped, err)
	}

	if err := f.uc.EnsureHubDepartureForBattle(ctx, 1001, matchID, binding); errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("connected source must wait for exact Departure: %v", err)
	}
	if f.placement.departureCalls != 0 {
		t.Fatalf("assignment miss was treated as departure proof: calls=%d", f.placement.departureCalls)
	}
	tombstone, found, err := f.repo.GetAssignment(ctx, 1001)
	if err != nil || !found || !tombstone.GetReleaseCleanupPending() ||
		!assignmentMatchesBattleDepartureSource(tombstone, 1001, f.placement.snapshot) {
		t.Fatalf("exact source tombstone=%+v found=%v err=%v", tombstone, found, err)
	}
	assertExactSourceEviction(t, f, f.uc, tombstone)
	finishSourceDeparture(t, f, f.uc)
	if err := f.uc.EnsureHubDepartureForBattle(ctx, 1001, matchID, binding); err != nil {
		t.Fatalf("departure replay after physical proof: %v", err)
	}
	if f.placement.departureCalls != 1 {
		t.Fatalf("exact cleanup did not confirm once: calls=%d", f.placement.departureCalls)
	}
	if _, found, err := f.repo.GetAssignment(ctx, 1001); err != nil || found {
		t.Fatalf("source tombstone survived completion: found=%v err=%v", found, err)
	}
	refs, err := f.repo.ListTransferCleanups(ctx, f.source.GetHubPodName())
	if err != nil || len(refs) != 0 {
		t.Fatalf("source cleanup index survived: refs=%+v err=%v", refs, err)
	}
}

func TestBattleDepartureLocatorACKLossReplaysStableProofBeforeDeletingTombstone(t *testing.T) {
	f := newOwnerCleanupFixture(t)
	ctx := context.Background()
	const matchID = uint64(811)
	binding := f.beginBattleDeparture(t, matchID)
	finishSourceDeparture(t, f, f.uc)
	signer := f.uc.signer.(*fakeSigner)
	baseSignCalls, baseCommitCalls := signer.calls, f.placement.commitCalls
	f.placement.pendingDepartureCommitErr = errcode.New(errcode.ErrUnavailable,
		"injected committed source-departure ACK loss")

	if err := f.uc.EnsureHubDepartureForBattle(ctx, 1001, matchID, binding); errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("committed locator ACK loss=%v", err)
	}
	tombstone, found, err := f.repo.GetAssignment(ctx, 1001)
	if err != nil || !found || !tombstone.GetReleaseCleanupPending() {
		t.Fatalf("ACK loss did not retain durable cleanup: %+v found=%v err=%v", tombstone, found, err)
	}
	f.placement.mu.Lock()
	firstProofID := f.placement.snapshot.SourceDepartureProofID
	firstCalls := f.placement.departureCalls
	f.placement.mu.Unlock()
	if firstCalls != 1 || firstProofID == "" || signer.calls != baseSignCalls ||
		f.placement.commitCalls != baseCommitCalls {
		t.Fatalf("ACK loss calls=%d proof=%q ticket_delta=%d commit_delta=%d", firstCalls, firstProofID,
			signer.calls-baseSignCalls, f.placement.commitCalls-baseCommitCalls)
	}

	restarted := f.restart(nil, nil)
	if err := restarted.EnsureHubDepartureForBattle(ctx, 1001, matchID, binding); err != nil {
		t.Fatalf("idempotent ACK replay: %v", err)
	}
	f.placement.mu.Lock()
	proofIDs := append([]string(nil), f.placement.departureProofIDs...)
	lastProof := f.placement.lastDepartureProof
	f.placement.mu.Unlock()
	if len(proofIDs) != 2 || proofIDs[0] != firstProofID || proofIDs[1] != firstProofID ||
		lastProof.SourceMatchID != 0 || lastProof.TargetMatchID != matchID {
		t.Fatalf("departure proof replay was not stable/exact: ids=%v proof=%+v", proofIDs, lastProof)
	}
	if _, found, err := f.repo.GetAssignment(ctx, 1001); err != nil || found {
		t.Fatalf("confirmed cleanup tombstone survived: found=%v err=%v", found, err)
	}
}

func TestBattleDepartureLocatorTimeoutHasZeroCleanupConfirmationTicketOrCommit(t *testing.T) {
	f := newOwnerCleanupFixture(t)
	ctx := context.Background()
	const matchID = uint64(812)
	binding := f.beginBattleDeparture(t, matchID)
	signer := f.uc.signer.(*fakeSigner)
	baseSignCalls, baseCommitCalls := signer.calls, f.placement.commitCalls
	f.placement.pendingReadErr = errcode.New(errcode.ErrUnavailable, "locator timeout")

	if err := f.uc.EnsureHubDepartureForBattle(ctx, 1001, matchID, binding); errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("locator timeout code=%v err=%v", errcode.As(err), err)
	}
	if f.placement.departureCalls != 0 || signer.calls != baseSignCalls ||
		f.placement.commitCalls != baseCommitCalls {
		t.Fatalf("timeout side effects: confirm=%d ticket_delta=%d commit_delta=%d",
			f.placement.departureCalls, signer.calls-baseSignCalls,
			f.placement.commitCalls-baseCommitCalls)
	}
	current, found, err := f.repo.GetAssignment(ctx, 1001)
	if err != nil || !found || current.GetAssignmentId() != f.source.GetAssignmentId() ||
		current.GetReleaseCleanupPending() {
		t.Fatalf("timeout mutated source assignment: %+v found=%v err=%v", current, found, err)
	}
}

func TestBattleDepartureExactInstanceTeardownCanConfirm(t *testing.T) {
	f := newOwnerCleanupFixture(t)
	ctx := context.Background()
	const matchID = uint64(813)
	binding := f.beginBattleDeparture(t, matchID)
	if err := f.authRepo.RecordInstanceTeardownProof(ctx, f.source.GetHubPodName(),
		f.source.GetHubInstanceUid(), time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := f.uc.EnsureHubDepartureForBattle(ctx, 1001, matchID, binding); err != nil {
		t.Fatalf("exact instance teardown did not unlock departure: %v", err)
	}
	f.placement.mu.Lock()
	confirmed, calls := f.placement.snapshot.SourceDepartureConfirmed, f.placement.departureCalls
	f.placement.mu.Unlock()
	if !confirmed || calls != 1 {
		t.Fatalf("teardown confirmation=%v calls=%d", confirmed, calls)
	}
	if sessions, _ := f.mr.HKeys("pandora:hub:sessions:{" + f.source.GetHubPodName() + "}"); len(sessions) != 0 {
		t.Fatalf("torn-down instance session survived: %v", sessions)
	}
}

func TestHubDeparturePhysicalLineageIgnoresHistoricalLogicalSourceMatch(t *testing.T) {
	f := newOwnerCleanupFixture(t)
	ctx := context.Background()
	current, found, err := f.repo.GetAssignment(ctx, 1001)
	if err != nil || !found {
		t.Fatalf("source assignment missing: found=%v err=%v", found, err)
	}
	next := proto.Clone(current).(*hubv1.HubAssignmentStorageRecord)
	next.SourceMatchId = 7001 // retained audit from an earlier Battle→Hub transition
	if swapped, swapErr := f.repo.CompareAndSwapAssignment(ctx, 1001, current, next,
		f.uc.assignmentSagaTTL()); swapErr != nil || !swapped {
		t.Fatalf("seed historical source_match_id: swapped=%v err=%v", swapped, swapErr)
	}
	f.source = next
	const matchID = uint64(816)
	binding := f.beginBattleDeparture(t, matchID)
	finishSourceDeparture(t, f, f.uc)
	if err := f.uc.EnsureHubDepartureForBattle(ctx, 1001, matchID, binding); err != nil {
		t.Fatalf("historical logical source match blocked physical proof: %v", err)
	}
	f.placement.mu.Lock()
	proof := f.placement.lastDepartureProof
	f.placement.mu.Unlock()
	if proof.SourceMatchID != 0 {
		t.Fatalf("historical logical match leaked into Hub physical proof: %+v", proof)
	}
}

func TestTerminalCancellationHubProofUsesPhysicalSourceMatchZero(t *testing.T) {
	f := newOwnerCleanupFixture(t)
	ctx := context.Background()
	const cancelledMatchID = uint64(814)
	targetBinding := placement.Binding{Version: f.source.GetPlacementVersion() + 1,
		OperationID: "323e4567-e89b-42d3-a456-426614174000", SourceMatchID: cancelledMatchID}
	f.placement.mu.Lock()
	f.placement.snapshot = data.HubPlacementSnapshot{
		Found: true, Binding: targetBinding,
		SourceBinding: placement.Binding{Version: f.source.GetPlacementVersion(),
			OperationID: f.source.GetPlacementOperationId()},
		SourceTarget:    placementTargetFromAssignment(f.source),
		CurrentRoute:    locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		ProofType:       locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_TERMINAL,
	}
	f.placement.target = placement.Target{}
	f.placement.mu.Unlock()
	signer := f.uc.signer.(*fakeSigner)
	baseSignCalls := signer.calls

	if got, err := f.uc.AssignHubWithPlacement(ctx, 1001, "global", 0, 0, targetBinding); errcode.As(err) != errcode.ErrUnavailable || got != nil {
		t.Fatalf("connected terminal-cancel source published target: got=%+v err=%v", got, err)
	}
	if f.placement.departureCalls != 0 || signer.calls != baseSignCalls {
		t.Fatalf("pre-departure confirm=%d ticket_delta=%d", f.placement.departureCalls,
			signer.calls-baseSignCalls)
	}
	target, found, err := f.repo.GetAssignment(ctx, 1001)
	if err != nil || !found || !target.GetTransferCleanupPending() {
		t.Fatalf("terminal-cancel cleanup target=%+v found=%v err=%v", target, found, err)
	}
	assertExactSourceEviction(t, f, f.uc, target)
	finishSourceDeparture(t, f, f.uc)
	got, err := f.uc.AssignHubWithPlacement(ctx, 1001, "global", 0, 0, targetBinding)
	if err != nil || got == nil || got.HubTicket == "" {
		t.Fatalf("terminal-cancel recovery=%+v err=%v", got, err)
	}
	f.placement.mu.Lock()
	proof := f.placement.lastDepartureProof
	f.placement.mu.Unlock()
	if proof.SourceMatchID != 0 || proof.TargetMatchID != 0 ||
		proof.TargetRoute != placement.RouteHub || proof.ProofType != placement.ProofHubDeparture ||
		proof.PlacementVersion != targetBinding.Version || proof.OperationID != targetBinding.OperationID {
		t.Fatalf("logical cancelled match leaked into physical Hub proof: %+v", proof)
	}
	if signer.calls != baseSignCalls+1 {
		t.Fatalf("ticket signed before/extra after confirm: delta=%d", signer.calls-baseSignCalls)
	}
}

func TestBattleToHubConsumesBattleConfirmationWithoutMintingIt(t *testing.T) {
	f := newOwnerCleanupFixture(t)
	ctx := context.Background()
	finishSourceDeparture(t, f, f.uc)
	if err := f.uc.ReleaseHub(ctx, 1001); err != nil {
		t.Fatalf("remove old Hub assignment: %v", err)
	}
	const matchID = uint64(815)
	targetBinding := placement.Binding{Version: 3,
		OperationID: "423e4567-e89b-42d3-a456-426614174000", SourceMatchID: matchID}
	battleSource := placement.Target{PodName: "battle-source", InstanceUID: "battle-source-uid",
		InstanceEpoch: 5, AllocationID: "battle-source-allocation", ReleaseTrack: releasetrack.Stable}
	f.placement.mu.Lock()
	f.placement.snapshot = data.HubPlacementSnapshot{
		Found: true, Binding: targetBinding,
		SourceBinding: placement.Binding{Version: 2,
			OperationID: "523e4567-e89b-42d3-a456-426614174000"},
		SourceTarget: battleSource,
		CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE, CurrentMatchID: matchID,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		ProofType:       locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_TERMINAL,
	}
	f.placement.target = placement.Target{}
	baseConfirmCalls, baseCommitCalls := f.placement.departureCalls, f.placement.commitCalls
	f.placement.mu.Unlock()
	signer := f.uc.signer.(*fakeSigner)
	baseSignCalls := signer.calls

	if got, err := f.uc.AssignHubWithPlacement(ctx, 1001, "global", 0, 0, targetBinding); errcode.As(err) != errcode.ErrLocatorConflict || got != nil {
		t.Fatalf("unconfirmed Battle source got Hub ticket: got=%+v err=%v", got, err)
	}
	target, found, err := f.repo.GetAssignment(ctx, 1001)
	if err != nil || !found {
		t.Fatalf("recoverable target assignment missing: %+v found=%v err=%v", target, found, err)
	}
	cred := f.sourceCredential
	if target.GetHubPodName() != f.source.GetHubPodName() {
		cred = f.targetCredential
	}
	if admitted, admissionErr := f.uc.AcknowledgeAdmissionWithPlacement(ctx, 1001,
		target.GetAssignmentId(), target.GetHubPodName(), uuid.NewString(), 2, targetBinding, cred); errcode.As(admissionErr) != errcode.ErrLocatorConflict || admitted != nil {
		t.Fatalf("unconfirmed Battle source reached local Admission: got=%+v err=%v", admitted, admissionErr)
	}
	if signer.calls != baseSignCalls || f.placement.departureCalls != baseConfirmCalls ||
		f.placement.commitCalls != baseCommitCalls {
		t.Fatalf("Hub forged/progressed Battle departure: ticket_delta=%d confirm_delta=%d commit_delta=%d",
			signer.calls-baseSignCalls, f.placement.departureCalls-baseConfirmCalls,
			f.placement.commitCalls-baseCommitCalls)
	}

	f.placement.mu.Lock()
	f.placement.snapshot.SourceDepartureConfirmed = true
	f.placement.snapshot.SourceDepartureProofType = locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_BATTLE_DEPARTURE
	f.placement.snapshot.SourceDepartureProofID = "battle-departure:815:1001"
	f.placement.mu.Unlock()
	assigned, err := f.uc.AssignHubWithPlacement(ctx, 1001, "global", 0, 0, targetBinding)
	if err != nil || assigned == nil || assigned.HubTicket == "" {
		t.Fatalf("confirmed Battle source did not publish Hub target: %+v err=%v", assigned, err)
	}
	admissionID := uuid.NewString()
	admitted, err := f.uc.AcknowledgeAdmissionWithPlacement(ctx, 1001,
		target.GetAssignmentId(), target.GetHubPodName(), admissionID, 2, targetBinding, cred)
	if err != nil || admitted == nil || !admitted.Admitted || !admitted.PlacementCommitted {
		t.Fatalf("confirmed Battle source admission=%+v err=%v", admitted, err)
	}
	if f.placement.departureCalls != baseConfirmCalls {
		t.Fatalf("Hub minted Battle proof calls=%d want=%d", f.placement.departureCalls, baseConfirmCalls)
	}
}

func TestBattleDepartureMissingAssignmentSucceedsOnlyAfterExactLedgerAbsence(t *testing.T) {
	f := newOwnerCleanupFixture(t)
	ctx := context.Background()
	const matchID = uint64(802)
	binding := f.beginBattleDeparture(t, matchID)
	finishSourceDeparture(t, f, f.uc)
	if swapped, err := f.repo.CompareAndSwapAssignment(ctx, 1001, f.source, nil, 0); err != nil || !swapped {
		t.Fatalf("remove canonical assignment: swapped=%v err=%v", swapped, err)
	}
	if err := f.uc.EnsureHubDepartureForBattle(ctx, 1001, matchID, binding); err != nil {
		t.Fatalf("exact AlreadyAbsent source must converge: %v", err)
	}
	if _, found, err := f.repo.GetAssignment(ctx, 1001); err != nil || found {
		t.Fatalf("temporary cleanup tombstone survived: found=%v err=%v", found, err)
	}
	members, err := f.repo.ListShardMembers(ctx, f.source.GetHubPodName())
	if err != nil || len(members) != 0 {
		t.Fatalf("stale source member index survived: members=%v err=%v", members, err)
	}
}

func TestBattleDepartureRejectsAssignmentABAThatDiffersFromPlacementSource(t *testing.T) {
	f := newOwnerCleanupFixture(t)
	ctx := context.Background()
	const matchID = uint64(803)
	binding := f.beginBattleDeparture(t, matchID)
	aba := proto.Clone(f.source).(*hubv1.HubAssignmentStorageRecord)
	aba.AssignmentId = uuid.NewString()
	if swapped, err := f.repo.CompareAndSwapAssignment(ctx, 1001, f.source, aba, f.uc.assignmentSagaTTL()); err != nil || !swapped {
		t.Fatalf("inject assignment ABA: swapped=%v err=%v", swapped, err)
	}
	if err := f.uc.EnsureHubDepartureForBattle(ctx, 1001, matchID, binding); errcode.As(err) != errcode.ErrLocatorConflict {
		t.Fatalf("assignment ABA must fail closed: %v", err)
	}
	if f.placement.departureCalls != 0 {
		t.Fatalf("assignment ABA reached locator confirmation: calls=%d", f.placement.departureCalls)
	}
	if sessions, _ := f.mr.HKeys("pandora:hub:sessions:{" + f.source.GetHubPodName() + "}"); len(sessions) != 1 || sessions[0] != f.source.GetAssignmentId() {
		t.Fatalf("ABA attempt changed exact source session: %v", sessions)
	}
	refs, err := f.repo.ListTransferCleanups(ctx, f.source.GetHubPodName())
	if err != nil || len(refs) != 0 {
		t.Fatalf("ABA attempt published cleanup: refs=%+v err=%v", refs, err)
	}
}

func TestBattleDepartureMissingAssignmentWithoutSourceTupleIsUnknown(t *testing.T) {
	f := newOwnerCleanupFixture(t)
	ctx := context.Background()
	const matchID = uint64(804)
	binding := f.beginBattleDeparture(t, matchID)
	f.placement.mu.Lock()
	f.placement.snapshot.SourceTarget = placement.Target{}
	f.placement.mu.Unlock()
	if swapped, err := f.repo.CompareAndSwapAssignment(ctx, 1001, f.source, nil, 0); err != nil || !swapped {
		t.Fatalf("remove canonical assignment: swapped=%v err=%v", swapped, err)
	}
	if err := f.uc.EnsureHubDepartureForBattle(ctx, 1001, matchID, binding); errcode.As(err) != errcode.ErrLocatorConflict {
		t.Fatalf("missing source tuple must remain UNKNOWN: %v", err)
	}
	if f.placement.departureCalls != 0 {
		t.Fatalf("UNKNOWN source reached locator confirmation: calls=%d", f.placement.departureCalls)
	}
	if _, found, err := f.repo.GetAssignment(ctx, 1001); err != nil || found {
		t.Fatalf("malformed source created a tombstone: found=%v err=%v", found, err)
	}
}

func TestReleaseHubDurableTombstoneWaitsForPhysicalDeparture(t *testing.T) {
	f := newOwnerCleanupFixture(t)
	ctx := context.Background()
	if err := f.uc.ReleaseHub(ctx, 1001); errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("ReleaseHub must wait for source DS departure: %v", err)
	}
	tombstone, found, err := f.repo.GetAssignment(ctx, 1001)
	if err != nil || !found || !tombstone.GetReleaseCleanupPending() {
		t.Fatalf("release tombstone=%+v found=%v err=%v", tombstone, found, err)
	}
	assertExactSourceEviction(t, f, f.uc, tombstone)
	finishSourceDeparture(t, f, f.uc)

	deleteFault := &commitThenErrorAssignmentRepo{HubRepo: f.repo,
		shouldFail: func(expected, next *hubv1.HubAssignmentStorageRecord) bool {
			return expected != nil && expected.GetReleaseCleanupPending() && next == nil
		}}
	restarted := f.restart(deleteFault, nil)
	if err := restarted.reconcileOwnerCleanups(ctx); errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("committed tombstone delete response loss: %v", err)
	}
	if _, found, err := f.repo.GetAssignment(ctx, 1001); err != nil || found {
		t.Fatalf("committed release tombstone still present: found=%v err=%v", found, err)
	}
	fresh := f.restart(nil, nil)
	if err := fresh.reconcileOwnerCleanups(ctx); err != nil {
		t.Fatalf("release orphan index cleanup: %v", err)
	}
	if err := fresh.ReleaseHub(ctx, 1001); err != nil {
		t.Fatalf("idempotent release replay: %v", err)
	}
	refs, _ := f.repo.ListTransferCleanups(ctx, f.source.GetHubPodName())
	if len(refs) != 0 {
		t.Fatalf("release cleanup ref remained: %+v", refs)
	}
	if sessions, _ := f.mr.HKeys("pandora:hub:sessions:{" + f.source.GetHubPodName() + "}"); len(sessions) != 0 {
		t.Fatalf("release left source physical owner: %v", sessions)
	}
}

func TestCleanupAssignmentCASComparisonIncludesNewPhaseFields(t *testing.T) {
	// Small regression guard: a rolling binary must compare the additive phase
	// fields, not silently overwrite them as unknown/default values.
	base := &hubv1.HubAssignmentStorageRecord{PlayerId: 1, AssignmentId: "a"}
	pending := proto.Clone(base).(*hubv1.HubAssignmentStorageRecord)
	pending.TransferCleanupPending = true
	if proto.Equal(base, pending) {
		t.Fatal("cleanup phase field was not part of protobuf equality")
	}
}
