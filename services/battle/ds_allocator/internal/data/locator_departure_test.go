package data

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
)

type departureLocatorClientStub struct {
	locatorv1.PlayerLocatorServiceClient
	response *locatorv1.GetPlacementResponse
	err      error
	confirm  func(*locatorv1.ConfirmPlacementSourceDepartureRequest) (*locatorv1.ConfirmPlacementSourceDepartureResponse, error)
}

func (s *departureLocatorClientStub) ConfirmPlacementSourceDeparture(
	_ context.Context,
	req *locatorv1.ConfirmPlacementSourceDepartureRequest,
	_ ...grpc.CallOption,
) (*locatorv1.ConfirmPlacementSourceDepartureResponse, error) {
	if s.confirm == nil {
		return nil, errors.New("unexpected confirmation")
	}
	return s.confirm(req)
}

func (s *departureLocatorClientStub) GetPlacement(
	_ context.Context,
	_ *locatorv1.GetPlacementRequest,
	_ ...grpc.CallOption,
) (*locatorv1.GetPlacementResponse, error) {
	return s.response, s.err
}

func pendingHubFromBattlePlacement() *locatorv1.PlayerPlacementStorageRecord {
	return &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: 10, CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		TransitionState: locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		Version:         18, OperationId: "223e4567-e89b-42d3-a456-426614174001", MatchId: 902,
		SourceMatchId: 902, ProofType: locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_TERMINAL,
		ProofId:                "terminal-902",
		SourcePlacementVersion: 17, SourceOperationId: "123e4567-e89b-42d3-a456-426614174000",
		SourceDsPodName: "battle-exact", SourceDsInstanceUid: "uid-exact",
		SourceDsInstanceEpoch: 7, SourceAllocationId: "92000000-0000-4000-8000-000000000001", SourceReleaseTrack: "stable",
	}
}

func pendingHubBattleDepartureExpected() BattlePlayerDepartureExpected {
	return BattlePlayerDepartureExpected{
		MatchID: 902, PlayerID: 10, PlacementVersion: 18,
		OperationID:            "223e4567-e89b-42d3-a456-426614174001",
		SourcePlacementVersion: 17, SourceOperationID: "123e4567-e89b-42d3-a456-426614174000",
		Source: BattleDepartureSource{DSPodName: "battle-exact", GameServerUID: "uid-exact",
			InstanceEpoch: 7, AllocationID: "92000000-0000-4000-8000-000000000001"},
	}
}

func TestVerifyPendingHubBattleDepartureExactLineage(t *testing.T) {
	reader := &GrpcLocationRefresher{client: &departureLocatorClientStub{response: &locatorv1.GetPlacementResponse{
		Code: commonv1.ErrCode_OK, Found: true, Placement: pendingHubFromBattlePlacement(),
	}}}
	if err := reader.VerifyPendingHubBattleDeparture(context.Background(), pendingHubBattleDepartureExpected()); err != nil {
		t.Fatalf("verify exact pending Hub source lineage: %v", err)
	}
}

func TestVerifyPendingHubBattleDepartureRejectsABAAndUnknown(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*locatorv1.PlayerPlacementStorageRecord)
	}{
		{name: "another transition version", mutate: func(p *locatorv1.PlayerPlacementStorageRecord) { p.Version++ }},
		{name: "another transition operation", mutate: func(p *locatorv1.PlayerPlacementStorageRecord) {
			p.OperationId = "323e4567-e89b-42d3-a456-426614174002"
		}},
		{name: "still stable battle", mutate: func(p *locatorv1.PlayerPlacementStorageRecord) {
			p.TransitionState = locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE
		}},
		{name: "not targeting hub", mutate: func(p *locatorv1.PlayerPlacementStorageRecord) {
			p.TargetRoute = locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE
		}},
		{name: "different source version", mutate: func(p *locatorv1.PlayerPlacementStorageRecord) { p.SourcePlacementVersion++ }},
		{name: "different source operation", mutate: func(p *locatorv1.PlayerPlacementStorageRecord) {
			p.SourceOperationId = "423e4567-e89b-42d3-a456-426614174003"
		}},
		{name: "same pod rebuilt uid", mutate: func(p *locatorv1.PlayerPlacementStorageRecord) { p.SourceDsInstanceUid = "uid-rebuilt" }},
		{name: "different allocation", mutate: func(p *locatorv1.PlayerPlacementStorageRecord) {
			p.SourceAllocationId = "92000000-0000-4000-8000-000000000003"
		}},
		{name: "source hub assignment present", mutate: func(p *locatorv1.PlayerPlacementStorageRecord) { p.SourceHubAssignmentId = "hub-source" }},
		{name: "route changed", mutate: func(p *locatorv1.PlayerPlacementStorageRecord) {
			p.CurrentRoute = locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			placement := pendingHubFromBattlePlacement()
			tc.mutate(placement)
			reader := &GrpcLocationRefresher{client: &departureLocatorClientStub{response: &locatorv1.GetPlacementResponse{
				Code: commonv1.ErrCode_OK, Found: true, Placement: placement,
			}}}
			err := reader.VerifyPendingHubBattleDeparture(context.Background(), pendingHubBattleDepartureExpected())
			if errcode.As(err) != errcode.ErrLocatorConflict {
				t.Fatalf("err=%v code=%v", err, errcode.As(err))
			}
		})
	}

	unknown := &GrpcLocationRefresher{client: &departureLocatorClientStub{response: &locatorv1.GetPlacementResponse{
		Code: commonv1.ErrCode_OK, Found: false,
	}}}
	if err := unknown.VerifyPendingHubBattleDeparture(context.Background(), pendingHubBattleDepartureExpected()); errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("unknown err=%v code=%v", err, errcode.As(err))
	}
	unavailable := &GrpcLocationRefresher{client: &departureLocatorClientStub{err: errors.New("locator down")}}
	if err := unavailable.VerifyPendingHubBattleDeparture(context.Background(), pendingHubBattleDepartureExpected()); errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("transport err=%v code=%v", err, errcode.As(err))
	}
}

