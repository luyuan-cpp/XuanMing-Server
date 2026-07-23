package biz

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/releasetrack"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/data"
)

type ownerCleanupFixture struct {
	uc                *HubUsecase
	repo              *data.RedisHubRepo
	authRepo          *data.RedisHubAuthRepo
	mr                *miniredis.Miniredis
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
	if _, err := uc.AssignHub(ctx, 1001, "global", 0, 0, 0, ""); err != nil {
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
	if got, err := uc.AcknowledgeAdmission(ctx, 1001, source.GetAssignmentId(), pod1,
		sourceAdmissionID, 1, "", sourceCredential); err != nil || !got.Admitted {
		t.Fatalf("source admission=%+v err=%v", got, err)
	}
	return &ownerCleanupFixture{uc: uc, repo: repo, authRepo: authRepo, mr: mr, source: source,
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
	return uc
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
	got, err := uc.AcknowledgeAdmission(context.Background(), 1001,
		target.GetAssignmentId(), target.GetHubPodName(), uuid.NewString(), 2, "", f.targetCredential)
	if err != nil || !got.Admitted {
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
