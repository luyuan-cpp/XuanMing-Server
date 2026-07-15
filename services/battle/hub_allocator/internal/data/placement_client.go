package data

import (
	"context"

	"google.golang.org/grpc"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
)

type HubPlacementClient interface {
	BeginHubTransfer(context.Context, uint64, uint64, string, string, string, int64) (placement.Binding, error)
	BindHubTarget(context.Context, uint64, placement.Binding, placement.Target) error
	CommitHubAdmission(context.Context, uint64, placement.Binding, placement.Target, string) error
}

func (g *GrpcHubPlacementClient) BeginHubTransfer(ctx context.Context, playerID, expectedVersion uint64,
	operationID, proofID, signature string, leaseDeadlineMs int64) (placement.Binding, error) {
	resp, err := g.client.BeginPlacementTransition(ctx, &locatorv1.BeginPlacementTransitionRequest{
		PlayerId: playerID, ExpectedVersion: expectedVersion,
		TargetRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		OperationId: operationID, ProofType: locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_HUB_TRANSFER,
		ProofId: proofID, ProofSignature: signature, LeaseDeadlineMs: leaseDeadlineMs,
	})
	if err != nil {
		return placement.Binding{}, errcode.NewCause(errcode.ErrUnavailable, err, "Hub transfer placement begin unavailable")
	}
	if resp.GetCode() != commonv1.ErrCode_OK || resp.GetPlacement() == nil {
		return placement.Binding{}, errcode.New(errcode.Code(resp.GetCode()), "Hub transfer placement begin rejected")
	}
	rec := resp.GetPlacement()
	b := placement.Binding{Version: rec.GetVersion(), OperationID: rec.GetOperationId(), SourceMatchID: rec.GetSourceMatchId()}
	if !b.Complete() || rec.GetTargetRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB ||
		rec.GetTransitionState() != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING {
		return placement.Binding{}, errcode.New(errcode.ErrLocatorConflict, "Hub transfer placement response invalid")
	}
	return b, nil
}

type GrpcHubPlacementClient struct {
	client locatorv1.PlayerLocatorServiceClient
}

func NewGrpcHubPlacementClient(conn *grpc.ClientConn) *GrpcHubPlacementClient {
	return &GrpcHubPlacementClient{client: locatorv1.NewPlayerLocatorServiceClient(conn)}
}

func (g *GrpcHubPlacementClient) BindHubTarget(ctx context.Context, playerID uint64, binding placement.Binding, target placement.Target) error {
	if playerID == 0 || !binding.Complete() || !target.CompleteHub() {
		return errcode.New(errcode.ErrInvalidArg, "complete Hub placement bind required")
	}
	resp, err := g.client.BindPlacementTarget(ctx, &locatorv1.BindPlacementTargetRequest{
		PlayerId: playerID, PlacementVersion: binding.Version, OperationId: binding.OperationID,
		TargetRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		DsPodName: target.PodName, DsInstanceUid: target.InstanceUID,
		DsInstanceEpoch: target.InstanceEpoch, HubAssignmentId: target.AssignmentID,
		ReleaseTrack: target.ReleaseTrack,
	})
	if err != nil {
		return errcode.NewCause(errcode.ErrUnavailable, err, "placement bind unavailable")
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return errcode.New(errcode.Code(resp.GetCode()), "placement bind rejected")
	}
	rec := resp.GetPlacement()
	if rec == nil || rec.GetVersion() != binding.Version || rec.GetOperationId() != binding.OperationID ||
		rec.GetSourceMatchId() != binding.SourceMatchID || rec.GetHubAssignmentId() != target.AssignmentID {
		return errcode.New(errcode.ErrLocatorConflict, "placement bind response identity mismatch")
	}
	return nil
}

func (g *GrpcHubPlacementClient) CommitHubAdmission(ctx context.Context, playerID uint64, binding placement.Binding, target placement.Target, admissionID string) error {
	if playerID == 0 || !binding.Complete() || !target.CompleteHub() || admissionID == "" {
		return errcode.New(errcode.ErrInvalidArg, "complete Hub placement admission required")
	}
	resp, err := g.client.CommitPlacementAdmission(ctx, &locatorv1.CommitPlacementAdmissionRequest{
		PlayerId: playerID, PlacementVersion: binding.Version, OperationId: binding.OperationID,
		TargetRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		DsPodName: target.PodName, DsInstanceUid: target.InstanceUID,
		DsInstanceEpoch: target.InstanceEpoch, HubAssignmentId: target.AssignmentID,
		ReleaseTrack: target.ReleaseTrack, AdmissionId: admissionID,
	})
	if err != nil {
		return errcode.NewCause(errcode.ErrUnavailable, err, "placement commit result unknown")
	}
	if resp.GetCode() != commonv1.ErrCode_OK || !resp.GetCommitted() {
		if resp.GetCode() == commonv1.ErrCode_OK {
			return errcode.New(errcode.ErrLocatorConflict, "placement commit not confirmed")
		}
		return errcode.New(errcode.Code(resp.GetCode()), "placement commit rejected")
	}
	rec := resp.GetPlacement()
	if rec == nil || rec.GetVersion() != binding.Version || rec.GetOperationId() != binding.OperationID ||
		rec.GetCurrentRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB ||
		rec.GetHubAssignmentId() != target.AssignmentID {
		return errcode.New(errcode.ErrLocatorConflict, "placement commit response identity mismatch")
	}
	return nil
}
