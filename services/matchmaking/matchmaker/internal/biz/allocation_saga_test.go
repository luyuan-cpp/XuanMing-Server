package biz

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"
	matchv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/match/v1"
	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/conf"
	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/data"
	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/model"
	"google.golang.org/protobuf/proto"
)

type switchingAllocationStub struct {
	first            model.BattleAllocation
	second           model.BattleAllocation
	allocateCalls    int
	signCalls        int
	abortCalls       int
	abortErr         error
	abortMatchID     uint64
	abortOperationID string
	abortAllocation  *model.BattleAllocation
}

func assertAllocationOwnershipIntact(t *testing.T, ctx context.Context, f *fixture, matchID uint64) {
	t.Helper()
	for _, ticketID := range []uint64{100, 200} {
		ticket, found, err := f.repo.GetTicket(ctx, ticketID)
		if err != nil || !found || ticket.GetMatchId() != matchID {
			t.Fatalf("allocation race changed ticket %d: found=%v ticket=%+v err=%v",
				ticketID, found, ticket, err)
		}
	}
	for playerID := uint64(1); playerID <= 10; playerID++ {
		ticketID, found, err := f.repo.GetPlayerTicket(ctx, playerID)
		if err != nil || !found || (ticketID != 100 && ticketID != 200) {
			t.Fatalf("allocation race changed player %d claim: ticket=%d found=%v err=%v",
				playerID, ticketID, found, err)
		}
	}
}

func (s *switchingAllocationStub) AllocateBattle(context.Context, uint64, []uint64, uint32) (*model.BattleAllocation, error) {
	s.allocateCalls++
	allocation := s.first
	if s.allocateCalls > 1 {
		allocation = s.second
	}
	return &allocation, nil
}

func (s *switchingAllocationStub) AbortBattleAllocation(_ context.Context, matchID uint64, operationID string, allocation *model.BattleAllocation) error {
	s.abortCalls++
	s.abortMatchID = matchID
	s.abortOperationID = operationID
	if allocation != nil {
		copy := *allocation
		s.abortAllocation = &copy
	}
	return s.abortErr
}

func (s *switchingAllocationStub) SignBattleTickets(
	_ context.Context,
	matchID uint64,
	playerIDs []uint64,
	_ *model.BattleAllocation,
	_ map[uint64]placement.Binding,
) (map[uint64]string, error) {
	s.signCalls++
	tickets := make(map[uint64]string, len(playerIDs))
	for _, playerID := range playerIDs {
		tickets[playerID] = fmt.Sprintf("ticket-%d-%d", matchID, playerID)
	}
	return tickets, nil
}

func (*switchingAllocationStub) SignBattleTicket(context.Context, uint64, uint64, *model.BattleAllocation, placement.Binding) (string, error) {
	return "ticket", nil
}

type failFirstPlacementBatch struct {
	calls       int
	allocations []model.BattleAllocation
}

func (*failFirstPlacementBatch) RequireStableHub(context.Context, []uint64) error { return nil }

func (*failFirstPlacementBatch) PreflightBattlePlacement(context.Context, string, uint64, []uint64) error {
	return nil
}

func (p *failFirstPlacementBatch) PrepareBattlePlacement(
	_ context.Context,
	operationID string,
	_ uint64,
	playerIDs []uint64,
	allocation *model.BattleAllocation,
) (map[uint64]placement.Binding, error) {
	p.calls++
	if allocation != nil {
		p.allocations = append(p.allocations, *allocation)
	}
	if p.calls == 1 {
		// The real coordinator may already have committed Begin/Bind for an
		// earlier player before a later player's RPC fails.
		return nil, errors.New("injected failure after first player was bound")
	}
	bindings := make(map[uint64]placement.Binding, len(playerIDs))
	for _, playerID := range playerIDs {
		bindings[playerID] = placement.Binding{Version: 8, OperationID: operationID}
	}
	return bindings, nil
}

func TestAllocationSagaCheckpointsExactTargetBeforePartialPlacementAndRestart(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 9901)
	seedAllocatingMatch(t, ctx, f, 9901, time.Now().Add(time.Minute).UnixMilli())

	first := model.BattleAllocation{Address: "10.0.0.1:7777", Target: placement.Target{
		PodName: "battle-a", InstanceUID: "uid-a", InstanceEpoch: 1,
		AllocationID: "allocation-a", ReleaseTrack: "stable",
	}}
	second := model.BattleAllocation{Address: "10.0.0.2:7777", Target: placement.Target{
		PodName: "battle-b", InstanceUID: "uid-b", InstanceEpoch: 2,
		AllocationID: "allocation-b", ReleaseTrack: "stable",
	}}
	allocator := &switchingAllocationStub{first: first, second: second}
	placementBatch := &failFirstPlacementBatch{}
	f.uc.allocator = allocator
	f.uc.SetPlacementCoordinator(placementBatch)

	if err := f.uc.advanceAllocationsOnce(ctx); err == nil {
		t.Fatal("partial placement failure must keep the durable job retryable")
	}
	afterFailure, found, err := f.repo.GetMatch(ctx, 9901)
	if err != nil || !found || afterFailure.GetStage() != stageAllocating {
		t.Fatalf("partial failure lost ALLOCATING job: found=%v match=%+v err=%v", found, afterFailure, err)
	}
	checkpoint, complete := allocationFromStoredTarget(afterFailure.GetBattleTarget())
	if !complete || !sameBattleAllocation(checkpoint, &first) || len(afterFailure.GetBattleTarget().GetPlayerBindings()) != 0 {
		t.Fatalf("exact target was not checkpointed before placement: %+v", afterFailure.GetBattleTarget())
	}
	if afterFailure.GetBattleDsAddr() != "" || f.pusher.lastStageFor(1) == stageReady || allocator.signCalls != 0 {
		t.Fatalf("partial placement leaked READY: ds=%q pushed=%s sign_calls=%d",
			afterFailure.GetBattleDsAddr(), f.pusher.lastStageFor(1), allocator.signCalls)
	}

	// Simulate a process restart.  The allocator deliberately returns a different
	// target on its second call; the restarted worker must not call it because the
	// canonical exact target is already checkpointed.
	if err := f.repo.UpdateMatchWithLock(ctx, 9901, f.cfg.OptimisticRetry, func(rec *matchv1.MatchStorageRecord) error {
		rec.AllocationNextAttemptAtMs = 0
		return nil
	}, f.cfg.MatchTTL.Std()); err != nil {
		t.Fatal(err)
	}
	restarted := NewMatchUsecase(f.repo, nil, f.pusher, allocator, &fakeIDGen{next: 10000}, f.locator, f.cfg)
	restarted.SetPlacementCoordinator(placementBatch)
	if err := restarted.advanceAllocationsOnce(ctx); err != nil {
		t.Fatalf("restart could not resume exact placement operation: %v", err)
	}

	ready, found, err := f.repo.GetMatch(ctx, 9901)
	if err != nil || !found || ready.GetStage() != stageReady {
		t.Fatalf("exact retry did not reach READY: found=%v match=%+v err=%v", found, ready, err)
	}
	readyTarget, complete := allocationFromStoredTarget(ready.GetBattleTarget())
	if !complete || !sameBattleAllocation(readyTarget, &first) || ready.GetBattleDsAddr() != first.Address {
		t.Fatalf("READY drifted to a new allocation: %+v", ready.GetBattleTarget())
	}
	if allocator.allocateCalls != 1 || allocator.signCalls != 1 || placementBatch.calls != 2 {
		t.Fatalf("unexpected saga calls: allocate=%d sign=%d placement=%d",
			allocator.allocateCalls, allocator.signCalls, placementBatch.calls)
	}
	if len(placementBatch.allocations) != 2 || !sameBattleAllocation(&placementBatch.allocations[0], &first) ||
		!sameBattleAllocation(&placementBatch.allocations[1], &first) {
		t.Fatalf("placement retry did not reuse exact target: %+v", placementBatch.allocations)
	}
	if len(ready.GetBattleTarget().GetPlayerBindings()) != len(ready.GetMembers()) {
		t.Fatalf("READY published without the complete binding set: %+v", ready.GetBattleTarget())
	}
}

