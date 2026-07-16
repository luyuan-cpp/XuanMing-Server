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
	GetHubPlacement(context.Context, uint64) (HubPlacementSnapshot, error)
	BeginHubTransfer(context.Context, uint64, uint64, string, string, string, int64) (placement.Binding, error)
	BindHubTarget(context.Context, uint64, placement.Binding, placement.Target, int64) error
	ConfirmHubSourceDeparture(context.Context, placement.SourceDepartureProof, string) error
	RetargetHubTarget(context.Context, uint64, placement.Binding, placement.Binding,
		placement.Target, placement.Target, placement.TargetUnavailableReason,
		string, string, int64) (placement.Binding, error)
	CommitHubAdmission(context.Context, uint64, placement.Binding, placement.Target, string) error
}

type HubPlacementSnapshot struct {
	Found   bool
	Binding placement.Binding
	// SourceBinding/SourceTarget are the immutable physical source captured by
	// player_locator at BeginPlacementTransition.  Target is the destination
	// being prepared and therefore cannot prove that the previous Hub owner has
	// departed.
	SourceBinding                       placement.Binding
	SourceTarget                        placement.Target
	CurrentRoute                        locatorv1.PlacementRoute
	CurrentMatchID                      uint64
	TargetRoute                         locatorv1.PlacementRoute
	TransitionState                     locatorv1.PlacementTransitionState
	ProofType                           locatorv1.PlacementProofType
	Target                              placement.Target
	TargetMatchID                       uint64
	LeaseDeadlineMs                     int64
	SourceDepartureConfirmed            bool
	SourceDepartureProofType            locatorv1.PlacementSourceDepartureProofType
	SourceDepartureProofID              string
	LastSourceDepartureProofType        locatorv1.PlacementSourceDepartureProofType
	LastSourceDepartureProofID          string
	LastSourceDeparturePlacementVersion uint64
	LastSourceDepartureOperationID      string
	RetargetCount                       uint32
	LastRetargetProofID                 string
	LastRetargetReason                  locatorv1.PlacementTargetUnavailableReason
}

func (g *GrpcHubPlacementClient) GetHubPlacement(ctx context.Context, playerID uint64) (HubPlacementSnapshot, error) {
	resp, err := g.client.GetPlacement(ctx, &locatorv1.GetPlacementRequest{PlayerId: playerID})
	if err != nil {
		return HubPlacementSnapshot{}, errcode.NewCause(errcode.ErrUnavailable, err, "Hub placement read unavailable")
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return HubPlacementSnapshot{}, errcode.New(errcode.Code(resp.GetCode()), "Hub placement read rejected")
	}
	if !resp.GetFound() || resp.GetPlacement() == nil {
		return HubPlacementSnapshot{Found: false}, nil
	}
	rec := resp.GetPlacement()
	binding := placement.Binding{Version: rec.GetVersion(), OperationID: rec.GetOperationId(),
		SourceMatchID: rec.GetSourceMatchId()}
	if rec.GetPlayerId() != playerID || !binding.Complete() {
		return HubPlacementSnapshot{}, errcode.New(errcode.ErrUnavailable,
			"Hub placement read returned malformed authority")
	}
	return HubPlacementSnapshot{Found: true,
		Binding: binding, CurrentRoute: rec.GetCurrentRoute(), CurrentMatchID: rec.GetMatchId(),
		SourceBinding: placement.Binding{Version: rec.GetSourcePlacementVersion(),
			OperationID: rec.GetSourceOperationId()},
		SourceTarget: placement.Target{PodName: rec.GetSourceDsPodName(),
			InstanceUID: rec.GetSourceDsInstanceUid(), InstanceEpoch: rec.GetSourceDsInstanceEpoch(),
			AssignmentID: rec.GetSourceHubAssignmentId(), AllocationID: rec.GetSourceAllocationId(),
			ReleaseTrack: rec.GetSourceReleaseTrack()},
		TargetRoute: rec.GetTargetRoute(), TransitionState: rec.GetTransitionState(),
		ProofType: rec.GetProofType(),
		Target: placement.Target{PodName: rec.GetDsPodName(), InstanceUID: rec.GetDsInstanceUid(),
			InstanceEpoch: rec.GetDsInstanceEpoch(), AssignmentID: rec.GetHubAssignmentId(),
			AllocationID: rec.GetAllocationId(), ReleaseTrack: rec.GetReleaseTrack()},
		TargetMatchID:                       rec.GetTargetMatchId(),
		LeaseDeadlineMs:                     rec.GetLeaseDeadlineMs(),
		SourceDepartureConfirmed:            rec.GetSourceDepartureConfirmed(),
		SourceDepartureProofType:            rec.GetSourceDepartureProofType(),
		SourceDepartureProofID:              rec.GetSourceDepartureProofId(),
		LastSourceDepartureProofType:        rec.GetLastSourceDepartureProofType(),
		LastSourceDepartureProofID:          rec.GetLastSourceDepartureProofId(),
		LastSourceDeparturePlacementVersion: rec.GetLastSourceDeparturePlacementVersion(),
		LastSourceDepartureOperationID:      rec.GetLastSourceDepartureOperationId(),
		RetargetCount:                       rec.GetRetargetCount(),
		LastRetargetProofID:                 rec.GetLastRetargetProofId(),
		LastRetargetReason:                  rec.GetLastRetargetReason()}, nil
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
		code := errcode.Code(resp.GetCode())
		if code == 0 {
			code = errcode.ErrUnavailable
		}
		return placement.Binding{}, errcode.New(code, "Hub transfer placement begin rejected")
	}
	rec := resp.GetPlacement()
	b := placement.Binding{Version: rec.GetVersion(), OperationID: rec.GetOperationId(), SourceMatchID: rec.GetSourceMatchId()}
	if rec.GetPlayerId() != playerID || b.Version != expectedVersion+1 || b.OperationID != operationID ||
		b.SourceMatchID != 0 || rec.GetTargetRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB ||
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

