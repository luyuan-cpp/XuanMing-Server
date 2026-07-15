package data

import (
	"context"
	"fmt"
	"time"

	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
	"google.golang.org/grpc"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"
	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/model"
)

// GrpcPlacementCoordinator 在 match READY 前，把全员从 stable HUB 线性化到同一个
// BATTLE_PENDING 并绑定 allocator 返回的完整 DS identity。最终 STABLE BATTLE 只能由
// Battle DS Admission 在 spawn gate 前调用 CommitPlacementAdmission 完成。
type GrpcPlacementCoordinator struct {
	client   placementClient
	signer   *placement.ProofSigner
	leaseTTL time.Duration
}

type placementClient interface {
	GetPlacement(context.Context, *locatorv1.GetPlacementRequest, ...grpc.CallOption) (*locatorv1.GetPlacementResponse, error)
	BeginPlacementTransition(context.Context, *locatorv1.BeginPlacementTransitionRequest, ...grpc.CallOption) (*locatorv1.BeginPlacementTransitionResponse, error)
	BindPlacementTarget(context.Context, *locatorv1.BindPlacementTargetRequest, ...grpc.CallOption) (*locatorv1.BindPlacementTargetResponse, error)
}

func NewGrpcPlacementCoordinator(client placementClient, signer *placement.ProofSigner, leaseTTL time.Duration) *GrpcPlacementCoordinator {
	if leaseTTL < 5*time.Minute {
		leaseTTL = 30 * time.Minute
	}
	return &GrpcPlacementCoordinator{client: client, signer: signer, leaseTTL: leaseTTL}
}

func (c *GrpcPlacementCoordinator) PrepareBattlePlacement(
	ctx context.Context,
	operationID string,
	matchID uint64,
	playerIDs []uint64,
	allocation *model.BattleAllocation,
) (map[uint64]placement.Binding, error) {
	if c == nil || c.client == nil || c.signer == nil {
		return nil, errcode.New(errcode.ErrUnavailable, "battle placement coordinator unavailable")
	}
	if !placement.ValidOperationID(operationID) || matchID == 0 || len(playerIDs) == 0 ||
		allocation == nil || allocation.Address == "" || !allocation.Target.CompleteBattle() {
		return nil, errcode.New(errcode.ErrInvalidArg, "complete battle placement operation required")
	}
	bindings := make(map[uint64]placement.Binding, len(playerIDs))
	for _, playerID := range playerIDs {
		if playerID == 0 {
			return nil, errcode.New(errcode.ErrInvalidArg, "battle placement contains zero player_id")
		}
		binding, err := c.preparePlayer(ctx, playerID, operationID, matchID, allocation.Target)
		if err != nil {
			return nil, fmt.Errorf("prepare battle placement player=%d match=%d: %w", playerID, matchID, err)
		}
		bindings[playerID] = binding
	}
	return bindings, nil
}