func TestCheckpointBattleAllocationRejectsAbortingEvenForSameTarget(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 9909)
	seedAllocatingMatch(t, ctx, f, 9909, time.Now().Add(time.Minute).UnixMilli())
	allocation := &model.BattleAllocation{Address: "10.0.0.9:7777", Target: placement.Target{
		PodName: "battle-checkpoint-abort", InstanceUID: "uid-checkpoint-abort", InstanceEpoch: 9,
		AllocationID: "allocation-checkpoint-abort", ReleaseTrack: "stable"}}
	operationID := allocationOperationID()
	if err := f.repo.UpdateMatchWithLock(ctx, 9909, f.cfg.OptimisticRetry,
		func(rec *matchv1.MatchStorageRecord) error {
			rec.AllocationOperationId = operationID
			rec.AllocationPhase = matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_REQUESTING
			return nil
		}, f.cfg.MatchTTL.Std()); err != nil {
		t.Fatal(err)
	}
	job, found, err := f.repo.GetMatch(ctx, 9909)
	if err != nil || !found {
		t.Fatalf("get requesting job: found=%v err=%v", found, err)
	}
	if err := f.repo.UpdateMatchWithLock(ctx, 9909, f.cfg.OptimisticRetry,
		func(rec *matchv1.MatchStorageRecord) error {
			rec.BattleTarget = battleTargetStorage(allocation, nil)
			return nil
		}, f.cfg.MatchTTL.Std()); err != nil {
		t.Fatal(err)
	}
	if got, err := f.uc.checkpointBattleAllocation(ctx, job, allocation); err != nil ||
		!sameBattleAllocation(got, allocation) {
		t.Fatalf("same REQUESTING checkpoint was not idempotent: got=%+v err=%v", got, err)
	}
	if err := f.repo.UpdateMatchWithLock(ctx, 9909, f.cfg.OptimisticRetry,
		func(rec *matchv1.MatchStorageRecord) error {
			rec.AllocationPhase = matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_ABORTING
			return nil
		}, f.cfg.MatchTTL.Std()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.uc.checkpointBattleAllocation(ctx, job, allocation); errcode.As(err) != errcode.ErrMatchConcurrent {
		t.Fatalf("ABORTING checkpoint code=%d err=%v, want ErrMatchConcurrent", errcode.As(err), err)
	}
	current, found, err := f.repo.GetMatch(ctx, 9909)
	stored, complete := allocationFromStoredTarget(current.GetBattleTarget())
	if err != nil || !found || current.GetStage() != stageAllocating ||
		current.GetAllocationPhase() != matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_ABORTING ||
		current.GetAllocationOperationId() != operationID || !complete || !sameBattleAllocation(stored, allocation) {
		t.Fatalf("checkpoint overwrote ABORTING: found=%v match=%+v err=%v", found, current, err)
	}
	assertAllocationOwnershipIntact(t, ctx, f, 9909)
}

type abortBeforePostCheckpointFenceRepo struct {
	data.MatchRepo
	matchID   uint64
	triggered atomic.Bool
}

func (r *abortBeforePostCheckpointFenceRepo) UpdateMatchWithLock(
	ctx context.Context,
	matchID uint64,
	maxRetry int,
	mutate func(*matchv1.MatchStorageRecord) error,
	ttl time.Duration,
) error {
	if matchID == r.matchID && !r.triggered.Load() {
		current, found, err := r.MatchRepo.GetMatch(ctx, matchID)
		if err != nil {
			return err
		}
		if found && current.GetStage() == stageAllocating &&
			current.GetAllocationPhase() == matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_REQUESTING &&
			current.GetBattleTarget() != nil && r.triggered.CompareAndSwap(false, true) {
			if err := r.MatchRepo.UpdateMatchWithLock(ctx, matchID, maxRetry,
				func(rec *matchv1.MatchStorageRecord) error {
					rec.AllocationPhase = matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_ABORTING
					return nil
				}, ttl); err != nil {
				return err
			}
		}
	}
	return r.MatchRepo.UpdateMatchWithLock(ctx, matchID, maxRetry, mutate, ttl)
}

type hookPlacementBatch struct {
	allowPlacement
	calls int
	hook  func() error
}

func (p *hookPlacementBatch) PrepareBattlePlacement(
	ctx context.Context,
	operationID string,
	matchID uint64,
	playerIDs []uint64,
	allocation *model.BattleAllocation,
) (map[uint64]placement.Binding, error) {
	p.calls++
	bindings, err := p.allowPlacement.PrepareBattlePlacement(ctx, operationID, matchID, playerIDs, allocation)
	if err != nil {
		return nil, err
	}
	if p.hook != nil {
		if err := p.hook(); err != nil {
			return nil, err
		}
	}
	return bindings, nil
}

type hookHubDeparture struct {
	calls int
	hook  func() error
}

func (h *hookHubDeparture) EnsureHubDeparted(context.Context, uint64, string,
	[]uint64, map[uint64]placement.Binding,
) error {
	h.calls++
	if h.hook != nil {
		return h.hook()
	}
	return nil
}

