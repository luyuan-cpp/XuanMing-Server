package data

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
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
		rec.SourcePlacementVersion = rec.GetVersion()
		rec.SourceOperationId = rec.GetOperationId()
		rec.SourceDsPodName = rec.GetDsPodName()
		rec.SourceDsInstanceUid = rec.GetDsInstanceUid()
		rec.SourceDsInstanceEpoch = rec.GetDsInstanceEpoch()
		rec.SourceHubAssignmentId = rec.GetHubAssignmentId()
		rec.SourceAllocationId = rec.GetAllocationId()
		rec.SourceReleaseTrack = rec.GetReleaseTrack()
		rec.Version++
		rec.TargetRoute = locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE
		rec.TransitionState = locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING
		rec.OperationId = req.GetOperationId()
		rec.SourceMatchId = req.GetSourceMatchId()
		rec.TargetMatchId = req.GetTargetMatchId()
		rec.ProofType = req.GetProofType()
		rec.ProofId = req.GetProofId()
		rec.AdmissionId = ""
		rec.DsPodName = ""
		rec.DsInstanceUid = ""
		rec.DsInstanceEpoch = 0
		rec.HubAssignmentId = ""
		rec.AllocationId = ""
		rec.ReleaseTrack = ""
		rec.SourceDepartureConfirmed = false
		rec.SourceDepartureProofType = locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_UNSPECIFIED
		rec.SourceDepartureProofId = ""
	}
	rec.UpdatedAtMs = time.Now().UnixMilli()
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
		1: exactHubPlacement(1, 7),
		2: exactHubPlacement(2, 11),
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
	fake.records[1].UpdatedAtMs = time.Now().Add(-2 * time.Minute).UnixMilli()
	fake.records[1].LeaseDeadlineMs = time.Now().Add(-time.Minute).UnixMilli()
	if _, err := coordinator.PrepareBattlePlacement(context.Background(), opID, 99, []uint64{1}, allocation); err != nil {
		t.Fatalf("retry same operation: %v", err)
	}
	if fake.records[1].GetLeaseDeadlineMs() <= time.Now().UnixMilli() || fake.beginCalls != 3 || fake.bindCalls != 3 {
		t.Fatalf("same operation was not renewed/rebound: deadline=%d begin=%d bind=%d",
			fake.records[1].GetLeaseDeadlineMs(), fake.beginCalls, fake.bindCalls)
	}
}

func exactHubPlacement(playerID, version uint64) *locatorv1.PlayerPlacementStorageRecord {
	return &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: playerID, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE,
		Version:         version, OperationId: uuid.NewString(), DsPodName: "hub-a", DsInstanceUid: "hub-uid-a",
		DsInstanceEpoch: 3, HubAssignmentId: uuid.NewString(), ReleaseTrack: "stable",
		UpdatedAtMs: time.Now().Add(-time.Minute).UnixMilli(),
	}
}

func exactUnboundBattlePending(playerID, matchID uint64, operationID string) *locatorv1.PlayerPlacementStorageRecord {
	source := exactHubPlacement(playerID, 7)
	now := time.Now()
	return &locatorv1.PlayerPlacementStorageRecord{
		PlayerId:        playerID,
		CurrentRoute:    locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		Version:         8, OperationId: operationID, TargetMatchId: matchID,
		UpdatedAtMs: now.UnixMilli(), LeaseDeadlineMs: now.Add(30 * time.Minute).UnixMilli(),
		ProofType:              locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_START,
		ProofId:                operationID,
		SourcePlacementVersion: source.GetVersion(), SourceOperationId: source.GetOperationId(),
		SourceDsPodName: source.GetDsPodName(), SourceDsInstanceUid: source.GetDsInstanceUid(),
		SourceDsInstanceEpoch: source.GetDsInstanceEpoch(),
		SourceHubAssignmentId: source.GetHubAssignmentId(), SourceReleaseTrack: source.GetReleaseTrack(),
	}
}

func bindBattleTarget(rec *locatorv1.PlayerPlacementStorageRecord, target placement.Target) {
	rec.DsPodName = target.PodName
	rec.DsInstanceUid = target.InstanceUID
	rec.DsInstanceEpoch = target.InstanceEpoch
	rec.HubAssignmentId = target.AssignmentID
	rec.AllocationId = target.AllocationID
	rec.ReleaseTrack = target.ReleaseTrack
}

func confirmHubSourceDeparture(rec *locatorv1.PlayerPlacementStorageRecord) {
	rec.SourceDepartureConfirmed = true
	rec.SourceDepartureProofType = locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_HUB_DEPARTURE
	rec.SourceDepartureProofId = "hub-departure:canonical-proof"
}