func (c *GrpcPlacementCoordinator) preparePlayer(
	ctx context.Context,
	playerID uint64,
	operationID string,
	matchID uint64,
	target placement.Target,
) (placement.Binding, error) {
	getResp, err := c.client.GetPlacement(ctx, &locatorv1.GetPlacementRequest{PlayerId: playerID})
	if err != nil {
		return placement.Binding{}, err
	}
	if getResp.GetCode() != commonv1.ErrCode_OK {
		return placement.Binding{}, errcode.New(errcode.Code(getResp.GetCode()), "GetPlacement code=%d", getResp.GetCode())
	}
	if !getResp.GetFound() || getResp.GetPlacement() == nil {
		return placement.Binding{}, errcode.New(errcode.ErrLocatorNotFound, "placement is UNKNOWN")
	}
	current := getResp.GetPlacement()
	expectedVersion := current.GetVersion()
	if sameBattlePendingOperation(current, operationID, matchID) {
		if current.GetVersion() <= 1 {
			return placement.Binding{}, errcode.New(errcode.ErrLocatorConflict, "pending placement has invalid version")
		}
		expectedVersion = current.GetVersion() - 1
	} else {
		if current.GetTransitionState() != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE ||
			current.GetCurrentRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB || current.GetMatchId() != 0 {
			return placement.Binding{}, errcode.New(errcode.ErrLocatorConflict,
				"expected stable HUB, got route=%s transition=%s version=%d operation=%q",
				current.GetCurrentRoute(), current.GetTransitionState(), current.GetVersion(), current.GetOperationId())
		}
	}

	// Begin 对同 operation 是幂等续租：即使此前已 Begin/Bind，outage 后也先用同一
	// signed proof 单调延长 lease，再重放 Bind，避免过期 pending 永久卡死。
	proof := placement.Proof{
		PlayerID:        playerID,
		ExpectedVersion: expectedVersion,
		SourceRoute:     placement.RouteHub,
		TargetRoute:     placement.RouteBattle,
		TargetMatchID:   matchID,
		ProofType:       placement.ProofMatchStart,
		ProofID:         operationID,
		OperationID:     operationID,
	}
	beginResp, berr := c.client.BeginPlacementTransition(ctx, &locatorv1.BeginPlacementTransitionRequest{
		PlayerId:        playerID,
		ExpectedVersion: expectedVersion,
		TargetRoute:     locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		OperationId:     operationID,
		ProofType:       locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_START,
		ProofId:         operationID,
		LeaseDeadlineMs: time.Now().Add(c.leaseTTL).UnixMilli(),
		TargetMatchId:   matchID,
		ProofSignature:  c.signer.Sign(proof),
	})
	if berr != nil {
		return placement.Binding{}, berr
	}
	if beginResp.GetCode() != commonv1.ErrCode_OK || beginResp.GetPlacement() == nil {
		return placement.Binding{}, errcode.New(errcode.Code(beginResp.GetCode()), "BeginPlacementTransition code=%d", beginResp.GetCode())
	}
	current = beginResp.GetPlacement()
	if !sameBattlePendingOperation(current, operationID, matchID) {
		return placement.Binding{}, errcode.New(errcode.ErrLocatorConflict, "BeginPlacementTransition returned another operation")
	}
	version := current.GetVersion()

	bindResp, err := c.client.BindPlacementTarget(ctx, &locatorv1.BindPlacementTargetRequest{
		PlayerId:         playerID,
		PlacementVersion: version,
		OperationId:      operationID,
		TargetRoute:      locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE,
		DsPodName:        target.PodName,
		DsInstanceUid:    target.InstanceUID,
		TargetMatchId:    matchID,
		DsInstanceEpoch:  target.InstanceEpoch,
		AllocationId:     target.AllocationID,
		ReleaseTrack:     target.ReleaseTrack,
	})
	if err != nil {
		return placement.Binding{}, err
	}
	if bindResp.GetCode() != commonv1.ErrCode_OK || bindResp.GetPlacement() == nil {
		return placement.Binding{}, errcode.New(errcode.Code(bindResp.GetCode()), "BindPlacementTarget code=%d", bindResp.GetCode())
	}
	bound := bindResp.GetPlacement()
	if !sameBattlePendingOperation(bound, operationID, matchID) || bound.GetDsPodName() != target.PodName ||
		bound.GetDsInstanceUid() != target.InstanceUID || bound.GetDsInstanceEpoch() != target.InstanceEpoch ||
		bound.GetAllocationId() != target.AllocationID || bound.GetReleaseTrack() != target.ReleaseTrack {
		return placement.Binding{}, errcode.New(errcode.ErrLocatorConflict, "BindPlacementTarget returned incomplete/different target")
	}
	return placement.Binding{Version: bound.GetVersion(), OperationID: operationID}, nil
}

func sameBattlePendingOperation(rec *locatorv1.PlayerPlacementStorageRecord, operationID string, matchID uint64) bool {
	return rec != nil && rec.GetTransitionState() == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
		rec.GetTargetRoute() == locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE &&
		rec.GetOperationId() == operationID && rec.GetTargetMatchId() == matchID && rec.GetVersion() > 0
}