func TestAbortFenceStopsPostCheckpointExternalSideEffects(t *testing.T) {
	t.Run("before placement", func(t *testing.T) {
		ctx := context.Background()
		f := newFixture(t, 9930)
		seedAllocatingMatch(t, ctx, f, 9930, time.Now().Add(time.Minute).UnixMilli())
		allocation := model.BattleAllocation{Address: "10.0.0.30:7777", Target: placement.Target{
			PodName: "battle-before-placement", InstanceUID: "uid-before-placement", InstanceEpoch: 30,
			AllocationID: "allocation-before-placement", ReleaseTrack: "stable"}}
		allocator := &switchingAllocationStub{first: allocation, second: allocation}
		placementBatch := &hookPlacementBatch{}
		hubDeparture := &hookHubDeparture{}
		f.uc.allocator = allocator
		f.uc.SetPlacementCoordinator(placementBatch)
		f.uc.SetHubDepartureCoordinator(hubDeparture)
		f.uc.repo = &abortBeforePostCheckpointFenceRepo{MatchRepo: f.repo, matchID: 9930}
		initial, found, err := f.repo.GetMatch(ctx, 9930)
		if err != nil || !found {
			t.Fatalf("get initial allocation: found=%v err=%v", found, err)
		}
		if err := f.uc.advanceAllocation(ctx, initial); errcode.As(err) != errcode.ErrMatchConcurrent {
			t.Fatalf("post-checkpoint fence result=%v", err)
		}
		current, found, err := f.repo.GetMatch(ctx, 9930)
		stored, complete := allocationFromStoredTarget(current.GetBattleTarget())
		if err != nil || !found || current.GetStage() != stageAllocating ||
			current.GetAllocationPhase() != matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_ABORTING ||
			!complete || !sameBattleAllocation(stored, &allocation) || allocator.allocateCalls != 1 ||
			placementBatch.calls != 0 || hubDeparture.calls != 0 || allocator.signCalls != 0 {
			t.Fatalf("abort-before-placement leaked side effects: found=%v match=%+v allocate=%d placement=%d departure=%d sign=%d err=%v",
				found, current, allocator.allocateCalls, placementBatch.calls, hubDeparture.calls, allocator.signCalls, err)
		}
		assertAllocationOwnershipIntact(t, ctx, f, 9930)
	})

	stages := []struct {
		name              string
		matchID           uint64
		abortAfterPrepare bool
		wantDeparture     int
	}{
		{name: "between placement and Hub departure", matchID: 9931, abortAfterPrepare: true},
		{name: "between Hub departure and ticket signing", matchID: 9932, wantDeparture: 1},
	}
	for _, tc := range stages {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t, tc.matchID)
			seedAllocatingMatch(t, ctx, f, tc.matchID, time.Now().Add(time.Minute).UnixMilli())
			allocation := model.BattleAllocation{Address: "10.0.0.31:7777", Target: placement.Target{
				PodName: "battle-post-step", InstanceUID: "uid-post-step", InstanceEpoch: 31,
				AllocationID: "allocation-post-step", ReleaseTrack: "stable"}}
			allocator := &switchingAllocationStub{first: allocation, second: allocation}
			abort := func() error {
				return f.repo.UpdateMatchWithLock(ctx, tc.matchID, f.cfg.OptimisticRetry,
					func(rec *matchv1.MatchStorageRecord) error {
						if rec.GetBattleTarget() == nil ||
							rec.GetAllocationPhase() != matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_REQUESTING {
							return errors.New("post-checkpoint operation was not REQUESTING")
						}
						rec.AllocationPhase = matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_ABORTING
						return nil
					}, f.cfg.MatchTTL.Std())
			}
			placementBatch := &hookPlacementBatch{}
			hubDeparture := &hookHubDeparture{}
			if tc.abortAfterPrepare {
				placementBatch.hook = abort
			} else {
				hubDeparture.hook = abort
			}
			f.uc.allocator = allocator
			f.uc.SetPlacementCoordinator(placementBatch)
			f.uc.SetHubDepartureCoordinator(hubDeparture)
			initial, found, err := f.repo.GetMatch(ctx, tc.matchID)
			if err != nil || !found {
				t.Fatalf("get initial allocation: found=%v err=%v", found, err)
			}
			if err := f.uc.advanceAllocation(ctx, initial); errcode.As(err) != errcode.ErrMatchConcurrent {
				t.Fatalf("post-step fence result=%v", err)
			}
			current, found, err := f.repo.GetMatch(ctx, tc.matchID)
			stored, complete := allocationFromStoredTarget(current.GetBattleTarget())
			if err != nil || !found || current.GetStage() != stageAllocating ||
				current.GetAllocationPhase() != matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_ABORTING ||
				!complete || !sameBattleAllocation(stored, &allocation) || allocator.allocateCalls != 1 ||
				placementBatch.calls != 1 || hubDeparture.calls != tc.wantDeparture || allocator.signCalls != 0 {
				t.Fatalf("post-step abort leaked later side effects: found=%v match=%+v allocate=%d placement=%d departure=%d sign=%d err=%v",
					found, current, allocator.allocateCalls, placementBatch.calls, hubDeparture.calls, allocator.signCalls, err)
			}
			assertAllocationOwnershipIntact(t, ctx, f, tc.matchID)
		})
	}
}

type incompletePlacementBatch struct{}

func (incompletePlacementBatch) RequireStableHub(context.Context, []uint64) error { return nil }

func (incompletePlacementBatch) PreflightBattlePlacement(context.Context, string, uint64, []uint64) error {
	return nil
}

func (incompletePlacementBatch) PrepareBattlePlacement(
	_ context.Context,
	operationID string,
	_ uint64,
	playerIDs []uint64,
	_ *model.BattleAllocation,
) (map[uint64]placement.Binding, error) {
	bindings := make(map[uint64]placement.Binding, len(playerIDs)-1)
	for _, playerID := range playerIDs[:len(playerIDs)-1] {
		bindings[playerID] = placement.Binding{Version: 2, OperationID: operationID}
	}
	return bindings, nil
}

type conflictPlacementBatch struct {
	allowPlacement
	preflightErr  error
	prepareErr    error
	preflightCall int
	prepareCall   int
}

func (p *conflictPlacementBatch) PreflightBattlePlacement(context.Context, string, uint64, []uint64) error {
	p.preflightCall++
	return p.preflightErr
}

func (p *conflictPlacementBatch) PrepareBattlePlacement(
	_ context.Context,
	operationID string,
	_ uint64,
	playerIDs []uint64,
	_ *model.BattleAllocation,
) (map[uint64]placement.Binding, error) {
	p.prepareCall++
	if p.prepareErr != nil {
		return nil, p.prepareErr
	}
	bindings := make(map[uint64]placement.Binding, len(playerIDs))
	for _, playerID := range playerIDs {
		bindings[playerID] = placement.Binding{Version: 2, OperationID: operationID}
	}
	return bindings, nil
}

