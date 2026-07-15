package data

import (
	"context"

	"google.golang.org/grpc"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
)

type HubPlacementAdmission struct {
	PlayerID uint64
	Binding placement.Binding
	Target placement.Target
}

// BattlePlacementAdmission is the exact identity that the Battle DS must
// commit before opening its spawn gate.  It deliberately contains no
// client-supplied routing data: every field comes from the verified ticket
// and the independently verified active DS credential/roster projection.
type BattlePlacementAdmission struct {
	PlayerID   uint64
	MatchID    uint64
	AdmissionID string
	Binding    placement.Binding
	Target     placement.Target
}

type PlacementSnapshot struct {
	Found bool
	CurrentRoute locatorv1.PlacementRoute
	TargetRoute locatorv1.PlacementRoute
	TransitionState locatorv1.PlacementTransitionState
	Version uint64
	OperationID string
	MatchID uint64
	TargetMatchID uint64
	SourceMatchID uint64
	TargetBound bool
	LeaseDeadlineMs int64
	ProofType locatorv1.PlacementProofType
	ProofID string
	Target placement.Target
}

func (s PlacementSnapshot) HubBinding() (placement.Binding, bool) {
	b := placement.Binding{Version: s.Version, OperationID: s.OperationID, SourceMatchID: s.SourceMatchID}
	if !s.Found || !b.Complete() {
		return placement.Binding{}, false
	}
	pending := s.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
		s.TargetRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB
	stable := s.TransitionState == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE &&
		s.CurrentRoute == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB
	return b, pending || stable
}

type PlacementAdmissionChecker interface {
	CheckHubAdmission(context.Context, HubPlacementAdmission) error
	GetPlacement(context.Context, uint64) (PlacementSnapshot, error)
	BootstrapHub(context.Context, uint64, string, string, string, int64) (placement.Binding, error)
	BeginHubFromBattle(context.Context, uint64, uint64, placement.BattleExitProof, int64) (placement.Binding, error)
	CommitBattleAdmission(context.Context, BattlePlacementAdmission) error
}

// BeginHubFromBattle consumes a proof minted and durably stored by the Battle
// terminal/leave authority. The operation id is part of that proof, so a lost
// response always retries the same transition instead of creating a second
// writer operation.
func (g *GrpcPlacementChecker) BeginHubFromBattle(ctx context.Context, playerID, matchID uint64,
	proof placement.BattleExitProof, leaseDeadlineMs int64) (placement.Binding, error) {
	resp, err := g.client.BeginPlacementTransition(ctx, &locatorv1.BeginPlacementTransitionRequest{
		PlayerId: playerID, ExpectedVersion: proof.ExpectedVersion,
		TargetRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB,
		OperationId: proof.OperationID, SourceMatchId: matchID,
		ProofType: locatorv1.PlacementProofType(proof.ProofType), ProofId: proof.ProofID,
		LeaseDeadlineMs: leaseDeadlineMs, ProofSignature: proof.Signature,
	})
	if err != nil {
		return placement.Binding{}, errcode.NewCause(errcode.ErrUnavailable, err, "placement begin Hub transition unavailable")
	}
	if resp.GetCode() != commonv1.ErrCode_OK || resp.GetPlacement() == nil {
		code := errcode.Code(resp.GetCode())
		if code == 0 {
			code = errcode.ErrUnavailable
		}
		return placement.Binding{}, errcode.New(code, "placement begin Hub transition rejected")
	}
	rec := resp.GetPlacement()
	b := placement.Binding{Version: rec.GetVersion(), OperationID: rec.GetOperationId(), SourceMatchID: rec.GetSourceMatchId()}
	if !b.Complete() || b.SourceMatchID != matchID ||
		rec.GetTransitionState() != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING ||
		rec.GetTargetRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB {
		return placement.Binding{}, errcode.New(errcode.ErrLocatorConflict, "placement begin Hub response identity mismatch")
	}
	return b, nil
}