func (g *GrpcHubPlacementClient) BindHubTarget(ctx context.Context, playerID uint64, binding placement.Binding, target placement.Target, leaseDeadlineMs int64) error {
	if playerID == 0 || !binding.Complete() || !target.CompleteHub() || leaseDeadlineMs <= 0 {
		return errcode.New(errcode.ErrInvalidArg, "complete Hub placement bind required")
	}
	resp, err := g.client.BindPlacementTarget(ctx, &locatorv1.BindPlacementTargetRequest{
		PlayerId: playerID, PlacementVersion: binding.Version, OperationId: binding.OperationID,
		TargetRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		DsPodName:   target.PodName, DsInstanceUid: target.InstanceUID,
		DsInstanceEpoch: target.InstanceEpoch, HubAssignmentId: target.AssignmentID,
		ReleaseTrack: target.ReleaseTrack, LeaseDeadlineMs: leaseDeadlineMs,
	})
	if err != nil {
		return errcode.NewCause(errcode.ErrUnavailable, err, "placement bind unavailable")
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return errcode.New(errcode.Code(resp.GetCode()), "placement bind rejected")
	}
	rec := resp.GetPlacement()
	if !hubPlacementTargetMatches(rec, playerID, binding, target) ||
		(rec.GetTransitionState() != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING ||
			rec.GetTargetRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB) &&
			(rec.GetTransitionState() != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE ||
				rec.GetCurrentRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB) {
		return errcode.New(errcode.ErrLocatorConflict, "placement bind response identity mismatch")
	}
	return nil
}

// ConfirmHubSourceDeparture submits the Hub allocator's physical cleanup ACK.
// It is intentionally separate from Bind/Commit: merely preparing a target is
// never proof that the old Hub no longer owns a PlayerController or seat.
func (g *GrpcHubPlacementClient) ConfirmHubSourceDeparture(ctx context.Context,
	proof placement.SourceDepartureProof, signature string,
) error {
	if err := validateHubSourceDepartureProof(proof, signature); err != nil {
		return err
	}
	resp, err := g.client.ConfirmPlacementSourceDeparture(ctx,
		&locatorv1.ConfirmPlacementSourceDepartureRequest{
			PlayerId: proof.PlayerID, PlacementVersion: proof.PlacementVersion,
			OperationId: proof.OperationID, TargetRoute: locatorv1.PlacementRoute(proof.TargetRoute),
			TargetMatchId: proof.TargetMatchID, SourcePlacementVersion: proof.SourcePlacementVersion,
			SourceOperationId: proof.SourceOperationID,
			SourceRoute:       locatorv1.PlacementRoute(proof.SourceRoute), SourceMatchId: proof.SourceMatchID,
			SourceTarget: placementTargetProto(proof.SourceTarget),
			ProofType:    locatorv1.PlacementSourceDepartureProofType(proof.ProofType),
			ProofId:      proof.ProofID, ProofSignature: signature,
		})
	if err != nil {
		return errcode.NewCause(errcode.ErrUnavailable, err,
			"Hub source departure confirmation unavailable")
	}
	if resp.GetCode() != commonv1.ErrCode_OK || !resp.GetConfirmed() || resp.GetPlacement() == nil {
		code := errcode.Code(resp.GetCode())
		if code == 0 {
			code = errcode.ErrLocatorConflict
		}
		return errcode.New(code, "Hub source departure confirmation rejected")
	}
	if !hubSourceDepartureResponseMatches(resp.GetPlacement(), proof) {
		return errcode.New(errcode.ErrLocatorConflict,
			"Hub source departure response identity mismatch")
	}
	return nil
}