func assertPlacementConflictReleasedMatchClaims(t *testing.T, ctx context.Context, f *fixture, matchID uint64) {
	t.Helper()
	failed, found, err := f.repo.GetMatch(ctx, matchID)
	if err != nil || !found || failed.GetStage() != stageFailed ||
		failed.GetAllocationPhase() != matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_FAILED {
		t.Fatalf("placement conflict not durably FAILED: found=%v match=%+v err=%v", found, failed, err)
	}
	for _, ticketID := range []uint64{100, 200} {
		if _, found, err := f.repo.GetTicket(ctx, ticketID); err != nil || found {
			t.Fatalf("placement conflict retained ticket %d: found=%v err=%v", ticketID, found, err)
		}
	}
	for playerID := uint64(1); playerID <= 10; playerID++ {
		if _, found, err := f.repo.GetPlayerTicket(ctx, playerID); err != nil || found {
			t.Fatalf("placement conflict retained player %d claim: found=%v err=%v", playerID, found, err)
		}
	}
}

func TestAllocationPreflightDefiniteConflictFailsBeforeExternalAllocate(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 9910)
	seedAllocatingMatch(t, ctx, f, 9910, time.Now().Add(time.Minute).UnixMilli())
	allocator := &switchingAllocationStub{first: model.BattleAllocation{Address: "10.0.0.10:7777",
		Target: placement.Target{PodName: "must-not-start", InstanceUID: "uid-x", InstanceEpoch: 1,
			AllocationID: "allocation-x", ReleaseTrack: "stable"}}}
	gate := &conflictPlacementBatch{preflightErr: errcode.New(errcode.ErrLocatorConflict,
		"player still has authoritative Battle placement")}
	f.uc.allocator = allocator
	f.uc.SetPlacementCoordinator(gate)

	if err := f.uc.advanceAllocationsOnce(ctx); err == nil || errcode.As(err) != errcode.ErrLocatorConflict {
		t.Fatalf("definite preflight conflict err=%v", err)
	}
	if allocator.allocateCalls != 0 || allocator.signCalls != 0 || gate.preflightCall != 1 || gate.prepareCall != 0 {
		t.Fatalf("preflight conflict leaked external side effect: allocate=%d sign=%d preflight=%d prepare=%d",
			allocator.allocateCalls, allocator.signCalls, gate.preflightCall, gate.prepareCall)
	}
	assertPlacementConflictReleasedMatchClaims(t, ctx, f, 9910)
}

func TestPlacementConflictCannotOverwriteChangedUncheckpointedGeneration(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 9914)
	seedAllocatingMatch(t, ctx, f, 9914, time.Now().Add(time.Minute).UnixMilli())
	operationID := allocationOperationID()
	if err := f.repo.UpdateMatchWithLock(ctx, 9914, f.cfg.OptimisticRetry,
		func(rec *matchv1.MatchStorageRecord) error {
			rec.AllocationOperationId = operationID
			rec.AllocationPhase = matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_REQUESTING
			return nil
		}, f.cfg.MatchTTL.Std()); err != nil {
		t.Fatal(err)
	}
	job, found, err := f.repo.GetMatch(ctx, 9914)
	if err != nil || !found {
		t.Fatalf("get requesting job: found=%v err=%v", found, err)
	}
	if err := f.repo.UpdateMatchWithLock(ctx, 9914, f.cfg.OptimisticRetry,
		func(rec *matchv1.MatchStorageRecord) error {
			// Even a malformed/partially repaired ABORTING generation must never
			// be overwritten by an older REQUESTING placement result.
			rec.AllocationPhase = matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_ABORTING
			return nil
		}, f.cfg.MatchTTL.Std()); err != nil {
		t.Fatal(err)
	}
	cause := errcode.New(errcode.ErrLocatorConflict, "stale placement conflict")
	if err := f.uc.failAllocationPlacementConflict(ctx, job, cause); errcode.As(err) != errcode.ErrLocatorConflict {
		t.Fatalf("stale placement conflict result=%v", err)
	}
	current, found, err := f.repo.GetMatch(ctx, 9914)
	if err != nil || !found || current.GetStage() != stageAllocating ||
		current.GetAllocationPhase() != matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_ABORTING ||
		current.GetAllocationOperationId() != operationID || current.GetBattleTarget() != nil {
		t.Fatalf("stale placement conflict overwrote generation: found=%v match=%+v err=%v", found, current, err)
	}
	assertAllocationOwnershipIntact(t, ctx, f, 9914)
}

type blockingOfflineLocator struct {
	*mockLocator
	started chan struct{}
	proceed chan struct{}
}

func (l *blockingOfflineLocator) FindOfflinePlayers(context.Context, []uint64) ([]uint64, error) {
	close(l.started)
	<-l.proceed
	return []uint64{1}, nil
}

func TestAllocationLivenessFailureCannotOverwriteCheckpointedAbortingGeneration(t *testing.T) {
	ctx := context.Background()
	f := newFixtureWith(t, 9915, func(c *conf.MatchConf) { c.LivenessGateEnabled = true })
	seedAllocatingMatch(t, ctx, f, 9915, time.Now().Add(time.Minute).UnixMilli())
	locator := &blockingOfflineLocator{mockLocator: f.locator,
		started: make(chan struct{}), proceed: make(chan struct{})}
	f.uc.locator = locator
	allocator := &switchingAllocationStub{}
	f.uc.allocator = allocator

	initial, found, err := f.repo.GetMatch(ctx, 9915)
	if err != nil || !found {
		t.Fatalf("get initial allocation: found=%v err=%v", found, err)
	}
	done := make(chan error, 1)
	go func() { done <- f.uc.advanceAllocation(ctx, initial) }()
	select {
	case <-locator.started:
	case <-time.After(2 * time.Second):
		t.Fatal("allocation worker did not reach liveness query")
	}
	allocation := model.BattleAllocation{Address: "10.0.0.15:7777", Target: placement.Target{
		PodName: "battle-liveness-race", InstanceUID: "uid-liveness-race", InstanceEpoch: 15,
		AllocationID: "allocation-liveness-race", ReleaseTrack: "stable"}}
	if err := f.repo.UpdateMatchWithLock(ctx, 9915, f.cfg.OptimisticRetry,
		func(rec *matchv1.MatchStorageRecord) error {
			if rec.GetAllocationPhase() != matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_REQUESTING ||
				!placement.ValidOperationID(rec.GetAllocationOperationId()) {
				return errors.New("worker did not checkpoint REQUESTING generation")
			}
			rec.BattleTarget = battleTargetStorage(&allocation, nil)
			rec.AllocationPhase = matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_ABORTING
			return nil
		}, f.cfg.MatchTTL.Std()); err != nil {
		t.Fatal(err)
	}
	close(locator.proceed)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stale liveness result unexpectedly committed FAILED")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("liveness worker did not return")
	}
	current, found, err := f.repo.GetMatch(ctx, 9915)
	checkpoint, complete := allocationFromStoredTarget(current.GetBattleTarget())
	if err != nil || !found || current.GetStage() != stageAllocating ||
		current.GetAllocationPhase() != matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_ABORTING ||
		!complete || !sameBattleAllocation(checkpoint, &allocation) || allocator.allocateCalls != 0 {
		t.Fatalf("liveness result overwrote checkpoint/abort fence: found=%v match=%+v allocation=%+v calls=%d err=%v",
			found, current, checkpoint, allocator.allocateCalls, err)
	}
	assertAllocationOwnershipIntact(t, ctx, f, 9915)
}