func TestPrepareBattlePlacementResumesExactTargetAfterHubDepartureConfirmation(t *testing.T) {
	signer, err := placement.NewProofSigner("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	operationID := uuid.NewString()
	const matchID = uint64(903)
	target := placement.Target{PodName: "battle-pod-903", InstanceUID: "battle-uid-903",
		InstanceEpoch: 4, AllocationID: uuid.NewString(), ReleaseTrack: "stable"}
	confirmed := exactUnboundBattlePending(1, matchID, operationID)
	bindBattleTarget(confirmed, target)
	confirmHubSourceDeparture(confirmed)
	unconfirmed := exactUnboundBattlePending(2, matchID, operationID)
	bindBattleTarget(unconfirmed, target)
	fake := &fakePlacementClient{signer: signer, records: map[uint64]*locatorv1.PlayerPlacementStorageRecord{
		1: confirmed,
		2: unconfirmed,
	}}
	coordinator := NewGrpcPlacementCoordinator(fake, signer, 30*time.Minute)
	allocation := &model.BattleAllocation{Address: "10.0.0.9:7777", Target: target}

	bindings, err := coordinator.PrepareBattlePlacement(context.Background(), operationID,
		matchID, []uint64{1, 2}, allocation)
	if err != nil {
		t.Fatalf("resume exact prepared placement: %v", err)
	}
	if fake.beginCalls != 2 || fake.bindCalls != 2 {
		t.Fatalf("same operation was not replayed exactly: begin=%d bind=%d",
			fake.beginCalls, fake.bindCalls)
	}
	if binding := bindings[1]; binding.Version != confirmed.GetVersion() ||
		binding.OperationID != operationID || binding.SourceMatchID != 0 {
		t.Fatalf("confirmed player binding mismatch: %+v", binding)
	}
	if !confirmed.GetSourceDepartureConfirmed() ||
		confirmed.GetSourceDepartureProofType() != locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_HUB_DEPARTURE ||
		confirmed.GetSourceDepartureProofId() != "hub-departure:canonical-proof" {
		t.Fatalf("same-operation Begin/Bind lost Hub departure confirmation: %+v", confirmed)
	}
}

func TestPrepareBattlePlacementRejectsMalformedConfirmedRetryBeforeWrites(t *testing.T) {
	signer, err := placement.NewProofSigner("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	operationID := uuid.NewString()
	const matchID = uint64(904)
	target := placement.Target{PodName: "battle-pod-904", InstanceUID: "battle-uid-904",
		InstanceEpoch: 7, AllocationID: uuid.NewString(), ReleaseTrack: "stable"}
	base := exactUnboundBattlePending(1, matchID, operationID)
	bindBattleTarget(base, target)
	confirmHubSourceDeparture(base)

	mutations := []struct {
		name   string
		mutate func(*locatorv1.PlayerPlacementStorageRecord)
	}{
		{name: "confirmed but target unbound", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			bindBattleTarget(rec, placement.Target{})
		}},
		{name: "confirmed with partial target", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			bindBattleTarget(rec, placement.Target{})
			rec.DsPodName = target.PodName
		}},
		{name: "different pod", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.DsPodName = "battle-other"
		}},
		{name: "different uid", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.DsInstanceUid = "battle-uid-other"
		}},
		{name: "different epoch", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.DsInstanceEpoch++
		}},
		{name: "different allocation", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.AllocationId = uuid.NewString()
		}},
		{name: "different release track", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.ReleaseTrack = "canary"
		}},
		{name: "wrong departure proof type", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.SourceDepartureProofType = locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_BATTLE_DEPARTURE
		}},
		{name: "empty departure proof id", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.SourceDepartureProofId = ""
		}},
		{name: "unconfirmed with stale proof", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.SourceDepartureConfirmed = false
		}},
		{name: "different operation", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.OperationId = uuid.NewString()
		}},
		{name: "different match", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.TargetMatchId++
		}},
		{name: "different source lineage", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.SourcePlacementVersion--
		}},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			rec := proto.Clone(base).(*locatorv1.PlayerPlacementStorageRecord)
			tc.mutate(rec)
			fake := &fakePlacementClient{signer: signer,
				records: map[uint64]*locatorv1.PlayerPlacementStorageRecord{1: rec}}
			coordinator := NewGrpcPlacementCoordinator(fake, signer, 30*time.Minute)
			allocation := &model.BattleAllocation{Address: "10.0.0.9:7777", Target: target}
			_, prepareErr := coordinator.PrepareBattlePlacement(context.Background(), operationID,
				matchID, []uint64{1}, allocation)
			if prepareErr == nil || errcode.As(prepareErr) != errcode.ErrLocatorConflict {
				t.Fatalf("malformed confirmed retry did not conflict: %v", prepareErr)
			}
			if fake.beginCalls != 0 || fake.bindCalls != 0 {
				t.Fatalf("malformed retry wrote placement: begin=%d bind=%d",
					fake.beginCalls, fake.bindCalls)
			}
		})
	}
}

