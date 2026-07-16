package data

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"

	"github.com/luyuancpp/pandora/pkg/placement"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/model"
)

type failSecondBindOnceClient struct {
	*fakePlacementClient
	bindAttempts int
}

func (f *failSecondBindOnceClient) BindPlacementTarget(
	ctx context.Context,
	req *locatorv1.BindPlacementTargetRequest,
	opts ...grpc.CallOption,
) (*locatorv1.BindPlacementTargetResponse, error) {
	f.bindAttempts++
	if f.bindAttempts == 2 {
		return nil, errors.New("injected bind transport failure")
	}
	return f.fakePlacementClient.BindPlacementTarget(ctx, req, opts...)
}

func TestPrepareBattlePlacementRetriesPartialBatchWithExactOperation(t *testing.T) {
	signer, err := placement.NewProofSigner("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	base := &fakePlacementClient{signer: signer, records: map[uint64]*locatorv1.PlayerPlacementStorageRecord{
		1: exactHubPlacement(1, 3),
		2: exactHubPlacement(2, 9),
	}}
	client := &failSecondBindOnceClient{fakePlacementClient: base}
	coordinator := NewGrpcPlacementCoordinator(client, signer, 30*time.Minute)
	operationID := uuid.NewString()
	allocation := &model.BattleAllocation{Address: "10.0.0.10:7777", Target: placement.Target{
		PodName: "battle-partial", InstanceUID: "uid-partial", InstanceEpoch: 4,
		AllocationID: "allocation-partial", ReleaseTrack: "stable",
	}}

	if _, err := coordinator.PrepareBattlePlacement(context.Background(), operationID, 808, []uint64{1, 2}, allocation); err == nil {
		t.Fatal("injected second-player Bind failure was not returned")
	}
	for _, playerID := range []uint64{1, 2} {
		rec := base.records[playerID]
		if !sameBattlePendingOperation(rec, operationID, 808) {
			t.Fatalf("player %d did not retain exact retryable operation: %+v", playerID, rec)
		}
	}
	if base.records[1].GetDsInstanceUid() != allocation.Target.InstanceUID ||
		base.records[2].GetDsInstanceUid() != "" {
		t.Fatalf("fault did not create the intended partial bind: p1=%+v p2=%+v", base.records[1], base.records[2])
	}

	bindings, err := coordinator.PrepareBattlePlacement(context.Background(), operationID, 808, []uint64{1, 2}, allocation)
	if err != nil {
		t.Fatalf("exact operation retry did not converge: %v", err)
	}
	for _, playerID := range []uint64{1, 2} {
		rec := base.records[playerID]
		binding := bindings[playerID]
		if rec.GetDsInstanceUid() != allocation.Target.InstanceUID || rec.GetAllocationId() != allocation.Target.AllocationID ||
			binding.Version != rec.GetVersion() || binding.OperationID != operationID {
			t.Fatalf("player %d did not converge to exact target/binding: rec=%+v binding=%+v", playerID, rec, binding)
		}
	}
	if base.beginCalls != 4 || client.bindAttempts != 4 || base.bindCalls != 3 {
		t.Fatalf("unexpected retry calls: begin=%d bind_attempts=%d committed_binds=%d",
			base.beginCalls, client.bindAttempts, base.bindCalls)
	}
}

func TestPrepareBattlePlacementPreflightFindsLaterConflictBeforeAnyBegin(t *testing.T) {
	signer, err := placement.NewProofSigner("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	client := &fakePlacementClient{signer: signer, records: map[uint64]*locatorv1.PlayerPlacementStorageRecord{
		1: exactHubPlacement(1, 3),
		2: {PlayerId: 2, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE, MatchId: 700,
			TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE, Version: 4},
	}}
	coordinator := NewGrpcPlacementCoordinator(client, signer, 30*time.Minute)
	allocation := &model.BattleAllocation{Address: "10.0.0.11:7777", Target: placement.Target{
		PodName: "battle-preflight", InstanceUID: "uid-preflight", InstanceEpoch: 5,
		AllocationID: "allocation-preflight", ReleaseTrack: "stable",
	}}
	if _, err := coordinator.PrepareBattlePlacement(context.Background(), uuid.NewString(), 809, []uint64{1, 2}, allocation); err == nil {
		t.Fatal("later conflicting player passed batch preflight")
	}
	if client.beginCalls != 0 || client.bindCalls != 0 ||
		client.records[1].GetTransitionState() != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE {
		t.Fatalf("preflight conflict leaked an earlier player side effect: begin=%d bind=%d p1=%+v",
			client.beginCalls, client.bindCalls, client.records[1])
	}
}