// CommitBattleAdmission is the final linearization point for HUB -> BATTLE.
// It is invoked before the JTI marker and before the Battle DS opens spawn;
// locator accepts an exact replay of the already committed target so an RPC
// response loss is safely retryable with the same admission id.
func (g *GrpcPlacementChecker) CommitBattleAdmission(ctx context.Context, in BattlePlacementAdmission) error {
	if in.PlayerID == 0 || in.MatchID == 0 || in.AdmissionID == "" || !in.Binding.Complete() ||
		!in.Target.CompleteBattle() || in.Binding.SourceMatchID != 0 {
		return errcode.New(errcode.ErrLoginTicketInvalid, "Battle placement admission identity incomplete")
	}
	resp, err := g.client.CommitPlacementAdmission(ctx, &locatorv1.CommitPlacementAdmissionRequest{
		PlayerId: in.PlayerID, PlacementVersion: in.Binding.Version,
		OperationId: in.Binding.OperationID,
		TargetRoute: locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		DsPodName: in.Target.PodName, DsInstanceUid: in.Target.InstanceUID,
		AdmissionId: in.AdmissionID, TargetMatchId: in.MatchID,
		DsInstanceEpoch: in.Target.InstanceEpoch, AllocationId: in.Target.AllocationID,
		ReleaseTrack: in.Target.ReleaseTrack,
	})
	if err != nil {
		return errcode.NewCause(errcode.ErrUnavailable, err, "placement Battle admission commit unavailable")
	}
	if resp.GetCode() != commonv1.ErrCode_OK || !resp.GetCommitted() || resp.GetPlacement() == nil {
		code := errcode.Code(resp.GetCode())
		if code == 0 {
			code = errcode.ErrUnavailable
		}
		return errcode.New(code, "placement Battle admission commit rejected")
	}
	rec := resp.GetPlacement()
	if rec.GetVersion() != in.Binding.Version || rec.GetOperationId() != in.Binding.OperationID ||
		rec.GetTransitionState() != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE ||
		rec.GetCurrentRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE || rec.GetMatchId() != in.MatchID ||
		rec.GetDsPodName() != in.Target.PodName || rec.GetDsInstanceUid() != in.Target.InstanceUID ||
		rec.GetDsInstanceEpoch() != in.Target.InstanceEpoch || rec.GetAllocationId() != in.Target.AllocationID ||
		rec.GetReleaseTrack() != in.Target.ReleaseTrack {
		return errcode.New(errcode.ErrLocatorConflict, "placement Battle admission commit response mismatch")
	}
	return nil
}

func (g *GrpcPlacementChecker) BootstrapHub(ctx context.Context, playerID uint64, operationID, proofID, signature string,
	leaseDeadlineMs int64) (placement.Binding, error) {
	resp, err := g.client.BootstrapPlacement(ctx, &locatorv1.BootstrapPlacementRequest{
		PlayerId: playerID, OperationId: operationID, ProofId: proofID,
		ProofSignature: signature, LeaseDeadlineMs: leaseDeadlineMs,
	})
	if err != nil {
		return placement.Binding{}, errcode.NewCause(errcode.ErrUnavailable, err, "placement bootstrap unavailable")
	}
	if resp.GetCode() != commonv1.ErrCode_OK || resp.GetPlacement() == nil {
		code := errcode.Code(resp.GetCode())
		if code == 0 {
			code = errcode.ErrUnavailable
		}
		return placement.Binding{}, errcode.New(code, "placement bootstrap rejected")
	}
	rec := resp.GetPlacement()
	b := placement.Binding{Version: rec.GetVersion(), OperationID: rec.GetOperationId(), SourceMatchID: rec.GetSourceMatchId()}
	if !b.Complete() || rec.GetTargetRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB ||
		rec.GetTransitionState() != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING {
		return placement.Binding{}, errcode.New(errcode.ErrLocatorConflict, "placement bootstrap response invalid")
	}
	return b, nil
}