type blockingDefiniteAllocationStub struct {
	started chan struct{}
	proceed chan struct{}
	err     error
	calls   int
}

func (s *blockingDefiniteAllocationStub) AllocateBattle(context.Context, uint64, []uint64, uint32) (*model.BattleAllocation, error) {
	s.calls++
	close(s.started)
	<-s.proceed
	return nil, s.err
}

func (*blockingDefiniteAllocationStub) AbortBattleAllocation(context.Context, uint64, string, *model.BattleAllocation) error {
	return errors.New("unexpected allocation abort")
}

func (*blockingDefiniteAllocationStub) SignBattleTickets(context.Context, uint64, []uint64, *model.BattleAllocation, map[uint64]placement.Binding) (map[uint64]string, error) {
	return nil, errors.New("unexpected ticket signing")
}

func (*blockingDefiniteAllocationStub) SignBattleTicket(context.Context, uint64, uint64, *model.BattleAllocation, placement.Binding) (string, error) {
	return "", errors.New("unexpected ticket signing")
}

func TestDefinitiveAllocatorFailureCannotOverwriteChangedGeneration(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*matchv1.MatchStorageRecord, *model.BattleAllocation)
	}{
		{name: "checkpointed target", mutate: func(rec *matchv1.MatchStorageRecord, allocation *model.BattleAllocation) {
			rec.BattleTarget = battleTargetStorage(allocation, nil)
		}},
		{name: "aborting checkpoint", mutate: func(rec *matchv1.MatchStorageRecord, allocation *model.BattleAllocation) {
			rec.BattleTarget = battleTargetStorage(allocation, nil)
			rec.AllocationPhase = matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_ABORTING
		}},
		{name: "operation changed", mutate: func(rec *matchv1.MatchStorageRecord, _ *model.BattleAllocation) {
			rec.AllocationOperationId = allocationOperationID()
		}},
		{name: "phase changed", mutate: func(rec *matchv1.MatchStorageRecord, _ *model.BattleAllocation) {
			rec.AllocationPhase = matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_PENDING
		}},
	}
	for index, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			matchID := uint64(9920 + index)
			f := newFixture(t, matchID)
			seedAllocatingMatch(t, ctx, f, matchID, time.Now().Add(time.Minute).UnixMilli())
			allocator := &blockingDefiniteAllocationStub{started: make(chan struct{}), proceed: make(chan struct{}),
				err: errcode.New(errcode.ErrDSNoAvailable, "definitive no capacity")}
			f.uc.allocator = allocator
			initial, found, err := f.repo.GetMatch(ctx, matchID)
			if err != nil || !found {
				t.Fatalf("get initial allocation: found=%v err=%v", found, err)
			}
			done := make(chan error, 1)
			go func() { done <- f.uc.advanceAllocation(ctx, initial) }()
			select {
			case <-allocator.started:
			case <-time.After(2 * time.Second):
				t.Fatal("allocation worker did not reach allocator")
			}
			checkpoint := &model.BattleAllocation{Address: "10.0.0.20:7777", Target: placement.Target{
				PodName: "battle-definite-race", InstanceUID: "uid-definite-race", InstanceEpoch: 20,
				AllocationID: "allocation-definite-race", ReleaseTrack: "stable"}}
			if err := f.repo.UpdateMatchWithLock(ctx, matchID, f.cfg.OptimisticRetry,
				func(rec *matchv1.MatchStorageRecord) error {
					if rec.GetAllocationPhase() != matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_REQUESTING ||
						!placement.ValidOperationID(rec.GetAllocationOperationId()) {
						return errors.New("worker did not checkpoint REQUESTING generation")
					}
					tc.mutate(rec, checkpoint)
					return nil
				}, f.cfg.MatchTTL.Std()); err != nil {
				t.Fatal(err)
			}
			before, _, _ := f.repo.GetMatch(ctx, matchID)
			close(allocator.proceed)
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("stale definitive failure unexpectedly committed FAILED")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("allocation worker did not return")
			}
			current, found, err := f.repo.GetMatch(ctx, matchID)
			if err != nil || !found || current.GetStage() != stageAllocating ||
				current.GetAllocationPhase() != before.GetAllocationPhase() ||
				current.GetAllocationOperationId() != before.GetAllocationOperationId() ||
				!proto.Equal(current.GetBattleTarget(), before.GetBattleTarget()) || allocator.calls != 1 {
				t.Fatalf("definitive failure overwrote generation: found=%v before=%+v current=%+v calls=%d err=%v",
					found, before, current, allocator.calls, err)
			}
			assertAllocationOwnershipIntact(t, ctx, f, matchID)
		})
	}
}

type blockingAbortSuccessAllocator struct {
	started    chan struct{}
	proceed    chan struct{}
	calls      int
	allocation *model.BattleAllocation
}

func (*blockingAbortSuccessAllocator) AllocateBattle(context.Context, uint64, []uint64, uint32) (*model.BattleAllocation, error) {
	return nil, errors.New("unexpected allocation")
}

func (s *blockingAbortSuccessAllocator) AbortBattleAllocation(_ context.Context, _ uint64, _ string, allocation *model.BattleAllocation) error {
	s.calls++
	if allocation != nil {
		copy := *allocation
		s.allocation = &copy
	}
	close(s.started)
	<-s.proceed
	return nil
}

func (*blockingAbortSuccessAllocator) SignBattleTickets(context.Context, uint64, []uint64, *model.BattleAllocation, map[uint64]placement.Binding) (map[uint64]string, error) {
	return nil, errors.New("unexpected ticket signing")
}

func (*blockingAbortSuccessAllocator) SignBattleTicket(context.Context, uint64, uint64, *model.BattleAllocation, placement.Binding) (string, error) {
	return "", errors.New("unexpected ticket signing")
}

