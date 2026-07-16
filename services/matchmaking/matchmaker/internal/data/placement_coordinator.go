package data

import (
	"context"
	"fmt"
	"strings"
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

// RequireStableHub is the StartMatch zero-side-effect authority gate.  A
// missing durable record is UNKNOWN (never "not in Battle"), and a short-TTL
// presence record is deliberately not consulted here.
func (c *GrpcPlacementCoordinator) RequireStableHub(ctx context.Context, playerIDs []uint64) error {
	if c == nil || c.client == nil || len(playerIDs) == 0 {
		return errcode.New(errcode.ErrUnavailable, "placement authority unavailable")
	}
	seen := make(map[uint64]struct{}, len(playerIDs))
	for _, playerID := range playerIDs {
		if playerID == 0 {
			return errcode.New(errcode.ErrInvalidArg, "placement gate contains zero player_id")
		}
		if _, duplicate := seen[playerID]; duplicate {
			return errcode.New(errcode.ErrInvalidArg, "placement gate contains duplicate player %d", playerID)
		}
		seen[playerID] = struct{}{}
		rec, found, err := c.getPlacement(ctx, playerID)
		if err != nil {
			return err
		}
		if !found {
			return errcode.New(errcode.ErrUnavailable,
				"placement is UNKNOWN for player %d", playerID)
		}
		if rec.GetPlayerId() != playerID {
			return errcode.New(errcode.ErrLocatorConflict,
				"placement authority returned player %d for requested player %d", rec.GetPlayerId(), playerID)
		}
		if exactStableHub(rec) {
			continue
		}
		if rec.GetTransitionState() == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE &&
			rec.GetCurrentRoute() == locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE && rec.GetMatchId() != 0 {
			return errcode.New(errcode.ErrMatchInBattle,
				"player %d has authoritative Battle placement match=%d", playerID, rec.GetMatchId())
		}
		return errcode.New(errcode.ErrInvalidState,
			"player %d placement is not exact STABLE_HUB (route=%s transition=%s version=%d operation=%q)",
			playerID, rec.GetCurrentRoute(), rec.GetTransitionState(), rec.GetVersion(), rec.GetOperationId())
	}
	return nil
}

// PreflightBattlePlacement is a read-only gate placed immediately before the
// external AllocateBattle call.  It accepts exact STABLE_HUB or a retry of this
// same durable BATTLE_PENDING operation; every other found state is a definite
// ErrLocatorConflict, while missing/read failure remains retryable UNKNOWN.
func (c *GrpcPlacementCoordinator) PreflightBattlePlacement(
	ctx context.Context,
	operationID string,
	matchID uint64,
	playerIDs []uint64,
) error {
	if c == nil || c.client == nil {
		return errcode.New(errcode.ErrUnavailable, "placement authority unavailable")
	}
	if !placement.ValidOperationID(operationID) || matchID == 0 || len(playerIDs) == 0 {
		return errcode.New(errcode.ErrInvalidArg, "complete battle placement preflight identity required")
	}
	seen := make(map[uint64]struct{}, len(playerIDs))
	for _, playerID := range playerIDs {
		if playerID == 0 {
			return errcode.New(errcode.ErrInvalidArg, "battle placement preflight contains zero player_id")
		}
		if _, duplicate := seen[playerID]; duplicate {
			return errcode.New(errcode.ErrInvalidArg, "battle placement preflight contains duplicate player %d", playerID)
		}
		seen[playerID] = struct{}{}
		if err := c.preflightBattleAllocation(ctx, playerID, operationID, matchID); err != nil {
			return fmt.Errorf("preflight battle placement player=%d match=%d: %w", playerID, matchID, err)
		}
	}
	return nil
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
	seen := make(map[uint64]struct{}, len(playerIDs))
	for _, playerID := range playerIDs {
		if playerID == 0 {
			return nil, errcode.New(errcode.ErrInvalidArg, "battle placement contains zero player_id")
		}
		if _, duplicate := seen[playerID]; duplicate {
			return nil, errcode.New(errcode.ErrInvalidArg, "battle placement contains duplicate player %d", playerID)
		}
		seen[playerID] = struct{}{}
	}
	// Best-effort batch preflight catches an already-conflicting/UNKNOWN later
	// player before the first Begin has any side effect.  It cannot replace each
	// per-player CAS (sources may race after this read), so preparePlayer re-reads
	// and the durable exact-operation retry remains the correctness mechanism.
	for _, playerID := range playerIDs {
		if _, err := c.readBattleBeginVersion(ctx, playerID, operationID, matchID); err != nil {
			return nil, fmt.Errorf("preflight battle placement player=%d match=%d: %w", playerID, matchID, err)
		}
	}
	bindings := make(map[uint64]placement.Binding, len(playerIDs))
	for _, playerID := range playerIDs {
		binding, err := c.preparePlayer(ctx, playerID, operationID, matchID, allocation.Target)
		if err != nil {
			return nil, fmt.Errorf("prepare battle placement player=%d match=%d: %w", playerID, matchID, err)
		}
		bindings[playerID] = binding
	}
	return bindings, nil
}

func (c *GrpcPlacementCoordinator) getPlacement(
	ctx context.Context,
	playerID uint64,
) (*locatorv1.PlayerPlacementStorageRecord, bool, error) {
	resp, err := c.client.GetPlacement(ctx, &locatorv1.GetPlacementRequest{PlayerId: playerID})
	if err != nil {
		return nil, false, errcode.NewCause(errcode.ErrUnavailable, err,
			"placement authority read failed for player %d", playerID)
	}
	if resp.GetCode() != commonv1.ErrCode_OK {
		code := errcode.Code(resp.GetCode())
		if code == errcode.ErrLocatorConflict {
			return nil, false, errcode.New(code,
				"placement authority conflict for player %d", playerID)
		}
		return nil, false, errcode.NewCause(errcode.ErrUnavailable,
			errcode.New(code, "GetPlacement code=%d", resp.GetCode()),
			"placement authority read unavailable for player %d", playerID)
	}
	if !resp.GetFound() || resp.GetPlacement() == nil {
		return nil, false, nil
	}
	return resp.GetPlacement(), true, nil
}

func exactStableHub(rec *locatorv1.PlayerPlacementStorageRecord) bool {
	if rec == nil || rec.GetVersion() == 0 || !placement.ValidOperationID(rec.GetOperationId()) ||
		rec.GetTransitionState() != locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_STABLE ||
		rec.GetCurrentRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB || rec.GetMatchId() != 0 ||
		rec.GetTargetRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_UNSPECIFIED || rec.GetTargetMatchId() != 0 ||
		rec.GetLeaseDeadlineMs() != 0 || rec.GetSourcePlacementVersion() != 0 || rec.GetSourceOperationId() != "" ||
		rec.GetSourceDepartureConfirmed() ||
		rec.GetSourceDepartureProofType() != locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_UNSPECIFIED ||
		rec.GetSourceDepartureProofId() != "" || !placementSourceTarget(rec).Equal(placement.Target{}) {
		return false
	}
	target := placementActiveTarget(rec)
	return target.CompleteHub() && target.AllocationID == "" &&
		canonicalDepartureHistory(rec, rec.GetVersion(), rec.GetOperationId())
}

func exactSameBattlePending(
	rec *locatorv1.PlayerPlacementStorageRecord,
	operationID string,
	matchID uint64,
) bool {
	if !sameBattlePendingOperation(rec, operationID, matchID) || rec.GetVersion() <= 1 ||
		rec.GetCurrentRoute() != locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB || rec.GetMatchId() != 0 ||
		rec.GetSourceMatchId() != 0 || rec.GetProofType() != locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_START ||
		rec.GetProofId() != operationID || rec.GetUpdatedAtMs() <= 0 ||
		rec.GetLeaseDeadlineMs() <= rec.GetUpdatedAtMs() || rec.GetAdmissionId() != "" ||
		rec.GetSourcePlacementVersion() != rec.GetVersion()-1 ||
		!placement.ValidOperationID(rec.GetSourceOperationId()) || rec.GetSourceOperationId() == rec.GetOperationId() ||
		rec.GetSourceDepartureConfirmed() ||
		rec.GetSourceDepartureProofType() != locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_UNSPECIFIED ||
		rec.GetSourceDepartureProofId() != "" {
		return false
	}
	source := placementSourceTarget(rec)
	if !source.CompleteHub() || source.AllocationID != "" ||
		!canonicalDepartureHistory(rec, rec.GetSourcePlacementVersion(), rec.GetSourceOperationId()) {
		return false
	}
	target := placementActiveTarget(rec)
	return target.Equal(placement.Target{}) ||
		(target.CompleteBattle() && target.AssignmentID == "")
}

// preflightBattleAllocation is intentionally stricter than the idempotent
// Prepare path.  Before there is a durable BattleTarget checkpoint, Allocate
// may run only from an exact STABLE_HUB or from the exact same Begin record
// while its target is still wholly unbound.  A complete or partial target is
// evidence that an earlier allocation has already escaped; allocating again
// would create two authoritative DS candidates.
func (c *GrpcPlacementCoordinator) preflightBattleAllocation(
	ctx context.Context,
	playerID uint64,
	operationID string,
	matchID uint64,
) error {
	current, found, err := c.getPlacement(ctx, playerID)
	if err != nil {
		return err
	}
	if !found {
		return errcode.New(errcode.ErrLocatorNotFound, "placement is UNKNOWN")
	}
	if current.GetPlayerId() != playerID {
		return errcode.New(errcode.ErrLocatorConflict,
			"placement authority returned player %d for requested player %d", current.GetPlayerId(), playerID)
	}
	if exactStableHub(current) {
		return nil
	}
	if exactSameBattlePending(current, operationID, matchID) &&
		placementActiveTarget(current).Equal(placement.Target{}) {
		return nil
	}
	return errcode.New(errcode.ErrLocatorConflict,
		"battle allocation preflight requires exact STABLE_HUB or canonical unbound BATTLE_PENDING")
}

func placementActiveTarget(rec *locatorv1.PlayerPlacementStorageRecord) placement.Target {
	return placement.Target{PodName: rec.GetDsPodName(), InstanceUID: rec.GetDsInstanceUid(),
		InstanceEpoch: rec.GetDsInstanceEpoch(), AssignmentID: rec.GetHubAssignmentId(),
		AllocationID: rec.GetAllocationId(), ReleaseTrack: rec.GetReleaseTrack()}
}

func placementSourceTarget(rec *locatorv1.PlayerPlacementStorageRecord) placement.Target {
	return placement.Target{PodName: rec.GetSourceDsPodName(), InstanceUID: rec.GetSourceDsInstanceUid(),
		InstanceEpoch: rec.GetSourceDsInstanceEpoch(), AssignmentID: rec.GetSourceHubAssignmentId(),
		AllocationID: rec.GetSourceAllocationId(), ReleaseTrack: rec.GetSourceReleaseTrack()}
}

// canonicalDepartureHistory mirrors player_locator's fail-closed audit shape:
// the quartet is either wholly absent or wholly bound to the exact placement
// lineage.  last_* is audit-only, but a partial or cross-lineage value is not a
// canonical source for a new allocation.
func canonicalDepartureHistory(
	rec *locatorv1.PlayerPlacementStorageRecord,
	expectedVersion uint64,
	expectedOperationID string,
) bool {
	proofType := rec.GetLastSourceDepartureProofType()
	proofID := rec.GetLastSourceDepartureProofId()
	version := rec.GetLastSourceDeparturePlacementVersion()
	operationID := rec.GetLastSourceDepartureOperationId()
	if proofType == locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_UNSPECIFIED &&
		proofID == "" && version == 0 && operationID == "" {
		return true
	}
	if proofType != locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_HUB_DEPARTURE &&
		proofType != locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_BATTLE_DEPARTURE {
		return false
	}
	return strings.TrimSpace(proofID) != "" && version == expectedVersion &&
		operationID == expectedOperationID && placement.ValidOperationID(operationID)
}

func (c *GrpcPlacementCoordinator) preparePlayer(
	ctx context.Context,
	playerID uint64,
	operationID string,
	matchID uint64,
	target placement.Target,
) (placement.Binding, error) {
	expectedVersion, err := c.readBattleBeginVersion(ctx, playerID, operationID, matchID)
	if err != nil {
		return placement.Binding{}, err
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
	current := beginResp.GetPlacement()
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
		LeaseDeadlineMs:  time.Now().Add(c.leaseTTL).UnixMilli(),
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

func (c *GrpcPlacementCoordinator) readBattleBeginVersion(
	ctx context.Context,
	playerID uint64,
	operationID string,
	matchID uint64,
) (uint64, error) {
	current, found, err := c.getPlacement(ctx, playerID)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, errcode.New(errcode.ErrLocatorNotFound, "placement is UNKNOWN")
	}
	if current.GetPlayerId() != playerID {
		return 0, errcode.New(errcode.ErrLocatorConflict,
			"placement authority returned player %d for requested player %d", current.GetPlayerId(), playerID)
	}
	expectedVersion := current.GetVersion()
	if exactSameBattlePending(current, operationID, matchID) {
		expectedVersion = current.GetVersion() - 1
	} else {
		if !exactStableHub(current) {
			return 0, errcode.New(errcode.ErrLocatorConflict,
				"expected stable HUB, got route=%s transition=%s version=%d operation=%q",
				current.GetCurrentRoute(), current.GetTransitionState(), current.GetVersion(), current.GetOperationId())
		}
	}
	return expectedVersion, nil
}

func sameBattlePendingOperation(rec *locatorv1.PlayerPlacementStorageRecord, operationID string, matchID uint64) bool {
	return rec != nil && rec.GetTransitionState() == locatorv1.PlacementTransitionState_PLACEMENT_TRANSITION_STATE_PENDING &&
		rec.GetTargetRoute() == locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE &&
		rec.GetOperationId() == operationID && rec.GetTargetMatchId() == matchID && rec.GetVersion() > 0
}