type GrpcPlacementChecker struct {
	client locatorv1.PlayerLocatorServiceClient
}

func NewGrpcPlacementChecker(conn *grpc.ClientConn) *GrpcPlacementChecker {
	return &GrpcPlacementChecker{client: locatorv1.NewPlayerLocatorServiceClient(conn)}
}

func (g *GrpcPlacementChecker) GetPlacement(ctx context.Context, playerID uint64) (PlacementSnapshot, error) {
	resp, err := g.client.GetPlacement(ctx, &locatorv1.GetPlacementRequest{PlayerId: playerID})
	if err != nil {
		return PlacementSnapshot{}, errcode.NewCause(errcode.ErrUnavailable, err, "placement authority unavailable")
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return PlacementSnapshot{}, errcode.New(errcode.Code(resp.GetCode()), "placement authority rejected read")
	}
	if !resp.GetFound() || resp.GetPlacement() == nil {
		return PlacementSnapshot{Found: false}, nil
	}
	rec := resp.GetPlacement()
	return PlacementSnapshot{Found: true, CurrentRoute: rec.GetCurrentRoute(), TargetRoute: rec.GetTargetRoute(),
		TransitionState: rec.GetTransitionState(), Version: rec.GetVersion(), OperationID: rec.GetOperationId(),
		MatchID: rec.GetMatchId(), TargetMatchID: rec.GetTargetMatchId(), SourceMatchID: rec.GetSourceMatchId(),
		TargetBound: rec.GetDsPodName() != "" && rec.GetDsInstanceUid() != "",
		LeaseDeadlineMs: rec.GetLeaseDeadlineMs(), ProofType: rec.GetProofType(), ProofID: rec.GetProofId(),
		Target: placement.Target{PodName: rec.GetDsPodName(), InstanceUID: rec.GetDsInstanceUid(),
			InstanceEpoch: rec.GetDsInstanceEpoch(), AssignmentID: rec.GetHubAssignmentId(),
			AllocationID: rec.GetAllocationId(), ReleaseTrack: rec.GetReleaseTrack()}}, nil
}

func (g *GrpcPlacementChecker) CheckHubAdmission(ctx context.Context, in HubPlacementAdmission) error {
	if in.PlayerID == 0 || !in.Binding.Complete() || !in.Target.CompleteHub() {
		return errcode.New(errcode.ErrLoginTicketInvalid, "Hub ticket placement binding incomplete")
	}
	resp, err := g.client.GetPlacement(ctx, &locatorv1.GetPlacementRequest{PlayerId: in.PlayerID})
	if err != nil {
		return errcode.NewCause(errcode.ErrUnavailable, err, "placement authority unavailable")
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		return errcode.New(errcode.Code(resp.GetCode()), "placement authority rejected admission read")
	}
	rec := resp.GetPlacement()
	if !resp.GetFound() || rec == nil {
		return errcode.New(errcode.ErrLocatorNotFound, "placement is UNKNOWN")
	}
	if rec.GetVersion() != in.Binding.Version || rec.GetOperationId() != in.Binding.OperationID ||
		rec.GetSourceMatchId() != in.Binding.SourceMatchID || rec.GetDsPodName() != in.Target.PodName ||
		rec.GetDsInstanceUid() != in.Target.InstanceUID || rec.GetDsInstanceEpoch() != in.Target.InstanceEpoch ||
		rec.GetHubAssignmentId() != in.Target.AssignmentID || rec.GetAllocationId() != "" ||
		rec.GetReleaseTrack() != in.Target.ReleaseTrack {
		return errcode.New(errcode.ErrLoginTicketInvalid, "Hub ticket no longer matches placement authority")
	}
	pending := rec.GetTransitionState() == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
		rec.GetTargetRoute() == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB
	stable := rec.GetTransitionState() == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE &&
		rec.GetCurrentRoute() == locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB
	if !pending && !stable {
		return errcode.New(errcode.ErrLocatorConflict, "placement does not authorize Hub admission")
	}
	return nil
}