func TestAllocationAbortAckCannotFailDriftedTargetSnapshot(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 9925)
	seedAllocatingMatch(t, ctx, f, 9925, time.Now().Add(time.Minute).UnixMilli())
	first := &model.BattleAllocation{Address: "10.0.0.25:7777", Target: placement.Target{
		PodName: "battle-abort-first", InstanceUID: "uid-abort-first", InstanceEpoch: 25,
		AllocationID: "allocation-abort-first", ReleaseTrack: "stable"}}
	second := &model.BattleAllocation{Address: "10.0.0.26:7777", Target: placement.Target{
		PodName: "battle-abort-second", InstanceUID: "uid-abort-second", InstanceEpoch: 26,
		AllocationID: "allocation-abort-second", ReleaseTrack: "stable"}}
	operationID := allocationOperationID()
	if err := f.repo.UpdateMatchWithLock(ctx, 9925, f.cfg.OptimisticRetry,
		func(rec *matchv1.MatchStorageRecord) error {
			rec.AllocationOperationId = operationID
			rec.AllocationPhase = matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_ABORTING
			rec.BattleTarget = battleTargetStorage(first, nil)
			return nil
		}, f.cfg.MatchTTL.Std()); err != nil {
		t.Fatal(err)
	}
	job, found, err := f.repo.GetMatch(ctx, 9925)
	if err != nil || !found {
		t.Fatalf("get abort job: found=%v err=%v", found, err)
	}
	allocator := &blockingAbortSuccessAllocator{started: make(chan struct{}), proceed: make(chan struct{})}
	f.uc.allocator = allocator
	cause := errcode.New(errcode.ErrLocatorConflict, "original placement conflict")
	done := make(chan error, 1)
	go func() { done <- f.uc.advanceAllocationAbort(ctx, job, cause, false) }()
	select {
	case <-allocator.started:
	case <-time.After(2 * time.Second):
		t.Fatal("abort worker did not reach allocator")
	}
	if err := f.repo.UpdateMatchWithLock(ctx, 9925, f.cfg.OptimisticRetry,
		func(rec *matchv1.MatchStorageRecord) error {
			rec.BattleTarget = battleTargetStorage(second, nil)
			return nil
		}, f.cfg.MatchTTL.Std()); err != nil {
		t.Fatal(err)
	}
	close(allocator.proceed)
	select {
	case err := <-done:
		if errcode.As(err) != errcode.ErrLocatorConflict {
			t.Fatalf("drifted abort result=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("abort worker did not return")
	}
	current, found, err := f.repo.GetMatch(ctx, 9925)
	checkpoint, complete := allocationFromStoredTarget(current.GetBattleTarget())
	if err != nil || !found || current.GetStage() != stageAllocating ||
		current.GetAllocationPhase() != matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_ABORTING ||
		current.GetAllocationOperationId() != operationID || !complete || !sameBattleAllocation(checkpoint, second) ||
		allocator.calls != 1 || !sameBattleAllocation(allocator.allocation, first) {
		t.Fatalf("abort ACK failed drifted generation: found=%v match=%+v sent=%+v calls=%d err=%v",
			found, current, allocator.allocation, allocator.calls, err)
	}
	assertAllocationOwnershipIntact(t, ctx, f, 9925)
}

func TestAllocationPrepareDefiniteConflictFailsAndReleasesClaims(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 9911)
	seedAllocatingMatch(t, ctx, f, 9911, time.Now().Add(time.Minute).UnixMilli())
	allocation := model.BattleAllocation{Address: "10.0.0.11:7777", Target: placement.Target{
		PodName: "battle-conflict", InstanceUID: "uid-conflict", InstanceEpoch: 2,
		AllocationID: "allocation-conflict", ReleaseTrack: "stable"}}
	allocator := &switchingAllocationStub{first: allocation, second: allocation}
	gate := &conflictPlacementBatch{prepareErr: errcode.New(errcode.ErrLocatorConflict,
		"placement changed after allocator preflight")}
	f.uc.allocator = allocator
	f.uc.SetPlacementCoordinator(gate)

	if err := f.uc.advanceAllocationsOnce(ctx); err == nil || errcode.As(err) != errcode.ErrLocatorConflict {
		t.Fatalf("definite prepare conflict err=%v", err)
	}
	if allocator.allocateCalls != 1 || allocator.signCalls != 0 || gate.preflightCall != 1 || gate.prepareCall != 1 {
		t.Fatalf("prepare conflict control flow: allocate=%d sign=%d preflight=%d prepare=%d",
			allocator.allocateCalls, allocator.signCalls, gate.preflightCall, gate.prepareCall)
	}
	if allocator.abortCalls != 1 || allocator.abortMatchID != 9911 ||
		allocator.abortOperationID == "" || !sameBattleAllocation(allocator.abortAllocation, &allocation) {
		t.Fatalf("prepare conflict did not abort exact checkpoint: calls=%d match=%d op=%q allocation=%+v",
			allocator.abortCalls, allocator.abortMatchID, allocator.abortOperationID, allocator.abortAllocation)
	}
	assertPlacementConflictReleasedMatchClaims(t, ctx, f, 9911)
	failed, _, _ := f.repo.GetMatch(ctx, 9911)
	stored, complete := allocationFromStoredTarget(failed.GetBattleTarget())
	if !complete || !sameBattleAllocation(stored, &allocation) || failed.GetBattleDsAddr() != "" ||
		f.pusher.lastStageFor(1) == stageReady {
		t.Fatalf("prepare conflict lost exact cleanup target or leaked READY: %+v", failed)
	}
}

