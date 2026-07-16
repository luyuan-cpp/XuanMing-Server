package data

import (
	"context"
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
	request  *locatorv1.ConfirmPlacementSourceDepartureRequest
	response *locatorv1.ConfirmPlacementSourceDepartureResponse
}

func (s *departureLocatorClientStub) ConfirmPlacementSourceDeparture(_ context.Context,
	in *locatorv1.ConfirmPlacementSourceDepartureRequest, _ ...grpc.CallOption,
) (*locatorv1.ConfirmPlacementSourceDepartureResponse, error) {
	s.request = proto.Clone(in).(*locatorv1.ConfirmPlacementSourceDepartureRequest)
	return s.response, nil
}

func validHubDepartureProof() placement.SourceDepartureProof {
	return placement.SourceDepartureProof{
		PlayerID: 81, PlacementVersion: 12,
		OperationID: "123e4567-e89b-42d3-a456-426614174000",
		TargetRoute: placement.RouteBattle, TargetMatchID: 9081,
		SourcePlacementVersion: 10,
		SourceOperationID:      "223e4567-e89b-42d3-a456-426614174001",
		SourceRoute:            placement.RouteHub, SourceMatchID: 0,
		SourceTarget: placement.Target{PodName: "hub-source", InstanceUID: "hub-source-uid",
			InstanceEpoch: 7, AssignmentID: "assignment-source", ReleaseTrack: "stable"},
		ProofType: placement.ProofHubDeparture, ProofID: "hub-departure:81",
	}
}

func confirmedHubDepartureRecord(proof placement.SourceDepartureProof) *locatorv1.PlayerPlacementStorageRecord {
	return &locatorv1.PlayerPlacementStorageRecord{
		PlayerId: proof.PlayerID, Version: proof.PlacementVersion, OperationId: proof.OperationID,
		CurrentRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		TargetRoute:  locatorv1.PlacementRoute(proof.TargetRoute), TargetMatchId: proof.TargetMatchID,
		TransitionState:        locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING,
		SourcePlacementVersion: proof.SourcePlacementVersion,
		SourceOperationId:      proof.SourceOperationID,
		SourceDsPodName:        proof.SourceTarget.PodName, SourceDsInstanceUid: proof.SourceTarget.InstanceUID,
		SourceDsInstanceEpoch:    proof.SourceTarget.InstanceEpoch,
		SourceHubAssignmentId:    proof.SourceTarget.AssignmentID,
		SourceReleaseTrack:       proof.SourceTarget.ReleaseTrack,
		SourceDepartureConfirmed: true,
		SourceDepartureProofType: locatorv1.PlacementSourceDepartureProofType(proof.ProofType),
		SourceDepartureProofId:   proof.ProofID,
	}
}

func TestConfirmHubSourceDepartureSendsAndChecksExactIdentity(t *testing.T) {
	proof := validHubDepartureProof()
	stub := &departureLocatorClientStub{response: &locatorv1.ConfirmPlacementSourceDepartureResponse{
		Code: commonv1.ErrCode_OK, Confirmed: true, Placement: confirmedHubDepartureRecord(proof),
	}}
	client := &GrpcHubPlacementClient{client: stub}
	if err := client.ConfirmHubSourceDeparture(context.Background(), proof, "signed-proof"); err != nil {
		t.Fatal(err)
	}
	req := stub.request
	if req == nil || req.GetPlayerId() != proof.PlayerID ||
		req.GetPlacementVersion() != proof.PlacementVersion || req.GetOperationId() != proof.OperationID ||
		int32(req.GetTargetRoute()) != proof.TargetRoute || req.GetTargetMatchId() != proof.TargetMatchID ||
		req.GetSourcePlacementVersion() != proof.SourcePlacementVersion ||
		req.GetSourceOperationId() != proof.SourceOperationID ||
		int32(req.GetSourceRoute()) != proof.SourceRoute || req.GetSourceMatchId() != 0 ||
		req.GetSourceTarget().GetDsInstanceUid() != proof.SourceTarget.InstanceUID ||
		int32(req.GetProofType()) != proof.ProofType || req.GetProofId() != proof.ProofID ||
		req.GetProofSignature() != "signed-proof" {
		t.Fatalf("request lost departure identity: %+v", req)
	}

	stub.response.Placement.SourceDepartureProofId = "another-proof"
	if err := client.ConfirmHubSourceDeparture(context.Background(), proof, "signed-proof"); errcode.As(err) != errcode.ErrLocatorConflict {
		t.Fatalf("mismatched locator ACK accepted: code=%v err=%v", errcode.As(err), err)
	}
}

func TestValidateHubSourceDepartureProofRejectsIncompleteOrWrongDomain(t *testing.T) {
	valid := validHubDepartureProof()
	tests := []struct {
		name string
		edit func(*placement.SourceDepartureProof)
	}{
		{name: "source match", edit: func(p *placement.SourceDepartureProof) { p.SourceMatchID = 1 }},
		{name: "source route", edit: func(p *placement.SourceDepartureProof) { p.SourceRoute = placement.RouteBattle }},
		{name: "battle proof type", edit: func(p *placement.SourceDepartureProof) { p.ProofType = placement.ProofBattleDeparture }},
		{name: "partial source", edit: func(p *placement.SourceDepartureProof) { p.SourceTarget.AssignmentID = "" }},
		{name: "same operation", edit: func(p *placement.SourceDepartureProof) { p.SourceOperationID = p.OperationID }},
		{name: "Hub target match", edit: func(p *placement.SourceDepartureProof) {
			p.TargetRoute = placement.RouteHub
		}},
		{name: "Battle target without match", edit: func(p *placement.SourceDepartureProof) { p.TargetMatchID = 0 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			proof := valid

			tc.edit(&proof)
			if err := validateHubSourceDepartureProof(proof, "signed-proof"); errcode.As(err) != errcode.ErrInvalidArg {
				t.Fatalf("code=%v err=%v", errcode.As(err), err)
			}
		})
	}
}