func validateHubSourceDepartureProof(proof placement.SourceDepartureProof, signature string) error {
	validTarget := (proof.TargetRoute == placement.RouteHub && proof.TargetMatchID == 0) ||
		(proof.TargetRoute == placement.RouteBattle && proof.TargetMatchID > 0)
	if proof.PlayerID == 0 || proof.PlacementVersion == 0 ||
		proof.SourcePlacementVersion == 0 || proof.SourcePlacementVersion >= proof.PlacementVersion ||
		!placement.ValidOperationID(proof.OperationID) ||
		!placement.ValidOperationID(proof.SourceOperationID) ||
		proof.OperationID == proof.SourceOperationID || !validTarget ||
		proof.SourceRoute != placement.RouteHub || proof.SourceMatchID != 0 ||
		!proof.SourceTarget.CompleteHub() || proof.SourceTarget.AllocationID != "" ||
		proof.ProofType != placement.ProofHubDeparture || proof.ProofID == "" || signature == "" {
		return errcode.New(errcode.ErrInvalidArg, "complete Hub source departure proof required")
	}
	return nil
}

func hubSourceDepartureResponseMatches(rec *locatorv1.PlayerPlacementStorageRecord,
	proof placement.SourceDepartureProof,
) bool {
	return rec != nil && rec.GetPlayerId() == proof.PlayerID &&
		rec.GetVersion() == proof.PlacementVersion && rec.GetOperationId() == proof.OperationID &&
		rec.GetTargetRoute() == locatorv1.PlacementRoute(proof.TargetRoute) &&
		rec.GetTargetMatchId() == proof.TargetMatchID &&
		rec.GetTransitionState() == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
		rec.GetCurrentRoute() == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB && rec.GetMatchId() == 0 &&
		rec.GetSourcePlacementVersion() == proof.SourcePlacementVersion &&
		rec.GetSourceOperationId() == proof.SourceOperationID &&
		rec.GetSourceDsPodName() == proof.SourceTarget.PodName &&
		rec.GetSourceDsInstanceUid() == proof.SourceTarget.InstanceUID &&
		rec.GetSourceDsInstanceEpoch() == proof.SourceTarget.InstanceEpoch &&
		rec.GetSourceHubAssignmentId() == proof.SourceTarget.AssignmentID &&
		rec.GetSourceAllocationId() == "" &&
		rec.GetSourceReleaseTrack() == proof.SourceTarget.ReleaseTrack &&
		rec.GetSourceDepartureConfirmed() &&
		int32(rec.GetSourceDepartureProofType()) == proof.ProofType &&
		rec.GetSourceDepartureProofId() == proof.ProofID
}