func TestAllocationAbortUnknownKeepsFailedSagaActiveAndRestartRetriesExactTarget(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 9912)
	seedAllocatingMatch(t, ctx, f, 9912, time.Now().Add(time.Minute).UnixMilli())
	allocation := model.BattleAllocation{Address: "10.0.0.12:7777", Target: placement.Target{
		PodName: "battle-abort", InstanceUID: "uid-abort", InstanceEpoch: 4,
		AllocationID: "allocation-abort", ReleaseTrack: "stable"}}
	allocator := &switchingAllocationStub{first: allocation, second: allocation,
		abortErr: errcode.New(errcode.ErrUnavailable, "allocator abort ACK lost")}
	gate := &conflictPlacementBatch{prepareErr: errcode.New(errcode.ErrLocatorConflict,
		"placement changed after allocation")}
	f.uc.allocator = allocator
	f.uc.SetPlacementCoordinator(gate)

	if err := f.uc.advanceAllocationsOnce(ctx); err == nil {
		t.Fatal("unknown abort must keep ALLOCATING saga retryable")
	}
	pending, found, err := f.repo.GetMatch(ctx, 9912)
	if err != nil || !found || pending.GetStage() != stageAllocating ||
		pending.GetAllocationPhase() != matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_ABORTING ||
		pending.GetAllocationNextAttemptAtMs() == -1 || allocator.abortCalls != 1 {
		t.Fatalf("abort UNKNOWN advanced canonical state: found=%v match=%+v calls=%d err=%v",
			found, pending, allocator.abortCalls, err)
	}
	for _, ticketID := range []uint64{100, 200} {
		ticket, ticketFound, ticketErr := f.repo.GetTicket(ctx, ticketID)
		if ticketErr != nil || !ticketFound || ticket.GetMatchId() != 9912 {
			t.Fatalf("abort UNKNOWN changed ticket %d: found=%v ticket=%+v err=%v",
				ticketID, ticketFound, ticket, ticketErr)
		}
	}
	for playerID := uint64(1); playerID <= 10; playerID++ {
		ticketID, claimed, claimErr := f.repo.GetPlayerTicket(ctx, playerID)
		if claimErr != nil || !claimed || (ticketID != 100 && ticketID != 200) {
			t.Fatalf("abort UNKNOWN changed player %d claim: ticket=%d claimed=%v err=%v",
				playerID, ticketID, claimed, claimErr)
		}
	}
	active, activeErr := f.repo.RangeActiveMatches(ctx)
	if activeErr != nil || !slices.Contains(active, uint64(9912)) {
		t.Fatalf("abort UNKNOWN removed active recovery job: active=%v err=%v", active, activeErr)
	}
	firstOperation := allocator.abortOperationID
	if firstOperation == "" || !sameBattleAllocation(allocator.abortAllocation, &allocation) ||
		allocator.signCalls != 0 || f.pusher.lastStageFor(1) == stageReady {
		t.Fatalf("abort UNKNOWN drift/leak: op=%q allocation=%+v sign=%d stage=%s",
			firstOperation, allocator.abortAllocation, allocator.signCalls, f.pusher.lastStageFor(1))
	}

	// Simulate a restarted service worker after the remote allocator journal has
	// recognized the first call despite its lost response.
	allocator.abortErr = nil
	if err := f.repo.UpdateMatchWithLock(ctx, 9912, f.cfg.OptimisticRetry,
		func(rec *matchv1.MatchStorageRecord) error {
			rec.AllocationNextAttemptAtMs = 0
			return nil
		}, f.cfg.MatchTTL.Std()); err != nil {
		t.Fatal(err)
	}
	restarted := NewMatchUsecase(f.repo, nil, f.pusher, allocator,
		&fakeIDGen{next: 20000}, f.locator, f.cfg)
	restarted.SetPlacementCoordinator(gate)
	if err := restarted.advanceAllocationsOnce(ctx); err != nil {
		t.Fatalf("restart abort retry: %v", err)
	}
	finished, found, err := f.repo.GetMatch(ctx, 9912)
	if err != nil || !found || finished.GetStage() != stageFailed ||
		finished.GetAllocationNextAttemptAtMs() != -1 || allocator.abortCalls != 2 ||
		allocator.abortOperationID != firstOperation || !sameBattleAllocation(allocator.abortAllocation, &allocation) {
		t.Fatalf("restart did not finish exact abort: found=%v match=%+v calls=%d op=%q allocation=%+v err=%v",
			found, finished, allocator.abortCalls, allocator.abortOperationID, allocator.abortAllocation, err)
	}
	for _, ticketID := range []uint64{100, 200} {
		if _, ticketFound, ticketErr := f.repo.GetTicket(ctx, ticketID); ticketErr != nil || ticketFound {
			t.Fatalf("definitive abort retained ticket %d: found=%v err=%v", ticketID, ticketFound, ticketErr)
		}
	}
}

type abortReadyRaceAllocator struct {
	allocation   model.BattleAllocation
	signStarted  chan struct{}
	signProceed  chan struct{}
	abortStarted chan struct{}
	abortProceed chan struct{}
	signCalls    atomic.Int32
	abortCalls   atomic.Int32
}

func (a *abortReadyRaceAllocator) AllocateBattle(context.Context, uint64, []uint64, uint32) (*model.BattleAllocation, error) {
	copy := a.allocation
	return &copy, nil
}

func (a *abortReadyRaceAllocator) AbortBattleAllocation(context.Context, uint64, string, *model.BattleAllocation) error {
	if a.abortCalls.Add(1) == 1 {
		close(a.abortStarted)
	}
	<-a.abortProceed
	return errcode.New(errcode.ErrUnavailable, "abort response unknown")
}

func (a *abortReadyRaceAllocator) SignBattleTickets(
	_ context.Context, matchID uint64, playerIDs []uint64, _ *model.BattleAllocation,
	_ map[uint64]placement.Binding,
) (map[uint64]string, error) {
	if a.signCalls.Add(1) == 1 {
		close(a.signStarted)
	}
	<-a.signProceed
	tickets := make(map[uint64]string, len(playerIDs))
	for _, playerID := range playerIDs {
		tickets[playerID] = fmt.Sprintf("race-ticket-%d-%d", matchID, playerID)
	}
	return tickets, nil
}

func (*abortReadyRaceAllocator) SignBattleTicket(context.Context, uint64, uint64, *model.BattleAllocation, placement.Binding) (string, error) {
	return "ticket", nil
}

type abortReadyRacePlacement struct{ calls atomic.Int32 }

func (*abortReadyRacePlacement) RequireStableHub(context.Context, []uint64) error { return nil }
func (*abortReadyRacePlacement) PreflightBattlePlacement(context.Context, string, uint64, []uint64) error {
	return nil
}
func (p *abortReadyRacePlacement) PrepareBattlePlacement(
	_ context.Context, operationID string, _ uint64, playerIDs []uint64, _ *model.BattleAllocation,
) (map[uint64]placement.Binding, error) {
	if p.calls.Add(1) > 1 {
		return nil, errcode.New(errcode.ErrLocatorConflict, "concurrent placement contradiction")
	}
	bindings := make(map[uint64]placement.Binding, len(playerIDs))
	for _, playerID := range playerIDs {
		bindings[playerID] = placement.Binding{Version: 8, OperationID: operationID}
	}
	return bindings, nil
}