func TestPreflightBattlePlacementAllowsOnlyExactStableHubOrCanonicalUnboundPending(t *testing.T) {
	operationID := uuid.NewString()
	const matchID = uint64(901)
	basePending := exactUnboundBattlePending(1, matchID, operationID)

	accepted := []struct {
		name string
		rec  *locatorv1.PlayerPlacementStorageRecord
	}{
		{name: "exact stable Hub", rec: exactHubPlacement(1, 7)},
		{name: "canonical unbound same operation", rec: basePending},
	}
	for _, tc := range accepted {
		t.Run("accept "+tc.name, func(t *testing.T) {
			client := &fakePlacementClient{records: map[uint64]*locatorv1.PlayerPlacementStorageRecord{
				1: proto.Clone(tc.rec).(*locatorv1.PlayerPlacementStorageRecord),
			}}
			coordinator := NewGrpcPlacementCoordinator(client, nil, 30*time.Minute)
			if err := coordinator.PreflightBattlePlacement(context.Background(), operationID, matchID, []uint64{1}); err != nil {
				t.Fatalf("canonical placement rejected: %v", err)
			}
		})
	}

	mutations := []struct {
		name   string
		mutate func(*locatorv1.PlayerPlacementStorageRecord)
	}{
		{name: "complete target already bound", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.DsPodName, rec.DsInstanceUid, rec.AllocationId, rec.ReleaseTrack = "battle-a", "uid-a", "allocation-a", "stable"
			rec.DsInstanceEpoch = 3
		}},
		{name: "partial target", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.DsPodName = "battle-partial"
		}},
		{name: "Hub assignment in Battle target", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.HubAssignmentId = uuid.NewString()
		}},
		{name: "different operation", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.OperationId = uuid.NewString()
		}},
		{name: "different match", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.TargetMatchId++
		}},
		{name: "source version not immediate predecessor", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.SourcePlacementVersion--
		}},
		{name: "source operation equals transition", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.SourceOperationId = rec.GetOperationId()
		}},
		{name: "partial source target", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.SourceDsInstanceUid = ""
		}},
		{name: "Battle-shaped source target", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.SourceHubAssignmentId = ""
			rec.SourceAllocationId = "source-allocation"
		}},
		{name: "nonzero source match", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.SourceMatchId = 77
		}},
		{name: "wrong proof type", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.ProofType = locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_TERMINAL
		}},
		{name: "proof id not operation", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.ProofId = uuid.NewString()
		}},
		{name: "active departure confirmation", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.SourceDepartureConfirmed = true
			rec.SourceDepartureProofType = locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_HUB_DEPARTURE
			rec.SourceDepartureProofId = uuid.NewString()
		}},
		{name: "partial departure marker", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.SourceDepartureProofId = uuid.NewString()
		}},
		{name: "cross-lineage departure history", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.LastSourceDepartureProofType = locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_HUB_DEPARTURE
			rec.LastSourceDepartureProofId = uuid.NewString()
			rec.LastSourceDeparturePlacementVersion = rec.GetSourcePlacementVersion() - 1
			rec.LastSourceDepartureOperationId = rec.GetSourceOperationId()
		}},
		{name: "admission already present", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.AdmissionId = uuid.NewString()
		}},
		{name: "lease predates update", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.LeaseDeadlineMs = rec.GetUpdatedAtMs()
		}},
		{name: "record player mismatch", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.PlayerId = 2
		}},
	}
	for _, tc := range mutations {
		t.Run("reject "+tc.name, func(t *testing.T) {
			rec := proto.Clone(basePending).(*locatorv1.PlayerPlacementStorageRecord)
			tc.mutate(rec)
			client := &fakePlacementClient{records: map[uint64]*locatorv1.PlayerPlacementStorageRecord{1: rec}}
			coordinator := NewGrpcPlacementCoordinator(client, nil, 30*time.Minute)
			if err := coordinator.PreflightBattlePlacement(context.Background(), operationID, matchID, []uint64{1}); err == nil || errcode.As(err) != errcode.ErrLocatorConflict {
				t.Fatalf("non-canonical placement did not fail with a definite conflict: %v", err)
			}
		})
	}
}

func TestPreflightBattlePlacementRejectsMalformedStableHub(t *testing.T) {
	operationID := uuid.NewString()
	const matchID = uint64(902)
	mutations := []struct {
		name   string
		mutate func(*locatorv1.PlayerPlacementStorageRecord)
	}{
		{name: "active source tuple", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.SourceDsPodName = "stale-source"
		}},
		{name: "partial active departure marker", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.SourceDepartureProofId = uuid.NewString()
		}},
		{name: "cross-lineage departure history", mutate: func(rec *locatorv1.PlayerPlacementStorageRecord) {
			rec.LastSourceDepartureProofType = locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_BATTLE_DEPARTURE
			rec.LastSourceDepartureProofId = uuid.NewString()
			rec.LastSourceDeparturePlacementVersion = rec.GetVersion() - 1
			rec.LastSourceDepartureOperationId = rec.GetOperationId()
		}},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			rec := exactHubPlacement(1, 7)
			tc.mutate(rec)
			client := &fakePlacementClient{records: map[uint64]*locatorv1.PlayerPlacementStorageRecord{1: rec}}
			coordinator := NewGrpcPlacementCoordinator(client, nil, 30*time.Minute)
			if err := coordinator.PreflightBattlePlacement(context.Background(), operationID, matchID, []uint64{1}); err == nil || errcode.As(err) != errcode.ErrLocatorConflict {
				t.Fatalf("malformed STABLE_HUB did not fail with a definite conflict: %v", err)
			}
		})
	}
}