func (g *GrpcHubPlacementClient) RetargetHubTarget(ctx context.Context, playerID uint64,
	expectedBinding, replacementBinding placement.Binding, expectedTarget, replacementTarget placement.Target,
	reason placement.TargetUnavailableReason, proofID, signature string, leaseDeadlineMs int64,
) (placement.Binding, error) {
	if playerID == 0 || !expectedBinding.Complete() || !replacementBinding.Complete() ||
		replacementBinding.Version != expectedBinding.Version+1 ||
		replacementBinding.SourceMatchID != expectedBinding.SourceMatchID ||
		!expectedTarget.CompleteHub() || !replacementTarget.CompleteHub() ||
		proofID == "" || signature == "" {
		return placement.Binding{}, errcode.New(errcode.ErrInvalidArg, "complete Hub placement retarget required")
	}
	resp, err := g.client.RetargetPlacementTarget(ctx, &locatorv1.RetargetPlacementTargetRequest{
		PlayerId: playerID, PlacementVersion: expectedBinding.Version,
		OperationId:            expectedBinding.OperationID,
		TargetRoute:            locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		ExpectedTarget:         placementTargetProto(expectedTarget),
		ReplacementVersion:     replacementBinding.Version,
		ReplacementOperationId: replacementBinding.OperationID,
		ReplacementTarget:      placementTargetProto(replacementTarget),
		ProofType:              locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_HUB_TRANSFER,
		Reason:                 locatorv1.PlacementTargetUnavailableReason(reason), ProofId: proofID,
		ProofSignature: signature, LeaseDeadlineMs: leaseDeadlineMs,
	})
	if err != nil {
		return placement.Binding{}, errcode.NewCause(errcode.ErrUnavailable, err, "Hub placement retarget unavailable")
	}
	if resp.GetCode() != commonv1.ErrCode_OK || resp.GetPlacement() == nil {
		code := errcode.Code(resp.GetCode())
		if code == 0 {
			code = errcode.ErrUnavailable
		}
		return placement.Binding{}, errcode.New(code, "Hub placement retarget rejected")
	}
	rec := resp.GetPlacement()
	got := placement.Binding{Version: rec.GetVersion(), OperationID: rec.GetOperationId(),
		SourceMatchID: rec.GetSourceMatchId()}
	if !got.Equal(replacementBinding) ||
		!hubPlacementTargetMatches(rec, playerID, replacementBinding, replacementTarget) ||
		rec.GetTransitionState() != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING ||
		rec.GetTargetRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB {
		return placement.Binding{}, errcode.New(errcode.ErrLocatorConflict,
			"Hub placement retarget response identity mismatch")
	}
	return got, nil
}

func placementTargetProto(target placement.Target) *locatorv1.PlacementTargetIdentity {
	return &locatorv1.PlacementTargetIdentity{DsPodName: target.PodName,
		DsInstanceUid: target.InstanceUID, DsInstanceEpoch: target.InstanceEpoch,
		HubAssignmentId: target.AssignmentID, AllocationId: target.AllocationID,
		ReleaseTrack: target.ReleaseTrack}
}

func (g *GrpcHubPlacementClient) CommitHubAdmission(ctx context.Context, playerID uint64, binding placement.Binding, target placement.Target, admissionID string) error {
	if playerID == 0 || !binding.Complete() || !target.CompleteHub() || admissionID == "" {
		return errcode.New(errcode.ErrInvalidArg, "complete Hub placement admission required")
	}
	resp, err := g.client.CommitPlacementAdmission(ctx, &locatorv1.CommitPlacementAdmissionRequest{
		PlayerId: playerID, PlacementVersion: binding.Version, OperationId: binding.OperationID,
		TargetRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		DsPodName:   target.PodName, DsInstanceUid: target.InstanceUID,
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
	if !hubPlacementTargetMatches(rec, playerID, binding, target) ||
		rec.GetTransitionState() != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE ||
		rec.GetCurrentRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB ||
		rec.GetTargetRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_UNSPECIFIED || rec.GetMatchId() != 0 {
		return errcode.New(errcode.ErrLocatorConflict, "placement commit response identity mismatch")
	}
	return nil
}

func hubPlacementTargetMatches(rec *locatorv1.PlayerPlacementStorageRecord, playerID uint64,
	binding placement.Binding, target placement.Target) bool {
	return rec != nil && rec.GetPlayerId() == playerID && rec.GetVersion() == binding.Version &&
		rec.GetOperationId() == binding.OperationID && rec.GetSourceMatchId() == binding.SourceMatchID &&
		rec.GetDsPodName() == target.PodName && rec.GetDsInstanceUid() == target.InstanceUID &&
		rec.GetDsInstanceEpoch() == target.InstanceEpoch && rec.GetHubAssignmentId() == target.AssignmentID &&
		rec.GetAllocationId() == "" && rec.GetTargetMatchId() == 0 && rec.GetReleaseTrack() == target.ReleaseTrack
}