func TestConfirmBattleSourceDepartureSignsExactLocatorSource(t *testing.T) {
	signer, err := placement.NewProofSigner("battle-departure-test-key-is-at-least-32-bytes")
	if err != nil {
		t.Fatal(err)
	}
	pending := pendingHubFromBattlePlacement()
	stub := &departureLocatorClientStub{response: &locatorv1.GetPlacementResponse{
		Code: commonv1.ErrCode_OK, Found: true, Placement: pending,
	}}
	stub.confirm = func(req *locatorv1.ConfirmPlacementSourceDepartureRequest) (*locatorv1.ConfirmPlacementSourceDepartureResponse, error) {
		proof := placement.SourceDepartureProof{
			PlayerID: req.GetPlayerId(), PlacementVersion: req.GetPlacementVersion(),
			OperationID: req.GetOperationId(), TargetRoute: int32(req.GetTargetRoute()),
			TargetMatchID: req.GetTargetMatchId(), SourcePlacementVersion: req.GetSourcePlacementVersion(),
			SourceOperationID: req.GetSourceOperationId(), SourceRoute: int32(req.GetSourceRoute()),
			SourceMatchID: req.GetSourceMatchId(), ProofType: int32(req.GetProofType()), ProofID: req.GetProofId(),
			SourceTarget: placement.Target{PodName: req.GetSourceTarget().GetDsPodName(),
				InstanceUID:   req.GetSourceTarget().GetDsInstanceUid(),
				InstanceEpoch: req.GetSourceTarget().GetDsInstanceEpoch(),
				AssignmentID:  req.GetSourceTarget().GetHubAssignmentId(),
				AllocationID:  req.GetSourceTarget().GetAllocationId(),
				ReleaseTrack:  req.GetSourceTarget().GetReleaseTrack()},
		}
		if !signer.VerifySourceDeparture(proof, req.GetProofSignature()) {
			t.Fatal("confirmation signature did not bind exact request")
		}
		if proof.SourceTarget.ReleaseTrack != "stable" || proof.ProofType != placement.ProofBattleDeparture {
			t.Fatalf("proof=%+v", proof)
		}
		confirmed := proto.Clone(pending).(*locatorv1.PlayerPlacementStorageRecord)
		confirmed.SourceDepartureConfirmed = true
		confirmed.SourceDepartureProofType = locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_BATTLE_DEPARTURE
		confirmed.SourceDepartureProofId = req.GetProofId()
		return &locatorv1.ConfirmPlacementSourceDepartureResponse{
			Code: commonv1.ErrCode_OK, Confirmed: true, Placement: confirmed,
		}, nil
	}
	reader := &GrpcLocationRefresher{client: stub, battleDepartureSigner: signer}
	if err := reader.ConfirmBattleSourceDeparture(context.Background(),
		pendingHubBattleDepartureExpected(), "departure-stable-902"); err != nil {
		t.Fatalf("confirm: %v", err)
	}
}

func TestConfirmBattleSourceDepartureResponseLossAndIdentityMismatchFailClosed(t *testing.T) {
	signer, err := placement.NewProofSigner("battle-departure-test-key-is-at-least-32-bytes")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		fn   func(*locatorv1.ConfirmPlacementSourceDepartureRequest) (*locatorv1.ConfirmPlacementSourceDepartureResponse, error)
		code errcode.Code
	}{
		{name: "response lost", code: errcode.ErrUnavailable,
			fn: func(*locatorv1.ConfirmPlacementSourceDepartureRequest) (*locatorv1.ConfirmPlacementSourceDepartureResponse, error) {
				return nil, errors.New("ACK lost")
			}},
		{name: "mismatched response", code: errcode.ErrLocatorConflict,
			fn: func(req *locatorv1.ConfirmPlacementSourceDepartureRequest) (*locatorv1.ConfirmPlacementSourceDepartureResponse, error) {
				wrong := proto.Clone(pendingHubFromBattlePlacement()).(*locatorv1.PlayerPlacementStorageRecord)
				wrong.OperationId = "323e4567-e89b-42d3-a456-426614174002"
				wrong.SourceDepartureConfirmed = true
				wrong.SourceDepartureProofType = req.GetProofType()
				wrong.SourceDepartureProofId = req.GetProofId()
				return &locatorv1.ConfirmPlacementSourceDepartureResponse{Code: commonv1.ErrCode_OK,
					Confirmed: true, Placement: wrong}, nil
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &departureLocatorClientStub{response: &locatorv1.GetPlacementResponse{
				Code: commonv1.ErrCode_OK, Found: true, Placement: pendingHubFromBattlePlacement(),
			}, confirm: tc.fn}
			reader := &GrpcLocationRefresher{client: stub, battleDepartureSigner: signer}
			err := reader.ConfirmBattleSourceDeparture(context.Background(),
				pendingHubBattleDepartureExpected(), "departure-stable-902")
			if errcode.As(err) != tc.code {
				t.Fatalf("err=%v code=%v want=%v", err, errcode.As(err), tc.code)
			}
		})
	}
}
