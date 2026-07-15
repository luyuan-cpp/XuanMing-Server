package data

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/placement"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/model"
)

type fakePlacementClient struct {
	signer     *placement.ProofSigner
	records    map[uint64]*locatorv1.PlayerPlacementStorageRecord
	beginCalls int
	bindCalls  int
}

func (f *fakePlacementClient) GetPlacement(_ context.Context, req *locatorv1.GetPlacementRequest, _ ...grpc.CallOption) (*locatorv1.GetPlacementResponse, error) {
	rec, ok := f.records[req.GetPlayerId()]
	if !ok {
		return &locatorv1.GetPlacementResponse{Code: commonv1.ErrCode_OK}, nil
	}
	return &locatorv1.GetPlacementResponse{Code: commonv1.ErrCode_OK, Found: true,
		Placement: proto.Clone(rec).(*locatorv1.PlayerPlacementStorageRecord)}, nil
}

func (f *fakePlacementClient) BeginPlacementTransition(_ context.Context, req *locatorv1.BeginPlacementTransitionRequest, _ ...grpc.CallOption) (*locatorv1.BeginPlacementTransitionResponse, error) {
	f.beginCalls++
	rec := f.records[req.GetPlayerId()]
	proof := placement.Proof{PlayerID: req.GetPlayerId(), ExpectedVersion: req.GetExpectedVersion(),
		SourceRoute: placement.RouteHub, TargetRoute: placement.RouteBattle,
		TargetMatchID: req.GetTargetMatchId(), ProofType: placement.ProofMatchStart,
		ProofID: req.GetProofId(), OperationID: req.GetOperationId()}
	if rec == nil || !f.signer.Verify(proof, req.GetProofSignature()) {
		return &locatorv1.BeginPlacementTransitionResponse{Code: commonv1.ErrCode_ERR_PERMISSION_DENY}, nil
	}
	if rec.GetTransitionState() == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE {
		rec.Version++
		rec.TargetRoute = locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE
		rec.TransitionState = locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING
		rec.OperationId = req.GetOperationId()
		rec.TargetMatchId = req.GetTargetMatchId()
	}
	rec.LeaseDeadlineMs = req.GetLeaseDeadlineMs()
	return &locatorv1.BeginPlacementTransitionResponse{Code: commonv1.ErrCode_OK,
		Placement: proto.Clone(rec).(*locatorv1.PlayerPlacementStorageRecord)}, nil
}

func (f *fakePlacementClient) BindPlacementTarget(_ context.Context, req *locatorv1.BindPlacementTargetRequest, _ ...grpc.CallOption) (*locatorv1.BindPlacementTargetResponse, error) {
	f.bindCalls++
	rec := f.records[req.GetPlayerId()]
	rec.DsPodName = req.GetDsPodName()
	rec.DsInstanceUid = req.GetDsInstanceUid()
	rec.DsInstanceEpoch = req.GetDsInstanceEpoch()
	rec.AllocationId = req.GetAllocationId()
	rec.ReleaseTrack = req.GetReleaseTrack()
	return &locatorv1.BindPlacementTargetResponse{Code: commonv1.ErrCode_OK,
		Placement: proto.Clone(rec).(*locatorv1.PlayerPlacementStorageRecord)}, nil
}

func TestPrepareBattlePlacementBindsFullTargetAndRenewsSameOperation(t *testing.T) {
	signer, err := placement.NewProofSigner("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakePlacementClient{signer: signer, records: map[uint64]*locatorv1.PlayerPlacementStorageRecord{
		1: {PlayerId: 1, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
			TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE, Version: 7},
		2: {PlayerId: 2, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
			TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE, Version: 11},
	}}
	coordinator := NewGrpcPlacementCoordinator(fake, signer, 30*time.Minute)
	opID := uuid.NewString()
	allocation := &model.BattleAllocation{Address: "10.0.0.1:7777", Target: placement.Target{
		PodName: "battle-pod-1", InstanceUID: "uid-1", InstanceEpoch: 9,
		AllocationID: uuid.NewString(), ReleaseTrack: "stable",
	}}
	bindings, err := coordinator.PrepareBattlePlacement(context.Background(), opID, 99, []uint64{1, 2}, allocation)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	for _, pid := range []uint64{1, 2} {
		rec := fake.records[pid]
		if rec.GetOperationId() != opID || rec.GetTargetMatchId() != 99 ||
			rec.GetDsPodName() != allocation.Target.PodName || rec.GetDsInstanceUid() != allocation.Target.InstanceUID ||
			rec.GetDsInstanceEpoch() != allocation.Target.InstanceEpoch || rec.GetAllocationId() != allocation.Target.AllocationID ||
			rec.GetReleaseTrack() != allocation.Target.ReleaseTrack {
			t.Fatalf("player %d target not fully bound: %+v", pid, rec)
		}
		if binding := bindings[pid]; binding.Version != rec.GetVersion() || binding.OperationID != opID || !binding.Complete() {
			t.Fatalf("player %d binding not returned from authoritative placement: %+v", pid, binding)
		}
	}
	// 同 operation 重放必须先 Begin 续租，再幂等 Bind；不能因旧 pending 卡死。
	fake.records[1].LeaseDeadlineMs = time.Now().Add(-time.Minute).UnixMilli()
	if _, err := coordinator.PrepareBattlePlacement(context.Background(), opID, 99, []uint64{1}, allocation); err != nil {
		t.Fatalf("retry same operation: %v", err)
	}
	if fake.records[1].GetLeaseDeadlineMs() <= time.Now().UnixMilli() || fake.beginCalls != 3 || fake.bindCalls != 3 {
		t.Fatalf("same operation was not renewed/rebound: deadline=%d begin=%d bind=%d",
			fake.records[1].GetLeaseDeadlineMs(), fake.beginCalls, fake.bindCalls)
	}
}