func TestAllocationAbortingFencePreventsConcurrentReadyCAS(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 9913)
	seedAllocatingMatch(t, ctx, f, 9913, time.Now().Add(time.Minute).UnixMilli())
	allocation := model.BattleAllocation{Address: "10.0.0.13:7777", Target: placement.Target{
		PodName: "battle-race", InstanceUID: "uid-race", InstanceEpoch: 5,
		AllocationID: "allocation-race", ReleaseTrack: "stable"}}
	allocator := &abortReadyRaceAllocator{
		allocation: allocation, signStarted: make(chan struct{}), signProceed: make(chan struct{}),
		abortStarted: make(chan struct{}), abortProceed: make(chan struct{}),
	}
	placementGate := &abortReadyRacePlacement{}
	f.uc.allocator = allocator
	f.uc.SetPlacementCoordinator(placementGate)

	first, found, err := f.repo.GetMatch(ctx, 9913)
	if err != nil || !found {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- f.uc.advanceAllocation(ctx, first) }()
	select {
	case <-allocator.signStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first worker never reached ticket signing")
	}
	// Force a second service replica's retry window while the first has local
	// signed tickets but has not reached the READY CAS.
	if err := f.repo.UpdateMatchWithLock(ctx, 9913, f.cfg.OptimisticRetry,
		func(rec *matchv1.MatchStorageRecord) error {
			rec.AllocationNextAttemptAtMs = 0
			return nil
		}, f.cfg.MatchTTL.Std()); err != nil {
		t.Fatal(err)
	}
	second, found, err := f.repo.GetMatch(ctx, 9913)
	if err != nil || !found {
		t.Fatal(err)
	}
	secondDone := make(chan error, 1)
	go func() { secondDone <- f.uc.advanceAllocation(ctx, second) }()
	select {
	case <-allocator.abortStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("second worker never fenced/started abort")
	}

	close(allocator.signProceed)
	select {
	case readyErr := <-firstDone:
		if readyErr == nil {
			t.Fatal("stale worker unexpectedly committed READY after ABORTING fence")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stale READY worker did not return")
	}
	pending, found, err := f.repo.GetMatch(ctx, 9913)
	if err != nil || !found || pending.GetStage() != stageAllocating ||
		pending.GetAllocationPhase() != matchv1.MatchAllocationPhase_MATCH_ALLOCATION_PHASE_ABORTING ||
		pending.GetBattleDsAddr() != "" || f.pusher.lastStageFor(1) == stageReady {
		t.Fatalf("ABORTING fence leaked READY: found=%v match=%+v pushed=%s err=%v",
			found, pending, f.pusher.lastStageFor(1), err)
	}
	close(allocator.abortProceed)
	select {
	case abortErr := <-secondDone:
		if abortErr == nil {
			t.Fatal("injected unknown abort unexpectedly succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("abort worker did not return")
	}
	for _, ticketID := range []uint64{100, 200} {
		ticket, ticketFound, ticketErr := f.repo.GetTicket(ctx, ticketID)
		if ticketErr != nil || !ticketFound || ticket.GetMatchId() != 9913 {
			t.Fatalf("ABORTING UNKNOWN changed ticket %d: found=%v ticket=%+v err=%v",
				ticketID, ticketFound, ticket, ticketErr)
		}
	}
}

type failOnceHubDeparture struct{ calls int }

func (f *failOnceHubDeparture) EnsureHubDeparted(context.Context, uint64, string,
	[]uint64, map[uint64]placement.Binding) error {
	f.calls++
	if f.calls == 1 {
		return errors.New("old Hub owner still connected")
	}
	return nil
}

func TestAllocationSagaDoesNotSignOrPublishBeforePhysicalHubDeparture(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 9903)
	seedAllocatingMatch(t, ctx, f, 9903, time.Now().Add(time.Minute).UnixMilli())
	allocator := &switchingAllocationStub{first: model.BattleAllocation{
		Address: "10.0.0.4:7777", Target: placement.Target{PodName: "battle-d",
			InstanceUID: "uid-d", InstanceEpoch: 4, AllocationID: "allocation-d", ReleaseTrack: "stable"}}}
	placementBatch := &failFirstPlacementBatch{calls: 1}
	departure := &failOnceHubDeparture{}
	f.uc.allocator = allocator
	f.uc.SetPlacementCoordinator(placementBatch)
	f.uc.SetHubDepartureCoordinator(departure)

	if err := f.uc.advanceAllocationsOnce(ctx); err == nil {
		t.Fatal("connected old Hub owner must keep allocation retryable")
	}
	pending, found, err := f.repo.GetMatch(ctx, 9903)
	if err != nil || !found || pending.GetStage() != stageAllocating ||
		pending.GetBattleDsAddr() != "" || allocator.signCalls != 0 ||
		f.pusher.lastStageFor(1) == stageReady {
		t.Fatalf("departure fence leaked READY: found=%v match=%+v sign=%d err=%v",
			found, pending, allocator.signCalls, err)
	}
	if err := f.repo.UpdateMatchWithLock(ctx, 9903, f.cfg.OptimisticRetry,
		func(rec *matchv1.MatchStorageRecord) error { rec.AllocationNextAttemptAtMs = 0; return nil },
		f.cfg.MatchTTL.Std()); err != nil {
		t.Fatal(err)
	}
	if err := f.uc.advanceAllocationsOnce(ctx); err != nil {
		t.Fatalf("exact departure retry did not converge: %v", err)
	}
	ready, found, err := f.repo.GetMatch(ctx, 9903)
	if err != nil || !found || ready.GetStage() != stageReady || allocator.signCalls != 1 || departure.calls != 2 {
		t.Fatalf("departure retry result found=%v match=%+v sign=%d calls=%d err=%v",
			found, ready, allocator.signCalls, departure.calls, err)
	}
}

func TestAllocationSagaRejectsIncompleteSuccessfulPlacementBatch(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 9902)
	seedAllocatingMatch(t, ctx, f, 9902, time.Now().Add(time.Minute).UnixMilli())
	allocator := &switchingAllocationStub{first: model.BattleAllocation{
		Address: "10.0.0.3:7777",
		Target: placement.Target{PodName: "battle-c", InstanceUID: "uid-c", InstanceEpoch: 3,
			AllocationID: "allocation-c", ReleaseTrack: "stable"},
	}}
	f.uc.allocator = allocator
	f.uc.SetPlacementCoordinator(incompletePlacementBatch{})

	if err := f.uc.advanceAllocationsOnce(ctx); err == nil {
		t.Fatal("incomplete binding set must fail closed")
	}
	match, found, err := f.repo.GetMatch(ctx, 9902)
	if err != nil || !found || match.GetStage() != stageAllocating || match.GetBattleDsAddr() != "" {
		t.Fatalf("incomplete bindings leaked READY: found=%v match=%+v err=%v", found, match, err)
	}
	if allocator.signCalls != 0 || f.pusher.lastStageFor(1) == stageReady {
		t.Fatalf("incomplete bindings reached signer/push: sign_calls=%d stage=%s",
			allocator.signCalls, f.pusher.lastStageFor(1))
	}
}
