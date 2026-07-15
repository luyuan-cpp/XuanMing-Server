package biz

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/luyuancpp/pandora/pkg/placement"
	matchv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/match/v1"
	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/model"
)

type switchingAllocationStub struct {
	first         model.BattleAllocation
	second        model.BattleAllocation
	allocateCalls int
	signCalls     int
}

func (s *switchingAllocationStub) AllocateBattle(context.Context, uint64, []uint64, uint32) (*model.BattleAllocation, error) {
	s.allocateCalls++
	allocation := s.first
	if s.allocateCalls > 1 {
		allocation = s.second
	}
	return &allocation, nil
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

type incompletePlacementBatch struct{}

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

type failOnceHubDeparture struct{ calls int }

func (f *failOnceHubDeparture) EnsureHubDeparted(context.Context, []uint64) error {
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
